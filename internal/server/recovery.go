package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
)

// Recovery mode (meta.RecoveryMode) makes the instance read-only for
// clients: the uploads and deletes of every format (repository writes, as
// the format's access decision says, and Docker pushes) and the API calls
// that upload, delete or move components answer 503 with the reason.
// Downloads and proxy fetches go on, and so do the admin APIs, the tasks and the
// readiness check (GET /service/rest/v1/status/writable), which a load
// balancer follows: recovery mode must not take the nodes out of service.

// contentWrites matches the API routes that upload, delete or move components (the
// patterns they are registered with).
var contentWrites = func() *http.ServeMux {
	m := http.NewServeMux()
	match := func(http.ResponseWriter, *http.Request) {}
	m.HandleFunc("DELETE /-/api/repositories/{name}/components", match)
	m.HandleFunc("POST /-/api/staging/move", match)
	m.HandleFunc("POST /-/api/staging/delete", match)
	m.HandleFunc("DELETE /service/rest/v1/components/{id}", match)
	m.HandleFunc("POST /service/rest/v1/components", match)
	m.HandleFunc("DELETE /service/rest/v1/assets/{id}", match)
	m.HandleFunc("POST /service/rest/v1/staging/move/{destination}", match)
	m.HandleFunc("POST /service/rest/v1/staging/delete", match)
	return m
}()

// refuseWrites answers 503 to Docker pushes and to the content-changing API
// calls while ReadOnly says so; repository writes are refused in
// repository, which knows their access.
func (s *Server) refuseWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case s.Docker != nil && strings.HasPrefix(r.URL.Path, "/v2/") && r.URL.Path != "/v2/token" &&
			r.Method != http.MethodGet && r.Method != http.MethodHead:
			if s.refused(w, r, s.Docker.WriteError) {
				return
			}
		case pattern(contentWrites, r) != "":
			if s.refused(w, r, writeJSONError) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func pattern(m *http.ServeMux, r *http.Request) string {
	_, p := m.Handler(r)
	return p
}

// writeJSONError answers err as the APIs do: {"error": "..."}, or
// {"id": "*", "message": "..."} on the Nexus API.
func writeJSONError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := repo.ErrorMessage(r, err)
	body := map[string]string{"error": msg}
	if strings.HasPrefix(r.URL.Path, "/service/rest/") {
		body = map[string]string{"id": "*", "message": msg} // the Nexus API's error shape
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// refused answers 503 with the reason when ReadOnly gives one, with
// writeErr (the format's error rendering), and reports whether it did.
func (s *Server) refused(w http.ResponseWriter, r *http.Request, writeErr func(http.ResponseWriter, *http.Request, error)) bool {
	if s.ReadOnly == nil {
		return false
	}
	msg := s.ReadOnly()
	if msg == "" {
		return false
	}
	w.Header().Set("Retry-After", "300")
	writeErr(w, r, repo.Errorf(http.StatusServiceUnavailable, "%s", msg))
	return true
}

// errorWriter is h's error rendering, repo.WriteError when it has none.
func errorWriter(h repo.Handler) func(http.ResponseWriter, *http.Request, error) {
	if ew, ok := h.(repo.ErrorWriter); ok {
		return ew.WriteError
	}
	return repo.WriteError
}
