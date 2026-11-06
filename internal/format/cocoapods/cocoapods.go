// Package cocoapods implements a CocoaPods proxy of the trunk CDN
// (https://cdn.cocoapods.org/), what `pod install` reads from a CDN
// source: the index files at the root (CocoaPods-version.yml, all_pods.txt,
// the all_pods_versions_<a>_<b>_<c>.txt shards, deprecated_podspecs.txt)
// and the podspecs under Specs/<a>/<b>/<c>/<Pod>/<version>/. A podspec whose
// source is an http archive, or a git repository on GitHub, Bitbucket or
// GitLab (whose tarball of the ref stands in for the clone), is served with
// its source pointing at the proxy (pods/<Pod>/<version>/<sha1 of the
// URL>.<ext>), which fetches and caches the archive on demand; other sources
// pass through untouched. Nexus has CocoaPods proxies only, and so does this:
// the legacy Specs git repository is not a remote (CDN only), and there is
// no hosted or group repository.
package cocoapods

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves cocoapods proxies.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	BaseURL func() string // live: the absolute URL the rewritten archive URLs start with

	gitHosts map[string]string // host -> the base URL of its tarballs; nil: the public hosts (tests)
}

func New(store *repo.Store, fetcher *proxy.Fetcher, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, BaseURL: baseURL}
}

// maxPodspec bounds a podspec read back from the cache; real ones are kilobytes.
const maxPodspec = 4 << 20

const (
	specsPrefix = "Specs/"
	podsPrefix  = "pods/"
	ctJSON      = "application/json"
)

