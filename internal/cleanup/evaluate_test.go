package cleanup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
)

var evalNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return evalNow.Add(-time.Duration(n) * 24 * time.Hour) }

type evalEnv struct {
	db *meta.DB
	ev *Evaluator
}

func newEvalEnv(t *testing.T) *evalEnv {
	t.Helper()
	db, err := meta.Open(metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &evalEnv{db: db, ev: &Evaluator{DB: db, Now: func() time.Time { return evalNow }}}
}

// put stores an asset created at created; a non-zero lastDl records one download then.
func (e *evalEnv) put(t *testing.T, repoName, p string, size int64, created, lastDl time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := e.db.PutAsset(ctx, meta.Asset{Repo: repoName, Path: p, BlobSHA256: "sha-" + p, Size: size, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if !lastDl.IsZero() {
		if err := e.db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: repoName, Path: p, N: 1, Last: lastDl}}); err != nil {
			t.Fatal(err)
		}
	}
}

func testRepo(name, format, typ string) *repo.Repository {
	return &repo.Repository{Repository: config.Repository{Name: name, Format: format, Type: typ}}
}

func policy(c Criteria) Policy { return Policy{Name: "p", Format: "all", Criteria: c} }

// labels evaluates and returns the candidates' labels, comma-separated, in order.
func (e *evalEnv) labels(t *testing.T, rp *repo.Repository, p Policy) string {
	t.Helper()
	cands, count, _, _, err := e.ev.Evaluate(context.Background(), rp, p, DefaultScanBudget)
	if err != nil {
		t.Fatal(err)
	}
	if count != len(cands) {
		t.Fatalf("count %d, %d candidates", count, len(cands))
	}
	var out []string
	for _, c := range cands {
		out = append(out, c.Label())
	}
	return strings.Join(out, ",")
}

func TestEvaluateComponentAge(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("m", "maven", "hosted")
	e.put(t, "m", "org/ex/old/1.0/old-1.0.jar", 10, daysAgo(40), time.Time{})
	e.put(t, "m", "org/ex/old/1.0/old-1.0.pom", 1, daysAgo(41), time.Time{})
	e.put(t, "m", "org/ex/new/1.0/new-1.0.jar", 10, daysAgo(10), time.Time{})
	// one asset is recent: the component's newest asset decides
	e.put(t, "m", "org/ex/mixed/1.0/mixed-1.0.jar", 10, daysAgo(90), time.Time{})
	e.put(t, "m", "org/ex/mixed/1.0/mixed-1.0-sources.jar", 10, daysAgo(2), time.Time{})
	// not a component
	e.put(t, "m", "org/ex/old/maven-metadata.xml", 1, daysAgo(400), time.Time{})

	if got := e.labels(t, rp, policy(Criteria{ComponentAgeDays: 30})); got != "org.ex:old:1.0" {
		t.Fatalf("age 30: %q", got)
	}
	if got := e.labels(t, rp, policy(Criteria{ComponentAgeDays: 1})); got != "org.ex:mixed:1.0,org.ex:new:1.0,org.ex:old:1.0" {
		t.Fatalf("age 1: %q", got)
	}
	cands, count, bytes, truncated, err := e.ev.Evaluate(context.Background(), rp, policy(Criteria{ComponentAgeDays: 30}), DefaultScanBudget)
	if err != nil || count != 1 || bytes != 11 || truncated {
		t.Fatalf("totals: %d %d %v %v", count, bytes, truncated, err)
	}
	c := cands[0]
	if c.Repo != "m" || c.Format != "maven" || len(c.Assets) != 2 || c.Bytes != 11 ||
		!c.Created.Equal(daysAgo(41)) || !c.LastDownloaded.IsZero() {
		t.Fatalf("candidate: %+v", c)
	}
}

