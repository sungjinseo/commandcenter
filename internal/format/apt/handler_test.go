package apt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
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
	"commandcenter/internal/pgp"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/resolve"
)

// arMember appends one ar archive member as dpkg-deb writes them: a
// 60-byte header, the data, a newline of padding to an even length.
func arMember(w *bytes.Buffer, name string, data []byte) {
	fmt.Fprintf(w, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(data))
	w.Write(data)
	if len(data)%2 == 1 {
		w.WriteByte('\n')
	}
}

func tarOf(t *testing.T, files map[string]string, compress bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var gz *gzip.Writer
	if compress {
		gz = gzip.NewWriter(&buf)
		w = gz
	}
	tw := tar.NewWriter(w)
	for _, name := range []string{"./", "./control", "./md5sums", "./usr/bin/hello", "control", "x"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			hdr.Typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	if gz != nil {
		gz.Close()
	}
	return buf.Bytes()
}

// deb builds a package as dpkg-deb does: debian-binary, control.tar.gz
// (or control.tar / control.tar.xz by name) with ./control, data.tar.gz.
func deb(t *testing.T, control string, controlName string) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	arMember(&buf, controlName, tarOf(t, map[string]string{"./": "", "./control": control, "./md5sums": "x  usr/bin/hello\n"}, strings.HasSuffix(controlName, ".gz")))
	arMember(&buf, "data.tar.gz", tarOf(t, map[string]string{"./usr/bin/hello": "#!/bin/sh\necho hello\n"}, true))
	return buf.Bytes()
}

const helloControl = "Package: hello\nVersion: 1.0\nArchitecture: amd64\nMaintainer: QA <qa@example.test>\nInstalled-Size: 12\n" +
	"Depends: libc6 (>= 2.34), curl | wget\nSection: utils\nPriority: optional\nHomepage: https://example.test/hello\n" +
	"Description: Greets you\n A longer description\n .\n on several lines\n"

func control(pkg, version, arch string) string {
	return "Package: " + pkg + "\nVersion: " + version + "\nArchitecture: " + arch + "\nMaintainer: QA <qa@example.test>\nDescription: " + pkg + "\n"
}

func sums(b []byte) (md5s, sha1s, sha256s string) {
	m, s1, s2 := md5.Sum(b), sha1.Sum(b), sha256.Sum256(b)
	return hex.EncodeToString(m[:]), hex.EncodeToString(s1[:]), hex.EncodeToString(s2[:])
}

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
	sink  *repotest.Sink
	hits  atomic.Int32
	down  atomic.Bool
	now   time.Time
	up    map[string][]byte // the upstream's files by path
}

func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	if e.down.Load() {
		http.Error(w, "down", 503)
		return
	}
	e.hits.Add(1)
	if b, ok := e.up[strings.TrimPrefix(r.URL.Path, "/debian/")]; ok {
		if strings.HasSuffix(r.URL.Path, "/") {
			w.Header().Set("Content-Type", "text/html")
		}
		w.Write(b)
		return
	}
	http.NotFound(w, r)
}

