// Package proxy fetches and caches content from remote repositories.
package proxy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Kind selects the cache lifetime: metadata expires, content usually never does.
type Kind int

const (
	Content Kind = iota
	Metadata
)

const UserAgent = "commandcenter/1"

var ErrRemote error = &repo.Error{Status: http.StatusBadGateway, Msg: "remote repository unavailable"}

// ErrRateLimited is the error for a remote that answered 429 to a fetch of
// an asset that is not cached (a cached one is served stale instead), so a
// client sees the remote's limit, not a generic 502.
var ErrRateLimited error = &repo.Error{Status: http.StatusTooManyRequests, Msg: "remote repository rate limit exceeded"}

// Stats, when set on a Fetcher, is told about each cache lookup and each
// fetch from the remote (metrics.Recorder).
type Stats interface {
	Cache(repo string, hit bool)
	Upstream(repo string, ok bool, d time.Duration)
}

// Result is a cached asset; Stale is set when the remote could not be reached
// or no longer has it and an expired copy is returned instead. For a lookup
// under ForHead, Head instead answers an asset that is not cached.
type Result struct {
	Asset meta.Asset
	Stale bool
	Head  *Head
}

// Head is a remote's answer to a HEAD of an asset that is not cached: nothing
// was fetched or stored.
type Head struct {
	Repo, Key     string // the proxy that asked, and the asset
	ContentType   string
	ContentLength int64 // -1: not said
	LastModified  string
}

type headKey struct{}

// FillOnHead marks ctx of a HEAD that is there to fill the cache (pull
// replication's prefetch): ForHead leaves it the GET it has always been.
func FillOnHead(ctx context.Context) context.Context {
	return context.WithValue(ctx, headKey{}, false)
}

// ForHead is r's context for looking up one file: on a client's HEAD, a proxy
// that does not hold the file asks its remote with a HEAD and stores nothing
// (Result.Head, ServeHead). Only for a handler that serves the asset as it is,
// not one that reads it: a checksum, a rewritten document. GET and a HEAD
// under FillOnHead are not affected.
func ForHead(r *http.Request) context.Context {
	ctx := r.Context()
	if _, marked := ctx.Value(headKey{}).(bool); marked || r.Method != http.MethodHead {
		return ctx
	}
	return context.WithValue(ctx, headKey{}, true)
}

// ServeHead answers a HEAD from h; the firewall decides as for a download.
func ServeHead(w http.ResponseWriter, r *http.Request, s *repo.Store, h *Head) {
	if s.Firewall != nil {
		if err := s.Firewall(r.Context(), h.Repo, h.Key); err != nil {
			repo.WriteError(w, r, err)
			return
		}
	}
	w.Header().Set("Content-Type", cmp.Or(h.ContentType, "application/octet-stream"))
	if h.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(h.ContentLength, 10))
	}
	if h.LastModified != "" {
		w.Header().Set("Last-Modified", h.LastModified)
	}
	w.WriteHeader(http.StatusOK)
}

