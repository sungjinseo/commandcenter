package blob

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// GCSConfig describes a Google Cloud Storage bucket that holds a store's
// blobs at <Prefix>/<sha256>, reached with the GCS JSON API. AccountKey is
// sealed in the database like the other stored secrets and never shown.
// (GCS also speaks the S3 API with HMAC keys, so an S3 store can use a
// bucket too; this type needs no HMAC keys and takes Google's credentials.)
type GCSConfig struct {
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix,omitempty"`   // object name prefix, no leading or trailing slash; empty: bucket root
	Endpoint string `json:"endpoint,omitempty"` // https://storage.googleapis.com when empty; an emulator's or a private endpoint's URL
	// Authentication is GCSAuthKey, GCSAuthDefault or GCSAuthNone; Validate
	// fills an empty one in (GCSAuthKey with AccountKey, else GCSAuthDefault).
	Authentication string `json:"authentication,omitempty"`
	AccountKey     string `json:"accountKey,omitempty"` // a service account key file's JSON, for GCSAuthKey
}

const (
	GCSAuthKey     = "accountKey"         // the service account key in AccountKey
	GCSAuthDefault = "applicationDefault" // the key file GOOGLE_APPLICATION_CREDENTIALS names, else the metadata server (GKE Workload Identity, GCE)
	GCSAuthNone    = "none"               // no credentials: an emulator
)

const gcsDefaultEndpoint = "https://storage.googleapis.com"

var gcsBucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*[a-z0-9]$`)

// validGCSBucket follows Cloud Storage's bucket names: 3 to 63 characters,
// or up to 222 with dots and 63 between them.
func validGCSBucket(b string) bool {
	if len(b) < 3 || len(b) > 222 || !gcsBucketRE.MatchString(b) {
		return false
	}
	for _, p := range strings.Split(b, ".") {
		if p == "" || len(p) > 63 {
			return false
		}
	}
	return true
}

// Validate reports the first thing wrong with c, after trimming the slashes
// of its prefix and filling in its authentication. It parses the account key
// but does not touch the bucket. No error holds the key.
func (c *GCSConfig) Validate() error {
	c.Prefix = strings.Trim(c.Prefix, "/")
	if c.Authentication == "" {
		c.Authentication = GCSAuthDefault
		if c.AccountKey != "" {
			c.Authentication = GCSAuthKey
		}
	}
	switch {
	case !validGCSBucket(c.Bucket):
		return errors.New("bucket must be a Cloud Storage bucket name: 3 to 63 lower-case letters, digits, hyphens and underscores (up to 222 with dots)")
	case c.Prefix != "" && !s3PrefixRE.MatchString(c.Prefix):
		return errors.New("prefix may hold letters, digits, slashes, dots, underscores and hyphens")
	case c.Authentication != GCSAuthKey && c.Authentication != GCSAuthDefault && c.Authentication != GCSAuthNone:
		return fmt.Errorf("authentication must be %q, %q or %q", GCSAuthKey, GCSAuthDefault, GCSAuthNone)
	case c.Authentication == GCSAuthKey && c.AccountKey == "":
		return errors.New(`authentication "accountKey" needs accountKey: the JSON of a service account key file`)
	case c.Authentication != GCSAuthKey && c.AccountKey != "":
		return fmt.Errorf("accountKey goes with authentication %q", GCSAuthKey)
	case c.Authentication == GCSAuthNone && c.Endpoint == "":
		return errors.New(`authentication "none" is for an emulator: set its endpoint`)
	}
	if err := validEndpoint(c.Endpoint); err != nil {
		return errors.New("endpoint " + err.Error())
	}
	if c.Authentication == GCSAuthKey {
		if _, err := parseServiceAccount([]byte(c.AccountKey)); err != nil {
			return fmt.Errorf("accountKey: %w", err)
		}
	}
	return nil
}

// endpointURL is the endpoint in one spelling (S3's rules: scheme and host
// lower-cased, no default port, no trailing slash).
func (c GCSConfig) endpointURL() *url.URL {
	ep := c.Endpoint
	if ep == "" {
		ep = gcsDefaultEndpoint
	}
	return S3Config{Endpoint: ep}.endpointURL()
}

// Location names the object namespace c targets like S3Config.Location: a
// GCS store on the default endpoint and an S3 store on the bucket through
// storage.googleapis.com (HMAC interoperability) compare equal.
func (c GCSConfig) Location() string {
	return c.endpointURL().String() + "/" + c.Bucket + "/" + strings.Trim(c.Prefix, "/")
}

// ServiceAccount is the account key's client_email, "" without a key.
func (c GCSConfig) ServiceAccount() string {
	var k struct {
		ClientEmail string `json:"client_email"`
	}
	json.Unmarshal([]byte(c.AccountKey), &k)
	return k.ClientEmail
}

// Tunables a test lowers.
var (
	gcsChunkSize int64 = 16 << 20 // the largest object sent in one request; a larger one goes up resumably in chunks of this size (a multiple of 256 KiB)
	gcsPageSize        = 1000     // objects per list page
	gcsRetries         = 4        // retries of a request answered 408, 429 or 5xx, or not at all
	// gcsBackoff is the wait before retry n (0-based): Google's truncated
	// exponential backoff, 1 s doubling, with jitter.
	gcsBackoff = func(n int) time.Duration {
		d := time.Second << min(n, 5)
		return d/2 + rand.N(d)
	}
)

// gcs is the backend for one bucket and prefix.
type gcs struct {
	cfg    GCSConfig
	base   string // endpoint URL, one spelling, no trailing slash
	client *http.Client
	auth   *gcsAuth // nil: no Authorization header
}

var _ metaBackend = (*gcs)(nil)

// OpenGCS opens a store whose blobs live in the bucket cfg describes;
// root/tmp holds its uploads in progress and Docker sessions, as for Open.
// It reads a GOOGLE_APPLICATION_CREDENTIALS key file but sends nothing:
// tokens are fetched on first use.
func OpenGCS(root string, cfg GCSConfig) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	be, err := newGCS(cfg)
	if err != nil {
		return nil, err
	}
	return open(root, be)
}

func newGCS(cfg GCSConfig) (*gcs, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.DisableCompression = true // a gzipped answer would hide the object's size
	// The JSON API never redirects a request this backend sends (a 308 is
	// a resumable upload's progress, read as it is).
	be := &gcs{cfg: cfg, base: cfg.endpointURL().String(),
		client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	tokens := &http.Client{Timeout: 30 * time.Second}
	switch cfg.Authentication {
	case GCSAuthKey:
		sa, err := parseServiceAccount([]byte(cfg.AccountKey))
		if err != nil {
			return nil, fmt.Errorf("accountKey: %w", err)
		}
		be.auth = newGCSAuth(sa.fetcher(tokens, time.Now))
	case GCSAuthDefault:
		fetch, err := defaultCredentials(tokens)
		if err != nil {
			return nil, err
		}
		be.auth = newGCSAuth(fetch)
	}
	return be, nil
}

func (g *gcs) key(sha string) string {
	if g.cfg.Prefix == "" {
		return sha
	}
	return g.cfg.Prefix + "/" + sha
}

func (g *gcs) bucketURL() string { return g.base + "/storage/v1/b/" + url.PathEscape(g.cfg.Bucket) }

func (g *gcs) objectURL(name string, q url.Values) string {
	return g.bucketURL() + "/o/" + url.PathEscape(name) + "?" + q.Encode()
}

func (g *gcs) uploadURL(rawQuery string) string {
	return g.base + "/upload/storage/v1/b/" + url.PathEscape(g.cfg.Bucket) + "/o?" + rawQuery
}

// gcsError is an answer the backend did not want. A 404 is ErrNotFound.
type gcsError struct {
	status int
	msg    string
}

func (e *gcsError) Error() string { return e.msg }
func (e *gcsError) Is(target error) bool {
	return target == ErrNotFound && e.status == http.StatusNotFound
}

// permanent marks an error a retry would only repeat (a token endpoint
// refusing the key).
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

func gcsRetryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// try sends one attempt of r with the bearer token: a response of any
// status, or an error when there was none (no token, no answer).
func (g *gcs) try(r *http.Request) (*http.Response, error) {
	if g.auth != nil {
		tok, err := g.auth.token()
		if err != nil {
			return nil, fmt.Errorf("gcs %s %s: credentials: %w", r.Method, r.URL.Path, err)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := g.client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("gcs %s %s: %w", r.Method, r.URL.Path, err)
	}
	return resp, nil
}

// gcsFail reads and closes resp, an answer of an unwanted status.
func gcsFail(r *http.Request, resp *http.Response) *gcsError {
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return &gcsError{status: resp.StatusCode,
		msg: fmt.Sprintf("gcs %s %s: %s: %s", r.Method, r.URL.Path, resp.Status, strings.TrimSpace(string(msg)))}
}

// do sends the request build makes (anew for each attempt, so a body starts
// over) and returns the response when its status is one of ok. No answer,
// 408, 429 and 5xx (from GCS or the token endpoint) are retried gcsRetries
// times with backoff (Google's retry guidance; every request here is
// idempotent: objects are named by their content).
func (g *gcs) do(build func() (*http.Request, error), ok ...int) (*http.Response, error) {
	for n := 0; ; n++ {
		r, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := g.try(r)
		if err == nil {
			if slices.Contains(ok, resp.StatusCode) {
				return resp, nil
			}
			e := gcsFail(r, resp)
			if !gcsRetryable(e.status) {
				return nil, e
			}
			err = e
		} else if errors.As(err, new(permanent)) {
			return nil, err
		}
		if n >= gcsRetries {
			return nil, err
		}
		time.Sleep(gcsBackoff(n))
	}
}

func getReq(u string, hdr map[string]string) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r, err
	}
}

// gcsObject is the part of an object resource the backend reads.
type gcsObject struct {
	Name    string      `json:"name"`
	Size    json.Number `json:"size"` // a string in the JSON API
	MD5Hash string      `json:"md5Hash"`
	Updated time.Time   `json:"updated"`
}

func gcsDecode(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v); err != nil {
		return fmt.Errorf("gcs %s %s: decode response: %w", resp.Request.Method, resp.Request.URL.Path, err)
	}
	return nil
}

// upload stores size bytes of r as object name: in one multipart request up
// to gcsChunkSize, resumably in chunks above. The MD5 goes in the metadata,
// which GCS checks the content against, and the stored size and MD5 are
// checked again in the answer. An existing object is replaced, which
// refreshes its updated time for GC's age guard.
func (g *gcs) upload(name string, r io.ReaderAt, size int64, md5sum []byte) error {
	sum := base64.StdEncoding.EncodeToString(md5sum)
	meta, _ := json.Marshal(map[string]string{"name": name, "md5Hash": sum, "contentType": "application/octet-stream"})
	var resp *http.Response
	var err error
	if size <= gcsChunkSize {
		resp, err = g.uploadMultipart(meta, r, size)
	} else {
		resp, err = g.uploadResumable(meta, r, size)
	}
	if err != nil {
		return err
	}
	var o gcsObject
	if err := gcsDecode(resp, &o); err != nil {
		return err
	}
	if n, err := o.Size.Int64(); err != nil || n != size || (o.MD5Hash != "" && o.MD5Hash != sum) {
		return fmt.Errorf("gcs upload %s: stored %s bytes with MD5 %s, sent %d with %s", name, o.Size, o.MD5Hash, size, sum)
	}
	return nil
}

func (g *gcs) uploadMultipart(meta []byte, r io.ReaderAt, size int64) (*http.Response, error) {
	b := multipart.NewWriter(io.Discard).Boundary() // random: it does not occur in the content
	head := "--" + b + "\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n" + string(meta) +
		"\r\n--" + b + "\r\nContent-Type: application/octet-stream\r\n\r\n"
	tail := "\r\n--" + b + "--\r\n"
	return g.do(func() (*http.Request, error) {
		body := io.MultiReader(strings.NewReader(head), io.NewSectionReader(r, 0, size), strings.NewReader(tail))
		req, err := http.NewRequest(http.MethodPost, g.uploadURL("uploadType=multipart"), body)
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(head)+len(tail)) + size
		req.Header.Set("Content-Type", "multipart/related; boundary="+b)
		return req, nil
	}, http.StatusOK, http.StatusCreated)
}

// uploadResumable starts a resumable upload session and sends the content
// in chunks; after a chunk fails it asks the session how far it got and
// goes on from there (an expired session, 404 or 410, fails the upload).
func (g *gcs) uploadResumable(meta []byte, r io.ReaderAt, size int64) (*http.Response, error) {
	resp, err := g.do(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, g.uploadURL("uploadType=resumable"), bytes.NewReader(meta))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Type", "application/octet-stream")
		req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
		return req, nil
	}, http.StatusOK)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	// The session URI is the upload URL with an upload_id; its query is
	// sent to the configured endpoint, so the token never goes elsewhere
	// and a private endpoint keeps the whole upload.
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("upload_id") == "" {
		return nil, fmt.Errorf("gcs resumable upload: no session URI in the answer (Location %q)", resp.Header.Get("Location"))
	}
	session := g.uploadURL(loc.RawQuery)
	total := strconv.FormatInt(size, 10)
	// tries counts the failed attempts since the session last got further;
	// after one, the next request asks the session how far it got.
	off, tries, query := int64(0), 0, false
	for {
		var body io.Reader = http.NoBody
		var n int64
		rng := "bytes */" + total
		if !query {
			n = min(gcsChunkSize, size-off)
			body, rng = io.NewSectionReader(r, off, n), "bytes "+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+n-1, 10)+"/"+total
		}
		req, err := http.NewRequest(http.MethodPut, session, body)
		if err != nil {
			return nil, err
		}
		req.ContentLength = n
		req.Header.Set("Content-Range", rng)
		resp, err := g.try(req)
		if err == nil {
			switch resp.StatusCode {
			case http.StatusOK, http.StatusCreated:
				return resp, nil
			case http.StatusPermanentRedirect: // 308 Resume Incomplete: Range "bytes=0-<last persisted>", none when nothing is
				resp.Body.Close()
				next := int64(0)
				if h := resp.Header.Get("Range"); h != "" {
					last, perr := strconv.ParseInt(strings.TrimPrefix(h, "bytes=0-"), 10, 64)
					if perr != nil || last+1 >= size { // all of it would have been a 200
						return nil, fmt.Errorf("gcs resumable upload: bad Range %q", h)
					}
					next = last + 1
				}
				wasQuery, progress := query, next > off
				off, query = next, false
				if progress {
					tries = 0
				}
				if progress || wasQuery {
					continue
				}
				err = fmt.Errorf("gcs resumable upload: the session kept nothing past byte %d", next)
			default:
				e := gcsFail(req, resp)
				if !gcsRetryable(e.status) {
					return nil, e
				}
				err, query = e, true
			}
		} else if errors.As(err, new(permanent)) {
			return nil, err
		} else {
			query = true
		}
		if tries >= gcsRetries {
			return nil, err
		}
		time.Sleep(gcsBackoff(tries))
		tries++
	}
}

func (g *gcs) put(tmp string, sums Sums) error {
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	md5sum, err := hex.DecodeString(sums.MD5)
	if err != nil {
		return err
	}
	return g.upload(g.key(sums.SHA256), f, sums.Size, md5sum)
}

// get starts a GET of object name from offset from, to offset to inclusive
// (-1: the end), and returns the body and the object's total size.
func (g *gcs) get(name string, from, to int64) (io.ReadCloser, int64, error) {
	var hdr map[string]string
	if from > 0 || to >= 0 {
		rng := "bytes=" + strconv.FormatInt(from, 10) + "-"
		if to >= 0 {
			rng += strconv.FormatInt(to, 10)
		}
		hdr = map[string]string{"Range": rng}
	}
	resp, err := g.do(getReq(g.objectURL(name, url.Values{"alt": {"media"}}), hdr), http.StatusOK, http.StatusPartialContent)
	if err != nil {
		return nil, 0, err
	}
	size := resp.ContentLength
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		_, total, ok := strings.Cut(resp.Header.Get("Content-Range"), "/")
		if size, err = strconv.ParseInt(total, 10, 64); !ok || err != nil {
			resp.Body.Close()
			return nil, 0, fmt.Errorf("gcs GET %s: bad Content-Range %q", name, resp.Header.Get("Content-Range"))
		}
	case from > 0:
		resp.Body.Close()
		return nil, 0, fmt.Errorf("gcs GET %s: the server ignored the Range header", name)
	case size < 0: // chunked: the stored length says where a clean end is
		if size, err = strconv.ParseInt(resp.Header.Get("X-Goog-Stored-Content-Length"), 10, 64); err != nil {
			resp.Body.Close()
			return nil, 0, fmt.Errorf("gcs GET %s: no Content-Length", name)
		}
	}
	return resp.Body, size, nil
}

func (g *gcs) open(sha string) (File, error) {
	name := g.key(sha)
	body, size, err := g.get(name, 0, -1)
	if err != nil {
		return nil, err
	}
	return &s3File{s: g, key: name, size: size, body: body}, nil
}

func (g *gcs) stat(sha string) (Info, error) {
	resp, err := g.do(getReq(g.objectURL(g.key(sha), url.Values{"fields": {"size,updated"}}), nil), http.StatusOK)
	if err != nil {
		return Info{}, err
	}
	var o gcsObject
	if err := gcsDecode(resp, &o); err != nil {
		return Info{}, err
	}
	size, err := o.Size.Int64()
	if err != nil {
		return Info{}, fmt.Errorf("gcs stat %s: size %q", sha, o.Size)
	}
	return Info{Size: size, ModTime: o.Updated}, nil
}

func (g *gcs) delete(name string) error {
	resp, err := g.do(func() (*http.Request, error) { return http.NewRequest(http.MethodDelete, g.objectURL(name, nil), nil) },
		http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (g *gcs) remove(sha string) error { return g.delete(g.key(sha)) }

type gcsListing struct {
	Items         []gcsObject `json:"items"`
	NextPageToken string      `json:"nextPageToken"`
}

// list pages through the objects whose names start with prefix, in name
// order, max per page (1: the first page only); with delimiter "/" it
// leaves out the ones in "subdirectories" (the meta objects, for walk).
func (g *gcs) list(prefix, delimiter string, max int, fn func(gcsObject) error) error {
	q := url.Values{"maxResults": {strconv.Itoa(max)}, "fields": {"items(name,size,updated),nextPageToken"}}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if delimiter != "" {
		q.Set("delimiter", delimiter)
	}
	for {
		resp, err := g.do(getReq(g.bucketURL()+"/o?"+q.Encode(), nil), http.StatusOK)
		if err != nil {
			return err
		}
		var page gcsListing
		if err := gcsDecode(resp, &page); err != nil {
			return err
		}
		for _, o := range page.Items {
			if err := fn(o); err != nil {
				return err
			}
		}
		if page.NextPageToken == "" || max == 1 {
			return nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

func (g *gcs) walk(fn func(sha string, size int64, modTime time.Time) error) error {
	prefix := g.key("")
	return g.list(prefix, "/", gcsPageSize, func(o gcsObject) error {
		sha := strings.TrimPrefix(o.Name, prefix)
		size, err := o.Size.Int64()
		if !validSHA(sha) || err != nil {
			return nil
		}
		return fn(sha, size, o.Updated)
	})
}

// check gets the bucket; credentials that may not (storage.buckets.get is
// not in roles/storage.objectAdmin) pass when they may list its objects.
func (g *gcs) check() error {
	resp, err := g.do(getReq(g.bucketURL()+"?fields=name", nil), http.StatusOK)
	if err == nil {
		return resp.Body.Close()
	}
	var e *gcsError
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("bucket %q not found", g.cfg.Bucket)
	case errors.As(err, &e) && e.status == http.StatusForbidden:
		if lerr := g.list(g.key(""), "/", 1, func(gcsObject) error { return nil }); lerr != nil {
			return fmt.Errorf("the credentials may neither get bucket %q nor list its objects: %w", g.cfg.Bucket, lerr)
		}
		return nil
	}
	return err
}

// Meta objects live at <prefix>/meta/<key>; walk lists with a delimiter, so
// it never sees them.

var gcsMetaKeyRE = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)

func (g *gcs) metaName(key string, prefix bool) (string, error) {
	if (!prefix && key == "") || !gcsMetaKeyRE.MatchString(key) || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return "", fmt.Errorf("invalid meta key %q", key)
	}
	return g.key("meta/" + key), nil
}

func (g *gcs) putMeta(key string, data []byte) error {
	name, err := g.metaName(key, false)
	if err != nil {
		return err
	}
	sum := md5.Sum(data)
	return g.upload(name, bytes.NewReader(data), int64(len(data)), sum[:])
}

func (g *gcs) getMeta(key string) ([]byte, error) {
	name, err := g.metaName(key, false)
	if err != nil {
		return nil, err
	}
	body, _, err := g.get(name, 0, -1)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

func (g *gcs) listMeta(prefix string, fn func(key string) error) error {
	name, err := g.metaName(prefix, true)
	if err != nil {
		return err
	}
	root := g.key("meta/")
	return g.list(name, "", gcsPageSize, func(o gcsObject) error { return fn(strings.TrimPrefix(o.Name, root)) })
}

func (g *gcs) removeMeta(key string) error {
	name, err := g.metaName(key, false)
	if err != nil {
		return err
	}
	return g.delete(name)
}
