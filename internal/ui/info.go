package ui

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/sysinfo"
)

func (u *UI) infoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/users", u.listUsers)
	mux.HandleFunc("GET /-/api/status/checks", u.statusChecks)
	mux.HandleFunc("GET /-/api/systeminformation", u.systemInformation)
	mux.HandleFunc("GET /-/api/nodes", u.listNodes)
}

// GET /-/api/nodes (admin): the server nodes registered on the database,
// oldest first, and which one answered.
func (u *UI) listNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	nodes, err := u.DB.Nodes(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, map[string]any{"id": n.ID, "hostname": n.Hostname, "version": n.Version, "schema": n.Schema,
			"startedAt": rfc3339(n.StartedAt), "heartbeatAt": rfc3339(n.HeartbeatAt),
			"alive": n.Alive, "draining": n.Draining, "leader": n.Leader, "runningTask": n.RunningTask})
	}
	writeJSON(w, http.StatusOK, map[string]any{"self": u.Node, "nodes": out})
}

// checkCluster lists the live nodes and fails when this node is not ready
// (draining, an incompatible schema, the lock connection or the listener
// down).
func (u *UI) checkCluster(rctx context.Context) func() (bool, string) {
	return func() (bool, string) {
		ctx, cancel := context.WithTimeout(rctx, 2*time.Second)
		defer cancel()
		nodes, err := u.DB.Nodes(ctx)
		if err != nil {
			return false, "cannot read the nodes: " + err.Error()
		}
		var live []string
		for _, n := range nodes {
			if !n.Alive {
				continue
			}
			s := n.ID
			if n.Leader {
				s += " (leader)"
			}
			if n.Draining {
				s += " (draining)"
			}
			live = append(live, s)
		}
		msg := "no live node is registered (not serving)"
		if len(live) > 0 {
			msg = fmt.Sprintf("%d live node(s): %s", len(live), strings.Join(live, ", "))
		}
		if err := u.DB.Ready(ctx); err != nil {
			return false, msg + "; this node is not ready: " + err.Error()
		}
		return true, msg
	}
}

// formatDuration renders a config duration like the config file does:
// "-1" for never, "24h", "10m", "30s".
func formatDuration(d config.Duration) string { return d.Text() }

// addPolicy fills the admin-only policy fields that apply to the
// repository's type.
func addPolicy(out *repoJSON, rp *repo.Repository) {
	switch rp.Type {
	case "hosted":
		out.VersionPolicy = rp.VersionPolicy
		out.WritePolicy = rp.WritePolicy
		out.Depth = rp.Depth
		out.Distribution, out.Component, out.SigningKeySet = rp.Distribution, rp.Component, rp.SigningKey != ""
		out.StrictContentTypeValidation = rp.StrictContentTypeValidation
	case "proxy":
		out.MetadataMaxAge = formatDuration(rp.MetadataMaxAge)
		out.ContentMaxAge = formatDuration(rp.ContentMaxAge)
		out.NegativeCacheTTL = formatDuration(rp.NegativeCacheTTL)
	}
	if rp.Format == "raw" || rp.Format == "gitlfs" {
		out.ContentDisposition = rp.ContentDisposition
		if out.ContentDisposition == "" {
			out.ContentDisposition = "inline"
		}
	}
}

