package ui

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/saml/samltest"
)

const samlACS = "http://repo.example/-/sso/saml/acs"
const samlEntity = "http://repo.example/-/sso/saml/metadata"

func samlBody(idp *samltest.IdP, extra string) string {
	md, _ := json.Marshal(idp.Metadata())
	return fmt.Sprintf(`{"idpMetadataXML":%s,"groupsAttribute":"groups","emailAttribute":"email","firstNameAttribute":"givenName","lastNameAttribute":"sn",
		"groupMappings":[{"group":"cc-admins","roles":["admin"]},{"group":"devs","roles":["deployer"]}]%s}`, md, extra)
}

// samlEnv is an env with a test IdP configured and the saml realm on.
func samlEnv(t *testing.T, extra string) (*env, *samltest.IdP) {
	t.Helper()
	e := newEnv(t, "http://repo.example")
	idp := samltest.New(t)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/saml", samlBody(idp, extra)), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"saml","enabled":true}]}`), http.StatusOK)
	return e, idp
}

// samlStart begins a login and returns the AuthnRequest's ID, the
// RelayState and the state cookie.
func (e *env) samlStart(t *testing.T, idp *samltest.IdP, query string) (reqID, relay string, state *http.Cookie, loc *url.URL) {
	t.Helper()
	r := e.req(t, "GET", "/-/sso/saml/login"+query, "", "", nil, false)
	wantStatus(t, r, http.StatusFound)
	for _, c := range r.cookie {
		if strings.HasSuffix(c.Name, "cc_saml_state") {
			state = c
		}
	}
	if state == nil || state.Value == "" || !state.HttpOnly || state.MaxAge != 600 {
		t.Fatalf("state cookie %+v", state)
	}
	loc, _ = url.Parse(r.header.Get("Location"))
	if loc.Scheme+"://"+loc.Host+loc.Path != idp.SSOURL {
		t.Fatalf("redirect to %s", loc)
	}
	q := loc.Query()
	relay = q.Get("RelayState")
	if relay != state.Value {
		t.Fatalf("RelayState %q, cookie %q", relay, state.Value)
	}
	raw, _ := base64.StdEncoding.DecodeString(q.Get("SAMLRequest"))
	x, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`ID="([^"]+)"`).FindSubmatch(x)
	if m == nil || !strings.Contains(string(x), `AssertionConsumerServiceURL="`+samlACS+`"`) || !strings.Contains(string(x), `Destination="`+idp.SSOURL+`"`) ||
		!strings.Contains(string(x), "<saml:Issuer>"+samlEntity+"</saml:Issuer>") {
		t.Fatalf("AuthnRequest %s", x)
	}
	return string(m[1]), relay, state, loc
}

// acs posts a SAMLResponse from the IdP's site.
func (e *env) acs(t *testing.T, doc, relay string, cookie *http.Cookie) resp {
	t.Helper()
	form := url.Values{"SAMLResponse": {samltest.Encode(doc)}}
	if relay != "" {
		form.Set("RelayState", relay)
	}
	r, _ := http.NewRequest("POST", e.ts.URL+"/-/sso/saml/acs", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://idp.example") // a cross-site form post, as browsers send it
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: string(b), cookie: res.Cookies()}
}

// samlResponse is a signed Response to reqID for p's mutation of the defaults.
func samlResponse(t *testing.T, idp *samltest.IdP, reqID string, mut func(*samltest.Params)) string {
	t.Helper()
	p := idp.Defaults(samlEntity, samlACS, reqID, time.Now())
	if mut != nil {
		mut(&p)
	}
	return idp.Sign(t, samltest.Response(p, idp.Sign(t, samltest.Assertion(p), p.AssertionID, samltest.SignOpts{})), p.ResponseID, samltest.SignOpts{})
}

func (e *env) samlLogin(t *testing.T, idp *samltest.IdP, mut func(*samltest.Params)) resp {
	t.Helper()
	reqID, relay, state, _ := e.samlStart(t, idp, "")
	return e.acs(t, samlResponse(t, idp, reqID, mut), relay, state)
}

