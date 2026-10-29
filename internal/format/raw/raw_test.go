package raw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/resolve"
	"commandcenter/internal/server"
)

type env struct {
	t     *testing.T
	ts    *httptest.Server
	h     *Handler
	sink  *repotest.Sink
	repos map[string]*repo.Repository
	hits  atomic.Int32 // upstream requests for /pub/lib.tgz
	miss  atomic.Int32 // upstream requests for paths it does not have
}

// newEnv serves raw repositories through the real server (its Basic
// authentication and body limits): files (hosted, allow_once), site (hosted,
// allow), ro (hosted, deny), mirror (proxy of a tiny web site) and all (a
// group of files then mirror, writing to files). ci may write the hosted
// ones (and would the proxy, which takes no writes); reader may write
// none; anonymous callers may read all.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pub/lib.tgz":
			e.hits.Add(1)
			w.Header().Set("Content-Type", "application/gzip")
			io.WriteString(w, "upstream-tgz")
		case "/pub/page.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<html>page</html>")
		case "/pub/":
			io.WriteString(w, "<html>Index of /pub/</html>")
		default:
			e.miss.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
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
		AnonymousRead: true,
		Users: []config.User{
			{Name: "ci", PasswordHash: string(hash), Roles: []string{"deployer"}},
			{Name: "reader", PasswordHash: string(hash)},
		},
		Roles: map[string]config.Role{"deployer": {Write: []string{"files", "site", "ro", "mirror"}}},
		Repositories: []config.Repository{
			{Name: "files", Format: "raw", Type: "hosted", WritePolicy: "allow_once"},
			{Name: "site", Format: "raw", Type: "hosted", WritePolicy: "allow"},
			{Name: "ro", Format: "raw", Type: "hosted", WritePolicy: "deny"},
			{Name: "mirror", Format: "raw", Type: "proxy", Remote: up.URL + "/",
				ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Hour)},
			{Name: "all", Format: "raw", Type: "group", Members: []string{"files", "mirror"}, WriteMember: "files"},
		},
	}
	if err := config.ValidateRepositories(cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	e.sink = &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: db, Now: time.Now, Events: e.sink}
	authn := auth.New(cfg, db)
	h := New(store, &resolve.Resolver{Store: store, Fetcher: proxy.New(store, up.Client(), 4)})
	h.Auth = authn
	registry := repo.NewRegistry(nil, cfg.Repositories)
	srv := &server.Server{Registry: registry, Handlers: map[string]repo.Handler{"raw": h}, Auth: authn,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	e.h, e.repos = h, registry.Snapshot().ByName
	return e
}

type opt func(*http.Request)

func as(user string) opt           { return func(r *http.Request) { r.SetBasicAuth(user, "pw") } }
func hdr(k, v string) opt          { return func(r *http.Request) { r.Header.Set(k, v) } }
func (e *env) url(p string) string { return e.ts.URL + "/repository/" + p }

// do sends a request through the server and returns the response with its body read.
func (e *env) do(method, p string, body string, opts ...opt) (*http.Response, string) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, e.url(p), rd)
	if err != nil {
		e.t.Fatal(err)
	}
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

// put uploads body to p as ci and expects 201.
func (e *env) put(p, body string, opts ...opt) {
	e.t.Helper()
	if resp, b := e.do("PUT", p, body, append(opts, as("ci"))...); resp.StatusCode != 201 {
		e.t.Fatalf("PUT %s: %d %s", p, resp.StatusCode, b)
	}
}

