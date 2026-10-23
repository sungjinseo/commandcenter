package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// accessFixture is a snapshot with one user per interesting privilege shape.
type accessFixture struct {
	users    []meta.User
	roles    []meta.Role
	settings meta.Settings
}

func newFixture(t *testing.T) *accessFixture {
	t.Helper()
	pw := hash(t, "pw")
	return &accessFixture{
		users: []meta.User{
			{ID: "admin", PasswordHash: pw, Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"admin"}},
			{ID: "adder", PasswordHash: pw, Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"adder"}},
			{ID: "editor", PasswordHash: pw, Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"editor"}},
			{ID: "npmdel", PasswordHash: pw, Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"npmdel"}},
			{ID: "browser", PasswordHash: pw, Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"browser"}},
			{ID: "plain", PasswordHash: pw, Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{}},
			{ID: "off", PasswordHash: pw, Source: meta.SourceLocal, Status: meta.StatusDisabled, Roles: []string{"admin"}},
		},
		roles: []meta.Role{
			meta.BuiltinAdminRole(),
			{ID: "adder", Privileges: []meta.Privilege{{Target: "repo:releases", Actions: []string{meta.ActionRead, meta.ActionAdd}}}},
			{ID: "editor", Privileges: []meta.Privilege{{Target: "*", Actions: []string{meta.ActionEdit}}}},
			{ID: "npmdel", Privileges: []meta.Privilege{{Target: "format:npm", Actions: []string{meta.ActionDelete, meta.ActionRead}}}},
			{ID: "browser", Privileges: []meta.Privilege{{Target: "repo:releases", Actions: []string{meta.ActionBrowse}}}},
		},
		settings: meta.DefaultSettings(),
	}
}

func (f *accessFixture) auth(t *testing.T) *Authenticator {
	t.Helper()
	a := New(&config.Config{}, memTokens{})
	formats := map[string]string{"releases": "maven", "npm-hosted": "npm", "private": "maven"}
	a.RepoFormat = func(name string) string { return formats[name] }
	no := false
	a.AnonymousRead = func(name string) *bool {
		if name == "private" {
			return &no
		}
		return nil
	}
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	return a
}

func lookup(t *testing.T, a *Authenticator, id string) Principal {
	t.Helper()
	p, ok := a.Lookup(id)
	if !ok {
		t.Fatalf("no principal for %q", id)
	}
	return p
}

func TestCanAccessKinds(t *testing.T) {
	f := newFixture(t)
	a := f.auth(t)
	admin, adder, editor, npmdel, browser := lookup(t, a, "admin"), lookup(t, a, "adder"), lookup(t, a, "editor"), lookup(t, a, "npmdel"), lookup(t, a, "browser")
	anon := Principal{}
	cases := []struct {
		p    Principal
		repo string
		acc  Access
		want bool
	}{
		{admin, "private", Delete, true},
		{admin, "private", Edit, true},
		{adder, "releases", Add, true},
		{adder, "releases", Edit, false},
		{adder, "releases", Delete, false},
		{adder, "npm-hosted", Add, false},
		{editor, "releases", Edit, true},
		{editor, "npm-hosted", Edit, true},
		{editor, "releases", Add, false},
		{npmdel, "npm-hosted", Delete, true},
		{npmdel, "releases", Delete, false},
		{browser, "releases", Add, false},
		{anon, "releases", Add, false},
		{anon, "releases", Public, true},
		{anon, "releases", Browse, false}, // anonymousRead is off by default
	}
	for i, c := range cases {
		if got := a.Can(c.p, c.repo, c.acc); got != c.want {
			t.Errorf("case %d: Can(%q, %q, %v) = %v, want %v", i, c.p.User, c.repo, c.acc, got, c.want)
		}
	}
}

