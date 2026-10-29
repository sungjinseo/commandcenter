package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ---- repository.change-blobstore ----
//
// The move registers itself (blob_store_moves and repo.Store.BeginMove):
// from then on the repository writes blobs to the target and reads them
// from the target, else from the source, so no read fails while blobs are
// copied. The task copies every blob the repository's assets reference
// and checks its sha256, then, in one transaction that first checks that
// every blob referenced now was copied, switches the repository's
// blobStore and drops the move. The source keeps its copies for garbage
// collection. A run that fails or stops leaves the move registered; the
// next run of the task continues it (copied blobs are not copied again).

// TypeChangeBlobStore moves a repository's blobs to another blob store.
const TypeChangeBlobStore = "repository.change-blobstore"

func init() {
	types[TypeChangeBlobStore] = typeDef{name: "Change repository blob store", params: moveParamsOf, run: runMove, fields: moveFields}
}

var moveFields = []ParamSpec{
	{Name: "repo", Type: "repository", Required: true, Help: "a hosted or proxy repository"},
	{Name: "target", Type: "blobStore", Required: true, Help: "the blob store its blobs move to"},
}

type moveParams struct {
	Repo   string `json:"repo"`
	Target string `json:"target"`
}

// defaultMoveSettle is how long a move waits, after the blob writes that
// started before it ended, for their asset rows to be recorded.
const defaultMoveSettle = 5 * time.Second

// maxMovePasses bounds the copy passes: uploads that keep adding blobs the
// last pass did not see make the next pass copy them.
const maxMovePasses = 10

var errLateBlobs = errors.New("blobs were added during the copy")

func moveParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p moveParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error() + " (repository.change-blobstore takes repo and target)"}
	}
	var errs []string
	if p.Repo == "" {
		errs = append(errs, "params.repo is required")
	}
	if p.Target == "" {
		errs = append(errs, "params.target is required")
	}
	if check && len(errs) == 0 {
		if err := s.checkMove(p.Repo, p.Target); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return encodeParams(p), errs
}

// checkMove reports why repository name cannot move to blob store target
// now, or nil.
func (s *Service) checkMove(name, target string) error {
	rp := s.Repos.Get(name)
	switch {
	case rp == nil:
		return fmt.Errorf("repository %q does not exist", name)
	case rp.Type == "group":
		return fmt.Errorf("repository %q is a group: groups have no blob store", name)
	case s.Store == nil || !s.Store.HasStore(target):
		return fmt.Errorf("blob store %q does not exist", target)
	}
	if m, ok := s.Store.MoveOf(name); ok {
		if m.Target != target {
			return fmt.Errorf("repository %q has an unfinished move to blob store %q: finish that one first", name, m.Target)
		}
		return nil
	}
	if repo.StoreOf(rp.Repository) == target {
		return fmt.Errorf("repository %q already uses blob store %q", name, target)
	}
	return nil
}

// StartBlobStoreMove moves repository name's blobs to blob store target in
// the background: the repository's repository.change-blobstore task
// (created on first use, its target updated) runs now for actor. A
// *repo.ValidationError when the move is not possible, a *BusyError when
// a task is running.
func (s *Service) StartBlobStoreMove(ctx context.Context, name, target, actor string) (meta.Task, error) {
	if err := s.checkMove(name, target); err != nil {
		return meta.Task{}, &repo.ValidationError{Errors: []string{err.Error()}}
	}
	list, err := s.DB.ListTasks(ctx)
	if err != nil {
		return meta.Task{}, err
	}
	params := json.RawMessage(encodeParams(moveParams{Repo: name, Target: target}))
	id := ""
	for _, t := range list {
		var p moveParams
		if t.Type == TypeChangeBlobStore && json.Unmarshal([]byte(t.Params), &p) == nil && p.Repo == name {
			id = t.ID
			break
		}
	}
	var t meta.Task
	if id == "" {
		t, err = s.Create(ctx, Def{Name: "Change blob store of " + name, Type: TypeChangeBlobStore, Schedule: Manual, Params: params})
	} else {
		t, err = s.Update(ctx, id, Def{Params: params})
	}
	if err != nil {
		return meta.Task{}, err
	}
	return t, s.RunAsync(t.ID, actor)
}

// MoveRunning reports whether repository name's repository.change-blobstore
// task is running, on this node or (PostgreSQL) another live one.
func (s *Service) MoveRunning(name string) bool {
	id := s.Running()
	if id == "" {
		return false
	}
	t, err := s.DB.GetTask(context.Background(), id)
	var p moveParams
	return err == nil && t.Type == TypeChangeBlobStore && json.Unmarshal([]byte(t.Params), &p) == nil && p.Repo == name
}

