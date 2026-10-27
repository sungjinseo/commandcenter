package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

type harness struct {
	s       *Service
	db      *meta.DB
	store   *repo.Store
	dataDir string
	rootB   string
	sink    *repotest.Sink
	now     atomic.Int64
}

// newHarness builds a data dir with the default store and a second store
// "b", the repositories "mvn" (default store), "mvn-b" (store b) and a group
// "grp", and a backup service writing to a fresh backup root.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dataDir := t.TempDir()
	db, err := meta.Open(filepath.Join(dataDir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	def, err := blob.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	rootB := filepath.Join(dataDir, "blobstores", "b")
	bs, err := blob.Open(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SeedBlobStore(ctx, repo.DefaultStore, dataDir); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBlobStore(ctx, meta.BlobStore{Name: "b", Path: "blobstores/b"}); err != nil {
		t.Fatal(err)
	}
	defs := []config.Repository{
		{Name: "mvn", Format: "maven", Type: "hosted", BlobStore: "default"},
		{Name: "mvn-b", Format: "maven", Type: "hosted", BlobStore: "b"},
		{Name: "grp", Format: "maven", Type: "group", Members: []string{"mvn", "mvn-b"}},
	}
	if _, err := db.ImportRepositories(ctx, defs); err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, d := range defs {
		byName[d.Name] = repo.StoreOf(d)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: def, "b": bs}, DB: db, Now: time.Now,
		StoreFor: func(name string) string { return byName[name] }}
	h := &harness{db: db, store: store, dataDir: dataDir, rootB: rootB, sink: &repotest.Sink{}}
	h.now.Store(time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC).Unix())
	h.s = &Service{DB: db, Store: store, Dir: filepath.Join(t.TempDir(), "backup"), Version: "test",
		Events: h.sink, Now: func() time.Time { return time.Unix(h.now.Add(1), 0).UTC() }}
	return h
}

