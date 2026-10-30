package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/webhook"
)

// usageApp is a server with an admin "root", the test config's "ci" and a
// "viewer" holding only the metrics read privilege.
func usageApp(t *testing.T, repos string) (*App, *httptest.Server) {
	t.Helper()
	cfg := testConfig(t, repos)
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = append(cfg.Users, config.User{Name: "root", PasswordHash: string(h), Roles: []string{"admin"}},
		config.User{Name: "viewer", PasswordHash: string(h), Roles: []string{"usage"}})
	cfg.Roles["usage"] = config.Role{}
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	if st, body := do(t, "PUT", ts.URL+"/-/api/roles/usage", "root", `{"privileges":[{"target":"metrics","actions":["read"]}]}`, true); st != 200 {
		t.Fatalf("role: %d %s", st, body)
	}
	return a, ts
}

func jsonDo(t *testing.T, method, url, user, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.SetBasicAuth(user, "pw")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func pomOf(g, a, v string) string {
	return "<project><groupId>" + g + "</groupId><artifactId>" + a + "</artifactId><version>" + v + "</version></project>"
}

func mustDo(t *testing.T, want int, method, url, user, body string, ui bool) string {
	t.Helper()
	st, out := do(t, method, url, user, body, ui)
	if st != want {
		t.Fatalf("%s %s: %d %s, want %d", method, url, st, out, want)
	}
	return out
}

// Repository evaluation end to end: hosted npm and Maven components found
// vulnerable through a fake OSV API, a repository that opts out skipped,
// the incremental re-run, an advisory import re-evaluating everything, the
// override turning one on while the setting is off, the dashboard and
// drill-down, the events and the webhook.
func TestRepositoryEvaluation(t *testing.T) {
	var queries atomic.Int64
	api := fakeOSVAPI(map[string][]string{
		"npm|lodash|4.17.20": {"GHSA-crit"},
		"Maven|org.apache.logging.log4j:log4j-core|2.14.1": {"GHSA-jfh8-c2jp-5v3q"},
	}, map[string]string{
		"GHSA-crit": `{"id":"GHSA-crit","summary":"Prototype pollution","severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}],"affected":[{"package":{"ecosystem":"npm","name":"lodash"},"ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"4.17.21"}]}]}]}`,
		"GHSA-jfh8-c2jp-5v3q": `{"id":"GHSA-jfh8-c2jp-5v3q","aliases":["CVE-2021-44228"],"summary":"Remote code injection in Log4j",
			"severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H"}],
			"affected":[{"package":{"ecosystem":"Maven","name":"org.apache.logging.log4j:log4j-core"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"2.0-beta9"},{"fixed":"2.15.0"}]}]}]}`,
	})
	osv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/querybatch" {
			queries.Add(1)
		}
		api.ServeHTTP(w, r)
	}))
	defer osv.Close()
	var hookMu sync.Mutex
	var hooks []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hookMu.Lock()
		hooks = append(hooks, string(b))
		hookMu.Unlock()
	}))
	defer receiver.Close()
	poll := webhook.PollEvery
	webhook.PollEvery = 20 * time.Millisecond
	defer func() { webhook.PollEvery = poll }()

	a, ts := usageApp(t, `
  - { name: npm-hosted, format: npm, type: hosted }
  - { name: npm-skip, format: npm, type: hosted, evaluation: disabled }
  - { name: raw-hosted, format: raw, type: hosted }
`)
	a.Webhooks.Deliver()
	mustDo(t, 200, "PUT", ts.URL+"/-/api/vulnerability/settings", "root", `{"enabled":true,"mode":"online","apiURL":"`+osv.URL+`","evaluateHosted":true}`, true)
	mustDo(t, 201, "POST", ts.URL+"/-/api/webhooks", "root", `{"name":"vuln","url":"`+receiver.URL+`/hook","events":["component.vulnerable"]}`, true)
	for _, c := range [][2]string{{"npm-hosted", "lodash@4.17.20"}, {"npm-hosted", "lodash@4.17.21"}, {"npm-skip", "lodash@4.17.20"}} {
		name, version, _ := strings.Cut(c[1], "@")
		if st, body := jsonDo(t, "PUT", ts.URL+"/repository/"+c[0]+"/"+name, "root", npmPublish(name, version)); st != 200 && st != 201 {
			t.Fatalf("publish %v: %d %s", c, st, body)
		}
	}
	const jar = "/repository/maven-releases/org/apache/logging/log4j/log4j-core/2.14.1/log4j-core-2.14.1"
	mustDo(t, 201, "PUT", ts.URL+jar+".jar", "ci", "jar", false)
	mustDo(t, 201, "PUT", ts.URL+jar+".pom", "ci", pomOf("org.apache.logging.log4j", "log4j-core", "2.14.1"), false)
	mustDo(t, 201, "PUT", ts.URL+"/repository/raw-hosted/notes.txt", "root", "x", false)

	body := mustDo(t, 201, "POST", ts.URL+"/-/api/tasks", "root", `{"name":"evaluate","type":"repository.evaluate","schedule":"manual"}`, true)
	var task struct{ ID string }
	json.Unmarshal([]byte(body), &task)
	run := func(want string) {
		t.Helper()
		out, err := a.Tasks.RunNow(task.ID, "root")
		if err != nil || out.Result != "OK" || !strings.Contains(out.Message, want) {
			t.Fatalf("run: %+v %v, want %q", out, err, want)
		}
	}

	// The first run: the two hosted repositories of a format with OSV data;
	// npm-skip opts out, raw has no OSV ecosystem.
	run("evaluated 2 repositories: 3 components, 3 looked up, 2 vulnerable, 2 with new advisories, 0 not checked online")
	calls := queries.Load()
	body = mustDo(t, 200, "GET", ts.URL+"/-/api/dashboard/repositories?sort=vulnerable", "viewer", "", false)
	var dash struct {
		Total int
		Items []struct {
			Name       string
			Components int64
			Evaluation struct {
				Covered, Monitored     bool
				Setting, LastEvaluated string
				Components, Vulnerable int64
				BySeverity             struct{ Critical int64 }
			}
		}
		UncoveredFormats []string
	}
	json.Unmarshal([]byte(body), &dash)
	if dash.Total != 5 || dash.Items[0].Name != "maven-releases" || dash.Items[1].Name != "npm-hosted" {
		t.Fatalf("dashboard order: %s", body)
	}
	for _, it := range dash.Items {
		e := it.Evaluation
		switch it.Name {
		case "maven-releases", "npm-hosted":
			if !e.Monitored || e.LastEvaluated == "" || e.Vulnerable != 1 || e.BySeverity.Critical != 1 || e.Components != it.Components {
				t.Fatalf("%s: %+v", it.Name, it)
			}
		case "npm-skip":
			if e.Monitored || e.Setting != "disabled" || e.LastEvaluated != "" || !e.Covered {
				t.Fatalf("%s: %+v", it.Name, it)
			}
		case "raw-hosted", "maven-public":
			if e.Covered || e.Monitored {
				t.Fatalf("%s: %+v", it.Name, it)
			}
		}
	}
	if !strings.Contains(body, `"uncoveredFormats":["raw"]`) {
		t.Fatalf("uncovered: %s", body)
	}
	body = mustDo(t, 200, "GET", ts.URL+"/-/api/dashboard/repositories/npm-hosted/vulnerabilities", "viewer", "", false)
	for _, want := range []string{`"total":1`, `"component":"lodash@4.17.20"`, `"severity":"critical"`, `"id":"GHSA-crit"`, `"fixedIn":["4.17.21"]`,
		`"url":"https://osv.dev/vulnerability/GHSA-crit"`, `"monitored":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("drill-down lacks %s: %s", want, body)
		}
	}
	body = mustDo(t, 200, "GET", ts.URL+"/-/api/dashboard/repositories/maven-releases/vulnerabilities?severity=critical", "viewer", "", false)
	if !strings.Contains(body, `"component":"org.apache.logging.log4j:log4j-core:2.14.1"`) || !strings.Contains(body, `"aliases":["CVE-2021-44228"]`) {
		t.Fatalf("maven drill-down: %s", body)
	}

	// Nothing changed: nothing is looked up again, the API is not asked.
	run("evaluated 2 repositories: 3 components, 0 looked up, 2 vulnerable, 0 with new advisories")
	// A new component alone is looked up.
	if st, body := jsonDo(t, "PUT", ts.URL+"/repository/npm-hosted/left-pad", "root", npmPublish("left-pad", "1.0.0")); st != 201 && st != 200 {
		t.Fatalf("publish: %d %s", st, body)
	}
	run("evaluated 2 repositories: 4 components, 1 looked up, 2 vulnerable, 0 with new advisories")
	if q := queries.Load(); q != calls+1 {
		t.Fatalf("OSV querybatch calls: %d after the first run, %d now; want one more", calls, q)
	}
	// New advisory data (an import) re-evaluates every component, from the
	// local data and the cached answers.
	mal := osvZipOf(t, "MAL-2026-0001.json", `{"id":"MAL-2026-0001","summary":"Malicious code in left-pad","affected":[{"package":{"ecosystem":"npm","name":"left-pad"},"versions":["1.0.0"]}]}`)
	mustDo(t, 200, "POST", ts.URL+"/-/api/vulnerability/import", "root", string(mal), true)
	run("evaluated 2 repositories: 4 components, 4 looked up, 3 vulnerable, 1 with new advisories")
	body = mustDo(t, 200, "GET", ts.URL+"/-/api/dashboard/repositories/npm-hosted/vulnerabilities", "viewer", "", false)
	if !strings.Contains(body, `"total":2`) || strings.Index(body, `"MAL-2026-0001"`) > strings.Index(body, `"GHSA-crit"`) {
		t.Fatalf("malicious first: %s", body)
	}

	// The setting off, an override on: npm-skip alone is evaluated, the others'
	// results go.
	mustDo(t, 200, "PUT", ts.URL+"/-/api/vulnerability/settings", "root", `{"evaluateHosted":false}`, true)
	mustDo(t, 200, "PUT", ts.URL+"/-/api/repositories/npm-skip", "root", `{"evaluation":"enabled"}`, true)
	if body := mustDo(t, 200, "GET", ts.URL+"/-/api/repositories/npm-skip/config", "root", "", false); !strings.Contains(body, `"evaluation":"enabled"`) {
		t.Fatalf("config: %s", body)
	}
	mustDo(t, 200, "PUT", ts.URL+"/-/api/repositories/npm-skip", "root", `{"writePolicy":"allow"}`, true) // an update without it keeps it
	run("evaluated 1 repositories: 1 components, 1 looked up, 1 vulnerable, 1 with new advisories")
	body = mustDo(t, 200, "GET", ts.URL+"/-/api/dashboard/repositories?q=NPM&vulnerable=true", "viewer", "", false)
	if !strings.Contains(body, `"total":1`) || !strings.Contains(body, `"name":"npm-skip"`) {
		t.Fatalf("filtered: %s", body)
	}
	if body := mustDo(t, 200, "GET", ts.URL+"/-/api/dashboard/repositories/npm-hosted/vulnerabilities", "viewer", "", false); !strings.Contains(body, `"total":0`) {
		t.Fatalf("cleared: %s", body)
	}
	if st, body := do(t, "PUT", ts.URL+"/-/api/repositories/raw-hosted", "root", `{"evaluation":"enabled"}`, true); st != 400 || !strings.Contains(body, "no vulnerability data") {
		t.Fatalf("raw override: %d %s", st, body)
	}

	// Events and webhooks: one per component with new advisories.
	var events string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		_, events = do(t, "GET", ts.URL+"/-/api/events?kind=component.vulnerable", "root", "", false)
		hookMu.Lock()
		n := len(hooks)
		hookMu.Unlock()
		if strings.Count(events, `"kind":"component.vulnerable"`) == 4 && n == 4 {
			break
		}
	}
	for _, want := range []string{`"repo":"npm-hosted","subject":"lodash@4.17.20","detail":"critical:GHSA-crit"`,
		`"repo":"maven-releases","subject":"org.apache.logging.log4j:log4j-core:2.14.1","detail":"critical:GHSA-jfh8-c2jp-5v3q"`,
		`"repo":"npm-hosted","subject":"left-pad@1.0.0","detail":"unknown:MAL-2026-0001"`, `"repo":"npm-skip","subject":"lodash@4.17.20"`} {
		if !strings.Contains(events, want) {
			t.Fatalf("events lack %s: %s", want, events)
		}
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	all := strings.Join(hooks, "\n")
	if len(hooks) != 4 || !strings.Contains(all, `"event":"component.vulnerable"`) || !strings.Contains(all, `"ids":["MAL-2026-0001"]`) ||
		!strings.Contains(all, `"group":"org.apache.logging.log4j"`) || !strings.Contains(all, `"severity":"critical"`) {
		t.Fatalf("webhooks: %s", all)
	}
}

// The dashboard's numbers follow uploads, deletes and downloads exactly;
// filters, sorting, paging and the privilege; the Nexus API's size.
func TestRepositoryDashboard(t *testing.T) {
	a, ts := usageApp(t, "  - { name: raw-hosted, format: raw, type: hosted, writePolicy: allow }\n")
	put := func(repo, path, content string) {
		mustDo(t, 201, "PUT", ts.URL+"/repository/"+repo+"/"+path, "root", content, false)
	}
	put("raw-hosted", "a.txt", "0123456789")
	put("raw-hosted", "dir/b.txt", "0123456789") // the same blob
	put("raw-hosted", "c.txt", "01234")
	put("maven-releases", "org/ex/app/1.0/app-1.0.jar", "jar-content")
	put("maven-releases", "org/ex/app/1.0/app-1.0.pom", pomOf("org.ex", "app", "1.0"))
	for range 2 {
		mustDo(t, 200, "GET", ts.URL+"/repository/raw-hosted/a.txt", "root", "", false)
	}
	mustDo(t, 204, "DELETE", ts.URL+"/repository/raw-hosted/c.txt", "root", "", false)
	if err := a.Metrics.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	type row struct {
		Name                      string
		Components, Assets, Bytes int64
		UniqueBytes, Requests24h  int64
		ComponentsCurrent         bool
		LastUpload, LastDownload  string
	}
	get := func(query string) (int, []row) {
		t.Helper()
		body := mustDo(t, 200, "GET", ts.URL+"/-/api/dashboard/repositories"+query, "viewer", "", false)
		var out struct {
			Total int
			Items []row
		}
		json.Unmarshal([]byte(body), &out)
		return out.Total, out.Items
	}
	total, rows := get("?sort=bytes")
	if total != 3 || len(rows) != 3 || rows[0].Name != "maven-releases" || rows[1].Name != "raw-hosted" || rows[2].Name != "maven-public" {
		t.Fatalf("by bytes: %+v", rows)
	}
	mvn, raw := rows[0], rows[1]
	if raw.Components != 2 || raw.Assets != 2 || raw.Bytes != 20 || raw.UniqueBytes != 10 || !raw.ComponentsCurrent ||
		raw.LastUpload == "" || raw.LastDownload == "" || raw.Requests24h != 6 {
		t.Fatalf("raw-hosted: %+v", raw)
	}
	// Maven adds maven-metadata.xml: compare with the aggregate over assets.
	live, _ := a.DB.RepoStats(context.Background())
	_, unique, _ := a.DB.BlobStatsInRepos(context.Background(), []string{"maven-releases"})
	if mvn.Components != 1 || mvn.Assets != live["maven-releases"].Assets || mvn.Bytes != live["maven-releases"].Bytes || mvn.UniqueBytes != unique ||
		mvn.Assets < 3 || mvn.LastDownload != "" {
		t.Fatalf("maven-releases: %+v, aggregate %+v and %d", mvn, live["maven-releases"], unique)
	}
	// A re-upload with other content, then a delete: counted again.
	put("raw-hosted", "dir/b.txt", "0123")
	mustDo(t, 204, "DELETE", ts.URL+"/repository/raw-hosted/a.txt", "root", "", false)
	if _, rows = get("?format=raw"); len(rows) != 1 || rows[0].Assets != 1 || rows[0].Bytes != 4 || rows[0].UniqueBytes != 4 || rows[0].Components != 1 {
		t.Fatalf("after re-upload and delete: %+v", rows)
	}
	if total, rows = get("?sort=name&order=desc&limit=1&offset=1"); total != 3 || len(rows) != 1 || rows[0].Name != "maven-releases" {
		t.Fatalf("page: %d %+v", total, rows)
	}
	if total, _ = get("?type=group"); total != 1 {
		t.Fatalf("type filter: %d", total)
	}
	if st, _ := do(t, "GET", ts.URL+"/-/api/dashboard/repositories?sort=bogus", "viewer", "", false); st != 400 {
		t.Fatalf("bad sort: %d", st)
	}
	for user, want := range map[string]int{"": 401, "ci": 403, "root": 200, "viewer": 200} {
		for _, path := range []string{"/-/api/dashboard/repositories", "/-/api/dashboard/repositories/raw-hosted/vulnerabilities", "/-/api/metrics/monthly"} {
			if st, body := do(t, "GET", ts.URL+path, user, "", false); st != want {
				t.Fatalf("%s as %q: %d %s, want %d", path, user, st, body, want)
			}
		}
	}
	// /-/api/me tells the UI whether to show the dashboard and usage entries.
	for user, want := range map[string]string{"ci": `"metrics":false`, "root": `"metrics":true`, "viewer": `"metrics":true`} {
		if body := mustDo(t, 200, "GET", ts.URL+"/-/api/me", user, "", false); !strings.Contains(body, want) {
			t.Fatalf("/-/api/me as %q: %s, want %s", user, body, want)
		}
	}
	if st, body := do(t, "POST", ts.URL+"/-/api/roles", "root", `{"id":"bad","privileges":[{"target":"metrics","actions":["delete"]}]}`, true); st != 400 {
		t.Fatalf("metrics delete privilege: %d %s", st, body)
	}
	body := mustDo(t, 200, "GET", ts.URL+"/service/rest/v1/repositories", "root", "", false)
	if !strings.Contains(body, `"name":"raw-hosted","size":4`) {
		t.Fatalf("nexus size: %s", body)
	}
}

// Monthly usage across a month boundary, with the clocks of the uploads,
// the counters and the samples moved there.
func TestMonthlyMetrics(t *testing.T) {
	a, ts := usageApp(t, "  - { name: raw-hosted, format: raw, type: hosted }\n")
	var now atomic.Int64
	clock := func() time.Time { return time.UnixMilli(now.Load()).UTC() }
	a.Store.Now, a.Metrics.Now = clock, clock
	jan := time.Date(2030, 1, 31, 23, 59, 0, 0, time.UTC)
	feb := time.Date(2030, 2, 1, 0, 1, 0, 0, time.UTC)
	step := func(at time.Time) {
		t.Helper()
		if err := a.Metrics.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := a.UI.RefreshUsage(context.Background(), at); err != nil {
			t.Fatal(err)
		}
	}
	now.Store(jan.UnixMilli())
	mustDo(t, 201, "PUT", ts.URL+"/repository/raw-hosted/a.bin", "root", strings.Repeat("a", 100), false)
	mustDo(t, 200, "GET", ts.URL+"/repository/raw-hosted/a.bin", "ci", "", false)
	step(jan)
	now.Store(feb.UnixMilli())
	mustDo(t, 201, "PUT", ts.URL+"/repository/raw-hosted/b.bin", "root", strings.Repeat("b", 30), false)
	for range 2 {
		mustDo(t, 200, "GET", ts.URL+"/repository/raw-hosted/b.bin", "", "", false)
	}
	mustDo(t, 204, "DELETE", ts.URL+"/repository/raw-hosted/a.bin", "root", "", false)
	step(feb)
	body := mustDo(t, 200, "GET", ts.URL+"/-/api/metrics/monthly?from=2029-12&to=2030-03", "viewer", "", false)
	var out struct {
		Months []struct {
			Month                                      string
			Requests, Downloads, Uploads, UploadBytes  int64
			EgressBytes                                int64
			StorageBytes, PeakStorageBytes, Components *int64
			UniqueUsers, UniqueAddresses               int64
		}
	}
	json.Unmarshal([]byte(body), &out)
	if len(out.Months) != 4 || out.Months[0].Month != "2029-12" || out.Months[3].Month != "2030-03" || out.Months[0].Requests != 0 ||
		out.Months[0].StorageBytes != nil {
		t.Fatalf("range: %s", body)
	}
	j, f := out.Months[1], out.Months[2]
	v := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	if j.Requests != 2 || j.Uploads != 1 || j.UploadBytes != 100 || j.Downloads != 1 || j.EgressBytes < 100 ||
		v(j.StorageBytes) != 100 || v(j.PeakStorageBytes) != 100 || v(j.Components) != 1 || j.UniqueUsers != 2 || j.UniqueAddresses != 1 {
		t.Fatalf("january: %+v (%s)", j, body)
	}
	// February started with January's 100 bytes; the samples never saw the
	// 130 between the upload and the delete.
	if f.Requests != 4 || f.Uploads != 1 || f.UploadBytes != 30 || f.Downloads != 2 || f.EgressBytes < 60 ||
		v(f.StorageBytes) != 30 || v(f.PeakStorageBytes) != 100 || v(f.Components) != 1 || f.UniqueUsers != 1 || f.UniqueAddresses != 1 {
		t.Fatalf("february: %+v (%s)", f, body)
	}
	if st, _ := do(t, "GET", ts.URL+"/-/api/metrics/monthly?from=2030-05&to=2030-01", "viewer", "", false); st != 400 {
		t.Fatalf("reversed range: %d", st)
	}
	if st, _ := do(t, "GET", ts.URL+"/-/api/metrics/monthly?from=2020-01&to=2030-01", "viewer", "", false); st != 400 {
		t.Fatalf("long range: %d", st)
	}
}

// Two nodes upload, download and delete in one repository at once: the
// dashboard (either node) and the month equal what the assets and the
// downloads add up to.
func TestUsageCountersTwoNodes(t *testing.T) {
	a, b := startCluster(t, "  - { name: raw-ha, format: raw, type: hosted, writePolicy: allow }\n")
	var wg sync.WaitGroup
	for i, n := range []*node{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 30 {
				p := fmt.Sprintf("/repository/raw-ha/n%d/f%d.bin", i, j%12)
				n.want(t, "PUT", p, strings.Repeat("x", 10+(i*30+j)%7), ok(201))
				n.want(t, "PUT", "/repository/raw-ha/shared.bin", strings.Repeat("s", 1+j%3), ok(201)) // both nodes replace it
				n.want(t, "GET", p, nil, ok(200))
				if j%5 == 4 {
					n.want(t, "DELETE", p, nil, ok(204))
				}
			}
		}()
	}
	wg.Wait()
	for _, n := range []*node{a, b} {
		if err := n.Metrics.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	live, _ := a.DB.RepoStats(context.Background())
	_, unique, _ := a.DB.BlobStatsInRepos(context.Background(), []string{"raw-ha"})
	var dl int64
	a.DB.WalkAssetPaths(context.Background(), "raw-ha", "", func(p string) {
		as, _ := a.DB.GetAsset(context.Background(), "raw-ha", p)
		dl += as.Downloads
	})
	for _, n := range []*node{a, b} {
		_, body, _ := n.do(t, "root", "GET", "/-/api/dashboard/repositories?q=raw-ha", nil)
		want := fmt.Sprintf(`"components":%d,`, live["raw-ha"].Assets)
		if !strings.Contains(body, fmt.Sprintf(`"assets":%d,"bytes":%d,"uniqueBytes":%d`, live["raw-ha"].Assets, live["raw-ha"].Bytes, unique)) ||
			!strings.Contains(body, want) || !strings.Contains(body, `"requests24h":`) {
			t.Fatalf("dashboard %s, aggregate %+v unique %d", body, live["raw-ha"], unique)
		}
	}
	// Every node's downloads are in the month, also those of the assets deleted since.
	if err := a.UI.RefreshUsage(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, body, _ := a.do(t, "root", "GET", "/-/api/metrics/monthly?from="+time.Now().UTC().Format("2006-01"), nil)
	if !strings.Contains(body, `"downloads":60,"uploads":120,`) || dl > 60 {
		t.Fatalf("month: %s (downloads left on assets %d)", body, dl)
	}
}
