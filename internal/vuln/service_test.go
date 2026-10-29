package vuln

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
)

var ctx = context.Background()

// fakeOSV serves the OSV API's querybatch and vulns endpoints and dumps.
type fakeOSV struct {
	mu        sync.Mutex
	affected  map[string][]string // "<ecosystem>|<name>|<version>" -> ids
	vulns     map[string]string   // id -> OSV JSON
	dumps     map[string][]byte   // ecosystem -> all.zip
	fail      bool
	split     bool // answer one id per page
	batches   int
	queries   int
	details   int
	lastQuery []osvQuery
}

func (f *fakeOSV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/querybatch":
		f.batches++
		var body struct{ Queries []osvQuery }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Queries) > batchSize {
			http.Error(w, "bad", 400)
			return
		}
		f.queries += len(body.Queries)
		f.lastQuery = body.Queries
		type vuln struct {
			ID       string `json:"id"`
			Modified string `json:"modified"`
		}
		type result struct {
			Vulns []vuln `json:"vulns,omitempty"`
			Next  string `json:"next_page_token,omitempty"`
		}
		var out struct {
			Results []result `json:"results"`
		}
		for _, q := range body.Queries {
			ids := f.affected[q.Package.Ecosystem+"|"+q.Package.Name+"|"+q.Version]
			var res result
			if f.split && len(ids) > 1 {
				if q.PageToken == "" {
					ids, res.Next = ids[:1], "page2"
				} else {
					ids = ids[1:]
				}
			}
			for _, id := range ids {
				res.Vulns = append(res.Vulns, vuln{ID: id, Modified: "2024-05-01T00:00:00Z"})
			}
			out.Results = append(out.Results, res)
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/vulns/"):
		f.details++
		js, ok := f.vulns[strings.TrimPrefix(r.URL.Path, "/v1/vulns/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(js))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/all.zip"):
		b, ok := f.dumps[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/all.zip")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOSV) counts() (batches, details int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batches, f.details
}

type recorder struct {
	mu  sync.Mutex
	evs []string
}

func (r *recorder) Emit(kind, actor, repoName, subject, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, kind+" "+repoName+" "+subject+" "+detail)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.evs)
}

type env struct {
	s   *Service
	osv *fakeOSV
	url string
	ev  *recorder
}

