package pypi

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// uploadBody builds what twine sends. Fields come before the file, as twine does.
func uploadBody(t *testing.T, fields map[string]string, filename, content string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, k := range []string{":action", "protocol_version", "name", "version", "filetype", "requires_python", "sha256_digest", "md5_digest", "blake2_256_digest"} {
		if v, ok := fields[k]; ok {
			mw.WriteField(k, v)
		}
	}
	if filename != "" {
		fw, _ := mw.CreateFormFile("content", filename)
		fw.Write([]byte(content))
	}
	mw.Close()
	return mw.FormDataContentType(), &buf
}

// dist builds a minimal valid distribution file whose metadata names name
// and version, plus extra header lines: a wheel's
// <name>-<version>.dist-info/METADATA, or an sdist's <base>/PKG-INFO in a
// gzipped tar or a zip, as file's extension says.
func dist(t *testing.T, name, version, file string, extra ...string) string {
	t.Helper()
	md := "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n" + strings.Join(extra, "")
	base, ext, _ := splitUploadExt(file)
	switch ext {
	case ".whl":
		f := strings.Split(base, "-")
		return string(wheelBytes(t, map[string]string{f[0] + "-" + f[1] + ".dist-info/METADATA": md}))
	case ".zip":
		return string(wheelBytes(t, map[string]string{base + "/PKG-INFO": md}))
	}
	return string(sdistBytes(t, map[string]string{base + "/PKG-INFO": md}))
}

// smoke is the sdist smoke_hello-1.0.0.tar.gz that goodFields describes.
func smoke(t *testing.T) string { return dist(t, "Smoke_Hello", "1.0.0", "smoke_hello-1.0.0.tar.gz") }

func goodFields(content string) map[string]string {
	m := md5.Sum([]byte(content))
	return map[string]string{
		":action": "file_upload", "protocol_version": "1", "name": "Smoke_Hello", "version": "1.0.0",
		"filetype": "sdist", "requires_python": ">=3.8", "sha256_digest": sha(content), "md5_digest": hex.EncodeToString(m[:]),
	}
}

func (e *env) upload(repoName, ct string, body *bytes.Buffer) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repository/"+repoName+"/", body)
	r.Header.Set("Content-Type", ct)
	e.h.Serve(w, r, e.repos[repoName], "")
	return w
}

