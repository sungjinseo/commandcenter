package helm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// maxIndex bounds an index.yaml read back from the store (bitnami's is
// ~30 MB; a hostile upstream could send anything).
const maxIndex = 256 << 20

// indexFile is index.yaml.
type indexFile struct {
	APIVersion string             `yaml:"apiVersion"`
	Entries    map[string][]entry `yaml:"entries"`
	Generated  time.Time          `yaml:"generated"`
}

func newIndex(now time.Time) *indexFile {
	return &indexFile{APIVersion: "v1", Entries: map[string][]entry{}, Generated: now.UTC()}
}

func (ix *indexFile) marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(ix); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// parseIndex reads an index.yaml. ponytail: yaml.v3 holds the whole document
// in memory; it runs once per change of a source (indexAsset), never per GET.
func parseIndex(r io.Reader) (*indexFile, error) {
	var ix indexFile
	if err := yaml.NewDecoder(io.LimitReader(r, maxIndex)).Decode(&ix); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if ix.Entries == nil {
		ix.Entries = map[string][]entry{}
	}
	return &ix, nil
}

// sortVersions orders a chart's versions newest first, as helm repo index does.
func sortVersions(vs []entry) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, okA := semver.Parse(vs[i].str("version"))
		b, okB := semver.Parse(vs[j].str("version"))
		if okA != okB {
			return okA
		}
		return okA && semver.Compare(a, b) > 0
	})
}

// buildHosted generates a hosted repository's index.yaml from its rows, each
// row's Attrs being its entry (publish): urls relative to the repository,
// so the file resolves against whichever repository URL (its own or a
// group's) served the index.
func buildHosted(ctx context.Context, tx *meta.Tx, repoName string, now time.Time) ([]byte, error) {
	ix := newIndex(now)
	names, err := tx.ListPackageNames(ctx, repoName)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		pkgs, err := tx.ListPackages(ctx, repoName, name)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			var e entry
			if err := json.Unmarshal([]byte(p.Attrs), &e); err != nil {
				return nil, err
			}
			ix.Entries[name] = append(ix.Entries[name], e)
		}
		sortVersions(ix.Entries[name])
	}
	return ix.marshal()
}

// upstream is what a proxy knows of its remote index: for each chart file
// the proxy serves, the remote URL and digest to fetch it with, and the
// chart names, for the owner rule. Keyed by the sha256 of the index it was
// read from, so a new upstream index replaces it (parseUpstream).
type upstream struct {
	sha    string
	charts map[string]upstreamChart // by ChartFile
	names  map[string]bool
}

type upstreamChart struct{ url, digest string }

// rewrite turns a proxy's cached upstream index into the one it serves:
// every chart gets one relative URL, its file here, so clients download
// through the proxy; what the remote URL was is kept in the upstream table.
// Entries without a usable name, version or URL are dropped.
func rewrite(ix *indexFile, rp *repo.Repository, sha string) *upstream {
	up := &upstream{sha: sha, charts: map[string]upstreamChart{}, names: map[string]bool{}}
	for name, vs := range ix.Entries {
		kept := vs[:0]
		for _, e := range vs {
			urls, _ := e["urls"].([]any)
			if len(urls) == 0 {
				continue
			}
			first, _ := urls[0].(string)
			ref, err := url.Parse(first)
			file := ChartFile(e.str("name"), e.str("version"))
			if first == "" || err != nil || e.str("name") != name || !fileRE.MatchString(file) {
				continue
			}
			up.charts[file] = upstreamChart{url: rp.RemoteURL.ResolveReference(ref).String(), digest: e.str("digest")}
			up.names[name] = true
			e["urls"] = []any{file}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(ix.Entries, name)
			continue
		}
		ix.Entries[name] = kept
	}
	return up
}

// merge is a group's index: for each chart name, the versions of the first
// member that has it (the owner), as cargo and docker groups answer. The
// members' urls are already relative, and a file resolves against the
// group's URL to the group's download, which applies the same owner rule.
func merge(members []*indexFile, now time.Time) *indexFile {
	ix := newIndex(now)
	for _, m := range members {
		for name, vs := range m.Entries {
			if _, ok := ix.Entries[name]; !ok && len(vs) > 0 {
				ix.Entries[name] = vs
			}
		}
	}
	return ix
}

// fingerprint identifies the sources a derived index was made from.
func fingerprint(srcs []meta.Asset) string {
	h := sha256.New()
	for _, a := range srcs {
		io.WriteString(h, a.BlobSHA256+"\n")
	}
	return "derived:" + indexFormatVersion + ":" + hex.EncodeToString(h.Sum(nil))
}

