package meta

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Webhook is one stored webhook. Secret is the signing secret, sealed at rest
// like the other secrets (secrets.go) and opened on read. Repo "" means the
// hook is global; otherwise only that repository's events reach it.
type Webhook struct {
	ID, Name, URL, Secret string
	Events                []string
	Repo                  string
	Enabled               bool
	Created, Updated      time.Time
}

// Delivery statuses.
const (
	DeliveryPending = "pending"
	DeliveryOK      = "ok"
	DeliveryFailed  = "failed"
)

// WebhookDelivery is one event sent, or to be sent, to one webhook.
type WebhookDelivery struct {
	ID, WebhookID, Event, Status string
	Attempts                     int
	LastError                    string
	Created, NextAt              time.Time
	ResponseCode                 int
	Payload                      string
	// The claim of the node sending it (ClaimDeliveries): the node's id,
	// and when the claim ends; "" and zero when none holds it.
	ClaimedBy  string
	ClaimUntil time.Time
}

// MaxWebhooks bounds how many webhooks may exist; a variable so tests can
// lower it.
var MaxWebhooks = 100

// ErrWebhookLimit is returned when MaxWebhooks webhooks exist already.
var ErrWebhookLimit = errors.New("too many webhooks")

// KeepDeliveries is how many deliveries are kept per webhook: older ones are
// deleted when a new one is inserted.
const KeepDeliveries = 200

// MaxDeliveryPayload bounds a stored payload.
const MaxDeliveryPayload = 64 << 10

const webhookCols = `id, name, url, secret, events, repo, enabled, created_at, updated_at`

func (d *DB) scanWebhook(row scanner) (Webhook, error) {
	var w Webhook
	var events string
	var enabled int
	var created, updated int64
	err := row.Scan(&w.ID, &w.Name, &w.URL, &w.Secret, &events, &w.Repo, &enabled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, err
	}
	if err := json.Unmarshal([]byte(events), &w.Events); err != nil {
		return Webhook{}, fmt.Errorf("webhook %q: decode events: %w", w.ID, err)
	}
	if w.Secret, err = d.box.open(w.Secret); err != nil {
		return Webhook{}, fmt.Errorf("webhook %q: secret: %w", w.ID, err)
	}
	w.Enabled = enabled != 0
	w.Created, w.Updated = fromMs(created), fromMs(updated)
	return w, nil
}

// ListWebhooks returns every webhook, ordered by name then id.
func (d *DB) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+webhookCols+` FROM webhooks ORDER BY name, id LIMIT ?`, MaxWebhooks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		w, err := d.scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWebhook returns the webhook, or ErrNotFound.
func (d *DB) GetWebhook(ctx context.Context, id string) (Webhook, error) {
	return d.scanWebhook(d.sql.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
}

func eventsJSON(events []string) string {
	if events == nil {
		events = []string{}
	}
	b, _ := json.Marshal(events)
	return string(b)
}

// CreateWebhook stores a new webhook; ErrExists if the id is taken,
// ErrWebhookLimit when MaxWebhooks exist.
func (d *DB) CreateWebhook(ctx context.Context, w Webhook) error {
	secret, err := d.box.seal(w.Secret)
	if err != nil {
		return fmt.Errorf("webhook secret: %w", err)
	}
	return d.Tx(ctx, func(tx *Tx) error {
		var n, exists int
		if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN id = ? THEN 1 ELSE 0 END), 0) FROM webhooks`, w.ID).
			Scan(&n, &exists); err != nil {
			return err
		}
		switch {
		case exists > 0:
			return ErrExists
		case n >= MaxWebhooks:
			return ErrWebhookLimit
		}
		now := toMs(time.Now())
		_, err := tx.q.ExecContext(ctx, `INSERT INTO webhooks(`+webhookCols+`) VALUES(?,?,?,?,?,?,?,?,?)`,
			w.ID, w.Name, w.URL, secret, eventsJSON(w.Events), w.Repo, boolInt(w.Enabled), now, now)
		return err
	})
}

// UpdateWebhook replaces everything but the id and created_at of the
// webhook; ErrNotFound if there is none.
func (d *DB) UpdateWebhook(ctx context.Context, w Webhook) error {
	secret, err := d.box.seal(w.Secret)
	if err != nil {
		return fmt.Errorf("webhook secret: %w", err)
	}
	res, err := d.sql.ExecContext(ctx, `UPDATE webhooks SET name = ?, url = ?, secret = ?, events = ?, repo = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		w.Name, w.URL, secret, eventsJSON(w.Events), w.Repo, boolInt(w.Enabled), toMs(time.Now()), w.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWebhook removes the webhook and its deliveries; ErrNotFound if there
// is none.
func (d *DB) DeleteWebhook(ctx context.Context, id string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		res, err := tx.q.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.q.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE webhook_id = ?`, id)
		return err
	})
}

const deliveryCols = `id, webhook_id, event, status, attempts, last_error, created_at, next_at, response_code, payload`

// deliveryRead is what scanDelivery reads: deliveryCols and the claim.
const deliveryRead = deliveryCols + `, claimed_by, claim_until`

func scanDelivery(row scanner) (WebhookDelivery, error) {
	var dl WebhookDelivery
	var created, next, claim int64
	err := row.Scan(&dl.ID, &dl.WebhookID, &dl.Event, &dl.Status, &dl.Attempts, &dl.LastError, &created, &next, &dl.ResponseCode, &dl.Payload,
		&dl.ClaimedBy, &claim)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookDelivery{}, ErrNotFound
	}
	if err != nil {
		return WebhookDelivery{}, err
	}
	dl.Created, dl.NextAt, dl.ClaimUntil = fromMs(created), fromMs(next), fromMs(claim)
	return dl, nil
}

func queryDeliveries(ctx context.Context, q querier, query string, args ...any) ([]WebhookDelivery, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookDelivery
	for rows.Next() {
		dl, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// InsertDeliveries stores the deliveries in one transaction and keeps only
// the newest KeepDeliveries of each webhook they belong to. A payload longer
// than MaxDeliveryPayload is refused.
func (d *DB) InsertDeliveries(ctx context.Context, dls []WebhookDelivery) error {
	if len(dls) == 0 {
		return nil
	}
	for _, dl := range dls {
		if len(dl.Payload) > MaxDeliveryPayload {
			return fmt.Errorf("delivery %q: payload of %d bytes exceeds %d", dl.ID, len(dl.Payload), MaxDeliveryPayload)
		}
	}
	return d.Tx(ctx, func(tx *Tx) error {
		hooks := map[string]bool{}
		for _, dl := range dls {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO webhook_deliveries(`+deliveryCols+`) VALUES(?,?,?,?,?,?,?,?,?,?)`,
				dl.ID, dl.WebhookID, dl.Event, dl.Status, dl.Attempts, dl.LastError, toMs(dl.Created), toMs(dl.NextAt), dl.ResponseCode, dl.Payload); err != nil {
				return err
			}
			hooks[dl.WebhookID] = true
		}
		for id := range hooks {
			if _, err := tx.q.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE webhook_id = ? AND id NOT IN
(SELECT id FROM (SELECT id FROM webhook_deliveries WHERE webhook_id = ? ORDER BY created_at DESC, id DESC LIMIT ?) AS keep)`,
				id, id, KeepDeliveries); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordDeliveryAttempt stores the outcome of one attempt: the status
// (DeliveryOK, DeliveryPending for a retry at nextAt, or DeliveryFailed),
// the attempt count, the response code and the error. It ends the claim.
func (d *DB) RecordDeliveryAttempt(ctx context.Context, id, status string, attempts, code int, lastError string, nextAt time.Time) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE webhook_deliveries SET status = ?, attempts = ?, response_code = ?, last_error = ?, next_at = ?,
  claimed_by = '', claim_until = 0 WHERE id = ?`,
		status, attempts, code, lastError, toMs(nextAt), id)
	return err
}

