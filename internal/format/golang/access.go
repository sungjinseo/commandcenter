package golang

import (
	"net/http"

	"commandcenter/internal/auth"
	"commandcenter/internal/repo"
)

// Access implements repo.AccessDecider (plan 12 Decision 2): GET/HEAD read,
// DELETE deletes, an upload adds.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return auth.Read
	case http.MethodDelete:
		return auth.Delete
	}
	return auth.Add // uploads of .mod, .zip and .info
}
