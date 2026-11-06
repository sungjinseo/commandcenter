package npm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// auditEnv is newEnv with a recording remote and more groups around its proxy.
// heard says what the remote was told since mark, in a path or a body.
type auditEnv struct {
	*env
	mu   sync.Mutex
	seen []string
}

func newAuditEnv(t *testing.T) *auditEnv {
	t.Helper()
	e := &auditEnv{env: newEnv(t)}
	var accepted string
	fake := fakeRegistry(&accepted)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		e.mu.Lock()
		e.seen = append(e.seen, r.Method+" "+r.URL.EscapedPath()+" "+r.URL.Path+" "+string(b))
		e.mu.Unlock()
		if strings.Contains(r.URL.Path, "failing-pkg") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fake(w, r)
	}))
	t.Cleanup(ts.Close)
	age := config.Duration(time.Hour)
	cfg := &config.Config{Repositories: []config.Repository{
		{Name: "npm-hosted", Format: "npm", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "npm-outside", Format: "npm", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "npm-proxy", Format: "npm", Type: "proxy", Remote: ts.URL, MetadataMaxAge: age, ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "npm-proxy2", Format: "npm", Type: "proxy", Remote: ts.URL + "/second/", MetadataMaxAge: age, ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "npm-all", Format: "npm", Type: "group", Members: []string{"npm-hosted", "npm-proxy"}, WriteMember: "npm-hosted"},
		{Name: "npm-proxy-first", Format: "npm", Type: "group", Members: []string{"npm-proxy", "npm-hosted"}},
		{Name: "npm-merged", Format: "npm", Type: "group", Members: []string{"npm-hosted", "npm-proxy"}, MergeMembers: true},
		{Name: "npm-two", Format: "npm", Type: "group", Members: []string{"npm-hosted", "npm-proxy", "npm-proxy2"}},
	}}
	e.repos = repo.Build(cfg)
	if _, err := e.h.Store.DB.ImportRepositories(context.Background(), cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *auditEnv) mark() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.seen)
}

func (e *auditEnv) heard(mark int, text string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, s := range e.seen[mark:] {
		if strings.Contains(s, text) {
			out = append(out, s)
		}
	}
	return out
}

const (
	bulkPath = "-/npm/v1/security/advisories/bulk"
	treePath = "-/npm/v1/security/audits/quick"
)

// sent is the body the fake remote echoes.
func sent(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m struct {
		Encoding string          `json:"encoding"`
		Body     json.RawMessage `json:"body"`
	}
	if w.Code != http.StatusTeapot || json.Unmarshal(w.Body.Bytes(), &m) != nil {
		t.Fatalf("no echo: %d %s", w.Code, w.Body)
	}
	if m.Encoding != "" {
		t.Fatalf("Content-Encoding %q sent to the remote", m.Encoding)
	}
	return string(m.Body)
}

func (e *auditEnv) publish(t *testing.T, repoName, name string) {
	t.Helper()
	if w := e.do("PUT", repoName, name, publishBody(name, "1.0.0", []byte("1.0.0"), false), ci); w.Code != 201 {
		t.Fatalf("publish %s: %d %s", name, w.Code, w.Body)
	}
}

// TestAuditPrivateNames pins the private-name rule (auditKeeper): a hosted
// repository's name reaches no remote, in a POST or a GET, whichever way the
// audit comes, unless a read already found it there.
func TestAuditPrivateNames(t *testing.T) {
	e := newAuditEnv(t)
	e.publish(t, "npm-hosted", "@acme/secret")
	e.publish(t, "npm-outside", "secret-outside") // a member of no group: a lock file can still name it
	e.publish(t, "npm-hosted", "left-pad")        // the remote has this one too
	body := `{"@acme/secret":["1.0.0"],"secret-outside":["1.0.0"],"lodash":["4.17.20"]}`
	for _, r := range []string{"npm-all", "npm-proxy-first", "npm-proxy", "npm-merged"} {
		mark := e.mark()
		if got := sent(t, e.do("POST", r, bulkPath, body, anon)); got != `{"lodash":["4.17.20"]}` {
			t.Errorf("%s: the remote got %s", r, got)
		}
		for _, n := range []string{"secret"} {
			if h := e.heard(mark, n); len(h) != 0 {
				t.Errorf("%s: the remote heard %s: %q", r, n, h)
			}
		}
	}

	// A name both have: with hosted first it stays. Otherwise it goes only once a
	// read through the proxy found it upstream.
	body = `{"left-pad":["1.0.0"]}`
	mark := e.mark()
	for _, r := range []string{"npm-all", "npm-proxy-first", "npm-proxy", "npm-merged"} {
		if got := sent(t, e.do("POST", r, bulkPath, body, anon)); got != `{}` {
			t.Errorf("%s, nothing read yet: %s", r, got)
		}
	}
	if h := e.heard(mark, "left-pad"); len(h) != 0 {
		t.Errorf("the remote heard left-pad: %q", h)
	}
	if w := e.do("GET", "npm-proxy", "left-pad", "", anon); w.Code != 200 {
		t.Fatal(w.Code)
	}
	for r, want := range map[string]string{"npm-all": `{}`, "npm-proxy-first": body, "npm-proxy": body, "npm-merged": body} {
		if got := sent(t, e.do("POST", r, bulkPath, body, anon)); got != want {
			t.Errorf("%s, after the read: %s, want %s", r, got, want)
		}
	}

	// The older endpoints' tree, as pnpm sends it, with npm 6's name and lists: gone
	// at every level, with all that hangs under a private package.
	tree := `{"name":"@acme/app","version":"1.0.0","install":["@acme/secret"],"remove":["@acme/secret"],"metadata":{"npm_version":"6"},"dev":false,
"requires":{".":"1.0.0","@acme/secret":"1.0.0"},
"dependencies":{".":{"version":"1.0.0","dev":false,"requires":{"@acme/secret":"1.0.0","lodash":"4.17.20"},
"dependencies":{"@acme/secret":{"version":"1.0.0","requires":{"under-secret":"1.0.0"},"dependencies":{"under-secret":{"version":"1.0.0"}}},"lodash":{"version":"4.17.20","integrity":"sha512-x==","count":1.50}}}}}`
	want := `{"version":"1.0.0","metadata":{"npm_version":"6"},"dev":false,"requires":{".":"1.0.0"},"dependencies":{".":{"version":"1.0.0","dev":false,"requires":{"lodash":"4.17.20"},"dependencies":{"lodash":{"version":"4.17.20","integrity":"sha512-x==","count":1.50}}}}}`
	mark = e.mark()
	if got := sent(t, e.do("POST", "npm-all", treePath, tree, anon)); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
	for _, n := range []string{"acme", "under-secret"} {
		if h := e.heard(mark, n); len(h) != 0 {
			t.Errorf("the remote heard %s: %q", n, h)
		}
	}

	// npm's own request is gzipped: the same, and it goes on unzipped.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, `{"@acme/secret":["1.0.0"],"lodash":["4.17.20"]}`)
	zw.Close()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/repository/npm-all/"+bulkPath, &gz)
	r.Header.Set("Content-Encoding", "gzip")
	r = r.WithContext(auth.WithPrincipal(r.Context(), anon))
	e.h.Serve(w, r, e.repos["npm-all"], bulkPath)
	if got := sent(t, w); got != `{"lodash":["4.17.20"]}` {
		t.Errorf("gzipped: %s", got)
	}
}

