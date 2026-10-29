package raw

import "commandcenter/internal/repo"

// LockPaths takes the locks an upload of paths to rp takes, for repository
// import (internal/transfer): one per path.
func (h *Handler) LockPaths(rp *repo.Repository, paths []string) (unlock func()) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		keys[i] = rp.Name + "\x00" + p
	}
	return h.locks.LockAll(keys)
}
