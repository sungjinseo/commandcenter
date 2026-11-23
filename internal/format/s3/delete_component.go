package s3

import (
	"context"
	"errors"
	"fmt"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DeleteComponent removes one object of a bucket for cleanup and the web
// UI: an s3 component is the object at the key its Name holds. An object
// already gone is not an error; one that changed since cleanup evaluated it
// is skipped (cleanup.CheckUnchanged). It takes the key's lock, as uploads
// do.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "s3" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("s3 component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version != "" {
		return component.Result{}, fmt.Errorf("s3 component %q must be an object key without a version", c.Label())
	}
	unlock, err := h.Store.Lock(ctx, lockKey(rp.Name, c.Name))
	if err != nil {
		return component.Result{}, err
	}
	defer unlock()
	var res component.Result
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = component.Result{}
		a, err := tx.GetAsset(ctx, rp.Name, c.Name)
		if errors.Is(err, meta.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := cleanup.CheckUnchanged(c, []meta.Asset{a}); err != nil {
			return err
		}
		size, err := tx.DeleteAsset(ctx, rp.Name, c.Name)
		if err != nil {
			return err
		}
		res.Add(size)
		return nil
	})
	if err != nil {
		return component.Result{}, err
	}
	if res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), "assets=1")
	}
	return res, nil
}
