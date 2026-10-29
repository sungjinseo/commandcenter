package repo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
)

// Move is a repository's change of blob store in progress (the
// repository.change-blobstore task): its blobs are read from Target, else
// from Source, and written to Target until the task switches the
// repository to Target. State is the task's: copying, switching, failed,
// or interrupted for a move a restart found unfinished. Copied and Total
// count the blobs of the copy pass.
type Move struct {
	Source, Target, State string
	Copied, Total         int64
}

// BeginMove routes repo's blob I/O for a move from source to target (or
// replaces the state of the move under way).
func (s *Store) BeginMove(repo, source, target, state string) {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.moves == nil {
		s.moves = map[string]*Move{}
	}
	s.moves[repo] = &Move{Source: source, Target: target, State: state}
}

// MoveOf returns repo's move in progress, if any.
func (s *Store) MoveOf(repo string) (Move, bool) {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if m := s.moves[repo]; m != nil {
		return *m, true
	}
	return Move{}, false
}

// SetMoveState updates the state and counts of repo's move, if it has one.
func (s *Store) SetMoveState(repo, state string, copied, total int64) {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if m := s.moves[repo]; m != nil {
		m.State, m.Copied, m.Total = state, copied, total
	}
}

// EndMove drops repo's move: its blobs go by its blobStore again.
func (s *Store) EndMove(repo string) {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	delete(s.moves, repo)
}

// Moving returns, sorted, the repositories whose move comes from or goes to
// the named store: garbage collection keeps their blobs in both, and the
// store cannot be deleted meanwhile.
func (s *Store) Moving(store string) []string {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	var out []string
	for repo, m := range s.moves {
		if m.Source == store || m.Target == store {
			out = append(out, repo)
		}
	}
	sort.Strings(out)
	return out
}

// writing counts a blob write to repo while no move routes it to the
// target: WaitWrites waits for the writes that may have gone to the old
// store. It returns the function that ends the count.
func (s *Store) writing(repo string) func() {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.moves[repo] != nil {
		return func() {}
	}
	if s.writes == nil {
		s.writes = map[string]int{}
	}
	s.writes[repo]++
	return func() {
		s.moveMu.Lock()
		defer s.moveMu.Unlock()
		if s.writes[repo]--; s.writes[repo] <= 0 {
			delete(s.writes, repo)
		}
	}
}

// WaitWrites returns once the blob writes to repo that started before its
// move began have finished (BeginMove first).
func (s *Store) WaitWrites(ctx context.Context, repo string) error {
	for {
		s.moveMu.Lock()
		n := s.writes[repo]
		s.moveMu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// SetBlobStore ends a blob store move of the named repository: in one
// transaction check runs (an error from it changes nothing), the stored
// blobStore becomes store and the move row goes (meta.DB.SwitchBlobStore),
// then the published definition follows. The repository keeps its
// incarnation (Gen), so requests in flight carry on.
func (r *Registry) SetBlobStore(ctx context.Context, name, store string, check func(*meta.Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.store(); err != nil {
		return err
	}
	cur := r.Get(name)
	if cur == nil {
		return Errorf(http.StatusNotFound, "repository %q not found", name)
	}
	def := cloneDef(cur.Repository)
	def.BlobStore = store
	if err := r.checkStore(def); err != nil {
		return err
	}
	if err := r.db.SwitchBlobStore(ctx, name, store, check); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return Errorf(http.StatusNotFound, "repository %q not found", name)
		}
		return err
	}
	list := r.definitions()
	for i := range list {
		if list[i].Name == name {
			list[i] = def
		}
	}
	r.snap.Store(newSnapshot(list, r.gens))
	return nil
}

// ComponentImporter is implemented by the format handlers that can take a
// component from another hosted repository of their format (a staging
// move). ImportComponent copies c, a component of src, into dst: its rows
// and asset rows, its blobs into dst's store when the stores differ, under
// the format's locks, and updates dst's derived metadata. A file or version
// that exists in dst is a 409 *Error unless overwrite (writePolicy allow and
// the caller may edit), when it is replaced. It returns the asset rows of
// src it copied; nothing is changed in src.
type ComponentImporter interface {
	ImportComponent(ctx context.Context, src, dst *Repository, c component.Component, overwrite bool) ([]meta.Asset, error)
}

// CheckImport is the importers' guard: src and dst are distinct hosted
// repositories of format, and c belongs to src.
func CheckImport(src, dst *Repository, c component.Component, format string) error {
	if src.Type != "hosted" || dst.Type != "hosted" || src.Format != format || dst.Format != format || src.Name == dst.Name {
		return fmt.Errorf("%s import from %s %s repository %q into %s %s repository %q: %w",
			format, src.Format, src.Type, src.Name, dst.Format, dst.Type, dst.Name, component.ErrNotSupported)
	}
	return c.CheckTarget(src.Name, format)
}

// Exists is the importers' conflict error.
func Exists(what, repoName string) error {
	return Errorf(http.StatusConflict, "%s already exists in repository %q", what, repoName)
}

// CopyBlobs makes sure dst's blob store holds the blobs of assets, rows of
// src: nothing is copied when both use the same store (blobs are content
// addressed); otherwise each blob is streamed over and its sha256 checked.
func (s *Store) CopyBlobs(src, dst string, assets []meta.Asset) error {
	from, err := s.BlobStore(src)
	if err != nil {
		return err
	}
	to, err := s.BlobStore(dst)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	for _, a := range assets {
		if err := s.copyBlob(a, dst); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) copyBlob(a meta.Asset, dst string) error {
	f, err := s.Open(a)
	if err != nil {
		return fmt.Errorf("%s: %w", a.Path, err)
	}
	defer f.Close()
	_, err = s.WriteBlob(dst, f, func(sums blob.Sums) error {
		if sums.SHA256 != a.BlobSHA256 {
			return fmt.Errorf("%s: the copy has sha256 %s, not %s", a.Path, sums.SHA256, a.BlobSHA256)
		}
		return nil
	})
	return err
}

// PutImported writes assets, rows of another repository, into dst in tx,
// with their times and digests. An asset dst has already is a conflict
// (Exists) unless overwrite.
func PutImported(ctx context.Context, tx *meta.Tx, dst string, assets []meta.Asset, overwrite bool) error {
	for _, a := range assets {
		switch _, err := tx.GetAsset(ctx, dst, a.Path); {
		case err == nil && !overwrite:
			return Exists(a.Path, dst)
		case err != nil && !errors.Is(err, meta.ErrNotFound):
			return err
		}
		a.Repo = dst
		if err := tx.PutAsset(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// InsertImported inserts rows, package rows of another repository, into
// dst in tx. A version dst has already is a conflict (Exists) unless
// overwrite, when its attrs are replaced.
func InsertImported(ctx context.Context, tx *meta.Tx, dst string, rows []meta.Package, overwrite bool) error {
	for _, p := range rows {
		p.Repo = dst
		switch err := tx.InsertPackage(ctx, p); {
		case errors.Is(err, meta.ErrExists) && !overwrite:
			return Exists(p.Name+" "+p.Version, dst)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, dst, p.Name, p.Version, p.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
	}
	return nil
}
