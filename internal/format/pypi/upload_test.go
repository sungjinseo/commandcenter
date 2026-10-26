package pypi

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// uploadBody builds what twine sends. Fields come before the file, as twine does.
func uploadBody(t *testing.T, fields map[string]string, filename, content string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, k := range []string{":action", "protocol_version", "name", "version", "filetype", "requires_python", "sha256_digest", "md5_digest"} {
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
	ct, body := uploadBody(t, goodFields("sdist-bytes"), "smoke_hello-1.0.0.tar.gz", "sdist-bytes")
	if w := e.upload("py-all", ct, body); w.Code != 200 {
		t.Fatalf("upload via group: %d %s", w.Code, w.Body.String())
	}
	w := e.do("GET", "py-hosted", "simple/smoke-hello/", "")
	if !strings.Contains(w.Body.String(), "/packages/smoke-hello/smoke_hello-1.0.0.tar.gz#sha256="+sha("sdist-bytes")) ||
		!strings.Contains(w.Body.String(), `data-requires-python="&gt;=3.8"`) {
		t.Fatalf("page: %s", w.Body.String())
	}
	if w := e.do("GET", "py-all", "packages/smoke-hello/smoke_hello-1.0.0.tar.gz", ""); w.Body.String() != "sdist-bytes" {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}

	ct, body = uploadBody(t, goodFields("sdist-bytes"), "smoke_hello-1.0.0.tar.gz", "sdist-bytes")
	w = e.upload("py-hosted", ct, body)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "File already exists") {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadRejects(t *testing.T) {
	e := newEnv(t)
	bad := goodFields("x")
	bad["sha256_digest"] = sha("other")
	cases := map[string]func() (string, *bytes.Buffer){
		"sha256 mismatch": func() (string, *bytes.Buffer) { return uploadBody(t, bad, "smoke_hello-1.0.0.tar.gz", "x") },
		"no content":      func() (string, *bytes.Buffer) { return uploadBody(t, goodFields("x"), "", "") },
		"wrong action": func() (string, *bytes.Buffer) {
			f := goodFields("x")
			f[":action"] = "remove_pkg"
			return uploadBody(t, f, "smoke_hello-1.0.0.tar.gz", "x")
		},
		"file of another project": func() (string, *bytes.Buffer) { return uploadBody(t, goodFields("x"), "other-1.0.0.tar.gz", "x") },
		"missing version": func() (string, *bytes.Buffer) {
			f := goodFields("x")
			delete(f, "version")
			return uploadBody(t, f, "smoke_hello-1.0.0.tar.gz", "x")
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
	for k, v := range goodFields("x") {
		mw.WriteField(k, v)
	}
	mw.WriteField("description", desc)
	fw, _ := mw.CreateFormFile("content", "smoke_hello-1.0.0.tar.gz")
	fw.Write([]byte("x"))
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
	f := goodFields("x")
	f["md5_digest"] = strings.Repeat("0", 32)
	ct, body := uploadBody(t, f, "smoke_hello-1.0.0.tar.gz", "x")
	if w := e.upload("py-hosted", ct, body); w.Code != 400 {
		t.Errorf("md5 mismatch: %d", w.Code)
	}
}

func TestUploadContentFirst(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("content", "smoke_hello-1.0.0.tar.gz")
	fw.Write([]byte("x"))
	for k, v := range goodFields("x") {
		mw.WriteField(k, v)
	}
	mw.Close()
	if w := e.upload("py-hosted", mw.FormDataContentType(), &buf); w.Code != 200 {
		t.Fatalf("content first: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadDuplicateViaGroup(t *testing.T) {
	e := newEnv(t)
	ct, body := uploadBody(t, goodFields("x"), "smoke_hello-1.0.0.tar.gz", "x")
	if w := e.upload("py-all", ct, body); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	ct, body = uploadBody(t, goodFields("x"), "smoke_hello-1.0.0.tar.gz", "x")
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
	ct, body := uploadBody(t, goodFields("x"), "smoke_hello-1.0.0.tar.gz", "x")
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
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
	f := goodFields("a")
	f["name"] = "qa_sec_shadow"
	f["version"] = "1.0"
	ct, body := uploadBody(t, f, "qa_sec_shadow-1.0.tar.gz", "a")
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	f = goodFields("b")
	f["name"] = "QA.SEC.SHADOW"
	f["version"] = "1.0"
	ct, body = uploadBody(t, f, "QA.SEC.SHADOW-1.0.tar.gz", "b")
	if w := e.upload("py-hosted", ct, body); w.Code != 400 {
		t.Fatalf("respelled duplicate: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadRejectsBadExtensionsAndVersions(t *testing.T) {
	e := newEnv(t)
	up := func(name, version, file string) int {
		f := goodFields("x")
		f["name"] = name
		f["version"] = version
		ct, body := uploadBody(t, f, file, "x")
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
		{"1.0.0", "qa_x-1.0.0-py3-none-any.whl"}, {"1.0.0", "qa_x-1.0.0.tar.gz"}, {"1.0.0", "QA-X-1.0.0.zip"},
	} {
		if code := up("qa_x", c.v, c.file); code != 200 {
			t.Errorf("%s: %d", c.file, code)
		}
	}
}

func TestRequiresPythonFromWheelMetadata(t *testing.T) {
	e := newEnv(t)
	whl := string(wheelBytes(t, map[string]string{"qa_w-1.0.dist-info/METADATA": "Name: qa-w\nRequires-Python: >=3.99\n"}))
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
	bad := goodFields("x")
	bad["requires_python"] = `>=3"><script>`
	ct, body = uploadBody(t, bad, "smoke_hello-1.0.0.tar.gz", "x")
	if w := e.upload("py-hosted", ct, body); w.Code != 400 {
		t.Fatalf("invalid specifier: %d", w.Code)
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
		f := goodFields("x")
		f["name"] = "pkg"
		f["version"] = v
		ct, body := uploadBody(t, f, file, "x")
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
	if w := upload("1.0", "Metadata-Version: 2.1\nName: x\nRequires-Python: >=3.11\n\n"+body); w.Code != 200 {
		t.Fatalf("3 MiB body: %d %s", w.Code, w.Body.String())
	}
	big := strings.Repeat("a", maxMetadata+10)
	if w := upload("2.0", "Metadata-Version: 2.1\nSummary: "+big+"\nRequires-Python: >=3.12\n\n"); w.Code != 200 {
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