func TestEvaluateLastDownloaded(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("n", "npm", "proxy")
	e.put(t, "n", "recent/-/recent-1.0.0.tgz", 1, daysAgo(100), daysAgo(5))
	e.put(t, "n", "stale/-/stale-1.0.0.tgz", 1, daysAgo(100), daysAgo(60))
	e.put(t, "n", "never-old/-/never-old-1.0.0.tgz", 1, daysAgo(100), time.Time{})
	e.put(t, "n", "never-new/-/never-new-1.0.0.tgz", 1, daysAgo(10), time.Time{})

	if got := e.labels(t, rp, policy(Criteria{LastDownloadedDays: 30})); got != "never-old@1.0.0,stale@1.0.0" {
		t.Fatalf("lastDownloaded 30: %q", got)
	}
	cands, _, _, _, _ := e.ev.Evaluate(context.Background(), rp, policy(Criteria{LastDownloadedDays: 30}), DefaultScanBudget)
	if !cands[1].LastDownloaded.Equal(daysAgo(60)) || !cands[0].LastDownloaded.IsZero() {
		t.Fatalf("lastDownloaded fields: %+v", cands)
	}
}

func TestEvaluateNeverDownloadedAssetKeepsComponent(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("m", "maven", "proxy")
	// The jar was downloaded long ago, but the sources jar arrived recently
	// and was never downloaded: it counts from its creation.
	e.put(t, "m", "g/a/1.0/a-1.0.jar", 1, daysAgo(200), daysAgo(100))
	e.put(t, "m", "g/a/1.0/a-1.0-sources.jar", 1, daysAgo(3), time.Time{})
	if got := e.labels(t, rp, policy(Criteria{LastDownloadedDays: 30})); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestEvaluateCriteriaCombineWithAnd(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("n", "npm", "hosted")
	e.put(t, "n", "both/-/both-1.0.0.tgz", 1, daysAgo(100), daysAgo(60))
	e.put(t, "n", "old-used/-/old-used-1.0.0.tgz", 1, daysAgo(100), daysAgo(1))
	e.put(t, "n", "new-unused/-/new-unused-1.0.0.tgz", 1, daysAgo(10), time.Time{})
	e.put(t, "n", "both/-/both-2.0.0-beta.1.tgz", 1, daysAgo(100), daysAgo(60))

	c := Criteria{ComponentAgeDays: 30, LastDownloadedDays: 30}
	if got := e.labels(t, rp, policy(c)); got != "both@1.0.0,both@2.0.0-beta.1" {
		t.Fatalf("ages: %q", got)
	}
	c.ReleaseType = "prerelease"
	if got := e.labels(t, rp, policy(c)); got != "both@2.0.0-beta.1" {
		t.Fatalf("prerelease: %q", got)
	}
	c.ReleaseType = "release"
	if got := e.labels(t, rp, policy(c)); got != "both@1.0.0" {
		t.Fatalf("release: %q", got)
	}
	c.ReleaseType, c.NamePattern = "any", "nothing"
	if got := e.labels(t, rp, policy(c)); got != "" {
		t.Fatalf("pattern: %q", got)
	}
}

func TestPrereleasePerFormat(t *testing.T) {
	for _, c := range []struct {
		format, version string
		pre             bool
	}{
		{"maven", "1.0-SNAPSHOT", true},
		{"maven", "1.0", false},
		{"maven", "1.0.0-rc1", false}, // a Maven release, whatever its name
		{"npm", "1.0.0", false},
		{"npm", "1.0.0-beta.1", true},
		{"npm", "not-semver", false},
		{"go", "v1.2.3", false},
		{"go", "v1.2.3-rc.1", true},
		{"go", "v0.0.0-20240101000000-abcdef123456", true}, // pseudo-version
		{"go", "v2.0.0+incompatible", false},
		{"cargo", "1.0.0", false},
		{"cargo", "1.0.0-alpha", true},
		{"pypi", "1.0", false},
		{"pypi", "1.0.post1", false},
		{"pypi", "1.0rc1", true},
		{"pypi", "2.0b2", true},
		{"pypi", "1.0a1", true},
		{"pypi", "1.0.dev3", true},
		{"pypi", "1.0.post1.dev2", true},
		{"pypi", "1.0+local.abc", false},
		{"pypi", "1!2.0.0beta", true},
	} {
		if got := Prerelease(c.format, c.version); got != c.pre {
			t.Errorf("Prerelease(%s, %s) = %v, want %v", c.format, c.version, got, c.pre)
		}
	}
}

func TestEvaluateReleaseTypeMavenAndPypi(t *testing.T) {
	e := newEvalEnv(t)
	m := testRepo("m", "maven", "hosted")
	e.put(t, "m", "g/a/1.0/a-1.0.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "g/a/1.1-SNAPSHOT/a-1.1-20260101.120000-1.jar", 1, daysAgo(60), time.Time{})
	if got := e.labels(t, m, policy(Criteria{ComponentAgeDays: 30, ReleaseType: "prerelease"})); got != "g:a:1.1-SNAPSHOT" {
		t.Fatalf("maven prerelease: %q", got)
	}
	if got := e.labels(t, m, policy(Criteria{ComponentAgeDays: 30, ReleaseType: "release"})); got != "g:a:1.0" {
		t.Fatalf("maven release: %q", got)
	}

	p := testRepo("py", "pypi", "hosted")
	e.put(t, "py", "packages/demo/demo-1.0.tar.gz", 1, daysAgo(60), time.Time{})
	e.put(t, "py", "packages/demo/demo-1.0-py3-none-any.whl", 1, daysAgo(60), time.Time{})
	e.put(t, "py", "packages/demo/demo-1.1rc1.tar.gz", 1, daysAgo(60), time.Time{})
	if got := e.labels(t, p, policy(Criteria{ComponentAgeDays: 30, ReleaseType: "prerelease"})); got != "demo@1.1rc1" {
		t.Fatalf("pypi prerelease: %q", got)
	}
	cands, _, _, _, _ := e.ev.Evaluate(context.Background(), p, policy(Criteria{ComponentAgeDays: 30, ReleaseType: "release"}), DefaultScanBudget)
	if len(cands) != 1 || cands[0].Label() != "demo@1.0" || len(cands[0].Assets) != 2 {
		t.Fatalf("pypi release: %+v", cands)
	}
}

func TestEvaluateNamePattern(t *testing.T) {
	e := newEvalEnv(t)
	m := testRepo("m", "maven", "hosted")
	e.put(t, "m", "org/ex/app/1.0/app-1.0.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "org/ex/app-test/1.0/app-test-1.0.jar", 1, daysAgo(60), time.Time{})
	e.put(t, "m", "com/other/app/1.0/app-1.0.jar", 1, daysAgo(60), time.Time{})
	for pattern, want := range map[string]string{
		`org\.ex:app`:      "org.ex:app:1.0",                     // whole group:name
		`org\.ex:.*`:       "org.ex:app:1.0,org.ex:app-test:1.0", // any name in the group
		`app`:              "",                                   // not a substring match
		`.*:app`:           "com.other:app:1.0,org.ex:app:1.0",
		`(?i)ORG\.EX:APP`:  "org.ex:app:1.0",
		`org\.ex:app|.*-t`: "org.ex:app:1.0",
	} {
		if got := e.labels(t, m, policy(Criteria{ComponentAgeDays: 30, NamePattern: pattern})); got != want {
			t.Errorf("pattern %q: got %q, want %q", pattern, got, want)
		}
	}

	n := testRepo("n", "npm", "hosted")
	e.put(t, "n", "@scope/util/-/util-1.0.0.tgz", 1, daysAgo(60), time.Time{})
	e.put(t, "n", "left-pad/-/left-pad-1.0.0.tgz", 1, daysAgo(60), time.Time{})
	if got := e.labels(t, n, policy(Criteria{ComponentAgeDays: 30, NamePattern: `left-.*`})); got != "left-pad@1.0.0" {
		t.Fatalf("npm unscoped: %q", got)
	}
	if got := e.labels(t, n, policy(Criteria{ComponentAgeDays: 30, NamePattern: `@scope:.*`})); got != "@scope/util@1.0.0" {
		t.Fatalf("npm scoped: %q", got)
	}
}

func TestEvaluateScanBudget(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("m", "maven", "proxy")
	for i := 1; i <= 4; i++ {
		e.put(t, "m", fmt.Sprintf("g/a%d/1.0/a%d-1.0.jar", i, i), 10, daysAgo(60), time.Time{})
		e.put(t, "m", fmt.Sprintf("g/a%d/1.0/a%d-1.0.pom", i, i), 1, daysAgo(60), time.Time{})
	}
	e.put(t, "m", "g/a3/2.0/a3-2.0.jar", 10, daysAgo(60), time.Time{})
	p := policy(Criteria{ComponentAgeDays: 30})
	defer func(n int) { scanPage = n }(scanPage)

	for _, page := range []int{1000, 2, 1} {
		scanPage = page
		// Budget 5 reads a1 (2), a2 (2) and the first a3 asset: every a3
		// version may be incomplete, so none of them is a candidate.
		cands, count, bytes, truncated, err := e.ev.Evaluate(context.Background(), rp, p, 5)
		if err != nil || !truncated || count != 2 || bytes != 22 {
			t.Fatalf("page %d budget 5: count=%d bytes=%d truncated=%v err=%v", page, count, bytes, truncated, err)
		}
		if cands[0].Label() != "g:a1:1.0" || cands[1].Label() != "g:a2:1.0" {
			t.Fatalf("page %d budget 5: %+v", page, cands)
		}
		// Exactly the number of assets, or more: not truncated.
		for _, budget := range []int{9, 10, DefaultScanBudget} {
			if _, count, _, truncated, _ := e.ev.Evaluate(context.Background(), rp, p, budget); truncated || count != 5 {
				t.Fatalf("page %d budget %d: count=%d truncated=%v", page, budget, count, truncated)
			}
		}
		// Budget 8 stops before a4: both a3 versions were read completely.
		if _, count, _, truncated, _ := e.ev.Evaluate(context.Background(), rp, p, 8); !truncated || count != 4 {
			t.Fatalf("page %d budget 8: count=%d truncated=%v", page, count, truncated)
		}
	}
}

func TestEvaluateRefusesRepositories(t *testing.T) {
	e := newEvalEnv(t)
	ctx := context.Background()
	p := policy(Criteria{ComponentAgeDays: 1})
	for _, rp := range []*repo.Repository{testRepo("g", "maven", "group"), testRepo("c", "cargo", "hosted")} {
		if _, _, _, _, err := e.ev.Evaluate(ctx, rp, p, DefaultScanBudget); !errors.Is(err, ErrNotSupported) {
			t.Errorf("%s %s: %v", rp.Format, rp.Type, err)
		}
	}
	if _, _, _, _, err := e.ev.Evaluate(ctx, testRepo("c", "cargo", "proxy"), p, DefaultScanBudget); err != nil {
		t.Errorf("cargo proxy: %v", err)
	}
	mp := Policy{Name: "p", Format: "npm", Criteria: Criteria{ComponentAgeDays: 1}}
	if _, _, _, _, err := e.ev.Evaluate(ctx, testRepo("m", "maven", "hosted"), mp, DefaultScanBudget); err == nil || !strings.Contains(err.Error(), "npm") {
		t.Errorf("format mismatch: %v", err)
	}
	if _, _, _, _, err := e.ev.Evaluate(ctx, testRepo("m", "maven", "hosted"), policy(Criteria{}), DefaultScanBudget); err == nil {
		t.Error("policy without ages evaluated")
	}
}

func TestPolicyValidate(t *testing.T) {
	ok := Policy{Name: "keep-30d", Format: "all", Criteria: Criteria{ComponentAgeDays: 30}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, p := range map[string]Policy{
		"no ages":       {Name: "p", Format: "all"},
		"bad name":      {Name: "bad name", Format: "all", Criteria: Criteria{ComponentAgeDays: 1}},
		"reserved":      {Name: "create", Format: "all", Criteria: Criteria{ComponentAgeDays: 1}},
		"bad format":    {Name: "p", Format: "docker", Criteria: Criteria{ComponentAgeDays: 1}},
		"negative":      {Name: "p", Format: "all", Criteria: Criteria{ComponentAgeDays: -1, LastDownloadedDays: 3}},
		"huge":          {Name: "p", Format: "all", Criteria: Criteria{ComponentAgeDays: 100000}},
		"release type":  {Name: "p", Format: "all", Criteria: Criteria{ComponentAgeDays: 1, ReleaseType: "stable"}},
		"bad regex":     {Name: "p", Format: "all", Criteria: Criteria{ComponentAgeDays: 1, NamePattern: "a("}},
		"long pattern":  {Name: "p", Format: "all", Criteria: Criteria{ComponentAgeDays: 1, NamePattern: strings.Repeat("a", 201)}},
		"backreference": {Name: "p", Format: "all", Criteria: Criteria{ComponentAgeDays: 1, NamePattern: `(a)\1`}},
	} {
		var ve *repo.ValidationError
		if err := p.Validate(); !errors.As(err, &ve) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := Policy{Name: "p", Format: "maven", Criteria: Criteria{LastDownloadedDays: 1}}
	if err := p.Validate(); err != nil || p.ReleaseType != "any" {
		t.Fatalf("default release type: %q %v", p.ReleaseType, err)
	}
}

// TestEvaluateScanBudgetPerFormat checks the partial-component rule at the
// budget for each format's path layout: budget n reads the first n assets in
// path order, and every component that may still have unread assets is left
// out: an npm or Cargo component is one file, a Go one its version's files, a
// Maven one its version directory, and PyPI leaves out the whole project.
func TestEvaluateScanBudgetPerFormat(t *testing.T) {
	cases := []struct {
		format string
		paths  []string       // in path order
		want   map[int]string // budget -> labels (all truncated except the last budget)
	}{
		{"npm", []string{
			"@s/a/-/a-1.0.0.tgz", "@s/a/-/a-2.0.0.tgz", // scoped
			"@s/ab/-/ab-1.0.0.tgz",
			"left/-/left-1.0.0.tgz", "left/-/left-1.1.0.tgz",
		}, map[int]string{
			1: "@s/a@1.0.0", 2: "@s/a@1.0.0,@s/a@2.0.0", 3: "@s/a@1.0.0,@s/a@2.0.0,@s/ab@1.0.0",
			4: "left@1.0.0,@s/a@1.0.0,@s/a@2.0.0,@s/ab@1.0.0", 5: "left@1.0.0,left@1.1.0,@s/a@1.0.0,@s/a@2.0.0,@s/ab@1.0.0", // unscoped (no group) first
		}},
		{"go", []string{
			"ex.com/m/@v/v1.0.0.info", "ex.com/m/@v/v1.0.0.mod", "ex.com/m/@v/v1.0.0.zip", "ex.com/m/@v/v1.1.0.zip",
			"ex.com/m/sub/@v/v0.1.0.zip", // a nested module has its own home
		}, map[int]string{
			2: "", 3: "ex.com/m@v1.0.0", 4: "ex.com/m@v1.0.0,ex.com/m@v1.1.0", 5: "ex.com/m@v1.0.0,ex.com/m@v1.1.0,ex.com/m/sub@v0.1.0",
		}},
		{"pypi", []string{
			"packages/demo/demo-1.0-py3-none-any.whl", "packages/demo/demo-1.0.tar.gz", // whl and sdist split by budget 1
			"packages/other/other-2.0.tar.gz",
		}, map[int]string{1: "", 2: "demo@1.0", 3: "demo@1.0,other@2.0"}},
		{"cargo", []string{
			"crates/a/1.0.0.crate", "crates/a/1.1.0.crate", "crates/ab/0.1.0.crate",
		}, map[int]string{1: "a@1.0.0", 2: "a@1.0.0,a@1.1.0", 3: "a@1.0.0,a@1.1.0,ab@0.1.0"}},
		{"helm", []string{ // index.yaml is not a component
			"a-1.0.0.tgz", "a-1.1.0.tgz", "ab-0.1.0.tgz", "index.yaml",
		}, map[int]string{1: "a@1.0.0", 2: "a@1.0.0,a@1.1.0", 3: "a@1.0.0,a@1.1.0,ab@0.1.0", 4: "a@1.0.0,a@1.1.0,ab@0.1.0"}},
		{"yum", []string{ // repodata/ is not a component; the directory qualifies the name
			"a-1.0-1.noarch.rpm", "a-1.0-1.x86_64.rpm", "el9/a-1.0-1.el9.x86_64.rpm", "el9/repodata/repomd.xml",
		}, map[int]string{1: "a@1.0-1.noarch", 2: "a@1.0-1.noarch,a@1.0-1.x86_64", 3: "a@1.0-1.noarch,a@1.0-1.x86_64,el9/a@1.0-1.el9.x86_64", 4: "a@1.0-1.noarch,a@1.0-1.x86_64,el9/a@1.0-1.el9.x86_64"}},
		{"nuget", []string{ // the .nuspec goes with its .nupkg; the proxy's v3-flatcontainer/ prefix too
			"v3-flatcontainer/a/1.0.0/a.1.0.0.nupkg", "v3-flatcontainer/a/1.0.0/a.1.0.0.nuspec",
			"v3-flatcontainer/a/1.1.0/a.1.1.0.nupkg", "v3-flatcontainer/a/1.1.0/a.1.1.0.nuspec",
			"v3-flatcontainer/a/index.json", "v3-flatcontainer/ab/0.1.0/ab.0.1.0.nupkg",
		}, map[int]string{1: "", 2: "a@1.0.0", 3: "a@1.0.0", 4: "a@1.0.0,a@1.1.0", 5: "a@1.0.0,a@1.1.0", 6: "a@1.0.0,a@1.1.0,ab@0.1.0"}},
		{"apt", []string{ // one file per package_version_arch; dists/ is not a component
			"dists/stable/Release", "pool/main/a/a/a_1.0_all.deb", "pool/main/a/a/a_1.0_amd64.deb", "pool/main/a/ab/ab_0.1_amd64.deb",
		}, map[int]string{2: "all/a@1.0", 3: "all/a@1.0,amd64/a@1.0", 4: "all/a@1.0,amd64/a@1.0,amd64/ab@0.1"}},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			e := newEvalEnv(t)
			rp := testRepo("r", c.format, "proxy")
			for _, p := range c.paths {
				e.put(t, "r", p, 1, daysAgo(60), time.Time{})
			}
			for budget, want := range c.want {
				cands, count, _, truncated, err := e.ev.Evaluate(context.Background(), rp, policy(Criteria{ComponentAgeDays: 30}), budget)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, cd := range cands {
					got = append(got, cd.Label())
				}
				if strings.Join(got, ",") != want || count != len(cands) || truncated != (budget < len(c.paths)) {
					t.Errorf("budget %d: got %q truncated=%v, want %q", budget, strings.Join(got, ","), truncated, want)
				}
			}
			// the split PyPI component has both files once complete
			if c.format == "pypi" {
				cands, _, _, _, _ := e.ev.Evaluate(context.Background(), rp, policy(Criteria{ComponentAgeDays: 30}), 2)
				if len(cands) != 1 || len(cands[0].Assets) != 2 {
					t.Fatalf("pypi assets: %+v", cands)
				}
			}
		})
	}
}

func TestEvaluateUnknownCreationTime(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("n", "npm", "proxy")
	// No creation time at all: the age is unknown, so no age selects it,
	// even with an old download.
	e.put(t, "n", "nocreate/-/nocreate-1.0.0.tgz", 1, time.Time{}, daysAgo(100))
	e.put(t, "n", "never/-/never-1.0.0.tgz", 1, time.Time{}, time.Time{})
	// An undated file that is not part of a component does not matter.
	e.put(t, "n", "mixed/-/mixed-1.0.0.tgz", 1, daysAgo(100), daysAgo(100))
	e.put(t, "n", "mixed/-/mixed-1.0.0.tgz.sig", 1, time.Time{}, time.Time{}) // not a component file
	e.put(t, "n", "old/-/old-1.0.0.tgz", 1, daysAgo(100), daysAgo(100))
	for _, c := range []Criteria{{ComponentAgeDays: 30}, {LastDownloadedDays: 30}, {ComponentAgeDays: 1, LastDownloadedDays: 1}} {
		if got := e.labels(t, rp, policy(c)); got != "mixed@1.0.0,old@1.0.0" {
			t.Errorf("%+v: %q", c, got)
		}
	}

	m := testRepo("m", "maven", "proxy")
	e.put(t, "m", "g/a/1.0/a-1.0.jar", 1, daysAgo(100), daysAgo(100))
	e.put(t, "m", "g/a/1.0/a-1.0.pom", 1, time.Time{}, time.Time{})
	if got := e.labels(t, m, policy(Criteria{ComponentAgeDays: 30})); got != "" {
		t.Fatalf("maven with an undated pom: %q", got)
	}
}

// Evaluate records what it saw of each candidate.
func TestEvaluateRecordsEvaluation(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("m", "maven", "hosted")
	e.put(t, "m", "g/a/1.0/a-1.0.jar", 1, daysAgo(90), daysAgo(40))
	e.put(t, "m", "g/a/1.0/a-1.0.pom", 1, daysAgo(60), time.Time{})
	cands, _, _, _, err := e.ev.Evaluate(context.Background(), rp, policy(Criteria{ComponentAgeDays: 30}), DefaultScanBudget)
	if err != nil || len(cands) != 1 {
		t.Fatalf("%+v %v", cands, err)
	}
	ev := cands[0].Evaluated
	if ev == nil || len(ev.Paths) != 2 || !ev.Paths["g/a/1.0/a-1.0.pom"] || !ev.MaxCreated.Equal(daysAgo(60)) || !ev.LastDownloaded.Equal(daysAgo(40)) {
		t.Fatalf("evaluated: %+v", ev)
	}
}

// EvaluateFrom resumes where the previous scan stopped, at the start of the
// component it cut, and reports the end of the repository with Next "".
func TestEvaluateFromResumes(t *testing.T) {
	e := newEvalEnv(t)
	rp := testRepo("m", "maven", "proxy")
	var all []string
	for i := 0; i < 5; i++ {
		for _, ext := range []string{"jar", "pom"} {
			p := fmt.Sprintf("g/a/1.%d/a-1.%d.%s", i, i, ext)
			e.put(t, "m", p, 1, daysAgo(60), time.Time{})
			all = append(all, p)
		}
	}
	p := policy(Criteria{ComponentAgeDays: 30})
	seen := map[string]int{}
	from := ""
	var starts []string
	for run := 0; run < 10; run++ {
		res, err := e.ev.EvaluateFrom(context.Background(), rp, p, 3, from)
		if err != nil {
			t.Fatal(err)
		}
		starts = append(starts, from)
		for _, c := range res.Candidates {
			seen[c.Label()]++
		}
		from = res.Next
		if from == "" {
			break
		}
	}
	// Budget 3 cuts a component in every run: the next run starts with it.
	if got := strings.Join(starts, " | "); got != " | g/a/1.1/ | g/a/1.2/ | g/a/1.3/ | g/a/1.4/" {
		t.Fatalf("starts: %s", got)
	}
	if from != "" || len(seen) != 5 {
		t.Fatalf("covered %v, next %q", seen, from)
	}
	for l, n := range seen {
		if n != 1 {
			t.Errorf("%s evaluated %d times", l, n)
		}
	}
}
