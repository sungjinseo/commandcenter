package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

func TestRunLogCap(t *testing.T) {
	now := func() time.Time { return t0 }
	check := func(l *RunLog) string {
		t.Helper()
		s := l.String()
		if n := strings.Count(s, "\n") + 1; n > MaxLogLines {
			t.Fatalf("%d lines", n)
		}
		if len(s) > MaxLogBytes {
			t.Fatalf("%d bytes", len(s))
		}
		return s
	}
	// Many short lines: the first 100 and the last 99, with a marker.
	h := &countHandler{}
	l := newRunLog(slog.New(h), now)
	for i := 0; i < 1000; i++ {
		l.Infof("line %d", i)
	}
	l.Errorf("failed: boom")
	s := check(l)
	lines := strings.Split(s, "\n")
	if len(lines) != MaxLogLines || !strings.HasSuffix(lines[0], " line 0") || !strings.HasSuffix(lines[99], " line 99") ||
		lines[100] != "… 802 lines omitted (see the server log) …" || !strings.HasSuffix(lines[101], " line 902") ||
		!strings.HasSuffix(lines[198], " line 999") || lines[199] != "2026-10-20T02:00:00Z ERROR failed: boom" {
		t.Fatalf("head and tail: %q ... %q", lines[:2], lines[98:])
	}
	if h.n.Load() != 1001 {
		t.Fatalf("slog got %d records, want every line", h.n.Load())
	}
	// Long lines: the byte cap; each line is cut at maxLineLen, and the
	// last line is still kept.
	l = newRunLog(nil, now)
	for i := 0; i < 100; i++ {
		l.Warnf("%d %s", i, strings.Repeat("é", 5000))
	}
	l.Infof("finished: OK")
	s = check(l)
	if !strings.Contains(s, "lines omitted") || !strings.Contains(s, " WARN 0 ") || !strings.HasSuffix(s, "INFO finished: OK") {
		t.Fatalf("byte cap: %d lines", strings.Count(s, "\n"))
	}
	// Small logs are kept whole.
	l = newRunLog(nil, now)
	l.Infof("a\nb")
	if s := l.String(); s != "2026-10-20T02:00:00Z INFO a b" {
		t.Fatalf("small: %q", s)
	}
}

// URLs in run logs, messages and event details lose credentials and query values.
func TestRedaction(t *testing.T) {
	h := newHarness(t)
	h.s.GC = func(context.Context, string) (int, int64, error) {
		return 0, 0, errors.New("fetch https://user:hunter2@repo.example.org/x?token=abc123 failed")
	}
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual"})
	out, _ := h.s.RunNow(tk.ID, "admin")
	got := h.get(t, tk.ID)
	for what, s := range map[string]string{"outcome": out.Message, "lastMessage": got.LastMessage, "lastLog": got.LastLog,
		"event": h.sink.Events()[0].Detail} {
		if strings.Contains(s, "hunter2") || strings.Contains(s, "abc123") || !strings.Contains(s, "https://repo.example.org/x?token=REDACTED") {
			t.Errorf("%s not redacted: %s", what, s)
		}
	}
}

