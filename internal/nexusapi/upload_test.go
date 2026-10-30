package nexusapi_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// startUploads runs the app with a hosted repository of every format the
// component upload takes: admin, ci (write on all of them) and viewer (no
// role; authenticatedRead lets it read), all with password "pw".
func startUploads(t *testing.T) *env {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	hosted := []string{"maven-releases", "maven-snapshots", "maven-mixed", "raw-files", "raw-offline", "apt-hosted", "npm-hosted",
		"pypi-hosted", "nuget-hosted", "gems", "yum-hosted", "helm-hosted", "r-hosted", "conda-hosted", "composer-hosted", "tf-hosted", "go-hosted"}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
dataDir: %s
baseURL: http://nexus.example.com
users:
  - { name: admin, passwordHash: %q, roles: [admin] }
  - { name: ci, passwordHash: %q, roles: [deployer] }
  - { name: viewer, passwordHash: %q, roles: [] }
roles:
  deployer: { write: [%s] }
repositories:
  - { name: maven-releases, format: maven, type: hosted, writePolicy: allow_once }
  - { name: maven-snapshots, format: maven, type: hosted, versionPolicy: snapshot, writePolicy: allow }
  - { name: maven-mixed, format: maven, type: hosted, versionPolicy: mixed, writePolicy: allow }
  - { name: raw-files, format: raw, type: hosted, writePolicy: allow_once }
  - { name: raw-offline, format: raw, type: hosted, online: false }
  - { name: raw-group, format: raw, type: group, members: [raw-files], writeMember: raw-files }
  - { name: raw-proxy, format: raw, type: proxy, remote: "https://example.invalid/" }
  - { name: apt-hosted, format: apt, type: hosted }
  - { name: npm-hosted, format: npm, type: hosted }
  - { name: pypi-hosted, format: pypi, type: hosted }
  - { name: nuget-hosted, format: nuget, type: hosted }
  - { name: gems, format: rubygems, type: hosted }
  - { name: yum-hosted, format: yum, type: hosted, depth: 1 }
  - { name: helm-hosted, format: helm, type: hosted }
  - { name: r-hosted, format: r, type: hosted }
  - { name: conda-hosted, format: conda, type: hosted }
  - { name: composer-hosted, format: composer, type: hosted }
  - { name: tf-hosted, format: terraform, type: hosted }
  - { name: go-hosted, format: go, type: hosted }
`, t.TempDir(), string(h), string(h), string(h), strings.Join(hosted, ", "))))
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

// part is one part of a form, as curl -F sends it: a text field (no
// Content-Type) or a file (filename and application/octet-stream).
type part struct {
	name, value string // a text field when file is false
	file        bool
	filename    string
	data        []byte
	hdr         textproto.MIMEHeader // overrides, e.g. a text part's Content-Type
}

func fld(name, value string) part { return part{name: name, value: value} }
func fil(name, filename string, data []byte) part {
	return part{name: name, file: true, filename: filename, data: data}
}

func form(parts ...part) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		if p.file {
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.name, p.filename))
			h.Set("Content-Type", "application/octet-stream")
		} else {
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q`, p.name))
		}
		for k, v := range p.hdr {
			h[k] = v
		}
		w, _ := mw.CreatePart(h)
		if p.file {
			w.Write(p.data)
		} else {
			io.WriteString(w, p.value)
		}
	}
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

