package npm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// member is a package document and the hosted or proxy repository it is from.
type member struct {
	repo *repo.Repository
	res  proxy.Result
}

// docs returns the package documents that answer for name in rp. A group
// answers with the first member that has the package, alone (resolve.Owner):
// no later member's versions, dist-tags or tarballs are mixed in, and a
// member ahead of it that fails ends the search (dependency confusion, QA
// M-4, N-1, N-11). A group with mergeMembers answers with every member's
// document instead, in member order, as Nexus does, for the caller to merge
// (mergeDocs): a member that is offline or fails is left out while another
// answers. The setting counts for the group asked and for groups nested in
// one that merges; in a group that does not, the owner rule goes all the way.
// ponytail: a merge runs per read, four passes over each document, and is
// not stored (the 304 of a warm client skips it); store it as
// Resolver.MergedTargetSalted does if merging large documents shows up.
func (h *Handler) docs(ctx context.Context, rp *repo.Repository, name string) ([]member, error) {
	if !rp.MergeMembers {
		owner, res, err := h.Resolver.Owner(ctx, rp, proxy.Target{Key: PackumentKey(name)}, proxy.Metadata)
		if err != nil {
			return nil, err
		}
		return []member{{owner, res}}, nil
	}
	var out []member
	var failed error
	for _, m := range rp.OnlineMembers() {
		ds, err := h.docs(ctx, m, name)
		out = append(out, ds...)
		if failed == nil && err != nil && repo.StatusOf(err) != http.StatusNotFound {
			slog.Warn("group member lookup failed", "group", rp.Name, "member", m.Name, "err", logsafe.Err(err))
			failed = err
		}
	}
	switch {
	case len(out) > 0:
		return out, nil
	case failed != nil:
		return nil, failed
	}
	return nil, repo.ErrNotFound
}

// tarball returns the tarball file of the package name in rp: from the
// member that owns the package (resolve.OwnerFile, which needs no document
// when the member has the file), or with mergeMembers from the first member
// that has the file, whichever members have the package.
func (h *Handler) tarball(ctx context.Context, rp *repo.Repository, name, file string) (proxy.Result, error) {
	key := TarballKey(name, file)
	if !rp.MergeMembers {
		return h.Resolver.OwnerFile(ctx, rp, proxy.Target{Key: key}, proxy.Target{Key: PackumentKey(name)})
	}
	var failed error
	for _, m := range rp.OnlineMembers() {
		res, err := h.tarball(ctx, m, name, file)
		if err == nil {
			return res, nil
		}
		if failed == nil && repo.StatusOf(err) != http.StatusNotFound {
			slog.Warn("group member lookup failed", "group", rp.Name, "member", m.Name, "err", logsafe.Err(err))
			failed = err
		}
	}
	if failed != nil {
		return proxy.Result{}, failed
	}
	return proxy.Result{}, repo.ErrNotFound
}

// proxyLookup is the resolver's lookup in an npm proxy (resolve.Resolver.Proxy):
// of a package document, or a tarball, by its key.
func (h *Handler) proxyLookup(ctx context.Context, m *repo.Repository, t proxy.Target, kind proxy.Kind) (proxy.Result, error) {
	if kind == proxy.Metadata {
		return h.proxyDoc(ctx, m, strings.TrimSuffix(t.Key, "/package.json"))
	}
	name, file, _ := strings.Cut(t.Key, "/-/")
	return h.proxyTarball(ctx, m, name, file)
}

// packumentTarget is the cache key and the request to proxy m's remote of a
// package document. A scoped name goes upstream as @scope%2fname, as the npm
// client sends it, which every registry has to route.
func packumentTarget(m *repo.Repository, name string) proxy.Target {
	t := proxy.Target{Key: PackumentKey(name), Remote: name, Header: http.Header{"Accept": {"application/json"}}, DeniedIsFailure: true}
	if strings.HasPrefix(name, "@") {
		u := *m.RemoteURL
		u.RawPath = m.RemoteURL.EscapedPath() + strings.Replace(name, "/", "%2f", 1)
		u.Path += name
		t.URL, t.Route = u.String(), name
	}
	return t
}

// settleTime is how long after its fetch a cached file counts as possibly
// still being looked at (exclusive): far longer than that takes.
const settleTime = 30 * time.Second

// settled returns the cached file key of proxy m if it may be served without
// further ado: within maxAge, and fetched so long ago that no exclusive can
// still be looking at it. Anything else (an error too) is for exclusive.
func (h *Handler) settled(ctx context.Context, m *repo.Repository, key string, maxAge config.Duration) (meta.Asset, bool) {
	a, err := h.Store.Get(ctx, m.Name, key)
	if err != nil || a.CheckedAt.IsZero() || maxAge.Off() {
		return meta.Asset{}, false
	}
	f := h.Resolver.Fetcher
	age := f.Now().Sub(a.CheckedAt)
	if age < settleTime || maxAge != config.Never && age >= time.Duration(maxAge) {
		return meta.Asset{}, false
	}
	if f.Stats != nil {
		f.Stats.Cache(m.Name, true)
	}
	return a, true
}

