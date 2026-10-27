package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
	"commandcenter/internal/tasks"
)

func testConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(`
dataDir: ` + t.TempDir() + `
anonymousRead: true
users:
  - { name: ci, passwordHash: "` + string(h) + `", roles: [deployer] }
roles:
  deployer: { write: [maven-releases] }
repositories:
  - { name: maven-releases, format: maven, type: hosted }
  - { name: maven-public, format: maven, type: group, members: [maven-releases] }
` + extra))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	return cfg
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestEndToEndMaven(t *testing.T) {
	a, err := New(testConfig(t, ""), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()

	put, _ := http.NewRequest("PUT", ts.URL+"/repository/maven-releases/org/example/app/1.0/app-1.0.jar", strings.NewReader("jar"))
	put.SetBasicAuth("ci", "pw")
	resp, err := http.DefaultClient.Do(put)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}
	resp, err = http.Get(ts.URL + "/repository/maven-public/org/example/app/1.0/app-1.0.jar")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "jar" {
		t.Fatalf("GET via group: %d %q", resp.StatusCode, b)
	}
	resp, _ = http.Get(ts.URL + "/repository/maven-public/org/example/app/maven-metadata.xml")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "<version>1.0</version>") {
		t.Fatalf("metadata: %s", b)
	}
}

