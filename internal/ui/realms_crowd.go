package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

// The Crowd realm configuration, as the LDAP one (realms.go): reads need an
// admin, changes and the connection test an interactive admin plus the UI
// header. The application password is a secret: accepted on PUT (empty
// keeps the stored one, clearApplicationPassword blanks it), never
// returned, logged, put in an event or in the test output.

// crowdReenterMessage is matched by the UI.
const crowdReenterMessage = "re-enter the application password when changing the server or weakening TLS"

func (u *UI) crowdRealmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/realms/crowd", u.getCrowd)
	mux.HandleFunc("PUT /-/api/realms/crowd", u.putCrowd)
	mux.HandleFunc("POST /-/api/realms/crowd/test", u.testCrowd)
}

type crowdDTO struct {
	URL                    string              `json:"url"`
	ApplicationName        string              `json:"applicationName"`
	ApplicationPasswordSet bool                `json:"applicationPasswordSet"`
	TimeoutSeconds         int                 `json:"timeoutSeconds"`
	CacheMinutes           int                 `json:"cacheMinutes"`
	GroupMappings          []meta.GroupMapping `json:"groupMappings"`
	DefaultRoles           []string            `json:"defaultRoles"`
}

func crowdJSON(c meta.Crowd) crowdDTO {
	l := ldapJSON(c.Mapping()) // the mapping lists, never nil
	return crowdDTO{URL: c.URL, ApplicationName: c.ApplicationName, ApplicationPasswordSet: c.ApplicationPassword != "",
		TimeoutSeconds: c.TimeoutSeconds, CacheMinutes: c.CacheMinutes, GroupMappings: l.GroupMappings, DefaultRoles: l.DefaultRoles}
}

type crowdRequest struct {
	URL                      string              `json:"url"`
	ApplicationName          string              `json:"applicationName"`
	ApplicationPassword      string              `json:"applicationPassword"`
	ClearApplicationPassword bool                `json:"clearApplicationPassword"`
	TimeoutSeconds           int                 `json:"timeoutSeconds"`
	CacheMinutes             int                 `json:"cacheMinutes"`
	GroupMappings            []meta.GroupMapping `json:"groupMappings"`
	DefaultRoles             []string            `json:"defaultRoles"`
}

func (u *UI) getCrowd(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, crowdJSON(u.Auth.Snapshot().Settings().Crowd))
}

