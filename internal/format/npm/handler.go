package npm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

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
	flights  singleflight.Group // exclusive
}

func New(store *repo.Store, resolver *resolve.Resolver, authn *auth.Authenticator, baseURL func() string) *Handler {
	h := &Handler{Store: store, Auth: authn, BaseURL: baseURL, Now: time.Now}
	if resolver != nil { // its own copy: the other formats' proxies just fetch
		r := *resolver
		r.Proxy = h.proxyLookup
		h.Resolver = &r
	}
	return h
}

// Access implements repo.AccessDecider.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	if rt, err := ParseRoute(path); err == nil {
		switch rt.Kind {
		case KindLogin, KindWebLogin:
			return auth.Public // npm logs in without a token
		case KindToken:
			// The handler answers it itself: logout needs only the
			// caller's own token.
			return auth.Public
		case KindWhoami, KindPing, KindAudit, KindSearch:
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
		case KindRev:
			return auth.Delete // npm unpublish, by PUT or DELETE
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
	hd.Del("ETag")
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
		return h.audit(w, r, rp, rt.Path)
	case KindSearch:
		return h.search(w, r, rp)
	case KindRev:
		// npm reads a 404 here as "already deleted" and reports success
		// (QA H-2): unpublish deletes for real or fails visibly (QA N-10).
		return h.unpublish(w, r, rp, rt)
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
		if r.Method == http.MethodDelete && (rt.Kind == KindPackument || rt.Kind == KindTarball) && repo.AcceptsWrites(rp) {
			// Nexus also deletes without a revision (a cleanup script's
			// `curl -X DELETE <package or tarball URL>`): unpublish, unchecked.
			return h.unpublish(w, r, rp, Route{Kind: KindRev, Package: rt.Package, File: rt.File})
		}
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allowed(rp, rt.Kind))
		}
		return h.write(w, r, target, rt)
	}
	return repo.MethodNotAllowed(allowed(rp, rt.Kind), "method %s not allowed", r.Method)
}

