package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ---- repository.verify-blobs ----

// DefaultVerifyBytes is how many bytes one checksum run reads (maxBytes).
const DefaultVerifyBytes = 10 << 30

// verifyLogPaths is how many missing or mismatched paths a run log names.
const verifyLogPaths = 100

// verifyParams takes one repository or, when empty, every repository.
// checksum re-reads every blob and compares its SHA-256, at most maxBytes
// per run; without it each blob is only looked up (and its size compared).
type verifyParams struct {
	Repository string `json:"repository,omitempty"`
	Checksum   bool   `json:"checksum,omitempty"`
	MaxBytes   int64  `json:"maxBytes,omitempty"`
}

var verifyFields = []ParamSpec{
	{Name: "repository", Type: "repository", Help: "empty: every repository"},
	{Name: "checksum", Type: "bool", Default: false, Help: "re-read every blob and compare its SHA-256"},
	{Name: "maxBytes", Type: "bytes", Default: int64(DefaultVerifyBytes), Help: "bytes read per run with checksum; the next run resumes"},
}

func verifyParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p verifyParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	if p.MaxBytes < 0 {
		return "", []string{"params.maxBytes must not be negative"}
	}
	if p.MaxBytes == 0 {
		p.MaxBytes = DefaultVerifyBytes
	}
	if check && p.Repository != "" && s.Repos.Get(p.Repository) == nil {
		return "", []string{fmt.Sprintf("repository %q does not exist", p.Repository)}
	}
	return encodeParams(p), nil
}

// verifyCount tallies one run.
type verifyCount struct {
	checked, missing, mismatched, failed int64
	bytes                                int64
	logged                               int
}

func (c *verifyCount) note(log *RunLog, format string, args ...any) {
	c.logged++
	if c.logged <= verifyLogPaths {
		log.Errorf(format, args...)
	}
}

func runVerify(ctx context.Context, s *Service, t meta.Task, _ string, log *RunLog) (summary, error) {
	var p verifyParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if s.Store == nil {
		return summary{msg: "blob stores are not available"}, errors.New("blob stores are not available")
	}
	var repos []*repo.Repository
	if p.Repository != "" {
		rp := s.Repos.Get(p.Repository)
		if rp == nil {
			return summary{}, fmt.Errorf("repository %q no longer exists: edit or delete this task", p.Repository)
		}
		repos = []*repo.Repository{rp}
	} else {
		repos = s.Repos.Snapshot().Sorted
	}
	var c verifyCount
	scanned := 0
	// ponytail: one byte budget for the whole run, repositories in name
	// order; a huge first repository delays the others until its cursor
	// reaches the end. Per-repository budgets if that bites.
	exhausted := false
	for _, rp := range repos {
		if err := ctx.Err(); err != nil {
			return verifySummary(c, scanned), err
		}
		if exhausted {
			log.Warnf("%s: not checked: the byte budget was used up; the next run continues", rp.Name)
			continue
		}
		if m, moving := s.Store.MoveOf(rp.Name); moving {
			log.Warnf("%s: not checked: its move to blob store %q is unfinished", rp.Name, m.Target)
			continue
		}
		bs, err := s.Store.BlobStore(rp.Name)
		if err != nil {
			c.failed++
			log.Errorf("%s: %v", rp.Name, err)
			continue
		}
		key := cursorKey(t.ID, rp.Name)
		from, err := s.DB.GetSetting(ctx, key)
		if err != nil && !errors.Is(err, meta.ErrNotFound) {
			c.failed++
			log.Errorf("%s: reading the scan cursor: %v", rp.Name, err)
			continue
		}
		before := c
		next, err := verifyRepo(ctx, s, bs, rp.Name, p, from, &c, log)
		if err != nil {
			c.failed++
			log.Errorf("%s: %v", rp.Name, err)
			continue
		}
		scanned++
		msg := fmt.Sprintf("%s: checked %d assets: %d missing, %d mismatched", rp.Name, c.checked-before.checked, c.missing-before.missing, c.mismatched-before.mismatched)
		if from != "" {
			msg += fmt.Sprintf(" (resumed at %q)", from)
		}
		if next != "" {
			exhausted = true
			msg += fmt.Sprintf("; stopped at the byte budget, the next run resumes at %q", next)
		}
		log.Infof("%s", msg)
		if next != from {
			if err := s.DB.SetSetting(context.WithoutCancel(ctx), key, next); err != nil {
				log.Warnf("%s: could not store the scan cursor: %v", rp.Name, err)
			}
		}
	}
	if c.logged > verifyLogPaths {
		log.Errorf("%d more problems not listed", c.logged-verifyLogPaths)
	}
	sum := verifySummary(c, scanned)
	log.Infof("%s", sum.msg)
	if c.missing > 0 || c.mismatched > 0 || c.failed > 0 {
		return sum, fmt.Errorf("%d blobs missing, %d mismatched, %d checks failed (see the log)", c.missing, c.mismatched, c.failed)
	}
	return sum, nil
}

func verifySummary(c verifyCount, repos int) summary {
	return summary{msg: fmt.Sprintf("checked %d assets in %d repositories: %d missing, %d mismatched", c.checked, repos, c.missing, c.mismatched), bytes: c.bytes}
}

// verifyRepo checks the assets of repoName from path from on, in path
// order, and returns where the next run resumes ("" when it reached the
// end).
func verifyRepo(ctx context.Context, s *Service, bs *blob.Store, repoName string, p verifyParams, from string, c *verifyCount, log *RunLog) (string, error) {
	const page = 1000
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return from, err
		}
		var assets []meta.ScanAsset
		var err error
		if first {
			assets, err = s.DB.ScanAssetsFrom(ctx, repoName, from, page)
		} else {
			assets, err = s.DB.ScanAssets(ctx, repoName, from, page)
		}
		if err != nil {
			return from, err
		}
		first = false
		for _, a := range assets {
			if p.Checksum && c.checked > 0 && c.bytes+a.Size > p.MaxBytes {
				return a.Path, nil
			}
			c.checked++
			info, err := bs.Stat(a.BlobSHA256)
			switch {
			case errors.Is(err, blob.ErrNotFound):
				c.missing++
				c.note(log, "missing: %s/%s (blob %s)", repoName, a.Path, a.BlobSHA256)
				continue
			case err != nil:
				c.failed++
				c.note(log, "%s/%s: %v", repoName, a.Path, err)
				continue
			case info.Size != a.Size:
				c.mismatched++
				c.note(log, "mismatch: %s/%s: the blob holds %d bytes, the asset says %d", repoName, a.Path, info.Size, a.Size)
				continue
			case !p.Checksum:
				continue
			}
			c.bytes += a.Size
			sum, err := blobSum(bs, a.BlobSHA256)
			switch {
			case err != nil:
				c.failed++
				c.note(log, "%s/%s: %v", repoName, a.Path, err)
			case sum != a.BlobSHA256:
				c.mismatched++
				c.note(log, "mismatch: %s/%s: the blob's SHA-256 is %s, not %s", repoName, a.Path, sum, a.BlobSHA256)
			}
		}
		if len(assets) < page {
			return "", nil
		}
		from = assets[len(assets)-1].Path
	}
}

func blobSum(bs *blob.Store, sha string) (string, error) {
	f, err := bs.Open(sha)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
