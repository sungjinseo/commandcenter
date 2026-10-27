package ui

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/backup"
	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/tasks"
)

// withBackups gives the env a backup service over its own small data dir
// (one real asset) and wires it into the task service.
func withBackups(t *testing.T, e *env) *backup.Service {
	t.Helper()
	dir := t.TempDir()
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bs, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: db, Now: time.Now}
	if _, err := store.Put(context.Background(), "maven-releases", "a/1/a-1.jar", "", strings.NewReader("jar")); err != nil {
		t.Fatal(err)
	}
	svc := &backup.Service{DB: db, Store: store, Dir: filepath.Join(t.TempDir(), "backup"), Version: "test"}
	e.ui.Backups = svc
	e.ui.Tasks.Backup = svc
	return svc
}

// withToken sends a request with a token principal.
func (e *env) withToken(t *testing.T, method, path, body, token string) resp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, _ := http.NewRequest(method, e.ts.URL+path, rd)
	r.Header.Set("Authorization", "Bearer "+token)
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: string(b)}
}

// waitRun polls the ad-hoc run until it finished and returns its JSON.
func waitRun(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := e.get(t, "/-/api/backup/runs/"+id, "admin")
		wantStatus(t, r, http.StatusOK)
		m := r.json(t)
		if m["status"] != "Running" {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish: %s", id, r.body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackupAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	svc := withBackups(t, e)
	sink := &repotest.Sink{}
	e.ui.Events = sink
	_, tok, err := e.ui.Auth.CreateToken(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, ciTok, err := e.ui.Auth.CreateToken(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}

	// Start: admin only; tokens (the CLI) and passwords need no UI header,
	// sessions do.
	wantStatus(t, e.req(t, "POST", "/-/api/backup", "", "", nil, true), http.StatusUnauthorized)
	wantStatus(t, e.req(t, "POST", "/-/api/backup", "", "reader", nil, true), http.StatusForbidden)
	wantStatus(t, e.withToken(t, "POST", "/-/api/backup", "", ciTok), http.StatusForbidden)
	cookie := login(t, e, "admin")
	// Without the UI header the cookie does not authenticate a POST at all.
	wantStatus(t, e.req(t, "POST", "/-/api/backup", `{"subdir":"s1"}`, "", cookie, false), http.StatusUnauthorized)
	r := e.req(t, "POST", "/-/api/backup", `{"subdir":"s1"}`, "", cookie, true)
	wantStatus(t, r, http.StatusAccepted)
	id, _ := r.json(t)["id"].(string)
	if !strings.HasPrefix(id, "backup-") {
		t.Fatalf("start: %s", r.body)
	}
	if m := waitRun(t, e, id); m["status"] != "OK" || !strings.Contains(m["message"].(string), "s1") {
		t.Fatalf("run: %v", m)
	}
	r = e.withToken(t, "POST", "/-/api/backup", `{"subdir":"s2","keep":5}`, tok)
	wantStatus(t, r, http.StatusAccepted)
	id2, _ := r.json(t)["id"].(string)
	if m := waitRun(t, e, id2); m["status"] != "OK" {
		t.Fatalf("token run: %v", m)
	}
	// An empty body uses the defaults; bad params are 400.
	r = e.req(t, "POST", "/-/api/backup", "", "admin", nil, false)
	wantStatus(t, r, http.StatusAccepted)
	waitRun(t, e, r.json(t)["id"].(string))
	for _, body := range []string{`{"subdir":"../x"}`, `{"subdir":"blobs"}`, `{"keep":0.5}`, `{"keep":0}`, `{"keep":-3}`, `{"keep":101}`, `{"nope":1}`} {
		if r := e.admin(t, "POST", "/-/api/backup", body); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, r.status, r.body)
		}
	}
	wantStatus(t, e.get(t, "/-/api/backup/runs/backup-nope", "admin"), http.StatusNotFound)
	wantStatus(t, e.get(t, "/-/api/backup/runs/"+id, "reader"), http.StatusForbidden)

	// List: admin, tokens allowed.
	wantStatus(t, e.get(t, "/-/api/backups", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/backups", "reader"), http.StatusForbidden)
	r = e.withToken(t, "GET", "/-/api/backups", "", tok)
	wantStatus(t, r, http.StatusOK)
	list := r.json(t)
	if n := len(list["backups"].([]any)); n != 3 || list["dir"] != svc.Dir || list["running"] != "" {
		t.Fatalf("list: %s", r.body)
	}

	// Busy: a second start while one runs is 409.
	entered, release := make(chan struct{}), make(chan struct{})
	svc.AfterSnapshot = func() { close(entered); <-release }
	r = e.admin(t, "POST", "/-/api/backup", `{"subdir":"held"}`)
	wantStatus(t, r, http.StatusAccepted)
	<-entered
	wantStatus(t, e.admin(t, "POST", "/-/api/backup", ""), http.StatusConflict)
	wantStatus(t, e.admin(t, "POST", "/-/api/backups/s1/verify", ""), http.StatusConflict)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/backups/s1", ""), http.StatusConflict)
	heldID, _ := r.json(t)["id"].(string)
	// The page follows the run through its ad-hoc id.
	if m := e.get(t, "/-/api/backups", "admin").json(t); m["running"] != heldID || m["runningKind"] != "run" {
		t.Fatalf("running: %v", m)
	}
	close(release)
	waitRun(t, e, heldID)
	svc.AfterSnapshot = nil

	// Verify: interactive admin with the UI header.
	wantStatus(t, e.withToken(t, "POST", "/-/api/backups/s1/verify", "", tok), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/backups/s1/verify", "", "admin", nil, false), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/backups/s1/verify", "", "reader", nil, true), http.StatusForbidden)
	sink.Reset()
	r = e.admin(t, "POST", "/-/api/backups/s1/verify", "")
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["ok"] != true || m["deep"] != false || m["blobs"] != float64(1) {
		t.Fatalf("verify: %s", r.body)
	}
	if got := sink.Events(); len(got) != 1 || got[0].Kind != "backup.verify" || got[0].Subject != "s1" || got[0].Actor != "admin" {
		t.Fatalf("verify events: %s", sink)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/backups/nope/verify", ""), http.StatusNotFound)
	wantStatus(t, e.admin(t, "POST", "/-/api/backups/..%2Fs1/verify", ""), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "POST", "/-/api/backups/s1/verify?deep=maybe", ""), http.StatusBadRequest)

	// A deep verify is a task run: 202, then the run's outcome.
	svc.Events = sink
	sink.Reset()
	r = e.admin(t, "POST", "/-/api/backups/s1/verify?deep=true", "")
	wantStatus(t, r, http.StatusAccepted)
	if m := waitRun(t, e, r.json(t)["id"].(string)); m["status"] != "OK" || !strings.Contains(m["message"].(string), "verified (deep)") {
		t.Fatalf("deep verify run: %v", m)
	}
	if got := sink.Events(); len(got) != 1 || got[0].Kind != "backup.verify" || got[0].Actor != "admin" || !strings.Contains(got[0].Detail, "deep=true") {
		t.Fatalf("deep verify events: %s", sink)
	}
	wantStatus(t, e.admin(t, "POST", "/-/api/backups/nope/verify?deep=true", ""), http.StatusNotFound)
	wantStatus(t, e.withToken(t, "POST", "/-/api/backups/s1/verify?deep=true", "", tok), http.StatusForbidden)
	svc.Events = nil

	// Delete: interactive admin with the UI header.
	wantStatus(t, e.withToken(t, "DELETE", "/-/api/backups/s1", "", tok), http.StatusForbidden)
	wantStatus(t, e.req(t, "DELETE", "/-/api/backups/s1", "", "admin", nil, false), http.StatusForbidden)
	sink.Reset()
	r = e.admin(t, "DELETE", "/-/api/backups/s1", "")
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["name"] != "s1" || m["sweepSkipped"] != nil {
		t.Fatalf("delete: %s", r.body)
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/backups/s1", ""), http.StatusNotFound)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/backups/blobs", ""), http.StatusBadRequest)
	if got := sink.Events(); len(got) != 1 || got[0].Kind != "backup.delete" || got[0].Subject != "s1" {
		t.Fatalf("delete events: %s", sink)
	}
	if m := e.get(t, "/-/api/backups", "admin").json(t); len(m["backups"].([]any)) != 3 {
		t.Fatalf("list after delete: %v", m)
	}
}