func (u *UI) putCrowd(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body crowdRequest
	if !decodeBody(w, r, &body) {
		return
	}
	u.accessMu.Lock() // load, lockout check, write and reload as one step
	defer u.accessMu.Unlock()
	// Merge into the stored value, so the password kept is the stored one.
	set, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	cur, def := set.Crowd, meta.DefaultCrowd()
	next := meta.Crowd{URL: strings.TrimRight(strings.TrimSpace(body.URL), "/"), ApplicationName: strings.TrimSpace(body.ApplicationName),
		ApplicationPassword: cur.ApplicationPassword, TimeoutSeconds: body.TimeoutSeconds, CacheMinutes: body.CacheMinutes,
		GroupMappings: []meta.GroupMapping{}, DefaultRoles: append([]string{}, body.DefaultRoles...)}
	for _, m := range body.GroupMappings {
		next.GroupMappings = append(next.GroupMappings, meta.GroupMapping{Group: strings.TrimSpace(m.Group), Roles: append([]string{}, m.Roles...)})
	}
	if next.TimeoutSeconds == 0 {
		next.TimeoutSeconds = def.TimeoutSeconds
	}
	if next.CacheMinutes == 0 {
		next.CacheMinutes = def.CacheMinutes
	}
	switch {
	case body.ClearApplicationPassword:
		next.ApplicationPassword = ""
	case body.ApplicationPassword != "":
		next.ApplicationPassword = body.ApplicationPassword
	}
	if body.ApplicationPassword == "" && !body.ClearApplicationPassword && cur.ApplicationPassword != "" && !crowdSameTrust(cur, next) {
		writeError(w, http.StatusBadRequest, crowdReenterMessage)
		return
	}
	var v validation
	if err := meta.ValidateCrowd(next); err != nil {
		v.add("%s", err.Error())
	}
	if !next.Complete() && realmEnabled(set.Realms, meta.RealmCrowd) {
		v.add("crowd url and applicationName are required while the crowd realm is enabled")
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
	err = u.prospectiveLockout(r.Context(), func(s *meta.Settings) { s.Crowd = next })
	if err == nil {
		err = u.DB.SetSettingJSON(r.Context(), meta.SettingCrowd, next)
	}
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if err := u.Auth.Reload(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "realms.crowd.update", "", "", crowdChanged(cur, next))
	writeJSON(w, http.StatusOK, crowdJSON(next))
}

// crowdChanged names the keys that differ, never their values.
func crowdChanged(a, b meta.Crowd) string {
	var keys []string
	add := func(changed bool, k string) {
		if changed {
			keys = append(keys, k)
		}
	}
	add(a.URL != b.URL, "url")
	add(a.ApplicationName != b.ApplicationName, "applicationName")
	add(a.ApplicationPassword != b.ApplicationPassword, "applicationPassword")
	add(a.TimeoutSeconds != b.TimeoutSeconds, "timeoutSeconds")
	add(a.CacheMinutes != b.CacheMinutes, "cacheMinutes")
	add(ldapJSONString(a.GroupMappings) != ldapJSONString(b.GroupMappings), "groupMappings")
	add(strings.Join(a.DefaultRoles, ",") != strings.Join(b.DefaultRoles, ","), "defaultRoles")
	return strings.Join(keys, ", ")
}

// crowdSameTrust reports whether the stored application password may stay
// with next: the same scheme, host and port, and not https to http.
func crowdSameTrust(cur, next meta.Crowd) bool {
	a, err1 := url.Parse(cur.URL)
	b, err2 := url.Parse(next.URL)
	return err1 == nil && err2 == nil && strings.EqualFold(a.Host, b.Host) && strings.EqualFold(a.Scheme, b.Scheme)
}

// testCrowd checks the saved configuration: the application's credentials,
// then, with a user name, the user's password (when given) or the user, and
// the groups and roles it maps to.
func (u *UI) testCrowd(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	set, err := u.DB.LoadSettings(r.Context()) // fresh: it carries the application password
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if !set.Crowd.Complete() {
		writeError(w, http.StatusBadRequest, "Crowd is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ldapTestCap)
	defer cancel()
	step := func(name string, err error, ok string) ldap.Step {
		if err != nil {
			return ldap.Step{Step: name, Detail: err.Error()}
		}
		return ldap.Step{Step: name, OK: true, Detail: ok}
	}
	steps := []ldap.Step{step("application", u.Auth.CrowdPing(ctx, set.Crowd), "Crowd accepted the application "+set.Crowd.ApplicationName)}
	user := strings.TrimSpace(body.Username)
	if steps[0].OK && user != "" {
		var cu auth.CrowdUser
		if body.Password != "" {
			cu, err = u.Auth.CrowdAuthenticate(ctx, set.Crowd, user, body.Password)
			steps = append(steps, step("authenticate", err, "the password is correct and the user is active"))
		} else {
			cu, err = u.Auth.CrowdFind(ctx, set.Crowd, user)
			steps = append(steps, step("user", err, "the user exists and is active"))
		}
		if err == nil {
			roles, matched := ldap.MapRoles(set.Crowd.Mapping(), cu.Groups)
			steps = append(steps, ldap.Step{Step: "groups", OK: true, Detail: fmt.Sprintf("%d groups (%s); mapped %s; roles %s",
				len(cu.Groups), strings.Join(cu.Groups, ", "), strings.Join(matched, ", "), strings.Join(roles, ", "))})
			if !meta.ValidUserID(cu.ID) {
				steps = append(steps, ldap.Step{Step: "name", Detail: "sign-in refuses this user: its name is not a valid user id"})
			}
		}
	}
	ok := true
	for _, s := range steps {
		ok = ok && s.OK
	}
	outcome := "failed"
	if ok {
		outcome = "ok"
	}
	u.emit(r, "realms.crowd.test", "", user, outcome)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "steps": steps})
}

// crowdCheck is the "Crowd reachable" status check: the application's
// credentials against the saved configuration.
func (u *UI) crowdCheck(ctx context.Context) func() (bool, string) {
	return func() (bool, string) {
		c := u.Auth.Snapshot().Settings().Crowd
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := u.Auth.CrowdPing(ctx, c); err != nil {
			return false, "Crowd at " + c.URL + " did not accept the application: " + err.Error() + "; Crowd users cannot sign in"
		}
		return true, "Crowd at " + c.URL + " accepts the application " + c.ApplicationName
	}
}
