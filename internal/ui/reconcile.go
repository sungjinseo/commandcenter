package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Recovery mode and the reconcile plans (tasks blobstore.reconcile-plan and
// blobstore.reconcile-execute). Reads are admin only; changes need an
// interactive admin and the UI header, as the task API does.

func (u *UI) reconcileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/recovery", u.getRecovery)
	mux.HandleFunc("PUT /-/api/recovery", u.putRecovery)
	mux.HandleFunc("GET /-/api/reconcile/plans", u.listPlans)
	mux.HandleFunc("GET /-/api/reconcile/plans/{id}", u.getPlan)
	mux.HandleFunc("GET /-/api/reconcile/plans/{id}/items", u.planItems)
	mux.HandleFunc("DELETE /-/api/reconcile/plans/{id}", u.deletePlan)
}

type recoveryJSON struct {
	Enabled              bool               `json:"enabled"`
	Since                string             `json:"since"`
	By                   string             `json:"by"`
	Message              string             `json:"message"`
	JournalRetentionDays int                `json:"journalRetentionDays"`
	Journal              *repo.JournalStats `json:"journal,omitempty"` // this node's
}

func (u *UI) recoveryDoc(r *http.Request) (recoveryJSON, error) {
	rm, err := u.DB.LoadRecoveryMode(r.Context())
	if err != nil {
		return recoveryJSON{}, err
	}
	days, err := u.DB.JournalDays(r.Context())
	if err != nil {
		return recoveryJSON{}, err
	}
	out := recoveryJSON{Enabled: rm.Enabled, Since: rfc3339(rm.Since), By: rm.By, Message: rm.Message, JournalRetentionDays: days}
	if u.Store != nil && u.Store.Journal != nil {
		st := u.Store.Journal.Stats()
		out.Journal = &st
	}
	return out, nil
}

