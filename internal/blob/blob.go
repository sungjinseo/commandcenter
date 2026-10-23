// Package blob stores content-addressed files on disk, keyed by SHA-256.
package blob

import (
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

type Store struct {
	root string
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

// Open prepares the blob directories under root.
func Open(root string) (*Store, error) {
	for _, d := range []string{"blobs/sha256", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

func (s *Store) path(sha string) string {
	return filepath.Join(s.root, "blobs", "sha256", sha[:2], sha[2:4], sha)
}

// Write streams r to a temp file while hashing it, then commits the file
// under its SHA-256. Identical content is stored once.
func (s *Store) Write(r io.Reader) (Sums, error) {
	f, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "up-*")
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

	h256, h1, h5, h512 := sha256.New(), sha1.New(), md5.New(), sha512.New()
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	n, err := io.CopyBuffer(io.MultiWriter(f, h256, h1, h5, h512), r, *buf)
	if err != nil {
		return Sums{}, mapErr(err)
	}
	if err := f.Sync(); err != nil {
		return Sums{}, mapErr(err)
	}
	if err := f.Close(); err != nil {
		return Sums{}, mapErr(err)
	}
	sums := Sums{SHA256: sum(h256), SHA1: sum(h1), MD5: sum(h5), SHA512: sum(h512), Size: n}

	dst := s.path(sums.SHA256)
	// A size mismatch means the stored blob is corrupt (e.g. truncated by a
	// crash); do not dedup against it, replace it with the fresh copy.
	if info, err := os.Stat(dst); err == nil && info.Size() == sums.Size {
		os.Remove(tmp)
		committed = true
		// Refresh mtime so GC's age guard protects a re-upload that is not yet
		// recorded as an asset.
		now := time.Now()
		if err := os.Chtimes(dst, now, now); err != nil {
			return Sums{}, mapErr(err)
		}
		return sums, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Sums{}, mapErr(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return Sums{}, mapErr(err)
	}
	committed = true
	return sums, nil
}

// Open opens the blob with the given SHA-256 for reading.
func (s *Store) Open(sha string) (*os.File, error) {
	if !validSHA(sha) {
		return nil, fmt.Errorf("invalid blob id %q", sha)
	}
	f, err := os.Open(s.path(sha))
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
	err := os.Remove(s.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Walk calls fn for every stored blob.
func (s *Store) Walk(fn func(sha string, modTime time.Time) error) error {
	return filepath.WalkDir(filepath.Join(s.root, "blobs", "sha256"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !validSHA(d.Name()) {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(d.Name(), info.ModTime())
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

// ModTime returns the blob's modification time, or ErrNotFound if it is missing.
func (s *Store) ModTime(sha string) (time.Time, error) {
	if !validSHA(sha) {
		return time.Time{}, fmt.Errorf("invalid blob id %q", sha)
	}
	info, err := os.Stat(s.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
