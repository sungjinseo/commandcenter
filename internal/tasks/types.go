package tasks

import (
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
	TypeCleanup: {name: "Cleanup repositories", params: cleanupParamsOf, run: runCleanup,
		fields: []ParamSpec{{Name: "repos", Type: "repositories", Help: "empty: every repository with a cleanup policy"}}},
	TypeRebuild: {name: "Rebuild repository metadata", params: rebuildParamsOf, run: runRebuild, fields: []ParamSpec{
		{Name: "repo", Type: "repository", Help: "a Maven hosted or proxy, or an apt, yum, helm or conda hosted repository; empty: every one"},
		{Name: "format", Type: "format", Help: "with an empty repo: only repositories of this format"}}},
	TypeVerify: {name: "Verify blobs", params: verifyParamsOf, run: runVerify, fields: verifyFields},
	TypePurge:  {name: "Remove Maven snapshots", params: purgeParamsOf, run: runPurge, fields: purgeFields},
	TypeRecalc: {name: "Recalculate blob store size", params: compactParams, run: runRecalc, fields: blobStoreFields},
	TypeTemp:   {name: "Delete blob store temp files", params: compactParams, run: runTemp, fields: blobStoreFields},

	TypePurgeUserTokens: {name: "Purge expired user tokens", params: noParams, run: runPurgeUserTokens, fields: []ParamSpec{}},
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
	if check && p.BlobStore != "" {
		if _, err := s.DB.GetBlobStore(context.Background(), p.BlobStore); err != nil {
			if errors.Is(err, meta.ErrNotFound) {
				return "", []string{fmt.Sprintf("blob store %q does not exist", p.BlobStore)}
			}
			return "", []string{err.Error()}
		}
	}
	return encodeParams(p), nil
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
}

func cleanupParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p cleanupParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	if len(p.Repos) > maxCleanupRepos {
		return "", []string{fmt.Sprintf("params.repos lists at most %d repositories", maxCleanupRepos)}
	}
	var errs []string
	seen := map[string]bool{}
	repos := []string{}
	snap := s.Repos.Snapshot()
	for _, name := range p.Repos {
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
	return encodeParams(cleanupParams{Repos: repos}), errs
}

// policyRecheck is how many components a cleanup run handles between two
// reads of the repository's policy (PolicyChanged forces one earlier).
const policyRecheck = 50

// cursorPrefix and cursorKey name the settings keys that hold a cleanup
// task's scan cursor per repository: where its next evaluation starts.
func cursorPrefix(taskID string) string        { return "tasks.cursor/" + taskID + "/" }
func cursorKey(taskID, repoName string) string { return cursorPrefix(taskID) + repoName }

