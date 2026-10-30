package cleanup

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// DefaultScanBudget is how many assets one evaluation reads per repository
// (plan 11 Decision 5).
const DefaultScanBudget = 20000

// scanPage is how many assets one query reads; a variable so tests can lower it.
var scanPage = 1000

// Candidate is a component a policy selects for deletion. Component.Assets
// lists every asset found for it, and LastUpdated is when its content was
// last written (written).
type Candidate struct {
	component.Component
	Created        time.Time // the oldest asset's creation time
	LastDownloaded time.Time // the newest download of any asset; zero if never downloaded
	Bytes          int64     // the assets' total size
	Reason         string    // why the policy selects it, for previews
}

// Evaluator finds the components a policy selects.
type Evaluator struct {
	DB  *meta.DB
	Now func() time.Time // nil means time.Now
}

type compKey struct{ group, name, version string }

// compAgg accumulates one component's assets during a scan.
type compAgg struct {
	assets        []component.Asset
	bytes         int64
	oldest        time.Time // min created
	newest        time.Time // max created
	modified      time.Time // max last_modified
	written       time.Time // max over assets of the later of created and last_modified (assetWritten)
	lastDl        time.Time // max last_downloaded
	lastUsed      time.Time // max over assets of last_downloaded, or created if never downloaded
	unknownCreate bool      // an asset has no creation time
	unit          string    // a path prefix every asset of the component has (unitPrefix)
	home          string    // a path prefix every asset of every version of the component has (namePrefix)
	rank          int       // with Retain: the version's place, newest first (1-based); 0 when not ranked
	ranked        int       // with Retain: how many versions were ranked
}

// Evaluation is the result of one bounded scan (EvaluateFrom).
type Evaluation struct {
	Candidates []Candidate // sorted by group, name and version
	Bytes      int64       // the candidates' total size
	Scanned    int         // assets read
	Truncated  bool        // the scan stopped at its budget before the end of the repository
	// Next is where the next scan resumes (the from of EvaluateFrom): the
	// start of the first component this scan could not read completely, or
	// the first unread path. "" when the scan reached the end of the
	// repository: the next one starts over.
	Next string
}

// Evaluate reads at most budget assets of rp in path order from the start,
// groups them into components (internal/component derivation) and returns
// the components p selects, sorted by group, name and version, with their
// count and total size. truncated reports that the repository has more
// assets than the budget; components that may have unread assets are then
// left out.
//
// Groups and Cargo hosted repositories are refused with ErrNotSupported, and
// a policy for another format is refused too.
func (e *Evaluator) Evaluate(ctx context.Context, rp *repo.Repository, p Policy, budget int) (candidates []Candidate, count int, bytes int64, truncated bool, err error) {
	ev, err := e.EvaluateFrom(ctx, rp, p, budget, "")
	if err != nil {
		return nil, 0, 0, false, err
	}
	return ev.Candidates, len(ev.Candidates), ev.Bytes, ev.Truncated, nil
}

