package meta

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUserTokens(t *testing.T) {
	db := openTest(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := db.PutUserToken(ctx, "alice", "aaaaaaaa", "h1", t0); err != nil {
		t.Fatal(err)
	}
	if err := db.PutUserToken(ctx, "ldap:bob", "aaaaaaaa", "h2", t0); !errors.Is(err, ErrExists) {
		t.Fatalf("taken name code: %v", err)
	}
	if err := db.PutUserToken(ctx, "ldap:bob", "AAAAAAAA", "h2", t0.Add(time.Hour)); err != nil {
		t.Fatalf("name codes are case-sensitive: %v", err)
	}
	tok, hash, err := db.UserTokenByNameCode(ctx, "aaaaaaaa")
	if err != nil || tok.Owner != "alice" || hash != "h1" || !tok.Created.Equal(t0) || !tok.LastUsed.IsZero() {
		t.Fatalf("by name code: %+v %q %v", tok, hash, err)
	}
	// A reset replaces the owner's row: the old name code is gone.
	if err := db.PutUserToken(ctx, "alice", "bbbbbbbb", "h3", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.UserTokenByNameCode(ctx, "aaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old name code: %v", err)
	}
	db.TouchUserToken(ctx, "alice")
	if got, err := db.GetUserToken(ctx, "alice"); err != nil || got.NameCode != "bbbbbbbb" || got.LastUsed.IsZero() {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, err := db.ListUserTokens(ctx)
	if err != nil || len(list) != 2 || list[0].Owner != "alice" || list[1].Owner != "ldap:bob" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if n, err := db.DeleteUserTokensBefore(ctx, t0.Add(90*time.Minute)); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, err := db.GetUserToken(ctx, "ldap:bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged token: %v", err)
	}
	// Deleting a user deletes its token in the same transaction.
	if err := db.CreateUser(ctx, User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetUserToken(ctx, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user's token: %v", err)
	}
	if err := db.DeleteUserToken(ctx, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	for _, o := range []string{"x", "y"} {
		if err := db.PutUserToken(ctx, o, o+"0000000", "h", t0); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := db.DeleteAllUserTokens(ctx); err != nil || n != 2 {
		t.Fatalf("delete all: %d %v", n, err)
	}
}

func TestUserTokenSettings(t *testing.T) {
	db := openTest(t)
	set, err := db.LoadSettings(ctx)
	if err != nil || set.UserTokens != (UserTokens{}) || set.Crowd.CacheMinutes != 1 || set.Crowd.TimeoutSeconds != 10 {
		t.Fatalf("defaults: %+v %+v %v", set.UserTokens, set.Crowd, err)
	}
	if err := db.SetSettingJSON(ctx, SettingUserTokens, UserTokens{ExpirationDays: 3651}); err == nil {
		t.Fatal("expirationDays 3651 accepted")
	}
	if err := db.SetSettingJSON(ctx, SettingUserTokens, UserTokens{Enabled: true, Required: true, ExpirationDays: 90}); err != nil {
		t.Fatal(err)
	}
	if set, _ := db.LoadSettings(ctx); set.UserTokens != (UserTokens{Enabled: true, Required: true, ExpirationDays: 90}) {
		t.Fatalf("stored %+v", set.UserTokens)
	}
	c := UserTokens{ExpirationDays: 2}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if got := c.Expires(t0); !got.Equal(t0.Add(48 * time.Hour)) {
		t.Fatalf("expires %v", got)
	}
	if !(UserTokens{}).Expires(t0).IsZero() {
		t.Fatal("expirationDays 0 expires")
	}
}

func TestCrowdSettingsSealed(t *testing.T) {
	db := openTest(t)
	c := DefaultCrowd()
	c.URL, c.ApplicationName, c.ApplicationPassword = "https://crowd.example.org/crowd", "cc", "crowd-secret"
	// Stored before a key was set: plain, then sealed by SealSecrets.
	if err := db.SetSettingJSON(ctx, SettingCrowd, c); err != nil {
		t.Fatal(err)
	}
	if raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingCrowd); !strings.Contains(raw, "crowd-secret") {
		t.Fatalf("without a key: %s", raw)
	}
	if err := db.SetSecretKey(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	if n, err := db.SealSecrets(ctx); err != nil || n != 1 {
		t.Fatalf("seal: %d %v", n, err)
	}
	raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingCrowd)
	if strings.Contains(raw, "crowd-secret") || !strings.Contains(raw, sealedPrefix) {
		t.Fatalf("sealed: %s", raw)
	}
	set, err := db.LoadSettings(ctx)
	if err != nil || set.Crowd.ApplicationPassword != "crowd-secret" {
		t.Fatalf("LoadSettings: %q %v", set.Crowd.ApplicationPassword, err)
	}
	data, err := db.LoadAccess(ctx)
	if err != nil || data.Settings.Crowd.ApplicationPassword != "crowd-secret" {
		t.Fatalf("LoadAccess: %v", err)
	}
	var got Crowd
	if _, err := db.GetSettingJSON(ctx, SettingCrowd, &got); err != nil || got.ApplicationPassword != "crowd-secret" {
		t.Fatalf("GetSettingJSON: %q %v", got.ApplicationPassword, err)
	}
	// SetSettingJSON and SaveSettings seal it too.
	c.ApplicationPassword = "other-secret"
	if err := db.SetSettingJSON(ctx, SettingCrowd, &c); err != nil {
		t.Fatal(err)
	}
	set.Crowd.ApplicationPassword = "third-secret"
	if err := db.SaveSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	if raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingCrowd); strings.Contains(raw, "secret") {
		t.Fatalf("SaveSettings stored it plain: %s", raw)
	}
	if s := c.String(); strings.Contains(s, "other-secret") {
		t.Fatalf("String: %s", s)
	}
	for _, bad := range []Crowd{
		{URL: "ftp://x", ApplicationName: "a", TimeoutSeconds: 10, CacheMinutes: 1},
		{URL: "https://u:p@x", ApplicationName: "a", TimeoutSeconds: 10, CacheMinutes: 1},
		{URL: "https://x", TimeoutSeconds: 10, CacheMinutes: 1},
		{URL: "https://x", ApplicationName: "a", TimeoutSeconds: 10, CacheMinutes: 61},
		{TimeoutSeconds: 0, CacheMinutes: 1},
		{TimeoutSeconds: 10, CacheMinutes: 1, GroupMappings: []GroupMapping{{Group: "g"}}},
	} {
		if err := ValidateCrowd(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
