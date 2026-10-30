// Package webhook delivers activity events to the configured HTTP endpoints
// (Nexus Repository's repository and global webhooks). The Dispatcher is an
// EventSink in front of the activity recorder: every event the server
// records, and every commit of content rows (Changes, for the webhooks of
// payload format nexus), is matched against the enabled webhooks, stored
// as one delivery per webhook and sent by a pool of workers, with retries
// persisted in webhook_deliveries so a restart resumes them. A worker
// sends only what its node claimed in the table, so each delivery is sent
// once however many nodes (and CLI commands, which store but never send)
// share the database; at least once when a node dies mid-send.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

const (
	// QueueSize bounds the events waiting to be stored and the deliveries
	// waiting for a worker.
	QueueSize = 1000
	// Workers is how many deliveries are sent at once.
	Workers = 4
	// RequestTimeout bounds one delivery request.
	RequestTimeout = 10 * time.Second
	// MaxAttempts is how often a delivery is tried before it is failed.
	MaxAttempts = 5
	// CloseTimeout bounds how long Close waits for in-flight deliveries;
	// the rest stay pending for the next start.
	CloseTimeout = 5 * time.Second

	maxField   = 128
	maxBatch   = 50 // events stored in one transaction
	maxSubject = 512
	maxDetail  = 1024
	drainBody  = 4 << 10
	warnEvery  = time.Minute
)

// Backoff is the wait before the second, third, ... attempt.
var Backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour}

// PollEvery is how often the pending deliveries are read when nothing
// kicks the dispatcher; a variable for tests.
var PollEvery = 10 * time.Second

// ClaimFor is how long a delivery a node claimed stays its own to send
// (meta.DB.ClaimDeliveries): a node that dies holding claims leaves those
// deliveries to the others after it. A variable for tests.
var ClaimFor = time.Minute

// maxClaimed bounds the deliveries a node has claimed and not yet sent, so
// that each is sent long before its claim ends: three rounds of the workers
// at RequestTimeout each take 30 s.
const maxClaimed = 3 * Workers

// Dispatcher matches events against the webhooks and delivers them. Set the
// fields, then Start, and Deliver in a process that serves; Close stops it.
// Emit never blocks.
type Dispatcher struct {
	DB    *meta.DB
	Next  repo.EventSink // the activity recorder; nil records nothing
	Repos *repo.Registry // repository formats for the payload; nil leaves them empty
	Log   *slog.Logger
	Node  string           // this process's node id, the nexus payloads' nodeId; "" leaves it out
	Now   func() time.Time // test hook

	mu      sync.RWMutex
	hooks   []meta.Webhook // enabled only, secrets opened
	clients map[string]*http.Client
	closed  bool

	events   chan event
	work     chan meta.WebhookDelivery
	kick     chan struct{}
	stop     chan struct{}
	node     string       // the claimer: this node's id, set by Deliver
	claimed  atomic.Int64 // claimed deliveries not yet attempted
	wg       sync.WaitGroup
	dropped  atomic.Int64
	lastWarn atomic.Int64
}

// Start loads the enabled webhooks and starts the intake, which stores the
// events as deliveries.
func (d *Dispatcher) Start() error {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	d.clients = map[string]*http.Client{}
	d.events = make(chan event, QueueSize)
	d.work = make(chan meta.WebhookDelivery, QueueSize)
	d.kick = make(chan struct{}, 1)
	d.stop = make(chan struct{})
	if err := d.Reload(context.Background()); err != nil {
		return err
	}
	d.wg.Add(1)
	go d.intake()
	return nil
}

// Deliver starts the poller and the workers, which claim the due
// deliveries and send them. Only a serving process calls it (app.Serve):
// a CLI command beside a server leaves what it stores to the server. Call
// it once, after Start.
func (d *Dispatcher) Deliver() {
	d.node = d.DB.NodeID()
	if d.node == "" { // a process that did not join as a node (tests)
		d.node = uuid()
	}
	d.wg.Add(1 + Workers)
	go d.poll()
	for i := 0; i < Workers; i++ {
		go d.worker()
	}
	d.Kick()
}

