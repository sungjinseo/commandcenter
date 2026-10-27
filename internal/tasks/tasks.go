// Package tasks runs the scheduled maintenance tasks (plan 11 Decision 7):
// blob store compaction, repository cleanup and Maven metadata rebuilds.
// Tasks are rows of the tasks table; a scheduler goroutine checks for due
// tasks every 30 s and runs at most one task at a time, which "run now"
// shares.
package tasks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"commandcenter/internal/backup"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Task types.
const (
	TypeCompact      = "blobstore.compact"
	TypeCleanup      = "repository.cleanup"
	TypeRebuild      = "repository.rebuild-metadata"
	TypeBackup       = "backup"
	TypeBackupVerify = "backup.verify"
)

// Seed task (Decision 7): created on the first start after plan 11 unless
// tasks exist; the settings flag keeps a deleted seed from coming back.
const (
	SeedID       = "compact-blobstore"
	seedFlag     = "tasks.seeded"
	seedSchedule = "daily:03:00"
)

// Defaults of the run limits.
const (
	DefaultInterval      = 30 * time.Second
	DefaultRunTimeout    = 6 * time.Hour
	DefaultMaxComponents = 5000
	maxNameLen           = 100
	maxCleanupRepos      = 1000
	// MaxNotifyEmail is how many failure notification addresses a task may have.
	MaxNotifyEmail = 20
)

// ErrNotFound is returned for a task that does not exist.
var ErrNotFound = meta.ErrNotFound

// ErrSeedTask refuses to delete the built-in compaction task: without it the
// blob store would never be compacted again, and it is not re-created on
// restart. Disable it instead.
var ErrSeedTask = errors.New("the built-in task " + SeedID + " cannot be deleted; disable it instead")

// BusyError refuses a run (or a delete) while a task is running.
type BusyError struct{ Running string }

func (e *BusyError) Error() string {
	if e.Running == "" {
		return "a task is already running"
	}
	return fmt.Sprintf("task %q is running", e.Running)
}

// Service owns the task definitions, the scheduler and the runs.
type Service struct {
	DB       *meta.DB
	Repos    *repo.Registry
	GC       func(ctx context.Context, store string) (int, int64, error) // blobstore.compact: blobs and bytes removed; store "" means all
	Deleters *cleanup.Registry                                           // repository.cleanup
	Maven    *maven.Handler                                              // repository.rebuild-metadata
	Backup   *backup.Service                                             // backup; runs under the run lock, so compaction waits
	// DataLock takes the cross-process data directory lock (datalock) and
	// returns its release; compaction and backup runs hold it, so an offline
	// `commandcenter gc` cannot delete blobs meanwhile. Nil: no lock.
	DataLock func(ctx context.Context) (release func(), err error)
	Events   repo.EventSink // task.run and cleanup events; nil records none
	Mail     Mailer         // failure e-mails (SendAsync never blocks); nil: none
	Metrics  Flusher        // pending download counts, flushed before cleanup evaluates; nil: none
	Log      *slog.Logger   // run logs go here too (and so to the log ring)

	// Test hooks and limits; zero values mean the defaults.
	Now           func() time.Time
	Loc           *time.Location   // schedule zone; nil is time.Local
	Ticks         <-chan time.Time // replaces the scheduler's ticker
	Interval      time.Duration    // scheduler tick
	RunTimeout    time.Duration    // per run
	MaxComponents int              // cleanup components deleted per run
	ScanBudget    int              // cleanup assets read per repository
	RebuildScan   int              // rebuild-metadata assets read

	runMu sync.Mutex // held for the whole of a run: one task at a time

	// recordRun stores a run (DB.RecordTaskRun); a field so tests can fail it.
	recordRun func(ctx context.Context, id string, r meta.TaskRun, next func(meta.Task) (time.Time, bool)) error
	policySig atomic.Uint64 // bumped by PolicyChanged

	mu       sync.Mutex
	running  string // id of the task being run, "" if none
	base     context.Context
	cancel   context.CancelFunc
	started  bool
	loopDone chan struct{}           // closed when the scheduler loop has returned
	runDone  chan struct{}           // closed when the run in progress has finished; nil if none
	runCtx   context.Context         // the run in progress's context, before its timeout
	runStop  context.CancelCauseFunc // cancels runCtx with ErrStopped (StopRun)
	// deferred holds, per task, the next run kept in memory when a
	// scheduled run could not be recorded (the stored next_run is stale):
	// the task is not due before it. The zero time: not due again.
	deferred map[string]time.Time
	// adhoc holds the outcomes of the last ad-hoc runs (StartAdHoc), oldest
	// first in adhocOrder; at most maxAdHoc are kept.
	adhoc      map[string]Outcome
	adhocOrder []string
	// runningRun is the id of the ad-hoc run in progress, "" if none.
	runningRun string
}

