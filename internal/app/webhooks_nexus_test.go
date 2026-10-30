package app

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/webhook"
)

type hookReq struct {
	path   string
	header http.Header
	body   string
	m      map[string]any
}

// hookReceiver records every webhook request it gets.
type hookReceiver struct {
	*httptest.Server
	mu  sync.Mutex
	got []hookReq
}

func newHookReceiver(t *testing.T) *hookReceiver {
	rc := &hookReceiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		rc.mu.Lock()
		rc.got = append(rc.got, hookReq{r.URL.Path, r.Header.Clone(), string(b), m})
		rc.mu.Unlock()
	}))
	t.Cleanup(rc.Close)
	return rc
}

// await waits up to 10 s for n requests to path that match, and returns them.
func (rc *hookReceiver) await(t *testing.T, path string, n int, match func(hookReq) bool) []hookReq {
	t.Helper()
	var out []hookReq
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		out = out[:0]
		rc.mu.Lock()
		for _, r := range rc.got {
			if r.path == path && match(r) {
				out = append(out, r)
			}
		}
		rc.mu.Unlock()
		if len(out) >= n {
			return out
		}
	}
	t.Fatalf("%s: got %d matching requests, want %d; all: %s", path, len(out), n, rc.dump())
	return nil
}

func (rc *hookReceiver) dump() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var b strings.Builder
	for _, r := range rc.got {
		b.WriteString("\n  " + r.header.Get("X-Nexus-Webhook-Id") + " " + r.body)
	}
	return b.String()
}

// get reads a dotted path of nested objects from m.
func get(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		o, _ := v.(map[string]any)
		v = o[k]
	}
	return v
}

func keys(v any) string {
	o, _ := v.(map[string]any)
	var ks []string
	for k := range o {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return strings.Join(ks, ",")
}

var nexusTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}\+00:00$`)

// Webhooks of payload format nexus end to end: the four Nexus event types
// with their X-Nexus-Webhook-ID, payload shapes and HMAC-SHA1 signature;
// component and asset events from hosted uploads, an overwrite, a delete and
// npm proxy caching; a commandcenter webhook beside them unchanged.
func TestNexusWebhooks(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tgz") {
			io.WriteString(w, "content of "+r.URL.Path)
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	rc := newHookReceiver(t)
	poll := webhook.PollEvery
	webhook.PollEvery = 20 * time.Millisecond
	t.Cleanup(func() { webhook.PollEvery = poll })
	a, ts := usageApp(t, `
  - { name: raw-hosted, format: raw, type: hosted, writePolicy: allow }
  - { name: npm-proxy, format: npm, type: proxy, remote: "`+up.URL+`/" }
