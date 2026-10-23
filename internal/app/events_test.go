package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/logring"
)

func do(t *testing.T, method, url, user, body string, ui bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, "pw")
	}
	if ui {
		req.Header.Set("X-Command-Center-UI", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Events flow from the real server into the database, including the UI's
// component delete (a plain Maven DELETE with the UI header), and the writer is
// drained by Close before the database goes away.
func TestEventsEndToEnd(t *testing.T) {
	cfg := testConfig(t, "")
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = append(cfg.Users, config.User{Name: "root", PasswordHash: string(h), Roles: []string{"admin"}})
	ring := logring.NewRing(100, 1<<20)
	log := slog.New(logring.NewHandler(slog.NewTextHandler(io.Discard, nil), ring))
	a, err := New(cfg, log, WithLogRing(ring))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	const jar = "/repository/maven-releases/org/example/app/1.0/app-1.0.jar"

	if st, body := do(t, "PUT", ts.URL+jar, "ci", "jar", false); st != 201 {
		t.Fatalf("PUT: %d %s", st, body)
	}
	if st, body := do(t, "PUT", ts.URL+jar+".sha1", "ci", "x", false); st != 201 {
		t.Fatalf("PUT sha1: %d %s", st, body)
	}
	if st, body := do(t, "DELETE", ts.URL+jar, "ci", "", true); st != 204 {
		t.Fatalf("DELETE: %d %s", st, body)
	}
	// An anonymous or unauthorized write emits nothing.
	do(t, "PUT", ts.URL+jar, "", "jar", false)

	// The admin API sees the events once the writer got to them.
	var list string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		_, list = do(t, "GET", ts.URL+"/-/api/events", "root", "", false)
		if strings.Contains(list, `"kind":"delete"`) {
			break
		}
	}
	for _, want := range []string{`"kind":"upload"`, `"kind":"delete"`, `"actor":"ci"`, `"repo":"maven-releases"`,
		`"subject":"org/example/app/1.0/app-1.0.jar"`} {
		if !strings.Contains(list, want) {
			t.Fatalf("events list lacks %s: %s", want, list)
		}
	}
	if strings.Count(list, `"kind":"upload"`) != 1 || strings.Count(list, `"kind":"delete"`) != 1 {
		t.Fatalf("want exactly one upload and one delete: %s", list)
	}

	// The server logger feeds the log viewer.
	log.Info("wired", "k", "v")
	_, logs := do(t, "GET", ts.URL+"/-/api/logs?q=wired", "root", "", false)
	if !strings.Contains(logs, `"message":"wired"`) || !strings.Contains(logs, `"attrs":"k=v"`) {
		t.Fatalf("logs: %s", logs)
	}

	a.Close()
}

func TestCloseDrainsEventsBeforeDBCloses(t *testing.T) {
	a, err := New(testConfig(t, ""), quiet())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		a.Events.Emit("upload", "ci", "maven-releases", "x", "")
	}
	// Close must write everything queued before the database is closed; a
	// failed insert would count as dropped.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.Events.Dropped() != 0 {
		t.Fatalf("dropped %d events on close", a.Events.Dropped())
	}
}
