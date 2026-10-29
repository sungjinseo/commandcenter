package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

// asUser sends a request as user (basic auth, password "pw") with the UI header.
func (e *env) asUser(t *testing.T, user, method, path, body string) resp {
	t.Helper()
	return e.req(t, method, path, body, user, nil, true)
}

func (e *env) basicWith(t *testing.T, user, pass, method, path, body string) resp {
	t.Helper()
	r, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	r.SetBasicAuth(user, pass)
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return resp{status: res.StatusCode}
}

func TestSeededUsersStillAuthenticate(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, u := range []string{"admin", "ci", "reader"} {
		wantStatus(t, e.get(t, "/-/api/me", u), http.StatusOK)
		if e.get(t, "/-/api/me", u).json(t)["user"] != u {
			t.Fatalf("user %s not authenticated", u)
		}
	}
}

func TestUserCRUD(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink

	r := e.admin(t, "GET", "/-/api/users", "")
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.body, "$2a$") || strings.Contains(strings.ToLower(r.body), "passwordhash") {
		t.Fatalf("hash leaked: %s", r.body)
	}
	if n := len(r.json(t)["users"].([]any)); n != 3 {
		t.Fatalf("users = %d: %s", n, r.body)
	}

	r = e.admin(t, "POST", "/-/api/users",
		`{"id":"alice@example.com","name":"Alice","email":"a@example.com","password":"s3cret-pw","roles":["deployer"]}`)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	if m["id"] != "alice@example.com" || m["source"] != "local" || m["status"] != "active" || m["hasPassword"] != true ||
		m["name"] != "Alice" || m["email"] != "a@example.com" || m["createdAt"] == nil || m["updatedAt"] == nil {
		t.Fatalf("created: %s", r.body)
	}
	if strings.Contains(r.body, "s3cret-pw") || strings.Contains(r.body, "$2a$") {
		t.Fatalf("secret in response: %s", r.body)
	}
	// The new user can sign in at once.
	if st := e.basicWith(t, "alice@example.com", "s3cret-pw", "GET", "/-/api/me", "").status; st != http.StatusOK {
		t.Fatalf("new user login: %d", st)
	}

	r = e.admin(t, "GET", "/-/api/users/alice@example.com", "")
	wantStatus(t, r, http.StatusOK)
	if got := r.json(t)["roles"].([]any); len(got) != 1 || got[0] != "deployer" {
		t.Fatalf("get: %s", r.body)
	}

	r = e.admin(t, "PUT", "/-/api/users/alice@example.com", `{"name":"Alice B","email":"","roles":["admin"],"status":"active"}`)
	wantStatus(t, r, http.StatusOK)
	m = r.json(t)
	if m["name"] != "Alice B" || m["roles"].([]any)[0] != "admin" || m["hasPassword"] != true {
		t.Fatalf("update: %s", r.body)
	}
	// Roles apply immediately.
	wantStatus(t, e.basicAs(t, "alice@example.com", "s3cret-pw", "GET", "/-/api/users", ""), http.StatusOK)

	wantStatus(t, e.admin(t, "DELETE", "/-/api/users/alice@example.com", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "GET", "/-/api/users/alice@example.com", ""), http.StatusNotFound)
	if st := e.basicWith(t, "alice@example.com", "s3cret-pw", "GET", "/-/api/me", "").status; st != http.StatusUnauthorized {
		t.Fatalf("deleted user still signs in: %d", st)
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/users/alice@example.com", ""), http.StatusNotFound)

	var kinds []string
	for _, ev := range sink.Events() {
		kinds = append(kinds, ev.Kind+":"+ev.Subject)
		if strings.Contains(ev.Detail, "s3cret") {
			t.Fatalf("secret in event: %+v", ev)
		}
	}
	want := "user.create:alice@example.com user.update:alice@example.com user.delete:alice@example.com"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events %q, want %q", got, want)
	}
}

