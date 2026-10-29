package raw

import (
	"context"
	"errors"
	"net/http"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ImportComponent copies the file c names from hosted src into hosted dst
// at the same path, under dst's per-path lock (repo.ComponentImporter).
func (h *Handler) ImportComponent(ctx context.Context, src, dst *repo.Repository, c component.Component, overwrite bool) ([]meta.Asset, error) {
	if err := repo.CheckImport(src, dst, c, "raw"); err != nil {
		return nil, err
	}
	unlock := h.locks.Lock(dst.Name + "\x00" + c.Name)
	defer unlock()
	a, err := h.Store.DB.GetAsset(ctx, src.Name, c.Name)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, repo.Errorf(http.StatusNotFound, "%s not found in repository %q", c.Name, src.Name)
	}
	if err != nil {
		return nil, err
	}
	assets := []meta.Asset{a}
	if err := h.Store.CopyBlobs(src.Name, dst.Name, assets); err != nil {
		return nil, err
	}
	return assets, h.Store.Tx(ctx, dst, func(tx *meta.Tx) error {
		return repo.PutImported(ctx, tx, dst.Name, assets, overwrite)
	})
}
