package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"commandcenter/internal/backup"
	"commandcenter/internal/events"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
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
)

// Start runs the scheduler until ctx is cancelled or Stop is called. Runs
// get contexts derived from ctx, so a shutdown cancels a run in progress.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
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

// Wait blocks until no run is in progress.
func (s *Service) Wait() {
	s.runMu.Lock()
	s.runMu.Unlock()
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

// RunDue runs the enabled tasks whose next run has come, one after the
// other, as the system. When a run is already going (a "run now"), it
// returns: the tasks are still due at the next tick.
func (s *Service) RunDue(ctx context.Context) {
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
	if !s.runMu.TryLock() {
		return &BusyError{Running: s.Running()}
	}
	t, err := s.DB.GetTask(context.Background(), id)
	if err != nil {
		s.runMu.Unlock()
		return err
	}
	done := s.begin(t.ID)
	go func() {
		defer s.runMu.Unlock()
		s.execute(t, actor, recordManual, done)
	}()
	return nil
}

// maxAdHoc is how many ad-hoc run outcomes are kept for AdHoc.
const maxAdHoc = 32

// StartAdHoc validates params for a task of type typ and starts an
// unrecorded run of it for actor in the background (POST /-/api/backup). It
// returns the run's id, which AdHoc answers for. A *repo.ValidationError for
// bad params or an unknown type, a *BusyError when a task is running.
func (s *Service) StartAdHoc(typ string, params json.RawMessage, actor string) (string, error) {
	return s.StartAdHocAs(typ, params, actor, true)
}

// StartAdHocAs is StartAdHoc for a caller that is interactive (a password or
// session login) or not (a token, a script). A non-interactive backup run
// cannot change what is destructive: its keep and deep are ignored (the ad-hoc
// row's stored values, set by an interactive run, or the defaults apply) and
// the row is not rewritten with its params.
func (s *Service) StartAdHocAs(typ string, params json.RawMessage, actor string, interactive bool) (string, error) {
	id, _, err := s.startAdHoc(typ, params, actor, interactive, "")
	return id, err
}

// startAdHoc validates params and starts the run; it returns the run id and
// the canonical params. secret is handed to the run in memory (takeSecret).
func (s *Service) startAdHoc(typ string, params json.RawMessage, actor string, interactive bool, secret string) (string, string, error) {
	def, ok := types[typ]
	if !ok {
		return "", "", &repo.ValidationError{Errors: []string{fmt.Sprintf("unknown type %q", typ)}}
	}
	canonical, errs := def.params(s, params, true)
	if len(errs) > 0 {
		return "", "", &repo.ValidationError{Errors: errs}
	}
	if !s.runMu.TryLock() {
		return "", "", &BusyError{Running: s.Running()}
	}
	var b [6]byte
	rand.Read(b[:])
	runID := typ + "-" + hex.EncodeToString(b[:])
	// The run is recorded on the type's ad-hoc task row, so its log persists
	// like a scheduled run's; without the row (task limit, DB error) it runs
	// unrecorded.
	if !interactive && typ == TypeBackup {
		canonical = s.limitBackupParams(canonical)
	}
	t, mode := s.adhocRow(typ, canonical, interactive), recordManual
	if t.ID == "" {
		t, mode = meta.Task{ID: runID, Name: TypeName(typ), Type: typ, Params: canonical}, recordNone
	}
	s.mu.Lock()
	if s.adhoc == nil {
		s.adhoc = map[string]Outcome{}
	}
	s.adhoc[runID] = Outcome{TaskID: t.ID} // Result "": running
	s.adhocOrder = append(s.adhocOrder, runID)
	s.runningRun = runID
	s.secret = secret
	for len(s.adhocOrder) > maxAdHoc {
		delete(s.adhoc, s.adhocOrder[0])
		s.adhocOrder = s.adhocOrder[1:]
	}
	s.mu.Unlock()
	done := s.begin(t.ID)
	go func() {
		defer s.runMu.Unlock()
		out := s.execute(t, actor, mode, done)
		s.mu.Lock()
		if _, ok := s.adhoc[runID]; ok {
			s.adhoc[runID] = out
		}
		s.runningRun = ""
		s.secret = ""
		s.mu.Unlock()
	}()
	return runID, canonical, nil
}

// AdHocTaskID is the id of the task row that records the ad-hoc (API/CLI)
// runs of a task type.
func AdHocTaskID(typ string) string { return "api-" + typ }

// adhocRow returns the type's ad-hoc task row with params set, creating it
// (manual schedule) when missing; the zero Task when it cannot. The caller
// holds runMu.
func (s *Service) adhocRow(typ, params string, store bool) meta.Task {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := AdHocTaskID(typ)
	// The row keeps the params without a backup subdir: "Run now" on it then
	// writes a fresh timestamped backup instead of failing on the last API
	// run's directory. This run still uses the subdir it was given.
	run := params
	if typ == TypeBackup {
		var p backup.Params
		if json.Unmarshal([]byte(params), &p) == nil {
			p.Subdir = ""
			params = encodeParams(p)
		}
	}
	t, err := s.DB.GetTask(ctx, id)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		t = meta.Task{ID: id, Name: TypeName(typ) + " (API/CLI)", Type: typ, Schedule: "manual", Params: params,
			Enabled: true, NotifyEmail: []string{}}
		if err := s.DB.CreateTask(ctx, t); err != nil {
			s.log().Warn("could not create the task that records API runs; this run is not recorded", "task", id, "err", logsafe.Err(err))
			return meta.Task{}
		}
		t.Params = run
		return t
	case err != nil:
		s.log().Warn("could not read the task that records API runs; this run is not recorded", "task", id, "err", logsafe.Err(err))
		return meta.Task{}
	case t.Type != typ:
		return meta.Task{}
	}
	if store {
		t.Params = params
		if err := s.DB.UpdateTask(ctx, t); err != nil {
			s.log().Warn("could not store the params of an API run", "task", id, "err", logsafe.Err(err))
		}
	}
	t.Params = run
	return t
}

