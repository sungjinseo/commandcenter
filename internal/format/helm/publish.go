package helm

import (
	"bufio"
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

// upload takes a chart into hosted rp: `PUT <name>-<version>.tgz` with the
// file as the body (curl --upload-file, as Nexus takes it), or helm cm-push's
// `POST api/charts`, a multipart form whose "chart" field is the file (a
// body that is not multipart is the file itself, as ChartMuseum takes it).
// A provenance file comes by `PUT <name>-<version>.tgz.prov`, as the form's
// "prov" field (ChartMuseum), or where a chart is taken on api/charts
// (Nexus: a "chart" field that holds one); it is stored after the chart of
// the same request. All answer 201 with ChartMuseum's {"saved":true}. The
// server's maxUploadMiB bounds the body.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	ctx := r.Context()
	var subject string // of the event: the chart, or the provenance file that came alone
	var prov []byte
	// take stores a chart, or keeps a provenance file (clear-signed text) for
	// after the chart; the first of each counts, as a form's first field does.
	take := func(body io.Reader, isProv bool) error {
		br := bufio.NewReader(body)
		if head, _ := br.Peek(len(provHead)); isProv || rt.Kind == KindAPI && string(head) == provHead {
			if prov != nil {
				return nil
			}
			b, err := io.ReadAll(io.LimitReader(br, maxProv+1))
			switch {
			case err != nil:
				return uploadErr(err)
			case len(b) > maxProv:
				return repo.Errorf(http.StatusRequestEntityTooLarge, "provenance file is larger than %d MiB", maxProv>>20)
			}
			prov = b
			return nil
		}
		if subject != "" {
			return nil
		}
		name, version, err := h.store(ctx, rp, br, rt.File)
		subject = name + "@" + version
		return err
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); rt.Kind == KindAPI && mt == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			return badChart("%v", err)
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return uploadErr(err)
			}
			if n := part.FormName(); n == "chart" || n == "prov" {
				if err := take(part, n == "prov"); err != nil {
					return err
				}
			}
		}
		if subject == "" && prov == nil {
			return badChart(`multipart body has no "chart" field`)
		}
	} else if err := take(r.Body, rt.Kind == KindProv); err != nil {
		return err
	}
	if prov != nil {
		wantFile := ""
		if rt.Kind == KindProv {
			wantFile = rt.File
		}
		file, err := h.storeProv(ctx, rp, prov, wantFile)
		if err != nil {
			return err
		}
		if subject == "" {
			subject = provKey(file)
		}
	}
	h.Store.Emit(r, "upload", rp.Name, subject, "")
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
// needs Edit and drops the provenance file of the content it replaces.
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
			if old, err := tx.GetAsset(ctx, rp.Name, file); err == nil && old.BlobSHA256 != sums.SHA256 {
				if _, err := tx.DeleteAsset(ctx, rp.Name, provKey(file)); err != nil && !errors.Is(err, meta.ErrNotFound) {
					return err
				}
			}
		case err != nil:
			return err
		default: // a new version: another chart can have its file name (demo 1.0.0-2.3.4 and demo-1.0.0 2.3.4)
			if _, err := tx.GetAsset(ctx, rp.Name, file); err == nil {
				return repo.Errorf(http.StatusConflict, "repository %q has a file %s already, of another chart", rp.Name, file)
			} else if !errors.Is(err, meta.ErrNotFound) {
				return err
			}
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, file, ctChart, sums)); err != nil {
			return err
		}
		return h.rebuildIndex(ctx, tx, rp)
	})
	return name, version, err
}

