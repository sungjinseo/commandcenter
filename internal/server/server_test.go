package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

type noTokens struct{}

func (noTokens) CreateToken(context.Context, string, string, string, time.Time) error { return nil }
func (noTokens) TokenUser(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, io.EOF
}
func (noTokens) TouchToken(context.Context, string) {}

type recordHandler struct{ calls []string }

func (h *recordHandler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	h.calls = append(h.calls, r.Method+" "+rp.Name+" "+path)
	w.WriteHeader(http.StatusOK)
}

func newServer(t *testing.T) (*httptest.Server, *recordHandler) { return newServerWith(t, nil) }

func newServerWith(t *testing.T, tweak func(*Server)) (*httptest.Server, *recordHandler) {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := &config.Config{
		AnonymousRead: true,
		Users: []config.User{
			{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}},
			{Name: "ci", PasswordHash: string(h), Roles: []string{"deployer"}},
			{Name: "reader", PasswordHash: string(h)},
		},
		Roles: map[string]config.Role{"deployer": {Write: []string{"releases"}}},
		Repositories: []config.Repository{
			{Name: "releases", Format: "maven", Type: "hosted"},
			{Name: "public", Format: "maven", Type: "group", Members: []string{"releases"}, WriteMember: "releases"},
		},
	}
	rec := &recordHandler{}
	s := &Server{
		Registry: repo.NewRegistry(nil, cfg.Repositories),
		Handlers: map[string]repo.Handler{"maven": rec},
		Auth:     auth.New(cfg, noTokens{}),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(s)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, rec
}

