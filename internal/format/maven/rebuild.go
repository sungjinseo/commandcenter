package maven

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/repo"
)

// Bounds of one RebuildMetadata run.
const (
	// DefaultRebuildScan is how many asset paths one rebuild reads.
	DefaultRebuildScan = 1000000
	// maxRebuildGAs bounds the GAs (and orphan documents) held in memory.
	maxRebuildGAs = 100000
)

// rebuildPage is how many asset paths one query reads; a var for tests.
var rebuildPage = 1000

// RebuildResult reports a RebuildMetadata run.
type RebuildResult struct {
	GAs       int  // GA documents regenerated
	Snapshots int  // SNAPSHOT version directories checked: document regenerated, kept as is when unchanged, or removed when no build is left
	Removed   int  // GA documents removed because the GA has no artifacts left
	Failed    int  // GA or SNAPSHOT version rebuilds that failed (logged)
	Truncated bool // the scan stopped at its budget: GAs past it were not rebuilt
	// OrphansTruncated: more GA documents than the cap were found, so only the
	// first ones were checked for orphans.
	OrphansTruncated bool
	// SnapshotsTruncated: more SNAPSHOT version directories than the cap held
	// a version document, so documents of the directories past it (ones
	// without a build the scan saw) were not regenerated or removed.
	SnapshotsTruncated bool
}

type gaKey struct{ group, artifact string }

func gaDir(group, artifact string) string {
	return strings.ReplaceAll(group, ".", "/") + "/" + artifact
}

