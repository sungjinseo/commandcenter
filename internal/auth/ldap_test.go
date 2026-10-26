package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/ldap"
	"commandcenter/internal/ldap/ldaptest"
	"commandcenter/internal/meta"
)

const (
	ldapSvcDN = "cn=svc,dc=example,dc=org"
	ldapSvcPW = "svc-secret"
	aliceDN   = "uid=alice,ou=people,dc=example,dc=org"
)

type ldapFixture struct {
	*accessFixture
	srv   *ldaptest.Server
	a     *Authenticator
	tok   memTokens
	clock time.Time
}

// newLDAPFixture serves alice (cc-admins) and bob (no groups) from a fake
// directory; realms are local, token, ldap (in that order).
func newLDAPFixture(t *testing.T) *ldapFixture {
	t.Helper()
	s := ldaptest.New(t, nil)
	s.Add(ldaptest.Entry{DN: ldapSvcDN, Password: ldapSvcPW})
	s.Add(ldaptest.Entry{DN: aliceDN, Password: "alice-pw", Attrs: map[string][]string{
		"uid": {"alice"}, "cn": {"Alice"}, "mail": {"alice@example.org"},
		"memberOf": {"cn=cc-admins,ou=groups,dc=example,dc=org"}}})
	s.Add(ldaptest.Entry{DN: "uid=bob,ou=people,dc=example,dc=org", Password: "bob-pw", Attrs: map[string][]string{
		"uid": {"bob"}, "cn": {"Bob"}}})
	f := &ldapFixture{accessFixture: newFixture(t), srv: s, clock: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	l := meta.DefaultLDAP()
	l.URL = s.URL("ldap")
	l.BindDN, l.BindPassword = ldapSvcDN, ldapSvcPW
	l.UserBaseDN = "ou=people,dc=example,dc=org"
	l.TimeoutSeconds = 2
	l.GroupMappings = []meta.GroupMapping{{Group: "cc-admins", Roles: []string{"admin"}}}
	f.settings.LDAP = l
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLocal, Enabled: true}, {ID: meta.RealmToken, Enabled: true}, {ID: meta.RealmLDAP, Enabled: true}}
	f.tok = memTokens{}
	f.a = New(&config.Config{}, f.tok)
	f.a.now = func() time.Time { return f.clock }
	f.apply()
	return f
}

func (f *ldapFixture) apply() { f.a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings)) }

func (f *ldapFixture) advance(d time.Duration) { f.clock = f.clock.Add(d) }

func TestLDAPLogin(t *testing.T) {
	f := newLDAPFixture(t)
	ctx := context.Background()
	p, err := f.a.CheckPassword(ctx, "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	if p.User != "alice" || !p.IsLDAP() || !p.Admin || p.Via != ViaPassword || p.Owner() != "ldap:alice" {
		t.Fatalf("principal %+v", p)
	}
	if got := p.Roles(); len(got) != 1 || got[0] != "admin" {
		t.Fatalf("roles %v", got)
	}
	bob, err := f.a.CheckPassword(ctx, "bob", "bob-pw")
	if err != nil || bob.Admin || !bob.IsLDAP() {
		t.Fatalf("bob %+v %v", bob, err)
	}
	if _, err := f.a.CheckPassword(ctx, "alice", "wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	// The local realm still works for local users.
	if p, err := f.a.CheckPassword(ctx, "admin", "pw"); err != nil || p.IsLDAP() {
		t.Fatalf("local admin %+v %v", p, err)
	}
	// The ldap realm off: no LDAP login.
	f.settings.Realms[2].Enabled = false
	f.apply()
	if _, err := f.a.CheckPassword(ctx, "bob", "bob-pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("realm off: %v", err)
	}
}

func TestLDAPEmptyPasswordNeverBinds(t *testing.T) {
	f := newLDAPFixture(t)
	before := f.srv.Binds()
	for _, pw := range []string{"", ""} {
		if _, err := f.a.CheckPassword(context.Background(), "alice", pw); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("empty password: %v", err)
		}
	}
	if f.srv.Binds() != before {
		t.Fatal("an empty password reached the directory")
	}
}