// samePolicy reports whether a policy read again is the one the run
// evaluated with.
func samePolicy(a, b cleanup.Policy) bool {
	return a.Format == b.Format && a.Criteria == b.Criteria && a.Updated.Equal(b.Updated)
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
	limit := s.MaxComponents
	if limit <= 0 {
		limit = DefaultMaxComponents
	}
	eval := &cleanup.Evaluator{DB: s.DB, Now: s.now}

	var comps, assets, bytes int64
	var failures, cleaned int
	limited := false
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return cleanupSummary(comps, assets, bytes, cleaned), err
		}
		if comps >= int64(limit) {
			limited = true
			break
		}
		rp := s.Repos.Get(name)
		if rp == nil {
			log.Warnf("%s: skipped: the repository no longer exists", name)
			continue
		}
		if rp.CleanupPolicy == "" {
			log.Warnf("%s: skipped: no cleanup policy is set", name)
			continue
		}
		// Taken before reading the policy, so an edit made while the policy is
		// read or the repository evaluated is noticed at the first component.
		sig := s.policySig.Load()
		policy, err := cleanup.GetPolicy(ctx, s.DB, rp.CleanupPolicy)
		if errors.Is(err, meta.ErrNotFound) {
			log.Warnf("%s: skipped: cleanup policy %q does not exist", name, rp.CleanupPolicy)
			continue
		}
		if err != nil {
			failures++
			log.Errorf("%s: reading cleanup policy %q: %v", name, rp.CleanupPolicy, err)
			continue
		}
		if !cleanup.FormatMatches(policy.Format, rp.Format) {
			log.Warnf("%s: skipped: cleanup policy %q is for %s repositories, not %s", name, policy.Name, policy.Format, rp.Format)
			continue
		}
		// Downloads still pending in memory count: write them first.
		if s.Metrics != nil {
			if err := s.Metrics.Flush(ctx); err != nil {
				if policy.LastDownloadedDays > 0 {
					failures++
					log.Errorf("%s: skipped: could not write the pending download counts that policy %q needs: %v", name, policy.Name, err)
					continue
				}
				log.Warnf("%s: could not write the pending download counts: %v", name, err)
			}
		}
		key := cursorKey(t.ID, name)
		from, err := s.DB.GetSetting(ctx, key)
		if err != nil && !errors.Is(err, meta.ErrNotFound) {
			failures++
			log.Errorf("%s: reading the scan cursor: %v", name, err)
			continue
		}
		ev, err := eval.EvaluateFrom(ctx, rp, policy, s.ScanBudget, from)
		if errors.Is(err, cleanup.ErrNotSupported) {
			log.Warnf("%s: skipped: %v", name, err)
			continue
		}
		if err != nil {
			failures++
			log.Errorf("%s: evaluating policy %q: %v", name, policy.Name, err)
			continue
		}
		cleaned++
		var rc, ra, rb int64
		var rf, kept int
		stopped := "" // why deleting in this repository stopped early
		complete := true
		for i, c := range ev.Candidates {
			if comps >= int64(limit) {
				limited, complete = true, false
				break
			}
			if err := ctx.Err(); err != nil {
				return cleanupSummary(comps, assets+ra, bytes+rb, cleaned), err
			}
			// Follow edits made during the run: the repository replaced, its
			// policy unassigned or changed, or the policy edited.
			if cur := s.Repos.Get(name); cur == nil || cur.Gen != rp.Gen || cur.CleanupPolicy != rp.CleanupPolicy {
				stopped = "the repository or its cleanup policy assignment changed during the run"
			} else if now := s.policySig.Load(); now != sig || (i > 0 && i%policyRecheck == 0) {
				sig = now
				if fresh, err := cleanup.GetPolicy(ctx, s.DB, policy.Name); err != nil || !samePolicy(fresh, policy) {
					stopped = fmt.Sprintf("cleanup policy %q changed during the run", policy.Name)
				}
			}
			if stopped != "" {
				complete = false
				break
			}
			res, err := s.Deleters.Delete(ctx, rp, c.Component, actor)
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
				comps++
			}
			ra += int64(res.Assets)
			rb += res.Bytes
		}
		if rf > 20 {
			log.Errorf("%s: %d more deletes failed", name, rf-20)
		}
		if stopped != "" {
			log.Warnf("%s: stopped deleting: %s; the next run evaluates it again", name, stopped)
		}
		failures += rf
		assets, bytes = assets+ra, bytes+rb
		msg := fmt.Sprintf("%s: policy %q: deleted %d of %d candidate components (%d assets, %d bytes)", name, policy.Name, rc, len(ev.Candidates), ra, rb)
		if kept > 0 {
			msg += fmt.Sprintf("; kept %d changed since evaluation", kept)
		}
		start := "the start"
		if from != "" {
			start = strconv.Quote(from)
		}
		switch {
		case !complete:
			// Not every candidate was handled: the next run scans this part again.
			if ev.Truncated || from != "" {
				msg += fmt.Sprintf("; scanned %d assets from %s, which the next run scans again", ev.Scanned, start)
			}
		case ev.Truncated:
			msg += fmt.Sprintf("; scanned %d assets from %s and stopped at the scan budget; the next run resumes at %q", ev.Scanned, start, ev.Next)
		case from != "":
			msg += fmt.Sprintf("; scanned %d assets from %s to the end; the next run starts from the beginning", ev.Scanned, start)
		}
		log.Infof("%s", msg)
		if complete && ev.Next != from {
			if err := s.DB.SetSetting(context.WithoutCancel(ctx), key, ev.Next); err != nil {
				log.Warnf("%s: could not store the scan cursor: %v", name, err)
			}
		}
	}
	if limited {
		log.Warnf("stopped at the limit of %d components per run; the next run continues", limit)
	}
	sum := cleanupSummary(comps, assets, bytes, cleaned)
	if failures > 0 {
		return sum, fmt.Errorf("%d cleanup steps failed (see the log)", failures)
	}
	return sum, nil
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
// of one format.
type rebuildParams struct {
	Repo       string `json:"repo,omitempty"`
	Repository string `json:"repository,omitempty"`
	Format     string `json:"format,omitempty"`
}

// rebuildable reports whether the task can rebuild rp's metadata: a Maven
// hosted or proxy repository, or a hosted one of a format with a rebuilder.
func (s *Service) rebuildable(rp *repo.Repository) bool {
	if rp.Format == "maven" {
		return rp.Type == "hosted" || rp.Type == "proxy"
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
	case !s.rebuildable(rp):
		return "", []string{fmt.Sprintf("repository %q is %s %s: the task takes a Maven hosted or proxy repository, or a hosted repository of %s", p.Repo, rp.Format, rp.Type, strings.Join(s.rebuildFormats(), ", "))}
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
		case !s.rebuildable(rp):
			return summary{}, fmt.Errorf("repository %q is now %s %s, which the task cannot rebuild: edit or delete this task", p.Repo, rp.Format, rp.Type)
		}
		return rebuildOne(ctx, s, rp, log)
	}
	for _, rp := range s.Repos.Snapshot().Sorted {
		if s.rebuildable(rp) && (p.Format == "" || p.Format == rp.Format) {
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

// rebuildOne rebuilds rp's metadata with the right rebuilder.
func rebuildOne(ctx context.Context, s *Service, rp *repo.Repository, log *RunLog) (summary, error) {
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
