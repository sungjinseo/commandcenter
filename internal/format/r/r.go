// Package r implements R (CRAN-style) repositories, what install.packages(),
// remotes, pak and renv read: /repository/<repo>/ is the repository, its
// contrib directories (src/contrib for sources, bin/windows/contrib/<R.x>
// and bin/macosx[/<build>]/contrib/<R.x> for binaries) hold the packages
// and the PACKAGES, PACKAGES.gz and PACKAGES.rds that list them. A hosted
// repository takes packages by PUT and keeps the three index files of
// each contrib directory itself, regenerated in the transaction of every
// upload and delete; a proxy caches a mirror's files at their paths; a
// group answers a file from the first member that has it and merges the
// members' PACKAGES of a directory (PACKAGES.gz and PACKAGES.rds derived
// from the merge).
package r

import (
	"bytes"
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

// Handler serves R repositories. Reads need Read, uploads Add (the
// server's default rule); overwriting a package under writePolicy allow
// needs Edit, checked here.
type Handler struct {
	Store    *repo.Store
	Resolver *resolve.Resolver
	Auth     *auth.Authenticator
	Now      func() time.Time
	locks    repo.KeyedMutex // one per hosted repository: every write rebuilds a directory's index
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

// doc classifies p as an index file of a contrib directory: the file
// (PACKAGES, PACKAGES.gz or PACKAGES.rds) and the directory, or "".
func doc(p string) (file, dir string) {
	dir, file = path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	switch file {
	case "PACKAGES", "PACKAGES.gz", "PACKAGES.rds":
		if component.RContribDir(dir) {
			return file, dir
		}
	}
	return "", ""
}

// isMetadata reports whether a proxy re-checks p every metadataMaxAge:
// any PACKAGES file (CRAN also has PACKAGES.json) and the Meta/ databases
// (archive.rds, current.rds) remotes and pak read.
func isMetadata(p string) bool {
	return strings.HasPrefix(path.Base(p), "PACKAGES") || strings.Contains(p, "/Meta/")
}

// get serves a file of rp. Index files are metadata, packages content. A
// group merges the members' PACKAGES of a contrib directory (cached
// until a member's changes) and derives PACKAGES.gz and PACKAGES.rds from
// the merge. A hosted repository or group without an index yet answers
// an empty one, so install.packages() of another repository's package
// does not warn about this one.
func (h *Handler) get(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	ctx := r.Context()
	file, dir := doc(p)
	if rp.Type == "group" && file != "" {
		t := proxy.Target{Key: dir + "/PACKAGES", Remote: dir + "/PACKAGES", NotHTML: true}
		a, err := h.Resolver.MergedTarget(ctx, rp, t, ctText, mergePackages)
		if repo.StatusOf(err) == http.StatusNotFound {
			return serveEmpty(w, r, file, dir)
		}
		if err != nil {
			return err
		}
		if file != "PACKAGES" {
			if a, err = h.derived(ctx, rp, dir, file, a); err != nil {
				return err
			}
		}
		repo.ServeAsset(w, r, h.Store, a)
		return nil
	}
	kind := proxy.Content
	if isMetadata(p) {
		kind = proxy.Metadata
	}
	res, err := h.Resolver.LookupTarget(ctx, rp, proxy.Target{Key: p, Remote: p, NotHTML: true}, kind)
	if repo.StatusOf(err) == http.StatusNotFound && file != "" && rp.Type == "hosted" {
		return serveEmpty(w, r, file, dir)
	}
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, res)
	repo.ServeAsset(w, r, h.Store, res.Asset)
	return nil
}

func serveEmpty(w http.ResponseWriter, r *http.Request, file, dir string) error {
	b, ct := index(file, dir, nil)
	repo.ServeText(w, r, string(b), ct, time.Time{})
	return nil
}

// index renders one of a contrib directory's index files from its stanzas.
func index(file, dir string, ss []stanza) (body []byte, contentType string) {
	switch file {
	case "PACKAGES.gz":
		return gzipBytes([]byte(render(ss))), ctGzip
	case "PACKAGES.rds":
		return writeRDS(columns(dir, ss), ss), ctRDS
	}
	return []byte(render(ss)), ctText
}

// derived is the group's <dir>/<file> made from its merged PACKAGES,
// stored in the group's namespace and reused until the merge changes
// (the fingerprint is the merged document's blob, as resolve.Merged
// keeps a member fingerprint).
func (h *Handler) derived(ctx context.Context, rp *repo.Repository, dir, file string, merged meta.Asset) (meta.Asset, error) {
	fp := "r:" + merged.BlobSHA256
	key := dir + "/" + file
	if a, err := h.Store.Get(ctx, rp.Name, key); err == nil && a.RemoteETag == fp {
		return a, nil
	}
	f, err := h.Store.Open(merged)
	if err != nil {
		return meta.Asset{}, err
	}
	defer f.Close()
	ss, err := parsePackages(f)
	if err != nil {
		return meta.Asset{}, fmt.Errorf("merged %s of %q: %w", key, rp.Name, err)
	}
	b, ct := index(file, dir, ss)
	return h.Store.Put(ctx, rp.Name, key, ct, bytes.NewReader(b), func(a *meta.Asset) { a.RemoteETag = fp })
}

// upload takes a package into hosted rp: `PUT <contrib dir>/<pkg>_<version>.tar.gz`
// (.zip under bin/windows/contrib/<R.x>, .tgz under bin/macosx/.../contrib/<R.x>)
// with the file as the body (curl --upload-file). The blob is written
// first and its DESCRIPTION read back from it (Package and Version must
// be the file name's), then the row, the file and the directory's
// rebuilt index files are recorded in one transaction under the
// repository's lock. An existing package is a 409 unless writePolicy is
// allow, when replacing it needs Edit.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	ctx := r.Context()
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	dir, name, version, ok := component.SplitR(p)
	if !ok || !component.RContribDir(dir) {
		return badPackage("upload <contrib dir>/<pkg>_<version>.tar.gz (.zip under bin/windows, .tgz under bin/macosx), not %s", p)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, nil)
	if err != nil {
		return uploadErr(err)
	}
	if sums.Size == 0 {
		return badPackage("empty upload")
	}
	rw, err := h.readStored(rp, sums, p)
	if err != nil {
		return err
	}
	attrs, err := json.Marshal(rw)
	if err != nil {
		return err
	}
	now := h.Now().UTC()
	ct := ctGzip
	if component.RExt(dir) == ".zip" {
		ct = ctZip
	}
	rowName := dir + "/" + name
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		pkg := meta.Package{Repo: rp.Name, Format: "r", Name: rowName, Version: version, Attrs: string(attrs), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, pkg); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "package %s already exists in repository %q", p, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, p)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, rowName, version, pkg.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, p, ct, sums)); err != nil {
			return err
		}
		return h.rebuild(ctx, tx, rp, dir)
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, component.Component{Group: dir, Name: name, Version: version}.Label(), p)
	w.WriteHeader(http.StatusCreated)
	return nil
}

