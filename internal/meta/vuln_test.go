package meta

import (
	"errors"

	"commandcenter/internal/config"
	"slices"
	"testing"
	"time"
)

func TestVulnTables(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	a := VulnAdvisory{ID: "GHSA-1", Ecosystem: "npm", Package: "lodash", Aliases: []string{"CVE-1"}, Summary: "proto pollution",
		Severity: "high", Score10: 75, Vector: "CVSS:3.1/x", Affected: `[{"versions":["1.0.0"]}]`, Published: t0, Modified: t0}
	b := a
	b.Package = "lodash-es"
	mal := VulnAdvisory{ID: "MAL-1", Ecosystem: "npm", Package: "evil", Severity: "unknown", Score10: -1, Malicious: true, Affected: "[]"}
	if err := db.ReplaceVulnAdvisories(ctx, []string{"GHSA-1", "MAL-1"}, []VulnAdvisory{a, b, mal}); err != nil {
		t.Fatal(err)
	}
	got, err := db.VulnAdvisoriesFor(ctx, "npm", "lodash")
	if err != nil || len(got) != 1 || got[0].ID != "GHSA-1" || got[0].Score10 != 75 || !slices.Equal(got[0].Aliases, []string{"CVE-1"}) ||
		!got[0].Modified.Equal(t0) || got[0].Malicious || !got[0].Withdrawn.IsZero() {
		t.Fatalf("for lodash: %+v %v", got, err)
	}
	if one, err := db.VulnAdvisory(ctx, "GHSA-1", "npm", "lodash-es"); err != nil || one.Package != "lodash-es" {
		t.Fatalf("by id: %+v %v", one, err)
	}
	if _, err := db.VulnAdvisory(ctx, "GHSA-9", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: %v", err)
	}
	by, malN, err := db.VulnStats(ctx)
	if err != nil || by["npm"] != 2 || malN != 1 {
		t.Fatalf("stats: %v %d %v", by, malN, err)
	}
	// A new version of GHSA-1 no longer affects lodash-es: its row goes.
	a.Withdrawn = t0
	if err := db.ReplaceVulnAdvisories(ctx, []string{"GHSA-1"}, []VulnAdvisory{a}); err != nil {
		t.Fatal(err)
	}
	var pkgs []string
	if err := db.WalkVulnPackages(ctx, func(e, p string) { pkgs = append(pkgs, e+"/"+p) }); err != nil {
		t.Fatal(err)
	}
	slices.Sort(pkgs)
	if !slices.Equal(pkgs, []string{"npm/evil", "npm/lodash"}) {
		t.Fatalf("packages: %v", pkgs)
	}
	if got, _ := db.VulnAdvisoriesFor(ctx, "npm", "lodash"); !got[0].Withdrawn.Equal(t0) {
		t.Fatalf("withdrawn: %+v", got)
	}

	// Online answers.
	if _, found, err := db.GetVulnQuery(ctx, "npm", "lodash", "1.0.0"); found || err != nil {
		t.Fatalf("no query yet: %v %v", found, err)
	}
	if err := db.PutVulnQueries(ctx, []VulnQuery{{Ecosystem: "npm", Package: "lodash", Version: "1.0.0", IDs: []string{"GHSA-1"}, Checked: t0}}); err != nil {
		t.Fatal(err)
	}
	if q, found, err := db.GetVulnQuery(ctx, "npm", "lodash", "1.0.0"); !found || err != nil || !slices.Equal(q.IDs, []string{"GHSA-1"}) || !q.Checked.Equal(t0) {
		t.Fatalf("query: %+v %v %v", q, found, err)
	}

	// Findings: counts, top, prune; a repository delete purges them.
	fs := []VulnFinding{
		{Repo: "r", Ecosystem: "npm", Package: "lodash", Version: "1.0.0", Name: "lodash@1.0.0", IDs: []string{"GHSA-1"}, Severity: "high", Rank: 3, Scanned: t0},
		{Repo: "r", Ecosystem: "npm", Package: "evil", Version: "1.0.0", Name: "evil@1.0.0", IDs: []string{"MAL-1"}, Severity: "unknown", Malicious: true, Scanned: t0},
		{Repo: "r", Ecosystem: "npm", Package: "ok", Version: "1.0.0", Name: "ok@1.0.0", Unchecked: true, Scanned: t0.Add(-time.Hour)},
	}
	if err := db.PutVulnFindings(ctx, fs); err != nil {
		t.Fatal(err)
	}
	c, err := db.VulnCountsByRepo(ctx, "")
	if err != nil || c["r"].Components != 3 || c["r"].Vulnerable != 2 || c["r"].Malicious != 1 || c["r"].High != 1 || c["r"].Other != 1 ||
		c["r"].Unchecked != 1 || !c["r"].Scanned.Equal(t0) {
		t.Fatalf("counts: %+v %v", c, err)
	}
	top, err := db.TopVulnFindings(ctx, "r", 10)
	if err != nil || len(top) != 2 || top[0].Name != "evil@1.0.0" || top[1].Name != "lodash@1.0.0" {
		t.Fatalf("top: %+v %v", top, err)
	}
	if err := db.PruneVulnFindings(ctx, "r", t0); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.VulnCountsByRepo(ctx, "r"); c["r"].Components != 2 {
		t.Fatalf("after prune: %+v", c)
	}

	// Quarantine.
	it := QuarantineItem{Repo: "r", Name: "evil", Version: "1.0.0", Ecosystem: "npm", Path: "evil/-/evil-1.0.0.tgz", IDs: []string{"MAL-1"}, Malicious: true, Last: t0}
	if created, err := db.Quarantine(ctx, it); !created || err != nil {
		t.Fatalf("first: %v %v", created, err)
	}
	it.Last = t0.Add(time.Minute)
	if created, err := db.Quarantine(ctx, it); created || err != nil {
		t.Fatalf("again: %v %v", created, err)
	}
	list, err := db.ListQuarantine(ctx, "r", 10)
	if err != nil || len(list) != 1 || list[0].Count != 2 || !list[0].First.Equal(t0) || !list[0].Last.Equal(t0.Add(time.Minute)) || !list[0].Malicious {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := db.CreateRepository(ctx, config.Repository{Name: "r", Format: "npm", Type: "proxy", Remote: "https://registry.example/"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteRepository(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.ListQuarantine(ctx, "", 10); len(list) != 0 {
		t.Fatalf("quarantine after the repository delete: %+v", list)
	}
	if c, _ := db.VulnCountsByRepo(ctx, ""); len(c) != 0 {
		t.Fatalf("findings after the repository delete: %+v", c)
	}
	if err := db.DeleteQuarantine(ctx, "r", "evil", "1.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}
