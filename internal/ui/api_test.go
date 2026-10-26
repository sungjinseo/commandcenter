package ui

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/config"
	"commandcenter/internal/format/cargo"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/server"
	"commandcenter/internal/tasks"
)

type env struct {
	ts  *httptest.Server
	db  *meta.DB
	ui  *UI
	ses *auth.Sessions
	srv *server.Server
}

func newEnv(t *testing.T, baseURL string) *env { return newEnvWith(t, baseURL, nil) }

// newEnvWith builds the test environment; mutate may adjust the config first.
func newEnvWith(t *testing.T, baseURL string, mutate func(*config.Config)) *env {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	yes := true
	cfg := &config.Config{
		BaseURL:       baseURL,
		AnonymousRead: false,
		Users: []config.User{
			{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}},
			{Name: "ci", PasswordHash: string(h), Roles: []string{"deployer"}},
			{Name: "reader", PasswordHash: string(h)},
		},
		Roles: map[string]config.Role{"deployer": {Write: []string{"maven-releases"}}},
		Repositories: []config.Repository{
			{Name: "npm-hosted", Format: "npm", Type: "hosted", AnonymousRead: &yes},
			{Name: "maven-releases", Format: "maven", Type: "hosted"},
			{Name: "maven-proxy", Format: "maven", Type: "proxy", Remote: "https://u:secret@repo1.example.org/maven2?token=x#frag"},
			{Name: "maven-public", Format: "maven", Type: "group",
				Members: []string{"maven-releases", "maven-proxy"}, WriteMember: "maven-releases"},
			{Name: "npm-private", Format: "npm", Type: "hosted"},
			{Name: "npm-public", Format: "npm", Type: "group", AnonymousRead: &yes,
				Members: []string{"npm-hosted", "npm-private"}, WriteMember: "npm-private"},
			{Name: "pypi-hosted", Format: "pypi", Type: "hosted"},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seed(t, db)

	// Users, roles and settings live in the database, as in production
	// (auth.Open); Source lets the API reload the snapshot after a write.
	if _, err := db.ImportAccess(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	authn := auth.New(cfg, db)
	authn.Source = db
	authn.Sessions = auth.NewSessions()
	if _, err := db.ImportRepositories(context.Background(), cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	repos := repo.NewRegistry(db, cfg.Repositories)
	repos.Policies = cleanup.PolicyLookup(db)
	authn.AnonymousRead = repos.AnonymousRead
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	taskSvc := &tasks.Service{DB: db, Repos: repos, Log: log}
	if err := taskSvc.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	webUI := &UI{Repos: repos, Auth: authn, DB: db, Version: "test",
		Started: time.Now(), Log: log, Config: cfg, DataDir: t.TempDir(), Tasks: taskSvc}
	s := &server.Server{
		Registry: repos,
		Handlers: map[string]repo.Handler{},
		Auth:     authn,
		Log:      log,
		UI:       webUI,
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &env{ts: ts, db: db, ses: authn.Sessions, ui: webUI, srv: s}
}

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func seed(t *testing.T, db *meta.DB) {
	t.Helper()
	ctx := context.Background()
	put := func(r, p string, size int64) {
		err := db.PutAsset(ctx, meta.Asset{Repo: r, Path: p, BlobSHA256: "sha256-" + p, Size: size,
			ContentType: "application/java-archive", SHA1: "sha1-" + p, MD5: "md5-" + p, SHA512: "sha512-" + p,
			CreatedAt: t0, LastModified: t0.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	put("maven-releases", "org/example/app/1.0/app-1.0.jar", 3)
	put("maven-releases", "org/example/app/1.0/app-1.0.pom", 5)
	put("maven-releases", "org/example/app/maven-metadata.xml", 7)
	put("maven-releases", "org/example/pad-util/1.0/pad-util-1.0.jar", 11)
	put("maven-releases", "readme.txt", 13)
	put("npm-hosted", "left-pad/-/left-pad-1.0.0.tgz", 17)
	err := db.Tx(ctx, func(tx *meta.Tx) error {
		for _, p := range []meta.Package{
			{Repo: "npm-hosted", Format: "npm", Name: "left-pad", Version: "1.0.0", Attrs: "{}", CreatedAt: t0},
			{Repo: "npm-hosted", Format: "npm", Name: "left-pad", Version: "1.1.0", Attrs: "{}", CreatedAt: t0.Add(time.Hour), Yanked: true},
			{Repo: "npm-hosted", Format: "npm", Name: "@scope/util", Version: "2.0.0", Attrs: "{}", CreatedAt: t0},
			{Repo: "pypi-hosted", Format: "pypi", Name: "my-pkg", Version: "my_pkg-1.0.tar.gz", Attrs: "{}", CreatedAt: t0},
		} {
			if err := tx.InsertPackage(ctx, p); err != nil {
				return err
			}
		}
		return tx.SetDistTag(ctx, "npm-hosted", "left-pad", "latest", "1.1.0")
	})
	if err != nil {
		t.Fatal(err)
	}
}

type resp struct {
	status int
	header http.Header
	body   string
	cookie []*http.Cookie
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return m
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// req sends a request. user "" is anonymous; "basic:NAME" uses basic auth;
// a non-nil cookie is sent as the session cookie.
func (e *env) req(t *testing.T, method, path, body, user string, cookie *http.Cookie, ui bool) resp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, _ := http.NewRequest(method, e.ts.URL+path, rd)
	if user != "" {
		r.SetBasicAuth(user, "pw")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if ui {
		r.Header.Set(auth.UIHeader, "1")
	}
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: string(b), cookie: res.Cookies()}
}

func (e *env) get(t *testing.T, path, user string) resp {
	t.Helper()
	return e.req(t, "GET", path, "", user, nil, false)
}

func wantStatus(t *testing.T, r resp, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d: %s", r.status, want, r.body)
	}
}

func checkAPIHeaders(t *testing.T, r resp) {
	t.Helper()
	for k, v := range map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
	} {
		if got := r.header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestRootRedirect(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, "/", "")
	wantStatus(t, r, http.StatusFound)
	if loc := r.header.Get("Location"); loc != "/ui/" {
		t.Fatalf("Location %q", loc)
	}
}

func TestUnknownPathsStill404(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, p := range []string{"/nope", "/-/nope", "/service/rest/v1/nope", "/index.html"} {
		if r := e.get(t, p, ""); r.status != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, r.status)
		}
	}
	if r := e.req(t, "POST", "/", "", "", nil, false); r.status != http.StatusNotFound {
		t.Errorf("POST /: %d", r.status)
	}
	// existing routes are untouched
	wantStatus(t, e.get(t, "/service/rest/v1/status", ""), http.StatusOK)
	// as a user who could read it: anonymous read is off, so an anonymous
	// caller gets the 401 of an unreadable repository
	if r := e.get(t, "/repository/missing/x", "reader"); r.status != http.StatusNotFound {
		t.Errorf("unknown repository: %d", r.status)
	}
	// unknown API paths answer JSON 404; a wrong method answers 405
	r := e.get(t, "/-/api/nope", "")
	wantStatus(t, r, http.StatusNotFound)
	checkAPIHeaders(t, r)
	if r.json(t)["error"] == "" {
		t.Fatalf("no error: %s", r.body)
	}
	r = e.get(t, "/-/api/login", "")
	wantStatus(t, r, http.StatusMethodNotAllowed)
	if !strings.Contains(r.header.Get("Content-Type"), "application/json") {
		t.Fatalf("405 content type %q", r.header.Get("Content-Type"))
	}
}

func TestStaticUI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, "/ui/", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") || !strings.Contains(r.body, "<title>Command Center</title>") {
		t.Fatalf("index: %q %q", r.header.Get("Content-Type"), r.body)
	}
	for k, v := range map[string]string{
		"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	} {
		if got := r.header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	r = e.get(t, "/ui/app.css", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/css") {
		t.Fatalf("css type %q", r.header.Get("Content-Type"))
	}
	r = e.get(t, "/ui/app.js", "")
	wantStatus(t, r, http.StatusOK)
	// typos fall back to the SPA shell
	r = e.get(t, "/ui/some/typo", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, "<title>Command Center</title>") || r.header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("fallback: %q", r.body)
	}
}

func login(t *testing.T, e *env, user string) *http.Cookie {
	t.Helper()
	r := e.req(t, "POST", "/-/api/login", `{"user":"`+user+`","password":"pw"}`, "", nil, true)
	wantStatus(t, r, http.StatusOK)
	for _, c := range r.cookie {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie: %v", r.header)
	return nil
}

func TestMe(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, "/-/api/me", "")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	if ct := r.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type %q", ct)
	}
	if got := r.json(t); !reflect.DeepEqual(got, map[string]any{"user": "", "admin": false, "anonymous": true, "defaultLanguage": "ko"}) {
		t.Fatalf("anonymous me: %v", got)
	}
	c := login(t, e, "admin")
	r = e.req(t, "GET", "/-/api/me", "", "", c, false)
	if got := r.json(t); !reflect.DeepEqual(got, map[string]any{"user": "admin", "admin": true, "anonymous": false, "roles": []any{"admin"}, "owner": "admin", "realm": "Local", "version": "test", "defaultLanguage": "ko"}) {
		t.Fatalf("session me: %v", got)
	}
	c = login(t, e, "reader")
	r = e.req(t, "GET", "/-/api/me", "", "", c, false)
	if got := r.json(t); !reflect.DeepEqual(got, map[string]any{"user": "reader", "admin": false, "anonymous": false, "roles": []any{}, "owner": "reader", "realm": "Local", "version": "test", "defaultLanguage": "ko"}) {
		t.Fatalf("roleless me: %v", got)
	}
	c = login(t, e, "ci")
	r = e.req(t, "GET", "/-/api/me", "", "", c, false)
	if got := r.json(t); !reflect.DeepEqual(got["roles"], []any{"deployer"}) {
		t.Fatalf("ci me: %v", got)
	}
}

func TestLogin(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.req(t, "POST", "/-/api/login", `{"user":"ci","password":"pw"}`, "", nil, true)
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	if got := r.json(t); !reflect.DeepEqual(got, map[string]any{"user": "ci", "admin": false}) {
		t.Fatalf("login body: %v", got)
	}
	raw := r.header.Get("Set-Cookie")
	for _, want := range []string{auth.SessionCookie + "=", "Path=/", "HttpOnly", "SameSite=Strict", "Max-Age=604800"} {
		if !strings.Contains(raw, want) {
			t.Errorf("cookie %q lacks %q", raw, want)
		}
	}
	if strings.Contains(raw, "Secure") {
		t.Errorf("cookie %q is Secure on http", raw)
	}
	// The https name and the legacy name are expired (a __Host- cookie
	// only counts with Secure, so its expiry is Secure even on http).
	if len(r.cookie) != 3 || r.cookie[1].Name != auth.HostSessionCookie || r.cookie[1].MaxAge >= 0 || !r.cookie[1].Secure ||
		r.cookie[2].Name != "repolite_session" || r.cookie[2].MaxAge >= 0 || r.cookie[2].Path != "/" {
		t.Errorf("other session cookies not expired on login: %v", r.header.Values("Set-Cookie"))
	}
	c := r.cookie[0]
	if len(c.Value) != 64 {
		t.Errorf("session id %q", c.Value)
	}
	if u, ok := e.ses.User(c.Value); !ok || u != "ci" {
		t.Errorf("session user %q %v", u, ok)
	}

	r = e.req(t, "POST", "/-/api/login", `{"user":"ci","password":"wrong"}`, "", nil, true)
	wantStatus(t, r, http.StatusUnauthorized)
	if r.json(t)["error"] == "" || len(r.cookie) != 0 {
		t.Fatalf("wrong password: %s %v", r.body, r.cookie)
	}
	r = e.req(t, "POST", "/-/api/login", `{"user":"nobody","password":"pw"}`, "", nil, true)
	wantStatus(t, r, http.StatusUnauthorized)

	// the UI header is required (403, documented choice)
	r = e.req(t, "POST", "/-/api/login", `{"user":"ci","password":"pw"}`, "", nil, false)
	wantStatus(t, r, http.StatusForbidden)
	if r.json(t)["error"] == "" || len(r.cookie) != 0 {
		t.Fatalf("no header: %s", r.body)
	}
	// strict JSON
	wantStatus(t, e.req(t, "POST", "/-/api/login", `{"user":"ci","password":"pw","x":1}`, "", nil, true), http.StatusBadRequest)
	wantStatus(t, e.req(t, "POST", "/-/api/login", `not json`, "", nil, true), http.StatusBadRequest)
	big := `{"user":"ci","password":"` + strings.Repeat("a", 70<<10) + `"}`
	if r := e.req(t, "POST", "/-/api/login", big, "", nil, true); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", r.status)
	}
}

