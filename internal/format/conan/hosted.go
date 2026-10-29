package conan

import (
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
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// revision is one recipe or package revision and when it was stored.
type revision struct {
	Rev  string
	Time time.Time
}

func (r revision) json() map[string]any {
	return map[string]any{"revision": r.Rev, "time": formatTime(r.Time)}
}

// revisions lists the recipe revisions of ref in hosted rp, newest first:
// one packages row per revision (name = the reference, version = the
// revision), stored with the revision's first file.
func (h *Handler) revisions(ctx context.Context, rp *repo.Repository, ref Ref) ([]revision, error) {
	rows, err := h.Store.DB.ListPackages(ctx, rp.Name, ref.String())
	if err != nil {
		return nil, err
	}
	out := make([]revision, 0, len(rows))
	for _, p := range rows {
		out = append(out, revision{p.Version, p.CreatedAt})
	}
	sortRevisions(out)
	return out, nil
}

func sortRevisions(revs []revision) {
	sort.Slice(revs, func(i, j int) bool {
		if !revs[i].Time.Equal(revs[j].Time) {
			return revs[i].Time.After(revs[j].Time)
		}
		return revs[i].Rev < revs[j].Rev
	})
}

// groupRevisions groups the assets under prefix (<rev>/files/...) by
// revision, newest first; a revision's time is its newest file's.
func groupRevisions(assets []meta.Asset, prefix string) []revision {
	times := map[string]time.Time{}
	for _, a := range assets {
		rev, rest, ok := strings.Cut(strings.TrimPrefix(a.Path, prefix), "/")
		if !ok || !strings.HasPrefix(rest, "files/") {
			continue
		}
		if a.CreatedAt.After(times[rev]) {
			times[rev] = a.CreatedAt
		}
	}
	out := make([]revision, 0, len(times))
	for rev, t := range times {
		out = append(out, revision{rev, t})
	}
	sortRevisions(out)
	return out
}

// filesDoc is the {"files": {name: {}}} listing of the assets under dir.
func filesDoc(assets []meta.Asset, dir string) (doc, error) {
	files := map[string]any{}
	for _, a := range assets {
		files[strings.TrimPrefix(a.Path, dir)] = map[string]any{}
	}
	if len(files) == 0 {
		return doc{}, repo.ErrNotFound
	}
	return jsonDoc(map[string]any{"files": files})
}

func jsonDoc(v any) (doc, error) {
	body, err := json.Marshal(v)
	return doc{body: body}, err
}

// hostedDoc computes a v2 metadata document of hosted rp.
func (h *Handler) hostedDoc(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	switch rt.Kind {
	case KindLatest, KindRevisions:
		revs, err := h.revisions(ctx, rp, rt.Ref)
		if err != nil {
			return doc{}, err
		}
		if len(revs) == 0 {
			return doc{}, repo.Errorf(http.StatusNotFound, "recipe %s not found", rt.Ref)
		}
		if rt.Kind == KindLatest {
			return jsonDoc(revs[0].json())
		}
		list := make([]map[string]any, 0, len(revs))
		for _, r := range revs {
			list = append(list, r.json())
		}
		return jsonDoc(map[string]any{"revisions": list})
	case KindFiles:
		dir := revDir(rt.Ref, rt.RRev) + "files/"
		assets, err := h.Store.DB.ListAssets(ctx, rp.Name, dir)
		if err != nil {
			return doc{}, err
		}
		return filesDoc(assets, dir)
	case KindPkgSearch:
		return h.packageSearch(ctx, rp, rt.Ref, rt.RRev)
	case KindPkgLatest, KindPkgRevisions:
		dir := pkgDir(rt.Ref, rt.RRev, rt.PkgID) + "revisions/"
		assets, err := h.Store.DB.ListAssets(ctx, rp.Name, dir)
		if err != nil {
			return doc{}, err
		}
		revs := groupRevisions(assets, dir)
		if len(revs) == 0 {
			return doc{}, repo.Errorf(http.StatusNotFound, "package %s:%s not found", rt.Ref, rt.PkgID)
		}
		if rt.Kind == KindPkgLatest {
			return jsonDoc(revs[0].json())
		}
		list := make([]map[string]any, 0, len(revs))
		for _, r := range revs {
			list = append(list, r.json())
		}
		return jsonDoc(map[string]any{"revisions": list})
	case KindPkgFiles:
		dir := prevDir(rt.Ref, rt.RRev, rt.PkgID, rt.PRev) + "files/"
		assets, err := h.Store.DB.ListAssets(ctx, rp.Name, dir)
		if err != nil {
			return doc{}, err
		}
		return filesDoc(assets, dir)
	}
	return doc{}, repo.Errorf(http.StatusNotFound, "not found")
}

// packageSearch is .../revisions/<rrev>/search: every package id of the
// recipe revision with the settings, options and requires of its latest
// revision's conaninfo.txt (and the file's content). An unknown recipe
// revision is 404; one without packages answers {}.
func (h *Handler) packageSearch(ctx context.Context, rp *repo.Repository, ref Ref, rrev string) (doc, error) {
	revs, err := h.revisions(ctx, rp, ref)
	if err != nil {
		return doc{}, err
	}
	known := false
	for _, r := range revs {
		known = known || r.Rev == rrev
	}
	if !known {
		return doc{}, repo.Errorf(http.StatusNotFound, "recipe revision %s#%s not found", ref, rrev)
	}
	dir := revDir(ref, rrev) + "packages/"
	assets, err := h.Store.DB.ListAssets(ctx, rp.Name, dir)
	if err != nil {
		return doc{}, err
	}
	byPkg := map[string][]meta.Asset{}
	for _, a := range assets {
		id, _, _ := strings.Cut(strings.TrimPrefix(a.Path, dir), "/")
		byPkg[id] = append(byPkg[id], a)
	}
	out := map[string]any{}
	for id, pkgAssets := range byPkg {
		revs := groupRevisions(pkgAssets, dir+id+"/revisions/")
		if len(revs) == 0 {
			continue
		}
		info, err := h.conanInfo(ctx, rp, prevDir(ref, rrev, id, revs[0].Rev)+"files/conaninfo.txt")
		if err != nil {
			return doc{}, err
		}
		out[id] = info
	}
	return jsonDoc(out)
}

// conanInfo reads and parses a stored conaninfo.txt; a package without one
// (an upload in progress) reads as empty.
func (h *Handler) conanInfo(ctx context.Context, rp *repo.Repository, path string) (map[string]any, error) {
	a, err := h.Store.Get(ctx, rp.Name, path)
	if errors.Is(err, repo.ErrNotFound) {
		return ParseConanInfo(""), nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := h.readDoc(a)
	if err != nil {
		return nil, err
	}
	return ParseConanInfo(string(raw)), nil
}

func lockKey(rp *repo.Repository, ref Ref) string {
	return "conan\x00" + rp.Name + "\x00" + ref.String()
}

// put stores a recipe or package file of hosted rp at its path. The same
// content again is a 200; different content under writePolicy allow_once
// is a 409, under allow it needs Edit. A checksum deploy (X-Checksum-Deploy:
// true with X-Checksum-Sha1) is answered 201 when the stored file has that
// sha1 and 404 otherwise, so the client uploads the content. The revision's
// row is created with its first recipe file; an upload event is emitted
// with conanmanifest.txt, the last file Conan sends. Answers 201.
func (h *Handler) put(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	ctx := r.Context()
	path := rt.v2Path()
	if strings.EqualFold(r.Header.Get("X-Checksum-Deploy"), "true") {
		a, err := h.Store.Get(ctx, rp.Name, path)
		switch {
		case err == nil && strings.EqualFold(a.SHA1, r.Header.Get("X-Checksum-Sha1")):
			w.WriteHeader(http.StatusCreated)
			return nil
		case err != nil && !errors.Is(err, repo.ErrNotFound):
			return err
		}
		// ponytail: no sha1 index over the blob store; a file not at this path is uploaded again.
		return repo.Errorf(http.StatusNotFound, "no stored content with that checksum; upload it")
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, nil)
	if err != nil {
		return uploadErr(err)
	}
	unlock, err := h.Store.Lock(ctx, lockKey(rp, rt.Ref))
	if err != nil {
		return err
	}
	defer unlock()
	created := false
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		created = false
		switch a, err := tx.GetAsset(ctx, rp.Name, path); {
		case err == nil && a.BlobSHA256 == sums.SHA256:
			return nil
		case err == nil && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "%s of %s#%s already exists with different content in repository %q", rt.File, rt.Ref, rt.RRev, rp.Name)
		case err == nil && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s of %s#%s", auth.FromContext(ctx).User, rt.File, rt.Ref, rt.RRev)
		case err != nil && !errors.Is(err, meta.ErrNotFound):
			return err
		}
		if rt.Kind == KindFile {
			row := meta.Package{Repo: rp.Name, Format: "conan", Name: rt.Ref.String(), Version: rt.RRev, Attrs: "{}", CreatedAt: h.Now().UTC()}
			if err := tx.InsertPackage(ctx, row); err != nil && !errors.Is(err, meta.ErrExists) {
				return err
			}
		}
		created = true
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, path, contentType(rt.File), sums))
	})
	if err != nil {
		return err
	}
	if !created {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	if rt.File == "conanmanifest.txt" {
		subject := rt.Ref.String() + "#" + rt.RRev
		if rt.PkgID != "" {
			subject += ":" + rt.PkgID
		}
		h.Store.Emit(r, "upload", rp.Name, subject, "")
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

// uploadErr maps a body read error to its status.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "file is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return repo.Errorf(http.StatusBadRequest, "upload ended early: %v", err)
	}
	return fmt.Errorf("reading upload: %w", err)
}

