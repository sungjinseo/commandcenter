package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/vuln"
)

// The repository dashboard, the evaluation results behind it and the
// monthly usage metrics (meta usage.go): admin, or the read privilege on
// auth.MetricsTarget.

func (u *UI) dashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/dashboard/repositories", u.dashboardRepositories)
	mux.HandleFunc("GET /-/api/dashboard/repositories/{name}/vulnerabilities", u.dashboardVulnerabilities)
	mux.HandleFunc("GET /-/api/metrics/monthly", u.monthlyMetrics)
}

const (
	// UsageRetentionMonths is how many calendar months the monthly usage
	// metrics keep, the current one included.
	UsageRetentionMonths = 24
	dashboardLimit       = 500
	maxMonthSpan         = 36
	// countBudget bounds the component counts one dashboard request or
	// usage refresh makes; the repositories left keep their last count
	// (componentsCurrent false) until the next one.
	countBudget   = 2 * time.Second
	refreshBudget = time.Minute
)

// requireMetrics answers 401 to anonymous callers and 403 unless the caller
// may read the usage data.
func (u *UI) requireMetrics(w http.ResponseWriter, r *http.Request) bool {
	p, ok := requireUser(w, r)
	if ok && !u.Auth.CanReadMetrics(p) {
		writeError(w, http.StatusForbidden, "admin or the read privilege on \""+auth.MetricsTarget+"\" is required")
		return false
	}
	return ok
}

// countComponents counts rp's components as the component search derives
// them: from the package rows in the package formats' hosted and group
// repositories, from the asset paths otherwise.
func (u *UI) countComponents(ctx context.Context, rp *repo.Repository) (int64, error) {
	// ponytail: one key per component of the repository in memory (~100
	// bytes; 50k components counted in ~70 ms); count in SQL if repositories
	// grow past millions.
	seen := map[[3]string]bool{}
	if packageFormats[rp.Format] && rp.Type != "proxy" {
		err := u.DB.WalkPackageVersions(ctx, rp.Name, func(name, version string) {
			g, n, v, _ := component.PackageComponent(rp.Format, name, version)
			seen[[3]string{g, n, v}] = true
		})
		return int64(len(seen)), err
	}
	err := u.DB.WalkAssetPaths(ctx, rp.Name, "", func(p string) {
		if g, n, v, ok := component.DeriveAsset(rp.Format, p); ok {
			seen[[3]string{g, n, v}] = true
		}
	})
	return int64(len(seen)), err
}

// refreshCounts counts again the components of the repositories whose
// assets changed since their last count, until budget is spent, and puts
// the new counts into usage.
func (u *UI) refreshCounts(ctx context.Context, repos []*repo.Repository, usage map[string]meta.RepoUsage, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for _, rp := range repos {
		cur, ok := usage[rp.Name]
		if !ok || cur.CountedChanges == cur.Changes {
			continue
		}
		if time.Now().After(deadline) {
			return nil
		}
		n, err := u.countComponents(ctx, rp)
		if err != nil {
			return err
		}
		at := u.clock()
		if err := u.DB.SetComponentCount(ctx, rp.Name, n, cur.Changes, at); err != nil {
			return err
		}
		cur.Components, cur.CountedChanges, cur.CountedAt = n, cur.Changes, at
		usage[rp.Name] = cur
	}
	return nil
}

// RefreshUsage counts again the changed repositories' components, samples
// the usage of the month of now and drops the months past
// UsageRetentionMonths. The leader runs it every minute (app); the
// monthly metrics request samples too.
func (u *UI) RefreshUsage(ctx context.Context, now time.Time) error {
	usage, err := u.DB.RepoUsages(ctx)
	if err != nil {
		return err
	}
	if err := u.refreshCounts(ctx, u.Repos.Snapshot().Sorted, usage, refreshBudget); err != nil {
		return err
	}
	if err := u.DB.SampleUsage(ctx, now); err != nil {
		return err
	}
	return u.DB.PruneUsage(ctx, now.AddDate(0, 1-UsageRetentionMonths, 0))
}

