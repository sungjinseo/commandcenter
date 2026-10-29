package swift

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// Handler serves Swift package registries. Reads need Read and a publish
// Add (the server's default rule); overwriting a version under writePolicy
// allow needs Edit, checked here as NuGet does.
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
// asset it came from (its staleness and modification time).
type doc struct {
	body []byte
	res  proxy.Result
}

// problem is an error the client sees as application/problem+json.
func problem(status int, format string, args ...any) error {
	return repo.Errorf(status, format, args...)
}

// WriteError renders every error, the server's 401 and 403 included, as
// the problem details the specification prescribes (repo.ErrorWriter).
func (h *Handler) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := repo.ErrorMessage(r, err)
	repo.SetAllow(w, err)
	w.Header().Set("Content-Version", "1")
	w.Header().Set("Content-Type", ctProblem)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		json.NewEncoder(w).Encode(map[string]any{"status": status, "detail": msg})
	}
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	w.Header().Set("Content-Version", "1")
	rt, err := ParseRoute(path)
	if err == nil {
		err = h.serve(w, r, rp, rt)
	}
	if err != nil {
		h.WriteError(w, r, err)
	}
}

// acceptRE is the registry media type in an Accept header, with its version.
var acceptRE = regexp.MustCompile(`application/vnd\.swift\.registry\.v([0-9]+)`)

// checkAccept is the 406 for a client that asks for another API version.
func checkAccept(r *http.Request) error {
	for _, m := range acceptRE.FindAllStringSubmatch(r.Header.Get("Accept"), -1) {
		if m[1] == "1" {
			return nil
		}
	}
	if acceptRE.MatchString(r.Header.Get("Accept")) {
		return problem(http.StatusNotAcceptable, "unsupported API version; this registry serves %s", ctRegistry)
	}
	return nil
}

// Access implements repo.AccessDecider: login needs only Read, so a reader
// signs in too; the rest is the server's default rule.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	switch {
	case path == "login", r.Method == http.MethodGet, r.Method == http.MethodHead:
		return auth.Read
	case r.Method == http.MethodDelete:
		return auth.Delete
	}
	return auth.Add
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if err := checkAccept(r); err != nil {
		return err
	}
	if rt.Kind == KindLogin {
		if r.Method != http.MethodPost {
			return repo.MethodNotAllowed("POST", "method %s not allowed", r.Method)
		}
		if auth.FromContext(r.Context()).Anonymous() { // anonymousRead let it through: ask for credentials
			w.Header().Set("WWW-Authenticate", `Basic realm="commandcenter"`)
			return problem(http.StatusUnauthorized, "authentication required")
		}
		w.WriteHeader(http.StatusOK)
		return nil
	}
	if rt.Kind == KindRelease && r.Method == http.MethodPut {
		allow := "GET, HEAD, PUT"
		if !repo.AcceptsWrites(rp) {
			allow = "GET, HEAD" // a proxy, or a group without writeMember
		}
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allow)
		}
		return h.publish(w, r, target, rt)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		allow := "GET, HEAD"
		if rt.Kind == KindRelease && repo.AcceptsWrites(rp) {
			allow += ", PUT"
		}
		return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
	}
	ctx := r.Context()
	switch rt.Kind {
	case KindIdentifiers:
		u := r.URL.Query().Get("url")
		if u == "" {
			return problem(http.StatusBadRequest, "identifiers: give url")
		}
		ids, res, err := h.identifiers(ctx, rp, u)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return problem(http.StatusNotFound, "no package has the repository URL %s", u)
		}
		return writeDoc(w, r, jsonDoc(map[string]any{"identifiers": ids}, res))
	case KindList:
		d, versions, err := h.list(ctx, rp, rp, rt)
		if err != nil {
			return err
		}
		if len(versions) > 0 {
			addLink(w, h.releaseURL(rp, rt, versions[0]), "latest-version")
		}
		return writeDoc(w, r, d)
	case KindRelease:
		d, err := h.release(ctx, rp, rt)
		if err != nil {
			return err
		}
		_, versions, err := h.list(ctx, rp, rp, rt)
		if err != nil {
			return err
		}
		h.releaseLinks(w, rp, rt, versions)
		return writeDoc(w, r, d)
	case KindManifest:
		return h.serveManifest(w, r, rp, rt)
	case KindArchive:
		res, err := h.archive(ctx, rp, rt)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		if raw, err := hex.DecodeString(res.Asset.BlobSHA256); err == nil {
			w.Header().Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(raw))
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+rt.Name+"-"+rt.Version+`.zip"`)
		res.Asset.ContentType = ctZip
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	}
	return fmt.Errorf("unknown route kind %d", rt.Kind)
}

