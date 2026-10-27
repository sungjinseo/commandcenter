package backup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/datalock"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// BeforeRestorePrefix names the directory a forced restore moves the old data
// into: <dataDir>/.before-restore-<UTC time>/. It is inside the data
// directory because a container's data directory is a volume mount, so a
// sibling cannot be created. The restored app never uses it: a blob store
// path in the restored meta.db that points into it is refused, and the
// default store lives at <dataDir>/blobs. Delete it by hand when the restore
// is confirmed good.
const BeforeRestorePrefix = ".before-restore-"

// StagingPrefix names the directories a restore stages into before swapping
// them into place (<dataDir>/.restore-staging-<time>, and the same inside an
// absolute blob store root). They are removed on success and on failure.
const StagingPrefix = ".restore-staging-"

// RestoreOptions are the inputs of Restore.
type RestoreOptions struct {
	From                string // one backup directory: <backupDir>/<name>
	Data                string // the data directory to restore into
	Force               bool   // allow a non-empty data directory (the old contents are moved aside)
	AllowMissing        bool   // succeed although some referenced blobs are missing or corrupt in the backup
	AllowAbsoluteStores bool   // write blob stores that have absolute paths to those paths
	Log                 Logger
}

// RestoreResult summarises a restore.
type RestoreResult struct {
	Name        string
	Blobs       int64 // distinct (store, sha256) pairs the backup references
	Bytes       int64 // their total size
	CopiedBlobs int64
	Missing     int64
	MissingList []string // the first few, "<store>/<sha>: why"
	Stores      map[string]string
	MovedTo     string // where the old contents went (--force on a non-empty directory)
	LegacyRefs  bool   // the manifest has no refs checksum
}