// send makes a request as user ("" anonymous; "name:password", else
// password "pw") and returns status, body and headers.
func (e *env) send(method, path, user, ctype string, body []byte) (int, []byte, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, bytes.NewReader(body))
	if user != "" {
		name, pass, ok := strings.Cut(user, ":")
		if !ok {
			pass = "pw"
		}
		req.SetBasicAuth(name, pass)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// upload posts the form to the component upload API of repository.
func (e *env) uploadForm(user, repository string, parts ...part) (int, []byte) {
	e.t.Helper()
	body, ct := form(parts...)
	st, b, _ := e.send("POST", v1+"/components?repository="+repository, user, ct, body)
	return st, b
}

func (e *env) wantUpload(status int, user, repository string, parts ...part) []byte {
	e.t.Helper()
	st, b := e.uploadForm(user, repository, parts...)
	if st != status {
		e.t.Fatalf("upload to %s as %q: %d %s, want %d", repository, user, st, b, status)
	}
	return b
}

// wantInvalid wants a 400 validation list with an entry whose message holds msg (and id, unless "").
func (e *env) wantInvalid(id, msg, user, repository string, parts ...part) {
	e.t.Helper()
	b := e.wantUpload(400, user, repository, parts...)
	var errs []struct{ ID, Message string }
	if err := json.Unmarshal(b, &errs); err != nil {
		e.t.Fatalf("upload to %s: not a validation list: %s", repository, b)
	}
	for _, x := range errs {
		if strings.Contains(x.Message, msg) && (id == "" || x.ID == id) {
			return
		}
	}
	e.t.Fatalf("upload to %s: no entry (%q, %q) in %s", repository, id, msg, b)
}

func (e *env) body(path string) string {
	e.t.Helper()
	st, b, _ := e.send("GET", path, "ci", "", nil)
	if st != 200 {
		e.t.Fatalf("GET %s: %d %s", path, st, b)
	}
	return string(b)
}

func (e *env) missing(path string) {
	e.t.Helper()
	if st, b, _ := e.send("GET", path, "ci", "", nil); st != 404 {
		e.t.Fatalf("GET %s: %d %s, want 404", path, st, b)
	}
}

// ---- fixtures ----

func tgzOf(files ...string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i+1 < len(files); i += 2 {
		tw.WriteHeader(&tar.Header{Name: files[i], Mode: 0o644, Size: int64(len(files[i+1])), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)})
		io.WriteString(tw, files[i+1])
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func tarOf(files ...string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := 0; i+1 < len(files); i += 2 {
		tw.WriteHeader(&tar.Header{Name: files[i], Mode: 0o644, Size: int64(len(files[i+1])), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)})
		io.WriteString(tw, files[i+1])
	}
	tw.Close()
	return buf.Bytes()
}

func gz(s string) string {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	io.WriteString(w, s)
	w.Close()
	return buf.String()
}

func zipOf(files ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i+1 < len(files); i += 2 {
		w, _ := zw.Create(files[i])
		io.WriteString(w, files[i+1])
	}
	zw.Close()
	return buf.Bytes()
}

// debOf is a .deb of package name version 1.0 for amd64, as dpkg-deb builds one.
func debOf(name string) []byte {
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	member := func(n string, data []byte) {
		fmt.Fprintf(&buf, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", n, 0, 0, 0, "100644", len(data))
		buf.Write(data)
		if len(data)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	ctl := "Package: " + name + "\nVersion: 1.0\nArchitecture: amd64\nMaintainer: QA <qa@example.org>\nDescription: a test\n"
	member("control.tar.gz", tgzOf("./control", ctl))
	member("data.tar.gz", tgzOf("./usr/bin/"+name, "#!/bin/sh\n"))
	return buf.Bytes()
}

// rpmOf is a minimal package name-ver-rel.arch (lead, signature and main
// headers, payload) as the yum format parses one.
func rpmOf(name, ver, rel, arch string) []byte {
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

func gemOf(name, version string) []byte {
	spec := "--- !ruby/object:Gem::Specification\nname: " + name + "\nversion: !ruby/object:Gem::Version\n  version: " + version +
		"\nplatform: ruby\nauthors:\n- QA\ndependencies: []\nfiles:\n- lib/" + name + ".rb\nrequire_paths:\n- lib\n" +
		"required_ruby_version: !ruby/object:Gem::Requirement\n  requirements:\n  - - \">=\"\n    - !ruby/object:Gem::Version\n      version: '0'\n" +
		"required_rubygems_version: !ruby/object:Gem::Requirement\n  requirements:\n  - - \">=\"\n    - !ruby/object:Gem::Version\n      version: '0'\n" +
		"summary: a test\nspecification_version: 4\n"
	return tarOf("metadata.gz", gz(spec), "data.tar.gz", gz(string(tarOf("lib/"+name+".rb", "puts 1\n"))),
		"checksums.yaml.gz", gz("---\nSHA256:\n  metadata.gz: x\n"))
}

// zstdRaw is b as a zstd frame of raw blocks.
func zstdRaw(b []byte) []byte {
	out := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38}
	for {
		n := min(len(b), 128<<10)
		h := n << 3
		if n == len(b) {
			h |= 1
		}
		out = append(append(out, byte(h), byte(h>>8), byte(h>>16)), b[:n]...)
		if b = b[n:]; len(b) == 0 {
			return out
		}
	}
}

// condaOf is the .conda of name-version-build (noarch).
func condaOf(name, version, build string) []byte {
	stem := name + "-" + version + "-" + build
	index := fmt.Sprintf(`{"name": %q, "version": %q, "build": %q, "build_number": 0, "depends": [], "license": "MIT", "subdir": "noarch"}`, name, version, build)
	return zipOf("metadata.json", `{"conda_pkg_format_version": 2}`,
		"info-"+stem+".tar.zst", string(zstdRaw(tarOf("info/index.json", index))),
		"pkg-"+stem+".tar.zst", string(zstdRaw(tarOf("lib/x.py", "print('hi')"))))
}

func simplePOM(g, a, v string) string {
	return fmt.Sprintf("<project><modelVersion>4.0.0</modelVersion><groupId>%s</groupId><artifactId>%s</artifactId><version>%s</version></project>", g, a, v)
}

// ---- tests ----

// GET /formats/upload-specs and /formats/{format}/upload-specs (rest-02).
func TestUploadSpecs(t *testing.T) {
	e := startUploads(t)
	type field struct {
		Name, Type string
		Optional   bool
	}
	type spec struct {
		Format          string
		MultipleUpload  bool
		ComponentFields []field
		AssetFields     []field
	}
	var specs []spec
	st, b, _ := e.send("GET", v1+"/formats/upload-specs", "", "", nil) // no privilege needed
	if st != 200 || json.Unmarshal(b, &specs) != nil {
		t.Fatalf("specs: %d %s", st, b)
	}
	names := func(fs []field) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Name)
		}
		return strings.Join(out, ",")
	}
	byFormat := map[string]spec{}
	for _, s := range specs {
		byFormat[s.Format] = s
		if last := s.AssetFields[len(s.AssetFields)-1]; last != (field{"asset", "FILE", false}) {
			t.Errorf("%s: last asset field %+v", s.Format, last)
		}
	}
	for _, f := range []string{"apt", "composer", "conda", "helm", "maven2", "npm", "nuget", "pypi", "r", "raw", "rubygems", "terraform", "yum"} {
		if _, ok := byFormat[f]; !ok {
			t.Errorf("no spec for %s: %s", f, b)
		}
	}
	mvn := byFormat["maven2"]
	if !mvn.MultipleUpload || names(mvn.ComponentFields) != "groupId,artifactId,version,generate-pom,packaging" ||
		names(mvn.AssetFields) != "classifier,extension,asset" || mvn.ComponentFields[3].Type != "BOOLEAN" || !mvn.ComponentFields[3].Optional {
		t.Fatalf("maven2 spec: %+v", mvn)
	}
	if r := byFormat["raw"]; !r.MultipleUpload || names(r.ComponentFields) != "directory" || names(r.AssetFields) != "filename,asset" {
		t.Fatalf("raw spec: %+v", r)
	}
	if y := byFormat["yum"]; y.MultipleUpload || names(y.ComponentFields) != "directory" || !y.ComponentFields[0].Optional || names(y.AssetFields) != "filename,asset" {
		t.Fatalf("yum spec: %+v", y)
	}
	if r := byFormat["r"]; names(r.AssetFields) != "pathId,asset" || names(r.ComponentFields) != "" {
		t.Fatalf("r spec: %+v", r)
	}
	for _, f := range []string{"apt", "npm", "pypi", "nuget", "rubygems", "helm"} {
		if s := byFormat[f]; s.MultipleUpload || len(s.ComponentFields) != 0 || names(s.AssetFields) != "asset" {
			t.Errorf("%s spec: %+v", f, s)
		}
	}
	var one spec
	e.get(v1+"/formats/maven2/upload-specs", "", &one)
	if one.Format != "maven2" || names(one.AssetFields) != "classifier,extension,asset" {
		t.Fatalf("maven2 alone: %+v", one)
	}
	for _, f := range []string{"docker", "maven", "go", "nope"} {
		e.want("GET", v1+"/formats/"+f+"/upload-specs", "", "", 404)
	}
}

