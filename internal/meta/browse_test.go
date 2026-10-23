package meta

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func putPaths(t *testing.T, db *DB, repo string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := db.PutAsset(ctx, Asset{Repo: repo, Path: p, BlobSHA256: "blob-" + p, Size: int64(len(p))}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListDir(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	if err := db.PutAsset(ctx, Asset{Repo: "r", Path: "a/b", BlobSHA256: "x", Size: 7, LastModified: t0}); err != nil {
		t.Fatal(err)
	}
	putPaths(t, db, "r", "a/b/c", "a/b/d/e", "a/b-x", "a/b0", "z")
	putPaths(t, db, "other", "a/q")

	root, trunc, err := db.ListDir(ctx, "r", "", 100)
	if err != nil || trunc {
		t.Fatalf("root: %v trunc=%v", err, trunc)
	}
	want := []DirEntry{{Name: "a", Dir: true}, {Name: "z", Size: 1}}
	if !reflect.DeepEqual(root, want) {
		t.Fatalf("root = %+v, want %+v", root, want)
	}

	a, _, err := db.ListDir(ctx, "r", "a/", 100)
	if err != nil {
		t.Fatal(err)
	}
	want = []DirEntry{
		{Name: "b", Size: 7, LastModified: t0},
		{Name: "b-x", Size: 5},
		{Name: "b", Dir: true},
		{Name: "b0", Size: 4},
	}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("a/ = %+v, want %+v", a, want)
	}

	b, _, err := db.ListDir(ctx, "r", "a/b/", 100)
	if err != nil {
		t.Fatal(err)
	}
	want = []DirEntry{{Name: "c", Size: 5}, {Name: "d", Dir: true}}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("a/b/ = %+v, want %+v", b, want)
	}
}

func TestListDirTruncatesAndEmpty(t *testing.T) {
	db := openTest(t)
	got, trunc, err := db.ListDir(ctx, "r", "", 10)
	if err != nil || trunc || len(got) != 0 {
		t.Fatalf("empty: %+v %v %v", got, trunc, err)
	}
	putPaths(t, db, "r", "a/1", "a/2", "b", "c/x")
	got, trunc, err = db.ListDir(ctx, "r", "", 2)
	if err != nil || !trunc || len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("limit 2: %+v %v %v", got, trunc, err)
	}
	got, trunc, err = db.ListDir(ctx, "r", "", 3)
	if err != nil || trunc || len(got) != 3 {
		t.Fatalf("limit 3: %+v %v %v", got, trunc, err)
	}
}

func TestSearchAssets(t *testing.T) {
	db := openTest(t)
	putPaths(t, db, "r1", "org/Foo/1.0/foo.jar", "org/bar/bar.jar")
	putPaths(t, db, "r2", "x/FOO.txt", "x/other")
	putPaths(t, db, "r3", "foo/ignored")

	hits, trunc, err := db.SearchAssets(ctx, []string{"r1", "r2"}, "foo", 10)
	if err != nil || trunc {
		t.Fatal(err, trunc)
	}
	want := []AssetHit{{"r1", "org/Foo/1.0/foo.jar"}, {"r2", "x/FOO.txt"}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits = %+v, want %+v", hits, want)
	}

	hits, trunc, err = db.SearchAssets(ctx, []string{"r1", "r2"}, "foo", 1)
	if err != nil || !trunc || len(hits) != 1 || hits[0] != want[0] {
		t.Fatalf("truncated: %+v %v %v", hits, trunc, err)
	}

	hits, trunc, err = db.SearchAssets(ctx, nil, "foo", 10)
	if err != nil || trunc || len(hits) != 0 {
		t.Fatalf("no repos: %+v %v %v", hits, trunc, err)
	}
}

