package meta

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/config"
)

func rawRow(t *testing.T, db *DB, query, key string) string {
	t.Helper()
	var v string
	if err := db.sql.QueryRowContext(ctx, query, key).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// storeSecrets writes one of each stored secret, all containing "secret".
func storeSecrets(t *testing.T, db *DB) {
	t.Helper()
	if err := db.SetSettingJSON(ctx, SettingSMTP, SMTP{Host: "h", Port: 25, Security: SMTPNone, From: "a@b.com", Password: "smtp-secret"}); err != nil {
		t.Fatal(err)
	}
	l := DefaultLDAP()
	l.URL, l.UserBaseDN, l.BindDN, l.BindPassword = "ldap://dir", "dc=x", "cn=svc", "ldap-secret"
	if err := db.SetSettingJSON(ctx, SettingLDAP, l); err != nil {
		t.Fatal(err)
	}
	for _, def := range []config.Repository{
		{Name: "p", Format: "maven", Type: "proxy", Remote: "https://r", RemoteAuth: &config.RemoteAuth{Username: "u", Password: "remote-secret"}},
		{Name: "q", Format: "npm", Type: "proxy", Remote: "https://n", RemoteAuth: &config.RemoteAuth{BearerToken: "bearer-secret"}},
		{Name: "h", Format: "maven", Type: "hosted"},
	} {
		if err := db.CreateRepository(ctx, def); err != nil {
			t.Fatal(err)
		}
	}
}

// rawSecrets returns the stored rows that hold secrets.
func rawSecrets(t *testing.T, db *DB) []string {
	t.Helper()
	const set, repo = `SELECT value FROM settings WHERE key = ?`, `SELECT config FROM repositories WHERE name = ?`
	return []string{rawRow(t, db, set, SettingSMTP), rawRow(t, db, set, SettingLDAP), rawRow(t, db, repo, "p"), rawRow(t, db, repo, "q")}
}

func checkSecretsRead(t *testing.T, db *DB) {
	t.Helper()
	set, err := db.LoadSettings(ctx)
	if err != nil || set.SMTP.Password != "smtp-secret" || set.LDAP.BindPassword != "ldap-secret" {
		t.Fatalf("LoadSettings: smtp %q ldap %q, %v", set.SMTP.Password, set.LDAP.BindPassword, err)
	}
	data, err := db.LoadAccess(ctx)
	if err != nil || data.Settings.SMTP.Password != "smtp-secret" || data.Settings.LDAP.BindPassword != "ldap-secret" {
		t.Fatalf("LoadAccess: %v", err)
	}
	var cur SMTP
	if _, err := db.GetSettingJSON(ctx, SettingSMTP, &cur); err != nil || cur.Password != "smtp-secret" {
		t.Fatalf("GetSettingJSON: %q, %v", cur.Password, err)
	}
	defs, err := db.ListRepositories(ctx)
	if err != nil || len(defs) != 3 || defs[1].RemoteAuth.Password != "remote-secret" || defs[2].RemoteAuth.BearerToken != "bearer-secret" {
		t.Fatalf("ListRepositories: %+v, %v", defs, err)
	}
}

// QA N-29: with a key the secrets are sealed in their rows and open on
// read; a database without the key, or with another one, cannot read them,
// and the errors never hold a secret.
func TestSecretsSealedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSecretKey(bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	storeSecrets(t, db)
	for _, raw := range rawSecrets(t, db) {
		if strings.Contains(raw, "secret") || !strings.Contains(raw, sealedPrefix) {
			t.Errorf("stored row is not sealed: %s", raw)
		}
	}
	checkSecretsRead(t, db)
	// An update seals again and leaves the caller's definition plain.
	def := config.Repository{Name: "p", Format: "maven", Type: "proxy", Remote: "https://r", RemoteAuth: &config.RemoteAuth{Password: "remote-secret"}}
	if err := db.UpdateRepository(ctx, def); err != nil || def.RemoteAuth.Password != "remote-secret" {
		t.Fatalf("UpdateRepository: %v; caller's password now %q", err, def.RemoteAuth.Password)
	}
	if raw := rawSecrets(t, db)[2]; strings.Contains(raw, "secret") {
		t.Fatalf("updated row is not sealed: %s", raw)
	}

	for name, key := range map[string][]byte{"no key": nil, "another key": bytes.Repeat([]byte{2}, 32)} {
		other, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if key != nil {
			if err := other.SetSecretKey(key); err != nil {
				t.Fatal(err)
			}
		}
		_, err1 := other.LoadSettings(ctx)
		_, err2 := other.ListRepositories(ctx)
		_, err3 := other.LoadAccess(ctx)
		for _, err := range []error{err1, err2, err3} {
			if err == nil || strings.Contains(err.Error(), "-secret") {
				t.Errorf("%s: read error %v", name, err)
			}
		}
		if _, err := other.SealSecrets(ctx); key != nil && (err == nil || !strings.Contains(err.Error(), "another key")) {
			t.Errorf("%s: SealSecrets = %v, want the wrong key named", name, err)
		}
		// The break-glass reset ("user reset-password") needs no key.
		if _, err := other.ResetLocalPassword(ctx, "rescue", "hash", false); err != nil {
			t.Errorf("%s: ResetLocalPassword: %v", name, err)
		}
		other.Close()
	}
}

// QA N-29: secrets stored without a key (today's behaviour) are sealed by
// the first SealSecrets with a key, once; their values do not change.
func TestSealSecretsMigratesPlainText(t *testing.T) {
	db := openTest(t)
	storeSecrets(t, db)
	for _, raw := range rawSecrets(t, db) {
		if !strings.Contains(raw, "secret") {
			t.Fatalf("without a key the secret must be stored as given: %s", raw)
		}
	}
	if err := db.SetSecretKey(bytes.Repeat([]byte{3}, 32)); err != nil {
		t.Fatal(err)
	}
	if n, err := db.SealSecrets(ctx); err != nil || n != 4 {
		t.Fatalf("SealSecrets = %d, %v; want 4 rows (smtp, ldap, p, q)", n, err)
	}
	for _, raw := range rawSecrets(t, db) {
		if strings.Contains(raw, "secret") || !strings.Contains(raw, sealedPrefix) {
			t.Errorf("row still plain: %s", raw)
		}
	}
	if n, err := db.SealSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("second SealSecrets = %d, %v; want 0", n, err)
	}
	checkSecretsRead(t, db)
}

// Without a key a value that looks sealed would not read back as given.
func TestSealRefusesSealedLookingPlainText(t *testing.T) {
	db := openTest(t)
	err := db.SetSettingJSON(ctx, SettingSMTP, SMTP{Host: "h", Port: 25, Security: SMTPNone, From: "a@b.com", Password: sealedPrefix + "x"})
	if err == nil {
		t.Fatal("a plain secret starting with the sealed prefix was stored")
	}
}