// ResetDelivery queues the delivery again from the first attempt, at now,
// unclaimed; ErrNotFound if there is none.
func (d *DB) ResetDelivery(ctx context.Context, id string, now time.Time) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE webhook_deliveries SET status = ?, attempts = 0, response_code = 0, last_error = '', next_at = ?,
  claimed_by = '', claim_until = 0 WHERE id = ?`,
		DeliveryPending, toMs(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetDelivery returns the delivery, or ErrNotFound.
func (d *DB) GetDelivery(ctx context.Context, id string) (WebhookDelivery, error) {
	return scanDelivery(d.sql.QueryRowContext(ctx, `SELECT `+deliveryRead+` FROM webhook_deliveries WHERE id = ?`, id))
}

// ListDeliveries returns up to limit deliveries of the webhook, newest first.
func (d *DB) ListDeliveries(ctx context.Context, webhookID string, limit int) ([]WebhookDelivery, error) {
	return queryDeliveries(ctx, d.sql, `SELECT `+deliveryRead+` FROM webhook_deliveries WHERE webhook_id = ?
ORDER BY created_at DESC, id DESC LIMIT ?`, webhookID, limit)
}

// ClaimDeliveries claims for node, until until, up to limit pending
// deliveries of enabled webhooks whose next attempt is not after now and
// that no claim holds (one that ended before now does not), and returns
// them earliest first. A claimed delivery is node's to send: two nodes
// claiming at once skip each other's rows (FOR UPDATE SKIP
// LOCKED), and a node that dies holding a claim leaves the delivery to the
// others once the claim ends. The statement takes no write lock: no
// transaction checks an invariant on the claim.
func (d *DB) ClaimDeliveries(ctx context.Context, node string, now, until time.Time, limit int) ([]WebhookDelivery, error) {
	list, err := queryDeliveries(ctx, d.sql, `UPDATE webhook_deliveries SET claimed_by = ?, claim_until = ?
WHERE id IN (SELECT d.id FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id
  WHERE d.status = ? AND d.next_at <= ? AND d.claim_until < ? AND w.enabled = 1
  ORDER BY d.next_at, d.id LIMIT ? FOR UPDATE OF d SKIP LOCKED)
RETURNING `+deliveryRead, node, toMs(until), DeliveryPending, toMs(now), toMs(now), limit)
	slices.SortFunc(list, func(a, b WebhookDelivery) int {
		return cmp.Or(a.NextAt.Compare(b.NextAt), strings.Compare(a.ID, b.ID))
	})
	return list, err
}

// ReleaseDeliveries ends the claims of node on pending deliveries, so that
// they are sent again at once.
func (d *DB) ReleaseDeliveries(ctx context.Context, node string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE webhook_deliveries SET claimed_by = '', claim_until = 0
WHERE claim_until > 0 AND status = ? AND claimed_by = ?`, DeliveryPending, node)
	return err
}