// Under an https baseURL the session cookie is __Host-cc_session: Secure,
// Path=/ and no Domain, so another host under the same domain cannot plant
// or shadow it. The http name is expired and does not authenticate.
// QA L-1: after repeated failures the UI login, Basic auth and the
// repository routes answer 429 with Retry-After for that name, even with
// the right password; other users are not affected.
func TestLoginThrottled(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for i := 0; i < 5; i++ {
		wantStatus(t, e.req(t, "POST", "/-/api/login", `{"user":"ci","password":"wrong"}`, "", nil, true), http.StatusUnauthorized)
	}
	r := e.req(t, "POST", "/-/api/login", `{"user":"ci","password":"pw"}`, "", nil, true)
	wantStatus(t, r, http.StatusTooManyRequests)
	if s, err := strconv.Atoi(r.header.Get("Retry-After")); err != nil || s < 1 || s > 60 || r.json(t)["retryAfter"] != float64(s) {
		t.Fatalf("Retry-After %q, body %s", r.header.Get("Retry-After"), r.body)
	}
	if len(r.cookie) != 0 {
		t.Fatalf("a refused login set cookies: %v", r.cookie)
	}
	for _, path := range []string{"/-/api/me", "/repository/maven-releases/org/x/1.0/x-1.0.pom"} {
		if r := e.get(t, path, "ci"); r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" ||
			r.header.Get("WWW-Authenticate") != "" {
			t.Fatalf("basic auth on %s: %d %v", path, r.status, r.header)
		}
	}
	// A session cannot guess the current password either.
	r = e.req(t, "PUT", "/-/api/account/password", `{"currentPassword":"pw","newPassword":"newpassword1"}`, "", e.cookieFor("ci"), true)
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Fatalf("password change: %d %s", r.status, r.body)
	}
	wantStatus(t, e.req(t, "POST", "/-/api/login", `{"user":"admin","password":"pw"}`, "", nil, true), http.StatusOK)
}

