package ui

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"commandcenter/internal/meta"
	"commandcenter/internal/webhook"
)

// Webhook API: reads are admin only; mutations need an interactive admin
// and the UI header. The secret is write-only: reads report secretSet.

const (
	webhookNameMax = 64
	deliveriesPage = 50
)

func (u *UI) webhookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/webhooks", u.listWebhooks)
	mux.HandleFunc("POST /-/api/webhooks", u.createWebhook)
	mux.HandleFunc("GET /-/api/webhooks/events", u.webhookEvents)
	mux.HandleFunc("GET /-/api/webhooks/{id}", u.getWebhook)
	mux.HandleFunc("PUT /-/api/webhooks/{id}", u.updateWebhook)
	mux.HandleFunc("DELETE /-/api/webhooks/{id}", u.deleteWebhook)
	mux.HandleFunc("POST /-/api/webhooks/{id}/test", u.testWebhook)
	mux.HandleFunc("GET /-/api/webhooks/{id}/deliveries", u.listDeliveries)
	mux.HandleFunc("POST /-/api/webhooks/{id}/deliveries/{did}/redeliver", u.redeliver)
}

// webhookRequest is the body of POST and PUT. On PUT an absent secret keeps
// the stored one; "" clears it. enabled defaults to true on POST. An absent
// payloadFormat is commandcenter on POST and keeps the stored one on PUT.
type webhookRequest struct {
	Name          string   `json:"name"`
	URL           string   `json:"url"`
	Secret        *string  `json:"secret"`
	Events        []string `json:"events"`
	Repo          string   `json:"repo"`
	Enabled       *bool    `json:"enabled"`
	PayloadFormat string   `json:"payloadFormat"`
}

func webhookJSON(w meta.Webhook) map[string]any {
	events := w.Events
	if events == nil {
		events = []string{}
	}
	return map[string]any{"id": w.ID, "name": w.Name, "url": w.URL, "secretSet": w.Secret != "", "events": events,
		"repo": w.Repo, "enabled": w.Enabled, "payloadFormat": w.Format, "created": rfc3339(w.Created), "updated": rfc3339(w.Updated)}
}

func deliveryJSON(d meta.WebhookDelivery) map[string]any {
	return map[string]any{"id": d.ID, "event": d.Event, "status": d.Status, "attempts": d.Attempts, "responseCode": d.ResponseCode,
		"lastError": d.LastError, "created": rfc3339(d.Created), "nextAt": rfc3339(d.NextAt)}
}

// validateWebhook checks body and returns the webhook to store (without
// id and secret) and the problems found.
func (u *UI) validateWebhook(body webhookRequest) (meta.Webhook, []string) {
	var errs []string
	name := strings.TrimSpace(body.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > webhookNameMax {
		errs = append(errs, fmt.Sprintf("name must be 1-%d characters", webhookNameMax))
	}
	if pu, err := url.Parse(body.URL); err != nil || !pu.IsAbs() || pu.Host == "" || pu.Scheme != "http" && pu.Scheme != "https" {
		errs = append(errs, "url must be an absolute http or https URL")
	}
	nexus := body.PayloadFormat == meta.PayloadNexus
	if body.PayloadFormat != "" && !nexus && body.PayloadFormat != meta.PayloadCommandCenter {
		errs = append(errs, fmt.Sprintf("payloadFormat must be %q or %q", meta.PayloadCommandCenter, meta.PayloadNexus))
	}
	switch {
	case len(body.Events) == 0 && nexus:
		errs = append(errs, "events must list at least one of the Nexus event names audit, repository, component and asset")
	case len(body.Events) == 0:
		errs = append(errs, "events must list at least one event kind or \"*\"")
	}
	for _, k := range body.Events {
		switch {
		case nexus && !webhook.NexusKind(k):
			errs = append(errs, fmt.Sprintf("a webhook of payload format nexus lists Nexus event names (audit, repository, component, asset), not %q", k))
		case !nexus && k != "*" && !webhook.KnownKind(k) && !webhook.NexusKind(k):
			errs = append(errs, fmt.Sprintf("unknown event kind %q", k))
		}
	}
	if body.Repo != "" && (u.Repos == nil || u.Repos.Get(body.Repo) == nil) {
		errs = append(errs, fmt.Sprintf("repository %q does not exist", body.Repo))
	}
	enabled := body.Enabled == nil || *body.Enabled
	return meta.Webhook{Name: name, URL: body.URL, Events: body.Events, Repo: body.Repo, Enabled: enabled, Format: body.PayloadFormat}, errs
}

func (u *UI) writeWebhookError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "webhook not found")
	case errors.Is(err, meta.ErrWebhookLimit):
		msg := fmt.Sprintf("at most %d webhooks can exist: delete one first", meta.MaxWebhooks)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg, "errors": []string{msg}})
	default:
		u.fail(w, r, err)
	}
}

// webhooksReady answers 501 when no dispatcher is wired.
func (u *UI) webhooksReady(w http.ResponseWriter) bool {
	if u.Webhooks == nil {
		writeError(w, http.StatusNotImplemented, "webhooks are not available")
		return false
	}
	return true
}

func (u *UI) reloadWebhooks(r *http.Request) {
	if err := u.Webhooks.Reload(r.Context()); err != nil && u.Log != nil {
		u.Log.Warn("could not reload webhooks after a change", "err", err.Error())
	}
	u.notify(r.Context(), "webhooks")
}

