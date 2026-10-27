package app

import (
	"context"
	"io"
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
	"commandcenter/internal/logring"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
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
		cfg.Database.URL = metatest.URL(t, cfg.DataDir)
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
		cfg.Database.URL = metatest.URL(t, cfg.DataDir)
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
		cfg.Database.URL = metatest.URL(t, cfg.DataDir)
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

// QA L-2 and N-24: every start warns about an active local user whose
// password hash is not a bcrypt hash (a placeholder fails every login without
// a word), and about a stored baseURL this version no longer accepts, without
// logging that value: it may hold a password.
func TestStartupWarnings(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.Users = append(cfg.Users, config.User{Name: "todo", PasswordHash: "<commandcenter hash-password 출력값>"})
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	// As an older build stored it (SetSetting does not validate).
	if err := a.DB.SetSetting(context.Background(), meta.SettingBaseURL, `"http://ci:s3cret@repo.example"`); err != nil {
		t.Fatal(err)
	}
	a.Close()
	var buf strings.Builder
	a, err = New(cfg, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var hashWarns []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "cannot sign in with a password") {
			hashWarns = append(hashWarns, line)
		}
	}
	if len(hashWarns) != 1 || !strings.Contains(hashWarns[0], "level=WARN") || !strings.Contains(hashWarns[0], "user=todo") {
		t.Fatalf("want one warning, for todo (ci has a real hash): %q", hashWarns)
	}
	if logs := buf.String(); !strings.Contains(logs, "stored baseURL is no longer accepted") || strings.Contains(logs, "s3cret") {
		t.Fatalf("baseURL warning: %s", logs)
	}
}

// QA N-25: deleting a repository removes the roles' privileges on it at
// once, without the reload loop, so a repository created again under the
// name does not let the roles' users in. One warning names the roles and
// each changed role gets a role.update event.
func TestRepositoryDeleteDropsRolePrivileges(t *testing.T) {
	cfg := testConfig(t, "  - { name: scratch, format: maven, type: hosted }\n")
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = append(cfg.Users, config.User{Name: "root", PasswordHash: string(h), Roles: []string{"admin"}})
	cfg.Roles["deployer"] = config.Role{Write: []string{"maven-releases", "scratch"}}
	cfg.Roles["viewer"] = config.Role{Write: []string{"scratch"}}
	ring := logring.NewRing(100, 1<<20)
	a, err := New(cfg, slog.New(logring.NewHandler(slog.NewTextHandler(io.Discard, nil), ring)), WithLogRing(ring))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	const jar = "/org/example/app/1.0/app-1.0.jar"

	if st, body := do(t, "PUT", ts.URL+"/repository/scratch"+jar, "ci", "jar", false); st != http.StatusCreated {
		t.Fatalf("PUT before the delete: %d %s", st, body)
	}
	if st, body := do(t, "DELETE", ts.URL+"/-/api/repositories/scratch", "root", "", true); st != http.StatusNoContent {
		t.Fatalf("delete: %d %s", st, body)
	}
	if st, body := do(t, "POST", ts.URL+"/-/api/repositories", "root", `{"name":"scratch","format":"maven","type":"hosted"}`, true); st != http.StatusCreated {
		t.Fatalf("create again: %d %s", st, body)
	}
	if st, body := do(t, "PUT", ts.URL+"/repository/scratch"+jar, "ci", "jar", false); st != http.StatusForbidden {
		t.Fatalf("PUT to the new repository: %d %s", st, body)
	}
	if st, body := do(t, "PUT", ts.URL+"/repository/maven-releases"+jar, "ci", "jar", false); st != http.StatusCreated {
		t.Fatalf("PUT to the role's other repository: %d %s", st, body)
	}

	warns := ring.Snapshot(slog.LevelWarn, "privileges on a deleted repository", 0)
	if len(warns) != 1 || warns[0].Attrs != `repository=scratch roles="deployer, viewer"` {
		t.Fatalf("warnings = %+v", warns)
	}
	var evs []meta.Event
	for deadline := time.Now().Add(5 * time.Second); len(evs) < 2 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if evs, err = a.DB.ListEvents(context.Background(), 10, 0, "role.update"); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.Actor+" "+e.Repo+" "+e.Subject+" "+e.Detail)
	}
	if strings.Join(got, "; ") != "root scratch viewer removed repo:scratch; root scratch deployer removed repo:scratch" {
		t.Fatalf("role.update events (newest first) = %q", got)
	}
}

// QA N-2: "user reset-password" next to a running server writes through
// another connection; the server applies the change within its reload
// interval and ends the user's sessions, as a reset through the API does.
func TestServeAppliesOutOfProcessAccessChanges(t *testing.T) {
	defer func(d time.Duration) { accessReloadEvery = d }(accessReloadEvery)
	accessReloadEvery = 20 * time.Millisecond
	cfg := testConfig(t, "")
	cfg.Listen = "127.0.0.1:0"
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	sid := a.Auth.Sessions.Create("ci")
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Serve(sctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	// A second handle on the database stands in for the CLI process.
	other, err := meta.Open(metatest.Target(t, filepath.Join(cfg.DataDir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	_, err = other.ResetLocalPassword(ctx, "ci", string(h), false)
	other.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The deadline only absorbs a slow machine: without the reload loop the
	// change never applies.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := a.Auth.CheckPassword(ctx, "ci", "new-password"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the running server did not apply the new password")
		}
	}
	if _, err := a.Auth.CheckPassword(ctx, "ci", "pw"); err == nil {
		t.Fatal("the old password still works")
	}
	if _, ok := a.Auth.Sessions.User(sid); ok {
		t.Fatal("the session of ci survived the reset")
	}
}
