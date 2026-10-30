package apt

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
	"commandcenter/internal/pgp"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// Handler serves apt repositories. Reads need Read, uploads Add (the
// server's default rule); replacing a package under writePolicy allow
// needs Edit, checked here as Maven does.
type Handler struct {
	Store    *repo.Store
	Resolver *resolve.Resolver
	Auth     *auth.Authenticator
	Now      func() time.Time
}

func New(store *repo.Store, resolver *resolve.Resolver, authn *auth.Authenticator) *Handler {
	return &Handler{Store: store, Resolver: resolver, Auth: authn, Now: time.Now}
}

const publicKeyPath = "public.key"

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	if err := h.serve(w, r, rp, p); err != nil {
		repo.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	if err := checkPath(p); err != nil {
		return err
	}
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case p == "": // the upload form: POST multipart, the file in "file"
		if r.Method != http.MethodPost || !repo.AcceptsWrites(rp) {
			allow := ""
			if repo.AcceptsWrites(rp) {
				allow = "POST"
			}
			return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
		}
	case p == publicKeyPath:
		if !get {
			return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
		}
		return h.publicKey(w, r, rp)
	case get:
		return h.get(w, r, rp, p)
	case r.Method != http.MethodPut || !strings.HasPrefix(p, "pool/") || !strings.HasSuffix(p, ".deb"):
		allow := "GET, HEAD"
		if repo.AcceptsWrites(rp) && strings.HasPrefix(p, "pool/") && strings.HasSuffix(p, ".deb") {
			allow += ", PUT"
		}
		return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
	}
	target, err := repo.WriteTarget(rp)
	if err != nil {
		return repo.WithAllow(err, "GET, HEAD")
	}
	return h.upload(w, r, target)
}

