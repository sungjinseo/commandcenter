package composer

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// maxDoc bounds an upstream JSON document read back from the cache (the
// metadata of a package with thousands of versions is a few MiB).
const maxDoc = 16 << 20

// upstream is what a proxy learned from its remote's packages.json: the
// root document and the absolute URL templates it fetches from.
type upstream struct {
	root        map[string]any
	metadataURL string // "%package%" to replace
	searchURL   string // "%query%" and "%type%" to replace; "" when the remote has no search
}

// rootURL is where rp's remote packages.json is: remote itself when it
// names the file, else packages.json under it.
func rootURL(rp *repo.Repository) string {
	u := *rp.RemoteURL
	if strings.HasSuffix(u.Path, ".json/") {
		u.Path = strings.TrimSuffix(u.Path, "/")
	} else {
		u.Path += "packages.json"
	}
	return u.String()
}

// absolute resolves a URL template of the remote root against the remote:
// absolute as is, "/path" on the remote's host, else under the remote URL.
// (url.Parse rejects the "%package%" placeholders, so strings do it.)
func absolute(rp *repo.Repository, t string) string {
	switch {
	case t == "" || strings.Contains(t, "://"):
		return t
	case strings.HasPrefix(t, "/"):
		return rp.RemoteURL.Scheme + "://" + rp.RemoteURL.Host + t
	}
	return rp.RemoteURL.String() + t
}

