package ui

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/logring"
	"commandcenter/internal/meta"
	"commandcenter/internal/metrics"
	"commandcenter/internal/repo/repotest"
)

// eventsOf returns the sink's events of kind, oldest first.
func eventsOf(sink *repotest.Sink, kind string) []repotest.Event {
	var out []repotest.Event
	for _, ev := range sink.Events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// promSample is one parsed exposition line.
type promSample struct {
	labels map[string]string
	value  float64
}

// parseProm is a tiny exposition parser: metric name -> samples, with the
// families' TYPE lines collected too.
func parseProm(t *testing.T, text string) (map[string][]promSample, map[string]string) {
	t.Helper()
	samples := map[string][]promSample{}
	types := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			f := strings.Fields(line)
			types[f[2]] = f[3]
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[sp+1:], 64)
		if err != nil {
			t.Fatalf("bad value in %q", line)
		}
		head := line[:sp]
		name, labels := head, map[string]string{}
		if i := strings.IndexByte(head, '{'); i >= 0 {
			name = head[:i]
			body := head[i+1 : len(head)-1]
			for body != "" {
				eq := strings.IndexByte(body, '=')
				key := body[:eq]
				// the value: a quoted string with \" \\ \n escapes
				var val strings.Builder
				j := eq + 2
				for ; j < len(body) && body[j] != '"'; j++ {
					if body[j] == '\\' {
						j++
						switch body[j] {
						case 'n':
							val.WriteByte('\n')
						default:
							val.WriteByte(body[j])
						}
						continue
					}
					val.WriteByte(body[j])
				}
				labels[key] = val.String()
				body = strings.TrimPrefix(body[j+1:], ",")
			}
		}
		samples[name] = append(samples[name], promSample{labels, v})
	}
	return samples, types
}

func find(samples []promSample, label, value string) (float64, bool) {
	for _, s := range samples {
		if s.labels[label] == value {
			return s.value, true
		}
	}
	return 0, false
}

func TestPrometheusMetrics(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	rec := metrics.New(e.db)
	e.ui.Metrics = rec
	e.ui.Logs = logring.NewRing(0, 0)

	wantStatus(t, e.get(t, "/-/metrics/prometheus", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/metrics/prometheus", "reader"), http.StatusForbidden)
	r := e.get(t, "/-/metrics/prometheus", "admin")
	wantStatus(t, r, http.StatusOK)
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type %q", ct)
	}
	samples, types := parseProm(t, r.body)
	for _, name := range []string{"commandcenter_build_info", "go_goroutines", "go_memstats_heap_alloc_bytes", "go_memstats_sys_bytes",
		"process_resident_memory_bytes", "go_gc_cycles_total", "commandcenter_requests_total", "commandcenter_request_errors_total",
		"commandcenter_request_bytes_total", "commandcenter_request_duration_ms_sum", "commandcenter_request_duration_ms_count",
		"commandcenter_upstream_requests_total", "commandcenter_upstream_failures_total", "commandcenter_cache_hits_total",
		"commandcenter_cache_misses_total", "commandcenter_repository_assets", "commandcenter_repository_bytes", "commandcenter_health_check"} {
		if len(samples[name]) == 0 {
			t.Errorf("metric %s missing", name)
		}
	}
	if types["commandcenter_requests_total"] != "counter" || types["commandcenter_repository_assets"] != "gauge" || types["commandcenter_request_duration_ms"] != "summary" {
		t.Fatalf("types %v", types)
	}
	if v, ok := find(samples["commandcenter_build_info"], "version", "test"); !ok || v != 1 {
		t.Fatalf("build_info: %v", samples["commandcenter_build_info"])
	}
	if v, ok := find(samples["commandcenter_repository_assets"], "repository", "maven-releases"); !ok || v != 5 {
		t.Fatalf("maven-releases assets = %v %v", v, ok)
	}
	if v, ok := find(samples["commandcenter_health_check"], "name", "Database"); !ok || v != 1 {
		t.Fatalf("health check Database = %v %v", v, ok)
	}
	if v, ok := find(samples["commandcenter_requests_total"], "repository", "maven-releases"); !ok || v != 0 {
		t.Fatalf("requests before traffic = %v %v", v, ok)
	}

	// Counters are process-lifetime totals: they grow across scrapes.
	rec.Request("maven-releases", 200, 10, 200, 5*time.Millisecond)
	rec.Request("maven-releases", 503, 0, 0, time.Millisecond)
	rec.Upstream("maven-proxy", false, time.Millisecond)
	rec.Cache("maven-proxy", true)
	samples, _ = parseProm(t, e.get(t, "/-/metrics/prometheus", "admin").body)
	checks := map[string][3]any{
		"commandcenter_requests_total":          {"repository", "maven-releases", 2.0},
		"commandcenter_request_errors_total":    {"repository", "maven-releases", 1.0},
		"commandcenter_request_duration_ms_sum": {"repository", "maven-releases", 6.0},
		"commandcenter_upstream_failures_total": {"repository", "maven-proxy", 1.0},
		"commandcenter_cache_hits_total":        {"repository", "maven-proxy", 1.0},
	}
	for name, c := range checks {
		if v, ok := find(samples[name], c[0].(string), c[1].(string)); !ok || v != c[2].(float64) {
			t.Errorf("%s = %v %v, want %v", name, v, ok, c[2])
		}
	}
	var in, out float64
	for _, s := range samples["commandcenter_request_bytes_total"] {
		if s.labels["repository"] == "maven-releases" {
			if s.labels["direction"] == "in" {
				in = s.value
			} else {
				out = s.value
			}
		}
	}
	if in != 10 || out != 200 {
		t.Fatalf("bytes in=%v out=%v", in, out)
	}
	rec.Request("maven-releases", 200, 0, 0, 0)
	samples, _ = parseProm(t, e.get(t, "/-/metrics/prometheus", "admin").body)
	if v, _ := find(samples["commandcenter_requests_total"], "repository", "maven-releases"); v != 3 {
		t.Fatalf("not monotonic: %v", v)
	}

	// metricsAnonymous opens the endpoint to everyone.
	if err := e.db.SetSettingJSON(context.Background(), meta.SettingMetricsAnonymous, true); err != nil {
		t.Fatal(err)
	}
	if err := e.ui.Auth.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.get(t, "/-/metrics/prometheus", ""), http.StatusOK)
	wantStatus(t, e.get(t, "/-/metrics/prometheus", "reader"), http.StatusOK)
}

