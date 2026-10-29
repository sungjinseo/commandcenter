package meta

import (
	"bytes"
	"strings"
	"testing"

	"commandcenter/internal/meta/metatest"
)

func validOIDC() OIDC {
	c := DefaultOIDC()
	c.Issuer, c.ClientID, c.ClientSecret = "https://idp.example/realms/x", "cc", "s3cret"
	c.GroupMappings = []GroupMapping{{Group: "org:team", Roles: []string{"admin"}}}
	return c
}

func TestValidateOIDC(t *testing.T) {
	if err := ValidateOIDC(validOIDC()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOIDC(DefaultOIDC()); err != nil { // partial: no issuer
		t.Fatal(err)
	}
	for _, iss := range []string{"http://127.0.0.1:8080", "http://localhost/x"} {
		c := validOIDC()
		c.Issuer = iss
		if err := ValidateOIDC(c); err != nil {
			t.Errorf("%s: %v", iss, err)
		}
	}
	for name, mut := range map[string]func(*OIDC){
		"http issuer":      func(c *OIDC) { c.Issuer = "http://idp.example" },
		"no host":          func(c *OIDC) { c.Issuer = "https://" },
		"userinfo":         func(c *OIDC) { c.Issuer = "https://u:p@idp.example" },
		"query":            func(c *OIDC) { c.Issuer = "https://idp.example/?a=b" },
		"fragment":         func(c *OIDC) { c.Issuer = "https://idp.example/#x" },
		"space":            func(c *OIDC) { c.Issuer = "https://idp.example/a b" },
		"timeout 0":        func(c *OIDC) { c.TimeoutSeconds = 0 },
		"timeout 61":       func(c *OIDC) { c.TimeoutSeconds = 61 },
		"no username":      func(c *OIDC) { c.UsernameClaim = "" },
		"claim space":      func(c *OIDC) { c.GroupsClaim = "my groups" },
		"no openid scope":  func(c *OIDC) { c.Scopes = []string{"profile"} },
		"scope control":    func(c *OIDC) { c.Scopes = []string{"openid", "a\nb"} },
		"bad cacert":       func(c *OIDC) { c.CACert = "not pem" },
		"empty group":      func(c *OIDC) { c.GroupMappings = []GroupMapping{{Group: " ", Roles: []string{"admin"}}} },
		"mapping no roles": func(c *OIDC) { c.GroupMappings = []GroupMapping{{Group: "g"}} },
	} {
		c := validOIDC()
		mut(&c)
		if err := ValidateOIDC(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := validOIDC()
	if !c.GrantsAdmin() || !c.Complete() || c.Mapping().GroupMappings[0].Group != "org:team" {
		t.Fatal("GrantsAdmin, Complete or Mapping")
	}
	if s := c.String(); strings.Contains(s, "s3cret") || !strings.Contains(s, "***") {
		t.Fatalf("String leaks the secret: %s", s)
	}
}

// The client secret is sealed at rest like the other secrets and opens on
// every read path; a stored partial configuration round-trips with defaults.
func TestOIDCSecretSealed(t *testing.T) {
	db, err := Open(metatest.URL(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSecretKey(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSettingJSON(ctx, SettingOIDC, validOIDC()); err != nil {
		t.Fatal(err)
	}
	raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingOIDC)
	if strings.Contains(raw, "s3cret") || !strings.Contains(raw, `"clientSecret":"`+sealedPrefix) {
		t.Fatalf("stored row is not sealed: %s", raw)
	}
	set, err := db.LoadSettings(ctx)
	if err != nil || set.OIDC.ClientSecret != "s3cret" || set.OIDC.Issuer != "https://idp.example/realms/x" || set.OIDC.DisplayName != "SSO" {
		t.Fatalf("LoadSettings: %+v %v", set.OIDC, err)
	}
	data, err := db.LoadAccess(ctx)
	if err != nil || data.Settings.OIDC.ClientSecret != "s3cret" {
		t.Fatalf("LoadAccess: %v", err)
	}
	var cur OIDC
	if _, err := db.GetSettingJSON(ctx, SettingOIDC, &cur); err != nil || cur.ClientSecret != "s3cret" {
		t.Fatalf("GetSettingJSON: %q %v", cur.ClientSecret, err)
	}
	// SaveSettings seals it too, and a bad value is refused.
	set.OIDC.ClientSecret = "other"
	if err := db.SaveSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	if raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingOIDC); strings.Contains(raw, "other") {
		t.Fatalf("SaveSettings stored the secret in plain text: %s", raw)
	}
	bad := validOIDC()
	bad.Issuer = "http://idp.example"
	if err := db.SetSettingJSON(ctx, SettingOIDC, bad); err == nil {
		t.Fatal("an http issuer was stored")
	}
	if err := db.SetSettingJSON(ctx, SettingOIDC, OIDC{}); err == nil {
		t.Fatal("an empty configuration passed validation") // timeout 0
	}
	if err := ValidateRealms([]Realm{{ID: RealmOIDC, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
}
