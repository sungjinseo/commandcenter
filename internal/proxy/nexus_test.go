package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// core-01: a 4xx other than 401, 402, 407 and 429 for an asset that is not
// cached is not found, negative-cached (S3 and CloudFront answer 403 for a
// missing key); 429 is passed through and not cached; 401 is a failure.
func TestFetchRemoteMissStatuses(t *testing.T) {
	for _, tc := range []struct {
		status, want int
		cached       bool
	}{
		{400, 404, true}, {403, 404, true}, {405, 404, true}, {410, 404, true}, {451, 404, true},
		{429, 429, false}, {401, 502, false}, {407, 502, false},
	} {
		rm := &remote{}
		f, rp, _ := setup(t, rm)
		rm.status.Store(int32(tc.status))
		for i := 0; i < 2; i++ {
			if _, err := f.Fetch(ctx, rp, "g/a-sources.jar", Content); repo.StatusOf(err) != tc.want {
				t.Fatalf("%d, fetch %d: %v (status %d), want %d", tc.status, i, err, repo.StatusOf(err), tc.want)
			}
		}
		if hits, want := rm.hits.Load(), map[bool]int32{true: 1, false: 2}[tc.cached]; hits != want {
			t.Fatalf("%d: %d remote requests, want %d", tc.status, hits, want)
		}
	}
}

// A package document that decides a group's owner keeps a 403 a failure.
func TestFetchDeniedIsFailure(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setup(t, rm)
	rm.status.Store(403)
	for i := 0; i < 2; i++ {
		if _, err := f.FetchTarget(ctx, rp, Target{Key: "pkg", Remote: "pkg", DeniedIsFailure: true}, Metadata); repo.StatusOf(err) != 502 {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if rm.hits.Load() != 2 {
		t.Fatalf("hits %d, want 2 (not negative-cached)", rm.hits.Load())
	}
}

// A cached asset whose revalidation is refused (403) is served stale, not
// dropped: the refusal says nothing about the content.
func TestFetchCachedForbiddenServesStale(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	if _, err := f.Fetch(ctx, rp, "g/maven-metadata.xml", Metadata); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(2 * time.Hour)
	rm.status.Store(403)
	if res, err := f.Fetch(ctx, rp, "g/maven-metadata.xml", Metadata); err != nil || !res.Stale {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// core-02: a max age of 0 revalidates on every request.
func TestFetchMaxAgeZero(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setup(t, rm)
	rp.MetadataMaxAge = config.Zero
	for i := 0; i < 3; i++ {
		if _, err := f.Fetch(ctx, rp, "g/maven-metadata.xml", Metadata); err != nil {
			t.Fatal(err)
		}
	}
	if rm.hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", rm.hits.Load())
	}
}

// core-02: negativeCacheEnabled false and a TTL of 0 record no misses; a TTL
// of -1 never expires.
func TestNegativeCacheOffAndForever(t *testing.T) {
	no := false
	for name, set := range map[string]func(*repo.Repository){
		"disabled": func(rp *repo.Repository) { rp.NegativeCacheEnabled = &no },
		"ttl 0":    func(rp *repo.Repository) { rp.NegativeCacheTTL = config.Zero },
	} {
		rm := &remote{}
		f, rp, _ := setup(t, rm)
		set(rp)
		f.Fetch(ctx, rp, "missing", Content)
		f.Fetch(ctx, rp, "missing", Content)
		if rm.hits.Load() != 2 {
			t.Fatalf("%s: hits = %d, want 2", name, rm.hits.Load())
		}
	}
	rm := &remote{}
	f, rp, c := setup(t, rm)
	rp.NegativeCacheTTL = config.Never
	f.Fetch(ctx, rp, "missing", Content)
	c.t = c.t.Add(10 * 365 * 24 * time.Hour)
	if _, err := f.Fetch(ctx, rp, "missing", Content); repo.StatusOf(err) != 404 || rm.hits.Load() != 1 {
		t.Fatalf("forever: %v, hits %d", err, rm.hits.Load())
	}
	if n, err := f.Store.DB.PruneNegative(ctx, c.t); err != nil || n != 0 {
		t.Fatalf("pruned %d %v", n, err)
	}
}

// core-03: an invalidated asset is revalidated even with contentMaxAge -1
// (a conditional request), then fresh again; a recorded miss is forgotten.
func TestFetchInvalidated(t *testing.T) {
	var conditional atomic.Int32
	rm := &remote{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			conditional.Add(1)
		}
		rm.ServeHTTP(w, r)
	}))
	defer ts.Close()
	f, rp, _ := setup(t, rm)
	u, _ := url.Parse(ts.URL + "/maven2/")
	rp.RemoteURL = u
	f.Fetch(ctx, rp, "g/a.jar", Content)
	f.Fetch(ctx, rp, "missing", Content)
	if err := f.Store.DB.InvalidateCache(ctx, []string{rp.Name}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.Fetch(ctx, rp, "g/a.jar", Content); err != nil {
			t.Fatal(err)
		}
	}
	f.Fetch(ctx, rp, "missing", Content)
	if rm.hits.Load() != 4 || conditional.Load() != 1 {
		t.Fatalf("hits %d (want 4), conditional %d (want 1)", rm.hits.Load(), conditional.Load())
	}
}

