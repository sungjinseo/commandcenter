package nexusapi

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/format/golang"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/format/npm"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
	"commandcenter/internal/vercmp"
	"commandcenter/internal/vuln"
)

// ---- what one caller may list ----

// view is what the caller of one request may list through this API, by
// Nexus's rules: the content of a repository it may browse or read, itself
// or through a group that contains it; where only content-selector grants
// give access, just the paths their expressions match.
type view struct {
	h      *Handler
	p      auth.Principal
	within map[string][]string // repository: the groups that contain it, nested ones included
}

func (h *Handler) viewOf(r *http.Request) *view { return &view{h: h, p: principal(r)} }

// groupsOf returns the groups that contain name, directly or through
// nested groups (offline members included: this is about grants).
func (v *view) groupsOf(name string) []string {
	if v.within == nil {
		v.within = map[string][]string{}
		for _, g := range v.h.Repos.Snapshot().Sorted {
			if g.Type != "group" {
				continue
			}
			seen := map[string]bool{g.Name: true}
			var walk func(*repo.Repository)
			walk = func(r *repo.Repository) {
				for _, m := range r.MemberRepos {
					if m != nil && !seen[m.Name] {
						seen[m.Name] = true
						v.within[m.Name] = append(v.within[m.Name], g.Name)
						walk(m)
					}
				}
			}
			walk(g)
		}
	}
	return v.within[name]
}

// can reports whether the caller may list the content of the named repository.
func (v *view) can(name string) bool {
	return v.h.Auth.Can(v.p, name, auth.Browse) ||
		slices.ContainsFunc(v.groupsOf(name), func(g string) bool { return v.h.Auth.Can(v.p, g, auth.Browse) })
}

// selectorPath is the path a content selector sees for file p of rp: the
// path itself, a docker file's image name (as the registry checks it).
func selectorPath(rp *repo.Repository, p string) string {
	if rp.Format == "docker" {
		for _, sep := range []string{"/manifests/", "/blobs/"} {
			if i := strings.LastIndex(p, sep); i >= 0 {
				return p[:i]
			}
		}
	}
	return p
}

// all reports whether the caller may list every file of rp: its access,
// direct or through a group, does not come from content selectors alone.
func (v *view) all(rp *repo.Repository) bool {
	return v.h.Auth.CanAll(v.p, rp.Name, auth.Browse) ||
		slices.ContainsFunc(v.groupsOf(rp.Name), func(g string) bool { return v.h.Auth.CanAll(v.p, g, auth.Browse) })
}

// sees reports whether the caller may list file p of rp: a selector grant
// counts only where its expression matches the path.
func (v *view) sees(rp *repo.Repository, p string) bool {
	p = selectorPath(rp, p)
	return v.h.Auth.CanPath(v.p, rp.Name, auth.Browse, p) ||
		slices.ContainsFunc(v.groupsOf(rp.Name), func(g string) bool { return v.h.Auth.CanPath(v.p, g, auth.Browse, p) })
}

// searchRepos resolves a search's repository parameter: names separated
// by spaces, with "*" and "?" wildcards, each group expanded to its leaf
// members. It returns the leaves to search and, for each, the repository
// whose URL its downloads use: itself when it was named (or no name was
// given), else the first group it was named through. ok is false when
// names were given and none is a repository the caller may list.
func (v *view) searchRepos(param string) (out []*repo.Repository, via map[string]string, ok bool) {
	via = map[string]string{}
	snap := v.h.Repos.Snapshot()
	names := strings.Fields(param)
	if len(names) == 0 {
		for _, rp := range snap.Sorted {
			if rp.Type != "group" && v.can(rp.Name) {
				out = append(out, rp)
				via[rp.Name] = rp.Name
			}
		}
		return out, via, true
	}
	var named []*repo.Repository
	for _, n := range names {
		if !strings.ContainsAny(n, "*?") {
			if rp := snap.ByName[n]; rp != nil && v.can(n) {
				named = append(named, rp)
			}
			continue
		}
		re := wildcard(n, true, false)
		for _, rp := range snap.Sorted {
			if re.MatchString(rp.Name) && v.can(rp.Name) {
				named = append(named, rp)
			}
		}
	}
	for _, rp := range named {
		if rp.Type != "group" {
			via[rp.Name] = rp.Name
		}
	}
	for _, rp := range named {
		for _, l := range resolve.Leaves(rp) {
			if _, dup := via[l.Name]; !dup {
				via[l.Name] = rp.Name
			}
		}
	}
	for _, rp := range snap.Sorted {
		if _, ok := via[rp.Name]; ok {
			out = append(out, rp)
		}
	}
	return out, via, len(named) > 0
}