func jsonDoc(v any, res proxy.Result) doc {
	body, _ := json.Marshal(v)
	return doc{body, res}
}

func writeDoc(w http.ResponseWriter, r *http.Request, d doc) error {
	proxy.SetStaleHeader(w, d.res)
	repo.ServeText(w, r, string(d.body), ctJSON, d.res.Asset.LastModified)
	return nil
}

func addLink(w http.ResponseWriter, target, rel string) {
	w.Header().Add("Link", "<"+target+`>; rel="`+rel+`"`)
}

// repoURL is the absolute URL of rp without a trailing slash: the release
// URLs are absolute, so baseURL must be what the clients open (https in
// production: SwiftPM refuses a plain-http registry without
// --allow-insecure-http).
func (h *Handler) repoURL(rp *repo.Repository) string {
	base := ""
	if h.BaseURL != nil {
		base = strings.TrimRight(h.BaseURL(), "/")
	}
	return base + "/repository/" + rp.Name
}

func (h *Handler) releaseURL(rp *repo.Repository, rt Route, version string) string {
	return h.repoURL(rp) + "/" + rt.Scope + "/" + rt.Name + "/" + version
}

// releaseLinks adds the latest-, successor- and predecessor-version links
// of rt.Version; versions is newest first.
func (h *Handler) releaseLinks(w http.ResponseWriter, rp *repo.Repository, rt Route, versions []string) {
	if len(versions) == 0 {
		return
	}
	addLink(w, h.releaseURL(rp, rt, versions[0]), "latest-version")
	for i, v := range versions {
		if v != rt.Version {
			continue
		}
		if i > 0 {
			addLink(w, h.releaseURL(rp, rt, versions[i-1]), "successor-version")
		}
		if i+1 < len(versions) {
			addLink(w, h.releaseURL(rp, rt, versions[i+1]), "predecessor-version")
		}
	}
}

// sortVersions orders versions newest first; what does not parse goes last.
func sortVersions(versions []string) {
	sort.SliceStable(versions, func(i, j int) bool {
		a, aok := semver.Parse(versions[i])
		b, bok := semver.Parse(versions[j])
		if aok != bok {
			return aok
		}
		if !aok {
			return versions[i] > versions[j]
		}
		return semver.Compare(a, b) > 0
	})
}

// list is the release list of rt's package served under serving's URLs,
// and its versions newest first.
func (h *Handler) list(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, []string, error) {
	switch rp.Type {
	case "hosted":
		rows, err := h.rows(ctx, rp, ID(rt.Scope, rt.Name))
		if err != nil {
			return doc{}, nil, err
		}
		if len(rows) == 0 {
			return doc{}, nil, notFoundPackage(rt)
		}
		releases := map[string]any{}
		versions := make([]string, 0, len(rows))
		for _, r := range rows {
			versions = append(versions, r.pkg.Version)
			releases[r.pkg.Version] = map[string]string{"url": h.releaseURL(serving, rt, r.pkg.Version)}
		}
		sortVersions(versions)
		return jsonDoc(map[string]any{"releases": releases}, proxy.Result{}), versions, nil
	case "proxy":
		return h.proxyList(ctx, serving, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt)
		if err != nil {
			return doc{}, nil, err
		}
		return h.list(ctx, serving, owner, rt)
	}
	return doc{}, nil, fmt.Errorf("unknown repository type %q", rp.Type)
}

func notFoundPackage(rt Route) error {
	return problem(http.StatusNotFound, "package %s.%s not found", rt.Scope, rt.Name)
}

func notFoundRelease(rt Route) error {
	return problem(http.StatusNotFound, "release %s.%s %s not found", rt.Scope, rt.Name, rt.Version)
}

