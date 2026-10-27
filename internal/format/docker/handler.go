package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Repos is the live registry of repositories (repo.Registry).
type Repos interface {
	Get(name string) *repo.Repository
	Snapshot() *repo.Snapshot
}

// Handler serves the registry API at /v2/ (ServeHTTP) and, for the web UI's
// download links, manifests and blobs under /repository/<repo>/ (Serve).
type Handler struct {
	Store   *repo.Store
	Auth    *auth.Authenticator
	Repos   Repos
	BaseURL func() string // live: read per request
	// Key signs the tokens the token endpoint issues (HMAC-SHA256); random
	// per process, so a restart invalidates them (clients fetch a new one).
	Key       []byte
	MaxUpload func() int64 // bound of one blob, summed over chunks; nil or 0: none
	Now       func() time.Time
	locks     repo.KeyedMutex // per image (manifests) and per upload session
}

func New(store *repo.Store, authn *auth.Authenticator, repos Repos, baseURL func() string, key []byte) *Handler {
	return &Handler{Store: store, Auth: authn, Repos: repos, BaseURL: baseURL, Key: key, Now: time.Now}
}

func (h *Handler) now() time.Time {
	if h.Now == nil {
		return time.Now()
	}
	return h.Now()
}

// caller is who makes a request: the server's principal (Basic or session),
// or the claims of a registry token (p is then anonymous).
type caller struct {
	p   auth.Principal
	tok *claims
}

func (c caller) anonymous() bool {
	if c.tok != nil {
		return c.tok.Sub == ""
	}
	return c.p.Anonymous()
}

// actor is the owner key recorded on events.
func (c caller) actor() string {
	if c.tok != nil {
		return c.tok.Sub
	}
	return c.p.Owner()
}

// principal is the live principal for privilege checks the token cannot
// answer (the catalog): the token's subject looked up again.
func (h *Handler) principal(c caller) auth.Principal {
	if c.tok == nil {
		return c.p
	}
	if c.tok.Sub == "" {
		return auth.Principal{}
	}
	p, _ := h.Auth.Lookup(c.tok.Sub)
	return p
}

// caller identifies r. The server authenticated Basic credentials and
// cookies; a Bearer token is a registry token, or else an API token the
// Authenticator knows.
func (h *Handler) caller(r *http.Request) (caller, error) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return caller{p: auth.FromContext(r.Context())}, nil
	}
	tok = strings.TrimSpace(tok)
	if c, err := h.verify(tok); err == nil {
		return caller{tok: c}, nil
	}
	p, err := h.Auth.Authenticate(r)
	if err != nil {
		return caller{}, Err(http.StatusUnauthorized, CodeUnauthorized, "invalid or expired token")
	}
	return caller{p: p}, nil
}

// allowed reports whether c may perform action on the registry name, whose
// first segment is the Command Center repository.
func (h *Handler) allowed(c caller, name, action string) bool {
	if c.tok != nil {
		return c.tok.has("repository", name, action)
	}
	repoName, _, _ := strings.Cut(name, "/")
	acc, ok := accessFor(action)
	return ok && h.Auth.Can(c.p, repoName, acc)
}

// deny is the error for a caller that may not: 401 (with the challenge) for
// an anonymous one, 403 for a signed-in one.
func deny(c caller) error {
	if c.anonymous() {
		return Err(http.StatusUnauthorized, CodeUnauthorized, "authentication required")
	}
	return Err(http.StatusForbidden, CodeDenied, "requested access to the resource is denied")
}

// neededAction is what r needs on its name: pull to read, push to write
// (cancelling an upload included), delete to delete a manifest.
func neededAction(r *http.Request, rt Route) string {
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		return "pull"
	case r.Method == http.MethodDelete && rt.Kind == KindManifest:
		return "delete"
	}
	return "push"
}

