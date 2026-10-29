package gitlfs

import (
	"context"

	"commandcenter/internal/repo"
)

// LockPaths is a no-op for repository import (internal/transfer): uploads
// take no lock, objects being content-addressed.
func (h *Handler) LockPaths(context.Context, *repo.Repository, []string) (unlock func(), err error) {
	return func() {}, nil
}
