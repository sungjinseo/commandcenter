package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo"
)

// Auto-block, previewed through the Now hook: three consecutive failures
// block the remote for 30 s (cached content is served stale, the rest is a
// 502 without a remote request); a failure after the back-off blocks it
// again for twice as long; an answer, even a 404, clears it.
func TestAutoBlock(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	if _, err := f.Fetch(ctx, rp, "g/cached.jar", Metadata); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(2 * time.Hour) // the metadata has expired
	rm.status.Store(503)
	for i := 0; i < autoBlockAfter; i++ {
		if _, err := f.Fetch(ctx, rp, "g/x.jar", Content); !errors.Is(err, ErrRemote) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	hits := rm.hits.Load()
	st := f.Remote(rp)
	if !st.Blocked || !st.Auto || !st.Until.Equal(c.t.Add(30*time.Second)) || st.LastError != "remote status 503" {
		t.Fatalf("status %+v", st)
	}
	if _, err := f.Fetch(ctx, rp, "g/y.jar", Content); repo.StatusOf(err) != http.StatusBadGateway {
		t.Fatalf("blocked: %v", err)
	}
	if res, err := f.Fetch(ctx, rp, "g/cached.jar", Metadata); err != nil || !res.Stale {
		t.Fatalf("stale: %+v %v", res, err)
	}
	if rm.hits.Load() != hits {
		t.Fatalf("a blocked remote was asked: %d hits, want %d", rm.hits.Load(), hits)
	}
	// The back-off has passed: one more failure blocks again, for 60 s.
	c.t = c.t.Add(31 * time.Second)
	f.Fetch(ctx, rp, "g/x.jar", Content)
	if st := f.Remote(rp); !st.Blocked || !st.Until.Equal(c.t.Add(time.Minute)) || rm.hits.Load() != hits+1 {
		t.Fatalf("second block: %+v hits %d", st, rm.hits.Load())
	}
	for range 5 { // doubling stops at 5 minutes
		c.t = c.t.Add(10 * time.Minute)
		f.Fetch(ctx, rp, "g/x.jar", Content)
	}
	if st := f.Remote(rp); !st.Until.Equal(c.t.Add(5 * time.Minute)) {
		t.Fatalf("cap: %+v", st)
	}
	// A 404 is an answer: the remote is reachable again.
	c.t = c.t.Add(10 * time.Minute)
	rm.status.Store(0)
	if _, err := f.Fetch(ctx, rp, "missing", Content); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if st := f.Remote(rp); st.Blocked || st.LastError != "" {
		t.Fatalf("after an answer: %+v", st)
	}
	rm.status.Store(503)
	f.Fetch(ctx, rp, "g/z1.jar", Content)
	f.Fetch(ctx, rp, "g/z2.jar", Content)
	if st := f.Remote(rp); st.Blocked { // the count restarted
		t.Fatalf("blocked after two failures: %+v", st)
	}
}

// autoBlock false never blocks; blocked never asks the remote.
func TestAutoBlockOffAndManualBlock(t *testing.T) {
	rm := &remote{}
	f, rp, _ := setup(t, rm)
	no := false
	rp.AutoBlock = &no
	rm.status.Store(500)
	for range 5 {
		f.Fetch(ctx, rp, "g/x.jar", Content)
	}
	if st := f.Remote(rp); st.Blocked || rm.hits.Load() != 5 {
		t.Fatalf("autoBlock off: %+v hits %d", st, rm.hits.Load())
	}
	rp.Blocked = true
	if _, err := f.Fetch(ctx, rp, "g/y.jar", Content); repo.StatusOf(err) != http.StatusBadGateway || rm.hits.Load() != 5 {
		t.Fatalf("manual block: %v hits %d", err, rm.hits.Load())
	}
	if st := f.Remote(rp); !st.Blocked || st.Auto {
		t.Fatalf("manual status %+v", st)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/x", strings.NewReader("{}"))
	if err := f.Forward(ctx, rp, w, r, "audit", 1<<20); !errors.Is(err, ErrRemote) || rm.hits.Load() != 5 {
		t.Fatalf("forward to a blocked remote: %v", err)
	}
}

// A connection error counts as a failure, like a 5xx.
func TestAutoBlockConnectionError(t *testing.T) {
	f, rp, _ := setup(t, &remote{})
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close() // nothing listens there any more
	u := *rp.RemoteURL
	u.Host = strings.TrimPrefix(ts.URL, "http://")
	rp.RemoteURL = &u
	for range autoBlockAfter {
		f.Fetch(ctx, rp, "g/x.jar", Content)
	}
	if st := f.Remote(rp); !st.Blocked || !strings.Contains(st.LastError, "refused") || strings.Contains(st.LastError, "127.0.0.1") {
		t.Fatalf("status %+v", st)
	}
}
