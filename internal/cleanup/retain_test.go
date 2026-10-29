package cleanup

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// versionsOf evaluates and returns the candidates' versions, sorted.
func (e *evalEnv) versionsOf(t *testing.T, rp string, format string, p Policy) []string {
	t.Helper()
	cands, _, _, _, err := e.ev.Evaluate(context.Background(), testRepo(rp, format, "proxy"), p, DefaultScanBudget) // proxies of every format can be cleaned
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cands {
		out = append(out, c.Version)
	}
	slices.Sort(out)
	return out
}

// TestEvaluateRetain: per format, files of one component listed in
// ascending version order and created newest first, so ranking by version
// and by date disagree. Retain 1 keeps the highest version, or the newest
// file with sortBy date; the rest are candidates.
func TestEvaluateRetain(t *testing.T) {
	for _, tc := range []struct {
		format   string
		paths    []string // ascending versions
		versions []string // the component versions of paths
	}{
		{"maven", []string{"org/ex/app/1.0-rc1/app-1.0-rc1.jar", "org/ex/app/1.0/app-1.0.jar", "org/ex/app/1.0.1/app-1.0.1.jar", "org/ex/app/1.9/app-1.9.jar", "org/ex/app/1.10/app-1.10.jar"},
			[]string{"1.0-rc1", "1.0", "1.0.1", "1.9", "1.10"}},
		{"npm", []string{"x/-/x-1.0.0-rc.1.tgz", "x/-/x-1.0.0.tgz", "x/-/x-1.2.0.tgz", "x/-/x-1.10.0.tgz"},
			[]string{"1.0.0-rc.1", "1.0.0", "1.2.0", "1.10.0"}},
		{"pypi", []string{"packages/demo/demo-1.0.dev1.tar.gz", "packages/demo/demo-1.0a1.tar.gz", "packages/demo/demo-1.0.tar.gz", "packages/demo/demo-1.0.post1.tar.gz"},
			[]string{"1.0.dev1", "1.0a1", "1.0", "1.0.post1"}},
		{"yum", []string{"el9/foo-1.0-1.el9.x86_64.rpm", "el9/foo-1.0-2.el9.x86_64.rpm", "el9/foo-1.0-10.el9.x86_64.rpm"},
			[]string{"1.0-1.el9.x86_64", "1.0-2.el9.x86_64", "1.0-10.el9.x86_64"}},
		{"apt", []string{"pool/main/f/foo/foo_1.0~rc1-1_amd64.deb", "pool/main/f/foo/foo_1.0-1_amd64.deb", "pool/main/f/foo/foo_1%3a0.5-1_amd64.deb"},
			[]string{"1.0~rc1-1", "1.0-1", "1:0.5-1"}},
		{"rubygems", []string{"gems/foo-0.9.gem", "gems/foo-1.0.0.pre.gem", "gems/foo-1.0.0.gem"},
			[]string{"0.9", "1.0.0.pre", "1.0.0"}},
		{"helm", []string{"foo-1.0.0.tgz", "foo-1.2.0.tgz", "foo-1.10.0.tgz"}, []string{"1.0.0", "1.2.0", "1.10.0"}},
		{"conda", []string{"linux-64/foo-1.0-0.tar.bz2", "linux-64/foo-1.9-0.conda", "linux-64/foo-1.10-0.tar.bz2"},
			[]string{"1.0-0", "1.9-0", "1.10-0"}},
		{"r", []string{"src/contrib/foo_1.0-1.tar.gz", "src/contrib/foo_1.0-2.tar.gz", "src/contrib/foo_1.0-10.tar.gz"},
			[]string{"1.0-1", "1.0-2", "1.0-10"}},
		{"nuget", []string{"foo/1.0.0-beta/foo.1.0.0-beta.nupkg", "foo/1.0.0/foo.1.0.0.nupkg", "foo/1.10.0/foo.1.10.0.nupkg"},
			[]string{"1.0.0-beta", "1.0.0", "1.10.0"}},
		{"go", []string{"ex.com/m/@v/v1.0.0.zip", "ex.com/m/@v/v1.2.0.zip", "ex.com/m/@v/v1.10.0.zip"}, []string{"v1.0.0", "v1.2.0", "v1.10.0"}},
		{"cargo", []string{"crates/foo/1.0.0.crate", "crates/foo/1.2.0.crate", "crates/foo/1.10.0.crate"}, []string{"1.0.0", "1.2.0", "1.10.0"}},
		{"composer", []string{"dist/v/p/1.0.0.0-beta1.zip", "dist/v/p/1.0.0.0.zip", "dist/v/p/1.10.0.0.zip"}, []string{"1.0.0.0-beta1", "1.0.0.0", "1.10.0.0"}},
	} {
		t.Run(tc.format, func(t *testing.T) {
			e := newEvalEnv(t)
			for i, p := range tc.paths {
				e.put(t, "r", p, 1, daysAgo(40+10*i), time.Time{}) // the lowest version is the newest file
			}
			n := len(tc.versions)
			byVersion := slices.Sorted(slices.Values(tc.versions[:n-1]))
			byDate := slices.Sorted(slices.Values(tc.versions[1:]))
			p := policy(Criteria{ComponentAgeDays: 30, Retain: 1})
			if got := e.versionsOf(t, "r", tc.format, p); !slices.Equal(got, byVersion) {
				t.Errorf("retain 1 by version: %v, want %v", got, byVersion)
			}
			p.RetainSortBy = "date"
			if got := e.versionsOf(t, "r", tc.format, p); !slices.Equal(got, byDate) {
				t.Errorf("retain 1 by date: %v, want %v", got, byDate)
			}
			p = policy(Criteria{Retain: n}) // retain alone, every version kept
			if got := e.versionsOf(t, "r", tc.format, p); len(got) != 0 {
				t.Errorf("retain %d: %v", n, got)
			}
			// The other criteria still apply to the older versions: none is
			// old enough here.
			if got := e.versionsOf(t, "r", tc.format, policy(Criteria{ComponentAgeDays: 1000, Retain: 1})); len(got) != 0 {
				t.Errorf("retain with age 1000: %v", got)
			}
		})
	}
}

