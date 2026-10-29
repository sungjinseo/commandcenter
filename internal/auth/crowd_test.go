package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"commandcenter/internal/auth/crowdtest"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

type crowdFixture struct {
	*accessFixture
	srv   *crowdtest.Server
	a     *Authenticator
	tok   memTokens
	clock time.Time
}

// newCrowdFixture serves alice (cc-admins), bob (devs) and carol
// (inactive) from a fake Crowd; realms are local, token, crowd.
func newCrowdFixture(t *testing.T) *crowdFixture {
	t.Helper()
	s := crowdtest.New(t)
	s.Put("alice", crowdtest.User{Password: "alice-pw", Email: "alice@example.org", DisplayName: "Alice", Groups: []string{"cc-admins", "devs"}})
	s.Put("bob", crowdtest.User{Password: "bob-pw", Groups: []string{"devs"}})
	s.Put("carol", crowdtest.User{Password: "carol-pw", Inactive: true})
	f := &crowdFixture{accessFixture: newFixture(t), srv: s, tok: memTokens{}, clock: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	c := meta.DefaultCrowd()
	c.URL, c.ApplicationName, c.ApplicationPassword = s.URL, s.App, s.AppPassword
	c.TimeoutSeconds = 2
	c.GroupMappings = []meta.GroupMapping{{Group: "cc-admins", Roles: []string{"admin"}}, {Group: "DEVS", Roles: []string{"adder"}}}
	c.DefaultRoles = []string{"browser"}
	f.settings.Crowd = c
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLocal, Enabled: true}, {ID: meta.RealmToken, Enabled: true}, {ID: meta.RealmCrowd, Enabled: true}}
	f.a = New(&config.Config{}, f.tok)
	f.a.now = func() time.Time { return f.clock }
	f.apply()
	return f
}

func (f *crowdFixture) apply() { f.a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings)) }

func TestCrowdLogin(t *testing.T) {
	f := newCrowdFixture(t)
	ctx := context.Background()
	p, err := f.a.CheckPassword(ctx, "alice", "alice-pw")
	if err != nil || p.User != "alice" || !p.IsCrowd() || !p.Admin || p.Owner() != "crowd:alice" || p.Via != ViaPassword {
		t.Fatalf("alice %+v %v", p, err)
	}
	bob, err := f.a.CheckPassword(ctx, "bob", "bob-pw")
	if err != nil || bob.Admin || len(bob.Roles()) != 2 || bob.Roles()[0] != "adder" || bob.Roles()[1] != "browser" {
		t.Fatalf("bob %+v %v", bob, err) // the group mapping (case-insensitive) and the default roles
	}
	if !f.a.Can(bob, "releases", Add) {
		t.Fatal("bob's mapped role does not apply")
	}
	for _, c := range []struct{ user, pw string }{{"alice", "wrong"}, {"carol", "carol-pw"}, {"dave", "x"}, {"alice", ""}} {
		if _, err := f.a.CheckPassword(ctx, c.user, c.pw); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("%s/%s: %v", c.user, c.pw, err)
		}
	}
	if got, ok := f.a.Lookup("crowd:alice"); !ok || !got.Admin || !got.IsCrowd() {
		t.Fatalf("lookup %+v %v", got, ok)
	}
	if _, ok := f.a.Lookup("alice"); ok {
		t.Fatal("a crowd user found as a local one")
	}
	us := f.a.CrowdUsers()
	if len(us) != 2 || us[0].ID != "alice" || us[0].Name != "Alice" || us[0].Email != "alice@example.org" || us[1].ID != "bob" {
		t.Fatalf("users %+v", us)
	}
	// An override row adds roles, and denies the user when disabled.
	f.users = append(f.users, meta.User{ID: "bob", Source: meta.SourceCrowd, Status: meta.StatusActive, Roles: []string{"editor"}})
	f.apply()
	if got, _ := f.a.Lookup("crowd:bob"); !hasRole(got.Roles(), "editor") {
		t.Fatalf("override roles: %+v", got)
	}
	f.users[len(f.users)-1].Status = meta.StatusDisabled
	f.apply()
	if _, err := f.a.CheckPassword(ctx, "bob", "bob-pw"); err == nil {
		t.Fatal("a disabled override row lets bob in")
	}
	// The realm off: nothing reaches Crowd.
	n := f.srv.Calls("authentication")
	f.settings.Realms = f.settings.Realms[:2]
	f.apply()
	if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err == nil || f.srv.Calls("authentication") != n {
		t.Fatalf("realm off: %v, %d calls", err, f.srv.Calls("authentication")-n)
	}
}

