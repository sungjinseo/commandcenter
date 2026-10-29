package helm

import (
	"context"
	"fmt"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// RebuildMetadata regenerates index.yaml of hosted rp from its rows, under
// the repository's lock (repo.MetadataRebuilder).
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	if rp.Format != "helm" || rp.Type != "hosted" {
		return fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	unlock, err := h.Store.Lock(ctx, "helm\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	logf("%s: regenerating %s", rp.Name, indexKey)
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error { return h.rebuildIndex(ctx, tx, rp) })
}