// Restore rebuilds a data directory from a backup, offline. It verifies the
// manifest, the meta.db checksum and the refs file before touching anything,
// then takes the data directory locks (refusing while a server holds them),
// moves old contents aside when allowed, copies the blobs into the blob
// stores the restored meta.db defines (streamed, sha256 verified, temp file
// and rename) and installs meta.db last, so a failed restore leaves no
// database behind.
func Restore(ctx context.Context, opt RestoreOptions) (res RestoreResult, err error) {
	log := opt.Log
	if log == nil {
		log = discard{}
	}
	from, err := filepath.Abs(opt.From)
	if err != nil {
		return res, err
	}
	data, err := filepath.Abs(opt.Data)
	if err != nil {
		return res, err
	}
	if inside(from, data) || inside(data, from) {
		return res, fmt.Errorf("the backup (%s) and the data directory (%s) must not contain each other", from, data)
	}

	// 1. The backup: manifest, meta.db checksum, refs.
	var m Manifest
	if err := readJSON(filepath.Join(from, manifestFile), &m); err != nil {
		return res, fmt.Errorf("%s is not a complete backup (manifest.json: %w)", from, err)
	}
	if m.Version != ManifestVersion || m.Product != "commandcenter" {
		return res, fmt.Errorf("unsupported manifest (version %d, product %q)", m.Version, m.Product)
	}
	res.Name = m.Name
	metaPath := filepath.Join(from, metaFile)
	if ok, err := exists(metaPath); err != nil || !ok {
		return res, errors.New("the backup has no meta.db")
	}
	if m.MetaSHA256 == "" {
		return res, errors.New("the manifest records no meta.db checksum; refusing to restore")
	}
	buf := make([]byte, copyBufSize)
	pool := filepath.Join(filepath.Dir(from), poolDir)
	if got, err := hashFile(metaPath, buf); err != nil {
		return res, fmt.Errorf("hash meta.db: %w", err)
	} else if got != m.MetaSHA256 {
		return res, fmt.Errorf("meta.db does not match the manifest (sha256 %s, recorded %s): the backup is corrupt", got, m.MetaSHA256)
	}
	refsPath := filepath.Join(from, refsFile)
	hasRefs, err := exists(refsPath)
	if err != nil {
		return res, err
	}
	if m.RefsSHA256 != "" {
		if !hasRefs {
			return res, errors.New("the refs file is missing but the manifest records one: the backup is corrupt")
		}
		if err := VerifyRefsFile(refsPath, m.RefsSHA256, m.RefsLines); err != nil {
			return res, fmt.Errorf("%w: the backup is corrupt", err)
		}
	} else {
		res.LegacyRefs = true
		log.Warnf("the manifest has no refs checksum (a backup from an older version); the refs list is trusted as is")
	}

	snap, err := meta.OpenReadOnly(metaPath)
	if err != nil {
		return res, fmt.Errorf("open meta.db: %w", err)
	}
	defer snap.Close()
	rows, err := snap.ListBlobStores(ctx)
	if err != nil {
		return res, fmt.Errorf("read blob_stores: %w", err)
	}
	roots, err := restoreRoots(data, rows)
	if err != nil {
		return res, err
	}
	res.Stores = roots
	each := func(fn func(store, sha string) error) error {
		if hasRefs {
			return eachRefsLine(refsPath, fn)
		}
		stores, err := StoreMap(ctx, snap)
		if err != nil {
			return err
		}
		return eachRef(ctx, snap, stores, func(store, sha string, _ int64) error { return fn(store, sha) })
	}

	absStore := map[string]bool{}
	for _, bs := range rows {
		if bs.Name != repo.DefaultStore && filepath.IsAbs(bs.Path) {
			absStore[bs.Name] = true
		}
	}

	// 2. Preflight, still read-only: every store is defined and every blob is
	// in the pool, so a doomed restore does not touch anything.
	var missing []string
	var lines int64
	used := map[string]bool{}
	need := map[string]int64{} // store root -> bytes to stage
	err = each(func(store, sha string) error {
		lines++
		if _, ok := roots[store]; !ok {
			return fmt.Errorf("the backup references blob store %q, which its meta.db does not define", store)
		}
		used[store] = true
		if fi, err := os.Lstat(poolFile(pool, store, sha)); err != nil || !fi.Mode().IsRegular() {
			if len(missing) < maxProblems {
				missing = append(missing, store+"/"+sha+": missing from the backup's pool")
			}
			res.Missing++
		} else {
			need[roots[store]] += fi.Size()
		}
		return ctx.Err()
	})
	if err != nil {
		return res, err
	}
	if !opt.AllowMissing {
		if res.Missing > 0 || m.MissingBlobs > 0 {
			n := max(res.Missing, m.MissingBlobs)
			return res, fmt.Errorf("%d blobs the backup references are missing from it (it is incomplete; e.g. %s); nothing was changed. Use --allow-missing to restore the rest",
				n, firstOf(missing))
		}
		if lines != m.Blobs {
			return res, fmt.Errorf("the refs list has %d blobs but the manifest says %d: the backup is inconsistent; nothing was changed (--allow-missing overrides)", lines, m.Blobs)
		}
	}
	res.Missing = 0 // recounted while copying
	res.MissingList = nil

	// Absolute blob store paths: safe and explicitly allowed.
	var abs []string
	for st := range used {
		if absStore[st] {
			abs = append(abs, st)
		}
	}
	sort.Strings(abs)
	backupRoot := filepath.Dir(from)
	for _, st := range abs {
		if err := checkAbsolute(roots[st], data, backupRoot); err != nil {
			return res, fmt.Errorf("blob store %q: %w", st, err)
		}
	}
	if len(abs) > 0 && !opt.AllowAbsoluteStores {
		var l []string
		for _, st := range abs {
			l = append(l, fmt.Sprintf("%s=%s", st, roots[st]))
		}
		return res, fmt.Errorf("the backup's blob stores use absolute paths (%s); they are written to those paths as is. Make sure they are mounted and pass --allow-absolute-stores", strings.Join(l, ", "))
	}

	// 3. The data directory: locks, emptiness.
	if err := os.MkdirAll(data, 0o755); err != nil {
		return res, err
	}
	for _, name := range []string{datalock.FileName, datalock.ServerFileName} {
		l, err := datalock.TryAcquire(filepath.Join(data, name))
		if errors.Is(err, datalock.ErrHeld) {
			return res, fmt.Errorf("the data directory is in use (a server or another task holds %s): stop the server and retry (%v). A server from a binary older than the restore command does not take .server.lock; make sure none runs", name, err)
		}
		if err != nil {
			return res, fmt.Errorf("data directory lock: %w", err)
		}
		defer l.Release()
	}
	entries, err := os.ReadDir(data)
	if err != nil {
		return res, err
	}
	var old []string
	for _, e := range entries {
		switch n := e.Name(); {
		case n == datalock.FileName || n == datalock.ServerFileName,
			strings.HasPrefix(n, BeforeRestorePrefix), strings.HasPrefix(n, StagingPrefix):
		default:
			old = append(old, n)
		}
	}
	if len(old) > 0 && !opt.Force {
		return res, fmt.Errorf("the data directory %s is not empty (%d entries, e.g. %s): use --force to move the old contents to %s<time> inside it; if an earlier restore was interrupted, delete its leftovers or use --force",
			data, len(old), old[0], BeforeRestorePrefix)
	}

	// 4. Stage: every blob and meta.db are copied (sha256 verified) into
	// staging directories; nothing in place is touched until all succeeded.
	// Stores inside the data dir are staged under <data>/.restore-staging-<ts>
	// mirroring their final place; absolute stores in <root>/.restore-staging-<ts>
	// (their own filesystem).
	stamp := time.Now().UTC().Format(dateLayout)
	stagingName := StagingPrefix + stamp
	staging := filepath.Join(data, stagingName)
	stageOf := func(store string) string {
		root := roots[store]
		if absStore[store] {
			return filepath.Join(root, stagingName)
		}
		rel, err := filepath.Rel(data, root)
		if err != nil || rel == "." {
			return staging
		}
		return filepath.Join(staging, rel)
	}
	committed, keep := false, false
	var cleanup []string
	var createdRoots []string
	defer func() {
		if committed || keep {
			return
		}
		for _, d := range cleanup {
			os.RemoveAll(d)
		}
		for _, r := range createdRoots {
			os.Remove(r) // only if still empty
		}
	}()
	cleanup = append(cleanup, staging)
	if err := os.Mkdir(staging, dirMode); err != nil {
		return res, fmt.Errorf("create the staging directory: %w", err)
	}
	for _, st := range abs {
		if _, err := os.Stat(roots[st]); errors.Is(err, fs.ErrNotExist) {
			createdRoots = append(createdRoots, roots[st])
		}
		if err := os.MkdirAll(roots[st], 0o755); err != nil {
			return res, fmt.Errorf("blob store %q: %w", st, err)
		}
		cleanup = append(cleanup, stageOf(st))
		if err := os.Mkdir(stageOf(st), dirMode); err != nil {
			return res, fmt.Errorf("blob store %q: create the staging directory: %w", st, err)
		}
	}
	warned := map[string]bool{}
	for root, n := range need {
		dir := data
		if !inside(root, data) {
			dir = root
		}
		if free, ok := freeBytes(dir); ok && uint64(n) > free && !warned[dir] {
			warned[dir] = true
			log.Warnf("%s has %d bytes free but about %d bytes of blobs are to be restored there", dir, free, n)
		}
	}
	err = each(func(store, sha string) error {
		res.Blobs++
		// Always a fresh verified copy: an existing blob is never trusted by size.
		size, _, err := restoreBlob(poolFile(pool, store, sha), blobFile(stageOf(store), sha), sha, buf)
		if errors.Is(err, errMissing) || errors.Is(err, errCorrupt) {
			if !opt.AllowMissing {
				return fmt.Errorf("blob %s of store %q: %v in the backup", sha, store, err)
			}
			res.Missing++
			if len(res.MissingList) < maxProblems {
				res.MissingList = append(res.MissingList, fmt.Sprintf("%s/%s: %v", store, sha, err))
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("restore blob %s of store %q: %w", sha, store, err)
		}
		res.Bytes += size
		res.CopiedBlobs++
		return ctx.Err()
	})
	if err != nil {
		return res, err
	}
	if !opt.AllowMissing && res.CopiedBlobs != m.Blobs {
		return res, fmt.Errorf("restored %d blobs but the manifest says %d: nothing was changed", res.CopiedBlobs, m.Blobs)
	}
	if _, _, err = copyVerified(metaPath, filepath.Join(staging, metaFile), m.MetaSHA256, buf); err != nil {
		return res, fmt.Errorf("restore meta.db: %w", err)
	}

	// 5. Commit. Absolute stores first (content-addressed blobs, verified,
	// renamed within their own filesystem), then the data dir: old contents
	// aside, staged entries renamed in (meta.db last).
	for _, st := range abs {
		if err := mergeStaged(stageOf(st), roots[st]); err != nil {
			return res, fmt.Errorf("blob store %q: %w (the data directory is untouched)", st, err)
		}
	}
	var aside string
	if len(old) > 0 {
		aside, err = moveAside(data, old, time.Now().UTC())
		if err != nil {
			return res, err
		}
		res.MovedTo = aside
		log.Infof("moved the old contents of %s to %s", data, aside)
	}
	staged, err := os.ReadDir(staging)
	if err != nil {
		return res, err
	}
	names := make([]string, 0, len(staged))
	for _, e := range staged {
		if e.Name() != metaFile {
			names = append(names, e.Name())
		}
	}
	names = append(names, metaFile)
	var swapped []string
	for _, n := range names {
		if err = os.Rename(filepath.Join(staging, n), filepath.Join(data, n)); err != nil {
			err = fmt.Errorf("move %s into place: %w", n, err)
			break
		}
		swapped = append(swapped, n)
	}
	if err != nil {
		var failed []string
		for i := len(swapped) - 1; i >= 0; i-- {
			if e := os.Rename(filepath.Join(data, swapped[i]), filepath.Join(staging, swapped[i])); e != nil {
				failed = append(failed, e.Error())
			}
		}
		if aside != "" {
			for _, n := range old {
				if e := os.Rename(filepath.Join(aside, n), filepath.Join(data, n)); e != nil {
					failed = append(failed, e.Error())
				}
			}
		}
		if len(failed) > 0 {
			keep = true
			return res, fmt.Errorf("%w; rolling back failed (%s). Manual recovery: the old data is in %s, the staged restore in %s", err, strings.Join(failed, "; "), aside, staging)
		}
		if aside != "" {
			os.Remove(aside)
			res.MovedTo = ""
		}
		return res, fmt.Errorf("%w (rolled back; the data directory is as before)", err)
	}
	committed = true
	for _, d := range cleanup {
		os.RemoveAll(d)
	}
	syncDir(data)
	log.Infof("restored %d blobs (%d bytes) and meta.db from backup %s", res.CopiedBlobs, res.Bytes, res.Name)
	return res, nil
}

// mergeStaged renames every staged blob (<stage>/blobs/sha256/aa/bb/<sha>)
// into <root>/blobs/sha256/..., replacing a file already there.
func mergeStaged(stage, root string) error {
	base := filepath.Join(stage, "blobs")
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Rename(p, dst)
	})
}

