package cleanup

import (
	"strings"
	"testing"
	"time"
)

// TestP2Evaluate: a proxy's cached features, plugins and binaries are one
// component each (the directory is the group), also under a composite
// child's .remote/<sha1>/ mirror; the repository documents, the legacy
// pack200 files and the child table are none.
func TestP2Evaluate(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("p", "p2", "proxy")
	for _, p := range []string{"plugins/a_1.0.0.jar", "plugins/a_1.0.0.jar.pack200.gz", "plugins/a_2.0.0.v20260101-1200.jar", "features/f_1.0.0.jar", "binary/b_root_1.0.0",
		".remote/da39a3ee5e6b4b0d3255bfef95601890afd80709/plugins/c_1.0.0.jar", ".remote/index.json", "p2.index", "content.jar", "artifacts.xml", "compositeContent.xml"} {
		e.put(t, "p", p, 1, daysAgo(60), time.Time{})
	}
	e.put(t, "p", "plugins/new_1.0.0.jar", 1, daysAgo(1), time.Time{})
	p := policy(Criteria{ComponentAgeDays: 30})
	p.Format = "p2"
	if got := e.labels(t, rp, p); got != "binary/b_root@1.0.0,features/f@1.0.0,plugins/a@1.0.0,plugins/a@2.0.0.v20260101-1200,plugins/c@1.0.0" {
		t.Errorf("candidates: %s", got)
	}
	p.NamePattern = "plugins:a"
	if got := e.labels(t, rp, p); got != "plugins/a@1.0.0,plugins/a@2.0.0.v20260101-1200" {
		t.Errorf("pattern: %s", got)
	}
	if err := (&Policy{Name: "p", Format: "p2", Criteria: Criteria{ComponentAgeDays: 1}}).Validate(); err != nil {
		t.Errorf("p2 is a policy format: %v", err)
	}
	if err := (&Policy{Name: "p", Format: "osgi", Criteria: Criteria{ComponentAgeDays: 1}}).Validate(); err == nil || !strings.Contains(err.Error(), " p2") {
		t.Errorf("message lists p2: %v", err)
	}
}
