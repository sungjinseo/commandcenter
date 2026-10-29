package conan

import (
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/repo"
)

// LockPaths takes the locks an upload of paths to rp takes, for repository
// import (internal/transfer): one per recipe reference.
func (h *Handler) LockPaths(rp *repo.Repository, paths []string) (unlock func()) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		keys[i] = rp.Name + "\x00" + p
		if group, name, version, ok := component.SplitConan(p); ok {
			ref := Ref{Name: name, User: "_", Channel: "_"}
			ref.Version, _, _ = strings.Cut(version, "#")
			if u, c, ok := strings.Cut(group, "/"); ok {
				ref.User, ref.Channel = u, c
			}
			keys[i] = lockKey(rp, ref)
		}
	}
	return h.locks.LockAll(keys)
}
