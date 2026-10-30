package vuln

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/events"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ---- repository evaluation (repository.evaluate) ----
//
// The monitored repositories' components are looked up as the scan's are
// (Check, online within the cache or from the local data) and stored per
// component in eval_components. A run evaluates only the components it has
// no current result for: new ones, those evaluated against other advisory
// data (an import or sync since, or another mode), those whose online
// answer is older than cacheHours (online mode: the API may know more by
// now) and those whose online lookup failed. Advisories that a component
// did not have before are announced as component.vulnerable events.

// GroupMembers is the set of repositories some group lists as a member.
func GroupMembers(snap *repo.Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, rp := range snap.Sorted {
		if rp.Type == "group" {
			for _, m := range rp.Members {
				out[m] = true
			}
		}
	}
	return out
}

// Monitored reports whether repository.evaluate evaluates rp: a hosted or
// proxy repository of a format with OSV data whose evaluation setting is
// "enabled", or follows the settings and is hosted with evaluateHosted, or
// a member of a group (members, GroupMembers) with evaluateGroupMembers.
func Monitored(set Settings, members map[string]bool, rp *repo.Repository) bool {
	if rp.Type == "group" || Ecosystems[rp.Format] == "" {
		return false
	}
	switch rp.Evaluation {
	case "enabled":
		return true
	case "disabled":
		return false
	}
	return (rp.Type == "hosted" && set.EvaluateHosted) || (members[rp.Name] && set.EvaluateGroupMembers)
}

// EvalTotals counts what an evaluation run did.
type EvalTotals struct {
	Repositories int // evaluated
	Components   int // of the evaluated repositories
	Evaluated    int // looked up this run; the others were current
	Vulnerable   int // of Components
	Unchecked    int // looked up, but the online lookup failed: local data only
	NewFindings  int // components with advisories they did not have before
	Removed      int // results of components no longer stored
	Cleared      int // repositories no longer monitored, whose results went
}

const (
	evalBatch = 500 // components looked up at once
	// evalEvents bounds the component.vulnerable events of one run (the
	// first evaluation of a large repository finds every vulnerable
	// component new); the log and the dashboard have the rest.
	evalEvents = 100
)

// dataGen names the advisory data a result is from: the mode and the
// last import or sync, which every node reads from the database.
func (s *Service) dataGen(ctx context.Context, set Settings) (string, error) {
	var db Database
	if _, err := s.DB.GetSettingJSON(ctx, databaseKey, &db); err != nil {
		return "", err
	}
	return set.Mode + "|" + db.ImportedAt.UTC().Format(time.RFC3339Nano), nil
}

// Evaluate evaluates every monitored repository (only the one named,
// unless only is "") and drops the results of repositories no longer
// monitored; logf reports progress. now stamps the results.
func (s *Service) Evaluate(ctx context.Context, only string, now func() time.Time, logf func(format string, args ...any)) (EvalTotals, error) {
	var t EvalTotals
	set := s.Settings(ctx)
	if !set.Enabled {
		return t, errors.New("vulnerability data is disabled in the settings")
	}
	gen, err := s.dataGen(ctx, set)
	if err != nil {
		return t, err
	}
	usage, err := s.DB.RepoUsages(ctx)
	if err != nil {
		return t, err
	}
	snap := s.Repos.Snapshot()
	members := GroupMembers(snap)
	budget := evalEvents
	for _, rp := range snap.Sorted {
		if only != "" && rp.Name != only {
			continue
		}
		if err := ctx.Err(); err != nil {
			return t, err
		}
		if !Monitored(set, members, rp) {
			if usage[rp.Name].Eval.At.IsZero() {
				continue
			}
			if _, err := s.DB.ClearEvaluation(ctx, rp.Name); err != nil {
				return t, fmt.Errorf("repository %q: %w", rp.Name, err)
			}
			logf("%s: no longer monitored: its results were removed", rp.Name)
			t.Cleared++
			continue
		}
		if usage[rp.Name].Eval.At.IsZero() { // stamped before its first rows, so a run that dies leaves none unaccounted for
			if _, err := s.DB.SummarizeEvaluation(ctx, rp.Name, now()); err != nil {
				return t, err
			}
		}
		r, err := s.evaluateRepo(ctx, rp, set, gen, now, &budget)
		if err != nil {
			return t, fmt.Errorf("repository %q: %w", rp.Name, err)
		}
		logf("%s: %d components, %d looked up, %d vulnerable (%d with new advisories), %d not checked online, %d removed",
			rp.Name, r.Components, r.Evaluated, r.Vulnerable, r.NewFindings, r.Unchecked, r.Removed)
		t.Repositories++
		t.Components += r.Components
		t.Evaluated += r.Evaluated
		t.Vulnerable += r.Vulnerable
		t.Unchecked += r.Unchecked
		t.NewFindings += r.NewFindings
		t.Removed += r.Removed
	}
	if budget < 0 {
		logf("%d components with new advisories were not announced as component.vulnerable events (at most %d per run)", -budget, evalEvents)
	}
	return t, nil
}

