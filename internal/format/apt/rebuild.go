package apt

import (
	"context"
	"fmt"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// RebuildMetadata regenerates the dists/ files of hosted rp from its rows,
// under the repository's lock (repo.MetadataRebuilder).
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	if rp.Format != "apt" || rp.Type != "hosted" {
		return fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	logf("%s: regenerating dists/", rp.Name)
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error { return h.regenerate(ctx, tx, rp) })
}
