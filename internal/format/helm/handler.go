package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves Helm chart repositories. Reads need Read, chart uploads
// Add and a DELETE Delete (the server's default rule); overwriting a
// version under writePolicy allow needs Edit, checked here as Maven does.
type Handler struct {
	Store     *repo.Store
	Fetcher   *proxy.Fetcher
	Auth      *auth.Authenticator
	Now       func() time.Time
	upstreams sync.Map           // proxy name -> *upstream
	reads     singleflight.Group // upstreamFor: one read of an index, however many requests wait for it
	rechecks  sync.Map           // proxy name -> time.Time: when a miss last made it ask its remote for the index (relisted)
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
	if get && rt.Kind != KindAPI {
		res, err := h.download(r.Context(), rp, rt.File, rt.Kind == KindProv)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		res.Asset.ContentType = ctChart
		if rt.Kind == KindProv {
			res.Asset.ContentType = ctProv
		}
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	}
	reads, writes := "", []string{http.MethodPost}
	if rt.Kind != KindAPI {
		reads, writes = "GET, HEAD", []string{http.MethodPut, http.MethodDelete}
	}
	allow := reads
	if repo.AcceptsWrites(rp) { // a proxy, or a group without writeMember, takes no upload and no DELETE
		allow = strings.TrimPrefix(reads+", "+strings.Join(writes, ", "), ", ")
	}
	if !slices.Contains(writes, r.Method) {
		return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
	}
	target, err := repo.WriteTarget(rp)
	if err != nil {
		return repo.WithAllow(err, allow)
	}
	if r.Method == http.MethodDelete {
		return h.remove(w, r, target, rt)
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

// download finds chart file in rp, or with prov its provenance file. A
// proxy fetches the chart from the URL its upstream index gives, verified
// against the index digest (the provenance file from that URL with ".prov"),
// and caches it as content: contentMaxAge and invalidate-cache make it ask
// the remote again, and so does an index that lists another digest for the
// cached chart (the version was published again). A cached file the index
// no longer lists, or whose index cannot be read, is still served. The
// proxy's routing rule sees the request path, before the cache is read. A
// group asks its online members in order; a member that owns the chart's
// name answers for it, so a version it does not have is not found even when
// a later member has one of that name (the owner rule of the merged index).
func (h *Handler) download(ctx context.Context, rp *repo.Repository, file string, prov bool) (proxy.Result, error) {
	key := file
	if prov {
		key = provKey(file)
	}
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, key)
		return proxy.Result{Asset: a}, err
	case "proxy":
		if !h.routed(rp, key) {
			return proxy.Result{}, repo.ErrNotFound
		}
		up, uerr := h.upstreamFor(ctx, rp)
		c, listed := upstreamChart{}, false
		if uerr == nil {
			c, listed = up.charts[file]
		}
		if !listed {
			a, err := h.Store.Get(ctx, rp.Name, key)
			if err == nil || !errors.Is(err, repo.ErrNotFound) {
				return proxy.Result{Asset: a}, err
			}
			if uerr == nil {
				c, listed, uerr = h.relisted(ctx, rp, file)
			}
			switch {
			case uerr != nil:
				return proxy.Result{}, uerr
			case !listed:
				return proxy.Result{}, repo.ErrNotFound
			}
		}
		t := proxy.Target{Key: key, URL: c.url, Route: key, NotHTML: true}
		if prov {
			u, err := url.Parse(c.url)
			if err != nil {
				return proxy.Result{}, repo.ErrNotFound
			}
			if u.Path += provExt; u.RawPath != "" {
				u.RawPath += provExt
			}
			t.URL = u.String()
		} else if proxy.IsSHA256Hex(c.digest) {
			t.SHA256 = c.digest
		}
		res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
		if err == nil && !h.current(res.Asset, c, prov) {
			res, err = h.Fetcher.FetchTarget(ctx, asking(rp), t, proxy.Content)
		}
		if repo.StatusOf(err) == http.StatusBadGateway { // the remote's file is not what its index lists
			if a, gerr := h.Store.Get(ctx, rp.Name, key); gerr == nil {
				return proxy.Result{Asset: a, Stale: true}, nil
			}
		}
		return res, err
	case "group":
		name := chartName(file)
		for _, m := range rp.OnlineMembers() {
			if m.Type == "proxy" && !h.routed(m, key) {
				continue // its routing rule: the member does not answer for this path, nor own it
			}
			res, err := h.download(ctx, m, file, prov)
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

// routed reports whether proxy rp's routing rule lets the request path
// through to its remote (repo.Registry.RouteAllowed).
func (h *Handler) routed(rp *repo.Repository, path string) bool {
	return h.Fetcher.RouteAllowed == nil || h.Fetcher.RouteAllowed(rp, "/"+path)
}

// asking is rp with its max ages off: a fetch through it asks the remote
// whatever the age of the cached copy, which is still served when the remote
// fails.
func asking(rp *repo.Repository) *repo.Repository {
	c := *rp
	c.MetadataMaxAge, c.ContentMaxAge = config.Zero, config.Zero
	return &c
}

// current reports whether a, a proxy's cached chart or provenance file, is
// the one the upstream index lists now: the chart by its digest, the
// provenance file by the digest it signs. An index entry without a sha256
// digest says nothing, and neither does a provenance file that cannot be
// read.
func (h *Handler) current(a meta.Asset, c upstreamChart, prov bool) bool {
	if !proxy.IsSHA256Hex(c.digest) {
		return true
	}
	if !prov {
		return strings.EqualFold(a.BlobSHA256, c.digest)
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return true
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, maxProv))
	_, signed, err := provFile(b)
	return err != nil || strings.EqualFold(signed, c.digest)
}

// recheck is how often a request for a chart file a proxy's index does not
// list makes it ask the remote for the index again.
const recheck = time.Minute

// relisted looks for file in the upstream index as the remote has it now:
// a version published since the cached index was read is not in it for up
// to metadataMaxAge. The remote is asked at most once per recheck and
// node, however many such requests there are.
func (h *Handler) relisted(ctx context.Context, rp *repo.Repository, file string) (upstreamChart, bool, error) {
	now := h.Now()
	if last, ok := h.rechecks.Load(rp.Name); ok && now.Sub(last.(time.Time)) < recheck {
		return upstreamChart{}, false, nil
	}
	h.rechecks.Store(rp.Name, now)
	up, err := h.upstreamFor(ctx, asking(rp))
	if err != nil {
		return upstreamChart{}, false, err
	}
	c, ok := up.charts[file]
	return c, ok, nil
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
