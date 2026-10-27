package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"commandcenter/internal/importer"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// import.nexus is started through the import API only: it is not offered
// or creatable as a task, and its params cannot hold a password.
func TestImportTypeIsAdHocOnly(t *testing.T) {
	h := newHarness(t)
	h.s.Import = &importer.Engine{DB: h.db, Repos: h.s.Repos}
	if slices.Contains(Types(), TypeImportNexus) {
		t.Fatal("import.nexus is offered as a task type")
	}
	_, err := h.s.Create(context.Background(), Def{Name: "imp", Type: TypeImportNexus, Schedule: "manual",
		Params: json.RawMessage(`{"url":"http://nexus.example","importContent":true}`)})
	var ve *repo.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "cannot be created") {
		t.Fatalf("create: %v", err)
	}
	_, _, err = h.s.StartImport(json.RawMessage(`{"url":"http://nexus.example","importContent":true,"password":"x"}`), "", "admin")
	if !errors.As(err, &ve) {
		t.Fatalf("a password in the params: %v", err)
	}
	if h.s.Running() != "" {
		t.Fatal("a refused import is running")
	}
	// Without an engine the type is unavailable.
	h.s.Import = nil
	if _, _, err := h.s.StartImport(json.RawMessage(`{"url":"http://nexus.example","importContent":true}`), "", "admin"); !errors.As(err, &ve) {
		t.Fatalf("no engine: %v", err)
	}
}

// The secret handed to StartImport is taken once by the run and cleared at
// its end.
func TestImportSecretIsTakenOnce(t *testing.T) {
	h := newHarness(t)
	h.s.mu.Lock()
	h.s.secret = "pw"
	h.s.mu.Unlock()
	if h.s.takeSecret() != "pw" || h.s.takeSecret() != "" {
		t.Fatal("the secret was not taken once")
	}
}

// The row that records API imports keeps a manual schedule.
func TestImportRowScheduleStaysManual(t *testing.T) {
	h := newHarness(t)
	h.s.Import = &importer.Engine{DB: h.db, Repos: h.s.Repos}
	id := AdHocTaskID(TypeImportNexus)
	if err := h.db.CreateTask(context.Background(), meta.Task{ID: id, Name: "Import (API/CLI)", Type: TypeImportNexus, Schedule: "manual",
		Params:  `{"url":"http://nexus.example","repositories":[],"importDefinitions":true,"importContent":true,"blobStore":"default","job":"j","concurrency":4}`,
		Enabled: true, NotifyEmail: []string{}}); err != nil {
		t.Fatal(err)
	}
	_, err := h.s.Update(context.Background(), id, Def{Schedule: "daily:03:00"})
	var ve *repo.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "schedule must be manual") {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := h.s.Update(context.Background(), id, Def{Name: "Renamed"}); err != nil {
		t.Fatalf("rename: %v", err)
	}
}
