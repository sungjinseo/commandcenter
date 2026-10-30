package ui

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

const policiesPath = "/-/api/cleanup/policies"

func TestCleanupPolicyCRUDAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink

	r := e.admin(t, "POST", policiesPath,
		`{"name":"old-maven","format":"maven","componentAgeDays":30,"releaseType":"prerelease","namePattern":"org\\.example:.*"}`)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	if m["name"] != "old-maven" || m["format"] != "maven" || m["componentAgeDays"] != float64(30) ||
		m["lastDownloadedDays"] != float64(0) || m["releaseType"] != "prerelease" || m["namePattern"] != `org\.example:.*` ||
		m["created"] == "" || len(m["repositories"].([]any)) != 0 {
		t.Fatalf("create: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"old-maven","format":"maven","componentAgeDays":1}`), http.StatusConflict)
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"any-age","format":"all","lastDownloadedDays":90}`), http.StatusCreated)

	r = e.get(t, policiesPath, "admin")
	wantStatus(t, r, http.StatusOK)
	list := r.json(t)["policies"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["name"] != "any-age" || list[1].(map[string]any)["releaseType"] != "prerelease" {
		t.Fatalf("list: %s", r.body)
	}

	r = e.admin(t, "PUT", policiesPath+"/any-age", `{"format":"npm","componentAgeDays":10,"lastDownloadedDays":20}`)
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["format"] != "npm" || m["componentAgeDays"] != float64(10) || m["releaseType"] != "any" {
		t.Fatalf("update: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", policiesPath+"/any-age", `{"name":"renamed","format":"npm","componentAgeDays":10}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", policiesPath+"/missing", `{"format":"npm","componentAgeDays":10}`), http.StatusNotFound)
	r = e.get(t, policiesPath+"/any-age", "admin")
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["format"] != "npm" || m["lastDownloadedDays"] != float64(20) {
		t.Fatalf("get: %s", r.body)
	}
	wantStatus(t, e.get(t, policiesPath+"/missing", "admin"), http.StatusNotFound)

	wantStatus(t, e.admin(t, "DELETE", policiesPath+"/any-age", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "DELETE", policiesPath+"/any-age", ""), http.StatusNotFound)
	wantStatus(t, e.get(t, policiesPath+"/any-age", "admin"), http.StatusNotFound)

	var kinds []string
	for _, ev := range sink.Events() {
		if ev.Actor != "admin" || ev.Repo != "" {
			t.Fatalf("event: %+v", ev)
		}
		kinds = append(kinds, ev.Kind+" "+ev.Subject)
	}
	if got := strings.Join(kinds, ","); got != "policy.create old-maven,policy.create any-age,policy.update any-age,policy.delete any-age" {
		t.Fatalf("events: %s", got)
	}
}

func TestCleanupPolicyValidationAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, body := range []string{
		`{"name":"no-age","format":"all"}`,
		`{"name":"bad name","format":"all","componentAgeDays":1}`,
		`{"name":"create","format":"all","componentAgeDays":1}`,
		`{"name":"p","format":"docker","componentAgeDays":1}`,
		`{"name":"p","format":"all","componentAgeDays":1,"releaseType":"stable"}`,
		`{"name":"p","format":"all","componentAgeDays":1,"namePattern":"("}`,
		`{"name":"p","format":"all","componentAgeDays":1,"namePattern":"` + strings.Repeat("a", 201) + `"}`,
		`{"name":"p","format":"all","componentAgeDays":1,"unknown":true}`,
	} {
		r := e.admin(t, "POST", policiesPath, body)
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: status %d %s", body, r.status, r.body)
		}
	}
	if got := e.get(t, policiesPath, "admin").json(t)["policies"].([]any); len(got) != 0 {
		t.Fatalf("stored: %v", got)
	}
}

