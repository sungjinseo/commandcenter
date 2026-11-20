package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
	"commandcenter/internal/server"
	"commandcenter/internal/sigv4"
)

type env struct {
	t     *testing.T
	ts    *httptest.Server
	h     *Handler
	db    *meta.DB
	bs    *blob.Store
	keys  map[string][2]string // user -> access key id, secret
	clock time.Time
}

// newEnv serves buckets "bkt" (ci may add, edit and delete) and "pub"
// (anyone may read, ci may add but not replace) through the real server, on
// the S3 host s3.test with virtual hosts. ci, reader and admin have keys.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	bs, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(metatest.URL(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := &config.Config{
		Users: []config.User{
			{Name: "ci", PasswordHash: string(hash), Roles: []string{"deployer"}},
			{Name: "reader", PasswordHash: string(hash)},
			{Name: "admin", PasswordHash: string(hash), Roles: []string{"admin"}},
		},
		Roles: map[string]config.Role{"deployer": {Write: []string{"bkt", "pub"}}},
		Repositories: []config.Repository{
			{Name: "bkt", Format: "s3", Type: "hosted", WritePolicy: "allow"},
			{Name: "pub", Format: "s3", Type: "hosted", WritePolicy: "allow_once", AnonymousRead: ptr(true)},
			{Name: "rawrepo", Format: "raw", Type: "hosted"},
		},
	}
	for i := range cfg.Repositories {
		config.ApplyRepositoryDefaults(&cfg.Repositories[i])
	}
	if err := config.ValidateRepositories(cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.ImportRepositories(ctx, cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: db, Now: time.Now}
	authn := auth.New(cfg, db)
	registry := repo.NewRegistry(db, cfg.Repositories)
	authn.AnonymousRead = registry.AnonymousRead
	e := &env{t: t, db: db, bs: bs, keys: map[string][2]string{}, clock: time.Now()}
	h := New(store, registry, authn, config.S3{Host: "s3.test", VirtualHost: true, Region: "us-east-1"})
	h.Now = func() time.Time { return e.clock }
	srv := &server.Server{Registry: registry, Handlers: map[string]repo.Handler{"s3": h}, Auth: authn, S3: h,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	e.h = h
	for _, u := range []string{"ci", "reader", "admin"} {
		k, secret, err := authn.CreateS3Key(ctx, u, "test", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		e.keys[u] = [2]string{k.AccessKeyID, secret}
	}
	return e
}

func ptr[T any](v T) *T { return &v }

type opt func(*http.Request)

func hdr(k, v string) opt { return func(r *http.Request) { r.Header.Set(k, v) } }
func vhost(b string) opt  { return func(r *http.Request) { r.Host = b + ".s3.test" } }

type resp struct {
	status int
	h      http.Header
	body   string
}

// s3 sends a request signed as user ("" for anonymous) to path on the S3
// host, with a signed payload hash.
func (e *env) s3(user, method, path, body string, opts ...opt) resp {
	e.t.Helper()
	r, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	r.Host = "s3.test"
	for _, o := range opts {
		o(r)
	}
	if user != "" {
		sum := sha256.Sum256([]byte(body))
		k := e.keys[user]
		sigv4.Sign(r, k[0], k[1], "us-east-1", e.clock, hex.EncodeToString(sum[:]))
	}
	return e.send(r)
}

func (e *env) send(r *http.Request) resp {
	e.t.Helper()
	res, err := e.ts.Client().Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return resp{res.StatusCode, res.Header, string(b)}
}

func (e *env) want(r resp, status int, code string) {
	e.t.Helper()
	if r.status != status || code != "" && !strings.Contains(r.body, "<Code>"+code+"</Code>") {
		e.t.Fatalf("got %d %s, want %d %s", r.status, r.body, status, code)
	}
}

func md5hex(s string) string {
	m := md5.Sum([]byte(s))
	return hex.EncodeToString(m[:])
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	e.want(e.s3("", "PUT", "/bkt/a.txt", "x"), 403, "AccessDenied")
	r, _ := http.NewRequest("GET", e.ts.URL+"/bkt/a.txt", nil)
	r.Host = "s3.test"
	sigv4.Sign(r, e.keys["ci"][0], "wrong", "us-east-1", e.clock, sigv4.UnsignedPayload)
	e.want(e.send(r), 403, "SignatureDoesNotMatch")
	r, _ = http.NewRequest("GET", e.ts.URL+"/bkt/a.txt", nil)
	r.Host = "s3.test"
	sigv4.Sign(r, "CCKUNKNOWNUNKNOWNUNK", "x", "us-east-1", e.clock, sigv4.UnsignedPayload)
	e.want(e.send(r), 403, "InvalidAccessKeyId")
	r, _ = http.NewRequest("GET", e.ts.URL+"/bkt/a.txt", nil)
	r.Host = "s3.test"
	sigv4.Sign(r, e.keys["ci"][0], e.keys["ci"][1], "us-east-1", e.clock.Add(-time.Hour), sigv4.UnsignedPayload)
	e.want(e.send(r), 403, "RequestTimeTooSkewed")
	// A body that is not the signed one.
	r, _ = http.NewRequest("PUT", e.ts.URL+"/bkt/a.txt", strings.NewReader("tampered"))
	r.Host = "s3.test"
	sum := sha256.Sum256([]byte("original"))
	sigv4.Sign(r, e.keys["ci"][0], e.keys["ci"][1], "us-east-1", e.clock, hex.EncodeToString(sum[:]))
	e.want(e.send(r), 400, "BadDigest")
	e.want(e.s3("reader", "PUT", "/bkt/a.txt", "x"), 403, "AccessDenied")
	// Another host is not the S3 API.
	r, _ = http.NewRequest("GET", e.ts.URL+"/bkt/a.txt", nil)
	if res := e.send(r); strings.Contains(res.body, "<Error>") {
		t.Fatalf("the main host answered S3: %s", res.body)
	}
}

func TestObjects(t *testing.T) {
	e := newEnv(t)
	put := e.s3("ci", "PUT", "/bkt/dir/a.txt", "hello", hdr("Content-Type", "text/plain"), hdr("X-Amz-Meta-Color", "blue"),
		hdr("Cache-Control", "max-age=60"), hdr("Content-Md5", base64.StdEncoding.EncodeToString(func() []byte { m := md5.Sum([]byte("hello")); return m[:] }())))
	e.want(put, 200, "")
	if put.h.Get("ETag") != `"`+md5hex("hello")+`"` {
		t.Fatalf("etag %q", put.h.Get("ETag"))
	}
	get := e.s3("ci", "GET", "/bkt/dir/a.txt", "")
	if get.status != 200 || get.body != "hello" || get.h.Get("Content-Type") != "text/plain" || get.h.Get("X-Amz-Meta-Color") != "blue" ||
		get.h.Get("Cache-Control") != "max-age=60" || get.h.Get("ETag") != put.h.Get("ETag") {
		t.Fatalf("get: %d %q %v", get.status, get.body, get.h)
	}
	if r := e.s3("ci", "GET", "/bkt/dir/a.txt", "", hdr("Range", "bytes=1-3")); r.status != 206 || r.body != "ell" {
		t.Fatalf("range: %d %q", r.status, r.body)
	}
	if r := e.s3("ci", "HEAD", "/bkt/dir/a.txt", ""); r.status != 200 || r.h.Get("Content-Length") != "5" {
		t.Fatalf("head: %d %v", r.status, r.h)
	}
	if r := e.s3("ci", "GET", "/bkt/dir/a.txt", "", hdr("If-None-Match", put.h.Get("ETag"))); r.status != 304 {
		t.Fatalf("if-none-match: %d", r.status)
	}
	e.want(e.s3("ci", "GET", "/bkt/missing", ""), 404, "NoSuchKey")
	e.want(e.s3("ci", "HEAD", "/bkt/missing", ""), 404, "")
	e.want(e.s3("ci", "GET", "/nobucket/x", ""), 404, "NoSuchBucket")
	e.want(e.s3("ci", "GET", "/rawrepo/x", ""), 404, "NoSuchBucket")
	e.want(e.s3("ci", "PUT", "/bkt/a/../b", "x"), 400, "KeyTooLongError")
	// Conditional writes: If-None-Match * refuses an existing key (Terraform's lock file).
	e.want(e.s3("ci", "PUT", "/bkt/dir/a.txt", "again", hdr("If-None-Match", "*")), 412, "PreconditionFailed")
	e.want(e.s3("ci", "PUT", "/bkt/dir/new.txt", "x", hdr("If-None-Match", "*")), 200, "")
	e.want(e.s3("ci", "PUT", "/bkt/dir/a.txt", "v2", hdr("If-Match", `"nope"`)), 412, "PreconditionFailed")
	e.want(e.s3("ci", "PUT", "/bkt/dir/a.txt", "v2", hdr("If-Match", put.h.Get("ETag"))), 200, "")
	// allow_once: no replacing in pub; anonymous read there.
	e.want(e.s3("ci", "PUT", "/pub/f", "1"), 200, "")
	e.want(e.s3("ci", "PUT", "/pub/f", "2"), 403, "AccessDenied")
	if r := e.s3("", "GET", "/pub/f", ""); r.status != 200 || r.body != "1" {
		t.Fatalf("anonymous read: %d %s", r.status, r.body)
	}
	e.want(e.s3("", "GET", "/bkt/dir/a.txt", ""), 403, "AccessDenied")
	// The read-only /repository/ view.
	if r := e.send(mustReq("GET", e.ts.URL+"/repository/pub/f")); r.status != 200 || r.body != "1" {
		t.Fatalf("/repository/: %d %s", r.status, r.body)
	}
	if r := e.send(mustReq("PUT", e.ts.URL+"/repository/pub/g")); r.status != 401 && r.status != 405 {
		t.Fatalf("/repository/ PUT: %d", r.status)
	}
	// Delete, also of a missing key.
	e.want(e.s3("reader", "DELETE", "/bkt/dir/a.txt", ""), 403, "AccessDenied")
	e.want(e.s3("ci", "DELETE", "/bkt/dir/a.txt", ""), 204, "")
	e.want(e.s3("ci", "DELETE", "/bkt/dir/a.txt", ""), 204, "")
	e.want(e.s3("ci", "GET", "/bkt/dir/a.txt", ""), 404, "NoSuchKey")
	// Virtual-hosted style.
	e.want(e.s3("ci", "PUT", "/v.txt", "vh", vhost("bkt")), 200, "")
	if r := e.s3("ci", "GET", "/bkt/v.txt", ""); r.body != "vh" {
		t.Fatalf("virtual host put: %q", r.body)
	}
}

func mustReq(method, url string) *http.Request {
	r, _ := http.NewRequest(method, url, nil)
	return r
}

func TestPresigned(t *testing.T) {
	e := newEnv(t)
	e.want(e.s3("ci", "PUT", "/bkt/p.bin", "secret data"), 200, "")
	r, _ := http.NewRequest("GET", e.ts.URL+"/bkt/p.bin?response-content-type=application%2Fx-test", nil)
	r.Host = "s3.test"
	sigv4.Presign(r, e.keys["ci"][0], e.keys["ci"][1], "us-east-1", e.clock, time.Hour)
	url := r.URL.String()
	g, _ := http.NewRequest("GET", url, nil)
	g.Host = "s3.test"
	res := e.send(g)
	if res.status != 200 || res.body != "secret data" || res.h.Get("Content-Type") != "application/x-test" {
		t.Fatalf("presigned: %d %s %v", res.status, res.body, res.h)
	}
	e.clock = e.clock.Add(2 * time.Hour)
	g, _ = http.NewRequest("GET", url, nil)
	g.Host = "s3.test"
	e.want(e.send(g), 403, "AccessDenied")
}

func TestChunkedUpload(t *testing.T) {
	e := newEnv(t)
	body := bytes.Repeat([]byte("chunky"), 50000)
	for _, mode := range []string{sigv4.StreamingSigned, sigv4.StreamingUnsignedTrailer, sigv4.StreamingSignedTrailer} {
		r, _ := http.NewRequest("PUT", e.ts.URL+"/bkt/chunked-"+mode, nil)
		r.Host = "s3.test"
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(body)))
		trailer := ""
		if mode != sigv4.StreamingSigned {
			trailer = "x-amz-checksum-crc32"
			r.Header.Set("X-Amz-Trailer", trailer)
		}
		sigv4.Sign(r, e.keys["ci"][0], e.keys["ci"][1], "us-east-1", e.clock, mode)
		q, err := sigv4.Parse(r)
		if err != nil {
			t.Fatal(err)
		}
		enc := sigv4.EncodeChunked(q, e.keys["ci"][1], body, 65536, trailer)
		r.Body = io.NopCloser(bytes.NewReader(enc))
		r.ContentLength = int64(len(enc))
		e.want(e.send(r), 200, "")
		got := e.s3("ci", "GET", "/bkt/chunked-"+mode, "")
		if !bytes.Equal([]byte(got.body), body) || got.h.Get("Content-Encoding") != "" {
			t.Fatalf("%s: %d bytes, encoding %q", mode, len(got.body), got.h.Get("Content-Encoding"))
		}
	}
}

