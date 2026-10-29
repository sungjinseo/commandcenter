package composer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

const ccBase = "http://cc.example"

// composerJSON is a package manifest as a library ships one.
func composerJSON(name, extra string) string {
	return `{"name":"` + name + `","description":"A demo package","type":"library","license":"MIT","homepage":"https://example.org/demo",
"require":{"php":">=8.1","monolog/monolog":"^3.0"},"require-dev":{"phpunit/phpunit":"^10"},"autoload":{"psr-4":{"Acme\\Demo\\":"src/"}}` + extra + `}`
}

// archive builds a package zip: composer.json at the root, or under dir
// as `git archive --prefix` and GitHub zipballs lay it out.
func archive(t *testing.T, dir, manifest string) []byte {
	t.Helper()
	files := map[string]string{dir + "composer.json": manifest, dir + "src/Demo.php": "<?php"}
	if manifest == "" {
		delete(files, dir+"composer.json")
	}
	return zipBytes(t, files)
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func sha1hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
	sink  *repotest.Sink
	up    string       // the upstream server URL
	hits  atomic.Int32 // upstream archive downloads
	query atomic.Pointer[string]
	zips  map[string][]byte
	now   time.Time
}

// upstream is a fake Packagist: a v2 root with the v1 keys Packagist still
// lists, minified package metadata whose dist URLs point at this host,
// a ~dev file with a wrong shasum, the archives and search.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	up := e.up
	j := func(v string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, v)
	}
	dist := func(name string) string {
		return `{"type":"zip","url":"` + up + `/zips/` + name + `.zip","reference":"ref-` + name + `","shasum":"` + sha1hex(e.zips[name]) + `"}`
	}
	switch p := r.URL.Path; {
	case p == "/packages.json":
		j(`{"packages":{},"metadata-url":"/p2/%package%.json","providers-url":"/p/%package%$%hash%.json","notify-batch":"` + up + `/downloads/",
"search":"` + up + `/search.json?q=%query%&type=%type%","list":"` + up + `/packages/list.json","security-advisories":{"api-url":"` + up + `/api/security-advisories/"},"minified":"composer/2.0"}`)
	case p == "/p2/monolog/monolog.json":
		j(`{"minified":"composer/2.0","packages":{"monolog/monolog":[
{"name":"monolog/monolog","description":"Sends your logs","version":"3.5.0","version_normalized":"3.5.0.0","license":["MIT"],"type":"library",
 "source":{"type":"git","url":"https://github.com/Seldaek/monolog.git","reference":"c915e2"},"dist":` + dist("monolog-3.5.0") + `,
 "require":{"php":">=8.1"},"time":"2026-01-02T00:00:00+00:00"},
{"version":"3.4.0","version_normalized":"3.4.0.0","source":{"type":"git","url":"https://github.com/Seldaek/monolog.git","reference":"b1"},"dist":` + dist("monolog-3.4.0") + `,
 "require":"__unset","time":"2026-01-01T00:00:00+00:00"}]}}`)
	case p == "/p2/monolog/monolog~dev.json":
		j(`{"minified":"composer/2.0","packages":{"monolog/monolog":[
{"name":"monolog/monolog","version":"dev-main","version_normalized":"dev-main","dist":{"type":"zip","url":"` + up + `/zips/monolog-main.zip","shasum":"0000000000000000000000000000000000000000"}}]}}`)
	case p == "/p2/acme/demo.json": // also hosted: the group's hosted member wins
		j(`{"packages":{"acme/demo":[{"name":"acme/demo","description":"upstream demo","version":"9.0.0","version_normalized":"9.0.0.0","dist":` + dist("demo-9") + `}]}}`)
	case p == "/p2/acme/demo~dev.json":
		j(`{"packages":{"acme/demo":[]}}`)
	case strings.HasPrefix(p, "/zips/"):
		b := e.zips[strings.TrimSuffix(strings.TrimPrefix(p, "/zips/"), ".zip")]
		if b == nil {
			http.NotFound(w, r)
			return
		}
		e.hits.Add(1)
		w.Header().Set("Content-Type", "application/zip")
		w.Write(b)
	case p == "/search.json":
		q := r.URL.RawQuery
		e.query.Store(&q)
		j(`{"results":[{"name":"monolog/monolog","description":"Sends your logs","url":"https://packagist.org/packages/monolog/monolog"},
{"name":"acme/demo","description":"upstream demo"}],"total":2}`)
	default:
		http.NotFound(w, r)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), zips: map[string][]byte{}}
	for _, n := range []string{"monolog-3.5.0", "monolog-3.4.0", "monolog-main", "demo-9"} {
		e.zips[n] = archive(t, n+"/", composerJSON("monolog/monolog", `,"version":"`+n+`"`))
	}
	ts := httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(ts.Close)
	e.up = ts.URL
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.URL(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.sink = &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: func() time.Time { return e.now }, Events: e.sink}
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "composer-hosted", Format: "composer", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "composer-allow", Format: "composer", Type: "hosted", WritePolicy: "allow"},
		{Name: "composer-ro", Format: "composer", Type: "hosted", WritePolicy: "deny"},
		{Name: "composer-proxy", Format: "composer", Type: "proxy", Remote: ts.URL + "/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "composer-all", Format: "composer", Type: "group", Members: []string{"composer-hosted", "composer-proxy"}, WriteMember: "composer-hosted"},
		{Name: "composer-proxy-first", Format: "composer", Type: "group", Members: []string{"composer-proxy", "composer-hosted"}},
	}}
	f := proxy.New(store, ts.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, f, auth.New(cfg, db), func() string { return ccBase + "/" })
	e.h.Now = func() time.Time { return e.now }
	e.repos = repo.Build(cfg)
	return e
}

