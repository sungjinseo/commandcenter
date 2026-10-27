package meta

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// The tests in this file pin the SQLite semantics the code relies on, so
// they hold on PostgreSQL too (COMMANDCENTER_TEST_POSTGRES_URL, see
// metatest).

func TestRebind(t *testing.T) {
	for in, want := range map[string]string{
		`SELECT 1`:                               `SELECT 1`,
		`a = ? AND b IN (?,?) LIMIT ?`:           `a = $1 AND b IN ($2,$3) LIMIT $4`,
		`x = '?' AND y = ? AND "q?" = 'it''s ?'`: `x = '?' AND y = $1 AND "q?" = 'it''s ?'`,
	} {
		if got := rebind(in); got != want {
			t.Errorf("rebind(%q) = %q, want %q", in, got, want)
		}
	}
}

// User and role ids are unique ignoring ASCII case (QA N-18), and only
// ASCII case, as SQLite's NOCASE.
func TestIDsUniqueIgnoringASCIICase(t *testing.T) {
	db := openTest(t)
	if err := db.CreateUser(ctx, User{ID: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, User{ID: "aLICE"}); !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), `"Alice"`) {
		t.Fatalf("aLICE next to Alice: %v", err)
	}
	if err := db.CreateRole(ctx, Role{ID: "ADMIN"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRole(ctx, Role{ID: "admin"}); !errors.Is(err, ErrExists) {
		t.Fatalf("admin next to ADMIN: %v", err)
	}
	for _, id := range []string{"é", "É"} { // not ASCII: not folded
		if err := db.CreateRole(ctx, Role{ID: id}); err != nil {
			t.Fatalf("role %q: %v", id, err)
		}
	}
}

// Searches match a substring ignoring ASCII case only, with no wildcard
// characters, as SQLite's instr(lower(...), lower(?)).
func TestSearchFoldsASCIIOnly(t *testing.T) {
	db := openTest(t)
	for _, p := range []string{"Dir/ReadMe.TXT", "x/a_b", "x/axb", "x/100%", "u/é", "u/É"} {
		if err := db.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string][]string{
		"readme.txt": {"Dir/ReadMe.TXT"},
		"DIR/":       {"Dir/ReadMe.TXT"},
		"a_b":        {"x/a_b"},
		"%":          {"x/100%"},
		"é":          {"u/é"},
		"É":          {"u/É"},
	} {
		hits, _, err := db.SearchAssets(ctx, []string{"r"}, q, 10)
		var got []string
		for _, h := range hits {
			got = append(got, h.Path)
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("SearchAssets(%q) = %v, %v; want %v", q, got, err, want)
		}
		infos, _, err := db.SearchAssetInfos(ctx, []string{"r"}, q, 10)
		if err != nil || len(infos) != len(want) {
			t.Errorf("SearchAssetInfos(%q) = %+v, %v; want %v", q, infos, err, want)
		}
	}
	for _, n := range []string{"Foo_Bar", "foo-bar"} {
		if err := db.Tx(ctx, func(tx *Tx) error {
			return tx.InsertPackage(ctx, Package{Repo: "p", Format: "pypi", Name: n, Version: "1", Attrs: "{}"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if names, _, err := db.SearchPackageNames(ctx, "p", "O_b", 10); err != nil || !reflect.DeepEqual(names, []string{"Foo_Bar"}) {
		t.Fatalf("SearchPackageNames: %v %v", names, err)
	}
	if names, err := db.ListPackageNamesCanonical(ctx, "p", "foo-bar"); err != nil || !reflect.DeepEqual(names, []string{"Foo_Bar", "foo-bar"}) {
		t.Fatalf("ListPackageNamesCanonical: %v %v", names, err)
	}
}

// Text sorts and ranges byte-wise (SQLite's BINARY), whatever the
// database's collation: "B" < "Z" < "a" < "é".
func TestTextOrderIsBytewise(t *testing.T) {
	db := openTest(t)
	for _, p := range []string{"é", "a", "Z", "B", "a/x", "A/y", "ab"} {
		if err := db.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := db.ListAssets(ctx, "r", "")
	var got []string
	for _, a := range list {
		got = append(got, a.Path)
	}
	if want := []string{"A/y", "B", "Z", "a", "a/x", "ab", "é"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAssets = %v, %v; want %v", got, err, want)
	}
	if list, err := db.ListAssets(ctx, "r", "a/"); err != nil || len(list) != 1 || list[0].Path != "a/x" {
		t.Fatalf("ListAssets(a/) = %+v, %v", list, err)
	}
	dir, _, err := db.ListDir(ctx, "r", "", 10)
	var names []string
	for _, e := range dir {
		names = append(names, e.Name)
	}
	if want := []string{"A", "B", "Z", "a", "a", "ab", "é"}; err != nil || !reflect.DeepEqual(names, want) {
		t.Fatalf("ListDir = %v, %v; want %v", names, err, want)
	}
	for _, id := range []string{"bob", "Carol", "alice"} {
		if err := db.CreateUser(ctx, User{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	users, err := db.ListUsers(ctx)
	if err != nil || len(users) != 3 || users[0].ID != "Carol" || users[1].ID != "alice" {
		t.Fatalf("ListUsers = %+v, %v; want Carol, alice, bob", users, err)
	}
}

// JSON is stored as text, so a document reads back byte for byte, and the
// queries into it (json_extract, json_each) match as SQLite's do.
func TestJSONQueries(t *testing.T) {
	db := openTest(t)
	attrs := `{ "name" : "Serde_JSON",  "deps":[1, 2.50] }`
	if err := db.Tx(ctx, func(tx *Tx) error {
		return tx.InsertPackage(ctx, Package{Repo: "c", Format: "cargo", Name: "serde_json", Version: "1.0.0", Attrs: attrs})
	}); err != nil {
		t.Fatal(err)
	}
	if list, err := db.ListPackages(ctx, "c", "serde_json"); err != nil || len(list) != 1 || list[0].Attrs != attrs {
		t.Fatalf("attrs read back as %+v, %v; want %q", list, err, attrs)
	}
	if n, err := db.PublishedName(ctx, "c", "serde_json"); err != nil || n != "Serde_JSON" {
		t.Fatalf("PublishedName = %q, %v", n, err)
	}
	if n, err := db.PublishedName(ctx, "c", "none"); err != nil || n != "" {
		t.Fatalf("PublishedName of no package = %q, %v", n, err)
	}

	for _, def := range []config.Repository{
		{Name: "b", Format: "maven", Type: "hosted", CleanupPolicy: "weekly"},
		{Name: "a", Format: "maven", Type: "hosted", CleanupPolicy: "weekly"},
		{Name: "c", Format: "maven", Type: "hosted", CleanupPolicy: "weekly-2"},
		{Name: "d", Format: "maven", Type: "hosted"},
	} {
		if err := db.CreateRepository(ctx, def); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := db.RepositoriesUsingPolicy(ctx, "weekly"); err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("RepositoriesUsingPolicy = %v, %v", got, err)
	}

	if err := db.CreateRole(ctx, Role{ID: "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, User{ID: "u", Roles: []string{"ops", "dev"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRole(ctx, "dev"); !errors.Is(err, ErrInUse) {
		t.Fatalf("DeleteRole of a held role: %v", err)
	}
	if err := db.DeleteRole(ctx, "DEV"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteRole of another case: %v", err)
	}
}

// Writers queue one at a time, as with SQLite's BEGIN IMMEDIATE: a
// transaction that reads a value and writes it back increased loses no
// update, neither to another transaction nor to a single-statement write.
func TestWritersAreSerialized(t *testing.T) {
	db := openTest(t)
	if err := db.SetSetting(ctx, "n", "0"); err != nil {
		t.Fatal(err)
	}
	const workers, rounds = 6, 15
	var wg sync.WaitGroup
	errs := make(chan error, 2*workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				errs <- db.Tx(ctx, func(tx *Tx) error {
					var v string
					if err := tx.q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'n'`).Scan(&v); err != nil {
						return err
					}
					n, err := strconv.Atoi(v)
					if err != nil {
						return err
					}
					time.Sleep(time.Millisecond) // another writer would get in here
					_, err = tx.q.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = 'n'`, strconv.Itoa(n+1))
					return err
				})
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				_, err := db.sql.ExecContext(ctx, `UPDATE settings SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT) WHERE key = 'n'`)
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if v, err := db.GetSetting(ctx, "n"); err != nil || v != strconv.Itoa(2*workers*rounds) {
		t.Fatalf("n = %s, %v; want %d", v, err, 2*workers*rounds)
	}
}

// Reads do not wait for a writer, as with WAL.
func TestReadsDoNotWaitForTheWriter(t *testing.T) {
	db := openTest(t)
	if err := db.SetSetting(ctx, "k", "before"); err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.q.ExecContext(ctx, `UPDATE settings SET value = 'during' WHERE key = 'k'`); err != nil {
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		v, err := db.GetSetting(rctx, "k")
		if err != nil || v != "before" {
			t.Errorf("read beside the writer: %q, %v; want the committed value", v, err)
		}
		if _, err := db.LoadAccess(rctx); err != nil {
			t.Errorf("LoadAccess beside the writer: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// On PostgreSQL the upgrade is one transaction too: when a step fails, the
// tables made before it and the version table are gone again.
func TestOpenPostgresInOneTransaction(t *testing.T) {
	if !metatest.Postgres() {
		t.Skip("PostgreSQL's upgrade; TestOpenMigratesInOneTransaction is SQLite's")
	}
	target := metatest.Target(t, filepath.Join(t.TempDir(), "meta.db"))
	raw := rawOpen(t, target)
	if _, err := raw.db.Exec(`CREATE TABLE assets(repo TEXT)`); err != nil { // no blob_sha256 to index
		t.Fatal(err)
	}
	if _, err := Open(target); err == nil || !strings.Contains(err.Error(), "blob_sha256") {
		t.Fatalf("Open: %v, want the assets index to fail", err)
	}
	var tables int
	if err := raw.db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('users', 'schema_version')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("the failed Open left %d of users and schema_version (err %v)", tables, err)
	}
	if _, err := raw.db.Exec(`DROP TABLE assets`); err != nil {
		t.Fatal(err)
	}
	db, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v := userVersion(t, db); v != schemaVersion {
		t.Fatalf("schema version %d, want %d", v, schemaVersion)
	}
}

func TestDriverAndSize(t *testing.T) {
	db := openTest(t)
	want := "sqlite"
	if metatest.Postgres() {
		want = "postgresql"
	}
	if db.Driver() != want {
		t.Fatalf("Driver = %q, want %q", db.Driver(), want)
	}
	if n, err := db.SizeBytes(ctx); err != nil || n <= 0 {
		t.Fatalf("SizeBytes = %d, %v", n, err)
	}
}

// SQLite stores any bytes as text, PostgreSQL only valid UTF-8 without NUL.
// There a value it cannot store matches no row, and a write of it is
// refused with ErrInvalidText (repo.StatusOf: 400) instead of failing on
// the server.
func TestUnstorableText(t *testing.T) {
	db := openTest(t)
	for _, p := range []string{"a\x00b", "caf\xe9"} {
		err := db.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s"})
		_, getErr := db.GetAsset(ctx, "r", p)
		if !db.sql.pg {
			if err != nil || getErr != nil {
				t.Fatalf("SQLite %q: put %v, get %v", p, err, getErr)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidText) || !errors.Is(getErr, ErrNotFound) {
			t.Fatalf("%q: put %v, get %v; want ErrInvalidText and ErrNotFound", p, err, getErr)
		}
		txErr := db.Tx(ctx, func(tx *Tx) error { return tx.PutAsset(ctx, Asset{Repo: "r", Path: p, BlobSHA256: "s"}) })
		list, listErr := db.ListAssets(ctx, "r", p)
		hits, _, searchErr := db.SearchAssets(ctx, []string{"r"}, p, 10)
		hit, negErr := db.NegativeHit(ctx, "r", p, time.Now())
		if !errors.Is(txErr, ErrInvalidText) || listErr != nil || len(list) != 0 || searchErr != nil || len(hits) != 0 || negErr != nil || hit {
			t.Fatalf("%q: tx %v, list %v %v, search %v %v, negative %v %v", p, txErr, list, listErr, hits, searchErr, hit, negErr)
		}
	}
}

// Review postgres-2: CreateUser and CreateBlobStore check and insert under
// one write lock, so concurrent creates of a look-alike id or the same name
// end with ErrExists for one of them, never two rows or a raw constraint
// error.
func TestCreatesCheckAndInsertUnderTheWriteLock(t *testing.T) {
	db := openTest(t)
	race := func(i int, a, b string, create func(id string) error) {
		t.Helper()
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, id := range []string{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[j] = create(id)
			}()
		}
		close(start)
		wg.Wait()
		if errs[0] == nil && errs[1] == nil {
			t.Fatalf("attempt %d: both %q and %q were created", i, a, b)
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, ErrExists) {
				t.Fatalf("attempt %d: a create failed with a raw error, not ErrExists: %v", i, err)
			}
		}
	}
	for i := 0; i < 100; i++ {
		race(i, fmt.Sprintf("User%d", i), fmt.Sprintf("user%d", i), func(id string) error { return db.CreateUser(ctx, User{ID: id}) })
		name := fmt.Sprintf("bs%d", i)
		race(i, name, name, func(id string) error { return db.CreateBlobStore(ctx, BlobStore{Name: id, Path: "/x"}) })
	}
}

// Review postgres-1: an up-to-date database opens without DDL rights, so a
// role with DML on the tables (created by a DBA, or by migrate-db with the
// admin URL) runs the server; one that needs an upgrade says what the role
// must be.
func TestOpenAsLeastPrivilegeRole(t *testing.T) {
	if !metatest.Postgres() {
		t.Skip("PostgreSQL only")
	}
	target := metatest.Target(t, filepath.Join(t.TempDir(), "meta.db"))
	db, err := Open(target) // the owner creates the tables
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	admin := rawOpen(t, target)
	var dbName string
	if err := admin.db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf("ccapp_%d", time.Now().UnixNano())
	for _, q := range []string{
		`CREATE ROLE ` + role + ` LOGIN PASSWORD 'pw'`,
		`GRANT CONNECT ON DATABASE ` + dbName + ` TO ` + role,
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		if _, err := admin.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{`DROP OWNED BY ` + role, `DROP ROLE ` + role} {
			if _, err := admin.db.Exec(q); err != nil {
				t.Error(q, err)
			}
		}
	})
	u, _ := url.Parse(target)
	u.User = url.UserPassword(role, "pw")
	app, err := Open(u.String())
	if err != nil {
		t.Fatalf("Open as a role with DML rights only on an up-to-date schema: %v", err)
	}
	if err := app.PutAsset(ctx, Asset{Repo: "r", Path: "p", BlobSHA256: "s"}); err != nil {
		t.Fatal(err)
	}
	app.Close()
	if err := stampSchemaVersion(ctx, admin, schemaVersion-1); err != nil {
		t.Fatal(err)
	}
	_, err = Open(u.String())
	if err == nil || !strings.Contains(err.Error(), "42501") || !strings.Contains(err.Error(), "OWNER") {
		t.Fatalf("Open of a database to upgrade as the DML-only role: %v, want the privilege error naming the owner", err)
	}
	t.Log(err)
}
