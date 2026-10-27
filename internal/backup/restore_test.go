package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/datalock"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

type captureLog struct{ warns []string }

func (c *captureLog) Infof(string, ...any)     {}
func (c *captureLog) Warnf(f string, a ...any) { c.warns = append(c.warns, f) }
func (c *captureLog) Errorf(string, ...any)    {}

func (h *harness) restore(t *testing.T, name string, data string, force, allowMissing bool) (RestoreResult, error) {
	t.Helper()
	return Restore(context.Background(), RestoreOptions{From: filepath.Join(h.s.Dir, name), Data: data, Force: force, AllowMissing: allowMissing})
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func blobAt(root, sha string) string {
	return filepath.Join(root, "blobs", "sha256", sha[:2], sha[2:4], sha)
}

func TestRestoreRoundTripAndStoreMapping(t *testing.T) {
	h := newHarness(t)
	a1 := h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	a2 := h.put(t, "mvn-b", "b/1/b-1.jar", "beta")
	if _, err := h.store.Blobs[repo.DefaultStore].Write(strings.NewReader("orphan")); err != nil {
		t.Fatal(err)
	}
	m := h.run(t, Params{Subdir: "b1", Deep: true})
	if m.RefsSHA256 == "" || m.RefsLines != 2 {
		t.Fatalf("manifest refs fields: %+v", m)
	}

	data := filepath.Join(t.TempDir(), "newdata")
	res, err := h.restore(t, "b1", data, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blobs != 2 || res.CopiedBlobs != 2 || res.Missing != 0 || res.Bytes != int64(len("alpha")+len("beta")) || res.MovedTo != "" {
		t.Fatalf("result: %+v", res)
	}
	// default store at <data>/blobs, store b at its relative path.
	if got := readFile(t, blobAt(data, a1.BlobSHA256)); got != "alpha" {
		t.Fatalf("default blob: %q", got)
	}
	if got := readFile(t, blobAt(filepath.Join(data, "blobstores", "b"), a2.BlobSHA256)); got != "beta" {
		t.Fatalf("store b blob: %q", got)
	}
	if _, err := os.Stat(blobAt(data, a2.BlobSHA256)); err == nil {
		t.Fatal("a store-b blob was put in the default store")
	}
	if shaOf("alpha") != a1.BlobSHA256 || shaOf("beta") != a2.BlobSHA256 {
		t.Fatal("blob ids are not the content sha256")
	}
	if _, err := os.Stat(blobAt(data, shaOf("orphan"))); err == nil {
		t.Fatal("an unreferenced blob was restored")
	}
	// The stores map to their directories.
	if res.Stores["b"] != filepath.Join(data, "blobstores", "b") || res.Stores[repo.DefaultStore] != data {
		t.Fatalf("stores: %v", res.Stores)
	}
	db, err := meta.Open(filepath.Join(data, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n, err := db.CountAssets(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("restored assets: %d %v", n, err)
	}
	// The locks are released.
	for _, f := range []string{datalock.FileName, datalock.ServerFileName} {
		l, err := datalock.TryAcquire(filepath.Join(data, f))
		if err != nil {
			t.Fatalf("%s still held: %v", f, err)
		}
		l.Release()
	}
	// The backup is untouched and restorable again.
	if _, err := h.restore(t, "b1", filepath.Join(t.TempDir(), "again"), false, false); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRootsRefuseEscapes(t *testing.T) {
	data := filepath.Join(t.TempDir(), "d")
	for _, p := range []string{"..", "../x", "a/../../x", ".", "", BeforeRestorePrefix + "20260101-000000/b", ".gc.lock/x", "meta.db"} {
		if _, err := restoreRoots(data, []meta.BlobStore{{Name: "evil", Path: p}}); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	roots, err := restoreRoots(data, []meta.BlobStore{{Name: "default", Path: "/old/place"}, {Name: "b", Path: "stores/b"}})
	if err != nil || roots["default"] != data || roots["b"] != filepath.Join(data, "stores", "b") {
		t.Fatalf("roots: %v %v", roots, err)
	}
}

func TestRestoreForceMovesOldDataAside(t *testing.T) {
	h := newHarness(t)
	a := h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "f1"})

	data := t.TempDir()
	mustWrite := func(rel, body string) {
		p := filepath.Join(data, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("meta.db", "old database")
	mustWrite("blobs/sha256/zz/zz/old", "old blob")
	mustWrite("blobstores/b/x", "old b")
	mustWrite(BeforeRestorePrefix+"20200101-000000/keep", "earlier restore")
	// A lock file does not make the directory non-empty.
	if err := os.WriteFile(filepath.Join(data, datalock.FileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.restore(t, "f1", data, false, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("a non-empty data dir without --force: %v", err)
	}
	if readFile(t, filepath.Join(data, "meta.db")) != "old database" {
		t.Fatal("the refused restore changed the data dir")
	}

	res, err := h.restore(t, "f1", data, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.MovedTo, filepath.Join(data, BeforeRestorePrefix)) {
		t.Fatalf("moved to %q", res.MovedTo)
	}
	if readFile(t, filepath.Join(res.MovedTo, "meta.db")) != "old database" ||
		readFile(t, filepath.Join(res.MovedTo, "blobs/sha256/zz/zz/old")) != "old blob" ||
		readFile(t, filepath.Join(res.MovedTo, "blobstores/b/x")) != "old b" {
		t.Fatal("the old contents are not in the aside directory")
	}
	if readFile(t, filepath.Join(data, BeforeRestorePrefix+"20200101-000000", "keep")) != "earlier restore" {
		t.Fatal("an earlier .before-restore directory was moved or lost")
	}
	if _, err := os.Stat(filepath.Join(data, "blobs/sha256/zz/zz/old")); err == nil {
		t.Fatal("old blob still in place")
	}
	if got := readFile(t, blobAt(data, a.BlobSHA256)); got != "alpha" {
		t.Fatalf("restored blob: %q", got)
	}
	db, err := meta.Open(filepath.Join(data, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Restoring again into the (now restored) dir with --force sets the
	// previous restore aside too, under a different name.
	res2, err := h.restore(t, "f1", data, true, false)
	if err != nil || res2.MovedTo == res.MovedTo {
		t.Fatalf("second force: %+v %v", res2, err)
	}
}

func TestRestoreMissingBlobFails(t *testing.T) {
	h := newHarness(t)
	a := h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	b := h.put(t, "mvn-b", "b/1/b-1.jar", "beta")
	h.run(t, Params{Subdir: "m1"})
	if err := os.Remove(h.poolPath("b", b.BlobSHA256)); err != nil {
		t.Fatal(err)
	}

	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "keepme"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := h.restore(t, "m1", data, true, false)
	if err == nil || !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "--allow-missing") {
		t.Fatalf("missing blob: %v", err)
	}
	// Nothing was moved or written.
	if readFile(t, filepath.Join(data, "keepme")) != "x" {
		t.Fatal("a doomed restore touched the data dir")
	}
	if _, err := os.Stat(filepath.Join(data, "meta.db")); err == nil {
		t.Fatal("meta.db written by a failed restore")
	}

	res, err := h.restore(t, "m1", data, true, true)
	if err != nil || res.Missing != 1 || res.Blobs != 2 {
		t.Fatalf("allow-missing: %+v %v", res, err)
	}
	if readFile(t, blobAt(data, a.BlobSHA256)) != "alpha" {
		t.Fatal("the present blob was not restored")
	}
}

func TestRestoreCorruptPoolBlobFailsWithoutMetaDB(t *testing.T) {
	h := newHarness(t)
	a := h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "c1"})
	if err := os.WriteFile(h.poolPath("default", a.BlobSHA256), []byte("ALPHA"), 0o600); err != nil { // same size
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "d")
	if _, err := h.restore(t, "c1", data, false, false); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("corrupt blob: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "meta.db")); err == nil {
		t.Fatal("a failed restore left a meta.db")
	}
	if _, err := os.Stat(blobAt(data, a.BlobSHA256)); err == nil {
		t.Fatal("the corrupt blob was installed")
	}
	res, err := h.restore(t, "c1", filepath.Join(t.TempDir(), "e"), false, true)
	if err != nil || res.Missing != 1 {
		t.Fatalf("allow-missing with a corrupt blob: %+v %v", res, err)
	}
}

func TestRestoreCorruptMetaDBFails(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "k1"})
	p := filepath.Join(h.s.Dir, "k1", "meta.db")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "d")
	_, err = h.restore(t, "k1", data, false, false)
	if err == nil || !strings.Contains(err.Error(), "meta.db does not match") {
		t.Fatalf("corrupt meta.db: %v", err)
	}
	if ents, _ := os.ReadDir(data); len(ents) != 0 {
		t.Fatalf("a refused restore wrote %d entries", len(ents))
	}
}

func TestRestoreRefusesBadManifest(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "x1"})
	if _, err := Restore(context.Background(), RestoreOptions{From: filepath.Join(h.s.Dir, "nope"), Data: t.TempDir()}); err == nil {
		t.Fatal("a missing backup was accepted")
	}
	mp := filepath.Join(h.s.Dir, "x1", "manifest.json")
	var raw map[string]any
	if err := json.Unmarshal([]byte(readFile(t, mp)), &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = 99
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(mp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(t, "x1", t.TempDir(), false, false); err == nil || !strings.Contains(err.Error(), "unsupported manifest") {
		t.Fatalf("version 99: %v", err)
	}
}

func TestRestoreRefusesWhileLocked(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "l1"})
	for _, f := range []string{datalock.FileName, datalock.ServerFileName} {
		data := t.TempDir()
		l, err := datalock.TryAcquire(filepath.Join(data, f))
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.restore(t, "l1", data, true, false)
		l.Release()
		if err == nil || !strings.Contains(err.Error(), "in use") {
			t.Fatalf("%s held: %v", f, err)
		}
		if _, err := os.Stat(filepath.Join(data, "meta.db")); err == nil {
			t.Fatal("restored while the lock was held")
		}
	}
}

func TestRestoreRefsIntegrity(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.put(t, "mvn-b", "b/1/b-1.jar", "beta")
	h.run(t, Params{Subdir: "r1"})
	refs := filepath.Join(h.s.Dir, "r1", "refs")
	orig := readFile(t, refs)

	// Same line count, different content.
	lines := strings.Split(strings.TrimSpace(orig), "\n")
	lines[0] = lines[0][:len(lines[0])-1] + "0"
	if lines[0] == strings.Split(strings.TrimSpace(orig), "\n")[0] {
		lines[0] = lines[0][:len(lines[0])-1] + "1"
	}
	if err := os.WriteFile(refs, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(t, "r1", filepath.Join(t.TempDir(), "d"), false, false); err == nil || !strings.Contains(err.Error(), "refs file does not match") {
		t.Fatalf("tampered refs: %v", err)
	}
	// Truncated: the line count and hash differ.
	if err := os.WriteFile(refs, []byte(lines[1]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(t, "r1", filepath.Join(t.TempDir(), "d"), false, false); err == nil || !strings.Contains(err.Error(), "refs file does not match the manifest (sha256 ") {
		t.Fatalf("truncated refs: %v", err)
	}
	// Deleted.
	os.Remove(refs)
	if _, err := h.restore(t, "r1", filepath.Join(t.TempDir(), "d"), false, false); err == nil || !strings.Contains(err.Error(), "refs file is missing") {
		t.Fatalf("missing refs: %v", err)
	}
	if err := os.WriteFile(refs, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(t, "r1", filepath.Join(t.TempDir(), "d"), false, false); err != nil {
		t.Fatalf("intact refs: %v", err)
	}
}

// A damaged refs file stops the sweep: nothing is deleted from the pool.
func TestSweepSkippedOnRefsMismatch(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "s1"})
	b := h.put(t, "mvn-b", "b/1/b-1.jar", "beta")
	h.run(t, Params{Subdir: "s2"})
	orphan := h.poolPath("default", h.writeOrphan(t))
	if err := os.WriteFile(filepath.Join(h.s.Dir, "s1", "refs"), []byte("default/"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := h.s.Delete(context.Background(), "s2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SweepSkipped, "refs file does not match") || res.SweptBlobs != 0 {
		t.Fatalf("sweep: %+v", res)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("the orphan was swept although the refs file was damaged")
	}
	if !h.inPool("b", b.BlobSHA256) {
		t.Fatal("a pool blob was deleted")
	}
}

// A backup from before the refs checksum (no refsSha256) still restores and
// sweeps, with a warning.
func TestLegacyBackupWithoutRefsChecksum(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "g1"})
	mp := filepath.Join(h.s.Dir, "g1", "manifest.json")
	var raw map[string]any
	if err := json.Unmarshal([]byte(readFile(t, mp)), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "refsSha256")
	delete(raw, "refsLines")
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(mp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cl := &captureLog{}
	res, err := Restore(context.Background(), RestoreOptions{From: filepath.Join(h.s.Dir, "g1"), Data: filepath.Join(t.TempDir(), "d"), Log: cl})
	if err != nil || !res.LegacyRefs || len(cl.warns) == 0 || res.Blobs != 1 {
		t.Fatalf("legacy restore: %+v %v warns=%v", res, err, cl.warns)
	}
	// Legacy without a refs file: the list comes from meta.db.
	os.Remove(filepath.Join(h.s.Dir, "g1", "refs"))
	res, err = h.restore(t, "g1", filepath.Join(t.TempDir(), "d"), false, false)
	if err != nil || res.Blobs != 1 || res.Missing != 0 {
		t.Fatalf("legacy restore without refs: %+v %v", res, err)
	}
	orphan := h.poolPath("default", h.writeOrphan(t))
	h.run(t, Params{Subdir: "g2", Keep: 1}) // prunes g1 and sweeps
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("the sweep did not run for a legacy backup")
	}
}

// makeLegacy strips the refs checksum fields from a backup's manifest, as in
// a backup made before they existed.
func (h *harness) makeLegacy(t *testing.T, name string) {
	t.Helper()
	mp := filepath.Join(h.s.Dir, name, "manifest.json")
	var raw map[string]any
	if err := json.Unmarshal([]byte(readFile(t, mp)), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "refsSha256")
	delete(raw, "refsLines")
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(mp, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
