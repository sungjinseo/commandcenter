package npm

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// publishWith publishes name@version to npm-hosted with fields added to its manifest.
func publishWith(t *testing.T, e *env, name, version string, fields map[string]any) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(publishBody(name, version, []byte(version), false)), &doc); err != nil {
		t.Fatal(err)
	}
	man := get(doc, "versions", version).(map[string]any)
	for k, v := range fields {
		man[k] = v
	}
	b, _ := json.Marshal(doc)
	if w := e.do("PUT", "npm-hosted", name, string(b), ci); w.Code != 201 {
		t.Fatalf("publish %s@%s: %d %s", name, version, w.Code, w.Body.String())
	}
}

// search runs GET -/v1/search?<query> on repoName and returns the answer
// and the names it found, in order.
func search(t *testing.T, e *env, repoName, query string) (map[string]any, []string) {
	t.Helper()
	return searchAs(t, e, ci, repoName, query)
}

func searchAs(t *testing.T, e *env, p auth.Principal, repoName, query string) (map[string]any, []string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/repository/"+repoName+"/-/v1/search?"+query, nil)
	e.h.Serve(w, r.WithContext(auth.WithPrincipal(r.Context(), p)), e.repos[repoName], "-/v1/search")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("search %s?%s: %d %v %s", repoName, query, w.Code, w.Header(), w.Body.String())
	}
	m := jsonBody(t, w)
	names := []string{}
	for _, o := range m["objects"].([]any) {
		names = append(names, get(o.(map[string]any), "package", "name").(string))
	}
	return m, names
}

