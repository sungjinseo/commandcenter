package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// DefaultStore is the name of the blob store every repository uses unless it
// names another; it is seeded on startup with the data directory as its root.
const DefaultStore = "default"

// Store combines the named blob stores and the metadata database.
type Store struct {
	// Blobs maps a blob-store name to its *blob.Store. "default" is always
	// present. blobMu guards concurrent create/delete against the request
	// readers; direct field assignment is only safe before the Store is shared
	// (tests and startup).
	Blobs  map[string]*blob.Store
	blobMu sync.RWMutex

	DB  *meta.DB
	Now func() time.Time

	// StoreFor resolves a repository name to its blob-store name (empty for the
	// default). It is set from the registry; a nil StoreFor routes every
	// repository to the default store.
	StoreFor func(repo string) string

	// Live, if set, reports whether rp is still the registered incarnation of its
	// name. Writes for a deleted or re-created repository are refused.
	Live func(rp *Repository) bool
	// CheckGen makes Tx also compare rp.Gen with the stored gen inside the
	// transaction (meta.Tx.RepositoryGen), which is exact even before this
	// node's registry saw another node delete and re-create the repository.
	// The app sets it; test stores whose repositories are not stored leave
	// it off. ponytail: writes outside Tx (Put, DB.PutAsset: raw files,
	// Maven artifacts) are checked by neither; move them into Tx if a
	// re-create racing them shows up.
	CheckGen bool

	// Downloads, if set, is told about every GET of an asset body that was
	// served with a 2xx status (metrics.Recorder.Download).
	Downloads func(repo, path string)

	// Events, if set, receives upload and delete events from the format handlers.
	Events EventSink

	// Lookup, if set, resolves a repository name to its live definition
	// (the registry); nil is unknown. ServeAsset reads the contentDisposition
	// of the asset's repository through it unless the handler set one.
	Lookup func(name string) *Repository
	// moves are the repositories whose blobs move to another store, and
	// writes the blob writes each started before its move (stage.go).
	moveMu sync.Mutex
	moves  map[string]*Move
	writes map[string]int
	// SyncNodes, if set, is Sync across the nodes of a PostgreSQL
	// database (the app's).
	SyncNodes func(ctx context.Context) error
	// Firewall, if set, is asked before ServeAsset serves a file of a
	// repository (vuln.Service.Firewall): an error, a 403 naming the
	// advisories, is answered instead, whether the file was just fetched or
	// cached.
	Firewall func(ctx context.Context, repo, path string) error
}

// Definition returns the live definition of the named repository, or nil.
func (s *Store) Definition(name string) *Repository {
	if s.Lookup == nil {
		return nil
	}
	return s.Lookup(name)
}

// StoreOf is the one rule for which blob store a repository uses, shared by
// routing, garbage collection and usage: its blobStore, with empty meaning
// the default store. Groups have no store of their own; what they write (the
// merged metadata stored under the group's name) goes to the default store.
func StoreOf(def config.Repository) string {
	if def.Type == "group" || def.BlobStore == "" {
		return DefaultStore
	}
	return def.BlobStore
}

// ReposForStore returns the repositories of s that StoreOf assigns to name.
func ReposForStore(s *Snapshot, name string) []string {
	var out []string
	for _, rp := range s.Sorted {
		if StoreOf(rp.Repository) == name {
			out = append(out, rp.Name)
		}
	}
	return out
}

// OrphanRepos returns the repositories of s whose store does not exist (has
// reports false). They cannot read or write blobs; garbage collection treats
// their assets as referencing blobs in every store.
func OrphanRepos(s *Snapshot, has func(string) bool) []string {
	var out []string
	for _, rp := range s.Sorted {
		if !has(StoreOf(rp.Repository)) {
			out = append(out, rp.Name)
		}
	}
	return out
}

// StoreRoot is the directory a stored blob store opens at (an S3 store keeps
// only its temp files there). The default store always lives at the current
// data directory (its row's path is only informational); a relative path is
// relative to the data directory, so it follows a moved dataDir; an absolute
// path is used as is.
func StoreRoot(dataDir string, bs meta.BlobStore) string {
	switch {
	case bs.Name == DefaultStore:
		return dataDir
	case filepath.IsAbs(bs.Path):
		return filepath.Clean(bs.Path)
	}
	return filepath.Join(dataDir, bs.Path)
}

