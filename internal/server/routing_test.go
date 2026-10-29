package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// A direct request to a proxy for a path its routing rule refuses is 403,
// after the access check and before the handler runs; other paths and the
// group that includes the proxy are not affected here.
func TestProxyRoutingRuleAnswers403(t *testing.T) {
	ts, rec := newServerWith(t, func(s *Server) {
		s.Registry = repo.NewRegistry(nil, []config.Repository{
			{Name: "releases", Format: "maven", Type: "hosted"},
			{Name: "central", Format: "maven", Type: "proxy", Remote: "https://repo1.example/maven2/", RoutingRule: "no-internal"},
		})
		if err := s.Registry.SetRoutingRules([]meta.RoutingRule{{Name: "no-internal", Mode: meta.RoutingBlock, Matchers: []string{`^/internal/`}}}); err != nil {
			t.Fatal(err)
		}
	})
	res, _ := http.Get(ts.URL + "/repository/central/internal/secret.jar")
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 403 || !strings.Contains(string(b), "blocked by routing rule") {
		t.Fatalf("blocked: %d %s", res.StatusCode, b)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("handler ran: %v", rec.calls)
	}
	if res := do(t, "GET", ts.URL+"/repository/central/org/lib.jar", ""); res.StatusCode != 200 {
		t.Fatalf("allowed: %d", res.StatusCode)
	}
	if res := do(t, "GET", ts.URL+"/repository/releases/internal/x", ""); res.StatusCode != 200 {
		t.Fatalf("hosted is not routed: %d", res.StatusCode)
	}
	// The access check comes first: a user who may not write gets 403 for
	// that, and an anonymous one 401, whatever the rule.
	if res := do(t, "PUT", ts.URL+"/repository/central/internal/x", ""); res.StatusCode != 401 {
		t.Fatalf("anonymous write: %d", res.StatusCode)
	}
}