func TestAuthenticatedRead(t *testing.T) {
	f := newFixture(t)
	a := f.auth(t)
	plain, adder, npmdel, browser := lookup(t, a, "plain"), lookup(t, a, "adder"), lookup(t, a, "npmdel"), lookup(t, a, "browser")
	// true (the default, today's behaviour): every signed-in user reads everything.
	for _, repo := range []string{"releases", "npm-hosted", "private"} {
		if !a.Can(plain, repo, Read) || !a.Can(plain, repo, Browse) {
			t.Fatalf("authenticatedRead=true: plain cannot read %s", repo)
		}
	}

	f.settings.AuthenticatedRead = false
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	cases := []struct {
		p    Principal
		repo string
		acc  Access
		want bool
	}{
		{plain, "releases", Read, false},
		{plain, "releases", Browse, false},
		{adder, "releases", Read, true},
		{adder, "releases", Browse, true}, // read implies browse
		{adder, "private", Read, false},
		{npmdel, "npm-hosted", Read, true},
		{npmdel, "releases", Read, false},
		{browser, "releases", Browse, true},
		{browser, "releases", Read, false}, // browse alone does not download
	}
	for i, c := range cases {
		if got := a.Can(c.p, c.repo, c.acc); got != c.want {
			t.Errorf("case %d: Can(%q, %q, %v) = %v, want %v", i, c.p.User, c.repo, c.acc, got, c.want)
		}
	}

	// Anonymous read still applies to signed-in users without privileges.
	f.settings.AnonymousRead = true
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	if !a.Can(plain, "releases", Read) || a.Can(plain, "private", Read) || !a.Can(Principal{}, "releases", Read) || a.Can(Principal{}, "private", Read) {
		t.Fatal("anonymousRead with authenticatedRead=false")
	}
}

func TestDisabledUser(t *testing.T) {
	f := newFixture(t)
	a := f.auth(t)
	a.Sessions = NewSessions()
	ctx := context.Background()
	if _, err := a.CheckPassword(ctx, "off", "pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("disabled password login: %v", err)
	}
	r := req("")
	r.SetBasicAuth("off", "pw")
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("disabled Basic auth: %v", err)
	}
	if _, ok := a.Lookup("off"); ok {
		t.Fatal("Lookup returned a disabled user")
	}
	if !a.UserExists("off") {
		t.Fatal("a disabled user still exists")
	}
	if _, _, err := a.CreateToken(ctx, "off"); err == nil {
		t.Fatal("token issued to a disabled user")
	}

	// A token issued while active stops working when the user is disabled,
	// and works again once re-enabled (Decision 1).
	f.users[6].Status = meta.StatusActive
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	_, tok, err := a.CreateToken(ctx, "off")
	if err != nil {
		t.Fatal(err)
	}
	sid := a.Sessions.Create("off")
	cookieReq := httptest.NewRequest("GET", "/", nil)
	cookieReq.AddCookie(&http.Cookie{Name: SessionCookie, Value: sid})
	if p, err := a.Authenticate(req("Bearer " + tok)); err != nil || p.User != "off" {
		t.Fatalf("active token: %+v %v", p, err)
	}
	if p, _ := a.Authenticate(cookieReq); p.User != "off" {
		t.Fatalf("active session: %+v", p)
	}
	f.users[6].Status = meta.StatusDisabled
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	if _, err := a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("disabled token: %v", err)
	}
	if p, _ := a.Authenticate(cookieReq); !p.Anonymous() {
		t.Fatalf("disabled session: %+v", p)
	}
	f.users[6].Status = meta.StatusActive
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	if p, err := a.Authenticate(req("Bearer " + tok)); err != nil || p.User != "off" {
		t.Fatalf("re-enabled token: %+v %v", p, err)
	}
}

func TestRealmsGateLogin(t *testing.T) {
	f := newFixture(t)
	a := f.auth(t)
	ctx := context.Background()
	_, tok, err := a.CreateToken(ctx, "adder")
	if err != nil {
		t.Fatal(err)
	}
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLocal, Enabled: false}, {ID: meta.RealmToken, Enabled: true}}
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	if _, err := a.CheckPassword(ctx, "adder", "pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("local realm off: %v", err)
	}
	if _, err := a.Authenticate(req("Bearer " + tok)); err != nil {
		t.Fatalf("token realm on: %v", err)
	}
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLocal, Enabled: true}}
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	if _, err := a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("token realm missing: %v", err)
	}
	if _, err := a.CheckPassword(ctx, "adder", "pw"); err != nil {
		t.Fatalf("local realm on: %v", err)
	}
}

