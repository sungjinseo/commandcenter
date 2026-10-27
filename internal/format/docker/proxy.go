package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// acceptManifests is what a proxy asks an upstream for, so a multi-arch
// image comes back as its index, not one platform's manifest.
var acceptManifests = http.Header{"Accept": {mtManifestList + ", " + mtOCIIndex + ", " + mtSchema2 + ", " + mtOCIManifest}}

// dockerHub are the hosts of Docker Hub, where an image without a path
// prefix is an official one under library/.
var dockerHub = map[string]bool{"registry-1.docker.io": true, "index.docker.io": true, "docker.io": true}

// upstreamPath is the path of a registry route of image on rp's remote,
// relative to the remote URL (which may or may not end in /v2/).
func upstreamPath(rp *repo.Repository, image, rest string) string {
	name := image
	if dockerHub[rp.RemoteURL.Hostname()] && !strings.Contains(image, "/") {
		name = "library/" + image
	}
	if strings.HasSuffix(rp.RemoteURL.Path, "/v2/") {
		return name + rest
	}
	return "v2/" + name + rest
}

// memberFailure logs a group member's error and returns it as a 5xx, as
// cargo groups do: a later (public) member never stands in for a failing
// earlier one.
func memberFailure(group, m *repo.Repository, err error) error {
	slog.Error("docker group member failed", "group", group.Name, "member", m.Name, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}

// walk runs fn on the repository that answers for image in rp: rp itself,
// or for a group its online members in order (nested groups in place,
// offline members skipped as cargo groups do) until one answers. A hosted
// member that has the image owns it: its not found is the group's answer,
// so a tag or blob a later (proxy) member has under the same name never
// stands in for an internal image (the owner rule of npm and PyPI groups).
// A member's failure other than not found ends the walk (memberFailure).
func (h *Handler) walk(ctx context.Context, rp *repo.Repository, image string, fn func(*repo.Repository) error) error {
	if rp.Type != "group" {
		return fn(rp)
	}
	for _, m := range rp.OnlineMembers() {
		err := h.walk(ctx, m, image, fn)
		if err == nil {
			return nil
		}
		if repo.StatusOf(err) != http.StatusNotFound {
			return memberFailure(rp, m, err)
		}
		if m.Type == "hosted" {
			st, serr := h.Store.DB.PackageStamp(ctx, m.Name, image)
			if serr != nil {
				return memberFailure(rp, m, serr)
			}
			if st.Count > 0 {
				return err
			}
		}
	}
	return Err(http.StatusNotFound, CodeNameUnknown, "repository name not known to registry: "+rp.Name+"/"+image)
}

// manifest finds the manifest ref (tag or digest) of image in rp (hosted or
// proxy; groups go through walk). digest is what Docker-Content-Digest
// reports: the asset's own sha256, which is the manifest digest since
// manifests are stored verbatim.
func (h *Handler) manifest(ctx context.Context, rp *repo.Repository, image, ref string) (proxy.Result, string, error) {
	notFound := Err(http.StatusNotFound, CodeManifestUnknown, "manifest unknown: "+ref)
	if !ValidDigest(ref) && !ValidTag(ref) {
		return proxy.Result{}, "", notFound
	}
	if rp.Type != "proxy" {
		digest := ref
		if !ValidDigest(ref) {
			tags, err := h.Store.DB.ListDistTags(ctx, rp.Name, image)
			if err != nil {
				return proxy.Result{}, "", err
			}
			digest = ""
			for _, t := range tags {
				if t.Tag == ref {
					digest = t.Version
				}
			}
			if digest == "" {
				return proxy.Result{}, "", notFound
			}
		}
		a, err := h.Store.DB.GetAsset(ctx, rp.Name, manifestKey(image, digest))
		if errors.Is(err, meta.ErrNotFound) {
			return proxy.Result{}, "", notFound
		}
		return proxy.Result{Asset: a}, digest, err
	}
	t := proxy.Target{Key: manifestKey(image, ref), Remote: upstreamPath(rp, image, "/manifests/"+ref), Header: acceptManifests, NotHTML: true, DeniedIsNotFound: true}
	kind := proxy.Metadata // a tag moves: revalidated after metadataMaxAge
	if ValidDigest(ref) {
		t.SHA256, kind = strings.TrimPrefix(ref, "sha256:"), proxy.Content
	}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, kind)
	if errors.Is(err, repo.ErrNotFound) {
		return proxy.Result{}, "", notFound
	}
	return res, "sha256:" + res.Asset.BlobSHA256, err
}

