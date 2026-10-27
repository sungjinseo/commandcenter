package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/format/npm"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

type fixedSource struct {
	users    []meta.User
	roles    []meta.Role
	settings *meta.Settings // nil: meta.DefaultSettings()
}

func (f fixedSource) LoadAccess(context.Context) (meta.AccessData, error) {
	s := meta.DefaultSettings()
	if f.settings != nil {
		s = *f.settings
	}
	return meta.AccessData{Users: f.users, Roles: f.roles, Settings: s}, nil
}

// editHandler asks for Edit on every non-GET request.
type editHandler struct{ recordHandler }

func (editHandler) Access(r *http.Request, rp *repo.Repository, path string) auth.Access {
	if r.Method == http.MethodGet {
		return auth.Read
	}
	return auth.Edit
}

func TestWriteKindsAndGroupWriteMember(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	role := func(id, target string, actions ...string) meta.Role {
		return meta.Role{ID: id, Privileges: []meta.Privilege{{Target: target, Actions: actions}}}
	}
	src := fixedSource{
		users: []meta.User{
			{ID: "member-editor", PasswordHash: string(h), Roles: []string{"me"}, Source: meta.SourceLocal, Status: meta.StatusActive},
			{ID: "group-editor", PasswordHash: string(h), Roles: []string{"ge"}, Source: meta.SourceLocal, Status: meta.StatusActive},
			{ID: "adder", PasswordHash: string(h), Roles: []string{"ad"}, Source: meta.SourceLocal, Status: meta.StatusActive},
			{ID: "group-adder", PasswordHash: string(h), Roles: []string{"ga"}, Source: meta.SourceLocal, Status: meta.StatusActive},
			{ID: "member-adder", PasswordHash: string(h), Roles: []string{"ma"}, Source: meta.SourceLocal, Status: meta.StatusActive},
		},
		roles: []meta.Role{
			role("me", "repo:releases", meta.ActionEdit),
			role("ge", "repo:public", meta.ActionEdit),
			role("ad", "format:maven", meta.ActionAdd),
			role("ga", "repo:public", meta.ActionAdd),
			role("ma", "repo:releases", meta.ActionAdd),
		},
	}
	a, err := auth.Open(context.Background(), src, noTokens{})
	if err != nil {
		t.Fatal(err)
	}
	repos := []config.Repository{
		{Name: "releases", Format: "maven", Type: "hosted"},
		{Name: "public", Format: "maven", Type: "group", Members: []string{"releases"}, WriteMember: "releases"},
	}
	reg := repo.NewRegistry(nil, repos)
	a.AnonymousRead = reg.AnonymousRead
	a.RepoFormat = func(n string) string {
		if rp := reg.Get(n); rp != nil {
			return rp.Format
		}
		return ""
	}
	run := func(handler repo.Handler, user, method, repoName string) int {
		s := &Server{Registry: reg, Handlers: map[string]repo.Handler{"maven": handler}, Auth: a,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		req, _ := http.NewRequest(method, ts.URL+"/repository/"+repoName+"/x", nil)
		req.SetBasicAuth(user, "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		handler      repo.Handler
		user, method string
		repo         string
		want         int
	}{
		// The group write is checked, with the same kind, on the writeMember.
		{&editHandler{}, "member-editor", "PUT", "public", 200},
		{&editHandler{}, "group-editor", "PUT", "public", 403},
		{&editHandler{}, "adder", "PUT", "releases", 403},
		// Without a decider: PUT adds, DELETE deletes.
		{&recordHandler{}, "adder", "PUT", "public", 200},
		{&recordHandler{}, "adder", "DELETE", "releases", 403},
		{&recordHandler{}, "member-editor", "PUT", "releases", 403},
		// Add on the group alone is not enough; Add on the member is.
		{&recordHandler{}, "group-adder", "PUT", "public", 403},
		{&recordHandler{}, "member-adder", "PUT", "public", 200},
		{&recordHandler{}, "member-adder", "PUT", "releases", 200},
	} {
		if got := run(c.handler, c.user, c.method, c.repo); got != c.want {
			t.Errorf("%s %s on %s: %d, want %d", c.user, c.method, c.repo, got, c.want)
		}
	}
}

// A caller who could not read a repository of some name must not learn from
// the answer whether it exists: a missing repository answers exactly like an
// unreadable one, also where the format has its own error body, and npm's
// ping is no longer public. A caller who could read it still gets 404.
func TestMissingRepositoryAnswersLikeUnreadable(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	reader := meta.User{ID: "reader", PasswordHash: string(h), Source: meta.SourceLocal, Status: meta.StatusActive}
	repos := []config.Repository{
		{Name: "private", Format: "maven", Type: "hosted"},
		{Name: "npm-private", Format: "npm", Type: "hosted"},
	}
	answer := func(ts *httptest.Server, path, user string) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if user != "" {
			req.SetBasicAuth(user, "pw")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("WWW-Authenticate") + " " + res.Header.Get("Content-Type") + " " + string(b)
	}
	for _, c := range []struct{ anonymousRead, authenticatedRead bool }{{true, false}, {false, false}, {false, true}} {
		settings := meta.DefaultSettings()
		settings.AnonymousRead, settings.AuthenticatedRead = c.anonymousRead, c.authenticatedRead
		a, err := auth.Open(context.Background(), fixedSource{users: []meta.User{reader}, settings: &settings}, noTokens{})
		if err != nil {
			t.Fatal(err)
		}
		reg := repo.NewRegistry(nil, repos)
		a.AnonymousRead = reg.AnonymousRead
		s := &Server{Registry: reg, Handlers: map[string]repo.Handler{"maven": &recordHandler{}, "npm": npm.New(nil, nil, a, nil)}, Auth: a,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		for _, user := range []string{"", "reader"} {
			denied := http.StatusUnauthorized
			if user != "" {
				denied = http.StatusForbidden
			}
			canRead := c.anonymousRead || user != "" && c.authenticatedRead
			for _, p := range []struct{ missing, hidden string }{
				{"/repository/ghost/x", "/repository/private/x"},
				{"/repository/ghost/-/ping", "/repository/npm-private/-/ping"},
			} {
				code, got := answer(ts, p.missing, user)
				hcode, want := answer(ts, p.hidden, user)
				if canRead && (code != 404 || hcode != 200) || !canRead && (code != denied || hcode != denied || got != want) {
					t.Errorf("%+v, user %q: %s %d %q; %s %d %q", c, user, p.missing, code, got, p.hidden, hcode, want)
				}
			}
		}
	}
}
