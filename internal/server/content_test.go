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
	"commandcenter/internal/format/cargo"
	"commandcenter/internal/format/golang"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/format/npm"
	"commandcenter/internal/format/pypi"
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

// QA N-27: a read of a group whose members are all offline, also through a
// nested group, answers 503 in every format instead of a 404 that clients
// cache. A write or a public path (npm ping) keeps its own answer.
func TestGroupOfOfflineMembersAnswers503(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	off := false
	cfg := &config.Config{AnonymousRead: true, Users: []config.User{{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}}}}
	reads := map[string]string{
		"maven": "org/example/app/1.0/app-1.0.jar",
		"npm":   "left-pad",
		"pypi":  "simple/left-pad/",
		"go":    "example.com/m/@v/list",
		"cargo": "le/ft/left-pad",
	}
	for f := range reads {
		cfg.Repositories = append(cfg.Repositories,
			config.Repository{Name: f + "-off", Format: f, Type: "hosted", Online: &off},
			config.Repository{Name: f + "-on", Format: f, Type: "hosted"},
			config.Repository{Name: f + "-inner", Format: f, Type: "group", Members: []string{f + "-off"}},
			config.Repository{Name: f + "-all", Format: f, Type: "group", Members: []string{f + "-off", f + "-inner"}},
			config.Repository{Name: f + "-some", Format: f, Type: "group", Members: []string{f + "-inner", f + "-on"}},
		)
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
	fetcher := proxy.New(store, http.DefaultClient, 4)
	resolver := &resolve.Resolver{Store: store, Fetcher: fetcher}
	base := func() string { return "http://localhost" }
	s := &Server{
		Registry: repo.NewRegistry(nil, cfg.Repositories),
		Handlers: map[string]repo.Handler{"maven": maven.New(store, resolver), "npm": npm.New(store, resolver, authn, base),
			"pypi": pypi.New(store, fetcher, base), "go": golang.New(store, resolver), "cargo": cargo.New(store, fetcher, authn, base)},
		Auth: authn,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	get := func(path string) (int, string) {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	for f, p := range reads {
		if code, body := get("/repository/" + f + "-all/" + p); code != 503 || !strings.Contains(body, "repository is offline") {
			t.Errorf("%s: all members offline: %d %s, want 503", f, code, body)
		}
		// The handler answers. npm and PyPI fail closed with their own 503,
		// because the offline member comes before the online one.
		if code, body := get("/repository/" + f + "-some/" + p); strings.Contains(body, "repository is offline") {
			t.Errorf("%s: an online member: %d %s, want the handler's answer", f, code, body)
		}
	}
	if code, body := get("/repository/npm-all/-/ping"); code == 503 {
		t.Errorf("npm ping: %d %s, want the handler's answer", code, body)
	}
	put := send(t, "PUT", ts.URL+"/repository/maven-all/"+reads["maven"], "x", func(r *http.Request) { r.SetBasicAuth("admin", "pw") })
	if put.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("write to a group without writeMember: %d, want 405", put.StatusCode)
	}
}