func TestEvaluateRetainDetails(t *testing.T) {
	e := newEvalEnv(t)
	// Two architectures of one EVR are one version: kept together.
	for _, p := range []string{"foo-1.0-1.el9.x86_64.rpm", "foo-1.0-1.el9.aarch64.rpm", "foo-0.9-1.el9.x86_64.rpm", "bar-0.1-1.noarch.rpm"} {
		e.put(t, "y", p, 1, daysAgo(60), time.Time{})
	}
	if got := e.versionsOf(t, "y", "yum", policy(Criteria{Retain: 1})); !slices.Equal(got, []string{"0.9-1.el9.x86_64"}) {
		t.Fatalf("yum ties: %v", got)
	}
	// Each group and name is ranked on its own; raw files have one version.
	e.put(t, "m", "org/a/x/1.0/x-1.0.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "org/a/x/2.0/x-2.0.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "org/b/x/1.0/x-1.0.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "org/b/x/1.0/x-1.0-sources.jar", 1, daysAgo(60), time.Time{})
	if got := e.labels(t, testRepo("m", "maven", "hosted"), policy(Criteria{Retain: 1})); got != "org.a:x:1.0" {
		t.Fatalf("per name: %q", got)
	}
	e.put(t, "raw", "a/b.txt", 1, daysAgo(60), time.Time{})
	if got := e.labels(t, testRepo("raw", "raw", "hosted"), policy(Criteria{Retain: 1})); got != "" {
		t.Fatalf("raw: %q", got)
	}
	// The reason names the rank.
	cands, _, _, _, err := e.ev.Evaluate(context.Background(), testRepo("m", "maven", "hosted"), policy(Criteria{Retain: 1, ComponentAgeDays: 30}), DefaultScanBudget)
	if err != nil || len(cands) != 1 || !strings.Contains(cands[0].Reason, "version 2 of 2 by version, retain 1") || !strings.Contains(cands[0].Reason, "componentAgeDays 30") {
		t.Fatalf("reason: %+v %v", cands, err)
	}
}

// A version set cut by the scan budget is not ranked, and the next scan
// resumes at the start of the component, so it is ranked whole there.
func TestEvaluateRetainAcrossScans(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("m", "maven", "hosted")
	for _, a := range []string{"a", "b"} {
		for _, v := range []string{"1.0", "1.1", "1.2"} {
			e.put(t, "m", "org/ex/"+a+"/"+v+"/"+a+"-"+v+".jar", 1, daysAgo(60), time.Time{})
		}
	}
	p := policy(Criteria{Retain: 1})
	labels := func(ev Evaluation) string {
		var out []string
		for _, c := range ev.Candidates {
			out = append(out, c.Label())
		}
		return strings.Join(out, ",")
	}
	ev, err := e.ev.EvaluateFrom(context.Background(), rp, p, 4, "")
	if err != nil || !ev.Truncated || ev.Next != "org/ex/b/" || labels(ev) != "org.ex:a:1.0,org.ex:a:1.1" {
		t.Fatalf("first scan: %q next %q truncated %v err %v", labels(ev), ev.Next, ev.Truncated, err)
	}
	ev, err = e.ev.EvaluateFrom(context.Background(), rp, p, 4, ev.Next)
	if err != nil || ev.Truncated || ev.Next != "" || labels(ev) != "org.ex:b:1.0,org.ex:b:1.1" {
		t.Fatalf("second scan: %q next %q err %v", labels(ev), ev.Next, err)
	}
	// Started inside b, the scan does not rank b.
	ev, _ = e.ev.EvaluateFrom(context.Background(), rp, p, 10, "org/ex/b/1.1/")
	if labels(ev) != "" {
		t.Fatalf("scan from inside b: %q", labels(ev))
	}
	// Without retain, the unit (the version directory) is what may be cut.
	ev, _ = e.ev.EvaluateFrom(context.Background(), rp, policy(Criteria{ComponentAgeDays: 30}), 4, "")
	if ev.Next != "org/ex/b/1.1/" || labels(ev) != "org.ex:a:1.0,org.ex:a:1.1,org.ex:a:1.2,org.ex:b:1.0" {
		t.Fatalf("no retain: %q next %q", labels(ev), ev.Next)
	}
}

func TestEvaluateExcludeAndPathPattern(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("m", "maven", "hosted")
	e.put(t, "m", "org/ex/app/1.0/app-1.0.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "org/ex/app/1.0/app-1.0-sources.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "org/ex/app/1.1/app-1.1.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "org/ex/app/2.0/app-2.0.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "org/ex/lib/1.0/lib-1.0.jar", 1, daysAgo(60), time.Time{})
	age := Criteria{ComponentAgeDays: 30}

	c := age
	c.Exclude = Exclusion{NamePattern: `org\.ex:lib`}
	if got := e.labels(t, rp, policy(c)); got != "org.ex:app:1.0,org.ex:app:1.1,org.ex:app:2.0" {
		t.Fatalf("exclude name: %q", got)
	}
	c.Exclude = Exclusion{VersionPattern: `1\..*`}
	if got := e.labels(t, rp, policy(c)); got != "org.ex:app:2.0" {
		t.Fatalf("exclude version: %q", got)
	}
	c.Exclude = Exclusion{VersionPattern: `1`} // anchored: matches no version
	if got := e.labels(t, rp, policy(c)); got != "org.ex:app:1.0,org.ex:app:1.1,org.ex:app:2.0,org.ex:lib:1.0" {
		t.Fatalf("anchored: %q", got)
	}
	// Excluded versions still count for retain: 2.0 is kept by retain, 1.1
	// by the exclusion, 1.0 is the only candidate of app.
	c = Criteria{Retain: 1, Exclude: Exclusion{VersionPattern: `1\.1`}}
	if got := e.labels(t, rp, policy(c)); got != "org.ex:app:1.0" {
		t.Fatalf("retain and exclude: %q", got)
	}
	c = age
	c.PathPattern = `.*-sources\.jar`
	if got := e.labels(t, rp, policy(c)); got != "org.ex:app:1.0" {
		t.Fatalf("path pattern: %q", got)
	}
	c.PathPattern = `/org/ex/lib/.*` // Nexus paths start with "/"
	if got := e.labels(t, rp, policy(c)); got != "org.ex:lib:1.0" {
		t.Fatalf("path pattern with slash: %q", got)
	}
}

func TestPolicyValidateProFields(t *testing.T) {
	for _, tc := range []struct {
		c    Criteria
		want string
	}{
		{Criteria{Retain: 3}, ""},
		{Criteria{Retain: 1001}, "retain must be between 0 and 1000"},
		{Criteria{Retain: -1, ComponentAgeDays: 1}, "retain must be between"},
		{Criteria{Retain: 3, RetainSortBy: "size"}, `unknown retainSortBy "size"`},
		{Criteria{ComponentAgeDays: 1, PathPattern: "("}, "pathPattern:"},
		{Criteria{ComponentAgeDays: 1, Exclude: Exclusion{NamePattern: "("}}, "exclude.namePattern:"},
		{Criteria{ComponentAgeDays: 1, Exclude: Exclusion{VersionPattern: strings.Repeat("a", 201)}}, "exclude.versionPattern: at most 200"},
		{Criteria{}, "set componentAgeDays, lastDownloadedDays or retain"},
	} {
		p := policy(tc.c)
		err := p.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%+v: %v", tc.c, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%+v: %v, want %q", tc.c, err, tc.want)
		}
	}
	p := policy(Criteria{Retain: 2})
	if p.Validate(); p.RetainSortBy != "version" {
		t.Fatalf("default sortBy: %q", p.RetainSortBy)
	}
	p = policy(Criteria{ComponentAgeDays: 1, RetainSortBy: "date"})
	if p.Validate(); p.RetainSortBy != "" {
		t.Fatalf("sortBy without retain: %q", p.RetainSortBy)
	}
	p = policy(Criteria{ComponentAgeDays: 1})
	p.Notes = strings.Repeat("n", 1001)
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "notes") {
		t.Fatalf("notes: %v", err)
	}
}