func TestLDAPFilterInjectionLogin(t *testing.T) {
	f := newLDAPFixture(t)
	f.settings.LDAP.UserFilter = "(&(uid={user})(cn=*))"
	f.apply()
	// Alice alone: an unescaped "*" would match exactly her.
	f.srv.Remove("uid=bob,ou=people,dc=example,dc=org")
	for _, name := range []string{"*", "*)(uid=*", "al*", "alice)(|(uid=*"} {
		if _, err := f.a.CheckPassword(context.Background(), name, "alice-pw"); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%q logged in: %v", name, err)
		}
	}
}

func TestLDAPRealmOrder(t *testing.T) {
	f := newLDAPFixture(t)
	ctx := context.Background()
	// A local user named alice (no admin role), password "pw".
	f.users = append(f.users, meta.User{ID: "alice", PasswordHash: hash(t, "pw"), Source: meta.SourceLocal,
		Status: meta.StatusActive, Roles: []string{"adder"}})
	f.apply()
	if p, err := f.a.CheckPassword(ctx, "alice", "pw"); err != nil || p.IsLDAP() || p.Admin {
		t.Fatalf("local first, local password: %+v %v", p, err)
	}
	if p, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err != nil || !p.IsLDAP() || !p.Admin {
		t.Fatalf("local first, ldap password falls through: %+v %v", p, err)
	}
	// ldap first.
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLDAP, Enabled: true}, {ID: meta.RealmLocal, Enabled: true}}
	f.apply()
	f.a.clearLDAP()
	if p, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err != nil || !p.IsLDAP() {
		t.Fatalf("ldap first: %+v %v", p, err)
	}
	if p, err := f.a.CheckPassword(ctx, "alice", "pw"); err != nil || p.IsLDAP() {
		t.Fatalf("ldap first, local password: %+v %v", p, err)
	}
	// Sessions are keyed by owner, so the two alices never mix.
	if p, ok := f.a.Lookup("alice"); !ok || p.IsLDAP() || p.Admin {
		t.Fatalf("Lookup(alice) = %+v", p)
	}
	if p, ok := f.a.Lookup("ldap:alice"); !ok || !p.IsLDAP() || !p.Admin {
		t.Fatalf("Lookup(ldap:alice) = %+v %v", p, ok)
	}
}

func TestLDAPUnreachableFallsThrough(t *testing.T) {
	f := newLDAPFixture(t)
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLDAP, Enabled: true}, {ID: meta.RealmLocal, Enabled: true}}
	f.apply()
	f.srv.SetDown(true)
	ctx := context.Background()
	if p, err := f.a.CheckPassword(ctx, "admin", "pw"); err != nil || p.IsLDAP() {
		t.Fatalf("local admin during an LDAP outage: %+v %v", p, err)
	}
	if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("ldap user during outage: %v", err)
	}
}

func TestLDAPOverrideRow(t *testing.T) {
	f := newLDAPFixture(t)
	ctx := context.Background()
	// An active ldap-source row adds its roles.
	f.users = append(f.users, meta.User{ID: "bob", Source: meta.SourceLDAP, Status: meta.StatusActive, Roles: []string{"adder"}})
	f.apply()
	p, err := f.a.CheckPassword(ctx, "bob", "bob-pw")
	if err != nil || !f.a.Can(p, "releases", Add) {
		t.Fatalf("override roles: %+v %v", p, err)
	}
	// A disabled row denies the LDAP user.
	f.users[len(f.users)-1].Status = meta.StatusDisabled
	f.apply()
	f.a.OnUserChanged("bob")
	if _, err := f.a.CheckPassword(ctx, "bob", "bob-pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("disabled override row: %v", err)
	}
	if _, ok := f.a.Lookup("ldap:bob"); ok {
		t.Fatal("disabled override row: session still valid")
	}
	// A local row with the same id grants the LDAP user nothing.
	f.users[len(f.users)-1] = meta.User{ID: "bob", PasswordHash: hash(t, "pw"), Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"admin"}}
	f.apply()
	f.a.OnUserChanged("bob")
	p, err = f.a.CheckPassword(ctx, "bob", "bob-pw")
	if err != nil || !p.IsLDAP() || p.Admin {
		t.Fatalf("local row leaked into the LDAP principal: %+v %v", p, err)
	}
}

