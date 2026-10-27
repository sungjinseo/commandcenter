package raw

import (
	"context"
	"errors"
	"fmt"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DeleteComponent removes one file of a hosted repository: a raw component
// is the file at the path its Name holds (component.DeriveAsset). A file that
// is already gone is not an error. A file that changed since cleanup
// evaluated it is skipped (cleanup.CheckUnchanged). actor is recorded on the
// event. Cleanup ignores writePolicy: a read-only or allow_once repository
// is still cleaned.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "raw" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("raw component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version != "" {
		return component.Result{}, fmt.Errorf("raw component %q must be a file path without a version", c.Label())
	}
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
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
