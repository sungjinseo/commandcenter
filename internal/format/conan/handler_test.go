package conan

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
)

const ccBase = "http://cc.example"

var (
	zlib  = Ref{"zlib", "1.3", "_", "_"}
	fooMe = Ref{"foo", "1.0", "me", "stable"}
)

const (
	conanfilePy = `from conan import ConanFile

class ZlibConan(ConanFile):
    name = "zlib"
    description = """A massively spiffy yet delicately
    unobtrusive compression library"""
    license = ("Zlib", "MIT")
    homepage = "https://zlib.net"
    url = "https://github.com/conan-io/conan-center-index"
    requires = ("bzip2/1.0.8", "libpng/[>=1.6]")
    tool_requires = "cmake/3.27"

    def requirements(self):
        self.requires("openssl/3.2.0")
        self.tool_requires("ninja/1.11")
`
	conaninfoTxt = "[settings]\n    arch=x86_64\n    os=Linux\n[options]\n    shared=False\n[requires]\n    zlib/1.Y.Z\n[full_requires]\n    zlib/1.3:abc\n[recipe_hash]\n    deadbeef\n"
)

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func sha1hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// manifest is a conanmanifest.txt listing files.
func manifest(files map[string][]byte) []byte {
	var b strings.Builder
	b.WriteString("1700000000\n")
	for _, name := range slices.Sorted(maps.Keys(files)) { // sorted, as conan writes it: map order would vary the md5
		b.WriteString(name + ": " + md5hex(files[name]) + "\n")
	}
	return []byte(b.String())
}

// recipeFiles are the files of a recipe revision; the export tgz carries
// tag so revisions differ.
func recipeFiles(tag string) map[string][]byte {
	f := map[string][]byte{"conanfile.py": []byte(conanfilePy), "conan_export.tgz": []byte("export-" + tag)}
	f["conanmanifest.txt"] = manifest(f)
	return f
}

func packageFiles(tag string) map[string][]byte {
	f := map[string][]byte{"conaninfo.txt": []byte(conaninfoTxt), "conan_package.tgz": []byte("package-" + tag)}
	f["conanmanifest.txt"] = manifest(f)
	return f
}

// fakeCenter is a fake Conan Center: v2 documents and files of zlib/1.3
// (revision aaaa, package pkg1 revision p1) and boost/1.84.0, and search.
// Under /private/ every request needs the Bearer token the last login
// issued; logins need Basic bob:secret.
type fakeCenter struct {
	mu     sync.Mutex
	hits   map[string]int
	logins int
	token  string
}

func (f *fakeCenter) count(path string) {
	f.mu.Lock()
	f.hits[path]++
	f.mu.Unlock()
}

func (f *fakeCenter) hit(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeCenter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if rest, ok := strings.CutPrefix(p, "/private"); ok {
		if rest == "/v1/users/authenticate" {
			if u, pw, ok := r.BasicAuth(); !ok || u != "bob" || pw != "secret" {
				w.WriteHeader(401)
				return
			}
			f.mu.Lock()
			f.logins++
			f.token = "tok" + strings.Repeat("x", f.logins)
			tok := f.token
			f.mu.Unlock()
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(tok))
			return
		}
		f.mu.Lock()
		tok := f.token
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(401)
			return
		}
		p = rest
	}
	f.count(p + "?" + r.URL.RawQuery)
	j := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	rec, pkg := recipeFiles("center"), packageFiles("center")
	files := func(m map[string][]byte) map[string]any {
		out := map[string]any{}
		for n := range m {
			out[n] = map[string]any{}
		}
		return map[string]any{"files": out}
	}
	switch {
	case p == "/v1/ping":
		w.Header().Set("X-Conan-Server-Capabilities", "revisions")
	case p == "/v2/conans/search":
		q := strings.TrimSuffix(r.URL.Query().Get("q"), "*")
		var out []string
		for _, ref := range []string{"zlib/1.3", "boost/1.84.0"} {
			if strings.HasPrefix(ref, q) || q == "" {
				out = append(out, ref)
			}
		}
		j(map[string]any{"results": out})
	case p == "/v2/conans/zlib/1.3/_/_/latest" || p == "/v2/conans/boost/1.84.0/_/_/latest":
		j(map[string]any{"revision": "aaaa", "time": "2026-01-02T03:04:05.000+0000"})
	case p == "/v2/conans/zlib/1.3/_/_/revisions":
		j(map[string]any{"revisions": []any{map[string]any{"revision": "aaaa", "time": "2026-01-02T03:04:05.000+0000"}}})
	case p == "/v2/conans/zlib/1.3/_/_/revisions/aaaa/files":
		j(files(rec))
	case strings.HasPrefix(p, "/v2/conans/zlib/1.3/_/_/revisions/aaaa/files/"):
		b := rec[strings.TrimPrefix(p, "/v2/conans/zlib/1.3/_/_/revisions/aaaa/files/")]
		if b == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	case p == "/v2/conans/zlib/1.3/_/_/revisions/aaaa/search":
		j(map[string]any{"pkg1": ParseConanInfo(conaninfoTxt)})
	case p == "/v2/conans/zlib/1.3/_/_/revisions/aaaa/packages/pkg1/latest":
		j(map[string]any{"revision": "p1", "time": "2026-01-02T03:04:06.000+0000"})
	case p == "/v2/conans/zlib/1.3/_/_/revisions/aaaa/packages/pkg1/revisions/p1/files":
		j(files(pkg))
	case strings.HasPrefix(p, "/v2/conans/zlib/1.3/_/_/revisions/aaaa/packages/pkg1/revisions/p1/files/"):
		w.Write(pkg[strings.TrimPrefix(p, "/v2/conans/zlib/1.3/_/_/revisions/aaaa/packages/pkg1/revisions/p1/files/")])
	default:
		http.NotFound(w, r)
	}
}

