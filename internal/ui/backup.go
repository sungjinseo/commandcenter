package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/backup"
	"commandcenter/internal/repo"
	"commandcenter/internal/tasks"
)

// Online backup API (plan 14 Decision 3).
//
//	GET    /-/api/backups                  list (admin; tokens allowed): {backups, dir, running, runningKind}; a directory without a
//	                                       manifest has incomplete: true; running is the ad-hoc run id (runningKind "run", poll
//	                                       /-/api/backup/runs/{id}) or a task id (runningKind "task", poll /-/api/tasks/{id})
//	POST   /-/api/backup                   start a backup task run, 202 {id} (admin; tokens allowed, sessions need the UI header)
//	GET    /-/api/backup/runs/{id}         the run's state (admin; tokens allowed): what `commandcenter backup --wait` polls
//	POST   /-/api/backups/{name}/verify    size verify, synchronous 200; ?deep=true starts a backup.verify task run, 202 {id} (interactive admin + UI header)
//	DELETE /-/api/backups/{name}           delete a backup and sweep the pool, 200 {name, sweptBlobs, sweptBytes, sweepSkipped} (interactive admin + UI header)
//
// Starting a backup accepts token principals so that scripts and cron can
// run it through the CLI, like the plan 14 delete API; deleting and
// verifying stay interactive. A token's keep and deep are ignored (the
// default or the interactively stored value applies), so a script cannot
// make a backup prune or repair.

func (u *UI) backupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/backups", u.listBackups)
	mux.HandleFunc("POST /-/api/backup", u.startBackup)
	mux.HandleFunc("GET /-/api/backup/runs/{id}", u.backupRun)
	mux.HandleFunc("POST /-/api/backups/{name}/verify", u.verifyBackup)
	mux.HandleFunc("DELETE /-/api/backups/{name}", u.deleteBackup)
}

// backupsAvailable answers 501 when the UI has no backup service.
func (u *UI) backupsAvailable(w http.ResponseWriter) bool {
	if u.Backups == nil || u.Tasks == nil {
		writeError(w, http.StatusNotImplemented, "backup is not available")
		return false
	}
	if err := u.Backups.Disabled; err != nil { // PostgreSQL: backup.ErrPostgres
		writeError(w, http.StatusNotImplemented, err.Error())
		return false
	}
	return true
}

// runningBackup returns what the backup page can follow: the id of the ad-hoc
// backup or verify run in progress (kind "run", polled at
// /-/api/backup/runs/{id}), or the id of the running backup task of a schedule
// or "run now" (kind "task", polled at /-/api/tasks/{id}); "" when neither runs.
func (u *UI) runningBackup(r *http.Request) (id, kind string) {
	if run := u.Tasks.RunningRun(); run != "" {
		if strings.HasPrefix(run, tasks.TypeBackup+"-") || strings.HasPrefix(run, tasks.TypeBackupVerify+"-") {
			return run, "run"
		}
		return "", ""
	}
	id = u.Tasks.Running()
	if id == "" {
		return "", ""
	}
	if t, err := u.Tasks.Get(r.Context(), id); err == nil && (t.Type == tasks.TypeBackup || t.Type == tasks.TypeBackupVerify) {
		return id, "task"
	}
	return "", ""
}

func (u *UI) listBackups(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	// On PostgreSQL the page shows why there are no online backups.
	if u.Backups != nil && u.Backups.Disabled != nil {
		writeJSON(w, http.StatusOK, map[string]any{"backups": []any{}, "unsupported": u.Backups.Disabled.Error()})
		return
	}
	if !u.backupsAvailable(w) {
		return
	}
	list, err := u.Backups.ListAll()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	running, kind := u.runningBackup(r)
	writeJSON(w, http.StatusOK, map[string]any{"backups": list, "dir": u.Backups.Dir, "running": running, "runningKind": kind})
}

func (u *UI) startBackup(w http.ResponseWriter, r *http.Request) {
	p, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if p.Via == auth.ViaSession && !requireUIHeader(w, r) {
		return
	}
	if !u.backupsAvailable(w) {
		return
	}
	// The body is optional: none means the defaults.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var params json.RawMessage
	if b := bytes.TrimSpace(body); len(b) > 0 {
		if !json.Valid(b) {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		params = b
	}
	// A token (a script) cannot change what is destructive: the task service
	// ignores its keep and deep.
	id, err := u.Tasks.StartAdHocAs(tasks.TypeBackup, params, p.Owner(), p.Via != auth.ViaToken)
	if err != nil {
		var ve *repo.ValidationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.Join(ve.Errors, "; "), "errors": ve.Errors})
			return
		}
		u.writeTaskError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "started": true})
}

func (u *UI) backupRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.backupsAvailable(w) {
		return
	}
	id := r.PathValue("id")
	if !strings.HasPrefix(id, tasks.TypeBackup+"-") && !strings.HasPrefix(id, tasks.TypeBackupVerify+"-") {
		writeError(w, http.StatusNotFound, "backup run not found")
		return
	}
	out, running, ok := u.Tasks.AdHoc(id)
	if !ok {
		writeError(w, http.StatusNotFound, "backup run not found")
		return
	}
	res := map[string]any{"id": id, "status": "Running"}
	if !running {
		res["status"] = out.Result
		res["message"] = out.Message
		res["durationMs"] = out.DurationMs
		res["bytes"] = out.Bytes
	}
	writeJSON(w, http.StatusOK, res)
}

// writeBackupError maps backup errors: invalid 400, not found 404, busy 409.
func (u *UI) writeBackupError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, backup.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, backup.ErrNotFound):
		writeError(w, http.StatusNotFound, "backup not found")
	case errors.Is(err, backup.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	default:
		u.fail(w, r, err)
	}
}

func (u *UI) verifyBackup(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.backupsAvailable(w) {
		return
	}
	deep := false
	if v := r.URL.Query().Get("deep"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "deep must be true or false")
			return
		}
		deep = b
	}
	name := r.PathValue("name")
	if deep {
		// A deep verify re-hashes every blob and repairs the pool: a task run
		// (backup.verify) under the run lock; poll /-/api/backup/runs/{id}.
		if err := u.Backups.Exists(name); err != nil {
			u.writeBackupError(w, r, err)
			return
		}
		params, _ := json.Marshal(map[string]string{"name": name})
		id, err := u.Tasks.StartAdHoc(tasks.TypeBackupVerify, params, principal(r).Owner())
		if err != nil {
			u.writeTaskError(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "started": true})
		return
	}
	res, err := u.Backups.Verify(r.Context(), name, false)
	if err != nil {
		u.writeBackupError(w, r, err)
		return
	}
	state := "OK"
	if !res.OK {
		state = "Failed"
	}
	u.emit(r, "backup.verify", "", name, fmt.Sprintf("%s: deep=%v blobs=%d missing=%d corrupt=%d", state, res.Deep, res.Blobs, res.Missing, res.Corrupt))
	writeJSON(w, http.StatusOK, res)
}

func (u *UI) deleteBackup(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.backupsAvailable(w) {
		return
	}
	name := r.PathValue("name")
	res, err := u.Backups.Delete(r.Context(), name)
	if err != nil {
		u.writeBackupError(w, r, err)
		return
	}
	detail := fmt.Sprintf("sweptBlobs=%d sweptBytes=%d", res.SweptBlobs, res.SweptBytes)
	if res.SweepSkipped != "" {
		detail = res.SweepSkipped
	}
	u.emit(r, "backup.delete", "", name, detail)
	writeJSON(w, http.StatusOK, res)
}