type listResult struct {
	Contents []struct {
		Key  string
		ETag string
		Size int64
	}
	CommonPrefixes []struct{ Prefix string }
	IsTruncated    bool
	NextToken      string `xml:"NextContinuationToken"`
	NextMarker     string
	KeyCount       int
}

func (e *env) list(path string) listResult {
	e.t.Helper()
	r := e.s3("ci", "GET", path, "")
	e.want(r, 200, "")
	var l listResult
	if err := xml.Unmarshal([]byte(r.body), &l); err != nil {
		e.t.Fatal(err)
	}
	return l
}

func keys(l listResult) string {
	var s []string
	for _, c := range l.Contents {
		s = append(s, c.Key)
	}
	for _, p := range l.CommonPrefixes {
		s = append(s, p.Prefix+"*")
	}
	return strings.Join(s, ",")
}

func TestList(t *testing.T) {
	e := newEnv(t)
	for _, k := range []string{"a.txt", "d/1", "d/2", "d/sub/3", "e/1", "f", "space here", "plus+sign"} {
		e.want(e.s3("ci", "PUT", "/bkt/"+strings.ReplaceAll(k, " ", "%20"), k), 200, "")
	}
	if l := e.list("/bkt?list-type=2"); keys(l) != "a.txt,d/1,d/2,d/sub/3,e/1,f,plus+sign,space here" || l.KeyCount != 8 || l.IsTruncated {
		t.Fatalf("all: %s %+v", keys(l), l)
	}
	if l := e.list("/bkt?list-type=2&delimiter=%2F"); keys(l) != "a.txt,f,plus+sign,space here,d/*,e/*" {
		t.Fatalf("delimiter: %s", keys(l))
	}
	if l := e.list("/bkt?list-type=2&prefix=d%2F&delimiter=%2F"); keys(l) != "d/1,d/2,d/sub/*" {
		t.Fatalf("prefix d/: %s", keys(l))
	}
	// Pages of two, with common prefixes counting as one.
	var all []string
	token := ""
	for i := 0; i < 10; i++ {
		p := "/bkt?list-type=2&delimiter=%2F&max-keys=2"
		if token != "" {
			p += "&continuation-token=" + token
		}
		l := e.list(p)
		all = append(all, keys(l))
		if !l.IsTruncated {
			break
		}
		token = l.NextToken
	}
	if got := strings.Join(all, "|"); got != "a.txt,d/*|f,e/*|plus+sign,space here" {
		t.Fatalf("pages: %s", got)
	}
	// V1 with markers.
	l := e.list("/bkt?delimiter=%2F&max-keys=2")
	if keys(l) != "a.txt,d/*" || !l.IsTruncated || l.NextMarker != "d/" {
		t.Fatalf("v1: %s %+v", keys(l), l)
	}
	if l := e.list("/bkt?delimiter=%2F&max-keys=2&marker=d%2F"); keys(l) != "f,e/*" {
		t.Fatalf("v1 page 2: %s", keys(l))
	}
	// encoding-type=url
	if l := e.list("/bkt?list-type=2&prefix=s&encoding-type=url"); keys(l) != "space%20here" {
		t.Fatalf("url encoding: %s", keys(l))
	}
	// Buckets.
	r := e.s3("reader", "GET", "/", "")
	if r.status != 200 || !strings.Contains(r.body, "<Name>pub</Name>") || strings.Contains(r.body, "<Name>rawrepo</Name>") {
		t.Fatalf("list buckets: %d %s", r.status, r.body)
	}
}