func TestUserValidation(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	long := strings.Repeat("a", 65)
	cases := []struct {
		name, body string
		want       int
	}{
		{"ok ldap", `{"id":"ldapper","source":"ldap","roles":[]}`, http.StatusCreated},
		{"duplicate", `{"id":"ci","password":"longenough"}`, http.StatusConflict},
		{"bad id slash", `{"id":"a/b","password":"longenough"}`, http.StatusBadRequest},
		{"bad id lead dot", `{"id":".a","password":"longenough"}`, http.StatusBadRequest},
		{"bad id space", `{"id":"a b","password":"longenough"}`, http.StatusBadRequest},
		{"empty id", `{"id":"","password":"longenough"}`, http.StatusBadRequest},
		{"long id", `{"id":"` + long + `","password":"longenough"}`, http.StatusBadRequest},
		{"reserved id", `{"id":"create","password":"longenough"}`, http.StatusBadRequest},
		{"short password", `{"id":"u1","password":"short"}`, http.StatusBadRequest},
		{"seven runes", `{"id":"u1","password":"1234567"}`, http.StatusBadRequest},
		{"eight runes", `{"id":"u8","password":"12345678"}`, http.StatusCreated},
		{"no password local", `{"id":"u2"}`, http.StatusBadRequest},
		{"unknown role", `{"id":"u3","password":"longenough","roles":["ghost"]}`, http.StatusBadRequest},
		{"bad source", `{"id":"u4","password":"longenough","source":"saml"}`, http.StatusBadRequest},
		{"bad status", `{"id":"u5","password":"longenough","status":"gone"}`, http.StatusBadRequest},
		{"ldap with password", `{"id":"u6","source":"ldap","password":"longenough"}`, http.StatusBadRequest},
		{"unknown field", `{"id":"u7","password":"longenough","hash":"x"}`, http.StatusBadRequest},
		{"not json", `{`, http.StatusBadRequest},
		// QA N-17, N-18, N-21
		{"email no at", `{"id":"u10","password":"longenough","email":"bad"}`, http.StatusBadRequest},
		{"email list", `{"id":"u11","password":"longenough","email":"x@y.z,evil@q.r"}`, http.StatusBadRequest},
		{"email display name", `{"id":"u12","password":"longenough","email":"Bob <bob@example.com>"}`, http.StatusBadRequest},
		{"email crlf", `{"id":"u13","password":"longenough","email":"a@example.com\r\nBcc: e@q.r"}`, http.StatusBadRequest},
		{"name newline", `{"id":"u14","password":"longenough","name":"A\nB"}`, http.StatusBadRequest},
		{"email ok", `{"id":"u15","password":"longenough","email":"a.b+c@example.com","name":"Ä B"}`, http.StatusCreated},
		{"spaces password", `{"id":"u16","password":"        "}`, http.StatusBadRequest},
		{"case variant id", `{"id":"CI","password":"longenough"}`, http.StatusConflict},
	}
	for _, c := range cases {
		r := e.admin(t, "POST", "/-/api/users", c.body)
		if r.status != c.want {
			t.Errorf("%s: %d, want %d: %s", c.name, r.status, c.want, r.body)
		}
		if c.want == http.StatusBadRequest && c.name != "unknown field" && c.name != "not json" {
			m := r.json(t)
			if m["error"] == "" || m["errors"] == nil {
				t.Errorf("%s: shape %s", c.name, r.body)
			}
		}
	}
	r := e.admin(t, "GET", "/-/api/users/ldapper", "")
	if m := r.json(t); m["source"] != "ldap" || m["hasPassword"] != false {
		t.Fatalf("ldap user: %s", r.body)
	}
	// update validation
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci", `{"roles":["ghost"]}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci", `{"status":"nope"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ghost", `{"roles":[]}`), http.StatusNotFound)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci", `{"password":"abcdefgh"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci", `{"email":"x@y.z,evil@q.r","roles":["deployer"]}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci", `{"name":"ci\u0000","roles":["deployer"]}`), http.StatusBadRequest)
	// The conflict names the stored id.
	if r := e.admin(t, "POST", "/-/api/users", `{"id":"Admin","password":"longenough"}`); r.status != http.StatusConflict || !strings.Contains(r.body, `already exists as \"admin\"`) {
		t.Fatalf("case variant: %d %s", r.status, r.body)
	}
}

