package huggingface

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
	"commandcenter/internal/server"
)

const (
	sha1st  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha2nd  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	body1   = "model-weights-version-one"
	body2   = "model-weights-version-two!"
	cfgJSON = `{"model_type":"bert"}`
)

// The hub's X-Linked-Etag of an LFS file is its sha256, which the proxy
// replays; the CDN's own ETag differs.
var etag1, etag2 = quotedSHA(body1), quotedSHA(body2)

func quotedSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

type env struct {
	t        *testing.T
	ts       *httptest.Server
	h        *Handler
	store    *repo.Store
	db       *meta.DB
	sink     *repotest.Sink
	registry *repo.Registry
	now      atomic.Int64
	main     atomic.Value // the sha main points at
	mu       sync.Mutex
	hubAuth  []string // Authorization values the hub saw
	cdnAuth  []string // Authorization values the CDN saw
	hubHits  map[string]int
	cdnHits  atomic.Int32
}

// newEnv serves the proxy hf (remoteAuth bearer "tok") of a fake hub and
// its fake CDN through the real server. The model org/model has
// model.bin (an LFS file: the hub redirects to the CDN) and config.json
// (served by the hub itself) at commit sha1st, which main points at until
// the test moves it to sha2nd; org/private answers 401 without the token.
// The dataset org/ds has data.csv.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, hubHits: map[string]int{}}
	e.main.Store(sha1st)
	files := map[string]string{ // the hub's own ETag of each is a git oid, "c0ffee" here
		sha1st + "/model.bin": body1, sha2nd + "/model.bin": body2,
		sha1st + "/config.json": cfgJSON, "ds/" + sha1st + "/data.csv": "a,b\n1,2\n",
	}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.cdnHits.Add(1)
		e.mu.Lock()
		e.cdnAuth = append(e.cdnAuth, r.Header.Get("Authorization"))
		e.mu.Unlock()
		f, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"cdn-etag"`)
		w.Header().Set("Content-Type", "binary/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(f))
	}))
	t.Cleanup(cdn.Close)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hubAuth = append(e.hubAuth, r.Header.Get("Authorization"))
		e.hubHits[r.Method+" "+r.URL.RequestURI()]++
		e.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/")
		main := e.main.Load().(string)
		switch {
		case p == "api/models/org/private/revision/main" || strings.HasPrefix(p, "org/private/resolve/"):
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"sha":"`+sha1st+`","private":true}`)
		case p == "api/models/org/model/revision/main" || p == "api/models/org/model":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"sha":"`+main+`","pipeline_tag":"text-classification","library_name":"transformers","tags":["license:apache-2.0"],"siblings":[{"rfilename":"config.json"},{"rfilename":"model.bin"}]}`)
		case p == "api/datasets/org/ds/revision/main":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"sha":"`+sha1st+`"}`)
		case p == "api/models/org/model/tree/main/100% done#.txt" && r.URL.RawQuery == "recursive=true":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `[{"path":"100% done#.txt"}]`)
		case p == "api/models/org/model/revision/v1":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"sha":"not-a-sha"}`)
		case strings.HasPrefix(p, "org/model/resolve/") || strings.HasPrefix(p, "datasets/org/ds/resolve/"):
			rest := strings.TrimPrefix(strings.TrimPrefix(p, "datasets/org/ds/resolve/"), "org/model/resolve/")
			if strings.HasPrefix(p, "datasets/") {
				rest = "ds/" + rest
			}
			rev, _, _ := strings.Cut(strings.TrimPrefix(rest, "ds/"), "/")
			if !IsSHA(rev) { // the proxy never asks for a branch
				w.WriteHeader(500)
				return
			}
			f, ok := files[rest]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("X-Repo-Commit", rev)
			w.Header().Set("ETag", `"c0ffee"`)
			if strings.HasSuffix(p, "config.json") { // not LFS: served by the hub
				w.Header().Set("Content-Type", "application/json")
				http.ServeContent(w, r, "", time.Time{}, strings.NewReader(f))
				return
			}
			w.Header().Set("X-Linked-Etag", quotedSHA(f))
			w.Header().Set("X-Linked-Size", itoa(len(f)))
			http.Redirect(w, r, cdn.URL+"/"+rest, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
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
		Users:         []config.User{{Name: "dev", PasswordHash: string(hash)}},
		Repositories: []config.Repository{
			{Name: "hf", Format: "huggingface", Type: "proxy", Remote: hub.URL + "/", RemoteAuth: &config.RemoteAuth{BearerToken: "tok"},
				MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Hour)},
		},
	}
	if err := config.ValidateRepositories(cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	e.sink = &repotest.Sink{}
	e.now.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(e.now.Load(), 0) }
	e.store = &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: db, Now: now, Events: e.sink}
	e.db = db
	f := proxy.New(e.store, proxy.NewClient(), 4)
	f.Now = now
	e.h = New(e.store, f)
	e.registry = repo.NewRegistry(nil, cfg.Repositories)
	srv := &server.Server{Registry: e.registry, Handlers: map[string]repo.Handler{"huggingface": e.h}, Auth: auth.New(cfg, db),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func itoa(n int) string { return strconv.Itoa(n) }

type opt func(*http.Request)

func hdr(k, v string) opt { return func(r *http.Request) { r.Header.Set(k, v) } }

// asDev sends the configured user's Basic credentials: the client's own
// Authorization, which must never reach the hub.
func asDev() opt { return func(r *http.Request) { r.SetBasicAuth("dev", "pw") } }

func (e *env) do(method, p string, opts ...opt) (*http.Response, string) {
	e.t.Helper()
	r, _ := http.NewRequest(method, e.ts.URL+"/repository/hf/"+p, nil)
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

func (e *env) hits(key string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hubHits[key]
}

func wantHeaders(t *testing.T, resp *http.Response, sha, etag string, size int) {
	t.Helper()
	h := resp.Header
	if h.Get("X-Repo-Commit") != sha || h.Get("X-Linked-Etag") != etag || h.Get("ETag") != etag || h.Get("X-Linked-Size") != itoa(size) || h.Get("Content-Length") != itoa(size) {
		t.Fatalf("headers: %v", h)
	}
}

func TestHeadThenGetFollowsRedirectAndCaches(t *testing.T) {
	e := newEnv(t)
	// HEAD before anything is cached: answered from the hub's redirect
	// headers, the CDN untouched, the file not downloaded.
	resp, _ := e.do("HEAD", "org/model/resolve/main/model.bin", asDev())
	if resp.StatusCode != 200 {
		t.Fatalf("HEAD: %d", resp.StatusCode)
	}
	wantHeaders(t, resp, sha1st, etag1, len(body1))
	if e.cdnHits.Load() != 0 || e.hits("HEAD /org/model/resolve/"+sha1st+"/model.bin") != 1 || e.hits("GET /api/models/org/model/revision/main") != 1 {
		t.Fatalf("upstream: cdn=%d hub=%v", e.cdnHits.Load(), e.hubHits)
	}
	// GET downloads through the redirect, caches under the commit, replays the headers.
	resp, b := e.do("GET", "org/model/resolve/main/model.bin")
	if resp.StatusCode != 200 || b != body1 || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("GET: %d %q", resp.StatusCode, b)
	}
	wantHeaders(t, resp, sha1st, etag1, len(body1))
	if _, err := e.store.Get(context.Background(), "hf", "org/model/resolve/"+sha1st+"/model.bin"); err != nil {
		t.Fatalf("not cached under the commit: %v", err)
	}
	// Cached: by sha and by branch alike, no upstream request; HEAD too.
	for _, p := range []string{"org/model/resolve/" + sha1st + "/model.bin", "org/model/resolve/main/model.bin"} {
		resp, b := e.do("GET", p)
		if b != body1 {
			t.Fatalf("%s: %q", p, b)
		}
		wantHeaders(t, resp, sha1st, etag1, len(body1))
		resp, b = e.do("HEAD", p)
		if resp.StatusCode != 200 || b != "" {
			t.Fatalf("HEAD %s: %d %q", p, resp.StatusCode, b)
		}
		wantHeaders(t, resp, sha1st, etag1, len(body1))
	}
	if e.cdnHits.Load() != 1 || e.hits("GET /org/model/resolve/"+sha1st+"/model.bin") != 1 {
		t.Fatalf("refetched: cdn=%d hub=%v", e.cdnHits.Load(), e.hubHits)
	}
	// Byte ranges for resumed downloads; If-None-Match with the hub's ETag.
	resp, b = e.do("GET", "org/model/resolve/main/model.bin", hdr("Range", "bytes=6-12"))
	if resp.StatusCode != 206 || b != body1[6:13] || resp.Header.Get("Content-Range") != "bytes 6-12/"+itoa(len(body1)) {
		t.Fatalf("range: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Range"))
	}
	if resp, _ := e.do("GET", "org/model/resolve/main/model.bin", hdr("If-None-Match", etag1)); resp.StatusCode != 304 {
		t.Fatalf("conditional: %d", resp.StatusCode)
	}
	// A file the hub serves itself (not LFS): a HEAD fetches it (it is
	// small), so its ETag is the same before and after it is cached.
	cfg := "org/model/resolve/" + sha1st + "/config.json"
	resp, _ = e.do("HEAD", "org/model/resolve/main/config.json")
	wantHeaders(t, resp, sha1st, quotedSHA(cfgJSON), len(cfgJSON))
	resp, b = e.do("GET", "org/model/resolve/main/config.json")
	if resp.StatusCode != 200 || b != cfgJSON {
		t.Fatalf("config.json: %d %q %v", resp.StatusCode, b, resp.Header)
	}
	wantHeaders(t, resp, sha1st, quotedSHA(cfgJSON), len(cfgJSON))
	if e.hits("HEAD /"+cfg) != 1 || e.hits("GET /"+cfg) != 1 {
		t.Fatalf("config.json upstream: %v", e.hubHits)
	}
	resp, b = e.do("GET", "datasets/org/ds/resolve/main/data.csv")
	if resp.StatusCode != 200 || b != "a,b\n1,2\n" {
		t.Fatalf("dataset: %d %q", resp.StatusCode, b)
	}
	if _, err := e.store.Get(context.Background(), "hf", "datasets/org/ds/resolve/"+sha1st+"/data.csv"); err != nil {
		t.Fatal("dataset file not cached under datasets/")
	}
	// The API documents pass through.
	resp, b = e.do("GET", "api/models/org/model")
	if resp.StatusCode != 200 || !strings.Contains(b, `"pipeline_tag"`) || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("api: %d %q", resp.StatusCode, b)
	}
	// A query selects another answer: cached apart, sent to the hub as is.
	e.do("GET", "api/models/org/model?blobs=true&expand[]=xetEnabled")
	e.do("GET", "api/models/org/model?blobs=true&expand[]=xetEnabled")
	if e.hits("GET /api/models/org/model?blobs=true&expand[]=xetEnabled") != 1 || e.hits("GET /api/models/org/model") != 1 {
		t.Fatalf("api query: %v", e.hubHits)
	}
	// The query goes with the path escaped as the client sent it.
	tree := "api/models/org/model/tree/main/100%25%20done%23.txt?recursive=true"
	if resp, b := e.do("GET", tree); resp.StatusCode != 200 || !strings.Contains(b, "100% done#.txt") || e.hits("GET /"+tree) != 1 {
		t.Fatalf("tree with a query: %d %q %v", resp.StatusCode, b, e.hubHits)
	}
	// The rest of the API (the token's account, listings) is not served.
	for _, p := range []string{"api/whoami-v2", "api/models", "api/models/", "api/settings/tokens"} {
		if resp, _ := e.do("GET", p); resp.StatusCode != 404 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	if e.hits("GET /api/whoami-v2") != 0 || e.hits("GET /api/models") != 0 {
		t.Fatalf("api outside a repository reached the hub: %v", e.hubHits)
	}
	// Not found, bad routes, methods.
	for _, p := range []string{"org/model/resolve/main/missing.bin", "org/model/resolve/" + sha2nd + "/nothing", "org/model/resolve/main/", "org/model/resolve/main/../x",
		"a/b/c/resolve/main/f", "org/model/blob/main/f", "api/../x", "org/model/resolve/v1/config.json", "org/nope/resolve/main/f"} {
		if resp, _ := e.do("GET", p); resp.StatusCode != 404 && resp.StatusCode != 502 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	w := httptest.NewRecorder()
	e.h.Serve(w, httptest.NewRequest("PUT", "/x", nil), e.registry.Get("hf"), "org/model/resolve/main/model.bin")
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("PUT: %d", w.Code)
	}
}

func TestBranchRevalidatedCommitImmutable(t *testing.T) {
	e := newEnv(t)
	e.do("GET", "org/model/resolve/main/model.bin")
	e.main.Store(sha2nd)
	// Within metadataMaxAge the branch still resolves to the cached commit.
	if _, b := e.do("GET", "org/model/resolve/main/model.bin"); b != body1 {
		t.Fatalf("fresh branch: %q", b)
	}
	e.now.Add(7200)
	resp, b := e.do("GET", "org/model/resolve/main/model.bin")
	if b != body2 || resp.Header.Get("X-Repo-Commit") != sha2nd || resp.Header.Get("ETag") != etag2 {
		t.Fatalf("moved branch: %q %v", b, resp.Header)
	}
	if e.hits("GET /api/models/org/model/revision/main") != 2 || e.cdnHits.Load() != 2 {
		t.Fatalf("hub=%v cdn=%d", e.hubHits, e.cdnHits.Load())
	}
	// The old commit's file is still cached and never fetched again.
	e.now.Add(365 * 86400)
	if resp, b := e.do("GET", "org/model/resolve/"+sha1st+"/model.bin"); b != body1 || resp.Header.Get("X-Repo-Commit") != sha1st {
		t.Fatalf("old commit: %q", b)
	}
	if e.cdnHits.Load() != 2 {
		t.Fatal("immutable commit refetched")
	}
}

func TestTokenGoesToTheHubOnly(t *testing.T) {
	e := newEnv(t)
	e.do("HEAD", "org/model/resolve/main/model.bin", asDev())
	e.do("GET", "org/model/resolve/main/model.bin", asDev())
	e.do("GET", "org/model/resolve/main/config.json", asDev())
	if resp, b := e.do("GET", "org/private/resolve/main/config.json"); resp.StatusCode != 200 || !strings.Contains(b, "private") {
		t.Fatalf("gated with the token: %d %s", resp.StatusCode, b)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.hubAuth) < 5 || len(e.cdnAuth) != 1 {
		t.Fatalf("requests: hub=%v cdn=%v", e.hubAuth, e.cdnAuth)
	}
	for _, a := range e.hubAuth {
		if a != "Bearer tok" {
			t.Fatalf("hub saw %q", a)
		}
	}
	if e.cdnAuth[0] != "" {
		t.Fatalf("CDN saw %q", e.cdnAuth[0])
	}
}

func TestProxyOnly(t *testing.T) {
	for _, def := range []config.Repository{
		{Name: "h", Format: "huggingface", Type: "hosted"},
		{Name: "g", Format: "huggingface", Type: "group", Members: []string{"p"}},
	} {
		defs := []config.Repository{def, {Name: "p", Format: "huggingface", Type: "proxy", Remote: "https://huggingface.co/"}}
		err := config.ValidateRepositories(defs)
		if err == nil || !strings.Contains(err.Error(), `repository "`+def.Name+`": huggingface supports proxy repositories only`) {
			t.Fatalf("%s: %v", def.Type, err)
		}
	}
	e := newEnv(t)
	w := httptest.NewRecorder()
	e.h.Serve(w, httptest.NewRequest("GET", "/x", nil), &repo.Repository{Repository: config.Repository{Name: "x", Format: "huggingface", Type: "hosted"}}, "api/models/x")
	if w.Code != 400 {
		t.Fatalf("hosted: %d", w.Code)
	}
}

func TestParseRoute(t *testing.T) {
	cases := map[string]string{
		"org/model/resolve/main/model.bin":                "models org/model main model.bin",
		"gpt2/resolve/" + sha1st + "/onnx/model.onnx":     "models gpt2 " + sha1st + " onnx/model.onnx",
		"datasets/org/ds/resolve/v1.0/data/train.parquet": "datasets org/ds v1.0 data/train.parquet",
		"spaces/org/app/resolve/main/app.py":              "spaces org/app main app.py",
		"org/model/resolve/main/":                         "",
		"org/model/resolve//f":                            "",
		"a/b/c/resolve/main/f":                            "",
		"org/model/resolve/main/../f":                     "",
		"datasets//resolve/main/f":                        "",
		"org/model/tree/main":                             "",
	}
	for p, want := range cases {
		rt, ok := ParseRoute(p)
		got := ""
		if ok {
			got = rt.Kind + " " + rt.RepoID + " " + rt.Rev + " " + rt.File
		}
		if got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}

func TestComponentsCleanupAndPrivileges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.do("GET", "org/model/resolve/main/model.bin")
	e.do("GET", "org/model/resolve/main/config.json")
	e.do("GET", "datasets/org/ds/resolve/main/data.csv")
	for p, want := range map[string]string{
		"org/model/resolve/" + sha1st + "/model.bin":       "org/model@" + sha1st,
		"org/model/resolve/" + sha1st + "/onnx/model.onnx": "org/model@" + sha1st,
		"gpt2/resolve/" + sha1st + "/config.json":          "gpt2@" + sha1st,
		"datasets/org/ds/resolve/" + sha1st + "/data.csv":  "datasets/org/ds@" + sha1st,
		"spaces/org/app/resolve/" + sha1st + "/app.py":     "spaces/org/app@" + sha1st,
		"api/models/org/model/revision/main":               "",
		"api/models/org/model/resolve/" + sha1st + "/x":    "",
		"org/model/resolve/main/model.bin":                 "",
		"a/b/c/resolve/" + sha1st + "/f":                   "",
		"org/model/resolve/" + sha1st + "/":                "",
	} {
		g, n, v, ok := component.DeriveAsset("huggingface", p)
		got := ""
		if ok {
			got = component.Component{Group: g, Name: n, Version: v}.Label()
		}
		if got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
	rp := e.registry.Get("hf")
	e.now.Add(10 * 86400)
	ev := &cleanup.Evaluator{DB: e.db, Now: e.store.Now}
	pol := cleanup.Policy{Name: "unused", Format: "huggingface", Criteria: cleanup.Criteria{LastDownloadedDays: 5}}
	if err := pol.Validate(); err != nil {
		t.Fatal(err)
	}
	res, err := ev.EvaluateFrom(ctx, rp, pol, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 2 || res.Candidates[0].Label() != "org/model@"+sha1st || len(res.Candidates[0].Assets) != 2 || res.Candidates[1].Label() != "datasets/org/ds@"+sha1st {
		t.Fatalf("candidates: %+v", res.Candidates)
	}
	r, err := cleanup.NewRegistry(e.store).Delete(ctx, rp, res.Candidates[0].Component, "system")
	if err != nil || r.Assets != 2 {
		t.Fatalf("delete: %+v %v", r, err)
	}
	if _, err := e.store.Get(ctx, "hf", "org/model/resolve/"+sha1st+"/model.bin"); err == nil {
		t.Fatal("file still cached")
	}
	if _, err := e.store.Get(ctx, "hf", "api/models/org/model/revision/main"); err != nil {
		t.Fatal("api document deleted")
	}
	if s := e.sink.String(); !strings.Contains(s, "delete|system|hf|org/model@"+sha1st) {
		t.Fatalf("events: %s", s)
	}
	if _, b := e.do("GET", "org/model/resolve/main/model.bin"); b != body1 {
		t.Fatalf("refetch: %q", b)
	}

	if err := e.db.CreateRole(ctx, meta.Role{ID: "hfreader", Privileges: []meta.Privilege{{Target: "format:huggingface", Actions: []string{meta.ActionRead, meta.ActionDelete}}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.CreateUser(ctx, meta.User{ID: "dev", PasswordHash: "x", Roles: []string{"hfreader"}}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Open(ctx, e.db, e.db)
	if err != nil {
		t.Fatal(err)
	}
	a.RepoFormat = func(name string) string { return map[string]string{"pods": "cocoapods", "hf": "huggingface"}[name] }
	p, _ := a.Lookup("dev")
	if !a.Can(p, "hf", auth.Read) || !a.Can(p, "hf", auth.Delete) || a.Can(p, "pods", auth.Delete) || a.Can(p, "hf", auth.Add) {
		t.Fatal("format:huggingface privilege")
	}
}

func TestParseMetadata(t *testing.T) {
	m, err := ParseMetadata([]byte(`{"sha":"` + sha1st + `","pipeline_tag":"text-generation","library_name":"transformers","tags":["license:mit"],"cardData":{"license":["apache-2.0","other"]}}`))
	if err != nil || m.SHA != sha1st || m.PipelineTag != "text-generation" || m.Library != "transformers" || m.License != "apache-2.0, other" {
		t.Fatalf("%+v %v", m, err)
	}
	if m, _ := ParseMetadata([]byte(`{"tags":["pytorch","license:mit"]}`)); m.License != "mit" {
		t.Fatalf("%+v", m)
	}
}
