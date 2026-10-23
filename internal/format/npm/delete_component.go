package npm

import (
	"context"
	"errors"
	"fmt"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// DeleteComponent removes one version of a hosted package in one
// incarnation-guarded transaction: its tarball asset and packages row, and the
// dist-tags that point at it. When latest pointed at it, latest moves to the
// newest remaining release (or the newest prerelease when only prereleases
// are left). When no version remains, the package document and its tags go
// too, so the package answers 404. A version that is gone already is not an
// error, and one whose tarball changed since cleanup evaluated it is skipped
// (cleanup.CheckUnchanged). actor is recorded on the event. Result counts
// the tarball, the component's only file; the package document is metadata.
// Cleanup ignores writePolicy.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "npm" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("npm component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("npm component %q is incomplete", c.Label())
	}
	name := c.Name
	if c.Group != "" {
		name = c.Group + "/" + c.Name
	}
	unlock := h.locks.Lock(rp.Name + "\x00" + name)
	defer unlock()

	var rowGone bool
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		tarball := TarballKey(name, TarballFile(name, c.Version))
		switch a, err := tx.GetAsset(ctx, rp.Name, tarball); {
		case err == nil:
			if err := cleanup.CheckUnchanged(c, []meta.Asset{a}); err != nil {
				return err
			}
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		switch err := tx.DeletePackage(ctx, rp.Name, name, c.Version); {
		case err == nil:
			rowGone = true
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		switch size, err := tx.DeleteAsset(ctx, rp.Name, tarball); {
		case err == nil:
			res.Add(size)
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		if !rowGone {
			return nil // no version row: the package document is unchanged
		}
		pkgs, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		tags, err := tx.ListDistTags(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		if len(pkgs) == 0 {
			for _, t := range tags {
				if err := tx.DeleteDistTag(ctx, rp.Name, name, t.Tag); err != nil {
					return err
				}
			}
			if _, err := tx.DeleteAsset(ctx, rp.Name, PackumentKey(name)); err != nil && !errors.Is(err, meta.ErrNotFound) {
				return err
			}
			return nil
		}
		for _, t := range tags {
			if t.Version != c.Version {
				continue
			}
			if t.Tag == "latest" {
				newest := ""
				for _, p := range pkgs {
					newest = semver.NewerRelease(newest, p.Version)
				}
				if err := tx.SetDistTag(ctx, rp.Name, name, "latest", newest); err != nil {
					return err
				}
				continue
			}
			if err := tx.DeleteDistTag(ctx, rp.Name, name, t.Tag); err != nil {
				return err
			}
		}
		return h.writePackument(ctx, tx, rp, name)
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowGone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
