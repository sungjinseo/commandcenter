package conda

import (
	"context"
	"fmt"
	"sort"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// RebuildMetadata regenerates repodata.json and current_repodata.json of
// every subdir of hosted rp that holds packages, and channeldata.json, from
// its rows, under the repository's lock (repo.MetadataRebuilder).
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	if rp.Format != "conda" || rp.Type != "hosted" {
		return fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	unlock, err := h.Store.Lock(ctx, "conda\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rows, err := rowsOf(ctx, tx, rp, "")
		if err != nil {
			return err
		}
		subdirs := map[string]bool{}
		for _, r := range rows {
			subdirs[r.Subdir] = true
		}
		sorted := make([]string, 0, len(subdirs))
		for d := range subdirs {
			sorted = append(sorted, d)
		}
		sort.Strings(sorted)
		for _, d := range sorted {
			logf("%s: regenerating %s/repodata.json", rp.Name, d)
			if err := h.rebuild(ctx, tx, rp, d); err != nil {
				return err
			}
		}
		return nil
	})
}
