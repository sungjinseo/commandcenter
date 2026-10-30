// Package replication pulls what is uploaded to a hosted repository of
// another Nexus Repository or Command Center instance into a proxy of this
// one ahead of any request (Nexus Pro "pull replication", pre-emptive pull):
// the repository.pull-replication task pages through the source's asset
// list, GET <base>/service/rest/v1/assets?repository=<name>, and asks the
// proxy, through its own repository endpoint, for every listed component
// file it does not hold yet. The proxy's remote names the source and its
// remoteAuth signs in, as for any fetch.
package replication

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Service replicates; the app wires it.
type Service struct {
	DB      *meta.DB
	Store   *repo.Store
	Fetcher *proxy.Fetcher // the source's API requests, under the proxy's egress policy and credentials
	// Prefetch asks proxy rp for path as actor, as a client's HEAD of
	// /repository/<rp>/<path> would (server.ServeRepository): routing rule,
	// firewall, content checks, blob store and events apply. It returns the
	// status.
	Prefetch func(ctx context.Context, rp *repo.Repository, path, actor string) int
	// Slack is how far before the last complete run's newest lastModified
	// a run looks again, as an upload's lastModified can precede its
	// commit; 0 is an hour (tests shorten it).
	Slack time.Duration
}

// Logger is the run log (tasks.RunLog).
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Asset is one entry of a source's asset list.
type Asset struct {
	Path, SHA1, SHA256 string
	DownloadURL        string
	LastModified       time.Time // zero when the source gives none
}

// Result counts a run's assets: Listed by the source, Matched (component
// files the regex selects), which end up Unchanged (older than the last
// complete run), Held (already cached with the source's checksum),
// Changed (cached with another checksum: the proxy keeps its copy until
// contentMaxAge expires it), Fetched (Bytes), Refused (the proxy answered
// 4xx: routing rule, firewall, validation, not found) or Failed (5xx,
// a checksum mismatch, a database error), which the next run retries.
type Result struct {
	Listed, Matched, Unchanged, Held, Changed, Fetched, Refused, Failed int
	Bytes                                                               int64
}

func (r Result) String() string {
	return fmt.Sprintf("%d assets listed, %d to replicate: %d fetched (%d bytes), %d held already, %d unchanged since the last run, "+
		"%d changed at the source, %d refused, %d failed", r.Listed, r.Matched, r.Fetched, r.Bytes, r.Held, r.Unchanged, r.Changed, r.Refused, r.Failed)
}

const (
	// workers bounds the prefetches of one run. ponytail: fixed; a task
	// parameter if a link needs more.
	workers   = 4
	maxLogged = 20       // per-asset lines of each kind per run
	maxPage   = 16 << 20 // bytes of one page of the asset list
)

// state is a proxy's progress, kept in the settings table so every node
// sees it: the newest lastModified a complete run listed, for the
// incarnation (gen) and regex it ran with.
type state struct {
	Gen          uint64    `json:"gen"`
	Regex        string    `json:"regex"`
	LastModified time.Time `json:"lastModified"`
	LastRun      time.Time `json:"lastRun"`
}

func stateKey(name string) string { return "replication/" + name }

