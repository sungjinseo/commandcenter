package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/logring"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type remote struct {
	hits     atomic.Int32
	status   atomic.Int32 // 0 means serve normally
	delay    time.Duration
	inflight atomic.Int32 // requests currently being served
	peak     atomic.Int32 // highest value inflight has reached
}

func (rm *remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rm.hits.Add(1)
	n := rm.inflight.Add(1)
	defer rm.inflight.Add(-1)
	for {
		p := rm.peak.Load()
		if n <= p || rm.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(rm.delay)
	if s := rm.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	if r.URL.Path == "/maven2/missing" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("If-None-Match") == `"v1"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", `"v1"`)
	io.WriteString(w, "body of "+r.URL.Path)
}

func setup(t *testing.T, rm *remote) (*Fetcher, *repo.Repository, *clock) {
	t.Helper()
	return setupN(t, rm, 4)
}

func setupN(t *testing.T, rm *remote, maxConcurrent int) (*Fetcher, *repo.Repository, *clock) {
	t.Helper()
	ts := httptest.NewServer(rm)
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: c.now}
	f := New(store, ts.Client(), maxConcurrent)
	f.Now = c.now
	cfg := &config.Config{Repositories: []config.Repository{{
		Name: "p", Format: "maven", Type: "proxy", Remote: ts.URL + "/maven2",
		MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never,
		NegativeCacheTTL: config.Duration(10 * time.Minute),
	}}}
	return f, repo.Build(cfg)["p"], c
}

func read(t *testing.T, f *Fetcher, a meta.Asset) string {
	t.Helper()
	file, err := f.Store.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	b, _ := io.ReadAll(file)
	return string(b)
}

var ctx = context.Background()

func TestFetchCachesContentForever(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	res, err := f.Fetch(ctx, rp, "g/a.jar", Content)
	if err != nil || read(t, f, res.Asset) != "body of /maven2/g/a.jar" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	c.t = c.t.Add(1000 * time.Hour)
	if _, err := f.Fetch(ctx, rp, "g/a.jar", Content); err != nil {
		t.Fatal(err)
	}
	if rm.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", rm.hits.Load())
	}
}

func TestFetchRevalidatesMetadata(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	f.Fetch(ctx, rp, "g/maven-metadata.xml", Metadata)
	f.Fetch(ctx, rp, "g/maven-metadata.xml", Metadata) // fresh: no hit
	if rm.hits.Load() != 1 {
		t.Fatalf("hits = %d", rm.hits.Load())
	}
	c.t = c.t.Add(2 * time.Hour)
	res, err := f.Fetch(ctx, rp, "g/maven-metadata.xml", Metadata) // 304
	if err != nil || res.Stale || rm.hits.Load() != 2 {
		t.Fatalf("res=%+v err=%v hits=%d", res, err, rm.hits.Load())
	}
	if !res.Asset.CheckedAt.Equal(c.t) {
		t.Fatalf("checkedAt = %v, want %v", res.Asset.CheckedAt, c.t)
	}
}

func TestFetchStaleOnError(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	f.Fetch(ctx, rp, "g/maven-metadata.xml", Metadata)
	c.t = c.t.Add(2 * time.Hour)
	rm.status.Store(503)
	res, err := f.Fetch(ctx, rp, "g/maven-metadata.xml", Metadata)
	if err != nil || !res.Stale {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	w := httptest.NewRecorder()
	SetStaleHeader(w, res)
	if w.Header().Get("Warning") == "" {
		t.Fatal("missing Warning header")
	}
}

func TestFetchRemoteDownWithoutCache(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setup(t, rm)
	rm.status.Store(500)
	_, err := f.Fetch(ctx, rp, "g/a.jar", Content)
	if !errors.Is(err, ErrRemote) || repo.StatusOf(err) != 502 {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchNegativeCache(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	for i := 0; i < 2; i++ {
		if _, err := f.Fetch(ctx, rp, "missing", Content); repo.StatusOf(err) != 404 {
			t.Fatalf("err = %v", err)
		}
	}
	if rm.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", rm.hits.Load())
	}
	c.t = c.t.Add(11 * time.Minute)
	f.Fetch(ctx, rp, "missing", Content)
	if rm.hits.Load() != 2 {
		t.Fatalf("hits after ttl = %d, want 2", rm.hits.Load())
	}
}

func TestFetchSingleflight(t *testing.T) {
	rm := &remote{delay: 100 * time.Millisecond}
	f, rp, _ := setup(t, rm)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.Fetch(ctx, rp, "g/big.jar", Content); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if rm.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", rm.hits.Load())
	}
}

func TestFetchWaiterCanCancel(t *testing.T) {
	rm := &remote{delay: 500 * time.Millisecond}
	f, rp, _ := setup(t, rm)

	first := make(chan error, 1)
	go func() {
		_, err := f.Fetch(ctx, rp, "g/slow.jar", Content)
		first <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the first caller start the shared flight

	wctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := f.Fetch(wctx, rp, "g/slow.jar", Content)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("waiter took %v to leave", elapsed)
	}

	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first fetch failed after waiter left: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first fetch did not finish")
	}
}

func TestFetchConcurrencyCap(t *testing.T) {
	rm := &remote{delay: 100 * time.Millisecond}
	f, rp, _ := setupN(t, rm, 2)

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.Fetch(ctx, rp, fmt.Sprintf("g/part%d.jar", i), Content)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak := rm.peak.Load(); peak > 2 {
		t.Fatalf("peak in-flight remote requests = %d, want <= 2", peak)
	}
}

func TestNewClampsConcurrency(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setupN(t, rm, 0)

	done := make(chan error, 1)
	go func() {
		_, err := f.Fetch(ctx, rp, "g/a.jar", Content)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch deadlocked with maxConcurrent=0")
	}
}

// A remote that sends its headers and then stalls the body used to hold a
// fetch slot until the 30-minute fetch timeout; bodyIdle without progress now
// ends the fetch, while a slow body that keeps arriving is not cut.
func TestFetchAbortsStalledBody(t *testing.T) {
	defer func(d time.Duration) { bodyIdle = d }(bodyIdle)
	bodyIdle = 500 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		for i := 0; i < 8; i++ {
			if r.URL.Path == "/stall" && i == 1 {
				<-r.Context().Done() // until the fetch gives up
				return
			}
			io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer ts.Close()
	f, _, _ := setup(t, &remote{})
	cfg := &config.Config{Repositories: []config.Repository{{Name: "s", Type: "proxy", Remote: ts.URL, ContentMaxAge: config.Never}}}
	rp := repo.Build(cfg)["s"]

	start := time.Now()
	if _, err := f.Fetch(ctx, rp, "stall", Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("stalled body: err = %v, want ErrRemote", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("stalled body held the fetch for %v", d)
	}
	if inUse, _ := f.Slots(); inUse != 0 {
		t.Fatalf("fetch slots in use after the stall = %d", inUse)
	}
	if _, err := f.Store.Get(ctx, "s", "stall"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("partial body was cached: %v", err)
	}
	res, err := f.Fetch(ctx, rp, "slow", Content) // 0.7 s in all, never 0.5 s without a byte
	if err != nil || read(t, f, res.Asset) != "xxxxxxxx" {
		t.Fatalf("slow body: res=%+v err=%v", res, err)
	}
}

// The stall timer runs only while a read waits for the remote: the time the
// fetch spends before its first read and between reads (writing what came to
// a blob store that has stalled) is not the remote's.
func TestStallTimerCountsOnlyReads(t *testing.T) {
	defer func(d time.Duration) { bodyIdle = d }(bodyIdle)
	bodyIdle = 50 * time.Millisecond
	var cancelled atomic.Bool
	s := newStallReader(strings.NewReader("ab"), func() { cancelled.Store(true) })
	p := make([]byte, 1)
	for range 3 { // "a", "b", EOF
		time.Sleep(4 * bodyIdle)
		s.Read(p)
	}
	if cancelled.Load() {
		t.Fatal("time outside Read cancelled the fetch")
	}
}

func TestFetchSendsRemoteAuth(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		io.WriteString(w, "x")
	}))
	defer ts.Close()
	f, _, _ := setup(t, &remote{})
	cfg := &config.Config{Repositories: []config.Repository{{Name: "a", Type: "proxy", Remote: ts.URL,
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "p"}, ContentMaxAge: config.Never}}}
	rp := repo.Build(cfg)["a"]
	if _, err := f.Fetch(ctx, rp, "x", Content); err != nil {
		t.Fatal(err)
	}
	if got != "Basic dTpw" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestFetchTargetUsesRemotePathAndHeaders(t *testing.T) {
	var gotPath, gotAccept string
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath, gotAccept = r.URL.Path, r.Header.Get("Accept")
		io.WriteString(w, `{"name":"pkg"}`)
	}))
	defer ts.Close()
	f, _, _ := setup(t, &remote{})
	cfg := &config.Config{Repositories: []config.Repository{{Name: "npm", Type: "proxy", Remote: ts.URL + "/reg",
		MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never}}}
	rp := repo.Build(cfg)["npm"]
	target := Target{Key: "pkg/package.json", Remote: "pkg", Header: http.Header{"Accept": {"application/json"}}}
	for i := 0; i < 2; i++ {
		res, err := f.FetchTarget(ctx, rp, target, Metadata)
		if err != nil {
			t.Fatal(err)
		}
		if read(t, f, res.Asset) != `{"name":"pkg"}` {
			t.Fatalf("content = %q", read(t, f, res.Asset))
		}
	}
	if gotPath != "/reg/pkg" || gotAccept != "application/json" || hits.Load() != 1 {
		t.Fatalf("path=%q accept=%q hits=%d", gotPath, gotAccept, hits.Load())
	}
	if _, err := f.Store.Get(ctx, "npm", "pkg/package.json"); err != nil {
		t.Fatalf("not cached under key: %v", err)
	}
}

func TestFetchTargetAbsoluteURLKeepsCredentialsAtHome(t *testing.T) {
	var gotAuth, gotPath string
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		io.WriteString(w, "wheel")
	}))
	defer files.Close()
	f, _, _ := setup(t, &remote{})
	cfg := &config.Config{Repositories: []config.Repository{{Name: "py", Type: "proxy", Remote: "https://index.example/",
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "p"}, ContentMaxAge: config.Never}}}
	rp := repo.Build(cfg)["py"]
	res, err := f.FetchTarget(ctx, rp, Target{Key: "packages/six/six.whl", URL: files.URL + "/x/six.whl"}, Content)
	if err != nil || read(t, f, res.Asset) != "wheel" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if gotPath != "/x/six.whl" || gotAuth != "" {
		t.Fatalf("path=%q auth=%q (credentials must not leave the remote host)", gotPath, gotAuth)
	}
}

func TestFetchTargetRejectsSHA256MismatchWithoutCaching(t *testing.T) {
	f, rp, _ := setup(t, &remote{})
	_, err := f.FetchTarget(ctx, rp, Target{Key: "a/b", Remote: "/a/b", SHA256: strings.Repeat("0", 64)}, Content)
	if repo.StatusOf(err) != http.StatusBadGateway {
		t.Fatalf("err = %v, want status 502", err)
	}
	if _, err := f.Store.Get(ctx, "p", "a/b"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("mismatching download was cached: %v", err)
	}
}

func TestFetchTargetRejectsMalformedSHA256BeforeFetching(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setup(t, rm)
	for _, bad := range []string{"00", strings.Repeat("g", 64), strings.Repeat("0", 63), strings.Repeat("0", 65)} {
		_, err := f.FetchTarget(ctx, rp, Target{Key: "a/b", Remote: "/a/b", SHA256: bad}, Content)
		if repo.StatusOf(err) != http.StatusInternalServerError {
			t.Fatalf("sha %q: err = %v, want status 500", bad, err)
		}
	}
	if rm.hits.Load() != 0 {
		t.Fatalf("remote was contacted %d times", rm.hits.Load())
	}
}

func TestSendsCredentialsRequiresSameSchemeAndHost(t *testing.T) {
	cfg := &config.Config{Repositories: []config.Repository{{Name: "py", Type: "proxy", Remote: "https://host/",
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "p"}, ContentMaxAge: config.Never}}}
	rp := repo.Build(cfg)["py"]
	for u, want := range map[string]bool{
		"https://host/x":  true,
		"http://host/x":   false,
		"https://other/x": false,
	} {
		pu, _ := url.Parse(u)
		if got := sendsCredentials(rp, pu); got != want {
			t.Errorf("sendsCredentials(%s) = %v, want %v", u, got, want)
		}
	}
}

func TestChecksumMismatchIsNegativeCached(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setup(t, rm) // the remote serves "body of <path>", which never matches
	want := strings.Repeat("a", 64)
	tg := Target{Key: "x", Remote: "/x", SHA256: want}
	if _, err := f.FetchTarget(ctx, rp, tg, Content); repo.StatusOf(err) != 502 {
		t.Fatalf("first: %v", err)
	}
	if _, err := f.FetchTarget(ctx, rp, tg, Content); repo.StatusOf(err) != 404 {
		t.Fatalf("second (negative-cached): %v", err)
	}
	if rm.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", rm.hits.Load())
	}
}

func TestFetchErrorBodyHidesRemoteURLAndCredentials(t *testing.T) {
	f, _, _ := setup(t, &remote{})
	f.Client = &http.Client{Timeout: 5 * time.Second}
	cfg := &config.Config{Repositories: []config.Repository{{
		Name: "down", Format: "maven", Type: "proxy", Remote: "http://svc-user:s3cretpw@127.0.0.1:1/maven2",
		MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never,
		NegativeCacheTTL: config.Duration(time.Minute),
	}}}
	rp := repo.Build(cfg)["down"]
	_, err := f.Fetch(ctx, rp, "g/a.jar", Content)
	if repo.StatusOf(err) != 502 {
		t.Fatalf("err = %v", err)
	}
	w := httptest.NewRecorder()
	repo.WriteError(w, httptest.NewRequest("GET", "/", nil), err)
	body := w.Body.String()
	for _, secret := range []string{"svc-user", "s3cretpw", "127.0.0.1", "maven2"} {
		if strings.Contains(body, secret) {
			t.Fatalf("502 body leaks %q: %s", secret, body)
		}
	}
	if w.Code != 502 || !strings.Contains(body, "remote repository unavailable") {
		t.Fatalf("%d %q", w.Code, body)
	}
}

func TestFetchRecreatedRepositoryDoesNotJoinOldFlight(t *testing.T) {
	slow := &remote{delay: 300 * time.Millisecond}
	f, old, _ := setup(t, slow)
	old.Gen = 1
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "new remote")
	}))
	defer fast.Close()
	recreated := &repo.Repository{Repository: old.Repository, RemoteURL: mustURL(t, fast.URL+"/maven2/")}
	recreated.Remote = fast.URL + "/maven2"
	recreated.Gen = 2

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Fetch(ctx, old, "g/a.jar", Content)
	}()
	time.Sleep(100 * time.Millisecond) // the old flight is running
	res, err := f.Fetch(ctx, recreated, "g/a.jar", Content)
	if err != nil || read(t, f, res.Asset) != "new remote" {
		t.Fatalf("a re-created repository must fetch from its own remote: %+v %v", res, err)
	}
	<-done
}

func TestFetchSkipsCacheWriteForReplacedRepository(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setup(t, rm)
	rp.Gen = 1
	stale := *rp
	f.Store.Live = func(r *repo.Repository) bool { return r.Gen == 2 }
	_, err := f.Fetch(ctx, &stale, "g/a.jar", Content)
	if err == nil {
		t.Fatal("fetching for a replaced repository must not succeed silently")
	}
	if _, gerr := f.Store.Get(ctx, "p", "g/a.jar"); !errors.Is(gerr, repo.ErrNotFound) {
		t.Fatalf("asset was cached for a replaced repository: %v", gerr)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A 404 answered for a repository that was replaced meanwhile (remote changed,
// deleted or re-created) must not leave a negative-cache entry behind.
func TestFetchSkipsNegativeWriteForReplacedRepository(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setup(t, rm)
	rp.Gen = 1
	stale := *rp
	f.Store.Live = func(r *repo.Repository) bool { return r.Gen == 2 }
	if _, err := f.Fetch(ctx, &stale, "missing", Content); err == nil {
		t.Fatal("fetching for a replaced repository must not succeed silently")
	}
	if hit, err := f.Store.DB.NegativeHit(ctx, "p", "missing", f.Now()); err != nil || hit {
		t.Fatalf("negative entry recorded for a replaced repository: hit=%v err=%v", hit, err)
	}
}

// A failed fetch from a remote with credentials in its URL must not put them
// in the logs, neither in the log line nor in the in-memory ring.
func TestFetchFailureLogsNoRemoteSecrets(t *testing.T) {
	f, rp, _ := setup(t, &remote{})
	bad := *rp
	u, _ := url.Parse("http://user:pw@127.0.0.1:1/maven2/?token=x")
	bad.RemoteURL = u

	ring := logring.NewRing(100, 1<<20)
	var out strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(logring.NewHandler(slog.NewTextHandler(&out, nil), ring)))
	defer slog.SetDefault(prev)

	if _, err := f.Fetch(context.Background(), &bad, "g/a.jar", Content); err == nil {
		t.Fatal("fetch from an unreachable remote succeeded")
	}
	var all strings.Builder
	for _, r := range ring.Snapshot(slog.LevelDebug, "", 0) {
		all.WriteString(r.String() + "\n")
	}
	if !strings.Contains(all.String(), "remote fetch failed") {
		t.Fatalf("expected a warning in the ring: %q", all.String())
	}
	for name, text := range map[string]string{"ring": all.String(), "handler output": out.String()} {
		for _, secret := range []string{"user", "pw", "token=x"} {
			if strings.Contains(text, secret) {
				t.Fatalf("%q leaked into the %s: %s", secret, name, text)
			}
		}
	}
}

func TestFetchRemoteAddsHop(t *testing.T) {
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(HopHeader))
		io.WriteString(w, "x")
	}))
	defer ts.Close()
	f, _, _ := setup(t, &remote{})
	cfg := &config.Config{Repositories: []config.Repository{{Name: "a", Type: "proxy", Remote: ts.URL, ContentMaxAge: config.Never}}}
	rp := repo.Build(cfg)["a"]
	if _, err := f.Fetch(ctx, rp, "one", Content); err != nil {
		t.Fatal(err)
	}
	// An incoming chain (another instance proxying through this one) is kept.
	if _, err := f.Fetch(WithHops(ctx, "aaaa1111aaaa1111"), rp, "two", Content); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != InstanceID || got[1] != "aaaa1111aaaa1111, "+InstanceID {
		t.Fatalf("hop headers = %q (instance %s)", got, InstanceID)
	}
}

func TestHopSanitized(t *testing.T) {
	var ids []string
	for i := 0; i < 10; i++ {
		ids = append(ids, fmt.Sprintf("%016x", i+1))
	}
	in := "junk, ABCDEF0123456789, " + strings.Join(ids, ", ") + ", short, " + strings.Repeat("a", 4000)
	got := strings.Split(hopValue(WithHops(ctx, in)), ", ")
	if len(got) != 8 || got[7] != InstanceID || got[0] != ids[3] || got[6] != ids[9] {
		t.Fatalf("hops = %q", got)
	}
	if hopValue(WithHops(ctx, "bad!")) != InstanceID {
		t.Fatal("malformed ids must be dropped")
	}
}
