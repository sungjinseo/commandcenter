// Package proxy fetches and caches content from remote repositories.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
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

// Result is a cached asset; Stale is set when the remote could not be reached
// and an expired copy is returned instead.
type Result struct {
	Asset meta.Asset
	Stale bool
}

// Target names an asset to fetch. Key is where it is cached in the repository,
// Remote is the path requested relative to the remote URL, URL (when set) is an
// absolute location used instead of Remote, and Header adds request headers.
type Target struct {
	Key    string
	Remote string
	URL    string
	Header http.Header
	// SHA256 is the expected content sha256 (hex); a mismatching download is rejected and not cached.
	SHA256 string
	// NotHTML marks an asset that is never a web page: a text/html answer
	// (an upstream directory listing or error page) is taken as a 404.
	NotHTML bool
}

type Fetcher struct {
	Store  *repo.Store
	Client *http.Client
	Now    func() time.Time
	sf     singleflight.Group
	sem    chan struct{}
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
	now := f.Now()
	a, err := f.Store.Get(ctx, rp.Name, t.Key)
	cached := err == nil
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return Result{}, err
	}
	if cached && fresh(rp, kind, a, now) {
		return Result{Asset: a}, nil
	}
	if !cached {
		hit, err := f.Store.DB.NegativeHit(ctx, rp.Name, t.Key, now)
		if err != nil {
			return Result{}, err
		}
		if hit {
			return Result{}, repo.ErrNotFound
		}
	}
	// The flight is detached from the caller so one cancelled client does not abort
	// a download that other waiters share. This caller may still stop waiting early.
	ch := f.sf.DoChan(fmt.Sprintf("%s\x00%d\x00%s", rp.Name, rp.Gen, t.Key), func() (any, error) {
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

func fresh(rp *repo.Repository, kind Kind, a meta.Asset, now time.Time) bool {
	maxAge := rp.ContentMaxAge
	if kind == Metadata {
		maxAge = rp.MetadataMaxAge
	}
	if maxAge == config.Never {
		return true
	}
	return now.Sub(a.CheckedAt) < time.Duration(maxAge)
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

	target := t.URL
	if target == "" {
		u := *rp.RemoteURL
		u.Path = rp.RemoteURL.Path + t.Remote
		u.RawPath = ""
		target = u.String()
	}
	req, err := http.NewRequestWithContext(withRemote(ctx, rp.RemoteURL), http.MethodGet, target, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set(HopHeader, hopValue(ctx))
	for k, vs := range t.Header {
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
	if cached {
		if a.RemoteETag != "" {
			req.Header.Set("If-None-Match", a.RemoteETag)
		}
		if a.RemoteLastModified != "" {
			req.Header.Set("If-Modified-Since", a.RemoteLastModified)
		}
	}

	resp, err := f.Client.Do(req)
	if err != nil {
		return f.stale(rp, t.Key, a, cached, err)
	}
	defer resp.Body.Close()
	now := f.Now()
	status := resp.StatusCode
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); status == http.StatusOK && t.NotHTML && mt == "text/html" {
		slog.Warn("remote answered with a web page, taken as not found", "repo", rp.Name, "path", t.Key)
		status = http.StatusNotFound
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
		body := newStallReader(resp.Body, cancel)
		var na meta.Asset
		var verr error
		sums, err := f.Store.WriteBlob(rp.Name, body, func(s blob.Sums) error {
			if t.SHA256 != "" && !strings.EqualFold(t.SHA256, s.SHA256) {
				verr = &repo.Error{Status: http.StatusBadGateway, Msg: "upstream content does not match the expected sha256"}
				if f.Store.Current(rp) != nil {
					return verr
				}
				if perr := f.Store.DB.PutNegative(ctx, rp.Name, t.Key, f.Now().Add(time.Duration(rp.NegativeCacheTTL))); perr != nil {
					slog.Warn("recording checksum mismatch failed", "repo", rp.Name, "key", t.Key, "err", logsafe.Err(perr))
				}
				return verr
			}
			return nil
		})
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
			err = f.Store.DB.PutAsset(ctx, na)
		}
		if err != nil {
			if verr != nil || errors.Is(err, blob.ErrDiskFull) {
				return Result{}, err
			}
			return f.stale(rp, t.Key, a, cached, err)
		}
		if err := f.Store.DB.ClearNegative(ctx, rp.Name, t.Key); err != nil {
			return Result{}, err
		}
		return Result{Asset: na}, nil

	case status == http.StatusNotFound || status == http.StatusGone:
		// A 404 from a remote that was replaced meanwhile must not outlive the change.
		if cerr := f.Store.Current(rp); cerr != nil {
			return Result{}, cerr
		}
		exp := now.Add(time.Duration(rp.NegativeCacheTTL))
		if err := f.Store.DB.PutNegative(ctx, rp.Name, t.Key, exp); err != nil {
			return Result{}, err
		}
		return Result{}, repo.ErrNotFound
	}
	return f.stale(rp, t.Key, a, cached, fmt.Errorf("remote status %d", resp.StatusCode))
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
	timer   *time.Timer
	stalled atomic.Bool
}

func newStallReader(r io.Reader, cancel context.CancelFunc) *stallReader {
	s := &stallReader{r: r}
	s.timer = time.AfterFunc(bodyIdle, func() {
		s.stalled.Store(true)
		cancel()
	})
	s.timer.Stop()
	return s
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.timer.Reset(bodyIdle)
	n, err := s.r.Read(p)
	s.timer.Stop()
	if err != nil && s.stalled.Load() {
		err = fmt.Errorf("remote body stalled for %v: %w", bodyIdle, err)
	}
	return n, err
}

func (f *Fetcher) stale(rp *repo.Repository, path string, a meta.Asset, cached bool, cause error) (Result, error) {
	slog.Warn("remote fetch failed", "repo", rp.Name, "path", path, "cached", cached, "err", logsafe.Err(cause))
	if cached {
		return Result{Asset: a, Stale: true}, nil
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