// ---- parameters ----

// wildcard compiles a search value: case-insensitive, "*" any run of
// characters, "?" one character (one) or any run (search values, where
// Nexus treats both as wildcards); anchored at both ends or a substring.
func wildcard(s string, one, anchored bool) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?is)")
	if anchored {
		b.WriteString("^")
	}
	for _, r := range s {
		switch {
		case r == '*', r == '?' && !one:
			b.WriteString(".*")
		case r == '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if anchored {
		b.WriteString("$")
	}
	return regexp.MustCompile(b.String())
}

// splitValue splits a search value into its alternatives: on spaces
// outside double quotes, which keep a phrase together.
func splitValue(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// term is a parameter's alternatives, any of which may match.
type term []*regexp.Regexp

func newTerm(alts []string, anchored bool) term {
	t := make(term, len(alts))
	for i, a := range alts {
		t[i] = wildcard(a, false, anchored)
	}
	return t
}

// matches reports whether any alternative matches one of values; with no
// alternative (a parameter given empty) only an empty value matches.
func (t term) matches(values ...string) bool {
	if len(t) == 0 {
		return values[0] == ""
	}
	for _, re := range t {
		for _, v := range values {
			if re.MatchString(v) {
				return true
			}
		}
	}
	return false
}

// Search parameter names: Nexus's aliases first, then its full attribute
// names (and the Command Center names kept from earlier releases).
var (
	qKeys          = []string{"q", "keyword"}
	groupKeys      = []string{"group", "group.raw", "maven.groupId", "attributes.maven2.groupId", "npm.scope"}
	nameKeys       = []string{"name", "name.raw", "maven.artifactId", "attributes.maven2.artifactId", "docker.imageName"}
	versionKeys    = []string{"version", "docker.imageTag"}
	baseKeys       = []string{"maven.baseVersion", "attributes.maven2.baseVersion"}
	extensionKeys  = []string{"maven.extension", "assets.attributes.maven2.extension"}
	classifierKeys = []string{"maven.classifier", "assets.attributes.maven2.classifier"}
	prereleaseKeys = []string{"prerelease", "isPrerelease"}
	updatedKeys    = []string{"last_updated", "lastBlobUpdated", "last_modified"}
	sumNames       = []string{"md5", "sha1", "sha256", "sha512"}
	pypiAttrs      = []string{"classifiers", "description", "keywords", "summary"}
	sortFields     = map[string]bool{"group": true, "name": true, "version": true, "repository": true, "format": true}
)

// gavRE is q's dependency form group:name[:version[:extension[:classifier]]].
var gavRE = regexp.MustCompile(`^([^\s:]+):([^\s:]+)(?::([^\s:]+))?(?::([^\s:]+))?(?::([^\s:]+))?$`)

// criteria are a search's parameters, as tests on a component and on each
// of its files. Parameters Command Center does not know are ignored, as
// Nexus ignores them.
type criteria struct {
	npm    map[string]term                // assets.attributes.npm.<field>: of a hosted package's manifest
	comp   []func(c *comp) bool           // all must hold, on any one of the component's files
	file   func(c *comp, p string) bool   // some file of the component must pass (q and its dependency form)
	asset  []func(c *comp, p string) bool // asset parameters: they cut a component's files
	sums   map[string]string              // asset checksums (md5, sha1, sha256, sha512), matched in the database
	pypi   map[string]term                // the pypi.* parameters, by their last part: matched in a file's core metadata
	since  time.Time                      // some file modified at or after it
	format string                         // Command Center spelling; "" any
	sort   string
	desc   bool
}

// parseCriteria reads the search parameters of q. A component parameter
// given empty is ignored; an asset parameter given empty (maven.classifier=)
// keeps only the files without that attribute. Bad values are errors in
// writeValidation's "field: message" form.
func parseCriteria(q url.Values) (criteria, error) {
	var cr criteria
	// value is the first of keys that is present, and whether one is.
	value := func(keys []string) (string, bool) {
		for _, k := range keys {
			if q.Has(k) {
				return strings.TrimSpace(q.Get(k)), true
			}
		}
		return "", false
	}
	compTerm := func(keys []string, get func(*comp) []string) {
		v, _ := value(keys)
		if alts := splitValue(v); len(alts) > 0 {
			t := newTerm(alts, true)
			cr.comp = append(cr.comp, func(c *comp) bool { return t.matches(get(c)...) })
		}
	}
	assetTerm := func(keys []string, get func(c *comp, p string) string) {
		if v, given := value(keys); given {
			t := newTerm(splitValue(v), true)
			cr.asset = append(cr.asset, func(c *comp, p string) bool { return t.matches(get(c, p)) })
		}
	}
	compTerm(groupKeys, groupForms)
	compTerm(nameKeys, nameForms)
	compTerm(versionKeys, versionForms)
	compTerm(baseKeys, func(c *comp) []string { return []string{baseVersion(c)} })
	assetTerm(extensionKeys, func(c *comp, p string) string { _, ext := fileParts(c, p); return ext })
	assetTerm(classifierKeys, func(c *comp, p string) string { cl, _ := fileParts(c, p); return cl })

	if v, _ := value(qKeys); v != "" {
		if m := gavRE.FindStringSubmatch(v); m != nil {
			cr.gav(m)
		} else if alts := splitValue(v); len(alts) > 0 {
			t := newTerm(alts, false)
			cr.file = func(c *comp, p string) bool { return t.matches(c.group, c.name, c.version, p) }
		}
	}
	for _, s := range sumNames {
		if v, given := value([]string{s, "assets.attributes.checksum." + s}); given {
			if cr.sums == nil {
				cr.sums = map[string]string{}
			}
			cr.sums[s] = strings.ToLower(v)
		}
	}
	for _, a := range pypiAttrs {
		if alts := splitValue(strings.TrimSpace(q.Get("pypi." + a))); len(alts) > 0 {
			if cr.pypi == nil {
				cr.pypi = map[string]term{}
			}
			cr.pypi[a] = newTerm(alts, false)
		}
	}
	for k := range q {
		if field, ok := strings.CutPrefix(k, "assets.attributes.npm."); ok {
			if cr.npm == nil {
				cr.npm = map[string]term{}
			}
			cr.npm[field] = newTerm(splitValue(strings.TrimSpace(q.Get(k))), false)
		}
	}
	if v, _ := value(prereleaseKeys); v != "" {
		want, err := parseBool(v)
		if err != nil {
			return cr, fmt.Errorf("prerelease: %v", err)
		}
		cr.comp = append(cr.comp, func(c *comp) bool { return cleanup.Prerelease(c.format, baseVersion(c)) == want })
	}
	if v, _ := value(updatedKeys); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			if t, err = time.Parse(time.DateOnly, v); err != nil {
				return cr, errors.New("last_updated: give an RFC 3339 time or a date (YYYY-MM-DD): the components with a file modified since then")
			}
		}
		cr.since = t
	}
	cr.format = ccFormat(q.Get("format"))
	if cr.format == "" {
		cr.format = impliedFormat(q)
	}
	cr.sort = strings.TrimSpace(q.Get("sort"))
	if cr.sort != "" && !sortFields[cr.sort] {
		return cr, fmt.Errorf("sort: unknown sort field %q: use group, name, version, repository or format", cr.sort)
	}
	switch d := strings.ToLower(strings.TrimSpace(q.Get("direction"))); d {
	case "":
		cr.desc = cr.sort == "version" // Nexus: newest first
	case "asc", "desc":
		cr.desc = d == "desc"
	default:
		return cr, fmt.Errorf("direction: unknown direction %q: use asc or desc", d)
	}
	return cr, nil
}

