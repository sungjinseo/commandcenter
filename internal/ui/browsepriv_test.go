package ui

import (
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

// okHandler answers every repository request with 200 "content".
type okHandler struct{}

func (okHandler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	w.Write([]byte("content"))
}

// I1 (final review): with authenticatedRead off, a browse-only role lists,
// browses and searches its repository and sees its components and assets in
// the UI, but cannot download content through /repository/. A role with
// nothing sees nothing.
func TestBrowsePrivilegeAllowsUIButNotDownload(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.srv.Handlers["maven"] = okHandler{}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"authenticatedRead":false}`), http.StatusOK)
	wantStatus(t, e.admin(t, "POST", "/-/api/roles",
		`{"id":"browser","privileges":[{"target":"repo:maven-releases","actions":["browse"]}]}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/roles",
		`{"id":"fetcher","privileges":[{"target":"repo:maven-releases","actions":["read"]}]}`), http.StatusCreated)
	for _, c := range []struct{ id, roles string }{{"bob", `["browser"]`}, {"nobody", `[]`}, {"fred", `["fetcher"]`}} {
		wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"`+c.id+`","password":"longenough","roles":`+c.roles+`}`), http.StatusCreated)
	}
	as := func(user, path string) resp {
		t.Helper()
		return e.basicAs(t, user, "longenough", "GET", path, "")
	}

	// browse-only: the UI shows the repository and its contents
	list := as("bob", "/-/api/repositories")
	wantStatus(t, list, http.StatusOK)
	if !strings.Contains(list.body, `"name":"maven-releases"`) {
		t.Fatalf("browse-only repo list: %s", list.body)
	}
	if strings.Contains(list.body, `"name":"npm-private"`) {
		t.Fatalf("browse-only user sees an unrelated repo: %s", list.body)
	}
	wantStatus(t, as("bob", "/-/api/repositories/maven-releases"), http.StatusOK)
	b := as("bob", "/-/api/repositories/maven-releases/browse?path=org/example/app/1.0/")
	wantStatus(t, b, http.StatusOK)
	if !strings.Contains(b.body, "app-1.0.jar") {
		t.Fatalf("browse: %s", b.body)
	}
	a := as("bob", "/-/api/repositories/maven-releases/asset?path=org/example/app/1.0/app-1.0.jar")
	wantStatus(t, a, http.StatusOK)
	if a.json(t)["canDownload"] != false {
		t.Fatalf("browse-only asset must say canDownload=false: %s", a.body)
	}
	s := as("bob", "/-/api/search?q=app-1.0")
	wantStatus(t, s, http.StatusOK)
	if !strings.Contains(s.body, "app-1.0.jar") {
		t.Fatalf("search: %s", s.body)
	}
	wantStatus(t, as("bob", "/-/api/search?q=app-1.0&repo=maven-releases"), http.StatusOK)
	c := as("bob", "/-/api/search/components?name=app")
	wantStatus(t, c, http.StatusOK)
	if !strings.Contains(c.body, `"name":"app"`) || !strings.Contains(c.body, "app-1.0.jar") {
		t.Fatalf("components: %s", c.body)
	}
	// ... but no content download
	wantStatus(t, as("bob", "/repository/maven-releases/org/example/app/1.0/app-1.0.jar"), http.StatusForbidden)

	// read: the download works and the asset says so
	wantStatus(t, as("fred", "/repository/maven-releases/org/example/app/1.0/app-1.0.jar"), http.StatusOK)
	a = as("fred", "/-/api/repositories/maven-releases/asset?path=org/example/app/1.0/app-1.0.jar")
	if a.json(t)["canDownload"] != true {
		t.Fatalf("reader asset must say canDownload=true: %s", a.body)
	}

	// nothing: nothing is visible (anonymous-read repos aside)
	list = as("nobody", "/-/api/repositories")
	if strings.Contains(list.body, `"name":"maven-releases"`) {
		t.Fatalf("roleless user sees maven-releases: %s", list.body)
	}
	wantStatus(t, as("nobody", "/-/api/repositories/maven-releases/browse"), http.StatusNotFound)
	wantStatus(t, as("nobody", "/-/api/repositories/maven-releases/asset?path=readme.txt"), http.StatusNotFound)
	wantStatus(t, as("nobody", "/-/api/search?q=app-1.0&repo=maven-releases"), http.StatusNotFound)
	if r := as("nobody", "/-/api/search?q=app-1.0"); strings.Contains(r.body, "maven-releases") {
		t.Fatalf("roleless search: %s", r.body)
	}
	wantStatus(t, as("nobody", "/-/api/search/components?name=app&repo=maven-releases"), http.StatusNotFound)
	wantStatus(t, as("nobody", "/repository/maven-releases/readme.txt"), http.StatusForbidden)
}

// I2 (final review): /-/api/me tells every caller, anonymous included, the
// server's defaultLanguage (the UI fallback when nothing is stored).
func TestMeDefaultLanguage(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"defaultLanguage":"en"}`), http.StatusOK)
	if got := e.get(t, "/-/api/me", "").json(t)["defaultLanguage"]; got != "en" {
		t.Fatalf("anonymous defaultLanguage = %v", got)
	}
	if got := e.get(t, "/-/api/me", "reader").json(t)["defaultLanguage"]; got != "en" {
		t.Fatalf("reader defaultLanguage = %v", got)
	}
}

// M4 (final review): canWrite is false while the write target is offline.
func TestCanWriteFalseForOfflineTarget(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	if l := repoList(t, e, "admin"); !l["maven-public"].CanWrite || !l["maven-releases"].CanWrite {
		t.Fatalf("online: %+v", l)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"online":false}`), http.StatusOK)
	l := repoList(t, e, "admin")
	if l["maven-public"].CanWrite || l["maven-releases"].CanWrite {
		t.Fatalf("offline write target still writable: %+v", l)
	}
}
