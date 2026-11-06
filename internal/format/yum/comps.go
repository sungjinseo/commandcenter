package yum

import (
	"compress/gzip"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// compsFiles are the package group files (dnf group) a hosted repository
// takes (component.YumComps), by the names they have in its repodata/, and the
// repomd.xml types that list them.
var compsFiles = []struct{ name, typ string }{{"comps.xml", "group"}, {"comps.xml.gz", "group_gz"}}

// compsDir is the directory whose repodata/ the comps file at p belongs to:
// ok is false when p is no comps file or not at rp's depth.
func compsDir(rp *repo.Repository, p string) (dir string, ok bool) {
	if !component.YumComps(p) {
		return "", false
	}
	dir, ok = repoDir(rp, p)
	return dir, ok && !isRepodata(dir) && p == dir+"repodata/"+path.Base(p)
}

// uploadComps takes `PUT <dir>repodata/comps.xml` (or comps.xml.gz) into
// hosted rp, dir being a directory of rp's depth, and lists it in dir's
// repomd.xml. Its root element is checked, the rest is the client's to read.
// A second upload is refused or needs Edit as for a package.
func (h *Handler) uploadComps(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	ctx := r.Context()
	dir, ok := compsDir(rp, p)
	if !ok {
		return repo.Errorf(http.StatusBadRequest, "repository %q has depth %d: put %s in the repodata/ that many directories down: %s", rp.Name, rp.Depth, path.Base(p), p)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, nil)
	if err != nil {
		return uploadErr(err)
	}
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return err
	}
	defer f.Close()
	ct, gz := ctXML, strings.HasSuffix(p, ".gz")
	if gz {
		ct = ctGzip
	}
	if err := checkComps(f, gz); err != nil {
		return repo.Errorf(http.StatusBadRequest, "%s is no package group file: %v", p, err)
	}
	unlock, err := h.Store.Lock(ctx, "yum\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		switch _, err := tx.GetAsset(ctx, rp.Name, p); {
		case err == nil && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "%s already exists in repository %q", p, rp.Name)
		case err == nil && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, p)
		case err != nil && !errors.Is(err, meta.ErrNotFound):
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
	h.Store.Emit(r, "upload", rp.Name, p, "")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// checkComps reads r up to its first element, which must be <comps>.
func checkComps(r io.Reader, gz bool) error {
	if gz {
		var err error
		if r, err = gzip.NewReader(r); err != nil {
			return err
		}
	}
	d := xml.NewDecoder(io.LimitReader(r, 1<<20))
	for {
		t, err := d.RawToken()
		if err != nil {
			return err
		}
		if se, ok := t.(xml.StartElement); ok {
			if se.Name.Local != "comps" {
				return errors.New("its root element is not <comps>")
			}
			return nil
		}
	}
}

// deleteComps is `DELETE <dir>repodata/comps.xml`: the file goes, and dir's
// repomd.xml no longer lists it. Packages are deleted as components instead.
func (h *Handler) deleteComps(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	ctx := r.Context()
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	dir, ok := compsDir(rp, p)
	if !ok {
		return repo.ErrNotFound
	}
	unlock, err := h.Store.Lock(ctx, "yum\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		if _, err := tx.DeleteAsset(ctx, rp.Name, p); err != nil {
			return err
		}
		return h.rebuild(ctx, tx, rp, dir)
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "delete", rp.Name, p, "")
	w.WriteHeader(http.StatusNoContent)
	return nil
}
