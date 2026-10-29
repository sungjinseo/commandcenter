package npm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// ImportComponent copies one version of a package (its row and tarball)
// from hosted src into hosted dst, under dst's package lock, and rewrites
// dst's package document. A version dst has already is a conflict whatever
// overwrite says: npm never publishes over a version. latest moves to the
// version when it is the newest release (repo.ComponentImporter).
func (h *Handler) ImportComponent(ctx context.Context, src, dst *repo.Repository, c component.Component, _ bool) ([]meta.Asset, error) {
	if err := repo.CheckImport(src, dst, c, "npm"); err != nil {
		return nil, err
	}
	if c.Name == "" || c.Version == "" {
		return nil, fmt.Errorf("npm component %q is incomplete", c.Label())
	}
	name := c.Name
	if c.Group != "" {
		name = c.Group + "/" + c.Name
	}
	unlock, err := h.Store.Lock(ctx, "npm\x00"+dst.Name+"\x00"+name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	rows, err := h.Store.DB.ListPackages(ctx, src.Name, name)
	if err != nil {
		return nil, err
	}
	var row *meta.Package
	for i := range rows {
		if rows[i].Version == c.Version {
			row = &rows[i]
		}
	}
	tarball, err := h.Store.DB.GetAsset(ctx, src.Name, TarballKey(name, TarballFile(name, c.Version)))
	if row == nil || errors.Is(err, meta.ErrNotFound) {
		return nil, repo.Errorf(http.StatusNotFound, "%s@%s not found in repository %q", name, c.Version, src.Name)
	}
	if err != nil {
		return nil, err
	}
	assets := []meta.Asset{tarball}
	if err := h.Store.CopyBlobs(src.Name, dst.Name, assets); err != nil {
		return nil, err
	}
	return assets, h.Store.Tx(ctx, dst, func(tx *meta.Tx) error {
		if err := repo.InsertImported(ctx, tx, dst.Name, []meta.Package{*row}, false); err != nil {
			return err
		}
		if err := repo.PutImported(ctx, tx, dst.Name, assets, false); err != nil {
			return err
		}
		tags, err := tx.ListDistTags(ctx, dst.Name, name)
		if err != nil {
			return err
		}
		latest := ""
		for _, t := range tags {
			if t.Tag == "latest" {
				latest = t.Version
			}
		}
		if newest := semver.NewerRelease(latest, c.Version); newest != latest {
			if err := tx.SetDistTag(ctx, dst.Name, name, "latest", newest); err != nil {
				return err
			}
		}
		return h.writePackument(ctx, tx, dst, name)
	})
}
