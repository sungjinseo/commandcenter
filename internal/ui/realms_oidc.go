package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/oidc"
)

// The OIDC realm configuration, as the LDAP one (realms.go): reads need an
// admin, changes and the diagnostic test an interactive admin plus the UI
// header. The client secret is a secret: accepted on PUT (empty keeps the
// stored one, clearClientSecret blanks it), never returned, logged, put in
// an event or in the test output.

// oidcTestCap bounds the diagnostic run.
const oidcTestCap = 30 * time.Second

// secretReenterMessage is matched by the UI.
const secretReenterMessage = "re-enter the client secret when changing the issuer or weakening TLS"

func (u *UI) oidcRealmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/realms/oidc", u.getOIDC)
	mux.HandleFunc("PUT /-/api/realms/oidc", u.putOIDC)
	mux.HandleFunc("POST /-/api/realms/oidc/test", u.testOIDC)
}

// oidcDTO is the response: everything but the client secret.
type oidcDTO struct {
	Issuer             string              `json:"issuer"`
	ClientID           string              `json:"clientId"`
	ClientSecretSet    bool                `json:"clientSecretSet"`
	DisplayName        string              `json:"displayName"`
	Scopes             []string            `json:"scopes"`
	UsernameClaim      string              `json:"usernameClaim"`
	GroupsClaim        string              `json:"groupsClaim"`
	DefaultRoles       []string            `json:"defaultRoles"`
	GroupMappings      []meta.GroupMapping `json:"groupMappings"`
	CACert             string              `json:"caCert"`
	InsecureSkipVerify bool                `json:"insecureSkipVerify"`
	TimeoutSeconds     int                 `json:"timeoutSeconds"`
}

func oidcJSON(c meta.OIDC) oidcDTO {
	c = c.Copy()
	for i := range c.GroupMappings {
		if c.GroupMappings[i].Roles == nil {
			c.GroupMappings[i].Roles = []string{}
		}
	}
	if c.GroupMappings == nil {
		c.GroupMappings = []meta.GroupMapping{}
	}
	return oidcDTO{Issuer: c.Issuer, ClientID: c.ClientID, ClientSecretSet: c.ClientSecret != "", DisplayName: c.DisplayName,
		Scopes: c.Scopes, UsernameClaim: c.UsernameClaim, GroupsClaim: c.GroupsClaim, DefaultRoles: c.DefaultRoles,
		GroupMappings: c.GroupMappings, CACert: c.CACert, InsecureSkipVerify: c.InsecureSkipVerify, TimeoutSeconds: c.TimeoutSeconds}
}

type oidcRequest struct {
	Issuer             string              `json:"issuer"`
	ClientID           string              `json:"clientId"`
	ClientSecret       string              `json:"clientSecret"`
	ClearClientSecret  bool                `json:"clearClientSecret"`
	DisplayName        string              `json:"displayName"`
	Scopes             []string            `json:"scopes"`
	UsernameClaim      string              `json:"usernameClaim"`
	GroupsClaim        string              `json:"groupsClaim"`
	DefaultRoles       []string            `json:"defaultRoles"`
	GroupMappings      []meta.GroupMapping `json:"groupMappings"`
	CACert             string              `json:"caCert"`
	InsecureSkipVerify bool                `json:"insecureSkipVerify"`
	TimeoutSeconds     int                 `json:"timeoutSeconds"`
}

// merged returns the configuration body describes, with defaults for empty
// fields and cur's secret unless the body replaces or clears it.
func (body oidcRequest) merged(cur meta.OIDC) meta.OIDC {
	def := meta.DefaultOIDC()
	next := meta.OIDC{
		Issuer: strings.TrimRight(strings.TrimSpace(body.Issuer), "/"), ClientID: strings.TrimSpace(body.ClientID),
		ClientSecret: cur.ClientSecret, DisplayName: orDefault(body.DisplayName, def.DisplayName),
		UsernameClaim: orDefault(body.UsernameClaim, def.UsernameClaim), GroupsClaim: orDefault(body.GroupsClaim, def.GroupsClaim),
		DefaultRoles: append([]string{}, body.DefaultRoles...), CACert: strings.TrimSpace(body.CACert),
		InsecureSkipVerify: body.InsecureSkipVerify, TimeoutSeconds: body.TimeoutSeconds,
	}
	for _, s := range body.Scopes {
		if s = strings.TrimSpace(s); s != "" {
			next.Scopes = append(next.Scopes, s)
		}
	}
	if next.Scopes == nil {
		next.Scopes = def.Scopes
	}
	for _, m := range body.GroupMappings {
		next.GroupMappings = append(next.GroupMappings, meta.GroupMapping{Group: strings.TrimSpace(m.Group), Roles: append([]string{}, m.Roles...)})
	}
	if next.GroupMappings == nil {
		next.GroupMappings = []meta.GroupMapping{}
	}
	if next.TimeoutSeconds == 0 {
		next.TimeoutSeconds = def.TimeoutSeconds
	}
	switch {
	case body.ClearClientSecret:
		next.ClientSecret = ""
	case body.ClientSecret != "":
		next.ClientSecret = body.ClientSecret
	}
	return next
}

