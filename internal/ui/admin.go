package ui

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"runtime"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/sysinfo"
)

// tokensLimit caps the token list (Decision 9).
const tokensLimit = 1000

func (u *UI) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/tokens", u.listTokens)
	mux.HandleFunc("POST /-/api/tokens", u.createToken)
	mux.HandleFunc("DELETE /-/api/tokens/{id}", u.deleteToken)
	mux.HandleFunc("GET /-/api/status", u.status)
	mux.HandleFunc("GET /-/api/stats/requests", u.requestStats)
}

// requireUser answers 401 to anonymous callers.
func requireUser(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p := principal(r)
	if p.Anonymous() {
		writeError(w, http.StatusUnauthorized, "login required")
		return p, false
	}
	return p, true
}

// requireInteractive answers 401 to anonymous callers and 403 to tokens: a
// token must not be able to mint or revoke tokens, only a password or session.
func requireInteractive(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := requireUser(w, r)
	if !ok {
		return p, false
	}
	if p.Via != auth.ViaPassword && p.Via != auth.ViaSession {
		writeError(w, http.StatusForbidden, "this action requires a password or session login")
		return p, false
	}
	return p, true
}

// requireAdminInteractive requires an admin who used a password or session.
func requireAdminInteractive(w http.ResponseWriter, r *http.Request) bool {
	p, ok := requireInteractive(w, r)
	if !ok {
		return false
	}
	if !p.Admin {
		writeError(w, http.StatusForbidden, "admin only")
		return false
	}
	return true
}

// requireAdmin answers 401 to anonymous callers and 403 to non-admins.
func requireAdmin(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := requireUser(w, r)
	if !ok {
		return p, false
	}
	if !p.Admin {
		writeError(w, http.StatusForbidden, "admin only")
		return p, false
	}
	return p, true
}

func tokenJSON(t meta.Token) map[string]any {
	return map[string]any{"id": t.ID, "user": t.User, "created": rfc3339(t.Created), "lastUsed": rfc3339(t.LastUsed)}
}

func (u *UI) listTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok {
		return
	}
	owner := p.Owner() // "ldap:<id>" for an LDAP user
	if p.Admin {
		owner = "" // all users
	}
	toks, truncated, err := u.DB.ListTokens(r.Context(), owner, tokensLimit)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(toks))
	for _, t := range toks {
		out = append(out, tokenJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "truncated": truncated})
}

func (u *UI) createToken(w http.ResponseWriter, r *http.Request) {
	p, ok := requireInteractive(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	// An empty body means "for myself", like {}.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var body struct {
		User string `json:"user"`
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if !decodeBody(w, r, &body) {
			return
		}
	}
	user := body.User
	if user == "" {
		user = p.User
	}
	if user != p.User {
		if !p.Admin {
			writeError(w, http.StatusForbidden, "only an admin can create tokens for other users")
			return
		}
		if !u.Auth.UserExists(user) {
			writeError(w, http.StatusBadRequest, "unknown user")
			return
		}
	}
	var id, tok string
	if user == p.User {
		// For oneself: an LDAP user's token is owned by "ldap:<id>".
		id, tok, err = u.Auth.CreateTokenFor(r.Context(), p)
		user = p.Owner()
	} else {
		id, tok, err = u.Auth.CreateToken(r.Context(), user)
	}
	if errors.Is(err, auth.ErrLDAPUser) {
		writeError(w, http.StatusBadRequest, "an LDAP user creates their own tokens")
		return
	}
	if errors.Is(err, auth.ErrUserDisabled) {
		writeError(w, http.StatusConflict, "the user is disabled")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "token.create", "", user, "id "+id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "user": user, "token": tok})
}

func (u *UI) deleteToken(w http.ResponseWriter, r *http.Request) {
	p, ok := requireInteractive(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	id := r.PathValue("id")
	t, err := u.DB.GetToken(r.Context(), id)
	owner := t.User
	if err == nil && t.User != p.Owner() && !p.Admin {
		err = meta.ErrNotFound // do not leak other users' token ids
	}
	if err == nil {
		err = u.DB.DeleteToken(r.Context(), id)
	}
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "token not found")
	case err != nil:
		u.fail(w, r, err)
	default:
		u.emit(r, "token.revoke", "", owner, "id "+id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (u *UI) status(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	stats, err := u.repoStats(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	blobs, blobBytes, err := u.blobStats(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	sorted := u.Repos.Snapshot().Sorted
	repos := make([]map[string]any, 0, len(sorted))
	for _, rp := range sorted {
		s := stats[rp.Name]
		repos = append(repos, map[string]any{"name": rp.Name, "assets": s.Assets, "bytes": s.Bytes})
	}
	var uptime int64
	if !u.Started.IsZero() {
		uptime = int64(time.Since(u.Started).Seconds())
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        u.Version,
		"uptimeSeconds":  uptime,
		"rssBytes":       sysinfo.RSSBytes(),
		"heapAllocBytes": ms.HeapAlloc,
		"heapSysBytes":   ms.HeapSys,
		"sysBytes":       ms.Sys,
		"goroutines":     runtime.NumGoroutine(),
		"numGC":          ms.NumGC,
		"repositories":   repos,
		"blobBytes":      blobBytes,
		"blobs":          blobs,
	})
}