func TestDeleteObjectsAndCopy(t *testing.T) {
	e := newEnv(t)
	for _, k := range []string{"x1", "x2", "x3"} {
		e.want(e.s3("ci", "PUT", "/bkt/"+k, "content "+k, hdr("X-Amz-Meta-K", k)), 200, "")
	}
	// Copy within the store shares the blob.
	before := countBlobs(t, e.bs)
	r := e.s3("ci", "PUT", "/bkt/copy", "", hdr("X-Amz-Copy-Source", "/bkt/x1"))
	e.want(r, 200, "")
	if !strings.Contains(r.body, "<CopyObjectResult") || countBlobs(t, e.bs) != before {
		t.Fatalf("copy: %s, blobs %d -> %d", r.body, before, countBlobs(t, e.bs))
	}
	if g := e.s3("ci", "GET", "/bkt/copy", ""); g.body != "content x1" || g.h.Get("X-Amz-Meta-K") != "x1" {
		t.Fatalf("copied: %q %v", g.body, g.h)
	}
	e.want(e.s3("ci", "PUT", "/bkt/x1", "", hdr("X-Amz-Copy-Source", "bkt/x1")), 400, "InvalidRequest")
	r = e.s3("ci", "PUT", "/bkt/x1", "", hdr("X-Amz-Copy-Source", "bkt/x1"), hdr("X-Amz-Metadata-Directive", "REPLACE"), hdr("X-Amz-Meta-New", "y"))
	e.want(r, 200, "")
	if g := e.s3("ci", "HEAD", "/bkt/x1", ""); g.h.Get("X-Amz-Meta-New") != "y" || g.h.Get("X-Amz-Meta-K") != "" {
		t.Fatalf("replaced metadata: %v", g.h)
	}
	body := `<Delete><Object><Key>x1</Key></Object><Object><Key>x2</Key></Object><Object><Key>nope</Key></Object></Delete>`
	r = e.s3("ci", "POST", "/bkt?delete", body)
	e.want(r, 200, "")
	if strings.Count(r.body, "<Deleted>") != 3 {
		t.Fatalf("delete objects: %s", r.body)
	}
	e.want(e.s3("ci", "GET", "/bkt/x2", ""), 404, "NoSuchKey")
	// A reader may list the bucket but not delete: each key fails.
	r = e.s3("reader", "POST", "/bkt?delete", body)
	if r.status != 200 || strings.Count(r.body, "<Code>AccessDenied</Code>") != 3 {
		t.Fatalf("reader's delete: %d %s", r.status, r.body)
	}
}

