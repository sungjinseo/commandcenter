package conan

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves Conan remotes. Reads need Read, an upload (PUT of a file,
// POST of v1 upload_urls) Add, a delete Delete; overwriting a file under
// writePolicy allow needs Edit, checked here. v1/ping and the v1/users
// endpoints are public (Access): v1/users/authenticate turns the server's
// Basic principal into a signed token the client sends back as Bearer,
// which auth.Authenticator accepts through VerifyToken.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher // of proxies: NewFetcher, whose client logs in to the remote
	Auth    *auth.Authenticator
	BaseURL func() string // live: read per request
	Key     []byte        // signs the login tokens (HMAC-SHA256)
	Now     func() time.Time
	locks   repo.KeyedMutex // serializes the writes of one recipe reference per repository
}

// New returns a Handler. A nil key gets a random one (tests).
func New(store *repo.Store, fetcher *proxy.Fetcher, authn *auth.Authenticator, baseURL func() string, key []byte) *Handler {
	if len(key) == 0 {
		key = make([]byte, 32)
		rand.Read(key)
	}
	return &Handler{Store: store, Fetcher: fetcher, Auth: authn, BaseURL: baseURL, Key: key, Now: time.Now}
}

// doc is a JSON document and, for a proxy, the cached upstream asset it
// came from (its staleness and modification time).
type doc struct {
	body []byte
	res  proxy.Result
}

// Access implements repo.AccessDecider: the ping and login endpoints are
// public, the rest follows the server's default rule.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	switch path {
	case "v1/ping", "v1/users/authenticate", "v1/users/check_credentials":
		return auth.Public
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return auth.Read
	case http.MethodDelete:
		return auth.Delete
	}
	return auth.Add
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
	allowed := allow(rt, repo.AcceptsWrites(rp))
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !slices.Contains(strings.Split(allowed, ", "), r.Method) {
		return repo.MethodNotAllowed(allowed, "method %s not allowed", r.Method)
	}
	switch rt.Kind {
	case KindPing:
		w.Header().Set("X-Conan-Server-Capabilities", Capabilities)
		w.WriteHeader(http.StatusOK)
		return nil
	case KindAuthenticate, KindCheckCredentials:
		p := auth.FromContext(r.Context())
		if p.Anonymous() {
			w.Header().Set("WWW-Authenticate", `Basic realm="commandcenter"`)
			return repo.Errorf(http.StatusUnauthorized, "authentication required")
		}
		body := p.User
		if rt.Kind == KindAuthenticate {
			body = h.mint(p.Owner())
		}
		repo.ServeText(w, r, body, ctText, time.Time{})
		return nil
	}
	if !get {
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allowed)
		}
		if target.WritePolicy == "deny" {
			return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", target.Name)
		}
		switch r.Method {
		case http.MethodPut:
			return h.put(w, r, target, rt)
		case http.MethodDelete:
			return h.remove(w, r, target, rt)
		}
		return h.v1UploadURLs(w, r, rp, target, rt)
	}
	ctx := r.Context()
	var d doc
	var err error
	switch rt.Kind {
	case KindFile, KindPkgFile:
		res, err := h.file(ctx, rp, rt)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		res.Asset.ContentType = contentType(rt.File)
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	case KindSearch:
		d, err = h.search(ctx, rp, r.URL.Query().Get("q"))
	default:
		if rt.V1 {
			d, err = h.v1(ctx, rp, rt)
		} else {
			d, err = h.doc(ctx, rp, rt)
		}
	}
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, d.res)
	repo.ServeText(w, r, string(d.body), ctJSON, d.res.Asset.LastModified)
	return nil
}

// contentType is the type a stored file is served with.
func contentType(file string) string {
	if strings.HasSuffix(file, ".txt") || strings.HasSuffix(file, ".py") {
		return ctText
	}
	return "application/octet-stream"
}

// repoURL is the absolute URL of rp without a trailing slash.
func (h *Handler) repoURL(rp *repo.Repository) string {
	base := ""
	if h.BaseURL != nil {
		base = strings.TrimRight(h.BaseURL(), "/")
	}
	return base + "/repository/" + rp.Name
}

// doc answers a v2 metadata route: computed from a hosted repository's rows
// and files, fetched (cached for metadataMaxAge) by a proxy, or the owning
// member's for a group.
func (h *Handler) doc(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	switch rp.Type {
	case "hosted":
		return h.hostedDoc(ctx, rp, rt)
	case "proxy":
		return h.fetchDoc(ctx, rp, proxy.Target{Key: rt.v2Path(), Remote: rt.v2Path(), Header: http.Header{"Accept": {ctJSON}}, NotHTML: true})
	case "group":
		owner, err := h.owner(ctx, rp, rt.Ref)
		if err != nil {
			return doc{}, err
		}
		return h.doc(ctx, owner, rt)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// file finds a stored, cached (fetched on demand) or owning member's file.
func (h *Handler) file(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, rt.v2Path())
		return proxy.Result{Asset: a}, err
	case "proxy":
		return h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: rt.v2Path(), Remote: rt.v2Path(), NotHTML: true}, proxy.Content)
	case "group":
		owner, err := h.owner(ctx, rp, rt.Ref)
		if err != nil {
			return proxy.Result{}, err
		}
		return h.file(ctx, owner, rt)
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// maxDoc bounds a document read back from the cache or the store.
const maxDoc = 16 << 20

