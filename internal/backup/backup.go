// Package backup makes online backups of a running Command Center (plan 14
// Decision 3): a consistent snapshot of the metadata database and a copy of
// every blob it references, into a blob pool shared by all the backups under
// the backup root, so successive backups only copy new blobs.
//
// Layout under the backup root (directories 0700, files 0600):
//
//	<root>/<name>/meta.db                         VACUUM INTO snapshot
//	<root>/<name>/manifest.json                   written last: a backup without it is incomplete
//	<root>/<name>/verify.json                     the last verify result
//	<root>/blobs/<store>/sha256/aa/bb/<sha256>    the shared pool, per blob store
//
// A backup runs as the "backup" task, so the task scheduler's run lock (and
// the cross-process data directory lock, see package datalock) keeps blob
// store compaction, the only code that deletes blobs, out for the whole
// backup: every blob the snapshot references stays in its store until it is
// copied. Every copy is streamed through one reused buffer; no blob is ever
// held in memory.
//
// The pool sweep (after retention and after a delete) trusts no file
// metadata. Every backup writes <name>/refs at creation: the sorted, unique
// "<store>/<sha256>" lines its meta.db references (a backup without one, from
// an older version, gets it generated from its meta.db first). The sweep
// merges all the refs files (one buffered reader each, so memory is O(number
// of backups)) against a sorted walk of the pool and deletes a pool file only
// when no refs file lists it. Every root directory with a meta.db or a
// manifest counts as a backup; other directories (lost+found) are not read.
// The sweep is skipped, deleting nothing and saying why ("sweep skipped:
// <reason>" in the run message and the result), when anything cannot be read,
// when a refs file is out of order, when a backup's refs match nothing in the
// pool (a reading error), and when the root or the pool holds a symbolic link:
// symlinks are not followed.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Limits and file names.
const (
	ManifestVersion = 1
	DefaultKeep     = 7
	MaxKeep         = 100
	maxSubdir       = 100
	maxProblems     = 20

	poolDir      = "blobs"
	metaFile     = "meta.db"
	manifestFile = "manifest.json"
	verifyFile   = "verify.json"
	tmpPrefix    = ".tmp-"
	dateLayout   = "20060102-150405"
	copyBufSize  = 64 << 10
	progressEach = 30 * time.Second
	dirMode      = 0o700
	fileMode     = 0o600
)

var (
	// ErrNotFound is returned for a backup that does not exist.
	ErrNotFound = errors.New("backup not found")
	// ErrExists refuses a backup into a directory that already exists.
	ErrExists = errors.New("backup already exists")
	// ErrBusy refuses a verify or delete while a backup writes the pool.
	ErrBusy = errors.New("a backup is running; try again when it has finished")
	// ErrInvalid marks invalid parameters (an unsafe subdir, keep out of range).
	ErrInvalid = errors.New("invalid backup parameters")

	errMissing = errors.New("missing from its blob store")
	errCorrupt = errors.New("its content does not match its sha256")
)

var segmentRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidSubdir checks a backup name: one path segment of 1-100 characters
// from [A-Za-z0-9._-], not "." or "..", and not the pool directory.
func ValidSubdir(s string) error {
	switch {
	case s == "" || len(s) > maxSubdir || !segmentRE.MatchString(s):
		return fmt.Errorf("%w: subdir must be one path segment of 1-%d letters, digits, '.', '_' or '-'", ErrInvalid, maxSubdir)
	case s == "." || s == "..":
		return fmt.Errorf("%w: subdir must not be %q", ErrInvalid, s)
	case strings.EqualFold(s, poolDir):
		return fmt.Errorf("%w: subdir %q is reserved for the shared blob pool", ErrInvalid, s)
	case len(s) >= len(tmpPrefix) && strings.EqualFold(s[:len(tmpPrefix)], tmpPrefix):
		return fmt.Errorf("%w: subdir must not start with %q", ErrInvalid, tmpPrefix)
	}
	return nil
}

// Params are the backup task's parameters.
type Params struct {
	Subdir string `json:"subdir,omitempty"` // default: the start time, YYYYMMDD-HHMMSS
	// Keep is how many complete backups (missingBlobs 0, this one included)
	// retention keeps; older ones are deleted. 0 means DefaultKeep.
	Keep   int  `json:"keep,omitempty"`
	Verify bool `json:"verify,omitempty"` // verify the new backup
	Deep   bool `json:"deep,omitempty"`   // the verify re-hashes every blob (implies verify)
}