`)
	a.Webhooks.Deliver()

	// The webhook API: the payload format, the Nexus names it lists.
	hook := func(name, body string) string {
		t.Helper()
		out := mustDo(t, 201, "POST", ts.URL+"/-/api/webhooks", "root", body, true)
		var h struct{ ID, PayloadFormat string }
		json.Unmarshal([]byte(out), &h)
		return h.ID
	}
	url := func(p string) string { return rc.URL + "/" + p }
	for _, bad := range []string{
		`{"name":"x","url":"` + url("x") + `","events":["component.created"],"payloadFormat":"nexus"}`,
		`{"name":"x","url":"` + url("x") + `","events":["*"],"payloadFormat":"nexus"}`,
		`{"name":"x","url":"` + url("x") + `","events":["audit"],"payloadFormat":"jenkins"}`,
	} {
		mustDo(t, 400, "POST", ts.URL+"/-/api/webhooks", "root", bad, true)
	}
	events := mustDo(t, 200, "GET", ts.URL+"/-/api/webhooks/events", "root", "", false)
	if !strings.Contains(events, `"nexusEvents":[{"kind":"audit"`) {
		t.Fatalf("events: %s", events)
	}
	const secret = "s3cret"
	global := hook("global", `{"name":"global","url":"`+url("global")+`","secret":"`+secret+`","events":["repository","audit"],"payloadFormat":"nexus"}`)
	hook("raw", `{"name":"raw","url":"`+url("raw")+`","secret":"`+secret+`","events":["component","asset"],"repo":"raw-hosted","payloadFormat":"nexus"}`)
	hook("npm", `{"name":"npm","url":"`+url("npm")+`","secret":"`+secret+`","events":["component","asset"],"repo":"npm-proxy","payloadFormat":"nexus"}`)
	hook("maven", `{"name":"maven","url":"`+url("maven")+`","secret":"`+secret+`","events":["component"],"repo":"maven-releases","payloadFormat":"nexus"}`)
	legacy := hook("legacy", `{"name":"legacy","url":"`+url("legacy")+`","events":["asset.created"],"repo":"raw-hosted"}`)
	hook("alias", `{"name":"alias","url":"`+url("alias")+`","events":["asset"],"repo":"maven-releases"}`)
	// A PUT without payloadFormat keeps the stored one.
	out := mustDo(t, 200, "PUT", ts.URL+"/-/api/webhooks/"+global, "root",
		`{"name":"global","url":"`+url("global")+`","events":["repository","audit"]}`, true)
	if !strings.Contains(out, `"payloadFormat":"nexus"`) {
		t.Fatalf("PUT without payloadFormat: %s", out)
	}
	if out := mustDo(t, 200, "GET", ts.URL+"/-/api/webhooks/"+legacy, "root", "", false); !strings.Contains(out, `"payloadFormat":"commandcenter"`) {
		t.Fatalf("default payload format: %s", out)
	}

	// Every nexus delivery: its id header, signature and common fields.
	checkCommon := func(r hookReq, id string, timestamp bool) {
		t.Helper()
		if got := r.header.Get("X-Nexus-Webhook-Id"); got != id {
			t.Fatalf("X-Nexus-Webhook-ID %q, want %q: %s", got, id, r.body)
		}
		m := hmac.New(sha1.New, []byte(secret))
		m.Write([]byte(r.body))
		if got, want := r.header.Get("X-Nexus-Webhook-Signature"), hex.EncodeToString(m.Sum(nil)); got != want {
			t.Fatalf("signature %q, want %q", got, want)
		}
		if r.header.Get("X-Nexus-Webhook-Delivery") == "" || r.header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers: %v", r.header)
		}
		if ts, _ := r.m["timestamp"].(string); timestamp != (ts != "") || timestamp && !nexusTimestamp.MatchString(ts) {
			t.Fatalf("timestamp %q (want one: %v): %s", ts, timestamp, r.body)
		}
		if r.m["nodeId"] == "" || r.m["nodeId"] == nil {
			t.Fatalf("no nodeId: %s", r.body)
		}
	}
	action := func(want string) func(hookReq) bool {
		return func(r hookReq) bool { return r.m["action"] == want }
	}
	both := func(fs ...func(hookReq) bool) func(hookReq) bool {
		return func(r hookReq) bool {
			for _, f := range fs {
				if !f(r) {
					return false
				}
			}
			return true
		}
	}
	isID := func(id string) func(hookReq) bool {
		return func(r hookReq) bool { return r.header.Get("X-Nexus-Webhook-Id") == id }
	}
	// Global repository: created and deleted, with the repository's format,
	// name and type; and its audit record.
	mustDo(t, 201, "POST", ts.URL+"/-/api/repositories", "root", `{"name":"maven-new","format":"maven","type":"hosted"}`, true)
	mustDo(t, 204, "DELETE", ts.URL+"/-/api/repositories/maven-new", "root", "", true)
	for _, act := range []string{"CREATED", "DELETED"} {
		r := rc.await(t, "/global", 1, both(isID("rm:global:repository"), action(act)))[0]
		checkCommon(r, "rm:global:repository", true)
		if keys(r.m) != "action,initiator,nodeId,repository,timestamp" || keys(r.m["repository"]) != "format,name,type" ||
			get(r.m, "repository", "format") != "maven2" || get(r.m, "repository", "name") != "maven-new" || get(r.m, "repository", "type") != "hosted" ||
			r.m["initiator"] != "root/127.0.0.1" {
			t.Fatalf("repository %s: %s", act, r.body)
		}
	}
	r := rc.await(t, "/global", 1, both(isID("rm:global:audit"), func(r hookReq) bool { return get(r.m, "audit", "domain") == "repository" }))[0]
	checkCommon(r, "rm:global:audit", false)
	if keys(r.m) != "audit,initiator,nodeId" || keys(r.m["audit"]) != "attributes,context,domain,type" || get(r.m, "audit", "context") != "maven-new" {
		t.Fatalf("audit: %s", r.body)
	}
	mustDo(t, 201, "POST", ts.URL+"/-/api/users", "root", `{"id":"newbie","password":"longenough","roles":["admin"]}`, true)
	rc.await(t, "/global", 1, func(r hookReq) bool {
		return get(r.m, "audit", "domain") == "security.user" && get(r.m, "audit", "type") == "created" && get(r.m, "audit", "context") == "newbie"
	})

	// Raw hosted: created; the same content again is no event; new content
	// updates; a delete deletes. The component of a raw file is the file.
	file := ts.URL + "/repository/raw-hosted/docs/notes.txt"
	mustDo(t, 201, "PUT", file, "root", "v1", false)
	mustDo(t, 201, "PUT", file, "root", "v1", false)
	mustDo(t, 201, "PUT", file, "root", "v2", false)
	mustDo(t, 204, "DELETE", file, "root", "", false)
	for _, act := range []string{"CREATED", "UPDATED", "DELETED"} {
		r := rc.await(t, "/raw", 1, both(isID("rm:repository:asset"), action(act)))[0]
		checkCommon(r, "rm:repository:asset", true)
		if keys(r.m) != "action,asset,initiator,nodeId,repositoryName,timestamp" || keys(r.m["asset"]) != "assetId,format,id,name" ||
			r.m["repositoryName"] != "raw-hosted" || get(r.m, "asset", "format") != "raw" || get(r.m, "asset", "name") != "/docs/notes.txt" ||
			r.m["initiator"] != "root/127.0.0.1" || len(get(r.m, "asset", "id").(string)) != 32 {
			t.Fatalf("raw asset %s: %s", act, r.body)
		}
		c := rc.await(t, "/raw", 1, both(isID("rm:repository:component"), action(act)))[0]
		checkCommon(c, "rm:repository:component", true)
		if keys(c.m) != "action,component,initiator,nodeId,repositoryName,timestamp" ||
			keys(c.m["component"]) != "componentId,format,group,id,name,version" ||
			get(c.m, "component", "name") != "/docs/notes.txt" || get(c.m, "component", "group") != "/docs" || get(c.m, "component", "version") != "" {
			t.Fatalf("raw component %s: %s", act, c.body)
		}
	}
	if n := len(rc.await(t, "/raw", 0, func(hookReq) bool { return true })); n != 6 {
		t.Fatalf("raw: %d deliveries, want 6 (an unchanged put is none): %s", n, rc.dump())
	}
	// The commandcenter webhook: its envelope and kinds as before, an
	// upload each time.
	legacyGot := rc.await(t, "/legacy", 3, func(hookReq) bool { return true })
	for _, r := range legacyGot {
		if r.m["event"] != "asset.created" || r.header.Get("X-Nexus-Webhook-Id") != "asset.created" || get(r.m, "data", "path") != "docs/notes.txt" {
			t.Fatalf("commandcenter delivery: %v %s", r.header, r.body)
		}
	}

	// Maven: the first file creates the component, the next updates it.
	const jar = "/repository/maven-releases/org/example/app/1.0/app-1.0"
	mustDo(t, 201, "PUT", ts.URL+jar+".jar", "ci", "jar", false)
	mustDo(t, 201, "PUT", ts.URL+jar+".pom", "ci", pomOf("org.example", "app", "1.0"), false)
	created := rc.await(t, "/maven", 1, action("CREATED"))[0]
	rc.await(t, "/maven", 1, action("UPDATED"))
	if get(created.m, "component", "format") != "maven2" || get(created.m, "component", "group") != "org.example" ||
		get(created.m, "component", "name") != "app" || get(created.m, "component", "version") != "1.0" || created.m["initiator"] != "ci/127.0.0.1" {
		t.Fatalf("maven component: %s", created.body)
	}
	id, _ := get(created.m, "component", "componentId").(string)
	if out := mustDo(t, 200, "GET", ts.URL+"/service/rest/v1/components/"+id, "root", "", false); !strings.Contains(out, `"name":"app"`) {
		t.Fatalf("the componentId is the REST id: %s", out)
	}
	// The Nexus name "asset" in a commandcenter webhook is an alias of
	// asset.*: the Maven files as before, in the commandcenter envelope.
	for _, r := range rc.await(t, "/alias", 2, func(r hookReq) bool { return r.m["event"] == "asset.created" }) {
		if r.header.Get("X-Nexus-Webhook-Id") != "asset.created" || r.m["repository"] != "maven-releases" {
			t.Fatalf("alias: %s", r.body)
		}
	}

	// npm proxy caching: the component and the tarball, by the anonymous
	// client.
	if st, body := do(t, "GET", ts.URL+"/repository/npm-proxy/lodash/-/lodash-4.17.21.tgz", "", "", false); st != 200 {
		t.Fatalf("npm proxy: %d %s", st, body)
	}
	c := rc.await(t, "/npm", 1, isID("rm:repository:component"))[0]
	if c.m["action"] != "CREATED" || get(c.m, "component", "format") != "npm" || get(c.m, "component", "name") != "lodash" ||
		get(c.m, "component", "version") != "4.17.21" || c.m["initiator"] != "anonymous/127.0.0.1" || c.m["repositoryName"] != "npm-proxy" {
		t.Fatalf("npm proxy component: %s", c.body)
	}
	as := rc.await(t, "/npm", 1, isID("rm:repository:asset"))[0]
	if as.m["action"] != "CREATED" || get(as.m, "asset", "name") != "/lodash/-/lodash-4.17.21.tgz" {
		t.Fatalf("npm proxy asset: %s", as.body)
	}
	aid, _ := get(as.m, "asset", "assetId").(string)
	mustDo(t, 200, "GET", ts.URL+"/service/rest/v1/assets/"+aid, "root", "", false)
	// The global audit webhook has the content records too.
	rc.await(t, "/global", 1, func(r hookReq) bool {
		return get(r.m, "audit", "domain") == "repository.component" && get(r.m, "audit", "context") == "lodash" &&
			get(r.m, "audit", "type") == "created" && get(r.m, "audit", "attributes", "repository.name") == "npm-proxy"
	})

	// The test button of a nexus webhook: an audit record.
	out = mustDo(t, 200, "POST", ts.URL+"/-/api/webhooks/"+global+"/test", "root", "", true)
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("test: %s", out)
	}
	p := rc.await(t, "/global", 1, func(r hookReq) bool { return get(r.m, "audit", "type") == "ping" })[0]
	checkCommon(p, "rm:global:audit", false)
}

// Sign-ins are recorded (auth.login, auth.failed, auth.logout), and every
// event a request causes names the client's address and the node.
func TestSignInEventsAndAddress(t *testing.T) {
	_, ts := usageApp(t, "")
	login := func(pw string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/-/api/login", strings.NewReader(`{"user":"root","password":"`+pw+`"}`))
		req.Header.Set("X-Command-Center-UI", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := login("wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp := login("pw")
	if resp.StatusCode != 200 || len(resp.Cookies()) == 0 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/-/api/logout", nil)
	req.Header.Set("X-Command-Center-UI", "1")
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	if out, err := http.DefaultClient.Do(req); err != nil || out.StatusCode != 204 {
		t.Fatalf("logout: %v %v", out, err)
	}
	type ev struct{ Kind, Actor, Subject, IP, Node string }
	var got []ev
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var page struct{ Events []ev }
		json.Unmarshal([]byte(mustDo(t, 200, "GET", ts.URL+"/-/api/events", "root", "", false)), &page)
		got = page.Events
		if len(got) >= 3 && got[0].Kind == "auth.logout" {
			break
		}
	}
	want := []ev{{"auth.logout", "root", "root", "127.0.0.1", ""}, {"auth.login", "root", "root", "127.0.0.1", ""}, {"auth.failed", "", "root", "127.0.0.1", ""}}
	if len(got) < 3 {
		t.Fatalf("events: %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.Node == "" {
			t.Fatalf("event %+v names no node", g)
		}
		if g.Node = ""; g != w {
			t.Fatalf("event %d: %+v, want %+v", i, g, w)
		}
	}
}