func (e *env) do(method, repoName, path string, body []byte) *httptest.ResponseRecorder {
	return e.doAs(auth.Principal{}, method, repoName, path, body)
}

func (e *env) doAs(p auth.Principal, method, repoName, path string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, bytes.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	path, _, _ = strings.Cut(path, "?")
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

func (e *env) upload(t *testing.T, repoName, name, version string, zipb []byte) *httptest.ResponseRecorder {
	t.Helper()
	return e.do("PUT", repoName, uploadPath+name+"/"+version, zipb)
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

// p2 is a metadata file as Composer reads it.
type p2 struct {
	Minified string `json:"minified"`
	Packages map[string][]struct {
		Name        string            `json:"name"`
		Version     string            `json:"version"`
		Normalized  string            `json:"version_normalized"`
		Description string            `json:"description"`
		Type        string            `json:"type"`
		License     any               `json:"license"`
		Require     map[string]string `json:"require"`
		Time        string            `json:"time"`
		Source      map[string]any    `json:"source"`
		Dist        struct {
			Type, URL, Shasum, Reference string
		} `json:"dist"`
	} `json:"packages"`
}

func (e *env) metadata(t *testing.T, repoName, name string, dev bool) p2 {
	t.Helper()
	var d p2
	decode(t, e.do("GET", repoName, metadataPath(name, dev), nil), &d)
	return d
}

type root struct {
	Packages    map[string]any `json:"packages"`
	MetadataURL string         `json:"metadata-url"`
	Search      string         `json:"search"`
	Available   []string       `json:"available-packages"`
	Minified    string         `json:"minified"`
}

type searchResult struct {
	Results []struct{ Name, Description string } `json:"results"`
	Total   int                                  `json:"total"`
}

func (s searchResult) names() string {
	var out []string
	for _, r := range s.Results {
		out = append(out, r.Name)
	}
	return strings.Join(out, ",")
}

func TestHostedUploadAndMetadata(t *testing.T) {
	e := newEnv(t)
	v1 := archive(t, "", composerJSON("acme/demo", ""))
	v2 := archive(t, "demo-2.0.0/", composerJSON("Acme/Demo", `,"version":"v2.0.0-beta1"`))
	main := archive(t, "", composerJSON("acme/demo", ""))
	for _, c := range []struct {
		version string
		zip     []byte
	}{{"v1.0.0", v1}, {"2.0.0-beta1", v2}, {"dev-main", main}} {
		if w := e.upload(t, "composer-hosted", "acme/demo", c.version, c.zip); w.Code != 201 {
			t.Fatalf("upload %s: %d %s", c.version, w.Code, w.Body)
		}
	}
	var rt root
	decode(t, e.do("GET", "composer-hosted", "packages.json", nil), &rt)
	base := ccBase + "/repository/composer-hosted"
	if rt.MetadataURL != base+"/p2/%package%.json" || rt.Search != base+"/search.json?q=%query%&type=%type%" || rt.Minified != "" || len(rt.Packages) != 0 {
		t.Fatalf("root: %+v", rt)
	}
	if strings.Join(rt.Available, ",") != "acme/demo" {
		t.Fatalf("available-packages: %v", rt.Available)
	}

	d := e.metadata(t, "composer-hosted", "acme/demo", false)
	vs := d.Packages["acme/demo"]
	if d.Minified != "" || len(vs) != 2 || vs[0].Normalized != "2.0.0.0-beta1" || vs[1].Normalized != "1.0.0.0" {
		t.Fatalf("p2: %+v", d)
	}
	v := vs[1]
	if v.Name != "acme/demo" || v.Version != "v1.0.0" || v.Description != "A demo package" || v.Type != "library" || v.Require["monolog/monolog"] != "^3.0" ||
		v.Time != "2026-10-28T12:00:00Z" || v.Source != nil {
		t.Fatalf("entry: %+v", v)
	}
	if v.Dist.Type != "zip" || v.Dist.URL != base+"/dist/acme/demo/1.0.0.0.zip" || v.Dist.Shasum != sha1hex(v1) {
		t.Fatalf("dist: %+v", v.Dist)
	}
	if vs[0].Version != "2.0.0-beta1" || vs[0].Dist.URL != base+"/dist/acme/demo/2.0.0.0-beta1.zip" {
		t.Fatalf("beta entry: %+v", vs[0])
	}
	dev := e.metadata(t, "composer-hosted", "acme/demo", true)
	if dv := dev.Packages["acme/demo"]; len(dv) != 1 || dv[0].Normalized != "dev-main" || dv[0].Dist.URL != base+"/dist/acme/demo/dev-main.zip" {
		t.Fatalf("~dev: %+v", dev)
	}
	// the archive, bytes as uploaded, served as a zip; HEAD too
	w := e.do("GET", "composer-hosted", "dist/acme/demo/2.0.0.0-beta1.zip", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), v2) || w.Header().Get("Content-Type") != ctZip {
		t.Fatalf("dist: %d %s", w.Code, w.Header())
	}
	if w := e.do("HEAD", "composer-hosted", "dist/acme/demo/1.0.0.0.zip", nil); w.Code != 200 {
		t.Fatalf("HEAD dist: %d", w.Code)
	}
	for _, p := range []string{"dist/acme/demo/1.0.0.zip", "dist/acme/demo/1.0.0.0/" + sha40 + ".zip", "p2/acme/other.json", "dist/acme/other/1.0.0.0.zip"} {
		if w := e.do("GET", "composer-hosted", p, nil); w.Code != 404 {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	if got := e.sink.String(); !strings.Contains(got, "upload||composer-hosted|acme/demo@1.0.0.0\n") {
		t.Fatalf("events: %s", got)
	}

	// search: name or description substring, type filter
	var sr searchResult
	decode(t, e.do("GET", "composer-hosted", "search.json?q=DEMO", nil), &sr)
	if sr.names() != "acme/demo" || sr.Total != 1 || sr.Results[0].Description != "A demo package" {
		t.Fatalf("search: %+v", sr)
	}
	decode(t, e.do("GET", "composer-hosted", "search.json?q=demo&type=composer-plugin", nil), &sr)
	if sr.Total != 0 {
		t.Fatalf("search by type: %+v", sr)
	}
}

func TestUploadRejections(t *testing.T) {
	e := newEnv(t)
	ok := archive(t, "", composerJSON("acme/demo", ""))
	cases := []struct {
		name, repo, pkg, version string
		zip                      []byte
		want                     int
	}{
		{"wrong name", "composer-hosted", "acme/other", "1.0.0", ok, 400},
		{"bad version", "composer-hosted", "acme/demo", "latest", ok, 400},
		{"branch with slash", "composer-hosted", "acme/demo", "dev-feature/x", ok, 404}, // not even a route
		{"version field disagrees", "composer-hosted", "acme/demo", "1.0.0", archive(t, "", composerJSON("acme/demo", `,"version":"2.0.0"`)), 400},
		{"no composer.json", "composer-hosted", "acme/demo", "1.0.0", archive(t, "", ""), 400},
		{"nested too deep", "composer-hosted", "acme/demo", "1.0.0", zipBytes(t, map[string]string{"a/b/composer.json": composerJSON("acme/demo", "")}), 400},
		{"not a zip", "composer-hosted", "acme/demo", "1.0.0", []byte("nope"), 400},
		{"empty", "composer-hosted", "acme/demo", "1.0.0", nil, 400},
		{"not an object", "composer-hosted", "acme/demo", "1.0.0", zipBytes(t, map[string]string{"composer.json": "[]"}), 400},
		{"read-only", "composer-ro", "acme/demo", "1.0.0", ok, 400},
		{"proxy", "composer-proxy", "acme/demo", "1.0.0", ok, 405},
		{"group without writeMember", "composer-proxy-first", "acme/demo", "1.0.0", ok, 405},
		{"first", "composer-hosted", "acme/demo", "1.0.0", ok, 201},
		{"again (allow_once)", "composer-hosted", "acme/demo", "v1.0", ok, 409},
		{"through the group", "composer-all", "acme/demo", "1.1.0", ok, 201},
	}
	for _, c := range cases {
		if w := e.upload(t, c.repo, c.pkg, c.version, c.zip); w.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body, c.want)
		}
	}
	if w := e.do("GET", "composer-proxy", uploadPath+"acme/demo/1.0.0", nil); w.Code != 405 || w.Header().Get("Allow") != "" {
		t.Fatalf("GET upload on a proxy: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("POST", "composer-hosted", uploadPath+"acme/demo/1.0.0", nil); w.Code != 405 || w.Header().Get("Allow") != "PUT, DELETE" {
		t.Fatalf("POST upload: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("POST", "composer-hosted", "packages.json", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST root: %d %q", w.Code, w.Header().Get("Allow"))
	}
	d := e.metadata(t, "composer-hosted", "acme/demo", false)
	if vs := d.Packages["acme/demo"]; len(vs) != 2 {
		t.Fatalf("versions: %+v", vs)
	}
}

// writePolicy allow: a version can be replaced, by a user with Edit.
func TestOverwriteNeedsEdit(t *testing.T) {
	e := newEnv(t)
	a, ps := repotest.Principals(t, e.h.Store.DB, "composer-allow", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	v1 := archive(t, "", composerJSON("acme/demo", ""))
	v1b := archive(t, "", composerJSON("acme/demo", `,"keywords":["replaced"]`))
	put := func(p auth.Principal, z []byte) int {
		return e.doAs(p, "PUT", "composer-allow", uploadPath+"acme/demo/1.0.0", z).Code
	}
	if c := put(ps["adder"], v1); c != 201 {
		t.Fatalf("first: %d", c)
	}
	if c := put(ps["adder"], v1b); c != 403 {
		t.Fatalf("add-only overwrite: %d", c)
	}
	if c := put(ps["editor"], v1b); c != 201 {
		t.Fatalf("edit overwrite: %d", c)
	}
	w := e.do("GET", "composer-allow", "p2/acme/demo.json", nil)
	if !strings.Contains(w.Body.String(), `"replaced"`) || strings.Count(w.Body.String(), `"version_normalized"`) != 1 {
		t.Fatalf("after overwrite: %s", w.Body)
	}
	if w := e.do("GET", "composer-allow", "dist/acme/demo/1.0.0.0.zip", nil); !bytes.Equal(w.Body.Bytes(), v1b) {
		t.Fatal("overwrite not stored")
	}
}

func TestProxy(t *testing.T) {
	e := newEnv(t)
	base := ccBase + "/repository/composer-proxy"
	w := e.do("GET", "composer-proxy", "packages.json", nil)
	var rt map[string]any
	decode(t, w, &rt)
	if rt["metadata-url"] != base+"/p2/%package%.json" || rt["search"] != base+"/search.json?q=%query%&type=%type%" {
		t.Fatalf("root: %v", rt)
	}
	for _, k := range []string{"providers-url", "notify-batch", "list", "security-advisories", "minified"} {
		if _, ok := rt[k]; ok {
			t.Errorf("root keeps %s: %v", k, rt[k])
		}
	}
	if rt["available-packages"] != nil {
		t.Fatalf("a proxy lists no available-packages: %v", rt)
	}

	// metadata: expanded, sources dropped, dist URLs on the proxy
	d := e.metadata(t, "composer-proxy", "monolog/monolog", false)
	vs := d.Packages["monolog/monolog"]
	if d.Minified != "" || len(vs) != 2 {
		t.Fatalf("p2: %+v", d)
	}
	ref := func(name string) string { return sha1hex([]byte(e.up + "/zips/" + name + ".zip")) }
	if v := vs[0]; v.Name != "monolog/monolog" || v.Normalized != "3.5.0.0" || v.Source != nil || v.Require["php"] != ">=8.1" ||
		v.Dist.URL != base+"/dist/monolog/monolog/3.5.0.0/"+ref("monolog-3.5.0")+".zip" || v.Dist.Shasum != sha1hex(e.zips["monolog-3.5.0"]) || v.Dist.Reference != "ref-monolog-3.5.0" {
		t.Fatalf("3.5.0: %+v", v)
	}
	if v := vs[1]; v.Name != "monolog/monolog" || v.Description != "Sends your logs" || v.Normalized != "3.4.0.0" || v.Source != nil || v.Require != nil ||
		v.Time != "2026-01-01T00:00:00+00:00" || v.Dist.URL != base+"/dist/monolog/monolog/3.4.0.0/"+ref("monolog-3.4.0")+".zip" {
		t.Fatalf("3.4.0 (expanded): %+v", v)
	}
	// the archive: fetched once, cached, bytes as upstream
	path := "dist/monolog/monolog/3.4.0.0/" + ref("monolog-3.4.0") + ".zip"
	for i := 0; i < 2; i++ {
		w := e.do("GET", "composer-proxy", path, nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.zips["monolog-3.4.0"]) || w.Header().Get("Content-Type") != ctZip {
			t.Fatalf("dist %d: %d %s", i, w.Code, w.Header())
		}
	}
	if e.hits.Load() != 1 {
		t.Fatalf("upstream downloads: %d", e.hits.Load())
	}
	// a wrong shasum is a 502 and nothing is cached
	dev := e.metadata(t, "composer-proxy", "monolog/monolog", true)
	devPath := strings.TrimPrefix(dev.Packages["monolog/monolog"][0].Dist.URL, base+"/")
	if devPath != "dist/monolog/monolog/dev-main/"+ref("monolog-main")+".zip" {
		t.Fatalf("dev dist: %s", devPath)
	}
	if w := e.do("GET", "composer-proxy", devPath, nil); w.Code != 502 || !strings.Contains(w.Body.String(), "shasum") {
		t.Fatalf("shasum mismatch: %d %s", w.Code, w.Body)
	}
	if _, err := e.h.Store.Get(context.Background(), "composer-proxy", devPath); err == nil {
		t.Fatal("mismatching archive cached")
	}
	// unknown ref, hosted-shaped path, unknown package
	for _, p := range []string{"dist/monolog/monolog/3.4.0.0/" + sha40 + ".zip", "dist/monolog/monolog/3.4.0.0.zip", "p2/acme/nope.json", "dist/acme/nope/1.0.0.0/" + sha40 + ".zip"} {
		if w := e.do("GET", "composer-proxy", p, nil); w.Code != 404 {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	// search passes through, with the query
	var sr searchResult
	decode(t, e.do("GET", "composer-proxy", "search.json?q=mono+log&type=library", nil), &sr)
	if sr.names() != "monolog/monolog,acme/demo" || *e.query.Load() != "q=mono+log&type=library" {
		t.Fatalf("search: %+v %q", sr, *e.query.Load())
	}
	// the proxy's cached files derive to components the UI and cleanup show
	if _, n, v, ok := component.DeriveAsset("composer", path); !ok || n != "monolog/monolog" || v != "3.4.0.0" {
		t.Fatalf("DeriveAsset(%s) = %s %s %v", path, n, v, ok)
	}
}

func TestProxyNotV2(t *testing.T) {
	e := newEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/packages.json" {
			io.WriteString(w, `{"packages":{},"providers-url":"/p/%package%$%hash%.json"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	for name, remote := range map[string]string{"v1": ts.URL + "/v1/", "none": ts.URL + "/nope/"} {
		cfg := &config.Config{Repositories: []config.Repository{{Name: "p", Format: "composer", Type: "proxy", Remote: remote,
			MetadataMaxAge: config.Duration(time.Hour), NegativeCacheTTL: config.Duration(time.Minute)}}}
		rp := repo.Build(cfg)["p"]
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/repository/p/packages.json", nil)
		e.h.Serve(w, r, rp, "packages.json")
		if w.Code != 502 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestGroup(t *testing.T) {
	e := newEnv(t)
	v1 := archive(t, "", composerJSON("acme/demo", ""))
	if w := e.upload(t, "composer-hosted", "acme/demo", "1.0.0", v1); w.Code != 201 {
		t.Fatal(w.Code)
	}
	base := ccBase + "/repository/composer-all"
	var rt root
	decode(t, e.do("GET", "composer-all", "packages.json", nil), &rt)
	if rt.MetadataURL != base+"/p2/%package%.json" || rt.Available != nil {
		t.Fatalf("group root: %+v", rt)
	}
	// hosted first: its acme/demo answers alone, even though the proxy has a 9.0.0
	d := e.metadata(t, "composer-all", "acme/demo", false)
	if vs := d.Packages["acme/demo"]; len(vs) != 1 || vs[0].Normalized != "1.0.0.0" || vs[0].Dist.URL != base+"/dist/acme/demo/1.0.0.0.zip" {
		t.Fatalf("owner hosted: %+v", d)
	}
	if w := e.do("GET", "composer-all", "dist/acme/demo/1.0.0.0.zip", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), v1) {
		t.Fatalf("group dist (hosted): %d", w.Code)
	}
	// proxy first: the upstream's acme/demo answers
	d = e.metadata(t, "composer-proxy-first", "acme/demo", false)
	if vs := d.Packages["acme/demo"]; len(vs) != 1 || vs[0].Normalized != "9.0.0.0" || !strings.HasPrefix(vs[0].Dist.URL, ccBase+"/repository/composer-proxy-first/dist/acme/demo/9.0.0.0/") {
		t.Fatalf("owner proxy: %+v", d)
	}
	// a package only the proxy has, under the group's URLs, and its archive
	d = e.metadata(t, "composer-all", "monolog/monolog", false)
	vs := d.Packages["monolog/monolog"]
	if len(vs) != 2 || !strings.HasPrefix(vs[0].Dist.URL, base+"/dist/monolog/monolog/3.5.0.0/") {
		t.Fatalf("group p2 from proxy: %+v", vs)
	}
	if w := e.do("GET", "composer-all", strings.TrimPrefix(vs[0].Dist.URL, base+"/"), nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.zips["monolog-3.5.0"]) {
		t.Fatalf("group dist (proxy): %d", w.Code)
	}
	if w := e.do("GET", "composer-all", "p2/acme/nope.json", nil); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
	// search merges, the first member keeping a name
	var sr searchResult
	decode(t, e.do("GET", "composer-all", "search.json?q=demo", nil), &sr)
	if sr.names() != "acme/demo,monolog/monolog" || sr.Results[0].Description != "A demo package" || sr.Total != 2 {
		t.Fatalf("group search: %+v", sr)
	}
	// an offline member is skipped
	off := false
	e.repos["composer-proxy"].Online = &off
	if w := e.do("GET", "composer-all", "p2/monolog/monolog.json", nil); w.Code != 404 {
		t.Fatalf("offline proxy member: %d", w.Code)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	z := archive(t, "", composerJSON("acme/demo", ""))
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if w := e.upload(t, "composer-hosted", "acme/demo", v, z); w.Code != 201 {
			t.Fatal(w.Code)
		}
	}
	if w := e.do("DELETE", "composer-hosted", uploadPath+"acme/demo/v1.0", nil); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "composer-hosted", uploadPath+"acme/demo/1.0.0", nil); w.Code != 404 {
		t.Fatalf("delete again: %d", w.Code)
	}
	if w := e.do("DELETE", "composer-ro", uploadPath+"acme/demo/1.0.0", nil); w.Code != 400 {
		t.Fatalf("delete read-only: %d", w.Code)
	}
	d := e.metadata(t, "composer-hosted", "acme/demo", false)
	if vs := d.Packages["acme/demo"]; len(vs) != 1 || vs[0].Normalized != "1.1.0.0" {
		t.Fatalf("after delete: %+v", vs)
	}
	if w := e.do("GET", "composer-hosted", "dist/acme/demo/1.0.0.0.zip", nil); w.Code != 404 {
		t.Fatalf("deleted dist: %d", w.Code)
	}
	// the component deleter, as the UI and cleanup call it
	ctx := context.Background()
	c := component.Component{Repo: "composer-hosted", Format: "composer", Name: "acme/demo", Version: "1.1.0.0"}
	res, err := e.h.DeleteComponent(ctx, e.repos["composer-hosted"], c, "admin")
	if err != nil || res.Assets != 1 || res.Bytes != int64(len(z)) {
		t.Fatalf("DeleteComponent: %+v %v", res, err)
	}
	if w := e.do("GET", "composer-hosted", "p2/acme/demo.json", nil); w.Code != 404 {
		t.Fatalf("after last delete: %d", w.Code)
	}
	if res, err := e.h.DeleteComponent(ctx, e.repos["composer-hosted"], c, "admin"); err != nil || res.Assets != 0 {
		t.Fatalf("gone already: %+v %v", res, err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["composer-proxy"], c, "admin"); err == nil {
		t.Fatal("proxy delete accepted")
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["composer-hosted"], component.Component{Format: "npm", Name: "x", Version: "1"}, "admin"); err == nil {
		t.Fatal("wrong format accepted")
	}
	got := e.sink.String()
	if !strings.Contains(got, "delete||composer-hosted|acme/demo@1.0.0.0\n") || !strings.Contains(got, "delete|admin|composer-hosted|acme/demo@1.1.0.0\n") {
		t.Fatalf("events: %s", got)
	}
	var rt root
	decode(t, e.do("GET", "composer-hosted", "packages.json", nil), &rt)
	if len(rt.Available) != 0 {
		t.Fatalf("available after delete: %v", rt.Available)
	}
}