// exclusive runs fn, which fetches the file key of proxy m and looks at what
// came, on this node and every other alone (the lock), and once for the
// requests of this node that want the same (flight, the flight goes on when
// its client leaves). The fetcher stores what the remote answered before fn
// can look at it, so a request that read the file in between would serve or
// keep what fn then refuses: it comes here instead (settled), and waits.
// ponytail: a crash in between leaves the file as the remote answered it, and
// while a remote hangs a node's requests wait behind the other nodes'
// fetches; a check inside proxy.Fetcher (before PutAsset) would need neither
// this nor reject.
func (h *Handler) exclusive(ctx context.Context, m *repo.Repository, key, flight string, fn func(context.Context) (proxy.Result, error)) (proxy.Result, error) {
	ch := h.flights.DoChan(fmt.Sprintf("%s\x00%d\x00%s\x00%s", m.Name, m.Gen, key, flight), func() (any, error) {
		ctx := context.WithoutCancel(ctx)
		unlock, err := h.Store.Lock(ctx, "npm\x00"+m.Name+"\x00"+key)
		if err != nil {
			return proxy.Result{}, err
		}
		defer unlock()
		return fn(ctx)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return proxy.Result{}, r.Err
		}
		return r.Val.(proxy.Result), nil
	case <-ctx.Done():
		return proxy.Result{}, ctx.Err()
	}
}

// routed reports whether the routing rule of proxy m lets a request for
// path through. The fetcher answers not found for one it refuses, cached or
// not, which must not be read as the remote's answer.
func (h *Handler) routed(m *repo.Repository, path string) bool {
	allowed := h.Resolver.Fetcher.RouteAllowed
	return allowed == nil || allowed(m, "/"+path)
}

// cached returns what proxy m holds under key, if anything: what a fetch
// that follows may replace.
func (h *Handler) cached(ctx context.Context, m *repo.Repository, key string) (meta.Asset, bool, error) {
	a, err := h.Store.Get(ctx, m.Name, key)
	if repo.StatusOf(err) == http.StatusNotFound {
		return meta.Asset{}, false, nil
	}
	return a, err == nil, err
}

// reject undoes a fetch whose content is not what was asked for (a sign-in
// or error page answered with 200): nothing of it stays cached. The copy it
// replaced (had) is put back and served as stale, as when the remote fails;
// without one the fetch failed (502, never the fetcher's not found for a
// web page, Target.NotHTML: a group would then ask its next member, and the
// miss would outlive the page). why is logged only.
func (h *Handler) reject(ctx context.Context, m *repo.Repository, key string, prev meta.Asset, had bool, why string) (proxy.Result, error) {
	slog.Warn("remote content refused, not cached", "repo", m.Name, "path", key, "why", why)
	if err := h.Store.Current(m); err != nil {
		return proxy.Result{}, err
	}
	if had {
		// The row as it was: its blob is still in the store (garbage
		// collection keeps what was referenced a moment ago).
		if err := h.Store.DB.PutAsset(ctx, prev); err != nil {
			return proxy.Result{}, err
		}
		return proxy.Result{Asset: prev, Stale: true}, nil
	}
	if err := h.Store.Delete(ctx, m.Name, key); err != nil && repo.StatusOf(err) != http.StatusNotFound {
		return proxy.Result{}, err
	}
	return proxy.Result{}, proxy.ErrRemote
}

// proxyDoc returns the package document of name from proxy m: the cached
// one, refreshed from the remote when its metadata age is over. A document
// the remote just answered is checked and completed first (refreshed).
func (h *Handler) proxyDoc(ctx context.Context, m *repo.Repository, name string) (proxy.Result, error) {
	t := packumentTarget(m, name)
	if !h.routed(m, t.Remote) {
		return proxy.Result{}, repo.ErrNotFound
	}
	if a, ok := h.settled(ctx, m, t.Key, m.MetadataMaxAge); ok {
		return proxy.Result{Asset: a}, nil
	}
	return h.exclusive(ctx, m, t.Key, "", func(ctx context.Context) (proxy.Result, error) {
		prev, had, err := h.cached(ctx, m, t.Key)
		if err != nil {
			return proxy.Result{}, err
		}
		res, err := h.Resolver.Fetcher.FetchTarget(ctx, m, t, proxy.Metadata)
		if err != nil || had && prev.BlobSHA256 == res.Asset.BlobSHA256 {
			return res, err
		}
		return h.refreshed(ctx, m, name, prev, had, res)
	})
}

