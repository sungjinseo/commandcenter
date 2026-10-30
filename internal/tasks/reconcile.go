package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ---- blobstore.reconcile-plan and blobstore.reconcile-execute ----
//
// After the database is restored from a backup older than the blob stores,
// what was uploaded since is in the blob stores but not in the database,
// and what was deleted since is back in it. The reconcile journal
// (repo.Journal) says which rows every commit wrote. The plan reads the
// journal of a time window, takes the latest record of each row and compares
// it with the database: ADD (the row is missing or older and its blob is
// there), DELETE (the latest record deletes a row the database still has),
// SKIP (the blob or the repository is missing) and CONFLICT (the database
// holds a newer version than the journal). Nothing is written but the plan.
// Executing it applies the ADDs and DELETEs with the functions an upload
// uses, each row checked again in its transaction, so running it again does
// nothing twice, and then regenerates the index documents of the hosted
// repositories it changed (as repository.rebuild-metadata does). Run both in
// recovery mode (meta.RecoveryMode), so that no upload races them and no
// compaction removes a blob the plan adds.

// Task types.
const (
	TypeReconcilePlan    = "blobstore.reconcile-plan"
	TypeReconcileExecute = "blobstore.reconcile-execute"
)

// DefaultReconcileHours is the plan's journal window unless its params say
// otherwise.
const DefaultReconcileHours = 24

func init() {
	types[TypeReconcilePlan] = typeDef{name: "Repair - Plan reconciling the database with the blob store", params: reconcilePlanParamsOf,
		run: runReconcilePlan, fields: []ParamSpec{
			{Name: "blobStore", Type: "blobStore", Help: "empty: every blob store"},
			{Name: "sinceHours", Type: "int", Default: DefaultReconcileHours, Help: "the journal of the last hours"},
			{Name: "since", Type: "string", Help: "RFC 3339 time, such as the backup's: the journal from then on (instead of sinceHours)"},
			{Name: "all", Type: "bool", Help: "the whole journal (it keeps reconcileJournalDays days)"},
		}}
	types[TypeReconcileExecute] = typeDef{name: "Repair - Execute a reconcile plan", params: reconcileExecuteParamsOf,
		run: runReconcileExecute, fields: []ParamSpec{
			{Name: "planId", Type: "string", Required: true, Help: "the plan blobstore.reconcile-plan made (GET /-/api/reconcile/plans)"},
		}}
}

// recoveryRefused are the task types that do not run in recovery mode: they
// delete or move content a reconcile plan may need.
var recoveryRefused = map[string]bool{
	TypeCompact: true, TypeCleanup: true, TypePurge: true, TypeChangeBlobStore: true, TypeGroupRemove: true, TypeImport: true,
	TypePurgeUnused: true,
}

// recoverySkip returns why a run of type typ is skipped now, "" when it
// runs: recovery mode is on, as the database says (another node may have
// just turned it on). An error when that cannot be read: the run fails.
func (s *Service) recoverySkip(ctx context.Context, typ string) (string, error) {
	if !recoveryRefused[typ] {
		return "", nil
	}
	rm, err := s.DB.LoadRecoveryMode(ctx)
	if err != nil {
		return "", fmt.Errorf("could not read whether recovery mode is on, so the task did not run: %w", err)
	}
	if rm.Enabled {
		return fmt.Sprintf("recovery mode is on (since %s): %s does not run until it is turned off", rm.Since.UTC().Format(time.RFC3339), typ), nil
	}
	return "", nil
}

type reconcilePlanParams struct {
	BlobStore  string `json:"blobStore,omitempty"`
	SinceHours int    `json:"sinceHours,omitempty"`
	Since      string `json:"since,omitempty"`
	All        bool   `json:"all,omitempty"`
}

// since is the start of the window; zero for the whole journal.
func (p reconcilePlanParams) since(now time.Time) time.Time {
	switch {
	case p.All:
		return time.Time{}
	case p.Since != "":
		t, _ := time.Parse(time.RFC3339, p.Since)
		return t.UTC()
	case p.SinceHours > 0:
		return now.Add(-time.Duration(p.SinceHours) * time.Hour)
	}
	return now.Add(-DefaultReconcileHours * time.Hour)
}

func reconcilePlanParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p reconcilePlanParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error() + " (" + TypeReconcilePlan + " takes blobStore, sinceHours, since and all)"}
	}
	var errs []string
	if p.SinceHours < 0 || p.SinceHours > meta.MaxJournalDays*24 {
		errs = append(errs, fmt.Sprintf("params.sinceHours must be between 0 and %d", meta.MaxJournalDays*24))
	}
	if p.Since != "" {
		if _, err := time.Parse(time.RFC3339, p.Since); err != nil {
			errs = append(errs, "params.since must be an RFC 3339 time such as 2026-10-30T09:00:00Z")
		}
	}
	if check && p.BlobStore != "" {
		if err := s.planStoreCheck(p.BlobStore); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return encodeParams(p), errs
}

// planStoreCheck reports why a plan cannot read the journal of the named
// store: it does not exist, or it is a group member (its group's journal
// holds its records).
func (s *Service) planStoreCheck(name string) error {
	if s.Store == nil || !s.Store.HasStore(name) {
		return fmt.Errorf("blob store %q does not exist", name)
	}
	if g := s.Store.GroupOf(name); g != "" {
		return fmt.Errorf("blob store %q is a member of the group %q: plan the group", name, g)
	}
	return nil
}

// candidate is the latest journal record of one row.
type candidate struct {
	c     meta.Change
	at    time.Time
	key   string // the journal record's
	idx   int    // in the record
	store string
}

func (a *candidate) before(b *candidate) bool {
	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	if a.key != b.key {
		return a.key < b.key
	}
	return a.idx < b.idx
}

// planSummary is a plan's summary JSON.
type planSummary struct {
	Stores         []string `json:"stores"`
	Records        int      `json:"records"`        // journal records read
	SkippedRecords int      `json:"skippedRecords"` // records that did not decode
	Rows           int      `json:"rows"`           // distinct rows they name
	Unchanged      int      `json:"unchanged"`      // rows the database holds as journaled
	Add            int      `json:"add"`
	Delete         int      `json:"delete"`
	Skip           int      `json:"skip"`
	Conflict       int      `json:"conflict"`
	// Set by execution:
	Done     int64    `json:"done,omitempty"`
	Failed   int64    `json:"failed,omitempty"`
	Pending  int64    `json:"pending,omitempty"`
	Rebuilt  []string `json:"rebuilt,omitempty"`
	Executed int      `json:"executions,omitempty"`
}

