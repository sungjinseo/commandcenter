package meta

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func validLDAP() LDAP {
	c := DefaultLDAP()
	c.URL = "ldap://ldap.example.org:389"
	c.UserBaseDN = "ou=people,dc=example,dc=org"
	c.BindDN, c.BindPassword = "cn=svc,dc=example,dc=org", "s3cret-bind"
	return c
}

func TestValidateLDAPStaticGroups(t *testing.T) {
	static := func() LDAP {
		c := validLDAP()
		c.GroupType, c.GroupBaseDN, c.GroupObjectClass = LDAPGroupsStatic, "ou=groups,dc=example,dc=org", "groupOfNames"
		c.GroupIDAttr, c.GroupMemberAttr, c.GroupMemberFormat = "cn", "member", "${dn}"
		return c
	}
	if err := ValidateLDAP(static()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"${username}", "uid=${username},ou=people", "cn=x,${dn}"} {
		c := static()
		c.GroupMemberFormat = f
		if err := ValidateLDAP(c); err != nil {
			t.Errorf("format %q: %v", f, err)
		}
	}
	for name, mut := range map[string]func(*LDAP){
		"another type":                           func(c *LDAP) { c.GroupType = "STATIC" },
		"no base":                                func(c *LDAP) { c.GroupBaseDN = " " },
		"no class":                               func(c *LDAP) { c.GroupObjectClass = "" },
		"no id":                                  func(c *LDAP) { c.GroupIDAttr = "" },
		"no member":                              func(c *LDAP) { c.GroupMemberAttr = "" },
		"no format":                              func(c *LDAP) { c.GroupMemberFormat = "" },
		"a fixed member, everyone in its groups": func(c *LDAP) { c.GroupMemberFormat = "uid=alice,ou=people" },
		"an unknown placeholder":                 func(c *LDAP) { c.GroupMemberFormat = "${user}" },
	} {
		c := static()
		mut(&c)
		if err := ValidateLDAP(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Dynamic groups need none of it, and a configuration without a server may be partial.
	c := validLDAP()
	c.GroupBaseDN = "ou=kept-for-later"
	if err := ValidateLDAP(c); err != nil {
		t.Errorf("dynamic: %v", err)
	}
	c = DefaultLDAP()
	c.GroupType = LDAPGroupsStatic
	if err := ValidateLDAP(c); err != nil {
		t.Errorf("no server: %v", err)
	}
	// What an older row holds: no group type at all.
	c = validLDAP()
	c.GroupType = ""
	if err := ValidateLDAP(c); err != nil || c.StaticGroups() {
		t.Errorf("no group type: %v, static %v", err, c.StaticGroups())
	}
}

func TestValidateLDAP(t *testing.T) {
	if err := ValidateLDAP(validLDAP()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLDAP(DefaultLDAP()); err != nil {
		t.Fatalf("an unset configuration is valid: %v", err)
	}
	for name, mut := range map[string]func(*LDAP){
		"http url":         func(c *LDAP) { c.URL = "http://x" },
		"no host":          func(c *LDAP) { c.URL = "ldap://" },
		"userinfo":         func(c *LDAP) { c.URL = "ldap://u:p@x" },
		"path":             func(c *LDAP) { c.URL = "ldap://x/dc=x" },
		"starttls ldaps":   func(c *LDAP) { c.URL, c.StartTLS = "ldaps://x", true },
		"timeout 0":        func(c *LDAP) { c.TimeoutSeconds = 0 },
		"timeout 61":       func(c *LDAP) { c.TimeoutSeconds = 61 },
		"no {user}":        func(c *LDAP) { c.UserFilter = "(uid=alice)" },
		"no base":          func(c *LDAP) { c.UserBaseDN = " " },
		"no id attr":       func(c *LDAP) { c.UserIDAttr = "" },
		"bindDN no pw":     func(c *LDAP) { c.BindPassword = "" },
		"empty group":      func(c *LDAP) { c.GroupMappings = []GroupMapping{{Group: "", Roles: []string{"admin"}}} },
		"mapping no roles": func(c *LDAP) { c.GroupMappings = []GroupMapping{{Group: "g"}} },
	} {
		c := validLDAP()
		mut(&c)
		if err := ValidateLDAP(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := validLDAP()
	c.URL, c.StartTLS = "ldap://x", true
	if err := ValidateLDAP(c); err != nil {
		t.Fatalf("starttls on ldap://: %v", err)
	}
}

func TestLDAPSettingRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	s, err := db.LoadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.LDAP.URL != "" || s.LDAP.TimeoutSeconds != 5 || s.LDAP.UserFilter != "(uid={user})" || s.LDAP.GroupAttr != "memberOf" {
		t.Fatalf("defaults: %+v", s.LDAP)
	}
	c := validLDAP()
	c.GroupMappings = []GroupMapping{{Group: "cc-admins", Roles: []string{"admin"}}}
	if err := db.SetSettingJSON(ctx, SettingLDAP, c); err != nil {
		t.Fatal(err)
	}
	bad := c
	bad.URL = "http://x"
	if err := db.SetSettingJSON(ctx, SettingLDAP, bad); err == nil {
		t.Fatal("invalid config stored")
	}
	s, err = db.LoadSettings(ctx)
	if err != nil || s.LDAP.BindPassword != "s3cret-bind" || len(s.LDAP.GroupMappings) != 1 || !s.LDAP.GrantsAdmin() || !s.LDAP.Complete() {
		t.Fatalf("loaded %+v %v", s.LDAP, err)
	}
	if err := db.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
}

func TestSecretsRedacted(t *testing.T) {
	l := validLDAP()
	sm := SMTP{Host: "h", Port: 25, Password: "smtp-pass-123", From: "a@b.c"}
	set := DefaultSettings()
	set.LDAP, set.SMTP = l, sm
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("x", "ldap", l, "smtp", sm, "settings", set)
	for _, out := range []string{
		fmt.Sprintf("%+v", l), fmt.Sprintf("%v", l), fmt.Sprintf("%+v", sm), fmt.Sprintf("%v", sm),
		fmt.Sprintf("%+v", set), fmt.Sprintf("%v", &l), buf.String(),
	} {
		if strings.Contains(out, "s3cret-bind") || strings.Contains(out, "smtp-pass-123") {
			t.Errorf("secret printed: %s", out)
		}
		if !strings.Contains(out, "***") {
			t.Errorf("no redaction marker: %s", out)
		}
	}
	if !strings.Contains(fmt.Sprintf("%+v", l), "ldap.example.org") {
		t.Fatal("non-secret fields missing")
	}
}

func TestValidUserID(t *testing.T) {
	for _, ok := range []string{"a", "alice.b", "x@y.z", "A_1-2"} {
		if !ValidUserID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-a", "a b", "ldap:x", "create", strings.Repeat("a", 65), "a*"} {
		if ValidUserID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
