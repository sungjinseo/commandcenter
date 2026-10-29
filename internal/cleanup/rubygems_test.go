package cleanup

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRubygemsEvaluate: gem files are one component each (the platform in
// the version), the index files none; a prerelease is a version with a
// letter, a platform suffix is not one.
func TestRubygemsEvaluate(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("r", "rubygems", "hosted")
	for _, p := range []string{"gems/a-1.0.0.gem", "gems/a-1.0.0-java.gem", "gems/a-2.0.0.pre.1.gem", "gems/ab-0.1.0.gem", "versions", "names"} {
		e.put(t, "r", p, 1, daysAgo(60), time.Time{})
	}
	for _, c := range []struct {
		release string
		want    string
	}{
		{"any", "a@1.0.0,a@1.0.0-java,a@2.0.0.pre.1,ab@0.1.0"},
		{"release", "a@1.0.0,a@1.0.0-java,ab@0.1.0"},
		{"prerelease", "a@2.0.0.pre.1"},
	} {
		p := policy(Criteria{ComponentAgeDays: 30, ReleaseType: c.release})
		p.Format = "rubygems"
		cands, _, _, _, err := e.ev.Evaluate(context.Background(), rp, p, 100)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, cd := range cands {
			got = append(got, cd.Label())
		}
		if strings.Join(got, ",") != c.want {
			t.Errorf("%s: %q, want %q", c.release, strings.Join(got, ","), c.want)
		}
	}
	gemsPolicy := Policy{Name: "p", Format: "rubygems", Criteria: Criteria{ComponentAgeDays: 1}}
	if err := gemsPolicy.Validate(); err != nil {
		t.Errorf("rubygems is a policy format: %v", err)
	}
}
