package nuget

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// nuspec is a manifest as `dotnet pack` writes one: a namespaced package
// element, license expression and URL, dependency groups per framework.
func nuspec(id, version, extra string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>` + id + `</id>
    <version>` + version + `</version>
    <authors>Jane Doe, Acme</authors>
    <description>A demo package</description>
    <license type="expression">MIT</license>
    <licenseUrl>https://licenses.nuget.org/MIT</licenseUrl>
    <projectUrl>https://example.org/demo</projectUrl>
    <repository type="git" url="https://git.example.org/demo" />
    <tags>demo sample</tags>
    <dependencies>
      <group targetFramework="net6.0">
        <dependency id="Newtonsoft.Json" version="13.0.1" />
      </group>
      <group targetFramework=".NETStandard2.0">
        <dependency id="Newtonsoft.Json" version="[12.0.0, 14.0.0)" />
        <dependency id="System.Memory" version="" />
      </group>
    </dependencies>
` + extra + `
  </metadata>
</package>`
}

// nupkg builds a package as dotnet pack does: the nuspec at the root, a
// content types part and a library.
func nupkg(t *testing.T, id, version, manifest string) []byte {
	t.Helper()
	if manifest == "" {
		manifest = nuspec(id, version, "")
	}
	return zipBytes(t, map[string]string{
		"[Content_Types].xml":       "<Types/>",
		id + ".nuspec":              manifest,
		"lib/net6.0/" + id + ".dll": "MZ",
	})
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

// form wraps a package as dotnet nuget push sends it: one multipart part
// named "package" with the file name package.nupkg.
func form(t *testing.T, filename string, pkg []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("package", filename)
	fw.Write(pkg)
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

type env struct {
	h       *Handler
	repos   map[string]*repo.Repository
	sink    *repotest.Sink
	up      string       // the upstream server URL
	hits    atomic.Int32 // upstream package downloads
	queries atomic.Pointer[string]
	down    atomic.Bool
	now     time.Time
}

// upstream is a fake V3 feed as api.nuget.org lays one out: a service
// index whose newest registration base (reg-gz) splits large indexes into
// pages, an older base (reg) that search results point at, a flat
// container and search.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	if e.down.Load() {
		http.Error(w, "down", 503)
		return
	}
	up := e.up
	j := func(v string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, v)
	}
	switch p := r.URL.Path; {
	case p == "/v3/index.json":
		j(`{"version":"3.0.0","resources":[
{"@id":"` + up + `/flat/","@type":"PackageBaseAddress/3.0.0"},
{"@id":"` + up + `/reg/","@type":"RegistrationsBaseUrl"},
{"@id":"` + up + `/reg-gz/","@type":"RegistrationsBaseUrl/3.6.0"},
{"@id":"` + up + `/search-old","@type":"SearchQueryService"},
{"@id":"` + up + `/search","@type":"SearchQueryService/3.5.0"},
{"@id":"` + up + `/search","@type":"SearchQueryService/3.5.0","comment":"secondary"},
{"@id":"` + up + `/catalog/index.json","@type":"Catalog/3.0.0"}]}`)
	case p == "/flat/newtonsoft.json/index.json":
		j(`{"versions":["12.0.3","13.0.1"]}`)
	case p == "/flat/demo/index.json":
		j(`{"versions":["2.0.0"]}`)
	case strings.HasSuffix(p, ".nupkg") && upPkg(p) != nil:
		e.hits.Add(1)
		w.Write(upPkg(p))
	case strings.HasSuffix(p, ".nuspec") && strings.HasPrefix(p, "/flat/"):
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, nuspec("Newtonsoft.Json", "13.0.1", ""))
	case p == "/reg-gz/newtonsoft.json/index.json":
		j(`{"@id":"` + up + `/reg-gz/newtonsoft.json/index.json","@type":["catalog:CatalogRoot","PackageRegistration"],"count":2,"items":[
{"@id":"` + up + `/reg-gz/newtonsoft.json/index.json#page/12.0.3/12.0.3","@type":"catalog:CatalogPage","count":1,"lower":"12.0.3","upper":"12.0.3","parent":"` + up + `/reg-gz/newtonsoft.json/index.json","items":[` + upLeaf(up, "12.0.3") + `]},
{"@id":"` + up + `/reg-gz/newtonsoft.json/page/13.0.1/13.0.1.json","@type":"catalog:CatalogPage","count":1,"lower":"13.0.1","upper":"13.0.1"}]}`)
	case p == "/reg-gz/newtonsoft.json/page/13.0.1/13.0.1.json":
		j(`{"@id":"` + up + `/reg-gz/newtonsoft.json/page/13.0.1/13.0.1.json","@type":"catalog:CatalogPage","count":1,"lower":"13.0.1","upper":"13.0.1","parent":"` + up + `/reg-gz/newtonsoft.json/index.json","items":[` + upLeaf(up, "13.0.1") + `]}`)
	case p == "/reg-gz/newtonsoft.json/13.0.1.json":
		j(upLeaf(up, "13.0.1"))
	case p == "/search":
		q := r.URL.RawQuery
		e.queries.Store(&q)
		j(`{"totalHits":2,"data":[
{"@id":"` + up + `/reg/newtonsoft.json/index.json","@type":"Package","registration":"` + up + `/reg/newtonsoft.json/index.json","id":"Newtonsoft.Json","version":"13.0.1","description":"Json.NET","versions":[{"version":"13.0.1","downloads":5,"@id":"` + up + `/reg/newtonsoft.json/13.0.1.json"}]},
{"@id":"` + up + `/reg/demo/index.json","@type":"Package","registration":"` + up + `/reg/demo/index.json","id":"Demo","version":"2.0.0","description":"upstream demo","versions":[{"version":"2.0.0","downloads":1,"@id":"` + up + `/reg/demo/2.0.0.json"}]}]}`)
	default:
		http.NotFound(w, r)
	}
}

func upLeaf(up, v string) string {
	return `{"@id":"` + up + `/reg-gz/newtonsoft.json/` + v + `.json","@type":"Package","commitId":"c","commitTimeStamp":"2026-01-02T03:04:05Z",
"catalogEntry":{"@id":"` + up + `/catalog/data/2026/newtonsoft.json.` + v + `.json","@type":"PackageDetails","id":"Newtonsoft.Json","version":"` + v + `","listed":true,"published":"2026-01-02T03:04:05Z",
"packageContent":"` + up + `/flat/newtonsoft.json/` + v + `/newtonsoft.json.` + v + `.nupkg",
"dependencyGroups":[{"@type":"PackageDependencyGroup","targetFramework":"net6.0","dependencies":[{"@type":"PackageDependency","id":"Microsoft.CSharp","range":"[4.3.0, )","registration":"` + up + `/reg-gz/microsoft.csharp/index.json"}]}]},
"packageContent":"` + up + `/flat/newtonsoft.json/` + v + `/newtonsoft.json.` + v + `.nupkg","registration":"` + up + `/reg-gz/newtonsoft.json/index.json"}`
}

var upPkgs = map[string][]byte{}

func upPkg(p string) []byte { return upPkgs[filepath.Base(p)] }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	ts := httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(ts.Close)
	e.up = ts.URL
	for _, c := range []struct{ id, v string }{{"Newtonsoft.Json", "13.0.1"}, {"Newtonsoft.Json", "12.0.3"}, {"Demo", "2.0.0"}} {
		upPkgs[strings.ToLower(c.id)+"."+c.v+".nupkg"] = nupkg(t, c.id, c.v, "")
	}
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
		{Name: "nuget-hosted", Format: "nuget", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "nuget-allow", Format: "nuget", Type: "hosted", WritePolicy: "allow"},
		{Name: "nuget-ro", Format: "nuget", Type: "hosted", WritePolicy: "deny"},
		{Name: "nuget-proxy", Format: "nuget", Type: "proxy", Remote: ts.URL + "/v3/index.json",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "nuget-all", Format: "nuget", Type: "group", Members: []string{"nuget-hosted", "nuget-proxy"}, WriteMember: "nuget-hosted"},
		{Name: "nuget-proxy-first", Format: "nuget", Type: "group", Members: []string{"nuget-proxy", "nuget-hosted"}},
	}}
	f := proxy.New(store, ts.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, f, auth.New(cfg, db), func() string { return ccBase + "/" })
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

// push sends pkg as dotnet nuget push does and returns the response.
func (e *env) push(t *testing.T, repoName string, pkg []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := form(t, "package.nupkg", pkg)
	return e.do("PUT", repoName, "api/v2/package", body, "Content-Type", ct)
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

// regIndex is a registration index as NuGet.Protocol reads it.
type regIndex struct {
	ID    string   `json:"@id"`
	Type  []string `json:"@type"`
	Count int      `json:"count"`
	Items []struct {
		ID     string `json:"@id"`
		Count  int    `json:"count"`
		Lower  string `json:"lower"`
		Upper  string `json:"upper"`
		Parent string `json:"parent"`
		Items  []regLeaf
	} `json:"items"`
}

type regLeaf struct {
	ID             string `json:"@id"`
	PackageContent string `json:"packageContent"`
	Registration   string `json:"registration"`
	CatalogEntry   struct {
		ID                string   `json:"@id"`
		PkgID             string   `json:"id"`
		Version           string   `json:"version"`
		Description       string   `json:"description"`
		Authors           string   `json:"authors"`
		LicenseExpression string   `json:"licenseExpression"`
		LicenseURL        string   `json:"licenseUrl"`
		ProjectURL        string   `json:"projectUrl"`
		Listed            bool     `json:"listed"`
		Published         string   `json:"published"`
		PackageContent    string   `json:"packageContent"`
		Tags              []string `json:"tags"`
		DependencyGroups  []struct {
			TargetFramework string `json:"targetFramework"`
			Dependencies    []struct {
				ID           string `json:"id"`
				Range        string `json:"range"`
				Registration string `json:"registration"`
			} `json:"dependencies"`
		} `json:"dependencyGroups"`
	} `json:"catalogEntry"`
}

type searchResult struct {
	TotalHits int `json:"totalHits"`
	Data      []struct {
		ID           string   `json:"@id"`
		PkgID        string   `json:"id"`
		Version      string   `json:"version"`
		Registration string   `json:"registration"`
		Authors      []string `json:"authors"`
		Versions     []struct {
			Version string `json:"version"`
			ID      string `json:"@id"`
		} `json:"versions"`
	} `json:"data"`
}

func (s searchResult) ids() string {
	var out []string
	for _, d := range s.Data {
		out = append(out, d.PkgID+"@"+d.Version)
	}
	return strings.Join(out, ",")
}

func TestServiceIndex(t *testing.T) {
	e := newEnv(t)
	var idx struct {
		Version   string              `json:"version"`
		Resources []map[string]string `json:"resources"`
	}
	decode(t, e.do("GET", "nuget-hosted", "index.json", nil), &idx)
	base := ccBase + "/repository/nuget-hosted"
	want := map[string]string{
		"PackageBaseAddress/3.0.0": base + "/v3-flatcontainer/", "RegistrationsBaseUrl/3.6.0": base + "/registration/",
		"RegistrationsBaseUrl": base + "/registration/", "SearchQueryService/3.5.0": base + "/query", "PackagePublish/2.0.0": base + "/api/v2/package",
	}
	got := map[string]string{}
	for _, r := range idx.Resources {
		got[r["@type"]] = r["@id"]
	}
	for typ, id := range want {
		if got[typ] != id {
			t.Errorf("%s: %q, want %q", typ, got[typ], id)
		}
	}
	if idx.Version != "3.0.0" || got["SymbolPackagePublish/4.9.0"] != "" {
		t.Fatalf("index: %+v", idx)
	}
	if w := e.do("POST", "nuget-hosted", "index.json", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST index: %d %q", w.Code, w.Header().Get("Allow"))
	}
	// a proxy advertises what its remote has and no publish endpoint; a
	// group with a writeMember does
	decode(t, e.do("GET", "nuget-proxy", "index.json", nil), &idx)
	got = map[string]string{}
	for _, r := range idx.Resources {
		got[r["@type"]] = r["@id"]
	}
	pbase := ccBase + "/repository/nuget-proxy"
	if got["PackageBaseAddress/3.0.0"] != pbase+"/v3-flatcontainer/" || got["RegistrationsBaseUrl/3.6.0"] != pbase+"/registration/" || got["SearchQueryService/3.5.0"] != pbase+"/query" || got["PackagePublish/2.0.0"] != "" {
		t.Fatalf("proxy index: %v", got)
	}
	decode(t, e.do("GET", "nuget-all", "index.json", nil), &idx)
	found := false
	for _, r := range idx.Resources {
		found = found || r["@type"] == "PackagePublish/2.0.0" && r["@id"] == ccBase+"/repository/nuget-all/api/v2/package"
	}
	if !found {
		t.Fatalf("group index: %+v", idx)
	}
}

// TestHostedPushAndRestore: dotnet nuget push, then what dotnet restore
// reads: the flat container index and files, and the registration index.
func TestHostedPushAndRestore(t *testing.T) {
	e := newEnv(t)
	pkg := nupkg(t, "Demo.Pkg", "1.0.0.0", "")
	if w := e.push(t, "nuget-hosted", pkg); w.Code != 201 {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "nuget-hosted", Subject: "demo.pkg@1.0.0"}) {
		t.Fatalf("events: %v", got)
	}
	// allow_once: the same version again is a 409 (dotnet nuget push --skip-duplicate looks for it)
	if w := e.push(t, "nuget-hosted", nupkg(t, "demo.pkg", "1.0.0", "")); w.Code != 409 {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
	for _, v := range []string{"1.1.0-beta.1", "1.10.0", "1.2.0"} {
		if w := e.push(t, "nuget-hosted", nupkg(t, "Demo.Pkg", v, "")); w.Code != 201 {
			t.Fatalf("push %s: %d %s", v, w.Code, w.Body)
		}
	}
	// the flat container: lower-case normalized versions, oldest first, under the lower-case id
	var flat struct{ Versions []string }
	decode(t, e.do("GET", "nuget-hosted", "v3-flatcontainer/demo.pkg/index.json", nil), &flat)
	if got := strings.Join(flat.Versions, ","); got != "1.0.0,1.1.0-beta.1,1.2.0,1.10.0" {
		t.Fatalf("versions: %s", got)
	}
	decode(t, e.do("GET", "nuget-hosted", "v3-flatcontainer/Demo.Pkg/index.json", nil), &flat) // any case
	if w := e.do("GET", "nuget-hosted", "v3-flatcontainer/nothere/index.json", nil); w.Code != 404 {
		t.Fatalf("unknown id: %d", w.Code)
	}
	w := e.do("GET", "nuget-hosted", "v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pkg) || w.Header().Get("Content-Type") != ctNupkg {
		t.Fatalf("nupkg: %d %v", w.Code, w.Header())
	}
	if w := e.do("HEAD", "nuget-hosted", "v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", w.Code, w.Body.Len())
	}
	w = e.do("GET", "nuget-hosted", "v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nuspec", nil)
	if w.Code != 200 || w.Body.String() != nuspec("Demo.Pkg", "1.0.0.0", "") || w.Header().Get("Content-Type") != ctNuspec {
		t.Fatalf("nuspec: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if w := e.do("GET", "nuget-hosted", "v3-flatcontainer/demo.pkg/3.0.0/demo.pkg.3.0.0.nupkg", nil); w.Code != 404 {
		t.Fatalf("missing version: %d", w.Code)
	}
	// the registration index: one inline page, NuGet.Protocol's shape
	base := ccBase + "/repository/nuget-hosted"
	var ri regIndex
	decode(t, e.do("GET", "nuget-hosted", "registration/demo.pkg/index.json", nil), &ri)
	if ri.ID != base+"/registration/demo.pkg/index.json" || ri.Count != 1 || len(ri.Items) != 1 || len(ri.Type) != 3 {
		t.Fatalf("index: %+v", ri)
	}
	page := ri.Items[0]
	if page.Count != 4 || page.Lower != "1.0.0" || page.Upper != "1.10.0" || page.Parent != ri.ID || !strings.HasPrefix(page.ID, ri.ID+"#page/") || len(page.Items) != 4 {
		t.Fatalf("page: %+v", page)
	}
	leaf := page.Items[0]
	ce := leaf.CatalogEntry
	content := base + "/v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg"
	switch {
	case leaf.ID != base+"/registration/demo.pkg/1.0.0.json", leaf.PackageContent != content, leaf.Registration != ri.ID:
		t.Fatalf("leaf: %+v", leaf)
	case ce.PkgID != "Demo.Pkg", ce.Version != "1.0.0", ce.PackageContent != content, !ce.Listed, ce.Published != "2026-10-27T12:00:00Z":
		t.Fatalf("catalogEntry: %+v", ce)
	case ce.Description != "A demo package", ce.Authors != "Jane Doe, Acme", ce.LicenseExpression != "MIT", ce.LicenseURL != "https://licenses.nuget.org/MIT", ce.ProjectURL != "https://example.org/demo":
		t.Fatalf("catalogEntry fields: %+v", ce)
	case strings.Join(ce.Tags, " ") != "demo sample", len(ce.DependencyGroups) != 2:
		t.Fatalf("tags/deps: %+v", ce)
	}
	g := ce.DependencyGroups
	if g[0].TargetFramework != "net6.0" || len(g[0].Dependencies) != 1 || g[0].Dependencies[0].ID != "Newtonsoft.Json" || g[0].Dependencies[0].Range != "[13.0.1, )" ||
		g[0].Dependencies[0].Registration != base+"/registration/newtonsoft.json/index.json" {
		t.Fatalf("group 0: %+v", g[0])
	}
	if g[1].TargetFramework != ".NETStandard2.0" || len(g[1].Dependencies) != 2 || g[1].Dependencies[0].Range != "[12.0.0, 14.0.0)" || g[1].Dependencies[1].Range != "(, )" {
		t.Fatalf("group 1: %+v", g[1])
	}
	if page.Items[1].CatalogEntry.Version != "1.1.0-beta.1" || page.Items[3].CatalogEntry.Version != "1.10.0" {
		t.Fatalf("page order: %+v", page.Items)
	}
	// the leaf by URL (version in any case or form), and the missing ones
	var l regLeaf
	decode(t, e.do("GET", "nuget-hosted", "registration/demo.pkg/1.1.0.0-BETA.1.json", nil), &l)
	if l.CatalogEntry.Version != "1.1.0-beta.1" || l.PackageContent != base+"/v3-flatcontainer/demo.pkg/1.1.0-beta.1/demo.pkg.1.1.0-beta.1.nupkg" {
		t.Fatalf("leaf: %+v", l)
	}
	for _, p := range []string{"registration/demo.pkg/9.9.9.json", "registration/nothere/index.json", "registration/demo.pkg/page/1/2.json"} {
		if w := e.do("GET", "nuget-hosted", p, nil); w.Code != 404 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	// the files and rows the UI sees
	ctx := context.Background()
	for _, p := range []string{"demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg", "demo.pkg/1.0.0/demo.pkg.1.0.0.nuspec"} {
		if _, err := e.h.Store.Get(ctx, "nuget-hosted", p); err != nil {
			t.Fatalf("asset %s: %v", p, err)
		}
	}
	pkgs, _ := e.h.Store.DB.ListPackages(ctx, "nuget-hosted", "demo.pkg")
	if len(pkgs) != 4 || pkgs[0].Format != "nuget" {
		t.Fatalf("rows: %+v", pkgs)
	}
}

func TestHostedRejections(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		body []byte
		want int
		msg  string
	}{
		{"not a zip", []byte("plain text"), 400, "not a zip"},
		{"no nuspec", zipBytes(t, map[string]string{"lib/a.dll": "x"}), 400, "no .nuspec"},
		{"nuspec in a directory", zipBytes(t, map[string]string{"sub/demo.nuspec": nuspec("Demo", "1.0.0", "")}), 400, "no .nuspec"},
		{"not xml", nupkg(t, "Demo", "1.0.0", "{}"), 400, "not XML"},
		{"no id", nupkg(t, "Demo", "1.0.0", "<package><metadata><version>1.0.0</version></metadata></package>"), 400, "needs an id"},
		{"bad id", nupkg(t, "Demo", "1.0.0", "<package><metadata><id>../x</id><version>1.0.0</version></metadata></package>"), 400, "needs an id"},
		{"bad version", nupkg(t, "Demo", "1.0.0", "<package><metadata><id>Demo</id><version>one</version></metadata></package>"), 400, "needs a version"},
		{"symbols package type", nupkg(t, "Demo", "1.0.0", nuspec("Demo", "1.0.0", `<packageTypes><packageType name="SymbolsPackage" /></packageTypes>`)), 400, "symbol packages"},
		{"empty", nil, 400, "empty upload"},
	}
	for _, c := range cases {
		w := e.push(t, "nuget-hosted", c.body)
		if w.Code != c.want || !strings.Contains(w.Body.String(), c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, w.Code, w.Body, c.want, c.msg)
		}
	}
	// a .snupkg by file name, and the symbol endpoint
	body, ct := form(t, "demo.1.0.0.snupkg", nupkg(t, "Demo", "1.0.0", ""))
	if w := e.do("PUT", "nuget-hosted", "api/v2/package", body, "Content-Type", ct); w.Code != 400 || !strings.Contains(w.Body.String(), ".snupkg") {
		t.Fatalf("snupkg: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "nuget-hosted", "api/v2/symbolpackage", body, "Content-Type", ct); w.Code != 400 || !strings.Contains(w.Body.String(), ".snupkg") {
		t.Fatalf("symbolpackage: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "nuget-hosted", "api/v2/symbolpackage", nil); w.Code != 405 || w.Header().Get("Allow") != "PUT" {
		t.Fatalf("GET symbolpackage: %d", w.Code)
	}
	if w := e.do("PUT", "nuget-hosted", "api/v2/package", []byte("--x--\r\n"), "Content-Type", "multipart/form-data; boundary=x"); w.Code != 400 || !strings.Contains(w.Body.String(), "no file part") {
		t.Fatalf("no file part: %d %s", w.Code, w.Body)
	}
	if pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "nuget-hosted", "demo"); len(pkgs) != 0 {
		t.Fatalf("a refused push left rows: %+v", pkgs)
	}
	if w := e.push(t, "nuget-ro", nupkg(t, "Demo", "1.0.0", "")); w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "nuget-ro", "api/v2/package/demo/1.0.0", nil); w.Code != 400 {
		t.Fatalf("deny delete: %d %s", w.Code, w.Body)
	}
	// the push endpoint's methods: a proxy takes none
	for _, c := range []struct{ method, repoName, path, allow string }{
		{"POST", "nuget-hosted", "api/v2/package", "PUT"}, {"GET", "nuget-hosted", "api/v2/package", "PUT"},
		{"PUT", "nuget-hosted", "api/v2/package/demo/1.0.0", "DELETE"},
		{"PUT", "nuget-proxy", "api/v2/package", ""}, {"DELETE", "nuget-proxy", "api/v2/package/demo/1.0.0", ""},
		{"PUT", "nuget-proxy-first", "api/v2/package", ""},
		{"DELETE", "nuget-hosted", "v3-flatcontainer/demo/index.json", "GET, HEAD"},
	} {
		w := e.do(c.method, c.repoName, c.path, nil)
		if w.Code != 405 || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s %s: %d Allow %q, want %q", c.method, c.repoName, c.path, w.Code, w.Header().Get("Allow"), c.allow)
		}
	}
	// a raw body (curl -T) works too
	if w := e.do("PUT", "nuget-hosted", "api/v2/package", nupkg(t, "Demo", "1.0.0", ""), "Content-Type", "application/octet-stream"); w.Code != 201 {
		t.Fatalf("raw body: %d %s", w.Code, w.Body)
	}
}

// writePolicy allow: a version can be replaced, by a user with Edit.
func TestOverwriteNeedsEdit(t *testing.T) {
	e := newEnv(t)
	a, ps := repotest.Principals(t, e.h.Store.DB, "nuget-allow", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	v1 := nupkg(t, "Demo", "1.0.0", "")
	v1b := nupkg(t, "Demo", "1.0.0", nuspec("Demo", "1.0.0", "<summary>replaced</summary>"))
	push := func(p auth.Principal, pkg []byte) int {
		body, ct := form(t, "package.nupkg", pkg)
		return e.doAs(p, "PUT", "nuget-allow", "api/v2/package", body, "Content-Type", ct).Code
	}
	if c := push(ps["adder"], v1); c != 201 {
		t.Fatalf("first: %d", c)
	}
	if c := push(ps["adder"], v1b); c != 403 {
		t.Fatalf("add-only overwrite: %d", c)
	}
	if c := push(ps["editor"], v1b); c != 201 {
		t.Fatalf("edit overwrite: %d", c)
	}
	var ri regIndex
	decode(t, e.do("GET", "nuget-allow", "registration/demo/index.json", nil), &ri)
	if n := ri.Items[0].Count; n != 1 {
		t.Fatalf("versions after overwrite: %d", n)
	}
	if w := e.do("GET", "nuget-allow", "v3-flatcontainer/demo/1.0.0/demo.1.0.0.nupkg", nil); !bytes.Equal(w.Body.Bytes(), v1b) {
		t.Fatal("overwrite not stored")
	}
	if w := e.do("GET", "nuget-allow", "v3-flatcontainer/demo/1.0.0/demo.1.0.0.nuspec", nil); !strings.Contains(w.Body.String(), "replaced") {
		t.Fatal("nuspec not replaced")
	}
}

func TestSearchHosted(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ id, v string }{{"Demo.Pkg", "1.0.0"}, {"Demo.Pkg", "2.0.0-rc.1"}, {"Demo.Pkg", "1.5.0"}, {"Other", "0.1.0"}, {"PreOnly", "1.0.0-beta"}} {
		if w := e.push(t, "nuget-hosted", nupkg(t, c.id, c.v, "")); w.Code != 201 {
			t.Fatalf("push %s %s: %d %s", c.id, c.v, w.Code, w.Body)
		}
	}
	base := ccBase + "/repository/nuget-hosted"
	var s searchResult
	decode(t, e.do("GET", "nuget-hosted", "query", nil), &s) // everything, releases only
	if s.TotalHits != 2 || s.ids() != "Demo.Pkg@1.5.0,Other@0.1.0" {
		t.Fatalf("all: %d %s", s.TotalHits, s.ids())
	}
	d := s.Data[0]
	if d.ID != base+"/registration/demo.pkg/index.json" || d.Registration != d.ID || strings.Join(d.Authors, "|") != "Jane Doe|Acme" ||
		len(d.Versions) != 2 || d.Versions[1].Version != "1.5.0" || d.Versions[1].ID != base+"/registration/demo.pkg/1.5.0.json" {
		t.Fatalf("hit: %+v", d)
	}
	decode(t, e.do("GET", "nuget-hosted", "query?prerelease=true", nil), &s)
	if s.ids() != "Demo.Pkg@2.0.0-rc.1,Other@0.1.0,PreOnly@1.0.0-beta" {
		t.Fatalf("prerelease: %s", s.ids())
	}
	decode(t, e.do("GET", "nuget-hosted", "query?q=DEMO", nil), &s)
	if s.ids() != "Demo.Pkg@1.5.0" {
		t.Fatalf("q: %s", s.ids())
	}
	decode(t, e.do("GET", "nuget-hosted", "query?q=packageid:demo.pkg", nil), &s)
	if s.ids() != "Demo.Pkg@1.5.0" {
		t.Fatalf("packageid: %s", s.ids())
	}
	decode(t, e.do("GET", "nuget-hosted", "query?q=packageid:demo", nil), &s)
	if s.TotalHits != 0 || len(s.Data) != 0 {
		t.Fatalf("packageid exact: %s", s.ids())
	}
	decode(t, e.do("GET", "nuget-hosted", "query?skip=1&take=1&prerelease=true", nil), &s)
	if s.TotalHits != 3 || s.ids() != "Other@0.1.0" {
		t.Fatalf("page: %d %s", s.TotalHits, s.ids())
	}
	decode(t, e.do("GET", "nuget-hosted", "query?q=zzz", nil), &s)
	if s.TotalHits != 0 || s.Data == nil {
		t.Fatalf("no match: %s", e.do("GET", "nuget-hosted", "query?q=zzz", nil).Body)
	}
}

func TestProxy(t *testing.T) {
	e := newEnv(t)
	base := ccBase + "/repository/nuget-proxy"
	var flat struct{ Versions []string }
	decode(t, e.do("GET", "nuget-proxy", "v3-flatcontainer/Newtonsoft.Json/index.json", nil), &flat)
	if strings.Join(flat.Versions, ",") != "12.0.3,13.0.1" {
		t.Fatalf("flat: %v", flat)
	}
	if w := e.do("GET", "nuget-proxy", "v3-flatcontainer/nothere/index.json", nil); w.Code != 404 {
		t.Fatalf("unknown id: %d %s", w.Code, w.Body)
	}
	// packages are fetched once and cached
	for i := 0; i < 2; i++ {
		w := e.do("GET", "nuget-proxy", "v3-flatcontainer/newtonsoft.json/13.0.1/newtonsoft.json.13.0.1.nupkg", nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), upPkgs["newtonsoft.json.13.0.1.nupkg"]) || w.Header().Get("Content-Type") != ctNupkg {
			t.Fatalf("nupkg #%d: %d %v", i, w.Code, w.Header())
		}
	}
	if e.hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1", e.hits.Load())
	}
	if w := e.do("GET", "nuget-proxy", "v3-flatcontainer/newtonsoft.json/13.0.1/newtonsoft.json.13.0.1.nuspec", nil); w.Code != 200 || w.Header().Get("Content-Type") != ctNuspec || !strings.Contains(w.Body.String(), "<id>Newtonsoft.Json</id>") {
		t.Fatalf("nuspec: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "nuget-proxy", "v3-flatcontainer/newtonsoft.json/9.0.0/newtonsoft.json.9.0.0.nupkg", nil); w.Code != 404 {
		t.Fatalf("missing upstream file: %d", w.Code)
	}
	// the registration index: every URL of the upstream's registration and
	// flat container bases points at the proxy, the catalog's stay
	var ri regIndex
	decode(t, e.do("GET", "nuget-proxy", "registration/newtonsoft.json/index.json", nil), &ri)
	if ri.ID != base+"/registration/newtonsoft.json/index.json" || ri.Count != 2 || len(ri.Items) != 2 {
		t.Fatalf("index: %+v", ri)
	}
	inline, split := ri.Items[0], ri.Items[1]
	leaf := inline.Items[0]
	switch {
	case inline.ID != ri.ID+"#page/12.0.3/12.0.3", inline.Parent != ri.ID, len(inline.Items) != 1:
		t.Fatalf("inline page: %+v", inline)
	case leaf.ID != base+"/registration/newtonsoft.json/12.0.3.json", leaf.Registration != ri.ID:
		t.Fatalf("leaf: %+v", leaf)
	case leaf.PackageContent != base+"/v3-flatcontainer/newtonsoft.json/12.0.3/newtonsoft.json.12.0.3.nupkg", leaf.CatalogEntry.PackageContent != leaf.PackageContent:
		t.Fatalf("packageContent: %+v", leaf)
	case leaf.CatalogEntry.ID != e.up+"/catalog/data/2026/newtonsoft.json.12.0.3.json":
		t.Fatalf("catalog URL must stay: %s", leaf.CatalogEntry.ID)
	case leaf.CatalogEntry.DependencyGroups[0].Dependencies[0].Registration != base+"/registration/microsoft.csharp/index.json":
		t.Fatalf("dependency registration: %+v", leaf.CatalogEntry.DependencyGroups)
	case split.ID != base+"/registration/newtonsoft.json/page/13.0.1/13.0.1.json", len(split.Items) != 0:
		t.Fatalf("split page: %+v", split)
	}
	// the client follows the split page's @id through the proxy
	var p struct {
		ID    string    `json:"@id"`
		Items []regLeaf `json:"items"`
	}
	decode(t, e.do("GET", "nuget-proxy", "registration/newtonsoft.json/page/13.0.1/13.0.1.json", nil), &p)
	if p.ID != split.ID || len(p.Items) != 1 || p.Items[0].PackageContent != base+"/v3-flatcontainer/newtonsoft.json/13.0.1/newtonsoft.json.13.0.1.nupkg" {
		t.Fatalf("page: %+v", p)
	}
	var l regLeaf
	decode(t, e.do("GET", "nuget-proxy", "registration/newtonsoft.json/13.0.1.json", nil), &l)
	if l.ID != base+"/registration/newtonsoft.json/13.0.1.json" {
		t.Fatalf("leaf: %+v", l)
	}
	if w := e.do("GET", "nuget-proxy", "registration/nothere/index.json", nil); w.Code != 404 {
		t.Fatalf("unknown registration: %d", w.Code)
	}
	// search: forwarded with semVerLevel=2.0.0, its URLs (the older
	// registration base too) rewritten
	var s searchResult
	decode(t, e.do("GET", "nuget-proxy", "query?q=json&prerelease=true&take=5", nil), &s)
	if q := *e.queries.Load(); q != "q=json&skip=0&take=5&prerelease=true&semVerLevel=2.0.0" {
		t.Fatalf("upstream query: %s", q)
	}
	if s.TotalHits != 2 || s.ids() != "Newtonsoft.Json@13.0.1,Demo@2.0.0" || s.Data[0].ID != base+"/registration/newtonsoft.json/index.json" ||
		s.Data[0].Registration != s.Data[0].ID || s.Data[0].Versions[0].ID != base+"/registration/newtonsoft.json/13.0.1.json" {
		t.Fatalf("search: %+v", s)
	}
	// the upstream down: cached documents are served stale, the rest is a 502
	e.down.Store(true)
	e.now = e.now.Add(2 * time.Hour) // past metadataMaxAge
	w := e.do("GET", "nuget-proxy", "v3-flatcontainer/newtonsoft.json/index.json", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Warning"), "Stale") {
		t.Fatalf("stale flat index: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "nuget-proxy", "registration/newtonsoft.json/index.json", nil); w.Code != 200 || !strings.Contains(w.Header().Get("Warning"), "Stale") {
		t.Fatalf("stale registration: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "nuget-proxy", "v3-flatcontainer/newtonsoft.json/13.0.1/newtonsoft.json.13.0.1.nupkg", nil); w.Code != 200 {
		t.Fatalf("cached nupkg while down: %d", w.Code)
	}
	if w := e.do("GET", "nuget-proxy", "v3-flatcontainer/demo/index.json", nil); w.Code != 502 {
		t.Fatalf("uncached while down: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "nuget-proxy", "v3-flatcontainer/newtonsoft.json/12.0.3/newtonsoft.json.12.0.3.nupkg", nil); w.Code != 502 {
		t.Fatalf("uncached nupkg while down: %d %s", w.Code, w.Body)
	}
}

// A wrong remote (no service index) is a 502 that says so, not an empty feed.
func TestProxyWrongRemote(t *testing.T) {
	e := newEnv(t)
	cfg := config.Repository{Name: "bad-proxy", Format: "nuget", Type: "proxy", Remote: e.up + "/nothing/",
		MetadataMaxAge: config.Duration(time.Hour), NegativeCacheTTL: config.Duration(time.Minute)}
	e.repos["bad-proxy"] = repo.Build(&config.Config{Repositories: []config.Repository{cfg}})["bad-proxy"]
	for _, p := range []string{"index.json", "v3-flatcontainer/demo/index.json", "registration/demo/index.json", "query"} {
		if w := e.do("GET", "bad-proxy", p, nil); w.Code != 502 || !strings.Contains(w.Body.String(), "no NuGet service index") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	// remote given as the directory of index.json works the same
	cfg.Name, cfg.Remote = "dir-proxy", e.up+"/v3/"
	e.repos["dir-proxy"] = repo.Build(&config.Config{Repositories: []config.Repository{cfg}})["dir-proxy"]
	if w := e.do("GET", "dir-proxy", "v3-flatcontainer/demo/index.json", nil); w.Code != 200 {
		t.Fatalf("directory remote: %d %s", w.Code, w.Body)
	}
}

func TestGroup(t *testing.T) {
	e := newEnv(t)
	if w := e.push(t, "nuget-hosted", nupkg(t, "Demo", "1.0.0", "")); w.Code != 201 {
		t.Fatal(w.Code)
	}
	base := ccBase + "/repository/nuget-all"
	// the hosted member owns "demo": the proxy's 2.0.0 is not listed and not served
	var flat struct{ Versions []string }
	decode(t, e.do("GET", "nuget-all", "v3-flatcontainer/demo/index.json", nil), &flat)
	if strings.Join(flat.Versions, ",") != "1.0.0" {
		t.Fatalf("owner rule: %v", flat.Versions)
	}
	if w := e.do("GET", "nuget-all", "v3-flatcontainer/demo/2.0.0/demo.2.0.0.nupkg", nil); w.Code != 404 {
		t.Fatalf("later member's version: %d", w.Code)
	}
	if w := e.do("GET", "nuget-all", "v3-flatcontainer/demo/1.0.0/demo.1.0.0.nupkg", nil); w.Code != 200 {
		t.Fatalf("owner's file: %d", w.Code)
	}
	var ri regIndex
	decode(t, e.do("GET", "nuget-all", "registration/demo/index.json", nil), &ri)
	if ri.ID != base+"/registration/demo/index.json" || ri.Items[0].Items[0].PackageContent != base+"/v3-flatcontainer/demo/1.0.0/demo.1.0.0.nupkg" {
		t.Fatalf("group URLs on a hosted document: %+v", ri)
	}
	// a proxy-only id is served from the proxy, under the group's URLs
	decode(t, e.do("GET", "nuget-all", "v3-flatcontainer/newtonsoft.json/index.json", nil), &flat)
	if strings.Join(flat.Versions, ",") != "12.0.3,13.0.1" {
		t.Fatalf("proxy member: %v", flat.Versions)
	}
	decode(t, e.do("GET", "nuget-all", "registration/newtonsoft.json/index.json", nil), &ri)
	if ri.ID != base+"/registration/newtonsoft.json/index.json" || ri.Items[0].Items[0].PackageContent != base+"/v3-flatcontainer/newtonsoft.json/12.0.3/newtonsoft.json.12.0.3.nupkg" {
		t.Fatalf("group URLs on a proxy document: %+v", ri)
	}
	if w := e.do("GET", "nuget-all", "v3-flatcontainer/newtonsoft.json/13.0.1/newtonsoft.json.13.0.1.nupkg", nil); w.Code != 200 {
		t.Fatalf("proxy file via group: %d", w.Code)
	}
	if w := e.do("GET", "nuget-all", "v3-flatcontainer/nothere/index.json", nil); w.Code != 404 {
		t.Fatalf("in no member: %d", w.Code)
	}
	// with the proxy first, it owns demo
	decode(t, e.do("GET", "nuget-proxy-first", "v3-flatcontainer/demo/index.json", nil), &flat)
	if strings.Join(flat.Versions, ",") != "2.0.0" {
		t.Fatalf("proxy first: %v", flat.Versions)
	}
	// search merges the members, one hit per id, the first member's
	var s searchResult
	decode(t, e.do("GET", "nuget-all", "query?prerelease=true", nil), &s)
	if s.TotalHits != 2 || s.ids() != "Demo@1.0.0,Newtonsoft.Json@13.0.1" || s.Data[1].ID != base+"/registration/newtonsoft.json/index.json" {
		t.Fatalf("merged search: %d %s %+v", s.TotalHits, s.ids(), s.Data)
	}
	decode(t, e.do("GET", "nuget-all", "query?prerelease=true&skip=1&take=1", nil), &s)
	if s.TotalHits != 2 || s.ids() != "Newtonsoft.Json@13.0.1" {
		t.Fatalf("merged page: %d %s", s.TotalHits, s.ids())
	}
	// a push to the group goes to its writeMember
	if w := e.push(t, "nuget-all", nupkg(t, "Demo", "1.1.0", "")); w.Code != 201 {
		t.Fatalf("push via group: %d %s", w.Code, w.Body)
	}
	decode(t, e.do("GET", "nuget-hosted", "v3-flatcontainer/demo/index.json", nil), &flat)
	if strings.Join(flat.Versions, ",") != "1.0.0,1.1.0" {
		t.Fatalf("writeMember: %v", flat.Versions)
	}
	// a failing member ahead of the owner fails the request (an id the
	// proxy has not cached, with the upstream down)
	if w := e.push(t, "nuget-hosted", nupkg(t, "Other", "1.0.0", "")); w.Code != 201 {
		t.Fatal(w.Code)
	}
	e.down.Store(true)
	if w := e.do("GET", "nuget-proxy-first", "v3-flatcontainer/other/index.json", nil); w.Code != 502 {
		t.Fatalf("failing member: %d %s", w.Code, w.Body)
	}
}

func TestDeleteComponent(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if w := e.push(t, "nuget-hosted", nupkg(t, "Demo", v, "")); w.Code != 201 {
			t.Fatal(w.Code)
		}
	}
	e.sink.Reset()
	ctx := context.Background()
	rp := e.repos["nuget-hosted"]
	res, err := e.h.DeleteComponent(ctx, rp, component.Component{Repo: "nuget-hosted", Format: "nuget", Name: "demo", Version: "1.0.0"}, "admin")
	if err != nil || res.Assets != 2 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Kind != "delete" || got[0].Actor != "admin" || got[0].Subject != "demo@1.0.0" || got[0].Detail != "assets=2" {
		t.Fatalf("events: %v", got)
	}
	for _, p := range []string{"v3-flatcontainer/demo/1.0.0/demo.1.0.0.nupkg", "v3-flatcontainer/demo/1.0.0/demo.1.0.0.nuspec", "registration/demo/1.0.0.json"} {
		if w := e.do("GET", "nuget-hosted", p, nil); w.Code != 404 {
			t.Fatalf("deleted %s: %d", p, w.Code)
		}
	}
	var flat struct{ Versions []string }
	decode(t, e.do("GET", "nuget-hosted", "v3-flatcontainer/demo/index.json", nil), &flat)
	if strings.Join(flat.Versions, ",") != "1.1.0" {
		t.Fatalf("after delete: %v", flat.Versions)
	}
	if as, _ := e.h.Store.DB.ListAssets(ctx, "nuget-hosted", "demo/1.0.0/"); len(as) != 0 {
		t.Fatalf("assets left: %+v", as)
	}
	// gone already: no error, no event
	if res, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "nuget", Name: "demo", Version: "1.0.0"}, "admin"); err != nil || res.Assets != 0 || len(e.sink.Events()) != 1 {
		t.Fatalf("again: %+v %v %v", res, err, e.sink.Events())
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["nuget-proxy"], component.Component{Format: "nuget", Name: "demo", Version: "1.0.0"}, "admin"); err == nil {
		t.Fatal("proxy: want ErrNotSupported")
	}
	if _, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "nuget", Name: "demo"}, "admin"); err == nil {
		t.Fatal("incomplete component: want an error")
	}
	// the push API's DELETE (dotnet nuget delete): the version, through the
	// group's writeMember too; 404 when there is none
	if w := e.do("DELETE", "nuget-all", "api/v2/package/Demo/1.1.0.0", nil); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "nuget-hosted", "api/v2/package/demo/1.1.0", nil); w.Code != 404 {
		t.Fatalf("DELETE again: %d", w.Code)
	}
	if w := e.do("GET", "nuget-hosted", "v3-flatcontainer/demo/index.json", nil); w.Code != 404 {
		t.Fatalf("no versions left: %d", w.Code)
	}
	if got := e.sink.Events(); len(got) != 2 || got[1].Subject != "demo@1.1.0" || got[1].Repo != "nuget-hosted" {
		t.Fatalf("events: %v", got)
	}
	// the component view's paths round-trip
	if _, name, version, path := component.PackageComponent("nuget", "demo", "1.1.0"); name != "demo" || version != "1.1.0" || path != "demo/1.1.0/demo.1.1.0.nupkg" {
		t.Fatalf("PackageComponent: %s %s %s", name, version, path)
	}
	for p, want := range map[string]string{
		"demo/1.1.0/demo.1.1.0.nupkg": "demo@1.1.0", "demo/1.1.0/demo.1.1.0.nuspec": "demo@1.1.0",
		"v3-flatcontainer/newtonsoft.json/13.0.1/newtonsoft.json.13.0.1.nupkg": "newtonsoft.json@13.0.1",
		"v3-flatcontainer/newtonsoft.json/index.json":                          "", "upstream/index.json": "", "query/abc.json": "", "demo/1.1.0/other.1.1.0.nupkg": "",
	} {
		_, name, version, ok := component.DeriveAsset("nuget", p)
		got := ""
		if ok {
			got = name + "@" + version
		}
		if got != want {
			t.Errorf("DeriveAsset(%s) = %q, want %q", p, got, want)
		}
	}
}

func TestParseNuspec(t *testing.T) {
	a, err := parseNuspec([]byte(`<?xml version="1.0"?>
<package xmlns="http://schemas.microsoft.com/packaging/2011/08/nuspec.xsd"><metadata>
<id>Legacy</id><version>2.0</version><authors>me</authors><description>d</description>
<requireLicenseAcceptance>True</requireLicenseAcceptance><license type="file">LICENSE.txt</license>
<dependencies><dependency id="A" version="1.0" /><dependency id="B" /></dependencies>
</metadata></package>`))
	if err != nil {
		t.Fatal(err)
	}
	deps := fmt.Sprint(a.DependencyGroups)
	if a.ID != "Legacy" || a.Version != "2.0.0" || !a.RequireLicenseAcceptance || a.License != "LICENSE.txt" || len(a.Tags) != 0 ||
		deps != "[{ [{A [1.0.0, )} {B (, )}]}]" {
		t.Fatalf("%+v %s", a, deps)
	}
}