func TestCargoIsWired(t *testing.T) {
	a, err := New(testConfig(t, "  - { name: cargo-hosted, format: cargo, type: hosted }\n"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/repository/cargo-hosted/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cfg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil || resp.StatusCode != 200 || cfg["dl"] == nil {
		t.Fatalf("config.json: %d %v %v", resp.StatusCode, cfg, err)
	}
}

func TestNpmIsWired(t *testing.T) {
	a, err := New(testConfig(t, "  - { name: npm-hosted, format: npm, type: hosted }\n"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/repository/npm-hosted/-/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ping: %d", resp.StatusCode)
	}
}

func TestGC(t *testing.T) {
	a, err := New(testConfig(t, ""), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	a.Store.Put(ctx, "maven-releases", "keep", "", strings.NewReader("keep"))
	a.Store.Put(ctx, "maven-releases", "drop", "", strings.NewReader("drop"))
	a.Store.Delete(ctx, "maven-releases", "drop")

	if n, freed, err := a.GC(ctx, time.Hour); err != nil || n != 0 || freed != 0 {
		t.Fatalf("young blobs must be kept: n=%d freed=%d err=%v", n, freed, err)
	}
	n, freed, err := a.GC(ctx, 0)
	if err != nil || n != 1 || freed != int64(len("drop")) {
		t.Fatalf("n=%d freed=%d err=%v", n, freed, err)
	}
	if _, err := a.Store.Get(ctx, "maven-releases", "keep"); err != nil {
		t.Fatal(err)
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.Listen = "127.0.0.1:0"
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

// Docker sends SIGKILL 10 s after SIGTERM unless the compose file says
// otherwise, which cuts the drain short: uploads fail and leave temp files.
func TestComposeStopGracePeriod(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Services map[string]struct {
			StopGracePeriod string `yaml:"stop_grace_period"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	grace, err := time.ParseDuration(f.Services["commandcenter"].StopGracePeriod)
	if err != nil {
		t.Fatalf("stop_grace_period: %v", err)
	}
	if grace <= shutdownTimeout {
		t.Fatalf("stop_grace_period %v, want more than the %v drain", grace, shutdownTimeout)
	}
}

// Go's default MaxHeaderBytes (1 MiB) lets one connection that is still
// sending headers cost ~1.5 MiB, so maxConns of them would not fit the
// container: headers above maxHeaderBytes are refused with 431.
func TestHTTPServerBoundsHeaders(t *testing.T) {
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = newHTTPServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Start()
	defer ts.Close()
	for size, want := range map[int]int{8 << 10: http.StatusOK, 32 << 10: http.StatusRequestHeaderFieldsTooLarge} {
		req, _ := http.NewRequest("GET", ts.URL, nil)
		req.Header.Set("X-Pad", strings.Repeat("a", size))
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%d KiB of headers: %d, want %d", size>>10, resp.StatusCode, want)
		}
	}
}

// While all maxConns slots are busy, an HTTP probe waits like any client: a
// failing liveness probe would restart the pod and cut every transfer. The
// kernel completes a TCP probe without the server accepting it; readiness
// and startup still ask the HTTP server.
func TestKubernetesLivenessBypassesConnCap(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "deploy", "kubernetes", "commandcenter.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type probe struct {
		HTTPGet   *struct{ Path string } `yaml:"httpGet"`
		TCPSocket *struct{ Port string } `yaml:"tcpSocket"`
	}
	type container struct {
		Startup   probe `yaml:"startupProbe"`
		Readiness probe `yaml:"readinessProbe"`
		Liveness  probe `yaml:"livenessProbe"`
	}
	var cs []container
	for dec := yaml.NewDecoder(f); ; {
		var doc struct {
			Kind string
			Spec struct {
				Template struct {
					Spec struct{ Containers []container }
				}
			}
		}
		if err := dec.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if doc.Kind == "StatefulSet" {
			cs = append(cs, doc.Spec.Template.Spec.Containers...)
		}
	}
	if len(cs) != 1 {
		t.Fatalf("%d StatefulSet containers, want 1", len(cs))
	}
	c := cs[0]
	if c.Liveness.TCPSocket == nil || c.Liveness.TCPSocket.Port != "http" || c.Liveness.HTTPGet != nil {
		t.Errorf("livenessProbe %+v, want only tcpSocket on port http", c.Liveness)
	}
	for name, p := range map[string]probe{"startupProbe": c.Startup, "readinessProbe": c.Readiness} {
		if p.HTTPGet == nil || p.HTTPGet.Path != "/service/rest/v1/status" {
			t.Errorf("%s %+v, want httpGet /service/rest/v1/status", name, p)
		}
	}
}

func TestGCKeepsReuploadedBlob(t *testing.T) {
	a, err := New(testConfig(t, ""), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	if _, err := a.Store.Put(ctx, "maven-releases", "gone", "", strings.NewReader("same")); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Delete(ctx, "maven-releases", "gone"); err != nil {
		t.Fatal(err)
	}
	sums, err := a.Store.StoreByName(repo.DefaultStore).Write(strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(a.Config.DataDir, "blobs", "sha256", sums.SHA256[:2], sums.SHA256[2:4], sums.SHA256)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(blobPath, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.StoreByName(repo.DefaultStore).Write(strings.NewReader("same")); err != nil {
		t.Fatal(err)
	}
	n, _, err := a.GC(ctx, time.Hour)
	if err != nil || n != 0 {
		t.Fatalf("re-uploaded blob must survive GC: n=%d err=%v", n, err)
	}
}

func TestPypiIsWired(t *testing.T) {
	a, err := New(testConfig(t, "  - { name: pypi-hosted, format: pypi, type: hosted }\n"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/repository/pypi-hosted/simple/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("simple root: %d", resp.StatusCode)
	}
}

func TestGoIsWired(t *testing.T) {
	a, err := New(testConfig(t, "  - { name: go-hosted, format: go, type: hosted }\n"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/repository/go-hosted/example.com/x/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown module list: %d", resp.StatusCode)
	}
}

func TestRepositoriesImportedOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write := func(repos string) *config.Config {
		t.Helper()
		yaml := "dataDir: " + dir + "\nanonymousRead: true\nroles:\n  deployer: { write: [maven-releases, gone] }\nrepositories:\n" + repos
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Database.URL = metatest.URL(t, cfg.DataDir)
		return cfg
	}
	start := func(cfg *config.Config) (*App, string) {
		t.Helper()
		var buf strings.Builder
		a, err := New(cfg, slog.New(slog.NewTextHandler(&buf, nil)))
		if err != nil {
			t.Fatal(err)
		}
		return a, buf.String()
	}

	a, logs := start(write("  - { name: maven-releases, format: maven, type: hosted }\n"))
	if strings.Contains(logs, "ignored") {
		t.Fatalf("first start warned: %s", logs)
	}
	if !strings.Contains(logs, "gone") {
		t.Fatalf("no warning for a role naming a missing repository: %s", logs)
	}
	if err := a.Registry.Create(context.Background(), config.Repository{Name: "npm-hosted", Format: "npm", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	resp, err := http.Get(ts.URL + "/repository/npm-hosted/-/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ts.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("created repository not served: %d", resp.StatusCode)
	}
	a.Close()

	// The second start has a different repositories section; the DB wins.
	a, logs = start(write("  - { name: other, format: go, type: hosted }\n"))
	defer a.Close()
	if !strings.Contains(logs, "ignored") || !strings.Contains(logs, path) {
		t.Fatalf("second start did not warn about the ignored section in %s: %s", path, logs)
	}
	s := a.Registry.Snapshot()
	if len(s.Sorted) != 2 || s.ByName["maven-releases"] == nil || s.ByName["npm-hosted"] == nil || s.ByName["other"] != nil {
		t.Fatalf("repositories after restart = %v", s.ByName)
	}
}

func TestInvalidRepositoriesSectionAfterImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	load := func(repos string) *config.Config {
		t.Helper()
		if err := os.WriteFile(path, []byte("dataDir: "+dir+"\nrepositories:\n"+repos), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load must not fail on the repositories section: %v", err)
		}
		cfg.Database.URL = metatest.URL(t, cfg.DataDir)
		return cfg
	}
	const invalid = "  - { name: x, format: nuget, type: hosted }\n"

	// Before the import, an invalid section still blocks startup.
	if _, err := New(load(invalid), quiet()); err == nil || !strings.Contains(err.Error(), `unknown format "nuget"`) {
		t.Fatalf("fresh start with an invalid section: %v", err)
	}

	a, err := New(load("  - { name: maven-releases, format: maven, type: hosted }\n"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	a.Close()

	// After the import, it is ignored with the warning only.
	var buf strings.Builder
	a, err = New(load(invalid+"  - { name: y, format: maven, type: hosted, bogus: 1 }\n"), slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("start after import with an invalid section: %v", err)
	}
	defer a.Close()
	if !strings.Contains(buf.String(), "ignored") || a.Registry.Get("maven-releases") == nil || a.Registry.Get("x") != nil {
		t.Fatalf("logs=%s repos=%v", buf.String(), a.Registry.Snapshot().ByName)
	}
}

func TestStartupWithStoredRepositoryNamedCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("dataDir: "+dir+"\nrepositories:\n  - { name: create, format: maven, type: hosted }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	for i := 0; i < 2; i++ { // the first start imports it, the second loads the stored one
		a, err := New(cfg, quiet())
		if err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
		if a.Registry.Get("create") == nil {
			t.Fatalf("start %d: repository create missing", i+1)
		}
		a.Close()
	}
}

func TestMetricsAreBatchedAndFlushedOnShutdown(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.Listen = "127.0.0.1:0"
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()

	const p = "org/example/app/1.0/app-1.0.jar"
	put, _ := http.NewRequest("PUT", ts.URL+"/repository/maven-releases/"+p, strings.NewReader("jar"))
	put.SetBasicAuth("ci", "pw")
	if resp, err := http.DefaultClient.Do(put); err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}
	for _, m := range []string{"GET", "GET", "HEAD"} {
		req, _ := http.NewRequest(m, ts.URL+"/repository/maven-public/"+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", m, resp, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	http.Get(ts.URL + "/repository/no-such-repo/x") // unknown names are not recorded

	ctx := context.Background()
	got, _ := a.DB.GetAsset(ctx, "maven-releases", p)
	if got.Downloads != 0 {
		t.Fatalf("DB written before a flush: %d", got.Downloads)
	}

	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Serve(sctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	// The final flush completes before Serve returns; poll with a deadline anyway.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ = a.DB.GetAsset(ctx, "maven-releases", p)
		if got.Downloads == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Downloads != 2 || got.LastDownloaded.IsZero() {
		t.Fatalf("after shutdown: downloads %d last %v, want 2 (HEAD not counted)", got.Downloads, got.LastDownloaded)
	}
	// Sum the buckets: a run across UTC midnight may split them.
	buckets, err := a.DB.RequestStats(ctx, 0, time.Now().Add(48*time.Hour).Unix(), 86400, "")
	var total int64
	for _, b := range buckets {
		total += b.Requests
	}
	if err != nil || total != 4 {
		t.Fatalf("requests %+v %v, want total 4 (PUT, 2 GET, HEAD)", buckets, err)
	}
	if other, _ := a.DB.RequestStats(ctx, 0, time.Now().Add(time.Hour).Unix(), 86400, "no-such-repo"); len(other) != 0 {
		t.Fatalf("unknown repository recorded: %+v", other)
	}
}

func TestTasksWired(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.Listen = "127.0.0.1:0"
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seed, err := a.Tasks.Get(ctx, tasks.SeedID)
	if err != nil || seed.Type != tasks.TypeCompact || seed.Schedule != "daily:03:00" || seed.NextRun.IsZero() {
		t.Fatalf("seed: %+v %v", seed, err)
	}
	// Serve runs the scheduler: a tick runs the due seed task.
	ticks := make(chan time.Time)
	a.Tasks.Ticks = ticks
	a.Tasks.Now = func() time.Time { return seed.NextRun.Add(time.Minute) }
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Serve(sctx) }()
	ticks <- time.Time{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := a.Tasks.Get(ctx, tasks.SeedID)
		if err == nil && got.LastResult == tasks.ResultOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scheduler did not run the seed task: %+v %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Deleted (the API refuses; QA L-3), the seed does not come back on the next start.
	if err := a.Tasks.DB.DeleteTask(ctx, tasks.SeedID); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if list, err := b.Tasks.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("tasks after restart: %+v %v", list, err)
	}
}
