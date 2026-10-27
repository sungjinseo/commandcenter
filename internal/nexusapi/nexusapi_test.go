package nexusapi_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

const v1 = "/service/rest/v1"

type env struct {
	t  *testing.T
	ts *httptest.Server
}

// start runs the whole app in process: admin "admin"/"pw", deployer
// "ci"/"pw" (every action on maven-releases and raw-files) and the given
// anonymousRead.
func start(t *testing.T, anonymousRead bool) *env {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
dataDir: %s
baseURL: http://nexus.example.com
anonymousRead: %v
users:
  - { name: admin, passwordHash: %q, roles: [admin] }
  - { name: ci, passwordHash: %q, roles: [deployer] }
roles:
  deployer: { write: [maven-releases, raw-files] }
repositories:
  - { name: maven-releases, format: maven, type: hosted, writePolicy: allow_once }
  - { name: maven-central, format: maven, type: proxy, remote: https://repo1.maven.org/maven2/, contentMaxAge: -1 }
  - { name: maven-public, format: maven, type: group, members: [maven-releases, maven-central] }
  - { name: raw-files, format: raw, type: hosted }
`, t.TempDir(), anonymousRead, string(h), string(h))))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Close() })
	return &env{t, ts}
}

// do sends method path as user ("" anonymous) with body and returns the
// status and body.
func (e *env) do(method, path, user, body string) (int, []byte, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, "pw")
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func (e *env) get(path, user string, into any) {
	e.t.Helper()
	st, b, _ := e.do("GET", path, user, "")
	if st != 200 {
		e.t.Fatalf("GET %s as %q: %d %s", path, user, st, b)
	}
	if err := json.Unmarshal(b, into); err != nil {
		e.t.Fatalf("GET %s: %v: %s", path, err, b)
	}
}

func (e *env) want(method, path, user, body string, status int) []byte {
	e.t.Helper()
	st, b, _ := e.do(method, path, user, body)
	if st != status {
		e.t.Fatalf("%s %s as %q: got %d want %d: %s", method, path, user, st, status, b)
	}
	return b
}

// upload puts n maven jars org/x/a<i>/1.0 into maven-releases.
func (e *env) upload(paths ...string) {
	e.t.Helper()
	for _, p := range paths {
		body := "bytes-of-" + p
		if strings.HasSuffix(p, ".pom") { // a hosted pom must declare the path's coordinates
			segs := strings.Split(strings.TrimPrefix(p, "maven-releases/"), "/")
			n := len(segs)
			body = fmt.Sprintf("<project><modelVersion>4.0.0</modelVersion><groupId>%s</groupId><artifactId>%s</artifactId><version>%s</version></project>",
				strings.Join(segs[:n-3], "."), segs[n-3], segs[n-2])
		}
		e.want("PUT", "/repository/"+p, "ci", body, 201)
	}
}

type repoView struct {
	Name, Format, Type, URL string
	Attributes              map[string]map[string]string
}

type comp struct {
	ID, Repository, Format string
	Group, Version         *string
	Name                   string
	Assets                 []asset
}

type asset struct {
	ID, Path, DownloadURL, Repository, Format, ContentType string
	Checksum                                               map[string]string
	FileSize                                               int64
}

type page[T any] struct {
	Items             []T
	ContinuationToken *string
}

func TestRepositoriesListAndSettings(t *testing.T) {
	e := start(t, true)
	var list []repoView
	e.get(v1+"/repositories", "", &list)
	if len(list) != 4 || list[0].Name != "maven-central" || list[0].Format != "maven2" || list[0].Type != "proxy" ||
		list[0].URL != "http://nexus.example.com/repository/maven-central" ||
		list[0].Attributes["proxy"]["remoteUrl"] != "https://repo1.maven.org/maven2/" {
		t.Fatalf("repositories = %+v", list)
	}
	if list[1].Format != "maven2" || len(list[1].Attributes) != 0 || list[3].Format != "raw" {
		t.Fatalf("repositories = %+v", list)
	}
	var one repoView
	e.get(v1+"/repositories/maven-public", "", &one)
	if one.Type != "group" || one.Format != "maven2" {
		t.Fatalf("repository = %+v", one)
	}

	// Settings are admin only, keyed by the Nexus format path.
	e.want("GET", v1+"/repositorySettings", "ci", "", 403)
	e.want("GET", v1+"/repositories/maven/proxy/maven-central", "", "", 401)
	var s map[string]any
	e.get(v1+"/repositories/maven/proxy/maven-central", "admin", &s)
	proxy := s["proxy"].(map[string]any)
	if s["format"] != "maven2" || s["online"] != true || proxy["remoteUrl"] != "https://repo1.maven.org/maven2/" ||
		proxy["contentMaxAge"] != float64(-1) || proxy["metadataMaxAge"] != float64(1440) ||
		s["negativeCache"].(map[string]any)["timeToLive"] != float64(10) ||
		s["storage"].(map[string]any)["blobStoreName"] != "default" || s["maven"].(map[string]any)["versionPolicy"] != "MIXED" {
		t.Fatalf("proxy settings = %v", s)
	}
	e.get(v1+"/repositories/maven/hosted/maven-releases", "admin", &s)
	if s["storage"].(map[string]any)["writePolicy"] != "ALLOW_ONCE" || s["cleanup"].(map[string]any)["policyNames"] == nil {
		t.Fatalf("hosted settings = %v", s)
	}
	e.get(v1+"/repositories/maven/group/maven-public", "admin", &s)
	if members := s["group"].(map[string]any)["memberNames"].([]any); len(members) != 2 || members[0] != "maven-releases" {
		t.Fatalf("group settings = %v", s)
	}
	e.want("GET", v1+"/repositories/npm/hosted/maven-releases", "admin", "", 404) // wrong format path
	var all []map[string]any
	e.get(v1+"/repositorySettings", "admin", &all)
	if len(all) != 4 {
		t.Fatalf("repositorySettings = %d", len(all))
	}
}

// The bodies are the examples of Nexus's own API documentation and what the
// Terraform provider (datadrivers/terraform-provider-nexus) and nexus-cli
// send, fields Command Center has no equivalent for included.
func TestRepositoriesCreateUpdateDelete(t *testing.T) {
	e := start(t, true)
	const hosted = `{"name":"maven-snapshots","online":true,
	  "storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"},
	  "cleanup":{"policyNames":[]},
	  "component":{"proprietaryComponents":true},
	  "maven":{"versionPolicy":"SNAPSHOT","layoutPolicy":"STRICT","contentDisposition":"INLINE"}}`
	const proxy = `{"name":"npm-proxy","online":true,
	  "storage":{"blobStoreName":"default","strictContentTypeValidation":true},
	  "cleanup":{"policyNames":["weekly"]},
	  "proxy":{"remoteUrl":"https://registry.npmjs.org","contentMaxAge":1440,"metadataMaxAge":60},
	  "negativeCache":{"enabled":true,"timeToLive":5},
	  "httpClient":{"blocked":false,"autoBlock":true,
	    "connection":{"retries":0,"userAgentSuffix":"string","timeout":60,"enableCircularRedirects":false,"enableCookies":false,"useTrustStore":false},
	    "authentication":{"type":"username","username":"bob","password":"secret"}},
	  "routingRule":"string","npm":{"removeNonCataloged":false,"removeQuarantined":false}}`
	const group = `{"name":"maven-all","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},
	  "group":{"memberNames":["maven-releases","maven-snapshots","maven-central"],"writableMember":"maven-snapshots"}}`

	e.want("POST", v1+"/repositories/maven/hosted", "ci", hosted, 403)
	e.want("POST", v1+"/repositories/maven/hosted", "", hosted, 401)
	// The cleanup policy does not exist: Nexus's validation array.
	b := e.want("POST", v1+"/repositories/npm/proxy", "admin", proxy, 400)
	var verrs []struct{ ID, Message string }
	if err := json.Unmarshal(b, &verrs); err != nil || len(verrs) == 0 || verrs[0].Message == "" {
		t.Fatalf("validation body = %s", b)
	}
	e.want("POST", v1+"/repositories/maven/hosted", "admin", hosted, 201)
	e.want("POST", v1+"/repositories/npm/proxy", "admin", strings.Replace(proxy, `["weekly"]`, `[]`, 1), 201)
	e.want("POST", v1+"/repositories/maven/group", "admin", group, 201)
	b = e.want("POST", v1+"/repositories/maven/hosted", "admin", hosted, 400) // exists: Nexus answers 400 too
	if !strings.Contains(string(b), "duplicate") {
		t.Fatalf("duplicate body = %s", b)
	}
	e.want("POST", v1+"/repositories/conda/hosted", "admin", `{"name":"conda-hosted","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"}}`, 400)

	var s map[string]any
	e.get(v1+"/repositories/npm/proxy/npm-proxy", "admin", &s)
	proxyS, nc, hc := s["proxy"].(map[string]any), s["negativeCache"].(map[string]any), s["httpClient"].(map[string]any)
	if proxyS["remoteUrl"] != "https://registry.npmjs.org/" || proxyS["contentMaxAge"] != float64(1440) || proxyS["metadataMaxAge"] != float64(60) ||
		nc["timeToLive"] != float64(5) || hc["authentication"].(map[string]any)["username"] != "bob" || s["format"] != "npm" {
		t.Fatalf("npm proxy settings = %v", s)
	}
	if _, leaked := hc["authentication"].(map[string]any)["password"]; leaked {
		t.Fatal("password in settings")
	}
	e.get(v1+"/repositories/maven/hosted/maven-snapshots", "admin", &s)
	if s["maven"].(map[string]any)["versionPolicy"] != "SNAPSHOT" || s["storage"].(map[string]any)["writePolicy"] != "ALLOW" {
		t.Fatalf("snapshots settings = %v", s)
	}
	e.get(v1+"/repositories/maven/group/maven-all", "admin", &s)
	if s["group"].(map[string]any)["writableMember"] != "maven-snapshots" {
		t.Fatalf("group settings = %v", s)
	}

	// Update: the Terraform provider PUTs the whole object back.
	upd := strings.Replace(hosted, `"writePolicy":"ALLOW"`, `"writePolicy":"DENY"`, 1)
	upd = strings.Replace(upd, `"online":true`, `"online":false`, 1)
	e.want("PUT", v1+"/repositories/maven/hosted/maven-snapshots", "ci", upd, 403)
	e.want("PUT", v1+"/repositories/maven/hosted/maven-snapshots", "admin", upd, 204)
	e.want("PUT", v1+"/repositories/maven/hosted/nope", "admin", upd, 404)
	e.get(v1+"/repositories/maven/hosted/maven-snapshots", "admin", &s)
	if s["storage"].(map[string]any)["writePolicy"] != "DENY" || s["online"] != false {
		t.Fatalf("updated settings = %v", s)
	}
	// The proxy's stored password survives a PUT without one (the provider
	// sends the username only when the password is not in state).
	e.want("PUT", v1+"/repositories/npm/proxy/npm-proxy", "admin",
		strings.Replace(strings.Replace(proxy, `["weekly"]`, `[]`, 1), `,"password":"secret"`, ``, 1), 204)

	e.want("DELETE", v1+"/repositories/maven-all", "ci", "", 403)
	e.want("DELETE", v1+"/repositories/maven-all", "admin", "", 204)
	e.want("DELETE", v1+"/repositories/maven-all", "admin", "", 404)
	var list []repoView
	e.get(v1+"/repositories", "admin", &list)
	for _, r := range list {
		if r.Name == "maven-all" {
			t.Fatal("deleted repository listed")
		}
	}
}

// Nexus formats that the Terraform provider and nexus-cli create, with the
// format sections those tools send.
func TestCreateOtherFormats(t *testing.T) {
	e := start(t, true)
	for _, c := range []struct{ path, body string }{
		{"docker/hosted", `{"name":"docker-hosted","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"},
		  "docker":{"v1Enabled":false,"forceBasicAuth":true,"httpPort":null,"httpsPort":null,"subdomain":null}}`},
		{"pypi/hosted", `{"name":"pypi-hosted","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW_ONCE"},"cleanup":null,"component":{"proprietaryComponents":false}}`},
		{"raw/hosted", `{"name":"raw-hosted","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"ALLOW"},"raw":{"contentDisposition":"ATTACHMENT"}}`},
		{"helm/proxy", `{"name":"helm-proxy","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},
		  "proxy":{"remoteUrl":"https://charts.bitnami.com/bitnami","contentMaxAge":1440,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1440},
		  "httpClient":{"blocked":false,"autoBlock":true}}`},
		{"go/proxy", `{"name":"go-proxy","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},
		  "proxy":{"remoteUrl":"https://proxy.golang.org","contentMaxAge":-1,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1440},"httpClient":{"blocked":false,"autoBlock":true}}`},
		{"cargo/hosted", `{"name":"cargo-hosted","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"}}`},
	} {
		e.want("POST", v1+"/repositories/"+c.path, "admin", c.body, 201)
	}
	var list []repoView
	e.get(v1+"/repositories", "admin", &list)
	got := map[string]string{}
	for _, r := range list {
		got[r.Name] = r.Format + "/" + r.Type
	}
	for name, want := range map[string]string{"docker-hosted": "docker/hosted", "pypi-hosted": "pypi/hosted", "raw-hosted": "raw/hosted",
		"helm-proxy": "helm/proxy", "go-proxy": "go/proxy", "cargo-hosted": "cargo/hosted"} {
		if got[name] != want {
			t.Fatalf("%s = %q, want %q (%v)", name, got[name], want, got)
		}
	}
}

