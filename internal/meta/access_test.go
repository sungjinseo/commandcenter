package meta

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
)

func TestUserCRUD(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	u := User{ID: "alice", Name: "Alice", Email: "a@example.com", PasswordHash: "h", Roles: []string{"dev"}}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, u); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	got, err := db.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceLocal || got.Status != StatusActive || got.Name != "Alice" || !reflect.DeepEqual(got.Roles, []string{"dev"}) ||
		got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("stored user = %+v", got)
	}
	got.Status, got.Roles = StatusDisabled, []string{}
	if err := db.UpdateUser(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateUser(ctx, User{ID: "nobody"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	list, err := db.ListUsers(ctx)
	if err != nil || len(list) != 1 || list[0].Status != StatusDisabled || list[0].Roles == nil || len(list[0].Roles) != 0 {
		t.Fatalf("list = %+v %v", list, err)
	}

	// Deleting a user removes its tokens.
	if err := db.CreateToken(ctx, "t1", "alice", "hash1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.TokenUser(ctx, "hash1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token survived the user: %v", err)
	}
	if err := db.DeleteUser(ctx, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestRoleCRUD(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	r := Role{ID: "dev", Description: "devs", Privileges: []Privilege{{Target: "repo:a", Actions: []string{ActionRead, ActionAdd}}}}
	if err := db.CreateRole(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRole(ctx, r); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := db.GetRole(ctx, "dev")
	if err != nil || !reflect.DeepEqual(got.Privileges, r.Privileges) || got.Description != "devs" {
		t.Fatalf("got %+v %v", got, err)
	}
	got.Privileges = append(got.Privileges, Privilege{Target: "*", Actions: []string{ActionBrowse}})
	if err := db.UpdateRole(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateRole(ctx, Role{ID: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if err := db.CreateUser(ctx, User{ID: "bob", Roles: []string{"dev"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRole(ctx, "dev"); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete of a used role: %v", err)
	}
	if err := db.UpdateUser(ctx, User{ID: "bob", Roles: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRole(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRole(ctx, "dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	list, err := db.ListRoles(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("roles = %+v %v", list, err)
	}
}

func TestSettingsDefaultsAndTypedHelpers(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	s, err := db.LoadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, DefaultSettings()) {
		t.Fatalf("defaults = %+v", s)
	}
	if !s.AuthenticatedRead || s.AnonymousRead || s.MaxUploadMiB != 1024 || s.BaseURL != "http://localhost:8081" ||
		!reflect.DeepEqual(s.Realms, []Realm{{ID: RealmLocal, Enabled: true}, {ID: RealmToken, Enabled: true}}) {
		t.Fatalf("defaults = %+v", s)
	}
	s.AuthenticatedRead, s.AnonymousRead, s.MaxUploadMiB, s.BaseURL = false, true, 5, "https://repo.example.com"
	s.Realms = []Realm{{ID: RealmToken, Enabled: true}, {ID: RealmLocal, Enabled: false}}
	if err := db.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadSettings(ctx)
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("reloaded %+v %v", got, err)
	}
	var b bool
	if ok, err := db.GetSettingJSON(ctx, SettingAuthenticatedRead, &b); err != nil || !ok || b {
		t.Fatalf("GetSettingJSON = %v %v %v", ok, b, err)
	}
	if ok, err := db.GetSettingJSON(ctx, "nope", &b); err != nil || ok {
		t.Fatalf("missing key = %v %v", ok, err)
	}
	if err := db.SetSettingJSON(ctx, SettingMaxUploadMiB, 7); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.LoadSettings(ctx); got.MaxUploadMiB != 7 {
		t.Fatalf("maxUploadMiB = %d", got.MaxUploadMiB)
	}
	// A corrupt value is an error, not a silent default.
	if err := db.SetSetting(ctx, SettingAnonymousRead, "maybe"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadSettings(ctx); err == nil {
		t.Fatal("corrupt setting loaded")
	}
}

func accessConfig() *config.Config {
	f := false
	return &config.Config{
		BaseURL:       "https://repo.example.com",
		MaxUploadMiB:  64,
		AnonymousRead: true,
		Users: []config.User{
			{Name: "admin", PasswordHash: "h1", Roles: []string{"admin"}},
			{Name: "ci", PasswordHash: "h2", Roles: []string{"deployer", "all"}},
			{Name: "reader", PasswordHash: "h3"},
		},
		Roles: map[string]config.Role{
			"deployer": {Write: []string{"releases", "snapshots", "releases"}},
			"all":      {Write: []string{"*"}},
			"empty":    {},
		},
		Repositories: []config.Repository{{Name: "private", AnonymousRead: &f}},
	}
}

func TestImportMapping(t *testing.T) {
	cfg := accessConfig()
	every := []string{ActionBrowse, ActionRead, ActionAdd, ActionEdit, ActionDelete}
	roles := RolesFromConfig(cfg)
	want := []Role{
		BuiltinAdminRole(),
		{ID: "all", Description: "Imported from the config file", Privileges: []Privilege{{Target: "*", Actions: every}}},
		{ID: "deployer", Description: "Imported from the config file", Privileges: []Privilege{
			{Target: "repo:releases", Actions: every}, {Target: "repo:snapshots", Actions: every}}},
		{ID: "empty", Description: "Imported from the config file", Privileges: []Privilege{}},
	}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles =\n%+v\nwant\n%+v", roles, want)
	}
	users := UsersFromConfig(cfg)
	if len(users) != 3 || users[1].ID != "ci" || users[1].PasswordHash != "h2" || users[1].Source != SourceLocal ||
		users[1].Status != StatusActive || !reflect.DeepEqual(users[1].Roles, []string{"deployer", "all"}) ||
		users[2].Roles == nil {
		t.Fatalf("users = %+v", users)
	}
	s := SettingsFromConfig(cfg)
	if s.BaseURL != "https://repo.example.com" || s.MaxUploadMiB != 64 || !s.AnonymousRead || !s.AuthenticatedRead ||
		!reflect.DeepEqual(s.Realms, DefaultSettings().Realms) {
		t.Fatalf("settings = %+v", s)
	}
	// A config role named admin never replaces the built-in one.
	cfg.Roles["admin"] = config.Role{Write: []string{"x"}}
	if r := RolesFromConfig(cfg); r[0].ID != AdminRole || len(r) != 4 || !reflect.DeepEqual(r[0], BuiltinAdminRole()) {
		t.Fatalf("roles with a config admin = %+v", r)
	}
}

func TestImportAccessOnce(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()
	if done, err := db.AccessImported(ctx); err != nil || done {
		t.Fatalf("before: %v %v", done, err)
	}
	imported, err := db.ImportAccess(ctx, accessConfig())
	if err != nil || !imported {
		t.Fatalf("import: %v %v", imported, err)
	}
	users, _ := db.ListUsers(ctx)
	roles, _ := db.ListRoles(ctx)
	s, _ := db.LoadSettings(ctx)
	if len(users) != 3 || len(roles) != 4 || roles[0].ID != "admin" || s.BaseURL != "https://repo.example.com" || !s.AnonymousRead {
		t.Fatalf("after import: %+v %+v %+v", users, roles, s)
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	other := &config.Config{Users: []config.User{{Name: "x", PasswordHash: "h"}}, BaseURL: "http://other"}
	if imported, err := db2.ImportAccess(ctx, other); err != nil || imported {
		t.Fatalf("second import: %v %v", imported, err)
	}
	if done, _ := db2.AccessImported(ctx); !done {
		t.Fatal("flag not set")
	}
	users, _ = db2.ListUsers(ctx)
	s, _ = db2.LoadSettings(ctx)
	if len(users) != 3 || s.BaseURL != "https://repo.example.com" {
		t.Fatalf("second import changed data: %+v %+v", users, s)
	}
}

func TestImportAccessIsAtomic(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	// A user already present makes the import's insert fail; nothing of the
	// import may remain, and the flag stays unset.
	if err := db.CreateUser(ctx, User{ID: "ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportAccess(ctx, accessConfig()); err == nil {
		t.Fatal("import over an existing user succeeded")
	}
	if done, _ := db.AccessImported(ctx); done {
		t.Fatal("flag set after a failed import")
	}
	if roles, _ := db.ListRoles(ctx); len(roles) != 0 {
		t.Fatalf("roles left behind: %+v", roles)
	}
	if ok, _ := db.GetSettingJSON(ctx, SettingBaseURL, new(string)); ok {
		t.Fatal("settings left behind")
	}
}

func TestUserJSONHasNoHash(t *testing.T) {
	b, err := json.Marshal(User{ID: "u", PasswordHash: "$2a$secret"})
	if err != nil || strings.Contains(string(b), "secret") {
		t.Fatalf("%s %v", b, err)
	}
}

func TestRealmsValidated(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for name, realms := range map[string][]Realm{
		"duplicate": {{ID: RealmLocal, Enabled: true}, {ID: RealmLocal, Enabled: false}},
		"unknown":   {{ID: "kerberos", Enabled: true}},
	} {
		s := DefaultSettings()
		s.Realms = realms
		if err := db.SaveSettings(ctx, s); err == nil {
			t.Errorf("%s: SaveSettings accepted %v", name, realms)
		}
		if err := db.SetSettingJSON(ctx, SettingRealms, realms); err == nil {
			t.Errorf("%s: SetSettingJSON accepted %v", name, realms)
		}
	}
	if err := ValidateRealms([]Realm{{ID: RealmLDAP}, {ID: RealmLocal, Enabled: true}, {ID: RealmToken}}); err != nil {
		t.Fatal(err)
	}
	// A bad stored list is an error on load.
	if err := db.SetSetting(ctx, SettingRealms, `[{"id":"x","enabled":true}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadSettings(ctx); err == nil {
		t.Fatal("invalid stored realms loaded")
	}
}

func TestLoadAccess(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if _, err := db.ImportAccess(ctx, accessConfig()); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadAccess(ctx)
	if err != nil || len(got.Users) != 3 || len(got.Roles) != 4 || got.Settings.BaseURL != "https://repo.example.com" {
		t.Fatalf("LoadAccess = %+v %v", got, err)
	}
}

func TestNewSettingsKeys(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	s, _ := db.LoadSettings(ctx)
	if s.SessionIdleMinutes != 720 || s.DefaultLanguage != "ko" {
		t.Fatalf("defaults = %+v", s)
	}
	s.SessionIdleMinutes, s.DefaultLanguage = 30, "en"
	if err := db.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.LoadSettings(ctx); got.SessionIdleMinutes != 30 || got.DefaultLanguage != "en" {
		t.Fatalf("reloaded = %+v", got)
	}
	for key, v := range map[string]any{
		SettingSessionIdleMinutes: 4,
		SettingMaxUploadMiB:       0,
		SettingBaseURL:            "ftp://x",
		SettingDefaultLanguage:    "fr",
	} {
		if err := db.SetSettingJSON(ctx, key, v); err == nil {
			t.Errorf("%s=%v accepted", key, v)
		}
	}
	for _, v := range []string{"http://u:p@repo.example", "http://repo.example/?q=1", "http://repo.example/#f"} { // QA N-24
		if err := db.SetSettingJSON(ctx, SettingBaseURL, v); err == nil {
			t.Errorf("baseURL %q accepted", v)
		}
	}
	for _, v := range []int{5, 10080} {
		if err := db.SetSettingJSON(ctx, SettingSessionIdleMinutes, v); err != nil {
			t.Errorf("idle %d: %v", v, err)
		}
	}
	if err := db.SetSettingJSON(ctx, SettingSessionIdleMinutes, 10081); err == nil {
		t.Error("idle 10081 accepted")
	}
}