// Reload reads the webhooks again; the admin API calls it after a change.
func (d *Dispatcher) Reload(ctx context.Context) error {
	all, err := d.DB.ListWebhooks(ctx)
	if err != nil {
		return fmt.Errorf("load webhooks: %w", err)
	}
	var hooks []meta.Webhook
	urls := map[string]bool{}
	for _, w := range all {
		if w.Enabled {
			hooks = append(hooks, w)
			urls[w.URL] = true
		}
	}
	d.mu.Lock()
	d.hooks = hooks
	for u, c := range d.clients {
		if !urls[u] {
			c.CloseIdleConnections()
			delete(d.clients, u)
		}
	}
	d.mu.Unlock()
	d.Kick()
	return nil
}

// Kick makes the poller read the due deliveries now.
func (d *Dispatcher) Kick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Dropped is the number of events lost because the queue was full.
func (d *Dispatcher) Dropped() int64 { return d.dropped.Load() }

// Emit forwards the activity event to Next and queues it for the webhooks
// that subscribe to it. It never blocks: when the queue is full the event is
// dropped (and counted) for the webhooks only.
func (d *Dispatcher) Emit(kind, actor, repoName, subject, detail string) {
	if d.Next != nil {
		d.Next.Emit(kind, actor, repoName, subject, detail)
	}
	d.activity(kind, actor, "", repoName, subject, detail)
}

// EmitCtx is Emit for an event a request caused: the nexus payloads name
// the client's address in their initiator (repo.CtxSink).
func (d *Dispatcher) EmitCtx(ctx context.Context, kind, actor, repoName, subject, detail string) {
	if d.Next != nil {
		repo.EmitContext(ctx, d.Next, kind, actor, repoName, subject, detail)
	}
	d.activity(kind, actor, auth.ClientIP(ctx), repoName, subject, detail)
}

func (d *Dispatcher) activity(kind, actor, ip, repoName, subject, detail string) {
	e := event{source: kind, actor: clip(actor, maxField), ip: clip(ip, maxField), repo: clip(repoName, maxField),
		subject: clip(subject, maxSubject), detail: clip(detail, maxDetail)}
	if repoName != "" {
		e.format = d.format(repoName)
	}
	e.kind = mapKind(kind, e.format, e.subject)
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.closed && slices.ContainsFunc(d.hooks, func(w meta.Webhook) bool {
		return matches(w, e.kind, e.repo) || len(nexusWants(w, e.repo, activityNames(e)...)) > 0
	}) {
		d.enqueue(e)
	}
}

// Changes queues the component and asset events of the content rows one
// commit wrote (meta.DB.OnCommit) for the nexus webhooks that subscribe to
// them or to audit, with the initiator of ctx, the committing request's.
// Whether a component was created, updated or deleted is read here, on the
// committing goroutine, before the writer can change the component again;
// it does not wait for the queue.
func (d *Dispatcher) Changes(ctx context.Context, at time.Time, cs []meta.Change) {
	d.mu.RLock()
	hooks, closed := d.hooks, d.closed // Reload replaces the slice, never changes it
	d.mu.RUnlock()
	wants := func(repo string, names ...string) bool {
		return slices.ContainsFunc(hooks, func(w meta.Webhook) bool { return len(nexusWants(w, repo, names...)) > 0 })
	}
	if closed || !slices.ContainsFunc(cs, func(c meta.Change) bool { return wants(c.Repo, "component", "asset", "audit") }) {
		return
	}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RequestTimeout)
	defer cancel()
	list := d.contents(qctx, cs, func(repo string) bool { return wants(repo, "component", "audit") })
	if len(list) == 0 {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.closed {
		d.enqueue(event{contents: list, at: at, actor: clip(auth.FromContext(ctx).Owner(), maxField), ip: clip(auth.ClientIP(ctx), maxField)})
	}
}

