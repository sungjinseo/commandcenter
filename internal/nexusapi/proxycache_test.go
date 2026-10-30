package nexusapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// upstream is a remote for the proxy tests: files it serves (with an ETag
// and 304 for If-None-Match), a status for every other path (404, or 403
// as S3 and CloudFront answer for a missing key), and what it was asked.
type upstream struct {
	*httptest.Server
	miss        int
	mu          sync.Mutex
	files       map[string]string
	hits        atomic.Int32
	conditional atomic.Int32
	lastUA      atomic.Value
	lastAuth    atomic.Value
}

func newUpstream(t *testing.T, miss int, files map[string]string) *upstream {
	u := &upstream{miss: miss, files: files}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.lastUA.Store(r.Header.Get("User-Agent"))
		u.lastAuth.Store(r.Header.Get("Authorization"))
		if strings.Contains(r.URL.Path, "/cookie/") {
			if _, err := r.Cookie("lb"); err != nil { // a load balancer's cookie check
				http.SetCookie(w, &http.Cookie{Name: "lb", Value: "1", Path: "/"})
				http.Redirect(w, r, r.URL.Path, http.StatusFound)
				return
			}
		}
		u.mu.Lock()
		body, ok := u.files[r.URL.Path]
		u.mu.Unlock()
		if !ok && !strings.Contains(r.URL.Path, "/cookie/") {
			w.WriteHeader(u.miss)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			u.conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"e"`)
		io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) put(path, body string) {
	u.mu.Lock()
	u.files[path] = body
	u.mu.Unlock()
}

// startProxies runs the app with maven-releases (hosted), s3 (a proxy of a
// remote answering 403 for what it lacks), central (one answering 404) and
// maven-public, the group of the three.
func startProxies(t *testing.T) (*env, *upstream, *upstream) {
	t.Helper()
	s3 := newUpstream(t, http.StatusForbidden, map[string]string{"/m2/org/s/1.0/s-1.0.jar": "from-s3"})
	central := newUpstream(t, http.StatusNotFound, map[string]string{"/m2/org/x/1.0/x-1.0.jar": "jar",
		"/m2/org/x/maven-metadata.xml": "<metadata/>"})
	return startRepos(t, fmt.Sprintf(`
  - { name: maven-releases, format: maven, type: hosted }
  - { name: s3, format: maven, type: proxy, remote: %s/m2/ }
  - { name: central, format: maven, type: proxy, remote: %s/m2/, contentMaxAge: "-1" }
  - { name: maven-public, format: maven, type: group, members: [maven-releases, s3, central] }
`, s3.URL, central.URL)), s3, central
}

// startRepos runs the app with admin "admin", user "ci" (no roles) and the
// given repositories section.
func startRepos(t *testing.T, repos string) *env {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
dataDir: %s
anonymousRead: true
users:
  - { name: admin, passwordHash: %q, roles: [admin] }
  - { name: ci, passwordHash: %q, roles: [] }
repositories:%s`, t.TempDir(), string(h), string(h), repos)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Close() })
	return &env{t, ts}
}

// core-01: an S3-backed member's 403 for a missing key is a miss: the group
// moves on to the member that has the file, and a file no member has is a
// 404 (not a 502), negative-cached at the 403 member.
func TestGroupSkipsForbiddenMember(t *testing.T) {
	e, s3, _ := startProxies(t)
	if b := e.want("GET", "/repository/maven-public/org/x/1.0/x-1.0.jar", "", "", 200); string(b) != "jar" {
		t.Fatalf("body %q", b)
	}
	e.want("GET", "/repository/maven-public/org/s/1.0/s-1.0.jar", "", "", 200)
	e.want("GET", "/repository/maven-public/org/x/1.0/x-1.0-sources.jar", "", "", 404)
	before := s3.hits.Load()
	e.want("GET", "/repository/maven-public/org/x/1.0/x-1.0-sources.jar", "", "", 404)
	e.want("GET", "/repository/s3/org/x/1.0/x-1.0-sources.jar", "", "", 404)
	if s3.hits.Load() != before {
		t.Fatalf("the 403 was not negative-cached: %d more requests", s3.hits.Load()-before)
	}
}

