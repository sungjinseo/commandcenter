package p2

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	"commandcenter/internal/xz"
)

// jar zips files (name -> content) as a p2 jar is zipped.
func jar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	zw.Close()
	return buf.Bytes()
}

// unjar reads a jar back.
func unjar(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(body)
	}
	return out
}

func sha1hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

const p2Index = "version=1\nmetadata.repository.factory.order=content.xml,\\!\nartifact.repository.factory.order=artifacts.xml,\\!\n"

func composite(children ...string) string {
	var sb strings.Builder
	sb.WriteString("<?xml version='1.0' encoding='UTF-8'?>\n<?compositeMetadataRepository version='1.0.0'?>\n<repository name='c' type='org.eclipse.equinox.internal.p2.metadata.repository.CompositeMetadataRepository' version='1.0.0'>\n  <children size='" + string(rune('0'+len(children))) + "'>\n")
	for _, c := range children {
		sb.WriteString("    <child location='" + c + "'/>\n")
	}
	sb.WriteString("  </children>\n</repository>\n")
	return sb.String()
}

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
	db    *meta.DB
	up    string // the upstream server URL
	now   time.Time
	mu    sync.Mutex
	hits  map[string]int
	files map[string][]byte // the upstream's files by URL path
	mod   string            // the upstream's Last-Modified
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), hits: map[string]int{}, mod: "Wed, 01 Oct 2026 00:00:00 GMT"}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hits[r.URL.Path]++
		body, ok := e.files[r.URL.Path]
		mod := e.mod
		e.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-Modified-Since") == mod {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".xml") {
			w.Header().Set("Content-Type", "application/xml")
		}
		w.Header().Set("Last-Modified", mod)
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	e.up = ts.URL
	manifest := "Manifest-Version: 1.0\r\nBundle-SymbolicName: a;singleton:=true\r\nBundle-Version: 1.0.0\r\nBundle-Name: Plugin\r\n A\r\nBundle-Vendor: Example\r\nRequire-Bundle: org.eclipse.core.runtime;bundle-version=\"[3.0.0,4.0.0)\",org.ecl\r\n ipse.ui;resolution:=optional\r\n\r\nName: x\r\nFoo: bar\r\n"
	e.files = map[string][]byte{
		// a simple site
		"/site/p2.index":    []byte(p2Index),
		"/site/content.jar": jar(t, map[string]string{"content.xml": "<?xml version='1.0'?><repository name='site'/>"}),
		"/site/artifacts.xml": []byte("<?xml version='1.0'?><repository><mappings size='1'><rule filter='(&amp; (classifier=osgi.bundle))' output='${repoUrl}/plugins/${id}_${version}.jar'/></mappings>" +
			"<artifacts size='1'><artifact classifier='osgi.bundle' id='a' version='1.0.0'/></artifacts></repository>"),
		"/site/plugins/a_1.0.0.jar":            jar(t, map[string]string{"META-INF/MANIFEST.MF": manifest}),
		"/site/plugins/a_1.0.0.jar.pack200.gz": []byte("pack200"),
		"/site/features/f_1.0.0.jar":           jar(t, map[string]string{"feature.xml": "<feature id='f' version='1.0.0'/>"}),
		"/site/binary/b_1.0.0":                 []byte("binary"),
		// a composite site: a relative child, an absolute one under the
		// remote, one elsewhere on the host and one on another host
		"/composite/compositeContent.xml":        []byte(composite("rel/", ts.URL+"/composite/abs/", ts.URL+"/elsewhere/", "http://other.example/x")),
		"/composite/compositeArtifacts.jar":      jar(t, map[string]string{"compositeArtifacts.xml": strings.ReplaceAll(composite("rel/", ts.URL+"/elsewhere/"), "Metadata", "Artifact")}),
		"/composite/sub/compositeContent.xml":    []byte(composite("../rel/", ts.URL+"/elsewhere/")),
		"/composite/rel/content.xml":             []byte("<repository name='rel'/>"),
		"/composite/abs/content.xml":             []byte("<repository name='abs'/>"),
		"/elsewhere/content.xml":                 []byte("<repository name='elsewhere'/>"),
		"/elsewhere/plugins/e_2.0.0.jar":         []byte("e"),
		"/elsewhere/nested/compositeContent.xml": []byte(composite("../", ts.URL+"/composite/rel/", ts.URL+"/elsewhere/other/")),
	}
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.URL(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db = db
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: func() time.Time { return e.now }, Events: &repotest.Sink{}}
	no := false
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "p2-site", Format: "p2", Type: "proxy", Remote: ts.URL + "/site/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "p2-proxy", Format: "p2", Type: "proxy", Remote: ts.URL + "/composite/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "p2-off", Format: "p2", Type: "proxy", Remote: ts.URL + "/nowhere/", Online: &no},
		{Name: "p2-all", Format: "p2", Type: "group", Members: []string{"p2-site", "p2-off", "p2-proxy"}},
		{Name: "p2-nested", Format: "p2", Type: "group", Members: []string{"p2-all"}},
	}}
	if err := config.ValidateRepositories(cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	f := proxy.New(store, ts.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, f)
	e.repos = repo.Build(cfg)
	return e
}

