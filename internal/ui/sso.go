package ui

import (
	"errors"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/oidc"
)

// OIDC browser login (the oidc realm). GET /-/sso/oidc/start sends the
// browser to the provider with a state, a nonce and a PKCE verifier kept in
// memory under the state; the provider returns it to /-/sso/oidc/callback,
// which redeems the code, verifies the ID token, records the identity in
// the authenticator and starts a session owned by "oidc:<username>".
//
// ponytail: the pending logins live in this process (single replica, like
// the sessions); a second replica would need them in the database.

const (
	ssoStateCookie     = "cc_oidc_state"
	ssoHostStateCookie = "__Host-" + ssoStateCookie
	ssoStateTTL        = 10 * time.Minute
	ssoStateMax        = 10000
	ssoNextMax         = 512
)

type ssoState struct {
	nonce, verifier, next string
	created               time.Time
}

// ssoStates holds the pending logins keyed by state.
type ssoStates struct {
	mu sync.Mutex
	m  map[string]ssoState
}

// put stores st under state after dropping expired entries; it fails when
// the store is full.
func (s *ssoStates) put(state string, st ssoState, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]ssoState{}
	}
	for k, v := range s.m {
		if now.Sub(v.created) > ssoStateTTL {
			delete(s.m, k)
		}
	}
	if len(s.m) >= ssoStateMax {
		return false
	}
	s.m[state] = st
	return true
}

// take returns and removes the entry of state; a missing or expired one is
// not found.
func (s *ssoStates) take(state string, now time.Time) (ssoState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[state]
	delete(s.m, state)
	if !ok || now.Sub(st.created) > ssoStateTTL {
		return ssoState{}, false
	}
	return st, true
}

func (u *UI) ssoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/sso/oidc/start", u.ssoStart)
	mux.HandleFunc("GET /-/sso/oidc/callback", u.ssoCallback)
}

// oidcProvider returns the client of the live OIDC configuration, rebuilt
// when the configuration changed since the last call.
func (u *UI) oidcProvider() (*oidc.Provider, error) {
	cfg := u.Auth.Snapshot().Settings().OIDC
	u.oidcMu.Lock()
	defer u.oidcMu.Unlock()
	if u.oidcProv != nil && oidcSame(u.oidcCfg, cfg) {
		return u.oidcProv, nil
	}
	p, err := oidc.New(cfg)
	if err != nil {
		return nil, err
	}
	u.oidcProv, u.oidcCfg = p, cfg
	return p, nil
}

// oidcSame reports whether two configurations build the same provider.
func oidcSame(a, b meta.OIDC) bool {
	return oidcChanged(a, b) == ""
}

// ssoUsable reports whether the oidc realm is on and configured.
func (u *UI) ssoUsable() bool {
	s := u.Auth.Snapshot()
	return s.RealmEnabled(meta.RealmOIDC) && s.Settings().OIDC.Complete()
}

// ssoProviders answers GET /-/api/auth/sso: the login buttons to show.
func (u *UI) ssoProviders(w http.ResponseWriter, r *http.Request) {
	providers := []map[string]string{}
	if u.ssoUsable() && u.Auth.Sessions != nil {
		name := strings.TrimSpace(u.Auth.Snapshot().Settings().OIDC.DisplayName)
		if name == "" {
			name = meta.DefaultOIDC().DisplayName
		}
		providers = append(providers, map[string]string{"id": meta.RealmOIDC, "name": name, "url": "/-/sso/oidc/start"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

// ssoCookieName is the state cookie's name: __Host- (Secure) under an https
// baseURL, like the session cookie.
func (u *UI) ssoCookieName() string {
	if u.Auth.SessionCookieName() == auth.HostSessionCookie {
		return ssoHostStateCookie
	}
	return ssoStateCookie
}

func (u *UI) ssoCookie(value string, maxAge int) *http.Cookie {
	name := u.ssoCookieName()
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: name == ssoHostStateCookie}
}

// ssoPage answers a navigation with a small HTML page: the API's JSON
// errors would be shown raw by the browser.
func ssoPage(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	securityHeaders(h)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", csp)
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte("<!doctype html><html><head><meta charset=\"utf-8\"><title>Command Center</title></head><body><p>" +
		html.EscapeString(msg) + "</p><p><a href=\"/ui/\">Back to Command Center</a></p></body></html>\n"))
}

func (u *UI) ssoStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !u.ssoUsable() || u.Auth.Sessions == nil {
		ssoPage(w, http.StatusNotFound, "single sign-on is not enabled")
		return
	}
	base := u.baseURL()
	if base == "" {
		ssoPage(w, http.StatusServiceUnavailable, "single sign-on needs the baseURL setting: set it in the general settings")
		return
	}
	p, err := u.oidcProvider()
	if err != nil {
		ssoPage(w, http.StatusServiceUnavailable, "single sign-on is misconfigured: "+err.Error())
		return
	}
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "#") || len(next) > ssoNextMax || strings.ContainsFunc(next, func(c rune) bool { return c < ' ' || c == 0x7f }) {
		next = ""
	}
	state, nonce, verifier := oidc.Random(), oidc.Random(), oidc.Random()
	target, err := p.AuthURL(r.Context(), base+"/-/sso/oidc/callback", state, nonce, verifier)
	if err != nil {
		u.ssoWarn(r, "oidc login could not start", err)
		ssoPage(w, http.StatusBadGateway, "the identity provider could not be reached")
		return
	}
	if !u.sso.put(state, ssoState{nonce: nonce, verifier: verifier, next: next, created: time.Now()}, time.Now()) {
		ssoPage(w, http.StatusServiceUnavailable, "too many logins are pending; try again in a few minutes")
		return
	}
	http.SetCookie(w, u.ssoCookie(state, int(ssoStateTTL.Seconds())))
	http.Redirect(w, r, target, http.StatusFound)
}