func TestComponentsPagingGetDelete(t *testing.T) {
	e := start(t, true)
	var paths []string
	for i := 0; i < 120; i++ {
		paths = append(paths, fmt.Sprintf("maven-releases/org/x/a%03d/1.0/a%03d-1.0.jar", i, i))
		if i < 3 { // a second file of the same component
			paths = append(paths, fmt.Sprintf("maven-releases/org/x/a%03d/1.0/a%03d-1.0.pom", i, i))
		}
	}
	e.upload(paths...)
	e.want("GET", v1+"/components", "", "", 400) // repository is required
	e.want("GET", v1+"/components?repository=nope", "ci", "", 404)

	var all []comp
	token := ""
	for pages := 0; ; pages++ {
		var p page[comp]
		e.get(v1+"/components?repository=maven-releases&continuationToken="+token, "", &p)
		all = append(all, p.Items...)
		if p.ContinuationToken == nil {
			if pages != 2 || len(p.Items) != 20 {
				t.Fatalf("page %d has %d items", pages, len(p.Items))
			}
			break
		}
		if len(p.Items) != 50 {
			t.Fatalf("page %d has %d items", pages, len(p.Items))
		}
		token = *p.ContinuationToken
	}
	if len(all) != 120 {
		t.Fatalf("%d components", len(all))
	}
	seen := map[string]bool{}
	for i, c := range all {
		if seen[c.ID] || c.Format != "maven2" || c.Repository != "maven-releases" || *c.Group != "org.x" ||
			c.Name != fmt.Sprintf("a%03d", i) || *c.Version != "1.0" {
			t.Fatalf("component %d = %+v", i, c)
		}
		seen[c.ID] = true
	}
	if id, _ := base64.RawURLEncoding.DecodeString(all[0].ID); string(id) != "maven-releases:org.x:a000:1.0" {
		t.Fatalf("id = %s", id)
	}
	if a := all[0].Assets; len(a) != 2 || a[0].Path != "org/x/a000/1.0/a000-1.0.jar" || a[1].Path != "org/x/a000/1.0/a000-1.0.pom" ||
		a[0].DownloadURL != "http://nexus.example.com/repository/maven-releases/org/x/a000/1.0/a000-1.0.jar" ||
		a[0].Format != "maven2" || a[0].FileSize != int64(len("bytes-of-"+paths[0])) || len(a[0].Checksum["sha1"]) != 40 || len(a[0].Checksum["sha256"]) != 64 || len(a[0].Checksum["md5"]) != 32 {
		t.Fatalf("assets = %+v", a)
	}
	e.want("GET", v1+"/components?repository=maven-releases&continuationToken=bogus", "", "", 400)

	var one comp
	e.get(v1+"/components/"+all[5].ID, "", &one)
	if one.Name != "a005" || len(one.Assets) != 1 {
		t.Fatalf("component = %+v", one)
	}
	e.want("GET", v1+"/components/"+base64.RawURLEncoding.EncodeToString([]byte("maven-releases:org.x:zzz:9")), "", "", 404)
	e.want("GET", v1+"/components/AAAA", "", "", 404) // no ":" inside

	e.want("DELETE", v1+"/components/"+all[5].ID, "", "", 401)
	e.want("DELETE", v1+"/components/"+all[0].ID, "ci", "", 204)
	e.want("DELETE", v1+"/components/"+all[0].ID, "ci", "", 404)
	e.want("GET", "/repository/maven-releases/org/x/a000/1.0/a000-1.0.jar", "", "", 404)
	e.want("GET", "/repository/maven-releases/org/x/a000/1.0/a000-1.0.pom", "", "", 404)
	var p page[comp]
	e.get(v1+"/components?repository=maven-releases", "", &p)
	if p.Items[0].Name != "a001" {
		t.Fatalf("first after delete = %+v", p.Items[0])
	}
}

