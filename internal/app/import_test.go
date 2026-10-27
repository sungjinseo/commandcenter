package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/importer"
	"commandcenter/internal/importer/nexus/nexustest"
	"commandcenter/internal/meta"
	"commandcenter/internal/tasks"
)

// The source password of the fake Nexus; it must never be persisted or logged.
const (
	srcUser = "nexus-admin"
	srcPass = "Imp0rt-S3cret-PW!"
)

type importEnv struct {
	t     *testing.T
	a     *App
	ts    *httptest.Server
	src   *nexustest.Fake
	token string
	logs  *syncBuf
	resps *syncBuf // every API response body, for the secret check
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newImportEnv(t *testing.T, extra string) *importEnv {
	t.Helper()
	cfg := testConfig(t, extra)
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = append(cfg.Users, config.User{Name: "root", PasswordHash: string(h), Roles: []string{"admin"}})
	logs := &syncBuf{}
	a, err := New(cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	_, tok, err := a.Auth.CreateToken(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	return &importEnv{t: t, a: a, ts: ts, src: nexustest.New(t, srcUser, srcPass), token: tok, logs: logs, resps: &syncBuf{}}
}

// call sends a JSON request with the admin token (asUI: basic auth + UI header).
func (e *importEnv) call(method, path string, body any, asUI bool) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if asUI {
		req.SetBasicAuth("root", "pw")
		req.Header.Set(auth.UIHeader, "1")
	} else {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	e.resps.Write(raw)
	json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"raw": string(raw)}
	}
	return resp.StatusCode, out
}

func (e *importEnv) body(extra map[string]any) map[string]any {
	b := map[string]any{"url": e.src.URL, "username": srcUser, "password": srcPass}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// start starts an import and returns the job.
func (e *importEnv) start(extra map[string]any) string {
	e.t.Helper()
	code, res := e.call("POST", "/-/api/import/nexus", e.body(extra), false)
	if code != http.StatusAccepted {
		e.t.Fatalf("start: %d %v", code, res)
	}
	return res["job"].(string)
}

// report waits for the run to end and returns the job's report.
func (e *importEnv) report(job string) *importer.Report {
	e.t.Helper()
	e.a.Tasks.Wait()
	code, res := e.call("GET", "/-/api/import/jobs/"+job, nil, false)
	if code != 200 {
		e.t.Fatalf("job %s: %d %v", job, code, res)
	}
	b, _ := json.Marshal(res["report"])
	rep := &importer.Report{}
	if err := json.Unmarshal(b, rep); err != nil {
		e.t.Fatalf("report: %v %s", err, b)
	}
	return rep
}

func (e *importEnv) get(path string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	e.resps.Write(b)
	return resp.StatusCode, string(b)
}

func repoReport(t *testing.T, rep *importer.Report, source string) *importer.RepoReport {
	t.Helper()
	for _, rr := range rep.Repositories {
		if rr.Source == source {
			return rr
		}
	}
	t.Fatalf("no report for %s: %+v", source, rep.Repositories)
	return nil
}

// An import of every supported format from the fake Nexus (real 3.97
// response shapes) into a running app; then an idempotent re-run.
func TestImportNexusEndToEnd(t *testing.T) {
	e := newImportEnv(t, "")
	e.src.PageSize = 2 // exercise continuationToken paging
	job := e.start(map[string]any{"job": "e2e"})
	if job != "e2e" {
		t.Fatalf("job %q", job)
	}
	rep := e.report(job)
	if rep.Status != importer.StatusOK {
		t.Fatalf("status %s: %s\n%+v", rep.Status, rep.Message, rep)
	}

	// Definitions. maven-releases exists in the test config (maven hosted): reused.
	reg := e.a.Registry
	for name, want := range map[string]string{
		"mvn-h": "maven/hosted", "maven-snapshots": "maven/hosted", "maven-central": "maven/proxy", "maven-public": "maven/group",
		"npm-h": "npm/hosted", "npm-p": "npm/proxy", "npm-g": "npm/group", "pypi-h": "pypi/hosted", "cargo-h": "cargo/hosted",
		"maven-releases": "maven/hosted",
	} {
		rp := reg.Get(name)
		if rp == nil || rp.Format+"/"+rp.Type != want {
			t.Fatalf("%s: %+v, want %s", name, rp, want)
		}
	}
	if d := repoReport(t, rep, "maven-releases").Definition; d != importer.DefExists {
		t.Fatalf("maven-releases definition %q", d)
	}
	if d := repoReport(t, rep, "mvn-h").Definition; d != importer.DefCreated {
		t.Fatalf("mvn-h definition %q", d)
	}
	snap := reg.Get("maven-snapshots")
	if snap.WritePolicy != "allow" || snap.VersionPolicy != "snapshot" || snap.BlobStore != "default" {
		t.Fatalf("maven-snapshots: %+v", snap.Repository)
	}
	if h := reg.Get("mvn-h"); h.WritePolicy != "allow_once" || h.VersionPolicy != "release" {
		t.Fatalf("mvn-h: %+v", h.Repository)
	}
	central := reg.Get("maven-central")
	if central.Remote != "https://repo1.maven.org/maven2/" || central.ContentMaxAge != config.Never ||
		central.MetadataMaxAge.Text() != "24h" || central.NegativeCacheTTL.Text() != "24h" {
		t.Fatalf("maven-central: %+v", central.Repository)
	}
	if d := repoReport(t, rep, "maven-public").Definition; d != importer.DefExists || rep.Created != 8 {
		t.Fatalf("maven-public definition %q, created %d", d, rep.Created)
	}
	if g := reg.Get("npm-g"); strings.Join(g.Members, ",") != "npm-h,npm-p" {
		t.Fatalf("npm-g members %v", g.Members)
	}
	// nuget: unsupported, reported with counts; no repository created.
	if len(rep.Unsupported) != 1 || rep.Unsupported[0].Format != "nuget" || rep.Unsupported[0].Count != 3 {
		t.Fatalf("unsupported %+v", rep.Unsupported)
	}
	if reg.Get("nuget-hosted") != nil || reg.Get("nuget-group") != nil {
		t.Fatal("an unsupported repository was created")
	}
	if len(rep.NotImported) == 0 || !strings.Contains(rep.NotImported[0], "users, roles") {
		t.Fatalf("not imported %v", rep.NotImported)
	}

	// Content, per format.
	if c := repoReport(t, rep, "mvn-h").Counts; c.Imported != 2 || c.Failed != 0 {
		t.Fatalf("mvn-h counts %+v", c)
	}
	if code, body := e.get("/repository/mvn-h/com/ex/app/maven-metadata.xml"); code != 200 || !strings.Contains(body, "<version>1.0</version>") {
		t.Fatalf("maven metadata: %d %s", code, body)
	}
	if code, _ := e.get("/repository/mvn-h/com/ex/app/1.0/app-1.0.jar.sha1"); code != 200 {
		t.Fatalf("maven sha1: %d", code)
	}
	// npm: 3 tarballs, 2 packuments (generated here: skipped).
	if c := repoReport(t, rep, "npm-h").Counts; c.Assets != 5 || c.Imported != 3 || c.Skipped != 2 || c.Failed != 0 {
		t.Fatalf("npm-h counts %+v", c)
	}
	code, body := e.get("/repository/npm-h/demo")
	var pk struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Deprecated string `json:"deprecated"`
		} `json:"versions"`
	}
	json.Unmarshal([]byte(body), &pk)
	if code != 200 || pk.DistTags["beta"] != "1.1.0-beta.1" || pk.DistTags["latest"] != "1.0.0" || pk.Versions["1.0.0"].Deprecated != "use 1.1" {
		t.Fatalf("npm packument: %d %s", code, body)
	}
	if code, body := e.get("/repository/npm-h/@acme%2fscoped"); code != 200 || !strings.Contains(body, `"2.0.0"`) {
		t.Fatalf("scoped packument: %d %s", code, body)
	}
	// pypi: the wheel and the sdist; the PEP 658 .metadata files are skipped.
	if c := repoReport(t, rep, "pypi-h").Counts; c.Imported != 2 || c.Skipped != 2 || c.Failed != 0 {
		t.Fatalf("pypi-h counts %+v", c)
	}
	if code, body := e.get("/repository/pypi-h/simple/demo-pkg/"); code != 200 || !strings.Contains(body, "demo_pkg-1.0.0-py3-none-any.whl") ||
		!strings.Contains(body, "demo_pkg-1.0.0.tar.gz") {
		t.Fatalf("pypi simple: %d %s", code, body)
	}
	// cargo: the crate with its index line; the index file is skipped.
	if c := repoReport(t, rep, "cargo-h").Counts; c.Imported != 1 || c.Skipped != 1 || c.Failed != 0 {
		t.Fatalf("cargo-h counts %+v", c)
	}
	if code, body := e.get("/repository/cargo-h/de/mo/democrate"); code != 200 ||
		!strings.Contains(body, "c503c661d104afea98e87ba3c8688f4d83b8693d91aef04f839126a873751274") {
		t.Fatalf("cargo index: %d %s", code, body)
	}
	if code, body := e.get("/repository/cargo-h/crates/democrate/0.1.0/download"); code != 200 || len(body) != 150 {
		t.Fatalf("crate download: %d %d", code, len(body))
	}
	if rep.Totals.Imported != 8 || rep.Totals.Bytes == 0 {
		t.Fatalf("totals %+v", rep.Totals)
	}
	// Proxies: none of the recorded ones authenticates to its remote.
	if len(rep.ProxiesNeedingCredentials) != 0 {
		t.Fatalf("proxies needing credentials %v", rep.ProxiesNeedingCredentials)
	}
	if nonGet := e.src.NonGET(); len(nonGet) != 0 {
		t.Fatalf("the import modified the source: %v", nonGet)
	}

	// The job list has it; the live progress is gone.
	code, list := e.call("GET", "/-/api/import/jobs", nil, false)
	jobs, _ := list["jobs"].([]any)
	if code != 200 || len(jobs) != 1 || list["running"] != nil {
		t.Fatalf("jobs: %d %v", code, list)
	}

	// Idempotent: the same job again downloads nothing.
	downloads := e.src.Downloads.Load()
	e.start(map[string]any{"job": "e2e"})
	again := e.report("e2e")
	if again.Status != importer.StatusOK || again.Totals.Imported != 0 || again.Totals.AlreadyDone != 8 || again.Created != 0 {
		t.Fatalf("re-run: %s %+v created=%d", again.Status, again.Totals, again.Created)
	}
	if d := e.src.Downloads.Load(); d != downloads {
		t.Fatalf("the re-run downloaded %d artifacts", d-downloads)
	}
	// A state recorded without a sha1 is verified again now that the source has one.
	if err := e.a.DB.SetImportState(context.Background(), "e2e", "npm-h", "demo/-/demo-1.0.0.tgz", "", meta.ImportDone, ""); err != nil {
		t.Fatal(err)
	}
	downloads = e.src.Downloads.Load()
	e.start(map[string]any{"job": "e2e"})
	if again := e.report("e2e"); again.Totals.AlreadyDone != 7 || again.Totals.Skipped != 5+1 || e.src.Downloads.Load()-downloads != 1 {
		t.Fatalf("re-verify: %+v, %d downloads", again.Totals, e.src.Downloads.Load()-downloads)
	}
	// A new job finds everything identical in the target: skipped, nothing imported.
	e.start(map[string]any{"job": "e2e-2"})
	fresh := e.report("e2e-2")
	if fresh.Status != importer.StatusOK || fresh.Totals.Imported != 0 || fresh.Totals.Failed != 0 || fresh.Totals.Skipped != 8+5 {
		t.Fatalf("new job: %s %+v %s", fresh.Status, fresh.Totals, fresh.Message)
	}
	for _, rr := range fresh.Repositories {
		if rr.Definition == importer.DefCreated {
			t.Fatalf("re-created %s", rr.Target)
		}
	}

	assertNoSecret(t, e)
}

