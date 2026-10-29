package gitlfs

import "commandcenter/internal/repo"

// LockPaths is a no-op for repository import (internal/transfer): uploads
// take no lock, objects being content-addressed.
func (h *Handler) LockPaths(*repo.Repository, []string) (unlock func()) { return func() {} }
