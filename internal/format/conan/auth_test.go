package conan

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/server"
)

// TestLoginAndPrivileges runs the handler behind the real server: `conan
// remote login` (Basic with the password or an API token at
// v1/users/authenticate) gets a token that the server accepts as Bearer
// on every route; an upload needs add (a format:conan privilege works), a
// delete needs delete; ping is public, reads are anonymous when
// anonymousRead is on and a 401 Basic challenge otherwise; an expired or
// forged token is refused.
func TestLoginAndPrivileges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	db := e.h.Store.DB
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	for _, r := range []meta.Role{
		{ID: "uploaders", Privileges: []meta.Privilege{{Target: "format:conan", Actions: []string{meta.ActionRead, meta.ActionAdd}}}},
		{ID: "deleters", Privileges: []meta.Privilege{{Target: "repo:conan-hosted", Actions: []string{meta.ActionRead, meta.ActionDelete}}}},
	} {
		if err := db.CreateRole(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []meta.User{
		{ID: "uploader", PasswordHash: string(hash), Roles: []string{"uploaders"}},
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
	_, apiToken, err := a.CreateToken(ctx, "uploader")
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	defs := []config.Repository{
		{Name: "conan-hosted", Format: "conan", Type: "hosted", WritePolicy: "allow_once", AnonymousRead: &yes},
		{Name: "conan-private", Format: "conan", Type: "hosted", AnonymousRead: &no},
	}
	reg := repo.NewRegistry(nil, defs)
	a.AnonymousRead = reg.AnonymousRead
	a.RepoFormat = func(n string) string {
		if rp := reg.Get(n); rp != nil {
			return rp.Format
		}
		return ""
	}
	a.Bearers = append(a.Bearers, e.h.VerifyToken)
	e.h.Auth = a
	s := &server.Server{Registry: reg, Handlers: map[string]repo.Handler{"conan": e.h}, Auth: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	do := func(method, path string, body []byte, cred func(*http.Request)) (int, http.Header, string) {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+"/repository/"+path, bytes.NewReader(body))
		if cred != nil {
			cred(r)
		}
		resp, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}
	basic := func(u, p string) func(*http.Request) { return func(r *http.Request) { r.SetBasicAuth(u, p) } }
	bearer := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}

	// Ping and login.
	if st, hdr, _ := do("GET", "conan-private/v1/ping", nil, nil); st != 200 || hdr.Get("X-Conan-Server-Capabilities") != Capabilities {
		t.Fatalf("ping: %d %v", st, hdr)
	}
	if st, hdr, _ := do("GET", "conan-hosted/v1/users/authenticate", nil, nil); st != 401 || hdr.Get("WWW-Authenticate") != `Basic realm="commandcenter"` {
		t.Fatalf("anonymous login: %d %v", st, hdr)
	}
	if st, _, _ := do("GET", "conan-hosted/v1/users/authenticate", nil, basic("uploader", "wrong")); st != 401 {
		t.Fatalf("wrong password: %d", st)
	}
	st, hdr, token := do("GET", "conan-hosted/v1/users/authenticate", nil, basic("uploader", "pw"))
	if st != 200 || hdr.Get("Content-Type") != ctText || token == "" {
		t.Fatalf("login: %d %v %q", st, hdr, token)
	}
	if st, _, tok2 := do("GET", "conan-hosted/v1/users/authenticate", nil, basic("uploader", apiToken)); st != 200 || tok2 == "" {
		t.Fatalf("login with an API token: %d", st)
	}
	if st, _, body := do("GET", "conan-hosted/v1/users/check_credentials", nil, bearer(token)); st != 200 || body != "uploader" {
		t.Fatalf("check_credentials: %d %q", st, body)
	}
	if st, _, _ := do("GET", "conan-hosted/v1/users/check_credentials", nil, bearer(token+"x")); st != 401 {
		t.Fatalf("forged token: %d", st)
	}
	if st, _, _ := do("GET", "conan-hosted/v1/users/check_credentials", nil, nil); st != 401 {
		t.Fatalf("anonymous check_credentials: %d", st)
	}

	file := func(rrev string) string { return "conan-hosted/" + RecipeFile(zlib, rrev, "conanfile.py") }
	cases := []struct {
		name, method, path string
		body               []byte
		cred               func(*http.Request)
		want               int
	}{
		{"upload with the login token", "PUT", file("aaaa"), []byte("a"), bearer(token), 201},
		{"upload with the API token as password", "PUT", file("bbbb"), []byte("b"), basic("uploader", apiToken), 201},
		{"upload with the password", "PUT", file("cccc"), []byte("c"), basic("uploader", "pw"), 201},
		{"anonymous upload", "PUT", file("dddd"), []byte("d"), nil, 401},
		{"reader upload", "PUT", file("dddd"), []byte("d"), basic("reader", "pw"), 403},
		{"uploader delete", "DELETE", "conan-hosted/v2/conans/zlib/1.3/_/_/revisions/aaaa", nil, bearer(token), 403},
		{"deleter delete", "DELETE", "conan-hosted/v2/conans/zlib/1.3/_/_/revisions/aaaa", nil, basic("deleter", "pw"), 200},
		{"anonymous read", "GET", "conan-hosted/v2/conans/zlib/1.3/_/_/latest", nil, nil, 200},
		{"anonymous read, private", "GET", "conan-private/v2/conans/zlib/1.3/_/_/latest", nil, nil, 401},
		{"anonymous search, private", "GET", "conan-private/v2/conans/search?q=*", nil, nil, 401},
		{"signed-in read, private (authenticatedRead)", "GET", "conan-private/v2/conans/search?q=*", nil, basic("reader", "pw"), 200},
		{"token read, private", "GET", "conan-private/v2/conans/search?q=*", nil, bearer(token), 200},
	}
	for _, c := range cases {
		st, hdr, body := do(c.method, c.path, c.body, c.cred)
		if st != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, st, body, c.want)
		}
		if c.want == 401 && hdr.Get("WWW-Authenticate") != `Basic realm="commandcenter"` {
			t.Errorf("%s: no Basic challenge (%q)", c.name, hdr.Get("WWW-Authenticate"))
		}
	}
	// An expired token is refused (Conan then logs in again).
	e.now = e.now.Add(TokenTTL + time.Minute)
	if st, _, _ := do("GET", "conan-hosted/v1/users/check_credentials", nil, bearer(token)); st != 401 {
		t.Errorf("expired token: %d", st)
	}
}
