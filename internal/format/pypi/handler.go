package pypi

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
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves Python package indexes. Reads need Read and uploads Add
// (Access); replacing a file under writePolicy allow needs Edit, checked here
// as Maven does.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	Auth    *auth.Authenticator
	BaseURL func() string // live: read per request (plan 12)
	Now     func() time.Time
}

func New(store *repo.Store, fetcher *proxy.Fetcher, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, BaseURL: baseURL, Now: time.Now}
}

// fileAttrs is the packages.attrs document of a hosted file. Blake2b256 and
// MetadataSHA256 (the sha256 of the file's core metadata sidecar, see
// metadataKey) are absent in rows older builds wrote; the sidecar is then
// extracted when the row is first listed (walk) or requested (see extract).
// MetadataSHA256 noMetadata marks a file none can be read from, which the
// index then does not advertise (the initial release stored files without
// reading them).
type fileAttrs struct {
	Filename       string `json:"filename"`
	Version        string `json:"version"`
	SHA256         string `json:"sha256"`
	RequiresPython string `json:"requires_python,omitempty"`
	Blake2b256     string `json:"blake2b_256,omitempty"`
	MetadataSHA256 string `json:"metadata_sha256,omitempty"`
	HasSig         bool   `json:"has_sig,omitempty"` // a signature came with the upload, at <file>.asc
}

const noMetadata = "-"

// errStop ends a walk early once the wanted file is found.
var errStop = errors.New("stop")

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
		allow := http.MethodPost
		if !repo.AcceptsWrites(rp) {
			allow = "" // a proxy, or a group without writeMember, accepts no method here
		}
		if r.Method != http.MethodPost {
			return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
		}
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allow)
		}
		return h.upload(w, r, target)
	}
	if rt.Kind == KindSearch {
		if r.Method != http.MethodPost {
			return repo.MethodNotAllowed("POST", "method %s not allowed", r.Method)
		}
		return h.search(w, r, rp)
	}
	allow := "GET, HEAD"
	if rt.Kind == KindFile && repo.AcceptsWrites(rp) {
		allow = "GET, HEAD, DELETE"
	}
	if r.Method == http.MethodDelete && rt.Kind == KindFile {
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allow)
		}
		return h.deleteFile(w, r, target, rt.Name, rt.File)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
	}
	switch rt.Kind {
	case KindRoot:
		// The root page uses relative links, so it must be addressed with a trailing slash.
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return nil
		}
		names, err := h.projects(r.Context(), rp)
		if err != nil {
			return err
		}
		known := make(map[string]bool, len(names))
		for _, n := range names {
			known[n] = true
		}
		err = writeProjects(w, wantsJSON(r.Header.Get("Accept")), names, func(emit func(string) error) error {
			if r.Method != http.MethodGet { // no body, so no reason to read a remote's list
				return nil
			}
			return h.remoteProjects(r.Context(), rp, known, emit)
		})
		if err != nil { // only a write fails it: the client left
			slog.Warn("pypi response write failed", "path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
		}
		return nil
	case KindProject:
		return h.project(w, r, rp, rt.Name)
	case KindJSON:
		owner, err := h.owner(r.Context(), rp, rt.Name)
		if err != nil {
			return err
		}
		doc, res, err := h.jsonDoc(r.Context(), rp, owner, rt.Name, rt.Version)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		repo.ServeText(w, r, string(doc), "application/json", res.Asset.LastModified)
		return nil
	case KindMetadata:
		res, err := h.metadata(r.Context(), rp, rt.Name, rt.File)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		a := res.Asset
		a.ContentType = metadataType
		repo.ServeAsset(w, r, h.Store, a)
		return nil
	}
	res, err := h.file(r.Context(), rp, rt.Name, rt.File)
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, res)
	a := res.Asset
	a.ContentType = "application/octet-stream"
	repo.ServeAsset(w, r, h.Store, a)
	return nil
}

