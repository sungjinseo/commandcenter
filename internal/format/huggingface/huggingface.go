// Package huggingface implements a Hugging Face Hub proxy
// (https://huggingface.co/), what huggingface_hub, transformers and
// datasets read with HF_ENDPOINT pointing at it: the JSON API under api/
// (models, datasets, spaces, their revisions and trees; cached for
// metadataMaxAge; the rest of the API, whoami-v2 say, is not served, so the
// remoteAuth token's account stays private) and the files, [datasets/|spaces/]<repo id>/resolve/
// <revision>/<path>. The hub answers a file request with a redirect to its
// CDN carrying X-Repo-Commit, X-Linked-Etag and X-Linked-Size; the proxy
// follows it, caches the file under the commit sha (immutable, so for
// contentMaxAge, never by default) and replays those headers with ETag,
// Content-Length and byte ranges. The ETag is the content's sha256, which
// is the hub's X-Linked-Etag of an LFS file: huggingface_hub names its
// cache entries by it, so a cached and an uncached answer must agree. A branch or tag revision is resolved to
// its commit through api/<kind>/<repo id>/revision/<revision> (so it moves
// with metadataMaxAge). The remoteAuth bearer token goes to the hub only,
// never to the CDN, and a client's own Authorization is not forwarded.
// Nexus has Hugging Face proxies only, and so does this.
//
// What huggingface_hub 1.x and 2.x need beyond the files: a tree listing
// (api/<kind>/<repo id>/tree/...) without the Xet hashes, which would send
// the client to the hub's Xet storage past the proxy, and with the Link to
// its next page pointing at the proxy; and the hub's X-Error-Code on a 404,
// which the client turns into EntryNotFoundError, RevisionNotFoundError or
// RepositoryNotFoundError (transformers probes optional files with it).
package huggingface

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves huggingface proxies.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	BaseURL func() string // live: read per request (the Link of a tree listing)

	// ponytail: one HEAD client per remote URL, never pruned; a handful
	// of repositories.
	heads sync.Map // remote URL -> *http.Client that does not follow redirects
}

func New(store *repo.Store, fetcher *proxy.Fetcher, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, BaseURL: baseURL}
}

// maxDoc bounds an API document read back from the cache (a revision's
// siblings list of a large dataset is megabytes).
const maxDoc = 32 << 20

// Route is a parsed file request.
type Route struct {
	Kind   string // models, datasets or spaces
	RepoID string // org/name, or a bare name
	Rev    string // a commit sha, branch or tag
	File   string
}

// prefix is where the files of rt's repository are: <repo id>/ for a
// model, datasets/<repo id>/ or spaces/<repo id>/ otherwise.
func (rt Route) prefix() string {
	if rt.Kind == "models" {
		return rt.RepoID + "/"
	}
	return rt.Kind + "/" + rt.RepoID + "/"
}

