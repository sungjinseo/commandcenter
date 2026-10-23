package ui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
)

func (e *env) cookieFor(user string) *http.Cookie {
	return &http.Cookie{Name: auth.SessionCookie, Value: e.ses.Create(user)}
}

// bearer GETs path with an API token and returns the status.
func (e *env) bearer(t *testing.T, path, tok string) int {
	t.Helper()
	r, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func (e *env) newToken(t *testing.T, user, body string) (id, tok string) {
	t.Helper()
	r := e.req(t, "POST", "/-/api/tokens", body, user, nil, true)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	return m["id"].(string), m["token"].(string)
}

func TestTokenLifecycleAsOwner(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	id, tok := e.newToken(t, "ci", "")
	if id == "" || !strings.HasPrefix(tok, "cc_") {
		t.Fatalf("id %q token %q", id, tok)
	}
	if st := e.bearer(t, "/repository/maven-releases/readme.txt", tok); st == http.StatusUnauthorized || st == http.StatusForbidden {
		t.Fatalf("token rejected: %d", st)
	}
	r := e.get(t, "/-/api/tokens", "ci")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	m := r.json(t)
	toks := m["tokens"].([]any)
	if len(toks) != 1 || m["truncated"] != false {
		t.Fatalf("list: %s", r.body)
	}
	row := toks[0].(map[string]any)
	if row["id"] != id || row["user"] != "ci" || row["created"] == "" {
		t.Fatalf("row: %v", row)
	}
	if strings.Contains(r.body, tok) || strings.Contains(r.body, "hash") {
		t.Fatalf("secret leaked: %s", r.body)
	}
	wantStatus(t, e.req(t, "DELETE", "/-/api/tokens/"+id, "", "ci", nil, true), http.StatusNoContent)
	if st := e.bearer(t, "/repository/maven-releases/readme.txt", tok); st != http.StatusUnauthorized {
		t.Fatalf("revoked token status %d, want 401", st)
	}
	wantStatus(t, e.req(t, "DELETE", "/-/api/tokens/"+id, "", "ci", nil, true), http.StatusNotFound)
	if got := e.get(t, "/-/api/tokens", "ci").json(t)["tokens"].([]any); len(got) != 0 {
		t.Fatalf("list after delete: %v", got)
	}
}

func TestTokenCreateBodies(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, body := range []string{"", "{}", `{"user":"ci"}`} {
		r := e.req(t, "POST", "/-/api/tokens", body, "ci", nil, true)
		wantStatus(t, r, http.StatusCreated)
		if r.json(t)["user"] != "ci" {
			t.Fatalf("body %q: %s", body, r.body)
		}
	}
	wantStatus(t, e.req(t, "POST", "/-/api/tokens", `{"user":`, "ci", nil, true), http.StatusBadRequest)
	wantStatus(t, e.req(t, "POST", "/-/api/tokens", `{"bogus":1}`, "ci", nil, true), http.StatusBadRequest)
}

func TestTokenForOtherUser(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.req(t, "POST", "/-/api/tokens", `{"user":"admin"}`, "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/tokens", `{"user":"ghost"}`, "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/tokens", `{"user":"ghost"}`, "admin", nil, true), http.StatusBadRequest)
	_, tok := e.newToken(t, "admin", `{"user":"reader"}`)
	r := e.get(t, "/-/api/tokens", "reader")
	if got := r.json(t)["tokens"].([]any); len(got) != 1 {
		t.Fatalf("reader tokens: %s", r.body)
	}
	if st := e.bearer(t, "/-/api/me", tok); st != http.StatusOK {
		t.Fatalf("token for reader: %d", st)
	}
}

func TestTokenDeleteOthersIs404AndAdminSeesAll(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	adminID, _ := e.newToken(t, "admin", "")
	ciID, _ := e.newToken(t, "ci", "")
	wantStatus(t, e.req(t, "DELETE", "/-/api/tokens/"+adminID, "", "ci", nil, true), http.StatusNotFound)
	// The same status as an id that does not exist at all.
	wantStatus(t, e.req(t, "DELETE", "/-/api/tokens/nope", "", "ci", nil, true), http.StatusNotFound)
	if got := e.get(t, "/-/api/tokens", "ci").json(t)["tokens"].([]any); len(got) != 1 {
		t.Fatalf("ci sees %d tokens, want 1", len(got))
	}
	if got := e.get(t, "/-/api/tokens", "admin").json(t)["tokens"].([]any); len(got) != 2 {
		t.Fatalf("admin sees %d tokens, want 2", len(got))
	}
	wantStatus(t, e.req(t, "DELETE", "/-/api/tokens/"+ciID, "", "admin", nil, true), http.StatusNoContent)
}

