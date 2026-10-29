package blob_test

import (
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

// replica is a second fake bucket in another region that a store lists as
// its failover.
func replica(t *testing.T) (*s3test.Server, blob.S3Failover) {
	srv := s3test.New(t)
	srv.Bucket = "replica"
	return srv, blob.S3Failover{Region: "eu-west-1", Bucket: "replica", Endpoint: srv.URL}
}

// While the primary endpoint is down, reads go to the failover bucket and
// writes fail; health shows which of the two answers. Before that, writes go
// to the primary only.
func TestS3FailoverPrimaryKilled(t *testing.T) {
	primary := s3test.New(t)
	rep, fo := replica(t)
	s := openS3(t, primary, func(c *blob.S3Config) { c.Prefix = "cc"; c.Failover = []blob.S3Failover{fo} })
	sums, err := s.Write(strings.NewReader("replicated"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := primary.Object("cc/" + sums.SHA256); !ok || len(rep.Keys()) != 0 {
		t.Fatalf("write: primary %v, replica %v", primary.Keys(), rep.Keys())
	}
	rep.Put("cc/"+sums.SHA256, []byte("replicated"), time.Now()) // the provider's replication
	if p := s.Health(); len(p) != 2 || !p[0].OK || !p[1].OK || !strings.HasPrefix(p[0].Target, "primary ") ||
		!strings.Contains(p[1].Target, "failover 1 "+rep.URL+"/replica/cc") {
		t.Fatalf("health with both up: %+v", p)
	}

	primary.Close()
	if got := readAll(t, s, sums.SHA256); got != "replicated" {
		t.Fatalf("read via the failover: %q", got)
	}
	if info, err := s.Stat(sums.SHA256); err != nil || info.Size != int64(len("replicated")) {
		t.Fatalf("stat via the failover: %+v, %v", info, err)
	}
	f, err := s.Open(sums.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if n, err := f.ReadAt(buf, 3); err != nil || string(buf[:n]) != "lica" {
		t.Fatalf("ReadAt via the failover: %q, %v", buf[:n], err)
	}
	f.Close()
	if _, err := s.Write(strings.NewReader("new")); err == nil {
		t.Fatal("a write succeeded with the primary down")
	}
	p := s.Health()
	if len(p) != 2 || p[0].OK || p[0].Error == "" || !p[1].OK {
		t.Fatalf("health with the primary down: %+v", p)
	}
}

// A 5xx fails over; a 404 and a 403 from the primary do not: the object is
// missing or the credentials are wrong, and a replica would not fix that.
func TestS3FailoverOnlyOnUnavailable(t *testing.T) {
	rep, fo := replica(t)
	const sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // of ""
	rep.Put(sha, []byte(""), time.Now())
	for _, tc := range []struct {
		status   int
		failover bool
	}{{http.StatusServiceUnavailable, true}, {http.StatusInternalServerError, true}, {http.StatusForbidden, false}, {http.StatusNotFound, false}} {
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			io.WriteString(w, "<Error><Code>X</Code></Error>")
		}))
		cfg := rep.Config()
		cfg.Bucket, cfg.Endpoint, cfg.Failover = "primary", down.URL, []blob.S3Failover{fo}
		s, err := blob.OpenS3(t.TempDir(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Stat(sha)
		if (err == nil) != tc.failover {
			t.Errorf("primary answering %d: stat err %v, want failover %v", tc.status, err, tc.failover)
		}
		if tc.status == http.StatusNotFound && !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("404: %v, want ErrNotFound", err)
		}
		down.Close()
	}
	// A primary that is up and lacks the object answers 404 itself.
	primary := s3test.New(t)
	s := openS3(t, primary, func(c *blob.S3Config) { c.Failover = []blob.S3Failover{fo} })
	if _, err := s.Open(sha); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing in the primary: %v", err)
	}
}

// Server-side encryption in Nexus's shape sends the SSE headers, the KMS key
// id with them, on single PUTs and on multipart initiations.
func TestS3EncryptionHeaders(t *testing.T) {
	old := *blob.S3PartSize
	t.Cleanup(func() { *blob.S3PartSize = old })
	srv := s3test.New(t)
	const key = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	kms := openS3(t, srv, func(c *blob.S3Config) {
		c.Encryption = &blob.S3Encryption{Type: "kmsManagedEncryption", KMSKeyID: key}
	})
	small, err := kms.Write(strings.NewReader("small"))
	if err != nil {
		t.Fatal(err)
	}
	*blob.S3PartSize = 5
	big, err := kms.Write(strings.NewReader("in several parts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{small.SHA256, big.SHA256} {
		if srv.SSE(sha) != "aws:kms" || srv.KMSKey(sha) != key {
			t.Errorf("%s: sse %q kms %q", sha, srv.SSE(sha), srv.KMSKey(sha))
		}
	}
	*blob.S3PartSize = old
	srv2 := s3test.New(t)
	aes := openS3(t, srv2, func(c *blob.S3Config) { c.Encryption = &blob.S3Encryption{Type: "s3ManagedEncryption"} })
	sums, err := aes.Write(strings.NewReader("aes"))
	if err != nil {
		t.Fatal(err)
	}
	if srv2.SSE(sums.SHA256) != "AES256" || srv2.KMSKey(sums.SHA256) != "" {
		t.Fatalf("s3ManagedEncryption: sse %q kms %q", srv2.SSE(sums.SHA256), srv2.KMSKey(sums.SHA256))
	}

	for _, tc := range []struct {
		mutate func(*blob.S3Config)
		want   string
	}{
		{func(c *blob.S3Config) { c.Encryption = &blob.S3Encryption{Type: "sse-c"} }, "encryption.type"},
		{func(c *blob.S3Config) {
			c.SSE = "AES256"
			c.Encryption = &blob.S3Encryption{Type: "kmsManagedEncryption"}
		}, "disagree"},
		{func(c *blob.S3Config) { c.Encryption = &blob.S3Encryption{Type: "s3ManagedEncryption", KMSKeyID: "k"} }, "kmsKeyId goes with"},
		{func(c *blob.S3Config) {
			c.Encryption = &blob.S3Encryption{Type: "kmsManagedEncryption", KMSKeyID: "a b"}
		}, "kmsKeyId must"},
		{func(c *blob.S3Config) { c.Failover = []blob.S3Failover{{Bucket: "X"}} }, "failover[0].bucket"},
		{func(c *blob.S3Config) { c.Failover = []blob.S3Failover{{Bucket: "ok-bucket", Endpoint: "ftp://x"}} }, "failover[0].endpoint"},
		{func(c *blob.S3Config) { c.Failover = []blob.S3Failover{{Bucket: "ok-bucket", Region: "EU West"}} }, "failover[0].region"},
	} {
		cfg := srv.Config()
		tc.mutate(&cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want %q", cfg, err, tc.want)
		}
	}
}