func testSigningKey(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "pgp", "testdata", "rsa2048.asc"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	upDeb := deb(t, control("upstream", "2.0", "amd64"), "control.tar.gz")
	e.up = map[string][]byte{
		"dists/bookworm/InRelease": []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nOrigin: Debian\nSuite: stable\n-----BEGIN PGP SIGNATURE-----\n\nabc\n-----END PGP SIGNATURE-----\n"),
		"dists/bookworm/Release":   []byte("Origin: Debian\nSuite: stable\n"),
		"dists/":                   []byte("<html>listing</html>"),
		"pool/main/u/upstream/upstream_2.0_amd64.deb": upDeb,
	}
	ts := httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(ts.Close)
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
		{Name: "apt-hosted", Format: "apt", Type: "hosted", WritePolicy: "allow_once", Distribution: "stable", Component: "main"},
		{Name: "apt-allow", Format: "apt", Type: "hosted", WritePolicy: "allow", Distribution: "stable", Component: "main"},
		{Name: "apt-ro", Format: "apt", Type: "hosted", WritePolicy: "deny", Distribution: "stable", Component: "main"},
		{Name: "apt-signed", Format: "apt", Type: "hosted", WritePolicy: "allow_once", Distribution: "jammy", Component: "internal", SigningKey: testSigningKey(t)},
		{Name: "apt-proxy", Format: "apt", Type: "proxy", Remote: ts.URL + "/debian/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "apt-all", Format: "apt", Type: "group", Members: []string{"apt-hosted", "apt-proxy"}, WriteMember: "apt-hosted"},
		{Name: "apt-proxy-first", Format: "apt", Type: "group", Members: []string{"apt-proxy", "apt-signed"}},
	}}
	f := proxy.New(store, ts.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, &resolve.Resolver{Store: store, Fetcher: f}, auth.New(cfg, db))
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
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// text GETs a path and returns its body, failing on anything but 200.
func (e *env) text(t *testing.T, repoName, path string) string {
	t.Helper()
	w := e.do("GET", repoName, path, nil)
	if w.Code != 200 {
		t.Fatalf("GET %s/%s: %d %s", repoName, path, w.Code, w.Body)
	}
	return w.Body.String()
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// releaseFor is the Release file build writes for the given Packages
// files (path -> body), in list order, with the repository's names.
func (e *env) releaseFor(name, dist, comp string, archs []string, files map[string]string) string {
	var sb strings.Builder
	sb.WriteString("Origin: " + name + "\nLabel: " + name + "\nSuite: " + dist + "\nCodename: " + dist + "\nDate: " + e.now.Format(time.RFC1123) +
		"\nArchitectures: " + strings.Join(archs, " ") + "\nComponents: " + comp + "\nDescription: Command Center apt repository " + name + "\n")
	var paths []string
	for _, a := range archs {
		paths = append(paths, comp+"/binary-"+a+"/Packages", comp+"/binary-"+a+"/Packages.gz")
	}
	for i, h := range []string{"MD5Sum", "SHA1", "SHA256"} {
		sb.WriteString(h + ":\n")
		for _, p := range paths {
			s := [3]string{}
			s[0], s[1], s[2] = sums([]byte(files[p]))
			sb.WriteString(fmt.Sprintf(" %s %16d %s\n", s[i], len(files[p]), p))
		}
	}
	return sb.String()
}

func TestHostedUploadAndMetadata(t *testing.T) {
	e := newEnv(t)
	// before any upload: an empty Release, so `apt update` succeeds
	if got := e.text(t, "apt-hosted", "dists/stable/Release"); got != e.releaseFor("apt-hosted", "stable", "main", nil, nil) {
		t.Fatalf("empty Release:\n%s", got)
	}
	if w := e.do("GET", "apt-hosted", "dists/stable/main/binary-amd64/Packages", nil); w.Code != 404 {
		t.Fatalf("empty repository Packages: %d", w.Code)
	}
	hello := deb(t, helloControl, "control.tar.gz")
	w := e.do("PUT", "apt-hosted", "pool/any/dir/hello.deb", hello)
	if w.Code != 201 || w.Body.String() != `{"architecture":"amd64","filename":"pool/main/h/hello/hello_1.0_amd64.deb","package":"hello","version":"1.0"}`+"\n" {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "apt-hosted", Subject: "amd64/hello@1.0", Detail: "pool/main/h/hello/hello_1.0_amd64.deb"}) {
		t.Fatalf("events: %v", got)
	}
	w = e.do("GET", "apt-hosted", "pool/main/h/hello/hello_1.0_amd64.deb", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), hello) || w.Header().Get("Content-Type") != ctDeb {
		t.Fatalf("GET deb: %d %v", w.Code, w.Header())
	}
	if w := e.do("HEAD", "apt-hosted", "pool/main/h/hello/hello_1.0_amd64.deb", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD deb: %d %d", w.Code, w.Body.Len())
	}
	// Packages, byte for byte: the control fields, Filename, Size and the
	// checksums, Description last, a blank line after the stanza
	m5, s1, s2 := sums(hello)
	helloStanza := "Package: hello\nVersion: 1.0\nArchitecture: amd64\nMaintainer: QA <qa@example.test>\nInstalled-Size: 12\n" +
		"Depends: libc6 (>= 2.34), curl | wget\nSection: utils\nPriority: optional\nHomepage: https://example.test/hello\n" +
		"Filename: pool/main/h/hello/hello_1.0_amd64.deb\nSize: " + fmt.Sprint(len(hello)) + "\nMD5sum: " + m5 + "\nSHA1: " + s1 + "\nSHA256: " + s2 + "\n" +
		"Description: Greets you\n A longer description\n .\n on several lines\n\n"
	w = e.do("GET", "apt-hosted", "dists/stable/main/binary-amd64/Packages", nil)
	if w.Code != 200 || w.Body.String() != helloStanza || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("Packages: %d %v\n%s", w.Code, w.Header(), w.Body)
	}
	w = e.do("GET", "apt-hosted", "dists/stable/main/binary-amd64/Packages.gz", nil)
	if w.Code != 200 || gunzip(t, w.Body.Bytes()) != helloStanza || w.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("Packages.gz: %d %v", w.Code, w.Header())
	}
	gz := w.Body.String()
	// Release, byte for byte: the files with their sizes and checksums
	want := e.releaseFor("apt-hosted", "stable", "main", []string{"amd64"}, map[string]string{"main/binary-amd64/Packages": helloStanza, "main/binary-amd64/Packages.gz": gz})
	if got := e.text(t, "apt-hosted", "dists/stable/Release"); got != want {
		t.Fatalf("Release:\n%s\nwant:\n%s", got, want)
	}
	// unsigned: no Release.gpg, InRelease or public key
	for _, p := range []string{"dists/stable/Release.gpg", "dists/stable/InRelease", "public.key"} {
		if w := e.do("GET", "apt-hosted", p, nil); w.Code != 404 {
			t.Fatalf("%s: %d, want 404", p, w.Code)
		}
	}
	// allow_once: the same package, version and architecture again is a 409
	if w := e.do("PUT", "apt-hosted", "pool/hello.deb", deb(t, strings.Replace(helloControl, "Section: utils", "Section: other", 1), "control.tar.gz")); w.Code != 409 || !strings.Contains(w.Body.String(), "hello version 1.0 (amd64)") {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
	// an "all" package is listed in every architecture's Packages, and in binary-all
	tool := deb(t, control("tool", "2.0", "all"), "control.tar")
	if w := e.do("PUT", "apt-hosted", "pool/tool.deb", tool); w.Code != 201 {
		t.Fatalf("all: %d %s", w.Code, w.Body)
	}
	libArm := deb(t, control("libfoo1", "1:0.9-2", "arm64"), "control.tar.gz")
	if w := e.do("PUT", "apt-hosted", "pool/x.deb", libArm); w.Code != 201 || !strings.Contains(w.Body.String(), `"filename":"pool/main/libf/libfoo1/libfoo1_1%3a0.9-2_arm64.deb"`) {
		t.Fatalf("lib prefix and epoch: %d %s", w.Code, w.Body)
	}
	amd64 := e.text(t, "apt-hosted", "dists/stable/main/binary-amd64/Packages")
	arm64 := e.text(t, "apt-hosted", "dists/stable/main/binary-arm64/Packages")
	all := e.text(t, "apt-hosted", "dists/stable/main/binary-all/Packages")
	if !strings.HasPrefix(amd64, helloStanza+"Package: tool\n") || strings.Contains(amd64, "libfoo1") {
		t.Fatalf("amd64 Packages:\n%s", amd64)
	}
	if !strings.HasPrefix(arm64, "Package: libfoo1\nVersion: 1:0.9-2\n") || !strings.Contains(arm64, "Filename: pool/main/libf/libfoo1/libfoo1_1%3a0.9-2_arm64.deb\n") || !strings.Contains(arm64, "Package: tool\n") {
		t.Fatalf("arm64 Packages:\n%s", arm64)
	}
	if strings.Count(all, "Package: ") != 1 || !strings.Contains(all, "Package: tool\n") {
		t.Fatalf("all Packages:\n%s", all)
	}
	if rel := e.text(t, "apt-hosted", "dists/stable/Release"); !strings.Contains(rel, "\nArchitectures: all amd64 arm64\n") || strings.Count(rel, "binary-arm64/Packages.gz\n") != 3 {
		t.Fatalf("Release:\n%s", rel)
	}
	// the dists files are stored assets with their media types
	if a, err := e.h.Store.Get(context.Background(), "apt-hosted", "dists/stable/Release"); err != nil || a.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("Release asset: %+v %v", a, err)
	}
}

