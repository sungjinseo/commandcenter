package meta

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta/metatest"
)

// An azure store's secrets are sealed in the row, open on read, are sealed
// again by a key rotation, and an update of its credentials keeps the soft
// quota. The first azure store raises compat to 7, which is refused while a
// node of an older build is live.
func TestBlobStoreAzureStored(t *testing.T) {
	target := metatest.URL(t, t.TempDir())
	db, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSecretKey(bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	raw := rawOpen(t, target)
	if _, c, err := readSchema(ctx, raw); err != nil || c != schemaCompat {
		t.Fatalf("compat before %d %v", c, err)
	}
	key := blob.AzureConfig{AccountName: "acct1", ContainerName: "c12", Prefix: "cc", AccountKey: "a2V5LTE="}
	app := blob.AzureConfig{AccountName: "acct1", ContainerName: "c34", Auth: blob.AzureClientSecret, TenantID: "t1", ClientID: "app", ClientSecret: "app-secret"}
	for _, c := range []*blob.AzureConfig{&key, &app} {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}

	// A live node of schema 6 keeps the first azure store out.
	if _, err := raw.db.Exec(`INSERT INTO nodes(id, hostname, version, schema, started_at, heartbeat_at) VALUES('old-1', 'h', 'old', 6, 1, ` + dbNow + `)`); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "k", Path: "/k", Type: BlobStoreAzure, Azure: &key}); err == nil || !strings.Contains(err.Error(), "old-1") {
		t.Fatalf("created beside a schema 6 node: %v", err)
	}
	if _, err := db.GetBlobStore(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused store stored: %v", err)
	}
	if _, err := raw.db.Exec(`UPDATE nodes SET heartbeat_at = 1 WHERE id = 'old-1'`); err != nil { // it stopped
		t.Fatal(err)
	}

	q := &Quota{Type: QuotaSpaceUsed, LimitBytes: 10}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "k", Path: "/k", Type: BlobStoreAzure, Azure: &key, Quota: q}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "a", Path: "/a", Type: BlobStoreAzure, Azure: &app}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "bad", Path: "/b", Type: BlobStoreAzure}); err == nil {
		t.Fatal("an azure store without its configuration")
	}
	if v, c, err := readSchema(ctx, raw); err != nil || v != schemaVersion || c != schemaAzure {
		t.Fatalf("after the first azure store: version %d compat %d %v", v, c, err)
	}
	stored := func(name string) string {
		t.Helper()
		var config string
		if err := raw.db.QueryRow(`SELECT config FROM blob_stores WHERE name = $1`, name).Scan(&config); err != nil {
			t.Fatal(err)
		}
		return config
	}
	if c := stored("k") + stored("a"); strings.Contains(c, "a2V5LTE=") || strings.Contains(c, "app-secret") || strings.Count(c, sealedPrefix) != 2 {
		t.Fatalf("not sealed: %s", c)
	}
	k, err := db.GetBlobStore(ctx, "k")
	if err != nil || k.Azure == nil || *k.Azure != key || k.Quota == nil || *k.Quota != *q {
		t.Fatalf("k %+v %+v %v", k, k.Azure, err)
	}
	if n, err := db.SealSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("SealSecrets = %d %v: stored in plain text", n, err)
	}
	before := stored("a")
	resealed, plain, err := db.RotateSecretKey(ctx, bytes.Repeat([]byte{2}, 32))
	if err != nil || resealed < 2 || plain != 0 {
		t.Fatalf("RotateSecretKey = %d %d %v", resealed, plain, err)
	}
	if stored("a") == before {
		t.Fatal("not sealed again")
	}
	if a, err := db.GetBlobStore(ctx, "a"); err != nil || *a.Azure != app {
		t.Fatalf("after rotation %+v %v", a.Azure, err)
	}

	sas := key
	sas.Auth, sas.SASToken = blob.AzureSASToken, "sv=1&sig=x"
	if err := sas.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBlobStoreAzure(ctx, "k", sas); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored("k"), "sig=x") {
		t.Fatalf("sas not sealed: %s", stored("k"))
	}
	if k, err := db.GetBlobStore(ctx, "k"); err != nil || *k.Azure != sas || k.Quota == nil || *k.Quota != *q {
		t.Fatalf("after update %+v %+v %v", k.Azure, k.Quota, err)
	}
	if err := db.UpdateBlobStoreAzure(ctx, "default-nope", sas); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing store: %v", err)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "f", Path: "/f"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBlobStoreAzure(ctx, "f", sas); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a file store: %v", err)
	}
}
