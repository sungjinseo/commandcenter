package yum

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// RebuildMetadata regenerates the repodata/ of every directory of hosted rp
// that holds packages or a comps.xml (one for depth 0, else each directory
// that deep) from its rows, under the repository's lock
// (repo.MetadataRebuilder). A package not that deep (uploaded before the
// depth was raised) is listed nowhere. The rows are first read again from the
// stored packages (reread).
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	if rp.Format != "yum" || rp.Type != "hosted" {
		return fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	unlock, err := h.Store.Lock(ctx, "yum\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	if err := h.reread(ctx, rp, logf); err != nil {
		return err
	}
	dirs := map[string]bool{}
	err = h.Store.DB.WalkAssetPaths(ctx, rp.Name, "", func(p string) {
		if d, ok := compsDir(rp, p); ok {
			dirs[d] = true
		}
	})
	if err != nil {
		return err
	}
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		names, err := tx.ListPackageNames(ctx, rp.Name)
		if err != nil {
			return err
		}
		for _, n := range names {
			if d, ok := repoDir(rp, n); ok {
				dirs[d] = true
			}
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

// reread parses the header of every stored package of rp again and writes
// the rows that change, each in a transaction of its own (a large
// repository's rows are not held in memory): a package uploaded before the
// server read a field (the weak dependencies) gains it, and one that kept
// its whole changelog loses all but the newest entries. The upload time and
// path stay. A package whose file cannot be read keeps its row. The caller
// holds the repository's lock, so the rows do not change meanwhile.
// ponytail: every rebuild reads every header (three ranged reads each on S3)
// under that lock; mark the rows already read if a large repository minds.
func (h *Handler) reread(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return err
	}
	changed := 0
	for _, name := range names {
		rows, err := h.Store.DB.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		for _, row := range rows {
			var old Package
			if err := json.Unmarshal([]byte(row.Attrs), &old); err != nil {
				return fmt.Errorf("package %s %s of %q: %w", row.Name, row.Version, rp.Name, err)
			}
			a, err := h.Store.DB.GetAsset(ctx, rp.Name, old.Location)
			if err != nil {
				logf("%s: %s is not read again: %v", rp.Name, old.Location, err)
				continue
			}
			pkg, err := h.readStored(rp, blob.Sums{SHA256: a.BlobSHA256, Size: a.Size})
			if err != nil {
				logf("%s: %s is not read again: %v", rp.Name, old.Location, err)
				continue
			}
			pkg.Checksum, pkg.Location, pkg.Uploaded = old.Checksum, old.Location, old.Uploaded
			attrs, err := json.Marshal(pkg)
			if err != nil {
				return err
			}
			if string(attrs) == row.Attrs {
				continue
			}
			err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
				return tx.UpdatePackage(ctx, rp.Name, row.Name, row.Version, string(attrs))
			})
			if err != nil {
				return err
			}
			changed++
		}
	}
	if changed > 0 {
		logf("%s: %d packages read again from their files", rp.Name, changed)
	}
	return nil
}
