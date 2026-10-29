package r

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// LockPaths takes the lock an upload to rp takes, for repository import
// (internal/transfer): one per repository, whatever the paths.
func (h *Handler) LockPaths(ctx context.Context, rp *repo.Repository, _ []string) (unlock func(), err error) {
	return h.Store.Lock(ctx, "r\x00"+rp.Name)
}

// RebuildMetadata regenerates the PACKAGES files of every contrib directory
// of hosted rp that holds packages, from its rows, under the repository's
// lock (repo.MetadataRebuilder; repository import uses it).
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	if rp.Format != "r" || rp.Type != "hosted" {
		return fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	unlock, err := h.Store.Lock(ctx, "r\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		names, err := tx.ListPackageNames(ctx, rp.Name)
		if err != nil {
			return err
		}
		dirs := map[string]bool{}
		for _, n := range names { // <contrib dir>/<pkg>
			if i := strings.LastIndexByte(n, '/'); i > 0 {
				dirs[n[:i]] = true
			}
		}
		sorted := make([]string, 0, len(dirs))
		for d := range dirs {
			sorted = append(sorted, d)
		}
		sort.Strings(sorted)
		for _, d := range sorted {
			logf("%s: regenerating %s/PACKAGES", rp.Name, d)
			if err := h.rebuild(ctx, tx, rp, d); err != nil {
				return err
			}
		}
		return nil
	})
}
