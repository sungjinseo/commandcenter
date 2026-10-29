package meta

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// openTestDB opens a new database and returns it with the target that
// reopens it.
func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := metatest.URL(t, t.TempDir())
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestImportRepositoriesOnce(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()
	first := []config.Repository{
		{Name: "b", Format: "maven", Type: "hosted"},
		{Name: "a", Format: "npm", Type: "hosted"},
	}
	imported, err := db.ImportRepositories(ctx, first)
	if err != nil || !imported {
		t.Fatalf("first import: %v %v", imported, err)
	}
	db.Close()

	// A second start, with a changed config, must not re-import.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	imported, err = db2.ImportRepositories(ctx, []config.Repository{{Name: "c", Format: "go", Type: "hosted"}})
	if err != nil || imported {
		t.Fatalf("second import: %v %v", imported, err)
	}
	got, err := db2.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("repositories = %+v", got)
	}
}

func TestImportEmptyListStillMarksImported(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if imported, err := db.ImportRepositories(ctx, nil); err != nil || !imported {
		t.Fatalf("first: %v %v", imported, err)
	}
	if imported, err := db.ImportRepositories(ctx, []config.Repository{{Name: "x", Format: "go", Type: "hosted"}}); err != nil || imported {
		t.Fatalf("second: %v %v", imported, err)
	}
}