// indexFormatVersion is mixed into every derived fingerprint; bump it when
// rewrite or merge changes what they produce from the same sources.
const indexFormatVersion = "1"

// indexAsset returns rp's index.yaml: a hosted repository's, which its
// writes keep up to date, or for a proxy or group one derived from its
// sources (a proxy's cached upstream index.yaml; each online member's own
// index.yaml, nested groups in place) that is stored under rp and reused
// until a source changes, as resolve.Merged does. Stale is set when a
// proxy's upstream could not be reached and its expired copy was used.
func (h *Handler) indexAsset(ctx context.Context, rp *repo.Repository) (proxy.Result, error) {
	var srcs []meta.Asset
	stale := false
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, indexKey)
		return proxy.Result{Asset: a}, err
	case "proxy":
		res, err := h.fetchUpstream(ctx, rp)
		if err != nil {
			return proxy.Result{}, err
		}
		srcs, stale = []meta.Asset{res.Asset}, res.Stale
	case "group":
		for _, m := range rp.OnlineMembers() {
			res, err := h.indexAsset(ctx, m)
			if errors.Is(err, repo.ErrNotFound) {
				continue // a hosted member nothing was published to yet
			}
			if err != nil {
				return proxy.Result{}, memberFailure(rp, m, err)
			}
			srcs = append(srcs, res.Asset)
			stale = stale || res.Stale
		}
	}
	fp := fingerprint(srcs)
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	if a, err := h.Store.Get(ctx, rp.Name, indexKey); err == nil && a.RemoteETag == fp {
		return proxy.Result{Asset: a, Stale: stale}, nil
	}
	var ix *indexFile
	if rp.Type == "proxy" {
		var err error
		if ix, _, err = h.parseUpstream(rp, srcs[0]); err != nil {
			return proxy.Result{}, err
		}
	} else {
		var members []*indexFile
		for _, a := range srcs {
			m, err := h.readIndex(a)
			if err != nil {
				return proxy.Result{}, err
			}
			members = append(members, m)
		}
		ix = merge(members, h.Now())
	}
	body, err := ix.marshal()
	if err != nil {
		return proxy.Result{}, err
	}
	a, err := h.Store.Put(ctx, rp.Name, indexKey, ctYAML, bytes.NewReader(body), func(a *meta.Asset) { a.RemoteETag = fp })
	return proxy.Result{Asset: a, Stale: stale}, err
}

func (h *Handler) fetchUpstream(ctx context.Context, rp *repo.Repository) (proxy.Result, error) {
	return h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: upstreamKey, Remote: indexKey, NotHTML: true}, proxy.Metadata)
}

func (h *Handler) readIndex(a meta.Asset) (*indexFile, error) {
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseIndex(f)
}

// parseUpstream reads a proxy's cached upstream index (a): the index the
// proxy serves (rewritten) and the upstream table, which it remembers for
// the downloads of charts not cached yet (upstreamFor reuses it while the
// cached index is the same content). An unreadable upstream index is a 502.
func (h *Handler) parseUpstream(rp *repo.Repository, a meta.Asset) (*indexFile, *upstream, error) {
	ix, err := h.readIndex(a)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) || errors.Is(err, meta.ErrNotFound) {
			return nil, nil, err
		}
		return nil, nil, repo.Errorf(http.StatusBadGateway, "upstream index.yaml of %q is not valid YAML: %v", rp.Name, err)
	}
	up := rewrite(ix, rp, a.BlobSHA256)
	h.upstreams.Store(rp.Name, up)
	return ix, up, nil
}

// upstreamFor returns the upstream table of the proxy's current index.
func (h *Handler) upstreamFor(ctx context.Context, rp *repo.Repository) (*upstream, error) {
	res, err := h.fetchUpstream(ctx, rp)
	if err != nil {
		return nil, err
	}
	if up, ok := h.upstreams.Load(rp.Name); ok && up.(*upstream).sha == res.Asset.BlobSHA256 {
		return up.(*upstream), nil
	}
	_, up, err := h.parseUpstream(rp, res.Asset)
	return up, err
}

// owns reports whether rp has chart name: a hosted repository by its rows,
// a proxy by its upstream index, a group by any online member.
func (h *Handler) owns(ctx context.Context, rp *repo.Repository, name string) (bool, error) {
	switch rp.Type {
	case "hosted":
		st, err := h.Store.DB.PackageStamp(ctx, rp.Name, name)
		return st.Count > 0, err
	case "proxy":
		up, err := h.upstreamFor(ctx, rp)
		if err != nil {
			return false, err
		}
		return up.names[name], nil
	}
	for _, m := range rp.OnlineMembers() {
		if ok, err := h.owns(ctx, m, name); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}