func runReconcilePlan(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	var p reconcilePlanParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if s.Store == nil {
		return summary{msg: "blob stores are not available"}, errors.New("blob stores are not available")
	}
	now := s.now().UTC()
	since := p.since(now)
	var stores []string
	if p.BlobStore != "" {
		if err := s.planStoreCheck(p.BlobStore); err != nil {
			return summary{}, err
		}
		stores = []string{p.BlobStore}
	} else {
		for _, name := range s.Store.StoreNames() {
			if s.Store.GroupOf(name) == "" { // read with its group
				stores = append(stores, name)
			}
		}
	}
	if j := s.Store.Journal; j != nil { // this node's records still queued
		if err := j.Flush(ctx); err != nil {
			return summary{}, err
		}
	}
	window := "the whole journal"
	if !since.IsZero() {
		window = "the journal since " + since.Format(time.RFC3339)
	}
	log.Infof("reading %s of the blob stores %v", window, stores)
	sum := planSummary{Stores: stores}
	latest := map[string]*candidate{}
	for _, name := range stores {
		bs := s.Store.StoreByName(name)
		if bs == nil {
			return summary{}, fmt.Errorf("blob store %q no longer exists", name)
		}
		skipped, err := repo.ReadJournal(bs, since, func(key string, e repo.JournalEntry) error {
			sum.Records++
			for i, c := range e.Changes {
				cand := &candidate{c: c, at: e.At, key: key, idx: i, store: name}
				k := c.Repo + "\x00" + c.Key()
				if cur := latest[k]; cur == nil || cur.before(cand) {
					latest[k] = cand
				}
			}
			return ctx.Err()
		})
		sum.SkippedRecords += skipped
		if errors.Is(err, blob.ErrNoMeta) {
			log.Warnf("%s: this blob store keeps no journal", name)
			continue
		}
		if err != nil {
			return summary{}, fmt.Errorf("blob store %q: reading the journal: %w", name, err)
		}
	}
	if sum.SkippedRecords > 0 {
		log.Warnf("%d journal records did not decode and were skipped", sum.SkippedRecords)
	}
	cands := make([]*candidate, 0, len(latest))
	for _, c := range latest {
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].before(cands[j]) })
	sum.Rows = len(cands)

	plan := meta.ReconcilePlan{ID: newPlanID(), BlobStore: p.BlobStore, State: meta.PlanPlanned, Since: since, Created: now, CreatedBy: actor}
	var items []meta.ReconcileItem
	noBlob := map[string]bool{} // commits (repository and record) one of whose assets has no blob
	for _, cand := range cands {
		if err := ctx.Err(); err != nil {
			return summary{}, err
		}
		action, reason, err := s.reconcileRow(ctx, cand)
		if err != nil {
			return summary{}, fmt.Errorf("%s %s: %w", cand.c.Repo, cand.c.Label(), err)
		}
		if action == "" {
			sum.Unchanged++
			continue
		}
		if action == meta.ItemSkip && cand.c.Kind == meta.ChangeAsset && cand.c.Op == meta.ChangePut {
			noBlob[cand.c.Repo+"\x00"+cand.key] = true
		}
		change, _ := json.Marshal(cand.c)
		it := meta.ReconcileItem{PlanID: plan.ID, Seq: int64(len(items) + 1), Repo: cand.c.Repo, Kind: cand.c.Kind, Label: cand.c.Label(),
			Action: action, State: meta.ItemPlanned, Reason: reason, JournalKey: cand.key, At: cand.at, Change: string(change)}
		if cand.c.Asset != nil {
			it.BlobSHA256 = cand.c.Asset.SHA256
		}
		items = append(items, it)
	}
	// A package version or tag is not added without the files its upload
	// wrote: an index would list a version whose file is missing.
	for i := range items {
		it := &items[i]
		if it.Kind != meta.ChangeAsset && it.Action == meta.ItemAdd && noBlob[it.Repo+"\x00"+it.JournalKey] {
			it.Action, it.Reason = meta.ItemSkip, "a file of the same upload has no blob"
		}
		switch it.Action {
		case meta.ItemAdd:
			sum.Add++
		case meta.ItemDelete:
			sum.Delete++
		case meta.ItemSkip:
			sum.Skip++
		case meta.ItemConflict:
			sum.Conflict++
		}
		if it.Action == meta.ItemSkip || it.Action == meta.ItemConflict {
			it.State = meta.ItemSkipped
		}
	}
	b, _ := json.Marshal(sum)
	plan.Summary = string(b)
	wctx := context.WithoutCancel(ctx)
	if err := s.DB.CreateReconcilePlan(wctx, plan); err != nil {
		return summary{}, err
	}
	if err := s.DB.AddReconcileItems(wctx, items); err != nil {
		s.DB.DeleteReconcilePlan(wctx, plan.ID)
		return summary{}, err
	}
	msg := fmt.Sprintf("plan %s: %d to add, %d to delete, %d skipped, %d conflicts, %d unchanged (%d journal records, %d rows)",
		plan.ID, sum.Add, sum.Delete, sum.Skip, sum.Conflict, sum.Unchanged, sum.Records, sum.Rows)
	log.Infof("%s", msg)
	if s.Events != nil {
		s.Events.Emit("reconcile.plan", actor, "", plan.ID, msg)
	}
	return summary{msg: msg}, nil
}

