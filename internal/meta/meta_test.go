package meta

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var ctx = context.Background()

func TestAssetRoundTripAndUpsert(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	a := Asset{Repo: "r", Path: "a/b.jar", BlobSHA256: "s1", Size: 3, ContentType: "x/y", SHA1: "h1", MD5: "m", SHA512: "x",
		CreatedAt: t0, LastModified: t0, RemoteETag: `"e"`, RemoteLastModified: "lm", CheckedAt: t0}
	if err := db.PutAsset(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAsset(ctx, "r", "a/b.jar")
	if err != nil {
		t.Fatal(err)
	}
	if got != a {
		t.Fatalf("got %+v\nwant %+v", got, a)
	}

	b := a
	b.BlobSHA256, b.CreatedAt, b.LastModified = "s2", t0.Add(time.Hour), t0.Add(time.Hour)
	if err := db.PutAsset(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetAsset(ctx, "r", "a/b.jar")
	if got.BlobSHA256 != "s2" || !got.CreatedAt.Equal(t0) || !got.LastModified.Equal(t0.Add(time.Hour)) {
		t.Fatalf("upsert: %+v", got)
	}
}

func TestGetDeleteNotFound(t *testing.T) {
	db := openTest(t)
	if _, err := db.GetAsset(ctx, "r", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v", err)
	}
	if err := db.DeleteAsset(ctx, "r", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete: %v", err)
	}
}

func TestListAssetsByPrefix(t *testing.T) {
	db := openTest(t)
	for _, p := range []string{"g/a/1.0/a-1.0.jar", "g/a/2.0/a-2.0.jar", "g/ab/1.0/ab-1.0.jar", "g/a0"} {
		if err := db.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	db.PutAsset(ctx, Asset{Repo: "other", Path: "g/a/1.0/x", BlobSHA256: "s"})
	list, err := db.ListAssets(ctx, "r", "g/a/")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Path != "g/a/1.0/a-1.0.jar" || list[1].Path != "g/a/2.0/a-2.0.jar" {
		t.Fatalf("list = %+v", list)
	}
	all, _ := db.ListAssets(ctx, "r", "")
	if len(all) != 4 {
		t.Fatalf("all = %d", len(all))
	}
}

func TestNegativeCache(t *testing.T) {
	db := openTest(t)
	now := time.Now()
	if err := db.PutNegative(ctx, "r", "p", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if hit, _ := db.NegativeHit(ctx, "r", "p", now); !hit {
		t.Fatal("want hit")
	}
	if hit, _ := db.NegativeHit(ctx, "r", "p", now.Add(2*time.Minute)); hit {
		t.Fatal("want expired")
	}
	db.ClearNegative(ctx, "r", "p")
	if hit, _ := db.NegativeHit(ctx, "r", "p", now); hit {
		t.Fatal("want cleared")
	}
}

// Expired negative-cache rows used to stay forever (only per-path deletes
// existed); PruneNegative removes exactly the rows that can no longer hit.
func TestPruneNegative(t *testing.T) {
	db := openTest(t)
	now := time.UnixMilli(1_700_000_000_000)
	for path, exp := range map[string]time.Time{"old": now.Add(-time.Hour), "edge": now, "live": now.Add(time.Minute)} {
		if err := db.PutNegative(ctx, "r", path, exp); err != nil {
			t.Fatal(err)
		}
	}
	n, err := db.PruneNegative(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("PruneNegative = %d, %v; want 2 rows", n, err)
	}
	if hit, _ := db.NegativeHit(ctx, "r", "live", now); !hit {
		t.Fatal("live entry was pruned")
	}
	var left int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM negative_cache`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("rows left = %d, %v; want 1", left, err)
	}
}

func TestMarkCheckedAndBlobInUse(t *testing.T) {
	db := openTest(t)
	db.PutAsset(ctx, Asset{Repo: "r", Path: "p", BlobSHA256: "abc"})
	t1 := time.UnixMilli(1_800_000_000_000).UTC()
	if err := db.MarkChecked(ctx, "r", "p", t1); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetAsset(ctx, "r", "p")
	if !got.CheckedAt.Equal(t1) {
		t.Fatalf("checked = %v", got.CheckedAt)
	}
	if used, _ := db.BlobInUse(ctx, "abc"); !used {
		t.Fatal("abc should be in use")
	}
	if used, _ := db.BlobInUse(ctx, "zzz"); used {
		t.Fatal("zzz should not be in use")
	}
}

func TestTokens(t *testing.T) {
	db := openTest(t)
	if err := db.CreateToken(ctx, "id1", "ci", "hash1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if u, _, err := db.TokenUser(ctx, "hash1"); err != nil || u != "ci" {
		t.Fatalf("user=%q err=%v", u, err)
	}
	if _, _, err := db.TokenUser(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenUseIsThrottled(t *testing.T) {
	db := openTest(t)
	db.CreateToken(ctx, "id", "ci", "h", time.Now())
	lastUsed := func() int64 {
		var v int64
		db.sql.QueryRow(`SELECT last_used_at FROM tokens WHERE hash = 'h'`).Scan(&v)
		return v
	}
	db.TouchToken(ctx, "h")
	first := lastUsed()
	if first == 0 {
		t.Fatal("first use must be recorded")
	}
	db.sql.Exec(`UPDATE tokens SET last_used_at = ? WHERE hash = 'h'`, time.Now().Add(-10*time.Second).UnixMilli())
	recent := lastUsed()
	db.TouchToken(ctx, "h")
	if lastUsed() != recent {
		t.Fatal("a use within a minute must not write again")
	}
	db.sql.Exec(`UPDATE tokens SET last_used_at = ? WHERE hash = 'h'`, time.Now().Add(-2*time.Minute).UnixMilli())
	old := lastUsed()
	db.TouchToken(ctx, "h")
	if lastUsed() == old {
		t.Fatal("a use after a minute must be recorded")
	}
}

func TestTouchTokenIsConditional(t *testing.T) {
	db := openTest(t)
	db.CreateToken(ctx, "id", "ci", "h", time.Now())
	lastUsed := func() int64 {
		var v int64
		db.sql.QueryRow(`SELECT last_used_at FROM tokens WHERE hash = 'h'`).Scan(&v)
		return v
	}
	if _, _, err := db.TokenUser(ctx, "h"); err != nil || lastUsed() != 0 {
		t.Fatalf("TokenUser must not write (err %v, last %d)", err, lastUsed())
	}
	db.TouchToken(ctx, "h")
	if lastUsed() == 0 {
		t.Fatal("first use must be recorded")
	}
	recent := time.Now().Add(-10 * time.Second).UnixMilli()
	db.sql.Exec(`UPDATE tokens SET last_used_at = ? WHERE hash = 'h'`, recent)
	db.TouchToken(ctx, "h")
	if lastUsed() != recent {
		t.Fatal("a use within a minute must not write again")
	}
	old := time.Now().Add(-2 * time.Minute).UnixMilli()
	db.sql.Exec(`UPDATE tokens SET last_used_at = ? WHERE hash = 'h'`, old)
	db.TouchToken(ctx, "h")
	if lastUsed() == old {
		t.Fatal("a use after a minute must be recorded")
	}
	db.TouchToken(ctx, "nope") // unknown hash: no error, no row
}