// EvaluateFrom is Evaluate starting at the path from ("" for the start of
// the repository), so successive scans of a large repository cover it all:
// pass each scan's Next to the next one. Components that may have assets
// before from, or past the point where the budget stopped the scan, are left
// out; a scan that starts at them evaluates them whole. Each candidate
// carries what the scan saw of it (Component.Evaluated), so the deleters
// skip it when it changed meanwhile (CheckUnchanged).
func (e *Evaluator) EvaluateFrom(ctx context.Context, rp *repo.Repository, p Policy, budget int, from string) (Evaluation, error) {
	switch {
	case rp.Type != "hosted" && rp.Type != "proxy":
		return Evaluation{}, fmt.Errorf("%s repository %q: %w", rp.Type, rp.Name, ErrNotSupported)
	case rp.Format == "cargo" && rp.Type == "hosted":
		return Evaluation{}, fmt.Errorf("cargo hosted repository %q (yank crates, or delete versions in the web UI): %w", rp.Name, ErrNotSupported)
	case rp.Format == "docker":
		return Evaluation{}, fmt.Errorf("docker repository %q (delete manifests in the web UI or with the registry API): %w", rp.Name, ErrNotSupported)
	case !FormatMatches(p.Format, rp.Format):
		return Evaluation{}, fmt.Errorf("cleanup policy %q is for %s repositories, not %s repository %q", p.Name, p.Format, rp.Format, rp.Name)
	}
	if err := p.Validate(); err != nil {
		return Evaluation{}, fmt.Errorf("cleanup policy %q: %w", p.Name, err)
	}
	m, err := p.matchers()
	if err != nil {
		return Evaluation{}, err
	}
	if budget <= 0 {
		budget = DefaultScanBudget
	}

	comps := map[compKey]*compAgg{}
	after, scanned, next, truncated := "", 0, "", false
scan:
	for first := true; ; first = false {
		// Read one asset past the budget: it says whether there are more,
		// and where the unread part of the repository starts.
		want := min(scanPage, budget+1-scanned)
		var page []meta.ScanAsset
		var err error
		if first {
			page, err = e.DB.ScanAssetsFrom(ctx, rp.Name, from, want)
		} else {
			page, err = e.DB.ScanAssets(ctx, rp.Name, after, want)
		}
		if err != nil {
			return Evaluation{}, err
		}
		for _, a := range page {
			if scanned == budget {
				truncated, next = true, a.Path
				break scan
			}
			scanned++
			addAsset(comps, rp.Format, a)
		}
		if len(page) < want { // the end of the repository
			break
		}
		after = page[len(page)-1].Path
	}

	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	t := now()
	// Every asset of a component has its unit prefix, and paths sharing a
	// prefix are contiguous in path order: only the components whose unit
	// holds the first unread path, or the start of the scan, may be partial.
	// With Retain the unit is the component's home, which holds all its
	// versions: the version set must be complete to be ranked.
	cutPrefix := func(c *compAgg) string {
		if p.Retain > 0 {
			return c.home
		}
		return c.unit
	}
	partial := func(prefix string) bool {
		return truncated && strings.HasPrefix(next, prefix) || from != "" && from != prefix && strings.HasPrefix(from, prefix)
	}
	res := Evaluation{Scanned: scanned, Truncated: truncated}
	if truncated {
		// Resume at the start of the first component cut by the budget, else
		// of the first unread component; never at or before this scan's
		// start, so scans advance.
		res.Next = next
		if g, n, _, ok := component.DeriveAsset(rp.Format, next); ok {
			res.Next = unitPrefix(rp.Format, next)
			if p.Retain > 0 {
				res.Next = namePrefix(rp.Format, next, g, n)
			}
		}
		for _, c := range comps {
			if u := cutPrefix(c); strings.HasPrefix(next, u) && u < res.Next {
				res.Next = u
			}
		}
		if res.Next <= from {
			res.Next = next
		}
	}
	if p.Retain > 0 {
		rankVersions(rp.Format, p.RetainSortBy, comps)
	}
	for k, c := range comps {
		if partial(cutPrefix(c)) {
			continue
		}
		reason := p.selects(rp.Format, k, c, m, t)
		if reason == "" {
			continue
		}
		paths := make(map[string]bool, len(c.assets))
		for _, a := range c.assets {
			paths[a.Path] = true
		}
		res.Candidates = append(res.Candidates, Candidate{
			Component: component.Component{Repo: rp.Name, Format: rp.Format, Group: k.group, Name: k.name, Version: k.version,
				LastUpdated: c.written, Assets: c.assets,
				Evaluated: &component.Evaluated{Paths: paths, MaxCreated: c.newest, MaxModified: c.modified, LastDownloaded: c.lastDl}},
			Created: c.oldest, LastDownloaded: c.lastDl, Bytes: c.bytes, Reason: reason,
		})
		res.Bytes += c.bytes
	}
	sort.Slice(res.Candidates, func(i, j int) bool {
		a, b := res.Candidates[i], res.Candidates[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Version < b.Version
	})
	return res, nil
}

func addAsset(comps map[compKey]*compAgg, format string, a meta.ScanAsset) {
	group, name, version, ok := component.DeriveAsset(format, a.Path)
	if !ok {
		return
	}
	k := compKey{group, name, version}
	c := comps[k]
	if c == nil {
		c = &compAgg{unit: unitPrefix(format, a.Path), home: namePrefix(format, a.Path, group, name), oldest: a.Created}
		comps[k] = c
	}
	c.assets = append(c.assets, component.Asset{Path: a.Path, Size: a.Size})
	c.bytes += a.Size
	if a.Created.IsZero() {
		c.unknownCreate = true
	} else if c.oldest.IsZero() || a.Created.Before(c.oldest) {
		c.oldest = a.Created
	}
	if a.Created.After(c.newest) {
		c.newest = a.Created
	}
	if a.Modified.After(c.modified) {
		c.modified = a.Modified
	}
	written := assetWritten(a)
	if written.After(c.written) {
		c.written = written
	}
	if a.LastDownloaded.After(c.lastDl) {
		c.lastDl = a.LastDownloaded
	}
	used := a.LastDownloaded
	if used.IsZero() {
		used = written
	}
	if used.After(c.lastUsed) {
		c.lastUsed = used
	}
}

// assetWritten is when a's current content was written, as near as the row
// tells (core-17; Nexus ages by the blob's creation): created_at stays at
// the first upload when the content is replaced, while last_modified moves
// with it. A proxy's last_modified is the remote's Last-Modified, usually
// older than the copy, so the later of the two is taken: an age never
// counts from before the content was first stored here.
func assetWritten(a meta.ScanAsset) time.Time {
	if a.Modified.After(a.Created) {
		return a.Modified
	}
	return a.Created
}

// unitPrefix returns a path prefix that every asset of the component of p
// has: the version directory for Maven, the version's file stem
// (module/@v/version.) for Go, the file itself for npm and Cargo (one file
// per version), and the project directory for PyPI, where the files of one
// version are not contiguous in path order.
func unitPrefix(format, p string) string {
	switch format {
	case "maven":
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i+1]
		}
	case "go":
		if i := strings.LastIndex(p, "."); i > strings.LastIndex(p, "/") {
			return p[:i+1]
		}
	case "npm", "cargo", "helm", "yum":
		return p
	case "nuget": // the version directory, <id>/<version>/ (under v3-flatcontainer/ in a proxy)
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i+1]
		}
	case "composer": // hosted: one file per version; proxy: the version directory
		if strings.Count(p, "/") == 3 {
			return p
		}
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i+1]
		}
	case "conda": // the file stem: a build's .tar.bz2 and .conda are one component
		if stem, ok := strings.CutSuffix(p, ".tar.bz2"); ok {
			return stem + "."
		}
		return strings.TrimSuffix(p, ".conda") + "."
	case "rubygems": // one .gem file per version
		return p
	case "conan": // the recipe revision directory, .../revisions/<rrev>/, holds the recipe files and its packages
		if dir, ok := component.ConanRevisionDir(p); ok {
			return dir
		}
	case "swift": // the version directory, <scope>/<name>/<version>/
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i+1]
		}
	case "cocoapods": // the version directory: the podspec and the archives fetched for it
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i+1]
		}
	case "huggingface": // <repo>/resolve/<sha>/: the files of one commit, in subdirectories too
		if i := strings.Index(p, "/resolve/"); i >= 0 {
			if j := strings.Index(p[i+9:], "/"); j >= 0 {
				return p[:i+9+j+1]
			}
		}
	case "terraform": // a module: one archive per version; a provider: the version directory
		if strings.HasPrefix(p, "modules/") {
			return p
		}
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i+1]
		}
	}
	return homePrefix(format, p)
}