func TestPrometheusLabelEscaping(t *testing.T) {
	var p promWriter
	p.sample("m", 1, "l", "a\"b\\c\nd")
	samples, _ := parseProm(t, p.b.String())
	if got := samples["m"][0].labels["l"]; got != "a\"b\\c\nd" {
		t.Fatalf("round trip %q", got)
	}
	if !strings.Contains(p.b.String(), `l="a\"b\\c\nd"`) {
		t.Fatalf("escaped %q", p.b.String())
	}
}

func TestThreadDump(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.get(t, "/-/api/support/threads", "ci"), http.StatusForbidden)
	r := e.get(t, "/-/api/support/threads", "admin")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, "goroutine ") || !strings.Contains(r.body, "ui.threadDump") || len(r.body) > maxThreadDump {
		t.Fatalf("dump %d bytes: %.200s", len(r.body), r.body)
	}
}

func TestSupportZip(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	e.ui.Metrics = metrics.New(e.db)
	e.ui.Logs = logring.NewRing(0, 0)
	e.ui.Logs.Add(logring.Record{Time: time.Now(), Level: slog.LevelInfo, Msg: "hello from the ring"})
	ctx := context.Background()
	// Stored secrets that must not appear in the bundle.
	if err := e.db.SetSecretKey(bytes.Repeat([]byte{9}, 32)); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetSettingJSON(ctx, meta.SettingSMTP, meta.SMTP{Host: "h", Port: 25, Security: "none", From: "a@b.com", Username: "u", Password: "smtp-pw-7Q", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	h := meta.DefaultHTTP()
	h.Proxy = meta.HTTPProxy{Host: "proxy.corp", Port: 3128, Username: "pu", Password: "proxy-pw-9Z"}
	if err := e.db.SetSettingJSON(ctx, meta.SettingHTTP, h); err != nil {
		t.Fatal(err)
	}
	if err := e.ui.Auth.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.get(t, "/-/api/support/zip", "ci"), http.StatusForbidden)
	r := e.get(t, "/-/api/support/zip", "admin")
	wantStatus(t, r, http.StatusOK)
	if r.header.Get("Content-Type") != "application/zip" || !strings.Contains(r.header.Get("Content-Disposition"), "commandcenter-support-") {
		t.Fatalf("headers %v", r.header)
	}
	zr, err := zip.NewReader(strings.NewReader(r.body), int64(len(r.body)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	for _, name := range []string{"info.json", "config.json", "repositories.json", "blobstores.json", "status.json", "sysinfo.json", "nodes.json",
		"tasks.json", "events.json", "logs.txt", "threads.txt", "metrics.txt", "request-stats.json"} {
		if _, ok := got[name]; !ok {
			t.Errorf("entry %s missing (have %v)", name, len(got))
		}
	}
	for name, body := range got {
		for _, secret := range []string{"enc:", "smtp-pw-7Q", "proxy-pw-9Z", "u:secret", "secret@"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s contains %q", name, secret)
			}
		}
	}
	var cfg struct {
		Settings struct {
			HTTP struct {
				Proxy struct {
					Host        string
					PasswordSet bool
				}
			}
		}
		SMTP struct{ PasswordSet bool }
	}
	if err := json.Unmarshal([]byte(got["config.json"]), &cfg); err != nil || cfg.Settings.HTTP.Proxy.Host != "proxy.corp" ||
		!cfg.Settings.HTTP.Proxy.PasswordSet || !cfg.SMTP.PasswordSet {
		t.Fatalf("config.json: %v %s", err, got["config.json"])
	}
	if !strings.Contains(got["logs.txt"], "hello from the ring") || !strings.Contains(got["threads.txt"], "goroutine ") ||
		!strings.Contains(got["metrics.txt"], "commandcenter_build_info") || !strings.Contains(got["info.json"], `"version": "test"`) {
		t.Fatalf("contents: %q %.100q %.100q", got["logs.txt"], got["threads.txt"], got["metrics.txt"])
	}
	if evs := eventsOf(sink, "support.zip"); len(evs) != 1 || evs[0].Actor != "admin" || !strings.HasPrefix(evs[0].Subject, "commandcenter-support-") {
		t.Fatalf("events %v", evs)
	}
}

func TestLoggingLevel(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	lv := new(slog.LevelVar)
	var out bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: lv}))
	e.ui.LogLevel = lv

	r := e.get(t, "/-/api/logging", "admin")
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["level"] != "info" {
		t.Fatalf("initial %s", r.body)
	}
	log.Debug("hidden")
	if out.Len() != 0 {
		t.Fatal("debug logged at info")
	}
	wantStatus(t, e.req(t, "PUT", "/-/api/logging", `{"level":"debug"}`, "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/logging", `{"level":"debug"}`, "admin", nil, false), http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/logging", `{"level":"loud"}`, "admin", nil, true), http.StatusBadRequest)
	r = e.req(t, "PUT", "/-/api/logging", `{"level":"debug"}`, "admin", nil, true)
	wantStatus(t, r, http.StatusOK)
	log.Debug("visible")
	if !strings.Contains(out.String(), "visible") {
		t.Fatalf("debug not logged after the change: %q", out.String())
	}
	if e.get(t, "/-/api/logging", "admin").json(t)["level"] != "debug" {
		t.Fatal("GET does not show debug")
	}
	wantStatus(t, e.req(t, "PUT", "/-/api/logging", `{"level":"WARN"}`, "admin", nil, true), http.StatusOK)
	out.Reset()
	log.Info("quiet")
	if out.Len() != 0 {
		t.Fatal("info logged at warn")
	}
	if evs := eventsOf(sink, "logging.level"); len(evs) != 2 || evs[0].Detail != "info -> debug" || evs[1].Subject != "warn" {
		t.Fatalf("events %+v", evs)
	}
	e.ui.LogLevel = nil
	wantStatus(t, e.get(t, "/-/api/logging", "admin"), http.StatusNotImplemented)
}

