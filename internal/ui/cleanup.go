package ui

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Cleanup policy API (plan 11 Decision 5). Reads are admin only; mutations
// need an interactive admin and the UI header.

// Preview caps; variables so tests can lower them.
var (
	previewLimit      = 500                       // components listed
	previewScanBudget = cleanup.DefaultScanBudget // assets read per repository
)

// previewTimeout bounds one preview, the wait for a heavy-query slot
// included; a variable so tests can lower it.
var previewTimeout = 30 * time.Second

// statusClientClosed answers a preview whose client went away (nginx's 499);
// nobody reads it, but it keeps such requests out of the 5xx counts.
const statusClientClosed = 499

// previewFailed answers a failed preview: the client's cancel is 499 and not
// logged, our own timeout is 504, anything else goes to fail (500).
func (u *UI) previewFailed(w http.ResponseWriter, r *http.Request, ctx context.Context, err error) {
	switch {
	case r.Context().Err() != nil:
		writeError(w, statusClientClosed, "request cancelled by the client")
	case ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		if u.Log != nil {
			u.Log.Warn("cleanup preview timed out", "id", r.Header.Get("X-Request-Id"), "path", r.URL.Path, "timeout", previewTimeout)
		}
		writeError(w, http.StatusGatewayTimeout, fmt.Sprintf("the preview did not finish within %s: preview one repository with ?repo=", previewTimeout))
	default:
		u.fail(w, r, err)
	}
}

func (u *UI) cleanupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/cleanup/policies", u.listPolicies)
	mux.HandleFunc("POST /-/api/cleanup/policies", u.createPolicy)
	mux.HandleFunc("GET /-/api/cleanup/policies/{name}", u.getPolicy)
	mux.HandleFunc("PUT /-/api/cleanup/policies/{name}", u.updatePolicy)
	mux.HandleFunc("DELETE /-/api/cleanup/policies/{name}", u.deletePolicy)
	mux.HandleFunc("GET /-/api/cleanup/policies/{name}/preview", u.previewPolicy)
	mux.HandleFunc("POST /-/api/cleanup/preview", u.previewDraft)
}

// policyRequest is the body of POST and PUT. On PUT, name may be omitted
// and must otherwise be the policy's own.
type policyRequest struct {
	Name               string        `json:"name"`
	Format             string        `json:"format"`
	ComponentAgeDays   int           `json:"componentAgeDays"`
	LastDownloadedDays int           `json:"lastDownloadedDays"`
	ReleaseType        string        `json:"releaseType"`
	NamePattern        string        `json:"namePattern"`
	PathPattern        string        `json:"pathPattern"`
	Retain             int           `json:"retain"`
	RetainSortBy       string        `json:"retainSortBy"`
	Exclude            exclusionJSON `json:"exclude"`
	Notes              string        `json:"notes"`
}

type exclusionJSON struct {
	NamePattern    string `json:"namePattern"`
	VersionPattern string `json:"versionPattern"`
}

func (b policyRequest) policy() cleanup.Policy {
	return cleanup.Policy{Name: b.Name, Format: b.Format, Notes: b.Notes, Criteria: cleanup.Criteria{
		ComponentAgeDays: b.ComponentAgeDays, LastDownloadedDays: b.LastDownloadedDays,
		ReleaseType: b.ReleaseType, NamePattern: b.NamePattern, PathPattern: b.PathPattern,
		Retain: b.Retain, RetainSortBy: b.RetainSortBy,
		Exclude: cleanup.Exclusion{NamePattern: b.Exclude.NamePattern, VersionPattern: b.Exclude.VersionPattern},
	}}
}

type policyJSON struct {
	Name               string        `json:"name"`
	Format             string        `json:"format"`
	ComponentAgeDays   int           `json:"componentAgeDays"`
	LastDownloadedDays int           `json:"lastDownloadedDays"`
	ReleaseType        string        `json:"releaseType"`
	NamePattern        string        `json:"namePattern"`
	PathPattern        string        `json:"pathPattern"`
	Retain             int           `json:"retain"`
	RetainSortBy       string        `json:"retainSortBy"` // "" when retain is 0
	Exclude            exclusionJSON `json:"exclude"`
	Notes              string        `json:"notes"`
	Created            string        `json:"created"`
	Updated            string        `json:"updated"`
	Repositories       []string      `json:"repositories"` // connected repositories, sorted
}

// policyUsers maps each policy name to the repositories using it, from s.
func policyUsers(s *repo.Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, rp := range s.Sorted {
		if rp.CleanupPolicy != "" {
			out[rp.CleanupPolicy] = append(out[rp.CleanupPolicy], rp.Name)
		}
	}
	return out
}

