package nexusapi_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/blob/s3test"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// startApp is start, with the app itself, a data directory the test can read
// and options; maven-central stays unreachable.
func startApp(t *testing.T, opts ...app.Option) (*env, *app.App) {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
dataDir: %s
baseURL: http://nexus.example.com
users:
  - { name: admin, passwordHash: %q, roles: [admin] }
  - { name: ci, passwordHash: %q, roles: [deployer] }
roles:
  deployer: { write: [maven-releases, raw-files] }
repositories:
  - { name: maven-releases, format: maven, type: hosted, writePolicy: allow_once }
  - { name: maven-central, format: maven, type: proxy, remote: https://repo1.maven.org/maven2/, contentMaxAge: -1 }
  - { name: maven-public, format: maven, type: group, members: [maven-releases, maven-central] }
  - { name: raw-files, format: raw, type: hosted }
`, t.TempDir(), string(h), string(h))))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Close() })
	return &env{t, ts}, a
}

// makeUser creates a role with the given privileges and a user holding it,
// password "longenough1".
func makeUser(t *testing.T, e *env, id, privileges string) {
	t.Helper()
	if st, b := uiPost(e, "/-/api/roles", `{"id":"`+id+`-role","privileges":`+privileges+`}`); st != 201 {
		t.Fatalf("role %s: %d %s", id, st, b)
	}
	if st, b := uiPost(e, "/-/api/users", `{"id":"`+id+`","password":"longenough1","roles":["`+id+`-role"]}`); st != 201 {
		t.Fatalf("user %s: %d %s", id, st, b)
	}
}

// tokenOf is an API token of user.
func tokenOf(t *testing.T, e *env, user, pass string) string {
	t.Helper()
	st, b := send(e, "POST", "/-/api/tokens", `{}`, user, pass, true)
	if st != 201 {
		t.Fatalf("token of %s: %d %s", user, st, b)
	}
	return decode[map[string]string](t, b)["token"]
}

// withToken sends method path with a Bearer token.
func withToken(t *testing.T, e *env, method, path, token, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// A caller without the right gets one answer from the repository PUT whether
// the repository exists or not, whatever the body says; a caller with it gets
// the name check. A yum body's deployPolicy, which depends on the stored
// depth, tells nothing either.
func TestRepositoryPutTellsNothingWithoutTheRight(t *testing.T) {
	e := start(t, false)
	makeUser(t, e, "ma", `[{"target":"admin:format:maven","actions":["browse","read","edit"]}]`)
	const renamed = `{"name":"other","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"}}`
	put := func(user, pass, path, body string) (int, string) {
		t.Helper()
		st, b := e.as("PUT", v1+"/repositories/"+path, user, pass, body)
		return st, string(b)
	}
	for _, tc := range []struct{ user, pass, exists, missing string }{
		{"ci", "pw", "maven2/hosted/maven-releases", "maven2/hosted/no-such-repo"},
		{"ma", "longenough1", "raw/hosted/raw-files", "raw/hosted/no-such-repo"}, // another format's
	} {
		for _, body := range []string{renamed, "not json", `{"yum":{"repodataDepth":1,"deployPolicy":"PERMISSIVE"}}`} {
			st1, b1 := put(tc.user, tc.pass, tc.exists, body)
			st2, b2 := put(tc.user, tc.pass, tc.missing, body)
			if st1 != 403 || st1 != st2 || b1 != b2 {
				t.Errorf("%s, body %s:\n  existing: %d %s\n  missing:  %d %s", tc.user, body, st1, b1, st2, b2)
			}
		}
	}
	for _, path := range []string{"maven2/hosted/maven-releases", "maven2/hosted/no-such-repo"} {
		if st, _, h := e.do("PUT", v1+"/repositories/"+path, "", renamed); st != 401 || h.Get("WWW-Authenticate") == "" {
			t.Errorf("anonymous %s: %d", path, st)
		}
	}
	// With the right, the checks answer: the name, a missing repository.
	e.wantField("PUT", v1+"/repositories/maven2/hosted/maven-releases", renamed, "name")
	if st, b := put("ma", "longenough1", "maven2/hosted/maven-releases", renamed); st != 400 || !strings.Contains(b, "cannot be renamed") {
		t.Fatalf("maven administrator: %d %s", st, b)
	}
	e.want("PUT", v1+"/repositories/maven2/hosted/no-such-repo", "admin", renamed, 404)
}

// ?type= matches the id an item is labelled with: Nexus's apt rebuild id
// finds the apt rebuild alone, Nexus's Maven rebuild id the Maven rebuild
// alone; Command Center's own id finds both.
func TestTaskTypeFilterMatchesTheItemsID(t *testing.T) {
	e := start(t, false)
	mk := func(typ, name string) {
		e.want("POST", v1+"/tasks", "admin", fmt.Sprintf(`{"type":%q,"name":%q,"frequency":{"schedule":"manual"},"properties":{"repositoryName":"*"}}`, typ, name), 201)
	}
	mk("repository.maven.rebuild-metadata", "m")
	mk("repository.apt.rebuild.metadata", "a")
	for typ, want := range map[string]string{
		"repository.maven.rebuild-metadata": "m",
		"repository.apt.rebuild.metadata":   "a",
		"repository.rebuild-metadata":       "a m",
		"blobstore.compact":                 "Compact blob store", // the seed
	} {
		var p page[map[string]any]
		e.get(v1+"/tasks?type="+typ, "admin", &p)
		names := []string{}
		for _, it := range p.Items {
			if typ != "repository.rebuild-metadata" && it["type"] != typ {
				t.Errorf("?type=%s lists %v", typ, it)
			}
			names = append(names, it["name"].(string))
		}
		if got := strings.Join(sorted(names), " "); got != want {
			t.Errorf("?type=%s: %q, want %q", typ, got, want)
		}
	}
}

func sorted(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

// A queued run reads RUNNING until it ended, a stopped one CANCELED, and a
// stop drops a queued run: nothing ran, so nothing is recorded.
func TestTaskStatesOfQueuedAndStoppedRuns(t *testing.T) {
	e, a := startApp(t)
	release := make(chan struct{})
	a.Tasks.GC = func(ctx context.Context, _ string) (int, int64, error) {
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-release:
			return 0, 0, nil
		}
	}
	t.Cleanup(func() { close(release) })
	mk := func(name string) string {
		var it map[string]any
		json.Unmarshal(e.want("POST", v1+"/tasks", "admin", `{"type":"blobstore.compact","name":"`+name+`","frequency":{"schedule":"manual"}}`, 201), &it)
		return it["id"].(string)
	}
	state := func(id string) (string, any) {
		t.Helper()
		var it map[string]any
		e.get(v1+"/tasks/"+id, "admin", &it)
		return it["currentState"].(string), it["lastRunResult"]
	}
	one, two := mk("one"), mk("two")
	e.want("POST", v1+"/tasks/"+one+"/run", "admin", "", 204)
	if st, _ := state(one); st != "RUNNING" {
		t.Fatalf("running: %s", st)
	}
	e.want("POST", v1+"/tasks/"+two+"/run", "admin", "", 204) // queued behind one
	if st, res := state(two); st != "RUNNING" || res != nil {
		t.Fatalf("queued: %s %v", st, res)
	}
	var p page[map[string]any]
	e.get(v1+"/tasks?type=blobstore.compact", "admin", &p)
	for _, it := range p.Items {
		if (it["id"] == one || it["id"] == two) && it["currentState"] != "RUNNING" {
			t.Fatalf("list: %v", it)
		}
	}
	e.want("POST", v1+"/tasks/"+two+"/stop", "admin", "", 204)
	if st, res := state(two); st != "WAITING" || res != nil {
		t.Fatalf("queued run dropped: %s %v", st, res)
	}
	e.want("POST", v1+"/tasks/"+two+"/stop", "admin", "", 409)
	e.want("POST", v1+"/tasks/"+one+"/stop", "admin", "", 204)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		st, res := state(one)
		if st == "WAITING" && res == "CANCELED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stopped: %s %v", st, res)
		}
	}
}

// The realm list Nexus shows by default leaves the API tokens working and
// reads back as sent, so a Terraform plan of it stays clean; the token realm
// goes off through the native API only, and a list that names a token realm
// turns it on again.
func TestRealmListLeavesTheTokenRealmAlone(t *testing.T) {
	e := start(t, false)
	tok := tokenOf(t, e, "ci", "pw")
	works := func(want bool) {
		t.Helper()
		st, b := withToken(t, e, "GET", v1+"/tags", tok, "")
		if (st == 200) != want || !want && st != 401 {
			t.Fatalf("token: %d %s, want working %v", st, b, want)
		}
	}
	active := func() string {
		t.Helper()
		var list []string
		e.get(v1+"/security/realms/active", "admin", &list)
		return strings.Join(list, ",")
	}
	const nexusDefault = `["NexusAuthenticatingRealm"]`
	e.want("PUT", v1+"/security/realms/active", "admin", nexusDefault, 204)
	if got := active(); got != "NexusAuthenticatingRealm" {
		t.Fatalf("active = %s", got)
	}
	works(true)
	e.want("PUT", v1+"/security/realms/active", "admin", nexusDefault, 204) // the next apply: no change
	works(true)

	// Off through the native API: a list naming no token realm leaves it off.
	if st, b := send(e, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":false}]}`, "admin", "pw", true); st != 200 {
		t.Fatalf("native off: %d %s", st, b)
	}
	works(false)
	if got := active(); got != "NexusAuthenticatingRealm" {
		t.Fatalf("active = %s", got)
	}
	e.want("PUT", v1+"/security/realms/active", "admin", nexusDefault, 204)
	works(false)

	// A list that names one is about tokens: on, and read back as sent.
	e.want("PUT", v1+"/security/realms/active", "admin", `["NexusAuthenticatingRealm","NpmToken"]`, 204)
	works(true)
	if got := active(); got != "NexusAuthenticatingRealm,NpmToken" {
		t.Fatalf("active = %s", got)
	}
	// Named and turned off natively, the list no longer tells the truth.
	if st, b := send(e, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":false}]}`, "admin", "pw", true); st != 200 {
		t.Fatalf("native off: %d %s", st, b)
	}
	if got := active(); got != "NexusAuthenticatingRealm" {
		t.Fatalf("active = %s", got)
	}
}

