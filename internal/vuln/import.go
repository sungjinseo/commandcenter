package vuln

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"commandcenter/internal/events"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// ErrBusy refuses an import or sync while another runs.
var ErrBusy = errors.New("an import or download of vulnerability data is already running")

// ErrTooLarge refuses a dump over the maxImportMiB setting.
var ErrTooLarge = errors.New("the archive is larger than the vulnerability.maxImportMiB setting")

// ArchiveError is a dump that is not a safe OSV zip archive; nothing of it
// past the last committed batch is stored.
type ArchiveError struct{ Msg string }

func (e *ArchiveError) Error() string { return e.Msg }

// Limits of one dump beyond its size (maxEntryBytes bounds one entry).
const (
	maxEntries    = 2_000_000
	maxTotalBytes = 64 << 30 // uncompressed
	importBatch   = 500      // advisories per transaction
)

// ImportResult counts what an import stored.
type ImportResult struct {
	Advisories int `json:"advisories"` // stored (affecting a covered package)
	Rows       int `json:"rows"`       // advisory-package rows
	Skipped    int `json:"skipped"`    // entries that are no OSV JSON or affect no covered package
}

func (r *ImportResult) add(o ImportResult) {
	r.Advisories, r.Rows, r.Skipped = r.Advisories+o.Advisories, r.Rows+o.Rows, r.Skipped+o.Skipped
}

// limitReader fails once more than n bytes were read, unlike
// io.LimitReader, which ends quietly.
type limitReader struct {
	r io.Reader
	n int64
}

var errEntryTooLarge = errors.New("entry too large")

func (l *limitReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if l.n -= int64(n); l.n < 0 {
		return n, errEntryTooLarge
	}
	return n, err
}