// ParseRoute parses [datasets/|spaces/]<repo id>/resolve/<rev>/<file>. A
// revision holding "/" (refs/pr/1) cannot be told from the file path once
// the server unescaped it and is not supported.
func ParseRoute(p string) (Route, bool) {
	head, rest, found := strings.Cut(p, "/resolve/")
	rev, file, hasFile := strings.Cut(rest, "/")
	if !found || !hasFile || !validSegment(rev) || file == "" {
		return Route{}, false
	}
	for _, s := range strings.Split(file, "/") {
		if !validSegment(s) {
			return Route{}, false
		}
	}
	rt := Route{Kind: "models", Rev: rev, File: file}
	for _, k := range []string{"datasets", "spaces"} {
		if id, cut := strings.CutPrefix(head, k+"/"); cut {
			rt.Kind, head = k, id
			break
		}
	}
	segs := strings.Split(head, "/")
	if len(segs) > 2 {
		return Route{}, false
	}
	for _, s := range segs {
		if !validSegment(s) {
			return Route{}, false
		}
	}
	rt.RepoID = head
	return rt, true
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `\`)
}

// IsSHA reports whether rev is a commit sha (40 lower-case hex digits).
func IsSHA(rev string) bool { return len(rev) == 40 && strings.Trim(rev, "0123456789abcdef") == "" }

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
		return repo.Errorf(http.StatusBadRequest, "huggingface repository %q must be a proxy", rp.Name)
	}
	ctx := r.Context()
	if strings.HasPrefix(p, "api/") {
		kind, rest, _ := strings.Cut(strings.TrimPrefix(p, "api/"), "/")
		if kind != "models" && kind != "datasets" && kind != "spaces" || rest == "" || strings.Contains(p, "/..") {
			return repo.ErrNotFound
		}
		id, doc, rev := splitAPI(rest)
		// A Xet token is the hub's for remoteAuth's account: with one the
		// client would fetch past the proxy, and any reader would hold it.
		if doc == "xet-read-token" || doc == "xet-write-token" {
			return repo.ErrNotFound
		}
		t := apiTarget(rp, p, r.URL.RawQuery)
		if doc == "tree" {
			t.Key += treeSuffix
			t.Keep = h.keepNext(ctx, rp, t.Key)
		}
		res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
		if err != nil {
			if doc == "" {
				return hubError(w, err, "RepoNotFound", "Repository not found")
			}
			return h.missing(ctx, w, rp, kind, id, rev, err)
		}
		proxy.SetStaleHeader(w, res)
		if doc == "tree" {
			return h.serveTree(w, r, rp, p, t.Key, res.Asset)
		}
		if res.Asset.ContentType == "" {
			res.Asset.ContentType = "application/json"
		}
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	}
	rt, ok := ParseRoute(p)
	if !ok {
		return repo.ErrNotFound
	}
	sha := rt.Rev
	if !IsSHA(sha) {
		var err error
		if sha, err = h.commit(ctx, rp, rt); err != nil {
			return h.missing(ctx, w, rp, rt.Kind, rt.RepoID, rt.Rev, err)
		}
	}
	key := rt.prefix() + "resolve/" + sha + "/" + rt.File
	a, err := h.Store.Get(ctx, rp.Name, key)
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return err
	}
	if err != nil && r.Method == http.MethodHead {
		if done, err := h.head(ctx, w, rp, key, sha); done || err != nil {
			return hubError(w, err, "EntryNotFound", "Entry not found")
		}
	}
	res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: key, Remote: key, NotHTML: true, DeniedIsNotFound: true}, proxy.Content)
	if err != nil {
		return hubError(w, err, "EntryNotFound", "Entry not found")
	}
	proxy.SetStaleHeader(w, res)
	a = res.Asset
	etag := `"` + a.BlobSHA256 + `"` // not RemoteETag: the CDN's, after the redirect
	hd := w.Header()
	hd.Set("ETag", etag)
	hd.Set("X-Linked-Etag", etag)
	hd.Set("X-Linked-Size", strconv.FormatInt(a.Size, 10))
	hd.Set("X-Repo-Commit", sha)
	hd.Set("Accept-Ranges", "bytes")
	a.SHA1 = "" // keeps the ETag above, which the client names its cache entries by
	repo.ServeAsset(w, r, h.Store, a)
	return nil
}

// hubError marks a 404 as the hub does: huggingface_hub reads X-Error-Code
// and raises EntryNotFoundError, RevisionNotFoundError or
// RepositoryNotFoundError for it. Without the code it reports a connection
// problem, and transformers, which probes optional files, gives up.
func hubError(w http.ResponseWriter, err error, code, msg string) error {
	if repo.StatusOf(err) == http.StatusNotFound {
		w.Header().Set("X-Error-Code", code)
		w.Header().Set("X-Error-Message", msg)
	}
	return err
}

// missing is hubError for a document of revision rev of a repository: the
// repository's own document and its revision document (cached, or their
// misses) tell whether the repository, the revision or, with both there,
// the entry asked for is missing (a directory tree/<rev>/<path> lists).
func (h *Handler) missing(ctx context.Context, w http.ResponseWriter, rp *repo.Repository, kind, id, rev string, err error) error {
	if repo.StatusOf(err) != http.StatusNotFound {
		return err
	}
	has := func(doc string) bool {
		_, err := h.Fetcher.FetchTarget(ctx, rp, apiTarget(rp, "api/"+kind+"/"+id+doc, ""), proxy.Metadata)
		return repo.StatusOf(err) != http.StatusNotFound
	}
	switch {
	case !has(""):
		return hubError(w, err, "RepoNotFound", "Repository not found")
	case rev != "" && !has("/revision/"+rev):
		return hubError(w, err, "RevisionNotFound", "Invalid rev id: "+rev)
	}
	return hubError(w, err, "EntryNotFound", "Entry not found")
}

// splitAPI splits what follows api/<kind>/ into the repository id (name or
// org/name), the document asked of it ("" for the repository's own,
// "revision", "tree", ...) and that document's revision.
func splitAPI(rest string) (id, doc, rev string) {
	segs := strings.Split(rest, "/")
	n := min(2, len(segs))
	if n == 2 && (segs[1] == "revision" || segs[1] == "tree") {
		n = 1 // a repository without an organisation (gpt2)
	}
	if len(segs) > n {
		doc = segs[n]
	}
	if len(segs) > n+1 {
		rev = segs[n+1]
	}
	return strings.Join(segs[:n], "/"), doc, rev
}

const (
	// treeSuffix marks a tree listing's cache key. A listing cached before
	// the next page was kept has no treeSuffix: a revalidation answers 304
	// to it and would never bring the page's Link.
	treeSuffix = "~tree"
	nextSuffix = "~next"
)

// keepNext is the Keep of a tree listing cached at key: the hub lists
// 1000 entries a page and names the next page in its Link header. Its
// query is stored at key~next (removed when the listing ends there) for
// serveTree to offer.
func (h *Handler) keepNext(ctx context.Context, rp *repo.Repository, key string) func(http.Header) {
	ctx = context.WithoutCancel(ctx) // the fetch outlives a client that went away
	return func(hd http.Header) {
		var err error
		if next := nextQuery(hd.Get("Link")); next != "" {
			_, err = h.Store.Put(ctx, rp.Name, key+nextSuffix, "text/plain", strings.NewReader(next))
		} else if err = h.Store.Delete(ctx, rp.Name, key+nextSuffix); errors.Is(err, repo.ErrNotFound) {
			err = nil
		}
		if err != nil {
			slog.Warn("huggingface proxy: could not record the next page of a tree listing", "repo", rp.Name, "path", key, "err", logsafe.Err(err))
		}
	}
}

// nextQuery is the query of the rel="next" URL of a Link header, "" without one.
func nextQuery(link string) string {
	for _, l := range strings.Split(link, ",") {
		target, params, _ := strings.Cut(l, ";")
		if !strings.Contains(params, `rel="next"`) {
			continue
		}
		if u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>")); err == nil {
			return u.RawQuery
		}
	}
	return ""
}

// serveTree answers the tree listing cached as a (at key, asked for at
// path p): without the entries' xetHash, with which huggingface_hub would
// skip the proxy and download from the hub's Xet storage, and with the
// next page's Link at the proxy.
func (h *Handler) serveTree(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p, key string, a meta.Asset) error {
	body, err := h.readDoc(a)
	if err != nil {
		return err
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(body, &entries) == nil {
		for _, e := range entries {
			delete(e, "xetHash")
		}
		if body, err = json.Marshal(entries); err != nil {
			return err
		}
	}
	if na, err := h.Store.Get(r.Context(), rp.Name, key+nextSuffix); err == nil {
		if next, err := h.readDoc(na); err == nil && len(next) > 0 && h.BaseURL != nil {
			u := url.URL{Path: "/repository/" + rp.Name + "/" + p, RawQuery: string(next)}
			w.Header().Set("Link", "<"+strings.TrimRight(h.BaseURL(), "/")+u.String()+`>; rel="next"`)
		}
	}
	repo.ServeText(w, r, string(body), "application/json", a.LastModified)
	return nil
}

// readDoc reads a cached API document.
func (h *Handler) readDoc(a meta.Asset) ([]byte, error) {
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxDoc))
}

// apiTarget caches an API document under its path, plus a hash of the
// query when it has one (expand[], blobs=true change the answer). The
// Fetcher escapes a "?" in Remote, so a query goes in an absolute URL,
// built from the remote as the Fetcher builds Remote's (p unescaped: a
// file path in tree/ may hold "%", "#" or "?").
func apiTarget(rp *repo.Repository, p, query string) proxy.Target {
	t := proxy.Target{Key: p, Remote: p, NotHTML: true, DeniedIsNotFound: true, Header: http.Header{"Accept": {"application/json"}}}
	if query != "" {
		u := *rp.RemoteURL
		u.Path, u.RawPath, u.RawQuery = rp.RemoteURL.Path+p, "", query
		sum := sha256.Sum256([]byte(query))
		t.Key += "~" + hex.EncodeToString(sum[:8])
		t.Remote, t.URL = "", u.String()
	}
	return t
}

// commit resolves a branch or tag to its commit sha through the revision
// document api/<kind>/<repo id>/revision/<rev> (cached for metadataMaxAge).
func (h *Handler) commit(ctx context.Context, rp *repo.Repository, rt Route) (string, error) {
	p := "api/" + rt.Kind + "/" + rt.RepoID + "/revision/" + rt.Rev
	res, err := h.Fetcher.FetchTarget(ctx, rp, apiTarget(rp, p, ""), proxy.Metadata)
	if err != nil {
		return "", err
	}
	f, err := h.Store.Open(res.Asset)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var doc struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(f, maxDoc)).Decode(&doc); err != nil || !IsSHA(doc.SHA) {
		return "", repo.Errorf(http.StatusBadGateway, "upstream revision document of %s %s has no commit sha", rt.RepoID, rt.Rev)
	}
	return doc.SHA, nil
}

// head answers a HEAD for an LFS file that is not cached without
// downloading it: a HEAD at the hub, whose redirect to the CDN carries the
// headers the client reads. A regular file (under 10 MB; the hub serves it
// itself without X-Linked-Etag or, now, redirects with the file's git blob
// id there) is left to the caller to fetch (done false), so its ETag is
// the content's sha256 as when it is cached.
func (h *Handler) head(ctx context.Context, w http.ResponseWriter, rp *repo.Repository, key, sha string) (done bool, err error) {
	u := *rp.RemoteURL
	u.Path += key
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", proxy.UserAgent)
	if ra := rp.RemoteAuth; ra != nil { // the hub itself: the only host this request reaches
		if ra.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+ra.BearerToken)
		} else {
			req.SetBasicAuth(ra.Username, ra.Password)
		}
	}
	resp, err := h.headClient(rp).Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", proxy.ErrRemote, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return false, repo.ErrNotFound
	default:
		return false, repo.Errorf(http.StatusBadGateway, "remote status %d", resp.StatusCode)
	}
	etag, size := resp.Header.Get("X-Linked-Etag"), resp.Header.Get("X-Linked-Size")
	if !proxy.IsSHA256Hex(strings.Trim(etag, `"`)) || size == "" {
		return false, nil
	}
	hd := w.Header()
	hd.Set("ETag", etag)
	hd.Set("X-Linked-Etag", etag)
	hd.Set("X-Linked-Size", size)
	hd.Set("Content-Length", size)
	hd.Set("X-Repo-Commit", sha)
	hd.Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
	return true, nil
}