func newPlanID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// reconcileRow compares a row's latest journal record with the database: the
// item's action and reason, or "" when the database holds the row as
// journaled.
func (s *Service) reconcileRow(ctx context.Context, cand *candidate) (action, reason string, err error) {
	c := cand.c
	rp := s.Repos.Get(c.Repo)
	switch {
	case rp == nil:
		return meta.ItemSkip, "the repository does not exist: create it with the same name, format and blob store, and plan again", nil
	case c.Format != "" && rp.Format != c.Format:
		return meta.ItemSkip, fmt.Sprintf("the repository is now a %s repository, the journal's was %s", rp.Format, c.Format), nil
	case rp.Type == "group":
		return meta.ItemSkip, "a group repository: its merged documents are made again on demand", nil
	}
	put := c.Op == meta.ChangePut
	switch c.Kind {
	case meta.ChangeAsset:
		cur, err := s.DB.GetAsset(ctx, c.Repo, c.Path)
		found := err == nil
		if err != nil && !errors.Is(err, meta.ErrNotFound) {
			return "", "", err
		}
		if !put {
			switch {
			case !found:
				return "", "", nil
			case cur.LastModified.After(cand.at):
				return meta.ItemConflict, "the database holds a version written after the journaled delete", nil
			}
			return meta.ItemDelete, "deleted after the database's copy was written", nil
		}
		if c.Asset == nil {
			return meta.ItemSkip, "the journal record has no asset row", nil
		}
		reason := "missing in the database"
		if found {
			if cur.BlobSHA256 == c.Asset.SHA256 {
				return "", "", nil
			}
			if cur.LastModified.After(c.Asset.LastModified) {
				return meta.ItemConflict, "the database holds a newer version", nil
			}
			reason = "the database holds an older version"
		}
		if why, err := s.blobProblem(c.Repo, c.Asset.SHA256, c.Asset.Size); err != nil || why != "" {
			return meta.ItemSkip, why, err
		}
		return meta.ItemAdd, reason, nil
	case meta.ChangePackage:
		cur, err := s.DB.GetPackage(ctx, c.Repo, c.Name, c.Version)
		found := err == nil
		if err != nil && !errors.Is(err, meta.ErrNotFound) {
			return "", "", err
		}
		if !put {
			switch {
			case !found:
				return "", "", nil
			case cur.CreatedAt.After(cand.at):
				return meta.ItemConflict, "the database holds a version published after the journaled delete", nil
			}
			return meta.ItemDelete, "deleted after the database's copy was written", nil
		}
		want := c.PackageOf()
		switch {
		case !found:
			return meta.ItemAdd, "missing in the database", nil
		case cur.Attrs == want.Attrs && cur.Yanked == want.Yanked:
			return "", "", nil
		case cur.CreatedAt.After(want.CreatedAt):
			return meta.ItemConflict, "the database holds a version published later", nil
		}
		return meta.ItemAdd, "the database holds another version of the row", nil
	case meta.ChangeTag:
		tags, err := s.DB.ListDistTags(ctx, c.Repo, c.Name)
		if err != nil {
			return "", "", err
		}
		i := slices.IndexFunc(tags, func(t meta.DistTag) bool { return t.Tag == c.Tag })
		switch {
		case !put && i < 0, put && i >= 0 && tags[i].Version == c.Version:
			return "", "", nil
		case !put:
			return meta.ItemDelete, "deleted after the database's copy was written", nil
		case i < 0:
			return meta.ItemAdd, "missing in the database", nil
		}
		return meta.ItemAdd, "the database names version " + tags[i].Version, nil
	}
	return meta.ItemSkip, "a journal record of an unknown kind " + c.Kind, nil
}

// blobProblem says why the blob an asset of repo names cannot be used: ""
// when the repository's store (or, while it moves, its source) holds it
// with the journaled size.
func (s *Service) blobProblem(repoName, sha string, size int64) (string, error) {
	bs, err := s.Store.BlobStore(repoName)
	if err != nil {
		return "the repository's blob store is not available", nil
	}
	info, err := bs.Stat(sha)
	if errors.Is(err, blob.ErrNotFound) {
		if m, ok := s.Store.MoveOf(repoName); ok {
			if src := s.Store.StoreByName(m.Source); src != nil {
				info, err = src.Stat(sha)
			}
		}
	}
	switch {
	case errors.Is(err, blob.ErrNotFound):
		return "the blob is missing from the blob store", nil
	case err != nil:
		return "", fmt.Errorf("blob %s: %w", sha, err)
	case info.Size != size:
		return fmt.Sprintf("the blob holds %d bytes, the journal %d", info.Size, size), nil
	}
	return "", nil
}