// Maven (maven-12): curl -F with generate-pom, then the layout of
// Jenkins's nexusPublisher (component fields after the files, blank
// classifiers, a POM asset, text parts with a charset), then refusals.
func TestUploadMaven(t *testing.T) {
	e := startUploads(t)
	jar := zipOf("META-INF/MANIFEST.MF", "Manifest-Version: 1.0\n")
	e.wantUpload(204, "ci", "maven-releases",
		fld("maven2.groupId", "org.example"), fld("maven2.artifactId", "app"), fld("maven2.version", "1.0"),
		fld("maven2.generate-pom", "true"), fld("maven2.packaging", "jar"),
		fil("maven2.asset1", "app.jar", jar), fld("maven2.asset1.extension", "jar"),
		fil("maven2.asset2", "app-sources.jar", jar), fld("maven2.asset2.classifier", "sources"), fld("maven2.asset2.extension", "jar"))
	const dir = "/repository/maven-releases/org/example/app/"
	if b := e.body(dir + "1.0/app-1.0.jar"); b != string(jar) {
		t.Fatalf("jar: %q", b)
	}
	e.body(dir + "1.0/app-1.0-sources.jar")
	if p := e.body(dir + "1.0/app-1.0.pom"); !strings.Contains(p, "<groupId>org.example</groupId>") || !strings.Contains(p, "<packaging>jar</packaging>") {
		t.Fatalf("generated pom: %s", p)
	}
	if md := e.body(dir + "maven-metadata.xml"); !strings.Contains(md, "<version>1.0</version>") {
		t.Fatalf("metadata: %s", md)
	}
	sum := sha1.Sum(jar)
	if s := e.body(dir + "1.0/app-1.0.jar.sha1"); s != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha1: %q", s)
	}
	// The search API finds the component the files make.
	var page struct {
		Items []struct{ Group, Name, Version string }
	}
	e.get(v1+"/search?repository=maven-releases&name=app", "ci", &page)
	if len(page.Items) != 1 || page.Items[0].Version != "1.0" {
		t.Fatalf("search: %+v", page)
	}

	// nexusPublisher: the component attributes, then each asset with its
	// extension and an empty classifier; text parts declare a charset.
	text := func(name, value string) part {
		p := fld(name, value)
		p.hdr = textproto.MIMEHeader{"Content-Type": {"text/plain; charset=ISO-8859-1"}}
		return p
	}
	e.wantUpload(204, "ci", "maven-releases",
		fil("maven2.asset1", "lib-2.0.jar", jar), text("maven2.asset1.extension", "jar"), text("maven2.asset1.classifier", ""),
		fil("maven2.asset2", "pom.xml", []byte(simplePOM("org.example", "lib", "2.0"))), text("maven2.asset2.extension", "pom"), text("maven2.asset2.classifier", ""),
		text("maven2.groupId", "org.example"), text("maven2.artifactId", "lib"), text("maven2.version", "2.0"), text("maven2.packaging", "jar"))
	e.body("/repository/maven-releases/org/example/lib/2.0/lib-2.0.jar")
	if p := e.body("/repository/maven-releases/org/example/lib/2.0/lib-2.0.pom"); p != simplePOM("org.example", "lib", "2.0") {
		t.Fatalf("uploaded pom: %s", p)
	}

	// A POM asset alone names the coordinates; its parent gives the group.
	parent := `<project><parent><groupId>org.parent</groupId><artifactId>p</artifactId><version>3.0</version></parent>` +
		`<artifactId>child</artifactId><properties><rev>3.1</rev></properties><version>${rev}</version></project>`
	e.wantUpload(204, "ci", "maven-releases", fil("maven2.asset1", "pom.xml", []byte(parent)), fld("maven2.asset1.extension", "pom"),
		fil("maven2.asset2", "c.jar", jar), fld("maven2.asset2.extension", "jar"))
	e.body("/repository/maven-releases/org/parent/child/3.1/child-3.1.jar")

	// Refusals: nothing is stored.
	e.wantInvalid("*", "does not allow updating", "ci", "maven-releases", // allow_once
		fld("maven2.groupId", "org.example"), fld("maven2.artifactId", "app"), fld("maven2.version", "1.0"),
		fil("maven2.asset1", "app.jar", jar), fld("maven2.asset1.extension", "jar"))
	e.wantInvalid("*", "SNAPSHOT", "ci", "maven-snapshots",
		fld("maven2.groupId", "org.example"), fld("maven2.artifactId", "app"), fld("maven2.version", "1.0-SNAPSHOT"),
		fil("maven2.asset1", "app.jar", jar), fld("maven2.asset1.extension", "jar"))
	e.wantInvalid("*", "RELEASE", "ci", "maven-releases",
		fld("maven2.groupId", "org.example"), fld("maven2.artifactId", "snap"), fld("maven2.version", "1.0-SNAPSHOT"),
		fil("maven2.asset1", "app.jar", jar), fld("maven2.asset1.extension", "jar"))
	e.missing("/repository/maven-releases/org/example/snap/1.0-SNAPSHOT/snap-1.0-SNAPSHOT.jar")
	e.wantInvalid("groupId", "groupId", "ci", "maven-releases", fld("maven2.artifactId", "x"), fld("maven2.version", "1"),
		fil("maven2.asset1", "x.jar", jar), fld("maven2.asset1.extension", "jar"))
	e.wantInvalid("extension", "extension", "ci", "maven-releases", fld("maven2.groupId", "g"), fld("maven2.artifactId", "x"), fld("maven2.version", "1"),
		fil("maven2.asset1", "x.jar", jar))
	e.wantInvalid("color", "unknown component field", "ci", "maven-releases", fld("maven2.groupId", "g"), fld("maven2.artifactId", "x"),
		fld("maven2.version", "1"), fld("maven2.color", "red"), fil("maven2.asset1", "x.jar", jar), fld("maven2.asset1.extension", "jar"))
	e.wantInvalid("*", "same path", "ci", "maven-releases", fld("maven2.groupId", "g"), fld("maven2.artifactId", "x"), fld("maven2.version", "1"),
		fil("maven2.asset1", "x.jar", jar), fld("maven2.asset1.extension", "jar"), fil("maven2.asset2", "y.jar", jar), fld("maven2.asset2.extension", "jar"))
	e.wantInvalid("*", ".pom", "ci", "maven-releases", fil("maven2.asset1", "pom.xml", []byte("<project><version>${v}</version></project>")),
		fld("maven2.asset1.extension", "pom"))
	// The POM is stored first: when it fails (allow_once here), no other file is.
	e.wantUpload(204, "ci", "maven-releases", fil("maven2.asset1", "pom.xml", []byte(simplePOM("org.example", "late", "1"))), fld("maven2.asset1.extension", "pom"))
	e.wantInvalid("*", "does not allow updating", "ci", "maven-releases", fld("maven2.groupId", "org.example"), fld("maven2.artifactId", "late"),
		fld("maven2.version", "1"), fld("maven2.generate-pom", "on"), fil("maven2.asset1", "x.jar", jar), fld("maven2.asset1.extension", "jar"))
	e.missing("/repository/maven-releases/org/example/late/1/late-1.jar")
	// A SNAPSHOT version in a mixed repository is a plain file.
	e.wantUpload(204, "ci", "maven-mixed", fld("maven2.groupId", "g"), fld("maven2.artifactId", "s"), fld("maven2.version", "1.0-SNAPSHOT"),
		fil("maven2.asset1", "s.jar", jar), fld("maven2.asset1.extension", "jar"))
	e.body("/repository/maven-mixed/g/s/1.0-SNAPSHOT/s-1.0-SNAPSHOT.jar")
}