// gav adds q's dependency form: exact group, name and version, and a
// component with a file of that extension and classifier (Nexus matches
// these on the component and leaves its files uncut).
func (cr *criteria) gav(m []string) {
	parts := []struct {
		v   string
		get func(*comp) []string
	}{{m[1], groupForms}, {m[2], nameForms}, {m[3], versionForms}}
	for _, part := range parts {
		if part.v != "" {
			t, get := literal(part.v), part.get
			cr.comp = append(cr.comp, func(c *comp) bool { return t.matches(get(c)...) })
		}
	}
	if m[4] != "" || m[5] != "" {
		ext, cl := literal(m[4]), literal(m[5])
		cr.file = func(c *comp, p string) bool {
			fc, fe := fileParts(c, p)
			return (m[4] == "" || ext.matches(fe)) && (m[5] == "" || cl.matches(fc))
		}
	}
}

// literal is a term matching s exactly but for case.
func literal(s string) term {
	return term{regexp.MustCompile("(?is)^" + regexp.QuoteMeta(s) + "$")}
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("%q is neither true nor false", s)
}

// impliedFormat is the format a format-qualified parameter (maven.*,
// npm.*, docker.*) implies when format is absent, as Nexus adds it.
func impliedFormat(q url.Values) string {
	for _, f := range []struct{ format, prefix string }{{"maven", "maven."}, {"maven", "attributes.maven2."},
		{"maven", "assets.attributes.maven2."}, {"npm", "npm."}, {"npm", "assets.attributes.npm."}, {"docker", "docker."}, {"pypi", "pypi."}, {"p2", "p2."}, {"p2", "attributes.p2."}} {
		for k := range q {
			if strings.HasPrefix(k, f.prefix) {
				return f.format
			}
		}
	}
	return ""
}