// Target names an asset to fetch. Key is where it is cached in the repository,
// Remote is the path requested relative to the remote URL, URL (when set) is an
// absolute location used instead of Remote, and Header adds request headers.
type Target struct {
	Key    string
	Remote string
	URL    string
	Header http.Header
	// Route, if set, is the path the proxy's routing rule sees instead of
	// the remote path: the request path within the repository, for a file
	// whose upstream URL lies elsewhere (a PyPI file on another host, a
	// chart or archive URL taken from an index), where the administrator's
	// rule cannot know the upstream path.
	Route string
	// SHA256 is the expected content sha256 (hex); a mismatching download is rejected and not cached.
	SHA256 string
	// NotHTML marks an asset that is never a web page: a text/html answer
	// (an upstream directory listing or error page) is taken as a 404, or as
	// a failed fetch when the asset is cached, which serves the cached copy.
	NotHTML bool
	// DeniedIsNotFound takes an upstream 401 or 403 for an asset that is not
	// cached as not found (negative-cached like a 404): Docker Hub answers
	// 401 for an image that does not exist, to not reveal which do. A
	// cached asset is served stale instead, as for any other failure.
	DeniedIsNotFound bool
	// DeniedIsFailure keeps every answer but 404 and 410 for an asset that
	// is not cached a failure (502), not a miss: for the document that
	// decides which group member owns a package (npm packument, PyPI project
	// page), so that an internal registry that refuses or errs (403, 400,
	// 451, ...) fails closed instead of letting a later member's same-named
	// package stand in (resolve.Owner).
	DeniedIsFailure bool

	// Skip, if set, tells the resolver which hosted or proxy repositories do
	// not answer for the target (a Maven version policy): such a repository
	// is not found without a lookup, so a proxy asks no remote, and a group
	// asks its other members.
	Skip func(rp *repo.Repository) bool

	// Keep, if set, is given the header of the remote's answer when a fetch
	// has stored its body, just before the asset is recorded: for what a
	// format keeps of the answer beside the asset (the Link to a listing's
	// next page), there for whoever then finds the asset on any node.
	Keep func(http.Header)

	// Check, if set, looks at a 200's body once it is in the blob store and
	// before the asset is recorded, for a format that can tell a wrong answer
	// by its content (a sign-in page of any declared type, a file that is not
	// what its document promises). An error refuses the answer: nothing is
	// recorded, a cached copy stays as it was, and the fetch has failed as when
	// the remote fails: the copy as stale, else a 502, never a miss, which a
	// group would take as its next member's turn. A 502 error (repo.Error) is
	// what the client is told; any other is only logged. Check returns the
	// sums of what to record: those it got, or of a blob it wrote to the
	// repository's store in their place (a format that rewrites what it fetches).
	Check func(ctx context.Context, body io.ReadSeeker, sums blob.Sums) (blob.Sums, error)
}

type Fetcher struct {
	Store  *repo.Store
	Client *http.Client
	Now    func() time.Time
	Stats  Stats // optional counters
	// RouteAllowed, if set, applies the proxy's routing rule to the remote
	// path (with a leading "/") before the cache is consulted: a refused
	// path is not found, cached or not, and no remote request is made
	// (repo.Registry.RouteAllowed). A group skips such a member for the
	// path; the server answers a direct request with 403.
	RouteAllowed func(rp *repo.Repository, path string) bool
	sf           singleflight.Group
	sem          chan struct{}

	blockMu sync.Mutex
	remotes map[string]*remoteState   // auto-block state by repository incarnation (autoblock.go)
	jars    map[string]http.CookieJar // connection.enableCookies jars by repository incarnation (connection.go)
}

// New creates a Fetcher that runs at most maxConcurrent remote requests at once.
// A value below 1 is clamped to 1, since a zero-capacity semaphore would block forever.
func New(store *repo.Store, client *http.Client, maxConcurrent int) *Fetcher {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Fetcher{Store: store, Client: client, Now: time.Now, sem: make(chan struct{}, maxConcurrent)}
}

// Fetch is FetchTarget for an asset cached under the same path it has remotely.
func (f *Fetcher) Fetch(ctx context.Context, rp *repo.Repository, path string, kind Kind) (Result, error) {
	return f.FetchTarget(ctx, rp, Target{Key: path, Remote: path}, kind)
}