func selfSigned(t *testing.T, cn string) (*x509.Certificate, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: cn, Organization: []string{"Test Org"}},
		Issuer: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestSSLTrustStore(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	ca, pemText := selfSigned(t, "Corp Root CA")
	id := meta.Fingerprint(ca)
	body, _ := json.Marshal(map[string]string{"pem": pemText})

	wantStatus(t, e.get(t, "/-/api/ssl/certificates", "ci"), http.StatusForbidden)
	r := e.get(t, "/-/api/ssl/certificates", "admin")
	wantStatus(t, r, http.StatusOK)
	if r.body != "{\"certificates\":[]}\n" {
		t.Fatalf("empty store: %s", r.body)
	}
	wantStatus(t, e.req(t, "POST", "/-/api/ssl/certificates", string(body), "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/ssl/certificates", `{"pem":"garbage"}`, "admin", nil, true), http.StatusBadRequest)
	r = e.req(t, "POST", "/-/api/ssl/certificates", string(body), "admin", nil, true)
	wantStatus(t, r, http.StatusCreated)
	var added struct{ Certificates []certJSON }
	json.Unmarshal([]byte(r.body), &added)
	if len(added.Certificates) != 1 || added.Certificates[0].ID != id || added.Certificates[0].SubjectCommonName != "Corp Root CA" ||
		!added.Certificates[0].IsCA || added.Certificates[0].SubjectOrganization != "Test Org" || !added.Certificates[0].Trusted {
		t.Fatalf("added %+v", added)
	}
	wantStatus(t, e.req(t, "POST", "/-/api/ssl/certificates", string(body), "admin", nil, true), http.StatusConflict)
	r = e.get(t, "/-/api/ssl/certificates", "admin")
	json.Unmarshal([]byte(r.body), &added)
	if len(added.Certificates) != 1 || added.Certificates[0].ID != id || !strings.Contains(added.Certificates[0].PEM, "BEGIN CERTIFICATE") {
		t.Fatalf("list %s", r.body)
	}
	// The live settings carry it (the proxy package reads them on change).
	if certs := e.ui.Auth.Snapshot().Settings().TrustedCertificates(); len(certs) != 1 || !certs[0].Equal(ca) {
		t.Fatalf("snapshot certs %d", len(certs))
	}

	// Retrieve from a TLS server; the leaf is reported and is not trusted.
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	port, _ := strconv.Atoi(u.Port())
	wantStatus(t, e.req(t, "POST", "/-/api/ssl/retrieve", `{"host":"127.0.0.1","port":1}`, "admin", nil, true), http.StatusBadGateway)
	wantStatus(t, e.req(t, "POST", "/-/api/ssl/retrieve", `{"host":"bad host"}`, "admin", nil, true), http.StatusBadRequest)
	r = e.req(t, "POST", "/-/api/ssl/retrieve", `{"host":"127.0.0.1","port":`+strconv.Itoa(port)+`}`, "admin", nil, true)
	wantStatus(t, r, http.StatusOK)
	var got struct {
		Host         string
		Port         int
		Certificates []certJSON
	}
	json.Unmarshal([]byte(r.body), &got)
	if got.Port != port || len(got.Certificates) != 1 || got.Certificates[0].ID != meta.Fingerprint(ts.Certificate()) || got.Certificates[0].Trusted {
		t.Fatalf("retrieve %s", r.body)
	}
	// Adding what retrieve returned makes it trusted.
	body, _ = json.Marshal(map[string]string{"pem": got.Certificates[0].PEM})
	wantStatus(t, e.req(t, "POST", "/-/api/ssl/certificates", string(body), "admin", nil, true), http.StatusCreated)
	r = e.req(t, "POST", "/-/api/ssl/retrieve", `{"host":"127.0.0.1","port":`+strconv.Itoa(port)+`}`, "admin", nil, true)
	json.Unmarshal([]byte(r.body), &got)
	if !got.Certificates[0].Trusted {
		t.Fatal("retrieved certificate not marked trusted after adding")
	}

	upper := strings.ToUpper(id[:8]) + id[8:]
	wantStatus(t, e.req(t, "DELETE", "/-/api/ssl/certificates/"+upper, "", "admin", nil, false), http.StatusForbidden)
	wantStatus(t, e.req(t, "DELETE", "/-/api/ssl/certificates/"+upper, "", "admin", nil, true), http.StatusNoContent)
	wantStatus(t, e.req(t, "DELETE", "/-/api/ssl/certificates/"+id, "", "admin", nil, true), http.StatusNotFound)
	r = e.get(t, "/-/api/ssl/certificates", "admin")
	json.Unmarshal([]byte(r.body), &added)
	if len(added.Certificates) != 1 || added.Certificates[0].ID == id {
		t.Fatalf("after delete %s", r.body)
	}
	var kinds []string
	for _, ev := range sink.Events() {
		if strings.HasPrefix(ev.Kind, "ssl.") {
			kinds = append(kinds, ev.Kind+":"+ev.Subject)
		}
	}
	if strings.Join(kinds, " ") != "ssl.add:"+id+" ssl.add:"+meta.Fingerprint(ts.Certificate())+" ssl.remove:"+id {
		t.Fatalf("events %v", kinds)
	}
}