// Run now starts in the background, shows Running at once, and leaves the
// scheduled next run alone.
func TestRunAsync(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.s.GC = func(context.Context, string) (int, int64, error) { <-release; return 1, 0, nil }
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "daily:03:00"})
	if err := h.s.RunAsync(tk.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if h.s.Running() != tk.ID {
		t.Fatal("not running after RunAsync returned")
	}
	var busy *BusyError
	if err := h.s.RunAsync(tk.ID, "admin"); !errors.As(err, &busy) || busy.Running != tk.ID {
		t.Fatalf("second RunAsync: %v", err)
	}
	close(release)
	h.s.Wait()
	got := h.get(t, tk.ID)
	if got.LastResult != ResultOK || got.LastRemoved != 1 || !got.NextRun.Equal(tk.NextRun) || h.s.Running() != "" {
		t.Fatalf("after run: %+v (next was %s)", got, tk.NextRun)
	}
	if err := h.s.RunAsync("nope", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if err := h.s.RunAsync(tk.ID, "admin"); err != nil { // the lock was released
		t.Fatal(err)
	}
	h.s.Wait()
}

// An edit during a scheduled run is not overwritten by the run's record.
func TestEditDuringRunKept(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "daily:03:00"})
	h.clock.Set(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC))
	h.s.GC = func(context.Context, string) (int, int64, error) {
		if _, err := h.s.Update(ctx, tk.ID, Def{Schedule: "daily:23:00"}); err != nil {
			t.Error(err)
		}
		h.clock.Set(time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC))
		return 0, 0, nil
	}
	h.s.RunDue(ctx)
	got := h.get(t, tk.ID)
	if got.LastResult != ResultOK || got.Schedule != "daily:23:00" || !got.NextRun.Equal(time.Date(2026, 10, 20, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("after run: %+v", got)
	}
	// Disabled during the run: no next run afterwards.
	h.clock.Set(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	off := false
	h.s.GC = func(context.Context, string) (int, int64, error) {
		if _, err := h.s.Update(ctx, tk.ID, Def{Enabled: &off}); err != nil {
			t.Error(err)
		}
		return 0, 0, nil
	}
	h.s.RunDue(ctx)
	if got := h.get(t, tk.ID); got.Enabled || !got.NextRun.IsZero() || got.LastRun.Before(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("disabled during run: %+v", got)
	}
}

// Stop cancels a scheduled run in flight and waits for it; a run that
// ignores the cancel is waited for stopWait only.
func TestStopWithScheduledRun(t *testing.T) {
	h := newHarness(t)
	ticks := make(chan time.Time)
	h.s.Ticks = ticks
	entered := make(chan struct{}, 1)
	h.s.GC = func(ctx context.Context, _ string) (int, int64, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "daily:03:00"})
	h.clock.Set(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC))
	h.s.Start(context.Background())
	ticks <- time.Time{}
	<-entered
	h.s.Stop()
	if got := h.get(t, tk.ID); got.LastResult != ResultFailed || !strings.Contains(got.LastMessage, "shutdown") {
		t.Fatalf("after stop: %+v", got)
	}

	old := stopWait
	stopWait = 50 * time.Millisecond
	t.Cleanup(func() { stopWait = old })
	h2 := newHarness(t)
	ticks2 := make(chan time.Time)
	h2.s.Ticks = ticks2
	release := make(chan struct{})
	h2.s.GC = func(context.Context, string) (int, int64, error) { entered <- struct{}{}; <-release; return 0, 0, nil } // ignores ctx
	h2.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "daily:03:00"})
	h2.clock.Set(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC))
	h2.s.Start(context.Background())
	ticks2 <- time.Time{}
	<-entered
	start := time.Now()
	h2.s.Stop()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Stop waited %s", d)
	}
	close(release)
	h2.s.Wait()
}