// TestAuditRoutingRule: a rule that refuses a name keeps it from the proxy.
func TestAuditRoutingRule(t *testing.T) {
	e := newAuditEnv(t)
	e.h.Resolver.Fetcher.RouteAllowed = func(rp *repo.Repository, path string) bool { return !strings.HasPrefix(path, "/@internal/") }
	mark := e.mark()
	for _, r := range []string{"npm-proxy", "npm-all"} {
		if got := sent(t, e.do("POST", r, bulkPath, `{"@internal/x":["1.0.0"],"lodash":["1.0.0"]}`, anon)); got != `{"lodash":["1.0.0"]}` {
			t.Errorf("%s: %s", r, got)
		}
	}
	if h := e.heard(mark, "internal"); len(h) != 0 {
		t.Errorf("the remote heard a refused name: %q", h)
	}
}

// TestAuditTwoProxies: the proxy that gets the audit hears only of what it has.
func TestAuditTwoProxies(t *testing.T) {
	e := newAuditEnv(t)
	mark := e.mark()
	if got := sent(t, e.do("POST", "npm-two", bulkPath, `{"left-pad":["1.0.0"],"elsewhere":["1.0.0"]}`, anon)); got != `{"left-pad":["1.0.0"]}` {
		t.Errorf("%s", got)
	}
	for _, h := range e.heard(mark, "elsewhere") {
		if !strings.HasPrefix(h, "GET ") { // a read would ask as well
			t.Errorf("the first proxy was told: %q", h)
		}
	}
	// A proxy that cannot say whether it has a package fails the audit: not a 400,
	// and not a name given to the proxy behind it or dropped as if it had none.
	if w := e.do("POST", "npm-two", bulkPath, `{"failing-pkg":["1.0.0"]}`, anon); w.Code != http.StatusBadGateway {
		t.Fatalf("a failing lookup: %d %s", w.Code, w.Body)
	}
}

// TestAuditBadRequests: what cannot be read is the client's failure: a 4xx, and
// no failure of the remote for its auto-block.
func TestAuditBadRequests(t *testing.T) {
	e := newAuditEnv(t)
	deep := strings.Repeat(`{"a":`, maxAuditDepth+1) + "1" + strings.Repeat("}", maxAuditDepth+1)
	for name, body := range map[string]string{"not JSON": "hello", "cut short": `{"a":`, "too deep": deep} {
		for range 4 {
			if w := e.do("POST", "npm-proxy", treePath, body, anon); w.Code != http.StatusBadRequest {
				t.Fatalf("%s: %d %.200s", name, w.Code, w.Body)
			}
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/repository/npm-proxy/"+bulkPath, strings.NewReader("not gzip"))
	r.Header.Set("Content-Encoding", "gzip")
	e.h.Serve(w, r, e.repos["npm-proxy"], bulkPath)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("not gzip: %d %s", w.Code, w.Body)
	}
	if got := sent(t, e.do("POST", "npm-proxy", bulkPath, `{"lodash":["1.0.0"]}`, anon)); got != `{"lodash":["1.0.0"]}` {
		t.Fatalf("after them: %s", got)
	}
	// A blocked remote is never asked: a 502 and nothing heard.
	e.repos["npm-proxy"].Blocked = true
	mark := e.mark()
	if w := e.do("POST", "npm-proxy", bulkPath, `{"lodash":["1.0.0"]}`, anon); w.Code != http.StatusBadGateway {
		t.Fatalf("blocked: %d", w.Code)
	}
	if h := e.heard(mark, "lodash"); len(h) != 0 || slices.Contains(h, "POST") {
		t.Fatalf("a blocked remote heard: %q", h)
	}
}
