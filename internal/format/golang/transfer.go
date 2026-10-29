package golang

import (
	"context"
	"path"
	"strings"

	"commandcenter/internal/repo"
)

// LockPaths takes the locks an upload of paths to rp takes, for repository
// import (internal/transfer): one per module version, <module>/@v/<version>.<ext>.
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, paths []string) (unlock func(), err error) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		keys[i] = "go\x00" + rp.Name + "\x00" + p
		if mod, file, ok := strings.Cut(p, "/@v/"); ok {
			keys[i] = "go\x00" + rp.Name + "\x00" + mod + "@" + strings.TrimSuffix(file, path.Ext(file))
		}
	}
	return h.Store.LockAll(ctx, keys)
}
