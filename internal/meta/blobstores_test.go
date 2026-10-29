package meta

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta/metatest"
)

// A blob_stores table from schema 1 (no type or config column) reads back
// as file stores after the upgrade, next to new S3 rows.
func TestBlobStoreUpgradeFromSchema1(t *testing.T) {
	path := metatest.URL(t, t.TempDir())
	raw := rawOpen(t, path)
	for _, q := range []string{
		`CREATE TABLE blob_stores(name TEXT PRIMARY KEY, path TEXT NOT NULL, created_at BIGINT NOT NULL)`,
		`INSERT INTO blob_stores(name, path, created_at) VALUES('old', 'blobstores/old', 1)`,
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "s3", Path: "blobstores/s3", Type: BlobStoreS3,
		S3: &blob.S3Config{Bucket: "bkt", Prefix: "p", Region: "eu-west-1", Endpoint: "http://minio:9000", PathStyle: true, SSE: "AES256"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedBlobStore(ctx, "default", "/data"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListBlobStores(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("ListBlobStores = %+v, %v", rows, err)
	}
	if rows[0].Name != "default" || rows[0].Type != BlobStoreFile || rows[0].S3 != nil {
		t.Fatalf("seeded: %+v", rows[0])
	}
	if rows[1].Name != "old" || rows[1].Type != BlobStoreFile || rows[1].S3 != nil || rows[1].Path != "blobstores/old" {
		t.Fatalf("upgraded: %+v", rows[1])
	}
	if s := rows[2]; s.Type != BlobStoreS3 || s.S3 == nil || s.S3.Bucket != "bkt" || s.S3.Prefix != "p" || s.S3.Region != "eu-west-1" ||
		s.S3.Endpoint != "http://minio:9000" || !s.S3.PathStyle || s.S3.SSE != "AES256" {
		t.Fatalf("s3: %+v %+v", s, s.S3)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "bad", Type: BlobStoreS3}); err == nil {
		t.Fatal("an s3 store without configuration was stored")
	}
}

// The soft quota lives in the config column next to an S3 store's sealed
// secret: setting it keeps the secret, rekeying keeps the quota.
func TestBlobStoreQuotaStored(t *testing.T) {
	db, err := Open(metatest.URL(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSecretKey(bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	q := &Quota{Type: QuotaSpaceUsed, LimitBytes: 10}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "f", Path: "/f", Quota: q}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "s", Path: "/s", Type: BlobStoreS3, S3: &blob.S3Config{Bucket: "b", SecretKey: "s3-secret"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "bad", Path: "/b", Quota: &Quota{Type: "x", LimitBytes: 1}}); err == nil {
		t.Fatal("invalid quota stored")
	}
	if err := db.SetBlobStoreQuota(ctx, "s", &Quota{Type: QuotaSpaceRemaining, LimitBytes: 5}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBlobStoreQuota(ctx, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing store: %v", err)
	}
	if _, _, err := db.RotateSecretKey(ctx, bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
	f, _ := db.GetBlobStore(ctx, "f")
	s, err := db.GetBlobStore(ctx, "s")
	if err != nil || f.Quota == nil || *f.Quota != *q || s.Quota == nil || s.Quota.Type != QuotaSpaceRemaining || s.S3.SecretKey != "s3-secret" {
		t.Fatalf("f %+v s %+v %v", f.Quota, s, err)
	}
	if err := db.SetBlobStoreQuota(ctx, "f", nil); err != nil {
		t.Fatal(err)
	}
	if f, _ := db.GetBlobStore(ctx, "f"); f.Quota != nil {
		t.Fatalf("not removed: %+v", f.Quota)
	}
}

// A group keeps its soft quota when its members change, and a quota set on
// a group survives the round trip.
func TestBlobStoreGroupKeepsQuota(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b"} {
		if err := db.CreateBlobStore(ctx, BlobStore{Name: n, Path: "/tmp/" + n}); err != nil {
			t.Fatal(err)
		}
	}
	q := &Quota{Type: "spaceUsedBytes", LimitBytes: 1 << 30}
	g := BlobStore{Name: "g", Path: "/tmp/g", Type: BlobStoreGroup, Quota: q,
		Group: &blob.GroupConfig{Members: []blob.GroupMember{{Name: "a"}}, FillPolicy: "roundRobin"}}
	if err := db.CreateBlobStore(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBlobStoreGroup(ctx, "g", blob.GroupConfig{Members: []blob.GroupMember{{Name: "a"}, {Name: "b"}}, FillPolicy: "writeToFirst"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetBlobStore(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if got.Quota == nil || *got.Quota != *q || got.Group == nil || len(got.Group.Members) != 2 || got.Group.FillPolicy != "writeToFirst" {
		t.Fatalf("group after update: quota %+v group %+v", got.Quota, got.Group)
	}
}
