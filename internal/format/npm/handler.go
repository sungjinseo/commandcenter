package npm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// Handler serves npm registries.
type Handler struct {
	Store    *repo.Store
	Resolver *resolve.Resolver
	Auth     *auth.Authenticator
	BaseURL  func() string // live: read per request (plan 12)
	Now      func() time.Time
	locks    repo.KeyedMutex // serializes writes per repository and package
}

func New(store *repo.Store, resolver *resolve.Resolver, authn *auth.Authenticator, baseURL func() string) *Handler {
	return &Handler{Store: store, Resolver: resolver, Auth: authn, BaseURL: baseURL, Now: time.Now}
}

// Access implements repo.AccessDecider.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	if rt, err := ParseRoute(path); err == nil {
		switch rt.Kind {
		case KindLogin, KindWebLogin:
			return auth.Public // npm logs in without a token
		case KindRev, KindToken:
			// The handler answers these itself: unpublish is refused for
			// every caller (405, not a 401/403 that npm would retry with
			// credentials), and logout needs only the caller's own token.
			return auth.Public
		case KindWhoami, KindPing, KindAudit:
			// A public ping or audit would tell a hidden repository from
			// a missing one.
			return auth.Read
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return auth.Read
	}
	if rt, err := ParseRoute(path); err == nil {
		switch rt.Kind {
		case KindDistTag, KindDistTags:
			return auth.Edit
		case KindPackument:
			if r.Method != http.MethodPut {
				break
			}
			// The same PUT publishes new versions (Add) or deprecates
			// existing ones (Edit), which only the body tells. A caller who
			// may edit but not add is checked for Edit here; publish then
			// checks each part of the document (publishAllowed).
			if h.Auth != nil && rp != nil {
				target := rp.Name
				if rp.WriteTarget != nil {
					target = rp.WriteTarget.Name
				}
				p := auth.FromContext(r.Context())
				if !h.Auth.Can(p, target, auth.Add) && h.Auth.Can(p, target, auth.Edit) {
					return auth.Edit
				}
			}
			return auth.Add
		}
	}
	if r.Method == http.MethodDelete {
		return auth.Delete
	}
	return auth.Add
}

// publishAllowed reports whether the caller may make a packument PUT on the
// hosted repository rp that adds versions (Add) and/or changes existing ones
// or only dist-tags (Edit). Without an Authenticator nothing is checked.
func (h *Handler) publishAllowed(ctx context.Context, rp *repo.Repository, adds, edits bool) bool {
	return (!adds || h.Auth.Permits(ctx, rp.Name, auth.Add)) && (!edits || h.Auth.Permits(ctx, rp.Name, auth.Edit))
}

// WriteError implements repo.ErrorWriter with npm's {"error": "..."} body.
func (h *Handler) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := repo.ErrorMessage(r, err)
	repo.SetAllow(w, err)
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// startedWriter records whether any response output has begun.
type startedWriter struct {
	http.ResponseWriter
	started bool
}

func (s *startedWriter) WriteHeader(code int) {
	s.started = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *startedWriter) Write(b []byte) (int, error) {
	s.started = true
	return s.ResponseWriter.Write(b)
}

// ReadFrom keeps sendfile-style copies working through the wrapper.
func (s *startedWriter) ReadFrom(src io.Reader) (int64, error) {
	s.started = true
	return io.Copy(s.ResponseWriter, src)
}

