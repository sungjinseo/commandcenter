package s3api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
)

var ctx = context.Background()

type env struct {
	t    *testing.T
	ts   *httptest.Server
	h    *Handler
	db   *meta.DB
	auth *auth.Authenticator
	keys map[string]meta.S3Key // by user
}

// newEnv serves the S3 API over buckets data (writePolicy allow), once
// (allow_once), pub (anonymous read) and a maven repository, which is no
// bucket. Users: admin; writer (read, add, edit, delete on data and once);
// adder (read and add on data, no edit); reader (read on data). Each has an
// access key.
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
	if err := db.SetSecretKey(bytes.Repeat([]byte("k"), 32)); err != nil {
		t.Fatal(err)
	}
	all := []string{"read", "browse", "add", "edit", "delete"}
	for user, actions := range map[string][]string{"writer": all, "adder": {"read", "add"}, "reader": {"read"}} {
		role := meta.Role{ID: "role-" + user, Privileges: []meta.Privilege{{Target: "repo:data", Actions: actions}, {Target: "repo:once", Actions: actions}}}
		if err := db.CreateRole(ctx, role); err != nil {
			t.Fatal(err)
		}
		if err := db.CreateUser(ctx, meta.User{ID: user, PasswordHash: "x", Roles: []string{role.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateUser(ctx, meta.User{ID: "admin", PasswordHash: "x", Roles: []string{"admin"}}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Open(ctx, db, db)
	if err != nil {
		t.Fatal(err)
	}
	a.AnonymousRead = func(name string) *bool {
		open := name == "pub"
		return &open
	}
	defs := []config.Repository{
		{Name: "data", Format: "raw", Type: "hosted", WritePolicy: "allow"},
		{Name: "once", Format: "raw", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "pub", Format: "raw", Type: "hosted", WritePolicy: "allow"},
		{Name: "jars", Format: "maven", Type: "hosted", WritePolicy: "allow"},
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: db, Now: time.Now}
	h := &Handler{Registry: repo.NewRegistry(nil, defs), Store: store, Auth: a, Region: "us-east-1", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e := &env{t: t, h: h, db: db, auth: a, keys: map[string]meta.S3Key{}}
	e.ts = httptest.NewServer(h)
	t.Cleanup(e.ts.Close)
	for i, user := range []string{"admin", "writer", "adder", "reader"} {
		k := meta.S3Key{ID: fmt.Sprintf("CCTESTKEY%011d", i), Owner: user, Secret: "secret-of-" + user, Created: time.Now()}
		if err := db.CreateS3Key(ctx, k); err != nil {
			t.Fatal(err)
		}
		e.keys[user] = k
	}
	return e
}

// sign signs r with k as an SDK does: the payload's SHA-256, at time at,
// for region.
func sign(r *http.Request, k meta.S3Key, region string, at time.Time, body []byte) {
	sum := sha256Hex(string(body))
	r.Header.Set("X-Amz-Content-Sha256", sum)
	r.Header.Set("X-Amz-Date", at.UTC().Format(timeFormat))
	cred := credential{k.ID, at.UTC().Format(dateFormat), region, "s3"}
	s := sigRequest{cred: cred, signed: []string{"host", "x-amz-content-sha256", "x-amz-date"}, at: at, payload: sum}
	r.Header.Set("Authorization", algorithm+" Credential="+k.ID+"/"+cred.scope()+", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+
		s.expected(r, signingKey(k.Secret, cred)))
}

type reqOpt func(*http.Request)

func hdr(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// do sends a request signed with user's key ("" for anonymous) and returns
// the response and its body.
func (e *env) do(user, method, target string, body []byte, opts ...reqOpt) (*http.Response, string) {
	e.t.Helper()
	r, err := http.NewRequest(method, e.ts.URL+target, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, o := range opts {
		o(r)
	}
	if user != "" {
		sign(r, e.keys[user], "us-east-1", time.Now(), body)
	}
	resp, err := e.ts.Client().Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// code is the S3 error code of an error body.
func code(body string) string {
	var e struct{ Code string }
	xml.Unmarshal([]byte(body), &e)
	return e.Code
}

// check sends a request as do does and wants status and the error code
// errCode ("" for any).
func (e *env) check(status int, errCode, user, method, target string, body []byte, opts ...reqOpt) {
	e.t.Helper()
	e.wantResp(status, errCode)(e.do(user, method, target, body, opts...))
}

func (e *env) wantResp(status int, errCode string) func(*http.Response, string) {
	return func(resp *http.Response, body string) {
		e.t.Helper()
		if resp.StatusCode != status || errCode != "" && code(body) != errCode {
			e.t.Fatalf("%s %s: %d %s, want %d %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body, status, errCode)
		}
	}
}

func TestAuthenticationErrors(t *testing.T) {
	e := newEnv(t)
	e.check(200, "", "writer", "PUT", "/data/a.txt", []byte("a"))
	at := time.Now()
	for _, c := range []struct {
		name         string
		mutate       func(r *http.Request)
		status       int
		code, region string
		at           time.Time
	}{
		{"bad signature", func(r *http.Request) { r.Header.Set("X-Amz-Date", at.Add(time.Second).UTC().Format(timeFormat)) }, 403, "SignatureDoesNotMatch", "us-east-1", at},
		{"wrong region", nil, 400, "AuthorizationHeaderMalformed", "eu-west-1", at},
		{"clock skew", nil, 403, "RequestTimeTooSkewed", "us-east-1", at.Add(-16 * time.Minute)},
		{"within the skew", nil, 200, "", "us-east-1", at.Add(14 * time.Minute)},
		{"unknown key", func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), e.keys["writer"].ID, "CCNOSUCHKEY000000000", 1))
		}, 403, "InvalidAccessKeyId", "us-east-1", at},
		{"signature version 2", func(r *http.Request) { r.Header.Set("Authorization", "AWS "+e.keys["writer"].ID+":c2lnbmF0dXJl") }, 400, "InvalidRequest", "us-east-1", at},
	} {
		r, _ := http.NewRequest("GET", e.ts.URL+"/data/a.txt", nil)
		sign(r, e.keys["writer"], c.region, c.at, nil)
		if c.mutate != nil {
			c.mutate(r)
		}
		resp, err := e.ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || code(string(b)) != c.code {
			t.Fatalf("%s: %d %s", c.name, resp.StatusCode, b)
		}
	}
}

// presign makes a presigned URL for user's key, issued at at.
func (e *env) presign(user, method, path string, at time.Time, expires int) string {
	k := e.keys[user]
	cred := credential{k.ID, at.UTC().Format(dateFormat), "us-east-1", "s3"}
	q := url.Values{"X-Amz-Algorithm": {algorithm}, "X-Amz-Credential": {k.ID + "/" + cred.scope()}, "X-Amz-Date": {at.UTC().Format(timeFormat)},
		"X-Amz-Expires": {fmt.Sprint(expires)}, "X-Amz-SignedHeaders": {"host"}}
	r := httptest.NewRequest(method, e.ts.URL+path+"?"+q.Encode(), nil)
	r.Host = strings.TrimPrefix(e.ts.URL, "http://")
	s := sigRequest{cred: cred, signed: []string{"host"}, at: at, presigned: true, payload: unsignedBody}
	q.Set("X-Amz-Signature", s.expected(r, signingKey(k.Secret, cred)))
	return path + "?" + q.Encode()
}

func TestPresigned(t *testing.T) {
	e := newEnv(t)
	e.check(200, "", "writer", "PUT", "/data/p.txt", []byte("presigned"))
	resp, body := e.do("", "GET", e.presign("writer", "GET", "/data/p.txt", time.Now(), 60), nil)
	if resp.StatusCode != 200 || body != "presigned" {
		t.Fatalf("presigned GET: %d %s", resp.StatusCode, body)
	}
	e.check(403, "AccessDenied", "", "GET", e.presign("writer", "GET", "/data/p.txt", time.Now().Add(-2*time.Minute), 60), nil)
	e.check(403, "SignatureDoesNotMatch", "", "GET", strings.Replace(e.presign("writer", "GET", "/data/p.txt", time.Now(), 60), "X-Amz-Expires=60", "X-Amz-Expires=61", 1), nil)
	e.check(400, "AuthorizationQueryParametersError", "", "GET", e.presign("writer", "GET", "/data/p.txt", time.Now(), 8*86400), nil)
	resp, _ = e.do("", "PUT", e.presign("writer", "PUT", "/data/via-url.txt", time.Now(), 60), []byte("put by URL"))
	if resp.StatusCode != 200 {
		t.Fatalf("presigned PUT: %d", resp.StatusCode)
	}
	if _, body := e.do("reader", "GET", "/data/via-url.txt", nil); body != "put by URL" {
		t.Fatalf("after presigned PUT: %s", body)
	}
}

// Authorization is the key's user's privileges on the repository, as raw
// checks them, and the key and the user must still be there.
func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	e.check(403, "AccessDenied", "reader", "PUT", "/data/r.txt", []byte("x"))
	e.check(403, "AccessDenied", "", "PUT", "/data/r.txt", []byte("x"))
	e.check(200, "", "adder", "PUT", "/data/a.txt", []byte("first"))
	e.check(403, "AccessDenied", "adder", "PUT", "/data/a.txt", []byte("again")) // overwrite needs edit
	e.check(200, "", "writer", "PUT", "/data/a.txt", []byte("again"))
	e.check(200, "", "writer", "PUT", "/once/a.txt", []byte("first"))
	e.check(403, "AccessDenied", "writer", "PUT", "/once/a.txt", []byte("again")) // allow_once
	e.check(403, "AccessDenied", "adder", "DELETE", "/data/a.txt", nil)
	e.check(403, "AccessDenied", "", "GET", "/data/a.txt", nil)
	e.check(403, "AccessDenied", "writer", "PUT", "/pub/open.txt", nil)
	e.check(200, "", "admin", "PUT", "/pub/open.txt", []byte("open"))
	if resp, body := e.do("", "GET", "/pub/open.txt", nil); resp.StatusCode != 200 || body != "open" {
		t.Fatalf("anonymous read of pub: %d %s", resp.StatusCode, body)
	}
	e.check(403, "AccessDenied", "", "GET", "/jars/", nil) // hidden from who cannot read it
	e.check(404, "NoSuchBucket", "admin", "GET", "/jars/", nil)
	e.check(403, "AccessDenied", "admin", "PUT", "/new-bucket", nil)
	e.check(409, "BucketAlreadyOwnedByYou", "writer", "PUT", "/data", nil)
	e.check(400, "InvalidArgument", "writer", "PUT", "/data/a//b", []byte("x"))

	// A revoked key stops at once; so does a disabled user's.
	if err := e.db.DeleteS3Key(ctx, e.keys["writer"].ID); err != nil {
		t.Fatal(err)
	}
	e.check(403, "InvalidAccessKeyId", "writer", "GET", "/data/a.txt", nil)
	u, err := e.db.GetUser(ctx, "reader")
	if err != nil {
		t.Fatal(err)
	}
	u.Status = meta.StatusDisabled
	if err := e.db.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := e.auth.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	e.check(403, "InvalidAccessKeyId", "reader", "GET", "/data/a.txt", nil)
	// Deleting a user deletes its keys.
	if err := e.db.DeleteUser(ctx, "adder"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.GetS3Key(ctx, e.keys["adder"].ID); err != meta.ErrNotFound {
		t.Fatalf("a deleted user's key: %v", err)
	}
}

func TestObjects(t *testing.T) {
	e := newEnv(t)
	body := []byte("hello object")
	resp, _ := e.do("writer", "PUT", "/data/dir/o.bin", body, hdr("Content-Type", "application/x-thing"), hdr("X-Amz-Meta-Color", "Blue"),
		hdr("Cache-Control", "no-cache"))
	sum := md5.Sum(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	if resp.StatusCode != 200 || resp.Header.Get("ETag") != etag {
		t.Fatalf("put: %d %s", resp.StatusCode, resp.Header.Get("ETag"))
	}
	resp, got := e.do("reader", "GET", "/data/dir/o.bin", nil)
	if got != string(body) || resp.Header.Get("Content-Type") != "application/x-thing" || resp.Header.Get("X-Amz-Meta-Color") != "Blue" ||
		resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("ETag") != etag {
		t.Fatalf("get: %v %s", resp.Header, got)
	}
	e.check(304, "", "reader", "GET", "/data/dir/o.bin", nil, hdr("If-None-Match", etag))
	e.check(412, "PreconditionFailed", "reader", "GET", "/data/dir/o.bin", nil, hdr("If-Match", `"other"`))
	if resp, got := e.do("reader", "GET", "/data/dir/o.bin", nil, hdr("Range", "bytes=6-11")); resp.StatusCode != 206 || got != "object" {
		t.Fatalf("range: %d %s", resp.StatusCode, got)
	}
	e.check(412, "PreconditionFailed", "writer", "PUT", "/data/dir/o.bin", []byte("x"), hdr("If-None-Match", "*"))
	e.check(200, "", "writer", "PUT", "/data/dir/new.bin", []byte("x"), hdr("If-None-Match", "*"))
	e.check(400, "BadDigest", "writer", "PUT", "/data/dir/new.bin", []byte("x"), hdr("Content-MD5", "1B2M2Y8AsgTpgAmY7PhCfg=="))
	// The S3 object is the raw asset at the same path.
	a, err := e.h.Store.Get(ctx, "data", "dir/o.bin")
	if err != nil || a.ContentType != "application/x-thing" || a.Size != int64(len(body)) {
		t.Fatalf("asset: %+v %v", a, err)
	}
	// Copy within the bucket keeps the attributes.
	e.check(200, "", "writer", "PUT", "/data/copy.bin", nil, hdr("X-Amz-Copy-Source", "/data/dir/o.bin"))
	if resp, got := e.do("reader", "HEAD", "/data/copy.bin", nil); resp.Header.Get("X-Amz-Meta-Color") == "" || resp.Header.Get("ETag") != etag || got != "" {
		t.Fatalf("copy: %v", resp.Header)
	}
	e.check(403, "AccessDenied", "adder", "PUT", "/data/copy.bin", nil, hdr("X-Amz-Copy-Source", "/data/dir/o.bin"))
	e.check(204, "", "writer", "DELETE", "/data/copy.bin", nil)
	e.check(204, "", "writer", "DELETE", "/data/copy.bin", nil)
	e.check(404, "NoSuchKey", "reader", "GET", "/data/copy.bin", nil)
}

func TestListObjects(t *testing.T) {
	e := newEnv(t)
	keys := []string{"a/1", "a/2", "a/b/3", "a/c/4", "a0", "b/5", "c"}
	for _, k := range keys {
		e.check(200, "", "writer", "PUT", "/data/"+k, []byte(k))
	}
	type result struct {
		Contents              []struct{ Key string }
		CommonPrefixes        []struct{ Prefix string }
		IsTruncated           bool
		NextContinuationToken string
		NextMarker            string
	}
	list := func(query string) result {
		t.Helper()
		resp, body := e.do("reader", "GET", "/data?"+query, nil)
		var r result
		if resp.StatusCode != 200 || xml.Unmarshal([]byte(body), &r) != nil {
			t.Fatalf("list %s: %d %s", query, resp.StatusCode, body)
		}
		return r
	}
	names := func(r result) string {
		var out []string
		for _, c := range r.Contents {
			out = append(out, c.Key)
		}
		for _, p := range r.CommonPrefixes {
			out = append(out, p.Prefix)
		}
		return strings.Join(out, ",")
	}
	if got := names(list("list-type=2")); got != strings.Join(keys, ",") {
		t.Fatalf("all: %s", got)
	}
	if got := names(list("list-type=2&delimiter=/")); got != "a0,c,a/,b/" {
		t.Fatalf("delimiter: %s", got)
	}
	if got := names(list("list-type=2&prefix=a/&delimiter=/")); got != "a/1,a/2,a/b/,a/c/" {
		t.Fatalf("prefix and delimiter: %s", got)
	}
	if got := names(list("list-type=2&start-after=a/b/3")); got != "a/c/4,a0,b/5,c" {
		t.Fatalf("start-after: %s", got)
	}
	// Paging one entry at a time, past common prefixes, ends where a whole listing does.
	var all []string
	token := ""
	for i := 0; i < 10; i++ {
		r := list("list-type=2&delimiter=/&max-keys=1&continuation-token=" + url.QueryEscape(token))
		all = append(all, names(r))
		if !r.IsTruncated {
			break
		}
		token = r.NextContinuationToken
	}
	if strings.Join(all, ",") != "a/,a0,b/,c" {
		t.Fatalf("pages: %v", all)
	}
	r := list("max-keys=2&prefix=a/")
	if names(r) != "a/1,a/2" || !r.IsTruncated || r.NextMarker != "a/2" {
		t.Fatalf("v1: %+v", r)
	}
	if got := names(list("marker=a/2&prefix=a/")); got != "a/b/3,a/c/4" {
		t.Fatalf("v1 marker: %s", got)
	}
}

func TestMultipart(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do("writer", "POST", "/data/big.bin?uploads", nil, hdr("Content-Type", "application/x-big"), hdr("X-Amz-Meta-Kind", "parts"))
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	if resp.StatusCode != 200 || xml.Unmarshal([]byte(body), &init) != nil || init.UploadID == "" {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	id, key := init.UploadID, "big.bin"
	parts := [][]byte{bytes.Repeat([]byte("1"), minPartSize), bytes.Repeat([]byte("2"), minPartSize), []byte("tail")}
	var etags []string
	all := md5.New()
	for i, p := range parts {
		resp, _ := e.do("writer", "PUT", fmt.Sprintf("/data/big.bin?partNumber=%d&uploadId=%s", i+1, id), p)
		sum := md5.Sum(p)
		if resp.StatusCode != 200 || resp.Header.Get("ETag") != `"`+hex.EncodeToString(sum[:])+`"` {
			t.Fatalf("part %d: %d %s", i+1, resp.StatusCode, resp.Header.Get("ETag"))
		}
		etags = append(etags, resp.Header.Get("ETag"))
		all.Write(sum[:])
	}
	// A part sent again replaces the first.
	e.check(200, "", "writer", "PUT", "/data/big.bin?partNumber=3&uploadId="+id, []byte("tail"))
	e.check(200, "", "adder", "PUT", "/data/big.bin?partNumber=3&uploadId="+id, []byte("x")) // add suffices for a part
	e.check(200, "", "writer", "PUT", "/data/big.bin?partNumber=3&uploadId="+id, []byte("tail"))
	e.check(403, "AccessDenied", "reader", "PUT", "/data/big.bin?partNumber=3&uploadId="+id, []byte("x"))
	complete := func(nums ...int) (*http.Response, string) {
		var b strings.Builder
		b.WriteString("<CompleteMultipartUpload>")
		for _, n := range nums {
			fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", n, etags[n-1])
		}
		b.WriteString("</CompleteMultipartUpload>")
		return e.do("writer", "POST", "/data/"+key+"?uploadId="+id, []byte(b.String()))
	}
	e.wantResp(400, "InvalidPartOrder")(complete(2, 1, 3))
	e.wantResp(400, "InvalidPartOrder")(complete(1, 1))
	e.check(404, "NoSuchUpload", "writer", "PUT", "/data/small?partNumber=1&uploadId="+id, []byte("x")) // another key
	resp, body = e.do("writer", "GET", "/data/big.bin?uploadId="+id, nil)
	if resp.StatusCode != 200 || strings.Count(body, "<Part>") != 3 {
		t.Fatalf("list parts: %s", body)
	}
	resp, body = complete(1, 2, 3)
	want := fmt.Sprintf(`"%s-3"`, hex.EncodeToString(all.Sum(nil)))
	var done struct{ ETag string }
	if resp.StatusCode != 200 || xml.Unmarshal([]byte(body), &done) != nil || done.ETag != want {
		t.Fatalf("complete: %d %s, want %s", resp.StatusCode, body, want)
	}
	resp, got := e.do("reader", "GET", "/data/big.bin", nil)
	if resp.Header.Get("ETag") != want || got != string(bytes.Join(parts, nil)) || resp.Header.Get("Content-Type") != "application/x-big" ||
		resp.Header.Get("X-Amz-Meta-Kind") != "parts" {
		t.Fatalf("get: %v (%d bytes)", resp.Header, len(got))
	}
	// The upload is gone, files and all.
	e.check(404, "NoSuchUpload", "writer", "GET", "/data/big.bin?uploadId="+id, nil)
	bs, _ := e.h.Store.BlobStore("data")
	if left, _ := filepath.Glob(bs.SessionPath("s3-*")); len(left) != 0 {
		t.Fatalf("left in tmp: %v", left)
	}
	// A part below 5 MiB that is not the last is refused; abort removes the upload.
	_, body = e.do("writer", "POST", "/data/small.bin?uploads", nil)
	xml.Unmarshal([]byte(body), &init)
	e.check(200, "", "writer", "PUT", "/data/small.bin?partNumber=1&uploadId="+init.UploadID, []byte("x"))
	e.check(200, "", "writer", "PUT", "/data/small.bin?partNumber=2&uploadId="+init.UploadID, []byte("y"))
	etags = []string{`"` + md5hex("x") + `"`, `"` + md5hex("y") + `"`}
	id, key = init.UploadID, "small.bin"
	e.wantResp(400, "EntityTooSmall")(complete(1, 2))
	if _, body := e.do("reader", "GET", "/data?uploads", nil); !strings.Contains(body, "<Key>small.bin</Key>") {
		t.Fatalf("list uploads: %s", body)
	}
	e.check(204, "", "writer", "DELETE", "/data/small.bin?uploadId="+id, nil)
	e.check(404, "NoSuchUpload", "writer", "DELETE", "/data/small.bin?uploadId="+id, nil)
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
