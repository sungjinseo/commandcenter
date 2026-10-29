package meta

import (
	"testing"
	"time"
)

func TestTransferQueries(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	for i, p := range []string{"a", "b", "c"} {
		if err := db.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s" + p, Size: int64(i),
			CreatedAt: t0, LastModified: t0.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.AssetPage(ctx, "r", "a", time.Time{}, 10)
	if err != nil || len(page) != 2 || page[0].Path != "b" || page[1].BlobSHA256 != "sc" {
		t.Fatalf("page after a: %+v %v", page, err)
	}
	if page, _ := db.AssetPage(ctx, "r", "", t0.Add(90*time.Minute), 10); len(page) != 1 || page[0].Path != "c" {
		t.Fatalf("since: %+v", page)
	}
	if page, _ := db.AssetPage(ctx, "r", "", time.Time{}, 1); len(page) != 1 || page[0].Path != "a" {
		t.Fatalf("limit: %+v", page)
	}

	// ImportAsset keeps every column, the creation time and counters included,
	// also over an existing row.
	want := Asset{Repo: "r", Path: "a", BlobSHA256: "new", Size: 9, ContentType: "x/y", SHA1: "1", MD5: "5", SHA512: "512",
		CreatedAt: t0.Add(-time.Hour), LastModified: t0.Add(-time.Minute), RemoteETag: "e", RemoteLastModified: "lm",
		CheckedAt: t0, Downloads: 7, LastDownloaded: t0.Add(time.Minute)}
	if err := db.Tx(ctx, func(tx *Tx) error { return tx.ImportAsset(ctx, want) }); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetAsset(ctx, "r", "a"); err != nil || got != want {
		t.Fatalf("imported asset\n got %+v %v\nwant %+v", got, err, want)
	}

	row := Package{Repo: "r", Format: "npm", Name: "p", Version: "1", Attrs: `{"a":1}`, Yanked: true, CreatedAt: t0}
	err = db.Tx(ctx, func(tx *Tx) error {
		if err := tx.ImportPackage(ctx, row, false); err != nil {
			return err
		}
		changed := row
		changed.Attrs, changed.Yanked = `{"a":2}`, false
		return tx.ImportPackage(ctx, changed, false) // kept
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ListPackages(ctx, "r", "p"); len(got) != 1 || got[0] != row {
		t.Fatalf("insert-if-missing: %+v", got)
	}
	changed := row
	changed.Attrs, changed.Yanked, changed.CreatedAt = `{"a":2}`, false, t0.Add(time.Hour)
	if err := db.Tx(ctx, func(tx *Tx) error { return tx.ImportPackage(ctx, changed, true) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ListPackages(ctx, "r", "p"); len(got) != 1 || got[0] != changed {
		t.Fatalf("replace: %+v", got)
	}
	if n, err := db.CountPackages(ctx, "r"); err != nil || n != 1 {
		t.Fatalf("count: %d %v", n, err)
	}
}
