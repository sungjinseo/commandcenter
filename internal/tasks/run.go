package tasks

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"commandcenter/internal/events"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// Results of a run.
const (
	ResultOK      = "OK"
	ResultFailed  = "Failed"
	ResultStopped = "Stopped" // StopRun cancelled the run
)

// Outcome is the result of one run.
type Outcome struct {
	TaskID     string
	Start      time.Time
	DurationMs int64
	Result     string // ResultOK, ResultFailed or ResultStopped
	Message    string // URLs redacted
	Removed    int64  // blobs (compact), components (cleanup), documents or a proxy's cached files (rebuild)
	Bytes      int64  // of the removed blobs (compact) or deleted assets (cleanup)
	Err        error  // why the run failed or stopped; nil when Result is OK
}

// How a run is recorded on its task.
type recordMode int

const (
	recordNone      recordMode = iota // an ad-hoc run without a task row
	recordManual                      // run now: next_run is left alone
	recordScheduled                   // a due run: next_run is recomputed
	recordQueued                      // a queued run now (RunOrQueue): as recordManual, if still queued
)

// runLock is the cluster lock (meta.DB.TryLock) a run holds from before it
// reads its task until after its outcome is recorded: one run at a time on
// every node, and the next one reads the next run its record stored. A
// node that dies mid-run releases it with its database session; the task
// stays due, so the leader runs it again (at least once).
const runLock = "tasks.run"

// Start runs the scheduler until ctx is cancelled or Stop is called. Runs
// get contexts derived from ctx, so a shutdown cancels a run in progress.
// It also answers the other nodes: a stop of the run in progress (StopRun
// there), an edited cleanup policy (PolicyChanged) and the pending
// download counts a cleanup there needs (Notify "flush").
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.DB.Subscribe("task.stop", func(id string) { s.stopLocal(id) })
	s.DB.Subscribe("policies", func(string) { s.policySig.Add(1) })
	if s.Metrics != nil {
		s.DB.Subscribe("flush", func(string) { go s.flushLocal() })
	}
	s.base, s.cancel = context.WithCancel(ctx)
	base := s.base
	loopDone := make(chan struct{})
	s.loopDone = loopDone
	s.mu.Unlock()

	ticks := s.Ticks
	var stopTicker func()
	if ticks == nil {
		every := s.Interval
		if every <= 0 {
			every = DefaultInterval
		}
		tk := time.NewTicker(every)
		ticks, stopTicker = tk.C, tk.Stop
	}
	go func() {
		defer close(loopDone)
		if stopTicker != nil {
			defer stopTicker()
		}
		for {
			select {
			case <-base.Done():
				return
			case <-ticks:
				s.RunDue(base)
			}
		}
	}()
}

// stopWait bounds how long Stop waits; a var for tests.
var stopWait = 30 * time.Second

// Stop cancels the scheduler and a run in progress (scheduled or run now)
// and waits, at most stopWait in all, for the scheduler loop and the run to
// return. A run that ignores its cancelled context is logged and left.
func (s *Service) Stop() {
	s.mu.Lock()
	cancel, loopDone := s.cancel, s.loopDone
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	timer := time.NewTimer(stopWait)
	defer timer.Stop()
	if loopDone != nil {
		select {
		case <-loopDone:
		case <-timer.C:
			s.log().Warn("a task run did not stop in time", "task", s.Running())
			return
		}
	}
	s.mu.Lock()
	runDone := s.runDone
	s.mu.Unlock()
	if runDone != nil {
		select {
		case <-runDone:
		case <-timer.C:
			s.log().Warn("a task run did not stop in time", "task", s.Running())
		}
	}
}

// Wait blocks until no run is in progress on this node.
func (s *Service) Wait() {
	for {
		s.mu.Lock()
		done := s.runDone
		s.mu.Unlock()
		if done == nil {
			return
		}
		<-done
	}
}

// Acquire takes runLock, as a run does, or answers *BusyError naming the
// running task. The gc command holds it, so that it never collects beside a
// run on any node.
func (s *Service) Acquire() (func(), error) {
	unlock, ok, err := s.DB.TryLock(runLock)
	if err != nil {
		return nil, fmt.Errorf("take the task run lock: %w", err)
	}
	if !ok {
		return nil, &BusyError{Running: s.Running()}
	}
	return unlock, nil
}