// compKey is a component as the repository shows it.
type compKey struct{ group, name, version string }

func (s *Service) evaluateRepo(ctx context.Context, rp *repo.Repository, set Settings, gen string, now func() time.Time, budget *int) (EvalTotals, error) {
	var t EvalTotals
	// ponytail: every component of the repository is held while it is
	// evaluated (~150 bytes each: 1M components ~150 MB); stage them in a
	// table if repositories grow past that.
	found := map[compKey]Package{}
	after := ""
	for {
		page, err := s.DB.ScanAssets(ctx, rp.Name, after, scanPage)
		if err != nil {
			return t, err
		}
		for _, a := range page {
			g, name, v, ok := component.DeriveAsset(rp.Format, a.Path)
			if !ok {
				continue
			}
			if p, ok := FromComponent(rp.Format, g, name, v); ok {
				found[compKey{g, name, v}] = p
			}
		}
		if len(page) < scanPage {
			break
		}
		after = page[len(page)-1].Path
	}
	t.Components = len(found)
	start := now()
	stale := time.Duration(set.CacheHours) * time.Hour
	var gone []meta.EvalComponent
	before := map[compKey][]string{} // the advisories of the components evaluated again
	err := s.DB.WalkEvalComponents(ctx, rp.Name, func(c meta.EvalComponent) {
		k := compKey{c.Group, c.Name, c.Version}
		p, ok := found[k]
		switch {
		case !ok:
			gone = append(gone, c)
		case c.Gen == gen && !c.Unchecked && p == Package{Ecosystem: c.Ecosystem, Name: c.Package, Version: c.PackageVersion} &&
			(set.Mode != "online" || start.Sub(c.Evaluated) < stale):
			delete(found, k) // current
			if len(c.IDs) > 0 {
				t.Vulnerable++
			}
		default:
			before[k] = c.IDs
		}
	})
	if err != nil {
		return t, err
	}
	keys := make([]compKey, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b compKey) int {
		return cmp.Or(strings.Compare(a.group, b.group), strings.Compare(a.name, b.name), strings.Compare(a.version, b.version))
	})
	for len(keys) > 0 {
		batch := keys[:min(evalBatch, len(keys))]
		keys = keys[len(batch):]
		if err := s.evaluateBatch(ctx, rp, gen, now, batch, found, before, budget, &t); err != nil {
			return t, err
		}
	}
	if err := s.DB.DeleteEvalComponents(ctx, rp.Name, gone); err != nil {
		return t, err
	}
	t.Removed = len(gone)
	_, err = s.DB.SummarizeEvaluation(ctx, rp.Name, now())
	return t, err
}

func (s *Service) evaluateBatch(ctx context.Context, rp *repo.Repository, gen string, now func() time.Time, batch []compKey,
	found map[compKey]Package, before map[compKey][]string, budget *int, t *EvalTotals) error {
	index := map[Package]int{}
	var pkgs []Package
	for _, k := range batch {
		if _, ok := index[found[k]]; !ok {
			index[found[k]] = len(pkgs)
			pkgs = append(pkgs, found[k])
		}
	}
	res, err := s.Check(ctx, pkgs, true)
	if err != nil {
		return err
	}
	at := now()
	rows := make([]meta.EvalComponent, len(batch))
	type finding struct {
		label, detail string
	}
	var news []finding
	for i, k := range batch {
		p := found[k]
		r := res[index[p]]
		ids := make([]string, len(r.Vulnerabilities))
		for j, v := range r.Vulnerabilities {
			ids[j] = v.ID
		}
		adv, err := json.Marshal(r.Vulnerabilities)
		if err != nil {
			return err
		}
		sev := r.MaxSeverity()
		rows[i] = meta.EvalComponent{Repo: rp.Name, Group: k.group, Name: k.name, Version: k.version,
			Ecosystem: p.Ecosystem, Package: p.Name, PackageVersion: p.Version, Gen: gen, Evaluated: at, IDs: ids,
			Severity: sev, Rank: Rank(sev), Malicious: r.Malicious(), Unchecked: r.Status == "unknown", Advisories: string(adv)}
		t.Evaluated++
		if len(ids) > 0 {
			t.Vulnerable++
		}
		if r.Status == "unknown" {
			t.Unchecked++
		}
		var added []string
		for _, id := range ids {
			if !slices.Contains(before[k], id) {
				added = append(added, id)
			}
		}
		if len(added) > 0 {
			t.NewFindings++
			label := component.Component{Format: rp.Format, Group: k.group, Name: k.name, Version: k.version}.Label()
			news = append(news, finding{label, truncate(cmp.Or(sev, "unknown")+":"+strings.Join(added, ","), 500)})
		}
	}
	if err := s.DB.PutEvalComponents(ctx, rows); err != nil {
		return err
	}
	for _, f := range news {
		if *budget--; *budget >= 0 {
			s.emit("component.vulnerable", events.System, rp.Name, f.label, f.detail)
		}
	}
	return nil
}