func TestAdminPasswordReset(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	cookie := e.cookieFor("ci")
	wantStatus(t, e.req(t, "GET", "/-/api/me", "", "", cookie, false), http.StatusOK)
	_, tok := e.newToken(t, "ci", "")

	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci/password", `{"password":"short"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ghost/password", `{"password":"longenough"}`), http.StatusNotFound)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci/password", `{"password":"brand-new-pw"}`), http.StatusNoContent)

	if st := e.basicWith(t, "ci", "pw", "GET", "/-/api/tokens", "").status; st != http.StatusUnauthorized {
		t.Fatalf("old password: %d", st)
	}
	if st := e.basicWith(t, "ci", "brand-new-pw", "GET", "/-/api/tokens", "").status; st != http.StatusOK {
		t.Fatalf("new password: %d", st)
	}
	// Its sessions ended.
	if e.req(t, "GET", "/-/api/me", "", "", cookie, false).json(t)["anonymous"] != true {
		t.Fatal("session survived the reset")
	}
	// Tokens are unaffected by a password reset.
	if st := e.bearer(t, "/-/api/me", tok); st != http.StatusOK {
		t.Fatalf("token after reset: %d", st)
	}
	ev := sink.Events()
	if len(ev) == 0 || ev[len(ev)-1].Kind != "user.password" || ev[len(ev)-1].Detail != "reset" || ev[len(ev)-1].Subject != "ci" {
		t.Fatalf("events: %s", sink)
	}
	// an LDAP user has no password to reset
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"ld","source":"ldap"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ld/password", `{"password":"longenough"}`), http.StatusBadRequest)
}

func TestSelfServicePassword(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	cookie := e.cookieFor("ci")
	put := func(body string, c *http.Cookie, user string) resp {
		return e.req(t, "PUT", "/-/api/account/password", body, user, c, true)
	}
	wantStatus(t, put(`{"currentPassword":"pw","newPassword":"newpassword1"}`, nil, ""), http.StatusUnauthorized)
	r := put(`{"currentPassword":"wrong","newPassword":"newpassword1"}`, cookie, "")
	if r.status != http.StatusForbidden {
		t.Fatalf("wrong current: %d %s", r.status, r.body)
	}
	wantStatus(t, put(`{"currentPassword":"pw","newPassword":"short"}`, cookie, ""), http.StatusBadRequest)
	// no UI header
	wantStatus(t, e.req(t, "PUT", "/-/api/account/password", `{"currentPassword":"pw","newPassword":"newpassword1"}`, "ci", nil, false), http.StatusForbidden)
	// a token cannot change passwords
	_, tok := e.newToken(t, "ci", "")
	tr, _ := http.NewRequest("PUT", e.ts.URL+"/-/api/account/password",
		strings.NewReader(`{"currentPassword":"pw","newPassword":"newpassword1"}`))
	tr.Header.Set("Authorization", "Bearer "+tok)
	tr.Header.Set("X-Command-Center-UI", "1")
	res, err := noRedirect.Do(tr)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("token: %d", res.StatusCode)
	}

	r = put(`{"currentPassword":"pw","newPassword":"newpassword1"}`, cookie, "")
	wantStatus(t, r, http.StatusNoContent)
	if st := e.basicWith(t, "ci", "pw", "GET", "/-/api/tokens", "").status; st != http.StatusUnauthorized {
		t.Fatalf("old password: %d", st)
	}
	if st := e.basicWith(t, "ci", "newpassword1", "GET", "/-/api/tokens", "").status; st != http.StatusOK {
		t.Fatalf("new password: %d", st)
	}
	// The caller's own session was replaced and keeps working.
	var fresh *http.Cookie
	for _, c := range r.cookie {
		if c.Name == cookie.Name && c.Value != "" {
			fresh = c
		}
	}
	if fresh == nil || fresh.Value == cookie.Value {
		t.Fatalf("no replacement session cookie: %v", r.cookie)
	}
	if e.req(t, "GET", "/-/api/me", "", "", fresh, false).json(t)["user"] != "ci" {
		t.Fatal("fresh session does not work")
	}
	if e.req(t, "GET", "/-/api/me", "", "", cookie, false).json(t)["anonymous"] != true {
		t.Fatal("old session survived")
	}
	ev := sink.Events()
	if len(ev) == 0 || ev[len(ev)-1].Kind != "user.password" || ev[len(ev)-1].Detail != "self" || ev[len(ev)-1].Actor != "ci" {
		t.Fatalf("events: %s", sink)
	}
}