// baseContext is the parent of run contexts: the scheduler's, or
// Background before Start.
func (s *Service) baseContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base != nil {
		return s.base
	}
	return context.Background()
}

// RunDue runs, on the leader only (meta.DB.Leader), the queued runs (see
// RunOrQueue) as whoever asked for them, oldest first, then the enabled
// tasks whose next run has come, as the system, one after the other. When
// a run is already going (a "run now", or on another node), it returns:
// the runs still wait at the next tick.
//
// A task whose next run passed while no node ran the scheduler (all of
// them were down) runs once at the first tick, however many runs it
// missed, and its next run is then computed from that time (core-47).
func (s *Service) RunDue(ctx context.Context) {
	if !s.DB.Leader() {
		return
	}
	queued, err := s.DB.QueuedTasks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log().Warn("scheduler: could not list queued runs", "err", logsafe.Err(err))
		}
		return
	}
	for _, t := range queued {
		if ctx.Err() != nil {
			return
		}
		_, err := s.run(t.ID, t.QueuedBy, recordQueued)
		var busy *BusyError
		if errors.As(err, &busy) {
			return
		}
	}
	due, err := s.DB.DueTasks(ctx, s.now())
	if err != nil {
		if ctx.Err() == nil {
			s.log().Warn("scheduler: could not list due tasks", "err", logsafe.Err(err))
		}
		return
	}
	for _, t := range due {
		if ctx.Err() != nil {
			return
		}
		_, err := s.run(t.ID, events.System, recordScheduled)
		var busy *BusyError
		if errors.As(err, &busy) {
			return
		}
	}
}

// RunNow runs task id for actor and returns when it is done; next_run is
// not changed. A *BusyError when a task is running, ErrNotFound for an
// unknown id. The run stops only on shutdown or after the run timeout.
func (s *Service) RunNow(id, actor string) (Outcome, error) { return s.run(id, actor, recordManual) }

// RunAsync starts task id for actor in the background ("run now"); next_run
// is not changed. The task shows as running when it returns. A *BusyError
// when a task is running, ErrNotFound for an unknown id.
func (s *Service) RunAsync(id, actor string) error {
	unlock, err := s.Acquire()
	if err != nil {
		return err
	}
	t, err := s.DB.GetTask(context.Background(), id)
	if err != nil {
		unlock()
		return err
	}
	go s.execute(t, actor, recordManual, s.begin(t.ID, unlock))
	return nil
}

