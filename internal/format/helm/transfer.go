package helm

import (
	"context"

	"commandcenter/internal/repo"
)

// LockPaths takes the lock an upload to rp takes, for repository import
// (internal/transfer): one per repository, whatever the paths.
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, _ []string) (unlock func(), err error) {
	return h.Store.Lock(ctx, "helm\x00"+rp.Name)
}