// OpenStore opens a stored blob store at its StoreRoot: a directory store,
// or for an S3 or azure row the bucket or container it names. A group opens
// with OpenGroup.
func OpenStore(dataDir string, bs meta.BlobStore) (*blob.Store, error) {
	root := StoreRoot(dataDir, bs)
	if bs.Type == meta.BlobStoreGroup {
		return nil, fmt.Errorf("blob store %q is a group: open it with OpenGroup", bs.Name)
	}
	if bs.Type == meta.BlobStoreS3 {
		if bs.S3 == nil {
			return nil, fmt.Errorf("blob store %q has no s3 configuration", bs.Name)
		}
		return blob.OpenS3(root, *bs.S3)
	}
	if bs.Type == meta.BlobStoreAzure {
		if bs.Azure == nil {
			return nil, fmt.Errorf("blob store %q has no azure configuration", bs.Name)
		}
		return blob.OpenAzure(root, *bs.Azure)
	}
	return blob.Open(root)
}

// OpenGroup opens a group row at its StoreRoot over the live member stores
// lookup returns by name.
func OpenGroup(dataDir string, bs meta.BlobStore, lookup func(string) *blob.Store) (*blob.Store, error) {
	if bs.Group == nil {
		return nil, fmt.Errorf("blob store %q has no group configuration", bs.Name)
	}
	return blob.OpenGroup(StoreRoot(dataDir, bs), *bs.Group, lookup)
}

// GroupOf returns the name of the group that has the named store as a
// member, "" when none has.
func (s *Store) GroupOf(name string) string {
	s.blobMu.RLock()
	defer s.blobMu.RUnlock()
	for g, bs := range s.Blobs {
		if cfg, ok := bs.Group(); ok {
			for _, m := range cfg.Members {
				if m.Name == name {
					return g
				}
			}
		}
	}
	return ""
}

// Routable reports whether a repository may name the store: it exists and
// is not a group member (whose blobs the group routes).
func (s *Store) Routable(name string) bool { return s.HasStore(name) && s.GroupOf(name) == "" }

// blobsFor resolves a blob-store name ("" is the default) to its *blob.Store.
// An unknown name is an error: the caller must not fall back to another store,
// whose garbage collection would not count the repository's assets.
func (s *Store) blobsFor(name string) (*blob.Store, error) {
	if name == "" {
		name = DefaultStore
	}
	s.blobMu.RLock()
	bs := s.Blobs[name]
	s.blobMu.RUnlock()
	if bs == nil {
		return nil, fmt.Errorf("blob store %q does not exist", name)
	}
	return bs, nil
}

// BlobStore returns the blob store that holds the given repository's blobs,
// or an error (logged) when the repository names a store that does not exist.
func (s *Store) BlobStore(repo string) (*blob.Store, error) {
	name := ""
	if s.StoreFor != nil {
		name = s.StoreFor(repo)
	}
	if m, ok := s.MoveOf(repo); ok { // writes go to the store it moves to
		name = m.Target
	}
	bs, err := s.blobsFor(name)
	if err != nil {
		slog.Error("repository names a blob store that does not exist; its blobs cannot be read or written",
			"repo", repo, "blobStore", name)
		return nil, fmt.Errorf("repository %q: %w", repo, err)
	}
	return bs, nil
}

// StoreByName returns the named blob store, or nil when there is none.
func (s *Store) StoreByName(name string) *blob.Store {
	s.blobMu.RLock()
	defer s.blobMu.RUnlock()
	return s.Blobs[name]
}

// HasStore reports whether a blob store with this name exists.
func (s *Store) HasStore(name string) bool {
	s.blobMu.RLock()
	defer s.blobMu.RUnlock()
	_, ok := s.Blobs[name]
	return ok
}

// StoreNames returns the names of every blob store, sorted.
func (s *Store) StoreNames() []string {
	s.blobMu.RLock()
	out := make([]string, 0, len(s.Blobs))
	for n := range s.Blobs {
		out = append(out, n)
	}
	s.blobMu.RUnlock()
	sort.Strings(out)
	return out
}

// AddStore adds (or replaces) a live blob store; it is concurrency-safe.
func (s *Store) AddStore(name string, bs *blob.Store) {
	s.blobMu.Lock()
	if s.Blobs == nil {
		s.Blobs = map[string]*blob.Store{}
	}
	s.Blobs[name] = bs
	s.blobMu.Unlock()
}

