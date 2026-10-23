package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"commandcenter/internal/component"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// scanLimit is a variable so tests can lower it.
var scanLimit = defaultScanLimit

// componentsTimeout bounds one component search; it is a variable so tests
// can shorten it.
var componentsTimeout = 10 * time.Second

const (
	componentsLimit     = 100  // components per response
	componentAssetLimit = 50   // assets listed per component
	defaultScanLimit    = 2000 // candidate assets or versions examined
)

// Component is a derived package version (Decision 3); the model lives in
// internal/component, shared with cleanup.
type Component = component.Component

// ComponentAsset is one file of a component.
type ComponentAsset = component.Asset

// criteria are the parsed search parameters.
type criteria struct {
	keyword, group, name, version, baseVersion, classifier, extension string
	format                                                            string
	repo                                                              string
	versionRE, baseVersionRE                                          *regexp.Regexp // set when the value has "*"
}

// compileGlob returns the matcher for a version value containing "*", else nil.
func compileGlob(want string) *regexp.Regexp {
	if !strings.Contains(want, "*") {
		return nil
	}
	parts := strings.Split(want, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("(?is)^" + strings.Join(parts, ".*") + "$")
}

func (c criteria) matchComponent(format, group, name, version string) bool {
	if c.keyword != "" && !(containsFold(group, c.keyword) || containsFold(name, c.keyword) || containsFold(version, c.keyword) ||
		(format == "pypi" && containsFold(name, pypi.Normalize(c.keyword)))) {
		return false
	}
	if c.group != "" && !containsFold(group, c.group) {
		return false
	}
	if c.name != "" && !(containsFold(name, c.name) || (format == "pypi" && containsFold(name, pypi.Normalize(c.name)))) {
		return false
	}
	// baseVersion is matched like version against the component version
	// (for Maven snapshots that is the base version, e.g. 1.0-SNAPSHOT).
	return matchVersion(c.version, c.versionRE, version) && matchVersion(c.baseVersion, c.baseVersionRE, version)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// matchVersion: an exact (case-insensitive) match, or re when want has "*".
func matchVersion(want string, re *regexp.Regexp, got string) bool {
	switch {
	case want == "":
		return true
	case re != nil:
		return re.MatchString(got)
	}
	return strings.EqualFold(want, got)
}

// assetParts returns the classifier and extension of a component file.
func assetParts(format, name, version, p string) (classifier, ext string) {
	file := path.Base(p)
	if format != "maven" {
		return "", strings.TrimPrefix(path.Ext(file), ".")
	}
	rest := strings.TrimPrefix(file, name+"-")
	if r, ok := strings.CutPrefix(rest, version); ok {
		rest = r
	} else if base, ok := strings.CutSuffix(version, "-SNAPSHOT"); ok {
		if r, ok := strings.CutPrefix(rest, base+"-"); ok {
			rest = component.SnapshotStamp.ReplaceAllString(r, "")
		}
	}
	if c, ok := strings.CutPrefix(rest, "-"); ok {
		i := strings.IndexByte(c, '.')
		if i < 0 {
			return c, ""
		}
		return c[:i], c[i+1:]
	}
	return "", strings.TrimPrefix(rest, ".")
}

func (c criteria) matchAsset(format, name, version, p string) bool {
	if c.classifier == "" && c.extension == "" {
		return true
	}
	cl, ext := assetParts(format, name, version, p)
	return (c.classifier == "" || containsFold(cl, c.classifier)) &&
		(c.extension == "" || containsFold(ext, strings.TrimPrefix(c.extension, ".")))
}

// longestChunk returns the longest piece of s between any of the separators.
func longestChunk(s string, seps string) string {
	best := ""
	for _, c := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		if len(c) > len(best) {
			best = c
		}
	}
	return best
}

// pathQuery picks the path substring that narrows the asset scan: every
// matching component's paths contain it. Group dots are slashes in paths.
func (c criteria) pathQuery(format string) string {
	q := c.rawPathQuery(format)
	if format == "go" { // matched against the path with "!" removed, lower-cased
		return strings.ToLower(strings.ReplaceAll(q, "!", ""))
	}
	return q
}

func (c criteria) rawPathQuery(format string) string {
	switch {
	case c.group != "":
		return strings.ReplaceAll(c.group, ".", "/")
	case c.name != "" && format == "pypi":
		return pypi.Normalize(c.name) // pypi paths always hold the normalized project
	case c.name != "":
		return c.name
	case c.version != "":
		return longestChunk(c.version, "*")
	case c.baseVersion != "":
		return longestChunk(c.baseVersion, "*")
	case c.keyword != "":
		seps := "."
		if format == "pypi" { // may match the project in any spelling: use an alphanumeric run
			seps = "._-/ "
		}
		if q := longestChunk(c.keyword, seps); q != "" {
			return q
		}
	case c.extension != "":
		return "." + strings.TrimPrefix(c.extension, ".")
	case c.classifier != "":
		return "-" + c.classifier
	}
	return "/"
}

// nameQuery picks the substring that narrows a package-name scan, or "".
// A bare keyword narrows by name too; versions that match only by version
// are found by a separate fallback scan.
func (c criteria) nameQuery(format string) string {
	norm := func(s string) string {
		if format == "pypi" {
			return pypi.Normalize(s)
		}
		return s
	}
	switch {
	case c.name != "":
		return norm(c.name)
	case c.group != "":
		return c.group
	case c.keyword != "":
		return norm(c.keyword)
	}
	return ""
}

type compKey struct{ repo, group, name, version string }

// builder accumulates components, enforcing both caps.
type builder struct {
	byKey     map[compKey]*Component
	truncated bool
}

func (b *builder) add(rp *repo.Repository, group, name, version string, a *ComponentAsset, lm time.Time) bool {
	k := compKey{rp.Name, group, name, version}
	c := b.byKey[k]
	if c == nil {
		if len(b.byKey) >= componentsLimit {
			b.truncated = true
			return false
		}
		c = &Component{Repo: rp.Name, Format: rp.Format, Group: group, Name: name, Version: version, Assets: []ComponentAsset{}}
		b.byKey[k] = c
	}
	if lm.After(c.LastUpdated) {
		c.LastUpdated = lm
	}
	if a != nil {
		if len(c.Assets) >= componentAssetLimit {
			b.truncated = true
		} else {
			c.Assets = append(c.Assets, *a)
		}
	}
	return true
}

type componentJSON struct {
	Repo        string               `json:"repo"`
	Format      string               `json:"format"`
	Group       string               `json:"group"`
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	LastUpdated string               `json:"lastUpdated"`
	Assets      []componentAssetJSON `json:"assets"`
}

type componentAssetJSON struct {
	Path           string `json:"path"`
	Size           int64  `json:"size"`
	Downloads      int64  `json:"downloads"`
	LastDownloaded string `json:"lastDownloaded"`
}

// searchComponents answers GET /-/api/search/components (Decision 4).
func (u *UI) searchComponents(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	q := r.URL.Query()
	get := func(k string) string { return strings.TrimSpace(q.Get(k)) }
	c := criteria{
		keyword: get("keyword"), group: get("group"), name: get("name"), version: get("version"),
		baseVersion: get("baseVersion"), classifier: get("classifier"), extension: get("extension"),
		format: get("format"), repo: get("repo"),
	}
	c.versionRE, c.baseVersionRE = compileGlob(c.version), compileGlob(c.baseVersion)
	enough := false
	for _, v := range []string{c.keyword, c.group, c.name, c.version, c.baseVersion, c.classifier, c.extension, c.format, c.repo} {
		if utf8.RuneCountInString(v) > maxQuery {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("parameters must be at most %d characters", maxQuery))
			return
		}
	}
	for _, v := range []string{c.keyword, c.group, c.name, c.version, c.baseVersion, c.classifier, c.extension} {
		if utf8.RuneCountInString(v) >= minQuery {
			enough = true
		}
	}
	if !enough {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("give at least one of keyword, group, name, version, baseVersion, classifier or extension with %d or more characters", minQuery))
		return
	}
	var repos []*repo.Repository
	if c.repo != "" {
		rp := u.visible(p, c.repo)
		if rp == nil {
			writeError(w, http.StatusNotFound, "repository \""+c.repo+"\" not found")
			return
		}
		repos = []*repo.Repository{rp}
	} else {
		repos = u.readable(p)
	}
	var searched []*repo.Repository
	for _, rp := range repos {
		if rp.Type != "group" && (c.format == "" || rp.Format == c.format) {
			searched = append(searched, rp)
		}
	}
	out := []componentJSON{}
	truncated := false
	if len(searched) > 0 {
		release, err := u.acquireHeavy(r.Context())
		if err != nil {
			u.fail(w, r, err)
			return
		}
		comps, tr, err := func() ([]*Component, bool, error) {
			defer release()
			return u.deriveComponents(r, searched, c)
		}()
		if err != nil {
			u.fail(w, r, err)
			return
		}
		truncated = tr
		for _, comp := range comps {
			j := componentJSON{Repo: comp.Repo, Format: comp.Format, Group: comp.Group, Name: comp.Name,
				Version: comp.Version, LastUpdated: rfc3339(comp.LastUpdated), Assets: make([]componentAssetJSON, 0, len(comp.Assets))}
			for _, a := range comp.Assets {
				j.Assets = append(j.Assets, componentAssetJSON{Path: a.Path, Size: a.Size, Downloads: a.Downloads, LastDownloaded: rfc3339(a.LastDownloaded)})
			}
			out = append(out, j)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"components": out, "truncated": truncated})
}