// core-02: metadataMaxAge 0 revalidates every request; negativeCache
// enabled false records no misses; both read back as sent.
func TestMaxAgeZeroAndNegativeCacheOff(t *testing.T) {
	e, _, central := startProxies(t)
	body := fmt.Sprintf(`{"name":"c0","online":true,"storage":{"blobStoreName":"default"},
	  "proxy":{"remoteUrl":"%s/m2/","contentMaxAge":-1,"metadataMaxAge":0},
	  "negativeCache":{"enabled":false,"timeToLive":1440},"httpClient":{"blocked":false,"autoBlock":true}}`, central.URL)
	e.want("POST", v1+"/repositories/maven/proxy", "admin", body, 201)
	var s map[string]any
	e.get(v1+"/repositories/maven/proxy/c0", "admin", &s)
	if p, nc := s["proxy"].(map[string]any), s["negativeCache"].(map[string]any); p["metadataMaxAge"] != float64(0) ||
		nc["enabled"] != false || nc["timeToLive"] != float64(1440) {
		t.Fatalf("settings %v", s)
	}
	var cfg map[string]any
	e.get("/-/api/repositories/c0/config", "admin", &cfg)
	if cfg["metadataMaxAge"] != "0s" || cfg["negativeCacheEnabled"] != false || cfg["negativeCacheTTL"] != "24h" {
		t.Fatalf("native config %v", cfg)
	}
	before := central.hits.Load()
	e.want("GET", "/repository/c0/org/x/maven-metadata.xml", "", "", 200)
	e.want("GET", "/repository/c0/org/x/maven-metadata.xml", "", "", 200)
	if central.hits.Load()-before != 2 {
		t.Fatalf("metadataMaxAge 0: %d remote requests, want 2", central.hits.Load()-before)
	}
	e.want("GET", "/repository/c0/org/n/1.0/n-1.0.jar", "", "", 404)
	central.put("/m2/org/n/1.0/n-1.0.jar", "new")
	e.want("GET", "/repository/c0/org/n/1.0/n-1.0.jar", "", "", 200)
	// A PUT that leaves negativeCache.enabled out keeps it off.
	e.want("PUT", v1+"/repositories/maven/proxy/c0", "admin", strings.Replace(body, `"enabled":false,`, "", 1), 204)
	e.get(v1+"/repositories/maven/proxy/c0", "admin", &s)
	if s["negativeCache"].(map[string]any)["enabled"] != false {
		t.Fatalf("after PUT %v", s["negativeCache"])
	}
}

