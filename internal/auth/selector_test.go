package auth

import (
	"context"
	"testing"

	"commandcenter/internal/meta"
)

// A "selector:<name>" privilege grants its actions on the paths the
// selector matches, in the repositories it names; Can (repository level)
// says yes whenever the repository is in scope, CanPath checks the path.
func TestSelectorPrivileges(t *testing.T) {
	settings := meta.DefaultSettings()
	settings.AnonymousRead, settings.AuthenticatedRead = false, false
	roles := []meta.Role{
		{ID: "acme", Privileges: []meta.Privilege{
			{Target: "selector:acme", Actions: []string{"read", "add"}, Repos: []string{"releases"}},
		}},
		{ID: "any", Privileges: []meta.Privilege{
			{Target: "selector:mvn", Actions: []string{"read"}, Repos: []string{"*"}},
		}},
		{ID: "gone", Privileges: []meta.Privilege{
			{Target: "selector:missing", Actions: []string{"read"}, Repos: []string{"*"}},
		}},
	}
	selectors := []meta.ContentSelector{
		{Name: "acme", Expression: `path =^ "/org/acme/"`},
		{Name: "mvn", Expression: `format == "maven2"`},
	}
	a := &Authenticator{}
	a.setSnapshot(NewSnapshotWith(nil, roles, settings, selectors))
	a.RepoFormat = func(name string) string {
		return map[string]string{"releases": "maven", "other": "maven", "npm": "npm"}[name]
	}
	user := func(roles ...string) Principal { return Principal{User: "u", roles: roles} }
	for _, c := range []struct {
		p          Principal
		repo, path string
		acc        Access
		can, path_ bool
	}{
		{user("acme"), "releases", "org/acme/lib/1.0/lib.jar", Read, true, true},
		{user("acme"), "releases", "org/acme/lib/1.0/lib.jar", Add, true, true},
		{user("acme"), "releases", "org/acme/lib/1.0/lib.jar", Delete, false, false},
		{user("acme"), "releases", "org/other/lib.jar", Read, true, false},
		{user("acme"), "releases", "", Read, true, false}, // the root: only a selector matching "/" grants it
		{user("acme"), "other", "org/acme/lib.jar", Read, false, false},
		{user("any"), "other", "anything", Read, true, true},
		{user("any"), "npm", "lodash", Read, true, false}, // in scope, but the format term fails
		{user("gone"), "releases", "x", Read, false, false},
		{user("acme"), "releases", "org/acme/x", Browse, true, true}, // read implies browse
	} {
		if got := a.Can(c.p, c.repo, c.acc); got != c.can {
			t.Errorf("Can(%v, %s, %v) = %v, want %v", c.p.roles, c.repo, c.acc, got, c.can)
		}
		if got := a.CanPath(c.p, c.repo, c.acc, c.path); got != c.path_ {
			t.Errorf("CanPath(%v, %s, %v, %q) = %v, want %v", c.p.roles, c.repo, c.acc, c.path, got, c.path_)
		}
	}
	// Permits follows the path the server recorded.
	ctx := WithPrincipal(context.Background(), user("acme"))
	if !a.Permits(ctx, "releases", Add) || !a.Permits(WithPath(ctx, "org/acme/x"), "releases", Add) || a.Permits(WithPath(ctx, "com/x"), "releases", Add) {
		t.Fatal("Permits ignores the context path")
	}
	// Admin and the plain targets are unchanged.
	if !a.Can(Principal{User: "root", Admin: true}, "releases", Delete) || !a.CanPath(Principal{User: "root", Admin: true}, "releases", Delete, "x") {
		t.Fatal("admin")
	}
}
