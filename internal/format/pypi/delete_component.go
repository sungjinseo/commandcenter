package pypi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DeleteComponent removes one version of a hosted project: every file of that
// version (its packages row and asset) in one incarnation-guarded transaction.
// A row holds one file, keyed by file name; its version is derived from the
// name as the component search does. The file's core metadata sidecar and
// signature go with it. A version that is gone already is not an
// error, and one that changed since cleanup evaluated it (a file added,
// replaced or downloaded) is skipped (cleanup.CheckUnchanged). actor is
// recorded on the event. Cleanup ignores writePolicy.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "pypi" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("pypi component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("pypi component %q is incomplete", c.Label())
	}
	project := Normalize(c.Name)
	unlock, err := h.Store.Lock(ctx, "pypi\x00"+rp.Name+"\x00"+project)
	if err != nil {
		return component.Result{}, err
	}
	defer unlock()

	rows := 0
	var res component.Result
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rows, res = 0, component.Result{}
		pkgs, err := tx.ListPackages(ctx, rp.Name, project)
		if err != nil {
			return err
		}
		var versions []string
		var files []meta.Asset
		for _, p := range pkgs {
			if component.PyPIDisplayVersion(project, p.Version) != c.Version {
				continue
			}
			versions = append(versions, p.Version)
			switch a, err := tx.GetAsset(ctx, rp.Name, fileKey(project, p.Version)); {
			case err == nil:
				files = append(files, a)
			case errors.Is(err, meta.ErrNotFound):
				files = append(files, meta.Asset{Path: fileKey(project, p.Version)}) // a row without its file still counts as a file of the version
			default:
				return err
			}
		}
		if err := cleanup.CheckUnchanged(c, files); err != nil {
			return err
		}
		for _, file := range versions {
			if err := tx.DeletePackage(ctx, rp.Name, project, file); err != nil {
				return err
			}
			rows++
			if err := removeFile(ctx, tx, rp.Name, project, file, &res); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return component.Result{}, err
	}
	if rows > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}

// removeFile deletes the assets of a file (fileKeys) in tx, adding them to res.
func removeFile(ctx context.Context, tx *meta.Tx, repoName, project, file string, res *component.Result) error {
	for _, key := range fileKeys(project, file) {
		switch size, err := tx.DeleteAsset(ctx, repoName, key); {
		case err == nil:
			res.Add(size)
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
	}
	return nil
}

// deleteFile answers DELETE of one file of hosted rp, which is also what the
// Nexus API's delete of an asset does: a distribution file goes with its row,
// core metadata and signature, a signature alone (its file's row forgets it).
// A project ends with its last file, as its page is read from the rows. 204,
// or 404 when there is no such file.
func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request, rp *repo.Repository, project, file string) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	ctx := r.Context()
	unlock, err := h.Store.Lock(ctx, "pypi\x00"+rp.Name+"\x00"+project)
	if err != nil {
		return err
	}
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		if err := tx.DeletePackage(ctx, rp.Name, project, file); err != nil && !errors.Is(err, meta.ErrNotFound) {
			return err
		}
		var res component.Result
		if err := removeFile(ctx, tx, rp.Name, project, file, &res); err != nil {
			return err
		}
		if res.Assets == 0 {
			return repo.Errorf(http.StatusNotFound, "file %q of project %q not found", file, project)
		}
		if dist, ok := strings.CutSuffix(file, sigExt); ok {
			return editRow(ctx, tx, rp.Name, project, dist, func(a *fileAttrs) { a.HasSig = false })
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "delete", rp.Name, file, "")
	w.WriteHeader(http.StatusNoContent)
	return nil
}