// groupForms are the spellings a group parameter may use: Nexus's (an npm
// scope without "@", a raw file's directory with a leading "/"), the npm
// scope with "@" and the directory without "/".
func groupForms(c *comp) []string {
	g := compGroup(c)
	switch c.format {
	case "npm":
		return []string{g, "@" + g}
	case "raw":
		return []string{g, strings.TrimPrefix(g, "/")}
	case "p2":
		return []string{c.group}
	}
	return []string{g}
}

// nameForms: a raw file's name with and without the leading "/", a PyPI
// project's normalized name with each of the separators PEP 503 folds, as
// Nexus indexes it, a Go module's path as it is and case-escaped, as Nexus
// names it.
func nameForms(c *comp) []string {
	switch c.format {
	case "raw":
		return []string{"/" + c.name, c.name}
	case "pypi":
		return []string{c.name, strings.ReplaceAll(c.name, "-", "_"), strings.ReplaceAll(c.name, "-", ".")}
	case "go":
		return []string{c.name, golang.Escape(c.name)}
	}
	return []string{c.name}
}

// versionForms: a Go version as it is and case-escaped.
func versionForms(c *comp) []string {
	if c.format == "go" {
		return []string{c.version, golang.Escape(c.version)}
	}
	return []string{c.version}
}

// compGroup is the group Nexus reports: an npm scope without "@", a raw
// file's directory ("/" at the top level), none for p2.
func compGroup(c *comp) string {
	switch c.format {
	case "raw":
		return "/" + strings.TrimSuffix(path.Dir(c.name), ".")
	case "npm":
		return strings.TrimPrefix(c.group, "@")
	case "p2":
		return ""
	}
	return c.group
}

// baseVersion is Maven's base version, the version directory of the
// component's files (1.0-SNAPSHOT for its timestamped builds); other
// formats have the version.
func baseVersion(c *comp) string {
	if c.format == "maven" && len(c.paths) > 0 {
		return path.Base(path.Dir(c.paths[0]))
	}
	return c.version
}

