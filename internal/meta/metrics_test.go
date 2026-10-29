package meta

import (
	"testing"
	"time"
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

func TestFlushRollsBackWholeBatchOnError(t *testing.T) {
	db := openTest(t)
	for _, p := range []string{"a", "bad"} {
		if err := db.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	// A trigger forces the second row of each batch to fail.
	ddls := []string{
		`CREATE FUNCTION fail_dl() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.path = 'bad' THEN RAISE 'forced'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER fail_dl BEFORE UPDATE ON assets FOR EACH ROW EXECUTE FUNCTION fail_dl()`,
		`CREATE FUNCTION fail_rq() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.repo = 'bad' THEN RAISE 'forced'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER fail_rq BEFORE INSERT ON request_stats FOR EACH ROW EXECUTE FUNCTION fail_rq()`,
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

func TestRepoStatsSumsCountersAndKeepsMaxima(t *testing.T) {
	db := openTest(t)
	base := int64(1_700_000_000) / 3600 * 3600
	for _, d := range []RequestDelta{
		{Hour: base, Repo: "p", Requests: 2, Bytes: 10, Errors: 1, BytesIn: 3, Ms: 40, MaxMs: 30, Upstream: 1, UpstreamFailures: 1, UpstreamMs: 500, UpstreamMaxMs: 500, CacheHits: 1},
		{Hour: base, Repo: "p", Requests: 1, Ms: 5, MaxMs: 5, Upstream: 1, UpstreamMs: 100, UpstreamMaxMs: 100, CacheMisses: 1}, // same hour: added, maxima kept
		{Hour: base + 3600, Repo: "p", Requests: 1, MaxMs: 90},
		{Hour: base - 3600, Repo: "p", Requests: 100}, // outside
		{Hour: base, Repo: "h", Requests: 1},
	} {
		if err := db.FlushRequests(ctx, []RequestDelta{d}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.RepoTrafficStats(ctx, base, base+7200)
	if err != nil {
		t.Fatal(err)
	}
	want := []RepoTraffic{{Repo: "h", Requests: 1}, {Repo: "p", Requests: 4, Bytes: 10, Errors: 1, BytesIn: 3, Ms: 45, MaxMs: 90,
		Upstream: 2, UpstreamFailures: 1, UpstreamMs: 600, UpstreamMaxMs: 500, CacheHits: 1, CacheMisses: 1}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
