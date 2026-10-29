package ui

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/oidc"
	"commandcenter/internal/proxy"
	"commandcenter/internal/saml"
)

// SAML 2.0 login (the saml realm). GET /-/sso/saml/login sends the browser
// to the identity provider with an AuthnRequest (HTTP-Redirect binding);
// the request ID and the return location wait in the database under a
// random state (meta.LoginState), which travels as RelayState and in a
// cookie. The IdP posts the Response to /-/sso/saml/acs (HTTP-POST
// binding) on any node, where the RelayState must match the cookie, the
// Response must answer the stored request ID and pass internal/saml's
// checks, and its assertion ID must be new to every node
// (meta.DB.FirstAssertion); then the identity is recorded in the
// authenticator and a session owned by "saml:<username>" starts, as for
// OIDC (sso.go). Single logout is not implemented: signing out ends the
// local session only. Each node fetches the IdP metadata itself.

const (
	samlStateCookie     = "cc_saml_state"
	samlHostStateCookie = "__Host-" + samlStateCookie
	samlMetadataPath    = "/-/sso/saml/metadata"
	samlACSPath         = "/-/sso/saml/acs"
	samlFetchTimeout    = 10 * time.Second
	samlRefresh         = 24 * time.Hour  // a fetched metadata document is re-fetched after this
	samlRetry           = 5 * time.Minute // a failed fetch is not retried sooner
	samlFormMax         = 1 << 20
)

// samlRealm is the IdP metadata this node fetched.
type samlRealm struct {
	mu        sync.Mutex // held across a metadata fetch, so logins wait for one fetch
	src       string     // the metadata source idp was built from
	idp       *saml.IdP
	err       error
	fetchedAt time.Time // the last successful fetch of a metadata URL
	triedAt   time.Time
}

func (r *samlRealm) reset() {
	r.mu.Lock()
	r.src, r.idp, r.err = "", nil, nil
	r.mu.Unlock()
}

func (u *UI) samlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/sso/saml/metadata", u.samlMetadata) // samlMetadataPath
	mux.HandleFunc("GET /-/sso/saml/login", u.samlLogin)
	mux.HandleFunc("POST /-/sso/saml/acs", u.samlACS) // samlACSPath
}

func (u *UI) samlRealmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/realms/saml", u.getSAML)
	mux.HandleFunc("PUT /-/api/realms/saml", u.putSAML)
	mux.HandleFunc("GET /-/api/realms/saml/sp-metadata", u.getSAMLSPMetadata)
}

// samlIdP returns the identity provider of cfg: the pasted metadata parsed
// once, or the metadata URL fetched and kept for a day (a stale copy is used
// while a re-fetch fails). fetchedAt is zero for pasted metadata.
func (u *UI) samlIdP(ctx context.Context, cfg meta.SAML) (idp *saml.IdP, fetchedAt time.Time, err error) {
	r := &u.saml
	r.mu.Lock()
	defer r.mu.Unlock()
	src := "xml:" + cfg.IdPMetadataXML
	if cfg.IdPMetadataURL != "" {
		src = "url:" + cfg.IdPMetadataURL
	}
	if r.src != src {
		r.src, r.idp, r.err, r.fetchedAt, r.triedAt = src, nil, nil, time.Time{}, time.Time{}
	}
	if cfg.IdPMetadataURL == "" {
		if r.idp == nil && r.err == nil {
			r.idp, r.err = saml.ParseMetadata([]byte(cfg.IdPMetadataXML))
		}
		return r.idp, time.Time{}, r.err
	}
	now := time.Now()
	if (r.idp == nil || now.Sub(r.fetchedAt) >= samlRefresh) && (r.triedAt.IsZero() || now.Sub(r.triedAt) >= samlRetry) {
		r.triedAt = now
		got, err := fetchSAMLMetadata(ctx, cfg.IdPMetadataURL)
		if err == nil {
			r.idp, r.fetchedAt, r.err = got, now, nil
		} else {
			r.err = err
			if u.Log != nil {
				u.Log.Warn("saml metadata fetch failed", "url", cfg.IdPMetadataURL, "err", err.Error())
			}
		}
	}
	if r.idp != nil {
		return r.idp, r.fetchedAt, nil
	}
	return nil, time.Time{}, r.err
}

