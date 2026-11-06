package docker

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// CollectUnused removes what no tag of the hosted repository rp reaches: the
// manifests that are no tag's and no reachable index's, last stored and last
// pulled before before, and then the layers no manifest references. An image
// built by a recent docker is an index (image and provenance), and deleting
// its tag leaves what the index listed, so this is what gives the space back
// (a blob store's compaction then frees the blobs). A layer has at least
// blob.SessionMaxAge, as a push may be on its way to reference it. With
// preview nothing changes. It returns the manifests it removed (would remove).
func (h *Handler) CollectUnused(ctx context.Context, rp *repo.Repository, before time.Time, preview bool, actor string, logf func(string, ...any)) (int64, error) {
	images, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return 0, err
	}
	var removed int64
	for _, image := range images {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		n, err := h.collectImage(ctx, rp, image, before, preview, actor, logf)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (h *Handler) collectImage(ctx context.Context, rp *repo.Repository, image string, before time.Time, preview bool, actor string, logf func(string, ...any)) (int64, error) {
	// The same lock as a push and a delete: an index naming an unused manifest
	// cannot arrive between the walk and the removal.
	unlock, err := h.Store.Lock(ctx, lockKey(rp, image))
	if err != nil {
		return 0, err
	}
	defer unlock()
	db := h.Store.DB
	pkgs, err := db.ListPackages(ctx, rp.Name, image)
	if err != nil {
		return 0, err
	}
	tags, err := db.ListDistTags(ctx, rp.Name, image)
	if err != nil {
		return 0, err
	}
	children := map[string][]string{}
	for _, p := range pkgs {
		var m manifestAttrs
		json.Unmarshal([]byte(p.Attrs), &m)
		children[p.Version] = m.Manifests
	}
	reached := map[string]bool{}
	var walk func(string)
	walk = func(d string) {
		if !reached[d] {
			reached[d] = true
			for _, c := range children[d] {
				walk(c)
			}
		}
	}
	for _, t := range tags {
		walk(t.Version)
	}
	var removed int64
	for _, p := range pkgs {
		if reached[p.Version] {
			continue
		}
		a, err := db.GetAsset(ctx, rp.Name, manifestKey(image, p.Version))
		switch {
		case err != nil && offline(err) != nil:
			return removed, err
		case err == nil && !lastUsed(a).Before(before): // in use by digest, or too new to judge
			continue
		}
		removed++
		logf("%s: unused manifest %s@%s", rp.Name, image, p.Version)
		if preview {
			continue
		}
		if err := h.deleteManifestLocked(ctx, rp, image, p.Version, actor); err != nil {
			return removed, err
		}
	}
	if preview {
		return removed, nil
	}
	return removed, h.collectLayers(ctx, rp, image, before, logf)
}

// collectLayers removes the layer rows of image no manifest references: those
// a delete kept because they were recent, and pushes that never finished.
func (h *Handler) collectLayers(ctx context.Context, rp *repo.Repository, image string, before time.Time, logf func(string, ...any)) error {
	if keep := h.now().Add(-blob.SessionMaxAge); keep.Before(before) {
		before = keep
	}
	db := h.Store.DB
	pkgs, err := db.ListPackages(ctx, rp.Name, image)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, p := range pkgs {
		var m manifestAttrs
		json.Unmarshal([]byte(p.Attrs), &m)
		for _, d := range m.Blobs {
			used[d] = true
		}
	}
	prefix := blobKey(image, "")
	assets, err := db.ListAssets(ctx, rp.Name, prefix)
	if err != nil {
		return err
	}
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		for _, a := range assets {
			d := strings.TrimPrefix(a.Path, prefix)
			if used[d] || !lastUsed(a).Before(before) {
				continue
			}
			logf("%s: unused layer %s@%s", rp.Name, image, d)
			if _, err := tx.DeleteAsset(ctx, rp.Name, a.Path); offline(err) != nil {
				return err
			}
		}
		return nil
	})
}

// lastUsed is when a manifest or layer row last showed a use: stored,
// replaced, or pulled. A row with no known time reads as just now.
func lastUsed(a meta.Asset) time.Time {
	t := a.CreatedAt
	if a.LastModified.After(t) {
		t = a.LastModified
	}
	if a.LastDownloaded.After(t) {
		t = a.LastDownloaded
	}
	if t.IsZero() {
		return time.Now()
	}
	return t
}
