package repotest

import (
	"context"
	"testing"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
)

// Principals stores one role and one user per entry of grants (user id ->
// actions on the repository named target) in db, opens an Authenticator over
// db and returns it with each user's principal.
func Principals(t testing.TB, db *meta.DB, target string, grants map[string][]string) (*auth.Authenticator, map[string]auth.Principal) {
	t.Helper()
	ctx := context.Background()
	for user, actions := range grants {
		role := meta.Role{ID: "role-" + user, Privileges: []meta.Privilege{{Target: "repo:" + target, Actions: actions}}}
		if err := db.CreateRole(ctx, role); err != nil {
			t.Fatal(err)
		}
		if err := db.CreateUser(ctx, meta.User{ID: user, PasswordHash: "x", Roles: []string{role.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.Open(ctx, db, db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]auth.Principal{}
	for user := range grants {
		p, ok := a.Lookup(user)
		if !ok {
			t.Fatalf("no principal for %q", user)
		}
		out[user] = p
	}
	return a, out
}