// readStored parses the stored blob, so the upload is never buffered in memory.
func (h *Handler) readStored(rp *repo.Repository, sums blob.Sums, p string) (row, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return row{}, err
	}
	defer f.Close()
	return parse(f, sums, p)
}

// rowsOf lists the rows of dir (names <dir>/<pkg>), by package then version.
func rowsOf(ctx context.Context, tx *meta.Tx, rp *repo.Repository, dir string) ([]row, error) {
	names, err := tx.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return nil, err
	}
	var out []row
	for _, name := range names {
		if rest, ok := strings.CutPrefix(name, dir+"/"); !ok || strings.Contains(rest, "/") {
			continue
		}
		pkgs, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			var r row
			if err := json.Unmarshal([]byte(p.Attrs), &r); err != nil {
				return nil, fmt.Errorf("package %s %s of %q: %w", p.Name, p.Version, rp.Name, err)
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// rebuild writes dir's PACKAGES, PACKAGES.gz and PACKAGES.rds from rp's
// rows, in the transaction that changed them.
func (h *Handler) rebuild(ctx context.Context, tx *meta.Tx, rp *repo.Repository, dir string) error {
	rows, err := rowsOf(ctx, tx, rp, dir)
	if err != nil {
		return err
	}
	ss := make([]stanza, 0, len(rows))
	for _, r := range rows {
		ss = append(ss, stanzaOf(r))
	}
	sortStanzas(ss)
	for _, file := range []string{"PACKAGES", "PACKAGES.gz", "PACKAGES.rds"} {
		b, ct := index(file, dir, ss)
		sums, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(b), nil)
		if err != nil {
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, dir+"/"+file, ct, sums)); err != nil {
			return err
		}
	}
	return nil
}

// DeleteComponent removes one package of a hosted repository (its group
// is the contrib directory): the row and the file, and rebuilds the
// directory's index files, in one transaction under the repository's
// lock, for the web UI's delete and cleanup policies. A package that is
// gone already is not an error, and one that changed since an evaluation
// is skipped (cleanup.CheckUnchanged). Proxies use the generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "r" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("r component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Group == "" || c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("r component %q is incomplete: contrib directory, package and version are needed", c.Label())
	}
	file := c.Group + "/" + c.Name + "_" + c.Version + component.RExt(c.Group)
	if _, _, _, ok := component.SplitR(file); !ok || !component.RContribDir(c.Group) {
		return component.Result{}, fmt.Errorf("r component %q is not a package of a contrib directory", c.Label())
	}
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	rowGone := false
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		switch a, err := tx.GetAsset(ctx, rp.Name, file); {
		case err == nil:
			if err := cleanup.CheckUnchanged(c, []meta.Asset{a}); err != nil {
				return err
			}
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		switch err := tx.DeletePackage(ctx, rp.Name, c.Group+"/"+c.Name, c.Version); {
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
		return h.rebuild(ctx, tx, rp, c.Group)
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowGone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