// storeProv stores a chart's provenance file beside the chart it signs,
// under the repository's lock. The chart must be in rp with the sha256 the
// file signs, so a stored provenance file is always the stored chart's;
// wantFile, when set, must be that chart's file name. As in Nexus one may
// be replaced whatever the writePolicy (a chart signed again); replacing
// needs Edit. It returns the chart's file name.
func (h *Handler) storeProv(ctx context.Context, rp *repo.Repository, prov []byte, wantFile string) (file string, err error) {
	file, digest, err := provFile(prov)
	if err != nil {
		return "", err
	}
	if wantFile != "" && !sameFile(file, wantFile) {
		return "", badChart("file name %q does not match the chart file the provenance file signs (%s)", provKey(wantFile), file)
	}
	sums, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(prov), nil)
	if err != nil {
		return "", uploadErr(err)
	}
	unlock, err := h.Store.Lock(ctx, "helm\x00"+rp.Name)
	if err != nil {
		return "", err
	}
	defer unlock()
	return file, h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		switch chart, err := tx.GetAsset(ctx, rp.Name, file); {
		case errors.Is(err, meta.ErrNotFound):
			return badChart("%s is not in repository %q: upload the chart before its provenance file", file, rp.Name)
		case err != nil:
			return err
		case !strings.EqualFold(chart.BlobSHA256, digest):
			return badChart("the provenance file signs another build of %s (sha256 %s, the repository has %s)", file, digest, chart.BlobSHA256)
		}
		switch _, err := tx.GetAsset(ctx, rp.Name, provKey(file)); {
		case err == nil && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, provKey(file))
		case err != nil && !errors.Is(err, meta.ErrNotFound):
			return err
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, provKey(file), ctProv, sums))
	})
}

// remove is `DELETE <name>-<version>.tgz`, as Nexus takes it: the chart
// version goes as DeleteComponent removes it (its row, file and provenance
// file, index.yaml rebuilt). DELETE of a provenance file removes that file
// alone. 200, or 404 when there is nothing to delete.
func (h *Handler) remove(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	ctx := r.Context()
	if rt.Kind == KindProv {
		unlock, err := h.Store.Lock(ctx, "helm\x00"+rp.Name)
		if err != nil {
			return err
		}
		defer unlock()
		if err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
			_, err := tx.DeleteAsset(ctx, rp.Name, provKey(rt.File))
			return err
		}); err != nil {
			return err // meta.ErrNotFound is a 404
		}
		h.Store.Emit(r, "delete", rp.Name, provKey(rt.File), "")
		return nil
	}
	name, version, ok := component.SplitHelm(rt.File)
	if !ok {
		return repo.ErrNotFound
	}
	c := component.Component{Repo: rp.Name, Format: "helm", Name: name, Version: version}
	res, err := h.DeleteComponent(ctx, rp, c, auth.FromContext(ctx).Owner())
	if err != nil {
		return err
	}
	if res.Assets == 0 {
		return repo.Errorf(http.StatusNotFound, "chart file %s not found in repository %q", rt.File, rp.Name)
	}
	return nil
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

// DeleteComponent removes one version of a hosted chart: its row, file and
// provenance file, and rebuilds index.yaml, in one incarnation-guarded
// transaction under the repository's lock, for the web UI's delete, cleanup
// policies and the DELETE of a chart file. A version
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
		var have []meta.Asset
		for _, f := range []string{file, provKey(file)} {
			switch a, err := tx.GetAsset(ctx, rp.Name, f); {
			case err == nil:
				have = append(have, a)
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
		}
		if err := cleanup.CheckUnchanged(c, have); err != nil {
			return err
		}
		// The row of the file, however its name reads: c may come from the file
		// name (component.SplitHelm), and demo 1.0.0-2.3.4 reads as demo-1.0.0 2.3.4.
		stem := strings.TrimSuffix(file, ".tgz")
		for i := strings.LastIndexByte(stem, '-'); i > 0; i = strings.LastIndexByte(stem[:i], '-') {
			switch err := tx.DeletePackage(ctx, rp.Name, stem[:i], stem[i+1:]); {
			case err == nil:
				rowGone = true
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
		}
		for _, a := range have {
			if _, err := tx.DeleteAsset(ctx, rp.Name, a.Path); err != nil {
				return err
			}
			res.Add(a.Size)
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