func TestRoleCRUD(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	r := e.admin(t, "GET", "/-/api/roles", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"admin"`) || !strings.Contains(r.body, `"deployer"`) {
		t.Fatalf("roles: %s", r.body)
	}
	body := `{"id":"npm-dev","description":"npm devs","privileges":[
		{"target":"format:npm","actions":["browse","read"]},
		{"target":"repo:npm-private","actions":["add","edit","delete"]},
		{"target":"*","actions":["browse"]}]}`
	r = e.admin(t, "POST", "/-/api/roles", body)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	if m["id"] != "npm-dev" || m["description"] != "npm devs" || len(m["privileges"].([]any)) != 3 || m["createdAt"] == nil || m["updatedAt"] == nil {
		t.Fatalf("created: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/roles", body), http.StatusConflict)
	r = e.admin(t, "GET", "/-/api/roles/npm-dev", "")
	wantStatus(t, r, http.StatusOK)

	r = e.admin(t, "PUT", "/-/api/roles/npm-dev", `{"description":"changed","privileges":[{"target":"format:maven","actions":["read"]}]}`)
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["description"] != "changed" || len(m["privileges"].([]any)) != 1 {
		t.Fatalf("updated: %s", r.body)
	}
	// Privileges apply immediately: a user with this role can read maven.
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"dev","password":"longenough","roles":["npm-dev"]}`), http.StatusCreated)
	// in use
	r = e.admin(t, "DELETE", "/-/api/roles/npm-dev", "")
	wantStatus(t, r, http.StatusConflict)
	if r.json(t)["error"] != "role is in use" {
		t.Fatalf("in use: %s", r.body)
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/users/dev", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/roles/npm-dev", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/roles/npm-dev", ""), http.StatusNotFound)
	wantStatus(t, e.admin(t, "GET", "/-/api/roles/npm-dev", ""), http.StatusNotFound)
	wantStatus(t, e.admin(t, "PUT", "/-/api/roles/ghost", `{"privileges":[]}`), http.StatusNotFound)

	var kinds []string
	for _, ev := range sink.Events() {
		if strings.HasPrefix(ev.Kind, "role.") {
			kinds = append(kinds, ev.Kind+":"+ev.Subject)
		}
	}
	if got := strings.Join(kinds, " "); got != "role.create:npm-dev role.update:npm-dev role.delete:npm-dev" {
		t.Fatalf("events %q", got)
	}
}