// fileParts is the classifier and extension of a component's file: Maven's
// coordinates, the file name's extension for other formats.
func fileParts(c *comp, p string) (classifier, ext string) {
	if c.format == "maven" {
		if mp, err := maven.ParsePath(p); err == nil && mp.Coords != nil {
			return mp.Coords.Classifier, mp.Coords.Extension
		}
	}
	return "", strings.TrimPrefix(path.Ext(p), ".")
}

// compare orders components by the sort field (Nexus's default direction:
// descending for version), then by repository and key, a total order: a
// continuation token, the last item's id, finds its place even after that
// item was deleted.
func (cr criteria) compare(a, b *comp) int {
	c := 0
	switch cr.sort {
	case "group":
		c = strings.Compare(compGroup(a), compGroup(b))
	case "name":
		c = strings.Compare(a.name, b.name)
	case "version":
		if a.format == "maven" && b.format == "maven" { // SNAPSHOT builds by timestamp and build number too
			c = maven.CompareVersions(a.version, b.version)
			break
		}
		eco := "" // vercmp's generic order across formats
		if a.format == b.format {
			eco = vuln.Ecosystems[a.format]
		}
		c = vercmp.Compare(eco, a.version, b.version)
	case "repository":
		c = strings.Compare(a.repo, b.repo)
	case "format":
		c = strings.Compare(a.format, b.format)
	}
	if cr.desc {
		c = -c
	}
	return cmp.Or(c, strings.Compare(a.repo, b.repo), strings.Compare(a.key(), b.key()))
}

// tokenComp decodes a continuation token that is a component id, with
// the format of its repository (for the sort).
func (h *Handler) tokenComp(id string) (comp, bool) {
	c, ok := parseCompID(id)
	if rp := h.Repos.Get(c.repo); ok && rp != nil {
		c.format = rp.Format
	}
	return c, ok
}

// after returns the items that follow token, the id of a previous page's
// last item, in items sorted by cr.compare, whether that item still exists
// or not; ok is false for a token that is no id.
func (h *Handler) after(items []*comp, cr criteria, token string) ([]*comp, bool) {
	if token == "" {
		return items, true
	}
	tok, ok := h.tokenComp(token)
	if !ok {
		return nil, false
	}
	return items[sort.Search(len(items), func(i int) bool { return cr.compare(items[i], &tok) > 0 }):], true
}

// ---- search ----

// searched runs the request's search: the matching components of the
// repositories it selects that the caller may list, in the requested
// order, each with the files the caller sees and its download repository.
func (h *Handler) searched(w http.ResponseWriter, r *http.Request, win window) ([]*comp, criteria, bool) {
	q := r.URL.Query()
	p := principal(r)
	cr, err := parseCriteria(q)
	if err != nil {
		writeValidation(w, []string{err.Error()})
		return nil, cr, false
	}
	tooMany := h.filterP2Plugins(r.Context(), q, &cr)
	v := h.viewOf(r)
	names := strings.Join(q["repository"], " ")
	repos, via, ok := v.searchRepos(names)
	switch {
	case p.Anonymous() && (!ok || strings.TrimSpace(names) == "" && len(repos) == 0):
		deny(w, p)
		return nil, cr, false
	case !ok:
		writeMessage(w, http.StatusNotFound, "repository not found")
		return nil, cr, false
	}
	comps, err := h.find(r.Context(), repos, cr, q.Get("tag"), win, v.sees)
	if err != nil {
		h.fail(w, err)
		return nil, cr, false
	}
	if *tooMany {
		writeMessage(w, http.StatusBadRequest, fmt.Sprintf("p2.pluginName reads the bundles it searches, and more than %d qualify: narrow the search with repository or name", maxP2Plugins))
		return nil, cr, false
	}
	for _, c := range comps {
		c.via = via[c.repo]
	}
	return comps, cr, true
}

// window is how much of a search a request can use, which is what a search
// keeps in memory however many components the repositories hold: the first
// limit components (0: all of them) in the search's order after after, and
// after itself with atToken. prefix narrows the scan to the paths under it.
type window struct {
	limit   int
	after   *comp
	atToken bool
	prefix  string
}

