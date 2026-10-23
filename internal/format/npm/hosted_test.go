package npm

import (
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"commandcenter/internal/auth"
)

// ci may do everything; access_test.go covers finer privileges.
var ci = auth.Principal{User: "ci", Admin: true}

// publishBody builds what `npm publish` sends. npm names the attachment
// "<full name>-<version>.tgz", scope included.
func publishBody(name, version string, tgz []byte, tamper bool) string {
	s1 := sha1.Sum(tgz)
	s512 := sha512.Sum512(tgz)
	shasum := hex.EncodeToString(s1[:])
	if tamper {
		shasum = "0000"
	}
	manifest := map[string]any{
		"name": name, "version": version, "description": "test",
		"dist": map[string]any{
			"shasum":    shasum,
			"integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
			"tarball":   "http://localhost/" + name + "/-/" + name + "-" + version + ".tgz",
		},
	}
	doc := map[string]any{
		"_id": name, "name": name,
		"dist-tags": map[string]string{"latest": version},
		"versions":  map[string]any{version: manifest},
		"_attachments": map[string]any{
			name + "-" + version + ".tgz": map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tgz),
				"length":       len(tgz),
			},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func TestPublishAndInstall(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-all", "@s/hello", publishBody("@s/hello", "1.0.0", []byte("tgz-1"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	m := jsonBody(t, e.do("GET", "npm-hosted", "@s/hello", "", anon))
	if get(m, "dist-tags", "latest") != "1.0.0" || get(m, "versions", "1.0.0", "description") != "test" {
		t.Fatalf("packument = %v", m)
	}
	if get(m, "versions", "1.0.0", "dist", "tarball") != base+"/repository/npm-hosted/@s/hello/-/hello-1.0.0.tgz" {
		t.Fatalf("tarball = %v", get(m, "versions", "1.0.0", "dist", "tarball"))
	}
	if get(m, "time", "1.0.0") == nil || get(m, "time", "modified") == nil {
		t.Fatalf("time = %v", get(m, "time"))
	}
	if w := e.do("GET", "npm-all", "@s/hello/-/hello-1.0.0.tgz", "", anon); w.Code != 200 || w.Body.String() != "tgz-1" {
		t.Fatalf("tarball via group: %d %q", w.Code, w.Body.String())
	}

	if w := e.do("PUT", "npm-hosted", "@s/hello", publishBody("@s/hello", "1.0.0", []byte("other"), false), ci); w.Code != 403 {
		t.Fatalf("republish: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "npm-hosted", "@s/hello", publishBody("@s/hello", "1.1.0", []byte("tgz-2"), false), ci); w.Code != 201 {
		t.Fatalf("second version: %d %s", w.Code, w.Body.String())
	}
	m = jsonBody(t, e.do("GET", "npm-hosted", "@s/hello", "", anon))
	if get(m, "dist-tags", "latest") != "1.1.0" || get(m, "versions", "1.0.0") == nil || get(m, "versions", "1.1.0") == nil {
		t.Fatalf("after second publish = %v", m)
	}
}

func TestPublishRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), true), ci); w.Code != 400 {
		t.Fatalf("tampered shasum: %d", w.Code)
	}
	if w := e.do("GET", "npm-hosted", "pkg", "", anon); w.Code != 404 {
		t.Fatalf("failed publish left a package: %d", w.Code)
	}
	if w := e.do("PUT", "npm-hosted", "other", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 400 {
		t.Fatalf("name mismatch: %d", w.Code)
	}
	noAttachment := `{"name":"pkg","versions":{"1.0.0":{"name":"pkg","version":"1.0.0"}}}`
	if w := e.do("PUT", "npm-hosted", "pkg", noAttachment, ci); w.Code != 400 {
		t.Fatalf("missing tarball: %d", w.Code)
	}
	badTag := `{"name":"pkg","dist-tags":{"latest":"9.9.9"},"versions":{}}`
	if w := e.do("PUT", "npm-hosted", "pkg", badTag, ci); w.Code != 400 {
		t.Fatalf("tag to unknown version: %d", w.Code)
	}
	if w := e.do("PUT", "npm-proxy", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 405 {
		t.Fatalf("publish to proxy: %d", w.Code)
	}
}

func TestDeprecateKeepsDist(t *testing.T) {
	e := newEnv(t)
	dep := deployer(t, e)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), dep); w.Code != 201 {
		t.Fatalf("setup publish: %d %s", w.Code, w.Body.String())
	}
	doc := jsonBody(t, e.do("GET", "npm-hosted", "pkg", "", anon))
	v := get(doc, "versions", "1.0.0").(map[string]any)
	v["deprecated"] = "use 2.x"
	v["description"] = "hijacked"
	v["dist"].(map[string]any)["tarball"] = "http://evil/x.tgz"
	b, _ := json.Marshal(doc)
	if w := e.do("PUT", "npm-hosted", "pkg", string(b), dep); w.Code != 201 {
		t.Fatalf("deprecate: %d %s", w.Code, w.Body.String())
	}
	got := jsonBody(t, e.do("GET", "npm-hosted", "pkg/1.0.0", "", anon))
	if got["deprecated"] != "use 2.x" || got["description"] != "test" || get(got, "dist", "tarball") != base+"/repository/npm-hosted/pkg/-/pkg-1.0.0.tgz" {
		t.Fatalf("version doc = %v", got)
	}
}