// homePrefix returns the path prefix under which every asset of the
// component's (group, name) is stored: g/a/ for Maven, name/-/ for npm,
// module/@v/ for Go, packages/name/ for PyPI and crates/name/ for Cargo.
func homePrefix(format, p string) string {
	cut := func(sep string) string {
		if i := strings.Index(p, sep); i >= 0 {
			return p[:i+len(sep)]
		}
		return p
	}
	switch format {
	case "maven":
		segs := strings.Split(p, "/")
		if len(segs) >= 3 {
			return strings.Join(segs[:len(segs)-2], "/") + "/"
		}
	case "npm":
		return cut("/-/")
	case "go":
		return cut("/@v/")
	case "pypi", "cargo":
		segs := strings.SplitN(p, "/", 3)
		if len(segs) == 3 {
			return segs[0] + "/" + segs[1] + "/"
		}
	case "nuget": // <id>/, or v3-flatcontainer/<id>/ in a proxy
		if segs := strings.SplitN(p, "/", 3); len(segs) == 3 && segs[0] == "v3-flatcontainer" {
			return segs[0] + "/" + segs[1] + "/"
		}
		return cut("/")
	case "composer": // dist/<vendor>/<package>/
		if segs := strings.SplitN(p, "/", 4); len(segs) == 4 {
			return segs[0] + "/" + segs[1] + "/" + segs[2] + "/"
		}
	case "rubygems": // gems/<name>-: the versions of one gem are contiguous under it
		if name, _, ok := component.SplitGem(strings.TrimPrefix(p, "gems/")); ok {
			return "gems/" + name + "-"
		}
	case "conan": // v2/conans/<name>/
		if segs := strings.SplitN(p, "/", 4); len(segs) == 4 {
			return segs[0] + "/" + segs[1] + "/" + segs[2] + "/"
		}
	case "swift": // <scope>/<name>/
		if segs := strings.SplitN(p, "/", 3); len(segs) == 3 {
			return segs[0] + "/" + segs[1] + "/"
		}
	case "cocoapods": // Specs/<a>/<b>/<c>/<Pod>/
		if segs := strings.SplitN(p, "/", 6); len(segs) == 6 {
			return strings.Join(segs[:5], "/") + "/"
		}
	case "huggingface": // <repo>/resolve/
		return cut("/resolve/")
	case "terraform": // modules/<ns>/<name>/<provider>/ or providers/<ns>/<type>/
		if name, _, ok := component.SplitTerraform(p); ok {
			return strings.SplitN(p, "/", 2)[0] + "/" + name + "/"
		}
	}
	return p
}

