package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/meta"
)

// QA N-29: a start without a key warns and keeps secrets as given; the next
// start with COMMANDCENTER_SECRET_KEY encrypts them; a start without the
// key, with another key or with both key sources then refuses, and the key
// file works like the variable. Neither the key nor a secret is logged.
func TestSecretKeyStartup(t *testing.T) {
	cfg := testConfig(t, "")
	ctx := context.Background()
	start := func() (*App, string, error) {
		t.Helper()
		var logs bytes.Buffer
		a, err := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
		return a, logs.String(), err
	}
	t.Setenv(secretKeyEnv, "")
	a, logs, err := start()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "no secret key is configured") {
		t.Fatalf("no warning without a key: %s", logs)
	}
	smtp := meta.SMTP{Host: "h", Port: 25, Security: meta.SMTPNone, From: "a@b.com", Password: "smtp-secret"}
	if err := a.DB.SetSettingJSON(ctx, meta.SettingSMTP, smtp); err != nil {
		t.Fatal(err)
	}
	a.Close()

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	t.Setenv(secretKeyEnv, key)
	a, logs, err = start()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "encrypted the stored secrets") || strings.Contains(logs, "no secret key") ||
		strings.Contains(logs, key) || strings.Contains(logs, "smtp-secret") {
		t.Fatalf("startup log with a key: %s", logs)
	}
	raw, err := a.DB.GetSetting(ctx, meta.SettingSMTP)
	if err != nil || strings.Contains(raw, "smtp-secret") {
		t.Fatalf("stored SMTP setting %q (err %v) still holds the password", raw, err)
	}
	if set, err := a.DB.LoadSettings(ctx); err != nil || set.SMTP.Password != "smtp-secret" {
		t.Fatalf("password reads back as %q, %v", set.SMTP.Password, err)
	}
	a.Close()

	keyFile := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ env, file, want string }{
		{"", "", "no secret key is set"},
		{base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)), "", "another key"},
		{"c2hvcnQ=", "", "32 random bytes"},
		{key, keyFile, "not both"},
		{"", keyFile, ""},
	} {
		t.Setenv(secretKeyEnv, c.env)
		cfg.SecretKeyFile = c.file
		a, logs, err := start()
		if c.want == "" {
			if err != nil {
				t.Fatalf("key file: %v", err)
			}
			a.Close()
			continue
		}
		if err == nil {
			a.Close()
			t.Fatalf("env %q file %q: started", c.env, c.file)
		}
		if !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error()+logs, "smtp-secret") ||
			(c.env != "" && strings.Contains(err.Error()+logs, c.env)) {
			t.Fatalf("env %q file %q: error %v, want %q", c.env, c.file, err, c.want)
		}
	}
}

// QA N-29 review: the key itself put into secretKeyFile, instead of the path
// of a file holding it, is not repeated by the startup error (main prints it
// to stderr, so it would end up in the container logs).
func TestSecretKeyFileNotEchoed(t *testing.T) {
	cfg := testConfig(t, "")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	t.Setenv(secretKeyEnv, "")
	cfg.SecretKeyFile = key // the value, not a path
	var logs bytes.Buffer
	a, err := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err == nil {
		a.Close()
		t.Fatal("started with an unreadable secretKeyFile")
	}
	if strings.Contains(err.Error(), key) || strings.Contains(logs.String(), key) {
		t.Fatalf("the startup error repeats the secret key: %v", err)
	}
	if !strings.Contains(err.Error(), "secretKeyFile") || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("error %v does not say which setting fails and why", err)
	}
}

// QA N-29 review: the key seals remoteAuth, not a proxy's remote URL, so with
// a key every start warns about the proxies whose URL holds a user name,
// password or query, naming them without the URL. Without such a URL, or
// without a key (that warning covers it), there is no such warning.
func TestSecretKeyRemoteURLWarning(t *testing.T) {
	const warning = "the secret key does not encrypt"
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	for _, c := range []struct {
		key, remote string
		warn        bool
	}{
		{key, "https://deploy:url-secret@repo.example.com/maven2/?token=query-secret", true},
		{key, "https://deploy:url-secret@repo.example.com/maven2/", true},
		{key, "https://repo.example.com/maven2/?token=query-secret", true},
		{key, "https://repo.example.com/maven2/", false},
		{"", "https://deploy:url-secret@repo.example.com/maven2/?token=query-secret", false},
	} {
		cfg := testConfig(t, `  - { name: px, format: maven, type: proxy, remote: "`+c.remote+
			`", remoteAuth: { username: u, password: auth-secret } }`+"\n")
		t.Setenv(secretKeyEnv, c.key)
		var logs bytes.Buffer
		a, err := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		a.Close()
		out := logs.String()
		if got := strings.Contains(out, warning); got != c.warn {
			t.Fatalf("key %v, remote %s: warning %v, want %v: %s", c.key != "", c.remote, got, c.warn, out)
		}
		if c.warn && !strings.Contains(out, `repos=px`) {
			t.Fatalf("warning does not name the proxy: %s", out)
		}
		for _, s := range []string{"url-secret", "query-secret", "auth-secret"} {
			if strings.Contains(out, s) {
				t.Fatalf("startup log holds %q: %s", s, out)
			}
		}
	}
}
