package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/app"
	"commandcenter/internal/backup"
	"commandcenter/internal/config"
	"commandcenter/internal/datalock"
)

func makeBackup(t *testing.T) (backupDir string) {
	t.Helper()
	cfg, err := config.Parse([]byte("dataDir: " + filepath.ToSlash(t.TempDir()) + "\nrepositories:\n  - { name: maven-releases, format: maven, type: hosted }\n"))
	if err != nil {
		t.Fatal(err)
	}
	backupDir = filepath.Join(t.TempDir(), "bk")
	cfg.BackupDir = backupDir
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Store.Put(context.Background(), "maven-releases", "a/1/a-1.jar", "application/java-archive", strings.NewReader("alpha")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Tasks.Backup.Run(context.Background(), backup.Params{Subdir: "one"}, "test", nil); err != nil {
		t.Fatal(err)
	}
	return backupDir
}

func TestRestoreCommand(t *testing.T) {
	for _, args := range [][]string{{"restore"}, {"restore", "--from", "x"}, {"restore", "--data", "x"}, {"restore", "--from", "x", "--data", "y", "extra"}, {"restore", "--bogus"}} {
		if err := run(args, strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "usage: commandcenter restore") {
			t.Errorf("%v: %v", args, err)
		}
	}
	bk := makeBackup(t)
	data := filepath.Join(t.TempDir(), "d")
	var out bytes.Buffer
	if err := run([]string{"restore", "--from", filepath.Join(bk, "one"), "--data", data}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"restored backup one", "blobs: 1 (5 bytes)", "blob store default: " + data} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("summary lacks %q:\n%s", w, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(data, "meta.db")); err != nil {
		t.Fatal(err)
	}
	// A second restore needs --force; the server lock of a running serve blocks it.
	err := run([]string{"restore", "--from", filepath.Join(bk, "one"), "--data", data}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("non-empty: %v", err)
	}
	l, err := datalock.TryAcquire(filepath.Join(data, datalock.ServerFileName))
	if err != nil {
		t.Fatal(err)
	}
	err = run([]string{"restore", "--from", filepath.Join(bk, "one"), "--data", data, "--force"}, strings.NewReader(""), io.Discard)
	l.Release()
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("server running: %v", err)
	}
	out.Reset()
	if err := run([]string{"restore", "--from", filepath.Join(bk, "one"), "--data", data, "--force"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ".before-restore-") {
		t.Fatalf("force summary:\n%s", out.String())
	}
}

// With PostgreSQL configured, the offline restore refuses before it touches
// anything: the metadata is not in the backup's meta.db.
func TestRestoreRefusesPostgres(t *testing.T) {
	data := t.TempDir()
	t.Setenv("COMMANDCENTER_DATABASE_URL", "postgres://u:secret@db.example:5432/cc")
	var out bytes.Buffer
	err := restoreCmd([]string{"--from", filepath.Join(t.TempDir(), "one"), "--data", data}, &out)
	if err == nil || !strings.Contains(err.Error(), "pg_dump") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("COMMANDCENTER_DATABASE_URL", "")
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfgFile, []byte("dataDir: /data\ndatabase:\n  url: postgres://u:secret@db.example:5432/cc\n"), 0o600)
	err = restoreCmd([]string{"--config", cfgFile, "--from", filepath.Join(t.TempDir(), "one"), "--data", data}, &out)
	if err == nil || !strings.Contains(err.Error(), "pg_dump") {
		t.Fatalf("config: err = %v", err)
	}
}
