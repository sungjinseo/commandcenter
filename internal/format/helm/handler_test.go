package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

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

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// chartTgz builds a chart tarball as helm package does: <name>/Chart.yaml,
// values.yaml and a template, gzip-compressed. chartYAML "" writes a v2
// Chart.yaml with a description, appVersion and one dependency.
func chartTgz(t *testing.T, name, version, chartYAML string) []byte {
	t.Helper()
	if chartYAML == "" {
		chartYAML = "apiVersion: v2\nname: " + name + "\nversion: " + version + "\ndescription: A demo chart\nappVersion: \"1.16.0\"\ntype: application\n" +
			"dependencies:\n  - name: common\n    version: 2.x.x\n    repository: https://charts.example.com/common\n    condition: common.enabled\n"
	}
	return tgz(t, map[string]string{
		name + "/Chart.yaml":                chartYAML,
		name + "/values.yaml":               "replicaCount: 1\n",
		name + "/templates/deployment.yaml": "kind: Deployment\n",
	})
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, p := range sortedKeys(files) {
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(files[p])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(files[p]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
	sink  *repotest.Sink
	hits  atomic.Int32
	index atomic.Pointer[string] // the upstream index.yaml
	up    string                 // the upstream server URL
	down  atomic.Bool            // the upstream answers 503
	now   time.Time
}

// upstream serves a chart repository at /charts/: its index.yaml (e.index)
// and charts, one of them at an absolute URL elsewhere on the server.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	if e.down.Load() {
		http.Error(w, "down", 503)
		return
	}
	switch r.URL.Path {
	case "/charts/index.yaml":
		io.WriteString(w, *e.index.Load())
	case "/charts/other-1.0.0.tgz", "/files/demo-2.0.0.tgz", "/charts/bad-1.0.0.tgz":
		e.hits.Add(1)
		w.Write(upChart[filepath.Base(r.URL.Path)])
	default:
		http.NotFound(w, r)
	}
}

var upChart = map[string][]byte{}

// upstreamIndex is an index.yaml as helm repo index writes one: absolute and
// relative urls, digests (bad's is wrong), created timestamps.
func upstreamIndex(up string) string {
	return "apiVersion: v1\nentries:\n" +
		"  demo:\n  - apiVersion: v2\n    appVersion: 2.0.0\n    created: \"2026-01-02T03:04:05.000000006Z\"\n    description: upstream demo\n    digest: " + sum(upChart["demo-2.0.0.tgz"]) + "\n    name: demo\n    urls:\n    - " + up + "/files/demo-2.0.0.tgz\n    version: 2.0.0\n" +
		"  other:\n  - apiVersion: v2\n    created: 2026-01-02T03:04:05Z\n    digest: " + sum(upChart["other-1.0.0.tgz"]) + "\n    name: other\n    urls:\n    - other-1.0.0.tgz\n    version: 1.0.0\n" +
		"  bad:\n  - name: bad\n    version: 1.0.0\n    digest: " + sum([]byte("not the chart")) + "\n    urls:\n    - bad-1.0.0.tgz\n" +
		"  nourl:\n  - name: nourl\n    version: 1.0.0\n" +
		"generated: \"2026-01-02T03:04:05Z\"\n"
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	ts := httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(ts.Close)
	e.up = ts.URL
	for _, c := range []struct{ name, version string }{{"other", "1.0.0"}, {"demo", "2.0.0"}, {"bad", "1.0.0"}} {
		upChart[ChartFile(c.name, c.version)] = chartTgz(t, c.name, c.version, "")
	}
	idx := upstreamIndex(ts.URL)
	e.index.Store(&idx)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.sink = &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: func() time.Time { return e.now }, Events: e.sink}
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "helm-hosted", Format: "helm", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "helm-allow", Format: "helm", Type: "hosted", WritePolicy: "allow"},
		{Name: "helm-ro", Format: "helm", Type: "hosted", WritePolicy: "deny"},
		{Name: "helm-proxy", Format: "helm", Type: "proxy", Remote: ts.URL + "/charts/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "helm-all", Format: "helm", Type: "group", Members: []string{"helm-hosted", "helm-proxy"}, WriteMember: "helm-hosted"},
		{Name: "helm-proxy-first", Format: "helm", Type: "group", Members: []string{"helm-proxy", "helm-hosted"}},
	}}
	f := proxy.New(store, ts.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, f, auth.New(cfg, db))
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
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// index parses an index.yaml response.
func index(t *testing.T, w *httptest.ResponseRecorder) *indexFile {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != ctYAML {
		t.Fatalf("index.yaml: %d %v %s", w.Code, w.Header(), w.Body)
	}
	ix, err := parseIndex(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("index.yaml: %v\n%s", err, w.Body)
	}
	return ix
}