func TestUploadThenInstall(t *testing.T) {
	e := newEnv(t)
	sd := smoke(t)
	ct, body := uploadBody(t, goodFields(sd), "smoke_hello-1.0.0.tar.gz", sd)
	if w := e.upload("py-all", ct, body); w.Code != 200 {
		t.Fatalf("upload via group: %d %s", w.Code, w.Body.String())
	}
	w := e.do("GET", "py-hosted", "simple/smoke-hello/", "")
	if !strings.Contains(w.Body.String(), "/packages/smoke-hello/smoke_hello-1.0.0.tar.gz#sha256="+sha(sd)) ||
		!strings.Contains(w.Body.String(), `data-requires-python="&gt;=3.8"`) {
		t.Fatalf("page: %s", w.Body.String())
	}
	if w := e.do("GET", "py-all", "packages/smoke-hello/smoke_hello-1.0.0.tar.gz", ""); w.Body.String() != sd {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}

	ct, body = uploadBody(t, goodFields(sd), "smoke_hello-1.0.0.tar.gz", sd)
	w = e.upload("py-hosted", ct, body)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "File already exists") {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadRejects(t *testing.T) {
	e := newEnv(t)
	sd := smoke(t)
	bad := goodFields(sd)
	bad["sha256_digest"] = sha("other")
	cases := map[string]func() (string, *bytes.Buffer){
		"sha256 mismatch": func() (string, *bytes.Buffer) { return uploadBody(t, bad, "smoke_hello-1.0.0.tar.gz", sd) },
		"no content":      func() (string, *bytes.Buffer) { return uploadBody(t, goodFields(sd), "", "") },
		"wrong action": func() (string, *bytes.Buffer) {
			f := goodFields(sd)
			f[":action"] = "remove_pkg"
			return uploadBody(t, f, "smoke_hello-1.0.0.tar.gz", sd)
		},
		"file of another project": func() (string, *bytes.Buffer) { return uploadBody(t, goodFields(sd), "other-1.0.0.tar.gz", sd) },
		"missing version": func() (string, *bytes.Buffer) {
			f := goodFields(sd)
			delete(f, "version")
			return uploadBody(t, f, "smoke_hello-1.0.0.tar.gz", sd)
		},
	}
	for name, mk := range cases {
		ct, body := mk()
		if w := e.upload("py-hosted", ct, body); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := e.upload("py-hosted", "text/plain", bytes.NewBufferString("hi")); w.Code != 400 {
		t.Errorf("not multipart: %d", w.Code)
	}
	if w := e.do("GET", "py-hosted", "simple/smoke-hello/", ""); w.Code != 404 {
		t.Fatalf("rejected uploads must leave nothing: %d", w.Code)
	}
	big := goodFields("x")
	big["requires_python"] = strings.Repeat("x", 5<<10)
	ct, body := uploadBody(t, big, "smoke_hello-1.0.0.tar.gz", "x")
	if w := e.upload("py-hosted", ct, body); w.Code != 413 {
		t.Errorf("oversized used field: %d", w.Code)
	}
}

// uploadWithDescription builds a twine-like upload with an extra description field.
func uploadWithDescription(t *testing.T, desc string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	sd := smoke(t)
	for k, v := range goodFields(sd) {
		mw.WriteField(k, v)
	}
	mw.WriteField("description", desc)
	fw, _ := mw.CreateFormFile("content", "smoke_hello-1.0.0.tar.gz")
	fw.Write([]byte(sd))
	mw.Close()
	return mw.FormDataContentType(), &buf
}

func TestUploadLargeDescription(t *testing.T) {
	e := newEnv(t)
	ct, body := uploadWithDescription(t, strings.Repeat("d", 200<<10))
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("200 KB description: %d %s", w.Code, w.Body.String())
	}
	e = newEnv(t)
	ct, body = uploadWithDescription(t, strings.Repeat("d", 20<<20))
	if w := e.upload("py-hosted", ct, body); w.Code != 413 {
		t.Errorf("20 MiB description: %d", w.Code)
	}
}

func TestUploadFieldCaps(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i < 300; i++ {
		mw.WriteField("f"+strconv.Itoa(i), "")
	}
	mw.Close()
	if w := e.upload("py-hosted", mw.FormDataContentType(), &buf); w.Code != 413 {
		t.Errorf("too many parts: %d", w.Code)
	}
}

func TestUploadTruncated(t *testing.T) {
	e := newEnv(t)
	content := strings.Repeat("abcdefgh", 1000)
	ct, body := uploadBody(t, goodFields(content), "smoke_hello-1.0.0.tar.gz", content)
	cut := bytes.NewBuffer(body.Bytes()[:body.Len()-3000])
	if w := e.upload("py-hosted", ct, cut); w.Code != 400 {
		t.Errorf("truncated: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadMD5Mismatch(t *testing.T) {
	e := newEnv(t)
	sd := smoke(t)
	f := goodFields(sd)
	f["md5_digest"] = strings.Repeat("0", 32)
	ct, body := uploadBody(t, f, "smoke_hello-1.0.0.tar.gz", sd)
	if w := e.upload("py-hosted", ct, body); w.Code != 400 || !strings.Contains(w.Body.String(), "md5_digest") {
		t.Errorf("md5 mismatch: %d %s", w.Code, w.Body.String())
	}
}

// QA L-18: twine's blake2_256_digest is checked like the other digests.
func TestUploadBlake2Digest(t *testing.T) {
	e := newEnv(t)
	sd := smoke(t)
	sum := blake2b.Sum256([]byte(sd))
	for _, d := range []string{"0", "zz", strings.Repeat("0", 64), hex.EncodeToString(sum[:31]) + "00"} {
		f := goodFields(sd)
		f["blake2_256_digest"] = d
		ct, body := uploadBody(t, f, "smoke_hello-1.0.0.tar.gz", sd)
		if w := e.upload("py-hosted", ct, body); w.Code != 400 || !strings.Contains(w.Body.String(), "blake2_256_digest") {
			t.Errorf("blake2_256_digest %q: %d %s", d, w.Code, w.Body.String())
		}
	}
	f := goodFields(sd)
	f["blake2_256_digest"] = strings.ToUpper(hex.EncodeToString(sum[:]))
	ct, body := uploadBody(t, f, "smoke_hello-1.0.0.tar.gz", sd)
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("matching blake2_256_digest: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadContentFirst(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	sd := smoke(t)
	fw, _ := mw.CreateFormFile("content", "smoke_hello-1.0.0.tar.gz")
	fw.Write([]byte(sd))
	for k, v := range goodFields(sd) {
		mw.WriteField(k, v)
	}
	mw.Close()
	if w := e.upload("py-hosted", mw.FormDataContentType(), &buf); w.Code != 200 {
		t.Fatalf("content first: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadDuplicateViaGroup(t *testing.T) {
	e := newEnv(t)
	sd := smoke(t)
	ct, body := uploadBody(t, goodFields(sd), "smoke_hello-1.0.0.tar.gz", sd)
	if w := e.upload("py-all", ct, body); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	ct, body = uploadBody(t, goodFields(sd), "smoke_hello-1.0.0.tar.gz", sd)
	if w := e.upload("py-all", ct, body); w.Code != 400 || !strings.Contains(w.Body.String(), "File already exists") {
		t.Errorf("dup via group: %d %s", w.Code, w.Body.String())
	}
}

// failingBody yields data, then fails with err.
type failingBody struct {
	data []byte
	err  error
}

func (f *failingBody) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestUploadBlobErrorMapping(t *testing.T) {
	e := newEnv(t)
	content := strings.Repeat("abcdefgh", 1000)
	ct, body := uploadBody(t, goodFields(content), "smoke_hello-1.0.0.tar.gz", content)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repository/py-hosted/", &failingBody{data: body.Bytes()[:body.Len()-3000], err: errors.New("disk exploded")})
	r.Header.Set("Content-Type", ct)
	e.h.Serve(w, r, e.repos["py-hosted"], "")
	if w.Code != 500 {
		t.Errorf("non-client error: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadDuplicateRejectedBeforeStreaming(t *testing.T) {
	e := newEnv(t)
	sd := smoke(t)
	ct, body := uploadBody(t, goodFields(sd), "smoke_hello-1.0.0.tar.gz", sd)
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	// The second body breaks inside the content part; only an early duplicate
	// check can answer 400 "File already exists." without touching the stream.
	content := strings.Repeat("abcdefgh", 1000)
	ct, body = uploadBody(t, goodFields(content), "smoke_hello-1.0.0.tar.gz", content)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repository/py-hosted/", &failingBody{data: body.Bytes()[:body.Len()-3000], err: errors.New("disk exploded")})
	r.Header.Set("Content-Type", ct)
	e.h.Serve(w, r, e.repos["py-hosted"], "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "File already exists") {
		t.Errorf("early duplicate: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadWritePolicyDeny(t *testing.T) {
	e := newEnv(t)
	ct, body := uploadBody(t, goodFields("x"), "smoke_hello-1.0.0.tar.gz", "x")
	w := e.upload("py-ro", ct, body)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "py-ro", "simple/smoke-hello/", ""); w.Code != 404 {
		t.Errorf("denied upload left data: %d", w.Code)
	}
}

func TestUploadOverGlobalCapInIgnoredFieldIs413(t *testing.T) {
	e := newEnv(t)
	ct, body := uploadWithDescription(t, strings.Repeat("d", 50<<10))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repository/py-hosted/", body)
	r.Header.Set("Content-Type", ct)
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	e.h.Serve(w, r, e.repos["py-hosted"], "")
	if w.Code != 413 {
		t.Fatalf("status %d %s, want 413", w.Code, w.Body.String())
	}
}

func TestUploadRejectsCanonicalDuplicate(t *testing.T) {
	e := newEnv(t)
	a := dist(t, "qa_sec_shadow", "1.0", "qa_sec_shadow-1.0.tar.gz")
	f := goodFields(a)
	f["name"] = "qa_sec_shadow"
	f["version"] = "1.0"
	ct, body := uploadBody(t, f, "qa_sec_shadow-1.0.tar.gz", a)
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	b := dist(t, "QA.SEC.SHADOW", "1.0", "QA.SEC.SHADOW-1.0.tar.gz")
	f = goodFields(b)
	f["name"] = "QA.SEC.SHADOW"
	f["version"] = "1.0"
	ct, body = uploadBody(t, f, "QA.SEC.SHADOW-1.0.tar.gz", b)
	if w := e.upload("py-hosted", ct, body); w.Code != 400 || !strings.Contains(w.Body.String(), `File already exists: this release already has "qa_sec_shadow-1.0.tar.gz"`) {
		t.Fatalf("respelled duplicate: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadRejectsBadExtensionsAndVersions(t *testing.T) {
	e := newEnv(t)
	up := func(name, version, file string) int {
		content := dist(t, name, version, file)
		f := goodFields(content)
		f["name"] = name
		f["version"] = version
		ct, body := uploadBody(t, f, file, content)
		return e.upload("py-hosted", ct, body).Code
	}
	for _, c := range []struct{ v, file string }{
		{"4.0.0", "qa_x-4.0.0.html"}, {"4.0.0", "qa_x-4.0.0"}, {"2.0.0", "qa_x-2.0.0.tar"},
		{"3.0.0", "qa_x-3.0.0 .tar.gz"}, {"1.0", "x.whl.asc"}, {"1.0ü", "qa_x-1.0ü.tar.gz"},
		{"9.9", "qa_x-1.0.tar.gz"}, // form version differs from the file's
	} {
		if code := up("qa_x", c.v, c.file); code != 400 {
			t.Errorf("%s (version %q): %d", c.file, c.v, code)
		}
	}
	for _, c := range []struct{ v, file string }{
		{"1.0.0", "qa_x-1.0.0-py3-none-any.whl"}, {"1.0.0", "qa_x-1.0.0.tar.gz"}, {"2.0", "QA-X-2.0.zip"},
	} {
		if code := up("qa_x", c.v, c.file); code != 200 {
			t.Errorf("%s: %d", c.file, code)
		}
	}
	// QA M-8: this used to pass; a release has one sdist (PEP 527), .tar.gz or .zip.
	if code := up("qa_x", "1.0.0", "QA-X-1.0.0.zip"); code != 400 {
		t.Errorf("second sdist of a release: %d", code)
	}
}

func TestRequiresPythonFromWheelMetadata(t *testing.T) {
	e := newEnv(t)
	whl := string(wheelBytes(t, map[string]string{"qa_w-1.0.dist-info/METADATA": "Name: qa-w\nVersion: 1.0\nRequires-Python: >=3.99\n"}))
	f := goodFields(whl)
	f["name"] = "qa_w"
	f["version"] = "1.0"
	delete(f, "requires_python")
	ct, body := uploadBody(t, f, "qa_w-1.0-py3-none-any.whl", whl)
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "py-hosted", "simple/qa-w/", ""); !strings.Contains(w.Body.String(), `data-requires-python="&gt;=3.99"`) {
		t.Fatalf("index: %s", w.Body.String())
	}
	sd := smoke(t)
	bad := goodFields(sd)
	bad["requires_python"] = `>=3"><script>`
	ct, body = uploadBody(t, bad, "smoke_hello-1.0.0.tar.gz", sd)
	if w := e.upload("py-hosted", ct, body); w.Code != 400 {
		t.Fatalf("invalid specifier: %d", w.Code)
	}
}

// QA N-9: an invalid Requires-Python in the file's metadata is a 400, like
// the form's; a valid one wins over the form's.
func TestRequiresPythonFromMetadataIsChecked(t *testing.T) {
	e := newEnv(t)
	up := func(version, rp string) *httptest.ResponseRecorder {
		file := "smoke_hello-" + version + ".tar.gz"
		content := dist(t, "smoke-hello", version, file, "Requires-Python: "+rp+"\n")
		f := goodFields(content)
		f["version"] = version
		ct, body := uploadBody(t, f, file, content)
		return e.upload("py-hosted", ct, body)
	}
	for i, rp := range []string{"banana", `>=3"><script>`, "3.8"} {
		if w := up("1."+strconv.Itoa(i), rp); w.Code != 400 || !strings.Contains(w.Body.String(), "Requires-Python") {
			t.Errorf("%q: %d %s", rp, w.Code, w.Body.String())
		}
	}
	if w := up("2.0", ">=3.9, <4"); w.Code != 200 {
		t.Fatalf("valid: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "py-hosted", "simple/smoke-hello/", ""); strings.Count(w.Body.String(), "<a ") != 1 || !strings.Contains(w.Body.String(), `data-requires-python="&gt;=3.9, &lt;4"`) {
		t.Fatalf("index: %s", w.Body.String())
	}
}

func TestLegacyOddFilenameStillServed(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "legacy", "Legacy_Odd.File.tar.gz.asc", "old")
	if w := e.do("GET", "py-hosted", "simple/legacy/", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Legacy_Odd.File.tar.gz.asc") {
		t.Fatalf("legacy: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "py-hosted", "packages/legacy/Legacy_Odd.File.tar.gz.asc", ""); w.Body.String() != "old" {
		t.Fatalf("legacy download: %d", w.Code)
	}
}

func TestUploadRejectsEquivalentVersionSpelling(t *testing.T) {
	e := newEnv(t)
	up := func(v, file string) int {
		content := dist(t, "pkg", v, file)
		f := goodFields(content)
		f["name"] = "pkg"
		f["version"] = v
		ct, body := uploadBody(t, f, file, content)
		return e.upload("py-hosted", ct, body).Code
	}
	if c := up("1.0", "pkg-1.0.tar.gz"); c != 200 {
		t.Fatalf("first: %d", c)
	}
	if c := up("1.0.0", "pkg-1.0.0.tar.gz"); c != 400 {
		t.Fatalf("1.0.0 after 1.0: %d", c)
	}
	if c := up("1.0", "pkg-1.0-1-py3-none-any.whl"); c != 200 { // distinct wheel build tag
		t.Fatalf("wheel: %d", c)
	}
}

// Final review I2: only the RFC 822 header block of METADATA is read, so a
// large long_description body (big README) never refuses the upload; a header
// block over the cap is ignored and the form's requires_python is kept.
func TestLargeMetadataBodyUploads(t *testing.T) {
	e := newEnv(t)
	upload := func(file, metadata string) *httptest.ResponseRecorder {
		t.Helper()
		whl := string(wheelBytes(t, map[string]string{"x-" + file + ".dist-info/METADATA": metadata}))
		f := goodFields(whl)
		f["name"], f["version"], f["filetype"] = "x", file, "bdist_wheel"
		ct, body := uploadBody(t, f, "x-"+file+"-py3-none-any.whl", whl)
		return e.upload("py-hosted", ct, body)
	}
	body := strings.Repeat("README line\n", (3<<20)/12)
	if w := upload("1.0", "Metadata-Version: 2.1\nName: x\nVersion: 1.0\nRequires-Python: >=3.11\n\n"+body); w.Code != 200 {
		t.Fatalf("3 MiB body: %d %s", w.Code, w.Body.String())
	}
	big := strings.Repeat("a", maxMetadata+10)
	if w := upload("2.0", "Metadata-Version: 2.1\nName: x\nVersion: 2.0\nSummary: "+big+"\nRequires-Python: >=3.12\n\n"); w.Code != 200 {
		t.Fatalf("oversized header block: %d %s", w.Code, w.Body.String())
	}
	w := e.do("GET", "py-hosted", "simple/x/", "")
	for _, want := range []string{
		`x-1.0-py3-none-any.whl#sha256=`, `data-requires-python="&gt;=3.11">x-1.0-py3-none-any.whl`,
		`data-requires-python="&gt;=3.8">x-2.0-py3-none-any.whl`, // header block ignored: form value
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("index lacks %q: %s", want, w.Body.String())
		}
	}
}

// QA N-8: a multipart body the server's IdleBody cut for stalling is a 408,
// not a 400.
func TestUploadStalledBodyIs408(t *testing.T) {
	e := newEnv(t)
	ct, body := uploadBody(t, goodFields("x"), "smoke_hello-1.0.0.tar.gz", "x")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repository/py-hosted/", &failingBody{data: body.Bytes()[:40], err: &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}})
	r.Header.Set("Content-Type", ct)
	e.h.Serve(w, r, e.repos["py-hosted"], "")
	if w.Code != 408 {
		t.Fatalf("status %d %s, want 408", w.Code, w.Body.String())
	}
}

// QA M-9: the archive must be a distribution of the release in the form:
// text, a wrong archive and another project's or version's metadata are 400s
// and leave nothing behind.
func TestUploadChecksArchiveContent(t *testing.T) {
	e := newEnv(t)
	whl := "smoke_hello-1.0.0-py3-none-any.whl"
	for name, c := range map[string]struct{ file, content, want string }{
		"text as wheel":            {whl, "x", "not a zip archive"},
		"text as sdist":            {"smoke_hello-1.0.0.tar.gz", "x", "not a gzip file"},
		"text as zip sdist":        {"smoke_hello-1.0.0.zip", "x", "not a zip archive"},
		"sdist named as a wheel":   {whl, smoke(t), "not a zip archive"},
		"another project's wheel":  {whl, dist(t, "other", "1.0.0", whl), `metadata is for "other" "1.0.0"`},
		"another version's wheel":  {whl, dist(t, "smoke_hello", "1.0.1", whl), `metadata is for "smoke_hello" "1.0.1"`},
		"another project's sdist":  {"smoke_hello-1.0.0.tar.gz", dist(t, "other", "1.0.0", "smoke_hello-1.0.0.tar.gz"), `metadata is for "other"`},
		"sdist of another release": {"smoke_hello-1.0.0.tar.gz", string(sdistBytes(t, map[string]string{"smoke_hello-0.9/PKG-INFO": "Name: smoke_hello\nVersion: 1.0.0\n"})), "PKG-INFO is in"},
		"wheel without METADATA":   {whl, string(wheelBytes(t, map[string]string{"smoke_hello/__init__.py": ""})), "no *.dist-info/METADATA"},
		"METADATA without Version": {whl, string(wheelBytes(t, map[string]string{"smoke_hello-1.0.0.dist-info/METADATA": "Name: smoke_hello\n"})), "no Version"},
		"two .dist-info dirs":      {whl, string(wheelBytes(t, map[string]string{"smoke_hello-1.0.0.dist-info/METADATA": "Name: smoke_hello\nVersion: 1.0.0\n", "evil-1.0.dist-info/METADATA": "Name: evil\nVersion: 1.0\n"})), "more than one .dist-info"},
	} {
		ct, body := uploadBody(t, goodFields(c.content), c.file, c.content)
		if w := e.upload("py-hosted", ct, body); w.Code != 400 || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "py-hosted", "simple/smoke-hello/", ""); w.Code != 404 {
		t.Fatalf("rejected uploads must leave nothing: %d %s", w.Code, w.Body.String())
	}
}

// QA M-8: the same wheel tag set or build number spelled differently, and a
// second sdist of a release, are the same file; other tags are not.
func TestUploadRejectsFileNameVariants(t *testing.T) {
	e := newEnv(t)
	up := func(file string) *httptest.ResponseRecorder {
		content := dist(t, "Smoke_Hello", "1.0.0", file)
		ct, body := uploadBody(t, goodFields(content), file, content)
		return e.upload("py-hosted", ct, body)
	}
	for _, c := range [][2]string{
		{"smoke_hello-1.0.0-py3-none-any.whl", "smoke_hello-1.0.0-py3.py3-none-any.whl"},
		{"smoke_hello-1.0.0-1-py3-none-any.whl", "smoke_hello-1.0.0-01-py3-none-any.whl"},
		{"smoke_hello-1.0.0.tar.gz", "smoke_hello-1.0.0.zip"},
	} {
		if w := up(c[0]); w.Code != 200 {
			t.Fatalf("%s: %d %s", c[0], w.Code, w.Body.String())
		}
		if w := up(c[1]); w.Code != 400 || !strings.Contains(w.Body.String(), "File already exists: this release already has "+strconv.Quote(c[0])) {
			t.Errorf("%s after %s: %d %s", c[1], c[0], w.Code, w.Body.String())
		}
	}
	for _, f := range []string{"smoke_hello-1.0.0-py2.py3-none-any.whl", "smoke_hello-1.0.0-2-py3-none-any.whl"} {
		if w := up(f); w.Code != 200 {
			t.Errorf("distinct wheel %s: %d %s", f, w.Code, w.Body.String())
		}
	}
	for _, f := range []string{"smoke_hello-1.0.0.whl", "smoke_hello-1.0.0-py3-none.whl", "smoke_hello-1.0.0-b1-py3-none-any.whl", "smoke_hello-1.0.0-py3.-none-any.whl"} {
		if w := up(f); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid wheel file name") {
			t.Errorf("%s: %d %s", f, w.Code, w.Body.String())
		}
	}
}

// QA N-7: every PEP 440 spelling is accepted and stored in normal form; form
// and file name may spell a version differently; one release spelled two ways
// is one release; a leading "v" is an invalid version, not another project.
func TestUploadPEP440Versions(t *testing.T) {
	e := newEnv(t)
	up := func(version, file string) *httptest.ResponseRecorder {
		content := dist(t, "qa_x", version, file)
		f := goodFields(content)
		f["name"], f["version"] = "qa_x", version
		ct, body := uploadBody(t, f, file, content)
		return e.upload("py-hosted", ct, body)
	}
	want := map[string]string{}
	for v, norm := range map[string]string{"1.0-1": "1.0.post1", "01.2": "1.2", "2.0a": "2.0a0", "1.0post2": "1.0.post2", "1.0.dev": "1.0.dev0", "1.0RC2": "1.0rc2"} {
		if w := up(v, "qa_x-"+v+".tar.gz"); w.Code != 200 {
			t.Errorf("%s: %d %s", v, w.Code, w.Body.String())
		}
		want["qa_x-"+v+".tar.gz"] = norm
	}
	if w := up("1.0-1", "qa_x-1.0.post1-py3-none-any.whl"); w.Code != 200 {
		t.Errorf("wheel of 1.0-1: %d %s", w.Code, w.Body.String())
	}
	want["qa_x-1.0.post1-py3-none-any.whl"] = "1.0.post1"
	pkgs, err := e.h.Store.DB.ListPackages(context.Background(), "py-hosted", "qa-x")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		var a fileAttrs
		if err := json.Unmarshal([]byte(p.Attrs), &a); err != nil || a.Version != want[p.Version] {
			t.Errorf("%s stored version %q, want %q (%v)", p.Version, a.Version, want[p.Version], err)
		}
		delete(want, p.Version)
	}
	if len(want) != 0 {
		t.Errorf("not stored: %v", want)
	}
	if w := up("1.0.post1", "qa_x-1.0.post1.zip"); w.Code != 400 || !strings.Contains(w.Body.String(), `File already exists: this release already has "qa_x-1.0-1.tar.gz"`) {
		t.Errorf("1.0.post1 sdist after 1.0-1: %d %s", w.Code, w.Body.String())
	}
	for _, c := range [][2]string{{"v1.3", "qa_x-v1.3.tar.gz"}, {"1.3", "qa_x-v1.3.tar.gz"}, {"v1.3", "qa_x-1.3.tar.gz"}} {
		if w := up(c[0], c[1]); w.Code != 400 || !strings.Contains(w.Body.String(), `invalid version "v1.3"`) {
			t.Errorf("version %s, file %s: %d %s", c[0], c[1], w.Code, w.Body.String())
		}
	}
}
