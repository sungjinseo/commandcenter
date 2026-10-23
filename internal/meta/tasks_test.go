package meta

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestTaskCRUD(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	next := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	tk := Task{ID: "t1", Name: "Compact", Type: "blobstore.compact", Schedule: "daily:03:00", Params: "{}", Enabled: true, NextRun: next}
	if err := db.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, tk); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := db.GetTask(ctx, "t1")
	if err != nil || got.Name != "Compact" || !got.Enabled || !got.NextRun.Equal(next) || got.Created.IsZero() || got.LastRun.IsZero() == false {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := db.GetTask(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	tk.Name, tk.Enabled, tk.Schedule = "Compact 2", false, "manual"
	tk.NextRun = time.Time{}
	if err := db.UpdateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTask(ctx, Task{ID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	run := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	if err := db.RecordTaskRun(ctx, "t1", TaskRun{Run: run, Result: "OK", DurationMs: 12, Message: "removed 3 blobs",
		Log: "line1\nline2", Removed: 3, Bytes: 99}, func(cur Task) (time.Time, bool) {
		if cur.Schedule != "manual" || cur.Name != "Compact 2" {
			t.Errorf("next sees %+v", cur)
		}
		return next, true
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetTask(ctx, "t1")
	if got.Name != "Compact 2" || got.Enabled || got.Schedule != "manual" || !got.LastRun.Equal(run) || got.LastResult != "OK" ||
		got.LastDurationMs != 12 || got.LastMessage != "removed 3 blobs" || got.LastLog != "line1\nline2" ||
		got.LastRemoved != 3 || got.LastBytes != 99 || !got.NextRun.Equal(next) {
		t.Fatalf("after run: %+v", got)
	}
	list, err := db.ListTasks(ctx)
	if err != nil || len(list) != 1 || list[0].LastLog != "" || list[0].LastMessage != "removed 3 blobs" {
		t.Fatalf("list (without logs): %+v %v", list, err)
	}
	if err := db.RecordTaskRun(ctx, "nope", TaskRun{}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record missing: %v", err)
	}
	if err := db.DeleteTask(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteTask(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestDueTasks(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, tk := range []Task{
		{ID: "late", Enabled: true, NextRun: now.Add(-time.Hour)},
		{ID: "now", Enabled: true, NextRun: now},
		{ID: "future", Enabled: true, NextRun: now.Add(time.Minute)},
		{ID: "off", Enabled: false, NextRun: now.Add(-time.Hour)},
		{ID: "manual", Enabled: true},
	} {
		tk.Name, tk.Type, tk.Schedule, tk.Params = tk.ID, "blobstore.compact", "daily:03:00", "{}"
		if err := db.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	due, err := db.DueTasks(ctx, now)
	if err != nil || len(due) != 2 || due[0].ID != "late" || due[1].ID != "now" {
		t.Fatalf("due: %+v %v", due, err)
	}
}

func TestTaskLimit(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	old := MaxTasks
	MaxTasks = 2
	t.Cleanup(func() { MaxTasks = old })
	for i := 0; i < 2; i++ {
		if err := db.CreateTask(ctx, Task{ID: fmt.Sprint(i), Name: "x", Type: "t", Schedule: "manual", Params: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateTask(ctx, Task{ID: "2", Name: "x", Type: "t", Schedule: "manual", Params: "{}"}); !errors.Is(err, ErrTaskLimit) {
		t.Fatalf("over limit: %v", err)
	}
}

func TestSeedTask(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	tk := Task{ID: "seed", Name: "Seed", Type: "blobstore.compact", Schedule: "daily:03:00", Params: "{}", Enabled: true}
	ok, err := db.SeedTask(ctx, "tasks.seeded", tk)
	if err != nil || !ok {
		t.Fatalf("first seed: %v %v", ok, err)
	}
	// Deleted by the admin: the flag keeps it from coming back.
	if err := db.DeleteTask(ctx, "seed"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.SeedTask(ctx, "tasks.seeded", tk); err != nil || ok {
		t.Fatalf("second seed: %v %v", ok, err)
	}
	if list, _ := db.ListTasks(ctx); len(list) != 0 {
		t.Fatalf("seed came back: %+v", list)
	}

	// A database that already has tasks is not seeded, but is marked.
	db2, _ := openTestDB(t)
	if err := db2.CreateTask(ctx, Task{ID: "mine", Name: "Mine", Type: "blobstore.compact", Schedule: "manual", Params: "{}"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db2.SeedTask(ctx, "tasks.seeded", tk); err != nil || ok {
		t.Fatalf("seed with tasks: %v %v", ok, err)
	}
	if list, _ := db2.ListTasks(ctx); len(list) != 1 || list[0].ID != "mine" {
		t.Fatalf("tasks: %+v", list)
	}
}

// Without next (or when next declines), next_run is kept.
func TestRecordTaskRunKeepsNextRun(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	next := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	if err := db.CreateTask(ctx, Task{ID: "t", Name: "t", Type: "x", Schedule: "daily:03:00", Params: "{}", Enabled: true, NextRun: next}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordTaskRun(ctx, "t", TaskRun{Result: "OK"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordTaskRun(ctx, "t", TaskRun{Result: "OK"}, func(Task) (time.Time, bool) { return time.Time{}, false }); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetTask(ctx, "t"); !got.NextRun.Equal(next) || got.LastResult != "OK" {
		t.Fatalf("got %+v", got)
	}
}