func versions(ix *indexFile, name string) []string {
	var out []string
	for _, e := range ix.Entries[name] {
		out = append(out, e.str("version"))
	}
	return out
}

func TestHostedPublishAndIndex(t *testing.T) {
	e := newEnv(t)
	ix := index(t, e.do("GET", "helm-hosted", "index.yaml", nil))
	if ix.APIVersion != "v1" || len(ix.Entries) != 0 || ix.Generated.IsZero() {
		t.Fatalf("empty repository index: %+v", ix) // helm repo add needs a 200
	}
	demo := chartTgz(t, "demo", "1.0.0", "")
	if w := e.do("PUT", "helm-hosted", "demo-1.0.0.tgz", demo); w.Code != 201 || w.Body.String() != "{\"saved\":true}\n" {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "helm-hosted", Subject: "demo@1.0.0"}) {
		t.Fatalf("events: %v", got)
	}
	ix = index(t, e.do("GET", "helm-hosted", "index.yaml", nil))
	en := ix.Entries["demo"]
	if len(en) != 1 {
		t.Fatalf("entries: %+v", ix.Entries)
	}
	deps, _ := en[0]["dependencies"].([]any)
	dep, _ := deps[0].(entry)
	urls, _ := en[0]["urls"].([]any)
	switch {
	case en[0].str("name") != "demo", en[0].str("version") != "1.0.0", en[0].str("description") != "A demo chart", en[0].str("appVersion") != "1.16.0", en[0].str("apiVersion") != "v2":
		t.Fatalf("Chart.yaml fields: %+v", en[0])
	case len(deps) != 1 || dep["name"] != "common" || dep["version"] != "2.x.x" || dep["repository"] != "https://charts.example.com/common":
		t.Fatalf("dependencies: %+v", deps)
	case en[0].str("digest") != sum(demo):
		t.Fatalf("digest %q", en[0].str("digest"))
	case len(urls) != 1 || urls[0] != "demo-1.0.0.tgz":
		t.Fatalf("urls %v: relative to the repository", urls)
	}
	if c, err := time.Parse(time.RFC3339Nano, en[0].str("created")); err != nil || !c.Equal(e.now) {
		t.Fatalf("created %v (%T), want %v", en[0]["created"], en[0]["created"], e.now)
	}
	w := e.do("GET", "helm-hosted", "demo-1.0.0.tgz", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), demo) || w.Header().Get("Content-Type") != ctChart {
		t.Fatalf("GET chart: %d %v", w.Code, w.Header())
	}
	if w := e.do("HEAD", "helm-hosted", "demo-1.0.0.tgz", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD chart: %d %d", w.Code, w.Body.Len())
	}
	// allow_once: the same version again is a 409, whatever its content
	if w := e.do("PUT", "helm-hosted", "demo-1.0.0.tgz", chartTgz(t, "demo", "1.0.0", "name: demo\nversion: 1.0.0\n")); w.Code != 409 {
		t.Fatalf("republish: %d %s", w.Code, w.Body)
	}
	// versions are listed newest first, whatever the upload order
	for _, v := range []string{"1.2.0", "1.1.0", "1.10.0-rc.1"} {
		if w := e.do("PUT", "helm-hosted", ChartFile("demo", v), chartTgz(t, "demo", v, "")); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", v, w.Code, w.Body)
		}
	}
	ix = index(t, e.do("GET", "helm-hosted", "index.yaml", nil))
	if got := strings.Join(versions(ix, "demo"), ","); got != "1.10.0-rc.1,1.2.0,1.1.0,1.0.0" {
		t.Fatalf("order: %s", got)
	}
	// the index is a stored asset the UI can list and download
	if a, err := e.h.Store.Get(context.Background(), "helm-hosted", "index.yaml"); err != nil || a.ContentType != ctYAML {
		t.Fatalf("index asset: %+v %v", a, err)
	}
}