func TestAssets(t *testing.T) {
	e := start(t, true)
	e.upload("maven-releases/org/x/a/1.0/a-1.0.jar", "raw-files/docs/readme.txt", "raw-files/top.bin")
	var p page[asset]
	e.get(v1+"/assets?repository=maven-releases", "", &p)
	if len(p.Items) < 1 || p.Items[0].Path != "org/x/a/1.0/a-1.0.jar" || p.ContinuationToken != nil {
		t.Fatalf("assets = %+v", p)
	}
	var a asset
	e.get(v1+"/assets/"+p.Items[0].ID, "", &a)
	if a.Path != p.Items[0].Path || a.Repository != "maven-releases" || a.DownloadURL == "" {
		t.Fatalf("asset = %+v", a)
	}
	e.want("DELETE", v1+"/assets/"+a.ID, "ci", "", 400) // not raw: whole components only

	e.get(v1+"/assets?repository=raw-files", "", &p)
	if len(p.Items) != 2 || p.Items[0].Path != "docs/readme.txt" {
		t.Fatalf("raw assets = %+v", p)
	}
	var comps page[comp]
	e.get(v1+"/components?repository=raw-files", "", &comps)
	if c := comps.Items[0]; c.Name != "docs/readme.txt" || *c.Group != "/docs" || c.Version != nil {
		t.Fatalf("raw component = %+v", c)
	}
	if c := comps.Items[1]; *c.Group != "/" {
		t.Fatalf("raw component = %+v", c)
	}
	e.want("DELETE", v1+"/assets/"+p.Items[0].ID, "", "", 401)
	e.want("DELETE", v1+"/assets/"+p.Items[0].ID, "ci", "", 204)
	e.want("GET", v1+"/assets/"+p.Items[0].ID, "ci", "", 404)
	e.want("GET", "/repository/raw-files/docs/readme.txt", "", "", 404)
}