// An empty blobsOlderThan is not given; a sub-minute duration reads as a
// minute, not as 0, which would mean "always ask the remote".
func TestEmptyOptionalsAndShortDurations(t *testing.T) {
	e := start(t, false)
	for _, v := range []string{`""`, `" "`, `"0"`, `0`, `null`} {
		e.want("POST", v1+"/tasks", "admin", `{"type":"blobstore.compact","name":"c","frequency":{"schedule":"manual"},"properties":{"blobsOlderThan":`+v+`}}`, 201)
	}
	for _, v := range []string{`"5"`, `5`, `true`} {
		e.want("POST", v1+"/tasks", "admin", `{"type":"blobstore.compact","name":"c","frequency":{"schedule":"manual"},"properties":{"blobsOlderThan":`+v+`}}`, 400)
	}
	for _, tc := range []struct {
		native string
		want   int
	}{{"30s", 1}, {"1m", 1}, {"61s", 2}, {"90m", 90}, {"-1", -1}, {"0s", 0}} {
		name := "short-" + strings.NewReplacer("-", "n").Replace(tc.native)
		if st, b := uiPost(e, "/-/api/repositories", fmt.Sprintf(`{"name":%q,"format":"raw","type":"proxy","remote":"http://127.0.0.1:1/","metadataMaxAge":%q,"contentMaxAge":%q,"negativeCacheTTL":%q}`,
			name, tc.native, tc.native, tc.native)); st != 201 {
			t.Fatalf("%s: %d %s", name, st, b)
		}
		var m struct {
			Proxy struct{ ContentMaxAge, MetadataMaxAge int }
		}
		e.get(v1+"/repositories/raw/proxy/"+name, "admin", &m)
		if m.Proxy.ContentMaxAge != tc.want || m.Proxy.MetadataMaxAge != tc.want {
			t.Errorf("%s: %+v, want %d", tc.native, m.Proxy, tc.want)
		}
	}
}

// A metrics-only user reads the metrics paths and the status check; without
// the privilege it stays 403, and metricsAnonymous opens neither the status
// check's messages nor the thread dump to an anonymous caller.
func TestMetricsPrivilegeOpensTheMetricsPaths(t *testing.T) {
	e := start(t, false)
	makeUser(t, e, "mon", `[{"target":"metrics","actions":["read"]}]`)
	for _, p := range []string{"/service/rest/metrics/ping", "/service/rest/metrics/prometheus", "/service/rest/metrics/data", "/service/rest/metrics/threads", v1 + "/status/check"} {
		e.asWant("GET", p, "mon", "longenough1", "", 200)
		e.want("GET", p, "ci", "", 403)
		e.want("GET", p, "", "", 401)
	}
	b := e.asWant("GET", v1+"/status/check", "mon", "longenough1", "", 200)
	var checks map[string]struct {
		Healthy bool
		Message string
	}
	json.Unmarshal(b, &checks)
	for _, name := range []string{"Blob Stores Ready", "Blob Stores Quota", "Available CPUs", "Database", "Blob store"} {
		if _, ok := checks[name]; !ok {
			t.Errorf("no check %q: %s", name, b)
		}
	}
	if c := checks["Blob Stores Ready"]; !c.Healthy || !strings.Contains(c.Message, "blob stores answer") {
		t.Errorf("ready: %+v", c)
	}
	if c := checks["Blob Stores Quota"]; !c.Healthy {
		t.Errorf("quota: %+v", c)
	}
	if st, b := send(e, "PUT", "/-/api/settings", `{"metricsAnonymous":true}`, "admin", "pw", true); st != 200 {
		t.Fatalf("settings: %d %s", st, b)
	}
	e.want("GET", "/service/rest/metrics/ping", "", "", 200)
	e.want("GET", v1+"/status/check", "", "", 401)
	e.want("GET", "/service/rest/metrics/threads", "", "", 401)
}

// fakeSMTP takes one message at a time and keeps what came in.
type fakeSMTP struct {
	ln  net.Listener
	mu  sync.Mutex
	got []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	io.WriteString(c, "220 fake\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch up := strings.ToUpper(strings.TrimSpace(line)); {
		case up == "DATA":
			io.WriteString(c, "354 go\r\n")
			var msg strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || strings.TrimSpace(l) == "." {
					break
				}
				msg.WriteString(l)
			}
			f.mu.Lock()
			f.got = append(f.got, msg.String())
			f.mu.Unlock()
			io.WriteString(c, "250 ok\r\n")
		case up == "QUIT":
			io.WriteString(c, "221 bye\r\n")
			return
		default:
			io.WriteString(c, "250 ok\r\n")
		}
	}
}

func (f *fakeSMTP) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		return ""
	}
	return f.got[len(f.got)-1]
}