// The server keys the throttle by client address too: after 20 failures
// from one address it allows one failed attempt per name, but a name
// without failures still signs in.
func TestLoginThrottledPerAddress(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	login := func(user, pw string) int {
		return e.req(t, "POST", "/-/api/login", `{"user":"`+user+`","password":"`+pw+`"}`, "", nil, true).status
	}
	for i := 0; i < 20; i++ {
		login([]string{"ci", "nobody1", "nobody2", "nobody3"}[i/5], "wrong")
	}
	if st := login("reader", "wrong"); st != http.StatusUnauthorized {
		t.Fatalf("first failure of a name: %d", st)
	}
	if st := login("reader", "pw"); st != http.StatusTooManyRequests {
		t.Fatalf("second attempt from the busy address: %d", st)
	}
	if st := login("admin", "pw"); st != http.StatusOK {
		t.Fatalf("a name without failures: %d", st)
	}
}

func TestLoginSecureCookie(t *testing.T) {
	e := newEnv(t, "https://repo.example")
	r := e.req(t, "POST", "/-/api/login", `{"user":"ci","password":"pw"}`, "", nil, true)
	wantStatus(t, r, http.StatusOK)
	raw := r.header.Get("Set-Cookie")
	if !strings.HasPrefix(raw, auth.HostSessionCookie+"=") || !strings.Contains(raw, "Secure") ||
		!strings.Contains(raw, "Path=/") || strings.Contains(raw, "Domain") {
		t.Fatalf("session cookie %q", raw)
	}
	c := r.cookie[0]
	if len(r.cookie) != 3 || r.cookie[1].Name != auth.SessionCookie || r.cookie[1].MaxAge >= 0 || !r.cookie[1].Secure {
		t.Fatalf("http cookie not expired: %v", r.header.Values("Set-Cookie"))
	}
	if got := e.req(t, "GET", "/-/api/me", "", "", c, false).json(t)["user"]; got != "ci" {
		t.Fatalf("__Host- cookie: me %v", got)
	}
	if got := e.req(t, "GET", "/-/api/me", "", "", &http.Cookie{Name: auth.SessionCookie, Value: c.Value}, false).json(t)["anonymous"]; got != true {
		t.Fatal("the http cookie name authenticated under an https baseURL")
	}
}