func TestRoleValidationAndAdminImmutable(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	cases := []struct {
		name, body string
		want       int
	}{
		{"admin reserved", `{"id":"admin","privileges":[]}`, http.StatusBadRequest},
		{"create reserved", `{"id":"create","privileges":[]}`, http.StatusBadRequest},
		{"at in id", `{"id":"a@b","privileges":[]}`, http.StatusBadRequest},
		{"empty id", `{"id":"","privileges":[]}`, http.StatusBadRequest},
		{"bad target", `{"id":"r1","privileges":[{"target":"everything","actions":["read"]}]}`, http.StatusBadRequest},
		{"bad format", `{"id":"r1","privileges":[{"target":"format:cran","actions":["read"]}]}`, http.StatusBadRequest},
		{"bad repo name", `{"id":"r1","privileges":[{"target":"repo:a/b","actions":["read"]}]}`, http.StatusBadRequest},
		{"empty repo name", `{"id":"r1","privileges":[{"target":"repo:","actions":["read"]}]}`, http.StatusBadRequest},
		{"empty actions", `{"id":"r1","privileges":[{"target":"*","actions":[]}]}`, http.StatusBadRequest},
		{"unknown action", `{"id":"r1","privileges":[{"target":"*","actions":["read","fly"]}]}`, http.StatusBadRequest},
		{"duplicate action", `{"id":"r1","privileges":[{"target":"*","actions":["read","read"]}]}`, http.StatusBadRequest},
		{"all formats ok", `{"id":"r2","privileges":[{"target":"format:maven","actions":["read"]},{"target":"format:npm","actions":["read"]},{"target":"format:pypi","actions":["read"]},{"target":"format:go","actions":["read"]},{"target":"format:cargo","actions":["read"]}]}`, http.StatusCreated},
		{"no privileges ok", `{"id":"r3"}`, http.StatusCreated},
		{"unknown field", `{"id":"r4","x":1}`, http.StatusBadRequest},
		// QA N-17, N-18, N-25, L-9
		{"admin reserved in any case", `{"id":"Admin","privileges":[]}`, http.StatusBadRequest},
		{"case variant id", `{"id":"Deployer","privileges":[]}`, http.StatusConflict},
		{"control in description", `{"id":"r6","description":"a\u0007b"}`, http.StatusBadRequest},
		{"duplicate target", `{"id":"r7","privileges":[{"target":"*","actions":["read"]},{"target":"*","actions":["browse"]}]}`, http.StatusBadRequest},
		{"unknown repository", `{"id":"r8","privileges":[{"target":"repo:does-not-exist","actions":["read"]}]}`, http.StatusBadRequest},
		{"existing repository", `{"id":"r9","privileges":[{"target":"repo:maven-releases","actions":["read"]}]}`, http.StatusCreated},
	}
	for _, c := range cases {
		r := e.admin(t, "POST", "/-/api/roles", c.body)
		if r.status != c.want {
			t.Errorf("%s: %d, want %d: %s", c.name, r.status, c.want, r.body)
		}
	}
	r := e.admin(t, "POST", "/-/api/roles", `{"id":"r5","privileges":[{"target":"x","actions":["read"]}]}`)
	if m := r.json(t); m["error"] == "" || len(m["errors"].([]any)) == 0 {
		t.Fatalf("shape: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/roles/deployer", `{"privileges":[{"target":"*","actions":["fly"]}]}`), http.StatusBadRequest)

	r = e.admin(t, "GET", "/-/api/roles/admin", "")
	wantStatus(t, r, http.StatusOK)
	r = e.admin(t, "PUT", "/-/api/roles/admin", `{"privileges":[]}`)
	wantStatus(t, r, http.StatusConflict)
	if r.json(t)["error"] != "the admin role cannot be modified" {
		t.Fatalf("%s", r.body)
	}
	r = e.admin(t, "DELETE", "/-/api/roles/admin", "")
	wantStatus(t, r, http.StatusConflict)
	if r.json(t)["error"] != "the admin role cannot be deleted" {
		t.Fatalf("%s", r.body)
	}
}

