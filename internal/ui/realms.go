package ui

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

// Realms and the LDAP configuration (plan 12 Task 6). Reads need an admin;
// changes and the diagnostic test need an interactive admin plus the UI
// header. Every change that could remove the last way for an admin to sign
// in is refused (409). The bind password is a secret: it is accepted on PUT
// (empty keeps the stored one, clearBindPassword blanks it) and never
// returned, logged, put in an event or in the test output.

// ldapTestCap bounds the diagnostic run.
const ldapTestCap = 30 * time.Second

// bindReenterMessage is matched by the UI (static/access.js).
const bindReenterMessage = "re-enter the bind password when changing the server or weakening TLS"

var availableRealms = []string{meta.RealmLocal, meta.RealmToken, meta.RealmLDAP, meta.RealmOIDC, meta.RealmCrowd, meta.RealmRUT, meta.RealmSAML}

func (u *UI) realmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/realms", u.getRealms)
	mux.HandleFunc("PUT /-/api/realms", u.putRealms)
	mux.HandleFunc("GET /-/api/realms/ldap", u.getLDAP)
	mux.HandleFunc("PUT /-/api/realms/ldap", u.putLDAP)
	mux.HandleFunc("POST /-/api/realms/ldap/test", u.testLDAP)
}

func realmsJSON(realms []meta.Realm) map[string]any {
	if realms == nil {
		realms = []meta.Realm{}
	}
	return map[string]any{"realms": realms, "available": availableRealms}
}

func realmEnabled(realms []meta.Realm, id string) bool {
	for _, r := range realms {
		if r.ID == id && r.Enabled {
			return true
		}
	}
	return false
}

func (u *UI) getRealms(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, realmsJSON(u.Auth.Snapshot().Settings().Realms))
}

// prospectiveLockout builds the snapshot that change would produce from the
// stored state and returns auth.CheckLockout of it.
func (u *UI) prospectiveLockout(ctx context.Context, change func(s *meta.Settings)) error {
	data, err := u.DB.LoadAccess(ctx)
	if err != nil {
		return err
	}
	change(&data.Settings)
	return auth.CheckLockout(auth.NewSnapshot(data.Users, data.Roles, data.Settings))
}