// core-03, rest-13: invalidate-cache makes a proxy's content stale even with
// contentMaxAge -1 (the next GET is a conditional request) and forgets its
// misses; on a group it reaches the proxy members. Hosted is a 400, an
// unknown name a 404; rebuild-index is accepted for all but groups.
func TestInvalidateCache(t *testing.T) {
	e, _, central := startProxies(t)
	const jar = "/repository/central/org/x/1.0/x-1.0.jar"
	e.want("GET", jar, "", "", 200)
	e.want("GET", "/repository/central/org/late/1.0/late-1.0.jar", "", "", 404)
	central.put("/m2/org/late/1.0/late-1.0.jar", "late")
	e.want("GET", jar, "", "", 200)
	e.want("GET", "/repository/central/org/late/1.0/late-1.0.jar", "", "", 404) // still negative-cached
	if central.conditional.Load() != 0 {
		t.Fatal("contentMaxAge -1 revalidated")
	}

	e.want("POST", v1+"/repositories/central/invalidate-cache", "ci", "", 403)
	e.want("POST", v1+"/repositories/central/invalidate-cache", "", "", 401)
	e.want("POST", v1+"/repositories/central/invalidate-cache", "admin", "", 204)
	e.want("GET", jar, "", "", 200)
	e.want("GET", jar, "", "", 200)
	if central.conditional.Load() != 1 {
		t.Fatalf("conditional requests %d, want 1", central.conditional.Load())
	}
	e.want("GET", "/repository/central/org/late/1.0/late-1.0.jar", "", "", 200)

	e.want("POST", v1+"/repositories/maven-public/invalidate-cache", "admin", "", 204)
	e.want("GET", "/repository/maven-public/org/x/1.0/x-1.0.jar", "", "", 200)
	if central.conditional.Load() != 2 {
		t.Fatalf("after the group's invalidation: conditional requests %d, want 2", central.conditional.Load())
	}
	b := e.want("POST", v1+"/repositories/maven-releases/invalidate-cache", "admin", "", 400)
	if !strings.Contains(string(b), "not a proxy or group") {
		t.Fatalf("hosted: %s", b)
	}
	e.want("POST", v1+"/repositories/nope/invalidate-cache", "admin", "", 404)
	// The admin API names the proxies it reached.
	req, _ := http.NewRequest("POST", e.ts.URL+"/-/api/repositories/maven-public/invalidate-cache", nil)
	req.SetBasicAuth("admin", "pw")
	req.Header.Set("X-Command-Center-UI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var res struct{ Invalidated []string }
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != 200 || fmt.Sprint(res.Invalidated) != "[s3 central]" {
		t.Fatalf("admin API: %d %v", resp.StatusCode, res)
	}

	e.want("POST", v1+"/repositories/maven-releases/rebuild-index", "admin", "", 204)
	e.want("POST", v1+"/repositories/central/rebuild-index", "admin", "", 204)
	e.want("POST", v1+"/repositories/maven-public/rebuild-index", "admin", "", 400)
	e.want("POST", v1+"/repositories/nope/rebuild-index", "admin", "", 404)
}

// core-06, rest-14: httpClient.connection is kept, applied (the User-Agent
// suffix, cookies) and read back; a PUT without it keeps it, {} clears it.
func TestConnectionSettings(t *testing.T) {
	e, _, central := startProxies(t)
	conn := `{"retries":1,"userAgentSuffix":"ci/1","timeout":20,"enableCircularRedirects":true,"enableCookies":true,"useTrustStore":true}`
	body := func(c string) string {
		return fmt.Sprintf(`{"name":"cc","online":true,"storage":{"blobStoreName":"default"},
		  "proxy":{"remoteUrl":"%s/m2/","contentMaxAge":-1,"metadataMaxAge":1440},
		  "negativeCache":{"enabled":true,"timeToLive":1440},"httpClient":{"blocked":false,"autoBlock":true%s}}`, central.URL, c)
	}
	e.want("POST", v1+"/repositories/maven/proxy", "admin", body(`,"connection":`+conn), 201)
	var s map[string]any
	e.get(v1+"/repositories/maven/proxy/cc", "admin", &s)
	got, _ := json.Marshal(s["httpClient"].(map[string]any)["connection"])
	var a, b map[string]any
	json.Unmarshal(got, &a)
	json.Unmarshal([]byte(conn), &b)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("connection %s, want %s", got, conn)
	}
	e.want("GET", "/repository/cc/org/x/1.0/x-1.0.jar", "", "", 200)
	if ua, _ := central.lastUA.Load().(string); ua != "commandcenter/1 ci/1" {
		t.Fatalf("User-Agent %q", ua)
	}
	e.want("GET", "/repository/cc/org/cookie/1.0/cookie-1.0.jar", "", "", 200) // set, redirect, sent back

	e.want("PUT", v1+"/repositories/maven/proxy/cc", "admin", body(""), 204)
	e.get(v1+"/repositories/maven/proxy/cc", "admin", &s)
	if s["httpClient"].(map[string]any)["connection"] == nil {
		t.Fatal("a PUT without connection dropped it")
	}
	e.want("PUT", v1+"/repositories/maven/proxy/cc", "admin", body(`,"connection":{}`), 204)
	e.get(v1+"/repositories/maven/proxy/cc", "admin", &s)
	if c := s["httpClient"].(map[string]any)["connection"]; c != nil {
		t.Fatalf("connection {} kept %v", c)
	}
	e.want("GET", "/repository/cc/org/cookie/2.0/cookie-2.0.jar", "", "", 502) // no cookies: the redirect loop ends
	e.want("PUT", v1+"/repositories/maven/proxy/cc", "admin", body(`,"connection":{"timeout":4000}`), 400)
	e.want("PUT", v1+"/repositories/maven/proxy/cc", "admin", body(`,"connection":{"userAgentSuffix":"a\nb"}`), 400)
}

