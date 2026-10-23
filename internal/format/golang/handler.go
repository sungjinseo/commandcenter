package golang

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// Handler serves Go module proxies.
type Handler struct {
	Store    *repo.Store
	Resolver *resolve.Resolver
	Now      func() time.Time
	locks    repo.KeyedMutex // serializes uploads per repository and module version

	// Auth checks overwrites for Edit; nil leaves every check to the server.
	Auth *auth.Authenticator
}

func New(store *repo.Store, resolver *resolve.Resolver) *Handler {
	return &Handler{Store: store, Resolver: resolver, Now: time.Now}
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	rt, err := ParseRoute(path)
	if err == nil {
		err = h.serve(w, r, rp, rt)
	} else if errors.Is(err, errBadVersion) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		// The go command only falls back on 404/410, never on 400.
		err = repo.Errorf(http.StatusNotFound, "not found: %s", path)
	}
	if err != nil {
		repo.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPut:
		if rt.Query {
			return errBadVersion
		}
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, "GET, HEAD")
		}
		return h.upload(w, r, target, rt)
	default:
		allow := "GET, HEAD"
		if !rt.Query && (rt.Kind == KindMod || rt.Kind == KindZip || rt.Kind == KindInfo) && repo.AcceptsWrites(rp) {
			allow += ", PUT"
		}
		return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
	}
	ctx := r.Context()
	if rt.Query {
		res, err := h.query(ctx, rp, rt)
		if err != nil {
			return err
		}
		return h.serveAsset(w, r, res, KindInfo)
	}
	switch rt.Kind {
	case KindList:
		versions, found, stale, err := h.list(ctx, rp, rt.Module)
		if err != nil {
			return err
		}
		if !found {
			return repo.Errorf(http.StatusNotFound, "module %s not found", rt.Module)
		}
		proxy.SetStaleHeader(w, proxy.Result{Stale: stale})
		body := ""
		if len(versions) > 0 {
			body = strings.Join(versions, "\n") + "\n"
		}
		repo.ServeText(w, r, body, contentType(KindList), time.Time{})
		return nil
	case KindLatest:
		res, err := h.latest(ctx, rp, rt.Module)
		if err != nil {
			return err
		}
		return h.serveAsset(w, r, res, KindLatest)
	}
	res, err := h.Resolver.Lookup(ctx, rp, assetKey(rt.Module, rt.Version+ext(rt.Kind)), proxy.Content)
	if err != nil {
		return err
	}
	return h.serveAsset(w, r, res, rt.Kind)
}

// query answers {module}/@v/{query}.info for a non-canonical version such as
// "master": only a remote can resolve it, so hosted repositories answer 404.
func (h *Handler) query(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	switch rp.Type {
	case "proxy":
		return h.Resolver.Fetcher.Fetch(ctx, rp, assetKey(rt.Module, rt.Version+".info"), proxy.Metadata)
	case "group":
		var firstErr error
		for _, m := range rp.OnlineMembers() {
			res, err := h.query(ctx, m, rt)
			if err == nil {
				return res, nil
			}
			if repo.StatusOf(err) != http.StatusNotFound && firstErr == nil {
				firstErr = err
			}
		}
		if firstErr != nil {
			return proxy.Result{}, firstErr
		}
	}
	return proxy.Result{}, repo.Errorf(http.StatusNotFound, "version %s of %s not found", rt.Version, rt.Module)
}

func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, res proxy.Result, k Kind) error {
	proxy.SetStaleHeader(w, res)
	a := res.Asset
	a.ContentType = contentType(k)
	repo.ServeAsset(w, r, h.Store, a)
	return nil
}

// list returns the module's versions in rp, sorted. found is false when the
// module is unknown (a remote that answers with an empty list still knows it);
// stale is set when a remote list came from an expired cache entry.
func (h *Handler) list(ctx context.Context, rp *repo.Repository, module string) (versions []string, found, stale bool, err error) {
	return h.listVersions(ctx, rp, module, false)
}