func TestRepositoryCleanupPolicyField(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"mvn","format":"maven","componentAgeDays":30}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"npm-only","format":"npm","componentAgeDays":30}`), http.StatusCreated)

	r := e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"writePolicy":"allow","cleanupPolicy":"mvn"}`)
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["cleanupPolicy"] != "mvn" {
		t.Fatalf("update: %s", r.body)
	}
	if got := e.get(t, "/-/api/repositories/maven-releases/config", "admin").json(t)["cleanupPolicy"]; got != "mvn" {
		t.Fatalf("config: %v", got)
	}
	// format mismatch, unknown policy, group, cargo hosted
	r = e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"writePolicy":"allow","cleanupPolicy":"npm-only"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "npm") {
		t.Fatalf("mismatch: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"cleanupPolicy":"ghost"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-public",
		`{"members":["maven-releases","maven-proxy"],"writeMember":"maven-releases","cleanupPolicy":"mvn"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"every","format":"all","componentAgeDays":30}`), http.StatusCreated)
	r = e.admin(t, "POST", "/-/api/repositories", `{"name":"crates","format":"cargo","type":"hosted","cleanupPolicy":"every"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "cargo") {
		t.Fatalf("cargo: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"npm-new","format":"npm","type":"hosted","cleanupPolicy":"npm-only"}`), http.StatusCreated)

	// The policy list shows the connected repositories.
	for _, p := range e.get(t, policiesPath, "admin").json(t)["policies"].([]any) {
		pm := p.(map[string]any)
		repos := fmt.Sprint(pm["repositories"])
		want := map[string]string{"mvn": "[maven-releases]", "npm-only": "[npm-new]", "every": "[]"}[pm["name"].(string)]
		if repos != want {
			t.Fatalf("%s repositories: %s, want %s", pm["name"], repos, want)
		}
	}

	// In use: delete is 409 naming the repositories, a format change is refused.
	r = e.admin(t, "DELETE", policiesPath+"/mvn", "")
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(r.json(t)["error"].(string), "maven-releases") {
		t.Fatalf("409: %s", r.body)
	}
	r = e.admin(t, "PUT", policiesPath+"/mvn", `{"format":"npm","componentAgeDays":30}`)
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(r.body, "maven-releases") {
		t.Fatalf("format change: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", policiesPath+"/mvn", `{"format":"all","componentAgeDays":60}`), http.StatusOK)

	// Once no repository uses it, it can go.
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"writePolicy":"allow","cleanupPolicy":""}`), http.StatusOK)
	wantStatus(t, e.admin(t, "DELETE", policiesPath+"/mvn", ""), http.StatusNoContent)
}

func TestCleanupPreviewAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.now = func() time.Time { return t0.Add(60 * 24 * time.Hour) }
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"old","format":"maven","componentAgeDays":30}`), http.StatusCreated)

	r := e.get(t, policiesPath+"/old/preview?repo=maven-releases", "admin")
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	comps := m["components"].([]any)
	if m["count"] != float64(2) || m["bytes"] != float64(19) || m["truncated"] != false || len(comps) != 2 {
		t.Fatalf("preview: %s", r.body)
	}
	c := comps[0].(map[string]any)
	if c["repo"] != "maven-releases" || c["group"] != "org.example" || c["name"] != "app" || c["version"] != "1.0" ||
		c["assets"] != float64(2) || c["bytes"] != float64(8) || c["created"] != rfc3339(t0) || c["lastDownloaded"] != "" {
		t.Fatalf("component: %v", c)
	}

	// Without repo: every repository connected to the policy (none yet).
	r = e.get(t, policiesPath+"/old/preview", "admin")
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["count"] != float64(0) || len(m["components"].([]any)) != 0 {
		t.Fatalf("unconnected: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"cleanupPolicy":"old"}`), http.StatusOK)
	if m := e.get(t, policiesPath+"/old/preview", "admin").json(t); m["count"] != float64(2) {
		t.Fatalf("connected: %v", m)
	}

	// Refused targets.
	wantStatus(t, e.get(t, policiesPath+"/old/preview?repo=maven-public", "admin"), http.StatusBadRequest)
	wantStatus(t, e.get(t, policiesPath+"/old/preview?repo=npm-hosted", "admin"), http.StatusBadRequest)
	wantStatus(t, e.get(t, policiesPath+"/old/preview?repo=nope", "admin"), http.StatusNotFound)
	wantStatus(t, e.get(t, policiesPath+"/nope/preview?repo=maven-releases", "admin"), http.StatusNotFound)
}

func TestCleanupPreviewCaps(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.now = func() time.Time { return t0.Add(60 * 24 * time.Hour) }
	defer func(l, b int) { previewLimit, previewScanBudget = l, b }(previewLimit, previewScanBudget)
	previewLimit = 2
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		p := fmt.Sprintf("x%d/-/x%d-1.0.0.tgz", i, i)
		if err := e.db.PutAsset(ctx, meta.Asset{Repo: "npm-private", Path: p, BlobSHA256: "s", Size: 1, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"old","format":"npm","componentAgeDays":30}`), http.StatusCreated)
	m := e.get(t, policiesPath+"/old/preview?repo=npm-private", "admin").json(t)
	if len(m["components"].([]any)) != 2 || m["count"] != float64(5) || m["bytes"] != float64(5) || m["truncated"] != true {
		t.Fatalf("list cap: %v", m)
	}
	previewLimit, previewScanBudget = 500, 3
	m = e.get(t, policiesPath+"/old/preview?repo=npm-private", "admin").json(t)
	if m["count"] != float64(3) || m["truncated"] != true {
		t.Fatalf("scan budget: %v", m)
	}
}

func TestCleanupPolicyAuth(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"p","format":"all","componentAgeDays":30}`), http.StatusCreated)
	reads := []string{policiesPath, policiesPath + "/p", policiesPath + "/p/preview?repo=maven-releases"}
	for _, p := range reads {
		wantStatus(t, e.get(t, p, ""), http.StatusUnauthorized)
		wantStatus(t, e.get(t, p, "ci"), http.StatusForbidden)
		wantStatus(t, e.get(t, p, "reader"), http.StatusForbidden)
		wantStatus(t, e.get(t, p, "admin"), http.StatusOK)
	}
	body := `{"name":"q","format":"all","componentAgeDays":30}`
	muts := []struct{ method, path, body string }{
		{"POST", policiesPath, body},
		{"PUT", policiesPath + "/p", `{"format":"all","componentAgeDays":5}`},
		{"DELETE", policiesPath + "/p", ""},
	}
	for _, c := range muts {
		wantStatus(t, e.req(t, c.method, c.path, c.body, "", nil, true), http.StatusUnauthorized)
		wantStatus(t, e.req(t, c.method, c.path, c.body, "ci", nil, true), http.StatusForbidden)
		// admin without the UI header: basic auth is refused, and a session
		// cookie without the header does not authenticate at all
		wantStatus(t, e.req(t, c.method, c.path, c.body, "admin", nil, false), http.StatusForbidden)
		wantStatus(t, e.req(t, c.method, c.path, c.body, "", e.cookieFor("admin"), false), http.StatusUnauthorized)
	}
	// an admin token is not interactive
	_, tok := e.newToken(t, "admin", "")
	for _, c := range muts {
		req, _ := http.NewRequest(c.method, e.ts.URL+c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set(auth.UIHeader, "1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("token %s %s: %d", c.method, c.path, res.StatusCode)
		}
	}
	m := e.get(t, policiesPath+"/p", "admin").json(t)
	if m["componentAgeDays"] != float64(30) {
		t.Fatalf("policy changed: %v", m)
	}
	if got := e.get(t, policiesPath, "admin").json(t)["policies"].([]any); len(got) != 1 {
		t.Fatalf("policies: %v", got)
	}
}

func TestRepositoryUpdateKeepsCleanupPolicyWhenAbsent(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"mvn","format":"maven","componentAgeDays":30}`), http.StatusCreated)
	policyOf := func() string {
		t.Helper()
		return e.get(t, "/-/api/repositories/maven-releases/config", "admin").json(t)["cleanupPolicy"].(string)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"writePolicy":"allow","cleanupPolicy":"mvn"}`), http.StatusOK)
	// absent and null keep the stored policy (a form that does not know the field)
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"writePolicy":"deny"}`), http.StatusOK)
	if got := policyOf(); got != "mvn" {
		t.Fatalf("absent: %q", got)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"writePolicy":"allow","cleanupPolicy":null}`), http.StatusOK)
	if got := policyOf(); got != "mvn" {
		t.Fatalf("null: %q", got)
	}
	// "" clears it
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"writePolicy":"allow","cleanupPolicy":""}`), http.StatusOK)
	if got := policyOf(); got != "" {
		t.Fatalf("cleared: %q", got)
	}
	// create without the field: none
	r := e.admin(t, "POST", "/-/api/repositories", `{"name":"m2","format":"maven","type":"hosted"}`)
	wantStatus(t, r, http.StatusCreated)
	if r.json(t)["cleanupPolicy"] != "" {
		t.Fatalf("create: %s", r.body)
	}
}

func TestCleanupPolicyCountCap(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	defer func(n int) { meta.MaxCleanupPolicies = n }(meta.MaxCleanupPolicies)
	meta.MaxCleanupPolicies = 2
	for _, n := range []string{"p1", "p2"} {
		wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"`+n+`","format":"all","componentAgeDays":30}`), http.StatusCreated)
	}
	r := e.admin(t, "POST", policiesPath, `{"name":"p3","format":"all","componentAgeDays":30}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "at most 2") {
		t.Fatalf("cap: %s", r.body)
	}
	// a duplicate at the cap is still a conflict
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"p1","format":"all","componentAgeDays":30}`), http.StatusConflict)
	wantStatus(t, e.admin(t, "DELETE", policiesPath+"/p2", ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"p3","format":"all","componentAgeDays":30}`), http.StatusCreated)
}

func TestCleanupPreviewTimeout(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"old","format":"maven","componentAgeDays":30}`), http.StatusCreated)
	defer func(d time.Duration) { previewTimeout = d }(previewTimeout)
	previewTimeout = time.Nanosecond
	r := e.get(t, policiesPath+"/old/preview?repo=maven-releases", "admin")
	wantStatus(t, r, http.StatusGatewayTimeout)
	if !strings.Contains(r.body, "did not finish") {
		t.Fatalf("timeout: %s", r.body)
	}
}