// readDoc reads a stored document whole, within maxDoc.
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
		return nil, repo.Errorf(http.StatusBadGateway, "document %s is over %d MiB", a.Path, maxDoc>>20)
	}
	return raw, nil
}

// fetchDoc fetches a metadata document of rp's remote and reads it; it must
// be JSON (a Conan server's answer, never a web page).
func (h *Handler) fetchDoc(ctx context.Context, rp *repo.Repository, t proxy.Target) (doc, error) {
	res, err := h.Fetcher.FetchTarget(ctx, rp, t, proxy.Metadata)
	if err != nil {
		return doc{}, err
	}
	raw, err := h.readDoc(res.Asset)
	if err != nil {
		return doc{}, err
	}
	if !json.Valid(raw) {
		return doc{}, repo.Errorf(http.StatusBadGateway, "upstream answer for %s is not JSON", t.Key)
	}
	return doc{raw, res}, nil
}

// owner returns the member of group rp that answers for ref: the first
// online member, in order, that has the recipe (its latest answers 200).
// That member answers alone, so a later (public) member never stands in
// for an internal recipe.
func (h *Handler) owner(ctx context.Context, rp *repo.Repository, ref Ref) (*repo.Repository, error) {
	for _, m := range rp.OnlineMembers() {
		_, err := h.doc(ctx, m, Route{Kind: KindLatest, Ref: ref})
		if err == nil {
			return m, nil
		}
		if errors.Is(err, repo.ErrNotFound) || repo.StatusOf(err) == http.StatusNotFound {
			continue
		}
		return nil, memberFailure(rp, m, err)
	}
	return nil, repo.ErrNotFound
}

// memberFailure logs a group member's error and returns it as a 5xx: a
// later (public) member never stands in for a failing one.
func memberFailure(group, m *repo.Repository, err error) error {
	slog.Error("conan group member failed", "group", group.Name, "member", m.Name, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}

// maxSearchResults bounds a search answer.
const maxSearchResults = 1000

// search answers conans/search?q=<pattern>: {"results": [references]}. A
// hosted repository matches the pattern (fnmatch-style, case-insensitive;
// a bare word matches anywhere) against its recipe references, a proxy
// passes the upstream's answer through (cached under a key of the query)
// and a group merges its members' results.
func (h *Handler) search(ctx context.Context, rp *repo.Repository, q string) (doc, error) {
	switch rp.Type {
	case "hosted":
		return searchDoc(h.hostedSearch(ctx, rp, q))
	case "proxy":
		sum := sha256.Sum256([]byte(q))
		u := rp.RemoteURL.String() + "v2/conans/search?q=" + url.QueryEscape(q)
		return h.fetchDoc(ctx, rp, proxy.Target{Key: searchPrefix + hex.EncodeToString(sum[:8]) + ".json", URL: u, Header: http.Header{"Accept": {ctJSON}}, NotHTML: true})
	case "group":
		var results []string
		for _, m := range rp.OnlineMembers() {
			d, err := h.search(ctx, m, q)
			if err != nil {
				return doc{}, memberFailure(rp, m, err)
			}
			var got struct {
				Results []string `json:"results"`
			}
			if err := json.Unmarshal(d.body, &got); err != nil {
				return doc{}, memberFailure(rp, m, err)
			}
			results = append(results, got.Results...)
		}
		return searchDoc(results, nil)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// searchDoc sorts and deduplicates results into a search answer.
func searchDoc(results []string, err error) (doc, error) {
	if err != nil {
		return doc{}, err
	}
	sort.Strings(results)
	out := []string{}
	for i, r := range results {
		if i == 0 || r != results[i-1] {
			out = append(out, r)
		}
	}
	body, err := json.Marshal(map[string]any{"results": out})
	return doc{body: body}, err
}

// hostedSearch lists the references of hosted rp that match pattern. The
// longest literal piece of the pattern narrows the row scan; the pattern
// is then matched against name/version, name/version@user/channel and
// name/version@_/_ (how Conan 2 may spell a reference without user).
// ponytail: at most maxSearchResults names are scanned per search.
func (h *Handler) hostedSearch(ctx context.Context, rp *repo.Repository, pattern string) ([]string, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if !strings.ContainsAny(pattern, "*?") && !strings.Contains(pattern, "/") {
		pattern = "*" + pattern + "*"
	}
	re := globRE(pattern)
	chunk := ""
	for _, c := range strings.FieldsFunc(pattern, func(r rune) bool { return r == '*' || r == '?' || r == '@' }) {
		if len(c) > len(chunk) {
			chunk = c
		}
	}
	names, _, err := h.Store.DB.SearchPackageNames(ctx, rp.Name, chunk, maxSearchResults)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		l := strings.ToLower(n)
		alt := l
		if !strings.Contains(l, "@") {
			alt = l + "@_/_"
		}
		if re.MatchString(l) || re.MatchString(alt) {
			out = append(out, n)
		}
	}
	return out, nil
}

// globRE compiles a Conan search pattern: "*" matches anything, "?" one
// character, the rest is literal.
func globRE(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// timeFormat is how revision times are written: Artifactory's ISO 8601
// shape, which Conan 1 and 2 parse.
const timeFormat = "2006-01-02T15:04:05.000-0700"

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }
