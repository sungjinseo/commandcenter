package nexusapi_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/pgp"
)

// debFile is a minimal Debian package: debian-binary, control.tar.gz with
// ./control, an empty data.tar.gz.
func debFile(t *testing.T, pkg, version, arch string) []byte {
	t.Helper()
	targz := func(files map[string]string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for name, body := range files {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
		tw.Close()
		gz.Close()
		return buf.Bytes()
	}
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range []struct {
		name string
		data []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", targz(map[string]string{"./control": "Package: " + pkg + "\nVersion: " + version + "\nArchitecture: " + arch + "\nMaintainer: QA <qa@example.test>\nDescription: test\n"})},
		{"data.tar.gz", targz(nil)},
	} {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		b.Write(m.data)
		if len(m.data)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

func testKey(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "pgp", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// Nexus apt bodies: apt.distribution and aptSigning (keypair with its
// passphrase) on hosted, apt.flat and enforceDistribution on proxies;
// GET reports them but never the key; an update without them keeps
// them, and a change regenerates dists/ at once.
func TestAptRepositoryBodies(t *testing.T) {
	e := start(t, true)
	hosted := func(dist, signing string) string {
		return `{"name":"apt-h","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW_ONCE"}` +
			dist + signing + `}`
	}
	locked := testKey(t, "ed25519-pass.asc")
	b := e.want("POST", v1+"/repositories/apt/hosted", "admin", hosted(`,"apt":{"distribution":"bionic"}`,
		`,"aptSigning":{"keypair":`+jsonString(locked)+`,"passphrase":"wrong"}`), 400)
	if !strings.Contains(string(b), "wrong passphrase") {
		t.Fatalf("wrong passphrase: %s", b)
	}
	e.want("POST", v1+"/repositories/apt/hosted", "admin", hosted(`,"apt":{"distribution":"bionic"}`,
		`,"aptSigning":{"keypair":`+jsonString(locked)+`,"passphrase":"pass"}`), 201)
	var s map[string]any
	raw := e.want("GET", v1+"/repositories/apt/hosted/apt-h", "admin", "", 200)
	json.Unmarshal(raw, &s)
	if s["apt"].(map[string]any)["distribution"] != "bionic" || s["aptSigning"] == nil || strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatalf("settings = %s", raw)
	}
	if sg := s["aptSigning"].(map[string]any); sg["keypair"] != nil || sg["passphrase"] != nil {
		t.Fatalf("aptSigning = %v", sg)
	}
	put := func(p string, body []byte) int {
		t.Helper()
		req, _ := http.NewRequest("PUT", e.ts.URL+"/repository/apt-h/"+p, bytes.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := put("pool/hello.deb", debFile(t, "hello", "1.0", "amd64")); st != 201 {
		t.Fatalf("upload: %d", st)
	}
	if b := e.want("GET", "/repository/apt-h/dists/bionic/InRelease", "", "", 200); !bytes.HasPrefix(b, []byte("-----BEGIN PGP SIGNED MESSAGE-----\n")) {
		t.Fatalf("InRelease:\n%s", b)
	}
	unlocked, _ := pgp.Unlock(locked, "pass")
	k, _ := pgp.ParsePrivateKey(unlocked)
	if b := e.want("GET", "/repository/apt-h/public.key", "", "", 200); string(b) != k.PublicKey() {
		t.Fatal("public.key is not the configured key's")
	}

	// an update without apt or aptSigning keeps both; one renaming the
	// distribution regenerates dists/ (the old one is gone at once)
	e.want("PUT", v1+"/repositories/apt/hosted/apt-h", "admin", hosted("", ""), 204)
	json.Unmarshal(e.want("GET", v1+"/repositories/apt/hosted/apt-h", "admin", "", 200), &s)
	if s["apt"].(map[string]any)["distribution"] != "bionic" || s["aptSigning"] == nil {
		t.Fatalf("after an update without apt: %v", s)
	}
	e.want("PUT", v1+"/repositories/apt/hosted/apt-h", "admin", hosted(`,"apt":{"distribution":"focal"}`, ""), 204)
	e.want("GET", "/repository/apt-h/dists/bionic/Release", "", "", 404)
	if b := e.want("GET", "/repository/apt-h/dists/focal/Release", "", "", 200); !bytes.Contains(b, []byte("\nSuite: focal\n")) {
		t.Fatalf("focal Release:\n%s", b)
	}
	// a new key signs at once
	before := e.want("GET", "/repository/apt-h/dists/focal/InRelease", "", "", 200)
	rsa := testKey(t, "rsa2048.asc")
	e.want("PUT", v1+"/repositories/apt/hosted/apt-h", "admin", hosted("", `,"aptSigning":{"keypair":`+jsonString(rsa)+`}`), 204)
	after := e.want("GET", "/repository/apt-h/dists/focal/InRelease", "", "", 200)
	k2, _ := pgp.ParsePrivateKey(rsa)
	if bytes.Equal(before, after) || string(e.want("GET", "/repository/apt-h/public.key", "", "", 200)) != k2.PublicKey() {
		t.Fatal("InRelease not re-signed with the new key")
	}
	// the UI's config never shows the key either
	if b := e.want("GET", "/-/api/repositories/apt-h/config", "admin", "", 200); strings.Contains(string(b), "PRIVATE") || !strings.Contains(string(b), `"signingKeySet":true`) {
		t.Fatalf("config: %s", b)
	}

	// proxies: distribution, flat, enforceDistribution
	proxy := `{"name":"apt-p","online":true,"storage":{"blobStoreName":"default"},"proxy":{"remoteUrl":"https://deb.debian.org/debian/","contentMaxAge":-1,"metadataMaxAge":1440},
	  "negativeCache":{"enabled":true,"timeToLive":1},"httpClient":{"blocked":true,"autoBlock":true},"apt":{"distribution":"bookworm","flat":false,"enforceDistribution":true}}`
	e.want("POST", v1+"/repositories/apt/proxy", "admin", proxy, 201)
	json.Unmarshal(e.want("GET", v1+"/repositories/apt/proxy/apt-p", "admin", "", 200), &s)
	if a := s["apt"].(map[string]any); a["distribution"] != "bookworm" || a["flat"] != false || a["enforceDistribution"] != true || s["aptSigning"] != nil {
		t.Fatalf("proxy settings = %v", s)
	}
	e.want("GET", "/repository/apt-p/dists/trixie/Release", "", "", 404) // enforced, the remote not asked (blocked)
	e.want("POST", v1+"/repositories/apt/proxy", "admin", strings.Replace(strings.Replace(proxy, `"apt-p"`, `"apt-p2"`, 1), `"distribution":"bookworm",`, "", 1), 400)
}

// raw.contentDisposition: a create without the raw block gets Nexus's
// ATTACHMENT; an update without it keeps the stored value.
func TestRawContentDispositionDefault(t *testing.T) {
	e := start(t, true)
	body := `{"name":"raw-d","online":true,"storage":{"blobStoreName":"default","writePolicy":"ALLOW"}%s}`
	disposition := func() string {
		var s map[string]any
		e.get(v1+"/repositories/raw/hosted/raw-d", "admin", &s)
		return s["raw"].(map[string]any)["contentDisposition"].(string)
	}
	e.want("POST", v1+"/repositories/raw/hosted", "admin", fmt.Sprintf(body, ""), 201)
	if d := disposition(); d != "ATTACHMENT" {
		t.Fatalf("create without raw: %s", d)
	}
	e.want("PUT", v1+"/repositories/raw/hosted/raw-d", "admin", fmt.Sprintf(body, `,"raw":{"contentDisposition":"INLINE"}`), 204)
	e.want("PUT", v1+"/repositories/raw/hosted/raw-d", "admin", fmt.Sprintf(body, ""), 204)
	if d := disposition(); d != "INLINE" {
		t.Fatalf("update without raw: %s", d)
	}
}

// GET /service/rest/repository/browse/: Nexus's HTML listing, with the
// 303 to the trailing slash, a group's members merged, and a 404 page for
// an unknown repository or path.
func TestBrowseHTML(t *testing.T) {
	e := start(t, false)
	for _, p := range []string{"docs/a.txt", "docs/sub/b.txt", "top<1>.txt"} {
		req, _ := http.NewRequest("PUT", e.ts.URL+"/repository/raw-files/"+p, strings.NewReader("x"))
		req.SetBasicAuth("ci", "pw")
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 201 {
			t.Fatalf("PUT %s: %v %v", p, err, resp)
		}
	}
	const b = "/service/rest/repository/browse/"
	e.want("GET", b+"raw-files/", "", "", 401) // anonymousRead off
	st, _, hdr := e.do("GET", b+"raw-files/docs", "ci", "")
	if st != 303 || hdr.Get("Location") != b+"raw-files/docs/" {
		t.Fatalf("no slash: %d %q", st, hdr.Get("Location"))
	}
	page := string(e.want("GET", b+"raw-files/docs/", "ci", "", 200))
	for _, want := range []string{"<title>Index of /raw-files/docs</title>", `<a href="../">Parent Directory</a>`, `<a href="sub/">sub/</a>`,
		`<a href="http://nexus.example.com/repository/raw-files/docs/a.txt">a.txt</a>`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
	if root := string(e.want("GET", b+"raw-files/", "ci", "", 200)); !strings.Contains(root, `>top&lt;1&gt;.txt</a>`) {
		t.Errorf("root:\n%s", root)
	}
	e.want("POST", v1+"/repositories/raw/group", "admin", `{"name":"raw-all","online":true,"storage":{"blobStoreName":"default"},"group":{"memberNames":["raw-files"]}}`, 201)
	if g := string(e.want("GET", b+"raw-all/docs/", "admin", "", 200)); !strings.Contains(g, `>a.txt</a>`) {
		t.Errorf("group:\n%s", g)
	}
	if list := string(e.want("GET", b, "ci", "", 200)); !strings.Contains(list, `<a href="raw-files/">raw-files/</a>`) {
		t.Errorf("repositories:\n%s", list)
	}
	e.want("GET", b+"nope/", "ci", "", 404)
	e.want("GET", b+"raw-files/nope/", "ci", "", 404)
}

// DELETE /-/api/repositories/{name}/folder deletes what a raw hosted
// folder holds, file by file.
func TestDeleteFolder(t *testing.T) {
	e := start(t, true)
	for _, p := range []string{"site/a.txt", "site/css/b.css", "keep/c.txt"} {
		req, _ := http.NewRequest("PUT", e.ts.URL+"/repository/raw-files/"+p, strings.NewReader("x"))
		req.SetBasicAuth("ci", "pw")
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 201 {
			t.Fatalf("PUT %s: %v %v", p, err, resp)
		}
	}
	del := func(user, q string) (int, string) {
		req, _ := http.NewRequest("DELETE", e.ts.URL+"/-/api/repositories/raw-files/folder?"+q, nil)
		req.SetBasicAuth(user, "pw")
		req.Header.Set("X-Command-Center-UI", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	for _, q := range []string{"path=", "path=site", "path=../"} {
		if st, _ := del("admin", q); st != 400 {
			t.Errorf("%s: %d", q, st)
		}
	}
	if st, b := del("ci", "path=site/"); st != 200 || !strings.Contains(b, `"assets":2`) {
		t.Fatalf("delete folder: %d %s", st, b)
	}
	e.want("GET", "/repository/raw-files/site/css/b.css", "", "", 404)
	e.want("GET", "/repository/raw-files/keep/c.txt", "", "", 200)
	if st, _ := del("ci", "path=site/"); st != 404 {
		t.Fatalf("again: %d", st)
	}
	if st, _ := del("admin", "path=x/"); st != 404 {
		t.Fatalf("missing folder: %d", st)
	}
}

// Nexus's apt rebuild task type creates repository.rebuild-metadata of
// apt repositories; its apt-only properties are dropped.
func TestAptRebuildTaskAlias(t *testing.T) {
	e := start(t, false)
	var created map[string]any
	json.Unmarshal(e.want("POST", v1+"/tasks", "admin", `{"type":"repository.apt.rebuild.metadata","name":"apt","frequency":{"schedule":"manual"},
		"properties":{"repositoryName":"*","rebuildAptMetadataFullRebuild":"true","resetProxyMetadata":"false"}}`, 201), &created)
	var m map[string]any
	e.get("/-/api/tasks/"+created["id"].(string), "admin", &m)
	if m["type"] != "repository.rebuild-metadata" || fmt.Sprint(m["params"]) != "map[format:apt]" {
		t.Fatalf("stored: %v", m)
	}
	e.want("PUT", v1+"/tasks/"+created["id"].(string), "admin", `{"type":"repository.apt.rebuild.metadata","name":"apt2"}`, 204)
	e.get("/-/api/tasks/"+created["id"].(string), "admin", &m)
	if m["name"] != "apt2" || fmt.Sprint(m["params"]) != "map[format:apt]" {
		t.Fatalf("updated: %v", m)
	}
}

// DELETE /v1/assets/{id} of an apt hosted package deletes the package.
func TestAptAssetDelete(t *testing.T) {
	e := start(t, true)
	e.want("POST", v1+"/repositories/apt/hosted", "admin", `{"name":"apt-h","online":true,"storage":{"blobStoreName":"default","writePolicy":"ALLOW_ONCE"},"apt":{"distribution":"stable"}}`, 201)
	req, _ := http.NewRequest("PUT", e.ts.URL+"/repository/apt-h/pool/x.deb", bytes.NewReader(debFile(t, "hello", "1.0", "amd64")))
	req.SetBasicAuth("admin", "pw")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload: %v %v", err, resp)
	}
	var page struct {
		Items []struct{ ID, Path string }
	}
	e.get(v1+"/assets?repository=apt-h", "admin", &page)
	id := ""
	for _, a := range page.Items {
		switch {
		case strings.HasSuffix(a.Path, ".deb"):
			id = a.ID
		case strings.HasSuffix(a.Path, "/Release"):
			e.want("DELETE", v1+"/assets/"+a.ID, "admin", "", 400) // generated: never deleted alone
		}
	}
	e.want("DELETE", v1+"/assets/"+id, "admin", "", 204)
	if b := e.want("GET", "/repository/apt-h/dists/stable/Release", "", "", 200); bytes.Contains(b, []byte("binary-amd64")) {
		t.Fatalf("Release still lists the package:\n%s", b)
	}
	var comps struct{ Items []any }
	e.get(v1+"/components?repository=apt-h", "admin", &comps)
	if len(comps.Items) != 0 {
		t.Fatalf("components: %v", comps.Items)
	}
}
