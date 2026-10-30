package nexusapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/ui"
)

// Users, roles, privileges, realms and anonymous access of the Nexus
// security API (security-01..05). Every call checks the application domain
// here first, so an unprivileged caller learns nothing of what exists; a
// write is then an in-process call of the UI's API, which checks the caller
// again (from a password or session login) and validates, so the rules are
// the UI's.

func (h *Handler) nxSecurityRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/security/users", h.nxUsers)
	m.HandleFunc("POST "+p+"/security/users", h.nxCreateUser)
	m.HandleFunc("PUT "+p+"/security/users/{id}", h.nxUpdateUser)
	m.HandleFunc("DELETE "+p+"/security/users/{id}", h.nxDeleteUser)
	m.HandleFunc("PUT "+p+"/security/users/{id}/change-password", h.nxChangePassword)
	m.HandleFunc("GET "+p+"/security/user-sources", h.nxUserSources)
	m.HandleFunc("GET "+p+"/security/roles", h.nxRoles)
	m.HandleFunc("POST "+p+"/security/roles", h.nxCreateRole)
	m.HandleFunc("GET "+p+"/security/roles/{id}", h.nxGetRole)
	m.HandleFunc("PUT "+p+"/security/roles/{id}", h.nxUpdateRole)
	m.HandleFunc("DELETE "+p+"/security/roles/{id}", h.nxDeleteRole)
	m.HandleFunc("GET "+p+"/security/privileges", h.nxPrivileges)
	m.HandleFunc("GET "+p+"/security/privileges/{name}", h.nxGetPrivilege)
	m.HandleFunc("DELETE "+p+"/security/privileges/{name}", h.nxDeletePrivilege)
	m.HandleFunc("POST "+p+"/security/privileges/{type}", h.nxCreatePrivilege)
	m.HandleFunc("PUT "+p+"/security/privileges/{type}/{name}", h.nxUpdatePrivilege)
	m.HandleFunc("GET "+p+"/security/realms/available", h.nxRealmsAvailable)
	m.HandleFunc("GET "+p+"/security/realms/active", h.nxRealmsActive)
	m.HandleFunc("PUT "+p+"/security/realms/active", h.nxSetRealms)
	m.HandleFunc("GET "+p+"/security/anonymous", h.nxAnonymous)
	m.HandleFunc("PUT "+p+"/security/anonymous", h.nxSetAnonymous)
}

// app answers 401 (anonymous) or 403 unless p is an admin or holds action
// of the application domain.
func (h *Handler) app(w http.ResponseWriter, r *http.Request, domain, action string) (auth.Principal, bool) {
	p := principal(r)
	if !h.Auth.CanApp(p, domain, action) {
		deny(w, p)
		return p, false
	}
	return p, true
}

func (h *Handler) repoFormatOf(name string) string {
	if rp := h.Repos.Get(name); rp != nil {
		return rp.Format
	}
	return ""
}

// writeBad answers a translation error 400 and reports whether err was one.
func writeBad(w http.ResponseWriter, err error) bool {
	var b *badRequest
	if errors.As(err, &b) {
		writeMessage(w, http.StatusBadRequest, b.msg)
		return true
	}
	return false
}

// ---- role ids: the built-in admin role is nx-admin in Nexus ----

const nxAdminRole = "nx-admin"

// roleSet is the stored roles by id, the built-in admin role included.
type roleSet map[string]meta.Role

func (h *Handler) storedRoles(r *http.Request) (roleSet, error) {
	list, err := h.DB.ListRoles(r.Context())
	if err != nil {
		return nil, err
	}
	out := roleSet{meta.AdminRole: meta.BuiltinAdminRole()}
	for _, x := range list {
		out[x.ID] = x
	}
	return out, nil
}

// out is the Nexus id of role id: nx-admin for the admin role, unless a
// stored role already has that id.
func (rs roleSet) out(id string) string {
	if _, taken := rs[nxAdminRole]; id == meta.AdminRole && !taken {
		return nxAdminRole
	}
	return id
}

// in is the Command Center id of a Nexus role id.
func (rs roleSet) in(id string) string {
	if _, taken := rs[nxAdminRole]; id == nxAdminRole && !taken {
		return meta.AdminRole
	}
	return id
}

func (rs roleSet) outAll(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		out = append(out, rs.out(id))
	}
	return out
}

// ---- users ----

