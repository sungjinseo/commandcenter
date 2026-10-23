package ui

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
)

// I3 (final review): two concurrent "disable the other admin" requests must
// not both pass the lockout check. accessChecked runs after a request's
// check and before its write; the first request is held there while the
// second runs. With the check and the write serialised, the second never
// reaches the hook and is refused once the first has written.
func TestConcurrentAdminDisablesKeepOneAdmin(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"admin2","password":"longenough","roles":["admin"]}`), http.StatusCreated)

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	e.ui.accessChecked = func() {
		arrived <- struct{}{}
		<-release
	}
	results := make(chan int, 2)
	go func() {
		results <- e.basicAs(t, "admin", "pw", "PUT", "/-/api/users/admin2", `{"roles":["admin"],"status":"disabled"}`).status
	}()
	go func() {
		results <- e.basicAs(t, "admin2", "longenough", "PUT", "/-/api/users/admin", `{"roles":["admin"],"status":"disabled"}`).status
	}()
	<-arrived // one request passed its check and waits before writing
	select {
	case <-arrived: // the other one passed its check too: the race is open
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	got := map[int]int{}
	for range 2 {
		got[<-results]++
	}
	e.ui.accessChecked = nil
	if got[http.StatusOK] != 1 || got[http.StatusConflict] != 1 {
		t.Fatalf("statuses = %v, want one 200 and one 409", got)
	}
	data, err := e.db.LoadAccess(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n := auth.RestorableAdmins(auth.NewSnapshot(data.Users, data.Roles, data.Settings)); n < 1 {
		t.Fatalf("restorable admins = %d", n)
	}
}

// M2 (final review): a role named by the LDAP settings (a group mapping or
// the default roles) cannot be deleted; the 409 names the reference and
// still matches the UI's /in use/.
func TestRoleDeleteRefusedWhileLDAPReferencesIt(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	ctx := context.Background()
	for _, id := range []string{"devs", "base", "free"} {
		wantStatus(t, e.admin(t, "POST", "/-/api/roles", `{"id":"`+id+`"}`), http.StatusCreated)
	}
	set, err := e.db.LoadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l := set.LDAP
	l.GroupMappings = []meta.GroupMapping{{Group: "cn=devs,ou=groups", Roles: []string{"devs"}}}
	l.DefaultRoles = []string{"base"}
	if err := e.db.SetSettingJSON(ctx, meta.SettingLDAP, l); err != nil {
		t.Fatal(err)
	}
	r := e.admin(t, "DELETE", "/-/api/roles/devs", "")
	wantStatus(t, r, http.StatusConflict)
	msg, _ := r.json(t)["error"].(string)
	if !strings.Contains(msg, "LDAP group mapping") || !strings.Contains(msg, "cn=devs,ou=groups") || !regexp.MustCompile("in use").MatchString(msg) {
		t.Fatalf("group mapping refusal: %s", r.body)
	}
	r = e.admin(t, "DELETE", "/-/api/roles/base", "")
	wantStatus(t, r, http.StatusConflict)
	if msg, _ := r.json(t)["error"].(string); !strings.Contains(msg, "LDAP default roles") {
		t.Fatalf("default roles refusal: %s", r.body)
	}
	wantStatus(t, e.admin(t, "GET", "/-/api/roles/devs", ""), http.StatusOK)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/roles/free", ""), http.StatusNoContent)
}