func TestHostedRejections(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		body []byte
		msg  string
	}{
		{"not an archive", []byte("plain text"), "not a Debian package"},
		{"xz control", deb(t, helloControl, "control.tar.xz"), `compression ".xz" is not supported`},
		{"zstd control", deb(t, helloControl, "control.tar.zst"), `compression ".zst" is not supported`},
		{"no Package", deb(t, "Version: 1.0\nArchitecture: amd64\n", "control.tar.gz"), `field Package "" is missing`},
		{"bad name", deb(t, "Package: Hello_World\nVersion: 1.0\nArchitecture: amd64\n", "control.tar.gz"), `field Package "Hello_World"`},
		{"bad version", deb(t, "Package: hello\nVersion: v1\nArchitecture: amd64\n", "control.tar.gz"), `field Version "v1"`},
		{"bad arch", deb(t, "Package: hello\nVersion: 1.0\nArchitecture: x86_64\n", "control.tar.gz"), `field Architecture "x86_64"`},
		{"not a field", deb(t, "Package: hello\nVersion 1.0\nArchitecture: amd64\n", "control.tar.gz"), "is not a field"},
		{"repeated field", deb(t, "Package: hello\nPackage: hello\nVersion: 1.0\nArchitecture: amd64\n", "control.tar.gz"), "is repeated"},
		{"empty", nil, "empty upload"},
	}
	for _, c := range cases {
		w := e.do("PUT", "apt-hosted", "pool/x.deb", c.body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), c.msg) {
			t.Errorf("%s: %d %s, want 400 %q", c.name, w.Code, w.Body, c.msg)
		}
	}
	// no control member at all, and a control archive without control
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	arMember(&buf, "data.tar.gz", tarOf(t, map[string]string{"./usr/bin/hello": "x"}, true))
	if w := e.do("PUT", "apt-hosted", "pool/x.deb", buf.Bytes()); w.Code != 400 || !strings.Contains(w.Body.String(), "not control.tar") {
		t.Fatalf("no control member: %d %s", w.Code, w.Body)
	}
	buf.Reset()
	buf.WriteString("!<arch>\n")
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	arMember(&buf, "control.tar.gz/", tarOf(t, map[string]string{"./md5sums": "x"}, true)) // GNU ar name with "/"
	if w := e.do("PUT", "apt-hosted", "pool/x.deb", buf.Bytes()); w.Code != 400 || !strings.Contains(w.Body.String(), "no control file") {
		t.Fatalf("no control file: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "apt-hosted", "dists/stable/main/binary-amd64/Packages", nil); w.Code != 404 {
		t.Fatalf("a refused upload left metadata: %d", w.Code)
	}
	if w := e.do("PUT", "apt-ro", "pool/x.deb", deb(t, helloControl, "control.tar.gz")); w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct{ method, repo, path, allow string }{
		{"PUT", "apt-hosted", "dists/stable/Release", "GET, HEAD"}, {"DELETE", "apt-hosted", "pool/x.deb", "GET, HEAD, PUT"},
		{"PUT", "apt-hosted", "pool/x.txt", "GET, HEAD"}, {"GET", "apt-hosted", "", "POST"}, {"PUT", "apt-hosted", "public.key", "GET, HEAD"},
		{"PUT", "apt-proxy", "pool/x.deb", "GET, HEAD"}, {"POST", "apt-proxy", "", ""}, {"PUT", "apt-proxy-first", "pool/x.deb", "GET, HEAD"},
	} {
		if w := e.do(c.method, c.repo, c.path, []byte("x")); w.Code != 405 || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s/%s: %d Allow %q, want 405 %q", c.method, c.repo, c.path, w.Code, w.Header().Get("Allow"), c.allow)
		}
	}
	for _, p := range []string{"../x", "pool//x.deb", "a/./b", `a\b`} {
		if w := e.do("GET", "apt-hosted", p, nil); w.Code != 400 {
			t.Errorf("%q: %d, want 400", p, w.Code)
		}
	}
	if w := e.do("GET", "apt-hosted", "pool/main/h/hello/hello_9_amd64.deb", nil); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}
}

