package ui

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"commandcenter/internal/logring"
	"commandcenter/internal/repo"
)

const (
	eventsDefault = 50
	eventsLimit   = 200
	logsDefault   = 200
	logsLimit     = 2000
)

func (u *UI) activityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/events", u.listEvents)
	mux.HandleFunc("GET /-/api/logs", u.listLogs)
	mux.HandleFunc("GET /-/api/logs/download", u.downloadLogs)
}

// emit records an activity event for the signed-in caller of r. repoName may
// be "". It does nothing when no sink is set.
func (u *UI) emit(r *http.Request, kind, repoName, subject, detail string) {
	if u.Events == nil {
		return
	}
	repo.EmitContext(r.Context(), u.Events, kind, principal(r).Owner(), repoName, subject, detail)
}

// GET /-/api/events?limit=50&before=<id>&kind= (admin): newest first. "next"
// is the id to pass as before for the following page; it is absent on the last
// page (plan 11 Decision 3).
func (u *UI) listEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	limit, ok := intParam(w, q.Get("limit"), eventsDefault, 1, eventsLimit, "limit")
	if !ok {
		return
	}
	before, ok := intParam(w, q.Get("before"), 0, 0, 1<<62, "before")
	if !ok {
		return
	}
	evs, err := u.DB.ListEvents(r.Context(), limit+1, int64(before), q.Get("kind"))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	var next int64
	if len(evs) > limit {
		evs = evs[:limit]
		next = evs[limit-1].ID
	}
	type row struct {
		ID      int64  `json:"id"`
		At      string `json:"at"`
		Kind    string `json:"kind"`
		Actor   string `json:"actor"`
		Repo    string `json:"repo"`
		Subject string `json:"subject"`
		Detail  string `json:"detail"`
		IP      string `json:"ip"`
		Node    string `json:"node"`
	}
	out := make([]row, 0, len(evs))
	for _, e := range evs {
		out = append(out, row{e.ID, rfc3339(e.At), e.Kind, e.Actor, e.Repo, e.Subject, e.Detail, e.IP, e.Node})
	}
	res := map[string]any{"events": out}
	if next != 0 {
		res["next"] = next
	}
	writeJSON(w, http.StatusOK, res)
}

// intParam parses an optional integer query parameter in [min, max].
func intParam(w http.ResponseWriter, s string, def, min, max int, name string) (int, bool) {
	if s == "" {
		return def, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min {
		writeError(w, http.StatusBadRequest, name+" must be a number >= "+strconv.Itoa(min))
		return 0, false
	}
	if n > max {
		n = max
	}
	return n, true
}

// logFilter reads the level and q parameters shared by the log endpoints.
func logFilter(w http.ResponseWriter, r *http.Request) (slog.Level, string, bool) {
	q := r.URL.Query()
	lvl, ok := logring.ParseLevel(q.Get("level"))
	if !ok {
		writeError(w, http.StatusBadRequest, "level must be DEBUG, INFO, WARN or ERROR")
		return 0, "", false
	}
	return lvl, q.Get("q"), true
}

// GET /-/api/logs?level=INFO|WARN|ERROR&q=&limit= (admin): recent log records
// from the in-memory ring, newest first (plan 11 Decision 4).
func (u *UI) listLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	lvl, q, ok := logFilter(w, r)
	if !ok {
		return
	}
	limit, ok := intParam(w, r.URL.Query().Get("limit"), logsDefault, 1, logsLimit, "limit")
	if !ok {
		return
	}
	type row struct {
		Time    string `json:"time"`
		Level   string `json:"level"`
		Message string `json:"message"`
		Attrs   string `json:"attrs"`
	}
	out := []row{}
	if u.Logs != nil {
		for _, rec := range u.Logs.Snapshot(lvl, q, limit) {
			out = append(out, row{rec.Time.UTC().Format(time.RFC3339Nano), rec.Level.String(), rec.Msg, rec.Attrs})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": out})
}

// GET /-/api/logs/download (admin): the ring as a text file, oldest first, so
// the newest line is last. The level and q filters apply as for the list.
func (u *UI) downloadLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	lvl, q, ok := logFilter(w, r)
	if !ok {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="commandcenter.log"`)
	if u.Logs == nil {
		return
	}
	recs := u.Logs.Snapshot(lvl, q, 0) // newest first
	for i := len(recs) - 1; i >= 0; i-- {
		if _, err := w.Write([]byte(recs[i].String() + "\n")); err != nil {
			return
		}
	}
}