// core-07, rest-14: authentication type ntlm is refused (not sent as
// Basic); bearerToken is kept as one and sent as Bearer; switching to
// username drops the token.
func TestRemoteAuthenticationTypes(t *testing.T) {
	e, _, central := startProxies(t)
	body := func(auth string) string {
		return fmt.Sprintf(`{"name":"ca","online":true,"storage":{"blobStoreName":"default"},
		  "proxy":{"remoteUrl":"%s/m2/","contentMaxAge":0,"metadataMaxAge":1440},
		  "negativeCache":{"enabled":true,"timeToLive":1440},"httpClient":{"blocked":false,"autoBlock":true,"authentication":%s}}`, central.URL, auth)
	}
	b := e.want("POST", v1+"/repositories/maven/proxy", "admin",
		body(`{"type":"ntlm","username":"u","password":"p","ntlmHost":"h","ntlmDomain":"d"}`), 400)
	if !strings.Contains(string(b), "ntlm") {
		t.Fatalf("ntlm: %s", b)
	}
	e.want("POST", v1+"/repositories/maven/proxy", "admin", body(`{"type":"bearerToken","bearerToken":"tok"}`), 201)
	var s map[string]any
	e.get(v1+"/repositories/maven/proxy/ca", "admin", &s)
	if a := s["httpClient"].(map[string]any)["authentication"].(map[string]any); a["type"] != "bearerToken" {
		t.Fatalf("authentication %v", a)
	}
	e.want("GET", "/repository/ca/org/x/1.0/x-1.0.jar", "", "", 200)
	if central.lastAuth.Load() != "Bearer tok" {
		t.Fatalf("Authorization %q", central.lastAuth.Load())
	}
	e.want("PUT", v1+"/repositories/maven/proxy/ca", "admin", body(`{"type":"username","username":"u","password":"p"}`), 204)
	e.want("GET", "/repository/ca/org/x/1.0/x-1.0.jar", "", "", 200) // contentMaxAge 0: asks again
	if auth, _ := central.lastAuth.Load().(string); !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("Authorization %q", auth)
	}
	e.get(v1+"/repositories/maven/proxy/ca", "admin", &s)
	if a := s["httpClient"].(map[string]any)["authentication"].(map[string]any); a["type"] != "username" || a["username"] != "u" {
		t.Fatalf("authentication %v", a)
	}
}

// core-11: a matcher stored through the Nexus API is anchored (and read
// back as sent); one that is not a regular expression by itself is a 400; a
// native rule's matcher is read back as stored, even one shaped like ^(?:…)$.
func TestRoutingMatchersNexusSemantics(t *testing.T) {
	e, _, _ := startProxies(t)
	e.want("POST", v1+"/routing-rules", "admin", `{"name":"x","mode":"BLOCK","matchers":["a)|(?:b"]}`, 400)
	e.want("POST", v1+"/routing-rules", "admin", `{"name":"acme","mode":"ALLOW","matchers":["/org/x/.*"]}`, 204)
	e.want("PUT", v1+"/repositories/maven/proxy/central", "admin", `{"name":"central","online":true,"storage":{"blobStoreName":"default"},
	  "proxy":{"remoteUrl":"`+"http://127.0.0.1:1/m2/"+`","contentMaxAge":-1,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1},"routingRule":"acme"}`, 204)
	e.want("GET", "/repository/central/com/org/x/1.0/x-1.0.jar", "", "", 403) // contains /org/x/, but does not start with it
	req, _ := http.NewRequest("POST", e.ts.URL+"/-/api/routing-rules", strings.NewReader(`{"name":"native","mode":"BLOCK","matchers":["^(?:a)|(?:b)$"]}`))
	req.SetBasicAuth("admin", "pw")
	req.Header.Set("X-Command-Center-UI", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("native create: %v %v", resp, err)
	}
	resp.Body.Close()
	var rule map[string]any
	e.get(v1+"/routing-rules/native", "admin", &rule)
	if fmt.Sprint(rule["matchers"]) != "[^(?:a)|(?:b)$]" {
		t.Fatalf("native matchers read back as %v", rule["matchers"])
	}
}

// core-01 keeps the npm and PyPI groups' fail-closed rule: a 403 for the
// package document of an internal registry is a failure, so a later
// member's same-named public package does not stand in for it.
func TestNpmGroupForbiddenPackumentFailsClosed(t *testing.T) {
	internal := newUpstream(t, http.StatusForbidden, map[string]string{})
	public := newUpstream(t, http.StatusNotFound, map[string]string{})
	public.put("/lp", `{"name":"lp","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{"name":"lp","version":"1.0.0",`+
		`"dist":{"tarball":"`+public.URL+`/lp/-/lp-1.0.0.tgz"}}}}`)
	e := startRepos(t, fmt.Sprintf(`
  - { name: npm-internal, format: npm, type: proxy, remote: %s/ }
  - { name: npm-public, format: npm, type: proxy, remote: %s/ }
  - { name: npm-all, format: npm, type: group, members: [npm-internal, npm-public] }
`, internal.URL, public.URL))
	e.want("GET", "/repository/npm-public/lp", "", "", 200)
	if st, b, _ := e.do("GET", "/repository/npm-all/lp", "", ""); st != 502 {
		t.Fatalf("group: %d %s", st, b)
	}
}
