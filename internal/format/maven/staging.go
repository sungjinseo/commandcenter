package maven

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ImportComponent copies the files of a version (those DeleteComponent
// would delete) from hosted src into hosted dst at the same paths, under
// dst's per-file locks, then rebuilds dst's GA maven-metadata.xml (and a
// SNAPSHOT's version document) as an upload does. dst's version policy
// applies (repo.ComponentImporter).
func (h *Handler) ImportComponent(ctx context.Context, src, dst *repo.Repository, c component.Component, overwrite bool) ([]meta.Asset, error) {
	if err := repo.CheckImport(src, dst, c, "maven"); err != nil {
		return nil, err
	}
	if c.Group == "" || c.Name == "" || c.Version == "" {
		return nil, fmt.Errorf("maven component %q is incomplete", c.Label())
	}
	verDir := strings.ReplaceAll(c.Group, ".", "/") + "/" + c.Name + "/" + c.Version
	all, err := h.Store.DB.ListAssets(ctx, src.Name, verDir+"/")
	if err != nil {
		return nil, err
	}
	var files []meta.Asset
	for _, a := range all {
		if g, name, v, ok := component.DeriveMaven(a.Path); ok && dir(a.Path) == verDir && g == c.Group && name == c.Name && v == c.Version {
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		return nil, repo.Errorf(http.StatusNotFound, "component %s not found in repository %q", c.Label(), src.Name)
	}
	for _, a := range files {
		p, err := ParsePath(a.Path)
		if err != nil {
			return nil, err
		}
		if err := checkVersionPolicy(dst.VersionPolicy, p); err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	keys := make([]string, len(files))
	for i, a := range files {
		keys[i] = "maven\x00asset\x00" + dst.Name + "\x00" + a.Path
	}
	unlock, err := h.Store.LockAll(ctx, keys)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := h.Store.CopyBlobs(src.Name, dst.Name, files); err != nil {
		return nil, err
	}
	if err := h.Store.Tx(ctx, dst, func(tx *meta.Tx) error {
		return repo.PutImported(ctx, tx, dst.Name, files, overwrite)
	}); err != nil {
		return nil, err
	}
	// Best-effort, as after a PUT: the files are stored.
	if err := h.rebuildGA(ctx, dst, c.Group, c.Name, false); err != nil {
		slog.Warn("maven-metadata.xml rebuild failed", "repo", dst.Name, "component", c.Label(), "err", logsafe.Err(err))
	}
	if strings.HasSuffix(c.Version, "-SNAPSHOT") {
		if err := h.rebuildSnapshotVersion(ctx, dst, verDir); err != nil {
			slog.Warn("snapshot version metadata rebuild failed", "repo", dst.Name, "component", c.Label(), "err", logsafe.Err(err))
		}
	}
	return files, nil
}