func TestLDAPInvalidUserIDDenied(t *testing.T) {
	f := newLDAPFixture(t)
	f.srv.Add(ldaptest.Entry{DN: "uid=weird,ou=people,dc=example,dc=org", Password: "w-pw", Attrs: map[string][]string{"uid": {"we ird"}}})
	f.settings.LDAP.UserFilter = "(cn={user})"
	f.srv.Add(ldaptest.Entry{DN: "cn=w,ou=people,dc=example,dc=org", Password: "w-pw", Attrs: map[string][]string{"cn": {"w"}, "uid": {"ldap:x"}}})
	f.apply()
	if _, err := f.a.CheckPassword(context.Background(), "w", "w-pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("invalid id accepted: %v", err)
	}
}

func TestLDAPPasswordCacheKey(t *testing.T) {
	f := newLDAPFixture(t)
	ctx := context.Background()
	if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err != nil {
		t.Fatal(err)
	}
	binds := f.srv.Binds()
	if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err != nil {
		t.Fatal(err)
	}
	if f.srv.Binds() != binds {
		t.Fatal("a repeated login within a minute was not cached")
	}
	f.a.ldapMu.Lock()
	for k := range f.a.ldapPass {
		if strings.Contains(k, "alice-pw") {
			t.Errorf("plaintext password in cache key %q", k)
		}
	}
	f.a.ldapMu.Unlock()
	// A wrong password is never served from the cache.
	if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw2"); err == nil {
		t.Fatal("wrong password accepted")
	}
	// After a minute the directory is asked again.
	f.advance(61 * time.Second)
	if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err != nil {
		t.Fatal(err)
	}
	if f.srv.Binds() == binds {
		t.Fatal("cache outlived a minute")
	}
}

func TestLDAPPrincipalRecheck(t *testing.T) {
	f := newLDAPFixture(t)
	if _, err := f.a.CheckPassword(context.Background(), "alice", "alice-pw"); err != nil {
		t.Fatal(err)
	}
	searches := f.srv.Searches()
	if p, ok := f.a.Lookup("ldap:alice"); !ok || !p.Admin || p.Via != ViaNone {
		t.Fatalf("fresh lookup %+v %v", p, ok)
	}
	if f.srv.Searches() != searches {
		t.Fatal("a fresh entry was re-checked")
	}
	f.advance(61 * time.Second)
	if _, ok := f.a.Lookup("ldap:alice"); !ok {
		t.Fatal("re-check failed")
	}
	if f.srv.Searches() != searches+1 {
		t.Fatalf("searches %d, want one re-check", f.srv.Searches()-searches)
	}
	// Removed from the directory: valid until the next re-check, then denied.
	f.srv.Remove(aliceDN)
	if _, ok := f.a.Lookup("ldap:alice"); !ok {
		t.Fatal("denied before the minute passed")
	}
	f.advance(61 * time.Second)
	if _, ok := f.a.Lookup("ldap:alice"); ok {
		t.Fatal("removed user still accepted")
	}
	// Group changes apply at the re-check.
	f.srv.Add(ldaptest.Entry{DN: aliceDN, Password: "alice-pw", Attrs: map[string][]string{"uid": {"alice"}}})
	if p, ok := f.a.Lookup("ldap:alice"); !ok || p.Admin {
		t.Fatalf("regroup %+v %v", p, ok)
	}
	// Unknown ids and invalid keys are denied.
	if _, ok := f.a.Lookup("ldap:nobody"); ok {
		t.Fatal("unknown LDAP id accepted")
	}
	if _, ok := f.a.Lookup("ldap:"); ok {
		t.Fatal("empty LDAP id accepted")
	}
}

