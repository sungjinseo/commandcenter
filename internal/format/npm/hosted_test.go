package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
)

// ci may do everything; access_test.go covers finer privileges.
var ci = auth.Principal{User: "ci", Admin: true}

type tarFile struct{ name, body string }

// tarGz builds a gzip+tar archive of regular files. The output depends only
// on the files, so tests can rebuild the bytes they expect.
func tarGz(files ...tarFile) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// testTarball is what `npm pack` makes for name@version, with content as
// package/index.js (different content, different tarball).
func testTarball(name, version string, content []byte) []byte {
	pj, _ := json.Marshal(map[string]string{"name": name, "version": version})
	return tarGz(tarFile{"package/package.json", string(pj)}, tarFile{"package/index.js", string(content)})
}

// publishBody builds what `npm publish` sends for testTarball(name, version, content).
func publishBody(name, version string, content []byte, tamper bool) string {
	return publishTgz(name, version, testTarball(name, version, content), tamper)
}

// publishTgz builds what `npm publish` sends for the tarball tgz. npm names
// the attachment "<full name>-<version>.tgz", scope included.
func publishTgz(name, version string, tgz []byte, tamper bool) string {
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
	if w := e.do("GET", "npm-all", "@s/hello/-/hello-1.0.0.tgz", "", anon); w.Code != 200 || w.Body.String() != string(testTarball("@s/hello", "1.0.0", []byte("tgz-1"))) {
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

// QA L-15: the server reads package.json from the stored tarball, checks the
// manifest name, and refuses new names npm would refuse.
func TestPublishChecksTarballAndName(t *testing.T) {
	e := newEnv(t)
	pj := func(name, version string) tarFile {
		b, _ := json.Marshal(map[string]string{"name": name, "version": version})
		return tarFile{"package/package.json", string(b)}
	}
	for what, tgz := range map[string][]byte{
		"empty":            nil,
		"not gzip":         []byte("x"),
		"no package.json":  tarGz(tarFile{"package/index.js", "x"}),
		"name mismatch":    tarGz(pj("other", "1.0.0")),
		"version mismatch": tarGz(pj("pkg", "2.0.0")),
	} {
		w := e.do("PUT", "npm-hosted", "pkg", publishTgz("pkg", "1.0.0", tgz, false), ci)
		if msg, _ := jsonBody(t, w)["error"].(string); w.Code != 400 || !strings.HasPrefix(msg, "invalid tarball for pkg@1.0.0: ") {
			t.Errorf("%s: %d %s", what, w.Code, w.Body.String())
		}
	}
	var doc map[string]any
	json.Unmarshal([]byte(publishBody("pkg", "1.0.0", []byte("x"), false)), &doc)
	get(doc, "versions", "1.0.0").(map[string]any)["name"] = "other"
	b, _ := json.Marshal(doc)
	if w := e.do("PUT", "npm-hosted", "pkg", string(b), ci); w.Code != 400 || !strings.Contains(w.Body.String(), "manifest name") {
		t.Errorf("versions[v].name mismatch: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{"pkg", "pkg/-/pkg-1.0.0.tgz"} {
		if w := e.do("GET", "npm-hosted", p, "", anon); w.Code != 404 {
			t.Fatalf("rejected publishes left %s: %d", p, w.Code)
		}
	}
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("valid publish after rejects: %d %s", w.Code, w.Body.String())
	}

	// New names: upper case and the reserved names here, the rest by the route.
	for _, name := range []string{"Upper", "@S/pkg", "@s/Pkg", "node_modules", "favicon.ico", ".dot", "_under", "a b", strings.Repeat("a", 215)} {
		if w := e.do("PUT", "npm-hosted", name, publishBody(name, "1.0.0", []byte("x"), false), ci); w.Code != 400 {
			t.Errorf("new name %q: %d %s", name, w.Code, w.Body.String())
		}
	}
	// validate-npm-package-name reserves only the whole names.
	if w := e.do("PUT", "npm-hosted", "@s/node_modules", publishBody("@s/node_modules", "1.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Errorf("@s/node_modules: %d %s", w.Code, w.Body.String())
	}

	// A package an older build stored under a legacy name keeps publishing,
	// and its stored manifest is not re-checked on deprecate.
	ctx := context.Background()
	rp := e.repos["npm-hosted"]
	err := e.h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		attrs := `{"name":"legacy-upper","version":"1.0.0","dist":{"tarball":"x"}}`
		if err := tx.InsertPackage(ctx, meta.Package{Repo: rp.Name, Format: "npm", Name: "Legacy-Upper", Version: "1.0.0", Attrs: attrs, CreatedAt: time.Now()}); err != nil {
			return err
		}
		return e.h.writePackument(ctx, tx, rp, "Legacy-Upper")
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do("PUT", "npm-hosted", "Legacy-Upper", publishBody("Legacy-Upper", "1.1.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("new version of a legacy name: %d %s", w.Code, w.Body.String())
	}
	doc = jsonBody(t, e.do("GET", "npm-hosted", "Legacy-Upper", "", anon))
	get(doc, "versions", "1.0.0").(map[string]any)["deprecated"] = "old"
	b, _ = json.Marshal(doc)
	if w := e.do("PUT", "npm-hosted", "Legacy-Upper", string(b), ci); w.Code != 201 {
		t.Fatalf("deprecate a legacy row: %d %s", w.Code, w.Body.String())
	}
	// Reads keep accepting such names (the proxy upstream has no such package).
	if w := e.do("GET", "npm-proxy", "Legacy-Upper", "", anon); w.Code != 404 {
		t.Fatalf("proxy read of a legacy name: %d %s", w.Code, w.Body.String())
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
	tgz := testTarball("pkg", "2.0.0", []byte("dist"))
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