func TestCleanupPreviewClientCancel(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	var logs bytes.Buffer
	e.ui.Log = slog.New(slog.NewTextHandler(&logs, nil))
	wantStatus(t, e.admin(t, "POST", policiesPath, `{"name":"old","format":"maven","componentAgeDays":30}`), http.StatusCreated)
	admin := auth.Principal{User: "admin", Admin: true, Via: auth.ViaPassword}
	call := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", policiesPath+"/old/preview?repo=maven-releases", nil)
		req.SetPathValue("name", "old")
		req = req.WithContext(auth.WithPrincipal(ctx, admin))
		rec := httptest.NewRecorder()
		e.ui.previewPolicy(rec, req)
		return rec
	}

	// Cancelled before it starts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rec := call(ctx); rec.Code != statusClientClosed || !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("cancelled: %d %s", rec.Code, rec.Body)
	}

	// Cancelled while waiting for a heavy-query slot.
	var releases []func()
	for i := 0; i < heavySlots; i++ {
		rel, err := e.ui.acquireHeavy(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- call(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	rec := <-done
	for _, rel := range releases {
		rel()
	}
	if rec.Code != statusClientClosed {
		t.Fatalf("cancelled while waiting: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), "timed out") {
		t.Fatalf("logged: %s", logs.String())
	}
}

// Several cleanup policies per repository (core-14): cleanupPolicies sets
// them in order, cleanupPolicy is the first; a client that sends only
// cleanupPolicy, or changes it in a body read back, sets that one alone.
func TestRepositorySeveralCleanupPolicies(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, b := range []string{`{"name":"mvn","format":"maven","componentAgeDays":30}`, `{"name":"keep3","format":"all","retain":3}`,
		`{"name":"npm-only","format":"npm","componentAgeDays":30}`} {
		wantStatus(t, e.admin(t, "POST", policiesPath, b), http.StatusCreated)
	}
	const repoPath = "/-/api/repositories/maven-releases"
	policies := func(m map[string]any) string { return fmt.Sprintf("%v %v", m["cleanupPolicy"], m["cleanupPolicies"]) }
	r := e.admin(t, "PUT", repoPath, `{"writePolicy":"allow","cleanupPolicies":["mvn","keep3","mvn"]}`)
	wantStatus(t, r, http.StatusOK)
	if got := policies(r.json(t)); got != "mvn [mvn keep3]" {
		t.Fatalf("set: %s", got)
	}
	cfg := e.get(t, repoPath+"/config", "admin").json(t)
	if got := policies(cfg); got != "mvn [mvn keep3]" {
		t.Fatalf("config: %s", got)
	}
	// Both policies list the repository; the second cannot be deleted.
	for _, p := range e.get(t, policiesPath, "admin").json(t)["policies"].([]any) {
		pm := p.(map[string]any)
		if want := map[string]string{"mvn": "[maven-releases]", "keep3": "[maven-releases]", "npm-only": "[]"}[pm["name"].(string)]; fmt.Sprint(pm["repositories"]) != want {
			t.Fatalf("%s repositories: %v", pm["name"], pm["repositories"])
		}
	}
	wantStatus(t, e.admin(t, "DELETE", policiesPath+"/keep3", ""), http.StatusConflict)
	// An update without either keeps them; the body read back keeps them too.
	if got := policies(e.admin(t, "PUT", repoPath, `{"writePolicy":"allow"}`).json(t)); got != "mvn [mvn keep3]" {
		t.Fatalf("kept: %s", got)
	}
	if got := policies(e.admin(t, "PUT", repoPath, `{"writePolicy":"allow","cleanupPolicy":"mvn","cleanupPolicies":["mvn","keep3"]}`).json(t)); got != "mvn [mvn keep3]" {
		t.Fatalf("read back: %s", got)
	}
	// A changed cleanupPolicy wins over the list read back with it.
	if got := policies(e.admin(t, "PUT", repoPath, `{"writePolicy":"allow","cleanupPolicy":"keep3","cleanupPolicies":["mvn","keep3"]}`).json(t)); got != "keep3 [keep3]" {
		t.Fatalf("changed cleanupPolicy: %s", got)
	}
	// Every policy is checked, each error named.
	r = e.admin(t, "PUT", repoPath, `{"cleanupPolicies":["mvn","ghost","npm-only"]}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "ghost") || !strings.Contains(r.body, "npm-only") {
		t.Fatalf("bad: %s", r.body)
	}
	if got := policies(e.admin(t, "PUT", repoPath, `{"writePolicy":"allow","cleanupPolicies":[]}`).json(t)); got != " []" {
		t.Fatalf("cleared: %s", got)
	}
	wantStatus(t, e.admin(t, "DELETE", policiesPath+"/keep3", ""), http.StatusNoContent)
}