func do(t *testing.T, method, url, user string) *http.Response {
	t.Helper()
	r, _ := http.NewRequest(method, url, nil)
	if user != "" {
		r.SetBasicAuth(user, "pw")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestRouting(t *testing.T) {
	ts, rec := newServer(t)
	if resp := do(t, "GET", ts.URL+"/repository/releases/g/a/1.0/a-1.0.jar", ""); resp.StatusCode != 200 {
		t.Fatalf("anon read: %d", resp.StatusCode)
	}
	if rec.calls[0] != "GET releases g/a/1.0/a-1.0.jar" {
		t.Fatalf("calls = %v", rec.calls)
	}
	if resp := do(t, "GET", ts.URL+"/repository/nope/x", ""); resp.StatusCode != 404 {
		t.Fatalf("unknown repo: %d", resp.StatusCode)
	}
}

func TestWriteAuthorization(t *testing.T) {
	ts, _ := newServer(t)
	resp := do(t, "PUT", ts.URL+"/repository/releases/x", "")
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("anon write: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := do(t, "PUT", ts.URL+"/repository/releases/x", "reader"); resp.StatusCode != 403 {
		t.Fatalf("reader write: %d", resp.StatusCode)
	}
	if resp := do(t, "PUT", ts.URL+"/repository/releases/x", "ci"); resp.StatusCode != 200 {
		t.Fatalf("ci write: %d", resp.StatusCode)
	}
	// group writes are authorized against the writeMember
	if resp := do(t, "PUT", ts.URL+"/repository/public/x", "ci"); resp.StatusCode != 200 {
		t.Fatalf("ci group write: %d", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/repository/releases/x", "ci-bad")
	if resp.StatusCode != 401 {
		t.Fatalf("bad credentials: %d", resp.StatusCode)
	}
}

// TestUIWriteNoChallenge: the web UI's upload (X-Command-Center-UI) gets a plain 401
// when its session is gone, so the browser shows no Basic-auth dialog.
func TestUIWriteNoChallenge(t *testing.T) {
	ts, _ := newServer(t)
	r, _ := http.NewRequest("PUT", ts.URL+"/repository/releases/x", nil)
	r.Header.Set("X-Command-Center-UI", "1")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != "" {
		t.Fatalf("UI anon write: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestStatusAndMetrics(t *testing.T) {
	ts, _ := newServer(t)
	if resp := do(t, "GET", ts.URL+"/service/rest/v1/status", ""); resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if resp := do(t, "GET", ts.URL+"/-/metrics", ""); resp.StatusCode != 401 {
		t.Fatalf("anon metrics: %d", resp.StatusCode)
	}
	if resp := do(t, "GET", ts.URL+"/-/metrics", "ci"); resp.StatusCode != 403 {
		t.Fatalf("ci metrics: %d", resp.StatusCode)
	}
	r, _ := http.NewRequest("GET", ts.URL+"/-/metrics", nil)
	r.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || m["heapAllocBytes"] == nil {
		t.Fatalf("metrics: %v %v", m, err)
	}
}

type jsonErrHandler struct{ recordHandler }

func (h *jsonErrHandler) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := repo.ErrorMessage(r, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// TestDenyUsesHandlerErrorWriter: a caller who may read the repository (here
// anonymously) gets a denied write in its format; one who may not read it
// gets the plain answer of a missing repository
// (TestMissingRepositoryAnswersLikeUnreadable).
func TestDenyUsesHandlerErrorWriter(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := &config.Config{
		AnonymousRead: true,
		Users:         []config.User{{Name: "reader", PasswordHash: string(h)}},
		Repositories:  []config.Repository{{Name: "n", Format: "npm", Type: "hosted"}},
	}
	s := &Server{
		Registry: repo.NewRegistry(nil, cfg.Repositories),
		Handlers: map[string]repo.Handler{"npm": &jsonErrHandler{}},
		Auth:     auth.New(cfg, noTokens{}),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/repository/n/pkg", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" || body["error"] == "" {
		t.Fatalf("status=%d header=%q body=%v", resp.StatusCode, resp.Header.Get("WWW-Authenticate"), body)
	}
}

func logServer(t *testing.T, buf *bytes.Buffer) *Server {
	t.Helper()
	cfg := &config.Config{AnonymousRead: true}
	return &Server{Auth: auth.New(cfg, noTokens{}), Log: slog.New(slog.NewTextHandler(buf, nil))}
}

func TestRequestLoggedOnAbortHandler(t *testing.T) {
	var buf bytes.Buffer
	s := logServer(t, &buf)
	h := s.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Error("panic must propagate")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/repository/x/big.bin", nil))
	}()
	if out := buf.String(); !strings.Contains(out, "msg=request") || !strings.Contains(out, "/repository/x/big.bin") || !strings.Contains(out, "bytes=7") {
		t.Fatalf("log = %q", out)
	}
}

func TestTokenPathRedactedInLog(t *testing.T) {
	var buf bytes.Buffer
	s := logServer(t, &buf)
	h := s.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("DELETE", "/repository/x/-/user/token/rl_secret", nil))
	out := buf.String()
	if strings.Contains(out, "rl_secret") || !strings.Contains(out, "/-/user/token/REDACTED") {
		t.Fatalf("log = %q", out)
	}
}

func TestStaleCredentialsDoNotBlockLogin(t *testing.T) {
	var buf bytes.Buffer
	s := logServer(t, &buf)
	h := s.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) }))
	serve := func(method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer rl_stale")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := serve("PUT", "/repository/npm-all/-/user/org.couchdb.user:ci"); c != 201 {
		t.Fatalf("login with stale token: %d", c)
	}
	if c := serve("GET", "/repository/npm-all/pkg"); c != 401 {
		t.Fatalf("stale token on other paths must 401: %d", c)
	}
	if c := serve("GET", "/repository/npm-all/-/user/org.couchdb.user:ci"); c != 401 {
		t.Fatalf("only PUT login is exempt: %d", c)
	}
}

type idHandler struct{ ids []string }

func (h *idHandler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	h.ids = append(h.ids, r.Header.Get("X-Request-Id"))
}

func TestRequestIDAndImplicitStatus(t *testing.T) {
	var logs bytes.Buffer
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{{Name: "r", Format: "maven", Type: "hosted"}}}
	h := &idHandler{}
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": h}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, id := range []string{"good-id.1", strings.Repeat("x", 65), "bad id"} {
		req, _ := http.NewRequest("GET", srv.URL+"/repository/r/x", nil)
		req.Header.Set("X-Request-Id", id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if h.ids[0] != "good-id.1" || len(h.ids[1]) != 16 || len(h.ids[2]) != 16 {
		t.Fatalf("ids = %q", h.ids)
	}
	if !strings.Contains(logs.String(), "status=200") || strings.Contains(logs.String(), "status=0") {
		t.Fatalf("logs = %s", logs.String())
	}
}

type readAllHandler struct{}

func (readAllHandler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	if _, err := io.ReadAll(r.Body); err != nil {
		repo.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func TestUploadCap(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := &config.Config{MaxUploadMiB: 1, Users: []config.User{{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}}},
		Repositories: []config.Repository{{Name: "r", Format: "maven", Type: "hosted"}}}
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": readAllHandler{}}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for body, want := range map[string]int{"small": 201, strings.Repeat("x", 2<<20): 413} { // the cap is the live maxUploadMiB setting: 1 MiB
		req, _ := http.NewRequest("PUT", srv.URL+"/repository/r/x", strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%d-byte body: %d, want %d", len(body), resp.StatusCode, want)
		}
	}
}

// TestUIBadCredentialsNoChallenge: invalid credentials from the web UI get a
// plain 401, while other clients still receive the Basic challenge.
func TestUIBadCredentialsNoChallenge(t *testing.T) {
	ts, _ := newServer(t)
	for _, tc := range []struct {
		ui        bool
		challenge bool
	}{{true, false}, {false, true}} {
		r, _ := http.NewRequest("GET", ts.URL+"/repository/releases/x", nil)
		r.SetBasicAuth("ci-bad", "pw")
		if tc.ui {
			r.Header.Set("X-Command-Center-UI", "1")
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("WWW-Authenticate") != ""; resp.StatusCode != 401 || got != tc.challenge {
			t.Fatalf("ui=%v: %d challenge=%v", tc.ui, resp.StatusCode, got)
		}
	}
}

func TestRequestStatsHook(t *testing.T) {
	type rec struct {
		repo  string
		bytes int64
	}
	var mu sync.Mutex
	var got []rec
	ts, _ := newServerWith(t, func(s *Server) {
		s.Handlers = map[string]repo.Handler{"maven": bodyHandler{}}
		s.Requests = func(repo string, bytes int64) {
			mu.Lock()
			got = append(got, rec{repo, bytes})
			mu.Unlock()
		}
	})
	do(t, "GET", ts.URL+"/repository/releases/a/b.jar", "")               // known repository
	do(t, "PUT", ts.URL+"/repository/releases/a/b.jar", "reader")         // denied (403): still a request
	do(t, "GET", ts.URL+"/repository/nope/a.jar", "")                     // unknown repository: not recorded
	do(t, "GET", ts.URL+"/repository/../x", "")                           // not a repository path
	do(t, "GET", ts.URL+"/service/rest/v1/status", "")                    // not a repository path
	do(t, "GET", ts.URL+"/repository/"+strings.Repeat("x", 300)+"/p", "") // scanner noise
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != (rec{"releases", 5}) || got[1].repo != "releases" || got[1].bytes == 0 {
		t.Fatalf("recorded %+v", got)
	}
}

// bodyHandler answers 200 with a 5-byte body.
type bodyHandler struct{}

func (bodyHandler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	w.Write([]byte("hello"))
}

func TestOfflineRepositoryAnswers503(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	off, yes := false, true
	cfg := &config.Config{Users: []config.User{
		{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}},
		{Name: "reader", PasswordHash: string(h)},
	},
		Repositories: []config.Repository{
			{Name: "r", Format: "maven", Type: "hosted", Online: &off},
			{Name: "pub", Format: "maven", Type: "hosted", Online: &off, AnonymousRead: &yes},
			{Name: "on", Format: "maven", Type: "hosted"},
		}}
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": readAllHandler{}}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, tc := range []struct {
		name, method, repo, user string
		want                     int
		offlineBody              bool
	}{
		{"authorized write to an offline repo", "PUT", "r", "admin", http.StatusServiceUnavailable, true},
		{"authorized read of an offline repo", "GET", "r", "admin", http.StatusServiceUnavailable, true},
		{"anonymous read of an offline anonymous-read repo", "GET", "pub", "", http.StatusServiceUnavailable, true},
		{"online repo", "PUT", "on", "admin", http.StatusCreated, false},
		// No offline leak: callers who may not use a private repository get
		// the normal answer, not 503.
		{"anonymous on a private offline repo", "GET", "r", "", http.StatusUnauthorized, false},
		{"user without write access to an offline repo", "PUT", "r", "reader", http.StatusForbidden, false},
		{"authenticated reader may read an offline repo", "GET", "r", "reader", http.StatusServiceUnavailable, true},
		{"unauthorized write to an anonymous-read offline repo", "PUT", "pub", "", http.StatusUnauthorized, false},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+"/repository/"+tc.repo+"/x", strings.NewReader("data"))
		if tc.user != "" {
			req.SetBasicAuth(tc.user, "pw")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: %d, want %d (%s)", tc.name, resp.StatusCode, tc.want, b)
		}
		if got := strings.Contains(string(b), `"error":"repository is offline"`); got != tc.offlineBody {
			t.Fatalf("%s: offline body = %v: %s", tc.name, got, b)
		}
	}
}