// RebuildMetadata regenerates every GA maven-metadata.xml of hosted
// repository rp from its stored artifacts, the way a DELETE does, and
// removes GA documents whose GA has no artifact left. SNAPSHOT version-level
// documents are regenerated from the timestamped builds (or removed when no
// build is left), as after an artifact PUT or DELETE. It reads at most
// maxAssets asset paths (0 means DefaultRebuildScan) in path order; GAs that
// may have unread artifacts are skipped, and so is the orphan cleanup, when
// the budget is reached. logf (may be nil) receives progress and per-GA
// failures, which do not stop the run.
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, maxAssets int,
	logf func(format string, args ...any)) (RebuildResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var res RebuildResult
	if rp.Format != "maven" || rp.Type != "hosted" {
		return res, fmt.Errorf("rebuild metadata of %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if maxAssets <= 0 {
		maxAssets = DefaultRebuildScan
	}

	gas := map[gaKey]bool{}
	var metaDirs []string         // directories holding a maven-metadata.xml
	snapDirs := map[string]bool{} // SNAPSHOT version directories (files or a document)
	after, scanned, next := "", 0, ""
scan:
	for {
		want := min(rebuildPage, maxAssets+1-scanned)
		page, err := h.Store.DB.ScanAssets(ctx, rp.Name, after, want)
		if err != nil {
			return res, err
		}
		for _, a := range page {
			if scanned == maxAssets {
				res.Truncated, next = true, a.Path
				break scan
			}
			scanned++
			p, err := ParsePath(a.Path)
			switch {
			case err != nil || p.Hash != "":
			case p.Metadata:
				switch {
				case isSnapshotPath(p):
					switch d := dir(p.Main); {
					case snapDirs[d] || len(snapDirs) < maxRebuildGAs:
						snapDirs[d] = true
					case !res.SnapshotsTruncated:
						res.SnapshotsTruncated = true
						logf("more than %d SNAPSHOT version directories: version documents without builds past the cap are not checked", maxRebuildGAs)
					}
				case len(metaDirs) < maxRebuildGAs:
					metaDirs = append(metaDirs, dir(p.Main))
				case !res.OrphansTruncated:
					res.OrphansTruncated = true
					logf("more than %d maven-metadata.xml documents: only the first are checked for orphans", maxRebuildGAs)
				}
			case p.Coords != nil:
				k := gaKey{p.Coords.GroupID, p.Coords.ArtifactID}
				d := dir(p.Main)
				newSnap := p.Coords.Snapshot && !snapDirs[d]
				if (!gas[k] && len(gas) >= maxRebuildGAs) || (newSnap && len(snapDirs) >= maxRebuildGAs) {
					res.Truncated, next = true, a.Path
					break scan
				}
				gas[k] = true
				if p.Coords.Snapshot {
					snapDirs[d] = true
				}
			}
		}
		if len(page) < want {
			break
		}
		after = page[len(page)-1].Path
	}
	if res.Truncated {
		logf("scan stopped after %d assets at %s: later GAs are not rebuilt", scanned, next)
	}

	keys := make([]gaKey, 0, len(gas))
	for k := range gas {
		// Paths sharing a prefix are contiguous: only a GA whose directory
		// holds the first unread path may have artifacts the scan missed.
		if res.Truncated && strings.HasPrefix(next, gaDir(k.group, k.artifact)+"/") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return gaDir(keys[i].group, keys[i].artifact) < gaDir(keys[j].group, keys[j].artifact)
	})
	for _, k := range keys {
		if err := h.checkRebuild(ctx, rp); err != nil {
			return res, err
		}
		if err := h.rebuildGA(ctx, rp, k.group, k.artifact, true); err != nil {
			res.Failed++
			logf("%s:%s: rebuild failed: %v", k.group, k.artifact, err)
			continue
		}
		res.GAs++
	}

	// SNAPSHOT version documents are server-owned (plan 13 Decision 6):
	// regenerate each, which also removes one whose folder has no build left.
	dirs := make([]string, 0, len(snapDirs))
	for d := range snapDirs {
		if res.Truncated && strings.HasPrefix(next, d+"/") {
			continue
		}
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if err := h.checkRebuild(ctx, rp); err != nil {
			return res, err
		}
		if err := h.rebuildSnapshotVersion(ctx, rp, d); err != nil {
			res.Failed++
			logf("%s: snapshot version metadata rebuild failed: %v", d, err)
			continue
		}
		res.Snapshots++
	}

	// Orphans: a GA-level document whose GA has no artifact left.
	// Plugin-group documents are kept; version-level (SNAPSHOT) ones were
	// handled above.
	if !res.Truncated {
		for _, d := range metaDirs {
			if gas[dirGA(d)] || gas[dirGA(path.Dir(d))] {
				continue
			}
			if err := h.checkRebuild(ctx, rp); err != nil {
				return res, err
			}
			m, err := h.loadMetadata(ctx, rp, d+"/maven-metadata.xml")
			if err != nil {
				res.Failed++
				logf("%s/maven-metadata.xml: %v", d, err)
				continue
			}
			if m == nil || m.GroupID == "" || m.ArtifactID == "" || len(m.Plugins) > 0 || gaDir(m.GroupID, m.ArtifactID) != d {
				continue
			}
			if err := h.rebuildGA(ctx, rp, m.GroupID, m.ArtifactID, true); err != nil {
				res.Failed++
				logf("%s:%s: removing the orphan metadata failed: %v", m.GroupID, m.ArtifactID, err)
				continue
			}
			res.Removed++
			logf("%s:%s: removed maven-metadata.xml, the GA has no artifacts", m.GroupID, m.ArtifactID)
		}
	}
	return res, nil
}

// checkRebuild stops a rebuild that was cancelled or whose repository was
// deleted or re-created meanwhile.
func (h *Handler) checkRebuild(ctx context.Context, rp *repo.Repository) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.Store.Current(rp)
}

// dirGA is the GA whose directory is d, the inverse of gaDir: a
// maven-metadata.xml at d belongs to that GA when the scan found it.
func dirGA(d string) gaKey {
	i := strings.LastIndexByte(d, '/')
	if i < 0 {
		return gaKey{}
	}
	return gaKey{strings.ReplaceAll(d[:i], "/", "."), d[i+1:]}
}