func TestCheckLockout(t *testing.T) {
	f := newFixture(t)
	if err := CheckLockout(NewSnapshot(f.users, f.roles, f.settings)); err != nil {
		t.Fatalf("fixture has an admin: %v", err)
	}
	cases := map[string]func(f *accessFixture){
		"admin disabled":  func(f *accessFixture) { f.users[0].Status = meta.StatusDisabled },
		"admin deleted":   func(f *accessFixture) { f.users = f.users[1:] },
		"admin demoted":   func(f *accessFixture) { f.users[0].Roles = []string{"adder"} },
		"no password":     func(f *accessFixture) { f.users[0].PasswordHash = "" },
		"ldap admin only": func(f *accessFixture) { f.users[0].Source = meta.SourceLDAP },
		"local realm off": func(f *accessFixture) {
			f.settings.Realms = []meta.Realm{{ID: meta.RealmLocal, Enabled: false}, {ID: meta.RealmToken, Enabled: true}}
		},
		"local realm missing": func(f *accessFixture) {
			f.settings.Realms = []meta.Realm{{ID: meta.RealmToken, Enabled: true}, {ID: meta.RealmLDAP, Enabled: true}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(f)
			if err := CheckLockout(NewSnapshot(f.users, f.roles, f.settings)); !errors.Is(err, ErrLockout) {
				t.Fatalf("CheckLockout = %v, want ErrLockout", err)
			}
		})
	}
	// A second enabled local admin keeps the system reachable.
	f = newFixture(t)
	f.users[0].Status = meta.StatusDisabled
	f.users[1].Roles = append(f.users[1].Roles, "admin")
	if err := CheckLockout(NewSnapshot(f.users, f.roles, f.settings)); err != nil {
		t.Fatalf("second admin: %v", err)
	}
}

func TestOnUserChangedInvalidates(t *testing.T) {
	f := newFixture(t)
	a := f.auth(t)
	a.Sessions = NewSessions()
	ctx := context.Background()
	sid := a.Sessions.Create("adder")
	other := a.Sessions.Create("plain")
	if _, err := a.CheckPassword(ctx, "adder", "pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CheckPassword(ctx, "plain", "pw"); err != nil {
		t.Fatal(err)
	}
	if a.cachedCount("adder") != 1 || a.cachedCount("plain") != 1 {
		t.Fatal("password checks were not cached")
	}
	a.OnUserChanged("adder")
	if _, ok := a.Sessions.User(sid); ok {
		t.Fatal("session survived OnUserChanged")
	}
	if _, ok := a.Sessions.User(other); !ok {
		t.Fatal("another user's session was ended")
	}
	if a.cachedCount("adder") != 0 || a.cachedCount("plain") != 1 {
		t.Fatal("password cache not cleared for exactly that user")
	}
}

func TestPasswordChangeDefeatsTheCache(t *testing.T) {
	// Even before OnUserChanged runs, a cached success never outlives the hash
	// it was checked against.
	f := newFixture(t)
	a := f.auth(t)
	ctx := context.Background()
	if _, err := a.CheckPassword(ctx, "adder", "pw"); err != nil {
		t.Fatal(err)
	}
	f.users[1].PasswordHash = hash(t, "new-password")
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	if _, err := a.CheckPassword(ctx, "adder", "pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("old password still accepted: %v", err)
	}
	if _, err := a.CheckPassword(ctx, "adder", "new-password"); err != nil {
		t.Fatal(err)
	}
}

func TestRoleChangesApplyLive(t *testing.T) {
	f := newFixture(t)
	a := f.auth(t)
	adder := lookup(t, a, "adder")
	if a.Can(adder, "releases", Delete) {
		t.Fatal("delete before the change")
	}
	f.roles[1].Privileges[0].Actions = append(f.roles[1].Privileges[0].Actions, meta.ActionDelete)
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	if !a.Can(adder, "releases", Delete) {
		t.Fatal("a role change did not apply to an existing principal")
	}
}