// Run replicates proxy rp once; full ignores the progress of earlier runs
// (every listed asset not held is fetched). The error is the listing's,
// or says how many assets failed.
func (s *Service) Run(ctx context.Context, rp *repo.Repository, full bool, actor string, log Logger) (Result, error) {
	if !rp.ReplicationEnabled() {
		return Result{}, errors.New("pull replication is not enabled")
	}
	base, src, ok := config.ReplicationSource(rp.Remote)
	if !ok {
		return Result{}, errors.New("the remote names no hosted repository (<base URL>/repository/<name>/)")
	}
	cfg := *rp.Replication
	var re *regexp.Regexp
	if cfg.AssetPathRegex != "" {
		var err error
		if re, err = regexp.Compile(cfg.AssetPathRegex); err != nil {
			return Result{}, err
		}
	}
	var st state
	if _, err := s.DB.GetSettingJSON(ctx, stateKey(rp.Name), &st); err != nil {
		return Result{}, err
	}
	var since time.Time
	if !full && st.Gen == rp.Gen && st.Regex == cfg.AssetPathRegex && !st.LastModified.IsZero() {
		slack := s.Slack
		if slack == 0 {
			slack = time.Hour
		}
		since = st.LastModified.Add(-slack)
		log.Infof("%s: replicating from %s, repository %s: assets modified since %s", rp.Name, redact(base), src, since.UTC().Format(time.RFC3339))
	} else {
		log.Infof("%s: replicating from %s, repository %s: every asset", rp.Name, redact(base), src)
	}
	r := &run{s: s, rp: rp, actor: actor, log: log, verify: cfg.VerifyChecksums}
	var g errgroup.Group
	g.SetLimit(workers)
	var newest time.Time
	listErr := s.list(ctx, rp, base, src, func(a Asset) {
		if a.LastModified.After(newest) {
			newest = a.LastModified
		}
		r.mu.Lock()
		r.res.Listed++
		if !wanted(rp.Format, a.Path, re) {
			r.mu.Unlock()
			return
		}
		r.res.Matched++
		if !since.IsZero() && !a.LastModified.IsZero() && a.LastModified.Before(since) {
			r.res.Unchanged++
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		g.Go(func() error { r.one(ctx, a); return nil })
	})
	g.Wait()
	res := r.result()
	if listErr == nil && ctx.Err() == nil {
		// Complete: the next run starts at the newest lastModified listed,
		// or at the oldest one that failed, so it is retried.
		mark := newest
		if !r.oldestFailed.IsZero() && r.oldestFailed.Before(mark) {
			mark = r.oldestFailed
		}
		next := state{Gen: rp.Gen, Regex: cfg.AssetPathRegex, LastModified: mark, LastRun: time.Now().UTC()}
		if err := s.DB.SetSettingJSON(context.WithoutCancel(ctx), stateKey(rp.Name), next); err != nil {
			log.Warnf("%s: could not store the replication progress: %v", rp.Name, err)
		}
	}
	r.logOverflow()
	switch {
	case listErr != nil:
		return res, fmt.Errorf("listing the source: %w", listErr)
	case ctx.Err() != nil:
		return res, ctx.Err()
	case res.Failed > 0:
		return res, fmt.Errorf("%d assets failed", res.Failed)
	}
	return res, nil
}

// run is one Run's shared state; mu guards res, the log (used by one
// goroutine at a time) and oldestFailed.
type run struct {
	s      *Service
	rp     *repo.Repository
	actor  string
	log    Logger
	verify bool

	mu           sync.Mutex
	res          Result
	oldestFailed time.Time
	logged       [3]int // lines of kind refused, failed, changed
}

const (
	kindRefused = iota
	kindFailed
	kindChanged
)

func (r *run) result() Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.res
}

// note counts a in kind and logs the first maxLogged lines of each kind.
func (r *run) note(kind int, a Asset, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch kind {
	case kindRefused:
		r.res.Refused++
	case kindFailed:
		r.res.Failed++
		if !a.LastModified.IsZero() && (r.oldestFailed.IsZero() || a.LastModified.Before(r.oldestFailed)) {
			r.oldestFailed = a.LastModified
		}
	case kindChanged:
		r.res.Changed++
	}
	if r.logged[kind]++; r.logged[kind] > maxLogged {
		return
	}
	msg := r.rp.Name + ": " + a.Path + ": " + fmt.Sprintf(format, args...)
	switch kind {
	case kindFailed:
		r.log.Errorf("%s", msg)
	case kindRefused:
		r.log.Warnf("%s", msg)
	default:
		r.log.Infof("%s", msg)
	}
}

func (r *run) logOverflow() {
	for kind, what := range []string{"refused", "failed", "changed at the source"} {
		if n := r.logged[kind] - maxLogged; n > 0 {
			r.log.Warnf("%s: %d more assets %s (not logged one by one)", r.rp.Name, n, what)
		}
	}
}

