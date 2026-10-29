// Package yum implements Yum/DNF (RPM) repositories (Nexus "yum"): RPM
// files at any path under /repository/<repo>/ and the repodata/ they are
// listed in (repomd.xml, primary, filelists, other). A hosted repository
// takes RPMs by PUT and keeps repodata/ itself, regenerated in the
// transaction of every upload and delete; at depth 0 one repodata/ at the
// root lists every RPM, at depth 1 each top-level directory has its own.
// A proxy caches a remote repository's files at their paths; a group
// answers from the first member that has the path (no merging, as Nexus).
package yum

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// Handler serves yum repositories. Reads need Read, uploads Add (the
// server's default rule); overwriting a package under writePolicy allow
// needs Edit, checked here.
type Handler struct {
	Store    *repo.Store
	Resolver *resolve.Resolver
	Auth     *auth.Authenticator
	Now      func() time.Time
}

func New(store *repo.Store, resolver *resolve.Resolver, authn *auth.Authenticator) *Handler {
	return &Handler{Store: store, Resolver: resolver, Auth: authn, Now: time.Now}
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	if err := h.serve(w, r, rp, p); err != nil {
		repo.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	if err := checkPath(p); err != nil {
		return err
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if p == "" || strings.HasSuffix(p, "/") {
			return repo.ErrNotFound
		}
		kind := proxy.Content
		if isRepodata(p) {
			kind = proxy.Metadata
		}
		res, err := h.Resolver.LookupTarget(r.Context(), rp, proxy.Target{Key: p, Remote: p, NotHTML: true}, kind)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	case http.MethodPut:
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, "GET, HEAD")
		}
		return h.upload(w, r, target, p)
	}
	allow := "GET, HEAD"
	if repo.AcceptsWrites(rp) {
		allow += ", PUT"
	}
	return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
}

// checkPath refuses paths that escape or alias the tree (as raw does).
func checkPath(p string) error {
	if strings.HasPrefix(p, "/") || strings.ContainsAny(p, `\?#;`) || strings.Contains(p, "//") {
		return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." {
			return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
		}
	}
	return nil
}

// isRepodata reports whether p is under a repodata/ directory.
func isRepodata(p string) bool {
	return strings.HasPrefix(p, "repodata/") || strings.Contains(p, "/repodata/")
}

// upload takes an RPM into hosted rp at p: `PUT <dir>/<name>-<version>-<release>.<arch>.rpm`
// with the file as the body (curl --upload-file). The file name must be
// the one the package's header gives (its NVRA), so the path names the
// component. The blob is written first and the header read back from it,
// then the row, the file and dir's rebuilt repodata are recorded in one
// transaction under the repository's lock. An existing package is a 409
// unless writePolicy is allow, when replacing it needs Edit.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	ctx := r.Context()
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	if !strings.HasSuffix(p, ".rpm") || isRepodata(p) || strings.HasSuffix(p, "/.rpm") {
		return repo.Errorf(http.StatusBadRequest, "upload a .rpm file outside repodata/: %s", p)
	}
	dir, ok := repoDir(rp, p)
	if !ok {
		return repo.Errorf(http.StatusBadRequest, "repository %q has depth 1: put the file in a directory (<dir>/%s)", rp.Name, p)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, nil)
	if err != nil {
		return uploadErr(err)
	}
	if sums.Size == 0 {
		return badRPM("empty upload")
	}
	pkg, err := h.readStored(rp, sums)
	if err != nil {
		return err
	}
	if path.Base(p) != pkg.NVRA() {
		return badRPM("file name %q does not match the package: %s", path.Base(p), pkg.NVRA())
	}
	now := h.Now().UTC()
	pkg.Checksum, pkg.Location, pkg.Uploaded = sums.SHA256, p, now.Unix()
	attrs, err := json.Marshal(pkg)
	if err != nil {
		return err
	}
	name, version := strings.TrimSuffix(p, pkg.NVRA())+pkg.Name, pkg.Version+"-"+pkg.Release+"."+pkg.Arch
	unlock, err := h.Store.Lock(ctx, "yum\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		row := meta.Package{Repo: rp.Name, Format: "yum", Name: name, Version: version, Attrs: string(attrs), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, row); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "package %s already exists in repository %q", p, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, p)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, name, version, row.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, p, ctRPM, sums)); err != nil {
			return err
		}
		return h.rebuild(ctx, tx, rp, dir)
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, name+"@"+version, "")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// readStored parses the header of the stored blob, so the upload is never
// buffered in memory.
func (h *Handler) readStored(rp *repo.Repository, sums blob.Sums) (*Package, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, sums.Size)
}

// DeleteComponent removes one package of a hosted repository: its row and
// file, and rebuilds its directory's repodata, in one transaction under
// the repository's lock, for the web UI's delete and cleanup policies. A
// package that is gone already is not an error, and one that changed
// since an evaluation is skipped (cleanup.CheckUnchanged). Proxies use the
// generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "yum" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("yum component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("yum component %q is incomplete", c.Label())
	}
	file := c.Name + "-" + c.Version + ".rpm"
	dir, ok := repoDir(rp, file)
	if !ok {
		return component.Result{}, fmt.Errorf("yum component %q is not in a directory of repository %q (depth 1)", c.Label(), rp.Name)
	}
	unlock, err := h.Store.Lock(ctx, "yum\x00"+rp.Name)
	if err != nil {
		return component.Result{}, err
	}
	defer unlock()
	rowGone := false
	var res component.Result
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
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
		return h.rebuild(ctx, tx, rp, dir)
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowGone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