// pageWindow is the window of a page of components after token.
func (h *Handler) pageWindow(token string) window {
	win := window{limit: pageSize + 1}
	if tok, ok := h.tokenComp(token); token != "" && ok {
		win.after = &tok
	}
	return win
}

// held is a component a search has seen and may still answer with.
type held struct {
	*comp
	file, recent bool // a file passed cr.file, a file was modified since cr.since
	picked       bool
}

// collector picks a search's result from a repository's files, in any order.
// A component qualifies once its files passed every test; among those, the
// best limit stay. Whatever ranks behind them can never come back, so it is
// dropped at once: memory follows the page, not the repository.
type collector struct {
	cr     criteria
	win    window
	keep   func(*repo.Repository, string) bool // the caller sees this file; nil: all
	tagged map[meta.ComponentKey]bool
	rp     *repo.Repository
	sums   map[string]bool
	recent map[string]bool
	open   map[string]*held
	best   []*held // qualified, sorted by cr once a limit applies
}

func (s *collector) add(group, name, version, p string) {
	c := comp{repo: s.rp.Name, format: s.rp.Format, group: group, name: name, version: version, paths: []string{p}}
	k := c.key()
	e := s.open[k]
	if e == nil && !s.wanted(&c) {
		return
	}
	// A file the caller may not list decides nothing, not even whether q
	// finds the component.
	if s.keep != nil && !s.keep(s.rp, p) {
		return
	}
	file := s.cr.file != nil && s.cr.file(&c, p)
	recent := s.recent != nil && s.recent[p]
	pass := (s.sums == nil || s.sums[p]) &&
		!slices.ContainsFunc(s.cr.asset, func(f func(*comp, string) bool) bool { return !f(&c, p) })
	if e == nil {
		if !file && !recent && !pass { // nothing to remember about this file
			return
		}
		x := c
		x.paths = nil
		e = &held{comp: &x, file: s.cr.file == nil, recent: s.recent == nil}
		s.open[k] = e
	}
	e.file, e.recent = e.file || file, e.recent || recent
	if pass {
		e.paths = append(e.paths, p)
	}
	if !e.picked && e.file && e.recent && len(e.paths) > 0 {
		s.pick(e)
	}
}

// wanted reports whether a component not held yet can be in the result.
func (s *collector) wanted(c *comp) bool {
	if a := s.win.after; a != nil {
		if d := s.cr.compare(c, a); d < 0 || (d == 0 && !s.win.atToken) {
			return false
		}
	}
	if n := len(s.best); s.win.limit > 0 && n == s.win.limit && s.cr.compare(c, s.best[n-1].comp) > 0 {
		return false
	}
	if s.tagged != nil && !s.tagged[c.metaKey()] {
		return false
	}
	return !slices.ContainsFunc(s.cr.comp, func(f func(*comp) bool) bool { return !f(c) })
}

func (s *collector) pick(e *held) {
	e.picked = true
	if s.win.limit == 0 {
		s.best = append(s.best, e)
		return
	}
	i, _ := slices.BinarySearchFunc(s.best, e, func(a, b *held) int { return s.cr.compare(a.comp, b.comp) })
	s.best = slices.Insert(s.best, i, e)
	if n := len(s.best); n > s.win.limit {
		delete(s.open, s.best[n-1].key())
		s.best = s.best[:n-1]
	}
}

// searchSem bounds the repository scans running at once: each is cheap to
// hold but streams every path, and 128 at once outrun the garbage collector
// in the 128 MiB a default install has (measured on 300 000 files).
var searchSem = make(chan struct{}, 4)

