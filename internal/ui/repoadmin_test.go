package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

func (e *env) admin(t *testing.T, method, path, body string) resp {
	t.Helper()
	return e.req(t, method, path, body, "admin", nil, true)
}

func TestRepoCreateHostedProxyGroup(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.admin(t, "POST", "/-/api/repositories",
		`{"name":"mvn-new","format":"maven","type":"hosted","versionPolicy":"mixed","writePolicy":"allow","anonymousRead":true}`)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	if m["name"] != "mvn-new" || m["versionPolicy"] != "mixed" || m["writePolicy"] != "allow" || m["anonymousRead"] != true {
		t.Fatalf("hosted: %s", r.body)
	}

	r = e.admin(t, "POST", "/-/api/repositories",
		`{"name":"npm-px","format":"npm","type":"proxy","remote":"https://registry.example.org/","remoteAuth":{"username":"bob","password":"hunter2"},"metadataMaxAge":"1h","contentMaxAge":"-1","negativeCacheTTL":""}`)
	wantStatus(t, r, http.StatusCreated)
	m = r.json(t)
	ra := m["remoteAuth"].(map[string]any)
	if ra["username"] != "bob" || ra["passwordSet"] != true || ra["bearerTokenSet"] != false {
		t.Fatalf("remoteAuth: %s", r.body)
	}
	if m["metadataMaxAge"] != "1h" || m["contentMaxAge"] != "-1" || m["negativeCacheTTL"] != "10m" || m["anonymousRead"] != nil {
		t.Fatalf("proxy: %s", r.body)
	}
	if strings.Contains(r.body, "hunter2") {
		t.Fatalf("secret leaked: %s", r.body)
	}

	r = e.admin(t, "POST", "/-/api/repositories",
		`{"name":"mvn-grp","format":"maven","type":"group","members":["mvn-new","maven-releases"],"writeMember":"mvn-new"}`)
	wantStatus(t, r, http.StatusCreated)
	m = r.json(t)
	if got := m["members"].([]any); len(got) != 2 || got[0] != "mvn-new" || m["writeMember"] != "mvn-new" {
		t.Fatalf("group: %s", r.body)
	}
	// the new repositories show up in the list
	if !strings.Contains(e.get(t, "/-/api/repositories", "admin").body, "npm-px") {
		t.Fatal("npm-px not listed")
	}
}