func runMove(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	var p moveParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if s.Store == nil {
		return summary{msg: "blob stores are not available"}, errors.New("blob stores are not available")
	}
	if err := s.checkMove(p.Repo, p.Target); err != nil {
		return summary{msg: err.Error()}, err
	}
	// Under the registry lock (blob store deletes take it too), after it
	// re-read the moves another node may have begun (PostgreSQL).
	var source string
	if err := s.Repos.WithWriteLock(ctx, func(snap *repo.Snapshot) error {
		rp := snap.ByName[p.Repo]
		switch m, ok := s.Store.MoveOf(p.Repo); {
		case rp == nil:
			return fmt.Errorf("repository %q does not exist", p.Repo)
		case ok && m.Target != p.Target:
			return fmt.Errorf("repository %q has an unfinished move to blob store %q: finish that one first", p.Repo, m.Target)
		case ok:
			source = m.Source
			s.Store.SetMoveState(p.Repo, "copying", 0, 0)
			log.Infof("%s: continuing the unfinished move from %q to %q", p.Repo, source, p.Target)
			return nil
		case !s.Store.HasStore(p.Target):
			return fmt.Errorf("blob store %q does not exist", p.Target)
		}
		source = repo.StoreOf(rp.Repository)
		if err := s.DB.PutBlobStoreMove(ctx, meta.BlobStoreMove{Repo: p.Repo, Source: source, Target: p.Target, Started: s.now()}); err != nil {
			return err
		}
		s.Store.BeginMove(p.Repo, source, p.Target, "copying")
		log.Infof("%s: moving its blobs from %q to %q; until the move ends it reads from both and writes to %q", p.Repo, source, p.Target, p.Target)
		return nil
	}); err != nil {
		return summary{msg: err.Error()}, err
	}
	s.notify("moves") // the other nodes route its writes to the target too

	var copied, kept, missing, total int64
	sum := func() summary {
		return summary{msg: fmt.Sprintf("%s: %d blobs: %d copied to %q, %d there already, %d missing in %q", p.Repo, total, copied, p.Target, kept, missing, source)}
	}
	fail := func(err error) (summary, error) {
		s.Store.SetMoveState(p.Repo, "failed", copied+kept+missing, total)
		log.Warnf("%s: the move stays unfinished: it reads from both stores and writes to %q until the task runs again and finishes", p.Repo, p.Target)
		return sum(), err
	}
	// Writes that resolved to the source before the move began, on any
	// node, are waited for (every live node applied the move and finished
	// them: Store.Sync), and then their asset rows, so the copy sees them.
	// ponytail: a write whose row is recorded later than the settle time
	// and the final check could reference a blob left in the source; track
	// whole uploads if that ever shows.
	if err := s.Store.Sync(ctx); err != nil {
		return fail(err)
	}
	settle := s.MoveSettle
	if settle <= 0 {
		settle = defaultMoveSettle
	}
	select {
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-time.After(settle):
	}
	src, dst := s.Store.StoreByName(source), s.Store.StoreByName(p.Target)
	if src == nil || dst == nil {
		return fail(fmt.Errorf("blob store %q or %q no longer exists", source, p.Target))
	}
	verified := map[string]bool{}
	for pass := 1; ; pass++ {
		blobs, err := s.DB.RepoBlobs(ctx, p.Repo)
		if err != nil {
			return fail(err)
		}
		total = int64(len(verified))
		for _, b := range blobs {
			if !verified[b.SHA256] {
				total++
			}
		}
		for _, b := range blobs {
			if verified[b.SHA256] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			did, gone, err := moveBlob(src, dst, b)
			if err != nil {
				return fail(fmt.Errorf("copy blob %s: %w", b.SHA256, err))
			}
			switch {
			case gone:
				missing++
				if missing <= 20 {
					log.Warnf("%s: blob %s is in neither store: its assets could not be read before the move either", p.Repo, b.SHA256)
				}
			case did:
				copied++
			default:
				kept++
			}
			verified[b.SHA256] = true
			s.Store.SetMoveState(p.Repo, "copying", copied+kept+missing, total)
			if s.AfterMoveCopy != nil {
				s.AfterMoveCopy(p.Repo, b.SHA256)
			}
		}
		s.Store.SetMoveState(p.Repo, "switching", copied+kept+missing, total)
		late := 0
		err = s.Repos.SetBlobStore(ctx, p.Repo, p.Target, func(tx *meta.Tx) error {
			now, err := tx.RepoBlobs(ctx, p.Repo)
			if err != nil {
				return err
			}
			for _, b := range now {
				if !verified[b.SHA256] {
					late++
				}
			}
			if late > 0 {
				return errLateBlobs
			}
			return nil
		})
		if errors.Is(err, errLateBlobs) && pass < maxMovePasses {
			log.Infof("%s: %d blobs were added during pass %d; copying them", p.Repo, late, pass)
			continue
		}
		if err != nil {
			return fail(err)
		}
		break
	}
	s.Store.EndMove(p.Repo)
	out := sum()
	out.msg += fmt.Sprintf("; the repository now uses %q (the copies in %q go at its next compaction)", p.Target, source)
	log.Infof("%s", out.msg)
	if s.Events != nil {
		s.Events.Emit("repo.blobstore", actor, p.Repo, p.Repo, fmt.Sprintf("from=%s to=%s blobs=%d copied=%d missing=%d", source, p.Target, total, copied, missing))
	}
	return out, nil
}

// moveBlob makes sure dst holds blob b: one there already (blobs are
// content addressed) is kept, else it is copied from src and its sha256
// checked. gone reports a blob neither store holds.
func moveBlob(src, dst *blob.Store, b meta.RepoBlob) (copied, gone bool, err error) {
	if st, err := dst.Stat(b.SHA256); err == nil && st.Size == b.Size {
		return false, false, nil
	}
	f, err := src.Open(b.SHA256)
	if errors.Is(err, blob.ErrNotFound) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	defer f.Close()
	sums, err := dst.Write(f)
	if err != nil {
		return false, false, err
	}
	if sums.SHA256 != b.SHA256 {
		return false, false, fmt.Errorf("the copy has sha256 %s: the stored blob is corrupt", sums.SHA256)
	}
	return true, false, nil
}
