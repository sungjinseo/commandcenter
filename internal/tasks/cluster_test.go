package tasks

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

// twoNodes is node A, the harness, and node B, a Service on a second handle
// of its database, both joined as nodes (two servers on one database, as
// high availability runs them); A joins
// first and holds the scheduler lease, and both are ready (listening).
func twoNodes(t *testing.T) (h *harness, b *Service, memberA *meta.Member) {
	t.Helper()
	h = newHarness(t)
	dbB, err := meta.Open(metatest.URL(t, h.dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbB.Close() })
	log := slog.New(slog.DiscardHandler)
	if memberA, err = h.db.Join(context.Background(), "node-a", "host-a", "test", log); err != nil {
		t.Fatal(err)
	}
	if _, err := dbB.Join(context.Background(), "node-b", "host-b", "test", log); err != nil {
		t.Fatal(err)
	}
	// Both listen for the other's notifications (a stop) before a test sends one.
	for _, d := range []*meta.DB{h.db, dbB} {
		for deadline := time.Now().Add(5 * time.Second); d.Ready(context.Background()) != nil; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("not ready: %v", d.Ready(context.Background()))
			}
		}
	}
	b = &Service{DB: dbB, Repos: h.reg, Store: h.store, Log: log, Now: h.clock.Now, Loc: time.UTC}
	return h, b, memberA
}

// tick sends a tick to a scheduler loop twice: the second send waits for
// the RunDue of the first.
func tick(ch chan time.Time) {
	ch <- time.Time{}
	ch <- time.Time{}
}

// Both nodes tick at once: the leader runs the due task once; B, even when
// it runs it directly meanwhile, is refused by the run lock, and after the
// run it reads the next run the record stored.
func TestTwoNodesTickOnce(t *testing.T) {
	h, b, _ := twoNodes(t)
	var runs atomic.Int32
	entered, release := make(chan string, 2), make(chan struct{})
	gc := func(node string) func(context.Context, string) (int, int64, error) {
		return func(context.Context, string) (int, int64, error) {
			runs.Add(1)
			entered <- node
			<-release
			return 1, 0, nil
		}
	}
	h.s.GC, b.GC = gc("a"), gc("b")
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "daily:03:00"})
	h.clock.Set(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC))
	ta, tb := make(chan time.Time), make(chan time.Time)
	h.s.Ticks, b.Ticks = ta, tb
	h.s.Start(context.Background())
	defer h.s.Stop()
	b.Start(context.Background())
	defer b.Stop()
	go func() { ta <- time.Time{} }()
	tb <- time.Time{}
	if node := <-entered; node != "a" {
		t.Fatalf("node %s ran the due task, want the leader a", node)
	}
	tick(tb)
	var busy *BusyError
	if _, err := b.run(tk.ID, "system", recordScheduled); !errors.As(err, &busy) || busy.Running != tk.ID {
		t.Fatalf("B's run during A's: %v, want busy with %s", err, tk.ID)
	}
	close(release)
	h.s.Wait()
	tick(ta)
	tick(tb)
	if _, err := b.run(tk.ID, "system", recordScheduled); err != nil {
		t.Fatal(err)
	}
	got := h.get(t, tk.ID)
	if n := runs.Load(); n != 1 || got.LastResult != ResultOK || !got.NextRun.Equal(time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("%d runs; task %+v", n, got)
	}
}