// FetchTarget returns the cached asset for t.Key, consulting the remote when the
// asset is missing or expired. Concurrent fetches of one key share a download.
func (f *Fetcher) FetchTarget(ctx context.Context, rp *repo.Repository, t Target, kind Kind) (Result, error) {
	if t.SHA256 != "" && !IsSHA256Hex(t.SHA256) {
		return Result{}, repo.Errorf(http.StatusInternalServerError, "malformed expected sha256 for %s", t.Key)
	}
	if f.RouteAllowed != nil && !f.RouteAllowed(rp, routePath(t)) {
		return Result{}, repo.ErrNotFound
	}
	now := f.Now()
	a, err := f.Store.Get(ctx, rp.Name, t.Key)
	cached := err == nil
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return Result{}, err
	}
	if cached && kind == Metadata && f.Store.Used != nil && now.Sub(a.LastDownloaded) >= usedEvery {
		f.Store.Used(rp.Name, t.Key)
	}
	if cached && fresh(rp, kind, a, now) {
		f.count(rp.Name, true)
		return Result{Asset: a}, nil
	}
	if !cached && rp.NegativeCacheOn() {
		hit, err := f.Store.DB.NegativeHit(ctx, rp.Name, t.Key, now)
		if err != nil {
			return Result{}, err
		}
		if hit {
			f.count(rp.Name, true)
			return Result{}, repo.ErrNotFound
		}
	}
	f.count(rp.Name, false)
	if err := f.blocked(rp, now); err != nil {
		return f.stale(rp, t.Key, a, cached, err)
	}
	if ask, _ := ctx.Value(headKey{}).(bool); ask && !cached {
		if h := f.headRemote(ctx, rp, t); h != nil {
			return Result{Head: h}, nil
		}
	}
	// The flight is detached from the caller so one cancelled client does not abort
	// a download that other waiters share. This caller may still stop waiting early.
	ch := f.sf.DoChan(fmt.Sprintf("%s\x00%d\x00%s\x00%s", rp.Name, rp.Gen, t.Key, t.URL), func() (any, error) {
		return f.fetchRemote(context.WithoutCancel(ctx), rp, t, a, cached)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return Result{}, r.Err
		}
		return r.Val.(Result), nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// usedEvery is how often a read of a cached document moves its last use: a
// proxy's documents are read far more often than repository.purge-unused,
// which counts in days, needs to know.
const usedEvery = time.Hour

// routePath is the path a routing rule sees for t: its Route, else the
// remote path or the path of an absolute URL, with one leading "/".
func routePath(t Target) string {
	p := cmp.Or(t.Route, t.Remote)
	if p == "" && t.URL != "" {
		if u, err := url.Parse(t.URL); err == nil {
			p = u.Path
		}
	}
	return "/" + strings.TrimPrefix(p, "/")
}

func (f *Fetcher) count(repo string, hit bool) {
	if f.Stats != nil {
		f.Stats.Cache(repo, hit)
	}
}

// IsSHA256Hex reports whether s is exactly 64 hexadecimal characters.
func IsSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// fresh reports whether a may be served without asking the remote: within
// its max age (-1 never expires, 0 always asks), and not invalidated (a
// zero CheckedAt, meta.DB.InvalidateCache).
func fresh(rp *repo.Repository, kind Kind, a meta.Asset, now time.Time) bool {
	maxAge := rp.ContentMaxAge
	if kind == Metadata {
		maxAge = rp.MetadataMaxAge
	}
	switch {
	case a.CheckedAt.IsZero() || maxAge.Off():
		return false
	case maxAge == config.Never:
		return true
	}
	return now.Sub(a.CheckedAt) < time.Duration(maxAge)
}

// forever is the expiry of a miss recorded under negativeCacheTTL -1.
var forever = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// putNegative records a miss of key for rp's negativeCacheTTL, unless its
// negative cache is off.
func (f *Fetcher) putNegative(ctx context.Context, rp *repo.Repository, key string, now time.Time) error {
	if !rp.NegativeCacheOn() {
		return nil
	}
	exp := now.Add(time.Duration(rp.NegativeCacheTTL))
	if rp.NegativeCacheTTL == config.Never {
		exp = forever
	}
	return f.Store.DB.PutNegative(ctx, rp.Name, key, exp)
}

// remoteMiss reports whether a remote's answer to an asset that is not
// cached means the asset is not there, as Nexus takes it: any 4xx but an
// authentication failure (401, 407), 402 and 429 (passed through to the
// client, not cached). S3 and CloudFront answer 403 for a missing key.
func remoteMiss(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusProxyAuthRequired, http.StatusTooManyRequests:
		return false
	}
	return status >= 400 && status < 500
}