type severityJSON struct {
	Critical int64 `json:"critical"`
	High     int64 `json:"high"`
	Medium   int64 `json:"medium"`
	Low      int64 `json:"low"`
	Unknown  int64 `json:"unknown"`
}

type evaluationJSON struct {
	Covered       bool         `json:"covered"` // a hosted or proxy repository of a format with OSV data
	Ecosystem     string       `json:"ecosystem"`
	Setting       string       `json:"setting"`   // the repository's: inherit, enabled or disabled
	Monitored     bool         `json:"monitored"` // repository.evaluate evaluates it
	LastEvaluated string       `json:"lastEvaluated"`
	Components    int64        `json:"components"`
	Vulnerable    int64        `json:"vulnerable"`
	Malicious     int64        `json:"malicious"`
	BySeverity    severityJSON `json:"bySeverity"` // vulnerable components by their highest severity
}

type dashboardRow struct {
	Name              string         `json:"name"`
	Format            string         `json:"format"`
	Type              string         `json:"type"`
	Online            bool           `json:"online"`
	Components        int64          `json:"components"`
	ComponentsCounted string         `json:"componentsCounted"` // when components was counted; "" with no asset ever
	ComponentsCurrent bool           `json:"componentsCurrent"` // no asset changed since
	Assets            int64          `json:"assets"`
	Bytes             int64          `json:"bytes"`       // the assets' sizes summed
	UniqueBytes       int64          `json:"uniqueBytes"` // each distinct blob once
	LastUpload        string         `json:"lastUpload"`
	LastDownload      string         `json:"lastDownload"`
	Requests24h       int64          `json:"requests24h"`
	Requests30d       int64          `json:"requests30d"`
	Evaluation        evaluationJSON `json:"evaluation"`
}

// dashboardSorts are the sort keys and their default order (true:
// descending).
var dashboardSorts = map[string]bool{"name": false, "format": false, "type": false, "components": true, "assets": true, "bytes": true,
	"uniqueBytes": true, "lastUpload": true, "lastDownload": true, "requests24h": true, "requests30d": true, "vulnerable": true}

func (row dashboardRow) compare(key string, o dashboardRow) int {
	switch key {
	case "format":
		return strings.Compare(row.Format, o.Format)
	case "type":
		return strings.Compare(row.Type, o.Type)
	case "components":
		return cmp.Compare(row.Components, o.Components)
	case "assets":
		return cmp.Compare(row.Assets, o.Assets)
	case "bytes":
		return cmp.Compare(row.Bytes, o.Bytes)
	case "uniqueBytes":
		return cmp.Compare(row.UniqueBytes, o.UniqueBytes)
	case "lastUpload": // RFC 3339 in UTC sorts as text; "" first
		return strings.Compare(row.LastUpload, o.LastUpload)
	case "lastDownload":
		return strings.Compare(row.LastDownload, o.LastDownload)
	case "requests24h":
		return cmp.Compare(row.Requests24h, o.Requests24h)
	case "requests30d":
		return cmp.Compare(row.Requests30d, o.Requests30d)
	case "vulnerable":
		e, f := row.Evaluation, o.Evaluation
		return cmp.Or(cmp.Compare(e.Vulnerable, f.Vulnerable), cmp.Compare(e.BySeverity.Critical, f.BySeverity.Critical),
			cmp.Compare(e.BySeverity.High, f.BySeverity.High))
	}
	return 0
}

