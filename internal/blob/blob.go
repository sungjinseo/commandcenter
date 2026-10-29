// Package blob stores content-addressed files keyed by SHA-256, on disk
// (Open) or in an S3 bucket (OpenS3).
package blob

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	ErrNotFound = errors.New("blob not found")
	ErrDiskFull = errors.New("disk full")
)

// Sums are the digests and size of a stored blob.
type Sums struct {
	SHA256, SHA1, MD5, SHA512 string
	Size                      int64
}

// Store is one blob store: a tmp directory under root for uploads in
// progress (Write spools there, Docker sessions live there) and a backend
// that holds the committed blobs.
type Store struct {
	root string
	be   backend
}

// backend holds committed blobs. put stores the closed, synced temp file tmp
// (digests sums) under sums.SHA256, replacing or refreshing an existing copy
// so that Walk and Stat report a fresh modTime; tmp is removed on every
// return. Everything else is the Store method of the same name.
type backend interface {
	put(tmp string, sums Sums) error
	open(sha string) (File, error)
	stat(sha string) (Info, error)
	remove(sha string) error
	walk(fn func(sha string, size int64, modTime time.Time) error) error
	check() error
}

// File is an open blob: http.ServeContent needs Seek, zip readers ReadAt.
type File interface {
	io.ReadSeekCloser
	io.ReaderAt
}

// Info is what Stat reports about a blob.
type Info struct {
	Size    int64
	ModTime time.Time
}

// dir is the filesystem backend: blobs/sha256/xx/yy/<sha> under root.
type dir struct{ root string }

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

// tmpPrefix names Write's temp files; SweepTmp removes nothing else, since a
// store may be created in a directory that holds other files.
const tmpPrefix = "up-"

// tmpMaxAge is how long a temp file may go unmodified before SweepTmp treats
// it as the leftover of a process killed mid-write. A live write touches its
// file at least once per upload or fetch idle timeout (minutes), so a CLI
// command started next to a running server never removes a write in progress.
const tmpMaxAge = time.Hour

// sessionPrefix names resumable upload sessions (Docker chunked uploads)
// under tmp: the data file and its ".json" sidecar. They outlive the
// request and a restart, so SweepTmp keeps them for SessionMaxAge after
// their last write instead of tmpMaxAge.
const sessionPrefix = "ses-"

// SessionMaxAge is how long an upload session may go without a write before
// SweepTmp removes it (Distribution expires uploads after 24 h).
const SessionMaxAge = 24 * time.Hour

// Open prepares the blob directories under root and sweeps stale temp files.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o755); err != nil {
		return nil, err
	}
	return open(root, dir{root})
}

// open prepares root/tmp for a store on be and sweeps stale temp files.
func open(root string, be backend) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{root: root, be: be}
	s.SweepTmp() // best effort: what it cannot remove now, the next sweep retries
	return s, nil
}

