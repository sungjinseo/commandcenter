package rubygems

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves RubyGems repositories. Reads need Read, a push Add and a
// yank Delete (the server's default rule); overwriting a version under
// writePolicy allow needs Edit, checked here as Helm does.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	Auth    *auth.Authenticator
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry // a group's merged versions and names, by group name and document
}

// cacheEntry is a merged group document, good for groupCacheTTL for one
// incarnation of the group.
type cacheEntry struct {
	gen   uint64
	at    time.Time
	body  []byte
	stale bool
}

const (
	groupCacheTTL = time.Minute
	// maxIndex bounds a versions, names, info or dependencies document read
	// whole (rubygems.org's versions file is some 20 MiB).
	maxIndex = 256 << 20
	// maxDependencies bounds the gems one dependency request may name.
	maxDependencies = 200
)

func New(store *repo.Store, fetcher *proxy.Fetcher, authn *auth.Authenticator) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, Auth: authn, Now: time.Now, cache: map[string]cacheEntry{}}
}

// doc is a generated or cached document and, for a proxy, the cached
// upstream asset it came from (its staleness and modification time).
type doc struct {
	body []byte
	res  proxy.Result
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
	if rt.Kind == KindPush || rt.Kind == KindYank {
		want := http.MethodPost
		if rt.Kind == KindYank {
			want = http.MethodDelete
		}
		allow := want
		if !repo.AcceptsWrites(rp) {
			allow = "" // a proxy, or a group without writeMember, accepts no method here
		}
		if r.Method != want {
			return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
		}
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allow)
		}
		if rt.Kind == KindYank {
			return h.yank(w, r, target)
		}
		return h.push(w, r, target)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
	}
	ctx := r.Context()
	switch rt.Kind {
	case KindVersions:
		if rp.Type == "group" {
			d, err := h.groupVersions(ctx, rp)
			if err != nil {
				return err
			}
			serveDoc(w, r, d, ctText, h.Now())
			return nil
		}
		res, err := h.versionsAsset(ctx, rp)
		if err != nil {
			return err
		}
		return h.serveAssetDoc(w, r, res)
	case KindNames:
		d, err := h.names(ctx, rp)
		if err != nil {
			return err
		}
		serveDoc(w, r, d, ctText, h.Now())
		return nil
	case KindInfo:
		d, err := h.info(ctx, rp, rt.Name)
		if err != nil {
			return err
		}
		if d.body == nil { // a proxy's cached copy
			return h.serveAssetDoc(w, r, d.res)
		}
		serveDoc(w, r, d, ctText, h.Now())
		return nil
	case KindSpecs:
		d, err := h.specs(ctx, rp, rt.File)
		if err != nil {
			return err
		}
		if d.body != nil {
			repo.ServeText(w, r, string(d.body), ctGzip, h.Now())
			return nil
		}
		proxy.SetStaleHeader(w, d.res)
		d.res.Asset.ContentType = ctGzip
		repo.ServeAsset(w, r, h.Store, d.res.Asset)
		return nil
	case KindGemspec, KindGem:
		res, body, err := h.file(ctx, rp, rt)
		if err != nil {
			return err
		}
		if body != nil {
			repo.ServeText(w, r, string(body), ctBinary, h.Now())
			return nil
		}
		proxy.SetStaleHeader(w, res)
		res.Asset.ContentType = ctBinary
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	case KindDependencies:
		names, err := parseGems(r.URL.Query().Get("gems"))
		if err != nil {
			return err
		}
		entries, res, err := h.dependencies(ctx, rp, names)
		if err != nil {
			return err
		}
		body, err := dependenciesDoc(entries)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		repo.ServeText(w, r, string(body), ctBinary, h.Now())
		return nil
	}
	return fmt.Errorf("unknown route kind %d", rt.Kind)
}

// serveDoc serves a generated document with the validators Bundler checks
// when it fetches increments: the ETag is the md5 of the whole body (older
// Bundler compares it to its file's), Repr-Digest its sha-256 (newer
// Bundler verifies it after appending a Range answer).
func serveDoc(w http.ResponseWriter, r *http.Request, d doc, ct string, modTime time.Time) {
	sum := sha256.Sum256(d.body)
	setValidators(w, md5hex(d.body), hex.EncodeToString(sum[:]))
	proxy.SetStaleHeader(w, d.res)
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, "", modTime, bytes.NewReader(d.body))
}

func setValidators(w http.ResponseWriter, md5Hex, sha256Hex string) {
	if md5Hex != "" {
		w.Header().Set("ETag", `"`+md5Hex+`"`)
	}
	if raw, err := hex.DecodeString(sha256Hex); err == nil && len(raw) == sha256.Size {
		w.Header().Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(raw)+":")
	}
}

