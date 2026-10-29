// Package conda implements conda channels (what conda, mamba and
// micromamba install from): /repository/<repo>/ is the channel, its
// subdirs (noarch, linux-64, osx-arm64, ...) hold the packages and the
// repodata.json that lists them, and channeldata.json at the root sums the
// channel up. A hosted channel takes packages by PUT and keeps the
// documents itself, regenerated in the transaction of every upload and
// delete; a proxy caches a remote channel's files at their paths; a group
// answers a file from the first member that has it and merges the
// members' repodata.json and channeldata.json.
package conda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// Handler serves conda channels. Reads need Read, uploads Add (the
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
		return h.get(w, r, rp, p)
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

func isPackage(p string) bool {
	return strings.HasSuffix(p, ".tar.bz2") || strings.HasSuffix(p, ".conda")
}

// doc classifies the documents a hosted channel generates and a group
// merges: "repodata" for <subdir>/repodata.json and current_repodata.json,
// "channeldata" for channeldata.json, "variant" for the compressed or
// patch forms of repodata (.bz2, .zst, .jlap) a hosted channel does not
// make, and "" for anything else.
func doc(p string) (kind, subdir string) {
	if p == "channeldata.json" {
		return "channeldata", ""
	}
	subdir, file, ok := strings.Cut(p, "/")
	if !ok || strings.Contains(file, "/") || !condaSubdir(subdir) {
		return "", ""
	}
	switch file {
	case "repodata.json", "current_repodata.json":
		return "repodata", subdir
	case "repodata.json.bz2", "repodata.json.zst", "current_repodata.json.bz2", "current_repodata.json.zst", "repodata.jlap":
		return "variant", subdir
	}
	return "", ""
}

func condaSubdir(s string) bool {
	_, _, _, ok := component.SplitConda(s + "/x-1-0.conda")
	return ok
}

// get serves a file of rp. Packages are content, everything else (the
// repodata and its variants, channeldata) metadata, re-checked by a proxy
// every metadataMaxAge. A group merges the members' repodata.json and
// channeldata.json (cached until a member's document changes) and has no
// compressed repodata, so clients fall back to the merged repodata.json.
// A hosted channel or group without a document yet answers an empty one
// (conda takes a missing subdir of a channel as an error, not as empty).
func (h *Handler) get(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	ctx := r.Context()
	kind, subdir := doc(p)
	t := proxy.Target{Key: p, Remote: p, NotHTML: true}
	var res proxy.Result
	var err error
	switch {
	case rp.Type == "group" && kind == "repodata":
		res.Asset, err = h.Resolver.MergedTarget(ctx, rp, t, ctJSON, mergeRepodata(subdir))
	case rp.Type == "group" && kind == "channeldata":
		res.Asset, err = h.Resolver.MergedTarget(ctx, rp, t, ctJSON, mergeChanneldata)
	case rp.Type == "group" && kind == "variant":
		err = repo.ErrNotFound
	case isPackage(p):
		res, err = h.Resolver.LookupTarget(ctx, rp, t, proxy.Content)
	default:
		res, err = h.Resolver.LookupTarget(ctx, rp, t, proxy.Metadata)
	}
	if repo.StatusOf(err) == http.StatusNotFound && rp.Type != "proxy" && (kind == "repodata" || kind == "channeldata") {
		var empty any = newChanneldata()
		if kind == "repodata" {
			empty = newRepodata(subdir)
		}
		b, _ := json.Marshal(empty)
		repo.ServeText(w, r, string(b), ctJSON, time.Time{})
		return nil
	}
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, res)
	repo.ServeAsset(w, r, h.Store, res.Asset)
	return nil
}

// upload takes a package into hosted rp: `PUT <subdir>/<name>-<version>-<build>.tar.bz2`
// (or .conda) with the file as the body (curl --upload-file). The blob is
// written first and its index read back from it, then the row, the file
// and the subdir's rebuilt documents are recorded in one transaction
// under the repository's lock. An existing file is a 409 unless
// writePolicy is allow, when replacing it needs Edit.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	ctx := r.Context()
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	subdir, name, _, ok := component.SplitConda(p)
	if !ok {
		return badPackage("upload <subdir>/<name>-<version>-<build>.tar.bz2 or .conda, not %s", p)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, nil)
	if err != nil {
		return uploadErr(err)
	}
	if sums.Size == 0 {
		return badPackage("empty upload")
	}
	rw, err := h.readStored(rp, sums, subdir, p[len(subdir)+1:])
	if err != nil {
		return err
	}
	attrs, err := json.Marshal(rw)
	if err != nil {
		return err
	}
	now := h.Now().UTC()
	ct := ctBzip2
	if strings.HasSuffix(p, ".conda") {
		ct = ctZip
	}
	unlock, err := h.Store.Lock(ctx, "conda\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		pkg := meta.Package{Repo: rp.Name, Format: "conda", Name: name, Version: p, Attrs: string(attrs), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, pkg); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "package %s already exists in repository %q", p, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, p)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, name, p, pkg.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, p, ct, sums)); err != nil {
			return err
		}
		return h.rebuild(ctx, tx, rp, subdir)
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, component.Component{Group: subdir, Name: name, Version: rw.version()}.Label(), p)
	w.WriteHeader(http.StatusCreated)
	return nil
}

// readStored parses the stored blob, so the upload is never buffered in memory.
func (h *Handler) readStored(rp *repo.Repository, sums blob.Sums, subdir, file string) (row, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return row{}, err
	}
	defer f.Close()
	return parse(f, sums, subdir, file)
}

// DeleteComponent removes one package of a hosted channel (its group is
// the subdir, its version <version>-<build>): the .tar.bz2 and the .conda
// of that build, rows and files, and rebuilds the subdir's documents, in
// one transaction under the repository's lock, for the web UI's delete
// and cleanup policies. A package that is gone already is not an error,
// and one that changed since an evaluation is skipped
// (cleanup.CheckUnchanged). Proxies use the generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "conda" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("conda component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Group == "" || c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("conda component %q is incomplete: subdir, name and version-build are needed", c.Label())
	}
	stem := c.Group + "/" + c.Name + "-" + c.Version
	if _, _, _, ok := component.SplitConda(stem + ".conda"); !ok {
		return component.Result{}, fmt.Errorf("conda component %q is not a package", c.Label())
	}
	files := []string{stem + ".tar.bz2", stem + ".conda"}
	unlock, err := h.Store.Lock(ctx, "conda\x00"+rp.Name)
	if err != nil {
		return component.Result{}, err
	}
	defer unlock()
	rowsGone := 0
	var res component.Result
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowsGone, res = 0, component.Result{}
		var current []meta.Asset
		for _, file := range files {
			switch a, err := tx.GetAsset(ctx, rp.Name, file); {
			case err == nil:
				current = append(current, a)
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
		}
		if err := cleanup.CheckUnchanged(c, current); err != nil {
			return err
		}
		for _, file := range files {
			switch err := tx.DeletePackage(ctx, rp.Name, c.Name, file); {
			case err == nil:
				rowsGone++
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
			switch size, err := tx.DeleteAsset(ctx, rp.Name, file); {
			case err == nil:
				res.Add(size)
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
		}
		if rowsGone == 0 && res.Assets == 0 {
			return nil
		}
		return h.rebuild(ctx, tx, rp, c.Group)
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowsGone > 0 || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