// reservedComponent reports whether a path has a component the restore (or
// the server) reserves.
func reservedComponent(p string) (string, bool) {
	for _, c := range strings.Split(filepath.ToSlash(filepath.Clean(p)), "/") {
		switch {
		case strings.HasPrefix(c, BeforeRestorePrefix), strings.HasPrefix(c, StagingPrefix),
			c == datalock.FileName, c == datalock.ServerFileName, c == metaFile:
			return c, true
		}
	}
	return "", false
}

// checkAbsolute refuses an absolute blob store root that is, or lies in, the
// data directory or the backup root, or has a reserved path component;
// symbolic links are resolved where the path exists.
func checkAbsolute(root, data, backupRoot string) error {
	if c, bad := reservedComponent(root); bad {
		return fmt.Errorf("the path %s has the reserved component %q", root, c)
	}
	r, d, b := resolve(root), resolve(data), resolve(backupRoot)
	switch {
	case inside(r, d):
		return fmt.Errorf("the path %s is the data directory or inside it", root)
	case inside(r, b):
		return fmt.Errorf("the path %s is the backup directory or inside it", root)
	case inside(d, r):
		return fmt.Errorf("the path %s contains the data directory", root)
	}
	if c, bad := reservedComponent(r); bad {
		return fmt.Errorf("the path %s resolves to %s, which has the reserved component %q", root, r, c)
	}
	return nil
}