func TestRepoValidationErrors(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.admin(t, "POST", "/-/api/repositories", `{"name":"bad name","format":"nope","type":"proxy"}`)
	wantStatus(t, r, http.StatusBadRequest)
	m := r.json(t)
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 || m["error"] != errs[0] {
		t.Fatalf("errors: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/repositories", `{"name":"x","format":"maven","type":"hosted","metadataMaxAge":"soon"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "metadataMaxAge") {
		t.Fatalf("duration: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"x","bogus":1}`), http.StatusBadRequest)
	// duplicate name
	r = e.admin(t, "POST", "/-/api/repositories", `{"name":"maven-releases","format":"maven","type":"hosted"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "duplicate") {
		t.Fatalf("duplicate: %s", r.body)
	}
	// QA L-9: fields of another repository type are refused, on create and update.
	for _, c := range []struct{ method, path, body, want string }{
		{"POST", "/-/api/repositories", `{"name":"h2","format":"maven","type":"hosted","remote":"https://repo1.example.org/"}`, "only valid for proxy"},
		{"POST", "/-/api/repositories", `{"name":"g2","format":"maven","type":"group","members":["maven-releases"],"versionPolicy":"release"}`, "versionPolicy is not valid for group"},
		{"PUT", "/-/api/repositories/maven-releases", `{"remote":"https://repo1.example.org/"}`, "only valid for proxy"},
		{"PUT", "/-/api/repositories/maven-public", `{"members":["maven-releases","maven-proxy"],"writeMember":"maven-releases","versionPolicy":"mixed"}`, "versionPolicy is not valid for group"},
	} {
		r := e.admin(t, c.method, c.path, c.body)
		if r.status != http.StatusBadRequest || !strings.Contains(r.body, c.want) {
			t.Errorf("%s %s %s: %d %s", c.method, c.path, c.body, r.status, r.body)
		}
	}
}

func TestRepoUpdateKeepsSecretAndClears(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"px","format":"npm","type":"proxy","remote":"https://r.example.org/","remoteAuth":{"username":"bob","password":"pw1","bearerToken":"tok1"}}`), http.StatusCreated)

	// edit other fields; empty secrets keep the stored ones
	r := e.admin(t, "PUT", "/-/api/repositories/px",
		`{"name":"px","format":"npm","type":"proxy","remote":"https://r.example.org/","remoteAuth":{"username":"bob","password":"","bearerToken":""},"metadataMaxAge":"2h","anonymousRead":false}`)
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	ra := m["remoteAuth"].(map[string]any)
	if m["metadataMaxAge"] != "2h" || m["anonymousRead"] != false || ra["passwordSet"] != true || ra["bearerTokenSet"] != true {
		t.Fatalf("update: %s", r.body)
	}
	if got := e.ui.Repos.Get("px").RemoteAuth; got.Password != "pw1" || got.BearerToken != "tok1" {
		t.Fatalf("stored secrets changed: %+v", got)
	}

	// anonymousRead back to inherit
	r = e.admin(t, "PUT", "/-/api/repositories/px", `{"remote":"https://r.example.org/","anonymousRead":null}`)
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["anonymousRead"] != nil {
		t.Fatalf("inherit: %s", r.body)
	}

	// clear flags
	r = e.admin(t, "PUT", "/-/api/repositories/px",
		`{"remote":"https://r.example.org/","remoteAuth":{"username":"bob","clearBearerToken":true}}`)
	wantStatus(t, r, http.StatusOK)
	ra = r.json(t)["remoteAuth"].(map[string]any)
	if ra["passwordSet"] != true || ra["bearerTokenSet"] != false {
		t.Fatalf("clear token: %s", r.body)
	}
	r = e.admin(t, "PUT", "/-/api/repositories/px",
		`{"remote":"https://r.example.org/","remoteAuth":{"clearPassword":true}}`)
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["remoteAuth"] != nil && m["remoteAuth"].(map[string]any)["passwordSet"] == true {
		t.Fatalf("clear password: %s", r.body)
	}

	// unknown repository
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/nope", `{}`), http.StatusNotFound)
}

func TestRepoUpdateImmutableAndHostChange(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, body := range []string{
		`{"name":"other"}`, `{"format":"npm"}`, `{"type":"proxy"}`,
	} {
		r := e.admin(t, "PUT", "/-/api/repositories/maven-releases", body)
		wantStatus(t, r, http.StatusBadRequest)
		if !strings.Contains(r.body, "cannot be changed") {
			t.Fatalf("%s: %s", body, r.body)
		}
	}
	// omitted or matching immutable fields are fine
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases",
		`{"name":"maven-releases","format":"maven","type":"hosted","versionPolicy":"mixed"}`), http.StatusOK)

	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"px","format":"npm","type":"proxy","remote":"https://a.example.org/","remoteAuth":{"username":"u","password":"p"}}`), http.StatusCreated)
	r := e.admin(t, "PUT", "/-/api/repositories/px", `{"remote":"https://b.example.org/"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "host changed") {
		t.Fatalf("host change: %s", r.body)
	}
}

