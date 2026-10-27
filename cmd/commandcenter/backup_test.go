package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/datalock"
)

const secretToken = "cc_supersecret0123456789"

// fakeBackupServer answers the backup API like the server does; polls says
// how many run polls answer Running before final.
type fakeBackupServer struct {
	mu        sync.Mutex
	start     int    // POST status
	final     string // OK or Failed
	polls     int
	gotAuth   []string
	gotBody   string
	gotUI     bool
	pollCount int
}

func (f *fakeBackupServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/-/api/backup":
		b, _ := io.ReadAll(r.Body)
		f.gotBody = string(b)
		f.gotUI = r.Header.Get("X-Command-Center-UI") != ""
		if f.start != http.StatusAccepted {
			w.WriteHeader(f.start)
			fmt.Fprint(w, `{"error":"a task is already running","running":"compact-blobstore"}`)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"id":"backup-abc123","started":true}`)
	case r.Method == "GET" && r.URL.Path == "/-/api/backup/runs/backup-abc123":
		f.pollCount++
		if f.pollCount <= f.polls {
			fmt.Fprint(w, `{"id":"backup-abc123","status":"Running"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "backup-abc123", "status": f.final,
			"message": "backup 20261026-030000: 3 blobs (10 bytes), copied 1 new blobs (4 bytes)", "durationMs": 12})
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found"}`)
	}
}

func runBackupCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(append([]string{"backup"}, args...), nil, &out)
	return out.String(), err
}

func noSecret(t *testing.T, out string, err error) {
	t.Helper()
	if strings.Contains(out, secretToken) || (err != nil && strings.Contains(err.Error(), secretToken)) {
		t.Fatalf("the token was printed: %q %v", out, err)
	}
}

func TestBackupCLI(t *testing.T) {
	old := backupPoll
	backupPoll = time.Millisecond
	t.Cleanup(func() { backupPoll = old })

	f := &fakeBackupServer{start: http.StatusAccepted, final: "OK", polls: 2}
	ts := httptest.NewServer(f)
	defer ts.Close()

	// Start without waiting: the token comes from the flag.
	out, err := runBackupCLI(t, "--url", ts.URL+"/", "--token", secretToken, "--subdir", "nightly", "--keep", "3")
	noSecret(t, out, err)
	if err != nil || !strings.Contains(out, "backup-abc123") {
		t.Fatalf("start: %q %v", out, err)
	}
	if f.gotAuth[0] != "Bearer "+secretToken || f.gotUI {
		t.Fatalf("auth %q ui %v", f.gotAuth[0], f.gotUI)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.gotBody), &body); err != nil || body["subdir"] != "nightly" || body["keep"] != float64(3) {
		t.Fatalf("body %q", f.gotBody)
	}
	if f.pollCount != 0 {
		t.Fatal("polled without --wait")
	}

	// --wait polls until the run finished; the token comes from the env.
	t.Setenv("COMMANDCENTER_TOKEN", secretToken)
	f.gotAuth = nil
	out, err = runBackupCLI(t, "--url", ts.URL, "--wait")
	noSecret(t, out, err)
	if err != nil || f.pollCount != 3 || !strings.Contains(out, "OK") || !strings.Contains(out, "copied 1 new blobs") {
		t.Fatalf("wait: %q %v (polls %d)", out, err, f.pollCount)
	}
	for _, a := range f.gotAuth {
		if a != "Bearer "+secretToken {
			t.Fatalf("auth header %q", a)
		}
	}
	if f.gotBody != "{}" {
		t.Fatalf("default body %q", f.gotBody)
	}

	// A failed run is an error.
	f.final, f.pollCount = "Failed", 0
	out, err = runBackupCLI(t, "--url", ts.URL, "--wait")
	noSecret(t, out, err)
	if err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("failed run: %q %v", out, err)
	}

	// A refused start shows the server's message.
	f.start = http.StatusConflict
	out, err = runBackupCLI(t, "--url", ts.URL)
	noSecret(t, out, err)
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("409: %v", err)
	}
}