func TestSearchPackageNames(t *testing.T) {
	db := openTest(t)
	err := db.Tx(ctx, func(tx *Tx) error {
		for _, p := range []Package{
			{Repo: "r", Format: "npm", Name: "left-pad", Version: "1"},
			{Repo: "r", Format: "npm", Name: "left-pad", Version: "2"},
			{Repo: "r", Format: "npm", Name: "Right", Version: "1"},
			{Repo: "other", Format: "npm", Name: "left-other", Version: "1"},
		} {
			if err := tx.InsertPackage(ctx, p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all, trunc, err := db.SearchPackageNames(ctx, "r", "", 10)
	if err != nil || trunc || !reflect.DeepEqual(all, []string{"Right", "left-pad"}) {
		t.Fatalf("all: %v %v %v", all, trunc, err)
	}
	got, _, err := db.SearchPackageNames(ctx, "r", "LEFT", 10)
	if err != nil || !reflect.DeepEqual(got, []string{"left-pad"}) {
		t.Fatalf("query: %v %v", got, err)
	}
	got, trunc, err = db.SearchPackageNames(ctx, "r", "", 1)
	if err != nil || !trunc || len(got) != 1 {
		t.Fatalf("trunc: %v %v %v", got, trunc, err)
	}
}

func TestStats(t *testing.T) {
	db := openTest(t)
	for _, a := range []Asset{
		{Repo: "r1", Path: "a", BlobSHA256: "same", Size: 10},
		{Repo: "r2", Path: "b", BlobSHA256: "same", Size: 10},
		{Repo: "r2", Path: "c", BlobSHA256: "other", Size: 5},
	} {
		if err := db.PutAsset(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := db.RepoStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]RepoStat{"r1": {1, 10}, "r2": {2, 15}}
	if !reflect.DeepEqual(rs, want) {
		t.Fatalf("RepoStats = %+v, want %+v", rs, want)
	}
	blobs, bytes, err := db.BlobStats(ctx)
	if err != nil || blobs != 2 || bytes != 15 {
		t.Fatalf("BlobStats = %d %d %v, want 2 15", blobs, bytes, err)
	}

	empty := openTest(t)
	blobs, bytes, err = empty.BlobStats(ctx)
	if err != nil || blobs != 0 || bytes != 0 {
		t.Fatalf("empty BlobStats = %d %d %v", blobs, bytes, err)
	}
}

func TestListGetDeleteTokens(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	for i, c := range []struct{ id, user string }{{"t1", "alice"}, {"t2", "bob"}, {"t3", "alice"}} {
		if err := db.CreateToken(ctx, c.id, c.user, "hash-"+c.id, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(ts []Token) []string {
		var out []string
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}

	all, trunc, err := db.ListTokens(ctx, "", 10)
	if err != nil || trunc || !reflect.DeepEqual(ids(all), []string{"t3", "t2", "t1"}) {
		t.Fatalf("all: %v %v %v", ids(all), trunc, err)
	}
	own, _, err := db.ListTokens(ctx, "alice", 10)
	if err != nil || !reflect.DeepEqual(ids(own), []string{"t3", "t1"}) {
		t.Fatalf("own: %v %v", ids(own), err)
	}
	if own[0].User != "alice" || !own[0].Created.Equal(t0.Add(2*time.Second)) || !own[0].LastUsed.IsZero() {
		t.Fatalf("token fields: %+v", own[0])
	}
	cut, trunc, err := db.ListTokens(ctx, "", 2)
	if err != nil || !trunc || !reflect.DeepEqual(ids(cut), []string{"t3", "t2"}) {
		t.Fatalf("cut: %v %v %v", ids(cut), trunc, err)
	}

	g, err := db.GetToken(ctx, "t2")
	if err != nil || g.User != "bob" {
		t.Fatalf("GetToken: %+v %v", g, err)
	}
	if _, err := db.GetToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetToken missing: %v", err)
	}
	if err := db.DeleteToken(ctx, "t2"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetToken(ctx, "t2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := db.DeleteToken(ctx, "t2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestListVersions(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	err := db.Tx(ctx, func(tx *Tx) error {
		for i, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
			p := Package{Repo: "r", Format: "npm", Name: "p", Version: v, Attrs: `{"big":"doc"}`,
				CreatedAt: t0.Add(time.Duration(i) * time.Hour), Yanked: v == "1.1.0"}
			if err := tx.InsertPackage(ctx, p); err != nil {
				return err
			}
		}
		// same created time: version DESC breaks the tie
		if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "p", Version: "1.9.0", CreatedAt: t0.Add(2 * time.Hour)}); err != nil {
			return err
		}
		return tx.InsertPackage(ctx, Package{Repo: "other", Format: "npm", Name: "p", Version: "9.0.0", CreatedAt: t0})
	})
	if err != nil {
		t.Fatal(err)
	}
	all, trunc, err := db.ListVersions(ctx, "r", "p", 10)
	if err != nil || trunc {
		t.Fatalf("all: %v trunc=%v", err, trunc)
	}
	want := []VersionInfo{
		{Version: "2.0.0", Created: t0.Add(2 * time.Hour)},
		{Version: "1.9.0", Created: t0.Add(2 * time.Hour)},
		{Version: "1.1.0", Yanked: true, Created: t0.Add(time.Hour)},
		{Version: "1.0.0", Created: t0},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("all = %+v, want %+v", all, want)
	}
	two, trunc, err := db.ListVersions(ctx, "r", "p", 2)
	if err != nil || !trunc || !reflect.DeepEqual(two, want[:2]) {
		t.Fatalf("limit 2 = %+v trunc=%v err=%v", two, trunc, err)
	}
	exact, trunc, _ := db.ListVersions(ctx, "r", "p", 4)
	if trunc || len(exact) != 4 {
		t.Fatalf("limit 4: %d trunc=%v", len(exact), trunc)
	}
	none, trunc, err := db.ListVersions(ctx, "r", "missing", 10)
	if err != nil || trunc || len(none) != 0 {
		t.Fatalf("missing: %v %v %v", none, trunc, err)
	}
}

func TestSearchAssetInfos(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	if err := db.PutAsset(ctx, Asset{Repo: "r1", Path: "org/Foo/1.0/foo.jar", BlobSHA256: "x", Size: 42, LastModified: t0}); err != nil {
		t.Fatal(err)
	}
	putPaths(t, db, "r1", "org/bar/bar.jar")
	putPaths(t, db, "r2", "x/FOO.txt")
	putPaths(t, db, "r3", "foo/ignored")

	hits, trunc, err := db.SearchAssetInfos(ctx, []string{"r1", "r2"}, "foo", 10)
	if err != nil || trunc || len(hits) != 2 {
		t.Fatalf("hits = %+v trunc=%v err=%v", hits, trunc, err)
	}
	if h := hits[0]; h.Repo != "r1" || h.Path != "org/Foo/1.0/foo.jar" || h.Size != 42 || !h.LastModified.Equal(t0) {
		t.Fatalf("first hit = %+v", h)
	}
	if hits[1].Repo != "r2" || hits[1].Size != int64(len("x/FOO.txt")) {
		t.Fatalf("second hit = %+v", hits[1])
	}

	hits, trunc, err = db.SearchAssetInfos(ctx, []string{"r1", "r2"}, "foo", 1)
	if err != nil || !trunc || len(hits) != 1 {
		t.Fatalf("truncated: %+v %v %v", hits, trunc, err)
	}
	hits, trunc, err = db.SearchAssetInfos(ctx, nil, "foo", 10)
	if err != nil || trunc || len(hits) != 0 {
		t.Fatalf("no repos: %+v %v %v", hits, trunc, err)
	}
}

func TestDeleteTokenByHash(t *testing.T) {
	db := openTest(t)
	if err := db.CreateToken(ctx, "t1", "alice", "hash-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteTokenByHash(ctx, "hash-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.TokenUser(ctx, "hash-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := db.DeleteTokenByHash(ctx, "hash-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
