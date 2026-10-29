package conan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"commandcenter/internal/repo"
)

// The v1 API (Conan 1.x without revisions) is a view over the latest
// revisions: snapshots come from the stored conanmanifest.txt, download
// and upload URLs point at the v2 file routes of the serving repository,
// and uploads land in revision "0" (what Conan 1 uses). v1 remove_files
// and packages/delete are not implemented.

// v1Revision is the revision v1 uploads go to.
const v1Revision = "0"

// latestRRev is the latest recipe revision of ref in rp (through the v2
// document, so a proxy or group answers too).
func (h *Handler) latestRRev(ctx context.Context, rp *repo.Repository, ref Ref) (string, error) {
	d, err := h.doc(ctx, rp, Route{Kind: KindLatest, Ref: ref})
	if err != nil {
		return "", err
	}
	var got struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(d.body, &got); err != nil || got.Revision == "" {
		return "", repo.Errorf(http.StatusBadGateway, "latest revision of %s is not a revision document", ref)
	}
	return got.Revision, nil
}

// latestPRev is the latest package revision of pkgid in ref#rrev.
func (h *Handler) latestPRev(ctx context.Context, rp *repo.Repository, ref Ref, rrev, pkgid string) (string, error) {
	d, err := h.doc(ctx, rp, Route{Kind: KindPkgLatest, Ref: ref, RRev: rrev, PkgID: pkgid})
	if err != nil {
		return "", err
	}
	var got struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(d.body, &got); err != nil || got.Revision == "" {
		return "", repo.Errorf(http.StatusBadGateway, "latest revision of %s:%s is not a revision document", ref, pkgid)
	}
	return got.Revision, nil
}

// fileList lists the files of a files route.
func (h *Handler) fileList(ctx context.Context, rp *repo.Repository, rt Route) ([]string, error) {
	d, err := h.doc(ctx, rp, rt)
	if err != nil {
		return nil, err
	}
	var got struct {
		Files map[string]any `json:"files"`
	}
	if err := json.Unmarshal(d.body, &got); err != nil {
		return nil, repo.Errorf(http.StatusBadGateway, "files of %s: not a files document", rt.Ref)
	}
	out := make([]string, 0, len(got.Files))
	for f := range got.Files {
		out = append(out, f)
	}
	return out, nil
}

// v1Target resolves a v1 recipe or package route to the latest revisions'
// files route (KindFiles or KindPkgFiles).
func (h *Handler) v1Target(ctx context.Context, rp *repo.Repository, rt Route) (Route, error) {
	rrev, err := h.latestRRev(ctx, rp, rt.Ref)
	if err != nil {
		return Route{}, err
	}
	if rt.PkgID == "" {
		return Route{Kind: KindFiles, Ref: rt.Ref, RRev: rrev}, nil
	}
	prev, err := h.latestPRev(ctx, rp, rt.Ref, rrev, rt.PkgID)
	if err != nil {
		return Route{}, err
	}
	return Route{Kind: KindPkgFiles, Ref: rt.Ref, RRev: rrev, PkgID: rt.PkgID, PRev: prev}, nil
}

// fileRoute is the route of one file of a files route.
func fileRoute(files Route, name string) Route {
	rt := files
	rt.File = name
	if files.Kind == KindPkgFiles {
		rt.Kind = KindPkgFile
	} else {
		rt.Kind = KindFile
	}
	return rt
}

// v1 answers the v1 read routes.
func (h *Handler) v1(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	switch rt.Kind {
	case KindPkgSearch:
		rrev, err := h.latestRRev(ctx, rp, rt.Ref)
		if err != nil {
			return doc{}, err
		}
		return h.doc(ctx, rp, Route{Kind: KindPkgSearch, Ref: rt.Ref, RRev: rrev})
	case KindRecipe, KindPackage:
		files, err := h.v1Target(ctx, rp, rt)
		if err != nil {
			return doc{}, err
		}
		return h.snapshot(ctx, rp, files)
	case KindV1Digest, KindV1PkgDigest:
		files, err := h.v1Target(ctx, rp, rt)
		if err != nil {
			return doc{}, err
		}
		return jsonDoc(map[string]string{manifestFile: h.repoURL(rp) + "/" + fileRoute(files, manifestFile).v2Path()})
	case KindV1DownloadURLs, KindV1PkgDownloadURLs:
		files, err := h.v1Target(ctx, rp, rt)
		if err != nil {
			return doc{}, err
		}
		names, err := h.fileList(ctx, rp, files)
		if err != nil {
			return doc{}, err
		}
		urls := map[string]string{}
		for _, n := range names {
			urls[n] = h.repoURL(rp) + "/" + fileRoute(files, n).v2Path()
		}
		return jsonDoc(urls)
	}
	return doc{}, repo.Errorf(http.StatusNotFound, "not found")
}

const manifestFile = "conanmanifest.txt"

// snapshot is the v1 {file: md5} listing of a files route: the md5s the
// stored conanmanifest.txt lists, and the manifest's own.
func (h *Handler) snapshot(ctx context.Context, rp *repo.Repository, files Route) (doc, error) {
	names, err := h.fileList(ctx, rp, files)
	if err != nil {
		return doc{}, err
	}
	sums := map[string]string{}
	res, err := h.file(ctx, rp, fileRoute(files, manifestFile))
	switch {
	case err == nil:
		raw, err := h.readDoc(res.Asset)
		if err != nil {
			return doc{}, err
		}
		sums = ParseManifest(string(raw))
		sums[manifestFile] = res.Asset.MD5
	case !errors.Is(err, repo.ErrNotFound) && repo.StatusOf(err) != http.StatusNotFound:
		return doc{}, err
	}
	out := map[string]string{}
	for _, n := range names {
		out[n] = sums[n]
	}
	return jsonDoc(out)
}

// v1UploadURLs answers POST upload_urls ({file: size}) with {file: url}:
// the v2 file URLs of the serving repository, in the latest recipe
// revision (v1Revision for a new recipe) and, for a package, in package
// revision v1Revision.
func (h *Handler) v1UploadURLs(w http.ResponseWriter, r *http.Request, serving, target *repo.Repository, rt Route) error {
	if rt.Kind != KindV1UploadURLs && rt.Kind != KindV1PkgUploadURLs {
		return repo.MethodNotAllowed(allow(rt, true), "method POST not allowed")
	}
	var sizes map[string]int64
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&sizes); err != nil {
		return repo.Errorf(http.StatusBadRequest, "upload_urls: body is not a {file: size} object: %v", err)
	}
	ctx := r.Context()
	rrev, err := h.latestRRev(ctx, target, rt.Ref)
	switch {
	case repo.StatusOf(err) == http.StatusNotFound && rt.PkgID == "":
		rrev = v1Revision
	case err != nil:
		return err
	}
	files := Route{Kind: KindFiles, Ref: rt.Ref, RRev: rrev}
	if rt.PkgID != "" {
		files = Route{Kind: KindPkgFiles, Ref: rt.Ref, RRev: rrev, PkgID: rt.PkgID, PRev: v1Revision}
	}
	urls := map[string]string{}
	for name := range sizes {
		frt, err := ParseRoute(fileRoute(files, name).v2Path())
		if err != nil {
			return repo.Errorf(http.StatusBadRequest, "upload_urls: invalid file name %q", name)
		}
		urls[name] = h.repoURL(serving) + "/" + frt.v2Path()
	}
	d, err := jsonDoc(urls)
	if err != nil {
		return err
	}
	repo.ServeText(w, r, string(d.body), ctJSON, h.Now())
	return nil
}
