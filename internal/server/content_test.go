package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
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
// cache. A write or a public path (npm web login) keeps its own answer; npm
// ping is a read (it no longer reveals hidden repositories).
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
	if code, body := get("/repository/npm-all/-/ping"); code != 503 || !strings.Contains(body, "repository is offline") {
		t.Errorf("npm ping: %d %s, want 503 like any read", code, body)
	}
	if res := send(t, "POST", ts.URL+"/repository/npm-all/-/v1/login", "{}", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("npm web login: %d, want the handler's 404", res.StatusCode)
	}
	put := send(t, "PUT", ts.URL+"/repository/maven-all/"+reads["maven"], "x", func(r *http.Request) { r.SetBasicAuth("admin", "pw") })
	if put.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("write to a group without writeMember: %d, want 405", put.StatusCode)
	}
}

// A browser replays Basic credentials it cached after the server's challenge
// on a cross-site form post: such a twine-style upload is refused before
// anything is stored. A CLI client (no Sec-Fetch-Site, no Origin), the web
// UI's same-origin upload and a cross-site download pass.
func TestCrossSiteUploadRefused(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := &config.Config{
		Users:        []config.User{{Name: "ci", PasswordHash: string(h), Roles: []string{"deployer"}}},
		Roles:        map[string]config.Role{"deployer": {Write: []string{"py"}}},
		Repositories: []config.Repository{{Name: "py", Format: "pypi", Type: "hosted"}},
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
		Handlers: map[string]repo.Handler{"pypi": pypi.New(store, proxy.New(store, http.DefaultClient, 4), func() string { return "http://localhost" })},
		Auth:     authn,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	upload := func(version string, mod func(*http.Request)) int {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField(":action", "file_upload")
		mw.WriteField("name", "demo")
		mw.WriteField("version", version)
		fw, _ := mw.CreateFormFile("content", "demo-"+version+".tar.gz")
		fw.Write(sdist(t, "demo-"+version+"/PKG-INFO", "Name: demo\nVersion: "+version+"\n")) // PyPI checks the archive
		mw.Close()
		return send(t, "POST", ts.URL+"/repository/py/", body.String(), func(r *http.Request) {
			r.Header.Set("Content-Type", mw.FormDataContentType())
			mod(r)
		}).StatusCode
	}
	crossSite := func(r *http.Request) {
		basic(r)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://evil.example")
	}
	stored := func(version string) int {
		return send(t, "GET", ts.URL+"/repository/py/packages/demo/demo-"+version+".tar.gz", "", crossSite).StatusCode
	}
	if c := upload("1.0", crossSite); c != http.StatusForbidden || stored("1.0") != http.StatusNotFound {
		t.Fatalf("cross-site upload: %d, then GET %d; want 403 and nothing stored", c, stored("1.0"))
	}
	if c := upload("1.0", basic); c != http.StatusOK || stored("1.0") != http.StatusOK {
		t.Fatalf("CLI upload: %d, then GET %d", c, stored("1.0"))
	}
	ui := func(r *http.Request) { // as the browser sends the web UI's upload
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: authn.Sessions.Create("ci")})
		r.Header.Set(auth.UIHeader, "1")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Origin", ts.URL)
	}
	if c := upload("2.0", ui); c != http.StatusOK || stored("2.0") != http.StatusOK {
		t.Fatalf("same-origin UI upload: %d, then GET %d", c, stored("2.0"))
	}
}

// sdist is a gzipped tar holding one file.
func sdist(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	io.WriteString(tw, content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// Behind a reverse proxy that rewrites Host (nginx's default), a page served
// over plain HTTP sends no Sec-Fetch-Site and an Origin that does not match
// Host: the origin of the live baseURL is trusted instead, and only that.
func TestCrossOriginTrustsLiveBaseURL(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	admin := meta.User{ID: "admin", PasswordHash: string(h), Roles: []string{meta.AdminRole}, Source: meta.SourceLocal, Status: meta.StatusActive}
	settings := meta.DefaultSettings()
	settings.BaseURL = "http://repo.example.com"
	a, err := auth.Open(context.Background(), fixedSource{users: []meta.User{admin}, settings: &settings}, noTokens{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Registry: repo.NewRegistry(nil, []config.Repository{{Name: "r", Format: "maven", Type: "hosted"}}),
		Handlers: map[string]repo.Handler{"maven": &recordHandler{}}, Auth: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	put := func(origin string) int {
		return send(t, "PUT", ts.URL+"/repository/r/x", "", func(r *http.Request) {
			r.SetBasicAuth("admin", "pw")
			r.Header.Set("Origin", origin)
		}).StatusCode
	}
	for origin, want := range map[string]int{
		ts.URL:                     http.StatusOK, // Origin matches Host: no proxy
		"http://repo.example.com":  http.StatusOK,
		"https://repo.example.com": http.StatusForbidden, // another scheme is another origin
		"http://evil.example":      http.StatusForbidden,
		"null":                     http.StatusForbidden,
	} {
		if got := put(origin); got != want {
			t.Errorf("Origin %s: %d, want %d", origin, got, want)
		}
	}
	settings.BaseURL = "https://new.example.com/cc"
	if err := a.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if old, cur := put("http://repo.example.com"), put("https://new.example.com"); old != http.StatusForbidden || cur != http.StatusOK {
		t.Fatalf("after the baseURL change: old origin %d, new origin %d; want 403 and 200", old, cur)
	}
}

// QA N-5: npm login (auth.Public) reads its body before anyone is signed in,
// and no upload is anonymous, so an anonymous body must arrive in full by a
// total deadline (the idle value here, 200 ms; 5 s in production): trickled
// a byte every 50 ms, well inside the idle timeout, it is cut with 408 and
// the connection closed. A signed-in upload may trickle on as long as it
// keeps coming (TestUploadIdleTimeoutAllowsTrickle).
func TestAnonymousBodyIsCutAtTotalDeadline(t *testing.T) {
	cfg := &config.Config{Repositories: []config.Repository{{Name: "n", Format: "npm", Type: "hosted"}}}
	authn := auth.New(cfg, noTokens{})
	s := &Server{Registry: repo.NewRegistry(nil, cfg.Repositories), Handlers: map[string]repo.Handler{"npm": npm.New(nil, nil, authn, nil)},
		Auth: authn, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadIdle: 200 * time.Millisecond}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	io.WriteString(c, "PUT /repository/n/-/user/org.couchdb.user:x HTTP/1.1\r\nHost: x\r\nContent-Length: 40\r\n\r\n")
	go func() { // 2 s for all of it
		for range 40 {
			if _, err := io.WriteString(c, " "); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if took := time.Since(start); resp.StatusCode != http.StatusRequestTimeout || took > time.Second {
		t.Fatalf("answered %d after %v, want 408 soon after the 200 ms total deadline", resp.StatusCode, took)
	}
	if !closedByServer(c) {
		t.Fatal("connection still open")
	}
}