func TestBackupCLIUsage(t *testing.T) {
	t.Setenv("COMMANDCENTER_TOKEN", "")
	if _, err := runBackupCLI(t, "--token", secretToken); err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("missing url: %v", err)
	}
	if _, err := runBackupCLI(t, "--url", "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "COMMANDCENTER_TOKEN") {
		t.Fatalf("missing token: %v", err)
	}
	_, err := runBackupCLI(t, "--url", "ftp://x", "--token", secretToken)
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("bad scheme: %v", err)
	}
	// A connection error does not echo the token.
	out, err := runBackupCLI(t, "--url", "http://127.0.0.1:1", "--token", secretToken)
	noSecret(t, out, err)
	if err == nil {
		t.Fatal("unreachable server must fail")
	}
}

// The offline gc takes the data directory lock: while a server run holds it,
// gc waits -wait, then fails pointing at the task and the API.
func TestGCWaitsForTheDataLock(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("dataDir: "+filepath.ToSlash(data)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := datalock.TryAcquire(filepath.Join(data, datalock.FileName))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = run([]string{"gc", "-config", cfgPath, "-wait", "300ms"}, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "a server is running; use the task 'compact-blobstore' or POST /-/api/gc") {
		t.Fatalf("gc while locked: %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("gc did not wait")
	}
	held.Release()
	var out bytes.Buffer
	if err := run([]string{"gc", "-config", cfgPath, "-wait", "1s"}, nil, &out); err != nil || !strings.Contains(out.String(), "removed 0") {
		t.Fatalf("gc after release: %q %v", out.String(), err)
	}
	// gc released the lock.
	l, err := datalock.TryAcquire(filepath.Join(data, datalock.FileName))
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
}

// Transient poll errors are retried; a server error that echoes the token
// is redacted before it is cut to length, so no part of the token leaks.
func TestBackupCLIRetriesAndRedacts(t *testing.T) {
	old := backupPoll
	backupPoll = time.Millisecond
	t.Cleanup(func() { backupPoll = old })
	var polls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/-/api/backup":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"backup-r"}`)
		case "/-/api/backup/runs/backup-r":
			polls++
			if polls <= 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, `{"status":"OK","message":"done"}`)
		}
	}))
	defer ts.Close()
	out, err := runBackupCLI(t, "--url", ts.URL, "--token", secretToken, "--wait")
	if err != nil || polls != 4 || !strings.Contains(out, "done") {
		t.Fatalf("retry: %q %v polls=%d", out, err, polls)
	}
	polls = -10 // more than three failures in a row fail
	if _, err := runBackupCLI(t, "--url", ts.URL, "--token", secretToken, "--wait"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("persistent poll errors: %v", err)
	}

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"%s%s"}`, strings.Repeat("x", 290), r.Header.Get("Authorization"))
	}))
	defer echo.Close()
	_, err = runBackupCLI(t, "--url", echo.URL, "--token", secretToken)
	if err == nil || strings.Contains(err.Error(), secretToken[:12]) {
		t.Fatalf("token fragment leaked: %v", err)
	}
}

// A plain-http URL to a non-loopback host sends the token in the clear: the
// CLI warns on stderr (and still runs); loopback and https do not warn.
func TestBackupCLIWarnsOnPlainHTTP(t *testing.T) {
	cases := map[string]bool{
		"http://backup.example.com:8081": true,
		"http://10.0.0.5:8081":           true,
		"http://[2001:db8::1]:8081":      true,
		"http://localhost:8081":          false,
		"http://127.0.0.1:8081":          false,
		"http://127.1.2.3":               false,
		"http://[::1]:8081":              false,
		"http://app.localhost:8081":      false,
		"https://backup.example.com":     false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		warnPlainHTTP(u, &buf)
		got := buf.Len() > 0
		if got != want || (got && (!strings.Contains(buf.String(), "warning") || strings.Contains(buf.String(), "cc_"))) {
			t.Errorf("%s: warned=%v (%q), want %v", raw, got, buf.String(), want)
		}
	}
	// The command itself warns before it talks to the server.
	old := stderr
	var errBuf bytes.Buffer
	stderr = &errBuf
	t.Cleanup(func() { stderr = old })
	f := &fakeBackupServer{start: http.StatusAccepted, final: "OK"}
	ts := httptest.NewServer(f) // 127.0.0.1: no warning
	defer ts.Close()
	if _, err := runBackupCLI(t, "--url", ts.URL, "--token", secretToken); err != nil {
		t.Fatal(err)
	}
	if errBuf.Len() != 0 {
		t.Fatalf("warned for loopback: %q", errBuf.String())
	}
}
