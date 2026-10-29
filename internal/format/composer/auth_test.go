package composer

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

// TestPrivileges runs the handler behind the real server: an upload needs
// add (a format:composer privilege works), a delete needs delete, reads
// are anonymous when anonymousRead is on and a 401 Basic challenge
// otherwise; an API token works as the Basic password.
func TestPrivileges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	db := e.h.Store.DB
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	for _, r := range []meta.Role{
		{ID: "uploaders", Privileges: []meta.Privilege{{Target: "format:composer", Actions: []string{meta.ActionRead, meta.ActionAdd}}}},
		{ID: "deleters", Privileges: []meta.Privilege{{Target: "repo:composer-hosted", Actions: []string{meta.ActionRead, meta.ActionDelete}}}},
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
	_, token, err := a.CreateToken(ctx, "uploader")
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	defs := []config.Repository{
		{Name: "composer-hosted", Format: "composer", Type: "hosted", WritePolicy: "allow_once", AnonymousRead: &yes},
		{Name: "composer-private", Format: "composer", Type: "hosted", AnonymousRead: &no},
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
	s := &server.Server{Registry: reg, Handlers: map[string]repo.Handler{"composer": e.h}, Auth: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	do := func(method, path string, body []byte, user, pw string) (int, http.Header) {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+"/repository/"+path, bytes.NewReader(body))
		if user != "" {
			r.SetBasicAuth(user, pw)
		}
		resp, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	z := archive(t, "", composerJSON("acme/demo", ""))
	up := "composer-hosted/" + uploadPath + "acme/demo/"
	cases := []struct {
		name, method, path string
		body               []byte
		user, pw           string
		want               int
	}{
		{"password", "PUT", up + "1.0.0", z, "uploader", "pw", 201},
		{"token as Basic password", "PUT", up + "1.1.0", z, "uploader", token, 201},
		{"wrong password", "PUT", up + "1.2.0", z, "uploader", "nope", 401},
		{"anonymous upload", "PUT", up + "1.2.0", z, "", "", 401},
		{"reader upload", "PUT", up + "1.2.0", z, "reader", "pw", 403},
		{"uploader delete", "DELETE", up + "1.0.0", nil, "uploader", "pw", 403},
		{"deleter delete", "DELETE", up + "1.0.0", nil, "deleter", "pw", 204},
		{"anonymous read", "GET", "composer-hosted/p2/acme/demo.json", nil, "", "", 200},
		{"anonymous dist", "GET", "composer-hosted/dist/acme/demo/1.1.0.0.zip", nil, "", "", 200},
		{"anonymous read, private", "GET", "composer-private/packages.json", nil, "", "", 401},
		{"signed-in read, private", "GET", "composer-private/packages.json", nil, "reader", "pw", 200},
	}
	for _, c := range cases {
		status, hdr := do(c.method, c.path, c.body, c.user, c.pw)
		if status != c.want {
			t.Errorf("%s: %d, want %d", c.name, status, c.want)
		}
		if c.want == 401 && hdr.Get("WWW-Authenticate") != `Basic realm="commandcenter"` {
			t.Errorf("%s: no Basic challenge (%q)", c.name, hdr.Get("WWW-Authenticate"))
		}
	}
}