// one replicates asset a unless the proxy holds it.
func (r *run) one(ctx context.Context, a Asset) {
	if !safePath(a.Path) {
		r.note(kindRefused, a, "not a plain relative path")
		return
	}
	name := r.rp.Name
	req, p := request(r.rp.Format, a)
	held, err := r.s.DB.GetAsset(ctx, name, p)
	switch {
	case err == nil && sameSum(held, a):
		r.mu.Lock()
		r.res.Held++
		r.mu.Unlock()
		return
	case err == nil:
		r.note(kindChanged, a, "changed at the source; the proxy keeps its copy until contentMaxAge expires it")
		return
	case !errors.Is(err, meta.ErrNotFound):
		r.note(kindFailed, a, "%v", err)
		return
	}
	// The source lists it: a miss recorded before it was uploaded is stale.
	if err := r.s.DB.ClearNegative(ctx, name, p); err != nil {
		r.note(kindFailed, a, "%v", err)
		return
	}
	switch status := r.s.Prefetch(ctx, r.rp, req, r.actor); {
	case status >= 500 || status == http.StatusTooManyRequests || status < 200:
		r.note(kindFailed, a, "the proxy answered %d", status)
		return
	case status >= 300:
		r.note(kindRefused, a, "the proxy answered %d", status)
		return
	}
	got, err := r.s.DB.GetAsset(ctx, name, p)
	if err != nil {
		r.note(kindFailed, a, "the proxy served it but holds no asset at this path: %v", err)
		return
	}
	if !sameSum(got, a) {
		if r.verify {
			if err := r.s.Store.Delete(context.WithoutCancel(ctx), name, p); err != nil {
				r.note(kindFailed, a, "checksum differs from the source's, and removing it failed: %v", err)
				return
			}
			r.note(kindFailed, a, "checksum differs from the source's: removed")
			return
		}
		r.mu.Lock()
		if r.logged[kindChanged]++; r.logged[kindChanged] <= maxLogged {
			r.log.Warnf("%s: %s: fetched, but its checksum differs from the source's (verifyChecksums is off)", name, p)
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.res.Fetched++
	r.res.Bytes += got.Size
	r.mu.Unlock()
}

// request maps a hosted asset the source lists to the path a client asks a
// proxy of format for and the key the proxy caches it under, the hosted
// path itself for most formats (Command Center's layouts, which follow
// Nexus's where it has the format).
func request(format string, a Asset) (req, key string) {
	switch p := a.Path; format {
	case "nuget": // a proxy keeps the flat container's paths
		return "v3-flatcontainer/" + p, "v3-flatcontainer/" + p
	case "swift": // <scope>/<name>/<version>/source-archive.zip is GET <scope>/<name>/<version>.zip
		if dir, ok := strings.CutSuffix(p, "/source-archive.zip"); ok {
			return dir + ".zip", p
		}
	case "composer": // dist/<name>/<version>.zip: a proxy names it after the sha1 of the source's dist URL
		if rest, ok := strings.CutSuffix(p, ".zip"); ok && a.DownloadURL != "" {
			sum := sha1.Sum([]byte(a.DownloadURL))
			k := rest + "/" + hex.EncodeToString(sum[:]) + ".zip"
			return k, k
		}
	}
	return a.Path, a.Path
}

// wanted reports whether p, a path the source lists, is a component file
// of format that re (nil: any) matches with or without its leading "/".
// Index documents (maven-metadata.xml, index.yaml, repodata) are left to
// the proxy, which fetches them by its metadataMaxAge.
func wanted(format, p string, re *regexp.Regexp) bool {
	if _, _, _, ok := component.DeriveAsset(format, p); !ok {
		return false
	}
	return re == nil || re.MatchString(p) || re.MatchString("/"+p)
}

// safePath refuses a listed path a client's request could not have: empty,
// absolute, with "." or ".." segments, empty segments or a NUL.
func safePath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.Contains(p, "\x00") && p != "." && p != ".."
}

// sameSum compares a held asset with the source's checksums: sha1, else
// sha256; with neither on both sides presence counts.
func sameSum(a meta.Asset, s Asset) bool {
	switch {
	case s.SHA1 != "" && a.SHA1 != "":
		return strings.EqualFold(a.SHA1, s.SHA1)
	case s.SHA256 != "" && a.BlobSHA256 != "":
		return strings.EqualFold(a.BlobSHA256, s.SHA256)
	}
	return true
}

// list pages through the source's asset list, calling fn for each asset.
func (s *Service) list(ctx context.Context, rp *repo.Repository, base, src string, fn func(Asset)) error {
	seen := map[string]bool{}
	for token := ""; ; {
		page, next, err := s.page(ctx, rp, base, src, token)
		if err != nil {
			return err
		}
		for _, a := range page {
			fn(a)
		}
		switch {
		case next == "":
			return nil
		case seen[next]:
			return errors.New("the source repeated a continuation token")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		seen[next], token = true, next
	}
}

// page reads one page of the source's asset list.
func (s *Service) page(ctx context.Context, rp *repo.Repository, base, src, token string) ([]Asset, string, error) {
	q := url.Values{"repository": {src}}
	if token != "" {
		q.Set("continuationToken", token)
	}
	var body struct {
		Items []struct {
			Path     string `json:"path"`
			Checksum struct {
				SHA1   string `json:"sha1"`
				SHA256 string `json:"sha256"`
			} `json:"checksum"`
			LastModified string `json:"lastModified"`
			DownloadURL  string `json:"downloadUrl"`
		} `json:"items"`
		ContinuationToken string `json:"continuationToken"`
	}
	if err := s.getJSON(ctx, rp, base+"/service/rest/v1/assets?"+q.Encode(), src, &body); err != nil {
		return nil, "", err
	}
	out := make([]Asset, 0, len(body.Items))
	for _, it := range body.Items {
		// Nexus 3.91 (and Command Center) list paths with a leading "/".
		out = append(out, Asset{Path: strings.TrimPrefix(it.Path, "/"), SHA1: it.Checksum.SHA1, SHA256: it.Checksum.SHA256, DownloadURL: it.DownloadURL,
			LastModified: parseTime(it.LastModified)})
	}
	return out, body.ContinuationToken, nil
}

// getJSON GETs u from the source and decodes its JSON answer into v.
func (s *Service) getJSON(ctx context.Context, rp *repo.Repository, u, src string, v any) error {
	resp, err := s.Fetcher.Request(ctx, rp, u)
	if err != nil {
		return fmt.Errorf("the source is unreachable (%v)", err)
	}
	defer resp.Body.Close()
	switch c := resp.StatusCode; {
	case c == http.StatusUnauthorized || c == http.StatusForbidden:
		return fmt.Errorf("the source refused the remote credentials (HTTP %d)", c)
	case c == http.StatusNotFound:
		return fmt.Errorf("the source has no repository %q, or the remote credentials may not read it (HTTP 404)", src)
	case c != http.StatusOK:
		return fmt.Errorf("the source answered HTTP %d", c)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPage)).Decode(v); err != nil {
		return fmt.Errorf("the source's answer is not the Nexus REST API's: %v", err)
	}
	return nil
}

// parseTime reads a lastModified: RFC 3339, or Java's offset without a colon.
func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.999999999Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// redact drops the userinfo of a base URL for the log.
func redact(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}

// Check is the answer of Test.
type Check struct {
	OK         bool   `json:"ok"`
	Source     string `json:"source"`     // the instance's base URL, without credentials
	Repository string `json:"repository"` // the hosted repository there
	Message    string `json:"message"`
}

// Test checks that rp's source answers with its credentials and has a
// hosted repository of rp's format.
func (s *Service) Test(ctx context.Context, rp *repo.Repository) Check {
	base, src, ok := config.ReplicationSource(rp.Remote)
	if !ok {
		return Check{Message: "the remote names no hosted repository: use <base URL>/repository/<name>/"}
	}
	c := Check{Source: redact(base), Repository: src}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var info struct {
		Format string `json:"format"`
		Type   string `json:"type"`
	}
	if err := s.getJSON(ctx, rp, base+"/service/rest/v1/repositories/"+url.PathEscape(src), src, &info); err != nil {
		c.Message = err.Error()
		return c
	}
	want := rp.Format
	if want == "maven" {
		want = "maven2" // Nexus's name, which both answer
	}
	if info.Format != want {
		c.Message = fmt.Sprintf("the source repository is a %s repository, not %s", info.Format, want)
		return c
	}
	if info.Type != "hosted" {
		c.Message = fmt.Sprintf("the source repository is a %s repository: replicate from a hosted one", info.Type)
		return c
	}
	if _, _, err := s.page(ctx, rp, base, src, ""); err != nil {
		c.Message = err.Error()
		return c
	}
	c.OK, c.Message = true, fmt.Sprintf("the source answers: %s hosted repository %q lists its assets", rp.Format, src)
	return c
}
