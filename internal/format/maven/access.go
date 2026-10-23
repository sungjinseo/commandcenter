package maven

import (
	"net/http"

	"commandcenter/internal/auth"
	"commandcenter/internal/repo"
)

// Access implements repo.AccessDecider (plan 12 Decision 2): GET/HEAD read, PUT adds, DELETE deletes.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return auth.Read
	case http.MethodDelete:
		return auth.Delete
	}
	// PUT is always Add, also over an existing path: whether an overwrite is
	// allowed at all is the writePolicy's decision, and maven-metadata.xml and
	// checksums are rewritten on every deploy (plan 12 Decision 2).
	return auth.Add
}