// Normalize validates p and fills in the defaults.
func (p Params) Normalize() (Params, error) {
	if p.Subdir != "" {
		if err := ValidSubdir(p.Subdir); err != nil {
			return p, err
		}
	}
	if p.Keep == 0 {
		p.Keep = DefaultKeep
	}
	if p.Keep < 1 || p.Keep > MaxKeep {
		return p, fmt.Errorf("%w: keep must be between 1 and %d", ErrInvalid, MaxKeep)
	}
	if p.Deep {
		p.Verify = true
	}
	return p, nil
}

// Manifest describes one backup; it is stored as manifest.json.
type Manifest struct {
	Version    int               `json:"version"`
	Product    string            `json:"product"`
	AppVersion string            `json:"appVersion"`
	Name       string            `json:"name"`
	Created    time.Time         `json:"created"`
	BlobStores map[string]string `json:"blobStores"` // name -> path as recorded (relative paths are relative to the data dir)
	// RepositoryStores maps each repository of the snapshot to its blob
	// store (repo.StoreOf); assets of a repository not listed are in the
	// default store.
	RepositoryStores map[string]string `json:"repositoryStores"`
	MetaSHA256       string            `json:"metaSha256"` // of meta.db
	// RefsSHA256 and RefsLines describe the refs file (written before the
	// manifest); the sweep and restore verify them. A legacy backup has
	// neither (RefsSHA256 is empty).
	RefsSHA256   string `json:"refsSha256,omitempty"`
	RefsLines    int64  `json:"refsLines"`
	Repositories int    `json:"repositories"`
	Assets       int64  `json:"assets"`
	Blobs        int64  `json:"blobs"` // distinct (store, sha256) pairs
	Bytes        int64  `json:"bytes"`
	CopiedBlobs  int64  `json:"copiedBlobs"`
	CopiedBytes  int64  `json:"copiedBytes"`
	MissingBlobs int64  `json:"missingBlobs,omitempty"` // referenced but missing or corrupt in the source store
	DurationMs   int64  `json:"durationMs"`

	// What the run did after writing the manifest; not stored.
	Verified      *VerifyResult `json:"-"`
	PrunedBackups int           `json:"-"`
	SweptBlobs    int64         `json:"-"`
	SweptBytes    int64         `json:"-"`
	SweepSkipped  string        `json:"-"` // why the pool sweep was skipped; "" when it ran
}

// Complete reports whether every referenced blob was backed up.
func (m Manifest) Complete() bool { return m.MissingBlobs == 0 }

// Info is a listed backup.
type Info struct {
	Manifest
	MetaBytes int64         `json:"metaBytes"`
	Verified  *VerifyResult `json:"verified,omitempty"` // the last verify, nil if never verified
	// Incomplete marks a directory with a meta.db and no manifest (a backup
	// that did not finish, or lost its manifest); only ListAll returns it,
	// and Delete removes it. Its Created is the directory's modification time.
	Incomplete bool `json:"incomplete,omitempty"`
}

// VerifyResult is the outcome of a verify; the last one is kept as verify.json.
type VerifyResult struct {
	Name     string    `json:"name"`
	At       time.Time `json:"at"`
	Deep     bool      `json:"deep"`
	OK       bool      `json:"ok"`
	Blobs    int64     `json:"blobs"`
	Bytes    int64     `json:"bytes"`
	Missing  int64     `json:"missing"`
	Corrupt  int64     `json:"corrupt"`
	Repaired int64     `json:"repaired"` // corrupt pool blobs removed so the next backup copies them again
	Problems []string  `json:"problems"`
}

// Logger receives a run's log lines (tasks.RunLog).
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

type discard struct{}

func (discard) Infof(string, ...any)  {}
func (discard) Warnf(string, ...any)  {}
func (discard) Errorf(string, ...any) {}

// ErrPostgres is why there are no online backups on PostgreSQL: they copy
// the SQLite file meta.db, and the offline restore writes one back.
var ErrPostgres = errors.New("online backup and restore support only the SQLite metadata database; " +
	"with PostgreSQL, back up the database with its own tools (pg_dump) and copy the blob stores separately " +
	"(operations guide, \"백업과 복원\")")

