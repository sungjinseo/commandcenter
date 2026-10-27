// Package s3test is an in-memory S3 bucket behind httptest for tests of the
// S3 blob store: path-style requests, SigV4 checked on every one, PUT, GET
// with Range, HEAD, DELETE, ListObjectsV2 with pagination and multipart
// uploads.
package s3test

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/blob"
)

// Server is one fake bucket. Its exported fields may be set before use.
type Server struct {
	*httptest.Server
	AccessKey, SecretKey, Bucket string
	MaxKeys                      int  // the largest list page it serves (1000)
	Full                         bool // answer every upload with 507, as MinIO does when its disks are full

	mu      sync.Mutex
	objects map[string]*object
	uploads map[string]*upload
	log     []string
}

type object struct {
	data []byte
	mod  time.Time
	sse  string
}

type upload struct {
	key   string
	sse   string
	parts map[int][]byte
}

// New starts a fake bucket "test" that t stops.
func New(t testing.TB) *Server {
	s := &Server{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Bucket: "test",
		MaxKeys: 1000, objects: map[string]*object{}, uploads: map[string]*upload{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Config is the blob-store configuration that reaches s.
func (s *Server) Config() blob.S3Config {
	return blob.S3Config{Bucket: s.Bucket, Region: "us-east-1", Endpoint: s.URL, PathStyle: true,
		AccessKey: s.AccessKey, SecretKey: s.SecretKey}
}

// Put stores an object as if uploaded at mod.
func (s *Server) Put(key string, data []byte, mod time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = &object{data: data, mod: mod}
}

// Object returns a stored object's bytes.
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), o.data...), true
}

// SSE returns the x-amz-server-side-encryption an object was stored with.
func (s *Server) SSE(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.objects[key]; o != nil {
		return o.sse
	}
	return ""
}

// Keys lists the stored keys, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedKeys()
}