// Run now on A while B runs a task: refused, naming B's task, which A also
// reports as running (and will not delete) and GET /-/api/nodes shows on B.
func TestRunNowBusyAcrossNodes(t *testing.T) {
	h, b, _ := twoNodes(t)
	ctx := context.Background()
	release := make(chan struct{})
	b.GC = func(context.Context, string) (int, int64, error) { <-release; return 1, 0, nil }
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual"})
	other := h.create(t, Def{Name: "d", Type: TypeCompact, Schedule: "manual"})
	if err := b.RunAsync(tk.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	var busy *BusyError
	if err := h.s.RunAsync(other.ID, "admin"); !errors.As(err, &busy) || busy.Running != tk.ID {
		t.Fatalf("run now on A: %v, want busy with %s", err, tk.ID)
	}
	if _, err := h.s.RunCompact("admin"); !errors.As(err, &busy) {
		t.Fatalf("compact on A: %v", err)
	}
	if err := h.s.Delete(ctx, tk.ID); !errors.As(err, &busy) {
		t.Fatalf("delete on A of B's running task: %v", err)
	}
	if r := h.s.Running(); r != tk.ID {
		t.Fatalf("A reports %q running", r)
	}
	running := func() map[string]string {
		t.Helper()
		nodes, err := h.db.Nodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, n := range nodes {
			out[n.ID] = n.RunningTask
		}
		return out
	}
	if r := running(); r["node-a"] != "" || r["node-b"] != tk.ID {
		t.Fatalf("nodes' running tasks: %v", r)
	}
	close(release)
	b.Wait()
	if r, n := h.s.Running(), running(); r != "" || n["node-b"] != "" {
		t.Fatalf("after the run: A reports %q, nodes %v", r, n)
	}
	if err := h.s.RunAsync(other.ID, "admin"); err != nil {
		t.Fatalf("after B's run: %v", err)
	}
	h.s.Wait()
}

// StopRun on A stops the run on B, which records it as stopped.
func TestStopRunOnOtherNode(t *testing.T) {
	h, b, _ := twoNodes(t)
	entered := make(chan struct{})
	b.GC = func(ctx context.Context, _ string) (int, int64, error) {
		close(entered)
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	b.Ticks = make(chan time.Time) // never ticks: B answers the other nodes
	b.Start(context.Background())
	defer b.Stop()
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual"})
	other := h.create(t, Def{Name: "d", Type: TypeCompact, Schedule: "manual"})
	if err := b.RunAsync(tk.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if h.s.StopRun(other.ID) {
		t.Fatal("A stopped a task that is not running")
	}
	if !h.s.StopRun(tk.ID) {
		t.Fatal("A did not stop B's run")
	}
	waitIdle(t, b)
	if got := h.get(t, tk.ID); got.LastResult != ResultStopped {
		t.Fatalf("after the stop: %+v", got)
	}
}

// A node whose run is cancelled and never recorded (it died) releases the
// run lock with its registration; its stale mark is ignored, and the next
// leader runs the task again, as it is still due.
func TestRerunAfterNodeDies(t *testing.T) {
	hb, ttl := meta.HeartbeatEvery, meta.NodeTTL
	meta.HeartbeatEvery, meta.NodeTTL = 50*time.Millisecond, time.Second
	t.Cleanup(func() { meta.HeartbeatEvery, meta.NodeTTL = hb, ttl }) // after the handles closed
	h, b, memberA := twoNodes(t)
	ctx := context.Background()
	entered := make(chan struct{}, 1)
	h.s.GC = func(ctx context.Context, _ string) (int, int64, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	h.s.recordRun = func(context.Context, string, meta.TaskRun, func(meta.Task) (time.Time, bool)) error {
		return errors.New("the node died before it recorded the run")
	}
	var runsB atomic.Int32
	b.GC = func(context.Context, string) (int, int64, error) { runsB.Add(1); return 1, 0, nil }
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "daily:03:00"})
	h.clock.Set(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC))
	ta, tb := make(chan time.Time), make(chan time.Time)
	h.s.Ticks, b.Ticks = ta, tb
	h.s.Start(ctx)
	b.Start(ctx)
	defer b.Stop()
	ta <- time.Time{}
	<-entered
	h.s.Stop()
	memberA.Leave(ctx)
	if err := h.db.SetTaskRunning(ctx, tk.ID, "node-a", t0); err != nil { // the mark a crash leaves
		t.Fatal(err)
	}
	if r := b.Running(); r != "" {
		t.Fatalf("B reports %q running on a node that is gone", r)
	}
	for deadline := time.Now().Add(5 * time.Second); !b.DB.Leader(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("B did not take the scheduler lease")
		}
	}
	tick(tb)
	got := h.get(t, tk.ID)
	if n := runsB.Load(); n != 1 || got.LastResult != ResultOK || !got.NextRun.Equal(time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("B ran it %d times; task %+v", n, got)
	}
}

// Run now on A while B runs another task: queued, not refused (core-31).
// The queued run waits for B's, then the leader A starts it at a tick as
// the admin who asked, records it as a run now, and drops the request; a
// second request meanwhile keeps the first, and run now of B's own task
// is still refused.
func TestRunNowQueuedAcrossNodes(t *testing.T) {
	h, b, _ := twoNodes(t)
	release := make(chan struct{})
	b.GC = func(context.Context, string) (int, int64, error) { <-release; return 1, 0, nil }
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual"})
	other := h.create(t, Def{Name: "d", Type: TypeCompact, Schedule: "daily:03:00"})
	if err := b.RunAsync(tk.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	var busy *BusyError
	if _, err := h.s.RunOrQueue(tk.ID, "alice"); !errors.As(err, &busy) || busy.Running != tk.ID {
		t.Fatalf("run now of the running task: %v", err)
	}
	for _, actor := range []string{"alice", "bob"} {
		if queued, err := h.s.RunOrQueue(other.ID, actor); err != nil || !queued {
			t.Fatalf("run now of another task by %s: queued %v, %v", actor, queued, err)
		}
	}
	if _, err := h.s.RunOrQueue("missing", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	if got := h.get(t, other.ID); got.QueuedBy != "alice" || got.QueuedAt.IsZero() {
		t.Fatalf("queued: %+v", got)
	}
	ta := make(chan time.Time)
	h.s.Ticks = ta
	h.s.Start(context.Background())
	defer h.s.Stop()
	tick(ta) // B still runs: the queued run waits
	if n := h.gcs.Load(); n != 0 {
		t.Fatalf("ran beside B's run: %d", n)
	}
	close(release)
	b.Wait()
	tick(ta)
	h.s.Wait()
	got := h.get(t, other.ID)
	if n := h.gcs.Load(); n != 1 || got.LastResult != ResultOK || !got.QueuedAt.IsZero() || got.QueuedBy != "" {
		t.Fatalf("%d runs; task %+v", n, got)
	}
	if want := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC); !got.NextRun.Equal(want) {
		t.Fatalf("the queued run moved the next run to %s", got.NextRun)
	}
	var actor string
	for _, e := range h.sink.Events() {
		if e.Kind == "task.run" && e.Subject == other.ID {
			actor = e.Actor
		}
	}
	if actor != "alice" {
		t.Fatalf("the queued run's actor %q:\n%s", actor, h.sink.String())
	}
	tick(ta) // consumed: no second run
	if n := h.gcs.Load(); n != 1 {
		t.Fatalf("%d runs after another tick", n)
	}
	// A run that starts another way (run now when the lock is free) answers
	// a queued request too.
	if err := h.db.QueueTaskRun(context.Background(), tk.ID, "carol", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if queued, err := h.s.RunOrQueue(tk.ID, "dave"); err != nil || queued {
		t.Fatalf("free lock: queued %v, %v", queued, err)
	}
	h.s.Wait()
	if got := h.get(t, tk.ID); !got.QueuedAt.IsZero() {
		t.Fatalf("still queued after its run: %+v", got)
	}
}
