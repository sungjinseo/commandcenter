package meta

import (
	"context"
	"reflect"
	"testing"
)

// I4 (final review): the offline recovery sets the password, re-activates
// the user (creating a local one if missing), adds the admin role on
// request, and makes sure the local realm is listed and enabled, all in one
// transaction; it reports each change.
func TestResetLocalPassword(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	if _, err := db.ImportAccess(ctx, accessConfig()); err != nil {
		t.Fatal(err)
	}
	// lock everything out: ci disabled, local realm gone
	ci, _ := db.GetUser(ctx, "ci")
	ci.Status = StatusDisabled
	if err := db.UpdateUser(ctx, ci); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSettingJSON(ctx, SettingRealms, []Realm{{ID: RealmToken, Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	changes, err := db.ResetLocalPassword(ctx, "ci", "newhash", true)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetUser(ctx, "ci")
	if got.PasswordHash != "newhash" || got.Status != StatusActive || !reflect.DeepEqual(got.Roles, []string{"deployer", "all", "admin"}) {
		t.Fatalf("ci = %+v", got)
	}
	s, _ := db.LoadSettings(ctx)
	if !reflect.DeepEqual(s.Realms, []Realm{{ID: RealmLocal, Enabled: true}, {ID: RealmToken, Enabled: true}}) {
		t.Fatalf("realms = %+v", s.Realms)
	}
	want := []string{`set the password of user "ci"`, `status of user "ci": disabled -> active`, `added the admin role to user "ci"`, `added the local realm (enabled) to the realm list`}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %q", changes)
	}

	// a disabled local realm is enabled in place; a missing user is created
	if err := db.SetSettingJSON(ctx, SettingRealms, []Realm{{ID: RealmToken, Enabled: true}, {ID: RealmLocal, Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	changes, err = db.ResetLocalPassword(ctx, "rescue", "h9", false)
	if err != nil {
		t.Fatal(err)
	}
	nu, err := db.GetUser(ctx, "rescue")
	if err != nil || nu.Source != SourceLocal || nu.Status != StatusActive || nu.PasswordHash != "h9" || len(nu.Roles) != 0 {
		t.Fatalf("rescue = %+v %v", nu, err)
	}
	s, _ = db.LoadSettings(ctx)
	if !reflect.DeepEqual(s.Realms, []Realm{{ID: RealmToken, Enabled: true}, {ID: RealmLocal, Enabled: true}}) {
		t.Fatalf("realms = %+v", s.Realms)
	}
	if !reflect.DeepEqual(changes, []string{`created local user "rescue"`, `enabled the local realm`}) {
		t.Fatalf("changes = %q", changes)
	}

	// an LDAP override row is refused and left untouched
	if err := db.CreateUser(ctx, User{ID: "dirk", Source: SourceLDAP, Status: StatusDisabled, Roles: []string{"deployer"}}); err != nil {
		t.Fatal(err)
	}
	_, err = db.ResetLocalPassword(ctx, "dirk", "h7", true)
	if err == nil || err.Error() != `user "dirk" is an LDAP role-override record; choose another id for a local recovery account` {
		t.Fatalf("dirk: %v", err)
	}
	dirk, _ := db.GetUser(ctx, "dirk")
	if dirk.Source != SourceLDAP || dirk.Status != StatusDisabled || dirk.PasswordHash != "" || !reflect.DeepEqual(dirk.Roles, []string{"deployer"}) {
		t.Fatalf("dirk changed: %+v", dirk)
	}

	if _, err := db.ResetLocalPassword(ctx, "bad:id", "h", false); err == nil {
		t.Fatal("invalid id accepted")
	}
}