// resolve evaluates symlinks of the longest existing prefix of p.
func resolve(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for q := p; ; {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(q)
		if parent == q {
			return p
		}
		rest = filepath.Join(filepath.Base(q), rest)
		q = parent
	}
}

// restoreRoots maps each blob store of the restored meta.db to its directory
// (repo.StoreRoot: default at the data directory, a relative path relative to
// it, an absolute path as is). A relative path must stay inside the data
// directory and have no reserved component; absolute paths are checked by
// checkAbsolute.
func restoreRoots(data string, rows []meta.BlobStore) (map[string]string, error) {
	roots := map[string]string{repo.DefaultStore: data}
	for _, bs := range rows {
		if !validStore(bs.Name) {
			return nil, fmt.Errorf("blob store name %q is not valid", bs.Name)
		}
		if bs.Name != repo.DefaultStore && !filepath.IsAbs(bs.Path) {
			clean := filepath.Clean(filepath.FromSlash(bs.Path))
			first := strings.Split(clean, string(filepath.Separator))[0]
			if clean == "." || clean == ".." || first == ".." || filepath.VolumeName(clean) != "" {
				return nil, fmt.Errorf("blob store %q has the path %q, which leaves the data directory", bs.Name, bs.Path)
			}
			if c, bad := reservedComponent(clean); bad {
				return nil, fmt.Errorf("blob store %q has the reserved path %q (component %q)", bs.Name, bs.Path, c)
			}
		}
		roots[bs.Name] = repo.StoreRoot(data, bs)
	}
	return roots, nil
}