// M4 (final review): a write to a group whose writeMember is offline answers
// 503 "repository is offline" to callers who may write, not 405.
func TestGroupWriteToOfflineWriteMember(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	off := false
	cfg := &config.Config{Users: []config.User{
		{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}},
		{Name: "reader", PasswordHash: string(h)},
	},
		Repositories: []config.Repository{
			{Name: "h", Format: "maven", Type: "hosted", Online: &off},
			{Name: "g", Format: "maven", Type: "group", Members: []string{"h"}, WriteMember: "h"},
		}}
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": readAllHandler{}}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, tc := range []struct {
		user        string
		want        int
		offlineBody bool
	}{
		{"admin", http.StatusServiceUnavailable, true},
		{"reader", http.StatusForbidden, false}, // no write access: no offline leak
	} {
		req, _ := http.NewRequest("PUT", srv.URL+"/repository/g/x", strings.NewReader("data"))
		req.SetBasicAuth(tc.user, "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: %d, want %d (%s)", tc.user, resp.StatusCode, tc.want, b)
		}
		if got := strings.Contains(string(b), `"error":"repository is offline"`); got != tc.offlineBody {
			t.Fatalf("%s: offline body = %v: %s", tc.user, got, b)
		}
	}
}

// pathLockHandler holds a per-path lock while it reads the body, like the
// Maven, Go and Cargo hosted handlers do.
type pathLockHandler struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (h *pathLockHandler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	h.mu.Lock()
	if h.locks == nil {
		h.locks = map[string]*sync.Mutex{}
	}
	l := h.locks[path]
	if l == nil {
		l = &sync.Mutex{}
		h.locks[path] = l
	}
	h.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	if _, err := io.ReadAll(r.Body); err != nil {
		repo.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func idleServer(t *testing.T, idle time.Duration) *httptest.Server {
	t.Helper()
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{{Name: "r", Format: "maven", Type: "hosted"}}}
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = []config.User{{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}}}
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": &pathLockHandler{}}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadIdle: idle}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestUploadIdleTimeoutCutsStalledBody(t *testing.T) {
	srv := idleServer(t, 200*time.Millisecond)
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// "admin:pw"
	io.WriteString(c, "PUT /repository/r/x HTTP/1.1\r\nHost: x\r\nAuthorization: Basic YWRtaW46cHc=\r\nContent-Length: 1000\r\n\r\nabc")
	time.Sleep(50 * time.Millisecond) // let the stalled upload take the path lock

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("PUT", srv.URL+"/repository/r/x", strings.NewReader("ok"))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- -1
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		if code != 201 {
			t.Fatalf("second PUT = %d, want 201", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second PUT blocked behind the stalled upload")
	}
}

func TestUploadIdleTimeoutAllowsTrickle(t *testing.T) {
	srv := idleServer(t, 200*time.Millisecond)
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 12; i++ { // 600 ms in total, 3x the idle timeout
			pw.Write([]byte("x"))
			time.Sleep(50 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("PUT", srv.URL+"/repository/r/y", pr)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("trickled upload = %d, want 201", resp.StatusCode)
	}
}

func TestProxyLoopDetected(t *testing.T) {
	ts, rec := newServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/repository/releases/x", nil)
	req.Header.Set(proxy.HopHeader, "other, "+proxy.InstanceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 508 || len(rec.calls) != 0 {
		t.Fatalf("status %d calls %v, want 508 and no handler call", resp.StatusCode, rec.calls)
	}
}

func TestProxyOtherInstanceNotALoop(t *testing.T) {
	ts, rec := newServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/repository/releases/x", nil)
	req.Header.Set(proxy.HopHeader, "0123456789abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(rec.calls) != 1 {
		t.Fatalf("status %d calls %v, want served", resp.StatusCode, rec.calls)
	}
}

// TestUploadDeadlineClearedAfterBodyEOF: once the body is fully read the idle
// deadline must be gone, or net/http's background read cancels the request
// context while the handler is still working.
func TestUploadDeadlineClearedAfterBodyEOF(t *testing.T) {
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{{Name: "r", Format: "maven", Type: "hosted"}}}
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = []config.User{{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}}}
	var ctxErr error
	slow := handlerFunc(func(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
		io.ReadAll(r.Body)
		time.Sleep(500 * time.Millisecond)
		ctxErr = r.Context().Err()
		w.WriteHeader(http.StatusCreated)
	})
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": slow}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadIdle: 200 * time.Millisecond}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("PUT", srv.URL+"/repository/r/x", strings.NewReader("hello"))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || ctxErr != nil {
		t.Fatalf("status %d, ctx err %v; want 201 and a live context", resp.StatusCode, ctxErr)
	}
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string)

