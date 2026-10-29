package pypi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ImportComponent copies one version of a project (the rows of its files,
// the files and their metadata sidecars) from hosted src into hosted dst,
// under dst's project lock. A file dst has already, in any spelling of its
// name, is a conflict whatever overwrite says, as for an upload. The simple
// index is derived from the rows (repo.ComponentImporter).
func (h *Handler) ImportComponent(ctx context.Context, src, dst *repo.Repository, c component.Component, _ bool) ([]meta.Asset, error) {
	if err := repo.CheckImport(src, dst, c, "pypi"); err != nil {
		return nil, err
	}
	if c.Name == "" || c.Version == "" {
		return nil, fmt.Errorf("pypi component %q is incomplete", c.Label())
	}
	project := Normalize(c.Name)
	unlock, err := h.Store.Lock(ctx, "pypi\x00"+dst.Name+"\x00"+project)
	if err != nil {
		return nil, err
	}
	defer unlock()
	all, err := h.Store.DB.ListPackages(ctx, src.Name, project)
	if err != nil {
		return nil, err
	}
	var rows []meta.Package
	var assets []meta.Asset
	for _, p := range all {
		if component.PyPIDisplayVersion(project, p.Version) != c.Version {
			continue
		}
		rows = append(rows, p)
		for _, key := range []string{fileKey(project, p.Version), metadataKey(project, p.Version)} {
			switch a, err := h.Store.DB.GetAsset(ctx, src.Name, key); {
			case err == nil:
				assets = append(assets, a)
			case !errors.Is(err, meta.ErrNotFound):
				return nil, err
			}
		}
	}
	if len(rows) == 0 {
		return nil, repo.Errorf(http.StatusNotFound, "%s %s not found in repository %q", project, c.Version, src.Name)
	}
	if err := h.Store.CopyBlobs(src.Name, dst.Name, assets); err != nil {
		return nil, err
	}
	return assets, h.Store.Tx(ctx, dst, func(tx *meta.Tx) error {
		have, err := tx.ListPackages(ctx, dst.Name, project)
		if err != nil {
			return err
		}
		for _, p := range rows {
			if err := duplicate(project, have, p.Version); err != nil {
				return repo.Exists(p.Version, dst.Name)
			}
		}
		if err := repo.InsertImported(ctx, tx, dst.Name, rows, false); err != nil {
			return err
		}
		return repo.PutImported(ctx, tx, dst.Name, assets, false)
	})
}
