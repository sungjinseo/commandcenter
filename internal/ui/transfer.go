package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"commandcenter/internal/tasks"
)

// Repository export and import shortcuts: each creates a manual
// repository.export or repository.import task with the body as its params
// and starts it, so progress and the result are the task's (GET
// /-/api/tasks/{id}).

func (u *UI) transferRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /-/api/export", func(w http.ResponseWriter, r *http.Request) { u.startTransfer(w, r, tasks.TypeExport, "Export") })
	mux.HandleFunc("POST /-/api/import", func(w http.ResponseWriter, r *http.Request) { u.startTransfer(w, r, tasks.TypeImport, "Import") })
}

// startTransfer answers 202 {id, task} once the run started, 400 for
// invalid params, 409 (leaving no task behind) while a task runs.
func (u *UI) startTransfer(w http.ResponseWriter, r *http.Request, typ, verb string) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.tasksAvailable(w) {
		return
	}
	var params json.RawMessage
	if !decodeBody(w, r, &params) {
		return
	}
	if running := u.Tasks.Running(); running != "" {
		u.writeTaskError(w, r, &tasks.BusyError{Running: running})
		return
	}
	t, err := u.Tasks.Create(r.Context(), tasks.Def{Name: verb + " " + time.Now().UTC().Format("2006-01-02 15:04:05"),
		Type: typ, Schedule: tasks.Manual, Params: params})
	if err != nil {
		u.writeTaskError(w, r, err)
		return
	}
	u.emit(r, "task.create", "", t.ID, fmt.Sprintf("%s (%s, %s)", t.Name, t.Type, t.Schedule))
	if err := u.Tasks.RunAsync(t.ID, principal(r).Owner()); err != nil {
		if u.Tasks.Delete(context.WithoutCancel(r.Context()), t.ID) == nil {
			u.emit(r, "task.delete", "", t.ID, fmt.Sprintf("%s (%s)", t.Name, t.Type))
		}
		u.writeTaskError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": t.ID, "task": u.toTaskJSON(t, t.ID, false)})
}
