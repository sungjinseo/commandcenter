package maven

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// The archetype catalog (archetype-catalog.xml at the repository root), as
// mvn archetype:generate reads it. A proxy fetches the remote's. A hosted
// repository's is the server's: an upload of a POM with packaging
// maven-archetype adds its GA and base version, a delete that leaves a GA
// without a version drops it, and repository.rebuild-metadata regenerates it
// from the stored POMs; a repository without archetypes has none (404). A
// group merges its members' catalogs.
const (
	catalogPath        = "archetype-catalog.xml"
	archetypePackaging = "maven-archetype"
	pluginPackaging    = "maven-plugin"
	catalogNS          = "http://maven.apache.org/plugins/maven-archetype-plugin/archetype-catalog/1.0.0"
	// maxCatalog bounds a catalog read (Central's is some 10 MiB) and
	// maxCatalogEntries the archetypes kept of all members.
	maxCatalog        = 64 << 20
	maxCatalogEntries = 200000
)

// Archetype is one <archetype> of a catalog.
type Archetype struct {
	GroupID     string `xml:"groupId"`
	ArtifactID  string `xml:"artifactId"`
	Version     string `xml:"version"`
	Repository  string `xml:"repository,omitempty"`
	Description string `xml:"description,omitempty"`
}

type catalogDoc struct {
	XMLName    xml.Name    `xml:"archetype-catalog"`
	NS         string      `xml:"xmlns,attr"`
	Archetypes []Archetype `xml:"archetypes>archetype"`
}

// catalogSem bounds the group catalog merges and hosted rebuilds running at
// once: a member's catalog may be tens of MiB.
var catalogSem = make(chan struct{}, 1)

// readCatalog streams the archetypes of r, one element at a time, to add;
// add returns false to stop. An unparsable document is an error.
func readCatalog(r io.Reader, add func(Archetype) bool) error {
	d := xml.NewDecoder(io.LimitReader(r, maxCatalog))
	d.CharsetReader = latin1
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "archetype" {
			var a Archetype
			if err := d.DecodeElement(&a, &se); err != nil {
				return err
			}
			if a.GroupID != "" && a.ArtifactID != "" && a.Version != "" && !add(a) {
				return nil
			}
		}
	}
}

