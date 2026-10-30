package ui

import (
	"net/http"
	"strings"
	"testing"
)

// Nested roles through the role API: unknown, self and cyclic contained
// roles are 400; a contained role cannot be deleted (409); a role that
// contains admin makes its holders admins, cannot become a default role,
// and the lockout check follows it (security-09).
func TestNestedRolesAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", "/-/api/roles", `{"id":"base","name":"Base","privileges":[{"target":"repo:maven-releases","actions":["read"]}]}`), http.StatusCreated)
	r := e.admin(t, "POST", "/-/api/roles", `{"id":"top","privileges":[],"roles":["base"]}`)
	wantStatus(t, r, http.StatusCreated)
	if m := r.json(t); m["roles"].([]any)[0] != "base" {
		t.Fatalf("top = %s", r.body)
	}
	for _, body := range []string{
		`{"id":"x","privileges":[],"roles":["nope"]}`,
		`{"id":"x","privileges":[],"roles":["base","base"]}`,
		`{"id":"x","privileges":[],"roles":["x"]}`,
	} {
		wantStatus(t, e.admin(t, "POST", "/-/api/roles", body), http.StatusBadRequest)
	}
	r = e.admin(t, "PUT", "/-/api/roles/base", `{"name":"Base","privileges":[],"roles":["top"]}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "cannot contain itself") {
		t.Fatalf("cycle: %s", r.body)
	}
	r = e.admin(t, "DELETE", "/-/api/roles/base", "")
	wantStatus(t, r, http.StatusConflict)
	if r.json(t)["error"] != `role is in use by role "top"` {
		t.Fatalf("contained: %s", r.body)
	}
	// boss contains admin: its holder is an admin, it cannot be a default
	// role, and dropping admin from it while it is the only admin path is
	// a lockout.
	wantStatus(t, e.admin(t, "POST", "/-/api/roles", `{"id":"boss","privileges":[],"roles":["admin"]}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/reader", `{"roles":["boss"]}`), http.StatusOK)
	if m := e.req(t, "GET", "/-/api/me", "", "reader", nil, true).json(t); m["admin"] != true {
		t.Fatalf("reader me = %v", m)
	}
	r = e.admin(t, "PUT", "/-/api/settings", `{"defaultRoles":["boss"]}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "contains the admin role") {
		t.Fatalf("default role: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"defaultRoles":["top"]}`), http.StatusOK)
	r = e.admin(t, "PUT", "/-/api/roles/top", `{"privileges":[],"roles":["boss"]}`)
	wantStatus(t, r, http.StatusBadRequest)                                                                                       // top is a default role
	wantStatus(t, e.req(t, "PUT", "/-/api/users/admin", `{"roles":[]}`, "reader", nil, true), http.StatusOK)                      // reader is the admin now
	wantStatus(t, e.req(t, "PUT", "/-/api/roles/boss", `{"privileges":[],"roles":[]}`, "reader", nil, true), http.StatusConflict) // the last admin path
	// Self-delete: refused once another admin exists (else the lockout says so).
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/admin", `{"roles":["admin"]}`), http.StatusForbidden) // admin no longer is one
	wantStatus(t, e.req(t, "PUT", "/-/api/users/admin", `{"roles":["admin"]}`, "reader", nil, true), http.StatusOK)
	r = e.req(t, "DELETE", "/-/api/users/reader", "", "reader", nil, true)
	wantStatus(t, r, http.StatusConflict)
	if r.json(t)["error"] != selfDeleteMessage {
		t.Fatalf("self delete: %s", r.body)
	}
}

// Application and repository-admin targets in the role API: domains and
// actions are checked, "add" on one repository and settings changes are
// refused with a message; "*-<format>" selector scopes are accepted.
func TestDelegatedTargetsValidation(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", "/-/api/content-selectors", `{"name":"all","expression":"path =^ \"/\""}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/roles", `{"id":"ok","privileges":[`+
		`{"target":"app:users","actions":["read","add"]},{"target":"app:tasks","actions":["run"]},`+
		`{"target":"admin:*","actions":["browse","read"]},{"target":"admin:format:maven","actions":["add"]},`+
		`{"target":"admin:repo:maven-releases","actions":["edit","delete"]},`+
		`{"target":"selector:all","actions":["read"],"repos":["*-maven2"]}]}`), http.StatusCreated)
	for body, want := range map[string]string{
		`{"target":"app:settings","actions":["edit"]}`:                    "delegates only read",
		`{"target":"app:capabilities","actions":["read"]}`:                "unknown application domain",
		`{"target":"app:users","actions":["browse"]}`:                     "unknown action",
		`{"target":"repo:maven-releases","actions":["run"]}`:              "unknown action",
		`{"target":"admin:repo:maven-releases","actions":["add"]}`:        "creates repositories",
		`{"target":"admin:repo:nope","actions":["read"]}`:                 "unknown repository",
		`{"target":"admin:metrics","actions":["read"]}`:                   "must be",
		`{"target":"selector:all","actions":["read"],"repos":["*-nope"]}`: "unknown format",
	} {
		r := e.admin(t, "POST", "/-/api/roles", `{"id":"bad","privileges":[`+body+`]}`)
		wantStatus(t, r, http.StatusBadRequest)
		if !strings.Contains(r.body, want) {
			t.Errorf("%s: %s, want %q", body, r.body, want)
		}
	}
}

// A repository administrator reads the lists its forms pick from, and
// nothing else of the admin API (security-06).
func TestRepoAdminPickers(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", "/-/api/roles", `{"id":"mvn","privileges":[{"target":"admin:format:maven","actions":["read","add"]}]}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/reader", `{"roles":["mvn"]}`), http.StatusOK)
	for _, p := range []string{"/-/api/blobstores", "/-/api/cleanup/policies", "/-/api/routing-rules"} {
		wantStatus(t, e.get(t, p, "reader"), http.StatusOK)
		wantStatus(t, e.get(t, p, "ci"), http.StatusForbidden)
	}
	wantStatus(t, e.get(t, "/-/api/blobstores/default", "reader"), http.StatusForbidden)
	wantStatus(t, e.get(t, "/-/api/tasks", "reader"), http.StatusForbidden)
	if m := e.req(t, "GET", "/-/api/me", "", "reader", nil, true).json(t); m["repositoryAdmin"] != true {
		t.Fatalf("me = %v", m)
	}
}

// The password policy setting: an RE2 pattern the whole new password must
// match, with its own message, on create, reset and self-change
// (security-18).
func TestPasswordPolicy(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"passwordPolicy":{"pattern":"(","message":"x"}}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"passwordPolicy":{"pattern":".*[0-9].*","message":"use a digit"}}`), http.StatusOK)
	if m := e.admin(t, "GET", "/-/api/settings", "").json(t); m["passwordPolicy"].(map[string]any)["message"] != "use a digit" {
		t.Fatalf("settings = %v", m)
	}
	r := e.admin(t, "POST", "/-/api/users", `{"id":"p1","password":"abcdefgh"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if r.json(t)["error"] != "use a digit" {
		t.Fatalf("create: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"p1","password":"abcdefg1"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/p1/password", `{"password":"nodigits"}`), http.StatusBadRequest)
	wantStatus(t, e.req(t, "PUT", "/-/api/account/password", `{"currentPassword":"pw","newPassword":"stillnodigits"}`, "ci", nil, true), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"passwordPolicy":{"pattern":"","message":""}}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/p1/password", `{"password":"nodigits"}`), http.StatusNoContent)
}