func TestRepoUpdateKeepsRemoteQueryWhenUnchanged(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	// maven-proxy's remote carries userinfo, a query and a fragment
	before := e.ui.Repos.Get("maven-proxy").Remote
	cfg := e.get(t, "/-/api/repositories/maven-proxy/config", "admin")
	wantStatus(t, cfg, http.StatusOK)
	if strings.Contains(cfg.body, "secret") || strings.Contains(cfg.body, "token=x") {
		t.Fatalf("remote not sanitized: %s", cfg.body)
	}
	public := cfg.json(t)["remote"].(string)
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-proxy", `{"remote":"`+public+`","metadataMaxAge":"3h"}`), http.StatusOK)
	if got := e.ui.Repos.Get("maven-proxy").Remote; got != before {
		t.Fatalf("remote changed: %q -> %q", before, got)
	}
}

func TestRepoDelete(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	ctx := context.Background()
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"tmp","format":"maven","type":"hosted"}`), http.StatusCreated)
	if err := e.db.PutAsset(ctx, meta.Asset{Repo: "tmp", Path: "a/b.txt", BlobSHA256: "x", Size: 1, CreatedAt: t0, LastModified: t0}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/repositories/tmp", ""), http.StatusNoContent)
	if as, err := e.db.ListAssets(ctx, "tmp", ""); err != nil || len(as) != 0 {
		t.Fatalf("assets left: %v %v", as, err)
	}
	if e.ui.Repos.Get("tmp") != nil {
		t.Fatal("still in registry")
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/repositories/tmp", ""), http.StatusNotFound)

	// member of a group: 409 naming the group
	r := e.admin(t, "DELETE", "/-/api/repositories/maven-releases", "")
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(r.body, "maven-public") {
		t.Fatalf("409 message: %s", r.body)
	}
	if e.ui.Repos.Get("maven-releases") == nil {
		t.Fatal("repository removed despite 409")
	}
}

func TestRepoAdminAuth(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	body := `{"name":"zz","format":"maven","type":"hosted"}`
	calls := []struct{ method, path, body string }{
		{"POST", "/-/api/repositories", body},
		{"PUT", "/-/api/repositories/maven-releases", `{}`},
		{"DELETE", "/-/api/repositories/npm-private", ""},
	}
	for _, c := range calls {
		if r := e.req(t, c.method, c.path, c.body, "ci", nil, true); r.status != http.StatusForbidden {
			t.Errorf("non-admin %s %s: %d", c.method, c.path, r.status)
		}
		if r := e.req(t, c.method, c.path, c.body, "", nil, true); r.status != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: %d", c.method, c.path, r.status)
		}
		if r := e.req(t, c.method, c.path, c.body, "admin", nil, false); r.status != http.StatusForbidden {
			t.Errorf("no UI header %s %s: %d", c.method, c.path, r.status)
		}
	}
	_, tok := e.newToken(t, "admin", "")
	for _, c := range calls {
		r, _ := http.NewRequest(c.method, e.ts.URL+c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("X-Command-Center-UI", "1")
		res, err := noRedirect.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("token admin %s %s: %d", c.method, c.path, res.StatusCode)
		}
	}
	// GET config: admin only
	if r := e.get(t, "/-/api/repositories/maven-releases/config", "ci"); r.status != http.StatusForbidden {
		t.Errorf("non-admin GET config: %d", r.status)
	}
	if r := e.get(t, "/-/api/repositories/maven-releases/config", ""); r.status != http.StatusUnauthorized {
		t.Errorf("anonymous GET config: %d", r.status)
	}
	if e.ui.Repos.Get("zz") != nil {
		t.Fatal("rejected create took effect")
	}
}

func TestRepoConfigNeverHasSecrets(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"px","format":"npm","type":"proxy","remote":"https://r.example.org/","remoteAuth":{"username":"bob","password":"S3CRET-PW","bearerToken":"S3CRET-TOK"}}`), http.StatusCreated)
	r := e.get(t, "/-/api/repositories/px/config", "admin")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	if strings.Contains(r.body, "S3CRET") {
		t.Fatalf("secret in config: %s", r.body)
	}
	ra := r.json(t)["remoteAuth"].(map[string]any)
	if ra["username"] != "bob" || ra["passwordSet"] != true || ra["bearerTokenSet"] != true {
		t.Fatalf("remoteAuth: %s", r.body)
	}
	if _, has := ra["password"]; has {
		t.Fatalf("password key present: %s", r.body)
	}
	// no response of the other repository endpoints leaks it either
	for _, p := range []string{"/-/api/repositories", "/-/api/repositories/px"} {
		if b := e.get(t, p, "admin").body; strings.Contains(b, "S3CRET") {
			t.Fatalf("%s leaks: %s", p, b)
		}
	}
	wantStatus(t, e.get(t, "/-/api/repositories/nope/config", "admin"), http.StatusNotFound)
	// a repository with no credentials has remoteAuth null
	if m := e.get(t, "/-/api/repositories/maven-releases/config", "admin").json(t); m["remoteAuth"] != nil {
		t.Fatalf("remoteAuth: %v", m["remoteAuth"])
	}
}