// namePrefix returns a path prefix that every asset of every version of
// the component (group, name) of path p has, so the scan can tell a
// complete version set: homePrefix where it is one, the name's file prefix
// for the flat layouts, "pool/" for apt (one package may be in several
// pool components) and "" for formats without one (raw, gitlfs).
func namePrefix(format, p, group, name string) string {
	switch format {
	case "maven", "npm", "go", "pypi", "cargo", "nuget", "composer", "rubygems", "conan":
		return homePrefix(format, p)
	case "helm", "yum": // <name>-<version>..., the yum name directory-qualified
		return name + "-"
	case "conda":
		return group + "/" + name + "-"
	case "r":
		return group + "/" + name + "_"
	case "apt":
		return "pool/"
	}
	return ""
}

// rankVersions sets rank and ranked on every component: its place among
// the versions of its group and name, newest first. Versions that compare
// equal share the place of the first of them.
func rankVersions(format, sortBy string, comps map[compKey]*compAgg) {
	type nameKey struct{ group, name string }
	byName := map[nameKey][]compKey{}
	for k := range comps {
		byName[nameKey{k.group, k.name}] = append(byName[nameKey{k.group, k.name}], k)
	}
	byDate := sortBy == "date" || !HasVersionOrder(format)
	newer := func(a, b compKey) int { // > 0: a is newer
		if byDate {
			return comps[a].written.Compare(comps[b].written) // an unknown time ranks oldest
		}
		return CompareVersions(format, a.version, b.version)
	}
	for _, keys := range byName {
		sort.Slice(keys, func(i, j int) bool {
			if c := newer(keys[i], keys[j]); c != 0 {
				return c > 0
			}
			return keys[i].version < keys[j].version
		})
		rank := 1
		for i, k := range keys {
			if i > 0 && newer(keys[i-1], k) != 0 {
				rank = i + 1
			}
			comps[k].rank, comps[k].ranked = rank, len(keys)
		}
	}
}