// refreshed takes the document the remote just answered for name, which the
// fetcher stored as res in place of prev (had). It must be a package
// document (scanDoc), or it is rejected. The versions of prev that the
// remote no longer lists and whose tarball this proxy has cached stay
// listed, as in Nexus: with their manifest and time entry from prev, and as
// latest when the remote's latest is no version of the result (the remote
// keeps a stub document of an unpublished package). The remote's other
// dist-tags are taken as they are. The result is stored under the remote's
// validators, so a 304 keeps it and the next change is merged with it.
func (h *Handler) refreshed(ctx context.Context, m *repo.Repository, name string, prev meta.Asset, had bool, res proxy.Result) (proxy.Result, error) {
	key := res.Asset.Path
	doc, err := h.Store.Open(res.Asset)
	if err != nil {
		return proxy.Result{}, err
	}
	defer doc.Close()
	listed := map[string]bool{}
	if err := scanDoc(doc, func(v string, _ versionDist) { listed[v] = true }); err != nil {
		return h.reject(ctx, m, key, prev, had, err.Error())
	}
	if !had {
		return res, nil
	}
	files := map[string]bool{} // the tarballs of name cached here
	prefix := TarballKey(name, "")
	if err := h.Store.DB.WalkAssetPaths(ctx, m.Name, prefix, func(p string) { files[p[len(prefix):]] = true }); err != nil {
		return proxy.Result{}, err
	}
	if len(files) == 0 {
		return res, nil
	}
	old, err := h.Store.Open(prev)
	if err != nil {
		slog.Warn("previous npm package document unreadable; versions the remote dropped are not kept", "repo", m.Name, "path", key, "err", logsafe.Err(err))
		return res, nil
	}
	defer old.Close()
	keep := map[string]bool{}
	scanDoc(old, func(v string, d versionDist) { // an unreadable prev keeps nothing
		if !listed[v] && files[tarballName(name, v, d.Tarball)] {
			keep[v] = true
		}
	})
	if len(keep) == 0 {
		return res, nil
	}
	tags, err := readDistTags(doc)
	if err != nil {
		return proxy.Result{}, err
	}
	tags = withLatest(tags, listed, keep)
	if err := h.Store.Current(m); err != nil {
		return proxy.Result{}, err
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(mergeDocs(pw, []io.ReadSeeker{doc, old}, tags, func(v string) bool { return keep[v] }, nil))
	}()
	a, err := h.Store.Put(ctx, m.Name, key, res.Asset.ContentType, pr, func(a *meta.Asset) {
		a.LastModified, a.CheckedAt = res.Asset.LastModified, res.Asset.CheckedAt
		a.RemoteETag, a.RemoteLastModified = res.Asset.RemoteETag, res.Asset.RemoteLastModified
	})
	pr.Close()
	<-done
	if err != nil {
		return proxy.Result{}, err
	}
	slog.Info("npm proxy keeps versions the remote dropped: their tarballs are cached", "repo", m.Name, "package", name, "versions", len(keep))
	return proxy.Result{Asset: a}, nil
}

// withLatest returns tags with latest pointing at a version of sets: tags'
// own when it names one, else the newest release of them.
func withLatest(tags []tagPair, sets ...map[string]bool) []tagPair {
	has := func(v string) bool {
		for _, s := range sets {
			if s[v] {
				return true
			}
		}
		return false
	}
	newest := ""
	for _, s := range sets {
		for v := range s {
			newest = semver.NewerRelease(newest, v)
		}
	}
	for i, t := range tags {
		if t.tag == "latest" {
			if !has(t.version) {
				tags[i].version = newest
			}
			return tags
		}
	}
	return append(tags, tagPair{"latest", newest})
}

