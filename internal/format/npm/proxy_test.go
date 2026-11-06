package npm

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// answer is what a scripted remote gives for a path.
type answer struct {
	status      int // 0: 200
	ctype, body string
}

// scripted is a registry whose answers a test sets while it runs; asked
// lists the paths it was asked for, query and uri are the last request's.
type scripted struct {
	mu         sync.Mutex
	answers    map[string]answer
	asked      []string
	query, uri string
	url        string
}

func (s *scripted) set(path string, a answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[path] = a
}

func (s *scripted) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.asked {
		if p == path {
			n++
		}
	}
	return n
}

// scriptedMember is an npm proxy member named name, with npm-proxy's
// settings, on a scripted remote. A path without an answer is a 404.
func scriptedMember(t *testing.T, e *env, name string) (*repo.Repository, *scripted) {
	t.Helper()
	s := &scripted{answers: map[string]answer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked, s.query, s.uri = append(s.asked, r.URL.Path), r.URL.RawQuery, r.RequestURI
		a, ok := s.answers[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", a.ctype)
		if a.status != 0 {
			w.WriteHeader(a.status)
		}
		io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	def := e.repos["npm-proxy"].Repository
	def.Name, def.Remote = name, srv.URL
	m := repo.Build(&config.Config{Repositories: []config.Repository{def}})[name]
	e.repos[name] = m
	return m, s
}

// docOf is the package document of name with one version per entry of
// tarballs (version to tarball URL and content), the last being latest.
func docOf(name string, versions ...[3]string) answer {
	vs, times, latest := map[string]any{}, map[string]any{"created": "2020-01-01T00:00:00.000Z"}, ""
	for _, v := range versions {
		sum := sha1.Sum([]byte(v[2]))
		vs[v[0]] = map[string]any{"name": name, "version": v[0], "dist": map[string]any{"tarball": v[1], "shasum": hex.EncodeToString(sum[:])}}
		times[v[0]] = "2020-01-0" + v[0][:1] + "T00:00:00.000Z"
		latest = v[0]
	}
	b, _ := json.Marshal(map[string]any{"_id": name, "_rev": "7-abc", "name": name, "dist-tags": map[string]string{"latest": latest}, "versions": vs, "time": times})
	return answer{ctype: "application/json", body: string(b)}
}

func tgzAnswer(body string) answer { return answer{ctype: "application/octet-stream", body: body} }

// afterMetadataAge moves the fetcher's clock past the proxies' metadataMaxAge.
func afterMetadataAge(e *env) {
	e.h.Resolver.Fetcher.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
}

// npm-05: a remote that keeps tarballs elsewhere than <name>/-/<file>.tgz is
// proxied by the URL its package document names: GitHub Packages ends it in
// a hash, GitLab puts the scope into the file's path. The file is cached
// under the request path, which is also what a routing rule sees. A URL that
// ends in a hash is offered under the usual name (the file is then a
// component like any other); the hash, which a lockfile written through
// Nexus holds, is answered too.
func TestProxyTarballFromTheURLTheDocumentNames(t *testing.T) {
	e := newEnv(t)
	hub, up := scriptedMember(t, e, "npm-hub")
	groupOf(e, "g-hub", e.repos["npm-hosted"], hub)
	p, q := string(testTarball("@o/p", "1.0.0", []byte("p"))), string(testTarball("@o/q", "1.0.0", []byte("q")))
	up.set("/@o/p", docOf("@o/p", [3]string{"1.0.0", up.url + "/download/@o/p/1.0.0/0123abcd", p}))
	up.set("/download/@o/p/1.0.0/0123abcd", tgzAnswer(p))
	up.set("/@o/q", docOf("@o/q", [3]string{"1.0.0", up.url + "/api/@o/q/-/@o/q-1.0.0.tgz", q}))
	up.set("/api/@o/q/-/@o/q-1.0.0.tgz", tgzAnswer(q))
	var routed []string
	e.h.Resolver.Fetcher.RouteAllowed = func(_ *repo.Repository, path string) bool {
		routed = append(routed, path)
		return path != "/@o/q/-/blocked.tgz"
	}

	m := jsonBody(t, e.do("GET", "g-hub", "@o/p", "", anon))
	if got := get(m, "versions", "1.0.0", "dist", "tarball"); got != base+"/repository/g-hub/@o/p/-/p-1.0.0.tgz" {
		t.Fatalf("tarball URL of a hash-named file = %v", got)
	}
	if v := jsonBody(t, e.do("GET", "g-hub", "@o/p/1.0.0", "", anon)); get(v, "dist", "tarball") != base+"/repository/g-hub/@o/p/-/p-1.0.0.tgz" {
		t.Fatalf("version document: %v", v)
	}
	for _, c := range []struct{ repo, path, body, from string }{
		{"g-hub", "@o/p/-/p-1.0.0.tgz", p, "/download/@o/p/1.0.0/0123abcd"},
		{"npm-hub", "@o/q/-/q-1.0.0.tgz", q, "/api/@o/q/-/@o/q-1.0.0.tgz"},
	} {
		for i := 0; i < 2; i++ { // the second time from the cache
			if w := e.do("GET", c.repo, c.path, "", anon); w.Code != 200 || w.Body.String() != c.body {
				t.Fatalf("%s/%s (%d): %d %q", c.repo, c.path, i, w.Code, w.Body.String())
			}
		}
		if n := up.count(c.from); n != 1 {
			t.Errorf("%s was fetched %d times, want once", c.from, n)
		}
		if a, err := e.h.Store.Get(context.Background(), "npm-hub", c.path); err != nil || a.Size != int64(len(c.body)) {
			t.Errorf("%s is not cached under the request path: %+v %v", c.path, a, err)
		}
	}
	// A file named as a tarball is asked for at the usual place first (an
	// install from a lockfile then needs no document).
	if n := up.count("/@o/q/-/q-1.0.0.tgz"); n != 1 {
		t.Errorf("requests at the usual place: %d, want 1", n)
	}
	// The hash stands for the version's usual name: the same cached file.
	for _, repoName := range []string{"npm-hub", "g-hub"} {
		if w := e.do("GET", repoName, "@o/p/-/0123abcd", "", anon); w.Code != 200 || w.Body.String() != p {
			t.Fatalf("%s by the hash: %d %q", repoName, w.Code, w.Body.String())
		}
	}
	if a, b := up.count("/@o/p/-/0123abcd"), up.count("/download/@o/p/1.0.0/0123abcd"); a != 0 || b != 1 {
		t.Errorf("by the hash: the remote was asked %d times for it and %d times for the file", a, b)
	}
	if _, err := e.h.Store.Get(context.Background(), "npm-hub", "@o/p/-/0123abcd"); repo.StatusOf(err) != 404 {
		t.Errorf("cached under the hash too: %v", err)
	}
	for _, path := range routed {
		if strings.HasPrefix(path, "/download/") || strings.HasPrefix(path, "/api/") {
			t.Errorf("the routing rule saw the upstream path %s", path)
		}
	}
	if !slices.Contains(routed, "/@o/p/-/p-1.0.0.tgz") || !slices.Contains(routed, "/@o/q/-/q-1.0.0.tgz") {
		t.Errorf("the routing rule did not see the request paths: %v", routed)
	}
	// A file the document does not name, and one the routing rule refuses.
	for _, path := range []string{"@o/p/-/ffff", "@o/q/-/q-9.9.9.tgz", "@o/q/-/blocked.tgz", "@o/none/-/none-1.0.0.tgz"} {
		if w := e.do("GET", "npm-hub", path, "", anon); w.Code != 404 {
			t.Errorf("%s: %d %s, want 404", path, w.Code, w.Body.String())
		}
	}
}

// npm-07: a sign-in or error page a remote answers with 200 is never cached
// as a package document or a tarball: the request fails, a copy cached
// before keeps answering, and the next correct answer is served. In a group
// such a member fails the request; the next member does not stand in.
func TestProxyDoesNotCacheWebPages(t *testing.T) {
	e := newEnv(t)
	odd, up := scriptedMember(t, e, "npm-odd")
	groupOf(e, "g-odd", odd, e.repos["npm-proxy"])
	ctx := context.Background()
	tgz := string(testTarball("left-pad", "1.1.0", []byte("x")))
	good := docOf("left-pad", [3]string{"1.1.0", up.url + "/left-pad/-/left-pad-1.1.0.tgz", tgz})
	page := "<!DOCTYPE html><html><body>Sign in</body></html>"

	for _, bad := range []answer{{ctype: "text/html", body: page}, {ctype: "application/json", body: page},
		{ctype: "application/json", body: `{"error":"maintenance"}`}, {ctype: "application/json", body: `["left-pad"]`}} {
		up.set("/left-pad", bad)
		for _, repoName := range []string{"npm-odd", "g-odd"} {
			if w := e.do("GET", repoName, "left-pad", "", anon); w.Code != 502 {
				t.Fatalf("%s, document %q as %s: %d %s, want 502", repoName, bad.body, bad.ctype, w.Code, w.Body.String())
			}
		}
		if _, err := e.h.Store.Get(ctx, "npm-odd", PackumentKey("left-pad")); repo.StatusOf(err) != 404 {
			t.Fatalf("document %q as %s was cached: %v", bad.body, bad.ctype, err)
		}
	}
	up.set("/left-pad", good)
	if w := e.do("GET", "g-odd", "left-pad", "", anon); w.Code != 200 || get(jsonBody(t, w), "versions", "1.1.0") == nil {
		t.Fatalf("the correct document after the pages: %d %s", w.Code, w.Body.String())
	}
	cached, _ := e.h.Store.Get(ctx, "npm-odd", PackumentKey("left-pad"))

	// The tarball: a page, whatever the remote declares it as, a JSON error,
	// and a file that is not the one the cached document promises. Nothing
	// is recorded as missing either: the correct file is served at once.
	for _, bad := range []answer{{ctype: "text/html", body: page}, tgzAnswer(page), tgzAnswer(`{"error":"maintenance"}`), tgzAnswer(tgz + "x")} {
		up.set("/"+tgz110, bad)
		if w := e.do("GET", "npm-odd", tgz110, "", anon); w.Code != 502 {
			t.Fatalf("tarball %q as %s: %d %s, want 502", bad.body[:8], bad.ctype, w.Code, w.Body.String())
		}
		if _, err := e.h.Store.Get(ctx, "npm-odd", tgz110); repo.StatusOf(err) != 404 {
			t.Fatalf("tarball %q as %s was cached: %v", bad.body[:8], bad.ctype, err)
		}
	}
	up.set("/"+tgz110, tgzAnswer(tgz))
	if w := e.do("GET", "npm-odd", tgz110, "", anon); w.Code != 200 || w.Body.String() != tgz {
		t.Fatalf("the correct tarball after the pages: %d", w.Code)
	}

	// With a cached copy the page is a failed refresh: the copy answers.
	afterMetadataAge(e)
	up.set("/left-pad", answer{ctype: "application/json", body: page})
	w := e.do("GET", "npm-odd", "left-pad", "", anon)
	if w.Code != 200 || get(jsonBody(t, w), "versions", "1.1.0") == nil || w.Header().Get("Warning") == "" {
		t.Fatalf("cached document during the page: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if now, err := e.h.Store.Get(ctx, "npm-odd", PackumentKey("left-pad")); err != nil || now.BlobSHA256 != cached.BlobSHA256 {
		t.Fatalf("the cached document was replaced by the page: %+v %v", now, err)
	}
}

// A refused answer leaves nothing behind for a crash, a webhook or the reconcile
// journal to find: no row is written at any moment. A refresh that keeps a
// version the remote dropped writes the merged document once, never the
// remote's in between.
func TestProxyStoresOnlyWhatItKeeps(t *testing.T) {
	e := newEnv(t)
	m, up := scriptedMember(t, e, "npm-once")
	var commits []meta.Change
	e.h.Store.DB.OnCommit = func(_ context.Context, _ time.Time, cs []meta.Change) { commits = append(commits, cs...) }
	tgz := func(v string) string { return string(testTarball("p", v, []byte(v))) }
	url := func(v string) string { return up.url + "/p/-/p-" + v + ".tgz" }
	page := answer{ctype: "application/json", body: "<!DOCTYPE html><html><body>Sign in</body></html>"}

	up.set("/p", page)
	up.set("/p/-/p-1.0.0.tgz", page)
	for _, path := range []string{"p", "p/-/p-1.0.0.tgz"} {
		if w := e.do("GET", m.Name, path, "", anon); w.Code != 502 {
			t.Fatalf("%s during the page: %d", path, w.Code)
		}
	}
	if len(commits) != 0 {
		t.Fatalf("a refused answer wrote %+v", commits)
	}

	up.set("/p", docOf("p", [3]string{"1.0.0", url("1.0.0"), tgz("1.0.0")}, [3]string{"2.0.0", url("2.0.0"), tgz("2.0.0")}))
	up.set("/p/-/p-1.0.0.tgz", tgzAnswer(tgz("1.0.0")))
	for _, path := range []string{"p", "p/-/p-1.0.0.tgz"} {
		if w := e.do("GET", m.Name, path, "", anon); w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	commits = nil
	afterMetadataAge(e)
	up.set("/p", docOf("p", [3]string{"2.0.0", url("2.0.0"), tgz("2.0.0")}))
	if m := jsonBody(t, e.do("GET", m.Name, "p", "", anon)); get(m, "versions", "1.0.0") == nil {
		t.Fatalf("the cached version went: %v", m)
	}
	stored, err := e.h.Store.Get(context.Background(), m.Name, PackumentKey("p"))
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].Op != meta.ChangePut || commits[0].Asset.SHA256 != stored.BlobSHA256 {
		t.Fatalf("the refresh wrote %+v, want the merged document once (%s)", commits, stored.BlobSHA256)
	}
}

// npm-06: a version the remote no longer lists stays in the proxy's document
// while this proxy has its tarball, with the manifest it had; one that was
// never fetched goes. A package the remote dropped altogether (404) keeps
// answering from the cache, and the remote is asked again only after the
// metadata age.
func TestProxyKeepsCachedVersionsTheRemoteDropped(t *testing.T) {
	e := newEnv(t)
	keep, up := scriptedMember(t, e, "npm-keep")
	groupOf(e, "g-keep", e.repos["npm-hosted"], keep)
	ctx := context.Background()
	tgz := func(v string) string { return string(testTarball("p", v, []byte(v))) }
	url := func(v string) string { return up.url + "/p/-/p-" + v + ".tgz" }
	up.set("/p", docOf("p", [3]string{"1.0.0", url("1.0.0"), tgz("1.0.0")}, [3]string{"2.0.0", url("2.0.0"), tgz("2.0.0")}, [3]string{"3.0.0", url("3.0.0"), tgz("3.0.0")}))
	up.set("/p/-/p-2.0.0.tgz", tgzAnswer(tgz("2.0.0")))
	for _, path := range []string{"p", "p/-/p-2.0.0.tgz"} {
		if w := e.do("GET", "g-keep", path, "", anon); w.Code != 200 {
			t.Fatalf("caching %s: %d %s", path, w.Code, w.Body.String())
		}
	}

	// The remote unpublishes 2.0.0 and 3.0.0; only 2.0.0 was fetched here.
	up.set("/p", docOf("p", [3]string{"1.0.0", url("1.0.0"), tgz("1.0.0")}))
	if m := jsonBody(t, e.do("GET", "npm-keep", "p", "", anon)); get(m, "versions", "3.0.0") == nil {
		t.Fatalf("inside the metadata age the cached document answers: %v", m)
	}
	afterMetadataAge(e)
	for i := 0; i < 2; i++ { // the second read is the stored result
		m := jsonBody(t, e.do("GET", "g-keep", "p", "", anon))
		vs, _ := get(m, "versions").(map[string]any)
		if len(vs) != 2 || vs["1.0.0"] == nil || get(m, "versions", "2.0.0", "dist", "shasum") == nil {
			t.Fatalf("read %d after the remote dropped two versions: %v", i, m)
		}
		if get(m, "versions", "2.0.0", "dist", "tarball") != base+"/repository/g-keep/p/-/p-2.0.0.tgz" || get(m, "time", "2.0.0") != "2020-01-02T00:00:00.000Z" || get(m, "time", "3.0.0") != nil {
			t.Fatalf("read %d: the kept version's tarball URL or time: %v", i, m)
		}
		if get(m, "dist-tags", "latest") != "1.0.0" {
			t.Fatalf("read %d: latest = %v, want the remote's 1.0.0", i, get(m, "dist-tags", "latest"))
		}
	}
	if w := e.do("GET", "g-keep", "p/2.0.0", "", anon); w.Code != 200 {
		t.Fatalf("version document of the kept version: %d %s", w.Code, w.Body.String())
	}

	// The remote leaves a stub of the unpublished package, as
	// registry.npmjs.org does: the cached version is all there is, and latest.
	e.h.Resolver.Fetcher.Now = func() time.Time { return time.Now().Add(4 * time.Hour) }
	up.set("/p", answer{ctype: "application/json", body: `{"_id":"p","name":"p","time":{"created":"2020-01-01T00:00:00.000Z","unpublished":{"time":"2026-01-01T00:00:00.000Z"}}}`})
	m := jsonBody(t, e.do("GET", "npm-keep", "p", "", anon))
	if vs, _ := get(m, "versions").(map[string]any); len(vs) != 1 || vs["2.0.0"] == nil || get(m, "dist-tags", "latest") != "2.0.0" || get(m, "time", "unpublished") != nil {
		t.Fatalf("after the remote unpublished the package: %v", m)
	}

	// Then the remote answers 404 for the package: document and tarball stay.
	e.h.Resolver.Fetcher.Now = func() time.Time { return time.Now().Add(6 * time.Hour) }
	up.mu.Lock()
	up.answers = map[string]answer{}
	up.mu.Unlock()
	asked := up.count("/p")
	for i := 0; i < 2; i++ {
		w := e.do("GET", "g-keep", "p", "", anon)
		if w.Code != 200 || get(jsonBody(t, w), "versions", "2.0.0") == nil {
			t.Fatalf("read %d after the remote dropped the package: %d %s", i, w.Code, w.Body.String())
		}
		if w := e.do("GET", "g-keep", "p/-/p-2.0.0.tgz", "", anon); w.Code != 200 || w.Body.String() != tgz("2.0.0") {
			t.Fatalf("tarball after the remote dropped the package: %d", w.Code)
		}
	}
	if n := up.count("/p") - asked; n < 1 { // asked again each time, as for any file the remote lost
		t.Fatalf("the remote was not asked about the dropped package")
	}
	// What was never cached is not found, and a routing rule still decides.
	if w := e.do("GET", "npm-keep", "gone", "", anon); w.Code != 404 {
		t.Fatalf("a package the remote never had: %d", w.Code)
	}
	e.h.Resolver.Fetcher.RouteAllowed = func(*repo.Repository, string) bool { return false }
	for _, path := range []string{"p", "p/-/p-2.0.0.tgz"} {
		if w := e.do("GET", "npm-keep", path, "", anon); w.Code != 404 {
			t.Fatalf("%s refused by the routing rule: %d, want 404", path, w.Code)
		}
	}
	if _, err := e.h.Store.Get(ctx, "npm-keep", PackumentKey("p")); err != nil {
		t.Fatalf("the cached document is gone: %v", err)
	}
}

// npm-09: a scoped package's document is asked for as the npm client asks,
// with the slash encoded; a routing rule still sees the name.
func TestProxyAsksForScopedNamesEncoded(t *testing.T) {
	e := newEnv(t)
	m, up := scriptedMember(t, e, "npm-enc")
	up.set("/@s/p", docOf("@s/p", [3]string{"1.0.0", up.url + "/@s/p/-/p-1.0.0.tgz", "x"}))
	var routed []string
	e.h.Resolver.Fetcher.RouteAllowed = func(_ *repo.Repository, path string) bool {
		routed = append(routed, path)
		return true
	}
	if w := e.do("GET", m.Name, "@s/p", "", anon); w.Code != 200 {
		t.Fatalf("document: %d %s", w.Code, w.Body.String())
	}
	if up.uri != "/@s%2fp" {
		t.Fatalf("the remote was asked for %q", up.uri)
	}
	if !slices.Contains(routed, "/@s/p") || slices.ContainsFunc(routed, func(p string) bool { return strings.Contains(p, "%") }) {
		t.Fatalf("the routing rule saw %v", routed)
	}
}

// npm-07 with requests at the same time: while a remote answers with a web
// page, every request for a tarball that is not cached fails and every
// request for a document gets the copy cached before, never the page, and the
// page stays nowhere.
func TestProxyWebPageUnderLoad(t *testing.T) {
	e := newEnv(t)
	m, up := scriptedMember(t, e, "npm-load")
	ctx := context.Background()
	tgz := string(testTarball("left-pad", "1.1.0", []byte("x")))
	up.set("/left-pad", docOf("left-pad", [3]string{"1.1.0", up.url + "/" + tgz110, tgz}))
	if w := e.do("GET", m.Name, "left-pad", "", anon); w.Code != 200 {
		t.Fatalf("caching: %d %s", w.Code, w.Body.String())
	}
	good, err := e.h.Store.Get(ctx, m.Name, PackumentKey("left-pad"))
	if err != nil {
		t.Fatal(err)
	}
	page := answer{ctype: "application/json", body: "<!DOCTYPE html><html><body>Sign in</body></html>"}
	load := func(check func() *httptest.ResponseRecorder, what string, code int) {
		t.Helper()
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if w := check(); w.Code != code {
					t.Errorf("%s during the page: %d %q, want %d", what, w.Code, w.Body.String(), code)
				}
			}()
		}
		wg.Wait()
	}
	up.set("/"+tgz110, page)
	load(func() *httptest.ResponseRecorder { return e.do("GET", m.Name, tgz110, "", anon) }, "tarball", 502)
	if _, err := e.h.Store.Get(ctx, m.Name, tgz110); repo.StatusOf(err) != 404 {
		t.Fatalf("the tarball after the page: %v", err)
	}
	up.set("/"+tgz110, tgzAnswer(tgz))
	if w := e.do("GET", m.Name, tgz110, "", anon); w.Code != 200 || w.Body.String() != tgz {
		t.Fatalf("the tarball when the remote is back: %d", w.Code)
	}

	afterMetadataAge(e)
	up.set("/left-pad", page)
	load(func() *httptest.ResponseRecorder {
		w := e.do("GET", m.Name, "left-pad", "", anon)
		if !strings.Contains(w.Body.String(), `"1.1.0"`) {
			w.Code = 0
		}
		return w
	}, "document", 200)
	if a, err := e.h.Store.Get(ctx, m.Name, PackumentKey("left-pad")); err != nil || a.BlobSHA256 != good.BlobSHA256 {
		t.Fatalf("the document after the page: %+v %v", a, err)
	}
}
