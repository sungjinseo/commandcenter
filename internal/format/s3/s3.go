// Package s3 implements the S3 API for the s3 format: a hosted repository
// is a bucket and its assets are the bucket's objects. Requests reach it on
// the S3 host or port (server: Server.S3), signed with SigV4 by an S3
// access key (auth/s3key.go); the objects are ordinary assets, so cleanup,
// browsing, blob stores and the other repository features apply to them.
// Under /repository/<bucket>/ the same objects are read-only.
package s3

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/repo"
	"commandcenter/internal/sigv4"
)

// Handler serves the S3 API and the read-only /repository/ view.
type Handler struct {
	Store    *repo.Store
	Registry *repo.Registry
	Auth     *auth.Authenticator
	Config   config.S3

	// ReadOnly, if set, returns why writes are refused now (recovery mode).
	ReadOnly func() string
	// Unavailable, if set, returns why rp cannot serve on this node now.
	Unavailable func(rp *repo.Repository) string

	Now func() time.Time // nil: time.Now
}

// New returns a handler over store and registry.
func New(store *repo.Store, registry *repo.Registry, authn *auth.Authenticator, cfg config.S3) *Handler {
	return &Handler{Store: store, Registry: registry, Auth: authn, Config: cfg}
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// accessLock is the lock the web UI takes around repository deletes, which
// change the roles' privileges (ui.accessLock).
const accessLock = "access"

// host returns r's Host in lower case without its port.
func host(r *http.Request) string {
	hst := r.Host
	if h, _, err := net.SplitHostPort(hst); err == nil {
		hst = h
	}
	return strings.ToLower(strings.TrimSuffix(hst, "."))
}

// Match reports whether r is for the S3 host: s3.host, or a bucket under it
// when virtualHost is on.
func (h *Handler) Match(r *http.Request) bool {
	if h.Config.Host == "" {
		return false
	}
	hst := host(r)
	return hst == h.Config.Host || h.Config.VirtualHost && strings.HasSuffix(hst, "."+h.Config.Host)
}

// target returns the bucket and key r names: <bucket>.<host>/<key> for a
// virtual-hosted request, /<bucket>/<key> otherwise.
func (h *Handler) target(r *http.Request) (bucket, key string) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if h.Config.VirtualHost && h.Config.Host != "" {
		if b, ok := strings.CutSuffix(host(r), "."+h.Config.Host); ok {
			return b, p
		}
	}
	bucket, key, _ = strings.Cut(p, "/")
	return bucket, key
}

// BucketOf names the bucket of r, for the request metrics.
func (h *Handler) BucketOf(r *http.Request) string {
	b, _ := h.target(r)
	return b
}

// req is one S3 request on its way through the handler.
type req struct {
	w      http.ResponseWriter
	r      *http.Request
	ctx    context.Context
	p      auth.Principal
	sig    *sigv4.Request // nil when anonymous
	body   io.Reader      // the decoded body (aws-chunked undone)
	bucket string
	key    string
	q      map[string][]string
}