func TestSearch(t *testing.T) {
	e := start(t, true)
	e.upload("maven-releases/org/x/lib/1.9/lib-1.9.jar", "maven-releases/org/x/lib/1.10/lib-1.10.jar", "maven-releases/org/x/lib/1.10/lib-1.10-sources.jar",
		"maven-releases/org/x/lib/1.10/lib-1.10.pom", "maven-releases/com/y/tool/2.0/tool-2.0.jar", "raw-files/notes/lib.txt")
	var p page[comp]
	e.get(v1+"/search?q=lib", "", &p)
	if len(p.Items) != 3 { // lib 1.9, lib 1.10, notes/lib.txt
		t.Fatalf("q=lib: %+v", p.Items)
	}
	e.get(v1+"/search?q=lib&format=maven2", "", &p)
	if len(p.Items) != 2 || p.Items[0].Format != "maven2" {
		t.Fatalf("format=maven2: %+v", p.Items)
	}
	e.get(v1+"/search?q=lib&format=maven", "", &p) // Command Center spelling accepted too
	if len(p.Items) != 2 {
		t.Fatalf("format=maven: %+v", p.Items)
	}
	e.get(v1+"/search?maven.groupId=org.x&maven.artifactId=lib&maven.baseVersion=1.1*", "", &p)
	if len(p.Items) != 1 || *p.Items[0].Version != "1.10" {
		t.Fatalf("maven.*: %+v", p.Items)
	}
	e.get(v1+"/search?repository=maven-releases&name=lib&sort=version&direction=desc", "", &p)
	if len(p.Items) != 2 || *p.Items[0].Version != "1.10" || *p.Items[1].Version != "1.9" {
		t.Fatalf("sort=version desc: %+v", p.Items)
	}
	e.get(v1+"/search?repository=maven-releases&name=lib&sort=version&direction=asc", "", &p)
	if *p.Items[0].Version != "1.9" {
		t.Fatalf("sort=version asc: %+v", p.Items)
	}
	e.get(v1+"/search?group=com.y", "", &p)
	if len(p.Items) != 1 || p.Items[0].Name != "tool" {
		t.Fatalf("group: %+v", p.Items)
	}
	e.want("GET", v1+"/search?repository=nope", "ci", "", 404)

	var assets page[asset]
	e.get(v1+"/search/assets?name=lib&version=1.10&maven.extension=jar", "", &assets)
	if len(assets.Items) != 2 {
		t.Fatalf("search/assets ext: %+v", assets.Items)
	}
	e.get(v1+"/search/assets?name=lib&version=1.10&maven.extension=jar&maven.classifier=sources", "", &assets)
	if len(assets.Items) != 1 || assets.Items[0].Path != "org/x/lib/1.10/lib-1.10-sources.jar" {
		t.Fatalf("search/assets classifier: %+v", assets.Items)
	}

	st, b, _ := e.do("GET", v1+"/search/assets/download?name=lib&maven.extension=jar&maven.classifier=", "", "")
	if st != 400 || !strings.Contains(string(b), "multiple assets") {
		t.Fatalf("ambiguous download: %d %s", st, b)
	}
	st, _, hd := e.do("GET", v1+"/search/assets/download?name=lib&maven.extension=jar&maven.classifier=sources", "", "")
	if st != 302 || hd.Get("Location") != "http://nexus.example.com/repository/maven-releases/org/x/lib/1.10/lib-1.10-sources.jar" {
		t.Fatalf("download: %d %s", st, hd.Get("Location"))
	}
	st, _, hd = e.do("GET", v1+"/search/assets/download?repository=maven-releases&group=org.x&name=lib&maven.extension=pom&sort=version&direction=desc", "", "")
	if st != 302 || !strings.HasSuffix(hd.Get("Location"), "/lib-1.10.pom") {
		t.Fatalf("download newest: %d %s", st, hd.Get("Location"))
	}
	e.want("GET", v1+"/search/assets/download?name=nothing", "", "", 404)
}

