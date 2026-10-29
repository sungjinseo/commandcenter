package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// DeleteComponent removes one version of a hosted package (deleteVersions).
// A version that is gone already is not an error, and one whose tarball
// changed since cleanup evaluated it is skipped (cleanup.CheckUnchanged).
// actor is recorded on the event. Result counts the tarball, the component's
// only file; the package document is metadata. Cleanup ignores writePolicy.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "npm" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("npm component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("npm component %q is incomplete", c.Label())
	}
	name := c.Name
	if c.Group != "" {
		name = c.Group + "/" + c.Name
	}
	_, res, err := h.deleteVersions(ctx, rp, name, actor, func(tx *meta.Tx, _ []meta.Package) ([]string, error) {
		switch a, err := tx.GetAsset(ctx, rp.Name, TarballKey(name, TarballFile(name, c.Version))); {
		case err == nil:
			if err := cleanup.CheckUnchanged(c, []meta.Asset{a}); err != nil {
				return nil, err
			}
		case !errors.Is(err, meta.ErrNotFound):
			return nil, err
		}
		return []string{c.Version}, nil
	})
	return res, err
}

// deleteVersions removes versions of the hosted package name in one
// incarnation-guarded transaction: for each version pick chooses from the
// rows stored now, its tarball and packages row, and the dist-tags that point
// at it. When latest pointed at one, latest moves to the newest remaining
// release (or the newest prerelease when only prereleases are left). When no
// version remains, the package document and its tags go too, so the package
// answers 404. A chosen version that is gone already is skipped; an error
// from pick deletes nothing. Each version removed is a delete event by actor.
// It returns how many versions it removed and their tarballs.
func (h *Handler) deleteVersions(ctx context.Context, rp *repo.Repository, name, actor string,
	pick func(tx *meta.Tx, stored []meta.Package) ([]string, error)) (int, component.Result, error) {
	unlock, err := h.Store.Lock(ctx, "npm\x00"+rp.Name+"\x00"+name)
	if err != nil {
		return 0, component.Result{}, err
	}
	defer unlock()

	var res component.Result
	var gone []string // versions whose row or tarball was removed
	var files map[string]int
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res, gone, files = component.Result{}, nil, map[string]int{}
		stored, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		versions, err := pick(tx, stored)
		if err != nil {
			return err
		}
		dropped := map[string]bool{} // versions whose row was removed
		for _, v := range versions {
			switch err := tx.DeletePackage(ctx, rp.Name, name, v); {
			case err == nil:
				dropped[v] = true
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
			switch size, err := tx.DeleteAsset(ctx, rp.Name, TarballKey(name, TarballFile(name, v))); {
			case err == nil:
				res.Add(size)
				files[v]++
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
			if dropped[v] || files[v] > 0 {
				gone = append(gone, v)
			}
		}
		if len(dropped) == 0 {
			return nil // no version row: the package document is unchanged
		}
		newest, left := "", false
		for _, p := range stored {
			if !dropped[p.Version] {
				newest, left = semver.NewerRelease(newest, p.Version), true
			}
		}
		tags, err := tx.ListDistTags(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		for _, t := range tags {
			switch {
			case left && !dropped[t.Version]:
				continue
			case left && t.Tag == "latest":
				err = tx.SetDistTag(ctx, rp.Name, name, "latest", newest)
			default:
				err = tx.DeleteDistTag(ctx, rp.Name, name, t.Tag)
			}
			if err != nil {
				return err
			}
		}
		if left {
			return h.writePackument(ctx, tx, rp, name)
		}
		if _, err := tx.DeleteAsset(ctx, rp.Name, PackumentKey(name)); err != nil && !errors.Is(err, meta.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, component.Result{}, err
	}
	for _, v := range gone {
		h.Store.EmitAs("delete", actor, rp.Name, name+"@"+v, fmt.Sprintf("assets=%d", files[v]))
	}
	return len(gone), res, nil
}

// unpublish answers npm unpublish, which needs the delete privilege (Access)
// in a hosted repository or a group's writeMember. npm removes one version by
// sending the package document back without it, PUT {pkg}/-rev/{rev}, then
// deleting its tarball, DELETE {pkg}/-/{file}/-rev/{rev}; a whole package goes
// with DELETE {pkg}/-rev/{rev}. Each request removes whole versions
// (deleteVersions), so the PUT already takes the tarball and the DELETE that
// follows answers 404, which npm reads as done. rev is the document's _rev
// (packumentRev): a request is refused (409) when a version it would delete
// was published, or published again, after that rev, so such a version is
// never deleted; removals and dist-tag or deprecate edits since npm read the
// document do not refuse it. npm sends "undefined" for a document an older
// build wrote without _rev; that, and any rev this build did not write (not
// a number), is not checked, as the request's own safety net is all it is.
func (h *Handler) unpublish(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if !repo.AcceptsWrites(rp) {
		// The resource accepts no method here: an empty Allow.
		return repo.MethodNotAllowed("", "unpublish is not supported in %s repository %q: unpublish from its hosted repository", rp.Type, rp.Name)
	}
	allow := "PUT, DELETE"
	if rt.File != "" {
		allow = http.MethodDelete
	}
	if r.Method != http.MethodDelete && (r.Method != http.MethodPut || rt.File != "") {
		return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
	}
	target, err := repo.WriteTarget(rp)
	if err != nil {
		return repo.WithAllow(err, allow)
	}
	if target.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", target.Name)
	}
	ctx := r.Context()
	var pick func(*meta.Tx, []meta.Package) ([]string, error)
	switch {
	case rt.File != "":
		v, ok := strings.CutPrefix(rt.File, baseName(rt.Package)+"-")
		v, isTgz := strings.CutSuffix(v, ".tgz")
		if !ok || !isTgz || v == "" {
			return repo.Errorf(http.StatusNotFound, "tarball %q not found", rt.File)
		}
		pick = func(*meta.Tx, []meta.Package) ([]string, error) { return []string{v}, nil }
	case r.Method == http.MethodDelete:
		pick = func(_ *meta.Tx, stored []meta.Package) ([]string, error) {
			all := make([]string, len(stored))
			for i, p := range stored {
				all[i] = p.Version
			}
			return all, nil
		}
	default:
		if pick, err = h.unpublishPick(ctx, w, r, target, rt.Package); err != nil {
			return err
		}
	}
	if seen, err := strconv.ParseInt(rt.Rev, 10, 64); err == nil {
		inner := pick
		pick = func(tx *meta.Tx, stored []meta.Package) ([]string, error) {
			drop, err := inner(tx, stored)
			if err != nil {
				return nil, err
			}
			for _, p := range stored {
				if p.CreatedAt.UnixMilli() > seen && slices.Contains(drop, p.Version) {
					return nil, repo.Errorf(http.StatusConflict, "%s@%s was published after revision %q was read: read the package again and retry", rt.Package, p.Version, rt.Rev)
				}
			}
			return drop, nil
		}
	}
	n, _, err := h.deleteVersions(ctx, target, rt.Package, auth.FromContext(ctx).Owner(), pick)
	if err != nil {
		return err
	}
	if n == 0 && r.Method == http.MethodDelete {
		if rt.File != "" {
			return repo.Errorf(http.StatusNotFound, "tarball %q not found", rt.File)
		}
		return repo.Errorf(http.StatusNotFound, "package %q not found", rt.Package)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// unpublishPick reads the document of an unpublish PUT and returns the pick
// that removes the stored versions it no longer lists under "versions". Such
// a version must be under "time" (npm leaves it there): one missing from both
// was published after the document was read, and the PUT is refused (409),
// so a concurrent publish is never lost. A version deleted and published
// again since is under "time" too; the rev check in unpublish refuses that
// one. dist-tags in the document are ignored; deleteVersions moves them. The
// body is capped like a publish manifest and decoded in a publish slot; only
// keys naming stored versions are kept, so memory is bounded by the stored
// package, not by the body.
func (h *Handler) unpublishPick(ctx context.Context, w http.ResponseWriter, r *http.Request, rp *repo.Repository, name string) (func(*meta.Tx, []meta.Package) ([]string, error), error) {
	stored, err := h.Store.DB.ListPackages(ctx, rp.Name, name)
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, repo.Errorf(http.StatusNotFound, "package %q not found", name)
	}
	known := map[string]bool{}
	for _, p := range stored {
		known[p.Version] = true
	}
	release, err := acquirePublish(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	kept, listed := map[string]bool{}, map[string]bool{}
	docName, hasVersions := "", false
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManifest))
	err = eachEntry(dec, func(key string) error {
		into := listed
		switch key {
		case "name":
			return dec.Decode(&docName)
		case "versions":
			into, hasVersions = kept, true
		case "time":
		default:
			return skipValue(dec)
		}
		return eachEntry(dec, func(v string) error {
			if known[v] {
				into[v] = true
			}
			return skipValue(dec)
		})
	})
	switch {
	case errors.As(err, new(*http.MaxBytesError)):
		return nil, repo.Errorf(http.StatusRequestEntityTooLarge, "unpublish document is larger than %d bytes: delete the version in the web UI", maxManifest)
	case err != nil:
		return nil, bodyDecodeErr(err, "invalid unpublish document")
	case docName != name:
		return nil, repo.Errorf(http.StatusBadRequest, "package name %q does not match URL %q", docName, name)
	case !hasVersions:
		return nil, repo.Errorf(http.StatusBadRequest, "invalid unpublish document: no versions")
	}
	return func(_ *meta.Tx, stored []meta.Package) ([]string, error) {
		var drop []string
		for _, p := range stored {
			switch {
			case kept[p.Version]:
			case !listed[p.Version]:
				return nil, repo.Errorf(http.StatusConflict, "%s@%s was published after this document was read: read the package again and retry", name, p.Version)
			default:
				drop = append(drop, p.Version)
			}
		}
		return drop, nil
	}, nil
}
