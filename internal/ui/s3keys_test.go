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
	e.ui.S3.Host, e.ui.S3.Region = "s3.repo.example", "us-east-1"
	wantStatus(t, e.get(t, "/-/api/s3keys", ""), http.StatusUnauthorized)
	if r := e.get(t, "/-/api/s3keys", "ci"); r.status != http.StatusOK || !strings.Contains(r.body, `"s3Keys":[]`) ||
		!strings.Contains(r.body, `"endpoint":"http://s3.repo.example"`) {
		t.Fatalf("empty: %d %s", r.status, r.body)
	}
	wantStatus(t, e.req(t, "POST", "/-/api/s3keys", `{"name":"ci"}`, "ci", nil, false), http.StatusForbidden) // no UI header
	wantStatus(t, e.req(t, "POST", "/-/api/s3keys", `{"expires":"2000-01-01"}`, "ci", nil, true), http.StatusBadRequest)
	r := e.req(t, "POST", "/-/api/s3keys", `{"name":"build"}`, "ci", nil, true)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	id, secret := m["accessKeyId"].(string), m["secretAccessKey"].(string)
	if !strings.HasPrefix(id, "CCK") || len(secret) != 40 || m["name"] != "build" {
		t.Fatalf("created: %s", r.body)
	}
	if r := e.get(t, "/-/api/s3keys", "ci"); !strings.Contains(r.body, id) || strings.Contains(r.body, secret) {
		t.Fatalf("list: %s", r.body)
	}
	wantStatus(t, e.get(t, "/-/api/s3keys?all=1", "ci"), http.StatusForbidden)
	if r := e.get(t, "/-/api/s3keys?all=1", "admin"); !strings.Contains(r.body, id) {
		t.Fatalf("admin list: %s", r.body)
	}
	// Someone else's key looks missing; the admin may delete it.
	wantStatus(t, e.req(t, "DELETE", "/-/api/s3keys/"+id, "", "reader", nil, true), http.StatusNotFound)
	wantStatus(t, e.req(t, "DELETE", "/-/api/s3keys/"+id, "", "admin", nil, true), http.StatusNoContent)
	wantStatus(t, e.req(t, "DELETE", "/-/api/s3keys/"+id, "", "ci", nil, true), http.StatusNotFound)
	if ev := sink.String(); !strings.Contains(ev, "s3key.create") || !strings.Contains(ev, "s3key.delete") || strings.Contains(ev, secret) {
		t.Fatalf("events: %s", ev)
	}
}
