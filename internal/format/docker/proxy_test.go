package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// fakeRegistry is an upstream registry: images by name with manifests by
// tag and digest, blobs by digest, an optional Bearer token flow (its own
// /token realm), and switches for outages, rate limits and corrupt blobs.
type fakeRegistry struct {
	ts *httptest.Server

	mu        sync.Mutex
	manifests map[string]fakeManifest // by name/<tag or digest>
	blobs     map[string][]byte       // by name/<digest>
	served    map[string]int          // 200s by path
	tokens    int                     // tokens issued
	bearer    bool                    // challenge requests without a token
	user, pw  string                  // credentials the token endpoint demands ("": anonymous ok)
	denied    map[string]bool         // names that are 401 even with a token
	status    int                     // when set, every registry request answers it
	corrupt   bool                    // blobs come back altered
	accept    string                  // the last Accept header of a manifest request
	authSeen  []string                // Authorization values of registry requests
}

type fakeManifest struct {
	body []byte
	mt   string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	fr := &fakeRegistry{manifests: map[string]fakeManifest{}, blobs: map[string][]byte{}, served: map[string]int{}, denied: map[string]bool{}}
	fr.ts = httptest.NewServer(fr)
	t.Cleanup(fr.ts.Close)
	return fr
}

func (fr *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fail := func(status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"errors":[{"code":%q,"message":"fake"}]}`, code)
	}
	if r.URL.Path == "/token" {
		if fr.user != "" {
			if u, p, ok := r.BasicAuth(); !ok || u != fr.user || p != fr.pw {
				fail(401, CodeUnauthorized)
				return
			}
		}
		fr.tokens++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":"upstream-token-%d","expires_in":300}`, fr.tokens)
		return
	}
	if fr.status != 0 {
		fail(fr.status, map[int]string{429: CodeTooManyRequests}[fr.status])
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		fail(404, CodeUnsupported)
		return
	}
	segs := strings.Split(rest, "/")
	n := len(segs)
	if n < 3 {
		fail(404, CodeUnsupported)
		return
	}
	name := strings.Join(segs[:n-2], "/")
	fr.authSeen = append(fr.authSeen, r.Header.Get("Authorization"))
	if fr.bearer && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer upstream-token-") || fr.denied[name] {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+fr.ts.URL+`/token",service="fake",scope="repository:`+name+`:pull"`)
		fail(401, CodeUnauthorized)
		return
	}
	switch {
	case segs[n-2] == "manifests":
		fr.accept = r.Header.Get("Accept")
		m, ok := fr.manifests[name+"/"+segs[n-1]]
		if !ok {
			fail(404, CodeManifestUnknown)
			return
		}
		fr.served[r.URL.Path]++
		w.Header().Set("Content-Type", m.mt)
		w.Header().Set("Docker-Content-Digest", digestOf(m.body))
		w.Header().Set("Content-Length", fmt.Sprint(len(m.body)))
		if r.Method != http.MethodHead {
			w.Write(m.body)
		}
	case segs[n-2] == "blobs":
		b, ok := fr.blobs[name+"/"+segs[n-1]]
		if !ok {
			fail(404, CodeBlobUnknown)
			return
		}
		fr.served[r.URL.Path]++
		if fr.corrupt {
			b = append([]byte("corrupt "), b...)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
	case segs[n-2] == "tags" && segs[n-1] == "list":
		var tags []string
		for k := range fr.manifests {
			if nm, ref, _ := strings.Cut(k, "/"); nm == name && !ValidDigest(ref) {
				tags = append(tags, ref)
			}
		}
		if tags == nil {
			fail(404, CodeNameUnknown)
			return
		}
		fr.served[r.URL.Path]++
		sort.Sort(sort.Reverse(sort.StringSlice(tags))) // unsorted on purpose
		writeJSON(w, 200, map[string]any{"name": name, "tags": tags})
	default:
		fail(404, CodeUnsupported)
	}
}

// putImage stores a schema2 image name:tag with the given layers and
// returns its manifest and digest.
func (fr *fakeRegistry) putImage(name, tag string, layers ...[]byte) ([]byte, string) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers"}}`)
	fr.blobs[name+"/"+digestOf(cfg)] = cfg
	m := map[string]any{"schemaVersion": 2, "mediaType": mtSchema2,
		"config": map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "digest": digestOf(cfg), "size": len(cfg)}}
	ls := []map[string]any{}
	for _, l := range layers {
		fr.blobs[name+"/"+digestOf(l)] = l
		ls = append(ls, map[string]any{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": digestOf(l), "size": len(l)})
	}
	m["layers"] = ls
	body, _ := json.Marshal(m)
	fr.manifests[name+"/"+tag] = fakeManifest{body, mtSchema2}
	fr.manifests[name+"/"+digestOf(body)] = fakeManifest{body, mtSchema2}
	return body, digestOf(body)
}

// putIndex stores an OCI index name:tag over the given manifest digests.
func (fr *fakeRegistry) putIndex(name, tag string, digests ...string) []byte {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var ms []string
	for i, d := range digests {
		ms = append(ms, fmt.Sprintf(`{"mediaType":%q,"digest":%q,"size":1,"platform":{"architecture":"arch%d","os":"linux"}}`, mtSchema2, d, i))
	}
	body := []byte(`{"schemaVersion":2,"mediaType":"` + mtOCIIndex + `","manifests":[` + strings.Join(ms, ",") + `]}`)
	fr.manifests[name+"/"+tag] = fakeManifest{body, mtOCIIndex}
	fr.manifests[name+"/"+digestOf(body)] = fakeManifest{body, mtOCIIndex}
	return body
}

func (fr *fakeRegistry) count(path string) int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.served[path]
}