// Service makes, lists, verifies and deletes the backups under Dir.
type Service struct {
	// Disabled, when set, is returned by every operation (ErrPostgres when
	// the metadata database is PostgreSQL); List returns nothing.
	Disabled error

	DB      *meta.DB    // the live database
	Store   *repo.Store // the live blob stores
	Dir     string      // the backup root (config backupDir)
	Version string      // recorded in the manifest
	Events  repo.EventSink
	Log     *slog.Logger
	Now     func() time.Time

	// AfterSnapshot, if set, runs after the database snapshot and before
	// the blobs are copied (a test hook).
	AfterSnapshot func()

	// mu is held for writing by a backup, a delete and a repairing verify
	// (they change the pool) and for reading by a plain verify.
	mu sync.RWMutex
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Service) emit(kind, actor, subject, detail string) {
	if s.Events != nil {
		s.Events.Emit(kind, actor, "", subject, detail)
	}
}

func (s *Service) poolPath(store, sha string) string {
	return filepath.Join(s.Dir, poolDir, store, "sha256", sha[:2], sha[2:4], sha)
}

// Run makes a backup. The caller must keep blob deletes out for the whole
// run (the task scheduler's run lock and the data directory lock do, for the
// backup task). A backup with blobs missing from their source store is kept,
// with missingBlobs in its manifest, and Run returns an error; retention then
// does not run.
func (s *Service) Run(ctx context.Context, p Params, actor string, log Logger) (Manifest, error) {
	if s.Disabled != nil {
		return Manifest{}, s.Disabled
	}
	if log == nil {
		log = discard{}
	}
	p, err := p.Normalize()
	if err != nil {
		return Manifest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	start := s.now()
	if err := os.MkdirAll(s.Dir, dirMode); err != nil {
		return Manifest{}, fmt.Errorf("create the backup root: %w", err)
	}
	name, err := s.makeDir(p.Subdir, start)
	if err != nil {
		return Manifest{}, err
	}
	dst := filepath.Join(s.Dir, name)
	kept := false
	defer func() {
		if !kept {
			if err := os.RemoveAll(dst); err != nil {
				log.Warnf("could not remove the incomplete backup %s: %v", dst, err)
			}
		}
	}()
	log.Infof("backing up into %s", dst)

	// 1. A consistent snapshot of the database. VACUUM INTO reads in one
	// transaction; writers continue meanwhile.
	if err := s.DB.Checkpoint(ctx); err != nil {
		log.Warnf("WAL checkpoint: %v", err)
	}
	tmp := filepath.Join(dst, tmpPrefix+metaFile)
	if err := s.DB.VacuumInto(ctx, tmp); err != nil {
		return Manifest{}, fmt.Errorf("snapshot the metadata database: %w", err)
	}
	if err := os.Chmod(tmp, fileMode); err != nil {
		return Manifest{}, err
	}
	metaPath := filepath.Join(dst, metaFile)
	if err := os.Rename(tmp, metaPath); err != nil {
		return Manifest{}, err
	}
	syncDir(dst)
	metaSHA, err := hashFile(metaPath, make([]byte, copyBufSize))
	if err != nil {
		return Manifest{}, fmt.Errorf("hash the snapshot: %w", err)
	}
	log.Infof("metadata database snapshot written")
	if s.AfterSnapshot != nil {
		s.AfterSnapshot()
	}

	// 2. The blobs the snapshot references.
	snap, err := meta.OpenReadOnly(metaPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("open the snapshot: %w", err)
	}
	defer snap.Close()
	repoStores, err := StoreMap(ctx, snap)
	if err != nil {
		return Manifest{}, fmt.Errorf("read the snapshot's repositories: %w", err)
	}

	m := Manifest{Version: ManifestVersion, Product: "commandcenter", AppVersion: s.Version, Name: name,
		Created: start.UTC(), BlobStores: map[string]string{}, RepositoryStores: repoStores,
		MetaSHA256: metaSHA, Repositories: len(repoStores)}
	rows, err := snap.ListBlobStores(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("read the snapshot's blob stores: %w", err)
	}
	for _, bs := range rows {
		m.BlobStores[bs.Name] = bs.Path
	}

	if m.Assets, err = snap.CountAssets(ctx); err != nil {
		return m, fmt.Errorf("count the snapshot's assets: %w", err)
	}

	// The refs file lists what this backup references, sorted, for the
	// pool sweep; it is written while the blobs are copied.
	refs, err := newRefsWriter(dst)
	if err != nil {
		return m, err
	}
	defer refs.abort()
	buf := make([]byte, copyBufSize)
	last := time.Now()
	err = eachRef(ctx, snap, repoStores, func(store, sha string, _ int64) error {
		m.Blobs++
		if err := refs.add(store, sha); err != nil {
			return fmt.Errorf("write refs: %w", err)
		}
		size, copied, err := s.copyBlob(store, sha, buf)
		if errors.Is(err, errMissing) || errors.Is(err, errCorrupt) {
			m.MissingBlobs++
			if m.MissingBlobs <= maxProblems {
				log.Errorf("blob %s of blob store %q: %v", sha, store, err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("copy blob %s of blob store %q: %w", sha, store, err)
		}
		m.Bytes += size
		if copied {
			m.CopiedBlobs++
			m.CopiedBytes += size
		}
		if time.Since(last) >= progressEach {
			last = time.Now()
			log.Infof("%d blobs so far (%d bytes), %d copied (%d bytes)", m.Blobs, m.Bytes, m.CopiedBlobs, m.CopiedBytes)
		}
		return nil
	})
	if err != nil {
		return m, err
	}
	if err := refs.commit(); err != nil {
		return m, fmt.Errorf("write refs: %w", err)
	}
	m.RefsSHA256, m.RefsLines = refs.sum, refs.lines
	if m.MissingBlobs > maxProblems {
		log.Errorf("%d more blobs were missing or corrupt", m.MissingBlobs-maxProblems)
	}

	// 3. The manifest, last: it marks the backup complete.
	m.DurationMs = s.now().Sub(start).Milliseconds()
	if err := writeJSON(filepath.Join(dst, manifestFile), m); err != nil {
		return m, fmt.Errorf("write the manifest: %w", err)
	}
	kept = true
	log.Infof("backup %s: %d repositories, %d assets, %d blobs (%d bytes); copied %d new blobs (%d bytes)",
		name, m.Repositories, m.Assets, m.Blobs, m.Bytes, m.CopiedBlobs, m.CopiedBytes)
	s.emit("backup", actor, name, fmt.Sprintf("blobs=%d bytes=%d copiedBlobs=%d copiedBytes=%d missingBlobs=%d",
		m.Blobs, m.Bytes, m.CopiedBlobs, m.CopiedBytes, m.MissingBlobs))
	if m.MissingBlobs > 0 {
		return m, fmt.Errorf("%d referenced blobs were missing or corrupt in their blob store, so backup %s is incomplete (old backups were not pruned)", m.MissingBlobs, name)
	}

	// 4. Verify, retention and the pool sweep.
	if p.Verify {
		res, err := s.verify(ctx, name, p.Deep, p.Deep)
		if err != nil {
			return m, fmt.Errorf("verify: %w", err)
		}
		m.Verified = &res
		logVerify(log, res)
		if !res.OK {
			return m, fmt.Errorf("verify of backup %s failed: %d missing, %d corrupt (old backups were not pruned)", name, res.Missing, res.Corrupt)
		}
	}
	pruned, err := s.prune(p.Keep, name, log)
	m.PrunedBackups = len(pruned)
	if len(pruned) > 0 {
		s.emit("backup.prune", actor, strings.Join(pruned, ","), fmt.Sprintf("keep=%d pruned=%s", p.Keep, strings.Join(pruned, ",")))
	}
	if err != nil {
		return m, fmt.Errorf("backup %s was written, but retention failed: %w", name, err)
	}
	m.SweptBlobs, m.SweptBytes, m.SweepSkipped = s.sweep(ctx, log)
	return m, nil
}

func logVerify(log Logger, res VerifyResult) {
	log.Infof("verify of %s (deep=%v): %d blobs, %d missing, %d corrupt, %d repaired", res.Name, res.Deep, res.Blobs, res.Missing, res.Corrupt, res.Repaired)
	if !res.OK {
		for _, pr := range res.Problems {
			log.Errorf("verify: %s", pr)
		}
	}
}

// numeric suffix when that name is taken.
func (s *Service) makeDir(subdir string, start time.Time) (string, error) {
	if subdir != "" {
		err := os.Mkdir(filepath.Join(s.Dir, subdir), dirMode)
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%w: %s", ErrExists, subdir)
		}
		return subdir, err
	}
	base := start.Format(dateLayout)
	for i := 1; i <= 100; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		err := os.Mkdir(filepath.Join(s.Dir, name), dirMode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return name, err
	}
	return "", fmt.Errorf("%w: %s", ErrExists, base)
}

// noWriterTo hides *os.File's WriteTo, so io.CopyBuffer uses the given buffer.
type noWriterTo struct{ io.Reader }

// copyBlob copies blob sha of the named live store into the pool, unless the
// pool has it with the same size. It hashes while copying and commits the
// temp file with a rename only when the hash matches.
func (s *Service) copyBlob(store, sha string, buf []byte) (size int64, copied bool, err error) {
	if !validSHA(sha) || !validStore(store) {
		return 0, false, fmt.Errorf("%w (invalid store %q or blob id)", errMissing, store)
	}
	src := s.Store.StoreByName(store)
	if src == nil {
		return 0, false, fmt.Errorf("%w (the blob store does not exist)", errMissing)
	}
	f, err := src.Open(sha)
	if errors.Is(err, blob.ErrNotFound) {
		return 0, false, errMissing
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	dst := s.poolPath(store, sha)
	if di, err := os.Lstat(dst); err == nil && di.Mode().IsRegular() && di.Size() == fi.Size() {
		return fi.Size(), false, nil
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return 0, false, err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return 0, false, err
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(tmp, h), noWriterTo{f}, buf)
	if err != nil {
		return 0, false, err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		return 0, false, errCorrupt
	}
	if err := tmp.Sync(); err != nil {
		return 0, false, err
	}
	if err := tmp.Close(); err != nil {
		return 0, false, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return 0, false, err
	}
	committed = true
	syncDir(dir)
	return n, true, nil
}

// StoreMap returns the blob store of each repository of a snapshot
// (repo.StoreOf). Restore uses the same rule; a repository not listed (an
// asset left by a deleted repository) resolves to the default store, like
// live routing.
func StoreMap(ctx context.Context, snap *meta.DB) (map[string]string, error) {
	defs, err := snap.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(defs))
	for _, d := range defs {
		m[d.Name] = repo.StoreOf(d)
	}
	return m, nil
}

// Exists returns nil when backup name is complete (has a manifest),
// ErrNotFound when it does not exist, or ErrInvalid for an unsafe name.
func (s *Service) Exists(name string) error {
	if err := ValidSubdir(name); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.Dir, name, manifestFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return err
	}
	return nil
}

// List returns the complete backups (those with a manifest), newest first.
func (s *Service) List() ([]Info, error) { return s.list(false) }

// ListAll is List plus the incomplete directories (a meta.db without a
// manifest), so they can be shown and deleted. While a backup runs its
// directory looks incomplete, so then none are listed.
func (s *Service) ListAll() ([]Info, error) {
	if !s.mu.TryRLock() {
		return s.list(false)
	}
	defer s.mu.RUnlock()
	return s.list(true)
}

func (s *Service) list(incomplete bool) ([]Info, error) {
	if s.Disabled != nil {
		return []Info{}, nil
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Info{}
	for _, e := range entries {
		if !e.IsDir() || ValidSubdir(e.Name()) != nil {
			continue
		}
		dir := filepath.Join(s.Dir, e.Name())
		var m Manifest
		if err := readJSON(filepath.Join(dir, manifestFile), &m); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				s.log().Warn("unreadable backup manifest", "backup", e.Name(), "err", logsafe.Err(err))
				continue
			}
			if !incomplete {
				continue
			}
			mi, merr := os.Stat(filepath.Join(dir, metaFile))
			if merr != nil {
				continue
			}
			created := mi.ModTime().UTC()
			if di, derr := e.Info(); derr == nil {
				created = di.ModTime().UTC()
			}
			out = append(out, Info{Manifest: Manifest{Name: e.Name(), Created: created}, MetaBytes: mi.Size(), Incomplete: true})
			continue
		}
		m.Name = e.Name()
		info := Info{Manifest: m}
		if fi, err := os.Stat(filepath.Join(dir, metaFile)); err == nil {
			info.MetaBytes = fi.Size()
		}
		var v VerifyResult
		if err := readJSON(filepath.Join(dir, verifyFile), &v); err == nil {
			info.Verified = &v
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// prune keeps the newest keep complete backups, the incomplete ones newer
// than the oldest of those, and current; it deletes the rest and returns
// their names.
func (s *Service) prune(keep int, current string, log Logger) ([]string, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	var pruned []string
	complete := 0
	for _, b := range list {
		if b.Name == current {
			if b.Complete() {
				complete++
			}
			continue
		}
		if complete < keep {
			if b.Complete() {
				complete++
			}
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Dir, b.Name)); err != nil {
			return pruned, fmt.Errorf("delete backup %s: %w", b.Name, err)
		}
		pruned = append(pruned, b.Name)
		log.Infof("retention: deleted backup %s (keeping %d complete backups)", b.Name, keep)
	}
	if len(pruned) > 0 {
		syncDir(s.Dir)
	}
	return pruned, nil
}

// Verify checks backup name: every blob its snapshot references is in the
// pool with the recorded size, and meta.db matches its recorded sha256; deep
// also re-hashes every blob and runs an SQLite quick check on the snapshot.
// It changes nothing but verify.json. ErrBusy while a backup or delete runs.
func (s *Service) Verify(ctx context.Context, name string, deep bool) (VerifyResult, error) {
	if s.Disabled != nil {
		return VerifyResult{}, s.Disabled
	}
	if err := ValidSubdir(name); err != nil {
		return VerifyResult{}, err
	}
	if !s.mu.TryRLock() {
		return VerifyResult{}, ErrBusy
	}
	defer s.mu.RUnlock()
	return s.verify(ctx, name, deep, false)
}

// VerifyAndRepair deep-verifies backup name ("" is the newest backup) and
// removes every corrupt pool blob it finds, so the next backup copies it
// again; it runs as the backup.verify task, under the run lock. It emits
// the backup.verify event.
func (s *Service) VerifyAndRepair(ctx context.Context, name, actor string, log Logger) (VerifyResult, error) {
	if s.Disabled != nil {
		return VerifyResult{}, s.Disabled
	}
	if log == nil {
		log = discard{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		list, err := s.List()
		if err != nil {
			return VerifyResult{}, err
		}
		if len(list) == 0 {
			return VerifyResult{}, fmt.Errorf("%w: there are no backups", ErrNotFound)
		}
		name = list[0].Name
	}
	if err := ValidSubdir(name); err != nil {
		return VerifyResult{}, err
	}
	res, err := s.verify(ctx, name, true, true)
	if err != nil {
		return res, err
	}
	logVerify(log, res)
	state := "OK"
	if !res.OK {
		state = "Failed"
	}
	s.emit("backup.verify", actor, name, fmt.Sprintf("%s: deep=true blobs=%d missing=%d corrupt=%d repaired=%d",
		state, res.Blobs, res.Missing, res.Corrupt, res.Repaired))
	return res, nil
}

const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// verify checks backup name; with repair (deep only, mu held for writing)
// corrupt pool blobs are removed.
func (s *Service) verify(ctx context.Context, name string, deep, repair bool) (VerifyResult, error) {
	dir := filepath.Join(s.Dir, name)
	var m Manifest
	if err := readJSON(filepath.Join(dir, manifestFile), &m); errors.Is(err, fs.ErrNotExist) {
		return VerifyResult{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	} else if err != nil {
		return VerifyResult{}, fmt.Errorf("manifest of %s: %w", name, err)
	}
	res := VerifyResult{Name: name, At: s.now().UTC(), Deep: deep, Problems: []string{}}
	var more int
	problem := func(format string, args ...any) {
		if len(res.Problems) < maxProblems {
			res.Problems = append(res.Problems, fmt.Sprintf(format, args...))
		} else {
			more++
		}
	}
	buf := make([]byte, copyBufSize)
	metaPath := filepath.Join(dir, metaFile)
	if m.MetaSHA256 != "" {
		if got, err := hashFile(metaPath, buf); err != nil {
			problem("meta.db cannot be read: %v", err)
		} else if got != m.MetaSHA256 {
			problem("meta.db has sha256 %s, the manifest records %s", got, m.MetaSHA256)
		}
	}
	err := func() error {
		snap, err := meta.OpenReadOnly(metaPath)
		if err != nil {
			problem("the snapshot cannot be opened: %v", err)
			return nil
		}
		defer snap.Close()
		if deep {
			if r, err := snap.QuickCheck(ctx); err != nil || r != "ok" {
				problem("the snapshot fails the SQLite quick check: %s %v", r, err)
			}
		}
		stores, err := StoreMap(ctx, snap)
		if err != nil {
			problem("the snapshot's repositories cannot be read: %v", err)
			return nil
		}
		return eachRef(ctx, snap, stores, func(store, sha string, size int64) error {
			res.Blobs++
			if !validSHA(sha) || !validStore(store) {
				res.Missing++
				problem("invalid blob reference %q in blob store %q", sha, store)
				return nil
			}
			p := s.poolPath(store, sha)
			fi, err := os.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				res.Missing++
				problem("blob %s of blob store %q is missing", sha, store)
				return nil
			}
			if err != nil {
				return err
			}
			res.Bytes += fi.Size()
			if !fi.Mode().IsRegular() {
				res.Corrupt++
				problem("blob %s of blob store %q is not a regular file", sha, store)
				return nil
			}
			var reasons []string
			if (size > 0 || sha == emptySHA) && fi.Size() != size {
				reasons = append(reasons, fmt.Sprintf("has %d bytes, the snapshot records %d", fi.Size(), size))
			}
			hashBad := false
			if deep {
				got, err := hashFile(p, buf)
				if err != nil {
					return err
				}
				if got != sha {
					hashBad = true
					reasons = append(reasons, "has sha256 "+got)
				}
			}
			if len(reasons) == 0 {
				return nil
			}
			res.Corrupt++
			msg := strings.Join(reasons, " and ")
			// Only content proven wrong by the re-hash is removed; a size
			// that differs from the snapshot's record alone is reported.
			if repair && hashBad {
				if err := os.Remove(p); err != nil {
					return err
				}
				res.Repaired++
				msg += " (removed from the pool; the next backup copies it again)"
			}
			problem("blob %s of blob store %q %s", sha, store, msg)
			return nil
		})
	}()
	if err != nil {
		return res, err
	}
	if more > 0 {
		res.Problems = append(res.Problems, fmt.Sprintf("… and %d more problems", more))
	}
	res.OK = res.Missing == 0 && res.Corrupt == 0 && len(res.Problems) == 0
	if err := writeJSON(filepath.Join(dir, verifyFile), res); err != nil {
		s.log().Warn("could not store the verify result", "backup", name, "err", logsafe.Err(err))
	}
	return res, nil
}

func hashFile(path string, buf []byte) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, noWriterTo{f}, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Delete removes backup name (its directory) and sweeps the pool. ErrBusy
// while a backup runs; ErrNotFound when there is no such directory. An
// incomplete backup (no manifest) can be deleted too.
func (s *Service) Delete(ctx context.Context, name string) (DeleteResult, error) {
	if s.Disabled != nil {
		return DeleteResult{}, s.Disabled
	}
	if err := ValidSubdir(name); err != nil {
		return DeleteResult{}, err
	}
	if !s.mu.TryLock() {
		return DeleteResult{}, ErrBusy
	}
	defer s.mu.Unlock()
	dir := filepath.Join(s.Dir, name)
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
		return DeleteResult{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return DeleteResult{}, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return DeleteResult{}, err
	}
	syncDir(s.Dir)
	res := DeleteResult{Name: name}
	res.SweptBlobs, res.SweptBytes, res.SweepSkipped = s.sweep(ctx, discard{})
	s.log().Info("backup deleted", "backup", name, "sweptBlobs", res.SweptBlobs, "sweptBytes", res.SweptBytes, "sweepSkipped", res.SweepSkipped)
	return res, nil
}

// DeleteResult is what a delete did.
type DeleteResult struct {
	Name         string `json:"name"`
	SweptBlobs   int64  `json:"sweptBlobs"`
	SweptBytes   int64  `json:"sweptBytes"`
	SweepSkipped string `json:"sweepSkipped,omitempty"` // why the pool sweep was skipped
}

// writeJSON writes v to path atomically (temp file, fsync, rename, fsync of
// the directory); the file is 0600.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPrefix+"*")
	if err != nil {
		return err
	}
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir fsyncs a directory so a rename in it is durable; best effort
// (Windows cannot sync a directory).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validStore reports whether a blob store name is safe as a pool directory.
func validStore(s string) bool {
	return s != "" && len(s) <= 255 && s != "." && s != ".." && segmentRE.MatchString(s)
}
