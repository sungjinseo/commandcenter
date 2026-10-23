package pypi

import (
	"net/http"

	"commandcenter/internal/auth"
	"commandcenter/internal/repo"
)

// Access implements repo.AccessDecider (plan 12 Decision 2): GET/HEAD read; an upload adds.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return auth.Read
	}
	return auth.Add // twine upload
}