// find runs a search in repos (groups skipped): the components in win that
// match cr and the tag, in cr's order, with the files that pass and keep.
func (h *Handler) find(ctx context.Context, repos []*repo.Repository, cr criteria, tag string, win window, keep func(*repo.Repository, string) bool) ([]*comp, error) {
	tagged, err := h.tagged(ctx, tag)
	if err != nil {
		return nil, err
	}
	select {
	case searchSem <- struct{}{}:
		defer func() { <-searchSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if len(cr.pypi) > 0 {
		sees := keep
		keep = func(rp *repo.Repository, p string) bool {
			return (sees == nil || sees(rp, p)) && h.pypiMatches(ctx, rp, p, cr.pypi)
		}
	}
	s := &collector{cr: cr, win: win, keep: keep, tagged: tagged, open: map[string]*held{}}
	for _, rp := range repos {
		if rp.Type == "group" || (cr.format != "" && rp.Format != cr.format) || (tagged != nil && !tagged[meta.ComponentKey{Repo: rp.Name}]) {
			continue
		}
		s.rp, s.sums, s.recent = rp, nil, nil
		if cr.sums != nil {
			if s.sums, err = h.DB.AssetPathsWhere(ctx, rp.Name, cr.sums, time.Time{}); err != nil {
				return nil, err
			}
		}
		if !cr.since.IsZero() {
			if s.recent, err = h.DB.AssetPathsWhere(ctx, rp.Name, nil, cr.since); err != nil {
				return nil, err
			}
		}
		if cr.npm != nil {
			paths, err := h.npmManifestPaths(ctx, rp, cr.npm)
			if err != nil {
				return nil, err
			}
			for p := range s.sums {
				if !paths[p] {
					delete(s.sums, p)
				}
			}
			if s.sums == nil {
				s.sums = paths
			}
		}
		if (s.sums != nil && len(s.sums) == 0) || (s.recent != nil && len(s.recent) == 0) {
			continue
		}
		if err := h.walkComponents(ctx, rp, win.prefix, s.add); err != nil {
			return nil, err
		}
	}
	if win.limit == 0 {
		slices.SortFunc(s.best, func(a, b *held) int { return cr.compare(a.comp, b.comp) })
	}
	out := make([]*comp, len(s.best))
	for i, e := range s.best {
		if e.format == "maven" { // the main file first, so search/assets/download finds it before a classified one
			slices.SortStableFunc(e.paths, func(a, b string) int {
				ca, _, _ := component.MavenFileParts(path.Base(a), e.name, e.version)
				cb, _, _ := component.MavenFileParts(path.Base(b), e.name, e.version)
				return cmpBool(ca != "", cb != "")
			})
		}
		out[i] = e.comp
	}
	return out, nil
}

// pypiMatches reports whether the core metadata stored beside file p of rp
// (a signature: beside its file) matches every term of a search's pypi.*
// parameters. A file without it matches none. ponytail: reads a file per file
// of the repository, only for a search with these parameters; keep the fields
// in the row if such searches get frequent.
func (h *Handler) pypiMatches(ctx context.Context, rp *repo.Repository, p string, terms map[string]term) bool {
	if rp.Format != "pypi" || h.Store == nil {
		return false
	}
	a, err := h.DB.GetAsset(ctx, rp.Name, strings.TrimSuffix(p, ".asc")+".metadata")
	if err != nil {
		return false
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, a.Size))
	if err != nil {
		return false
	}
	attrs := pypi.SearchAttributes(raw)
	for name, t := range terms {
		if !t.matches(attrs[name]...) {
			return false
		}
	}
	return true
}

// npmManifestPaths returns the tarballs of the versions of hosted rp whose
// manifests match every term (npm.ManifestField; a field it does not know
// matches nothing). A proxy keeps no manifests per version, so it has none.
// ponytail: reads every version's row per search, for a parameter scripts
// rarely use; attributes per asset if it comes to matter.
func (h *Handler) npmManifestPaths(ctx context.Context, rp *repo.Repository, terms map[string]term) (map[string]bool, error) {
	paths := map[string]bool{}
	names, err := h.DB.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		rows, err := h.DB.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			ok := true
			for field, t := range terms {
				vals := npm.ManifestField(row.Attrs, field)
				ok = ok && len(vals) > 0 && t.matches(vals...)
			}
			if ok {
				paths[npm.TarballKey(name, npm.TarballFile(name, row.Version))] = true
			}
		}
	}
	return paths, nil
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	comps, cr, ok := h.searched(w, r, h.pageWindow(r.URL.Query().Get("continuationToken")))
	if !ok {
		return
	}
	h.writeComponentPage(w, r, comps, cr, r.URL.Query().Get("continuationToken"))
}