func countBlobs(t *testing.T, bs *blob.Store) int {
	n := 0
	bs.Walk(func(string, time.Time) error { n++; return nil })
	return n
}

func TestBuckets(t *testing.T) {
	e := newEnv(t)
	e.want(e.s3("ci", "PUT", "/newbucket", ""), 403, "AccessDenied")
	e.want(e.s3("admin", "PUT", "/Bad_Name", ""), 400, "InvalidBucketName")
	e.want(e.s3("admin", "PUT", "/newbucket", ""), 200, "")
	e.want(e.s3("admin", "PUT", "/newbucket", ""), 409, "BucketAlreadyOwnedByYou")
	e.want(e.s3("admin", "PUT", "/rawrepo", ""), 409, "BucketAlreadyExists")
	e.want(e.s3("admin", "HEAD", "/newbucket", ""), 200, "")
	e.want(e.s3("admin", "PUT", "/newbucket/k", "v"), 200, "")
	e.want(e.s3("admin", "DELETE", "/newbucket", ""), 409, "BucketNotEmpty")
	e.want(e.s3("admin", "DELETE", "/newbucket/k", ""), 204, "")
	e.want(e.s3("ci", "DELETE", "/newbucket", ""), 403, "AccessDenied")
	e.want(e.s3("admin", "DELETE", "/newbucket", ""), 204, "")
	e.want(e.s3("admin", "HEAD", "/newbucket", ""), 404, "")
	if r := e.s3("ci", "GET", "/bkt?location", ""); r.status != 200 || !strings.Contains(r.body, "<LocationConstraint") {
		t.Fatalf("location: %s", r.body)
	}
	e.want(e.s3("ci", "GET", "/bkt?versioning", ""), 200, "")
	e.want(e.s3("ci", "GET", "/bkt?policy", ""), 404, "NoSuchBucketPolicy")
	e.want(e.s3("ci", "PUT", "/bkt?tagging", "<Tagging/>"), 501, "NotImplemented")
}