var (
	// indexRE is an index file at the CDN root: CocoaPods-version.yml,
	// all_pods.txt, all_pods_versions_a_b_c.txt, deprecated_podspecs.txt.
	indexRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.(txt|yml)$`)
	// segRE is a pod name or version as the CDN spells them in paths
	// (gRPC's pods are named !ProtoCompiler and !ProtoCompiler-gRPCPlugin).
	segRE  = regexp.MustCompile(`^[A-Za-z0-9_!][A-Za-z0-9_.+!-]*$`)
	sha1RE = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	if err := h.serve(w, r, rp, p); err != nil {
		repo.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
	}
	if rp.Type != "proxy" {
		return repo.Errorf(http.StatusBadRequest, "cocoapods repository %q must be a proxy", rp.Name)
	}
	ctx := r.Context()
	switch {
	case indexRE.MatchString(p):
		res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: p, Remote: p, NotHTML: true}, proxy.Metadata)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		res.Asset.ContentType = "text/plain; charset=utf-8"
		repo.ServeAsset(w, r, h.Store, res.Asset)
	case strings.HasPrefix(p, specsPrefix):
		pod, version, ok := splitSpec(p)
		if !ok {
			// An archive an earlier version cached beside its podspec, where
			// the REST API's downloadUrl leads; from the cache only.
			a, err := h.Store.Get(ctx, rp.Name, p)
			if err != nil {
				return err
			}
			serveArchive(w, r, h.Store, proxy.Result{Asset: a}, p)
			return nil
		}
		res, spec, err := h.podspec(ctx, rp, pod, version)
		if err != nil {
			return err
		}
		body, err := h.rewrite(rp, spec, pod, version)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		repo.ServeText(w, r, string(body), ctJSON, res.Asset.LastModified)
	case strings.HasPrefix(p, podsPrefix):
		res, err := h.archive(ctx, rp, p)
		if err != nil {
			return err
		}
		serveArchive(w, r, h.Store, res, p)
	default:
		return repo.ErrNotFound
	}
	return nil
}

func serveArchive(w http.ResponseWriter, r *http.Request, store *repo.Store, res proxy.Result, p string) {
	proxy.SetStaleHeader(w, res)
	if res.Asset.ContentType == "" {
		res.Asset.ContentType = contentType(p)
	}
	repo.ServeAsset(w, r, store, res.Asset)
}

// Shard returns the CDN's shard directory of a pod, a/b/c: the first three
// hex digits of the md5 of its name.
func Shard(pod string) string {
	sum := md5.Sum([]byte(pod))
	hx := hex.EncodeToString(sum[:3])
	return hx[0:1] + "/" + hx[1:2] + "/" + hx[2:3]
}

// SpecPath is the CDN path, and the cache key, of a version's podspec.
func SpecPath(pod, version string) string {
	return specsPrefix + Shard(pod) + "/" + pod + "/" + version + "/" + pod + ".podspec.json"
}

// splitSpec parses Specs/<a>/<b>/<c>/<Pod>/<version>/<Pod>.podspec.json;
// the shard must be the pod's own, so one podspec has one cache key.
func splitSpec(p string) (pod, version string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) != 7 {
		return "", "", false
	}
	pod, version = segs[4], segs[5]
	if !segRE.MatchString(pod) || !segRE.MatchString(version) || segs[6] != pod+".podspec.json" || p != SpecPath(pod, version) {
		return "", "", false
	}
	return pod, version, true
}

// podspec fetches (or reads from the cache) the podspec of pod version,
// parsed. It is metadata, re-checked every metadataMaxAge: trunk can change a
// published podspec, a deprecation for one.
func (h *Handler) podspec(ctx context.Context, rp *repo.Repository, pod, version string) (proxy.Result, map[string]any, error) {
	key := SpecPath(pod, version)
	res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: key, Remote: key, NotHTML: true, Check: proxy.JSONDocument()}, proxy.Metadata)
	if err != nil {
		return res, nil, err
	}
	f, err := h.Store.Open(res.Asset)
	if err != nil {
		return res, nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPodspec+1))
	if err != nil {
		return res, nil, err
	}
	var spec map[string]any
	if len(raw) > maxPodspec || json.Unmarshal(raw, &spec) != nil || spec == nil {
		return res, nil, repo.Errorf(http.StatusBadGateway, "upstream podspec of %s %s is not a JSON object", pod, version)
	}
	return res, spec, nil
}

// archiveSource returns the URL of the archive the proxy serves for a
// podspec's source, source.http or the tarball of a git source (gitTarball),
// and its sha1 (the proxy's file name for the archive); ok false for a source
// that passes through.
func (h *Handler) archiveSource(spec map[string]any) (u, ref string, ok bool) {
	src, _ := spec["source"].(map[string]any)
	u, _ = src["http"].(string)
	if u == "" {
		u = h.gitTarball(src)
	}
	if u == "" {
		return "", "", false
	}
	sum := sha1.Sum([]byte(u))
	return u, hex.EncodeToString(sum[:]), true
}

// publicGitHosts are the hosts whose tarballs stand in for a clone, and the
// base URLs of the tarballs: GitHub's web URL, not its API (which allows an
// unauthenticated address 60 requests an hour).
var publicGitHosts = map[string]string{"github.com": "https://github.com", "bitbucket.org": "https://bitbucket.org", "gitlab.com": "https://gitlab.com"}

// gitTarball is the URL of the tarball of a git source's ref (the first of
// commit, tag and branch, the default branch without one), "" when the source
// has to stay a clone: another host, a path that is no repository on its host,
// a ref that is not a string, or submodules, which a tarball does not hold.
func (h *Handler) gitTarball(src map[string]any) string {
	raw, _ := src["git"].(string)
	if raw == "" || src["submodules"] == true {
		return ""
	}
	host, repoPath, ok := parseGitURL(raw)
	bases := h.gitHosts
	if bases == nil {
		bases = publicGitHosts
	}
	base, known := bases[host]
	if !ok || !known {
		return ""
	}
	segs := strings.Split(repoPath, "/")
	if len(segs) < 2 || slices.Contains(segs, "") || slices.Contains(segs, "..") || (len(segs) != 2 && host != "gitlab.com") {
		return "" // GitLab alone nests projects in groups
	}
	ref := "HEAD"
	for _, k := range []string{"commit", "tag", "branch"} {
		if v, set := src[k]; set {
			if ref, _ = v.(string); ref == "" {
				return ""
			}
			break
		}
	}
	var file string
	switch host {
	case "github.com":
		file = repoPath + "/archive/" + ref + ".tar.gz"
	case "bitbucket.org":
		file = repoPath + "/get/" + ref + ".tar.gz"
	case "gitlab.com":
		file = repoPath + "/-/archive/" + ref + "/" + path.Base(repoPath) + "-" + strings.ReplaceAll(ref, "/", "-") + ".tar.gz"
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.JoinPath(file).String()
}

// parseGitURL splits a git remote, a URL or the scp form git@host:owner/repo,
// into its lower-case host and its path without slashes around it or ".git".
func parseGitURL(raw string) (host, repoPath string, ok bool) {
	if !strings.Contains(raw, "://") {
		user, rest, found := strings.Cut(raw, "@")
		if !found || strings.Contains(user, "/") {
			rest = raw
		}
		if host, repoPath, ok = strings.Cut(rest, ":"); !ok {
			return "", "", false
		}
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", false
		}
		host, repoPath = u.Hostname(), u.Path
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	return host, strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git"), host != ""
}

// archiveExt is the extension the proxy's archive path keeps from the
// upstream URL, so CocoaPods still tells the archive type by it
// (.zip, .tgz, .tar.gz, .tar.bz2, .tar.xz, ...); ".bin" when the URL has
// none (the podspec's source.type says what it is then).
func archiveExt(u string) string {
	p := u
	if pu, err := url.Parse(u); err == nil {
		p = pu.Path
	}
	ext := path.Ext(p)
	if ext == "" || len(ext) > 8 || strings.ContainsAny(ext, "%/") {
		return ".bin"
	}
	if stem := strings.TrimSuffix(p, ext); strings.HasSuffix(stem, ".tar") {
		ext = ".tar" + ext
	}
	return ext
}

// rewrite returns the podspec with source.http pointing at the proxy:
// <base>/repository/<repo>/pods/<Pod>/<version>/<sha1 of the URL>.<ext>,
// without the keys of a git source, which the client's http downloader
// refuses. Sources that are not served as archives are left as they are.
func (h *Handler) rewrite(rp *repo.Repository, spec map[string]any, pod, version string) ([]byte, error) {
	if u, ref, ok := h.archiveSource(spec); ok {
		src := map[string]any{}
		for k, v := range spec["source"].(map[string]any) {
			switch k {
			case "git", "commit", "tag", "branch", "submodules":
			default:
				src[k] = v
			}
		}
		src["http"] = h.repoURL(rp) + "/" + podsPrefix + pod + "/" + version + "/" + ref + archiveExt(u)
		spec["source"] = src
	}
	return json.Marshal(spec)
}

// repoURL is the absolute URL of rp without a trailing slash: CocoaPods
// downloads source.http with curl, so the rewritten URL must be absolute.
func (h *Handler) repoURL(rp *repo.Repository) string {
	base := ""
	if h.BaseURL != nil {
		base = strings.TrimRight(h.BaseURL(), "/")
	}
	return base + "/repository/" + rp.Name
}

// archive serves pods/<Pod>/<version>/<sha1>.<ext>: the archive the podspec's
// source names, fetched from there the first time and cached under that path,
// verified against the podspec's sha256 when it has one. An archive an
// earlier version cached beside the podspec is served from there.
func (h *Handler) archive(ctx context.Context, rp *repo.Repository, p string) (proxy.Result, error) {
	segs := strings.Split(strings.TrimPrefix(p, podsPrefix), "/")
	if len(segs) != 3 || !segRE.MatchString(segs[0]) || !segRE.MatchString(segs[1]) {
		return proxy.Result{}, repo.ErrNotFound
	}
	pod, version, file := segs[0], segs[1], segs[2]
	ref, _, _ := strings.Cut(file, ".")
	if !sha1RE.MatchString(ref) || file == ref {
		return proxy.Result{}, repo.ErrNotFound
	}
	for _, key := range []string{p, path.Dir(SpecPath(pod, version)) + "/" + file} {
		a, err := h.Store.Get(ctx, rp.Name, key)
		if err == nil {
			return proxy.Result{Asset: a}, nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return proxy.Result{}, err
		}
	}
	_, spec, err := h.podspec(ctx, rp, pod, version)
	if err != nil {
		return proxy.Result{}, err
	}
	u, want, ok := h.archiveSource(spec)
	if !ok || want != ref || file != ref+archiveExt(u) {
		return proxy.Result{}, repo.ErrNotFound
	}
	t := proxy.Target{Key: p, URL: u, Route: p, NotHTML: true}
	if sum, _ := spec["source"].(map[string]any)["sha256"].(string); proxy.IsSHA256Hex(sum) {
		t.SHA256 = sum
	}
	return h.Fetcher.FetchTarget(ctx, rp, t, proxy.Content)
}

// contentType is the media type of an archive path by its extension.
func contentType(p string) string {
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// Metadata is what the UI's component metadata shows of a cached podspec
// (the name of the asset the UI reads is SpecPath).
type Metadata struct {
	Summary      string
	Description  string
	License      string
	Homepage     string
	Source       string              // the upstream source: its git or http URL
	Dependencies map[string][]string // pod -> requirements
}

// ParseMetadata reads Metadata out of a podspec.json.
func ParseMetadata(raw []byte) (Metadata, error) {
	var spec struct {
		Summary      string         `json:"summary"`
		Description  string         `json:"description"`
		License      any            `json:"license"`
		Homepage     string         `json:"homepage"`
		Source       map[string]any `json:"source"`
		Dependencies map[string]any `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return Metadata{}, err
	}
	m := Metadata{Summary: spec.Summary, Description: spec.Description, Homepage: spec.Homepage, Dependencies: map[string][]string{}}
	switch l := spec.License.(type) {
	case string:
		m.License = l
	case map[string]any:
		m.License, _ = l["type"].(string)
	}
	for _, k := range []string{"git", "http", "svn", "hg"} {
		if s, _ := spec.Source[k].(string); s != "" {
			m.Source = s
			break
		}
	}
	for name, req := range spec.Dependencies {
		var reqs []string
		if list, ok := req.([]any); ok {
			for _, x := range list {
				if s, ok := x.(string); ok {
					reqs = append(reqs, s)
				}
			}
		}
		m.Dependencies[name] = reqs
	}
	return m, nil
}