// Login and logout end the session of every session cookie the request
// carries, under both names. So a live session another host planted beside
// the user's own, which makes the cookies refused (auth.cookiePrincipal), is
// gone once the user signs in again.
func TestLoginEndsEveryCookieSession(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	planted, own, stale := e.ses.Create("reader"), e.ses.Create("ci"), e.ses.Create("ci")
	send := func(method, path, body string, cookies ...*http.Cookie) resp {
		t.Helper()
		r, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
		r.Header.Set(auth.UIHeader, "1")
		for _, c := range cookies {
			r.AddCookie(c)
		}
		res, err := noRedirect.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return resp{status: res.StatusCode, header: res.Header, body: string(b), cookie: res.Cookies()}
	}
	plantedC := &http.Cookie{Name: auth.SessionCookie, Value: planted}
	if got := send("GET", "/-/api/me", "", plantedC, &http.Cookie{Name: auth.SessionCookie, Value: own}).json(t)["anonymous"]; got != true {
		t.Fatal("two users' sessions: one was picked")
	}
	r := send("POST", "/-/api/login", `{"user":"ci","password":"pw"}`, plantedC,
		&http.Cookie{Name: auth.SessionCookie, Value: own}, &http.Cookie{Name: auth.HostSessionCookie, Value: stale})
	wantStatus(t, r, http.StatusOK)
	for _, id := range []string{planted, own, stale} {
		if _, ok := e.ses.User(id); ok {
			t.Fatal("a session named by a cookie survived the login")
		}
	}
	// The planted cookie may linger in the browser; it names no session now.
	if got := send("GET", "/-/api/me", "", plantedC, r.cookie[0]).json(t)["user"]; got != "ci" {
		t.Fatalf("after signing in again: me %v", got)
	}
	wantStatus(t, send("POST", "/-/api/logout", "", plantedC, r.cookie[0]), http.StatusNoContent)
	if _, ok := e.ses.User(r.cookie[0].Value); ok {
		t.Fatal("session survived logout")
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	c := login(t, e, "reader")
	// without the UI header the cookie is not even accepted
	wantStatus(t, e.req(t, "POST", "/-/api/logout", "", "", c, false), http.StatusForbidden)
	if _, ok := e.ses.User(c.Value); !ok {
		t.Fatal("session deleted without the UI header")
	}
	r := e.req(t, "POST", "/-/api/logout", "", "", c, true)
	wantStatus(t, r, http.StatusNoContent)
	checkAPIHeaders(t, r)
	if len(r.cookie) != 3 || r.cookie[0].Name != auth.SessionCookie || r.cookie[0].MaxAge >= 0 ||
		r.cookie[1].Name != auth.HostSessionCookie || r.cookie[1].MaxAge >= 0 || !r.cookie[1].Secure {
		t.Fatalf("cookies not cleared: %v", r.header.Values("Set-Cookie"))
	}
	if lc := r.cookie[2]; lc.Name != "repolite_session" || lc.MaxAge >= 0 || lc.Path != "/" {
		t.Fatalf("legacy cookie not expired: %v", r.header.Values("Set-Cookie"))
	}
	if _, ok := e.ses.User(c.Value); ok {
		t.Fatal("session survived logout")
	}
	me := e.req(t, "GET", "/-/api/me", "", "", c, false).json(t)
	if me["anonymous"] != true {
		t.Fatalf("me after logout: %v", me)
	}
}

type repoView struct {
	Name        string   `json:"name"`
	Format      string   `json:"format"`
	Type        string   `json:"type"`
	URL         string   `json:"url"`
	Remote      string   `json:"remote"`
	Members     []string `json:"members"`
	WriteMember string   `json:"writeMember"`
	CanWrite    bool     `json:"canWrite"`
	Assets      int64    `json:"assets"`
	Bytes       int64    `json:"bytes"`
}

func repoList(t *testing.T, e *env, user string) map[string]repoView {
	t.Helper()
	r := e.get(t, "/-/api/repositories", user)
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	var body struct{ Repositories []repoView }
	if err := json.Unmarshal([]byte(r.body), &body); err != nil {
		t.Fatal(err)
	}
	m := map[string]repoView{}
	var names []string
	for _, rp := range body.Repositories {
		m[rp.Name] = rp
		names = append(names, rp.Name)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("not sorted: %v", names)
		}
	}
	return m
}

