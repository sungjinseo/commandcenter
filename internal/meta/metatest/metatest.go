// Package metatest gives tests their metadata database. By default it is
// the SQLite file a test names, as always. With COMMANDCENTER_TEST_POSTGRES_URL
// set to a PostgreSQL server, such as
// postgres://postgres:pw@host:5432/postgres?sslmode=disable for a user that
// may create databases, each test gets a database of its own instead,
// created on first use and dropped when the test ends, so test packages
// that run in parallel never share one.
package metatest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvURL names the PostgreSQL server the tests run on.
const EnvURL = "COMMANDCENTER_TEST_POSTGRES_URL"

var (
	mu     sync.Mutex
	server *sql.DB
	dbs    = map[string]string{} // SQLite path -> URL of the database standing in for it
)

// Postgres reports whether the tests run on PostgreSQL.
func Postgres() bool { return os.Getenv(EnvURL) != "" }

// Target returns what meta.Open opens for the SQLite file at path: path
// itself, or on PostgreSQL the URL of a database created for it. The same
// path gives the same database until t ends and drops it, so a test can
// reopen it.
func Target(t testing.TB, path string) string {
	t.Helper()
	if !Postgres() {
		return path
	}
	path = filepath.Clean(path)
	mu.Lock()
	defer mu.Unlock()
	if u, ok := dbs[path]; ok {
		return u
	}
	u, err := url.Parse(os.Getenv(EnvURL))
	if err != nil {
		t.Fatalf("%s does not parse", EnvURL) // without the URL: it holds a password
	}
	if server == nil {
		if server, err = sql.Open("pgx", u.String()); err != nil {
			t.Fatalf("%s: %v", EnvURL, err)
		}
	}
	b := make([]byte, 8)
	rand.Read(b)
	name := "cctest_" + hex.EncodeToString(b)
	// ICU's en-US does not sort byte-wise, so a query that relies on a
	// database collation rather than the schema's COLLATE "C" fails here.
	if _, err := server.Exec(`CREATE DATABASE ` + name + ` TEMPLATE template0 LOCALE_PROVIDER icu ICU_LOCALE 'en-US'`); err != nil {
		if _, err := server.Exec(`CREATE DATABASE ` + name); err != nil { // a server without ICU
			t.Fatalf("create test database: %v", err)
		}
	}
	u.Path = "/" + name
	dbs[path] = u.String()
	t.Cleanup(func() {
		mu.Lock()
		delete(dbs, path)
		mu.Unlock()
		if _, err := server.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	return u.String()
}

// URL returns the database.url for a test config with dataDir: "" (SQLite
// at <dataDir>/meta.db) or on PostgreSQL Target's URL for that file.
func URL(t testing.TB, dataDir string) string {
	t.Helper()
	if !Postgres() {
		return ""
	}
	return Target(t, filepath.Join(dataDir, "meta.db"))
}