// matchers are a policy's compiled patterns; nil when unset.
type matchers struct{ name, path, exName, exVersion *regexp.Regexp }

func (p Policy) matchers() (*matchers, error) {
	var m matchers
	for _, f := range []struct {
		re      **regexp.Regexp
		pattern string
	}{{&m.name, p.NamePattern}, {&m.path, p.PathPattern}, {&m.exName, p.Exclude.NamePattern}, {&m.exVersion, p.Exclude.VersionPattern}} {
		re, err := compilePattern(f.pattern)
		if err != nil {
			return nil, err
		}
		*f.re = re
	}
	return &m, nil
}

// olderThan reports whether t is known and more than days days before now.
func olderThan(t time.Time, days int, now time.Time) bool {
	return !t.IsZero() && now.Sub(t) > time.Duration(days)*24*time.Hour
}

// selects applies the criteria to one component and says why it is
// selected; "" when it is not. A component with an asset of unknown
// creation time has no known age, so the ages never select it.
func (p Policy) selects(format string, k compKey, c *compAgg, m *matchers, now time.Time) string {
	if c.unknownCreate {
		return ""
	}
	subject := k.name
	if k.group != "" {
		subject = k.group + ":" + k.name
	}
	if m.exName != nil && m.exName.MatchString(subject) || m.exVersion != nil && m.exVersion.MatchString(k.version) {
		return ""
	}
	var why []string
	if p.Retain > 0 {
		if c.rank <= p.Retain {
			return ""
		}
		why = append(why, fmt.Sprintf("version %d of %d by %s, retain %d", c.rank, c.ranked, p.RetainSortBy, p.Retain))
	}
	if p.ComponentAgeDays > 0 {
		if !olderThan(c.written, p.ComponentAgeDays, now) {
			return ""
		}
		why = append(why, fmt.Sprintf("updated %d days ago (componentAgeDays %d)", int(now.Sub(c.written)/(24*time.Hour)), p.ComponentAgeDays))
	}
	if p.LastDownloadedDays > 0 {
		if !olderThan(c.lastUsed, p.LastDownloadedDays, now) || p.DownloadedOnly && c.lastDl.IsZero() {
			return ""
		}
		why = append(why, fmt.Sprintf("not downloaded for %d days (lastDownloadedDays %d)", int(now.Sub(c.lastUsed)/(24*time.Hour)), p.LastDownloadedDays))
	}
	switch p.ReleaseType {
	case "release":
		if Prerelease(format, k.version) {
			return ""
		}
		why = append(why, "release")
	case "prerelease":
		if !Prerelease(format, k.version) {
			return ""
		}
		why = append(why, "prerelease")
	}
	if m.name != nil {
		if !m.name.MatchString(subject) {
			return ""
		}
		why = append(why, "name matches namePattern")
	}
	if m.path != nil {
		if !slices.ContainsFunc(c.assets, func(a component.Asset) bool { return m.path.MatchString(a.Path) || m.path.MatchString("/"+a.Path) }) {
			return ""
		}
		why = append(why, "an asset path matches pathPattern")
	}
	return strings.Join(why, "; ")
}