func TestLDAPFailClosed(t *testing.T) {
	f := newLDAPFixture(t)
	if _, err := f.a.CheckPassword(context.Background(), "alice", "alice-pw"); err != nil {
		t.Fatal(err)
	}
	f.srv.SetDown(true)
	if _, ok := f.a.Lookup("ldap:alice"); !ok {
		t.Fatal("fresh entry needs no directory")
	}
	f.advance(61 * time.Second)
	if _, ok := f.a.Lookup("ldap:alice"); ok {
		t.Fatal("outage: re-check failed open")
	}
	searches := f.srv.Searches()
	f.srv.SetDown(false)
	if _, ok := f.a.Lookup("ldap:alice"); ok {
		t.Fatal("retried before the back-off")
	}
	if f.srv.Searches() != searches {
		t.Fatal("dialled during the back-off")
	}
	f.advance(ldapRetry)
	if _, ok := f.a.Lookup("ldap:alice"); !ok {
		t.Fatal("not restored after the outage")
	}
	// Local admins work throughout.
	f.srv.SetDown(true)
	f.advance(2 * time.Minute)
	if _, ok := f.a.Lookup("admin"); !ok {
		t.Fatal("local admin affected by the LDAP outage")
	}
}

func TestLDAPRecheckSerialized(t *testing.T) {
	f := newLDAPFixture(t)
	if _, err := f.a.CheckPassword(context.Background(), "alice", "alice-pw"); err != nil {
		t.Fatal(err)
	}
	f.advance(61 * time.Second)
	searches := f.srv.Searches()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := f.a.Lookup("ldap:alice"); !ok {
				t.Error("lookup failed")
			}
		}()
	}
	wg.Wait()
	if n := f.srv.Searches() - searches; n != 1 {
		t.Fatalf("%d re-checks, want 1", n)
	}
}

func TestLDAPTokens(t *testing.T) {
	f := newLDAPFixture(t)
	ctx := context.Background()
	p, err := f.a.CheckPassword(ctx, "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.a.CreateTokenFor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if f.tok[hashToken(tok)] != "ldap:alice" {
		t.Fatalf("owner %q", f.tok[hashToken(tok)])
	}
	got, err := f.a.Authenticate(req("Bearer " + tok))
	if err != nil || got.User != "alice" || !got.IsLDAP() || !got.Admin || got.Via != ViaToken {
		t.Fatalf("bearer %+v %v", got, err)
	}
	r := req("")
	r.SetBasicAuth("alice", tok)
	if got, err := f.a.Authenticate(r); err != nil || !got.IsLDAP() || got.Via != ViaToken {
		t.Fatalf("basic own token %+v %v", got, err)
	}
	r = req("")
	r.SetBasicAuth("bob", tok)
	if _, err := f.a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("basic other user: %v", err)
	}
	// A local user named alice cannot use (or be confused with) the LDAP token owner.
	f.users = append(f.users, meta.User{ID: "alice", PasswordHash: hash(t, "pw"), Source: meta.SourceLocal, Status: meta.StatusActive})
	f.apply()
	if got, err := f.a.Authenticate(req("Bearer " + tok)); err != nil || !got.IsLDAP() || !got.Admin {
		t.Fatalf("collision %+v %v", got, err)
	}
	// Removed from the directory: the token stops after the re-check.
	f.srv.Remove(aliceDN)
	f.advance(61 * time.Second)
	if _, err := f.a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("removed user's token: %v", err)
	}
	// Realms gate LDAP tokens: both ldap and token must be on.
	f.srv.Add(ldaptest.Entry{DN: aliceDN, Password: "alice-pw", Attrs: map[string][]string{"uid": {"alice"}}})
	f.advance(61 * time.Second)
	if _, err := f.a.Authenticate(req("Bearer " + tok)); err != nil {
		t.Fatalf("restored: %v", err)
	}
	f.settings.Realms[2].Enabled = false
	f.apply()
	if _, err := f.a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("ldap realm off: %v", err)
	}
	f.settings.Realms[2].Enabled, f.settings.Realms[1].Enabled = true, false
	f.apply()
	if _, err := f.a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("token realm off: %v", err)
	}
	// An anonymous or forged LDAP principal cannot mint tokens.
	if _, _, err := f.a.CreateTokenFor(ctx, Principal{}); err == nil {
		t.Fatal("anonymous token")
	}
}