func TestStatus(t *testing.T) {
	e := start(t, true)
	e.want("GET", v1+"/status", "", "", 200)
	e.want("GET", v1+"/status/writable", "", "", 200)
	e.want("GET", v1+"/status/check", "", "", 401)
	e.want("GET", v1+"/status/check", "ci", "", 403)
	var checks map[string]struct {
		Healthy   bool
		Message   string
		Error     string
		Details   map[string]any
		Time      int64
		Duration  int64
		Timestamp string
	}
	e.get(v1+"/status/check", "admin", &checks)
	db, ok := checks["Database"]
	if !ok || !db.Healthy || db.Message == "" || db.Timestamp == "" || db.Time == 0 || db.Details == nil {
		t.Fatalf("status/check = %+v", checks)
	}
	if _, ok := checks["Repositories"]; !ok {
		t.Fatalf("status/check = %+v", checks)
	}
}

func TestAnonymousReadOff(t *testing.T) {
	e := start(t, false)
	e.upload("maven-releases/org/x/a/1.0/a-1.0.jar")
	for _, p := range []string{"/repositories", "/components?repository=maven-releases", "/assets?repository=maven-releases", "/search?q=a",
		"/components/" + base64.RawURLEncoding.EncodeToString([]byte("maven-releases:org.x:a:1.0"))} {
		st, _, hd := e.do("GET", v1+p, "", "")
		if st != 401 || !strings.HasPrefix(hd.Get("WWW-Authenticate"), "Basic") {
			t.Fatalf("anonymous GET %s: %d %q", p, st, hd.Get("WWW-Authenticate"))
		}
		e.want("GET", v1+p, "ci", "", 200)
	}
	e.want("GET", v1+"/status", "", "", 200)
}

