package docker

import (
	"context"
	"strings"

	"commandcenter/internal/repo"
)

// LockPaths takes the locks a push of paths to rp takes, for repository
// import (internal/transfer): one per image, <image>/manifests/<digest> and
// <image>/blobs/<digest>.
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, paths []string) (unlock func(), err error) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		image := p
		if j := strings.LastIndex(p, "/manifests/"); j > 0 {
			image = p[:j]
		} else if j := strings.LastIndex(p, "/blobs/"); j > 0 {
			image = p[:j]
		}
		keys[i] = lockKey(rp, image)
	}
	return h.Store.LockAll(ctx, keys)
}
