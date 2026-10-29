package terraform

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
// add (a format:terraform privilege works), a delete needs delete, reads
// are anonymous when anonymousRead is on and a 401 otherwise; an API
// token works as `Authorization: Bearer` (what ~/.terraformrc credentials
// send) and as the Basic password. The host-root discovery is public.
func TestPrivileges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	db := e.h.Store.DB
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	for _, r := range []meta.Role{
		{ID: "uploaders", Privileges: []meta.Privilege{{Target: "format:terraform", Actions: []string{meta.ActionRead, meta.ActionAdd}}}},
		{ID: "deleters", Privileges: []meta.Privilege{{Target: "repo:tf-hosted", Actions: []string{meta.ActionRead, meta.ActionDelete}}}},
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
	_, readerToken, err := a.CreateToken(ctx, "reader")
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	defs := []config.Repository{
		{Name: "tf-hosted", Format: "terraform", Type: "hosted", WritePolicy: "allow_once", AnonymousRead: &yes},
		{Name: "tf-private", Format: "terraform", Type: "hosted", AnonymousRead: &no},
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
	s := &server.Server{Registry: reg, Handlers: map[string]repo.Handler{"terraform": e.h}, Auth: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		TerraformDiscovery: func() []byte { return DiscoveryDoc("https://cc.example/repository/tf-hosted") }}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	do := func(method, path string, body []byte, authz string) (int, http.Header, string) {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		resp, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, resp.Header, string(b)
	}
	basic := func(user, pw string) string {
		r, _ := http.NewRequest("GET", "/", nil)
		r.SetBasicAuth(user, pw)
		return r.Header.Get("Authorization")
	}
	mod := tgzBytes(t, map[string]string{"main.tf": "x"})
	up := "/repository/tf-hosted/v1/modules/acme/demo/aws/"
	cases := []struct {
		name, method, path string
		body               []byte
		authz              string
		want               int
	}{
		{"bearer token", "PUT", up + "1.0.0", mod, "Bearer " + token, 201},
		{"password", "PUT", up + "1.1.0", mod, basic("uploader", "pw"), 201},
		{"token as Basic password", "PUT", up + "1.2.0", mod, basic("uploader", token), 201},
		{"wrong password", "PUT", up + "1.3.0", mod, basic("uploader", "nope"), 401},
		{"anonymous upload", "PUT", up + "1.3.0", mod, "", 401},
		{"reader upload", "PUT", up + "1.3.0", mod, "Bearer " + readerToken, 403},
		{"uploader delete", "DELETE", up + "1.0.0", nil, "Bearer " + token, 403},
		{"deleter delete", "DELETE", up + "1.0.0", nil, basic("deleter", "pw"), 204},
		{"anonymous versions", "GET", up + "versions", nil, "", 200},
		{"anonymous download", "GET", up + "1.1.0/download", nil, "", 204},
		{"anonymous archive", "GET", "/repository/tf-hosted/modules/acme/demo/aws/1.1.0.tgz", nil, "", 200},
		{"anonymous discovery", "GET", "/repository/tf-hosted/.well-known/terraform.json", nil, "", 200},
		{"anonymous, private", "GET", "/repository/tf-private/v1/modules/acme/demo/aws/versions", nil, "", 401},
		{"bearer, private", "GET", "/repository/tf-private/v1/modules/acme/demo/aws/versions", nil, "Bearer " + readerToken, 404},
		{"root discovery", "GET", "/.well-known/terraform.json", nil, "", 200},
	}
	for _, c := range cases {
		status, hdr, body := do(c.method, c.path, c.body, c.authz)
		if status != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, status, c.want, body)
		}
		if c.want == 401 && hdr.Get("WWW-Authenticate") != `Basic realm="commandcenter"` {
			t.Errorf("%s: no Basic challenge (%q)", c.name, hdr.Get("WWW-Authenticate"))
		}
		if c.name == "root discovery" && body != `{"modules.v1":"https://cc.example/repository/tf-hosted/v1/modules/","providers.v1":"https://cc.example/repository/tf-hosted/v1/providers/"}`+"\n" {
			t.Errorf("root discovery: %s", body)
		}
	}
}