func TestCrowdCache(t *testing.T) {
	f := newCrowdFixture(t)
	ctx := context.Background()
	login := func(pw string) error { _, err := f.a.CheckPassword(ctx, "alice", pw); return err }
	if err := login("alice-pw"); err != nil {
		t.Fatal(err)
	}
	if err := login("alice-pw"); err != nil || f.srv.Calls("authentication") != 1 {
		t.Fatalf("second login within cacheMinutes: %v, %d calls", err, f.srv.Calls("authentication"))
	}
	if _, ok := f.a.Lookup("crowd:alice"); !ok || f.srv.Calls("user") != 0 {
		t.Fatalf("lookup within cacheMinutes re-read the user: %d", f.srv.Calls("user"))
	}
	f.clock = f.clock.Add(time.Minute)
	if err := login("alice-pw"); err != nil || f.srv.Calls("authentication") != 2 {
		t.Fatalf("after cacheMinutes: %v, %d calls", err, f.srv.Calls("authentication"))
	}
	// A password changed in Crowd: the new one goes to Crowd and drops the
	// old one's cached success.
	f.srv.Put("alice", crowdtest.User{Password: "new-pw", Groups: []string{"cc-admins"}})
	if err := login("new-pw"); err != nil || f.srv.Calls("authentication") != 3 {
		t.Fatalf("new password: %v, %d calls", err, f.srv.Calls("authentication"))
	}
	if err := login("alice-pw"); err == nil || f.srv.Calls("authentication") != 4 {
		t.Fatalf("old password: %v, %d calls", err, f.srv.Calls("authentication"))
	}
	// A configuration change drops the cache.
	f.settings.Crowd.CacheMinutes = 5
	f.apply()
	if err := login("new-pw"); err != nil || f.srv.Calls("authentication") != 5 {
		t.Fatalf("after a configuration change: %v, %d calls", err, f.srv.Calls("authentication"))
	}
	f.clock = f.clock.Add(4 * time.Minute)
	if err := login("new-pw"); err != nil || f.srv.Calls("authentication") != 5 {
		t.Fatalf("within 5 cacheMinutes: %v, %d calls", err, f.srv.Calls("authentication"))
	}
}

func TestCrowdFailClosed(t *testing.T) {
	f := newCrowdFixture(t)
	ctx := context.Background()
	p, err := f.a.CheckPassword(ctx, "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.a.CreateTokenFor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.a.checkToken(ctx, tok, ""); err != nil || got.Owner() != "crowd:alice" || f.tok[hashToken(tok)] != "crowd:alice" {
		t.Fatalf("token %+v %v", got, err)
	}
	// Past cacheMinutes the user is re-read from Crowd: deactivated there,
	// it is denied.
	f.clock = f.clock.Add(time.Minute)
	f.srv.Put("alice", crowdtest.User{Password: "alice-pw", Inactive: true})
	if _, err := f.a.checkToken(ctx, tok, ""); err == nil || f.srv.Calls("user") != 1 {
		t.Fatalf("inactive user's token: %v, %d user calls", err, f.srv.Calls("user"))
	}
	f.srv.Put("alice", crowdtest.User{Password: "alice-pw", Groups: []string{"devs"}})
	if got, err := f.a.checkToken(ctx, tok, ""); err != nil || got.Admin {
		t.Fatalf("active again, re-read groups: %+v %v", got, err)
	}
	// Crowd down: sign-in and the re-read fail closed, and a failed re-read
	// is not retried for crowdRetry.
	f.srv.Close()
	if _, err := f.a.CheckPassword(ctx, "bob", "bob-pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("sign-in with Crowd down: %v", err)
	}
	f.clock = f.clock.Add(time.Minute)
	if _, ok := f.a.Lookup("crowd:alice"); ok {
		t.Fatal("lookup with Crowd down")
	}
	f.a.crowdMu.Lock()
	failedAt := f.a.crowdUsers["alice"].failedAt
	f.a.crowdMu.Unlock()
	if failedAt.IsZero() {
		t.Fatal("no back-off after the failed re-read")
	}
	if err := f.a.CrowdPing(ctx, f.settings.Crowd); err == nil {
		t.Fatal("ping with Crowd down")
	}
}

func TestCrowdPing(t *testing.T) {
	f := newCrowdFixture(t)
	ctx := context.Background()
	if err := f.a.CrowdPing(ctx, f.settings.Crowd); err != nil {
		t.Fatal(err)
	}
	c := f.settings.Crowd
	c.ApplicationPassword = "wrong"
	var ce *CrowdError
	if err := f.a.CrowdPing(ctx, c); !errors.As(err, &ce) || ce.Status != http.StatusUnauthorized || ce.Reason != "APPLICATION_ACCESS_DENIED" {
		t.Fatalf("wrong application password: %v", err)
	}
	// The wrong application password fails sign-in, as an outage would.
	f.settings.Crowd = c
	f.apply()
	if _, err := f.a.CheckPassword(ctx, "alice", "alice-pw"); err == nil {
		t.Fatal("sign-in with a wrong application password")
	}
	u, err := f.a.CrowdFind(ctx, f.settings.Crowd, "alice")
	if err == nil {
		t.Fatalf("find with a wrong application password: %+v", u)
	}
}

func TestCrowdLockout(t *testing.T) {
	f := newCrowdFixture(t)
	f.settings.Realms = []meta.Realm{{ID: meta.RealmCrowd, Enabled: true}}
	if err := CheckLockout(NewSnapshot(f.users, f.roles, f.settings)); err != nil {
		t.Fatalf("crowd granting admin: %v", err)
	}
	f.settings.Crowd.GroupMappings = nil
	if err := CheckLockout(NewSnapshot(f.users, f.roles, f.settings)); !errors.Is(err, ErrLockout) {
		t.Fatalf("crowd without admin: %v", err)
	}
}