func (h *harness) put(t *testing.T, repoName, path, body string) meta.Asset {
	t.Helper()
	a, err := h.store.Put(context.Background(), repoName, path, "application/octet-stream", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (h *harness) run(t *testing.T, p Params) Manifest {
	t.Helper()
	m, err := h.s.Run(context.Background(), p, "admin", nil)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return m
}

func (h *harness) poolPath(store, sha string) string {
	return filepath.Join(h.s.Dir, "blobs", store, "sha256", sha[:2], sha[2:4], sha)
}

func (h *harness) inPool(store, sha string) bool {
	_, err := os.Stat(h.poolPath(store, sha))
	return err == nil
}

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestBackupCopiesReferencedBlobs(t *testing.T) {
	h := newHarness(t)
	a1 := h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	a2 := h.put(t, "mvn-b", "b/1/b-1.jar", "beta")
	h.put(t, "mvn-b", "b/1/b-1.jar.copy", "beta")        // same blob twice in one store
	a3 := h.put(t, "mvn", "shared/1/s-1.jar", "beta")    // same content in the default store too
	g := h.put(t, "grp", "a/maven-metadata.xml", "meta") // group writes go to the default store
	// A blob no asset references is not backed up.
	orphan, err := h.store.Blobs[repo.DefaultStore].Write(strings.NewReader("orphan"))
	if err != nil {
		t.Fatal(err)
	}

	m := h.run(t, Params{Subdir: "first"})
	if m.Version != 1 || m.Product != "commandcenter" || m.AppVersion != "test" || m.Name != "first" {
		t.Fatalf("manifest header: %+v", m)
	}
	if m.Repositories != 3 || m.Assets != 5 || m.Blobs != 4 || m.CopiedBlobs != 4 || m.MissingBlobs != 0 {
		t.Fatalf("manifest counts: %+v", m)
	}
	if want := int64(len("alpha") + 2*len("beta") + len("meta")); m.Bytes != want || m.CopiedBytes != want {
		t.Fatalf("bytes %d copied %d, want %d", m.Bytes, m.CopiedBytes, want)
	}
	if m.BlobStores["default"] != h.dataDir || m.BlobStores["b"] != "blobstores/b" {
		t.Fatalf("blob stores: %v", m.BlobStores)
	}
	for _, c := range []struct{ store, sha string }{
		{"default", a1.BlobSHA256}, {"b", a2.BlobSHA256}, {"default", a3.BlobSHA256}, {"default", g.BlobSHA256},
	} {
		if !h.inPool(c.store, c.sha) {
			t.Fatalf("blob %s of store %s is not in the pool", c.sha, c.store)
		}
	}
	if h.inPool("default", orphan.SHA256) {
		t.Fatal("an unreferenced blob was backed up")
	}
	if h.inPool("b", a1.BlobSHA256) {
		t.Fatal("a blob was copied into the wrong store's pool")
	}
	// The snapshot is a standalone database with the assets.
	snap, err := meta.OpenReadOnly(filepath.Join(h.s.Dir, "first", "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if n, err := snap.CountAssets(context.Background()); err != nil || n != 5 {
		t.Fatalf("snapshot assets %d %v", n, err)
	}
	for _, f := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(h.s.Dir, "first", "meta.db"+f)); err == nil {
			t.Fatalf("the snapshot left a meta.db%s file", f)
		}
	}
	// The manifest on disk matches, and the backup is listed.
	var disk Manifest
	b, err := os.ReadFile(filepath.Join(h.s.Dir, "first", "manifest.json"))
	if err != nil || json.Unmarshal(b, &disk) != nil || disk.Blobs != 4 {
		t.Fatalf("manifest.json: %s %v", b, err)
	}
	list, err := h.s.List()
	if err != nil || len(list) != 1 || list[0].Name != "first" || list[0].MetaBytes == 0 {
		t.Fatalf("list: %+v %v", list, err)
	}
	// The backup event names the backup.
	evs := h.sink.Events()
	if len(evs) != 1 || evs[0].Kind != "backup" || evs[0].Actor != "admin" || evs[0].Subject != "first" {
		t.Fatalf("events: %+v", evs)
	}
}

func TestDefaultSubdirIsTheDate(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a", "x")
	m := h.run(t, Params{})
	if !regexpDate(m.Name) {
		t.Fatalf("default name %q", m.Name)
	}
	if _, err := os.Stat(filepath.Join(h.s.Dir, m.Name, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}

func regexpDate(s string) bool {
	_, err := time.Parse("20060102-150405", s)
	return err == nil
}

func TestIncrementalCopiesOnlyNewBlobs(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a", "one")
	h.put(t, "mvn-b", "b", "two")
	first := h.run(t, Params{Subdir: "b1"})
	if first.CopiedBlobs != 2 {
		t.Fatalf("first: %+v", first)
	}
	h.put(t, "mvn", "c", "three")
	second := h.run(t, Params{Subdir: "b2"})
	if second.Blobs != 3 || second.CopiedBlobs != 1 || second.CopiedBytes != int64(len("three")) {
		t.Fatalf("second: %+v", second)
	}
	// A pool blob of the wrong size (a torn copy) is copied again.
	sha := shaOf("one")
	if err := os.WriteFile(h.poolPath("default", sha), []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	third := h.run(t, Params{Subdir: "b3"})
	if third.CopiedBlobs != 1 {
		t.Fatalf("third: %+v", third)
	}
	if b, _ := os.ReadFile(h.poolPath("default", sha)); string(b) != "one" {
		t.Fatalf("pool blob not repaired: %q", b)
	}
	// No temp files are left behind.
	filepath.WalkDir(h.s.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".tmp") {
			t.Errorf("temp file left: %s", p)
		}
		return nil
	})
}

func TestRetentionPrunesManifestsAndOrphanBlobs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(t, "mvn", "keep", "kept-everywhere")
	gone := h.put(t, "mvn-b", "gone", "only-in-first")
	h.run(t, Params{Subdir: "r1", Keep: 2})
	// The asset is deleted from the live data; only r1 references its blob.
	if err := h.db.DeleteAsset(ctx, "mvn-b", "gone"); err != nil {
		t.Fatal(err)
	}
	h.put(t, "mvn", "second", "second")
	h.run(t, Params{Subdir: "r2", Keep: 2})
	if !h.inPool("b", gone.BlobSHA256) {
		t.Fatal("swept a blob that r1 still references")
	}
	h.put(t, "mvn", "third", "third")
	m := h.run(t, Params{Subdir: "r3", Keep: 2})
	if m.PrunedBackups != 1 || m.SweptBlobs != 1 {
		t.Fatalf("pruned %d swept %d", m.PrunedBackups, m.SweptBlobs)
	}
	list, err := h.s.List()
	if err != nil || len(list) != 2 || list[0].Name != "r3" || list[1].Name != "r2" {
		t.Fatalf("list after retention: %+v %v", list, err)
	}
	if _, err := os.Stat(filepath.Join(h.s.Dir, "r1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("r1 still exists: %v", err)
	}
	if h.inPool("b", gone.BlobSHA256) {
		t.Fatal("the orphan pool blob was not swept")
	}
	for _, s := range []string{"kept-everywhere", "second", "third"} {
		if !h.inPool("default", shaOf(s)) {
			t.Fatalf("swept the referenced blob %q", s)
		}
	}
}

func TestVerifyFindsMissingAndCorruptBlobs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.put(t, "mvn", "a", "apple")
	b := h.put(t, "mvn-b", "b", "banana")
	c := h.put(t, "mvn", "c", "cherry")
	h.run(t, Params{Subdir: "v"})

	res, err := h.s.Verify(ctx, "v", true)
	if err != nil || !res.OK || res.Blobs != 3 || res.Missing != 0 || res.Corrupt != 0 {
		t.Fatalf("clean verify: %+v %v", res, err)
	}
	// Missing.
	if err := os.Remove(h.poolPath("default", a.BlobSHA256)); err != nil {
		t.Fatal(err)
	}
	// Same size, other bytes: only a deep verify notices.
	if err := os.WriteFile(h.poolPath("b", b.BlobSHA256), []byte("BANANA"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = h.s.Verify(ctx, "v", false)
	if err != nil || res.OK || res.Missing != 1 || res.Corrupt != 0 || len(res.Problems) != 1 {
		t.Fatalf("standard verify: %+v %v", res, err)
	}
	res, err = h.s.Verify(ctx, "v", true)
	if err != nil || res.OK || res.Missing != 1 || res.Corrupt != 1 || !res.Deep {
		t.Fatalf("deep verify: %+v %v", res, err)
	}
	// A truncated blob is found by the size check.
	if err := os.WriteFile(h.poolPath("default", c.BlobSHA256), []byte("ch"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = h.s.Verify(ctx, "v", false)
	if err != nil || res.Missing != 1 || res.Corrupt != 1 {
		t.Fatalf("size check: %+v %v", res, err)
	}
	// The last result is kept with the backup and listed.
	list, err := h.s.List()
	if err != nil || len(list) != 1 || list[0].Verified == nil || list[0].Verified.OK || list[0].Verified.Corrupt != 1 {
		t.Fatalf("listed verify state: %+v %v", list, err)
	}
	if _, err := h.s.Verify(ctx, "nope", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown backup: %v", err)
	}
	if _, err := h.s.Verify(ctx, "../v", false); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("unsafe name: %v", err)
	}
}

func TestVerifyParamAfterBackup(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a", "apple")
	m := h.run(t, Params{Subdir: "x", Verify: true, Deep: true})
	if m.Verified == nil || !m.Verified.OK || !m.Verified.Deep {
		t.Fatalf("verify after backup: %+v", m.Verified)
	}
}

func TestSubdirSafety(t *testing.T) {
	for _, ok := range []string{"a", "20261026-030000", "nightly.1", "A_b-c.d", "..a", "a.."} {
		if err := ValidSubdir(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x", `a\b`, "/abs", "a b", "blobs", "é", "a\x00", strings.Repeat("a", 101), "c:"} {
		if err := ValidSubdir(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	h := newHarness(t)
	h.put(t, "mvn", "a", "x")
	if _, err := h.s.Run(context.Background(), Params{Subdir: "../escape"}, "admin", nil); err == nil {
		t.Fatal("an unsafe subdir was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.s.Dir), "escape")); err == nil {
		t.Fatal("wrote outside the backup root")
	}
	h.run(t, Params{Subdir: "dup"})
	if _, err := h.s.Run(context.Background(), Params{Subdir: "dup"}, "admin", nil); !errors.Is(err, ErrExists) {
		t.Fatalf("existing subdir: %v", err)
	}
	if _, err := h.s.Delete(context.Background(), "../dup"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("delete with unsafe name: %v", err)
	}
}

func TestMissingSourceBlobFailsTheBackup(t *testing.T) {
	h := newHarness(t)
	a := h.put(t, "mvn", "a", "here")
	lost := h.put(t, "mvn", "b", "lost")
	if err := h.store.Blobs[repo.DefaultStore].Remove(lost.BlobSHA256); err != nil {
		t.Fatal(err)
	}
	m, err := h.s.Run(context.Background(), Params{Subdir: "m"}, "admin", nil)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("want a missing-blob error, got %v", err)
	}
	if m.MissingBlobs != 1 || m.CopiedBlobs != 1 || !h.inPool("default", a.BlobSHA256) {
		t.Fatalf("manifest: %+v", m)
	}
}

func TestDeleteRemovesBackupAndSweeps(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(t, "mvn", "a", "shared")
	only := h.put(t, "mvn", "b", "only-d1")
	h.run(t, Params{Subdir: "d1"})
	if err := h.db.DeleteAsset(ctx, "mvn", "b"); err != nil {
		t.Fatal(err)
	}
	h.run(t, Params{Subdir: "d2"})
	if _, err := h.s.Delete(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.s.Dir, "d1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("d1 still exists")
	}
	if h.inPool("default", only.BlobSHA256) || !h.inPool("default", shaOf("shared")) {
		t.Fatal("sweep after delete is wrong")
	}
	if _, err := h.s.Delete(ctx, "d1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := h.s.Delete(ctx, "blobs"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting the pool: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.s.Dir, "blobs")); err != nil {
		t.Fatal("the pool is gone")
	}
}

// While a backup runs, verify and delete answer ErrBusy instead of racing
// the pool.
func TestBusyWhileBackupRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(t, "mvn", "a", "x")
	h.run(t, Params{Subdir: "old"})
	entered, release := make(chan struct{}), make(chan struct{})
	h.s.AfterSnapshot = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { _, err := h.s.Run(ctx, Params{Subdir: "new"}, "admin", nil); done <- err }()
	<-entered
	if _, err := h.s.Verify(ctx, "old", false); !errors.Is(err, ErrBusy) {
		t.Errorf("verify during backup: %v", err)
	}
	if _, err := h.s.Delete(ctx, "old"); !errors.Is(err, ErrBusy) {
		t.Errorf("delete during backup: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Uploads keep arriving while the backup runs: the snapshot is consistent and
// every blob it references is in the pool.
func TestConsistentUnderConcurrentUploads(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := range 50 {
		h.put(t, []string{"mvn", "mvn-b"}[i%2], fmt.Sprintf("pre/%d", i), fmt.Sprintf("pre-%d", i))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var uploaded atomic.Int64
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				body := bytes.Repeat([]byte(fmt.Sprintf("w%d-%d;", w, i)), 50)
				if _, err := h.store.Put(ctx, []string{"mvn", "mvn-b"}[w%2], fmt.Sprintf("live/%d/%d", w, i), "", bytes.NewReader(body)); err != nil {
					t.Error(err)
					return
				}
				uploaded.Add(1)
			}
		}()
	}
	for uploaded.Load() < 20 {
		time.Sleep(time.Millisecond)
	}
	m, err := h.s.Run(ctx, Params{Subdir: "live"}, "admin", nil)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if m.Assets < 70 {
		t.Fatalf("snapshot has %d assets", m.Assets)
	}
	res, err := h.s.Verify(ctx, "live", true)
	if err != nil || !res.OK || res.Blobs != m.Blobs {
		t.Fatalf("verify: %+v %v", res, err)
	}
	if uploaded.Load() <= 20 {
		t.Log("uploads did not overlap the backup much")
	}
}

// Asset rows deleted after the snapshot (cleanup may still remove rows while
// a backup runs) do not matter: the snapshot's blobs are still copied.
func TestRowsDeletedAfterSnapshot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.put(t, "mvn", "a", "alpha")
	b := h.put(t, "mvn-b", "b", "beta")
	h.s.AfterSnapshot = func() {
		for _, x := range []struct{ repo, path string }{{"mvn", "a"}, {"mvn-b", "b"}} {
			if err := h.db.DeleteAsset(ctx, x.repo, x.path); err != nil {
				t.Error(err)
			}
		}
	}
	m := h.run(t, Params{Subdir: "rows"})
	if m.Blobs != 2 || m.CopiedBlobs != 2 || !h.inPool("default", a.BlobSHA256) || !h.inPool("b", b.BlobSHA256) {
		t.Fatalf("manifest %+v", m)
	}
	h.s.AfterSnapshot = nil
	if res, err := h.s.Verify(ctx, "rows", true); err != nil || !res.OK || res.Blobs != 2 {
		t.Fatalf("verify: %+v %v", res, err)
	}
}

func TestPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	h := newHarness(t)
	a := h.put(t, "mvn", "a", "alpha")
	h.run(t, Params{Subdir: "p"})
	for path, want := range map[string]os.FileMode{
		h.s.Dir: 0o700, filepath.Join(h.s.Dir, "p"): 0o700, filepath.Join(h.s.Dir, "blobs"): 0o700,
		filepath.Dir(h.poolPath("default", a.BlobSHA256)): 0o700,
		filepath.Join(h.s.Dir, "p", "meta.db"):            0o600,
		filepath.Join(h.s.Dir, "p", "manifest.json"):      0o600,
		h.poolPath("default", a.BlobSHA256):               0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
}

// A deep verify that finds a corrupt pool blob removes it, so the next
// backup copies it again and the backup verifies clean.
func TestDeepVerifyRepairs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.put(t, "mvn", "a", "apple")
	h.run(t, Params{Subdir: "v1"})
	if err := os.WriteFile(h.poolPath("default", a.BlobSHA256), []byte("APPLE"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := h.s.VerifyAndRepair(ctx, "", "admin", nil) // "" is the newest backup
	if err != nil || res.OK || res.Name != "v1" || res.Corrupt != 1 || res.Repaired != 1 ||
		len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "removed from the pool") {
		t.Fatalf("repairing verify: %+v %v", res, err)
	}
	if h.inPool("default", a.BlobSHA256) {
		t.Fatal("the corrupt pool blob was not removed")
	}
	if evs := h.sink.Events(); evs[len(evs)-1].Kind != "backup.verify" || !strings.Contains(evs[len(evs)-1].Detail, "repaired=1") {
		t.Fatalf("events: %s", h.sink)
	}
	m := h.run(t, Params{Subdir: "v2"})
	if m.CopiedBlobs != 1 {
		t.Fatalf("the next backup did not copy it again: %+v", m)
	}
	if res, err := h.s.Verify(ctx, "v1", true); err != nil || !res.OK {
		t.Fatalf("verify after the repair: %+v %v", res, err)
	}
}

// writeOrphan puts a pool blob that no backup references.
func (h *harness) writeOrphan(t *testing.T) string {
	t.Helper()
	sha := shaOf("orphan-in-pool")
	p := h.poolPath("default", sha)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("orphan-in-pool"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestSweepAbortsOnUnreadableManifest(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a", "alpha")
	h.run(t, Params{Subdir: "s1"})
	orphan := h.writeOrphan(t)
	manifest := filepath.Join(h.s.Dir, "s1", "manifest.json")
	good, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := h.run(t, Params{Subdir: "s2"}); m.SweptBlobs != 0 || !h.inPool("default", orphan) {
		t.Fatalf("swept with an unreadable manifest: %+v", m)
	}
	// Repaired, the next sweep removes the orphan.
	if err := os.WriteFile(manifest, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if m := h.run(t, Params{Subdir: "s3"}); m.SweptBlobs != 1 || h.inPool("default", orphan) {
		t.Fatalf("control sweep: %+v", m)
	}
}

// A directory with a meta.db and no manifest still protects its blobs.
func TestSweepKeepsBackupWithoutManifest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	only := h.put(t, "mvn", "a", "only-in-n1")
	h.run(t, Params{Subdir: "n1"})
	if err := h.db.DeleteAsset(ctx, "mvn", "a"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.s.Dir, "n1", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	h.put(t, "mvn", "b", "other")
	h.run(t, Params{Subdir: "n2"})
	if !h.inPool("default", only.BlobSHA256) {
		t.Fatal("swept a blob of a backup whose manifest is missing")
	}
}

func TestSweepRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	h := newHarness(t)
	h.put(t, "mvn", "a", "alpha")
	h.run(t, Params{Subdir: "l1"})
	orphan := h.writeOrphan(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(h.s.Dir, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if m := h.run(t, Params{Subdir: "l2"}); m.SweptBlobs != 0 || !h.inPool("default", orphan) {
		t.Fatalf("swept with a symlink in the root: %+v", m)
	}
}

// Retention counts only complete backups towards keep and reports what it
// pruned in a backup.prune event.
func TestRetentionCountsCompleteBackups(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(t, "mvn", "a", "alpha")
	h.run(t, Params{Subdir: "c1", Keep: 2})
	lost := h.put(t, "mvn", "b", "lost-for-a-while")
	if err := h.store.Blobs[repo.DefaultStore].Remove(lost.BlobSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Run(ctx, Params{Subdir: "i1", Keep: 2}, "admin", nil); err == nil {
		t.Fatal("the incomplete backup did not fail")
	}
	h.put(t, "mvn", "b", "lost-for-a-while") // the blob is back
	if m := h.run(t, Params{Subdir: "c2", Keep: 2}); m.PrunedBackups != 0 {
		t.Fatalf("c1 was pruned although only one other complete backup exists: %+v", m)
	}
	h.sink.Reset()
	if m := h.run(t, Params{Subdir: "c3", Keep: 2}); m.PrunedBackups != 2 {
		t.Fatalf("pruned %d, want i1 and c1", m.PrunedBackups)
	}
	list, _ := h.s.List()
	if len(list) != 2 || list[0].Name != "c3" || list[1].Name != "c2" {
		t.Fatalf("list: %+v", list)
	}
	var prune *repotest.Event
	for _, e := range h.sink.Events() {
		if e.Kind == "backup.prune" {
			prune = &e
		}
	}
	if prune == nil || prune.Actor != "admin" || prune.Subject != "i1,c1" {
		t.Fatalf("prune event: %s", h.sink)
	}
	if _, err := (Params{Keep: 101}).Normalize(); err == nil {
		t.Fatal("keep 101 accepted")
	}
}

// The manifest records the repository-to-store map and the snapshot's
// sha256, which verify checks.
func TestManifestStoresAndMetaHash(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(t, "mvn", "a", "alpha")
	m := h.run(t, Params{Subdir: "mh"})
	if m.RepositoryStores["mvn"] != "default" || m.RepositoryStores["mvn-b"] != "b" || m.RepositoryStores["grp"] != "default" || len(m.MetaSHA256) != 64 {
		t.Fatalf("manifest: %+v", m)
	}
	f, err := os.OpenFile(filepath.Join(h.s.Dir, "mh", "meta.db"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("tamper"))
	f.Close()
	if res, err := h.s.Verify(ctx, "mh", false); err != nil || res.OK || !strings.Contains(strings.Join(res.Problems, ";"), "meta.db has sha256") {
		t.Fatalf("verify of a changed meta.db: %+v %v", res, err)
	}
}

// The sweep trusts no file metadata: pool files whose mtimes are all the
// same, or nonsense, are kept exactly when a backup references them.
func TestSweepIgnoresMtimes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(t, "mvn", "a", "shared")
	h.put(t, "mvn-b", "b", "in-store-b")
	only := h.put(t, "mvn", "c", "only-in-m1")
	h.run(t, Params{Subdir: "m1"})
	if err := h.db.DeleteAsset(ctx, "mvn", "c"); err != nil {
		t.Fatal(err)
	}
	h.run(t, Params{Subdir: "m2"})
	orphan := h.writeOrphan(t)
	// A filesystem that rounds or caches times: every pool file carries one
	// of a couple of meaningless timestamps.
	i := 0
	filepath.WalkDir(filepath.Join(h.s.Dir, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			ts := []time.Time{time.Unix(0, 0), time.Unix(315532800, 0), time.Now().Add(time.Hour)}[i%3]
			os.Chtimes(p, ts, ts)
			i++
		}
		return nil
	})
	res, err := h.s.Delete(ctx, "m1")
	if err != nil || res.SweepSkipped != "" || res.SweptBlobs != 2 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if h.inPool("default", only.BlobSHA256) || h.inPool("default", orphan) {
		t.Fatal("unreferenced pool blobs were kept")
	}
	if !h.inPool("default", shaOf("shared")) || !h.inPool("b", shaOf("in-store-b")) {
		t.Fatal("swept a referenced blob")
	}
	if res, err := h.s.Verify(ctx, "m2", true); err != nil || !res.OK {
		t.Fatalf("verify: %+v %v", res, err)
	}
}

// Every backup writes a sorted refs file; one without it (an older backup)
// gets it generated from its meta.db before the sweep.
func TestSweepOldBackupWithoutRefs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	only := h.put(t, "mvn", "a", "only-in-o1")
	h.put(t, "mvn-b", "b", "beta")
	h.run(t, Params{Subdir: "o1"})
	refs := filepath.Join(h.s.Dir, "o1", "refs")
	want, err := os.ReadFile(refs)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(want)), "\n")
	if len(lines) != 2 || lines[0] != "b/"+shaOf("beta") || lines[1] != "default/"+only.BlobSHA256 {
		t.Fatalf("refs: %q", want)
	}
	h.makeLegacy(t, "o1")
	if err := os.Remove(refs); err != nil {
		t.Fatal(err)
	}
	if err := h.db.DeleteAsset(ctx, "mvn", "a"); err != nil {
		t.Fatal(err)
	}
	m := h.run(t, Params{Subdir: "o2"})
	if m.SweepSkipped != "" || !h.inPool("default", only.BlobSHA256) {
		t.Fatalf("o1's blob was swept: %+v", m)
	}
	if got, err := os.ReadFile(refs); err != nil || string(got) != string(want) {
		t.Fatalf("regenerated refs %q %v", got, err)
	}
}

// Refs that match nothing in the pool mean a reading error: the sweep is
// skipped, nothing is deleted, and the run says why. Root directories with
// neither meta.db nor manifest are not read.
func TestSweepBackstopAndForeignDirs(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a", "alpha")
	h.run(t, Params{Subdir: "k1"})
	orphan := h.writeOrphan(t)
	if err := os.MkdirAll(filepath.Join(h.s.Dir, "lost+found", "junk"), 0o700); err != nil {
		t.Fatal(err)
	}
	if m := h.run(t, Params{Subdir: "k2"}); m.SweepSkipped != "" || h.inPool("default", orphan) {
		t.Fatalf("a foreign directory blocked the sweep: %+v", m)
	}
	orphan = h.writeOrphan(t)
	// k1's refs now name blobs the pool does not hold.
	bogus := "default/" + strings.Repeat("0", 64) + "\n"
	if err := os.WriteFile(filepath.Join(h.s.Dir, "k1", "refs"), []byte(bogus), 0o600); err != nil {
		t.Fatal(err)
	}
	m := h.run(t, Params{Subdir: "k3"})
	if !strings.Contains(m.SweepSkipped, "k1") || m.SweptBlobs != 0 || !h.inPool("default", orphan) {
		t.Fatalf("backstop: %+v", m)
	}
	// Refs out of order are a reading error too.
	unsorted := "default/" + strings.Repeat("f", 64) + "\ndefault/" + strings.Repeat("0", 64) + "\n"
	if err := os.WriteFile(filepath.Join(h.s.Dir, "k1", "refs"), []byte(unsorted), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := h.s.Delete(context.Background(), "k2"); err != nil || res.SweepSkipped == "" || !h.inPool("default", orphan) {
		t.Fatalf("unsorted refs: %+v %v", res, err)
	}
}

// A size mismatch alone is reported but not repaired: only a re-hash that
// differs from the sha removes a pool file.
func TestRepairOnlyOnHashMismatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.put(t, "mvn", "a", "apple")
	// The asset row records a wrong size; the blob itself is fine.
	if err := h.db.PutAsset(ctx, meta.Asset{Repo: "mvn", Path: "a", BlobSHA256: a.BlobSHA256, Size: 999}); err != nil {
		t.Fatal(err)
	}
	h.run(t, Params{Subdir: "z"})
	res, err := h.s.VerifyAndRepair(ctx, "z", "admin", nil)
	if err != nil || res.OK || res.Corrupt != 1 || res.Repaired != 0 {
		t.Fatalf("verify: %+v %v", res, err)
	}
	if !h.inPool("default", a.BlobSHA256) {
		t.Fatal("a pool blob with the right content was removed")
	}
}

// countingReader yields the lines "<store>/<sha of i>" for i in [0, n) with
// step, lazily.
type lineGen struct {
	i, n, step int
	store      string
	buf        []byte
}

func (g *lineGen) Read(p []byte) (int, error) {
	for len(g.buf) < len(p) && g.i < g.n {
		g.buf = append(g.buf, fmt.Sprintf("%s/%064x\n", g.store, g.i)...)
		g.i += g.step
	}
	if len(g.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, g.buf)
	g.buf = g.buf[n:]
	return n, nil
}

// The merge of large refs streams against a large pool is correct and its
// memory stays flat.
func TestMergeLargeFlat(t *testing.T) {
	const n = 400000
	pool := func(yield func(store, sha string) error) error {
		for i := 0; i < n; i++ {
			if err := yield("default", fmt.Sprintf("%064x", i)); err != nil {
				return err
			}
		}
		return nil
	}
	var base runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
		}
	}()
	var dead int
	st, err := mergeRefs(context.Background(), pool,
		[]refSource{{name: "evens", r: &lineGen{n: n, step: 2, store: "default"}}, {name: "threes", r: &lineGen{n: n, step: 3, store: "default"}}},
		func(store, sha string) error { dead++; return nil })
	close(stop)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for i := 0; i < n; i++ {
		if i%2 == 0 || i%3 == 0 {
			live++
		}
	}
	if st.live != int64(live) || dead != n-live || st.pool != n {
		t.Fatalf("live %d dead %d pool %d, want live %d dead %d", st.live, dead, st.pool, live, n-live)
	}
	if grew := int64(peak.Load()) - int64(base.HeapInuse); grew > 16<<20 {
		t.Fatalf("heap grew by %d MiB during the merge", grew>>20)
	}
}

// Reserved names are refused whatever their case: on a case-insensitive
// file system "Blobs" is the shared pool directory.
func TestSubdirReservedNamesCaseInsensitive(t *testing.T) {
	for _, bad := range []string{"blobs", "Blobs", "BLOBS", "bLoBs", ".tmp-x", ".TMP-x", ".Tmp-"} {
		if err := ValidSubdir(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	for _, ok := range []string{"blobs2", "myblobs", "tmp-x", "x.tmp-y"} {
		if err := ValidSubdir(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

// ListAll shows a directory with a meta.db and no manifest as incomplete
// (so it can be deleted); List does not, and a running backup's directory
// is not listed.
func TestListAllShowsIncompleteDirectories(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a", "alpha")
	h.run(t, Params{Subdir: "good"})
	h.run(t, Params{Subdir: "broken"})
	if err := os.Remove(filepath.Join(h.s.Dir, "broken", manifestFile)); err != nil {
		t.Fatal(err)
	}
	// A directory without a meta.db is not a backup at all.
	if err := os.MkdirAll(filepath.Join(h.s.Dir, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	list, err := h.s.List()
	if err != nil || len(list) != 1 || list[0].Name != "good" || list[0].Incomplete {
		t.Fatalf("List: %+v %v", list, err)
	}
	all, err := h.s.ListAll()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAll: %+v %v", all, err)
	}
	var broken *Info
	for i := range all {
		if all[i].Name == "broken" {
			broken = &all[i]
		}
	}
	if broken == nil || !broken.Incomplete || broken.MetaBytes == 0 || broken.Created.IsZero() {
		t.Fatalf("broken: %+v", broken)
	}
	b, _ := json.Marshal(broken)
	if !strings.Contains(string(b), `"incomplete":true`) {
		t.Fatalf("json: %s", b)
	}
	if _, err := h.s.Delete(context.Background(), "broken"); err != nil {
		t.Fatalf("delete incomplete: %v", err)
	}
	// While a backup holds the lock its directory is not an orphan.
	if err := os.MkdirAll(filepath.Join(h.s.Dir, "running"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.s.Dir, "running", metaFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.s.mu.Lock()
	all, err = h.s.ListAll()
	h.s.mu.Unlock()
	if err != nil || len(all) != 1 || all[0].Name != "good" {
		t.Fatalf("ListAll during a backup: %+v %v", all, err)
	}
}

// With PostgreSQL as the metadata database the online backup is not
// available: every operation refuses with ErrPostgres, which names the
// database's own tools.
func TestDisabledServiceRefuses(t *testing.T) {
	h := newHarness(t)
	h.s.Disabled = ErrPostgres
	ctx := context.Background()
	if _, err := h.s.Run(ctx, Params{Subdir: "x"}, "admin", nil); !errors.Is(err, ErrPostgres) {
		t.Fatalf("run: %v", err)
	}
	if _, err := h.s.Verify(ctx, "x", false); !errors.Is(err, ErrPostgres) {
		t.Fatalf("verify: %v", err)
	}
	if _, err := h.s.VerifyAndRepair(ctx, "x", "admin", nil); !errors.Is(err, ErrPostgres) {
		t.Fatalf("deep verify: %v", err)
	}
	if _, err := h.s.Delete(ctx, "x"); !errors.Is(err, ErrPostgres) {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(ErrPostgres.Error(), "pg_dump") {
		t.Fatalf("message: %v", ErrPostgres)
	}
	if _, err := os.Stat(filepath.Join(h.s.Dir, "x")); !os.IsNotExist(err) {
		t.Fatalf("a backup directory was made: %v", err)
	}
}