// upstreamFor reads the remote root (cached for metadataMaxAge). A remote
// without one, or without metadata-url (a Composer v1 repository), is a
// 502: a wrong remote URL, not a missing package.
func (h *Handler) upstreamFor(ctx context.Context, rp *repo.Repository) (upstream, proxy.Result, error) {
	t := proxy.Target{Key: upstreamKey, URL: rootURL(rp), Header: http.Header{"Accept": {ctJSON}}, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if errors.Is(err, repo.ErrNotFound) {
		return upstream{}, res, repo.Errorf(http.StatusBadGateway, "remote repository has no packages.json at %s", rootURL(rp))
	}
	if err != nil {
		return upstream{}, res, err
	}
	raw, err := h.readDoc(res.Asset)
	if err != nil {
		return upstream{}, res, err
	}
	var up upstream
	if err := json.Unmarshal(raw, &up.root); err != nil || up.root == nil {
		return upstream{}, res, repo.Errorf(http.StatusBadGateway, "remote packages.json is not a JSON object: %v", err)
	}
	m, _ := up.root["metadata-url"].(string)
	if m == "" {
		return upstream{}, res, repo.Errorf(http.StatusBadGateway, "remote packages.json at %s has no metadata-url (not a Composer v2 repository)", rootURL(rp))
	}
	s, _ := up.root["search"].(string)
	up.metadataURL, up.searchURL = absolute(rp, m), absolute(rp, s)
	return up, res, nil
}

// readDoc reads a cached document whole, within maxDoc.
func (h *Handler) readDoc(a meta.Asset) ([]byte, error) {
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxDoc+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDoc {
		return nil, repo.Errorf(http.StatusBadGateway, "upstream document %s is over %d MiB", a.Path, maxDoc>>20)
	}
	return raw, nil
}

// fetchDoc fetches a metadata document of rp's remote into key and reads it.
func (h *Handler) fetchDoc(ctx context.Context, rp *repo.Repository, key, url string) (doc, error) {
	t := proxy.Target{Key: key, URL: url, Header: http.Header{"Accept": {ctJSON}}, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if err != nil {
		return doc{}, err
	}
	raw, err := h.readDoc(res.Asset)
	return doc{raw, res}, err
}

// droppedRootKeys are what a proxy's packages.json leaves out of the
// upstream's: the v1 provider protocol, download notifications and the
// upstream's own APIs, which would point clients past the proxy.
var droppedRootKeys = []string{"providers-url", "provider-includes", "providers-lazy-url", "providers-api", "notify-batch", "security-advisories", "list", "minified"}

// proxyRoot is the upstream packages.json with metadata-url and search
// pointing at the proxy. Inline packages (a Satis repository without
// providers) are passed through as they are.
func (h *Handler) proxyRoot(ctx context.Context, rp *repo.Repository) (doc, error) {
	up, res, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return doc{}, err
	}
	out := map[string]any{}
	for k, v := range up.root {
		out[k] = v
	}
	inline, _ := out["packages"].(map[string]any)
	for _, k := range droppedRootKeys {
		if k != "minified" || len(inline) == 0 { // minified describes the inline packages only
			delete(out, k)
		}
	}
	base := h.repoURL(rp)
	for k, v := range rootDoc(base) {
		if k == "packages" && out[k] != nil {
			continue
		}
		if k == "search" && up.searchURL == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	body, err := json.Marshal(out)
	return doc{body, res}, err
}

// proxyDoc fetches (or reads from the cache) the upstream metadata of name
// and returns its versions, expanded when the upstream minified them.
func (h *Handler) proxyDoc(ctx context.Context, rp *repo.Repository, name string, dev bool) ([]entry, proxy.Result, error) {
	up, _, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return nil, proxy.Result{}, err
	}
	pkg := name
	if dev {
		pkg += "~dev"
	}
	d, err := h.fetchDoc(ctx, rp, metadataPath(name, dev), fill(up.metadataURL, pkg, "", ""))
	if err != nil {
		return nil, d.res, err
	}
	var parsed struct {
		Packages map[string][]entry `json:"packages"`
		Minified string             `json:"minified"`
	}
	if err := json.Unmarshal(d.body, &parsed); err != nil {
		return nil, d.res, repo.Errorf(http.StatusBadGateway, "upstream metadata of %q is not JSON: %v", name, err)
	}
	versions := parsed.Packages[name]
	if parsed.Minified == "composer/2.0" {
		versions = expand(versions)
	}
	return versions, d.res, nil
}

// expand undoes Composer's metadata minification: each version after the
// first lists only the fields that differ from the one before, "__unset"
// marking a removed field.
func expand(versions []entry) []entry {
	out := make([]entry, 0, len(versions))
	var prev entry
	for _, v := range versions {
		e := entry{}
		for k, val := range prev {
			e[k] = val
		}
		for k, val := range v {
			if s, ok := val.(string); ok && s == "__unset" {
				delete(e, k)
				continue
			}
			e[k] = val
		}
		out = append(out, e)
		prev = e
	}
	return out
}

// proxyMetadata serves the upstream metadata of a package with every dist
// URL rewritten to serving (dist/<name>/<version>/<sha1 of the upstream
// URL>.zip) and the source entries dropped, so Composer never reaches
// past the proxy or into git.
func (h *Handler) proxyMetadata(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, error) {
	versions, res, err := h.proxyDoc(ctx, rp, rt.Name, rt.Dev)
	if err != nil {
		return doc{}, err
	}
	base := h.repoURL(serving)
	out := make([]entry, 0, len(versions))
	for _, e := range versions {
		delete(e, "source")
		if _, ref, ok := distRef(e); ok {
			d := entry{} // a copy: expanded entries may share one dist map
			for k, v := range e["dist"].(map[string]any) {
				d[k] = v
			}
			d["url"] = base + "/" + distPrefix + rt.Name + "/" + normalizedOf(e) + "/" + ref + ".zip"
			e["dist"] = d
		}
		out = append(out, e)
	}
	body, err := json.Marshal(map[string]any{"packages": map[string]any{rt.Name: out}})
	return doc{body, res}, err
}

// distRef returns an entry's upstream dist URL and its sha1 (the proxy's
// file name for it).
func distRef(e entry) (url, ref string, ok bool) {
	d, _ := e["dist"].(map[string]any)
	url, _ = d["url"].(string)
	if url == "" {
		return "", "", false
	}
	sum := sha1.Sum([]byte(url))
	return url, hex.EncodeToString(sum[:]), true
}

// normalizedOf is an entry's version_normalized, derived from version when
// the upstream left it out.
func normalizedOf(e entry) string {
	if v, _ := e["version_normalized"].(string); v != "" {
		return v
	}
	v, _ := e["version"].(string)
	if n, ok := Normalize(v); ok {
		return n
	}
	return v
}

// proxyDist serves a cached archive, fetched from the upstream dist URL
// the cached metadata records for rt.Ref the first time. An upstream
// shasum (sha1), when given, must match the download.
func (h *Handler) proxyDist(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	key := distPrefix + rt.Name + "/" + rt.Version + "/" + rt.Ref + ".zip"
	a, err := h.Store.Get(ctx, rp.Name, key)
	if err == nil {
		return proxy.Result{Asset: a}, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return proxy.Result{}, err
	}
	versions, _, err := h.proxyDoc(ctx, rp, rt.Name, IsDev(rt.Version))
	if err != nil {
		return proxy.Result{}, err
	}
	var upstreamURL, shasum string
	for _, e := range versions {
		if u, ref, ok := distRef(e); ok && ref == rt.Ref {
			d := e["dist"].(map[string]any)
			shasum, _ = d["shasum"].(string)
			upstreamURL = u
			break
		}
	}
	if upstreamURL == "" {
		return proxy.Result{}, repo.ErrNotFound
	}
	res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: key, URL: upstreamURL, NotHTML: true}, proxy.Content)
	if err != nil {
		return proxy.Result{}, err
	}
	if shasum != "" && !strings.EqualFold(shasum, res.Asset.SHA1) {
		if err := h.Store.Delete(ctx, rp.Name, key); err != nil && !errors.Is(err, repo.ErrNotFound) {
			return proxy.Result{}, err
		}
		return proxy.Result{}, repo.Errorf(http.StatusBadGateway, "upstream archive of %s %s does not match its shasum", rt.Name, rt.Version)
	}
	return res, nil
}

// proxySearch forwards a search to the upstream (cached under a key made
// of the query, for metadataMaxAge) and passes its answer through.
// ponytail: every distinct query leaves a small cached asset under search/.
func (h *Handler) proxySearch(ctx context.Context, rp *repo.Repository, q, typ string) (doc, error) {
	up, _, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return doc{}, err
	}
	if up.searchURL == "" {
		return doc{}, repo.Errorf(http.StatusBadGateway, "remote repository has no search")
	}
	u := fill(up.searchURL, "", q, typ)
	sum := sha256.Sum256([]byte(u))
	return h.fetchDoc(ctx, rp, "search/"+hex.EncodeToString(sum[:8])+".json", u)
}