// RemoveStore removes a live blob store; it is concurrency-safe.
func (s *Store) RemoveStore(name string) {
	s.blobMu.Lock()
	delete(s.Blobs, name)
	s.blobMu.Unlock()
}

// SyncStores brings the live blob stores in line with the stored rows, which
// another node may have changed: a new store is opened at its StoreRoot under
// dataDir, a group takes its stored members and fill policy, a store
// converted to a group is replaced by the group, and a deleted one goes.
// Callers hold the registry lock, as every blob store change does, so this
// node changes none meanwhile. A store that does not open is left as it is
// and named in the error.
func (s *Store) SyncStores(ctx context.Context, dataDir string) error {
	rows, err := s.DB.ListBlobStores(ctx)
	if err != nil {
		return err
	}
	var errs []error
	stored := map[string]bool{}
	var groups []meta.BlobStore // after their members
	for _, row := range rows {
		stored[row.Name] = true
		if row.Type == meta.BlobStoreGroup {
			groups = append(groups, row)
			continue
		}
		if cur := s.StoreByName(row.Name); cur != nil {
			if az, ok := cur.Azure(); ok && row.Azure != nil && az != *row.Azure { // new credentials, in place
				if err := cur.SetAzure(*row.Azure); err != nil {
					errs = append(errs, fmt.Errorf("blob store %q: %w", row.Name, err))
				}
				continue
			}
			if _, isGroup := cur.Group(); !isGroup { // a file, S3 or azure store's location never changes
				continue
			}
		}
		bs, err := OpenStore(dataDir, row)
		if err != nil {
			errs = append(errs, fmt.Errorf("open blob store %q: %w", row.Name, err))
			continue
		}
		s.AddStore(row.Name, bs)
	}
	for _, row := range groups {
		if row.Group == nil {
			continue
		}
		if cur := s.StoreByName(row.Name); cur != nil {
			if cfg, ok := cur.Group(); ok {
				if cfg.FillPolicy != row.Group.FillPolicy || !slices.Equal(cfg.Members, row.Group.Members) {
					if err := cur.SetGroup(*row.Group, s.StoreByName); err != nil {
						errs = append(errs, fmt.Errorf("blob store group %q: %w", row.Name, err))
					}
				}
				continue
			}
		}
		g, err := OpenGroup(dataDir, row, s.StoreByName)
		if err != nil {
			errs = append(errs, fmt.Errorf("open blob store group %q: %w", row.Name, err))
			continue
		}
		s.AddStore(row.Name, g)
	}
	for _, name := range s.StoreNames() {
		if !stored[name] && name != DefaultStore {
			s.RemoveStore(name)
		}
	}
	return errors.Join(errs...)
}

// errReplaced is returned for a write that raced with a repository delete or re-create.
func errReplaced(rp *Repository) error {
	return Errorf(http.StatusServiceUnavailable, "repository %q changed during the request; retry", rp.Name)
}

// Current returns nil while rp is the registered incarnation of its name, and
// an error once it was deleted or re-created.
func (s *Store) Current(rp *Repository) error {
	if s.Live != nil && !s.Live(rp) {
		return errReplaced(rp)
	}
	return nil
}

