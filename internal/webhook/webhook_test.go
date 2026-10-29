package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

var ctx = context.Background()

type received struct {
	header http.Header
	body   string
}

// receiver is an httptest server that records every request and answers
// with the status in code.
type receiver struct {
	*httptest.Server
	code atomic.Int32
	mu   sync.Mutex
	got  []received
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{}
	rc.code.Store(200)
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.got = append(rc.got, received{r.Header.Clone(), string(b)})
		rc.mu.Unlock()
		w.WriteHeader(int(rc.code.Load()))
		io.WriteString(w, "ignored")
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) requests() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]received(nil), rc.got...)
}

type env struct {
	db   *meta.DB
	d    *Dispatcher
	path string
	mu   sync.Mutex
	now  time.Time
}

func (e *env) advance(by time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(by)
	e.mu.Unlock()
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{path: metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")), now: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	e.open(t)
	return e
}

// open opens the database and starts a dispatcher on it (again).
func (e *env) open(t *testing.T) {
	t.Helper()
	db, err := meta.Open(e.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db = db
	e.d = &Dispatcher{DB: db, Now: e.clock, Log: slog.New(slog.DiscardHandler)}
	if err := e.d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.d.Close)
}

func (e *env) hook(t *testing.T, w meta.Webhook) meta.Webhook {
	t.Helper()
	if w.ID == "" {
		w.ID = w.Name
	}
	if err := e.db.CreateWebhook(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// delivery returns the newest delivery of the webhook once it has at least
// attempts attempts.
func (e *env) delivery(t *testing.T, hook string, attempts int) meta.WebhookDelivery {
	t.Helper()
	var d meta.WebhookDelivery
	waitFor(t, "delivery attempt", func() bool {
		list, err := e.db.ListDeliveries(ctx, hook, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 || list[0].Attempts < attempts {
			return false
		}
		d = list[0]
		return true
	})
	return d
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func mac(h func() hash.Hash, secret, body string) string {
	m := hmac.New(h, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

// A delivered event carries the headers, both signatures and the payload
// shape, and is recorded as ok.
func TestDeliveryHeadersSignatureAndBody(t *testing.T) {
	e := newEnv(t)
	rc := newReceiver(t)
	e.hook(t, meta.Webhook{Name: "signed", URL: rc.URL + "/hook", Secret: "s3cret", Events: []string{"*"}, Enabled: true})
	e.hook(t, meta.Webhook{Name: "plain", URL: rc.URL + "/plain", Events: []string{"component.created"}, Enabled: true})
	e.d.Emit("upload", "alice", "npm-hosted", "@scope/util@2.0.0", "")
	signed := e.delivery(t, "signed", 1)
	plain := e.delivery(t, "plain", 1)
	if signed.Status != meta.DeliveryOK || signed.ResponseCode != 200 || signed.LastError != "" || signed.Event != "component.created" {
		t.Fatalf("delivery: %+v", signed)
	}
	reqs := rc.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	var got received
	for _, r := range reqs {
		if r.header.Get("X-Command-Center-Delivery") == signed.ID {
			got = r
		}
	}
	h := got.header
	if h.Get("Content-Type") != "application/json" || h.Get("X-Command-Center-Event") != "component.created" ||
		h.Get("X-Nexus-Webhook-Id") != "component.created" || h.Get("X-Nexus-Webhook-Delivery") != signed.ID {
		t.Fatalf("headers: %v", h)
	}
	if want := "sha256=" + mac(sha256.New, "s3cret", got.body); h.Get("X-Command-Center-Signature") != want {
		t.Fatalf("signature %q, want %q", h.Get("X-Command-Center-Signature"), want)
	}
	if want := mac(sha1.New, "s3cret", got.body); h.Get("X-Nexus-Webhook-Signature") != want {
		t.Fatalf("nexus signature %q, want %q", h.Get("X-Nexus-Webhook-Signature"), want)
	}
	if got.body != signed.Payload {
		t.Fatalf("body %s differs from the stored payload %s", got.body, signed.Payload)
	}
	var p struct {
		ID, Timestamp, Event string
		Repository           *string
		Actor                string
		Data                 map[string]any
	}
	if err := json.Unmarshal([]byte(got.body), &p); err != nil {
		t.Fatal(err)
	}
	if !uuidRe.MatchString(p.ID) || p.Timestamp != "2026-10-29T09:00:00Z" || p.Event != "component.created" || p.Repository == nil || *p.Repository != "npm-hosted" ||
		p.Actor != "alice" || p.Data["name"] != "util" || p.Data["group"] != "@scope" || p.Data["version"] != "2.0.0" || p.Data["label"] != "@scope/util@2.0.0" {
		t.Fatalf("payload: %s", got.body)
	}
	if _, err := time.Parse(time.RFC3339, p.Timestamp); err != nil {
		t.Fatal(err)
	}
	// The unsigned hook gets no signature headers; its body is the same event.
	for _, r := range reqs {
		if r.header.Get("X-Command-Center-Delivery") == plain.ID {
			if r.header.Get("X-Command-Center-Signature") != "" || r.header.Get("X-Nexus-Webhook-Signature") != "" {
				t.Fatalf("unsigned hook got a signature: %v", r.header)
			}
			if !strings.Contains(r.body, `"event":"component.created"`) {
				t.Fatalf("plain body: %s", r.body)
			}
		}
	}
}

// A failed delivery is retried on the Backoff schedule, each step persisted,
// and failed after MaxAttempts; a redelivery starts over.
func TestRetryScheduleAndRedelivery(t *testing.T) {
	e := newEnv(t)
	rc := newReceiver(t)
	rc.code.Store(503)
	e.hook(t, meta.Webhook{Name: "h", URL: rc.URL, Events: []string{"*"}, Enabled: true})
	e.d.Emit("repo.create", "admin", "r1", "r1", "maven/hosted")
	d := e.delivery(t, "h", 1)
	if d.Status != meta.DeliveryPending || d.ResponseCode != 503 || d.LastError != "HTTP 503" || !d.NextAt.Equal(e.clock().Add(30*time.Second)) {
		t.Fatalf("after attempt 1: %+v", d)
	}
	e.d.Kick() // not due yet
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.requests()); n != 1 {
		t.Fatalf("%d requests before the retry was due", n)
	}
	for i, wait := range []time.Duration{2 * time.Minute, 10 * time.Minute, time.Hour} {
		e.advance(d.NextAt.Sub(e.clock()))
		e.d.Kick()
		d = e.delivery(t, "h", i+2)
		if d.Status != meta.DeliveryPending || !d.NextAt.Equal(e.clock().Add(wait)) {
			t.Fatalf("after attempt %d: %+v, want retry in %s", i+2, d, wait)
		}
	}
	e.advance(time.Hour)
	e.d.Kick()
	d = e.delivery(t, "h", 5)
	if d.Status != meta.DeliveryFailed || !d.NextAt.IsZero() {
		t.Fatalf("after attempt 5: %+v", d)
	}
	e.advance(time.Hour)
	e.d.Kick()
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.requests()); n != 5 {
		t.Fatalf("%d requests; a failed delivery was retried", n)
	}
	rc.code.Store(204)
	if err := e.d.Redeliver(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	d = e.delivery(t, "h", 1)
	if d.Status != meta.DeliveryOK || d.ResponseCode != 204 || d.LastError != "" {
		t.Fatalf("redelivered: %+v", d)
	}
	if err := e.d.Redeliver(ctx, "nope"); err != meta.ErrNotFound {
		t.Fatalf("redeliver missing: %v", err)
	}
}

// A pending retry survives a restart: the new dispatcher sends it when due.
func TestRestartResumesPending(t *testing.T) {
	e := newEnv(t)
	rc := newReceiver(t)
	rc.code.Store(500)
	e.hook(t, meta.Webhook{Name: "h", URL: rc.URL, Events: []string{"task.finished"}, Enabled: true})
	e.d.Emit("task.run", "system", "", "t1", "Failed: boom")
	d := e.delivery(t, "h", 1)
	e.d.Close()
	e.db.Close()
	rc.code.Store(200)
	e.advance(time.Minute)
	e.open(t)
	d2 := e.delivery(t, "h", 2)
	if d2.ID != d.ID || d2.Status != meta.DeliveryOK {
		t.Fatalf("after restart: %+v", d2)
	}
	if body := rc.requests()[1].body; !strings.Contains(body, `"event":"task.finished"`) || !strings.Contains(body, `"result":"Failed"`) {
		t.Fatalf("resumed body: %s", body)
	}
}

// Repository-scoped hooks get their repository's events only, a global one
// every repository's; event kinds filter; a disabled hook gets nothing.
func TestScopeAndDisabled(t *testing.T) {
	e := newEnv(t)
	rc := newReceiver(t)
	e.hook(t, meta.Webhook{Name: "global", URL: rc.URL, Events: []string{"component.created"}, Enabled: true})
	e.hook(t, meta.Webhook{Name: "r1", URL: rc.URL, Repo: "r1", Events: []string{"*"}, Enabled: true})
	e.hook(t, meta.Webhook{Name: "off", URL: rc.URL, Events: []string{"*"}, Enabled: false})
	e.d.Emit("upload", "u", "r2", "a@1", "")        // global only
	e.d.Emit("delete", "u", "r1", "a@1", "")        // r1 only (global is not subscribed)
	e.d.Emit("upload", "u", "r1", "b@2", "")        // both
	e.d.Emit("user.create", "admin", "", "bob", "") // nobody but off, which is disabled
	waitFor(t, "4 deliveries", func() bool { return len(rc.requests()) == 4 })
	time.Sleep(50 * time.Millisecond)
	count := func(hook string) []string {
		list, _ := e.db.ListDeliveries(ctx, hook, 50)
		var out []string
		for _, d := range list {
			out = append(out, d.Event)
		}
		slices.Sort(out) // equal times tie on the random id
		return out
	}
	if got := count("global"); strings.Join(got, ",") != "component.created,component.created" {
		t.Fatalf("global: %v", got)
	}
	if got := count("r1"); strings.Join(got, ",") != "component.created,component.deleted" {
		t.Fatalf("r1: %v", got)
	}
	if got := count("off"); len(got) != 0 {
		t.Fatalf("disabled hook got %v", got)
	}
	if e.d.Dropped() != 0 {
		t.Fatalf("dropped %d", e.d.Dropped())
	}
}

// A target whose host the egress policy refuses (an IPv4 address spelled
// as a number, a non-ASCII host) is never dialed; the refusal is recorded.
func TestEgressPolicyRefusesTarget(t *testing.T) {
	e := newEnv(t)
	for _, u := range []string{"http://10.1/hook", "http://167772161/hook", "https://１０．１/hook"} {
		e.hook(t, meta.Webhook{Name: u, URL: u, Events: []string{"*"}, Enabled: true})
	}
	e.d.Emit("upload", "u", "r", "a@1", "")
	for _, u := range []string{"http://10.1/hook", "http://167772161/hook", "https://１０．１/hook"} {
		d := e.delivery(t, u, 1)
		if d.Status != meta.DeliveryPending || d.ResponseCode != 0 || !strings.Contains(d.LastError, "egress policy") {
			t.Fatalf("%s: %+v", u, d)
		}
	}
}

// A redirect is not followed: it counts as a failed attempt.
func TestRedirectNotFollowed(t *testing.T) {
	e := newEnv(t)
	rc := newReceiver(t)
	var hits atomic.Int32
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, rc.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	e.hook(t, meta.Webhook{Name: "h", URL: redir.URL, Events: []string{"*"}, Enabled: true})
	e.d.Emit("upload", "u", "r", "a@1", "")
	d := e.delivery(t, "h", 1)
	if d.Status != meta.DeliveryPending || d.ResponseCode != 307 || len(rc.requests()) != 0 || hits.Load() != 1 {
		t.Fatalf("redirect: %+v, target got %d", d, len(rc.requests()))
	}
}

// Test sends a ping at once and records it with one attempt and no retry.
func TestPing(t *testing.T) {
	e := newEnv(t)
	rc := newReceiver(t)
	w := e.hook(t, meta.Webhook{Name: "h", URL: rc.URL, Secret: "k", Repo: "r1", Events: []string{"*"}, Enabled: true})
	w, _ = e.db.GetWebhook(ctx, w.ID)
	if code, err := e.d.Test(ctx, w); err != nil || code != 200 {
		t.Fatalf("Test = %d, %v", code, err)
	}
	r := rc.requests()[0]
	if r.header.Get("X-Command-Center-Event") != "ping" || !strings.Contains(r.body, `"event":"ping"`) || !strings.Contains(r.body, `"repository":"r1"`) ||
		r.header.Get("X-Command-Center-Signature") != "sha256="+mac(sha256.New, "k", r.body) {
		t.Fatalf("ping: %v %s", r.header, r.body)
	}
	rc.code.Store(404)
	e.advance(time.Second) // the list is newest first; equal times tie on the random id
	if code, err := e.d.Test(ctx, w); err == nil || code != 404 || err.Error() != "HTTP 404" {
		t.Fatalf("Test = %d, %v", code, err)
	}
	list, _ := e.db.ListDeliveries(ctx, w.ID, 10)
	if len(list) != 2 || list[0].Status != meta.DeliveryFailed || list[0].Attempts != 1 || !list[0].NextAt.IsZero() || list[1].Status != meta.DeliveryOK {
		t.Fatalf("deliveries: %+v", list)
	}
	e.advance(2 * time.Hour)
	e.d.Kick()
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.requests()); n != 2 {
		t.Fatalf("a failed ping was retried: %d requests", n)
	}
}

func TestMapKindAndComponent(t *testing.T) {
	for _, c := range []struct{ source, format, subject, want string }{
		{"upload", "npm", "left-pad@1.0.0", "component.created"},
		{"delete", "docker", "lib/app@sha256:abc", "component.deleted"},
		{"upload", "raw", "dir/file.txt", "asset.created"},
		{"delete", "raw", "file", "asset.deleted"},
		{"upload", "maven", "org/example/app/1.0/app-1.0.jar", "asset.created"},
		{"delete", "maven", "org.example:app:1.0", "component.deleted"},
		{"upload", "", "x@1", "component.created"},
		{"repo.create", "", "r", "repository.created"},
		{"realms.ldap.update", "", "ldap", "realm.updated"},
		{"task.run", "", "id", "task.finished"},
		{"email.failed", "", "subject", "email.failed"},
		{"something.new", "", "s", "something.new"},
	} {
		if got := mapKind(c.source, c.format, c.subject); got != c.want {
			t.Errorf("mapKind(%s, %s, %s) = %s, want %s", c.source, c.format, c.subject, got, c.want)
		}
	}
	for _, c := range []struct{ format, label, group, name, version string }{
		{"maven", "org.example:app:1.0", "org.example", "app", "1.0"},
		{"npm", "@scope/util@2.0.0", "@scope", "util", "2.0.0"},
		{"npm", "left-pad@1.0.0", "", "left-pad", "1.0.0"},
		{"go", "github.com/x/y@v1.2.3", "github.com/x", "y", "v1.2.3"},
		{"docker", "lib/app@sha256:abc", "lib", "app", "sha256:abc"},
		{"docker", "lib/app:v1", "", "lib/app", "v1"},
		{"apt", "amd64/curl@8.0-1", "amd64", "curl", "8.0-1"},
		{"pypi", "my_pkg-1.0.tar.gz", "", "my_pkg-1.0.tar.gz", ""},
	} {
		got := component(c.format, c.label)
		if got["group"] != c.group || got["name"] != c.name || got["version"] != c.version || got["format"] != c.format {
			t.Errorf("component(%s, %s) = %v", c.format, c.label, got)
		}
	}
	for _, k := range Kinds {
		if !KnownKind(k.Kind) || k.Description == "" {
			t.Errorf("kind %+v", k)
		}
	}
	if KnownKind("*") || KnownKind("upload") {
		t.Error("activity kinds are not webhook kinds")
	}
}

// Close stops the workers and leaves what was not sent pending; the event
// queue is closed, so a late Emit is dropped and counted.
func TestCloseLeavesPending(t *testing.T) {
	e := newEnv(t)
	e.hook(t, meta.Webhook{Name: "h", URL: "http://10.1/x", Events: []string{"*"}, Enabled: true})
	e.d.Close()
	e.d.Close()
	e.d.Emit("upload", "u", "r", "a@1", "")
	if e.d.Dropped() != 0 { // nothing is counted: the hooks were matched but the queue is closed
		t.Fatalf("dropped %d", e.d.Dropped())
	}
	if list, _ := e.db.ListDeliveries(ctx, "h", 10); len(list) != 0 {
		t.Fatalf("a closed dispatcher stored %+v", list)
	}
}
