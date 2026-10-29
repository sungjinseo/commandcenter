package transfer_test

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/transfer"
)

var pwHash = sync.OnceValue(func() string {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	return string(h)
})

// srv is a Command Center with its own database and blob directory.
type srv struct {
	t   *testing.T
	a   *app.App
	ts  *httptest.Server
	dir string
}

func newSrv(t *testing.T, root, repos string) *srv {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte("dataDir: " + dir + "\nexportRoot: " + root + "\nusers:\n  - { name: admin, passwordHash: \"" +
		pwHash() + "\", roles: [admin] }\n" + repos))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, dir)
	a, err := app.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Close() })
	return &srv{t: t, a: a, ts: ts, dir: dir}
}

// do sends an admin request and returns the status and body.
func (s *srv) do(method, path string, body []byte, hdr ...string) (int, []byte) {
	s.t.Helper()
	r, err := http.NewRequest(method, s.ts.URL+path, bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	r.SetBasicAuth("admin", "pw")
	r.Header.Set("X-Command-Center-UI", "1")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (s *srv) must(method, path string, body []byte, hdr ...string) []byte {
	s.t.Helper()
	code, b := s.do(method, path, body, hdr...)
	if code/100 != 2 {
		s.t.Fatalf("%s %s: %d %s", method, path, code, b)
	}
	return b
}

type taskState struct {
	ID          string `json:"id"`
	LastResult  string `json:"lastResult"`
	LastMessage string `json:"lastMessage"`
	LastLog     string `json:"lastLog"`
}

// start runs POST /-/api/<kind> and waits for the task.
func (s *srv) start(kind string, params any) taskState {
	s.t.Helper()
	body, _ := json.Marshal(params)
	code, b := s.do("POST", "/-/api/"+kind, body)
	if code != http.StatusAccepted {
		s.t.Fatalf("POST /-/api/%s %s: %d %s", kind, body, code, b)
	}
	var res struct{ ID string }
	json.Unmarshal(b, &res)
	return s.wait(res.ID)
}

func (s *srv) wait(id string) taskState {
	s.t.Helper()
	s.a.Tasks.Wait()
	var st taskState
	json.Unmarshal(s.must("GET", "/-/api/tasks/"+id, nil), &st)
	return st
}

func (s *srv) ok(st taskState) taskState {
	s.t.Helper()
	if st.LastResult != "OK" {
		s.t.Fatalf("task %s: %s: %s\n%s", st.ID, st.LastResult, st.LastMessage, st.LastLog)
	}
	return st
}

// ---- artifacts as the clients send them ----

func tgz(files ...string) []byte { // name, body, name, body...
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

func npmPublish(name, version string) []byte {
	pj, _ := json.Marshal(map[string]string{"name": name, "version": version})
	tarball := tgz("package/package.json", string(pj), "package/index.js", "module.exports = '"+version+"'")
	s1, s512 := sha1.Sum(tarball), sha512.Sum512(tarball)
	doc, _ := json.Marshal(map[string]any{
		"_id": name, "name": name, "dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{"name": name, "version": version, "description": "demo",
			"dist": map[string]any{"shasum": hex.EncodeToString(s1[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
				"tarball": "http://localhost/" + name + "/-/" + name + "-" + version + ".tgz"}}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(tarball), "length": len(tarball)}},
	})
	return doc
}

func pypiUpload(name, version string) ([]byte, string) {
	file := name + "-" + version + ".tar.gz"
	content := tgz(name+"-"+version+"/PKG-INFO", "Metadata-Version: 2.1\nName: "+name+"\nVersion: "+version+"\n")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	sum := sha256.Sum256(content)
	for _, kv := range [][2]string{{":action", "file_upload"}, {"protocol_version", "1"}, {"name", name}, {"version", version},
		{"filetype", "sdist"}, {"sha256_digest", hex.EncodeToString(sum[:])}} {
		mw.WriteField(kv[0], kv[1])
	}
	fw, _ := mw.CreateFormFile("content", file)
	fw.Write(content)
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func helmChart(name, version string) []byte {
	return tgz(name+"/Chart.yaml", "apiVersion: v2\nname: "+name+"\nversion: "+version+"\ndescription: demo\n", name+"/values.yaml", "a: 1\n")
}

func nugetPush(id, version string) ([]byte, string) {
	var pkg bytes.Buffer
	zw := zip.NewWriter(&pkg)
	w, _ := zw.Create(id + ".nuspec")
	w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?><package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata>` +
		`<id>` + id + `</id><version>` + version + `</version><authors>QA</authors><description>demo</description></metadata></package>`))
	w, _ = zw.Create("lib/net8.0/" + id + ".dll")
	w.Write([]byte("MZ"))
	zw.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("package", "package.nupkg")
	fw.Write(pkg.Bytes())
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

// rpm builds a minimal package: lead, signature header, main header.
func rpm(name, version, release, arch string) []byte {
	header := func(entries [][2]any) []byte { // tag, string or int32
		var index, data bytes.Buffer
		for _, e := range entries {
			for data.Len()%4 != 0 {
				data.WriteByte(0)
			}
			off := data.Len()
			typ := int32(6)
			switch v := e[1].(type) {
			case string:
				data.WriteString(v + "\x00")
			case int32:
				typ = 4
				binary.Write(&data, binary.BigEndian, v)
			}
			binary.Write(&index, binary.BigEndian, []int32{e[0].(int32), typ, int32(off), 1})
		}
		var out bytes.Buffer
		out.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})
		binary.Write(&out, binary.BigEndian, []int32{int32(len(entries)), int32(data.Len())})
		out.Write(index.Bytes())
		out.Write(data.Bytes())
		return out.Bytes()
	}
	var out bytes.Buffer
	lead := make([]byte, 96)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0})
	out.Write(lead)
	out.Write(header([][2]any{{int32(1000), int32(1)}}))
	for out.Len()%8 != 0 {
		out.WriteByte(0)
	}
	out.Write(header([][2]any{{int32(1000), name}, {int32(1001), version}, {int32(1002), release}, {int32(1004), name + " summary"},
		{int32(1005), name + " description"}, {int32(1006), int32(1700000001)}, {int32(1009), int32(1)}, {int32(1022), arch}}))
	out.WriteString(name)
	return out.Bytes()
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

const repos = `repositories:
  - { name: maven-releases, format: maven, type: hosted }
  - { name: npm-hosted, format: npm, type: hosted }
  - { name: pypi-hosted, format: pypi, type: hosted }
  - { name: raw-hosted, format: raw, type: hosted, writePolicy: allow }
  - { name: helm-hosted, format: helm, type: hosted }
  - { name: docker-hosted, format: docker, type: hosted }
  - { name: yum-hosted, format: yum, type: hosted }
  - { name: nuget-hosted, format: nuget, type: hosted }
  - { name: maven-public, format: maven, type: group, members: [maven-releases] }
`

// populate uploads one component of each format through its protocol and
// returns the paths whose answers the import must reproduce.
func populate(s *srv) []string {
	s.must("PUT", "/repository/maven-releases/org/example/app/1.0/app-1.0.jar", []byte("jar bytes"))
	s.must("PUT", "/repository/maven-releases/org/example/app/1.0/app-1.0.pom",
		[]byte(`<project><modelVersion>4.0.0</modelVersion><groupId>org.example</groupId><artifactId>app</artifactId><version>1.0</version></project>`))
	s.must("PUT", "/repository/npm-hosted/demo", npmPublish("demo", "1.0.0"), "Content-Type", "application/json")
	body, ct := pypiUpload("demo", "1.0")
	s.must("POST", "/repository/pypi-hosted/", body, "Content-Type", ct)
	s.must("PUT", "/repository/raw-hosted/docs/readme.txt", []byte("readme v1"))
	s.must("PUT", "/repository/helm-hosted/demo-0.1.0.tgz", helmChart("demo", "0.1.0"))
	s.must("PUT", "/repository/yum-hosted/Packages/d/demo-1.0-1.el9.x86_64.rpm", rpm("demo", "1.0", "1.el9", "x86_64"))
	body, ct = nugetPush("Demo", "1.0.0")
	s.must("PUT", "/repository/nuget-hosted/api/v2/package", body, "Content-Type", ct)

	config, layer := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`), tgz("hello", "world")
	for _, b := range [][]byte{config, layer} {
		s.must("POST", "/v2/docker-hosted/app/blobs/uploads/?digest="+digest(b), b, "Content-Type", "application/octet-stream")
	}
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "size": len(config), "digest": digest(config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "size": len(layer), "digest": digest(layer)}}})
	s.must("PUT", "/v2/docker-hosted/app/manifests/v1", manifest, "Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{"mediaType": "application/vnd.docker.distribution.manifest.v2+json", "size": len(manifest),
			"digest": digest(manifest), "platform": map[string]string{"architecture": "amd64", "os": "linux"}}}})
	s.must("PUT", "/v2/docker-hosted/app/manifests/multi", index, "Content-Type", "application/vnd.oci.image.index.v1+json")

	return []string{
		"/repository/maven-releases/org/example/app/1.0/app-1.0.jar",
		"/repository/maven-releases/org/example/app/1.0/app-1.0.pom",
		"/repository/maven-releases/org/example/app/maven-metadata.xml",
		"/repository/npm-hosted/demo",
		"/repository/npm-hosted/demo/-/demo-1.0.0.tgz",
		"/repository/pypi-hosted/simple/demo/",
		"/repository/pypi-hosted/packages/demo/demo-1.0.tar.gz",
		"/repository/raw-hosted/docs/readme.txt",
		"/repository/helm-hosted/index.yaml",
		"/repository/helm-hosted/demo-0.1.0.tgz",
		"/v2/docker-hosted/app/manifests/v1",
		"/v2/docker-hosted/app/manifests/multi",
		"/v2/docker-hosted/app/manifests/" + digest(manifest),
		"/v2/docker-hosted/app/blobs/" + digest(layer),
		"/v2/docker-hosted/app/blobs/" + digest(config),
		"/v2/docker-hosted/app/tags/list",
		"/repository/yum-hosted/repodata/repomd.xml",
		"/repository/yum-hosted/Packages/d/demo-1.0-1.el9.x86_64.rpm",
		"/repository/nuget-hosted/registration/demo/index.json",
		"/repository/nuget-hosted/v3-flatcontainer/demo/1.0.0/demo.1.0.0.nupkg",
	}
}