func TestSAMLBrowserFlow(t *testing.T) {
	e, idp := samlEnv(t, "")
	r := e.get(t, "/-/api/auth/sso", "")
	if r.body != `{"providers":[{"id":"saml","name":"SAML","url":"/-/sso/saml/login"}]}`+"\n" {
		t.Fatalf("providers: %s", r.body)
	}
	reqID, relay, state, _ := e.samlStart(t, idp, "?next="+url.QueryEscape("#/repositories"))
	if state.Name != "cc_saml_state" || state.SameSite != 0 { // no SameSite attribute
		t.Fatalf("http state cookie %+v", state)
	}
	doc := samlResponse(t, idp, reqID, nil)
	r = e.acs(t, doc, relay, state)
	wantStatus(t, r, http.StatusSeeOther)
	if loc := r.header.Get("Location"); loc != "/#/repositories" {
		t.Fatalf("redirect to %q", loc)
	}
	sc := sessionCookie(r)
	if sc == nil {
		t.Fatalf("no session cookie: %v", r.cookie)
	}
	cleared := false
	for _, c := range r.cookie {
		cleared = cleared || (c.Name == "cc_saml_state" && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("state cookie not cleared")
	}
	me := e.req(t, "GET", "/-/api/me", "", "", sc, false)
	m := me.json(t)
	if m["user"] != "alice" || m["owner"] != "saml:alice" || m["realm"] != "SAML" || m["admin"] != true || fmt.Sprint(m["roles"]) != "[admin]" {
		t.Fatalf("me: %s", me.body)
	}
	users := e.admin(t, "GET", "/-/api/users", "")
	if !strings.Contains(users.body, `"id":"alice"`) || !strings.Contains(users.body, `"source":"saml"`) ||
		!strings.Contains(users.body, `"email":"alice@example.org"`) || !strings.Contains(users.body, `"name":"Alice Liddell"`) {
		t.Fatalf("users: %s", users.body)
	}
	// The same response again: the state is spent.
	r = e.acs(t, doc, relay, state)
	if r.status != http.StatusBadRequest || sessionCookie(r) != nil || !strings.Contains(r.body, "already used") {
		t.Fatalf("reposted: %d %s", r.status, r.body)
	}
	// A "return" path under /ui/ is honoured, another origin is not.
	for ret, want := range map[string]string{"/ui/#/browse": "/ui/#/browse", "https://evil.example/": "/"} {
		reqID, relay, state, _ = e.samlStart(t, idp, "?return="+url.QueryEscape(ret))
		r = e.acs(t, samlResponse(t, idp, reqID, nil), relay, state)
		if loc := r.header.Get("Location"); loc != want {
			t.Fatalf("return %q: redirect to %q", ret, loc)
		}
	}
	// Tokens for the SAML user, revoked with its override row's tokens.
	tr := e.req(t, "POST", "/-/api/tokens", "", "", sc, true)
	wantStatus(t, tr, http.StatusCreated)
	tok, _ := tr.json(t)["token"].(string)
	if tr.json(t)["user"] != "saml:alice" || e.bearer(t, "/-/api/me", tok) != http.StatusOK {
		t.Fatalf("token: %s", tr.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"alice","source":"saml"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/tokens", `{"user":"alice"}`), http.StatusBadRequest)
	rv := e.admin(t, "DELETE", "/-/api/users/alice/tokens", "")
	if fmt.Sprint(rv.json(t)["revoked"]) != "1" || e.bearer(t, "/-/api/me", tok) != http.StatusUnauthorized {
		t.Fatalf("revoke: %s", rv.body)
	}
}

func TestSAMLReturnPaths(t *testing.T) {
	for in, want := range map[string]string{"#/x": "/#/x", "/ui/": "/ui/", "/ui/#/a": "/ui/#/a", "https://evil.example": "", "//evil.example": "",
		"/ui//evil": "", "/ui/../x": "", "/other": "", "/ui/\\x": "", "#\n": ""} {
		if got := samlReturn(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestSAMLRefusals(t *testing.T) {
	e, idp := samlEnv(t, "")
	refused := func(name string, r resp, want string) {
		t.Helper()
		if r.status == http.StatusSeeOther || sessionCookie(r) != nil || !strings.Contains(r.body, want) {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	reqID, relay, state, _ := e.samlStart(t, idp, "")
	doc := samlResponse(t, idp, reqID, nil)
	// No cookie, a wrong cookie: the state does not match (and IdP-initiated
	// logins are off).
	refused("no cookie", e.acs(t, doc, relay, nil), "state does not match")
	refused("wrong cookie", e.acs(t, doc, relay, &http.Cookie{Name: state.Name, Value: "x"}), "state does not match")
	refused("IdP-initiated", e.acs(t, samlResponse(t, idp, "", func(p *samltest.Params) { p.InResponseTo = "" }), "", nil), "not allowed")
	// The state survived the mismatches: the right cookie works once.
	wantStatus(t, e.acs(t, doc, relay, state), http.StatusSeeOther)
	// Each check reaches the browser as a 400 page.
	for name, c := range map[string]struct {
		mut  func(*samltest.Params)
		want string
	}{
		"expired": {func(p *samltest.Params) {
			p.Now, p.NotOnOrAfter = time.Now().Add(-time.Hour), time.Now().Add(-10*time.Minute)
		}, "expired"},
		"audience":       {func(p *samltest.Params) { p.Audience = "https://other.example" }, "audience"},
		"recipient":      {func(p *samltest.Params) { p.Recipient = "https://evil.example/acs" }, "recipient"},
		"InResponseTo":   {func(p *samltest.Params) { p.InResponseTo = "_other" }, "does not answer"},
		"bad username":   {func(p *samltest.Params) { p.NameID = "no spaces!" }, "unusable username"},
		"failure status": {func(p *samltest.Params) { p.Status = "urn:oasis:names:tc:SAML:2.0:status:Requester" }, "status:Requester"},
	} {
		r := e.samlLogin(t, idp, c.mut)
		if r.status != http.StatusBadRequest || !strings.Contains(r.header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d", name, r.status)
		}
		refused(name, r, c.want)
	}
	// Tampered after signing.
	reqID, relay, state, _ = e.samlStart(t, idp, "")
	refused("tampered", e.acs(t, strings.Replace(samlResponse(t, idp, reqID, nil), ">Alice<", ">Admin<", 1), relay, state), "does not verify")
	// Another IdP's key.
	other := samltest.New(t)
	other.EntityID = idp.EntityID
	reqID, relay, state, _ = e.samlStart(t, idp, "")
	refused("other key", e.acs(t, samlResponse(t, other, reqID, nil), relay, state), "not in the identity provider")
	// acs posts cross-site, which the CSRF guard lets through for the ACS
	// only: every other route still refuses such a post.
	req, _ := http.NewRequest("POST", e.ts.URL+"/-/api/login", strings.NewReader(`{}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://idp.example")
	res, _ := noRedirect.Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site login post: %d", res.StatusCode)
	}
}

func TestSAMLIdPInitiatedAndReplay(t *testing.T) {
	e, idp := samlEnv(t, `,"allowIdpInitiated":true`)
	doc := samlResponse(t, idp, "", func(p *samltest.Params) { p.InResponseTo = "" })
	r := e.acs(t, doc, "#/browse", nil)
	wantStatus(t, r, http.StatusSeeOther)
	if r.header.Get("Location") != "/#/browse" || sessionCookie(r) == nil {
		t.Fatalf("IdP-initiated: %v %v", r.header, r.cookie)
	}
	// The same assertion again: a replay.
	r = e.acs(t, doc, "", nil)
	if r.status != http.StatusBadRequest || sessionCookie(r) != nil || !strings.Contains(r.body, "already used") {
		t.Fatalf("replay: %d %s", r.status, r.body)
	}
	// An unsolicited response may not claim to answer a request.
	r = e.acs(t, samlResponse(t, idp, "_forged", nil), "", nil)
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "did not send") {
		t.Fatalf("unsolicited with InResponseTo: %d %s", r.status, r.body)
	}
	// SP-initiated logins still work.
	wantStatus(t, e.samlLogin(t, idp, nil), http.StatusSeeOther)
}

func TestSAMLGroupsAndOverride(t *testing.T) {
	e, idp := samlEnv(t, `,"usernameAttribute":"email"`)
	sc := sessionCookie(e.samlLogin(t, idp, func(p *samltest.Params) {
		p.Attributes = [][2]string{{"email", "Bob@Example.org"}, {"groups", "devs"}, {"groups", "other"}}
	}))
	me := e.req(t, "GET", "/-/api/me", "", "", sc, false)
	if m := me.json(t); m["user"] != "bob@example.org" || m["admin"] != false || fmt.Sprint(m["roles"]) != "[deployer]" {
		t.Fatalf("me: %s", me.body)
	}
	rp := e.req(t, "GET", "/-/api/repositories/maven-releases", "", "", sc, false)
	if rp.json(t)["canWrite"] != true {
		t.Fatalf("deployer cannot write: %s", rp.body)
	}
	// An override row adds a role; disabled, it ends the session and refuses the login.
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"bob@example.org","source":"saml","roles":["admin"]}`), http.StatusCreated)
	if m := e.req(t, "GET", "/-/api/me", "", "", sc, false).json(t); m["admin"] != true {
		t.Fatalf("override: %v", m)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/users/bob@example.org", `{"roles":[],"status":"disabled"}`), http.StatusOK)
	if m := e.req(t, "GET", "/-/api/me", "", "", sc, false).json(t); m["anonymous"] != true {
		t.Fatalf("disabled still signed in: %v", m)
	}
	r := e.samlLogin(t, idp, func(p *samltest.Params) { p.Attributes = [][2]string{{"email", "bob@example.org"}} })
	if r.status != http.StatusForbidden || sessionCookie(r) != nil {
		t.Fatalf("disabled login: %d", r.status)
	}
	// No username attribute: refused.
	r = e.samlLogin(t, idp, func(p *samltest.Params) { p.Attributes = nil })
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "unusable username") {
		t.Fatalf("no username: %d %s", r.status, r.body)
	}
	// Default roles apply to a user without mapped groups.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/saml", samlBody(idp, `,"usernameAttribute":"email","defaultRoles":["deployer"]`)), http.StatusOK)
	sc = sessionCookie(e.samlLogin(t, idp, func(p *samltest.Params) { p.Attributes = [][2]string{{"email", "carol@example.org"}} }))
	if m := e.req(t, "GET", "/-/api/me", "", "", sc, false).json(t); m["user"] != "carol@example.org" || fmt.Sprint(m["roles"]) != "[deployer]" {
		t.Fatalf("carol: %v", m)
	}
}

func TestSAMLSettingsAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	// Before any configuration: defaults, no key, the realm cannot be enabled.
	r := e.admin(t, "GET", "/-/api/realms/saml", "")
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if m["enabled"] != false || m["usernameAttribute"] != "NameID" || m["validateResponseSignature"] != true || m["validateAssertionSignature"] != true ||
		fmt.Sprint(m["clockSkewSeconds"]) != "120" || m["spCertificate"] != "" || m["idp"] != nil || m["acsUrl"] != samlACS || m["effectiveEntityId"] != samlEntity {
		t.Fatalf("GET: %s", r.body)
	}
	r = e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"saml","enabled":true}]}`)
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "identity provider metadata") {
		t.Fatalf("enable unconfigured: %d %s", r.status, r.body)
	}
	if !strings.Contains(e.admin(t, "GET", "/-/api/realms", "").body, `"saml"`) {
		t.Fatal("saml is not an available realm")
	}
	wantStatus(t, e.req(t, "GET", "/-/sso/saml/metadata", "", "", nil, false), http.StatusNotFound)
	// Privileges.
	idp := samltest.New(t)
	wantStatus(t, e.get(t, "/-/api/realms/saml", "ci"), http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/realms/saml", samlBody(idp, ""), "admin", nil, false), http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/realms/saml", samlBody(idp, ""), "ci", nil, true), http.StatusForbidden)
	// Validation.
	for name, body := range map[string]string{
		"bad metadata":      `{"idpMetadataXML":"<x/>"}`,
		"dtd metadata":      `{"idpMetadataXML":"<!DOCTYPE x><x/>"}`,
		"both sources":      strings.Replace(samlBody(idp, ""), `{"idpMetadataXML"`, `{"idpMetadataURL":"https://idp.example/md","idpMetadataXML"`, 1),
		"http url":          `{"idpMetadataURL":"http://idp.example/md"}`,
		"no signatures":     samlBody(idp, `,"validateResponseSignature":false,"validateAssertionSignature":false`),
		"unknown role":      samlBody(idp, `,"defaultRoles":["nope"]`),
		"skew":              samlBody(idp, `,"clockSkewSeconds":3600`),
		"unknown field":     samlBody(idp, `,"spPrivateKey":"x"`),
		"bad entity":        samlBody(idp, `,"entityId":"not a uri"`),
		"mapping no roles":  samlBody(idp, `,"groupMappings":[{"group":"g","roles":[]}]`),
		"attribute control": samlBody(idp, `,"groupsAttribute":"a\nb"`),
	} {
		if r := e.admin(t, "PUT", "/-/api/realms/saml", body); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// A save: the key pair is generated and kept; the summary describes the IdP.
	r = e.admin(t, "PUT", "/-/api/realms/saml", samlBody(idp, `,"displayName":"Corp SAML","clockSkewSeconds":0`))
	wantStatus(t, r, http.StatusOK)
	m = r.json(t)
	idpm, _ := m["idp"].(map[string]any)
	certs, _ := idpm["certificates"].([]any)
	if idpm["entityId"] != idp.EntityID || idpm["ssoUrl"] != idp.SSOURL || len(certs) != 1 || m["idpError"] != "" ||
		certs[0].(map[string]any)["expired"] != false || !strings.Contains(fmt.Sprint(certs[0]), ":") || fmt.Sprint(m["clockSkewSeconds"]) != "0" {
		t.Fatalf("PUT: %s", r.body)
	}
	if strings.Contains(r.body, "PRIVATE KEY") || !strings.Contains(m["spCertificate"].(string), "BEGIN CERTIFICATE") || m["spCertificateInfo"] == nil {
		t.Fatalf("key material: %s", r.body)
	}
	set, _ := e.db.LoadSettings(context.Background())
	key1 := set.SAML.SPPrivateKey
	if !strings.Contains(key1, "PRIVATE KEY") || set.SAML.DisplayName != "Corp SAML" {
		t.Fatalf("stored %v", set.SAML)
	}
	evs := sink.Events()
	if ev := evs[len(evs)-1]; ev.Kind != "realms.saml.update" || !strings.Contains(ev.Detail, "idpMetadata") || !strings.Contains(ev.Detail, "spKey") ||
		strings.Contains(ev.Detail, "PRIVATE") {
		t.Fatalf("event %+v", ev)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/saml", samlBody(idp, `,"displayName":"Corp"`)), http.StatusOK)
	set, _ = e.db.LoadSettings(context.Background())
	if set.SAML.SPPrivateKey != key1 {
		t.Fatal("the key pair changed on a save")
	}
	if evs := sink.Events(); evs[len(evs)-1].Detail != "displayName, clockSkewSeconds" {
		t.Fatalf("event %+v", evs[len(evs)-1])
	}
	// Both metadata endpoints serve the SP metadata; the public one also when the realm is off.
	pub := e.req(t, "GET", "/-/sso/saml/metadata", "", "", nil, false)
	adm := e.admin(t, "GET", "/-/api/realms/saml/sp-metadata", "")
	wantStatus(t, pub, http.StatusOK)
	if pub.body != adm.body || pub.header.Get("Content-Type") != "application/samlmetadata+xml" ||
		!strings.Contains(pub.body, `entityID="`+samlEntity+`"`) || !strings.Contains(pub.body, `Location="`+samlACS+`"`) || !strings.Contains(pub.body, `use="encryption"`) {
		t.Fatalf("metadata: %s", pub.body)
	}
	wantStatus(t, e.get(t, "/-/api/realms/saml/sp-metadata", "ci"), http.StatusForbidden)
	// The realm on: the metadata cannot be removed, a mapped role not deleted.
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"saml","enabled":true}]}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/saml", `{}`), http.StatusBadRequest)
	if r := e.admin(t, "DELETE", "/-/api/roles/deployer", ""); r.status != http.StatusConflict || !strings.Contains(r.body, "SAML") {
		t.Fatalf("role delete: %d %s", r.status, r.body)
	}
	if body := e.get(t, "/-/api/auth/sso", "").body; !strings.Contains(body, `"name":"Corp"`) {
		t.Fatalf("providers: %s", body)
	}
	if m := e.admin(t, "GET", "/-/api/realms/saml", "").json(t); m["enabled"] != true {
		t.Fatalf("enabled: %v", m)
	}
}

func TestSAMLMetadataURLAndSignedRequests(t *testing.T) {
	idp := samltest.New(t)
	fetches := 0
	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		io.WriteString(w, idp.Metadata())
	}))
	defer mdSrv.Close()
	e, _ := samlEnv(t, "") // pasted first, then the URL
	md, _ := json.Marshal(mdSrv.URL + "/md")
	body := fmt.Sprintf(`{"idpMetadataURL":%s,"groupsAttribute":"groups","signAuthnRequests":true,"groupMappings":[{"group":"cc-admins","roles":["admin"]}]}`, md)
	r := e.admin(t, "PUT", "/-/api/realms/saml", body)
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	idpm, _ := m["idp"].(map[string]any)
	if idpm["entityId"] != idp.EntityID || idpm["fetchedAt"] == "" || fetches != 1 {
		t.Fatalf("PUT with URL (%d fetches): %s", fetches, r.body)
	}
	// The AuthnRequest is signed with the published SP certificate.
	_, _, _, loc := e.samlStart(t, idp, "")
	if fetches != 1 {
		t.Fatalf("the metadata was fetched again: %d", fetches)
	}
	raw := loc.RawQuery
	signed := raw[:strings.Index(raw, "&Signature=")]
	sig, _ := base64.StdEncoding.DecodeString(loc.Query().Get("Signature"))
	blk, _ := pem.Decode([]byte(m["spCertificate"].(string)))
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(cert.PublicKey.(*rsa.PublicKey), crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("AuthnRequest signature: %v", err)
	}
	if md := e.req(t, "GET", "/-/sso/saml/metadata", "", "", nil, false).body; !strings.Contains(md, `AuthnRequestsSigned="true"`) || !strings.Contains(md, `use="signing"`) {
		t.Fatalf("metadata: %s", md)
	}
	wantStatus(t, e.samlLogin(t, idp, nil), http.StatusSeeOther)
	// An unreachable URL is reported, not fatal to the save.
	mdSrv.Close()
	r = e.admin(t, "PUT", "/-/api/realms/saml", strings.Replace(body, "/md", "/gone", 1))
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["idp"] != nil || m["idpError"] == "" {
		t.Fatalf("unreachable: %s", r.body)
	}
	wantStatus(t, e.req(t, "GET", "/-/sso/saml/login", "", "", nil, false), http.StatusServiceUnavailable)
}

func TestSAMLEncryptedAndHTTPS(t *testing.T) {
	e := newEnv(t, "https://repo.example")
	idp := samltest.New(t)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms/saml", samlBody(idp, "")), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"saml","enabled":true}]}`), http.StatusOK)
	r := e.req(t, "GET", "/-/sso/saml/login", "", "", nil, false)
	wantStatus(t, r, http.StatusFound)
	var state *http.Cookie
	for _, c := range r.cookie {
		if c.Name == "__Host-cc_saml_state" {
			state = c
		}
	}
	if state == nil || !state.Secure || state.SameSite != http.SameSiteNoneMode || state.Path != "/" {
		t.Fatalf("https state cookie: %v", r.cookie)
	}
	loc, _ := url.Parse(r.header.Get("Location"))
	raw, _ := base64.StdEncoding.DecodeString(loc.Query().Get("SAMLRequest"))
	x, _ := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	reqID := string(regexp.MustCompile(`ID="([^"]+)"`).FindSubmatch(x)[1])
	// An assertion encrypted to the SP certificate from the API.
	blk, _ := pem.Decode([]byte(e.admin(t, "GET", "/-/api/realms/saml", "").json(t)["spCertificate"].(string)))
	cert, _ := x509.ParseCertificate(blk.Bytes)
	p := idp.Defaults("https://repo.example/-/sso/saml/metadata", "https://repo.example/-/sso/saml/acs", reqID, time.Now())
	enc := samltest.Encrypt(t, idp.Sign(t, samltest.Assertion(p), p.AssertionID, samltest.SignOpts{}), cert, samltest.AES256CBC, false)
	res := e.acs(t, samltest.Response(p, enc), loc.Query().Get("RelayState"), state)
	wantStatus(t, res, http.StatusSeeOther)
	var sc *http.Cookie
	for _, c := range res.cookie {
		if c.Name == "__Host-cc_session" && c.Value != "" {
			sc = c
		}
	}
	if sc == nil {
		t.Fatalf("no session: %v", res.cookie)
	}
	if m := e.req(t, "GET", "/-/api/me", "", "", sc, false).json(t); m["owner"] != "saml:alice" {
		t.Fatalf("me: %v", m)
	}
}

func TestSAMLDisabledRealm(t *testing.T) {
	e, idp := samlEnv(t, "")
	reqID, relay, state, _ := e.samlStart(t, idp, "")
	e.setRealmsDB(t, []meta.Realm{{ID: meta.RealmLocal, Enabled: true}, {ID: meta.RealmToken, Enabled: true}, {ID: meta.RealmSAML, Enabled: false}})
	if body := e.get(t, "/-/api/auth/sso", "").body; body != `{"providers":[]}`+"\n" {
		t.Fatalf("providers: %s", body)
	}
	wantStatus(t, e.req(t, "GET", "/-/sso/saml/login", "", "", nil, false), http.StatusNotFound)
	wantStatus(t, e.acs(t, samlResponse(t, idp, reqID, nil), relay, state), http.StatusNotFound)
	e.ui.Auth.Sessions = nil
	wantStatus(t, e.req(t, "GET", "/-/sso/saml/login", "", "", nil, false), http.StatusNotFound)
}
