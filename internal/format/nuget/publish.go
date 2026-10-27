package nuget

import (
	"bytes"
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

// push takes a package into hosted rp: `PUT api/v2/package`, the .nupkg as
// a multipart form's file part (dotnet nuget push, nuget.exe) or as the
// body itself (curl -T). The server's maxUploadMiB bounds the body. An
// existing version is a 409 unless writePolicy is allow, when replacing it
// needs Edit. Answers 201.
func (h *Handler) push(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	body, filename := io.Reader(r.Body), ""
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			return badPackage("%v", err)
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return badPackage("multipart body has no file part")
			}
			if err != nil {
				return uploadErr(err)
			}
			if part.FileName() != "" || part.FormName() == "package" {
				body, filename = part, part.FileName()
				break
			}
		}
	}
	if strings.HasSuffix(strings.ToLower(filename), ".snupkg") {
		return badPackage("symbol packages (.snupkg) are not supported; push the .nupkg only")
	}
	sums, err := h.Store.WriteBlob(rp.Name, body, nil)
	if err != nil {
		return uploadErr(err)
	}
	if sums.Size == 0 {
		return badPackage("empty upload")
	}
	a, raw, err := h.readStored(rp, sums)
	if err != nil {
		return err
	}
	id, ver := strings.ToLower(a.ID), strings.ToLower(a.Version)
	now := h.Now().UTC()
	a.Listed, a.Published, a.PackageHash, a.Size = true, now.Format(time.RFC3339), hashBase64(sums.SHA512), sums.Size
	attrs, err := json.Marshal(a)
	if err != nil {
		return err
	}
	nuspecSums, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(raw), nil)
	if err != nil {
		return err
	}
	unlock := h.locks.Lock(rp.Name + "\x00" + id)
	defer unlock()
	ctx := r.Context()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		row := meta.Package{Repo: rp.Name, Format: "nuget", Name: id, Version: ver, Attrs: string(attrs), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, row); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "package %s version %s already exists in repository %q", a.ID, a.Version, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s %s", auth.FromContext(ctx).User, a.ID, a.Version)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, id, ver, row.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, assetPath(id, ver, NupkgFile(id, ver)), ctNupkg, sums)); err != nil {
			return err
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, assetPath(id, ver, NuspecFile(id, ver)), ctNuspec, nuspecSums))
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, id+"@"+ver, "")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// readStored reads the .nuspec from the stored blob, so nothing is buffered.
func (h *Handler) readStored(rp *repo.Repository, sums blob.Sums) (pkgAttrs, []byte, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return pkgAttrs{}, nil, err
	}
	defer f.Close()
	return readNupkg(f, sums.Size)
}

// unpublish is `DELETE api/v2/package/<id>/<version>`: the version is
// deleted (its files and row; there is no unlisted state). 204, or 404
// when there is no such version.
func (h *Handler) unpublish(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	c := component.Component{Repo: rp.Name, Format: "nuget", Name: rt.ID, Version: rt.Version}
	res, err := h.DeleteComponent(r.Context(), rp, c, auth.FromContext(r.Context()).Owner())
	if err != nil {
		return err
	}
	if res.Assets == 0 {
		return repo.Errorf(http.StatusNotFound, "package %s version %s not found in repository %q", rt.ID, rt.Version, rp.Name)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// DeleteComponent removes one version of a hosted package: its row, .nupkg
// and .nuspec, in one incarnation-guarded transaction under the package's
// lock, for the push API's DELETE, the web UI's delete and cleanup
// policies. Registration and flat container are derived from the rows, so
// nothing else changes. A version that is gone already is not an error,
// and one that changed since an evaluation is skipped
// (cleanup.CheckUnchanged). Proxies use the generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "nuget" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("nuget component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("nuget component %q is incomplete", c.Label())
	}
	id, ver := strings.ToLower(c.Name), strings.ToLower(c.Version)
	unlock := h.locks.Lock(rp.Name + "\x00" + id)
	defer unlock()
	rowGone := false
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		files := []string{assetPath(id, ver, NupkgFile(id, ver)), assetPath(id, ver, NuspecFile(id, ver))}
		var have []meta.Asset
		for _, p := range files {
			switch a, err := tx.GetAsset(ctx, rp.Name, p); {
			case err == nil:
				have = append(have, a)
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
		}
		if err := cleanup.CheckUnchanged(c, have); err != nil {
			return err
		}
		switch err := tx.DeletePackage(ctx, rp.Name, id, ver); {
		case err == nil:
			rowGone = true
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		for _, p := range files {
			switch size, err := tx.DeleteAsset(ctx, rp.Name, p); {
			case err == nil:
				res.Add(size)
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
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