// moveAside moves the named entries of dir into a new
// <dir>/.before-restore-<time> and returns it; on a failure it moves back
// what it moved and, if that fails too, names every directory involved.
func moveAside(dir string, names []string, now time.Time) (string, error) {
	base := filepath.Join(dir, BeforeRestorePrefix+now.Format(dateLayout))
	aside := base
	for i := 2; ; i++ {
		err := os.Mkdir(aside, dirMode)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || i > 100 {
			return "", err
		}
		aside = fmt.Sprintf("%s-%d", base, i)
	}
	var moved []string
	for _, n := range names {
		if err := os.Rename(filepath.Join(dir, n), filepath.Join(aside, n)); err != nil {
			var failed []string
			for _, b := range moved {
				if e := os.Rename(filepath.Join(aside, b), filepath.Join(dir, b)); e != nil {
					failed = append(failed, b)
				}
			}
			if len(failed) > 0 {
				return "", fmt.Errorf("move %s aside: %w; moving back failed for %s: the old data is split between %s and %s", n, err, strings.Join(failed, ", "), dir, aside)
			}
			os.Remove(aside)
			return "", fmt.Errorf("move %s aside: %w", n, err)
		}
		moved = append(moved, n)
	}
	syncDir(dir)
	return aside, nil
}

func poolFile(pool, store, sha string) string {
	return filepath.Join(pool, store, "sha256", sha[:2], sha[2:4], sha)
}

func blobFile(root, sha string) string {
	return filepath.Join(root, "blobs", "sha256", sha[:2], sha[2:4], sha)
}

// restoreBlob copies one pool blob to dst through a temp file, checking the
// sha256 while copying. A dst that already has the right size is kept.
func restoreBlob(src, dst, sha string, buf []byte) (size int64, copied bool, err error) {
	fi, err := os.Lstat(src)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false, errMissing
	}
	if di, err := os.Lstat(dst); err == nil && di.Mode().IsRegular() && di.Size() == fi.Size() {
		return fi.Size(), false, nil
	}
	return copyVerified(src, dst, sha, buf)
}

// copyVerified streams src to dst (temp file in dst's directory, fsync,
// rename), failing with errCorrupt unless the content's sha256 is want.
func copyVerified(src, dst, want string, buf []byte) (int64, bool, error) {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, errMissing
	}
	if err != nil {
		return 0, false, err
	}
	defer in.Close()
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, false, err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return 0, false, err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(tmp, h), noWriterTo{in}, buf)
	if err != nil {
		return 0, false, err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
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
	done = true
	syncDir(dir)
	return n, true, nil
}

// eachRefsLine streams a refs file, checking its order.
func eachRefsLine(path string, fn func(store, sha string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := refStream{name: "the backup", r: bufio.NewReaderSize(f, refsBufSize)}
	for {
		if err := s.advance(); err != nil {
			return err
		}
		if !s.ok {
			return nil
		}
		if err := fn(s.head.store, s.head.sha); err != nil {
			return err
		}
	}
}

// inside reports whether path is dir or below it.
func inside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func firstOf(l []string) string {
	if len(l) == 0 {
		return "none listed"
	}
	return l[0]
}
