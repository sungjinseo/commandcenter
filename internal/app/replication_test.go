package app

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/tasks"
)

// Pull replication: a source and a target instance, each on its own
// database, with their own HTTP servers.

// startApp is startNode without the cache loop, returning the server so a
// test can shut the instance's HTTP side down early. It drops the hop
// header, which names this process (proxy.InstanceID) for both instances:
// across processes the source would not find itself in it.
func startApp(t *testing.T, cfg *config.Config) (*node, *httptest.Server) {
	t.Helper()
	var h http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(proxy.HopHeader)
		h.ServeHTTP(w, r)
	}))
	cfg.BaseURL = "http://" + ts.Listener.Addr().String() // the URLs cargo's dl and terraform's downloads name
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	h = a.Handler
	ts.Start()
	t.Cleanup(func() {
		ts.Close()
		a.Tasks.Stop()
		a.Close()
	})
	return &node{a, ts.URL}, ts
}

// listedAsset is an entry of the Nexus REST API's asset list.
type listedAsset struct {
	Path        string `json:"path"`
	DownloadURL string `json:"downloadUrl"`
}

// sourceAssets lists the assets of repository name on n through the Nexus
// REST API, following the continuation tokens.
func sourceAssets(t *testing.T, n *node, name string) []listedAsset {
	t.Helper()
	var out []listedAsset
	for token := ""; ; {
		q := url.Values{"repository": {name}}
		if token != "" {
			q.Set("continuationToken", token)
		}
		var page struct {
			Items             []listedAsset `json:"items"`
			ContinuationToken string        `json:"continuationToken"`
		}
		if err := json.Unmarshal([]byte(n.want(t, "GET", "/service/rest/v1/assets?"+q.Encode(), nil, ok(200))), &page); err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Items...)
		if token = page.ContinuationToken; token == "" {
			return out
		}
	}
}

