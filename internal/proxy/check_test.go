package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// A read of a cached document is its last use, whether it was fresh, or could
// not be refreshed (a long outage, a blocked proxy): repository.purge-unused goes
// by it. At most once per usedEvery, and a file of content is counted when it is
// served, not here.
func TestFetchRecordsUseOfDocuments(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	used := map[string]int{}
	f.Store.Used = func(repo, path string) { used[repo+"/"+path]++ }
	if _, err := f.Fetch(ctx, rp, "g/doc", Metadata); err != nil { // fetched, not read from the cache
		t.Fatal(err)
	}
	if _, err := f.Fetch(ctx, rp, "g/a.jar", Content); err != nil {
		t.Fatal(err)
	}
	if len(used) != 0 {
		t.Fatalf("used: %v", used)
	}
	if _, err := f.Fetch(ctx, rp, "g/doc", Metadata); err != nil { // fresh
		t.Fatal(err)
	}
	f.Fetch(ctx, rp, "g/a.jar", Content)
	if used["p/g/doc"] != 1 || len(used) != 1 {
		t.Fatalf("used: %v", used)
	}
	c.t = c.t.Add(2 * time.Hour)
	rm.status.Store(503)
	if res, err := f.Fetch(ctx, rp, "g/doc", Metadata); err != nil || !res.Stale {
		t.Fatalf("%+v %v", res, err)
	}
	if used["p/g/doc"] != 2 {
		t.Fatalf("stale: %v", used)
	}
	if err := f.Store.DB.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "p", Path: "g/doc", Last: c.t}}); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(usedEvery - time.Minute)
	f.Fetch(ctx, rp, "g/doc", Metadata)
	if used["p/g/doc"] != 2 {
		t.Fatalf("within usedEvery: %v", used)
	}
	a, err := f.Store.Get(ctx, "p", "g/doc")
	if err != nil || a.Downloads != 0 || a.LastDownloaded.IsZero() {
		t.Fatalf("a use is no download: %+v %v", a, err)
	}
}

// Target.Check refuses an answer before anything is recorded: no row, no
// commit (so no webhook and nothing in the journal), and no miss either, so the
// next good answer is served at once. The fetch fails as when the remote fails:
// a cached copy, untouched, as stale; else ErrRemote, or the refusal when it is
// a 502, which says why.
func TestFetchTargetCheckRefuses(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	var commits []meta.Change
	f.Store.DB.OnCommit = func(_ context.Context, _ time.Time, cs []meta.Change) { commits = append(commits, cs...) }
	seen := ""
	refusal := errors.New("the remote's own words")
	tg := Target{Key: "g/a", Remote: "g/a", Check: func(_ context.Context, body io.ReadSeeker, s blob.Sums) (blob.Sums, error) {
		b, err := io.ReadAll(body)
		if err != nil || s.Size != int64(len(b)) {
			t.Errorf("body %q, sums %+v, %v", b, s, err)
		}
		seen = string(b)
		return s, refusal
	}}
	if _, err := f.FetchTarget(ctx, rp, tg, Metadata); !errors.Is(err, ErrRemote) {
		t.Fatalf("refused, not cached: %v", err)
	}
	if seen != "body of /maven2/g/a" {
		t.Fatalf("the check saw %q", seen)
	}
	if _, err := f.Store.Get(ctx, "p", "g/a"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("a refused answer was recorded: %v", err)
	}
	if hit, err := f.Store.DB.NegativeHit(ctx, "p", "g/a", c.t); hit || err != nil {
		t.Fatalf("a refused answer was recorded as a miss: %v %v", hit, err)
	}
	if len(commits) != 0 {
		t.Fatalf("commits: %+v", commits)
	}
	refusal = repo.Errorf(http.StatusBadGateway, "the archive does not match")
	if _, err := f.FetchTarget(ctx, rp, tg, Metadata); err == nil || err.Error() != refusal.Error() {
		t.Fatalf("a 502 says why: %v", err)
	}

	// The copy cached before stays as it was, and answers as stale.
	good := Target{Key: "g/a", Remote: "g/a"}
	if _, err := f.FetchTarget(ctx, rp, good, Metadata); err != nil {
		t.Fatal(err)
	}
	before, err := f.Store.Get(ctx, "p", "g/a")
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(2 * time.Hour)
	before.RemoteETag = "" // so the remote answers 200, not 304
	if err := f.Store.DB.PutAsset(ctx, before); err != nil {
		t.Fatal(err)
	}
	before, _ = f.Store.Get(ctx, "p", "g/a")
	commits = nil
	res, err := f.FetchTarget(ctx, rp, tg, Metadata)
	if err != nil || !res.Stale || res.Asset.BlobSHA256 != before.BlobSHA256 {
		t.Fatalf("refused, cached: %+v %v", res, err)
	}
	after, _ := f.Store.Get(ctx, "p", "g/a")
	if after != before || len(commits) != 0 {
		t.Fatalf("the row changed: %+v, was %+v; commits %+v", after, before, commits)
	}
	w := httptest.NewRecorder()
	SetStaleHeader(w, res)
	if w.Header().Get("Warning") == "" {
		t.Fatal("no Warning on the stale copy")
	}
}

