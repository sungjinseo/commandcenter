package composer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves Composer v2 repositories. Reads need Read, an upload Add
// and a delete Delete (the server's default rule); overwriting a version
// under writePolicy allow needs Edit, checked here as NuGet does.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	Auth    *auth.Authenticator
	BaseURL func() string // live: read per request
	Now     func() time.Time
	locks   repo.KeyedMutex // serializes the writes of one package per repository
}

func New(store *repo.Store, fetcher *proxy.Fetcher, authn *auth.Authenticator, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, Auth: authn, BaseURL: baseURL, Now: time.Now}
}

// doc is a derived JSON document and, for a proxy, the cached upstream
// asset it came from (its staleness and modification time).
type doc struct {
	body []byte
	res  proxy.Result
}

// entry is one version of a package as Composer's metadata lists it: the
// composer.json fields plus version_normalized, dist and time.
type entry = map[string]any

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
	if rt.Kind == KindUpload {
		allow := "PUT, DELETE"
		if !repo.AcceptsWrites(rp) {
			allow = "" // a proxy, or a group without writeMember, accepts no method here
		}
		if r.Method != http.MethodPut && r.Method != http.MethodDelete {
			return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
		}
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allow)
		}
		if r.Method == http.MethodDelete {
			return h.unpublish(w, r, target, rt)
		}
		return h.upload(w, r, target, rt)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
	}
	ctx := r.Context()
	var d doc
	var err error
	switch rt.Kind {
	case KindRoot:
		d, err = h.root(ctx, rp)
	case KindMetadata:
		d, err = h.metadata(ctx, rp, rp, rt)
	case KindDist:
		res, err := h.dist(ctx, rp, rt)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		res.Asset.ContentType = ctZip
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	case KindSearch:
		q := r.URL.Query()
		d, err = h.search(ctx, rp, q.Get("q"), q.Get("type"))
	}
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, d.res)
	repo.ServeText(w, r, string(d.body), ctJSON, d.res.Asset.LastModified)
	return nil
}

// repoURL is the absolute URL of rp without a trailing slash: the documents
// carry absolute URLs, which Composer accepts alongside relative ones.
func (h *Handler) repoURL(rp *repo.Repository) string {
	base := ""
	if h.BaseURL != nil {
		base = strings.TrimRight(h.BaseURL(), "/")
	}
	return base + "/repository/" + rp.Name
}

// rootDoc is the packages.json of a repository served at base: metadata
// per package under p2/ and search. The versions are sent expanded, so the
// root carries no "minified" key.
func rootDoc(base string) map[string]any {
	return map[string]any{
		"packages":     map[string]any{},
		"metadata-url": base + "/" + p2Prefix + "%package%.json",
		"search":       base + "/search.json?q=%query%&type=%type%",
	}
}