// assertNoSecret greps the source password, and the Base64 of user:pass
// that Basic auth sends, in the server log, the API responses, the events,
// the task rows (params and run logs), the import jobs (params and
// reports), the asset states and the raw database files.
func assertNoSecret(t *testing.T, e *importEnv) {
	t.Helper()
	ctx := context.Background()
	b64 := base64.StdEncoding.EncodeToString([]byte(srcUser + ":" + srcPass))
	leaks := func(s string) bool { return strings.Contains(s, srcPass) || strings.Contains(s, b64) }
	if leaks(e.logs.String()) {
		t.Fatal("the password is in the server log")
	}
	if e.resps.String() == "" || leaks(e.resps.String()) {
		t.Fatalf("the password is in an API response (or none was seen)")
	}
	if !strings.Contains(e.logs.String(), "import.nexus") {
		t.Fatal("the log check saw no import log lines")
	}
	e.a.Events.Close() // flush the event writer
	evs, err := e.a.DB.ListEvents(ctx, 1000, 0, "")
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v %v", evs, err)
	}
	for _, ev := range evs {
		if leaks(fmt.Sprintf("%+v", ev)) {
			t.Fatalf("the password is in event %+v", ev)
		}
	}
	list, err := e.a.DB.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, tk := range list {
		full, err := e.a.DB.GetTask(ctx, tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if full.Type == tasks.TypeImportNexus {
			seen = true
			if !strings.Contains(full.Params, srcUser) || full.LastLog == "" {
				t.Fatalf("import task row: %+v", full)
			}
		}
		if leaks(fmt.Sprintf("%+v", full)) {
			t.Fatalf("the password is in task %s", tk.ID)
		}
	}
	if !seen {
		t.Fatal("no import.nexus task row")
	}
	jobs, err := e.a.DB.ListImportJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		full, err := e.a.DB.GetImportJob(ctx, j.Job)
		if err != nil {
			t.Fatal(err)
		}
		if leaks(full.Params + full.Report) {
			t.Fatalf("the password is in job %s", j.Job)
		}
		states, err := e.a.DB.ListImportState(ctx, j.Job, 100000)
		if err != nil {
			t.Fatal(err)
		}
		if leaks(fmt.Sprintf("%+v", states)) {
			t.Fatalf("the password is in the asset states of job %s", j.Job)
		}
	}
	for _, f := range []string{"meta.db", "meta.db-wal"} {
		b, err := os.ReadFile(filepath.Join(e.a.Config.DataDir, f))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if leaks(string(b)) {
			t.Fatalf("the password is in %s", f)
		}
	}
}