func TestRepositoryCRUD(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	no := false
	p := config.Repository{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/",
		RemoteAuth:     &config.RemoteAuth{Username: "u", Password: "secret"},
		MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute),
		AnonymousRead: &no}
	if err := db.CreateRepository(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRepository(ctx, p); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	got, err := db.ListRepositories(ctx)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], p) {
		t.Fatalf("list = %+v, %v; want %+v", got, err, p)
	}

	p.ContentMaxAge = config.Duration(48 * time.Hour)
	p.RemoteAuth = nil
	if err := db.UpdateRepository(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ListRepositories(ctx)
	if len(got) != 1 || !reflect.DeepEqual(got[0], p) {
		t.Fatalf("after update = %+v", got)
	}
	if err := db.UpdateRepository(ctx, config.Repository{Name: "missing", Format: "go", Type: "hosted"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}

	if _, err := db.DeleteRepository(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteRepository(ctx, "p"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	if got, _ := db.ListRepositories(ctx); len(got) != 0 {
		t.Fatalf("after delete = %+v", got)
	}
}

func TestDeleteRepositoryPurgesOnlyThatRepo(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for _, name := range []string{"a", "ab", "b"} {
		if err := db.CreateRepository(ctx, config.Repository{Name: name, Format: "npm", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutAsset(ctx, Asset{Repo: name, Path: "x/1.tgz", BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutNegative(ctx, name, "missing", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		err := db.Tx(ctx, func(tx *Tx) error {
			if err := tx.InsertPackage(ctx, Package{Repo: name, Format: "npm", Name: "x", Version: "1.0.0", Attrs: "{}"}); err != nil {
				return err
			}
			return tx.SetDistTag(ctx, name, "x", "latest", "1.0.0")
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DeleteRepository(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	count := func(table, repo string) int {
		var n int
		if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE repo = ?`, repo).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{"assets", "packages", "dist_tags", "negative_cache"} {
		if n := count(table, "a"); n != 0 {
			t.Fatalf("%s still has %d rows for a", table, n)
		}
		for _, other := range []string{"ab", "b"} {
			if n := count(table, other); n != 1 {
				t.Fatalf("%s has %d rows for %s, want 1", table, n, other)
			}
		}
	}
}

// QA N-25: a repository delete removes, in the same transaction, the role
// privileges on exactly that repository, so a repository created later
// under the name does not get them. Other targets stay as they were.
func TestDeleteRepositoryDropsRolePrivileges(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for _, name := range []string{"a", "ab"} {
		if err := db.CreateRepository(ctx, config.Repository{Name: name, Format: "npm", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
	}
	read := []string{ActionRead}
	roles := []Role{
		{ID: "mixed", Privileges: []Privilege{{Target: "*", Actions: read}, {Target: "repo:a", Actions: read},
			{Target: "repo:ab", Actions: read}, {Target: "format:npm", Actions: read}}},
		{ID: "only-a", Privileges: []Privilege{{Target: "repo:a", Actions: []string{ActionRead, ActionAdd}}}},
		{ID: "other", Privileges: []Privilege{{Target: "repo:ab", Actions: read}, {Target: "repo:A", Actions: read}}},
		{ID: "stale", Privileges: []Privilege{{Target: "repo:gone", Actions: read}}}, // left by an older version
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range roles {
		r.UpdatedAt = old
		if err := db.CreateRole(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.DeleteRepository(ctx, "a")
	if err != nil || !reflect.DeepEqual(got, []string{"mixed", "only-a"}) {
		t.Fatalf("changed roles = %v, %v", got, err)
	}
	if _, err := db.DeleteRepository(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	for id, want := range map[string][]Privilege{
		"mixed":  {{Target: "*", Actions: read}, {Target: "repo:ab", Actions: read}, {Target: "format:npm", Actions: read}},
		"only-a": {},
		"other":  roles[2].Privileges,
		"stale":  roles[3].Privileges,
	} {
		r, err := db.GetRole(ctx, id)
		if err != nil || !reflect.DeepEqual(r.Privileges, want) {
			t.Fatalf("role %s = %+v, %v; want %+v", id, r.Privileges, err, want)
		}
		if changed := id == "mixed" || id == "only-a"; r.UpdatedAt.Equal(old) == changed {
			t.Fatalf("role %s updated at %v (changed: %v)", id, r.UpdatedAt, changed)
		}
	}

	// A failure while changing the roles keeps the repository and its rows.
	if err := db.PutAsset(ctx, Asset{Repo: "ab", Path: "x/1.tgz", BlobSHA256: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `UPDATE roles SET privileges = 'not json' WHERE id = 'stale'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteRepository(ctx, "ab"); err == nil {
		t.Fatal("delete with an unreadable role succeeded")
	}
	if defs, _ := db.ListRepositories(ctx); len(defs) != 1 || defs[0].Name != "ab" {
		t.Fatalf("repositories after the failed delete = %+v", defs)
	}
	if _, err := db.GetAsset(ctx, "ab", "x/1.tgz"); err != nil {
		t.Fatalf("asset after the failed delete: %v", err)
	}
}

func TestCreateRepositoryPurgesLeftoverRows(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	// Rows left under a name that has no definition (a late write that raced
	// with a delete's purge) must not reappear in a new repository.
	for _, repo := range []string{"x", "y"} {
		if err := db.PutAsset(ctx, Asset{Repo: repo, Path: "old.jar", BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutNegative(ctx, repo, "gone", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := db.Tx(ctx, func(tx *Tx) error {
			if err := tx.InsertPackage(ctx, Package{Repo: repo, Format: "npm", Name: "old", Version: "1.0.0", Attrs: "{}"}); err != nil {
				return err
			}
			return tx.SetDistTag(ctx, repo, "old", "latest", "1.0.0")
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateRepository(ctx, config.Repository{Name: "x", Format: "npm", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"assets", "packages", "dist_tags", "negative_cache"} {
		var nx, ny int
		if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE repo = ?`, "x").Scan(&nx); err != nil {
			t.Fatal(err)
		}
		if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE repo = ?`, "y").Scan(&ny); err != nil {
			t.Fatal(err)
		}
		if nx != 0 || ny != 1 {
			t.Fatalf("%s: x=%d (want 0) y=%d (want 1)", table, nx, ny)
		}
	}
}

func TestImportKeepsExistingRows(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	// Data written before plan 9 belongs to the imported repositories.
	if err := db.PutAsset(ctx, Asset{Repo: "x", Path: "a.jar", BlobSHA256: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportRepositories(ctx, []config.Repository{{Name: "x", Format: "maven", Type: "hosted"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAsset(ctx, "x", "a.jar"); err != nil {
		t.Fatalf("import purged existing data: %v", err)
	}
}

func TestUpdateRepositoryRemoteChangeClearsNegativeCache(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	p := config.Repository{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/"}
	q := config.Repository{Name: "q", Format: "maven", Type: "proxy", Remote: "https://x.example/"}
	for _, d := range []config.Repository{p, q} {
		if err := db.CreateRepository(ctx, d); err != nil {
			t.Fatal(err)
		}
		if err := db.PutNegative(ctx, d.Name, "a/b", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	hit := func(repo string) bool {
		ok, err := db.NegativeHit(ctx, repo, "a/b", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	p.ContentMaxAge = config.Duration(time.Hour) // remote unchanged
	if err := db.UpdateRepository(ctx, p); err != nil || !hit("p") {
		t.Fatalf("an update that keeps the remote keeps the negative cache (err %v, hit %v)", err, hit("p"))
	}
	p.Remote = "https://y.example/"
	if err := db.UpdateRepository(ctx, p); err != nil {
		t.Fatal(err)
	}
	if hit("p") {
		t.Fatal("a remote change must clear the repository's negative cache")
	}
	if !hit("q") {
		t.Fatal("other repositories keep their negative cache")
	}
}