func (f handlerFunc) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	f(w, r, rp, path)
}

// slowServer serves repository r with handler and a 200 ms upload idle timeout.
func slowServer(t *testing.T, handler repo.Handler) *httptest.Server {
	t.Helper()
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{{Name: "r", Format: "maven", Type: "hosted"}}}
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = []config.User{{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}}}
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": handler}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadIdle: 200 * time.Millisecond}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// Final review M1a: a body-less write (DELETE, Content-Length: 0) whose
// handler works for longer than the idle timeout keeps a live context: no
// deadline is armed for a body that is never read.
func TestBodylessWriteOutlivesIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		method string
		body   io.Reader
	}{
		{"DELETE", nil},
		{"PUT", strings.NewReader("")},
		{"POST", http.NoBody},
	} {
		var ctxErr error
		srv := slowServer(t, handlerFunc(func(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
			time.Sleep(500 * time.Millisecond) // a lock wait or a rebuild
			ctxErr = r.Context().Err()
			w.WriteHeader(http.StatusNoContent)
		}))
		req, _ := http.NewRequest(tc.method, srv.URL+"/repository/r/x", tc.body)
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || ctxErr != nil {
			t.Errorf("%s: status %d, ctx err %v; want 204 and a live context", tc.method, resp.StatusCode, ctxErr)
		}
	}
}