// checkPath refuses paths that escape or alias the tree (as raw does).
func checkPath(p string) error {
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, `\`) || strings.Contains(p, "//") {
		return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." {
			return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
		}
	}
	return nil
}

// get serves a stored or cached file: dists/ is metadata (re-checked
// after metadataMaxAge on a proxy), everything else content. A group asks
// its members in order. A hosted repository nothing was uploaded to yet
// answers its (empty) Release files, so `apt update` works before the
// first upload.
func (h *Handler) get(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	kind := proxy.Content
	if strings.HasPrefix(p, "dists/") {
		kind = proxy.Metadata
	}
	res, err := h.Resolver.LookupTarget(r.Context(), rp, proxy.Target{Key: p, Remote: p, NotHTML: true}, kind)
	if errors.Is(err, repo.ErrNotFound) && rp.Type == "hosted" && strings.HasPrefix(p, "dists/"+rp.Distribution+"/") {
		names, err := h.Store.DB.ListPackageNames(r.Context(), rp.Name)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return repo.ErrNotFound
		}
		files, err := build(rp, nil, h.Now())
		if err != nil {
			return err
		}
		b, ok := files[p]
		if !ok {
			return repo.ErrNotFound
		}
		repo.ServeText(w, r, string(b), contentType(p), time.Time{})
		return nil
	}
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, res)
	a := res.Asset
	if a.ContentType == "" || a.ContentType == "application/octet-stream" {
		a.ContentType = contentType(p)
	}
	repo.ServeAsset(w, r, h.Store, a)
	return nil
}

// publicKey serves the public key of the repository's signing key, for
// `signed-by=` and `apt-key add`; a group answers with the first online
// member's that has one.
func (h *Handler) publicKey(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	key := signingKey(rp)
	if key == "" {
		return repo.ErrNotFound
	}
	k, err := pgp.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("repository %q: signingKey: %w", rp.Name, err)
	}
	repo.ServeText(w, r, k.PublicKey(), "application/pgp-keys", time.Time{})
	return nil
}

func signingKey(rp *repo.Repository) string {
	if rp.Type != "group" {
		return rp.SigningKey
	}
	for _, m := range rp.OnlineMembers() {
		if k := signingKey(m); k != "" {
			return k
		}
	}
	return ""
}

// upload takes a .deb into hosted rp: `PUT pool/<any>/<file>.deb` with the
// file as the body, or `POST /` with the package as the body, as Nexus
// takes it (its documented curl sends --data-binary with a bare
// multipart/form-data type, so a type without a boundary is no form). A
// POST whose body is a multipart form (a boundary given) carries the
// package in its "file" field or its first file part. The file is stored
// at its pool path, whatever path was given, and the answer is 201 with
// the package, version, architecture and filename as JSON. The server's
// maxUploadMiB bounds the body.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	body := io.Reader(r.Body)
	if mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); r.Method == http.MethodPost && mt == "multipart/form-data" && params["boundary"] != "" {
		mr, err := r.MultipartReader()
		if err != nil {
			return badDeb("%v", err)
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return badDeb(`multipart body has no "file" field or file part`)
			}
			if err != nil {
				return uploadErr(err)
			}
			if part.FormName() == "file" || part.FileName() != "" {
				body = part
				break
			}
		}
	}
	row, err := h.store(r.Context(), rp, body)
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, label(row.Architecture, row.Package, row.Version), row.Filename)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"package": row.Package, "version": row.Version, "architecture": row.Architecture, "filename": row.Filename})
	return nil
}

func label(arch, pkg, version string) string {
	return component.Component{Group: arch, Name: pkg, Version: version}.Label()
}

// store streams a package into rp: the blob first, then its control file
// is read back from it, and the row, the pool file and the regenerated
// dists/ are recorded in one transaction under the repository's lock. An
// existing package_version_architecture is a 409 unless writePolicy is
// allow, when replacing it needs Edit.
func (h *Handler) store(ctx context.Context, rp *repo.Repository, body io.Reader) (row, error) {
	sums, err := h.Store.WriteBlob(rp.Name, body, nil)
	if err != nil {
		return row{}, uploadErr(err)
	}
	if sums.Size == 0 {
		return row{}, badDeb("empty upload")
	}
	f, err := h.readStored(rp, sums)
	if err != nil {
		return row{}, err
	}
	rw := newRow(f, rp.Component, sums)
	attrs, err := json.Marshal(rw)
	if err != nil {
		return row{}, err
	}
	now := h.Now().UTC()
	// Every upload and delete regenerates dists/. ponytail: one lock per repository.
	unlock, err := h.Store.Lock(ctx, "apt\x00"+rp.Name)
	if err != nil {
		return row{}, err
	}
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		p := meta.Package{Repo: rp.Name, Format: "apt", Name: rw.Package, Version: rw.key(), Attrs: string(attrs), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, p); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "package %s version %s (%s) already exists in repository %q", rw.Package, rw.Version, rw.Architecture, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, rw.Filename)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, rw.Package, rw.key(), p.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, rw.Filename, ctDeb, sums)); err != nil {
			return err
		}
		return h.regenerate(ctx, tx, rp)
	})
	return rw, err
}

// readStored reads the control file from the stored blob, so nothing is buffered.
func (h *Handler) readStored(rp *repo.Repository, sums blob.Sums) (fields, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readDeb(f)
}

// regenerate writes rp's dists/ files from its rows, in the transaction
// that changed them, and drops the dists/ files that no longer apply (an
// architecture without packages, a renamed distribution).
func (h *Handler) regenerate(ctx context.Context, tx *meta.Tx, rp *repo.Repository) error {
	rows, err := loadRows(ctx, tx, rp.Name)
	if err != nil {
		return err
	}
	files, err := build(rp, rows, h.Now())
	if err != nil {
		return err
	}
	old, err := tx.ListAssets(ctx, rp.Name, "dists/")
	if err != nil {
		return err
	}
	for _, a := range old {
		if _, keep := files[a.Path]; keep {
			continue
		}
		if _, err := tx.DeleteAsset(ctx, rp.Name, a.Path); err != nil && !errors.Is(err, meta.ErrNotFound) {
			return err
		}
	}
	for p, b := range files {
		sums, err := h.Store.WriteBlob(rp.Name, strings.NewReader(string(b)), nil)
		if err != nil {
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, p, contentType(p), sums)); err != nil {
			return err
		}
	}
	return nil
}

// DeleteComponent removes one package (its group is the architecture):
// the row and the pool file, and regenerates dists/, in one
// incarnation-guarded transaction under the repository's lock, for the
// web UI's delete and cleanup policies. A package that is gone already is
// not an error, and one that changed since an evaluation is skipped
// (cleanup.CheckUnchanged). apt proxies use the generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "apt" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("apt component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Group == "" || c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("apt component %q is incomplete: architecture, package and version are needed", c.Label())
	}
	unlock, err := h.Store.Lock(ctx, "apt\x00"+rp.Name)
	if err != nil {
		return component.Result{}, err
	}
	defer unlock()
	rowGone := false
	var res component.Result
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		key := c.Version + "_" + c.Group
		file := poolPath(rp.Component, c.Name, c.Version, c.Group)
		pkgs, err := tx.ListPackages(ctx, rp.Name, c.Name)
		if err != nil {
			return err
		}
		for _, p := range pkgs {
			if p.Version == key {
				if r, err := parseRow(p.Attrs); err == nil && r.Filename != "" {
					file = r.Filename // where the upload put it, whatever the component setting is now
				}
			}
		}
		switch a, err := tx.GetAsset(ctx, rp.Name, file); {
		case err == nil:
			if err := cleanup.CheckUnchanged(c, []meta.Asset{a}); err != nil {
				return err
			}
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		switch err := tx.DeletePackage(ctx, rp.Name, c.Name, key); {
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
		return h.regenerate(ctx, tx, rp)
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowGone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
