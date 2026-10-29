package npm

import (
	"context"
	"fmt"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// LockPaths takes the locks a publish of paths to rp takes, for repository
// import (internal/transfer): one per package, <name>/-/<file> and
// <name>/package.json.
func (h *Handler) LockPaths(rp *repo.Repository, paths []string) (unlock func()) {
	keys := make([]string, len(paths))
	for i, p := range paths {
		name, _, ok := strings.Cut(p, "/-/")
		if !ok {
			name = strings.TrimSuffix(p, "/package.json")
		}
		keys[i] = rp.Name + "\x00" + name
	}
	return h.locks.LockAll(keys)
}

// RebuildMetadata regenerates the stored package document of every package
// of hosted rp from its rows, under each package's lock, as a publish does
// (repo.MetadataRebuilder; repository import uses it).
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	if rp.Format != "npm" || rp.Type != "hosted" {
		return fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return err
	}
	for _, name := range names {
		unlock := h.locks.Lock(rp.Name + "\x00" + name)
		err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error { return h.writePackument(ctx, tx, rp, name) })
		unlock()
		if err != nil {
			return fmt.Errorf("package %s: %w", name, err)
		}
	}
	logf("%s: regenerated %d package documents", rp.Name, len(names))
	return nil
}
