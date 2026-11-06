package composer

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	advisoryURL string // its security advisories API; "" when it has none
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
	if sa, ok := up.root["security-advisories"].(map[string]any); ok {
		api, _ := sa["api-url"].(string)
		up.advisoryURL = absolute(rp, api)
	}
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
	return doc{body: raw, res: res}, err
}

// keptRootKeys are what a proxy's packages.json keeps of the upstream's: inline
// packages and notices. The rest is the v1 provider protocol, download
// notifications and the upstream's own APIs, and what a later Packagist adds
// (the filter lists' summary-url made composer audit fail on a closed network),
// which would point clients past the proxy.
var keptRootKeys = []string{"packages", "minified", "available-packages", "available-package-patterns", "warning", "warning-versions", "info", "info-versions", "warnings", "infos"}

// proxyRoot is the upstream packages.json with metadata-url and search
// pointing at the proxy. Inline packages (a Satis repository without
// providers) are passed through as they are. It has no Last-Modified: it is
// also made of what the upstream's file does not say (the routing rule, this
// build), and Composer revalidates with If-Modified-Since, so a client that
// got it from a build without the advisories would keep that copy for as long
// as the upstream's file does not change.
func (h *Handler) proxyRoot(ctx context.Context, rp *repo.Repository) (doc, error) {
	up, res, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return doc{}, err
	}
	out := map[string]any{}
	for _, k := range keptRootKeys {
		if v, ok := up.root[k]; ok {
			out[k] = v
		}
	}
	if inline, _ := out["packages"].(map[string]any); len(inline) == 0 {
		delete(out, "minified") // it describes the inline packages only
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
	if up.advisoryURL != "" && h.routeAllowed(rp, advisoriesPath) {
		out["security-advisories"] = advisoryConfig(base)
	}
	body, err := json.Marshal(out)
	return doc{body: body, res: proxy.Result{Stale: res.Stale}}, err
}

// metadataFile fetches (or finds in the cache) the upstream metadata file of
// name. The file is not read: Packagist's largest are over 1 MiB, and holding
// one per request took a 128 MiB server down at 8 clients at once.
func (h *Handler) metadataFile(ctx context.Context, rp *repo.Repository, name string, dev bool) (proxy.Result, error) {
	up, _, err := h.upstreamFor(ctx, rp)
	if err != nil {
		return proxy.Result{}, err
	}
	pkg := name
	if dev {
		pkg += "~dev"
	}
	t := proxy.Target{Key: metadataPath(name, dev), URL: fill(up.metadataURL, pkg, "", ""), Header: http.Header{"Accept": {ctJSON}}, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if err != nil {
		return res, err
	}
	if res.Asset.Size > maxDoc {
		return res, repo.Errorf(http.StatusBadGateway, "upstream document %s is over %d MiB", res.Asset.Path, maxDoc>>20)
	}
	return res, nil
}

// rawEntry is one version as a metadata file lists it; a minified file lists
// only the fields that differ from the version before, "__unset" marking a
// removed one.
type rawEntry = map[string]json.RawMessage

// errStop ends eachVersion early.
var errStop = errors.New("stop")

// eachVersion reads the metadata file r as a stream and calls fn with each
// version of name in it, holding one at a time. It returns the file's
// "minified" value, which comes before or after the versions.
func eachVersion(r io.Reader, name string, fn func(rawEntry) error) (minified string, err error) {
	dec := json.NewDecoder(r)
	if _, err := dec.Token(); err != nil { // the opening brace
		return "", err
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return minified, err
		}
		switch key {
		case "minified":
			err = dec.Decode(&minified)
		case "packages":
			err = eachPackageVersion(dec, name, fn)
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return minified, err
		}
	}
	_, err = dec.Token()
	return minified, err
}

