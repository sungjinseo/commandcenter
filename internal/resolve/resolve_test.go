package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

var ctx = context.Background()

func setup(t *testing.T, remote http.HandlerFunc) (*Resolver, map[string]*repo.Repository) {
	t.Helper()
	ts := httptest.NewServer(remote)
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.URL(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: time.Now}
	cfg := &config.Config{Repositories: []config.Repository{
		{Name: "h1", Format: "maven", Type: "hosted"},
		{Name: "h2", Format: "maven", Type: "hosted"},
		{Name: "p", Format: "maven", Type: "proxy", Remote: ts.URL, ContentMaxAge: config.Never, MetadataMaxAge: config.Duration(time.Hour), NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "inner", Format: "maven", Type: "group", Members: []string{"h2", "p"}},
		{Name: "g", Format: "maven", Type: "group", Members: []string{"h1", "inner", "h2"}},
	}}
	return &Resolver{Store: store, Fetcher: proxy.New(store, ts.Client(), 4)}, repo.Build(cfg)
}

func put(t *testing.T, r *Resolver, repoName, path, body string) {
	t.Helper()
	if _, err := r.Store.Put(ctx, repoName, path, "text/plain", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func content(t *testing.T, r *Resolver, a meta.Asset) string {
	t.Helper()
	f, err := r.Store.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

func remoteServing(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shared" || r.URL.Path == "/only-remote" {
			io.WriteString(w, body)
			return
		}
		http.NotFound(w, r)
	}
}

func TestLookupGroupOrder(t *testing.T) {
	r, repos := setup(t, remoteServing("from-remote"))
	put(t, r, "h2", "shared", "from-h2")
	res, err := r.Lookup(ctx, repos["g"], "shared", proxy.Content)
	if err != nil || content(t, r, res.Asset) != "from-h2" {
		t.Fatalf("got %+v %v", res, err)
	}
	res, err = r.Lookup(ctx, repos["g"], "only-remote", proxy.Content)
	if err != nil || content(t, r, res.Asset) != "from-remote" {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, err := r.Lookup(ctx, repos["g"], "nowhere", proxy.Content); repo.StatusOf(err) != 404 {
		t.Fatalf("err = %v", err)
	}
}

func TestLookupSkipsFailingMember(t *testing.T) {
	r, repos := setup(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	put(t, r, "h2", "x", "from-h2")
	// inner = [h2, p]; p fails but h2 has it
	res, err := r.Lookup(ctx, repos["inner"], "x", proxy.Content)
	if err != nil || content(t, r, res.Asset) != "from-h2" {
		t.Fatalf("got %+v %v", res, err)
	}
	// nothing has y and p fails: the remote error is reported
	if _, err := r.Lookup(ctx, repos["inner"], "y", proxy.Content); repo.StatusOf(err) != 502 {
		t.Fatalf("err = %v", err)
	}
}

// Owner fails closed where Lookup (Maven, Go) skips: an offline member ahead
// of the one that has the asset is a 503, also inside a nested group (QA N-1).
func TestOwnerFailsClosedOnOfflineMember(t *testing.T) {
	r, repos := setup(t, remoteServing("from-remote"))
	put(t, r, "h2", "x", "from-h2")
	// g = h1, inner(h2, p), h2
	owner, res, err := r.Owner(ctx, repos["g"], pathTarget("x"), proxy.Content)
	if err != nil || owner.Name != "h2" || content(t, r, res.Asset) != "from-h2" {
		t.Fatalf("owner=%v err=%v", owner, err)
	}
	if owner, _, err := r.Owner(ctx, repos["g"], pathTarget("only-remote"), proxy.Content); err != nil || owner.Name != "p" {
		t.Fatalf("owner=%v err=%v", owner, err)
	}
	off := false
	repos["h2"].Online = &off
	for _, path := range []string{"x", "only-remote", "nowhere"} {
		if _, _, err := r.Owner(ctx, repos["g"], pathTarget(path), proxy.Content); repo.StatusOf(err) != http.StatusServiceUnavailable {
			t.Errorf("%s: err = %v, want 503", path, err)
		}
	}
	if res, err := r.Lookup(ctx, repos["g"], "only-remote", proxy.Content); err != nil || content(t, r, res.Asset) != "from-remote" {
		t.Fatalf("Lookup must keep skipping offline members: %v", err)
	}
}

// OwnerFile asks a member without the file for the document unless it is
// the last one: inner's last member p is not last in g, and its document
// "only-remote" makes it the owner, so the later h1's file is not served.
func TestOwnerFileStopsAtOwnerInNestedGroup(t *testing.T) {
	r, repos := setup(t, remoteServing("from-remote"))
	put(t, r, "h1", "f", "from-h1")
	g := *repos["g"]
	g.MemberRepos = []*repo.Repository{repos["inner"], repos["h1"]} // inner = h2, p
	if _, err := r.OwnerFile(ctx, &g, pathTarget("f"), pathTarget("only-remote")); repo.StatusOf(err) != http.StatusNotFound {
		t.Fatalf("p owns the package: err = %v, want 404", err)
	}
	if res, err := r.OwnerFile(ctx, &g, pathTarget("f"), pathTarget("nowhere")); err != nil || content(t, r, res.Asset) != "from-h1" {
		t.Fatalf("no owner ahead of h1: err = %v", err)
	}
	if res, err := r.OwnerFile(ctx, &g, pathTarget("shared"), pathTarget("only-remote")); err != nil || content(t, r, res.Asset) != "from-remote" {
		t.Fatalf("p has the file: err = %v", err)
	}
	off := false
	repos["h2"].Online = &off
	if _, err := r.OwnerFile(ctx, &g, pathTarget("f"), pathTarget("nowhere")); repo.StatusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("h2 offline ahead of h1: err = %v, want 503", err)
	}
}

func TestAllFlattensAndDedupes(t *testing.T) {
	r, repos := setup(t, remoteServing("R"))
	put(t, r, "h1", "shared", "1")
	put(t, r, "h2", "shared", "2")
	res, err := r.All(ctx, repos["g"], "shared", proxy.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range res {
		got = append(got, content(t, r, x.Asset))
	}
	if strings.Join(got, ",") != "1,2,R" { // g = h1, inner(h2, p), h2(dup)
		t.Fatalf("got %v", got)
	}
}

func TestMergedCachesUntilMembersChange(t *testing.T) {
	r, repos := setup(t, remoteServing("R"))
	put(t, r, "h1", "shared", "1")
	calls := 0
	merge := func(_ context.Context, in []io.ReadSeeker, w io.Writer) error {
		calls++
		var parts []string
		for _, x := range in {
			b, _ := io.ReadAll(x)
			parts = append(parts, string(b))
		}
		_, err := io.WriteString(w, strings.Join(parts, "+"))
		return err
	}
	a, err := r.Merged(ctx, repos["g"], "shared", "text/plain", merge)
	if err != nil || content(t, r, a) != "1+R" {
		t.Fatalf("got %q %v", content(t, r, a), err)
	}
	r.Merged(ctx, repos["g"], "shared", "text/plain", merge)
	if calls != 1 {
		t.Fatalf("merge calls = %d, want 1 (cached)", calls)
	}
	put(t, r, "h2", "shared", "2")
	a, _ = r.Merged(ctx, repos["g"], "shared", "text/plain", merge)
	if calls != 2 || content(t, r, a) != "1+2+R" {
		t.Fatalf("calls=%d content=%q", calls, content(t, r, a))
	}
	if _, err := r.Merged(ctx, repos["g"], "none", "text/plain", merge); repo.StatusOf(err) != 404 {
		t.Fatalf("err = %v", err)
	}
}

func TestMergedTargetReadsMembersTwiceAndStoresUnderKey(t *testing.T) {
	r, repos := setup(t, remoteServing("R"))
	put(t, r, "h1", "pkg/package.json", "1")
	merge := func(_ context.Context, in []io.ReadSeeker, w io.Writer) error {
		for pass := 0; pass < 2; pass++ { // members are re-readable
			for _, x := range in {
				if _, err := x.Seek(0, io.SeekStart); err != nil {
					return err
				}
				b, _ := io.ReadAll(x)
				w.Write(b)
			}
		}
		return nil
	}
	target := proxy.Target{Key: "pkg/package.json", Remote: "shared"} // remote serves "R" at /shared
	a, err := r.MergedTarget(ctx, repos["g"], target, "application/json", merge)
	if err != nil || content(t, r, a) != "1R1R" {
		t.Fatalf("got %q %v", content(t, r, a), err)
	}
	if _, err := r.Store.Get(ctx, "g", "pkg/package.json"); err != nil {
		t.Fatalf("merged asset not stored under key: %v", err)
	}
}

func TestMergedRecoversPanicAndStoresNothing(t *testing.T) {
	r, repos := setup(t, remoteServing("R"))
	put(t, r, "h1", "x", "1")
	put(t, r, "h2", "x", "2")
	_, err := r.Merged(ctx, repos["g"], "x", "text/plain", func(context.Context, []io.ReadSeeker, io.Writer) error {
		panic("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.Store.Get(ctx, "g", "x"); repo.StatusOf(err) != 404 {
		t.Fatalf("nothing must be stored, got %v", err)
	}
}

func TestMergedErrorIsNotCached(t *testing.T) {
	r, repos := setup(t, remoteServing("R"))
	put(t, r, "h1", "x", "1")
	put(t, r, "h2", "x", "2")
	_, err := r.Merged(ctx, repos["g"], "x", "text/plain", func(_ context.Context, _ []io.ReadSeeker, w io.Writer) error {
		io.WriteString(w, "partial")
		return errors.New("bad member")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, err := r.Store.Get(ctx, "g", "x"); repo.StatusOf(err) != 404 {
		t.Fatalf("failed merge must not be stored, got %v", err)
	}
}

// Decision 7: the single-member shortcut holds only while the group has no
// stored merged copy (TestGroupMetadataLastModifiedMonotonic covers the rest).
func TestMergedSingleMemberIsServedAsIs(t *testing.T) {
	r, repos := setup(t, remoteServing("R"))
	put(t, r, "h1", "solo", "only")
	a, err := r.Merged(ctx, repos["g"], "solo", "text/plain", func(context.Context, []io.ReadSeeker, io.Writer) error {
		t.Fatal("merge must not run for a single member")
		return nil
	})
	if err != nil || content(t, r, a) != "only" || a.Repo != "h1" {
		t.Fatalf("asset=%+v err=%v", a, err)
	}
	if _, err := r.Store.Get(ctx, "g", "solo"); repo.StatusOf(err) != 404 {
		t.Fatalf("nothing may be stored in the group: %v", err)
	}
}

// M-16 / Decision 7: once the group stored a merged copy, a member drop that
// leaves one result re-stores the document instead of serving the member's
// older one, so Last-Modified never moves backwards.
func TestGroupMetadataLastModifiedMonotonic(t *testing.T) {
	r, repos := setup(t, remoteServing("R"))
	t1 := time.Date(2026, 10, 22, 1, 37, 5, 0, time.UTC)
	now := t1.Add(-time.Hour)
	r.Store.Now = func() time.Time { return now }
	put(t, r, "h1", "doc", "1")
	put(t, r, "h2", "doc", "2")
	concat := func(_ context.Context, in []io.ReadSeeker, w io.Writer) error {
		for _, x := range in {
			b, _ := io.ReadAll(x)
			w.Write(b)
		}
		return nil
	}
	now = t1
	a, err := r.Merged(ctx, repos["g"], "doc", "text/plain", concat)
	if err != nil || content(t, r, a) != "12" || !a.LastModified.Equal(t1) {
		t.Fatalf("merged: %q %v %v", content(t, r, a), a.LastModified, err)
	}

	now = t1.Add(10 * time.Second)
	if err := r.Store.Delete(ctx, "h2", "doc"); err != nil {
		t.Fatal(err)
	}
	a, err = r.Merged(ctx, repos["g"], "doc", "text/plain", concat)
	if err != nil || content(t, r, a) != "1" {
		t.Fatalf("after drop: %q %v", content(t, r, a), err)
	}
	if a.LastModified.Before(t1) || a.Repo != "g" {
		t.Fatalf("Last-Modified went backwards or member served: %v repo=%s", a.LastModified, a.Repo)
	}
	req := httptest.NewRequest("GET", "/doc", nil)
	req.Header.Set("If-Modified-Since", t1.Format(http.TimeFormat))
	w := httptest.NewRecorder()
	repo.ServeAsset(w, req, r.Store, a)
	if w.Code != 200 || w.Body.String() != "1" {
		t.Fatalf("If-Modified-Since T1: %d %q", w.Code, w.Body)
	}
	// Unchanged members reuse the stored copy.
	now = t1.Add(time.Hour)
	b, err := r.Merged(ctx, repos["g"], "doc", "text/plain", concat)
	if err != nil || !b.LastModified.Equal(a.LastModified) {
		t.Fatalf("re-stored without a change: %v %v", b.LastModified, err)
	}
}

// TestMergedSaltReMergesOnlySaltedCallers: a salt makes copies stored with the
// old (unsalted) fingerprint re-merge once (plan 13 fix I-1); unsalted callers
// such as Maven keep the exact fingerprint older builds stored and are not
// re-merged.
func TestMergedSaltReMergesOnlySaltedCallers(t *testing.T) {
	r, repos := setup(t, remoteServing("R"))
	put(t, r, "h1", "shared", "1")
	calls := 0
	merge := func(_ context.Context, in []io.ReadSeeker, w io.Writer) error {
		calls++
		_, err := fmt.Fprintf(w, "merge-%d", calls)
		return err
	}
	a, err := r.Merged(ctx, repos["g"], "shared", "text/plain", merge)
	if err != nil || calls != 1 {
		t.Fatalf("first merge: calls=%d err=%v", calls, err)
	}
	// The unsalted fingerprint is the one older builds stored, byte for byte.
	results, err := r.All(ctx, repos["g"], "shared", proxy.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	for _, res := range results {
		io.WriteString(h, res.Asset.BlobSHA256+"\n")
	}
	if want := "group:" + hex.EncodeToString(h.Sum(nil)); a.RemoteETag != want {
		t.Fatalf("unsalted fingerprint %q, want %q", a.RemoteETag, want)
	}
	if _, err := r.Merged(ctx, repos["g"], "shared", "text/plain", merge); err != nil || calls != 1 {
		t.Fatalf("unsalted caller re-merged: calls=%d err=%v", calls, err)
	}

	target := proxy.Target{Key: "shared", Remote: "shared"}
	a, err = r.MergedTargetSalted(ctx, repos["g"], target, "text/plain", "v2;", merge)
	if err != nil || calls != 2 || content(t, r, a) != "merge-2" || !strings.HasPrefix(a.RemoteETag, "group:v2;") {
		t.Fatalf("salted: calls=%d %q %q %v", calls, content(t, r, a), a.RemoteETag, err)
	}
	if _, err := r.MergedTargetSalted(ctx, repos["g"], target, "text/plain", "v2;", merge); err != nil || calls != 2 {
		t.Fatalf("salted copy must be reused: calls=%d err=%v", calls, err)
	}
}
