package meta

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/meta/metatest"
)

func validSAML() SAML {
	c := DefaultSAML()
	c.IdPMetadataURL = "https://idp.example/metadata"
	c.SPPrivateKey, c.SPCertificate = "-----BEGIN PRIVATE KEY-----\nk\n-----END PRIVATE KEY-----\n", "-----BEGIN CERTIFICATE-----\nc\n-----END CERTIFICATE-----\n"
	c.GroupMappings = []GroupMapping{{Group: "cc-admins", Roles: []string{"admin"}}}
	return c
}

func TestValidateSAML(t *testing.T) {
	if err := ValidateSAML(validSAML()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSAML(DefaultSAML()); err != nil { // partial: no metadata
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*SAML){
		"no signature checks": func(c *SAML) { c.ValidateResponseSignature, c.ValidateAssertionSignature = false, false },
		"skew":                func(c *SAML) { c.ClockSkewSeconds = 601 },
		"negative skew":       func(c *SAML) { c.ClockSkewSeconds = -1 },
		"http url":            func(c *SAML) { c.IdPMetadataURL = "http://idp.example/md" },
		"both sources":        func(c *SAML) { c.IdPMetadataXML = "<x/>" },
		"attr space":          func(c *SAML) { c.GroupsAttribute = "my groups" },
		"no username":         func(c *SAML) { c.UsernameAttribute = "" },
		"entity id":           func(c *SAML) { c.EntityID = "no-scheme" },
		"key alone":           func(c *SAML) { c.SPCertificate = "" },
		"mapping no roles":    func(c *SAML) { c.GroupMappings = []GroupMapping{{Group: "g"}} },
	} {
		c := validSAML()
		mut(&c)
		if err := ValidateSAML(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := validSAML()
	if !c.Complete() || c.Mapping().GroupMappings[0].Group != "cc-admins" {
		t.Fatal("Complete or Mapping")
	}
	if s := c.String(); strings.Contains(s, "PRIVATE KEY") || !strings.Contains(s, "***") {
		t.Fatalf("String leaks the key: %s", s)
	}
	if err := ValidateRealms([]Realm{{ID: RealmSAML, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
}

// The SP key is sealed at rest and opens on every read path; key rotation
// and SealSecrets cover it.
func TestSAMLKeySealed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSettingJSON(ctx, SettingSAML, validSAML()); err != nil { // no key yet: plain
		t.Fatal(err)
	}
	if err := db.SetSecretKey(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	if n, err := db.SealSecrets(ctx); err != nil || n != 1 {
		t.Fatalf("SealSecrets: %d %v", n, err)
	}
	if raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingSAML); strings.Contains(raw, "PRIVATE KEY") || !strings.Contains(raw, sealedPrefix) {
		t.Fatalf("stored in the clear: %s", raw)
	}
	set, err := db.LoadSettings(ctx)
	if err != nil || set.SAML.SPPrivateKey != validSAML().SPPrivateKey || set.SAML.ClockSkewSeconds != 120 || !set.SAML.ValidateAssertionSignature {
		t.Fatalf("LoadSettings: %+v %v", set.SAML, err)
	}
	if data, err := db.LoadAccess(ctx); err != nil || data.Settings.SAML.SPPrivateKey != validSAML().SPPrivateKey {
		t.Fatalf("LoadAccess: %v", err)
	}
	if _, _, err := db.RotateSecretKey(ctx, bytes.Repeat([]byte{8}, 32)); err != nil {
		t.Fatal(err)
	}
	var cur SAML
	if _, err := db.GetSettingJSON(ctx, SettingSAML, &cur); err != nil || cur.SPPrivateKey != validSAML().SPPrivateKey {
		t.Fatalf("after rotation: %v", err)
	}
	set.SAML.SPPrivateKey = "-----BEGIN PRIVATE KEY-----\nother\n-----END PRIVATE KEY-----\n"
	if err := db.SaveSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	if raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingSAML); strings.Contains(raw, "other") {
		t.Fatal("SaveSettings stored the key in the clear")
	}
	bad := validSAML()
	bad.ValidateResponseSignature, bad.ValidateAssertionSignature = false, false
	if err := db.SetSettingJSON(ctx, SettingSAML, bad); err == nil {
		t.Fatal("an invalid configuration was stored")
	}
}
