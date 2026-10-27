package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

func (h *harness) restoreOpts(t *testing.T, name string, o RestoreOptions) (RestoreResult, error) {
	t.Helper()
	o.From = filepath.Join(h.s.Dir, name)
	return Restore(context.Background(), o)
}

// addAbsStore adds the store "c" at the absolute path abs with the repository
// "mvn-c", puts one asset into it and returns the asset.
func (h *harness) addAbsStore(t *testing.T, abs, body string) meta.Asset {
	t.Helper()
	ctx := context.Background()
	if err := h.db.CreateBlobStore(ctx, meta.BlobStore{Name: "c", Path: abs}); err != nil {
		t.Fatal(err)
	}
	cs, err := blob.Open(abs)
	if err != nil {
		t.Fatal(err)
	}
	h.store.Blobs["c"] = cs
	if err := h.db.CreateRepository(ctx, config.Repository{Name: "mvn-c", Format: "maven", Type: "hosted", BlobStore: "c"}); err != nil {
		t.Fatal(err)
	}
	prev := h.store.StoreFor
	h.store.StoreFor = func(n string) string {
		if n == "mvn-c" {
			return "c"
		}
		return prev(n)
	}
	return h.put(t, "mvn-c", "c/1/c-1.jar", body)
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if n := e.Name(); n != ".gc.lock" && n != ".server.lock" {
			out = append(out, n)
		}
	}
	return out
}

func TestRestoreAbsoluteStoreNeedsFlag(t *testing.T) {
	h := newHarness(t)
	abs := filepath.Join(t.TempDir(), "elsewhere")
	a := h.addAbsStore(t, abs, "gamma")
	h.run(t, Params{Subdir: "abs"})
	if err := os.RemoveAll(abs); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "d")
	_, err := h.restoreOpts(t, "abs", RestoreOptions{Data: data})
	if err == nil || !strings.Contains(err.Error(), "--allow-absolute-stores") || !strings.Contains(err.Error(), "c="+abs) {
		t.Fatalf("without the flag: %v", err)
	}
	if _, serr := os.Stat(abs); serr == nil {
		t.Fatal("the refused restore created the store directory")
	}
	if ents, _ := os.ReadDir(data); len(ents) != 0 {
		t.Fatalf("the refused restore wrote %v", ents)
	}
	res, err := h.restoreOpts(t, "abs", RestoreOptions{Data: data, AllowAbsoluteStores: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, blobAt(abs, a.BlobSHA256)); got != "gamma" || res.Stores["c"] != abs {
		t.Fatalf("absolute store: %q %v", got, res.Stores)
	}
	// No staging directory is left anywhere.
	for _, d := range []string{data, abs} {
		for _, n := range listDir(t, d) {
			if strings.HasPrefix(n, StagingPrefix) {
				t.Fatalf("%s kept %s", d, n)
			}
		}
	}
}

