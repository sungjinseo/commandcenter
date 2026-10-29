package nuget

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ImportComponent copies one version of a package (its row, .nupkg and
// .nuspec) from hosted src into hosted dst, under dst's package lock.
// Registration and flat container are derived from the rows
// (repo.ComponentImporter).
func (h *Handler) ImportComponent(ctx context.Context, src, dst *repo.Repository, c component.Component, overwrite bool) ([]meta.Asset, error) {
	if err := repo.CheckImport(src, dst, c, "nuget"); err != nil {
		return nil, err
	}
	if c.Name == "" || c.Version == "" {
		return nil, fmt.Errorf("nuget component %q is incomplete", c.Label())
	}
	id, ver := strings.ToLower(c.Name), strings.ToLower(c.Version)
	unlock, err := h.Store.Lock(ctx, "nuget\x00"+dst.Name+"\x00"+id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	rows, err := h.Store.DB.ListPackages(ctx, src.Name, id)
	if err != nil {
		return nil, err
	}
	var row []meta.Package
	for _, p := range rows {
		if p.Version == ver {
			row = append(row, p)
		}
	}
	var assets []meta.Asset
	for _, p := range []string{assetPath(id, ver, NupkgFile(id, ver)), assetPath(id, ver, NuspecFile(id, ver))} {
		switch a, err := h.Store.DB.GetAsset(ctx, src.Name, p); {
		case err == nil:
			assets = append(assets, a)
		case !errors.Is(err, meta.ErrNotFound):
			return nil, err
		}
	}
	if len(row) == 0 || len(assets) == 0 {
		return nil, repo.Errorf(http.StatusNotFound, "%s %s not found in repository %q", id, ver, src.Name)
	}
	if err := h.Store.CopyBlobs(src.Name, dst.Name, assets); err != nil {
		return nil, err
	}
	return assets, h.Store.Tx(ctx, dst, func(tx *meta.Tx) error {
		if err := repo.InsertImported(ctx, tx, dst.Name, row, overwrite); err != nil {
			return err
		}
		return repo.PutImported(ctx, tx, dst.Name, assets, overwrite)
	})
}