// nxSource is the Nexus user source of a Command Center one.
var nxSource = map[string]string{meta.SourceLocal: "default", meta.SourceLDAP: "LDAP", meta.SourceCrowd: "Crowd",
	meta.SourceSAML: "SAML", meta.SourceOIDC: "OAuth2", meta.SourceRUT: "rutauth"}

// realmSource maps the Nexus realm ids of DELETE ?realm= to sources.
var realmSource = map[string]string{"NexusAuthenticatingRealm": meta.SourceLocal, "LdapRealm": meta.SourceLDAP, "Crowd": meta.SourceCrowd,
	"SamlRealm": meta.SourceSAML, "OAuth2Realm": meta.SourceOIDC, "rutauth-realm": meta.SourceRUT}

type nxUser struct {
	UserID        string   `json:"userId"`
	FirstName     string   `json:"firstName"`
	LastName      string   `json:"lastName"`
	EmailAddress  string   `json:"emailAddress"`
	Source        string   `json:"source"`
	Status        string   `json:"status"`
	ReadOnly      bool     `json:"readOnly"`
	Roles         []string `json:"roles"`
	ExternalRoles []string `json:"externalRoles"`
	Password      string   `json:"password,omitempty"` // create only; never answered
}

// splitName is name as Nexus's firstName and lastName: last when it is the
// end of name (as written through this API), else split at the last space.
func splitName(name, last string) (string, string) {
	if last != "" && (name == last || strings.HasSuffix(name, " "+last)) {
		return strings.TrimSuffix(strings.TrimSuffix(name, last), " "), last
	}
	if i := strings.LastIndex(name, " "); i > 0 {
		return name[:i], name[i+1:]
	}
	return name, ""
}

func joinName(first, last string) string {
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}

// ccStatus maps a Nexus user status: locked is disabled here, and
// changepassword (a forced change at the next login) active.
func ccStatus(s string) string {
	switch strings.ToLower(s) {
	case "", "active", "changepassword":
		return meta.StatusActive
	case "disabled", "locked":
		return meta.StatusDisabled
	}
	return s // the UI refuses it
}

func toNxUser(u meta.User, rs roleSet) nxUser {
	first, last := splitName(u.Name, u.LastName)
	src := nxSource[u.Source]
	if src == "" {
		src = u.Source
	}
	return nxUser{UserID: u.ID, FirstName: first, LastName: last, EmailAddress: u.Email, Source: src, Status: u.Status,
		ReadOnly: u.Source != meta.SourceLocal, Roles: rs.outAll(u.Roles), ExternalRoles: []string{}}
}

// nxUsers is GET /security/users: the stored users; userId is a
// case-insensitive prefix and source a user source (default, LDAP, ...).
// Directory users without a stored record are not listed.
func (h *Handler) nxUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "users", meta.ActionRead); !ok {
		return
	}
	prefix := strings.ToLower(r.URL.Query().Get("userId"))
	source := r.URL.Query().Get("source")
	list, err := h.DB.ListUsers(r.Context())
	rs, err2 := h.storedRoles(r)
	if err = errors.Join(err, err2); err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := []nxUser{}
	for _, u := range list {
		x := toNxUser(u, rs)
		if !strings.HasPrefix(strings.ToLower(u.ID), prefix) || source != "" && !strings.EqualFold(source, x.Source) {
			continue
		}
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) nxUserRoles(rs roleSet, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, rs.in(id))
	}
	return out
}

// nxCreateUser is POST /security/users: a local user; answers the user.
func (h *Handler) nxCreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "users", meta.ActionAdd); !ok { // the UI checks again
		return
	}
	var b nxUser
	if !decodeSmall(w, r, &b) {
		return
	}
	rs, err := h.storedRoles(r)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	if b.Password == "" {
		writeValidation(w, []string{"password: a password is required"})
		return
	}
	body := map[string]any{"id": b.UserID, "name": joinName(b.FirstName, b.LastName), "lastName": strings.TrimSpace(b.LastName),
		"email": b.EmailAddress, "password": b.Password, "status": ccStatus(b.Status), "roles": h.nxUserRoles(rs, b.Roles)}
	status, res := h.call(r, http.MethodPost, "/-/api/users", body)
	if status != http.StatusCreated {
		writeUIError(w, status, res)
		return
	}
	u, err := h.DB.GetUser(r.Context(), b.UserID)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toNxUser(u, rs))
}