func TestTokenListTruncated(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for i := 0; i <= tokensLimit; i++ {
		id := fmt.Sprintf("bulk%04d", i)
		if err := e.db.CreateToken(context.Background(), id, "ci", "h-"+id, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	m := e.get(t, "/-/api/tokens", "ci").json(t)
	if len(m["tokens"].([]any)) != tokensLimit || m["truncated"] != true {
		t.Fatalf("got %d tokens truncated=%v", len(m["tokens"].([]any)), m["truncated"])
	}
}

func TestAdminEndpointsAuthLevels(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Tasks.GC = func(context.Context, string) (int, error) { return 0, nil }
	cases := []struct{ method, path string }{
		{"GET", "/-/api/tokens"}, {"POST", "/-/api/tokens"}, {"DELETE", "/-/api/tokens/x"},
		{"POST", "/-/api/gc"}, {"GET", "/-/api/status"},
	}
	for _, c := range cases {
		wantStatus(t, e.req(t, c.method, c.path, "", "", nil, true), http.StatusUnauthorized)
	}
	for _, c := range cases[3:] {
		wantStatus(t, e.req(t, c.method, c.path, "", "ci", nil, true), http.StatusForbidden)
	}
}

func TestAdminMutationsNeedUIHeader(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Tasks.GC = func(context.Context, string) (int, error) { t.Error("gc ran"); return 0, nil }
	id, _ := e.newToken(t, "admin", "")
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/-/api/tokens", ""}, {"DELETE", "/-/api/tokens/" + id, ""}, {"POST", "/-/api/gc", ""},
	} {
		// Basic auth without the header, then cookie-only without the header.
		for _, r := range []resp{
			e.req(t, c.method, c.path, c.body, "admin", nil, false),
			e.req(t, c.method, c.path, c.body, "", e.cookieFor("admin"), false),
		} {
			if r.status < 400 {
				t.Fatalf("%s %s without header: %d", c.method, c.path, r.status)
			}
		}
	}
	if got := e.get(t, "/-/api/tokens", "admin").json(t)["tokens"].([]any); len(got) != 1 {
		t.Fatalf("token was deleted or created: %v", got)
	}
}

func TestTokensWithSessionCookie(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.req(t, "POST", "/-/api/tokens", "", "", e.cookieFor("ci"), true)
	wantStatus(t, r, http.StatusCreated)
	if r.json(t)["user"] != "ci" {
		t.Fatal(r.body)
	}
}

func TestGCConcurrency(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e.ui.Tasks.GC = func(context.Context, string) (int, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return 7, nil
	}
	first := make(chan resp, 1)
	go func() { first <- e.req(t, "POST", "/-/api/gc", "", "admin", nil, true) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("gc did not start")
	}
	r := e.req(t, "POST", "/-/api/gc", "", "admin", nil, true)
	wantStatus(t, r, http.StatusConflict)
	if r.json(t)["error"] != "a task is already running" {
		t.Fatalf("body %s", r.body)
	}
	close(release)
	select {
	case r1 := <-first:
		wantStatus(t, r1, http.StatusOK)
		m := r1.json(t)
		if m["removed"] != float64(7) {
			t.Fatalf("body %s", r1.body)
		}
		if _, ok := m["ms"]; !ok {
			t.Fatalf("no ms: %s", r1.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first gc did not finish")
	}
	// The lock is released: a later run proceeds.
	e.ui.Tasks.GC = func(context.Context, string) (int, error) { return 0, nil }
	wantStatus(t, e.req(t, "POST", "/-/api/gc", "", "admin", nil, true), http.StatusOK)
	if calls.Load() != 1 {
		t.Fatalf("blocking gc called %d times", calls.Load())
	}
}

func TestStatus(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, "/-/api/status", "admin")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	m := r.json(t)
	for _, k := range []string{"version", "uptimeSeconds", "rssBytes", "heapAllocBytes", "sysBytes",
		"goroutines", "numGC", "repositories", "blobBytes", "blobs"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q: %s", k, r.body)
		}
	}
	if m["version"] != "test" {
		t.Errorf("version %v", m["version"])
	}
	if m["blobs"] != float64(6) || m["blobBytes"] != float64(3+5+7+11+13+17) {
		t.Errorf("blobs %v bytes %v", m["blobs"], m["blobBytes"])
	}
	repos := m["repositories"].([]any)
	if len(repos) != 7 {
		t.Fatalf("%d repositories: %s", len(repos), r.body)
	}
	found := false
	for _, x := range repos {
		row := x.(map[string]any)
		if row["name"] == "maven-releases" {
			found = true
			if row["assets"] != float64(5) || row["bytes"] != float64(39) {
				t.Errorf("maven-releases row %v", row)
			}
		}
	}
	if !found {
		t.Error("maven-releases missing")
	}
}

func TestStatusZeroStartedUptime(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Started = time.Time{}
	if got := e.get(t, "/-/api/status", "admin").json(t)["uptimeSeconds"]; got != float64(0) {
		t.Fatalf("uptimeSeconds %v", got)
	}
}

func TestInteractiveRequiredMessage(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	_, tok := e.newToken(t, "admin", "")
	r, _ := http.NewRequest("POST", e.ts.URL+"/-/api/repositories", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("X-Command-Center-UI", "1")
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || strings.Contains(string(b), "manage tokens") ||
		!strings.Contains(string(b), "requires a password or session login") {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}
