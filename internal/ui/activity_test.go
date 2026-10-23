package ui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/logring"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/tasks"
)

func TestEventSitesInUI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	e.ui.Tasks.Events = sink
	e.ui.Tasks.GC = func(context.Context, string) (int, error) { return 3, nil }

	last := func() repotest.Event {
		t.Helper()
		got := sink.Events()
		if len(got) == 0 {
			t.Fatal("no event emitted")
		}
		return got[len(got)-1]
	}
	cases := []struct {
		name string
		do   func() resp
		want repotest.Event
		det  string // substring of the detail
	}{
		{"repo.create", func() resp {
			return e.admin(t, "POST", "/-/api/repositories", `{"name":"mvn-ev","format":"maven","type":"hosted"}`)
		}, repotest.Event{Kind: "repo.create", Actor: "admin", Repo: "mvn-ev", Subject: "mvn-ev"}, "maven/hosted"},
		{"repo.update", func() resp {
			return e.admin(t, "PUT", "/-/api/repositories/mvn-ev", `{"name":"mvn-ev","format":"maven","type":"hosted","writePolicy":"allow"}`)
		}, repotest.Event{Kind: "repo.update", Actor: "admin", Repo: "mvn-ev", Subject: "mvn-ev"}, ""},
		{"repo.delete", func() resp { return e.admin(t, "DELETE", "/-/api/repositories/mvn-ev", "") },
			repotest.Event{Kind: "repo.delete", Actor: "admin", Repo: "mvn-ev", Subject: "mvn-ev"}, ""},
		{"task.run via gc", func() resp { return e.admin(t, "POST", "/-/api/gc", "") },
			repotest.Event{Kind: "task.run", Actor: "admin", Subject: tasks.SeedID}, "removed 3 blobs"},
		{"task.run via task", func() resp {
			r := e.admin(t, "POST", "/-/api/tasks/"+tasks.SeedID+"/run", "")
			e.ui.Tasks.Wait() // the run is asynchronous
			return r
		},
			repotest.Event{Kind: "task.run", Actor: "admin", Subject: tasks.SeedID}, "removed 3 blobs"},
	}
	for _, c := range cases {
		before := len(sink.Events())
		if r := c.do(); r.status >= 300 {
			t.Fatalf("%s: status %d: %s", c.name, r.status, r.body)
		}
		if got := len(sink.Events()); got != before+1 {
			t.Fatalf("%s: %d events emitted, want 1 (%s)", c.name, got-before, sink)
		}
		got := last()
		det := got.Detail
		got.Detail = ""
		if got != c.want || !strings.Contains(det, c.det) {
			t.Fatalf("%s: got %+v detail %q, want %+v detail ~%q", c.name, got, det, c.want, c.det)
		}
	}

	// A failed task run is recorded too.
	e.ui.Tasks.GC = func(context.Context, string) (int, error) { return 0, errors.New("disk on fire") }
	e.admin(t, "POST", "/-/api/gc", "")
	if got := last(); got.Kind != "task.run" || got.Detail != "Failed: disk on fire (removed 0 blobs)" {
		t.Fatalf("failed run: %+v", got)
	}

	// Tokens: created for another user (the subject) by the admin, then revoked.
	sink.Reset()
	id, tok := e.newToken(t, "admin", `{"user":"ci"}`)
	if got := last(); got != (repotest.Event{Kind: "token.create", Actor: "admin", Subject: "ci", Detail: "id " + id}) {
		t.Fatalf("token.create: %+v", got)
	}
	wantStatus(t, e.admin(t, "DELETE", "/-/api/tokens/"+id, ""), http.StatusNoContent)
	if got := last(); got != (repotest.Event{Kind: "token.revoke", Actor: "admin", Subject: "ci", Detail: "id " + id}) {
		t.Fatalf("token.revoke: %+v", got)
	}
	for _, ev := range sink.Events() {
		if strings.Contains(ev.Detail+ev.Subject, tok) {
			t.Fatalf("token secret in event: %+v", ev)
		}
	}

	// Rejected requests emit nothing.
	sink.Reset()
	e.admin(t, "POST", "/-/api/repositories", `{"name":"bad name","format":"maven","type":"hosted"}`)
	e.admin(t, "DELETE", "/-/api/repositories/no-such", "")
	e.admin(t, "DELETE", "/-/api/tokens/nope", "")
	e.req(t, "POST", "/-/api/gc", "", "ci", nil, true)
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("events after rejected requests: %s", sink)
	}
}

func TestEventsAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	ctx := context.Background()
	var evs []meta.Event
	for i := 0; i < 7; i++ {
		k := "upload"
		if i%3 == 0 {
			k = "delete"
		}
		evs = append(evs, meta.Event{At: t0.Add(time.Duration(i) * time.Minute), Kind: k, Actor: "ci", Repo: "maven-releases",
			Subject: "s" + strconv.Itoa(i), Detail: "d"})
	}
	if err := e.db.InsertEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}

	r := e.get(t, "/-/api/events?limit=3", "admin")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	m := r.json(t)
	page := m["events"].([]any)
	first := page[0].(map[string]any)
	if len(page) != 3 || first["subject"] != "s6" || first["kind"] != "delete" || first["actor"] != "ci" ||
		first["repo"] != "maven-releases" || first["detail"] != "d" || first["at"] != "2026-10-01T12:06:00Z" || first["id"] == nil {
		t.Fatalf("page 1: %s", r.body)
	}
	next := m["next"].(float64)
	if next != page[2].(map[string]any)["id"] {
		t.Fatalf("next %v is not the last id: %s", next, r.body)
	}
	r = e.get(t, "/-/api/events?limit=3&before="+strconv.Itoa(int(next)), "admin")
	m = r.json(t)
	page = m["events"].([]any)
	if len(page) != 3 || page[0].(map[string]any)["subject"] != "s3" || m["next"] == nil {
		t.Fatalf("page 2: %s", r.body)
	}
	r = e.get(t, "/-/api/events?limit=3&before="+strconv.Itoa(int(m["next"].(float64))), "admin")
	m = r.json(t)
	if page = m["events"].([]any); len(page) != 1 || page[0].(map[string]any)["subject"] != "s0" || m["next"] != nil {
		t.Fatalf("last page: %s", r.body)
	}

	m = e.get(t, "/-/api/events?kind=delete", "admin").json(t)
	if page = m["events"].([]any); len(page) != 3 {
		t.Fatalf("kind filter: %v", page)
	}
	for _, p := range page {
		if p.(map[string]any)["kind"] != "delete" {
			t.Fatalf("kind filter: %v", page)
		}
	}
	// An empty result is [] and not null.
	if m = e.get(t, "/-/api/events?kind=none", "admin").json(t); m["events"] == nil || len(m["events"].([]any)) != 0 {
		t.Fatalf("empty: %v", m)
	}
	for _, bad := range []string{"limit=0", "limit=x", "before=-1", "before=x"} {
		wantStatus(t, e.get(t, "/-/api/events?"+bad, "admin"), http.StatusBadRequest)
	}
}