// fetchSAMLMetadata GETs and parses the metadata at raw through the
// outbound HTTP client (egress policy, proxy, SSL trust store); a redirect
// may not leave https.
func fetchSAMLMetadata(ctx context.Context, raw string) (*saml.IdP, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, samlFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/samlmetadata+xml, application/xml;q=0.9, */*;q=0.1")
	c := proxy.NewClientFor(target, nil)
	c.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 || (via[0].URL.Scheme == "https" && next.URL.Scheme != "https") {
			return errors.New("the metadata URL redirects too often or away from https")
		}
		return nil
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the metadata URL could not be fetched: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the metadata URL answered %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, saml.MaxMetadataSize+1))
	if err != nil {
		return nil, fmt.Errorf("the metadata URL could not be read: %w", err)
	}
	return saml.ParseMetadata(b)
}

// samlEntityID is this server's entity ID under cfg.
func samlEntityID(cfg meta.SAML, base string) string {
	if cfg.EntityID != "" {
		return cfg.EntityID
	}
	return base + samlMetadataPath
}

// samlBaseSP is the service provider of cfg without its identity provider.
func samlBaseSP(cfg meta.SAML, base string) (*saml.SP, error) {
	sp := &saml.SP{EntityID: samlEntityID(cfg, base), ACSURL: base + samlACSPath,
		ValidateResponseSignature: cfg.ValidateResponseSignature, ValidateAssertionSignature: cfg.ValidateAssertionSignature,
		AllowSHA1: cfg.AllowSHA1Signatures, ClockSkew: time.Duration(cfg.ClockSkewSeconds) * time.Second, SignRequests: cfg.SignAuthnRequests}
	if cfg.SPPrivateKey != "" {
		var err error
		if sp.Key, sp.Cert, err = saml.ParseKeyPair(cfg.SPPrivateKey, cfg.SPCertificate); err != nil {
			return nil, err
		}
	}
	return sp, nil
}

var errNoBaseURL = errors.New("single sign-on needs the baseURL setting: set it in the general settings")

// samlSP is the live service provider with its identity provider.
func (u *UI) samlSP(ctx context.Context) (*saml.SP, error) {
	cfg, base := u.Auth.Snapshot().Settings().SAML, u.baseURL()
	if base == "" {
		return nil, errNoBaseURL
	}
	sp, err := samlBaseSP(cfg, base)
	if err != nil {
		return nil, err
	}
	if sp.IdP, _, err = u.samlIdP(ctx, cfg); err != nil {
		return nil, fmt.Errorf("the identity provider metadata: %w", err)
	}
	return sp, nil
}

// samlUsable reports whether the saml realm is on, configured and sessions exist.
func (u *UI) samlUsable() bool {
	s := u.Auth.Snapshot()
	return s.RealmEnabled(meta.RealmSAML) && s.Settings().SAML.Complete() && u.Auth.Sessions != nil
}

// samlProvider is the login button of the saml realm (GET /-/api/auth/sso), or nil.
func (u *UI) samlProvider() map[string]string {
	if !u.samlUsable() {
		return nil
	}
	name := strings.TrimSpace(u.Auth.Snapshot().Settings().SAML.DisplayName)
	if name == "" {
		name = meta.DefaultSAML().DisplayName
	}
	return map[string]string{"id": meta.RealmSAML, "name": name, "url": "/-/sso/saml/login"}
}

// samlCookie is the login state cookie. The IdP posts the Response from
// its own site, so under https the cookie is SameSite=None (with Secure and
// the __Host- prefix); under http, where None needs Secure, it has no
// SameSite attribute and reaches the ACS from a same-site IdP or a browser
// that does not default to Lax.
func (u *UI) samlCookie(value string, maxAge int) *http.Cookie {
	c := &http.Cookie{Name: samlStateCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true}
	if u.Auth.SessionCookieName() == auth.HostSessionCookie {
		c.Name, c.Secure, c.SameSite = samlHostStateCookie, true, http.SameSiteNoneMode
	}
	return c
}

// samlReturn is the UI location to land on after the login: the "next"
// hash route the login dialog sends ("#/..."), or a "return" path under
// /ui/; anything else (another origin, a path outside the UI) is dropped.
func samlReturn(v string) string {
	if v == "" || len(v) > ssoNextMax || strings.ContainsFunc(v, func(c rune) bool { return c < ' ' || c == 0x7f || c == '\\' }) {
		return ""
	}
	if strings.HasPrefix(v, "#") {
		return "/" + v
	}
	if (v == "/ui/" || strings.HasPrefix(v, "/ui/")) && !strings.Contains(v, "//") && !strings.Contains(v, "..") {
		return v
	}
	return ""
}

func (u *UI) samlMetadata(w http.ResponseWriter, r *http.Request) {
	cfg := u.Auth.Snapshot().Settings().SAML
	if !cfg.Complete() {
		ssoPage(w, http.StatusNotFound, "SAML single sign-on is not configured")
		return
	}
	u.writeSPMetadata(w, r, cfg)
}

// getSAMLSPMetadata answers GET /-/api/realms/saml/sp-metadata: the same
// document for an admin, whether or not the realm is configured.
func (u *UI) getSAMLSPMetadata(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	u.writeSPMetadata(w, r, u.Auth.Snapshot().Settings().SAML)
}

func (u *UI) writeSPMetadata(w http.ResponseWriter, r *http.Request, cfg meta.SAML) {
	base := u.baseURL()
	if base == "" {
		writeError(w, http.StatusServiceUnavailable, errNoBaseURL.Error())
		return
	}
	sp, err := samlBaseSP(cfg, base)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	h := w.Header()
	securityHeaders(h)
	h.Set("Content-Type", "application/samlmetadata+xml")
	w.Write(sp.Metadata())
}

func (u *UI) samlLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !u.samlUsable() {
		ssoPage(w, http.StatusNotFound, "SAML single sign-on is not enabled")
		return
	}
	sp, err := u.samlSP(r.Context())
	if err != nil {
		u.ssoWarn(r, "saml login could not start", err)
		ssoPage(w, http.StatusServiceUnavailable, "SAML single sign-on is misconfigured: "+err.Error())
		return
	}
	q := r.URL.Query()
	next := samlReturn(q.Get("return"))
	if next == "" {
		next = samlReturn(q.Get("next"))
	}
	state, now := oidc.Random(), time.Now()
	id, target, err := sp.AuthnRequestURL(state, now)
	if err != nil {
		ssoPage(w, http.StatusServiceUnavailable, "SAML single sign-on is misconfigured: "+err.Error())
		return
	}
	if !u.putLoginState(w, r, meta.RealmSAML, state, meta.LoginState{Nonce: id, Next: next}) {
		return
	}
	http.SetCookie(w, u.samlCookie(state, int(meta.LoginStateTTL.Seconds())))
	http.Redirect(w, r, target, http.StatusFound)
}

func (u *UI) samlACS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !u.samlUsable() {
		ssoPage(w, http.StatusNotFound, "SAML single sign-on is not enabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, samlFormMax)
	if err := r.ParseForm(); err != nil {
		ssoPage(w, http.StatusBadRequest, "login failed: the form could not be read")
		return
	}
	http.SetCookie(w, u.samlCookie("", -1)) // one use, whatever happens
	encoded, relay := r.PostForm.Get("SAMLResponse"), r.PostForm.Get("RelayState")
	if encoded == "" {
		ssoPage(w, http.StatusBadRequest, "login failed: the identity provider sent no SAMLResponse")
		return
	}
	cfg, now := u.Auth.Snapshot().Settings().SAML, time.Now()
	reqID, next := "", ""
	c, cerr := r.Cookie(u.samlCookie("", 0).Name)
	switch {
	case relay != "" && cerr == nil && c.Value == relay:
		st, ok := u.takeLoginState(w, r, meta.RealmSAML, relay)
		if !ok {
			return
		}
		reqID, next = st.Nonce, st.Next
	case cfg.AllowIdPInitiated:
		next = samlReturn(relay) // an IdP-initiated login: no request to answer
	default:
		ssoPage(w, http.StatusBadRequest, "login failed: the login state does not match (logins started at the identity provider are not allowed); start again from Command Center")
		return
	}
	sp, err := u.samlSP(r.Context())
	if err != nil {
		u.ssoWarn(r, "saml login failed", err)
		ssoPage(w, http.StatusServiceUnavailable, "SAML single sign-on is misconfigured: "+err.Error())
		return
	}
	a, err := sp.ParseResponse(encoded, reqID, now)
	if err != nil {
		u.ssoWarn(r, "saml login refused", err)
		ssoPage(w, http.StatusBadRequest, "login failed: "+err.Error())
		return
	}
	fresh, err := u.DB.FirstAssertion(r.Context(), a.ID, a.Expires, now)
	if err != nil {
		u.ssoWarn(r, "saml login failed", err)
		ssoPage(w, http.StatusServiceUnavailable, "the login could not be completed; start again")
		return
	}
	if !fresh {
		u.ssoWarn(r, "saml login refused: replayed assertion", nil)
		ssoPage(w, http.StatusBadRequest, "login failed: this response was already used; start again")
		return
	}
	name := a.NameID
	if cfg.UsernameAttribute != meta.SAMLNameID {
		name = first(a.Values(cfg.UsernameAttribute))
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !meta.ValidUserID(name) {
		u.ssoWarn(r, "saml login refused: unusable username", nil)
		ssoPage(w, http.StatusBadRequest, "login failed: unusable username (it must be 1-64 letters, digits, '.', '_', '@' or '-')")
		return
	}
	attr := func(n string) string {
		if n == "" {
			return ""
		}
		return trunc(first(a.Values(n)), 256)
	}
	var groups []string
	if cfg.GroupsAttribute != "" {
		groups = a.Values(cfg.GroupsAttribute)
	}
	full := strings.TrimSpace(attr(cfg.FirstNameAttribute) + " " + attr(cfg.LastNameAttribute))
	pr, err := u.Auth.RecordSAML(r.Context(), auth.SAMLUser{ID: name, Name: full, Email: attr(cfg.EmailAttribute), Groups: groups})
	if errors.Is(err, auth.ErrSAMLRefused) {
		ssoPage(w, http.StatusForbidden, "login failed: this account is not allowed to sign in")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	sc, ok := u.startSession(w, r, pr.Owner())
	if !ok {
		return
	}
	http.SetCookie(w, sc)
	u.expireCookies(w, sc.Name)
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func first(vs []string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// The realm configuration API, as the OIDC one (realms_oidc.go): reads need
// an admin, changes an interactive admin plus the UI header. The service
// provider key is generated on the first save, stored sealed and never
// returned; its certificate is.

type samlCertDTO struct {
	Subject   string `json:"subject"`
	SHA256    string `json:"sha256"`
	NotBefore string `json:"notBefore"`
	NotAfter  string `json:"notAfter"`
	Expired   bool   `json:"expired"`
}

func samlCert(c *x509.Certificate) samlCertDTO {
	return samlCertDTO{Subject: c.Subject.String(), SHA256: saml.Fingerprint(c), NotBefore: rfc3339(c.NotBefore),
		NotAfter: rfc3339(c.NotAfter), Expired: time.Now().After(c.NotAfter)}
}

type samlIdPDTO struct {
	EntityID                string        `json:"entityId"`
	SSOURL                  string        `json:"ssoUrl"`
	WantAuthnRequestsSigned bool          `json:"wantAuthnRequestsSigned"`
	Certificates            []samlCertDTO `json:"certificates"`
	FetchedAt               string        `json:"fetchedAt"` // "" for pasted metadata
}

type samlDTO struct {
	Enabled                    bool                `json:"enabled"` // the saml realm's state (PUT /-/api/realms switches it)
	DisplayName                string              `json:"displayName"`
	IdPMetadataXML             string              `json:"idpMetadataXML"`
	IdPMetadataURL             string              `json:"idpMetadataURL"`
	EntityID                   string              `json:"entityId"`
	EffectiveEntityID          string              `json:"effectiveEntityId"`
	ACSURL                     string              `json:"acsUrl"`
	MetadataURL                string              `json:"metadataUrl"`
	UsernameAttribute          string              `json:"usernameAttribute"`
	FirstNameAttribute         string              `json:"firstNameAttribute"`
	LastNameAttribute          string              `json:"lastNameAttribute"`
	EmailAttribute             string              `json:"emailAttribute"`
	GroupsAttribute            string              `json:"groupsAttribute"`
	GroupMappings              []meta.GroupMapping `json:"groupMappings"`
	DefaultRoles               []string            `json:"defaultRoles"`
	ValidateResponseSignature  bool                `json:"validateResponseSignature"`
	ValidateAssertionSignature bool                `json:"validateAssertionSignature"`
	AllowSHA1Signatures        bool                `json:"allowSHA1Signatures"`
	AllowIdPInitiated          bool                `json:"allowIdpInitiated"`
	ClockSkewSeconds           int                 `json:"clockSkewSeconds"`
	SignAuthnRequests          bool                `json:"signAuthnRequests"`
	SPCertificate              string              `json:"spCertificate"` // PEM; "" before the first save
	SPCertificateInfo          *samlCertDTO        `json:"spCertificateInfo"`
	IdP                        *samlIdPDTO         `json:"idp"`      // null when not configured or not loadable
	IdPError                   string              `json:"idpError"` // why idp is null; "" otherwise
}

func (u *UI) samlJSON(ctx context.Context, c meta.SAML) samlDTO {
	c = c.Copy()
	for i := range c.GroupMappings {
		if c.GroupMappings[i].Roles == nil {
			c.GroupMappings[i].Roles = []string{}
		}
	}
	base := u.baseURL()
	out := samlDTO{Enabled: u.Auth.Snapshot().RealmEnabled(meta.RealmSAML), DisplayName: c.DisplayName,
		IdPMetadataXML: c.IdPMetadataXML, IdPMetadataURL: c.IdPMetadataURL, EntityID: c.EntityID,
		UsernameAttribute: c.UsernameAttribute, FirstNameAttribute: c.FirstNameAttribute, LastNameAttribute: c.LastNameAttribute,
		EmailAttribute: c.EmailAttribute, GroupsAttribute: c.GroupsAttribute, GroupMappings: c.GroupMappings, DefaultRoles: c.DefaultRoles,
		ValidateResponseSignature: c.ValidateResponseSignature, ValidateAssertionSignature: c.ValidateAssertionSignature,
		AllowSHA1Signatures: c.AllowSHA1Signatures, AllowIdPInitiated: c.AllowIdPInitiated, ClockSkewSeconds: c.ClockSkewSeconds,
		SignAuthnRequests: c.SignAuthnRequests, SPCertificate: c.SPCertificate}
	if base != "" {
		out.EffectiveEntityID, out.ACSURL, out.MetadataURL = samlEntityID(c, base), base+samlACSPath, base+samlMetadataPath
	}
	if _, cert, err := saml.ParseKeyPair(c.SPPrivateKey, c.SPCertificate); err == nil {
		ci := samlCert(cert)
		out.SPCertificateInfo = &ci
	}
	if !c.Complete() {
		return out
	}
	idp, fetched, err := u.samlIdP(ctx, c)
	if err != nil {
		out.IdPError = err.Error()
		return out
	}
	d := &samlIdPDTO{EntityID: idp.EntityID, SSOURL: idp.SSOURL, WantAuthnRequestsSigned: idp.WantAuthnRequestsSigned, Certificates: []samlCertDTO{}}
	for _, cert := range idp.Certs {
		d.Certificates = append(d.Certificates, samlCert(cert))
	}
	if !fetched.IsZero() {
		d.FetchedAt = rfc3339(fetched)
	}
	out.IdP = d
	return out
}

type samlRequest struct {
	DisplayName                string              `json:"displayName"`
	IdPMetadataXML             string              `json:"idpMetadataXML"`
	IdPMetadataURL             string              `json:"idpMetadataURL"`
	EntityID                   string              `json:"entityId"`
	UsernameAttribute          string              `json:"usernameAttribute"`
	FirstNameAttribute         string              `json:"firstNameAttribute"`
	LastNameAttribute          string              `json:"lastNameAttribute"`
	EmailAttribute             string              `json:"emailAttribute"`
	GroupsAttribute            string              `json:"groupsAttribute"`
	GroupMappings              []meta.GroupMapping `json:"groupMappings"`
	DefaultRoles               []string            `json:"defaultRoles"`
	ValidateResponseSignature  *bool               `json:"validateResponseSignature"`  // absent: true
	ValidateAssertionSignature *bool               `json:"validateAssertionSignature"` // absent: true
	AllowSHA1Signatures        bool                `json:"allowSHA1Signatures"`
	AllowIdPInitiated          bool                `json:"allowIdpInitiated"`
	ClockSkewSeconds           *int                `json:"clockSkewSeconds"` // absent: 120
	SignAuthnRequests          bool                `json:"signAuthnRequests"`
}

// merged is the configuration body describes, with defaults for absent
// fields and cur's key pair.
func (body samlRequest) merged(cur meta.SAML) meta.SAML {
	def := meta.DefaultSAML()
	next := meta.SAML{DisplayName: orDefault(body.DisplayName, def.DisplayName),
		IdPMetadataXML: strings.TrimSpace(body.IdPMetadataXML), IdPMetadataURL: strings.TrimSpace(body.IdPMetadataURL),
		EntityID: strings.TrimSpace(body.EntityID), UsernameAttribute: orDefault(body.UsernameAttribute, def.UsernameAttribute),
		FirstNameAttribute: strings.TrimSpace(body.FirstNameAttribute), LastNameAttribute: strings.TrimSpace(body.LastNameAttribute),
		EmailAttribute: strings.TrimSpace(body.EmailAttribute), GroupsAttribute: strings.TrimSpace(body.GroupsAttribute),
		DefaultRoles: append([]string{}, body.DefaultRoles...), GroupMappings: []meta.GroupMapping{},
		ValidateResponseSignature:  body.ValidateResponseSignature == nil || *body.ValidateResponseSignature,
		ValidateAssertionSignature: body.ValidateAssertionSignature == nil || *body.ValidateAssertionSignature,
		AllowSHA1Signatures:        body.AllowSHA1Signatures, AllowIdPInitiated: body.AllowIdPInitiated,
		ClockSkewSeconds: def.ClockSkewSeconds, SignAuthnRequests: body.SignAuthnRequests,
		SPPrivateKey: cur.SPPrivateKey, SPCertificate: cur.SPCertificate}
	if body.ClockSkewSeconds != nil {
		next.ClockSkewSeconds = *body.ClockSkewSeconds
	}
	for _, m := range body.GroupMappings {
		next.GroupMappings = append(next.GroupMappings, meta.GroupMapping{Group: strings.TrimSpace(m.Group), Roles: append([]string{}, m.Roles...)})
	}
	return next
}

func (u *UI) getSAML(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, u.samlJSON(r.Context(), u.Auth.Snapshot().Settings().SAML))
}

func (u *UI) putSAML(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body samlRequest
	if !decodeBody(w, r, &body) {
		return
	}
	u.accessMu.Lock() // load, write and reload as one step, as the other access changes
	defer u.accessMu.Unlock()
	set, err := u.DB.LoadSettings(r.Context()) // the stored value carries the key
	if err != nil {
		u.fail(w, r, err)
		return
	}
	cur := set.SAML
	next := body.merged(cur)
	var v validation
	if err := meta.ValidateSAML(next); err != nil {
		v.add("%s", err.Error())
	}
	if next.IdPMetadataXML != "" {
		if _, err := saml.ParseMetadata([]byte(next.IdPMetadataXML)); err != nil {
			v.add("saml idpMetadataXML: %s", err.Error())
		}
	}
	if !next.Complete() && realmEnabled(set.Realms, meta.RealmSAML) {
		v.add("saml identity provider metadata is required while the saml realm is enabled")
	}
	for i, m := range next.GroupMappings {
		var mv validation
		if err := u.checkRoleRefs(r.Context(), &mv, m.Roles); err != nil {
			u.fail(w, r, err)
			return
		}
		for _, msg := range mv {
			v.add("group mapping %d: %s", i+1, msg)
		}
	}
	if err := u.checkRoleRefs(r.Context(), &v, next.DefaultRoles); err != nil {
		u.fail(w, r, err)
		return
	}
	if err := v.err(); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if next.SPPrivateKey == "" {
		cn := "commandcenter"
		if b, err := url.Parse(u.baseURL()); err == nil && b.Hostname() != "" {
			cn = b.Hostname()
		}
		if next.SPPrivateKey, next.SPCertificate, err = saml.GenerateKeyPair(cn); err != nil {
			u.fail(w, r, err)
			return
		}
	}
	if err := u.DB.SetSettingJSON(r.Context(), meta.SettingSAML, next); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if err := u.Auth.Reload(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	}
	u.saml.reset() // a saved URL is fetched again at once
	u.emit(r, "realms.saml.update", "", "", samlChangedKeys(cur, next))
	writeJSON(w, http.StatusOK, u.samlJSON(r.Context(), next))
}

// samlChangedKeys names the keys that differ, never their values.
func samlChangedKeys(a, b meta.SAML) string {
	var keys []string
	add := func(changed bool, k string) {
		if changed {
			keys = append(keys, k)
		}
	}
	add(a.DisplayName != b.DisplayName, "displayName")
	add(a.IdPMetadataXML != b.IdPMetadataXML || a.IdPMetadataURL != b.IdPMetadataURL, "idpMetadata")
	add(a.EntityID != b.EntityID, "entityId")
	add(a.UsernameAttribute != b.UsernameAttribute || a.FirstNameAttribute != b.FirstNameAttribute || a.LastNameAttribute != b.LastNameAttribute ||
		a.EmailAttribute != b.EmailAttribute || a.GroupsAttribute != b.GroupsAttribute, "attributes")
	add(ldapJSONString(a.GroupMappings) != ldapJSONString(b.GroupMappings), "groupMappings")
	add(strings.Join(a.DefaultRoles, ",") != strings.Join(b.DefaultRoles, ","), "defaultRoles")
	add(a.ValidateResponseSignature != b.ValidateResponseSignature || a.ValidateAssertionSignature != b.ValidateAssertionSignature ||
		a.AllowSHA1Signatures != b.AllowSHA1Signatures, "signatureChecks")
	add(a.AllowIdPInitiated != b.AllowIdPInitiated, "allowIdpInitiated")
	add(a.ClockSkewSeconds != b.ClockSkewSeconds, "clockSkewSeconds")
	add(a.SignAuthnRequests != b.SignAuthnRequests, "signAuthnRequests")
	add(a.SPCertificate != b.SPCertificate, "spKey")
	return strings.Join(keys, ", ")
}
