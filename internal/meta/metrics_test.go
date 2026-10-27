package meta

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"commandcenter/internal/meta/metatest"
)

func TestFlushDownloadsAddsAndTakesMax(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	for _, p := range []string{"a", "b"} {
		if err := db.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s", CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	err := db.FlushDownloads(ctx, []DownloadDelta{
		{Repo: "r", Path: "a", N: 3, Last: t0.Add(2 * time.Hour)},
		{Repo: "r", Path: "b", N: 1, Last: t0.Add(time.Hour)},
		{Repo: "r", Path: "gone", N: 9, Last: t0}, // no such asset: ignored
	})
	if err != nil {
		t.Fatal(err)
	}
	// An older "last" must not lower last_downloaded; counts add up.
	if err := db.FlushDownloads(ctx, []DownloadDelta{{Repo: "r", Path: "a", N: 2, Last: t0.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	a, _ := db.GetAsset(ctx, "r", "a")
	if a.Downloads != 5 || !a.LastDownloaded.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("a: downloads %d last %v", a.Downloads, a.LastDownloaded)
	}
	b, _ := db.GetAsset(ctx, "r", "b")
	if b.Downloads != 1 {
		t.Fatalf("b: %d", b.Downloads)
	}
	// A re-upload keeps the counters.
	if err := db.PutAsset(ctx, Asset{Repo: "r", Path: "a", BlobSHA256: "s2", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if a, _ = db.GetAsset(ctx, "r", "a"); a.Downloads != 5 {
		t.Fatalf("after upsert: %d", a.Downloads)
	}
}

func TestRequestStatsBucketsAndPrune(t *testing.T) {
	db := openTest(t)
	day := int64(86400)
	base := int64(1_700_000_000) / day * day // a UTC midnight
	err := db.FlushRequests(ctx, []RequestDelta{
		{Hour: base, Repo: "r1", Requests: 2, Bytes: 10},
		{Hour: base + 3600, Repo: "r1", Requests: 3, Bytes: 20},
		{Hour: base + 3600, Repo: "r2", Requests: 4, Bytes: 40},
		{Hour: base + day, Repo: "r1", Requests: 1, Bytes: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Flushing the same key again adds.
	if err := db.FlushRequests(ctx, []RequestDelta{{Hour: base, Repo: "r1", Requests: 1, Bytes: 1}}); err != nil {
		t.Fatal(err)
	}
	hourly, err := db.RequestStats(ctx, base, base+day, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []RequestBucket{{T: base, Requests: 3, Bytes: 11}, {T: base + 3600, Requests: 7, Bytes: 60}}
	if len(hourly) != 2 || hourly[0] != want[0] || hourly[1] != want[1] {
		t.Fatalf("hourly %+v", hourly)
	}
	daily, _ := db.RequestStats(ctx, base, base+2*day, day, "")
	if len(daily) != 2 || daily[0] != (RequestBucket{T: base, Requests: 10, Bytes: 71}) || daily[1].Requests != 1 {
		t.Fatalf("daily %+v", daily)
	}
	one, _ := db.RequestStats(ctx, base, base+2*day, day, "r2")
	if len(one) != 1 || one[0].Requests != 4 {
		t.Fatalf("repo filter %+v", one)
	}
	n, err := db.PruneRequestStats(ctx, base+3600)
	if err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	if left, _ := db.RequestStats(ctx, 0, base+2*day, day, ""); len(left) != 2 {
		t.Fatalf("after prune %+v", left)
	}
}

func TestMigrationIsIdempotentOnOldDB(t *testing.T) {
	if metatest.Postgres() {
		t.Skip("SQLite's migration: PostgreSQL support starts at schema 1")
	}
	path := filepath.Join(t.TempDir(), "meta.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE assets(
  repo TEXT NOT NULL, path TEXT NOT NULL, blob_sha256 TEXT NOT NULL, size INTEGER NOT NULL DEFAULT 0,
  content_type TEXT NOT NULL DEFAULT '', sha1 TEXT NOT NULL DEFAULT '', md5 TEXT NOT NULL DEFAULT '',
  sha512 TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0, last_modified INTEGER NOT NULL DEFAULT 0,
  remote_etag TEXT NOT NULL DEFAULT '', remote_last_modified TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(repo, path)) WITHOUT ROWID;
INSERT INTO assets(repo, path, blob_sha256) VALUES('r','p','s');`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	for i := 0; i < 3; i++ { // each start runs the migration again
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		a, err := db.GetAsset(ctx, "r", "p")
		if err != nil || a.Downloads != 0 || !a.LastDownloaded.IsZero() {
			t.Fatalf("open %d: %+v %v", i, a, err)
		}
		db.Close()
	}
}

func TestFlushRollsBackWholeBatchOnError(t *testing.T) {
	db := openTest(t)
	for _, p := range []string{"a", "bad"} {
		if err := db.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	// A trigger forces the second row of each batch to fail.
	ddls := []string{
		`CREATE TRIGGER fail_dl BEFORE UPDATE ON assets WHEN NEW.path = 'bad' BEGIN SELECT RAISE(ABORT, 'forced'); END`,
		`CREATE TRIGGER fail_rq BEFORE INSERT ON request_stats WHEN NEW.repo = 'bad' BEGIN SELECT RAISE(ABORT, 'forced'); END`,
	}
	if db.sql.pg {
		ddls = []string{
			`CREATE FUNCTION fail_dl() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.path = 'bad' THEN RAISE 'forced'; END IF; RETURN NEW; END $$`,
			`CREATE TRIGGER fail_dl BEFORE UPDATE ON assets FOR EACH ROW EXECUTE FUNCTION fail_dl()`,
			`CREATE FUNCTION fail_rq() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.repo = 'bad' THEN RAISE 'forced'; END IF; RETURN NEW; END $$`,
			`CREATE TRIGGER fail_rq BEFORE INSERT ON request_stats FOR EACH ROW EXECUTE FUNCTION fail_rq()`,
		}
	}
	for _, ddl := range ddls {
		if _, err := db.sql.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	if err := db.FlushDownloads(ctx, []DownloadDelta{
		{Repo: "r", Path: "a", N: 3, Last: t0},
		{Repo: "r", Path: "bad", N: 1, Last: t0},
	}); err == nil {
		t.Fatal("want error")
	}
	if a, _ := db.GetAsset(ctx, "r", "a"); a.Downloads != 0 || !a.LastDownloaded.IsZero() {
		t.Fatalf("first row of the failed batch was applied: %+v", a)
	}
	if err := db.FlushRequests(ctx, []RequestDelta{
		{Hour: 3600, Repo: "r", Requests: 1, Bytes: 1},
		{Hour: 3600, Repo: "bad", Requests: 1, Bytes: 1},
	}); err == nil {
		t.Fatal("want error")
	}
	if b, _ := db.RequestStats(ctx, 0, 1<<40, 3600, ""); len(b) != 0 {
		t.Fatalf("first row of the failed batch was applied: %+v", b)
	}
}
