package rubygems

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// push takes a gem into hosted rp: `POST api/v1/gems`, the .gem as the
// body (gem push). The server's maxUploadMiB bounds the body. An existing
// version is a 409 unless writePolicy is allow, when replacing it needs
// Edit. Answers 200 with the line gem push prints.
func (h *Handler) push(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, nil)
	if err != nil {
		return uploadErr(err)
	}
	if sums.Size == 0 {
		return badGem("empty upload")
	}
	a, err := h.readStored(rp, sums)
	if err != nil {
		return err
	}
	a.SHA256, a.Size = sums.SHA256, sums.Size
	attrs, err := json.Marshal(a)
	if err != nil {
		return err
	}
	name, ver := a.Name, a.rowVersion()
	now := h.Now().UTC()
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	ctx := r.Context()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		row := meta.Package{Repo: rp.Name, Format: "rubygems", Name: name, Version: ver, Attrs: string(attrs), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, row); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "gem %s version %s already exists in repository %q", name, ver, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s %s", auth.FromContext(ctx).User, name, ver)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, name, ver, row.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, gemFile(fullName(name, ver)), ctBinary, sums)); err != nil {
			return err
		}
		return h.writeVersions(ctx, tx, rp, name, ver, false)
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, name+"@"+ver, "")
	w.Header().Set("Content-Type", ctText)
	fmt.Fprintf(w, "Successfully registered gem: %s (%s)\n", name, ver)
	return nil
}

func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "gem is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badGem("upload ended early: %v", err)
	}
	return fmt.Errorf("reading gem: %w", err)
}

// readStored reads the specification from the stored blob, so nothing is
// buffered.
func (h *Handler) readStored(rp *repo.Repository, sums blob.Sums) (gemAttrs, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return gemAttrs{}, err
	}
	defer f.Close()
	return readGem(f)
}

// writeVersions updates rp's versions file in the transaction that changed
// the rows of gem name: a line for the version (prefixed "-" when removed)
// with the md5 of the gem's new info file is appended, Bundler fetching
// only what was appended. Without a file yet (or with name empty), the
// whole file is built from the rows.
func (h *Handler) writeVersions(ctx context.Context, tx *meta.Tx, rp *repo.Repository, name, version string, removed bool) error {
	var body []byte
	a, err := tx.GetAsset(ctx, rp.Name, versionsKey)
	switch {
	case err == nil && name != "":
		if body, err = h.readDoc(a); err != nil {
			return err
		}
		rs, err := rows(ctx, tx, rp.Name, name)
		if err != nil {
			return err
		}
		if removed {
			version = "-" + version
		}
		body = append(body, versionsLine(name, []string{version}, infoDoc(rs))...)
	case err == nil || errors.Is(err, meta.ErrNotFound):
		if body, err = buildVersions(ctx, tx, rp.Name, h.Now()); err != nil {
			return err
		}
	default:
		return err
	}
	sums, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(body), nil)
	if err != nil {
		return err
	}
	return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, versionsKey, ctText, sums))
}

// yank is `DELETE api/v1/gems/yank` (gem yank): the form's gem_name,
// version and optional platform name the version, which is deleted (its
// file and row; a yanked gem is gone from every index). 200, or 404 when
// there is no such version.
func (h *Handler) yank(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	form := r.URL.Query()
	if raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10)); err == nil { // DELETE bodies are not parsed by ParseForm
		if values, err := url.ParseQuery(string(raw)); err == nil {
			for k, v := range values {
				form[k] = append(form[k], v...)
			}
		}
	}
	name, version, platform := strings.TrimSpace(form.Get("gem_name")), strings.TrimSpace(form.Get("version")), strings.TrimSpace(form.Get("platform"))
	if !nameRE.MatchString(name) || !versionRE.MatchString(version) || platform != "" && platform != "ruby" && !platformRE.MatchString(platform) {
		return repo.Errorf(http.StatusBadRequest, "give gem_name, version and optionally platform")
	}
	ver := version
	if platform != "" && platform != "ruby" {
		ver += "-" + platform
	}
	c := component.Component{Repo: rp.Name, Format: "rubygems", Name: name, Version: ver}
	res, err := h.DeleteComponent(r.Context(), rp, c, auth.FromContext(r.Context()).Owner())
	if err != nil {
		return err
	}
	if res.Assets == 0 {
		return repo.Errorf(http.StatusNotFound, "gem %s version %s not found in repository %q", name, ver, rp.Name)
	}
	w.Header().Set("Content-Type", ctText)
	fmt.Fprintf(w, "Successfully deleted gem: %s (%s)\n", name, ver)
	return nil
}

// DeleteComponent removes one version of a hosted gem: its row and file,
// and appends the removal to the versions file, in one incarnation-guarded
// transaction under the repository's lock, for gem yank, the web UI's
// delete and cleanup policies. The other indexes are derived from the rows.
// A version that is gone already is not an error, and one that changed
// since an evaluation is skipped (cleanup.CheckUnchanged). Proxies use the
// generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "rubygems" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("rubygems component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("rubygems component %q is incomplete", c.Label())
	}
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	rowGone := false
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		file := gemFile(fullName(c.Name, c.Version))
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
		return h.writeVersions(ctx, tx, rp, c.Name, c.Version, true)
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowGone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