func TestLDAPTokenAtMostOneSearchPerMinute(t *testing.T) {
	f := newLDAPFixture(t)
	ctx := context.Background()
	p, err := f.a.CheckPassword(ctx, "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.a.CreateTokenFor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	f.a.clearLDAP() // as after a restart: the cache is empty
	searches := f.srv.Searches()
	for i := 0; i < 5; i++ {
		if _, err := f.a.Authenticate(req("Bearer " + tok)); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.srv.Searches() - searches; n != 1 {
		t.Fatalf("%d searches for 5 uses", n)
	}
}

func TestLDAPOnUserChangedAndReload(t *testing.T) {
	f := newLDAPFixture(t)
	f.a.Sessions = NewSessions()
	ctx := context.Background()
	p, err := f.a.CheckPassword(ctx, "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	sid := f.a.Sessions.Create(p.Owner())
	f.a.OnUserChanged("alice")
	if _, ok := f.a.Sessions.User(sid); ok {
		t.Fatal("LDAP session survived OnUserChanged")
	}
	if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err != nil {
		t.Fatal(err)
	}
	// A changed LDAP configuration drops the cached LDAP state.
	f.settings.LDAP.GroupMappings = nil
	f.apply()
	f.a.ldapMu.Lock()
	n, m := len(f.a.ldapUsers), len(f.a.ldapPass)
	f.a.ldapMu.Unlock()
	if n != 0 || m != 0 {
		t.Fatalf("cache kept %d users, %d passwords after an LDAP change", n, m)
	}
	if p, ok := f.a.Lookup("ldap:alice"); !ok || p.Admin {
		t.Fatalf("mapping removal not applied: %+v %v", p, ok)
	}
}

type sourceFunc func(context.Context) (meta.AccessData, error)

func (f sourceFunc) LoadAccess(ctx context.Context) (meta.AccessData, error) { return f(ctx) }

// The app reloads every few seconds (QA N-2): a reload that changes nothing
// keeps the LDAP and password caches, so it costs no directory round trips
// or bcrypt compares.
func TestReloadWithoutChangeKeepsCaches(t *testing.T) {
	f := newLDAPFixture(t)
	f.a.Source = sourceFunc(func(context.Context) (meta.AccessData, error) {
		return meta.AccessData{Users: f.users, Roles: f.roles, Settings: f.settings}, nil
	})
	ctx := context.Background()
	for user, pw := range map[string]string{"alice": "alice-pw", "admin": "pw"} {
		if _, err := f.a.CheckPassword(ctx, user, pw); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := f.a.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	f.a.ldapMu.Lock()
	n, m := len(f.a.ldapUsers), len(f.a.ldapPass)
	f.a.ldapMu.Unlock()
	if n != 1 || m != 1 || f.a.cachedCount("admin") != 1 {
		t.Fatalf("after reloads: %d LDAP users, %d LDAP passwords, %d local passwords cached", n, m, f.a.cachedCount("admin"))
	}
}

func TestCheckLockoutLDAP(t *testing.T) {
	f := newLDAPFixture(t)
	snap := func() *Snapshot { return NewSnapshot(f.users, f.roles, f.settings) }
	// Local realm off, ldap on with an admin mapping: an admin can log in.
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLocal, Enabled: false}, {ID: meta.RealmLDAP, Enabled: true}}
	if err := CheckLockout(snap()); err != nil {
		t.Fatalf("ldap admin mapping: %v", err)
	}
	// Default roles granting admin count too.
	f.settings.LDAP.GroupMappings = nil
	f.settings.LDAP.DefaultRoles = []string{"admin"}
	if err := CheckLockout(snap()); err != nil {
		t.Fatalf("ldap default admin: %v", err)
	}
	for name, mut := range map[string]func(){
		"no admin mapping": func() { f.settings.LDAP.DefaultRoles = []string{"adder"} },
		"ldap realm off":   func() { f.settings.Realms[1].Enabled = false },
		"no url":           func() { f.settings.LDAP.URL = "" },
		"no base":          func() { f.settings.LDAP.UserBaseDN = "" },
		"ldap admin row only": func() {
			f.settings.LDAP.DefaultRoles = nil
			f.users = append(f.users, meta.User{ID: "x", Source: meta.SourceLDAP, Status: meta.StatusActive, Roles: []string{"admin"}})
		},
	} {
		save := *f.accessFixture
		save.settings.LDAP = f.settings.LDAP.Copy()
		save.settings.Realms = append([]meta.Realm{}, f.settings.Realms...)
		save.users = append([]meta.User{}, f.users...)
		mut()
		if err := CheckLockout(snap()); !errors.Is(err, ErrLockout) {
			t.Errorf("%s: %v", name, err)
		}
		*f.accessFixture = save
	}
}

func TestRestorableAdmins(t *testing.T) {
	f := newFixture(t)
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLocal, Enabled: false}}
	if n := RestorableAdmins(NewSnapshot(f.users, f.roles, f.settings)); n != 1 {
		t.Fatalf("restorable %d, want 1 (realm state ignored; the disabled admin does not count)", n)
	}
	for _, h := range []string{"", "<commandcenter hash-password 출력값>"} {
		f.users[0].PasswordHash = h
		if n := RestorableAdmins(NewSnapshot(f.users, f.roles, f.settings)); n != 0 {
			t.Fatalf("hash %q: restorable %d", h, n)
		}
	}
}

