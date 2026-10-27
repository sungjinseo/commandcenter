package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/tasks"
)

func TestTaskCRUD(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink

	// create
	r := e.admin(t, "POST", "/-/api/tasks", `{"name":"Rebuild releases","type":"repository.rebuild-metadata","schedule":"weekly:sun:04:00","params":{"repo":"maven-releases"}}`)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	id, _ := m["id"].(string)
	if id == "" || m["status"] != "Waiting" || m["schedule"] != "weekly:sun:04:00" || m["enabled"] != true ||
		m["nextRun"] == "" || m["typeName"] != "Rebuild Maven metadata" || m["params"].(map[string]any)["repo"] != "maven-releases" {
		t.Fatalf("created: %s", r.body)
	}
	if got := sink.Events(); len(got) != 1 || got[0].Kind != "task.create" || got[0].Actor != "admin" || got[0].Subject != id {
		t.Fatalf("create event: %s", sink)
	}

	// list (no logs) and get (with the log)
	r = e.get(t, "/-/api/tasks", "admin")
	wantStatus(t, r, http.StatusOK)
	list := r.json(t)
	if n := len(list["tasks"].([]any)); n != 2 || strings.Contains(r.body, "lastLog") || len(list["types"].([]any)) != 3 {
		t.Fatalf("list: %s", r.body)
	}
	r = e.get(t, "/-/api/tasks/"+id, "admin")
	wantStatus(t, r, http.StatusOK)
	if _, ok := r.json(t)["lastLog"]; !ok {
		t.Fatalf("get: %s", r.body)
	}
	wantStatus(t, e.get(t, "/-/api/tasks/nope", "admin"), http.StatusNotFound)

	// validation
	for _, body := range []string{
		`{"name":"x","type":"repository.rebuild-metadata","schedule":"daily:03:00","params":{"repo":"maven-public"}}`,
		`{"name":"x","type":"repository.cleanup","schedule":"daily:03:00","params":{"repos":["nope"]}}`,
		`{"name":"x","type":"blobstore.compact","schedule":"every day"}`,
		`{"name":"","type":"blobstore.compact","schedule":"manual"}`,
		`{"name":"x","type":"rm -rf","schedule":"manual"}`,
		`{"name":"x","type":"blobstore.compact","schedule":"manual","extra":1}`,
	} {
		r := e.admin(t, "POST", "/-/api/tasks", body)
		if r.status != http.StatusBadRequest || r.json(t)["error"] == "" {
			t.Errorf("%s: %d %s", body, r.status, r.body)
		}
	}

	// update: disable it
	sink.Reset()
	r = e.admin(t, "PUT", "/-/api/tasks/"+id, `{"name":"Rebuild","schedule":"daily:05:00","params":{"repo":"maven-releases"},"enabled":false}`)
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["status"] != "Disabled" || m["nextRun"] != "" || m["name"] != "Rebuild" || m["type"] != tasks.TypeRebuild {
		t.Fatalf("updated: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/tasks/nope", `{"name":"x","schedule":"manual"}`), http.StatusNotFound)
	wantStatus(t, e.admin(t, "PUT", "/-/api/tasks/"+id, `{"name":"x","type":"blobstore.compact","schedule":"manual"}`), http.StatusBadRequest)
	if got := sink.Events(); len(got) != 1 || got[0].Kind != "task.update" || !strings.Contains(got[0].Detail, "disabled") {
		t.Fatalf("update events: %s", sink)
	}

	// delete
	sink.Reset()
	wantStatus(t, e.admin(t, "DELETE", "/-/api/tasks/"+id, ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/tasks/"+id, ""), http.StatusNotFound)
	if got := sink.Events(); len(got) != 1 || got[0].Kind != "task.delete" || got[0].Subject != id || !strings.Contains(got[0].Detail, "Rebuild") {
		t.Fatalf("delete events: %s", sink)
	}
}

func TestTaskAPIAuth(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Tasks.GC = func(context.Context, string) (int, int64, error) { t.Error("gc ran"); return 0, 0, nil }
	body := `{"name":"x","type":"blobstore.compact","schedule":"manual"}`
	id := tasks.SeedID
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/-/api/tasks", ""}, {"GET", "/-/api/tasks/" + id, ""},
		{"POST", "/-/api/tasks", body}, {"PUT", "/-/api/tasks/" + id, body}, {"DELETE", "/-/api/tasks/" + id, ""},
		{"POST", "/-/api/tasks/" + id + "/run", ""}, {"POST", "/-/api/tasks/" + id + "/stop", ""}, {"POST", "/-/api/gc", ""},
	} {
		wantStatus(t, e.req(t, c.method, c.path, c.body, "", nil, true), http.StatusUnauthorized)
		wantStatus(t, e.req(t, c.method, c.path, c.body, "ci", nil, true), http.StatusForbidden)
		if c.method != "GET" { // mutations need the UI header
			wantStatus(t, e.req(t, c.method, c.path, c.body, "admin", nil, false), http.StatusForbidden)
		}
	}
	if list, _ := e.ui.Tasks.List(context.Background()); len(list) != 1 || list[0].Name != "Compact blob store" {
		t.Fatalf("tasks changed: %+v", list)
	}
}

func TestTasksUnavailable(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Tasks = nil
	wantStatus(t, e.get(t, "/-/api/tasks", "admin"), http.StatusNotImplemented)
	wantStatus(t, e.admin(t, "POST", "/-/api/gc", ""), http.StatusNotImplemented)
}

// A run's details are kept: the run log and message are served by GET.
func TestTaskRunLogServed(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Tasks.GC = func(context.Context, string) (int, int64, error) { return 2, 20, nil }
	r := e.admin(t, "POST", "/-/api/tasks/"+tasks.SeedID+"/run", "")
	wantStatus(t, r, http.StatusAccepted)
	e.ui.Tasks.Wait()
	m := e.get(t, "/-/api/tasks/"+tasks.SeedID, "admin").json(t)
	if !strings.Contains(m["lastLog"].(string), "removed 2 blobs (20 bytes)") || m["lastMessage"] != "removed 2 blobs (20 bytes)" ||
		m["lastRemoved"] != float64(2) || m["lastBytes"] != float64(20) {
		t.Fatalf("task: %v", m)
	}
}

// POST /-/api/tasks/{id}/stop cancels the running task (202) and answers
// 409 when it is not running; a run refused while another task runs names it.
func TestTaskStopAndConflict(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	entered := make(chan struct{})
	e.ui.Tasks.GC = func(ctx context.Context, _ string) (int, int64, error) {
		close(entered)
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	stop := "/-/api/tasks/" + tasks.SeedID + "/stop"
	wantStatus(t, e.admin(t, "POST", stop, ""), http.StatusConflict)
	wantStatus(t, e.admin(t, "POST", "/-/api/tasks/nope/stop", ""), http.StatusNotFound)

	wantStatus(t, e.admin(t, "POST", "/-/api/tasks/"+tasks.SeedID+"/run", ""), http.StatusAccepted)
	<-entered
	other := e.admin(t, "POST", "/-/api/tasks", `{"name":"Other","type":"blobstore.compact","schedule":"manual"}`).json(t)["id"].(string)
	r := e.admin(t, "POST", "/-/api/tasks/"+other+"/run", "")
	wantStatus(t, r, http.StatusConflict)
	if m := r.json(t); m["running"] != tasks.SeedID || m["runningName"] != "Compact blob store" {
		t.Fatalf("409 body: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", stop, ""), http.StatusAccepted)
	e.ui.Tasks.Wait()
	if m := e.get(t, "/-/api/tasks/"+tasks.SeedID, "admin").json(t); m["lastResult"] != tasks.ResultStopped {
		t.Fatalf("after stop: %v", m)
	}
}

// QA L-3: the built-in compaction task cannot be deleted through the API.
func TestSeedTaskDeleteIsConflict(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.admin(t, "DELETE", "/-/api/tasks/compact-blobstore", "")
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(r.body, "cannot be deleted") {
		t.Fatalf("body: %s", r.body)
	}
	wantStatus(t, e.get(t, "/-/api/tasks/compact-blobstore", "admin"), http.StatusOK)
}

// QA L-4: POST /-/api/gc while another task runs names the busy state with
// the generic message, not "gc already running".
func TestGCWhileTaskRunsIsConflict(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	entered := make(chan struct{})
	release := make(chan struct{})
	e.ui.Tasks.GC = func(ctx context.Context, _ string) (int, int64, error) { close(entered); <-release; return 0, 0, nil }
	defer func() { close(release); e.ui.Tasks.Wait() }()
	wantStatus(t, e.admin(t, "POST", "/-/api/tasks/"+tasks.SeedID+"/run", ""), http.StatusAccepted)
	<-entered
	r := e.admin(t, "POST", "/-/api/gc", "")
	wantStatus(t, r, http.StatusConflict)
	if m := r.json(t); m["error"] != "a task is already running" || m["running"] != tasks.SeedID {
		t.Fatalf("409 body: %s", r.body)
	}
}