// Tx runs fn in a metadata transaction for repository rp, first checking that rp
// was not deleted or re-created while the request was running. Never call
// Lock inside it (lock order: meta/cluster.go).
func (s *Store) Tx(ctx context.Context, rp *Repository, fn func(*meta.Tx) error) error {
	return s.DB.Tx(ctx, func(tx *meta.Tx) error {
		if err := s.Current(rp); err != nil {
			return err
		}
		if s.CheckGen {
			gen, err := tx.RepositoryGen(ctx, rp.Name)
			if errors.Is(err, meta.ErrNotFound) || err == nil && gen != rp.Gen {
				return errReplaced(rp)
			}
			if err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

// Lock takes the lock named key on every node (meta.DB.Lock). The formats
// serialize the writes of one path, package or derived index with it; their
// keys start with the format's name and a NUL, so no two formats share one.
// Take it before Tx, never inside (lock order: meta/cluster.go).
func (s *Store) Lock(ctx context.Context, key string) (unlock func(), err error) {
	return s.DB.Lock(ctx, key)
}

// LockAll is Lock for several keys at once (meta.DB.LockAll).
func (s *Store) LockAll(ctx context.Context, keys []string) (unlock func(), err error) {
	return s.DB.LockAll(ctx, keys)
}

func (s *Store) Get(ctx context.Context, repo, path string) (meta.Asset, error) {
	a, err := s.DB.GetAsset(ctx, repo, path)
	if errors.Is(err, meta.ErrNotFound) {
		return meta.Asset{}, ErrNotFound
	}
	return a, err
}

// Open opens the asset's blob from the blob store of the asset's repository.
// While the repository moves to another store, a blob not copied there yet
// is read from the store it moves from.
func (s *Store) Open(a meta.Asset) (blob.File, error) {
	bs, err := s.BlobStore(a.Repo)
	if err != nil {
		return nil, err
	}
	f, err := bs.Open(a.BlobSHA256)
	if errors.Is(err, blob.ErrNotFound) {
		if m, ok := s.MoveOf(a.Repo); ok {
			if src := s.StoreByName(m.Source); src != nil {
				return src.Open(a.BlobSHA256)
			}
		}
	}
	return f, err
}

// WriteBlob streams r into repo's blob store. If verify rejects the content, its
// error is returned and the blob stays unreferenced until `commandcenter gc`
// removes it.
func (s *Store) WriteBlob(repo string, r io.Reader, verify func(blob.Sums) error) (blob.Sums, error) {
	defer s.writing(repo)()
	bs, err := s.BlobStore(repo)
	if err != nil {
		return blob.Sums{}, err
	}
	sums, err := bs.Write(r)
	if err != nil {
		return blob.Sums{}, err
	}
	if verify != nil {
		if err := verify(sums); err != nil {
			return blob.Sums{}, err
		}
	}
	return sums, nil
}

// NewAsset builds the record for stored content at repo/path.
func (s *Store) NewAsset(repo, path, contentType string, sums blob.Sums) meta.Asset {
	now := s.Now().UTC().Truncate(time.Millisecond)
	return meta.Asset{
		Repo: repo, Path: path, BlobSHA256: sums.SHA256, Size: sums.Size, ContentType: contentType,
		SHA1: sums.SHA1, MD5: sums.MD5, SHA512: sums.SHA512,
		CreatedAt: now, LastModified: now, CheckedAt: now,
	}
}

// Put streams r into the blob store and records it as repo/path.
// opts can adjust the asset (e.g. remote validators) before it is saved.
func (s *Store) Put(ctx context.Context, repo, path, contentType string, r io.Reader, opts ...func(*meta.Asset)) (meta.Asset, error) {
	sums, err := s.WriteBlob(repo, r, nil)
	if err != nil {
		return meta.Asset{}, err
	}
	a := s.NewAsset(repo, path, contentType, sums)
	for _, o := range opts {
		o(&a)
	}
	if err := s.DB.PutAsset(ctx, a); err != nil {
		return meta.Asset{}, err
	}
	return a, nil
}

// Delete removes the asset record. Its blob is reclaimed later by `commandcenter gc`.
func (s *Store) Delete(ctx context.Context, repo, path string) error {
	err := s.DB.DeleteAsset(ctx, repo, path)
	if errors.Is(err, meta.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// ServeAsset streams the asset; it handles HEAD, Range and conditional requests.
func ServeAsset(w http.ResponseWriter, r *http.Request, s *Store, a meta.Asset) {
	if s.Firewall != nil {
		if err := s.Firewall(r.Context(), a.Repo, a.Path); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	f, err := s.Open(a)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	defer f.Close()
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if rp := s.Definition(a.Repo); rp != nil && w.Header().Get("Content-Disposition") == "" {
		SetContentDisposition(w, rp.ContentDisposition, a.Path)
	}
	if a.SHA1 != "" {
		w.Header().Set("ETag", `"{SHA1{`+a.SHA1+`}}"`)
	}
	if s.Downloads == nil || r.Method != http.MethodGet {
		http.ServeContent(w, r, "", a.LastModified, f)
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	http.ServeContent(sw, r, "", a.LastModified, f)
	// Counted once the 2xx response was sent: a transfer the client aborts
	// midway still counts, because the headers (and some bytes) went out.
	if sw.status >= 200 && sw.status < 300 {
		s.Downloads(a.Repo, a.Path)
	}
}

// statusWriter remembers the response status. ReadFrom keeps the underlying
// writer's sendfile fast path, which http.ServeContent's io.CopyN relies on.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return io.Copy(s.ResponseWriter, r)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ServeText serves a small generated document such as a checksum.
func ServeText(w http.ResponseWriter, r *http.Request, body, contentType string, modTime time.Time) {
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", modTime, strings.NewReader(body))
}
