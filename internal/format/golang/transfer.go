package golang

import (
	"path"
	"strings"

	"commandcenter/internal/repo"
)

// LockPaths takes the locks an upload of paths to rp takes, for repository
// import (internal/transfer): one per module version, <module>/@v/<version>.<ext>.
func (h *Handler) LockPaths(rp *repo.Repository, paths []string) (unlock func()) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		keys[i] = rp.Name + "\x00" + p
		if mod, file, ok := strings.Cut(p, "/@v/"); ok {
			keys[i] = rp.Name + "\x00" + mod + "@" + strings.TrimSuffix(file, path.Ext(file))
		}
	}
	return h.locks.LockAll(keys)
}
