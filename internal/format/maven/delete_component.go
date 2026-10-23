package maven

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DeleteComponent removes every file of a hosted component (the artifacts and
// their sidecars, such as signatures) in one incarnation-guarded transaction,
// then rebuilds the GA maven-metadata.xml the way DELETE does. Only files
// directly in the version directory count: a GA nested under it is another
// component. A component that has no files left is not an error. A component
// that changed since cleanup evaluated it is skipped (cleanup.CheckUnchanged).
// actor is recorded on the event. Cleanup ignores writePolicy: a
// read-only or allow_once repository is still cleaned.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "maven" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("maven component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Group == "" || c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("maven component %q is incomplete", c.Label())
	}
	verDir := strings.ReplaceAll(c.Group, ".", "/") + "/" + c.Name + "/" + c.Version
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = component.Result{}
		assets, err := tx.ListAssets(ctx, rp.Name, verDir+"/")
		if err != nil {
			return err
		}
		var files []meta.Asset
		for _, a := range assets {
			if dir(a.Path) != verDir {
				continue
			}
			g, name, v, ok := component.DeriveMaven(a.Path)
			if !ok || g != c.Group || name != c.Name || v != c.Version {
				continue
			}
			files = append(files, a)
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
	if res.Assets == 0 {
		return res, nil
	}
	h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	// The metadata writes are not part of the transaction: like DELETE they are
	// best-effort, and skipped once the repository was replaced.
	if h.Store.Current(rp) != nil {
		return res, nil
	}
	if err := h.rebuildGA(ctx, rp, c.Group, c.Name, true); err != nil {
		slog.Warn("maven-metadata.xml rebuild failed", "repo", rp.Name, "component", c.Label(), "err", logsafe.Err(err))
	}
	if strings.HasSuffix(c.Version, "-SNAPSHOT") {
		if err := h.rebuildSnapshotVersion(ctx, rp, verDir); err != nil {
			slog.Warn("snapshot version metadata rebuild failed", "repo", rp.Name, "component", c.Label(), "err", logsafe.Err(err))
		}
	}
	return res, nil
}