func (q *req) has(name string) bool { _, ok := q.q[name]; return ok }
func (q *req) get(name string) string {
	if v := q.q[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// ServeHTTP answers an S3 API request. The server's wrap has logged it and
// bounded its body; it has not authenticated it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := &req{w: w, r: r, ctx: r.Context(), body: r.Body, q: r.URL.Query()}
	q.bucket, q.key = h.target(r)
	w.Header().Set("Server", "CommandCenter")
	if err := h.authenticate(q); err != nil {
		writeError(w, r, err)
		return
	}
	q.ctx = auth.WithPrincipal(q.ctx, q.p)
	q.r = r.WithContext(q.ctx)
	if err := h.dispatch(q); err != nil {
		writeError(w, q.r, err)
	}
}

// authenticate checks the SigV4 signature of q, if any, and sets its
// principal and decoded body.
func (h *Handler) authenticate(q *req) error {
	sig, err := sigv4.Parse(q.r)
	if errors.Is(err, sigv4.ErrNotSigned) {
		return nil // anonymous: only what anonymous callers may read
	}
	if err != nil {
		return err
	}
	if sig.Service != "s3" {
		return &Error{Status: http.StatusBadRequest, Code: "AuthorizationHeaderMalformed", Msg: "the credential scope must name the s3 service"}
	}
	var secret string
	p, err := h.Auth.S3Check(auth.WithClientAddr(q.ctx, q.r.RemoteAddr), sig.AccessKey, func(s string) error {
		secret = s
		return sig.Verify(q.r, s, h.now())
	})
	if err != nil {
		return err
	}
	body, err := sig.Body(q.r, secret)
	if err != nil {
		return err
	}
	q.p, q.sig, q.body = p, sig, body
	return nil
}

// payloadHash is the hex SHA-256 the signature says the body has, "" when
// it says nothing checkable (unsigned, streaming or anonymous).
func (q *req) payloadHash() string {
	if q.sig == nil || q.sig.PayloadHash == sigv4.UnsignedPayload || sigv4.Streaming(q.sig.PayloadHash) {
		return ""
	}
	return strings.ToLower(q.sig.PayloadHash)
}

// subresources the API does not implement: a write of one is NotImplemented.
var unsupported = []string{"acl", "policy", "tagging", "lifecycle", "cors", "website", "versioning", "logging",
	"notification", "replication", "encryption", "object-lock", "retention", "legal-hold", "requestPayment",
	"accelerate", "analytics", "inventory", "metrics", "intelligent-tiering", "ownershipControls",
	"publicAccessBlock", "restore", "select", "torrent", "attributes", "versionId", "versions"}

func (h *Handler) unsupported(q *req) bool {
	for _, s := range unsupported {
		if q.has(s) {
			return true
		}
	}
	return false
}

func (h *Handler) dispatch(q *req) error {
	m := q.r.Method
	if q.bucket == "" {
		if m != http.MethodGet {
			return s3err(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
		}
		return h.listBuckets(q)
	}
	if q.key == "" {
		switch {
		case m == http.MethodPut && !h.unsupported(q):
			return h.createBucket(q)
		case m == http.MethodDelete && !h.unsupported(q):
			return h.deleteBucket(q)
		}
		rp, err := h.bucket(q, auth.Read)
		if err != nil {
			return err
		}
		switch {
		case m == http.MethodHead:
			q.w.Header().Set("X-Amz-Bucket-Region", h.Config.Region)
			q.w.WriteHeader(http.StatusOK)
			return nil
		case m == http.MethodGet && q.has("location"):
			return h.location(q)
		case m == http.MethodGet && q.has("versioning"):
			writeXML(q.w, q.r, versioningConfiguration{Xmlns: xmlns})
			return nil
		case m == http.MethodGet && q.has("acl"):
			return h.acl(q)
		case m == http.MethodGet && q.has("policy"):
			return s3err(http.StatusNotFound, "NoSuchBucketPolicy", "The bucket policy does not exist")
		case m == http.MethodGet && (q.has("tagging") || q.has("lifecycle") || q.has("cors") || q.has("encryption") ||
			q.has("website") || q.has("object-lock") || q.has("replication")):
			return s3err(http.StatusNotFound, "NoSuchConfiguration", "The specified configuration does not exist.")
		case h.unsupported(q):
			return errNotImplemented
		case m == http.MethodGet && q.has("uploads"):
			return h.listUploads(q, rp)
		case m == http.MethodGet:
			return h.listObjects(q, rp)
		case m == http.MethodPost && q.has("delete"):
			return h.deleteObjects(q, rp)
		}
		return s3err(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
	}
	if err := checkKey(q.key); err != nil {
		return err
	}
	if h.unsupported(q) && !(m == http.MethodGet && q.has("acl")) {
		return errNotImplemented
	}
	switch m {
	case http.MethodGet, http.MethodHead:
		rp, err := h.bucket(q, auth.Read)
		if err != nil {
			return err
		}
		switch {
		case q.has("uploadId") && m == http.MethodGet:
			return h.listParts(q, rp)
		case q.has("acl"):
			return h.acl(q)
		}
		return h.getObject(q, rp)
	case http.MethodPut:
		rp, err := h.bucket(q, auth.Add)
		if err != nil {
			return err
		}
		switch {
		case q.has("uploadId") || q.has("partNumber"):
			if q.r.Header.Get("X-Amz-Copy-Source") != "" {
				return errNotImplemented // UploadPartCopy
			}
			return h.uploadPart(q, rp)
		case q.r.Header.Get("X-Amz-Copy-Source") != "":
			return h.copyObject(q, rp)
		}
		return h.putObject(q, rp)
	case http.MethodDelete:
		if q.has("uploadId") {
			rp, err := h.bucket(q, auth.Add)
			if err != nil {
				return err
			}
			return h.abortUpload(q, rp)
		}
		rp, err := h.bucket(q, auth.Delete)
		if err != nil {
			return err
		}
		return h.deleteObject(q, rp)
	case http.MethodPost:
		rp, err := h.bucket(q, auth.Add)
		if err != nil {
			return err
		}
		switch {
		case q.has("uploads"):
			return h.createUpload(q, rp)
		case q.has("uploadId"):
			return h.completeUpload(q, rp)
		}
	}
	return s3err(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
}

// bucket resolves q's bucket for an operation needing acc: an s3
// repository the caller may use that way and that serves now.
func (h *Handler) bucket(q *req, acc auth.Access) (*repo.Repository, error) {
	rp := h.Registry.Lookup(q.ctx, q.bucket)
	if rp == nil || rp.Format != "s3" {
		if !h.Auth.Can(q.p, q.bucket, auth.Read) {
			return nil, errAccessDenied // whoever cannot read it does not learn whether it exists
		}
		return nil, errNoSuchBucket
	}
	allowed := h.Auth.Can(q.p, rp.Name, acc)
	if q.key != "" {
		allowed = h.Auth.CanPath(q.p, rp.Name, acc, q.key)
	}
	if !allowed {
		return nil, errAccessDenied
	}
	if !rp.IsOnline() {
		return nil, s3err(http.StatusServiceUnavailable, "ServiceUnavailable", "The bucket's repository is offline.")
	}
	if h.Unavailable != nil {
		if why := h.Unavailable(rp); why != "" {
			return nil, s3err(http.StatusServiceUnavailable, "ServiceUnavailable", why)
		}
	}
	if acc.IsWrite() {
		if h.ReadOnly != nil {
			if why := h.ReadOnly(); why != "" {
				return nil, s3err(http.StatusServiceUnavailable, "ServiceUnavailable", why)
			}
		}
		if rp.WritePolicy == "deny" {
			return nil, s3err(http.StatusForbidden, "AccessDenied", "The bucket is read-only.")
		}
	}
	return rp, nil
}

// checkKey refuses keys the repository cannot hold or serve: empty, over
// 1024 bytes, not UTF-8, with control characters, a leading "/" or a "."
// or ".." segment.
func checkKey(k string) error {
	if k == "" || len(k) > 1024 || !utf8.ValidString(k) || strings.HasPrefix(k, "/") {
		return errInvalidKey
	}
	for _, c := range k {
		if c < 0x20 || c == 0x7f {
			return errInvalidKey
		}
	}
	for _, s := range strings.Split(k, "/") {
		if s == "." || s == ".." {
			return errInvalidKey
		}
	}
	return nil
}

// Serve answers /repository/<bucket>/<key>: the objects, read-only.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		repo.WriteError(w, r, repo.MethodNotAllowed("GET, HEAD", "s3 repositories take writes through the S3 API"))
		return
	}
	if p == "" || strings.HasSuffix(p, "/") {
		h.serveDir(w, r, rp, p)
		return
	}
	a, err := h.Store.Get(r.Context(), rp.Name, p)
	if err != nil {
		repo.WriteError(w, r, err)
		return
	}
	h.serveObject(w, r, rp, a, nil)
}
