package pypi

import (
	"context"
	"strings"

	"commandcenter/internal/repo"
)

// LockPaths takes the locks an upload of paths to rp takes, for repository
// import (internal/transfer): one per project, packages/<project>/<file>.
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, paths []string) (unlock func(), err error) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		key := p
		if segs := strings.SplitN(p, "/", 3); len(segs) == 3 && segs[0] == "packages" {
			key = segs[1]
		}
		keys[i] = "pypi\x00" + rp.Name + "\x00" + key
	}
	return h.Store.LockAll(ctx, keys)
}
