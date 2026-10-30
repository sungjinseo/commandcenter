package nexusapi

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
	"commandcenter/internal/format/maven"
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

// sees reports whether the caller may list file p of rp: a selector grant
// counts only where its expression matches the path.
func (v *view) sees(rp *repo.Repository, p string) bool {
	p = selectorPath(rp, p)
	return v.h.Auth.CanPath(v.p, rp.Name, auth.Browse, p) ||
		slices.ContainsFunc(v.groupsOf(rp.Name), func(g string) bool { return v.h.Auth.CanPath(v.p, g, auth.Browse, p) })
}

// visible returns c with only the files the caller sees, nil if none.
func (v *view) visible(c *comp) *comp {
	rp := v.h.Repos.Get(c.repo)
	if rp == nil {
		return nil
	}
	var keep []string
	for _, p := range c.paths {
		if v.sees(rp, p) {
			keep = append(keep, p)
		}
	}
	switch len(keep) {
	case 0:
		return nil
	case len(c.paths):
		return c
	}
	out := *c
	out.paths = keep
	return &out
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
	sortFields     = map[string]bool{"group": true, "name": true, "version": true, "repository": true, "format": true}
)

// gavRE is q's dependency form group:name[:version[:extension[:classifier]]].
var gavRE = regexp.MustCompile(`^([^\s:]+):([^\s:]+)(?::([^\s:]+))?(?::([^\s:]+))?(?::([^\s:]+))?$`)

// criteria are a search's parameters, as tests on a component and on each
// of its files. Parameters Command Center does not know are ignored, as
// Nexus ignores them.
type criteria struct {
	comp   []func(c *comp) bool           // all must hold
	asset  []func(c *comp, p string) bool // asset parameters: they cut a component's files
	sums   map[string]string              // asset checksums (md5, sha1, sha256, sha512), matched in the database
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
	compTerm(versionKeys, func(c *comp) []string { return []string{c.version} })
	compTerm(baseKeys, func(c *comp) []string { return []string{baseVersion(c)} })
	assetTerm(extensionKeys, func(c *comp, p string) string { _, ext := fileParts(c, p); return ext })
	assetTerm(classifierKeys, func(c *comp, p string) string { cl, _ := fileParts(c, p); return cl })

	if v, _ := value(qKeys); v != "" {
		if m := gavRE.FindStringSubmatch(v); m != nil {
			cr.gav(m)
		} else if alts := splitValue(v); len(alts) > 0 {
			t := newTerm(alts, false)
			cr.comp = append(cr.comp, func(c *comp) bool {
				return t.matches(append([]string{c.group, c.name, c.version}, c.paths...)...)
			})
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
	}{{m[1], groupForms}, {m[2], nameForms}, {m[3], func(c *comp) []string { return []string{c.version} }}}
	for _, part := range parts {
		if part.v != "" {
			t, get := literal(part.v), part.get
			cr.comp = append(cr.comp, func(c *comp) bool { return t.matches(get(c)...) })
		}
	}
	if m[4] != "" || m[5] != "" {
		ext, cl := literal(m[4]), literal(m[5])
		cr.comp = append(cr.comp, func(c *comp) bool {
			return slices.ContainsFunc(c.paths, func(p string) bool {
				fc, fe := fileParts(c, p)
				return (m[4] == "" || ext.matches(fe)) && (m[5] == "" || cl.matches(fc))
			})
		})
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
		{"maven", "assets.attributes.maven2."}, {"npm", "npm."}, {"docker", "docker."}} {
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
	}
	return []string{g}
}

// nameForms: a raw file's name with and without the leading "/".
func nameForms(c *comp) []string {
	if c.format == "raw" {
		return []string{"/" + c.name, c.name}
	}
	return []string{c.name}
}

// compGroup is the group Nexus reports: an npm scope without "@", a raw
// file's directory ("/" at the top level).
func compGroup(c *comp) string {
	switch c.format {
	case "raw":
		return "/" + strings.TrimSuffix(path.Dir(c.name), ".")
	case "npm":
		return strings.TrimPrefix(c.group, "@")
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

// matches returns c when it matches cr, with its files cut by the asset
// parameters and, when given, to sums (the files with the checksums); nil
// when it does not match or no file is left.
func (cr criteria) matches(c *comp, sums map[string]bool) *comp {
	for _, f := range cr.comp {
		if !f(c) {
			return nil
		}
	}
	if len(cr.asset) == 0 && sums == nil {
		return c
	}
	out := *c
	out.paths = nil
	for _, p := range c.paths {
		if sums != nil && !sums[p] {
			continue
		}
		if !slices.ContainsFunc(cr.asset, func(f func(*comp, string) bool) bool { return !f(c, p) }) {
			out.paths = append(out.paths, p)
		}
	}
	if len(out.paths) == 0 {
		return nil
	}
	return &out
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
func (h *Handler) searched(w http.ResponseWriter, r *http.Request) ([]*comp, criteria, bool) {
	q := r.URL.Query()
	p := principal(r)
	cr, err := parseCriteria(q)
	if err != nil {
		writeValidation(w, []string{err.Error()})
		return nil, cr, false
	}
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
	comps, err := h.find(r.Context(), repos, cr, q.Get("tag"))
	if err != nil {
		h.fail(w, err)
		return nil, cr, false
	}
	out := comps[:0]
	for _, c := range comps {
		if c = v.visible(c); c != nil {
			c.via = via[c.repo]
			out = append(out, c)
		}
	}
	return out, cr, true
}

// find runs a search in repos (groups skipped): the components that match
// cr and the tag, in cr's order.
func (h *Handler) find(ctx context.Context, repos []*repo.Repository, cr criteria, tag string) ([]*comp, error) {
	tagged, err := h.tagged(ctx, tag)
	if err != nil {
		return nil, err
	}
	var out []*comp
	for _, rp := range repos {
		if rp.Type == "group" || (cr.format != "" && rp.Format != cr.format) || (tagged != nil && !tagged[meta.ComponentKey{Repo: rp.Name}]) {
			continue
		}
		var sums, recent map[string]bool
		if cr.sums != nil {
			if sums, err = h.DB.AssetPathsWhere(ctx, rp.Name, cr.sums, time.Time{}); err != nil {
				return nil, err
			}
		}
		if !cr.since.IsZero() {
			if recent, err = h.DB.AssetPathsWhere(ctx, rp.Name, nil, cr.since); err != nil {
				return nil, err
			}
		}
		if (sums != nil && len(sums) == 0) || (recent != nil && len(recent) == 0) {
			continue
		}
		comps, err := h.components(ctx, rp)
		if err != nil {
			return nil, err
		}
		for _, c := range comps {
			if (tagged != nil && !tagged[c.metaKey()]) || (recent != nil && !slices.ContainsFunc(c.paths, func(p string) bool { return recent[p] })) {
				continue
			}
			if m := cr.matches(c, sums); m != nil {
				out = append(out, m)
			}
		}
	}
	slices.SortFunc(out, cr.compare)
	return out, nil
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	comps, cr, ok := h.searched(w, r)
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
	comps, cr, ok := h.searched(w, r)
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
	comps, cr, ok := h.searched(w, r)
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