// A dry run lists what would happen and writes nothing.
func TestImportNexusDryRunWritesNothing(t *testing.T) {
	e := newImportEnv(t, "")
	// npm-p authenticates to its remote: the API shows the block, never the secret.
	e.src.Settings = func(b []byte) []byte {
		i := bytes.Index(b, []byte(`"remoteUrl" : "https://registry.npmjs.org"`))
		j := i + bytes.Index(b[i:], []byte(`"authentication" : null`))
		return append(append(append([]byte{}, b[:j]...), `"authentication" : { "type" : "username", "username" : "svc" }`...), b[j+len(`"authentication" : null`):]...)
	}
	before := len(e.a.Registry.Snapshot().Sorted)
	job := e.start(map[string]any{"job": "dry", "dryRun": true})
	e.a.Tasks.Wait()
	code, res := e.call("GET", "/-/api/import/jobs/"+job, nil, false)
	if code != 200 || res["dryRun"] != true {
		t.Fatalf("dry run job: %d %v", code, res)
	}
	b, _ := json.Marshal(res["report"])
	rep := &importer.Report{}
	json.Unmarshal(b, rep)
	if rep.Status != importer.StatusDryRun || rep.Created != 8 || rep.Totals.WouldImport != 8 || rep.Totals.Imported != 0 {
		t.Fatalf("dry run report: %s created=%d %+v", rep.Status, rep.Created, rep.Totals)
	}
	if d := repoReport(t, rep, "npm-g").Definition; d != importer.DefWouldCreate {
		t.Fatalf("npm-g %q", d)
	}
	if len(rep.ProxiesNeedingCredentials) != 1 || rep.ProxiesNeedingCredentials[0] != "npm-p" {
		t.Fatalf("proxies needing credentials: %v", rep.ProxiesNeedingCredentials)
	}
	if n := len(e.a.Registry.Snapshot().Sorted); n != before {
		t.Fatalf("repositories: %d, want %d", n, before)
	}
	ctx := context.Background()
	if jobs, _ := e.a.DB.ListImportJobs(ctx, 10); len(jobs) != 0 {
		t.Fatalf("a dry run stored a job: %+v", jobs)
	}
	if st, _ := e.a.DB.ImportStateCounts(ctx, job); len(st) != 0 {
		t.Fatalf("a dry run stored asset state: %v", st)
	}
	if e.src.Downloads.Load() != 0 {
		t.Fatal("a dry run downloaded artifacts")
	}
	var blobs int
	filepath.WalkDir(filepath.Join(e.a.Config.DataDir, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs != 0 {
		t.Fatalf("a dry run wrote %d blobs", blobs)
	}
	code, list := e.call("GET", "/-/api/import/jobs", nil, false)
	if dry, _ := list["dryRuns"].([]any); code != 200 || len(dry) != 1 {
		t.Fatalf("jobs: %v", list)
	}
	assertNoSecret(t, e)
}

// A stopped import resumes with the same job: what was done is not fetched
// again, and the rest completes.
func TestImportNexusResumeAfterStop(t *testing.T) {
	e := newImportEnv(t, "")
	blocked := make(chan struct{})
	var once sync.Once
	release := make(chan struct{})
	var served int
	var mu sync.Mutex
	e.src.BeforeDownload = func(r *http.Request, repo, path string) {
		mu.Lock()
		served++
		n := served
		mu.Unlock()
		if n < 4 {
			return
		}
		once.Do(func() { close(blocked) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	job := e.start(map[string]any{"job": "resume", "concurrency": 1, "importDefinitions": true})
	select {
	case <-blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("the import never reached the fourth download")
	}
	code, res := e.call("GET", "/-/api/import/jobs/"+job, nil, false)
	prog, _ := res["progress"].(map[string]any)
	if code != 200 || res["status"] != importer.StatusRunning || prog == nil || prog["assetsDone"].(float64) < 3 {
		t.Fatalf("live progress: %d %v", code, res)
	}
	if code, res := e.call("POST", "/-/api/import/jobs/"+job+"/stop", nil, false); code != http.StatusAccepted {
		t.Fatalf("stop: %d %v", code, res)
	}
	rep := e.report(job)
	if rep.Status != importer.StatusStopped || rep.Totals.Imported != 3 {
		t.Fatalf("stopped report: %s %+v", rep.Status, rep.Totals)
	}
	if code, _ := e.call("POST", "/-/api/import/jobs/"+job+"/stop", nil, false); code != http.StatusConflict {
		t.Fatalf("stop of a finished job: %d", code)
	}
	close(release)
	downloads := e.src.Downloads.Load()
	e.start(map[string]any{"job": "resume"})
	done := e.report(job)
	if done.Status != importer.StatusOK || done.Totals.AlreadyDone != 3 || done.Totals.Imported != 5 {
		t.Fatalf("resumed: %s %+v %s", done.Status, done.Totals, done.Message)
	}
	if d := e.src.Downloads.Load() - downloads; d != 5 {
		t.Fatalf("the resume downloaded %d artifacts, want 5", d)
	}
	if code, body := e.get("/repository/npm-h/demo"); code != 200 || !strings.Contains(body, `"beta"`) {
		t.Fatalf("npm tags after resume: %d %s", code, body)
	}
	// Without the password the stored task cannot run again.
	if err := e.a.Tasks.RunAsync(tasks.AdHocTaskID(tasks.TypeImportNexus), "root"); err != nil {
		t.Fatal(err)
	}
	e.a.Tasks.Wait()
	row, _ := e.a.Tasks.Get(context.Background(), tasks.AdHocTaskID(tasks.TypeImportNexus))
	if row.LastResult != tasks.ResultFailed || !strings.Contains(row.LastMessage, "password is not available") {
		t.Fatalf("run now without the password: %s %s", row.LastResult, row.LastMessage)
	}
	assertNoSecret(t, e)
}

// Name conflicts skip the repository; a different file at the same path
// fails the asset and never overwrites; renamePrefix imports beside them;
// failed assets are retried with retryFailed.
func TestImportNexusConflicts(t *testing.T) {
	e := newImportEnv(t, "  - { name: npm-h, format: maven, type: hosted }\n  - { name: mvn-h, format: maven, type: hosted }\n")
	// A different jar at the path the source holds.
	put, _ := http.NewRequest("PUT", e.ts.URL+"/repository/mvn-h/com/ex/app/1.0/app-1.0.jar", strings.NewReader("other"))
	put.SetBasicAuth("root", "pw")
	if resp, err := http.DefaultClient.Do(put); err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}
	job := e.start(map[string]any{"job": "conflicts"})
	rep := e.report(job)
	if rep.Status != importer.StatusFailed {
		t.Fatalf("status %s", rep.Status)
	}
	if rr := repoReport(t, rep, "npm-h"); rr.Definition != importer.DefConflict || rr.Content {
		t.Fatalf("npm-h: %+v", rr)
	}
	mvn := repoReport(t, rep, "mvn-h")
	if mvn.Definition != importer.DefExists || mvn.Counts.Failed != 1 || mvn.Counts.Imported != 1 {
		t.Fatalf("mvn-h: %+v", mvn)
	}
	var sawName, sawAsset bool
	for _, c := range rep.Conflicts {
		sawName = sawName || strings.Contains(c, `"npm-h" exists as maven hosted`)
		sawAsset = sawAsset || strings.Contains(c, "app-1.0.jar already exists with different content")
	}
	if !sawName || !sawAsset {
		t.Fatalf("conflicts %v", rep.Conflicts)
	}
	if _, body := e.get("/repository/mvn-h/com/ex/app/1.0/app-1.0.jar"); body != "other" {
		t.Fatalf("the jar was overwritten: %q", body)
	}
	// Re-run: the failure is not retried without retryFailed.
	e.start(map[string]any{"job": "conflicts"})
	if again := e.report(job); again.Totals.NotRetried != 1 || again.Totals.Failed != 0 {
		t.Fatalf("re-run: %+v", again.Totals)
	}
	e.start(map[string]any{"job": "conflicts", "retryFailed": true})
	if again := e.report(job); again.Totals.NotRetried != 0 || again.Totals.Failed != 1 {
		t.Fatalf("retryFailed: %+v", again.Totals)
	}
	// A prefix imports the clashing repository under another name.
	e.start(map[string]any{"job": "prefixed", "renamePrefix": "nx-", "repositories": []string{"npm-h"}})
	pre := e.report("prefixed")
	if pre.Status != importer.StatusOK || len(pre.Repositories) != 1 || pre.Repositories[0].Target != "nx-npm-h" || pre.Totals.Imported != 3 {
		t.Fatalf("prefixed: %s %+v", pre.Status, pre.Repositories)
	}
	if rp := e.a.Registry.Get("nx-npm-h"); rp == nil || rp.Format != "npm" {
		t.Fatalf("nx-npm-h: %+v", rp)
	}
	if !strings.Contains(strings.Join(pre.Warnings, " | "), `renamePrefix "nx-"`) {
		t.Fatalf("the renamePrefix rule is not in the report: %v", pre.Warnings)
	}
	// A dry run of the job with an earlier failure is a preview, not a
	// failure; a new job finds identical files in the target.
	e.start(map[string]any{"job": "conflicts", "dryRun": true})
	if dry := e.report("conflicts"); dry.Status != importer.StatusDryRun || dry.Totals.NotRetried != 1 {
		t.Fatalf("dry run of a job with failures: %s %+v", dry.Status, dry.Totals)
	}
	e.start(map[string]any{"job": "conflicts-dry", "dryRun": true})
	dry := e.report("conflicts-dry")
	// mvn-h pom, pypi 2, cargo 1 identical (npm-h is the conflict); the jar differs.
	if dry.Status != importer.StatusDryRun || dry.Totals.WouldSkip != 4 || dry.Totals.WouldImport != 1 {
		t.Fatalf("dry run of a new job: %s %+v", dry.Status, dry.Totals)
	}
	assertNoSecret(t, e)
}

// Who may call the import API, and what is refused.
func TestImportNexusAPIAccess(t *testing.T) {
	e := newImportEnv(t, "")
	// The test connection: interactive admin with the UI header only.
	code, res := e.call("POST", "/-/api/import/nexus/test", map[string]any{"url": e.src.URL, "username": srcUser, "password": srcPass}, true)
	repos, _ := res["repositories"].([]any)
	if code != 200 || len(repos) != 13 {
		t.Fatalf("test: %d %v", code, res)
	}
	flags := map[string]string{}
	for _, r := range repos {
		m := r.(map[string]any)
		flags[m["name"].(string)] = fmt.Sprintf("%v/%v", m["supported"], m["content"])
	}
	if flags["nuget-hosted"] != "false/false" || flags["mvn-h"] != "true/true" || flags["maven-central"] != "true/false" {
		t.Fatalf("flags %v", flags)
	}
	if code, _ := e.call("POST", "/-/api/import/nexus/test", map[string]any{"url": e.src.URL, "username": srcUser, "password": "wrong"}, true); code != http.StatusBadGateway {
		t.Fatalf("test with a wrong password: %d", code)
	}
	if code, _ := e.call("POST", "/-/api/import/nexus/test", map[string]any{"url": e.src.URL}, false); code != http.StatusForbidden {
		t.Fatalf("test with a token: %d", code)
	}
	if n := len(e.a.Registry.Snapshot().Sorted); n != 2 {
		t.Fatalf("the test call created repositories: %d", n)
	}
	// Start: a session or password login needs the UI header.
	req, _ := http.NewRequest("POST", e.ts.URL+"/-/api/import/nexus", strings.NewReader(`{"url":"`+e.src.URL+`","dryRun":true}`))
	req.SetBasicAuth("root", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("start without the UI header: %v %v", resp, err)
	}
	resp.Body.Close()
	// A non-admin cannot start or read.
	req, _ = http.NewRequest("GET", e.ts.URL+"/-/api/import/jobs", nil)
	req.SetBasicAuth("ci", "pw")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("jobs as non-admin: %v %v", resp, err)
	}
	// Validation.
	for _, bad := range []map[string]any{
		{"url": "ftp://x"}, {"url": e.src.URL, "concurrency": 17}, {"url": e.src.URL, "blobStore": "nope"},
		{"url": e.src.URL, "renamePrefix": "a/b"}, {"url": e.src.URL, "importDefinitions": false, "importContent": false},
	} {
		if code, res := e.call("POST", "/-/api/import/nexus", bad, false); code != http.StatusBadRequest {
			t.Errorf("%v: %d %v", bad, code, res)
		}
	}
	// An unknown field (the password must go in "password") is refused.
	if code, _ := e.call("POST", "/-/api/import/nexus", map[string]any{"url": e.src.URL, "passwd": "x"}, false); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
	if code, _ := e.call("GET", "/-/api/import/jobs/none", nil, false); code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", code)
	}
	// The task type cannot be created through the task API.
	if code, _ := e.call("POST", "/-/api/tasks", map[string]any{"name": "imp", "type": tasks.TypeImportNexus, "schedule": "manual",
		"params": map[string]any{"url": e.src.URL}}, true); code != http.StatusBadRequest {
		t.Fatalf("task create: %d", code)
	}
	// The UI may start with its header (dry run).
	if code, res := e.call("POST", "/-/api/import/nexus", e.body(map[string]any{"dryRun": true, "job": "ui"}), true); code != http.StatusAccepted || res["dryRun"] != true {
		t.Fatalf("UI start: %d %v", code, res)
	}
	e.a.Tasks.Wait()
	assertNoSecret(t, e)
}

// A download whose content does not match the source's checksum fails that
// asset (verified while streaming) and stores nothing for it.
func TestImportNexusChecksumMismatch(t *testing.T) {
	e := newImportEnv(t, "")
	e.src.Override("pypi-h", "packages/demo-pkg/1.0.0/demo_pkg-1.0.0.tar.gz", []byte("tampered"))
	job := e.start(map[string]any{"job": "sums", "repositories": []string{"pypi-h"}})
	rep := e.report(job)
	rr := repoReport(t, rep, "pypi-h")
	if rep.Status != importer.StatusFailed || rr.Counts.Failed != 1 || rr.Counts.Imported != 1 ||
		len(rr.Errors) != 1 || !strings.Contains(rr.Errors[0], "checksum mismatch") {
		t.Fatalf("%s %+v", rep.Status, rr)
	}
	if code, body := e.get("/repository/pypi-h/simple/demo-pkg/"); code != 200 || strings.Contains(body, "demo_pkg-1.0.0.tar.gz") {
		t.Fatalf("the tampered file was stored: %d %s", code, body)
	}
	assertNoSecret(t, e)
}

// Credentials revoked partway: the asset in flight and the repositories
// after it fail with the authentication error, which the report, the run
// log and the asset states carry, without the secret.
func TestImportNexusSourceAuthFailsMidRun(t *testing.T) {
	e := newImportEnv(t, "")
	e.src.FailAuthAfter = 2 // the crate and the jar, then 401
	job := e.start(map[string]any{"job": "revoked", "concurrency": 1})
	rep := e.report(job)
	if rep.Status != importer.StatusFailed || rep.Totals.Imported != 2 {
		t.Fatalf("%s %+v", rep.Status, rep.Totals)
	}
	mvn := repoReport(t, rep, "mvn-h")
	if mvn.Counts.Failed != 1 || len(mvn.Errors) == 0 || !strings.Contains(mvn.Errors[0], "authentication failed") {
		t.Fatalf("mvn-h: %+v", mvn)
	}
	if npmR := repoReport(t, rep, "npm-h"); len(npmR.Errors) == 0 || !strings.Contains(npmR.Errors[0], "authentication failed") {
		t.Fatalf("npm-h: %+v", npmR)
	}
	row, _ := e.a.Tasks.Get(context.Background(), tasks.AdHocTaskID(tasks.TypeImportNexus))
	if !strings.Contains(row.LastLog, "authentication failed") {
		t.Fatalf("run log: %s", row.LastLog)
	}
	states, _ := e.a.DB.ListImportState(context.Background(), job, 100)
	var failed int
	for _, st := range states {
		if st.Status == meta.ImportFailed {
			failed++
			if !strings.Contains(st.Error, "authentication failed") {
				t.Fatalf("state error: %+v", st)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("states: %+v", states)
	}
	assertNoSecret(t, e)
}

// Credentials embedded in a proxy's remote URL are stripped; the proxy is
// listed as needing credentials.
func TestImportNexusStripsRemoteCredentials(t *testing.T) {
	e := newImportEnv(t, "")
	e.src.Settings = func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"remoteUrl" : "https://repo1.maven.org/maven2/"`), []byte(`"remoteUrl" : "https://svc:hunter2@repo1.maven.org/maven2/"`), 1)
	}
	job := e.start(map[string]any{"job": "remote", "repositories": []string{"maven-central"}})
	rep := e.report(job)
	rp := e.a.Registry.Get("maven-central")
	if rep.Status != importer.StatusOK || rp == nil || rp.Remote != "https://repo1.maven.org/maven2/" {
		t.Fatalf("%s %+v", rep.Status, rp)
	}
	rr := repoReport(t, rep, "maven-central")
	if strings.Join(rep.ProxiesNeedingCredentials, ",") != "maven-central" ||
		!strings.Contains(strings.Join(rr.Notes, ";"), "credentials were embedded in the source URL and were not copied") {
		t.Fatalf("report: %v %v", rep.ProxiesNeedingCredentials, rr.Notes)
	}
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), "hunter2") || strings.Contains(e.logs.String(), "hunter2") {
		t.Fatal("the remote credentials leaked")
	}
}

// Downloads are capped: at the listed size plus 1 MiB, or at the upload
// limit when the source lists no size. Nothing is stored for them.
func TestImportNexusDownloadCap(t *testing.T) {
	e := newImportEnv(t, "")
	e.a.Tasks.Import.MaxUpload = func() int64 { return 1 << 20 } // maxUploadMiB 1
	big := bytes.Repeat([]byte("q"), 2<<20)
	e.src.Override("npm-h", "demo/-/demo-1.0.0.tgz", big)      // listed at 147 bytes: min(147 + 1 MiB, 1 MiB)
	e.src.Override("mvn-h", "com/ex/app/1.0/app-1.0.jar", big) // listed at 0: the upload limit applies
	job := e.start(map[string]any{"job": "cap", "repositories": []string{"npm-h", "mvn-h"}})
	rep := e.report(job)
	for _, name := range []string{"npm-h", "mvn-h"} {
		rr := repoReport(t, rep, name)
		if rr.Counts.Failed != 1 || !strings.Contains(strings.Join(rr.Errors, ";"), "larger than maxUploadMiB (1 MiB); raise maxUploadMiB to import it") {
			t.Fatalf("%s: %+v", name, rr)
		}
	}
	if code, _ := e.get("/repository/mvn-h/com/ex/app/1.0/app-1.0.jar"); code != 404 {
		t.Fatalf("the capped jar was stored: %d", code)
	}
	filepath.WalkDir(e.a.Config.DataDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), "meta.db") {
			if fi, _ := d.Info(); fi != nil && (fi.Size() >= 1<<20) {
				t.Errorf("a partial blob was left: %s (%d bytes)", p, fi.Size())
			}
		}
		return nil
	})
}

// GET /-/api/import/jobs/{job}?run=<id> answers about that run: a run that
// ended without a report gives its outcome, never an earlier report of the
// same job (what the CLI --wait and the UI poll).
func TestImportNexusJobByRun(t *testing.T) {
	e := newImportEnv(t, "")
	job := e.start(map[string]any{"job": "byrun", "repositories": []string{"npm-h"}})
	if rep := e.report(job); rep.Status != importer.StatusOK {
		t.Fatalf("first run: %s %s", rep.Status, rep.Message)
	}
	// A second run of the job without the password ends before any report.
	code, res := e.call("POST", "/-/api/import/nexus", map[string]any{"url": e.src.URL, "username": srcUser, "job": "byrun"}, false)
	if code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, res)
	}
	id := res["id"].(string)
	e.a.Tasks.Wait()
	code, res = e.call("GET", "/-/api/import/jobs/byrun?run="+id, nil, false)
	if code != 200 || res["status"] != importer.StatusFailed || res["noReport"] != true || !strings.Contains(fmt.Sprint(res["message"]), "password is not available") {
		t.Fatalf("by run: %d %v", code, res)
	}
	// Without the run id the stored report of the first run is served.
	if code, res = e.call("GET", "/-/api/import/jobs/byrun", nil, false); code != 200 || res["status"] != importer.StatusOK {
		t.Fatalf("by job: %d %v", code, res)
	}
	// A run with a report: the report.
	code, res = e.call("POST", "/-/api/import/nexus", e.body(map[string]any{"job": "byrun", "repositories": []string{"npm-h"}}), false)
	if code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, res)
	}
	id = res["id"].(string)
	e.a.Tasks.Wait()
	code, res = e.call("GET", "/-/api/import/jobs/byrun?run="+id, nil, false)
	if rep, _ := res["report"].(map[string]any); code != 200 || res["status"] != importer.StatusOK || rep == nil {
		t.Fatalf("by run with a report: %d %v", code, res)
	}
	assertNoSecret(t, e)
}

// I2: a re-run never moves what the target already has. A dist-tag the
// target moved stays where it is, and the report says the target value was
// kept.
func TestImportNexusKeepsTargetDistTags(t *testing.T) {
	e := newImportEnv(t, "")
	if rep := e.report(e.start(map[string]any{"job": "first", "repositories": []string{"npm-h"}})); rep.Status != importer.StatusOK {
		t.Fatalf("first: %s %+v", rep.Status, rep.Repositories)
	}
	put, _ := http.NewRequest("PUT", e.ts.URL+"/repository/npm-h/-/package/demo/dist-tags/latest", strings.NewReader(`"1.1.0-beta.1"`))
	put.SetBasicAuth("root", "pw")
	put.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(put); err != nil || resp.StatusCode/100 != 2 {
		t.Fatalf("dist-tag: %v %v", resp, err)
	}
	rep := e.report(e.start(map[string]any{"job": "second", "repositories": []string{"npm-h"}}))
	rr := repoReport(t, rep, "npm-h")
	if notes := strings.Join(rr.Notes, ";"); !strings.Contains(notes, "demo: dist-tag latest (target 1.1.0-beta.1, source 1.0.0): kept target value") {
		t.Fatalf("notes: %v", rr.Notes)
	}
	_, body := e.get("/repository/npm-h/demo")
	var pk struct {
		DistTags map[string]string `json:"dist-tags"`
	}
	if err := json.Unmarshal([]byte(body), &pk); err != nil || pk.DistTags["latest"] != "1.1.0-beta.1" || pk.DistTags["beta"] != "1.1.0-beta.1" {
		t.Fatalf("dist-tags: %v %v", pk.DistTags, err)
	}
}
