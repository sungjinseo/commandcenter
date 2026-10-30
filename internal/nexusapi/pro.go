package nexusapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/tasks"
)

// The endpoints Nexus Repository Pro has: cleanup policies, task create,
// update and delete, and the system information report. As everywhere in
// this package, writes are in-process calls of the web UI's JSON API.

func (h *Handler) proRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/cleanup-policies", h.listCleanupPolicies)
	m.HandleFunc("POST "+p+"/cleanup-policies", h.createCleanupPolicy)
	m.HandleFunc("GET "+p+"/cleanup-policies/{name}", h.getCleanupPolicy)
	m.HandleFunc("PUT "+p+"/cleanup-policies/{name}", h.updateCleanupPolicy)
	m.HandleFunc("DELETE "+p+"/cleanup-policies/{name}", h.deleteCleanupPolicy)
	m.HandleFunc("GET "+p+"/tasks/{id}", h.getTask)
	m.HandleFunc("POST "+p+"/tasks", h.createTask)
	m.HandleFunc("PUT "+p+"/tasks/{id}", h.updateTask)
	m.HandleFunc("DELETE "+p+"/tasks/{id}", h.deleteTask)
	m.HandleFunc("POST "+p+"/tasks/{id}/run", h.runTask)
	m.HandleFunc("POST "+p+"/tasks/{id}/stop", h.stopTask)
	m.HandleFunc("GET /service/rest/atlas/system-information", h.systemInformation)
}

// decodeJSON reads a Nexus request body into v; unknown fields are
// ignored, as Nexus clients send fields of newer versions.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// ---- cleanup policies ----

// uiPolicy is the UI's policy JSON (GET /-/api/cleanup/policies/{name}).
type uiPolicy struct {
	Name               string `json:"name"`
	Format             string `json:"format"`
	ComponentAgeDays   int    `json:"componentAgeDays"`
	LastDownloadedDays int    `json:"lastDownloadedDays"`
	ReleaseType        string `json:"releaseType"`
	NamePattern        string `json:"namePattern"`
	PathPattern        string `json:"pathPattern"`
	Retain             int    `json:"retain"`
	RetainSortBy       string `json:"retainSortBy"`
	Exclude            struct {
		NamePattern    string `json:"namePattern"`
		VersionPattern string `json:"versionPattern"`
	} `json:"exclude"`
	Notes string `json:"notes"`
}

// nexusPolicy is Nexus's cleanup policy body. The criteria map to
// Command Center's: criteriaLastBlobUpdated is componentAgeDays,
// criteriaLastDownloaded lastDownloadedDays, criteriaAssetRegex
// pathPattern, sortBy retainSortBy. namePattern and exclude have no Nexus
// field: they are not shown, and an update keeps them.
type nexusPolicy struct {
	Name                    string  `json:"name"`
	Notes                   string  `json:"notes"`
	Format                  string  `json:"format"`
	CriteriaLastBlobUpdated *int    `json:"criteriaLastBlobUpdated"`
	CriteriaLastDownloaded  *int    `json:"criteriaLastDownloaded"`
	CriteriaReleaseType     *string `json:"criteriaReleaseType"`
	CriteriaAssetRegex      *string `json:"criteriaAssetRegex"`
	Retain                  *int    `json:"retain"`
	SortBy                  *string `json:"sortBy"`
}

// policyFields renames the UI's fields in its validation messages.
var policyFields = strings.NewReplacer("componentAgeDays", "criteriaLastBlobUpdated", "lastDownloadedDays", "criteriaLastDownloaded",
	"releaseType", "criteriaReleaseType", "pathPattern", "criteriaAssetRegex", "retainSortBy", "sortBy")

