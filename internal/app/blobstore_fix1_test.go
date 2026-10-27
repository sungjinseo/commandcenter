package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

// orphanApp starts an App whose database already holds a repository "orphan"
// naming a blob store "ghost" that does not exist (as a bad import or a lost
// race could leave it). It returns the App and the startup log.
func orphanApp(t *testing.T) (*App, *bytes.Buffer) {
	t.Helper()
	cfg := testConfig(t, "")
	db, err := meta.Open(metatest.Target(t, filepath.Join(cfg.DataDir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	def := config.Repository{Name: "orphan", Format: "maven", Type: "hosted"}
	config.ApplyRepositoryDefaults(&def)
	def.BlobStore = "ghost"
	if err := db.CreateRepository(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var logs bytes.Buffer
	a, err := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("a repository naming a missing store must not stop startup: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a, &logs
}

func countBlobs(t *testing.T, s *blob.Store) int {
	t.Helper()
	n := 0
	if err := s.Walk(func(string, time.Time) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

// C1: a referenced blob of a repository whose store cannot be resolved is
// never collected, from any store.
func TestGCKeepsBlobOfRepoWithMissingStore(t *testing.T) {
	a, logs := orphanApp(t)
	ctx := context.Background()
	if !strings.Contains(logs.String(), "ghost") || !strings.Contains(logs.String(), "orphan") {
		t.Fatalf("startup must log the repository naming a missing store; log:\n%s", logs)
	}
	def := a.Store.StoreByName("default")
	sums, err := def.Write(strings.NewReader("orphan-data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DB.PutAsset(ctx, meta.Asset{Repo: "orphan", Path: "p", BlobSHA256: sums.SHA256, Size: sums.Size}); err != nil {
		t.Fatal(err)
	}
	ageBlob(t, a.Config.DataDir, sums.SHA256)
	n, _, err := a.GCStore(ctx, "default", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || !present(t, def, sums.SHA256) {
		t.Fatalf("GC removed %d blobs; the orphan repository's referenced blob must survive", n)
	}
}

// C1: routing to a missing store fails and writes nothing to default.
func TestRoutingToMissingStoreFails(t *testing.T) {
	a, _ := orphanApp(t)
	def := a.Store.StoreByName("default")
	before := countBlobs(t, def)
	if _, err := a.Store.Put(context.Background(), "orphan", "x", "", strings.NewReader("must-not-land")); err == nil {
		t.Fatal("Put to a repository naming a missing store succeeded")
	}
	if after := countBlobs(t, def); after != before {
		t.Fatalf("default store gained %d blobs from a misrouted write", after-before)
	}
}

// C1: a group's merged metadata (stored under the group's name) is in the
// default store's scope.
func TestGroupMergedBlobSurvivesDefaultGC(t *testing.T) {
	a, _ := twoStoreApp(t)
	ctx := context.Background()
	asset, err := a.Store.Put(ctx, "maven-public", "org/m/maven-metadata.xml", "application/xml", strings.NewReader("<merged/>"))
	if err != nil {
		t.Fatal(err)
	}
	ageBlob(t, a.Config.DataDir, asset.BlobSHA256)
	if n, _, err := a.GCStore(ctx, "default", time.Hour); err != nil || n != 0 {
		t.Fatalf("GC removed %d (err %v); the group's merged blob is referenced", n, err)
	}
	if !present(t, a.Store.StoreByName("default"), asset.BlobSHA256) {
		t.Fatal("group's merged blob was deleted")
	}
}

// C1: the one-time config import refuses a repository naming a missing store.
func TestConfigImportRejectsUnknownBlobStore(t *testing.T) {
	cfg := testConfig(t, "  - { name: bad, format: maven, type: hosted, blobStore: nope }\n")
	a, err := New(cfg, quiet())
	if err == nil {
		a.Close()
		t.Fatal("import of a repository naming a missing blob store succeeded")
	}
	if !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("error should name the repository and store: %v", err)
	}
}

// I3: moving dataDir moves the default store and relative named stores.
func TestDataDirMoveResolvesStores(t *testing.T) {
	cfg := testConfig(t, "")
	ctx := context.Background()
	oldDir := cfg.DataDir
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DB.CreateBlobStore(ctx, meta.BlobStore{Name: "rel", Path: filepath.Join("blobstores", "rel")}); err != nil {
		t.Fatal(err)
	}
	a.Close()

	newDir := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = newDir
	b, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	row, err := b.DB.GetBlobStore(ctx, "default")
	if err != nil || row.Path != newDir {
		t.Fatalf("default row path = %q (err %v), want %s", row.Path, err, newDir)
	}
	for _, c := range []struct{ store, root string }{
		{"default", newDir},
		{"rel", filepath.Join(newDir, "blobstores", "rel")},
	} {
		s := b.Store.StoreByName(c.store)
		if s == nil {
			t.Fatalf("store %s not opened", c.store)
		}
		sums, err := s.Write(strings.NewReader("content-" + c.store))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(blobFilePath(c.root, sums.SHA256)); err != nil {
			t.Fatalf("store %s did not write under %s: %v", c.store, c.root, err)
		}
	}
}

// Minor 5: one failing store does not stop GC of the others.
func TestGCContinuesAfterStoreError(t *testing.T) {
	a, rootB := twoStoreApp(t)
	ctx := context.Background()
	sums, err := a.Store.StoreByName("default").Write(strings.NewReader("orphan-default"))
	if err != nil {
		t.Fatal(err)
	}
	ageBlob(t, a.Config.DataDir, sums.SHA256)
	if err := os.RemoveAll(filepath.Join(rootB, "blobs")); err != nil { // store b's walk now fails
		t.Fatal(err)
	}
	n, _, err := a.GC(ctx, time.Hour)
	if err == nil {
		t.Fatal("GC hid store b's error")
	}
	if n != 1 || present(t, a.Store.StoreByName("default"), sums.SHA256) {
		t.Fatalf("GC removed %d; it must still collect the default store after b failed", n)
	}
}