func TestLockoutProtection(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	const msg = "this change would leave no admin who can log in"
	refused := func(r resp) {
		t.Helper()
		wantStatus(t, r, http.StatusConflict)
		if r.json(t)["error"] != msg {
			t.Fatalf("error: %s", r.body)
		}
	}
	// admin is the only admin: disabling, deleting and demoting are refused
	refused(e.admin(t, "PUT", "/-/api/users/admin", `{"roles":["admin"],"status":"disabled"}`))
	refused(e.admin(t, "DELETE", "/-/api/users/admin", ""))
	refused(e.admin(t, "PUT", "/-/api/users/admin", `{"roles":[],"status":"active"}`))
	// nothing changed
	if m := e.admin(t, "GET", "/-/api/users/admin", "").json(t); m["status"] != "active" || len(m["roles"].([]any)) != 1 {
		t.Fatalf("admin changed: %v", m)
	}
	wantStatus(t, e.get(t, "/-/api/users", "admin"), http.StatusOK)

	// an LDAP admin, or a disabled one, does not count as a second admin
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"ldapadmin","source":"ldap","roles":["admin"]}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"offadmin","password":"longenough","roles":["admin"],"status":"disabled"}`), http.StatusCreated)
	refused(e.admin(t, "DELETE", "/-/api/users/admin", ""))

	// with a second qualifying admin the first can be demoted
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"admin2","password":"longenough","roles":["admin"]}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/admin", `{"roles":[],"status":"active"}`), http.StatusOK)
	// admin2 is now the last one
	refused(e.basicAs(t, "admin2", "longenough", "DELETE", "/-/api/users/admin2", ""))
	refused(e.basicAs(t, "admin2", "longenough", "PUT", "/-/api/users/admin2", `{"roles":["admin"],"status":"disabled"}`))
}

// QA L-2: an admin with a placeholder passwordHash cannot sign in, so it
// does not show hasPassword and does not keep the last real admin removable.
func TestLockoutIgnoresPlaceholderHash(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Users = append(c.Users, config.User{Name: "placeholder", PasswordHash: "<commandcenter hash-password 출력값>", Roles: []string{"admin"}})
	})
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/admin", `{"roles":[],"status":"active"}`), http.StatusConflict)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/admin", `{"roles":["admin"],"status":"disabled"}`), http.StatusConflict)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/users/admin", ""), http.StatusConflict)
	if m := e.admin(t, "GET", "/-/api/users/placeholder", "").json(t); m["hasPassword"] != false {
		t.Fatalf("placeholder: %v", m)
	}
	var list struct {
		Users []struct {
			ID          string `json:"id"`
			HasPassword bool   `json:"hasPassword"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(e.admin(t, "GET", "/-/api/users", "").body), &list); err != nil {
		t.Fatal(err)
	}
	for _, u := range list.Users {
		if u.HasPassword != (u.ID != "placeholder") {
			t.Errorf("list: %s hasPassword = %v", u.ID, u.HasPassword)
		}
	}
}

// basicAs is req with an explicit password and the UI header.
func (e *env) basicAs(t *testing.T, user, pass, method, path, body string) resp {
	t.Helper()
	r, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	r.SetBasicAuth(user, pass)
	r.Header.Set("X-Command-Center-UI", "1")
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp{status: res.StatusCode, header: res.Header, body: sb.String()}
}

