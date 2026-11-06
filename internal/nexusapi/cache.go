package nexusapi

import (
	"net/http"
	"net/url"

	"commandcenter/internal/auth"
)

// Repository maintenance: invalidate-cache and rebuild-index.
func (h *Handler) cacheRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("POST "+p+"/repositories/{name}/invalidate-cache", h.invalidateCache)
	m.HandleFunc("POST "+p+"/repositories/{name}/rebuild-index", h.rebuildIndex)
}

// invalidateCache: a proxy's cached content goes stale and its recorded
// misses are forgotten; a group does so for every proxy it reaches. 204;
// 400 for a hosted repository, 404 for an unknown one. The UI checks
// repository-admin edit.
func (h *Handler) invalidateCache(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/repositories/"+url.PathEscape(r.PathValue("name"))+"/invalidate-cache", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rebuildIndex answers as Nexus does (204 for hosted and proxy, 400 for a
// group) and does nothing: search reads the live tables, so there is no
// index to rebuild.
func (h *Handler) rebuildIndex(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if p := principal(r); !h.Auth.CanAdminRepo(p, name, h.repoFormatOf(name), auth.Edit) {
		deny(w, p)
		return
	}
	rp := h.Repos.Lookup(r.Context(), name)
	switch {
	case rp == nil:
		writeMessage(w, http.StatusNotFound, "repository not found")
	case rp.Type == "group":
		writeMessage(w, http.StatusBadRequest, "a group repository has no search index of its own")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
