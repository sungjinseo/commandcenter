package ui

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/sysinfo"
)

// Operations endpoints (Nexus's support features): the Prometheus
// exposition, the thread dump, the support bundle, the runtime log level
// and the SSL trust store.

func (u *UI) opsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/support/threads", u.threadDump)
	mux.HandleFunc("GET /-/api/support/zip", u.supportZip)
	mux.HandleFunc("GET /-/api/logging", u.getLogging)
	mux.HandleFunc("PUT /-/api/logging", u.putLogging)
	mux.HandleFunc("GET /-/api/ssl/certificates", u.listCertificates)
	mux.HandleFunc("POST /-/api/ssl/certificates", u.addCertificate)
	mux.HandleFunc("DELETE /-/api/ssl/certificates/{id}", u.removeCertificate)
	mux.HandleFunc("POST /-/api/ssl/retrieve", u.retrieveCertificate)
}

// ---- Prometheus ----

// GET /-/metrics/prometheus: the text exposition format. Admins always;
// anyone when the metricsAnonymous setting is on.
func (u *UI) prometheus(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin && !u.Auth.Snapshot().Settings().MetricsAnonymous {
		if p.Anonymous() {
			w.Header().Set("WWW-Authenticate", `Basic realm="commandcenter"`)
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		writeError(w, http.StatusForbidden, "admin only")
		return
	}
	b, err := u.prometheusText(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.Write(b)
}

// promWriter accumulates one metric family at a time.
type promWriter struct{ b bytes.Buffer }

func (p *promWriter) family(name, typ, help string) {
	fmt.Fprintf(&p.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// sample writes one line; labels are name/value pairs.
func (p *promWriter) sample(name string, value any, labels ...string) {
	p.b.WriteString(name)
	if len(labels) > 0 {
		p.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				p.b.WriteByte(',')
			}
			p.b.WriteString(labels[i])
			p.b.WriteString(`="`)
			p.b.WriteString(promEscape(labels[i+1]))
			p.b.WriteByte('"')
		}
		p.b.WriteByte('}')
	}
	fmt.Fprintf(&p.b, " %v\n", value)
}

var promEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func promEscape(s string) string { return promEscaper.Replace(s) }

// prometheusText renders every metric. The per-repository counters are
// process-lifetime totals from the metrics recorder; the asset and blob
// store gauges come from the cached statistics (statsTTL).
func (u *UI) prometheusText(ctx context.Context) ([]byte, error) {
	var p promWriter
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p.family("commandcenter_build_info", "gauge", "Command Center build information.")
	p.sample("commandcenter_build_info", 1, "version", u.Version, "goversion", runtime.Version())
	if u.Node != "" {
		p.family("commandcenter_node_info", "gauge", "The node id of this process in the nodes table (GET /-/api/nodes).")
		p.sample("commandcenter_node_info", 1, "node", u.Node, "version", u.Version)
	}
	p.family("commandcenter_uptime_seconds", "gauge", "Seconds since the process started.")
	p.sample("commandcenter_uptime_seconds", int64(time.Since(u.Started).Seconds()))
	p.family("go_goroutines", "gauge", "Number of goroutines.")
	p.sample("go_goroutines", runtime.NumGoroutine())
	p.family("go_memstats_heap_alloc_bytes", "gauge", "Heap bytes allocated and in use.")
	p.sample("go_memstats_heap_alloc_bytes", ms.HeapAlloc)
	p.family("go_memstats_sys_bytes", "gauge", "Bytes obtained from the system.")
	p.sample("go_memstats_sys_bytes", ms.Sys)
	p.family("process_resident_memory_bytes", "gauge", "Resident memory size in bytes (Linux only; 0 elsewhere).")
	p.sample("process_resident_memory_bytes", sysinfo.RSSBytes())
	p.family("go_gc_cycles_total", "counter", "Completed GC cycles.")
	p.sample("go_gc_cycles_total", ms.NumGC)

	repos := u.Repos.Snapshot().Sorted
	totals := map[string]metricsTotals{}
	if u.Metrics != nil {
		for name, t := range u.Metrics.Totals() {
			totals[name] = metricsTotals(t)
		}
	}
	counter := func(name, help string, get func(metricsTotals) int64) {
		p.family(name, "counter", help)
		for _, rp := range repos {
			p.sample(name, get(totals[rp.Name]), "repository", rp.Name)
		}
	}
	counter("commandcenter_requests_total", "Requests to the repository since the process started.", func(t metricsTotals) int64 { return t.Requests })
	counter("commandcenter_request_errors_total", "Requests answered 5xx or 429.", func(t metricsTotals) int64 { return t.Errors })
	p.family("commandcenter_request_bytes_total", "counter", "Request (in) and response (out) bytes.")
	for _, rp := range repos {
		t := totals[rp.Name]
		p.sample("commandcenter_request_bytes_total", t.BytesIn, "repository", rp.Name, "direction", "in")
		p.sample("commandcenter_request_bytes_total", t.Bytes, "repository", rp.Name, "direction", "out")
	}
	p.family("commandcenter_request_duration_ms", "summary", "Response time of requests in milliseconds.")
	for _, rp := range repos {
		t := totals[rp.Name]
		p.sample("commandcenter_request_duration_ms_sum", t.Ms, "repository", rp.Name)
		p.sample("commandcenter_request_duration_ms_count", t.Requests, "repository", rp.Name)
	}
	counter("commandcenter_upstream_requests_total", "Fetches from a proxy repository's remote.", func(t metricsTotals) int64 { return t.Upstream })
	counter("commandcenter_upstream_failures_total", "Remote fetches that failed or answered 5xx or 429.", func(t metricsTotals) int64 { return t.UpstreamFailures })
	counter("commandcenter_cache_hits_total", "Proxy cache lookups answered from the cache.", func(t metricsTotals) int64 { return t.CacheHits })
	counter("commandcenter_cache_misses_total", "Proxy cache lookups that went to the remote.", func(t metricsTotals) int64 { return t.CacheMisses })

	stats, err := u.repoStats(ctx)
	if err != nil {
		return nil, err
	}
	p.family("commandcenter_repository_assets", "gauge", "Assets stored in the repository.")
	for _, rp := range repos {
		p.sample("commandcenter_repository_assets", stats[rp.Name].Assets, "repository", rp.Name)
	}
	p.family("commandcenter_repository_bytes", "gauge", "Bytes of the repository's assets.")
	for _, rp := range repos {
		p.sample("commandcenter_repository_bytes", stats[rp.Name].Bytes, "repository", rp.Name)
	}
	stores, err := u.storeTotals(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(stores))
	for n := range stores {
		names = append(names, n)
	}
	sort.Strings(names)
	p.family("commandcenter_blobstore_blobs", "gauge", "Distinct blobs referenced by the store's repositories.")
	for _, n := range names {
		p.sample("commandcenter_blobstore_blobs", stores[n].blobs, "store", n)
	}
	p.family("commandcenter_blobstore_bytes", "gauge", "Bytes of those blobs.")
	for _, n := range names {
		p.sample("commandcenter_blobstore_bytes", stores[n].bytes, "store", n)
	}
	p.family("commandcenter_tasks_last_result", "gauge", "1 when the task's last run succeeded, 0 when it failed; absent until it ran.")
	if u.Tasks != nil {
		list, err := u.Tasks.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			switch t.LastResult {
			case "OK":
				p.sample("commandcenter_tasks_last_result", 1, "task", t.Name, "type", t.Type)
			case "Failed":
				p.sample("commandcenter_tasks_last_result", 0, "task", t.Name, "type", t.Type)
			}
		}
	}
	p.family("commandcenter_health_check", "gauge", "1 when the status check passes, 0 when it fails.")
	for _, c := range u.healthChecks(ctx) {
		v := 0
		if c.Healthy {
			v = 1
		}
		p.sample("commandcenter_health_check", v, "name", c.Name)
	}
	return p.b.Bytes(), nil
}

// metricsTotals mirrors metrics.Totals so the zero value stands in for a
// repository without traffic.
type metricsTotals struct {
	Requests, Bytes, Errors, BytesIn, Ms   int64
	Upstream, UpstreamFailures, UpstreamMs int64
	CacheHits, CacheMisses                 int64
}

// storeTotals returns every blob store's blob count and bytes, cached and
// slot-bounded like repoStats.
func (u *UI) storeTotals(ctx context.Context) (map[string]blobTotals, error) {
	return u.storeCache.get(ctx, u.clock, func(ctx context.Context) (map[string]blobTotals, error) {
		release, err := u.acquireHeavy(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		rows, err := u.DB.ListBlobStores(ctx)
		if err != nil {
			return nil, err
		}
		snap := u.Repos.Snapshot()
		out := make(map[string]blobTotals, len(rows))
		for _, bs := range rows {
			n, b, err := u.DB.BlobStatsInRepos(ctx, repo.ReposForStore(snap, bs.Name))
			if err != nil {
				return nil, err
			}
			out[bs.Name] = blobTotals{n, b}
		}
		return out, nil
	})
}

// ---- thread dump ----

// maxThreadDump bounds the stack text.
const maxThreadDump = 8 << 20

func threadDump() []byte {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) || len(buf) >= maxThreadDump {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// GET /-/api/support/threads (admin): every goroutine's stack as text.
func (u *UI) threadDump(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(threadDump())
}

// ---- support bundle ----

// callAPI serves a GET of path through the UI's own API as the caller of r
// (its principal is in the context) and returns the status and body.
func (u *UI) callAPI(r *http.Request, path string) (int, []byte) {
	u.internalOnce.Do(func() { u.internal = u.apiHandler() })
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req.Header.Set("X-Request-Id", r.Header.Get("X-Request-Id"))
	rec := &bodyRecorder{h: http.Header{}}
	u.internal.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.status, rec.body.Bytes()
}

type bodyRecorder struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (b *bodyRecorder) Header() http.Header         { return b.h }
func (b *bodyRecorder) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bodyRecorder) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

// apiJSON is the body of a successful call, or an object naming the status.
func (u *UI) apiJSON(r *http.Request, path string) json.RawMessage {
	status, body := u.callAPI(r, path)
	if status != http.StatusOK || !json.Valid(body) {
		b, _ := json.Marshal(map[string]any{"error": "status " + strconv.Itoa(status)})
		return b
	}
	return body
}

// GET /-/api/support/zip (interactive admin): a zip of the information the
// support pages show, for a bug report. Every entry is an API view, so no
// secret is in it: passwords and keys appear only as "set" flags. The
// bundle is bounded: 500 events, the log ring (1 MiB), the thread dump
// (8 MiB) and the API listings.
func (u *UI) supportZip(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) {
		return
	}
	host, _ := os.Hostname()
	host = strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' {
			return c
		}
		return '-'
	}, host)
	if host == "" {
		host = "unknown"
	}
	now := u.clock().UTC()
	name := fmt.Sprintf("commandcenter-support-%s-%s.zip", host, now.Format("20060102T150405Z"))
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	zw := zip.NewWriter(w)
	add := func(entry string, b []byte) {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: entry, Method: zip.Deflate, Modified: now})
		if err == nil {
			f.Write(b)
		}
	}
	addJSON := func(entry string, v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		add(entry, b)
	}
	info := map[string]any{
		"version": u.Version, "goVersion": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"pid": os.Getpid(), "hostname": host, "node": u.Node, "numCPU": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0),
		"generatedAt": now.Format(time.RFC3339),
	}
	if !u.Started.IsZero() {
		info["started"] = rfc3339(u.Started)
		info["uptimeSeconds"] = int64(now.Sub(u.Started).Seconds())
	}
	addJSON("info.json", info)
	process := map[string]any{"database": u.DB.Driver()}
	if c := u.Config; c != nil {
		process["dataDir"], process["listen"], process["memoryLimitMiB"] = c.DataDir, c.Listen, c.MemoryLimitMiB
	}
	addJSON("config.json", map[string]any{
		"process":  process,
		"settings": u.apiJSON(r, "/-/api/settings"),
		"smtp":     u.apiJSON(r, "/-/api/settings/smtp"),
		"realms":   u.apiJSON(r, "/-/api/realms"),
		"ldap":     u.apiJSON(r, "/-/api/realms/ldap"),
		"oidc":     u.apiJSON(r, "/-/api/realms/oidc"),
		"saml":     u.apiJSON(r, "/-/api/realms/saml"),
		"ssl":      u.apiJSON(r, "/-/api/ssl/certificates"),
	})
	addJSON("repositories.json", u.apiJSON(r, "/-/api/repositories"))
	addJSON("blobstores.json", u.apiJSON(r, "/-/api/blobstores"))
	addJSON("status.json", map[string]any{"checks": u.healthChecks(r.Context())})
	addJSON("sysinfo.json", u.apiJSON(r, "/-/api/systeminformation"))
	addJSON("nodes.json", u.apiJSON(r, "/-/api/nodes"))
	addJSON("tasks.json", u.apiJSON(r, "/-/api/tasks"))
	addJSON("webhooks.json", u.apiJSON(r, "/-/api/webhooks"))
	addJSON("request-stats.json", u.apiJSON(r, "/-/api/stats/repositories?range=24h"))
	if evs, err := u.DB.ListEvents(r.Context(), 500, 0, ""); err == nil {
		rows := make([]map[string]any, 0, len(evs))
		for _, e := range evs {
			rows = append(rows, map[string]any{"id": e.ID, "at": rfc3339(e.At), "kind": e.Kind, "actor": e.Actor,
				"repo": e.Repo, "subject": e.Subject, "detail": e.Detail})
		}
		addJSON("events.json", map[string]any{"events": rows})
	}
	var logs bytes.Buffer
	if u.Logs != nil {
		recs := u.Logs.Snapshot(slog.LevelDebug, "", 0)
		for i := len(recs) - 1; i >= 0; i-- {
			logs.WriteString(recs[i].String())
			logs.WriteByte('\n')
		}
	}
	add("logs.txt", logs.Bytes())
	add("threads.txt", threadDump())
	if b, err := u.prometheusText(r.Context()); err == nil {
		add("metrics.txt", b)
	} else {
		add("metrics.txt", []byte("error: "+logsafe.Err(err)+"\n"))
	}
	if err := zw.Close(); err != nil {
		return
	}
	u.emit(r, "support.zip", "", name, "")
}

