package ui

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"commandcenter/internal/ldap/ldaptest"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

const (
	uiSvcDN = "cn=svc,dc=example,dc=org"
	uiSvcPW = "svc-Secret-42"
)

// ldapEnv is an env with a fake directory: alice (cc-admins) and bob.
func ldapEnv(t *testing.T) (*env, *ldaptest.Server) {
	t.Helper()
	e := newEnv(t, "http://repo.example")
	s := ldaptest.New(t, nil)
	s.Add(ldaptest.Entry{DN: uiSvcDN, Password: uiSvcPW})
	s.Add(ldaptest.Entry{DN: "uid=alice,ou=people,dc=example,dc=org", Password: "alice-pw", Attrs: map[string][]string{
		"uid": {"alice"}, "cn": {"Alice"}, "mail": {"alice@example.org"},
		"memberOf": {"cn=cc-admins,ou=groups,dc=example,dc=org"}}})
	s.Add(ldaptest.Entry{DN: "uid=bob,ou=people,dc=example,dc=org", Password: "bob-pw", Attrs: map[string][]string{
		"uid": {"bob"}, "cn": {"Bob"}, "memberOf": {"cn=devs,ou=groups,dc=example,dc=org"}}})
	return e, s
}

func ldapBody(url string, extra string) string {
	return fmt.Sprintf(`{"url":%q,"bindDN":%q,"bindPassword":%q,"userBaseDN":"ou=people,dc=example,dc=org",
		"timeoutSeconds":2,"groupMappings":[{"group":"cc-admins","roles":["admin"]},{"group":"devs","roles":["deployer"]}]%s}`,
		url, uiSvcDN, uiSvcPW, extra)
}

