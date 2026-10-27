package blob_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/blob/s3test"
)

// The GET Object example of "Signature Calculations for the Authorization
// Header: Transferring Payload in a Single Chunk" (AWS S3 API reference).
func TestSignV4KnownAnswer(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	r.Header.Set("Range", "bytes=0-9")
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	blob.SignV4(r, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "", "us-east-1",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", at)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := r.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n%s\nwant\n%s", got, want)
	}
	if r.Header.Get("x-amz-date") != "20130524T000000Z" {
		t.Fatalf("x-amz-date = %q", r.Header.Get("x-amz-date"))
	}
}

func openS3(t *testing.T, srv *s3test.Server, mutate func(*blob.S3Config)) *blob.Store {
	t.Helper()
	cfg := srv.Config()
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := blob.OpenS3(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func count(log []string, substr string) int {
	n := 0
	for _, l := range log {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// An object over the part size goes up in parts, with the SSE header on the
// multipart initiation, and reads back whole; one under it is one PUT.
func TestS3Multipart(t *testing.T) {
	old := *blob.S3PartSize
	*blob.S3PartSize = 5
	t.Cleanup(func() { *blob.S3PartSize = old })
	srv := s3test.New(t)
	s := openS3(t, srv, func(c *blob.S3Config) { c.Prefix = "/cc/blobs/"; c.SSE = "AES256" })
	sums, err := s.Write(strings.NewReader("twelve bytes"))
	if err != nil {
		t.Fatal(err)
	}
	key := "cc/blobs/" + sums.SHA256
	if got, ok := srv.Object(key); !ok || string(got) != "twelve bytes" {
		t.Fatalf("stored %q, %v", got, ok)
	}
	if srv.SSE(key) != "AES256" {
		t.Fatalf("sse = %q", srv.SSE(key))
	}
	log := srv.Log()
	if count(log, "partNumber=") != 3 || count(log, "?uploads=") != 1 || count(log, "POST /test/"+key+"?uploadId=") != 1 {
		t.Fatalf("multipart requests:\n%s", strings.Join(log, "\n"))
	}
	if _, err := s.Write(strings.NewReader("tiny")); err != nil {
		t.Fatal(err)
	}
	if log := srv.Log()[len(log):]; len(log) != 1 || !strings.HasPrefix(log[0], "PUT /test/cc/blobs/") {
		t.Fatalf("small write: %v", log)
	}
	f, err := s.Open(sums.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if b, _ := io.ReadAll(f); string(b) != "twelve bytes" {
		t.Fatalf("read back %q", b)
	}
}

// A failed part aborts the upload and stores nothing.
func TestS3MultipartAborts(t *testing.T) {
	old := *blob.S3PartSize
	*blob.S3PartSize = 5
	t.Cleanup(func() { *blob.S3PartSize = old })
	srv := s3test.New(t)
	s := openS3(t, srv, nil)
	if _, err := s.Write(io.MultiReader(strings.NewReader("twelve bytes"), failingReader{})); err == nil {
		t.Fatal("expected error")
	}
	if len(srv.Keys()) != 0 || count(srv.Log(), "?uploads=") != 0 {
		t.Fatalf("a failed read uploaded: %v %v", srv.Keys(), srv.Log())
	}
	srv.Full = true
	if _, err := s.Write(strings.NewReader("twelve bytes")); !errors.Is(err, blob.ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull", err)
	}
	if _, err := s.Write(strings.NewReader("tiny")); !errors.Is(err, blob.ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull", err)
	}
}

// Seek, ReadAt and http.ServeContent's Range handling become Range GETs.
func TestS3RangeReads(t *testing.T) {
	srv := s3test.New(t)
	s := openS3(t, srv, nil)
	sums, err := s.Write(strings.NewReader("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Open(sums.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if n, err := f.Seek(0, io.SeekEnd); n != 11 || err != nil {
		t.Fatalf("Seek end = %d, %v", n, err)
	}
	if n, err := f.Seek(6, io.SeekStart); n != 6 || err != nil {
		t.Fatalf("Seek = %d, %v", n, err)
	}
	if b, err := io.ReadAll(f); string(b) != "world" || err != nil {
		t.Fatalf("read from 6 = %q, %v", b, err)
	}
	p := make([]byte, 3)
	if n, err := f.ReadAt(p, 2); n != 3 || err != nil || string(p) != "llo" {
		t.Fatalf("ReadAt = %q, %d, %v", p, n, err)
	}
	p = make([]byte, 8)
	if n, err := f.ReadAt(p, 8); n != 3 || err != io.EOF || string(p[:n]) != "rld" {
		t.Fatalf("ReadAt past end = %q, %d, %v", p[:n], n, err)
	}
	if _, err := f.ReadAt(p, 11); err != io.EOF {
		t.Fatalf("ReadAt at end: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(f); string(b) != "hello world" {
		t.Fatalf("read from 0 = %q", b)
	}
	if got := count(srv.Log(), "Range: bytes="); got < 3 {
		t.Fatalf("Range GETs = %d:\n%s", got, strings.Join(srv.Log(), "\n"))
	}

	// What repo.ServeAsset does: Open once, ServeContent with a Range.
	f2, err := s.Open(sums.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=6-")
	http.ServeContent(rec, req, "", time.Time{}, f2)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "world" || rec.Header().Get("Content-Range") != "bytes 6-10/11" {
		t.Fatalf("ServeContent: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Range"))
	}
}

// Walk pages through the listing, only under the prefix, and skips keys
// that are not blobs.
func TestS3WalkPaginates(t *testing.T) {
	srv := s3test.New(t)
	srv.MaxKeys = 2
	s := openS3(t, srv, func(c *blob.S3Config) { c.Prefix = "p" })
	var want []string
	for _, c := range []string{"a", "b", "c", "d", "e"} {
		sums, err := s.Write(strings.NewReader(c))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, sums.SHA256)
	}
	old := time.Now().Add(-3 * time.Hour)
	srv.Put("p/notes.txt", []byte("x"), old)
	srv.Put("p/sub/"+helloSHA256, []byte("x"), old)
	srv.Put(helloSHA256, []byte("x"), old)
	srv.Put("q/"+helloSHA256, []byte("x"), old)
	var got []string
	err := s.Walk(func(sha string, mod time.Time) error {
		if time.Since(mod) > time.Minute {
			t.Errorf("%s: mod %v", sha, mod)
		}
		got = append(got, sha)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || count(srv.Log(), "continuation-token=") != 3 {
		t.Fatalf("walked %v over\n%s", got, strings.Join(srv.Log(), "\n"))
	}
	for _, w := range want {
		if count(got, w) != 1 {
			t.Fatalf("%s walked %d times", w, count(got, w))
		}
	}
	stop := errors.New("stop")
	if err := s.Walk(func(string, time.Time) error { return stop }); err != stop {
		t.Fatalf("Walk did not stop: %v", err)
	}
}

// Writing a blob again refreshes its LastModified, which GC's age guard
// relies on, as the directory store's touch does.
func TestS3WriteRefreshesModTime(t *testing.T) {
	srv := s3test.New(t)
	s := openS3(t, srv, nil)
	srv.Put(helloSHA256, []byte("hello"), time.Now().Add(-2*time.Hour))
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(helloSHA256)
	if err != nil || time.Since(info.ModTime) > time.Minute || info.Size != 5 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
}

// Credentials come from the store or, when it has none, the AWS_* variables
// (a session token is sent and signed).
func TestS3Credentials(t *testing.T) {
	srv := s3test.New(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	cfg := srv.Config()
	cfg.AccessKey, cfg.SecretKey = "", ""
	if _, err := blob.OpenS3(t.TempDir(), cfg); err == nil || !strings.Contains(err.Error(), "no S3 credentials") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", srv.AccessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", srv.SecretKey)
	t.Setenv("AWS_SESSION_TOKEN", "tok")
	s, err := blob.OpenS3(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wrong")
	s, err = blob.OpenS3(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("wrong secret: err = %v", err)
	}
	if _, err := s.Open(helloSHA256); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("wrong secret: Open err = %v", err)
	}
}

func TestS3CheckAndErrors(t *testing.T) {
	srv := s3test.New(t)
	if err := openS3(t, srv, nil).Check(); err != nil {
		t.Fatal(err)
	}
	s := openS3(t, srv, func(c *blob.S3Config) { c.Bucket = "other" })
	if err := s.Check(); err == nil || !strings.Contains(err.Error(), `bucket "other" not found`) {
		t.Fatalf("missing bucket: %v", err)
	}
	if _, err := s.Stat(helloSHA256); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Stat in a missing bucket: %v", err)
	}
	srv.Close()
	if err := openS3(t, srv, nil).Check(); err == nil {
		t.Fatal("Check passed with the server gone")
	}
}

func TestS3URLs(t *testing.T) {
	srv := s3test.New(t)
	for _, c := range []struct {
		cfg  blob.S3Config
		want string
	}{
		{blob.S3Config{Bucket: "bkt", AccessKey: "a", SecretKey: "s"}, "https://bkt.s3.us-east-1.amazonaws.com"},
		{blob.S3Config{Bucket: "bkt", Region: "eu-west-1", AccessKey: "a", SecretKey: "s"}, "https://bkt.s3.eu-west-1.amazonaws.com"},
		{blob.S3Config{Bucket: "bkt", Endpoint: "http://minio:9000/", PathStyle: true, AccessKey: "a", SecretKey: "s"}, "http://minio:9000/bkt"},
		{blob.S3Config{Bucket: "bkt", Endpoint: "https://rgw.example/s3", AccessKey: "a", SecretKey: "s"}, "https://bkt.rgw.example/s3"},
		{srv.Config(), srv.URL + "/test"},
	} {
		s, err := blob.OpenS3(t.TempDir(), c.cfg)
		if err != nil {
			t.Fatalf("%+v: %v", c.cfg, err)
		}
		if got := blob.S3BaseURL(s); got != c.want {
			t.Errorf("%+v: base %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestS3ConfigValidate(t *testing.T) {
	ok := blob.S3Config{Bucket: "my-bucket", Prefix: "/a/b/", Endpoint: "http://minio:9000", SSE: "aws:kms", AccessKey: "a", SecretKey: "s"}
	if err := ok.Validate(); err != nil || ok.Prefix != "a/b" {
		t.Fatalf("valid config: %v, prefix %q", err, ok.Prefix)
	}
	for _, c := range []struct {
		mutate func(*blob.S3Config)
		want   string
	}{
		{func(c *blob.S3Config) { c.Bucket = "" }, "bucket"},
		{func(c *blob.S3Config) { c.Bucket = "Upper" }, "bucket"},
		{func(c *blob.S3Config) { c.Prefix = "a b" }, "prefix"},
		{func(c *blob.S3Config) { c.Endpoint = "minio:9000" }, "endpoint"},
		{func(c *blob.S3Config) { c.Endpoint = "ftp://x" }, "endpoint"},
		{func(c *blob.S3Config) { c.Endpoint = "http://u:p@x" }, "endpoint"},
		{func(c *blob.S3Config) { c.SSE = "yes" }, "sse"},
		{func(c *blob.S3Config) { c.SecretKey = "" }, "accessKey and secretKey"},
	} {
		cfg := ok
		c.mutate(&cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want %q", cfg, err, c.want)
		}
	}
}

// A zero-byte blob is a PUT with Content-Length: 0, not a chunked body.
func TestS3EmptyBlob(t *testing.T) {
	srv := s3test.New(t)
	s := openS3(t, srv, nil)
	sums, err := s.Write(bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Open(sums.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if b, err := io.ReadAll(f); len(b) != 0 || err != nil {
		t.Fatalf("read %q, %v", b, err)
	}
}