func TestSettingsHTTP(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	r := e.get(t, "/-/api/settings", "admin")
	wantStatus(t, r, http.StatusOK)
	var dto settingsDTO
	json.Unmarshal([]byte(r.body), &dto)
	if dto.HTTP.ConnectTimeoutSeconds != 10 || dto.HTTP.TimeoutSeconds != 30 || dto.HTTP.Retries != 0 || dto.MetricsAnonymous ||
		dto.HTTP.Proxy.NonProxyHosts == nil {
		t.Fatalf("defaults %s", r.body)
	}
	if !strings.Contains(r.body, `"nonProxyHosts":[]`) {
		t.Fatalf("nonProxyHosts not an array: %s", r.body)
	}
	body := `{"metricsAnonymous":true,"http":{"userAgentSuffix":" corp/1 ","connectTimeoutSeconds":5,"timeoutSeconds":20,"retries":2,
		"proxy":{"host":"proxy.corp","port":3128,"username":"u","password":"proxy-pw-1","nonProxyHosts":["*.corp"," 10.* ",""]}}}`
	r = e.req(t, "PUT", "/-/api/settings", body, "admin", nil, true)
	wantStatus(t, r, http.StatusOK)
	json.Unmarshal([]byte(r.body), &dto)
	if dto.HTTP.UserAgentSuffix != "corp/1" || dto.HTTP.Retries != 2 || dto.HTTP.Proxy.Host != "proxy.corp" || !dto.HTTP.Proxy.PasswordSet ||
		!dto.MetricsAnonymous || strings.Join(dto.HTTP.Proxy.NonProxyHosts, ",") != "*.corp,10.*" || strings.Contains(r.body, "proxy-pw-1") {
		t.Fatalf("after put %s", r.body)
	}
	s, err := e.db.LoadSettings(context.Background())
	if err != nil || s.HTTP.Proxy.Password != "proxy-pw-1" {
		t.Fatalf("stored %+v %v", s.HTTP, err)
	}
	// An empty password keeps the stored one; the live snapshot has it.
	r = e.req(t, "PUT", "/-/api/settings", `{"http":{"connectTimeoutSeconds":5,"timeoutSeconds":20,"retries":1,"proxy":{"host":"proxy.corp","port":3128,"username":"u"}}}`, "admin", nil, true)
	wantStatus(t, r, http.StatusOK)
	if live := e.ui.Auth.Snapshot().Settings().HTTP; live.Proxy.Password != "proxy-pw-1" || live.Retries != 1 {
		t.Fatalf("live %+v", live)
	}
	// Validation.
	r = e.req(t, "PUT", "/-/api/settings", `{"http":{"connectTimeoutSeconds":5,"timeoutSeconds":20,"retries":11,"proxy":{"host":"proxy.corp","port":3128}}}`, "admin", nil, true)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "http.retries") {
		t.Fatalf("validation %s", r.body)
	}
	// clearPassword, and an empty host drops the password too.
	wantStatus(t, e.req(t, "PUT", "/-/api/settings", `{"http":{"connectTimeoutSeconds":5,"timeoutSeconds":20,"proxy":{"host":"proxy.corp","port":3128,"clearPassword":true}}}`, "admin", nil, true), http.StatusOK)
	if s, _ := e.db.LoadSettings(context.Background()); s.HTTP.Proxy.Password != "" {
		t.Fatal("clearPassword kept the password")
	}
	if evs := eventsOf(sink, "settings.http"); len(evs) != 3 || !strings.Contains(evs[0].Detail, "proxy.password") || strings.Contains(evs[0].Detail, "proxy-pw") {
		t.Fatalf("events %+v", evs)
	}
	if evs := eventsOf(sink, "settings.update"); len(evs) != 1 || evs[0].Detail != "metricsAnonymous" {
		t.Fatalf("settings.update events %+v", evs)
	}
}