// deleterFunc adapts a function to cleanup.ComponentDeleter.
type deleterFunc func(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (cleanup.Result, error)

func (f deleterFunc) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (cleanup.Result, error) {
	return f(ctx, rp, c, actor)
}

func (h *harness) oldLibs(t *testing.T, versions ...string) {
	t.Helper()
	h.clock.Set(t0.AddDate(0, 0, -60))
	for _, v := range versions {
		h.put(t, "mvn", "org/example/lib/"+v+"/lib-"+v+".jar", "jar-"+v)
	}
	h.clock.Set(t0)
}

func TestCleanupCancelledMidRun(t *testing.T) {
	h := newHarness(t)
	h.oldLibs(t, "1.0", "1.1", "1.2")
	ctx, cancel := context.WithCancel(context.Background())
	h.s.Ticks = make(chan time.Time)
	h.s.Start(ctx)
	defer h.s.Stop()
	deleters := cleanup.NewRegistry(h.store)
	deleters.Register("maven", "hosted", deleterFunc(func(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (cleanup.Result, error) {
		res, err := h.mvn.DeleteComponent(ctx, rp, c, actor)
		cancel() // the server shuts down after the first delete
		return res, err
	}))
	h.s.Deleters = deleters
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultFailed || out.Removed != 1 || out.Bytes != int64(len("jar-1.0")) || !strings.Contains(out.Message, "shutdown") {
		t.Fatalf("cancelled: %+v %v", out, err)
	}
	if got := h.get(t, tk.ID); got.LastResult != ResultFailed || got.LastRemoved != 1 || got.LastBytes != out.Bytes {
		t.Fatalf("recorded: %+v", got)
	}
	if as, _ := h.db.ListAssets(context.Background(), "mvn", "org/example/lib/1.1/"); len(as) != 1 {
		t.Fatal("deleted past the cancel")
	}
}

// A failed delete does not use up the per-run limit; a format mismatch is
// skipped with a warning, not counted as a failure.
func TestCleanupFailedDeleteNotCounted(t *testing.T) {
	h := newHarness(t)
	h.oldLibs(t, "1.0", "1.1")
	deleters := cleanup.NewRegistry(h.store)
	deleters.Register("maven", "hosted", deleterFunc(func(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (cleanup.Result, error) {
		if c.Version == "1.0" {
			return cleanup.Result{}, errors.New("disk on fire")
		}
		return h.mvn.DeleteComponent(ctx, rp, c, actor)
	}))
	h.s.Deleters = deleters
	h.s.MaxComponents = 1
	tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	out, _ := h.s.RunNow(tk.ID, "admin")
	if out.Result != ResultFailed || out.Removed != 1 || !strings.Contains(out.Message, "1 cleanup steps failed") {
		t.Fatalf("outcome: %+v", out)
	}
	if as, _ := h.db.ListAssets(context.Background(), "mvn", "org/example/lib/1.1/"); len(as) != 0 {
		t.Fatal("1.1 was not deleted after the failed 1.0")
	}

	mismatch := h.create(t, Def{Name: "m", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn-npm-policy"]}`)})
	out, _ = h.s.RunNow(mismatch.ID, "admin")
	if got := h.get(t, mismatch.ID); out.Result != ResultOK || !strings.Contains(got.LastLog, `WARN mvn-npm-policy: skipped: cleanup policy "npm-only" is for npm repositories`) {
		t.Fatalf("mismatch: %+v\n%s", out, got.LastLog)
	}
}

// Stale values can be kept: a passed once time, a deleted repository.
func TestUpdateKeepsStaleValues(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	once := h.create(t, Def{Name: "o", Type: TypeCompact, Schedule: "once:2026-10-20T05:00:00Z"})
	h.clock.Set(t0.Add(24 * time.Hour))
	off := false
	if up, err := h.s.Update(ctx, once.ID, Def{Enabled: &off}); err != nil || up.Enabled || up.Schedule != once.Schedule {
		t.Fatalf("disable a passed once: %+v %v", up, err)
	}
	if up, err := h.s.Update(ctx, once.ID, Def{Name: "renamed", Schedule: "once:2026-10-20T05:00:00Z"}); err != nil || up.Name != "renamed" {
		t.Fatalf("rename with the same passed once: %+v %v", up, err)
	}
	if _, err := h.s.Update(ctx, once.ID, Def{Schedule: "once:2026-10-20T06:00:00Z"}); err == nil {
		t.Fatal("a new passed once time accepted")
	}

	clean := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn-none"]}`)})
	rebuild := h.create(t, Def{Name: "r", Type: TypeRebuild, Schedule: "manual", Params: json.RawMessage(`{"repo":"mvn-none"}`)})
	var defs []config.Repository // mvn-none is deleted
	for _, d := range h.defs {
		if d.Name != "mvn-none" {
			defs = append(defs, d)
		}
	}
	h.s.Repos = repo.NewRegistry(h.db, defs)
	if up, err := h.s.Update(ctx, clean.ID, Def{Name: "c2", Params: json.RawMessage(`{"repos":["mvn-none"]}`)}); err != nil || up.Name != "c2" {
		t.Fatalf("rename keeping a deleted repo: %+v %v", up, err)
	}
	if _, err := h.s.Update(ctx, clean.ID, Def{Params: json.RawMessage(`{"repos":["mvn-none","mvn"]}`)}); err == nil {
		t.Fatal("changed params with a deleted repo accepted")
	}
	if _, err := h.s.Update(ctx, rebuild.ID, Def{Enabled: &off}); err != nil {
		t.Fatalf("disable a rebuild of a deleted repo: %v", err)
	}
	out, _ := h.s.RunNow(rebuild.ID, "admin")
	if out.Result != ResultFailed || out.Message != `repository "mvn-none" no longer exists: edit or delete this task` {
		t.Fatalf("rebuild of a deleted repo: %+v", out)
	}
}