func (e *env) do(method, repoName, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, nil)
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

func (e *env) get(t *testing.T, repoName, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := e.do("GET", repoName, path)
	if w.Code != 200 {
		t.Fatalf("GET %s/%s: %d %s", repoName, path, w.Code, w.Body)
	}
	return w
}

func (e *env) hit(p string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hits[p]
}

func TestProxyPassThrough(t *testing.T) {
	e := newEnv(t)
	// the remote's Content-Type is kept (the test server sniffs zips)
	for p, ct := range map[string]string{"p2.index": "text/plain", "content.jar": "application/zip", "artifacts.xml": "application/xml",
		"plugins/a_1.0.0.jar": "application/zip", "plugins/a_1.0.0.jar.pack200.gz": "text/plain", "features/f_1.0.0.jar": "application/zip", "binary/b_1.0.0": "text/plain"} {
		w := e.get(t, "p2-site", p)
		if !bytes.Equal(w.Body.Bytes(), e.files["/site/"+p]) || !strings.HasPrefix(w.Header().Get("Content-Type"), ct) {
			t.Fatalf("%s: %q %q", p, w.Header().Get("Content-Type"), w.Body)
		}
		if w := e.do("HEAD", "p2-site", p); w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("HEAD %s: %d", p, w.Code)
		}
	}
	// artifacts.xml mappings use ${repoUrl}: nothing to rewrite
	if !strings.Contains(e.get(t, "p2-site", "artifacts.xml").Body.String(), "${repoUrl}/plugins/${id}_${version}.jar") {
		t.Fatal("artifacts.xml was altered")
	}
	// cached: content is never re-checked, metadata after metadataMaxAge
	e.now = e.now.Add(2 * time.Hour)
	e.get(t, "p2-site", "plugins/a_1.0.0.jar")
	e.get(t, "p2-site", "p2.index")
	e.get(t, "p2-site", "content.jar")
	if e.hit("/site/plugins/a_1.0.0.jar") != 1 || e.hit("/site/p2.index") != 2 || e.hit("/site/content.jar") != 2 {
		t.Fatalf("hits: %v", e.hits)
	}
	// a missing file is a 404, remembered for negativeCacheTTL
	for range 2 {
		if w := e.do("GET", "p2-site", "plugins/none_1.0.0.jar"); w.Code != 404 {
			t.Fatalf("missing: %d", w.Code)
		}
	}
	if e.hit("/site/plugins/none_1.0.0.jar") != 1 {
		t.Fatalf("negative cache: %v", e.hits)
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{{"PUT", "plugins/x_1.0.0.jar", 405}, {"DELETE", "plugins/a_1.0.0.jar", 405}, {"GET", "../x", 400}, {"GET", "plugins/", 400}, {"GET", "", 400}, {"GET", ".remote/index.json", 404}, {"GET", ".remote/xyz/content.xml", 404}} {
		if w := e.do(c.method, "p2-site", c.path); w.Code != c.want {
			t.Fatalf("%s %q: %d, want %d", c.method, c.path, w.Code, c.want)
		}
	}
	if w := e.do("PUT", "p2-site", "x"); w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("Allow: %q", w.Header().Get("Allow"))
	}
	// hosted is refused by config validation; a handler asked anyway answers 404
	if err := config.ValidateRepositories([]config.Repository{{Name: "h", Format: "p2", Type: "hosted"}}); err == nil || !strings.Contains(err.Error(), "p2 supports proxy and group repositories only") {
		t.Fatalf("hosted: %v", err)
	}
	hosted := &repo.Repository{Repository: config.Repository{Name: "h", Format: "p2", Type: "hosted"}}
	w := httptest.NewRecorder()
	e.h.Serve(w, httptest.NewRequest("GET", "/repository/h/p2.index", nil), hosted, "p2.index")
	if w.Code != 404 {
		t.Fatalf("hosted serve: %d", w.Code)
	}
}

