package maven

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
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
	Removed   int  // GA documents removed because the GA has no artifacts left (their <plugins> stay)
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

// RebuildScope narrows a rebuild (Nexus's rebuild-metadata properties):
// GroupID, then ArtifactID, then BaseVersion name the level rebuilt; empty
// is the whole repository. The levels below a named one are rebuilt too
// unless NoCascade.
type RebuildScope struct {
	GroupID, ArtifactID, BaseVersion string
	NoCascade                        bool
}

// prefix is the path prefix every asset of the scope has ("" for all).
func (s RebuildScope) prefix() string {
	if s.GroupID == "" {
		return ""
	}
	p := strings.ReplaceAll(s.GroupID, ".", "/") + "/"
	if s.ArtifactID != "" {
		p += s.ArtifactID + "/"
		if s.BaseVersion != "" {
			p += s.BaseVersion + "/"
		}
	}
	return p
}

// RebuildMetadata regenerates every GA maven-metadata.xml of hosted
// repository rp from its stored artifacts, the way a DELETE does, and
// removes GA documents whose GA has no artifact left. SNAPSHOT version-level
// documents are regenerated from the timestamped builds (or removed when no
// build is left), as after an artifact PUT or DELETE. The plugin-group
// documents' <plugins> and the archetype catalog are regenerated from the
// stored POMs and plugin jars (RebuildMetadataScope). It reads at most
// maxAssets asset paths (0 means DefaultRebuildScan) in path order; GAs that
// may have unread artifacts are skipped, and so are the orphan cleanup, the
// plugins and the catalog, when the budget is reached. logf (may be nil)
// receives progress and per-GA failures, which do not stop the run.
func (h *Handler) RebuildMetadata(ctx context.Context, rp *repo.Repository, maxAssets int,
	logf func(format string, args ...any)) (RebuildResult, error) {
	return h.RebuildMetadataScope(ctx, rp, maxAssets, RebuildScope{}, logf)
}