// npm-14: `npm search` (GET /-/v1/search). A hosted repository finds its
// packages by name, description and keywords of their latest version, with
// Nexus's qualifiers, and answers in the registry API's shape.
func TestSearchHosted(t *testing.T) {
	e := newEnv(t)
	publishWith(t, e, "@acme/logger", "0.9.0", map[string]any{"description": "Structured LOGGING", "keywords": []string{"log", "json"},
		"author": "Ann Example <ann@example.com> (https://example.com)", "homepage": "https://example.com/logger",
		"repository": map[string]any{"type": "git", "url": "git+https://example.com/logger.git"}, "bugs": map[string]any{"url": "https://example.com/bugs"}})
	publishWith(t, e, "@acme/logger", "1.2.0", map[string]any{"description": "Structured LOGGING", "keywords": []string{"log", "json"}, "author": map[string]any{"name": "Ann Example", "email": "ann@example.com"}})
	publishWith(t, e, "left-pad", "0.3.0", map[string]any{"description": "pads strings", "keywords": "string, pad", "maintainers": []any{map[string]any{"name": "bob", "email": "bob@example.com"}}})
	publishWith(t, e, "zeta", "2.0.0", nil)

	for query, want := range map[string]string{
		"text=logg":                         "@acme/logger", // the name
		"text=LOGGING":                      "@acme/logger", // the description, any case
		"text=json":                         "@acme/logger", // a keyword
		"text=pad":                          "left-pad",
		"text=test":                         "zeta", // publishBody's description
		"text=a":                            "@acme/logger left-pad zeta",
		"text=a&size=1":                     "@acme/logger",
		"text=a&size=2&from=1":              "left-pad zeta",
		"text=a&from=5":                     "",
		"text=":                             "",
		"text=nothing-like-it":              "",
		"text=logger+pad":                   "", // every word must match
		"text=keywords:pad":                 "left-pad",
		"text=author:ann":                   "@acme/logger",
		"text=maintainer:bob":               "left-pad",
		"text=scope:acme":                   "@acme/logger",
		"text=scope:@acme+json":             "@acme/logger",
		"text=is:unstable":                  "left-pad",
		"text=not:unstable":                 "@acme/logger zeta",
		"text=not:unstable+keywords:string": "",
	} {
		if _, names := search(t, e, "npm-hosted", query); strings.Join(names, " ") != want {
			t.Errorf("%s: found %v, want %q", query, names, want)
		}
	}

	m, _ := search(t, e, "npm-hosted", "text=logger")
	b, _ := json.Marshal(m["objects"].([]any)[0])
	want := `{"package":{"date":"` + get(jsonBody(t, e.do("GET", "npm-hosted", "@acme/logger", "", anon)), "time", "1.2.0").(string) + `",` +
		`"description":"Structured LOGGING","keywords":["log","json"],"links":{"npm":"http://repo.test/repository/npm-hosted/@acme/logger"},` +
		`"maintainers":[{"email":"ann@example.com","username":"Ann Example"}],"name":"@acme/logger","publisher":{"username":"ci"},"version":"1.2.0"},` +
		`"score":{"detail":{"maintenance":0,"popularity":0,"quality":0},"final":0},"searchScore":0}`
	if string(b) != want {
		t.Fatalf("search object:\n%s\nwant\n%s", b, want)
	}
	if m["total"] != float64(1) || m["time"] == "" {
		t.Fatalf("total and time: %v %v", m["total"], m["time"])
	}
	// The older version's links show what the manifest's fields turn into.
	e.do("DELETE", "npm-hosted", "@acme/logger/-/logger-1.2.0.tgz", "", ci)
	m, _ = search(t, e, "npm-hosted", "text=logger")
	b, _ = json.Marshal(get(m["objects"].([]any)[0].(map[string]any), "package", "links"))
	if string(b) != `{"bugs":"https://example.com/bugs","homepage":"https://example.com/logger","npm":"http://repo.test/repository/npm-hosted/@acme/logger","repository":"git+https://example.com/logger.git"}` {
		t.Fatalf("links = %s", b)
	}
	if w := e.do("POST", "npm-hosted", "-/v1/search", "", anon); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
	// Not ASCII, and what PostgreSQL cannot store.
	publishWith(t, e, "uni", "1.0.0", map[string]any{"description": "ÉTÉ 한글"})
	if _, names := search(t, e, "npm-hosted", "text=%C3%89T%C3%89"); strings.Join(names, " ") != "uni" {
		t.Fatalf("found %v", names)
	}
	if _, names := search(t, e, "npm-hosted", "text=a%00b"); len(names) != 0 {
		t.Fatalf("a NUL in the text found %v", names)
	}
	// A from no client asks for must not break the paging.
	if _, names := search(t, e, "npm-hosted", "text=a&from=9223372036854775807&size=250"); len(names) != 0 {
		t.Fatalf("a huge from: %v", names)
	}
	if got := e.h.Access(httptest.NewRequest("GET", "/", nil), e.repos["npm-hosted"], "-/v1/search"); got != auth.Read {
		t.Fatalf("search needs %v", got)
	}
}

// A search lists a package only to a caller who may read it: a caller whose
// access comes from a content selector does not learn the other names.
func TestSearchHidesWhatTheCallerMayNotRead(t *testing.T) {
	e := newEnv(t)
	publishWith(t, e, "@open/pad", "1.0.0", nil)
	publishWith(t, e, "@secret/pad", "1.0.0", nil)
	db := e.h.Store.DB
	ctx := t.Context()
	if err := db.CreateContentSelector(ctx, meta.ContentSelector{Name: "not-secret", Expression: `path =~ "^/(-|@open)/.*"`}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRole(ctx, meta.Role{ID: "sel", Privileges: []meta.Privilege{{Target: "selector:not-secret", Actions: []string{meta.ActionRead}, Repos: []string{"npm-hosted"}}}}); err != nil {
		t.Fatal(err)
	}
	settings := meta.DefaultSettings()
	settings.AnonymousRead, settings.AuthenticatedRead = false, false
	if err := db.SaveSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, metaUser("sel", "sel")); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Open(ctx, db, db)
	if err != nil {
		t.Fatal(err)
	}
	e.h.Auth = a
	p, ok := a.Lookup("sel")
	if !ok {
		t.Fatal("sel")
	}
	if _, names := searchAs(t, e, p, "npm-hosted", "text=pad"); strings.Join(names, " ") != "@open/pad" {
		t.Fatalf("selector-only caller found %v", names)
	}
	if _, names := search(t, e, "npm-hosted", "text=pad"); strings.Join(names, " ") != "@open/pad @secret/pad" {
		t.Fatalf("admin found %v", names)
	}
}

