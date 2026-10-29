package helm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves Helm chart repositories. Reads need Read, chart uploads
// Add (the server's default rule); overwriting a version under writePolicy
// allow needs Edit, checked here as Maven does.
type Handler struct {
	Store     *repo.Store
	Fetcher   *proxy.Fetcher
	Auth      *auth.Authenticator
	Now       func() time.Time
	upstreams sync.Map // proxy name -> *upstream
}

func New(store *repo.Store, fetcher *proxy.Fetcher, authn *auth.Authenticator) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, Auth: authn, Now: time.Now}
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
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	if rt.Kind == KindIndex {
		if !get {
			return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
		}
		return h.index(w, r, rp)
	}
	if get && rt.Kind == KindChart {
		res, err := h.download(r.Context(), rp, rt.File)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		res.Asset.ContentType = ctChart
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	}
	want, allow := http.MethodPost, "POST"
	if rt.Kind == KindChart {
		want, allow = http.MethodPut, "GET, HEAD, PUT"
	}
	if !repo.AcceptsWrites(rp) {
		allow = strings.TrimSuffix(strings.TrimSuffix(allow, want), ", ") // a proxy, or a group without writeMember, takes no upload
	}
	if r.Method != want {
		return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
	}
	target, err := repo.WriteTarget(rp)
	if err != nil {
		return repo.WithAllow(err, allow)
	}
	return h.upload(w, r, target, rt)
}

// index serves rp's index.yaml; a hosted repository nothing was published
// to yet answers an empty one, so `helm repo add` works before the first
// push. A proxy whose upstream has none answers the upstream's 404 (or 502),
// so a wrong remote URL shows instead of an empty repository.
func (h *Handler) index(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	res, err := h.indexAsset(r.Context(), rp)
	if errors.Is(err, repo.ErrNotFound) && rp.Type == "hosted" {
		body, err := newIndex(h.Now()).marshal()
		if err != nil {
			return err
		}
		repo.ServeText(w, r, string(body), ctYAML, time.Time{})
		return nil
	}
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, res)
	res.Asset.ContentType = ctYAML
	repo.ServeAsset(w, r, h.Store, res.Asset)
	return nil
}

// download finds chart file in rp. A proxy fetches it from the URL its
// upstream index gives, verified against the index digest, and caches it. A
// group asks its online members in order; a member that owns the chart's
// name answers for it, so a version it does not have is not found even when
// a later member has one of that name (the owner rule of the merged index).
func (h *Handler) download(ctx context.Context, rp *repo.Repository, file string) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, file)
		return proxy.Result{Asset: a}, err
	case "proxy":
		a, err := h.Store.Get(ctx, rp.Name, file)
		if err == nil {
			return proxy.Result{Asset: a}, nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return proxy.Result{}, err
		}
		up, err := h.upstreamFor(ctx, rp)
		if err != nil {
			return proxy.Result{}, err
		}
		c, ok := up.charts[file]
		if !ok {
			return proxy.Result{}, repo.ErrNotFound
		}
		t := proxy.Target{Key: file, URL: c.url, NotHTML: true}
		if proxy.IsSHA256Hex(c.digest) {
			t.SHA256 = c.digest
		}
		return h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
	case "group":
		name := chartName(file)
		for _, m := range rp.OnlineMembers() {
			res, err := h.download(ctx, m, file)
			if err == nil {
				return res, nil
			}
			if repo.StatusOf(err) != http.StatusNotFound {
				return proxy.Result{}, memberFailure(rp, m, err)
			}
			if name != "" {
				owner, err := h.owns(ctx, m, name)
				if err != nil {
					return proxy.Result{}, memberFailure(rp, m, err)
				}
				if owner {
					return proxy.Result{}, repo.ErrNotFound
				}
			}
		}
		return proxy.Result{}, repo.ErrNotFound
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// memberFailure logs a group member's error and returns it as a 5xx, as
// cargo groups do: a later (public) member never stands in for a failing one.
func memberFailure(group, m *repo.Repository, err error) error {
	slog.Error("helm group member failed", "group", group.Name, "member", m.Name, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}
