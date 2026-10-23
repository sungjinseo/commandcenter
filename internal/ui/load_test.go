package ui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

func TestStatsAreCachedFor30s(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	var repoCalls, blobCalls atomic.Int32
	e.ui.repoStatsQuery = func(ctx context.Context) (map[string]meta.RepoStat, error) {
		repoCalls.Add(1)
		return e.db.RepoStats(ctx)
	}
	e.ui.blobStatsQuery = func(ctx context.Context) (int64, int64, error) {
		blobCalls.Add(1)
		return e.db.BlobStats(ctx)
	}
	now := time.Now()
	e.ui.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		wantStatus(t, e.get(t, "/-/api/repositories", "admin"), http.StatusOK)
		wantStatus(t, e.get(t, "/-/api/status", "admin"), http.StatusOK)
	}
	if repoCalls.Load() != 1 || blobCalls.Load() != 1 {
		t.Fatalf("queries repo=%d blob=%d, want 1 each within the TTL", repoCalls.Load(), blobCalls.Load())
	}
	now = now.Add(31 * time.Second)
	wantStatus(t, e.get(t, "/-/api/repositories", "admin"), http.StatusOK)
	wantStatus(t, e.get(t, "/-/api/status", "admin"), http.StatusOK)
	if repoCalls.Load() != 2 || blobCalls.Load() != 2 {
		t.Fatalf("after expiry repo=%d blob=%d, want 2 each", repoCalls.Load(), blobCalls.Load())
	}
}

func TestNoStatsQueryWhenNothingReadable(t *testing.T) {
	no := false
	e := newEnvWith(t, "http://repo.example", func(cfg *config.Config) {
		cfg.AnonymousRead = false
		for i := range cfg.Repositories {
			cfg.Repositories[i].AnonymousRead = &no
		}
	})
	var calls atomic.Int32
	e.ui.repoStatsQuery = func(ctx context.Context) (map[string]meta.RepoStat, error) {
		calls.Add(1)
		return e.db.RepoStats(ctx)
	}
	r := e.get(t, "/-/api/repositories", "")
	wantStatus(t, r, http.StatusOK)
	if got := r.json(t)["repositories"].([]any); len(got) != 0 {
		t.Fatalf("anonymous sees %v", got)
	}
	if calls.Load() != 0 {
		t.Fatalf("RepoStats ran %d times for a caller who can read nothing", calls.Load())
	}
	wantStatus(t, e.get(t, "/-/api/repositories", "admin"), http.StatusOK)
	if calls.Load() != 1 {
		t.Fatalf("RepoStats calls = %d for an admin, want 1", calls.Load())
	}
}

func TestHeavySlotsAreBounded(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	var rel []func()
	for i := 0; i < heavySlots; i++ {
		release, err := e.ui.acquireHeavy(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.ui.acquireHeavy(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire with all slots busy: err = %v", err)
	}
	// A search waits for a slot with the request context and then gives up.
	sctx, scancel := context.WithTimeout(auth.WithPrincipal(context.Background(), auth.Principal{User: "admin", Admin: true}), 50*time.Millisecond)
	defer scancel()
	w := httptest.NewRecorder()
	e.ui.search(w, httptest.NewRequest("GET", "/-/api/search?q=app", nil).WithContext(sctx))
	if w.Code < 500 {
		t.Fatalf("search with no free slot: status %d: %s", w.Code, w.Body)
	}
	rel[0]()
	release, err := e.ui.acquireHeavy(context.Background())
	if err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	release()
	for _, r := range rel[1:] {
		r()
	}
	wantStatus(t, e.get(t, "/-/api/search?q=app", "admin"), http.StatusOK)
}

func basicTokenReq(t *testing.T, e *env, method, path, body, user, pass string, ui bool) resp {
	r, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	r.SetBasicAuth(user, pass)
	if ui {
		r.Header.Set(auth.UIHeader, "1")
	}
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: string(b), cookie: res.Cookies()}
}

func bearerReq(t *testing.T, e *env, method, path, body, tok string, ui bool) resp {
	r, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	if ui {
		r.Header.Set(auth.UIHeader, "1")
	}
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: string(b)}
}

func TestTokensCannotMintTokensOrSessions(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	id, tok := e.newToken(t, "ci", "")

	// Bearer token plus the UI header: still no new token, no revocation.
	wantStatus(t, bearerReq(t, e, "POST", "/-/api/tokens", "{}", tok, true), http.StatusForbidden)
	wantStatus(t, bearerReq(t, e, "DELETE", "/-/api/tokens/"+id, "", tok, true), http.StatusForbidden)
	// The token as Basic password, same.
	wantStatus(t, basicTokenReq(t, e, "POST", "/-/api/tokens", "{}", "ci", tok, true), http.StatusForbidden)
	wantStatus(t, basicTokenReq(t, e, "DELETE", "/-/api/tokens/"+id, "", "ci", tok, true), http.StatusForbidden)
	// Login takes real passwords only.
	body := `{"user":"ci","password":"` + tok + `"}`
	wantStatus(t, e.req(t, "POST", "/-/api/login", body, "", nil, true), http.StatusUnauthorized)

	// A session and the real password still work.
	c := e.cookieFor("ci")
	wantStatus(t, e.req(t, "POST", "/-/api/tokens", "{}", "", c, true), http.StatusCreated)
	wantStatus(t, e.req(t, "POST", "/-/api/tokens", "{}", "ci", nil, true), http.StatusCreated)
	wantStatus(t, e.req(t, "DELETE", "/-/api/tokens/"+id, "", "", c, true), http.StatusNoContent)
}

func TestTokenAsBasicPassword(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	_, tok := e.newToken(t, "ci", "")
	get := func(user, pass string) int {
		return basicTokenReq(t, e, "GET", "/-/api/me", "", user, pass, false).status
	}
	if st := get("ci", tok); st != http.StatusOK {
		t.Fatalf("user:token = %d, want 200", st)
	}
	if me := basicTokenReq(t, e, "GET", "/-/api/me", "", "ci", tok, false).json(t); me["user"] != "ci" {
		t.Fatalf("me = %v", me)
	}
	if st := get("admin", tok); st != http.StatusUnauthorized {
		t.Fatalf("otheruser:token = %d, want 401", st)
	}
}

// QA L-8: creating or deleting a repository drops the cached statistics, so a
// re-created empty repository is not shown with its predecessor's counts.
func TestRepoStatsCacheResetOnCreateAndDelete(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	var calls atomic.Int32
	e.ui.repoStatsQuery = func(ctx context.Context) (map[string]meta.RepoStat, error) {
		calls.Add(1)
		return e.db.RepoStats(ctx)
	}
	now := time.Now()
	e.ui.now = func() time.Time { return now }
	wantStatus(t, e.get(t, "/-/api/repositories", "admin"), http.StatusOK)
	wantStatus(t, e.get(t, "/-/api/repositories", "admin"), http.StatusOK)
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories", `{"name":"fresh","format":"maven","type":"hosted"}`), http.StatusCreated)
	wantStatus(t, e.get(t, "/-/api/repositories", "admin"), http.StatusOK)
	if calls.Load() != 2 {
		t.Fatalf("after create calls = %d, want 2", calls.Load())
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/repositories/fresh", ""), http.StatusNoContent)
	wantStatus(t, e.get(t, "/-/api/repositories", "admin"), http.StatusOK)
	if calls.Load() != 3 {
		t.Fatalf("after delete calls = %d, want 3", calls.Load())
	}
}
