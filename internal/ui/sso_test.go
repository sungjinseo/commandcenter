package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"commandcenter/internal/meta"
	"commandcenter/internal/oidc/oidctest"
	"commandcenter/internal/repo/repotest"
)

// oidcEnv is an env with a fake provider configured and the oidc realm on;
// cc-admins maps to admin and org:devs to deployer.
func oidcEnv(t *testing.T, useTLS bool) (*env, *oidctest.Server) {
	t.Helper()
	e := newEnv(t, "http://repo.example")
	idp := oidctest.New(t, useTLS)
	idp.Claims = map[string]any{"preferred_username": "Alice", "groups": []string{"cc-admins"}, "email": "alice@example.org", "name": "Alice A"}
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", oidcBody(idp, "")), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"oidc","enabled":true}]}`), http.StatusOK)
	return e, idp
}

func oidcBody(idp *oidctest.Server, extra string) string {
	return oidcBodyCA(idp, idp.CACertPEM(), extra)
}

func oidcBodyCA(idp *oidctest.Server, caCert, extra string) string {
	ca, _ := json.Marshal(caCert)
	return fmt.Sprintf(`{"issuer":%q,"clientId":%q,"clientSecret":%q,"caCert":%s,"timeoutSeconds":5,
		"groupMappings":[{"group":"cc-admins","roles":["admin"]},{"group":"org:devs","roles":["deployer"]}]%s}`,
		idp.URL, idp.ClientID, idp.ClientSecret, ca, extra)
}

// ssoStart begins a login and returns the provider's redirect back to the
// callback (already pointed at the test server) and the state cookie.
func (e *env) ssoStart(t *testing.T, idp *oidctest.Server, next string) (callback string, state *http.Cookie) {
	t.Helper()
	path := "/-/sso/oidc/start"
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	r := e.req(t, "GET", path, "", "", nil, false)
	wantStatus(t, r, http.StatusFound)
	for _, c := range r.cookie {
		if c.Name == "cc_oidc_state" {
			state = c
		}
	}
	if state == nil || state.Value == "" || !state.HttpOnly || state.SameSite != http.SameSiteLaxMode || state.MaxAge != 600 {
		t.Fatalf("state cookie %+v", state)
	}
	loc := r.header.Get("Location")
	res, err := idp.Client().Get(loc)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("provider answered %d to %s", res.StatusCode, loc)
	}
	back := res.Header.Get("Location")
	if !strings.HasPrefix(back, "http://repo.example/-/sso/oidc/callback?") {
		t.Fatalf("provider redirects to %s", back)
	}
	return strings.TrimPrefix(back, "http://repo.example"), state
}

// ssoLogin runs the whole flow and returns the callback's response.
func (e *env) ssoLogin(t *testing.T, idp *oidctest.Server, next string) resp {
	t.Helper()
	cb, state := e.ssoStart(t, idp, next)
	return e.req(t, "GET", cb, "", "", state, false)
}