// Target.Check can store something else than the answer: the row names that
// blob, with the remote's validators, so that a 304 keeps it. The answer as
// it came is never recorded.
func TestFetchTargetCheckReplaces(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	var commits []meta.Change
	f.Store.DB.OnCommit = func(_ context.Context, _ time.Time, cs []meta.Change) { commits = append(commits, cs...) }
	tg := Target{Key: "g/a", Remote: "g/a", Check: func(_ context.Context, body io.ReadSeeker, _ blob.Sums) (blob.Sums, error) {
		b, _ := io.ReadAll(body)
		return f.Store.WriteBlob("p", strings.NewReader("rewritten "+string(b)), nil)
	}}
	res, err := f.FetchTarget(ctx, rp, tg, Metadata)
	if err != nil || read(t, f, res.Asset) != "rewritten body of /maven2/g/a" || res.Asset.RemoteETag != `"v1"` {
		t.Fatalf("%+v %v", res, err)
	}
	if len(commits) != 1 || commits[0].Asset.SHA256 != res.Asset.BlobSHA256 {
		t.Fatalf("commits: %+v", commits)
	}
	c.t = c.t.Add(2 * time.Hour)
	res, err = f.FetchTarget(ctx, rp, tg, Metadata) // a 304
	if err != nil || res.Stale || read(t, f, res.Asset) != "rewritten body of /maven2/g/a" {
		t.Fatalf("after a 304: %+v %v", res, err)
	}
}

// The same key from two places (npm's tarball at the usual path, then where
// the document says) is two downloads, not one that answers for both.
func TestFetchFlightPerURL(t *testing.T) {
	rm := &remote{delay: 500 * time.Millisecond}
	f, rp, _ := setup(t, rm)
	var wg sync.WaitGroup
	for _, tg := range []Target{{Key: "k", Remote: "g/a"}, {Key: "k", URL: rp.RemoteURL.String() + "/g/b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.FetchTarget(ctx, rp, tg, Content)
		}()
	}
	wg.Wait()
	if rm.hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", rm.hits.Load())
	}
}

// JSONDocument tells a document from what a remote says instead, as a
// stream. With keys it reads no further than the first of them.
func TestJSONDocument(t *testing.T) {
	for _, c := range []struct {
		body string
		keys []string
		ok   bool
	}{
		{`{"a":[1,{"b":null}],"c":"d"}`, nil, true},
		{`["a"]`, nil, true},
		{"\xef\xbb\xbf{\"a\":1}", nil, true},
		{`{"n":1e999}`, nil, true},
		{"<!DOCTYPE html><html>Sign in</html>", nil, false},
		{"Sign in", nil, false},
		{"", nil, false},
		{`"a string"`, nil, false},
		{`123`, nil, false},
		{`{"a":1`, nil, false},
		{`{"a":1} and more`, nil, false},
		{strings.Repeat("[", jsonCheckDepth+1), nil, false},
		{`{"a":1,"versions":[]}`, []string{"versions"}, true},
		{`{"modules":[]}`, []string{"versions", "modules"}, true},
		{`{"a":{"versions":[]},"b":[{"versions":1}],"versions":2}`, []string{"versions"}, true},
		{`{"versions":[] <html>`, []string{"versions"}, true}, // no further than the key
		{`{"error":"maintenance"}`, []string{"versions"}, false},
		{`{"a":{"versions":[]}}`, []string{"versions"}, false},
		{`{"a":"versions"}`, []string{"versions"}, false},
		{`["versions"]`, []string{"versions"}, false},
		{"<html>", []string{"versions"}, false},
	} {
		_, err := JSONDocument(c.keys...)(ctx, strings.NewReader(c.body), blob.Sums{})
		if (err == nil) != c.ok {
			t.Errorf("%.40q with %v: %v", c.body, c.keys, err)
		}
	}
	// A document past what is read is not judged by what it was not read of.
	long := `{"a":"` + strings.Repeat("x", jsonCheckMax) + `"}`
	if _, err := JSONDocument()(ctx, strings.NewReader(long), blob.Sums{}); err != nil {
		t.Errorf("a long document: %v", err)
	}
	if _, err := JSONDocument("versions")(ctx, strings.NewReader(`{"versions":[`+strings.Repeat(`"1",`, jsonCheckMax)), blob.Sums{}); err != nil {
		t.Errorf("a key at the start of a long document: %v", err)
	}
}