func TestMultipart(t *testing.T) {
	e := newEnv(t)
	r := e.s3("ci", "POST", "/bkt/big.bin?uploads", "", hdr("Content-Type", "application/x-big"), hdr("X-Amz-Meta-M", "1"))
	e.want(r, 200, "")
	var init struct{ UploadId string }
	xml.Unmarshal([]byte(r.body), &init)
	id := init.UploadId
	p1 := strings.Repeat("a", 5<<20)
	p2 := strings.Repeat("b", 5<<20)
	p3 := "tail"
	etags := map[int]string{}
	for n, p := range map[int]string{1: p1, 2: p2, 3: p3} {
		r := e.s3("ci", "PUT", fmt.Sprintf("/bkt/big.bin?partNumber=%d&uploadId=%s", n, id), p)
		e.want(r, 200, "")
		etags[n] = r.h.Get("ETag")
	}
	e.want(e.s3("reader", "PUT", "/bkt/big.bin?partNumber=1&uploadId="+id, "x"), 403, "AccessDenied")
	if r := e.s3("ci", "GET", "/bkt/big.bin?uploadId="+id, ""); strings.Count(r.body, "<Part>") != 3 {
		t.Fatalf("list parts: %s", r.body)
	}
	if r := e.s3("ci", "GET", "/bkt?uploads", ""); !strings.Contains(r.body, id) {
		t.Fatalf("list uploads: %s", r.body)
	}
	complete := func(parts ...int) string {
		var b strings.Builder
		b.WriteString("<CompleteMultipartUpload>")
		for _, n := range parts {
			fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", n, etags[n])
		}
		b.WriteString("</CompleteMultipartUpload>")
		return b.String()
	}
	e.want(e.s3("ci", "POST", "/bkt/big.bin?uploadId="+id, complete(2, 1, 3)), 400, "InvalidPartOrder")
	e.want(e.s3("ci", "POST", "/bkt/big.bin?uploadId="+id, complete(1, 3, 2)), 400, "InvalidPartOrder")
	e.want(e.s3("ci", "POST", "/bkt/big.bin?uploadId="+id, complete(3, 3)), 400, "InvalidPartOrder")
	r = e.s3("ci", "POST", "/bkt/big.bin?uploadId="+id, complete(1, 2, 3))
	e.want(r, 200, "")
	h := md5.New()
	for _, n := range []int{1, 2, 3} {
		raw, _ := hex.DecodeString(strings.Trim(etags[n], `"`))
		h.Write(raw)
	}
	wantETag := `"` + hex.EncodeToString(h.Sum(nil)) + `-3"`
	if !strings.Contains(r.body, "<ETag>&#34;"+strings.Trim(wantETag, `"`)+"&#34;</ETag>") && !strings.Contains(r.body, wantETag) {
		t.Fatalf("complete: %s (want %s)", r.body, wantETag)
	}
	g := e.s3("ci", "GET", "/bkt/big.bin", "")
	if g.body != p1+p2+p3 || g.h.Get("ETag") != wantETag || g.h.Get("Content-Type") != "application/x-big" || g.h.Get("X-Amz-Meta-M") != "1" {
		t.Fatalf("object: %d bytes, %v", len(g.body), g.h)
	}
	e.want(e.s3("ci", "POST", "/bkt/big.bin?uploadId="+id, complete(1, 2, 3)), 404, "NoSuchUpload")
	// A small part that is not the last.
	r = e.s3("ci", "POST", "/bkt/small?uploads", "")
	xml.Unmarshal([]byte(r.body), &init)
	for n, p := range map[int]string{1: "small", 2: "parts"} {
		etags[n] = e.s3("ci", "PUT", fmt.Sprintf("/bkt/small?partNumber=%d&uploadId=%s", n, init.UploadId), p).h.Get("ETag")
	}
	e.want(e.s3("ci", "POST", "/bkt/small?uploadId="+init.UploadId, complete(1, 2)), 400, "EntityTooSmall")
	e.want(e.s3("ci", "DELETE", "/bkt/small?uploadId="+init.UploadId, ""), 204, "")
	e.want(e.s3("ci", "GET", "/bkt/small?uploadId="+init.UploadId, ""), 404, "NoSuchUpload")
	// Stale uploads go.
	r = e.s3("ci", "POST", "/bkt/stale?uploads", "")
	xml.Unmarshal([]byte(r.body), &init)
	e.clock = e.clock.Add(UploadMaxAge + time.Hour)
	if n, err := e.h.SweepUploads(context.Background(), UploadMaxAge); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
}