func TestCompositeRewrite(t *testing.T) {
	e := newEnv(t)
	elsewhere, other := sha1hex(e.up+"/elsewhere/"), sha1hex("http://other.example/x/")
	body := e.get(t, "p2-proxy", "compositeContent.xml").Body.String()
	for _, want := range []string{"location='rel/'", "location='abs/'", "location='.remote/" + elsewhere + "/'", "location='.remote/" + other + "/'"} {
		if !strings.Contains(body, want) {
			t.Fatalf("compositeContent.xml lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, e.up) || strings.Contains(body, "other.example") {
		t.Fatalf("a remote URL leaked:\n%s", body)
	}
	// the table maps each mirrored child to its URL; the cached asset is the remote's
	a, err := e.h.Store.Get(context.Background(), "p2-proxy", remoteIndex)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := e.h.Store.Open(a)
	var table map[string]string
	json.NewDecoder(f).Decode(&table)
	f.Close()
	if len(table) != 2 || table[elsewhere] != e.up+"/elsewhere/" || table[other] != "http://other.example/x/" {
		t.Fatalf("table: %v", table)
	}
	if a, err := e.h.Store.Get(context.Background(), "p2-proxy", "compositeContent.xml"); err != nil || a.Size != int64(len(e.files["/composite/compositeContent.xml"])) {
		t.Fatalf("cached composite: %+v %v", a, err)
	}
	// a jar: unzipped, rewritten, zipped again
	w := e.get(t, "p2-proxy", "compositeArtifacts.jar")
	files := unjar(t, w.Body.Bytes())
	if x := files["compositeArtifacts.xml"]; len(files) != 1 || !strings.Contains(x, "location='rel/'") || !strings.Contains(x, "location='.remote/"+elsewhere+"/'") || !strings.Contains(x, "CompositeArtifactRepository") {
		t.Fatalf("compositeArtifacts.jar: %v", files)
	}
	if w.Header().Get("Content-Type") != "application/java-archive" {
		t.Fatalf("jar content type: %q", w.Header().Get("Content-Type"))
	}
	// below the root: paths are relative to the document's directory
	body = e.get(t, "p2-proxy", "sub/compositeContent.xml").Body.String()
	if !strings.Contains(body, "location='../rel/'") || !strings.Contains(body, "location='../.remote/"+elsewhere+"/'") {
		t.Fatalf("sub/compositeContent.xml:\n%s", body)
	}
	// the rewritten document is served again from the cache (one remote hit)
	e.get(t, "p2-proxy", "compositeContent.xml")
	if e.hit("/composite/compositeContent.xml") != 1 {
		t.Fatalf("hits: %v", e.hits)
	}
}

func TestRemoteChild(t *testing.T) {
	e := newEnv(t)
	elsewhere := sha1hex(e.up + "/elsewhere/")
	if w := e.do("GET", "p2-proxy", ".remote/"+elsewhere+"/content.xml"); w.Code != 404 {
		t.Fatalf("before the composite was read: %d", w.Code) // no table yet
	}
	e.get(t, "p2-proxy", "compositeContent.xml")
	if got := e.get(t, "p2-proxy", ".remote/"+elsewhere+"/content.xml").Body.String(); got != "<repository name='elsewhere'/>" {
		t.Fatalf("child content.xml: %q", got)
	}
	if got := e.get(t, "p2-proxy", ".remote/"+elsewhere+"/plugins/e_2.0.0.jar").Body.String(); got != "e" {
		t.Fatalf("child plugin: %q", got)
	}
	if a, err := e.h.Store.Get(context.Background(), "p2-proxy", ".remote/"+elsewhere+"/plugins/e_2.0.0.jar"); err != nil || a.Size != 1 {
		t.Fatalf("child plugin cached: %+v %v", a, err)
	}
	// a composite under a child: its own relative children stay, one under
	// the remote root is relative to it, another elsewhere is mirrored
	body := e.get(t, "p2-proxy", ".remote/"+elsewhere+"/nested/compositeContent.xml").Body.String()
	otherSub := sha1hex(e.up + "/elsewhere/other/")
	for _, want := range []string{"location='../'", "location='../../../rel/'", "location='../../../.remote/" + otherSub + "/'"} {
		if !strings.Contains(body, want) {
			t.Fatalf("nested composite lacks %s:\n%s", want, body)
		}
	}
	if w := e.do("GET", "p2-proxy", ".remote/"+sha1hex("http://nowhere.example/")+"/content.xml"); w.Code != 404 {
		t.Fatalf("unknown child: %d", w.Code)
	}
	if w := e.do("GET", "p2-proxy", ".remote/"+elsewhere+"/missing.xml"); w.Code != 404 {
		t.Fatalf("missing under child: %d", w.Code)
	}
}

func TestGroup(t *testing.T) {
	e := newEnv(t)
	if got := e.get(t, "p2-all", "p2.index").Body.String(); got != "version=1\nmetadata.repository.factory.order=compositeContent.xml,\\!\nartifact.repository.factory.order=compositeArtifacts.xml,\\!\n" {
		t.Fatalf("p2.index: %q", got)
	}
	content := e.get(t, "p2-all", "compositeContent.xml").Body.String()
	want := "  <children size='2'>\n    <child location='../p2-site/'/>\n    <child location='../p2-proxy/'/>\n  </children>\n"
	if !strings.Contains(content, want) || !strings.Contains(content, "<?compositeMetadataRepository version='1.0.0'?>") || !strings.Contains(content, "CompositeMetadataRepository' version='1.0.0'>") || !strings.Contains(content, "name='p2-all'") {
		t.Fatalf("compositeContent.xml:\n%s", content) // the offline member is left out
	}
	artifacts := e.get(t, "p2-all", "compositeArtifacts.xml").Body.String()
	if !strings.Contains(artifacts, want) || !strings.Contains(artifacts, "<?compositeArtifactRepository version='1.0.0'?>") || !strings.Contains(artifacts, "CompositeArtifactRepository' version='1.0.0'>") {
		t.Fatalf("compositeArtifacts.xml:\n%s", artifacts)
	}
	if !strings.Contains(e.get(t, "p2-nested", "compositeContent.xml").Body.String(), "<child location='../p2-all/'/>") {
		t.Fatal("nested group")
	}
	// a group is a composite: its members' own documents are not its
	for _, p := range []string{"compositeContent.jar", "compositeArtifacts.jar", "content.xml", "content.jar", "artifacts.xml", "site.xml"} {
		if w := e.do("GET", "p2-all", p); w.Code != 404 {
			t.Fatalf("%s on a group: %d", p, w.Code)
		}
	}
	// other paths: the first member that has them, each proxy with its own
	// table (the child mirror of p2-proxy, through the group)
	if !bytes.Equal(e.get(t, "p2-all", "plugins/a_1.0.0.jar").Body.Bytes(), e.files["/site/plugins/a_1.0.0.jar"]) {
		t.Fatal("plugin through the group")
	}
	if got := e.get(t, "p2-nested", "rel/content.xml").Body.String(); got != "<repository name='rel'/>" {
		t.Fatalf("rel/content.xml through the nested group: %q", got)
	}
	elsewhere := sha1hex(e.up + "/elsewhere/")
	e.get(t, "p2-all", "sub/compositeContent.xml")
	if got := e.get(t, "p2-all", ".remote/"+elsewhere+"/content.xml").Body.String(); got != "<repository name='elsewhere'/>" {
		t.Fatalf("child through the group: %q", got)
	}
	if w := e.do("PUT", "p2-all", "plugins/x_1.0.0.jar"); w.Code != 405 {
		t.Fatalf("PUT on a group: %d", w.Code)
	}
}

// TestGroupLastModified pins p2-04: a group's documents carry when the group
// or a member last changed, read from the database, the same on every node;
// without it a client reads them again on every load.
func TestGroupLastModified(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if w := e.get(t, "p2-all", "compositeContent.xml"); w.Header().Get("Last-Modified") != "" {
		t.Fatalf("not stored, but dated: %q", w.Header().Get("Last-Modified"))
	}
	for _, name := range []string{"p2-all", "p2-site"} {
		if err := e.db.CreateRepository(ctx, e.repos[name].Repository); err != nil {
			t.Fatal(err)
		}
	}
	lastModified := func(p string) string {
		t.Helper()
		lm := e.do("HEAD", "p2-all", p).Header().Get("Last-Modified")
		if _, err := http.ParseTime(lm); err != nil {
			t.Fatalf("%s: Last-Modified %q", p, lm)
		}
		return lm
	}
	first := lastModified("compositeContent.xml")
	if lastModified("compositeArtifacts.xml") != first || lastModified("p2.index") != first {
		t.Fatal("the documents of one group differ")
	}
	time.Sleep(1100 * time.Millisecond) // Last-Modified has seconds
	if lastModified("compositeContent.xml") != first {
		t.Fatal("Last-Modified moved with nothing changed")
	}
	def := e.repos["p2-site"].Repository
	def.MetadataMaxAge = config.Duration(2 * time.Hour)
	if err := e.db.UpdateRepository(ctx, def); err != nil {
		t.Fatal(err)
	}
	if lastModified("compositeContent.xml") == first {
		t.Fatal("Last-Modified did not move with a member")
	}
	r := httptest.NewRequest("GET", "/repository/p2-all/p2.index", nil)
	r.Header.Set("If-Modified-Since", lastModified("p2.index"))
	w := httptest.NewRecorder()
	e.h.Serve(w, r, e.repos["p2-all"], "p2.index")
	if w.Code != 304 {
		t.Fatalf("conditional GET: %d", w.Code)
	}
}

func mirrored(t *testing.T) string {
	t.Helper()
	return "<?xml version='1.0' encoding='UTF-8'?>\n<?artifactRepository version='1.1.0'?>\n<repository name='r' type='org.eclipse.equinox.p2.artifact.repository.simpleRepository' version='1'>\n" +
		"  <properties size='3'>\n    <property name='p2.timestamp' value='1'/>\n    <property name='p2.mirrorsURL' value='http://mirrors.example/list?a=1&amp;b=2'/>\n    <property name='p2.compressed' value='true'/>\n  </properties>\n" +
		"  <mappings size='1'>\n    <rule filter='(&amp; (classifier=osgi.bundle))' output='${repoUrl}/plugins/${id}_${version}.jar'/>\n  </mappings>\n" +
		"  <artifacts size='1'>\n    <artifact classifier='osgi.bundle' id='a' version='1.0.0'>\n      <properties size='1'>\n        <property name='artifact.size' value='9'/>\n      </properties>\n    </artifact>\n  </artifacts>\n</repository>\n"
}

func stripped(t *testing.T) string {
	t.Helper()
	return strings.NewReplacer("<properties size='3'>", "<properties size='2'>", "    <property name='p2.mirrorsURL' value='http://mirrors.example/list?a=1&amp;b=2'/>\n", "").Replace(mirrored(t))
}

func xzStored(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := xz.NewStoredWriter(&buf)
	io.WriteString(w, s)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func unxz(t *testing.T, b []byte) string {
	t.Helper()
	r, err := xz.NewReader(bytes.NewReader(b), xz.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestMirrorsRemoved pins p2-01: the three forms of an artifacts document, at
// the root, below it and in a mirrored child, come without p2.mirrorsURL and
// with the size corrected, however a client asks; the cached file is the
// rewritten one, and the remote's validators keep working on it.
func TestMirrorsRemoved(t *testing.T) {
	e := newEnv(t)
	e.mu.Lock()
	e.files["/site/artifacts.xml"] = []byte(mirrored(t))
	e.files["/site/artifacts.jar"] = jar(t, map[string]string{"artifacts.xml": mirrored(t)})
	e.files["/site/artifacts.xml.xz"] = xzStored(t, mirrored(t))
	e.files["/site/sub/artifacts.jar"] = e.files["/site/artifacts.jar"]
	e.files["/composite/compositeArtifacts.xml"] = []byte(strings.ReplaceAll(composite(e.up+"/elsewhere/"), "Metadata", "Artifact"))
	e.files["/elsewhere/artifacts.xml.xz"] = xzStored(t, mirrored(t))
	e.mu.Unlock()
	read := map[string]func(*testing.T, []byte) string{
		"artifacts.xml":     func(_ *testing.T, b []byte) string { return string(b) },
		"artifacts.jar":     func(t *testing.T, b []byte) string { return unjar(t, b)["artifacts.xml"] },
		"artifacts.xml.xz":  unxz,
		"sub/artifacts.jar": func(t *testing.T, b []byte) string { return unjar(t, b)["artifacts.xml"] },
	}
	for p, get := range read {
		w := e.get(t, "p2-site", p)
		if got := get(t, w.Body.Bytes()); got != stripped(t) {
			t.Fatalf("%s:\n%s", p, got)
		}
		// after the remote's, so a client holding the remote's bytes reads this
		if lm := w.Header().Get("Last-Modified"); lm != "Thu, 01 Oct 2026 00:00:01 GMT" {
			t.Fatalf("%s: Last-Modified %q", p, lm)
		}
		// what the REST API lists is what is served
		a, err := e.h.Store.Get(context.Background(), "p2-site", p)
		if err != nil || a.Size != int64(w.Body.Len()) || a.RemoteLastModified != "Wed, 01 Oct 2026 00:00:00 GMT" {
			t.Fatalf("%s cached: %+v %v", p, a, err)
		}
		if w := e.do("HEAD", "p2-site", p); w.Code != 200 {
			t.Fatalf("HEAD %s: %d", p, w.Code)
		}
	}
	// a mirrored child's document too, and through a group
	e.get(t, "p2-proxy", "compositeArtifacts.xml")
	child := ".remote/" + sha1hex(e.up+"/elsewhere/") + "/artifacts.xml.xz"
	if got := unxz(t, e.get(t, "p2-proxy", child).Body.Bytes()); got != stripped(t) {
		t.Fatalf("%s:\n%s", child, got)
	}
	if got := unxz(t, e.get(t, "p2-all", child).Body.Bytes()); got != stripped(t) {
		t.Fatalf("%s through the group:\n%s", child, got)
	}
	// Once: a second read neither asks the remote nor writes.
	before, _ := e.h.Store.Get(context.Background(), "p2-site", "artifacts.jar")
	hits := e.hit("/site/artifacts.jar")
	e.get(t, "p2-site", "artifacts.jar")
	after, _ := e.h.Store.Get(context.Background(), "p2-site", "artifacts.jar")
	if e.hit("/site/artifacts.jar") != hits || after.BlobSHA256 != before.BlobSHA256 {
		t.Fatalf("read again: %d remote requests, blob %s -> %s", e.hit("/site/artifacts.jar")-hits, before.BlobSHA256, after.BlobSHA256)
	}
	// Past metadataMaxAge the remote's 304 keeps the rewritten document.
	e.now = e.now.Add(2 * time.Hour)
	if got := unjar(t, e.get(t, "p2-site", "artifacts.jar").Body.Bytes())["artifacts.xml"]; got != stripped(t) || e.hit("/site/artifacts.jar") != hits+1 {
		t.Fatalf("after a 304: %d remote requests:\n%s", e.hit("/site/artifacts.jar")-hits, got)
	}
	// A new document from the remote is rewritten in its turn.
	e.mu.Lock()
	e.mod = "Fri, 02 Oct 2026 00:00:00 GMT"
	e.files["/site/artifacts.jar"] = jar(t, map[string]string{"artifacts.xml": strings.ReplaceAll(mirrored(t), "value='1'", "value='2'")})
	e.mu.Unlock()
	e.now = e.now.Add(2 * time.Hour)
	w := e.get(t, "p2-site", "artifacts.jar")
	if got := unjar(t, w.Body.Bytes())["artifacts.xml"]; got != strings.ReplaceAll(stripped(t), "value='1'", "value='2'") || w.Header().Get("Last-Modified") != "Fri, 02 Oct 2026 00:00:01 GMT" {
		t.Fatalf("new document %q:\n%s", w.Header().Get("Last-Modified"), got)
	}
}

// Clients that come at once all get the rewritten document, whichever of them
// fetched and rewrote it, and it is what stays cached.
func TestMirrorsRemovedForAll(t *testing.T) {
	e := newEnv(t)
	e.mu.Lock()
	e.files["/site/artifacts.jar"] = jar(t, map[string]string{"artifacts.xml": mirrored(t)})
	e.mu.Unlock()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := e.do("GET", "p2-site", "artifacts.jar")
			if got := unjar(t, w.Body.Bytes())["artifacts.xml"]; w.Code != 200 || got != stripped(t) {
				t.Errorf("%d:\n%s", w.Code, got)
			}
		}()
	}
	wg.Wait()
	if got := unjar(t, e.get(t, "p2-site", "artifacts.jar").Body.Bytes())["artifacts.xml"]; got != stripped(t) {
		t.Fatalf("after them:\n%s", got)
	}
}

// A document without the property is the remote's, byte for byte, in every
// form; one that cannot be read is served as it came.
func TestMirrorsLeftAlone(t *testing.T) {
	e := newEnv(t)
	plain := strings.ReplaceAll(mirrored(t), "    <property name='p2.mirrorsURL' value='http://mirrors.example/list?a=1&amp;b=2'/>\n", "")
	e.mu.Lock()
	e.files["/site/artifacts.xml"] = []byte(plain)
	e.files["/site/artifacts.jar"] = jar(t, map[string]string{"artifacts.xml": plain, "other.txt": "kept"})
	e.files["/site/artifacts.xml.xz"] = xzStored(t, plain)
	e.files["/site/a/artifacts.jar"] = []byte("not a jar")
	e.files["/site/b/artifacts.xml.xz"] = []byte("not xz")
	e.files["/site/c/artifacts.xml"] = []byte("<repository><properties size='1'><property name='p2.mirrorsURL' value='x'/>")
	e.files["/site/d/artifacts.jar"] = jar(t, map[string]string{"content.xml": "<repository/>"})
	e.mu.Unlock()
	for _, p := range []string{"artifacts.xml", "artifacts.jar", "artifacts.xml.xz", "a/artifacts.jar", "b/artifacts.xml.xz", "c/artifacts.xml", "d/artifacts.jar"} {
		w := e.get(t, "p2-site", p)
		if !bytes.Equal(w.Body.Bytes(), e.files["/site/"+p]) {
			t.Fatalf("%s altered: %q", p, w.Body)
		}
		if lm := w.Header().Get("Last-Modified"); lm != "Thu, 01 Oct 2026 00:00:00 GMT" {
			t.Fatalf("%s: Last-Modified %q", p, lm)
		}
	}
}

func TestStripMirrors(t *testing.T) {
	big := strings.Repeat("k", 200<<10) // longer than the read buffer, as PGP keys are
	for _, c := range []struct{ in, want string }{
		{"<properties size='1'><property name='p2.mirrorsURL' value='x'/></properties>", "<properties size='0'></properties>"},
		{`<properties size="2"><property value="x" name="p2.mirrorsURL"/><property name="a" value="b"/></properties>`, `<properties size="1"><property name="a" value="b"/></properties>`},
		{"<properties size='2'>\n <property name = 'p2.mirrorsURL' value='x'></property>\n <property name='a' value='b'/>\n</properties>", "<properties size='1'>\n <property name='a' value='b'/>\n</properties>"},
		{"<a><properties size='2'><property name='k' value='" + big + "'/><property name='p2.mirrorsURL' value='x'/></properties></a>", "<a><properties size='1'><property name='k' value='" + big + "'/></properties></a>"},
		{"<artifact><properties size='1'><property name='p2.mirrorsURL' value='x'/></properties></artifact><properties size='1'><property name='k' value='v'/></properties>", "<artifact><properties size='0'></properties></artifact><properties size='1'><property name='k' value='v'/></properties>"},
		// a size attribute that is not there stays that way
		{"<properties><property name='p2.mirrorsURL' value='x'/></properties>", "<properties></properties>"},
	} {
		var out bytes.Buffer
		removed, err := stripMirrors(&out, strings.NewReader(c.in))
		if err != nil || !removed || out.String() != c.want {
			t.Errorf("%.80q: %v %v\n%.200q", c.in, removed, err, out.String())
		}
	}
	for _, in := range []string{"", "<repository/>", "<properties size='0'/>", "<properties size='1'><property name='p2.timestamp' value='p2.mirrorsURL'/></properties>", "<property name='p2.mirrorsURL' value='outside properties'/>", "text only"} {
		var out bytes.Buffer
		if removed, err := stripMirrors(&out, strings.NewReader(in)); err != nil || removed || out.String() != in {
			t.Errorf("%q: %v %v\n%q", in, removed, err, out.String())
		}
	}
	for _, in := range []string{"<properties size='1'>", "<properties size='1'><property name='p2.mirrorsURL' value='a>b'/></properties>"} {
		if _, err := stripMirrors(io.Discard, strings.NewReader(in)); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
	if _, err := stripMirrors(io.Discard, strings.NewReader(strings.Repeat("k", maxTag+1<<17))); !errors.Is(err, errTagTooLarge) {
		t.Errorf("a tag without end: %v", err)
	}
}

// TestRoutingRuleSeesRequestPaths pins p2-03: a rule written for repository
// paths decides for a mirrored child's files, whose own URL it cannot know.
func TestRoutingRuleSeesRequestPaths(t *testing.T) {
	e := newEnv(t)
	rule, err := repo.CompileRoute(meta.RoutingRule{Name: "repo-paths", Mode: meta.RoutingAllow, Matchers: []string{`^/\.remote/.*`, `^/composite[^/]*$`}})
	if err != nil {
		t.Fatal(err)
	}
	e.h.Fetcher.RouteAllowed = func(_ *repo.Repository, p string) bool {
		ok, _ := rule.Allowed(p)
		return ok
	}
	e.get(t, "p2-proxy", "compositeContent.xml")
	child := ".remote/" + sha1hex(e.up+"/elsewhere/") + "/plugins/e_2.0.0.jar"
	if got := e.get(t, "p2-proxy", child).Body.String(); got != "e" {
		t.Fatalf("child plugin: %q", got)
	}
	if w := e.do("GET", "p2-proxy", "rel/content.xml"); w.Code != 404 {
		t.Fatalf("outside the rule: %d", w.Code) // the handler's view: the server answers 403 first
	}
}

// TestCleanupDelete: a cached plugin is a component (plugins/a@1.0.0) the
// generic proxy deleter removes; it is fetched again on the next request.
func TestCleanupDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.get(t, "p2-site", "plugins/a_1.0.0.jar")
	e.get(t, "p2-site", "plugins/a_1.0.0.jar.pack200.gz")
	rp := e.repos["p2-site"]
	g, n, v, ok := component.DeriveAsset("p2", "plugins/a_1.0.0.jar")
	if !ok || g != "plugins" || n != "a" || v != "1.0.0" {
		t.Fatalf("derive: %s %s %s %v", g, n, v, ok)
	}
	c := component.Component{Repo: "p2-site", Format: "p2", Group: "plugins", Name: "a", Version: "1.0.0", Assets: []component.Asset{{Path: "plugins/a_1.0.0.jar"}}}
	res, err := cleanup.NewRegistry(e.h.Store).Delete(ctx, rp, c, "tester")
	if err != nil || res.Assets != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if _, err := e.h.Store.Get(ctx, "p2-site", "plugins/a_1.0.0.jar"); err == nil {
		t.Fatal("still cached")
	}
	e.get(t, "p2-site", "plugins/a_1.0.0.jar")
	if e.hit("/site/plugins/a_1.0.0.jar") != 2 {
		t.Fatalf("hits: %v", e.hits)
	}
	if err := (&cleanup.Policy{Name: "p", Format: "p2", Criteria: cleanup.Criteria{ComponentAgeDays: 1}}).Validate(); err != nil {
		t.Fatalf("p2 is a policy format: %v", err)
	}
}

// TestFormatPrivilege: a role's format:p2 target grants its actions on
// every p2 repository (add, which no read default grants).
func TestFormatPrivilege(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for user, format := range map[string]string{"p2user": "p2", "mvnuser": "maven"} {
		if err := e.db.CreateRole(ctx, meta.Role{ID: "role-" + user, Privileges: []meta.Privilege{{Target: "format:" + format, Actions: []string{"add"}}}}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.CreateUser(ctx, meta.User{ID: user, PasswordHash: "x", Roles: []string{"role-" + user}}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.Open(ctx, e.db, e.db)
	if err != nil {
		t.Fatal(err)
	}
	a.RepoFormat = func(name string) string { return e.repos[name].Format }
	p2user, _ := a.Lookup("p2user")
	mvnuser, _ := a.Lookup("mvnuser")
	if !a.Can(p2user, "p2-proxy", auth.Add) || !a.Can(p2user, "p2-all", auth.Add) || a.Can(mvnuser, "p2-proxy", auth.Add) || a.Can(p2user, "p2-proxy", auth.Delete) {
		t.Fatal("format:p2 privilege")
	}
}

func TestManifest(t *testing.T) {
	e := newEnv(t)
	b := e.files["/site/plugins/a_1.0.0.jar"]
	mf := ReadManifest(bytes.NewReader(b), int64(len(b)))
	if mf["Bundle-SymbolicName"] != "a;singleton:=true" || mf["Bundle-Version"] != "1.0.0" || mf["Bundle-Name"] != "PluginA" || mf["Foo"] != "" {
		t.Fatalf("manifest: %v", mf)
	}
	names, versions := SplitHeader(mf["Require-Bundle"])
	if strings.Join(names, ",") != "org.eclipse.core.runtime,org.eclipse.ui" || strings.Join(versions, ",") != "[3.0.0,4.0.0)," {
		t.Fatalf("Require-Bundle: %v %v", names, versions)
	}
	if ReadManifest(bytes.NewReader([]byte("not a jar")), 9) != nil {
		t.Fatal("not a jar")
	}
}