// Raw (aptraw-05, rest-01) and the refusals every format shares.
func TestUploadRaw(t *testing.T) {
	e := startUploads(t)
	e.wantUpload(204, "ci", "raw-files", fld("raw.directory", "/a//b/"),
		fil("raw.asset1", "x", []byte("hello")), fld("raw.asset1.filename", "c.txt"),
		fil("raw.asset2", "y", []byte("two")), fld("raw.asset2.filename", "sub/d.txt"))
	if b := e.body("/repository/raw-files/a/b/c.txt"); b != "hello" {
		t.Fatalf("c.txt: %q", b)
	}
	if _, _, h := e.send("GET", "/repository/raw-files/a/b/c.txt", "ci", "", nil); !strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Fatalf("c.txt type: %s", h.Get("Content-Type"))
	}
	e.body("/repository/raw-files/a/b/sub/d.txt")
	// The native upload's event, by the caller.
	var list []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		_, list, _ = e.send("GET", "/-/api/events?kind=upload", "admin", "", nil)
		if strings.Contains(string(list), `"subject":"a/b/sub/d.txt"`) {
			break
		}
	}
	if !strings.Contains(string(list), `"subject":"a/b/c.txt"`) || !strings.Contains(string(list), `"actor":"ci"`) {
		t.Fatalf("events: %s", list)
	}

	ok := []part{fld("raw.directory", "z"), fil("raw.asset1", "f", []byte("x")), fld("raw.asset1.filename", "f.txt")}
	// Not multipart: 400 {"message"}.
	if st, b, _ := e.send("POST", v1+"/components?repository=raw-files", "ci", "application/json", []byte(`{}`)); st != 400 || !strings.Contains(string(b), `"message"`) {
		t.Fatalf("json body: %d %s", st, b)
	}
	if st, b, _ := e.send("POST", v1+"/components?repository=raw-files", "ci", "multipart/form-data", []byte("x")); st != 400 {
		t.Fatalf("no boundary: %d %s", st, b)
	}
	if st, b, h := e.send("POST", v1+"/components?repository=raw-files", "", "multipart/form-data; boundary=x", nil); st != 401 || h.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous: %d %s", st, b)
	}
	e.wantUpload(403, "viewer", "raw-files", ok...)
	e.wantUpload(404, "ci", "nope", ok...)
	e.wantUpload(400, "ci", "", ok...)
	e.wantInvalid("*", "hosted", "ci", "raw-proxy", ok...)
	e.wantInvalid("*", "hosted", "ci", "raw-group", ok...)
	e.wantInvalid("*", "offline", "ci", "raw-offline", ok...)
	e.wantInvalid("*", "go components", "ci", "go-hosted", ok...)
	e.wantInvalid("directory", "directory", "ci", "raw-files", fil("raw.asset1", "f", []byte("x")), fld("raw.asset1.filename", "f.txt"))
	e.wantInvalid("filename", "filename", "ci", "raw-files", fld("raw.directory", "z"), fil("raw.asset1", "f", []byte("x")))
	e.wantInvalid("*", "no asset", "ci", "raw-files", fld("raw.directory", "z"))
	e.wantInvalid("asset1", "unknown component field", "ci", "raw-files", fld("raw.directory", "z"), fld("raw.asset1", "not a file"))
	// One bad path stops them all.
	e.wantInvalid("filename", "invalid path", "ci", "raw-files", fld("raw.directory", "z"),
		fil("raw.asset1", "f", []byte("x")), fld("raw.asset1.filename", "good.txt"),
		fil("raw.asset2", "f", []byte("x")), fld("raw.asset2.filename", "../bad.txt"))
	e.missing("/repository/raw-files/z/good.txt")
	e.wantUpload(204, "ci", "raw-files", ok...)
	e.wantInvalid("*", "does not allow updating", "ci", "raw-files", ok...) // allow_once
	// The upload cap counts the whole form.
	if st, b := uiCall(e, "PUT", "/-/api/settings", `{"maxUploadMiB":1}`); st != 200 {
		t.Fatalf("settings: %d %s", st, b)
	}
	e.wantUpload(413, "ci", "raw-files", fld("raw.directory", "big"), fil("raw.asset1", "f", bytes.Repeat([]byte("x"), 2<<20)), fld("raw.asset1.filename", "f"))
	e.missing("/repository/raw-files/big/f")
	// Recovery mode refuses the upload as it does every write.
	if st, b := uiCall(e, "PUT", "/-/api/recovery", `{"enabled":true,"message":"restoring"}`); st != 200 {
		t.Fatalf("recovery: %d %s", st, b)
	}
	e.wantUpload(503, "ci", "raw-files", fld("raw.directory", "r"), fil("raw.asset1", "f", []byte("x")), fld("raw.asset1.filename", "f"))
}

