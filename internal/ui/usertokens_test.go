package ui

import (
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/repo/repotest"
)

func TestMyUserTokenAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	wantStatus(t, e.get(t, "/-/api/me/usertoken", ""), http.StatusUnauthorized)
	if r := e.get(t, "/-/api/me/usertoken", "ci"); r.status != http.StatusOK || !strings.Contains(r.body, `"enabled":false`) || !strings.Contains(r.body, `"exists":false`) {
		t.Fatalf("before: %d %s", r.status, r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"userTokens":{"enabled":true,"expirationDays":3651}}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"userTokens":{"enabled":true,"expirationDays":7}}`), http.StatusOK)
	wantStatus(t, e.req(t, "POST", "/-/api/me/usertoken", "", "ci", nil, false), http.StatusForbidden) // no UI header
	r := e.req(t, "POST", "/-/api/me/usertoken", "", "ci", nil, true)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	if m["expires"] == "" || m["source"] != "local" || m["userId"] != "ci" {
		t.Fatalf("created: %s", r.body)
	}
	if r := e.get(t, "/-/api/me/usertoken", "ci"); !strings.Contains(r.body, `"exists":true`) || !strings.Contains(r.body, `"expirationDays":7`) ||
		!strings.Contains(r.body, `"nameCode":"`+m["nameCode"].(string)[:2]+`******"`) {
		t.Fatalf("after: %s", r.body)
	}
	wantStatus(t, e.req(t, "DELETE", "/-/api/me/usertoken", "", "ci", nil, true), http.StatusNoContent)
	wantStatus(t, e.req(t, "DELETE", "/-/api/me/usertoken", "", "ci", nil, true), http.StatusNotFound)
	// Admin only, and changes need an interactive admin.
	wantStatus(t, e.get(t, "/-/api/usertokens", "ci"), http.StatusForbidden)
	wantStatus(t, e.req(t, "DELETE", "/-/api/usertokens/ci", "", "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "POST", "/-/api/usertokens/nobody", "", "admin", nil, true), http.StatusConflict)
	if ev := sink.String(); !strings.Contains(ev, "settings.userTokens") || !strings.Contains(ev, "usertoken.create|ci||ci") || !strings.Contains(ev, "usertoken.delete|ci||ci") {
		t.Fatalf("events: %s", ev)
	}
	for _, ev := range sink.Events() {
		if strings.Contains(ev.Detail, m["passCode"].(string)) {
			t.Fatalf("event %+v holds the pass code", ev)
		}
	}
}
