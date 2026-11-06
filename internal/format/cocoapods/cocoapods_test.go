package cocoapods

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/server"
)

const (
	archive = "PK\x03\x04fake-zip-bytes"
	tarball = "\x1f\x8bfake-tarball-bytes"
)

type env struct {
	t        *testing.T
	ts       *httptest.Server
	h        *Handler
	store    *repo.Store
	db       *meta.DB
	sink     *repotest.Sink
	registry *repo.Registry
	now      atomic.Int64 // the fetcher's clock, unix seconds
	cdn      atomic.Int32 // CDN requests
	zips     atomic.Int32 // archive host requests
	archives string       // the archive host's URL

	specMu       sync.Mutex
	specOverride map[string]string // what trunk now says for a podspec path
}

// newEnv serves the proxy pods of a fake trunk CDN (index files, the
// podspecs of Alamofire 5.0.0 with an http source, GitPod 1.0.0 with a
// GitHub source, SvnPod 1.0.0 with a source that passes through and Bad
// 1.0.0 whose sha256 does not match its archive) and a separate archive host,
// which also stands in for the git hosts, through the real server. Anonymous
// callers may read.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvRule(t, "")
}

// newEnvRule is newEnv with the routing rule the proxy names.
func newEnvRule(t *testing.T, rule string) *env {
	t.Helper()
	e := &env{t: t}
	zips := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.zips.Add(1)
		switch r.URL.Path {
		case "/Alamofire-5.0.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			io.WriteString(w, archive)
		case "/x/GitPod/archive/1.0.0.tar.gz":
			w.Header().Set("Content-Type", "application/x-gzip")
			io.WriteString(w, tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(zips.Close)
	e.archives = zips.URL
	sum := sha256.Sum256([]byte(archive))
	specs := map[string]string{
		SpecPath("Alamofire", "5.0.0"):       `{"name":"Alamofire","version":"5.0.0","summary":"Elegant HTTP","license":"MIT","homepage":"https://alamofire.org","source":{"http":"` + zips.URL + `/Alamofire-5.0.0.zip","sha256":"` + hex.EncodeToString(sum[:]) + `","flatten":true},"dependencies":{"Foo":["~> 1.0"],"Bar":[]}}`,
		SpecPath("GitPod", "1.0.0"):          `{"name":"GitPod","version":"1.0.0","source":{"git":"https://github.com/x/GitPod.git","tag":"1.0.0"},"license":{"type":"Apache-2.0"}}`,
		SpecPath("SvnPod", "1.0.0"):          `{"name":"SvnPod","version":"1.0.0","source":{"svn":"https://svn.example/x","tag":"1.0.0"}}`,
		SpecPath("!ProtoCompiler", "3.21.5"): `{"name":"!ProtoCompiler","version":"3.21.5","source":{"http":"` + zips.URL + `/protoc.zip"}}`,
		SpecPath("Bad", "1.0.0"):             `{"name":"Bad","version":"1.0.0","source":{"http":"` + zips.URL + `/Alamofire-5.0.0.zip","sha256":"` + strings.Repeat("0", 64) + `"}}`,
	}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.cdn.Add(1)
		p := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case p == "CocoaPods-version.yml":
			w.Header().Set("ETag", `"v1"`)
			io.WriteString(w, "---\nmin: 1.0.0\nlast: 1.15.2\nprefix_lengths:\n- 1\n- 1\n- 1\n")
		case p == "all_pods.txt" || p == "deprecated_podspecs.txt" || strings.HasPrefix(p, "all_pods_versions_"):
			io.WriteString(w, "Alamofire/5.0.0\n")
		case specs[p] != "":
			w.Header().Set("Content-Type", "application/json")
			e.specMu.Lock()
			defer e.specMu.Unlock()
			if b, ok := e.specOverride[p]; ok {
				io.WriteString(w, b)
				return
			}
			io.WriteString(w, specs[p])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cdn.Close)
	dir := t.TempDir()
	bs, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(metatest.URL(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{
		BaseURL:       "http://cc.example",
		AnonymousRead: true,
		Repositories: []config.Repository{
			{Name: "pods", Format: "cocoapods", Type: "proxy", Remote: cdn.URL + "/", RoutingRule: rule,
				MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Hour)},
		},
	}
	if err := config.ValidateRepositories(cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	e.sink = &repotest.Sink{}
	e.now.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(e.now.Load(), 0) }
	e.store = &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: db, Now: now, Events: e.sink}
	e.db = db
	f := proxy.New(e.store, proxy.NewClient(), 4)
	f.Now = now
	e.h = New(e.store, f, func() string { return cfg.BaseURL })
	e.h.gitHosts = map[string]string{"github.com": zips.URL, "bitbucket.org": zips.URL, "gitlab.com": zips.URL}
	e.registry = repo.NewRegistry(nil, cfg.Repositories)
	f.RouteAllowed = e.registry.RouteAllowed
	if err := e.registry.SetRoutingRules([]meta.RoutingRule{
		{Name: "repo-paths", Mode: meta.RoutingAllow, Matchers: []string{`^/[^/]+\.txt$`, `^/[^/]+\.yml$`, `^/Specs/.*`, `^/pods/.*`}},
		{Name: "no-alamofire", Mode: meta.RoutingBlock, Matchers: []string{`^/pods/Alamofire/.*`}},
	}); err != nil {
		t.Fatal(err)
	}
	srv := &server.Server{Registry: e.registry, Handlers: map[string]repo.Handler{"cocoapods": e.h}, Auth: auth.New(cfg, db),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *env) get(method, p string) (*http.Response, string) {
	e.t.Helper()
	r, _ := http.NewRequest(method, e.ts.URL+"/repository/pods/"+p, nil)
	resp, err := e.ts.Client().Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func ref(u string) string {
	s := sha1.Sum([]byte(u))
	return hex.EncodeToString(s[:])
}

func TestIndexFilesAreMetadata(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"CocoaPods-version.yml", "all_pods.txt", "all_pods_versions_a_b_c.txt", "deprecated_podspecs.txt"} {
		if resp, b := e.get("GET", p); resp.StatusCode != 200 || b == "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
			t.Fatalf("%s: %d %q %q", p, resp.StatusCode, b, resp.Header.Get("Content-Type"))
		}
	}
	n := e.cdn.Load()
	e.get("GET", "CocoaPods-version.yml")
	if e.cdn.Load() != n {
		t.Fatal("fresh index fetched again")
	}
	e.now.Add(7200) // past metadataMaxAge: revalidated
	e.get("GET", "CocoaPods-version.yml")
	if e.cdn.Load() != n+1 {
		t.Fatal("expired index not revalidated")
	}
	if resp, _ := e.get("GET", "nothing.txt"); resp.StatusCode != 404 {
		t.Fatalf("missing index: %d", resp.StatusCode)
	}
	if resp, _ := e.get("GET", "other/thing.txt"); resp.StatusCode != 404 {
		t.Fatalf("unknown path: %d", resp.StatusCode)
	}
	w := httptest.NewRecorder()
	e.h.Serve(w, httptest.NewRequest("PUT", "/all_pods.txt", nil), e.registry.Get("pods"), "all_pods.txt")
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("PUT: %d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestPodspecRewriteAndArchive(t *testing.T) {
	e := newEnv(t)
	resp, b := e.get("GET", SpecPath("Alamofire", "5.0.0"))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctJSON {
		t.Fatalf("podspec: %d %s", resp.StatusCode, b)
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(b), &spec); err != nil {
		t.Fatal(err)
	}
	src := spec["source"].(map[string]any)
	orig := e.archives + "/Alamofire-5.0.0.zip"
	want := "http://cc.example/repository/pods/pods/Alamofire/5.0.0/" + ref(orig) + ".zip"
	if src["http"] != want || src["flatten"] != true || src["sha256"] == nil || spec["summary"] != "Elegant HTTP" {
		t.Fatalf("rewritten source: %v", src)
	}
	// A wrong shard, pod or version is not found (and not asked upstream twice).
	if resp, _ := e.get("GET", "Specs/0/0/0/Alamofire/5.0.0/Alamofire.podspec.json"); resp.StatusCode != 404 {
		t.Fatalf("wrong shard: %d", resp.StatusCode)
	}
	if resp, _ := e.get("GET", SpecPath("Alamofire", "9.9.9")); resp.StatusCode != 404 {
		t.Fatalf("missing version: %d", resp.StatusCode)
	}

	// A pod name may start with "!" (gRPC's).
	_, b = e.get("GET", SpecPath("!ProtoCompiler", "3.21.5"))
	if !strings.Contains(b, `"http":"http://cc.example/repository/pods/pods/!ProtoCompiler/3.21.5/`+ref(e.archives+"/protoc.zip")+`.zip"`) {
		t.Fatalf("!ProtoCompiler: %s", b)
	}

	// A source that is not served as an archive passes through untouched.
	_, b = e.get("GET", SpecPath("SvnPod", "1.0.0"))
	if err := json.Unmarshal([]byte(b), &spec); err != nil {
		t.Fatal(err)
	}
	if svn := spec["source"].(map[string]any); svn["svn"] != "https://svn.example/x" || svn["tag"] != "1.0.0" || svn["http"] != nil {
		t.Fatalf("svn source: %v", svn)
	}

	// The archive is fetched from the podspec's URL once, cached under the
	// path the client asks for, and served with its type.
	resp, b = e.get("GET", strings.TrimPrefix(want, "http://cc.example/repository/pods/"))
	if resp.StatusCode != 200 || b != archive || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("archive: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	e.get("GET", strings.TrimPrefix(want, "http://cc.example/repository/pods/"))
	if e.zips.Load() != 1 {
		t.Fatalf("archive host hits: %d", e.zips.Load())
	}
	if _, err := e.store.Get(context.Background(), "pods", "pods/Alamofire/5.0.0/"+ref(orig)+".zip"); err != nil {
		t.Fatalf("archive not cached at its request path: %v", err)
	}
	if resp, _ := e.get("HEAD", strings.TrimPrefix(want, "http://cc.example/repository/pods/")); resp.StatusCode != 200 || resp.ContentLength != int64(len(archive)) {
		t.Fatalf("HEAD archive: %d %d", resp.StatusCode, resp.ContentLength)
	}
	// Another sha1, another extension or a pod without an http source: 404.
	for _, p := range []string{"pods/Alamofire/5.0.0/" + strings.Repeat("a", 40) + ".zip", "pods/Alamofire/5.0.0/" + ref(orig) + ".tgz",
		"pods/SvnPod/1.0.0/" + ref("x") + ".zip", "pods/Alamofire/5.0.0/" + ref(orig), "pods/Alamofire/5.0.0"} {
		if resp, _ := e.get("GET", p); resp.StatusCode != 404 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	// A podspec sha256 that does not match the download is a 502, nothing cached.
	bad := "pods/Bad/1.0.0/" + ref(orig) + ".zip"
	if resp, _ := e.get("GET", bad); resp.StatusCode != 502 {
		t.Fatalf("bad sha256: %d", resp.StatusCode)
	}
}

// TestGitSourceIsATarball pins cocoapods-01: a git source on a known host is
// answered as a tarball through the repository, without the git keys the
// client's http downloader refuses, and the tarball is fetched from the host
// once.
func TestGitSourceIsATarball(t *testing.T) {
	e := newEnv(t)
	resp, b := e.get("GET", SpecPath("GitPod", "1.0.0"))
	if resp.StatusCode != 200 {
		t.Fatalf("podspec: %d %s", resp.StatusCode, b)
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(b), &spec); err != nil {
		t.Fatal(err)
	}
	up := e.archives + "/x/GitPod/archive/1.0.0.tar.gz"
	file := "pods/GitPod/1.0.0/" + ref(up) + ".tar.gz"
	src := spec["source"].(map[string]any)
	if len(src) != 1 || src["http"] != "http://cc.example/repository/pods/"+file {
		t.Fatalf("source: %v", src)
	}
	for range 2 {
		if resp, b := e.get("GET", file); resp.StatusCode != 200 || b != tarball {
			t.Fatalf("tarball: %d %q", resp.StatusCode, b)
		}
	}
	if e.zips.Load() != 1 {
		t.Fatalf("git host hits: %d", e.zips.Load())
	}
	if _, err := e.store.Get(context.Background(), "pods", file); err != nil {
		t.Fatalf("tarball not cached at its request path: %v", err)
	}
}

func TestGitTarball(t *testing.T) {
	h := &Handler{}
	for _, c := range []struct {
		src  map[string]any
		want string
	}{
		{map[string]any{"git": "https://github.com/Alamofire/Alamofire.git", "tag": "5.9.1"}, "https://github.com/Alamofire/Alamofire/archive/5.9.1.tar.gz"},
		{map[string]any{"git": "https://github.com/o/r", "commit": "abc", "tag": "1", "branch": "b"}, "https://github.com/o/r/archive/abc.tar.gz"},
		{map[string]any{"git": "https://github.com/o/r.git", "tag": "1", "branch": "b"}, "https://github.com/o/r/archive/1.tar.gz"},
		{map[string]any{"git": "https://github.com/o/r.git", "branch": "dev/next"}, "https://github.com/o/r/archive/dev/next.tar.gz"},
		{map[string]any{"git": "https://github.com/o/r.git"}, "https://github.com/o/r/archive/HEAD.tar.gz"},
		{map[string]any{"git": "https://GitHub.com/o/r.git/", "tag": "v1#2"}, "https://github.com/o/r/archive/v1%232.tar.gz"},
		{map[string]any{"git": "git://github.com/o/r.git", "tag": "1"}, "https://github.com/o/r/archive/1.tar.gz"},
		{map[string]any{"git": "ssh://git@github.com/o/r.git", "tag": "1"}, "https://github.com/o/r/archive/1.tar.gz"},
		{map[string]any{"git": "git@github.com:o/r.git", "tag": "1"}, "https://github.com/o/r/archive/1.tar.gz"},
		{map[string]any{"git": "https://www.github.com/o/r.git", "tag": "1", "submodules": false}, "https://github.com/o/r/archive/1.tar.gz"},
		{map[string]any{"git": "https://bitbucket.org/o/r.git", "tag": "1.0"}, "https://bitbucket.org/o/r/get/1.0.tar.gz"},
		{map[string]any{"git": "https://bitbucket.org/o/r.git"}, "https://bitbucket.org/o/r/get/HEAD.tar.gz"},
		{map[string]any{"git": "https://gitlab.com/g/p.git", "tag": "1.0"}, "https://gitlab.com/g/p/-/archive/1.0/p-1.0.tar.gz"},
		{map[string]any{"git": "https://gitlab.com/g/sub/p.git", "branch": "a/b"}, "https://gitlab.com/g/sub/p/-/archive/a/b/p-a-b.tar.gz"},
		// These stay clones: another host, no repository path, a ref that
		// is no string, submodules a tarball does not hold, other sources.
		{map[string]any{"git": "https://git.example/o/r.git", "tag": "1"}, ""},
		{map[string]any{"git": "https://github.com/o", "tag": "1"}, ""},
		{map[string]any{"git": "https://github.com/o/r/x", "tag": "1"}, ""},
		{map[string]any{"git": "https://bitbucket.org/o/r/x", "tag": "1"}, ""},
		{map[string]any{"git": "https://github.com/o/r", "tag": 1.5}, ""},
		{map[string]any{"git": "https://github.com/o/r", "tag": ""}, ""},
		{map[string]any{"git": "https://github.com/o/r", "tag": "1", "submodules": true}, ""},
		{map[string]any{"git": "/local/path", "tag": "1"}, ""},
		{map[string]any{"svn": "https://github.com/o/r", "tag": "1"}, ""},
		{map[string]any{"hg": "https://bitbucket.org/o/r", "revision": "1"}, ""},
	} {
		if got := h.gitTarball(c.src); got != c.want {
			t.Errorf("%v: %q, want %q", c.src, got, c.want)
		}
	}
	// What the client decides by: the tarball's extension survives the rewrite.
	for _, u := range []string{"https://github.com/o/r/archive/v1.2.3.tar.gz", "https://gitlab.com/g/p/-/archive/1.0/p-1.0.tar.gz", "https://bitbucket.org/o/r/get/HEAD.tar.gz"} {
		if got := archiveExt(u); got != ".tar.gz" {
			t.Errorf("%s: %q", u, got)
		}
	}
}

// TestArchiveCachedByAnEarlierVersion: an archive stored beside its podspec
// before archives moved to their request path is still what the client gets,
// and what a REST downloadUrl (cocoapods-02) leads to, without asking anyone.
func TestArchiveCachedByAnEarlierVersion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u := e.archives + "/Alamofire-5.0.0.zip"
	legacy := "Specs/" + Shard("Alamofire") + "/Alamofire/5.0.0/" + ref(u) + ".zip"
	if _, err := e.store.Put(ctx, "pods", legacy, "application/zip", strings.NewReader("cached earlier")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"pods/Alamofire/5.0.0/" + ref(u) + ".zip", legacy} {
		if resp, b := e.get("GET", p); resp.StatusCode != 200 || b != "cached earlier" || resp.Header.Get("Content-Type") != "application/zip" {
			t.Fatalf("%s: %d %q %q", p, resp.StatusCode, b, resp.Header.Get("Content-Type"))
		}
	}
	if e.zips.Load() != 0 || e.cdn.Load() != 0 {
		t.Fatalf("asked upstream: %d %d", e.zips.Load(), e.cdn.Load())
	}
	// Only what is cached: this path never goes upstream.
	other := "Specs/" + Shard("Alamofire") + "/Alamofire/5.0.0/" + strings.Repeat("a", 40) + ".zip"
	if resp, _ := e.get("GET", other); resp.StatusCode != 404 || e.zips.Load() != 0 || e.cdn.Load() != 0 {
		t.Fatalf("not cached: %d, upstream %d %d", resp.StatusCode, e.zips.Load(), e.cdn.Load())
	}
}

// TestRoutingRuleSeesRequestPaths pins cocoapods-03: a rule written for
// repository paths decides for archives too, whose upstream URL it cannot know.
func TestRoutingRuleSeesRequestPaths(t *testing.T) {
	e := newEnvRule(t, "repo-paths")
	file := "pods/Alamofire/5.0.0/" + ref(e.archives+"/Alamofire-5.0.0.zip") + ".zip"
	for _, p := range []string{"CocoaPods-version.yml", SpecPath("Alamofire", "5.0.0"), file} {
		if resp, b := e.get("GET", p); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, b)
		}
	}
	if resp, _ := e.get("GET", "other/thing"); resp.StatusCode != 403 {
		t.Fatalf("outside the rule: %d", resp.StatusCode)
	}
	e = newEnvRule(t, "no-alamofire")
	file = "pods/Alamofire/5.0.0/" + ref(e.archives+"/Alamofire-5.0.0.zip") + ".zip"
	if resp, _ := e.get("GET", file); resp.StatusCode != 403 {
		t.Fatalf("blocked archive: %d", resp.StatusCode)
	}
	if resp, _ := e.get("GET", SpecPath("Alamofire", "5.0.0")); resp.StatusCode != 200 {
		t.Fatalf("podspec of a blocked archive: %d", resp.StatusCode)
	}
}

// TestPodspecIsMetadata pins cocoapods-04: within metadataMaxAge no request
// goes upstream; after it what trunk says now is served.
func TestPodspecIsMetadata(t *testing.T) {
	e := newEnv(t)
	p := SpecPath("Alamofire", "5.0.0")
	e.get("GET", p)
	n := e.cdn.Load()
	e.get("GET", p)
	if e.cdn.Load() != n {
		t.Fatal("fresh podspec fetched again")
	}
	e.specMu.Lock()
	e.specOverride = map[string]string{p: `{"name":"Alamofire","version":"5.0.0","deprecated":true,"source":{"git":"https://github.com/x/GitPod.git","tag":"1.0.0"}}`}
	e.specMu.Unlock()
	e.now.Add(7200)
	resp, b := e.get("GET", p)
	if e.cdn.Load() != n+1 || resp.StatusCode != 200 || !strings.Contains(b, `"deprecated":true`) {
		t.Fatalf("expired podspec: %d upstream, %d %s", e.cdn.Load()-n, resp.StatusCode, b)
	}
}

func TestArchiveExt(t *testing.T) {
	for u, want := range map[string]string{
		"https://x/a.zip": ".zip", "https://x/a-1.0.tar.gz": ".tar.gz", "https://x/a.tgz": ".tgz", "https://x/a.tar.bz2": ".tar.bz2",
		"https://x/a?x=1": ".bin", "https://x/archive/v1.0.0.zip?download": ".zip", "https://x/a.something-very-long": ".bin",
	} {
		if got := archiveExt(u); got != want {
			t.Errorf("%s: %q, want %q", u, got, want)
		}
	}
}

func TestProxyOnly(t *testing.T) {
	for _, def := range []config.Repository{
		{Name: "h", Format: "cocoapods", Type: "hosted"},
		{Name: "g", Format: "cocoapods", Type: "group", Members: []string{"p"}},
	} {
		defs := []config.Repository{def, {Name: "p", Format: "cocoapods", Type: "proxy", Remote: "https://cdn.cocoapods.org/"}}
		err := config.ValidateRepositories(defs)
		if err == nil || !strings.Contains(err.Error(), `repository "`+def.Name+`": cocoapods supports proxy repositories only`) {
			t.Fatalf("%s: %v", def.Type, err)
		}
	}
	if err := config.ValidateRepositories([]config.Repository{{Name: "p", Format: "cocoapods", Type: "proxy", Remote: "https://cdn.cocoapods.org/"}}); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	hosted := &repo.Repository{Repository: config.Repository{Name: "x", Format: "cocoapods", Type: "hosted"}}
	w := httptest.NewRecorder()
	e.h.Serve(w, httptest.NewRequest("GET", "/all_pods.txt", nil), hosted, "all_pods.txt")
	if w.Code != 400 {
		t.Fatalf("hosted: %d", w.Code)
	}
}

func TestComponentsCleanupAndPrivileges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	orig := e.archives + "/Alamofire-5.0.0.zip"
	e.get("GET", SpecPath("Alamofire", "5.0.0"))
	e.get("GET", "pods/Alamofire/5.0.0/"+ref(orig)+".zip")
	e.get("GET", SpecPath("GitPod", "1.0.0")) // read, not installed: no component
	e.get("GET", "all_pods.txt")
	for p, want := range map[string]string{
		"pods/Alamofire/5.0.0/" + ref(orig) + ".zip":                             "Alamofire@5.0.0",
		"Specs/" + Shard("Alamofire") + "/Alamofire/5.0.0/" + ref(orig) + ".zip": "Alamofire@5.0.0", // cached by an earlier version
		SpecPath("Alamofire", "5.0.0"):                                           "",
		SpecPath("GitPod", "1.0.0"):                                              "",
		"all_pods.txt":                                                           "", "CocoaPods-version.yml": "", "Specs/x/y/z/A/1/a.zip": "", "Specs/a/b/c/A/1": "", "pods/A/1": "", "pods/A/1/x/y": "",
	} {
		g, n, v, ok := component.DeriveAsset("cocoapods", p)
		got := ""
		if ok {
			got = n + "@" + v
		}
		if got != want || g != "" {
			t.Errorf("%s: %q %q, want %q", p, g, got, want)
		}
	}
	rp := e.registry.Get("pods")
	e.now.Add(10 * 86400)
	ev := &cleanup.Evaluator{DB: e.db, Now: e.store.Now}
	pol := cleanup.Policy{Name: "old", Format: "cocoapods", Criteria: cleanup.Criteria{ComponentAgeDays: 5}}
	if err := pol.Validate(); err != nil {
		t.Fatal(err)
	}
	res, err := ev.EvaluateFrom(ctx, rp, pol, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Label() != "Alamofire@5.0.0" || len(res.Candidates[0].Assets) != 1 {
		t.Fatalf("candidates: %+v", res.Candidates)
	}
	deleters := cleanup.NewRegistry(e.store)
	r, err := deleters.Delete(ctx, rp, res.Candidates[0].Component, "system")
	if err != nil || r.Assets != 1 {
		t.Fatalf("delete: %+v %v", r, err)
	}
	if _, err := e.store.Get(ctx, "pods", "pods/Alamofire/5.0.0/"+ref(orig)+".zip"); err == nil {
		t.Fatal("archive still cached")
	}
	if s := e.sink.String(); !strings.Contains(s, "delete|system|pods|Alamofire@5.0.0") {
		t.Fatalf("events: %s", s)
	}
	// The metadata stays: the podspec and the index files are no component's.
	for _, p := range []string{"all_pods.txt", SpecPath("Alamofire", "5.0.0")} {
		if _, err := e.store.Get(ctx, "pods", p); err != nil {
			t.Fatalf("%s deleted", p)
		}
	}
	// Fetched again on demand.
	if resp, _ := e.get("GET", "pods/Alamofire/5.0.0/"+ref(orig)+".zip"); resp.StatusCode != 200 || e.zips.Load() != 2 {
		t.Fatalf("refetch: %d, %d archive host hits", resp.StatusCode, e.zips.Load())
	}

	// format:cocoapods grants on cocoapods repositories only.
	if err := e.db.CreateRole(ctx, meta.Role{ID: "podreader", Privileges: []meta.Privilege{{Target: "format:cocoapods", Actions: []string{meta.ActionRead, meta.ActionDelete}}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.CreateUser(ctx, meta.User{ID: "dev", PasswordHash: "x", Roles: []string{"podreader"}}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Open(ctx, e.db, e.db)
	if err != nil {
		t.Fatal(err)
	}
	a.RepoFormat = func(name string) string { return map[string]string{"pods": "cocoapods", "hf": "huggingface"}[name] }
	p, _ := a.Lookup("dev")
	if !a.Can(p, "pods", auth.Read) || !a.Can(p, "pods", auth.Delete) || a.Can(p, "hf", auth.Delete) || a.Can(p, "pods", auth.Add) {
		t.Fatal("format:cocoapods privilege")
	}
}

func TestParseMetadata(t *testing.T) {
	m, err := ParseMetadata([]byte(`{"summary":"S","description":"D","license":{"type":"MIT","file":"LICENSE"},"homepage":"https://h","source":{"git":"https://g","tag":"1"},"dependencies":{"A":["~> 1.0",">= 0.9"],"B":[]}}`))
	if err != nil || m.Summary != "S" || m.License != "MIT" || m.Homepage != "https://h" || m.Source != "https://g" || len(m.Dependencies) != 2 || m.Dependencies["A"][1] != ">= 0.9" {
		t.Fatalf("%+v %v", m, err)
	}
	if m, _ := ParseMetadata([]byte(`{"license":"BSD","source":{"http":"https://x/a.zip"}}`)); m.License != "BSD" || m.Source != "https://x/a.zip" {
		t.Fatalf("%+v", m)
	}
}