// A content-selector privilege is checked per path, before any file is stored.
func TestUploadSelectorPrivilege(t *testing.T) {
	e := startUploads(t)
	e.want("POST", v1+"/security/content-selectors", "admin", `{"name":"pub","expression":"path =^ \"/pub/ok\""}`, 204)
	e.want("POST", v1+"/security/privileges/repository-content-selector", "admin",
		`{"name":"pub-add","actions":["READ","ADD"],"format":"raw","repository":"raw-files","contentSelector":"pub"}`, 201)
	if st, b := uiPost(e, "/-/api/users", `{"id":"dev","password":"longenough","roles":["priv:pub-add"]}`); st != 201 {
		t.Fatalf("user: %d %s", st, b)
	}
	const dev = "dev:longenough"
	e.wantUpload(204, dev, "raw-files", fld("raw.directory", "pub"), fil("raw.asset1", "f", []byte("x")), fld("raw.asset1.filename", "ok.txt"))
	e.wantUpload(403, dev, "raw-files", fld("raw.directory", "pub"),
		fil("raw.asset1", "f", []byte("x")), fld("raw.asset1.filename", "ok2.txt"),
		fil("raw.asset2", "f", []byte("x")), fld("raw.asset2.filename", "no.txt"))
	e.missing("/repository/raw-files/pub/ok2.txt")
	e.wantUpload(403, "viewer", "raw-files", fld("raw.directory", "pub"), fil("raw.asset1", "f", []byte("x")), fld("raw.asset1.filename", "ok3.txt"))
}

