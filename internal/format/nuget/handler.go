package nuget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves NuGet V3 feeds. Reads need Read, a push Add and an
// unpublish Delete (the server's default rule); overwriting a version under
// writePolicy allow needs Edit, checked here as Helm does.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	Auth    *auth.Authenticator
	BaseURL func() string // live: read per request
	Now     func() time.Time
}

func New(store *repo.Store, fetcher *proxy.Fetcher, authn *auth.Authenticator, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, Auth: authn, BaseURL: baseURL, Now: time.Now}
}

// doc is a derived JSON document and, for a proxy, the cached upstream
// asset it came from (its staleness and modification time). A proxy's
// registration and search documents have write instead of body: they are
// written as they are read.
type doc struct {
	body  []byte
	res   proxy.Result
	write func(io.Writer) error
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	rt, err := ParseRoute(path)
	if err == nil {
		err = h.serve(w, r, rp, rt)
	}
	if err != nil {
		repo.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	switch rt.Kind {
	case KindPublish:
		want := http.MethodPut
		if rt.ID != "" {
			want = http.MethodDelete
		}
		allow := want
		if !repo.AcceptsWrites(rp) {
			allow = "" // a proxy, or a group without writeMember, accepts no method here
		}
		if r.Method != want {
			return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
		}
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allow)
		}
		if rt.ID != "" {
			return h.unpublish(w, r, target, rt)
		}
		return h.push(w, r, target)
	case KindSymbols:
		if r.Method != http.MethodPut {
			return repo.MethodNotAllowed("PUT", "method %s not allowed", r.Method)
		}
		return badPackage("symbol packages (.snupkg) are not supported; push the .nupkg only")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
	}
	ctx := r.Context()
	var d doc
	var err error
	switch rt.Kind {
	case KindIndex:
		d, err = h.serviceIndex(ctx, rp)
	case KindFlatIndex:
		d, err = h.flatIndex(ctx, rp, rt.ID)
	case KindFlatFile:
		res, err := h.flatFile(ctx, rp, rt)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		res.Asset.ContentType = ctNupkg
		if strings.HasSuffix(rt.File, ".nuspec") {
			res.Asset.ContentType = ctNuspec
		}
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	case KindRegistration:
		d, err = h.registration(ctx, rp, rp, rt)
	case KindSearch:
		d, err = h.search(ctx, rp, rp, parseQuery(r))
	case KindVulnIndex:
		d, err = h.vulnIndexDoc(ctx, rp)
	case KindVulnPage:
		res, err := h.vulnPage(ctx, rp, rt.Rest)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		res.Asset.ContentType = ctJSON
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	}
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, d.res)
	if d.write != nil {
		return repo.ServeStream(w, r, ctJSON, d.res.Asset.LastModified, d.write)
	}
	repo.ServeBytes(w, r, d.body, ctJSON, d.res.Asset.LastModified)
	return nil
}

// repoURL is the absolute URL of rp without a trailing slash: NuGet clients
// need absolute @id URLs in every document.
func (h *Handler) repoURL(rp *repo.Repository) string {
	base := ""
	if h.BaseURL != nil {
		base = strings.TrimRight(h.BaseURL(), "/")
	}
	return base + "/repository/" + rp.Name
}

// serviceIndex is index.json: the resources every repository type serves.
// A proxy lists only what its remote has (and never a publish endpoint),
// so a wrong remote shows at `dotnet nuget add source` time, not later.
func (h *Handler) serviceIndex(ctx context.Context, rp *repo.Repository) (doc, error) {
	base := h.repoURL(rp)
	has := func(string) bool { return true }
	var res proxy.Result
	if rp.Type == "proxy" {
		up, r, err := h.upstreamFor(ctx, rp)
		if err != nil {
			return doc{}, err
		}
		res = r
		has = func(kind string) bool {
			switch kind {
			case "flat":
				return up.flat != ""
			case "reg":
				return up.reg != ""
			case "search":
				return up.search != ""
			}
			return false
		}
	}
	var resources []map[string]string
	add := func(id string, types ...string) {
		for _, t := range types {
			resources = append(resources, map[string]string{"@id": id, "@type": t})
		}
	}
	if has("flat") {
		add(base+"/"+flatPrefix, "PackageBaseAddress/3.0.0")
	}
	if has("reg") {
		add(base+"/"+regPrefix, "RegistrationsBaseUrl", "RegistrationsBaseUrl/3.0.0-beta", "RegistrationsBaseUrl/3.0.0-rc", "RegistrationsBaseUrl/3.4.0", "RegistrationsBaseUrl/3.6.0")
	}
	if has("search") {
		add(base+"/query", "SearchQueryService", "SearchQueryService/3.0.0-beta", "SearchQueryService/3.0.0-rc", "SearchQueryService/3.5.0")
	}
	if repo.AcceptsWrites(rp) {
		add(base+"/api/v2/package", "PackagePublish/2.0.0")
	}
	if h.offersVuln(ctx, rp) {
		add(base+"/"+vulnIndex, "VulnerabilityInfo/6.7.0")
	}
	body, err := json.Marshal(map[string]any{"version": "3.0.0", "resources": resources})
	return doc{body: body, res: res}, err
}

