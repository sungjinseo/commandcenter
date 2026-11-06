package pypi

import (
	"net/http"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/repo"
)

// Access implements repo.AccessDecider (plan 12 Decision 2): GET/HEAD read, as
// does pip search's POST; DELETE deletes; an upload adds.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead || strings.TrimSuffix(path, "/") == "pypi":
		return auth.Read
	case r.Method == http.MethodDelete:
		return auth.Delete
	}
	return auth.Add // twine upload
}
