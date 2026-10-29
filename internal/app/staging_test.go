package app

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// stagingEnv runs the whole app: admin "admin", deployer "ci" (every
// action everywhere) and "dev", all with password "pw".
type stagingEnv struct {
	t  *testing.T
	a  *App
	ts *httptest.Server
}

func newStagingEnv(t *testing.T) *stagingEnv {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
dataDir: %s
baseURL: http://cc.example
anonymousRead: false
users:
  - { name: admin, passwordHash: %q, roles: [admin] }
  - { name: ci, passwordHash: %q, roles: [deployer] }
  - { name: dev, passwordHash: %q, roles: [] }
roles:
  deployer: { write: ["*"] }
repositories:
  - { name: maven-staging, format: maven, type: hosted }
  - { name: maven-releases, format: maven, type: hosted, writePolicy: allow_once }
  - { name: npm-staging, format: npm, type: hosted }
  - { name: npm-releases, format: npm, type: hosted }
  - { name: raw-a, format: raw, type: hosted }
  - { name: pypi-a, format: pypi, type: hosted }
  - { name: pypi-b, format: pypi, type: hosted }
  - { name: nuget-a, format: nuget, type: hosted }
  - { name: nuget-b, format: nuget, type: hosted }
  - { name: helm-a, format: helm, type: hosted }
  - { name: helm-b, format: helm, type: hosted }
  - { name: yum-a, format: yum, type: hosted }
  - { name: yum-b, format: yum, type: hosted }
  - { name: docker-a, format: docker, type: hosted }
  - { name: docker-b, format: docker, type: hosted }
`, t.TempDir(), string(h), string(h), string(h))))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	a.Tasks.MoveSettle = time.Millisecond
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Tasks.Stop(); a.Close() })
	return &stagingEnv{t, a, ts}
}

// do sends method path as user with body (a string or bytes) and the
// headers given as name, value pairs, and returns the status and body.
func (e *stagingEnv) do(method, path, user string, body any, hdr ...string) (int, string) {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case string:
		r = strings.NewReader(b)
	case []byte:
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, r)
	req.SetBasicAuth(user, "pw")
	req.Header.Set(auth.UIHeader, "1")
	if s, ok := body.(string); ok && strings.HasPrefix(s, "{") {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *stagingEnv) want(status int, method, path, user string, body any, hdr ...string) string {
	e.t.Helper()
	st, b := e.do(method, path, user, body, hdr...)
	if st != status {
		e.t.Fatalf("%s %s as %s: %d, want %d: %s", method, path, user, st, status, b)
	}
	return b
}

func (e *stagingEnv) json(status int, method, path, user string, body any) map[string]any {
	e.t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(e.want(status, method, path, user, body)), &out); err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return out
}

// moved lists "group:name:version" of the components under data.<field>.
func moved(res map[string]any, field string) []string {
	data, _ := res["data"].(map[string]any)
	list, _ := data[field].([]any)
	out := []string{}
	for _, x := range list {
		c := x.(map[string]any)
		out = append(out, fmt.Sprint(c["group"], ":", c["name"], ":", c["version"]))
	}
	return out
}

func eq(got []string, want ...string) bool { return strings.Join(got, ",") == strings.Join(want, ",") }

// putMaven uploads a jar and a pom of g:a:v.
func (e *stagingEnv) putMaven(repoName, g, a, v, jar string) {
	e.t.Helper()
	dir := "/repository/" + repoName + "/" + strings.ReplaceAll(g, ".", "/") + "/" + a + "/" + v + "/" + a + "-" + v
	pom := fmt.Sprintf("<project><modelVersion>4.0.0</modelVersion><groupId>%s</groupId><artifactId>%s</artifactId><version>%s</version></project>", g, a, v)
	e.want(201, "PUT", dir+".pom", "ci", pom)
	e.want(201, "PUT", dir+".jar", "ci", jar)
}

func tgz(files ...string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i+1 < len(files); i += 2 {
		tw.WriteHeader(&tar.Header{Name: files[i], Mode: 0o644, Size: int64(len(files[i+1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(files[i+1]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipOf(files ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i+1 < len(files); i += 2 {
		w, _ := zw.Create(files[i])
		w.Write([]byte(files[i+1]))
	}
	zw.Close()
	return buf.Bytes()
}

// npmPublish is what `npm publish` sends for name@version.
func npmPublish(name, version string) string {
	pj, _ := json.Marshal(map[string]string{"name": name, "version": version})
	t := tgz("package/package.json", string(pj), "package/index.js", version)
	s1, s512 := sha1.Sum(t), sha512.Sum512(t)
	doc := map[string]any{
		"_id": name, "name": name, "dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{"name": name, "version": version,
			"dist": map[string]any{"shasum": hex.EncodeToString(s1[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:])}}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(t), "length": len(t)}},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// rpm builds a minimal package name-ver-rel.arch (lead, signature and main
// headers, payload) as the yum format parses one.
func rpm(name, ver, rel, arch string) []byte {
	type entry struct {
		tag int32
		val any // string or int32
	}
	header := func(es []entry) []byte {
		var index, data bytes.Buffer
		for _, e := range es {
			var typ int32
			off := data.Len()
			switch v := e.val.(type) {
			case string:
				typ = 6
				data.WriteString(v + "\x00")
			case int32:
				for data.Len()%4 != 0 {
					data.WriteByte(0)
				}
				off, typ = data.Len(), 4
				binary.Write(&data, binary.BigEndian, v)
			}
			binary.Write(&index, binary.BigEndian, []int32{e.tag, typ, int32(off), 1})
		}
		var out bytes.Buffer
		out.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})
		binary.Write(&out, binary.BigEndian, []int32{int32(len(es)), int32(data.Len())})
		out.Write(index.Bytes())
		out.Write(data.Bytes())
		return out.Bytes()
	}
	var out bytes.Buffer
	lead := make([]byte, 96)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0})
	out.Write(lead)
	out.Write(header([]entry{{1000, int32(1)}}))
	for out.Len()%8 != 0 {
		out.WriteByte(0)
	}
	out.Write(header([]entry{{1000, name}, {1001, ver}, {1002, rel}, {1022, arch}, {1004, name + " summary"},
		{1005, name + " description"}, {1006, int32(1700000000)}, {1009, int32(1)}}))
	out.WriteString("payload")
	return out.Bytes()
}

func gunzip(t *testing.T, b string) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(zr)
	return string(out)
}

func TestTagsAPIAndSearch(t *testing.T) {
	e := newStagingEnv(t)
	e.putMaven("maven-staging", "org.example", "app", "1.0", "jar-1.0")
	e.putMaven("maven-staging", "org.example", "app", "1.1", "jar-1.1")
	e.putMaven("maven-staging", "org.other", "lib", "1.0", "lib-1.0")

	// CRUD: our API and the Nexus one.
	tag := e.json(201, "POST", "/-/api/tags", "admin", `{"name":"rc-1","attributes":{"jvm":"17","build":{"id":7}}}`)
	if tag["name"] != "rc-1" || tag["attributes"].(map[string]any)["jvm"] != "17" || tag["firstCreated"] == "" || tag["lastUpdated"] != tag["firstCreated"] {
		t.Fatalf("created: %v", tag)
	}
	e.want(409, "POST", "/-/api/tags", "admin", `{"name":"rc-1"}`)
	e.want(400, "POST", "/-/api/tags", "admin", `{"name":"-bad name"}`)
	e.want(400, "POST", "/-/api/tags", "admin", `{"name":"t","attributes":[1]}`)
	e.want(403, "POST", "/-/api/tags", "ci", `{"name":"t"}`)
	if st, b := e.do("POST", "/service/rest/v1/tags", "ci", `{"name":"t"}`); st != 403 || !strings.Contains(b, `"message"`) {
		t.Fatalf("non-admin create via REST: %d %s", st, b)
	}
	got := e.json(200, "POST", "/service/rest/v1/tags", "admin", `{"name":"t2","attributes":{}}`)
	if got["name"] != "t2" {
		t.Fatalf("REST create: %v", got)
	}
	list := e.json(200, "GET", "/service/rest/v1/tags", "ci", "")
	if items := list["items"].([]any); len(items) != 2 || list["continuationToken"] != nil {
		t.Fatalf("REST list: %v", list)
	}
	up := e.json(200, "PUT", "/service/rest/v1/tags/rc-1", "admin", `{"attributes":{"jvm":"21"}}`)
	if up["attributes"].(map[string]any)["jvm"] != "21" {
		t.Fatalf("REST update: %v", up)
	}
	e.want(404, "GET", "/service/rest/v1/tags/nope", "admin", "")
	e.want(204, "DELETE", "/service/rest/v1/tags/t2", "admin", "")
	e.want(404, "GET", "/-/api/tags/t2", "admin", "")

	// Associate by search, through the Nexus path.
	res := e.json(200, "POST", "/service/rest/v1/tags/associate/rc-1?repository=maven-staging&group=org.example&version=1.0", "ci", "")
	if res["status"].(float64) != 200 || res["message"] != "Association successful" || !eq(moved(res, "components associated"), "org.example:app:1.0") {
		t.Fatalf("associate: %v", res)
	}
	e.want(404, "POST", "/service/rest/v1/tags/associate/nope?repository=maven-staging&name=app", "ci", "")
	// dev may read but not edit: nothing is associated.
	e.want(201, "POST", "/-/api/roles", "admin", `{"id":"reader","privileges":[{"target":"repo:maven-staging","actions":["read","delete"]}]}`)
	e.want(200, "PUT", "/-/api/users/dev", "admin", `{"roles":["reader"]}`)
	e.want(403, "POST", "/-/api/tags/rc-1/associate?repository=maven-staging&name=lib", "dev", "")

	// The search filters by tag and shows the tags.
	var search struct {
		Items []struct {
			Name, Version string
			Tags          []string
		}
	}
	json.Unmarshal([]byte(e.want(200, "GET", "/service/rest/v1/search?tag=rc-1", "ci", "")), &search)
	if len(search.Items) != 1 || search.Items[0].Version != "1.0" || strings.Join(search.Items[0].Tags, ",") != "rc-1" {
		t.Fatalf("REST search by tag: %+v", search)
	}
	json.Unmarshal([]byte(e.want(200, "GET", "/service/rest/v1/search?repository=maven-staging&name=app", "ci", "")), &search)
	if len(search.Items) != 2 || len(search.Items[1].Tags) != 0 {
		t.Fatalf("REST search: %+v", search)
	}
	var ui struct {
		Components []struct {
			Repo, Name, Version string
			Tags                []string
			Assets              []struct{ Path string }
		}
	}
	json.Unmarshal([]byte(e.want(200, "GET", "/-/api/search/components?tag=rc-1", "ci", "")), &ui)
	if len(ui.Components) != 1 || ui.Components[0].Version != "1.0" || len(ui.Components[0].Assets) != 2 || ui.Components[0].Tags[0] != "rc-1" {
		t.Fatalf("UI search by tag: %+v", ui)
	}
	json.Unmarshal([]byte(e.want(200, "GET", "/-/api/search/components?name=app", "ci", "")), &ui)
	if len(ui.Components) != 2 || strings.Join(ui.Components[0].Tags, ",") != "rc-1" || ui.Components[1].Tags == nil || len(ui.Components[1].Tags) != 0 {
		t.Fatalf("UI search tags: %+v", ui)
	}

	// Dissociate removes it from what had it; a component delete drops it.
	res = e.json(200, "DELETE", "/service/rest/v1/tags/associate/rc-1?repository=maven-staging&name=app", "ci", "")
	if res["message"] != "Disassociation successful" || !eq(moved(res, "components disassociated"), "org.example:app:1.0") {
		t.Fatalf("dissociate: %v", res)
	}
	e.want(200, "POST", "/-/api/tags/rc-1/associate?repository=maven-staging&name=lib", "ci", "")
	e.want(200, "DELETE", "/-/api/repositories/maven-staging/components?group=org.other&name=lib&version=1.0", "ci", "")
	json.Unmarshal([]byte(e.want(200, "GET", "/service/rest/v1/search?tag=rc-1", "ci", "")), &search)
	e.want(201, "PUT", "/repository/maven-staging/org/other/lib/1.0/lib-1.0.jar", "ci", "again")
	if json.Unmarshal([]byte(e.want(200, "GET", "/service/rest/v1/search?tag=rc-1", "ci", "")), &search); len(search.Items) != 0 {
		t.Fatalf("a deleted component kept its tag: %+v", search)
	}
	// A tag delete removes its associations.
	e.want(200, "POST", "/-/api/tags/rc-1/associate?repository=maven-staging&name=app&version=1.1", "ci", "")
	e.want(204, "DELETE", "/-/api/tags/rc-1", "admin", "")
	e.want(201, "POST", "/-/api/tags", "admin", `{"name":"rc-1"}`)
	if json.Unmarshal([]byte(e.want(200, "GET", "/service/rest/v1/search?tag=rc-1", "ci", "")), &search); len(search.Items) != 0 {
		t.Fatalf("a re-created tag has the old associations: %+v", search)
	}
}

func TestStagingMoveMaven(t *testing.T) {
	e := newStagingEnv(t)
	e.putMaven("maven-staging", "org.example", "app", "1.0", "jar-1.0")
	e.putMaven("maven-staging", "org.example", "app", "1.1", "jar-1.1")
	e.putMaven("maven-staging", "org.example", "app", "1.2", "jar-1.2")
	e.want(201, "POST", "/-/api/tags", "admin", `{"name":"promote"}`)
	e.want(200, "POST", "/-/api/tags/promote/associate?repository=maven-staging&version=1.0", "ci", "")
	metadata := func(repoName string) string {
		return e.want(200, "GET", "/repository/"+repoName+"/org/example/app/maven-metadata.xml", "ci", "")
	}

	// Without add on the destination: 403, nothing moved.
	e.want(201, "POST", "/-/api/roles", "admin", `{"id":"stager","privileges":[{"target":"repo:maven-staging","actions":["read","delete"]},{"target":"repo:maven-releases","actions":["read"]}]}`)
	e.want(200, "PUT", "/-/api/users/dev", "admin", `{"roles":["stager"]}`)
	if st, b := e.do("POST", "/service/rest/v1/staging/move/maven-releases?repository=maven-staging&tag=promote", "dev", ""); st != 403 {
		t.Fatalf("move without add: %d %s", st, b)
	}
	e.want(200, "GET", "/repository/maven-staging/org/example/app/1.0/app-1.0.jar", "ci", "")
	e.want(400, "POST", "/service/rest/v1/staging/move/maven-releases?repository=maven-staging", "ci", "") // no selector
	e.want(400, "POST", "/service/rest/v1/staging/move/maven-staging?repository=maven-staging&tag=promote", "ci", "")
	e.want(400, "POST", "/service/rest/v1/staging/move/npm-releases?repository=maven-staging&tag=promote", "ci", "")
	e.want(404, "POST", "/service/rest/v1/staging/move/nope?repository=maven-staging&tag=promote", "ci", "")

	// Move by tag: the files, the metadata of both and the tag go along.
	res := e.json(200, "POST", "/service/rest/v1/staging/move/maven-releases?repository=maven-staging&tag=promote", "ci", "")
	if res["message"] != "Move Successful" || res["data"].(map[string]any)["destination"] != "maven-releases" ||
		!eq(moved(res, "components moved"), "org.example:app:1.0") {
		t.Fatalf("move: %v", res)
	}
	if b := e.want(200, "GET", "/repository/maven-releases/org/example/app/1.0/app-1.0.jar", "ci", ""); b != "jar-1.0" {
		t.Fatalf("moved jar: %q", b)
	}
	e.want(200, "GET", "/repository/maven-releases/org/example/app/1.0/app-1.0.pom.sha1", "ci", "")
	e.want(404, "GET", "/repository/maven-staging/org/example/app/1.0/app-1.0.jar", "ci", "")
	if m := metadata("maven-releases"); !strings.Contains(m, "<version>1.0</version>") || strings.Contains(m, "<version>1.1</version>") {
		t.Fatalf("destination metadata: %s", m)
	}
	if m := metadata("maven-staging"); strings.Contains(m, "<version>1.0</version>") || !strings.Contains(m, "<version>1.1</version>") {
		t.Fatalf("source metadata: %s", m)
	}
	var search struct {
		Items []struct{ Repository, Version string }
	}
	json.Unmarshal([]byte(e.want(200, "GET", "/service/rest/v1/search?tag=promote", "ci", "")), &search)
	if len(search.Items) != 1 || search.Items[0].Repository != "maven-releases" {
		t.Fatalf("the tag did not move: %+v", search)
	}

	// allow_once: a version the destination has fails; the others move.
	e.putMaven("maven-releases", "org.example", "app", "1.1", "other-1.1")
	res = e.json(409, "POST", "/-/api/staging/move?destination=maven-releases&repository=maven-staging&name=app&version=1.*", "ci", "")
	if !eq(moved(res, "components moved"), "org.example:app:1.2") || !eq(moved(res, "components failed"), "org.example:app:1.1") {
		t.Fatalf("partial move: %v", res)
	}
	if f := res["data"].(map[string]any)["components failed"].([]any)[0].(map[string]any); !strings.Contains(f["error"].(string), "already exists") {
		t.Fatalf("failure: %v", f)
	}
	if b := e.want(200, "GET", "/repository/maven-staging/org/example/app/1.1/app-1.1.jar", "ci", ""); b != "jar-1.1" {
		t.Fatalf("the failed component left the source: %q", b)
	}
	if b := e.want(200, "GET", "/repository/maven-releases/org/example/app/1.1/app-1.1.jar", "ci", ""); b != "other-1.1" {
		t.Fatalf("the destination's file was replaced: %q", b)
	}
	if m := metadata("maven-releases"); !strings.Contains(m, "<version>1.2</version>") {
		t.Fatalf("destination metadata: %s", m)
	}

	// Staging delete.
	e.putMaven("maven-staging", "org.example", "app", "2.0", "jar-2.0")
	res = e.json(200, "POST", "/service/rest/v1/staging/delete?repository=maven-staging&version=2.0", "ci", "")
	if res["message"] != "Delete Successful" || !eq(moved(res, "components deleted"), "org.example:app:2.0") {
		t.Fatalf("delete: %v", res)
	}
	e.want(404, "GET", "/repository/maven-staging/org/example/app/2.0/app-2.0.jar", "ci", "")
	if m := metadata("maven-staging"); strings.Contains(m, "<version>2.0</version>") || !strings.Contains(m, "<version>1.1</version>") {
		t.Fatalf("source metadata after delete: %s", m)
	}
	e.want(400, "POST", "/service/rest/v1/staging/delete?repository=maven-staging", "ci", "")
	e.want(403, "POST", "/service/rest/v1/staging/delete?repository=maven-releases&version=1.0", "dev", "")

	// The activity log and the webhooks get the events.
	evs := e.json(200, "GET", "/-/api/events?kind=staging.move", "admin", "")["events"].([]any)
	if len(evs) != 2 || !strings.Contains(evs[0].(map[string]any)["detail"].(string), "from=maven-staging moved=1 failed=1") {
		t.Fatalf("events: %v", evs)
	}
	e.want(201, "POST", "/-/api/webhooks", "admin", `{"name":"h","url":"http://127.0.0.1:1/hook","events":["staging.move","staging.delete"]}`)
}

func TestStagingMoveNpmRawPypiNugetHelmYum(t *testing.T) {
	e := newStagingEnv(t)
	packument := func(repoName string) map[string]any {
		return e.json(200, "GET", "/repository/"+repoName+"/demo", "ci", "")
	}

	// npm: the version and its tarball move; both package documents follow.
	e.want(201, "PUT", "/repository/npm-staging/demo", "ci", npmPublish("demo", "1.0.0"))
	e.want(201, "PUT", "/repository/npm-staging/demo", "ci", npmPublish("demo", "1.1.0"))
	res := e.json(200, "POST", "/service/rest/v1/staging/move/npm-releases?repository=npm-staging&name=demo&version=1.0.0", "ci", "")
	if !eq(moved(res, "components moved"), ":demo:1.0.0") {
		t.Fatalf("npm move: %v", res)
	}
	if p := packument("npm-releases"); len(p["versions"].(map[string]any)) != 1 || p["dist-tags"].(map[string]any)["latest"] != "1.0.0" {
		t.Fatalf("destination packument: %v", p)
	}
	if p := packument("npm-staging"); p["versions"].(map[string]any)["1.0.0"] != nil || p["dist-tags"].(map[string]any)["latest"] != "1.1.0" {
		t.Fatalf("source packument: %v", p)
	}
	e.want(200, "GET", "/repository/npm-releases/demo/-/demo-1.0.0.tgz", "ci", "")
	e.json(200, "POST", "/-/api/staging/move?destination=npm-releases&repository=npm-staging&name=demo", "ci", "")
	if p := packument("npm-releases"); len(p["versions"].(map[string]any)) != 2 || p["dist-tags"].(map[string]any)["latest"] != "1.1.0" {
		t.Fatalf("destination packument: %v", p)
	}
	e.want(404, "GET", "/repository/npm-staging/demo", "ci", "")

	// raw, into a repository on another blob store: the blob is copied there.
	e.want(201, "POST", "/-/api/blobstores", "admin", `{"name":"second"}`)
	e.want(201, "POST", "/-/api/repositories", "admin", `{"name":"raw-b","format":"raw","type":"hosted","blobStore":"second"}`)
	e.want(201, "PUT", "/repository/raw-a/docs/a.txt", "ci", "alpha")
	e.want(201, "PUT", "/repository/raw-a/docs/b.txt", "ci", "beta")
	res = e.json(200, "POST", "/service/rest/v1/staging/move/raw-b?repository=raw-a&name=docs/a.txt", "ci", "")
	if !eq(moved(res, "components moved"), ":docs/a.txt:") {
		t.Fatalf("raw move: %v", res)
	}
	if b := e.want(200, "GET", "/repository/raw-b/docs/a.txt", "ci", ""); b != "alpha" {
		t.Fatalf("raw-b: %q", b)
	}
	e.want(404, "GET", "/repository/raw-a/docs/a.txt", "ci", "")
	e.want(200, "GET", "/repository/raw-a/docs/b.txt", "ci", "")
	sum := sha256.Sum256([]byte("alpha"))
	if _, err := e.a.Store.StoreByName("second").Stat(hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("the blob was not copied to the destination's store: %v", err)
	}

	// pypi: the files of the version; the simple index is the rows'.
	md := "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 1.0.0\n"
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, kv := range [][2]string{{":action", "file_upload"}, {"protocol_version", "1"}, {"name", "demo-pkg"}, {"version", "1.0.0"}, {"filetype", "bdist_wheel"}} {
		mw.WriteField(kv[0], kv[1])
	}
	fw, _ := mw.CreateFormFile("content", "demo_pkg-1.0.0-py3-none-any.whl")
	fw.Write(zipOf("demo_pkg-1.0.0.dist-info/METADATA", md))
	mw.Close()
	e.want(200, "POST", "/repository/pypi-a/", "ci", buf.Bytes(), "Content-Type", mw.FormDataContentType())
	res = e.json(200, "POST", "/service/rest/v1/staging/move/pypi-b?repository=pypi-a&name=demo-pkg", "ci", "")
	if !eq(moved(res, "components moved"), ":demo-pkg:1.0.0") {
		t.Fatalf("pypi move: %v", res)
	}
	if b := e.want(200, "GET", "/repository/pypi-b/simple/demo-pkg/", "ci", ""); !strings.Contains(b, "demo_pkg-1.0.0-py3-none-any.whl") {
		t.Fatalf("pypi-b index: %s", b)
	}
	e.want(200, "GET", "/repository/pypi-b/packages/demo-pkg/demo_pkg-1.0.0-py3-none-any.whl.metadata", "ci", "")
	if st, b := e.do("GET", "/repository/pypi-a/simple/demo-pkg/", "ci", ""); st == 200 && strings.Contains(b, ".whl") {
		t.Fatalf("pypi-a index: %s", b)
	}

	// nuget: the row, .nupkg and .nuspec; registration is the rows'.
	nuspec := `<?xml version="1.0"?><package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata>` +
		`<id>Demo.Pkg</id><version>1.0.0</version><authors>QA</authors><description>demo</description></metadata></package>`
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	fw, _ = mw.CreateFormFile("package", "package.nupkg")
	fw.Write(zipOf("[Content_Types].xml", "<Types/>", "Demo.Pkg.nuspec", nuspec, "lib/net6.0/Demo.Pkg.dll", "MZ"))
	mw.Close()
	e.want(201, "PUT", "/repository/nuget-a/api/v2/package", "ci", buf.Bytes(), "Content-Type", mw.FormDataContentType())
	res = e.json(200, "POST", "/service/rest/v1/staging/move/nuget-b?repository=nuget-a&name=demo.pkg", "ci", "")
	if !eq(moved(res, "components moved"), ":demo.pkg:1.0.0") {
		t.Fatalf("nuget move: %v", res)
	}
	if b := e.want(200, "GET", "/repository/nuget-b/v3-flatcontainer/demo.pkg/index.json", "ci", ""); !strings.Contains(b, "1.0.0") {
		t.Fatalf("nuget-b versions: %s", b)
	}
	e.want(200, "GET", "/repository/nuget-b/v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg", "ci", "")
	e.want(404, "GET", "/repository/nuget-a/v3-flatcontainer/demo.pkg/index.json", "ci", "")

	// helm: the chart and both index.yaml documents.
	chart := tgz("demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: 1.0.0\ndescription: demo chart\n", "demo/values.yaml", "a: 1\n")
	e.want(201, "PUT", "/repository/helm-a/demo-1.0.0.tgz", "ci", chart)
	e.want(201, "PUT", "/repository/helm-a/demo-1.1.0.tgz", "ci",
		tgz("demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: 1.1.0\n", "demo/values.yaml", "a: 1\n"))
	res = e.json(200, "POST", "/service/rest/v1/staging/move/helm-b?repository=helm-a&name=demo&version=1.0.0", "ci", "")
	if !eq(moved(res, "components moved"), ":demo:1.0.0") {
		t.Fatalf("helm move: %v", res)
	}
	if ix := e.want(200, "GET", "/repository/helm-b/index.yaml", "ci", ""); !strings.Contains(ix, "version: 1.0.0") || strings.Contains(ix, "version: 1.1.0") {
		t.Fatalf("helm-b index: %s", ix)
	}
	if ix := e.want(200, "GET", "/repository/helm-a/index.yaml", "ci", ""); strings.Contains(ix, "version: 1.0.0") || !strings.Contains(ix, "version: 1.1.0") {
		t.Fatalf("helm-a index: %s", ix)
	}
	if b := e.want(200, "GET", "/repository/helm-b/demo-1.0.0.tgz", "ci", ""); b != string(chart) {
		t.Fatal("helm-b chart differs")
	}

	// yum: the package and the repodata of both.
	e.want(201, "PUT", "/repository/yum-a/demo-1.0-1.x86_64.rpm", "ci", rpm("demo", "1.0", "1", "x86_64"))
	e.want(201, "PUT", "/repository/yum-a/other-2.0-1.x86_64.rpm", "ci", rpm("other", "2.0", "1", "x86_64"))
	res = e.json(200, "POST", "/service/rest/v1/staging/move/yum-b?repository=yum-a&name=demo", "ci", "")
	if !eq(moved(res, "components moved"), ":demo:1.0-1.x86_64") {
		t.Fatalf("yum move: %v", res)
	}
	primary := func(repoName string) string {
		repomd := e.want(200, "GET", "/repository/"+repoName+"/repodata/repomd.xml", "ci", "")
		m := regexp.MustCompile(`href="(repodata/[^"]*primary[^"]*)"`).FindStringSubmatch(repomd)
		if m == nil {
			t.Fatalf("%s repomd.xml: %s", repoName, repomd)
		}
		return gunzip(t, e.want(200, "GET", "/repository/"+repoName+"/"+m[1], "ci", ""))
	}
	if p := primary("yum-b"); !strings.Contains(p, "<name>demo</name>") || strings.Contains(p, "<name>other</name>") {
		t.Fatalf("yum-b primary: %s", p)
	}
	if p := primary("yum-a"); strings.Contains(p, "<name>demo</name>") || !strings.Contains(p, "<name>other</name>") {
		t.Fatalf("yum-a primary: %s", p)
	}
	e.want(200, "GET", "/repository/yum-b/demo-1.0-1.x86_64.rpm", "ci", "")

	// docker is refused as a whole.
	if b := e.want(400, "POST", "/service/rest/v1/staging/move/docker-b?repository=docker-a&name=x", "ci", ""); !strings.Contains(b, "format docker cannot be staged yet") {
		t.Fatalf("docker: %s", b)
	}
}

func TestChangeBlobStore(t *testing.T) {
	e := newStagingEnv(t)
	e.want(201, "POST", "/-/api/blobstores", "admin", `{"name":"s1"}`)
	e.want(201, "POST", "/-/api/blobstores", "admin", `{"name":"s2"}`)
	e.want(201, "POST", "/-/api/repositories", "admin", `{"name":"raw-m","format":"raw","type":"hosted","blobStore":"s1"}`)
	files := map[string]string{"a.txt": "alpha", "b.txt": "beta", "c.txt": "gamma"}
	for p, body := range files {
		e.want(201, "PUT", "/repository/raw-m/"+p, "ci", body)
	}
	has := func(store, body string) bool {
		s := sha256.Sum256([]byte(body))
		_, err := e.a.Store.StoreByName(store).Stat(hex.EncodeToString(s[:]))
		return err == nil
	}
	e.want(400, "POST", "/-/api/repositories/raw-m/blobstore", "admin", `{"blobStore":"nope"}`)
	e.want(400, "POST", "/-/api/repositories/raw-m/blobstore", "admin", `{"blobStore":"s1"}`)
	e.want(403, "POST", "/-/api/repositories/raw-m/blobstore", "ci", `{"blobStore":"s2"}`)
	e.want(404, "POST", "/-/api/repositories/nope/blobstore", "admin", `{"blobStore":"s2"}`)

	// The copy pauses after its first blob: reads and an upload meanwhile.
	paused, resume := make(chan struct{}), make(chan struct{})
	n := 0
	e.a.Tasks.AfterMoveCopy = func(repoName, _ string) {
		if n++; n == 1 {
			close(paused)
			<-resume
		}
	}
	res := e.json(202, "POST", "/-/api/repositories/raw-m/blobstore", "admin", `{"blobStore":"s2"}`)
	task := res["task"].(string)
	<-paused
	var repos struct {
		Repositories []struct {
			Name, BlobStore string
			BlobStoreMove   *struct {
				Source, Target, State string
				Copied, Total         int64
			}
		}
	}
	json.Unmarshal([]byte(e.want(200, "GET", "/-/api/repositories", "admin", "")), &repos)
	for _, r := range repos.Repositories {
		if r.Name == "raw-m" && (r.BlobStoreMove == nil || *r.BlobStoreMove != struct {
			Source, Target, State string
			Copied, Total         int64
		}{"s1", "s2", "copying", 1, 3}) {
			t.Fatalf("move state: %+v", r.BlobStoreMove)
		}
	}
	for p, body := range files {
		if b := e.want(200, "GET", "/repository/raw-m/"+p, "ci", ""); b != body {
			t.Fatalf("read %s during the move: %q", p, b)
		}
	}
	e.want(201, "PUT", "/repository/raw-m/d.txt", "ci", "delta")
	if !has("s2", "delta") || has("s1", "delta") {
		t.Fatal("an upload during the move was not written to the target only")
	}
	e.want(409, "POST", "/-/api/repositories/raw-m/blobstore", "admin", `{"blobStore":"s2"}`) // its task runs
	e.want(409, "DELETE", "/-/api/blobstores/s2", "admin", "")
	close(resume)
	e.a.Tasks.Wait()

	got := e.json(200, "GET", "/-/api/tasks/"+task, "admin", "")
	if got["lastResult"] != "OK" || !strings.Contains(got["lastLog"].(string), "4 blobs: 3 copied") {
		t.Fatalf("task: %v", got)
	}
	if cfg := e.json(200, "GET", "/-/api/repositories/raw-m/config", "admin", ""); cfg["blobStore"] != "s2" {
		t.Fatalf("config: %v", cfg)
	}
	repos.Repositories = nil // decoding into the old elements would keep their moves
	json.Unmarshal([]byte(e.want(200, "GET", "/-/api/repositories", "admin", "")), &repos)
	for _, r := range repos.Repositories {
		if r.Name == "raw-m" && (r.BlobStore != "s2" || r.BlobStoreMove != nil) {
			t.Fatalf("after the move: %+v", r)
		}
	}
	files["d.txt"] = "delta"
	for p, body := range files {
		if !has("s2", body) {
			t.Fatalf("%s is not in s2", p)
		}
	}
	// The old copies go at the next compaction of s1; reads use s2.
	if n, _, err := e.a.GCStore(context.Background(), "s1", 0); err != nil || n != 3 {
		t.Fatalf("gc s1: %d %v", n, err)
	}
	for p, body := range files {
		if b := e.want(200, "GET", "/repository/raw-m/"+p, "ci", ""); b != body {
			t.Fatalf("read %s after the move: %q", p, b)
		}
	}
	e.want(400, "POST", "/-/api/repositories/raw-m/blobstore", "admin", `{"blobStore":"s2"}`)
	evs := e.json(200, "GET", "/-/api/events?kind=repo.blobstore", "admin", "")["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("events: %v", evs)
	}
}

// TestChangeBlobStoreResumes stops a move midway: the repository keeps
// reading from both stores and writing to the target, and the next run
// finishes it.
func TestChangeBlobStoreResumes(t *testing.T) {
	e := newStagingEnv(t)
	e.want(201, "POST", "/-/api/blobstores", "admin", `{"name":"s2"}`)
	for _, p := range []string{"a", "b", "c"} {
		e.want(201, "PUT", "/repository/raw-a/"+p, "ci", "body-"+p)
	}
	stopped := make(chan struct{})
	n := 0
	e.a.Tasks.AfterMoveCopy = func(string, string) {
		if n++; n == 1 {
			e.a.Tasks.StopRun(e.a.Tasks.Running())
			close(stopped)
		}
	}
	res := e.json(202, "POST", "/-/api/repositories/raw-a/blobstore", "admin", `{"blobStore":"s2"}`)
	task := res["task"].(string)
	<-stopped
	e.a.Tasks.Wait()
	if m, ok := e.a.Store.MoveOf("raw-a"); !ok || m.State != "failed" {
		t.Fatalf("stopped move: %+v %v", m, ok)
	}
	for _, p := range []string{"a", "b", "c"} {
		if b := e.want(200, "GET", "/repository/raw-a/"+p, "ci", ""); b != "body-"+p {
			t.Fatalf("read %s: %q", p, b)
		}
	}
	e.want(400, "POST", "/-/api/repositories/raw-a/blobstore", "admin", `{"blobStore":"default"}`) // another target
	e.a.Tasks.AfterMoveCopy = nil
	e.want(202, "POST", "/-/api/repositories/raw-a/blobstore", "admin", `{"blobStore":"s2"}`)
	e.a.Tasks.Wait()
	if got := e.json(200, "GET", "/-/api/tasks/"+task, "admin", ""); got["lastResult"] != "OK" {
		t.Fatalf("resumed: %v", got)
	}
	if _, ok := e.a.Store.MoveOf("raw-a"); ok || e.a.Registry.Get("raw-a").BlobStore != "s2" {
		t.Fatal("the move did not finish")
	}
}
