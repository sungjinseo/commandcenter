package meta

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"commandcenter/internal/config"
)

// liveUsage is what the triggers must keep: the aggregate over assets
// (last_download is the repository's last download, which may have been of
// an asset deleted since: at least the newest of the assets left).
func liveUsage(t *testing.T, db *DB) map[string]RepoUsage {
	t.Helper()
	rows, err := db.sql.QueryContext(ctx, `SELECT repo, SUM(n), SUM(b), SUM(sz), MAX(ld) FROM (SELECT repo, blob_sha256,
  COUNT(*) AS n, SUM(size) AS b, MAX(size) AS sz, MAX(last_downloaded) AS ld FROM assets GROUP BY repo, blob_sha256) x GROUP BY repo`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]RepoUsage{}
	for rows.Next() {
		var u RepoUsage
		var ld int64
		if err := rows.Scan(&u.Repo, &u.Assets, &u.Bytes, &u.UniqueBytes, &ld); err != nil {
			t.Fatal(err)
		}
		u.LastDownload = fromMs(ld)
		out[u.Repo] = u
	}
	return out
}

func checkUsage(t *testing.T, db *DB, step string) map[string]RepoUsage {
	t.Helper()
	got, err := db.RepoUsages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := liveUsage(t, db)
	for repo, g := range got {
		w := want[repo]
		if g.Assets != w.Assets || g.Bytes != w.Bytes || g.UniqueBytes != w.UniqueBytes || g.LastDownload.Before(w.LastDownload) {
			t.Fatalf("%s: %s: counters %+v, aggregate %+v", step, repo, g, w)
		}
	}
	for repo := range want {
		if _, ok := got[repo]; !ok {
			t.Fatalf("%s: %s has assets but no counters", step, repo)
		}
	}
	return got
}