func (f *Fetcher) fetchRemote(ctx context.Context, rp *repo.Repository, t Target, a meta.Asset, cached bool) (Result, error) {
	// The timeout covers queueing for a semaphore slot as well as the transfer.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		return Result{}, fmt.Errorf("%w: %v", ErrRemote, ctx.Err())
	}
	defer func() { <-f.sem }()

	req, err := f.newRequest(ctx, rp, targetURL(rp, t), t.Header)
	if err != nil {
		return Result{}, err
	}
	if cached {
		if a.RemoteETag != "" {
			req.Header.Set("If-None-Match", a.RemoteETag)
		}
		if a.RemoteLastModified != "" {
			req.Header.Set("If-Modified-Since", a.RemoteLastModified)
		}
	}

	t0 := time.Now()
	resp, err := f.client(rp).Do(req)
	if f.Stats != nil {
		f.Stats.Upstream(rp.Name, err == nil && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests, time.Since(t0))
	}
	f.note(rp, req, resp, err)
	if err != nil {
		return f.stale(rp, t.Key, a, cached, err)
	}
	defer resp.Body.Close()
	now := f.Now()
	status := resp.StatusCode
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); status == http.StatusOK && t.NotHTML && mt == "text/html" {
		if cached { // a maintenance or sign-in page is a remote failure, not a removal
			return f.stale(rp, t.Key, a, cached, errors.New("remote answered with a web page"))
		}
		slog.Warn("remote answered with a web page, taken as not found", "repo", rp.Name, "path", t.Key)
		status = http.StatusNotFound
	}
	var body io.Reader
	if status == http.StatusOK {
		idle := bodyIdle
		if c := rp.Connection; c != nil && c.Timeout > 0 {
			idle = time.Duration(c.Timeout) * time.Second
		}
		body = newStallReader(resp.Body, idle, cancel)
		if rp.StrictContentTypeValidation {
			// Strict content type validation of a proxy (Nexus): a file whose
			// first bytes are not what its name promises (a sign-in page for
			// a .jar) is not the asset, as a web page for a NotHTML target.
			checked, cerr := repo.CheckContent(t.Key, "", body)
			switch {
			case cerr == nil:
				body = checked
			case cached || repo.StatusOf(cerr) != http.StatusBadRequest: // or the read failed
				return f.stale(rp, t.Key, a, cached, cerr)
			default:
				slog.Warn("remote content does not match its type, taken as not found", "repo", rp.Name, "path", t.Key)
				status = http.StatusNotFound
			}
		}
	}

	switch {
	case status == http.StatusNotModified && cached:
		if err := f.Store.Current(rp); err != nil {
			return Result{}, err
		}
		if err := f.Store.DB.MarkChecked(ctx, rp.Name, t.Key, now); err != nil {
			return Result{}, err
		}
		a.CheckedAt = now.UTC().Truncate(time.Millisecond)
		return Result{Asset: a}, nil

	case status == http.StatusOK:
		var na meta.Asset
		var verr error
		sums, err := f.Store.WriteBlob(rp.Name, body, func(s blob.Sums) error {
			if t.SHA256 != "" && !strings.EqualFold(t.SHA256, s.SHA256) {
				verr = &repo.Error{Status: http.StatusBadGateway, Msg: "upstream content does not match the expected sha256"}
				if f.Store.Current(rp) != nil {
					return verr
				}
				if perr := f.putNegative(ctx, rp, t.Key, f.Now()); perr != nil {
					slog.Warn("recording checksum mismatch failed", "repo", rp.Name, "key", t.Key, "err", logsafe.Err(perr))
				}
				return verr
			}
			return nil
		})
		if err == nil && t.Check != nil {
			sums, err = f.check(ctx, rp, t, sums)
		}
		if err == nil {
			na = f.Store.NewAsset(rp.Name, t.Key, resp.Header.Get("Content-Type"), sums)
			na.RemoteETag = resp.Header.Get("ETag")
			na.RemoteLastModified = resp.Header.Get("Last-Modified")
			if lm, perr := http.ParseTime(na.RemoteLastModified); perr == nil {
				na.LastModified = lm.UTC()
			}
			// A repository deleted or re-created during the download must not receive the asset.
			if cerr := f.Store.Current(rp); cerr != nil {
				return Result{}, cerr
			}
			if t.Keep != nil {
				t.Keep(resp.Header)
			}
			err = f.Store.DB.PutAsset(ctx, na)
		}
		if err != nil {
			if verr != nil || errors.Is(err, blob.ErrDiskFull) || repo.StatusOf(err) == http.StatusServiceUnavailable { // a store that did not open
				return Result{}, err
			}
			return f.stale(rp, t.Key, a, cached, err)
		}
		if err := f.Store.DB.ClearNegative(ctx, rp.Name, t.Key); err != nil {
			return Result{}, err
		}
		return Result{Asset: na}, nil

	// A cached asset the remote no longer has stays served, marked stale and
	// asked about again next time, as in Nexus. So does the document that
	// decides a group's owner: an internal registry's 404 must not hand a
	// package to a later, public member. Deleting the copy moves it on.
	case (status == http.StatusNotFound || status == http.StatusGone) && !cached,
		t.DeniedIsNotFound && !cached && status == http.StatusUnauthorized,
		!cached && remoteMiss(status) && !t.DeniedIsFailure:
		// A 404 from a remote that was replaced meanwhile must not outlive the change.
		if cerr := f.Store.Current(rp); cerr != nil {
			return Result{}, cerr
		}
		if err := f.putNegative(ctx, rp, t.Key, now); err != nil {
			return Result{}, err
		}
		return Result{}, repo.ErrNotFound
	}
	if status == http.StatusTooManyRequests && !cached {
		slog.Warn("remote rate limit", "repo", rp.Name, "path", t.Key)
		return Result{}, ErrRateLimited
	}
	return f.stale(rp, t.Key, a, cached, fmt.Errorf("remote status %d", resp.StatusCode))
}

