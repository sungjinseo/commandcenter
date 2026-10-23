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
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/metrics"
	"commandcenter/internal/repo"
)

// wrapMaven makes the maven hosted deleter run before(c) first.
func (h *harness) wrapMaven(before func(c component.Component)) {
	deleters := cleanup.NewRegistry(h.store)
	deleters.Register("maven", "hosted", deleterFunc(func(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (cleanup.Result, error) {
		before(c)
		return h.mvn.DeleteComponent(ctx, rp, c, actor)
	}))
	h.s.Deleters = deleters
}

func (h *harness) mvnPaths(t *testing.T, prefix string) string {
	t.Helper()
	as, err := h.db.ListAssets(context.Background(), "mvn", prefix)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range as {
		out = append(out, a.Path)
	}
	return strings.Join(out, ",")
}

// A SNAPSHOT build deployed between the evaluation and the delete makes the
// component newer than the policy allows: it is kept, and the run is OK.
func TestCleanupKeepsComponentChangedAfterEvaluation(t *testing.T) {
	h := newHarness(t)
	const dir = "org/example/snap/1.0-SNAPSHOT/"
	h.clock.Set(t0.AddDate(0, 0, -60))
	h.put(t, "mvn", dir+"snap-1.0-20260801.000000-1.jar", "build-1")
	h.clock.Set(t0)
	h.wrapMaven(func(component.Component) {
		h.put(t, "mvn", dir+"snap-1.0-20261020.020000-2.jar", "build-2")
	})
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultOK || out.Removed != 0 {
		t.Fatalf("outcome: %+v %v", out, err)
	}
	if got := h.mvnPaths(t, dir); got != dir+"snap-1.0-20260801.000000-1.jar,"+dir+"snap-1.0-20261020.020000-2.jar" {
		t.Fatalf("assets left: %s", got)
	}
	if got := h.get(t, tk.ID); !strings.Contains(got.LastLog, "changed since evaluation") || !strings.Contains(got.LastLog, "1 changed since evaluation") {
		t.Fatalf("log:\n%s", got.LastLog)
	}
}

// A download recorded between the evaluation and the delete keeps the
// component too.
func TestCleanupKeepsComponentDownloadedAfterEvaluation(t *testing.T) {
	h := newHarness(t)
	h.oldLibs(t, "1.0")
	if err := cleanup.UpdatePolicy(context.Background(), h.db, cleanup.Policy{Name: "old", Format: "maven",
		Criteria: cleanup.Criteria{LastDownloadedDays: 30}}); err != nil {
		t.Fatal(err)
	}
	h.wrapMaven(func(component.Component) {
		if err := h.db.FlushDownloads(context.Background(), []meta.DownloadDelta{{Repo: "mvn", Path: "org/example/lib/1.0/lib-1.0.jar", N: 1, Last: t0}}); err != nil {
			t.Error(err)
		}
	})
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultOK || out.Removed != 0 {
		t.Fatalf("outcome: %+v %v", out, err)
	}
	if got := h.mvnPaths(t, "org/example/lib/"); got != "org/example/lib/1.0/lib-1.0.jar" {
		t.Fatalf("assets left: %s", got)
	}
}

// Downloads still pending in the metrics recorder are flushed before the
// evaluation, so they protect the component.
func TestCleanupFlushesPendingDownloads(t *testing.T) {
	h := newHarness(t)
	h.oldLibs(t, "1.0", "1.1")
	if err := cleanup.UpdatePolicy(context.Background(), h.db, cleanup.Policy{Name: "old", Format: "maven",
		Criteria: cleanup.Criteria{LastDownloadedDays: 30}}); err != nil {
		t.Fatal(err)
	}
	rec := metrics.New(h.db)
	rec.Now = h.clock.Now
	rec.Download("mvn", "org/example/lib/1.0/lib-1.0.jar") // pending, not flushed
	h.s.Metrics = rec
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultOK || out.Removed != 1 {
		t.Fatalf("outcome: %+v %v", out, err)
	}
	if got := h.mvnPaths(t, "org/example/lib/1."); got != "org/example/lib/1.0/lib-1.0.jar" {
		t.Fatalf("assets left: %s", got)
	}
}

// Unassigning the policy during a run stops deleting in that repository.
func TestCleanupStopsWhenPolicyUnassigned(t *testing.T) {
	h := newHarness(t)
	defs := append([]config.Repository{}, h.defs...)
	for i := range defs {
		config.ApplyRepositoryDefaults(&defs[i])
	}
	if _, err := h.db.ImportRepositories(context.Background(), defs); err != nil { // so the registry can update them
		t.Fatal(err)
	}
	h.oldLibs(t, "1.0", "1.1", "1.2")
	calls := 0
	h.wrapMaven(func(component.Component) {
		calls++
		if calls != 1 {
			return
		}
		def := h.reg.Get("mvn").Repository
		def.CleanupPolicy = ""
		if err := h.reg.Update(context.Background(), "mvn", def, repo.ClearSecrets{}); err != nil {
			t.Error(err)
		}
	})
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Removed != 1 || calls != 1 {
		t.Fatalf("outcome: %+v %v (%d deletes)", out, err, calls)
	}
	if got := h.get(t, tk.ID); !strings.Contains(got.LastLog, "changed during the run") {
		t.Fatalf("log:\n%s", got.LastLog)
	}
}

// Editing the policy during a run stops deleting in the repositories that
// use it.
func TestCleanupStopsWhenPolicyEdited(t *testing.T) {
	h := newHarness(t)
	h.oldLibs(t, "1.0", "1.1", "1.2")
	calls := 0
	h.wrapMaven(func(component.Component) {
		calls++
		if calls != 1 {
			return
		}
		if err := cleanup.UpdatePolicy(context.Background(), h.db, cleanup.Policy{Name: "old", Format: "maven",
			Criteria: cleanup.Criteria{ComponentAgeDays: 90}}); err != nil {
			t.Error(err)
		}
		h.s.PolicyChanged()
	})
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Removed != 1 || calls != 1 {
		t.Fatalf("outcome: %+v %v (%d deletes)", out, err, calls)
	}
	if got := h.get(t, tk.ID); !strings.Contains(got.LastLog, `cleanup policy "old" changed`) {
		t.Fatalf("log:\n%s", got.LastLog)
	}
}

// StopRun cancels the running task, which records Stopped; a task that is
// not running cannot be stopped.
func TestStopRun(t *testing.T) {
	h := newHarness(t)
	entered := make(chan struct{})
	h.s.GC = func(ctx context.Context, _ string) (int, error) { close(entered); <-ctx.Done(); return 2, ctx.Err() }
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual"})
	if h.s.StopRun(tk.ID) {
		t.Fatal("stopped a task that is not running")
	}
	if err := h.s.RunAsync(tk.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if h.s.StopRun("other") {
		t.Fatal("stopped another task")
	}
	if !h.s.StopRun(tk.ID) {
		t.Fatal("StopRun refused the running task")
	}
	h.s.Wait()
	got := h.get(t, tk.ID)
	if got.LastResult != ResultStopped || !strings.Contains(got.LastMessage, "stopped") || h.s.Running() != "" {
		t.Fatalf("after stop: %+v", got)
	}
	// A cleanup run stops between components.
	h.oldLibs(t, "1.0", "1.1", "1.2")
	calls := 0
	deleters := cleanup.NewRegistry(h.store)
	deleters.Register("maven", "hosted", deleterFunc(func(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (cleanup.Result, error) {
		calls++
		res, err := h.mvn.DeleteComponent(ctx, rp, c, actor)
		h.s.StopRun(h.s.Running()) // stopped while the first delete finishes
		return res, err
	}))
	h.s.Deleters = deleters
	cl := h.create(t, Def{Name: "cl", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	out, _ := h.s.RunNow(cl.ID, "admin")
	if out.Result != ResultStopped || out.Removed != 1 || calls != 1 {
		t.Fatalf("cleanup stopped: %+v (%d deletes)", out, calls)
	}
}

// The scan budget advances: each run starts where the previous one stopped
// and wraps around at the end, so successive runs cover the repository.
func TestCleanupScanResumes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 1.0-1.7 are new, 1.8 and 1.9 old: a scan that always started at the
	// beginning would never reach them.
	for i := 0; i < 8; i++ {
		h.put(t, "mvn", fmt.Sprintf("org/example/lib/1.%d/lib-1.%d.jar", i, i), "new")
	}
	h.oldLibs(t, "1.8", "1.9")
	h.s.ScanBudget = 4
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	var removed []int64
	for i := 0; i < 3; i++ {
		out, err := h.s.RunNow(tk.ID, "admin")
		if err != nil || out.Result != ResultOK {
			t.Fatalf("run %d: %+v %v", i, out, err)
		}
		removed = append(removed, out.Removed)
	}
	if fmt.Sprint(removed) != "[0 0 2]" {
		t.Fatalf("removed per run: %v", removed)
	}
	if got := h.get(t, tk.ID); !strings.Contains(got.LastLog, "the next run starts from the beginning") {
		t.Fatalf("log of the third run:\n%s", got.LastLog)
	}
	// The fourth run wraps around to the start.
	h.clock.Set(t0.AddDate(0, 0, 60)) // everything is old now
	out, _ := h.s.RunNow(tk.ID, "admin")
	if out.Removed != 4 {
		t.Fatalf("wrapped run: %+v", out)
	}
	if got := h.mvnPaths(t, "org/example/lib/1.0/"); got != "" {
		t.Fatalf("1.0 left after the wrap: %s", got)
	}
	// Deleting the task drops its cursors.
	if err := h.s.Delete(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.GetSetting(ctx, cursorKey(tk.ID, "mvn")); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("cursor kept after delete: %v", err)
	}
}

// A run whose record cannot be stored does not run again on every tick: the
// next run moves on in memory.
func TestRecordFailureAdvancesNextRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "hourly:00"})
	h.s.recordRun = func(context.Context, string, meta.TaskRun, func(meta.Task) (time.Time, bool)) error {
		return errors.New("disk full")
	}
	h.clock.Set(tk.NextRun.Add(time.Minute))
	h.s.RunDue(ctx)
	h.clock.Set(tk.NextRun.Add(2 * time.Minute))
	h.s.RunDue(ctx)
	if n := h.gcs.Load(); n != 1 {
		t.Fatalf("ran %d times before the next hour", n)
	}
	h.clock.Set(tk.NextRun.Add(time.Hour + time.Minute))
	h.s.RunDue(ctx)
	if n := h.gcs.Load(); n != 2 {
		t.Fatalf("ran %d times after the next hour", n)
	}
}

// QA L-3: deleting the seeded compaction task would silently stop compaction.
func TestDeleteSeedTaskRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Delete(ctx, SeedID); !errors.Is(err, ErrSeedTask) {
		t.Fatalf("delete seed: %v", err)
	}
	if _, err := h.db.GetTask(ctx, SeedID); err != nil {
		t.Fatalf("seed task gone: %v", err)
	}
}
