// Package server routes HTTP requests to repository handlers.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
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

	// UploadIdle is how long a request body may stall before the connection
	// is cut; 0 means UploadIdleTimeout.
	UploadIdle time.Duration

	// Requests, if set, is told about each finished request to a known
	// repository: its status, request and response bytes and duration
	// (metrics.Recorder.Request).
	Requests func(repo string, status int, bytesIn, bytesOut int64, d time.Duration)

	// UI registers the web UI routes; nil serves no UI.
	UI interface{ Routes(*http.ServeMux) }

	// Nexus serves the Nexus REST API compatibility layer under
	// /service/rest/v1/ (nexusapi.Handler); nil serves only the status ping.
	Nexus http.Handler

	// Docker serves the Docker/OCI registry API at /v2/ and renders its
	// errors; nil serves none. It authenticates Bearer tokens there itself
	// (the tokens its /v2/token issues); Basic credentials are checked here.
	Docker interface {
		http.Handler
		repo.ErrorWriter
	}

	// TerraformDiscovery, if set, answers GET /.well-known/terraform.json,
	// Terraform's service discovery at the host root: the document naming
	// the one terraform repository this host serves (terraform addresses a
	// registry by host alone), or nil for a 404. It is public: terraform
	// reads it before sending credentials.
	TerraformDiscovery func() []byte

	// ReadOnly, if set, returns why uploads and deletes are refused now
	// (recovery mode), "" while they are not (recovery.go).
	ReadOnly func() string
	// Seen, if set, is told the principal (Owner, "" for anonymous) and the
	// client address (host:port) of each request Requests is told about
	// (metrics.Recorder.Seen: the monthly distinct users and addresses).
	Seen func(user, addr string)
}

const realm = `Basic realm="commandcenter"`

// UploadIdleTimeout is how long a request body may go without data, so a
// stalled client cannot hold the connection or a per-key lock.
const UploadIdleTimeout = 60 * time.Second

// anonymousBodyTimeout bounds the whole body of a request from a caller who is
// not signed in (at most the idle timeout). No upload is anonymous, so such a
// body is small: a login, which carries its password in the body.
const anonymousBodyTimeout = 5 * time.Second

// requestIDRE accepts client-supplied request ids; anything else is replaced.
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// crossOrigin refuses non-safe cross-origin browser requests (CSRF). A
// request with neither Sec-Fetch-Site nor Origin (every CLI client) passes.
var crossOrigin http.CrossOriginProtection

