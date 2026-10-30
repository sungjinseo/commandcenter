package auth

import (
	"testing"

	"commandcenter/internal/meta"
)

// Nested roles grant transitively and survive a cycle; a role containing
// admin makes an admin, but never as a default role; application and
// repository-admin privileges answer CanApp and CanAdminRepo; a selector
// scope "*-<format>" covers that format's repositories (security-06, -07,
// -09, -25).
func TestNestedRolesAndDelegation(t *testing.T) {
	settings := meta.DefaultSettings()
	settings.AnonymousRead, settings.AuthenticatedRead = false, false
	settings.DefaultRoles = []string{"sneaky", "base"}
	roles := []meta.Role{
		{ID: "base", Privileges: []meta.Privilege{{Target: "repo:releases", Actions: []string{"read"}}}},
		{ID: "mid", Roles: []string{"base"}, Privileges: []meta.Privilege{{Target: "app:users", Actions: []string{"read"}}}},
		{ID: "top", Roles: []string{"mid", "missing"}},
		{ID: "loop-a", Roles: []string{"loop-b"}, Privileges: []meta.Privilege{{Target: "format:npm", Actions: []string{"add"}}}},
		{ID: "loop-b", Roles: []string{"loop-a"}, Privileges: []meta.Privilege{{Target: "app:tasks", Actions: []string{"run"}}}},
		{ID: "boss", Roles: []string{"admin"}},
		{ID: "sneaky", Roles: []string{"boss"}},
		{ID: "mvn-admin", Privileges: []meta.Privilege{{Target: "admin:format:maven", Actions: []string{"read", "add"}},
			{Target: "admin:repo:releases", Actions: []string{"edit"}}}},
		{ID: "all-maven", Privileges: []meta.Privilege{{Target: "selector:any", Actions: []string{"read"}, Repos: []string{"*-maven2"}}}},
	}
	a := &Authenticator{}
	s := NewSnapshotWith([]meta.User{{ID: "u", Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"top"}},
		{ID: "n", Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"boss"}, PasswordHash: string(dummyHash)}}, roles, settings,
		[]meta.ContentSelector{{Name: "any", Expression: `path =^ "/"`}})
	a.setSnapshot(s)
	a.RepoFormat = func(name string) string {
		return map[string]string{"releases": "maven", "snaps": "maven", "web": "npm"}[name]
	}
	user := func(roles ...string) Principal { return s.principal(meta.User{ID: "x", Roles: roles}) }

	if !a.Can(user("top"), "releases", Read) || a.Can(user("top"), "releases", Add) {
		t.Error("top does not grant exactly base's read through mid")
	}
	if !a.CanApp(user("top"), "users", "read") || a.CanApp(user("top"), "users", "edit") || a.CanApp(user("base"), "users", "read") {
		t.Error("app:users read is not granted exactly through mid")
	}
	if !a.Can(user("loop-a"), "web", Add) || !a.CanApp(user("loop-a"), "tasks", "run") || !a.CanApp(user("loop-b"), "tasks", "run") || !a.Can(user("loop-b"), "web", Add) {
		t.Error("a cycle does not grant both roles' privileges")
	}
	if !user("boss").Admin || !user("sneaky").Admin || user("top").Admin {
		t.Error("admin by nesting")
	}
	// Default roles: base applies to everyone, sneaky (admin by nesting) to nobody.
	if p := user(); p.Admin || !a.Can(p, "releases", Read) {
		t.Errorf("default roles: admin %v, base read %v", p.Admin, a.Can(p, "releases", Read))
	}
	// Repository admin: format add and read, one repository's edit.
	p := user("mvn-admin")
	for _, c := range []struct {
		repo, format string
		acc          Access
		want         bool
	}{
		{"", "maven", Add, true}, {"", "npm", Add, false}, {"snaps", "maven", Read, true}, {"snaps", "maven", Browse, true},
		{"releases", "maven", Edit, true}, {"snaps", "maven", Edit, false}, {"releases", "maven", Delete, false},
		{"web", "npm", Read, false},
	} {
		if got := a.CanAdminRepo(p, c.repo, c.format, c.acc); got != c.want {
			t.Errorf("CanAdminRepo(%q, %q, %v) = %v", c.repo, c.format, c.acc, got)
		}
	}
	if !a.AdminsSomeRepo(p) || a.AdminsSomeRepo(user("top")) {
		t.Error("AdminsSomeRepo")
	}
	if a.CanAdminRepo(Principal{}, "releases", "maven", Read) || !a.CanAdminRepo(user("boss"), "web", "npm", Delete) {
		t.Error("anonymous or admin")
	}
	// "*-maven2": every maven repository, no other format.
	if !a.CanPath(user("all-maven"), "snaps", Read, "x") || a.CanPath(user("all-maven"), "web", Read, "x") {
		t.Error("selector scope *-maven2")
	}
	// Lockout counts an admin by nesting (n holds boss, which contains admin).
	if CheckLockout(s) != nil || RestorableAdmins(s) != 1 {
		t.Errorf("lockout %v, restorable %d", CheckLockout(s), RestorableAdmins(s))
	}
	if !s.ContainsCycle("base", []string{"top"}) || s.ContainsCycle("top", []string{"loop-a"}) || !s.ContainsCycle("loop-a", []string{"loop-b"}) {
		t.Error("ContainsCycle")
	}
	if got := a.AppActions(user("top")); len(got) != 1 || len(got["users"]) != 1 {
		t.Errorf("AppActions = %v", got)
	}
}