// m5: an unreachable directory is skipped on the login path for a while,
// so a local break-glass login does not wait on it every time.
func TestLDAPLoginOutageBackoff(t *testing.T) {
	f := newLDAPFixture(t)
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLDAP, Enabled: true}, {ID: meta.RealmLocal, Enabled: true}}
	f.apply()
	ctx := context.Background()
	f.srv.SetDown(true)
	d0 := f.srv.Dials()
	for i := 0; i < ldapDialFailures; i++ { // three consecutive dial failures start it
		if _, err := f.a.CheckPassword(ctx, "admin", "pw"); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.srv.Dials() - d0; n != ldapDialFailures {
		t.Fatalf("first logins: %d dials", n)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.a.CheckPassword(ctx, "admin", "pw"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("alice during back-off: %v", err)
		}
	}
	if n := f.srv.Dials() - d0; n != ldapDialFailures {
		t.Fatalf("dialled %d times during the back-off", n-ldapDialFailures)
	}
	f.srv.SetDown(false)
	f.advance(ldapLoginBackoff)
	if p, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err != nil || !p.IsLDAP() {
		t.Fatalf("after the back-off: %+v %v", p, err)
	}
	// A rejected password (resultCode 49) or an unknown user is not an outage.
	if _, err := f.a.CheckPassword(ctx, "bob", "wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	if _, err := f.a.CheckPassword(ctx, "nobody", "x"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	if p, err := f.a.CheckPassword(ctx, "bob", "bob-pw"); err != nil || !p.IsLDAP() {
		t.Fatalf("after a wrong password: %+v %v", p, err)
	}
}

func TestIsOutage(t *testing.T) {
	ctx := context.Background()
	for _, err := range []error{ldap.ErrInvalidCredentials, ldap.ErrNoSuchUser, ldap.ErrAmbiguous,
		fmt.Errorf("x: %w", ldap.ErrBadEntry), &ldap.ResultError{Op: "search", Code: 53}, &ldap.ResultError{Op: "bind", Code: 49}} {
		if isOutage(ctx, err) {
			t.Errorf("%v counted as an outage", err)
		}
	}
	if !isOutage(ctx, &ldap.DialError{Phase: "connect", Err: errors.New("ldap: connect to x: i/o timeout")}) {
		t.Error("connect failure is an outage")
	}
	// Untyped failures after the service bind (EOF mid-search, a notice of
	// disconnection, a parse or cap error) are not.
	for _, err := range []error{errors.New("ldap: read: unexpected EOF"), errors.New("ldap: the server closed the connection")} {
		if isOutage(ctx, err) {
			t.Errorf("%v counted as an outage", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if isOutage(cancelled, &ldap.DialError{Phase: "bind", Err: errors.New("ldap: read: context canceled")}) {
		t.Error("a caller that left is not an outage")
	}
}

// Fix round 2: only dial-phase failures (connect, TLS, service bind I/O)
// count toward the login back-off, and only three in a row.

// dialsDuring returns how many connections f makes.
func (f *ldapFixture) dialsDuring(fn func()) int64 {
	d0 := f.srv.Dials()
	fn()
	return f.srv.Dials() - d0
}

// ldapStillOn checks that an LDAP login still reaches the directory.
func (f *ldapFixture) ldapStillOn(t *testing.T) {
	t.Helper()
	f.a.clearPassCacheForTest()
	if n := f.dialsDuring(func() {
		if p, err := f.a.CheckPassword(context.Background(), "alice", "alice-pw"); err != nil || !p.IsLDAP() {
			t.Fatalf("LDAP login after the attempts: %+v %v", p, err)
		}
	}); n != 1 {
		t.Fatalf("LDAP login made %d connections (back-off tripped?)", n)
	}
}

func ldapFirst(t *testing.T) *ldapFixture {
	f := newLDAPFixture(t)
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLDAP, Enabled: true}, {ID: meta.RealmLocal, Enabled: true}}
	f.apply()
	return f
}

func TestLDAPOversizedCredentialsNeverReachTheDirectory(t *testing.T) {
	f := ldapFirst(t)
	ctx := context.Background()
	longName, longPass := strings.Repeat("a", 257), strings.Repeat("p", 1025)
	if n := f.dialsDuring(func() {
		for i := 0; i < 5; i++ {
			if _, err := f.a.CheckPassword(ctx, longName, "alice-pw"); !errors.Is(err, ErrBadCredentials) {
				t.Fatal(err)
			}
			if _, err := f.a.CheckPassword(ctx, "alice", longPass); !errors.Is(err, ErrBadCredentials) {
				t.Fatal(err)
			}
			r := req("")
			r.SetBasicAuth("alice", longPass)
			if _, err := f.a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
				t.Fatal(err)
			}
		}
	}); n != 0 {
		t.Fatalf("oversized credentials made %d connections", n)
	}
	// The caps are exclusive limits: 256 and 1024 bytes still reach the realm.
	if _, err := f.a.CheckPassword(ctx, strings.Repeat("a", 256), strings.Repeat("p", 1024)); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	f.ldapStillOn(t)
}

