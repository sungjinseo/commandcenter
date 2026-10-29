package meta

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/meta/metatest"
)

func TestWebhookCRUD(t *testing.T) {
	db := openTest(t)
	w := Webhook{ID: "h1", Name: "ci", URL: "https://ci.example/hook", Secret: "hook-secret", Events: []string{"component.created", "*"}, Repo: "r1", Enabled: true}
	if err := db.CreateWebhook(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWebhook(ctx, w); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := db.GetWebhook(ctx, "h1")
	if err != nil || got.Name != "ci" || got.URL != w.URL || got.Secret != "hook-secret" || fmt.Sprint(got.Events) != "[component.created *]" ||
		got.Repo != "r1" || !got.Enabled || got.Created.IsZero() {
		t.Fatalf("get: %+v, %v", got, err)
	}
	if err := db.CreateWebhook(ctx, Webhook{ID: "h0", Name: "a", URL: "http://x", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListWebhooks(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "h0" || list[1].ID != "h1" || len(list[0].Events) != 0 {
		t.Fatalf("list: %+v, %v", list, err)
	}
	w.Name, w.Secret, w.Enabled, w.Events = "ci2", "", false, []string{"ping"}
	if err := db.UpdateWebhook(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetWebhook(ctx, "h1"); got.Name != "ci2" || got.Secret != "" || got.Enabled || fmt.Sprint(got.Events) != "[ping]" {
		t.Fatalf("after update: %+v", got)
	}
	if err := db.UpdateWebhook(ctx, Webhook{ID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	MaxWebhooks = 2
	defer func() { MaxWebhooks = 100 }()
	if err := db.CreateWebhook(ctx, Webhook{ID: "h2", Name: "c", URL: "http://x"}); !errors.Is(err, ErrWebhookLimit) {
		t.Fatalf("limit: %v", err)
	}
	if err := db.DeleteWebhook(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteWebhook(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := db.GetWebhook(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
}

// The signing secret is sealed in its row with a key, never stored in plain
// text, and follows a key rotation.
func TestWebhookSecretSealed(t *testing.T) {
	path := metatest.URL(t, t.TempDir())
	open := func(key byte) *DB {
		t.Helper()
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if key != 0 {
			if err := db.SetSecretKey(bytes.Repeat([]byte{key}, 32)); err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	a := open(1)
	if err := a.CreateWebhook(ctx, Webhook{ID: "h", Name: "n", URL: "http://x", Secret: "hook-secret", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	const q = `SELECT secret FROM webhooks WHERE id = ?`
	if raw := rawRow(t, a, q, "h"); strings.Contains(raw, "hook-secret") || !strings.HasPrefix(raw, sealedPrefix) {
		t.Fatalf("row not sealed: %s", raw)
	}
	if w, err := a.GetWebhook(ctx, "h"); err != nil || w.Secret != "hook-secret" {
		t.Fatalf("read back: %+v, %v", w, err)
	}
	if _, err := open(0).ListWebhooks(ctx); err == nil || !strings.Contains(err.Error(), "no secret key") || strings.Contains(err.Error(), "hook-secret") {
		t.Fatalf("without the key: %v", err)
	}
	if _, _, err := a.RotateSecretKey(ctx, bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
	if w, err := open(2).GetWebhook(ctx, "h"); err != nil || w.Secret != "hook-secret" {
		t.Fatalf("after rotation: %+v, %v", w, err)
	}
	if _, err := open(1).GetWebhook(ctx, "h"); err == nil || !strings.Contains(err.Error(), "another key") {
		t.Fatalf("old key: %v", err)
	}
	// A plain-text secret (stored without a key) is sealed by SealSecrets.
	if err := open(0).UpdateWebhook(ctx, Webhook{ID: "h", Name: "n", URL: "http://x", Secret: "plain-secret", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	b := open(2)
	if n, err := b.SealSecrets(ctx); err != nil || n != 1 {
		t.Fatalf("SealSecrets = %d, %v", n, err)
	}
	if raw := rawRow(t, b, q, "h"); strings.Contains(raw, "plain-secret") {
		t.Fatalf("still plain: %s", raw)
	}
}

func TestWebhookDeliveries(t *testing.T) {
	db := openTest(t)
	for _, id := range []string{"on", "off"} {
		if err := db.CreateWebhook(ctx, Webhook{ID: id, Name: id, URL: "http://x", Enabled: id == "on"}); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	mk := func(id, hook string, at time.Time) WebhookDelivery {
		return WebhookDelivery{ID: id, WebhookID: hook, Event: "ping", Status: DeliveryPending, Created: at, NextAt: at, Payload: `{"id":"` + id + `"}`}
	}
	if err := db.InsertDeliveries(ctx, []WebhookDelivery{mk("d1", "on", t0), mk("d2", "on", t0.Add(time.Minute)), mk("d3", "off", t0)}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDeliveries(ctx, []WebhookDelivery{{ID: "big", WebhookID: "on", Payload: strings.Repeat("x", MaxDeliveryPayload+1)}}); err == nil {
		t.Fatal("an oversized payload was stored")
	}
	claim := func(node string, now time.Time) []WebhookDelivery {
		t.Helper()
		list, err := db.ClaimDeliveries(ctx, node, now, now.Add(time.Minute), 10)
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	if due := claim("n1", t0); len(due) != 1 || due[0].ID != "d1" || due[0].ClaimedBy != "n1" || !due[0].ClaimUntil.Equal(t0.Add(time.Minute)) {
		t.Fatalf("claimed at t0: %+v", due)
	}
	if due := claim("n2", t0.Add(30*time.Second)); len(due) != 0 { // d1 is n1's; d2 is not due
		t.Fatalf("claimed under a claim: %+v", due)
	}
	// Once the claim ended both are due again; the disabled hook's d3 never is.
	later := t0.Add(time.Hour)
	due := claim("n2", later)
	if len(due) != 2 || due[0].ID != "d1" || due[1].ID != "d2" || due[0].Payload != `{"id":"d1"}` || due[1].ClaimedBy != "n2" {
		t.Fatalf("due (disabled hook excluded): %+v", due)
	}
	if err := db.RecordDeliveryAttempt(ctx, "d1", DeliveryPending, 1, 500, "HTTP 500", later.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	d, err := db.GetDelivery(ctx, "d1")
	if err != nil || d.Attempts != 1 || d.ResponseCode != 500 || d.LastError != "HTTP 500" || !d.NextAt.Equal(later.Add(30*time.Second)) ||
		d.Status != DeliveryPending || d.ClaimedBy != "" || !d.ClaimUntil.IsZero() {
		t.Fatalf("after attempt (claim ended): %+v, %v", d, err)
	}
	if due := claim("n3", later.Add(10*time.Second)); len(due) != 0 { // d1 not yet due, d2 n2's
		t.Fatalf("not yet due: %+v", due)
	}
	if err := db.ReleaseDeliveries(ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	if due := claim("n3", later.Add(10*time.Second)); len(due) != 1 || due[0].ID != "d2" {
		t.Fatalf("after n2 released its claims: %+v", due)
	}
	if err := db.RecordDeliveryAttempt(ctx, "d1", DeliveryOK, 2, 200, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.ResetDelivery(ctx, "d1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d, _ = db.GetDelivery(ctx, "d1"); d.Status != DeliveryPending || d.Attempts != 0 || d.LastError != "" || d.ResponseCode != 0 || !d.NextAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("after reset: %+v", d)
	}
	if err := db.ResetDelivery(ctx, "nope", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset missing: %v", err)
	}
	list, err := db.ListDeliveries(ctx, "on", 50)
	if err != nil || len(list) != 2 || list[0].ID != "d2" || list[1].ID != "d1" {
		t.Fatalf("list newest first: %+v, %v", list, err)
	}
	// Only the newest KeepDeliveries per webhook survive an insert.
	var many []WebhookDelivery
	for i := 0; i < KeepDeliveries+10; i++ {
		many = append(many, mk(fmt.Sprintf("m%03d", i), "on", t0.Add(time.Duration(i+10)*time.Minute)))
	}
	if err := db.InsertDeliveries(ctx, many); err != nil {
		t.Fatal(err)
	}
	if list, _ = db.ListDeliveries(ctx, "on", 1000); len(list) != KeepDeliveries || list[0].ID != "m209" || list[len(list)-1].ID != "m010" {
		t.Fatalf("pruned to %d, first %s last %s", len(list), list[0].ID, list[len(list)-1].ID)
	}
	if _, err := db.GetDelivery(ctx, "d1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old delivery survived pruning: %v", err)
	}
	if list, _ = db.ListDeliveries(ctx, "off", 10); len(list) != 1 {
		t.Fatalf("other hook's deliveries pruned: %+v", list)
	}
	if err := db.DeleteWebhook(ctx, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetDelivery(ctx, "d3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deliveries of a deleted webhook kept: %v", err)
	}
}