// eachPackageVersion reads the value of "packages": an object of version
// lists, or the empty array PHP writes for an empty one.
func eachPackageVersion(dec *json.Decoder, name string, fn func(rawEntry) error) error {
	open, err := dec.Token()
	if err != nil {
		return err
	}
	if open == json.Delim('[') {
		for dec.More() {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	if open != json.Delim('{') {
		return fmt.Errorf("packages is %v, not an object", open)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		if key != name {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if open, err := dec.Token(); err != nil {
			return err
		} else if open != json.Delim('[') {
			return fmt.Errorf("the versions of %s are %v, not an array", name, open)
		}
		for dec.More() {
			var e rawEntry
			if err := dec.Decode(&e); err != nil {
				return err
			}
			if err := fn(e); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// proxyMetadata serves the upstream metadata of a package with every dist
// URL rewritten to serving (dist/<name>/<version>/<sha1 of the upstream
// URL>.zip) and the source entries dropped, so Composer never reaches
// past the proxy or into git. The file stays minified as the upstream wrote
// it: Composer expands it, as it does Packagist's.
func (h *Handler) proxyMetadata(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, error) {
	res, err := h.metadataFile(ctx, rp, rt.Name, rt.Dev)
	if err != nil {
		return doc{}, err
	}
	base := h.repoURL(serving)
	return doc{res: res, write: func(w io.Writer) error {
		f, err := h.Store.Open(res.Asset)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := writeMetadata(w, f, rt.Name, base); err != nil {
			return repo.Errorf(http.StatusBadGateway, "upstream metadata of %q is not JSON: %v", rt.Name, err)
		}
		return nil
	}}, nil
}

// writeMetadata copies the versions of name in the metadata file src to w,
// rewriting them for the proxy at base.
func writeMetadata(w io.Writer, src io.Reader, name, base string) error {
	head, err := json.Marshal(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, `{"packages":{%s:[`, head)
	normalized, n := "", 0
	minified, err := eachVersion(src, name, func(e rawEntry) error {
		delete(e, "source")
		normalized = entryVersion(e, normalized)
		if err := rewriteDist(e, base+"/"+distPrefix+name+"/"+normalized+"/"); err != nil {
			return err
		}
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if n++; n > 1 {
			io.WriteString(w, ",")
		}
		_, err = w.Write(b)
		return err
	})
	if err != nil {
		return err
	}
	io.WriteString(w, "]}")
	if minified != "" {
		m, err := json.Marshal(minified)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, `,"minified":%s`, m)
	}
	_, err = io.WriteString(w, "}")
	return err
}

// entryVersion is the normalized version of an entry: its version_normalized,
// derived from version when the upstream left that out, or the previous
// entry's when a minified entry has neither.
func entryVersion(e rawEntry, previous string) string {
	var v string
	if json.Unmarshal(e["version_normalized"], &v) == nil && v != "" {
		return v
	}
	if json.Unmarshal(e["version"], &v) != nil || v == "" {
		return previous
	}
	if n, ok := Normalize(v); ok {
		return n
	}
	return v
}

// distURL is the part of an entry's dist the proxy uses. Unmarshal fails on
// "__unset" and leaves it empty for a null.
type distURL struct {
	URL    string `json:"url"`
	Shasum string `json:"shasum"`
}

// distOf returns an entry's dist, if it has one with a URL, and the sha1 of
// that URL (the proxy's file name for it).
func distOf(e rawEntry) (d distURL, ref string, ok bool) {
	if json.Unmarshal(e["dist"], &d) != nil || d.URL == "" {
		return distURL{}, "", false
	}
	sum := sha1.Sum([]byte(d.URL))
	return d, hex.EncodeToString(sum[:]), true
}

// rewriteDist points the entry's dist, if it has one with a URL, at prefix
// and the sha1 of the upstream URL.
func rewriteDist(e rawEntry, prefix string) error {
	_, ref, ok := distOf(e)
	if !ok {
		return nil
	}
	var d rawEntry
	if err := json.Unmarshal(e["dist"], &d); err != nil {
		return err
	}
	u, err := json.Marshal(prefix + ref + ".zip")
	if err != nil {
		return err
	}
	d["url"] = u
	e["dist"], err = json.Marshal(d)
	return err
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
	file, err := h.metadataFile(ctx, rp, rt.Name, IsDev(rt.Version))
	if err != nil {
		return proxy.Result{}, err
	}
	f, err := h.Store.Open(file.Asset)
	if err != nil {
		return proxy.Result{}, err
	}
	defer f.Close()
	var found distURL
	_, err = eachVersion(f, rt.Name, func(e rawEntry) error {
		if d, ref, ok := distOf(e); ok && ref == rt.Ref {
			found = d
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return proxy.Result{}, repo.Errorf(http.StatusBadGateway, "upstream metadata of %q is not JSON: %v", rt.Name, err)
	}
	if found.URL == "" {
		return proxy.Result{}, repo.ErrNotFound
	}
	res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: key, URL: found.URL, NotHTML: true}, proxy.Content)
	if err != nil {
		return proxy.Result{}, err
	}
	if found.Shasum != "" && !strings.EqualFold(found.Shasum, res.Asset.SHA1) {
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
