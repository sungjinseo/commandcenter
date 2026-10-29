package composer

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// maxComposerJSON bounds a composer.json read out of an archive.
const maxComposerJSON = 1 << 20

func badPackage(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, "package: "+format, args...)
}

// row is one stored version of a hosted package: the normalized version
// and the metadata entry the upload stored as the row's attrs.
type row struct {
	version string
	entry   entry
	pkg     meta.Package
}

// rows lists the versions of name in hosted rp, highest normalized version
// first (by string: Composer sorts versions itself). A row whose attrs
// cannot be read still lists with its name and version.
func (h *Handler) rows(ctx context.Context, rp *repo.Repository, name string) ([]row, error) {
	pkgs, err := h.Store.DB.ListPackages(ctx, rp.Name, name)
	if err != nil {
		return nil, err
	}
	out := make([]row, 0, len(pkgs))
	for _, p := range pkgs {
		r := row{version: p.Version, pkg: p}
		if json.Unmarshal([]byte(p.Attrs), &r.entry) != nil || r.entry["name"] == nil {
			r.entry = entry{"name": p.Name, "version": p.Version, "version_normalized": p.Version}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version > out[j].version })
	return out, nil
}

// upload takes a version into hosted rp: `PUT packages/upload/<vendor>/
// <package>/<version>` with the zip as the body. composer.json is read from
// the archive's root (or its first directory, as git archives are laid
// out) and must name vendor/package; the version comes from the URL and
// is normalized as Composer does. The server's maxUploadMiB bounds the
// body. An existing version is a 409 unless writePolicy is allow, when
// replacing it needs Edit. Answers 201.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	normalized, ok := Normalize(rt.Version)
	if !ok {
		return badPackage("invalid version %q", rt.Version)
	}
	if strings.Contains(normalized, "/") {
		return badPackage("version %q: a branch name with \"/\" cannot be uploaded", rt.Version)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, nil)
	if err != nil {
		return uploadErr(err)
	}
	if sums.Size == 0 {
		return badPackage("empty upload")
	}
	e, err := h.readStored(rp, sums)
	if err != nil {
		return err
	}
	if name, _ := e["name"].(string); strings.ToLower(strings.TrimSpace(name)) != rt.Name {
		return badPackage("composer.json names %q, not %s", name, rt.Name)
	}
	if v, _ := e["version"].(string); v != "" {
		if nv, ok := Normalize(v); !ok || nv != normalized {
			return badPackage("composer.json version %q is not %s", v, rt.Version)
		}
	}
	now := h.Now().UTC()
	delete(e, "source")
	e["name"], e["version"], e["version_normalized"] = rt.Name, rt.Version, normalized
	e["dist"] = entry{"type": "zip", "shasum": sums.SHA1} // the URL is added when served
	e["time"] = now.Format(time.RFC3339)
	attrs, err := json.Marshal(e)
	if err != nil {
		return err
	}
	unlock := h.locks.Lock(rp.Name + "\x00" + rt.Name)
	defer unlock()
	ctx := r.Context()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		row := meta.Package{Repo: rp.Name, Format: "composer", Name: rt.Name, Version: normalized, Attrs: string(attrs), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, row); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "package %s version %s already exists in repository %q", rt.Name, normalized, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s %s", auth.FromContext(ctx).User, rt.Name, normalized)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, rt.Name, normalized, row.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, distPath(rt.Name, normalized), ctZip, sums))
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, rt.Name+"@"+normalized, "")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// readStored reads composer.json out of the stored archive, so nothing is
// buffered: the file at the root, else the first one directory down.
func (h *Handler) readStored(rp *repo.Repository, sums blob.Sums) (entry, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readArchive(f, sums.Size)
}

func readArchive(r io.ReaderAt, size int64) (entry, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, badPackage("not a zip: %v", err)
	}
	var found *zip.File
	for _, f := range zr.File {
		if f.Name == "composer.json" {
			found = f
			break
		}
		if found == nil && strings.HasSuffix(f.Name, "/composer.json") && strings.Count(f.Name, "/") == 1 {
			found = f
		}
	}
	if found == nil {
		return nil, badPackage("no composer.json at the root of the archive")
	}
	if found.UncompressedSize64 > maxComposerJSON {
		return nil, badPackage("%s is larger than %d bytes", found.Name, maxComposerJSON)
	}
	rc, err := found.Open()
	if err != nil {
		return nil, badPackage("reading %s: %v", found.Name, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxComposerJSON+1))
	if err != nil || len(raw) > maxComposerJSON {
		return nil, badPackage("reading %s: %v", found.Name, errors.Join(err, errors.New("too large")))
	}
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil || e == nil {
		return nil, badPackage("%s is not a JSON object: %v", found.Name, err)
	}
	return e, nil
}

// unpublish is `DELETE packages/upload/<vendor>/<package>/<version>`: the
// version's row and archive are deleted. 204, or 404 when there is no such
// version.
func (h *Handler) unpublish(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	normalized, ok := Normalize(rt.Version)
	if !ok {
		return badPackage("invalid version %q", rt.Version)
	}
	c := component.Component{Repo: rp.Name, Format: "composer", Name: rt.Name, Version: normalized}
	res, err := h.DeleteComponent(r.Context(), rp, c, auth.FromContext(r.Context()).Owner())
	if err != nil {
		return err
	}
	if res.Assets == 0 {
		return repo.Errorf(http.StatusNotFound, "package %s version %s not found in repository %q", rt.Name, normalized, rp.Name)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// DeleteComponent removes one version of a hosted package: its row and
// archive, in one incarnation-guarded transaction under the package's
// lock, for the upload API's DELETE, the web UI's delete and cleanup
// policies. The metadata files are derived from the rows, so nothing else
// changes. A version that is gone already is not an error, and one that
// changed since an evaluation is skipped (cleanup.CheckUnchanged). Proxies
// use the generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "composer" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("composer component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("composer component %q is incomplete", c.Label())
	}
	name := strings.ToLower(c.Name)
	unlock := h.locks.Lock(rp.Name + "\x00" + name)
	defer unlock()
	rowGone := false
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		p := distPath(name, c.Version)
		var have []meta.Asset
		switch a, err := tx.GetAsset(ctx, rp.Name, p); {
		case err == nil:
			have = append(have, a)
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		if err := cleanup.CheckUnchanged(c, have); err != nil {
			return err
		}
		switch err := tx.DeletePackage(ctx, rp.Name, name, c.Version); {
		case err == nil:
			rowGone = true
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		switch size, err := tx.DeleteAsset(ctx, rp.Name, p); {
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

// uploadErr maps a body read error to its status.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "package is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badPackage("upload ended early: %v", err)
	}
	return fmt.Errorf("reading package: %w", err)
}
