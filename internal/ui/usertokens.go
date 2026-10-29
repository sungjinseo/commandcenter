package ui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
)

// User tokens (auth/usertoken.go). Anyone signed in reads their own token's
// metadata; creating, resetting or deleting it needs a password or session
// login plus the UI header (a token never mints one). The admin list, a
// reset of another user's token and the reset of all need an admin, the
// changes an interactive one. The pass code is answered once, on creation,
// and never stored.

func (u *UI) userTokenRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/me/usertoken", u.getMyUserToken)
	mux.HandleFunc("POST /-/api/me/usertoken", u.createMyUserToken)
	mux.HandleFunc("DELETE /-/api/me/usertoken", u.deleteMyUserToken)
	mux.HandleFunc("GET /-/api/usertokens", u.listUserTokens)
	mux.HandleFunc("POST /-/api/usertokens/{user}", u.createUserToken)
	mux.HandleFunc("DELETE /-/api/usertokens/{user}", u.resetUserToken)
	mux.HandleFunc("DELETE /-/api/usertokens", u.resetAllUserTokens)
}

// splitOwner returns the user id and source of an owner key ("ldap:alice").
func splitOwner(owner string) (id, source string) {
	if src, id, ok := strings.Cut(owner, ":"); ok {
		return id, src
	}
	return owner, meta.SourceLocal
}

// userTokenJSON describes t under the settings c; full shows the name code.
func userTokenJSON(t meta.UserToken, c meta.UserTokens, now time.Time, full bool) map[string]any {
	id, source := splitOwner(t.Owner)
	name := t.NameCode
	if !full {
		name = name[:min(2, len(name))] + "******"
	}
	return map[string]any{"user": t.Owner, "userId": id, "source": source, "nameCode": name,
		"created": rfc3339(t.Created), "expires": rfc3339(c.Expires(t.Created)),
		"expired": auth.UserTokenExpired(c, t.Created, now), "lastUsed": rfc3339(t.LastUsed)}
}

func (u *UI) getMyUserToken(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok {
		return
	}
	c := u.Auth.Snapshot().Settings().UserTokens
	out := map[string]any{"enabled": c.Enabled, "required": c.Enabled && c.Required, "expirationDays": c.ExpirationDays, "exists": false}
	t, err := u.DB.GetUserToken(r.Context(), p.Owner())
	switch {
	case err == nil:
		for k, v := range userTokenJSON(t, c, u.clock(), false) {
			out[k] = v
		}
		out["exists"] = true
	case !errors.Is(err, meta.ErrNotFound):
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// issueUserToken creates (or replaces) owner's token and answers its codes.
func (u *UI) issueUserToken(w http.ResponseWriter, r *http.Request, owner string) {
	_, err := u.DB.GetUserToken(r.Context(), owner)
	kind := "usertoken.reset"
	if errors.Is(err, meta.ErrNotFound) {
		kind = "usertoken.create"
	} else if err != nil {
		u.fail(w, r, err)
		return
	}
	name, pass, err := u.Auth.CreateUserToken(r.Context(), owner)
	switch {
	case errors.Is(err, auth.ErrUserTokensDisabled):
		writeError(w, http.StatusConflict, "user tokens are disabled")
		return
	case errors.Is(err, auth.ErrUserDisabled):
		writeError(w, http.StatusConflict, "the user is unknown or disabled")
		return
	case err != nil:
		u.fail(w, r, err)
		return
	}
	t, err := u.DB.GetUserToken(r.Context(), owner)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, kind, "", owner, "name code "+name)
	out := userTokenJSON(t, u.Auth.Snapshot().Settings().UserTokens, u.clock(), true)
	out["passCode"] = pass
	writeJSON(w, http.StatusCreated, out)
}

func (u *UI) createMyUserToken(w http.ResponseWriter, r *http.Request) {
	p, ok := requireInteractive(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	u.issueUserToken(w, r, p.Owner())
}

func (u *UI) deleteMyUserToken(w http.ResponseWriter, r *http.Request) {
	p, ok := requireInteractive(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	u.dropUserToken(w, r, p.Owner(), "usertoken.delete")
}

func (u *UI) dropUserToken(w http.ResponseWriter, r *http.Request, owner, kind string) {
	err := u.DB.DeleteUserToken(r.Context(), owner)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "no user token")
	case err != nil:
		u.fail(w, r, err)
	default:
		u.emit(r, kind, "", owner, "")
		w.WriteHeader(http.StatusNoContent)
	}
}

func (u *UI) listUserTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	list, err := u.DB.ListUserTokens(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	c, now := u.Auth.Snapshot().Settings().UserTokens, u.clock()
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, userTokenJSON(t, c, now, true))
	}
	writeJSON(w, http.StatusOK, map[string]any{"userTokens": out})
}

func (u *UI) createUserToken(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	u.issueUserToken(w, r, r.PathValue("user"))
}

func (u *UI) resetUserToken(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	u.dropUserToken(w, r, r.PathValue("user"), "usertoken.reset")
}

// resetAllUserTokens deletes every user token; ?confirm=true is required,
// as it breaks every build that uses one until it gets a new token.
func (u *UI) resetAllUserTokens(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		writeError(w, http.StatusBadRequest, "resetting every user token needs ?confirm=true")
		return
	}
	n, err := u.DB.DeleteAllUserTokens(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "usertoken.reset", "", "", fmt.Sprintf("all %d", n))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}
