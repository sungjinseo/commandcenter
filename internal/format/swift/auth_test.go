package swift

import (
	"bytes"
	"context"
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

// TestPrivileges runs the handler behind the real server: a publish needs
// add (a format:swift privilege works), reads are anonymous when
// anonymousRead is on and a 401 Basic challenge otherwise; an API token
// works as `swift package-registry login` sends it (the Basic password,
// or a Bearer token), POST login answers a signed-in reader, and the
// server's 401/403 are problem details.
func TestPrivileges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	db := e.h.Store.DB
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	for _, r := range []meta.Role{
		{ID: "publishers", Privileges: []meta.Privilege{{Target: "format:swift", Actions: []string{meta.ActionRead, meta.ActionAdd}}}},
	} {
		if err := db.CreateRole(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []meta.User{
		{ID: "publisher", PasswordHash: string(hash), Roles: []string{"publishers"}},
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
	_, token, err := a.CreateToken(ctx, "publisher")
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	defs := []config.Repository{
		{Name: "swift-hosted", Format: "swift", Type: "hosted", WritePolicy: "allow_once", AnonymousRead: &yes},
		{Name: "swift-private", Format: "swift", Type: "hosted", AnonymousRead: &no},
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
	s := &server.Server{Registry: reg, Handlers: map[string]repo.Handler{"swift": e.h}, Auth: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	do := func(method, path string, body []byte, user, pw string) (int, http.Header) {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+"/repository/"+path, bytes.NewReader(body))
		r.Header.Set("Content-Type", ctZip)
		r.Header.Set("Accept", ctRegistry+"+json")
		switch {
		case user == "bearer":
			r.Header.Set("Authorization", "Bearer "+pw)
		case user != "":
			r.SetBasicAuth(user, pw)
		}
		resp, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	z := archive(t, "LinkedList/")
	up := "swift-hosted/mona/LinkedList/"
	cases := []struct {
		name, method, path string
		body               []byte
		user, pw           string
		want               int
		problem            bool // the handler renders the refusal (the server's credential and no-read 401s stay plain)
	}{
		{"password", "PUT", up + "1.0.0", z, "publisher", "pw", 201, false},
		{"token as Basic password", "PUT", up + "1.1.0", z, "publisher", token, 201, false},
		{"bearer token", "PUT", up + "1.2.0", z, "bearer", token, 201, false},
		{"wrong password", "PUT", up + "1.3.0", z, "publisher", "nope", 401, false},
		{"anonymous publish", "PUT", up + "1.3.0", z, "", "", 401, true},
		{"reader publish", "PUT", up + "1.3.0", z, "reader", "pw", 403, true},
		{"anonymous read", "GET", up[:len(up)-1], nil, "", "", 200, false},
		{"anonymous archive", "GET", up + "1.0.0.zip", nil, "", "", 200, false},
		{"anonymous read, private", "GET", "swift-private/mona/LinkedList", nil, "", "", 401, false},
		{"signed-in read, private", "GET", "swift-private/mona/LinkedList", nil, "reader", "pw", 404, true},
		{"login", "POST", "swift-hosted/login", nil, "reader", "pw", 200, false}, // swift package-registry login: Read is enough
		{"login with a token", "POST", "swift-hosted/login", nil, "bearer", token, 200, false},
		{"anonymous login", "POST", "swift-hosted/login", nil, "", "", 401, true},
		{"login, wrong password", "POST", "swift-hosted/login", nil, "reader", "nope", 401, false},
	}
	for _, c := range cases {
		status, hdr := do(c.method, c.path, c.body, c.user, c.pw)
		if status != c.want {
			t.Errorf("%s: %d, want %d", c.name, status, c.want)
		}
		if c.want == 401 && hdr.Get("WWW-Authenticate") != `Basic realm="commandcenter"` {
			t.Errorf("%s: no Basic challenge (%q)", c.name, hdr.Get("WWW-Authenticate"))
		}
		if c.problem && (hdr.Get("Content-Type") != ctProblem || hdr.Get("Content-Version") != "1") {
			t.Errorf("%s: not a problem: %v", c.name, hdr)
		}
	}
}