// blob finds the blob digest of image in rp (hosted or proxy). A proxy
// caches it on first fetch, verified against the digest: a mismatch is a
// 502 and a negative-cache entry (proxy.Fetcher).
func (h *Handler) blob(ctx context.Context, rp *repo.Repository, image, digest string) (proxy.Result, error) {
	notFound := Err(http.StatusNotFound, CodeBlobUnknown, "blob unknown to registry: "+digest)
	if rp.Type != "proxy" {
		a, err := h.Store.DB.GetAsset(ctx, rp.Name, blobKey(image, digest))
		if errors.Is(err, meta.ErrNotFound) {
			return proxy.Result{}, notFound
		}
		return proxy.Result{Asset: a}, err
	}
	t := proxy.Target{Key: blobKey(image, digest), Remote: upstreamPath(rp, image, "/blobs/"+digest), SHA256: strings.TrimPrefix(digest, "sha256:"), NotHTML: true, DeniedIsNotFound: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
	if errors.Is(err, repo.ErrNotFound) {
		return proxy.Result{}, notFound
	}
	return res, err
}

// imageTags lists the tags of image in rp (hosted or proxy), sorted. A
// proxy serves the upstream's list, cached for metadataMaxAge (the first
// page the upstream returns; paginated here, not upstream).
func (h *Handler) imageTags(ctx context.Context, rp *repo.Repository, image string) ([]string, error) {
	var tags []string
	if rp.Type != "proxy" {
		st, err := h.Store.DB.PackageStamp(ctx, rp.Name, image)
		if err != nil {
			return nil, err
		}
		if st.Count == 0 {
			return nil, errUnknownName(rp.Name + "/" + image)
		}
		dt, err := h.Store.DB.ListDistTags(ctx, rp.Name, image)
		if err != nil {
			return nil, err
		}
		for _, t := range dt {
			tags = append(tags, t.Tag)
		}
	} else {
		t := proxy.Target{Key: image + "/tags/list", Remote: upstreamPath(rp, image, "/tags/list"), NotHTML: true, DeniedIsNotFound: true}
		res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
		if errors.Is(err, repo.ErrNotFound) {
			return nil, errUnknownName(rp.Name + "/" + image)
		}
		if err != nil {
			return nil, err
		}
		f, err := h.Store.Open(res.Asset)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		var doc struct {
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(io.LimitReader(f, maxManifest)).Decode(&doc); err != nil {
			return nil, repo.Errorf(http.StatusBadGateway, "upstream tags list is not valid JSON")
		}
		for _, t := range doc.Tags {
			if ValidTag(t) {
				tags = append(tags, t)
			}
		}
	}
	sort.Strings(tags)
	return tags, nil
}

// images lists the images rp has: a hosted repository's manifest rows, what
// a proxy has cached (its manifest assets; proxies record no package rows,
// as in the other formats), and the union over a group's online members.
func (h *Handler) images(ctx context.Context, rp *repo.Repository) ([]string, error) {
	switch rp.Type {
	case "hosted":
		return h.Store.DB.ListPackageNames(ctx, rp.Name)
	case "proxy":
		// ponytail: walks every asset path of the proxy; a path LIKE query
		// if a catalog of a large proxy gets slow.
		seen := map[string]bool{}
		var out []string
		err := h.Store.DB.WalkAssetPaths(ctx, rp.Name, "", func(path string) {
			if img, _, ok := strings.Cut(path, "/manifests/"); ok && !seen[img] {
				seen[img] = true
				out = append(out, img)
			}
		})
		return out, err
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range rp.OnlineMembers() {
		imgs, err := h.images(ctx, m)
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		for _, img := range imgs {
			if !seen[img] {
				seen[img] = true
				out = append(out, img)
			}
		}
	}
	return out, nil
}
