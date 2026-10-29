package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta/metatest"
)

// uiCall sends a UI request with the session cookie id, if any, and returns the
// status, the body and the session cookie it set, if any.
func uiCall(t *testing.T, method, url, body, session string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set(auth.UIHeader, "1")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	set := ""
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookie && c.MaxAge >= 0 {
			set = c.Value
		}
	}
	return resp.StatusCode, string(b), set
}

func uiLogin(t *testing.T, base, pw string) string {
	t.Helper()
	st, b, id := uiCall(t, "POST", base+"/-/api/login", `{"user":"ci","password":"`+pw+`"}`, "")
	if st != 200 || id == "" {
		t.Fatalf("login: %d %s", st, b)
	}
	return id
}

// signedIn reports whether the node at base takes session id as ci's.
func signedIn(t *testing.T, base, id string) bool {
	t.Helper()
	st, b, _ := uiCall(t, "GET", base+"/-/api/me", "", id)
	if st != 200 {
		t.Fatalf("me: %d %s", st, b)
	}
	return strings.Contains(b, `"user":"ci"`)
}

// twoApps starts two nodes on one PostgreSQL database, as an HA cluster
// behind a load balancer would run them (without Serve: not registered).
func twoApps(t *testing.T) (a, b *App, ua, ub string) {
	t.Helper()
	if !metatest.Postgres() {
		t.Skip("nodes share a database on PostgreSQL only")
	}
	cfg := testConfig(t, "  - { name: conan-hosted, format: conan, type: hosted }\n")
	start := func() (*App, string) {
		x, err := New(cfg, quiet())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { x.Close() })
		ts := httptest.NewServer(x.Handler)
		t.Cleanup(ts.Close)
		return x, ts.URL
	}
	a, ua = start()
	b, ub = start()
	return a, b, ua, ub
}

// A session works on every node and ends on every node (HA design 2.4).
func TestClusterSessions(t *testing.T) {
	_, b, ua, ub := twoApps(t)
	id := uiLogin(t, ua, "pw")
	if !signedIn(t, ub, id) {
		t.Fatal("B does not take A's session")
	}
	if st, body, _ := uiCall(t, "POST", ub+"/-/api/logout", "", id); st != 204 {
		t.Fatalf("logout on B: %d %s", st, body)
	}
	if signedIn(t, ua, id) {
		t.Fatal("A takes a session B ended")
	}

	// A password change on A ends the other sessions on B at once; the
	// session it starts survives B's reload, which sees the change late.
	old, other := uiLogin(t, ua, "pw"), uiLogin(t, ub, "pw")
	st, body, fresh := uiCall(t, "PUT", ua+"/-/api/account/password", `{"currentPassword":"pw","newPassword":"newpassword1"}`, old)
	if st != 204 || fresh == "" {
		t.Fatalf("password change: %d %s", st, body)
	}
	if signedIn(t, ub, other) || signedIn(t, ub, old) {
		t.Fatal("B takes a session started before the password change")
	}
	if err := b.Auth.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !signedIn(t, ub, fresh) {
		t.Fatal("B's reload ended the session of the password change")
	}
}

// The nodes refuse a name together after 5 failures, wherever they were
// made (HA design 2.4).
func TestClusterThrottle(t *testing.T) {
	_, _, ua, ub := twoApps(t)
	for i, base := range []string{ua, ua, ua, ub, ub} {
		if st, body, _ := uiCall(t, "POST", base+"/-/api/login", `{"user":"ci","password":"wrong"}`, ""); st != 401 {
			t.Fatalf("failure %d: %d %s", i+1, st, body)
		}
	}
	for _, base := range []string{ua, ub} {
		if st, body, _ := uiCall(t, "POST", base+"/-/api/login", `{"user":"ci","password":"pw"}`, ""); st != 429 {
			t.Fatalf("the right password after 5 failures: %d %s", st, body)
		}
	}
}

// A Docker or Conan token one node issued verifies on the other (HA design
// 2.5).
func TestClusterRegistryTokens(t *testing.T) {
	_, _, ua, ub := twoApps(t)
	st, body := utCall(t, "GET", ua+"/v2/token?service=x&scope=repository:x/app:pull", "ci", "pw", "", false)
	if st != 200 {
		t.Fatalf("docker token: %d %s", st, body)
	}
	tok := utJSON(t, body)["token"].(string)
	for _, c := range []struct {
		tok  string
		want int
	}{{tok, 200}, {tok + "x", 401}} {
		if st, body := utCall(t, "GET", ub+"/v2/", "", "", "", false, "Authorization", "Bearer "+c.tok); st != c.want {
			t.Fatalf("docker token on B: %d %s, want %d", st, body, c.want)
		}
	}

	st, tok = utCall(t, "GET", ua+"/repository/conan-hosted/v1/users/authenticate", "ci", "pw", "", false)
	if st != 200 || tok == "" {
		t.Fatalf("conan login: %d %s", st, tok)
	}
	if st, body := utCall(t, "GET", ub+"/repository/conan-hosted/v1/users/check_credentials", "", "", "", false, "Authorization", "Bearer "+tok); st != 200 || body != "ci" {
		t.Fatalf("conan token on B: %d %s", st, body)
	}
}

// A session survives a restart, on SQLite too.
func TestSessionSurvivesRestart(t *testing.T) {
	cfg := testConfig(t, "")
	start := func() string {
		a, err := New(cfg, quiet())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close() })
		ts := httptest.NewServer(a.Handler)
		t.Cleanup(ts.Close)
		return ts.URL
	}
	u := start()
	id := uiLogin(t, u, "pw")
	if !signedIn(t, start(), id) {
		t.Fatal("the session did not survive the restart")
	}
}

// On PostgreSQL a node with another key, or none, does not start, even
// before a secret is stored (HA design 2.5); SQLite checks nothing.
func TestClusterSecretKey(t *testing.T) {
	cfg := testConfig(t, "")
	key := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	for _, c := range []struct{ key, want string }{
		{key(1), ""}, {key(2), "another key"}, {"", "no secret key is set"}, {key(1), ""},
	} {
		t.Setenv(secretKeyEnv, c.key)
		a, err := New(cfg, quiet())
		if !metatest.Postgres() {
			c.want = ""
		}
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Fatalf("start with key %q: %v, want %q", c.key, err, c.want)
		}
		if err == nil {
			a.Close()
		}
	}
}