// release is the release metadata of rt.
func (h *Handler) release(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	switch rp.Type {
	case "hosted":
		r, err := h.row(ctx, rp, rt)
		if err != nil {
			return doc{}, err
		}
		res := map[string]any{"name": "source-archive", "type": ctZip, "checksum": r.attrs.Checksum}
		if r.attrs.Signing != nil {
			res["signing"] = r.attrs.Signing
		}
		out := map[string]any{"id": r.attrs.ID, "version": r.pkg.Version, "resources": []any{res}, "publishedAt": r.attrs.PublishedAt}
		if len(r.attrs.Metadata) > 0 {
			out["metadata"] = r.attrs.Metadata
		} else {
			out["metadata"] = map[string]any{}
		}
		return jsonDoc(out, proxy.Result{}), nil
	case "proxy":
		return h.proxyRelease(ctx, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt)
		if err != nil {
			return doc{}, err
		}
		return h.release(ctx, owner, rt)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// archive is a version's source archive: stored (hosted), cached or
// fetched and checked against the release metadata's checksum (proxy), or
// the owning member's (group).
func (h *Handler) archive(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		r, err := h.row(ctx, rp, rt)
		if err != nil {
			return proxy.Result{}, err
		}
		a, err := h.Store.Get(ctx, rp.Name, archivePath(rt.Scope, rt.Name, r.pkg.Version))
		return proxy.Result{Asset: a}, err
	case "proxy":
		return h.proxyArchive(ctx, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt)
		if err != nil {
			return proxy.Result{}, err
		}
		return h.archive(ctx, owner, rt)
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// serveManifest answers Package.swift[?swift-version=]: the manifest read
// out of the archive (hosted and proxy alike), with the version-specific
// variants as alternate links. A variant that the archive lacks redirects
// to the unqualified manifest, as the specification has it.
func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	res, err := h.archive(r.Context(), rp, rt)
	if err != nil {
		return err
	}
	want := strings.TrimSpace(r.URL.Query().Get("swift-version"))
	m, err := h.readManifests(res.Asset)
	if err != nil {
		return err
	}
	base := h.releaseURL(rp, rt, rt.Version) + "/" + manifestFile
	file := manifestFile
	if want != "" {
		file = "Package@swift-" + want + ".swift"
		if _, ok := m.bodies[file]; !ok {
			w.Header().Set("Location", base)
			w.WriteHeader(http.StatusSeeOther)
			return nil
		}
	}
	body, err := m.read(file)
	if err != nil {
		return err
	}
	for _, name := range m.names {
		if name == file || name == manifestFile {
			continue
		}
		tools := variantRE.FindStringSubmatch(name)[1]
		w.Header().Add("Link", "<"+base+"?swift-version="+url.QueryEscape(tools)+`>; rel="alternate"; filename="`+name+`"; swift-tools-version="`+tools+`"`)
	}
	proxy.SetStaleHeader(w, res)
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	repo.ServeText(w, r, string(body), ctSwift, res.Asset.LastModified)
	return nil
}

// identifiers finds the packages whose repositoryURLs name u.
func (h *Handler) identifiers(ctx context.Context, rp *repo.Repository, u string) ([]string, proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		ids, err := h.hostedIdentifiers(ctx, rp, u)
		return ids, proxy.Result{}, err
	case "proxy":
		return h.proxyIdentifiers(ctx, rp, u)
	case "group":
		var ids []string
		seen := map[string]bool{}
		for _, m := range rp.OnlineMembers() {
			got, _, err := h.identifiers(ctx, m, u)
			if err != nil && repo.StatusOf(err) != http.StatusNotFound {
				return nil, proxy.Result{}, memberFailure(rp, m, err)
			}
			for _, id := range got {
				if !seen[strings.ToLower(id)] {
					seen[strings.ToLower(id)] = true
					ids = append(ids, id)
				}
			}
		}
		return ids, proxy.Result{}, nil
	}
	return nil, proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// owner returns the member of group rp that answers for rt's package: the
// first online member, in order, that has it (hosted: a row; proxy: an
// upstream release list; group: its own owner). That member answers
// alone, so a later (public) member never stands in for an internal package.
func (h *Handler) owner(ctx context.Context, rp *repo.Repository, rt Route) (*repo.Repository, error) {
	for _, m := range rp.OnlineMembers() {
		has, err := h.has(ctx, m, rt)
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		if has {
			return m, nil
		}
	}
	return nil, notFoundPackage(rt)
}

func (h *Handler) has(ctx context.Context, rp *repo.Repository, rt Route) (bool, error) {
	var err error
	switch rp.Type {
	case "hosted":
		var rows []row
		rows, err = h.rows(ctx, rp, ID(rt.Scope, rt.Name))
		return len(rows) > 0, err
	case "proxy":
		_, _, err = h.proxyList(ctx, rp, rp, rt)
	case "group":
		_, err = h.owner(ctx, rp, rt)
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
	slog.Error("swift group member failed", "group", group.Name, "member", m.Name, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}

// isMultipart reports whether r carries a multipart form.
func isMultipart(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "multipart/form-data"
}