func (s *Server) sortedKeys() []string {
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Log returns every request served, as "METHOD /path?query" plus " Range: ..."
// when the request had one.
func (s *Server) Log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	entry := r.Method + " " + r.URL.RequestURI()
	if rng := r.Header.Get("Range"); rng != "" {
		entry += " Range: " + rng
	}
	s.mu.Lock()
	s.log = append(s.log, entry)
	s.mu.Unlock()
	if err := s.verify(r, body); err != nil {
		s.fail(w, http.StatusForbidden, "SignatureDoesNotMatch", err.Error())
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != s.Bucket {
		s.fail(w, http.StatusNotFound, "NoSuchBucket", "no bucket "+bucket)
		return
	}
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case key == "" && r.Method == http.MethodGet:
		s.list(w, q)
	case r.Method == http.MethodPost && q.Has("uploads"):
		if s.Full {
			s.fail(w, http.StatusInsufficientStorage, "XMinioStorageFull", "disk full")
			return
		}
		id := hex.EncodeToString([]byte(fmt.Sprintf("up%d", len(s.uploads)+1)))
		s.uploads[id] = &upload{key: key, sse: r.Header.Get("x-amz-server-side-encryption"), parts: map[int][]byte{}}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, bucket, key, id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		up := s.uploads[q.Get("uploadId")]
		n, err := strconv.Atoi(q.Get("partNumber"))
		if up == nil || up.key != key || err != nil || n < 1 {
			s.fail(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		up.parts[n] = body
		w.Header().Set("ETag", etag(body))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		up := s.uploads[q.Get("uploadId")]
		if up == nil || up.key != key {
			s.fail(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
			s.fail(w, http.StatusBadRequest, "MalformedXML", "bad complete request")
			return
		}
		var data []byte
		for i, p := range req.Parts {
			part, ok := up.parts[p.PartNumber]
			if !ok || p.PartNumber != i+1 || p.ETag != etag(part) {
				s.fail(w, http.StatusBadRequest, "InvalidPart", fmt.Sprintf("part %d", p.PartNumber))
				return
			}
			data = append(data, part...)
		}
		delete(s.uploads, q.Get("uploadId"))
		s.objects[key] = &object{data: data, mod: time.Now(), sse: up.sse}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`, key, etag(data))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(s.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		if s.Full {
			s.fail(w, http.StatusInsufficientStorage, "XMinioStorageFull", "disk full")
			return
		}
		if r.ContentLength != int64(len(body)) {
			s.fail(w, http.StatusBadRequest, "MissingContentLength", "Content-Length required")
			return
		}
		s.objects[key] = &object{data: body, mod: time.Now(), sse: r.Header.Get("x-amz-server-side-encryption")}
		w.Header().Set("ETag", etag(body))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		o := s.objects[key]
		if o == nil {
			s.fail(w, http.StatusNotFound, "NoSuchKey", "no such key")
			return
		}
		s.get(w, r, o)
	default:
		s.fail(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, o *object) {
	w.Header().Set("Last-Modified", o.mod.UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", etag(o.data))
	w.Header().Set("Accept-Ranges", "bytes")
	from, to := int64(0), int64(len(o.data))-1
	if rng, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok {
		a, b, _ := strings.Cut(rng, "-")
		from, _ = strconv.ParseInt(a, 10, 64)
		if b != "" {
			to, _ = strconv.ParseInt(b, 10, 64)
		}
		if from >= int64(len(o.data)) || to < from {
			w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(len(o.data)))
			s.fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", rng)
			return
		}
		to = min(to, int64(len(o.data))-1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(o.data)))
		w.Header().Set("Content-Length", strconv.FormatInt(to-from+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodGet {
		w.Write(o.data[from : to+1])
	}
}

func (s *Server) list(w http.ResponseWriter, q url.Values) {
	prefix, token := q.Get("prefix"), q.Get("continuation-token")
	maxKeys := s.MaxKeys
	if n, err := strconv.Atoi(q.Get("max-keys")); err == nil && n < maxKeys {
		maxKeys = n
	}
	var keys []string
	for _, k := range s.sortedKeys() {
		if strings.HasPrefix(k, prefix) && k > token {
			keys = append(keys, k)
		}
	}
	truncated := len(keys) > maxKeys
	if truncated {
		keys = keys[:maxKeys]
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><MaxKeys>%d</MaxKeys><IsTruncated>%t</IsTruncated>`, s.Bucket, prefix, maxKeys, truncated)
	if truncated {
		fmt.Fprintf(&b, `<NextContinuationToken>%s</NextContinuationToken>`, keys[len(keys)-1])
	}
	for _, k := range keys {
		o := s.objects[k]
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>%s</ETag><Size>%d</Size></Contents>`,
			k, o.mod.UTC().Format("2006-01-02T15:04:05.000Z"), etag(o.data), len(o.data))
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, b.String())
}

// verify checks the request's SigV4 signature against s's credentials, by
// signing it again, and that x-amz-content-sha256 is the body's hash.
func (s *Server) verify(r *http.Request, body []byte) error {
	auth := r.Header.Get("Authorization")
	cred := "Credential=" + s.AccessKey + "/"
	if !strings.Contains(auth, cred) {
		return fmt.Errorf("authorization %q is not for %s", auth, s.AccessKey)
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("x-amz-date"))
	if err != nil {
		return fmt.Errorf("bad x-amz-date %q", r.Header.Get("x-amz-date"))
	}
	if d := time.Since(at); d > 15*time.Minute || d < -15*time.Minute {
		return fmt.Errorf("x-amz-date %v is too far from now", at)
	}
	sum := sha256.Sum256(body)
	if got := r.Header.Get("x-amz-content-sha256"); got != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("x-amz-content-sha256 %q is not the body's hash", got)
	}
	// SignV4 signs the host, x-amz-* and Range: hand it the same headers.
	clone := r.Clone(r.Context())
	clone.Header = http.Header{}
	for k, v := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") || lk == "range" {
			clone.Header[k] = v
		}
	}
	_, scope, _ := strings.Cut(auth, cred)
	region := strings.Split(scope, "/")[1]
	blob.SignV4(clone, s.AccessKey, s.SecretKey, "", region, r.Header.Get("x-amz-content-sha256"), at)
	if want := clone.Header.Get("Authorization"); want != auth {
		return fmt.Errorf("signature mismatch:\n got %s\nwant %s", auth, want)
	}
	return nil
}

func (s *Server) fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

func etag(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
