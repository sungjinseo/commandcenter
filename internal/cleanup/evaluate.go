package cleanup

import (
	"context"
	"fmt"
	"regexp"
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
// lists every asset found for it, and LastUpdated is its newest asset's
// creation time.
type Candidate struct {
	component.Component
	Created        time.Time // the oldest asset's creation time
	LastDownloaded time.Time // the newest download of any asset; zero if never downloaded
	Bytes          int64     // the assets' total size
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
	lastDl        time.Time // max last_downloaded
	lastUsed      time.Time // max over assets of last_downloaded, or created if never downloaded
	unknownCreate bool      // an asset has no creation time
	unit          string    // a path prefix every asset of the component has (unitPrefix)
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
	pattern, err := compilePattern(p.NamePattern)
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
	res := Evaluation{Scanned: scanned, Truncated: truncated}
	if truncated {
		// Resume at the start of the first component cut by the budget, else
		// of the first unread component; never at or before this scan's
		// start, so scans advance.
		res.Next = next
		if _, _, _, ok := component.DeriveAsset(rp.Format, next); ok {
			res.Next = unitPrefix(rp.Format, next)
		}
		for _, c := range comps {
			if strings.HasPrefix(next, c.unit) && c.unit < res.Next {
				res.Next = c.unit
			}
		}
		if res.Next <= from {
			res.Next = next
		}
	}
	for k, c := range comps {
		// Every asset of a component has its unit prefix, and paths sharing
		// a prefix are contiguous in path order: only the components whose
		// unit holds the first unread path, or the start of the scan, may be
		// partial.
		if truncated && strings.HasPrefix(next, c.unit) {
			continue
		}
		if from != "" && from != c.unit && strings.HasPrefix(from, c.unit) {
			continue
		}
		if !p.selects(rp.Format, k, c, pattern, t) {
			continue
		}
		paths := make(map[string]bool, len(c.assets))
		for _, a := range c.assets {
			paths[a.Path] = true
		}
		res.Candidates = append(res.Candidates, Candidate{
			Component: component.Component{Repo: rp.Name, Format: rp.Format, Group: k.group, Name: k.name, Version: k.version,
				LastUpdated: c.newest, Assets: c.assets,
				Evaluated: &component.Evaluated{Paths: paths, MaxCreated: c.newest, MaxModified: c.modified, LastDownloaded: c.lastDl}},
			Created: c.oldest, LastDownloaded: c.lastDl, Bytes: c.bytes,
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
		c = &compAgg{unit: unitPrefix(format, a.Path), oldest: a.Created}
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
	if a.LastDownloaded.After(c.lastDl) {
		c.lastDl = a.LastDownloaded
	}
	used := a.LastDownloaded
	if used.IsZero() {
		used = a.Created
	}
	if used.After(c.lastUsed) {
		c.lastUsed = used
	}
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
	}
	return p
}

// olderThan reports whether t is known and more than days days before now.
func olderThan(t time.Time, days int, now time.Time) bool {
	return !t.IsZero() && now.Sub(t) > time.Duration(days)*24*time.Hour
}

// selects applies the criteria to one component. A component with an asset
// of unknown creation time has no known age, so the ages never select it.
func (p Policy) selects(format string, k compKey, c *compAgg, pattern *regexp.Regexp, now time.Time) bool {
	if c.unknownCreate {
		return false
	}
	if p.ComponentAgeDays > 0 && !olderThan(c.newest, p.ComponentAgeDays, now) {
		return false
	}
	if p.LastDownloadedDays > 0 && !olderThan(c.lastUsed, p.LastDownloadedDays, now) {
		return false
	}
	switch p.ReleaseType {
	case "release":
		if Prerelease(format, k.version) {
			return false
		}
	case "prerelease":
		if !Prerelease(format, k.version) {
			return false
		}
	}
	if pattern != nil {
		subject := k.name
		if k.group != "" {
			subject = k.group + ":" + k.name
		}
		if !pattern.MatchString(subject) {
			return false
		}
	}
	return true
}
