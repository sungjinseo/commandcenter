package rubygems

import (
	"context"
	"fmt"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// LockPaths takes the lock an upload to rp takes, for repository import
// (internal/transfer): one per repository, whatever the paths.
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, _ []string) (unlock func(), err error) {
	return h.Store.Lock(ctx, "rubygems\x00"+rp.Name)
}

// RebuildMetadata rewrites the versions file of hosted rp from its rows,
// under the repository's lock (repo.MetadataRebuilder; repository import
// uses it).
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	if rp.Format != "rubygems" || rp.Type != "hosted" {
		return fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	unlock, err := h.Store.Lock(ctx, "rubygems\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	logf("%s: regenerating %s", rp.Name, versionsKey)
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error { return h.writeVersions(ctx, tx, rp, "", "", false) })
}