// configureLDAP stores a working configuration and enables the ldap realm after local and token.
func configureLDAP(t *testing.T, e *env, s *ldaptest.Server) {
	t.Helper()
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/ldap", ldapBody(s.URL("ldap"), "")), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":true}]}`), http.StatusOK)
}

func (e *env) setRealmsDB(t *testing.T, realms []meta.Realm) {
	t.Helper()
	if err := e.db.SetSettingJSON(context.Background(), meta.SettingRealms, realms); err != nil {
		t.Fatal(err)
	}
	if err := e.ui.Auth.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRealmsAPI(t *testing.T) {
	e, s := ldapEnv(t)
	r := e.admin(t, "GET", "/-/api/realms", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true}]`) ||
		!strings.Contains(r.body, `"available":["local","token","ldap","oidc","crowd"]`) {
		t.Fatalf("GET realms: %s", r.body)
	}
	wantStatus(t, e.get(t, "/-/api/realms", "ci"), http.StatusForbidden)
	wantStatus(t, e.req(t, "GET", "/-/api/realms", "", "", nil, false), http.StatusUnauthorized)
	body := `{"realms":[{"id":"token","enabled":true},{"id":"local","enabled":true}]}`
	wantStatus(t, e.req(t, "PUT", "/-/api/realms", body, "admin", nil, false), http.StatusForbidden) // no UI header
	wantStatus(t, e.req(t, "PUT", "/-/api/realms", body, "ci", nil, true), http.StatusForbidden)
	_, tok := e.newToken(t, "admin", "")
	rq, _ := http.NewRequest("PUT", e.ts.URL+"/-/api/realms", strings.NewReader(body))
	rq.Header.Set("Authorization", "Bearer "+tok)
	rq.Header.Set("X-Command-Center-UI", "1")
	if res, err := noRedirect.Do(rq); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("token PUT: %v %v", res, err)
	}

	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", body), http.StatusOK)
	if got := e.ui.Auth.Snapshot().Settings().Realms; len(got) != 2 || got[0].ID != "token" {
		t.Fatalf("reordered: %v", got)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"x","enabled":true}]}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"local","enabled":true}]}`), http.StatusBadRequest)
	// Enabling ldap needs a configuration.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"ldap","enabled":true}]}`), http.StatusBadRequest)
	// Disabling local without another admin path is a lockout.
	r = e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":false},{"id":"token","enabled":true}]}`)
	wantStatus(t, r, http.StatusConflict)
	if r.json(t)["error"] != lockoutMessage {
		t.Fatalf("lockout: %s", r.body)
	}
	r = e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"token","enabled":true}]}`)
	wantStatus(t, r, http.StatusConflict)

	// With an LDAP admin mapping and the ldap realm on, local may be turned off.
	configureLDAP(t, e, s)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"ldap","enabled":true},{"id":"local","enabled":false},{"id":"token","enabled":true}]}`), http.StatusOK)
	// The ldap realm cannot then be turned off as well.
	alice := e.loginCookie(t, "alice", "alice-pw")
	r = e.req(t, "PUT", "/-/api/realms", `{"realms":[{"id":"ldap","enabled":false},{"id":"local","enabled":false}]}`, "", alice, true)
	wantStatus(t, r, http.StatusConflict)
	wantStatus(t, e.req(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":true}]}`, "", alice, true), http.StatusOK)
}

// loginCookie signs in through /-/api/login and returns the session cookie.
func (e *env) loginCookie(t *testing.T, user, pass string) *http.Cookie {
	t.Helper()
	r := e.req(t, "POST", "/-/api/login", fmt.Sprintf(`{"user":%q,"password":%q}`, user, pass), "", nil, true)
	wantStatus(t, r, http.StatusOK)
	for _, c := range r.cookie {
		if c.Name == "cc_session" && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie: %v", r.cookie)
	return nil
}

func TestLDAPSettingsAPI(t *testing.T) {
	e, s := ldapEnv(t)
	sink := &repotest.Sink{}
	e.ui.Events = sink
	r := e.admin(t, "GET", "/-/api/realms/ldap", "")
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if m["bindPasswordSet"] != false || m["userFilter"] != "(uid={user})" || m["timeoutSeconds"] != float64(5) || m["groupAttr"] != "memberOf" {
		t.Fatalf("defaults: %s", r.body)
	}
	wantStatus(t, e.get(t, "/-/api/realms/ldap", "ci"), http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/realms/ldap", ldapBody(s.URL("ldap"), ""), "admin", nil, false), http.StatusForbidden)

	r = e.admin(t, "PUT", "/-/api/realms/ldap", ldapBody(s.URL("ldap"), ""))
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.body, uiSvcPW) || r.json(t)["bindPasswordSet"] != true {
		t.Fatalf("PUT response: %s", r.body)
	}
	if _, ok := r.json(t)["bindPassword"]; ok {
		t.Fatalf("bindPassword field returned: %s", r.body)
	}
	r = e.admin(t, "GET", "/-/api/realms/ldap", "")
	if strings.Contains(r.body, uiSvcPW) || r.json(t)["bindPasswordSet"] != true || r.json(t)["url"] != s.URL("ldap") {
		t.Fatalf("GET: %s", r.body)
	}
	// An empty password keeps the stored one.
	keep := strings.Replace(ldapBody(s.URL("ldap"), ""), `"bindPassword":"`+uiSvcPW+`"`, `"bindPassword":""`, 1)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/ldap", keep), http.StatusOK)
	set, _ := e.db.LoadSettings(context.Background())
	if set.LDAP.BindPassword != uiSvcPW {
		t.Fatal("password not kept")
	}
	// clearBindPassword blanks it (with no bind DN, an anonymous search).
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/ldap", fmt.Sprintf(`{"url":%q,"userBaseDN":"ou=people,dc=example,dc=org","clearBindPassword":true}`, s.URL("ldap"))), http.StatusOK)
	set, _ = e.db.LoadSettings(context.Background())
	if set.LDAP.BindPassword != "" || set.LDAP.BindDN != "" {
		t.Fatalf("not cleared: %v", set.LDAP)
	}
	for name, body := range map[string]string{
		"unknown role":     strings.Replace(ldapBody(s.URL("ldap"), ""), `"deployer"`, `"nope"`, 1),
		"default unknown":  ldapBody(s.URL("ldap"), `,"defaultRoles":["ghost"]`),
		"bad filter":       ldapBody(s.URL("ldap"), `,"userFilter":"(uid={user}"`),
		"substring filter": ldapBody(s.URL("ldap"), `,"userFilter":"(uid={user}*)"`),
		"no {user}":        ldapBody(s.URL("ldap"), `,"userFilter":"(uid=x)"`),
		"bad scheme":       ldapBody("http://x", ""),
		"starttls ldaps":   ldapBody("ldaps://x", `,"startTLS":true`),
		"timeout":          strings.Replace(ldapBody(s.URL("ldap"), ""), `"timeoutSeconds":2`, `"timeoutSeconds":99`, 1),
		"unknown field":    ldapBody(s.URL("ldap"), `,"bogus":1`),
	} {
		if r := e.admin(t, "PUT", "/-/api/realms/ldap", body); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	configureLDAP(t, e, s)
	// The ldap realm is on: the URL cannot be removed.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/ldap", `{"url":""}`), http.StatusBadRequest)
	// local off: removing the last admin mapping is a lockout.
	alice := e.loginCookie(t, "alice", "alice-pw")
	wantStatus(t, e.req(t, "PUT", "/-/api/realms", `{"realms":[{"id":"ldap","enabled":true},{"id":"local","enabled":false},{"id":"token","enabled":true}]}`, "", alice, true), http.StatusOK)
	noAdmin := strings.Replace(ldapBody(s.URL("ldap"), ""), `"roles":["admin"]`, `"roles":["deployer"]`, 1)
	r = e.req(t, "PUT", "/-/api/realms/ldap", noAdmin, "", alice, true)
	wantStatus(t, r, http.StatusConflict)
	if r.json(t)["error"] != lockoutMessage {
		t.Fatalf("%s", r.body)
	}
	for _, ev := range sink.Events() {
		if strings.Contains(ev.Detail, uiSvcPW) || strings.Contains(ev.Detail, "alice-pw") {
			t.Fatalf("secret in event %+v", ev)
		}
	}
	if !strings.Contains(sink.String(), "realms.ldap.update") || !strings.Contains(sink.String(), "realms.update") {
		t.Fatalf("events: %s", sink.String())
	}
	// The general settings still exclude the LDAP configuration.
	r = e.req(t, "GET", "/-/api/settings", "", "", alice, false)
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.body, "ldap") || strings.Contains(r.body, uiSvcPW) {
		t.Fatalf("settings leak: %s", r.body)
	}
}

func TestLDAPTestEndpoint(t *testing.T) {
	e, s := ldapEnv(t)
	wantStatus(t, e.admin(t, "POST", "/-/api/realms/ldap/test", `{}`), http.StatusBadRequest) // not configured
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/ldap", ldapBody(s.URL("ldap"), "")), http.StatusOK)
	wantStatus(t, e.req(t, "POST", "/-/api/realms/ldap/test", `{}`, "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/realms/ldap/test", `{}`, "admin", nil, false), http.StatusForbidden)

	r := e.admin(t, "POST", "/-/api/realms/ldap/test", `{"user":"alice","password":"alice-pw"}`)
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.body, uiSvcPW) || strings.Contains(r.body, "alice-pw") {
		t.Fatalf("secret in test output: %s", r.body)
	}
	m := r.json(t)
	steps := m["steps"].([]any)
	var names []string
	for _, st := range steps {
		names = append(names, st.(map[string]any)["step"].(string))
	}
	if m["ok"] != true || strings.Join(names, ",") != "connect,bind,search,userBind,groups" {
		t.Fatalf("test: %s", r.body)
	}
	if last := steps[len(steps)-1].(map[string]any)["detail"].(string); !strings.Contains(last, "cc-admins") || !strings.Contains(last, "admin") {
		t.Fatalf("groups detail: %s", last)
	}
	r = e.admin(t, "POST", "/-/api/realms/ldap/test", `{"user":"alice","password":"wrong-pw"}`)
	if r.json(t)["ok"] != false || strings.Contains(r.body, "wrong-pw") {
		t.Fatalf("wrong password: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/realms/ldap/test", `{}`)
	if r.json(t)["ok"] != true {
		t.Fatalf("connection only: %s", r.body)
	}
	s.SetDown(true)
	r = e.admin(t, "POST", "/-/api/realms/ldap/test", `{}`)
	if r.json(t)["ok"] != false {
		t.Fatalf("down: %s", r.body)
	}
}

// Re-review rr-auth-1: sign-in refuses a name that is not plain printable
// ASCII (auth.PlainLoginName) with the generic 401, before any realm. The
// LDAP test, the admin's tool for "why can't this user sign in", applies
// the same rule to the name as sent: it never reports such a name as
// working, and its first step says why sign-in refuses it.
func TestLDAPTestAgreesWithSignIn(t *testing.T) {
	e, s := ldapEnv(t)
	s.Add(ldaptest.Entry{DN: "uid=juergen,ou=people,dc=example,dc=org", Password: "j-pw", Attrs: map[string][]string{
		"uid": {"juergen"}, "cn": {"Jürgen"}, "mail": {"jürgen@example.org"}}})
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/ldap", ldapBody(s.URL("ldap"), `,"userFilter":"(mail={user})"`)), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":true}]}`), http.StatusOK)
	for _, c := range []struct{ user, pw string }{
		{"jürgen@example.org", "j-pw"},     // the filter matches a non-ASCII value
		{" alice@example.org", "alice-pw"}, // outer spaces, as an API client sends them (the UI trims)
		{"alice@example.org ", "alice-pw"},
		{"alice@example.org", "alice-pw"}, // a plain name works in both
	} {
		body := fmt.Sprintf(`{"user":%q,"password":%q}`, c.user, c.pw)
		r := e.admin(t, "POST", "/-/api/realms/ldap/test", body)
		wantStatus(t, r, http.StatusOK)
		m := r.json(t)
		login := e.req(t, "POST", "/-/api/login", body, "", nil, true)
		refused := login.status != http.StatusOK
		if (m["ok"] == true) == refused {
			t.Errorf("%q: the LDAP test answers %s, but signing in answers %d %s", c.user, r.body, login.status, login.body)
			continue
		}
		first := m["steps"].([]any)[0].(map[string]any)
		if refused != (first["step"] == "name") || refused && (first["ok"] != false || !strings.Contains(r.body, "sign-in refuses this name")) {
			t.Errorf("%q: %s", c.user, r.body)
		}
	}
}

func TestLDAPLoginThroughAPI(t *testing.T) {
	e, s := ldapEnv(t)
	configureLDAP(t, e, s)

	wantStatus(t, e.req(t, "POST", "/-/api/login", `{"user":"alice","password":"nope"}`, "", nil, true), http.StatusUnauthorized)
	wantStatus(t, e.req(t, "POST", "/-/api/login", `{"user":"alice","password":""}`, "", nil, true), http.StatusUnauthorized)
	alice := e.loginCookie(t, "alice", "alice-pw")
	r := e.req(t, "GET", "/-/api/me", "", "", alice, false)
	m := r.json(t)
	if m["user"] != "alice" || m["owner"] != "ldap:alice" || m["admin"] != true || m["realm"] != "LDAP" || fmt.Sprint(m["roles"]) != "[admin]" {
		t.Fatalf("me: %s", r.body)
	}
	// Basic auth with the LDAP password.
	if r := e.basicAs(t, "bob", "bob-pw", "GET", "/-/api/me", ""); r.status != http.StatusOK || r.json(t)["realm"] != "LDAP" ||
		fmt.Sprint(r.json(t)["roles"]) != "[deployer]" {
		t.Fatalf("basic: %d %s", r.status, r.body)
	}
	if r := e.basicAs(t, "bob", "bad", "GET", "/-/api/repositories/maven-releases", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("basic wrong password: %d", r.status)
	}
	// Own password: managed by LDAP.
	r = e.req(t, "PUT", "/-/api/account/password", `{"currentPassword":"alice-pw","newPassword":"something-new"}`, "", alice, true)
	wantStatus(t, r, http.StatusBadRequest)
	if r.json(t)["error"] != "password is managed by LDAP" {
		t.Fatalf("%s", r.body)
	}
	// Tokens: create, list, use, delete.
	r = e.req(t, "POST", "/-/api/tokens", "", "", alice, true)
	wantStatus(t, r, http.StatusCreated)
	m = r.json(t)
	id, tok := m["id"].(string), m["token"].(string)
	if m["user"] != "ldap:alice" {
		t.Fatalf("create: %s", r.body)
	}
	if st := e.bearer(t, "/-/api/realms", tok); st != http.StatusOK {
		t.Fatalf("LDAP admin token: %d", st)
	}
	r = e.req(t, "GET", "/-/api/tokens", "", "", e.loginCookie(t, "bob", "bob-pw"), false)
	if strings.Contains(r.body, id) {
		t.Fatalf("bob sees alice's token: %s", r.body)
	}
	bobCookie := e.loginCookie(t, "bob", "bob-pw")
	r = e.req(t, "POST", "/-/api/tokens", "", "", bobCookie, true)
	wantStatus(t, r, http.StatusCreated)
	bobID := r.json(t)["id"].(string)
	r = e.req(t, "GET", "/-/api/tokens", "", "", bobCookie, false)
	if !strings.Contains(r.body, bobID) || strings.Contains(r.body, id) || !strings.Contains(r.body, `"user":"ldap:bob"`) {
		t.Fatalf("bob's list: %s", r.body)
	}
	// The token page keeps the tokens whose user is /me's owner, not its user.
	if me := e.req(t, "GET", "/-/api/me", "", "", bobCookie, false).json(t); me["owner"] != "ldap:bob" || me["user"] != "bob" {
		t.Fatalf("bob's me: %v", me)
	}
	wantStatus(t, e.req(t, "DELETE", "/-/api/tokens/"+id, "", "", bobCookie, true), http.StatusNotFound)
	wantStatus(t, e.req(t, "DELETE", "/-/api/tokens/"+bobID, "", "", bobCookie, true), http.StatusNoContent)
	// A local user named bob does not see or own LDAP bob's tokens.
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"bob","password":"bobs-local-pw"}`), http.StatusCreated)
	r = e.req(t, "POST", "/-/api/tokens", "", "", bobCookie, true)
	wantStatus(t, r, http.StatusCreated)
	bobID = r.json(t)["id"].(string)
	if r := e.basicAs(t, "bob", "bobs-local-pw", "GET", "/-/api/tokens", ""); strings.Contains(r.body, bobID) {
		t.Fatalf("local bob sees LDAP bob's token: %s", r.body)
	}
	wantStatus(t, e.basicAs(t, "bob", "bobs-local-pw", "DELETE", "/-/api/tokens/"+bobID, ""), http.StatusNotFound)
	// The user list shows LDAP principals.
	r = e.admin(t, "GET", "/-/api/users", "")
	if !strings.Contains(r.body, `"realm":"LDAP"`) || !strings.Contains(r.body, `"userId":"alice"`) {
		t.Fatalf("users: %s", r.body)
	}
	// The ldap realm off: sessions, Basic and tokens of LDAP users stop.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":false}]}`), http.StatusOK)
	if r := e.req(t, "GET", "/-/api/me", "", "", alice, false); r.json(t)["anonymous"] != true {
		t.Fatalf("session after realm off: %s", r.body)
	}
	if st := e.bearer(t, "/-/api/realms", tok); st != http.StatusUnauthorized {
		t.Fatalf("token after realm off: %d", st)
	}
	if r := e.basicAs(t, "alice", "alice-pw", "GET", "/-/api/me", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("basic after realm off: %d", r.status)
	}
}

