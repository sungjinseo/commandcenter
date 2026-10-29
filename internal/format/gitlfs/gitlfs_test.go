package gitlfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
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
	authn *auth.Authenticator
	sink  *repotest.Sink
	repos map[string]*repo.Repository
}

// newEnv serves two gitlfs repositories through the real server (its Basic
// and Bearer authentication and body limits): lfs (hosted, allow_once) and
// ro (hosted, deny). ci may write both; reader has no privilege; anonymous
// callers read when anonRead is set.
func newEnv(t *testing.T, anonRead bool) *env {
	t.Helper()
	e := &env{t: t}
	dir := t.TempDir()
	bs, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := &config.Config{
		AnonymousRead: anonRead,
		Users: []config.User{
			{Name: "ci", PasswordHash: string(hash), Roles: []string{"deployer"}},
			{Name: "reader", PasswordHash: string(hash)},
		},
		Roles: map[string]config.Role{"deployer": {Write: []string{"lfs", "ro"}}},
		Repositories: []config.Repository{
			{Name: "lfs", Format: "gitlfs", Type: "hosted", WritePolicy: "allow_once"},
			{Name: "ro", Format: "gitlfs", Type: "hosted", WritePolicy: "deny"},
		},
	}
	if err := config.ValidateRepositories(cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	e.sink = &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: db, Now: time.Now, Events: e.sink}
	e.authn = auth.New(cfg, db)
	registry := repo.NewRegistry(nil, cfg.Repositories)
	e.h = New(store, e.authn, func() string { return e.ts.URL })
	srv := &server.Server{Registry: registry, Handlers: map[string]repo.Handler{"gitlfs": e.h}, Auth: e.authn,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	e.repos = registry.Snapshot().ByName
	return e
}

type opt func(*http.Request)

func as(user string) opt  { return func(r *http.Request) { r.SetBasicAuth(user, "pw") } }
func hdr(k, v string) opt { return func(r *http.Request) { r.Header.Set(k, v) } }

func (e *env) do(method, p string, body string, opts ...opt) (*http.Response, string) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, e.ts.URL+"/repository/"+p, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	r.Header.Set("Accept", MediaType)
	for _, o := range opts {
		o(r)
	}
	resp, err := e.ts.Client().Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// batch posts a batch request and decodes the answer.
func (e *env) batch(rp, operation string, objects []object, opts ...opt) (*http.Response, map[string]any) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"operation": operation, "transfers": []string{"basic"}, "ref": map[string]string{"name": "refs/heads/main"}, "objects": objects, "hash_algo": "sha256"})
	resp, b := e.do("POST", rp+"/info/lfs/objects/batch", string(body), append(opts, hdr("Content-Type", MediaType))...)
	var out map[string]any
	if resp.StatusCode == 200 {
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			e.t.Fatalf("batch answer %q: %v", b, err)
		}
	}
	return resp, out
}

func oidOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func objectsOf(out map[string]any) []map[string]any {
	var objs []map[string]any
	for _, o := range out["objects"].([]any) {
		objs = append(objs, o.(map[string]any))
	}
	return objs
}

func actionHref(o map[string]any, name string) string {
	acts, _ := o["actions"].(map[string]any)
	a, _ := acts[name].(map[string]any)
	href, _ := a["href"].(string)
	return href
}