// ---- log level ----

func levelName(l slog.Level) string { return strings.ToLower(l.String()) }

func parseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

// GET /-/api/logging (admin): {"level": "info"}, the root logger's level.
func (u *UI) getLogging(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	if u.LogLevel == nil {
		writeError(w, http.StatusNotImplemented, "the log level cannot be changed in this process")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"level": levelName(u.LogLevel.Level()), "persistent": false})
}

// PUT /-/api/logging {"level": "debug|info|warn|error"} (interactive admin):
// changes the level at once, on every node, until the next start.
func (u *UI) putLogging(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		Level string `json:"level"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	lvl, ok := parseLevel(body.Level)
	if !ok {
		writeError(w, http.StatusBadRequest, "level must be debug, info, warn or error")
		return
	}
	if u.LogLevel == nil {
		writeError(w, http.StatusNotImplemented, "the log level cannot be changed in this process")
		return
	}
	was := u.LogLevel.Level()
	u.LogLevel.Set(lvl)
	if err := u.DB.Notify(r.Context(), "loglevel:"+levelName(lvl)); err != nil && u.Log != nil { // the other nodes too
		u.Log.Warn("could not tell the other nodes the new log level", "err", logsafe.Err(err))
	}
	if was != lvl {
		u.emit(r, "logging.level", "", levelName(lvl), levelName(was)+" -> "+levelName(lvl))
	}
	writeJSON(w, http.StatusOK, map[string]any{"level": levelName(lvl), "persistent": false})
}

// ---- SSL trust store ----

// certJSON is one certificate as the API shows it.
type certJSON struct {
	ID                        string `json:"id"` // the SHA-256 fingerprint, lowercase hex
	Subject                   string `json:"subject"`
	Issuer                    string `json:"issuer"`
	SubjectCommonName         string `json:"subjectCommonName"`
	SubjectOrganization       string `json:"subjectOrganization"`
	SubjectOrganizationalUnit string `json:"subjectOrganizationalUnit"`
	IssuerCommonName          string `json:"issuerCommonName"`
	IssuerOrganization        string `json:"issuerOrganization"`
	IssuerOrganizationalUnit  string `json:"issuerOrganizationalUnit"`
	SerialNumber              string `json:"serialNumber"`
	NotBefore                 string `json:"notBefore"`
	NotAfter                  string `json:"notAfter"`
	Expired                   bool   `json:"expired"`
	IsCA                      bool   `json:"isCA"`
	Trusted                   bool   `json:"trusted"` // in the trust store
	PEM                       string `json:"pem"`
}

func certPEM(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

func (u *UI) certView(c *x509.Certificate, trusted map[string]bool) certJSON {
	first := func(s []string) string {
		if len(s) > 0 {
			return s[0]
		}
		return ""
	}
	id := meta.Fingerprint(c)
	return certJSON{
		ID: id, Subject: c.Subject.String(), Issuer: c.Issuer.String(),
		SubjectCommonName: c.Subject.CommonName, SubjectOrganization: first(c.Subject.Organization), SubjectOrganizationalUnit: first(c.Subject.OrganizationalUnit),
		IssuerCommonName: c.Issuer.CommonName, IssuerOrganization: first(c.Issuer.Organization), IssuerOrganizationalUnit: first(c.Issuer.OrganizationalUnit),
		SerialNumber: c.SerialNumber.Text(16), NotBefore: rfc3339(c.NotBefore), NotAfter: rfc3339(c.NotAfter),
		Expired: u.clock().After(c.NotAfter), IsCA: c.IsCA, Trusted: trusted[id], PEM: certPEM(c),
	}
}

// trusted returns the stored trust store's fingerprints.
func trustedIDs(pems []string) map[string]bool {
	ids := map[string]bool{}
	for _, p := range pems {
		if certs, err := meta.ParseCertificates(p); err == nil {
			for _, c := range certs {
				ids[meta.Fingerprint(c)] = true
			}
		}
	}
	return ids
}

func (u *UI) trustStoreViews(pems []string) []certJSON {
	ids := trustedIDs(pems)
	out := []certJSON{}
	for _, p := range pems {
		if certs, err := meta.ParseCertificates(p); err == nil {
			for _, c := range certs {
				out = append(out, u.certView(c, ids))
			}
		}
	}
	return out
}

// GET /-/api/ssl/certificates (admin): the trust store.
func (u *UI) listCertificates(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"certificates": u.trustStoreViews(u.Auth.Snapshot().Settings().TrustedCerts)})
}

// loadTrustStore reads the stored list (not the snapshot: a concurrent
// change must not be written back stale). Callers hold the access lock.
func (u *UI) loadTrustStore(ctx context.Context) ([]string, error) {
	pems := []string{}
	_, err := u.DB.GetSettingJSON(ctx, meta.SettingTrustedCertificates, &pems)
	return pems, err
}

func (u *UI) saveTrustStore(w http.ResponseWriter, r *http.Request, pems []string) bool {
	if err := meta.ValidateTrustedCertificates(pems); err != nil {
		var v validation
		v.add("%s", err.Error())
		u.writeAccessError(w, r, v.err())
		return false
	}
	if err := u.DB.SetSettingJSON(r.Context(), meta.SettingTrustedCertificates, pems); err != nil {
		u.fail(w, r, err)
		return false
	}
	if err := u.reloadAccess(r.Context()); err != nil {
		u.fail(w, r, err)
		return false
	}
	return true
}

// POST /-/api/ssl/certificates {"pem": "..."} (interactive admin): adds every
// certificate of the PEM text that is not in the store yet; 201 with the
// ones added, 409 when all were there already.
func (u *UI) addCertificate(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		PEM string `json:"pem"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	certs, err := meta.ParseCertificates(body.PEM)
	if err != nil {
		writeError(w, http.StatusBadRequest, "pem: "+err.Error())
		return
	}
	unlock, ok := u.lockAccess(w, r)
	if !ok {
		return
	}
	defer unlock()
	pems, err := u.loadTrustStore(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	ids := trustedIDs(pems)
	var added []certJSON
	for _, c := range certs {
		id := meta.Fingerprint(c)
		if ids[id] {
			continue
		}
		ids[id] = true
		pems = append(pems, certPEM(c))
		added = append(added, u.certView(c, ids))
	}
	if len(added) == 0 {
		writeError(w, http.StatusConflict, "the certificate is already in the trust store")
		return
	}
	if !u.saveTrustStore(w, r, pems) {
		return
	}
	for _, c := range added {
		u.emit(r, "ssl.add", "", c.ID, c.Subject)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"certificates": added})
}