func TestHostedRejections(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, file string
		body       []byte
		want       int
		msg        string
	}{
		{"file name of another chart", "other-1.0.0.tgz", chartTgz(t, "demo", "1.0.0", ""), 400, "does not match chart demo version 1.0.0"},
		{"not a tarball", "demo-1.0.0.tgz", []byte("plain text"), 400, "not a gzip-compressed tar"},
		{"no Chart.yaml", "demo-1.0.0.tgz", tgz(t, map[string]string{"demo/values.yaml": "a: 1\n"}), 400, "no Chart.yaml"},
		{"Chart.yaml at the root", "demo-1.0.0.tgz", tgz(t, map[string]string{"Chart.yaml": "name: demo\nversion: 1.0.0\n"}), 400, "no Chart.yaml"},
		{"not semver", "demo-1.0.tgz", chartTgz(t, "demo", "1.0", "name: demo\nversion: \"1.0\"\n"), 400, "SemVer 2"},
		{"no name", "demo-1.0.0.tgz", chartTgz(t, "demo", "1.0.0", "version: 1.0.0\n"), 400, "needs a name"},
		{"name with a slash", "demo-1.0.0.tgz", chartTgz(t, "demo", "1.0.0", "name: ../demo\nversion: 1.0.0\n"), 400, "needs a name"},
		{"Chart.yaml not a mapping", "demo-1.0.0.tgz", chartTgz(t, "demo", "1.0.0", "- a\n"), 400, "not a YAML mapping"},
		{"empty", "demo-1.0.0.tgz", nil, 400, "empty upload"},
	}
	for _, c := range cases {
		w := e.do("PUT", "helm-hosted", c.file, c.body)
		if w.Code != c.want || !strings.Contains(w.Body.String(), c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, w.Code, w.Body, c.want, c.msg)
		}
	}
	if ix := index(t, e.do("GET", "helm-hosted", "index.yaml", nil)); len(ix.Entries) != 0 {
		t.Fatalf("a refused upload left entries: %+v", ix.Entries)
	}
	if w := e.do("PUT", "helm-ro", "demo-1.0.0.tgz", chartTgz(t, "demo", "1.0.0", "")); w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	// "+" in a version is "_" in the file helm package writes
	if w := e.do("PUT", "helm-hosted", "demo-1.0.0_build.2.tgz", chartTgz(t, "demo", "1.0.0+build.2", "name: demo\nversion: 1.0.0+build.2\n")); w.Code != 201 {
		t.Fatalf("build metadata: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "helm-hosted", "demo-1.0.0+build.2.tgz", nil); w.Code != 200 {
		t.Fatalf("GET build metadata: %d", w.Code)
	}
	for _, c := range []struct{ method, path, allow string }{
		{"DELETE", "index.yaml", "GET, HEAD"}, {"POST", "demo-1.0.0.tgz", "GET, HEAD, PUT"}, {"GET", "api/charts", "POST"},
	} {
		if w := e.do(c.method, "helm-hosted", c.path, nil); w.Code != 405 || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s: %d Allow %q", c.method, c.path, w.Code, w.Header().Get("Allow"))
		}
	}
	// a proxy takes no upload: an empty Allow
	for _, c := range []struct{ method, path, allow string }{{"PUT", "demo-1.0.0.tgz", "GET, HEAD"}, {"POST", "api/charts", ""}} {
		w := e.do(c.method, "helm-proxy", c.path, chartTgz(t, "demo", "1.0.0", ""))
		if w.Code != 405 || w.Header().Get("Allow") != c.allow {
			t.Errorf("proxy %s %s: %d Allow %q", c.method, c.path, w.Code, w.Header().Get("Allow"))
		}
	}
	if w := e.do("GET", "helm-hosted", "nothere-1.0.0.tgz", nil); w.Code != 404 {
		t.Fatalf("missing chart: %d", w.Code)
	}
}

