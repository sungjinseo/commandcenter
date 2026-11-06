package cleanup

import (
	"testing"
	"time"
)

// core-18: a Maven SNAPSHOT build is a component of its own, aged alone;
// an exclude versionPattern written for X-SNAPSHOT keeps its builds; and
// retain counts base versions, the builds of one sharing its place.
func TestEvaluateMavenSnapshotBuilds(t *testing.T) {
	e := newEvalEnv(t)
	m := testRepo("m", "maven", "hosted")
	d := "g/a/1.0-SNAPSHOT/"
	for _, b := range []struct {
		file string
		days int
	}{{"a-1.0-20261001.120000-1.jar", 20}, {"a-1.0-20261001.120000-1.pom", 20}, {"a-1.0-20261011.120000-2.jar", 10}, {"a-1.0-20261020.120000-3.jar", 1}} {
		e.put(t, "m", d+b.file, 1, daysAgo(b.days), time.Time{})
	}
	e.put(t, "m", "g/a/0.9-SNAPSHOT/a-0.9-20260101.120000-1.jar", 1, daysAgo(300), time.Time{})
	if got := e.labels(t, m, policy(Criteria{ComponentAgeDays: 7})); got != "g:a:0.9-20260101.120000-1,g:a:1.0-20261001.120000-1,g:a:1.0-20261011.120000-2" {
		t.Fatalf("age: %q", got)
	}
	p := policy(Criteria{ComponentAgeDays: 7})
	p.Exclude.VersionPattern = ".*-SNAPSHOT"
	if got := e.labels(t, m, p); got != "" {
		t.Fatalf("exclude: %q", got)
	}
	p = policy(Criteria{Retain: 1, ComponentAgeDays: 7})
	if got := e.labels(t, m, p); got != "g:a:0.9-20260101.120000-1" {
		t.Fatalf("retain 1 keeps every build of 1.0-SNAPSHOT: %q", got)
	}
	// The three builds of 1.0-SNAPSHOT are one place, not three: retain 2
	// keeps 0.9-SNAPSHOT too, and retain 3 a release behind them, by
	// version and by date.
	e.put(t, "m", "g/a/0.8/a-0.8.jar", 1, daysAgo(400), time.Time{})
	for _, by := range []string{"version", "date"} {
		p = policy(Criteria{Retain: 2})
		p.RetainSortBy = by
		if got := e.labels(t, m, p); got != "g:a:0.8" {
			t.Fatalf("retain 2 by %s: %q", by, got)
		}
		p.Retain = 3
		if got := e.labels(t, m, p); got != "" {
			t.Fatalf("retain 3 by %s: %q", by, got)
		}
	}
}