// The SAML assertion consumer service is a cross-site form post by design
// (the HTTP-POST binding): the signed response and the login state cookie
// protect it (ui/saml.go).
func init() { crossOrigin.AddInsecureBypassPattern("POST /-/sso/saml/acs") }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /service/rest/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /-/metrics", s.metrics)
	mux.HandleFunc("/repository/{name}/{path...}", s.repository)
	mux.HandleFunc("GET /.well-known/terraform.json", func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if s.TerraformDiscovery != nil {
			body = s.TerraformDiscovery()
		}
		if body == nil {
			http.Error(w, "no terraform repository", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
	if s.Docker != nil {
		mux.Handle("/v2/", s.Docker)
	}
	if s.UI != nil {
		s.UI.Routes(mux) // still behind wrap: authenticated and logged
	}
	if s.Nexus != nil {
		mux.Handle("/service/rest/v1/", s.Nexus) // the exact status route above stays the plain ping
		mux.Handle("GET /service/rest/swagger.json", s.Nexus)
		mux.Handle("GET /service/rest/atlas/system-information", s.Nexus)
		mux.Handle("/service/rest/beta/", s.Nexus) // Nexus's deprecated twins, rewritten to v1 there
		mux.Handle("GET /service/metrics/", s.Nexus)
		mux.Handle("GET /service/rest/metrics/ping", s.Nexus)
		mux.Handle("GET /service/rest/metrics/prometheus", s.Nexus)
		mux.Handle("GET /service/rest/metrics/data", s.Nexus)
		mux.Handle("GET /service/rest/metrics/threads", s.Nexus)
	}
	return s.wrap(s.refuseWrites(mux))
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
	p := auth.FromContext(r.Context())
	if !p.Anonymous() {
		hd.Set("Cache-Control", "private") // authorized content must not enter shared caches
	}
	rp := s.Registry.Lookup(r.Context(), r.PathValue("name"))
	if rp == nil {
		// Whoever could not read a repository of this name (an unknown name
		// follows the global anonymousRead and authenticatedRead) gets the
		// answer of an unreadable one, so hidden names cannot be probed.
		if !s.Auth.Can(p, r.PathValue("name"), auth.Read) {
			deny(w, r, p, nil)
			return
		}
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
	if !s.Auth.CanPath(p, target, acc, path) {
		if !s.Auth.Can(p, rp.Name, auth.Read) {
			h = nil // its format's error body would tell it from a missing repository
		}
		deny(w, r, p, h)
		return
	}
	// A proxy's routing rule refuses the path before anything is served,
	// cached or not (a group member's rule makes the member skip the path).
	if rp.Type == "proxy" && !s.Registry.RouteAllowed(rp, path) {
		err := repo.Errorf(http.StatusForbidden, "path %q is blocked by routing rule %q", "/"+path, rp.RoutingRule)
		if ew, ok := h.(repo.ErrorWriter); ok {
			ew.WriteError(w, r, err)
		} else {
			repo.WriteError(w, r, err)
		}
		return
	}
	r = r.WithContext(auth.WithPath(r.Context(), path)) // for the handlers' own Permits checks
	// Only callers who may use the repository learn that it is offline. A
	// group write goes to its writeMember, so an offline one answers the same
	// 503 (final review M4); a group read whose members are all offline
	// does too, not a 404 that clients cache (QA N-27).
	if !rp.IsOnline() || (acc.IsWrite() && rp.WriteTarget != nil && !rp.WriteTarget.IsOnline()) ||
		(acc == auth.Read && rp.MembersOffline()) {
		writeOffline(w)
		return
	}
	if acc.IsWrite() && s.refused(w, r, errorWriter(h)) {
		return
	}
	h.Serve(w, r, rp, path)
}

// ServeRepository serves r as a request for /repository/{name}/{path...}:
// its "name" and "path" path values set and its caller in its context
// (auth.WithPrincipal), without Handler's authentication, request log and
// body limits. It is for in-process requests (pull replication's
// prefetch), which then go through the same checks and format handler as
// a client's.
func (s *Server) ServeRepository(w http.ResponseWriter, r *http.Request) { s.repository(w, r) }

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

// wrap assigns a request id, bounds the request body, authenticates the
// caller and logs the request.
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
				name, ok := strings.CutPrefix(r.URL.Path, "/repository/")
				if !ok {
					name, ok = strings.CutPrefix(r.URL.Path, "/v2/")
				}
				if ok {
					name, _, _ = strings.Cut(name, "/")
					if s.Registry.Get(name) != nil {
						// ponytail: Content-Length only; a chunked upload counts 0 bytes in.
						s.Requests(name, status, max(r.ContentLength, 0), rec.bytes, time.Since(start))
						if s.Seen != nil {
							s.Seen(p.Owner(), r.RemoteAddr)
						}
					}
				}
			}
		}()
		// Before authentication, so no answer (401, 404, 503, ...) leaves a
		// stalled body holding the connection (QA N-5). On a copy of r:
		// net/http checks its own r.Body after the handler. The address
		// keys the sign-in throttle (behind a proxy it is the proxy's).
		r = r.WithContext(auth.WithClientAddr(r.Context(), r.RemoteAddr))
		get := r.Method == http.MethodGet || r.Method == http.MethodHead
		if limit := s.maxUpload(); limit > 0 && !get {
			// Inside the idle body, which a handler's own IdleBody (npm
			// publish) must find to replace the idle value.
			r.Body = http.MaxBytesReader(rec, r.Body, limit)
		}
		idle := s.UploadIdle
		if idle <= 0 {
			idle = UploadIdleTimeout
		}
		r.Body = repo.IdleBody(w, r, idle)
		defer r.Body.Close()
		if get {
			// No GET or HEAD handler reads a body, and net/http drains it
			// without a deadline once a long response starts: drain it now.
			r.Body.Close()
		}
		// A browser replays Basic credentials it cached after our challenge
		// on a cross-site form post too: refuse such writes, on every route,
		// before authenticating.
		writeErr := repo.WriteError
		if s.Docker != nil && strings.HasPrefix(r.URL.Path, "/v2/") {
			writeErr = s.Docker.WriteError
		}
		if err := crossOrigin.Check(r); err != nil && !s.fromBaseURL(r) {
			writeErr(rec, r, repo.Errorf(http.StatusForbidden, "%v", err))
			return
		}
		var err error
		// A registry token is the registry's to check (it is no API token);
		// its bearer is not anonymous either, so no anonymous body deadline.
		bearer := s.Docker != nil && strings.HasPrefix(r.URL.Path, "/v2/") && isBearer(r)
		if !bearer {
			p, err = s.Auth.Authenticate(r)
		}
		if err != nil && isLogin(r) {
			// npm sends its stale saved token with the login PUT; the password in
			// the body is what counts there.
			p, err = auth.Principal{}, nil
		}
		if wait := auth.RetryAfter(rec.Header(), err); wait > 0 {
			writeErr(rec, r, repo.Errorf(http.StatusTooManyRequests, "too many failed sign-ins; try again in %d seconds", wait))
		} else if err != nil {
			if r.Header.Get(auth.UIHeader) != "1" { // the web UI handles 401 itself: no browser dialog
				rec.Header().Set("WWW-Authenticate", realm)
			}
			msg := "invalid credentials"
			if errors.Is(err, auth.ErrUserTokenRequired) {
				msg = "a user token is required: send its name code and pass code instead of the user name and password"
			}
			writeErr(rec, r, repo.Errorf(http.StatusUnauthorized, "%s", msg))
		} else {
			if p.Anonymous() && !bearer {
				// Read before anyone is signed in: a byte per idle period
				// must not hold the connection for weeks (QA N-5).
				r.Body = repo.WithDeadline(r.Body, time.Now().Add(min(idle, anonymousBodyTimeout)))
			}
			next.ServeHTTP(rec, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		}
	})
}