// replicate runs a repository.pull-replication task on n for repository
// ("" for every replicating proxy) and returns the outcome and its log.
func replicate(t *testing.T, n *node, repository string, full bool) (tasks.Outcome, string) {
	t.Helper()
	ctx := context.Background()
	params, _ := json.Marshal(map[string]any{"repository": repository, "full": full})
	tk, err := n.Tasks.Create(ctx, tasks.Def{Name: "replicate " + repository, Type: tasks.TypePullReplication, Schedule: tasks.Manual, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	out, err := n.Tasks.RunNow(tk.ID, "root")
	if err != nil {
		t.Fatal(err)
	}
	got, err := n.Tasks.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out, got.LastLog
}

// Every format with pull replication: what is uploaded to the source's
// hosted repository is fetched by the target's replicating proxy in one
// run, and served by the target once the source is gone.
func TestPullReplicationFormats(t *testing.T) {
	var formats []hostedFormat
	covered := map[string]bool{}
	for _, f := range hostedFormats() {
		if format := strings.TrimPrefix(f.def, "format: "); config.ReplicationFormats[format] {
			formats = append(formats, f)
			covered[format] = true
		}
	}
	for format := range config.ReplicationFormats {
		if !covered[format] {
			t.Errorf("%s has pull replication but no hosted upload in hostedFormats", format)
		}
	}
	var hosted strings.Builder
	for _, f := range formats {
		fmt.Fprintf(&hosted, "  - { name: %s, %s, type: hosted, writePolicy: allow }\n", f.repo, f.def)
	}
	src, srcTS := startApp(t, clusterConfig(t, hosted.String()))
	var proxies strings.Builder
	for _, f := range formats {
		fmt.Fprintf(&proxies, "  - { name: %s-p, %s, type: proxy, remote: %q, replication: { preemptivePullEnabled: true, verifyChecksums: true } }\n",
			f.repo, f.def, src.url+"/repository/"+f.repo+"/")
	}
	tgt, _ := startApp(t, clusterConfig(t, proxies.String()))
	for _, f := range formats {
		f.put(t, src, 1)
	}
	want := map[string]map[string]string{} // repository -> the proxy's client path of a component file -> body
	for _, f := range formats {
		format := strings.TrimPrefix(f.def, "format: ")
		want[f.repo] = map[string]string{}
		for _, a := range sourceAssets(t, src, f.repo) {
			if _, _, _, isFile := component.DeriveAsset(format, a.Path); isFile {
				hosted, proxied := clientPaths(format, a)
				want[f.repo][proxied] = src.want(t, "GET", "/repository/"+f.repo+"/"+hosted, nil, ok(200))
			}
		}
		if len(want[f.repo]) == 0 {
			t.Errorf("%s: the source lists no component file", f.repo)
		}
	}
	out, log := replicate(t, tgt, "", false)
	if out.Result != tasks.ResultOK {
		t.Fatalf("replication: %s %s\n%s", out.Result, out.Message, log)
	}
	srcTS.Close()
	for _, f := range formats {
		for p, body := range want[f.repo] {
			if got := tgt.want(t, "GET", "/repository/"+f.repo+"-p/"+p, nil, ok(200)); got != body {
				t.Errorf("%s-p/%s: served %d bytes, the source had %d", f.repo, p, len(got), len(body))
			}
		}
	}
	// What a composer client reads first names the replicated archive.
	for p := range want["composer-h"] {
		if doc := tgt.want(t, "GET", "/repository/composer-h-p/p2/acme/demo.json", nil, ok(200)); !strings.Contains(doc, `/repository/composer-h-p/`+p) {
			t.Errorf("composer metadata does not name %s: %s", p, doc)
		}
	}
	if t.Failed() {
		t.Logf("run log:\n%s", log)
	}
}

// clientPaths are where a client GETs hosted asset a of format from the
// hosted repository and from a proxy of it, where the format's URLs are not
// its storage paths.
func clientPaths(format string, a listedAsset) (hosted, proxied string) {
	switch p := a.Path; format {
	case "nuget":
		return "v3-flatcontainer/" + p, "v3-flatcontainer/" + p
	case "swift":
		dir := strings.TrimSuffix(p, "/source-archive.zip")
		return dir + ".zip", dir + ".zip"
	case "composer": // the proxy's dist path ends in the sha1 of the source's dist URL
		sum := sha1.Sum([]byte(a.DownloadURL))
		return p, strings.TrimSuffix(p, ".zip") + "/" + hex.EncodeToString(sum[:]) + ".zip"
	}
	return a.Path, a.Path
}

// One maven proxy in depth: incremental runs, a miss recorded before the
// upload, the include regex, bad credentials, the test endpoint, the APIs'
// fields and the formats refused.
func TestPullReplication(t *testing.T) {
	src, srcTS := startApp(t, clusterConfig(t, ""))
	remote := src.url + "/repository/maven-releases/"
	tgt, _ := startApp(t, clusterConfig(t, fmt.Sprintf(`
  - { name: mirror, format: maven, type: proxy, remote: %q, replication: { preemptivePullEnabled: true } }
  - { name: mirror-jar, format: maven, type: proxy, remote: %q, replication: { preemptivePullEnabled: true, assetPathRegex: '\.jar$' } }
  - { name: mirror-bad, format: maven, type: proxy, remote: %q, remoteAuth: { username: replicator, password: wrong }, replication: { preemptivePullEnabled: true } }
`, remote, remote, remote)))
	tgt.Tasks.Replication.Slack = time.Nanosecond
	ctx := context.Background()
	put := func(p, body string) { src.want(t, "PUT", "/repository/maven-releases/"+p, body, ok(201)) }
	put("org/ex/lib/1.0/lib-1.0.jar", "jar 1.0")
	pom := "<project><modelVersion>4.0.0</modelVersion><groupId>org.ex</groupId><artifactId>lib</artifactId><version>1.0</version></project>"
	put("org/ex/lib/1.0/lib-1.0.pom", pom)
	// A miss of a version not uploaded yet is negative-cached by the proxy.
	tgt.want(t, "GET", "/repository/mirror/org/ex/lib/2.0/lib-2.0.jar", nil, ok(404))

	out, log := replicate(t, tgt, "mirror", false)
	if out.Result != tasks.ResultOK || !strings.Contains(out.Message, "fetched 2 assets") {
		t.Fatalf("first run: %s %s\n%s", out.Result, out.Message, log)
	}
	var st struct {
		LastModified time.Time `json:"lastModified"`
	}
	if found, err := tgt.DB.GetSettingJSON(ctx, "replication/mirror", &st); !found || err != nil || st.LastModified.IsZero() {
		t.Fatalf("progress: %v %v %+v", found, err, st)
	}
	time.Sleep(1100 * time.Millisecond) // the source's lastModified has a precision of seconds
	put("org/ex/lib/2.0/lib-2.0.jar", "jar 2.0")
	out, log = replicate(t, tgt, "mirror", false)
	if out.Result != tasks.ResultOK || !strings.Contains(out.Message, "fetched 1 assets") {
		t.Fatalf("incremental run: %s %s\n%s", out.Result, out.Message, log)
	}
	// Older than the newest asset the last run listed: not even looked up.
	out, log = replicate(t, tgt, "mirror", false)
	if out.Result != tasks.ResultOK || !strings.Contains(out.Message, "fetched 0 assets") || !strings.Contains(log, "1 held already, 2 unchanged since the last run") {
		t.Fatalf("run without news: %s %s\n%s", out.Result, out.Message, log)
	}
	out, log = replicate(t, tgt, "mirror", true)
	if out.Result != tasks.ResultOK || !strings.Contains(out.Message, "fetched 0 assets") || !strings.Contains(log, "3 held already") {
		t.Fatalf("full run: %s %s\n%s", out.Result, out.Message, log)
	}

	out, log = replicate(t, tgt, "mirror-jar", false)
	if out.Result != tasks.ResultOK || !strings.Contains(out.Message, "fetched 2 assets") {
		t.Fatalf("regex run: %s %s\n%s", out.Result, out.Message, log)
	}
	if _, err := tgt.DB.GetAsset(ctx, "mirror-jar", "org/ex/lib/1.0/lib-1.0.pom"); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("the pom was replicated past the regex: %v", err)
	}

	out, log = replicate(t, tgt, "mirror-bad", false)
	if out.Result != tasks.ResultFailed || !strings.Contains(log, "refused the remote credentials (HTTP 401)") {
		t.Fatalf("bad credentials: %s %s\n%s", out.Result, out.Message, log)
	}
	var check struct {
		OK         bool   `json:"ok"`
		Repository string `json:"repository"`
		Message    string `json:"message"`
	}
	json.Unmarshal([]byte(tgt.want(t, "POST", "/-/api/repositories/mirror-bad/replication/test", nil, ok(200))), &check)
	if check.OK || !strings.Contains(check.Message, "401") {
		t.Errorf("test with bad credentials: %+v", check)
	}
	json.Unmarshal([]byte(tgt.want(t, "POST", "/-/api/repositories/mirror/replication/test", nil, ok(200))), &check)
	if !check.OK || check.Repository != "maven-releases" {
		t.Errorf("test: %+v", check)
	}

	// The APIs show the settings, never the credentials; an update without
	// replication keeps it.
	cfg := tgt.want(t, "GET", "/-/api/repositories/mirror-bad/config", nil, ok(200))
	if !strings.Contains(cfg, `"replication":{"preemptivePullEnabled":true}`) || !strings.Contains(cfg, `"passwordSet":true`) ||
		strings.Contains(cfg, "wrong") || !strings.Contains(cfg, `"replicationSource":{"url":"`+src.url+`","repository":"maven-releases"}`) {
		t.Errorf("config: %s", cfg)
	}
	tgt.want(t, "PUT", "/-/api/repositories/mirror-jar", `{"remote":"`+remote+`"}`, ok(200))
	nx := tgt.want(t, "GET", "/service/rest/v1/repositories/maven/proxy/mirror-jar", nil, ok(200))
	if !strings.Contains(nx, `"replication":{"assetPathRegex":"\\.jar$","preemptivePullEnabled":true,"verifyChecksums":false}`) {
		t.Errorf("nexus settings: %s", nx)
	}

	// Refused: a format without it, a remote that names no hosted
	// repository, on both APIs.
	for _, body := range []string{
		`{"name":"d","format":"docker","type":"proxy","remote":"https://registry-1.docker.io/","replication":{"preemptivePullEnabled":true}}`,
		`{"name":"m","format":"maven","type":"proxy","remote":"https://repo1.maven.org/maven2/","replication":{"preemptivePullEnabled":true}}`,
		`{"name":"h","format":"maven","type":"hosted","replication":{"preemptivePullEnabled":true}}`,
	} {
		if st, b, _ := tgt.do(t, "root", "POST", "/-/api/repositories", body); st != 400 || !strings.Contains(b, "replication") {
			t.Errorf("%s: %d %s", body, st, b)
		}
	}
	if st, b, _ := tgt.do(t, "root", "POST", "/service/rest/v1/repositories/docker/proxy",
		`{"name":"d","online":true,"storage":{"blobStoreName":"default"},"proxy":{"remoteUrl":"https://registry-1.docker.io/"},"replication":{"preemptivePullEnabled":true}}`,
		"Content-Type", "application/json"); st != 400 || !strings.Contains(b, "not supported for docker") {
		t.Errorf("nexus docker proxy: %d %s", st, b)
	}

	// With the source gone, the target serves what it replicated.
	srcTS.Close()
	for p, body := range map[string]string{"org/ex/lib/1.0/lib-1.0.jar": "jar 1.0", "org/ex/lib/1.0/lib-1.0.pom": pom, "org/ex/lib/2.0/lib-2.0.jar": "jar 2.0"} {
		if got := tgt.want(t, "GET", "/repository/mirror/"+p, nil, ok(200)); got != body {
			t.Errorf("%s: %q", p, got)
		}
	}
}
