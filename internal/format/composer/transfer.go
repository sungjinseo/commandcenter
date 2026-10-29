package composer

import (
	"context"
	"strings"

	"commandcenter/internal/repo"
)

// LockPaths takes the locks an upload of paths to rp takes, for repository
// import (internal/transfer): one per package, dist/<vendor>/<package>/...
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, paths []string) (unlock func(), err error) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		key := p
		if segs := strings.SplitN(p, "/", 4); len(segs) == 4 && segs[0] == "dist" {
			key = segs[1] + "/" + segs[2]
		}
		keys[i] = "composer\x00" + rp.Name + "\x00" + key
	}
	return h.Store.LockAll(ctx, keys)
}
