package rubygems

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/server"
)

// TestPushAuthAndPrivileges runs the handler behind the real server: a
// push authenticates with the API key gem push sends as the bare
// Authorization value (an API token), with the token as the Basic
// password, or with the password; it needs add, a yank needs delete, reads
// are anonymous when anonymousRead is on and a 401 Basic challenge
// otherwise.
func TestPushAuthAndPrivileges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	db := e.h.Store.DB
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	for _, r := range []meta.Role{
		{ID: "pushers", Privileges: []meta.Privilege{{Target: "format:rubygems", Actions: []string{meta.ActionRead, meta.ActionAdd}}}},
		{ID: "deleters", Privileges: []meta.Privilege{{Target: "repo:gems-hosted", Actions: []string{meta.ActionRead, meta.ActionDelete}}}},
	} {
		if err := db.CreateRole(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []meta.User{
		{ID: "pusher", PasswordHash: string(hash), Roles: []string{"pushers"}},
		{ID: "deleter", PasswordHash: string(hash), Roles: []string{"deleters"}},
		{ID: "reader", PasswordHash: string(hash)},
	} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.Open(ctx, db, db)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := a.CreateToken(ctx, "pusher")
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	defs := []config.Repository{
		{Name: "gems-hosted", Format: "rubygems", Type: "hosted", WritePolicy: "allow_once", AnonymousRead: &yes},
		{Name: "gems-private", Format: "rubygems", Type: "hosted", AnonymousRead: &no},
	}
	reg := repo.NewRegistry(nil, defs)
	a.AnonymousRead = reg.AnonymousRead
	a.RepoFormat = func(n string) string {
		if rp := reg.Get(n); rp != nil {
			return rp.Format
		}
		return ""
	}
	e.h.Auth = a
	s := &server.Server{Registry: reg, Handlers: map[string]repo.Handler{"rubygems": e.h}, Auth: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	do := func(method, path string, body []byte, ct string, cred func(*http.Request)) (int, http.Header, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+"/repository/"+path, bytes.NewReader(body))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		if cred != nil {
			cred(r)
		}
		resp, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, b
	}
	apiKey := func(k string) func(*http.Request) { return func(r *http.Request) { r.Header.Set("Authorization", k) } }
	basic := func(u, p string) func(*http.Request) { return func(r *http.Request) { r.SetBasicAuth(u, p) } }
	yank := []byte("gem_name=demo&version=1.0.0")
	cases := []struct {
		name, method, path string
		body               []byte
		ct                 string
		cred               func(*http.Request)
		want               int
	}{
		{"api key", "POST", "gems-hosted/api/v1/gems", gem(t, "demo", "1.0.0", "", false), ctBinary, apiKey(token), 200},
		{"token as Basic password", "POST", "gems-hosted/api/v1/gems", gem(t, "demo", "1.1.0", "", false), ctBinary, basic("pusher", token), 200},
		{"password", "POST", "gems-hosted/api/v1/gems", gem(t, "demo", "1.2.0", "", false), ctBinary, basic("pusher", "pw"), 200},
		{"wrong api key", "POST", "gems-hosted/api/v1/gems", gem(t, "demo", "1.3.0", "", false), ctBinary, apiKey("cc_nope"), 401},
		{"anonymous push", "POST", "gems-hosted/api/v1/gems", gem(t, "demo", "1.3.0", "", false), ctBinary, nil, 401},
		{"reader push", "POST", "gems-hosted/api/v1/gems", gem(t, "demo", "1.3.0", "", false), ctBinary, basic("reader", "pw"), 403},
		{"pusher yank", "DELETE", "gems-hosted/api/v1/gems/yank", yank, "application/x-www-form-urlencoded", apiKey(token), 403},
		{"deleter yank", "DELETE", "gems-hosted/api/v1/gems/yank", yank, "application/x-www-form-urlencoded", basic("deleter", "pw"), 200},
		{"anonymous read", "GET", "gems-hosted/info/demo", nil, "", nil, 200},
		{"anonymous read, private", "GET", "gems-private/versions", nil, "", nil, 401},
		{"signed-in read, private (authenticatedRead)", "GET", "gems-private/versions", nil, "", basic("reader", "pw"), 200},
		{"pusher, private", "GET", "gems-private/versions", nil, "", apiKey(token), 200},
	}
	for _, c := range cases {
		status, hdr, _ := do(c.method, c.path, c.body, c.ct, c.cred)
		if status != c.want {
			t.Errorf("%s: %d, want %d", c.name, status, c.want)
		}
		if c.want == 401 && hdr.Get("WWW-Authenticate") != `Basic realm="commandcenter"` {
			t.Errorf("%s: no Basic challenge (%q)", c.name, hdr.Get("WWW-Authenticate"))
		}
	}
	_, _, body := do("GET", "gems-hosted/info/demo", nil, "", nil)
	if got := string(body); !strings.HasPrefix(got, "---\n1.1.0 ") || !strings.Contains(got, "\n1.2.0 ") || strings.Contains(got, "1.0.0 ") {
		t.Fatalf("after: %q", got)
	}
}