// E-mail through the Nexus API: subjectPrefix and nexusTrustStoreEnabled read
// back as sent, the prefix goes on the subject, and verify sends while e-mail
// is switched off.
func TestEmailPrefixTrustStoreAndVerifyWhileDisabled(t *testing.T) {
	e := start(t, false)
	smtp := newFakeSMTP(t)
	port := smtp.ln.Addr().(*net.TCPAddr).Port
	body := func(enabled bool, prefix string, trust any) string {
		return fmt.Sprintf(`{"enabled":%v,"host":"127.0.0.1","port":%d,"fromAddress":"cc@example.com","subjectPrefix":%s,"nexusTrustStoreEnabled":%v}`, enabled, port, prefix, trust)
	}
	e.want("POST", v1+"/email/verify", "admin", "a@example.com", 400) // nothing configured
	e.want("PUT", v1+"/email", "admin", body(false, `"[cc]"`, false), 204)
	got := decodeMap(t, e.want("GET", v1+"/email", "admin", "", 200))
	if got["subjectPrefix"] != "[cc]" || got["nexusTrustStoreEnabled"] != false || got["enabled"] != false {
		t.Fatalf("GET: %v", got)
	}
	if m := decodeMap(t, e.want("POST", v1+"/email/verify", "admin", "a@example.com", 200)); m["success"] != true {
		t.Fatalf("verify: %v", m)
	}
	if msg := smtp.last(); !strings.Contains(msg, "Subject: =?utf-8?q?[cc]_[Command_Center]") && !strings.Contains(msg, "Subject: [cc] [Command Center]") {
		t.Fatalf("message:\n%s", msg)
	}
	// true reads back true with no TLS at all: the value sent; null clears the
	// prefix; absent keeps the trust store switch.
	e.want("PUT", v1+"/email", "admin", body(false, `null`, true), 204)
	got = decodeMap(t, e.want("GET", v1+"/email", "admin", "", 200))
	if got["subjectPrefix"] != nil || got["nexusTrustStoreEnabled"] != true {
		t.Fatalf("GET: %v", got)
	}
	e.want("PUT", v1+"/email", "admin", fmt.Sprintf(`{"enabled":false,"host":"127.0.0.1","port":%d,"fromAddress":"cc@example.com"}`, port), 204)
	if got = decodeMap(t, e.want("GET", v1+"/email", "admin", "", 200)); got["nexusTrustStoreEnabled"] != true {
		t.Fatalf("GET: %v", got)
	}
	// The form of the native API knows neither: a save keeps them.
	e.want("PUT", v1+"/email", "admin", body(false, `"[cc]"`, false), 204)
	if st, b := send(e, "PUT", "/-/api/settings/smtp", fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"security":"none","from":"cc@example.com","enabled":false}`, port), "admin", "pw", true); st != 200 {
		t.Fatalf("native: %d %s", st, b)
	}
	if got = decodeMap(t, e.want("GET", v1+"/email", "admin", "", 200)); got["subjectPrefix"] != "[cc]" || got["nexusTrustStoreEnabled"] != false {
		t.Fatalf("after the native save: %v", got)
	}
	e.wantField("PUT", v1+"/email", body(false, `"a\nb"`, false), "*")
	e.want("DELETE", v1+"/email", "admin", "", 204)
	if got = decodeMap(t, e.want("GET", v1+"/email", "admin", "", 200)); got["subjectPrefix"] != nil {
		t.Fatalf("after DELETE: %v", got)
	}
}

// PUT /blobstores/s3/{name} rotates keys and moves the endpoint after the
// bucket answered; what it does not take leaves the store as it was.
func TestS3BlobStoreConnectionUpdate(t *testing.T) {
	e := start(t, false)
	srv := s3test.New(t)
	c := srv.Config()
	body := func(region, endpoint, access, secret string, pathStyle bool, pool string) string {
		return fmt.Sprintf(`{"name":"s3a","softQuota":null,"bucketConfiguration":{"bucket":{"region":%q,"name":%q,"prefix":"cc"},`+
			`"bucketSecurity":{"accessKeyId":%q,"secretAccessKey":%q},"advancedBucketConnection":{"endpoint":%q,"forcePathStyle":%v%s}}}`,
			region, c.Bucket, access, secret, endpoint, pathStyle, pool)
	}
	e.want("POST", v1+"/blobstores/s3", "admin", body(c.Region, c.Endpoint, c.AccessKey, c.SecretKey, true, `,"maxConnectionPoolSize":7`), 201)
	bc := func() map[string]any {
		t.Helper()
		return decodeMap(t, e.want("GET", v1+"/blobstores/s3/s3a", "admin", "", 200))["bucketConfiguration"].(map[string]any)
	}
	if adv := bc()["advancedBucketConnection"].(map[string]any); adv["maxConnectionPoolSize"] != float64(7) {
		t.Fatalf("pool: %v", adv)
	}
	e.want("POST", v1+"/repositories/raw/hosted", "admin", `{"name":"raw-s3","online":true,"storage":{"blobStoreName":"s3a","strictContentTypeValidation":false,"writePolicy":"ALLOW"}}`, 201)
	e.want("PUT", "/repository/raw-s3/a.txt", "admin", "first", 201)

	// The bucket's keys change; the store's still are the old ones, and the
	// checks and the store list say so.
	srv.SetKeys("NEWACCESSKEY", "new-secret-key")
	e.want("GET", "/repository/raw-s3/a.txt", "admin", "", 500)
	ready := func() (bool, string) {
		t.Helper()
		var checks map[string]struct {
			Healthy bool
			Message string
		}
		e.get(v1+"/status/check", "admin", &checks)
		c := checks["Blob Stores Ready"]
		return c.Healthy, c.Message
	}
	if ok, msg := ready(); ok || !strings.Contains(msg, `"s3a"`) {
		t.Fatalf("ready with old keys: %v %s", ok, msg)
	}
	if b := e.want("PUT", v1+"/blobstores/s3/s3a", "admin", body(c.Region, c.Endpoint, "NEWACCESSKEY", "wrong", true, ""), 400); !strings.Contains(string(b), "not usable") || strings.Contains(string(b), "wrong\"") {
		t.Fatalf("wrong secret: %s", b)
	}
	if got := bc()["bucketSecurity"].(map[string]any); got["accessKeyId"] != c.AccessKey {
		t.Fatalf("a refused update was stored: %v", got)
	}
	e.want("PUT", v1+"/blobstores/s3/s3a", "ci", body(c.Region, c.Endpoint, "NEWACCESSKEY", "new-secret-key", true, ""), 403)
	e.want("PUT", v1+"/blobstores/s3/s3a", "admin", body(c.Region, c.Endpoint, "NEWACCESSKEY", "new-secret-key", true, ""), 204)
	if b := e.want("GET", "/repository/raw-s3/a.txt", "admin", "", 200); string(b) != "first" {
		t.Fatalf("read with the new keys: %s", b)
	}
	if ok, msg := ready(); !ok { // this node's probe of the old settings is forgotten
		t.Fatalf("ready with new keys: %s", msg)
	}
	e.want("PUT", "/repository/raw-s3/b.txt", "admin", "second", 201)
	if got := bc()["bucketSecurity"].(map[string]any); got["accessKeyId"] != "NEWACCESSKEY" || got["secretAccessKey"] != "#~NXRM~PLACEHOLDER~PASSWORD~#" {
		t.Fatalf("GET: %v", got)
	}

	// An empty secret and the placeholder keep the stored one.
	for _, secret := range []string{"", "#~NXRM~PLACEHOLDER~PASSWORD~#"} {
		e.want("PUT", v1+"/blobstores/s3/s3a", "admin", body(c.Region, c.Endpoint, "NEWACCESSKEY", secret, true, ""), 204)
	}
	e.want("GET", "/repository/raw-s3/b.txt", "admin", "", 200)

	// A region and the pool change; a path style the endpoint does not take
	// is refused and changes nothing.
	e.want("PUT", v1+"/blobstores/s3/s3a", "admin", body("eu-west-1", c.Endpoint, "NEWACCESSKEY", "", true, `,"maxConnectionPoolSize":3`), 204)
	if got := bc(); got["bucket"].(map[string]any)["region"] != "eu-west-1" || got["advancedBucketConnection"].(map[string]any)["maxConnectionPoolSize"] != float64(3) {
		t.Fatalf("GET: %v", got)
	}
	e.want("GET", "/repository/raw-s3/b.txt", "admin", "", 200)
	e.want("PUT", v1+"/blobstores/s3/s3a", "admin", body("eu-west-1", c.Endpoint, "NEWACCESSKEY", "", false, ""), 400)
	e.want("GET", "/repository/raw-s3/b.txt", "admin", "", 200)

	// The endpoint moves: the secret comes along only if sent again.
	other := s3test.New(t)
	other.SetKeys("NEWACCESSKEY", "new-secret-key")
	for _, secret := range []string{"", "#~NXRM~PLACEHOLDER~PASSWORD~#"} {
		e.wantField("PUT", v1+"/blobstores/s3/s3a", body("eu-west-1", other.URL, "NEWACCESSKEY", secret, true, ""), "bucketConfiguration.bucketSecurity.secretAccessKey")
	}
	e.want("PUT", v1+"/blobstores/s3/s3a", "admin", body("eu-west-1", other.URL, "NEWACCESSKEY", "new-secret-key", true, ""), 204)
	e.want("PUT", "/repository/raw-s3/c.txt", "admin", "third", 201)
	if !slices.ContainsFunc(other.Keys(), func(k string) bool { return !strings.Contains(k, "/meta/") }) {
		t.Fatalf("not written to the new endpoint: %v", other.Keys())
	}

	// Never the bucket or the prefix, nor encryption; and bad settings 400.
	e.wantField("PUT", v1+"/blobstores/s3/s3a", strings.Replace(body("eu-west-1", other.URL, "NEWACCESSKEY", "", true, ""), `"prefix":"cc"`, `"prefix":"elsewhere"`, 1), "bucketConfiguration")
	e.wantField("PUT", v1+"/blobstores/s3/s3a", strings.Replace(body("eu-west-1", other.URL, "NEWACCESSKEY", "", true, ""), `"bucketSecurity"`, `"encryption":{"encryptionType":"s3ManagedEncryption"},"bucketSecurity"`, 1), "bucketConfiguration")
	e.wantField("PUT", v1+"/blobstores/s3/s3a", body("eu-west-1", other.URL, "NEWACCESSKEY", "", true, `,"maxConnectionPoolSize":-1`), "bucketConfiguration")

	// A second store on the location this one would move to is refused.
	second := fmt.Sprintf(`{"name":"s3b","softQuota":null,"bucketConfiguration":{"bucket":{"region":"us-east-1","name":%q,"prefix":"cc"},`+
		`"bucketSecurity":{"accessKeyId":%q,"secretAccessKey":%q},"advancedBucketConnection":{"endpoint":%q,"forcePathStyle":true}}}`, c.Bucket, "NEWACCESSKEY", "new-secret-key", c.Endpoint)
	e.want("POST", v1+"/blobstores/s3", "admin", second, 201)
	if b := e.want("PUT", v1+"/blobstores/s3/s3a", "admin", body("eu-west-1", c.Endpoint, "NEWACCESSKEY", "new-secret-key", true, ""), 400); !strings.Contains(string(b), "already the") {
		t.Fatalf("isolation: %s", b)
	}
}

// The native update takes the same settings: an s3 object with what it wants
// them to be.
func TestS3BlobStoreNativeUpdate(t *testing.T) {
	e := start(t, false)
	srv := s3test.New(t)
	c := srv.Config()
	if st, b := uiPost(e, "/-/api/blobstores", fmt.Sprintf(`{"name":"s3n","type":"s3","s3":{"bucket":%q,"prefix":"n","region":%q,"endpoint":%q,"pathStyle":true,"accessKeyId":%q,"secretAccessKey":%q,"sse":"AES256"}}`,
		c.Bucket, c.Region, c.Endpoint, c.AccessKey, c.SecretKey)); st != 201 {
		t.Fatalf("create: %d %s", st, b)
	}
	put := func(s3 string) (int, string) {
		t.Helper()
		st, b := send(e, "PUT", "/-/api/blobstores/s3n", `{"s3":`+s3+`}`, "admin", "pw", true)
		return st, string(b)
	}
	srv.SetKeys("NATIVEKEY", "native-secret")
	conn := fmt.Sprintf(`"region":%q,"endpoint":%q,"pathStyle":true`, c.Region, c.Endpoint)
	if st, b := put(`{"accessKeyId":"NATIVEKEY",` + conn + `}`); st != 400 || !strings.Contains(b, "not usable") { // the old secret goes with the new key
		t.Fatalf("no secret: %d %s", st, b)
	}
	if st, b := put(`{"accessKeyId":"NATIVEKEY","secretAccessKey":"native-secret",` + conn + `}`); st != 200 || !strings.Contains(b, `"accessKeyId":"NATIVEKEY"`) || strings.Contains(b, "native-secret") {
		t.Fatalf("rotate: %d %s", st, b)
	}
	if st, b := put(`{"accessKeyId":"NATIVEKEY",` + conn + `}`); st != 200 || !strings.Contains(b, `"sse":"AES256"`) || !strings.Contains(b, `"secretSet":true`) { // nothing new: encryption kept
		t.Fatalf("keep: %d %s", st, b)
	}
	for s3, want := range map[string]string{
		`{"bucket":"other-bucket","accessKeyId":"NATIVEKEY",` + conn + `}`:                "bucket and prefix cannot change",
		`{"prefix":"/m/","accessKeyId":"NATIVEKEY",` + conn + `}`:                         "bucket and prefix cannot change",
		`{"sse":"aws:kms","accessKeyId":"NATIVEKEY",` + conn + `}`:                        "encryption and failover cannot change",
		`{"failover":[{"bucket":"other-bucket"}],"accessKeyId":"NATIVEKEY",` + conn + `}`: "encryption and failover cannot change",
		`{"secretAccessKey":"x"}`: "go together",
		`{"accessKeyId":"NATIVEKEY","endpoint":"ftp://x","pathStyle":true}`: "endpoint",
	} {
		if st, b := put(s3); st != 400 || !strings.Contains(b, want) {
			t.Errorf("%s: %d %s, want ~%s", s3, st, b, want)
		}
	}
	if st, b := send(e, "PUT", "/-/api/blobstores/default", `{"s3":{"accessKeyId":"x"}}`, "admin", "pw", true); st != 400 || !strings.Contains(string(b), "s3 store") {
		t.Fatalf("a file store: %d %s", st, b)
	}
	if st, _ := send(e, "PUT", "/-/api/blobstores/s3n", `{"s3":{"accessKeyId":"x"}}`, "ci", "pw", true); st != 403 {
		t.Fatalf("not an admin: %d", st)
	}
	tok := tokenOf(t, e, "admin", "pw")
	if st, _ := withToken(t, e, "PUT", "/-/api/blobstores/s3n", tok, `{"s3":{"accessKeyId":"x"}}`); st != 403 {
		t.Fatalf("a token: %d", st)
	}
}

// A relative path of the Nexus API is under <dataDir>/blobstores and reads
// back as sent; what leaves that directory is refused; the native API stays
// absolute.
func TestFileBlobStoreRelativePath(t *testing.T) {
	e := start(t, false)
	e.want("POST", v1+"/blobstores/file", "admin", `{"name":"rel","path":"disks/one"}`, 204)
	if b := e.want("GET", v1+"/blobstores/file/rel", "admin", "", 200); string(b) != `{"path":"disks/one","softQuota":null}`+"\n" {
		t.Fatalf("GET = %s", b)
	}
	st, b := uiGet(e, "/-/api/blobstores/rel")
	if st != 200 || !strings.Contains(string(b), filepath.Join("blobstores", "disks", "one")+`"`) {
		t.Fatalf("native: %d %s", st, b)
	}
	e.want("PUT", v1+"/blobstores/file/rel", "admin", `{"path":"disks/one","softQuota":{"type":"spaceUsedQuota","limit":10}}`, 204)
	e.wantField("PUT", v1+"/blobstores/file/rel", `{"path":"disks/two"}`, "path")
	for _, bad := range []string{"../out", "a/../../out", "."} {
		e.wantField("POST", v1+"/blobstores/file", `{"name":"bad","path":"`+bad+`"}`, "path")
	}
	e.want("POST", v1+"/blobstores/file", "admin", `{"name":"inside","path":"disks/one/nested"}`, 204) // not over another's blobs
	if st, b := uiPost(e, "/-/api/blobstores", `{"name":"nat","path":"relative/here"}`); st != 400 || !strings.Contains(string(b), "must be absolute") {
		t.Fatalf("native relative: %d %s", st, b)
	}
}