// An existing blob file is never trusted by its size: a different file of the
// same size at the target is replaced.
func TestRestoreReplacesExistingBlobOfSameSize(t *testing.T) {
	h := newHarness(t)
	abs := filepath.Join(t.TempDir(), "elsewhere")
	a := h.addAbsStore(t, abs, "gamma")
	h.run(t, Params{Subdir: "abs"})
	if err := os.WriteFile(blobAt(abs, a.BlobSHA256), []byte("GAMMA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restoreOpts(t, "abs", RestoreOptions{Data: filepath.Join(t.TempDir(), "d"), AllowAbsoluteStores: true}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, blobAt(abs, a.BlobSHA256)); got != "gamma" {
		t.Fatalf("the same-size file was kept: %q", got)
	}
}

func TestRestoreRefusesUnsafeAbsolutePaths(t *testing.T) {
	h := newHarness(t)
	data := filepath.Join(t.TempDir(), "d")
	back := filepath.Dir(filepath.Join(h.s.Dir, "x"))
	os.MkdirAll(data, 0o755)
	link := filepath.Join(t.TempDir(), "link")
	linked := os.Symlink(data, link) == nil
	cases := map[string]string{
		"data dir":        data,
		"inside data":     filepath.Join(data, "stores", "c"),
		"backup root":     back,
		"inside backup":   filepath.Join(back, "sub"),
		"before-restore":  filepath.Join(t.TempDir(), BeforeRestorePrefix+"20260101-000000", "c"),
		"staging":         filepath.Join(t.TempDir(), StagingPrefix+"x"),
		"lock name":       filepath.Join(t.TempDir(), ".gc.lock"),
		"contains data":   filepath.Dir(data),
		"symlink to data": filepath.Join(link, "inner"),
	}
	for name, p := range cases {
		if name == "symlink to data" && !linked {
			continue
		}
		if err := checkAbsolute(p, data, back); err == nil {
			t.Errorf("%s: %s was accepted", name, p)
		}
	}
	if err := checkAbsolute(filepath.Join(t.TempDir(), "ok"), data, back); err != nil {
		t.Errorf("a safe path was refused: %v", err)
	}
	// And end to end: a store inside the data dir is refused even with the flag.
	abs := filepath.Join(data, "inner-store")
	h.addAbsStore(t, abs, "gamma")
	h.run(t, Params{Subdir: "u1"})
	_, err := h.restoreOpts(t, "u1", RestoreOptions{Data: data, AllowAbsoluteStores: true})
	if err == nil || !strings.Contains(err.Error(), "inside it") {
		t.Fatalf("store inside the data dir: %v", err)
	}
}

// A corrupt pool blob with --force leaves the old data entirely in place:
// nothing is moved aside, nothing is left behind.
func TestRestoreFailureBeforeSwapLeavesOldDataUntouched(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	b := h.put(t, "mvn-b", "b/1/b-1.jar", "beta")
	h.run(t, Params{Subdir: "s1"})
	if err := os.WriteFile(h.poolPath("b", b.BlobSHA256), []byte("BETA"), 0o600); err != nil { // same size, wrong hash
		t.Fatal(err)
	}
	data := t.TempDir()
	for _, f := range []string{"meta.db", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(data, f), []byte("old "+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(data, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := listDir(t, data)
	_, err := h.restoreOpts(t, "s1", RestoreOptions{Data: data, Force: true})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("corrupt blob: %v", err)
	}
	after := listDir(t, data)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("data dir changed: %v -> %v", before, after)
	}
	if readFile(t, filepath.Join(data, "meta.db")) != "old meta.db" || readFile(t, filepath.Join(data, "keep.txt")) != "old keep.txt" {
		t.Fatal("old files changed")
	}
}

func TestRestoreFailureBeforeSwapCleansAbsoluteStaging(t *testing.T) {
	h := newHarness(t)
	abs := filepath.Join(t.TempDir(), "elsewhere")
	a := h.addAbsStore(t, abs, "gamma")
	b := h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "s1"})
	os.WriteFile(h.poolPath("default", b.BlobSHA256), []byte("ALPHA"), 0o600)
	os.RemoveAll(abs)
	data := filepath.Join(t.TempDir(), "d")
	if _, err := h.restoreOpts(t, "s1", RestoreOptions{Data: data, AllowAbsoluteStores: true}); err == nil {
		t.Fatal("corrupt blob accepted")
	}
	if _, err := os.Stat(blobAt(abs, a.BlobSHA256)); err == nil {
		t.Fatal("a blob reached the absolute store although the restore failed")
	}
	if _, err := os.Stat(abs); err == nil {
		t.Fatalf("the store directory (created by the restore) remains: %v", listDir(t, abs))
	}
	for _, n := range listDir(t, data) {
		if strings.HasPrefix(n, StagingPrefix) {
			t.Fatalf("staging left: %s", n)
		}
	}
}

func TestRestoreCountsMustMatchManifest(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.put(t, "mvn-b", "b/1/b-1.jar", "beta")
	h.run(t, Params{Subdir: "n1"})
	mp := filepath.Join(h.s.Dir, "n1", "manifest.json")
	var raw map[string]any
	if err := json.Unmarshal([]byte(readFile(t, mp)), &raw); err != nil {
		t.Fatal(err)
	}
	raw["blobs"] = 3
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(mp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "d")
	_, err := h.restoreOpts(t, "n1", RestoreOptions{Data: data})
	if err == nil || !strings.Contains(err.Error(), "refs list has 2 blobs but the manifest says 3") {
		t.Fatalf("count mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "meta.db")); err == nil {
		t.Fatal("restored despite the mismatch")
	}
	if _, err := h.restoreOpts(t, "n1", RestoreOptions{Data: data, AllowMissing: true}); err != nil {
		t.Fatalf("--allow-missing: %v", err)
	}
}

func TestRestoreRefsLinesMismatch(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "n2"})
	mp := filepath.Join(h.s.Dir, "n2", "manifest.json")
	var raw map[string]any
	if err := json.Unmarshal([]byte(readFile(t, mp)), &raw); err != nil {
		t.Fatal(err)
	}
	raw["refsLines"] = 5
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(mp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(t, "n2", filepath.Join(t.TempDir(), "d"), false, false); err == nil || !strings.Contains(err.Error(), "1 lines, recorded 5") {
		t.Fatalf("refsLines: %v", err)
	}
}

func TestRestoreNeedsMetaChecksum(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "m2"})
	mp := filepath.Join(h.s.Dir, "m2", "manifest.json")
	var raw map[string]any
	if err := json.Unmarshal([]byte(readFile(t, mp)), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "metaSha256")
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(mp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(t, "m2", filepath.Join(t.TempDir(), "d"), false, false); err == nil || !strings.Contains(err.Error(), "no meta.db checksum") {
		t.Fatalf("no metaSha256: %v", err)
	}
}

func TestRestoreNonEmptyMessageMentionsInterruptedRestore(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/1/a-1.jar", "alpha")
	h.run(t, Params{Subdir: "i1"})
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "blobs"), []byte("x"), 0o600)
	_, err := h.restore(t, "i1", data, false, false)
	if err == nil || !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("message: %v", err)
	}
}