func TestDistTagWrites(t *testing.T) {
	e := newEnv(t)
	dep := deployer(t, e)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), dep); w.Code != 201 {
		t.Fatalf("setup publish: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "npm-all", "-/package/pkg/dist-tags/beta", `"1.0.0"`, dep); w.Code != 201 {
		t.Fatalf("add tag via group: %d %s", w.Code, w.Body.String())
	}
	tags := jsonBody(t, e.do("GET", "npm-hosted", "-/package/pkg/dist-tags", "", anon))
	if tags["beta"] != "1.0.0" || tags["latest"] != "1.0.0" {
		t.Fatalf("tags = %v", tags)
	}
	if w := e.do("PUT", "npm-hosted", "-/package/pkg/dist-tags/beta", `"9.9.9"`, dep); w.Code != 400 {
		t.Fatalf("unknown version: %d", w.Code)
	}
	if w := e.do("PUT", "npm-hosted", "-/package/nope/dist-tags/beta", `"1.0.0"`, dep); w.Code != 404 {
		t.Fatalf("unknown package: %d", w.Code)
	}
	if w := e.do("DELETE", "npm-hosted", "-/package/pkg/dist-tags/beta", "", dep); w.Code != 200 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := e.do("DELETE", "npm-hosted", "-/package/pkg/dist-tags/latest", "", dep); w.Code != 400 {
		t.Fatalf("delete latest: %d", w.Code)
	}
	if w := e.do("DELETE", "npm-hosted", "-/package/pkg/dist-tags/beta", "", dep); w.Code != 404 {
		t.Fatalf("delete missing: %d", w.Code)
	}
	if tags := jsonBody(t, e.do("GET", "npm-hosted", "-/package/pkg/dist-tags", "", anon)); tags["beta"] != nil {
		t.Fatalf("tags after delete = %v", tags)
	}
}

func TestPublishSetsServerDist(t *testing.T) {
	e := newEnv(t)
	tgz := []byte("tgz-dist")
	doc := map[string]any{
		"name":      "pkg",
		"dist-tags": map[string]string{"latest": "2.0.0"},
		"versions": map[string]any{"2.0.0": map[string]any{
			"name": "pkg", "version": "2.0.0",
			"dist": map[string]any{"tarball": "http://x/pkg/-/pkg-1.0.0.tgz", "fileCount": 3},
		}},
		"_attachments": map[string]any{"pkg-2.0.0.tgz": map[string]any{
			"content_type": "application/octet-stream",
			"data":         base64.StdEncoding.EncodeToString(tgz),
		}},
	}
	b, _ := json.Marshal(doc)
	if w := e.do("PUT", "npm-hosted", "pkg", string(b), ci); w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	got := jsonBody(t, e.do("GET", "npm-hosted", "pkg/2.0.0", "", anon))
	s1 := sha1.Sum(tgz)
	if get(got, "dist", "tarball") != base+"/repository/npm-hosted/pkg/-/pkg-2.0.0.tgz" {
		t.Fatalf("tarball = %v", get(got, "dist", "tarball"))
	}
	if get(got, "dist", "shasum") != hex.EncodeToString(s1[:]) {
		t.Fatalf("shasum = %v", get(got, "dist", "shasum"))
	}
	if s, _ := get(got, "dist", "integrity").(string); !strings.HasPrefix(s, "sha512-") {
		t.Fatalf("integrity = %v", get(got, "dist", "integrity"))
	}
	if get(got, "dist", "fileCount") == nil {
		t.Fatalf("client dist extras dropped: %v", got)
	}
}

func TestPublishRejectsEmptyPackage(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "ghost", `{"name":"ghost","versions":{}}`, ci); w.Code != 400 {
		t.Fatalf("empty publish: %d", w.Code)
	}
	if w := e.do("GET", "npm-hosted", "ghost", "", anon); w.Code != 404 {
		t.Fatalf("ghost package exists: %d", w.Code)
	}
}
