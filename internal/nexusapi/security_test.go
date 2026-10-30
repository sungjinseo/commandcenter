package nexusapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// startSecurity is start with routing rules and content selectors in the
// config file, imported on the first start like roles.
func startSecurity(t *testing.T) *env {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
dataDir: %s
baseURL: http://nexus.example.com
anonymousRead: false
users:
  - { name: admin, passwordHash: %q, roles: [admin] }
  - { name: ci, passwordHash: %q, roles: [deployer] }
roles:
  deployer: { write: [maven-releases] }
routingRules:
  - { name: no-internal, description: "keep internal off the remote", mode: BLOCK, matchers: ["^/internal/"] }
contentSelectors:
  - { name: acme, description: "acme", expression: 'path =^ "/org/acme/"' }
repositories:
  - { name: maven-releases, format: maven, type: hosted, writePolicy: allow_once }
  - { name: maven-central, format: maven, type: proxy, remote: https://repo1.maven.org/maven2/, contentMaxAge: -1, routingRule: no-internal }
`, t.TempDir(), string(h), string(h))))
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

func TestRoutingRulesCompat(t *testing.T) {
	e := startSecurity(t)
	// Imported from the config file and applied: the proxy refuses the path
	// before any remote request.
	var rules []map[string]any
	e.get(v1+"/routing-rules", "admin", &rules)
	if len(rules) != 1 || rules[0]["name"] != "no-internal" || rules[0]["mode"] != "BLOCK" {
		t.Fatalf("rules = %v", rules)
	}
	e.want("GET", "/repository/maven-central/internal/x.jar", "ci", "", 403)
	e.want("GET", v1+"/routing-rules", "ci", "", 403)

	e.want("POST", v1+"/routing-rules", "admin", `{"name":"bad","mode":"BLOCK","matchers":["("]}`, 400)
	e.want("POST", v1+"/routing-rules", "admin", `{"name":"only-org","description":"d","mode":"ALLOW","matchers":["/org/.*"]}`, 204)
	e.want("POST", v1+"/routing-rules", "admin", `{"name":"only-org","mode":"ALLOW","matchers":["/org/.*"]}`, 409)
	var rule map[string]any
	e.get(v1+"/routing-rules/only-org", "admin", &rule)
	if rule["mode"] != "ALLOW" || rule["description"] != "d" || fmt.Sprint(rule["matchers"]) != "[/org/.*]" {
		t.Fatalf("rule = %v", rule)
	}
	// Nexus matches the whole path: /org/.* is stored anchored, so a path
	// that only contains /org/ is not matched by it.
	e.want("PUT", v1+"/routing-rules/only-org", "admin", `{"name":"only-org","mode":"BLOCK","matchers":["/org/.*","/com/.*"]}`, 204)
	e.get(v1+"/routing-rules/only-org", "admin", &rule)
	if rule["mode"] != "BLOCK" || fmt.Sprint(rule["matchers"]) != "[/org/.* /com/.*]" {
		t.Fatalf("updated rule = %v", rule)
	}
	var native map[string]any
	e.get("/-/api/routing-rules/only-org", "admin", &native)
	if fmt.Sprint(native["matchers"]) != "[^(?:/org/.*)$ ^(?:/com/.*)$]" {
		t.Fatalf("stored matchers = %v", native["matchers"])
	}

	// The repository bodies carry routingRule (and routingRuleName on read).
	e.want("PUT", v1+"/repositories/maven/proxy/maven-central", "admin",
		`{"name":"maven-central","online":true,"storage":{"blobStoreName":"default"},"proxy":{"remoteUrl":"https://repo1.maven.org/maven2/","contentMaxAge":-1,"metadataMaxAge":60},"negativeCache":{"enabled":true,"timeToLive":5},"routingRule":"only-org"}`, 204)
	var s map[string]any
	e.get(v1+"/repositories/maven/proxy/maven-central", "admin", &s)
	if s["routingRule"] != "only-org" || s["routingRuleName"] != "only-org" {
		t.Fatalf("settings = %v", s)
	}
	e.want("GET", "/repository/maven-central/org/x.jar", "ci", "", 403)
	if st, b, _ := e.do("GET", "/repository/maven-central/x/org/a.jar", "ci", ""); st == 403 { // goes to the remote
		t.Fatalf("/x/org/a.jar: %d %s", st, b)
	}
	e.want("DELETE", v1+"/routing-rules/only-org", "admin", "", 400) // in use by maven-central: Nexus's 400
	e.want("DELETE", v1+"/routing-rules/no-internal", "admin", "", 204)
	e.want("GET", v1+"/routing-rules/no-internal", "admin", "", 404)
}

func TestContentSelectorsCompat(t *testing.T) {
	e := startSecurity(t)
	var sels []map[string]any
	e.get(v1+"/security/content-selectors", "admin", &sels)
	if len(sels) != 1 || sels[0]["name"] != "acme" || sels[0]["type"] != "csel" || sels[0]["expression"] != `path =^ "/org/acme/"` {
		t.Fatalf("selectors = %v", sels)
	}
	e.want("POST", v1+"/security/content-selectors", "admin", `{"name":"bad","expression":"path = x"}`, 400)
	e.want("POST", v1+"/security/content-selectors", "admin", `{"name":"snapshots","description":"d","expression":"path =~ \".*-SNAPSHOT.*\""}`, 204)
	e.want("PUT", v1+"/security/content-selectors/snapshots", "admin", `{"description":"d2","expression":"path =~ \".*SNAPSHOT.*\""}`, 204)
	var sel map[string]any
	e.get(v1+"/security/content-selectors/snapshots", "admin", &sel)
	if sel["description"] != "d2" || sel["expression"] != `path =~ ".*SNAPSHOT.*"` {
		t.Fatalf("selector = %v", sel)
	}
	e.want("DELETE", v1+"/security/content-selectors/snapshots", "admin", "", 204)
	e.want("GET", v1+"/security/content-selectors/snapshots", "admin", "", 404)

	// A content-selector privilege becomes the role priv:<name> with one
	// privilege; it can be assigned, and it scopes access to the paths.
	e.want("POST", v1+"/security/privileges/repository-content-selector", "admin",
		`{"name":"acme-read","description":"read acme","actions":["BROWSE","READ"],"format":"maven2","repository":"maven-releases","contentSelector":"acme"}`, 201)
	e.want("POST", v1+"/security/privileges/repository-content-selector", "admin",
		`{"name":"acme-read","actions":["READ"],"format":"maven2","repository":"maven-releases","contentSelector":"acme"}`, 400) // Nexus: a taken name is 400
	e.want("POST", v1+"/security/privileges/repository-content-selector", "admin",
		`{"name":"bad","actions":["READ"],"format":"maven2","repository":"nope","contentSelector":"acme"}`, 400)
	var priv map[string]any
	e.get(v1+"/security/privileges/acme-read", "admin", &priv)
	if priv["type"] != "repository-content-selector" || priv["contentSelector"] != "acme" || priv["repository"] != "maven-releases" ||
		fmt.Sprint(priv["actions"]) != "[BROWSE READ]" || priv["description"] != "read acme" || priv["readOnly"] != false {
		t.Fatalf("privilege = %v", priv)
	}
	e.want("POST", "/-/api/users", "admin", "", 403) // not through the Nexus API: no UI header
	e.upload("maven-releases/org/acme/lib/1.0/lib-1.0.jar", "maven-releases/org/other/lib/1.0/lib-1.0.jar")
	// Assign through the UI's user API (the Nexus users API is read-only);
	// authenticatedRead off, so only the privilege grants reads.
	if st, b := uiCall(e, "PUT", "/-/api/settings", `{"authenticatedRead":false}`); st != 200 {
		t.Fatalf("settings: %d %s", st, b)
	}
	st, b := uiPost(e, "/-/api/users", `{"id":"dev","password":"longenough","roles":["priv:acme-read"]}`)
	if st != 201 {
		t.Fatalf("create user: %d %s", st, b)
	}
	if st, b := getAs(e, "/repository/maven-releases/org/acme/lib/1.0/lib-1.0.jar", "dev", "longenough"); st != 200 {
		t.Fatalf("acme path as dev: %d %s", st, b)
	}
	if st, _ := getAs(e, "/repository/maven-releases/org/other/lib/1.0/lib-1.0.jar", "dev", "longenough"); st != 403 {
		t.Fatalf("other path as dev: %d", st)
	}
	// PUT replaces the privilege; "*" is any repository.
	e.want("PUT", v1+"/security/privileges/repository-content-selector/acme-read", "admin",
		`{"name":"acme-read","description":"all repos","actions":["read","add"],"format":"*","repository":"*","contentSelector":"acme"}`, 204)
	var role map[string]any
	st, b = uiGet(e, "/-/api/roles/priv:acme-read")
	if st != 200 || json.Unmarshal(b, &role) != nil || !strings.Contains(string(b), `"repos":["*"]`) || !strings.Contains(string(b), `"add"`) {
		t.Fatalf("role after PUT: %d %s", st, b)
	}
	e.want("DELETE", v1+"/security/content-selectors/acme", "admin", "", 409) // the role names it
}

// uiPost and uiGet call the web UI's JSON API as admin with the UI header.
func uiPost(e *env, path, body string) (int, []byte) { return uiCall(e, "POST", path, body) }
func uiGet(e *env, path string) (int, []byte)        { return uiCall(e, "GET", path, "") }

func uiCall(e *env, method, path, body string) (int, []byte) {
	return send(e, method, path, body, "admin", "pw", true)
}

func getAs(e *env, path, user, pass string) (int, []byte) {
	return send(e, "GET", path, "", user, pass, false)
}

func send(e *env, method, path, body, user, pass string, ui bool) (int, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	req.SetBasicAuth(user, pass)
	if ui {
		req.Header.Set("X-Command-Center-UI", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