func TestLDAPDisabledOverrideRowDenied(t *testing.T) {
	e, s := ldapEnv(t)
	configureLDAP(t, e, s)
	alice := e.loginCookie(t, "alice", "alice-pw")
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"alice","source":"ldap","status":"disabled"}`), http.StatusCreated)
	// The session ends (creating the row is a change of the user) and login is refused.
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/alice", `{"status":"disabled","roles":[]}`), http.StatusOK)
	if r := e.req(t, "GET", "/-/api/me", "", "", alice, false); r.json(t)["anonymous"] != true {
		t.Fatalf("session of a disabled LDAP user: %s", r.body)
	}
	wantStatus(t, e.req(t, "POST", "/-/api/login", `{"user":"alice","password":"alice-pw"}`, "", nil, true), http.StatusUnauthorized)
	// An active override row adds roles.
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/bob", `{"status":"active","roles":[]}`), http.StatusNotFound)
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"bob","source":"ldap","roles":["admin"]}`), http.StatusCreated)
	r := e.basicAs(t, "bob", "bob-pw", "GET", "/-/api/me", "")
	if r.json(t)["admin"] != true {
		t.Fatalf("override roles: %s", r.body)
	}
	// An override row is not a principal: no session or token of its own.
	wantStatus(t, e.admin(t, "POST", "/-/api/tokens", `{"user":"bob"}`), http.StatusBadRequest)
	if r := e.req(t, "GET", "/-/api/me", "", "", e.cookieFor("bob"), false); r.json(t)["anonymous"] != true {
		t.Fatalf("bare session of an override row: %s", r.body)
	}
}