// Final review M1b: a PUT that waits longer than the idle timeout before its
// first read (a per-path lock held by another upload) still succeeds: the
// idle clock starts at the first read, not when the body is wrapped. The
// client sends "Expect: 100-continue" (curl, Maven) so nothing of the body is
// buffered before the handler reads.
func TestUploadWaitingBeforeFirstReadSucceeds(t *testing.T) {
	got := make(chan string, 1)
	srv := slowServer(t, handlerFunc(func(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
		time.Sleep(500 * time.Millisecond)
		b, err := io.ReadAll(r.Body)
		if err != nil {
			got <- "error: " + err.Error()
			repo.WriteError(w, r, err)
			return
		}
		got <- string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "PUT /repository/r/x HTTP/1.1\r\nHost: x\r\nAuthorization: Basic YWRtaW46cHc=\r\n"+
		"Content-Length: 7\r\nExpect: 100-continue\r\n\r\n")
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "100") {
		t.Fatalf("want 100 Continue, got %q %v", line, err)
	}
	br.ReadString('\n') // blank line after the interim response
	io.WriteString(c, "payload")
	if line, _ = br.ReadString('\n'); !strings.Contains(line, "201") {
		t.Fatalf("status line %q, handler got %q", line, <-got)
	}
	if b := <-got; b != "payload" {
		t.Fatalf("handler got %q", b)
	}
}

// A handler's own IdleBody (npm publish: 5 minutes) replaces the server's
// idle value: a pause longer than the server's timeout but shorter than the
// handler's does not cut the body.
func TestHandlerIdleBodyOverridesServerIdle(t *testing.T) {
	got := make(chan string, 1)
	srv := slowServer(t, handlerFunc(func(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
		b, err := io.ReadAll(repo.IdleBody(w, r, time.Minute))
		if err != nil {
			got <- "error: " + err.Error()
			repo.WriteError(w, r, err)
			return
		}
		got <- string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "PUT /repository/r/x HTTP/1.1\r\nHost: x\r\nAuthorization: Basic YWRtaW46cHc=\r\nContent-Length: 6\r\n\r\nabc")
	time.Sleep(500 * time.Millisecond) // > the server's 200 ms, < the handler's minute
	io.WriteString(c, "def")
	if b := <-got; b != "abcdef" {
		t.Fatalf("handler got %q", b)
	}
}

// stall sends head with "Content-Length: 1000" and only 3 body bytes, then
// waits: the server must answer want and close the connection.
func stall(addr, head string, want int) error {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	io.WriteString(c, head+"Content-Length: 1000\r\n\r\nabc")
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("%q: %v", head, err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != want || err != nil {
		return fmt.Errorf("%q: status %d (body err %v), want %d", head, resp.StatusCode, err, want)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return fmt.Errorf("%q: connection not closed: %v", head, err)
	}
	return nil
}

// QA N-5: unauthenticated requests that stall their body cannot pile up:
// at most the cap are served at once, and each is answered and closed.
func TestStalledBodiesAreCappedAndReleased(t *testing.T) {
	const limit, clients = 4, 20
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{{Name: "r", Format: "maven", Type: "hosted"}}}
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": readAllHandler{}}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadIdle: 200 * time.Millisecond}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener = LimitConns(ts.Config, ts.Listener, limit)
	var mu sync.Mutex
	open, peak := 0, 0
	free := ts.Config.ConnState
	ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
		mu.Lock()
		switch st {
		case http.StateNew:
			open++
			peak = max(peak, open)
		case http.StateClosed:
			open--
		}
		mu.Unlock()
		free(c, st)
	}
	ts.Start()
	defer ts.Close()
	errs := make(chan error, clients)
	for range clients {
		go func() { errs <- stall(ts.Listener.Addr().String(), "PUT /repository/r/x HTTP/1.1\r\nHost: x\r\n", 401) }()
	}
	for range clients {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > limit {
		t.Fatalf("%d connections served at once, want at most %d", peak, limit)
	}
}

// QA N-8: a body that stalls for the idle timeout is answered 408, not 500,
// and the connection is closed.
func TestUploadIdleTimeoutAnswers408(t *testing.T) {
	srv := idleServer(t, 200*time.Millisecond)
	// "admin:pw"
	if err := stall(srv.Listener.Addr().String(), "PUT /repository/r/x HTTP/1.1\r\nHost: x\r\nAuthorization: Basic YWRtaW46cHc=\r\n", 408); err != nil {
		t.Fatal(err)
	}
}

// QA N-5: a GET whose body stalls does not hold the connection either, also
// when its response is long enough to be sent while the handler runs.
func TestStalledBodyOnGetIsReleased(t *testing.T) {
	srv := slowServer(t, handlerFunc(func(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
		io.WriteString(w, strings.Repeat("x", 64<<10))
	}))
	if err := stall(srv.Listener.Addr().String(), "GET /repository/r/x HTTP/1.1\r\nHost: x\r\n", 200); err != nil {
		t.Fatal(err)
	}
}

// capServer serves repository r with handler under a cap of n connections and
// without net/http's header and idle timeouts: only the cap frees a slot.
func capServer(t *testing.T, n int, handler repo.Handler) *httptest.Server {
	t.Helper()
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{{Name: "r", Format: "maven", Type: "hosted"}}}
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = []config.User{{Name: "admin", PasswordHash: string(h), Roles: []string{"admin"}}}
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"maven": handler}, Auth: auth.New(cfg, noTokens{}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener = LimitConns(ts.Config, ts.Listener, n)
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// getStatus asks for the status page on a new connection, which it returns
// open, idle between requests, once the 200 came within a second.
func getStatus(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	io.WriteString(c, "GET /service/rest/v1/status HTTP/1.1\r\nHost: x\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("status not answered within 1 s: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status answered %d", resp.StatusCode)
	}
	return c
}

// closedByServer reports whether the server closed c within a second.
func closedByServer(c net.Conn) bool {
	c.SetReadDeadline(time.Now().Add(time.Second))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// QA N-5: once the cap is reached by connections that cost their clients
// nothing to hold, idle between requests or never finishing their headers, a
// new client is still served at once: the one idle or waiting the longest is
// closed to make room. An upload in progress is never closed for it.
func TestConnCapShedsIdleAndSlowHeaderConns(t *testing.T) {
	reading := make(chan struct{})
	got := make(chan int64, 1)
	ts := capServer(t, 3, handlerFunc(func(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
		close(reading)
		n, err := io.Copy(io.Discard, r.Body)
		got <- n
		if err != nil {
			repo.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	addr := ts.Listener.Addr().String()

	// A signed-in upload that keeps coming: 32 KiB every 20 ms until stop.
	pr, pw := io.Pipe()
	stop := make(chan struct{})
	end := sync.OnceFunc(func() { close(stop) })
	defer end() // also on t.Fatal: ts.Close waits for the upload
	go func() {
		defer pw.Close()
		chunk := make([]byte, 32<<10)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
	}()
	upload := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("PUT", ts.URL+"/repository/r/big.jar", pr)
		req.SetBasicAuth("admin", "pw")
		resp, err := ts.Client().Do(req)
		if err != nil {
			upload <- 0
			return
		}
		resp.Body.Close()
		upload <- resp.StatusCode
	}()
	<-reading
	idle := getStatus(t, addr)
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	io.WriteString(slow, "GET /service/rest/v1/status HTTP/1.1\r\nHost: x\r\n") // headers never finished
	time.Sleep(shedAfter + 100*time.Millisecond)

	// The cap of 3 is reached: each new client takes the place of the oldest
	// cheap connection, the idle one first, then the slow one.
	getStatus(t, addr)
	getStatus(t, addr)
	if !closedByServer(idle) || !closedByServer(slow) {
		t.Fatal("the idle and the slow-header connection were not both closed")
	}
	end()
	if code, n := <-upload, <-got; code != http.StatusCreated || n < 32<<10 {
		t.Fatalf("upload answered %d after %d bytes, want 201", code, n)
	}
}