func (u *UI) putRealms(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		Realms []meta.Realm `json:"realms"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	if body.Realms == nil {
		body.Realms = []meta.Realm{}
	}
	var v validation
	if err := meta.ValidateRealms(body.Realms); err != nil {
		v.add("%s", err.Error())
		u.writeAccessError(w, r, v.err())
		return
	}
	set, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if realmEnabled(body.Realms, meta.RealmLDAP) && !set.LDAP.Complete() {
		v.add("configure LDAP (url, userBaseDN, userFilter) before enabling the ldap realm")
		u.writeAccessError(w, r, v.err())
		return
	}
	if realmEnabled(body.Realms, meta.RealmOIDC) && !set.OIDC.Complete() {
		v.add("configure OIDC (issuer, clientId) before enabling the oidc realm")
		u.writeAccessError(w, r, v.err())
		return
	}
	if realmEnabled(body.Realms, meta.RealmCrowd) && !set.Crowd.Complete() {
		v.add("configure Crowd (url, applicationName) before enabling the crowd realm")
		u.writeAccessError(w, r, v.err())
		return
	}
	if realmEnabled(body.Realms, meta.RealmRUT) && !set.RUT.Complete() {
		v.add("configure rutauth (header, trustedProxies) before enabling the rutauth realm")
		u.writeAccessError(w, r, v.err())
		return
	}
	if realmEnabled(body.Realms, meta.RealmSAML) && !set.SAML.Complete() {
		v.add("configure SAML (the identity provider metadata) before enabling the saml realm")
		u.writeAccessError(w, r, v.err())
		return
	}
	err = u.prospectiveLockout(r.Context(), func(s *meta.Settings) { s.Realms = body.Realms })
	if err == nil {
		err = u.DB.SetSettingJSON(r.Context(), meta.SettingRealms, body.Realms)
	}
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if err := u.reloadAccess(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	}
	var parts []string
	for _, x := range body.Realms {
		state := "off"
		if x.Enabled {
			state = "on"
		}
		parts = append(parts, x.ID+" "+state)
	}
	u.emit(r, "realms.update", "", "", strings.Join(parts, ", "))
	writeJSON(w, http.StatusOK, realmsJSON(body.Realms))
}

// ldapDTO is the response: everything but the bind password.
type ldapDTO struct {
	URL                string              `json:"url"`
	StartTLS           bool                `json:"startTLS"`
	InsecureSkipVerify bool                `json:"insecureSkipVerify"`
	BindDN             string              `json:"bindDN"`
	BindPasswordSet    bool                `json:"bindPasswordSet"`
	TimeoutSeconds     int                 `json:"timeoutSeconds"`
	UserBaseDN         string              `json:"userBaseDN"`
	UserFilter         string              `json:"userFilter"`
	UserIDAttr         string              `json:"userIdAttr"`
	NameAttr           string              `json:"nameAttr"`
	EmailAttr          string              `json:"emailAttr"`
	GroupAttr          string              `json:"groupAttr"`
	GroupMappings      []meta.GroupMapping `json:"groupMappings"`
	DefaultRoles       []string            `json:"defaultRoles"`
}

func ldapJSON(c meta.LDAP) ldapDTO {
	c = c.Copy()
	for i := range c.GroupMappings {
		if c.GroupMappings[i].Roles == nil {
			c.GroupMappings[i].Roles = []string{}
		}
	}
	return ldapDTO{URL: c.URL, StartTLS: c.StartTLS, InsecureSkipVerify: c.InsecureSkipVerify, BindDN: c.BindDN,
		BindPasswordSet: c.BindPassword != "", TimeoutSeconds: c.TimeoutSeconds, UserBaseDN: c.UserBaseDN,
		UserFilter: c.UserFilter, UserIDAttr: c.UserIDAttr, NameAttr: c.NameAttr, EmailAttr: c.EmailAttr,
		GroupAttr: c.GroupAttr, GroupMappings: c.GroupMappings, DefaultRoles: c.DefaultRoles}
}

type ldapRequest struct {
	URL                string              `json:"url"`
	StartTLS           bool                `json:"startTLS"`
	InsecureSkipVerify bool                `json:"insecureSkipVerify"`
	BindDN             string              `json:"bindDN"`
	BindPassword       string              `json:"bindPassword"`
	ClearBindPassword  bool                `json:"clearBindPassword"`
	TimeoutSeconds     int                 `json:"timeoutSeconds"`
	UserBaseDN         string              `json:"userBaseDN"`
	UserFilter         string              `json:"userFilter"`
	UserIDAttr         string              `json:"userIdAttr"`
	NameAttr           string              `json:"nameAttr"`
	EmailAttr          string              `json:"emailAttr"`
	GroupAttr          string              `json:"groupAttr"`
	GroupMappings      []meta.GroupMapping `json:"groupMappings"`
	DefaultRoles       []string            `json:"defaultRoles"`
}

func (u *UI) getLDAP(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, ldapJSON(u.Auth.Snapshot().Settings().LDAP))
}

// orDefault returns s trimmed, or def when empty.
func orDefault(s, def string) string {
	if s = strings.TrimSpace(s); s == "" {
		return def
	}
	return s
}

func (u *UI) putLDAP(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body ldapRequest
	if !decodeBody(w, r, &body) {
		return
	}
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	// Merge into the stored value, not the snapshot, so the password that
	// is kept is the stored one.
	set, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	cur := set.LDAP
	def := meta.DefaultLDAP()
	next := meta.LDAP{
		URL: strings.TrimSpace(body.URL), StartTLS: body.StartTLS, InsecureSkipVerify: body.InsecureSkipVerify,
		BindDN: strings.TrimSpace(body.BindDN), BindPassword: cur.BindPassword, TimeoutSeconds: body.TimeoutSeconds,
		UserBaseDN: strings.TrimSpace(body.UserBaseDN), UserFilter: orDefault(body.UserFilter, def.UserFilter),
		UserIDAttr: orDefault(body.UserIDAttr, def.UserIDAttr), NameAttr: orDefault(body.NameAttr, def.NameAttr),
		EmailAttr: orDefault(body.EmailAttr, def.EmailAttr), GroupAttr: orDefault(body.GroupAttr, def.GroupAttr),
		DefaultRoles: append([]string{}, body.DefaultRoles...),
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
	case body.ClearBindPassword:
		next.BindPassword = ""
	case body.BindPassword != "":
		next.BindPassword = body.BindPassword
	}
	// The stored password never follows the configuration to another server
	// or over a weaker transport: the admin must type it again (as mergeAuth
	// does for remote credentials).
	if body.BindPassword == "" && !body.ClearBindPassword && cur.BindPassword != "" && !ldapSameTrust(cur, next) {
		writeError(w, http.StatusBadRequest, bindReenterMessage)
		return
	}
	var v validation
	if err := meta.ValidateLDAP(next); err != nil {
		v.add("%s", err.Error())
	}
	if next.URL == "" && realmEnabled(set.Realms, meta.RealmLDAP) {
		v.add("ldap url is required while the ldap realm is enabled")
	}
	if _, err := ldap.CompileFilter(ldap.UserFilter(next.UserFilter, "x")); err != nil {
		v.add("ldap userFilter: %s", strings.TrimPrefix(err.Error(), "ldap filter: "))
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
	err = u.prospectiveLockout(r.Context(), func(s *meta.Settings) { s.LDAP = next })
	if err == nil {
		err = u.DB.SetSettingJSON(r.Context(), meta.SettingLDAP, next)
	}
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if err := u.reloadAccess(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "realms.ldap.update", "", "", ldapChanged(cur, next))
	writeJSON(w, http.StatusOK, ldapJSON(next))
}

// ldapChanged names the keys that differ, never their values.
func ldapChanged(a, b meta.LDAP) string {
	var keys []string
	add := func(changed bool, k string) {
		if changed {
			keys = append(keys, k)
		}
	}
	add(a.URL != b.URL, "url")
	add(a.StartTLS != b.StartTLS, "startTLS")
	add(a.InsecureSkipVerify != b.InsecureSkipVerify, "insecureSkipVerify")
	add(a.BindDN != b.BindDN, "bindDN")
	add(a.BindPassword != b.BindPassword, "bindPassword")
	add(a.TimeoutSeconds != b.TimeoutSeconds, "timeoutSeconds")
	add(a.UserBaseDN != b.UserBaseDN, "userBaseDN")
	add(a.UserFilter != b.UserFilter, "userFilter")
	add(a.UserIDAttr != b.UserIDAttr || a.NameAttr != b.NameAttr || a.EmailAttr != b.EmailAttr || a.GroupAttr != b.GroupAttr, "attributes")
	add(ldapJSONString(a.GroupMappings) != ldapJSONString(b.GroupMappings), "groupMappings")
	add(strings.Join(a.DefaultRoles, ",") != strings.Join(b.DefaultRoles, ","), "defaultRoles")
	return strings.Join(keys, ", ")
}

func ldapJSONString(ms []meta.GroupMapping) string {
	var b strings.Builder
	for _, m := range ms {
		b.WriteString(m.Group + "=" + strings.Join(m.Roles, ",") + ";")
	}
	return b.String()
}

func (u *UI) testLDAP(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// The saved configuration, read fresh: it carries the bind password.
	set, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if !set.LDAP.Complete() {
		writeError(w, http.StatusBadRequest, "LDAP is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ldapTestCap)
	defer cancel()
	var steps []ldap.Step
	if body.User != "" && !auth.PlainLoginName(body.User) { // as sent: sign-in does not trim it
		steps = append(steps, ldap.Step{Step: "name",
			Detail: "sign-in refuses this name: it must be printable ASCII without leading, trailing or double spaces"})
	}
	steps = append(steps, (&ldap.Client{}).Diagnose(ctx, set.LDAP, strings.TrimSpace(body.User), body.Password)...)
	ok := len(steps) > 0
	for _, s := range steps {
		ok = ok && s.OK
	}
	if steps == nil {
		steps = []ldap.Step{}
	}
	outcome := "failed"
	if ok {
		outcome = "ok"
	}
	u.emit(r, "realms.ldap.test", "", strings.TrimSpace(body.User), outcome)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "steps": steps})
}

// ldapEndpoint returns an LDAP URL's scheme and host:port (default ports
// filled in, host lower-cased); ok is false when it does not parse.
func ldapEndpoint(raw string) (scheme, addr string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	scheme = strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = map[string]string{"ldap": "389", "ldaps": "636"}[scheme]
	}
	return scheme, net.JoinHostPort(strings.ToLower(u.Hostname()), port), true
}

// ldapSameTrust reports whether the stored bind password may stay with next:
// the same host and port, and no weaker transport (ldaps to ldap, StartTLS
// turned off, certificate verification turned off).
func ldapSameTrust(cur, next meta.LDAP) bool {
	cs, ca, ok1 := ldapEndpoint(cur.URL)
	ns, na, ok2 := ldapEndpoint(next.URL)
	switch {
	case !ok1 || !ok2 || ca != na:
		return false
	case cs == "ldaps" && ns != "ldaps":
		return false
	case cur.StartTLS && !next.StartTLS:
		return false
	case !cur.InsecureSkipVerify && next.InsecureSkipVerify:
		return false
	}
	return true
}
