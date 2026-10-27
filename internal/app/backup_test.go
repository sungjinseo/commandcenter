package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/backup"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/tasks"
)

// An online backup through the real server while uploads keep arriving: the
// run completes, its snapshot opens, and a deep verify finds every blob the
// snapshot references in the pool. Uploads made after the snapshot are not
// in it, and none of them failed.
func TestOnlineBackupDuringUploads(t *testing.T) {
	if metatest.Postgres() {
		t.Skip("online backup and restore need the SQLite metadata database")
	}
	cfg := testConfig(t, "")
	cfg.BackupDir = filepath.Join(t.TempDir(), "backup")
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = append(cfg.Users, config.User{Name: "root", PasswordHash: string(h), Roles: []string{"admin"}})
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	if a.Tasks.Backup == nil || a.Tasks.Backup.Dir != cfg.BackupDir {
		t.Fatal("the backup service is not wired")
	}
	_, tok, err := a.Auth.CreateToken(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	put := func(i int, prefix string) error {
		path := fmt.Sprintf("/repository/maven-releases/org/ex/%s/%d/%s-%d.jar", prefix, i, prefix, i)
		req, _ := http.NewRequest("PUT", ts.URL+path, strings.NewReader(strings.Repeat(fmt.Sprintf("%s-%d;", prefix, i), 200)))
		req.SetBasicAuth("ci", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("PUT %s: %d", path, resp.StatusCode)
		}
		return nil
	}
	for i := range 30 {
		if err := put(i, "pre"); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var live atomic.Int64
	for w := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := put(w*100000+i, "live"); err != nil {
					t.Error(err)
					return
				}
				live.Add(1)
			}
		}()
	}
	call := func(method, path string) (int, map[string]any) {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	for live.Load() < 10 {
		time.Sleep(time.Millisecond)
	}
	st, m := call("POST", "/-/api/backup")
	if st != http.StatusAccepted {
		t.Fatalf("start: %d %v", st, m)
	}
	id := m["id"].(string)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, m = call("GET", "/-/api/backup/runs/"+id)
		if m["status"] != "Running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the backup did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if m["status"] != "OK" {
		t.Fatalf("backup run: %v", m)
	}
	list, err := a.Tasks.Backup.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	b := list[0]
	if b.Assets < 30 || b.Blobs < 30 || b.BlobStores["default"] != cfg.DataDir || b.Repositories != 2 {
		t.Fatalf("manifest: %+v", b.Manifest)
	}
	res, err := a.Tasks.Backup.Verify(context.Background(), b.Name, true)
	if err != nil || !res.OK || res.Blobs != b.Blobs {
		t.Fatalf("verify: %+v %v", res, err)
	}
	snap, err := meta.OpenReadOnly(filepath.Join(cfg.BackupDir, b.Name, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	n, err := snap.CountAssets(context.Background())
	if err != nil || n != b.Assets {
		t.Fatalf("snapshot assets %d %v, manifest %d", n, err, b.Assets)
	}
	total, err := a.DB.CountAssets(context.Background())
	if err != nil || total < n {
		t.Fatalf("live assets %d < snapshot %d (%v)", total, n, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.BackupDir, "blobs", "default", "sha256")); err != nil {
		t.Fatal(err)
	}
}

// On PostgreSQL the app's backup service is disabled with ErrPostgres, so
// the task, the API and the page refuse (the online backup copies meta.db).
func TestBackupDisabledOnPostgres(t *testing.T) {
	if !metatest.Postgres() {
		t.Skip("needs " + metatest.EnvURL)
	}
	a, err := New(testConfig(t, ""), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !errors.Is(a.Tasks.Backup.Disabled, backup.ErrPostgres) {
		t.Fatalf("disabled: %v", a.Tasks.Backup.Disabled)
	}
	if _, err := a.Tasks.StartAdHoc(tasks.TypeBackup, nil, "admin"); err == nil || !strings.Contains(err.Error(), "pg_dump") {
		t.Fatalf("start: %v", err)
	}
}
