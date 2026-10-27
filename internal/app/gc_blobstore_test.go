package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// twoStoreApp builds an App with the default store plus a second store "b"
// holding a maven-hosted repository "maven-b", alongside the default-store
// "maven-releases". It returns the App and the root directory of store b.
func twoStoreApp(t *testing.T) (*App, string) {
	t.Helper()
	a, err := New(testConfig(t, ""), quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ctx := context.Background()

	rootB := filepath.Join(t.TempDir(), "store-b")
	bs, err := blob.Open(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DB.CreateBlobStore(ctx, meta.BlobStore{Name: "b", Path: rootB}); err != nil {
		t.Fatal(err)
	}
	a.Store.AddStore("b", bs)
	if err := a.Registry.Create(ctx, config.Repository{Name: "maven-b", Format: "maven", Type: "hosted", BlobStore: "b"}); err != nil {
		t.Fatal(err)
	}
	return a, rootB
}

func blobFilePath(root, sha string) string {
	return filepath.Join(root, "blobs", "sha256", sha[:2], sha[2:4], sha)
}

func ageBlob(t *testing.T, root, sha string) {
	t.Helper()
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(blobFilePath(root, sha), old, old); err != nil {
		t.Fatalf("age blob: %v", err)
	}
}

func present(t *testing.T, store *blob.Store, sha string) bool {
	t.Helper()
	f, err := store.Open(sha)
	if err == nil {
		f.Close()
		return true
	}
	return false
}

// TestBlobRoutingPerStore confirms a repository's uploads land in its own
// store's directory and not the default one, and are read back from there.
func TestBlobRoutingPerStore(t *testing.T) {
	a, rootB := twoStoreApp(t)
	ctx := context.Background()

	asset, err := a.Store.Put(ctx, "maven-b", "org/r/1.0/r-1.0.jar", "application/java-archive", strings.NewReader("in-store-b"))
	if err != nil {
		t.Fatal(err)
	}
	// The blob file exists under store B, not under the default store.
	if _, err := os.Stat(blobFilePath(rootB, asset.BlobSHA256)); err != nil {
		t.Fatalf("blob not written to store B: %v", err)
	}
	if _, err := os.Stat(blobFilePath(a.Config.DataDir, asset.BlobSHA256)); err == nil {
		t.Fatal("blob leaked into the default store directory")
	}
	// It reads back through the routed Open.
	f, err := a.Store.Open(asset)
	if err != nil {
		t.Fatalf("Open routed asset: %v", err)
	}
	f.Close()
}

// TestBackwardCompatDefaultRouting checks a repository with no blobStore (an
// old stored row) resolves to the default store and reads a blob written there.
func TestBackwardCompatDefaultRouting(t *testing.T) {
	a, _ := twoStoreApp(t)
	ctx := context.Background()
	// maven-releases carries no explicit store beyond the applied default; a
	// blob written under the default tree is served through it.
	asset, err := a.Store.Put(ctx, "maven-releases", "org/c/1.0/c-1.0.jar", "application/java-archive", strings.NewReader("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blobFilePath(a.Config.DataDir, asset.BlobSHA256)); err != nil {
		t.Fatalf("default-store blob missing: %v", err)
	}
	f, err := a.Store.Open(asset)
	if err != nil {
		t.Fatalf("read default-store blob: %v", err)
	}
	f.Close()
}

// TestGCKeepsReferencedBlob is the explicit data-safety guarantee: a blob a
// repository still references is never removed by GC, even when it is old.
func TestGCKeepsReferencedBlob(t *testing.T) {
	a, _ := twoStoreApp(t)
	ctx := context.Background()

	asset, err := a.Store.Put(ctx, "maven-releases", "org/x/1.0/x-1.0.jar", "application/java-archive", strings.NewReader("referenced"))
	if err != nil {
		t.Fatal(err)
	}
	ageBlob(t, a.Config.DataDir, asset.BlobSHA256) // old, so only the in-use check can save it

	n, err := a.GC(ctx, time.Hour)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 0 {
		t.Fatalf("GC removed %d blobs; a referenced blob must survive", n)
	}
	if !present(t, a.Store.StoreByName("default"), asset.BlobSHA256) {
		t.Fatal("referenced blob was deleted by GC")
	}
}

// TestGCRemovesUnreferencedBlob confirms GC still reclaims an old blob no asset
// references, in the store it belongs to.
func TestGCRemovesUnreferencedBlob(t *testing.T) {
	a, _ := twoStoreApp(t)
	ctx := context.Background()

	sums, err := a.Store.StoreByName("default").Write(strings.NewReader("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	ageBlob(t, a.Config.DataDir, sums.SHA256)

	n, err := a.GCStore(ctx, "default", time.Hour)
	if err != nil {
		t.Fatalf("GCStore: %v", err)
	}
	if n != 1 {
		t.Fatalf("GCStore removed %d blobs, want 1", n)
	}
	if present(t, a.Store.StoreByName("default"), sums.SHA256) {
		t.Fatal("unreferenced blob survived GC")
	}
}

// QA N-30: a build without named stores (a downgrade) writes the blobs of a
// named-store repository to the default store. A GC of the default store
// keeps such a blob, the only copy, until it is moved into its store.
func TestGCDefaultKeepsBlobOfNamedStoreRepo(t *testing.T) {
	a, _ := twoStoreApp(t)
	ctx := context.Background()
	sums, err := a.Store.StoreByName("default").Write(strings.NewReader("written-by-an-older-build"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DB.PutAsset(ctx, meta.Asset{Repo: "maven-b", Path: "org/o/1.0/o-1.0.jar", BlobSHA256: sums.SHA256, Size: sums.Size}); err != nil {
		t.Fatal(err)
	}
	ageBlob(t, a.Config.DataDir, sums.SHA256)
	if n, err := a.GC(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("GC removed %d (err %v); a blob a repository references must survive in the default store", n, err)
	}
	if !present(t, a.Store.StoreByName("default"), sums.SHA256) {
		t.Fatal("the default store lost a blob that maven-b references")
	}
}

// TestGCSweepsStaleTempFiles: a write killed mid-way leaves a partial temp
// file; the compact pass removes it once stale, in the store it collects.
func TestGCSweepsStaleTempFiles(t *testing.T) {
	a, rootB := twoStoreApp(t)
	old := time.Now().Add(-2 * time.Hour)
	tmpFile := func(root string) string {
		p := filepath.Join(root, "tmp", "up-killed")
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	inB, inDefault := tmpFile(rootB), tmpFile(a.Config.DataDir)

	if _, err := a.GCStore(context.Background(), "b", time.Hour); err != nil {
		t.Fatalf("GCStore(b): %v", err)
	}
	if _, err := os.Stat(inB); !os.IsNotExist(err) {
		t.Fatalf("stale temp file in store b survived its GC: %v", err)
	}
	if _, err := os.Stat(inDefault); err != nil {
		t.Fatalf("GC of store b touched the default store's tmp: %v", err)
	}
}

// TestGCStoreIsolation proves collecting store A never touches store B: an
// unreferenced blob in B survives a GC of A, and a referenced blob in B is
// safe too.
func TestGCStoreIsolation(t *testing.T) {
	a, rootB := twoStoreApp(t)
	ctx := context.Background()

	// A referenced blob in store B.
	kept, err := a.Store.Put(ctx, "maven-b", "org/y/1.0/y-1.0.jar", "application/java-archive", strings.NewReader("kept-in-b"))
	if err != nil {
		t.Fatal(err)
	}
	ageBlob(t, rootB, kept.BlobSHA256)
	// An unreferenced blob in store B.
	orphanB, err := a.Store.StoreByName("b").Write(strings.NewReader("orphan-in-b"))
	if err != nil {
		t.Fatal(err)
	}
	ageBlob(t, rootB, orphanB.SHA256)

	// Collect store A only.
	if _, err := a.GCStore(ctx, "default", time.Hour); err != nil {
		t.Fatalf("GCStore(default): %v", err)
	}
	if !present(t, a.Store.StoreByName("b"), kept.BlobSHA256) {
		t.Fatal("GC of store A removed a referenced blob from store B")
	}
	if !present(t, a.Store.StoreByName("b"), orphanB.SHA256) {
		t.Fatal("GC of store A removed an unreferenced blob from store B")
	}
}

// TestGCPerStoreDedup checks the per-store scoping of "in use": the same sha
// can live in two stores, and collecting one store judges the blob only by its
// own repositories' references.
func TestGCPerStoreDedup(t *testing.T) {
	a, rootB := twoStoreApp(t)
	ctx := context.Background()

	const content = "shared-content"
	inA, err := a.Store.Put(ctx, "maven-releases", "org/z/1.0/z-1.0.jar", "application/java-archive", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	inB, err := a.Store.Put(ctx, "maven-b", "org/z/1.0/z-1.0.jar", "application/java-archive", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if inA.BlobSHA256 != inB.BlobSHA256 {
		t.Fatalf("identical content hashed differently: %s vs %s", inA.BlobSHA256, inB.BlobSHA256)
	}
	sha := inA.BlobSHA256
	// Each store has its own copy (per-store dedup).
	if !present(t, a.Store.StoreByName("default"), sha) || !present(t, a.Store.StoreByName("b"), sha) {
		t.Fatal("both stores should hold their own copy of the shared blob")
	}

	// Drop the reference in store B only; the blob file stays until GC.
	if err := a.Store.Delete(ctx, "maven-b", "org/z/1.0/z-1.0.jar"); err != nil {
		t.Fatal(err)
	}
	ageBlob(t, a.Config.DataDir, sha)
	ageBlob(t, rootB, sha)

	// Collecting store B removes its now-unreferenced copy.
	n, err := a.GCStore(ctx, "b", time.Hour)
	if err != nil {
		t.Fatalf("GCStore(b): %v", err)
	}
	if n != 1 {
		t.Fatalf("GCStore(b) removed %d, want 1", n)
	}
	if present(t, a.Store.StoreByName("b"), sha) {
		t.Fatal("store B kept a blob none of its repositories reference")
	}
	// Store A still references it: it must remain, and a GC of A must keep it.
	if !present(t, a.Store.StoreByName("default"), sha) {
		t.Fatal("store A lost its copy when store B was collected")
	}
	if nn, err := a.GCStore(ctx, "default", time.Hour); err != nil || nn != 0 {
		t.Fatalf("GCStore(default) removed %d (err %v); store A still references the blob", nn, err)
	}
	if !present(t, a.Store.StoreByName("default"), sha) {
		t.Fatal("store A's referenced blob was deleted")
	}
}