// writeCatalog writes archetypes deduplicated by GAV and sorted by groupId,
// artifactId and Maven version.
func writeCatalog(w io.Writer, archetypes []Archetype) error {
	sort.SliceStable(archetypes, func(i, j int) bool {
		a, b := archetypes[i], archetypes[j]
		if a.GroupID != b.GroupID {
			return a.GroupID < b.GroupID
		}
		if a.ArtifactID != b.ArtifactID {
			return a.ArtifactID < b.ArtifactID
		}
		return Compare(a.Version, b.Version) < 0
	})
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(catalogDoc{NS: catalogNS, Archetypes: archetypes}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// mergeCatalogs is the resolve.MergeFunc of a group's archetype-catalog.xml:
// the members' archetypes, the first of each GAV kept, without their
// repository element. An unparsable member is skipped.
func mergeCatalogs(ctx context.Context, members []io.ReadSeeker, w io.Writer) error {
	select {
	case catalogSem <- struct{}{}:
		defer func() { <-catalogSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	seen := map[string]bool{}
	var out []Archetype
	for _, m := range members {
		var got []Archetype
		err := readCatalog(m, func(a Archetype) bool {
			got = append(got, a)
			return len(out)+len(got) < maxCatalogEntries
		})
		if err != nil {
			slog.Warn("skipping unparsable archetype-catalog.xml in group merge", "err", err)
			continue
		}
		for _, a := range got {
			if k := a.GroupID + ":" + a.ArtifactID + ":" + a.Version; !seen[k] {
				seen[k] = true
				a.Repository = ""
				out = append(out, a)
			}
		}
	}
	return writeCatalog(w, out)
}

// loadCatalog returns the stored catalog of hosted rp (nil: none).
func (h *Handler) loadCatalog(ctx context.Context, rp *repo.Repository) ([]Archetype, bool, error) {
	a, err := h.Store.Get(ctx, rp.Name, catalogPath)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var out []Archetype
	if err := readCatalog(f, func(a Archetype) bool { out = append(out, a); return len(out) < maxCatalogEntries }); err != nil {
		slog.Warn("ignoring unparsable stored archetype-catalog.xml", "repo", rp.Name, "err", err)
		return nil, true, nil
	}
	return out, true, nil
}

// storeCatalog stores archetypes as the catalog of rp, or removes the
// catalog when there are none.
func (h *Handler) storeCatalog(ctx context.Context, rp *repo.Repository, archetypes []Archetype) error {
	if len(archetypes) == 0 {
		if err := h.Store.Delete(ctx, rp.Name, catalogPath); err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}
		return nil
	}
	var buf bytes.Buffer
	if err := writeCatalog(&buf, archetypes); err != nil {
		return err
	}
	_, err := h.Store.Put(ctx, rp.Name, catalogPath, "application/xml", &buf)
	return err
}

func (h *Handler) lockCatalog(ctx context.Context, rp *repo.Repository) (func(), error) {
	return h.Store.Lock(ctx, "maven\x00"+rp.Name+"\x00"+catalogPath)
}

// addArchetype records the GA and base version of c, whose POM has packaging
// maven-archetype, in rp's catalog.
func (h *Handler) addArchetype(ctx context.Context, rp *repo.Repository, c *Coordinates) error {
	unlock, err := h.lockCatalog(ctx, rp)
	if err != nil {
		return err
	}
	defer unlock()
	list, _, err := h.loadCatalog(ctx, rp)
	if err != nil {
		return err
	}
	for _, a := range list {
		if a.GroupID == c.GroupID && a.ArtifactID == c.ArtifactID && a.Version == c.BaseVersion {
			return nil
		}
	}
	return h.storeCatalog(ctx, rp, append(list, Archetype{GroupID: c.GroupID, ArtifactID: c.ArtifactID, Version: c.BaseVersion}))
}

// syncArchetypes drops from rp's catalog the versions of GA group:artifact
// that are not in versions (the GA's versions after a delete).
func (h *Handler) syncArchetypes(ctx context.Context, rp *repo.Repository, group, artifact string, versions []string) error {
	if _, err := h.Store.Get(ctx, rp.Name, catalogPath); errors.Is(err, repo.ErrNotFound) {
		return nil // the common case: no catalog, no lock
	}
	unlock, err := h.lockCatalog(ctx, rp)
	if err != nil {
		return err
	}
	defer unlock()
	list, found, err := h.loadCatalog(ctx, rp)
	if err != nil || !found {
		return err
	}
	keep := map[string]bool{}
	for _, v := range versions {
		keep[v] = true
	}
	out := list[:0:0]
	for _, a := range list {
		if a.GroupID != group || a.ArtifactID != artifact || keep[a.Version] {
			out = append(out, a)
		}
	}
	if len(out) == len(list) {
		return nil
	}
	return h.storeCatalog(ctx, rp, out)
}

// gaFacts is what a rebuild reads of a GA: the facts of its newest
// version's POM and, for a Maven plugin, its goal prefix.
type gaFacts struct {
	pomFacts
	prefix string
}

// readGAFacts reads the POM of the newest of versions of GA group:artifact
// (the timestamped one of a SNAPSHOT) and, for a maven-plugin, the goal
// prefix its jar's META-INF/maven/plugin.xml declares, else the one Maven
// derives from the artifactId. ok is false without a readable POM.
func (h *Handler) readGAFacts(ctx context.Context, rp *repo.Repository, group, artifact string, versions []string) (gaFacts, bool) {
	if len(versions) == 0 {
		return gaFacts{}, false
	}
	newest := versions[0]
	for _, v := range versions[1:] {
		if Compare(v, newest) > 0 {
			newest = v
		}
	}
	d := gaDir(group, artifact) + "/" + newest + "/"
	assets, err := h.Store.DB.ListAssets(ctx, rp.Name, d)
	if err != nil {
		return gaFacts{}, false
	}
	var pom meta.Asset
	for _, a := range assets {
		p, err := ParsePath(a.Path)
		if err != nil || p.Hash != "" || p.Coords == nil || dir(a.Path)+"/" != d || p.Coords.Extension != "pom" || p.Coords.Classifier != "" {
			continue
		}
		if pom.Path == "" || a.Path > pom.Path { // the newest build's POM of a SNAPSHOT
			pom = a
		}
	}
	if pom.Path == "" || pom.Size > maxPOMSize {
		return gaFacts{}, false
	}
	f, err := h.Store.Open(pom)
	if err != nil {
		return gaFacts{}, false
	}
	defer f.Close()
	vals, err := readPOM(f)
	if err != nil {
		return gaFacts{}, false
	}
	name, _ := expand(vals, vals["name"])
	facts := gaFacts{pomFacts: pomFacts{Packaging: vals["packaging"], Name: name}}
	if facts.Packaging == pluginPackaging {
		facts.prefix = h.jarGoalPrefix(ctx, rp, strings.TrimSuffix(pom.Path, ".pom")+".jar")
		if facts.prefix == "" {
			facts.prefix = defaultGoalPrefix(artifact)
		}
	}
	return facts, true
}

// maxPluginXML bounds the plugin descriptor read from a jar.
const maxPluginXML = 16 << 20

// jarGoalPrefix is the <goalPrefix> of the plugin descriptor in the jar at
// path, "" when there is none.
func (h *Handler) jarGoalPrefix(ctx context.Context, rp *repo.Repository, path string) string {
	a, err := h.Store.Get(ctx, rp.Name, path)
	if err != nil {
		return ""
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return ""
	}
	defer f.Close()
	zr, err := zip.NewReader(f, a.Size)
	if err != nil {
		return ""
	}
	for _, e := range zr.File {
		if e.Name != "META-INF/maven/plugin.xml" {
			continue
		}
		rc, err := e.Open()
		if err != nil {
			return ""
		}
		defer rc.Close()
		return goalPrefix(io.LimitReader(rc, maxPluginXML))
	}
	return ""
}

// goalPrefix reads <plugin><goalPrefix> of a plugin descriptor; it stops at
// the first child element below <plugin> that holds others (mojos), which
// follows it.
func goalPrefix(r io.Reader) string {
	d := xml.NewDecoder(r)
	d.CharsetReader = latin1
	depth := 0
	for {
		tok, err := d.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && t.Name.Local == "goalPrefix" {
				var s string
				if d.DecodeElement(&s, &t) != nil {
					return ""
				}
				return strings.TrimSpace(s)
			}
			if depth == 3 {
				return ""
			}
		case xml.EndElement:
			depth--
		}
	}
}

var (
	prefixMavenRE  = regexp.MustCompile(`-?maven-?`)
	prefixPluginRE = regexp.MustCompile(`-?plugin-?`)
)

// defaultGoalPrefix is the prefix Maven derives from a plugin's artifactId
// when its descriptor names none: maven-foo-plugin and foo-maven-plugin
// give foo.
func defaultGoalPrefix(artifact string) string {
	if artifact == "maven-plugin-plugin" {
		return "plugin"
	}
	return prefixPluginRE.ReplaceAllString(prefixMavenRE.ReplaceAllString(artifact, ""), "")
}

// editPlugins replaces the <plugins> of the plugin-group document in
// directory groupDir with what edit makes of them, under the directory's
// lock. A document left with neither plugins nor versions is removed.
func (h *Handler) editPlugins(ctx context.Context, rp *repo.Repository, groupDir string, edit func([]Plugin) []Plugin) error {
	unlock, err := h.Store.Lock(ctx, "maven\x00"+rp.Name+"\x00"+groupDir)
	if err != nil {
		return err
	}
	defer unlock()
	path := groupDir + "/maven-metadata.xml"
	m, err := h.loadMetadata(ctx, rp, path)
	if err != nil {
		return err
	}
	if m == nil {
		m = &Metadata{}
	}
	plugins := edit(append([]Plugin(nil), m.Plugins...))
	sort.SliceStable(plugins, func(i, j int) bool { return plugins[i].ArtifactID < plugins[j].ArtifactID })
	if samePlugins(m.Plugins, plugins) {
		return nil
	}
	m.Plugins = plugins
	if len(plugins) == 0 && m.Versioning == nil {
		if err := h.Store.Delete(ctx, rp.Name, path); err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}
		return nil
	}
	if m.ModelVersion == "" {
		m.ModelVersion = "1.1.0"
	}
	return h.storeMetadata(ctx, rp, path, m)
}

func samePlugins(a, b []Plugin) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dropPlugin removes the entries of plugin artifact from the plugin-group
// document of its groupId, once its GA has no version left, so the prefix
// no longer resolves to a deleted plugin.
func (h *Handler) dropPlugin(ctx context.Context, rp *repo.Repository, group, artifact string) error {
	groupDir := strings.ReplaceAll(group, ".", "/")
	m, err := h.loadMetadata(ctx, rp, groupDir+"/maven-metadata.xml")
	if err != nil || m == nil || !hasPlugin(m.Plugins, artifact) {
		return err // the common case: nothing to drop, no lock
	}
	return h.editPlugins(ctx, rp, groupDir, func(old []Plugin) []Plugin {
		keep := old[:0]
		for _, p := range old {
			if p.ArtifactID != artifact {
				keep = append(keep, p)
			}
		}
		return keep
	})
}

func hasPlugin(plugins []Plugin, artifact string) bool {
	for _, p := range plugins {
		if p.ArtifactID == artifact {
			return true
		}
	}
	return false
}

// importedArchetype adds an imported component to dst's catalog when its
// POM (among files, as stored in dst) has packaging maven-archetype.
func (h *Handler) importedArchetype(ctx context.Context, dst *repo.Repository, files []meta.Asset) {
	for _, a := range files {
		p, err := ParsePath(a.Path)
		if err != nil || p.Hash != "" || p.Coords == nil || p.Coords.Extension != "pom" || p.Coords.Classifier != "" || a.Size > maxPOMSize {
			continue
		}
		f, err := h.Store.Open(meta.Asset{Repo: dst.Name, BlobSHA256: a.BlobSHA256})
		if err != nil {
			continue
		}
		vals, err := readPOM(f)
		f.Close()
		if err == nil && vals["packaging"] == archetypePackaging {
			if err := h.addArchetype(ctx, dst, p.Coords); err != nil {
				slog.Warn("archetype-catalog.xml update failed", "repo", dst.Name, "path", a.Path, "err", err)
			}
		}
		return
	}
}