// Mailer queues an e-mail without blocking (mail.Sender).
type Mailer interface {
	SendAsync(cfg meta.SMTP, to []string, subject, body string)
}

// Flusher writes pending download counts (metrics.Recorder).
type Flusher interface {
	Flush(ctx context.Context) error
}

// ErrStopped is the cause of a run stopped with StopRun.
var ErrStopped = errors.New("stopped on request")

// PolicyChanged tells a running cleanup that a cleanup policy was edited, so
// it reads its policy again before the next component.
func (s *Service) PolicyChanged() { s.policySig.Add(1) }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) loc() *time.Location {
	if s.Loc != nil {
		return s.Loc
	}
	return time.Local
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Running returns the id of the task being run, "" if none.
func (s *Service) Running() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// TypeName is the English display name of a task type.
func TypeName(typ string) string {
	if d, ok := types[typ]; ok {
		return d.name
	}
	return typ
}

// Types lists the task types, sorted.
func Types() []string {
	out := make([]string, 0, len(types))
	for t := range types {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// NextRun returns when t runs next after now: the zero time for a manual
// schedule, a once in the past, an unparsable schedule or a disabled task.
func (s *Service) NextRun(t meta.Task, now time.Time) time.Time {
	if !t.Enabled {
		return time.Time{}
	}
	sc, err := ParseScheduleIn(t.Schedule, s.loc())
	if err != nil {
		return time.Time{}
	}
	return sc.Next(now, s.loc())
}

// Seed creates the compact-blobstore task on the first start (see SeedID).
func (s *Service) Seed(ctx context.Context) error {
	t := meta.Task{ID: SeedID, Name: "Compact blob store", Type: TypeCompact, Schedule: seedSchedule, Params: "{}", Enabled: true}
	t.NextRun = s.NextRun(t, s.now())
	seeded, err := s.DB.SeedTask(ctx, seedFlag, t)
	if err != nil {
		return fmt.Errorf("seed tasks: %w", err)
	}
	if seeded {
		s.log().Info("created the scheduled task "+SeedID, "schedule", seedSchedule)
	}
	return nil
}

// Def is a task definition as the API receives it. Enabled nil means true
// on create and "unchanged" on update; Type may be omitted on update.
type Def struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Schedule string          `json:"schedule"`
	Params   json.RawMessage `json:"params"`
	Enabled  *bool           `json:"enabled"`
	// NotifyEmail lists who is mailed when a run fails. Nil means none on
	// create and "unchanged" on update; an empty list clears it.
	NotifyEmail []string `json:"notifyEmail"`
}

// List returns every task, without the last logs.
func (s *Service) List(ctx context.Context) ([]meta.Task, error) { return s.DB.ListTasks(ctx) }

// Get returns one task with its last log.
func (s *Service) Get(ctx context.Context, id string) (meta.Task, error) {
	return s.DB.GetTask(ctx, id)
}

// Create validates d and stores it as a new task with a generated id.
func (s *Service) Create(ctx context.Context, d Def) (meta.Task, error) {
	t := meta.Task{Type: d.Type, Enabled: d.Enabled == nil || *d.Enabled}
	if err := s.apply(&t, d, nil); err != nil {
		return meta.Task{}, err
	}
	var b [8]byte
	rand.Read(b[:])
	t.ID = hex.EncodeToString(b[:])
	if err := s.DB.CreateTask(ctx, t); err != nil {
		return meta.Task{}, err
	}
	return s.DB.GetTask(ctx, t.ID)
}

// Update changes the name, schedule, params and enabled flag of task id.
// An omitted (empty or null) field is kept, and a value equal to the stored
// one is not validated again: a task whose once time passed, or whose
// repository was deleted, can still be renamed or disabled. The next run is
// recomputed when the schedule or the enabled flag changes. The type cannot
// change.
func (s *Service) Update(ctx context.Context, id string, d Def) (meta.Task, error) {
	t, err := s.DB.GetTask(ctx, id)
	if err != nil {
		return meta.Task{}, err
	}
	if d.Type != "" && d.Type != t.Type {
		return meta.Task{}, &repo.ValidationError{Errors: []string{"the type of a task cannot change"}}
	}
	prev := t
	if d.Enabled != nil {
		t.Enabled = *d.Enabled
	}
	if d.Name == "" {
		d.Name = prev.Name
	}
	if strings.TrimSpace(d.Schedule) == "" {
		d.Schedule = prev.Schedule
	}
	if isNull(d.Params) {
		d.Params = json.RawMessage(prev.Params)
	}
	if err := s.apply(&t, d, &prev); err != nil {
		return meta.Task{}, err
	}
	if t.Schedule == prev.Schedule && t.Enabled == prev.Enabled {
		t.NextRun = prev.NextRun
	}
	if err := s.DB.UpdateTask(ctx, t); err != nil {
		return meta.Task{}, err
	}
	if !t.NextRun.Equal(prev.NextRun) {
		s.mu.Lock()
		delete(s.deferred, id) // the stored next run is current again
		s.mu.Unlock()
	}
	return s.DB.GetTask(ctx, id)
}

func isNull(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) == 0 || string(b) == "null"
}

// Delete removes task id and its cleanup scan cursors; a running task cannot
// be deleted.
func (s *Service) Delete(ctx context.Context, id string) error {
	if id == SeedID {
		return ErrSeedTask
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running == id {
		return &BusyError{Running: id}
	}
	if err := s.DB.DeleteTask(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.deferred, id)
	s.mu.Unlock()
	if err := s.DB.DeleteSettings(ctx, cursorPrefix(id)); err != nil {
		s.log().Warn("could not delete the cleanup scan cursors of a deleted task", "task", id, "err", logsafe.Err(err))
	}
	return nil
}

// apply validates d's name, schedule and params against t.Type and stores
// their canonical forms and the next run in t. With prev (an update), a
// schedule or params equal to the stored ones are not checked against the
// present (time, repositories). Problems are reported as one
// *repo.ValidationError.
func (s *Service) apply(t *meta.Task, d Def, prev *meta.Task) error {
	var errs []string
	name := strings.TrimSpace(d.Name)
	switch {
	case name == "":
		errs = append(errs, "name is required")
	case utf8.RuneCountInString(name) > maxNameLen:
		errs = append(errs, fmt.Sprintf("name must be at most %d characters", maxNameLen))
	case strings.ContainsFunc(name, unicode.IsControl):
		errs = append(errs, "name must not contain control characters")
	}
	t.Name = name

	if d.NotifyEmail != nil {
		list, nerrs := normalizeNotify(d.NotifyEmail)
		errs = append(errs, nerrs...)
		t.NotifyEmail = list
	} else if t.NotifyEmail == nil {
		t.NotifyEmail = []string{}
	}

	now := s.now()
	sc, err := ParseScheduleIn(d.Schedule, s.loc())
	if err != nil {
		errs = append(errs, err.Error())
	} else {
		t.Schedule = sc.String()
		unchanged := prev != nil && t.Schedule == prev.Schedule
		if sc.Kind == Once && !sc.At.After(now) && !unchanged {
			errs = append(errs, "the once time is in the past")
		}
	}

	def, ok := types[t.Type]
	if !ok {
		errs = append(errs, fmt.Sprintf("unknown type %q: use %s", t.Type, strings.Join(Types(), ", ")))
	} else {
		params, perrs := def.params(s, d.Params, false)
		if len(perrs) > 0 || prev == nil || params != prev.Params {
			params, perrs = def.params(s, d.Params, true)
		}
		errs = append(errs, perrs...)
		t.Params = params
	}
	if len(errs) > 0 {
		return &repo.ValidationError{Errors: errs}
	}
	t.NextRun = s.NextRun(*t, now)
	return nil
}

// normalizeNotify trims and checks the notification addresses.
func normalizeNotify(in []string) ([]string, []string) {
	if len(in) > MaxNotifyEmail {
		return nil, []string{fmt.Sprintf("notifyEmail takes at most %d addresses", MaxNotifyEmail)}
	}
	out := make([]string, 0, len(in))
	var errs []string
	for _, a := range in {
		a = strings.TrimSpace(a)
		if _, err := mail.ParseAddress(a); err != nil || len(a) > 254 || strings.ContainsFunc(a, unicode.IsControl) {
			errs = append(errs, fmt.Sprintf("notifyEmail: %q is not a valid e-mail address", clipStr(a, 60)))
			continue
		}
		out = append(out, a)
	}
	return out, errs
}

func clipStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// decodeParams decodes raw (null or absent means {}) into v, refusing
// unknown fields.
func decodeParams(raw json.RawMessage, v any) error {
	if isNull(raw) {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid params: %v", err)
	}
	if dec.More() {
		return errors.New("invalid params: trailing data")
	}
	return nil
}

func encodeParams(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