// One JSON line per event, tagged; what the event says about a URL's secrets
// stays out of it.
func TestAuditLogLine(t *testing.T) {
	var mu sync.Mutex
	var out bytes.Buffer
	e, _ := startApp(t, app.WithAuditLog(slog.New(slog.NewJSONHandler(lockedWriter{&mu, &out}, nil))))
	e.want("POST", v1+"/repositories/raw/hosted", "admin", `{"name":"audited","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"ALLOW"}}`, 201)
	mu.Lock()
	lines := out.String()
	mu.Unlock()
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		if m["audit"] != true || m["msg"] != "audit" || m["level"] != "INFO" {
			t.Fatalf("not tagged: %v", m)
		}
		if m["kind"] == "repo.create" && m["actor"] == "admin" && m["repo"] == "audited" && m["detail"] == "raw/hosted" && m["node"] != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no line for the create:\n%s", lines)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func zipEntries(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%v: %.100s", err, b)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(body)
	}
	return out
}

// POST /support/supportzip: the sections the body names, the task logs among
// them; an empty body is all of them; tokens and non-admins are refused.
func TestSupportZipRoute(t *testing.T) {
	e := start(t, false)
	var created map[string]any
	json.Unmarshal(e.want("POST", v1+"/tasks", "admin", `{"type":"blobstore.delete-temp-files","name":"tmp","frequency":{"schedule":"manual"},"properties":{"blobstoreName":"default"}}`, 201), &created)
	id := created["id"].(string)
	e.want("POST", v1+"/tasks/"+id+"/run", "admin", "", 204)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var it map[string]any
		e.get(v1+"/tasks/"+id, "admin", &it)
		if it["lastRunResult"] != nil && it["currentState"] == "WAITING" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not finish: %v", it)
		}
	}
	all := zipEntries(t, e.want("POST", v1+"/support/supportzip", "admin", "", 200))
	for _, name := range []string{"info.json", "config.json", "tasks.json", "events.json", "logs.txt", "threads.txt", "metrics.txt", "status.json"} {
		if _, ok := all[name]; !ok {
			t.Errorf("no %s in the full zip", name)
		}
	}
	var log string
	for name, body := range all {
		if strings.HasPrefix(name, "task-logs/"+id+"/") {
			log = body
		}
	}
	if !strings.Contains(log, "blobstore.delete-temp-files") || !strings.Contains(log, "OK") || !strings.Contains(log, "finished") {
		t.Fatalf("task log: %q", log)
	}
	some := zipEntries(t, e.want("POST", v1+"/support/supportzip", "admin", `{"taskLog":true,"security":true,"jmx":true,"threadDump":false,"limitZipSize":true,"archivedLog":3}`, 200))
	for name := range some {
		if name != "info.json" && name != "config.json" && name != "tasks.json" && !strings.HasPrefix(name, "task-logs/") {
			t.Errorf("%s came along", name)
		}
	}
	if !strings.Contains(some["config.json"], `"realms"`) || strings.Contains(some["config.json"], `"smtp"`) {
		t.Errorf("config.json: %s", some["config.json"])
	}
	none := zipEntries(t, e.want("POST", v1+"/support/supportzip", "admin", `{}`, 200))
	if len(none) != 1 || none["info.json"] == "" {
		t.Errorf("no section: %v", len(none))
	}
	if st, b := uiGet(e, "/-/api/support/zip?sections=threadDump,bogus"); st != 400 || !strings.Contains(string(b), "bogus") {
		t.Errorf("unknown section: %d %s", st, b)
	}
	e.want("POST", v1+"/support/supportzip", "admin", "{", 400)
	e.want("POST", v1+"/support/supportzip", "ci", "", 403)
	e.want("POST", v1+"/support/supportzip", "", "", 401)
	if st, _ := withToken(t, e, "POST", v1+"/support/supportzip", tokenOf(t, e, "admin", "pw"), ""); st != 403 {
		t.Fatalf("a token: %d", st)
	}
}