func toPolicyJSON(p cleanup.Policy, users []string) policyJSON {
	return policyJSON{
		Name: p.Name, Format: p.Format,
		ComponentAgeDays: p.ComponentAgeDays, LastDownloadedDays: p.LastDownloadedDays,
		ReleaseType: p.ReleaseType, NamePattern: p.NamePattern, PathPattern: p.PathPattern,
		Retain: p.Retain, RetainSortBy: p.RetainSortBy,
		Exclude: exclusionJSON{NamePattern: p.Exclude.NamePattern, VersionPattern: p.Exclude.VersionPattern},
		Notes:   p.Notes, Created: rfc3339(p.Created), Updated: rfc3339(p.Updated),
		Repositories: append([]string{}, users...),
	}
}

// writePolicyError maps policy errors: validation 400, not found 404, a
// name taken or a policy in use 409, the rest 500.
func (u *UI) writePolicyError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *repo.ValidationError
	var inUse *meta.PolicyInUseError
	var he *repo.Error
	switch {
	case errors.As(err, &ve):
		first := "invalid cleanup policy"
		if len(ve.Errors) > 0 {
			first = ve.Errors[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": first, "errors": ve.Errors})
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "cleanup policy not found")
	case errors.Is(err, meta.ErrPolicyLimit):
		msg := fmt.Sprintf("at most %d cleanup policies can exist: delete one first", meta.MaxCleanupPolicies)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg, "errors": []string{msg}})
	case errors.Is(err, meta.ErrExists):
		writeError(w, http.StatusConflict, "a cleanup policy with this name already exists")
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": inUse.Error(), "repositories": inUse.Repos})
	case errors.As(err, &he):
		writeError(w, he.Status, he.Msg)
	default:
		u.fail(w, r, err)
	}
}

