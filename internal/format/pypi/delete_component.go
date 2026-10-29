package pypi

import (
	"context"
	"errors"
	"fmt"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DeleteComponent removes one version of a hosted project: every file of that
// version (its packages row and asset) in one incarnation-guarded transaction.
// A row holds one file, keyed by file name; its version is derived from the
// name as the component search does. The file's core metadata sidecar goes
// with it. A version that is gone already is not an
// error, and one that changed since cleanup evaluated it (a file added,
// replaced or downloaded) is skipped (cleanup.CheckUnchanged). actor is
// recorded on the event. Cleanup ignores writePolicy.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "pypi" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("pypi component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("pypi component %q is incomplete", c.Label())
	}
	project := Normalize(c.Name)
	unlock, err := h.Store.Lock(ctx, "pypi\x00"+rp.Name+"\x00"+project)
	if err != nil {
		return component.Result{}, err
	}
	defer unlock()

	rows := 0
	var res component.Result
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rows, res = 0, component.Result{}
		pkgs, err := tx.ListPackages(ctx, rp.Name, project)
		if err != nil {
			return err
		}
		var versions []string
		var files []meta.Asset
		for _, p := range pkgs {
			if component.PyPIDisplayVersion(project, p.Version) != c.Version {
				continue
			}
			versions = append(versions, p.Version)
			switch a, err := tx.GetAsset(ctx, rp.Name, fileKey(project, p.Version)); {
			case err == nil:
				files = append(files, a)
			case errors.Is(err, meta.ErrNotFound):
				files = append(files, meta.Asset{Path: fileKey(project, p.Version)}) // a row without its file still counts as a file of the version
			default:
				return err
			}
		}
		if err := cleanup.CheckUnchanged(c, files); err != nil {
			return err
		}
		for _, file := range versions {
			if err := tx.DeletePackage(ctx, rp.Name, project, file); err != nil {
				return err
			}
			rows++
			for _, key := range []string{fileKey(project, file), metadataKey(project, file)} {
				switch size, err := tx.DeleteAsset(ctx, rp.Name, key); {
				case err == nil:
					res.Add(size)
				case !errors.Is(err, meta.ErrNotFound):
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return component.Result{}, err
	}
	if rows > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
