package auth

import (
	"context"
	"errors"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

type oidcFixture struct {
	*accessFixture
	a   *Authenticator
	tok memTokens
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	f := &oidcFixture{accessFixture: newFixture(t), tok: memTokens{}}
	c := meta.DefaultOIDC()
	c.Issuer, c.ClientID = "https://idp.example", "cc"
	c.GroupMappings = []meta.GroupMapping{{Group: "cc-admins", Roles: []string{"admin"}}, {Group: "org:devs", Roles: []string{"adder"}}}
	f.settings.OIDC = c
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLocal, Enabled: true}, {ID: meta.RealmToken, Enabled: true}, {ID: meta.RealmOIDC, Enabled: true}}
	f.a = New(&config.Config{}, f.tok)
	f.a.Sessions = NewSessions()
	f.apply()
	return f
}

func (f *oidcFixture) apply() { f.a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings)) }

func TestOIDCRecordAndLookup(t *testing.T) {
	f := newOIDCFixture(t)
	p, err := f.a.RecordOIDC(OIDCUser{ID: "alice", Name: "Alice", Email: "a@x", Groups: []string{"cc-admins"}})
	if err != nil || p.User != "alice" || !p.IsOIDC() || p.IsLDAP() || !p.Admin || p.Owner() != "oidc:alice" {
		t.Fatalf("principal %+v %v", p, err)
	}
	bob, err := f.a.RecordOIDC(OIDCUser{ID: "bob", Groups: []string{"org:devs"}})
	if err != nil || bob.Admin || bob.Roles()[0] != "adder" {
		t.Fatalf("bob %+v %v", bob, err)
	}
	if got, ok := f.a.Lookup("oidc:alice"); !ok || !got.Admin || !got.IsOIDC() {
		t.Fatalf("lookup %+v %v", got, ok)
	}
	if _, ok := f.a.Lookup("oidc:carol"); ok {
		t.Fatal("unknown user found")
	}
	if _, ok := f.a.Lookup("alice"); ok {
		t.Fatal("oidc user found as a local one")
	}
	us := f.a.OIDCUsers()
	if len(us) != 2 || us[0].ID != "alice" || us[0].Name != "Alice" || us[0].Roles[0] != "admin" || us[1].ID != "bob" {
		t.Fatalf("users %+v", us)
	}
	// Roles follow the live mappings.
	f.settings.OIDC.GroupMappings[0].Roles = []string{"editor"}
	f.apply() // the cache is dropped with the configuration change...
	if _, ok := f.a.Lookup("oidc:alice"); ok {
		t.Fatal("found after a configuration change")
	}
	if _, err := f.a.RecordOIDC(OIDCUser{ID: "alice", Groups: []string{"cc-admins"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.a.Lookup("oidc:alice"); got.Admin || got.Roles()[0] != "editor" {
		t.Fatalf("after mapping change %+v", got)
	}
	// Invalid ids never enter.
	if _, err := f.a.RecordOIDC(OIDCUser{ID: "not valid!"}); !errors.Is(err, ErrOIDCRefused) {
		t.Fatalf("invalid id: %v", err)
	}
}

func TestOIDCOverrideRow(t *testing.T) {
	f := newOIDCFixture(t)
	if _, err := f.a.RecordOIDC(OIDCUser{ID: "alice", Groups: []string{"org:devs"}}); err != nil {
		t.Fatal(err)
	}
	// An override row adds roles.
	f.users = append(f.users, meta.User{ID: "alice", Source: meta.SourceOIDC, Status: meta.StatusActive, Roles: []string{"editor"}})
	f.apply()
	if p, ok := f.a.Lookup("oidc:alice"); !ok || len(p.Roles()) != 2 || !hasRole(p.roles, "editor") || !hasRole(p.roles, "adder") {
		t.Fatalf("with override %+v %v", p, ok)
	}
	// A disabled row denies the user, also at login.
	f.users[len(f.users)-1].Status = meta.StatusDisabled
	f.apply()
	if _, ok := f.a.Lookup("oidc:alice"); ok {
		t.Fatal("disabled override admitted")
	}
	if _, err := f.a.RecordOIDC(OIDCUser{ID: "alice"}); !errors.Is(err, ErrOIDCRefused) {
		t.Fatalf("login with disabled override: %v", err)
	}
	if n := len(f.a.OIDCUsers()); n != 0 {
		t.Fatalf("%d users listed", n)
	}
	// The override row itself is never a principal.
	if _, ok := f.a.Lookup("alice"); ok {
		t.Fatal("override row looked up as a principal")
	}
	if _, _, err := f.a.CreateToken(context.Background(), "alice"); err == nil {
		t.Fatal("token for a disabled override row")
	}
	f.users[len(f.users)-1].Status = meta.StatusActive
	f.apply()
	if _, _, err := f.a.CreateToken(context.Background(), "alice"); !errors.Is(err, ErrLDAPUser) {
		t.Fatalf("token for override row: %v", err)
	}
}

func TestOIDCTokens(t *testing.T) {
	f := newOIDCFixture(t)
	ctx := context.Background()
	p, err := f.a.RecordOIDC(OIDCUser{ID: "alice", Groups: []string{"cc-admins"}})
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.a.CreateTokenFor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if f.tok[hashToken(tok)] != "oidc:alice" {
		t.Fatalf("owner %q", f.tok[hashToken(tok)])
	}
	got, err := f.a.Authenticate(req("Bearer " + tok))
	if err != nil || got.User != "alice" || !got.IsOIDC() || !got.Admin || got.Via != ViaToken {
		t.Fatalf("bearer %+v %v", got, err)
	}
	r := req("")
	r.SetBasicAuth("alice", tok)
	if got, err := f.a.Authenticate(r); err != nil || !got.IsOIDC() || got.Via != ViaToken {
		t.Fatalf("basic own token %+v %v", got, err)
	}
	r = req("")
	r.SetBasicAuth("bob", tok)
	if _, err := f.a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("basic other user: %v", err)
	}
	// A local alice shares nothing with the OIDC one.
	f.users = append(f.users, meta.User{ID: "alice", PasswordHash: hash(t, "pw"), Source: meta.SourceLocal, Status: meta.StatusActive})
	f.apply()
	if got, err := f.a.Authenticate(req("Bearer " + tok)); err != nil || !got.IsOIDC() || !got.Admin {
		t.Fatalf("collision %+v %v", got, err)
	}
	// After a restart (empty cache) the token fails closed until the user
	// signs in again; the realm being off fails it too.
	f.a.clearOIDC()
	if _, err := f.a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("after restart: %v", err)
	}
	if _, err := f.a.RecordOIDC(OIDCUser{ID: "alice", Groups: []string{"cc-admins"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.Authenticate(req("Bearer " + tok)); err != nil {
		t.Fatalf("after re-login: %v", err)
	}
	f.settings.Realms[2].Enabled = false
	f.apply()
	if _, err := f.a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("realm off: %v", err)
	}
	if _, err := f.a.RecordOIDC(OIDCUser{ID: "alice"}); !errors.Is(err, ErrOIDCRefused) {
		t.Fatalf("login with realm off: %v", err)
	}
	if _, _, err := f.a.CreateTokenFor(ctx, p); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("token with realm off: %v", err)
	}
}

func TestOIDCSessionsEndOnUserChange(t *testing.T) {
	f := newOIDCFixture(t)
	if _, err := f.a.RecordOIDC(OIDCUser{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	id := f.a.Sessions.Create("oidc:alice")
	if _, ok := f.a.Sessions.User(id); !ok {
		t.Fatal("no session")
	}
	f.a.OnUserChanged("alice")
	if _, ok := f.a.Sessions.User(id); ok {
		t.Fatal("session survived OnUserChanged")
	}
	if _, ok := f.a.Lookup("oidc:alice"); ok {
		t.Fatal("entry survived OnUserChanged")
	}
}
