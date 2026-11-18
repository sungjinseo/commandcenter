package ui

import (
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/repo/repotest"
)

func TestS3KeyAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	wantStatus(t, e.get(t, "/-/api/s3keys", ""), http.StatusUnauthorized)
	wantStatus(t, e.req(t, "POST", "/-/api/s3keys", "", "ci", nil, false), http.StatusForbidden) // no UI header
	r := e.req(t, "POST", "/-/api/s3keys", "", "ci", nil, true)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	id, secret := m["accessKeyId"].(string), m["secretAccessKey"].(string)
	if len(id) != 20 || len(secret) != 40 || m["user"] != "ci" {
		t.Fatalf("created: %s", r.body)
	}
	// The secret is shown once: the list leaves it out.
	if r := e.get(t, "/-/api/s3keys", "ci"); !strings.Contains(r.body, id) || strings.Contains(r.body, secret) {
		t.Fatalf("list: %s", r.body)
	}
	if r := e.get(t, "/-/api/s3keys", "reader"); strings.Contains(r.body, id) {
		t.Fatalf("another user's list: %s", r.body)
	}
	// Only an admin creates keys for others, and only for a user that exists.
	wantStatus(t, e.req(t, "POST", "/-/api/s3keys", `{"user":"reader"}`, "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/s3keys", `{"user":"nobody"}`, "admin", nil, true), http.StatusBadRequest)
	r = e.req(t, "POST", "/-/api/s3keys", `{"user":"reader"}`, "admin", nil, true)
	wantStatus(t, r, http.StatusCreated)
	readerKey := r.json(t)["accessKeyId"].(string)
	if r := e.get(t, "/-/api/s3keys", "admin"); !strings.Contains(r.body, id) || !strings.Contains(r.body, readerKey) {
		t.Fatalf("admin's list: %s", r.body)
	}
	// A user revokes their own keys only; an admin any.
	wantStatus(t, e.req(t, "DELETE", "/-/api/s3keys/"+readerKey, "", "ci", nil, true), http.StatusNotFound)
	wantStatus(t, e.req(t, "DELETE", "/-/api/s3keys/"+id, "", "ci", nil, true), http.StatusNoContent)
	wantStatus(t, e.req(t, "DELETE", "/-/api/s3keys/"+id, "", "ci", nil, true), http.StatusNotFound)
	wantStatus(t, e.req(t, "DELETE", "/-/api/s3keys/"+readerKey, "", "admin", nil, true), http.StatusNoContent)
	if ev := sink.String(); !strings.Contains(ev, "s3key.create|ci||ci") || !strings.Contains(ev, "s3key.create|admin||reader") ||
		!strings.Contains(ev, "s3key.revoke|admin||reader") || strings.Contains(ev, secret) {
		t.Fatalf("events: %s", ev)
	}
}