// check runs t.Check on the blob of sums in rp's store.
func (f *Fetcher) check(ctx context.Context, rp *repo.Repository, t Target, sums blob.Sums) (blob.Sums, error) {
	body, err := f.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return sums, err
	}
	defer body.Close()
	return t.Check(ctx, body, sums)
}

// note counts the outcome of a request for rp's auto-block. Only the remote's
// own host counts: an absolute target on a CDN or a mirror says nothing about it.
func (f *Fetcher) note(rp *repo.Repository, req *http.Request, resp *http.Response, err error) {
	switch {
	case !sendsCredentials(rp, req.URL):
	case err != nil:
		f.noteFailure(rp, err)
	case resp.StatusCode >= 500:
		f.noteFailure(rp, fmt.Errorf("remote status %d", resp.StatusCode))
	default:
		f.noteSuccess(rp)
	}
}

// targetURL is where t is on rp's remote.
func targetURL(rp *repo.Repository, t Target) string {
	if t.URL != "" {
		return t.URL
	}
	u := *rp.RemoteURL
	u.Path = rp.RemoteURL.Path + t.Remote
	u.RawPath = ""
	return u.String()
}

// headRemote asks rp's remote about t with a HEAD. Only a plain 200 answers
// the client: for anything else (a remote that takes no HEAD or answers it
// differently than a GET, a 404, an error) it returns nil, and the GET that
// follows decides with all its rules, records a miss and counts for auto-block.
func (f *Fetcher) headRemote(ctx context.Context, rp *repo.Repository, t Target) *Head {
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	defer func() { <-f.sem }()
	req, err := f.newRequest(ctx, rp, targetURL(rp, t), t.Header)
	if err != nil {
		return nil
	}
	req.Method = http.MethodHead
	t0 := time.Now()
	resp, err := f.client(rp).Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || t.NotHTML && mt == "text/html" {
		return nil
	}
	if f.Stats != nil {
		f.Stats.Upstream(rp.Name, true, time.Since(t0))
	}
	return &Head{Repo: rp.Name, Key: t.Key, ContentType: resp.Header.Get("Content-Type"), ContentLength: resp.ContentLength,
		LastModified: resp.Header.Get("Last-Modified")}
}

