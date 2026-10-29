package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/blob/s3test"
	"commandcenter/internal/meta"
)

func s3Body(t *testing.T, srv *s3test.Server, name string, mutate func(m map[string]any)) string {
	t.Helper()
	cfg := srv.Config()
	s3 := map[string]any{"bucket": cfg.Bucket, "prefix": "cc/", "region": cfg.Region, "endpoint": cfg.Endpoint,
		"pathStyle": true, "accessKey": cfg.AccessKey, "secretKey": cfg.SecretKey, "sse": "AES256"}
	m := map[string]any{"name": name, "type": "s3", "s3": s3}
	if mutate != nil {
		mutate(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// An S3 store is created with its bucket checked, shows its settings but
// never the secret key, keeps the key sealed in the database, and routes a
// repository's blobs to the bucket; its temp directory is under dataDir.
func TestBlobStoreS3(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	dataDir := withBlobStore(t, e)
	if err := e.db.SetSecretKey([]byte(strings.Repeat("k", 32))); err != nil {
		t.Fatal(err)
	}
	srv := s3test.New(t)
	r := e.admin(t, "POST", "/-/api/blobstores", s3Body(t, srv, "cloud", nil))
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	s3, _ := m["s3"].(map[string]any)
	if m["type"] != "s3" || m["path"] != filepath.Join(dataDir, "blobstores", "cloud") || m["fsTotal"] != nil || m["warning"] != nil ||
		s3 == nil || s3["bucket"] != srv.Bucket || s3["prefix"] != "cc" || s3["sse"] != "AES256" || s3["accessKeyId"] != srv.AccessKey || s3["secretSet"] != true {
		t.Fatalf("create: %s", r.body)
	}
	if strings.Contains(r.body, srv.SecretKey) || strings.Contains(r.body, "secretAccessKey\":\"") {
		t.Fatalf("the secret key is shown: %s", r.body)
	}
	for _, path := range []string{"/-/api/blobstores", "/-/api/blobstores/cloud"} {
		r := e.admin(t, "GET", path, "")
		wantStatus(t, r, http.StatusOK)
		if strings.Contains(r.body, srv.SecretKey) || !strings.Contains(r.body, `"type":"s3"`) {
			t.Fatalf("GET %s: %s", path, r.body)
		}
	}
	// The row was sealed on write: the startup pass has nothing left to seal.
	if n, err := e.db.SealSecrets(context.Background()); err != nil || n != 0 {
		t.Fatalf("SealSecrets = %d, %v; the store's secret key was stored in plain text", n, err)
	}
	if !e.ui.Store.HasStore("cloud") || !strings.Contains(strings.Join(srv.Log(), "\n"), "list-type=2") {
		t.Fatalf("store not live or bucket not checked: %v", srv.Log())
	}

	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"mvn-cloud","format":"maven","type":"hosted","blobStore":"cloud"}`), http.StatusCreated)
	ctx := context.Background()
	a, err := e.ui.Store.Put(ctx, "mvn-cloud", "g/a/1/a-1.jar", "application/java-archive", strings.NewReader("in the bucket"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := srv.Object("cc/" + a.BlobSHA256); !ok || string(got) != "in the bucket" || srv.SSE("cc/"+a.BlobSHA256) != "AES256" {
		t.Fatalf("object: %q %v sse %q; keys %v", got, ok, srv.SSE("cc/"+a.BlobSHA256), srv.Keys())
	}
	f, err := e.ui.Store.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "in the bucket" {
		t.Fatalf("read back %q", b)
	}
	r = e.admin(t, "GET", "/-/api/blobstores/cloud", "")
	if m := r.json(t); m["blobCount"] != float64(1) || m["repoCount"] != float64(1) {
		t.Fatalf("usage: %s", r.body)
	}

	// The same bucket and prefix again is refused; another prefix is fine.
	r = e.admin(t, "POST", "/-/api/blobstores", s3Body(t, srv, "cloud2", nil))
	if r.status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(r.json(t)["error"]), `already the "cloud" store's`) {
		t.Fatalf("same bucket and prefix: %d %s", r.status, r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", s3Body(t, srv, "cloud2", func(m map[string]any) {
		m["s3"].(map[string]any)["prefix"] = "other"
	})), http.StatusCreated)

	wantStatus(t, e.admin(t, "DELETE", "/-/api/blobstores/cloud", ""), http.StatusConflict)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/blobstores/cloud2", ""), http.StatusNoContent)
	if e.ui.Store.HasStore("cloud2") {
		t.Fatal("deleted store still live")
	}
}

func TestBlobStoreS3Rejected(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	srv := s3test.New(t)
	for name, c := range map[string]struct {
		mutate func(m map[string]any)
		want   string
	}{
		"unknown type":    {func(m map[string]any) { m["type"] = "gcs" }, `must be "file", "s3" or "group"`},
		"no s3":           {func(m map[string]any) { delete(m, "s3") }, "needs"},
		"no bucket":       {func(m map[string]any) { m["s3"].(map[string]any)["bucket"] = "" }, "bucket"},
		"bad endpoint":    {func(m map[string]any) { m["s3"].(map[string]any)["endpoint"] = "minio:9000" }, "endpoint"},
		"bad sse":         {func(m map[string]any) { m["s3"].(map[string]any)["sse"] = "yes" }, "sse"},
		"no secret":       {func(m map[string]any) { m["s3"].(map[string]any)["secretKey"] = "" }, "secretKey"},
		"wrong secret":    {func(m map[string]any) { m["s3"].(map[string]any)["secretKey"] = "nope" }, "not usable"},
		"no bucket there": {func(m map[string]any) { m["s3"].(map[string]any)["bucket"] = "elsewhere" }, "not usable"},
		"relative path":   {func(m map[string]any) { m["path"] = "tmp/x" }, "must be absolute"},
	} {
		r := e.admin(t, "POST", "/-/api/blobstores", s3Body(t, srv, "bad", c.mutate))
		if r.status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(r.json(t)["error"]), c.want) || strings.Contains(r.body, srv.SecretKey) {
			t.Errorf("%s: %d %s (want 400 with %q)", name, r.status, r.body, c.want)
		}
	}
	if e.ui.Store.HasStore("bad") {
		t.Fatal("a refused store was added")
	}
	if _, err := e.db.GetBlobStore(context.Background(), "bad"); err != meta.ErrNotFound {
		t.Fatalf("a refused store was stored: %v", err)
	}
}

// Review S3-3: the same bucket and prefix with the endpoint spelled
// differently (a trailing slash) is still the same object namespace, and
// is refused: the two stores' GCs would delete each other's blobs.
func TestBlobStoreS3IsolationEndpointSpelling(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	srv := s3test.New(t)
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", s3Body(t, srv, "a", nil)), http.StatusCreated)
	r := e.admin(t, "POST", "/-/api/blobstores", s3Body(t, srv, "b", func(m map[string]any) {
		m["s3"].(map[string]any)["endpoint"] = strings.ToUpper(srv.URL) + "/"
	}))
	if r.status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(r.json(t)["error"]), `already the "a" store's`) {
		t.Fatalf("same bucket and prefix at %s/ accepted: %d %s", srv.URL, r.status, r.body)
	}
}
