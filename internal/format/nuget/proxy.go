package nuget

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// fetchFile fetches a metadata document of rp's remote into key, within maxDoc.
func (h *Handler) fetchFile(ctx context.Context, rp *repo.Repository, key, url string) (proxy.Result, error) {
	t := proxy.Target{Key: key, URL: url, Header: http.Header{"Accept": {ctJSON}}, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if err != nil {
		return res, err
	}
	if res.Asset.Size > maxDoc {
		return res, repo.Errorf(http.StatusBadGateway, "upstream document %s is over %d MiB", res.Asset.Path, maxDoc>>20)
	}
	return res, nil
}

// fetchDoc fetches a metadata document of rp's remote into key and reads it.
func (h *Handler) fetchDoc(ctx context.Context, rp *repo.Repository, key, url string) (doc, error) {
	res, err := h.fetchFile(ctx, rp, key, url)
	if err != nil {
		return doc{}, err
	}
	raw, err := h.readDoc(res.Asset)
	return doc{body: raw, res: res}, err
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
	res, err := h.fetchFile(ctx, rp, regPrefix+rt.ID+"/"+rt.Rest, up.reg+rt.ID+"/"+rt.Rest)
	if err != nil {
		return doc{}, err
	}
	return h.rewritten(res, up, h.repoURL(serving), fmt.Sprintf("registration of %q", rt.ID)), nil
}

// searchFile forwards a search to the upstream, the answer cached under a key
// made of the query, for metadataMaxAge.
// ponytail: every distinct query leaves a small cached asset under query/.
func (h *Handler) searchFile(ctx context.Context, rp *repo.Repository, q query) (proxy.Result, upstream, error) {
	up, _, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return proxy.Result{}, up, err
	}
	if up.search == "" {
		return proxy.Result{}, up, repo.Errorf(http.StatusBadGateway, "remote repository has no SearchQueryService resource")
	}
	qs := q.encode()
	sum := sha256.Sum256([]byte(qs))
	sep := "?"
	if strings.Contains(up.search, "?") {
		sep = "&"
	}
	res, err := h.fetchFile(ctx, rp, "query/"+hex.EncodeToString(sum[:8])+".json", up.search+sep+qs)
	return res, up, err
}

// proxySearch serves a proxy's search answer with its URLs rewritten to serving.
func (h *Handler) proxySearch(ctx context.Context, serving, rp *repo.Repository, q query) (doc, error) {
	res, up, err := h.searchFile(ctx, rp, q)
	if err != nil {
		return doc{}, err
	}
	return h.rewritten(res, up, h.repoURL(serving), "search result"), nil
}

// rewritten is a cached upstream document written through rewriteJSON.
func (h *Handler) rewritten(res proxy.Result, up upstream, base, what string) doc {
	return doc{res: res, write: func(w io.Writer) error {
		f, err := h.Store.Open(res.Asset)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := rewriteJSON(w, f, up, base); err != nil {
			return repo.Errorf(http.StatusBadGateway, "upstream %s is not JSON: %v", what, err)
		}
		return nil
	}}
}

// writeUpstreamHits reads a proxy member's cached search answer as a stream,
// one hit at a time, and writes those onPage takes with their URLs rewritten.
func (h *Handler) writeUpstreamHits(w io.Writer, src searchSource, base string, onPage func(id string) bool) error {
	f, err := h.Store.Open(src.file.Asset)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if _, err := dec.Token(); err != nil { // the opening brace
		return err
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		if key != "data" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if _, err := dec.Token(); err != nil { // the opening bracket
			return err
		}
		for dec.More() {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return err
			}
			var hit struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &hit); err != nil {
				return err
			}
			if !onPage(hit.ID) {
				continue
			}
			if err := rewriteJSON(w, bytes.NewReader(raw), src.up, base); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	return nil
}

// maxJSONDepth bounds the nesting rewriteJSON follows (the decoder keeps a
// stack entry per level, so a document of brackets must not grow it).
const maxJSONDepth = 64

// rewriteJSON copies the JSON document src to w token by token, holding none
// of it, and replaces in every string value an upstream registration or flat
// container base with the proxy's (base), so clients follow registration
// pages, dependency registrations and packageContent through the proxy.
// Everything else (catalog URLs, say) is kept. Decoding whole documents here
// took a 128 MiB server down: at 64 clients on registration pages, and at 2
// on a search with take=1000.
func rewriteJSON(w io.Writer, src io.Reader, up upstream, base string) error {
	type frame struct {
		object bool
		n      int // the keys and values (object) or elements (array) so far
	}
	dec := json.NewDecoder(src)
	dec.UseNumber()
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			if _, err := io.WriteString(w, d.String()); err != nil {
				return err
			}
			if stack = stack[:len(stack)-1]; len(stack) == 0 {
				return nil
			}
			continue
		}
		key := false
		if len(stack) > 0 {
			f := &stack[len(stack)-1]
			key = f.object && f.n%2 == 0
			if f.n > 0 && (key || !f.object) {
				io.WriteString(w, ",")
			}
			f.n++
		}
		switch v := tok.(type) {
		case json.Delim:
			if len(stack) == maxJSONDepth {
				return fmt.Errorf("nesting deeper than %d", maxJSONDepth)
			}
			io.WriteString(w, v.String())
			stack = append(stack, frame{object: v == '{'})
			continue
		case string:
			if !key {
				tok = up.toProxy(v, base)
			}
		}
		b, err := json.Marshal(tok)
		if err != nil {
			return err
		}
		if key {
			b = append(b, ':')
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if len(stack) == 0 {
			return nil
		}
	}
}

// toProxy is s with an upstream registration or flat container base replaced
// by the proxy's.
func (up upstream) toProxy(s, base string) string {
	for _, b := range up.regs {
		if rest, ok := strings.CutPrefix(s, b); ok {
			return base + "/" + regPrefix + rest
		}
	}
	for _, b := range up.flats {
		if rest, ok := strings.CutPrefix(s, b); ok {
			return base + "/" + flatPrefix + rest
		}
	}
	return s
}