func (fr *fakeRegistry) set(fn func()) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fn()
}

func newEnvWith(t *testing.T, extra ...config.Repository) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), extra: extra}
	e.open()
	return e
}

func proxyOf(fr *fakeRegistry, name string, ra *config.RemoteAuth) config.Repository {
	tr := true
	return config.Repository{Name: name, Format: "docker", Type: "proxy", Remote: fr.ts.URL, RemoteAuth: ra, AnonymousRead: &tr}
}

func TestProxyPull(t *testing.T) {
	fr := newFakeRegistry(t)
	m1, d1 := fr.putImage("team/app", "v1", []byte("layer one"))
	e := newEnvWith(t, proxyOf(fr, "hub", nil))

	resp, b := e.do("GET", "/v2/hub/team/app/manifests/v1", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, m1) || resp.Header.Get("Content-Type") != mtSchema2 || resp.Header.Get("Docker-Content-Digest") != d1 {
		t.Fatalf("manifest by tag: %v %s", resp.Header, b)
	}
	for _, mt := range []string{mtSchema2, mtManifestList, mtOCIManifest, mtOCIIndex} {
		if !strings.Contains(fr.accept, mt) {
			t.Fatalf("upstream Accept %q lacks %s", fr.accept, mt)
		}
	}
	resp, b = e.do("HEAD", "/v2/hub/team/app/manifests/"+d1, nil)
	e.check(resp, b, 200, "")
	if resp.Header.Get("Docker-Content-Digest") != d1 || resp.ContentLength != int64(len(m1)) {
		t.Fatalf("manifest HEAD by digest: %v", resp.Header)
	}
	resp, b = e.do("GET", "/v2/hub/team/app/blobs/"+digestOf([]byte("layer one")), nil)
	e.check(resp, b, 200, "")
	if string(b) != "layer one" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("blob: %v %s", resp.Header, b)
	}
	// cached: the second round reaches the upstream no more
	for _, p := range []string{"/v2/hub/team/app/manifests/v1", "/v2/hub/team/app/manifests/" + d1, "/v2/hub/team/app/blobs/" + digestOf([]byte("layer one"))} {
		resp, b = e.do("GET", p, nil)
		e.check(resp, b, 200, "")
		if n := fr.count("/v2/team/app" + strings.TrimPrefix(p, "/v2/hub/team/app")); n != 1 {
			t.Fatalf("%s: upstream served %d times", p, n)
		}
	}
	// unknown image and blob: 404 from the upstream becomes the registry's
	resp, b = e.do("GET", "/v2/hub/team/nope/manifests/v1", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	resp, b = e.do("GET", "/v2/hub/team/app/blobs/"+digestOf([]byte("nope")), nil)
	e.check(resp, b, 404, CodeBlobUnknown)
	// the UI's download link
	resp, b = e.do("GET", "/repository/hub/team/app/manifests/v1", nil)
	e.check(resp, b, 200, "")
}