// serveAssetDoc serves a stored compact index document (a hosted versions
// file, a proxy's cached copy) from its blob, with Range and the validators
// of serveDoc, so Bundler fetches only what was appended.
func (h *Handler) serveAssetDoc(w http.ResponseWriter, r *http.Request, res proxy.Result) error {
	a := res.Asset
	if a.Size > maxIndex {
		return repo.Errorf(http.StatusBadGateway, "document %s is over %d MiB", a.Path, maxIndex>>20)
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return err
	}
	defer f.Close()
	setValidators(w, a.MD5, a.BlobSHA256)
	proxy.SetStaleHeader(w, res)
	w.Header().Set("Content-Type", ctText)
	http.ServeContent(w, r, "", a.LastModified, f)
	return nil
}

// readDoc reads a stored document whole, within maxIndex.
func (h *Handler) readDoc(a meta.Asset) ([]byte, error) {
	if a.Size > maxIndex {
		return nil, repo.Errorf(http.StatusBadGateway, "document %s is over %d MiB", a.Path, maxIndex>>20)
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxIndex))
}

// fetch fetches path of rp's remote into the cache under the same path.
func (h *Handler) fetch(ctx context.Context, rp *repo.Repository, path string, kind proxy.Kind) (proxy.Result, error) {
	return h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: path, Remote: path, NotHTML: true}, kind)
}

// fetchDoc fetches a metadata document of rp's remote and reads it.
func (h *Handler) fetchDoc(ctx context.Context, rp *repo.Repository, path string) (doc, error) {
	res, err := h.fetch(ctx, rp, path, proxy.Metadata)
	if err != nil {
		return doc{}, err
	}
	body, err := h.readDoc(res.Asset)
	return doc{body, res}, err
}

// versionsAsset is the stored versions file of a hosted repository (built
// from its rows the first time) or a proxy's cached copy of its remote's.
func (h *Handler) versionsAsset(ctx context.Context, rp *repo.Repository) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, versionsKey)
		if errors.Is(err, repo.ErrNotFound) {
			unlock, lerr := h.Store.Lock(ctx, "rubygems\x00"+rp.Name)
			if lerr != nil {
				return proxy.Result{}, lerr
			}
			defer unlock()
			if err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error { return h.writeVersions(ctx, tx, rp, "", "", false) }); err != nil {
				return proxy.Result{}, err
			}
			a, err = h.Store.Get(ctx, rp.Name, versionsKey)
		}
		return proxy.Result{Asset: a}, err
	case "proxy":
		return h.fetch(ctx, rp, versionsKey, proxy.Metadata)
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// versionsBody is rp's versions file read whole: a group's merge, else
// the asset's content.
func (h *Handler) versionsBody(ctx context.Context, rp *repo.Repository) (doc, error) {
	if rp.Type == "group" {
		return h.groupVersions(ctx, rp)
	}
	res, err := h.versionsAsset(ctx, rp)
	if err != nil {
		return doc{}, err
	}
	body, err := h.readDoc(res.Asset)
	return doc{body, res}, err
}

// groupVersions merges the online members' versions files (mergeVersions),
// cached for groupCacheTTL per incarnation of the group. The result is not
// append-only across members' changes: Bundler notices by the validators
// and downloads the whole file again.
func (h *Handler) groupVersions(ctx context.Context, rp *repo.Repository) (doc, error) {
	return h.cached(rp, versionsKey, func() (doc, error) {
		var bodies [][]byte
		var res proxy.Result
		for _, m := range rp.OnlineMembers() {
			d, err := h.versionsBody(ctx, m)
			if err != nil {
				return doc{}, memberFailure(rp, m, err)
			}
			bodies = append(bodies, d.body)
			res.Stale = res.Stale || d.res.Stale
		}
		return doc{mergeVersions(bodies, h.Now()), res}, nil
	})
}

func (h *Handler) cached(rp *repo.Repository, key string, build func() (doc, error)) (doc, error) {
	k := rp.Name + "\x00" + key
	now := h.Now()
	h.mu.Lock()
	e, ok := h.cache[k]
	h.mu.Unlock()
	if ok && e.gen == rp.Gen && now.Sub(e.at) < groupCacheTTL {
		return doc{e.body, proxy.Result{Stale: e.stale}}, nil
	}
	d, err := build()
	if err != nil {
		return doc{}, err
	}
	h.mu.Lock()
	h.cache[k] = cacheEntry{gen: rp.Gen, at: now, body: d.body, stale: d.res.Stale}
	h.mu.Unlock()
	return d, nil
}

