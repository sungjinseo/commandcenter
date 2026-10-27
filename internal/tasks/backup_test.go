package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/backup"
	"commandcenter/internal/datalock"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
)

func (h *harness) withBackup(t *testing.T) *backup.Service {
	t.Helper()
	if metatest.Postgres() {
		t.Skip("online backups need the SQLite metadata database")
	}
	b := &backup.Service{DB: h.db, Store: h.store, Dir: filepath.Join(t.TempDir(), "backup"), Version: "test", Events: h.sink}
	h.s.Backup = b
	return b
}

func TestBackupParams(t *testing.T) {
	h := newHarness(t)
	h.withBackup(t)
	tk := h.create(t, Def{Name: "Nightly backup", Type: TypeBackup, Schedule: "daily:02:00", Params: json.RawMessage(`{"verify":false,"deep":true}`)})
	if tk.Params != `{"keep":7,"verify":true,"deep":true}` {
		t.Fatalf("canonical params: %s", tk.Params)
	}
	for _, bad := range []string{`{"subdir":"../x"}`, `{"subdir":"a/b"}`, `{"subdir":".."}`, `{"keep":-1}`, `{"keep":100000}`, `{"other":1}`} {
		_, err := h.s.Create(context.Background(), Def{Name: "b", Type: TypeBackup, Schedule: "manual", Params: json.RawMessage(bad)})
		var ve *repo.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: want a validation error, got %v", bad, err)
		}
	}
	if TypeName(TypeBackup) != "Back up" {
		t.Fatalf("type name %q", TypeName(TypeBackup))
	}
}

