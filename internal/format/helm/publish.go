package helm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// upload takes a chart into hosted rp: `PUT <name>-<version>.tgz` with the
// file as the body (curl --upload-file, as Nexus takes it), or helm cm-push's
// `POST api/charts`, a multipart form whose "chart" field is the file (a
// body that is not multipart is the file itself, as ChartMuseum takes it).
// Both answer 201 with ChartMuseum's {"saved":true}. The server's
// maxUploadMiB bounds the body.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	body, wantFile := io.Reader(r.Body), rt.File
	if rt.Kind == KindAPI {
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
			mr, err := r.MultipartReader()
			if err != nil {
				return badChart("%v", err)
			}
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					return badChart(`multipart body has no "chart" field`)
				}
				if err != nil {
					return uploadErr(err)
				}
				if part.FormName() == "chart" {
					body = part
					break
				}
			}
		}
	}
	name, version, err := h.store(r.Context(), rp, body, wantFile)
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, name+"@"+version, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]bool{"saved": true})
	return nil
}

// store streams a chart into rp: the blob first, then Chart.yaml is read back
// from it, and the row (its index entry), the file and the rebuilt index.yaml
// are recorded in one transaction under the repository's lock. wantFile,
// when set, must be the file name the chart's name and version give. An
// existing version is a 409 unless writePolicy is allow, when replacing it
// needs Edit.
func (h *Handler) store(ctx context.Context, rp *repo.Repository, body io.Reader, wantFile string) (name, version string, err error) {
	sums, err := h.Store.WriteBlob(rp.Name, body, nil)
	if err != nil {
		return "", "", uploadErr(err)
	}
	if sums.Size == 0 {
		return "", "", badChart("empty upload")
	}
	e, err := h.readStored(rp, sums)
	if err != nil {
		return "", "", err
	}
	name, version = e.str("name"), e.str("version")
	file := ChartFile(name, version)
	if wantFile != "" && !sameFile(file, wantFile) {
		return "", "", badChart("file name %q does not match chart %s version %s (%s)", wantFile, name, version, file)
	}
	now := h.Now().UTC()
	e["created"], e["digest"], e["urls"] = now.Format(time.RFC3339Nano), sums.SHA256, []string{file}
	attrs, err := json.Marshal(e)
	if err != nil {
		return "", "", badChart("Chart.yaml cannot be stored as JSON: %v", err)
	}
	// One lock per repository (each write rewrites index.yaml). ponytail: per
	// chart if hosted pushes ever queue up (they rebuild the whole index anyway).
	unlock, err := h.Store.Lock(ctx, "helm\x00"+rp.Name)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		row := meta.Package{Repo: rp.Name, Format: "helm", Name: name, Version: version, Attrs: string(attrs), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, row); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "chart %s version %s already exists in repository %q", name, version, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, file)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, name, version, row.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, file, ctChart, sums)); err != nil {
			return err
		}
		return h.rebuildIndex(ctx, tx, rp)
	})
	return name, version, err
}

// readStored reads Chart.yaml from the stored blob, so nothing is buffered.
func (h *Handler) readStored(rp *repo.Repository, sums blob.Sums) (entry, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readChart(f)
}

// rebuildIndex writes rp's index.yaml from its rows, in the transaction
// that changed them.
func (h *Handler) rebuildIndex(ctx context.Context, tx *meta.Tx, rp *repo.Repository) error {
	body, err := buildHosted(ctx, tx, rp.Name, h.Now())
	if err != nil {
		return err
	}
	sums, err := h.Store.WriteBlob(rp.Name, strings.NewReader(string(body)), nil)
	if err != nil {
		return err
	}
	return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, indexKey, ctYAML, sums))
}

// DeleteComponent removes one version of a hosted chart: its row and file,
// and rebuilds index.yaml, in one incarnation-guarded transaction under the
// repository's lock, for the web UI's delete and cleanup policies. A version
// that is gone already is not an error, and one that changed since an
// evaluation is skipped (cleanup.CheckUnchanged). Helm proxies use the
// generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "helm" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("helm component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("helm component %q is incomplete", c.Label())
	}
	unlock, err := h.Store.Lock(ctx, "helm\x00"+rp.Name)
	if err != nil {
		return component.Result{}, err
	}
	defer unlock()
	rowGone := false
	var res component.Result
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		file := ChartFile(c.Name, c.Version)
		switch a, err := tx.GetAsset(ctx, rp.Name, file); {
		case err == nil:
			if err := cleanup.CheckUnchanged(c, []meta.Asset{a}); err != nil {
				return err
			}
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		switch err := tx.DeletePackage(ctx, rp.Name, c.Name, c.Version); {
		case err == nil:
			rowGone = true
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		switch size, err := tx.DeleteAsset(ctx, rp.Name, file); {
		case err == nil:
			res.Add(size)
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		if !rowGone && res.Assets == 0 {
			return nil
		}
		return h.rebuildIndex(ctx, tx, rp)
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowGone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
