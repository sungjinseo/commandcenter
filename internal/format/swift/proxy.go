package swift

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// maxDoc bounds an upstream JSON document read back from the cache.
const maxDoc = 16 << 20

// fetchDoc fetches a document of rp's remote (remote is the path under
// the registry root, or an absolute URL) into key and reads it.
func (h *Handler) fetchDoc(ctx context.Context, rp *repo.Repository, key, remote, accept string) (doc, error) {
	t := proxy.Target{Key: key, Header: http.Header{"Accept": {accept}}, NotHTML: true}
	if strings.Contains(remote, "://") {
		t.URL = remote
	} else {
		t.Remote = remote
	}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if err != nil {
		return doc{}, err
	}
	raw, err := h.readDoc(res.Asset)
	return doc{raw, res}, err
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

// proxyList serves the upstream release list with every release URL
// rewritten to serving, so SwiftPM never reaches past the proxy. The
// upstream's other keys of a release (problem) are kept.
func (h *Handler) proxyList(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, []string, error) {
	d, err := h.fetchDoc(ctx, rp, strings.ToLower(rt.Scope+"/"+rt.Name)+"/"+releasesFile, rt.Scope+"/"+rt.Name, ctRegistry+"+json")
	if errors.Is(err, repo.ErrNotFound) {
		return doc{}, nil, notFoundPackage(rt)
	}
	if err != nil {
		return doc{}, nil, err
	}
	var parsed struct {
		Releases map[string]map[string]any `json:"releases"`
	}
	if err := json.Unmarshal(d.body, &parsed); err != nil || parsed.Releases == nil {
		return doc{}, nil, repo.Errorf(http.StatusBadGateway, "upstream release list of %s.%s is not JSON: %v", rt.Scope, rt.Name, err)
	}
	versions := make([]string, 0, len(parsed.Releases))
	for v, rel := range parsed.Releases {
		if rel == nil {
			rel = map[string]any{}
		}
		rel["url"] = h.releaseURL(serving, rt, v)
		parsed.Releases[v] = rel
		versions = append(versions, v)
	}
	sortVersions(versions)
	body, err := json.Marshal(map[string]any{"releases": parsed.Releases})
	return doc{body, d.res}, versions, err
}

// proxyRelease passes the upstream release metadata through (it carries
// no URLs), cached for metadataMaxAge.
func (h *Handler) proxyRelease(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	d, err := h.fetchDoc(ctx, rp, versionDir(rt.Scope, rt.Name, rt.Version)+releaseFile, rt.Scope+"/"+rt.Name+"/"+rt.Version, ctRegistry+"+json")
	if errors.Is(err, repo.ErrNotFound) {
		return doc{}, notFoundRelease(rt)
	}
	return d, err
}

// upstreamChecksum is the archive's sha256 the upstream release metadata
// states ("" when it states none).
func (h *Handler) upstreamChecksum(ctx context.Context, rp *repo.Repository, rt Route) (string, error) {
	d, err := h.proxyRelease(ctx, rp, rt)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Resources []struct {
			Name, Checksum string
		} `json:"resources"`
	}
	if err := json.Unmarshal(d.body, &parsed); err != nil {
		return "", repo.Errorf(http.StatusBadGateway, "upstream release metadata of %s.%s %s is not JSON: %v", rt.Scope, rt.Name, rt.Version, err)
	}
	for _, r := range parsed.Resources {
		if r.Name == "source-archive" && proxy.IsSHA256Hex(r.Checksum) {
			return strings.ToLower(r.Checksum), nil
		}
	}
	return "", nil
}

// proxyArchive serves a cached archive, fetched from the upstream the
// first time and checked against the checksum its release metadata states
// (a mismatch is a 502 and nothing is cached).
func (h *Handler) proxyArchive(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	key := archivePath(rt.Scope, rt.Name, rt.Version)
	a, err := h.Store.Get(ctx, rp.Name, key)
	if err == nil {
		return proxy.Result{Asset: a}, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return proxy.Result{}, err
	}
	sum, err := h.upstreamChecksum(ctx, rp, rt)
	if err != nil {
		return proxy.Result{}, err
	}
	t := proxy.Target{Key: key, Remote: rt.Scope + "/" + rt.Name + "/" + rt.Version + ".zip", Header: http.Header{"Accept": {ctRegistry + "+zip"}}, SHA256: sum, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
	if errors.Is(err, repo.ErrNotFound) {
		return proxy.Result{}, notFoundRelease(rt)
	}
	return res, err
}

// proxyIdentifiers forwards identifiers?url= to the upstream (cached under
// a key made of the URL, for metadataMaxAge). A 404 is no identifiers.
func (h *Handler) proxyIdentifiers(ctx context.Context, rp *repo.Repository, u string) ([]string, proxy.Result, error) {
	sum := sha256.Sum256([]byte(normalizeURL(u)))
	d, err := h.fetchDoc(ctx, rp, "identifiers/"+hex.EncodeToString(sum[:8])+".json", rp.RemoteURL.String()+"identifiers?url="+url.QueryEscape(u), ctRegistry+"+json")
	if errors.Is(err, repo.ErrNotFound) {
		return nil, d.res, nil
	}
	if err != nil {
		return nil, d.res, err
	}
	var parsed struct {
		Identifiers []string `json:"identifiers"`
	}
	if err := json.Unmarshal(d.body, &parsed); err != nil {
		return nil, d.res, repo.Errorf(http.StatusBadGateway, "upstream identifiers answer is not JSON: %v", err)
	}
	return parsed.Identifiers, d.res, nil
}