// GET /-/api/dashboard/repositories?format=&type=&q=&vulnerable=true&sort=&order=asc|desc&limit=&offset=
func (u *UI) dashboardRepositories(w http.ResponseWriter, r *http.Request) {
	if !u.requireMetrics(w, r) {
		return
	}
	q := r.URL.Query()
	limit, ok := intParam(w, q.Get("limit"), 50, 1, dashboardLimit, "limit")
	if !ok {
		return
	}
	offset, ok := intParam(w, q.Get("offset"), 0, 0, 1<<30, "offset")
	if !ok {
		return
	}
	key := cmp.Or(q.Get("sort"), "name")
	desc, known := dashboardSorts[key]
	if !known {
		writeError(w, http.StatusBadRequest, "sort must be name, format, type, components, assets, bytes, uniqueBytes, lastUpload, lastDownload, requests24h, requests30d or vulnerable")
		return
	}
	switch q.Get("order") {
	case "":
	case "asc":
		desc = false
	case "desc":
		desc = true
	default:
		writeError(w, http.StatusBadRequest, "order must be asc or desc")
		return
	}
	format, typ, name := q.Get("format"), q.Get("type"), strings.ToLower(q.Get("q"))
	vulnerableOnly := q.Get("vulnerable") == "true" || q.Get("vulnerable") == "1"
	snap := u.Repos.Snapshot()
	var repos []*repo.Repository
	for _, rp := range snap.Sorted {
		if (format == "" || rp.Format == format) && (typ == "" || rp.Type == typ) && strings.Contains(strings.ToLower(rp.Name), name) {
			repos = append(repos, rp)
		}
	}
	ctx := r.Context()
	usage, err := u.DB.RepoUsages(ctx)
	if err == nil {
		var release func()
		if release, err = u.acquireHeavy(ctx); err == nil {
			err = u.refreshCounts(ctx, repos, usage, countBudget)
			release()
		}
	}
	end := u.clock().Unix()/3600*3600 + 3600
	var day, month []meta.RepoTraffic
	if err == nil {
		day, err = u.DB.RepoTrafficStats(ctx, end-24*3600, end)
	}
	if err == nil {
		month, err = u.DB.RepoTrafficStats(ctx, end-30*24*3600, end)
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	requests := map[string][2]int64{}
	for _, t := range day {
		requests[t.Repo] = [2]int64{t.Requests}
	}
	for _, t := range month {
		requests[t.Repo] = [2]int64{requests[t.Repo][0], t.Requests}
	}
	var set vuln.Settings
	if u.Vuln != nil {
		set = u.Vuln.Settings(ctx)
	}
	members := vuln.GroupMembers(snap)
	rows := []dashboardRow{}
	formats := map[string]bool{}
	for _, rp := range snap.Sorted {
		formats[rp.Format] = true
	}
	for _, rp := range repos {
		us, ok := usage[rp.Name]
		e := us.Eval
		row := dashboardRow{Name: rp.Name, Format: rp.Format, Type: rp.Type, Online: rp.IsOnline(),
			Components: max(us.Components, 0), ComponentsCounted: rfc3339(us.CountedAt), ComponentsCurrent: !ok || us.CountedChanges == us.Changes,
			Assets: us.Assets, Bytes: us.Bytes, UniqueBytes: us.UniqueBytes, LastUpload: rfc3339(us.LastUpload), LastDownload: rfc3339(us.LastDownload),
			Requests24h: requests[rp.Name][0], Requests30d: requests[rp.Name][1],
			Evaluation: evaluationJSON{Covered: rp.Type != "group" && vuln.Ecosystems[rp.Format] != "", Ecosystem: vuln.Ecosystems[rp.Format],
				Setting: cmp.Or(rp.Evaluation, "inherit"), Monitored: u.Vuln != nil && set.Enabled && vuln.Monitored(set, members, rp),
				LastEvaluated: rfc3339(e.At), Components: e.Components, Vulnerable: e.Vulnerable, Malicious: e.Malicious,
				BySeverity: severityJSON{Critical: e.Critical, High: e.High, Medium: e.Medium, Low: e.Low, Unknown: e.Unknown}}}
		if !vulnerableOnly || e.Vulnerable > 0 {
			rows = append(rows, row)
		}
	}
	slices.SortStableFunc(rows, func(a, b dashboardRow) int {
		c := a.compare(key, b)
		if desc {
			c = -c
		}
		return cmp.Or(c, strings.Compare(a.Name, b.Name))
	})
	total := len(rows)
	rows = rows[min(offset, total):min(offset+limit, total)]
	uncovered := []string{}
	for f := range formats {
		if vuln.Ecosystems[f] == "" {
			uncovered = append(uncovered, f)
		}
	}
	slices.Sort(uncovered)
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "offset": offset, "sort": key, "order": map[bool]string{false: "asc", true: "desc"}[desc],
		"items": rows, "uncoveredFormats": uncovered})
}

