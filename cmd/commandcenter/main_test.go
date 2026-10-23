package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/meta"
)

func TestHashPassword(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"hash-password"}, strings.NewReader("s3cret\n"), &out); err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(out.String())
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cret")) != nil {
		t.Fatalf("hash %q does not match", hash)
	}
	if err := run([]string{"hash-password"}, strings.NewReader("\n"), &out); err == nil {
		t.Fatal("empty password must fail")
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	if err := run([]string{"healthcheck", "-url", ok.URL}, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if err := run([]string{"healthcheck", "-url", bad.URL}, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("503 must fail")
	}
}

func TestUnknownCommand(t *testing.T) {
	if err := run([]string{"bogus"}, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestPickConfigPath(t *testing.T) {
	dir := t.TempDir()
	newP, oldP := filepath.Join(dir, "new.yaml"), filepath.Join(dir, "old.yaml")
	var warned []string
	warn := func(msg string) { warned = append(warned, msg) }
	touch := func(p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// neither exists: keep the new path so the load error names it
	if got := pickConfigPath(newP, false, oldP, warn); got != newP || len(warned) != 0 {
		t.Fatalf("got %q warned %v", got, warned)
	}
	// only the old one exists: fall back and warn
	touch(oldP)
	if got := pickConfigPath(newP, false, oldP, warn); got != oldP || len(warned) != 1 {
		t.Fatalf("got %q warned %v", got, warned)
	}
	// an explicit -config never falls back
	warned = nil
	if got := pickConfigPath(newP, true, oldP, warn); got != newP || len(warned) != 0 {
		t.Fatalf("explicit: got %q warned %v", got, warned)
	}
	// both exist: the new one wins, silently
	touch(newP)
	if got := pickConfigPath(newP, false, oldP, warn); got != newP || len(warned) != 0 {
		t.Fatalf("both: got %q warned %v", got, warned)
	}
}

// I4 (final review): user reset-password works offline on the database,
// reads the password from stdin with the API's rules, and reports what it
// changed.
func TestUserResetPassword(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("dataDir: "+filepath.ToSlash(filepath.Join(dir, "data"))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"user", "reset-password", "-config", cfgPath, "-user", "boss", "-admin"}
	for _, bad := range []string{"short\n", strings.Repeat("x", 73) + "\n", "\n"} {
		if err := run(args, strings.NewReader(bad), &bytes.Buffer{}); err == nil {
			t.Fatalf("password %q accepted", bad)
		}
	}
	if err := run([]string{"user", "reset-password", "-config", cfgPath}, strings.NewReader("longenough\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("missing -user accepted")
	}
	var out bytes.Buffer
	if err := run(args, strings.NewReader("longenough\n"), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`created local user "boss"`, `added the admin role to user "boss"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q lacks %q", out.String(), want)
		}
	}
	db, err := meta.Open(filepath.Join(dir, "data", "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, err := db.GetUser(context.Background(), "boss")
	if err != nil || u.Status != meta.StatusActive || len(u.Roles) != 1 || u.Roles[0] != "admin" ||
		bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("longenough")) != nil {
		t.Fatalf("boss = %+v %v", u, err)
	}
	if done, _ := db.AccessImported(context.Background()); !done {
		t.Fatal("the config import must run before the reset")
	}

	// an existing source=ldap override row is refused and left untouched
	if err := db.CreateUser(context.Background(), meta.User{ID: "dirk", Source: meta.SourceLDAP, Status: meta.StatusDisabled}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	err = run([]string{"user", "reset-password", "-config", cfgPath, "-user", "dirk"}, strings.NewReader("longenough\n"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `user "dirk" is an LDAP role-override record; choose another id for a local recovery account`) {
		t.Fatalf("ldap override id: %v", err)
	}
	db, err = meta.Open(filepath.Join(dir, "data", "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if d, _ := db.GetUser(context.Background(), "dirk"); d.Source != meta.SourceLDAP || d.Status != meta.StatusDisabled || d.PasswordHash != "" {
		t.Fatalf("dirk changed: %+v", d)
	}
}

func TestUserCommandUsage(t *testing.T) {
	if err := run([]string{"user"}, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "reset-password") {
		t.Fatalf("usage: %v", err)
	}
}