// row is one stored version of a hosted package.
type row struct {
	v     version
	attrs pkgAttrs
	pkg   meta.Package
}

// newRow reads a stored version. A row whose attrs cannot be read (never
// written by this build) still lists with its id and version.
func newRow(p meta.Package) (row, bool) {
	v, ok := parseVersion(p.Version)
	if !ok {
		return row{}, false
	}
	r := row{v: v, pkg: p}
	if json.Unmarshal([]byte(p.Attrs), &r.attrs) != nil || r.attrs.ID == "" {
		r.attrs = pkgAttrs{ID: p.Name, Version: v.String(), Tags: []string{}, DependencyGroups: []depGroup{}, Listed: true}
	}
	return r, true
}

// versionsOf lists the versions of id in hosted rp, oldest first, without
// their metadata: the rows of a package with thousands of versions are
// megabytes, which took a 128 MiB server down at some clients at once.
func (h *Handler) versionsOf(ctx context.Context, rp *repo.Repository, id string) ([]version, error) {
	stored, err := h.Store.DB.ListPackageVersions(ctx, rp.Name, id)
	if err != nil {
		return nil, err
	}
	out := make([]version, 0, len(stored))
	for _, p := range stored {
		if v, ok := parseVersion(p.Version); ok {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return compareVersions(out[i], out[j]) < 0 })
	return out, nil
}