type reconcileExecuteParams struct {
	PlanID string `json:"planId"`
}

func reconcileExecuteParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p reconcileExecuteParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error() + " (" + TypeReconcileExecute + " takes planId)"}
	}
	if p.PlanID == "" {
		return "", []string{"params.planId is required"}
	}
	if check {
		if _, err := s.DB.GetReconcilePlan(context.Background(), p.PlanID); errors.Is(err, meta.ErrNotFound) {
			return "", []string{fmt.Sprintf("reconcile plan %q does not exist", p.PlanID)}
		} else if err != nil {
			return "", []string{err.Error()}
		}
	}
	return encodeParams(p), nil
}

// executePage is how many items execution reads at once.
const executePage = 500

func runReconcileExecute(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	var p reconcileExecuteParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	plan, err := s.DB.GetReconcilePlan(ctx, p.PlanID)
	if errors.Is(err, meta.ErrNotFound) {
		return summary{}, fmt.Errorf("reconcile plan %q does not exist: edit or delete this task", p.PlanID)
	}
	if err != nil {
		return summary{}, err
	}
	if s.Store == nil {
		return summary{msg: "blob stores are not available"}, errors.New("blob stores are not available")
	}
	if rm, err := s.DB.LoadRecoveryMode(ctx); err == nil && !rm.Enabled {
		log.Warnf("recovery mode is off: an upload or delete of a row the plan changes may race this execution (each row is checked again before it is written)")
	}
	log.Infof("executing plan %s (made %s)", plan.ID, plan.Created.Format(time.RFC3339))
	ex := &executor{s: s, plan: plan.ID, actor: actor, log: log, touched: map[string]bool{}}
	var after int64
	for {
		page, err := s.DB.ReconcileItems(ctx, plan.ID, after, executePage, meta.ItemFilter{States: []string{meta.ItemPlanned, meta.ItemFailed}})
		if err != nil {
			return ex.summary(), err
		}
		for start := 0; start < len(page); {
			end := start + 1
			for end < len(page) && page[end].Repo == page[start].Repo && page[end].JournalKey == page[start].JournalKey {
				end++
			}
			if err := ex.unit(ctx, page[start:end]); err != nil {
				return ex.summary(), err
			}
			start = end
		}
		if len(page) < executePage {
			break
		}
		after = page[len(page)-1].Seq
	}
	var rebuilt []string
	var rebuildErrs []error
	for _, name := range slices.Sorted(maps.Keys(ex.touched)) {
		rp := s.Repos.Get(name)
		if rp == nil || rp.Type != "hosted" || s.Transfer == nil || s.Transfer.Rebuild[rp.Format] == nil {
			continue
		}
		log.Infof("%s: regenerating the %s index documents", name, rp.Format)
		if err := s.Transfer.Rebuild[rp.Format](context.WithoutCancel(ctx), rp, log.Infof); err != nil {
			rebuildErrs = append(rebuildErrs, fmt.Errorf("%s: regenerating the index documents: %w", name, err))
			log.Errorf("%s: regenerating the index documents: %v (run repository.rebuild-metadata)", name, err)
			continue
		}
		rebuilt = append(rebuilt, name)
	}
	counts, err := s.DB.CountReconcileItems(context.WithoutCancel(ctx), plan.ID)
	if err != nil {
		return ex.summary(), err
	}
	var sum planSummary
	json.Unmarshal([]byte(plan.Summary), &sum)
	sum.Done = counts[meta.ItemAdd+"/"+meta.ItemDone] + counts[meta.ItemDelete+"/"+meta.ItemDone]
	sum.Failed = counts[meta.ItemAdd+"/"+meta.ItemFailed] + counts[meta.ItemDelete+"/"+meta.ItemFailed]
	sum.Pending = counts[meta.ItemAdd+"/"+meta.ItemPlanned] + counts[meta.ItemDelete+"/"+meta.ItemPlanned]
	sum.Rebuilt, sum.Executed = rebuilt, sum.Executed+1
	state := meta.PlanExecuted
	if sum.Failed > 0 || sum.Pending > 0 || len(rebuildErrs) > 0 {
		state = meta.PlanFailed
	}
	b, _ := json.Marshal(sum)
	if err := s.DB.UpdateReconcilePlan(context.WithoutCancel(ctx), plan.ID, state, string(b), s.now()); errors.Is(err, meta.ErrNotFound) {
		return ex.summary(), fmt.Errorf("reconcile plan %q was deleted during the execution", plan.ID)
	} else if err != nil {
		return ex.summary(), err
	}
	out := ex.summary()
	out.msg += fmt.Sprintf("; regenerated the index documents of %d repositories", len(rebuilt))
	log.Infof("%s", out.msg)
	switch {
	case sum.Failed > 0:
		return out, fmt.Errorf("%d items of plan %s failed (GET /-/api/reconcile/plans/%s/items?state=FAILED); executing it again retries them", sum.Failed, plan.ID, plan.ID)
	case len(rebuildErrs) > 0:
		return out, errors.Join(rebuildErrs...)
	}
	return out, nil
}

