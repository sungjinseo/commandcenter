package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// storedRepos stores the harness's repositories, so the registry can update
// them, and lets it look up cleanup policies.
func (h *harness) storedRepos(t *testing.T) {
	t.Helper()
	defs := append([]config.Repository{}, h.defs...)
	for i := range defs {
		config.ApplyRepositoryDefaults(&defs[i])
	}
	if _, err := h.db.ImportRepositories(context.Background(), defs); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.reg.Policies = cleanup.PolicyLookup(h.db)
}

func (h *harness) setPolicies(t *testing.T, name string, policies ...string) {
	t.Helper()
	def := h.reg.Get(name).Repository
	def.SetCleanupPolicies(policies)
	if err := h.reg.Update(context.Background(), name, def, repo.ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
}

// Several policies on one repository: one run applies each, every policy
// deleting what it selects (core-14); each keeps its own scan cursor.
func TestCleanupSeveralPolicies(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.storedRepos(t)
	if err := cleanup.CreatePolicy(ctx, h.db, cleanup.Policy{Name: "keep1", Format: "maven", Criteria: cleanup.Criteria{Retain: 1}}); err != nil {
		t.Fatal(err)
	}
	h.setPolicies(t, "mvn", "old", "keep1", "old")
	rp := h.reg.Get("mvn")
	if got := strings.Join(rp.CleanupPolicyNames(), ","); got != "old,keep1" || rp.CleanupPolicy != "old" {
		t.Fatalf("stored policies %q (first %q)", got, rp.CleanupPolicy)
	}
	h.oldLibs(t, "1.0")                                     // old: componentAgeDays 30 deletes it
	h.put(t, "mvn", "org/example/app/1.0/app-1.0.jar", "a") // keep1 deletes it: 2.0 is newer
	h.put(t, "mvn", "org/example/app/2.0/app-2.0.jar", "b")
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultOK || out.Removed != 2 {
		t.Fatalf("run: %+v %v", out, err)
	}
	if got := h.mvnPaths(t, "org/example/"); got != "org/example/app/2.0/app-2.0.jar,org/example/app/maven-metadata.xml" {
		t.Fatalf("left: %s", got)
	}
	log := h.get(t, tk.ID).LastLog
	if !strings.Contains(log, `mvn: policy "old": deleted 1`) || !strings.Contains(log, `mvn: policy "keep1": deleted 1`) {
		t.Fatalf("log:\n%s", log)
	}
	if k := policyCursorKey(tk.ID, rp, "keep1"); k != cursorKey(tk.ID, "mvn")+"/keep1" || policyCursorKey(tk.ID, rp, "old") != cursorKey(tk.ID, "mvn") {
		t.Fatalf("cursor keys: %s", k)
	}
	// A policy in use as the second cannot be deleted either.
	var inUse *meta.PolicyInUseError
	if err := h.db.DeleteCleanupPolicy(ctx, "keep1"); !errors.As(err, &inUse) || strings.Join(inUse.Repos, ",") != "mvn" {
		t.Fatalf("delete keep1: %v", err)
	}
	// Unknown and mismatched policies are refused, each named.
	def := h.reg.Get("mvn").Repository
	def.SetCleanupPolicies([]string{"old", "ghost", "npm-only"})
	err = h.reg.Update(ctx, "mvn", def, repo.ClearSecrets{})
	var ve *repo.ValidationError
	if !errors.As(err, &ve) || len(ve.Errors) != 2 || !strings.Contains(ve.Errors[0], "ghost") || !strings.Contains(ve.Errors[1], "npm-only") {
		t.Fatalf("bad policies: %v", err)
	}
}

// The run limits are task params (core-15): maxComponents caps the
// components a run deletes, scanAssets the assets it reads per repository
// and policy, in scans of the default size each.
func TestCleanupRunLimits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, bad := range []string{`{"maxComponents":-1}`, `{"scanAssets":2000000000}`, `{"maxComponents":"x"}`} {
		if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(bad)}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	plain := h.create(t, Def{Name: "plain", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	if plain.Params != `{"repos":["mvn"]}` {
		t.Fatalf("params without limits changed form: %s", plain.Params)
	}
	// 1.0-1.5 are new, 1.6-1.9 old; a scan reads 4 assets.
	for i := 0; i < 6; i++ {
		h.put(t, "mvn", fmt.Sprintf("org/example/lib/1.%d/lib-1.%d.jar", i, i), "new")
	}
	h.oldLibs(t, "1.6", "1.7", "1.8", "1.9")
	h.s.ScanBudget = 4

	// Without scanAssets a run reads one scan: nothing old in the first 4.
	if out, _ := h.s.RunNow(plain.ID, "admin"); out.Removed != 0 {
		t.Fatalf("one scan: %+v", out)
	}
	// scanAssets 12 reads three scans in one run, to the end; maxComponents
	// 3 stops it after three deletes.
	tk := h.create(t, Def{Name: "big", Type: TypeCleanup, Schedule: "manual",
		Params: json.RawMessage(`{"repos":["mvn"],"scanAssets":12,"maxComponents":3}`)})
	if tk.Params != `{"repos":["mvn"],"maxComponents":3,"scanAssets":12}` {
		t.Fatalf("params: %s", tk.Params)
	}
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Removed != 3 {
		t.Fatalf("limited run: %+v %v", out, err)
	}
	if log := h.get(t, tk.ID).LastLog; !strings.Contains(log, "stopped at the limit of 3 components per run") {
		t.Fatalf("log:\n%s", log)
	}
	// The next run continues and deletes the last old one.
	if out, _ := h.s.RunNow(tk.ID, "admin"); out.Removed != 1 {
		t.Fatalf("second run: %+v", out)
	}
	if got := h.mvnPaths(t, "org/example/lib/1.9/"); got != "" {
		t.Fatalf("1.9 left: %s", got)
	}
}

// Age counts from when the content was last written (core-17): an asset
// uploaded 100 days ago and replaced today is not 30 days old.
func TestCleanupAgeFromLastWrite(t *testing.T) {
	h := newHarness(t)
	h.oldLibs(t, "1.0", "1.1")
	h.put(t, "mvn", "org/example/lib/1.0/lib-1.0.jar", "redeployed today")
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Removed != 1 {
		t.Fatalf("run: %+v %v", out, err)
	}
	if got := h.mvnPaths(t, "org/example/lib/"); !strings.Contains(got, "lib-1.0.jar") || strings.Contains(got, "lib-1.1.jar") {
		t.Fatalf("left: %s", got)
	}
}

// repository.purge-unused deletes from proxies the components no one
// downloaded in the last lastUsed days; never downloaded ones stay (core-42).
func TestPurgeUnused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, bad := range []string{`{}`, `{"lastUsed":0}`, `{"lastUsed":7,"repos":["mvn"]}`, `{"lastUsed":7,"repos":["nope"]}`} {
		if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypePurgeUnused, Schedule: "manual", Params: json.RawMessage(bad)}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	h.clock.Set(t0.AddDate(0, 0, -60))
	for _, v := range []string{"1.0", "1.1", "1.2"} {
		h.put(t, "mvn-proxy", "org/example/lib/"+v+"/lib-"+v+".jar", "jar")
	}
	// 1.0 was downloaded 60 days ago, 1.1 yesterday, 1.2 never.
	for v, at := range map[string]int{"1.0": -60, "1.1": -1} {
		if err := h.db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "mvn-proxy", Path: "org/example/lib/" + v + "/lib-" + v + ".jar", N: 1, Last: t0.AddDate(0, 0, at)}}); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.Set(t0)
	tk := h.create(t, Def{Name: "p", Type: TypePurgeUnused, Schedule: "manual", Params: json.RawMessage(`{"lastUsed":7}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultOK || out.Removed != 1 {
		t.Fatalf("run: %+v %v\n%s", out, err, h.get(t, tk.ID).LastLog)
	}
	as, _ := h.db.ListAssets(ctx, "mvn-proxy", "org/example/lib/")
	var left []string
	for _, a := range as {
		left = append(left, a.Path)
	}
	if got := strings.Join(left, ","); got != "org/example/lib/1.1/lib-1.1.jar,org/example/lib/1.2/lib-1.2.jar" {
		t.Fatalf("left: %s", got)
	}
}

// more gives the harness more repositories, created in order.
func (h *harness) more(t *testing.T, defs ...config.Repository) {
	t.Helper()
	for _, d := range defs {
		if d.Type == "proxy" {
			d.Remote = "https://remote.example/" + d.Name + "/"
		}
		if err := h.reg.Create(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
}

// paths lists what repoName holds.
func (h *harness) paths(t *testing.T, repoName string) string {
	t.Helper()
	as, err := h.db.ListAssets(context.Background(), repoName, "")
	if err != nil {
		t.Fatal(err)
	}
	left := []string{}
	for _, a := range as {
		left = append(left, a.Path)
	}
	return strings.Join(left, " ")
}

// downloaded records a download of repoName/path, at days from t0.
func (h *harness) downloaded(t *testing.T, repoName, path string, days int) {
	t.Helper()
	if err := h.db.FlushDownloads(context.Background(), []meta.DownloadDelta{{Repo: repoName, Path: path, N: 1, Last: t0.AddDate(0, 0, days)}}); err != nil {
		t.Fatal(err)
	}
}

// repository.purge-unused also deletes a proxy's cached files without a
// component that no one read and the proxy did not check in the last lastUsed
// days, as Nexus deletes assets without a component (core-42, cocoapods-09,
// golang-07, p2-10). What it must keep: a document that holds what its remote
// dropped, one that decides a group's owner with a member after it, what a
// blocked proxy cannot fetch back, and a proxy's own records.
func TestPurgeUnusedFiles(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.storedRepos(t)
	proxy := func(name, format string) config.Repository {
		return config.Repository{Name: name, Format: format, Type: "proxy"}
	}
	blocked := proxy("npm-blocked", "npm")
	blocked.Blocked = true
	h.more(t, proxy("mvn-first", "maven"),
		proxy("npm-solo", "npm"), proxy("npm-first", "npm"), proxy("npm-last", "npm"), proxy("npm-nested", "npm"), proxy("npm-outer", "npm"), blocked,
		proxy("py", "pypi"), proxy("p2-site", "p2"), proxy("hf", "huggingface"), proxy("tf", "terraform"), proxy("go-px", "go"),
		proxy("r-first", "r"), proxy("r-last", "r"), proxy("conda-first", "conda"), proxy("conda-last", "conda"),
		proxy("yum-first", "yum"), proxy("yum-last", "yum"), proxy("apt-first", "apt"), proxy("apt-last", "apt"),
		config.Repository{Name: "yum-all", Format: "yum", Type: "group", Members: []string{"yum-first", "yum-last"}},
		config.Repository{Name: "apt-all", Format: "apt", Type: "group", Members: []string{"apt-first", "apt-last"}},
		config.Repository{Name: "r-all", Format: "r", Type: "group", Members: []string{"r-first", "r-last"}},
		config.Repository{Name: "conda-all", Format: "conda", Type: "group", Members: []string{"conda-first", "conda-last"}},
		config.Repository{Name: "mvn-all", Format: "maven", Type: "group", Members: []string{"mvn-first", "mvn-proxy"}},
		config.Repository{Name: "npm-all", Format: "npm", Type: "group", Members: []string{"npm-first", "npm-last"}},
		config.Repository{Name: "npm-in", Format: "npm", Type: "group", Members: []string{"npm-nested"}},
		config.Repository{Name: "npm-all2", Format: "npm", Type: "group", Members: []string{"npm-in", "npm-outer"}})

	h.clock.Set(t0.AddDate(0, 0, -60))
	seed := map[string][]string{
		"mvn-proxy":   {"org/x/lib/1.0/lib-1.0.jar", "org/x/lib/maven-metadata.xml", "org/x/checked/maven-metadata.xml", "org/x/read/maven-metadata.xml", "org/x/invalid/maven-metadata.xml"},
		"mvn-first":   {"org/y/maven-metadata.xml"}, // a member follows it, but Maven's groups merge
		"npm-solo":    {"a/package.json", "b/-/b-1.0.0.tgz", "b/package.json", "c/-/c-1.0.0.tgz", "c/package.json", "@s/d/-/d-1.0.0.tgz", "@s/d/package.json"},
		"npm-first":   {"e/-/e-1.0.0.tgz", "e/package.json"},
		"npm-last":    {"f/package.json"},
		"npm-nested":  {"g/package.json"},
		"npm-outer":   {"h/package.json"},
		"npm-blocked": {"i/package.json"},
		"py":          {"packages/six/six-1.0.tar.gz", "pypi/six/json", "simple/six/index", "simple/gone/index"},
		"p2-site":     {".remote/index.json", "content.jar", "plugins/x_1.0.0.jar"},
		"hf":          {"api/models/o/m/tree/main~tree", "api/models/o/m/tree/main~tree~next"},
		"tf":          {"v1/modules/o/m/p/1.0.0/download", "v1/modules/o/m/p/versions"},
		"go-px":       {"example.com/m/@latest", "example.com/m/@v/list", "example.com/m/@v/v1.0.0.zip"},
		"r-first":     {"src/contrib/PACKAGES"},
		"r-last":      {"src/contrib/PACKAGES"},
		"conda-first": {"linux-64/repodata.json"},
		"conda-last":  {"linux-64/repodata.json"},
		"yum-first":   {"el9/repodata/repomd.xml"},
		"yum-last":    {"el9/repodata/repomd.xml"},
		"apt-first":   {"dists/stable/Release"},
		"apt-last":    {"dists/stable/Release"},
	}
	for name, paths := range seed {
		for _, p := range paths {
			h.put(t, name, p, name+"/"+p)
		}
	}
	h.clock.Set(t0)
	h.put(t, "mvn-proxy", "org/x/new/maven-metadata.xml", "written yesterday, as far as this run is concerned")
	if err := h.db.MarkChecked(ctx, "mvn-proxy", "org/x/checked/maven-metadata.xml", t0.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	// Invalidated: no check to go by, and written long ago.
	if err := h.db.MarkChecked(ctx, "mvn-proxy", "org/x/invalid/maven-metadata.xml", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	h.downloaded(t, "mvn-proxy", "org/x/read/maven-metadata.xml", -1)
	h.downloaded(t, "npm-solo", "c/-/c-1.0.0.tgz", -60) // goes, and so does its document, in one run
	h.downloaded(t, "npm-solo", "@s/d/-/d-1.0.0.tgz", -1)
	h.downloaded(t, "npm-first", "e/-/e-1.0.0.tgz", -60)
	h.downloaded(t, "py", "packages/six/six-1.0.tar.gz", -1)

	tk := h.create(t, Def{Name: "p", Type: TypePurgeUnused, Schedule: "manual", Params: json.RawMessage(`{"lastUsed":7}`)})
	h.s.ScanBudget = 3 // a run reads a part of each repository; the next ones go on from there
	for range 4 {
		if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultOK {
			t.Fatalf("run: %+v %v\n%s", out, err, h.get(t, tk.ID).LastLog)
		}
	}
	for name, want := range map[string]string{
		"mvn-proxy":   "org/x/checked/maven-metadata.xml org/x/lib/1.0/lib-1.0.jar org/x/new/maven-metadata.xml org/x/read/maven-metadata.xml",
		"mvn-first":   "",
		"npm-solo":    "@s/d/-/d-1.0.0.tgz @s/d/package.json b/-/b-1.0.0.tgz b/package.json",
		"npm-first":   "e/package.json", // the component goes as ever
		"npm-last":    "",               // no member follows it: nobody can stand in
		"npm-nested":  "g/package.json", // the outer group's next member follows it
		"npm-outer":   "",
		"npm-blocked": "i/package.json",
		"py":          "packages/six/six-1.0.tar.gz simple/six/index",
		"p2-site":     ".remote/index.json plugins/x_1.0.0.jar",
		"hf":          "api/models/o/m/tree/main~tree~next",
		"tf":          "v1/modules/o/m/p/1.0.0/download",
		"go-px":       "example.com/m/@v/v1.0.0.zip",
		"r-first":     "src/contrib/PACKAGES", // a first-member merge decides by these too
		"r-last":      "",
		"conda-first": "linux-64/repodata.json",
		"conda-last":  "",
		"yum-first":   "el9/repodata/repomd.xml", // their paths too
		"yum-last":    "",
		"apt-first":   "dists/stable/Release",
		"apt-last":    "",
	} {
		if got := h.paths(t, name); got != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}
	log := h.get(t, tk.ID).LastLog
	for _, want := range []string{"npm-first: keeps its cached files", "another member of group npm-all follows it", "npm-nested: keeps its cached files", "npm-blocked: keeps its cached files"} {
		if !strings.Contains(log, want) {
			t.Errorf("log without %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "npm-last: keeps") || strings.Contains(log, "mvn-first: keeps") {
		t.Errorf("log:\n%s", log)
	}
}

// A purge's files count against the run's limits: scanAssets bounds what a
// run reads and maxComponents what it deletes, and the next run goes on. Each
// batch is one transaction.
func TestPurgeUnusedFilesLimits(t *testing.T) {
	h := newHarness(t)
	h.storedRepos(t)
	saved := fileBatch
	fileBatch = 2
	t.Cleanup(func() { fileBatch = saved })
	var commits [][]meta.Change
	h.db.OnCommit = func(_ context.Context, _ time.Time, cs []meta.Change) { commits = append(commits, cs) }
	h.clock.Set(t0.AddDate(0, 0, -60))
	for i := range 10 {
		h.put(t, "mvn-proxy", fmt.Sprintf("org/x/l%d/maven-metadata.xml", i), "m")
	}
	h.clock.Set(t0)
	count := func() int {
		as, _ := h.db.ListAssets(context.Background(), "mvn-proxy", "")
		return len(as)
	}
	limited := h.create(t, Def{Name: "lim", Type: TypePurgeUnused, Schedule: "manual", Params: json.RawMessage(`{"lastUsed":7,"maxComponents":3}`)})
	commits = nil
	out, err := h.s.RunNow(limited.ID, "admin")
	if err != nil || out.Removed != 0 || count() != 7 {
		t.Fatalf("limited run: %+v %v, %d left\n%s", out, err, count(), h.get(t, limited.ID).LastLog)
	}
	if log := h.get(t, limited.ID).LastLog; !strings.Contains(log, "stopped at the limit of 3") || !strings.Contains(log, "3 of the assets are cached files") {
		t.Fatalf("log:\n%s", log)
	}
	if len(commits) != 2 || len(commits[0]) != 2 || len(commits[1]) != 1 || commits[0][0].Op != meta.ChangeDelete {
		t.Fatalf("commits: %+v", commits)
	}
	// The limited run did not move its cursor: this one, with a scan of 4, reads what is left from the start.
	scan := h.create(t, Def{Name: "scan", Type: TypePurgeUnused, Schedule: "manual", Params: json.RawMessage(`{"lastUsed":7,"scanAssets":4}`)})
	h.s.ScanBudget = 4
	if out, err := h.s.RunNow(scan.ID, "admin"); err != nil || count() != 3 {
		t.Fatalf("scan: %+v %v, %d left\n%s", out, err, count(), h.get(t, scan.ID).LastLog)
	}
	if out, err := h.s.RunNow(scan.ID, "admin"); err != nil || count() != 0 {
		t.Fatalf("scan: %+v %v, %d left\n%s", out, err, count(), h.get(t, scan.ID).LastLog)
	}
}

// A tarball or file fetched after a scan chose its document keeps the document:
// the check is in the delete too, under the lock every writer takes.
func TestDeleteFilesKeepsDocumentOfWhatWasFetchedSince(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.storedRepos(t)
	h.more(t, config.Repository{Name: "npm-p", Format: "npm", Type: "proxy"}, config.Repository{Name: "py-p", Format: "pypi", Type: "proxy"})
	h.clock.Set(t0.AddDate(0, 0, -60))
	for name, docs := range map[string][]string{"npm-p": {"a/package.json", "b/package.json", "@s/c/package.json"}, "py-p": {"simple/a/index", "simple/b/index"}} {
		for _, p := range docs {
			h.put(t, name, p, p)
		}
	}
	h.clock.Set(t0)
	h.put(t, "npm-p", "a/-/a-1.0.0.tgz", "fetched after the scan")
	h.put(t, "npm-p", "@s/c/-/c-1.0.0.tgz", "fetched after the scan")
	h.put(t, "py-p", "packages/a/a-1.0.tar.gz", "fetched after the scan")
	before := t0.AddDate(0, 0, -7)
	for name, c := range map[string]struct {
		docs []string
		left string
	}{
		"npm-p": {[]string{"a/package.json", "b/package.json", "@s/c/package.json"}, "@s/c/-/c-1.0.0.tgz @s/c/package.json a/-/a-1.0.0.tgz a/package.json"},
		"py-p":  {[]string{"simple/a/index", "simple/b/index"}, "packages/a/a-1.0.tar.gz simple/a/index"},
	} {
		res, err := h.s.Deleters.DeleteFiles(ctx, h.reg.Get(name), c.docs, before, "admin")
		if err != nil || res.Assets != 1 {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		if got := h.paths(t, name); got != c.left {
			t.Errorf("%s left %q, want %q", name, got, c.left)
		}
	}
}

// A file read or fetched after a scan chose it stays (the check is in the delete).
func TestDeleteFilesKeepsWhatWasUsedSince(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.storedRepos(t)
	h.clock.Set(t0.AddDate(0, 0, -60))
	for _, p := range []string{"a/maven-metadata.xml", "b/maven-metadata.xml", "c/maven-metadata.xml", "d/maven-metadata.xml"} {
		h.put(t, "mvn-proxy", p, p)
	}
	h.clock.Set(t0)
	h.downloaded(t, "mvn-proxy", "a/maven-metadata.xml", 0)
	if err := h.db.MarkChecked(ctx, "mvn-proxy", "b/maven-metadata.xml", t0); err != nil {
		t.Fatal(err)
	}
	h.put(t, "mvn-proxy", "c/maven-metadata.xml", "fetched again")
	before := t0.AddDate(0, 0, -7)
	res, err := h.s.Deleters.DeleteFiles(ctx, h.reg.Get("mvn-proxy"), []string{"a/maven-metadata.xml", "b/maven-metadata.xml", "c/maven-metadata.xml", "d/maven-metadata.xml", "none"}, before, "admin")
	if err != nil || res.Assets != 1 || res.Bytes != int64(len("d/maven-metadata.xml")) {
		t.Fatalf("%+v %v", res, err)
	}
	if got := h.paths(t, "mvn-proxy"); got != "a/maven-metadata.xml b/maven-metadata.xml c/maven-metadata.xml" {
		t.Fatalf("left: %s", got)
	}
}

// A snapshot build an import dated years back, not downloaded since it came:
// the purge counts from when it came, not from the date it brought.
func TestSnapBuildLastUsedOfAnImport(t *testing.T) {
	var b snapBuild
	b.paths = map[string]bool{}
	came, dated := t0.AddDate(0, 0, -2), t0.AddDate(-3, 0, 0)
	b.add(meta.ScanAsset{Path: "x", Created: dated, Modified: dated, Checked: came})
	if got := b.lastUsed(); !got.Equal(came) {
		t.Errorf("never downloaded: %v, want %v", got, came)
	}
	b.lastDl = t0.AddDate(0, 0, -1)
	if got := b.lastUsed(); !got.Equal(b.lastDl) {
		t.Errorf("downloaded: %v", got)
	}
}