func TestRepositories(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	anon := repoList(t, e, "")
	if len(anon) != 2 || anon["npm-hosted"].Name == "" || anon["npm-public"].Name == "" {
		t.Fatalf("anonymous sees %v", anon)
	}
	npm := anon["npm-hosted"]
	if npm.Format != "npm" || npm.Type != "hosted" || npm.URL != "http://repo.example/repository/npm-hosted/" ||
		npm.Assets != 1 || npm.Bytes != 17 || npm.CanWrite || npm.Members == nil {
		t.Fatalf("npm-hosted: %+v", npm)
	}

	reader := repoList(t, e, "reader")
	if len(reader) != 7 {
		t.Fatalf("reader sees %v", reader)
	}
	if rel := reader["maven-releases"]; rel.Assets != 5 || rel.Bytes != 3+5+7+11+13 {
		t.Fatalf("maven-releases stats: %+v", rel)
	}
	pub := reader["maven-public"]
	if !reflect.DeepEqual(pub.Members, []string{"maven-releases", "maven-proxy"}) || pub.WriteMember != "maven-releases" || pub.CanWrite {
		t.Fatalf("group: %+v", pub)
	}
	if reader["maven-proxy"].Remote != "" {
		t.Fatalf("remote shown to non-admin: %q", reader["maven-proxy"].Remote)
	}

	ci := repoList(t, e, "ci")
	if !ci["maven-public"].CanWrite || !ci["maven-releases"].CanWrite || ci["maven-proxy"].CanWrite || ci["npm-hosted"].CanWrite {
		t.Fatalf("ci canWrite: %+v", ci)
	}

	admin := repoList(t, e, "admin")
	if got := admin["maven-proxy"].Remote; got != "https://repo1.example.org/maven2/" {
		t.Fatalf("admin remote %q", got)
	}
	if b := e.get(t, "/-/api/repositories", "admin").body; strings.Contains(b, "secret") || strings.Contains(b, "token=x") || strings.Contains(b, "frag") {
		t.Fatal("remote credentials leaked")
	}
}

func TestRepositoryDirect(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, "/-/api/repositories/maven-releases", "")
	wantStatus(t, r, http.StatusNotFound)
	checkAPIHeaders(t, r)
	nf := r.body
	r = e.get(t, "/-/api/repositories/does-not-exist", "")
	wantStatus(t, r, http.StatusNotFound)
	if r.body != strings.Replace(nf, "maven-releases", "does-not-exist", 1) {
		t.Fatalf("hidden and missing repos answer differently: %q vs %q", nf, r.body)
	}
	r = e.get(t, "/-/api/repositories/npm-hosted", "")
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["name"] != "npm-hosted" {
		t.Fatalf("direct: %s", r.body)
	}
	r = e.get(t, "/-/api/repositories/maven-proxy", "admin")
	if r.json(t)["remote"] != "https://repo1.example.org/maven2/" {
		t.Fatalf("direct remote: %s", r.body)
	}
}

type browseJSON struct {
	Path    string `json:"path"`
	Entries []struct {
		Name         string  `json:"name"`
		Dir          bool    `json:"dir"`
		Size         *int64  `json:"size"`
		LastModified *string `json:"lastModified"`
	} `json:"entries"`
	Truncated bool `json:"truncated"`
}

