// Package ui serves the web UI: a JSON API under /-/api/ and an embedded
// single-page app under /ui/.
package ui

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/config"
	"commandcenter/internal/logring"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/metrics"
	"commandcenter/internal/oidc"
	"commandcenter/internal/repo"
	"commandcenter/internal/tasks"
	"commandcenter/internal/vuln"
	"commandcenter/internal/webhook"
)

//go:embed static
var staticFiles embed.FS

const csp = "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'"

// UI holds what the UI handlers need. Auth.Sessions must be set.
type UI struct {
	Repos   *repo.Registry // live repository definitions; read a Snapshot once per request
	Auth    *auth.Authenticator
	DB      *meta.DB
	Store   *repo.Store   // live blob stores (the name→store map) and routing; nil disables the blob-store API
	BaseURL func() string // the live baseURL setting, without trailing slash; nil reads the auth snapshot
	Version string
	Node    string // this process's node id (meta.NewNodeID); "" outside a server
	Started time.Time
	Tasks   *tasks.Service // scheduled tasks and runs; nil serves 501
	Log     *slog.Logger
	Events  repo.EventSink // activity events; nil records none
	Logs    *logring.Ring  // recent log records for the log viewer; nil serves none
	Mail    Mailer         // synchronous sender for the SMTP test; nil serves 501

	Metrics  *metrics.Recorder // process-lifetime request counters for /-/metrics/prometheus; nil exports none
	LogLevel *slog.LevelVar    // the root logger's level (GET/PUT /-/api/logging); nil serves 501

	Config     *config.Config          // read-only; never serialised wholesale
	DataDir    string                  // blob root, for the health and filestore checks
	FetchSlots func() (inUse, max int) // nil skips the fetch-slot check
	Deleters   *cleanup.Registry       // component deletes in hosted repositories; nil serves 501
	Webhooks   *webhook.Dispatcher     // webhook delivery and reload after a change; nil serves 501
	Vuln       *vuln.Service           // vulnerability data and the proxy firewall; nil serves 501

	// Remote reports a proxy's live remote state (proxy.Fetcher.Remote);
	// nil shows none.
	Remote func(rp *repo.Repository) repo.RemoteStatus
	// Tags and staging (staging.go): the search that selects components and
	// the format importers of a staging move; nil serves 501 or refuses the format.
	FindComponents ComponentFinder
	Importers      map[string]repo.ComponentImporter

	smtpTest       smtpTestState              // rate limit of the SMTP test send
	settingsLoaded func()                     // test hook: runs between putSettings' read and its write
	accessChecked  func()                     // test hook: runs after an access change passed its lockout check, before the write
	memReader      func() (used, limit int64) // test hook; nil reads the runtime
	blobChecker    func() (bool, string)      // test hook; nil runs checkBlobStore
	blobBusy       atomic.Bool                // a blob check goroutine is still running

	// OIDC login (sso.go): the provider client of the configuration it
	// was built from.
	oidcMu   sync.Mutex
	oidcCfg  meta.OIDC
	oidcProv *oidc.Provider
	saml     samlRealm // SAML login (saml.go)

	// Heavy queries (search, stats) share heavySlots slots and the stats are
	// cached; the function fields and now are test hooks.
	slotsOnce      sync.Once
	slots          chan struct{}
	repoCache      ttlCache[map[string]meta.RepoStat]
	blobCache      ttlCache[blobTotals]
	repoStatsQuery func(context.Context) (map[string]meta.RepoStat, error)
	blobStatsQuery func(context.Context) (int64, int64, error)
	now            func() time.Time

	// ops.go: the per-store totals cache and the in-process API mux of the
	// support bundle.
	storeCache   ttlCache[map[string]blobTotals]
	internalOnce sync.Once
	internal     http.Handler

	// quotaMu guards quotaViolated: which blob stores the last check found
	// over their soft quota, so the blobstore.quota event fires once per flip.
	quotaMu       sync.Mutex
	quotaViolated map[string]bool
}

// accessLock is the cluster lock of access changes (meta.DB.Lock): users,
// roles, realms and the settings that name roles are loaded, checked against
// a lockout, written and reloaded under it, whichever node takes the request
// (final review I3). Lock order (meta/cluster.go): it comes before the
// registry's.
const accessLock = "access"

// lockAccess takes the access lock. It answers the request and reports
// false when the lock cannot be had.
func (u *UI) lockAccess(w http.ResponseWriter, r *http.Request) (unlock func(), ok bool) {
	unlock, err := u.DB.Lock(r.Context(), accessLock)
	if err != nil {
		u.fail(w, r, err)
		return nil, false
	}
	return unlock, true
}

// reloadAccess reloads the auth snapshot after an access or settings write
// and tells the other nodes to do the same.
func (u *UI) reloadAccess(ctx context.Context) error {
	if err := u.Auth.Reload(ctx); err != nil {
		return err
	}
	u.notify(ctx, accessLock)
	return nil
}