// SweepTmp removes Write's temp files that have not been modified for
// tmpMaxAge, and upload sessions (SessionPath) not written to for
// SessionMaxAge, and returns how many it removed. Write removes its temp
// file on every return, so only a killed process (SIGKILL, OOM, a stop
// timeout) leaves one behind; a session is left behind by a client that
// never finished it.
func (s *Store) SweepTmp() (int, error) {
	dir := filepath.Join(s.root, "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	n := 0
	var errs []error
	for _, e := range entries {
		var cutoff time.Time
		switch {
		case strings.HasPrefix(e.Name(), tmpPrefix):
			cutoff = now.Add(-tmpMaxAge)
		case strings.HasPrefix(e.Name(), sessionPrefix):
			cutoff = now.Add(-SessionMaxAge)
		default:
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			n++
		} else if !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// SessionPath is the file an upload session id (hex, the caller's to
// validate) accumulates its bytes in; its sidecar is the same path with
// ".json". CommitFile moves a finished one into the store.
func (s *Store) SessionPath(id string) string {
	return filepath.Join(s.root, "tmp", sessionPrefix+id)
}

func (d dir) path(sha string) string {
	return filepath.Join(d.root, "blobs", "sha256", sha[:2], sha[2:4], sha)
}

// Write streams r to a temp file while hashing it, then commits the file
// under its SHA-256. Identical content is stored once.
func (s *Store) Write(r io.Reader) (Sums, error) {
	f, err := os.CreateTemp(filepath.Join(s.root, "tmp"), tmpPrefix+"*")
	if err != nil {
		return Sums{}, mapErr(err)
	}
	tmp := f.Name()
	committed := false
	defer func() {
		if !committed {
			f.Close()
			os.Remove(tmp)
		}
	}()

	sums, err := hashTo(f, r)
	if err != nil {
		return Sums{}, mapErr(err)
	}
	if err := f.Sync(); err != nil {
		return Sums{}, mapErr(err)
	}
	if err := f.Close(); err != nil {
		return Sums{}, mapErr(err)
	}
	committed = true // commit removes tmp on every return
	return s.commit(tmp, sums)
}

// commit hands the closed, synced temp file to the backend; tmp is removed
// on every return.
func (s *Store) commit(tmp string, sums Sums) (Sums, error) {
	if err := s.be.put(tmp, sums); err != nil {
		return Sums{}, mapErr(err)
	}
	return sums, nil
}

// CommitFile hashes the file at path (a SessionPath the caller wrote and
// synced) and commits it under its SHA-256 like Write, without copying it
// again; path is gone on return, whatever happens.
func (s *Store) CommitFile(path string) (Sums, error) {
	f, err := os.Open(path)
	if err != nil {
		os.Remove(path)
		return Sums{}, mapErr(err)
	}
	sums, err := hashTo(io.Discard, f)
	f.Close()
	if err != nil {
		os.Remove(path)
		return Sums{}, mapErr(err)
	}
	return s.commit(path, sums)
}

// hashTo copies r to w and returns the digests and size of what it copied.
func hashTo(w io.Writer, r io.Reader) (Sums, error) {
	h256, h1, h5, h512 := sha256.New(), sha1.New(), md5.New(), sha512.New()
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	n, err := io.CopyBuffer(io.MultiWriter(w, h256, h1, h5, h512), r, *buf)
	if err != nil {
		return Sums{}, err
	}
	return Sums{SHA256: sum(h256), SHA1: sum(h1), MD5: sum(h5), SHA512: sum(h512), Size: n}, nil
}

// put moves the closed, synced file tmp with digests sums to its blob path.
func (d dir) put(tmp string, sums Sums) error {
	defer os.Remove(tmp)
	dst := d.path(sums.SHA256)
	// A size mismatch means the stored blob is corrupt (e.g. truncated by a
	// crash); do not dedup against it, replace it with the fresh copy.
	if info, err := os.Stat(dst); err == nil && info.Size() == sums.Size {
		// Refresh mtime so GC's age guard protects a re-upload that is not yet
		// recorded as an asset.
		now := time.Now()
		return os.Chtimes(dst, now, now)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	err := rename(tmp, dst)
	if errors.Is(err, syscall.EXDEV) { // a group's temp file, on another filesystem than this member
		return d.copyIn(tmp, dst)
	}
	return err
}

// rename is os.Rename; a test makes it fail across filesystems.
var rename = os.Rename

// copyIn copies src to dst through a temp file in d's own tmp directory, for
// a src that cannot be renamed into d because it is on another filesystem.
func (d dir) copyIn(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.CreateTemp(filepath.Join(d.root, "tmp"), tmpPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // a no-op after the rename
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

// Open opens the blob with the given SHA-256 for reading.
func (s *Store) Open(sha string) (File, error) {
	if !validSHA(sha) {
		return nil, fmt.Errorf("invalid blob id %q", sha)
	}
	return s.be.open(sha)
}

func (d dir) open(sha string) (File, error) {
	f, err := os.Open(d.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Remove deletes the blob. Removing a missing blob is not an error.
func (s *Store) Remove(sha string) error {
	if !validSHA(sha) {
		return fmt.Errorf("invalid blob id %q", sha)
	}
	return s.be.remove(sha)
}

func (d dir) remove(sha string) error {
	err := os.Remove(d.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Walk calls fn for every stored blob (for a group: every member's copy).
func (s *Store) Walk(fn func(sha string, modTime time.Time) error) error {
	return s.be.walk(func(sha string, _ int64, mod time.Time) error { return fn(sha, mod) })
}

// Usage walks the store and returns how many blobs it holds and their bytes
// (for a group: every member's copies); it stops when ctx is done.
func (s *Store) Usage(ctx context.Context) (blobs, bytes int64, err error) {
	err = s.be.walk(func(_ string, size int64, _ time.Time) error {
		blobs, bytes = blobs+1, bytes+size
		return ctx.Err()
	})
	return blobs, bytes, err
}

func (d dir) walk(fn func(sha string, size int64, modTime time.Time) error) error {
	return filepath.WalkDir(filepath.Join(d.root, "blobs", "sha256"), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !validSHA(e.Name()) {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		return fn(e.Name(), info.Size(), info.ModTime())
	})
}

func sum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

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

func mapErr(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%w: %v", ErrDiskFull, err)
	}
	return err
}

// Stat returns the blob's size and modification time, or ErrNotFound if it
// is missing.
func (s *Store) Stat(sha string) (Info, error) {
	if !validSHA(sha) {
		return Info{}, fmt.Errorf("invalid blob id %q", sha)
	}
	return s.be.stat(sha)
}

func (d dir) stat(sha string) (Info, error) {
	info, err := os.Stat(d.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, err
	}
	return Info{Size: info.Size(), ModTime: info.ModTime()}, nil
}