func TestReloadFromDatabase(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateRole(ctx, meta.BuiltinAdminRole()); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, meta.User{ID: "root", PasswordHash: hash(t, "pw"), Roles: []string{"admin"}}); err != nil {
		t.Fatal(err)
	}
	a, err := Open(ctx, db, db)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := a.CheckPassword(ctx, "root", "pw"); err != nil || !p.Admin {
		t.Fatalf("root: %+v %v", p, err)
	}
	if _, ok := a.Lookup("later"); ok {
		t.Fatal("unknown user")
	}
	if err := db.CreateUser(ctx, meta.User{ID: "later", PasswordHash: hash(t, "pw")}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSettingJSON(ctx, meta.SettingAuthenticatedRead, false); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	later := lookup(t, a, "later")
	if a.Can(later, "x", Read) {
		t.Fatal("authenticatedRead=false was not reloaded")
	}
	if a.Snapshot().Settings().AuthenticatedRead {
		t.Fatal("snapshot settings not reloaded")
	}
	if err := CheckLockout(a.Snapshot()); err != nil {
		t.Fatalf("lockout on the reloaded snapshot: %v", err)
	}
}

func TestReloadUnderConcurrency(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	pw := hash(t, "pw")
	if err := db.CreateRole(ctx, meta.BuiltinAdminRole()); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRole(ctx, meta.Role{ID: "w", Privileges: []meta.Privilege{{Target: "*", Actions: []string{meta.ActionAdd}}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, meta.User{ID: "u", PasswordHash: pw, Roles: []string{"w"}}); err != nil {
		t.Fatal(err)
	}
	a, err := Open(ctx, db, db)
	if err != nil {
		t.Fatal(err)
	}
	a.Sessions = NewSessions()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if p, ok := a.Lookup("u"); ok {
					a.Can(p, "r", Add)
					a.Can(p, "r", Read)
				}
				a.CheckPassword(ctx, "u", "pw")
				a.Snapshot().Users()
			}
		}()
	}
	for i := 0; i < 21; i++ { // ends on an even i: active
		status := meta.StatusActive
		if i%2 == 1 {
			status = meta.StatusDisabled
		}
		if err := db.UpdateUser(ctx, meta.User{ID: "u", PasswordHash: pw, Status: status, Roles: []string{"w"}}); err != nil {
			t.Fatal(err)
		}
		if err := a.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		a.OnUserChanged("u")
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if _, ok := a.Lookup("u"); !ok { // the last write left u active
		t.Fatal("final snapshot not published")
	}
}

// cachedCount returns the number of cached password successes of user.
func (a *Authenticator) cachedCount(user string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for k := range a.cache {
		if strings.HasPrefix(k, user+"\x00") {
			n++
		}
	}
	return n
}

func TestBasicTokenOfDisabledUser(t *testing.T) {
	f := newFixture(t)
	f.users[6].Status = meta.StatusActive
	a := f.auth(t)
	_, tok, err := a.CreateToken(context.Background(), "off")
	if err != nil {
		t.Fatal(err)
	}
	r := req("")
	r.SetBasicAuth("off", tok)
	if p, err := a.Authenticate(r); err != nil || p.User != "off" {
		t.Fatalf("active Basic token: %+v %v", p, err)
	}
	f.users[6].Status = meta.StatusDisabled
	a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings))
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("disabled Basic token: %v", err)
	}
}

func TestCreateTokenDisabledIsTyped(t *testing.T) {
	a := newFixture(t).auth(t)
	if _, _, err := a.CreateToken(context.Background(), "off"); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("err = %v, want ErrUserDisabled", err)
	}
}

func TestSnapshotAccessorsHideHashes(t *testing.T) {
	s := NewSnapshot(newFixture(t).users, nil, meta.DefaultSettings())
	for _, u := range s.Users() {
		if u.PasswordHash != "" {
			t.Fatalf("Users() leaks the hash of %s", u.ID)
		}
	}
	if u, _ := s.User("admin"); u.PasswordHash != "" {
		t.Fatal("User() leaks the hash")
	}
	if err := CheckLockout(s); err != nil {
		t.Fatalf("lockout still sees the hashes internally: %v", err)
	}
}