type vulnerableJSON struct {
	Component      string          `json:"component"` // the label: group:name:version for Maven, [group/]name@version otherwise
	Group          string          `json:"group"`
	Name           string          `json:"name"`
	Version        string          `json:"version"`
	Ecosystem      string          `json:"ecosystem"`
	Package        string          `json:"package"` // as OSV names it
	PackageVersion string          `json:"packageVersion"`
	Severity       string          `json:"severity"`
	Malicious      bool            `json:"malicious"`
	Unchecked      bool            `json:"unchecked"` // the online lookup failed: local data only
	Evaluated      string          `json:"evaluated"`
	Advisories     json.RawMessage `json:"advisories"` // the component vulnerabilities API's objects
}

// GET /-/api/dashboard/repositories/{name}/vulnerabilities?severity=low|medium|high|critical&limit=&offset=
// lists the repository's vulnerable components of at least that severity
// (malicious ones always), malicious first, then by severity.
func (u *UI) dashboardVulnerabilities(w http.ResponseWriter, r *http.Request) {
	if !u.requireMetrics(w, r) {
		return
	}
	rp := u.Repos.Get(r.PathValue("name"))
	if rp == nil {
		writeError(w, http.StatusNotFound, "repository \""+r.PathValue("name")+"\" not found")
		return
	}
	q := r.URL.Query()
	limit, ok := intParam(w, q.Get("limit"), 50, 1, dashboardLimit, "limit")
	if !ok {
		return
	}
	offset, ok := intParam(w, q.Get("offset"), 0, 0, 1<<30, "offset")
	if !ok {
		return
	}
	sev := q.Get("severity")
	if sev != "" && vuln.Rank(sev) == 0 {
		writeError(w, http.StatusBadRequest, "severity must be low, medium, high or critical")
		return
	}
	cs, total, err := u.DB.VulnerableComponents(r.Context(), rp.Name, vuln.Rank(sev), limit, offset)
	var usage map[string]meta.RepoUsage
	if err == nil {
		usage, err = u.DB.RepoUsages(r.Context())
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	items := make([]vulnerableJSON, 0, len(cs))
	for _, c := range cs {
		items = append(items, vulnerableJSON{Component: component.Component{Format: rp.Format, Group: c.Group, Name: c.Name, Version: c.Version}.Label(),
			Group: c.Group, Name: c.Name, Version: c.Version, Ecosystem: c.Ecosystem, Package: c.Package, PackageVersion: c.PackageVersion,
			Severity: cmp.Or(c.Severity, "unknown"), Malicious: c.Malicious, Unchecked: c.Unchecked, Evaluated: rfc3339(c.Evaluated),
			Advisories: json.RawMessage(c.Advisories)})
	}
	monitored := false
	if u.Vuln != nil {
		set := u.Vuln.Settings(r.Context())
		monitored = set.Enabled && vuln.Monitored(set, vuln.GroupMembers(u.Repos.Snapshot()), rp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"repository": rp.Name, "format": rp.Format, "type": rp.Type, "monitored": monitored,
		"lastEvaluated": rfc3339(usage[rp.Name].Eval.At), "total": total, "limit": limit, "offset": offset, "items": items})
}

type monthJSON struct {
	Month            string `json:"month"` // YYYY-MM, UTC
	Start            string `json:"start"`
	Current          bool   `json:"current"` // not over yet
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	EgressBytes      int64  `json:"egressBytes"`
	IngressBytes     int64  `json:"ingressBytes"`
	Downloads        int64  `json:"downloads"`
	Uploads          int64  `json:"uploads"`
	UploadBytes      int64  `json:"uploadBytes"`
	StorageBytes     *int64 `json:"storageBytes"` // null: never sampled
	PeakStorageBytes *int64 `json:"peakStorageBytes"`
	Assets           *int64 `json:"assets"`
	Components       *int64 `json:"components"`
	PeakComponents   *int64 `json:"peakComponents"`
	UniqueUsers      int64  `json:"uniqueUsers"`
	UniqueAddresses  int64  `json:"uniqueAddresses"`
	Sampled          string `json:"sampled"`
}

// parseMonth reads YYYY-MM.
func parseMonth(w http.ResponseWriter, s string, def time.Time, name string) (time.Time, bool) {
	if s == "" {
		return def, true
	}
	t, err := time.Parse("2006-01", s)
	if err != nil {
		writeError(w, http.StatusBadRequest, name+" must be a month, YYYY-MM")
		return t, false
	}
	return t, true
}

// GET /-/api/metrics/monthly?from=YYYY-MM&to=YYYY-MM: every month of the
// range (default the last 12), oldest first, months without data as zero.
// The current month is sampled first.
func (u *UI) monthlyMetrics(w http.ResponseWriter, r *http.Request) {
	if !u.requireMetrics(w, r) {
		return
	}
	now := u.clock()
	cur := meta.MonthStart(now)
	to, ok := parseMonth(w, r.URL.Query().Get("to"), cur, "to")
	if !ok {
		return
	}
	from, ok := parseMonth(w, r.URL.Query().Get("from"), to.AddDate(0, -11, 0), "from")
	if !ok {
		return
	}
	if from.After(to) || from.AddDate(0, maxMonthSpan, 0).Before(to.AddDate(0, 1, 0)) {
		writeError(w, http.StatusBadRequest, "from must not be after to, and the range must span at most 36 months")
		return
	}
	ctx := r.Context()
	if !to.Before(cur) {
		release, err := u.acquireHeavy(ctx)
		if err == nil {
			var usage map[string]meta.RepoUsage
			if usage, err = u.DB.RepoUsages(ctx); err == nil {
				err = u.refreshCounts(ctx, u.Repos.Snapshot().Sorted, usage, countBudget)
			}
			release()
		}
		if err == nil {
			err = u.DB.SampleUsage(ctx, now)
		}
		if err != nil {
			u.fail(w, r, err)
			return
		}
	}
	got, err := u.DB.MonthlyUsage(ctx, from, to.AddDate(0, 1, 0))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	byMonth := map[int64]meta.MonthUsage{}
	for _, m := range got {
		byMonth[m.Month.Unix()] = m
	}
	months := []monthJSON{}
	for m := from; !m.After(to); m = m.AddDate(0, 1, 0) {
		d := byMonth[m.Unix()]
		out := monthJSON{Month: m.Format("2006-01"), Start: rfc3339(m), Current: m.Equal(cur), Requests: d.Requests, Errors: d.Errors,
			EgressBytes: d.EgressBytes, IngressBytes: d.IngressBytes, Downloads: d.Downloads, Uploads: d.Uploads, UploadBytes: d.UploadBytes,
			UniqueUsers: d.Users, UniqueAddresses: d.Addresses, Sampled: rfc3339(d.Sampled)}
		if !d.Sampled.IsZero() {
			out.StorageBytes, out.PeakStorageBytes, out.Assets = &d.StorageBytes, &d.PeakStorageBytes, &d.Assets
			out.Components, out.PeakComponents = &d.Components, &d.PeakComponents
		}
		months = append(months, out)
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from.Format("2006-01"), "to": to.Format("2006-01"), "retentionMonths": UsageRetentionMonths,
		"collectedAt": rfc3339(now), "months": months})
}
