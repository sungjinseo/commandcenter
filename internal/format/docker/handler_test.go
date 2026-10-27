package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/server"
)

type env struct {
	t     *testing.T
	ts    *httptest.Server
	h     *Handler
	dir   string
	sink  *repotest.Sink
	now   time.Time
	repos []config.Repository
	extra []config.Repository // added to the hosted ones; ci may write them too
}

// newEnv serves the registry through the real server (its Basic
// authentication and body limits) with three hosted repositories: dh
// (anonymous read), priv (no anonymous read) and once (allow_once). ci may
// write all of them; reader may write none.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	e.open()
	return e
}

// open builds the handler and server over e.dir; a second call is a
// restart on the same data directory.
func (e *env) open() {
	t := e.t
	if e.ts != nil {
		e.ts.Close()
		e.h.Store.DB.Close()
	}
	bs, err := blob.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(metatest.Target(t, filepath.Join(e.dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	f, tr := false, true
	cfg := &config.Config{
		Users: []config.User{
			{Name: "admin", PasswordHash: string(hash), Roles: []string{"admin"}},
			{Name: "ci", PasswordHash: string(hash), Roles: []string{"deployer"}},
			{Name: "reader", PasswordHash: string(hash)},
		},
		Roles: map[string]config.Role{"deployer": {Write: []string{"dh", "priv", "once"}}},
		Repositories: []config.Repository{
			{Name: "dh", Format: "docker", Type: "hosted", WritePolicy: "allow", AnonymousRead: &tr},
			{Name: "priv", Format: "docker", Type: "hosted", WritePolicy: "allow", AnonymousRead: &f},
			{Name: "once", Format: "docker", Type: "hosted", WritePolicy: "allow_once", AnonymousRead: &tr},
			{Name: "ro", Format: "docker", Type: "hosted", WritePolicy: "deny", AnonymousRead: &tr},
			{Name: "npm", Format: "npm", Type: "hosted", AnonymousRead: &tr},
		},
	}
	for _, x := range e.extra {
		cfg.Repositories = append(cfg.Repositories, x)
		cfg.Roles["deployer"] = config.Role{Write: append(cfg.Roles["deployer"].Write, x.Name)}
	}
	for i := range cfg.Repositories {
		config.ApplyRepositoryDefaults(&cfg.Repositories[i])
	}
	e.repos = cfg.Repositories
	sink := &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: db, Now: func() time.Time { return e.now }, Events: sink}
	e.sink = sink
	authn := auth.New(cfg, db)
	registry := repo.NewRegistry(nil, cfg.Repositories)
	var base string
	fetcher := NewFetcher(store, 4)
	fetcher.Now = func() time.Time { return e.now }
	h := New(store, fetcher, authn, registry, func() string { return base }, []byte("test-key"))
	h.Now = func() time.Time { return e.now }
	srv := &server.Server{Registry: registry, Handlers: map[string]repo.Handler{"docker": h}, Auth: authn, Docker: h,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	base = e.ts.URL
	e.h = h
}

type opt func(*http.Request)

func bearer(tok string) opt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}
func basic(user string) opt { return func(r *http.Request) { r.SetBasicAuth(user, "pw") } }
func hdr(k, v string) opt   { return func(r *http.Request) { r.Header.Set(k, v) } }

// do sends a request and returns the response with its body read.
func (e *env) do(method, path string, body []byte, opts ...opt) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, o := range opts {
		o(r)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// token fetches a token for scopes as user ("" for anonymous).
func (e *env) token(user string, scopes ...string) string {
	e.t.Helper()
	q := url.Values{}
	for _, s := range scopes {
		q.Add("scope", s)
	}
	var opts []opt
	if user != "" {
		opts = append(opts, basic(user))
	}
	resp, b := e.do("GET", "/v2/token?service=x&"+q.Encode(), nil, opts...)
	if resp.StatusCode != 200 {
		e.t.Fatalf("token for %q: %d %s", user, resp.StatusCode, b)
	}
	var out struct{ Token string }
	json.Unmarshal(b, &out)
	return out.Token
}

func code(b []byte) string {
	var out struct {
		Errors []struct{ Code string }
	}
	json.Unmarshal(b, &out)
	if len(out.Errors) == 0 {
		return ""
	}
	return out.Errors[0].Code
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func (e *env) check(resp *http.Response, body []byte, status int, c string) {
	e.t.Helper()
	if resp.StatusCode != status || (c != "" && code(body) != c) {
		e.t.Fatalf("%s %s: got %d %q, want %d %q: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, code(body), status, c, body)
	}
}

// pushBlob uploads b to name in one chunked session and returns its digest.
func (e *env) pushBlob(name string, b []byte, opts ...opt) string {
	e.t.Helper()
	resp, body := e.do("POST", "/v2/"+name+"/blobs/uploads/", nil, opts...)
	e.check(resp, body, 202, "")
	loc := resp.Header.Get("Location")
	d := digestOf(b)
	resp, body = e.do("PUT", loc+"?digest="+d, b, opts...)
	e.check(resp, body, 201, "")
	return d
}

// pushImage pushes a schema2 image with the given layers to name:tag and
// returns the manifest bytes and digest.
func (e *env) pushImage(name, tag string, layers [][]byte, opts ...opt) ([]byte, string) {
	e.t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers"}}`)
	m := map[string]any{"schemaVersion": 2, "mediaType": mtSchema2,
		"config": map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "digest": e.pushBlob(name, cfg, opts...), "size": len(cfg)}}
	var ls []map[string]any
	for _, l := range layers {
		ls = append(ls, map[string]any{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": e.pushBlob(name, l, opts...), "size": len(l)})
	}
	m["layers"] = ls
	body, _ := json.Marshal(m)
	resp, rb := e.do("PUT", "/v2/"+name+"/manifests/"+tag, body, append(opts, hdr("Content-Type", mtSchema2))...)
	e.check(resp, rb, 201, "")
	if got := resp.Header.Get("Docker-Content-Digest"); got != digestOf(body) {
		e.t.Fatalf("digest header %q", got)
	}
	return body, digestOf(body)
}

func TestPing(t *testing.T) {
	e := newEnv(t)
	resp, b := e.do("GET", "/v2/", nil)
	e.check(resp, b, 401, CodeUnauthorized)
	want := `Bearer realm="` + e.ts.URL + `/v2/token",service="` + strings.TrimPrefix(e.ts.URL, "http://") + `"`
	if got := resp.Header.Get("WWW-Authenticate"); got != want {
		t.Fatalf("challenge %q, want %q", got, want)
	}
	for _, o := range []opt{basic("reader"), bearer(e.token(""))} {
		resp, b = e.do("GET", "/v2/", nil, o)
		e.check(resp, b, 200, "")
		if resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
			t.Fatalf("version header missing: %v", resp.Header)
		}
	}
	// a push challenge names the scope
	resp, _ = e.do("POST", "/v2/dh/app/blobs/uploads/", nil)
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasSuffix(got, `,scope="repository:dh/app:pull,push"`) {
		t.Fatalf("push challenge %q", got)
	}
	resp, b = e.do("GET", "/v2/nope", nil, basic("ci"))
	e.check(resp, b, 404, CodeUnsupported)
}

func TestTokenFlow(t *testing.T) {
	e := newEnv(t)
	access := func(tok string) []scope {
		c, err := e.h.verify(tok)
		if err != nil {
			t.Fatal(err)
		}
		return c.Access
	}
	// anonymous: pull where anonymous read is on, nothing on priv
	got := access(e.token("", "repository:dh/app:pull,push", "repository:priv/app:pull,push", "registry:catalog:*"))
	if fmt.Sprint(got) != "[{repository dh/app [pull]} {repository priv/app []} {registry catalog [*]}]" {
		t.Fatalf("anonymous access %v", got)
	}
	// ci: everything it was granted; reader: pull only (authenticatedRead)
	if got := access(e.token("ci", "repository:priv/app:pull,push,delete")); fmt.Sprint(got) != "[{repository priv/app [pull push delete]}]" {
		t.Fatalf("ci access %v", got)
	}
	if got := access(e.token("reader", "repository:priv/app:pull,push")); fmt.Sprint(got) != "[{repository priv/app [pull]}]" {
		t.Fatalf("reader access %v", got)
	}
	// a token as the Basic password works like the other formats
	_, tok, err := e.h.Auth.CreateToken(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", e.ts.URL+"/v2/token?scope=repository:priv/app:push", nil)
	r.SetBasicAuth("ci", tok)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct{ Token string }
	json.Unmarshal(b, &out)
	if resp.StatusCode != 200 || fmt.Sprint(access(out.Token)) != "[{repository priv/app [push]}]" {
		t.Fatalf("api token: %d %s", resp.StatusCode, b)
	}
	// wrong password: 401 in the registry's JSON
	r, _ = http.NewRequest("GET", e.ts.URL+"/v2/token", nil)
	r.SetBasicAuth("ci", "nope")
	resp, _ = http.DefaultClient.Do(r)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 || code(b) != CodeUnauthorized {
		t.Fatalf("bad password: %d %s", resp.StatusCode, b)
	}
	// the token's scope is what counts: an anonymous token cannot push (401),
	// a signed-in user's token without push gets 403
	resp, b = e.do("POST", "/v2/dh/app/blobs/uploads/", nil, bearer(e.token("", "repository:dh/app:pull,push")))
	e.check(resp, b, 401, CodeUnauthorized)
	resp, b = e.do("POST", "/v2/dh/app/blobs/uploads/", nil, bearer(e.token("reader", "repository:dh/app:pull,push")))
	e.check(resp, b, 403, CodeDenied)
	resp, b = e.do("POST", "/v2/dh/app/blobs/uploads/", nil, bearer(e.token("ci", "repository:dh/other:pull,push")))
	e.check(resp, b, 403, CodeDenied)
	resp, b = e.do("POST", "/v2/dh/app/blobs/uploads/", nil, bearer(e.token("ci", "repository:dh/app:pull,push")))
	e.check(resp, b, 202, "")
	// expired, wrong service, tampered
	tok = e.token("ci", "repository:dh/app:pull")
	e.now = e.now.Add(TokenTTL + time.Second)
	resp, b = e.do("GET", "/v2/dh/app/tags/list", nil, bearer(tok))
	e.check(resp, b, 401, CodeUnauthorized)
	e.now = e.now.Add(-TokenTTL)
	other := e.h.mint(claims{Sub: "ci", Svc: "other.example", Exp: e.now.Add(time.Hour).Unix(), Access: []scope{{Type: "repository", Name: "dh/app", Actions: []string{"pull"}}}})
	resp, b = e.do("GET", "/v2/dh/app/tags/list", nil, bearer(other))
	e.check(resp, b, 401, CodeUnauthorized)
	resp, b = e.do("GET", "/v2/dh/app/tags/list", nil, bearer(tok[:len(tok)-2]+"zz"))
	e.check(resp, b, 401, CodeUnauthorized)
	// Basic directly on /v2/ works too
	resp, b = e.do("GET", "/v2/dh/app/tags/list", nil, basic("ci"))
	e.check(resp, b, 404, CodeNameUnknown)
	// an unknown repository looks like a forbidden one to whoever may not read it
	resp, b = e.do("GET", "/v2/nope/app/tags/list", nil)
	e.check(resp, b, 401, CodeUnauthorized)
	resp, b = e.do("GET", "/v2/npm/app/tags/list", nil, basic("ci"))
	e.check(resp, b, 404, CodeNameUnknown)
	// the token endpoint ignores a Bearer header (anonymous token)
	if got := access(e.token("", "repository:dh/app:pull")); fmt.Sprint(got) != "[{repository dh/app [pull]}]" {
		t.Fatalf("anonymous token %v", got)
	}
}

func TestPushPullSchema2(t *testing.T) {
	e := newEnv(t)
	ci := bearer(e.token("ci", "repository:dh/team/app:pull,push"))
	layer := bytes.Repeat([]byte("layer-one-"), 100)
	// chunked upload: POST, PATCH x2, PUT (no bytes)
	resp, b := e.do("POST", "/v2/dh/team/app/blobs/uploads/", nil, ci)
	e.check(resp, b, 202, "")
	loc, id := resp.Header.Get("Location"), resp.Header.Get("Docker-Upload-UUID")
	if loc != "/v2/dh/team/app/blobs/uploads/"+id || resp.Header.Get("Range") != "0-0" || id == "" {
		t.Fatalf("start: %v", resp.Header)
	}
	resp, b = e.do("PATCH", loc, layer[:400], ci, hdr("Content-Range", "0-399"), hdr("Content-Type", "application/octet-stream"))
	e.check(resp, b, 202, "")
	if resp.Header.Get("Range") != "0-399" || resp.Header.Get("Docker-Upload-UUID") != id {
		t.Fatalf("chunk 1: %v", resp.Header)
	}
	// a chunk out of order is refused with the current range
	resp, b = e.do("PATCH", loc, layer[500:], ci, hdr("Content-Range", "500-999"))
	e.check(resp, b, 416, CodeBlobUploadInvalid)
	if resp.Header.Get("Range") != "0-399" {
		t.Fatalf("out of order: %v", resp.Header)
	}
	resp, b = e.do("PATCH", loc, layer[400:], ci, hdr("Content-Range", "400-999"))
	e.check(resp, b, 202, "")
	resp, b = e.do("GET", loc, nil, ci)
	e.check(resp, b, 204, "")
	if resp.Header.Get("Range") != "0-999" {
		t.Fatalf("status: %v", resp.Header)
	}
	// wrong digest at completion: the upload is gone
	resp, b = e.do("PUT", loc+"?digest="+digestOf([]byte("other")), nil, ci)
	e.check(resp, b, 400, CodeDigestInvalid)
	resp, b = e.do("GET", loc, nil, ci)
	e.check(resp, b, 404, CodeBlobUploadUnknown)
	// again, completing with bytes in the PUT
	resp, b = e.do("POST", "/v2/dh/team/app/blobs/uploads/", nil, ci)
	e.check(resp, b, 202, "")
	loc = resp.Header.Get("Location")
	resp, b = e.do("PATCH", loc, layer[:400], ci)
	e.check(resp, b, 202, "")
	ld := digestOf(layer)
	resp, b = e.do("PUT", loc+"?digest="+ld, layer[400:], ci)
	e.check(resp, b, 201, "")
	if resp.Header.Get("Location") != "/v2/dh/team/app/blobs/"+ld || resp.Header.Get("Docker-Content-Digest") != ld {
		t.Fatalf("complete: %v", resp.Header)
	}
	// monolithic config
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	cd := digestOf(cfg)
	resp, b = e.do("POST", "/v2/dh/team/app/blobs/uploads/?digest="+cd, cfg, ci)
	e.check(resp, b, 201, "")
	resp, b = e.do("POST", "/v2/dh/team/app/blobs/uploads/?digest="+cd, []byte("tampered"), ci)
	e.check(resp, b, 400, CodeDigestInvalid)
	// manifest, stored verbatim (odd spacing included)
	manifest := []byte(`{"schemaVersion": 2,  "mediaType": "` + mtSchema2 + `", "config": {"mediaType": "application/vnd.docker.container.image.v1+json", "digest": "` + cd + `", "size": ` + fmt.Sprint(len(cfg)) + `}, "layers": [{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": "` + ld + `", "size": 1000}]}`)
	md := digestOf(manifest)
	resp, b = e.do("PUT", "/v2/dh/team/app/manifests/v1.0", manifest, ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 201, "")
	if resp.Header.Get("Docker-Content-Digest") != md || resp.Header.Get("Location") != "/v2/dh/team/app/manifests/"+md {
		t.Fatalf("manifest put: %v", resp.Header)
	}
	if ev := e.sink.Events(); len(ev) != 1 || ev[0].Kind != "upload" || ev[0].Actor != "ci" || ev[0].Subject != "team/app:v1.0" {
		t.Fatalf("events %v", ev)
	}
	// pull anonymously: by tag, by digest, HEAD
	for _, ref := range []string{"v1.0", md} {
		resp, b = e.do("GET", "/v2/dh/team/app/manifests/"+ref, nil)
		e.check(resp, b, 200, "")
		if !bytes.Equal(b, manifest) || resp.Header.Get("Content-Type") != mtSchema2 || resp.Header.Get("Docker-Content-Digest") != md {
			t.Fatalf("manifest %s: %v %s", ref, resp.Header, b)
		}
	}
	resp, b = e.do("HEAD", "/v2/dh/team/app/manifests/v1.0", nil)
	e.check(resp, b, 200, "")
	if resp.Header.Get("Docker-Content-Digest") != md || resp.ContentLength != int64(len(manifest)) {
		t.Fatalf("head: %v", resp.Header)
	}
	resp, b = e.do("GET", "/v2/dh/team/app/manifests/v9", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	// blobs: GET, HEAD, Range
	resp, b = e.do("GET", "/v2/dh/team/app/blobs/"+ld, nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, layer) || resp.Header.Get("Docker-Content-Digest") != ld || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("blob: %v", resp.Header)
	}
	resp, b = e.do("HEAD", "/v2/dh/team/app/blobs/"+ld, nil)
	e.check(resp, b, 200, "")
	if resp.ContentLength != int64(len(layer)) {
		t.Fatalf("blob head: %v", resp.Header)
	}
	resp, b = e.do("GET", "/v2/dh/team/app/blobs/"+ld, nil, hdr("Range", "bytes=10-19"))
	e.check(resp, b, 206, "")
	if !bytes.Equal(b, layer[10:20]) || resp.Header.Get("Content-Range") != "bytes 10-19/1000" {
		t.Fatalf("range: %v %q", resp.Header, b)
	}
	resp, b = e.do("GET", "/v2/dh/team/app/blobs/"+digestOf([]byte("nope")), nil)
	e.check(resp, b, 404, CodeBlobUnknown)
	resp, b = e.do("GET", "/v2/dh/other/blobs/"+ld, nil) // blobs are per image
	e.check(resp, b, 404, CodeBlobUnknown)
	// tags and catalog
	resp, b = e.do("GET", "/v2/dh/team/app/tags/list", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"dh/team/app","tags":["v1.0"]}` {
		t.Fatalf("tags: %s", b)
	}
	resp, b = e.do("GET", "/v2/_catalog", nil, bearer(e.token("", "registry:catalog:*")))
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"repositories":["dh/team/app"]}` {
		t.Fatalf("catalog: %s", b)
	}
	// the UI's download link
	resp, b = e.do("GET", "/repository/dh/team/app/manifests/v1.0", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, manifest) {
		t.Fatalf("ui manifest: %s", b)
	}
	resp, b = e.do("GET", "/repository/dh/team/app/blobs/"+ld, nil)
	e.check(resp, b, 200, "")
	resp, b = e.do("GET", "/repository/dh/team/app/tags/list", nil)
	e.check(resp, b, 404, "")
	// read-only repository
	resp, b = e.do("POST", "/v2/ro/app/blobs/uploads/", nil, basic("admin"))
	e.check(resp, b, 403, CodeDenied)
}

func ociManifest(e *env, name string, layer []byte, ci opt) ([]byte, string) {
	cfg := []byte(`{"architecture":"arm64","os":"linux","config":{"x":"` + string(layer[:4]) + `"}}`)
	m := []byte(`{"schemaVersion":2,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + e.pushBlob(name, cfg, ci) + `","size":` + fmt.Sprint(len(cfg)) + `},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"` + e.pushBlob(name, layer, ci) + `","size":` + fmt.Sprint(len(layer)) + `}]}`)
	d := digestOf(m)
	// no mediaType in the document: the Content-Type decides
	resp, b := e.do("PUT", "/v2/"+name+"/manifests/"+d, m, ci, hdr("Content-Type", mtOCIManifest))
	e.check(resp, b, 201, "")
	return m, d
}

func TestOCIIndex(t *testing.T) {
	e := newEnv(t)
	ci := bearer(e.token("ci", "repository:dh/app:pull,push"))
	_, d1 := ociManifest(e, "dh/app", []byte("amd64-layer"), ci)
	_, d2 := ociManifest(e, "dh/app", []byte("arm64-layer"), ci)
	index := []byte(`{"schemaVersion":2,"mediaType":"` + mtOCIIndex + `","manifests":[` +
		`{"mediaType":"` + mtOCIManifest + `","digest":"` + d1 + `","size":1,"platform":{"architecture":"amd64","os":"linux"}},` +
		`{"mediaType":"` + mtOCIManifest + `","digest":"` + d2 + `","size":1,"platform":{"architecture":"arm64","os":"linux"}}]}`)
	resp, b := e.do("PUT", "/v2/dh/app/manifests/latest", index, ci, hdr("Content-Type", mtOCIIndex))
	e.check(resp, b, 201, "")
	resp, b = e.do("GET", "/v2/dh/app/manifests/latest", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, index) || resp.Header.Get("Content-Type") != mtOCIIndex {
		t.Fatalf("index: %v %s", resp.Header, b)
	}
	resp, b = e.do("GET", "/v2/dh/app/manifests/"+d2, nil)
	e.check(resp, b, 200, "")
	if resp.Header.Get("Content-Type") != mtOCIManifest {
		t.Fatalf("platform manifest: %v", resp.Header)
	}
	// an index naming a manifest that is not there
	bad := strings.Replace(string(index), d2, digestOf([]byte("missing")), 1)
	resp, b = e.do("PUT", "/v2/dh/app/manifests/bad", []byte(bad), ci, hdr("Content-Type", mtOCIIndex))
	e.check(resp, b, 404, CodeManifestBlobUnknown)
	// a docker manifest list works the same
	list := strings.Replace(string(index), mtOCIIndex, mtManifestList, 1)
	resp, b = e.do("PUT", "/v2/dh/app/manifests/list", []byte(list), ci, hdr("Content-Type", mtManifestList))
	e.check(resp, b, 201, "")
}

// A chart pushed with `helm push chart.tgz oci://host/dh` is an OCI manifest
// whose config is application/vnd.cncf.helm.config.v1+json and whose one
// layer is the chart tarball, tagged with the chart version: a docker
// repository takes it as any image (the helm format serves index.yaml
// repositories; OCI charts need no helm repository).
func TestOCIHelmChart(t *testing.T) {
	e := newEnv(t)
	ci := bearer(e.token("ci", "repository:dh/charts/demo:pull,push"))
	cfg := []byte(`{"name":"demo","version":"1.0.0","description":"a chart","apiVersion":"v2"}`)
	chart := []byte("\x1f\x8b\x08\x00not really a tarball")
	m := []byte(`{"schemaVersion":2,"mediaType":"` + mtOCIManifest + `",` +
		`"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"` + e.pushBlob("dh/charts/demo", cfg, ci) + `","size":` + fmt.Sprint(len(cfg)) + `},` +
		`"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"` + e.pushBlob("dh/charts/demo", chart, ci) + `","size":` + fmt.Sprint(len(chart)) + `}],` +
		`"annotations":{"org.opencontainers.image.title":"demo","org.opencontainers.image.version":"1.0.0"}}`)
	resp, b := e.do("PUT", "/v2/dh/charts/demo/manifests/1.0.0", m, ci, hdr("Content-Type", mtOCIManifest))
	e.check(resp, b, 201, "")
	// helm pull: the manifest by tag, then the chart layer by digest
	resp, b = e.do("GET", "/v2/dh/charts/demo/manifests/1.0.0", nil, hdr("Accept", mtOCIManifest))
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, m) || resp.Header.Get("Content-Type") != mtOCIManifest {
		t.Fatalf("manifest: %v %s", resp.Header, b)
	}
	resp, b = e.do("GET", "/v2/dh/charts/demo/blobs/"+digestOf(chart), nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, chart) {
		t.Fatalf("chart layer: %q", b)
	}
	resp, b = e.do("GET", "/v2/dh/charts/demo/tags/list", nil)
	e.check(resp, b, 200, "")
	if !strings.Contains(string(b), `"tags":["1.0.0"]`) {
		t.Fatalf("tags: %s", b)
	}
}

func TestManifestRejections(t *testing.T) {
	e := newEnv(t)
	ci := bearer(e.token("ci", "repository:dh/app:pull,push"))
	cfg := []byte(`{}`)
	cd := e.pushBlob("dh/app", cfg, ci)
	missing := digestOf([]byte("never uploaded"))
	m := []byte(`{"schemaVersion":2,"mediaType":"` + mtSchema2 + `","config":{"digest":"` + cd + `","size":2},"layers":[{"digest":"` + missing + `","size":5}]}`)
	resp, b := e.do("PUT", "/v2/dh/app/manifests/v1", m, ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 404, CodeManifestBlobUnknown)
	if !strings.Contains(string(b), `"detail":{"digest":"`+missing+`"}`) {
		t.Fatalf("detail: %s", b)
	}
	resp, b = e.do("GET", "/v2/dh/app/manifests/v1", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	// schema 1
	s1 := []byte(`{"schemaVersion":1,"name":"dh/app","tag":"v1","fsLayers":[],"history":[]}`)
	resp, b = e.do("PUT", "/v2/dh/app/manifests/v1", s1, ci, hdr("Content-Type", "application/vnd.docker.distribution.manifest.v1+prettyjws"))
	e.check(resp, b, 400, CodeManifestInvalid)
	// unknown media type, bad JSON, digest mismatch, bad tag
	resp, b = e.do("PUT", "/v2/dh/app/manifests/v1", []byte(`{"schemaVersion":2,"mediaType":"application/x-what"}`), ci)
	e.check(resp, b, 415, CodeUnsupported)
	resp, b = e.do("PUT", "/v2/dh/app/manifests/v1", []byte(`{`), ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 400, CodeManifestInvalid)
	good := []byte(`{"schemaVersion":2,"mediaType":"` + mtSchema2 + `","config":{"digest":"` + cd + `","size":2},"layers":[]}`)
	resp, b = e.do("PUT", "/v2/dh/app/manifests/"+digestOf([]byte("x")), good, ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 400, CodeDigestInvalid)
	resp, b = e.do("PUT", "/v2/dh/app/manifests/-bad", good, ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 400, CodeManifestInvalid)
	resp, b = e.do("PUT", "/v2/dh/App/manifests/v1", good, ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 400, CodeNameInvalid)
	// too large
	huge := append([]byte(`{"schemaVersion":2,"pad":"`), bytes.Repeat([]byte("x"), maxManifest)...)
	resp, b = e.do("PUT", "/v2/dh/app/manifests/v1", append(huge, '"', '}'), ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 413, CodeManifestInvalid)
}

func TestMount(t *testing.T) {
	e := newEnv(t)
	layer := []byte("shared layer bytes")
	ld := digestOf(layer)
	ci := bearer(e.token("ci", "repository:dh/a:pull,push", "repository:dh/b:pull,push", "repository:priv/c:pull,push"))
	e.pushBlob("dh/a", layer, ci)
	// same repository, another image
	resp, b := e.do("POST", "/v2/dh/b/blobs/uploads/?mount="+ld+"&from=dh/a", nil, ci)
	e.check(resp, b, 201, "")
	if resp.Header.Get("Location") != "/v2/dh/b/blobs/"+ld || resp.Header.Get("Docker-Content-Digest") != ld {
		t.Fatalf("mount: %v", resp.Header)
	}
	resp, b = e.do("GET", "/v2/dh/b/blobs/"+ld, nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, layer) {
		t.Fatalf("mounted blob: %s", b)
	}
	// across repositories, when the caller may pull from the source
	resp, b = e.do("POST", "/v2/priv/c/blobs/uploads/?mount="+ld+"&from=dh/a", nil, basic("ci"))
	e.check(resp, b, 201, "")
	// the token lacks pull on priv: a normal session instead (202)
	e.pushBlob("priv/c", []byte("private layer"), ci)
	pd := digestOf([]byte("private layer"))
	tok := bearer(e.token("", "repository:dh/a:pull,push", "repository:priv/c:pull"))
	resp, b = e.do("POST", "/v2/dh/a/blobs/uploads/?mount="+pd+"&from=priv/c", nil, bearer(e.token("ci", "repository:dh/a:pull,push")))
	e.check(resp, b, 202, "")
	if resp.Header.Get("Docker-Upload-UUID") == "" {
		t.Fatalf("fallback: %v", resp.Header)
	}
	resp, b = e.do("POST", "/v2/dh/a/blobs/uploads/?mount="+pd+"&from=priv/c", nil, tok)
	e.check(resp, b, 401, CodeUnauthorized)
	// an unknown blob falls back too
	resp, b = e.do("POST", "/v2/dh/b/blobs/uploads/?mount="+digestOf([]byte("nope"))+"&from=dh/a", nil, ci)
	e.check(resp, b, 202, "")
}

func TestTagPagination(t *testing.T) {
	e := newEnv(t)
	ci := bearer(e.token("ci", "repository:dh/app:pull,push"))
	for _, tag := range []string{"v3", "v1", "v2", "latest"} {
		e.pushImage("dh/app", tag, [][]byte{[]byte("l-" + tag)}, ci)
	}
	resp, b := e.do("GET", "/v2/dh/app/tags/list?n=2", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"dh/app","tags":["latest","v1"]}` || resp.Header.Get("Link") != `</v2/dh/app/tags/list?last=v1&n=2>; rel="next"` {
		t.Fatalf("page 1: %s %q", b, resp.Header.Get("Link"))
	}
	resp, b = e.do("GET", "/v2/dh/app/tags/list?n=2&last=v1", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"dh/app","tags":["v2","v3"]}` || resp.Header.Get("Link") != "" {
		t.Fatalf("page 2: %s %q", b, resp.Header.Get("Link"))
	}
	resp, b = e.do("GET", "/v2/dh/app/tags/list?last=v3", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"dh/app","tags":[]}` {
		t.Fatalf("past the end: %s", b)
	}
	resp, b = e.do("GET", "/v2/dh/app/tags/list?n=x", nil)
	e.check(resp, b, 400, "")
}

func TestCatalog(t *testing.T) {
	e := newEnv(t)
	e.pushImage("dh/pub", "v1", nil, bearer(e.token("ci", "repository:dh/pub:pull,push")))
	e.pushImage("priv/sec", "v1", nil, basic("ci"))
	e.pushImage("dh/pub", "v2", nil, basic("ci"))
	resp, b := e.do("GET", "/v2/_catalog", nil, bearer(e.token("", "registry:catalog:*")))
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"repositories":["dh/pub"]}` {
		t.Fatalf("anonymous catalog: %s", b)
	}
	resp, b = e.do("GET", "/v2/_catalog?n=1", nil, basic("ci"))
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"repositories":["dh/pub"]}` || resp.Header.Get("Link") != `</v2/_catalog?last=dh%2Fpub&n=1>; rel="next"` {
		t.Fatalf("ci catalog: %s %q", b, resp.Header.Get("Link"))
	}
	resp, b = e.do("GET", "/v2/_catalog?n=1&last=dh/pub", nil, bearer(e.token("ci", "registry:catalog:*")))
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"repositories":["priv/sec"]}` {
		t.Fatalf("ci catalog page 2: %s", b)
	}
	// a token without the catalog scope
	resp, b = e.do("GET", "/v2/_catalog", nil, bearer(e.token("ci", "repository:dh/pub:pull")))
	e.check(resp, b, 403, CodeDenied)
	resp, b = e.do("GET", "/v2/_catalog", nil)
	e.check(resp, b, 401, CodeUnauthorized)
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasSuffix(got, `,scope="registry:catalog:*"`) {
		t.Fatalf("catalog challenge %q", got)
	}
}

func TestAllowOnce(t *testing.T) {
	e := newEnv(t)
	ci := bearer(e.token("ci", "repository:once/app:pull,push", "repository:dh/app:pull,push"))
	m1, d1 := e.pushImage("once/app", "v1", [][]byte{[]byte("one")}, ci)
	// the same manifest again is fine; another one under the tag is not
	resp, b := e.do("PUT", "/v2/once/app/manifests/v1", m1, ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 201, "")
	cd := e.pushBlob("once/app", []byte(`{"v":2}`), ci)
	m2 := []byte(`{"schemaVersion":2,"mediaType":"` + mtSchema2 + `","config":{"digest":"` + cd + `","size":7},"layers":[]}`)
	resp, b = e.do("PUT", "/v2/once/app/manifests/v1", m2, ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 409, CodeDenied)
	resp, b = e.do("GET", "/v2/once/app/manifests/v1", nil)
	if resp.Header.Get("Docker-Content-Digest") != d1 {
		t.Fatalf("tag moved: %v", resp.Header)
	}
	// the manifest itself was stored and may get another tag
	resp, b = e.do("PUT", "/v2/once/app/manifests/v2", m2, ci, hdr("Content-Type", mtSchema2))
	e.check(resp, b, 201, "")
	// writePolicy allow overwrites
	e.pushImage("dh/app", "v1", [][]byte{[]byte("one")}, ci)
	_, d := e.pushImage("dh/app", "v1", [][]byte{[]byte("two")}, ci)
	resp, _ = e.do("GET", "/v2/dh/app/manifests/v1", nil)
	if resp.Header.Get("Docker-Content-Digest") != d {
		t.Fatalf("tag not moved: %v", resp.Header)
	}
}

func TestDeleteByDigest(t *testing.T) {
	e := newEnv(t)
	ci := bearer(e.token("ci", "repository:dh/app:pull,push,delete"))
	shared, own := []byte("shared"), []byte("own")
	_, d1 := e.pushImage("dh/app", "v1", [][]byte{shared, own}, ci)
	_, d2 := e.pushImage("dh/app", "v2", [][]byte{shared}, ci)
	resp, b := e.do("DELETE", "/v2/dh/app/manifests/v1", nil, ci)
	e.check(resp, b, 400, CodeUnsupported)
	resp, b = e.do("DELETE", "/v2/dh/app/manifests/"+d1, nil, bearer(e.token("reader", "repository:dh/app:pull,push,delete")))
	e.check(resp, b, 403, CodeDenied)
	e.sink.Reset()
	resp, b = e.do("DELETE", "/v2/dh/app/manifests/"+d1, nil, ci)
	e.check(resp, b, 202, "")
	if ev := e.sink.Events(); len(ev) != 1 || ev[0].Kind != "delete" || ev[0].Subject != "app@"+d1 {
		t.Fatalf("events %v", ev)
	}
	resp, b = e.do("GET", "/v2/dh/app/manifests/"+d1, nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	resp, b = e.do("GET", "/v2/dh/app/manifests/v1", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	resp, b = e.do("GET", "/v2/dh/app/tags/list", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"dh/app","tags":["v2"]}` {
		t.Fatalf("tags: %s", b)
	}
	// v1's own layer is gone, the shared one stays with v2
	resp, b = e.do("GET", "/v2/dh/app/blobs/"+digestOf(own), nil)
	e.check(resp, b, 404, CodeBlobUnknown)
	resp, b = e.do("GET", "/v2/dh/app/blobs/"+digestOf(shared), nil)
	e.check(resp, b, 200, "")
	resp, b = e.do("DELETE", "/v2/dh/app/manifests/"+d1, nil, ci)
	e.check(resp, b, 404, CodeManifestUnknown)
	// the UI's component delete
	res, err := e.h.DeleteComponent(context.Background(), e.h.Repos.Get("dh"), component.Component{Repo: "dh", Format: "docker", Name: "app", Version: d2}, "admin")
	if err != nil || res.Assets != 1 {
		t.Fatalf("DeleteComponent: %+v %v", res, err)
	}
	resp, b = e.do("GET", "/v2/dh/app/tags/list", nil)
	e.check(resp, b, 404, CodeNameUnknown)
	resp, b = e.do("GET", "/v2/dh/app/blobs/"+digestOf(shared), nil)
	e.check(resp, b, 404, CodeBlobUnknown)
}

func TestSessionRestartAndExpiry(t *testing.T) {
	e := newEnv(t)
	layer := bytes.Repeat([]byte("abc"), 1000)
	ci := bearer(e.token("ci", "repository:dh/app:pull,push"))
	resp, b := e.do("POST", "/v2/dh/app/blobs/uploads/", nil, ci)
	e.check(resp, b, 202, "")
	loc := resp.Header.Get("Location")
	resp, b = e.do("PATCH", loc, layer[:1000], ci)
	e.check(resp, b, 202, "")
	e.open() // restart on the same data directory; the token is for the same service
	ci = bearer(e.token("ci", "repository:dh/app:pull,push"))
	resp, b = e.do("GET", loc, nil, ci)
	e.check(resp, b, 204, "")
	if resp.Header.Get("Range") != "0-999" {
		t.Fatalf("after restart: %v", resp.Header)
	}
	resp, b = e.do("PATCH", loc, layer[1000:], ci)
	e.check(resp, b, 202, "")
	resp, b = e.do("PUT", loc+"?digest="+digestOf(layer), nil, ci)
	e.check(resp, b, 201, "")
	resp, b = e.do("GET", "/v2/dh/app/blobs/"+digestOf(layer), nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, layer) {
		t.Fatal("blob differs")
	}
	// a session of another image is unknown; cancelling removes it
	resp, b = e.do("POST", "/v2/dh/app/blobs/uploads/", nil, ci)
	e.check(resp, b, 202, "")
	loc = resp.Header.Get("Location")
	resp, b = e.do("GET", "/v2/dh/other/blobs/uploads/"+resp.Header.Get("Docker-Upload-UUID"), nil, bearer(e.token("ci", "repository:dh/other:pull,push")))
	e.check(resp, b, 404, CodeBlobUploadUnknown)
	resp, b = e.do("DELETE", loc, nil, ci)
	e.check(resp, b, 204, "")
	resp, b = e.do("GET", loc, nil, ci)
	e.check(resp, b, 404, CodeBlobUploadUnknown)
	// expiry: 24 h after it started the session is unknown and its files go
	resp, b = e.do("POST", "/v2/dh/app/blobs/uploads/", nil, ci)
	e.check(resp, b, 202, "")
	loc = resp.Header.Get("Location")
	tmp := filepath.Join(e.dir, "tmp")
	files := func() int {
		n := 0
		entries, _ := os.ReadDir(tmp)
		for _, f := range entries {
			if strings.HasPrefix(f.Name(), "ses-") {
				n++
			}
		}
		return n
	}
	if files() != 2 {
		t.Fatalf("session files: %d", files())
	}
	e.now = e.now.Add(blob.SessionMaxAge + time.Minute)
	resp, b = e.do("GET", loc, nil, bearer(e.token("ci", "repository:dh/app:pull,push")))
	e.check(resp, b, 404, CodeBlobUploadUnknown)
	if files() != 0 {
		t.Fatalf("session files after expiry: %d", files())
	}
	// the sweep removes what no request touched: a day-old session, but
	// not a fresh one
	resp, b = e.do("POST", "/v2/dh/app/blobs/uploads/", nil, bearer(e.token("ci", "repository:dh/app:pull,push")))
	e.check(resp, b, 202, "")
	old := filepath.Join(tmp, "ses-"+strings.Repeat("0", 32))
	os.WriteFile(old, []byte("x"), 0o644)
	os.WriteFile(old+".json", []byte("{}"), 0o644)
	past := time.Now().Add(-blob.SessionMaxAge - time.Hour)
	os.Chtimes(old, past, past)
	os.Chtimes(old+".json", past, past)
	if n, err := e.h.Store.Blobs[repo.DefaultStore].SweepTmp(); err != nil || n != 2 || files() != 2 {
		t.Fatalf("sweep: %d %v, left %d", n, err, files())
	}
}

func TestUploadLimit(t *testing.T) {
	e := newEnv(t)
	e.h.MaxUpload = func() int64 { return 100 }
	ci := bearer(e.token("ci", "repository:dh/app:pull,push"))
	resp, b := e.do("POST", "/v2/dh/app/blobs/uploads/", nil, ci)
	e.check(resp, b, 202, "")
	loc := resp.Header.Get("Location")
	resp, b = e.do("PATCH", loc, bytes.Repeat([]byte("x"), 60), ci)
	e.check(resp, b, 202, "")
	resp, b = e.do("PATCH", loc, bytes.Repeat([]byte("x"), 60), ci)
	e.check(resp, b, 413, CodeSizeInvalid)
	resp, b = e.do("GET", loc, nil, ci)
	e.check(resp, b, 404, CodeBlobUploadUnknown)
	resp, b = e.do("POST", "/v2/dh/app/blobs/uploads/", nil, ci)
	e.check(resp, b, 202, "")
	resp, b = e.do("PATCH", resp.Header.Get("Location"), bytes.Repeat([]byte("x"), 100), ci)
	e.check(resp, b, 202, "")
}
