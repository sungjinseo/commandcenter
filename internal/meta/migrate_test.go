package meta

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/meta/metatest"
)

// copyTables names every table of the schema with every one of its columns,
// on both dialects, so a new table or column cannot be left behind.
func TestCopyTablesCoverSchema(t *testing.T) {
	db := openTest(t)
	want := map[string]bool{}
	for _, m := range regexp.MustCompile(`CREATE TABLE IF NOT EXISTS (\w+)`).FindAllStringSubmatch(schema, -1) {
		want[m[1]] = true
	}
	for _, c := range copyTables {
		if !want[c.name] {
			t.Errorf("copyTables lists %s, which the schema lacks", c.name)
		}
		delete(want, c.name)
		var listed []string
		for _, col := range strings.Split(c.cols, ",") {
			listed = append(listed, strings.Trim(strings.TrimSpace(col), `"`))
		}
		var got []string
		q := `SELECT column_name FROM information_schema.columns WHERE table_name = ? ORDER BY ordinal_position`
		if !db.sql.pg {
			q = `SELECT name FROM pragma_table_info(?)`
		}
		rows, err := db.sql.QueryContext(ctx, q, c.name)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			got = append(got, n)
		}
		rows.Close()
		if fmt.Sprint(got) != fmt.Sprint(listed) {
			t.Errorf("%s: database columns %v, copyTables %v", c.name, got, listed)
		}
	}
	for n := range want {
		t.Errorf("table %s is not in copyTables", n)
	}
}

// dump returns every row of every table as text, in a fixed order.
func dump(t *testing.T, db *DB) []string {
	t.Helper()
	var out []string
	for _, c := range copyTables {
		rows, err := db.sql.QueryContext(ctx, `SELECT `+c.cols+` FROM `+c.name+` ORDER BY `+c.cols)
		if err != nil {
			t.Fatal(err)
		}
		n := strings.Count(c.cols, ",") + 1
		vals := make([]any, n)
		ptrs := make([]any, n)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, c.name+": "+fmt.Sprintf("%#v", vals))
		}
		rows.Close()
	}
	return out
}

func openSQLite(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Copy moves every row, as stored, from SQLite to the test database (the
// same or PostgreSQL) and back again, batching, verifying and refusing a
// non-empty or differently versioned target.
func TestCopy(t *testing.T) {
	src := openSQLite(t)
	t0 := time.UnixMilli(1_700_000_000_123).UTC()
	if err := src.PutAsset(ctx, Asset{Repo: "r", Path: "a/b.jar", BlobSHA256: "s1", Size: 3, ContentType: "x/y",
		CreatedAt: t0, LastModified: t0.Add(time.Second), RemoteETag: `"e"`, CheckedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := src.Tx(ctx, func(tx *Tx) error {
		return tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "p", Version: "1.0.0", Attrs: `{"a": 1,  "b":"üé"}`, CreatedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	if err := src.CreateToken(ctx, "tok1", "ci", "hash1", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := src.sql.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?)`, "smtp", `enc:v1:AAAA`+strings.Repeat("x", 40)); err != nil {
		t.Fatal(err)
	}
	if err := src.InsertEvents(ctx, []Event{{At: t0, Kind: "upload", Actor: "ci", Repo: "r", Subject: "a/b.jar", Detail: `{"size":3}`},
		{At: t0.Add(time.Minute), Kind: "delete", Actor: "root"}}); err != nil {
		t.Fatal(err)
	}
	// Enough negative-cache rows for several INSERT batches (3 columns: 333 rows each).
	for i := range 700 {
		if err := src.PutNegative(ctx, "r", fmt.Sprintf("miss/%04d", i), t0); err != nil {
			t.Fatal(err)
		}
	}
	want := dump(t, src)
	if len(want) < 705 {
		t.Fatalf("source holds %d rows", len(want))
	}

	dst := openTest(t)
	counts, err := Copy(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := dump(t, dst); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("target differs from source:\n%v\n%v", got, want)
	}
	n := map[string]int64{}
	for _, c := range counts {
		n[c.Table] = c.Rows
	}
	if len(counts) != len(copyTables) || n["negative_cache"] != 700 || n["events"] != 2 || n["assets"] != 1 || n["tokens"] != 1 {
		t.Fatalf("counts %+v", counts)
	}
	// The target's event ids continue after the copied ones.
	if err := dst.InsertEvents(ctx, []Event{{At: t0, Kind: "k"}}); err != nil {
		t.Fatal(err)
	}
	if evs, err := dst.ListEvents(ctx, 1, 0, ""); err != nil || len(evs) != 1 || evs[0].ID != 3 {
		t.Fatalf("next event: %+v %v", evs, err)
	}

	// A non-empty target is refused before anything is written.
	if _, err := Copy(ctx, src, dst); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("non-empty target: %v", err)
	}
	// So is a schema version other than the source's.
	stale := openTest(t)
	if err := stampSchemaVersion(ctx, stale.sql, schemaVersion+1, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, src, stale); err == nil || !strings.Contains(err.Error(), "schema version mismatch") {
		t.Fatalf("version mismatch: %v", err)
	}
	if got := dump(t, stale); len(got) != 0 {
		t.Fatalf("refused copy wrote %d rows", len(got))
	}

	// Back the other way (PostgreSQL -> SQLite on the PostgreSQL run), with
	// the event added above.
	want = dump(t, dst)
	back := openSQLite(t)
	if _, err := Copy(ctx, dst, back); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, back); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("round trip differs:\n%v\n%v", got, want)
	}
	if metatest.Postgres() && (src.sql.pg || !dst.sql.pg) {
		t.Fatal("the test must cross dialects on PostgreSQL")
	}
}