func TestProxyIndex(t *testing.T) {
	fr := newFakeRegistry(t)
	_, d1 := fr.putImage("multi", "amd64", []byte("amd64 layer"))
	_, d2 := fr.putImage("multi", "arm64", []byte("arm64 layer"))
	index := fr.putIndex("multi", "latest", d1, d2)
	e := newEnvWith(t, proxyOf(fr, "hub", nil))
	resp, b := e.do("GET", "/v2/hub/multi/manifests/latest", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, index) || resp.Header.Get("Content-Type") != mtOCIIndex {
		t.Fatalf("index: %v %s", resp.Header, b)
	}
	for _, d := range []string{d1, d2} {
		resp, b = e.do("GET", "/v2/hub/multi/manifests/"+d, nil)
		e.check(resp, b, 200, "")
		if digestOf(b) != d || resp.Header.Get("Content-Type") != mtSchema2 {
			t.Fatalf("platform manifest %s: %v", d, resp.Header)
		}
	}
	resp, b = e.do("GET", "/v2/hub/multi/blobs/"+digestOf([]byte("arm64 layer")), nil)
	e.check(resp, b, 200, "")
}

func TestProxyRevalidation(t *testing.T) {
	fr := newFakeRegistry(t)
	m1, d1 := fr.putImage("app", "latest", []byte("l1"))
	e := newEnvWith(t, proxyOf(fr, "hub", nil))
	resp, b := e.do("GET", "/v2/hub/app/manifests/latest", nil)
	e.check(resp, b, 200, "")
	// the tag moves upstream: served from the cache within metadataMaxAge
	m2, d2 := fr.putImage("app", "latest", []byte("l2"))
	e.now = e.now.Add(time.Hour)
	resp, b = e.do("GET", "/v2/hub/app/manifests/latest", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, m1) {
		t.Fatalf("within metadataMaxAge: %s", b)
	}
	// revalidated after it
	e.now = e.now.Add(24 * time.Hour)
	resp, b = e.do("GET", "/v2/hub/app/manifests/latest", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, m2) || resp.Header.Get("Docker-Content-Digest") != d2 {
		t.Fatalf("after metadataMaxAge: %v %s", resp.Header, b)
	}
	// by digest: never revalidated
	resp, b = e.do("GET", "/v2/hub/app/manifests/"+d1, nil)
	e.check(resp, b, 200, "")
	e.now = e.now.Add(48 * time.Hour)
	fr.set(func() { fr.status = 503 })
	resp, b = e.do("GET", "/v2/hub/app/manifests/"+d1, nil)
	e.check(resp, b, 200, "")
	if resp.Header.Get("Warning") != "" || !bytes.Equal(b, m1) {
		t.Fatalf("by digest during an outage: %v", resp.Header)
	}
	// a tag during an outage: the cached manifest, marked stale
	resp, b = e.do("GET", "/v2/hub/app/manifests/latest", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, m2) || !strings.Contains(resp.Header.Get("Warning"), "Stale") {
		t.Fatalf("stale tag: %v %s", resp.Header, b)
	}
	// nothing cached: 502
	resp, b = e.do("GET", "/v2/hub/other/manifests/latest", nil)
	e.check(resp, b, 502, "")
	fr.set(func() { fr.status = 0 })
}