// writePolicy allow: a version can be replaced, by a user with Edit.
func TestOverwriteNeedsEdit(t *testing.T) {
	e := newEnv(t)
	a, ps := repotest.Principals(t, e.h.Store.DB, "helm-allow", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	v1 := chartTgz(t, "demo", "1.0.0", "")
	v1b := chartTgz(t, "demo", "1.0.0", "name: demo\nversion: 1.0.0\ndescription: replaced\n")
	if w := e.doAs(ps["adder"], "PUT", "helm-allow", "demo-1.0.0.tgz", v1); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["adder"], "PUT", "helm-allow", "demo-1.0.0.tgz", v1b); w.Code != 403 {
		t.Fatalf("add-only overwrite: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["editor"], "PUT", "helm-allow", "demo-1.0.0.tgz", v1b); w.Code != 201 {
		t.Fatalf("edit overwrite: %d %s", w.Code, w.Body)
	}
	ix := index(t, e.do("GET", "helm-allow", "index.yaml", nil))
	if en := ix.Entries["demo"]; len(en) != 1 || en[0].str("digest") != sum(v1b) || en[0].str("description") != "replaced" {
		t.Fatalf("index after overwrite: %+v", en)
	}
	if w := e.do("GET", "helm-allow", "demo-1.0.0.tgz", nil); !bytes.Equal(w.Body.Bytes(), v1b) {
		t.Fatal("overwrite not stored")
	}
}

// helm cm-push: POST api/charts, a multipart form with the file in "chart".
func TestPostAPICharts(t *testing.T) {
	e := newEnv(t)
	form := func(field string, chart []byte) ([]byte, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile(field, "demo-1.0.0.tgz")
		fw.Write(chart)
		mw.Close()
		return buf.Bytes(), mw.FormDataContentType()
	}
	body, ct := form("chart", chartTgz(t, "demo", "1.0.0", ""))
	// through the group: its writeMember takes it
	if w := e.do("POST", "helm-all", "api/charts", body, "Content-Type", ct); w.Code != 201 || w.Body.String() != "{\"saved\":true}\n" {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	if ix := index(t, e.do("GET", "helm-hosted", "index.yaml", nil)); len(ix.Entries["demo"]) != 1 {
		t.Fatalf("index: %+v", ix.Entries)
	}
	if w := e.do("POST", "helm-hosted", "api/charts", body, "Content-Type", ct); w.Code != 409 {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
	body, ct = form("file", chartTgz(t, "demo", "1.1.0", ""))
	if w := e.do("POST", "helm-hosted", "api/charts", body, "Content-Type", ct); w.Code != 400 || !strings.Contains(w.Body.String(), `"chart" field`) {
		t.Fatalf("wrong field: %d %s", w.Code, w.Body)
	}
	// ChartMuseum's other form: the file as the body
	if w := e.do("POST", "helm-hosted", "api/charts", chartTgz(t, "demo", "1.1.0", ""), "Content-Type", "application/octet-stream"); w.Code != 201 {
		t.Fatalf("raw body: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 2 || got[1].Subject != "demo@1.1.0" || got[1].Repo != "helm-hosted" {
		t.Fatalf("events: %v", got)
	}
}

func TestProxy(t *testing.T) {
	e := newEnv(t)
	ix := index(t, e.do("GET", "helm-proxy", "index.yaml", nil))
	for name, want := range map[string][]any{"demo": {"demo-2.0.0.tgz"}, "other": {"other-1.0.0.tgz"}, "bad": {"bad-1.0.0.tgz"}} {
		en := ix.Entries[name]
		if len(en) != 1 || !yamlEqual(en[0]["urls"], want) {
			t.Fatalf("%s: %+v, want urls %v", name, en, want)
		}
	}
	if _, ok := ix.Entries["nourl"]; ok {
		t.Fatal("an entry without urls cannot be served and is dropped")
	}
	if en := ix.Entries["demo"][0]; en.str("description") != "upstream demo" || en.str("digest") != sum(upChart["demo-2.0.0.tgz"]) {
		t.Fatalf("entry fields kept: %+v", en)
	}
	// a chart is fetched from the URL the upstream index gives (absolute or
	// relative), verified against its digest, and cached
	for _, f := range []string{"demo-2.0.0.tgz", "other-1.0.0.tgz"} {
		for i := 0; i < 2; i++ {
			w := e.do("GET", "helm-proxy", f, nil)
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), upChart[f]) || w.Header().Get("Content-Type") != ctChart {
				t.Fatalf("%s #%d: %d %v", f, i, w.Code, w.Header())
			}
		}
	}
	if e.hits.Load() != 2 {
		t.Fatalf("upstream hits %d, want 2 (cached after the first)", e.hits.Load())
	}
	if w := e.do("GET", "helm-proxy", "bad-1.0.0.tgz", nil); w.Code != 502 {
		t.Fatalf("digest mismatch: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "helm-proxy", "nothere-1.0.0.tgz", nil); w.Code != 404 {
		t.Fatalf("not in the index: %d", w.Code)
	}
	// the derived index is stored and reused while the upstream copy stands
	a1, err := e.h.Store.Get(context.Background(), "helm-proxy", "index.yaml")
	if err != nil {
		t.Fatal(err)
	}
	index(t, e.do("GET", "helm-proxy", "index.yaml", nil))
	if a2, _ := e.h.Store.Get(context.Background(), "helm-proxy", "index.yaml"); a2.CreatedAt != a1.CreatedAt || a2.RemoteETag != a1.RemoteETag {
		t.Fatalf("derived index regenerated: %+v %+v", a1, a2)
	}
	// after metadataMaxAge the upstream index is read again
	idx := strings.Replace(*e.index.Load(), "    version: 1.0.0\n  bad:", "    version: 1.0.0\n  - name: other\n    version: 1.1.0\n    digest: x\n    urls:\n    - other-1.1.0.tgz\n  bad:", 1)
	e.index.Store(&idx)
	index(t, e.do("GET", "helm-proxy", "index.yaml", nil))
	e.now = e.now.Add(2 * time.Hour)
	ix = index(t, e.do("GET", "helm-proxy", "index.yaml", nil))
	if got := strings.Join(versions(ix, "other"), ","); got != "1.0.0,1.1.0" {
		t.Fatalf("refreshed index: %s", got)
	}
	// the upstream down: the expired copy is served, marked stale
	e.down.Store(true)
	e.now = e.now.Add(2 * time.Hour)
	w := e.do("GET", "helm-proxy", "index.yaml", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Warning"), "Stale") {
		t.Fatalf("stale index: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "helm-proxy", "demo-2.0.0.tgz", nil); w.Code != 200 {
		t.Fatalf("cached chart while down: %d", w.Code)
	}
	if w := e.do("GET", "helm-proxy", "other-1.1.0.tgz", nil); w.Code != 502 {
		t.Fatalf("uncached chart while down: %d %s", w.Code, w.Body)
	}
}

func TestProxyUpstreamNotYAML(t *testing.T) {
	e := newEnv(t)
	idx := "entries: [not, a, map\n"
	e.index.Store(&idx)
	if w := e.do("GET", "helm-proxy", "index.yaml", nil); w.Code != 502 {
		t.Fatalf("invalid upstream index: %d %s", w.Code, w.Body)
	}
}

func yamlEqual(a, b any) bool {
	x, _ := yaml.Marshal(a)
	y, _ := yaml.Marshal(b)
	return bytes.Equal(x, y)
}

// A group's index lists each chart name from the first member that has it
// (the owner), and its downloads follow the same rule.
func TestGroup(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "helm-all", "demo-1.0.0.tgz", chartTgz(t, "demo", "1.0.0", "")); w.Code != 201 {
		t.Fatalf("PUT through the group: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "helm-proxy-first", "demo-1.0.0.tgz", chartTgz(t, "demo", "1.0.0", "")); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("PUT to a group without writeMember: %d Allow %q", w.Code, w.Header().Get("Allow"))
	}
	ix := index(t, e.do("GET", "helm-all", "index.yaml", nil))
	if got := strings.Join(versions(ix, "demo"), ","); got != "1.0.0" {
		t.Fatalf("hosted first owns demo: %s", got)
	}
	if got := strings.Join(versions(ix, "other"), ","); got != "1.0.0" {
		t.Fatalf("other from the proxy: %s", got)
	}
	if urls, _ := ix.Entries["other"][0]["urls"].([]any); len(urls) != 1 || urls[0] != "other-1.0.0.tgz" {
		t.Fatalf("urls stay relative: %v", urls)
	}
	// downloads: the owner's version, not the proxy's other version of the name
	if w := e.do("GET", "helm-all", "demo-1.0.0.tgz", nil); w.Code != 200 {
		t.Fatalf("owner's chart: %d", w.Code)
	}
	if w := e.do("GET", "helm-all", "demo-2.0.0.tgz", nil); w.Code != 404 {
		t.Fatalf("a version the owner does not have: %d, want 404", w.Code)
	}
	if w := e.do("GET", "helm-all", "other-1.0.0.tgz", nil); w.Code != 200 || e.hits.Load() != 1 {
		t.Fatalf("proxy member's chart: %d hits %d", w.Code, e.hits.Load())
	}
	// the proxy first: it owns demo, so the hosted 1.0.0 is hidden
	ix = index(t, e.do("GET", "helm-proxy-first", "index.yaml", nil))
	if got := strings.Join(versions(ix, "demo"), ","); got != "2.0.0" {
		t.Fatalf("proxy first owns demo: %s", got)
	}
	if w := e.do("GET", "helm-proxy-first", "demo-1.0.0.tgz", nil); w.Code != 404 {
		t.Fatalf("hidden hosted version: %d, want 404", w.Code)
	}
	// a new push changes the merged index
	if w := e.do("PUT", "helm-hosted", "demo-1.1.0.tgz", chartTgz(t, "demo", "1.1.0", "")); w.Code != 201 {
		t.Fatal(w.Code)
	}
	ix = index(t, e.do("GET", "helm-all", "index.yaml", nil))
	if got := strings.Join(versions(ix, "demo"), ","); got != "1.1.0,1.0.0" {
		t.Fatalf("merged index after a push: %s", got)
	}
	// a failing member is a 502, not skipped
	e.down.Store(true)
	e.now = e.now.Add(2 * time.Hour)
	fresh := newEnv(t) // nothing cached: the proxy member fails
	fresh.down.Store(true)
	if w := fresh.do("GET", "helm-all", "index.yaml", nil); w.Code != 502 {
		t.Fatalf("failing member: %d %s", w.Code, w.Body)
	}
	if w := fresh.do("GET", "helm-all", "other-1.0.0.tgz", nil); w.Code != 502 {
		t.Fatalf("failing member download: %d %s", w.Code, w.Body)
	}
}

func TestDeleteComponent(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if w := e.do("PUT", "helm-hosted", ChartFile("demo", v), chartTgz(t, "demo", v, "")); w.Code != 201 {
			t.Fatal(w.Code)
		}
	}
	e.sink.Reset()
	ctx := context.Background()
	rp := e.repos["helm-hosted"]
	res, err := e.h.DeleteComponent(ctx, rp, component.Component{Repo: "helm-hosted", Format: "helm", Name: "demo", Version: "1.0.0"}, "admin")
	if err != nil || res.Assets != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Kind != "delete" || got[0].Actor != "admin" || got[0].Subject != "demo@1.0.0" {
		t.Fatalf("events: %v", got)
	}
	if w := e.do("GET", "helm-hosted", "demo-1.0.0.tgz", nil); w.Code != 404 {
		t.Fatalf("deleted chart: %d", w.Code)
	}
	ix := index(t, e.do("GET", "helm-hosted", "index.yaml", nil))
	if got := strings.Join(versions(ix, "demo"), ","); got != "1.1.0" {
		t.Fatalf("index after delete: %s", got)
	}
	// gone already: no error, no event
	if res, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "helm", Name: "demo", Version: "1.0.0"}, "admin"); err != nil || res.Assets != 0 || len(e.sink.Events()) != 1 {
		t.Fatalf("again: %+v %v %v", res, err, e.sink.Events())
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["helm-proxy"], component.Component{Format: "helm", Name: "demo", Version: "1.0.0"}, "admin"); err == nil {
		t.Fatal("proxy: want ErrNotSupported")
	}
	if _, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "helm", Name: "demo"}, "admin"); err == nil {
		t.Fatal("incomplete component: want an error")
	}
}
