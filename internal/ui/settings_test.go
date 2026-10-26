package ui

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

// readAll answers 201 once it has read the whole body, so a body over the cap
// fails with 413.
type readAll struct{}

func (readAll) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	if _, err := io.ReadAll(r.Body); err != nil {
		repo.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func TestSettingsGet(t *testing.T) {
	e := newEnv(t, "http://repo.example/")
	r := e.admin(t, "GET", "/-/api/settings", "")
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if m["baseURL"] != "http://repo.example" || m["anonymousRead"] != false || m["authenticatedRead"] != true ||
		m["sessionIdleMinutes"] != float64(720) || m["maxUploadMiB"] != float64(1024) || m["defaultLanguage"] != "ko" {
		t.Fatalf("settings: %s", r.body)
	}
	for _, k := range []string{"realms", "smtp", "ldap"} {
		if _, ok := m[k]; ok {
			t.Fatalf("%s must not be in the general settings: %s", k, r.body)
		}
	}
}

func TestSettingsGates(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.req(t, "GET", "/-/api/settings", "", "", nil, false), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/settings", "ci"), http.StatusForbidden)
	wantStatus(t, e.get(t, "/-/api/settings", "admin"), http.StatusOK)
	body := `{"maxUploadMiB":5}`
	wantStatus(t, e.req(t, "PUT", "/-/api/settings", body, "admin", nil, false), http.StatusForbidden) // no UI header
	wantStatus(t, e.req(t, "PUT", "/-/api/settings", body, "ci", nil, true), http.StatusForbidden)
	wantStatus(t, e.req(t, "PUT", "/-/api/settings", body, "", nil, true), http.StatusUnauthorized)
	// An admin's API token may read but not change settings.
	_, tok := e.newToken(t, "admin", "")
	if st := e.bearer(t, "/-/api/settings", tok); st != http.StatusOK {
		t.Fatalf("token read: %d", st)
	}
	r, _ := http.NewRequest("PUT", e.ts.URL+"/-/api/settings", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("X-Command-Center-UI", "1")
	res, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("token PUT: %d", res.StatusCode)
	}
	if e.admin(t, "GET", "/-/api/settings", "").json(t)["maxUploadMiB"] != float64(1024) {
		t.Fatal("a rejected PUT changed the settings")
	}
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for name, body := range map[string]string{
		"empty baseURL":    `{"baseURL":""}`,
		"ftp baseURL":      `{"baseURL":"ftp://x"}`,
		"relative baseURL": `{"baseURL":"repo.example"}`,
		"baseURL userinfo": `{"baseURL":"https://u:secret@repo.example"}`, // QA N-24
		"baseURL query":    `{"baseURL":"https://repo.example/?q=1"}`,
		"baseURL fragment": `{"baseURL":"https://repo.example/#f"}`,
		"baseURL bare #":   `{"baseURL":"https://repo.example/#"}`,
		"idle too small":   `{"sessionIdleMinutes":4}`,
		"idle too large":   `{"sessionIdleMinutes":10081}`,
		"upload zero":      `{"maxUploadMiB":0}`,
		"upload too large": `{"maxUploadMiB":1048577}`,
		"bad language":     `{"defaultLanguage":"fr"}`,
		"unknown field":    `{"realms":[]}`,
		"valid + invalid":  `{"maxUploadMiB":5,"defaultLanguage":"xx"}`,
		"wrong type":       `{"sessionIdleMinutes":"ten"}`,
	} {
		r := e.admin(t, "PUT", "/-/api/settings", body)
		wantStatus(t, r, http.StatusBadRequest)
		if name != "unknown field" && name != "wrong type" {
			m := r.json(t)
			if m["error"] == nil || len(m["errors"].([]any)) == 0 {
				t.Fatalf("%s: shape %s", name, r.body)
			}
		}
	}
	// Nothing was written, not even the valid half of a mixed request.
	if m := e.admin(t, "GET", "/-/api/settings", "").json(t); m["maxUploadMiB"] != float64(1024) {
		t.Fatalf("settings changed: %v", m)
	}
	for _, body := range []string{`{"sessionIdleMinutes":5}`, `{"sessionIdleMinutes":10080}`, `{"maxUploadMiB":1048576}`, `{"maxUploadMiB":1}`, `{"defaultLanguage":"en"}`} {
		wantStatus(t, e.admin(t, "PUT", "/-/api/settings", body), http.StatusOK)
	}
}

func TestSettingsPartialUpdateEventAndSessionIdle(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	r := e.admin(t, "PUT", "/-/api/settings",
		`{"baseURL":" https://new.example/ ","sessionIdleMinutes":30,"anonymousRead":true}`)
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if m["baseURL"] != "https://new.example" || m["sessionIdleMinutes"] != float64(30) || m["anonymousRead"] != true ||
		m["maxUploadMiB"] != float64(1024) || m["authenticatedRead"] != true || m["defaultLanguage"] != "ko" {
		t.Fatalf("partial update: %s", r.body)
	}
	if got := e.admin(t, "GET", "/-/api/settings", "").body; got != r.body {
		t.Fatalf("GET %s != PUT %s", got, r.body)
	}
	if e.ses.Idle() != 30*time.Minute {
		t.Fatalf("session idle = %v", e.ses.Idle())
	}
	evs := sink.Events()
	if len(evs) != 1 || evs[0].Kind != "settings.update" || evs[0].Subject != "" ||
		evs[0].Detail != "baseURL, anonymousRead, sessionIdleMinutes" {
		t.Fatalf("events: %+v", evs)
	}
	// An empty body is refused and emits nothing.
	r = e.admin(t, "PUT", "/-/api/settings", `{}`)
	wantStatus(t, r, http.StatusBadRequest)
	if r.json(t)["error"] != "no settings provided" || len(sink.Events()) != 1 {
		t.Fatalf("empty body: %s events=%d", r.body, len(sink.Events()))
	}
}

func TestLiveBaseURL(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	url := func() string {
		for _, x := range e.admin(t, "GET", "/-/api/repositories", "").json(t)["repositories"].([]any) {
			if m := x.(map[string]any); m["name"] == "maven-releases" {
				return m["url"].(string)
			}
		}
		return ""
	}
	if got := url(); got != "http://repo.example/repository/maven-releases/" {
		t.Fatalf("before: %s", got)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"baseURL":"https://secure.example"}`), http.StatusOK)
	if got := url(); got != "https://secure.example/repository/maven-releases/" {
		t.Fatalf("after: %s", got)
	}
	// The session cookie's Secure flag follows the new scheme.
	r := e.req(t, "POST", "/-/api/login", `{"user":"admin","password":"pw"}`, "", nil, true)
	wantStatus(t, r, http.StatusOK)
	secure := false
	for _, c := range r.cookie {
		secure = secure || c.Secure
	}
	if !secure {
		t.Fatal("cookie is not Secure under an https baseURL")
	}
}

func TestLiveMaxUpload(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.srv.Handlers["maven"] = readAll{}
	put := func(n int) int {
		return e.req(t, "PUT", "/repository/maven-releases/x", strings.Repeat("x", n), "admin", nil, false).status
	}
	if st := put(2 << 20); st != http.StatusCreated {
		t.Fatalf("under the default cap: %d", st)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"maxUploadMiB":1}`), http.StatusOK)
	if st := put(2 << 20); st != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the new cap: %d", st)
	}
	if st := put(10); st != http.StatusCreated {
		t.Fatalf("under the new cap: %d", st)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings", `{"maxUploadMiB":4}`), http.StatusOK)
	if st := put(2 << 20); st != http.StatusCreated {
		t.Fatalf("cap raised again: %d", st)
	}
}