// names is the names file: a hosted repository's gems, a proxy's cached
// copy, or the union of a group's members' (cached as the versions are).
func (h *Handler) names(ctx context.Context, rp *repo.Repository) (doc, error) {
	switch rp.Type {
	case "hosted":
		names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
		return doc{body: namesDoc(names)}, err
	case "proxy":
		return h.fetchDoc(ctx, rp, namesKey)
	case "group":
		return h.cached(rp, namesKey, func() (doc, error) {
			seen := map[string]bool{}
			var names []string
			var res proxy.Result
			for _, m := range rp.OnlineMembers() {
				d, err := h.names(ctx, m)
				if err != nil {
					return doc{}, memberFailure(rp, m, err)
				}
				res.Stale = res.Stale || d.res.Stale
				for _, n := range parseNames(d.body) {
					if !seen[n] {
						seen[n] = true
						names = append(names, n)
					}
				}
			}
			return doc{namesDoc(names), res}, nil
		})
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// info is info/<name>: derived from a hosted repository's rows, a proxy's
// cached copy, or the owning member's.
func (h *Handler) info(ctx context.Context, rp *repo.Repository, name string) (doc, error) {
	switch rp.Type {
	case "hosted":
		rs, err := rows(ctx, h.Store.DB, rp.Name, name)
		if err != nil {
			return doc{}, err
		}
		if len(rs) == 0 {
			return doc{}, repo.ErrNotFound
		}
		return doc{body: infoDoc(rs)}, nil
	case "proxy":
		res, err := h.fetch(ctx, rp, infoPrefix+name, proxy.Metadata)
		return doc{res: res}, err
	case "group":
		owner, err := h.owner(ctx, rp, name)
		if err != nil {
			return doc{}, err
		}
		return h.info(ctx, owner, name)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// specs is one of the legacy specs indexes: built from a hosted
// repository's rows (a body, served from memory), a proxy's cached copy (a
// result), or the first member's that has one (not merged).
// ponytail: merging members' specs means decoding rubygems.org's 1.5M-entry
// index per group generation; gem since 3.2 resolves from the compact
// index, which groups do merge.
func (h *Handler) specs(ctx context.Context, rp *repo.Repository, file string) (doc, error) {
	switch rp.Type {
	case "hosted":
		body, err := h.hostedSpecs(ctx, rp, file)
		return doc{body: body}, err
	case "proxy":
		res, err := h.fetch(ctx, rp, file, proxy.Metadata)
		return doc{res: res}, err
	case "group":
		for _, m := range rp.OnlineMembers() {
			d, err := h.specs(ctx, m, file)
			if errors.Is(err, repo.ErrNotFound) {
				continue
			}
			if err != nil {
				return doc{}, memberFailure(rp, m, err)
			}
			return d, nil
		}
		return doc{}, repo.ErrNotFound
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// hostedSpecs builds a specs index from every row of a hosted repository.
func (h *Handler) hostedSpecs(ctx context.Context, rp *repo.Repository, file string) ([]byte, error) {
	names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return nil, err
	}
	var entries []specEntry
	for _, name := range names {
		rs, err := rows(ctx, h.Store.DB, rp.Name, name)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			entries = append(entries, specEntry{name, r.version, r.platform})
		}
	}
	return specsDoc(file, entries)
}

// file finds a gem (gems/<full>.gem) or its gemspec: stored (hosted; the
// gemspec is built from the row), cached or fetched (proxy), or the owning
// member's (group). A built gemspec comes back as body, the rest as a result.
func (h *Handler) file(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, []byte, error) {
	switch rp.Type {
	case "hosted":
		if rt.Kind == KindGem {
			a, err := h.Store.Get(ctx, rp.Name, gemFile(rt.Full))
			return proxy.Result{Asset: a}, nil, err
		}
		for _, c := range candidates(rt.Full) {
			rs, err := rows(ctx, h.Store.DB, rp.Name, c[0])
			if err != nil {
				return proxy.Result{}, nil, err
			}
			for _, r := range rs {
				if r.pkg.Version == c[1] {
					body, err := gemspecRz(r.attrs)
					return proxy.Result{}, body, err
				}
			}
		}
		return proxy.Result{}, nil, repo.ErrNotFound
	case "proxy":
		path := gemFile(rt.Full)
		if rt.Kind == KindGemspec {
			path = quickPrefix + rt.Full + ".gemspec.rz"
		}
		res, err := h.fetch(ctx, rp, path, proxy.Content)
		return res, nil, err
	case "group":
		for _, c := range candidates(rt.Full) {
			owner, err := h.owner(ctx, rp, c[0])
			if errors.Is(err, repo.ErrNotFound) {
				continue
			}
			if err != nil {
				return proxy.Result{}, nil, err
			}
			return h.file(ctx, owner, rt)
		}
		return proxy.Result{}, nil, repo.ErrNotFound
	}
	return proxy.Result{}, nil, fmt.Errorf("unknown repository type %q", rp.Type)
}

// gemspecRz is quick/Marshal.4.8/<full>.gemspec.rz: the Marshal-dumped
// Gem::Specification, zlib-deflated.
func gemspecRz(a gemAttrs) ([]byte, error) {
	spec, err := gemSpecification(a)
	if err != nil {
		return nil, err
	}
	raw, err := marshalDump(spec)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(raw)
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parseGems reads the gems= parameter of a dependency request: distinct
// gem names, sorted (they key a proxy's cache).
func parseGems(param string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, n := range strings.Split(param, ",") {
		if n = strings.TrimSpace(n); n == "" || seen[n] {
			continue
		}
		if !nameRE.MatchString(n) {
			return nil, repo.Errorf(http.StatusBadRequest, "invalid gem name %q", n)
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) > maxDependencies {
		return nil, repo.Errorf(http.StatusBadRequest, "at most %d gems per request", maxDependencies)
	}
	sort.Strings(names)
	return names, nil
}

// dependencies answers api/v1/dependencies for names: every version of
// each gem with its runtime dependencies, from the rows (hosted), the
// remote's answer (proxy, cached for metadataMaxAge under a key made of the
// names) or the members in order, each asked for the gems none before it
// had (group).
func (h *Handler) dependencies(ctx context.Context, rp *repo.Repository, names []string) ([]any, proxy.Result, error) {
	entries := []any{}
	switch rp.Type {
	case "hosted":
		for _, name := range names {
			rs, err := rows(ctx, h.Store.DB, rp.Name, name)
			if err != nil {
				return nil, proxy.Result{}, err
			}
			for _, r := range rs {
				entries = append(entries, dependencyEntry(r))
			}
		}
		return entries, proxy.Result{}, nil
	case "proxy":
		if len(names) == 0 {
			return entries, proxy.Result{}, nil
		}
		joined := strings.Join(names, ",")
		sum := sha256.Sum256([]byte(joined))
		t := proxy.Target{Key: depsPrefix + hex.EncodeToString(sum[:8]), URL: rp.RemoteURL.String() + "api/v1/dependencies?gems=" + url.QueryEscape(joined), NotHTML: true}
		res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
		if err != nil {
			return nil, res, err
		}
		raw, err := h.readDoc(res.Asset)
		if err != nil {
			return nil, res, err
		}
		v, err := marshalLoad(raw)
		arr, ok := v.([]any)
		if err != nil || !ok {
			return nil, res, repo.Errorf(http.StatusBadGateway, "remote dependency answer is not a Marshal array")
		}
		return arr, res, nil
	case "group":
		var res proxy.Result
		remaining := names
		for _, m := range rp.OnlineMembers() {
			if len(remaining) == 0 {
				break
			}
			got, r, err := h.dependencies(ctx, m, remaining)
			if err != nil {
				return nil, res, memberFailure(rp, m, err)
			}
			res.Stale = res.Stale || r.Stale
			has := map[string]bool{}
			for _, e := range got {
				if hv, ok := e.(hash); ok {
					for _, kv := range hv {
						if kv.key == sym("name") {
							if n, ok := kv.val.(string); ok {
								has[n] = true
							}
						}
					}
				}
				entries = append(entries, e)
			}
			var rest []string
			for _, n := range remaining {
				if !has[n] {
					rest = append(rest, n)
				}
			}
			remaining = rest
		}
		return entries, res, nil
	}
	return nil, proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// owner returns the member of group rp that answers for gem name: the
// first online member, in order, that has it (hosted: a row; proxy: an
// upstream info file; group: its own owner). That member answers alone, so
// a later (public) member never stands in for an internal gem.
func (h *Handler) owner(ctx context.Context, rp *repo.Repository, name string) (*repo.Repository, error) {
	for _, m := range rp.OnlineMembers() {
		has, err := h.has(ctx, m, name)
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		if has {
			return m, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (h *Handler) has(ctx context.Context, rp *repo.Repository, name string) (bool, error) {
	var err error
	switch rp.Type {
	case "hosted":
		var pkgs []meta.Package
		pkgs, err = h.Store.DB.ListPackages(ctx, rp.Name, name)
		return len(pkgs) > 0, err
	case "proxy":
		_, err = h.fetch(ctx, rp, infoPrefix+name, proxy.Metadata)
	case "group":
		_, err = h.owner(ctx, rp, name)
	default:
		return false, fmt.Errorf("unknown repository type %q", rp.Type)
	}
	if errors.Is(err, repo.ErrNotFound) || repo.StatusOf(err) == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// memberFailure logs a group member's error and returns it as a 5xx, as
// helm groups do: a later (public) member never stands in for a failing one.
func memberFailure(group, m *repo.Repository, err error) error {
	slog.Error("rubygems group member failed", "group", group.Name, "member", m.Name, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}
