package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/tasks"
)

// Scheduled task API (plan 11 Decision 7). Reads are admin only; mutations
// and runs need an interactive admin and the UI header. The plan 7-10
// fields keep their names; the others are new.

func (u *UI) taskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/tasks", u.listTasks)
	mux.HandleFunc("POST /-/api/tasks", u.createTask)
	mux.HandleFunc("GET /-/api/tasks/{id}", u.getTask)
	mux.HandleFunc("PUT /-/api/tasks/{id}", u.updateTask)
	mux.HandleFunc("DELETE /-/api/tasks/{id}", u.deleteTask)
	mux.HandleFunc("POST /-/api/tasks/{id}/run", u.runTask)
	mux.HandleFunc("POST /-/api/tasks/{id}/stop", u.stopTask)
	mux.HandleFunc("POST /-/api/gc", u.runGC)
}

type taskJSON struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	TypeName       string          `json:"typeName"`
	Status         string          `json:"status"` // Running, Waiting or Disabled
	Schedule       string          `json:"schedule"`
	Enabled        bool            `json:"enabled"`
	Params         json.RawMessage `json:"params"`
	NotifyEmail    []string        `json:"notifyEmail"`
	NotifyOn       string          `json:"notifyOn"` // failure or success_failure
	Queued         bool            `json:"queued"`   // a run now waits for the running task (RunOrQueue)
	NextRun        string          `json:"nextRun"`
	LastRun        string          `json:"lastRun"`
	LastResult     string          `json:"lastResult"`
	LastDurationMs int64           `json:"lastDurationMs"`
	LastRemoved    int64           `json:"lastRemoved"`
	LastBytes      int64           `json:"lastBytes"`
	LastMessage    string          `json:"lastMessage"`
	LastLog        *string         `json:"lastLog,omitempty"` // GET /-/api/tasks/{id} only
	Created        string          `json:"created"`
	Updated        string          `json:"updated"`
}

func (u *UI) toTaskJSON(t meta.Task, running string, withLog bool) taskJSON {
	status := "Waiting"
	switch {
	case t.ID == running:
		status = "Running"
	case !t.Enabled:
		status = "Disabled"
	}
	params := json.RawMessage(t.Params)
	if !json.Valid(params) {
		params = json.RawMessage("{}")
	}
	notify := t.NotifyEmail
	if notify == nil {
		notify = []string{}
	}
	out := taskJSON{
		ID: t.ID, Name: t.Name, Type: t.Type, TypeName: tasks.TypeName(t.Type), Status: status,
		Schedule: t.Schedule, Enabled: t.Enabled, Params: params, NotifyEmail: notify, NotifyOn: t.NotifyOn, Queued: !t.QueuedAt.IsZero(),
		NextRun: rfc3339(t.NextRun), LastRun: rfc3339(t.LastRun), LastResult: t.LastResult,
		LastDurationMs: t.LastDurationMs, LastRemoved: t.LastRemoved, LastBytes: t.LastBytes,
		LastMessage: t.LastMessage, Created: rfc3339(t.Created), Updated: rfc3339(t.Updated),
	}
	if withLog {
		out.LastLog = &t.LastLog
	}
	return out
}

// tasksAvailable answers 501 when the UI has no task service.
func (u *UI) tasksAvailable(w http.ResponseWriter) bool {
	if u.Tasks == nil {
		writeError(w, http.StatusNotImplemented, "tasks are not available")
		return false
	}
	return true
}

// writeTaskError maps task errors: validation 400, not found 404, a
// running task 409, the rest 500.
func (u *UI) writeTaskError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *repo.ValidationError
	var busy *tasks.BusyError
	switch {
	case errors.As(err, &ve):
		first := "invalid task"
		if len(ve.Errors) > 0 {
			first = ve.Errors[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": first, "errors": ve.Errors})
	case errors.Is(err, meta.ErrTaskLimit):
		msg := fmt.Sprintf("at most %d tasks can exist: delete one first", meta.MaxTasks)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg, "errors": []string{msg}})
	case errors.Is(err, tasks.ErrNotFound):
		writeError(w, http.StatusNotFound, "task not found")
	case errors.Is(err, tasks.ErrSeedTask):
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, u.busyJSON(r, busy.Error(), busy))
	default:
		u.fail(w, r, err)
	}
}