// AdHoc returns what is known of ad-hoc run id: running, or its outcome
// once it finished. ok is false for an unknown (or forgotten) id.
func (s *Service) AdHoc(id string) (out Outcome, running, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok = s.adhoc[id]
	return out, ok && out.Result == "", ok
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
	if !s.runMu.TryLock() {
		return Outcome{}, &BusyError{Running: s.Running()}
	}
	defer s.runMu.Unlock()
	t := meta.Task{ID: TypeCompact, Type: TypeCompact, Params: "{}"}
	return s.execute(t, actor, recordNone, s.begin(t.ID)), nil
}

// run takes the run lock (or answers *BusyError), reads the task and runs
// it. A scheduled run skips a task that is no longer due (disabled,
// rescheduled or deleted while another task ran).
func (s *Service) run(id, actor string, mode recordMode) (Outcome, error) {
	if !s.runMu.TryLock() {
		return Outcome{}, &BusyError{Running: s.Running()}
	}
	defer s.runMu.Unlock()
	t, err := s.DB.GetTask(context.Background(), id)
	if err != nil {
		return Outcome{}, err
	}
	if mode == recordScheduled && (!t.Enabled || t.NextRun.IsZero() || t.NextRun.After(s.now()) || s.isDeferred(t.ID)) {
		return Outcome{}, nil
	}
	return s.execute(t, actor, mode, s.begin(t.ID)), nil
}

// isDeferred reports whether a scheduled run of id is held back in memory
// because the last one could not be recorded (see Service.deferred).
func (s *Service) isDeferred(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.deferred[id]
	return ok && (until.IsZero() || s.now().Before(until))
}

// begin marks id as running and creates the run's context, which StopRun
// cancels; the caller holds runMu. execute ends it by closing the returned
// channel.
func (s *Service) begin(id string) chan struct{} {
	done := make(chan struct{})
	ctx, stop := context.WithCancelCause(s.baseContext())
	s.mu.Lock()
	s.running, s.runDone = id, done
	s.runCtx, s.runStop = ctx, stop
	s.mu.Unlock()
	return done
}

func (s *Service) end(done chan struct{}) {
	s.mu.Lock()
	stop := s.runStop
	s.running, s.runDone, s.runCtx, s.runStop = "", nil, nil, nil
	s.secret = "" // an import password never outlives its run
	s.mu.Unlock()
	if stop != nil {
		stop(nil)
	}
	close(done)
}

// StopRun cancels the run of task id if it is the one running and reports
// whether it was. The run ends at its next cancellation check (a cleanup
// before its next component) and records ResultStopped.
func (s *Service) StopRun(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || s.running != id || s.runStop == nil {
		return false
	}
	s.runStop(ErrStopped)
	return true
}

// execute runs t (begun with begin, under runMu) with the run timeout,
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
		if def.dataLock && s.DataLock != nil {
			rlog.Infof("taking the data directory lock")
			release, err := s.DataLock(ctx)
			if err != nil {
				return summary{msg: "the data directory lock was not acquired"}, fmt.Errorf("data directory lock: %w", err)
			}
			defer release()
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
	if out.Result == ResultFailed {
		s.notifyFailure(t, out, rlog)
	}
	if s.Events != nil {
		detail := out.Result + ": " + out.Message
		if out.Result == ResultOK {
			detail = fmt.Sprintf("%s: %s in %d ms", ResultOK, out.Message, out.DurationMs)
		}
		s.Events.Emit("task.run", actor, "", t.ID, detail)
		if t.Type == TypeCleanup {
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

// RunningRun returns the id of the ad-hoc run (StartAdHoc) in progress, ""
// when none: a scheduled or "run now" run has a task id (Running) but no
// ad-hoc run id.
func (s *Service) RunningRun() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningRun
}

// limitBackupParams is what a non-interactive caller's backup params become:
// keep and deep are those stored on the ad-hoc row (set by an interactive
// admin) or the defaults, whatever the caller asked for. A requested verify
// stays (a plain verify changes nothing). The caller holds runMu.
func (s *Service) limitBackupParams(canonical string) string {
	var p backup.Params
	if json.Unmarshal([]byte(canonical), &p) != nil {
		return canonical
	}
	asked := p.Verify
	p.Keep, p.Deep = backup.DefaultKeep, false
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if row, err := s.DB.GetTask(ctx, AdHocTaskID(TypeBackup)); err == nil && row.Type == TypeBackup {
		var stored backup.Params
		if json.Unmarshal([]byte(row.Params), &stored) == nil {
			if stored.Keep >= 1 && stored.Keep <= backup.MaxKeep {
				p.Keep = stored.Keep
			}
			p.Deep = stored.Deep
		}
	}
	p.Verify = asked || p.Deep
	return encodeParams(p)
}
