package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"

	"commandcenter/internal/meta/metatest"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")))
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
	var walked []string
	if err := db.WalkAssetPaths(ctx, "r", "g/a/", func(p string) { walked = append(walked, p) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(walked, ",") != "g/a/1.0/a-1.0.jar,g/a/2.0/a-2.0.jar" {
		t.Fatalf("walked = %v", walked)
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

func userVersion(t *testing.T, db *DB) int {
	t.Helper()
	v, err := schemaVersionOf(ctx, db.sql)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// rawOpen opens target with the bare driver, without Open's upgrade.
func rawOpen(t *testing.T, target string) *pool {
	t.Helper()
	driver := "sqlite"
	if IsPostgresURL(target) {
		driver = "pgx"
	}
	db, err := sql.Open(driver, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &pool{db: db, pg: driver == "pgx"}
}

// QA N-30: Open stamps the schema version on new and older (version 0)
// databases, and refuses one a newer build wrote, naming both versions and
// the way back, without changing it.
func TestOpenSchemaVersion(t *testing.T) {
	path := metatest.Target(t, filepath.Join(t.TempDir(), "meta.db"))
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := userVersion(t, db); v != schemaVersion {
		t.Fatalf("new database: user_version %d, want %d", v, schemaVersion)
	}
	if err := stampSchemaVersion(ctx, db.sql, 0, 0); err != nil { // as builds before the guard left it
		t.Fatal(err)
	}
	db.Close()
	if db, err = Open(path); err != nil {
		t.Fatalf("older database: %v", err)
	}
	if v := userVersion(t, db); v != schemaVersion {
		t.Fatalf("older database: user_version %d, want %d", v, schemaVersion)
	}
	if err := stampSchemaVersion(ctx, db.sql, schemaVersion+1, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = Open(path)
	if err == nil {
		t.Fatal("a database from a newer schema was opened")
	}
	for _, want := range []string{fmt.Sprintf("schema %d", schemaVersion+1), fmt.Sprintf("up to %d", schemaVersion), "backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if v, err := schemaVersionOf(ctx, rawOpen(t, path)); err != nil || v != schemaVersion+1 {
		t.Fatalf("refused database: user_version %d (err %v), want it untouched at %d", v, err, schemaVersion+1)
	}
}

// Open creates the tables, migrates and stamps the version in one
// transaction: when a step fails, as a crash would stop it (here the last
// migration, on a tasks view that ALTER TABLE cannot change), the earlier
// steps are undone too and the version stays 0, so the next start migrates
// again instead of trusting a half-migrated database. That next start
// commits once, so the stamp cannot lag the migration.
func TestOpenMigratesInOneTransaction(t *testing.T) {
	if metatest.Postgres() {
		t.Skip("SQLite's migration; TestOpenPostgresInOneTransaction is PostgreSQL's")
	}
	path := filepath.Join(t.TempDir(), "meta.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("CREATE TABLE assets(repo TEXT, path TEXT, blob_sha256 TEXT); CREATE VIEW tasks AS SELECT 1 AS id"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "view") {
		t.Fatalf("Open: %v, want the migration of the tasks view to fail", err)
	}
	var v, cols, tables int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 0 {
		t.Fatalf("user_version %d (err %v), want 0", v, err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM pragma_table_info('assets') WHERE name = 'downloads'").Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("the failed Open added assets.downloads (%d, err %v)", cols, err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'users'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("the failed Open created the users table (%d, err %v)", tables, err)
	}

	if _, err := raw.Exec("DROP VIEW tasks"); err != nil {
		t.Fatal(err)
	}
	commits := 0
	sqlite.RegisterConnectionHook(func(c sqlite.ExecQuerierContext, dsn string) error {
		if strings.HasPrefix(dsn, "file:"+path+"?") { // Open's connections only
			c.(sqlite.HookRegisterer).RegisterCommitHook(func() int32 { commits++; return 0 })
		}
		return nil
	})
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v := userVersion(t, db); commits != 1 || v != schemaVersion {
		t.Fatalf("upgrade: %d commits, user_version %d; want one commit that stamps %d", commits, v, schemaVersion)
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
		db.sql.QueryRowContext(ctx, `SELECT last_used_at FROM tokens WHERE hash = 'h'`).Scan(&v)
		return v
	}
	db.TouchToken(ctx, "h")
	first := lastUsed()
	if first == 0 {
		t.Fatal("first use must be recorded")
	}
	db.sql.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = 'h'`, time.Now().Add(-10*time.Second).UnixMilli())
	recent := lastUsed()
	db.TouchToken(ctx, "h")
	if lastUsed() != recent {
		t.Fatal("a use within a minute must not write again")
	}
	db.sql.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = 'h'`, time.Now().Add(-2*time.Minute).UnixMilli())
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
		db.sql.QueryRowContext(ctx, `SELECT last_used_at FROM tokens WHERE hash = 'h'`).Scan(&v)
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
	db.sql.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = 'h'`, recent)
	db.TouchToken(ctx, "h")
	if lastUsed() != recent {
		t.Fatal("a use within a minute must not write again")
	}
	old := time.Now().Add(-2 * time.Minute).UnixMilli()
	db.sql.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = 'h'`, old)
	db.TouchToken(ctx, "h")
	if lastUsed() == old {
		t.Fatal("a use after a minute must be recorded")
	}
	db.TouchToken(ctx, "nope") // unknown hash: no error, no row
}