func (s *startedWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	sw := &startedWriter{ResponseWriter: w}
	rt, err := ParseRoute(path)
	if err == nil {
		err = h.serve(sw, r, rp, rt)
	}
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the client went away; nothing to report
	}
	if sw.started {
		slog.Error("npm response failed after output started", "path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
		panic(http.ErrAbortHandler)
	}
	hd := w.Header()
	hd.Del("Last-Modified")
	hd.Del("Warning")
	hd.Del("Content-Type")
	h.WriteError(w, r, err)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	switch rt.Kind {
	case KindPing:
		writeJSON(w, http.StatusOK, struct{}{})
		return nil
	case KindAudit:
		// An empty answer read as "found 0 vulnerabilities" (QA M-3).
		return repo.Errorf(http.StatusNotImplemented, "npm audit is not supported by this registry")
	case KindRev:
		// npm reads a 404 here as "already deleted" and reports success
		// (QA H-2); unpublish is not supported (plan 13 Decision 1), and the
		// message names only a means that exists (QA N-10).
		// The resource accepts no method: an empty Allow (RFC 9110 15.5.6).
		return repo.MethodNotAllowed("", "unpublish is not supported by this registry; use npm deprecate instead")
	case KindToken:
		return h.logout(w, r, rt.Token)
	case KindWebLogin:
		return repo.Errorf(http.StatusNotFound, "web login is not supported; npm falls back to legacy login")
	case KindWhoami:
		p := auth.FromContext(r.Context())
		if p.Anonymous() {
			w.Header().Set("WWW-Authenticate", `Basic realm="commandcenter"`)
			return repo.Errorf(http.StatusUnauthorized, "not logged in")
		}
		writeJSON(w, http.StatusOK, map[string]string{"username": p.User})
		return nil
	case KindLogin:
		if r.Method != http.MethodPut {
			return repo.MethodNotAllowed(http.MethodPut, "method %s not allowed", r.Method)
		}
		return h.login(w, r, rt.User)
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return h.read(w, r, rp, rt)
	case http.MethodPut, http.MethodDelete:
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allowed(rp, rt.Kind))
		}
		return h.write(w, r, target, rt)
	}
	return repo.MethodNotAllowed(allowed(rp, rt.Kind), "method %s not allowed", r.Method)
}

// write handles publishing and dist-tag changes on a hosted repository.
func (h *Handler) write(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	ctx := r.Context()
	switch {
	case rt.Kind == KindPackument && r.Method == http.MethodPut:
		if err := h.publish(ctx, w, rp, rt.Package, r, func(v string) {
			h.Store.Emit(r, "upload", rp.Name, rt.Package+"@"+v, "")
		}); err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
		return nil
	case rt.Kind == KindDistTag && r.Method == http.MethodPut:
		var version string
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&version); err != nil {
			return bodyDecodeErr(err, "dist-tag body must be a JSON string")
		}
		if err := h.setDistTag(ctx, rp, rt.Package, rt.Tag, version); err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
		return nil
	case rt.Kind == KindDistTag && r.Method == http.MethodDelete:
		if err := h.deleteDistTag(ctx, rp, rt.Package, rt.Tag); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return nil
	}
	return repo.MethodNotAllowed(allowed(rp, rt.Kind), "method %s not allowed here", r.Method)
}

func (h *Handler) tarballURL(rp *repo.Repository, name string) func(file string) string {
	prefix := h.baseURL() + "/repository/" + rp.Name + "/" + name + "/-/"
	return func(file string) string { return prefix + file }
}

// packumentTarget is the cache key and upstream request of a package document.
func packumentTarget(name string) proxy.Target {
	return proxy.Target{Key: PackumentKey(name), Remote: name, Header: http.Header{"Accept": {"application/json"}}}
}

