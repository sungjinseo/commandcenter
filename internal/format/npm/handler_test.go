package npm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

const base = "http://repo.test"

type env struct {
	h        *Handler
	repos    map[string]*repo.Repository
	accepted *string
}

// fakeRegistry serves left-pad like registry.npmjs.org does.
func fakeRegistry(accept *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/left-pad":
			*accept = r.Header.Get("Accept")
			io.WriteString(w, leftPad)
		case "/left-pad/-/left-pad-1.1.0.tgz":
			io.WriteString(w, "remote-tgz")
		case "/-/npm/v1/security/audits/quick", "/-/npm/v1/security/advisories/bulk":
			// the audit endpoints echo what they were sent (TestAudit)
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTeapot)
			io.WriteString(w, `{"method":"`+r.Method+`","encoding":"`+r.Header.Get("Content-Encoding")+`","body":`+string(b)+`}`)
		default:
			http.NotFound(w, r)
		}
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	var accepted string
	ts := httptest.NewServer(fakeRegistry(&accepted))
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: time.Now}
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := &config.Config{
		Users: []config.User{{Name: "ci", PasswordHash: string(hash)}},
		Repositories: []config.Repository{
			{Name: "npm-hosted", Format: "npm", Type: "hosted", WritePolicy: "allow_once"},
			{Name: "npm-proxy", Format: "npm", Type: "proxy", Remote: ts.URL,
				MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
			{Name: "npm-all", Format: "npm", Type: "group", Members: []string{"npm-hosted", "npm-proxy"}, WriteMember: "npm-hosted"},
		},
	}
	res := &resolve.Resolver{Store: store, Fetcher: proxy.New(store, ts.Client(), 4)}
	return &env{h: New(store, res, auth.New(cfg, db), func() string { return base }), repos: repo.Build(cfg), accepted: &accepted}
}

func (e *env) do(method, repoName, path, body string, p auth.Principal) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+strings.ReplaceAll(path, " ", "%20"), strings.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

func (e *env) seed(t *testing.T, repoName, path, body string) {
	t.Helper()
	if _, err := e.h.Store.Put(context.Background(), repoName, path, "", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON (%d): %q", w.Code, w.Body.String())
	}
	return m
}

var anon = auth.Principal{}

func TestHostedPackumentRewritesTarballs(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "npm-hosted", PackumentKey("left-pad"), hostedDoc)
	w := e.do("GET", "npm-hosted", "left-pad", "", anon)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	m := jsonBody(t, w)
	if get(m, "versions", "1.0.1", "from") != "hosted" {
		t.Fatalf("body = %v", m)
	}
}

func TestProxyPackumentAndTarball(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "npm-proxy", "left-pad", "", anon)
	m := jsonBody(t, w)
	if get(m, "versions", "1.1.0", "dist", "tarball") != base+"/repository/npm-proxy/left-pad/-/left-pad-1.1.0.tgz" {
		t.Fatalf("tarball = %v", get(m, "versions", "1.1.0", "dist", "tarball"))
	}
	if *e.accepted != "application/json" {
		t.Fatalf("Accept sent upstream = %q", *e.accepted)
	}
	w = e.do("GET", "npm-proxy", "left-pad/-/left-pad-1.1.0.tgz", "", anon)
	if w.Code != 200 || w.Body.String() != "remote-tgz" {
		t.Fatalf("tarball: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-proxy", "nope", "", anon); w.Code != 404 || jsonBody(t, w)["error"] == nil {
		t.Fatalf("missing: %d %q", w.Code, w.Body.String())
	}
}

