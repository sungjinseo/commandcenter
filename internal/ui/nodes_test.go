package ui

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// GET /-/api/nodes and the node in the Cluster check, the metrics and the
// system information.
func TestNodesAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	ctx := context.Background()
	wantStatus(t, e.get(t, "/-/api/nodes", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/nodes", "ci"), http.StatusForbidden)
	if r := e.get(t, "/-/api/nodes", "admin"); strings.TrimSpace(r.body) != `{"nodes":[],"self":""}` {
		t.Fatalf("before any node joined: %s", r.body)
	}
	if c := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Cluster"); c["healthy"] != true {
		t.Fatalf("Cluster check outside a server: %v", c)
	}

	m, err := e.db.Join(ctx, "host-1a2b", "host", "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Leave(ctx)
	e.ui.Node = "host-1a2b"
	for deadline := time.Now().Add(5 * time.Second); e.db.Ready(ctx) != nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %v", e.db.Ready(ctx))
		}
	}
	r := e.get(t, "/-/api/nodes", "admin")
	wantStatus(t, r, http.StatusOK)
	var got struct {
		Self  string
		Nodes []struct {
			ID, Hostname, Version, StartedAt, HeartbeatAt string
			Schema                                        int
			Alive, Draining, Leader                       bool
		}
	}
	if err := json.Unmarshal([]byte(r.body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Self != "host-1a2b" || len(got.Nodes) != 1 {
		t.Fatalf("nodes: %s", r.body)
	}
	n := got.Nodes[0]
	if n.ID != "host-1a2b" || n.Hostname != "host" || n.Version != "test" || n.Schema == 0 || !n.Alive || n.Draining || !n.Leader {
		t.Fatalf("node: %s", r.body)
	}
	if _, err := time.Parse(time.RFC3339, n.StartedAt); err != nil {
		t.Fatalf("startedAt %q", n.StartedAt)
	}
	if _, err := time.Parse(time.RFC3339, n.HeartbeatAt); err != nil {
		t.Fatalf("heartbeatAt %q", n.HeartbeatAt)
	}
	if c := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Cluster"); c["healthy"] != true || c["message"] != "1 live node(s): host-1a2b (leader)" {
		t.Fatalf("Cluster check: %v", c)
	}
	if b := e.get(t, "/-/metrics/prometheus", "admin").body; !strings.Contains(b, `commandcenter_node_info{node="host-1a2b",version="test"} 1`) {
		t.Fatalf("metrics: %.300s", b)
	}
	if s := e.get(t, "/-/api/systeminformation", "admin").json(t)["commandcenter-status"].(map[string]any); s["node"] != "host-1a2b" {
		t.Fatalf("system information: %v", s)
	}

	m.Drain(ctx)
	if c := checkByName(t, e.get(t, "/-/api/status/checks", "admin"), "Cluster"); c["healthy"] != false || !strings.Contains(c["message"].(string), "draining") {
		t.Fatalf("Cluster check while draining: %v", c)
	}
}