// proxyTarball returns the tarball file of name from proxy m. It is asked
// for at <name>/-/<file> of the remote, where a registry keeps it: an
// install from a lockfile needs no document. A remote that keeps tarballs
// elsewhere (GitHub Packages, GitLab) does not answer there: then the
// remote's package document says where the file is (upstreamTarball), and
// the routing rule still sees the request path. A file name that is no
// tarball's is the last segment of such a URL, which a lockfile written
// through Nexus holds: it stands for the version's usual name, so that the
// firewall and the component listings know the file. A file just fetched is
// checked (checkTarball).
func (h *Handler) proxyTarball(ctx context.Context, m *repo.Repository, name, file string) (proxy.Result, error) {
	if !strings.HasSuffix(file, ".tgz") {
		doc, err := h.proxyDoc(ctx, m, name)
		if err != nil {
			return proxy.Result{}, err
		}
		v, _, err := h.distOf(doc.Asset, name, file)
		if err != nil {
			return proxy.Result{}, err
		}
		if v == "" {
			return proxy.Result{}, repo.ErrNotFound
		}
		file = TarballFile(name, v)
	}
	key := TarballKey(name, file)
	if !h.routed(m, key) {
		return proxy.Result{}, repo.ErrNotFound
	}
	if a, ok := h.settled(ctx, m, key, m.ContentMaxAge); ok {
		return proxy.Result{Asset: a}, nil
	}
	t := proxy.Target{Key: key, Remote: key}
	res, err := h.fetchTarball(ctx, m, name, file, t)
	if err == nil {
		return res, nil
	}
	named, derr := h.upstreamTarball(ctx, m, name, file)
	usual := *m.RemoteURL
	usual.Path += key
	switch {
	case derr != nil && repo.StatusOf(err) == http.StatusNotFound:
		return proxy.Result{}, derr // whether the file exists is not known
	case derr != nil, named == "", named == usual.String():
		return proxy.Result{}, err
	}
	t.URL, t.Route = named, key
	return h.fetchTarball(ctx, m, name, file, t)
}

// fetchTarball fetches the tarball file of name into proxy m as t says, and
// checks what came (checkTarball).
func (h *Handler) fetchTarball(ctx context.Context, m *repo.Repository, name, file string, t proxy.Target) (proxy.Result, error) {
	return h.exclusive(ctx, m, t.Key, t.URL, func(ctx context.Context) (proxy.Result, error) {
		prev, had, err := h.cached(ctx, m, t.Key)
		if err != nil {
			return proxy.Result{}, err
		}
		if t.URL != "" { // the miss at the usual place was recorded under the same key
			if err := h.Store.DB.ClearNegative(ctx, m.Name, t.Key); err != nil {
				return proxy.Result{}, err
			}
		}
		res, err := h.Resolver.Fetcher.FetchTarget(ctx, m, t, proxy.Content)
		if err != nil || had && prev.BlobSHA256 == res.Asset.BlobSHA256 {
			return res, err
		}
		if why := h.checkTarball(ctx, m, name, file, res.Asset); why != "" {
			return h.reject(ctx, m, t.Key, prev, had, why)
		}
		return res, nil
	})
}

// upstreamTarball returns the URL the package document of name in proxy m
// gives for the tarball file: dist.tarball of the version it belongs to, ""
// when the document has none. The document is the cached one, fetched or
// refreshed as for a client.
func (h *Handler) upstreamTarball(ctx context.Context, m *repo.Repository, name, file string) (string, error) {
	res, err := h.proxyDoc(ctx, m, name)
	if err != nil {
		return "", err
	}
	_, d, err := h.distOf(res.Asset, name, file)
	if err != nil || !strings.HasPrefix(d.Tarball, "http://") && !strings.HasPrefix(d.Tarball, "https://") {
		return "", err
	}
	return d.Tarball, nil
}

// distOf returns the version whose tarball goes by file in the stored package
// document doc of name (tarballName, or the last segment of its upstream
// URL), and what the document says of that tarball; "" when there is none.
func (h *Handler) distOf(doc meta.Asset, name, file string) (string, versionDist, error) {
	f, err := h.Store.Open(doc)
	if err != nil {
		return "", versionDist{}, err
	}
	defer f.Close()
	version, found := "", versionDist{}
	err = scanDoc(f, func(v string, d versionDist) {
		if version == "" && (tarballName(name, v, d.Tarball) == file || fileFromURL(d.Tarball) == file) {
			version, found = v, d
		}
	})
	return version, found, err
}

// checkTarball looks at the tarball a of name that proxy m just fetched and
// returns why it must not be cached, "" when it may: it is no gzip file (a
// sign-in or error page, whatever type the remote declared), or it is not
// the file the cached package document promises (dist.shasum,
// dist.integrity), which an npm client would refuse on every install while
// the file stays cached. Without a cached document only the first holds.
func (h *Handler) checkTarball(ctx context.Context, m *repo.Repository, name, file string, a meta.Asset) string {
	f, err := h.Store.Open(a)
	if err != nil {
		return "" // serving it will report the store
	}
	_, err = repo.CheckContent(".tgz", "", f)
	f.Close()
	if err != nil {
		return "the remote's answer is not a gzip file"
	}
	doc, had, err := h.cached(ctx, m, PackumentKey(name))
	if err != nil || !had {
		return ""
	}
	_, d, err := h.distOf(doc, name, file)
	if err != nil {
		return ""
	}
	if verifyTarball(d.Shasum, d.Integrity, blob.Sums{SHA1: a.SHA1, SHA512: a.SHA512}) != nil {
		return "the content does not match the checksum in the package document"
	}
	return ""
}
