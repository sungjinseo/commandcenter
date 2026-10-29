package yum

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ImportComponent copies one package (its row and .rpm) from hosted src
// into hosted dst at the same path, and rebuilds the repodata of its
// directory in dst in the same transaction under dst's lock, as an upload
// does. The path must suit dst's depth (repo.ComponentImporter).
func (h *Handler) ImportComponent(ctx context.Context, src, dst *repo.Repository, c component.Component, overwrite bool) ([]meta.Asset, error) {
	if err := repo.CheckImport(src, dst, c, "yum"); err != nil {
		return nil, err
	}
	if c.Name == "" || c.Version == "" {
		return nil, fmt.Errorf("yum component %q is incomplete", c.Label())
	}
	file := c.Name + "-" + c.Version + ".rpm"
	dir, ok := repoDir(dst, file)
	if !ok {
		return nil, repo.Errorf(http.StatusBadRequest, "repository %q has depth 1: %s is not in a directory", dst.Name, file)
	}
	unlock, err := h.Store.Lock(ctx, "yum\x00"+dst.Name)
	if err != nil {
		return nil, err
	}
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
	rpm, err := h.Store.DB.GetAsset(ctx, src.Name, file)
	if len(row) == 0 || errors.Is(err, meta.ErrNotFound) {
		return nil, repo.Errorf(http.StatusNotFound, "package %s not found in repository %q", file, src.Name)
	}
	if err != nil {
		return nil, err
	}
	assets := []meta.Asset{rpm}
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
		return h.rebuild(ctx, tx, dst, dir)
	})
}