// notify tells every node that what it caches of topic changed
// (meta.DB.Notify), after the change committed. A node that misses it
// re-reads within the app's reload interval.
func (u *UI) notify(ctx context.Context, topic string) {
	if err := u.DB.Notify(context.WithoutCancel(ctx), topic); err != nil && u.Log != nil {
		u.Log.Warn("could not tell the other nodes of a change; they apply it within seconds", "topic", topic, "err", logsafe.Err(err))
	}
}

// Routes registers "/", "GET /ui/", "/-/api/" and the "/-/sso/" login
// handlers on mux.
func (u *UI) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/{$}", u.root)
	mux.Handle("GET /ui/", u.static())
	mux.Handle("/-/api/", u.apiHandler())
	mux.HandleFunc("GET /-/metrics/prometheus", u.prometheus)
	u.ssoRoutes(mux)
	u.samlRoutes(mux)
}

// root redirects to the UI. Only GET and HEAD are redirected; other methods
// keep the 404 that "/" answered before the UI existed.
func (u *UI) root(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

func securityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
}

// static serves the embedded files. Unknown paths serve index.html: routing
// is hash-based, so this only matters for typos.
func (u *UI) static() http.Handler {
	// Go's built-in table has no entry for web fonts; nosniff would then
	// make browsers refuse an octet-stream font.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // the embed directive guarantees the directory
	}
	files := http.FileServerFS(sub)
	etags := staticETags(sub) // computed once; the files are embedded and never change
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		securityHeaders(h)
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		name := r.URL.Path[len("/ui/"):]
		if name != "" {
			if st, err := fs.Stat(sub, name); err != nil || st.IsDir() || !fs.ValidPath(name) {
				name = ""
			}
		}
		r2 := new(http.Request)
		*r2 = *r
		u2 := *r.URL
		u2.Path, u2.RawPath = "/"+name, ""
		r2.URL = &u2
		key := name
		if key == "" {
			key = "index.html"
		}
		// no-cache + ETag: browsers revalidate (cheap 304) and pick up new JS after an upgrade.
		h.Set("Cache-Control", "no-cache")
		if tag := etags[key]; tag != "" {
			h.Set("ETag", tag)
		}
		files.ServeHTTP(w, r2)
	})
}

// apiHandler serves /-/api/ from its own mux so that unknown paths and
// wrong methods still answer JSON, with the API headers.
func (u *UI) apiHandler() http.Handler {
	api := http.NewServeMux()
	u.apiRoutes(api)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		securityHeaders(h)
		h.Set("Cache-Control", "no-store")
		handler, pattern := api.Handler(r)
		if pattern == "" {
			// The mux's own 404 or 405: capture its status and Allow header.
			c := &catcher{h: http.Header{}}
			handler.ServeHTTP(c, r)
			if c.status >= 300 && c.status < 400 {
				api.ServeHTTP(w, r) // path-cleaning redirect: let the mux answer it
				return
			}
			if allow := c.h.Get("Allow"); allow != "" {
				h.Set("Allow", allow)
			}
			status := c.status
			if status == 0 || status == http.StatusOK {
				status = http.StatusNotFound
			}
			writeError(w, status, http.StatusText(status))
			return
		}
		api.ServeHTTP(w, r)
	})
}

// catcher records a response's status and headers and discards its body.
type catcher struct {
	h      http.Header
	status int
}

func (c *catcher) Header() http.Header { return c.h }
func (c *catcher) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return len(b), nil
}
func (c *catcher) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail answers 500 and logs err; internal details are not exposed.
func (u *UI) fail(w http.ResponseWriter, r *http.Request, err error) {
	id := r.Header.Get("X-Request-Id")
	if ctxDone(err) { // the caller left or timed out waiting for a slot
		if u.Log != nil && errors.Is(err, context.DeadlineExceeded) {
			u.Log.Warn("ui api timed out", "id", id, "method", r.Method, "path", r.URL.Path, "err", logsafe.Err(err))
		}
		writeError(w, http.StatusServiceUnavailable, "server busy")
		return
	}
	if u.Log != nil {
		u.Log.Error("ui api failed", "id", id, "method", r.Method, "path", r.URL.Path, "err", logsafe.Err(err))
	}
	writeError(w, http.StatusInternalServerError, "internal error (request id "+id+")")
}

// rfc3339 formats t in UTC; the zero time is "".
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// staticETags maps every embedded file to a strong ETag, the quoted hex sha256
// of its content.
func staticETags(fsys fs.FS) map[string]string {
	tags := map[string]string{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		tags[p] = `"` + hex.EncodeToString(sum[:]) + `"`
		return nil
	})
	return tags
}

// baseURL is the current external URL without a trailing slash. It is read
// per request, so a change of the baseURL setting applies at once.
func (u *UI) baseURL() string {
	if u.BaseURL != nil {
		return strings.TrimRight(u.BaseURL(), "/")
	}
	return strings.TrimRight(u.Auth.Snapshot().Settings().BaseURL, "/")
}