// TestGroupOwnerAnswersAlone: hosted has left-pad@1.0.0, the proxy has 1.0.0,
// 1.1.0 and a beta tag. The hosted member owns left-pad and answers alone
// through the group URLs: no proxy version, tag or tarball gets through, not
// even by its direct URL (dependency confusion, QA N-1, N-11).
func TestGroupOwnerAnswersAlone(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "left-pad", publishBody("left-pad", "1.0.0", []byte("hosted-tgz"), false), ci); w.Code != 201 {
		t.Fatalf("setup publish: %d %s", w.Code, w.Body.String())
	}
	m := jsonBody(t, e.do("GET", "npm-all", "left-pad", "", anon))
	vs, _ := get(m, "versions").(map[string]any)
	if len(vs) != 1 || get(m, "versions", "1.0.0", "description") != "test" || get(m, "readme") != nil {
		t.Fatalf("group document = %v", m)
	}
	if tags, _ := get(m, "dist-tags").(map[string]any); len(tags) != 1 || tags["latest"] != "1.0.0" {
		t.Fatalf("document dist-tags = %v", tags)
	}
	if get(m, "versions", "1.0.0", "dist", "tarball") != base+"/repository/npm-all/left-pad/-/left-pad-1.0.0.tgz" {
		t.Fatalf("tarball = %v", get(m, "versions", "1.0.0", "dist", "tarball"))
	}
	if w := e.do("GET", "npm-all", "left-pad/-/left-pad-1.0.0.tgz", "", anon); w.Code != 200 || w.Body.String() != string(testTarball("left-pad", "1.0.0", []byte("hosted-tgz"))) {
		t.Fatalf("group tarball: %d %q", w.Code, w.Body.String())
	}
	if v := jsonBody(t, e.do("GET", "npm-all", "left-pad/latest", "", anon)); v["version"] != "1.0.0" {
		t.Fatalf("version doc = %v", v)
	}
	tags := jsonBody(t, e.do("GET", "npm-all", "-/package/left-pad/dist-tags", "", anon))
	if len(tags) != 1 || tags["latest"] != "1.0.0" {
		t.Fatalf("dist-tags = %v", tags)
	}
	// The proxy serves 1.1.0 and now has it cached; the group still does not.
	if w := e.do("GET", "npm-proxy", "left-pad/-/left-pad-1.1.0.tgz", "", anon); w.Code != 200 {
		t.Fatalf("sanity: the proxy itself serves 1.1.0: %d", w.Code)
	}
	for _, p := range []string{"left-pad/1.1.0", "left-pad/beta", "left-pad/-/left-pad-1.1.0.tgz"} {
		if w := e.do("GET", "npm-all", p, "", anon); w.Code != 404 {
			t.Errorf("proxy-only %s through the group: %d %q, want 404", p, w.Code, w.Body.String())
		}
	}
	if w := e.do("HEAD", "npm-all", "left-pad", "", anon); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", w.Code, w.Body.Len())
	}
	// HEAD of a version or tag answers what the GET would (clients and the
	// Nexus importer probe versions this way): 200 when present, else 404.
	if w := e.do("HEAD", "npm-proxy", "left-pad/1.1.0", "", anon); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD version: %d %d", w.Code, w.Body.Len())
	}
	if w := e.do("HEAD", "npm-proxy", "left-pad/9.9.9", "", anon); w.Code != 404 {
		t.Fatalf("HEAD missing version: %d, want 404", w.Code)
	}
}

func TestAccountEndpoints(t *testing.T) {
	e := newEnv(t)
	if w := e.do("GET", "npm-all", "-/ping", "", anon); w.Code != 200 {
		t.Fatalf("ping: %d", w.Code)
	}
	if w := e.do("POST", "npm-all", "-/v1/login", "{}", anon); w.Code != 404 {
		t.Fatalf("web login must 404 so npm falls back: %d", w.Code)
	}
	if w := e.do("GET", "npm-all", "-/whoami", "", anon); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anon whoami: %d", w.Code)
	}
	if m := jsonBody(t, e.do("GET", "npm-all", "-/whoami", "", auth.Principal{User: "ci"})); m["username"] != "ci" {
		t.Fatalf("whoami = %v", m)
	}

	w := e.do("PUT", "npm-all", "-/user/org.couchdb.user:ci", `{"name":"ci","password":"pw"}`, anon)
	m := jsonBody(t, w)
	tok, _ := m["token"].(string)
	if w.Code != 201 || m["ok"] != true || !strings.HasPrefix(tok, "cc_") {
		t.Fatalf("login: %d %v", w.Code, m)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("login Cache-Control = %q", cc)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	if p, err := e.h.Auth.Authenticate(r); err != nil || p.User != "ci" {
		t.Fatalf("issued token: %+v %v", p, err)
	}
	if w := e.do("PUT", "npm-all", "-/user/org.couchdb.user:ci", `{"name":"ci","password":"bad"}`, anon); w.Code != 401 {
		t.Fatalf("bad password: %d", w.Code)
	}
	if w := e.do("PUT", "npm-all", "-/user/org.couchdb.user:ci", `{"name":"other","password":"pw"}`, anon); w.Code != 400 {
		t.Fatalf("name mismatch: %d", w.Code)
	}
}

