package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/importer"
	"commandcenter/internal/importer/nexus"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/tasks"
)

// Import from Nexus Repository 3 (plan 15 Decision 7).
//
//	POST /-/api/import/nexus             start an import.nexus task run, 202 {job, id, dryRun}
//	POST /-/api/import/nexus/test        connect to the source and list its repositories with support flags; writes nothing
//	GET  /-/api/import/jobs              the jobs: stored ones, the running one, the dry runs kept in memory
//	GET  /-/api/import/jobs/{job}        one job: live progress while it runs, then its report (?download=1: as a file;
//	                                     ?run=<id> of the 202 answer: about that run, so no earlier report of the job)
//	POST /-/api/import/jobs/{job}/stop   stop the running job, 202
//
// All need an admin. Start and stop accept an admin token (the CLI) or an
// interactive login with the UI header; the test call is for the UI only
// (interactive + UI header). The source password is read from the request
// body and handed to the run in memory; it is never stored, logged or
// echoed.

const (
	importJobsLimit = 200
	importTestWait  = 60 * time.Second
)

func (u *UI) importRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /-/api/import/nexus", u.startImport)
	mux.HandleFunc("POST /-/api/import/nexus/test", u.testImport)
	mux.HandleFunc("GET /-/api/import/jobs", u.listImportJobs)
	mux.HandleFunc("GET /-/api/import/jobs/{job}", u.importJob)
	mux.HandleFunc("POST /-/api/import/jobs/{job}/stop", u.stopImport)
}

func (u *UI) importAvailable(w http.ResponseWriter) bool {
	if u.Import == nil || u.Tasks == nil {
		writeError(w, http.StatusNotImplemented, "import is not available")
		return false
	}
	return true
}

// requireImportCaller allows an admin token, or an admin who logged in
// interactively and sends the UI header.
func requireImportCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := requireAdmin(w, r)
	if !ok {
		return p, false
	}
	switch p.Via {
	case auth.ViaToken:
		return p, true
	case auth.ViaPassword, auth.ViaSession:
		return p, requireUIHeader(w, r)
	}
	writeError(w, http.StatusForbidden, "this action requires an admin token or an interactive login")
	return p, false
}

// importRequest is the body of POST /-/api/import/nexus. Password is
// separated before the params are stored.
type importRequest struct {
	URL                string   `json:"url"`
	Username           string   `json:"username"`
	Password           string   `json:"password"`
	InsecureSkipVerify bool     `json:"insecureSkipVerify"`
	Repositories       []string `json:"repositories"`
	ImportDefinitions  *bool    `json:"importDefinitions"` // default true
	ImportContent      *bool    `json:"importContent"`     // default true
	BlobStore          string   `json:"blobStore"`
	RenamePrefix       string   `json:"renamePrefix"`
	Job                string   `json:"job"`
	Concurrency        int      `json:"concurrency"`
	DryRun             bool     `json:"dryRun"`
	RetryFailed        bool     `json:"retryFailed"`
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func (u *UI) startImport(w http.ResponseWriter, r *http.Request) {
	p, ok := requireImportCaller(w, r)
	if !ok || !u.importAvailable(w) {
		return
	}
	var req importRequest
	if !decodeBody(w, r, &req) {
		return
	}
	params := importer.Params{URL: req.URL, Username: req.Username, InsecureSkipVerify: req.InsecureSkipVerify,
		Repositories: req.Repositories, ImportDefinitions: boolOr(req.ImportDefinitions, true), ImportContent: boolOr(req.ImportContent, true),
		BlobStore: req.BlobStore, RenamePrefix: req.RenamePrefix, Job: req.Job, Concurrency: req.Concurrency,
		DryRun: req.DryRun, RetryFailed: req.RetryFailed}
	raw, _ := json.Marshal(params) // no password
	id, job, err := u.Tasks.StartImport(raw, req.Password, p.Owner())
	req.Password = ""
	if err != nil {
		var ve *repo.ValidationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.Join(ve.Errors, "; "), "errors": ve.Errors})
			return
		}
		u.writeTaskError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "job": job, "dryRun": params.DryRun, "started": true})
}

