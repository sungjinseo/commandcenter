package tasks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// summary is what a run reports besides its error.
type summary struct {
	msg                    string
	removed, assets, bytes int64 // assets: cleanup only
}

// typeDef describes a task type: its display name, how its params are
// validated (returning the canonical JSON) and how it runs.
type typeDef struct {
	name   string
	params func(s *Service, raw json.RawMessage, check bool) (string, []string) // check: against the repositories
	run    func(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error)
	fields []ParamSpec // for the UI's form
}

var types = map[string]typeDef{
	TypeCompact: {name: "Compact blob store", params: compactParams, run: runCompact, fields: blobStoreFields},
	TypeCleanup: {name: "Cleanup repositories", params: cleanupParamsOf, run: runCleanup, fields: cleanupFields},
	TypeRebuild: {name: "Rebuild repository metadata", params: rebuildParamsOf, run: runRebuild, fields: []ParamSpec{
		{Name: "repo", Type: "repository", Help: "a Maven hosted or proxy, or an apt, yum, helm or conda hosted repository; empty: every one"},
		{Name: "format", Type: "format", Help: "with an empty repo: only repositories of this format"},
		{Name: "resetProxyMetadata", Type: "bool", Help: "apt proxies too: drop their cached metadata, so it is fetched again (after a change of the remote)"}}},
	TypeVerify: {name: "Verify blobs", params: verifyParamsOf, run: runVerify, fields: verifyFields},
	TypePurge:  {name: "Remove Maven snapshots", params: purgeParamsOf, run: runPurge, fields: purgeFields},
	TypeRecalc: {name: "Recalculate blob store size", params: compactParams, run: runRecalc, fields: blobStoreFields},
	TypeTemp:   {name: "Delete blob store temp files", params: tempParamsOf, run: runTemp, fields: tempFields},

	TypePurgeUserTokens: {name: "Purge expired user tokens", params: noParams, run: runPurgeUserTokens, fields: []ParamSpec{}},
	TypeExport:          {name: "Export repositories", params: exportParamsOf, run: runExport, fields: exportFields},
	TypePurgeUnused:     {name: "Delete unused components", params: purgeUnusedParamsOf, run: runPurgeUnused, fields: purgeUnusedFields},
	TypeImport:          {name: "Import repositories", params: importParamsOf, run: runImport, fields: importFields},
}

var blobStoreFields = []ParamSpec{{Name: "blobStore", Type: "blobStore", Help: "empty: every blob store"}}

// ---- blobstore.compact ----