// removal names what a delete removes: the assets under prefix and the
// recipe revision rows (one, or every row of the reference).
type removal struct {
	prefix  string
	rrev    string // the row to drop, "" for none
	allRows bool
}

// remove is a DELETE: the recipe (every revision), a recipe revision, all
// packages of a revision, a package (every revision) or a package revision.
// 200, or 404 when nothing was there.
func (h *Handler) remove(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	var rm removal
	label := rt.Ref.String()
	switch rt.Kind {
	case KindRecipe:
		rm = removal{prefix: rt.Ref.dir() + "revisions/", allRows: true}
	case KindRevision:
		rm = removal{prefix: revDir(rt.Ref, rt.RRev), rrev: rt.RRev}
		label += "#" + rt.RRev
	case KindPackages:
		rm = removal{prefix: revDir(rt.Ref, rt.RRev) + "packages/"}
		label += "#" + rt.RRev + ":*"
	case KindPackage:
		rm = removal{prefix: pkgDir(rt.Ref, rt.RRev, rt.PkgID)}
		label += "#" + rt.RRev + ":" + rt.PkgID
	case KindPkgRevision:
		rm = removal{prefix: prevDir(rt.Ref, rt.RRev, rt.PkgID, rt.PRev)}
		label += "#" + rt.RRev + ":" + rt.PkgID + "#" + rt.PRev
	default:
		return repo.MethodNotAllowed(allow(rt, true), "method DELETE not allowed")
	}
	res, gone, err := h.delete(r.Context(), rp, rt.Ref, rm, nil, auth.FromContext(r.Context()).Owner(), label)
	if err != nil {
		return err
	}
	if res.Assets == 0 && !gone {
		return repo.Errorf(http.StatusNotFound, "%s not found in repository %q", label, rp.Name)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// delete removes rm of ref in one incarnation-guarded transaction under
// the reference's lock, checking the assets against ev (cleanup.CheckUnchanged)
// first, and emits the delete event. gone reports that a row was dropped.
func (h *Handler) delete(ctx context.Context, rp *repo.Repository, ref Ref, rm removal, ev *component.Evaluated, actor, label string) (res component.Result, gone bool, err error) {
	unlock, err := h.Store.Lock(ctx, lockKey(rp, ref))
	if err != nil {
		return component.Result{}, false, err
	}
	defer unlock()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res, gone = component.Result{}, false
		assets, err := tx.ListAssets(ctx, rp.Name, rm.prefix)
		if err != nil {
			return err
		}
		if err := cleanup.CheckUnchanged(component.Component{Evaluated: ev, Format: "conan", Name: label}, assets); err != nil {
			return err
		}
		for _, a := range assets {
			size, err := tx.DeleteAsset(ctx, rp.Name, a.Path)
			if err != nil {
				return err
			}
			res.Add(size)
		}
		rrevs := []string{rm.rrev}
		if rm.allRows {
			rows, err := tx.ListPackages(ctx, rp.Name, ref.String())
			if err != nil {
				return err
			}
			rrevs = rrevs[:0]
			for _, p := range rows {
				rrevs = append(rrevs, p.Version)
			}
		}
		for _, rrev := range rrevs {
			if rrev == "" {
				continue
			}
			switch err := tx.DeletePackage(ctx, rp.Name, ref.String(), rrev); {
			case err == nil:
				gone = true
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
		}
		return nil
	})
	if err != nil {
		return component.Result{}, false, err
	}
	if gone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, label, fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, gone, nil
}

// DeleteComponent removes one recipe revision of a hosted repository, its
// packages included: the component (group user/channel or "", name, version
// <version>#<rrev>) of the web UI's delete and cleanup policies. A revision
// that is gone already is not an error; one that changed since an
// evaluation is skipped (cleanup.CheckUnchanged). Proxies use the generic
// proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "conan" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("conan component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	ref, rrev, ok := ComponentRef(c.Group, c.Name, c.Version)
	if !ok {
		return component.Result{}, fmt.Errorf("conan component %q is not a recipe revision (name, version#revision)", c.Label())
	}
	res, _, err := h.delete(ctx, rp, ref, removal{prefix: revDir(ref, rrev), rrev: rrev}, c.Evaluated, actor, c.Label())
	return res, err
}

// ComponentRef maps a component (group user/channel or "", name, version
// <version>#<rrev>) back to its reference and recipe revision.
func ComponentRef(group, name, version string) (Ref, string, bool) {
	v, rrev, ok := strings.Cut(version, "#")
	if !ok || name == "" || v == "" || !revRE.MatchString(rrev) {
		return Ref{}, "", false
	}
	ref := Ref{Name: name, Version: v, User: "_", Channel: "_"}
	if group != "" {
		if ref.User, ref.Channel, ok = strings.Cut(group, "/"); !ok {
			return Ref{}, "", false
		}
	}
	if _, ok := ParseRef(ref.String()); !ok {
		return Ref{}, "", false
	}
	return ref, rrev, true
}