func TestDisableAndDeleteEndSessionsAndTokens(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	cookie := e.cookieFor("ci")
	id, tok := e.newToken(t, "ci", "")
	_ = id
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci", `{"roles":["deployer"],"status":"disabled"}`), http.StatusOK)
	if e.req(t, "GET", "/-/api/me", "", "", cookie, false).json(t)["anonymous"] != true {
		t.Fatal("session survived disabling")
	}
	if st := e.bearer(t, "/-/api/tokens", tok); st != http.StatusUnauthorized {
		t.Fatalf("token of a disabled user: %d", st)
	}
	// re-enabling makes the token work again
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci", `{"roles":["deployer"],"status":"active"}`), http.StatusOK)
	if st := e.bearer(t, "/-/api/tokens", tok); st != http.StatusOK {
		t.Fatalf("token after re-enable: %d", st)
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/users/ci", ""), http.StatusNoContent)
	if st := e.bearer(t, "/-/api/tokens", tok); st != http.StatusUnauthorized {
		t.Fatalf("token of a deleted user: %d", st)
	}
}

// QA N-20: a disabled source=ldap row is what keeps a directory user out,
// so deleting it, which would let the user in again, is refused until it
// is set active. Disabled local users can still be deleted.
func TestDeleteDisabledLDAPOverride(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"ld","source":"ldap","status":"disabled"}`), http.StatusCreated)
	r := e.admin(t, "DELETE", "/-/api/users/ld", "")
	wantStatus(t, r, http.StatusConflict)
	if msg, _ := r.json(t)["error"].(string); !strings.Contains(msg, "blocks LDAP user ld") {
		t.Fatalf("error: %s", r.body)
	}
	wantStatus(t, e.admin(t, "GET", "/-/api/users/ld", ""), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ld", `{"roles":[],"status":"active"}`), http.StatusOK)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/users/ld", ""), http.StatusNoContent)

	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"off","password":"longenough","status":"disabled"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/users/off", ""), http.StatusNoContent)
}

func TestAccessGates(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	_, adminTok := e.newToken(t, "admin", "")
	userBody := `{"id":"x1","password":"longenough"}`
	type call struct{ method, path, body string }
	mutations := []call{
		{"POST", "/-/api/users", userBody},
		{"PUT", "/-/api/users/ci", `{"roles":[]}`},
		{"DELETE", "/-/api/users/ci", ""},
		{"PUT", "/-/api/users/ci/password", `{"password":"longenough"}`},
		{"POST", "/-/api/roles", `{"id":"rx"}`},
		{"PUT", "/-/api/roles/deployer", `{"privileges":[]}`},
		{"DELETE", "/-/api/roles/deployer", ""},
	}
	reads := []call{
		{"GET", "/-/api/users", ""}, {"GET", "/-/api/users/ci", ""},
		{"GET", "/-/api/roles", ""}, {"GET", "/-/api/roles/deployer", ""},
	}
	for _, c := range mutations {
		id := c.method + " " + c.path
		wantStatus2(t, id+" anon", e.req(t, c.method, c.path, c.body, "", nil, true), http.StatusUnauthorized)
		wantStatus2(t, id+" non-admin", e.req(t, c.method, c.path, c.body, "ci", nil, true), http.StatusForbidden)
		wantStatus2(t, id+" no header", e.req(t, c.method, c.path, c.body, "admin", nil, false), http.StatusForbidden)
		wantStatus2(t, id+" session no header", e.req(t, c.method, c.path, c.body, "", e.cookieFor("admin"), false), http.StatusUnauthorized)
		tr, _ := http.NewRequest(c.method, e.ts.URL+c.path, strings.NewReader(c.body))
		tr.Header.Set("Authorization", "Bearer "+adminTok)
		tr.Header.Set("X-Command-Center-UI", "1")
		res, err := noRedirect.Do(tr)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s token: %d", id, res.StatusCode)
		}
	}
	for _, c := range reads {
		id := c.method + " " + c.path
		wantStatus2(t, id+" anon", e.req(t, c.method, c.path, "", "", nil, false), http.StatusUnauthorized)
		wantStatus2(t, id+" non-admin", e.req(t, c.method, c.path, "", "ci", nil, false), http.StatusForbidden)
		wantStatus2(t, id+" admin", e.req(t, c.method, c.path, "", "admin", nil, false), http.StatusOK)
	}
	// nothing was changed by the refused calls
	if n := len(e.admin(t, "GET", "/-/api/users", "").json(t)["users"].([]any)); n != 3 {
		t.Fatalf("users = %d", n)
	}
}

func wantStatus2(t *testing.T, what string, r resp, want int) {
	t.Helper()
	if r.status != want {
		t.Errorf("%s: status %d, want %d: %s", what, r.status, want, r.body)
	}
}

// QA N-21: a new password of only spaces, or equal to the current one, is
// refused by the self-service change.
func TestSelfServicePasswordMustChange(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/ci/password", `{"password":"same-password"}`), http.StatusNoContent)
	cookie := e.cookieFor("ci")
	put := func(body string) resp { return e.req(t, "PUT", "/-/api/account/password", body, "", cookie, true) }
	if r := put(`{"currentPassword":"same-password","newPassword":"same-password"}`); r.status != http.StatusBadRequest ||
		!strings.Contains(r.body, "must differ from the current one") {
		t.Fatalf("same password: %d %s", r.status, r.body)
	}
	wantStatus(t, put(`{"currentPassword":"same-password","newPassword":"          "}`), http.StatusBadRequest)
	wantStatus(t, put(`{"currentPassword":"same-password","newPassword":"another-pw"}`), http.StatusNoContent)
}

// QA N-25: a "repo:" target must name an existing repository, unless the
// stored role already has it (as an older version left it after a delete).
// A delete now removes the target, so a form that still shows it gets 400.
func TestRoleRepoTargetMustExist(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.admin(t, "POST", "/-/api/roles", `{"id":"future","privileges":[{"target":"repo:future-repo","actions":["read"]}]}`)
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, `unknown repository \"future-repo\"`) {
		t.Fatalf("missing repository: %d %s", r.status, r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"tmp-repo","format":"maven","type":"hosted"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/roles", `{"id":"keeper","privileges":[{"target":"repo:tmp-repo","actions":["read"]}]}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/repositories/tmp-repo", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "PUT", "/-/api/roles/keeper", `{"privileges":[{"target":"repo:tmp-repo","actions":["read"]}]}`), http.StatusBadRequest)
	// As an older version left it.
	if err := e.db.UpdateRole(context.Background(), meta.Role{ID: "keeper",
		Privileges: []meta.Privilege{{Target: "repo:tmp-repo", Actions: []string{"read"}}}}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/roles/keeper",
		`{"description":"kept","privileges":[{"target":"repo:tmp-repo","actions":["read","browse"]}]}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/roles/keeper",
		`{"privileges":[{"target":"repo:tmp-repo","actions":["read"]},{"target":"repo:other-missing","actions":["read"]}]}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/roles/keeper",
		`{"privileges":[{"target":"repo:tmp-repo","actions":["read"]},{"target":"repo:tmp-repo","actions":["browse"]}]}`), http.StatusBadRequest)
}