func TestSecurityBlobstoresTasksAndNotFound(t *testing.T) {
	e := start(t, true)
	for _, p := range []string{"/security/users", "/security/roles", "/blobstores", "/tasks"} {
		e.want("GET", v1+p, "", "", 401)
		e.want("GET", v1+p, "ci", "", 403)
	}
	var users []struct {
		UserID, Source, Status string
		Roles                  []string
		ReadOnly               bool
	}
	e.get(v1+"/security/users?userId=ci", "admin", &users)
	if len(users) != 1 || users[0].UserID != "ci" || users[0].Source != "default" || users[0].Status != "active" || users[0].Roles[0] != "deployer" {
		t.Fatalf("users = %+v", users)
	}
	e.get(v1+"/security/users", "admin", &users)
	if len(users) != 2 {
		t.Fatalf("users = %+v", users)
	}
	var roles []struct {
		ID, Name   string
		ReadOnly   bool
		Privileges []string
	}
	e.get(v1+"/security/roles", "admin", &roles)
	if len(roles) != 2 || roles[0].ID != "admin" || !roles[0].ReadOnly || roles[1].ID != "deployer" || roles[1].ReadOnly ||
		len(roles[1].Privileges) == 0 || !strings.HasPrefix(roles[1].Privileges[0], "repo:maven-releases:") {
		t.Fatalf("roles = %+v", roles)
	}
	var stores []struct {
		Name, Type       string
		BlobCount        int64
		TotalSizeInBytes int64
		Unavailable      bool
	}
	e.get(v1+"/blobstores", "admin", &stores)
	if len(stores) != 1 || stores[0].Name != "default" || stores[0].Type != "File" || stores[0].Unavailable {
		t.Fatalf("blobstores = %+v", stores)
	}
	var tasks page[struct {
		ID, Name, Type, CurrentState string
		LastRunResult, NextRun       *string
	}]
	e.get(v1+"/tasks", "admin", &tasks)
	if len(tasks.Items) == 0 || tasks.Items[0].ID == "" || tasks.Items[0].CurrentState == "" {
		t.Fatalf("tasks = %+v", tasks)
	}
	st, b, _ := e.do("GET", v1+"/nope", "", "")
	if st != 404 || !strings.Contains(string(b), `"message"`) {
		t.Fatalf("unknown: %d %s", st, b)
	}
}