// enqueue queues e for intake, or drops and counts it when the queue is
// full. The caller holds mu.
func (d *Dispatcher) enqueue(e event) {
	select {
	case d.events <- e:
	default:
		n := d.dropped.Add(1)
		now := time.Now().UnixNano()
		if last := d.lastWarn.Load(); now-last >= int64(warnEvery) && d.lastWarn.CompareAndSwap(last, now) {
			d.Log.Warn("webhook event queue is full: events are not delivered", "dropped", n)
		}
	}
}

// Payload is the body of a delivery.
type Payload struct {
	ID         string         `json:"id"`
	Timestamp  string         `json:"timestamp"`
	Event      string         `json:"event"`
	Repository *string        `json:"repository"`
	Actor      string         `json:"actor"`
	Data       map[string]any `json:"data"`
}

// intake stores one delivery per subscribed webhook and event name for
// each queued event, the events queued together in one transaction.
func (d *Dispatcher) intake() {
	defer d.wg.Done()
	for e := range d.events {
		batch := []event{e}
	fill:
		for len(batch) < maxBatch {
			select {
			case e, ok := <-d.events:
				if !ok {
					break fill
				}
				batch = append(batch, e)
			default:
				break fill
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		var dls []meta.WebhookDelivery
		for _, e := range batch {
			dls = append(dls, d.deliveries(ctx, e)...)
		}
		if err := d.DB.InsertDeliveries(ctx, dls); err != nil {
			d.dropped.Add(int64(len(batch)))
			d.Log.Warn("could not store webhook deliveries", "events", len(batch), "err", logsafe.Err(err))
		} else if len(dls) > 0 {
			d.Kick()
		}
		cancel()
	}
}

// deliveries returns the deliveries of e to the webhooks: a commandcenter
// webhook gets the event once, with the event kind; a nexus one gets one
// delivery per Nexus event name it lists that e reaches, with the
// rm:<scope>:<name> id.
func (d *Dispatcher) deliveries(ctx context.Context, e event) []meta.WebhookDelivery {
	d.mu.RLock()
	hooks := d.hooks // Reload replaces the slice, never changes it
	d.mu.RUnlock()
	now := d.Now()
	at := e.at
	if at.IsZero() {
		at = now
	}
	head := nexusHead{Timestamp: nexusTime(at), NodeID: d.Node, Initiator: initiator(e.actor, e.ip)}
	var dls []meta.WebhookDelivery
	add := func(w meta.Webhook, kind, body string) {
		if len(body) > meta.MaxDeliveryPayload {
			d.dropped.Add(1)
			d.Log.Warn("a webhook payload is too large to deliver", "webhook", w.Name, "event", kind, "bytes", len(body))
			return
		}
		dls = append(dls, meta.WebhookDelivery{ID: uuid(), WebhookID: w.ID, Event: kind, Status: meta.DeliveryPending,
			Created: now, NextAt: now, Payload: body})
	}
	bodies := map[string]string{}
	if e.contents != nil {
		for _, c := range e.contents {
			clear(bodies)
			for _, w := range hooks {
				for _, n := range nexusWants(w, c.repo, c.typ, "audit") {
					if bodies[n] == "" {
						bodies[n] = marshal(c.body(n, head))
					}
					add(w, nexusID[n], bodies[n])
				}
			}
		}
		return dls
	}
	var data map[string]any
	for _, w := range hooks {
		names := nexusWants(w, e.repo, activityNames(e)...)
		cc := matches(w, e.kind, e.repo)
		if data == nil && (cc || len(names) > 0) {
			data = d.data(ctx, e)
		}
		if cc {
			if bodies[""] == "" {
				bodies[""] = ccBody(e, data, now)
			}
			add(w, e.kind, bodies[""])
		}
		for _, n := range names {
			if bodies[n] == "" {
				bodies[n] = marshal(activityBody(n, e, data, head))
			}
			add(w, nexusID[n], bodies[n])
		}
	}
	return dls
}

// ccBody is the commandcenter payload of e.
func ccBody(e event, data map[string]any, now time.Time) string {
	p := Payload{ID: uuid(), Timestamp: now.UTC().Format(time.RFC3339), Event: e.kind, Actor: e.actor, Data: data}
	if e.repo != "" {
		p.Repository = &e.repo
	}
	body, err := json.Marshal(p)
	if err != nil || len(body) > meta.MaxDeliveryPayload {
		p.Data = map[string]any{"subject": e.subject} // the envelope alone
		body, _ = json.Marshal(p)
	}
	return string(body)
}

// poll claims due deliveries for the workers, up to maxClaimed at a time,
// on a kick (a worker kicks after each attempt) and every PollEvery (a
// retry's time comes without a kick).
func (d *Dispatcher) poll() {
	defer d.wg.Done()
	t := time.NewTicker(PollEvery)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-d.kick:
		case <-t.C:
		}
		n := maxClaimed - int(d.claimed.Load())
		if n <= 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		now := d.Now()
		due, err := d.DB.ClaimDeliveries(ctx, d.node, now, now.Add(ClaimFor), n)
		cancel()
		if err != nil {
			d.Log.Warn("could not claim pending webhook deliveries", "err", logsafe.Err(err))
			continue
		}
		d.claimed.Add(int64(len(due)))
		for _, dl := range due {
			select {
			case d.work <- dl:
			case <-d.stop:
				return
			}
		}
	}
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.stop:
			return
		case dl := <-d.work:
			d.attempt(dl)
			d.claimed.Add(-1)
			d.Kick()
		}
	}
}

