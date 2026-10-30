package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

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