func (u *UI) getOIDC(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, oidcJSON(u.Auth.Snapshot().Settings().OIDC))
}

func (u *UI) putOIDC(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body oidcRequest
	if !decodeBody(w, r, &body) {
		return
	}
	unlock, ok := u.lockAccess(w, r) // load, write and reload as one step, as the other access changes
	if !ok {
		return
	}
	defer unlock()
	// Merge into the stored value, not the snapshot, so the secret that is
	// kept is the stored one.
	set, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	cur := set.OIDC
	next := body.merged(cur)
	// The stored secret never follows the configuration to another issuer
	// or over a weaker transport: the admin must type it again.
	if body.ClientSecret == "" && !body.ClearClientSecret && cur.ClientSecret != "" && !oidcSameTrust(cur, next) {
		writeError(w, http.StatusBadRequest, secretReenterMessage)
		return
	}
	var v validation
	if err := meta.ValidateOIDC(next); err != nil {
		v.add("%s", err.Error())
	}
	if !next.Complete() && realmEnabled(set.Realms, meta.RealmOIDC) {
		v.add("oidc issuer and clientId are required while the oidc realm is enabled")
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
	if err := u.DB.SetSettingJSON(r.Context(), meta.SettingOIDC, next); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if err := u.reloadAccess(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "realms.oidc.update", "", "", oidcChanged(cur, next))
	writeJSON(w, http.StatusOK, oidcJSON(next))
}

// oidcChanged names the keys that differ, never their values.
func oidcChanged(a, b meta.OIDC) string {
	var keys []string
	add := func(changed bool, k string) {
		if changed {
			keys = append(keys, k)
		}
	}
	add(a.Issuer != b.Issuer, "issuer")
	add(a.ClientID != b.ClientID, "clientId")
	add(a.ClientSecret != b.ClientSecret, "clientSecret")
	add(a.DisplayName != b.DisplayName, "displayName")
	add(strings.Join(a.Scopes, " ") != strings.Join(b.Scopes, " "), "scopes")
	add(a.UsernameClaim != b.UsernameClaim || a.GroupsClaim != b.GroupsClaim, "claims")
	add(strings.Join(a.DefaultRoles, ",") != strings.Join(b.DefaultRoles, ","), "defaultRoles")
	add(ldapJSONString(a.GroupMappings) != ldapJSONString(b.GroupMappings), "groupMappings")
	add(a.CACert != b.CACert, "caCert")
	add(a.InsecureSkipVerify != b.InsecureSkipVerify, "insecureSkipVerify")
	add(a.TimeoutSeconds != b.TimeoutSeconds, "timeoutSeconds")
	return strings.Join(keys, ", ")
}

// oidcSameTrust reports whether the stored client secret may stay with
// next: the same issuer, and certificate verification not turned off.
func oidcSameTrust(cur, next meta.OIDC) bool {
	return strings.EqualFold(strings.TrimRight(cur.Issuer, "/"), next.Issuer) && (cur.InsecureSkipVerify || !next.InsecureSkipVerify)
}

// testOIDC fetches the provider's discovery document with the submitted
// configuration (the stored secret is not needed for that) and reports
// what it says.
func (u *UI) testOIDC(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body oidcRequest
	if !decodeBody(w, r, &body) {
		return
	}
	cfg := body.merged(meta.OIDC{})
	if cfg.Issuer == "" {
		writeError(w, http.StatusBadRequest, "oidc issuer is required")
		return
	}
	if err := meta.ValidateOIDC(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := oidc.New(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), oidcTestCap)
	defer cancel()
	d, err := p.Discover(ctx)
	if err != nil {
		u.emit(r, "realms.oidc.test", "", "", "failed")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u.emit(r, "realms.oidc.test", "", "", "ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": d.Issuer, "authorizationEndpoint": d.AuthorizationEndpoint, "tokenEndpoint": d.TokenEndpoint,
		"jwksUri": d.JWKSURI, "scopesSupported": nonNil(d.ScopesSupported), "algs": nonNil(d.Algs)})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