// hit is one file of a search result.
type hit struct {
	c *comp
	p string
}

// hitToken is a file's continuation token: its component's id and its path.
func hitToken(x hit) string {
	return compID(x.c) + "." + base64.RawURLEncoding.EncodeToString([]byte(x.p))
}

// hits flattens the components' files, in component then path order, after
// token (a previous page's hitToken); ok is false for a bad token.
func (h *Handler) hits(comps []*comp, cr criteria, token string) ([]hit, bool) {
	var out []hit
	for _, c := range comps {
		for _, p := range c.paths {
			out = append(out, hit{c, p})
		}
	}
	if token == "" {
		return out, true
	}
	cid, enc, _ := strings.Cut(token, ".")
	tc, ok := h.tokenComp(cid)
	tp, err := base64.RawURLEncoding.DecodeString(enc)
	if !ok || err != nil || len(tp) == 0 {
		return nil, false
	}
	return out[sort.Search(len(out), func(i int) bool {
		return cmp.Or(cr.compare(out[i].c, &tc), strings.Compare(out[i].p, string(tp))) > 0
	}):], true
}

// asset reads a hit's row; ok is false when the file is gone meanwhile.
func (h *Handler) asset(ctx context.Context, x hit) (meta.Asset, bool, error) {
	a, err := h.DB.GetAsset(ctx, x.c.repo, x.p)
	if errors.Is(err, meta.ErrNotFound) {
		return a, false, nil
	}
	return a, err == nil, err
}

func (h *Handler) searchAssets(w http.ResponseWriter, r *http.Request) {
	// The token's own component may have no file left to give, the others
	// at least one each, and one more tells that another page follows.
	cid, _, _ := strings.Cut(r.URL.Query().Get("continuationToken"), ".")
	win := h.pageWindow(cid)
	win.limit, win.atToken = pageSize+2, true
	comps, cr, ok := h.searched(w, r, win)
	if !ok {
		return
	}
	hits, ok := h.hits(comps, cr, r.URL.Query().Get("continuationToken"))
	if !ok {
		writeMessage(w, http.StatusBadRequest, "invalid continuationToken")
		return
	}
	next := ""
	if len(hits) > pageSize {
		hits = hits[:pageSize]
		next = hitToken(hits[pageSize-1])
	}
	items := make([]map[string]any, 0, len(hits))
	for _, x := range hits {
		a, found, err := h.asset(r.Context(), x)
		if err != nil {
			h.fail(w, err)
			return
		}
		if found {
			items = append(items, h.assetJSON(h.Repos.Get(x.c.repo), x.c.via, a))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "continuationToken": nullable(next)})
}

// searchDownload redirects to the one matching asset; several match only
// with a sort, which picks the first (sort=version&direction=desc: the
// newest), as Nexus does.
func (h *Handler) searchDownload(w http.ResponseWriter, r *http.Request) {
	comps, cr, ok := h.searched(w, r, window{limit: pageSize})
	if !ok {
		return
	}
	hits, _ := h.hits(comps, cr, "")
	var found []hit
	for _, x := range hits {
		if _, ok, err := h.asset(r.Context(), x); err != nil {
			h.fail(w, err)
			return
		} else if ok {
			if found = append(found, x); len(found) == 2 || cr.sort != "" {
				break
			}
		}
	}
	switch {
	case len(found) == 0:
		writeMessage(w, http.StatusNotFound, "Search returned no assets")
	case len(found) > 1 && cr.sort == "":
		writeMessage(w, http.StatusBadRequest, "Search returned multiple assets, please refine search criteria to find a single asset")
	default:
		x := found[0]
		http.Redirect(w, r, h.repoURL(cmp.Or(x.c.via, x.c.repo))+"/"+escapePath(x.p), http.StatusFound)
	}
}
