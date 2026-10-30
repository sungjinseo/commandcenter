package ui

import (
	"fmt"
	"net/http"

	"commandcenter/internal/config"
)

// Pull replication (internal/replication): a proxy's replication settings
// are part of its definition (repoadmin.go); this checks its source.

func (u *UI) replicationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /-/api/repositories/{name}/replication/test", u.testReplication)
}

// testReplication checks the stored proxy's source: it answers with the
// remote credentials and has a hosted repository of the proxy's format.
// A source problem is 200 with ok false and the reason.
func (u *UI) testReplication(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	if u.Tasks == nil || u.Tasks.Replication == nil {
		writeError(w, http.StatusNotImplemented, "pull replication is not available")
		return
	}
	rp := u.Repos.Get(r.PathValue("name"))
	switch {
	case rp == nil:
		writeError(w, http.StatusNotFound, "repository not found")
	case rp.Type != "proxy":
		writeError(w, http.StatusBadRequest, "pull replication is only valid for proxy repositories")
	case !config.ReplicationFormats[rp.Format]:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("pull replication is not supported for %s repositories", rp.Format))
	default:
		writeJSON(w, http.StatusOK, u.Tasks.Replication.Test(r.Context(), rp))
	}
}
