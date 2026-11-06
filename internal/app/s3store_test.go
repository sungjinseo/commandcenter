package app

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// core-26: new keys set on A are in use on B when the PUT answers, and at the
// next start; a wrong key leaves both nodes as they were.
func TestS3ConnectionAcrossNodes(t *testing.T) {
	srv := s3test.New(t)
	c := srv.Config()
	cfg := clusterConfig(t, "")
	a, b := startNode(t, cfg, true, AsServer()), startNode(t, cfg, true, AsServer())
	conn := func(access, secret string) string {
		return `"accessKeyId":"` + access + `","secretAccessKey":"` + secret + `","region":"` + c.Region + `","endpoint":"` + c.Endpoint + `","pathStyle":true`
	}
	a.want(t, "POST", "/-/api/blobstores", `{"name":"s3x","type":"s3","s3":{"bucket":"`+c.Bucket+`",`+conn(c.AccessKey, c.SecretKey)+`}}`, ok(201))
	b.want(t, "POST", "/-/api/repositories", `{"name":"raw-s3","format":"raw","type":"hosted","blobStore":"s3x"}`, ok(201))
	b.want(t, "PUT", "/repository/raw-s3/f.txt", "in s3", ok(201))
	srv.SetKeys("ROTATED", "rotated-secret")
	a.want(t, "PUT", "/-/api/blobstores/s3x", `{"s3":{`+conn("ROTATED", "wrong")+`}}`, ok(400))
	a.want(t, "PUT", "/-/api/blobstores/s3x", `{"s3":{`+conn("ROTATED", "rotated-secret")+`}}`, ok(200))
	if got, _ := b.Store.StoreByName("s3x").S3(); got.AccessKey != "ROTATED" {
		t.Fatalf("B's store when the PUT answered: %s", got.AccessKey)
	}
	if got := b.want(t, "GET", "/repository/raw-s3/f.txt", nil, ok(200)); got != "in s3" {
		t.Fatalf("read on B: %q", got)
	}
	b.want(t, "PUT", "/repository/raw-s3/g.txt", "also in s3", ok(201))
	if got := a.want(t, "GET", "/repository/raw-s3/g.txt", nil, ok(200)); got != "also in s3" {
		t.Fatalf("read on A: %q", got)
	}
	c3 := startNode(t, cfg, true)
	if got := c3.want(t, "GET", "/repository/raw-s3/g.txt", nil, ok(200)); got != "also in s3" {
		t.Fatalf("read on a node started later: %q", got)
	}
}

// core-23: the node that leads checks the soft quotas without a client asking.
func TestLeaderChecksSoftQuotas(t *testing.T) {
	old := usageEvery
	usageEvery = 20 * time.Millisecond
	t.Cleanup(func() { usageEvery = old })
	a := startNode(t, clusterConfig(t, ""), false, AsServer())
	a.want(t, "POST", "/-/api/blobstores", `{"name":"q","path":"`+filepath.Join(t.TempDir(), "q")+`","quota":{"type":"spaceRemainingBytes","limitBytes":9000000000000000000}}`, ok(201))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.usageLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if !eventually(20*time.Second, func() bool {
		evs, err := a.DB.ListEvents(ctx, 10, 0, "blobstore.quota")
		return err == nil && len(evs) == 1 && evs[0].Subject == "q"
	}) {
		t.Fatal("no blobstore.quota event")
	}
}
