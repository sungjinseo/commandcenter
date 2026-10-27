package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/tasks"
)

func TestUsersEndpoint(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.get(t, "/-/api/users", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/users", "ci"), http.StatusForbidden)
	r := e.get(t, "/-/api/users", "admin")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	if strings.Contains(r.body, "$2a$") || strings.Contains(strings.ToLower(r.body), "hash") {
		t.Fatalf("hash leaked: %s", r.body)
	}
	users := r.json(t)["users"].([]any)
	if len(users) != 3 {
		t.Fatalf("users: %s", r.body)
	}
	byID := map[string]map[string]any{}
	for _, x := range users {
		m := x.(map[string]any)
		byID[m["userId"].(string)] = m
	}
	if byID["admin"]["admin"] != true || byID["ci"]["admin"] != false || byID["ci"]["realm"] != "Local" || byID["ci"]["status"] != "active" {
		t.Fatalf("rows: %s", r.body)
	}
	if w := byID["ci"]["writeRepositories"].([]any); len(w) != 1 || w[0] != "maven-releases" {
		t.Fatalf("ci writes: %v", w)
	}
}

func TestStatusChecksHealthy(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.memReader = func() (int64, int64) { return 10, 100 }
	e.ui.FetchSlots = func() (int, int) { return 1, 32 }
	wantStatus(t, e.get(t, "/-/api/status/checks", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/status/checks", "ci"), http.StatusForbidden)
	r := e.get(t, "/-/api/status/checks", "admin")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	checks := r.json(t)["checks"].([]any)
	var names []string
	for _, c := range checks {
		m := c.(map[string]any)
		names = append(names, m["name"].(string))
		if m["healthy"] != true || m["lastChecked"] == "" {
			t.Errorf("unhealthy: %v", m)
		}
	}
	want := "Database,Blob store,Memory,Remote fetch slots,Repositories"
	if strings.Join(names, ",") != want {
		t.Fatalf("names %v", names)
	}
}

func checkByName(t *testing.T, r resp, name string) map[string]any {
	t.Helper()
	for _, c := range r.json(t)["checks"].([]any) {
		if m := c.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no check %q", name)
	return nil
}

func TestStatusChecksMemoryAndSlots(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.memReader = func() (int64, int64) { return 90, 100 }
	e.ui.FetchSlots = func() (int, int) { return 32, 32 }
	r := e.get(t, "/-/api/status/checks", "admin")
	if m := checkByName(t, r, "Memory"); m["healthy"] != false {
		t.Fatalf("memory at 90%%: %v", m)
	}
	if m := checkByName(t, r, "Remote fetch slots"); m["healthy"] != false {
		t.Fatalf("slots full: %v", m)
	}
	e.ui.memReader = func() (int64, int64) { return 85, 100 }
	m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Memory")
	if m["healthy"] != true || !strings.Contains(strings.ToLower(m["message"].(string)), "warn") {
		t.Fatalf("memory at 85%%: %v", m)
	}
	e.ui.FetchSlots = nil // check skipped
	for _, c := range e.get(t, "/-/api/status/checks", "admin").json(t)["checks"].([]any) {
		if c.(map[string]any)["name"] == "Remote fetch slots" {
			t.Fatal("slots check should be skipped")
		}
	}
}

func TestSystemInformation(t *testing.T) {
	t.Setenv("COMMANDCENTER_SECRET_TEST", "topsecret")
	t.Setenv("HTTPS_PROXY", "http://user:pw@proxy.example:3128")
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.get(t, "/-/api/systeminformation", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/systeminformation", "reader"), http.StatusForbidden)
	r := e.get(t, "/-/api/systeminformation", "admin")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	m := r.json(t)
	for _, k := range []string{"commandcenter-status", "commandcenter-configuration", "system-runtime", "system-environment", "system-filestores"} {
		if _, ok := m[k].(map[string]any); !ok {
			t.Fatalf("missing section %s: %s", k, r.body)
		}
	}
	for _, bad := range []string{"COMMANDCENTER_SECRET_TEST", "topsecret", "proxy.example", "$2a$"} {
		if strings.Contains(r.body, bad) {
			t.Fatalf("leaked %q: %s", bad, r.body)
		}
	}
	env := m["system-environment"].(map[string]any)
	if env["HTTPS_PROXY"] != "yes" {
		t.Fatalf("env: %v", env)
	}
	for k := range env {
		if k != "GOMEMLIMIT" && k != "HTTPS_PROXY" && k != "TZ" {
			t.Errorf("unexpected env key %s", k)
		}
	}
	if m["commandcenter-status"].(map[string]any)["version"] != "test" {
		t.Fatalf("status: %v", m["commandcenter-status"])
	}
	if m["commandcenter-configuration"].(map[string]any)["userCount"] != float64(3) {
		t.Fatalf("config: %v", m["commandcenter-configuration"])
	}
}

func firstTask(t *testing.T, e *env) map[string]any {
	t.Helper()
	return e.get(t, "/-/api/tasks", "admin").json(t)["tasks"].([]any)[0].(map[string]any)
}

func TestTasks(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	e.ui.Tasks.GC = func(ctx context.Context, _ string) (int, int64, error) {
		once.Do(func() { close(started) })
		<-release
		return 3, 0, nil
	}
	wantStatus(t, e.get(t, "/-/api/tasks", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/tasks", "ci"), http.StatusForbidden)
	r := e.get(t, "/-/api/tasks", "admin")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	task := firstTask(t, e)
	if task["id"] != tasks.SeedID || task["status"] != "Waiting" || task["schedule"] != "daily:03:00" || task["lastRun"] != "" ||
		task["type"] != "blobstore.compact" || task["enabled"] != true || task["nextRun"] == "" {
		t.Fatalf("task: %v", task)
	}
	run := "/-/api/tasks/compact-blobstore/run"
	wantStatus(t, e.req(t, "POST", run, "", "admin", nil, false), http.StatusForbidden) // no UI header
	wantStatus(t, e.req(t, "POST", run, "", "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/tasks/nope/run", "", "admin", nil, true), http.StatusNotFound)

	// Run now answers 202 at once; the task shows Running until it is done.
	r = e.req(t, "POST", run, "", "admin", nil, true)
	wantStatus(t, r, http.StatusAccepted)
	if r.json(t)["started"] != true {
		t.Fatalf("run body: %s", r.body)
	}
	<-started
	if got := firstTask(t, e)["status"]; got != "Running" {
		t.Fatalf("status while running: %v", got)
	}
	wantStatus(t, e.req(t, "POST", run, "", "admin", nil, true), http.StatusConflict)
	wantStatus(t, e.req(t, "POST", "/-/api/gc", "", "admin", nil, true), http.StatusConflict) // alias shares state
	close(release)
	e.ui.Tasks.Wait()
	task = firstTask(t, e)
	if task["status"] != "Waiting" || task["lastRun"] == "" || task["lastResult"] != "OK" || task["lastRemoved"] != float64(3) {
		t.Fatalf("after run: %v", task)
	}

	// failures are recorded
	e.ui.Tasks.GC = func(context.Context, string) (int, int64, error) { return 0, 0, errors.New("boom") }
	wantStatus(t, e.req(t, "POST", run, "", "admin", nil, true), http.StatusAccepted)
	e.ui.Tasks.Wait()
	if task = firstTask(t, e); task["lastResult"] != "Failed" {
		t.Fatalf("failed run: %v", task)
	}
	// the legacy alias records state too
	e.ui.Tasks.GC = func(context.Context, string) (int, int64, error) { return 1, 0, nil }
	wantStatus(t, e.req(t, "POST", "/-/api/gc", "", "admin", nil, true), http.StatusOK)
	if task = firstTask(t, e); task["lastResult"] != "OK" || task["lastRemoved"] != float64(1) {
		t.Fatalf("alias run: %v", task)
	}
}

func TestRepositoryDetailPolicyFields(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		for i := range c.Repositories {
			switch c.Repositories[i].Name {
			case "maven-releases":
				c.Repositories[i].VersionPolicy = "release"
				c.Repositories[i].WritePolicy = "allow_once"
			case "maven-proxy":
				c.Repositories[i].MetadataMaxAge = config.Duration(24 * time.Hour)
				c.Repositories[i].ContentMaxAge = config.Never
				c.Repositories[i].NegativeCacheTTL = config.Duration(10 * time.Minute)
			}
		}
	})
	m := e.get(t, "/-/api/repositories/maven-releases", "admin").json(t)
	if m["versionPolicy"] != "release" || m["writePolicy"] != "allow_once" {
		t.Fatalf("hosted admin: %v", m)
	}
	m = e.get(t, "/-/api/repositories/maven-proxy", "admin").json(t)
	if m["metadataMaxAge"] != "24h" || m["contentMaxAge"] != "-1" || m["negativeCacheTTL"] != "10m" {
		t.Fatalf("proxy admin: %v", m)
	}
	for _, name := range []string{"maven-releases", "maven-proxy"} {
		m = e.get(t, "/-/api/repositories/"+name, "ci").json(t)
		for _, k := range []string{"versionPolicy", "writePolicy", "metadataMaxAge", "contentMaxAge", "negativeCacheTTL"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s: non-admin sees %s", name, k)
			}
		}
	}
}

func TestTaskRunRequiresInteractive(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Tasks.GC = func(context.Context, string) (int, int64, error) { return 0, 0, nil }
	_, tok := e.newToken(t, "admin", "")
	for _, path := range []string{"/-/api/tasks/compact-blobstore/run", "/-/api/gc"} {
		r, _ := http.NewRequest("POST", e.ts.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set(auth.UIHeader, "1")
		res, err := noRedirect.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s with token: %d", path, res.StatusCode)
		}
		// a session cookie works (run now answers 202, the alias waits: 200)
		want := http.StatusOK
		if path != "/-/api/gc" {
			want = http.StatusAccepted
		}
		wantStatus(t, e.req(t, "POST", path, "", "", e.cookieFor("admin"), true), want)
		e.ui.Tasks.Wait()
	}
}

// Listing tasks must not disturb a run: runs started one after the other
// never see a 409 while listTasks hammers the endpoint.
func TestListTasksDoesNotCauseConflict(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Tasks.GC = func(context.Context, string) (int, int64, error) { return 0, 0, nil }
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					e.get(t, "/-/api/tasks", "admin")
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if r := e.req(t, "POST", "/-/api/tasks/compact-blobstore/run", "", "admin", nil, true); r.status != http.StatusAccepted {
			close(stop)
			wg.Wait()
			t.Fatalf("run %d: %d %s", i, r.status, r.body)
		}
		e.ui.Tasks.Wait()
	}
	close(stop)
	wg.Wait()
}

func TestTaskPanicRecordsFailed(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	e.ui.Tasks.Events = sink
	e.ui.Tasks.GC = func(context.Context, string) (int, int64, error) { panic("boom") }
	r, _ := http.NewRequest("POST", e.ts.URL+"/-/api/tasks/compact-blobstore/run", nil)
	r.SetBasicAuth("admin", "pw")
	r.Header.Set(auth.UIHeader, "1")
	if res, err := noRedirect.Do(r); err == nil {
		res.Body.Close()
	}
	e.ui.Tasks.Wait()
	task := firstTask(t, e)
	if task["lastResult"] != "Failed" || task["status"] != "Waiting" {
		t.Fatalf("after panic: %v", task)
	}
	if got := sink.Events(); len(got) != 1 || got[0].Kind != "task.run" || got[0].Detail != "Failed: panic: boom" || got[0].Actor != "admin" {
		t.Fatalf("events after panic: %v", got)
	}
	e.ui.Tasks.GC = func(context.Context, string) (int, int64, error) { return 0, 0, nil } // lock was released
	wantStatus(t, e.req(t, "POST", "/-/api/gc", "", "admin", nil, true), http.StatusOK)
}

func TestStatusChecksBlobStoreTimeout(t *testing.T) {
	old := blobCheckTimeout
	blobCheckTimeout = 50 * time.Millisecond
	t.Cleanup(func() { blobCheckTimeout = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e := newEnv(t, "http://repo.example")
	e.ui.blobChecker = func() (bool, string) { <-release; return true, "writable" }
	start := time.Now()
	r := e.get(t, "/-/api/status/checks", "admin")
	if time.Since(start) > 2*time.Second {
		t.Fatal("a blocked blob store must not block the checks")
	}
	m := checkByName(t, r, "Blob store")
	if m["healthy"] != false || !strings.Contains(m["message"].(string), "timed out") {
		t.Fatalf("blob store: %v", m)
	}
	if m := checkByName(t, r, "Database"); m["healthy"] != true {
		t.Fatalf("database: %v", m)
	}
}

func TestStatusChecksBlobStoreSingleInFlight(t *testing.T) {
	old := blobCheckTimeout
	blobCheckTimeout = 30 * time.Millisecond
	t.Cleanup(func() { blobCheckTimeout = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e := newEnv(t, "http://repo.example")
	var started atomic.Int32
	e.ui.blobChecker = func() (bool, string) { started.Add(1); <-release; return true, "writable" }
	for i := 0; i < 3; i++ {
		m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Blob store")
		msg := m["message"].(string)
		if m["healthy"] != false || (i > 0 && !strings.Contains(msg, "previous check still running")) {
			t.Fatalf("round %d: %v", i, m)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("a hung check started %d goroutines, want 1", n)
	}
}

// dbAuth replaces e's Authenticator with one over e.db holding the given
// users and roles (plus admin, password "pw").
func dbAuth(t *testing.T, e *env, users []meta.User, roles []meta.Role) {
	t.Helper()
	ctx := context.Background()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	roles = append(roles, meta.BuiltinAdminRole())
	users = append(users, meta.User{ID: "admin", Roles: []string{"admin"}})
	for _, r := range roles {
		if err := e.db.CreateRole(ctx, r); err != nil && !errors.Is(err, meta.ErrExists) {
			t.Fatal(err)
		}
	}
	for _, u := range users {
		u.PasswordHash = string(h)
		if err := e.db.CreateUser(ctx, u); err != nil && !errors.Is(err, meta.ErrExists) {
			t.Fatal(err)
		}
	}
	a, err := auth.Open(ctx, e.db, e.db)
	if err != nil {
		t.Fatal(err)
	}
	a.Sessions, a.AnonymousRead, a.RepoFormat = e.ses, e.ui.Auth.AnonymousRead, e.ui.Auth.RepoFormat
	e.ui.Auth, e.srv.Auth = a, a
}

func TestUsersShowPrivilegeTargets(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	dbAuth(t, e, []meta.User{{ID: "mixed", Roles: []string{"mixed"}}}, []meta.Role{{ID: "mixed", Privileges: []meta.Privilege{
		{Target: "repo:maven-releases", Actions: []string{meta.ActionAdd}},
		{Target: "format:npm", Actions: []string{meta.ActionAdd}},
		{Target: "*", Actions: []string{meta.ActionRead}},
	}}})
	r := e.get(t, "/-/api/users", "admin")
	wantStatus(t, r, http.StatusOK)
	var mixed map[string]any
	for _, x := range r.json(t)["users"].([]any) {
		if m := x.(map[string]any); m["userId"] == "mixed" {
			mixed = m
		}
	}
	if w := mixed["writeRepositories"].([]any); len(w) != 1 || w[0] != "maven-releases" {
		t.Fatalf("writeRepositories: %v", w)
	}
	privs := mixed["privileges"].([]any)
	if len(privs) != 3 || privs[1].(map[string]any)["target"] != "format:npm" || privs[2].(map[string]any)["target"] != "*" ||
		privs[0].(map[string]any)["role"] != "mixed" {
		t.Fatalf("privileges: %v", privs)
	}
}

func TestTokenForDisabledUserIsConflict(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	dbAuth(t, e, []meta.User{{ID: "off", Status: meta.StatusDisabled}}, nil)
	wantStatus(t, e.req(t, "POST", "/-/api/tokens", `{"user":"off"}`, "admin", nil, true), http.StatusConflict)
}
