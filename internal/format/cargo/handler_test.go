package cargo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

const base = "http://repo.test"

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
	hits  *atomic.Int32
}

// fakeIndex serves a sparse index like index.crates.io and crates like static.crates.io.
func fakeIndex(hits *atomic.Int32, files string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config.json":
			io.WriteString(w, `{"dl":"`+files+`/crates","api":"https://crates.io"}`)
		case "/it/oa/itoa":
			io.WriteString(w, `{"name":"itoa","vers":"1.0.0","deps":[],"cksum":"`+sum("itoa-1.0.0")+`","features":{},"yanked":false}`+"\n"+
				`{"name":"itoa","vers":"1.0.1","deps":[],"cksum":"`+sum("other")+`","features":{},"yanked":false}`+"\n")
		case "/no/su/nosum":
			io.WriteString(w, `{"name":"nosum","vers":"1.0.0","deps":[],"cksum":"","features":{},"yanked":false}`+"\n")
		case "/ev/il/evil":
			io.WriteString(w, `{"name":"other","vers":"1.0.0","deps":[],"cksum":"`+sum("x")+`","features":{},"yanked":false}`+"\n"+
				`{"name":"../evil","vers":"1.0.0","deps":[],"cksum":"`+sum("x")+`","features":{},"yanked":false}`+"\n")
		case "/crates/other/1.0.0/download":
			io.WriteString(w, "x")
		case "/crates/itoa/1.0.0/download":
			hits.Add(1)
			io.WriteString(w, "itoa-1.0.0")
		case "/crates/itoa/1.0.1/download":
			io.WriteString(w, "tampered")
		default:
			http.NotFound(w, r)
		}
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	hits := &atomic.Int32{}
	var files string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fakeIndex(hits, files)(w, r) }))
	t.Cleanup(ts.Close)
	files = ts.URL
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: time.Now}
	f := false
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "cargo-hosted", Format: "cargo", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "cargo-proxy", Format: "cargo", Type: "proxy", Remote: ts.URL + "/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "cargo-all", Format: "cargo", Type: "group", Members: []string{"cargo-hosted", "cargo-proxy"}, WriteMember: "cargo-hosted"},
		{Name: "cargo-private", Format: "cargo", Type: "hosted", AnonymousRead: &f},
	}}
	return &env{h: New(store, proxy.New(store, ts.Client(), 4), auth.New(cfg, db), func() string { return base }), repos: repo.Build(cfg), hits: hits}
}

