package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/webhook"
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

// Two servers on one database start and serve side by side (high
// availability), both registered, alive and ready, one the leader, and the
// Cluster check is healthy.
func TestTwoServersStart(t *testing.T) {
	fastNodes(t)
	cfg := testConfig(t, "")
	cfg.Listen = "127.0.0.1:0"
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = append(cfg.Users, config.User{Name: "root", PasswordHash: string(h), Roles: []string{"admin"}})
	var apps []*App
	for range 2 {
		a, err := New(cfg, quiet(), AsServer())
		if err != nil {
			t.Fatalf("server %d: %v", len(apps)+1, err)
		}
		defer a.Close()
		stop := serveBackground(t, a)
		defer func() {
			if err := stop(); err != nil {
				t.Error(err)
			}
		}()
		apps = append(apps, a)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		nodes, err := apps[0].DB.Nodes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		alive, leaders := 0, 0
		for _, n := range nodes {
			if n.Alive && (n.ID == apps[0].Node || n.ID == apps[1].Node) {
				alive++
			}
			if n.Leader {
				leaders++
			}
		}
		ready := apps[0].DB.Ready(context.Background()) == nil && apps[1].DB.Ready(context.Background()) == nil
		if alive == 2 && leaders == 1 && ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nodes %+v (ready %v), want both alive and ready with one leader", nodes, ready)
		}
	}
	ts := httptest.NewServer(apps[0].Handler)
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/-/api/status/checks", nil)
	req.SetBasicAuth("root", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Checks []struct {
			Name, Message string
			Healthy       bool
		}
	}
	json.NewDecoder(resp.Body).Decode(&out)
	for _, c := range out.Checks {
		if c.Name != "Cluster" {
			continue
		}
		if !c.Healthy || !strings.Contains(c.Message, "2 live node(s)") {
			t.Fatalf("Cluster check: %+v", c)
		}
		return
	}
	t.Fatalf("no Cluster check: %+v", out)
}

// Readiness (GET /service/rest/v1/status/writable) is 200 while serving;
// on cancel the node drains: 503 and the row says draining, for drainDelay
// before the server stops; then the row is gone.
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
	until("the row does not say draining", func() bool { n := nodes(); return len(n) == 1 && n[0].Draining })
	select {
	case <-stopped:
		t.Fatal("Serve returned before the drain delay")
	default:
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < drainDelay {
		t.Fatalf("Serve took %v to stop (drain delay %v)", d, drainDelay)
	}
	if n := nodes(); len(n) != 0 {
		t.Fatalf("nodes after Serve returned: %+v", n)
	}
	if code := writable(); code != http.StatusServiceUnavailable {
		t.Fatalf("writable after Serve returned: %d", code)
	}
}

// A process that does not serve (a CLI command such as gc beside a server)
// stores the webhook deliveries its events make and sends none; a server on
// the database sends them, once.
func TestCLIStoresWebhookDeliveriesWithoutSending(t *testing.T) {
	var sent atomic.Int32
	rc := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent.Add(1) }))
	defer rc.Close()
	poll := webhook.PollEvery
	webhook.PollEvery = 20 * time.Millisecond
	t.Cleanup(func() { webhook.PollEvery = poll })
	ctx := context.Background()
	cfg := testConfig(t, "")
	cfg.Listen = "127.0.0.1:0"
	cli, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.DB.CreateWebhook(ctx, meta.Webhook{ID: "h", Name: "h", URL: rc.URL, Events: []string{"*"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := cli.Webhooks.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	cli.Webhooks.Emit("upload", "cli", "r", "a@1", "")
	waitDelivery := func(status string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			list, err := cli.DB.ListDeliveries(ctx, "h", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) == 1 && list[0].Status == status {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("deliveries %+v, want one %s", list, status)
			}
		}
	}
	waitDelivery(meta.DeliveryPending)
	cli.Webhooks.Kick()
	time.Sleep(200 * time.Millisecond)
	if n := sent.Load(); n != 0 {
		t.Fatalf("the CLI process sent %d requests", n)
	}
	srv, err := New(cfg, quiet(), AsServer())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	stop := serveBackground(t, srv)
	waitDelivery(meta.DeliveryOK)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if n := sent.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}
