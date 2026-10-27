package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
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

// migrate-db copies an SQLite meta.db populated through the real handlers
// into an empty target (PostgreSQL on the PostgreSQL run, else another
// SQLite file), prints per-table counts and nothing secret, refuses a
// non-empty target, and the server answers the same on both. On PostgreSQL
// the reverse direction is checked too.
func TestMigrateDB(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	key := bytes.Repeat([]byte{7}, 32)
	t.Setenv("COMMANDCENTER_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	// The source is SQLite at <dataDir>/meta.db whatever the test database is.
	yaml := "dataDir: " + filepath.ToSlash(dataDir) + `
anonymousRead: true
users:
  - { name: ci, passwordHash: "` + string(h) + `", roles: [deployer] }
  - { name: root, passwordHash: "` + string(h) + `", roles: [admin] }
roles:
  deployer: { write: [maven-releases, npm-hosted] }
repositories:
  - { name: maven-releases, format: maven, type: hosted }
  - { name: maven-public, format: maven, type: group, members: [maven-releases] }
  - { name: npm-hosted, format: npm, type: hosted }
`
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	do := func(t *testing.T, ts *httptest.Server, method, path, user, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if user != "" {
			req.SetBasicAuth(user, "pw")
		}
		req.Header.Set("X-Command-Center-UI", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const jar = "/repository/maven-releases/org/example/app/1.0/app-1.0.jar"
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	for _, p := range []string{jar, "/repository/maven-releases/org/example/app/1.1/app-1.1.jar"} {
		if st, body := do(t, ts, "PUT", p, "ci", "jar"); st != 201 {
			t.Fatalf("PUT %s: %d %s", p, st, body)
		}
	}
	if st, body := do(t, ts, "PUT", "/repository/npm-hosted/pkg", "ci", npmPublish("pkg", "1.0.0")); st != 201 {
		t.Fatalf("npm publish: %d %s", st, body)
	}
	_, token, err := a.Auth.CreateToken(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	if st, body := do(t, ts, "PUT", "/-/api/settings/smtp", "root", `{"host":"mail","port":25,"security":"none","from":"a@b.com","password":"smtp-secret"}`); st/100 != 2 {
		t.Fatalf("PUT settings: %d %s", st, body)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, list := do(t, ts, "GET", "/-/api/events", "root", ""); strings.Count(list, `"kind":"upload"`) == 3 && strings.Contains(list, `"kind":"settings.smtp"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("events not recorded")
		}
	}
	ts.Close()
	a.Close()

	type target struct {
		name, to, dataDir string // dataDir: where an app on the target runs
		url               string // database.url for that app
	}
	var targets []target
	if metatest.Postgres() {
		u := metatest.Target(t, filepath.Join(dir, "pg", "meta.db"))
		targets = append(targets, target{"postgres", u, dataDir, u})
	} else {
		copyDir := filepath.Join(dir, "copy")
		targets = append(targets, target{"sqlite", filepath.Join(copyDir, "meta.db"), copyDir, ""})
	}
	srcCfg := cfgPath
	for i, tg := range targets {
		if tg.dataDir != dataDir { // an SQLite target runs beside its own copy of the blobs
			if err := os.CopyFS(filepath.Join(tg.dataDir, "blobs"), os.DirFS(filepath.Join(dataDir, "blobs"))); err != nil {
				t.Fatal(err)
			}
		}
		var out bytes.Buffer
		if err := run([]string{"migrate-db", "-config", srcCfg, "-to", tg.to}, nil, &out); err != nil {
			t.Fatalf("%s: %v", tg.name, err)
		}
		for _, want := range []string{"repositories       3\n", "users              2\n", "tokens             1\n", "assets             5\n",
			"packages           1\n", "dist_tags          1\n", "settings           ", "events             ", "done; copied "} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s: output lacks %q:\n%s", tg.name, want, out.String())
			}
		}
		pw := ""
		if u, err := url.Parse(tg.to); err == nil && u.User != nil {
			pw, _ = u.User.Password()
		}
		for _, secret := range []string{tg.to, token, "smtp-secret", pw} {
			if secret != "" && strings.Contains(out.String(), secret) {
				t.Fatalf("%s: output holds a secret:\n%s", tg.name, out.String())
			}
		}
		// Again: the target is no longer empty, and the error names no URL.
		err := run([]string{"migrate-db", "-config", srcCfg, "-to", tg.to}, nil, &out)
		if err == nil || !strings.Contains(err.Error(), "not empty") || (pw != "" && strings.Contains(err.Error(), pw)) {
			t.Fatalf("%s: second run: %v", tg.name, err)
		}

		// The sealed SMTP password opens with the same key on the target.
		db, err := meta.Open(tg.to)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetSecretKey(key); err != nil {
			t.Fatal(err)
		}
		if s, err := db.LoadSettings(context.Background()); err != nil || s.SMTP.Password != "smtp-secret" {
			t.Fatalf("%s: smtp password %q, %v", tg.name, s.SMTP.Password, err)
		}
		db.Close()

		// The HTTP API answers identically on the source and the target.
		tgCfg := filepath.Join(dir, tg.name+".yaml")
		y := strings.Replace(yaml, filepath.ToSlash(dataDir), filepath.ToSlash(tg.dataDir), 1)
		if tg.url != "" {
			y += "database: {url: " + strconv.Quote(tg.url) + "}\n"
		}
		if err := os.WriteFile(tgCfg, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		var servers []*httptest.Server
		for _, p := range []string{cfgPath, tgCfg} {
			c, err := config.Load(p)
			if err != nil {
				t.Fatal(err)
			}
			a, err := app.New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatalf("%s: %v", tg.name, err)
			}
			defer a.Close()
			s := httptest.NewServer(a.Handler)
			defer s.Close()
			servers = append(servers, s)
		}
		for _, c := range []struct{ method, path, user, body string }{
			{"GET", "/-/api/repositories", "root", ""},
			{"GET", "/-/api/repositories/maven-releases/packages", "root", ""},
			{"GET", "/-/api/repositories/maven-releases/browse?path=org/example/app/1.0/", "root", ""},
			{"GET", jar, "", ""},
			{"GET", "/repository/maven-public/org/example/app/maven-metadata.xml", "", ""},
			{"GET", "/-/api/repositories/npm-hosted/packages", "root", ""},
			{"GET", "/repository/npm-hosted/pkg", "", ""},
			{"GET", "/repository/npm-hosted/pkg/-/pkg-1.0.0.tgz", "", ""},
			{"GET", "/-/api/events", "root", ""},
			{"GET", "/-/api/users", "root", ""},
			{"GET", "/-/api/tokens", "ci", ""},
			{"POST", "/-/api/login", "", `{"user":"ci","password":"pw"}`},
			{"POST", "/-/api/login", "", `{"user":"ci","password":"wrong"}`},
		} {
			st1, b1 := do(t, servers[0], c.method, c.path, c.user, c.body)
			st2, b2 := do(t, servers[1], c.method, c.path, c.user, c.body)
			b1 = strings.ReplaceAll(b1, servers[0].URL, "<base>") // npm tarball URLs
			b2 = strings.ReplaceAll(b2, servers[1].URL, "<base>")
			if st1 != st2 || b1 != b2 {
				t.Fatalf("%s: %s %s differs: source %d %s; target %d %s", tg.name, c.method, c.path, st1, b1, st2, b2)
			}
			if st1 != 200 && !(strings.Contains(c.body, "wrong") && st1 == 401) {
				t.Fatalf("%s %s: %d %s", c.method, c.path, st1, b1)
			}
		}
		req, _ := http.NewRequest("GET", servers[1].URL+"/-/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		me, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(me), `"user":"ci"`) {
			t.Fatalf("%s: token on the target: %d %s", tg.name, resp.StatusCode, me)
		}
		if i == 0 && metatest.Postgres() { // and back: PostgreSQL -> a new SQLite file
			backDir := filepath.Join(dir, "back")
			targets = append(targets, target{"back", filepath.Join(backDir, "meta.db"), backDir, ""})
			srcCfg = tgCfg
		}
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"migrate-db", "-config", cfgPath}, "-to is required"},
		{[]string{"migrate-db", "-config", cfgPath, "-to", "mysql://u:p@h/db"}, "must be a postgres://"},
	} {
		if err := run(c.args, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: %v", c.args, err)
		}
	}
}

// npmPublish builds what `npm publish` sends for a package holding only its package.json.
func npmPublish(name, version string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	pj, _ := json.Marshal(map[string]string{"name": name, "version": version})
	tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(pj)), Typeflag: tar.TypeReg})
	tw.Write(pj)
	tw.Close()
	gz.Close()
	tgz := buf.Bytes()
	s1, s512 := sha1.Sum(tgz), sha512.Sum512(tgz)
	file := name + "-" + version + ".tgz"
	doc, _ := json.Marshal(map[string]any{
		"_id": name, "name": name, "dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{"name": name, "version": version, "dist": map[string]string{
			"shasum": hex.EncodeToString(s1[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
			"tarball": "http://localhost/" + name + "/-/" + file}}},
		"_attachments": map[string]any{file: map[string]any{"content_type": "application/octet-stream",
			"data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz)}},
	})
	return string(doc)
}
