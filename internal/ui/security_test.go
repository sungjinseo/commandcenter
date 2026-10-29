package ui

import (
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

// writeHandler answers every repository request with 200 and records nothing.
type writeHandler struct{}

func (writeHandler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	w.WriteHeader(http.StatusOK)
}

func TestRoutingRulesAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.srv.Handlers["maven"] = writeHandler{}
	// Validation: name, mode, a bad regex.
	r := e.admin(t, "POST", "/-/api/routing-rules", `{"name":"bad name","mode":"deny","matchers":["("]}`)
	wantStatus(t, r, http.StatusBadRequest)
	for _, want := range []string{"invalid routing rule name", "mode must be", "invalid regular expression"} {
		if !strings.Contains(r.body, want) {
			t.Fatalf("validation body lacks %q: %s", want, r.body)
		}
	}
	r = e.admin(t, "POST", "/-/api/routing-rules", `{"name":"no-internal","description":"keep internal artifacts off the remote","mode":"block","matchers":["^/internal/",".*-SNAPSHOT.*"]}`)
	wantStatus(t, r, http.StatusCreated)
	if m := r.json(t); m["mode"] != "BLOCK" || m["name"] != "no-internal" {
		t.Fatalf("created: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/routing-rules", `{"name":"no-internal","mode":"BLOCK","matchers":["x"]}`), http.StatusConflict)
	wantStatus(t, e.req(t, "GET", "/-/api/routing-rules", "", "reader", nil, false), http.StatusForbidden)

	// test: by name, and by mode+matchers.
	r = e.admin(t, "POST", "/-/api/routing-rules/test", `{"path":"/internal/x.jar","name":"no-internal"}`)
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["allowed"] != false || m["matchedBy"] != "^/internal/" {
		t.Fatalf("test by name: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/routing-rules/test", `{"path":"org/x.jar","mode":"ALLOW","matchers":["^/org/"]}`)
	if m := r.json(t); m["allowed"] != true || m["matchedBy"] != "^/org/" {
		t.Fatalf("test by matchers: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/routing-rules/test", `{"path":"/com/x.jar","mode":"ALLOW","matchers":["^/org/"]}`)
	if m := r.json(t); m["allowed"] != false || m["matchedBy"] != nil {
		t.Fatalf("test no match: %s", r.body)
	}

	// The repository form takes routingRule on a proxy; the rule is applied at once.
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-proxy", `{"remote":"https://repo1.example.org/maven2","routingRule":"nope"}`), http.StatusBadRequest)
	r = e.admin(t, "PUT", "/-/api/repositories/maven-proxy", `{"remote":"https://repo1.example.org/maven2","routingRule":"no-internal"}`)
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["routingRule"] != "no-internal" {
		t.Fatalf("repo config: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"routingRule":"no-internal"}`), http.StatusBadRequest)
	wantStatus(t, e.req(t, "GET", "/repository/maven-proxy/internal/x.jar", "", "admin", nil, false), http.StatusForbidden)
	wantStatus(t, e.req(t, "GET", "/repository/maven-proxy/org/x.jar", "", "admin", nil, false), http.StatusOK)
	r = e.admin(t, "GET", "/-/api/routing-rules/no-internal", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"repositories":["maven-proxy"]`) {
		t.Fatalf("users: %s", r.body)
	}

	// In use: 409 naming the proxy; after disconnecting, the delete works.
	r = e.admin(t, "DELETE", "/-/api/routing-rules/no-internal", "")
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(r.body, "maven-proxy") {
		t.Fatalf("in use: %s", r.body)
	}
	// Update changes the live rule.
	wantStatus(t, e.admin(t, "PUT", "/-/api/routing-rules/no-internal", `{"name":"other","mode":"BLOCK","matchers":["^/x/"]}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/routing-rules/no-internal", `{"mode":"ALLOW","matchers":["^/internal/"]}`), http.StatusOK)
	wantStatus(t, e.req(t, "GET", "/repository/maven-proxy/internal/x.jar", "", "admin", nil, false), http.StatusOK)
	wantStatus(t, e.req(t, "GET", "/repository/maven-proxy/org/x.jar", "", "admin", nil, false), http.StatusForbidden)
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-proxy", `{"remote":"https://repo1.example.org/maven2","routingRule":""}`), http.StatusOK)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/routing-rules/no-internal", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/routing-rules/no-internal", ""), http.StatusNotFound)
	wantStatus(t, e.req(t, "GET", "/repository/maven-proxy/org/x.jar", "", "admin", nil, false), http.StatusOK)
}

func TestContentSelectorsAPIAndPrivileges(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.srv.Handlers["maven"] = writeHandler{}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"authenticatedRead":false}`), http.StatusOK)
	// Validation and CRUD.
	r := e.admin(t, "POST", "/-/api/content-selectors", `{"name":"acme","expression":"path =^ /org/acme/"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "expression:") {
		t.Fatalf("bad expression: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/content-selectors", `{"name":"acme","description":"acme's artifacts","expression":"format == \"maven2\" and path =^ \"/org/example/app/\""}`)
	wantStatus(t, r, http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/content-selectors", `{"name":"acme","expression":"path =^ \"/\""}`), http.StatusConflict)
	r = e.admin(t, "GET", "/-/api/content-selectors", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"name":"acme"`) || !strings.Contains(r.body, `"roles":[]`) {
		t.Fatalf("list: %s", r.body)
	}

	// Preview: the matching asset paths of a repository.
	r = e.admin(t, "POST", "/-/api/content-selectors/preview", `{"expression":"path =^ \"/org/example/app/\"","repo":"maven-releases"}`)
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, "org/example/app/1.0/app-1.0.jar") || strings.Contains(r.body, "readme.txt") || strings.Contains(r.body, "pad-util") {
		t.Fatalf("preview: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/content-selectors/preview", `{"expression":"path =^ \"/\"","repo":"nope"}`), http.StatusNotFound)
	wantStatus(t, e.admin(t, "POST", "/-/api/content-selectors/preview", `{"expression":"(","repo":"maven-releases"}`), http.StatusBadRequest)

	// A role privilege on the selector: validation of target and repos.
	r = e.admin(t, "POST", "/-/api/roles", `{"id":"acme-dev","privileges":[{"target":"selector:nope","actions":["read"],"repos":["maven-releases"]},{"target":"selector:acme","actions":["read"]},{"target":"repo:maven-releases","actions":["read"],"repos":["x"]},{"target":"selector:acme","actions":["read"],"repos":["ghost"]}]}`)
	wantStatus(t, r, http.StatusBadRequest)
	for _, want := range []string{`unknown content selector`, "needs repos", `repos is only valid`, `unknown repository`} {
		if !strings.Contains(r.body, want) {
			t.Fatalf("role validation lacks %q: %s", want, r.body)
		}
	}
	r = e.admin(t, "POST", "/-/api/roles", `{"id":"acme-dev","privileges":[{"target":"selector:acme","actions":["browse","read","add","delete"],"repos":["maven-releases"]}]}`)
	wantStatus(t, r, http.StatusCreated)
	if !strings.Contains(r.body, `"repos":["maven-releases"]`) {
		t.Fatalf("role: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"dev","password":"longenough","roles":["acme-dev"]}`), http.StatusCreated)
	as := func(method, path string) resp {
		t.Helper()
		return e.basicAs(t, "dev", "longenough", method, path, "")
	}
	// Through the real server: the selector's path is readable, writable
	// and deletable; other paths of the repository and other repositories
	// are 403.
	wantStatus(t, as("GET", "/repository/maven-releases/org/example/app/1.0/app-1.0.jar"), http.StatusOK)
	wantStatus(t, as("PUT", "/repository/maven-releases/org/example/app/2.0/app-2.0.jar"), http.StatusOK)
	wantStatus(t, as("DELETE", "/repository/maven-releases/org/example/app/1.0/app-1.0.jar"), http.StatusOK)
	wantStatus(t, as("GET", "/repository/maven-releases/org/example/pad-util/1.0/pad-util-1.0.jar"), http.StatusForbidden)
	wantStatus(t, as("PUT", "/repository/maven-releases/org/example/pad-util/2.0/x.jar"), http.StatusForbidden)
	wantStatus(t, as("DELETE", "/repository/maven-releases/readme.txt"), http.StatusForbidden)
	wantStatus(t, as("GET", "/repository/maven-public/org/example/app/1.0/app-1.0.jar"), http.StatusForbidden) // the group is not in scope
	// Listing works at the repository level; the asset view says whether
	// the path can be downloaded.
	list := as("GET", "/-/api/repositories")
	wantStatus(t, list, http.StatusOK)
	if !strings.Contains(list.body, `"name":"maven-releases"`) || strings.Contains(list.body, `"name":"maven-public"`) {
		t.Fatalf("list: %s", list.body)
	}
	b := as("GET", "/-/api/repositories/maven-releases/browse?path=org/example/")
	wantStatus(t, b, http.StatusOK)
	if !strings.Contains(b.body, "pad-util") {
		t.Fatalf("browse hides names: %s", b.body)
	}
	if a := as("GET", "/-/api/repositories/maven-releases/asset?path=org/example/pad-util/1.0/pad-util-1.0.jar"); a.json(t)["canDownload"] != false {
		t.Fatalf("canDownload outside the selector: %s", a.body)
	}
	if a := as("GET", "/-/api/repositories/maven-releases/asset?path=org/example/app/1.0/app-1.0.pom"); a.json(t)["canDownload"] != true {
		t.Fatalf("canDownload inside the selector: %s", a.body)
	}
	// A component delete needs the whole repository: 403 for a path-scoped user.
	wantStatus(t, as("DELETE", "/-/api/repositories/maven-releases/components?group=org.example&name=app&version=1.0"), http.StatusForbidden)

	// The role references the selector: its delete is 409 naming the role.
	r = e.admin(t, "DELETE", "/-/api/content-selectors/acme", "")
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(r.body, `"roles":["acme-dev"]`) {
		t.Fatalf("in use: %s", r.body)
	}
	// Editing the expression applies at once.
	wantStatus(t, e.admin(t, "PUT", "/-/api/content-selectors/acme", `{"expression":"path =^ \"/org/example/pad-util/\""}`), http.StatusOK)
	wantStatus(t, as("GET", "/repository/maven-releases/org/example/pad-util/1.0/pad-util-1.0.jar"), http.StatusOK)
	wantStatus(t, as("GET", "/repository/maven-releases/org/example/app/1.0/app-1.0.pom"), http.StatusForbidden)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/users/dev", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/roles/acme-dev", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/content-selectors/acme", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "GET", "/-/api/content-selectors/acme", ""), http.StatusNotFound)
}
