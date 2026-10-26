package cargo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DeleteComponent removes one version of a hosted crate: its index line
// (packages row) and its .crate, in one incarnation-guarded transaction under
// the crate's lock, so the index stops listing it (its ETag follows the rows)
// and the download answers 404. Publishers cannot delete (yank retires a
// version that lockfiles may still pin); this is the web UI's and API's
// delete, with the delete privilege. Cleanup never reaches it: policies skip
// Cargo hosted repositories (cleanup.Evaluate). A version that is gone
// already is not an error, and one that changed since an evaluation is
// skipped (cleanup.CheckUnchanged). actor is recorded on the event. Cargo
// proxies use the generic proxy deleter instead.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "cargo" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("cargo component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("cargo component %q is incomplete", c.Label())
	}
	name := strings.ToLower(c.Name)
	unlock := h.locks.Lock(rp.Name + "\x00" + canonicalName(name))
	defer unlock()

	rowGone := false
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		key := crateKey(name, c.Version)
		switch a, err := tx.GetAsset(ctx, rp.Name, key); {
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
		switch size, err := tx.DeleteAsset(ctx, rp.Name, key); {
		case err == nil:
			res.Add(size)
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		return nil
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowGone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
