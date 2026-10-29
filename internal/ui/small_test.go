package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

// hreq sends method path with extra headers; user "" sends no credentials.
func (e *env) hreq(t *testing.T, method, path, body, user string, hdr map[string]string) resp {
	t.Helper()
	r, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if user != "" {
		r.SetBasicAuth(user, "pw")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: string(b)}
}

func kinds(sink *repotest.Sink, kind string) []repotest.Event {
	var out []repotest.Event
	for _, ev := range sink.Events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// RUT realm through the real server: only a trusted RemoteAddr counts (a
// forwarded address is just another header), Authorization wins, groups
// map to roles, an override row disables the user, tokens work while the
// user keeps coming through the gateway.
func TestRUTRealm(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	r := e.admin(t, "GET", "/-/api/realms/rutauth", "")
	if m := r.json(t); m["header"] != "REMOTE_USER" || len(m["trustedProxies"].([]any)) != 0 {
		t.Fatalf("default: %s", r.body)
	}
	enable := `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"rutauth","enabled":true}]}`
	if r := e.admin(t, "PUT", "/-/api/realms", enable); r.status != http.StatusBadRequest || !strings.Contains(r.body, "configure rutauth") {
		t.Fatalf("enable unconfigured: %d %s", r.status, r.body)
	}
	for body, want := range map[string]string{
		`{"trustedProxies":["10.0.0.0"]}`:                                                           "not a CIDR",
		`{"header":"X Bad","trustedProxies":["10.0.0.0/8"]}`:                                        "header name",
		`{"trustedProxies":["10.0.0.0/8"],"groupMappings":[{"group":"devs","roles":["deployer"]}]}`: "need a groupHeader",
		`{"trustedProxies":["10.0.0.0/8"],"defaultRoles":["nope"]}`:                                 `unknown role \"nope\"`,
	} {
		if r := e.admin(t, "PUT", "/-/api/realms/rutauth", body); r.status != http.StatusBadRequest || !strings.Contains(r.body, want) {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	put := func(proxies string) {
		t.Helper()
		wantStatus(t, e.admin(t, "PUT", "/-/api/realms/rutauth",
			`{"trustedProxies":[`+proxies+`],"groupHeader":"X-Groups","groupMappings":[{"group":"devs","roles":["deployer"]}]}`), http.StatusOK)
	}
	put(`"10.0.0.0/8"`)
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", enable), http.StatusOK)
	if len(kinds(sink, "realms.rutauth.update")) != 1 {
		t.Fatalf("events %+v", sink.Events())
	}
	me := func(user string, hdr map[string]string) map[string]any {
		t.Helper()
		r := e.hreq(t, "GET", "/-/api/me", "", user, hdr)
		wantStatus(t, r, http.StatusOK)
		return r.json(t)
	}
	alice := map[string]string{"REMOTE_USER": " Alice ", "X-Groups": "devs, other"}
	// the test server's RemoteAddr is 127.0.0.1: a spoofed forwarded address does not make it trusted
	spoofed := map[string]string{"REMOTE_USER": "alice", "X-Forwarded-For": "10.1.2.3", "X-Real-Ip": "10.1.2.3", "Forwarded": "for=10.1.2.3"}
	if m := me("", spoofed); m["anonymous"] != true {
		t.Fatalf("spoofed: %v", m)
	}
	put(`"127.0.0.1/32"`)
	m := me("", alice)
	if m["user"] != "alice" || m["realm"] != "RUT" || m["owner"] != "rutauth:alice" || !strings.Contains(strings.Join(anyStrings(m["roles"]), ","), "deployer") {
		t.Fatalf("trusted: %v", m)
	}
	if m := me("reader", alice); m["user"] != "reader" { // Authorization wins
		t.Fatalf("with Authorization: %v", m)
	}
	if m := me("", map[string]string{"REMOTE_USER": "bad/name"}); m["anonymous"] != true {
		t.Fatalf("invalid id: %v", m)
	}
	// a token created through the gateway works on its own
	r = e.hreq(t, "POST", "/-/api/tokens", "{}", "", map[string]string{"REMOTE_USER": "alice", "X-Command-Center-UI": "1"})
	wantStatus(t, r, http.StatusCreated)
	tok := r.json(t)["token"].(string)
	if m := me("", map[string]string{"Authorization": "Bearer " + tok}); m["user"] != "alice" {
		t.Fatalf("token: %v", m)
	}
	// an override row (source rutauth) adds roles or disables the user
	wantStatus(t, e.admin(t, "POST", "/-/api/users", `{"id":"alice","source":"rutauth","status":"disabled"}`), http.StatusCreated)
	if m := me("", alice); m["anonymous"] != true {
		t.Fatalf("disabled: %v", m)
	}
	if r := e.admin(t, "GET", "/-/api/users", ""); !strings.Contains(r.body, `"RUT"`) {
		t.Fatalf("users: %s", r.body)
	}
	// the realm off: the header means nothing
	wantStatus(t, e.admin(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true}]}`), http.StatusOK)
	if m := me("", map[string]string{"REMOTE_USER": "bob"}); m["anonymous"] != true {
		t.Fatalf("realm off: %v", m)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// defaultRoles: granted to every signed-in principal, validated, and a role
// it names cannot be deleted.
func TestDefaultRoles(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	wantStatus(t, e.admin(t, "POST", "/-/api/roles", `{"id":"browse","privileges":[{"target":"repo:npm-private","actions":["browse","read","add"]}]}`), http.StatusCreated)
	canWrite := func(user string) bool {
		t.Helper()
		r := e.get(t, "/-/api/repositories/npm-private", user)
		return r.status == http.StatusOK && r.json(t)["canWrite"] == true
	}
	if canWrite("reader") {
		t.Fatal("reader can write before defaultRoles")
	}
	for body, want := range map[string]string{
		`{"defaultRoles":["nope"]}`:            `unknown role \"nope\"`,
		`{"defaultRoles":["browse","browse"]}`: "duplicate role",
		`{"defaultRoles":["admin"]}`:           "admin role cannot be a default role",
	} {
		if r := e.admin(t, "PUT", "/-/api/settings", body); r.status != http.StatusBadRequest || !strings.Contains(r.body, want) {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	r := e.admin(t, "PUT", "/-/api/settings", `{"defaultRoles":["browse"]}`)
	if wantStatus(t, r, http.StatusOK); !strings.Contains(r.body, `"defaultRoles":["browse"]`) {
		t.Fatalf("put: %s", r.body)
	}
	if ev := kinds(sink, "settings.defaultRoles"); len(ev) != 1 || ev[0].Detail != "browse" || len(kinds(sink, "settings.update")) != 0 {
		t.Fatalf("events %+v", sink.Events())
	}
	if !canWrite("reader") || !canWrite("ci") {
		t.Fatal("defaultRoles not granted")
	}
	_, tok := e.newToken(t, "reader", "")
	if r := e.hreq(t, "GET", "/-/api/me", "", "", map[string]string{"Authorization": "Bearer " + tok}); !strings.Contains(r.body, `"browse"`) {
		t.Fatalf("token principal: %s", r.body)
	}
	if canWrite("") {
		t.Fatal("anonymous holds the default roles")
	}
	if r := e.admin(t, "DELETE", "/-/api/roles/browse", ""); r.status != http.StatusConflict || !strings.Contains(r.body, "defaultRoles") {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	if r := e.admin(t, "GET", "/-/api/settings", ""); !strings.Contains(r.body, `"defaultRoles":["browse"]`) {
		t.Fatalf("get: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"defaultRoles":[]}`), http.StatusOK)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/roles/browse", ""), http.StatusNoContent)
}

// Soft quota: set on create or by PUT, evaluated by the status checks, shown
// in the blob store API; the event fires once per flip; writes go on.
func TestBlobStoreQuota(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	sink := &repotest.Sink{}
	e.ui.Events = sink
	for _, body := range []string{
		`{"name":"q","quota":{"type":"bytes","limitBytes":10}}`,
		`{"name":"q","quota":{"type":"spaceUsedBytes","limitBytes":0}}`,
	} {
		if r := e.admin(t, "POST", "/-/api/blobstores", body); r.status != http.StatusBadRequest || !strings.Contains(r.body, "quota") {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	r := e.admin(t, "POST", "/-/api/blobstores", `{"name":"q","quota":{"type":"spaceUsedBytes","limitBytes":5}}`)
	wantStatus(t, r, http.StatusCreated)
	if !strings.Contains(r.body, `"quota":{"type":"spaceUsedBytes","limitBytes":5},"quotaViolated":false`) {
		t.Fatalf("create: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"raw-q","format":"raw","type":"hosted","blobStore":"q"}`), http.StatusCreated)
	if err := e.db.PutAsset(context.Background(), meta.Asset{Repo: "raw-q", Path: "a", BlobSHA256: strings.Repeat("a", 64), Size: 10, CreatedAt: t0, LastModified: t0}); err != nil {
		t.Fatal(err)
	}
	check := func() map[string]any {
		t.Helper()
		var res struct{ Checks []map[string]any }
		json.Unmarshal([]byte(e.admin(t, "GET", "/-/api/status/checks", "").body), &res)
		for _, c := range res.Checks {
			if c["name"] == "Blob store quota: q" {
				return c
			}
		}
		return nil
	}
	for range 2 {
		if c := check(); c == nil || c["healthy"] != false || !strings.Contains(c["message"].(string), "soft quota exceeded: 10 of 5 bytes used") {
			t.Fatalf("violated: %v", c)
		}
	}
	if ev := kinds(sink, "blobstore.quota"); len(ev) != 1 || ev[0].Subject != "q" || ev[0].Actor != "system" || !strings.HasPrefix(ev[0].Detail, "violated") {
		t.Fatalf("events %+v", ev)
	}
	if r := e.admin(t, "GET", "/-/api/blobstores/q", ""); !strings.Contains(r.body, `"quotaViolated":true`) {
		t.Fatalf("get: %s", r.body)
	}
	r = e.admin(t, "PUT", "/-/api/blobstores/q", `{"quota":{"type":"spaceUsedBytes","limitBytes":100}}`)
	if wantStatus(t, r, http.StatusOK); !strings.Contains(r.body, `"limitBytes":100},"quotaViolated":false`) {
		t.Fatalf("put: %s", r.body)
	}
	if c := check(); c["healthy"] != true {
		t.Fatalf("within: %v", c)
	}
	if ev := kinds(sink, "blobstore.quota"); len(ev) != 2 || !strings.HasPrefix(ev[1].Detail, "within limit") {
		t.Fatalf("events %+v", ev)
	}
	// spaceRemainingBytes: no filesystem has an exabyte free
	wantStatus(t, e.admin(t, "PUT", "/-/api/blobstores/q", `{"quota":{"type":"spaceRemainingBytes","limitBytes":1152921504606846976}}`), http.StatusOK)
	if c := check(); c["healthy"] != false || !strings.Contains(c["message"].(string), "bytes free") {
		t.Fatalf("remaining: %v", c)
	}
	r = e.admin(t, "PUT", "/-/api/blobstores/q", `{"quota":null}`)
	if wantStatus(t, r, http.StatusOK); !strings.Contains(r.body, `"quota":null`) || check() != nil {
		t.Fatalf("cleared: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/blobstores/nope", `{"quota":null}`), http.StatusNotFound)
	wantStatus(t, e.req(t, "PUT", "/-/api/blobstores/q", `{"quota":null}`, "admin", nil, false), http.StatusForbidden) // no UI header
}

// The OpenAPI document is public.
func TestOpenAPIDocument(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, "/-/api/openapi.json", "")
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["openapi"] != "3.0.3" || m["paths"].(map[string]any)["/-/api/openapi.json"] == nil {
		t.Fatalf("doc: %.300s", r.body)
	}
}

// Repository JSON: content handling for admins, remote state for signed-in
// callers but not anonymous ones.
func TestRepositoryContentAndRemoteFields(t *testing.T) {
	yes := true
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		for i := range c.Repositories {
			if c.Repositories[i].Name == "maven-proxy" {
				c.Repositories[i].AnonymousRead = &yes
			}
		}
	})
	e.ui.Remote = func(rp *repo.Repository) repo.RemoteStatus {
		return repo.RemoteStatus{Blocked: true, Auto: true, Until: t0, LastError: "remote status 503"}
	}
	r := e.get(t, "/-/api/repositories/maven-proxy", "")
	if wantStatus(t, r, http.StatusOK); strings.Contains(r.body, "remoteStatus") || strings.Contains(r.body, "autoBlock") {
		t.Fatalf("anonymous: %s", r.body)
	}
	r = e.get(t, "/-/api/repositories/maven-proxy", "reader")
	if !strings.Contains(r.body, `"blocked":false,"autoBlock":true,"remoteStatus":{"blocked":true,"auto":true,"until":"2026-10-01T12:00:00Z","lastError":"remote status 503"}`) {
		t.Fatalf("reader: %s", r.body)
	}
	for body, want := range map[string]string{
		`{"name":"x1","format":"raw","type":"hosted","contentDisposition":"download"}`:                                 "contentDisposition must be",
		`{"name":"x2","format":"npm","type":"hosted","contentDisposition":"attachment"}`:                               "only valid for raw and gitlfs",
		`{"name":"x3","format":"raw","type":"proxy","remote":"https://r.example/","strictContentTypeValidation":true}`: "only valid for hosted",
		`{"name":"x4","format":"raw","type":"hosted","blocked":true}`:                                                  "only valid for proxy",
	} {
		if r := e.admin(t, "POST", "/-/api/repositories", body); r.status != http.StatusBadRequest || !strings.Contains(r.body, want) {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	r = e.admin(t, "POST", "/-/api/repositories", `{"name":"raw-s","format":"raw","type":"hosted","strictContentTypeValidation":true,"contentDisposition":"attachment"}`)
	if wantStatus(t, r, http.StatusCreated); !strings.Contains(r.body, `"strictContentTypeValidation":true,"contentDisposition":"attachment"`) {
		t.Fatalf("create: %s", r.body)
	}
	if r := e.admin(t, "GET", "/-/api/repositories/raw-s", ""); !strings.Contains(r.body, `"strictContentTypeValidation":true,"contentDisposition":"attachment"`) {
		t.Fatalf("describe: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/repositories", `{"name":"raw-p","format":"raw","type":"proxy","remote":"https://r.example/","autoBlock":false,"blocked":true}`)
	if wantStatus(t, r, http.StatusCreated); !strings.Contains(r.body, `"blocked":true,"autoBlock":false`) {
		t.Fatalf("proxy: %s", r.body)
	}
	// autoBlock absent on update keeps the stored value
	r = e.admin(t, "PUT", "/-/api/repositories/raw-p", `{"name":"raw-p","format":"raw","type":"proxy","remote":"https://r.example/"}`)
	if wantStatus(t, r, http.StatusOK); !strings.Contains(r.body, `"blocked":false,"autoBlock":false`) {
		t.Fatalf("update: %s", r.body)
	}
}
