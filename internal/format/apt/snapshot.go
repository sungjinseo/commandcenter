package apt

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Snapshots (Nexus's apt snapshots, hosted repositories): a frozen copy
// of dists/ under snapshots/<id>/, so a client can pin a release with
// `deb <url>/snapshots/<id> <distribution> <component>`. MKCOL
// snapshots/<id> takes the copy (Add), PUT does too with a control-style
// body ("Architectures: amd64 arm64", "Components: main") keeping only
// those Packages files, DELETE drops it (Delete); activity events
// snapshot.create and snapshot.delete name it. GET serves the frozen
// files; a package path under it is the live pool's (packages are never
// rewritten under allow_once; under allow a replaced package fails the
// frozen checksums, as in Nexus). The copies share the live files' blobs.

const snapshotsDir = "snapshots/"

var snapshotIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// isSnapshotWrite reports whether method on p takes or drops a snapshot.
func isSnapshotWrite(method, p string) bool {
	_, ok := snapshotRoot(p)
	return ok && (method == "MKCOL" || method == http.MethodPut || method == http.MethodDelete)
}

// snapshotRoot returns the id of p when p is snapshots/<id>.
func snapshotRoot(p string) (string, bool) {
	id, ok := strings.CutPrefix(p, snapshotsDir)
	return id, ok && snapshotIDRE.MatchString(id)
}

// snapshotPackage is the live pool path a package path under a snapshot
// stands for, or p itself.
func snapshotPackage(p string) string {
	if rest, ok := strings.CutPrefix(p, snapshotsDir); ok && isPackageFile(p) {
		if _, file, ok := strings.Cut(rest, "/"); ok && strings.HasPrefix(file, "pool/") {
			return file
		}
	}
	return p
}

// snapshot answers MKCOL, PUT and DELETE on snapshots/<id> of hosted rp.
func (h *Handler) snapshot(w http.ResponseWriter, r *http.Request, rp *repo.Repository, id string) error {
	if rp.Type != "hosted" {
		return repo.MethodNotAllowed("GET, HEAD", "snapshots are taken in hosted repositories")
	}
	keep := func(string) bool { return true }
	if r.Method == http.MethodPut {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			return uploadErr(err)
		}
		f, err := parseSelector(b)
		if err != nil {
			return err
		}
		archs, comps := strings.Fields(f.get("Architectures")), strings.Fields(f.get("Components"))
		keep = func(p string) bool { return selected(p, rp.Distribution, archs, comps) }
	}
	dir := snapshotsDir + id + "/"
	unlock, err := h.Store.Lock(r.Context(), "apt\x00"+rp.Name)
	if err != nil {
		return err
	}
	defer unlock()
	found := false
	err = h.Store.Tx(r.Context(), rp, func(tx *meta.Tx) error {
		have, err := tx.ListAssets(r.Context(), rp.Name, dir)
		if err != nil {
			return err
		}
		found = len(have) > 0
		if r.Method == http.MethodDelete {
			for _, a := range have {
				if _, err := tx.DeleteAsset(r.Context(), rp.Name, a.Path); err != nil {
					return err
				}
			}
			return nil
		}
		if found {
			return repo.Errorf(http.StatusConflict, "snapshot %q exists in repository %q", id, rp.Name)
		}
		return h.copyDists(r.Context(), tx, rp, dir, keep)
	})
	switch {
	case err != nil:
		return err
	case r.Method == http.MethodDelete && !found:
		return repo.ErrNotFound
	case r.Method == http.MethodDelete:
		h.Store.Emit(r, "snapshot.delete", rp.Name, id, "")
		w.WriteHeader(http.StatusNoContent)
	default:
		h.Store.Emit(r, "snapshot.create", rp.Name, id, "")
		w.WriteHeader(http.StatusCreated)
	}
	return nil
}

// copyDists records the dists/ files keep accepts under dir, sharing
// their blobs; a repository nothing was uploaded to gets its (empty)
// files written first.
func (h *Handler) copyDists(ctx context.Context, tx *meta.Tx, rp *repo.Repository, dir string, keep func(string) bool) error {
	files, err := tx.ListAssets(ctx, rp.Name, "dists/")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		if err := h.regenerate(ctx, tx, rp); err != nil {
			return err
		}
		if files, err = tx.ListAssets(ctx, rp.Name, "dists/"); err != nil {
			return err
		}
	}
	for _, a := range files {
		if !keep(a.Path) {
			continue
		}
		c := h.Store.NewAsset(rp.Name, dir+a.Path, a.ContentType, blob.Sums{SHA256: a.BlobSHA256, Size: a.Size, SHA1: a.SHA1, MD5: a.MD5, SHA512: a.SHA512})
		if err := tx.PutAsset(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// selected reports whether a dists/ file belongs in a filtered snapshot:
// the Release files always, an index of a listed architecture ("all"
// counts as listed) and component, or anything else of a listed
// component. Empty lists select everything.
func selected(p, dist string, archs, comps []string) bool {
	rest, ok := strings.CutPrefix(p, "dists/"+dist+"/")
	comp, file, nested := strings.Cut(rest, "/")
	if !ok || !nested {
		return true // Release, InRelease, Release.gpg
	}
	if len(comps) > 0 && !slices.Contains(comps, comp) {
		return false
	}
	arch, isBinary := strings.CutPrefix(strings.SplitN(file, "/", 2)[0], "binary-")
	return !isBinary || len(archs) == 0 || arch == "all" || slices.Contains(archs, arch)
}

// parseSelector reads a PUT snapshot body: a control stanza, possibly
// empty (everything).
func parseSelector(b []byte) (fields, error) {
	if strings.TrimSpace(string(b)) == "" {
		return nil, nil
	}
	var out fields
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" || line[0] == '#' {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !fieldRE.MatchString(name) {
			return nil, repo.Errorf(http.StatusBadRequest, "snapshot selector line %q is not a field", line)
		}
		out = append(out, [2]string{name, strings.TrimSpace(value)})
	}
	return out, nil
}
