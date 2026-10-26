package pypi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves Python package indexes.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	BaseURL func() string // live: read per request (plan 12)
	Now     func() time.Time
	locks   repo.KeyedMutex // serializes uploads per repository and project
}

func New(store *repo.Store, fetcher *proxy.Fetcher, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, BaseURL: baseURL, Now: time.Now}
}

// fileAttrs is the packages.attrs document of a hosted file.
type fileAttrs struct {
	Filename       string `json:"filename"`
	Version        string `json:"version"`
	SHA256         string `json:"sha256"`
	RequiresPython string `json:"requires_python,omitempty"`
}

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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
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
		return writeProjects(w, wantsJSON(r.Header.Get("Accept")), names)
	case KindProject:
		return h.project(w, r, rp, rt.Name)
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

func (h *Handler) fileURL(rp *repo.Repository, project, file string) string {
	return h.baseURL() + "/repository/" + rp.Name + "/packages/" + project + "/" + url.PathEscape(file)
}

func (h *Handler) project(w http.ResponseWriter, r *http.Request, rp *repo.Repository, name string) error {
	ren := newRenderer(w, wantsJSON(r.Header.Get("Accept")), name)
	seen := map[string]bool{}
	err := h.walk(r.Context(), rp, name, func(f File) error {
		if seen[f.Filename] {
			return nil
		}
		seen[f.Filename] = true
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

func indexTarget(project string) proxy.Target {
	return proxy.Target{
		Key:    "simple/" + project + "/index",
		Remote: "simple/" + project + "/",
		Header: http.Header{"Accept": {jsonType + ", text/html;q=0.1"}},
	}
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
			if err := fn(File{Filename: a.Filename, SHA256: a.SHA256, RequiresPython: a.RequiresPython}); err != nil {
				return err
			}
		}
		return nil
	case "proxy":
		res, err := h.Fetcher.FetchTarget(ctx, rp, indexTarget(project), proxy.Metadata)
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
		base := rp.RemoteURL.ResolveReference(&url.URL{Path: "simple/" + project + "/"})
		return parseIndex(f, res.Asset.ContentType, base, fn)
	case "group":
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

// owner returns the repository whose files answer for project in rp: rp
// itself if it lists a file of project, or for a group the first member in
// order (nested groups searched in place) that does. As in Cargo groups, that
// member answers alone: files of a later (public) member never mix in, so its
// release cannot stand in for an internal one (dependency confusion, QA
// N-1). Only "not found" moves on; a member ahead of the owner that is
// offline (503) or fails (5xx) fails the request.
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
	for _, m := range rp.MemberRepos {
		if !m.IsOnline() {
			return nil, repo.Errorf(http.StatusServiceUnavailable, "group member %q is offline", m.Name)
		}
		o, err := h.owner(ctx, m, project)
		if err == nil {
			return o, nil
		}
		if repo.StatusOf(err) != http.StatusNotFound {
			return nil, memberFailure(rp, m, project, err)
		}
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

// file returns a distribution file. Proxies find the upstream URL in the
// project's cached index and verify the download against its sha256.
func (h *Handler) file(ctx context.Context, rp *repo.Repository, project, filename string) (proxy.Result, error) {
	key := fileKey(project, filename)
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, key)
		return proxy.Result{Asset: a}, err
	case "proxy":
		a, err := h.Store.Get(ctx, rp.Name, key)
		if err == nil {
			return proxy.Result{Asset: a}, nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
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
		if !ok || found.URL == "" {
			return proxy.Result{}, repo.ErrNotFound
		}
		res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: key, URL: found.URL, SHA256: found.SHA256}, proxy.Content)
		if err != nil {
			return res, err
		}
		return res, nil
	case "group":
		// Only the owner's files, even by direct URL (QA N-1).
		m, err := h.owner(ctx, rp, project)
		if err != nil {
			return proxy.Result{}, err
		}
		return h.file(ctx, m, project, filename)
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
	case "group":
		for _, m := range rp.OnlineMembers() {
			if err := h.collectProjects(ctx, m, set); err != nil {
				return err
			}
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