type env struct {
	h      *Handler
	repos  map[string]*repo.Repository
	sink   *repotest.Sink
	center *fakeCenter
	now    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), center: &fakeCenter{hits: map[string]int{}}}
	ts := httptest.NewServer(e.center)
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.sink = &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: func() time.Time { return e.now }, Events: e.sink}
	cache := config.Repository{MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)}
	proxyDef := func(name, remote string, ra *config.RemoteAuth) config.Repository {
		d := cache
		d.Name, d.Format, d.Type, d.Remote, d.RemoteAuth = name, "conan", "proxy", remote, ra
		return d
	}
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "conan-hosted", Format: "conan", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "conan-allow", Format: "conan", Type: "hosted", WritePolicy: "allow"},
		{Name: "conan-ro", Format: "conan", Type: "hosted", WritePolicy: "deny"},
		proxyDef("conan-proxy", ts.URL+"/", nil),
		proxyDef("conan-private", ts.URL+"/private/", &config.RemoteAuth{Username: "bob", Password: "secret"}),
		{Name: "conan-all", Format: "conan", Type: "group", Members: []string{"conan-hosted", "conan-proxy"}, WriteMember: "conan-hosted"},
		{Name: "conan-proxy-first", Format: "conan", Type: "group", Members: []string{"conan-proxy", "conan-hosted"}},
	}}
	c := ts.Client()
	c.Transport = &upstream{base: c.Transport, tokens: map[string]string{}}
	f := proxy.New(store, c, 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, f, auth.New(cfg, db), func() string { return ccBase + "/" }, nil)
	e.h.Now = func() time.Time { return e.now }
	e.repos = repo.Build(cfg)
	return e
}

func (e *env) do(method, repoName, path string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	return e.doAs(auth.Principal{}, method, repoName, path, body, hdr...)
}

func (e *env) doAs(p auth.Principal, method, repoName, path string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	path, _, _ = strings.Cut(path, "?")
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// uploadRecipe PUTs a recipe revision's files (and a package's, when
// pkgid is set), as Conan does, each a second apart.
func (e *env) uploadRecipe(t *testing.T, repoName string, ref Ref, rrev, tag string) {
	t.Helper()
	for _, name := range []string{"conan_export.tgz", "conanfile.py", "conanmanifest.txt"} {
		e.now = e.now.Add(time.Second)
		if w := e.do("PUT", repoName, RecipeFile(ref, rrev, name), recipeFiles(tag)[name]); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", name, w.Code, w.Body)
		}
	}
}

func (e *env) uploadPackage(t *testing.T, repoName string, ref Ref, rrev, pkgid, prev, tag string) {
	t.Helper()
	for _, name := range []string{"conan_package.tgz", "conaninfo.txt", "conanmanifest.txt"} {
		e.now = e.now.Add(time.Second)
		if w := e.do("PUT", repoName, prevDir(ref, rrev, pkgid, prev)+"files/"+name, packageFiles(tag)[name]); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", name, w.Code, w.Body)
		}
	}
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != ctJSON {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
}

type rev struct{ Revision, Time string }

func (e *env) latest(t *testing.T, repoName, path string) rev {
	t.Helper()
	var r rev
	decode(t, e.do("GET", repoName, path, nil), &r)
	return r
}

func (e *env) revisions(t *testing.T, repoName, path string) []rev {
	t.Helper()
	var got struct{ Revisions []rev }
	decode(t, e.do("GET", repoName, path, nil), &got)
	return got.Revisions
}

func (e *env) files(t *testing.T, repoName, path string) []string {
	t.Helper()
	var got struct{ Files map[string]any }
	decode(t, e.do("GET", repoName, path, nil), &got)
	var out []string
	for f := range got.Files {
		out = append(out, f)
	}
	return out
}

func (e *env) search(t *testing.T, repoName, path string) []string {
	t.Helper()
	var got struct{ Results []string }
	decode(t, e.do("GET", repoName, path, nil), &got)
	return got.Results
}