// memHandler is a tiny in-memory format handler: PUT stores, GET returns.
type memHandler struct {
	mu   sync.Mutex
	data map[string]string
}

func (h *memHandler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	key := rp.Name + "/" + path
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		h.data[key] = string(b)
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		v, ok := h.data[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, v)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestCreatedRepoIsImmediatelyServable(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.srv.Handlers["maven"] = &memHandler{data: map[string]string{}}

	url := "/repository/fresh/org/x/1.0/x-1.0.txt"
	if r := e.get(t, url, "admin"); r.status != http.StatusNotFound {
		t.Fatalf("before create: %d", r.status)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"fresh","format":"maven","type":"hosted"}`), http.StatusCreated)
	wantStatus(t, e.req(t, "PUT", url, "hello", "admin", nil, false), http.StatusCreated)
	r := e.get(t, url, "admin")
	wantStatus(t, r, http.StatusOK)
	if r.body != "hello" {
		t.Fatalf("body %q", r.body)
	}
	// deleted repositories stop being served at once
	wantStatus(t, e.admin(t, "DELETE", "/-/api/repositories/fresh", ""), http.StatusNoContent)
	if r := e.get(t, url, "admin"); r.status != http.StatusNotFound || strings.Contains(r.body, "hello") {
		t.Fatalf("after delete: %d %q", r.status, r.body)
	}
}

func TestRepoRemoteHiddenParts(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	// maven-proxy's remote has userinfo, a query and a fragment
	cfg := e.get(t, "/-/api/repositories/maven-proxy/config", "admin")
	wantStatus(t, cfg, http.StatusOK)
	m := cfg.json(t)
	if m["remoteHasHiddenParts"] != true {
		t.Fatalf("flag missing: %s", cfg.body)
	}
	public := m["remote"].(string)
	// a plain remote reports false
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"px","format":"npm","type":"proxy","remote":"https://r.example.org/"}`), http.StatusCreated)
	if e.get(t, "/-/api/repositories/px/config", "admin").json(t)["remoteHasHiddenParts"] != false {
		t.Fatal("flag set for a plain remote")
	}
	// keep: submitting the public form leaves the stored remote alone
	r := e.admin(t, "PUT", "/-/api/repositories/maven-proxy", `{"remote":"`+public+`"}`)
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["remoteHasHiddenParts"] != true {
		t.Fatalf("PUT response flag: %s", r.body)
	}
	if got := e.ui.Repos.Get("maven-proxy").Remote; !strings.Contains(got, "token=x") {
		t.Fatalf("hidden parts lost: %q", got)
	}
	// clear: the stripped URL is stored
	r = e.admin(t, "PUT", "/-/api/repositories/maven-proxy", `{"remote":"`+public+`","clearRemoteHiddenParts":true}`)
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["remoteHasHiddenParts"] != false {
		t.Fatalf("flag after clear: %s", r.body)
	}
	if got := e.ui.Repos.Get("maven-proxy").Remote; got != public {
		t.Fatalf("remote %q, want %q", got, public)
	}
	if strings.Contains(e.admin(t, "POST", "/-/api/repositories",
		`{"name":"py","format":"pypi","type":"proxy","remote":"https://u:p@h.example/x?k=v"}`).body, "k=v") {
		t.Fatal("create response leaks the query")
	}
}

