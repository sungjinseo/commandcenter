// Package s3api serves an S3-compatible API on a listener of its own
// (config s3.listen), so that S3 clients (aws cli, mc, boto3, rclone, s3cmd,
// restic, Terraform's S3 backend) use Command Center as they use AWS S3 or
// MinIO. The specification is AWS's public S3 API reference; where it is
// vague, how MinIO behaves was read from its source (never copied).
//
// A bucket is a hosted raw repository and an object is the raw file at the
// same path, so everything raw has applies unchanged: privileges (read for
// GET, HEAD and listings, add for a new key, edit to overwrite, delete),
// writePolicy, strict content validation, blob stores, quotas, cleanup,
// browse and search, and a file put through S3 downloads at
// /repository/<bucket>/<key> and the other way round. Path-style addressing
// only (http://host:9000/bucket/key).
//
// Callers sign with AWS Signature Version 4 (sigv4.go) using an S3 access
// key a user made (meta.S3Key); a request without a signature is
// anonymous and gets the repository's anonymous read.
package s3api

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Handler serves the S3 API.
type Handler struct {
	Registry *repo.Registry
	Store    *repo.Store
	Auth     *auth.Authenticator
	Region   string // the region clients sign for (config s3.region)
	Log      *slog.Logger

	// ReadOnly, Unavailable, Requests and Seen are server.Server's: recovery
	// mode, a repository whose blob store did not open here, and the
	// request counters.
	ReadOnly    func() string
	Unavailable func(rp *repo.Repository) string
	Requests    func(repo string, status int, bytesIn, bytesOut int64, d time.Duration)
	Seen        func(user, addr string)

	// UploadIdle is how long a request body may stall; 0 means a minute.
	UploadIdle time.Duration

	now func() time.Time // test hook; nil is time.Now
}

func (h *Handler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// maxUpload is the live maxUploadMiB setting in bytes (0: no cap).
func (h *Handler) maxUpload() int64 {
	return int64(h.Auth.Snapshot().Settings().MaxUploadMiB) << 20
}

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := r.Header.Get("X-Request-Id")
	if !requestIDRE.MatchString(id) {
		b := make([]byte, 8)
		rand.Read(b)
		id = hex.EncodeToString(b)
		r.Header.Set("X-Request-Id", id)
	}
	rec := &recorder{ResponseWriter: w}
	hd := rec.Header()
	hd.Set("X-Amz-Request-Id", id)
	hd.Set("Server", "CommandCenter")
	// Stored content is untrusted, as on /repository/: a presigned link
	// opened in a browser must not run it.
	hd.Set("Content-Security-Policy", "sandbox allow-downloads; default-src 'none'")
	hd.Set("X-Content-Type-Options", "nosniff")
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	var p auth.Principal
	defer func() {
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		h.Log.Info("request", "id", id, "api", "s3", "method", r.Method, "path", logsafe.Path(r.URL.Path), "status", status,
			"bytes", rec.bytes, "ms", time.Since(start).Milliseconds(), "user", p.Owner(), "remote", r.RemoteAddr)
		if h.Requests != nil && bucket != "" && h.Registry.Get(bucket) != nil {
			h.Requests(bucket, status, max(r.ContentLength, 0), rec.bytes, time.Since(start))
			if h.Seen != nil {
				h.Seen(p.Owner(), r.RemoteAddr)
			}
		}
	}()
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	if limit := h.maxUpload(); limit > 0 && !get {
		r.Body = http.MaxBytesReader(rec, r.Body, limit+limit/8) // the aws-chunked framing on top of the content
	}
	idle := h.UploadIdle
	if idle <= 0 {
		idle = time.Minute
	}
	r.Body = repo.IdleBody(rec, r, idle)
	defer r.Body.Close()
	p, sig, err := h.authenticate(r)
	if err != nil {
		writeError(rec, r, err)
		return
	}
	ctx := auth.WithClientAddr(auth.WithPrincipal(r.Context(), p), r.RemoteAddr)
	r = r.WithContext(ctx)
	if err := h.route(rec, r, p, sig, bucket, key); err != nil {
		writeError(rec, r, err)
	}
}

