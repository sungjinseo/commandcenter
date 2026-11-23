package meta

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
)

func TestS3Keys(t *testing.T) {
	db := openTest(t)
	if err := db.SetSecretKey(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	k := S3Key{AccessKeyID: "CCKAAAAAAAAAAAAAAAAA", Owner: "alice", Name: "ci", Created: t0, Expires: t0.Add(24 * time.Hour)}
	if err := db.PutS3Key(ctx, k, "s3cr3t"); err != nil {
		t.Fatal(err)
	}
	if err := db.PutS3Key(ctx, k, "other"); !errors.Is(err, ErrExists) {
		t.Fatalf("same id: %v", err)
	}
	var stored string
	db.sql.QueryRowContext(ctx, `SELECT secret FROM s3_keys`).Scan(&stored)
	if !strings.HasPrefix(stored, sealedPrefix) || strings.Contains(stored, "s3cr3t") {
		t.Fatalf("stored secret %q is not sealed", stored)
	}
	got, secret, err := db.S3KeyByID(ctx, k.AccessKeyID)
	if err != nil || secret != "s3cr3t" || got != k {
		t.Fatalf("by id: %+v %q %v", got, secret, err)
	}
	if err := db.PutS3Key(ctx, S3Key{AccessKeyID: "CCKBBBBBBBBBBBBBBBBB", Owner: "ldap:bob", Created: t0}, "x"); err != nil {
		t.Fatal(err)
	}
	db.TouchS3Key(ctx, k.AccessKeyID, t0.Add(time.Minute))
	if l, _ := db.ListS3Keys(ctx, "alice"); len(l) != 1 || !l[0].LastUsed.Equal(t0.Add(time.Minute)) {
		t.Fatalf("alice's: %+v", l)
	}
	if l, _ := db.ListS3Keys(ctx, ""); len(l) != 2 || l[0].Owner != "alice" {
		t.Fatalf("all: %+v", l)
	}
	if _, _, err := db.S3KeyByID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Rotation reseals the key's secret.
	if _, _, err := db.RotateSecretKey(ctx, bytes.Repeat([]byte{8}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, secret, err := db.S3KeyByID(ctx, k.AccessKeyID); err != nil || secret != "s3cr3t" {
		t.Fatalf("after rotation: %q %v", secret, err)
	}
	if d, err := db.DeleteS3Key(ctx, k.AccessKeyID); err != nil || d.Owner != "alice" {
		t.Fatalf("delete: %+v %v", d, err)
	}
	if _, err := db.DeleteS3Key(ctx, k.AccessKeyID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if n, err := db.DeleteS3KeysOf(ctx, "ldap:bob"); err != nil || n != 1 {
		t.Fatalf("of bob: %d %v", n, err)
	}
}

func TestS3ObjectMetaFollowsAsset(t *testing.T) {
	db := openTest(t)
	a := Asset{Repo: "b", Path: "k/x.bin", BlobSHA256: "s1", Size: 1}
	if err := db.PutAsset(ctx, a); err != nil {
		t.Fatal(err)
	}
	m := S3ObjectMeta{BlobSHA256: "s1", ETag: "abc-2", Headers: map[string]string{"x-amz-meta-a": "1"}}
	if err := db.Tx(ctx, func(tx *Tx) error { return tx.PutS3ObjectMeta(ctx, "b", "k/x.bin", m) }); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetS3ObjectMeta(ctx, "b", "k/x.bin")
	if err != nil || got.ETag != "abc-2" || got.Headers["x-amz-meta-a"] != "1" {
		t.Fatalf("get: %+v %v", got, err)
	}
	// No asset, no row.
	if err := db.Tx(ctx, func(tx *Tx) error { return tx.PutS3ObjectMeta(ctx, "b", "missing", m) }); err == nil {
		t.Fatal("a row without its asset was stored")
	}
	if err := db.DeleteAsset(ctx, "b", "k/x.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetS3ObjectMeta(ctx, "b", "k/x.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after the asset's delete: %v", err)
	}
}

func TestS3Uploads(t *testing.T) {
	db := openTest(t)
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for i, u := range []S3Upload{{ID: "u1", Path: "a/one"}, {ID: "u2", Path: "a/one"}, {ID: "u3", Path: "b/two"}} {
		u.Repo, u.Initiator, u.Started, u.Updated = "bk", "alice", t0, t0.Add(time.Duration(i)*time.Hour)
		if err := db.PutS3Upload(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if u, err := db.GetS3Upload(ctx, "u1"); err != nil || u.Path != "a/one" || u.Headers == nil {
		t.Fatalf("get: %+v %v", u, err)
	}
	l, _ := db.ListS3Uploads(ctx, "bk", "a/", "", "", 10)
	if len(l) != 2 || l[0].ID != "u1" || l[1].ID != "u2" {
		t.Fatalf("list a/: %+v", l)
	}
	if l, _ := db.ListS3Uploads(ctx, "bk", "", "a/one", "u1", 10); len(l) != 2 || l[0].ID != "u2" || l[1].ID != "u3" {
		t.Fatalf("list after: %+v", l)
	}
	if err := db.TouchS3Upload(ctx, "u1", t0.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if l, _ := db.StaleS3Uploads(ctx, t0.Add(90*time.Minute)); len(l) != 1 || l[0].ID != "u2" {
		t.Fatalf("stale: %+v", l)
	}
	if err := db.DeleteS3Upload(ctx, "u2"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteS3Upload(ctx, "u2"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestListObjects(t *testing.T) {
	db := openTest(t)
	paths := []string{"a", "a/b", "a/c", "a/c/d", "a0", "b", "한/글", "\U0010FFFFz"}
	for _, p := range paths {
		if err := db.PutAsset(ctx, Asset{Repo: "bk", Path: p, BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	names := func(as []Asset) string {
		var s []string
		for _, a := range as {
			s = append(s, a.Path)
		}
		return strings.Join(s, ",")
	}
	for _, c := range []struct {
		prefix, from string
		excl         bool
		limit        int
		want         string
	}{
		{"", "", false, 100, "a,a/b,a/c,a/c/d,a0,b,한/글,\U0010FFFFz"},
		{"a/", "", false, 100, "a/b,a/c,a/c/d"},
		{"a/", "a/c", true, 100, "a/c/d"},
		{"a/", "a/c", false, 1, "a/c"},
		{"a", "a/c/d", true, 100, "a0"},
		{"한", "", false, 100, "한/글"},
		{"\U0010FFFF", "", false, 100, "\U0010FFFFz"},
		{"zz", "", false, 100, ""},
	} {
		got, err := db.ListObjects(ctx, "bk", c.prefix, c.from, c.excl, c.limit)
		if err != nil || names(got) != c.want {
			t.Errorf("%+v: %q %v", c, names(got), err)
		}
	}
	if ok, _ := db.RepoHasAssets(ctx, "bk"); !ok {
		t.Fatal("has assets")
	}
	if ok, _ := db.RepoHasAssets(ctx, "empty"); ok {
		t.Fatal("empty has assets")
	}
}

func TestS3RepositoryRaisesCompat(t *testing.T) {
	db := openTest(t)
	if err := db.CreateRepository(ctx, config.Repository{Name: "raw1", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	var compat int
	db.sql.QueryRowContext(ctx, `SELECT compat FROM schema_version`).Scan(&compat)
	if compat != schemaCompat {
		t.Fatalf("compat %d before an s3 repository", compat)
	}
	if err := db.CreateRepository(ctx, config.Repository{Name: "bucket1", Format: "s3", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	db.sql.QueryRowContext(ctx, `SELECT compat FROM schema_version`).Scan(&compat)
	if compat != schemaS3 {
		t.Fatalf("compat %d after an s3 repository", compat)
	}
}
