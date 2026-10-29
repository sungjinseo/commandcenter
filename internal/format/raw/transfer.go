package raw

import (
	"context"

	"commandcenter/internal/repo"
)

// LockPaths takes the locks an upload of paths to rp takes, for repository
// import (internal/transfer): one per path.
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, paths []string) (unlock func(), err error) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		keys[i] = "raw\x00" + rp.Name + "\x00" + p
	}
	return h.Store.LockAll(ctx, keys)
}
