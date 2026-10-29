package ui

import (
	"net/http"
	"strings"

	"commandcenter/internal/meta"
)

// The Remote User Token realm configuration (meta.RUT), as the LDAP and
// OIDC ones: reads need an admin, changes an interactive admin plus the UI
// header. It holds no secret. Enabling the realm (PUT /-/api/realms) needs a
// header and at least one trusted proxy.

func (u *UI) rutRealmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/realms/rutauth", u.getRUT)
	mux.HandleFunc("PUT /-/api/realms/rutauth", u.putRUT)
}

func rutJSON(c meta.RUT) meta.RUT {
	c = c.Copy()
	for i := range c.GroupMappings {
		if c.GroupMappings[i].Roles == nil {
			c.GroupMappings[i].Roles = []string{}
		}
	}
	return c
}

func (u *UI) getRUT(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, rutJSON(u.Auth.Snapshot().Settings().RUT))
}

func (u *UI) putRUT(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body meta.RUT
	if !decodeBody(w, r, &body) {
		return
	}
	u.accessMu.Lock()
	defer u.accessMu.Unlock()
	set, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	cur := set.RUT
	next := meta.RUT{Header: orDefault(body.Header, meta.DefaultRUT().Header), GroupHeader: strings.TrimSpace(body.GroupHeader),
		TrustedProxies: []string{}, DefaultRoles: append([]string{}, body.DefaultRoles...), GroupMappings: []meta.GroupMapping{}}
	for _, p := range body.TrustedProxies {
		if p = strings.TrimSpace(p); p != "" {
			next.TrustedProxies = append(next.TrustedProxies, p)
		}
	}
	for _, m := range body.GroupMappings {
		next.GroupMappings = append(next.GroupMappings, meta.GroupMapping{Group: strings.TrimSpace(m.Group), Roles: append([]string{}, m.Roles...)})
	}
	var v validation
	if err := meta.ValidateRUT(next); err != nil {
		v.add("%s", err.Error())
	}
	if !next.Complete() && realmEnabled(set.Realms, meta.RealmRUT) {
		v.add("rutauth header and trustedProxies are required while the rutauth realm is enabled")
	}
	if next.GroupHeader == "" && len(next.GroupMappings) > 0 {
		v.add("rutauth groupMappings need a groupHeader")
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
	if err := u.DB.SetSettingJSON(r.Context(), meta.SettingRUT, next); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if err := u.Auth.Reload(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	}
	var keys []string
	add := func(changed bool, k string) {
		if changed {
			keys = append(keys, k)
		}
	}
	add(cur.Header != next.Header, "header")
	add(cur.GroupHeader != next.GroupHeader, "groupHeader")
	add(strings.Join(cur.TrustedProxies, ",") != strings.Join(next.TrustedProxies, ","), "trustedProxies")
	add(strings.Join(cur.DefaultRoles, ",") != strings.Join(next.DefaultRoles, ","), "defaultRoles")
	add(ldapJSONString(cur.GroupMappings) != ldapJSONString(next.GroupMappings), "groupMappings")
	u.emit(r, "realms.rutauth.update", "", "", strings.Join(keys, ", "))
	writeJSON(w, http.StatusOK, rutJSON(next))
}