func TestSystemNode(t *testing.T) {
	e := start(t, false)
	var m map[string]string
	e.get(v1+"/system/node", "admin", &m)
	if !regexp.MustCompile(`.+-[0-9a-f]+$`).MatchString(m["nodeId"]) {
		t.Fatalf("node: %v", m)
	}
	e.want("GET", v1+"/system/node", "ci", "", 403)
	e.want("GET", v1+"/system/node", "", "", 401)
}

// A repository administrator invalidates the cache of a repository it may
// edit, with a password or a session; rebuild-index follows the same rule.
func TestInvalidateCacheForARepositoryAdministrator(t *testing.T) {
	e := start(t, false)
	makeUser(t, e, "pa", `[{"target":"admin:repo:maven-central","actions":["browse","read","edit"]}]`)
	for _, p := range []string{"invalidate-cache", "rebuild-index"} {
		e.asWant("POST", v1+"/repositories/maven-central/"+p, "pa", "longenough1", "", 204)
		e.asWant("POST", v1+"/repositories/maven-public/"+p, "pa", "longenough1", "", 403)
		e.asWant("POST", v1+"/repositories/no-such-repo/"+p, "pa", "longenough1", "", 403) // no telling what exists
		e.want("POST", v1+"/repositories/no-such-repo/"+p, "admin", "", 404)
		e.want("POST", v1+"/repositories/maven-central/"+p, "ci", "", 403)
	}
	e.want("POST", v1+"/repositories/maven-public/invalidate-cache", "admin", "", 204)
	e.want("POST", v1+"/repositories/maven-public/rebuild-index", "admin", "", 400)
	e.want("POST", v1+"/repositories/maven-releases/invalidate-cache", "admin", "", 400)
	if st, _ := withToken(t, e, "POST", v1+"/repositories/maven-central/invalidate-cache", tokenOf(t, e, "pa", "longenough1"), ""); st != 403 {
		t.Fatalf("a token: %d", st)
	}
}