// packument resolves the stored package document for name in rp and the
// repository it comes from. A group answers with the first member that has
// the package, alone (resolve.Owner): no later member's versions, dist-tags
// or tarballs are mixed in (dependency confusion, QA M-4, N-1, N-11).
func (h *Handler) packument(ctx context.Context, rp *repo.Repository, name string) (*repo.Repository, proxy.Result, error) {
	return h.Resolver.Owner(ctx, rp, packumentTarget(name), proxy.Metadata)
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	ctx := r.Context()
	if rt.Kind == KindTarball {
		// A group takes tarballs only from the member that owns the
		// package, so a later member's version is not served even by its
		// direct URL (QA N-1). A member that has the tarball needs no
		// packument (resolve.OwnerFile).
		key := TarballKey(rt.Package, rt.File)
		res, err := h.Resolver.OwnerFile(ctx, rp, proxy.Target{Key: key, Remote: key}, packumentTarget(rt.Package))
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		a := res.Asset
		a.ContentType = "application/octet-stream"
		repo.ServeAsset(w, r, h.Store, a)
		return nil
	}
	if rt.Kind != KindPackument && rt.Kind != KindVersion && rt.Kind != KindDistTags {
		return repo.MethodNotAllowed(allowed(rp, rt.Kind), "method %s not allowed", r.Method)
	}

	src := rp
	if rp.Type == "group" && rp.WriteTarget != nil && r.URL.Query().Get("write") == "true" {
		// npm reads with ?write=true before sending the whole document back
		// (deprecate, dist-tag, owner): it must hold only what the
		// writeMember stores, not another member's versions (QA M-7). An
		// offline writeMember is a 503.
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return err
		}
		src = target
	}
	_, res, err := h.packument(ctx, src, rt.Package)
	if err != nil {
		return err
	}
	a := res.Asset
	f, err := h.Store.Open(a)
	if err != nil {
		return err
	}
	defer f.Close()
	proxy.SetStaleHeader(w, res)
	w.Header().Set("Content-Type", "application/json")
	if !a.LastModified.IsZero() {
		w.Header().Set("Last-Modified", a.LastModified.UTC().Format(http.TimeFormat))
	}
	if rt.Kind == KindDistTags {
		// npm caches responses with Last-Modified heuristically, so
		// `npm dist-tag ls` right after `npm dist-tag add` saw the old tags.
		// Dist-tags are tiny and mutable: always revalidate.
		cc := "no-cache"
		if prev := w.Header().Get("Cache-Control"); prev != "" {
			cc = prev + ", " + cc
		}
		w.Header().Set("Cache-Control", cc)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	switch rt.Kind {
	case KindVersion:
		return writeVersionDoc(f, rt.Version, h.tarballURL(rp, rt.Package), w)
	case KindDistTags:
		tags, err := readDistTags(f)
		if err != nil {
			return err
		}
		return writeDistTags(w, tags)
	}
	return rewritePackument(f, w, h.tarballURL(rp, rt.Package))
}

// login implements npm's legacy (CouchDB-style) login: it checks the password
// and answers with a new API token.
func (h *Handler) login(w http.ResponseWriter, r *http.Request, user string) error {
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		return bodyDecodeErr(err, "invalid login request")
	}
	if body.Name != "" && body.Name != user {
		return repo.Errorf(http.StatusBadRequest, "user name %q does not match URL %q", body.Name, user)
	}
	p, err := h.Auth.CheckPassword(r.Context(), user, body.Password)
	if err != nil {
		return repo.Errorf(http.StatusUnauthorized, "invalid user name or password")
	}
	_, tok, err := h.Auth.CreateTokenFor(r.Context(), p) // "ldap:<id>" for an LDAP user
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "id": "org.couchdb.user:" + user, "rev": "_we_dont_use_revs_any_more", "token": tok,
	})
	return nil
}

// logout implements `npm logout` (DELETE /-/user/token/<token>): the token is
// revoked when it belongs to the caller (QA M-6). Any other token, or one
// that does not exist, is a 404 whatever its owner.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request, token string) error {
	if r.Method != http.MethodDelete {
		return repo.MethodNotAllowed(http.MethodDelete, "method %s not allowed", r.Method)
	}
	p := auth.FromContext(r.Context())
	if p.Anonymous() {
		w.Header().Set("WWW-Authenticate", `Basic realm="commandcenter"`)
		return repo.Errorf(http.StatusUnauthorized, "not logged in")
	}
	err := h.Auth.RevokeToken(r.Context(), token, p)
	if errors.Is(err, meta.ErrNotFound) {
		return repo.Errorf(http.StatusNotFound, "token not found")
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// bodyDecodeErr maps a failed JSON decode of the request body: an exceeded
// upload cap (413) or a stalled body (408) passes through unchanged, anything
// else is a 400.
func bodyDecodeErr(err error, what string) error {
	if errors.As(err, new(*http.MaxBytesError)) || errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	return repo.Errorf(http.StatusBadRequest, "%s: %v", what, err)
}

// baseURL is the current external URL without a trailing slash.
func (h *Handler) baseURL() string {
	if h.BaseURL == nil {
		return ""
	}
	return strings.TrimRight(h.BaseURL(), "/")
}

// allowed lists the methods the resource of kind accepts in rp, for the Allow
// header of a 405 (a GET answered with 405 must not be told GET is allowed).
func allowed(rp *repo.Repository, kind Kind) string {
	writable := repo.AcceptsWrites(rp)
	switch kind {
	case KindPackument:
		if writable {
			return "GET, HEAD, PUT"
		}
		return "GET, HEAD"
	case KindVersion, KindTarball, KindDistTags:
		return "GET, HEAD"
	case KindDistTag:
		if writable {
			return "PUT, DELETE"
		}
	}
	return ""
}