// compactParams takes an optional blobStore name; empty compacts every store.
// When check is set, a named store must exist.
func compactParams(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p struct {
		BlobStore string `json:"blobStore,omitempty"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error() + " (blobstore.compact takes an optional blobStore)"}
	}
	if errs := checkBlobStore(s, p.BlobStore, check); errs != nil {
		return "", errs
	}
	return encodeParams(p), nil
}

// checkBlobStore checks, when check is set, that a named blob store exists.
func checkBlobStore(s *Service, name string, check bool) []string {
	if !check || name == "" {
		return nil
	}
	if _, err := s.DB.GetBlobStore(context.Background(), name); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return []string{fmt.Sprintf("blob store %q does not exist", name)}
		}
		return []string{err.Error()}
	}
	return nil
}

func runCompact(ctx context.Context, s *Service, t meta.Task, _ string, log *RunLog) (summary, error) {
	if s.GC == nil {
		return summary{msg: "garbage collection is not available"}, errors.New("garbage collection is not available")
	}
	var p struct {
		BlobStore string `json:"blobStore,omitempty"`
	}
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if p.BlobStore != "" {
		log.Infof("compacting the %q blob store: removing blobs no asset references", p.BlobStore)
	} else {
		log.Infof("compacting every blob store: removing blobs no asset references")
	}
	n, freed, err := s.GC(ctx, p.BlobStore)
	sum := summary{msg: fmt.Sprintf("removed %d blobs (%d bytes)", n, freed), removed: int64(n), bytes: freed}
	if err != nil {
		log.Errorf("compact failed after removing %d blobs (%d bytes): %v", n, freed, err)
		return sum, err
	}
	log.Infof("%s", sum.msg)
	return sum, nil
}

// ---- repository.cleanup ----

type cleanupParams struct {
	Repos []string `json:"repos"` // empty: every repository with a cleanup policy
	// The run limits (core-15); 0 means the default. MaxComponents is how
	// many components one run deletes in all (DefaultMaxComponents),
	// ScanAssets how many assets one run reads per repository and policy
	// (cleanup.DefaultScanBudget), in scans of at most the default each, so
	// memory stays bounded; the next run continues where a limit stopped
	// this one.
	MaxComponents int `json:"maxComponents,omitempty"`
	ScanAssets    int `json:"scanAssets,omitempty"`
}

// maxRunLimit bounds the run limits a task may set.
const maxRunLimit = 1_000_000_000

var cleanupFields = []ParamSpec{
	{Name: "repos", Type: "repositories", Help: "empty: every repository with a cleanup policy"},
	{Name: "maxComponents", Type: "int", Default: DefaultMaxComponents, Help: "components one run deletes at most; the next run continues"},
	{Name: "scanAssets", Type: "int", Default: cleanup.DefaultScanBudget, Help: "assets one run reads per repository and policy at most; the next run continues"},
}

func cleanupParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p cleanupParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	var errs []string
	p.Repos, errs = cleanupRepos(s, p.Repos, check)
	errs = append(errs, runLimitErrors(p.MaxComponents, p.ScanAssets)...)
	return encodeParams(p), errs
}

// runLimitErrors checks maxComponents and scanAssets.
func runLimitErrors(maxComponents, scanAssets int) []string {
	var errs []string
	for _, l := range []struct {
		name string
		v    int
	}{{"maxComponents", maxComponents}, {"scanAssets", scanAssets}} {
		if l.v < 0 || l.v > maxRunLimit {
			errs = append(errs, fmt.Sprintf("params.%s must be from 0 (the default) to %d", l.name, maxRunLimit))
		}
	}
	return errs
}

// cleanupRepos returns the repositories a cleanup task names, sorted and
// without repeats, and, when check is set, why cleanup cannot clean some.
func cleanupRepos(s *Service, names []string, check bool) ([]string, []string) {
	if len(names) > maxCleanupRepos {
		return nil, []string{fmt.Sprintf("params.repos lists at most %d repositories", maxCleanupRepos)}
	}
	var errs []string
	seen := map[string]bool{}
	repos := []string{}
	snap := s.Repos.Snapshot()
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		rp := snap.ByName[name]
		switch {
		case !check:
		case rp == nil:
			errs = append(errs, fmt.Sprintf("repository %q does not exist", name))
		case rp.Type == "group":
			errs = append(errs, fmt.Sprintf("repository %q is a group: clean its members", name))
		case rp.Type == "hosted" && rp.Format == "cargo":
			errs = append(errs, fmt.Sprintf("repository %q is Cargo hosted: yank crates, or delete versions in the web UI", name))
		case rp.Format == "docker":
			errs = append(errs, fmt.Sprintf("repository %q is a docker repository: delete manifests in the web UI or with the registry API", name))
		}
		repos = append(repos, name)
	}
	sort.Strings(repos)
	return repos, errs
}

// policyRecheck is how many components a cleanup run handles between two
// reads of the repository's policy (PolicyChanged forces one earlier).
const policyRecheck = 50

// cursorPrefix and cursorKey name the settings keys that hold a cleanup
// task's scan cursor per repository: where its next evaluation starts.
// The repository's first policy has the key of the repository, a further
// one its own (policyCursorKey).
func cursorPrefix(taskID string) string        { return "tasks.cursor/" + taskID + "/" }
func cursorKey(taskID, repoName string) string { return cursorPrefix(taskID) + repoName }

func policyCursorKey(taskID string, rp *repo.Repository, policy string) string {
	if policy == rp.CleanupPolicy {
		return cursorKey(taskID, rp.Name)
	}
	return cursorKey(taskID, rp.Name) + "/" + policy
}

// samePolicy reports whether a policy read again is the one the run
// evaluated with.
func samePolicy(a, b cleanup.Policy) bool {
	return a.Format == b.Format && a.Criteria == b.Criteria && a.Updated.Equal(b.Updated)
}

// cleanupRun is one run of a cleanup task: its limits and totals.
type cleanupRun struct {
	s                    *Service
	t                    meta.Task
	actor                string
	log                  *RunLog
	limit, chunk, budget int // components per run; assets per scan, and per repository and policy
	comps, assets, bytes int64
	failures, cleaned    int
	limited, flushed     bool // flushed: the other nodes' download counts were asked for
}

func runCleanup(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	var p cleanupParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if s.Deleters == nil {
		return summary{msg: "cleanup is not available"}, errors.New("cleanup is not available")
	}
	snap := s.Repos.Snapshot()
	names := p.Repos
	if len(names) == 0 {
		for _, rp := range snap.Sorted {
			if rp.CleanupPolicy != "" {
				names = append(names, rp.Name)
			}
		}
		if len(names) == 0 {
			log.Infof("no repository has a cleanup policy")
		}
	}
	chunk := cmp.Or(s.ScanBudget, cleanup.DefaultScanBudget)
	r := &cleanupRun{s: s, t: t, actor: actor, log: log, limit: cmp.Or(p.MaxComponents, s.MaxComponents, DefaultMaxComponents),
		chunk: chunk, budget: cmp.Or(p.ScanAssets, chunk)}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return r.summary(), err
		}
		if r.comps >= int64(r.limit) {
			r.limited = true
			break
		}
		rp := s.Repos.Get(name)
		if rp == nil {
			log.Warnf("%s: skipped: the repository no longer exists", name)
			continue
		}
		policies := rp.CleanupPolicyNames()
		if len(policies) == 0 {
			log.Warnf("%s: skipped: no cleanup policy is set", name)
			continue
		}
		for _, pname := range policies {
			if r.comps >= int64(r.limit) {
				r.limited = true
				break
			}
			// Taken before reading the policy, so an edit made while the
			// policy is read or the repository evaluated is noticed at the
			// first component.
			sig := s.policySig.Load()
			policy, err := cleanup.GetPolicy(ctx, s.DB, pname)
			if errors.Is(err, meta.ErrNotFound) {
				log.Warnf("%s: skipped: cleanup policy %q does not exist", name, pname)
				continue
			}
			if err != nil {
				r.failures++
				log.Errorf("%s: reading cleanup policy %q: %v", name, pname, err)
				continue
			}
			if err := r.clean(ctx, rp, policy, true, sig, policyCursorKey(t.ID, rp, pname)); err != nil {
				return r.summary(), err
			}
		}
	}
	if r.limited {
		log.Warnf("stopped at the limit of %d components per run; the next run continues", r.limit)
	}
	sum := r.summary()
	if r.failures > 0 {
		return sum, fmt.Errorf("%d cleanup steps failed (see the log)", r.failures)
	}
	return sum, nil
}

func (r *cleanupRun) summary() summary { return cleanupSummary(r.comps, r.assets, r.bytes, r.cleaned) }

// clean applies policy to rp: scans of at most r.chunk assets from the
// stored cursor (key) until the run's budget or component limit, or the
// end of the repository, deleting each scan's candidates. It returns an
// error only when ctx ends; other failures are counted and logged.
//
// An assigned policy (a repository's) is followed: the run stops deleting
// when it is edited or unassigned. A policy that is not (purge-unused's)
// only stops with the repository.
func (r *cleanupRun) clean(ctx context.Context, rp *repo.Repository, policy cleanup.Policy, assigned bool, sig uint64, key string) error {
	s, log, name := r.s, r.log, rp.Name
	if !cleanup.FormatMatches(policy.Format, rp.Format) {
		log.Warnf("%s: skipped: cleanup policy %q is for %s repositories, not %s", name, policy.Name, policy.Format, rp.Format)
		return nil
	}
	// Downloads still pending in memory count: write them first, on the
	// other nodes too (once a run).
	if policy.LastDownloadedDays > 0 && !r.flushed {
		s.flushOthers(ctx)
		r.flushed = true
	}
	if s.Metrics != nil {
		if err := s.Metrics.Flush(ctx); err != nil {
			if policy.LastDownloadedDays > 0 {
				r.failures++
				log.Errorf("%s: skipped: could not write the pending download counts that policy %q needs: %v", name, policy.Name, err)
				return nil
			}
			log.Warnf("%s: could not write the pending download counts: %v", name, err)
		}
	}
	first, err := s.DB.GetSetting(ctx, key)
	if err != nil && !errors.Is(err, meta.ErrNotFound) {
		r.failures++
		log.Errorf("%s: reading the scan cursor: %v", name, err)
		return nil
	}
	var rc, ra, rb int64
	var rf, kept, candidates, scanned int
	stopped := "" // why deleting in this repository stopped early
	complete := true
	from, i, evaluated := first, 0, false
	var ev cleanup.Evaluation
	for {
		ev, err = s.evaluate(ctx, rp, policy, from, min(r.chunk, r.budget-scanned))
		if errors.Is(err, cleanup.ErrNotSupported) {
			log.Warnf("%s: skipped: %v", name, err)
			return nil
		}
		if err != nil {
			r.failures++
			log.Errorf("%s: evaluating policy %q: %v", name, policy.Name, err)
			return nil
		}
		if !evaluated {
			r.cleaned, evaluated = r.cleaned+1, true
		}
		scanned += ev.Scanned
		candidates += len(ev.Candidates)
		for _, c := range ev.Candidates {
			if r.comps >= int64(r.limit) {
				r.limited, complete = true, false
				break
			}
			if err := ctx.Err(); err != nil {
				r.assets, r.bytes = r.assets+ra, r.bytes+rb
				return err
			}
			// Follow edits made during the run: the repository replaced, the
			// policy unassigned, or the policy edited.
			if cur := s.Repos.Get(name); cur == nil || cur.Gen != rp.Gen || assigned && !slices.Contains(cur.CleanupPolicyNames(), policy.Name) {
				stopped = "the repository or its cleanup policy assignment changed during the run"
			} else if now := s.policySig.Load(); assigned && (now != sig || (i > 0 && i%policyRecheck == 0)) {
				sig = now
				if fresh, err := cleanup.GetPolicy(ctx, s.DB, policy.Name); err != nil || !samePolicy(fresh, policy) {
					stopped = fmt.Sprintf("cleanup policy %q changed during the run", policy.Name)
				}
			}
			if stopped != "" {
				complete = false
				break
			}
			i++
			res, err := s.Deleters.Delete(ctx, rp, c.Component, r.actor)
			if errors.Is(err, cleanup.ErrChanged) {
				kept++
				if kept <= 20 {
					log.Infof("%s: kept %s: it changed since evaluation (%v)", name, c.Label(), err)
				}
				continue
			}
			if err != nil {
				rf++
				if rf <= 20 {
					log.Errorf("%s: deleting %s: %v", name, c.Label(), err)
				}
				continue
			}
			if res.Assets > 0 {
				rc++
				r.comps++
			}
			ra += int64(res.Assets)
			rb += res.Bytes
		}
		if complete && ev.Next != from {
			if err := s.DB.SetSetting(context.WithoutCancel(ctx), key, ev.Next); err != nil {
				log.Warnf("%s: could not store the scan cursor: %v", name, err)
			}
		}
		if r.comps >= int64(r.limit) && ev.Next != "" {
			r.limited = true
		}
		if !complete || ev.Next == "" || scanned >= r.budget || r.limited {
			break
		}
		from = ev.Next
	}
	if rf > 20 {
		log.Errorf("%s: %d more deletes failed", name, rf-20)
	}
	if stopped != "" {
		log.Warnf("%s: stopped deleting: %s; the next run evaluates it again", name, stopped)
	}
	r.failures += rf
	r.assets, r.bytes = r.assets+ra, r.bytes+rb
	msg := fmt.Sprintf("%s: policy %q: deleted %d of %d candidate components (%d assets, %d bytes)", name, policy.Name, rc, candidates, ra, rb)
	if kept > 0 {
		msg += fmt.Sprintf("; kept %d changed since evaluation", kept)
	}
	start := "the start"
	if first != "" {
		start = strconv.Quote(first)
	}
	switch {
	case !complete:
		// Not every candidate was handled: the next run scans this part again.
		if ev.Truncated || from != "" {
			again := "the start"
			if from != "" {
				again = strconv.Quote(from)
			}
			msg += fmt.Sprintf("; scanned %d assets from %s; the next run scans again from %s", scanned, start, again)
		}
	case ev.Truncated:
		msg += fmt.Sprintf("; scanned %d assets from %s and stopped at the scan budget; the next run resumes at %q", scanned, start, ev.Next)
	case first != "":
		msg += fmt.Sprintf("; scanned %d assets from %s to the end; the next run starts from the beginning", scanned, start)
	}
	log.Infof("%s", msg)
	return nil
}

// ---- repository.purge-unused ----

// purgeParams are repository.purge-unused's (Nexus's "Delete unused
// components", core-42): proxy repositories (empty: every one) whose
// components no one downloaded in the last LastUsed days are deleted;
// components never downloaded stay. It is a cleanup run with that one
// criterion, and the run limits of cleanupParams.
type purgeUnusedParams struct {
	Repos         []string `json:"repos"`
	LastUsed      int      `json:"lastUsed"`
	MaxComponents int      `json:"maxComponents,omitempty"`
	ScanAssets    int      `json:"scanAssets,omitempty"`
}

var purgeUnusedFields = append([]ParamSpec{
	{Name: "repos", Type: "repositories", Help: "proxy repositories; empty: every proxy repository"},
	{Name: "lastUsed", Type: "int", Required: true, Help: "delete the components no one downloaded in the last that many days (never downloaded ones stay)"},
}, cleanupFields[1:]...)

func purgeUnusedParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p purgeUnusedParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	var errs []string
	if p.LastUsed < 1 || p.LastUsed > 36500 {
		errs = append(errs, "params.lastUsed must be from 1 to 36500 days")
	}
	var rerrs []string
	p.Repos, rerrs = cleanupRepos(s, p.Repos, check)
	errs = append(append(errs, rerrs...), runLimitErrors(p.MaxComponents, p.ScanAssets)...)
	for _, name := range p.Repos {
		if rp := s.Repos.Get(name); check && rp != nil && rp.Type != "proxy" {
			errs = append(errs, fmt.Sprintf("repository %q is %s: the task takes proxy repositories", name, rp.Type))
		}
	}
	return encodeParams(p), errs
}

func runPurgeUnused(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	var p purgeUnusedParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil || p.LastUsed < 1 {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %v", cmp.Or(err, errors.New("lastUsed is not set")))
	}
	if s.Deleters == nil {
		return summary{msg: "cleanup is not available"}, errors.New("cleanup is not available")
	}
	names := p.Repos
	if len(names) == 0 {
		for _, rp := range s.Repos.Snapshot().Sorted {
			if rp.Type == "proxy" && rp.Format != "docker" {
				names = append(names, rp.Name)
			}
		}
	}
	chunk := cmp.Or(s.ScanBudget, cleanup.DefaultScanBudget)
	r := &cleanupRun{s: s, t: t, actor: actor, log: log, limit: cmp.Or(p.MaxComponents, s.MaxComponents, DefaultMaxComponents),
		chunk: chunk, budget: cmp.Or(p.ScanAssets, chunk)}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return r.summary(), err
		}
		if r.comps >= int64(r.limit) {
			r.limited = true
			break
		}
		rp := s.Repos.Get(name)
		if rp == nil || rp.Type != "proxy" {
			log.Warnf("%s: skipped: no longer a proxy repository", name)
			continue
		}
		policy := cleanup.Policy{Name: fmt.Sprintf("lastUsed-%d", p.LastUsed), Format: rp.Format,
			Criteria: cleanup.Criteria{LastDownloadedDays: p.LastUsed, ReleaseType: "any"}, DownloadedOnly: true}
		if err := r.clean(ctx, rp, policy, false, 0, cursorKey(t.ID, rp.Name)); err != nil {
			return r.summary(), err
		}
	}
	if r.limited {
		log.Warnf("stopped at the limit of %d components per run; the next run continues", r.limit)
	}
	sum := r.summary()
	if r.failures > 0 {
		return sum, fmt.Errorf("%d purge steps failed (see the log)", r.failures)
	}
	return sum, nil
}

// evaluate is the one cleanup evaluation: what a cleanup run deletes and
// what PreviewCleanup lists, one scan of at most budget assets (0: the
// default) of rp from from.
func (s *Service) evaluate(ctx context.Context, rp *repo.Repository, p cleanup.Policy, from string, budget int) (cleanup.Evaluation, error) {
	return (&cleanup.Evaluator{DB: s.DB, Now: s.now}).EvaluateFrom(ctx, rp, p, budget, from)
}

// PreviewCleanup calls visit with every component policy p would delete
// from rp now, deleting nothing: the evaluation cleanup runs make, scan
// after scan from the start of the repository to its end, as successive
// runs cover it. Pending download counts are written first, as a run does;
// when that fails and p uses lastDownloadedDays, the preview fails.
func (s *Service) PreviewCleanup(ctx context.Context, rp *repo.Repository, p cleanup.Policy, visit func(cleanup.Candidate) error) error {
	if p.LastDownloadedDays > 0 {
		s.flushOthers(ctx)
	}
	if s.Metrics != nil {
		if err := s.Metrics.Flush(ctx); err != nil && p.LastDownloadedDays > 0 {
			return fmt.Errorf("write the pending download counts: %w", err)
		}
	}
	from := ""
	for {
		ev, err := s.evaluate(ctx, rp, p, from, s.ScanBudget)
		if err != nil {
			return err
		}
		for _, c := range ev.Candidates {
			if err := visit(c); err != nil {
				return err
			}
		}
		if ev.Next == "" {
			return nil
		}
		from = ev.Next
	}
}

func cleanupSummary(comps, assets, bytes int64, repos int) summary {
	return summary{
		msg:     fmt.Sprintf("deleted %d components (%d assets, %d bytes) in %d repositories", comps, assets, bytes, repos),
		removed: comps, assets: assets, bytes: bytes,
	}
}

// cleanupDetail is the detail of the cleanup event.
func cleanupDetail(sum summary) string {
	return fmt.Sprintf("components=%d assets=%d bytes=%d", sum.removed, sum.assets, sum.bytes)
}

// ---- repository.rebuild-metadata ----

// rebuildParams names one repository (repo; repository is accepted as an
// alias) or, when empty, every repository the task can rebuild, optionally
// of one format. ResetProxyMetadata (core-45, Nexus's apt resetProxyMetadata)
// takes in apt proxies too: their cached dists/ files (Release, InRelease,
// Packages) are dropped, so the next request fetches them again, as after
// a change of the remote.
type rebuildParams struct {
	Repo               string `json:"repo,omitempty"`
	Repository         string `json:"repository,omitempty"`
	Format             string `json:"format,omitempty"`
	ResetProxyMetadata bool   `json:"resetProxyMetadata,omitempty"`
}

// rebuildable reports whether the task can rebuild rp's metadata: a Maven
// hosted or proxy repository, a hosted one of a format with a rebuilder,
// or with reset an apt proxy.
func (s *Service) rebuildable(rp *repo.Repository, reset bool) bool {
	switch {
	case rp.Format == "maven":
		return rp.Type == "hosted" || rp.Type == "proxy"
	case rp.Format == "apt" && rp.Type == "proxy":
		return reset
	}
	return rp.Type == "hosted" && s.Rebuilders[rp.Format] != nil
}

func (s *Service) rebuildFormats() []string {
	out := []string{"maven"}
	for f := range s.Rebuilders {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func rebuildParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p rebuildParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	if p.Repo == "" {
		p.Repo = p.Repository
	}
	p.Repository = ""
	if p.Format != "" && !slices.Contains(s.rebuildFormats(), p.Format) {
		return "", []string{fmt.Sprintf("params.format %q is not one of %s", p.Format, strings.Join(s.rebuildFormats(), ", "))}
	}
	rp := s.Repos.Get(p.Repo)
	switch {
	case p.Repo == "" || !check:
	case rp == nil:
		return "", []string{fmt.Sprintf("repository %q does not exist", p.Repo)}
	case rp.Format == "apt" && rp.Type == "proxy" && !p.ResetProxyMetadata:
		return "", []string{fmt.Sprintf("repository %q is an apt proxy: set resetProxyMetadata to drop its cached metadata", p.Repo)}
	case !s.rebuildable(rp, p.ResetProxyMetadata):
		return "", []string{fmt.Sprintf("repository %q is %s %s: the task takes a Maven hosted or proxy repository, an apt proxy with resetProxyMetadata, or a hosted repository of %s", p.Repo, rp.Format, rp.Type, strings.Join(s.rebuildFormats(), ", "))}
	case p.Format != "" && p.Format != rp.Format:
		return "", []string{fmt.Sprintf("repository %q is %s, not %s", p.Repo, rp.Format, p.Format)}
	}
	return encodeParams(p), nil
}

func runRebuild(ctx context.Context, s *Service, t meta.Task, _ string, log *RunLog) (summary, error) {
	var p rebuildParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	var repos []*repo.Repository
	if p.Repo != "" {
		rp := s.Repos.Get(p.Repo)
		switch {
		case rp == nil:
			return summary{}, fmt.Errorf("repository %q no longer exists: edit or delete this task", p.Repo)
		case !s.rebuildable(rp, p.ResetProxyMetadata):
			return summary{}, fmt.Errorf("repository %q is now %s %s, which the task cannot rebuild: edit or delete this task", p.Repo, rp.Format, rp.Type)
		}
		return rebuildOne(ctx, s, rp, log)
	}
	for _, rp := range s.Repos.Snapshot().Sorted {
		if s.rebuildable(rp, p.ResetProxyMetadata) && (p.Format == "" || p.Format == rp.Format) {
			repos = append(repos, rp)
		}
	}
	var removed int64
	failed := 0
	for _, rp := range repos {
		if err := ctx.Err(); err != nil {
			return summary{msg: fmt.Sprintf("rebuilt the metadata of %d repositories", len(repos)), removed: removed}, err
		}
		sum, err := rebuildOne(ctx, s, rp, log)
		removed += sum.removed
		if err != nil {
			failed++
			log.Errorf("%s: %v", rp.Name, err)
		}
	}
	sum := summary{msg: fmt.Sprintf("rebuilt the metadata of %d repositories (%d failed)", len(repos), failed), removed: removed}
	log.Infof("%s", sum.msg)
	if failed > 0 {
		return sum, fmt.Errorf("%d repositories failed (see the log)", failed)
	}
	return sum, nil
}

// resetAptProxy drops the cached dists/ files of the apt proxy rp in one
// transaction; the packages under pool/ stay.
func resetAptProxy(ctx context.Context, s *Service, rp *repo.Repository, log *RunLog) (summary, error) {
	if s.Store == nil {
		return summary{msg: "metadata reset is not available"}, errors.New("metadata reset is not available")
	}
	n := 0
	err := s.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		n = 0
		list, err := tx.ListAssets(ctx, rp.Name, "dists/")
		if err != nil {
			return err
		}
		for _, a := range list {
			if _, err := tx.DeleteAsset(ctx, rp.Name, a.Path); err != nil && !errors.Is(err, meta.ErrNotFound) {
				return err
			}
			n++
		}
		return nil
	})
	sum := summary{msg: fmt.Sprintf("%s: dropped %d cached metadata files; the next requests fetch them from the remote", rp.Name, n), removed: int64(n)}
	if err == nil {
		log.Infof("%s", sum.msg)
	}
	return sum, err
}

// rebuildOne rebuilds rp's metadata with the right rebuilder.
func rebuildOne(ctx context.Context, s *Service, rp *repo.Repository, log *RunLog) (summary, error) {
	if rp.Format == "apt" && rp.Type == "proxy" {
		return resetAptProxy(ctx, s, rp, log)
	}
	if rp.Format != "maven" {
		r := s.Rebuilders[rp.Format]
		if r == nil {
			return summary{msg: "metadata rebuild is not available"}, errors.New("metadata rebuild is not available for " + rp.Format)
		}
		err := r.RebuildMetadata(ctx, rp, log.Infof)
		sum := summary{msg: fmt.Sprintf("%s: rebuilt the %s metadata", rp.Name, rp.Format)}
		if err == nil {
			log.Infof("%s", sum.msg)
		}
		return sum, err
	}
	if s.Maven == nil {
		return summary{msg: "metadata rebuild is not available"}, errors.New("metadata rebuild is not available")
	}
	if rp.Type == "proxy" {
		// A proxy's metadata is the remote's: what is repaired is the cache,
		// which older builds filled with files that are not Maven files (QA L-12).
		log.Infof("%s: removing cached files that are not Maven files", rp.Name)
		n, err := s.Maven.PurgeUnfetchable(ctx, rp, log.Infof)
		sum := summary{msg: fmt.Sprintf("%s: removed %d cached files that are not Maven files", rp.Name, n), removed: int64(n)}
		log.Infof("%s", sum.msg)
		return sum, err
	}
	log.Infof("%s: rebuilding maven-metadata.xml of every GA", rp.Name)
	res, err := s.Maven.RebuildMetadata(ctx, rp, s.RebuildScan, log.Infof)
	sum := summary{msg: fmt.Sprintf("%s: rebuilt %d GA metadata documents, removed %d; refreshed %d SNAPSHOT version documents",
		rp.Name, res.GAs, res.Removed, res.Snapshots), removed: int64(res.Removed)}
	if res.Truncated {
		sum.msg += " (the scan stopped at its budget)"
	}
	if res.OrphansTruncated {
		sum.msg += " (orphan documents were checked up to the cap)"
	}
	if res.SnapshotsTruncated {
		sum.msg += " (SNAPSHOT version documents were checked up to the cap)"
	}
	log.Infof("%s", sum.msg)
	if err == nil && res.Failed > 0 {
		err = fmt.Errorf("%d metadata documents failed (see the log)", res.Failed)
	}
	return sum, err
}
