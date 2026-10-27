package nuget

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/server"
)

// TestPushAuthAndPrivileges runs the handler behind the real server: a
// push authenticates with X-NuGet-ApiKey (an API token), with the token as
// the Basic password, or with the password; it needs add, an unpublish
// needs delete, reads are anonymous when anonymousRead is on and a 401
// Basic challenge otherwise.
func TestPushAuthAndPrivileges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	db := e.h.Store.DB
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	for _, r := range []meta.Role{
		{ID: "pushers", Privileges: []meta.Privilege{{Target: "format:nuget", Actions: []string{meta.ActionRead, meta.ActionAdd}}}},
		{ID: "deleters", Privileges: []meta.Privilege{{Target: "repo:nuget-hosted", Actions: []string{meta.ActionRead, meta.ActionDelete}}}},
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
		{Name: "nuget-hosted", Format: "nuget", Type: "hosted", WritePolicy: "allow_once", AnonymousRead: &yes},
		{Name: "nuget-private", Format: "nuget", Type: "hosted", AnonymousRead: &no},
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
	s := &server.Server{Registry: reg, Handlers: map[string]repo.Handler{"nuget": e.h}, Auth: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	do := func(method, path string, pkg []byte, cred func(*http.Request)) (int, http.Header, []byte) {
		t.Helper()
		var body io.Reader
		ct := ""
		if pkg != nil {
			b, c := form(t, "package.nupkg", pkg)
			body, ct = bytes.NewReader(b), c
		}
		r, _ := http.NewRequest(method, ts.URL+"/repository/"+path, body)
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
	apiKey := func(k string) func(*http.Request) { return func(r *http.Request) { r.Header.Set("X-NuGet-ApiKey", k) } }
	basic := func(u, p string) func(*http.Request) { return func(r *http.Request) { r.SetBasicAuth(u, p) } }
	cases := []struct {
		name, method, path string
		pkg                []byte
		cred               func(*http.Request)
		want               int
	}{
		{"api key", "PUT", "nuget-hosted/api/v2/package", nupkg(t, "Demo", "1.0.0", ""), apiKey(token), 201},
		{"token as Basic password", "PUT", "nuget-hosted/api/v2/package", nupkg(t, "Demo", "1.1.0", ""), basic("pusher", token), 201},
		{"password", "PUT", "nuget-hosted/api/v2/package", nupkg(t, "Demo", "1.2.0", ""), basic("pusher", "pw"), 201},
		{"wrong api key", "PUT", "nuget-hosted/api/v2/package", nupkg(t, "Demo", "1.3.0", ""), apiKey("cc_nope"), 401},
		{"anonymous push", "PUT", "nuget-hosted/api/v2/package", nupkg(t, "Demo", "1.3.0", ""), nil, 401},
		{"reader push", "PUT", "nuget-hosted/api/v2/package", nupkg(t, "Demo", "1.3.0", ""), basic("reader", "pw"), 403},
		{"pusher delete", "DELETE", "nuget-hosted/api/v2/package/demo/1.0.0", nil, apiKey(token), 403},
		{"deleter delete", "DELETE", "nuget-hosted/api/v2/package/demo/1.0.0", nil, basic("deleter", "pw"), 204},
		{"anonymous read", "GET", "nuget-hosted/v3-flatcontainer/demo/index.json", nil, nil, 200},
		{"anonymous read, private", "GET", "nuget-private/index.json", nil, nil, 401},
		{"signed-in read, private (authenticatedRead)", "GET", "nuget-private/index.json", nil, basic("reader", "pw"), 200},
		{"pusher, private", "GET", "nuget-private/index.json", nil, apiKey(token), 200},
	}
	for _, c := range cases {
		status, hdr, _ := do(c.method, c.path, c.pkg, c.cred)
		if status != c.want {
			t.Errorf("%s: %d, want %d", c.name, status, c.want)
		}
		if c.want == 401 && hdr.Get("WWW-Authenticate") != `Basic realm="commandcenter"` {
			t.Errorf("%s: no Basic challenge (%q)", c.name, hdr.Get("WWW-Authenticate"))
		}
	}
	_, _, body := do("GET", "nuget-hosted/v3-flatcontainer/demo/index.json", nil, nil)
	var flat struct{ Versions []string }
	if err := json.Unmarshal(body, &flat); err != nil || len(flat.Versions) != 2 {
		t.Fatalf("after: %v %v", flat, err)
	}
}