// The single-file formats: each upload lands where its own client puts
// it, with the metadata rebuilt.
func TestUploadFormats(t *testing.T) {
	e := startUploads(t)

	// apt: the package is parsed and stored at its pool path.
	e.wantUpload(204, "ci", "apt-hosted", fil("apt.asset", "hello.deb", debOf("hello")))
	if p := e.body("/repository/apt-hosted/dists/stable/main/binary-amd64/Packages"); !strings.Contains(p, "Package: hello\n") {
		t.Fatalf("Packages: %s", p)
	}
	e.wantInvalid("*", "already exists", "ci", "apt-hosted", fil("apt.asset", "hello.deb", debOf("hello"))) // 409 natively
	e.wantInvalid("*", "deb", "ci", "apt-hosted", fil("apt.asset", "x.deb", []byte("not a deb")))
	e.wantInvalid("*", "one asset", "ci", "apt-hosted", fil("apt.asset", "a.deb", debOf("a")), fil("apt.asset2", "b.deb", debOf("b")))

	// npm: published as npm publish would, tagged latest.
	pj := `{"name":"@acme/demo","version":"1.2.0","description":"d","dist":{"shasum":"bogus"}}`
	e.wantUpload(204, "ci", "npm-hosted", fil("npm.asset", "demo-1.2.0.tgz", tgzOf("package/package.json", pj, "package/index.js", "1")))
	var doc struct {
		DistTags map[string]string         `json:"dist-tags"`
		Versions map[string]map[string]any `json:"versions"`
	}
	if err := json.Unmarshal([]byte(e.body("/repository/npm-hosted/@acme%2fdemo")), &doc); err != nil || doc.DistTags["latest"] != "1.2.0" || doc.Versions["1.2.0"]["description"] != "d" {
		t.Fatalf("packument: %+v %v", doc, err)
	}
	e.body("/repository/npm-hosted/@acme/demo/-/demo-1.2.0.tgz")
	e.wantUpload(403, "ci", "npm-hosted", fil("npm.asset", "demo-1.2.0.tgz", tgzOf("package/package.json", pj))) // npm: cannot publish over
	e.wantInvalid("*", "package.json", "ci", "npm-hosted", fil("npm.asset", "x.tgz", tgzOf("other/readme", "x")))

	// pypi: name and version come from the wheel's own metadata.
	whl := zipOf("demo_pkg-1.0.0.dist-info/METADATA", "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 1.0.0\n")
	e.wantUpload(204, "ci", "pypi-hosted", fil("pypi.asset", "demo_pkg-1.0.0-py3-none-any.whl", whl))
	if b := e.body("/repository/pypi-hosted/simple/demo-pkg/"); !strings.Contains(b, "demo_pkg-1.0.0-py3-none-any.whl") {
		t.Fatalf("simple index: %s", b)
	}
	sdist := tgzOf("demo-pkg-2.0/PKG-INFO", "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 2.0\n")
	e.wantUpload(204, "ci", "pypi-hosted", fil("pypi.asset", "demo-pkg-2.0.tar.gz", sdist))
	e.body("/repository/pypi-hosted/packages/demo-pkg/demo-pkg-2.0.tar.gz")
	e.wantInvalid("*", "unsupported file name", "ci", "pypi-hosted", fil("pypi.asset", "demo.exe", whl))
	e.wantInvalid("*", "does not belong", "ci", "pypi-hosted", fil("pypi.asset", "other-1.0.0-py3-none-any.whl", whl))

	// nuget: pushed as the body.
	nuspec := `<?xml version="1.0"?><package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata>` +
		`<id>Demo.Pkg</id><version>1.0.0</version><authors>QA</authors><description>demo</description></metadata></package>`
	e.wantUpload(204, "ci", "nuget-hosted", fil("nuget.asset", "Demo.Pkg.1.0.0.nupkg", zipOf("[Content_Types].xml", "<Types/>", "Demo.Pkg.nuspec", nuspec)))
	if b := e.body("/repository/nuget-hosted/v3-flatcontainer/demo.pkg/index.json"); !strings.Contains(b, "1.0.0") {
		t.Fatalf("nuget versions: %s", b)
	}

	// rubygems: gem push.
	e.wantUpload(204, "ci", "gems", fil("rubygems.asset", "demo-0.1.0.gem", gemOf("demo", "0.1.0")))
	e.body("/repository/gems/gems/demo-0.1.0.gem")
	if b := e.body("/repository/gems/info/demo"); !strings.Contains(b, "0.1.0") {
		t.Fatalf("gem info: %s", b)
	}

	// yum: directory/filename; this repository keeps repodata per directory.
	e.wantUpload(204, "ci", "yum-hosted", fld("yum.directory", "/el9/"),
		fil("yum.asset", "x.rpm", rpmOf("demo", "1.0", "1", "x86_64")), fld("yum.asset.filename", "demo-1.0-1.x86_64.rpm"))
	e.body("/repository/yum-hosted/el9/demo-1.0-1.x86_64.rpm")
	if b := e.body("/repository/yum-hosted/el9/repodata/repomd.xml"); !strings.Contains(b, "primary") {
		t.Fatalf("repomd: %s", b)
	}
	e.wantInvalid("*", "does not match", "ci", "yum-hosted", fld("yum.directory", "el9"),
		fil("yum.asset", "x.rpm", rpmOf("demo", "1.0", "1", "x86_64")), fld("yum.asset.filename", "wrong-1-1.x86_64.rpm"))

	// helm: the chart's Chart.yaml names it.
	e.wantUpload(204, "ci", "helm-hosted", fil("helm.asset", "chart.tgz", tgzOf("demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: 0.1.0\n")))
	if b := e.body("/repository/helm-hosted/index.yaml"); !strings.Contains(b, "demo-0.1.0.tgz") {
		t.Fatalf("index.yaml: %s", b)
	}

	// r: pathId/<the part's file name>.
	desc := "Package: hello\nVersion: 1.0.0\nTitle: Hi\nLicense: MIT\n"
	e.wantUpload(204, "ci", "r-hosted", fil("r.asset", "hello_1.0.0.tar.gz", tgzOf("hello/DESCRIPTION", desc, "hello/R/hello.R", "1")),
		fld("r.asset.pathId", "/src/contrib/"))
	if b := e.body("/repository/r-hosted/src/contrib/PACKAGES"); !strings.Contains(b, "Package: hello") {
		t.Fatalf("PACKAGES: %s", b)
	}
	e.wantInvalid("pathId", "pathId", "ci", "r-hosted", fil("r.asset", "hello_1.0.0.tar.gz", []byte("x")))

	// conda (an extension): directory/<the part's file name>.
	e.wantUpload(204, "ci", "conda-hosted", fld("conda.directory", "noarch"), fil("conda.asset", "demo-1.0-py_0.conda", condaOf("demo", "1.0", "py_0")))
	if b := e.body("/repository/conda-hosted/noarch/repodata.json"); !strings.Contains(b, "demo-1.0-py_0.conda") {
		t.Fatalf("repodata.json: %s", b)
	}

	// composer (an extension): packages/upload/<vendor>/<package>/<version>.
	e.wantUpload(204, "ci", "composer-hosted", fld("composer.vendor", "acme"), fld("composer.package", "lib"), fld("composer.version", "1.0.0"),
		fil("composer.asset", "lib.zip", zipOf("composer.json", `{"name":"acme/lib","type":"library"}`, "src/A.php", "<?php")))
	if b := e.body("/repository/composer-hosted/p2/acme/lib.json"); !strings.Contains(b, "1.0.0") {
		t.Fatalf("p2: %s", b)
	}
	e.wantInvalid("vendor", "one path segment", "ci", "composer-hosted", fld("composer.vendor", "a/b"), fld("composer.package", "lib"),
		fld("composer.version", "1.0.0"), fil("composer.asset", "lib.zip", []byte("x")))

	// terraform (an extension): a module archive.
	e.wantUpload(204, "ci", "tf-hosted", fld("terraform.namespace", "acme"), fld("terraform.name", "net"), fld("terraform.provider", "aws"),
		fld("terraform.version", "1.0.0"), fil("terraform.asset", "net.tgz", tgzOf("main.tf", "variable \"x\" {}\n")))
	if b := e.body("/repository/tf-hosted/v1/modules/acme/net/aws/versions"); !strings.Contains(b, "1.0.0") {
		t.Fatalf("module versions: %s", b)
	}
}

