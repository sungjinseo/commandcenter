package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// newMavenServer serves a real hosted Maven repository with sessions enabled.
func newMavenServer(t *testing.T) (*httptest.Server, *auth.Sessions) {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := &config.Config{
		AnonymousRead: true,
		Users:         []config.User{{Name: "ci", PasswordHash: string(h), Roles: []string{"deployer"}}},
		Roles:         map[string]config.Role{"deployer": {Write: []string{"releases"}}},
		Repositories: []config.Repository{
			{Name: "releases", Format: "maven", Type: "hosted", VersionPolicy: "release", WritePolicy: "allow"},
		},
	}
	dir := t.TempDir()
	b, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: time.Now}
	authn := auth.New(cfg, db)
	authn.Sessions = auth.NewSessions()
	s := &Server{
		Registry: repo.NewRegistry(nil, cfg.Repositories),
		Handlers: map[string]repo.Handler{"maven": maven.New(store, &resolve.Resolver{Store: store, Fetcher: proxy.New(store, http.DefaultClient, 4)})},
		Auth:     authn,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, authn.Sessions
}

func send(t *testing.T, method, url, body string, mod func(*http.Request)) *http.Response {
	t.Helper()
	r, _ := http.NewRequest(method, url, strings.NewReader(body))
	if mod != nil {
		mod(r)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res
}

func basic(r *http.Request) { r.SetBasicAuth("ci", "pw") }

const jarPath = "/repository/releases/org/example/app/1.0/app-1.0.jar"

func TestRepositoryContentIsSandboxed(t *testing.T) {
	ts, _ := newMavenServer(t)
	if res := send(t, "PUT", ts.URL+jarPath, "<html><script>alert(1)</script>", basic); res.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status %d", res.StatusCode)
	}
	for _, c := range []struct {
		name, path string
		want       int
	}{
		{"stored asset", jarPath, http.StatusOK},
		{"missing asset", "/repository/releases/nope/missing.jar", http.StatusNotFound},
		{"missing repository", "/repository/ghost/x", http.StatusNotFound},
	} {
		res := send(t, "GET", ts.URL+c.path, "", nil)
		if res.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, res.StatusCode, c.want)
		}
		if got := res.Header.Get("Content-Security-Policy"); got != "sandbox allow-downloads; default-src 'none'" {
			t.Errorf("%s: Content-Security-Policy = %q", c.name, got)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", c.name, got)
		}
	}
}

func TestRepositoryCachePrivateForAuthenticated(t *testing.T) {
	ts, _ := newMavenServer(t)
	send(t, "PUT", ts.URL+jarPath, "x", basic)
	if got := send(t, "GET", ts.URL+jarPath, "", basic).Header.Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Errorf("authenticated Cache-Control = %q, want private", got)
	}
	if got := send(t, "GET", ts.URL+jarPath, "", nil).Header.Get("Cache-Control"); strings.Contains(got, "private") {
		t.Errorf("anonymous Cache-Control = %q, must not be private", got)
	}
}

// Decision 7: a session cookie deletes Maven assets only with the UI header.
func TestMavenDeleteWithSession(t *testing.T) {
	ts, ses := newMavenServer(t)
	cookie := &http.Cookie{Name: auth.SessionCookie, Value: ses.Create("ci")}
	put := func() {
		if res := send(t, "PUT", ts.URL+jarPath, "x", basic); res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
			t.Fatalf("PUT status %d", res.StatusCode)
		}
	}
	put()
	res := send(t, "DELETE", ts.URL+jarPath, "", func(r *http.Request) { r.AddCookie(cookie) })
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie only: status %d, want 401", res.StatusCode)
	}
	res = send(t, "DELETE", ts.URL+jarPath, "", func(r *http.Request) {
		r.AddCookie(cookie)
		r.Header.Set(auth.UIHeader, "1")
	})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("cookie and UI header: status %d, want 204", res.StatusCode)
	}
	if res := send(t, "GET", ts.URL+jarPath, "", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("asset still there: %d", res.StatusCode)
	}
}
