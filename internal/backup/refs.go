package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// refsFile lists, sorted by (store, sha256) and unique, the "<store>/<sha>"
// pool entries a backup's meta.db references (see the package comment).
const refsFile = "refs"

// refsBufSize is each refs reader's buffer: the sweep's memory is this times
// the number of backups.
const refsBufSize = 16 << 10

// eachRef calls fn once per distinct (store, sha) the snapshot references,
// ordered by store name then sha (the order of the refs file and of the
// pool walk), with the largest recorded size. It streams one SQL query per
// store; nothing but the current row is held.
func eachRef(ctx context.Context, snap *meta.DB, repoStores map[string]string, fn func(store, sha string, size int64) error) error {
	byStore := map[string][]string{}
	var nonDefault []string
	for r, st := range repoStores {
		if st != repo.DefaultStore {
			byStore[st] = append(byStore[st], r)
			nonDefault = append(nonDefault, r)
		}
	}
	stores := []string{repo.DefaultStore}
	for st := range byStore {
		stores = append(stores, st)
	}
	sort.Strings(stores)
	for _, st := range stores {
		visit := func(sha string, size int64) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn(st, sha, size)
		}
		var err error
		if st == repo.DefaultStore {
			// The default store also holds the assets of repositories the
			// snapshot does not list.
			err = snap.EachStoreBlob(ctx, nonDefault, true, visit)
		} else {
			err = snap.EachStoreBlob(ctx, byStore[st], false, visit)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ref is one pool entry; refs compare by store, then sha.
type ref struct{ store, sha string }

func (a ref) less(b ref) bool {
	if a.store != b.store {
		return a.store < b.store
	}
	return a.sha < b.sha
}

// refsWriter writes a backup's refs file through a temp file.
type refsWriter struct {
	dir   string
	f     *os.File
	w     *bufio.Writer
	h     hash.Hash // of the bytes written
	lines int64
	sum   string // sha256 of the committed file
	prev  ref
	done  bool
}

func newRefsWriter(dir string) (*refsWriter, error) {
	f, err := os.CreateTemp(dir, tmpPrefix+refsFile+"-*")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	return &refsWriter{dir: dir, f: f, h: h, w: bufio.NewWriterSize(io.MultiWriter(f, h), refsBufSize)}, nil
}

// add writes one entry; entries must come in eachRef order. Invalid names
// are left out (they cannot be in the pool).
func (rw *refsWriter) add(store, sha string) error {
	if !validStore(store) || !validSHA(sha) {
		return nil
	}
	r := ref{store, sha}
	if rw.prev.store != "" && !rw.prev.less(r) {
		return fmt.Errorf("refs out of order: %s/%s after %s/%s", store, sha, rw.prev.store, rw.prev.sha)
	}
	rw.prev = r
	if _, err := rw.w.WriteString(store + "/" + sha + "\n"); err != nil {
		return err
	}
	rw.lines++
	return nil
}

// commit flushes, syncs and renames the file to refs.
func (rw *refsWriter) commit() error {
	if err := rw.w.Flush(); err != nil {
		return err
	}
	if err := rw.f.Sync(); err != nil {
		return err
	}
	if err := rw.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(rw.f.Name(), filepath.Join(rw.dir, refsFile)); err != nil {
		return err
	}
	rw.done = true
	rw.sum = hex.EncodeToString(rw.h.Sum(nil))
	syncDir(rw.dir)
	return nil
}

// abort removes the temp file unless commit succeeded.
func (rw *refsWriter) abort() {
	if !rw.done {
		rw.f.Close()
		os.Remove(rw.f.Name())
	}
}

// generateRefs writes the refs file of a backup that has none, from its meta.db.
func generateRefs(ctx context.Context, dir string) error {
	snap, err := meta.OpenReadOnly(filepath.Join(dir, metaFile))
	if err != nil {
		return err
	}
	defer snap.Close()
	stores, err := StoreMap(ctx, snap)
	if err != nil {
		return err
	}
	rw, err := newRefsWriter(dir)
	if err != nil {
		return err
	}
	defer rw.abort()
	if err := eachRef(ctx, snap, stores, func(store, sha string, _ int64) error { return rw.add(store, sha) }); err != nil {
		return err
	}
	return rw.commit()
}

// refSource is one backup's refs stream.
type refSource struct {
	name string
	r    io.Reader
}

// refStream reads a refs stream in order, checking that it is sorted.
type refStream struct {
	name    string
	r       *bufio.Reader
	head    ref
	ok      bool // head is valid
	lines   int64
	matched int64
}

func (s *refStream) advance() error {
	for {
		line, err := s.r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("refs of %s: %w", s.name, err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if err != nil { // EOF
				s.ok = false
				return nil
			}
			continue
		}
		store, sha, found := strings.Cut(line, "/")
		if !found || !validStore(store) || !validSHA(sha) {
			return fmt.Errorf("refs of %s: invalid line %q", s.name, line)
		}
		r := ref{store, sha}
		if s.lines > 0 && !s.head.less(r) {
			return fmt.Errorf("refs of %s are not sorted (%s/%s after %s/%s)", s.name, store, sha, s.head.store, s.head.sha)
		}
		s.head, s.ok = r, true
		s.lines++
		return nil
	}
}

// mergeStats is what a merge saw.
type mergeStats struct {
	pool, live int64
	sources    []refStream // lines and matched per source (readers dropped)
}

// mergeRefs walks the pool entries (pool must yield them in ref order) and
// calls onDead for each one that no source lists. Every source is read to
// its end, so out-of-order streams are always found. Memory: one buffered
// reader per source.
func mergeRefs(ctx context.Context, pool func(yield func(store, sha string) error) error, srcs []refSource,
	onDead func(store, sha string) error) (mergeStats, error) {
	streams := make([]refStream, len(srcs))
	for i, src := range srcs {
		streams[i] = refStream{name: src.name, r: bufio.NewReaderSize(src.r, refsBufSize)}
		if err := streams[i].advance(); err != nil {
			return mergeStats{}, err
		}
	}
	var st mergeStats
	var prev ref
	err := pool(func(store, sha string) error {
		key := ref{store, sha}
		if st.pool > 0 && !prev.less(key) {
			return fmt.Errorf("the pool walk is not sorted at %s/%s", store, sha)
		}
		prev = key
		st.pool++
		if st.pool%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		live := false
		for i := range streams {
			s := &streams[i]
			for s.ok && s.head.less(key) {
				if err := s.advance(); err != nil {
					return err
				}
			}
			if s.ok && s.head == key {
				live = true
				s.matched++
			}
		}
		if live {
			st.live++
			return nil
		}
		return onDead(store, sha)
	})
	if err != nil {
		return st, err
	}
	for i := range streams {
		for streams[i].ok {
			if err := streams[i].advance(); err != nil {
				return st, err
			}
		}
		streams[i].r = nil
	}
	st.sources = streams
	return st, nil
}

// walkPool yields the pool's blobs in ref order: store directories sorted by
// name, then sha256/aa/bb/<sha> with every directory's entries sorted (the
// names are hex, so this is sha order). A file outside its aa/bb directory
// is ignored. onTmp, if set, receives the temp files a crashed copy left.
// A symbolic link anywhere is an error: links are not followed.
func walkPool(ctx context.Context, root string, yield func(store, sha string) error, onTmp func(path string) error) error {
	stores, err := readDirSorted(root)
	if err != nil {
		return err
	}
	for _, st := range stores {
		if st.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("the blob pool holds a symbolic link (%s); symlinks are not followed", filepath.Join(root, st.Name()))
		}
		if !st.IsDir() || !validStore(st.Name()) {
			continue
		}
		base := filepath.Join(root, st.Name(), "sha256")
		aas, err := readDirSorted(base)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, aa := range aas {
			if err := ctx.Err(); err != nil {
				return err
			}
			if aa.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("the blob pool holds a symbolic link (%s); symlinks are not followed", filepath.Join(base, aa.Name()))
			}
			if !aa.IsDir() {
				continue
			}
			bbs, err := readDirSorted(filepath.Join(base, aa.Name()))
			if err != nil {
				return err
			}
			for _, bb := range bbs {
				dir := filepath.Join(base, aa.Name(), bb.Name())
				if bb.Type()&fs.ModeSymlink != 0 {
					return fmt.Errorf("the blob pool holds a symbolic link (%s); symlinks are not followed", dir)
				}
				if !bb.IsDir() {
					continue
				}
				files, err := readDirSorted(dir)
				if err != nil {
					return err
				}
				for _, f := range files {
					name := f.Name()
					switch {
					case f.Type()&fs.ModeSymlink != 0:
						return fmt.Errorf("the blob pool holds a symbolic link (%s); symlinks are not followed", filepath.Join(dir, name))
					case !f.Type().IsRegular():
					case strings.HasPrefix(name, tmpPrefix):
						if onTmp != nil {
							if err := onTmp(filepath.Join(dir, name)); err != nil {
								return err
							}
						}
					case validSHA(name) && name[:2] == aa.Name() && name[2:4] == bb.Name():
						if err := yield(st.Name(), name); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// readDirSorted reads a directory's entries sorted by name (os.ReadDir sorts).
func readDirSorted(dir string) ([]fs.DirEntry, error) { return os.ReadDir(dir) }

// sweep deletes the pool blobs that no backup references, and temp files a
// crashed copy left. The caller holds mu for writing. When it is skipped,
// nothing is deleted and the reason is returned (and logged).
func (s *Service) sweep(ctx context.Context, log Logger) (int64, int64, string) {
	n, b, err := s.sweepErr(ctx)
	if err != nil {
		reason := "sweep skipped: " + err.Error()
		log.Warnf("%s (nothing was deleted)", reason)
		s.log().Warn("backup pool "+reason+" (nothing was deleted)", "backupDir", s.Dir)
		return 0, 0, reason
	}
	if n > 0 {
		log.Infof("removed %d pool blobs (%d bytes) that no backup references", n, b)
	}
	return n, b, ""
}

func (s *Service) sweepErr(ctx context.Context) (int64, int64, error) {
	root := filepath.Join(s.Dir, poolDir)
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	} else if err != nil {
		return 0, 0, err
	}
	refPaths, err := s.collectRefs(ctx)
	if err != nil {
		return 0, 0, err
	}
	run := func(onDead func(store, sha string) error, onTmp func(string) error) (mergeStats, error) {
		srcs := make([]refSource, 0, len(refPaths))
		var files []*os.File
		defer func() {
			for _, f := range files {
				f.Close()
			}
		}()
		for _, p := range refPaths {
			f, err := os.Open(p)
			if err != nil {
				return mergeStats{}, err
			}
			files = append(files, f)
			srcs = append(srcs, refSource{name: filepath.Base(filepath.Dir(p)), r: f})
		}
		pool := func(yield func(store, sha string) error) error { return walkPool(ctx, root, yield, onTmp) }
		return mergeRefs(ctx, pool, srcs, onDead)
	}

	// Pass 1 counts; the backstop checks happen before anything is deleted.
	var dead int64
	st, err := run(func(string, string) error { dead++; return nil }, nil)
	if err != nil {
		return 0, 0, err
	}
	for _, src := range st.sources {
		if src.lines > 0 && src.matched == 0 {
			return 0, 0, fmt.Errorf("none of the %d blobs backup %s references is in the pool (a reading error?)", src.lines, src.name)
		}
	}
	if len(refPaths) > 0 && st.pool > 0 && st.live == 0 {
		return 0, 0, fmt.Errorf("no backup references any of the %d pool blobs (a reading error?)", st.pool)
	}
	if dead == 0 {
		// Still clear temp files.
		_, err := run(func(string, string) error { return nil }, func(p string) error { return os.Remove(p) })
		return 0, 0, err
	}

	// Pass 2 deletes.
	var n, bytes int64
	_, err = run(func(store, sha string) error {
		p := s.poolPath(store, sha)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		n++
		bytes += fi.Size()
		return nil
	}, func(p string) error { return os.Remove(p) })
	return n, bytes, err
}

// collectRefs returns the refs file of every backup in the root, generating
// it for a backup with a meta.db and none. Directories with neither a meta.db
// nor a manifest are skipped without reading them.
func (s *Service) collectRefs(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("the backup root holds a symbolic link (%s); symlinks are not followed", e.Name())
		}
		if !e.IsDir() || e.Name() == poolDir {
			continue
		}
		dir := filepath.Join(s.Dir, e.Name())
		hasMeta, err := exists(filepath.Join(dir, metaFile))
		if err != nil {
			return nil, err
		}
		hasManifest, err := exists(filepath.Join(dir, manifestFile))
		if err != nil {
			return nil, err
		}
		if !hasMeta && !hasManifest {
			continue
		}
		var m Manifest
		if hasManifest {
			if err := readJSON(filepath.Join(dir, manifestFile), &m); err != nil {
				return nil, fmt.Errorf("manifest of %s: %w", e.Name(), err)
			}
		}
		refs := filepath.Join(dir, refsFile)
		has, err := exists(refs)
		if err != nil {
			return nil, err
		}
		if m.RefsSHA256 != "" {
			// The manifest vouches for the refs file: a damaged one could
			// make the sweep delete blobs this backup needs.
			if !has {
				return nil, fmt.Errorf("backup %s: the refs file is missing but its manifest records one", e.Name())
			}
			if err := VerifyRefsFile(refs, m.RefsSHA256, m.RefsLines); err != nil {
				return nil, fmt.Errorf("backup %s: %w", e.Name(), err)
			}
		} else if hasManifest {
			s.log().Warn("backup has no refs checksum in its manifest (legacy backup); its refs file is trusted as is", "backup", e.Name())
		}
		if !has {
			if !hasMeta {
				return nil, fmt.Errorf("backup %s has a manifest but neither meta.db nor refs", e.Name())
			}
			if err := generateRefs(ctx, dir); err != nil {
				return nil, fmt.Errorf("generate the refs of %s: %w", e.Name(), err)
			}
		}
		out = append(out, refs)
	}
	return out, nil
}

// exists reports whether a regular file is at path; an error other than
// not-found (or a non-regular file) is returned.
func exists(path string) (bool, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.Mode().IsRegular():
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	return true, nil
}

// VerifyRefsFile checks that the refs file at path has the sha256 and line
// count the manifest recorded (streamed, constant memory).
func VerifyRefsFile(path, wantSHA string, wantLines int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("refs file: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	var lines int64
	buf := make([]byte, refsBufSize)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			lines += int64(bytes.Count(buf[:n], []byte{'\n'}))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("refs file: %w", err)
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("the refs file does not match the manifest (sha256 %s, recorded %s)", got, wantSHA)
	}
	if lines != wantLines {
		return fmt.Errorf("the refs file does not match the manifest (%d lines, recorded %d)", lines, wantLines)
	}
	return nil
}