// Real curl (as CI scripts and the Nexus documentation use it): the
// component API with -F, and APT's raw-body POST (aptraw-01).
func TestUploadCurl(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	e := startUploads(t)
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(curl, append([]string{"-sS", "-u", "ci:pw", "-o", "/dev/null", "-w", "%{http_code}"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("curl %v: %v %s", args, err, out)
		}
		return string(out)
	}
	api := e.ts.URL + v1 + "/components?repository="
	if st := run("-F", "raw.directory=docs", "-F", "raw.asset1=@"+write("readme.txt", []byte("read me")), "-F", "raw.asset1.filename=readme.txt", api+"raw-files"); st != "204" {
		t.Fatalf("raw: %s", st)
	}
	if b := e.body("/repository/raw-files/docs/readme.txt"); b != "read me" {
		t.Fatalf("raw file: %q", b)
	}
	jar := write("app-1.0.jar", zipOf("a", "b"))
	if st := run("-F", "maven2.groupId=com.example", "-F", "maven2.artifactId=app", "-F", "maven2.version=1.0", "-F", "maven2.generate-pom=true",
		"-F", "maven2.asset1=@"+jar, "-F", "maven2.asset1.extension=jar", api+"maven-releases"); st != "204" {
		t.Fatalf("maven: %s", st)
	}
	e.body("/repository/maven-releases/com/example/app/1.0/app-1.0.pom")
	if st := run("-F", "pypi.asset=@"+write("demo_pkg-1.0.0-py3-none-any.whl", zipOf("demo_pkg-1.0.0.dist-info/METADATA", "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 1.0.0\n")), api+"pypi-hosted"); st != "204" {
		t.Fatalf("pypi: %s", st)
	}
	deb := write("hello.deb", debOf("hello"))
	if st := run("-H", "Content-Type: multipart/form-data", "--data-binary", "@"+deb, e.ts.URL+"/repository/apt-hosted/"); st != "201" {
		t.Fatalf("apt raw POST: %s", st)
	}
	if p := e.body("/repository/apt-hosted/dists/stable/main/binary-amd64/Packages"); !strings.Contains(p, "Package: hello\n") {
		t.Fatalf("Packages: %s", p)
	}
	if st := run("-F", "apt.asset=@"+write("other.deb", debOf("other")), api+"apt-hosted"); st != "204" {
		t.Fatalf("apt API: %s", st)
	}
	if st := run("-H", "Content-Type: application/json", "-d", "{}", api+"raw-files"); st != "400" {
		t.Fatalf("json: %s", st)
	}
}