func (u *UI) getRecovery(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	doc, err := u.recoveryDoc(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// putRecovery turns recovery mode on or off and sets the journal's
// retention; a field left out keeps its value.
func (u *UI) putRecovery(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var req struct {
		Enabled              *bool   `json:"enabled"`
		Message              *string `json:"message"`
		JournalRetentionDays *int    `json:"journalRetentionDays"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if d := req.JournalRetentionDays; d != nil && (*d < 1 || *d > meta.MaxJournalDays) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("journalRetentionDays must be between 1 and %d", meta.MaxJournalDays))
		return
	}
	if req.Message != nil && len(*req.Message) > 500 {
		writeError(w, http.StatusBadRequest, "message takes at most 500 characters")
		return
	}
	ctx := r.Context()
	if d := req.JournalRetentionDays; d != nil {
		if err := u.DB.SetJournalDays(ctx, *d); err != nil {
			u.fail(w, r, err)
			return
		}
		u.emit(r, "settings.update", "", "reconcileJournalDays", strconv.Itoa(*d))
	}
	if req.Enabled != nil || req.Message != nil {
		rm, err := u.DB.LoadRecoveryMode(ctx)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		was := rm.Enabled
		if req.Enabled != nil {
			rm.Enabled = *req.Enabled
		}
		if req.Message != nil {
			rm.Message = *req.Message
		}
		switch {
		case rm.Enabled && !was:
			rm.Since, rm.By = time.Now().UTC().Truncate(time.Second), principal(r).Owner()
		case !rm.Enabled:
			rm = meta.RecoveryMode{}
		}
		if err := u.DB.SetRecoveryMode(ctx, rm); err != nil {
			u.fail(w, r, err)
			return
		}
		state := "off"
		if rm.Enabled {
			state = "on"
		}
		u.emit(r, "settings.update", "", "recoveryMode", state)
		if u.Log != nil && rm.Enabled != was {
			u.Log.Warn("recovery mode turned "+state, "actor", principal(r).Owner())
		}
	}
	doc, err := u.recoveryDoc(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

type planJSON struct {
	ID        string          `json:"id"`
	BlobStore string          `json:"blobStore"` // "" for every blob store
	State     string          `json:"state"`
	Since     string          `json:"since"` // "" for the whole journal
	Created   string          `json:"created"`
	CreatedBy string          `json:"createdBy"`
	Executed  string          `json:"executed"`
	Summary   json.RawMessage `json:"summary"`
}

func toPlanJSON(p meta.ReconcilePlan) planJSON {
	sum := json.RawMessage(p.Summary)
	if !json.Valid(sum) {
		sum = json.RawMessage("{}")
	}
	return planJSON{ID: p.ID, BlobStore: p.BlobStore, State: p.State, Since: rfc3339(p.Since), Created: rfc3339(p.Created),
		CreatedBy: p.CreatedBy, Executed: rfc3339(p.Executed), Summary: sum}
}

// pageLimit reads ?limit= (default 100, at most 1000).
func pageLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 100, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1000 {
		writeError(w, http.StatusBadRequest, "limit must be between 1 and 1000")
		return 0, false
	}
	return n, true
}

func (u *UI) listPlans(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	plans, next, err := u.DB.ListReconcilePlans(r.Context(), r.URL.Query().Get("continuationToken"), limit)
	if errors.Is(err, meta.ErrBadToken) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]planJSON, 0, len(plans))
	for _, p := range plans {
		out = append(out, toPlanJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "continuationToken": nullable(next)})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (u *UI) getPlan(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	p, err := u.DB.GetReconcilePlan(r.Context(), r.PathValue("id"))
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "reconcile plan not found")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPlanJSON(p))
}

type planItemJSON struct {
	Seq         int64  `json:"seq"`
	Repository  string `json:"repository"`
	Kind        string `json:"kind"` // asset, package or tag
	Path        string `json:"path"` // an asset's path, name@version or name:tag
	BlobID      string `json:"blobId"`
	DatePathRef string `json:"datePathRef"` // the journal record's key
	At          string `json:"at"`          // its commit time
	Action      string `json:"action"`
	State       string `json:"state"`
	Reason      string `json:"reason"`
}

var (
	itemActions = map[string]bool{meta.ItemAdd: true, meta.ItemDelete: true, meta.ItemSkip: true, meta.ItemConflict: true}
	itemStates  = map[string]bool{meta.ItemPlanned: true, meta.ItemDone: true, meta.ItemFailed: true, meta.ItemSkipped: true}
)

// planItems pages a plan's items in execution order, optionally of one
// action and state; continuationToken is the last seq of the previous page.
func (u *UI) planItems(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := meta.ItemFilter{Action: q.Get("action"), State: q.Get("state")}
	if f.Action != "" && !itemActions[f.Action] || f.State != "" && !itemStates[f.State] {
		writeError(w, http.StatusBadRequest, "action is ADD, DELETE, SKIP or CONFLICT; state is PLANNED, DONE, FAILED or SKIPPED")
		return
	}
	var after int64
	if t := q.Get("continuationToken"); t != "" {
		var err error
		if after, err = strconv.ParseInt(t, 10, 64); err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, "invalid continuationToken")
			return
		}
	}
	id := r.PathValue("id")
	if _, err := u.DB.GetReconcilePlan(r.Context(), id); errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "reconcile plan not found")
		return
	} else if err != nil {
		u.fail(w, r, err)
		return
	}
	items, err := u.DB.ReconcileItems(r.Context(), id, after, limit+1, f)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.FormatInt(items[limit-1].Seq, 10)
	}
	out := make([]planItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, planItemJSON{Seq: it.Seq, Repository: it.Repo, Kind: it.Kind, Path: it.Label, BlobID: it.BlobSHA256,
			DatePathRef: it.JournalKey, At: rfc3339(it.At), Action: it.Action, State: it.State, Reason: it.Reason})
	}
	writeJSON(w, http.StatusOK, map[string]any{"planId": id, "items": out, "continuationToken": nullable(next)})
}

func (u *UI) deletePlan(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	id := r.PathValue("id")
	err := u.DB.DeleteReconcilePlan(r.Context(), id)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "reconcile plan not found")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "reconcile.plan.delete", "", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// reconcileMetrics appends the journal counters and the recovery mode.
func (u *UI) reconcileMetrics(p *promWriter) {
	if u.Store != nil && u.Store.Journal != nil {
		st := u.Store.Journal.Stats()
		p.family("commandcenter_reconcile_journal_writes_total", "counter", "Reconcile journal records this node wrote, and failed to write.")
		p.sample("commandcenter_reconcile_journal_writes_total", st.Written, "result", "ok")
		p.sample("commandcenter_reconcile_journal_writes_total", st.Failed, "result", "failed")
		p.family("commandcenter_reconcile_journal_queued", "gauge", "Reconcile journal records waiting to be written.")
		p.sample("commandcenter_reconcile_journal_queued", st.Queued)
	}
	v := 0
	if u.DB.Recovery().Enabled {
		v = 1
	}
	p.family("commandcenter_recovery_mode", "gauge", "1 while recovery mode refuses uploads and deletes.")
	p.sample("commandcenter_recovery_mode", v)
}