// I1 (Task 2 carry-over): an already locked-out system must not let the
// last restorable admin go.
func TestLockoutRestorableAdmins(t *testing.T) {
	e, s := ldapEnv(t)
	configureLDAP(t, e, s)
	admin := e.cookieFor("admin")
	as := func(method, path, body string) resp { return e.req(t, method, path, body, "", admin, true) }
	for name, realms := range map[string][]meta.Realm{
		// local off, ldap admin mapping usable: not locked out, but the
		// last local admin is the only way back if LDAP fails.
		"ldap usable": {{ID: "local", Enabled: false}, {ID: "token", Enabled: true}, {ID: "ldap", Enabled: true}},
		// local off, ldap off: locked out already.
		"locked out": {{ID: "local", Enabled: false}, {ID: "token", Enabled: true}, {ID: "ldap", Enabled: false}},
	} {
		t.Run(name, func(t *testing.T) {
			e.setRealmsDB(t, realms)
			for what, r := range map[string]resp{
				"delete":  as("DELETE", "/-/api/users/admin", ""),
				"disable": as("PUT", "/-/api/users/admin", `{"roles":["admin"],"status":"disabled"}`),
				"demote":  as("PUT", "/-/api/users/admin", `{"roles":[],"status":"active"}`),
			} {
				if r.status != http.StatusConflict {
					t.Errorf("%s: %d %s", what, r.status, r.body)
				}
			}
			// Unrelated changes still work.
			wantStatus(t, as("PUT", "/-/api/users/ci", `{"roles":["deployer"],"status":"active"}`), http.StatusOK)
		})
	}
	// A truly empty system (no restorable admin) can still be fixed.
	e.setRealmsDB(t, []meta.Realm{{ID: "local", Enabled: false}, {ID: "token", Enabled: true}})
	u, err := e.db.GetUser(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	u.PasswordHash = ""
	if err := e.db.UpdateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if err := e.ui.Auth.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	admin = e.cookieFor("admin") // the reload ended the sessions of admin, whose password changed (QA N-2)
	wantStatus(t, as("PUT", "/-/api/users/ci", `{"roles":["deployer","admin"],"status":"active"}`), http.StatusOK)
	wantStatus(t, as("POST", "/-/api/users", `{"id":"rescue","password":"longenough","roles":["admin"]}`), http.StatusCreated)
	// ci and rescue are restorable now: removing one is fine, both is not.
	wantStatus(t, as("PUT", "/-/api/users/ci", `{"roles":["deployer"],"status":"active"}`), http.StatusOK)
	wantStatus(t, as("DELETE", "/-/api/users/rescue", ""), http.StatusConflict)
}

// A local user's own password change proves the local password: the LDAP
// password of a same-named LDAP user must not do.
func TestOwnPasswordChangeNeedsLocalPassword(t *testing.T) {
	e, s := ldapEnv(t)
	configureLDAP(t, e, s)
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"alice","password":"alice-local-pw"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"ldap","enabled":true},{"id":"local","enabled":true},{"id":"token","enabled":true}]}`), http.StatusOK)
	local := e.cookieFor("alice") // a local session of alice
	r := e.req(t, "PUT", "/-/api/account/password", `{"currentPassword":"alice-pw","newPassword":"taken-over-pw"}`, "", local, true)
	wantStatus(t, r, http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/account/password", `{"currentPassword":"alice-local-pw","newPassword":"changed-pw-1"}`, "", local, true), http.StatusNoContent)
}

// The stored bind password never follows the configuration to another
// server or over a weaker transport without being re-entered (I2).
func TestLDAPBindPasswordNotCarriedToAnotherServer(t *testing.T) {
	e, s := ldapEnv(t)
	const msg = "re-enter the bind password when changing the server or weakening TLS"
	put := func(url, extra string, withPassword bool) resp {
		b := ldapBody(url, extra)
		if !withPassword {
			b = strings.Replace(b, `"bindPassword":"`+uiSvcPW+`"`, `"bindPassword":""`, 1)
		}
		return e.admin(t, "PUT", "/-/api/realms/ldap", b)
	}
	host, port, _ := strings.Cut(strings.TrimPrefix(s.URL("ldap"), "ldap://"), ":")
	wantStatus(t, put("ldaps://"+host+":"+port, `,"startTLS":false`, true), http.StatusOK)
	for name, r := range map[string]resp{
		"host":        put("ldaps://evil.example.net:"+port, "", false),
		"port":        put("ldaps://"+host+":1", "", false),
		"ldaps→ldap":  put("ldap://"+host+":"+port, "", false),
		"insecure on": put("ldaps://"+host+":"+port, `,"insecureSkipVerify":true`, false),
	} {
		if r.status != http.StatusBadRequest || r.json(t)["error"] != msg {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// StartTLS on -> off.
	wantStatus(t, put("ldap://"+host+":"+port, `,"startTLS":true`, true), http.StatusOK)
	if r := put("ldap://"+host+":"+port, "", false); r.status != http.StatusBadRequest || r.json(t)["error"] != msg {
		t.Errorf("starttls off: %d %s", r.status, r.body)
	}
	// Unrelated changes keep it; the default port is the same server.
	wantStatus(t, put("ldap://"+host+":"+port, `,"startTLS":true,"userFilter":"(cn={user})"`, false), http.StatusOK)
	wantStatus(t, put("ldap://"+host+":389", `,"startTLS":true`, true), http.StatusOK)
	wantStatus(t, put("ldap://"+host, `,"startTLS":true`, false), http.StatusOK)
	set, _ := e.db.LoadSettings(context.Background())
	if set.LDAP.BindPassword != uiSvcPW {
		t.Fatal("password lost")
	}
	// Re-entered, it may move.
	wantStatus(t, put("ldap://other.example.org", "", true), http.StatusOK)
}

// The general-settings PUT writes only its own keys: a stale read must not
// bring back realms, LDAP or SMTP values changed meanwhile (m4).
func TestGeneralSettingsWriteOnlyTheirKeys(t *testing.T) {
	e, s := ldapEnv(t)
	configureLDAP(t, e, s)
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", smtpBody), http.StatusOK)
	e.ui.settingsLoaded = func() {
		// Between the handler's read and its write: realms change, the
		// LDAP bind password and the SMTP password are cleared.
		ctx := context.Background()
		if err := e.db.SetSettingJSON(ctx, meta.SettingRealms, []meta.Realm{{ID: "local", Enabled: true}, {ID: "token", Enabled: false}}); err != nil {
			t.Error(err)
		}
		cur, _ := e.db.LoadSettings(ctx)
		cur.LDAP.BindDN, cur.LDAP.BindPassword = "", ""
		cur.SMTP.Password = ""
		if err := e.db.SetSettingJSON(ctx, meta.SettingLDAP, cur.LDAP); err != nil {
			t.Error(err)
		}
		if err := e.db.SetSettingJSON(ctx, meta.SettingSMTP, cur.SMTP); err != nil {
			t.Error(err)
		}
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"maxUploadMiB":7}`), http.StatusOK)
	got, err := e.db.LoadSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxUploadMiB != 7 || len(got.Realms) != 2 || got.Realms[1].Enabled || got.LDAP.BindPassword != "" || got.SMTP.Password != "" {
		t.Fatalf("stale keys written back: realms %v ldap pw set %v smtp pw set %v", got.Realms, got.LDAP.BindPassword != "", got.SMTP.Password != "")
	}
}

// m6: the access log and events name an LDAP user "ldap:<id>", so local
// alice and LDAP alice are told apart.
func TestAuditIdentityOfLDAPUsers(t *testing.T) {
	e, s := ldapEnv(t)
	configureLDAP(t, e, s)
	var buf safeBuffer
	e.srv.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	sink := &repotest.Sink{}
	e.ui.Events = sink
	alice := e.loginCookie(t, "alice", "alice-pw")
	wantStatus(t, e.req(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":true}]}`, "", alice, true), http.StatusOK)
	wantStatus(t, e.basicAs(t, "alice", "alice-pw", "GET", "/-/api/me", ""), http.StatusOK)
	if !strings.Contains(buf.String(), `"user":"ldap:alice"`) {
		t.Fatalf("access log: %s", buf.String())
	}
	if evs := sink.Events(); len(evs) == 0 || evs[len(evs)-1].Actor != "ldap:alice" {
		t.Fatalf("events: %s", sink.String())
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *safeBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