// normalizeFingerprint accepts the hex id with or without colons, any case.
func normalizeFingerprint(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
}

// DELETE /-/api/ssl/certificates/{id} (interactive admin): 204, or 404.
func (u *UI) removeCertificate(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	id := normalizeFingerprint(r.PathValue("id"))
	unlock, ok := u.lockAccess(w, r)
	if !ok {
		return
	}
	defer unlock()
	pems, err := u.loadTrustStore(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	var kept []string
	var removed *x509.Certificate
	for _, p := range pems {
		certs, err := meta.ParseCertificates(p)
		if err == nil && len(certs) == 1 && meta.Fingerprint(certs[0]) == id && removed == nil {
			removed = certs[0]
			continue
		}
		kept = append(kept, p)
	}
	if removed == nil {
		writeError(w, http.StatusNotFound, "certificate not found")
		return
	}
	if kept == nil {
		kept = []string{}
	}
	if !u.saveTrustStore(w, r, kept) {
		return
	}
	u.emit(r, "ssl.remove", "", id, removed.Subject.String())
	w.WriteHeader(http.StatusNoContent)
}

// POST /-/api/ssl/retrieve {"host": "...", "port": 443} (interactive admin):
// connects to the host and returns the certificate chain it presents, leaf
// first, each marked whether it is in the trust store already. The
// connection goes through the egress policy and the proxy settings and
// does not verify the server.
func (u *UI) retrieveCertificate(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	host := strings.TrimSpace(body.Host)
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " /@:\t\r\n") {
		writeError(w, http.StatusBadRequest, "host must be a host name or address")
		return
	}
	if body.Port == 0 {
		body.Port = 443
	}
	if body.Port < 1 || body.Port > 65535 {
		writeError(w, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}
	chain, err := proxy.RetrieveCertificates(r.Context(), host, body.Port)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not retrieve the certificate: "+logsafe.Err(err))
		return
	}
	ids := trustedIDs(u.Auth.Snapshot().Settings().TrustedCerts)
	out := make([]certJSON, 0, len(chain))
	for _, c := range chain {
		out = append(out, u.certView(c, ids))
	}
	writeJSON(w, http.StatusOK, map[string]any{"host": host, "port": body.Port, "certificates": out})
}
