package yum

import "commandcenter/internal/repo"

// LockPaths takes the lock an upload to rp takes, for repository import
// (internal/transfer): one per repository, whatever the paths.
func (h *Handler) LockPaths(rp *repo.Repository, _ []string) (unlock func()) {
	return h.locks.Lock(rp.Name)
}