// deriveComponents gathers candidates for repos, derives and filters them,
// and returns at most componentsLimit components sorted by repo, group,
// name and version.
func (u *UI) deriveComponents(r *http.Request, repos []*repo.Repository, c criteria) ([]*Component, bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), componentsTimeout)
	defer cancel()
	b := &builder{byKey: map[compKey]*Component{}}
	if err := u.scanComponents(ctx, b, repos, c); err != nil {
		// A deadline of our own keeps what was found; a cancelled request
		// or any other failure is an error.
		if !errors.Is(err, context.DeadlineExceeded) || r.Context().Err() != nil {
			return nil, false, err
		}
		b.truncated = true
	}
	return b.sorted(), b.truncated, nil
}

// scanComponents fills b from the assets table and the packages table.
func (u *UI) scanComponents(ctx context.Context, b *builder, repos []*repo.Repository, c criteria) error {
	byName := map[string]*repo.Repository{}
	// Repositories whose components come from the assets table, by format.
	assetRepos := map[string][]string{}
	var pkgRepos []*repo.Repository
	for _, rp := range repos {
		byName[rp.Name] = rp
		if packageFormats[rp.Format] && rp.Type != "proxy" {
			pkgRepos = append(pkgRepos, rp)
		} else {
			assetRepos[rp.Format] = append(assetRepos[rp.Format], rp.Name)
		}
	}
	formats := make([]string, 0, len(assetRepos))
	for f := range assetRepos {
		formats = append(formats, f)
	}
	sort.Strings(formats)
	for _, f := range formats {
		search := u.DB.SearchAssetInfos
		if f == "go" { // case-encoded paths: match on the "!"-stripped lower-case path
			search = u.DB.SearchGoAssetInfos
		}
		hits, tr, err := search(ctx, assetRepos[f], c.pathQuery(f), scanLimit)
		if err != nil {
			return err
		}
		b.truncated = b.truncated || tr
		for _, h := range hits {
			group, name, version, ok := component.DeriveAsset(f, h.Path)
			if !ok || !c.matchComponent(f, group, name, version) || !c.matchAsset(f, name, version, h.Path) {
				continue
			}
			b.add(byName[h.Repo], group, name, version, &ComponentAsset{Path: h.Path, Size: h.Size, Downloads: h.Downloads, LastDownloaded: h.LastDownloaded}, h.LastModified)
		}
	}
	scanned := 0
	for _, rp := range pkgRepos {
		done := map[compKey]bool{}
		q := c.nameQuery(rp.Format)
		if err := u.scanPackages(ctx, b, rp, c, q, &scanned, nil, done); err != nil {
			return err
		}
		// A bare keyword also matches versions: after the name-narrowed pass,
		// look through the unfiltered scan, if the caps still allow. Components
		// found by the first pass are complete and skipped here.
		if q != "" && c.name == "" && c.group == "" && !b.full() && scanned < scanLimit {
			if err := u.scanPackages(ctx, b, rp, c, "", &scanned, done, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// scanPackages derives the components of the packages of rp whose names
// contain q. Components in skip (found by an earlier pass) are left out, and
// every component added is recorded in seen. A component may span several
// rows (PyPI stores one row per file), so seen must not be consulted while
// it is being filled.
func (u *UI) scanPackages(ctx context.Context, b *builder, rp *repo.Repository, c criteria, q string, scanned *int, skip, seen map[compKey]bool) error {
	names, tr, err := u.DB.SearchPackageNames(ctx, rp.Name, q, scanLimit)
	if err != nil {
		return err
	}
	b.truncated = b.truncated || tr
	for _, pkgName := range names {
		versions, tr, err := u.DB.ListVersions(ctx, rp.Name, pkgName, versionsLimit)
		if err != nil {
			return err
		}
		b.truncated = b.truncated || tr
		for _, v := range versions {
			if *scanned++; *scanned > scanLimit {
				b.truncated = true
				return nil
			}
			group, name, version, apath := component.PackageComponent(rp.Format, pkgName, v.Version)
			if !c.matchComponent(rp.Format, group, name, version) {
				continue
			}
			key := compKey{rp.Name, group, name, version}
			if skip[key] {
				continue
			}
			var asset *ComponentAsset
			lm := v.Created
			if apath != "" {
				a, err := u.DB.GetAsset(ctx, rp.Name, apath)
				switch {
				case err == nil:
					asset, lm = &ComponentAsset{Path: apath, Size: a.Size, Downloads: a.Downloads, LastDownloaded: a.LastDownloaded}, a.LastModified
				case errors.Is(err, meta.ErrNotFound):
					// the file is gone or was never stored: no asset to list
				default:
					return err
				}
			}
			if asset != nil && !c.matchAsset(rp.Format, name, version, apath) {
				continue
			}
			if asset == nil && (c.classifier != "" || c.extension != "") {
				continue
			}
			if !b.add(rp, group, name, version, asset, lm) {
				return nil
			}
			if seen != nil {
				seen[key] = true
			}
		}
	}
	return nil
}

// full reports whether the component cap was hit.
func (b *builder) full() bool { return len(b.byKey) >= componentsLimit }

// sorted returns the components ordered by repo, group, name and version,
// with each component's assets ordered by path.
func (b *builder) sorted() []*Component {
	comps := make([]*Component, 0, len(b.byKey))
	for _, comp := range b.byKey {
		comps = append(comps, comp)
	}
	sort.Slice(comps, func(i, j int) bool {
		a, z := comps[i], comps[j]
		if a.Repo != z.Repo {
			return a.Repo < z.Repo
		}
		if a.Group != z.Group {
			return a.Group < z.Group
		}
		if a.Name != z.Name {
			return a.Name < z.Name
		}
		return a.Version < z.Version
	})
	for _, comp := range comps {
		sort.Slice(comp.Assets, func(i, j int) bool { return comp.Assets[i].Path < comp.Assets[j].Path })
	}
	return comps
}
