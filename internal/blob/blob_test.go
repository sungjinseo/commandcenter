package blob

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	helloSHA256 = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	helloSHA1   = "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"
	helloMD5    = "5d41402abc4b2a76b9719d911017c592"
)

func TestWriteAndOpen(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sums, err := s.Write(strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if sums.SHA256 != helloSHA256 || sums.SHA1 != helloSHA1 || sums.MD5 != helloMD5 || sums.Size != 5 || len(sums.SHA512) != 128 {
		t.Fatalf("sums = %+v", sums)
	}
	f, err := s.Open(sums.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
}

func TestWriteDeduplicates(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := s.Walk(func(string, time.Time) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("blobs = %d, want 1", n)
	}
	assertEmptyTmp(t, root)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestFailedWriteLeavesNoFiles(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	if _, err := s.Write(io.MultiReader(strings.NewReader("partial"), failingReader{})); err == nil {
		t.Fatal("expected error")
	}
	assertEmptyTmp(t, root)
}

// A process killed mid-write leaves its temp file behind; Open (and SweepTmp,
// which the compact task runs) removes it once it is stale, but never a temp
// file a live write may still be using, nor files Write did not create.
func TestSweepTmpRemovesOnlyStaleTempFiles(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(name string, age time.Duration) {
		t.Helper()
		p, mod := filepath.Join(tmp, name), time.Now().Add(-age)
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	put("up-stale", 2*time.Hour)
	put("up-live", 30*time.Minute)
	put("notes.txt", 2*time.Hour)

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "up-stale")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open kept the stale temp file: %v", err)
	}
	for _, name := range []string{"up-live", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(tmp, name)); err != nil {
			t.Fatalf("%s was removed: %v", name, err)
		}
	}

	put("up-later", 2*time.Hour)
	if n, err := s.SweepTmp(); n != 1 || err != nil {
		t.Fatalf("SweepTmp = %d, %v; want 1, nil", n, err)
	}
}

func TestOpenErrors(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, err := s.Open("../../etc/passwd"); err == nil {
		t.Fatal("expected error for invalid sha")
	}
	if _, err := s.Open(helloSHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRemove(t *testing.T) {
	s, _ := Open(t.TempDir())
	sums, _ := s.Write(strings.NewReader("hello"))
	if err := s.Remove(sums.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(sums.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func assertEmptyTmp(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp not empty: %d entries", len(entries))
	}
}

func TestWriteDedupRefreshesModTime(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(root, "blobs", "sha256", "2c", "f2", helloSHA256)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(blobPath, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	var mod time.Time
	if err := s.Walk(func(sha string, m time.Time) error {
		if sha == helloSHA256 {
			mod = m
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if mod.IsZero() || time.Since(mod) > time.Minute {
		t.Fatalf("dedup write did not refresh mtime: %v", mod)
	}
}

func TestWriteReplacesCorruptBlob(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(root, "blobs", "sha256", "2c", "f2", helloSHA256)
	if err := os.Truncate(blobPath, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	f, err := s.Open(helloSHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != "hello" {
		t.Fatalf("content = %q, want hello", b)
	}
}

func TestModTime(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, err := s.ModTime(helloSHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing blob: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Write(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	m, err := s.ModTime(helloSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(m) > time.Minute {
		t.Fatalf("unexpected mtime %v", m)
	}
}
