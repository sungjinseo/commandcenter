package ui

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/component"
	"commandcenter/internal/repo"
)

// deleteFolder is DELETE /-/api/repositories/{name}/folder?path=<dir>/:
// Nexus's "Delete folder" for a raw hosted repository. Every file under
// the directory that was stored before the request is deleted, each as
// its own component delete (events, tags), where the caller has the
// delete privilege on its path; a file stored or replaced meanwhile is
// kept. The answer counts the files deleted, those the caller may not
// delete (all of them: 403) and those that changed meanwhile.
func (u *UI) deleteFolder(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	rp, ok := u.lookup(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("path")
	switch {
	case dir == "" || !validDir(dir):
		writeError(w, http.StatusBadRequest, "path must be a relative directory ending in \"/\"")
		return
	case u.Deleters == nil:
		writeError(w, http.StatusNotImplemented, "deletion is not available")
		return
	case rp.Format != "raw" || rp.Type != "hosted":
		writeError(w, http.StatusBadRequest, "folders can be deleted in raw hosted repositories")
		return
	case !rp.IsOnline():
		writeError(w, http.StatusServiceUnavailable, "repository is offline")
		return
	case rp.WritePolicy == "deny":
		writeError(w, http.StatusBadRequest, fmt.Sprintf("repository %q is read-only", rp.Name))
		return
	}
	start := time.Now().UTC()
	// ponytail: the folder's rows are read at once; page them if folders of millions of files appear.
	assets, err := u.DB.ListAssets(r.Context(), rp.Name, dir)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if len(assets) == 0 {
		writeError(w, http.StatusNotFound, "folder "+dir+" not found")
		return
	}
	var deleted, denied, changed int
	var bytes int64
	for _, a := range assets {
		if !u.Auth.CanPath(p, rp.Name, auth.Delete, a.Path) {
			denied++
			continue
		}
		// the cleanup guard keeps a file stored or replaced after start
		c := Component{Repo: rp.Name, Format: rp.Format, Name: a.Path,
			Evaluated: &component.Evaluated{Paths: map[string]bool{a.Path: true}, MaxCreated: start, MaxModified: start, LastDownloaded: notDownloaded}}
		res, err := u.Deleters.Delete(r.Context(), rp, c, p.Owner())
		var he *repo.Error
		switch {
		case errors.Is(err, component.ErrChanged):
			changed++
		case errors.As(err, &he):
			writeError(w, he.Status, he.Msg)
			return
		case err != nil:
			u.fail(w, r, err)
			return
		default:
			deleted += res.Assets
			bytes += res.Bytes
		}
	}
	if deleted > 0 {
		u.repoCache.reset()
	}
	if denied == len(assets) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("user %q may not delete in %s", p.User, dir))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": deleted, "bytes": bytes, "denied": denied, "changed": changed})
}

// notDownloaded is a download time no file has: the folder delete ignores downloads.
var notDownloaded = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