// bodyIdle is how long a read of a remote body may wait for data before its
// fetch is cancelled; a var for tests.
var bodyIdle = 60 * time.Second

// stallReader cancels its fetch when a read waits bodyIdle for the remote,
// so a stalled remote gives up its fetch slot long before the fetch timeout.
// The timer runs only inside Read: the time the fetch spends before the first
// read and between reads (writing to a slow blob store) is not the remote's.
type stallReader struct {
	r       io.Reader
	idle    time.Duration
	timer   *time.Timer
	stalled atomic.Bool
}

func newStallReader(r io.Reader, idle time.Duration, cancel context.CancelFunc) *stallReader {
	s := &stallReader{r: r, idle: idle}
	s.timer = time.AfterFunc(idle, func() {
		s.stalled.Store(true)
		cancel()
	})
	s.timer.Stop()
	return s
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.timer.Reset(s.idle)
	n, err := s.r.Read(p)
	s.timer.Stop()
	if err != nil && s.stalled.Load() {
		err = fmt.Errorf("remote body stalled for %v: %w", s.idle, err)
	}
	return n, err
}

func (f *Fetcher) stale(rp *repo.Repository, path string, a meta.Asset, cached bool, cause error) (Result, error) {
	slog.Warn("remote fetch failed", "repo", rp.Name, "path", path, "cached", cached, "err", logsafe.Err(cause))
	if cached {
		return Result{Asset: a, Stale: true}, nil
	}
	if repo.StatusOf(cause) == http.StatusBadGateway { // a refusal that says why (Target.Check)
		return Result{}, cause
	}
	// The cause (a *url.Error carries the remote URL) is only logged: client bodies must not expose it.
	return Result{}, ErrRemote
}

// SetStaleHeader marks a response served from an expired cache entry.
func SetStaleHeader(w http.ResponseWriter, res Result) {
	if res.Stale {
		w.Header().Set("Warning", `110 commandcenter "Response is Stale"`)
	}
}

// sendsCredentials reports whether remote credentials may be sent to u: only
// the configured remote's own scheme and host.
func sendsCredentials(rp *repo.Repository, u *url.URL) bool {
	return u.Scheme == rp.RemoteURL.Scheme && u.Host == rp.RemoteURL.Host
}

// Slots reports how many remote-fetch slots are in use and the maximum.
func (f *Fetcher) Slots() (inUse, max int) { return len(f.sem), cap(f.sem) }

// newRequest builds a GET of target for rp: the user agent, the hop chain,
// header, and the remote's credentials when target is on the remote's host.
func (f *Fetcher) newRequest(ctx context.Context, rp *repo.Repository, target string, header http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(repoContext(ctx, rp), http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set(HopHeader, hopValue(ctx))
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Credentials belong to the configured remote; never send them to other hosts.
	if ra := rp.RemoteAuth; ra != nil && sendsCredentials(rp, req.URL) {
		if ra.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+ra.BearerToken)
		} else {
			req.SetBasicAuth(ra.Username, ra.Password)
		}
	}
	return req, nil
}

// Request performs an uncached GET of url for rp, under the egress policy
// of rp's remote and with its credentials when url is on the remote's
// host: for answers that are not content to cache (a Terraform module
// download endpoint answers 204 with the archive URL in a header). The
// caller closes the body. A blocked remote is not asked (ErrRemote), a network
// error is ErrRemote too, and the outcome counts for auto-block as a fetch's does.
func (f *Fetcher) Request(ctx context.Context, rp *repo.Repository, url string) (*http.Response, error) {
	if err := f.blocked(rp, f.Now()); err != nil {
		slog.Warn("remote request skipped", "repo", rp.Name, "url", logsafe.Path(url), "err", logsafe.Err(err))
		return nil, ErrRemote
	}
	req, err := f.newRequest(ctx, rp, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client(rp).Do(req)
	f.note(rp, req, resp, err)
	if err != nil {
		slog.Warn("remote request failed", "repo", rp.Name, "url", logsafe.Path(url), "err", logsafe.Err(err))
		return nil, ErrRemote
	}
	return resp, nil
}