func (u *UI) testImport(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.importAvailable(w) {
		return
	}
	var req struct {
		URL                string `json:"url"`
		Username           string `json:"username"`
		Password           string `json:"password"`
		InsecureSkipVerify bool   `json:"insecureSkipVerify"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	// Only the source fields are checked; the rest gets valid placeholders.
	p, errs := importer.Params{URL: req.URL, Username: req.Username, InsecureSkipVerify: req.InsecureSkipVerify,
		ImportDefinitions: true, Job: "test"}.Normalize(time.Now(), nil)
	if len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.Join(errs, "; "), "errors": errs})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), importTestWait)
	defer cancel()
	list, err := u.Import.Test(ctx, p, importer.Credentials{Password: req.Password})
	req.Password = ""
	if err != nil {
		status := http.StatusBadGateway
		if !errors.As(err, new(*nexus.Error)) {
			status = http.StatusBadRequest
		}
		// The client's errors are safe: no credentials, no response bodies.
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": list})
}

// importJobJSON is a job as the API lists it.
func importJobJSON(j meta.ImportJob) map[string]any {
	var params importer.Params
	_ = json.Unmarshal([]byte(j.Params), &params)
	return map[string]any{"job": j.Job, "source": j.Source, "status": j.Status, "actor": j.Actor,
		"started": rfc3339(j.Started), "finished": rfc3339(j.Finished), "dryRun": false, "params": params}
}

func (u *UI) listImportJobs(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.importAvailable(w) {
		return
	}
	stored, err := u.DB.ListImportJobs(r.Context(), importJobsLimit)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	jobs := make([]map[string]any, 0, len(stored))
	for _, j := range stored {
		jobs = append(jobs, importJobJSON(j))
	}
	dry := make([]map[string]any, 0)
	for _, rep := range u.Import.DryRuns() {
		dry = append(dry, map[string]any{"job": rep.Job, "source": rep.Source, "status": rep.Status, "dryRun": true,
			"started": rfc3339(rep.Started), "finished": rfc3339(rep.Finished)})
	}
	res := map[string]any{"jobs": jobs, "dryRuns": dry, "running": nil}
	if prog, ok := u.Import.Running(); ok {
		res["running"] = prog
	}
	writeJSON(w, http.StatusOK, res)
}

func (u *UI) importJob(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.importAvailable(w) {
		return
	}
	job := r.PathValue("job")
	if !importer.ValidJob(job) {
		writeError(w, http.StatusNotFound, "import job not found")
		return
	}
	download := r.URL.Query().Get("download") != ""
	// ?run=<id> (the id of the 202 answer) ties the answer to that run: while
	// the run starts it is "running" (not an earlier report of the job), and
	// a run that ended without a report answers with its outcome.
	var runOut tasks.Outcome
	runKnown, runRunning := false, false
	if run := r.URL.Query().Get("run"); strings.HasPrefix(run, tasks.TypeImportNexus+"-") {
		runOut, runRunning, runKnown = u.Tasks.AdHoc(run)
	}
	if prog, ok := u.Import.Running(); ok && prog.Job == job {
		res := map[string]any{"job": job, "status": importer.StatusRunning, "dryRun": prog.DryRun, "progress": prog}
		if j, err := u.DB.GetImportJob(r.Context(), job); err == nil {
			res["source"] = j.Source
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	if runKnown && runRunning {
		writeJSON(w, http.StatusOK, map[string]any{"job": job, "status": importer.StatusRunning, "starting": true})
		return
	}
	var res map[string]any
	j, err := u.DB.GetImportJob(r.Context(), job)
	// A dry run newer than the stored run of the job is what was asked last.
	dry, hasDry := u.Import.DryRun(job)
	if err == nil && hasDry && dry.Started.After(j.Started) {
		err = meta.ErrNotFound
	}
	if runKnown {
		// Reports older than the run are from earlier runs of the job (stored
		// times have millisecond precision).
		runStart := runOut.Start.Truncate(time.Millisecond)
		if err == nil && j.Started.Before(runStart) {
			err = meta.ErrNotFound
		}
		if hasDry && dry.Started.Before(runStart) {
			hasDry = false
		}
		if errors.Is(err, meta.ErrNotFound) && !hasDry {
			writeJSON(w, http.StatusOK, map[string]any{"job": job, "status": importer.StatusFailed, "message": runOut.Message, "noReport": true})
			return
		}
	}
	switch {
	case err == nil:
		res = importJobJSON(j)
		if j.Report != "" {
			res["report"] = json.RawMessage(j.Report)
		}
	case errors.Is(err, meta.ErrNotFound):
		rep, ok := dry, hasDry
		if !ok {
			writeError(w, http.StatusNotFound, "import job not found")
			return
		}
		res = map[string]any{"job": rep.Job, "source": rep.Source, "status": rep.Status, "dryRun": true,
			"started": rfc3339(rep.Started), "finished": rfc3339(rep.Finished), "report": rep}
	default:
		u.fail(w, r, err)
		return
	}
	if download {
		w.Header().Set("Content-Disposition", `attachment; filename="import-`+job+`.json"`)
	}
	writeJSON(w, http.StatusOK, res)
}

func (u *UI) stopImport(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireImportCaller(w, r); !ok || !u.importAvailable(w) {
		return
	}
	job := r.PathValue("job")
	prog, ok := u.Import.Running()
	if !ok || prog.Job != job {
		writeError(w, http.StatusConflict, "import job "+clipName(job)+" is not running")
		return
	}
	run := u.Tasks.Running()
	if run == "" || run != tasks.AdHocTaskID(tasks.TypeImportNexus) && !strings.HasPrefix(run, tasks.TypeImportNexus+"-") {
		writeError(w, http.StatusConflict, "import job "+clipName(job)+" is not running")
		return
	}
	if !u.Tasks.StopRun(run) {
		writeError(w, http.StatusConflict, "import job "+clipName(job)+" is not running")
		return
	}
	u.emit(r, "import.stop", "", job, "")
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "stopping": true})
}

func clipName(s string) string {
	if len(s) > 100 {
		return s[:100]
	}
	return s
}