// headClient is the client of head for rp's remote: policed as the
// Fetcher's fetches are, but stopping at the hub's redirect, whose headers
// are the answer.
func (h *Handler) headClient(rp *repo.Repository) *http.Client {
	k := rp.RemoteURL.String()
	if c, ok := h.heads.Load(k); ok {
		return c.(*http.Client)
	}
	c := proxy.NewClientFor(rp.RemoteURL, nil)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	v, _ := h.heads.LoadOrStore(k, c)
	return v.(*http.Client)
}

// Metadata is what the UI's component metadata shows of a cached API
// document of a repository (api/<kind>/<repo id>, or its revision).
type Metadata struct {
	SHA         string
	PipelineTag string
	Library     string
	License     string
}

// ParseMetadata reads Metadata out of an api/models (datasets, spaces)
// document: the license is cardData.license, else a license: tag.
func ParseMetadata(raw []byte) (Metadata, error) {
	var doc struct {
		SHA         string   `json:"sha"`
		PipelineTag string   `json:"pipeline_tag"`
		Library     string   `json:"library_name"`
		Tags        []string `json:"tags"`
		CardData    struct {
			License any `json:"license"`
		} `json:"cardData"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Metadata{}, err
	}
	m := Metadata{SHA: doc.SHA, PipelineTag: doc.PipelineTag, Library: doc.Library}
	switch l := doc.CardData.License.(type) {
	case string:
		m.License = l
	case []any:
		var names []string
		for _, x := range l {
			if s, ok := x.(string); ok {
				names = append(names, s)
			}
		}
		m.License = strings.Join(names, ", ")
	}
	if m.License == "" {
		for _, t := range doc.Tags {
			if l, ok := strings.CutPrefix(t, "license:"); ok {
				m.License = l
				break
			}
		}
	}
	return m, nil
}
