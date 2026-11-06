package nexusapi_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
)

const goModule = "/go/github.com/!burnt!sushi/toml/@v/"

// startGo runs the app with go-hosted and go-px, a proxy of an upstream that
// has github.com/BurntSushi/toml v1.4.0 and v1.5.0-RC1.
func startGo(t *testing.T) (*env, *upstream) {
	t.Helper()
	up := newUpstream(t, http.StatusNotFound, map[string]string{
		goModule + "list":              "v1.4.0\nv1.5.0-RC1\n",
		goModule + "v1.4.0.info":       `{"Version":"v1.4.0","Time":"2024-01-01T00:00:00Z"}`,
		goModule + "v1.4.0.mod":        "module github.com/BurntSushi/toml\n",
		goModule + "v1.4.0.zip":        "zip",
		goModule + "v1.5.0-!r!c1.info": `{"Version":"v1.5.0-RC1","Time":"2024-01-01T00:00:00Z"}`,
		"/go/rsc.io/quote/@v/list":     "v1.5.2\n",
	})
	return startRepos(t, fmt.Sprintf(`
  - { name: go-hosted, format: go, type: hosted }
  - { name: go-px, format: go, type: proxy, remote: %s/go/ }
`, up.URL)), up
}

// golang-02: a routing rule does not decide about a path that never goes to
// the remote: the go command takes anything but a 404 for sumdb/.../supported
// as the checksum database being down.
func TestGoRoutingRuleLeavesSumdbAlone(t *testing.T) {
	e, _ := startGo(t)
	body := func(rule string) string {
		return fmt.Sprintf(`{"name":"go-px","online":true,"storage":{"blobStoreName":"default"},"routingRule":%q,
		  "proxy":{"remoteUrl":"%s","contentMaxAge":-1,"metadataMaxAge":1440},"httpClient":{"blocked":false,"autoBlock":true},"negativeCache":{"enabled":true,"timeToLive":10}}`, rule, remoteOf(t, e, "go-px"))
	}
	e.want("POST", v1+"/routing-rules", "admin", `{"name":"allow-github","mode":"ALLOW","matchers":["^/github\\.com/.*"]}`, 204)
	e.want("POST", v1+"/routing-rules", "admin", `{"name":"block-sumdb","mode":"BLOCK","matchers":["^/sumdb/.*","^/rsc\\.io/.*"]}`, 204)
	const repo = "/repository/go-px/"
	for _, rule := range []string{"allow-github", "block-sumdb"} {
		e.want("PUT", v1+"/repositories/go/proxy/go-px", "admin", body(rule), 204)
		for _, p := range []string{"sumdb/sum.golang.org/supported", "sumdb/sum.golang.org/latest", "sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2", "sumdb/sum.golang.org/tile/8/0/000"} {
			e.want("GET", repo+p, "", "", 404)
		}
		e.want("GET", repo+"rsc.io/quote/@v/list", "", "", 403) // a GOPROXY path: the rule's to decide
		e.want("GET", repo+"github.com/!burnt!sushi/toml/@v/v1.4.0.mod", "", "", 200)
	}
}

// golang-03, kept: a group whose proxy is down answers 502 for what no other
// member has (Nexus: 404), so an outage does not read as "no such module";
// what a member has is served, and a list holds what the others know.
func TestGoGroupWithAProxyDown(t *testing.T) {
	e := startRepos(t, `
  - { name: go-hosted, format: go, type: hosted }
  - { name: go-down, format: go, type: proxy, remote: "http://127.0.0.1:9/" }
  - { name: go-all, format: go, type: group, members: [go-hosted, go-down] }
`)
	if s, b, _ := e.send("PUT", "/repository/go-hosted/example.com/m/@v/v1.0.0.zip", "admin", "application/zip", goZip(t, "example.com/m@v1.0.0")); s != 201 {
		t.Fatalf("upload: %d %s", s, b)
	}
	const repo = "/repository/go-all/"
	for _, p := range []string{"example.com/none/@v/list", "example.com/none/@v/v1.0.0.info", "example.com/none/@v/v1.0.0.mod", "example.com/none/@latest"} {
		e.want("GET", repo+p, "", "", 502)
	}
	e.want("GET", repo+"example.com/m/@v/v1.0.0.mod", "", "", 200)
	if b := e.want("GET", repo+"example.com/m/@v/list", "", "", 200); string(b) != "v1.0.0\n" {
		t.Fatalf("list: %q", b)
	}
}

// remoteOf is a proxy's remote URL as the Nexus API shows it.
func remoteOf(t *testing.T, e *env, name string) string {
	t.Helper()
	var s struct {
		Proxy struct {
			RemoteURL string `json:"remoteUrl"`
		} `json:"proxy"`
	}
	e.get(v1+"/repositories/go/proxy/"+name, "admin", &s)
	return s.Proxy.RemoteURL
}