// spool copies r into a new temporary file, at most limit bytes.
func (s *Service) spool(r io.Reader, limit int64) (string, error) {
	f, err := os.CreateTemp(s.TempDir, "osv-import-*.zip")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = ErrTooLarge
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// Import stores the OSV dump read from r (an uploaded all.zip) and reports
// it as an activity event of actor.
func (s *Service) Import(ctx context.Context, r io.Reader, actor string) (ImportResult, error) {
	if !s.busy.TryLock() {
		return ImportResult{}, ErrBusy
	}
	defer s.busy.Unlock()
	path, err := s.spool(r, int64(s.Settings(ctx).MaxImportMiB)<<20)
	if err != nil {
		return ImportResult{}, err
	}
	defer os.Remove(path)
	res, err := s.importFile(ctx, path, "upload")
	if err == nil {
		s.emit("vulnerability.import", actor, "", "upload", fmt.Sprintf("%d advisories", res.Advisories))
	}
	return res, err
}

// importFile stores the dump at path. The index answers "maybe" for every
// package meanwhile and is rebuilt afterwards; the import is recorded
// under databaseKey, which tells the other instances to rebuild theirs.
func (s *Service) importFile(ctx context.Context, path, source string) (ImportResult, error) {
	s.importing.Store(true)
	defer func() {
		s.invalidate()
		s.importing.Store(false)
	}()
	res, err := s.readArchive(ctx, path)
	if res.Advisories > 0 || err == nil {
		db := Database{ImportedAt: time.Now().UTC(), Source: source, Advisories: res.Advisories}
		if serr := s.DB.SetSettingJSON(context.WithoutCancel(ctx), databaseKey, db); serr != nil && err == nil {
			err = serr
		} else if serr == nil {
			s.mu.Lock()
			s.dbGen = db.ImportedAt // this instance rebuilds once, below
			s.mu.Unlock()
		}
	}
	return res, err
}

func (s *Service) readArchive(ctx context.Context, path string) (ImportResult, error) {
	var res ImportResult
	zr, err := zip.OpenReader(path)
	if errors.Is(err, zip.ErrInsecurePath) {
		return res, &ArchiveError{Msg: "the archive holds an entry with an unsafe name (an absolute path or \"..\")"}
	}
	if err != nil {
		return res, &ArchiveError{Msg: "not a zip archive"}
	}
	defer zr.Close()
	if len(zr.File) > maxEntries {
		return res, &ArchiveError{Msg: fmt.Sprintf("the archive holds more than %d entries", maxEntries)}
	}
	// Entries are never extracted, so a hostile name cannot write anywhere;
	// an archive with one is still refused: it is no OSV dump.
	for _, f := range zr.File {
		if !filepath.IsLocal(f.Name) || strings.Contains(f.Name, `\`) {
			return res, &ArchiveError{Msg: fmt.Sprintf("the archive holds an entry with an unsafe name: %q", f.Name)}
		}
	}
	var ids []string
	var rows []meta.VulnAdvisory
	flush := func() error {
		if len(ids) == 0 {
			return nil
		}
		if err := s.DB.ReplaceVulnAdvisories(ctx, ids, rows); err != nil {
			return err
		}
		res.Rows += len(rows)
		ids, rows = ids[:0], rows[:0]
		return nil
	}
	var total uint64
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if f.FileInfo().IsDir() || !strings.HasSuffix(f.Name, ".json") {
			res.Skipped++
			continue
		}
		if total += f.UncompressedSize64; f.UncompressedSize64 > maxEntryBytes || total > maxTotalBytes {
			return res, &ArchiveError{Msg: fmt.Sprintf("entry %q is larger than %d MiB, or the archive than %d GiB uncompressed", f.Name, maxEntryBytes>>20, maxTotalBytes>>30)}
		}
		rc, err := f.Open()
		if err != nil {
			return res, &ArchiveError{Msg: fmt.Sprintf("entry %q: %v", f.Name, err)}
		}
		var e Entry
		err = json.NewDecoder(&limitReader{r: rc, n: maxEntryBytes}).Decode(&e)
		rc.Close()
		if errors.Is(err, errEntryTooLarge) {
			return res, &ArchiveError{Msg: fmt.Sprintf("entry %q is larger than %d MiB", f.Name, maxEntryBytes>>20)}
		}
		if err != nil || !idRE.MatchString(e.ID) {
			res.Skipped++
			continue
		}
		rs := e.Rows()
		ids, rows = append(ids, e.ID), append(rows, rs...) // an id without rows drops its stored ones
		if len(rs) == 0 {
			res.Skipped++
		} else {
			res.Advisories++
		}
		if len(ids) >= importBatch {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	return res, flush()
}

// Sync downloads <dumpURL>/<ecosystem>/all.zip for each configured
// ecosystem and stores it; logf reports progress. Every ecosystem is
// tried; the errors are joined.
func (s *Service) Sync(ctx context.Context, logf func(format string, args ...any)) (ImportResult, error) {
	var total ImportResult
	set := s.Settings(ctx)
	if !set.Enabled {
		return total, errors.New("vulnerability data is disabled in the settings")
	}
	if !s.busy.TryLock() {
		return total, ErrBusy
	}
	defer s.busy.Unlock()
	c, err := s.client(set.DumpURL)
	if err != nil {
		return total, err
	}
	ecos := set.Ecosystems
	if len(ecos) == 0 {
		ecos = CoveredEcosystems()
	}
	var errs []error
	var done []string
	for _, eco := range ecos {
		res, err := s.syncOne(ctx, c, set, eco)
		if err != nil {
			logf("%s: %v", eco, err)
			errs = append(errs, fmt.Errorf("%s: %w", eco, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		logf("%s: %d advisories (%d rows, %d entries skipped)", eco, res.Advisories, res.Rows, res.Skipped)
		total.add(res)
		done = append(done, eco)
	}
	if len(done) > 0 {
		s.emit("vulnerability.import", events.System, "", "sync", fmt.Sprintf("%d advisories (%s)", total.Advisories, strings.Join(done, ", ")))
	}
	return total, errors.Join(errs...)
}

func (s *Service) syncOne(ctx context.Context, c *http.Client, set Settings, eco string) (ImportResult, error) {
	target := strings.TrimRight(set.DumpURL, "/") + "/" + url.PathEscape(eco) + "/all.zip"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ImportResult{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		s.log().Warn("could not download an OSV dump", "ecosystem", eco, "err", logsafe.Err(err))
		return ImportResult{}, errors.New("download failed (see the server log)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ImportResult{}, fmt.Errorf("download answered status %d", resp.StatusCode)
	}
	path, err := s.spool(resp.Body, int64(set.MaxImportMiB)<<20)
	if err != nil {
		return ImportResult{}, err
	}
	defer os.Remove(path)
	return s.importFile(ctx, path, "sync:"+eco)
}