func (e *env) do(method, repoName, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/x", body)
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// seedHosted stores a published version the way publish does.
func (e *env) seedHosted(t *testing.T, name, vers, crate string) {
	t.Helper()
	ctx := context.Background()
	sums, err := e.h.Store.WriteBlob("cargo-hosted", strings.NewReader(crate), nil)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := indexLine{Name: name, Vers: vers, Deps: []indexDep{}, Cksum: sums.SHA256, Features: map[string][]string{}}.marshal()
	err = e.h.Store.DB.Tx(ctx, func(tx *meta.Tx) error {
		if err := tx.InsertPackage(ctx, meta.Package{Repo: "cargo-hosted", Format: "cargo", Name: strings.ToLower(name), Version: vers, Attrs: string(line)}); err != nil {
			return err
		}
		return tx.PutAsset(ctx, e.h.Store.NewAsset("cargo-hosted", crateKey(strings.ToLower(name), vers), "application/octet-stream", sums))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func lines(body string) []map[string]any {
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		var m map[string]any
		json.Unmarshal([]byte(l), &m)
		out = append(out, m)
	}
	return out
}

func TestConfigJSON(t *testing.T) {
	e := newEnv(t)
	var c map[string]any
	w := e.do("GET", "cargo-all", "config.json", nil)
	json.Unmarshal(w.Body.Bytes(), &c)
	if c["dl"] != base+"/repository/cargo-all/crates" || c["api"] != base+"/repository/cargo-all" || c["auth-required"] != false {
		t.Fatalf("config = %s", w.Body.String())
	}
	json.Unmarshal(e.do("GET", "cargo-private", "config.json", nil).Body.Bytes(), &c)
	if c["auth-required"] != true {
		t.Fatalf("private config = %v", c)
	}
}

func TestProxyIndexAndDownload(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "cargo-proxy", "it/oa/itoa", nil)
	if l := lines(w.Body.String()); w.Code != 200 || len(l) != 2 || l[0]["vers"] != "1.0.0" {
		t.Fatalf("index: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		if w := e.do("GET", "cargo-proxy", "crates/itoa/1.0.0/download", nil); w.Code != 200 || w.Body.String() != "itoa-1.0.0" {
			t.Fatalf("download: %d %q", w.Code, w.Body.String())
		}
	}
	if e.hits.Load() != 1 {
		t.Fatalf("downloads from upstream = %d, want 1", e.hits.Load())
	}
	// The first mismatch is a 502; it is then negative-cached, so the repeat is a 404.
	for i, want := range []int{502, 404} {
		if w := e.do("GET", "cargo-proxy", "crates/itoa/1.0.1/download", nil); w.Code != want {
			t.Fatalf("cksum mismatch #%d: %d, want %d", i+1, w.Code, want)
		}
	}
	if w := e.do("GET", "cargo-proxy", "crates/itoa/9.9.9/download", nil); w.Code != 404 {
		t.Fatalf("unknown version: %d", w.Code)
	}
	w = e.do("GET", "cargo-proxy", "3/n/nop", nil)
	var errBody struct{ Errors []struct{ Detail string } }
	if w.Code != 404 || json.Unmarshal(w.Body.Bytes(), &errBody) != nil || len(errBody.Errors) != 1 {
		t.Fatalf("unknown crate: %d %s", w.Code, w.Body.String())
	}
}

func TestHostedAndGroup(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "itoa", "1.0.0", "internal itoa")
	e.seedHosted(t, "smoke-hello", "1.0.0", "hello crate")

	w := e.do("GET", "cargo-hosted", "sm/ok/smoke-hello", nil)
	if l := lines(w.Body.String()); len(l) != 1 || l[0]["cksum"] != sum("hello crate") {
		t.Fatalf("hosted index: %s", w.Body.String())
	}
	w = e.do("GET", "cargo-all", "it/oa/itoa", nil)
	l := lines(w.Body.String())
	if len(l) != 1 || l[0]["cksum"] != sum("internal itoa") {
		t.Fatalf("group index (first member with the crate answers alone): %s", w.Body.String())
	}
	if w := e.do("GET", "cargo-all", "crates/itoa/1.0.0/download", nil); w.Body.String() != "internal itoa" {
		t.Fatalf("group download from hosted: %q", w.Body.String())
	}
	if w := e.do("GET", "cargo-all", "crates/smoke-hello/1.0.0/download", nil); w.Body.String() != "hello crate" {
		t.Fatalf("group hosted crate: %q", w.Body.String())
	}
	if w := e.do("GET", "cargo-all", "3/n/nop", nil); w.Code != 404 {
		t.Fatalf("unknown crate in group: %d", w.Code)
	}
	if w := e.do("PUT", "cargo-proxy", "api/v1/crates/new", strings.NewReader("x")); w.Code != 405 {
		t.Fatalf("publish to proxy: %d", w.Code)
	}
}

func TestProxyDownloadRequiresCksum(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 2; i++ {
		if w := e.do("GET", "cargo-proxy", "crates/nosum/1.0.0/download", nil); w.Code != 502 {
			t.Fatalf("empty cksum: %d %s", w.Code, w.Body.String())
		}
	}
	if _, err := e.h.Store.Get(context.Background(), "cargo-proxy", crateKey("nosum", "1.0.0")); err == nil {
		t.Fatal("crate without cksum was cached")
	}
}

func TestProxyDownloadIgnoresForeignIndexNames(t *testing.T) {
	e := newEnv(t)
	if w := e.do("GET", "cargo-proxy", "crates/evil/1.0.0/download", nil); w.Code != 404 {
		t.Fatalf("foreign names: %d %s", w.Code, w.Body.String())
	}
}

func TestProxyUpstreamWithoutConfigJSON(t *testing.T) {
	e := newEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/it/oa/itoa" {
			io.WriteString(w, `{"name":"itoa","vers":"1.0.0","deps":[],"cksum":"`+sum("itoa-1.0.0")+`","features":{},"yanked":false}`+"\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	rp := *e.repos["cargo-proxy"]
	rp.Name = "cargo-noconfig"
	rp.RemoteURL, _ = url.Parse(ts.URL + "/")
	e.repos[rp.Name] = &rp
	w := e.do("GET", rp.Name, "crates/itoa/1.0.0/download", nil)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "upstream registry has no config.json") {
		t.Fatalf("missing config.json: %d %s", w.Code, w.Body.String())
	}
}

func TestConfigJSONUsesLiveBaseURL(t *testing.T) {
	e := newEnv(t)
	cur := "https://one.example/"
	e.h.BaseURL = func() string { return cur }
	for _, want := range []string{"https://one.example", "https://two.example"} {
		var c map[string]any
		json.Unmarshal(e.do("GET", "cargo-all", "config.json", nil).Body.Bytes(), &c)
		if c["api"] != want+"/repository/cargo-all" {
			t.Fatalf("api = %v, want base %s", c["api"], want)
		}
		cur = "https://two.example"
	}
}

func TestGroupIndexFirstMemberWins(t *testing.T) {
	e := newEnv(t)
	// hosted has itoa 1.0.0; the proxy has itoa 1.0.0 and 1.0.1.
	e.seedHosted(t, "itoa", "1.0.0", "internal itoa")
	l := lines(e.do("GET", "cargo-all", "it/oa/itoa", nil).Body.String())
	if len(l) != 1 || l[0]["vers"] != "1.0.0" || l[0]["cksum"] != sum("internal itoa") {
		t.Fatalf("group index must come from the first member only: %v", l)
	}
	if px := lines(e.do("GET", "cargo-proxy", "it/oa/itoa", nil).Body.String()); len(px) != 2 {
		t.Fatalf("proxy sanity: %v", px)
	}
	// A crate only the second member has still resolves.
	e2 := newEnv(t)
	if l := lines(e2.do("GET", "cargo-all", "it/oa/itoa", nil).Body.String()); len(l) != 2 {
		t.Fatalf("falls through to the next member: %v", l)
	}
}

// failingGroup is a group whose first member is a proxy that errors (upstream
// 500), followed by the working proxy that has itoa.
func failingGroup(t *testing.T, e *env) *repo.Repository {
	t.Helper()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	t.Cleanup(bad.Close)
	def := e.repos["cargo-proxy"].Repository
	def.Name, def.Remote = "cargo-bad", bad.URL+"/"
	badRepo := repo.Build(&config.Config{Repositories: []config.Repository{def}})["cargo-bad"]
	g := *e.repos["cargo-all"]
	g.MemberRepos = []*repo.Repository{badRepo, e.repos["cargo-proxy"]}
	return &g
}

func TestGroupFailsClosedOnMemberError(t *testing.T) {
	e := newEnv(t)
	g := failingGroup(t, e)
	for _, p := range []string{"it/oa/itoa", "crates/itoa/1.0.0/download"} {
		w := httptest.NewRecorder()
		e.h.Serve(w, httptest.NewRequest("GET", "/repository/cargo-all/x", nil), g, p)
		if w.Code < 500 || strings.Contains(w.Body.String(), "itoa") && w.Code == 200 {
			t.Errorf("%s: %d %q, want 5xx", p, w.Code, w.Body.String())
		}
	}
	// A member that simply lacks the crate still falls through.
	for _, p := range []string{"it/oa/itoa", "crates/itoa/1.0.0/download"} {
		if w := e.do("GET", "cargo-all", p, nil); w.Code != 200 {
			t.Errorf("fall-through %s: %d", p, w.Code)
		}
	}
}