// nx-privileges-* grants the roles domain (privileges are roles here) and a
// role naming it reads back as written.
func TestPrivilegesDomain(t *testing.T) {
	e := start(t, false)
	e.want("POST", v1+"/security/roles", "admin", `{"id":"sec-read","name":"sec-read","privileges":["nx-privileges-read"]}`, 200)
	e.want("POST", v1+"/security/roles", "admin", `{"id":"sec-all","name":"sec-all","privileges":["nx-privileges-all"]}`, 200)
	for id, want := range map[string]string{"sec-read": "nx-privileges-read", "sec-all": "nx-privileges-all"} {
		if r := decode[nxRoleT](t, e.want("GET", v1+"/security/roles/"+id, "admin", "", 200)); strings.Join(r.Privileges, ",") != want {
			t.Errorf("%s: %v", id, r.Privileges)
		}
	}
	if p := decodeMap(t, e.want("GET", v1+"/security/privileges/nx-privileges-update", "admin", "", 200)); p["domain"] != "privileges" || p["type"] != "application" {
		t.Fatalf("built-in: %v", p)
	}
	e.want("POST", v1+"/security/users", "admin", `{"userId":"sr","firstName":"a","lastName":"b","emailAddress":"a@example.com","password":"longenough1","status":"active","roles":["sec-read"]}`, 200)
	e.asWant("GET", v1+"/security/roles", "sr", "longenough1", "", 200)
	e.asWant("POST", v1+"/security/roles", "sr", "longenough1", `{"id":"x","name":"x"}`, 403)
	e.want("POST", v1+"/security/privileges/application", "admin", `{"name":"p-app","description":"d","domain":"privileges","actions":["READ"]}`, 201)
	e.want("POST", v1+"/security/privileges/wildcard", "admin", `{"name":"p-wild","description":"d","pattern":"nexus:privileges:read"}`, 201)
	e.want("POST", v1+"/security/roles", "admin", `{"id":"other","name":"other","privileges":["nx-ldap-all"]}`, 400) // still none
}

// A grant on a group opens GET /repositories/{name} of its members.
func TestRepositoryReadThroughAGroupGrant(t *testing.T) {
	e := start(t, false)
	if st, b := uiCall(e, "PUT", "/-/api/settings", `{"authenticatedRead":false}`); st != 200 {
		t.Fatalf("settings: %d %s", st, b)
	}
	makeUser(t, e, "gr", `[{"target":"repo:maven-public","actions":["read"]}]`)
	e.asWant("GET", v1+"/repositories/maven-public", "gr", "longenough1", "", 200)
	e.asWant("GET", v1+"/repositories/maven-releases", "gr", "longenough1", "", 200)
	e.asWant("GET", v1+"/repositories/raw-files", "gr", "longenough1", "", 404)
	e.asWant("GET", v1+"/repositories/no-such-repo", "gr", "longenough1", "", 404)
	e.want("GET", v1+"/repositories/maven-releases", "", "", 401)
}

// app:tags delegates creating, editing and deleting tags, a token too, as CI
// makes a tag per build; nx-tags-* maps onto it; without it they stay 403.
func TestTagsDomain(t *testing.T) {
	e := start(t, false)
	makeUser(t, e, "tagger", `[{"target":"app:tags","actions":["read","add","edit"]}]`)
	e.asWant("POST", v1+"/tags", "tagger", "longenough1", `{"name":"build-1"}`, 200)
	e.asWant("PUT", v1+"/tags/build-1", "tagger", "longenough1", `{"attributes":{"n":1}}`, 200)
	e.asWant("DELETE", v1+"/tags/build-1", "tagger", "longenough1", "", 403)
	e.want("POST", v1+"/tags", "ci", `{"name":"build-2"}`, 403)
	e.want("PUT", v1+"/tags/build-1", "ci", `{"attributes":{}}`, 403)
	e.want("DELETE", v1+"/tags/build-1", "ci", "", 403)
	if st, b := withToken(t, e, "POST", v1+"/tags", tokenOf(t, e, "tagger", "longenough1"), `{"name":"build-3"}`); st != 200 {
		t.Fatalf("a token: %d %s", st, b)
	}
	e.want("GET", v1+"/tags", "ci", "", 200) // reading stays open to the signed-in

	e.want("POST", v1+"/security/roles", "admin", `{"id":"tag-admin","name":"tag-admin","privileges":["nx-tags-all"]}`, 200)
	if r := decode[nxRoleT](t, e.want("GET", v1+"/security/roles/tag-admin", "admin", "", 200)); strings.Join(r.Privileges, ",") != "nx-tags-all" {
		t.Fatalf("role: %v", r.Privileges)
	}
	e.want("POST", v1+"/security/users", "admin", `{"userId":"ta","firstName":"a","lastName":"b","emailAddress":"a@example.com","password":"longenough1","status":"active","roles":["tag-admin"]}`, 200)
	e.asWant("DELETE", v1+"/tags/build-1", "ta", "longenough1", "", 204)
	e.want("POST", v1+"/security/privileges/application", "admin", `{"name":"t-app","description":"d","domain":"tags","actions":["CREATE"]}`, 201)
	if st, b := uiPost(e, "/-/api/roles", `{"id":"bad-tags","privileges":[{"target":"app:tags","actions":["run"]}]}`); st != 400 {
		t.Fatalf("an action tags does not delegate: %d %s", st, b)
	}
	// The domains the UI shows a signed-in holder.
	me := decodeMap(t, e.asWant("GET", "/-/api/me", "tagger", "longenough1", "", 200))
	if !strings.Contains(fmt.Sprint(me), "tags") {
		t.Fatalf("me: %v", me)
	}
}

// A cached file whose remote now says 404 or 410 stays served, marked stale;
// a file never cached is still a 404. The document that decides a group's
// owner is not such a file: when it goes, the package goes.
func TestCachedAssetTheRemoteLost(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		u := newUpstream(t, status, map[string]string{"/f.txt": "cached body"})
		e := startRepos(t, fmt.Sprintf("\n  - { name: px, format: raw, type: proxy, remote: %s/, contentMaxAge: 0s }\n", u.URL))
		if b := e.want("GET", "/repository/px/f.txt", "", "", 200); string(b) != "cached body" {
			t.Fatalf("first: %s", b)
		}
		u.mu.Lock()
		delete(u.files, "/f.txt")
		u.mu.Unlock()
		for i := 0; i < 2; i++ { // asked again each time, no miss recorded
			before := u.hits.Load()
			st, b, h := e.do("GET", "/repository/px/f.txt", "", "")
			if st != 200 || string(b) != "cached body" || !strings.Contains(h.Get("Warning"), "110") {
				t.Fatalf("%d, try %d: %d %q %v", status, i, st, b, h)
			}
			if u.hits.Load() != before+1 {
				t.Fatalf("%d, try %d: not asked", status, i)
			}
		}
		u.put("/f.txt", "new body")
		if b := e.want("GET", "/repository/px/f.txt", "", "", 200); string(b) != "new body" && string(b) != "cached body" {
			t.Fatalf("back: %q", b)
		}
		e.want("GET", "/repository/px/never.txt", "", "", 404)
	}

	internal := newUpstream(t, http.StatusNotFound, map[string]string{})
	public := newUpstream(t, http.StatusNotFound, map[string]string{})
	doc := func(from string) string {
		return `{"name":"lp","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{"name":"lp","version":"1.0.0","description":"` + from + `","dist":{"tarball":"http://x/lp/-/lp-1.0.0.tgz"}}}}`
	}
	internal.put("/lp", doc("internal"))
	public.put("/lp", doc("public"))
	e := startRepos(t, fmt.Sprintf(`
  - { name: npm-internal, format: npm, type: proxy, remote: %s/, metadataMaxAge: 0s }
  - { name: npm-public, format: npm, type: proxy, remote: %s/, metadataMaxAge: 0s }
  - { name: npm-all, format: npm, type: group, members: [npm-internal, npm-public] }
`, internal.URL, public.URL))
	if b := e.want("GET", "/repository/npm-all/lp", "", "", 200); !strings.Contains(string(b), "internal") {
		t.Fatalf("group: %s", b)
	}
	internal.mu.Lock()
	delete(internal.files, "/lp")
	internal.mu.Unlock()
	if b := e.want("GET", "/repository/npm-all/lp", "", "", 200); !strings.Contains(string(b), "public") {
		t.Fatalf("the package left the internal registry: %s", b)
	}
	e.want("GET", "/repository/npm-internal/lp", "", "", 404)
}