// core-05: only the remote's own host counts for its auto-block, and a
// change of how it is reached (here its connection settings) clears it.
func TestAutoBlockOwnHostAndReset(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer cdn.Close()
	rm := &remote{}
	f, rp, _ := setup(t, rm)
	for i := 0; i < autoBlockAfter+1; i++ {
		f.FetchTarget(ctx, rp, Target{Key: "x", URL: cdn.URL + "/x"}, Content)
	}
	if st := f.Remote(rp); st.Blocked {
		t.Fatalf("blocked by another host's failures: %+v", st)
	}
	rm.status.Store(503)
	for i := 0; i < autoBlockAfter; i++ {
		f.Fetch(ctx, rp, "g/x.jar", Content)
	}
	if st := f.Remote(rp); !st.Blocked {
		t.Fatalf("not blocked: %+v", st)
	}
	edited := *rp
	edited.Connection = &config.Connection{Timeout: 5}
	if st := f.Remote(&edited); st.Blocked {
		t.Fatalf("still blocked after a settings change: %+v", st)
	}
}

// core-06: the remote's cookies come back to it when enableCookies is set
// (a remote that redirects to itself until the cookie returns), and go to no
// other host.
func TestFetchCookies(t *testing.T) {
	var other atomic.Value
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Store(r.Header.Get("Cookie"))
		w.Write([]byte("cdn"))
	}))
	defer elsewhere.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/maven2/cdn.jar" {
			http.Redirect(w, r, elsewhere.URL+"/cdn.jar", http.StatusFound)
			return
		}
		if _, err := r.Cookie("lb"); err != nil {
			http.SetCookie(w, &http.Cookie{Name: "lb", Value: "node1", Path: "/"})
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	f, rp, _ := setup(t, &remote{})
	u, _ := url.Parse(ts.URL + "/maven2/")
	rp.RemoteURL = u
	f.Client = ts.Client()
	f.Client.CheckRedirect = NewClient().CheckRedirect
	if _, err := f.Fetch(ctx, rp, "g/a.jar", Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("without cookies: %v", err)
	}
	rp.Connection = &config.Connection{EnableCookies: true}
	res, err := f.Fetch(ctx, rp, "g/b.jar", Content)
	if err != nil || read(t, f, res.Asset) != "ok" {
		t.Fatalf("with cookies: %+v %v", res, err)
	}
	if _, err := f.Fetch(ctx, rp, "cdn.jar", Content); err != nil || other.Load() != "" {
		t.Fatalf("cdn: %v, cookie sent there: %q", err, other.Load())
	}
}

// core-06: a repository's timeout, retries and User-Agent suffix replace the
// global ones for its requests only.
func TestConnectionOverrides(t *testing.T) {
	defer SetConfig(Config{})
	SetConfig(Config{UserAgentSuffix: "global", HeaderTimeout: 10 * time.Second})
	defer func(d time.Duration) { retryWait = d }(retryWait)
	retryWait = time.Millisecond
	var ua atomic.Value
	var drops atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.Header.Get("User-Agent"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/slow.jar"):
			time.Sleep(1500 * time.Millisecond)
		case strings.HasSuffix(r.URL.Path, "/flaky.jar") && drops.Add(1) <= 2:
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close() // a network error, retried
			return
		}
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	f, rp, _ := setup(t, &remote{})
	u, _ := url.Parse(ts.URL + "/maven2/")
	rp.RemoteURL = u
	f.Client = NewClient()
	if _, err := f.Fetch(ctx, rp, "g/a.jar", Content); err != nil || ua.Load() != UserAgent+" global" {
		t.Fatalf("global: %v %q", err, ua.Load())
	}
	if _, err := f.Fetch(ctx, rp, "g/flaky.jar", Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("no retries: %v", err)
	}
	drops.Store(0)
	two := 2
	rp.Connection = &config.Connection{Timeout: 1, Retries: &two, UserAgentSuffix: "mine/1"}
	if _, err := f.Fetch(ctx, rp, "g/b.jar", Content); err != nil || ua.Load() != UserAgent+" mine/1" {
		t.Fatalf("own suffix: %v %q", err, ua.Load())
	}
	if _, err := f.Fetch(ctx, rp, "g/flaky.jar", Content); err != nil {
		t.Fatalf("two retries: %v", err)
	}
	rp.Connection = &config.Connection{Timeout: 1} // a timeout is retried too: none here
	start := time.Now()
	if _, err := f.Fetch(ctx, rp, "g/slow.jar", Content); !errors.Is(err, ErrRemote) || time.Since(start) > 1400*time.Millisecond {
		t.Fatalf("timeout 1 s: %v after %v", err, time.Since(start))
	}
	rp.Connection = nil
	if _, err := f.Fetch(ctx, rp, "g/slow.jar", Content); err != nil {
		t.Fatalf("global timeout: %v", err)
	}
}
