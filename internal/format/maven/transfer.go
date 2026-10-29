package maven

import (
	"path"
	"strings"

	"commandcenter/internal/repo"
)

// LockPaths takes the locks a write of paths to rp takes, for repository
// import (internal/transfer): an artifact's per-asset lock, and a
// maven-metadata.xml's per-directory lock.
func (h *Handler) LockPaths(rp *repo.Repository, paths []string) (unlock func()) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		if d, file := path.Split(p); strings.HasPrefix(file, "maven-metadata.xml") {
			keys[i] = rp.Name + "\x00" + strings.TrimSuffix(d, "/")
		} else {
			keys[i] = "asset\x00" + rp.Name + "\x00" + p
		}
	}
	return h.locks.LockAll(keys)
}