func sessionCookie(r resp) *http.Cookie {
	for _, c := range r.cookie {
		if c.Name == "cc_session" && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestSSOHappyPath(t *testing.T) {
	e, idp := oidcEnv(t, false)
	r := e.get(t, "/-/api/auth/sso", "")
	wantStatus(t, r, http.StatusOK)
	if r.body != `{"providers":[{"id":"oidc","name":"SSO","url":"/-/sso/oidc/start"}]}`+"\n" {
		t.Fatalf("providers: %s", r.body)
	}
	r = e.ssoLogin(t, idp, "#/repositories")
	wantStatus(t, r, http.StatusFound)
	if loc := r.header.Get("Location"); loc != "/#/repositories" {
		t.Fatalf("redirect to %q", loc)
	}
	sc := sessionCookie(r)
	if sc == nil {
		t.Fatalf("no session cookie: %v", r.cookie)
	}
	cleared := false
	for _, c := range r.cookie {
		cleared = cleared || (c.Name == "cc_oidc_state" && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("state cookie not cleared")
	}
	me := e.req(t, "GET", "/-/api/me", "", "", sc, false)
	wantStatus(t, me, http.StatusOK)
	m := me.json(t)
	if m["user"] != "alice" || m["owner"] != "oidc:alice" || m["realm"] != "OIDC" || m["admin"] != true || fmt.Sprint(m["roles"]) != "[admin]" {
		t.Fatalf("me: %s", me.body)
	}
	// The users screen lists the signed-in user under source oidc.
	users := e.admin(t, "GET", "/-/api/users", "")
	wantStatus(t, users, http.StatusOK)
	if !strings.Contains(users.body, `"id":"alice"`) || !strings.Contains(users.body, `"source":"oidc"`) || !strings.Contains(users.body, `"email":"alice@example.org"`) {
		t.Fatalf("users: %s", users.body)
	}
	// The authorization request carried PKCE and the nonce; the token
	// endpoint was called once.
	if n := len(idp.Authorizes); n != 1 || idp.Authorizes[0].Get("code_challenge_method") != "S256" || idp.Authorizes[0].Get("nonce") == "" || idp.TokenCalls != 1 {
		t.Fatalf("%d authorizations, %d token calls", n, idp.TokenCalls)
	}
	// A bad next is dropped.
	r = e.ssoLogin(t, idp, "javascript:alert(1)")
	wantStatus(t, r, http.StatusFound)
	if loc := r.header.Get("Location"); loc != "/" {
		t.Fatalf("redirect to %q", loc)
	}
}

func TestSSOTokensForOIDCUser(t *testing.T) {
	e, idp := oidcEnv(t, false)
	sc := sessionCookie(e.ssoLogin(t, idp, ""))
	r := e.req(t, "POST", "/-/api/tokens", "", "", sc, true)
	wantStatus(t, r, http.StatusCreated)
	tok, _ := r.json(t)["token"].(string)
	if r.json(t)["user"] != "oidc:alice" || tok == "" {
		t.Fatalf("token: %s", r.body)
	}
	if st := e.bearer(t, "/-/api/me", tok); st != http.StatusOK {
		t.Fatalf("bearer: %d", st)
	}
	wantStatus(t, e.basicWith(t, "alice", tok, "GET", "/-/api/me", ""), http.StatusOK)
	wantStatus(t, e.basicWith(t, "bob", tok, "GET", "/-/api/me", ""), http.StatusUnauthorized)
	// An admin cannot mint a token for the provider user's override row.
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"alice","source":"oidc"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/tokens", `{"user":"alice"}`), http.StatusBadRequest)
	// Revoking the user's tokens covers the oidc owner.
	r = e.admin(t, "DELETE", "/-/api/users/alice/tokens", "")
	wantStatus(t, r, http.StatusOK)
	if fmt.Sprint(r.json(t)["revoked"]) != "1" {
		t.Fatalf("revoked: %s", r.body)
	}
	if st := e.bearer(t, "/-/api/me", tok); st != http.StatusUnauthorized {
		t.Fatalf("after revoke: %d", st)
	}
}

func TestSSOStateChecks(t *testing.T) {
	e, idp := oidcEnv(t, false)
	// Wrong state cookie.
	cb, state := e.ssoStart(t, idp, "")
	wrong := &http.Cookie{Name: state.Name, Value: "other"}
	wantStatus(t, e.req(t, "GET", cb, "", "", wrong, false), http.StatusBadRequest)
	// No cookie.
	wantStatus(t, e.req(t, "GET", cb, "", "", nil, false), http.StatusBadRequest)
	// The entry is still there: the right cookie works once, then the
	// state (and with it the code) is spent.
	wantStatus(t, e.req(t, "GET", cb, "", "", state, false), http.StatusFound)
	r := e.req(t, "GET", cb, "", "", state, false)
	wantStatus(t, r, http.StatusBadRequest)
	if sessionCookie(r) != nil {
		t.Fatal("a session for a reused state")
	}
	// A spent code under a fresh state is refused by the provider.
	cb2, state2 := e.ssoStart(t, idp, "")
	q1, _ := url.ParseQuery(strings.TrimPrefix(cb, "/-/sso/oidc/callback?"))
	q2, _ := url.ParseQuery(strings.TrimPrefix(cb2, "/-/sso/oidc/callback?"))
	q2.Set("code", q1.Get("code"))
	r = e.req(t, "GET", "/-/sso/oidc/callback?"+q2.Encode(), "", "", state2, false)
	wantStatus(t, r, http.StatusBadRequest)
	if sessionCookie(r) != nil {
		t.Fatal("a session for a reused code")
	}
	// The provider's error answer.
	_, state3 := e.ssoStart(t, idp, "")
	r = e.req(t, "GET", "/-/sso/oidc/callback?error=access_denied&error_description=nope&state="+state3.Value, "", "", state3, false)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "access_denied") || !strings.Contains(r.header.Get("Content-Type"), "text/html") {
		t.Fatalf("error page: %s", r.body)
	}
}

func TestSSOTokenChecks(t *testing.T) {
	e, idp := oidcEnv(t, false)
	for name, mut := range map[string]func(){
		"bad signature": func() { idp.WrongKey = true },
		"wrong iss":     func() { idp.Claims["iss"] = "https://other.example" },
		"wrong aud":     func() { idp.Claims["aud"] = "other" },
		"wrong nonce":   func() { idp.Claims["nonce"] = "x" },
		"expired":       func() { idp.Expired = true },
		"bad username":  func() { idp.Claims["preferred_username"] = "no spaces!" },
	} {
		idp.Lock()
		idp.WrongKey, idp.Expired = false, false
		idp.Claims = map[string]any{"preferred_username": "alice", "groups": []string{"cc-admins"}}
		mut()
		idp.Unlock()
		r := e.ssoLogin(t, idp, "")
		if r.status != http.StatusBadRequest || sessionCookie(r) != nil {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	idp.Lock()
	idp.WrongKey, idp.Expired = false, false
	idp.Claims = map[string]any{"preferred_username": "alice", "groups": []string{"cc-admins"}}
	idp.Unlock()
	wantStatus(t, e.ssoLogin(t, idp, ""), http.StatusFound)
	// ES256 works too.
	idp.Lock()
	idp.Alg = "ES256"
	idp.Unlock()
	wantStatus(t, e.ssoLogin(t, idp, ""), http.StatusFound)
}

func TestSSOGroupsAndOverride(t *testing.T) {
	e, idp := oidcEnv(t, false)
	idp.Claims = map[string]any{"preferred_username": "bob", "groups": []string{"org:devs", "other"}}
	sc := sessionCookie(e.ssoLogin(t, idp, ""))
	me := e.req(t, "GET", "/-/api/me", "", "", sc, false)
	if m := me.json(t); m["user"] != "bob" || m["admin"] != false || fmt.Sprint(m["roles"]) != "[deployer]" {
		t.Fatalf("me: %s", me.body)
	}
	// A deployer may upload to maven-releases, as the role says.
	rp := e.req(t, "GET", "/-/api/repositories/maven-releases", "", "", sc, false)
	wantStatus(t, rp, http.StatusOK)
	if rp.json(t)["canWrite"] != true {
		t.Fatalf("deployer cannot write: %s", rp.body)
	}
	// An override row adds a role; disabled, it shuts the user out (403
	// at login, and the live session ends with the user change).
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"bob","source":"oidc","roles":["admin"]}`), http.StatusCreated)
	me = e.req(t, "GET", "/-/api/me", "", "", sc, false)
	wantStatus(t, me, http.StatusOK)
	if m := me.json(t); m["admin"] != true {
		t.Fatalf("with override: %s", me.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/bob", `{"roles":[],"status":"disabled"}`), http.StatusOK)
	me = e.req(t, "GET", "/-/api/me", "", "", sc, false)
	if me.json(t)["anonymous"] != true {
		t.Fatalf("disabled override still signed in: %s", me.body)
	}
	r := e.ssoLogin(t, idp, "")
	wantStatus(t, r, http.StatusForbidden)
	if sessionCookie(r) != nil {
		t.Fatal("session for a disabled user")
	}
	// A group without a mapping and no default roles: signed in, no roles.
	idp.Claims = map[string]any{"preferred_username": "carol", "groups": []string{"nobody"}}
	sc = sessionCookie(e.ssoLogin(t, idp, ""))
	if m := e.req(t, "GET", "/-/api/me", "", "", sc, false).json(t); m["user"] != "carol" || fmt.Sprint(m["roles"]) != "[]" {
		t.Fatalf("carol: %v", m)
	}
}

func TestSSODisabledRealm(t *testing.T) {
	e, _ := oidcEnv(t, false)
	e.setRealmsDB(t, []meta.Realm{{ID: meta.RealmLocal, Enabled: true}, {ID: meta.RealmToken, Enabled: true}, {ID: meta.RealmOIDC, Enabled: false}})
	r := e.get(t, "/-/api/auth/sso", "")
	wantStatus(t, r, http.StatusOK)
	if r.body != `{"providers":[]}`+"\n" {
		t.Fatalf("providers: %s", r.body)
	}
	wantStatus(t, e.req(t, "GET", "/-/sso/oidc/start", "", "", nil, false), http.StatusNotFound)
	wantStatus(t, e.req(t, "GET", "/-/sso/oidc/callback?code=x&state=y", "", "", nil, false), http.StatusNotFound)
	// Enabling the realm needs a configuration.
	e2 := newEnv(t, "http://repo.example")
	r = e2.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"oidc","enabled":true}]}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "issuer, clientId") {
		t.Fatalf("error: %s", r.body)
	}
}

func TestOIDCSettingsAPI(t *testing.T) {
	e, idp := oidcEnv(t, false)
	sink := &repotest.Sink{}
	e.ui.Events = sink
	r := e.admin(t, "GET", "/-/api/realms/oidc", "")
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if strings.Contains(r.body, idp.ClientSecret) || strings.Contains(r.body, "clientSecret\"") || m["clientSecretSet"] != true ||
		m["issuer"] != idp.URL || m["usernameClaim"] != "preferred_username" || m["groupsClaim"] != "groups" || m["displayName"] != "SSO" ||
		fmt.Sprint(m["scopes"]) != "[openid profile email groups]" || fmt.Sprint(m["timeoutSeconds"]) != "5" {
		t.Fatalf("GET: %s", r.body)
	}
	wantStatus(t, e.get(t, "/-/api/realms/oidc", "ci"), http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/realms/oidc", oidcBody(idp, ""), "admin", nil, false), http.StatusForbidden)
	// An empty secret keeps the stored one; a change of other fields is
	// named in the event without values.
	keep := strings.Replace(oidcBody(idp, `,"displayName":"Corp login"`), `"clientSecret":"`+idp.ClientSecret+`"`, `"clientSecret":""`, 1)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", keep), http.StatusOK)
	set, _ := e.db.LoadSettings(context.Background())
	if set.OIDC.ClientSecret != idp.ClientSecret || set.OIDC.DisplayName != "Corp login" {
		t.Fatalf("stored %+v", set.OIDC)
	}
	evs := sink.Events()
	ev := evs[len(evs)-1]
	if ev.Kind != "realms.oidc.update" || ev.Detail != "displayName" || strings.Contains(ev.Detail, idp.ClientSecret) {
		t.Fatalf("event %+v", ev)
	}
	if body := e.get(t, "/-/api/auth/sso", "").body; !strings.Contains(body, `"name":"Corp login"`) {
		t.Fatalf("providers: %s", body)
	}
	// Re-enter rule: another issuer, or verification turned off, needs the
	// secret again; sending it passes.
	other := strings.Replace(keep, idp.URL, "https://idp2.example", 1)
	r = e.admin(t, "PUT", "/-/api/realms/oidc", other)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, secretReenterMessage) {
		t.Fatalf("error: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", strings.Replace(keep, `"timeoutSeconds":5`, `"timeoutSeconds":5,"insecureSkipVerify":true`, 1)), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", oidcBody(idp, `,"insecureSkipVerify":true`)), http.StatusOK)
	// Validation: an unknown role, a bad issuer, a bad CA.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", oidcBody(idp, `,"defaultRoles":["nope"]`)), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", strings.Replace(oidcBody(idp, ""), idp.URL, "http://idp.example", 1)), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", oidcBodyCA(idp, "not pem", "")), http.StatusBadRequest)
	// The realm is on, so the issuer cannot be blanked.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", strings.Replace(oidcBody(idp, ""), idp.URL, "", 1)), http.StatusBadRequest)
	// Clearing the secret.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", strings.Replace(keep, `"clientSecret":""`, `"clientSecret":"","clearClientSecret":true`, 1)), http.StatusOK)
	set, _ = e.db.LoadSettings(context.Background())
	if set.OIDC.ClientSecret != "" {
		t.Fatal("not cleared")
	}
	// A role named by the mappings cannot be deleted.
	wantStatus(t, e.admin(t, "DELETE", "/-/api/roles/deployer", ""), http.StatusConflict)
}

func TestOIDCTestEndpoint(t *testing.T) {
	e, idp := oidcEnv(t, false)
	r := e.admin(t, "POST", "/-/api/realms/oidc/test", oidcBody(idp, ""))
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if m["issuer"] != idp.URL || m["authorizationEndpoint"] != idp.URL+"/authorize" || m["tokenEndpoint"] != idp.URL+"/token" ||
		m["jwksUri"] != idp.URL+"/jwks" || fmt.Sprint(m["scopesSupported"]) != "[openid profile email groups]" || fmt.Sprint(m["algs"]) != "[RS256 ES256]" {
		t.Fatalf("test: %s", r.body)
	}
	idp.Issuer = "https://other.example"
	r = e.admin(t, "POST", "/-/api/realms/oidc/test", oidcBody(idp, ""))
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "issuer") {
		t.Fatalf("error: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/realms/oidc/test", `{"clientId":"x"}`), http.StatusBadRequest)
	wantStatus(t, e.req(t, "POST", "/-/api/realms/oidc/test", oidcBody(idp, ""), "ci", nil, true), http.StatusForbidden)
}

func TestSSOTLSCACert(t *testing.T) {
	e, idp := oidcEnv(t, true)
	// Without the CA the provider is not trusted: the test fails and the
	// login cannot start.
	noCA := oidcBodyCA(idp, "", "")
	wantStatus(t, e.admin(t, "POST", "/-/api/realms/oidc/test", noCA), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", noCA), http.StatusOK)
	wantStatus(t, e.req(t, "GET", "/-/sso/oidc/start", "", "", nil, false), http.StatusBadGateway)
	// With it, the whole flow works over TLS.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/oidc", oidcBody(idp, "")), http.StatusOK)
	wantStatus(t, e.admin(t, "POST", "/-/api/realms/oidc/test", oidcBody(idp, "")), http.StatusOK)
	r := e.ssoLogin(t, idp, "")
	wantStatus(t, r, http.StatusFound)
	if sessionCookie(r) == nil {
		t.Fatal("no session")
	}
}

func TestSSONeedsSessions(t *testing.T) {
	e, _ := oidcEnv(t, false)
	e.ui.Auth.Sessions = nil
	wantStatus(t, e.req(t, "GET", "/-/sso/oidc/start", "", "", nil, false), http.StatusNotFound)
	if body := e.get(t, "/-/api/auth/sso", "").body; body != `{"providers":[]}`+"\n" {
		t.Fatalf("providers: %s", body)
	}
}
