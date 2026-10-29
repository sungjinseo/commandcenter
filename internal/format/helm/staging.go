package helm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ImportComponent copies one chart version (its row, the index entry, and
// its file) from hosted src into hosted dst, and rebuilds dst's index.yaml
// in the same transaction under dst's lock, as an upload does
// (repo.ComponentImporter).
func (h *Handler) ImportComponent(ctx context.Context, src, dst *repo.Repository, c component.Component, overwrite bool) ([]meta.Asset, error) {
	if err := repo.CheckImport(src, dst, c, "helm"); err != nil {
		return nil, err
	}
	if c.Name == "" || c.Version == "" {
		return nil, fmt.Errorf("helm component %q is incomplete", c.Label())
	}
	unlock := h.locks.Lock(dst.Name)
	defer unlock()
	rows, err := h.Store.DB.ListPackages(ctx, src.Name, c.Name)
	if err != nil {
		return nil, err
	}
	var row []meta.Package
	for _, p := range rows {
		if p.Version == c.Version {
			row = append(row, p)
		}
	}
	file, err := h.Store.DB.GetAsset(ctx, src.Name, ChartFile(c.Name, c.Version))
	if len(row) == 0 || errors.Is(err, meta.ErrNotFound) {
		return nil, repo.Errorf(http.StatusNotFound, "chart %s version %s not found in repository %q", c.Name, c.Version, src.Name)
	}
	if err != nil {
		return nil, err
	}
	assets := []meta.Asset{file}
	if err := h.Store.CopyBlobs(src.Name, dst.Name, assets); err != nil {
		return nil, err
	}
	return assets, h.Store.Tx(ctx, dst, func(tx *meta.Tx) error {
		if err := repo.InsertImported(ctx, tx, dst.Name, row, overwrite); err != nil {
			return err
		}
		if err := repo.PutImported(ctx, tx, dst.Name, assets, overwrite); err != nil {
			return err
		}
		return h.rebuildIndex(ctx, tx, dst)
	})
}