func browse(t *testing.T, e *env, repoName, path, user string) (resp, browseJSON) {
	t.Helper()
	r := e.get(t, "/-/api/repositories/"+repoName+"/browse?path="+url.QueryEscape(path), user)
	var b browseJSON
	if r.status == http.StatusOK {
		if err := json.Unmarshal([]byte(r.body), &b); err != nil {
			t.Fatal(err)
		}
	}
	return r, b
}

func TestBrowse(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r, b := browse(t, e, "maven-releases", "", "reader")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	if b.Path != "" || b.Truncated || len(b.Entries) != 2 ||
		b.Entries[0].Name != "org" || !b.Entries[0].Dir || b.Entries[0].Size != nil ||
		b.Entries[1].Name != "readme.txt" || b.Entries[1].Dir || b.Entries[1].Size == nil || *b.Entries[1].Size != 13 ||
		b.Entries[1].LastModified == nil || *b.Entries[1].LastModified != "2026-10-01T13:00:00Z" {
		t.Fatalf("root: %s", r.body)
	}
	r, b = browse(t, e, "maven-releases", "org/example/app/", "reader")
	wantStatus(t, r, http.StatusOK)
	if b.Path != "org/example/app/" || len(b.Entries) != 2 || b.Entries[0].Name != "1.0" || !b.Entries[0].Dir ||
		b.Entries[1].Name != "maven-metadata.xml" || *b.Entries[1].Size != 7 {
		t.Fatalf("nested: %s", r.body)
	}
	r, b = browse(t, e, "maven-releases", "nothing/", "reader")
	wantStatus(t, r, http.StatusOK)
	if b.Entries == nil || len(b.Entries) != 0 || !strings.Contains(r.body, `"entries":[]`) {
		t.Fatalf("empty dir: %s", r.body)
	}
	for _, bad := range []string{"org", "/org/", "org/../", "../", "org//", "./", "org/./example/", "//"} {
		r, _ := browse(t, e, "maven-releases", bad, "reader")
		wantStatus(t, r, http.StatusBadRequest)
		if r.json(t)["error"] == "" {
			t.Fatalf("bad path %q: %s", bad, r.body)
		}
	}
	r, _ = browse(t, e, "maven-releases", "", "")
	wantStatus(t, r, http.StatusNotFound)
	r, _ = browse(t, e, "npm-hosted", "", "")
	wantStatus(t, r, http.StatusOK)
}

func TestAsset(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	p := "org/example/app/1.0/app-1.0.jar"
	r := e.get(t, "/-/api/repositories/maven-releases/asset?path="+url.QueryEscape(p), "reader")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	want := map[string]any{
		"path": p, "size": float64(3), "contentType": "application/java-archive",
		"sha1": "sha1-" + p, "md5": "md5-" + p, "sha256": "sha256-" + p, "sha512": "sha512-" + p,
		"created": "2026-10-01T12:00:00Z", "lastModified": "2026-10-01T13:00:00Z", "checked": "", "downloads": float64(0), "lastDownloaded": "",
		"downloadUrl": "http://repo.example/repository/maven-releases/" + p, "canDownload": true,
	}
	if got := r.json(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("asset:\n got %v\nwant %v", got, want)
	}
	r = e.get(t, "/-/api/repositories/maven-releases/asset?path=org/missing.jar", "reader")
	wantStatus(t, r, http.StatusNotFound)
	if r.json(t)["error"] == "" {
		t.Fatalf("missing asset: %s", r.body)
	}
	for _, bad := range []string{"", "org/", "../x", "/readme.txt", "a//b"} {
		r = e.get(t, "/-/api/repositories/maven-releases/asset?path="+url.QueryEscape(bad), "reader")
		wantStatus(t, r, http.StatusBadRequest)
	}
	wantStatus(t, e.get(t, "/-/api/repositories/maven-releases/asset?path="+url.QueryEscape(p), ""), http.StatusNotFound)
}

func TestPackages(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, "/-/api/repositories/npm-hosted/packages", "")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	if got := r.json(t); !reflect.DeepEqual(got, map[string]any{"packages": []any{"@scope/util", "left-pad"}, "truncated": false}) {
		t.Fatalf("packages: %v", got)
	}
	r = e.get(t, "/-/api/repositories/npm-hosted/packages?q=PAD", "")
	if got := r.json(t)["packages"]; !reflect.DeepEqual(got, []any{"left-pad"}) {
		t.Fatalf("filtered: %v", got)
	}
	r = e.get(t, "/-/api/repositories/maven-releases/packages", "reader")
	wantStatus(t, r, http.StatusOK)
	if r.body != `{"packages":[],"truncated":false}`+"\n" {
		t.Fatalf("maven packages: %q", r.body)
	}

	r = e.get(t, "/-/api/repositories/npm-hosted/packages/left-pad", "")
	wantStatus(t, r, http.StatusOK)
	checkAPIHeaders(t, r)
	want := map[string]any{
		"name": "left-pad",
		"versions": []any{
			map[string]any{"version": "1.1.0", "created": "2026-10-01T13:00:00Z", "yanked": true},
			map[string]any{"version": "1.0.0", "created": "2026-10-01T12:00:00Z", "yanked": false},
		},
		"tags":      map[string]any{"latest": "1.1.0"},
		"truncated": false,
		"kind":      "versions",
	}
	if got := r.json(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("versions:\n got %v\nwant %v", got, want)
	}
	r = e.get(t, "/-/api/repositories/npm-hosted/packages/@scope/util", "")
	wantStatus(t, r, http.StatusOK)
	if got := r.json(t); got["name"] != "@scope/util" || !reflect.DeepEqual(got["tags"], map[string]any{}) {
		t.Fatalf("scoped: %v", got)
	}
	r = e.get(t, "/-/api/repositories/npm-hosted/packages/%40scope%2Futil", "")
	wantStatus(t, r, http.StatusOK)
	wantStatus(t, e.get(t, "/-/api/repositories/npm-hosted/packages/nope", ""), http.StatusNotFound)
	wantStatus(t, e.get(t, "/-/api/repositories/maven-releases/packages", ""), http.StatusNotFound)
}