// newEnv is a Service over a new database with an npm proxy (firewall on,
// blocking malware and critical advisories), an npm hosted repository and
// a PyPI proxy without a firewall, and a fake OSV API.
func newEnv(t *testing.T, mode string) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	reg := repo.NewRegistry(db, nil)
	for _, d := range []config.Repository{
		{Name: "npm-proxy", Format: "npm", Type: "proxy", Remote: "https://registry.example/",
			Firewall: &config.Firewall{Enabled: true, BlockSeverityAtLeast: "critical"}},
		{Name: "npm-hosted", Format: "npm", Type: "hosted"},
		{Name: "pypi-proxy", Format: "pypi", Type: "proxy", Remote: "https://pypi.example/"},
	} {
		if _, err := reg.CreateDef(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeOSV{affected: map[string][]string{}, vulns: map[string]string{}, dumps: map[string][]byte{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	e := &env{osv: f, url: ts.URL, ev: &recorder{}}
	e.s = &Service{DB: db, Repos: reg, Events: e.ev, TempDir: dir}
	t.Cleanup(e.s.Close) // before the database closes
	set := DefaultSettings()
	set.Enabled, set.Mode, set.APIURL, set.DumpURL, set.MaxImportMiB = true, mode, ts.URL, ts.URL, 4
	if err := e.s.SaveSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	return e
}

const (
	ghsaCritical = `{"id":"GHSA-crit","modified":"2024-05-01T00:00:00Z","published":"2024-04-01T00:00:00Z","aliases":["CVE-2024-1"],
"summary":"Remote code execution in lodash",
"severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}],
"affected":[{"package":{"ecosystem":"npm","name":"lodash"},"ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"4.17.21"}]}]}]}`
	ghsaLow = `{"id":"GHSA-low","modified":"2024-05-01T00:00:00Z","summary":"Minor issue",
"severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:H/PR:N/UI:R/S:U/C:L/I:N/A:N"}],
"affected":[{"package":{"ecosystem":"npm","name":"lodash"},"ranges":[{"type":"SEMVER","events":[{"introduced":"4.0.0"},{"fixed":"4.17.22"}]}]}]}`
	malEvil = `{"id":"MAL-2024-0001","modified":"2024-05-01T00:00:00Z","summary":"Malicious code in evil (npm)",
"affected":[{"package":{"ecosystem":"npm","name":"evil"},"ranges":[{"type":"SEMVER","events":[{"introduced":"0"}]}]}]}`
)

func TestOnlineLookupAndCache(t *testing.T) {
	e := newEnv(t, "online")
	e.osv.affected["npm|lodash|4.17.20"] = []string{"GHSA-crit", "GHSA-low"}
	e.osv.vulns["GHSA-crit"], e.osv.vulns["GHSA-low"] = ghsaCritical, ghsaLow
	e.osv.split = true // two pages for lodash

	res, err := e.s.Lookup(ctx, Package{"npm", "lodash", "4.17.20"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || len(res.Vulnerabilities) != 2 {
		t.Fatalf("result: %+v", res)
	}
	v := res.Vulnerabilities[0]
	if v.ID != "GHSA-crit" || v.Severity != "critical" || v.Score == nil || *v.Score != 9.8 || !slices.Equal(v.FixedIn, []string{"4.17.21"}) ||
		!slices.Equal(v.Aliases, []string{"CVE-2024-1"}) || v.URL != "https://osv.dev/vulnerability/GHSA-crit" || v.Malicious ||
		v.Published != "2024-04-01T00:00:00Z" || v.Summary != "Remote code execution in lodash" {
		t.Fatalf("critical: %+v", v)
	}
	if res.Vulnerabilities[1].ID != "GHSA-low" || res.Vulnerabilities[1].Severity != "low" {
		t.Fatalf("low: %+v", res.Vulnerabilities[1])
	}
	batches, details := e.osv.counts()
	if batches != 2 || details != 2 {
		t.Fatalf("API calls: %d batches %d details, want 2 and 2", batches, details)
	}
	// Within cacheHours: no API call. A clean version is cached too.
	if _, err := e.s.Lookup(ctx, Package{"npm", "lodash", "4.17.20"}); err != nil {
		t.Fatal(err)
	}
	res, _ = e.s.Lookup(ctx, Package{"npm", "lodash", "4.17.22"})
	if len(res.Vulnerabilities) != 0 || res.Status != "ok" {
		t.Fatalf("clean: %+v", res)
	}
	e.s.Lookup(ctx, Package{"npm", "lodash", "4.17.22"})
	if b, d := e.osv.counts(); b != 3 || d != 2 {
		t.Fatalf("API calls after cache hits: %d batches %d details", b, d)
	}
	// The details are stored: a version only the local ranges cover is
	// found without asking (the offline mode reads only them).
	set := e.s.Settings(ctx)
	set.Mode = "offline"
	if err := e.s.SaveSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	res, _ = e.s.Lookup(ctx, Package{"npm", "lodash", "4.10.0"})
	if len(res.Vulnerabilities) != 2 {
		t.Fatalf("offline from stored details: %+v", res)
	}
	if b, _ := e.osv.counts(); b != 3 {
		t.Fatalf("offline asked the API")
	}
	// Disabled: nothing is reported.
	set.Enabled = false
	e.s.SaveSettings(ctx, set)
	if res, _ := e.s.Lookup(ctx, Package{"npm", "lodash", "4.10.0"}); res.Status != "disabled" || len(res.Vulnerabilities) != 0 {
		t.Fatalf("disabled: %+v", res)
	}
}

// An online failure fails open: the lookup reports status unknown with the
// local data, and the firewall still blocks what the local data shows as
// malicious, but nothing else.
func TestOnlineFailureFailsOpen(t *testing.T) {
	e := newEnv(t, "online")
	if _, err := e.s.Import(ctx, bytes.NewReader(makeZip(t, [][2]string{{"MAL-2024-0001.json", malEvil}})), "admin"); err != nil {
		t.Fatal(err)
	}
	e.osv.fail = true
	res, err := e.s.Lookup(ctx, Package{"npm", "lodash", "4.17.20"})
	if err != nil || res.Status != "unknown" || len(res.Vulnerabilities) != 0 {
		t.Fatalf("lodash: %+v %v", res, err)
	}
	if err := e.s.Firewall(ctx, "npm-proxy", "lodash/-/lodash-4.17.20.tgz"); err != nil {
		t.Fatalf("lodash refused while the API is down: %v", err)
	}
	err = e.s.Firewall(ctx, "npm-proxy", "evil/-/evil-1.0.0.tgz")
	if repo.StatusOf(err) != http.StatusForbidden || !strings.Contains(err.Error(), "MAL-2024-0001 (malicious package)") {
		t.Fatalf("evil: %v", err)
	}
	st, _ := e.s.Status(ctx)
	if st.LookupFailures != 3 || st.LastFailure == nil || !strings.Contains(st.LastFailure.Message, "status 503") {
		t.Fatalf("status: %+v", st)
	}
	// A stale cached answer stands in for a failed refresh.
	e.osv.fail = false
	e.osv.affected["npm|lodash|4.17.20"] = []string{"GHSA-crit"}
	e.osv.vulns["GHSA-crit"] = ghsaCritical
	if err := e.s.DB.PutVulnQueries(ctx, []meta.VulnQuery{{Ecosystem: "npm", Package: "lodash", Version: "4.17.20", IDs: []string{"GHSA-crit"},
		Checked: time.Now().Add(-48 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	e.osv.fail = true
	res, _ = e.s.Lookup(ctx, Package{"npm", "lodash", "4.17.20"})
	if res.Status != "unknown" || len(res.Vulnerabilities) != 1 || res.Vulnerabilities[0].ID != "GHSA-crit" {
		t.Fatalf("stale: %+v", res)
	}
}

func TestFirewallQuarantineAndRelease(t *testing.T) {
	e := newEnv(t, "online")
	e.osv.affected["npm|lodash|4.17.20"] = []string{"GHSA-crit"}
	e.osv.affected["npm|lodash|4.17.21"] = []string{"GHSA-low"}
	e.osv.affected["npm|evil|1.0.0"] = []string{"MAL-2024-0001"}
	e.osv.vulns["GHSA-crit"], e.osv.vulns["GHSA-low"], e.osv.vulns["MAL-2024-0001"] = ghsaCritical, ghsaLow, malEvil

	err := e.s.Firewall(ctx, "npm-proxy", "lodash/-/lodash-4.17.20.tgz")
	if repo.StatusOf(err) != http.StatusForbidden || !strings.Contains(err.Error(), "npm lodash@4.17.20 is quarantined") ||
		!strings.Contains(err.Error(), "GHSA-crit (critical 9.8)") {
		t.Fatalf("critical: %v", err)
	}
	if err := e.s.Firewall(ctx, "npm-proxy", "lodash/-/lodash-4.17.21.tgz"); err != nil {
		t.Fatalf("low is served: %v", err)
	}
	if err := e.s.Firewall(ctx, "npm-proxy", "evil/-/evil-1.0.0.tgz"); repo.StatusOf(err) != http.StatusForbidden {
		t.Fatalf("malware: %v", err)
	}
	// Hosted repositories, proxies without a firewall and metadata files are never refused.
	for _, c := range [][2]string{{"npm-hosted", "evil/-/evil-1.0.0.tgz"}, {"pypi-proxy", "packages/evil/evil-1.0.tar.gz"}, {"npm-proxy", "evil"}} {
		if err := e.s.Firewall(ctx, c[0], c[1]); err != nil {
			t.Fatalf("%s %s: %v", c[0], c[1], err)
		}
	}
	e.s.Firewall(ctx, "npm-proxy", "evil/-/evil-1.0.0.tgz") // counted, one event
	items, _ := e.s.DB.ListQuarantine(ctx, "npm-proxy", 10)
	if len(items) != 2 {
		t.Fatalf("quarantine: %+v", items)
	}
	for _, it := range items {
		if it.Name == "evil" && (it.Count != 2 || !it.Malicious || !slices.Equal(it.IDs, []string{"MAL-2024-0001"})) {
			t.Fatalf("evil: %+v", it)
		}
	}
	evs := e.ev.list()
	if len(evs) != 2 || evs[0] != "firewall.blocked npm-proxy lodash@4.17.20 GHSA-crit" || evs[1] != "firewall.blocked npm-proxy evil@1.0.0 MAL-2024-0001" {
		t.Fatalf("events: %q", evs)
	}

	// Release: an allow-list entry, the row gone, the component served.
	entry, err := e.s.Release(ctx, "npm-proxy", "lodash", "4.17.20")
	if err != nil || entry != `^lodash@4\.17\.20$` {
		t.Fatalf("release: %q %v", entry, err)
	}
	if fw := e.s.Repos.Get("npm-proxy").Firewall; !slices.Equal(fw.AllowList, []string{entry}) || !fw.Enabled || fw.BlockSeverityAtLeast != "critical" {
		t.Fatalf("firewall after release: %+v", fw)
	}
	if err := e.s.Firewall(ctx, "npm-proxy", "lodash/-/lodash-4.17.20.tgz"); err != nil {
		t.Fatalf("released: %v", err)
	}
	if items, _ := e.s.DB.ListQuarantine(ctx, "npm-proxy", 10); len(items) != 1 || items[0].Name != "evil" {
		t.Fatalf("quarantine after release: %+v", items)
	}
	if _, err := e.s.Release(ctx, "npm-hosted", "x", "1"); repo.StatusOf(err) != http.StatusBadRequest {
		t.Fatalf("release on hosted: %v", err)
	}
}

func makeZip(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f[0])
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f[1]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportAndOfflineLookup(t *testing.T) {
	e := newEnv(t, "offline")
	withdrawn := `{"id":"GHSA-gone","withdrawn":"2024-06-01T00:00:00Z","affected":[{"package":{"ecosystem":"npm","name":"lodash"},"versions":["4.17.20"]}]}`
	pysec := `{"id":"PYSEC-2024-1","modified":"2024-05-01T00:00:00Z","database_specific":{"severity":"HIGH"},
"affected":[{"package":{"ecosystem":"PyPI","name":"Requests"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"2.31.0"}]}]}]}`
	debian := `{"id":"DSA-1","affected":[{"package":{"ecosystem":"Debian:12","name":"openssl"},"versions":["3.0.1"]}]}`
	zipped := makeZip(t, [][2]string{
		{"GHSA-crit.json", ghsaCritical}, {"MAL-2024-0001.json", malEvil}, {"GHSA-gone.json", withdrawn},
		{"sub/PYSEC-2024-1.json", pysec}, {"DSA-1.json", debian}, {"README.txt", "not an advisory"}, {"bad.json", "{"},
	})
	res, err := e.s.Import(ctx, bytes.NewReader(zipped), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if res.Advisories != 4 || res.Rows != 4 || res.Skipped != 3 {
		t.Fatalf("import: %+v", res)
	}
	for _, c := range []struct {
		p    Package
		want []string
	}{
		{Package{"npm", "lodash", "4.17.20"}, []string{"GHSA-crit"}}, // the withdrawn one is not reported
		{Package{"npm", "lodash", "4.17.21"}, nil},
		{Package{"npm", "evil", "9.9.9"}, []string{"MAL-2024-0001"}},
		{Package{"PyPI", "requests", "2.30.0"}, []string{"PYSEC-2024-1"}}, // names are matched normalized
		{Package{"PyPI", "requests", "2.31.0"}, nil},
		{Package{"npm", "other", "1.0.0"}, nil},
	} {
		got, err := e.s.Lookup(ctx, c.p)
		var ids []string
		for _, v := range got.Vulnerabilities {
			ids = append(ids, v.ID)
		}
		if err != nil || got.Status != "ok" || !slices.Equal(ids, c.want) {
			t.Errorf("%+v: %v %+v %v", c.p, ids, got, err)
		}
	}
	if b, _ := e.osv.counts(); b != 0 {
		t.Fatalf("offline asked the API %d times", b)
	}
	// The index, once built, skips the database for packages without advisories.
	waitIndex(t, e.s)
	if e.s.maybe("npm", "other") || !e.s.maybe("npm", "evil") || !e.s.maybe("PyPI", "requests") {
		t.Fatal("index")
	}
	st, err := e.s.Status(ctx)
	if err != nil || st.Advisories != 4 || st.Malicious != 1 || st.ByEcosystem["npm"] != 3 || st.LastImport == nil || st.LastImport.Source != "upload" {
		t.Fatalf("status: %+v %v", st, err)
	}
	if evs := e.ev.list(); len(evs) != 1 || evs[0] != "vulnerability.import  upload 4 advisories" {
		t.Fatalf("events: %q", evs)
	}
}

func waitIndex(t *testing.T, s *Service) {
	t.Helper()
	s.maybe("", "") // starts the build
	for i := 0; s.index.Load() == nil; i++ {
		if i > 200 {
			t.Fatal("index not built")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestImportRefusesUnsafeArchives(t *testing.T) {
	e := newEnv(t, "offline")
	imp := func(b []byte) error {
		_, err := e.s.Import(ctx, bytes.NewReader(b), "admin")
		return err
	}
	var ae *ArchiveError
	for _, name := range []string{"../evil.json", "a/../../evil.json", "/etc/evil.json", `..\evil.json`} {
		if err := imp(makeZip(t, [][2]string{{"ok.json", malEvil}, {name, malEvil}})); !errors.As(err, &ae) || !strings.Contains(err.Error(), "unsafe") {
			t.Errorf("%q: %v", name, err)
		}
	}
	if st, _ := e.s.Status(ctx); st.Advisories != 0 {
		t.Fatalf("an unsafe archive stored %d advisories", st.Advisories)
	}
	if err := imp([]byte("not a zip")); !errors.As(err, &ae) {
		t.Errorf("not a zip: %v", err)
	}
	// An entry over the per-entry limit: a valid JSON document padded with
	// spaces, which compresses to almost nothing.
	big := `{"id":"GHSA-big"` + strings.Repeat(" ", maxEntryBytes) + `}`
	if err := imp(makeZip(t, [][2]string{{"big.json", big}})); !errors.As(err, &ae) || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big entry: %v", err)
	}
	// An upload over maxImportMiB (4 in newEnv).
	if err := imp(bytes.Repeat([]byte("x"), 4<<20+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
	// One at a time.
	e.s.busy.Lock()
	if err := imp(makeZip(t, nil)); !errors.Is(err, ErrBusy) {
		t.Errorf("busy: %v", err)
	}
	e.s.busy.Unlock()
}

func TestSync(t *testing.T) {
	e := newEnv(t, "offline")
	e.osv.dumps["npm"] = makeZip(t, [][2]string{{"MAL-2024-0001.json", malEvil}})
	set := e.s.Settings(ctx)
	set.Ecosystems = []string{"npm", "PyPI"} // PyPI's dump is missing: the error is reported, npm stored
	if err := e.s.SaveSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	var lines []string
	res, err := e.s.Sync(ctx, func(f string, a ...any) { lines = append(lines, f) })
	if err == nil || !strings.Contains(err.Error(), "PyPI: download answered status 404") || res.Advisories != 1 {
		t.Fatalf("sync: %+v %v %v", res, err, lines)
	}
	if got, _ := e.s.Lookup(ctx, Package{"npm", "evil", "1.0.0"}); len(got.Vulnerabilities) != 1 {
		t.Fatalf("after sync: %+v", got)
	}
	if st, _ := e.s.Status(ctx); st.LastImport == nil || st.LastImport.Source != "sync:npm" {
		t.Fatalf("status: %+v", st)
	}
	set.Ecosystems = []string{"Debian"}
	if err := e.s.SaveSettings(ctx, set); err == nil {
		t.Fatal("an ecosystem without data was accepted")
	}
}

func TestScan(t *testing.T) {
	e := newEnv(t, "online")
	e.osv.affected["npm|lodash|4.17.20"] = []string{"GHSA-crit"}
	e.osv.affected["npm|evil|1.0.0"] = []string{"MAL-2024-0001"}
	e.osv.vulns["GHSA-crit"], e.osv.vulns["MAL-2024-0001"] = ghsaCritical, malEvil
	now := time.Now().UTC()
	for _, p := range []string{"lodash/-/lodash-4.17.20.tgz", "evil/-/evil-1.0.0.tgz", "clean/-/clean-1.0.0.tgz", "clean/-/clean-1.0.0.tgz.sha1", "lodash"} {
		if err := e.s.DB.PutAsset(ctx, meta.Asset{Repo: "npm-hosted", Path: p, BlobSHA256: "x", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	tot, err := e.s.Scan(ctx, "", func(f string, a ...any) { lines = append(lines, f) })
	if err != nil || tot.Repositories != 3 || tot.Components != 3 || tot.Vulnerable != 2 || tot.Unchecked != 0 {
		t.Fatalf("scan: %+v %v %q", tot, err, lines)
	}
	c, _ := e.s.DB.VulnCountsByRepo(ctx, "npm-hosted")
	if got := c["npm-hosted"]; got.Components != 3 || got.Vulnerable != 2 || got.Malicious != 1 || got.Critical != 1 {
		t.Fatalf("counts: %+v", got)
	}
	top, _ := e.s.DB.TopVulnFindings(ctx, "npm-hosted", 10)
	if len(top) != 2 || top[0].Name != "evil@1.0.0" || top[1].Name != "lodash@4.17.20" || top[1].Severity != "critical" {
		t.Fatalf("top: %+v", top)
	}
	// A component gone since is pruned by the next scan; a failed API marks unchecked.
	e.s.DB.DeleteAsset(ctx, "npm-hosted", "evil/-/evil-1.0.0.tgz")
	e.osv.fail = true
	e.s.DB.PutVulnQueries(ctx, []meta.VulnQuery{{Ecosystem: "npm", Package: "clean", Version: "1.0.0", Checked: now.Add(-48 * time.Hour)}})
	if tot, err = e.s.Scan(ctx, "npm-hosted", func(string, ...any) {}); err != nil || tot.Components != 2 || tot.Unchecked != 1 {
		t.Fatalf("rescan: %+v %v", tot, err)
	}
	c, _ = e.s.DB.VulnCountsByRepo(ctx, "npm-hosted")
	if got := c["npm-hosted"]; got.Components != 2 || got.Malicious != 0 || got.Unchecked != 1 || got.Critical != 1 {
		t.Fatalf("counts after rescan: %+v", got)
	}
}
