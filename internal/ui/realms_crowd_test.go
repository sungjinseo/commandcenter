package ui

import (
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/auth"
	"commandcenter/internal/auth/crowdtest"
	"commandcenter/internal/repo/repotest"
)

func crowdBody(url, extra string) string {
	return `{"url":"` + url + `","applicationName":"commandcenter","applicationPassword":"app-secret","timeoutSeconds":2,` +
		`"groupMappings":[{"group":"cc-admins","roles":["admin"]},{"group":"devs","roles":["deployer"]}]` + extra + `}`
}

func TestCrowdRealmAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	s := crowdtest.New(t)
	s.Put("alice", crowdtest.User{Password: "pw", DisplayName: "Alice", Email: "alice@example.org", Groups: []string{"devs"}})
	s.Put("carol", crowdtest.User{Password: "pw", Inactive: true})

	r := e.admin(t, "GET", "/-/api/realms/crowd", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"cacheMinutes":1`) || !strings.Contains(r.body, `"applicationPasswordSet":false`) {
		t.Fatalf("defaults: %s", r.body)
	}
	wantStatus(t, e.get(t, "/-/api/realms/crowd", "ci"), http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/realms/crowd", crowdBody(s.URL, ""), "admin", nil, false), http.StatusForbidden) // no UI header
	// Enabling the realm needs a configuration.
	realms := `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"crowd","enabled":true}]}`
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", realms), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/crowd", `{"url":"ftp://x","applicationName":"a"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/crowd", `{"url":"`+s.URL+`","applicationName":"a","groupMappings":[{"group":"g","roles":["nope"]}]}`), http.StatusBadRequest)

	r = e.admin(t, "PUT", "/-/api/realms/crowd", crowdBody(s.URL+"/", ""))
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.body, "app-secret") || !strings.Contains(r.body, `"applicationPasswordSet":true`) || !strings.Contains(r.body, `"url":"`+s.URL+`"`) {
		t.Fatalf("PUT: %s", r.body)
	}
	if raw, _ := e.db.GetSetting(t.Context(), "crowd"); strings.Contains(raw, "app-secret") == e.db.HasSecretKey() {
		t.Fatalf("stored %s", raw) // sealed when a key is set; this env has none
	}
	// The stored password stays with the same server, not with another one.
	keep := `{"url":"` + s.URL + `","applicationName":"commandcenter","timeoutSeconds":2,"cacheMinutes":2}`
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/crowd", keep), http.StatusOK)
	r = e.admin(t, "PUT", "/-/api/realms/crowd", `{"url":"https://other.example/crowd","applicationName":"commandcenter"}`)
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, crowdReenterMessage) {
		t.Fatalf("other server: %d %s", r.status, r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/crowd", crowdBody(s.URL, "")), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", realms), http.StatusOK)

	// The connection test: the application, then a user's password or the user.
	r = e.admin(t, "POST", "/-/api/realms/crowd/test", `{}`)
	if wantStatus(t, r, http.StatusOK); !strings.Contains(r.body, `"ok":true`) || !strings.Contains(r.body, `"step":"application"`) {
		t.Fatalf("test: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/realms/crowd/test", `{"username":"alice","password":"pw"}`)
	if !strings.Contains(r.body, `"ok":true`) || !strings.Contains(r.body, "roles deployer") || strings.Contains(r.body, `"pw"`) {
		t.Fatalf("test alice: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/realms/crowd/test", `{"username":"alice","password":"wrong"}`)
	if !strings.Contains(r.body, `"ok":false`) || !strings.Contains(r.body, "INVALID_USER_AUTHENTICATION") {
		t.Fatalf("test wrong password: %s", r.body)
	}
	if r = e.admin(t, "POST", "/-/api/realms/crowd/test", `{"username":"carol"}`); !strings.Contains(r.body, "INACTIVE_ACCOUNT") {
		t.Fatalf("test carol: %s", r.body)
	}

	// A Crowd user signs in to the UI, and shows in /me, the user list
	// and the status checks.
	r = e.req(t, "POST", "/-/api/login", `{"user":"alice","password":"pw"}`, "", nil, true)
	wantStatus(t, r, http.StatusOK)
	var cookie *http.Cookie
	for _, c := range r.cookie {
		if c.Name == auth.SessionCookie {
			cookie = c
		}
	}
	r = e.req(t, "GET", "/-/api/me", "", "", cookie, false)
	if !strings.Contains(r.body, `"realm":"Crowd"`) || !strings.Contains(r.body, `"owner":"crowd:alice"`) || !strings.Contains(r.body, `"deployer"`) {
		t.Fatalf("me: %s", r.body)
	}
	wantStatus(t, e.req(t, "POST", "/-/api/login", `{"user":"carol","password":"pw"}`, "", nil, true), http.StatusUnauthorized)
	if r = e.get(t, "/-/api/users", "admin"); !strings.Contains(r.body, `"realm":"Crowd"`) || !strings.Contains(r.body, `"name":"Alice"`) {
		t.Fatalf("users: %s", r.body)
	}
	if r = e.get(t, "/-/api/status/checks", "admin"); !strings.Contains(r.body, `"name":"Crowd reachable","healthy":true`) {
		t.Fatalf("checks: %s", r.body)
	}
	// A Crowd override row is accepted.
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"alice","source":"crowd","roles":["admin"]}`), http.StatusCreated)
	s.Close()
	if r = e.get(t, "/-/api/status/checks", "admin"); !strings.Contains(r.body, `"name":"Crowd reachable","healthy":false`) {
		t.Fatalf("checks with Crowd down: %s", r.body)
	}
	if ev := sink.String(); !strings.Contains(ev, "realms.crowd.update") || !strings.Contains(ev, "realms.crowd.test") {
		t.Fatalf("events: %s", ev)
	}
	for _, ev := range sink.Events() {
		if strings.Contains(ev.Detail, "app-secret") {
			t.Fatalf("event %+v holds the application password", ev)
		}
	}
}

// A role a Crowd group mapping names cannot be deleted.
func TestCrowdMappedRoleNotDeleted(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	s := crowdtest.New(t)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/crowd", crowdBody(s.URL, "")), http.StatusOK)
	if r := e.admin(t, "DELETE", "/-/api/roles/deployer", ""); r.status != http.StatusConflict || !strings.Contains(r.body, "Crowd") {
		t.Fatalf("role delete: %d %s", r.status, r.body)
	}
}