func (u *UI) ssoCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !u.ssoUsable() || u.Auth.Sessions == nil {
		ssoPage(w, http.StatusNotFound, "single sign-on is not enabled")
		return
	}
	http.SetCookie(w, u.ssoCookie("", -1)) // one use, whatever happens
	q := r.URL.Query()
	if code := q.Get("error"); code != "" {
		msg := "login failed: the identity provider answered " + trunc(code, 64)
		if d := q.Get("error_description"); d != "" {
			msg += " (" + trunc(d, 200) + ")"
		}
		ssoPage(w, http.StatusBadRequest, msg)
		return
	}
	state := q.Get("state")
	c, err := r.Cookie(u.ssoCookieName())
	if state == "" || err != nil || c.Value != state {
		ssoPage(w, http.StatusBadRequest, "login failed: the login state does not match; start again")
		return
	}
	st, ok := u.sso.take(state, time.Now())
	if !ok {
		ssoPage(w, http.StatusBadRequest, "login failed: the login expired or was already used; start again")
		return
	}
	p, err := u.oidcProvider()
	if err != nil {
		ssoPage(w, http.StatusServiceUnavailable, "single sign-on is misconfigured: "+err.Error())
		return
	}
	id, err := p.Exchange(r.Context(), q.Get("code"), u.baseURL()+"/-/sso/oidc/callback", st.verifier, st.nonce)
	if err != nil {
		u.ssoWarn(r, "oidc login failed", err)
		ssoPage(w, http.StatusBadRequest, "login failed: the identity provider's answer could not be verified")
		return
	}
	name := strings.ToLower(id.Username)
	if !meta.ValidUserID(name) {
		u.ssoWarn(r, "oidc login refused: unusable username", nil)
		ssoPage(w, http.StatusBadRequest, "login failed: unusable username (it must be 1-64 letters, digits, '.', '_', '@' or '-')")
		return
	}
	pr, err := u.Auth.RecordOIDC(auth.OIDCUser{ID: name, Name: id.Name, Email: id.Email, Groups: id.Groups})
	if errors.Is(err, auth.ErrOIDCRefused) {
		ssoPage(w, http.StatusForbidden, "login failed: this account is not allowed to sign in")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.endCookieSessions(r)
	sc := u.cookie(u.Auth.Sessions.Create(pr.Owner()), sessionMaxAge)
	http.SetCookie(w, sc)
	u.expireCookies(w, sc.Name)
	http.Redirect(w, r, "/"+st.next, http.StatusFound)
}

// ssoWarn logs a failed login without the code, tokens or state.
func (u *UI) ssoWarn(r *http.Request, msg string, err error) {
	if u.Log == nil {
		return
	}
	if err != nil {
		u.Log.Warn(msg, "id", r.Header.Get("X-Request-Id"), "err", logsafe.Err(err))
		return
	}
	u.Log.Warn(msg, "id", r.Header.Get("X-Request-Id"))
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