// root is packages.json. A hosted repository lists its packages as
// available-packages, so Composer asks it only for those; a group cannot
// enumerate its proxies and lists none.
func (h *Handler) root(ctx context.Context, rp *repo.Repository) (doc, error) {
	switch rp.Type {
	case "hosted":
		names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
		if err != nil {
			return doc{}, err
		}
		if names == nil {
			names = []string{}
		}
		out := rootDoc(h.repoURL(rp))
		out["available-packages"] = names
		body, err := json.Marshal(out)
		return doc{body: body}, err
	case "proxy":
		return h.proxyRoot(ctx, rp)
	case "group":
		body, err := json.Marshal(rootDoc(h.repoURL(rp)))
		return doc{body: body}, err
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// metadata serves p2/<name>.json or p2/<name>~dev.json of rp with the URLs
// of serving (a group serves a member's document under its own URLs).
func (h *Handler) metadata(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, error) {
	switch rp.Type {
	case "hosted":
		rows, err := h.rows(ctx, rp, rt.Name)
		if err != nil {
			return doc{}, err
		}
		if len(rows) == 0 {
			return doc{}, repo.ErrNotFound
		}
		base := h.repoURL(serving)
		entries := []entry{}
		for _, r := range rows {
			if IsDev(r.version) != rt.Dev {
				continue
			}
			e := entry{}
			for k, v := range r.entry {
				e[k] = v
			}
			dist := entry{}
			if d, ok := r.entry["dist"].(map[string]any); ok {
				for k, v := range d {
					dist[k] = v
				}
			}
			dist["type"], dist["url"] = "zip", base+"/"+distPath(rt.Name, r.version)
			e["dist"] = dist
			entries = append(entries, e)
		}
		body, err := json.Marshal(map[string]any{"packages": map[string]any{rt.Name: entries}})
		return doc{body: body}, err
	case "proxy":
		return h.proxyMetadata(ctx, serving, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt.Name)
		if err != nil {
			return doc{}, err
		}
		return h.metadata(ctx, serving, owner, rt)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// dist finds a version's archive: stored (hosted), cached or fetched from
// the upstream dist URL (proxy), or the owning member's (group). A hosted
// path names the version, a proxy path the version and the sha1 of the
// upstream URL, so each kind answers only its own shape.
func (h *Handler) dist(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		if rt.Ref != "" {
			return proxy.Result{}, repo.ErrNotFound
		}
		a, err := h.Store.Get(ctx, rp.Name, distPath(rt.Name, rt.Version))
		return proxy.Result{Asset: a}, err
	case "proxy":
		if rt.Ref == "" {
			return proxy.Result{}, repo.ErrNotFound
		}
		return h.proxyDist(ctx, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt.Name)
		if err != nil {
			return proxy.Result{}, err
		}
		return h.dist(ctx, owner, rt)
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// search answers search.json?q=&type=: {"results": [{name, description,
// ...}], "total"}. A hosted repository matches the text against package
// names and descriptions, a proxy passes the upstream's answer through,
// and a group merges its members' results, the first member keeping a name.
func (h *Handler) search(ctx context.Context, rp *repo.Repository, q, typ string) (doc, error) {
	switch rp.Type {
	case "hosted":
		results, err := h.hostedSearch(ctx, rp, q, typ)
		if err != nil {
			return doc{}, err
		}
		return searchDoc(results)
	case "proxy":
		return h.proxySearch(ctx, rp, q, typ)
	case "group":
		var results []entry
		seen := map[string]bool{}
		for _, m := range rp.OnlineMembers() {
			d, err := h.search(ctx, m, q, typ)
			if err != nil {
				return doc{}, memberFailure(rp, m, err)
			}
			var got struct {
				Results []entry `json:"results"`
			}
			if err := json.Unmarshal(d.body, &got); err != nil {
				return doc{}, memberFailure(rp, m, err)
			}
			for _, e := range got.Results {
				name, _ := e["name"].(string)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				results = append(results, e)
			}
		}
		return searchDoc(results)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

func searchDoc(results []entry) (doc, error) {
	if results == nil {
		results = []entry{}
	}
	body, err := json.Marshal(map[string]any{"results": results, "total": len(results)})
	return doc{body: body}, err
}

// maxSearchResults bounds a hosted search answer.
const maxSearchResults = 100

// hostedSearch lists the packages of hosted rp whose name or description
// (of the newest version) contains q, of type typ when given.
// ponytail: reads every package's rows; a search index if repositories
// grow past a few thousand packages.
func (h *Handler) hostedSearch(ctx context.Context, rp *repo.Repository, q, typ string) ([]entry, error) {
	names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return nil, err
	}
	q = strings.ToLower(strings.TrimSpace(q))
	var results []entry
	for _, name := range names {
		rows, err := h.rows(ctx, rp, name)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			continue
		}
		e := rows[0].entry
		desc, _ := e["description"].(string)
		ptype, _ := e["type"].(string)
		if ptype == "" {
			ptype = "library"
		}
		if typ != "" && ptype != typ {
			continue
		}
		if q != "" && !strings.Contains(name, q) && !strings.Contains(strings.ToLower(desc), q) {
			continue
		}
		results = append(results, entry{"name": name, "description": desc})
		if len(results) == maxSearchResults {
			break
		}
	}
	return results, nil
}

// owner returns the member of group rp that answers for name: the first
// online member, in order, that has the package (hosted: a row; proxy: an
// upstream metadata file; group: its own owner). That member answers
// alone, so a later (public) member never stands in for an internal package.
func (h *Handler) owner(ctx context.Context, rp *repo.Repository, name string) (*repo.Repository, error) {
	for _, m := range rp.OnlineMembers() {
		has, err := h.has(ctx, m, name)
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		if has {
			return m, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (h *Handler) has(ctx context.Context, rp *repo.Repository, name string) (bool, error) {
	var err error
	switch rp.Type {
	case "hosted":
		var pkgs []meta.Package
		pkgs, err = h.Store.DB.ListPackages(ctx, rp.Name, name)
		return len(pkgs) > 0, err
	case "proxy":
		_, _, err = h.proxyDoc(ctx, rp, name, false)
	case "group":
		_, err = h.owner(ctx, rp, name)
	default:
		return false, fmt.Errorf("unknown repository type %q", rp.Type)
	}
	if errors.Is(err, repo.ErrNotFound) || repo.StatusOf(err) == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// memberFailure logs a group member's error and returns it as a 5xx, as
// NuGet groups do: a later (public) member never stands in for a failing one.
func memberFailure(group, m *repo.Repository, err error) error {
	slog.Error("composer group member failed", "group", group.Name, "member", m.Name, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}

// fill substitutes Composer's URL template placeholders.
func fill(template, pkg, query, typ string) string {
	r := strings.NewReplacer("%package%", pkg, "%query%", url.QueryEscape(query), "%type%", url.QueryEscape(typ))
	return r.Replace(template)
}