// writePolicy allow: a package can be replaced, by a user with Edit.
func TestOverwriteNeedsEdit(t *testing.T) {
	e := newEnv(t)
	a, ps := repotest.Principals(t, e.h.Store.DB, "apt-allow", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	v1 := deb(t, helloControl, "control.tar.gz")
	v1b := deb(t, strings.Replace(helloControl, "Section: utils", "Section: misc", 1), "control.tar.gz")
	if w := e.doAs(ps["adder"], "PUT", "apt-allow", "pool/hello.deb", v1); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["adder"], "PUT", "apt-allow", "pool/hello.deb", v1b); w.Code != 403 {
		t.Fatalf("add-only overwrite: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["editor"], "PUT", "apt-allow", "pool/hello.deb", v1b); w.Code != 201 {
		t.Fatalf("edit overwrite: %d %s", w.Code, w.Body)
	}
	_, _, s2 := sums(v1b)
	if p := e.text(t, "apt-allow", "dists/stable/main/binary-amd64/Packages"); !strings.Contains(p, "Section: misc\n") || !strings.Contains(p, "SHA256: "+s2+"\n") || strings.Count(p, "Package: ") != 1 {
		t.Fatalf("Packages after overwrite:\n%s", p)
	}
	if w := e.do("GET", "apt-allow", "pool/main/h/hello/hello_1.0_amd64.deb", nil); !bytes.Equal(w.Body.Bytes(), v1b) {
		t.Fatal("overwrite not stored")
	}
}

// POST /: a multipart form with the package in "file", through a group's writeMember.
func TestPostMultipart(t *testing.T) {
	e := newEnv(t)
	form := func(field string, pkg []byte) ([]byte, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile(field, "hello.deb")
		fw.Write(pkg)
		mw.Close()
		return buf.Bytes(), mw.FormDataContentType()
	}
	body, ct := form("file", deb(t, helloControl, "control.tar.gz"))
	if w := e.do("POST", "apt-all", "", body, "Content-Type", ct); w.Code != 201 || !strings.Contains(w.Body.String(), `"package":"hello"`) {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	if p := e.text(t, "apt-hosted", "dists/stable/main/binary-amd64/Packages"); !strings.HasPrefix(p, "Package: hello\n") {
		t.Fatalf("Packages:\n%s", p)
	}
	if w := e.do("POST", "apt-hosted", "", body, "Content-Type", ct); w.Code != 409 {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
	body, ct = form("package", deb(t, control("other", "1.0", "amd64"), "control.tar.gz"))
	if w := e.do("POST", "apt-hosted", "", body, "Content-Type", ct); w.Code != 400 || !strings.Contains(w.Body.String(), `"file" field`) {
		t.Fatalf("wrong field: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "apt-hosted", "", deb(t, control("other", "1.0", "amd64"), "control.tar.gz"), "Content-Type", "application/octet-stream"); w.Code != 400 || !strings.Contains(w.Body.String(), "multipart") {
		t.Fatalf("not multipart: %d %s", w.Code, w.Body)
	}
}

// A repository with a signing key writes Release.gpg and InRelease and
// serves its public key; the signatures are the pgp package's over the
// Release text (verified against gpgv in that package's tests).
func TestSigned(t *testing.T) {
	e := newEnv(t)
	key, err := pgp.ParsePrivateKey(testSigningKey(t))
	if err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "apt-signed", "public.key", nil)
	if w.Code != 200 || w.Body.String() != key.PublicKey() || w.Header().Get("Content-Type") != "application/pgp-keys" {
		t.Fatalf("public.key: %d %v", w.Code, w.Header())
	}
	check := func(stage string) {
		t.Helper()
		release := e.text(t, "apt-signed", "dists/jammy/Release")
		sig, _ := key.DetachSign([]byte(release), e.now)
		in, _ := key.ClearSign([]byte(release), e.now)
		w := e.do("GET", "apt-signed", "dists/jammy/Release.gpg", nil)
		if w.Code != 200 || w.Body.String() != sig || w.Header().Get("Content-Type") != "application/pgp-signature" {
			t.Fatalf("%s Release.gpg: %d %v\n%s", stage, w.Code, w.Header(), w.Body)
		}
		if got := e.text(t, "apt-signed", "dists/jammy/InRelease"); got != in || !strings.Contains(got, "\n\n"+release+"-----BEGIN PGP SIGNATURE-----\n") {
			t.Fatalf("%s InRelease:\n%s", stage, got)
		}
	}
	check("empty") // generated on the fly before the first upload
	if w := e.do("PUT", "apt-signed", "pool/hello.deb", deb(t, helloControl, "control.tar.gz")); w.Code != 201 || !strings.Contains(w.Body.String(), "pool/internal/h/hello/") {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	check("after upload")
	if rel := e.text(t, "apt-signed", "dists/jammy/Release"); !strings.Contains(rel, "\nComponents: internal\n") || !strings.Contains(rel, " internal/binary-amd64/Packages\n") {
		t.Fatalf("Release:\n%s", rel)
	}
	// the group serves the first member's key that has one
	if w := e.do("GET", "apt-proxy-first", "public.key", nil); w.Code != 200 || w.Body.String() != key.PublicKey() {
		t.Fatalf("group public.key: %d", w.Code)
	}
	if w := e.do("GET", "apt-all", "public.key", nil); w.Code != 404 {
		t.Fatalf("group without a key: %d", w.Code)
	}
	// a key that does not parse is a 500, not a silently unsigned repository
	e.repos["apt-signed"].SigningKey = "garbage"
	if w := e.do("GET", "apt-signed", "public.key", nil); w.Code != 500 {
		t.Fatalf("bad key: %d", w.Code)
	}
	if w := e.do("PUT", "apt-signed", "pool/x.deb", deb(t, control("xx", "1", "all"), "control.tar.gz")); w.Code != 500 {
		t.Fatalf("upload with a bad key: %d", w.Code)
	}
}

// A proxy caches what its remote serves at the same paths: dists/ as
// metadata, re-checked after metadataMaxAge, pool/ as content, for good.
func TestProxy(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 2; i++ {
		if got := e.text(t, "apt-proxy", "dists/bookworm/InRelease"); got != string(e.up["dists/bookworm/InRelease"]) {
			t.Fatalf("InRelease #%d:\n%s", i, got)
		}
	}
	if e.hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1 (cached)", e.hits.Load())
	}
	w := e.do("GET", "apt-proxy", "pool/main/u/upstream/upstream_2.0_amd64.deb", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.up["pool/main/u/upstream/upstream_2.0_amd64.deb"]) || w.Header().Get("Content-Type") != ctDeb {
		t.Fatalf("deb: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "apt-proxy", "dists/bookworm/Release.gpg", nil); w.Code != 404 {
		t.Fatalf("missing upstream file: %d", w.Code)
	}
	if w := e.do("GET", "apt-proxy", "dists/", nil); w.Code != 400 {
		t.Fatalf("directory path: %d", w.Code)
	}
	// after metadataMaxAge dists/ is fetched again, pool/ is not
	e.hits.Store(0)
	e.now = e.now.Add(2 * time.Hour)
	e.text(t, "apt-proxy", "dists/bookworm/InRelease")
	e.text(t, "apt-proxy", "pool/main/u/upstream/upstream_2.0_amd64.deb")
	if e.hits.Load() != 1 {
		t.Fatalf("hits after expiry %d, want 1 (the InRelease re-check)", e.hits.Load())
	}
	// the remote down: the expired metadata is served stale, cached content as is
	e.down.Store(true)
	e.now = e.now.Add(2 * time.Hour)
	w = e.do("GET", "apt-proxy", "dists/bookworm/InRelease", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Warning"), "Stale") {
		t.Fatalf("stale: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "apt-proxy", "pool/main/u/upstream/upstream_2.0_amd64.deb", nil); w.Code != 200 {
		t.Fatalf("cached deb while down: %d", w.Code)
	}
	if w := e.do("GET", "apt-proxy", "dists/bookworm/Release", nil); w.Code != 502 {
		t.Fatalf("uncached while down: %d", w.Code)
	}
	// a proxy's hosted-only paths: no generated metadata, no public key
	e.down.Store(false)
	for _, p := range []string{"dists/stable/Release", "public.key"} {
		if w := e.do("GET", "apt-proxy", p, nil); w.Code != 404 {
			t.Fatalf("%s on a proxy: %d", p, w.Code)
		}
	}
}

// A group answers from the first member that has the path (the owner
// rule of raw): a hosted member's dists/ and pool/ ahead of the proxy's.
func TestGroup(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "apt-all", "pool/hello.deb", deb(t, helloControl, "control.tar.gz")); w.Code != 201 {
		t.Fatalf("PUT through the group: %d %s", w.Code, w.Body)
	}
	if got := e.text(t, "apt-all", "dists/stable/Release"); !strings.HasPrefix(got, "Origin: apt-hosted\n") {
		t.Fatalf("group Release:\n%s", got)
	}
	if got := e.text(t, "apt-all", "dists/stable/main/binary-amd64/Packages"); !strings.HasPrefix(got, "Package: hello\n") {
		t.Fatalf("group Packages:\n%s", got)
	}
	if w := e.do("GET", "apt-all", "pool/main/h/hello/hello_1.0_amd64.deb", nil); w.Code != 200 {
		t.Fatalf("hosted member's deb: %d", w.Code)
	}
	// paths only the proxy member has
	if got := e.text(t, "apt-all", "dists/bookworm/Release"); got != "Origin: Debian\nSuite: stable\n" {
		t.Fatalf("proxy member's Release: %q", got)
	}
	if w := e.do("GET", "apt-all", "pool/main/u/upstream/upstream_2.0_amd64.deb", nil); w.Code != 200 {
		t.Fatalf("proxy member's deb: %d", w.Code)
	}
	// the proxy first: its 404 lets the hosted member after it answer
	if w := e.do("PUT", "apt-signed", "pool/x.deb", deb(t, control("xx", "1", "all"), "control.tar.gz")); w.Code != 201 {
		t.Fatal(w.Code)
	}
	if got := e.text(t, "apt-proxy-first", "dists/jammy/Release"); !strings.HasPrefix(got, "Origin: apt-signed\n") {
		t.Fatalf("proxy-first Release:\n%s", got)
	}
	if w := e.do("GET", "apt-all", "pool/nothere.deb", nil); w.Code != 404 {
		t.Fatalf("missing everywhere: %d", w.Code)
	}
	// a failing member ahead of the owner: 502 for what is not cached
	fresh := newEnv(t)
	fresh.down.Store(true)
	if w := fresh.do("GET", "apt-proxy-first", "dists/jammy/Release", nil); w.Code != 502 {
		t.Fatalf("failing member: %d %s", w.Code, w.Body)
	}
}

func TestDeleteComponent(t *testing.T) {
	e := newEnv(t)
	for _, c := range []string{helloControl, control("hello", "1.1", "amd64"), control("tool", "2.0", "all")} {
		if w := e.do("PUT", "apt-hosted", "pool/x.deb", deb(t, c, "control.tar.gz")); w.Code != 201 {
			t.Fatal(w.Code)
		}
	}
	e.sink.Reset()
	ctx := context.Background()
	rp := e.repos["apt-hosted"]
	res, err := e.h.DeleteComponent(ctx, rp, component.Component{Repo: "apt-hosted", Format: "apt", Group: "amd64", Name: "hello", Version: "1.0"}, "admin")
	if err != nil || res.Assets != 1 || res.Bytes == 0 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Kind != "delete" || got[0].Actor != "admin" || got[0].Subject != "amd64/hello@1.0" {
		t.Fatalf("events: %v", got)
	}
	if w := e.do("GET", "apt-hosted", "pool/main/h/hello/hello_1.0_amd64.deb", nil); w.Code != 404 {
		t.Fatalf("deleted deb: %d", w.Code)
	}
	if p := e.text(t, "apt-hosted", "dists/stable/main/binary-amd64/Packages"); strings.Contains(p, "Version: 1.0\n") || !strings.Contains(p, "Version: 1.1\n") || !strings.Contains(p, "Package: tool\n") {
		t.Fatalf("Packages after delete:\n%s", p)
	}
	// gone already: no error, no event
	if res, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "apt", Group: "amd64", Name: "hello", Version: "1.0"}, "admin"); err != nil || res.Assets != 0 || len(e.sink.Events()) != 1 {
		t.Fatalf("again: %+v %v %v", res, err, e.sink.Events())
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["apt-proxy"], component.Component{Format: "apt", Group: "amd64", Name: "hello", Version: "1.1"}, "admin"); err == nil {
		t.Fatal("proxy: want ErrNotSupported")
	}
	if _, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "apt", Name: "hello", Version: "1.1"}, "admin"); err == nil {
		t.Fatal("no architecture: want an error")
	}
	// the last amd64 package gone: binary-amd64 disappears, "all" stays
	if _, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "apt", Group: "amd64", Name: "hello", Version: "1.1"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "apt-hosted", "dists/stable/main/binary-amd64/Packages", nil); w.Code != 404 {
		t.Fatalf("binary-amd64 after the last amd64 package: %d", w.Code)
	}
	if rel := e.text(t, "apt-hosted", "dists/stable/Release"); !strings.Contains(rel, "\nArchitectures: all\n") || strings.Contains(rel, "binary-amd64") {
		t.Fatalf("Release:\n%s", rel)
	}
	// the last package of all: an empty Release remains, as before any upload
	if _, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "apt", Group: "all", Name: "tool", Version: "2.0"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := e.text(t, "apt-hosted", "dists/stable/Release"); got != e.releaseFor("apt-hosted", "stable", "main", nil, nil) {
		t.Fatalf("empty Release:\n%s", got)
	}
	// a component the evaluation saw differently is skipped
	if w := e.do("PUT", "apt-hosted", "pool/x.deb", deb(t, helloControl, "control.tar.gz")); w.Code != 201 {
		t.Fatal(w.Code)
	}
	c := component.Component{Format: "apt", Group: "amd64", Name: "hello", Version: "1.0",
		Evaluated: &component.Evaluated{Paths: map[string]bool{"pool/main/h/hello/hello_1.0_amd64.deb": true}, MaxCreated: e.now.Add(-time.Hour)}}
	if _, err := e.h.DeleteComponent(ctx, rp, c, "cleanup"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed component: %v", err)
	}
	// the asset path derives the same component the delete takes
	if g, n, v, ok := component.DeriveAsset("apt", "pool/main/h/hello/hello_1.0_amd64.deb"); !ok || g != "amd64" || n != "hello" || v != "1.0" {
		t.Fatalf("DeriveAsset: %q %q %q %v", g, n, v, ok)
	}
}

