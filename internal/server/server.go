// Package server routes HTTP requests to repository handlers.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/sysinfo"
)

type Server struct {
	Registry  *repo.Registry          // live repository definitions
	Handlers  map[string]repo.Handler // keyed by format
	Auth      *auth.Authenticator
	Log       *slog.Logger
	MaxUpload int64 // caps request bodies of uploads; 0 means no cap

	// UploadIdle is how long an upload body may stall before the connection
	// is cut; 0 means UploadIdleTimeout.
	UploadIdle time.Duration

	// Requests, if set, is told about each finished request to a known
	// repository (any status) with its response bytes (metrics.Recorder.Request).
	Requests func(repo string, bytes int64)

	// UI registers the web UI routes; nil serves no UI.
	UI interface{ Routes(*http.ServeMux) }
}

const realm = `Basic realm="commandcenter"`

// UploadIdleTimeout is how long a non-GET/HEAD request body may go without
// data, so a stalled client cannot hold the connection or a per-key lock.
const UploadIdleTimeout = 60 * time.Second

// requestIDRE accepts client-supplied request ids; anything else is replaced.
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /service/rest/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /-/metrics", s.metrics)
	mux.HandleFunc("/repository/{name}/{path...}", s.repository)
	if s.UI != nil {
		s.UI.Routes(mux) // still behind wrap: authenticated and logged
	}
	return s.wrap(mux)
}

func (s *Server) repository(w http.ResponseWriter, r *http.Request) {
	// A proxy whose remote leads back to this instance: refuse instead of
	// stalling until the remote's timeout.
	if proxy.HasHop(r.Header.Values(proxy.HopHeader)) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLoopDetected)
		io.WriteString(w, `{"error":"proxy loop detected"}`+"\n")
		return
	}
	if hops := r.Header.Values(proxy.HopHeader); len(hops) > 0 {
		r = r.WithContext(proxy.WithHops(r.Context(), strings.Join(hops, ", ")))
	}
	// Stored content is untrusted: never let a browser run it as part of this
	// origin, even when someone opens a download link.
	hd := w.Header()
	hd.Set("Content-Security-Policy", "sandbox allow-downloads; default-src 'none'")
	hd.Set("X-Content-Type-Options", "nosniff")
	if !auth.FromContext(r.Context()).Anonymous() {
		hd.Set("Cache-Control", "private") // authorized content must not enter shared caches
	}
	rp := s.Registry.Get(r.PathValue("name"))
	if rp == nil {
		repo.WriteError(w, r, repo.Errorf(http.StatusNotFound, "repository %q not found", r.PathValue("name")))
		return
	}
	h := s.Handlers[rp.Format]
	if h == nil {
		repo.WriteError(w, r, repo.Errorf(http.StatusNotFound, "format %q is not supported", rp.Format))
		return
	}
	path := r.PathValue("path")

	acc := DefaultAccess(r)
	if d, ok := h.(repo.AccessDecider); ok {
		acc = d.Access(r, rp, path)
	}
	// A group write is checked, with the same kind, against its writeMember.
	target := rp.Name
	if acc.IsWrite() && rp.WriteTarget != nil {
		target = rp.WriteTarget.Name
	}
	p := auth.FromContext(r.Context())
	if !s.Auth.Can(p, target, acc) {
		deny(w, r, p, h)
		return
	}
	// Only callers who may use the repository learn that it is offline. A
	// group write goes to its writeMember, so an offline one answers the same
	// 503 (final review M4).
	if !rp.IsOnline() || (acc.IsWrite() && rp.WriteTarget != nil && !rp.WriteTarget.IsOnline()) {
		writeOffline(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		idle := s.UploadIdle
		if idle <= 0 {
			idle = UploadIdleTimeout
		}
		if limit := s.maxUpload(); limit > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		// Outermost, so a handler's own IdleBody (npm publish) replaces the
		// idle value instead of nesting; body-less writes are not wrapped.
		r.Body = repo.IdleBody(w, r, idle)
		defer r.Body.Close() // clears the read deadline
	}
	h.Serve(w, r, rp, path)
}