type searchJSON struct {
	Results []struct {
		Repo   string `json:"repo"`
		Format string `json:"format"`
		Path   string `json:"path"`
	} `json:"results"`
	Truncated bool `json:"truncated"`
}

func search(t *testing.T, e *env, query, user string) (resp, searchJSON) {
	t.Helper()
	r := e.get(t, "/-/api/search?"+query, user)
	var s searchJSON
	if r.status == http.StatusOK {
		if err := json.Unmarshal([]byte(r.body), &s); err != nil {
			t.Fatal(err)
		}
	}
	return r, s
}

func TestSearch(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, q := range []string{"", "q=", "q=a", "q=%ED%95%9C"} {
		r, _ := search(t, e, q, "reader")
		wantStatus(t, r, http.StatusBadRequest)
		checkAPIHeaders(t, r)
	}
	r, s := search(t, e, "q=PAD", "reader")
	wantStatus(t, r, http.StatusOK)
	if s.Truncated || len(s.Results) != 2 ||
		s.Results[0].Repo != "maven-releases" || s.Results[0].Format != "maven" || s.Results[0].Path != "org/example/pad-util/1.0/pad-util-1.0.jar" ||
		s.Results[1].Repo != "npm-hosted" || s.Results[1].Format != "npm" || s.Results[1].Path != "left-pad/-/left-pad-1.0.0.tgz" {
		t.Fatalf("reader search: %s", r.body)
	}
	r, s = search(t, e, "q=pad", "")
	if len(s.Results) != 1 || s.Results[0].Repo != "npm-hosted" {
		t.Fatalf("anonymous search: %s", r.body)
	}
	r, s = search(t, e, "q=pad&repo=maven-releases", "reader")
	if len(s.Results) != 1 || s.Results[0].Repo != "maven-releases" {
		t.Fatalf("repo filter: %s", r.body)
	}
	r, _ = search(t, e, "q=pad&repo=maven-releases", "")
	wantStatus(t, r, http.StatusNotFound)
	r, s = search(t, e, "q=zzz", "reader")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"results":[]`) {
		t.Fatalf("empty search: %s", r.body)
	}
}

func TestPackagesPyPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.get(t, "/-/api/repositories/pypi-hosted/packages", "reader")
	if got := r.json(t)["packages"]; !reflect.DeepEqual(got, []any{"my-pkg"}) {
		t.Fatalf("pypi packages: %s", r.body)
	}
	for _, name := range []string{"my-pkg", "My_Pkg", "my.__pkg"} {
		r = e.get(t, "/-/api/repositories/pypi-hosted/packages/"+name, "reader")
		wantStatus(t, r, http.StatusOK)
		want := map[string]any{
			"name":      "my-pkg",
			"versions":  []any{map[string]any{"version": "my_pkg-1.0.tar.gz", "created": "2026-10-01T12:00:00Z", "yanked": false}},
			"tags":      map[string]any{},
			"truncated": false,
			"kind":      "files",
		}
		if got := r.json(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got %v\nwant %v", name, got, want)
		}
	}
	// npm names are not normalized
	wantStatus(t, e.get(t, "/-/api/repositories/npm-hosted/packages/Left-Pad", ""), http.StatusNotFound)
}

// TestPackagesCargoPublishedName: cargo rows and paths hold crate names
// lowercased; the API and component search answer with the published
// spelling (what `cargo add` needs) and find a crate in any case (QA L-20).
func TestPackagesCargoPublishedName(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "cargo-hosted", Format: "cargo", Type: "hosted"})
	})
	ctx := context.Background()
	err := e.db.Tx(ctx, func(tx *meta.Tx) error {
		for _, p := range []meta.Package{
			{Repo: "cargo-hosted", Format: "cargo", Name: "foo-bar", Version: "1.0.0", Attrs: `{"name":"Foo-Bar","vers":"1.0.0"}`, CreatedAt: t0},
			{Repo: "cargo-hosted", Format: "cargo", Name: "foo-baz", Version: "1.0.0", Attrs: `{"name":"other"}`, CreatedAt: t0},
		} {
			if err := tx.InsertPackage(ctx, p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	putAsset(t, e.db, "cargo-hosted", "crates/foo-bar/1.0.0.crate", 9)
	r := e.get(t, "/-/api/repositories/cargo-hosted/packages", "admin")
	// A spelling that is not the row's name in another case is ignored.
	if got := r.json(t)["packages"]; !reflect.DeepEqual(got, []any{"Foo-Bar", "foo-baz"}) {
		t.Fatalf("packages: %s", r.body)
	}
	for _, name := range []string{"Foo-Bar", "foo-bar", "FOO-BAR"} {
		r = e.get(t, "/-/api/repositories/cargo-hosted/packages/"+name, "admin")
		wantStatus(t, r, http.StatusOK)
		if got := r.json(t); got["name"] != "Foo-Bar" || len(got["versions"].([]any)) != 1 {
			t.Fatalf("%s: %s", name, r.body)
		}
	}
	c := searchComponents(t, e, "name=FOO-B&format=cargo", "admin", 200)
	if got, want := names(c), "cargo-hosted::Foo-Bar:1.0.0,cargo-hosted::foo-baz:1.0.0"; got != want {
		t.Fatalf("components = %s, want %s", got, want)
	}
	if a := c.Components[0].Assets; len(a) != 1 || a[0].Path != "crates/foo-bar/1.0.0.crate" {
		t.Fatalf("assets keep the stored path: %+v", a)
	}
}

func TestPackagesQueryCap(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.get(t, "/-/api/repositories/npm-hosted/packages?q="+strings.Repeat("a", 200), ""), http.StatusOK)
	r := e.get(t, "/-/api/repositories/npm-hosted/packages?q="+strings.Repeat("a", 201), "")
	wantStatus(t, r, http.StatusBadRequest)
	checkAPIHeaders(t, r)
}

func TestGroupHidesHiddenMembers(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	g := repoList(t, e, "")["npm-public"]
	if !reflect.DeepEqual(g.Members, []string{"npm-hosted"}) || g.WriteMember != "" {
		t.Fatalf("anonymous view of group: %+v", g)
	}
	if b := e.get(t, "/-/api/repositories/npm-public", "").body; strings.Contains(b, "npm-private") {
		t.Fatalf("hidden member leaked: %s", b)
	}
	g = repoList(t, e, "reader")["npm-public"]
	if !reflect.DeepEqual(g.Members, []string{"npm-hosted", "npm-private"}) || g.WriteMember != "npm-private" {
		t.Fatalf("reader view of group: %+v", g)
	}
}

func TestExistingMethodBehaviour(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.req(t, "PUT", "/", "x", "", nil, false), http.StatusNotFound)
	wantStatus(t, e.req(t, "POST", "/-/metrics", "", "admin", nil, false), http.StatusMethodNotAllowed)
}

func TestStaticNoListing(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, p := range []string{"/ui/../x", "/ui/%2e%2e/x", "/ui/static/", "/ui/static/index.html", "/ui/./", "/ui/a/../", "/ui/.."} {
		r, err := http.Get(e.ts.URL + p) // follows the mux's path-cleaning redirects
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		body := string(b)
		switch {
		case strings.Contains(body, "<pre>"):
			t.Errorf("%s: directory listing: %q", p, body)
		case r.StatusCode == http.StatusOK && !strings.Contains(body, "<title>Command Center</title>"):
			t.Errorf("%s: 200 but not index.html: %q", p, body)
		case r.StatusCode != http.StatusOK && r.StatusCode != http.StatusNotFound:
			t.Errorf("%s: status %d", p, r.StatusCode)
		}
	}
}

// A cargo asset's downloadUrl is its stored path (crates/<n>/<v>.crate); the
// cargo handler must serve it.
func TestAssetDownloadURLWorks(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "cargo-hosted", Format: "cargo", Type: "hosted"})
	})
	bs, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: e.db, Now: time.Now}
	e.srv.Handlers["cargo"] = cargo.New(store, nil, e.srv.Auth, func() string { return "http://repo.example" })
	sums, err := store.WriteBlob("cargo-hosted", strings.NewReader("crate bytes"), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := "crates/qa-crate/0.1.0.crate"
	if err := e.db.PutAsset(context.Background(), store.NewAsset("cargo-hosted", p, "application/octet-stream", sums)); err != nil {
		t.Fatal(err)
	}
	r := e.get(t, "/-/api/repositories/cargo-hosted/asset?path="+url.QueryEscape(p), "admin")
	wantStatus(t, r, http.StatusOK)
	dl, _ := r.json(t)["downloadUrl"].(string)
	if !strings.HasPrefix(dl, "http://repo.example/") {
		t.Fatalf("downloadUrl = %q", dl)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+strings.TrimPrefix(dl, "http://repo.example"), nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "crate bytes" {
		t.Fatalf("GET %s: %d %q", dl, resp.StatusCode, body)
	}
}
