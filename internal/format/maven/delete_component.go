package maven

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DeleteComponent removes every file of a hosted component (the artifacts and
// their sidecars, such as signatures; for a SNAPSHOT build, the build's
// files) in one incarnation-guarded transaction,
// then rebuilds the GA maven-metadata.xml the way DELETE does. Only files
// directly in the version directory count: a GA nested under it is another
// component. A component that has no files left is not an error. A component
// that changed since cleanup evaluated it is skipped (cleanup.CheckUnchanged).
// actor is recorded on the event. Cleanup ignores writePolicy: a
// read-only or allow_once repository is still cleaned.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "maven" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("maven component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Group == "" || c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("maven component %q is incomplete", c.Label())
	}
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = component.Result{}
		files, err := componentFiles(ctx, tx.ListAssets, rp.Name, c)
		if err != nil {
			return err
		}
		if err := cleanup.CheckUnchanged(c, files); err != nil {
			return err
		}
		for _, a := range files {
			size, err := tx.DeleteAsset(ctx, rp.Name, a.Path)
			if err != nil {
				return err
			}
			res.Add(size)
		}
		return nil
	})
	if err != nil {
		return component.Result{}, err
	}
	if res.Assets == 0 {
		return res, nil
	}
	h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	// The metadata writes are not part of the transaction: like DELETE they are
	// best-effort, and skipped once the repository was replaced.
	if h.Store.Current(rp) != nil {
		return res, nil
	}
	base := component.MavenBaseVersion(c.Version)
	verDir := gaDir(c.Group, c.Name) + "/" + base
	// A SNAPSHOT build deleted while others of its version remain leaves the
	// GA document as it is, and the version document too unless it names
	// the build: cleanup and snapshot removal delete builds by the
	// thousand, and each rebuild reads the whole directory.
	others := base != c.Version && strings.HasSuffix(base, "-SNAPSHOT") && h.hasBuilds(ctx, rp, verDir, base)
	if !others {
		if _, err := h.rebuildGA(ctx, rp, c.Group, c.Name, true); err != nil {
			slog.Warn("maven-metadata.xml rebuild failed", "repo", rp.Name, "component", c.Label(), "err", logsafe.Err(err))
		}
	}
	if strings.HasSuffix(base, "-SNAPSHOT") && (!others || h.docNames(ctx, rp, verDir, c.Version)) {
		if err := h.rebuildSnapshotVersion(ctx, rp, verDir); err != nil {
			slog.Warn("snapshot version metadata rebuild failed", "repo", rp.Name, "component", c.Label(), "err", logsafe.Err(err))
		}
	}
	return res, nil
}

// hasBuilds reports whether version directory verDir still holds a file of
// base version base among its first paths (false when unsure).
func (h *Handler) hasBuilds(ctx context.Context, rp *repo.Repository, verDir, base string) bool {
	page, err := h.Store.DB.ScanAssets(ctx, rp.Name, verDir+"/", 100)
	if err != nil {
		return false
	}
	for _, a := range page {
		if !strings.HasPrefix(a.Path, verDir+"/") {
			return false
		}
		if p, err := ParsePath(a.Path); err == nil && p.Coords != nil && dir(a.Path) == verDir && p.Coords.BaseVersion == base {
			return true
		}
	}
	return false
}

// docNames reports whether the version document of verDir names build
// version (its <snapshot> or a <snapshotVersion>), or cannot be read.
func (h *Handler) docNames(ctx context.Context, rp *repo.Repository, verDir, version string) bool {
	m, err := h.loadMetadata(ctx, rp, verDir+"/maven-metadata.xml")
	if err != nil || m == nil || m.Versioning == nil {
		return true
	}
	v := m.Versioning
	if s := v.Snapshot; s != nil && strings.TrimSuffix(m.Version, "SNAPSHOT")+s.Timestamp+"-"+strconv.Itoa(s.BuildNumber) == version {
		return true
	}
	for _, sv := range v.SnapshotVersions {
		if sv.Value == version {
			return true
		}
	}
	return false
}

// componentFiles lists the files of component c in repository repoName:
// the files directly in its version directories (component.MavenVersionDirs)
// that derive to c. list is a DB's or a transaction's ListAssets.
func componentFiles(ctx context.Context, list func(context.Context, string, string) ([]meta.Asset, error), repoName string, c component.Component) ([]meta.Asset, error) {
	var files []meta.Asset
	for _, v := range component.MavenVersionDirs(c.Version) {
		verDir := gaDir(c.Group, c.Name) + "/" + v
		assets, err := list(ctx, repoName, verDir+"/")
		if err != nil {
			return nil, err
		}
		for _, a := range assets {
			if g, name, v, ok := component.DeriveMaven(a.Path); ok && dir(a.Path) == verDir && g == c.Group && name == c.Name && v == c.Version {
				files = append(files, a)
			}
		}
	}
	return files, nil
}