// executor applies a plan's items.
type executor struct {
	s       *Service
	plan    string
	actor   string
	log     *RunLog
	touched map[string]bool // repositories written

	added, deleted, failed, skipped, logged int
}

func (ex *executor) summary() summary {
	return summary{msg: fmt.Sprintf("plan %s: added %d, deleted %d, failed %d, skipped %d (changed since planned)",
		ex.plan, ex.added, ex.deleted, ex.failed, ex.skipped), removed: int64(ex.deleted)}
}

func (ex *executor) note(format string, args ...any) {
	if ex.logged++; ex.logged <= 50 {
		ex.log.Warnf(format, args...)
	}
}

// unit applies the items of one journal record in one repository, in one
// transaction under the locks an upload of their paths takes.
func (ex *executor) unit(ctx context.Context, items []meta.ReconcileItem) error {
	s := ex.s
	results := make([]meta.ItemResult, len(items))
	changes := make([]meta.Change, len(items))
	fail := func(reason string) error {
		for i, it := range items {
			results[i] = meta.ItemResult{Seq: it.Seq, State: meta.ItemFailed, Reason: reason}
		}
		ex.failed += len(items)
		ex.note("%s: %d items failed: %s", items[0].Repo, len(items), reason)
		return s.DB.MarkReconcileItems(context.WithoutCancel(ctx), ex.plan, results)
	}
	var paths []string
	for i, it := range items {
		if err := json.Unmarshal([]byte(it.Change), &changes[i]); err != nil {
			return fail("the item's journal record does not decode")
		}
		if changes[i].Kind == meta.ChangeAsset {
			paths = append(paths, changes[i].Path)
		}
	}
	rp := s.Repos.Get(items[0].Repo)
	if rp == nil {
		return fail("the repository no longer exists")
	}
	// Outside the transaction, which must not wait on a blob store: an
	// ADD whose blob went meanwhile fails.
	for i, it := range items {
		if c := changes[i]; it.Action == meta.ItemAdd && c.Asset != nil {
			if why, err := s.blobProblem(rp.Name, c.Asset.SHA256, c.Asset.Size); err != nil || why != "" {
				if err != nil {
					why = err.Error()
				}
				return fail(it.Label + ": " + why)
			}
		}
	}
	if s.Transfer != nil && len(paths) > 0 {
		if l := s.Transfer.Lockers[rp.Format]; l != nil {
			unlock, err := l.LockPaths(ctx, rp, paths)
			if err != nil {
				return err
			}
			defer unlock()
		}
	}
	applied := make([]bool, len(items))
	err := s.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		for i, it := range items {
			state, note, err := apply(ctx, tx, it, changes[i])
			if err != nil {
				return err
			}
			applied[i] = note == ""
			if applied[i] {
				note = it.Reason
			}
			results[i] = meta.ItemResult{Seq: it.Seq, State: state, Reason: note}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail(err.Error())
	}
	for i, r := range results {
		it, c := items[i], changes[i]
		switch {
		case r.State == meta.ItemSkipped:
			ex.skipped++
			ex.note("%s %s: skipped: %s", it.Repo, it.Label, r.Reason)
			continue
		case !applied[i]: // already as planned
			continue
		case it.Action == meta.ItemAdd:
			ex.added++
		default:
			ex.deleted++
		}
		ex.touched[rp.Name] = true
		if s.Events != nil && c.Kind != meta.ChangeTag {
			kind := "upload"
			if it.Action == meta.ItemDelete {
				kind = "delete"
			}
			s.Events.Emit(kind, ex.actor, rp.Name, it.Label, "reconcile plan "+ex.plan)
		}
	}
	return s.DB.MarkReconcileItems(context.WithoutCancel(ctx), ex.plan, results)
}