// DefaultAccess is the access a request needs when its handler is no
// repo.AccessDecider: GET/HEAD read, DELETE deletes, anything else adds.
func DefaultAccess(r *http.Request) auth.Access {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return auth.Read
	case http.MethodDelete:
		return auth.Delete
	}
	return auth.Add
}

// deny rejects the request: 401 with a challenge for anonymous callers, otherwise 403.
// h formats the error when it implements repo.ErrorWriter; it may be nil.
func deny(w http.ResponseWriter, r *http.Request, p auth.Principal, h repo.Handler) {
	var err error
	if p.Anonymous() {
		// The web UI (upload) handles 401 itself; a challenge would pop up
		// the browser's Basic-auth dialog over it.
		if r.Header.Get(auth.UIHeader) != "1" {
			w.Header().Set("WWW-Authenticate", realm)
		}
		err = repo.Errorf(http.StatusUnauthorized, "authentication required")
	} else {
		err = repo.Errorf(http.StatusForbidden, "user %q is not allowed to do this", p.User)
	}
	if ew, ok := h.(repo.ErrorWriter); ok {
		ew.WriteError(w, r, err)
		return
	}
	repo.WriteError(w, r, err)
}

// wrap assigns a request id, authenticates the caller and logs the request.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if !requestIDRE.MatchString(id) {
			id = newID()
			r.Header.Set("X-Request-Id", id)
		}
		rec := &statusRecorder{ResponseWriter: w}
		var p auth.Principal
		// Deferred so the request is logged even when a handler panics
		// (http.ErrAbortHandler mid-stream); the panic still propagates.
		defer func() {
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			s.Log.Info("request", "id", id, "method", r.Method, "path", logsafe.Path(r.URL.Path), "status", status,
				"bytes", rec.bytes, "ms", time.Since(start).Milliseconds(), "user", p.Owner(), "remote", r.RemoteAddr)
			if s.Requests != nil {
				// Only names the registry knows: a scanner cannot add rows.
				if name, ok := strings.CutPrefix(r.URL.Path, "/repository/"); ok {
					name, _, _ = strings.Cut(name, "/")
					if s.Registry.Get(name) != nil {
						s.Requests(name, rec.bytes)
					}
				}
			}
		}()
		p, err := s.Auth.Authenticate(r)
		if err != nil && isLogin(r) {
			// npm sends its stale saved token with the login PUT; the password in
			// the body is what counts there.
			p, err = auth.Principal{}, nil
		}
		if err != nil {
			if r.Header.Get(auth.UIHeader) != "1" { // the web UI handles 401 itself: no browser dialog
				rec.Header().Set("WWW-Authenticate", realm)
			}
			repo.WriteError(rec, r, repo.Errorf(http.StatusUnauthorized, "invalid credentials"))
		} else {
			next.ServeHTTP(rec, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		}
	})
}

// isLogin reports whether r is npm's legacy login request.
func isLogin(r *http.Request) bool {
	return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/-/user/org.couchdb.user:")
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.Admin {
		deny(w, r, p, nil)
		return
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"heapAllocBytes": ms.HeapAlloc,
		"heapSysBytes":   ms.HeapSys,
		"sysBytes":       ms.Sys,
		"numGC":          ms.NumGC,
		"goroutines":     runtime.NumGoroutine(),
		"rssBytes":       sysinfo.RSSBytes(),
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the underlying writer's sendfile fast path.
func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := io.Copy(s.ResponseWriter, r)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// writeOffline answers a request for an offline repository. The cleanup and
// compact tasks do not come through here, so they still run.
func writeOffline(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]string{"error": "repository is offline"})
}

// maxUpload is the current cap on upload bodies: the live setting, or the
// MaxUpload field when there is no Authenticator.
func (s *Server) maxUpload() int64 {
	if s.Auth != nil {
		if snap := s.Auth.Snapshot(); snap != nil {
			return int64(snap.Settings().MaxUploadMiB) << 20
		}
	}
	return s.MaxUpload
}