// The triggers keep repo_usage equal to the aggregate over assets through
// inserts, replacements (same and other blobs), deletes, download flushes,
// imports and a repository purge.
func TestUsageCountersFollowAssets(t *testing.T) {
	db := openTest(t)
	rng := rand.New(rand.NewPCG(1, 2))
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	repos, blobs := []string{"r1", "r2"}, []string{"s1", "s2", "s3", "s4"}
	size := map[string]int64{"s1": 10, "s2": 20, "s3": 30, "s4": 40}
	for i := range 400 {
		repo, path := repos[rng.IntN(2)], fmt.Sprintf("p%d", rng.IntN(12))
		switch op := rng.IntN(10); {
		case op < 5:
			b := blobs[rng.IntN(4)]
			if err := db.PutAsset(ctx, Asset{Repo: repo, Path: path, BlobSHA256: b, Size: size[b], LastModified: t0.Add(time.Duration(i) * time.Second)}); err != nil {
				t.Fatal(err)
			}
		case op < 7:
			if err := db.DeleteAsset(ctx, repo, path); err != nil && err != ErrNotFound {
				t.Fatal(err)
			}
		case op < 9:
			if err := db.FlushDownloads(ctx, []DownloadDelta{{Repo: repo, Path: path, N: 2, Last: t0.Add(time.Duration(i) * time.Minute)},
				{Repo: repo, Path: "p1", N: 1, Last: t0}}); err != nil {
				t.Fatal(err)
			}
		default:
			b := blobs[rng.IntN(4)]
			if err := db.Tx(ctx, func(tx *Tx) error {
				return tx.ImportAsset(ctx, Asset{Repo: repo, Path: path, BlobSHA256: b, Size: size[b], Downloads: int64(rng.IntN(3))})
			}); err != nil {
				t.Fatal(err)
			}
		}
		checkUsage(t, db, fmt.Sprintf("step %d", i))
	}
	if err := db.CreateRepository(ctx, config.Repository{Name: "r1", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteRepository(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if got := checkUsage(t, db, "purge"); got["r1"].Repo != "" {
		t.Fatalf("a deleted repository keeps its row: %+v", got["r1"])
	}
	// A changed component count needs a recount; an older count is dropped.
	u, _ := db.RepoUsages(ctx)
	r2 := u["r2"]
	if err := db.SetComponentCount(ctx, "r2", 7, r2.Changes, t0); err != nil {
		t.Fatal(err)
	}
	if err := db.SetComponentCount(ctx, "r2", 3, r2.Changes-1, t0); err != nil {
		t.Fatal(err)
	}
	if u, _ = db.RepoUsages(ctx); u["r2"].Components != 7 || u["r2"].CountedChanges != r2.Changes {
		t.Fatalf("count: %+v", u["r2"])
	}
}

// The monthly rows follow request_stats, downloads and hosted uploads, by
// the month each is dated in; samples keep the peaks.
func TestMonthlyUsage(t *testing.T) {
	db := openTest(t)
	for _, def := range []config.Repository{{Name: "h", Format: "raw", Type: "hosted"}, {Name: "p", Format: "raw", Type: "proxy", Remote: "http://x/"}} {
		if err := db.CreateRepository(ctx, def); err != nil {
			t.Fatal(err)
		}
	}
	oct := time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)
	nov := oct.Add(2 * time.Minute)
	put := func(repo, path string, size int64, at time.Time) {
		if err := db.PutAsset(ctx, Asset{Repo: repo, Path: path, BlobSHA256: path, Size: size, LastModified: at}); err != nil {
			t.Fatal(err)
		}
	}
	put("h", "a", 100, oct)
	put("p", "cached", 50, oct) // a proxy's cache: no upload
	if err := db.SampleUsage(ctx, oct); err != nil {
		t.Fatal(err)
	}
	put("h", "b", 30, nov)
	put("h", "a", 101, nov) // replaced: an upload of November
	if err := db.FlushRequests(ctx, []RequestDelta{{Hour: oct.Unix() / 3600 * 3600, Repo: "h", Requests: 2, Bytes: 7, BytesIn: 100, Errors: 1},
		{Hour: nov.Unix() / 3600 * 3600, Repo: "h", Requests: 3, Bytes: 9}}); err != nil {
		t.Fatal(err)
	}
	if err := db.FlushRequests(ctx, []RequestDelta{{Hour: nov.Unix() / 3600 * 3600, Repo: "h", Requests: 1, Bytes: 1}}); err != nil { // the conflict path
		t.Fatal(err)
	}
	if err := db.FlushDownloads(ctx, []DownloadDelta{{Repo: "h", Path: "a", N: 4, Last: oct}, {Repo: "p", Path: "cached", N: 1, Last: nov}}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAsset(ctx, "h", "a"); err != nil {
		t.Fatal(err)
	}
	if err := db.SampleUsage(ctx, nov); err != nil {
		t.Fatal(err)
	}
	if err := db.FlushSeen(ctx, []SeenKey{{MonthStart(nov).Unix(), "user", "u1"}, {MonthStart(nov).Unix(), "user", "u1"}, {MonthStart(nov).Unix(), "addr", "a1"}}); err != nil {
		t.Fatal(err)
	}
	ms, err := db.MonthlyUsage(ctx, oct, nov.AddDate(0, 1, 0))
	if err != nil || len(ms) != 2 {
		t.Fatalf("%+v %v", ms, err)
	}
	o, n := ms[0], ms[1]
	if !o.Month.Equal(MonthStart(oct)) || o.Requests != 2 || o.Errors != 1 || o.EgressBytes != 7 || o.IngressBytes != 100 ||
		o.Uploads != 1 || o.UploadBytes != 100 || o.Downloads != 4 || o.StorageBytes != 150 || o.PeakStorageBytes != 150 || o.Assets != 2 {
		t.Fatalf("october %+v", o)
	}
	// November started with October's 150 bytes; it peaked there (the samples
	// do not see the 181 bytes between them) and ends with 80.
	if n.Requests != 4 || n.EgressBytes != 10 || n.Uploads != 2 || n.UploadBytes != 131 || n.Downloads != 1 ||
		n.StorageBytes != 80 || n.PeakStorageBytes != 150 || n.Assets != 2 || n.Users != 1 || n.Addresses != 1 {
		t.Fatalf("november %+v", n)
	}
	if err := db.PruneUsage(ctx, nov); err != nil {
		t.Fatal(err)
	}
	if ms, _ := db.MonthlyUsage(ctx, oct, nov.AddDate(0, 1, 0)); len(ms) != 1 || !ms[0].Month.Equal(MonthStart(nov)) {
		t.Fatalf("after prune %+v", ms)
	}
}
