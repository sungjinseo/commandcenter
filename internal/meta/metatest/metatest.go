// Package metatest gives tests their metadata database: a PostgreSQL
// database of their own on the server COMMANDCENTER_TEST_POSTGRES_URL names,
// such as postgres://postgres:pw@host:5432/postgres?sslmode=disable for a
// user that may create databases. Each database is created on first use and
// dropped when the test ends, so test packages that run in parallel never
// share one.
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
	dbs    = map[string]string{} // key -> URL of its database
	hint   sync.Once
)

// URL returns the URL of the test database for key, usually the test's
// dataDir or a t.TempDir(): the same key gives the same database until t
// ends and drops it, so a test can reopen it. It fails the test when
// EnvURL is not set.
func URL(t testing.TB, key string) string {
	t.Helper()
	env := os.Getenv(EnvURL)
	if env == "" {
		msg := EnvURL + " is not set"
		hint.Do(func() { // once per package: every test that needs a database fails the same way
			msg += ": the tests need a PostgreSQL server and a user that may create databases, e.g. " + EnvURL +
				"='postgres://postgres:PASSWORD@localhost:5432/postgres?sslmode=disable' " +
				"(docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=PASSWORD postgres:17-alpine)"
		})
		t.Fatal(msg)
	}
	key = filepath.Clean(key)
	mu.Lock()
	defer mu.Unlock()
	if u, ok := dbs[key]; ok {
		return u
	}
	u, err := url.Parse(env)
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
	dbs[key] = u.String()
	t.Cleanup(func() {
		mu.Lock()
		delete(dbs, key)
		mu.Unlock()
		if _, err := server.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	return u.String()
}
