package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Config describes an S3-compatible bucket (AWS S3, MinIO, Ceph RGW) that
// holds a store's blobs at <Prefix>/<sha256>. The fields are the ones the
// blob-store API takes; SecretKey is sealed in the database like the other
// stored secrets.
type S3Config struct {
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`    // key prefix, no leading or trailing slash; empty: bucket root
	Region    string `json:"region,omitempty"`    // SigV4 region; "us-east-1" when empty
	Endpoint  string `json:"endpoint,omitempty"`  // https://s3.<region>.amazonaws.com when empty; MinIO/Ceph: their URL
	PathStyle bool   `json:"pathStyle,omitempty"` // <endpoint>/<bucket>/<key> instead of <bucket>.<endpoint host>/<key> (MinIO)
	AccessKey string `json:"accessKey,omitempty"` // with SecretKey; both empty: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN
	SecretKey string `json:"secretKey,omitempty"`
	SSE       string `json:"sse,omitempty"` // x-amz-server-side-encryption on every upload: "AES256" or "aws:kms"; empty: none
}

var (
	s3BucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3PrefixRE = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
)

// Validate reports the first thing wrong with c, after trimming the slashes
// of its prefix. It does not touch the bucket.
func (c *S3Config) Validate() error {
	c.Prefix = strings.Trim(c.Prefix, "/")
	switch {
	case !s3BucketRE.MatchString(c.Bucket):
		return errors.New("bucket must be 3 to 63 lower-case letters, digits, dots or hyphens")
	case c.Prefix != "" && !s3PrefixRE.MatchString(c.Prefix):
		return errors.New("prefix may hold letters, digits, slashes, dots, underscores and hyphens")
	case c.SSE != "" && c.SSE != "AES256" && c.SSE != "aws:kms":
		return errors.New(`sse must be "AES256", "aws:kms" or empty`)
	case (c.AccessKey == "") != (c.SecretKey == ""):
		return errors.New("accessKey and secretKey go together (leave both empty for the AWS_* environment)")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.User != nil {
			return errors.New("endpoint must be an http(s) URL with a host and nothing after the path")
		}
	}
	return nil
}

// s3PartSize is the largest object sent as one PUT; a larger one goes up in
// multipart parts of this size (a test lowers it).
var s3PartSize int64 = 64 << 20

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// s3 is the backend for one bucket and prefix.
type s3 struct {
	cfg                           S3Config
	accessKey, secretKey, session string
	base                          *url.URL // scheme://host, with /<bucket> for path style
	client                        *http.Client
	now                           func() time.Time
}

// OpenS3 opens a store whose blobs live in the bucket cfg describes;
// root/tmp holds its uploads in progress and Docker sessions, as for Open.
func OpenS3(root string, cfg S3Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	be := &s3{cfg: cfg, accessKey: cfg.AccessKey, secretKey: cfg.SecretKey, now: time.Now}
	if be.accessKey == "" {
		be.accessKey, be.secretKey, be.session = os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")
		if be.accessKey == "" || be.secretKey == "" {
			return nil, errors.New("no S3 credentials: set accessKey and secretKey, or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
		}
	}
	u, _ := url.Parse(cfg.Endpoint) // validated above
	u.Path, u.RawPath = strings.TrimSuffix(u.Path, "/"), ""
	if cfg.PathStyle {
		u.Path += "/" + cfg.Bucket
	} else {
		u.Host = cfg.Bucket + "." + u.Host
	}
	be.base = u
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	be.client = &http.Client{Transport: tr}
	return open(root, be)
}

func (s *s3) key(sha string) string {
	if s.cfg.Prefix == "" {
		return sha
	}
	return s.cfg.Prefix + "/" + sha
}

// request builds a signed request for key ("" is the bucket) with query q,
// headers hdr and a body of the given size whose SHA-256 is payloadHash.
func (s *s3) request(method, key string, q url.Values, hdr map[string]string, body io.Reader, size int64, payloadHash string) (*http.Request, error) {
	u := *s.base
	u.Path += "/" + key
	u.RawQuery = canonicalQuery(q)
	if size == 0 {
		body = http.NoBody // a nil-length body would go chunked, which S3 refuses
	}
	r, err := http.NewRequestWithContext(context.Background(), method, u.String(), body)
	if err != nil {
		return nil, err
	}
	r.ContentLength = size
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	SignV4(r, s.accessKey, s.secretKey, s.session, s.cfg.Region, payloadHash, s.now())
	return r, nil
}

// sse is the server-side encryption header for uploads, if configured.
func (s *s3) sse() map[string]string {
	if s.cfg.SSE == "" {
		return nil
	}
	return map[string]string{"x-amz-server-side-encryption": s.cfg.SSE}
}

// do sends r and returns the response when its status is one of ok, else
// an error naming the status and S3's message. A 404 is ErrNotFound; a full
// store (507, MinIO) is ErrDiskFull.
func (s *s3) do(r *http.Request, ok ...int) (*http.Response, error) {
	resp, err := s.client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s: %w", r.Method, r.URL.Path, err)
	}
	for _, c := range ok {
		if resp.StatusCode == c {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	err = fmt.Errorf("s3 %s %s: %s: %s", r.Method, r.URL.Path, resp.Status, strings.TrimSpace(string(msg)))
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	case http.StatusInsufficientStorage:
		return nil, fmt.Errorf("%w: %v", ErrDiskFull, err)
	}
	return nil, err
}

// put uploads the spooled file: one PUT up to s3PartSize, multipart above.
// An existing copy is overwritten, which refreshes its LastModified for GC's
// age guard the way the directory backend touches a file.
func (s *s3) put(tmp string, sums Sums) error {
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	key := s.key(sums.SHA256)
	if sums.Size <= s3PartSize {
		r, err := s.request(http.MethodPut, key, nil, s.sse(), f, sums.Size, sums.SHA256)
		if err != nil {
			return err
		}
		resp, err := s.do(r, http.StatusOK)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	return s.putMultipart(f, key, sums.Size)
}

func (s *s3) putMultipart(f *os.File, key string, size int64) (err error) {
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	r, err := s.request(http.MethodPost, key, url.Values{"uploads": {""}}, s.sse(), nil, 0, emptySHA256)
	if err != nil {
		return err
	}
	if err := s.doXML(r, &init); err != nil {
		return err
	}
	id := init.UploadID
	defer func() {
		if err != nil { // best effort: an abandoned upload is billed until aborted
			if r, e := s.request(http.MethodDelete, key, url.Values{"uploadId": {id}}, nil, nil, 0, emptySHA256); e == nil {
				if resp, e := s.do(r, http.StatusNoContent); e == nil {
					resp.Body.Close()
				}
			}
		}
	}()
	type part struct {
		XMLName xml.Name `xml:"Part"`
		Number  int      `xml:"PartNumber"`
		ETag    string   `xml:"ETag"`
	}
	var parts []part
	for off := int64(0); off < size; off += s3PartSize {
		n := min(s3PartSize, size-off)
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, off, n)); err != nil {
			return err
		}
		q := url.Values{"partNumber": {strconv.Itoa(len(parts) + 1)}, "uploadId": {id}}
		r, err := s.request(http.MethodPut, key, q, nil, io.NewSectionReader(f, off, n), n, hex.EncodeToString(h.Sum(nil)))
		if err != nil {
			return err
		}
		resp, err := s.do(r, http.StatusOK)
		if err != nil {
			return err
		}
		resp.Body.Close()
		parts = append(parts, part{Number: len(parts) + 1, ETag: resp.Header.Get("ETag")})
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part
	}{Parts: parts})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	r, err = s.request(http.MethodPost, key, url.Values{"uploadId": {id}}, nil, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		return err
	}
	// S3 may answer 200 and still fail, with an <Error> document.
	var done struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if err := s.doXML(r, &done); err != nil {
		return err
	}
	if done.XMLName.Local == "Error" {
		return fmt.Errorf("s3 complete multipart upload %s: %s: %s", key, done.Code, done.Message)
	}
	return nil
}

// doXML sends r, expects 200 and decodes the XML body into v.
func (s *s3) doXML(r *http.Request, v any) error {
	resp, err := s.do(r, http.StatusOK)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v); err != nil {
		return fmt.Errorf("s3 %s %s: decode response: %w", r.Method, r.URL.Path, err)
	}
	return nil
}

// get starts a GET of key from offset from, to offset to inclusive (-1: the
// end), and returns the body and the object's total size.
func (s *s3) get(key string, from, to int64) (io.ReadCloser, int64, error) {
	var hdr map[string]string
	if from > 0 || to >= 0 {
		rng := "bytes=" + strconv.FormatInt(from, 10) + "-"
		if to >= 0 {
			rng += strconv.FormatInt(to, 10)
		}
		hdr = map[string]string{"Range": rng}
	}
	r, err := s.request(http.MethodGet, key, nil, hdr, nil, 0, emptySHA256)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.do(r, http.StatusOK, http.StatusPartialContent)
	if err != nil {
		return nil, 0, err
	}
	size := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		// Content-Range: bytes from-to/size
		_, total, ok := strings.Cut(resp.Header.Get("Content-Range"), "/")
		if size, err = strconv.ParseInt(total, 10, 64); !ok || err != nil {
			resp.Body.Close()
			return nil, 0, fmt.Errorf("s3 GET %s: bad Content-Range %q", key, resp.Header.Get("Content-Range"))
		}
	} else if from > 0 {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("s3 GET %s: the server ignored the Range header", key)
	}
	return resp.Body, size, nil
}

func (s *s3) open(sha string) (File, error) {
	body, size, err := s.get(s.key(sha), 0, -1)
	if err != nil {
		return nil, err
	}
	return &s3File{s: s, key: s.key(sha), size: size, body: body}, nil
}

func (s *s3) stat(sha string) (Info, error) {
	r, err := s.request(http.MethodHead, s.key(sha), nil, nil, nil, 0, emptySHA256)
	if err != nil {
		return Info{}, err
	}
	resp, err := s.do(r, http.StatusOK)
	if err != nil {
		return Info{}, err
	}
	resp.Body.Close()
	mod, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return Info{Size: resp.ContentLength, ModTime: mod}, nil
}

func (s *s3) remove(sha string) error {
	r, err := s.request(http.MethodDelete, s.key(sha), nil, nil, nil, 0, emptySHA256)
	if err != nil {
		return err
	}
	resp, err := s.do(r, http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// list is one ListObjectsV2 page under the prefix.
func (s *s3) list(maxKeys int, token string) (*s3Listing, error) {
	q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(maxKeys)}}
	if s.cfg.Prefix != "" {
		q.Set("prefix", s.cfg.Prefix+"/")
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	r, err := s.request(http.MethodGet, "", q, nil, nil, 0, emptySHA256)
	if err != nil {
		return nil, err
	}
	var page s3Listing
	if err := s.doXML(r, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

type s3Listing struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *s3) walk(fn func(sha string, modTime time.Time) error) error {
	token := ""
	for {
		page, err := s.list(1000, token)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			sha := strings.TrimPrefix(o.Key, s.key(""))
			if !validSHA(sha) {
				continue
			}
			if err := fn(sha, o.LastModified); err != nil {
				return err
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}

func (s *s3) check() error {
	_, err := s.list(1, "")
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("bucket %q not found, or the credentials may not list it", s.cfg.Bucket)
	}
	return err
}

func (dir) check() error { return nil }

// Check confirms the backend is reachable with the store's credentials (an
// S3 bucket that lists); a directory store always passes.
func (s *Store) Check() error { return s.be.check() }

// s3File reads an object with one GET stream that follows sequential reads;
// a Seek elsewhere closes it and the next Read opens a Range GET from there,
// and ReadAt is a Range GET of its own.
type s3File struct {
	s         *s3
	key       string
	size, off int64
	body      io.ReadCloser // positioned at off, or nil
}

func (f *s3File) Read(p []byte) (int, error) {
	if f.off >= f.size {
		return 0, io.EOF
	}
	if f.body == nil {
		body, _, err := f.s.get(f.key, f.off, -1)
		if err != nil {
			return 0, err
		}
		f.body = body
	}
	n, err := f.body.Read(p)
	f.off += int64(n)
	if err == io.EOF && f.off < f.size { // the stream ended short: not a clean end
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (f *s3File) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.off + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	if abs != f.off {
		f.dropBody()
		f.off = abs
	}
	return abs, nil
}

func (f *s3File) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.size || len(p) == 0 {
		return 0, io.EOF
	}
	end := min(off+int64(len(p)), f.size)
	body, _, err := f.s.get(f.key, off, end-1)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	n, err := io.ReadFull(body, p[:end-off])
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (f *s3File) dropBody() {
	if f.body != nil {
		f.body.Close()
		f.body = nil
	}
}

func (f *s3File) Close() error { f.dropBody(); return nil }

// SignV4 signs r in place for S3 with AWS Signature Version 4 (the
// Authorization header, with x-amz-date, x-amz-content-sha256 and, with a
// session token, x-amz-security-token). It signs the host, every x-amz-*
// header and Range when present. payloadHash is the hex SHA-256 of the body.
func SignV4(r *http.Request, accessKey, secretKey, sessionToken, region, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	r.Header.Set("x-amz-date", amzDate)
	r.Header.Set("x-amz-content-sha256", payloadHash)
	if sessionToken != "" {
		r.Header.Set("x-amz-security-token", sessionToken)
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	headers := map[string]string{"host": host}
	for k, v := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") || lk == "range" {
			headers[lk] = strings.TrimSpace(v[0])
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{r.Method, uriEncode(r.URL.Path, true), canonicalQuery(r.URL.Query()),
		canon.String(), signed, payloadHash}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256([]byte(canonical))
	k := []byte("AWS4" + secretKey)
	for _, p := range []string{date, region, "s3", "aws4_request"} {
		k = hmacSHA256(k, p)
	}
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(k, toSign)))
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalQuery is SigV4's query string: keys and values URI-encoded and
// sorted. It is also what the requests carry, so both sides agree.
func canonicalQuery(q url.Values) string {
	var parts []string
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's URI encoding: unreserved characters stay, every other
// byte is %XX in upper case, and with keepSlash a path's slashes stay too.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~',
			keepSlash && c == '/':
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}