func TestProxyUpstreamAuth(t *testing.T) {
	fr := newFakeRegistry(t)
	fr.bearer, fr.user, fr.pw = true, "hubuser", "hubsecret"
	fr.denied["private/x"] = true
	_, d1 := fr.putImage("app", "v1", []byte("l1"))
	e := newEnvWith(t,
		proxyOf(fr, "hub", &config.RemoteAuth{Username: "hubuser", Password: "hubsecret"}),
		proxyOf(fr, "anon", nil))

	resp, b := e.do("GET", "/v2/hub/app/manifests/v1", nil)
	e.check(resp, b, 200, "")
	resp2, b2 := e.do("GET", "/v2/hub/app/blobs/"+digestOf([]byte("l1")), nil)
	e.check(resp2, b2, 200, "")
	if fr.tokens != 1 {
		t.Fatalf("tokens issued: %d, want 1 (cached per scope)", fr.tokens)
	}
	// the upstream's token and our credentials never reach the client
	for _, r := range []*http.Response{resp, resp2} {
		dump := fmt.Sprint(r.Header)
		for _, secret := range []string{"upstream-token", "hubsecret", "hubuser"} {
			if strings.Contains(dump, secret) || strings.Contains(string(b), secret) {
				t.Fatalf("leaked %q: %s", secret, dump)
			}
		}
	}
	// the registry got the token, never the Basic credentials after it
	if last := fr.authSeen[len(fr.authSeen)-1]; !strings.HasPrefix(last, "Bearer upstream-token-") {
		t.Fatalf("upstream auth %q", last)
	}
	// wrong credentials: the token endpoint refuses, the manifest is not cached: 404
	resp, b = e.do("GET", "/v2/anon/app/manifests/v1", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	// a private image: denied upstream even with a token: not found here
	resp, b = e.do("GET", "/v2/hub/private/x/manifests/v1", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	// by digest it is the same
	resp, b = e.do("GET", "/v2/hub/private/x/manifests/"+d1, nil)
	e.check(resp, b, 404, CodeManifestUnknown)

	// anonymous token flow: no credentials demanded
	fr.set(func() { fr.user, fr.pw = "", "" })
	resp, b = e.do("GET", "/v2/anon/app/manifests/"+d1, nil)
	e.check(resp, b, 200, "")
}

func TestProxyBlobMismatch(t *testing.T) {
	fr := newFakeRegistry(t)
	fr.putImage("app", "v1", []byte("good layer"))
	e := newEnvWith(t, proxyOf(fr, "hub", nil))
	fr.set(func() { fr.corrupt = true })
	ld := digestOf([]byte("good layer"))
	resp, b := e.do("GET", "/v2/hub/app/blobs/"+ld, nil)
	e.check(resp, b, 502, "")
	// the mismatch is negative-cached until negativeCacheTTL passes
	fr.set(func() { fr.corrupt = false })
	resp, b = e.do("GET", "/v2/hub/app/blobs/"+ld, nil)
	e.check(resp, b, 404, CodeBlobUnknown)
	e.now = e.now.Add(11 * time.Minute)
	resp, b = e.do("GET", "/v2/hub/app/blobs/"+ld, nil)
	e.check(resp, b, 200, "")
	if string(b) != "good layer" {
		t.Fatalf("blob after the fix: %s", b)
	}
}

func TestProxyRateLimit(t *testing.T) {
	fr := newFakeRegistry(t)
	m1, _ := fr.putImage("app", "v1", []byte("l1"))
	e := newEnvWith(t, proxyOf(fr, "hub", nil))
	resp, b := e.do("GET", "/v2/hub/app/manifests/v1", nil)
	e.check(resp, b, 200, "")
	fr.set(func() { fr.status = 429 })
	resp, b = e.do("GET", "/v2/hub/app/manifests/v2", nil)
	e.check(resp, b, 429, CodeTooManyRequests)
	// a cached tag past metadataMaxAge is served stale instead
	e.now = e.now.Add(25 * time.Hour)
	resp, b = e.do("GET", "/v2/hub/app/manifests/v1", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, m1) || !strings.Contains(resp.Header.Get("Warning"), "Stale") {
		t.Fatalf("stale during a rate limit: %v", resp.Header)
	}
}

func TestProxyTagsAndWrites(t *testing.T) {
	fr := newFakeRegistry(t)
	fr.putImage("app", "v2", []byte("l2"))
	fr.putImage("app", "v1", []byte("l1"))
	fr.putImage("app", "latest", []byte("l2"))
	e := newEnvWith(t, proxyOf(fr, "hub", nil))
	resp, b := e.do("GET", "/v2/hub/app/tags/list", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"hub/app","tags":["latest","v1","v2"]}` {
		t.Fatalf("tags: %s", b)
	}
	fr.putImage("app", "v3", []byte("l3"))
	resp, b = e.do("GET", "/v2/hub/app/tags/list?n=2&last=v1", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"hub/app","tags":["v2"]}` || fr.count("/v2/app/tags/list") != 1 {
		t.Fatalf("cached tags page: %s (upstream served %d)", b, fr.count("/v2/app/tags/list"))
	}
	e.now = e.now.Add(25 * time.Hour)
	resp, b = e.do("GET", "/v2/hub/app/tags/list", nil)
	e.check(resp, b, 200, "")
	if !strings.Contains(string(b), `"v3"`) {
		t.Fatalf("tags after metadataMaxAge: %s", b)
	}
	resp, b = e.do("GET", "/v2/hub/nope/tags/list", nil)
	e.check(resp, b, 404, CodeNameUnknown)

	// writes: 405 UNSUPPORTED with an empty Allow, for a caller that could push to a hosted one
	for _, c := range []struct{ method, path string }{
		{"POST", "/v2/hub/app/blobs/uploads/"}, {"PUT", "/v2/hub/app/manifests/v1"},
		{"DELETE", "/v2/hub/app/manifests/" + digestOf([]byte("x"))}, {"PATCH", "/v2/hub/app/blobs/uploads/" + strings.Repeat("a", 32)},
	} {
		resp, b = e.do(c.method, c.path, []byte("{}"), basic("ci"))
		e.check(resp, b, 405, CodeUnsupported)
		if v, ok := resp.Header["Allow"]; !ok || len(v) != 1 || v[0] != "" {
			t.Fatalf("%s %s: Allow %q", c.method, c.path, v)
		}
	}
	// a token never carries push or delete on a proxy
	c, err := e.h.verify(e.token("ci", "repository:hub/app:pull,push,delete"))
	if err != nil || fmt.Sprint(c.Access) != "[{repository hub/app [pull]}]" {
		t.Fatalf("proxy token access %v %v", c.Access, err)
	}
	// the catalog lists what is cached: nothing until a manifest was pulled
	resp, b = e.do("GET", "/v2/_catalog", nil, basic("ci"))
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"repositories":[]}` {
		t.Fatalf("catalog before a pull: %s", b)
	}
	resp, b = e.do("GET", "/v2/hub/app/manifests/v3", nil)
	e.check(resp, b, 200, "")
	resp, b = e.do("GET", "/v2/_catalog", nil, basic("ci"))
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"repositories":["hub/app"]}` {
		t.Fatalf("catalog: %s", b)
	}
}

func TestUpstreamPath(t *testing.T) {
	for _, c := range []struct{ remote, image, want string }{
		{"https://registry-1.docker.io", "nginx", "v2/library/nginx/manifests/latest"},
		{"https://registry-1.docker.io/", "team/app", "v2/team/app/manifests/latest"},
		{"https://index.docker.io/v2/", "nginx", "library/nginx/manifests/latest"},
		{"https://ghcr.io", "nginx", "v2/nginx/manifests/latest"},
		{"http://mirror.local:5000/v2", "nginx", "nginx/manifests/latest"},
	} {
		u, _ := url.Parse(c.remote)
		if !strings.HasSuffix(u.Path, "/") {
			u.Path += "/"
		}
		rp := &repo.Repository{RemoteURL: u}
		if got := upstreamPath(rp, c.image, "/manifests/latest"); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.remote, c.image, got, c.want)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	ch, ok := parseChallenge([]string{`Basic realm="x"`, `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull,push",error="insufficient_scope"`})
	if !ok || ch.realm != "https://auth.docker.io/token" || ch.service != "registry.docker.io" || ch.scope != "repository:library/nginx:pull,push" {
		t.Fatalf("challenge %+v %v", ch, ok)
	}
	if _, ok := parseChallenge([]string{`Basic realm="x"`}); ok {
		t.Fatal("Basic parsed as Bearer")
	}
}

func TestGroup(t *testing.T) {
	fr := newFakeRegistry(t)
	mWebUp, _ := fr.putImage("web", "latest", []byte("web layer"))
	mAppUp, _ := fr.putImage("app", "v1", []byte("upstream app layer")) // the hosted member has app too
	fr.putImage("app", "v2", []byte("only upstream"))
	tr, f := true, false
	e := newEnvWith(t,
		proxyOf(fr, "hub", nil),
		config.Repository{Name: "down", Format: "docker", Type: "hosted", Online: &f, AnonymousRead: &tr},
		config.Repository{Name: "all", Format: "docker", Type: "group", Members: []string{"down", "dh", "hub"}, AnonymousRead: &tr},
		config.Repository{Name: "dead", Format: "docker", Type: "group", Members: []string{"down"}, AnonymousRead: &tr},
		config.Repository{Name: "nested", Format: "docker", Type: "group", Members: []string{"all"}, AnonymousRead: &f})
	hostedLayer := []byte("hosted app layer")
	mApp, dApp := e.pushImage("dh/app", "v1", [][]byte{hostedLayer}, basic("ci"))

	// owner rule: the hosted member has app, so its manifest and blobs
	// answer, and what only the proxy has under app is not found
	resp, b := e.do("GET", "/v2/all/app/manifests/v1", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, mApp) || resp.Header.Get("Docker-Content-Digest") != dApp {
		t.Fatalf("group manifest: %v %s", resp.Header, b)
	}
	resp, b = e.do("GET", "/v2/all/app/blobs/"+digestOf(hostedLayer), nil)
	e.check(resp, b, 200, "")
	resp, b = e.do("GET", "/v2/all/app/blobs/"+digestOf([]byte("upstream app layer")), nil)
	e.check(resp, b, 404, CodeBlobUnknown)
	resp, b = e.do("GET", "/v2/all/app/manifests/v2", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	resp, b = e.do("GET", "/v2/all/app/manifests/"+digestOf(mAppUp), nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	if fr.count("/v2/app/manifests/v1") != 0 {
		t.Fatal("the proxy was asked for an image the hosted member owns")
	}
	// an image only the proxy has: fetched through it (the offline member ahead is skipped)
	resp, b = e.do("GET", "/v2/all/web/manifests/latest", nil)
	e.check(resp, b, 200, "")
	if !bytes.Equal(b, mWebUp) {
		t.Fatalf("group proxy manifest: %s", b)
	}
	resp, b = e.do("GET", "/v2/all/web/blobs/"+digestOf([]byte("web layer")), nil)
	e.check(resp, b, 200, "")
	// nested group
	resp, b = e.do("GET", "/v2/nested/web/manifests/latest", nil, basic("reader"))
	e.check(resp, b, 200, "")
	// tags come from the owner
	resp, b = e.do("GET", "/v2/all/app/tags/list", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"all/app","tags":["v1"]}` {
		t.Fatalf("group tags: %s", b)
	}
	resp, b = e.do("GET", "/v2/all/web/tags/list", nil)
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"name":"all/web","tags":["latest"]}` {
		t.Fatalf("group proxy tags: %s", b)
	}
	// catalog: the union, filtered by what the caller may read
	e.pushImage("priv/sec", "v1", nil, basic("ci"))
	resp, b = e.do("GET", "/v2/_catalog", nil, bearer(e.token("", "registry:catalog:*")))
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"repositories":["all/app","all/web","dh/app","hub/web"]}` {
		t.Fatalf("anonymous catalog: %s", b)
	}
	resp, b = e.do("GET", "/v2/_catalog", nil, basic("ci"))
	e.check(resp, b, 200, "")
	if string(bytes.TrimSpace(b)) != `{"repositories":["all/app","all/web","dh/app","hub/web","nested/app","nested/web","priv/sec"]}` {
		t.Fatalf("ci catalog: %s", b)
	}
	// every member offline: 503
	resp, b = e.do("GET", "/v2/dead/app/manifests/v1", nil)
	e.check(resp, b, 503, "")
	// a failing member ahead of the answer: 5xx, never a silent skip
	fr.set(func() { fr.status = 503 })
	resp, b = e.do("GET", "/v2/all/other/manifests/v1", nil)
	e.check(resp, b, 502, "")
	fr.set(func() { fr.status = 0 })
	// writes: 405 UNSUPPORTED, empty Allow; push is never granted
	resp, b = e.do("POST", "/v2/all/app/blobs/uploads/", nil, basic("ci"))
	e.check(resp, b, 405, CodeUnsupported)
	if v, ok := resp.Header["Allow"]; !ok || v[0] != "" {
		t.Fatalf("Allow %q", v)
	}
	resp, b = e.do("PUT", "/v2/all/app/manifests/v1", mApp, basic("ci"), hdr("Content-Type", mtSchema2))
	e.check(resp, b, 405, CodeUnsupported)
	c, err := e.h.verify(e.token("ci", "repository:all/app:pull,push,delete", "repository:dh/app:pull,push"))
	if err != nil || fmt.Sprint(c.Access) != "[{repository all/app [pull]} {repository dh/app [pull push]}]" {
		t.Fatalf("group token access %v %v", c.Access, err)
	}
	// pull with that token works; push through it is refused by the token
	resp, b = e.do("GET", "/v2/all/app/manifests/v1", nil, bearer(e.h.mint(*c)))
	e.check(resp, b, 200, "")
	resp, b = e.do("POST", "/v2/all/app/blobs/uploads/", nil, bearer(e.h.mint(*c)))
	e.check(resp, b, 403, CodeDenied)
}

// A transient failure of the upstream token service is a remote failure
// (5xx, no negative-cache row), not the registry's 401 turned into a
// not-found that outlives the outage.
func TestTokenOutageIsNotNegativeCached(t *testing.T) {
	fr := &fakeRegistry{manifests: map[string]fakeManifest{}, blobs: map[string][]byte{}, served: map[string]int{}, denied: map[string]bool{}}
	var tokenDown atomic.Bool
	fr.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" && tokenDown.Load() {
			http.Error(w, "auth service down", http.StatusServiceUnavailable)
			return
		}
		fr.ServeHTTP(w, r)
	}))
	t.Cleanup(fr.ts.Close)
	fr.bearer = true
	fr.putImage("team/app", "v1", []byte("layer one"))
	e := newEnvWith(t, proxyOf(fr, "hub", nil))

	tokenDown.Store(true)
	resp, b := e.do("GET", "/v2/hub/team/app/manifests/v1", nil)
	e.check(resp, b, 502, "")
	tokenDown.Store(false)
	resp, b = e.do("GET", "/v2/hub/team/app/manifests/v1", nil)
	e.check(resp, b, 200, "")
}

// The remote's 429 reaches the client through a group too, not as a 502.
func TestGroupPassesRateLimitThrough(t *testing.T) {
	fr := newFakeRegistry(t)
	fr.putImage("team/app", "v1", []byte("layer one"))
	tr := true
	e := newEnvWith(t, proxyOf(fr, "hub", nil),
		config.Repository{Name: "grp", Format: "docker", Type: "group", Members: []string{"hub"}, AnonymousRead: &tr})
	fr.set(func() { fr.status = 429 })
	resp, b := e.do("GET", "/v2/hub/team/app/manifests/v1", nil)
	e.check(resp, b, 429, CodeTooManyRequests)
	resp, b = e.do("GET", "/v2/grp/team/app/manifests/v1", nil)
	e.check(resp, b, 429, CodeTooManyRequests)
}

// The owner rule holds through a nested group: the inner group's hosted
// member owns the image, so the outer group never falls through to a proxy
// that has the tag.
func TestNestedGroupOwnerRule(t *testing.T) {
	fr := newFakeRegistry(t)
	fr.putImage("app", "v2", []byte("upstream layer"))
	tr := true
	e := newEnvWith(t, proxyOf(fr, "hub", nil),
		config.Repository{Name: "inner", Format: "docker", Type: "group", Members: []string{"dh"}, AnonymousRead: &tr},
		config.Repository{Name: "outer", Format: "docker", Type: "group", Members: []string{"inner", "hub"}, AnonymousRead: &tr})
	e.pushImage("dh/app", "v1", [][]byte{[]byte("l")}, basic("ci"))
	resp, b := e.do("GET", "/v2/inner/app/manifests/v2", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	resp, b = e.do("GET", "/v2/outer/app/manifests/v2", nil)
	e.check(resp, b, 404, CodeManifestUnknown)
	if fr.count("/v2/app/manifests/v2") != 0 {
		t.Fatal("the proxy was asked for an image a nested hosted member owns")
	}
	// what no hosted member owns still comes from the proxy
	fr.putImage("web", "latest", []byte("web layer"))
	resp, b = e.do("GET", "/v2/outer/web/manifests/latest", nil)
	e.check(resp, b, 200, "")
}
