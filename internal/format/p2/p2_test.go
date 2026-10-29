package p2

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
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), hits: map[string]int{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hits[r.URL.Path]++
		body, ok := e.files[r.URL.Path]
		e.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".xml") {
			w.Header().Set("Content-Type", "application/xml")
		}
		w.Header().Set("Last-Modified", "Wed, 01 Oct 2026 00:00:00 GMT")
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
	e.h = New(store, f, func() string { return "http://cc.example/" })
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
	want := "  <children size='2'>\n    <child location='http://cc.example/repository/p2-site/'/>\n    <child location='http://cc.example/repository/p2-proxy/'/>\n  </children>\n"
	if !strings.Contains(content, want) || !strings.Contains(content, "<?compositeMetadataRepository version='1.0.0'?>") || !strings.Contains(content, "CompositeMetadataRepository' version='1.0.0'>") || !strings.Contains(content, "name='p2-all'") {
		t.Fatalf("compositeContent.xml:\n%s", content) // the offline member is left out
	}
	artifacts := e.get(t, "p2-all", "compositeArtifacts.xml").Body.String()
	if !strings.Contains(artifacts, want) || !strings.Contains(artifacts, "<?compositeArtifactRepository version='1.0.0'?>") || !strings.Contains(artifacts, "CompositeArtifactRepository' version='1.0.0'>") {
		t.Fatalf("compositeArtifacts.xml:\n%s", artifacts)
	}
	if !strings.Contains(e.get(t, "p2-nested", "compositeContent.xml").Body.String(), "<child location='http://cc.example/repository/p2-all/'/>") {
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
