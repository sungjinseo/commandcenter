package ui

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/webhook"
)

const webhooksPath = "/-/api/webhooks"

// withWebhooks starts a dispatcher on the environment's database.
func withWebhooks(t *testing.T, e *env) *webhook.Dispatcher {
	t.Helper()
	d := &webhook.Dispatcher{DB: e.db, Repos: e.ui.Repos, Log: slog.New(slog.DiscardHandler)}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	d.Deliver()
	t.Cleanup(d.Close)
	e.ui.Webhooks = d
	return d
}

func TestWebhookCRUDAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withWebhooks(t, e)
	sink := &repotest.Sink{}
	e.ui.Events = sink
	if err := e.db.SetSecretKey(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}

	r := e.admin(t, "POST", webhooksPath, `{"name":"ci","url":"https://ci.example/hook","secret":"top-secret","events":["component.created","*"],"repo":"maven-releases"}`)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	id, _ := m["id"].(string)
	if id == "" || m["name"] != "ci" || m["url"] != "https://ci.example/hook" || m["secretSet"] != true || m["repo"] != "maven-releases" ||
		m["enabled"] != true || len(m["events"].([]any)) != 2 || m["created"] == "" {
		t.Fatalf("create: %s", r.body)
	}
	if _, has := m["secret"]; has || strings.Contains(r.body, "top-secret") {
		t.Fatalf("the secret came back: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", webhooksPath, `{"name":"global","url":"http://127.0.0.1:1/x","events":["ping"],"enabled":false}`), http.StatusCreated)

	r = e.get(t, webhooksPath, "admin")
	wantStatus(t, r, http.StatusOK)
	list := r.json(t)["webhooks"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["name"] != "ci" || list[1].(map[string]any)["enabled"] != false || list[1].(map[string]any)["secretSet"] != false {
		t.Fatalf("list: %s", r.body)
	}
	if strings.Contains(r.body, "top-secret") || strings.Contains(e.get(t, webhooksPath+"/"+id, "admin").body, "top-secret") {
		t.Fatal("the secret is readable")
	}

	// An update without a secret keeps it; "" clears it.
	r = e.admin(t, "PUT", webhooksPath+"/"+id, `{"name":"ci2","url":"https://ci.example/hook2","events":["ping"],"repo":"","enabled":false}`)
	wantStatus(t, r, http.StatusOK)
	if m = r.json(t); m["name"] != "ci2" || m["secretSet"] != true || m["repo"] != "" || m["enabled"] != false || m["url"] != "https://ci.example/hook2" {
		t.Fatalf("update: %s", r.body)
	}
	r = e.admin(t, "PUT", webhooksPath+"/"+id, `{"name":"ci2","url":"https://ci.example/hook2","events":["ping"],"secret":""}`)
	wantStatus(t, r, http.StatusOK)
	if m = r.json(t); m["secretSet"] != false || m["enabled"] != true {
		t.Fatalf("clear secret: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", webhooksPath+"/missing", `{"name":"x","url":"https://x","events":["*"]}`), http.StatusNotFound)
	wantStatus(t, e.get(t, webhooksPath+"/missing", "admin"), http.StatusNotFound)

	wantStatus(t, e.admin(t, "DELETE", webhooksPath+"/"+id, ""), http.StatusNoContent)
	wantStatus(t, e.admin(t, "DELETE", webhooksPath+"/"+id, ""), http.StatusNotFound)
	wantStatus(t, e.get(t, webhooksPath+"/"+id, "admin"), http.StatusNotFound)

	var kinds []string
	for _, ev := range sink.Events() {
		if ev.Actor != "admin" || strings.Contains(ev.Detail, "top-secret") {
			t.Fatalf("event: %+v", ev)
		}
		kinds = append(kinds, ev.Kind+" "+ev.Repo+" "+ev.Subject)
	}
	if got := strings.Join(kinds, ","); got != "webhook.create maven-releases ci,webhook.create  global,webhook.update  ci2,webhook.update  ci2,webhook.delete  ci2" {
		t.Fatalf("events: %s", got)
	}
}

func TestWebhookValidationAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withWebhooks(t, e)
	for _, c := range []struct{ body, want string }{
		{`{"url":"https://x","events":["*"]}`, "name must be"},
		{`{"name":"` + strings.Repeat("n", 65) + `","url":"https://x","events":["*"]}`, "name must be"},
		{`{"name":"n","url":"/relative","events":["*"]}`, "url must be"},
		{`{"name":"n","url":"ftp://x/y","events":["*"]}`, "url must be"},
		{`{"name":"n","url":"https://","events":["*"]}`, "url must be"},
		{`{"name":"n","url":"https://x"}`, "events must"},
		{`{"name":"n","url":"https://x","events":[]}`, "events must"},
		{`{"name":"n","url":"https://x","events":["upload"]}`, "unknown event kind"},
		{`{"name":"n","url":"https://x","events":["*"],"repo":"ghost"}`, "does not exist"},
		{`{"name":"n","url":"https://x","events":["*"],"unknown":1}`, "invalid JSON"},
	} {
		r := e.admin(t, "POST", webhooksPath, c.body)
		if r.status != http.StatusBadRequest || !strings.Contains(r.body, c.want) {
			t.Errorf("%s: %d %s, want %q", c.body, r.status, r.body, c.want)
		}
	}
	if got := e.get(t, webhooksPath, "admin").json(t)["webhooks"].([]any); len(got) != 0 {
		t.Fatalf("stored: %v", got)
	}
}

func TestWebhookEventsListAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, webhooksPath+"/events", "admin")
	wantStatus(t, r, http.StatusOK)
	list := r.json(t)["events"].([]any)
	if len(list) != len(webhook.Kinds) || list[0].(map[string]any)["kind"] != "component.created" || list[0].(map[string]any)["description"] == "" {
		t.Fatalf("events: %s", r.body)
	}
	wantStatus(t, e.get(t, webhooksPath+"/events", "ci"), http.StatusForbidden)
}

func TestWebhookPrivilegesAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withWebhooks(t, e)
	r := e.admin(t, "POST", webhooksPath, `{"name":"h","url":"https://x","events":["*"]}`)
	wantStatus(t, r, http.StatusCreated)
	id := r.json(t)["id"].(string)
	cases := []struct{ method, path string }{
		{"GET", webhooksPath}, {"POST", webhooksPath}, {"GET", webhooksPath + "/" + id}, {"PUT", webhooksPath + "/" + id},
		{"DELETE", webhooksPath + "/" + id}, {"POST", webhooksPath + "/" + id + "/test"}, {"GET", webhooksPath + "/" + id + "/deliveries"},
		{"POST", webhooksPath + "/" + id + "/deliveries/x/redeliver"},
	}
	for _, c := range cases {
		wantStatus(t, e.req(t, c.method, c.path, "", "", nil, true), http.StatusUnauthorized)
		wantStatus(t, e.req(t, c.method, c.path, "", "ci", nil, true), http.StatusForbidden)
	}
	// Mutations need the UI header and an interactive login (not a token).
	_, tok := e.newToken(t, "admin", "")
	for _, c := range cases {
		if c.method == "GET" {
			continue
		}
		wantStatus(t, e.req(t, c.method, c.path, "", "admin", nil, false), http.StatusForbidden)
		req, _ := http.NewRequest(c.method, e.ts.URL+c.path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Command-Center-UI", "1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s with a token: %d", c.method, c.path, res.StatusCode)
		}
	}
	if got := e.get(t, webhooksPath, "admin").json(t)["webhooks"].([]any); len(got) != 1 {
		t.Fatalf("the webhook was changed: %v", got)
	}
	// Without a dispatcher the API answers 501.
	e.ui.Webhooks = nil
	wantStatus(t, e.get(t, webhooksPath, "admin"), http.StatusNotImplemented)
}

func TestWebhookTestAndDeliveriesAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withWebhooks(t, e)
	var mu sync.Mutex
	var got []http.Header
	var fail atomic.Bool
	rc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Clone())
		mu.Unlock()
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	received := func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), got...)
	}
	defer rc.Close()
	r := e.admin(t, "POST", webhooksPath, `{"name":"h","url":"`+rc.URL+`/hook","events":["*"]}`)
	wantStatus(t, r, http.StatusCreated)
	id := r.json(t)["id"].(string)

	r = e.admin(t, "POST", webhooksPath+"/"+id+"/test", "")
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["ok"] != true || m["status"] != float64(200) || m["error"] != "" {
		t.Fatalf("test: %s", r.body)
	}
	if hs := received(); len(hs) != 1 || hs[0].Get("X-Command-Center-Event") != "ping" {
		t.Fatalf("receiver: %v", hs)
	}
	fail.Store(true)
	r = e.admin(t, "POST", webhooksPath+"/"+id+"/test", "")
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["ok"] != false || m["status"] != float64(502) || m["error"] != "HTTP 502" {
		t.Fatalf("failed test: %s", r.body)
	}
	wantStatus(t, e.admin(t, "POST", webhooksPath+"/missing/test", ""), http.StatusNotFound)

	r = e.get(t, webhooksPath+"/"+id+"/deliveries", "admin")
	wantStatus(t, r, http.StatusOK)
	list := r.json(t)["deliveries"].([]any)
	if len(list) != 2 {
		t.Fatalf("deliveries: %s", r.body)
	}
	byStatus := map[string]map[string]any{} // two in the same millisecond have no fixed order
	for _, d := range list {
		byStatus[d.(map[string]any)["status"].(string)] = d.(map[string]any)
	}
	failed, ok := byStatus["failed"]
	if !ok || byStatus["ok"] == nil || failed["attempts"] != float64(1) || failed["responseCode"] != float64(502) || failed["lastError"] != "HTTP 502" ||
		failed["event"] != "ping" || failed["created"] == "" || failed["nextAt"] != "" {
		t.Fatalf("deliveries: %s", r.body)
	}
	did := failed["id"].(string)
	fail.Store(false)
	r = e.admin(t, "POST", webhooksPath+"/"+id+"/deliveries/"+did+"/redeliver", "")
	wantStatus(t, r, http.StatusAccepted)
	if m := r.json(t); m["status"] != "pending" || m["attempts"] != float64(0) || m["id"] != did {
		t.Fatalf("redeliver: %s", r.body)
	}
	var redelivered map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && redelivered == nil; time.Sleep(10 * time.Millisecond) {
		for _, d := range e.get(t, webhooksPath+"/"+id+"/deliveries", "admin").json(t)["deliveries"].([]any) {
			if d := d.(map[string]any); d["id"] == did && d["status"] == "ok" {
				redelivered = d
			}
		}
	}
	if redelivered == nil || redelivered["attempts"] != float64(1) || redelivered["responseCode"] != float64(200) {
		t.Fatalf("redelivered: %v", redelivered)
	}
	if n := len(received()); n != 3 {
		t.Fatalf("receiver got %d requests, want 3", n)
	}
	wantStatus(t, e.admin(t, "POST", webhooksPath+"/"+id+"/deliveries/nope/redeliver", ""), http.StatusNotFound)
	// A delivery of another webhook is not reachable through this one.
	r = e.admin(t, "POST", webhooksPath, `{"name":"other","url":"`+rc.URL+`","events":["*"]}`)
	wantStatus(t, r, http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", webhooksPath+"/"+r.json(t)["id"].(string)+"/deliveries/"+did+"/redeliver", ""), http.StatusNotFound)
	wantStatus(t, e.get(t, webhooksPath+"/missing/deliveries", "admin"), http.StatusNotFound)
}
