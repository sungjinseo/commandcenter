// Package crowdtest is a fake Atlassian Crowd server for tests: the
// usermanagement REST resources the crowd realm uses (authentication, user,
// nested groups, cookie config), behind the application's Basic auth.
package crowdtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// User is a Crowd user.
type User struct {
	Password, Email, DisplayName string
	Inactive                     bool
	Groups                       []string
}

// Server is the fake. URL is the Crowd base URL (with the /crowd context
// path); App and AppPassword are the application credentials it accepts.
type Server struct {
	URL, App, AppPassword string

	srv   *httptest.Server
	mu    sync.Mutex
	users map[string]User
	calls map[string]int // by resource ("authentication", "user", "groups", "cookie")
}

// New starts a server with application "commandcenter" / "app-secret".
func New(t testing.TB) *Server {
	s := &Server{App: "commandcenter", AppPassword: "app-secret", users: map[string]User{}, calls: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	s.URL = s.srv.URL + "/crowd"
	return s
}

// Close stops the server: Crowd is down.
func (s *Server) Close() { s.srv.Close() }

// Put adds or replaces a user.
func (s *Server) Put(name string, u User) {
	s.mu.Lock()
	s.users[name] = u
	s.mu.Unlock()
}

// Calls returns how often a resource was called.
func (s *Server) Calls(resource string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[resource]
}

func fail(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"reason": reason, "message": reason})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if app, pw, ok := r.BasicAuth(); !ok || app != s.App || pw != s.AppPassword {
		fail(w, http.StatusUnauthorized, "APPLICATION_ACCESS_DENIED")
		return
	}
	if r.Header.Get("Accept") != "application/json" {
		fail(w, http.StatusNotAcceptable, "NOT_ACCEPTABLE")
		return
	}
	name := r.URL.Query().Get("username")
	s.mu.Lock()
	u, found := s.users[name]
	resource := map[string]string{"/crowd/rest/usermanagement/1/authentication": "authentication",
		"/crowd/rest/usermanagement/1/user": "user", "/crowd/rest/usermanagement/1/user/group/nested": "groups",
		"/crowd/rest/usermanagement/1/config/cookie": "cookie"}[r.URL.Path]
	s.calls[resource]++
	s.mu.Unlock()
	user := map[string]any{"name": name, "email": u.Email, "display-name": u.DisplayName, "active": !u.Inactive}
	switch {
	case resource == "cookie" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(map[string]any{"domain": ".example.org", "secure": true, "name": "crowd.token_key"})
	case resource == "authentication" && r.Method == http.MethodPost:
		var body struct {
			Value string `json:"value"`
		}
		if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil {
			fail(w, http.StatusBadRequest, "ILLEGAL_ARGUMENT")
		} else if !found {
			fail(w, http.StatusBadRequest, "USER_NOT_FOUND")
		} else if u.Inactive {
			fail(w, http.StatusBadRequest, "INACTIVE_ACCOUNT")
		} else if body.Value != u.Password {
			fail(w, http.StatusBadRequest, "INVALID_USER_AUTHENTICATION")
		} else {
			json.NewEncoder(w).Encode(user)
		}
	case resource == "user" && r.Method == http.MethodGet:
		if !found {
			fail(w, http.StatusNotFound, "USER_NOT_FOUND")
		} else {
			json.NewEncoder(w).Encode(user)
		}
	case resource == "groups" && r.Method == http.MethodGet:
		if !found {
			fail(w, http.StatusNotFound, "USER_NOT_FOUND")
			return
		}
		groups := []map[string]string{}
		for _, g := range u.Groups {
			groups = append(groups, map[string]string{"name": g})
		}
		json.NewEncoder(w).Encode(map[string]any{"expand": "group", "groups": groups})
	default:
		fail(w, http.StatusNotFound, "NOT_FOUND")
	}
}
