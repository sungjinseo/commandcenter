package yum

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// RebuildMetadata regenerates the repodata/ of every directory of hosted rp
// that holds packages (one for depth 0, each top-level directory for depth
// 1) from its rows, under the repository's lock (repo.MetadataRebuilder).
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	if rp.Format != "yum" || rp.Type != "hosted" {
		return fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		names, err := tx.ListPackageNames(ctx, rp.Name)
		if err != nil {
			return err
		}
		dirs := map[string]bool{}
		for _, n := range names {
			d := ""
			if rp.Depth == 1 {
				d, _, _ = strings.Cut(n, "/")
				d += "/"
			}
			dirs[d] = true
		}
		sorted := make([]string, 0, len(dirs))
		for d := range dirs {
			sorted = append(sorted, d)
		}
		sort.Strings(sorted)
		for _, d := range sorted {
			logf("%s: regenerating %srepodata/", rp.Name, d)
			if err := h.rebuild(ctx, tx, rp, d); err != nil {
				return err
			}
		}
		return nil
	})
}