func TestRepoBodyLimits(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	big := `{"name":"zz","format":"maven","type":"hosted","remote":"` + strings.Repeat("a", 70<<10) + `"}`
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", big), http.StatusRequestEntityTooLarge)
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"zz","format":"maven","type":"hosted"} garbage`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"zz","format":"maven","type":"hosted"}{"x":1}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"bogus":true}`), http.StatusBadRequest)
	// the top-level clear flags are gone: only remoteAuth carries them
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"clearPassword":true}`), http.StatusBadRequest)
	if e.ui.Repos.Get("zz") != nil {
		t.Fatal("rejected create took effect")
	}
}

func TestRepoDeleteWriteMemberConflict(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	// npm-private is both a member and the writeMember of npm-public
	r := e.admin(t, "DELETE", "/-/api/repositories/npm-private", "")
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(r.body, "npm-public") {
		t.Fatalf("409 message: %s", r.body)
	}
	// group validation requires writeMember to be a member, so a
	// writeMember-only dependency cannot be built through the API
	r = e.admin(t, "POST", "/-/api/repositories",
		`{"name":"npm-g2","format":"npm","type":"group","members":["npm-hosted"],"writeMember":"npm-private"}`)
	wantStatus(t, r, http.StatusBadRequest)
}

func TestRepoCreateNameRules(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, name := range []string{"..", "a b", "create", "a#b", strings.Repeat("a", 65)} {
		body, _ := json.Marshal(map[string]string{"name": name, "format": "maven", "type": "hosted"})
		wantStatus(t, e.admin(t, "POST", "/-/api/repositories", string(body)), http.StatusBadRequest)
		if e.ui.Repos.Get(name) != nil {
			t.Errorf("rejected name %q took effect", name)
		}
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"ok.name_1-x","format":"maven","type":"hosted"}`), http.StatusCreated)
}

// C1: a stored group that lists a member twice (accepted by older builds)
// does not block edits of other repositories; the definition being created
// or updated is validated (negative durations, duplicate members -> 400).
func TestRepoLegacyRowsDoNotBlockOtherEdits(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "legacy-group", Format: "maven", Type: "group",
			Members: []string{"maven-releases", "maven-releases"}, NegativeCacheTTL: config.Duration(-5 * time.Minute)})
	})
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"fresh","format":"maven","type":"hosted"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/fresh", `{"writePolicy":"allow"}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/npm-hosted", `{"writePolicy":"allow"}`), http.StatusOK)

	for _, c := range []struct{ method, path, body, want string }{
		{"POST", "/-/api/repositories", `{"name":"neg","format":"maven","type":"hosted","negativeCacheTTL":"-5m"}`, "negativeCacheTTL"},
		{"POST", "/-/api/repositories", `{"name":"dup","format":"maven","type":"group","members":["maven-releases","maven-releases"]}`, "duplicate member"},
		{"PUT", "/-/api/repositories/fresh", `{"metadataMaxAge":"-1h"}`, "metadataMaxAge"},
		{"PUT", "/-/api/repositories/maven-public", `{"members":["maven-releases","maven-proxy","maven-proxy"]}`, "duplicate member"},
	} {
		r := e.admin(t, c.method, c.path, c.body)
		wantStatus(t, r, http.StatusBadRequest)
		if !strings.Contains(r.body, c.want) {
			t.Errorf("%s %s: %s", c.method, c.body, r.body)
		}
	}
	// "-1" still means never.
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/fresh", `{"contentMaxAge":"-1"}`), http.StatusOK)
}
