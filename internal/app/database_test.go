package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// OpenDB takes the URL from COMMANDCENTER_DATABASE_URL or database.url, not
// both, accepts postgres:// and postgresql:// only, and no error holds the
// URL's password. Without a URL it is SQLite at <dataDir>/meta.db.
func TestOpenDB(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	db, err := OpenDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db.Driver() != "sqlite" {
		t.Fatalf("no URL: driver %s", db.Driver())
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "meta.db")); err != nil {
		t.Fatal(err)
	}

	const pw = "s3cret-pw"
	for _, c := range []struct{ env, file, want string }{
		{"postgres://u:" + pw + "@127.0.0.1:1/db", "postgres://u:" + pw + "@127.0.0.1:1/db", "set COMMANDCENTER_DATABASE_URL or database.url, not both"},
		{"mysql://u:" + pw + "@db/cc", "", "COMMANDCENTER_DATABASE_URL must be a postgres:// or postgresql:// URL"},
		{"", "u:" + pw + "@db/cc", "database.url must be a postgres:// or postgresql:// URL"},
		{"", "postgresql://u:" + pw + "@db:port/cc", "the database URL does not parse"},
		{"postgres://u:" + pw + "@127.0.0.1:1/cc?sslmode=disable&connect_timeout=5", "", "failed to connect"},
	} {
		t.Setenv(databaseEnv, c.env)
		cfg.Database.URL = c.file
		_, err := OpenDB(cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), pw) {
			t.Errorf("env %q, file %q: %v; want %q without the password", c.env, c.file, err, c.want)
		}
	}

	if !metatest.Postgres() {
		return
	}
	t.Setenv(databaseEnv, metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")))
	cfg.Database = config.Database{MaxConns: 2}
	if db, err = OpenDB(cfg); err != nil || db.Driver() != "postgresql" {
		t.Fatalf("from %s: %v", databaseEnv, err)
	}
	db.Close()
}
