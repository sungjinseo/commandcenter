package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

// core-25: a blob store that does not open no longer stops the node: its
// repositories answer 503, the others serve, the status check and the blob
// store lists name it, and it opens once it can (the registry's resync).
func TestBlobStoreThatDoesNotOpen(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(`
dataDir: ` + t.TempDir() + `
anonymousRead: true
users:
  - { name: admin, passwordHash: "` + string(h) + `", roles: [admin] }
repositories:
  - { name: files, format: raw, type: hosted }
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	root := filepath.Join(t.TempDir(), "store")
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil { // as a lost mount or a wrong path would
		t.Fatal(err)
	}
	db, err := meta.Open(cfg.Database.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.CreateBlobStore(ctx, meta.BlobStore{Name: "broken", Path: root, Type: meta.BlobStoreFile}); err != nil {
		t.Fatal(err)
	}
	def := config.Repository{Name: "r", Format: "raw", Type: "hosted", BlobStore: "broken"}
	config.ApplyRepositoryDefaults(&def)
	if err := db.CreateRepository(ctx, def); err != nil {
		t.Fatal(err)
	}
	db.Close()

	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatalf("a blob store that does not open stopped the node: %v", err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, b := do("GET", "/repository/r/a.txt", ""); st != 503 || !strings.Contains(b, "broken") {
		t.Fatalf("GET: %d %s", st, b)
	}
	if st, b := do("PUT", "/repository/r/a.txt", "x"); st != 503 {
		t.Fatalf("PUT: %d %s", st, b)
	}
	if st, b := do("PUT", "/repository/files/a.txt", "x"); st != 201 {
		t.Fatalf("another store's repository: %d %s", st, b)
	}
	st, b := do("GET", "/-/api/status/checks", "")
	var checks struct {
		Checks []struct {
			Name    string
			Healthy bool
			Message string
		}
	}
	json.Unmarshal([]byte(b), &checks)
	found := false
	for _, c := range checks.Checks {
		if c.Name == "Blob store" {
			found = !c.Healthy && strings.Contains(c.Message, `"broken"`)
		}
	}
	if st != 200 || !found {
		t.Fatalf("status checks: %d %s", st, b)
	}
	if st, b := do("GET", "/service/rest/v1/blobstores", ""); st != 200 || !strings.Contains(b, `"name":"broken"`) ||
		!strings.Contains(b, `"unavailable":true`) {
		t.Fatalf("nexus blob stores: %d %s", st, b)
	}

	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := a.Registry.Resync(ctx); err != nil {
		t.Fatal(err)
	}
	if why := a.Store.Unopened("broken"); why != "" {
		t.Fatalf("still unopened: %s", why)
	}
	if st, b := do("PUT", "/repository/r/a.txt", "x"); st != 201 {
		t.Fatalf("PUT after the store opened: %d %s", st, b)
	}
	if st, b := do("GET", "/repository/r/a.txt", ""); st != 200 || b != "x" {
		t.Fatalf("GET after the store opened: %d %s", st, b)
	}
}