// QA N-16: an admin revokes every API token of a user at once; a password
// reset alone keeps them.
func TestRevokeUserTokens(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	_, tok1 := e.newToken(t, "ci", "")
	_, tok2 := e.newToken(t, "ci", "")
	_, other := e.newToken(t, "reader", "")
	wantStatus(t, e.asUser(t, "reader", "DELETE", "/-/api/users/ci/tokens", ""), http.StatusForbidden)
	wantStatus(t, e.req(t, "DELETE", "/-/api/users/ci/tokens", "", "admin", nil, false), http.StatusForbidden) // no UI header
	r := e.admin(t, "DELETE", "/-/api/users/ci/tokens", "")
	wantStatus(t, r, http.StatusOK)
	if n := r.json(t)["revoked"]; n != float64(2) {
		t.Fatalf("revoked = %v: %s", n, r.body)
	}
	for _, tok := range []string{tok1, tok2} {
		if st := e.bearer(t, "/-/api/me", tok); st != http.StatusUnauthorized {
			t.Fatalf("revoked token: %d", st)
		}
	}
	if st := e.bearer(t, "/-/api/me", other); st != http.StatusOK {
		t.Fatalf("another user's token: %d", st)
	}
	ev := sink.Events()
	if len(ev) == 0 || ev[len(ev)-1].Kind != "token.revoke" || ev[len(ev)-1].Subject != "ci" || ev[len(ev)-1].Detail != "all 2" {
		t.Fatalf("events: %s", sink)
	}
	if n := e.admin(t, "DELETE", "/-/api/users/ci/tokens", "").json(t)["revoked"]; n != float64(0) {
		t.Fatalf("second revoke = %v", n)
	}
}

// QA N-17: values an older version stored are not re-checked unless they
// change, so such a user can still be edited and disabled.
func TestLegacyUserFieldsTolerated(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	ctx := context.Background()
	if err := e.db.CreateUser(ctx, meta.User{ID: "old", Name: "a\nb", Email: "x@y.z,evil@q.r"}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.CreateRole(ctx, meta.Role{ID: "oldrole", Description: "line\r\nbreak"}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/old", `{"name":"a\nb","email":"x@y.z,evil@q.r","status":"disabled"}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/old", `{"name":"a\nb","email":"other@q.r,evil@q.r"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/old", `{"name":"a\nc","email":"x@y.z,evil@q.r"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/old", `{"name":"A B","email":"a@example.com"}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/roles/oldrole", `{"description":"line\r\nbreak"}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/roles/oldrole", `{"description":"new\u0000"}`), http.StatusBadRequest)
}
