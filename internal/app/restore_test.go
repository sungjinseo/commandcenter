package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/backup"
	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

// Back up a live app (one repository in the default store, one in a second
// store at a relative path), restore into an empty directory and start an app
// on it: every asset downloads with the same sha256, uploads still work, and
// the second store is read from its restored directory.
func TestRestoreRoundTripStartsApp(t *testing.T) {
	if metatest.Postgres() {
		t.Skip("online backup and restore need the SQLite metadata database")
	}
	cfg := testConfig(t, "")
	cfg.BackupDir = filepath.Join(t.TempDir(), "backup")
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rootB := filepath.Join(cfg.DataDir, "stores", "b")
	bs, err := blob.Open(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DB.CreateBlobStore(ctx, meta.BlobStore{Name: "b", Path: "stores/b"}); err != nil {
		t.Fatal(err)
	}
	a.Store.AddStore("b", bs)
	if err := a.Registry.Create(ctx, config.Repository{Name: "maven-b", Format: "maven", Type: "hosted", BlobStore: "b"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{} // URL path -> sha256 of the body
	bodies := map[string]string{}
	put := func(repo, path, body string) {
		t.Helper()
		if _, err := a.Store.Put(ctx, repo, path, "application/java-archive", strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		want["/repository/"+repo+"/"+path] = hex.EncodeToString(sum[:])
		bodies["/repository/"+repo+"/"+path] = body
	}
	for i := range 20 {
		put("maven-releases", fmt.Sprintf("org/ex/a/%d/a-%d.jar", i, i), strings.Repeat(fmt.Sprintf("a%d;", i), 300))
	}
	for i := range 5 {
		put("maven-b", fmt.Sprintf("org/ex/b/%d/b-%d.jar", i, i), strings.Repeat(fmt.Sprintf("b%d;", i), 300))
	}
	m, err := a.Tasks.Backup.Run(ctx, backup.Params{Subdir: "rt", Deep: true}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()

	data := filepath.Join(t.TempDir(), "restored")
	res, err := backup.Restore(ctx, backup.RestoreOptions{From: filepath.Join(cfg.BackupDir, "rt"), Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if res.Blobs != m.Blobs || res.Missing != 0 || res.Bytes != m.Bytes {
		t.Fatalf("restore %+v vs manifest %+v", res, m)
	}

	cfg2 := testConfig(t, "")
	cfg2.DataDir = data
	b, err := New(cfg2, quiet())
	if err != nil {
		t.Fatalf("start on the restored data: %v", err)
	}
	defer b.Close()
	ts := httptest.NewServer(b.Handler)
	defer ts.Close()
	for p, sum := range want {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := sha256.Sum256(body)
		if resp.StatusCode != 200 || hex.EncodeToString(got[:]) != sum || string(body) != bodies[p] {
			t.Fatalf("GET %s: %d, sha256 %x want %s", p, resp.StatusCode, got, sum)
		}
	}
	// Uploads work on the restored instance.
	req, _ := http.NewRequest("PUT", ts.URL+"/repository/maven-releases/org/ex/new/1/new-1.jar", strings.NewReader("fresh"))
	req.SetBasicAuth("ci", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload after restore: %d", resp.StatusCode)
	}
}