// authenticate checks r's signature and returns its key's user, or the
// anonymous principal for an unsigned request.
func (h *Handler) authenticate(r *http.Request) (auth.Principal, *signer, error) {
	if !signed(r) {
		return auth.Principal{}, nil, nil
	}
	s, err := parseSigned(r)
	if err != nil {
		return auth.Principal{}, nil, err
	}
	malformed := "AuthorizationHeaderMalformed"
	if s.presigned {
		malformed = "AuthorizationQueryParametersError"
	}
	switch {
	case s.cred.service != "s3":
		return auth.Principal{}, nil, errf(http.StatusBadRequest, malformed, "the credential's service must be s3, not %q", s.cred.service)
	case s.cred.region != h.Region:
		return auth.Principal{}, nil, errf(http.StatusBadRequest, malformed, "the credential's region %q is wrong: this server is %q", s.cred.region, h.Region)
	case s.cred.date != s.at.UTC().Format(dateFormat):
		return auth.Principal{}, nil, errf(http.StatusBadRequest, malformed, "the credential's date is not the request's")
	}
	if err := s.checkTime(h.clock()); err != nil {
		return auth.Principal{}, nil, err
	}
	k, err := h.Store.DB.GetS3Key(r.Context(), s.cred.key)
	if errors.Is(err, meta.ErrNotFound) {
		return auth.Principal{}, nil, errf(http.StatusForbidden, "InvalidAccessKeyId", "the access key id does not exist")
	}
	if err != nil {
		return auth.Principal{}, nil, err
	}
	key := signingKey(k.Secret, s.cred)
	if !hmac.Equal([]byte(s.expected(r, key)), []byte(s.signature)) {
		return auth.Principal{}, nil, errf(http.StatusForbidden, "SignatureDoesNotMatch", "the request signature does not match the one calculated with the key's secret")
	}
	// The owner is looked up on every request: a disabled or deleted user's
	// keys stop working at once.
	p, ok := h.Auth.Lookup(k.Owner)
	if !ok {
		return auth.Principal{}, nil, errf(http.StatusForbidden, "InvalidAccessKeyId", "the access key's user is disabled or does not exist")
	}
	if time.Since(k.LastUsed) >= meta.TokenTouchInterval {
		h.Store.DB.TouchS3Key(r.Context(), k.ID)
	}
	p.Via = auth.ViaToken // a key, like a token, never stands for a password or session
	return p, &signer{key: key, at: s.at, scope: s.cred.scope(), signature: s.signature, payload: s.payload}, nil
}

// unsupported are the subresources that select operations Command Center
// does not implement; a request naming one answers NotImplemented.
var unsupported = []string{"acl", "policy", "policyStatus", "tagging", "cors", "lifecycle", "website", "logging", "notification",
	"replication", "encryption", "object-lock", "retention", "legal-hold", "torrent", "restore", "select", "attributes", "versions",
	"accelerate", "analytics", "inventory", "metrics", "intelligent-tiering", "ownershipControls", "publicAccessBlock",
	"requestPayment", "renameObject", "session"}

// route dispatches by the path's bucket and key, the method and the
// subresource in the query.
func (h *Handler) route(w http.ResponseWriter, r *http.Request, p auth.Principal, sig *signer, bucket, key string) error {
	q := r.URL.Query()
	for _, s := range unsupported {
		if q.Has(s) {
			return notImplemented("the ?" + s + " subresource")
		}
	}
	if bucket == "" {
		if r.Method != http.MethodGet {
			return errf(http.StatusMethodNotAllowed, "MethodNotAllowed", "the service root only answers GET (ListBuckets)")
		}
		return h.listBuckets(w, r, p)
	}
	if key == "" {
		switch {
		case r.Method == http.MethodPut && len(q) == 0:
			return h.createBucket(w, r, p, bucket)
		case r.Method == http.MethodDelete && len(q) == 0:
			return h.deleteBucket(r, p, bucket)
		}
		rp, err := h.bucket(r, p, bucket, auth.Browse, "")
		if err != nil {
			return err
		}
		switch {
		case r.Method == http.MethodHead:
			w.Header().Set("X-Amz-Bucket-Region", h.Region)
			return nil
		case r.Method == http.MethodGet && q.Has("location"):
			return h.location(w)
		case r.Method == http.MethodGet && q.Has("versioning"):
			writeXML(w, struct {
				XMLName struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ VersioningConfiguration"`
			}{})
			return nil
		case r.Method == http.MethodGet && q.Has("uploads"):
			return h.listUploads(w, r, rp)
		case r.Method == http.MethodGet:
			return h.listObjects(w, r, p, rp)
		case r.Method == http.MethodPost && q.Has("delete"):
			return h.deleteObjects(w, r, p, sig, rp)
		}
		return notImplemented(r.Method + " on a bucket with this query")
	}
	uploadID := q.Get("uploadId")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if uploadID != "" && r.Method == http.MethodGet {
			return h.listParts(w, r, p, bucket, key, uploadID)
		}
		return h.getObject(w, r, p, bucket, key)
	case http.MethodPut:
		switch {
		case uploadID != "":
			return h.uploadPart(w, r, p, sig, bucket, key, uploadID)
		case r.Header.Get("X-Amz-Copy-Source") != "":
			return h.copyObject(w, r, p, sig, bucket, key)
		}
		return h.putObject(w, r, p, sig, bucket, key)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			return h.createUpload(w, r, p, bucket, key)
		case uploadID != "":
			return h.completeUpload(w, r, p, sig, bucket, key, uploadID)
		}
		return notImplemented("POST object uploads (browser forms)")
	case http.MethodDelete:
		if uploadID != "" {
			return h.abortUpload(w, r, p, bucket, key, uploadID)
		}
		return h.deleteObject(w, r, p, bucket, key)
	}
	return errf(http.StatusMethodNotAllowed, "MethodNotAllowed", "method %s is not allowed on an object", r.Method)
}