func TestLDAPNoticeAfterServiceBindIsNotAnOutage(t *testing.T) {
	f := ldapFirst(t)
	f.srv.SetNoticeOnSearch(true)
	for i := 0; i < 5; i++ {
		if _, err := f.a.CheckPassword(context.Background(), "alice", "alice-pw"); !errors.Is(err, ErrBadCredentials) {
			t.Fatal(err)
		}
	}
	f.srv.SetNoticeOnSearch(false)
	f.ldapStillOn(t)
}

func TestLDAPOverCapEntryIsNotAnOutage(t *testing.T) {
	f := ldapFirst(t)
	groups := make([]string, 1100)
	for i := range groups {
		groups[i] = fmt.Sprintf("cn=g%d,ou=groups,dc=example,dc=org", i)
	}
	f.srv.Add(ldaptest.Entry{DN: "uid=huge,ou=people,dc=example,dc=org", Password: "x", Attrs: map[string][]string{
		"uid": {"huge"}, "memberOf": groups}})
	for i := 0; i < 5; i++ {
		if _, err := f.a.CheckPassword(context.Background(), "huge", "anything"); !errors.Is(err, ErrBadCredentials) {
			t.Fatal(err)
		}
	}
	f.ldapStillOn(t)
}

func TestLDAPBackoffNeedsThreeDialFailures(t *testing.T) {
	f := ldapFirst(t)
	ctx := context.Background()
	login := func() {
		if _, err := f.a.CheckPassword(ctx, "admin", "pw"); err != nil {
			t.Fatal(err)
		}
	}
	f.srv.SetDown(true)
	if n := f.dialsDuring(func() { login(); login() }); n != 2 {
		t.Fatalf("two failures: %d dials", n)
	}
	// A success in between resets the count.
	f.srv.SetDown(false)
	f.ldapStillOn(t)
	f.srv.SetDown(true)
	if n := f.dialsDuring(func() { login(); login() }); n != 2 {
		t.Fatalf("two failures after a success: %d dials (back-off tripped too early)", n)
	}
	if n := f.dialsDuring(login); n != 1 {
		t.Fatalf("third failure: %d dials", n)
	}
	if n := f.dialsDuring(func() { login(); login() }); n != 0 {
		t.Fatalf("after three failures: %d dials, want the back-off", n)
	}
}

func (a *Authenticator) clearPassCacheForTest() {
	a.ldapMu.Lock()
	a.ldapPass = map[string]ldapPassHit{}
	a.ldapMu.Unlock()
}
