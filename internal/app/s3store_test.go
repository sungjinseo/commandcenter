package app

import (
	"context"
	"io"
	"strings"
	"testing"

	"commandcenter/internal/blob/s3test"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// An S3 store recorded in the database opens at startup, serves a
// repository's blobs from the bucket, and its GC removes only blobs its
// repositories no longer reference, from the bucket.
func TestS3StoreStartupAndGC(t *testing.T) {
	srv := s3test.New(t)
	cfg := testConfig(t, "")
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s3 := srv.Config()
	s3.Prefix = "cc"
	if err := a.DB.CreateBlobStore(ctx, meta.BlobStore{Name: "cloud", Path: "blobstores/cloud", Type: meta.BlobStoreS3, S3: &s3}); err != nil {
		t.Fatal(err)
	}
	a.Close()

	a, err = New(cfg, quiet())
	if err != nil {
		t.Fatalf("restart with an s3 store: %v", err)
	}
	defer a.Close()
	if !a.Store.HasStore("cloud") {
		t.Fatal("s3 store not opened at startup")
	}
	if err := a.Registry.Create(ctx, config.Repository{Name: "maven-cloud", Format: "maven", Type: "hosted", BlobStore: "cloud"}); err != nil {
		t.Fatal(err)
	}
	keep, err := a.Store.Put(ctx, "maven-cloud", "g/a/1/keep.jar", "application/java-archive", strings.NewReader("keep"))
	if err != nil {
		t.Fatal(err)
	}
	gone, err := a.Store.Put(ctx, "maven-cloud", "g/a/1/gone.jar", "application/java-archive", strings.NewReader("gone"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Object("cc/" + keep.BlobSHA256); !ok {
		t.Fatalf("blob not in the bucket: %v", srv.Keys())
	}
	f, err := a.Store.Open(keep)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(f); string(b) != "keep" {
		t.Fatalf("read back %q", b)
	}
	f.Close()
	if err := a.Store.Delete(ctx, "maven-cloud", "g/a/1/gone.jar"); err != nil {
		t.Fatal(err)
	}
	// A blob also referenced from the default store's repository is not
	// shared across stores: the bucket's copy goes when cloud drops it.
	if _, err := a.Store.Put(ctx, "maven-releases", "g/a/1/gone.jar", "application/java-archive", strings.NewReader("gone")); err != nil {
		t.Fatal(err)
	}
	removed, freed, err := a.GCStore(ctx, "cloud", 0)
	if err != nil || removed != 1 || freed != 4 {
		t.Fatalf("GCStore = %d, %d, %v; want 1 blob of 4 bytes", removed, freed, err)
	}
	if _, ok := srv.Object("cc/" + gone.BlobSHA256); ok {
		t.Fatal("unreferenced blob still in the bucket")
	}
	if _, err := a.Store.StoreByName("cloud").Stat(keep.BlobSHA256); err != nil {
		t.Fatalf("referenced blob gone: %v", err)
	}
	if _, err := a.Store.StoreByName("default").Stat(gone.BlobSHA256); err != nil {
		t.Fatalf("default store's copy gone: %v", err)
	}
	if removed, _, err := a.GCStore(ctx, "cloud", 0); err != nil || removed != 0 {
		t.Fatalf("second GC = %d, %v", removed, err)
	}
}