// bucket returns the hosted raw repository name names when p may do acc
// at path there ("" for the bucket as a whole), as the repository endpoint
// decides it: the privilege (a selector's on the path), the repository
// online and served here, and for a write, no recovery mode.
func (h *Handler) bucket(r *http.Request, p auth.Principal, name string, acc auth.Access, path string) (*repo.Repository, error) {
	rp := h.Registry.Lookup(r.Context(), name)
	if rp == nil || rp.Format != "raw" || rp.Type != "hosted" {
		// Whoever could not read a repository of this name gets the answer
		// of an unreadable one, so hidden names cannot be probed.
		if !h.Auth.Can(p, name, auth.Read) {
			return nil, denied(p)
		}
		return nil, errNoSuchBucket
	}
	allowed := h.Auth.Can(p, rp.Name, acc)
	if path != "" {
		allowed = h.Auth.CanPath(p, rp.Name, acc, path)
	}
	if !allowed {
		return nil, denied(p)
	}
	if !rp.IsOnline() {
		return nil, errf(http.StatusServiceUnavailable, "ServiceUnavailable", "the repository is offline")
	}
	if h.Unavailable != nil {
		if why := h.Unavailable(rp); why != "" {
			return nil, errf(http.StatusServiceUnavailable, "ServiceUnavailable", "%s", why)
		}
	}
	if acc.IsWrite() && h.ReadOnly != nil {
		if why := h.ReadOnly(); why != "" {
			return nil, errf(http.StatusServiceUnavailable, "ServiceUnavailable", "%s", why)
		}
	}
	return rp, nil
}

func denied(p auth.Principal) error {
	if p.Anonymous() {
		return errf(http.StatusForbidden, "AccessDenied", "access denied: sign the request with an S3 access key")
	}
	return errf(http.StatusForbidden, "AccessDenied", "user %q is not allowed to do this", p.User)
}

// createBucket does not create repositories: as on the native API, a
// change to the configuration needs an interactive sign-in, which a key is
// not. An existing bucket the caller may write to answers
// BucketAlreadyOwnedByYou, which clients that create their bucket first
// (rclone, mc mb --ignore-existing) accept.
func (h *Handler) createBucket(w http.ResponseWriter, r *http.Request, p auth.Principal, name string) error {
	io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10)) // the optional CreateBucketConfiguration
	if _, err := h.bucket(r, p, name, auth.Add, ""); err == nil {
		return errf(http.StatusConflict, "BucketAlreadyOwnedByYou", "the bucket exists and you may write to it")
	} else if asS3Error(err).code != "NoSuchBucket" {
		return err
	}
	return errf(http.StatusForbidden, "AccessDenied", "buckets are hosted raw repositories: an administrator creates them in Command Center")
}

// deleteBucket does not delete repositories, for the reason createBucket
// does not create them.
func (h *Handler) deleteBucket(r *http.Request, p auth.Principal, name string) error {
	if _, err := h.bucket(r, p, name, auth.Read, ""); err != nil {
		return err
	}
	return errf(http.StatusForbidden, "AccessDenied", "buckets are hosted raw repositories: an administrator deletes them in Command Center")
}

func (h *Handler) location(w http.ResponseWriter) error {
	region := h.Region
	if region == "us-east-1" {
		region = "" // AWS answers the classic region with an empty constraint
	}
	writeXML(w, struct {
		XMLName struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
		Region  string   `xml:",chardata"`
	}{Region: region})
	return nil
}

// recorder remembers the response status and body bytes.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *recorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *recorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the underlying writer's sendfile fast path.
func (s *recorder) ReadFrom(r io.Reader) (int64, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := io.Copy(s.ResponseWriter, r)
	s.bytes += n
	return n, err
}

func (s *recorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
