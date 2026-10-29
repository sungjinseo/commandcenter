package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

func TestMain(m *testing.M) {
	drainDelay = 0 // a Serve under test stops at once; TestServeReadinessAndDrain sets its own
	os.Exit(m.Run())
}

// fastNodes makes heartbeats and node expiry fast for the test.
func fastNodes(t *testing.T) {
	t.Helper()
	hb, ttl := meta.HeartbeatEvery, meta.NodeTTL
	meta.HeartbeatEvery, meta.NodeTTL = 100*time.Millisecond, time.Second
	t.Cleanup(func() { meta.HeartbeatEvery, meta.NodeTTL = hb, ttl })
}

// serveBackground runs a.Serve until the returned stop is called.
func serveBackground(t *testing.T, a *App) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return after cancel")
			return nil
		}
	}
}

// The single-writer guard (HA design 4.4): a second server on one
// PostgreSQL database is refused, naming the first, and starts once the
// first stopped. On SQLite there is no guard, as before.
func TestSecondServerRefused(t *testing.T) {
	fastNodes(t)
	cfg := testConfig(t, "")
	cfg.Listen = "127.0.0.1:0"
	a, err := New(cfg, quiet(), AsServer())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stop := serveBackground(t, a)
	b, err := New(cfg, quiet(), AsServer())
	if !metatest.Postgres() {
		if err != nil {
			t.Fatalf("SQLite: %v", err)
		}
		b.Close()
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		return
	}
	var guard *meta.GuardError
	if !errors.As(err, &guard) || guard.Other.ID != a.Node {
		t.Fatalf("second server: %v, want the guard naming %s", err, a.Node)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	b, err = New(cfg, quiet(), AsServer())
	if err != nil {
		t.Fatalf("after the first stopped: %v", err)
	}
	defer b.Close()
	if d := time.Since(start); d > meta.NodeTTL/2 {
		t.Fatalf("waited %v for a node that left", d)
	}
}

// Readiness (GET /service/rest/v1/status/writable) is 200 while serving;
// on cancel the node drains: 503 and the row says draining, for drainDelay
// on PostgreSQL before the server stops; then the row is gone.
func TestServeReadinessAndDrain(t *testing.T) {
	fastNodes(t)
	defer func(d time.Duration) { drainDelay = d }(drainDelay)
	drainDelay = time.Second
	cfg := testConfig(t, "")
	cfg.Listen = "127.0.0.1:0"
	a, err := New(cfg, quiet(), AsServer())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler) // the same handler, reachable without Serve's port
	defer ts.Close()
	writable := func() int {
		t.Helper()
		resp, err := http.Get(ts.URL + "/service/rest/v1/status/writable")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	nodes := func() []meta.Node {
		t.Helper()
		n, err := a.DB.Nodes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	stop := serveBackground(t, a)
	until("not ready while serving", func() bool { return writable() == http.StatusOK })
	if n := nodes(); len(n) != 1 || n[0].ID != a.Node || !n[0].Alive || !n[0].Leader || n[0].Draining {
		t.Fatalf("nodes while serving: %+v", n)
	}
	stopped := make(chan error, 1)
	start := time.Now()
	go func() { stopped <- stop() }()
	until("ready after cancel", func() bool { return writable() == http.StatusServiceUnavailable })
	if metatest.Postgres() {
		until("the row does not say draining", func() bool { n := nodes(); return len(n) == 1 && n[0].Draining })
		select {
		case <-stopped:
			t.Fatal("Serve returned before the drain delay")
		default:
		}
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); metatest.Postgres() && d < drainDelay || !metatest.Postgres() && d >= drainDelay {
		t.Fatalf("Serve took %v to stop (drain delay %v, PostgreSQL only)", d, drainDelay)
	}
	if n := nodes(); len(n) != 0 {
		t.Fatalf("nodes after Serve returned: %+v", n)
	}
	if code := writable(); code != http.StatusServiceUnavailable {
		t.Fatalf("writable after Serve returned: %d", code)
	}
}
