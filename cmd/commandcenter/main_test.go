package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

// configYAML is a config file with dataDir, on the test's database (see
// metatest).
func configYAML(t *testing.T, dataDir string) []byte {
	y := "dataDir: " + filepath.ToSlash(dataDir) + "\n"
	if u := metatest.URL(t, dataDir); u != "" {
		y += "database: {url: " + strconv.Quote(u) + "}\n"
	}
	return []byte(y)
}

func TestHashPassword(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"hash-password"}, strings.NewReader("s3cret-pw\n"), &out); err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(out.String())
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cret-pw")) != nil {
		t.Fatalf("hash %q does not match", hash)
	}
	// The API's password rules apply, as for user reset-password (the old
	// "s3cret" is too short now).
	for _, bad := range []string{"\n", "s3cret\n", "seven77\n", strings.Repeat("x", 73) + "\n", "        \n", " \t \t    \n"} {
		out.Reset()
		if err := run([]string{"hash-password"}, strings.NewReader(bad), &out); err == nil || out.Len() != 0 {
			t.Fatalf("password %q accepted: %v %q", bad, err, out.String())
		}
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
	if err := os.WriteFile(cfgPath, configYAML(t, filepath.Join(dir, "data")), 0o600); err != nil {
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
	// A running server reloads users every few seconds (QA N-2), so the
	// message no longer asks for a (re)start.
	for _, want := range []string{`created local user "boss"`, `added the admin role to user "boss"`, "a running server applies the change within a few seconds"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q lacks %q", out.String(), want)
		}
	}
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "data", "meta.db")))
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
	db, err = meta.Open(metatest.Target(t, filepath.Join(dir, "data", "meta.db")))
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

// secret-key rotate seals the stored secrets with the key in -new-key-file
// instead of the current one (here COMMANDCENTER_SECRET_KEY) and prints only
// counts. It refuses to run without the new key file, its usage says to stop
// the server first, and it never echoes a key.
func TestSecretKeyRotate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, configYAML(t, dir), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	open := func(key []byte) *meta.DB {
		t.Helper()
		db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetSecretKey(key); err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open(oldKey)
	l := meta.DefaultLDAP()
	l.URL, l.UserBaseDN, l.BindDN, l.BindPassword = "ldap://dir", "dc=x", "cn=svc", "ldap-secret"
	if err := db.SetSettingJSON(ctx, meta.SettingLDAP, l); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSettingJSON(ctx, meta.SettingSMTP, meta.SMTP{Host: "h", Port: 25, Security: meta.SMTPNone, From: "a@b.com", Password: "smtp-secret"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	oldB64, newB64 := base64.StdEncoding.EncodeToString(oldKey), base64.StdEncoding.EncodeToString(newKey)
	keyFile := filepath.Join(dir, "new.key")
	if err := os.WriteFile(keyFile, []byte(newB64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMANDCENTER_SECRET_KEY", oldB64)

	var out bytes.Buffer
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"secret-key"}, "stop the server first"},
		{[]string{"secret-key", "rotate", "-config", cfgPath}, "-new-key-file is required"},
		{[]string{"secret-key", "rotate", "-config", cfgPath, "-new-key-file", newB64}, "cannot read the file"}, // the key instead of its file
	} {
		err := run(c.args, nil, &out)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), newB64) || out.Len() != 0 {
			t.Fatalf("%q: error %v, output %q; want %q", c.args, err, out.String(), c.want)
		}
	}
	rotate := []string{"secret-key", "rotate", "-config", cfgPath, "-new-key-file", keyFile}
	if err := run(rotate, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "re-encrypted 2 stored secrets with the new key and encrypted 0 that were plain text") {
		t.Fatalf("output %q", out.String())
	}
	for _, s := range []string{oldB64, newB64, "smtp-secret", "ldap-secret"} {
		if strings.Contains(out.String(), s) {
			t.Fatalf("output %q holds %q", out.String(), s)
		}
	}
	// Again with the old key: refused, as the secrets no longer open with it.
	if err := run(rotate, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("second rotation: %v", err)
	}
	db = open(newKey)
	defer db.Close()
	if set, err := db.LoadSettings(ctx); err != nil || set.SMTP.Password != "smtp-secret" || set.LDAP.BindPassword != "ldap-secret" {
		t.Fatalf("with the new key: smtp %q ldap %q, %v", set.SMTP.Password, set.LDAP.BindPassword, err)
	}
}