// golang-01: a Go component is found by its real name and by the escaped one
// a Nexus search names it by; the JSON keeps showing the real one.
func TestGoSearchByEscapedName(t *testing.T) {
	e, _ := startGo(t)
	e.want("GET", "/repository/go-px/github.com/!burnt!sushi/toml/@v/v1.4.0.mod", "", "", 200)
	e.want("GET", "/repository/go-px/github.com/!burnt!sushi/toml/@v/v1.5.0-!r!c1.info", "", "", 200)
	search := func(q string) string { return v1 + "/search?repository=go-px&sort=version&direction=asc&" + q }
	both := []string{"github.com/BurntSushi/toml", "github.com/BurntSushi/toml"}
	for _, n := range []string{"github.com/BurntSushi/toml", "github.com/!burnt!sushi/toml", "github.com/!burnt!sushi/*", "*/!burnt!sushi/toml"} {
		eq(t, n, e.items(search("name="+url.QueryEscape(n)), "admin", "name"), both...)
	}
	eq(t, "half escaped", e.items(search("name="+url.QueryEscape("github.com/burnt!sushi/toml")), "admin", "name"))
	for _, v := range []string{"v1.5.0-RC1", "v1.5.0-!r!c1"} {
		eq(t, v, e.items(search("version="+url.QueryEscape(v)), "admin", "version"), "v1.5.0-RC1")
	}
	eq(t, "asset search", e.items(v1+"/search/assets?repository=go-px&name="+url.QueryEscape("github.com/!burnt!sushi/toml")+"&version=v1.4.0", "admin", "path"),
		"/github.com/!burnt!sushi/toml/@v/v1.4.0.mod")
}

// assetIDs maps the paths of a repository's assets to their ids.
func assetIDs(e *env, repository string) map[string]string {
	e.t.Helper()
	var p page[map[string]any]
	e.get(v1+"/assets?repository="+repository, "admin", &p)
	out := map[string]string{}
	for _, it := range p.Items {
		out[fmt.Sprint(it["path"])] = fmt.Sprint(it["id"])
	}
	return out
}

// golang-04, a proxy: deleting a cached file of a version drops the version's
// files; they come back from the remote. A version list is a file of its own.
func TestGoDeleteCachedAsset(t *testing.T) {
	e, up := startGo(t)
	const repo, mod = "/repository/go-px/", "github.com/!burnt!sushi/toml/@v/"
	for _, f := range []string{"v1.4.0.info", "v1.4.0.mod", "v1.4.0.zip", "v1.5.0-!r!c1.info", "list"} {
		e.want("GET", repo+mod+f, "", "", 200)
	}
	ids := assetIDs(e, "go-px")
	e.want("DELETE", v1+"/assets/"+ids["/"+mod+"v1.4.0.zip"], "", "", 401)
	e.want("DELETE", v1+"/assets/"+ids["/"+mod+"v1.4.0.zip"], "ci", "", 403)
	e.want("DELETE", v1+"/assets/"+ids["/"+mod+"v1.4.0.zip"], "admin", "", 204)
	left := assetIDs(e, "go-px")
	for _, f := range []string{"v1.4.0.info", "v1.4.0.mod", "v1.4.0.zip"} {
		if _, ok := left["/"+mod+f]; ok {
			t.Fatalf("%s is still cached: %v", f, left)
		}
	}
	if _, ok := left["/"+mod+"list"]; !ok || left["/"+mod+"v1.5.0-!r!c1.info"] == "" {
		t.Fatalf("other files went too: %v", left)
	}
	e.want("GET", v1+"/assets/"+ids["/"+mod+"v1.4.0.mod"], "admin", "", 404)
	e.want("DELETE", v1+"/assets/"+ids["/"+mod+"v1.4.0.mod"], "admin", "", 404)
	e.want("DELETE", v1+"/assets/"+left["/"+mod+"list"], "admin", "", 204)
	if _, ok := assetIDs(e, "go-px")["/"+mod+"v1.5.0-!r!c1.info"]; !ok {
		t.Fatal("deleting the list took a version")
	}
	before := up.hits.Load()
	if b := e.want("GET", repo+mod+"v1.4.0.zip", "", "", 200); string(b) != "zip" {
		t.Fatalf("refetched: %q", b)
	}
	if up.hits.Load() != before+1 {
		t.Fatalf("the remote was not asked: %d", up.hits.Load()-before)
	}
}

func goZip(t *testing.T, modVersion string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"go.mod": "module example.com/m\n", "m.go": "package m\n"} {
		w, err := zw.Create(modVersion + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	zw.Close()
	return buf.Bytes()
}

// golang-04, hosted: any file of a version deletes the version, which leaves
// the list, as the native DELETE does; it answered 400 before.
func TestGoDeleteHostedAsset(t *testing.T) {
	e, _ := startGo(t)
	const repo, mod = "/repository/go-hosted/", "example.com/m/@v/"
	for _, v := range []string{"v1.0.0", "v1.1.0"} {
		if s, b, _ := e.send("PUT", repo+mod+v+".zip", "admin", "application/zip", goZip(t, "example.com/m@"+v)); s != 201 && s != 200 {
			t.Fatalf("upload %s: %d %s", v, s, b)
		}
	}
	ids := assetIDs(e, "go-hosted")
	for _, f := range []string{"v1.0.0.info", "v1.0.0.mod", "v1.0.0.zip"} {
		if ids["/"+mod+f] == "" {
			t.Fatalf("no %s: %v", f, ids)
		}
	}
	e.want("DELETE", v1+"/assets/"+ids["/"+mod+"v1.0.0.mod"], "ci", "", 403)
	e.want("DELETE", v1+"/assets/"+ids["/"+mod+"v1.0.0.mod"], "admin", "", 204)
	for _, f := range []string{"v1.0.0.info", "v1.0.0.mod", "v1.0.0.zip"} {
		e.want("GET", repo+mod+f, "", "", 404)
	}
	if b := e.want("GET", repo+mod+"list", "", "", 200); string(b) != "v1.1.0\n" {
		t.Fatalf("list: %q", b)
	}
	if left := assetIDs(e, "go-hosted"); left["/"+mod+"v1.1.0.zip"] == "" {
		t.Fatalf("the other version went too: %v", left)
	}
}