// hook returns the enabled webhook with id, if any.
func (d *Dispatcher) hook(id string) (meta.Webhook, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, w := range d.hooks {
		if w.ID == id {
			return w, true
		}
	}
	return meta.Webhook{}, false
}

// attempt sends dl once and records the outcome: ok on a 2xx, else a retry
// after the Backoff for the attempt, or failed after MaxAttempts.
func (d *Dispatcher) attempt(dl meta.WebhookDelivery) {
	w, ok := d.hook(dl.WebhookID)
	if !ok { // disabled or deleted since it was read: left pending
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	// A copy read before an earlier attempt was recorded, or whose claim
	// ended and was taken again, is not sent: only while the row is still
	// pending at the attempt count and under the claim it was read with.
	if cur, err := d.DB.GetDelivery(ctx, dl.ID); err != nil || cur.Status != meta.DeliveryPending || cur.Attempts != dl.Attempts ||
		cur.ClaimedBy != dl.ClaimedBy || !cur.ClaimUntil.Equal(dl.ClaimUntil) {
		return
	}
	code, err := d.send(ctx, w, dl)
	attempts := dl.Attempts + 1
	status, errText, next := meta.DeliveryOK, "", time.Time{}
	if err != nil {
		errText = clip(logsafe.Err(err), maxDetail)
		status = meta.DeliveryFailed
		if attempts < MaxAttempts {
			status = meta.DeliveryPending
			next = d.Now().Add(Backoff[min(attempts, len(Backoff))-1])
		}
		d.Log.Warn("webhook delivery failed", "webhook", w.Name, "id", w.ID, "delivery", dl.ID, "event", dl.Event,
			"attempt", attempts, "status", code, "err", errText, "retry", status == meta.DeliveryPending)
	}
	rctx, rcancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer rcancel()
	if err := d.DB.RecordDeliveryAttempt(rctx, dl.ID, status, attempts, code, errText, next); err != nil {
		d.Log.Warn("could not record a webhook delivery attempt", "delivery", dl.ID, "err", logsafe.Err(err))
	}
}

// client returns the HTTP client for u: NewClientFor's egress policy (the
// target's own address class and public ones; numeric and non-ASCII host
// spellings refused), RequestTimeout, redirects not followed.
func (d *Dispatcher) client(rawurl string) (*http.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.clients[rawurl]; c != nil {
		return c, nil
	}
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	c := proxy.NewClientFor(u, nil)
	c.Timeout = RequestTimeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	d.clients[rawurl] = c
	return c, nil
}

// send posts dl's payload to w and returns the response status; a status
// outside 2xx is an error. The response body is drained and ignored.
func (d *Dispatcher) send(ctx context.Context, w meta.Webhook, dl meta.WebhookDelivery) (int, error) {
	c, err := d.client(w.URL)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader([]byte(dl.Payload)))
	if err != nil {
		return 0, err
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "CommandCenter-Webhook")
	h.Set("X-Command-Center-Event", dl.Event)
	h.Set("X-Command-Center-Delivery", dl.ID)
	h.Set("X-Nexus-Webhook-Id", dl.Event)
	h.Set("X-Nexus-Webhook-Delivery", dl.ID)
	if w.Secret != "" {
		h.Set("X-Command-Center-Signature", "sha256="+sign(sha256.New, w.Secret, dl.Payload))
		h.Set("X-Nexus-Webhook-Signature", sign(sha1.New, w.Secret, dl.Payload))
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, drainBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func sign(h func() hash.Hash, secret, body string) string {
	m := hmac.New(h, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

// Test sends a ping event to w at once, outside the queue, and records it
// as a delivery with one attempt and no retry. It returns the response
// status (0 when no response came) and the error of a failed send.
//
// A nexus webhook's ping is an audit record (rm:global:audit) of domain
// webhook and type ping, as Nexus has no test event of its own.
func (d *Dispatcher) Test(ctx context.Context, w meta.Webhook) (int, error) {
	now := d.Now()
	kind := "ping"
	var body string
	if w.Format == meta.PayloadNexus {
		b := nexusAuditBody{NodeID: d.Node, Initiator: ctxInitiator(ctx)}
		b.Audit.Domain, b.Audit.Type, b.Audit.Context = "webhook", "ping", w.Name
		b.Audit.Attributes = map[string]any{"webhook": w.Name}
		kind, body = nexusID["audit"], marshal(b)
	} else {
		p := Payload{ID: uuid(), Timestamp: now.UTC().Format(time.RFC3339), Event: "ping", Actor: "", Data: map[string]any{"webhook": w.Name}}
		if w.Repo != "" {
			p.Repository = &w.Repo
		}
		body = marshal(p)
	}
	// Sent before it is stored: a pending row would be the poller's to send too.
	dl := meta.WebhookDelivery{ID: uuid(), WebhookID: w.ID, Event: kind, Status: meta.DeliveryOK, Attempts: 1, Created: now, Payload: body}
	sctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	code, err := d.send(sctx, w, dl)
	dl.ResponseCode = code
	if err != nil {
		dl.Status, dl.LastError = meta.DeliveryFailed, clip(logsafe.Err(err), maxDetail)
		err = errors.New(dl.LastError)
	}
	if serr := d.DB.InsertDeliveries(ctx, []meta.WebhookDelivery{dl}); serr != nil {
		return code, serr
	}
	return code, err
}

// Redeliver queues the delivery again from the first attempt. ErrNotFound
// when there is none.
func (d *Dispatcher) Redeliver(ctx context.Context, id string) error {
	if err := d.DB.ResetDelivery(ctx, id, d.Now()); err != nil {
		return err
	}
	d.Kick()
	return nil
}

// Close stops accepting events, stores the queued ones, waits up to
// CloseTimeout for the deliveries in flight and leaves the rest pending.
// Call it before closing the database. It is safe to call twice.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	if d.closed || d.events == nil {
		d.mu.Unlock()
		return
	}
	d.closed = true
	close(d.events)
	d.mu.Unlock()
	close(d.stop)
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
		if d.node != "" { // the other nodes may send what this one claimed at once
			ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
			if err := d.DB.ReleaseDeliveries(ctx, d.node); err != nil {
				d.Log.Warn("could not release this node's webhook delivery claims; they end after "+ClaimFor.String(), "err", logsafe.Err(err))
			}
			cancel()
		}
	case <-time.After(CloseTimeout):
		d.Log.Warn("webhook deliveries were still in flight at the shutdown deadline; they stay pending")
	}
	d.mu.Lock()
	for _, c := range d.clients {
		c.CloseIdleConnections()
	}
	d.mu.Unlock()
}

// uuid returns a random UUID (version 4).
func uuid() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// clip truncates s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