// ServeHTTP serves /v2/.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Docker-Distribution-API-Version", "registry/2.0")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "sandbox allow-downloads; default-src 'none'")
	if err := h.serve(w, r); err != nil {
		h.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) error {
	rt, ok := ParseRoute(strings.TrimPrefix(r.URL.Path, "/v2/"))
	if !ok {
		return Err(http.StatusNotFound, CodeUnsupported, "unknown registry route")
	}
	if rt.Kind == KindToken {
		return h.token(w, r)
	}
	c, err := h.caller(r)
	if err != nil {
		return err
	}
	switch rt.Kind {
	case KindBase:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
		}
		// Clients learn the token realm from this answer's challenge, so a
		// request without credentials gets it even where anonymous read is
		// on: the token endpoint then issues an anonymous token.
		if c.tok == nil && c.p.Anonymous() {
			return deny(c)
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	case KindCatalog:
		if r.Method != http.MethodGet {
			return repo.MethodNotAllowed("GET", "method %s not allowed", r.Method)
		}
		if c.tok == nil && c.p.Anonymous() || c.tok != nil && !c.tok.has("registry", "catalog", "*") {
			return deny(c)
		}
		return h.catalog(w, r, h.principal(c))
	}
	if !ValidName(rt.Name()) {
		return Err(http.StatusBadRequest, CodeNameInvalid, "invalid repository name "+rt.Name())
	}
	action := neededAction(r, rt)
	if !h.allowed(c, rt.Name(), action) {
		return deny(c)
	}
	rp := h.Repos.Get(rt.Repo)
	if rp == nil || rp.Format != "docker" {
		return errUnknownName(rt.Name())
	}
	if !rp.IsOnline() {
		return repo.Errorf(http.StatusServiceUnavailable, "repository is offline")
	}
	if action != "pull" {
		if rp.Type != "hosted" {
			return repo.MethodNotAllowed("GET, HEAD", "repository %q does not accept uploads", rp.Name)
		}
		if rp.WritePolicy == "deny" {
			return Err(http.StatusForbidden, CodeDenied, "repository "+rp.Name+" is read-only")
		}
	}
	switch rt.Kind {
	case KindManifest:
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			return h.getManifest(w, r, rp, rt)
		case http.MethodPut:
			return h.putManifest(w, r, rp, rt, c)
		case http.MethodDelete:
			if !ValidDigest(rt.Reference) {
				return Err(http.StatusBadRequest, CodeUnsupported, "delete a manifest by digest, not by tag")
			}
			if err := h.deleteManifest(r.Context(), rp, rt.Image, rt.Reference, c.actor()); err != nil {
				return err
			}
			w.WriteHeader(http.StatusAccepted)
			return nil
		}
		return repo.MethodNotAllowed("GET, HEAD, PUT, DELETE", "method %s not allowed", r.Method)
	case KindTags:
		if r.Method != http.MethodGet {
			return repo.MethodNotAllowed("GET", "method %s not allowed", r.Method)
		}
		return h.tags(w, r, rp, rt)
	case KindBlob:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
		}
		return h.getBlob(w, r, rp, rt)
	case KindUploads:
		if r.Method != http.MethodPost {
			return repo.MethodNotAllowed("POST", "method %s not allowed", r.Method)
		}
		return h.startUpload(w, r, rp, rt, c)
	}
	return h.upload(w, r, rp, rt)
}

// Serve implements repo.Handler for the web UI's download links: GET/HEAD
// of <image>/manifests/<reference> and <image>/blobs/<digest> under
// /repository/<repo>/. Clients use /v2/.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	rt, ok := ParseRoute(rp.Name + "/" + path)
	if !ok || (rt.Kind != KindManifest && rt.Kind != KindBlob) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		repo.WriteError(w, r, repo.Errorf(http.StatusNotFound, "docker repositories are served at /v2/%s/<image>/...", rp.Name))
		return
	}
	var err error
	if rt.Kind == KindManifest {
		err = h.getManifest(w, r, rp, rt)
	} else {
		err = h.getBlob(w, r, rp, rt)
	}
	if err != nil {
		repo.WriteError(w, r, err)
	}
}

// resolve returns the digest a reference (tag or digest) names.
func (h *Handler) resolve(ctx context.Context, rp *repo.Repository, image, ref string) (string, error) {
	if ValidDigest(ref) {
		return ref, nil
	}
	if ValidTag(ref) {
		tags, err := h.Store.DB.ListDistTags(ctx, rp.Name, image)
		if err != nil {
			return "", err
		}
		for _, t := range tags {
			if t.Tag == ref {
				return t.Version, nil
			}
		}
	}
	return "", Err(http.StatusNotFound, CodeManifestUnknown, "manifest unknown: "+ref)
}

