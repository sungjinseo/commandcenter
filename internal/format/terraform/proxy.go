package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// maxDoc bounds an upstream JSON document read back from the cache.
const maxDoc = 16 << 20

// services is what a proxy learned from its remote's discovery document:
// the absolute base URLs of the modules and providers services.
type services struct {
	modules, providers string
}

// discover reads the remote's .well-known/terraform.json (cached for
// metadataMaxAge). A remote without one is a 502: a wrong remote URL.
func (h *Handler) discover(ctx context.Context, rp *repo.Repository) (services, error) {
	u := *rp.RemoteURL
	u.Path = "/" + discoveryPath
	t := proxy.Target{Key: discoveryPath, URL: u.String(), Header: http.Header{"Accept": {ctJSON}}, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if errors.Is(err, repo.ErrNotFound) {
		return services{}, repo.Errorf(http.StatusBadGateway, "remote registry has no %s at %s", discoveryPath, u.String())
	}
	if err != nil {
		return services{}, err
	}
	raw, err := h.readDoc(res.Asset)
	if err != nil {
		return services{}, err
	}
	var d struct {
		Modules   string `json:"modules.v1"`
		Providers string `json:"providers.v1"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return services{}, repo.Errorf(http.StatusBadGateway, "remote %s is not JSON: %v", discoveryPath, err)
	}
	s := services{modules: absolute(&u, d.Modules), providers: absolute(&u, d.Providers)}
	if s.modules == "" && s.providers == "" {
		return services{}, repo.Errorf(http.StatusBadGateway, "remote %s lists neither modules.v1 nor providers.v1", discoveryPath)
	}
	return s, nil
}

// absolute resolves a service URL against the discovery document's URL,
// with a trailing slash so paths append.
func absolute(base *url.URL, s string) string {
	if s == "" {
		return ""
	}
	ref, err := url.Parse(s)
	if err != nil {
		return ""
	}
	out := base.ResolveReference(ref).String()
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// serviceURL is the upstream URL of rt's versions or download endpoint.
func (h *Handler) serviceURL(ctx context.Context, rp *repo.Repository, rt Route) (string, error) {
	s, err := h.discover(ctx, rp)
	if err != nil {
		return "", err
	}
	modules := isModule(rt.Kind)
	base := s.providers
	if modules {
		base = s.modules
	}
	if base == "" {
		return "", repo.Errorf(http.StatusBadGateway, "remote registry serves no %s", map[bool]string{true: "modules.v1", false: "providers.v1"}[modules])
	}
	return base + rt.Name + "/", nil
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

// fetchDoc fetches a JSON document of rp's remote into key and reads it.
func (h *Handler) fetchDoc(ctx context.Context, rp *repo.Repository, key, url string) (doc, error) {
	t := proxy.Target{Key: key, URL: url, Header: http.Header{"Accept": {ctJSON}}, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if err != nil {
		return doc{}, err
	}
	raw, err := h.readDoc(res.Asset)
	return doc{raw, res}, err
}

// proxyVersions passes the upstream version list through (cached under
// the request path for metadataMaxAge).
func (h *Handler) proxyVersions(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	u, err := h.serviceURL(ctx, rp, rt)
	if err != nil {
		return doc{}, err
	}
	prefix := providersPrefix
	if isModule(rt.Kind) { // also a group's owner lookup for a download or an archive
		prefix = modulesPrefix
	}
	return h.fetchDoc(ctx, rp, prefix+rt.Name+"/versions", u+"versions")
}

// source is a module version's upstream X-Terraform-Get, split: key is
// where the proxy caches the archive and ext its format when the source
// is an http(s) archive the proxy can fetch; raw is the header passed
// through unchanged otherwise (git::, ssh, or an unknown format).
type source struct {
	raw, url, key, ext, subdir string
}

// proxyModuleSource asks the upstream download endpoint for a version's
// source on every call (it may be a short-lived signed URL; the archive
// itself is cached once fetched). The last answer is kept at
// v1/modules/<name>/<version>/download and stands in when the remote
// fails, so cached modules install while it is down.
func (h *Handler) proxyModuleSource(ctx context.Context, rp *repo.Repository, rt Route) (source, error) {
	u, err := h.serviceURL(ctx, rp, rt)
	if err != nil {
		return source{}, err
	}
	endpoint := u + rt.Version + "/download"
	key := modulesPrefix + rt.Name + "/" + rt.Version + "/download"
	kept := ""
	if a, err := h.Store.Get(ctx, rp.Name, key); err == nil {
		if b, err := h.readDoc(a); err == nil {
			kept = string(b)
		}
	}
	raw, err := h.upstreamSource(ctx, rp, endpoint)
	switch {
	case err == nil && raw != kept:
		if _, err := h.Store.Put(ctx, rp.Name, key, "text/plain", strings.NewReader(raw)); err != nil {
			return source{}, err
		}
	case err != nil && (kept == "" || errors.Is(err, repo.ErrNotFound)):
		return source{}, err
	case err != nil:
		slog.Warn("terraform module download endpoint failed, using the last source", "repo", rp.Name, "module", rt.Name+"@"+rt.Version, "err", logsafe.Err(err))
		raw = kept
	}
	return parseSource(endpoint, raw, rt), nil
}

// upstreamSource is the X-Terraform-Get of the upstream download endpoint.
func (h *Handler) upstreamSource(ctx context.Context, rp *repo.Repository, endpoint string) (string, error) {
	resp, err := h.Fetcher.Request(ctx, rp, endpoint)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	raw := resp.Header.Get("X-Terraform-Get")
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", repo.ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", proxy.ErrRateLimited
	case resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK, raw == "":
		return "", repo.Errorf(http.StatusBadGateway, "remote download endpoint answered %d without X-Terraform-Get", resp.StatusCode)
	}
	return raw, nil
}

// parseSource reads a go-getter source: a relative URL is resolved against
// the download endpoint; an http(s) URL of a tgz or zip archive (by the
// archive query parameter or the path's extension) is cached under the
// module's archive path, keeping its "//subdir" and query; anything else
// (a forced getter such as git::, another scheme or format) passes through.
func parseSource(endpoint, raw string, rt Route) source {
	s := source{raw: raw}
	if strings.Contains(raw, "::") {
		return s
	}
	if ref, err := url.Parse(raw); err == nil && ref.Scheme == "" {
		if base, err := url.Parse(endpoint); err == nil {
			s.raw = base.ResolveReference(ref).String()
		}
	}
	rest := s.raw
	var query string
	if i := strings.Index(rest, "?"); i >= 0 {
		rest, query = rest[:i], rest[i+1:]
	}
	if !strings.HasPrefix(rest, "https://") && !strings.HasPrefix(rest, "http://") {
		return s
	}
	if i := strings.Index(rest[8:], "//"); i >= 0 {
		rest, s.subdir = rest[:8+i], rest[8+i:]
	}
	q, _ := url.ParseQuery(query)
	ext := q.Get("archive")
	if ext == "" {
		switch {
		case strings.HasSuffix(rest, ".tar.gz"), strings.HasSuffix(rest, ".tgz"):
			ext = "tgz"
		case strings.HasSuffix(rest, ".zip"):
			ext = "zip"
		}
	}
	switch ext {
	case "tar.gz", "tgz":
		ext = "tgz"
	case "zip":
	default:
		return s
	}
	q.Del("archive")
	s.url = rest
	if enc := q.Encode(); enc != "" {
		s.url += "?" + enc
	}
	s.key, s.ext = modulePath(rt.Name, rt.Version, ext), ext
	return s
}

// proxyModuleArchive fetches a module archive from the source the
// upstream names for the version, into key.
func (h *Handler) proxyModuleArchive(ctx context.Context, rp *repo.Repository, rt Route, key string) (proxy.Result, error) {
	s, err := h.proxyModuleSource(ctx, rp, rt)
	if err != nil {
		return proxy.Result{}, err
	}
	if s.key != key {
		return proxy.Result{}, repo.ErrNotFound
	}
	return h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: key, URL: s.url, NotHTML: true}, proxy.Content)
}

// downloadDoc is the upstream provider download document, as read and as
// rewritten.
type downloadDoc map[string]any

func (d downloadDoc) str(k string) string { s, _ := d[k].(string); return s }

// downloadKey is where a proxy caches a platform's download document.
func downloadKey(rt Route) string {
	return providerDir(rt.Name, rt.Version) + "download_" + rt.OS + "_" + rt.Arch + ".json"
}

// proxyDownloadDoc fetches (or reads from the cache) the upstream download
// document of a provider version for one platform.
func (h *Handler) proxyDownloadDoc(ctx context.Context, rp *repo.Repository, rt Route) (downloadDoc, proxy.Result, error) {
	u, err := h.serviceURL(ctx, rp, rt)
	if err != nil {
		return nil, proxy.Result{}, err
	}
	d, err := h.fetchDoc(ctx, rp, downloadKey(rt), u+rt.Version+"/download/"+rt.OS+"/"+rt.Arch)
	if err != nil {
		return nil, d.res, err
	}
	var out downloadDoc
	if err := json.Unmarshal(d.body, &out); err != nil || out == nil {
		return nil, d.res, repo.Errorf(http.StatusBadGateway, "upstream download document of %s %s is not JSON: %v", rt.Name, rt.Version, err)
	}
	return out, d.res, nil
}

// proxyProviderDownload serves the upstream download document with its
// three file URLs rewritten to serving, under the names a hosted
// repository uses (the upstream URLs may be opaque or signed): providers/
// <ns>/<type>/<version>/terraform-provider-<type>_<version>_<os>_<arch>.zip,
// ..._SHA256SUMS and ..._SHA256SUMS.sig. filename and shasum stay as the
// upstream gave them, and terraform checks the zip against both.
func (h *Handler) proxyProviderDownload(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, error) {
	d, res, err := h.proxyDownloadDoc(ctx, rp, rt)
	if err != nil {
		return doc{}, err
	}
	base := h.repoURL(serving) + "/" + providerDir(rt.Name, rt.Version)
	sums := providerFile(rt.Name, rt.Version, "", "")
	for k, file := range map[string]string{"download_url": providerFile(rt.Name, rt.Version, rt.OS, rt.Arch), "shasums_url": sums, "shasums_signature_url": sums + ".sig"} {
		if d.str(k) != "" {
			d[k] = base + file
		}
	}
	body, err := json.Marshal(d)
	return doc{body, res}, err
}

// proxyProviderFile fetches a provider file on demand from the upstream URL
// a download document names for it: a platform's zip from that platform's
// document (its sha256 verified against the document's shasum), the
// SHA256SUMS or its signature from any cached document of the version
// (the client fetched one just before).
func (h *Handler) proxyProviderFile(ctx context.Context, rp *repo.Repository, rt Route, key string) (proxy.Result, error) {
	sums := providerFile(rt.Name, rt.Version, "", "")
	var d downloadDoc
	var field string
	switch os, arch, ok := platformOf(rt.File); {
	case ok && rt.File == providerFile(rt.Name, rt.Version, os, arch):
		rt.OS, rt.Arch, field = os, arch, "download_url"
		var err error
		if d, _, err = h.proxyDownloadDoc(ctx, rp, rt); err != nil {
			return proxy.Result{}, err
		}
	case rt.File == sums:
		field = "shasums_url"
	case rt.File == sums+".sig":
		field = "shasums_signature_url"
	default:
		return proxy.Result{}, repo.ErrNotFound
	}
	if d == nil {
		docs, err := h.Store.DB.ListAssets(ctx, rp.Name, providerDir(rt.Name, rt.Version)+"download_")
		if err != nil {
			return proxy.Result{}, err
		}
		for _, a := range docs {
			var cand downloadDoc
			if raw, err := h.readDoc(a); err == nil && json.Unmarshal(raw, &cand) == nil && cand.str(field) != "" {
				d = cand
				break
			}
		}
	}
	u := d.str(field)
	if u == "" {
		return proxy.Result{}, repo.ErrNotFound
	}
	t := proxy.Target{Key: key, URL: u, NotHTML: true}
	if field == "download_url" {
		t.SHA256 = d.str("shasum")
	}
	return h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
}