// RunOrQueue is RunAsync, except that while another task runs, on any
// node, the run is queued instead of refused and queued is true: the
// leader starts it at a scheduler tick once the run lock is free, as
// actor, and records it as a run now. A second request for a task that
// is already queued keeps the first. Different tasks never run at once:
// compaction, blob store moves and reconcile plans rely on it. A
// *BusyError when task id itself is running, ErrNotFound for an unknown
// id.
func (s *Service) RunOrQueue(id, actor string) (queued bool, err error) {
	err = s.RunAsync(id, actor)
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Running == id {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.DB.QueueTaskRun(ctx, id, actor, s.now()); err != nil {
		return false, err
	}
	return true, nil
}

// RunCompact runs blob store compaction for actor and waits for it (POST
// /-/api/gc): the first blobstore.compact task when one exists, so its
// state is recorded, otherwise an unrecorded run with the type as the
// event subject.
func (s *Service) RunCompact(actor string) (Outcome, error) {
	list, err := s.DB.ListTasks(context.Background())
	if err != nil {
		return Outcome{}, err
	}
	for _, t := range list {
		if t.Type == TypeCompact {
			out, err := s.run(t.ID, actor, recordManual)
			if !errors.Is(err, ErrNotFound) { // deleted meanwhile: run unrecorded
				return out, err
			}
			break
		}
	}
	unlock, err := s.Acquire()
	if err != nil {
		return Outcome{}, err
	}
	t := meta.Task{ID: TypeCompact, Type: TypeCompact, Params: "{}"}
	return s.execute(t, actor, recordNone, s.begin(t.ID, unlock)), nil
}

// run takes the run lock (or answers *BusyError), reads the task and runs
// it. A scheduled run skips a task that is no longer due (disabled,
// rescheduled or deleted while another task ran), a queued one a task no
// longer queued (another run of it began).
func (s *Service) run(id, actor string, mode recordMode) (Outcome, error) {
	unlock, err := s.Acquire()
	if err != nil {
		return Outcome{}, err
	}
	t, err := s.DB.GetTask(context.Background(), id)
	if err != nil {
		unlock()
		return Outcome{}, err
	}
	if mode == recordScheduled && (!t.Enabled || t.NextRun.IsZero() || t.NextRun.After(s.now()) || s.isDeferred(t.ID)) ||
		mode == recordQueued && t.QueuedAt.IsZero() {
		unlock()
		return Outcome{}, nil
	}
	return s.execute(t, actor, mode, s.begin(t.ID, unlock)), nil
}

// isDeferred reports whether a scheduled run of id is held back in memory
// because the last one could not be recorded (see Service.deferred).
func (s *Service) isDeferred(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.deferred[id]
	return ok && (until.IsZero() || s.now().Before(until))
}

// begin marks id as running, here and in its row, and creates the run's
// context, which StopRun cancels; unlock releases runLock, which the
// caller took. execute ends it by closing the returned channel.
func (s *Service) begin(id string, unlock func()) chan struct{} {
	done := make(chan struct{})
	ctx, stop := context.WithCancelCause(s.baseContext())
	s.mu.Lock()
	s.running, s.runDone = id, done
	s.runCtx, s.runStop, s.unlock = ctx, stop, unlock
	s.mu.Unlock()
	s.mark(id, s.now())
	// Any run begun now answers a run now that waits for one.
	uctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.DB.UnqueueTask(uctx, id); err != nil {
		s.log().Warn("could not drop the queued run of a task that began", "task", id, "err", logsafe.Err(err))
	}
	return done
}

// end clears the run, releases runLock (the outcome is recorded by now)
// and closes done.
func (s *Service) end(done chan struct{}) {
	s.mu.Lock()
	id, stop, unlock := s.running, s.runStop, s.unlock
	s.running, s.runDone, s.runCtx, s.runStop, s.unlock = "", nil, nil, nil, nil
	s.mu.Unlock()
	if stop != nil {
		stop(nil)
	}
	s.mark(id, time.Time{})
	unlock()
	close(done)
}

// mark stores in task id's row that this node runs it since since, or with
// the zero time that it does not (tasks.running_node): Running and StopRun
// on the other nodes and GET /-/api/nodes read it. A process that did not
// join as a node marks nothing.
func (s *Service) mark(id string, since time.Time) {
	node := s.DB.NodeID()
	if node == "" {
		return
	}
	if since.IsZero() {
		node = ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.DB.SetTaskRunning(ctx, id, node, since); err != nil {
		s.log().Warn("could not mark the task run on its row; other nodes may not see it", "task", id, "err", logsafe.Err(err))
	}
}

// StopRun cancels the run of task id if it is the one running, on this
// node or another live one, and reports whether it was. The
// run ends at its next cancellation check (a cleanup before its next
// component) and records ResultStopped.
func (s *Service) StopRun(id string) bool {
	if s.stopLocal(id) {
		return true
	}
	if id == "" || s.Running() != id {
		return false
	}
	return s.notify("task.stop:"+id) == nil // the node running it stops it (Start)
}

// stopLocal is StopRun on this node.
func (s *Service) stopLocal(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || s.running != id || s.runStop == nil {
		return false
	}
	s.runStop(ErrStopped)
	return true
}

// execute runs t (begun with begin, under runLock) with the run timeout,
// records the outcome per mode (when the task still exists), and emits the
// events. A panic in the task is recovered and recorded as a failure.
func (s *Service) execute(t meta.Task, actor string, mode recordMode, done chan struct{}) Outcome {
	defer s.end(done)

	timeout := s.RunTimeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	s.mu.Lock()
	runCtx := s.runCtx
	s.mu.Unlock()
	if runCtx == nil {
		runCtx = s.baseContext()
	}
	ctx, cancel := context.WithTimeout(runCtx, timeout)
	defer cancel()
	rlog := newRunLog(s.log(), s.now, "task", t.ID, "type", t.Type)
	start := s.now()
	s.log().Info("task started", "task", t.ID, "type", t.Type, "actor", actor)

	sum, err := func() (sum summary, err error) {
		defer func() {
			if p := recover(); p != nil {
				s.log().Error("task panicked", "task", t.ID, "panic", logsafe.URLs(fmt.Sprint(p)), "stack", string(debug.Stack()))
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		def, ok := types[t.Type]
		if !ok {
			return summary{msg: "unknown task type"}, fmt.Errorf("unknown task type %q", t.Type)
		}
		if why, err := s.recoverySkip(ctx, t.Type); err != nil {
			return summary{}, err
		} else if why != "" {
			rlog.Warnf("skipped: %s", why)
			return summary{msg: "skipped: " + why}, nil
		}
		return def.run(ctx, s, t, actor, rlog)
	}()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	end := s.now()
	out := Outcome{TaskID: t.ID, Start: start, DurationMs: end.Sub(start).Milliseconds(), Result: ResultOK,
		Message: sum.msg, Removed: sum.removed, Bytes: sum.bytes}
	if err != nil {
		out.Result, out.Err = ResultFailed, err
		switch {
		case errors.Is(context.Cause(ctx), ErrStopped):
			out.Result = ResultStopped
			err = fmt.Errorf("stopped on request before it finished: %w", err)
		case errors.Is(err, context.DeadlineExceeded):
			err = fmt.Errorf("stopped after the run timeout of %s: %w", timeout, err)
		case errors.Is(err, context.Canceled):
			err = fmt.Errorf("stopped by the server shutdown: %w", err)
		}
		if out.Result == ResultStopped {
			rlog.Warnf("%v", err)
		} else {
			rlog.Errorf("failed: %v", err)
		}
		out.Message = err.Error()
		if sum.msg != "" {
			out.Message += " (" + sum.msg + ")"
		}
	}
	out.Message = logsafe.URLs(out.Message)
	rlog.Infof("finished: %s in %d ms", out.Result, out.DurationMs)

	if mode != recordNone {
		s.record(t, out, rlog, mode == recordScheduled)
	}
	s.notifyRun(t, out, rlog)
	if s.Events != nil {
		detail := out.Result + ": " + out.Message
		if out.Result == ResultOK {
			detail = fmt.Sprintf("%s: %s in %d ms", ResultOK, out.Message, out.DurationMs)
		}
		s.Events.Emit("task.run", actor, "", t.ID, detail)
		if t.Type == TypeCleanup || t.Type == TypePurgeUnused {
			s.Events.Emit("cleanup", actor, "", t.ID, cleanupDetail(sum))
		}
	}
	return out
}

// record stores out on task t. For a scheduled run the next run is
// recomputed in the same transaction from the task as stored then (it may
// have been edited during the run); a manual run leaves it alone. A
// context of its own: the run's may be cancelled or timed out.
//
// When a scheduled run cannot be recorded, its stored next_run is stale and
// the task would run again at every tick: the next run is then kept in
// memory (Service.deferred) until a record succeeds or the task is edited.
func (s *Service) record(t meta.Task, out Outcome, rlog *RunLog, scheduled bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseContext()), 30*time.Second)
	defer cancel()
	run := meta.TaskRun{Run: out.Start, Result: out.Result, DurationMs: out.DurationMs, Message: out.Message,
		Log: rlog.String(), Removed: out.Removed, Bytes: out.Bytes}
	var next func(meta.Task) (time.Time, bool)
	if scheduled {
		next = func(cur meta.Task) (time.Time, bool) {
			// An edit during the run already set a future next run.
			if cur.NextRun.After(out.Start) {
				return time.Time{}, false
			}
			return s.NextRun(cur, s.now()), true
		}
	}
	recordRun := s.recordRun
	if recordRun == nil {
		recordRun = s.DB.RecordTaskRun
	}
	err := recordRun(ctx, t.ID, run, next)
	switch {
	case err == nil:
		s.mu.Lock()
		delete(s.deferred, t.ID)
		s.mu.Unlock()
	case errors.Is(err, meta.ErrNotFound):
	case scheduled:
		nr := s.NextRun(t, s.now())
		s.mu.Lock()
		if s.deferred == nil {
			s.deferred = map[string]time.Time{}
		}
		s.deferred[t.ID] = nr
		s.mu.Unlock()
		when := "not again until the task is edited or the server restarts"
		if !nr.IsZero() {
			when = "next at " + nr.UTC().Format(time.RFC3339) + " (kept in memory)"
		}
		s.log().Error("could not record the task run; it runs "+when, "task", t.ID, "err", logsafe.Err(err))
	default:
		s.log().Error("could not record the task run", "task", t.ID, "err", logsafe.Err(err))
	}
}