func TestPing(t *testing.T) {
	e := newEnv(t)
	for _, r := range []string{"conan-hosted", "conan-proxy", "conan-all"} {
		w := e.do("GET", r, "v1/ping", nil)
		if w.Code != 200 || w.Header().Get("X-Conan-Server-Capabilities") != Capabilities {
			t.Errorf("%s: %d %v", r, w.Code, w.Header())
		}
	}
	if w := e.do("POST", "conan-hosted", "v1/ping", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST ping: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("GET", "conan-hosted", "v2/nope", nil); w.Code != 404 {
		t.Errorf("unknown route: %d", w.Code)
	}
}

// TestV2Cycle uploads a recipe with two revisions and a package, reads
// everything back as Conan 2 does and deletes it piece by piece.
func TestV2Cycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if w := e.do("GET", "conan-hosted", zlib.dir()+"latest", nil); w.Code != 404 {
		t.Fatalf("latest of nothing: %d", w.Code)
	}
	if w := e.do("GET", "conan-hosted", zlib.dir()+"revisions", nil); w.Code != 404 {
		t.Fatalf("revisions of nothing: %d", w.Code)
	}
	e.uploadRecipe(t, "conan-hosted", zlib, "aaaa", "a")
	e.uploadPackage(t, "conan-hosted", zlib, "aaaa", "pkg1", "p1", "a")
	e.uploadRecipe(t, "conan-hosted", zlib, "bbbb", "b")
	e.uploadRecipe(t, "conan-hosted", fooMe, "cccc", "c")

	if got := e.latest(t, "conan-hosted", zlib.dir()+"latest"); got.Revision != "bbbb" || !strings.HasSuffix(got.Time, "+0000") {
		t.Errorf("latest: %+v", got)
	}
	revs := e.revisions(t, "conan-hosted", zlib.dir()+"revisions")
	if len(revs) != 2 || revs[0].Revision != "bbbb" || revs[1].Revision != "aaaa" {
		t.Errorf("revisions: %+v", revs)
	}
	if ts, err := time.Parse(timeFormat, revs[0].Time); err != nil || !ts.After(e.now.Add(-time.Hour)) {
		t.Errorf("time %q: %v", revs[0].Time, err)
	}
	if got := e.files(t, "conan-hosted", revDir(zlib, "aaaa")+"files"); len(got) != 3 {
		t.Errorf("files: %v", got)
	}
	if w := e.do("GET", "conan-hosted", revDir(zlib, "zzzz")+"files", nil); w.Code != 404 {
		t.Errorf("files of unknown revision: %d", w.Code)
	}
	if w := e.do("GET", "conan-hosted", RecipeFile(zlib, "aaaa", "conan_export.tgz"), nil); w.Code != 200 || w.Body.String() != "export-a" {
		t.Errorf("file: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "conan-hosted", RecipeFile(zlib, "aaaa", "conanfile.py"), nil); !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("conanfile.py type: %q", w.Header().Get("Content-Type"))
	}
	if w := e.do("GET", "conan-hosted", RecipeFile(zlib, "aaaa", "missing.txt"), nil); w.Code != 404 {
		t.Errorf("missing file: %d", w.Code)
	}
	// Packages of a revision, from conaninfo.txt.
	var pkgs map[string]struct {
		Settings     map[string]string
		Options      map[string]string
		Requires     []string
		FullRequires []string `json:"full_requires"`
		RecipeHash   string   `json:"recipe_hash"`
		Content      string
	}
	decode(t, e.do("GET", "conan-hosted", revDir(zlib, "aaaa")+"search", nil), &pkgs)
	p := pkgs["pkg1"]
	if len(pkgs) != 1 || p.Settings["arch"] != "x86_64" || p.Settings["os"] != "Linux" || p.Options["shared"] != "False" ||
		len(p.Requires) != 1 || p.Requires[0] != "zlib/1.Y.Z" || p.FullRequires[0] != "zlib/1.3:abc" || p.RecipeHash != "deadbeef" || p.Content != conaninfoTxt {
		t.Errorf("package search: %+v", pkgs)
	}
	var none map[string]any
	decode(t, e.do("GET", "conan-hosted", revDir(zlib, "bbbb")+"search", nil), &none)
	if len(none) != 0 {
		t.Errorf("packages of bbbb: %v", none)
	}
	if w := e.do("GET", "conan-hosted", revDir(zlib, "zzzz")+"search", nil); w.Code != 404 {
		t.Errorf("packages of unknown revision: %d", w.Code)
	}
	if got := e.latest(t, "conan-hosted", pkgDir(zlib, "aaaa", "pkg1")+"latest"); got.Revision != "p1" {
		t.Errorf("package latest: %+v", got)
	}
	if got := e.revisions(t, "conan-hosted", pkgDir(zlib, "aaaa", "pkg1")+"revisions"); len(got) != 1 || got[0].Revision != "p1" {
		t.Errorf("package revisions: %+v", got)
	}
	if got := e.files(t, "conan-hosted", prevDir(zlib, "aaaa", "pkg1", "p1")+"files"); len(got) != 3 {
		t.Errorf("package files: %v", got)
	}
	if w := e.do("GET", "conan-hosted", prevDir(zlib, "aaaa", "pkg1", "p1")+"files/conan_package.tgz", nil); w.Body.String() != "package-a" {
		t.Errorf("package file: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "conan-hosted", pkgDir(zlib, "aaaa", "pkg2")+"latest", nil); w.Code != 404 {
		t.Errorf("unknown package: %d", w.Code)
	}
	// Events: one per manifest.
	var uploads []string
	for _, ev := range e.sink.Got {
		if ev.Kind == "upload" {
			uploads = append(uploads, ev.Subject)
		}
	}
	if strings.Join(uploads, " ") != "zlib/1.3#aaaa zlib/1.3#aaaa:pkg1 zlib/1.3#bbbb foo/1.0@me/stable#cccc" {
		t.Errorf("upload events: %v", uploads)
	}

	// Re-uploads: the same content is a 200, different content a 409 (allow_once).
	if w := e.do("PUT", "conan-hosted", RecipeFile(zlib, "aaaa", "conan_export.tgz"), []byte("export-a")); w.Code != 200 {
		t.Errorf("same content: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "conan-hosted", RecipeFile(zlib, "aaaa", "conan_export.tgz"), []byte("export-x")); w.Code != 409 {
		t.Errorf("different content: %d %s", w.Code, w.Body)
	}
	// Checksum deploy: 404 until the file is there, 201 once it is.
	body := []byte("export-d")
	path := RecipeFile(zlib, "dddd", "conan_export.tgz")
	if w := e.do("PUT", "conan-hosted", path, nil, "X-Checksum-Deploy", "true", "X-Checksum-Sha1", sha1hex(body)); w.Code != 404 {
		t.Errorf("checksum deploy before: %d", w.Code)
	}
	if w := e.do("PUT", "conan-hosted", path, body); w.Code != 201 {
		t.Errorf("upload after checksum deploy: %d", w.Code)
	}
	if w := e.do("PUT", "conan-hosted", path, nil, "X-Checksum-Deploy", "true", "X-Checksum-Sha1", sha1hex(body)); w.Code != 201 {
		t.Errorf("checksum deploy after: %d", w.Code)
	}
	if w := e.do("PUT", "conan-hosted", path, nil, "X-Checksum-Deploy", "true", "X-Checksum-Sha1", sha1hex([]byte("other"))); w.Code != 404 {
		t.Errorf("checksum deploy, other sha1: %d", w.Code)
	}
	if rows, _ := e.h.Store.DB.ListPackages(ctx, "conan-hosted", "zlib/1.3"); len(rows) != 3 {
		t.Errorf("rows: %d", len(rows))
	}

	// Deletes, innermost first.
	del := func(path string, want int) {
		t.Helper()
		if w := e.do("DELETE", "conan-hosted", path, nil); w.Code != want {
			t.Errorf("DELETE %s: %d %s, want %d", path, w.Code, w.Body, want)
		}
	}
	e.uploadPackage(t, "conan-hosted", zlib, "aaaa", "pkg1", "p2", "a2")
	e.uploadPackage(t, "conan-hosted", zlib, "aaaa", "pkg2", "p1", "a3")
	del(strings.TrimSuffix(prevDir(zlib, "aaaa", "pkg1", "p2"), "/"), 200)
	if got := e.latest(t, "conan-hosted", pkgDir(zlib, "aaaa", "pkg1")+"latest"); got.Revision != "p1" {
		t.Errorf("after package revision delete: %+v", got)
	}
	del(strings.TrimSuffix(prevDir(zlib, "aaaa", "pkg1", "p2"), "/"), 404)
	del(strings.TrimSuffix(pkgDir(zlib, "aaaa", "pkg2"), "/"), 200)
	if w := e.do("GET", "conan-hosted", pkgDir(zlib, "aaaa", "pkg2")+"latest", nil); w.Code != 404 {
		t.Errorf("after package delete: %d", w.Code)
	}
	del(revDir(zlib, "aaaa")+"packages", 200)
	decode(t, e.do("GET", "conan-hosted", revDir(zlib, "aaaa")+"search", nil), &none)
	if len(none) != 0 {
		t.Errorf("after packages delete: %v", none)
	}
	del(strings.TrimSuffix(revDir(zlib, "dddd"), "/"), 200)
	if got := e.revisions(t, "conan-hosted", zlib.dir()+"revisions"); len(got) != 2 {
		t.Errorf("after revision delete: %+v", got)
	}
	del(strings.TrimSuffix(zlib.dir(), "/"), 200)
	if w := e.do("GET", "conan-hosted", zlib.dir()+"latest", nil); w.Code != 404 {
		t.Errorf("after recipe delete: %d", w.Code)
	}
	del(strings.TrimSuffix(zlib.dir(), "/"), 404)
	if assets, _ := e.h.Store.DB.ListAssets(ctx, "conan-hosted", zlib.dir()); len(assets) != 0 {
		t.Errorf("assets left: %d", len(assets))
	}
	if got := e.latest(t, "conan-hosted", fooMe.dir()+"latest"); got.Revision != "cccc" {
		t.Errorf("foo survived? %+v", got)
	}
	if w := e.do("GET", "conan-hosted", strings.TrimSuffix(zlib.dir(), "/"), nil); w.Code != 405 || w.Header().Get("Allow") != "DELETE" {
		t.Errorf("GET recipe route: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("DELETE", "conan-proxy", strings.TrimSuffix(zlib.dir(), "/"), nil); w.Code != 405 || w.Header().Get("Allow") != "" {
		t.Errorf("DELETE on a proxy: %d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestSearch(t *testing.T) {
	e := newEnv(t)
	e.uploadRecipe(t, "conan-hosted", zlib, "aaaa", "a")
	e.uploadRecipe(t, "conan-hosted", fooMe, "cccc", "c")
	e.uploadRecipe(t, "conan-hosted", Ref{"zlib", "1.2.13", "_", "_"}, "aaaa", "a")
	for _, c := range []struct{ q, want string }{
		{"*", "foo/1.0@me/stable zlib/1.2.13 zlib/1.3"},
		{"zlib*", "zlib/1.2.13 zlib/1.3"},
		{"ZLIB/1.3", "zlib/1.3"},
		{"zlib/1.3@*/*", "zlib/1.3"},
		{"zlib/1.3@_/_", "zlib/1.3"},
		{"zlib/1.?", "zlib/1.3"},
		{"foo", "foo/1.0@me/stable"},
		{"foo/*@me/*", "foo/1.0@me/stable"},
		{"*stable", "foo/1.0@me/stable"},
		{"", "foo/1.0@me/stable zlib/1.2.13 zlib/1.3"},
		{"nothing", ""},
	} {
		for _, prefix := range []string{"v2", "v1"} {
			got := e.search(t, "conan-hosted", prefix+"/conans/search?q="+c.q)
			if strings.Join(got, " ") != c.want {
				t.Errorf("%s search %q: %v, want %q", prefix, c.q, got, c.want)
			}
		}
	}
	// A proxy passes the upstream answer through; a group merges.
	if got := e.search(t, "conan-proxy", "v2/conans/search?q=b*"); strings.Join(got, " ") != "boost/1.84.0" {
		t.Errorf("proxy search: %v", got)
	}
	if got := e.search(t, "conan-all", "v2/conans/search?q=*"); strings.Join(got, " ") != "boost/1.84.0 foo/1.0@me/stable zlib/1.2.13 zlib/1.3" {
		t.Errorf("group search: %v", got)
	}
	e.search(t, "conan-proxy", "v2/conans/search?q=b*")
	if n := e.center.hit("/v2/conans/search?q=b%2A"); n != 1 {
		t.Errorf("upstream search hits: %d", n)
	}
}

// TestV1 reads the hosted recipe as a Conan 1.x client without revisions
// does, and gets upload URLs that a PUT accepts.
func TestV1(t *testing.T) {
	e := newEnv(t)
	e.uploadRecipe(t, "conan-hosted", zlib, "aaaa", "a")
	e.uploadPackage(t, "conan-hosted", zlib, "aaaa", "pkg1", "p1", "a")
	v1 := "v1/conans/zlib/1.3/_/_"
	var snap map[string]string
	decode(t, e.do("GET", "conan-all", v1, nil), &snap)
	rec := recipeFiles("a")
	if len(snap) != 3 || snap["conanfile.py"] != md5hex(rec["conanfile.py"]) || snap["conanmanifest.txt"] != md5hex(rec["conanmanifest.txt"]) {
		t.Errorf("snapshot: %v", snap)
	}
	var digest map[string]string
	decode(t, e.do("GET", "conan-all", v1+"/digest", nil), &digest)
	want := ccBase + "/repository/conan-all/" + RecipeFile(zlib, "aaaa", "conanmanifest.txt")
	if digest["conanmanifest.txt"] != want {
		t.Errorf("digest: %v", digest)
	}
	var urls map[string]string
	decode(t, e.do("GET", "conan-all", v1+"/download_urls", nil), &urls)
	if len(urls) != 3 || urls["conan_export.tgz"] != ccBase+"/repository/conan-all/"+RecipeFile(zlib, "aaaa", "conan_export.tgz") {
		t.Errorf("download_urls: %v", urls)
	}
	// The URLs are served by the group.
	rel := strings.TrimPrefix(urls["conan_export.tgz"], ccBase+"/repository/conan-all/")
	if w := e.do("GET", "conan-all", rel, nil); w.Code != 200 || w.Body.String() != "export-a" {
		t.Errorf("download: %d %s", w.Code, w.Body)
	}
	snap = nil
	decode(t, e.do("GET", "conan-hosted", v1+"/packages/pkg1", nil), &snap)
	if len(snap) != 3 || snap["conaninfo.txt"] != md5hex([]byte(conaninfoTxt)) {
		t.Errorf("package snapshot: %v", snap)
	}
	decode(t, e.do("GET", "conan-hosted", v1+"/packages/pkg1/download_urls", nil), &urls)
	if urls["conan_package.tgz"] != ccBase+"/repository/conan-hosted/"+prevDir(zlib, "aaaa", "pkg1", "p1")+"files/conan_package.tgz" {
		t.Errorf("package download_urls: %v", urls)
	}
	decode(t, e.do("GET", "conan-hosted", v1+"/packages/pkg1/digest", nil), &digest)
	if digest["conanmanifest.txt"] != ccBase+"/repository/conan-hosted/"+prevDir(zlib, "aaaa", "pkg1", "p1")+"files/conanmanifest.txt" {
		t.Errorf("package digest: %v", digest)
	}
	var pkgs map[string]struct {
		Settings     map[string]string
		FullRequires []string `json:"full_requires"`
		RecipeHash   string   `json:"recipe_hash"`
	}
	decode(t, e.do("GET", "conan-hosted", v1+"/search", nil), &pkgs)
	if pkgs["pkg1"].Settings["os"] != "Linux" || pkgs["pkg1"].RecipeHash != "deadbeef" {
		t.Errorf("v1 package search: %+v", pkgs)
	}
	for _, p := range []string{"v1/conans/boost/1.0/_/_", "v1/conans/boost/1.0/_/_/download_urls", "v1/conans/zlib/1.3/_/_/packages/nope", "v1/conans/zlib/1.3/_/_/packages/nope/download_urls"} {
		if w := e.do("GET", "conan-hosted", p, nil); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	// Upload URLs: an existing recipe's latest revision, a new one's revision 0, a package's revision 0.
	decode(t, e.do("POST", "conan-all", v1+"/upload_urls", []byte(`{"conanfile.py": 10, "conanmanifest.txt": 20}`)), &urls)
	if urls["conanfile.py"] != ccBase+"/repository/conan-all/"+RecipeFile(zlib, "aaaa", "conanfile.py") {
		t.Errorf("upload_urls: %v", urls)
	}
	decode(t, e.do("POST", "conan-all", "v1/conans/new/2.0/_/_/upload_urls", []byte(`{"conanfile.py": 10}`)), &urls)
	if urls["conanfile.py"] != ccBase+"/repository/conan-all/v2/conans/new/2.0/_/_/revisions/0/files/conanfile.py" {
		t.Errorf("upload_urls of a new recipe: %v", urls)
	}
	rel = strings.TrimPrefix(urls["conanfile.py"], ccBase+"/repository/conan-all/")
	if w := e.do("PUT", "conan-all", rel, []byte("class New: pass")); w.Code != 201 {
		t.Errorf("PUT at the upload url: %d %s", w.Code, w.Body)
	}
	if got := e.latest(t, "conan-hosted", "v2/conans/new/2.0/_/_/latest"); got.Revision != "0" {
		t.Errorf("v1 upload landed in: %+v", got)
	}
	decode(t, e.do("POST", "conan-hosted", "v1/conans/new/2.0/_/_/packages/pkgA/upload_urls", []byte(`{"conaninfo.txt": 10}`)), &urls)
	if urls["conaninfo.txt"] != ccBase+"/repository/conan-hosted/v2/conans/new/2.0/_/_/revisions/0/packages/pkgA/revisions/0/files/conaninfo.txt" {
		t.Errorf("package upload_urls: %v", urls)
	}
	if w := e.do("POST", "conan-hosted", "v1/conans/none/1.0/_/_/packages/pkgA/upload_urls", []byte(`{"a": 1}`)); w.Code != 404 {
		t.Errorf("package upload_urls of an unknown recipe: %d", w.Code)
	}
	if w := e.do("POST", "conan-hosted", v1+"/upload_urls", []byte(`{"../x": 1}`)); w.Code != 400 {
		t.Errorf("bad file name: %d", w.Code)
	}
	if w := e.do("POST", "conan-proxy", v1+"/upload_urls", []byte(`{"a": 1}`)); w.Code != 405 {
		t.Errorf("upload_urls on a proxy: %d", w.Code)
	}
	if w := e.do("DELETE", "conan-hosted", v1, nil); w.Code != 200 {
		t.Errorf("v1 delete: %d", w.Code)
	}
	if w := e.do("GET", "conan-hosted", v1, nil); w.Code != 404 {
		t.Errorf("after v1 delete: %d", w.Code)
	}
}

func TestProxy(t *testing.T) {
	e := newEnv(t)
	if got := e.latest(t, "conan-proxy", zlib.dir()+"latest"); got.Revision != "aaaa" {
		t.Errorf("latest: %+v", got)
	}
	e.latest(t, "conan-proxy", zlib.dir()+"latest")
	if n := e.center.hit("/v2/conans/zlib/1.3/_/_/latest?"); n != 1 {
		t.Errorf("latest fetched %d times", n)
	}
	e.now = e.now.Add(2 * time.Hour) // metadata expired: asked again
	e.latest(t, "conan-proxy", zlib.dir()+"latest")
	if n := e.center.hit("/v2/conans/zlib/1.3/_/_/latest?"); n != 2 {
		t.Errorf("latest fetched %d times after expiry", n)
	}
	if got := e.files(t, "conan-proxy", revDir(zlib, "aaaa")+"files"); len(got) != 3 {
		t.Errorf("files: %v", got)
	}
	for i := 0; i < 2; i++ {
		if w := e.do("GET", "conan-proxy", RecipeFile(zlib, "aaaa", "conan_export.tgz"), nil); w.Code != 200 || w.Body.String() != "export-center" {
			t.Errorf("file: %d %s", w.Code, w.Body)
		}
	}
	e.now = e.now.Add(2 * time.Hour)
	e.do("GET", "conan-proxy", RecipeFile(zlib, "aaaa", "conan_export.tgz"), nil)
	if n := e.center.hit("/v2/conans/zlib/1.3/_/_/revisions/aaaa/files/conan_export.tgz?"); n != 1 {
		t.Errorf("content fetched %d times", n)
	}
	var pkgs map[string]struct{ Settings map[string]string }
	decode(t, e.do("GET", "conan-proxy", revDir(zlib, "aaaa")+"search", nil), &pkgs)
	if pkgs["pkg1"].Settings["arch"] != "x86_64" {
		t.Errorf("package search: %+v", pkgs)
	}
	if got := e.latest(t, "conan-proxy", pkgDir(zlib, "aaaa", "pkg1")+"latest"); got.Revision != "p1" {
		t.Errorf("package latest: %+v", got)
	}
	if w := e.do("GET", "conan-proxy", prevDir(zlib, "aaaa", "pkg1", "p1")+"files/conan_package.tgz", nil); w.Body.String() != "package-center" {
		t.Errorf("package file: %d %s", w.Code, w.Body)
	}
	for i := 0; i < 2; i++ {
		if w := e.do("GET", "conan-proxy", "v2/conans/nope/1.0/_/_/latest", nil); w.Code != 404 {
			t.Errorf("unknown recipe: %d", w.Code)
		}
	}
	if n := e.center.hit("/v2/conans/nope/1.0/_/_/latest?"); n != 1 {
		t.Errorf("negative cache: %d fetches", n)
	}
	// The v1 view works over the proxy: the snapshot reads the cached manifest.
	var snap map[string]string
	decode(t, e.do("GET", "conan-proxy", "v1/conans/zlib/1.3/_/_", nil), &snap)
	if snap["conan_export.tgz"] != md5hex([]byte("export-center")) || snap["conanmanifest.txt"] == "" {
		t.Errorf("v1 snapshot over proxy: %v", snap)
	}
	if w := e.do("PUT", "conan-proxy", RecipeFile(zlib, "aaaa", "x.txt"), []byte("x")); w.Code != 405 {
		t.Errorf("PUT on a proxy: %d", w.Code)
	}
	// Cached files are components; the cached documents are not.
	assets, _ := e.h.Store.DB.ListAssets(context.Background(), "conan-proxy", "")
	comps := map[string]int{}
	for _, a := range assets {
		g, n, v, ok := component.DeriveAsset("conan", a.Path)
		if ok {
			comps[g+"|"+n+"|"+v]++
		}
	}
	if len(comps) != 1 || comps["|zlib|1.3#aaaa"] != 3 { // the export, the package and the manifest the v1 snapshot read
		t.Errorf("components of the cache: %v (assets %d)", comps, len(assets))
	}
}

// TestProxyRemoteLogin: a proxy with remoteAuth logs in at the remote's
// v1/users/authenticate and sends the token; a refused token is renewed.
func TestProxyRemoteLogin(t *testing.T) {
	e := newEnv(t)
	if got := e.latest(t, "conan-private", zlib.dir()+"latest"); got.Revision != "aaaa" {
		t.Errorf("latest: %+v", got)
	}
	if w := e.do("GET", "conan-private", RecipeFile(zlib, "aaaa", "conan_export.tgz"), nil); w.Body.String() != "export-center" {
		t.Errorf("file: %d %s", w.Code, w.Body)
	}
	if e.center.logins != 1 {
		t.Errorf("logins: %d", e.center.logins)
	}
	e.center.mu.Lock()
	e.center.token = "revoked"
	e.center.mu.Unlock()
	if got := e.files(t, "conan-private", revDir(zlib, "aaaa")+"files"); len(got) != 3 {
		t.Errorf("files after revocation: %v", got)
	}
	if e.center.logins != 2 {
		t.Errorf("logins after revocation: %d", e.center.logins)
	}
}

func TestGroup(t *testing.T) {
	e := newEnv(t)
	// The hosted member's revision wins where it has the recipe, whatever a later member has.
	e.uploadRecipe(t, "conan-all", zlib, "bbbb", "b") // through the group: writeMember
	if got := e.latest(t, "conan-hosted", zlib.dir()+"latest"); got.Revision != "bbbb" {
		t.Errorf("upload via group landed: %+v", got)
	}
	if got := e.latest(t, "conan-all", zlib.dir()+"latest"); got.Revision != "bbbb" {
		t.Errorf("group latest: %+v", got)
	}
	if got := e.revisions(t, "conan-all", zlib.dir()+"revisions"); len(got) != 1 || got[0].Revision != "bbbb" {
		t.Errorf("group revisions: %+v", got)
	}
	if w := e.do("GET", "conan-all", RecipeFile(zlib, "aaaa", "conan_export.tgz"), nil); w.Code != 404 {
		t.Errorf("proxy's revision through the group: %d", w.Code)
	}
	if got := e.latest(t, "conan-proxy-first", zlib.dir()+"latest"); got.Revision != "aaaa" {
		t.Errorf("proxy-first latest: %+v", got)
	}
	// A recipe only the proxy has comes from it.
	if got := e.latest(t, "conan-all", "v2/conans/boost/1.84.0/_/_/latest"); got.Revision != "aaaa" {
		t.Errorf("boost: %+v", got)
	}
	if w := e.do("GET", "conan-all", "v2/conans/none/1.0/_/_/latest", nil); w.Code != 404 {
		t.Errorf("unknown: %d", w.Code)
	}
	if w := e.do("PUT", "conan-proxy-first", RecipeFile(zlib, "cccc", "x.txt"), []byte("x")); w.Code != 405 {
		t.Errorf("PUT on a group without writeMember: %d", w.Code)
	}
	// An offline member ahead is skipped.
	off := false
	e.repos["conan-proxy"].Online = &off
	if got := e.latest(t, "conan-proxy-first", zlib.dir()+"latest"); got.Revision != "bbbb" {
		t.Errorf("offline proxy skipped: %+v", got)
	}
}

func TestWritePolicy(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "conan-ro", RecipeFile(zlib, "aaaa", "conanfile.py"), []byte("x")); w.Code != 400 {
		t.Errorf("deny: %d", w.Code)
	}
	if w := e.do("DELETE", "conan-ro", strings.TrimSuffix(zlib.dir(), "/"), nil); w.Code != 400 {
		t.Errorf("deny delete: %d", w.Code)
	}
	a, ps := repotest.Principals(t, e.h.Store.DB, "conan-allow", map[string][]string{"editor": {"read", "add", "edit"}, "adder": {"read", "add"}})
	e.h.Auth = a
	e.uploadRecipe(t, "conan-allow", zlib, "aaaa", "a")
	path := RecipeFile(zlib, "aaaa", "conan_export.tgz")
	if w := e.doAs(ps["adder"], "PUT", "conan-allow", path, []byte("export-x")); w.Code != 403 {
		t.Errorf("overwrite without edit: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["editor"], "PUT", "conan-allow", path, []byte("export-x")); w.Code != 201 {
		t.Errorf("overwrite with edit: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "conan-allow", path, nil); w.Body.String() != "export-x" {
		t.Errorf("after overwrite: %s", w.Body)
	}
	if w := e.doAs(ps["adder"], "PUT", "conan-allow", path, []byte("export-x")); w.Code != 200 {
		t.Errorf("same content without edit: %d", w.Code)
	}
}

func TestDeleteComponentAndCleanup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["conan-hosted"]
	e.uploadRecipe(t, "conan-hosted", zlib, "aaaa", "a")
	e.uploadPackage(t, "conan-hosted", zlib, "aaaa", "pkg1", "p1", "a")
	e.now = e.now.Add(48 * time.Hour)
	e.uploadRecipe(t, "conan-hosted", zlib, "bbbb", "b")
	e.uploadRecipe(t, "conan-hosted", fooMe, "cccc", "c")

	// Cleanup evaluation groups the files by recipe revision.
	ev := &cleanup.Evaluator{DB: e.h.Store.DB, Now: func() time.Time { return e.now.Add(time.Hour) }}
	pol := cleanup.Policy{Name: "old", Format: "conan", Criteria: cleanup.Criteria{ComponentAgeDays: 1, ReleaseType: "any"}}
	cands, _, _, _, err := ev.Evaluate(ctx, rp, pol, 0)
	if err != nil || len(cands) != 1 || cands[0].Name != "zlib" || cands[0].Version != "1.3#aaaa" || len(cands[0].Assets) != 6 {
		t.Fatalf("evaluate: %+v %v", cands, err)
	}
	pol.Criteria.ComponentAgeDays, pol.Criteria.NamePattern = 0, "me/stable:foo"
	pol.Criteria.LastDownloadedDays = 1
	ev.Now = func() time.Time { return e.now.Add(72 * time.Hour) }
	if cands, _, _, _, err = ev.Evaluate(ctx, rp, pol, 0); err != nil || len(cands) != 1 || cands[0].Group != "me/stable" {
		t.Fatalf("evaluate by name: %+v %v", cands, err)
	}
	// A component that changed since the evaluation is skipped.
	c := cands[0].Component
	e.uploadPackage(t, "conan-hosted", fooMe, "cccc", "pkgX", "p1", "x")
	if _, err := e.h.DeleteComponent(ctx, rp, c, "system"); !isChanged(err) {
		t.Fatalf("changed component: %v", err)
	}
	c.Evaluated = nil
	res, err := e.h.DeleteComponent(ctx, rp, c, "system")
	if err != nil || res.Assets != 6 {
		t.Fatalf("delete foo: %+v %v", res, err)
	}
	if w := e.do("GET", "conan-hosted", fooMe.dir()+"latest", nil); w.Code != 404 {
		t.Errorf("foo after delete: %d", w.Code)
	}
	// The UI's delete: a recipe revision with its packages; the other revision stays.
	c = component.Component{Repo: "conan-hosted", Format: "conan", Name: "zlib", Version: "1.3#aaaa"}
	if res, err := e.h.DeleteComponent(ctx, rp, c, "admin"); err != nil || res.Assets != 6 {
		t.Fatalf("delete zlib#aaaa: %+v %v", res, err)
	}
	if res, err := e.h.DeleteComponent(ctx, rp, c, "admin"); err != nil || res.Assets != 0 {
		t.Fatalf("delete again: %+v %v", res, err)
	}
	if got := e.revisions(t, "conan-hosted", zlib.dir()+"revisions"); len(got) != 1 || got[0].Revision != "bbbb" {
		t.Errorf("after delete: %+v", got)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["conan-proxy"], c, "admin"); err == nil {
		t.Error("proxy accepted")
	}
	if _, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "conan", Name: "zlib", Version: "1.3"}, "admin"); err == nil {
		t.Error("a version without revision accepted")
	}
	if _, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "npm", Name: "x", Version: "1#a"}, "admin"); err == nil {
		t.Error("another format accepted")
	}
	var deletes int
	for _, ev := range e.sink.Got {
		if ev.Kind == "delete" {
			deletes++
		}
	}
	if deletes != 2 {
		t.Errorf("delete events: %d", deletes)
	}
}

func isChanged(err error) bool {
	return err != nil && strings.Contains(err.Error(), "since it was evaluated")
}

func TestDeriveAsset(t *testing.T) {
	for _, c := range []struct {
		path, group, name, version string
		ok                         bool
	}{
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/files/conanfile.py", "", "zlib", "1.3#aaaa", true},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/files/metadata/logs/build.log", "", "zlib", "1.3#aaaa", true},
		{"v2/conans/foo/1.0/me/stable/revisions/c/packages/pkg1/revisions/p1/files/conan_package.tgz", "me/stable", "foo", "1.0#c", true},
		{"v2/conans/zlib/1.3/_/_/latest", "", "", "", false},
		{"v2/conans/zlib/1.3/_/_/revisions", "", "", "", false},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/files", "", "", "", false},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/search", "", "", "", false},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/packages/pkg1/latest", "", "", "", false},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/packages/pkg1/revisions/p1/files", "", "", "", false},
		{"search/abcd.json", "", "", "", false},
	} {
		g, n, v, ok := component.DeriveAsset("conan", c.path)
		if g != c.group || n != c.name || v != c.version || ok != c.ok {
			t.Errorf("%s: %q %q %q %v", c.path, g, n, v, ok)
		}
	}
	if g, n, v, p := component.PackageComponent("conan", "foo/1.0@me/stable", "c"); g != "me/stable" || n != "foo" || v != "1.0#c" || p != "v2/conans/foo/1.0/me/stable/revisions/c/files/conanmanifest.txt" {
		t.Errorf("PackageComponent: %q %q %q %q", g, n, v, p)
	}
	if d, ok := component.ConanRevisionDir("v2/conans/zlib/1.3/_/_/revisions/aaaa/packages/pkg1/revisions/p1/files/x"); !ok || d != "v2/conans/zlib/1.3/_/_/revisions/aaaa/" {
		t.Errorf("ConanRevisionDir: %q %v", d, ok)
	}
	if ref, rrev, ok := ComponentRef("me/stable", "foo", "1.0#c"); !ok || ref.String() != "foo/1.0@me/stable" || rrev != "c" {
		t.Errorf("ComponentRef: %+v %q %v", ref, rrev, ok)
	}
	if !cleanup.Prerelease("conan", "1.0.0-rc1#abc") || cleanup.Prerelease("conan", "1.0.0#abc") {
		t.Error("Prerelease")
	}
}

func TestParseRoute(t *testing.T) {
	for _, c := range []struct {
		path string
		kind Kind
		v1   bool
		file string
	}{
		{"v1/ping", KindPing, true, ""},
		{"v2/conans/search", KindSearch, false, ""},
		{"v2/conans/zlib/1.3/_/_", KindRecipe, false, ""},
		{"v2/conans/zlib/1.3/_/_/latest", KindLatest, false, ""},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa", KindRevision, false, ""},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/files/conanfile.py;sha1=abc", KindFile, false, "conanfile.py"},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/files/metadata/logs/b.log", KindFile, false, "metadata/logs/b.log"},
		{"v2/conans/zlib/1.3/_/_/revisions/aaaa/packages/pkg1/revisions/p1/files/conaninfo.txt", KindPkgFile, false, "conaninfo.txt"},
		{"v1/conans/zlib/1.3/_/_/packages/pkg1/upload_urls", KindV1PkgUploadURLs, true, ""},
	} {
		rt, err := ParseRoute(c.path)
		if err != nil || rt.Kind != c.kind || rt.V1 != c.v1 || rt.File != c.file {
			t.Errorf("%s: %+v %v", c.path, rt, err)
		}
	}
	for _, p := range []string{"", "v3/conans/x/1/_/_", "v2/conans/zlib/1.3/_", "v2/conans/zlib/1.3/_/_/revisions/aaaa/files/../x", "v2/conans/zlib/1.3/_/_/revisions/a a/files", "v2/conans/zl ib/1.3/_/_/latest", "v1/conans/zlib/1.3/_/_/revisions/a/files"} {
		if _, err := ParseRoute(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestParse(t *testing.T) {
	cf := ParseConanfile([]byte(conanfilePy))
	if cf.Description != "A massively spiffy yet delicately\n    unobtrusive compression library" || cf.License != "Zlib, MIT" ||
		cf.Homepage != "https://zlib.net" || cf.URL != "https://github.com/conan-io/conan-center-index" {
		t.Errorf("conanfile: %+v", cf)
	}
	if got := strings.Join(cf.Requires["requires"], " "); got != "bzip2/1.0.8 libpng/[>=1.6] openssl/3.2.0" {
		t.Errorf("requires: %q", got)
	}
	if got := strings.Join(cf.Requires["tool_requires"], " "); got != "cmake/3.27 ninja/1.11" {
		t.Errorf("tool_requires: %q", got)
	}
	if cf := ParseConanfile([]byte("class X(ConanFile):\n    license = 'MIT'\n    requires = [\"a/1\",\n        \"b/2\"]\n")); cf.License != "MIT" || len(cf.Requires["requires"]) != 2 {
		t.Errorf("list: %+v", cf)
	}
	if m := ParseManifest("1700\nconanfile.py: abc\nconan_export.tgz: def\n"); m["conanfile.py"] != "abc" || m["conan_export.tgz"] != "def" || len(m) != 2 {
		t.Errorf("manifest: %v", m)
	}
	if info := ParseConanInfo(""); len(info["requires"].([]string)) != 0 || info["recipe_hash"] != nil {
		t.Errorf("empty info: %v", info)
	}
}