func (u *UI) listPolicies(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	list, err := cleanup.ListPolicies(r.Context(), u.DB)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	users := policyUsers(u.Repos.Snapshot())
	out := make([]policyJSON, 0, len(list))
	for _, p := range list {
		out = append(out, toPolicyJSON(p, users[p.Name]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

func (u *UI) getPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	p, err := cleanup.GetPolicy(r.Context(), u.DB, r.PathValue("name"))
	if err != nil {
		u.writePolicyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPolicyJSON(p, policyUsers(u.Repos.Snapshot())[p.Name]))
}

func (u *UI) createPolicy(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body policyRequest
	if !decodeBody(w, r, &body) {
		return
	}
	p := body.policy()
	if err := cleanup.CreatePolicy(r.Context(), u.DB, p); err != nil {
		u.writePolicyError(w, r, err)
		return
	}
	stored, err := cleanup.GetPolicy(r.Context(), u.DB, p.Name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "policy.create", "", stored.Name, policySummary(stored))
	writeJSON(w, http.StatusCreated, toPolicyJSON(stored, nil))
}

// updatePolicy replaces a policy's format and criteria. A format change that
// would no longer match a connected repository is refused with 409; the
// registry's write lock keeps repositories from being connected meanwhile.
func (u *UI) updatePolicy(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	var body policyRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Name != "" && body.Name != name {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a cleanup policy cannot be renamed",
			"errors": []string{"a cleanup policy cannot be renamed"}})
		return
	}
	body.Name = name
	p := body.policy()
	var users []string
	err := u.Repos.WithWriteLock(r.Context(), func(s *repo.Snapshot) error {
		var mismatched []string
		for _, rp := range s.Sorted {
			if rp.CleanupPolicy != name {
				continue
			}
			users = append(users, rp.Name)
			if !cleanup.FormatMatches(p.Format, rp.Format) {
				mismatched = append(mismatched, rp.Name)
			}
		}
		if len(mismatched) > 0 {
			return repo.Errorf(http.StatusConflict, "cleanup policy %q is used by repositories of another format: %s",
				name, strings.Join(mismatched, ", "))
		}
		return cleanup.UpdatePolicy(r.Context(), u.DB, p)
	})
	if err != nil {
		u.writePolicyError(w, r, err)
		return
	}
	stored, err := cleanup.GetPolicy(r.Context(), u.DB, name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if u.Tasks != nil {
		u.Tasks.PolicyChanged() // a running cleanup reads its policy again
	}
	u.emit(r, "policy.update", "", name, policySummary(stored))
	writeJSON(w, http.StatusOK, toPolicyJSON(stored, users))
}

// deletePolicy removes a policy no repository uses (409 naming them
// otherwise). The check and the delete run in one transaction, under the
// registry's write lock.
func (u *UI) deletePolicy(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	err := u.Repos.WithWriteLock(r.Context(), func(*repo.Snapshot) error {
		return u.DB.DeleteCleanupPolicy(r.Context(), name)
	})
	if err != nil {
		u.writePolicyError(w, r, err)
		return
	}
	u.emit(r, "policy.delete", "", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// policySummary is the event detail of a policy change.
func policySummary(p cleanup.Policy) string {
	s := fmt.Sprintf("format=%s componentAgeDays=%d lastDownloadedDays=%d releaseType=%s",
		p.Format, p.ComponentAgeDays, p.LastDownloadedDays, p.ReleaseType)
	for _, f := range [][2]string{{"namePattern", p.NamePattern}, {"pathPattern", p.PathPattern},
		{"exclude.namePattern", p.Exclude.NamePattern}, {"exclude.versionPattern", p.Exclude.VersionPattern}} {
		if f[1] != "" {
			s += " " + f[0] + "=" + f[1]
		}
	}
	if p.Retain > 0 {
		s += fmt.Sprintf(" retain=%d retainSortBy=%s", p.Retain, p.RetainSortBy)
	}
	return s
}

type previewComponentJSON struct {
	Repo           string `json:"repo"`
	Group          string `json:"group"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	Created        string `json:"created"`
	LastDownloaded string `json:"lastDownloaded"`
	Assets         int    `json:"assets"`
	Bytes          int64  `json:"bytes"`
}

// previewPolicy answers GET /-/api/cleanup/policies/{name}/preview?repo=:
// the components the policy would delete from repo, or from every
// repository connected to it when repo is empty. At most previewLimit are
// listed. count and bytes cover every candidate the scan found; when
// truncated is set (the list was cut, or a repository had more assets than
// the scan budget) they are lower bounds, since unscanned assets may hold
// more candidates.
func (u *UI) previewPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	p, err := cleanup.GetPolicy(r.Context(), u.DB, r.PathValue("name"))
	if err != nil {
		if r.Context().Err() != nil {
			u.previewFailed(w, r, nil, err)
			return
		}
		u.writePolicyError(w, r, err)
		return
	}
	snap := u.Repos.Snapshot()
	var targets []*repo.Repository
	if name := r.URL.Query().Get("repo"); name != "" {
		rp := snap.ByName[name]
		if !previewable(w, rp, name, p) {
			return
		}
		targets = []*repo.Repository{rp}
	} else {
		for _, rp := range snap.Sorted {
			if rp.CleanupPolicy == p.Name {
				targets = append(targets, rp)
			}
		}
	}

	out := []previewComponentJSON{}
	var count int
	var bytes int64
	truncated := false
	if len(targets) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), previewTimeout)
		defer cancel()
		release, err := u.acquireHeavy(ctx)
		if err != nil {
			u.previewFailed(w, r, ctx, err)
			return
		}
		ev := &cleanup.Evaluator{DB: u.DB, Now: u.clock}
		err = func() error {
			defer release()
			for _, rp := range targets {
				cands, n, b, tr, err := ev.Evaluate(ctx, rp, p, previewScanBudget)
				if err != nil {
					return err
				}
				count += n
				bytes += b
				truncated = truncated || tr
				for _, c := range cands {
					if len(out) >= previewLimit {
						truncated = true
						break
					}
					out = append(out, previewComponentJSON{Repo: c.Repo, Group: c.Group, Name: c.Name, Version: c.Version,
						Created: rfc3339(c.Created), LastDownloaded: rfc3339(c.LastDownloaded), Assets: len(c.Assets), Bytes: c.Bytes})
				}
			}
			return nil
		}()
		if err != nil {
			u.previewFailed(w, r, ctx, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"components": out, "count": count, "bytes": bytes, "truncated": truncated})
}

// previewable answers 404 or 400 when policy p cannot be previewed on rp
// (named name; nil when there is none).
func previewable(w http.ResponseWriter, rp *repo.Repository, name string, p cleanup.Policy) bool {
	msg := ""
	switch {
	case rp == nil:
		writeError(w, http.StatusNotFound, "repository \""+name+"\" not found")
		return false
	case rp.Type != "hosted" && rp.Type != "proxy":
		msg = "cleanup applies to hosted and proxy repositories only"
	case rp.Format == "cargo" && rp.Type == "hosted":
		msg = "cleanup is not supported for cargo hosted repositories (yank crates, or delete versions in the web UI)"
	case rp.Format == "docker":
		msg = "cleanup is not supported for docker repositories (delete manifests in the web UI or with the registry API)"
	case !cleanup.FormatMatches(p.Format, rp.Format):
		msg = fmt.Sprintf("cleanup policy %q is for %s repositories", p.Name, p.Format)
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return false
	}
	return true
}

// previewDraftLimit caps the components a JSON draft preview lists; a
// variable so tests can lower it.
var previewDraftLimit = 1000

// previewDraftRequest is the body of POST /-/api/cleanup/preview: a policy
// as POST /-/api/cleanup/policies takes it, name optional, and the
// repository to preview it on.
type previewDraftRequest struct {
	policyRequest
	Repository string `json:"repository"`
}

type draftComponentJSON struct {
	Group          string `json:"group"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	LastUpdated    string `json:"lastUpdated"`
	LastDownloaded string `json:"lastDownloaded"`
	Assets         int    `json:"assets"`
	Bytes          int64  `json:"bytes"`
	Reason         string `json:"reason"`
}

// csvCell keeps a cell from being read as a formula by spreadsheets.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// wroteWriter records whether anything reached the client.
type wroteWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *wroteWriter) Write(b []byte) (int, error) { w.wrote = true; return w.ResponseWriter.Write(b) }

// previewDraft answers POST /-/api/cleanup/preview: the components the
// policy in the body, saved or not, would delete from the repository now,
// without deleting anything. It runs the cleanup task's own evaluation
// (tasks.Service.PreviewCleanup) over the whole repository, so a run of
// the policy deletes exactly these (unless they change first). The JSON
// lists at most previewDraftLimit components, with total and bytes over
// all of them; ?format=csv streams every one, with a header row.
func (u *UI) previewDraft(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.tasksAvailable(w) {
		return
	}
	asCSV := false
	switch r.URL.Query().Get("format") {
	case "", "json":
	case "csv":
		asCSV = true
	default:
		writeError(w, http.StatusBadRequest, "format must be json or csv")
		return
	}
	var body previewDraftRequest
	if !decodeBody(w, r, &body) {
		return
	}
	p := body.policy()
	if p.Name == "" {
		p.Name = "preview"
	}
	if err := p.Validate(); err != nil {
		u.writePolicyError(w, r, err)
		return
	}
	if body.Repository == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "repository is required", "errors": []string{"repository is required"}})
		return
	}
	rp := u.Repos.Get(body.Repository)
	if !previewable(w, rp, body.Repository, p) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), previewTimeout)
	defer cancel()
	release, err := u.acquireHeavy(ctx)
	if err != nil {
		u.previewFailed(w, r, ctx, err)
		return
	}
	defer release()

	if asCSV {
		ww := &wroteWriter{ResponseWriter: w}
		h := w.Header()
		h.Set("Content-Type", "text/csv; charset=utf-8")
		h.Set("Content-Disposition", `attachment; filename="cleanup-preview-`+rp.Name+`.csv"`)
		cw := csv.NewWriter(ww)
		cw.Write([]string{"group", "name", "version", "lastUpdated", "lastDownloaded", "assets", "bytes", "reason"})
		err := u.Tasks.PreviewCleanup(ctx, rp, p, func(c cleanup.Candidate) error {
			return cw.Write([]string{csvCell(c.Group), csvCell(c.Name), csvCell(c.Version), rfc3339(c.LastUpdated), rfc3339(c.LastDownloaded),
				strconv.Itoa(len(c.Assets)), strconv.FormatInt(c.Bytes, 10), csvCell(c.Reason)})
		})
		if err == nil {
			cw.Flush()
			err = cw.Error()
		}
		if err != nil {
			if !ww.wrote {
				h.Del("Content-Disposition")
				u.previewFailed(w, r, ctx, err)
				return
			}
			panic(http.ErrAbortHandler) // cut the download: a short CSV must not look complete
		}
		return
	}
	out := []draftComponentJSON{}
	total, bytes := 0, int64(0)
	err = u.Tasks.PreviewCleanup(ctx, rp, p, func(c cleanup.Candidate) error {
		total++
		bytes += c.Bytes
		if len(out) < previewDraftLimit {
			out = append(out, draftComponentJSON{Group: c.Group, Name: c.Name, Version: c.Version, LastUpdated: rfc3339(c.LastUpdated),
				LastDownloaded: rfc3339(c.LastDownloaded), Assets: len(c.Assets), Bytes: c.Bytes, Reason: c.Reason})
		}
		return nil
	})
	if err != nil {
		u.previewFailed(w, r, ctx, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"components": out, "total": total, "bytes": bytes, "truncated": total > len(out)})
}