// Control files as the tools write them: CRLF, comments, continuation
// lines with tabs, an uncompressed control.tar.
func TestParseControl(t *testing.T) {
	f, err := parseControl([]byte("# comment\r\nPackage: aa\r\nVersion: 1\r\nArchitecture: all\r\nDescription: one\r\n\tcont\r\n .\r\n\r\nPackage: ignored\r\n"))
	if err != nil || f.get("package") != "aa" || f.get("Description") != "one\n\tcont\n ." || len(f) != 4 {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := parseControl([]byte(" cont\nPackage: aa\n")); err == nil {
		t.Fatal("leading continuation")
	}
	if _, err := parseControl(nil); err == nil {
		t.Fatal("empty")
	}
	r := newRow(fields{{"Package", "a"}, {"Description", "d"}, {"Version", "1"}, {"Architecture", "all"}}, "main", blob.Sums{Size: 3, MD5: "m", SHA1: "s", SHA256: "x"})
	if r.stanza() != "Package: a\nVersion: 1\nArchitecture: all\nFilename: pool/main/a/a/a_1_all.deb\nSize: 3\nMD5sum: m\nSHA1: s\nSHA256: x\nDescription: d\n" {
		t.Fatalf("stanza:\n%s", r.stanza())
	}
	if p := poolPath("main", "lib", "1", "all"); p != "pool/main/l/lib/lib_1_all.deb" {
		t.Fatalf("lib alone is not a library prefix: %s", p)
	}
}
