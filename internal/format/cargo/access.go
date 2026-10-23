package cargo

import (
	"net/http"

	"commandcenter/internal/auth"
	"commandcenter/internal/repo"
)

// Access implements repo.AccessDecider (plan 12 Decision 2): GET/HEAD read, publish adds, yank and unyank edit.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	if rt, err := ParseRoute(path); err == nil {
		switch rt.Kind {
		case KindPublish:
			return auth.Add
		case KindYank, KindUnyank:
			return auth.Edit
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return auth.Read
	case http.MethodDelete:
		return auth.Delete
	}
	return auth.Add
}