func TestPolicyStoresProFields(t *testing.T) {
	e := newEvalEnv(t)
	ctx := context.Background()
	p := Policy{Name: "pro", Format: "maven", Notes: "keep three", Criteria: Criteria{Retain: 3, RetainSortBy: "date",
		PathPattern: ".*\\.jar", Exclude: Exclusion{NamePattern: "a:b", VersionPattern: "1.*"}}}
	if err := CreatePolicy(ctx, e.db, p); err != nil {
		t.Fatal(err)
	}
	got, err := GetPolicy(ctx, e.db, "pro")
	if err != nil {
		t.Fatal(err)
	}
	p.ReleaseType = "any"
	if got.Criteria != p.Criteria || got.Notes != "keep three" {
		t.Fatalf("stored: %+v", got)
	}
	m, _ := e.db.GetCleanupPolicy(ctx, "pro")
	plain := Policy{Name: "plain", Format: "maven", Criteria: Criteria{ComponentAgeDays: 1}}
	if err := CreatePolicy(ctx, e.db, plain); err != nil {
		t.Fatal(err)
	}
	m2, _ := e.db.GetCleanupPolicy(ctx, "plain")
	if strings.Contains(m2.Config, "retain") || strings.Contains(m2.Config, "exclude") || strings.Contains(m2.Config, "notes") ||
		!strings.Contains(m.Config, `"exclude":{"namePattern":"a:b","versionPattern":"1.*"}`) {
		t.Fatalf("configs: %s / %s", m.Config, m2.Config)
	}
}