// A store's probe fails after it opened: Blob Stores Ready and the store's
// unavailable flag say so (the ui package's test shows a re-probe; here, the
// first probe after the store broke).
func TestStoreThatStoppedWorkingIsReported(t *testing.T) {
	e := start(t, false)
	root := filepath.Join(t.TempDir(), "disk")
	e.want("POST", v1+"/blobstores/file", "admin", `{"name":"disk","path":"`+root+`"}`, 204)
	if err := os.RemoveAll(filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tmp"), nil, 0o644); err != nil { // a read-only or lost mount, whoever runs the test
		t.Fatal(err)
	}
	b := e.want("GET", v1+"/status/check", "admin", "", 200)
	var checks map[string]struct {
		Healthy bool
		Message string
	}
	json.Unmarshal(b, &checks)
	if c := checks["Blob Stores Ready"]; c.Healthy || !strings.Contains(c.Message, `"disk"`) {
		t.Fatalf("ready: %+v", c)
	}
	if c := checks["Database"]; !c.Healthy {
		t.Fatalf("database: %+v", c)
	}
	var list []map[string]any
	e.get(v1+"/blobstores", "admin", &list)
	for _, s := range list {
		if want := s["name"] == "disk"; s["unavailable"] != want {
			t.Errorf("%v: unavailable %v", s["name"], s["unavailable"])
		}
	}
}

// core-12: a HEAD of a raw or Maven file a proxy does not hold is the remote's
// HEAD, and nothing is fetched or stored. Only a 200 answers: a remote that
// takes no HEAD, or says 404 to it, gets the GET a HEAD has always been. A
// file held, and a checksum, which is computed from the file, are as before.
func TestHeadOfAFileNotCached(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	noHead := false
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.Method]++
		refuse := noHead
		mu.Unlock()
		switch {
		case r.Method == http.MethodHead && refuse:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case strings.Contains(r.URL.Path, "missing"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			w.Header().Set("Content-Length", "6")
			if r.Method == http.MethodGet {
				io.WriteString(w, "remote")
			}
		}
	}))
	t.Cleanup(remote.Close)
	count := func(method string) int {
		mu.Lock()
		defer mu.Unlock()
		return asked[method]
	}
	reset := func(refuse bool) {
		mu.Lock()
		defer mu.Unlock()
		asked, noHead = map[string]int{}, refuse
	}
	e := startRepos(t, fmt.Sprintf(`
  - { name: raw-px, format: raw, type: proxy, remote: %s/ }
  - { name: raw-all, format: raw, type: group, members: [raw-px] }
  - { name: mvn-px, format: maven, type: proxy, remote: %s/ }
`, remote.URL, remote.URL))
	assetsOf := func(repo string) int {
		var p page[asset]
		e.get(v1+"/assets?repository="+repo, "admin", &p)
		return len(p.Items)
	}
	for _, path := range []string{"/repository/raw-px/a.txt", "/repository/raw-all/b.txt", "/repository/mvn-px/org/x/1.0/x-1.0.jar"} {
		reset(false)
		st, _, h := e.do("HEAD", path, "", "")
		if st != 200 || h.Get("Content-Length") != "6" || h.Get("Last-Modified") == "" || count("HEAD") != 1 || count("GET") != 0 {
			t.Fatalf("%s: %d %v, %v", path, st, h, asked)
		}
	}
	if assetsOf("raw-px") != 0 || assetsOf("mvn-px") != 0 {
		t.Fatal("a HEAD stored something")
	}

	// Held: from the cache.
	reset(false)
	e.want("GET", "/repository/raw-px/a.txt", "", "", 200)
	if count("GET") != 1 || assetsOf("raw-px") != 1 {
		t.Fatalf("GET: %v", asked)
	}
	reset(false)
	if st, _, _ := e.do("HEAD", "/repository/raw-px/a.txt", "", ""); st != 200 || count("HEAD")+count("GET") != 0 {
		t.Fatalf("held: %d %v", st, asked)
	}

	// A miss: the GET decides, and records it.
	reset(false)
	if st, _, _ := e.do("HEAD", "/repository/raw-px/missing.txt", "", ""); st != 404 || count("HEAD") != 1 || count("GET") != 1 {
		t.Fatalf("missing: %d %v", st, asked)
	}
	reset(false)
	if st, _, _ := e.do("HEAD", "/repository/raw-px/missing.txt", "", ""); st != 404 || count("HEAD")+count("GET") != 0 {
		t.Fatalf("recorded miss: %d %v", st, asked)
	}

	// A remote that takes no HEAD: as before, it is fetched and cached.
	reset(true)
	if st, _, _ := e.do("HEAD", "/repository/raw-px/c.txt", "", ""); st != 200 || count("GET") != 1 || assetsOf("raw-px") != 2 {
		t.Fatalf("no HEAD: %d %v", st, asked)
	}

	// A checksum reads the file: fetched, as before.
	reset(false)
	if st, _, _ := e.do("HEAD", "/repository/mvn-px/org/y/1.0/y-1.0.jar.sha1", "", ""); st != 200 || count("GET") != 1 {
		t.Fatalf("checksum: %d %v", st, asked)
	}
}