// apply writes one item in tx after checking the row is still as planned:
// DONE with no note when written, DONE with one when the row already is as
// the item wants it, SKIPPED when it changed since the plan.
func apply(ctx context.Context, tx *meta.Tx, it meta.ReconcileItem, c meta.Change) (state, reason string, err error) {
	add := it.Action == meta.ItemAdd
	switch c.Kind {
	case meta.ChangeAsset:
		cur, err := tx.GetAsset(ctx, c.Repo, c.Path)
		found := err == nil
		if err != nil && !errors.Is(err, meta.ErrNotFound) {
			return "", "", err
		}
		switch {
		case add && c.Asset == nil:
			return meta.ItemSkipped, "the journal record has no asset row", nil
		case add && found && cur.BlobSHA256 == c.Asset.SHA256:
			return meta.ItemDone, "already in the database", nil
		case add && found && cur.LastModified.After(c.Asset.LastModified):
			return meta.ItemSkipped, "the database holds a newer version written since the plan", nil
		case add:
			return meta.ItemDone, "", tx.PutAsset(ctx, c.Asset.Asset(c.Repo, c.Path))
		case !found:
			return meta.ItemDone, "already deleted", nil
		case cur.LastModified.After(it.At):
			return meta.ItemSkipped, "written again since the journaled delete", nil
		}
		_, err = tx.DeleteAsset(ctx, c.Repo, c.Path)
		return meta.ItemDone, "", err
	case meta.ChangePackage:
		cur, err := tx.GetPackage(ctx, c.Repo, c.Name, c.Version)
		found := err == nil
		if err != nil && !errors.Is(err, meta.ErrNotFound) {
			return "", "", err
		}
		want := c.PackageOf()
		switch {
		case add && found && cur.Attrs == want.Attrs && cur.Yanked == want.Yanked:
			return meta.ItemDone, "already in the database", nil
		case add && found && cur.CreatedAt.After(want.CreatedAt):
			return meta.ItemSkipped, "the database holds a version published since the plan", nil
		case add:
			return meta.ItemDone, "", tx.ImportPackage(ctx, want, true)
		case !found:
			return meta.ItemDone, "already deleted", nil
		case cur.CreatedAt.After(it.At):
			return meta.ItemSkipped, "published again since the journaled delete", nil
		}
		return meta.ItemDone, "", tx.DeletePackage(ctx, c.Repo, c.Name, c.Version)
	case meta.ChangeTag:
		if add {
			return meta.ItemDone, "", tx.SetDistTag(ctx, c.Repo, c.Name, c.Tag, c.Version)
		}
		if err := tx.DeleteDistTag(ctx, c.Repo, c.Name, c.Tag); errors.Is(err, meta.ErrNotFound) {
			return meta.ItemDone, "already deleted", nil
		} else if err != nil {
			return "", "", err
		}
		return meta.ItemDone, "", nil
	}
	return meta.ItemSkipped, "a journal record of an unknown kind " + c.Kind, nil
}