// newWebhookID is 16 random hex characters.
func newWebhookID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func webhookSummary(w meta.Webhook) string {
	scope := "global"
	if w.Repo != "" {
		scope = "repo:" + w.Repo
	}
	state := "enabled"
	if !w.Enabled {
		state = "disabled"
	}
	return scope + " " + state + " " + w.Format + " events=" + strings.Join(w.Events, ",")
}

// GET /-/api/webhooks/events (admin): the event kinds with descriptions,
// and the Nexus event names a webhook of payload format nexus lists.
func (u *UI) webhookEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": webhook.Kinds, "nexusEvents": webhook.NexusKinds})
}

func (u *UI) listWebhooks(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.webhooksReady(w) {
		return
	}
	list, err := u.DB.ListWebhooks(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, h := range list {
		out = append(out, webhookJSON(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

func (u *UI) getWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.webhooksReady(w) {
		return
	}
	h, err := u.DB.GetWebhook(r.Context(), r.PathValue("id"))
	if err != nil {
		u.writeWebhookError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, webhookJSON(h))
}

func (u *UI) createWebhook(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.webhooksReady(w) {
		return
	}
	var body webhookRequest
	if !decodeBody(w, r, &body) {
		return
	}
	h, errs := u.validateWebhook(body)
	if len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": errs[0], "errors": errs})
		return
	}
	h.ID = newWebhookID()
	if body.Secret != nil {
		h.Secret = *body.Secret
	}
	if err := u.DB.CreateWebhook(r.Context(), h); err != nil {
		u.writeWebhookError(w, r, err)
		return
	}
	stored, err := u.DB.GetWebhook(r.Context(), h.ID)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.reloadWebhooks(r)
	u.emit(r, "webhook.create", h.Repo, stored.Name, webhookSummary(stored))
	writeJSON(w, http.StatusCreated, webhookJSON(stored))
}

func (u *UI) updateWebhook(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.webhooksReady(w) {
		return
	}
	cur, err := u.DB.GetWebhook(r.Context(), r.PathValue("id"))
	if err != nil {
		u.writeWebhookError(w, r, err)
		return
	}
	var body webhookRequest
	if !decodeBody(w, r, &body) {
		return
	}
	body.PayloadFormat = cmp.Or(body.PayloadFormat, cur.Format)
	h, errs := u.validateWebhook(body)
	if len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": errs[0], "errors": errs})
		return
	}
	h.ID, h.Secret = cur.ID, cur.Secret
	if body.Secret != nil {
		h.Secret = *body.Secret
	}
	if err := u.DB.UpdateWebhook(r.Context(), h); err != nil {
		u.writeWebhookError(w, r, err)
		return
	}
	stored, err := u.DB.GetWebhook(r.Context(), h.ID)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.reloadWebhooks(r)
	u.emit(r, "webhook.update", h.Repo, stored.Name, webhookSummary(stored))
	writeJSON(w, http.StatusOK, webhookJSON(stored))
}

func (u *UI) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.webhooksReady(w) {
		return
	}
	cur, err := u.DB.GetWebhook(r.Context(), r.PathValue("id"))
	if err == nil {
		err = u.DB.DeleteWebhook(r.Context(), cur.ID)
	}
	if err != nil {
		u.writeWebhookError(w, r, err)
		return
	}
	u.reloadWebhooks(r)
	u.emit(r, "webhook.delete", cur.Repo, cur.Name, "")
	w.WriteHeader(http.StatusNoContent)
}

// POST /-/api/webhooks/{id}/test: sends a ping now and reports the
// response status, or the error of a failed send, as 200 {ok, status, error}.
func (u *UI) testWebhook(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.webhooksReady(w) {
		return
	}
	h, err := u.DB.GetWebhook(r.Context(), r.PathValue("id"))
	if err != nil {
		u.writeWebhookError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webhook.RequestTimeout+time.Second)
	defer cancel()
	code, err := u.Webhooks.Test(ctx, h)
	out := map[string]any{"ok": err == nil, "status": code, "error": ""}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /-/api/webhooks/{id}/deliveries: the newest deliveriesPage deliveries.
func (u *UI) listDeliveries(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !u.webhooksReady(w) {
		return
	}
	h, err := u.DB.GetWebhook(r.Context(), r.PathValue("id"))
	if err != nil {
		u.writeWebhookError(w, r, err)
		return
	}
	list, err := u.DB.ListDeliveries(r.Context(), h.ID, deliveriesPage)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, deliveryJSON(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

// POST /-/api/webhooks/{id}/deliveries/{did}/redeliver: queues the delivery
// again from its first attempt; 202 with the delivery.
func (u *UI) redeliver(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) || !u.webhooksReady(w) {
		return
	}
	d, err := u.DB.GetDelivery(r.Context(), r.PathValue("did"))
	if err == nil && d.WebhookID != r.PathValue("id") {
		err = meta.ErrNotFound
	}
	if err == nil {
		err = u.Webhooks.Redeliver(r.Context(), d.ID)
	}
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			writeError(w, http.StatusNotFound, "delivery not found")
			return
		}
		u.fail(w, r, err)
		return
	}
	d, err = u.DB.GetDelivery(r.Context(), d.ID)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, deliveryJSON(d))
}
