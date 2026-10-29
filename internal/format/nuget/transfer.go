package nuget

import (
	"context"
	"strings"

	"commandcenter/internal/repo"
)

// LockPaths takes the locks a push of paths to rp takes, for repository
// import (internal/transfer): one per package id, <id>/<version>/<file>.
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, paths []string) (unlock func(), err error) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		id, _, _ := strings.Cut(strings.TrimPrefix(p, "v3-flatcontainer/"), "/")
		keys[i] = "nuget\x00" + rp.Name + "\x00" + id
	}
	return h.Store.LockAll(ctx, keys)
}