func TestBackupTaskRunsAndRecords(t *testing.T) {
	h := newHarness(t)
	b := h.withBackup(t)
	h.put(t, "mvn", "a/1/a-1.jar", "jar")
	tk := h.create(t, Def{Name: "Backup", Type: TypeBackup, Schedule: "manual", Params: json.RawMessage(`{"subdir":"one"}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultOK || !strings.Contains(out.Message, "one") {
		t.Fatalf("run: %+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "one", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, tk.ID); got.LastResult != ResultOK || !strings.Contains(got.LastLog, "copied 1 new blobs") {
		t.Fatalf("recorded: %+v", got)
	}
	// A second run into the same subdir fails and is recorded as failed.
	out, _ = h.s.RunNow(tk.ID, "admin")
	if out.Result != ResultFailed || !strings.Contains(out.Message, "already exists") {
		t.Fatalf("second run: %+v", out)
	}
	if !strings.Contains(h.sink.String(), "backup|admin||one") {
		t.Fatalf("events: %s", h.sink.String())
	}
}

// Compaction cannot run while a backup holds the run lock: a compact asked
// for during the backup is refused (the scheduler retries it at its next
// tick), and runs once the backup is done.
func TestCompactWaitsForBackup(t *testing.T) {
	h := newHarness(t)
	b := h.withBackup(t)
	h.put(t, "mvn", "a", "x")
	entered, release := make(chan struct{}), make(chan struct{})
	b.AfterSnapshot = func() { close(entered); <-release }
	id, err := h.s.StartAdHoc(TypeBackup, json.RawMessage(`{"subdir":"held"}`), "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := h.s.RunCompact("admin"); !isBusy(err) {
		t.Fatalf("compact during backup: %v", err)
	}
	compact := h.create(t, Def{Name: "Compact", Type: TypeCompact, Schedule: "manual"})
	if err := h.s.RunAsync(compact.ID, "admin"); !isBusy(err) {
		t.Fatalf("compact run now during backup: %v", err)
	}
	if _, err := h.s.StartAdHoc(TypeBackup, nil, "admin"); !isBusy(err) {
		t.Fatalf("second backup: %v", err)
	}
	if n := h.gcs.Load(); n != 0 {
		t.Fatalf("GC ran %d times during the backup", n)
	}
	if out, running, ok := h.s.AdHoc(id); !ok || !running || out.Result != "" {
		t.Fatalf("ad-hoc state while running: %+v %v %v", out, running, ok)
	}
	close(release)
	h.s.Wait()
	out, running, ok := h.s.AdHoc(id)
	if !ok || running || out.Result != ResultOK {
		t.Fatalf("ad-hoc outcome: %+v %v %v", out, running, ok)
	}
	if out, err := h.s.RunCompact("admin"); err != nil || out.Result != ResultOK || h.gcs.Load() != 1 {
		t.Fatalf("compact after backup: %+v %v", out, err)
	}
	if _, _, ok := h.s.AdHoc("backup-unknown"); ok {
		t.Fatal("unknown ad-hoc run found")
	}
}

func TestStartAdHocValidates(t *testing.T) {
	h := newHarness(t)
	h.withBackup(t)
	_, err := h.s.StartAdHoc(TypeBackup, json.RawMessage(`{"subdir":"../x"}`), "admin")
	var ve *repo.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want a validation error, got %v", err)
	}
	if _, err := h.s.StartAdHoc("nope", nil, "admin"); !errors.As(err, &ve) {
		t.Fatalf("unknown type: %v", err)
	}
	if h.s.Running() != "" {
		t.Fatal("a refused run is running")
	}
	// Without a backup service the run fails cleanly.
	h.s.Backup = nil
	id, err := h.s.StartAdHoc(TypeBackup, nil, "admin")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, running, _ := h.s.AdHoc(id)
		if !running && out.Result == ResultFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no failed outcome: %+v", out)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func isBusy(err error) bool {
	var busy *BusyError
	return errors.As(err, &busy)
}

// Compaction and backup runs take the cross-process data directory lock, so
// an offline `commandcenter gc` holding it makes them wait (and the other
// way round).
func TestRunsTakeTheDataLock(t *testing.T) {
	h := newHarness(t)
	h.withBackup(t)
	path := filepath.Join(t.TempDir(), datalock.FileName)
	var taken atomic.Int32
	h.s.DataLock = func(ctx context.Context) (func(), error) {
		l, err := datalock.Acquire(ctx, path)
		if err != nil {
			return nil, err
		}
		taken.Add(1)
		return l.Release, nil
	}
	held, err := datalock.TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Outcome, 1)
	go func() { out, _ := h.s.RunCompact("admin"); done <- out }()
	time.Sleep(300 * time.Millisecond)
	if n := h.gcs.Load(); n != 0 {
		t.Fatalf("GC ran while another process held the lock")
	}
	held.Release()
	if out := <-done; out.Result != ResultOK || h.gcs.Load() != 1 {
		t.Fatalf("compact after release: %+v", out)
	}
	h.put(t, "mvn", "a", "x")
	tk := h.create(t, Def{Name: "Backup", Type: TypeBackup, Schedule: "manual"})
	if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultOK {
		t.Fatalf("backup: %+v %v", out, err)
	}
	if taken.Load() != 2 {
		t.Fatalf("the lock was taken %d times, want 2 (compact, backup)", taken.Load())
	}
	// The lock is free again after the runs.
	l, err := datalock.TryAcquire(path)
	if err != nil {
		t.Fatalf("lock still held after the runs: %v", err)
	}
	l.Release()
}

// Ad-hoc (API/CLI) runs are recorded on a per-type task row, so their run
// log persists like a scheduled run's.
func TestAdHocRunIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.withBackup(t)
	h.put(t, "mvn", "a", "x")
	id, err := h.s.StartAdHoc(TypeBackup, json.RawMessage(`{"subdir":"api1"}`), "admin")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Wait()
	out, running, ok := h.s.AdHoc(id)
	if !ok || running || out.Result != ResultOK || out.TaskID != AdHocTaskID(TypeBackup) {
		t.Fatalf("outcome: %+v %v %v", out, running, ok)
	}
	row := h.get(t, AdHocTaskID(TypeBackup))
	if row.Type != TypeBackup || row.LastResult != ResultOK || !strings.Contains(row.LastLog, "copied 1 new blobs") ||
		strings.Contains(row.Params, "subdir") || !strings.Contains(row.Params, `"keep":7`) || row.Schedule != "manual" || !row.NextRun.IsZero() {
		t.Fatalf("recorded row: %+v", row)
	}
	// A second run reuses the row.
	if _, err := h.s.StartAdHoc(TypeBackup, json.RawMessage(`{"subdir":"api2"}`), "admin"); err != nil {
		t.Fatal(err)
	}
	h.s.Wait()
	if row := h.get(t, AdHocTaskID(TypeBackup)); strings.Contains(row.Params, "api2") || row.LastResult != ResultOK {
		t.Fatalf("second run: %+v", row)
	}
	// "Run now" on the row does not reuse an API run's fixed subdir: it gets
	// a fresh timestamped one.
	for range 2 {
		out, err := h.s.RunNow(AdHocTaskID(TypeBackup), "admin")
		if err != nil || out.Result != ResultOK {
			t.Fatalf("run now on the API row: %+v %v", out, err)
		}
		h.clock.Set(h.clock.Now().Add(time.Second))
	}
}

// The deep verify task repairs corrupt pool blobs and fails while it finds
// problems.
func TestBackupVerifyTask(t *testing.T) {
	h := newHarness(t)
	b := h.withBackup(t)
	a, err := h.store.Put(context.Background(), "mvn", "a", "", strings.NewReader("apple"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(context.Background(), backup.Params{Subdir: "v"}, "admin", nil); err != nil {
		t.Fatal(err)
	}
	pool := filepath.Join(b.Dir, "blobs", "default", "sha256", a.BlobSHA256[:2], a.BlobSHA256[2:4], a.BlobSHA256)
	if err := os.WriteFile(pool, []byte("APPLE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.StartAdHoc(TypeBackupVerify, json.RawMessage(`{"name":"../x"}`), "admin"); err == nil {
		t.Fatal("unsafe name accepted")
	}
	id, err := h.s.StartAdHoc(TypeBackupVerify, json.RawMessage(`{"name":"v"}`), "admin")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Wait()
	out, _, _ := h.s.AdHoc(id)
	if out.Result != ResultFailed || !strings.Contains(out.Message, "1 corrupt") || !strings.Contains(out.Message, "1 removed") {
		t.Fatalf("verify outcome: %+v", out)
	}
	if _, err := os.Stat(pool); !os.IsNotExist(err) {
		t.Fatal("the corrupt pool blob is still there")
	}
	if row := h.get(t, AdHocTaskID(TypeBackupVerify)); row.LastResult != ResultFailed || !strings.Contains(row.LastLog, "removed from the pool") {
		t.Fatalf("recorded: %+v", row)
	}
}

// A non-interactive caller (a token) cannot change retention or ask for the
// repairing deep verify: its keep and deep are ignored, the row's stored
// values (set by an interactive admin) or the defaults apply, and the row is
// not rewritten with them.
func TestAdHocLimitedIgnoresKeepAndDeep(t *testing.T) {
	h := newHarness(t)
	b := h.withBackup(t)
	h.put(t, "mvn", "a", "x")
	runAs := func(interactive bool, params string) {
		t.Helper()
		if _, err := h.s.StartAdHocAs(TypeBackup, json.RawMessage(params), "ci", interactive); err != nil {
			t.Fatal(err)
		}
		h.s.Wait()
	}
	names := func() []string {
		list, err := b.List()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, i := range list {
			out = append(out, i.Name)
		}
		return out
	}
	// A token's keep 1 with no stored row: the default (7) applies, so
	// nothing is pruned.
	runAs(true, `{"subdir":"b1"}`)
	runAs(true, `{"subdir":"b2"}`)
	runAs(false, `{"subdir":"b3","keep":1}`)
	if got := names(); len(got) != 3 {
		t.Fatalf("a token's keep pruned backups: %v", got)
	}
	if row := h.get(t, AdHocTaskID(TypeBackup)); !strings.Contains(row.Params, `"keep":7`) {
		t.Fatalf("row after the token run: %s", row.Params)
	}
	// An interactive admin stores keep 3; a token's keep 1 then uses 3.
	runAs(true, `{"subdir":"b4","keep":3}`)
	if row := h.get(t, AdHocTaskID(TypeBackup)); !strings.Contains(row.Params, `"keep":3`) {
		t.Fatalf("row after the admin run: %s", row.Params)
	}
	if got := names(); len(got) != 3 {
		t.Fatalf("after keep 3: %v", got)
	}
	runAs(false, `{"subdir":"b5","keep":1,"verify":true,"deep":true}`)
	if got := names(); len(got) != 3 {
		t.Fatalf("a token's keep 1 pruned to %v, want the stored 3", got)
	}
	if row := h.get(t, AdHocTaskID(TypeBackup)); !strings.Contains(row.Params, `"keep":3`) || strings.Contains(row.Params, `"deep":true`) {
		t.Fatalf("row after the second token run: %s", row.Params)
	}
	// The token's verify ran, but not as the repairing deep verify.
	list, _ := b.List()
	if list[0].Name != "b5" || list[0].Verified == nil || list[0].Verified.Deep {
		t.Fatalf("token verify: %+v", list[0])
	}
	// Out of range is still refused for a token, and keep 0 for everyone.
	var ve *repo.ValidationError
	if _, err := h.s.StartAdHocAs(TypeBackup, json.RawMessage(`{"keep":101}`), "ci", false); !errors.As(err, &ve) {
		t.Fatalf("keep 101: %v", err)
	}
	for _, interactive := range []bool{true, false} {
		if _, err := h.s.StartAdHocAs(TypeBackup, json.RawMessage(`{"keep":0}`), "ci", interactive); !errors.As(err, &ve) {
			t.Fatalf("keep 0 (interactive=%v): %v", interactive, err)
		}
	}
}

func TestBackupParamsRefuseExplicitKeepZero(t *testing.T) {
	h := newHarness(t)
	h.withBackup(t)
	for _, bad := range []string{`{"keep":0}`, `{"subdir":"x","keep":0}`} {
		_, err := h.s.Create(context.Background(), Def{Name: "b", Type: TypeBackup, Schedule: "manual", Params: json.RawMessage(bad)})
		var ve *repo.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: want a validation error, got %v", bad, err)
		}
	}
	// Omitted keep still means the default.
	if tk := h.create(t, Def{Name: "b", Type: TypeBackup, Schedule: "manual", Params: json.RawMessage(`{}`)}); !strings.Contains(tk.Params, `"keep":7`) {
		t.Fatalf("params: %s", tk.Params)
	}
}

// RunningRun is the ad-hoc run in progress, "" for none or a scheduled or
// "run now" run.
func TestRunningRun(t *testing.T) {
	h := newHarness(t)
	b := h.withBackup(t)
	h.put(t, "mvn", "a", "x")
	if h.s.RunningRun() != "" {
		t.Fatal("run before any start")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	b.AfterSnapshot = func() { close(entered); <-release }
	id, err := h.s.StartAdHoc(TypeBackup, json.RawMessage(`{"subdir":"r1"}`), "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if got := h.s.RunningRun(); got != id || h.s.Running() != AdHocTaskID(TypeBackup) {
		t.Fatalf("RunningRun %q Running %q, want %q", got, h.s.Running(), id)
	}
	close(release)
	h.s.Wait()
	if got := h.s.RunningRun(); got != "" {
		t.Fatalf("RunningRun after the run: %q", got)
	}
	// A "run now" run has a task id but no ad-hoc run id.
	b.AfterSnapshot = nil
	tk := h.create(t, Def{Name: "B", Type: TypeBackup, Schedule: "manual", Params: json.RawMessage(`{"subdir":"r2"}`)})
	if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultOK || h.s.RunningRun() != "" {
		t.Fatalf("run now: %+v %v %q", out, err, h.s.RunningRun())
	}
}

// On PostgreSQL the backup task types cannot be created or started.
func TestBackupDisabledOnPostgres(t *testing.T) {
	h := newHarness(t)
	h.s.Backup = &backup.Service{DB: h.db, Dir: t.TempDir(), Disabled: backup.ErrPostgres}
	for _, typ := range []string{TypeBackup, TypeBackupVerify} {
		_, err := h.s.Create(context.Background(), Def{Name: "b " + typ, Type: typ, Schedule: "manual"})
		var ve *repo.ValidationError
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), "pg_dump") {
			t.Errorf("create %s: %v", typ, err)
		}
		if _, err := h.s.StartAdHoc(typ, nil, "admin"); !errors.As(err, &ve) || !strings.Contains(err.Error(), "pg_dump") {
			t.Errorf("start %s: %v", typ, err)
		}
	}
}