// flatIndex is v3-flatcontainer/<id>/index.json: {"versions": [...]} with
// the lower-cased normalized versions, oldest first.
func (h *Handler) flatIndex(ctx context.Context, rp *repo.Repository, id string) (doc, error) {
	switch rp.Type {
	case "hosted":
		stored, err := h.versionsOf(ctx, rp, id)
		if err != nil {
			return doc{}, err
		}
		if len(stored) == 0 {
			return doc{}, repo.ErrNotFound
		}
		versions := make([]string, 0, len(stored))
		for _, v := range stored {
			versions = append(versions, v.key())
		}
		body, err := json.Marshal(map[string]any{"versions": versions})
		return doc{body: body}, err
	case "proxy":
		return h.proxyFlatIndex(ctx, rp, id)
	case "group":
		owner, err := h.owner(ctx, rp, id)
		if err != nil {
			return doc{}, err
		}
		return h.flatIndex(ctx, owner, id)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// flatFile finds a .nupkg or .nuspec: stored (hosted), cached or fetched
// from the upstream flat container (proxy), or the owning member's (group).
func (h *Handler) flatFile(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, assetPath(rt.ID, rt.Version, rt.File))
		return proxy.Result{Asset: a}, err
	case "proxy":
		return h.proxyFlatFile(ctx, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt.ID)
		if err != nil {
			return proxy.Result{}, err
		}
		return h.flatFile(ctx, owner, rt)
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// registration serves registration/<id>/<rest> of rp with the URLs of
// serving (a group serves a member's documents under its own URLs).
func (h *Handler) registration(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, error) {
	switch rp.Type {
	case "hosted":
		base := h.repoURL(serving)
		if rt.Rest == "index.json" {
			versions, err := h.versionsOf(ctx, rp, rt.ID)
			if err != nil {
				return doc{}, err
			}
			if len(versions) == 0 {
				return doc{}, repo.ErrNotFound
			}
			return doc{write: func(w io.Writer) error { return h.writeRegistrationIndex(ctx, w, base, rp, rt.ID, versions) }}, nil
		}
		if rt.Version == "" {
			return doc{}, repo.ErrNotFound
		}
		p, err := h.Store.DB.GetPackage(ctx, rp.Name, rt.ID, rt.Version)
		if errors.Is(err, meta.ErrNotFound) {
			return doc{}, repo.ErrNotFound
		}
		if err != nil {
			return doc{}, err
		}
		r, ok := newRow(p)
		if !ok {
			return doc{}, repo.ErrNotFound
		}
		body, err := json.Marshal(h.registrationLeaf(base, rt.ID, r))
		return doc{body: body}, err
	case "proxy":
		return h.proxyRegistration(ctx, serving, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt.ID)
		if err != nil {
			return doc{}, err
		}
		return h.registration(ctx, serving, owner, rt)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// writeRegistrationIndex writes registration/<id>/index.json with every
// version in one inline page, as NuGet.Protocol reads it: items[].items[] each
// with a catalogEntry and packageContent, the page's count, lower and upper.
// The leaves are written as a batch of rows at a time.
func (h *Handler) writeRegistrationIndex(ctx context.Context, w io.Writer, base string, rp *repo.Repository, id string, versions []version) error {
	index := base + "/" + regPrefix + id + "/index.json"
	lower, upper := versions[0].key(), versions[len(versions)-1].key()
	head, err := json.Marshal(map[string]any{
		"@id": index + "#page/" + lower + "/" + upper, "@type": "catalog:CatalogPage",
		"count": len(versions), "lower": lower, "upper": upper, "parent": index,
	})
	if err != nil {
		return err
	}
	root, err := json.Marshal(map[string]any{"@id": index, "@type": []string{"catalog:CatalogRoot", "PackageRegistration", "catalog:Permalink"}, "count": 1})
	if err != nil {
		return err
	}
	// the objects are closed by hand: "items" is added to each
	fmt.Fprintf(w, `%s,"items":[%s,"items":[`, root[:len(root)-1], head[:len(head)-1])
	keys := make([]string, 0, len(versions))
	for _, v := range versions {
		keys = append(keys, v.key())
	}
	n := 0
	err = h.Store.DB.EachPackage(ctx, rp.Name, id, keys, func(p meta.Package) error {
		r, ok := newRow(p)
		if !ok {
			return nil
		}
		leaf, err := json.Marshal(h.registrationLeaf(base, id, r))
		if err != nil {
			return err
		}
		if n++; n > 1 {
			io.WriteString(w, ",")
		}
		_, err = w.Write(leaf)
		return err
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "]}]}")
	return err
}

// registrationLeaf is one version: the item of the registration page, also
// served at registration/<id>/<version>.json.
func (h *Handler) registrationLeaf(base, id string, r row) map[string]any {
	leaf := base + "/" + regPrefix + id + "/" + r.v.key() + ".json"
	content := base + "/" + flatPrefix + id + "/" + r.v.key() + "/" + NupkgFile(id, r.v.key())
	groups := make([]map[string]any, 0, len(r.attrs.DependencyGroups))
	for _, g := range r.attrs.DependencyGroups {
		deps := make([]map[string]any, 0, len(g.Dependencies))
		for _, d := range g.Dependencies {
			deps = append(deps, map[string]any{
				"@type": "PackageDependency", "id": d.ID, "range": d.Range,
				"registration": base + "/" + regPrefix + strings.ToLower(d.ID) + "/index.json",
			})
		}
		group := map[string]any{"@type": "PackageDependencyGroup", "dependencies": deps}
		if g.TargetFramework != "" {
			group["targetFramework"] = g.TargetFramework
		}
		groups = append(groups, group)
	}
	a := r.attrs
	entry := map[string]any{
		"@id": leaf, "@type": "PackageDetails", "id": a.ID, "version": a.Version,
		"authors": a.Authors, "description": a.Description, "summary": a.Summary, "title": a.Title,
		"iconUrl": a.IconURL, "licenseUrl": a.LicenseURL, "licenseExpression": a.License, "projectUrl": a.ProjectURL,
		"tags": a.Tags, "requireLicenseAcceptance": a.RequireLicenseAcceptance, "listed": a.Listed,
		"published": a.Published, "packageContent": content, "dependencyGroups": groups,
	}
	return map[string]any{
		"@id": leaf, "@type": "Package", "commitId": r.pkg.Version, "commitTimeStamp": a.Published,
		"catalogEntry": entry, "packageContent": content, "registration": base + "/" + regPrefix + id + "/index.json",
	}
}

// query is a search request: q is the text (lower-cased), skip and take
// the page, prerelease whether prerelease versions count.
type query struct {
	q          string
	skip, take int
	prerelease bool
}

const maxTake = 1000

func parseQuery(r *http.Request) query {
	v := r.URL.Query()
	q := query{q: strings.ToLower(strings.TrimSpace(v.Get("q"))), take: 20}
	if n, err := strconv.Atoi(v.Get("skip")); err == nil && n > 0 {
		q.skip = n
	}
	if n, err := strconv.Atoi(v.Get("take")); err == nil && n > 0 {
		q.take = min(n, maxTake)
	}
	q.prerelease, _ = strconv.ParseBool(v.Get("prerelease"))
	return q
}

// encode is the query string sent to an upstream search, in a fixed order
// (it keys the cache). semVerLevel=2.0.0 asks for SemVer 2 packages too.
func (q query) encode() string {
	return "q=" + url.QueryEscape(q.q) + "&skip=" + strconv.Itoa(q.skip) + "&take=" + strconv.Itoa(q.take) +
		"&prerelease=" + strconv.FormatBool(q.prerelease) + "&semVerLevel=2.0.0"
}

// hit is one search result: a package and its versions, newest last.
type hit struct {
	id   string // lower-case, the merge key
	data map[string]any
}

// search answers query?q= of rp under serving's URLs: {"totalHits", "data"}.
func (h *Handler) search(ctx context.Context, serving, rp *repo.Repository, q query) (doc, error) {
	switch rp.Type {
	case "hosted":
		hits, err := h.hostedSearch(ctx, serving, rp, q)
		if err != nil {
			return doc{}, err
		}
		return searchDoc(hits, q)
	case "proxy":
		return h.proxySearch(ctx, serving, rp, q)
	case "group":
		// Each member is asked for skip+take, merged in member order with
		// the first member keeping an id, and the page is cut from the merge.
		// ponytail: a proxy member is asked for all of skip+take every page.
		sources, err := h.searchSources(ctx, serving, rp, query{q: q.q, take: min(q.skip+q.take, maxTake), prerelease: q.prerelease})
		if err != nil {
			return doc{}, err
		}
		base := h.repoURL(serving)
		return doc{write: func(w io.Writer) error { return h.writeMerged(w, sources, base, q) }}, nil
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// searchSource is what one member of a group answers a search with: a hosted
// member's hits, or a proxy member's cached upstream answer. A failing member
// fails the search here, before anything is written.
type searchSource struct {
	hits []hit
	file proxy.Result
	up   upstream
}

func (h *Handler) searchSources(ctx context.Context, serving, rp *repo.Repository, q query) ([]searchSource, error) {
	var out []searchSource
	for _, m := range rp.OnlineMembers() {
		var src []searchSource
		var err error
		switch m.Type {
		case "hosted":
			var hits []hit
			hits, err = h.hostedSearch(ctx, serving, m, q)
			src = []searchSource{{hits: hits}}
		case "proxy":
			var s searchSource
			s.file, s.up, err = h.searchFile(ctx, m, q)
			src = []searchSource{s}
		case "group":
			src, err = h.searchSources(ctx, serving, m, q)
		default:
			err = fmt.Errorf("unknown repository type %q", m.Type)
		}
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		out = append(out, src...)
	}
	return out, nil
}

// writeMerged writes a group's search answer from its members' in order, the
// first keeping an id: the upstream answers are read one hit at a time, so
// take=1000 (10 MiB from nuget.org) is not in memory, and totalHits comes last.
func (h *Handler) writeMerged(w io.Writer, sources []searchSource, base string, q query) error {
	seen := map[string]bool{}
	total, written := 0, 0
	// onPage counts a hit and says whether it goes on the page, ready for it.
	onPage := func(id string) bool {
		if id = strings.ToLower(id); id == "" || seen[id] {
			return false
		}
		seen[id] = true
		if total++; total <= q.skip || written == q.take {
			return false
		}
		if written++; written > 1 {
			io.WriteString(w, ",")
		}
		return true
	}
	io.WriteString(w, `{"data":[`)
	for _, src := range sources {
		for _, hit := range src.hits {
			if !onPage(hit.id) {
				continue
			}
			data, err := json.Marshal(hit.data)
			if err != nil {
				return err
			}
			if _, err := w.Write(data); err != nil {
				return err
			}
		}
		if src.file.Asset.BlobSHA256 == "" {
			continue
		}
		if err := h.writeUpstreamHits(w, src, base, onPage); err != nil {
			return repo.Errorf(http.StatusBadGateway, "upstream search result is not JSON: %v", err)
		}
	}
	_, err := fmt.Fprintf(w, `],"totalHits":%d}`, total)
	return err
}

// searchDoc cuts the page q asks for from hits and writes the response.
func searchDoc(hits []hit, q query) (doc, error) {
	total := len(hits)
	hits = hits[min(q.skip, total):]
	hits = hits[:min(q.take, len(hits))]
	data := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		data = append(data, h.data)
	}
	body, err := json.Marshal(map[string]any{"totalHits": total, "data": data})
	return doc{body: body}, err
}

// hostedSearch matches q against the package ids of hosted rp ("packageid:x"
// and "id:x" match an id exactly) and lists each match with its versions,
// prerelease ones only when asked, the newest as the hit's version, whose
// metadata the hit shows.
// ponytail: two queries per matching id; a search index if feeds grow past a
// few thousand packages.
func (h *Handler) hostedSearch(ctx context.Context, serving, rp *repo.Repository, q query) ([]hit, error) {
	text, exact := q.q, false
	for _, p := range []string{"packageid:", "id:"} {
		if rest, ok := strings.CutPrefix(text, p); ok {
			text, exact = strings.TrimSpace(rest), true
			break
		}
	}
	names, _, err := h.Store.DB.SearchPackageNames(ctx, rp.Name, text, maxTake)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	base := h.repoURL(serving)
	var hits []hit
	for _, id := range names {
		if exact && id != text {
			continue
		}
		stored, err := h.versionsOf(ctx, rp, id)
		if err != nil {
			return nil, err
		}
		var versions []map[string]any
		var newest string
		for _, v := range stored {
			if v.prerelease() && !q.prerelease {
				continue
			}
			versions = append(versions, map[string]any{
				"version": v.String(), "downloads": 0,
				"@id": base + "/" + regPrefix + id + "/" + v.key() + ".json",
			})
			newest = v.key()
		}
		if newest == "" {
			continue
		}
		p, err := h.Store.DB.GetPackage(ctx, rp.Name, id, newest)
		if errors.Is(err, meta.ErrNotFound) { // deleted since the list
			continue
		}
		if err != nil {
			return nil, err
		}
		latest, ok := newRow(p)
		if !ok {
			continue
		}
		a := latest.attrs
		reg := base + "/" + regPrefix + id + "/index.json"
		authors := strings.Split(a.Authors, ",")
		for i := range authors {
			authors[i] = strings.TrimSpace(authors[i])
		}
		hits = append(hits, hit{id: id, data: map[string]any{
			"@id": reg, "@type": "Package", "registration": reg, "id": a.ID, "version": a.Version,
			"description": a.Description, "summary": a.Summary, "title": a.Title, "iconUrl": a.IconURL,
			"licenseUrl": a.LicenseURL, "projectUrl": a.ProjectURL, "tags": a.Tags, "authors": authors,
			"totalDownloads": 0, "verified": false, "versions": versions,
		}})
	}
	return hits, nil
}

// owner returns the member of group rp that answers for id: the first
// online member, in order, that has the id (hosted: a row; proxy: an
// upstream flat index; group: its own owner). That member answers alone,
// so a later (public) member never stands in for an internal package.
func (h *Handler) owner(ctx context.Context, rp *repo.Repository, id string) (*repo.Repository, error) {
	for _, m := range rp.OnlineMembers() {
		has, err := h.has(ctx, m, id)
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		if has {
			return m, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (h *Handler) has(ctx context.Context, rp *repo.Repository, id string) (bool, error) {
	var err error
	switch rp.Type {
	case "hosted":
		st, err := h.Store.DB.PackageStamp(ctx, rp.Name, id)
		return st.Count > 0, err
	case "proxy":
		_, err = h.proxyFlatIndex(ctx, rp, id)
	case "group":
		_, err = h.owner(ctx, rp, id)
	default:
		return false, fmt.Errorf("unknown repository type %q", rp.Type)
	}
	if errors.Is(err, repo.ErrNotFound) || repo.StatusOf(err) == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// memberFailure logs a group member's error and returns it as a 5xx, as
// helm groups do: a later (public) member never stands in for a failing one.
func memberFailure(group, m *repo.Repository, err error) error {
	slog.Error("nuget group member failed", "group", group.Name, "member", m.Name, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}