// fileURL is a file's link in a project page: host-relative, keeping the
// path of baseURL, so pip downloads from the host (and with the credentials)
// it reached the index with (QA L-5). PEP 503 and PEP 691 allow relative URLs.
func (h *Handler) fileURL(rp *repo.Repository, project, file string) string {
	prefix := ""
	if u, err := url.Parse(h.baseURL()); err == nil {
		prefix = u.EscapedPath()
	}
	return prefix + "/repository/" + rp.Name + "/packages/" + project + "/" + url.PathEscape(file)
}

func (h *Handler) project(w http.ResponseWriter, r *http.Request, rp *repo.Repository, name string) error {
	ren := newRenderer(w, wantsJSON(r.Header.Get("Accept")), name)
	seen := map[string]bool{}
	err := h.walk(r.Context(), rp, name, func(f File) error {
		key := strings.ToLower(f.Filename) // the first member's, in a group that merges its members
		if seen[key] {
			return nil
		}
		seen[key] = true
		return ren.File(f, h.fileURL(rp, name, f.Filename))
	})
	if err != nil {
		if ren.Started() {
			slog.Error("pypi response failed after output started", "path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
			panic(http.ErrAbortHandler)
		}
		return err
	}
	sent, err := ren.Close()
	if err != nil {
		slog.Warn("pypi response write failed", "path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
		return nil
	}
	if !sent {
		return repo.Errorf(http.StatusNotFound, "project %q not found", name)
	}
	return nil
}

func indexTarget(rp *repo.Repository, project string) proxy.Target {
	return proxy.Target{
		Key:    "simple/" + project + "/index",
		Remote: "simple/" + project + "/",
		Header: http.Header{"Accept": {jsonType + ", text/html;q=0.1"}},
		// A refused project page fails a group closed (proxy.Target).
		DeniedIsFailure: true,
		Check: func(_ context.Context, body io.ReadSeeker, s blob.Sums) (blob.Sums, error) {
			return s, checkPage(body, projectBase(rp, project), project)
		},
	}
}

func projectBase(rp *repo.Repository, project string) *url.URL {
	return rp.RemoteURL.ResolveReference(&url.URL{Path: "simple/" + project + "/"})
}

// walk calls fn for every file of project in rp; a group walks only the
// member that owns the project (see owner).
func (h *Handler) walk(ctx context.Context, rp *repo.Repository, project string, fn func(File) error) error {
	switch rp.Type {
	case "hosted":
		pkgs, err := h.Store.DB.ListPackages(ctx, rp.Name, project)
		if err != nil {
			return err
		}
		for _, p := range pkgs {
			var a fileAttrs
			if err := json.Unmarshal([]byte(p.Attrs), &a); err != nil {
				return err
			}
			// A row an older build stored without its core metadata learns it
			// now (extract marks the row either way), so the index advertises
			// only sidecars that answer: pip aborts on an advertised 404.
			if a.MetadataSHA256 == "" {
				sc, err := h.sidecar(ctx, rp, project, a.Filename)
				switch {
				case err == nil:
					a.MetadataSHA256 = sc.BlobSHA256
				case repo.StatusOf(err) == http.StatusNotFound:
					a.MetadataSHA256 = noMetadata
				default:
					return err
				}
			}
			f := File{Filename: a.Filename, SHA256: a.SHA256, RequiresPython: a.RequiresPython, HasMetadata: a.MetadataSHA256 != noMetadata, HasSig: a.HasSig}
			if f.HasMetadata {
				f.MetadataSHA256 = a.MetadataSHA256
			}
			if err := fn(f); err != nil {
				return err
			}
		}
		return nil
	case "proxy":
		res, err := h.Fetcher.FetchTarget(ctx, rp, indexTarget(rp, project), proxy.Metadata)
		if repo.StatusOf(err) == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		f, err := h.Store.Open(res.Asset)
		if err != nil {
			return err
		}
		defer f.Close()
		base := projectBase(rp, project)
		if !strings.Contains(res.Asset.ContentType, "json") {
			// Only the project's files: what checkPage let through may link
			// to others, and a page an older build cached is unchecked.
			page := fn
			fn = func(f File) error {
				if !fileMatches(project, f.Filename) {
					return nil
				}
				return page(f)
			}
		}
		return parseIndex(f, res.Asset.ContentType, base, fn)
	case "group":
		if rp.MergeMembers {
			return h.walkMerged(ctx, rp, project, fn)
		}
		m, err := h.owner(ctx, rp, project)
		if repo.StatusOf(err) == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		return h.walk(ctx, m, project, fn)
	}
	return fmt.Errorf("unknown repository type %q", rp.Type)
}

// walkMerged calls fn for the files of project in every online member of a
// group with mergeMembers, in member order, as Nexus's groups list them
// (project keeps the first of a file name). A member that fails is passed
// over, so an upstream outage leaves the internal packages installable; it
// fails the request only when no other member has the project.
func (h *Handler) walkMerged(ctx context.Context, rp *repo.Repository, project string, fn func(File) error) error {
	found := false
	var failure, stop error
	for _, m := range rp.OnlineMembers() {
		err := h.walk(ctx, m, project, func(f File) error {
			found = true
			stop = fn(f)
			return stop
		})
		if stop != nil {
			return stop
		}
		if err != nil {
			failure = memberFailure(rp, m, project, err)
		}
	}
	if !found {
		return failure
	}
	return nil
}

// owner returns the repository whose files answer for project in rp: rp
// itself if it lists a file of project, or for a group the first member in
// order (nested groups searched in place) that does. As in Cargo groups, that
// member answers alone: files of a later (public) member never mix in, so its
// release cannot stand in for an internal one (dependency confusion, QA
// N-1). Only "not found" moves on; a member ahead of the owner that is
// offline (503) or fails (5xx) fails the request, unless the group merges its
// members (walkMerged), which passes it over: there the owner answers only the
// JSON API, whose documents are not merged.
func (h *Handler) owner(ctx context.Context, rp *repo.Repository, project string) (*repo.Repository, error) {
	if rp.Type != "group" {
		found := false
		err := h.walk(ctx, rp, project, func(File) error {
			found = true
			return errStop
		})
		switch {
		case found:
			return rp, nil
		case err == nil:
			return nil, repo.ErrNotFound
		}
		return nil, err
	}
	var failure error
	for _, m := range rp.MemberRepos {
		if !m.IsOnline() {
			if rp.MergeMembers {
				continue
			}
			return nil, repo.MemberOffline(m)
		}
		o, err := h.owner(ctx, m, project)
		if err == nil {
			return o, nil
		}
		if repo.StatusOf(err) != http.StatusNotFound {
			if !rp.MergeMembers {
				return nil, memberFailure(rp, m, project, err)
			}
			failure = memberFailure(rp, m, project, err)
		}
	}
	if failure != nil {
		return nil, failure
	}
	return nil, repo.ErrNotFound
}

// memberFailure logs a group member's error and returns it as a 5xx.
func memberFailure(group, m *repo.Repository, project string, err error) error {
	slog.Error("pypi group member failed", "group", group.Name, "member", m.Name, "project", project, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}

// routed reports whether proxy rp's routing rule lets key, the request path
// of a file, through: as Nexus has it, before the cache is read, and whichever
// host the upstream index says the file is on.
func (h *Handler) routed(rp *repo.Repository, key string) bool {
	return h.Fetcher.RouteAllowed == nil || h.Fetcher.RouteAllowed(rp, "/"+key)
}

// file returns a distribution file, or the signature of one. Proxies find the
// upstream URL in the project's cached index and verify the download against
// its sha256; a signature is at the file's URL, when the index says it has one.
func (h *Handler) file(ctx context.Context, rp *repo.Repository, project, filename string) (proxy.Result, error) {
	key := fileKey(project, filename)
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, key)
		return proxy.Result{Asset: a}, err
	case "proxy":
		if !h.routed(rp, key) {
			return proxy.Result{}, repo.ErrNotFound
		}
		a, err := h.Store.Get(ctx, rp.Name, key)
		if err == nil {
			return proxy.Result{Asset: a}, nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return proxy.Result{}, err
		}
		dist, isSig := strings.CutSuffix(filename, sigExt)
		var found File
		ok := false
		err = h.walk(ctx, rp, project, func(f File) error {
			if f.Filename == filename || isSig && f.HasSig && f.Filename == dist {
				found, ok = f, true
				return errStop
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return proxy.Result{}, err
		}
		if !ok || found.URL == "" {
			return proxy.Result{}, repo.ErrNotFound
		}
		// Never a web page; the sha256, where the index has one, says so already.
		t := proxy.Target{Key: key, URL: found.URL, Route: key, SHA256: found.SHA256, NotHTML: found.SHA256 == ""}
		if found.Filename != filename {
			t.URL, t.SHA256, t.NotHTML = found.URL+sigExt, "", true
		}
		return h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
	case "group":
		return h.fromMember(ctx, rp, project, func(m *repo.Repository) (proxy.Result, error) {
			return h.file(ctx, m, project, filename)
		})
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// fromMember answers a group's request for a file of project with get of
// the member that has it: only the owner's files, even by direct URL (QA
// N-1), as in resolve.OwnerFile. The first member that has the file answers,
// a member without it that lists the project owns the project, and the last
// member is not asked. So a cached file needs no index, also while the
// upstream is down or after it dropped the project. In a group with
// mergeMembers the first member that has the file answers, whichever others
// list the project or fail (walkMerged).
func (h *Handler) fromMember(ctx context.Context, rp *repo.Repository, project string, get func(*repo.Repository) (proxy.Result, error)) (proxy.Result, error) {
	if rp.MergeMembers {
		var failure error
		for _, m := range rp.OnlineMembers() {
			res, err := get(m)
			if err == nil {
				return res, nil
			}
			if repo.StatusOf(err) != http.StatusNotFound {
				failure = memberFailure(rp, m, project, err)
			}
		}
		if failure != nil {
			return proxy.Result{}, failure
		}
		return proxy.Result{}, repo.ErrNotFound
	}
	for i, m := range rp.MemberRepos {
		if !m.IsOnline() {
			return proxy.Result{}, repo.MemberOffline(m)
		}
		res, err := get(m)
		if err == nil {
			return res, nil
		}
		if repo.StatusOf(err) == http.StatusNotFound && i < len(rp.MemberRepos)-1 {
			if _, err = h.owner(ctx, m, project); err == nil {
				return proxy.Result{}, repo.ErrNotFound // m owns the project, without this file
			}
		}
		if repo.StatusOf(err) != http.StatusNotFound {
			return proxy.Result{}, memberFailure(rp, m, project, err)
		}
	}
	return proxy.Result{}, repo.ErrNotFound
}

// metadata returns a file's PEP 658 core metadata sidecar. A hosted file's
// is stored at upload (or extracted from the file on first request). A
// proxy serves the cached sidecar, extracts it from the cached file, or
// fetches <file URL>.metadata when the upstream index advertises it
// (verified against the advertised sha256); a group answers as for files.
func (h *Handler) metadata(ctx context.Context, rp *repo.Repository, project, filename string) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		a, err := h.sidecar(ctx, rp, project, filename)
		return proxy.Result{Asset: a}, err
	case "proxy":
		if !h.routed(rp, metadataKey(project, filename)) {
			return proxy.Result{}, repo.ErrNotFound
		}
		a, err := h.Store.Get(ctx, rp.Name, metadataKey(project, filename))
		if err == nil {
			return proxy.Result{Asset: a}, nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return proxy.Result{}, err
		}
		if _, err := h.Store.Get(ctx, rp.Name, fileKey(project, filename)); err == nil {
			a, err := h.extract(ctx, rp, project, filename)
			return proxy.Result{Asset: a}, err
		} else if !errors.Is(err, repo.ErrNotFound) {
			return proxy.Result{}, err
		}
		var found File
		ok := false
		err = h.walk(ctx, rp, project, func(f File) error {
			if f.Filename == filename {
				found, ok = f, true
				return errStop
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return proxy.Result{}, err
		}
		if !ok || found.URL == "" || !found.HasMetadata {
			return proxy.Result{}, repo.ErrNotFound
		}
		t := proxy.Target{Key: metadataKey(project, filename), URL: found.URL + ".metadata", Route: metadataKey(project, filename), SHA256: found.MetadataSHA256, NotHTML: true}
		return h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
	case "group":
		return h.fromMember(ctx, rp, project, func(m *repo.Repository) (proxy.Result, error) {
			return h.metadata(ctx, m, project, filename)
		})
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// projects lists hosted projects plus projects cached by proxies, sorted and unique.
func (h *Handler) projects(ctx context.Context, rp *repo.Repository) ([]string, error) {
	set := map[string]bool{}
	if err := h.collectProjects(ctx, rp, set); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func (h *Handler) collectProjects(ctx context.Context, rp *repo.Repository, set map[string]bool) error {
	switch rp.Type {
	case "hosted":
		names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
		if err != nil {
			return err
		}
		for _, n := range names {
			set[n] = true
		}
	case "proxy":
		assets, err := h.Store.DB.ListAssets(ctx, rp.Name, "simple/")
		if err != nil {
			return err
		}
		for _, a := range assets {
			if n, ok := strings.CutSuffix(strings.TrimPrefix(a.Path, "simple/"), "/index"); ok {
				set[n] = true
			}
		}
		docs, err := h.Store.DB.ListAssets(ctx, rp.Name, "pypi/")
		if err != nil {
			return err
		}
		for _, a := range docs { // pypi/<project>/json and pypi/<project>/<version>/json
			if n, _, ok := strings.Cut(strings.TrimPrefix(a.Path, "pypi/"), "/"); ok && strings.HasSuffix(a.Path, "/json") {
				set[n] = true
			}
		}
	case "group":
		for _, m := range rp.OnlineMembers() {
			if err := h.collectProjects(ctx, m, set); err != nil {
				return err
			}
		}
	}
	return nil
}

// rootKey is where a proxy caches its remote's root page.
const rootKey = "upstream/simple/index"

// remoteProjects gives emit the projects of the root pages of the proxies rp
// reaches, without the known ones (hosted and cached projects, listed
// already): a proxy's list is its remote's, as in Nexus. pypi.org's page is
// about 48 MB and 900 000 projects, so it is only fetched when the list is
// asked for and never held in memory. A remote without such a page, or that
// cannot be reached, leaves the known projects, so the list always answers:
// only emit's error, a failed write, is returned.
// ponytail: a group with two proxies that have the same project lists it
// twice, which clients take; a set of the names does not fit the 128 MiB.
func (h *Handler) remoteProjects(ctx context.Context, rp *repo.Repository, known map[string]bool, emit func(string) error) error {
	switch rp.Type {
	case "group":
		for _, m := range rp.OnlineMembers() {
			if err := h.remoteProjects(ctx, m, known, emit); err != nil {
				return err
			}
		}
	case "proxy":
		res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: rootKey, Remote: "simple/", Header: http.Header{"Accept": {"text/html"}}}, proxy.Metadata)
		if err != nil {
			if repo.StatusOf(err) != http.StatusNotFound {
				slog.Warn("pypi proxy root page unavailable: listing the cached projects", "repo", rp.Name, "err", logsafe.Err(err))
			}
			return nil
		}
		f, err := h.Store.Open(res.Asset)
		if err != nil {
			slog.Warn("pypi proxy root page unavailable: listing the cached projects", "repo", rp.Name, "err", logsafe.Err(err))
			return nil
		}
		defer f.Close()
		var werr error
		err = parseHTMLIndex(f, rp.RemoteURL.ResolveReference(&url.URL{Path: "simple/"}), func(p File) error {
			if name := Normalize(p.Filename); ValidName(p.Filename) && !known[name] {
				werr = emit(name)
			}
			return werr
		})
		if werr != nil {
			return werr
		}
		if err != nil {
			slog.Warn("pypi proxy root page is not readable to its end", "repo", rp.Name, "err", logsafe.Err(err))
		}
	}
	return nil
}

// baseURL is the current external URL without a trailing slash.
func (h *Handler) baseURL() string {
	if h.BaseURL == nil {
		return ""
	}
	return strings.TrimRight(h.BaseURL(), "/")
}
