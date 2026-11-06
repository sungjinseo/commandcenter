package ui

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/events"
	"commandcenter/internal/repo/repotest"
)

// A store that opened and then stopped working is reported by the checks and
// its unavailable field, after the probe's answer expired; it recovers the
// same way.
func TestStoreProbeFindsAStoreThatStoppedWorking(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	now := time.Now()
	e.ui.now = func() time.Time { return now }
	root := filepath.Join(t.TempDir(), "disk")
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"disk","path":"`+root+`"}`), http.StatusCreated)
	if m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Blob Stores Ready"); m["healthy"] != true {
		t.Fatalf("ready: %v", m)
	}
	tmp := filepath.Join(root, "tmp")
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, nil, 0o644); err != nil { // a lost mount, whoever runs this
		t.Fatal(err)
	}
	if m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Blob Stores Ready"); m["healthy"] != true {
		t.Fatalf("the answer is cached for %s: %v", storeProbeTTL, m)
	}
	now = now.Add(storeProbeTTL)
	m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Blob Stores Ready")
	if m["healthy"] != false || !strings.Contains(m["message"].(string), `"disk"`) || !strings.Contains(m["message"].(string), "1 of 2") {
		t.Fatalf("ready: %v", m)
	}
	if v := e.get(t, "/-/api/blobstores/disk", "admin").json(t); !strings.Contains(v["unavailable"].(string), "directory") {
		t.Fatalf("view: %v", v)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	now = now.Add(storeProbeTTL)
	if m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Blob Stores Ready"); m["healthy"] != true {
		t.Fatalf("recovered: %v", m)
	}
	if v := e.get(t, "/-/api/blobstores/disk", "admin").json(t); v["unavailable"] != nil {
		t.Fatalf("view: %v", v)
	}
}

// A store that does not answer does not hold the checks up, is reported, and
// gets no second probe on top of the first.
func TestStoreProbeThatHangs(t *testing.T) {
	old := blobCheckTimeout
	blobCheckTimeout = 30 * time.Millisecond
	t.Cleanup(func() { blobCheckTimeout = old })
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	release := make(chan struct{})
	var requests atomic.Int32
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-release
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(release) })
	bs, err := blob.OpenS3(t.TempDir(), blob.S3Config{Bucket: "test", Endpoint: hung.URL, PathStyle: true, AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	e.ui.Store.AddStore("hung", bs)
	for i := 0; i < 3; i++ {
		start := time.Now()
		bad := e.ui.storeProblems()
		if !strings.Contains(bad["hung"], "no answer") || len(bad) != 1 {
			t.Fatalf("round %d: %v", i, bad)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("a hung store held the check up")
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d probes of a hung store, want 1", n)
	}
}

// Without a client asking, the leader's quota check logs a store over its
// quota and records one event per flip; the aggregate check says the same.
func TestCheckQuotasWithoutAClient(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	var log bytes.Buffer
	e.ui.Log = slog.New(slog.NewTextHandler(&log, nil))
	sink := &repotest.Sink{}
	e.ui.Events = sink
	root := filepath.Join(t.TempDir(), "disk")
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"disk","path":"`+root+`","quota":{"type":"spaceRemainingBytes","limitBytes":9000000000000000000}}`), http.StatusCreated)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		e.ui.CheckQuotas(ctx)
	}
	if n := strings.Count(log.String(), "over its soft quota"); n != 2 {
		t.Fatalf("%d warnings:\n%s", n, log.String())
	}
	if evs := eventsOf(sink, "blobstore.quota"); len(evs) != 1 || evs[0].Subject != "disk" || evs[0].Actor != events.System {
		t.Fatalf("events: %+v", evs)
	}
	m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Blob Stores Quota")
	if m["healthy"] != false || !strings.Contains(m["message"].(string), "disk:") {
		t.Fatalf("aggregate: %v", m)
	}
	if m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Blob store quota: disk"); m["healthy"] != false {
		t.Fatalf("per store: %v", m)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/blobstores/disk", `{"quota":null}`), http.StatusOK)
	if m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Blob Stores Quota"); m["healthy"] != true {
		t.Fatalf("no quota left: %v", m)
	}
}

func TestCheckCPUs(t *testing.T) {
	if ok, msg := checkCPUs(1); !ok || !strings.Contains(msg, "warning") {
		t.Fatalf("1: %v %s", ok, msg)
	}
	if ok, msg := checkCPUs(minCPUs); !ok || strings.Contains(msg, "warning") {
		t.Fatalf("%d: %v %s", minCPUs, ok, msg)
	}
}

// The scheduler pause: admin only, from a password or session, shown in the
// task list and the checks.
func TestPauseScheduler(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "PUT", "/-/api/tasks/scheduler", `{}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/tasks/scheduler", `{"paused":true}`), http.StatusOK)
	if v := e.get(t, "/-/api/tasks", "admin").json(t); v["paused"] != true {
		t.Fatalf("list: %v", v["paused"])
	}
	if m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Scheduler"); m["healthy"] != false || !strings.Contains(m["message"].(string), "paused") {
		t.Fatalf("check: %v", m)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/tasks/scheduler", `{"paused":false}`), http.StatusOK)
	if m := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Scheduler"); m["healthy"] != true {
		t.Fatalf("check: %v", m)
	}
}