func intOrNull(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func toNexusPolicy(p uiPolicy) map[string]any {
	format := nexusFormat(p.Format)
	if p.Format == "all" {
		format = "*"
	}
	var rt any
	switch p.ReleaseType {
	case "release":
		rt = "RELEASES"
	case "prerelease":
		rt = "PRERELEASES"
	}
	return map[string]any{"name": p.Name, "notes": p.Notes, "format": format,
		"criteriaLastBlobUpdated": intOrNull(p.ComponentAgeDays), "criteriaLastDownloaded": intOrNull(p.LastDownloadedDays),
		"criteriaReleaseType": rt, "criteriaAssetRegex": nullable(p.PathPattern), "retain": intOrNull(p.Retain), "sortBy": nullable(p.RetainSortBy)}
}

// uiPolicyBody maps a Nexus body onto stored (the policy updated; zero for
// a new one) as the UI's POST and PUT take it.
func uiPolicyBody(b nexusPolicy, stored uiPolicy) (map[string]any, []string) {
	var errs []string
	format := strings.ToLower(b.Format)
	switch format {
	case "*", "all", "all_formats":
		format = "all"
	default:
		format = ccFormat(format)
	}
	rt := "any"
	if b.CriteriaReleaseType != nil {
		switch strings.ToUpper(*b.CriteriaReleaseType) {
		case "":
		case "RELEASES", "RELEASE":
			rt = "release"
		case "PRERELEASES", "PRERELEASE", "PRELEASE":
			rt = "prerelease"
		default:
			errs = append(errs, fmt.Sprintf("criteriaReleaseType: %q: use RELEASES or PRERELEASES", *b.CriteriaReleaseType))
		}
	}
	deref := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return map[string]any{"name": b.Name, "notes": b.Notes, "format": format,
		"componentAgeDays": deref(b.CriteriaLastBlobUpdated), "lastDownloadedDays": deref(b.CriteriaLastDownloaded),
		"releaseType": rt, "pathPattern": str(b.CriteriaAssetRegex), "retain": deref(b.Retain),
		"retainSortBy": strings.ToLower(str(b.SortBy)), "namePattern": stored.NamePattern, "exclude": stored.Exclude}, errs
}

// writePolicyUIError is writeUIError with the UI's field names replaced by
// Nexus's.
func writePolicyUIError(w http.ResponseWriter, status int, body []byte) {
	var e struct {
		Error  string   `json:"error"`
		Errors []string `json:"errors"`
	}
	json.Unmarshal(body, &e)
	for i, m := range e.Errors {
		e.Errors[i] = policyFields.Replace(m)
	}
	e.Error = policyFields.Replace(e.Error)
	b, _ := json.Marshal(e)
	writeUIError(w, status, b)
}

func policyPath(name string) string { return "/-/api/cleanup/policies/" + url.PathEscape(name) }

func (h *Handler) listCleanupPolicies(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/cleanup/policies", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Policies []uiPolicy `json:"policies"`
	}
	json.Unmarshal(body, &res)
	out := []map[string]any{}
	for _, p := range res.Policies {
		out = append(out, toNexusPolicy(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// storedPolicy reads the named policy, or answers the error.
func (h *Handler) storedPolicy(w http.ResponseWriter, r *http.Request, name string) (uiPolicy, bool) {
	var p uiPolicy
	status, body := h.call(r, http.MethodGet, policyPath(name), nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return p, false
	}
	json.Unmarshal(body, &p)
	return p, true
}

func (h *Handler) getCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	if p, ok := h.storedPolicy(w, r, r.PathValue("name")); ok {
		writeJSON(w, http.StatusOK, toNexusPolicy(p))
	}
}

func (h *Handler) createCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b nexusPolicy
	if !decodeJSON(w, r, &b) {
		return
	}
	ui, errs := uiPolicyBody(b, uiPolicy{})
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/cleanup/policies", ui)
	if status != http.StatusCreated {
		writePolicyUIError(w, status, body)
		return
	}
	var p uiPolicy
	json.Unmarshal(body, &p)
	writeJSON(w, http.StatusCreated, toNexusPolicy(p))
}

// updateCleanupPolicy replaces the policy's Nexus fields (an absent
// criterion is off) and keeps namePattern and exclude.
func (h *Handler) updateCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	var b nexusPolicy
	if !decodeJSON(w, r, &b) {
		return
	}
	if b.Name != "" && b.Name != name {
		writeValidation(w, []string{"name: a cleanup policy cannot be renamed"})
		return
	}
	stored, ok := h.storedPolicy(w, r, name)
	if !ok {
		return
	}
	b.Name = name
	ui, errs := uiPolicyBody(b, stored)
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, body := h.call(r, http.MethodPut, policyPath(name), ui)
	if status != http.StatusOK {
		writePolicyUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, policyPath(r.PathValue("name")), nil)
	if status != http.StatusNoContent {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- tasks ----

// uiTask is what the Nexus task views take from the UI's task JSON.
type uiTask struct {
	ID, Name, Type, Status, LastMessage, LastResult, NextRun, LastRun string
	Params                                                            map[string]any
}

// nexusTaskAliases are Nexus type ids that a Command Center type serves with
// some parameters fixed (core-45).
var nexusTaskAliases = map[string]struct {
	typ    string
	params map[string]any
}{
	"repository.apt.rebuild.metadata": {"repository.rebuild-metadata", map[string]any{"format": "apt"}},
}

// nexusTaskID is the Nexus type id of t: its alias when its parameters
// are an alias's, else nexusTaskType.
func nexusTaskID(t uiTask) string {
	for id, a := range nexusTaskAliases {
		if t.Type != a.typ {
			continue
		}
		match := true
		for k, v := range a.params {
			match = match && t.Params[k] == v
		}
		if match {
			return id
		}
	}
	return nexusTaskType(t.Type)
}

// nexusTaskRefusals are Nexus type ids Command Center has no task for, and
// why (core-44).
var nexusTaskRefusals = map[string]string{
	"repository.maven.publish-dotindex": "Command Center does not publish Maven indexer files (.index/): " +
		"search with GET " + Prefix + "/search?maven.groupId=...&maven.artifactId=... instead",
	"repository.maven.unpublish-dotindex":  "Command Center publishes no Maven indexer files (.index/): there is nothing to unpublish",
	"create.browse.nodes":                  "not needed: the browse tree is read from the repository's assets, there is nothing to rebuild",
	"repository.trim-browse-tree":          "not needed: the browse tree is read from the repository's assets, there is nothing to trim",
	"repository.rebuild-index":             "not needed: search reads the repository's assets, there is no index to rebuild",
	"repository.maven.repair-base-version": "not needed: the base version of a SNAPSHOT is read from its path",
	"h2.backup.task":                       "Command Center keeps its metadata in PostgreSQL: back it up with pg_dump",
	"script":                               "Command Center has no script API",
}

func nexusTaskItem(t uiTask) map[string]any {
	var result any
	switch t.LastResult {
	case "OK":
		result = "OK"
	case "Failed":
		result = "FAILED"
	}
	return map[string]any{"id": t.ID, "name": t.Name, "type": nexusTaskID(t), "message": t.LastMessage,
		"currentState": strings.ToUpper(t.Status), "lastRunResult": result, "nextRun": nullable(t.NextRun), "lastRun": nullable(t.LastRun)}
}

func taskPath(id string) string { return "/-/api/tasks/" + url.PathEscape(id) }

// storedTask reads task id as the UI shows it, or answers the error.
func (h *Handler) storedTask(w http.ResponseWriter, r *http.Request, id string) (uiTask, bool) {
	var t uiTask
	status, body := h.call(r, http.MethodGet, taskPath(id), nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return t, false
	}
	json.Unmarshal(body, &t)
	return t, true
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	if t, ok := h.storedTask(w, r, r.PathValue("id")); ok {
		writeJSON(w, http.StatusOK, nexusTaskItem(t))
	}
}

// taskTemplate is Nexus's TaskTemplateXO.
type taskTemplate struct {
	Type                  string         `json:"type"`
	Name                  string         `json:"name"`
	Enabled               *bool          `json:"enabled"`
	AlertEmail            *string        `json:"alertEmail"`
	NotificationCondition string         `json:"notificationCondition"`
	Frequency             *frequency     `json:"frequency"`
	Properties            map[string]any `json:"properties"`
}

type frequency struct {
	Schedule       string `json:"schedule"`
	StartDate      int64  `json:"startDate"` // Unix milliseconds (seconds accepted)
	TimeZoneOffset string `json:"timeZoneOffset"`
	RecurringDays  []int  `json:"recurringDays"`
	CronExpression string `json:"cronExpression"`
}

// taskTypes is the UI's task type list: each type and its parameters.
type taskTypes []struct {
	Type   string `json:"type"`
	Params []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"params"`
}

func (h *Handler) taskTypes(w http.ResponseWriter, r *http.Request) (taskTypes, bool) {
	status, body := h.call(r, http.MethodGet, "/-/api/tasks", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return nil, false
	}
	var res struct {
		Types taskTypes `json:"types"`
	}
	json.Unmarshal(body, &res)
	return res.Types, true
}

// resolve maps a Nexus type id, or a Command Center one, to the type and
// the parameters an alias fixes (nil for none).
func (tt taskTypes) resolve(id string) (int, map[string]any, bool) {
	a, alias := nexusTaskAliases[id]
	if alias {
		id = a.typ
	}
	for i, t := range tt {
		if t.Type == id || nexusTaskType(t.Type) == id {
			return i, a.params, true
		}
	}
	return 0, nil, false
}

func (tt taskTypes) ids() string {
	var ids []string
	for _, t := range tt {
		ids = append(ids, nexusTaskType(t.Type))
	}
	for id, a := range nexusTaskAliases {
		if _, _, ok := tt.resolve(a.typ); ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return strings.Join(ids, ", ")
}

// nexusProperties are Nexus property names with another name here.
var nexusProperties = map[string]string{"blobstoreName": "blobStore", "repositoryName": "repository", "gracePeriodInDays": "gracePeriodDays"}

// allBlobStores is Nexus's blobstoreName for every blob store.
const allBlobStores = "(All Blob Stores)"

// params maps Nexus task properties (strings) to the parameters of type i:
// names renamed, values converted to the parameter's type, then fixed (an
// alias's) set. Others go through as they are, so the type's own
// validation names them.
func (tt taskTypes) params(i int, props, fixed map[string]any) (map[string]any, []string) {
	t := tt[i]
	out := map[string]any{}
	var errs []string
	for k, v := range props {
		name := k
		if n, ok := nexusProperties[k]; ok {
			name = n
		}
		switch {
		case t.Type == "repository.rebuild-metadata" && name == "repository":
			name = "repo"
			if v == "*" {
				v = ""
			}
		case t.Type == "repository.purge-unused" && name == "repository":
			name = "repos" // one repository, a comma list, or "*" for every proxy
			if v == "*" {
				v = ""
			}
		case name == "blobStore" && v == allBlobStores:
			v = ""
		case t.Type == "repository.rebuild-metadata" && name == "rebuildAptMetadataFullRebuild":
			continue // Command Center always rebuilds apt metadata from its rows
		case t.Type == "blobstore.compact" && name == "blobsOlderThan":
			if v != "0" && v != float64(0) {
				errs = append(errs, fmt.Sprintf("properties.blobsOlderThan: %v: Command Center keeps no deleted blobs: compaction removes "+
					"the blobs no asset references once they are an hour old; only 0 is supported", v))
			}
			continue
		}
		s, isString := v.(string)
		for _, p := range t.Params {
			if p.Name != name || !isString {
				continue
			}
			var err error
			switch p.Type {
			case "bool":
				v, err = strconv.ParseBool(s)
			case "int", "bytes":
				v, err = strconv.ParseInt(s, 10, 64)
			case "repositories":
				list := []string{}
				for _, n := range strings.Split(s, ",") {
					if n = strings.TrimSpace(n); n != "" {
						list = append(list, n)
					}
				}
				v = list
			}
			if err != nil {
				errs = append(errs, fmt.Sprintf("properties.%s: %q is not a %s", k, s, p.Type))
			}
		}
		out[name] = v
	}
	for k, v := range fixed {
		out[k] = v
	}
	return out, errs
}

var weekdayNames = [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// offsetRE is a time zone offset: +9, -05:00, +0530.
var offsetRE = regexp.MustCompile(`^([+-]?)([0-9]{1,2})(?::?([0-9]{2}))?$`)

func offsetZone(s string) (*time.Location, error) {
	s = strings.TrimSpace(s)
	switch strings.ToUpper(s) {
	case "", "Z", "UTC", "GMT":
		return time.UTC, nil
	}
	m := offsetRE.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("frequency.timeZoneOffset: %q: use an offset such as +09:00 or -5", s)
	}
	h, _ := strconv.Atoi(m[2])
	mins := 0
	if m[3] != "" {
		mins, _ = strconv.Atoi(m[3])
	}
	if h > 14 || mins > 59 {
		return nil, fmt.Errorf("frequency.timeZoneOffset: %q is out of range", s)
	}
	secs := h*3600 + mins*60
	if m[1] == "-" {
		secs = -secs
	}
	return time.FixedZone(s, secs), nil
}

// scheduleOf maps a Nexus frequency to a schedule string. startDate is an
// instant and recurringDays are days in the timeZoneOffset zone (weekly: 1
// Sunday to 7 Saturday; monthly: 1 to 31, and 999 for the last day); the
// schedule runs at the same instants in the server's zone loc. cron
// takes any Quartz expression, in the server's zone.
func scheduleOf(f *frequency, loc *time.Location) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(f.Schedule))
	switch kind {
	case "manual":
		return "manual", nil
	case "cron":
		return cronSchedule(f.CronExpression)
	case "once", "hourly", "daily", "weekly", "monthly":
	default:
		return "", fmt.Errorf("frequency.schedule: %q: use manual, once, hourly, daily, weekly, monthly or cron", f.Schedule)
	}
	if f.StartDate <= 0 {
		return "", fmt.Errorf("frequency.startDate is required for a %s schedule", kind)
	}
	zone, err := offsetZone(f.TimeZoneOffset)
	if err != nil {
		return "", err
	}
	ms := f.StartDate
	if ms < 1e11 { // seconds: 1e11 ms is 1973, 1e11 s the year 5138
		ms *= 1000
	}
	start := time.UnixMilli(ms).In(zone)
	srv := start.In(loc)
	switch kind {
	case "once":
		return "once:" + start.Format(time.RFC3339), nil
	case "hourly":
		return fmt.Sprintf("hourly:%02d", srv.Minute()), nil
	case "daily":
		return fmt.Sprintf("daily:%02d:%02d", srv.Hour(), srv.Minute()), nil
	}
	if len(f.RecurringDays) == 0 || len(f.RecurringDays) > 32 {
		return "", fmt.Errorf("frequency.recurringDays: a %s schedule takes one or more days", kind)
	}
	var days []string
	var occ time.Time
	if kind == "weekly" {
		for _, d := range f.RecurringDays {
			if d < 1 || d > 7 {
				return "", fmt.Errorf("frequency.recurringDays: %d: a weekly schedule takes days 1 (Sunday) to 7 (Saturday)", d)
			}
			occ = time.Date(start.Year(), start.Month(), start.Day()+(d-1-int(start.Weekday())+7)%7, start.Hour(), start.Minute(), 0, 0, zone).In(loc)
			days = append(days, weekdayNames[occ.Weekday()])
		}
		return fmt.Sprintf("weekly:%s:%02d:%02d", strings.Join(days, ","), occ.Hour(), occ.Minute()), nil
	}
	// How many days the start time moves in the server's zone: days near
	// the month's end cannot move with it (a 28th that becomes a 29th
	// would skip February).
	occ = time.Date(start.Year(), start.Month(), 15, start.Hour(), start.Minute(), 0, 0, zone).In(loc)
	shift := occ.Day() - 15
	for _, d := range f.RecurringDays {
		switch {
		case d == 999 && shift == 0:
			days = append(days, "last")
		case d < 1 || d > 31 && d != 999:
			return "", fmt.Errorf("frequency.recurringDays: %d: a monthly schedule takes days 1 to 31, and 999 for the last day", d)
		case shift == 0:
			days = append(days, strconv.Itoa(d))
		case d == 999 || d+shift < 1 || d+shift > 28:
			return "", fmt.Errorf("frequency.recurringDays: day %d at %s falls on another day in the server's time zone (%s), "+
				"which months do not all have: give the start date in the server's offset, or use a cron expression", d, start.Format("15:04 -07:00"), occ.Format("-07:00"))
		default:
			days = append(days, strconv.Itoa(d+shift))
		}
	}
	return fmt.Sprintf("monthly:%s:%02d:%02d", strings.Join(days, ","), occ.Hour(), occ.Minute()), nil
}

// cronSchedule maps a Quartz expression (seconds minutes hours
// day-of-month month day-of-week [year]) to the schedule kind of the same
// shape: hourly, daily, weekly on one day, monthly on one day, otherwise
// cron. All run in the server's zone.
func cronSchedule(expr string) (string, error) {
	c, err := tasks.ParseCron(expr)
	if err != nil {
		return "", fmt.Errorf("frequency.cronExpression: %q: %v", expr, err)
	}
	f := strings.Fields(c.String())
	if len(f) == 7 && f[6] != "*" || f[0] != "0" || f[4] != "*" {
		return "cron:" + c.String(), nil
	}
	num := func(s string, lo, hi int) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= lo && n <= hi && allDigits(s)
	}
	every := func(s string) bool { return s == "*" || s == "?" }
	minute, okMin := num(f[1], 0, 59)
	hour, okHour := num(f[2], 0, 23)
	dom, dow := f[3], f[5]
	switch {
	case !okMin:
	case f[2] == "*" && every(dom) && every(dow):
		return fmt.Sprintf("hourly:%02d", minute), nil
	case !okHour:
	case every(dom) && every(dow):
		return fmt.Sprintf("daily:%02d:%02d", hour, minute), nil
	case every(dom):
		wd := slices.Index([]string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}, dow)
		if n, ok := num(dow, 1, 7); ok { // Quartz: 1 is Sunday
			wd = n - 1
		}
		if wd >= 0 {
			return fmt.Sprintf("weekly:%s:%02d:%02d", weekdayNames[wd], hour, minute), nil
		}
	case every(dow):
		if d, ok := num(dom, 1, 31); ok {
			return fmt.Sprintf("monthly:%d:%02d:%02d", d, hour, minute), nil
		}
	}
	return "cron:" + c.String(), nil
}

func allDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// uiTaskDef maps a TaskTemplateXO to the UI's task body; i is the
// resolved type and fixed the parameters its alias fixes. Absent fields
// are left out, so an update keeps them.
func uiTaskDef(tt taskTypes, i int, fixed map[string]any, b taskTemplate) (map[string]any, []string) {
	d := map[string]any{"type": tt[i].Type}
	var errs []string
	if b.Name != "" {
		d["name"] = b.Name
	}
	if b.Enabled != nil {
		d["enabled"] = *b.Enabled
	}
	if b.Frequency != nil {
		sc, err := scheduleOf(b.Frequency, time.Local) // the task service's zone
		if err != nil {
			errs = append(errs, err.Error())
		}
		d["schedule"] = sc
	}
	if b.Properties != nil || fixed != nil {
		params, perrs := tt.params(i, b.Properties, fixed)
		errs = append(errs, perrs...)
		d["params"] = params
	}
	switch strings.ToUpper(b.NotificationCondition) {
	case "":
	case "FAILURE":
		d["notifyOn"] = tasks.NotifyFailure
	case "SUCCESS_FAILURE":
		d["notifyOn"] = tasks.NotifySuccessFailure
	default:
		errs = append(errs, fmt.Sprintf("notificationCondition: %q: use FAILURE or SUCCESS_FAILURE", b.NotificationCondition))
	}
	if b.AlertEmail != nil {
		to := []string{}
		for _, a := range strings.Split(*b.AlertEmail, ",") {
			if a = strings.TrimSpace(a); a != "" {
				to = append(to, a)
			}
		}
		d["notifyEmail"] = to
	}
	return d, errs
}

// createTask is POST /tasks: 201 with the task, 400 for an unknown type
// (listing the supported ids) or invalid fields.
func (h *Handler) createTask(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b taskTemplate
	if !decodeJSON(w, r, &b) {
		return
	}
	tt, ok := h.taskTypes(w, r)
	if !ok {
		return
	}
	i, fixed, ok := tt.resolve(b.Type)
	if why, refused := nexusTaskRefusals[b.Type]; !ok && refused {
		writeValidation(w, []string{fmt.Sprintf("type: %s: %s", b.Type, why)})
		return
	}
	if !ok {
		writeValidation(w, []string{fmt.Sprintf("type: unknown task type %q: supported are %s", b.Type, tt.ids())})
		return
	}
	if b.Frequency == nil {
		writeValidation(w, []string{"frequency: required"})
		return
	}
	d, errs := uiTaskDef(tt, i, fixed, b)
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/tasks", d)
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	var t uiTask
	json.Unmarshal(body, &t)
	writeJSON(w, http.StatusCreated, nexusTaskItem(t))
}

// updateTask is PUT /tasks/{id}: 204. Absent fields are kept; the type
// cannot change.
func (h *Handler) updateTask(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	var b taskTemplate
	if !decodeJSON(w, r, &b) {
		return
	}
	cur, ok := h.storedTask(w, r, id)
	if !ok {
		return
	}
	tt, ok := h.taskTypes(w, r)
	if !ok {
		return
	}
	i, fixed, ok := tt.resolve(nexusTaskID(cur))
	if !ok {
		writeMessage(w, http.StatusConflict, "the task's type "+strconv.Quote(cur.Type)+" is no longer available")
		return
	}
	if b.Type != "" && b.Type != cur.Type && b.Type != nexusTaskID(cur) {
		writeValidation(w, []string{"type: the type of a task cannot change"})
		return
	}
	if b.Properties == nil {
		fixed = nil // the stored params, which hold them, are kept
	}
	d, errs := uiTaskDef(tt, i, fixed, b)
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, body := h.call(r, http.MethodPut, taskPath(id), d)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// noContent forwards method path to the UI and answers 204 when it
// answered want.
func (h *Handler) noContent(w http.ResponseWriter, r *http.Request, method, path string, want int) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, method, path, nil)
	if status != want {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteTask(w http.ResponseWriter, r *http.Request) {
	h.noContent(w, r, http.MethodDelete, taskPath(r.PathValue("id")), http.StatusNoContent)
}

func (h *Handler) runTask(w http.ResponseWriter, r *http.Request) {
	h.noContent(w, r, http.MethodPost, taskPath(r.PathValue("id"))+"/run", http.StatusAccepted)
}

func (h *Handler) stopTask(w http.ResponseWriter, r *http.Request) {
	h.noContent(w, r, http.MethodPost, taskPath(r.PathValue("id"))+"/stop", http.StatusAccepted)
}

// ---- system information ----

// systemInformation is GET /service/rest/atlas/system-information (admin):
// the UI's system information and blob store views, in Nexus's sections.
// Both are API views that hold no secret (the support bundle is built from
// the same views): no database URL, environment allow-listed, proxy
// credentials reported as present or not, S3 stores without their keys.
func (h *Handler) systemInformation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/systeminformation", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var info map[string]map[string]any
	json.Unmarshal(body, &info)
	status, body = h.call(r, http.MethodGet, "/-/api/blobstores", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var bs struct {
		Stores []struct {
			Name      string  `json:"name"`
			Type      string  `json:"type"`
			Path      string  `json:"path"`
			BlobCount int64   `json:"blobCount"`
			UsedBytes int64   `json:"usedBytes"`
			FsTotal   *uint64 `json:"fsTotal"`
			FsFree    *uint64 `json:"fsFree"`
			S3        *struct {
				Bucket   string `json:"bucket"`
				Region   string `json:"region"`
				Endpoint string `json:"endpoint"`
			} `json:"s3"`
			Google *struct {
				Bucket   string `json:"bucket"`
				Endpoint string `json:"endpoint"`
			} `json:"google"`
		} `json:"blobstores"`
	}
	json.Unmarshal(body, &bs)
	stores := map[string]any{}
	for _, s := range bs.Stores {
		v := map[string]any{"name": s.Name, "type": s.Type, "path": s.Path, "blobCount": s.BlobCount, "usedBytes": s.UsedBytes,
			"totalSpace": s.FsTotal, "usableSpace": s.FsFree}
		if s.S3 != nil {
			v["bucket"], v["region"], v["endpoint"] = s.S3.Bucket, s.S3.Region, s.S3.Endpoint
		}
		if s.Google != nil {
			v["bucket"], v["endpoint"] = s.Google.Bucket, s.Google.Endpoint
		}
		stores[s.Name] = v
	}

	st := map[string]any{"edition": "Command Center", "state": "STARTED"}
	for k, v := range info["commandcenter-status"] {
		st[k] = v
	}
	host, _ := os.Hostname()
	now := time.Now()
	zone, offset := now.Zone()
	rt := map[string]any{}
	for k, v := range info["system-runtime"] {
		rt[k] = v
	}
	// Nexus's JVM names for the Go runtime's figures.
	rt["availableProcessors"], rt["threads"] = rt["numCPU"], rt["goroutines"]
	rt["totalMemory"], rt["maxMemory"] = rt["sys"], rt["gomemlimit"]
	if sys, ok := rt["sys"].(float64); ok {
		if inuse, ok := rt["heapInuse"].(float64); ok {
			rt["freeMemory"] = sys - inuse
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nexus-status":        st,
		"nexus-node":          map[string]any{"node-id": host, "hostname": host},
		"nexus-configuration": info["commandcenter-configuration"],
		"nexus-properties":    info["system-environment"],
		"system-time": map[string]any{"timezone": time.Local.String(), "zone": zone, "offsetSeconds": offset,
			"current": now.UnixMilli(), "iso8601": now.Format(time.RFC3339)},
		"system-runtime":     rt,
		"system-filestores":  stores,
		"system-environment": info["system-environment"], // also under nexus-properties, where earlier builds put it
		"system-network":     systemNetwork(),
		"system-properties":  systemProperties(),
	})
}
