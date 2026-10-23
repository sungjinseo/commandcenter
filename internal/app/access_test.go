package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

func TestAccessImportedOnceThenConfigIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	write := func(users string) *config.Config {
		t.Helper()
		yaml := "dataDir: " + dir + "\nbaseURL: https://repo.example.com/\nmaxUploadMiB: 7\n" +
			"users:\n" + users + "roles:\n  deployer: { write: [maven-releases] }\n" +
			"repositories:\n  - { name: maven-releases, format: maven, type: hosted }\n"
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	start := func(cfg *config.Config) (*App, string) {
		t.Helper()
		var buf strings.Builder
		a, err := New(cfg, slog.New(slog.NewTextHandler(&buf, nil)))
		if err != nil {
			t.Fatal(err)
		}
		return a, buf.String()
	}

	a, logs := start(write("  - { name: ci, passwordHash: \"" + string(h) + "\", roles: [deployer] }\n"))
	if strings.Contains(logs, "ignored") {
		t.Fatalf("first start warned: %s", logs)
	}
	s := a.Auth.Snapshot().Settings()
	if s.BaseURL != "https://repo.example.com" || s.MaxUploadMiB != 7 || !s.AuthenticatedRead || s.AnonymousRead {
		t.Fatalf("imported settings = %+v", s)
	}
	// The imported deployer may upload, as before.
	ts := httptest.NewServer(a.Handler)
	put, _ := http.NewRequest("PUT", ts.URL+"/repository/maven-releases/org/x/a/1.0/a-1.0.jar", strings.NewReader("jar"))
	put.SetBasicAuth("ci", "pw")
	resp, err := http.DefaultClient.Do(put)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}
	resp.Body.Close()
	ts.Close()
	// A user created in the database (as the API will) survives restarts.
	if err := a.DB.CreateUser(context.Background(), meta.User{ID: "later", PasswordHash: string(h)}); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// The second start has a different users section; the DB wins.
	a, logs = start(write("  - { name: other, passwordHash: \"" + string(h) + "\" }\n"))
	defer a.Close()
	for _, key := range []string{"users", "roles", "baseURL", "maxUploadMiB"} {
		if !strings.Contains(logs, key) {
			t.Fatalf("warning does not name %s: %s", key, logs)
		}
	}
	if strings.Count(logs, "users, roles and settings were imported") != 1 {
		t.Fatalf("want exactly one access warning: %s", logs)
	}
	snap := a.Auth.Snapshot()
	if _, ok := snap.User("ci"); !ok {
		t.Fatal("imported user lost")
	}
	if _, ok := snap.User("later"); !ok {
		t.Fatal("database user lost")
	}
	if _, ok := snap.User("other"); ok {
		t.Fatal("config user re-imported")
	}
}

func TestInvalidUsersSectionAfterImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	load := func(users string) *config.Config {
		t.Helper()
		if err := os.WriteFile(path, []byte("dataDir: "+dir+"\n"+users), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load must not fail on the users section: %v", err)
		}
		return cfg
	}
	const invalid = "users: [{name: u, passwordHash: x, roles: [nope]}]\n"

	// Before the import, an invalid section still blocks startup.
	if _, err := New(load(invalid), quiet()); err == nil || !strings.Contains(err.Error(), `unknown role "nope"`) {
		t.Fatalf("fresh start with an invalid users section: %v", err)
	}
	a, err := New(load("users: [{name: u, passwordHash: x}]\n"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	a.Close()

	// After the import it is ignored with the warning only.
	var buf strings.Builder
	a, err = New(load(invalid), slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("start after import with an invalid users section: %v", err)
	}
	defer a.Close()
	if !strings.Contains(buf.String(), "ignored") || !strings.Contains(buf.String(), "nope") {
		t.Fatalf("logs = %s", buf.String())
	}
	if _, ok := a.Auth.Snapshot().User("u"); !ok {
		t.Fatal("imported user lost")
	}
}

func TestStaleSettingsKeysAfterImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	load := func(extra string) *config.Config {
		t.Helper()
		if err := os.WriteFile(path, []byte("dataDir: "+dir+"\n"+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load must not fail on import-only settings keys: %v", err)
		}
		return cfg
	}
	// Before the import an invalid baseURL or maxUploadMiB still stops startup.
	for _, bad := range []string{"baseURL: ftp://x\n", "maxUploadMiB: -3\n"} {
		if _, err := New(load(bad), quiet()); err == nil {
			t.Fatalf("fresh start with %q must fail", bad)
		}
	}
	a, err := New(load("baseURL: https://ok.example\n"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, _ := a.DB.LoadSettings(ctx)
	s.SessionIdleMinutes = 45
	if err := a.DB.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// After the import the stale values are ignored with a warning, the
	// stored settings win and the stored session idle time is applied.
	var buf strings.Builder
	a, err = New(load("baseURL: ftp://stale\nmaxUploadMiB: -3\n"), slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("start after import with stale settings keys: %v", err)
	}
	defer a.Close()
	if !strings.Contains(buf.String(), "ignored") || !strings.Contains(buf.String(), "baseURL") {
		t.Fatalf("logs = %s", buf.String())
	}
	if got := a.Auth.Snapshot().Settings().BaseURL; got != "https://ok.example" {
		t.Fatalf("baseURL = %q", got)
	}
	if got := a.Auth.Sessions.Idle(); got != 45*time.Minute {
		t.Fatalf("session idle = %v", got)
	}
}