func (h *Handler) getManifest(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	digest, err := h.resolve(r.Context(), rp, rt.Image, rt.Reference)
	if err != nil {
		return err
	}
	a, err := h.Store.DB.GetAsset(r.Context(), rp.Name, manifestKey(rt.Image, digest))
	if errors.Is(err, meta.ErrNotFound) {
		return Err(http.StatusNotFound, CodeManifestUnknown, "manifest unknown: "+rt.Reference)
	}
	if err != nil {
		return err
	}
	w.Header().Set("Docker-Content-Digest", digest)
	repo.ServeAsset(w, r, h.Store, a)
	return nil
}

func (h *Handler) getBlob(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if !ValidDigest(rt.Digest) {
		return Err(http.StatusBadRequest, CodeDigestInvalid, "invalid digest: "+rt.Digest)
	}
	a, err := h.Store.DB.GetAsset(r.Context(), rp.Name, blobKey(rt.Image, rt.Digest))
	if errors.Is(err, meta.ErrNotFound) {
		return Err(http.StatusNotFound, CodeBlobUnknown, "blob unknown to registry: "+rt.Digest)
	}
	if err != nil {
		return err
	}
	a.ContentType = "application/octet-stream"
	w.Header().Set("Docker-Content-Digest", rt.Digest)
	repo.ServeAsset(w, r, h.Store, a)
	return nil
}

// page applies the n/last pagination of the tags and catalog endpoints to
// the sorted list and sets the Link header when more follow.
func page(w http.ResponseWriter, r *http.Request, list []string) ([]string, error) {
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		list = list[sort.SearchStrings(list, last+"\x00"):]
	}
	if ns := q.Get("n"); ns != "" {
		n, err := strconv.Atoi(ns)
		if err != nil || n < 0 {
			return nil, Err(http.StatusBadRequest, CodeUnsupported, "invalid n")
		}
		if n < len(list) {
			list = list[:n]
			u := *r.URL
			v := u.Query()
			v.Set("last", list[len(list)-1])
			u.RawQuery = v.Encode()
			w.Header().Set("Link", `<`+u.RequestURI()+`>; rel="next"`)
		}
	}
	if list == nil {
		list = []string{}
	}
	return list, nil
}

func (h *Handler) tags(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	ctx := r.Context()
	st, err := h.Store.DB.PackageStamp(ctx, rp.Name, rt.Image)
	if err != nil {
		return err
	}
	if st.Count == 0 {
		return errUnknownName(rt.Name())
	}
	dt, err := h.Store.DB.ListDistTags(ctx, rp.Name, rt.Image)
	if err != nil {
		return err
	}
	tags := make([]string, 0, len(dt))
	for _, t := range dt {
		tags = append(tags, t.Tag)
	}
	sort.Strings(tags)
	if tags, err = page(w, r, tags); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": rt.Name(), "tags": tags})
	return nil
}

// catalog lists the images of the docker repositories p may read.
func (h *Handler) catalog(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var names []string
	for _, rp := range h.Repos.Snapshot().Sorted {
		if rp.Format != "docker" || !h.Auth.Can(p, rp.Name, auth.Read) {
			continue
		}
		images, err := h.Store.DB.ListPackageNames(r.Context(), rp.Name)
		if err != nil {
			return err
		}
		for _, img := range images {
			names = append(names, rp.Name+"/"+img)
		}
	}
	sort.Strings(names)
	names, err := page(w, r, names)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": names})
	return nil
}

func lockKey(rp *repo.Repository, image string) string { return rp.Name + "\x00" + image }

func offline(err error) error {
	if errors.Is(err, meta.ErrNotFound) {
		return nil
	}
	return err
}

// subject names a manifest in events.
func subject(image, ref string) string {
	if ValidDigest(ref) {
		return image + "@" + ref
	}
	return fmt.Sprintf("%s:%s", image, ref)
}