func TestBackupAPIUnavailable(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.get(t, "/-/api/backups", "admin"), http.StatusNotImplemented)
	wantStatus(t, e.admin(t, "POST", "/-/api/backup", ""), http.StatusNotImplemented)
	_ = auth.UIHeader
}

// A token cannot change retention: its keep is ignored (the default, or the
// value an interactive admin stored) and never stored on the api-backup row.
func TestBackupAPITokenCannotPrune(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	svc := withBackups(t, e)
	_, tok, err := e.ui.Auth.CreateToken(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		list, err := svc.List()
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	for _, name := range []string{"a1", "a2", "a3"} {
		r := e.admin(t, "POST", "/-/api/backup", `{"subdir":"`+name+`"}`)
		wantStatus(t, r, http.StatusAccepted)
		waitRun(t, e, r.json(t)["id"].(string))
	}
	r := e.withToken(t, "POST", "/-/api/backup", `{"subdir":"t1","keep":1,"deep":true}`, tok)
	wantStatus(t, r, http.StatusAccepted)
	if m := waitRun(t, e, r.json(t)["id"].(string)); m["status"] != "OK" {
		t.Fatalf("token run: %v", m)
	}
	if n := count(); n != 4 {
		t.Fatalf("a token's keep 1 pruned backups: %d left", n)
	}
	row, err := e.ui.Tasks.Get(context.Background(), tasks.AdHocTaskID(tasks.TypeBackup))
	if err != nil || !strings.Contains(row.Params, `"keep":7`) || strings.Contains(row.Params, `"deep":true`) {
		t.Fatalf("row: %+v %v", row, err)
	}
	// An interactive admin may set it, and the token then runs with that value.
	r = e.admin(t, "POST", "/-/api/backup", `{"subdir":"a4","keep":2}`)
	wantStatus(t, r, http.StatusAccepted)
	waitRun(t, e, r.json(t)["id"].(string))
	if n := count(); n != 2 {
		t.Fatalf("admin keep 2: %d left", n)
	}
	r = e.withToken(t, "POST", "/-/api/backup", `{"subdir":"t2","keep":1}`, tok)
	wantStatus(t, r, http.StatusAccepted)
	waitRun(t, e, r.json(t)["id"].(string))
	if n := count(); n != 2 {
		t.Fatalf("token with the stored keep 2: %d left", n)
	}
	wantStatus(t, e.withToken(t, "POST", "/-/api/backup", `{"keep":0}`, tok), http.StatusBadRequest)
}

// The backup page follows a scheduled (or "run now") backup by its task id,
// kind "task", through /-/api/tasks/{id}; an ad-hoc run by its run id.
func TestBackupListFollowsScheduledRun(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	svc := withBackups(t, e)
	tk, err := e.ui.Tasks.Create(context.Background(), tasks.Def{Name: "Nightly", Type: tasks.TypeBackup, Schedule: "manual", Params: []byte(`{"subdir":"sched"}`)})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	svc.AfterSnapshot = func() { close(entered); <-release }
	if err := e.ui.Tasks.RunAsync(tk.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	<-entered
	m := e.get(t, "/-/api/backups", "admin").json(t)
	if m["running"] != tk.ID || m["runningKind"] != "task" {
		t.Fatalf("list while a scheduled run runs: %v", m)
	}
	if got := e.get(t, "/-/api/tasks/"+tk.ID, "admin").json(t); got["status"] != "Running" {
		t.Fatalf("task: %v", got)
	}
	close(release)
	e.ui.Tasks.Wait()
	m = e.get(t, "/-/api/backups", "admin").json(t)
	if m["running"] != "" || m["runningKind"] != "" {
		t.Fatalf("list after the run: %v", m)
	}
	if got := e.get(t, "/-/api/tasks/"+tk.ID, "admin").json(t); got["status"] == "Running" {
		t.Fatalf("task after the run: %v", got)
	}
}

// A directory with a meta.db and no manifest is listed as incomplete, and
// deleting it works.
func TestBackupListShowsIncomplete(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	svc := withBackups(t, e)
	for _, name := range []string{"ok1", "bad1"} {
		r := e.admin(t, "POST", "/-/api/backup", `{"subdir":"`+name+`"}`)
		wantStatus(t, r, http.StatusAccepted)
		waitRun(t, e, r.json(t)["id"].(string))
	}
	if err := os.Remove(filepath.Join(svc.Dir, "bad1", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	m := e.get(t, "/-/api/backups", "admin").json(t)
	var bad, good map[string]any
	for _, x := range m["backups"].([]any) {
		b := x.(map[string]any)
		switch b["name"] {
		case "bad1":
			bad = b
		case "ok1":
			good = b
		}
	}
	if bad == nil || bad["incomplete"] != true || good == nil || good["incomplete"] != nil {
		t.Fatalf("list: %v", m)
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/backups/bad1", ""), http.StatusOK)
	if m := e.get(t, "/-/api/backups", "admin").json(t); len(m["backups"].([]any)) != 1 {
		t.Fatalf("after delete: %v", m)
	}
}

// On PostgreSQL the backup page gets the reason instead of the backups, and
// every action answers 501 with it.
func TestBackupsOnPostgres(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	svc := withBackups(t, e)
	svc.Disabled = backup.ErrPostgres
	r := e.get(t, "/-/api/backups", "admin")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"unsupported":`) || !strings.Contains(r.body, "pg_dump") || !strings.Contains(r.body, `"backups":[]`) {
		t.Fatalf("list: %s", r.body)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/-/api/backup"}, {"POST", "/-/api/backups/x/verify"}, {"DELETE", "/-/api/backups/x"},
	} {
		r := e.req(t, c.method, c.path, "", "admin", nil, true)
		if r.status != http.StatusNotImplemented || !strings.Contains(r.body, "pg_dump") {
			t.Errorf("%s %s: %d %s", c.method, c.path, r.status, r.body)
		}
	}
}