func (u *UI) listTasks(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.tasksAvailable(w) {
		return
	}
	list, err := u.Tasks.List(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	running := u.Tasks.Running()
	out := make([]taskJSON, 0, len(list))
	for _, t := range list {
		out = append(out, u.toTaskJSON(t, running, false))
	}
	type typeJSON struct {
		Type   string            `json:"type"`
		Name   string            `json:"name"`
		Params []tasks.ParamSpec `json:"params"`
	}
	types := []typeJSON{}
	for _, t := range tasks.Types() {
		types = append(types, typeJSON{t, tasks.TypeName(t), tasks.Params(t)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out, "types": types, "running": running})
}

func (u *UI) getTask(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.tasksAvailable(w) {
		return
	}
	t, err := u.Tasks.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		u.writeTaskError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u.toTaskJSON(t, u.Tasks.Running(), true))
}

func (u *UI) createTask(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.tasksAvailable(w) {
		return
	}
	var d tasks.Def
	if !decodeBody(w, r, &d) {
		return
	}
	t, err := u.Tasks.Create(r.Context(), d)
	if err != nil {
		u.writeTaskError(w, r, err)
		return
	}
	u.emit(r, "task.create", "", t.ID, fmt.Sprintf("%s (%s, %s)", t.Name, t.Type, t.Schedule))
	writeJSON(w, http.StatusCreated, u.toTaskJSON(t, u.Tasks.Running(), true))
}

func (u *UI) updateTask(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.tasksAvailable(w) {
		return
	}
	var d tasks.Def
	if !decodeBody(w, r, &d) {
		return
	}
	t, err := u.Tasks.Update(r.Context(), r.PathValue("id"), d)
	if err != nil {
		u.writeTaskError(w, r, err)
		return
	}
	state := "enabled"
	if !t.Enabled {
		state = "disabled"
	}
	u.emit(r, "task.update", "", t.ID, fmt.Sprintf("%s (%s, %s)", t.Name, t.Schedule, state))
	writeJSON(w, http.StatusOK, u.toTaskJSON(t, u.Tasks.Running(), true))
}

func (u *UI) deleteTask(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.tasksAvailable(w) {
		return
	}
	id := r.PathValue("id")
	t, err := u.Tasks.Get(r.Context(), id)
	if err == nil {
		err = u.Tasks.Delete(r.Context(), id)
	}
	if err != nil {
		u.writeTaskError(w, r, err)
		return
	}
	u.emit(r, "task.delete", "", id, fmt.Sprintf("%s (%s)", t.Name, t.Type))
	w.WriteHeader(http.StatusNoContent)
}

// runTask starts a task now and answers 202 at once; the task list shows it
// Running, then its last result. While another task runs it is queued
// instead (202 with queued true): the scheduler starts it when that run
// ends. 409 while the task itself runs.
func (u *UI) runTask(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.tasksAvailable(w) {
		return
	}
	queued, err := u.Tasks.RunOrQueue(r.PathValue("id"), principal(r).Owner())
	if err != nil {
		u.writeTaskError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": !queued, "queued": queued})
}

// runGC is the plan 7 alias of running the compact task. It waits for the
// run, as before: 200 with {removed, ms}, 409 while a task runs, 500 when
// the run failed.
func (u *UI) runGC(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	if u.Tasks == nil || u.Tasks.GC == nil {
		writeError(w, http.StatusNotImplemented, "garbage collection is not available")
		return
	}
	out, err := u.Tasks.RunCompact(principal(r).Owner())
	u.writeRun(w, r, out, err)
}

func (u *UI) writeRun(w http.ResponseWriter, r *http.Request, out tasks.Outcome, err error) {
	var busy *tasks.BusyError
	if errors.As(err, &busy) {
		writeJSON(w, http.StatusConflict, u.busyJSON(r, "a task is already running", busy))
		return
	}
	if err != nil {
		u.writeTaskError(w, r, err)
		return
	}
	if out.Err != nil {
		u.fail(w, r, fmt.Errorf("task %s: %s", out.TaskID, out.Message)) // the message is redacted
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": out.TaskID, "result": out.Result, "message": out.Message,
		"removed": out.Removed, "bytes": out.Bytes, "ms": out.DurationMs,
	})
}

// stopTask stops the running task: 202 when it was running (its run then
// records Stopped), 409 when it is not, 404 for an unknown task.
func (u *UI) stopTask(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.tasksAvailable(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := u.Tasks.Get(r.Context(), id); err != nil {
		u.writeTaskError(w, r, err)
		return
	}
	if !u.Tasks.StopRun(id) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the task is not running", "running": u.Tasks.Running()})
		return
	}
	if u.Log != nil {
		u.Log.Info("task stop requested", "task", id, "actor", principal(r).Owner())
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"stopping": true})
}

// busyJSON is the 409 body of a refused run: the running task's id and name.
func (u *UI) busyJSON(r *http.Request, msg string, busy *tasks.BusyError) map[string]any {
	out := map[string]any{"error": msg, "running": busy.Running}
	if busy.Running != "" {
		if t, err := u.Tasks.Get(r.Context(), busy.Running); err == nil {
			out["runningName"] = t.Name
		}
	}
	return out
}