func TestEventsCapIs200(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	evs := make([]meta.Event, 250)
	for i := range evs {
		evs[i] = meta.Event{At: t0, Kind: "upload"}
	}
	if err := e.db.InsertEvents(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	m := e.get(t, "/-/api/events?limit=1000", "admin").json(t)
	if len(m["events"].([]any)) != eventsLimit || m["next"] == nil {
		t.Fatalf("got %d events, next %v", len(m["events"].([]any)), m["next"])
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func fillLogs(e *env) {
	ring := logring.NewRing(100, 1<<20)
	log := slog.New(logring.NewHandler(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelDebug}), ring))
	log.Debug("dbg line")
	log.Info("first info", "path", "/repository/x")
	log.Warn("a warning", "user", "Alice")
	log.Error("last error", "err", "boom")
	e.ui.Logs = ring
}

func TestLogsAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	fillLogs(e)
	msgs := func(path string) string {
		t.Helper()
		r := e.get(t, path, "admin")
		wantStatus(t, r, http.StatusOK)
		checkAPIHeaders(t, r)
		var out []string
		for _, l := range r.json(t)["logs"].([]any) {
			out = append(out, l.(map[string]any)["message"].(string))
		}
		return strings.Join(out, "|")
	}
	if got := msgs("/-/api/logs"); got != "last error|a warning|first info|dbg line" { // newest first
		t.Fatalf("all: %s", got)
	}
	if got := msgs("/-/api/logs?level=INFO"); got != "last error|a warning|first info" {
		t.Fatalf("INFO: %s", got)
	}
	if got := msgs("/-/api/logs?level=warn"); got != "last error|a warning" {
		t.Fatalf("WARN: %s", got)
	}
	if got := msgs("/-/api/logs?level=ERROR"); got != "last error" {
		t.Fatalf("ERROR: %s", got)
	}
	if got := msgs("/-/api/logs?q=" + url.QueryEscape("ALICE")); got != "a warning" {
		t.Fatalf("q: %s", got)
	}
	if got := msgs("/-/api/logs?limit=2"); got != "last error|a warning" {
		t.Fatalf("limit: %s", got)
	}
	row := e.get(t, "/-/api/logs?level=ERROR", "admin").json(t)["logs"].([]any)[0].(map[string]any)
	if row["level"] != "ERROR" || row["attrs"] != "err=boom" || row["time"] == "" {
		t.Fatalf("row: %v", row)
	}
	for _, bad := range []string{"level=LOUD", "limit=0", "limit=x"} {
		wantStatus(t, e.get(t, "/-/api/logs?"+bad, "admin"), http.StatusBadRequest)
	}
	// No ring configured: an empty list, not an error.
	e.ui.Logs = nil
	if m := e.get(t, "/-/api/logs", "admin").json(t); m["logs"] == nil || len(m["logs"].([]any)) != 0 {
		t.Fatalf("no ring: %v", m)
	}
}

func TestLogsDownload(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	fillLogs(e)
	r := e.get(t, "/-/api/logs/download", "admin")
	wantStatus(t, r, http.StatusOK)
	if got := r.header.Get("Content-Disposition"); got != `attachment; filename="commandcenter.log"` {
		t.Fatalf("Content-Disposition %q", got)
	}
	if got := r.header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type %q", got)
	}
	checkAPIHeaders(t, r)
	lines := strings.Split(strings.TrimSpace(r.body), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], "DEBUG dbg line") || !strings.Contains(lines[3], "ERROR last error err=boom") {
		t.Fatalf("body (oldest first, newest last):\n%s", r.body)
	}
	if !strings.Contains(lines[1], "INFO first info path=/repository/x") {
		t.Fatalf("line 2: %s", lines[1])
	}
	r = e.get(t, "/-/api/logs/download?level=WARN", "admin")
	if n := len(strings.Split(strings.TrimSpace(r.body), "\n")); n != 2 {
		t.Fatalf("level filter: %d lines", n)
	}
	wantStatus(t, e.get(t, "/-/api/logs/download?level=LOUD", "admin"), http.StatusBadRequest)
}

func TestActivityEndpointsAdminOnly(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	fillLogs(e)
	for _, p := range []string{"/-/api/events", "/-/api/logs", "/-/api/logs/download"} {
		wantStatus(t, e.get(t, p, ""), http.StatusUnauthorized)
		wantStatus(t, e.get(t, p, "ci"), http.StatusForbidden)
		wantStatus(t, e.get(t, p, "reader"), http.StatusForbidden)
		wantStatus(t, e.get(t, p, "admin"), http.StatusOK)
	}
}