// RebuildMetadataScope is RebuildMetadata for the part of rp scope names:
// a groupId rebuilds its plugin-group document's <plugins> and catalog
// entries and, cascading, its GAs as an artifactId does; an artifactId its
// GA document and, cascading, its SNAPSHOT version documents; a baseVersion
// that version's document.
func (h *Handler) RebuildMetadataScope(ctx context.Context, rp *repo.Repository, maxAssets int, scope RebuildScope,
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
	prefix := scope.prefix()
	inScope := func(k gaKey) bool {
		return (scope.GroupID == "" || k.group == scope.GroupID) && (scope.ArtifactID == "" || k.artifact == scope.ArtifactID)
	}
	cascade := !scope.NoCascade || scope.GroupID == ""
	doGAs := scope.BaseVersion == "" && (scope.ArtifactID != "" || cascade)
	doSnaps := scope.BaseVersion != "" || cascade
	doGroups := scope.ArtifactID == "" // plugin-group documents and the catalog

	gas := map[gaKey]map[string]bool{} // GA -> its base versions
	var metaDirs []string              // directories holding a maven-metadata.xml
	snapDirs := map[string]bool{}      // SNAPSHOT version directories (files or a document)
	after, scanned, next := prefix, 0, ""
scan:
	for {
		want := min(rebuildPage, maxAssets+1-scanned)
		page, err := h.Store.DB.ScanAssets(ctx, rp.Name, after, want)
		if err != nil {
			return res, err
		}
		for _, a := range page {
			if !strings.HasPrefix(a.Path, prefix) {
				break scan
			}
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
					if !inScope(dirGA(dir(dir(p.Main)))) || (scope.BaseVersion != "" && dir(p.Main)+"/" != prefix) {
						break
					}
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
				if !inScope(k) || (scope.BaseVersion != "" && d+"/" != prefix) {
					break
				}
				newSnap := p.Coords.Snapshot && !snapDirs[d]
				if (gas[k] == nil && len(gas) >= maxRebuildGAs) || (newSnap && len(snapDirs) >= maxRebuildGAs) {
					res.Truncated, next = true, a.Path
					break scan
				}
				if gas[k] == nil {
					gas[k] = map[string]bool{}
				}
				gas[k][p.Coords.BaseVersion] = true
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
		if !doGAs {
			break
		}
		if err := h.checkRebuild(ctx, rp); err != nil {
			return res, err
		}
		if _, err := h.rebuildGA(ctx, rp, k.group, k.artifact, true); err != nil {
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
		if !doSnaps || res.Truncated && strings.HasPrefix(next, d+"/") {
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

	// Orphans: a GA-level document whose GA has no artifact left. The
	// directory names the GA, whatever groupId and artifactId the document
	// names (older builds stored uploads as sent). A plugin-group document
	// keeps its <plugins>; version-level (SNAPSHOT) ones were handled above.
	if !res.Truncated && doGAs && scope.ArtifactID == "" {
		for _, d := range metaDirs {
			k := dirGA(d)
			if gas[k] != nil || gaDir(k.group, k.artifact) != d || !inScope(k) {
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
			if m == nil || m.Versioning == nil {
				continue
			}
			if _, err := h.rebuildGA(ctx, rp, k.group, k.artifact, true); err != nil {
				res.Failed++
				logf("%s:%s: removing the orphan metadata failed: %v", k.group, k.artifact, err)
				continue
			}
			res.Removed++
			logf("%s:%s: removed the versions in maven-metadata.xml, the GA has no artifacts", k.group, k.artifact)
		}
	}
	if doGroups && !res.Truncated {
		if err := h.rebuildGroups(ctx, rp, scope.GroupID, gas, metaDirs, &res, logf); err != nil {
			return res, err
		}
	}
	return res, nil
}

// rebuildGroups regenerates, from the stored POMs and plugin jars, the
// <plugins> of the plugin-group documents of groupID ("" for every one) and
// the archetype catalog's entries. gas are the GAs the scan found, with
// their versions, metaDirs the directories holding a maven-metadata.xml.
// An uploaded plugin entry of a GA that still has versions is kept as sent;
// entries of GAs without versions are dropped.
// ponytail: one POM read per GA (and a jar read per plugin); record the
// packaging at upload if rebuilds of huge repositories get slow.
func (h *Handler) rebuildGroups(ctx context.Context, rp *repo.Repository, groupID string, gas map[gaKey]map[string]bool,
	metaDirs []string, res *RebuildResult, logf func(string, ...any)) error {
	plugins := map[string][]Plugin{} // groupId -> plugins found
	var archetypes []Archetype
	keys := make([]gaKey, 0, len(gas))
	for k := range gas {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return gaDir(keys[i].group, keys[i].artifact) < gaDir(keys[j].group, keys[j].artifact)
	})
	for _, k := range keys {
		vs := make([]string, 0, len(gas[k]))
		for v := range gas[k] {
			vs = append(vs, v)
		}
		sort.Strings(vs)
		if err := h.checkRebuild(ctx, rp); err != nil {
			return err
		}
		facts, ok := h.readGAFacts(ctx, rp, k.group, k.artifact, vs)
		switch {
		case !ok:
		case facts.Packaging == archetypePackaging:
			for _, v := range vs {
				archetypes = append(archetypes, Archetype{GroupID: k.group, ArtifactID: k.artifact, Version: v})
			}
		case facts.prefix != "":
			plugins[k.group] = append(plugins[k.group], Plugin{Name: cmpOr(facts.Name, k.artifact), Prefix: facts.prefix, ArtifactID: k.artifact})
		}
	}
	groups := map[string]bool{}
	for g := range plugins {
		groups[g] = true
	}
	for _, d := range metaDirs { // stored plugin-group documents, which may hold stale entries
		if g := strings.ReplaceAll(d, "/", "."); gas[dirGA(d)] == nil && (groupID == "" || g == groupID) {
			groups[g] = true
		}
	}
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		found := plugins[g]
		err := h.editPlugins(ctx, rp, strings.ReplaceAll(g, ".", "/"), func(old []Plugin) []Plugin {
			var out []Plugin
			kept := map[string]bool{}
			for _, p := range old {
				if gas[gaKey{g, p.ArtifactID}] != nil {
					out = append(out, p)
					kept[p.ArtifactID] = true
				}
			}
			for _, p := range found {
				if !kept[p.ArtifactID] {
					out = append(out, p)
				}
			}
			return out
		})
		if err != nil {
			res.Failed++
			logf("%s: plugin-group metadata rebuild failed: %v", g, err)
		}
	}
	unlock, err := h.lockCatalog(ctx, rp)
	if err != nil {
		return err
	}
	defer unlock()
	if groupID != "" || rp.LayoutPolicy == "permissive" {
		// Keep the other groups' entries, and in a permissive repository,
		// which takes a catalog upload, the entries sent.
		old, _, err := h.loadCatalog(ctx, rp)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, a := range archetypes {
			seen[a.GroupID+":"+a.ArtifactID+":"+a.Version] = true
		}
		for _, a := range old {
			if (groupID != "" && a.GroupID != groupID || rp.LayoutPolicy == "permissive") && !seen[a.GroupID+":"+a.ArtifactID+":"+a.Version] {
				archetypes = append(archetypes, a)
			}
		}
	}
	if err := h.storeCatalog(ctx, rp, archetypes); err != nil {
		res.Failed++
		logf("%s: rebuild failed: %v", catalogPath, err)
	}
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

// PurgeUnfetchable deletes the assets of Maven proxy rp at paths a proxy no
// longer fetches (fetchable): the directory listings and IDE indexes that
// older builds cached, which are not served any more but still show in
// Browse (QA L-12). An asset at a Maven-file path is kept whatever its
// content type, as it may be a real artifact an upstream mislabelled. The
// blobs are left to blobstore.compact. logf (may be nil) receives the first
// removed paths. It returns how many assets it deleted.
func (h *Handler) PurgeUnfetchable(ctx context.Context, rp *repo.Repository, logf func(format string, args ...any)) (int, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if rp.Format != "maven" || rp.Type != "proxy" {
		return 0, fmt.Errorf("purge %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if rp.LayoutPolicy == "permissive" { // it fetches any path: what it cached is wanted
		logf("layout policy permissive: cached files that are not Maven files are kept")
		return 0, nil
	}
	removed, after := 0, ""
	for {
		page, err := h.Store.DB.ScanAssets(ctx, rp.Name, after, rebuildPage)
		if err != nil {
			return removed, err
		}
		var gone []string
		for _, a := range page {
			if p, err := ParsePath(a.Path); err == nil && !p.fetchable() {
				gone = append(gone, a.Path)
			}
		}
		if len(gone) > 0 {
			n := 0
			err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
				for _, g := range gone {
					if _, err := tx.DeleteAsset(ctx, rp.Name, g); err == nil {
						n++
					} else if !errors.Is(err, meta.ErrNotFound) {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return removed, err
			}
			for i, g := range gone {
				if removed+i < 20 {
					logf("removed %q", g)
				}
			}
			removed += n
		}
		if len(page) < rebuildPage {
			return removed, nil
		}
		after = page[len(page)-1].Path
	}
}