func (u *UI) listUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	users := []map[string]any{}
	s := u.Auth.Snapshot()
	// The stored records add the editable fields (plan 12); the snapshot has no
	// password hashes, so hasPassword comes from here.
	stored := map[string]meta.User{}
	if list, err := u.DB.ListUsers(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	} else {
		for _, x := range list {
			stored[x.ID] = x
		}
	}
	// summarize lists the privileges of roles. writeRepositories keeps its
	// plan 10 meaning: repository names the user may upload to, or "*" for
	// all. format: targets are not repositories; privileges lists every
	// target as it is.
	summarize := func(roles []string) (bool, []string, []map[string]any) {
		isAdmin := false
		writes := map[string]bool{}
		privs := []map[string]any{}
		for _, id := range roles {
			if id == meta.AdminRole {
				isAdmin = true
			}
			role, _ := s.Role(id)
			for _, p := range role.Privileges {
				privs = append(privs, map[string]any{"role": role.ID, "target": p.Target, "actions": p.Actions})
				if !hasAction(p.Actions, meta.ActionAdd) {
					continue
				}
				if name, ok := strings.CutPrefix(p.Target, "repo:"); ok {
					writes[name] = true
				} else if p.Target == "*" {
					writes["*"] = true
				}
			}
		}
		if isAdmin {
			writes = map[string]bool{"*": true}
		}
		wl := make([]string, 0, len(writes))
		for repo := range writes {
			wl = append(wl, repo)
		}
		sort.Strings(wl)
		return isAdmin, wl, privs
	}
	for _, su := range s.Users() {
		isAdmin, wl, privs := summarize(su.Roles)
		realm := "Local"
		switch su.Source {
		case meta.SourceLDAP:
			realm = "LDAP"
		case meta.SourceOIDC:
			realm = "OIDC"
		case meta.SourceCrowd:
			realm = "Crowd"
		case meta.SourceRUT:
			realm = "RUT"
		case meta.SourceSAML:
			realm = "SAML"
		}
		st := stored[su.ID]
		users = append(users, map[string]any{
			"userId": su.ID, "realm": realm, "roles": su.Roles,
			"admin": isAdmin, "status": su.Status, "writeRepositories": wl, "privileges": privs,
			"id": su.ID, "name": st.Name, "email": st.Email, "source": su.Source, "hasPassword": auth.HashUsable(st.PasswordHash),
			"createdAt": rfc3339(st.CreatedAt), "updatedAt": rfc3339(st.UpdatedAt),
		})
	}
	// Signed-in LDAP users (plan 12 Task 6) with their mapped roles; a
	// stored ldap-source row above holds only their override roles.
	for _, lu := range u.Auth.LDAPUsers() {
		isAdmin, wl, privs := summarize(lu.Roles)
		users = append(users, map[string]any{
			"userId": lu.ID, "realm": "LDAP", "roles": lu.Roles,
			"admin": isAdmin, "status": meta.StatusActive, "writeRepositories": wl, "privileges": privs,
			"id": lu.ID, "name": lu.Name, "email": lu.Email, "source": meta.SourceLDAP, "hasPassword": false,
			"createdAt": "", "updatedAt": "", "directory": true,
		})
	}
	// Signed-in OIDC users likewise; "directory" marks a row without a
	// stored record of its own.
	for _, ou := range u.Auth.OIDCUsers() {
		isAdmin, wl, privs := summarize(ou.Roles)
		users = append(users, map[string]any{
			"userId": ou.ID, "realm": "OIDC", "roles": ou.Roles,
			"admin": isAdmin, "status": meta.StatusActive, "writeRepositories": wl, "privileges": privs,
			"id": ou.ID, "name": ou.Name, "email": ou.Email, "source": meta.SourceOIDC, "hasPassword": false,
			"createdAt": "", "updatedAt": "", "directory": true,
		})
	}
	for _, cu := range u.Auth.CrowdUsers() {
		isAdmin, wl, privs := summarize(cu.Roles)
		users = append(users, map[string]any{
			"userId": cu.ID, "realm": "Crowd", "roles": cu.Roles,
			"admin": isAdmin, "status": meta.StatusActive, "writeRepositories": wl, "privileges": privs,
			"id": cu.ID, "name": cu.Name, "email": cu.Email, "source": meta.SourceCrowd, "hasPassword": false,
			"createdAt": "", "updatedAt": "", "directory": true,
		})
	}
	for _, su := range u.Auth.SAMLUsers() {
		isAdmin, wl, privs := summarize(su.Roles)
		users = append(users, map[string]any{
			"userId": su.ID, "realm": "SAML", "roles": su.Roles,
			"admin": isAdmin, "status": meta.StatusActive, "writeRepositories": wl, "privileges": privs,
			"id": su.ID, "name": su.Name, "email": su.Email, "source": meta.SourceSAML, "hasPassword": false,
			"createdAt": "", "updatedAt": "", "directory": true,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func hasAction(actions []string, a string) bool {
	for _, x := range actions {
		if x == a {
			return true
		}
	}
	return false
}

type check struct {
	Name        string `json:"name"`
	Healthy     bool   `json:"healthy"`
	Message     string `json:"message"`
	DurationMs  int64  `json:"durationMs"`
	LastChecked string `json:"lastChecked"`
}

// runCheck times fn and wraps its outcome.
func runCheck(name string, fn func() (bool, string)) check {
	start := time.Now()
	ok, msg := fn()
	return check{Name: name, Healthy: ok, Message: msg,
		DurationMs: time.Since(start).Milliseconds(), LastChecked: rfc3339(time.Now())}
}

// readMem returns memory in use and its limit; limit 0 means unknown.
func (u *UI) readMem() (used, limit int64) {
	if u.memReader != nil {
		return u.memReader()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	used = int64(ms.HeapAlloc)
	if rss := sysinfo.RSSBytes(); rss > used {
		used = rss
	}
	limit = debug.SetMemoryLimit(-1) // negative input only reads the limit
	if limit == math.MaxInt64 && u.Config != nil {
		limit = int64(u.Config.MemoryLimitMiB) << 20
	}
	if limit == math.MaxInt64 {
		limit = 0
	}
	return used, limit
}

func (u *UI) checkMemory() (bool, string) {
	used, limit := u.readMem()
	if limit <= 0 {
		return true, fmt.Sprintf("%d MiB in use, no limit configured", used>>20)
	}
	pct := float64(used) * 100 / float64(limit)
	msg := fmt.Sprintf("%d of %d MiB (%.0f%%)", used>>20, limit>>20, pct)
	switch {
	case pct >= 90:
		return false, msg + ": memory nearly exhausted"
	case pct >= 80:
		return true, msg + ": warning, memory use is high"
	}
	return true, msg
}

// blobCheckTimeout bounds the blob store health check; a var for tests.
var blobCheckTimeout = 2 * time.Second

// checkBlobStoreTimed runs the blob store check in a goroutine so a hung disk
// reports unhealthy instead of hanging the status page.
func (u *UI) checkBlobStoreTimed() (bool, string) {
	fn := u.checkBlobStore
	if u.blobChecker != nil {
		fn = u.blobChecker
	}
	// A check that hangs keeps its goroutine; do not start another on top of it.
	if !u.blobBusy.CompareAndSwap(false, true) {
		return false, "previous check still running"
	}
	type result struct {
		ok  bool
		msg string
	}
	ch := make(chan result, 1) // buffered: a late result must not leak the goroutine
	go func() {
		defer u.blobBusy.Store(false)
		ok, msg := fn()
		ch <- result{ok, msg}
	}()
	select {
	case r := <-ch:
		return r.ok, r.msg
	case <-time.After(blobCheckTimeout):
		return false, fmt.Sprintf("blob store check timed out after %s", blobCheckTimeout)
	}
}

func (u *UI) checkBlobStore() (bool, string) {
	if u.DataDir == "" {
		return false, "data directory unknown"
	}
	dir := filepath.Join(u.DataDir, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, "not writable: " + err.Error()
	}
	f, err := os.CreateTemp(dir, "health-*")
	if err != nil {
		return false, "not writable: " + err.Error()
	}
	name := f.Name()
	f.Close()
	if err := os.Remove(name); err != nil {
		return false, "not writable: " + err.Error()
	}
	return true, "writable"
}

func (u *UI) checkRepositories() (bool, string) {
	s := u.Repos.Snapshot()
	for _, rp := range s.Sorted {
		for _, m := range rp.Members {
			if s.ByName[m] == nil {
				return false, fmt.Sprintf("%s: member %q does not exist", rp.Name, m)
			}
		}
	}
	return true, fmt.Sprintf("%d repositories, all group members exist", len(s.Sorted))
}

func (u *UI) statusChecks(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": u.healthChecks(r.Context())})
}

// healthChecks runs the status checks.
func (u *UI) healthChecks(rctx context.Context) []check {
	checks := []check{
		runCheck("Database", func() (bool, string) {
			ctx, cancel := context.WithTimeout(rctx, 2*time.Second)
			defer cancel()
			if err := u.DB.Ping(ctx); err != nil {
				return false, "database not responding: " + err.Error()
			}
			return true, "responding"
		}),
		runCheck("Cluster", u.checkCluster(rctx)),
		runCheck("Blob store", u.checkBlobStoreTimed),
		runCheck("Memory", u.checkMemory),
	}
	if u.FetchSlots != nil {
		checks = append(checks, runCheck("Remote fetch slots", func() (bool, string) {
			inUse, max := u.FetchSlots()
			msg := fmt.Sprintf("%d of %d in use", inUse, max)
			if max > 0 && inUse >= max {
				return false, msg + ": all slots busy"
			}
			return true, msg
		}))
	}
	checks = append(checks, runCheck("Repositories", u.checkRepositories))
	if u.Auth.Snapshot().RealmEnabled(meta.RealmCrowd) {
		checks = append(checks, runCheck("Crowd reachable", u.crowdCheck(rctx)))
	}
	checks = append(checks, u.quotaChecks(rctx)...)
	return checks
}

func (u *UI) systemInformation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	blobs, blobBytes, err := u.blobStats(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	status := map[string]any{"version": u.Version, "node": u.Node}
	if !u.Started.IsZero() {
		status["uptimeSeconds"] = int64(time.Since(u.Started).Seconds())
		status["started"] = rfc3339(u.Started)
	}

	cfg := map[string]any{}
	if c := u.Config; c != nil {
		cfg["dataDir"] = c.DataDir
		cfg["listen"] = c.Listen
		cfg["memoryLimitMiB"] = c.MemoryLimitMiB
	}
	// Users and settings live in the database since plan 12 (the auth
	// snapshot); the config file keeps only process settings.
	snap := u.Auth.Snapshot()
	settings := snap.Settings()
	cfg["baseURL"] = settings.BaseURL
	cfg["maxUploadMiB"] = settings.MaxUploadMiB
	cfg["anonymousRead"] = settings.AnonymousRead
	cfg["authenticatedRead"] = settings.AuthenticatedRead
	cfg["userCount"] = len(snap.Users())
	// Repository definitions live in the registry, not in the config file.
	repos := u.Repos.Snapshot().Sorted
	cfg["repositoryCount"] = len(repos)
	counts := map[string]int{}
	for _, rp := range repos {
		counts[rp.Format+"/"+rp.Type]++
	}
	cfg["repositories"] = counts

	memLimit := "unset"
	if l := debug.SetMemoryLimit(-1); l != math.MaxInt64 {
		memLimit = fmt.Sprintf("%d", l)
	}
	runtimeInfo := map[string]any{
		"goVersion":  runtime.Version(),
		"goos":       runtime.GOOS,
		"goarch":     runtime.GOARCH,
		"numCPU":     runtime.NumCPU(),
		"gomaxprocs": runtime.GOMAXPROCS(0),
		"goroutines": runtime.NumGoroutine(),
		"gomemlimit": memLimit,
		"heapAlloc":  ms.HeapAlloc,
		"heapSys":    ms.HeapSys,
		"heapInuse":  ms.HeapInuse,
		"sys":        ms.Sys,
		"numGC":      ms.NumGC,
		"rssBytes":   sysinfo.RSSBytes(),
	}

	// Allow-list only: never the full environment (proxy URLs may carry
	// credentials, so HTTPS_PROXY is reported as present or not).
	proxySet := "no"
	if os.Getenv("HTTPS_PROXY") != "" {
		proxySet = "yes"
	}
	env := map[string]any{
		"GOMEMLIMIT":  os.Getenv("GOMEMLIMIT"),
		"HTTPS_PROXY": proxySet,
		"TZ":          os.Getenv("TZ"),
	}

	stores := map[string]any{"blobCount": blobs, "blobBytes": blobBytes}
	if n, err := u.DB.SizeBytes(r.Context()); err == nil { // the PostgreSQL database
		stores["metaDbBytes"] = n
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"commandcenter-status":        status,
		"commandcenter-configuration": cfg,
		"system-runtime":              runtimeInfo,
		"system-environment":          env,
		"system-filestores":           stores,
	})
}
