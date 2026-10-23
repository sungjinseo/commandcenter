package golang

import (
	"context"
	"errors"
	"fmt"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DeleteComponent removes module@version from a hosted repository: its .mod,
// .info and .zip in one incarnation-guarded transaction. The hosted @v/list
// is derived from the .mod assets, so the version leaves it with them. A
// version that is gone already is not an error, and one that changed since
// cleanup evaluated it is skipped (cleanup.CheckUnchanged). actor is recorded
// on the event. Cleanup ignores writePolicy.
//
// Only the case-encoded spelling is deleted. Hosted uploads always store
// encoded paths (ParseRoute rejects anything else), so a raw spelling, which
// DeriveGo keeps for paths that do not decode, cannot name a hosted file.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "go" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("go component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("go component %q is incomplete", c.Label())
	}
	module, version := Escape(c.Name), Escape(c.Version)
	unlock := h.locks.Lock(rp.Name + "\x00" + module + "@" + version)
	defer unlock()

	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = component.Result{}
		var files []meta.Asset
		for _, k := range []Kind{KindMod, KindInfo, KindZip} {
			switch a, err := tx.GetAsset(ctx, rp.Name, assetKey(module, version+ext(k))); {
			case err == nil:
				files = append(files, a)
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
		}
		if err := cleanup.CheckUnchanged(c, files); err != nil {
			return err
		}
		for _, a := range files {
			size, err := tx.DeleteAsset(ctx, rp.Name, a.Path)
			if err != nil {
				return err
			}
			res.Add(size)
		}
		return nil
	})
	if err != nil {
		return component.Result{}, err
	}
	if res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