// A proxy passes the search and its parameters to the remote and the answer
// back as it is; a group gives its members' hits in member order, each name
// once, and pages over them. A proxy member without search is left out.
func TestSearchProxyAndGroup(t *testing.T) {
	e := newEnv(t)
	up, remote := scriptedMember(t, e, "npm-up")
	groupOf(e, "g-search", e.repos["npm-hosted"], up, e.repos["npm-proxy"]) // npm-proxy's remote has no search
	upstream := `{"objects":[{"package":{"name":"left-pad","version":"1.3.0"},"searchScore":9},{"package":{"name":"pad-right","version":"2.0.0"}}],"total":4711,"time":"then"}`
	remote.set("/-/v1/search", answer{ctype: "application/json", body: upstream})
	publishWith(t, e, "left-pad", "0.3.0", map[string]any{"description": "the hosted one"})
	publishWith(t, e, "apad", "1.0.0", nil)

	w := httptest.NewRecorder()
	e.h.Serve(w, httptest.NewRequest("GET", "/repository/npm-up/-/v1/search?text=pad&size=7&from=3&quality=0.65", nil), up, "-/v1/search")
	if w.Code != 200 || w.Body.String() != upstream || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("proxy search: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if remote.query != "text=pad&size=7&from=3&quality=0.65" {
		t.Fatalf("the remote was asked %q", remote.query)
	}
	remote.set("/-/v1/search", answer{status: 404, ctype: "application/json", body: `{"error":"no search"}`})
	if w := e.do("GET", "npm-up", "-/v1/search", "", anon); w.Code != 404 {
		t.Fatalf("a remote without search: %d", w.Code)
	}
	if _, names := search(t, e, "g-search", "text=pad"); strings.Join(names, " ") != "apad left-pad" {
		t.Fatalf("group search without a searching remote: %v", names)
	}

	remote.set("/-/v1/search", answer{ctype: "application/json", body: upstream})
	m, names := search(t, e, "g-search", "text=pad&quality=0.65")
	if strings.Join(names, " ") != "apad left-pad pad-right" || m["total"] != float64(3) {
		t.Fatalf("group search: %v total %v", names, m["total"])
	}
	if get(m["objects"].([]any)[1].(map[string]any), "package", "description") != "the hosted one" {
		t.Fatalf("the first member's left-pad must win: %v", m["objects"])
	}
	if !strings.Contains(remote.query, "text=pad") || !strings.Contains(remote.query, "from=0") || !strings.Contains(remote.query, "size=20") || !strings.Contains(remote.query, "quality=0.65") {
		t.Fatalf("the member's remote was asked %q", remote.query)
	}
	if _, names := search(t, e, "g-search", "text=pad&size=1&from=2"); strings.Join(names, " ") != "pad-right" {
		t.Fatalf("group search, third hit: %v", names)
	}

	// A blocked remote is never asked, and a routing rule sees the path.
	asked := remote.count("/-/v1/search")
	up.Blocked = true
	if w := e.do("GET", "npm-up", "-/v1/search", "", anon); w.Code != 502 {
		t.Fatalf("blocked proxy: %d", w.Code)
	}
	if _, names := search(t, e, "g-search", "text=pad"); strings.Join(names, " ") != "apad left-pad" {
		t.Fatalf("group search with a blocked member: %v", names)
	}
	up.Blocked = false
	e.h.Resolver.Fetcher.RouteAllowed = func(_ *repo.Repository, path string) bool { return path != "/-/v1/search" }
	if _, names := search(t, e, "g-search", "text=pad"); strings.Join(names, " ") != "apad left-pad" {
		t.Fatalf("group search with a member's routing rule: %v", names)
	}
	if n := remote.count("/-/v1/search"); n != asked {
		t.Fatalf("the remote was asked %d times more", n-asked)
	}
}