func TestAccessDecisions(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		method, path string
		want         auth.Access
	}{
		{"PUT", "-/user/org.couchdb.user:ci", auth.Public},
		{"POST", "-/v1/login", auth.Public},
		{"GET", "-/ping", auth.Read}, // were public, which revealed hidden repositories
		{"POST", "-/npm/v1/security/audits/quick", auth.Read},
		{"GET", "-/whoami", auth.Read},
		{"GET", "left-pad", auth.Read},
		{"PUT", "left-pad", auth.Add},
		{"PUT", "-/package/left-pad/dist-tags/beta", auth.Edit},
		{"DELETE", "-/package/left-pad/dist-tags/beta", auth.Edit},
		{"PUT", "left-pad/-rev/undefined", auth.Delete}, // npm unpublish
		{"DELETE", "left-pad/-rev/undefined", auth.Delete},
		{"DELETE", "left-pad/-/left-pad-1.0.0.tgz/-rev/x", auth.Delete},
		{"GET", "left-pad/-rev/undefined", auth.Read},
		{"DELETE", "-/user/token/cc_x", auth.Public},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/", nil)
		if got := e.h.Access(r, e.repos["npm-all"], c.path); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestErrorsAreJSON(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "npm-all", "a b", "", anon)
	if w.Code != 400 || jsonBody(t, w)["error"] == nil {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "npm-proxy", "left-pad", "{}", auth.Principal{User: "ci"}); w.Code != 405 {
		t.Fatalf("write to proxy: %d", w.Code)
	}
}

func TestErrorAfterOutputAborts(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "npm-hosted", PackumentKey("big"),
		`{"name":"big","readme":"`+strings.Repeat("x", 40000)+`","versions":{"1.0.0":`)
	var got any
	func() {
		defer func() { got = recover() }()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/repository/npm-hosted/big", nil)
		e.h.Serve(w, r, e.repos["npm-hosted"], "big")
	}()
	if got != http.ErrAbortHandler {
		t.Fatalf("recovered %v, want http.ErrAbortHandler", got)
	}
}

func TestErrorBeforeOutputIsCleanJSON(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "npm-hosted", PackumentKey("left-pad"), hostedDoc)
	w := e.do("GET", "npm-hosted", "left-pad/9.9.9", "", anon)
	if w.Code != 404 || jsonBody(t, w)["error"] == nil {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Last-Modified") != "" {
		t.Fatalf("stale Last-Modified on error: %v", w.Header())
	}
}

// QA L-1: npm login is throttled like every password check: 429 with
// Retry-After, even for the right password, until the refusal ends.
func TestLoginThrottled(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		if w := e.do("PUT", "npm-all", "-/user/org.couchdb.user:ci", `{"name":"ci","password":"bad"}`, anon); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	w := e.do("PUT", "npm-all", "-/user/org.couchdb.user:ci", `{"name":"ci","password":"pw"}`, anon)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || jsonBody(t, w)["token"] != nil {
		t.Fatalf("throttled login: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
}

func TestLoginEdges(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-all", "-/user/org.couchdb.user:nobody", `{"name":"nobody","password":"pw"}`, anon); w.Code != 401 {
		t.Fatalf("unknown user: %d", w.Code)
	}
	if w := e.do("PUT", "npm-all", "-/user/org.couchdb.user:ci", `{"name":"ci","password":""}`, anon); w.Code != 401 {
		t.Fatalf("empty password: %d", w.Code)
	}
	if w := e.do("GET", "npm-all", "-/user/org.couchdb.user:ci", "", anon); w.Code != 405 {
		t.Fatalf("GET login: %d", w.Code)
	}
}

func TestTarballURLUsesLiveBaseURL(t *testing.T) {
	e := newEnv(t)
	cur := "https://one.example/"
	e.h.BaseURL = func() string { return cur }
	rp := e.repos["npm-hosted"]
	if got := e.h.tarballURL(rp, "p")("p-1.tgz"); got != "https://one.example/repository/npm-hosted/p/-/p-1.tgz" {
		t.Fatalf("first: %s", got)
	}
	cur = "https://two.example"
	if got := e.h.tarballURL(rp, "p")("p-1.tgz"); got != "https://two.example/repository/npm-hosted/p/-/p-1.tgz" {
		t.Fatalf("after change: %s", got)
	}
}
