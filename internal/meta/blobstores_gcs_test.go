package meta

import (
	"bytes"
	"strings"
	"testing"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta/metatest"
)

// A google store's account key is sealed in its row, opens on read, is
// sealed again by a key rotation (the quota next to it kept) and does not
// open without the key.
func TestBlobStoreGoogleSealed(t *testing.T) {
	path := metatest.URL(t, t.TempDir())
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSecretKey(bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	cfg := blob.GCSConfig{Bucket: "bkt", Prefix: "cc", Authentication: blob.GCSAuthKey, AccountKey: `{"private_key":"gcs-secret"}`}
	q := &Quota{Type: QuotaSpaceUsed, LimitBytes: 10}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "g", Path: "blobstores/g", Type: BlobStoreGoogle, Google: &cfg, Quota: q}); err != nil {
		t.Fatal(err)
	}
	if cfg.AccountKey != `{"private_key":"gcs-secret"}` {
		t.Fatal("the caller's key was sealed in place")
	}
	const row = `SELECT config FROM blob_stores WHERE name = ?`
	raw := rawRow(t, db, row, "g")
	if strings.Contains(raw, "gcs-secret") || !strings.Contains(raw, sealedPrefix) {
		t.Fatalf("stored row is not sealed: %s", raw)
	}
	read := func(d *DB) {
		t.Helper()
		bs, err := d.GetBlobStore(ctx, "g")
		if err != nil || bs.Type != BlobStoreGoogle || bs.Google == nil || bs.Google.AccountKey != cfg.AccountKey ||
			bs.Google.Bucket != "bkt" || bs.Google.Prefix != "cc" || bs.Quota == nil || *bs.Quota != *q {
			t.Fatalf("GetBlobStore: %+v %+v, %v", bs, bs.Google, err)
		}
	}
	read(db)
	if n, err := db.SealSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("SealSecrets = %d, %v: nothing should be left in plain text", n, err)
	}
	if _, _, err := db.RotateSecretKey(ctx, bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
	if again := rawRow(t, db, row, "g"); again == raw || strings.Contains(again, "gcs-secret") {
		t.Fatalf("rotation left the row as it was or in plain text: %s", again)
	}
	read(db)

	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.GetBlobStore(ctx, "g"); err == nil || strings.Contains(err.Error(), "gcs-secret") {
		t.Fatalf("read without the key: %v", err)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "bad", Type: BlobStoreGoogle}); err == nil {
		t.Fatal("a google store without its configuration was stored")
	}
}