// isBearer reports whether r carries a Bearer token.
func isBearer(r *http.Request) bool {
	scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	return strings.EqualFold(scheme, "Bearer")
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

// shedAfter is how long a connection must have been idle between requests,
// or waiting for its first request's headers, before LimitConns may close it
// to make room.
const shedAfter = time.Second

// LimitConns makes srv serve at most n connections at once. When n are open,
// Accept makes room by closing the connection that has been idle between
// requests, or waiting for its first request's headers, the longest, if that
// is at least shedAfter: holding such connections costs a client nothing, so
// they must not lock others out (QA N-5). A connection serving a request is
// never closed; while none qualifies, the new one waits. LimitConns sets
// srv.ConnState.
func LimitConns(srv *http.Server, ln net.Listener, n int) net.Listener {
	l := &limitListener{Listener: ln, n: n, conns: map[net.Conn]connState{}, changed: make(chan struct{}, 1), done: make(chan struct{})}
	srv.ConnState = l.track
	return l
}

type limitListener struct {
	net.Listener
	n       int
	mu      sync.Mutex
	conns   map[net.Conn]connState // being served; Accept drops those it closes
	changed chan struct{}          // a connection changed state
	done    chan struct{}          // closed by Close
	once    sync.Once
}

// Close also ends an Accept that waits for a slot: Server.Shutdown and Close
// wait for Serve to return before they close any connection.
func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type connState struct {
	state http.ConnState
	since time.Time
}

func (l *limitListener) track(c net.Conn, st http.ConnState) {
	l.mu.Lock()
	if _, ok := l.conns[c]; ok {
		if st == http.StateClosed || st == http.StateHijacked {
			delete(l.conns, c)
		} else {
			l.conns[c] = connState{st, time.Now()}
		}
	}
	l.mu.Unlock()
	select {
	case l.changed <- struct{}{}:
	default:
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	var shed net.Conn
	for shed == nil && len(l.conns) >= l.n {
		var wait time.Duration
		if shed, wait = l.cheapest(time.Now()); shed != nil {
			delete(l.conns, shed)
		} else {
			l.mu.Unlock()
			select {
			case <-l.changed:
			case <-time.After(wait):
			case <-l.done:
				c.Close()
				return nil, net.ErrClosed
			}
			l.mu.Lock()
		}
	}
	l.conns[c] = connState{http.StateNew, time.Now()}
	l.mu.Unlock()
	if shed != nil {
		shed.Close() // its read fails and net/http ends it
	}
	return c, nil
}

// cheapest returns the connection idle or waiting for headers the longest if
// that has been shedAfter, else nil and when to look again.
func (l *limitListener) cheapest(now time.Time) (net.Conn, time.Duration) {
	var shed net.Conn
	var oldest time.Time
	wait := shedAfter
	for c, s := range l.conns {
		if s.state != http.StateNew && s.state != http.StateIdle {
			continue
		}
		if left := shedAfter - now.Sub(s.since); left > 0 {
			wait = min(wait, left)
		} else if shed == nil || s.since.Before(oldest) {
			shed, oldest = c, s.since
		}
	}
	return shed, wait
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

// fromBaseURL reports whether r's Origin is the origin of the live baseURL,
// which crossOrigin then trusts: a reverse proxy that rewrites Host (nginx's
// default) breaks its Origin-Host match, which pages without Sec-Fetch-Site
// (plain HTTP) depend on. Compared per request rather than AddTrustedOrigin,
// which would keep trusting a baseURL after it changed.
func (s *Server) fromBaseURL(r *http.Request) bool {
	snap := s.Auth.Snapshot()
	if snap == nil {
		return false
	}
	u, err := url.Parse(snap.Settings().BaseURL)
	return err == nil && strings.EqualFold(r.Header.Get("Origin"), u.Scheme+"://"+u.Host)
}