// listVersions is list; withPseudo keeps hosted pseudo-versions, which @latest
// needs as a last resort when a module has nothing else.
func (h *Handler) listVersions(ctx context.Context, rp *repo.Repository, module string, withPseudo bool) (versions []string, found, stale bool, err error) {
	switch rp.Type {
	case "hosted":
		assets, err := h.Store.DB.ListAssets(ctx, rp.Name, assetKey(module, ""))
		if err != nil {
			return nil, false, false, err
		}
		known := false
		for _, a := range assets {
			file := strings.TrimPrefix(a.Path, assetKey(module, ""))
			// a version exists once its .zip does; a .mod alone is not listed
			v, ok := strings.CutSuffix(file, ".zip")
			if !ok || strings.Contains(v, "/") {
				continue
			}
			if u, err := Unescape(v); err == nil {
				known = true
				if !withPseudo && isPseudoVersion(u) {
					continue // pseudo-versions stay fetchable but are not listed
				}
				versions = append(versions, u)
			}
		}
		return sortVersions(versions), known, false, nil
	case "proxy":
		res, err := h.Resolver.Fetcher.Fetch(ctx, rp, assetKey(module, "list"), proxy.Metadata)
		if repo.StatusOf(err) == http.StatusNotFound {
			return nil, false, false, nil
		}
		if err != nil {
			return nil, false, false, err
		}
		vs, err := h.readList(res.Asset)
		return vs, err == nil, res.Stale, err
	case "group":
		var all []string
		var firstErr error
		for _, m := range rp.OnlineMembers() {
			vs, f, st, err := h.listVersions(ctx, m, module, withPseudo)
			if err != nil {
				slog.Warn("go group member list failed", "group", rp.Name, "member", m.Name, "module", module, "err", logsafe.Err(err))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			all = append(all, vs...)
			found = found || f
			stale = stale || st
		}
		if !found && firstErr != nil {
			return nil, false, false, firstErr
		}
		return sortVersions(all), found, stale, nil
	}
	return nil, false, false, fmt.Errorf("unknown repository type %q", rp.Type)
}

func (h *Handler) readList(a meta.Asset) ([]string, error) {
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var versions []string
	cr := &countReader{r: io.LimitReader(f, maxDoc+1)}
	sc := bufio.NewScanner(cr)
	sc.Buffer(make([]byte, 0, 4<<10), 64<<10)
	for sc.Scan() {
		// some proxies append a timestamp after the version
		if fields := strings.Fields(sc.Text()); len(fields) > 0 && versionRE.MatchString(fields[0]) {
			versions = append(versions, fields[0])
		}
	}
	if cr.n > maxDoc {
		return nil, repo.Errorf(http.StatusBadGateway, "version list exceeds %d bytes", maxDoc)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, repo.Errorf(http.StatusBadGateway, "version list has a line longer than 64 KiB")
		}
		return nil, err
	}
	return sortVersions(versions), nil
}

// latest answers @latest: the .info of the highest listed version, or, when no
// version is listed, the first member whose remote answers @latest itself.
func (h *Handler) latest(ctx context.Context, rp *repo.Repository, module string) (proxy.Result, error) {
	versions, _, _, err := h.list(ctx, rp, module)
	if err != nil {
		return proxy.Result{}, err
	}
	if v := pickLatest(versions); v != "" {
		return h.Resolver.Lookup(ctx, rp, assetKey(module, Escape(v)+".info"), proxy.Content)
	}
	if rp.Type == "hosted" {
		// Only pseudo-versions exist: like go, fall back to the newest one.
		all, _, _, err := h.listVersions(ctx, rp, module, true)
		if err != nil {
			return proxy.Result{}, err
		}
		if n := len(all); n > 0 {
			return h.Resolver.Lookup(ctx, rp, assetKey(module, Escape(all[n-1])+".info"), proxy.Content)
		}
	}
	switch rp.Type {
	case "proxy":
		return h.Resolver.Fetcher.Fetch(ctx, rp, module+"/@latest", proxy.Metadata)
	case "group":
		var firstErr error
		for _, m := range rp.OnlineMembers() {
			res, err := h.latest(ctx, m, module)
			if err == nil {
				return res, nil
			}
			if repo.StatusOf(err) != http.StatusNotFound && firstErr == nil {
				firstErr = err
			}
		}
		if firstErr != nil {
			return proxy.Result{}, firstErr
		}
	}
	return proxy.Result{}, repo.Errorf(http.StatusNotFound, "module %s has no versions", module)
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
