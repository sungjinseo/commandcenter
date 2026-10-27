package nuget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// maxDoc bounds an upstream JSON document read back from the cache (a
// registration page of a package with thousands of versions is a few MiB).
const maxDoc = 16 << 20

// upstream is what a proxy learned from its remote's service index: the
// base URLs it fetches from (the newest of each kind) and every base of
// each kind, which rewrite replaces in the documents.
type upstream struct {
	flat, reg, search string
	flats, regs       []string
}

// serviceIndexURL is where rp's remote service index is: remote itself when
// it names the index.json (https://api.nuget.org/v3/index.json), else
// index.json under it.
func serviceIndexURL(rp *repo.Repository) string {
	u := *rp.RemoteURL
	if strings.HasSuffix(u.Path, ".json/") {
		u.Path = strings.TrimSuffix(u.Path, "/")
	} else {
		u.Path += "index.json"
	}
	return u.String()
}

// upstreamFor reads the remote service index (cached for metadataMaxAge).
// A remote without one is a 502: a wrong remote URL, not a missing package.
func (h *Handler) upstreamFor(ctx context.Context, rp *repo.Repository) (upstream, proxy.Result, error) {
	t := proxy.Target{Key: upstreamKey, URL: serviceIndexURL(rp), Header: http.Header{"Accept": {ctJSON}}, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if errors.Is(err, repo.ErrNotFound) {
		return upstream{}, res, repo.Errorf(http.StatusBadGateway, "remote repository has no NuGet service index at %s", serviceIndexURL(rp))
	}
	if err != nil {
		return upstream{}, res, err
	}
	raw, err := h.readDoc(res.Asset)
	if err != nil {
		return upstream{}, res, err
	}
	var idx struct {
		Resources []struct {
			ID   string `json:"@id"`
			Type string `json:"@type"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return upstream{}, res, repo.Errorf(http.StatusBadGateway, "remote service index is not JSON: %v", err)
	}
	var up upstream
	pick := func(dst *string, all *[]string, prefer ...string) {
		for _, want := range prefer {
			for _, r := range idx.Resources {
				if r.Type == want && *dst == "" {
					*dst = r.ID
				}
			}
		}
		if all == nil {
			return
		}
		for _, r := range idx.Resources {
			if r.Type == prefer[len(prefer)-1] || strings.HasPrefix(r.Type, prefer[len(prefer)-1]+"/") {
				*all = append(*all, withSlash(r.ID))
			}
		}
		sort.Slice(*all, func(i, j int) bool { return len((*all)[i]) > len((*all)[j]) }) // longest base first
	}
	pick(&up.flat, &up.flats, "PackageBaseAddress/3.0.0", "PackageBaseAddress")
	pick(&up.reg, &up.regs, "RegistrationsBaseUrl/3.6.0", "RegistrationsBaseUrl/3.4.0", "RegistrationsBaseUrl/3.0.0-rc", "RegistrationsBaseUrl/3.0.0-beta", "RegistrationsBaseUrl")
	pick(&up.search, nil, "SearchQueryService/3.5.0", "SearchQueryService/3.0.0-rc", "SearchQueryService/3.0.0-beta", "SearchQueryService")
	up.flat, up.reg = withSlash(up.flat), withSlash(up.reg)
	return up, res, nil
}

func withSlash(s string) string {
	if s == "" || strings.HasSuffix(s, "/") {
		return s
	}
	return s + "/"
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

// proxyFlatIndex serves the upstream flat container index of id, as is.
func (h *Handler) proxyFlatIndex(ctx context.Context, rp *repo.Repository, id string) (doc, error) {
	up, _, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return doc{}, err
	}
	if up.flat == "" {
		return doc{}, repo.Errorf(http.StatusBadGateway, "remote repository has no PackageBaseAddress resource")
	}
	return h.fetchDoc(ctx, rp, flatPrefix+id+"/index.json", up.flat+id+"/index.json")
}

// proxyFlatFile serves a cached .nupkg or .nuspec, fetched from the
// upstream flat container the first time.
func (h *Handler) proxyFlatFile(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	key := flatPrefix + assetPath(rt.ID, rt.Version, rt.File)
	a, err := h.Store.Get(ctx, rp.Name, key)
	if err == nil {
		return proxy.Result{Asset: a}, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return proxy.Result{}, err
	}
	up, _, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return proxy.Result{}, err
	}
	if up.flat == "" {
		return proxy.Result{}, repo.Errorf(http.StatusBadGateway, "remote repository has no PackageBaseAddress resource")
	}
	t := proxy.Target{Key: key, URL: up.flat + assetPath(rt.ID, rt.Version, rt.File), NotHTML: true}
	return h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
}

// proxyRegistration serves an upstream registration document (the index,
// a leaf, or a page the upstream split its index into, fetched when the
// client follows its rewritten @id) with its URLs rewritten to serving.
func (h *Handler) proxyRegistration(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, error) {
	up, _, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return doc{}, err
	}
	if up.reg == "" {
		return doc{}, repo.Errorf(http.StatusBadGateway, "remote repository has no RegistrationsBaseUrl resource")
	}
	d, err := h.fetchDoc(ctx, rp, regPrefix+rt.ID+"/"+rt.Rest, up.reg+rt.ID+"/"+rt.Rest)
	if err != nil {
		return doc{}, err
	}
	d.body, err = rewrite(d.body, up, h.repoURL(serving))
	if err != nil {
		return doc{}, repo.Errorf(http.StatusBadGateway, "upstream registration of %q is not JSON: %v", rt.ID, err)
	}
	return d, nil
}

// proxySearch forwards a search to the upstream (cached under a key made
// of the query, for metadataMaxAge) and rewrites its URLs to serving.
// ponytail: every distinct query leaves a small cached asset under query/.
func (h *Handler) proxySearch(ctx context.Context, serving, rp *repo.Repository, q query) (doc, error) {
	up, _, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return doc{}, err
	}
	if up.search == "" {
		return doc{}, repo.Errorf(http.StatusBadGateway, "remote repository has no SearchQueryService resource")
	}
	qs := q.encode()
	sum := sha256.Sum256([]byte(qs))
	sep := "?"
	if strings.Contains(up.search, "?") {
		sep = "&"
	}
	d, err := h.fetchDoc(ctx, rp, "query/"+hex.EncodeToString(sum[:8])+".json", up.search+sep+qs)
	if err != nil {
		return doc{}, err
	}
	d.body, err = rewrite(d.body, up, h.repoURL(serving))
	if err != nil {
		return doc{}, repo.Errorf(http.StatusBadGateway, "upstream search result is not JSON: %v", err)
	}
	return d, nil
}

// rewrite replaces, in every string of the JSON document raw, an upstream
// registration or flat container base with the proxy's (base), so clients
// follow registration pages, dependency registrations and packageContent
// through the proxy. Everything else (catalog URLs, say) is kept.
func rewrite(raw []byte, up upstream, base string) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, e := range t {
				t[k] = walk(e)
			}
		case []any:
			for i, e := range t {
				t[i] = walk(e)
			}
		case string:
			for _, b := range up.regs {
				if rest, ok := strings.CutPrefix(t, b); ok {
					return base + "/" + regPrefix + rest
				}
			}
			for _, b := range up.flats {
				if rest, ok := strings.CutPrefix(t, b); ok {
					return base + "/" + flatPrefix + rest
				}
			}
		}
		return x
	}
	return json.Marshal(walk(v))
}