var accept = []string{"Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json"}

// same checks that b answers every path as a does, byte for byte once a's
// own URL is replaced with b's.
func same(t *testing.T, a, b *srv, paths []string) {
	t.Helper()
	for _, p := range paths {
		ca, ba := a.do("GET", p, nil, accept...)
		cb, bb := b.do("GET", p, nil, accept...)
		want := bytes.ReplaceAll(ba, []byte(a.ts.URL), []byte(b.ts.URL))
		if ca != 200 || cb != 200 || !bytes.Equal(want, bb) {
			t.Errorf("GET %s: source %d, import %d\nsource: %.400s\nimport: %.400s", p, ca, cb, want, bb)
		}
	}
}

func assetInfo(s *srv, repoName, path string) map[string]any {
	var m map[string]any
	json.Unmarshal(s.must("GET", "/-/api/repositories/"+repoName+"/asset?path="+path, nil), &m)
	return m
}

func readLines(t *testing.T, file string) []transfer.Line {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []transfer.Line
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l transfer.Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func readManifest(t *testing.T, dir string) transfer.Manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m transfer.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func lineFor(t *testing.T, lines []transfer.Line, path string) transfer.Line {
	t.Helper()
	for _, l := range lines {
		if l.Path == path {
			return l
		}
	}
	t.Fatalf("no line for %s", path)
	return transfer.Line{}
}

func storeFile(dataDir, sha string) string {
	return filepath.Join(dataDir, "blobs", "sha256", sha[:2], sha[2:4], sha)
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(fa, fb)
}

func TestExportImportRoundTrip(t *testing.T) {
	root := t.TempDir()
	a := newSrv(t, root, repos)
	paths := populate(a)

	full := a.ok(a.start("export", map[string]any{"repositories": "*", "destination": "full"}))
	dir := filepath.Join(root, "full", full.ID)
	m := readManifest(t, dir)
	if m.FormatVersion != 1 || len(m.Repositories) != 8 || m.ID != full.ID {
		t.Fatalf("manifest: %+v", m)
	}
	for _, e := range m.Repositories {
		if e.Assets == 0 || e.AssetListSHA256 == "" {
			t.Errorf("manifest entry %+v", e)
		}
	}
	raw := readLines(t, filepath.Join(dir, "raw-hosted", "assets.jsonl"))
	readme := lineFor(t, raw, "docs/readme.txt")
	if !sameFile(t, filepath.Join(dir, "raw-hosted", "blobs", readme.SHA256[:2], readme.SHA256), storeFile(a.dir, readme.SHA256)) {
		t.Error("the blob of a file store on the destination's filesystem is not hard-linked")
	}
	npm := lineFor(t, readLines(t, filepath.Join(dir, "npm-hosted", "assets.jsonl")), "demo/-/demo-1.0.0.tgz")
	if len(npm.Packages) != 1 || npm.Packages[0].Version != "1.0.0" || len(npm.Packages[0].Tags) != 1 || npm.Packages[0].Tags[0] != "latest" {
		t.Errorf("npm tarball line: %+v", npm.Packages)
	}

	// A re-run skips the blobs already there and rewrites the lists.
	if st := a.ok(a.wait(runTask(a, full.ID))); !strings.Contains(st.LastLog, "already there") || !strings.Contains(st.LastLog, "0 copied") {
		t.Errorf("re-run log:\n%s", st.LastLog)
	}

	// A fresh server (another database and blob directory) answers alike.
	b := newSrv(t, root, "")
	imp := b.ok(b.start("import", map[string]any{"source": "full/" + full.ID, "createMissing": true}))
	same(t, a, b, paths)
	for _, c := range [][2]string{{"maven-releases", "org/example/app/1.0/app-1.0.jar"}, {"npm-hosted", "demo/-/demo-1.0.0.tgz"},
		{"raw-hosted", "docs/readme.txt"}, {"helm-hosted", "index.yaml"}, {"docker-hosted", strings.TrimPrefix(paths[12], "/v2/docker-hosted/")}} {
		ia, ib := assetInfo(a, c[0], c[1]), assetInfo(b, c[0], c[1])
		if ia["created"] != ib["created"] || ia["lastModified"] != ib["lastModified"] || ia["sha256"] != ib["sha256"] {
			t.Errorf("%s %s: source %v, import %v", c[0], c[1], ia, ib)
		}
	}
	if !strings.Contains(imp.LastLog, "created docker hosted repository") {
		t.Errorf("import log:\n%s", imp.LastLog)
	}
	// Importing the same export again changes nothing.
	again := b.ok(b.start("import", map[string]any{"source": "full/" + full.ID}))
	if !strings.Contains(again.LastMessage, "imported 0 files") {
		t.Errorf("second import: %s", again.LastMessage)
	}

	t.Run("delta", func(t *testing.T) {
		since := time.Now().UTC()
		time.Sleep(5 * time.Millisecond)
		a.must("PUT", "/repository/raw-hosted/docs/new.txt", []byte("new file"))
		a.must("PUT", "/repository/helm-hosted/demo-0.2.0.tgz", helmChart("demo", "0.2.0"))
		a.must("PUT", "/repository/npm-hosted/demo", npmPublish("demo", "1.1.0"), "Content-Type", "application/json")
		delta := a.ok(a.start("export", map[string]any{"repositories": []string{"raw-hosted", "helm-hosted", "npm-hosted"},
			"destination": "delta", "since": since.Format(time.RFC3339Nano)}))
		ddir := filepath.Join(root, "delta", delta.ID)
		if m := readManifest(t, ddir); m.Since.IsZero() || len(m.Repositories) != 3 {
			t.Fatalf("delta manifest: %+v", m)
		}
		if lines := readLines(t, filepath.Join(ddir, "raw-hosted", "assets.jsonl")); len(lines) != 1 || lines[0].Path != "docs/new.txt" {
			t.Fatalf("delta raw lines: %+v", lines)
		}
		st := b.ok(b.start("import", map[string]any{"source": "delta/" + delta.ID}))
		if got := b.must("GET", "/repository/raw-hosted/docs/new.txt", nil); string(got) != "new file" {
			t.Errorf("new.txt: %q", got)
		}
		// The index documents are regenerated from the rows of both imports.
		index := string(b.must("GET", "/repository/helm-hosted/index.yaml", nil))
		if !strings.Contains(index, "version: 0.1.0") || !strings.Contains(index, "version: 0.2.0") {
			t.Errorf("helm index after the delta:\n%s", index)
		}
		var doc struct {
			DistTags map[string]string `json:"dist-tags"`
			Versions map[string]any    `json:"versions"`
		}
		json.Unmarshal(b.must("GET", "/repository/npm-hosted/demo", nil), &doc)
		// overwrite off: the tag already here is kept, and reported.
		if len(doc.Versions) != 2 || doc.DistTags["latest"] != "1.0.0" || !strings.Contains(st.LastLog, "tag latest of demo") {
			t.Errorf("packument after the delta: %+v\n%s", doc, st.LastLog)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		a.must("PUT", "/repository/raw-hosted/docs/readme.txt", []byte("readme v2"))
		ow := a.ok(a.start("export", map[string]any{"repositories": []string{"raw-hosted"}, "destination": "ow"}))
		st := b.ok(b.start("import", map[string]any{"source": "ow/" + ow.ID}))
		if got := b.must("GET", "/repository/raw-hosted/docs/readme.txt", nil); string(got) != "readme v1" || !strings.Contains(st.LastLog, "docs/readme.txt kept") {
			t.Errorf("overwrite off: %q\n%s", got, st.LastLog)
		}
		b.ok(b.start("import", map[string]any{"source": "ow/" + ow.ID, "overwrite": true}))
		if got := b.must("GET", "/repository/raw-hosted/docs/readme.txt", nil); string(got) != "readme v2" {
			t.Errorf("overwrite on: %q", got)
		}
	})

	t.Run("copy and corrupt blob", func(t *testing.T) {
		cp := a.ok(a.start("export", map[string]any{"repositories": []string{"raw-hosted", "maven-releases"}, "destination": "copy", "hardLinks": false}))
		cdir := filepath.Join(root, "copy", cp.ID)
		lines := readLines(t, filepath.Join(cdir, "raw-hosted", "assets.jsonl"))
		readme := lineFor(t, lines, "docs/readme.txt")
		blobFile := filepath.Join(cdir, "raw-hosted", "blobs", readme.SHA256[:2], readme.SHA256)
		if sameFile(t, blobFile, storeFile(a.dir, readme.SHA256)) {
			t.Fatal("hardLinks false still hard-linked")
		}
		if err := os.WriteFile(blobFile, []byte("XXXXXX v2"), 0o644); err != nil {
			t.Fatal(err)
		}
		st := b.wait(startRaw(t, b, "import", map[string]any{"source": "copy/" + cp.ID, "createMissing": true,
			"repositories": map[string]string{"raw-hosted": "raw-corrupt", "maven-releases": "maven-copy"}}))
		if st.LastResult != "Failed" || !strings.Contains(st.LastMessage, "1 components were skipped") || !strings.Contains(st.LastLog, "corrupt") {
			t.Fatalf("corrupt import: %s %s\n%s", st.LastResult, st.LastMessage, st.LastLog)
		}
		if code, _ := b.do("GET", "/repository/raw-corrupt/docs/readme.txt", nil); code != 404 {
			t.Errorf("the corrupt file was imported: %d", code)
		}
		if got := b.must("GET", "/repository/raw-corrupt/docs/new.txt", nil); string(got) != "new file" {
			t.Errorf("the other file: %q", got)
		}
		if got, want := b.must("GET", "/repository/maven-copy/org/example/app/maven-metadata.xml", nil),
			a.must("GET", "/repository/maven-releases/org/example/app/maven-metadata.xml", nil); !bytes.Equal(got, want) {
			t.Errorf("maven-metadata.xml of the renamed copy:\n%s\n%s", got, want)
		}
	})
}

// runTask runs a stored task again (POST /-/api/tasks/{id}/run).
func runTask(s *srv, id string) string {
	s.t.Helper()
	s.must("POST", "/-/api/tasks/"+id+"/run", nil)
	return id
}

// startRaw starts a run like start, without waiting for it.
func startRaw(t *testing.T, s *srv, kind string, params any) string {
	t.Helper()
	body, _ := json.Marshal(params)
	code, b := s.do("POST", "/-/api/"+kind, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST /-/api/%s: %d %s", kind, code, b)
	}
	var res struct{ ID string }
	json.Unmarshal(b, &res)
	return res.ID
}

func TestExportRootEscapes(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	a := newSrv(t, root, "repositories:\n  - { name: raw-hosted, format: raw, type: hosted }\n")
	a.must("PUT", "/repository/raw-hosted/f.txt", []byte("x"))
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ kind, field, dir string }{
		{"export", "destination", "../x"}, {"export", "destination", outside}, {"export", "destination", "a/../../x"},
		{"export", "destination", "link"}, {"export", "destination", "link/sub"}, {"export", "destination", ""},
		{"import", "source", "../x"}, {"import", "source", "link"}, {"import", "source", "/"},
	} {
		body, _ := json.Marshal(map[string]any{c.field: c.dir})
		if code, b := a.do("POST", "/-/api/"+c.kind, body); code != http.StatusBadRequest {
			t.Errorf("%s %s %q: %d %s", c.kind, c.field, c.dir, code, b)
		}
	}
	for _, params := range []string{`{"destination":"d","repositories":"x"}`, `{"destination":"d","repositories":["nope"]}`,
		`{"destination":"d","since":"yesterday"}`, `{"destination":"d","unknown":1}`, `{"source":"s","repositories":{"raw-hosted":"bad name"}}`} {
		kind := "export"
		if strings.Contains(params, "source") {
			kind = "import"
		}
		if code, b := a.do("POST", "/-/api/"+kind, []byte(params)); code != http.StatusBadRequest {
			t.Errorf("%s %s: %d %s", kind, params, code, b)
		}
	}
	// A symbolic link made after the task was saved is refused when it runs.
	body, _ := json.Marshal(map[string]any{"name": "later", "type": "repository.export", "schedule": "manual",
		"params": map[string]any{"destination": "later"}})
	var task taskState
	json.Unmarshal(a.must("POST", "/-/api/tasks", body), &task)
	if err := os.Symlink(outside, filepath.Join(root, "later")); err != nil {
		t.Fatal(err)
	}
	if st := a.wait(runTask(a, task.ID)); st.LastResult != "Failed" {
		t.Errorf("export through a later symlink: %+v", st)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("written outside the export root: %v", entries)
	}
	// Without an export root the tasks are refused.
	b := newSrv(t, "", "")
	if code, msg := b.do("POST", "/-/api/export", []byte(`{"destination":"x"}`)); code != http.StatusBadRequest || !strings.Contains(string(msg), "exportRoot") {
		t.Errorf("no export root: %d %s", code, msg)
	}
}

func TestExportStoppedLeavesNoManifest(t *testing.T) {
	root := t.TempDir()
	a := newSrv(t, root, "repositories:\n  - { name: raw-hosted, format: raw, type: hosted }\n")
	for _, p := range []string{"a", "b", "c"} {
		a.must("PUT", "/repository/raw-hosted/"+p, []byte(p))
	}
	body, _ := json.Marshal(map[string]any{"name": "stop me", "type": "repository.export", "schedule": "manual",
		"params": map[string]any{"destination": "stopped"}})
	var task taskState
	json.Unmarshal(a.must("POST", "/-/api/tasks", body), &task)
	defer transfer.SetAfterAsset(func(string) { a.a.Tasks.StopRun(task.ID) })()
	if st := a.wait(runTask(a, task.ID)); st.LastResult != "Stopped" {
		t.Fatalf("stopped export: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(root, "stopped", task.ID, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("a stopped export has a manifest: %v", err)
	}
	b := newSrv(t, root, "")
	if code, msg := b.do("POST", "/-/api/import", []byte(`{"source":"stopped/`+task.ID+`"}`)); code != http.StatusAccepted {
		t.Fatalf("import: %d %s", code, msg)
	} else {
		var res struct{ ID string }
		json.Unmarshal(msg, &res)
		if st := b.wait(res.ID); st.LastResult != "Failed" || !strings.Contains(st.LastMessage, "no manifest.json") {
			t.Errorf("import of an unfinished export: %+v", st)
		}
	}
}

// events polls the activity list until it holds every kind.
func events(t *testing.T, s *srv, kinds ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		body := string(s.must("GET", "/-/api/events?limit=200", nil))
		missing := ""
		for _, k := range kinds {
			if !strings.Contains(body, `"`+k+`"`) {
				missing = k
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event: %s", missing, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An index listed before its manifests waits for them.
func TestImportDockerIndexBeforeItsManifest(t *testing.T) {
	root := t.TempDir()
	a := newSrv(t, root, repos)
	paths := populate(a)
	exp := a.ok(a.start("export", map[string]any{"repositories": []string{"docker-hosted"}, "destination": "d"}))
	dir := filepath.Join(root, "d", exp.ID)
	lines := readLines(t, filepath.Join(dir, "docker-hosted", "assets.jsonl"))
	var index, rest []transfer.Line
	for _, l := range lines {
		if len(l.Packages) > 0 && strings.Contains(l.Packages[0].Attrs, `"manifests"`) {
			index = append(index, l)
		} else {
			rest = append(rest, l)
		}
	}
	if len(index) != 1 {
		t.Fatalf("lines: %+v", lines)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range append(index, rest...) {
		enc.Encode(l)
	}
	os.WriteFile(filepath.Join(dir, "docker-hosted", "assets.jsonl"), buf.Bytes(), 0o644)
	m := readManifest(t, dir)
	sum := sha256.Sum256(buf.Bytes())
	m.Repositories[0].AssetListSHA256 = hex.EncodeToString(sum[:])
	mb, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)

	b := newSrv(t, root, "")
	if st := b.ok(b.start("import", map[string]any{"source": "d/" + exp.ID, "createMissing": true})); !strings.Contains(st.LastLog, "importing 1 manifest lists after") {
		t.Errorf("import log:\n%s", st.LastLog)
	}
	var docker []string
	for _, p := range paths {
		if strings.HasPrefix(p, "/v2/") {
			docker = append(docker, p)
		}
	}
	same(t, a, b, docker)
	events(t, b, "task.create", "repo.create", "repo.import")
}