// write handles publishing and dist-tag changes on a hosted repository. Its
// writePolicy decides as in the other formats: deny refuses every write
// (unpublish checks it itself), and only allow lets a publish replace a
// version (publish).
func (h *Handler) write(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	ctx := r.Context()
	if rp.WritePolicy == "deny" && (rt.Kind == KindPackument || rt.Kind == KindDistTag) {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
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

// tarballURL returns what a read of rp puts into a version's dist.tarball,
// given the URL stored there.
func (h *Handler) tarballURL(rp *repo.Repository, name string) func(version, upstream string) string {
	prefix := h.baseURL() + "/repository/" + rp.Name + "/" + name + "/-/"
	return func(version, upstream string) string { return prefix + tarballName(name, version, upstream) }
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	ctx := r.Context()
	if rt.Kind == KindTarball {
		res, err := h.tarball(ctx, rp, rt.Package, rt.File)
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
	if rt.Kind == KindVersion && rt.Version == "package.json" {
		// The document's stored path, which listings and the UI link to.
		rt.Kind = KindPackument
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
	members, err := h.docs(ctx, src, rt.Package)
	if err != nil {
		return err
	}
	// One document, or those a group with mergeMembers merges.
	docs := make([]io.ReadSeeker, len(members))
	hosted := false
	var modified time.Time
	for i, m := range members {
		f, err := h.Store.Open(m.res.Asset)
		if err != nil {
			return err
		}
		defer f.Close()
		docs[i] = f
		proxy.SetStaleHeader(w, m.res)
		hosted = hosted || m.repo.Type == "hosted"
		if lm := m.res.Asset.LastModified; lm.After(modified) {
			modified = lm
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !modified.IsZero() {
		w.Header().Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}
	etag := h.documentETag(rp, rt, members)
	w.Header().Set("ETag", etag)
	if rt.Kind == KindDistTags || hosted {
		// npm caches responses with Last-Modified heuristically, so
		// `npm dist-tag ls` right after `npm dist-tag add` saw the old tags,
		// and a warm cache kept offering an unpublished version. Dist-tags
		// and hosted documents change in place: always revalidate.
		cc := "no-cache"
		if prev := w.Header().Get("Cache-Control"); prev != "" {
			cc = prev + ", " + cc
		}
		w.Header().Set("Cache-Control", cc)
	}
	if notModified(r, etag, modified) {
		w.Header().Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	url := h.tarballURL(rp, rt.Package)
	head := r.Method == http.MethodHead
	switch {
	case rt.Kind == KindVersion:
		// A HEAD answers what the GET would: a missing version or tag is 404.
		out := io.Writer(w)
		if head {
			out = io.Discard
		}
		err = writeVersion(docs, rt.Version, url, out)
	case head:
	case len(docs) == 1 && rt.Kind == KindPackument:
		err = rewritePackument(docs[0], w, url)
	default:
		var tags []tagPair
		if tags, err = mergedTags(docs); err != nil {
			return err
		}
		if rt.Kind == KindDistTags {
			err = writeDistTags(w, tags)
		} else {
			err = mergeDocs(w, docs, tags, nil, url)
		}
	}
	if err == nil && head {
		w.WriteHeader(http.StatusOK)
	}
	return err
}

// documentRev is mixed into the ETags of the documents: change it when what
// a read makes of a stored document changes, so a client's copy is not kept.
const documentRev = "1"

// documentETag identifies what a read answers: the stored documents it is
// made from, and what it does with them (the repository and the base URL go
// into the tarball URLs).
func (h *Handler) documentETag(rp *repo.Repository, rt Route, members []member) string {
	s := sha256.New()
	for _, v := range []string{documentRev, h.baseURL(), rp.Name, strconv.Itoa(int(rt.Kind)), rt.Version} {
		io.WriteString(s, v+"\x00")
	}
	for _, m := range members {
		io.WriteString(s, m.res.Asset.BlobSHA256+"\x00")
	}
	return `"` + hex.EncodeToString(s.Sum(nil))[:32] + `"`
}

// notModified evaluates a GET or HEAD's conditions on a document with these
// validators: If-None-Match, else If-Modified-Since (RFC 9110 13.2.2).
func notModified(r *http.Request, etag string, modified time.Time) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, v := range strings.Split(inm, ",") {
			if v = strings.TrimPrefix(strings.TrimSpace(v), "W/"); v == etag || v == "*" {
				return true
			}
		}
		return false
	}
	t, err := http.ParseTime(r.Header.Get("If-Modified-Since"))
	return err == nil && !modified.IsZero() && !modified.Truncate(time.Second).After(t)
}

// login implements npm's legacy (CouchDB-style) login: it checks the password
// and answers with a new API token (the older ones of a user who logs in
// again and again go: auth.MaxLoginTokens).
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
	p, err := h.Auth.CheckRepoPassword(r.Context(), user, body.Password) // a user token's codes, or the password unless tokens are required
	if wait := auth.RetryAfter(w.Header(), err); wait > 0 {
		return repo.Errorf(http.StatusTooManyRequests, "too many failed sign-ins; try again in %d seconds", wait)
	}
	if errors.Is(err, auth.ErrUserTokenRequired) {
		return repo.Errorf(http.StatusUnauthorized, "a user token is required: log in with its name code and pass code")
	}
	if err != nil {
		return repo.Errorf(http.StatusUnauthorized, "invalid user name or password")
	}
	tok, err := h.Auth.CreateLoginToken(r.Context(), p) // "ldap:<id>" for an LDAP user
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
			return "GET, HEAD, PUT, DELETE"
		}
		return "GET, HEAD"
	case KindTarball:
		if writable {
			return "GET, HEAD, DELETE"
		}
		return "GET, HEAD"
	case KindVersion, KindDistTags:
		return "GET, HEAD"
	case KindDistTag:
		if writable {
			return "PUT, DELETE"
		}
	}
	return ""
}

// auditBodyMax bounds the audit request body forwarded to the remote.
const auditBodyMax = 16 << 20

// emptyAudit is the report of a registry that scans nothing, so `npm
// install` against a hosted repository does not fail on its audit step.
const emptyAudit = `{"actions":[],"advisories":{},"muted":[],"metadata":{"vulnerabilities":{"info":0,"low":0,"moderate":0,"high":0,"critical":0},"dependencies":0,"devDependencies":0,"optionalDependencies":0,"totalDependencies":0}}`

// audit forwards an npm audit request (POST /-/npm/v1/security/audits,
// /audits/quick, /advisories/bulk) to the remote of a proxy, or of a group's
// first proxy member, without caching; a hosted repository (or a group
// without a proxy) answers an empty report.
func (h *Handler) audit(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) error {
	if r.Method != http.MethodPost {
		return repo.MethodNotAllowed(http.MethodPost, "method %s not allowed", r.Method)
	}
	if target := auditTarget(rp); target != nil {
		return h.Resolver.Fetcher.Forward(r.Context(), target, w, r, path, auditBodyMax)
	}
	body := emptyAudit
	if strings.HasSuffix(path, "/advisories/bulk") {
		body = "{}"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body+"\n")
	return nil
}

// auditTarget is the proxy that answers audits for rp: rp itself, or the
// first proxy among a group's members (depth first); nil for hosted.
func auditTarget(rp *repo.Repository) *repo.Repository {
	if rp.Type == "proxy" {
		return rp
	}
	for _, m := range rp.MemberRepos {
		if t := auditTarget(m); t != nil {
			return t
		}
	}
	return nil
}