func TestHostedUploadDownload(t *testing.T) {
	e := newEnv(t)
	if resp, _ := e.do("PUT", "files/docs/a.txt", "x"); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous PUT: %d", resp.StatusCode)
	}
	if resp, _ := e.do("PUT", "files/docs/a.txt", "x", as("reader")); resp.StatusCode != 403 {
		t.Fatalf("reader PUT: %d", resp.StatusCode)
	}
	e.put("files/docs/a.txt", "hello", hdr("Content-Type", "application/x-custom"))
	e.put("files/docs/data.json", `{"k":1}`) // no Content-Type: sniffed by extension
	e.put("files/blob.bin", "\x00\x01")      // unknown extension

	resp, b := e.do("GET", "files/docs/a.txt", "")
	if resp.StatusCode != 200 || b != "hello" || resp.Header.Get("Content-Type") != "application/x-custom" {
		t.Fatalf("GET: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("ETag") == "" || resp.Header.Get("Last-Modified") == "" {
		t.Fatalf("GET headers: %v", resp.Header)
	}
	if resp, b := e.do("GET", "files/docs/data.json", ""); resp.StatusCode != 200 || b != `{"k":1}` || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET json: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	if resp, _ := e.do("GET", "files/blob.bin", ""); resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("GET bin: %q", resp.Header.Get("Content-Type"))
	}
	if resp, b := e.do("HEAD", "files/docs/a.txt", ""); resp.StatusCode != 200 || b != "" || resp.Header.Get("Content-Length") != "5" {
		t.Fatalf("HEAD: %d %q %v", resp.StatusCode, b, resp.Header)
	}
	if resp, _ := e.do("GET", "files/docs/missing.txt", ""); resp.StatusCode != 404 {
		t.Fatalf("missing: %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "files/docs", ""); resp.StatusCode != 404 { // a directory is not a file
		t.Fatalf("dir as file: %d", resp.StatusCode)
	}

	// allow_once refuses a second upload; allow takes it (ci may edit).
	if resp, b := e.do("PUT", "files/docs/a.txt", "again", as("ci")); resp.StatusCode != 400 || !strings.Contains(b, "does not allow updating") {
		t.Fatalf("redeploy: %d %s", resp.StatusCode, b)
	}
	e.put("site/a.txt", "one")
	e.put("site/a.txt", "two")
	if _, b := e.do("GET", "site/a.txt", ""); b != "two" {
		t.Fatalf("overwrite: %q", b)
	}
	if resp, _ := e.do("PUT", "ro/a.txt", "x", as("ci")); resp.StatusCode != 400 {
		t.Fatalf("deny PUT: %d", resp.StatusCode)
	}

	// DELETE needs the delete privilege.
	if resp, _ := e.do("DELETE", "files/docs/a.txt", "", as("reader")); resp.StatusCode != 403 {
		t.Fatalf("reader DELETE: %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "files/docs/a.txt", "", as("ci")); resp.StatusCode != 204 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "files/docs/a.txt", ""); resp.StatusCode != 404 {
		t.Fatalf("after DELETE: %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "files/docs/a.txt", "", as("ci")); resp.StatusCode != 404 {
		t.Fatalf("DELETE again: %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "ro/a.txt", "", as("ci")); resp.StatusCode != 400 {
		t.Fatalf("deny DELETE: %d", resp.StatusCode)
	}
	want := []repotest.Event{
		{Kind: "upload", Repo: "files", Subject: "docs/a.txt"}, {Kind: "upload", Repo: "files", Subject: "docs/data.json"},
		{Kind: "upload", Repo: "files", Subject: "blob.bin"}, {Kind: "upload", Repo: "site", Subject: "a.txt"},
		{Kind: "upload", Repo: "site", Subject: "a.txt"}, {Kind: "delete", Repo: "files", Subject: "docs/a.txt"},
	}
	got := e.sink.Events()
	if len(got) != len(want) {
		t.Fatalf("events: %v", got)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Repo != want[i].Repo || got[i].Subject != want[i].Subject {
			t.Fatalf("event %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A bad Content-Type header is not stored; the extension decides instead.
func TestInvalidContentTypeFallsBack(t *testing.T) {
	e := newEnv(t)
	e.put("files/x.txt", "t", hdr("Content-Type", "not a type"))
	if resp, _ := e.do("GET", "files/x.txt", ""); resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type %q", resp.Header.Get("Content-Type"))
	}
}

func TestWritePaths(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"files/docs/", "files/a%3Fb", "files/a%23b", "files/a%3Bb", "files/a%01b", "files/%ff"} {
		if resp, _ := e.do("PUT", p, "x", as("ci")); resp.StatusCode != 400 {
			t.Errorf("PUT %s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := e.do("DELETE", "files/docs/", "", as("ci")); resp.StatusCode != 400 {
		t.Errorf("DELETE dir: %d", resp.StatusCode)
	}
	// Traversal and aliases, which the server's mux would clean before the
	// handler sees them, are refused by the handler itself.
	for _, p := range []string{"/abs", `a\b`, "a//b", "../x", "a/./b", "a/.."} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/repository/files/x", nil)
		e.h.Serve(w, r, e.repos["files"], p)
		if w.Code != 400 {
			t.Errorf("GET %q: %d", p, w.Code)
		}
	}
	// Spaces and Unicode are fine.
	e.put("files/my%20dir/r%C3%A9sum%C3%A9.txt", "cv")
	if resp, b := e.do("GET", "files/my%20dir/r%C3%A9sum%C3%A9.txt", ""); resp.StatusCode != 200 || b != "cv" {
		t.Fatalf("unicode: %d %q", resp.StatusCode, b)
	}
}

// Overwriting an existing file (writePolicy allow) needs Edit; a new path
// only Add.
func TestOverwriteNeedsEdit(t *testing.T) {
	e := newEnv(t)
	a, ps := repotest.Principals(t, e.h.Store.DB, "site", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	do := func(p auth.Principal, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/repository/site/f.txt", strings.NewReader(body))
		e.h.Serve(w, r.WithContext(auth.WithPrincipal(r.Context(), p)), e.repos["site"], "f.txt")
		return w.Code
	}
	if c := do(ps["adder"], "a"); c != 201 {
		t.Fatalf("add-only new path: %d", c)
	}
	if c := do(ps["adder"], "b"); c != 403 {
		t.Fatalf("add-only overwrite: %d", c)
	}
	if c := do(ps["editor"], "c"); c != 201 {
		t.Fatalf("edit overwrite: %d", c)
	}
	if _, b := e.do("GET", "site/f.txt", ""); b != "c" {
		t.Fatalf("stored: %q", b)
	}
}

func TestDirectoryListing(t *testing.T) {
	e := newEnv(t)
	e.put("files/docs/a.txt", "a")
	e.put("files/docs/sub/b.txt", "b")
	e.put("files/docs/we%20ird%3Cx%3E.txt", "c")
	e.put("files/top.bin", "t")

	resp, b := e.do("GET", "files/docs/", "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("GET dir: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, s := range []string{"<title>Index of /repository/files/docs/</title>", `<a href="../">../</a>`, `<a href="a.txt">a.txt</a>`,
		`<a href="sub/">sub/</a>`, `<a href="we%20ird%3Cx%3E.txt">we ird&lt;x&gt;.txt</a>`} {
		if !strings.Contains(b, s) {
			t.Errorf("listing lacks %q:\n%s", s, b)
		}
	}
	if resp, b := e.do("GET", "files/", ""); resp.StatusCode != 200 || !strings.Contains(b, `<a href="docs/">`) || !strings.Contains(b, `<a href="top.bin">`) || strings.Contains(b, "../") {
		t.Fatalf("root listing: %d\n%s", resp.StatusCode, b)
	}
	if resp, b := e.do("HEAD", "files/docs/", ""); resp.StatusCode != 200 || b != "" {
		t.Fatalf("HEAD dir: %d %q", resp.StatusCode, b)
	}

	resp, b = e.do("GET", "files/docs/", "", hdr("Accept", "application/json"))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET dir json: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var got struct {
		Path    string
		Entries []struct {
			Name         string
			Dir          bool
			Size         int64
			LastModified string
		}
		Truncated bool
	}
	if err := json.Unmarshal([]byte(b), &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "docs/" || got.Truncated || len(got.Entries) != 3 ||
		got.Entries[0].Name != "a.txt" || got.Entries[0].Dir || got.Entries[0].Size != 1 || got.Entries[0].LastModified == "" ||
		got.Entries[1].Name != "sub" || !got.Entries[1].Dir || got.Entries[1].Size != 0 || got.Entries[2].Name != "we ird<x>.txt" {
		t.Fatalf("json listing: %s", b)
	}

	if resp, _ := e.do("GET", "files/nope/", ""); resp.StatusCode != 404 {
		t.Fatalf("missing dir: %d", resp.StatusCode)
	}
	// An empty hosted repository still lists its root; proxies and groups list nothing.
	if resp, _ := e.do("GET", "site/", ""); resp.StatusCode != 200 {
		t.Fatalf("empty root: %d", resp.StatusCode)
	}
	for _, p := range []string{"mirror/pub/", "all/docs/", "mirror/", "all/"} {
		if resp, _ := e.do("GET", p, ""); resp.StatusCode != 404 {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
	if e.miss.Load() != 0 {
		t.Fatalf("directory requests reached the remote: %d", e.miss.Load())
	}
}

func TestProxy(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 2; i++ {
		resp, b := e.do("GET", "mirror/pub/lib.tgz", "")
		if resp.StatusCode != 200 || b != "upstream-tgz" || resp.Header.Get("Content-Type") != "application/gzip" {
			t.Fatalf("GET %d: %d %q %q", i, resp.StatusCode, b, resp.Header.Get("Content-Type"))
		}
	}
	if e.hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1 (cached)", e.hits.Load())
	}
	// Web pages are content here.
	if resp, b := e.do("GET", "mirror/pub/page.html", ""); resp.StatusCode != 200 || b != "<html>page</html>" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("html: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	// A 404 is negative-cached.
	for i := 0; i < 2; i++ {
		if resp, _ := e.do("GET", "mirror/pub/nope.txt", ""); resp.StatusCode != 404 {
			t.Fatalf("missing %d: %d", i, resp.StatusCode)
		}
	}
	if e.miss.Load() != 1 {
		t.Fatalf("upstream misses %d, want 1 (negative cache)", e.miss.Load())
	}
	if resp, _ := e.do("PUT", "mirror/pub/x.txt", "x", as("ci")); resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("PUT proxy: %d Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

func TestGroup(t *testing.T) {
	e := newEnv(t)
	// Writes go to the writeMember; the first member that has the path answers.
	e.put("all/pub/lib.tgz", "hosted-tgz")
	e.put("all/docs/a.txt", "a")
	if _, b := e.do("GET", "files/pub/lib.tgz", ""); b != "hosted-tgz" {
		t.Fatalf("stored in writeMember: %q", b)
	}
	if _, b := e.do("GET", "all/pub/lib.tgz", ""); b != "hosted-tgz" {
		t.Fatalf("group prefers the first member: %q", b)
	}
	if _, b := e.do("GET", "all/pub/page.html", ""); b != "<html>page</html>" {
		t.Fatalf("group falls through to the proxy: %q", b)
	}
	if resp, _ := e.do("GET", "all/pub/nope.txt", ""); resp.StatusCode != 404 {
		t.Fatalf("group missing: %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "all/docs/a.txt", "", as("ci")); resp.StatusCode != 204 {
		t.Fatalf("group DELETE: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "all/x", "x", as("ci")); resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, HEAD, PUT, DELETE" {
		t.Fatalf("POST: %d Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

func TestDeleteComponentAndCleanup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put("files/docs/old.txt", "old")
	e.put("files/docs/used.txt", "used")
	e.put("files/new.txt", "new")
	db := e.h.Store.DB
	now := time.Now()
	for p, age := range map[string]time.Duration{"docs/old.txt": 90 * 24 * time.Hour, "docs/used.txt": 24 * time.Hour} {
		if err := db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "files", Path: p, N: 1, Last: now.Add(-age)}}); err != nil {
			t.Fatal(err)
		}
	}
	// Every file is a component named by its path, without a version;
	// one downloaded long ago is selected by last download, one downloaded
	// yesterday and one just stored are not.
	ev := &cleanup.Evaluator{DB: db, Now: func() time.Time { return now }}
	cands, _, _, _, err := ev.Evaluate(ctx, e.repos["files"], cleanup.Policy{Name: "p", Format: "raw", Criteria: cleanup.Criteria{LastDownloadedDays: 30}}, cleanup.DefaultScanBudget)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Name != "docs/old.txt" || cands[0].Version != "" || cands[0].Label() != "docs/old.txt" || len(cands[0].Assets) != 1 {
		t.Fatalf("candidates: %+v", cands)
	}
	// A download after the evaluation skips the delete.
	if err := db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "files", Path: "docs/old.txt", N: 1, Last: now}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["files"], cands[0].Component, "system"); !errors.Is(err, cleanup.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	e.sink.Reset()
	res, err := e.h.DeleteComponent(ctx, e.repos["files"], component.Component{Format: "raw", Name: "docs/old.txt"}, "system")
	if err != nil || res.Assets != 1 || res.Bytes != 3 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if resp, _ := e.do("GET", "files/docs/old.txt", ""); resp.StatusCode != 404 {
		t.Fatalf("after delete: %d", resp.StatusCode)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Kind != "delete" || got[0].Subject != "docs/old.txt" || got[0].Actor != "system" {
		t.Fatalf("events: %v", got)
	}
	res, err = e.h.DeleteComponent(ctx, e.repos["files"], component.Component{Format: "raw", Name: "docs/old.txt"}, "system")
	if err != nil || res.Assets != 0 {
		t.Fatalf("delete again: %+v %v", res, err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["files"], component.Component{Format: "raw", Name: "x", Version: "1"}, "system"); err == nil {
		t.Fatal("versioned raw component accepted")
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["mirror"], component.Component{Format: "raw", Name: "x"}, "system"); !errors.Is(err, component.ErrNotSupported) {
		t.Fatalf("proxy: %v", err)
	}
}