func TestBatchAndBasicTransfer(t *testing.T) {
	e := newEnv(t, true)
	a, b := "hello lfs", "another object"
	oa, ob := oidOf(a), oidOf(b)
	objs := []object{{OID: oa, Size: int64(len(a))}, {OID: ob, Size: int64(len(b))}}

	// Upload batch: neither object is stored, both get upload and verify actions.
	resp, out := e.batch("lfs", "upload", objs, as("ci"))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != MediaType || out["transfer"] != "basic" {
		t.Fatalf("upload batch: %d %v %v", resp.StatusCode, resp.Header, out)
	}
	for i, o := range objectsOf(out) {
		want := e.ts.URL + "/repository/lfs/objects/" + objs[i].OID
		if actionHref(o, "upload") != want || actionHref(o, "verify") != e.ts.URL+"/repository/lfs/objects/verify" || o["authenticated"] != true || o["size"] != float64(objs[i].Size) {
			t.Fatalf("upload object %d: %v", i, o)
		}
	}

	// PUT: the content must hash to the oid.
	if resp, body := e.do("PUT", "lfs/objects/"+oa, "not the content", as("ci")); resp.StatusCode != 422 || !strings.Contains(body, "sha256") {
		t.Fatalf("sha mismatch: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do("PUT", "lfs/objects/"+oa, a, as("ci"), hdr("Content-Type", "application/octet-stream")); resp.StatusCode != 200 {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Kind != "upload" || got[0].Subject != oa || got[0].Actor != "ci" {
		t.Fatalf("events: %v", got)
	}
	// A re-upload of the same object is idempotent under allow_once.
	if resp, _ := e.do("PUT", "lfs/objects/"+oa, a, as("ci")); resp.StatusCode != 200 {
		t.Fatalf("re-PUT: %d", resp.StatusCode)
	}
	// Verify: the stored size must match.
	verify := func(oid string, size int) int {
		resp, _ := e.do("POST", "lfs/objects/verify", fmt.Sprintf(`{"oid":%q,"size":%d}`, oid, size), as("ci"))
		return resp.StatusCode
	}
	if verify(oa, len(a)) != 200 || verify(oa, len(a)+1) != 422 || verify(ob, len(b)) != 422 {
		t.Fatalf("verify: %d %d %d", verify(oa, len(a)), verify(oa, len(a)+1), verify(ob, len(b)))
	}
	// Upload batch again: the stored object has no actions, the other still does.
	_, out = e.batch("lfs", "upload", objs, as("ci"))
	if got := objectsOf(out); got[0]["actions"] != nil || actionHref(got[1], "upload") == "" {
		t.Fatalf("second upload batch: %v", got)
	}

	// Download batch (anonymous read): present with a download action, missing with a 404 error.
	resp, out = e.batch("lfs", "download", objs)
	if resp.StatusCode != 200 {
		t.Fatalf("download batch: %d", resp.StatusCode)
	}
	got := objectsOf(out)
	if actionHref(got[0], "download") != e.ts.URL+"/repository/lfs/objects/"+oa || got[0]["size"] != float64(len(a)) || got[0]["error"] != nil {
		t.Fatalf("present: %v", got[0])
	}
	if got[1]["actions"] != nil || got[1]["error"].(map[string]any)["code"] != float64(404) {
		t.Fatalf("missing: %v", got[1])
	}

	// GET/HEAD/Range of the object.
	resp, body := e.do("GET", "lfs/objects/"+oa, "")
	if resp.StatusCode != 200 || body != a || resp.Header.Get("ETag") == "" || resp.Header.Get("Content-Length") != fmt.Sprint(len(a)) || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("GET: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	etag := resp.Header.Get("ETag")
	if resp, body := e.do("HEAD", "lfs/objects/"+oa, ""); resp.StatusCode != 200 || body != "" || resp.Header.Get("Content-Length") != fmt.Sprint(len(a)) || resp.Header.Get("ETag") != etag {
		t.Fatalf("HEAD: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, body := e.do("GET", "lfs/objects/"+oa, "", hdr("Range", "bytes=0-4")); resp.StatusCode != 206 || body != "hello" {
		t.Fatalf("Range: %d %q", resp.StatusCode, body)
	}
	if resp, _ := e.do("GET", "lfs/objects/"+oa, "", hdr("If-None-Match", etag)); resp.StatusCode != 304 {
		t.Fatalf("If-None-Match: %d", resp.StatusCode)
	}
	if resp, body := e.do("GET", "lfs/objects/"+ob, ""); resp.StatusCode != 404 || !strings.Contains(body, `"message"`) {
		t.Fatalf("missing GET: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do("GET", "lfs/objects/nothex", ""); resp.StatusCode != 404 {
		t.Fatalf("bad oid: %d", resp.StatusCode)
	}

	// lfs.url without the info/lfs suffix reaches the same batch endpoint.
	if resp, _ := e.do("POST", "lfs/objects/batch", `{"operation":"download","objects":[]}`); resp.StatusCode != 200 {
		t.Fatalf("short batch path: %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "lfs/info/lfs/objects/batch", ""); resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET batch: %d %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	for _, body := range []string{`{"operation":"delete","objects":[]}`, `{"operation":"download","transfers":["ssh"],"objects":[]}`, `{"operation":"download","hash_algo":"sha1","objects":[]}`} {
		if resp, _ := e.do("POST", "lfs/info/lfs/objects/batch", body); resp.StatusCode != 422 {
			t.Fatalf("batch %s: %d", body, resp.StatusCode)
		}
	}
	if resp, _ := e.do("POST", "lfs/info/lfs/objects/batch", `{"operation":"download","objects":[{"oid":"zz","size":1}]}`); resp.StatusCode != 200 {
		t.Fatalf("bad oid in batch: %d", resp.StatusCode)
	} else if _, out := e.batch("lfs", "download", []object{{OID: "zz", Size: 1}}); objectsOf(out)[0]["error"].(map[string]any)["code"] != float64(422) {
		t.Fatalf("bad oid in batch: %v", out)
	}

	// Locks are not supported: 501, which git-lfs tolerates.
	for _, p := range []string{"lfs/info/lfs/locks", "lfs/info/lfs/locks/verify", "lfs/locks/1/unlock"} {
		if resp, body := e.do("POST", p, `{}`, as("ci")); resp.StatusCode != 501 || !strings.Contains(body, "locks are not supported") {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t, false)
	objs := []object{{OID: oidOf("x"), Size: 1}}
	resp, _ := e.batch("lfs", "download", objs)
	if resp.StatusCode != 401 || resp.Header.Get("LFS-Authenticate") != `Basic realm="commandcenter"` || resp.Header.Get("WWW-Authenticate") == "" || resp.Header.Get("Content-Type") != MediaType {
		t.Fatalf("anonymous batch: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := e.batch("lfs", "download", objs, as("reader")); resp.StatusCode != 200 { // authenticatedRead (default on)
		t.Fatalf("reader signed in: %d", resp.StatusCode)
	}
	if resp, _ := e.batch("lfs", "download", objs, as("ci")); resp.StatusCode != 200 {
		t.Fatalf("ci basic: %d", resp.StatusCode)
	}
	if resp, _ := e.batch("lfs", "download", objs, func(r *http.Request) { r.SetBasicAuth("ci", "wrong") }); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" { // the server's answer
		t.Fatalf("bad password: %d %v", resp.StatusCode, resp.Header)
	}
	_, token, err := e.authn.CreateToken(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.batch("lfs", "upload", objs, func(r *http.Request) { r.SetBasicAuth("ci", token) }); resp.StatusCode != 200 {
		t.Fatalf("token as password: %d", resp.StatusCode)
	}
	if resp, _ := e.batch("lfs", "upload", objs, hdr("Authorization", "Bearer "+token)); resp.StatusCode != 200 {
		t.Fatalf("bearer token: %d", resp.StatusCode)
	}
	// The transfer endpoints are checked by the server (WWW-Authenticate;
	// git-lfs reads either header).
	if resp, _ := e.do("GET", "lfs/objects/"+oidOf("x"), ""); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous GET: %d", resp.StatusCode)
	}
}

func TestPrivileges(t *testing.T) {
	e := newEnv(t, true)
	objs := []object{{OID: oidOf("x"), Size: 1}}
	// Download is a read: anonymous and reader may. Upload needs add: the
	// anonymous caller is challenged, reader refused.
	if resp, _ := e.batch("lfs", "download", objs); resp.StatusCode != 200 {
		t.Fatalf("anonymous download: %d", resp.StatusCode)
	}
	if resp, _ := e.batch("lfs", "download", objs, as("reader")); resp.StatusCode != 200 {
		t.Fatalf("reader download: %d", resp.StatusCode)
	}
	resp, _ := e.batch("lfs", "upload", objs)
	if resp.StatusCode != 401 || resp.Header.Get("LFS-Authenticate") == "" || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous upload: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, body := e.batch("lfs", "upload", objs, as("reader")); resp.StatusCode != 403 {
		t.Fatalf("reader upload: %d %v", resp.StatusCode, body)
	}
	if resp, _ := e.do("PUT", "lfs/objects/"+oidOf("x"), "x"); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous PUT: %d", resp.StatusCode)
	}
	if resp, _ := e.do("PUT", "lfs/objects/"+oidOf("x"), "x", as("reader")); resp.StatusCode != 403 {
		t.Fatalf("reader PUT: %d", resp.StatusCode)
	}
	if resp, _ := e.do("PUT", "lfs/objects/"+oidOf("x"), "x", as("ci")); resp.StatusCode != 200 {
		t.Fatalf("ci PUT: %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "lfs/objects/"+oidOf("x"), "", as("reader")); resp.StatusCode != 403 {
		t.Fatalf("reader DELETE: %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "lfs/objects/"+oidOf("x"), "", as("ci")); resp.StatusCode != 204 {
		t.Fatalf("ci DELETE: %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "lfs/objects/"+oidOf("x"), ""); resp.StatusCode != 404 {
		t.Fatalf("after DELETE: %d", resp.StatusCode)
	}
}

func TestWritePolicyDeny(t *testing.T) {
	e := newEnv(t, true)
	objs := []object{{OID: oidOf("x"), Size: 1}}
	if resp, body := e.batch("ro", "upload", objs, as("ci")); resp.StatusCode != 400 {
		t.Fatalf("deny upload batch: %d %v", resp.StatusCode, body)
	}
	if resp, body := e.do("PUT", "ro/objects/"+oidOf("x"), "x", as("ci")); resp.StatusCode != 400 || !strings.Contains(body, "read-only") {
		t.Fatalf("deny PUT: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.batch("ro", "download", objs); resp.StatusCode != 200 {
		t.Fatalf("deny download batch: %d", resp.StatusCode)
	}
}

func TestDeleteComponentAndCleanup(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	old, used, fresh := "old object", "used object", "new object"
	for _, body := range []string{old, used, fresh} {
		if resp, _ := e.do("PUT", "lfs/objects/"+oidOf(body), body, as("ci")); resp.StatusCode != 200 {
			t.Fatalf("PUT: %d", resp.StatusCode)
		}
	}
	db := e.h.Store.DB
	now := time.Now()
	for body, age := range map[string]time.Duration{old: 90 * 24 * time.Hour, used: 24 * time.Hour} {
		if err := db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "lfs", Path: ObjectPath(oidOf(body)), N: 1, Last: now.Add(-age)}}); err != nil {
			t.Fatal(err)
		}
	}
	ev := &cleanup.Evaluator{DB: db, Now: func() time.Time { return now }}
	cands, _, _, _, err := ev.Evaluate(ctx, e.repos["lfs"], cleanup.Policy{Name: "p", Format: "gitlfs", Criteria: cleanup.Criteria{LastDownloadedDays: 30}}, cleanup.DefaultScanBudget)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Name != oidOf(old) || cands[0].Version != "" || cands[0].Label() != oidOf(old) || len(cands[0].Assets) != 1 {
		t.Fatalf("candidates: %+v", cands)
	}
	if err := db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "lfs", Path: ObjectPath(oidOf(old)), N: 1, Last: now}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["lfs"], cands[0].Component, "system"); !errors.Is(err, cleanup.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	e.sink.Reset()
	res, err := e.h.DeleteComponent(ctx, e.repos["lfs"], component.Component{Format: "gitlfs", Name: oidOf(old)}, "system")
	if err != nil || res.Assets != 1 || res.Bytes != int64(len(old)) {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if resp, _ := e.do("GET", "lfs/objects/"+oidOf(old), ""); resp.StatusCode != 404 {
		t.Fatalf("after delete: %d", resp.StatusCode)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Kind != "delete" || got[0].Subject != oidOf(old) || got[0].Actor != "system" {
		t.Fatalf("events: %v", got)
	}
	if res, err := e.h.DeleteComponent(ctx, e.repos["lfs"], component.Component{Format: "gitlfs", Name: oidOf(old)}, "system"); err != nil || res.Assets != 0 {
		t.Fatalf("delete again: %+v %v", res, err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["lfs"], component.Component{Format: "gitlfs", Name: oidOf(old), Version: "1"}, "system"); err == nil {
		t.Fatal("versioned component accepted")
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["lfs"], component.Component{Format: "gitlfs", Name: "not-an-oid"}, "system"); err == nil {
		t.Fatal("non-oid name accepted")
	}
	// Cleanup ignores writePolicy: a read-only repository is still cleaned.
	if res, err := e.h.DeleteComponent(ctx, e.repos["ro"], component.Component{Format: "gitlfs", Name: oidOf(old)}, "system"); err != nil || res.Assets != 0 {
		t.Fatalf("deny repo: %+v %v", res, err)
	}
}

func TestConfigRefusesProxyAndGroup(t *testing.T) {
	defs := []config.Repository{
		{Name: "h", Format: "gitlfs", Type: "hosted"},
		{Name: "p", Format: "gitlfs", Type: "proxy", Remote: "https://github.example/"},
		{Name: "g", Format: "gitlfs", Type: "group", Members: []string{"h"}},
	}
	err := config.ValidateRepositories(defs)
	if err == nil || !strings.Contains(err.Error(), `repository "p": gitlfs supports hosted repositories only`) || !strings.Contains(err.Error(), `repository "g": gitlfs supports hosted repositories only`) {
		t.Fatalf("validation: %v", err)
	}
	if err := config.ValidateRepositories(defs[:1]); err != nil {
		t.Fatal(err)
	}
	if _, name, version, ok := component.DeriveAsset("gitlfs", ObjectPath(oidOf("x"))); !ok || name != oidOf("x") || version != "" {
		t.Fatalf("DeriveAsset: %q %q %v", name, version, ok)
	}
	if _, _, _, ok := component.DeriveAsset("gitlfs", "objects/zz/zz/"+oidOf("x")); ok {
		t.Fatal("misplaced object derived")
	}
}