// nxUpdateUser is PUT /security/users/{id}. A local user takes the names,
// e-mail, status and roles; a directory user (an override record, made
// when missing) only its roles.
func (h *Handler) nxUpdateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "users", meta.ActionEdit); !ok { // the UI checks again
		return
	}
	id := r.PathValue("id")
	var b nxUser
	if !decodeSmall(w, r, &b) {
		return
	}
	if b.UserID != id {
		writeMessage(w, http.StatusBadRequest, "the userId in the body does not match the one in the path")
		return
	}
	rs, err := h.storedRoles(r)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	roles := h.nxUserRoles(rs, b.Roles)
	cur, err := h.DB.GetUser(r.Context(), id)
	if errors.Is(err, meta.ErrNotFound) {
		src := ""
		for cc, nx := range nxSource {
			if cc != meta.SourceLocal && strings.EqualFold(nx, b.Source) {
				src = cc
			}
		}
		if src == "" {
			writeMessage(w, http.StatusNotFound, "user not found")
			return
		}
		status, res := h.call(r, http.MethodPost, "/-/api/users", map[string]any{"id": id, "source": src, "roles": roles})
		if status != http.StatusCreated {
			writeUIError(w, status, res)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	body := map[string]any{"name": cur.Name, "email": cur.Email, "status": cur.Status, "roles": roles}
	if cur.Source == meta.SourceLocal {
		body["name"], body["lastName"] = joinName(b.FirstName, b.LastName), strings.TrimSpace(b.LastName)
		body["email"], body["status"] = b.EmailAddress, ccStatus(b.Status)
	}
	status, res := h.call(r, http.MethodPut, "/-/api/users/"+url.PathEscape(id), body)
	if status != http.StatusOK {
		writeUIError(w, status, res)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nxDeleteUser is DELETE /security/users/{id}?realm=: a directory user's
// record needs its realm, as in Nexus.
func (h *Handler) nxDeleteUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "users", meta.ActionDelete); !ok { // the UI checks again
		return
	}
	id := r.PathValue("id")
	realm := r.URL.Query().Get("realm")
	if cur, err := h.DB.GetUser(r.Context(), id); err == nil && cur.Source != meta.SourceLocal {
		if realm == "" {
			writeMessage(w, http.StatusBadRequest, "a non-local user cannot be deleted without its realm (?realm=)")
			return
		}
		if realmSource[realm] != cur.Source {
			writeMessage(w, http.StatusNotFound, "user not found in realm "+realm)
			return
		}
	}
	status, res := h.call(r, http.MethodDelete, "/-/api/users/"+url.PathEscape(id), nil)
	if status != http.StatusNoContent {
		writeUIError(w, status, res)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nxChangePassword is PUT /security/users/{id}/change-password with the new
// password as a text/plain body.
func (h *Handler) nxChangePassword(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "users", meta.ActionEdit); !ok { // the UI checks again
		return
	}
	pw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeMessage(w, http.StatusBadRequest, "password body too large")
		return
	}
	status, res := h.call(r, http.MethodPut, "/-/api/users/"+url.PathEscape(r.PathValue("id"))+"/password", map[string]string{"password": string(pw)})
	if status != http.StatusNoContent {
		writeUIError(w, status, res)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nxUserSources is GET /security/user-sources: default, and the external
// sources whose realm is on.
func (h *Handler) nxUserSources(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "users", meta.ActionRead); !ok {
		return
	}
	s := h.Auth.Snapshot()
	out := []map[string]string{{"id": "default", "name": "Local"}}
	for _, x := range []struct{ realm, id string }{{meta.RealmLDAP, "LDAP"}, {meta.RealmCrowd, "Crowd"}, {meta.RealmSAML, "SAML"}, {meta.RealmOIDC, "OAuth2"}} {
		if s.RealmEnabled(x.realm) {
			out = append(out, map[string]string{"id": x.id, "name": x.id})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- roles ----

type nxRole struct {
	ID          string   `json:"id"`
	Source      string   `json:"source"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ReadOnly    bool     `json:"readOnly"`
	Privileges  []string `json:"privileges"`
	Roles       []string `json:"roles"`
}

// roleGrant translates a Nexus role's privilege names (built-in, or made
// through this API: a "priv:" role) and contained role ids.
func (h *Handler) roleGrant(rs roleSet, privs, roles []string) (*grant, error) {
	g := newGrant()
	for _, name := range privs {
		if b, ok := builtinPriv(name, h.repoFormatOf); ok {
			for _, p := range b.grant.privs {
				g.add(p.Target, p.Repos, p.Actions...)
			}
			for _, c := range b.grant.roles {
				g.contain(c)
			}
			continue
		}
		if _, ok := rs[ui.NexusPrivRolePrefix+name]; ok {
			g.contain(ui.NexusPrivRolePrefix + name)
			continue
		}
		return nil, bad("privileges: privilege %q not found", name)
	}
	for _, id := range roles {
		cc := rs.in(id)
		if _, ok := rs[cc]; !ok || strings.HasPrefix(cc, ui.NexusPrivRolePrefix) {
			return nil, bad("roles: role %q not found", id)
		}
		g.contain(cc)
	}
	return g, nil
}

// toNxRole reads role x in Nexus terms: as written through this API while
// that still translates to what it holds, else derived from it.
func (h *Handler) toNxRole(x meta.Role, rs roleSet) nxRole {
	out := nxRole{ID: rs.out(x.ID), Source: "default", Name: x.Name, Description: x.Description, ReadOnly: x.ID == meta.AdminRole,
		Privileges: []string{}, Roles: []string{}}
	if out.Name == "" {
		out.Name = out.ID
	}
	if x.ID == meta.AdminRole {
		out.Privileges = []string{"nx-all"}
		return out
	}
	if f, ok := decodeForm(x.Nexus); ok && f.Privilege == nil {
		if g, err := h.roleGrant(rs, f.Privileges, f.Roles); err == nil && canon(g.privileges(), g.roles) == canon(x.Privileges, x.Roles) {
			out.Privileges, out.Roles = append(out.Privileges, f.Privileges...), append(out.Roles, f.Roles...)
			return out
		}
	}
	for _, p := range x.Privileges {
		for _, n := range derivedNames(p, h.repoFormatOf) {
			if !slices.Contains(out.Privileges, n) {
				out.Privileges = append(out.Privileges, n)
			}
		}
	}
	for _, c := range x.Roles {
		if n, ok := strings.CutPrefix(c, ui.NexusPrivRolePrefix); ok {
			out.Privileges = append(out.Privileges, n)
		} else {
			out.Roles = append(out.Roles, rs.out(c))
		}
	}
	return out
}

// sourceOK answers 400 for a role source that is not a Nexus one and
// reports whether the source is the default one (the others list nothing
// here: external groups map to roles explicitly).
func sourceOK(w http.ResponseWriter, source string) (isDefault, ok bool) {
	switch {
	case source == "" || source == "default":
		return true, true
	case slices.ContainsFunc([]string{"LDAP", "Crowd", "SAML", "OAuth2"}, func(s string) bool { return strings.EqualFold(s, source) }):
		return false, true
	}
	writeMessage(w, http.StatusBadRequest, "source "+strconvQuote(source)+" not found")
	return false, false
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// nxRoles is GET /security/roles?source=: the roles by id. The "priv:"
// roles are privileges in Nexus terms and listed under privileges.
func (h *Handler) nxRoles(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionRead); !ok {
		return
	}
	isDefault, ok := sourceOK(w, r.URL.Query().Get("source"))
	if !ok {
		return
	}
	out := []nxRole{}
	if isDefault {
		rs, err := h.storedRoles(r)
		if err != nil {
			writeMessage(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, x := range rs {
			if !strings.HasPrefix(x.ID, ui.NexusPrivRolePrefix) {
				out = append(out, h.toNxRole(x, rs))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) nxGetRole(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionRead); !ok {
		return
	}
	isDefault, ok := sourceOK(w, r.URL.Query().Get("source"))
	if !ok {
		return
	}
	rs, err := h.storedRoles(r)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	id := r.PathValue("id")
	x, found := rs[rs.in(id)]
	if !isDefault || !found || strings.HasPrefix(x.ID, ui.NexusPrivRolePrefix) {
		writeMessage(w, http.StatusNotFound, "role "+strconvQuote(id)+" not found")
		return
	}
	writeJSON(w, http.StatusOK, h.toNxRole(x, rs))
}

type nxRoleBody struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Privileges  []string `json:"privileges"`
	Roles       []string `json:"roles"`
}

// uiRole is the UI body of a Nexus role (without id).
func (h *Handler) uiRole(w http.ResponseWriter, rs roleSet, b nxRoleBody) (map[string]any, bool) {
	g, err := h.roleGrant(rs, b.Privileges, b.Roles)
	if err != nil {
		writeBad(w, err)
		return nil, false
	}
	name := b.Name
	if name == b.ID {
		name = "" // shown as the id anyway
	}
	return map[string]any{"name": name, "description": b.Description, "privileges": uiPrivs(g.privileges()), "roles": nonNilList(g.roles),
		"nexus": nexusForm{Privileges: nonNilList(b.Privileges), Roles: nonNilList(b.Roles)}.encode()}, true
}

func nonNilList(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// uiPrivs is privileges in the UI's JSON shape.
func uiPrivs(privs []meta.Privilege) []map[string]any {
	out := []map[string]any{}
	for _, p := range privs {
		m := map[string]any{"target": p.Target, "actions": p.Actions}
		if p.Repos != nil {
			m["repos"] = p.Repos
		}
		out = append(out, m)
	}
	return out
}

// nexusRoleStatus maps the UI's answers to Nexus's: a taken id is 400.
func nexusRoleStatus(status int) int {
	if status == http.StatusConflict {
		return http.StatusBadRequest
	}
	return status
}

// nxCreateRole is POST /security/roles; answers the role.
func (h *Handler) nxCreateRole(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionAdd); !ok { // the UI checks again
		return
	}
	var b nxRoleBody
	if !decodeSmall(w, r, &b) {
		return
	}
	rs, err := h.storedRoles(r)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	if strings.EqualFold(b.ID, nxAdminRole) || strings.HasPrefix(b.ID, ui.NexusPrivRolePrefix) {
		writeMessage(w, http.StatusBadRequest, "role id "+strconvQuote(b.ID)+" is reserved")
		return
	}
	body, ok := h.uiRole(w, rs, b)
	if !ok {
		return
	}
	body["id"] = b.ID
	status, res := h.call(r, http.MethodPost, "/-/api/roles", body)
	if status != http.StatusCreated {
		writeUIError(w, nexusRoleStatus(status), res)
		return
	}
	x, err := h.DB.GetRole(r.Context(), b.ID)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	rs[x.ID] = x
	writeJSON(w, http.StatusOK, h.toNxRole(x, rs))
}

// nxUpdateRole is PUT /security/roles/{id}: 409 when the body names
// another id, 400 for the built-in admin role.
func (h *Handler) nxUpdateRole(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionEdit); !ok { // the UI checks again
		return
	}
	id := r.PathValue("id")
	var b nxRoleBody
	if !decodeSmall(w, r, &b) {
		return
	}
	if b.ID != id {
		writeMessage(w, http.StatusConflict, "the role id "+strconvQuote(b.ID)+" does not match the id used in the path "+strconvQuote(id))
		return
	}
	rs, err := h.storedRoles(r)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	cc := rs.in(id)
	if cc == meta.AdminRole {
		writeMessage(w, http.StatusBadRequest, "role "+strconvQuote(id)+" is internal and cannot be modified or deleted")
		return
	}
	if _, ok := rs[cc]; !ok || strings.HasPrefix(cc, ui.NexusPrivRolePrefix) {
		writeMessage(w, http.StatusNotFound, "role "+strconvQuote(id)+" not found")
		return
	}
	body, ok := h.uiRole(w, rs, b)
	if !ok {
		return
	}
	status, res := h.call(r, http.MethodPut, "/-/api/roles/"+url.PathEscape(cc), body)
	if status != http.StatusOK {
		writeUIError(w, status, res)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) nxDeleteRole(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionDelete); !ok { // the UI checks again
		return
	}
	id := r.PathValue("id")
	rs, err := h.storedRoles(r)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	cc := rs.in(id)
	if cc == meta.AdminRole {
		writeMessage(w, http.StatusBadRequest, "role "+strconvQuote(id)+" is internal and cannot be modified or deleted")
		return
	}
	if strings.HasPrefix(cc, ui.NexusPrivRolePrefix) {
		writeMessage(w, http.StatusNotFound, "role "+strconvQuote(id)+" not found")
		return
	}
	status, res := h.call(r, http.MethodDelete, "/-/api/roles/"+url.PathEscape(cc), nil)
	if status != http.StatusNoContent {
		writeUIError(w, status, res)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- privileges ----

// nxActions is a Command Center repository action list in Nexus spelling.
func nxActions(acts []string, app bool) []string {
	if app {
		return nxAppActions(canonActions(acts))
	}
	out := []string{}
	for _, a := range canonActions(acts) {
		out = append(out, strings.ToUpper(a))
	}
	return out
}

// toNxPriv reads the "priv:" role x as a Nexus privilege: as written while
// that still translates to what it holds, else derived from its first
// privilege.
func (h *Handler) toNxPriv(x meta.Role) nxPriv {
	name := strings.TrimPrefix(x.ID, ui.NexusPrivRolePrefix)
	if f, ok := decodeForm(x.Nexus); ok && f.Privilege != nil {
		if g, err := translatePriv(*f.Privilege, h.repoFormatOf); err == nil && canon(g.privileges(), g.roles) == canon(x.Privileges, x.Roles) {
			p := *f.Privilege
			p.Name, p.Description, p.ReadOnly = name, x.Description, false
			return p
		}
	}
	out := nxPriv{Type: ptWildcard, Name: name, Description: x.Description}
	if len(x.Privileges) == 0 {
		if slices.Contains(x.Roles, meta.AdminRole) {
			out.Pattern = "nexus:*"
		}
		return out
	}
	p := x.Privileges[0]
	target, admin := strings.CutPrefix(p.Target, meta.AdminTargetPrefix)
	switch {
	case strings.HasPrefix(target, meta.SelectorPrefix) && !admin:
		out.Type, out.ContentSelector, out.Format, out.Actions = ptSelector, strings.TrimPrefix(target, meta.SelectorPrefix), "*", nxActions(p.Actions, false)
		out.Repository = "*"
		if len(p.Repos) > 0 {
			out.Repository = p.Repos[0]
		}
	case strings.HasPrefix(target, meta.AppTargetPrefix) && !admin:
		out.Type, out.Domain, out.Actions = ptApplication, strings.TrimPrefix(target, meta.AppTargetPrefix), nxActions(p.Actions, true)
	case target == auth.MetricsTarget && !admin:
		out.Type, out.Domain, out.Actions = ptApplication, "metrics", []string{"READ"}
	default:
		out.Type, out.Actions, out.Format, out.Repository = ptView, nxActions(p.Actions, false), "*", "*"
		if admin {
			out.Type = ptAdmin
		}
		switch {
		case strings.HasPrefix(target, "format:"):
			out.Format = nexusFormat(strings.TrimPrefix(target, "format:"))
		case strings.HasPrefix(target, "repo:"):
			out.Repository = strings.TrimPrefix(target, "repo:")
			out.Format = nexusFormat(h.repoFormatOf(out.Repository))
		}
	}
	out.Pattern = ""
	return out
}

// repoFormats is every repository's Command Center format by name.
func (h *Handler) repoFormats() map[string]string {
	out := map[string]string{}
	for _, rp := range h.Repos.Snapshot().Sorted {
		out[rp.Name] = rp.Format
	}
	return out
}

// nxPrivileges is GET /security/privileges: the built-in ones (read-only)
// and those made through this API, by name.
func (h *Handler) nxPrivileges(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionRead); !ok {
		return
	}
	out := []nxPriv{}
	for _, n := range builtinNames(h.repoFormats(), ui.AccessFormats()) {
		if b, ok := builtinPriv(n, h.repoFormatOf); ok {
			out = append(out, b.nxPriv)
		}
	}
	rs, err := h.storedRoles(r)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, x := range rs {
		if strings.HasPrefix(x.ID, ui.NexusPrivRolePrefix) {
			out = append(out, h.toNxPriv(x))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) nxGetPrivilege(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionRead); !ok {
		return
	}
	name := r.PathValue("name")
	if b, ok := builtinPriv(name, h.repoFormatOf); ok {
		writeJSON(w, http.StatusOK, b.nxPriv)
		return
	}
	x, err := h.DB.GetRole(r.Context(), ui.NexusPrivRolePrefix+name)
	if err != nil {
		writeMessage(w, http.StatusNotFound, "privilege "+strconvQuote(name)+" not found")
		return
	}
	writeJSON(w, http.StatusOK, h.toNxPriv(x))
}

func (h *Handler) nxDeletePrivilege(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionDelete); !ok { // the UI checks again
		return
	}
	name := r.PathValue("name")
	if _, ok := builtinPriv(name, h.repoFormatOf); ok {
		writeMessage(w, http.StatusBadRequest, "privilege "+strconvQuote(name)+" is internal and cannot be modified or deleted")
		return
	}
	status, res := h.call(r, http.MethodDelete, "/-/api/roles/"+url.PathEscape(ui.NexusPrivRolePrefix+name), nil)
	switch {
	case status == http.StatusNotFound:
		writeMessage(w, http.StatusNotFound, "privilege "+strconvQuote(name)+" not found")
	case status != http.StatusNoContent:
		writeUIError(w, status, res)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// uiPrivRole is the UI body of the "priv:" role of privilege p.
func (h *Handler) uiPrivRole(w http.ResponseWriter, typ string, p nxPriv) (map[string]any, bool) {
	p.Type, p.ReadOnly = typ, false
	g, err := translatePriv(p, h.repoFormatOf)
	if err != nil {
		writeBad(w, err)
		return nil, false
	}
	for i, a := range p.Actions {
		p.Actions[i] = strings.ToUpper(strings.TrimSpace(a))
	}
	stored := p
	stored.Name, stored.Description = "", "" // the role's id and description hold them
	return map[string]any{"description": p.Description, "privileges": uiPrivs(g.privileges()), "roles": nonNilList(g.roles),
		"nexus": nexusForm{Privilege: &stored}.encode()}, true
}

// nxCreatePrivilege is POST /security/privileges/{type}: 201.
func (h *Handler) nxCreatePrivilege(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionAdd); !ok { // the UI checks again
		return
	}
	var p nxPriv
	if !decodeSmall(w, r, &p) {
		return
	}
	if _, ok := builtinPriv(p.Name, h.repoFormatOf); ok {
		writeMessage(w, http.StatusBadRequest, "privilege "+strconvQuote(p.Name)+" already exists, use a unique name")
		return
	}
	body, ok := h.uiPrivRole(w, r.PathValue("type"), p)
	if !ok {
		return
	}
	body["id"] = ui.NexusPrivRolePrefix + p.Name
	status, res := h.call(r, http.MethodPost, "/-/api/roles", body)
	switch {
	case status == http.StatusConflict:
		writeMessage(w, http.StatusBadRequest, "privilege "+strconvQuote(p.Name)+" already exists, use a unique name")
	case status != http.StatusCreated:
		writeUIError(w, status, res)
	default:
		w.WriteHeader(http.StatusCreated)
	}
}

// nxUpdatePrivilege is PUT /security/privileges/{type}/{name}: 204; 409
// when the body names another privilege.
func (h *Handler) nxUpdatePrivilege(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "roles", meta.ActionEdit); !ok { // the UI checks again
		return
	}
	name := r.PathValue("name")
	var p nxPriv
	if !decodeSmall(w, r, &p) {
		return
	}
	if p.Name == "" {
		p.Name = name
	}
	if p.Name != name {
		writeMessage(w, http.StatusConflict, "the privilege name "+strconvQuote(p.Name)+" does not match the name used in the path "+strconvQuote(name))
		return
	}
	if _, ok := builtinPriv(name, h.repoFormatOf); ok {
		writeMessage(w, http.StatusBadRequest, "privilege "+strconvQuote(name)+" is internal and cannot be modified or deleted")
		return
	}
	body, ok := h.uiPrivRole(w, r.PathValue("type"), p)
	if !ok {
		return
	}
	status, res := h.call(r, http.MethodPut, "/-/api/roles/"+url.PathEscape(ui.NexusPrivRolePrefix+name), body)
	switch {
	case status == http.StatusNotFound:
		writeMessage(w, http.StatusNotFound, "privilege "+strconvQuote(name)+" not found")
	case status != http.StatusOK:
		writeUIError(w, status, res)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- realms ----

// nxRealms are the Nexus realm ids this API knows and the Command Center
// realm each stands for: the four token realms are the one token realm,
// and DefaultRole and User-Token-Realm are accepted with no effect
// (default roles are a setting; user tokens have their own switch).
var nxRealms = []struct{ id, name, cc string }{
	{"ConanToken", "Conan Bearer Token Realm", meta.RealmToken},
	{"Crowd", "Crowd Realm", meta.RealmCrowd},
	{"DefaultRole", "Default Role Realm", ""},
	{"DockerToken", "Docker Bearer Token Realm", meta.RealmToken},
	{"LdapRealm", "LDAP Realm", meta.RealmLDAP},
	{"NexusAuthenticatingRealm", "Local Authenticating Realm", meta.RealmLocal},
	{"NpmToken", "npm Bearer Token Realm", meta.RealmToken},
	{"NuGetApiKey", "NuGet API-Key Realm", meta.RealmToken},
	{"OAuth2Realm", "OAuth2 / OpenID Connect Realm", meta.RealmOIDC},
	{"SamlRealm", "SAML Realm", meta.RealmSAML},
	{"User-Token-Realm", "User Token Realm", ""},
	{"rutauth-realm", "Rut Auth Realm", meta.RealmRUT},
}

// nexusRealmsKey keeps the realm list last set through this API, read
// back while it still matches the realms.
const nexusRealmsKey = "nexusapi.realms"

// ccRealms is the Command Center realm order of Nexus realm ids, or the
// unknown ids.
func ccRealms(ids []string) (order, unknown []string) {
	for _, id := range ids {
		i := slices.IndexFunc(nxRealms, func(x struct{ id, name, cc string }) bool { return x.id == id })
		switch {
		case i < 0:
			unknown = append(unknown, id)
		case nxRealms[i].cc != "" && !slices.Contains(order, nxRealms[i].cc):
			order = append(order, nxRealms[i].cc)
		}
	}
	return order, unknown
}

func (h *Handler) nxRealmsAvailable(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "settings", meta.ActionRead); !ok {
		return
	}
	out := []map[string]string{}
	for _, x := range nxRealms {
		out = append(out, map[string]string{"id": x.id, "name": x.name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	writeJSON(w, http.StatusOK, out)
}

// nxRealmsActive is GET /security/realms/active: the enabled realms in
// order, in Nexus ids.
func (h *Handler) nxRealmsActive(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "settings", meta.ActionRead); !ok {
		return
	}
	set, err := h.DB.LoadSettings(r.Context())
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	var enabled []string
	for _, x := range set.Realms {
		if x.Enabled {
			enabled = append(enabled, x.ID)
		}
	}
	var stored []string
	if v, err := h.DB.GetSetting(r.Context(), nexusRealmsKey); err == nil && json.Unmarshal([]byte(v), &stored) == nil {
		if order, unknown := ccRealms(stored); len(unknown) == 0 && slices.Equal(order, enabled) {
			writeJSON(w, http.StatusOK, stored)
			return
		}
	}
	out := []string{}
	for _, id := range enabled {
		for _, x := range nxRealms {
			if x.cc == id {
				out = append(out, x.id)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// nxSetRealms is PUT /security/realms/active: the listed realms on, in
// order, every other one off. An unknown id is 400; a list that would
// leave no admin able to sign in is the UI's 409.
func (h *Handler) nxSetRealms(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var ids []string
	if !decodeSmall(w, r, &ids) {
		return
	}
	order, unknown := ccRealms(ids)
	if len(unknown) > 0 {
		writeMessage(w, http.StatusBadRequest, "Unknown realmIds: ["+strings.Join(unknown, ", ")+"]")
		return
	}
	set, err := h.DB.LoadSettings(r.Context())
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	realms := []meta.Realm{}
	for _, id := range order {
		realms = append(realms, meta.Realm{ID: id, Enabled: true})
	}
	for _, x := range set.Realms {
		if !slices.Contains(order, x.ID) {
			realms = append(realms, meta.Realm{ID: x.ID})
		}
	}
	status, res := h.call(r, http.MethodPut, "/-/api/realms", map[string]any{"realms": realms})
	if status != http.StatusOK {
		writeUIError(w, status, res)
		return
	}
	if err := h.DB.SetSettingJSON(r.Context(), nexusRealmsKey, nonNilList(ids)); err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- anonymous access ----

// Anonymous is one fixed identity here, read-only by design.
const (
	nxAnonymousUser  = "anonymous"
	nxAnonymousRealm = "NexusAuthorizingRealm"
)

func anonymousXO(enabled bool) map[string]any {
	return map[string]any{"enabled": enabled, "userId": nxAnonymousUser, "realmName": nxAnonymousRealm}
}

func (h *Handler) nxAnonymous(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "settings", meta.ActionRead); !ok {
		return
	}
	writeJSON(w, http.StatusOK, anonymousXO(h.Auth.Snapshot().Settings().AnonymousRead))
}

// nxSetAnonymous is PUT /security/anonymous: enabled turns the global
// anonymous read on or off. The identity and its realm are fixed, so
// another userId or realmName is 400.
func (h *Handler) nxSetAnonymous(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b struct {
		Enabled   bool   `json:"enabled"`
		UserID    string `json:"userId"`
		RealmName string `json:"realmName"`
	}
	if !decodeSmall(w, r, &b) {
		return
	}
	var errs []string
	if b.UserID != "" && b.UserID != nxAnonymousUser {
		errs = append(errs, "userId: anonymous access uses the fixed user "+strconvQuote(nxAnonymousUser))
	}
	if b.RealmName != "" && b.RealmName != nxAnonymousRealm {
		errs = append(errs, "realmName: Realm does not exist")
	}
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, res := h.call(r, http.MethodPut, "/-/api/settings", map[string]any{"anonymousRead": b.Enabled})
	if status != http.StatusOK {
		writeUIError(w, status, res)
		return
	}
	writeJSON(w, http.StatusOK, anonymousXO(b.Enabled))
}
