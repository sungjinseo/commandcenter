package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestRepoOnlineOffline(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.srv.Handlers["maven"] = readAll{}
	list := func() map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, x := range e.admin(t, "GET", "/-/api/repositories", "").json(t)["repositories"].([]any) {
			m := x.(map[string]any)
			out[m["name"].(string)] = m
		}
		return out
	}
	status := func(path string) int { return e.req(t, "GET", path, "", "admin", nil, false).status }

	// Existing and new repositories are online unless told otherwise.
	if list()["maven-releases"]["online"] != true {
		t.Fatalf("existing repo: %v", list()["maven-releases"])
	}
	r := e.admin(t, "POST", "/-/api/repositories", `{"name":"mvn-new","format":"maven","type":"hosted"}`)
	wantStatus(t, r, http.StatusCreated)
	if r.json(t)["online"] != true {
		t.Fatalf("create default: %s", r.body)
	}
	r = e.admin(t, "POST", "/-/api/repositories", `{"name":"mvn-off","format":"maven","type":"hosted","online":false}`)
	wantStatus(t, r, http.StatusCreated)
	if r.json(t)["online"] != false || status("/repository/mvn-off/x") != http.StatusServiceUnavailable {
		t.Fatalf("created offline: %s", r.body)
	}
	if st := status("/repository/maven-releases/readme.txt"); st != http.StatusCreated {
		t.Fatalf("online repo: %d", st)
	}

	// Toggle offline: 503 with the JSON error; config and list show it.
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"online":false}`), http.StatusOK)
	res := e.req(t, "GET", "/repository/maven-releases/readme.txt", "", "admin", nil, false)
	if res.status != http.StatusServiceUnavailable || !strings.Contains(res.body, `"error":"repository is offline"`) {
		t.Fatalf("offline: %d %s", res.status, res.body)
	}
	if e.admin(t, "GET", "/-/api/repositories/maven-releases/config", "").json(t)["online"] != false || list()["maven-releases"]["online"] != false {
		t.Fatal("offline not shown")
	}
	// An update that omits online keeps it offline; the state is persisted.
	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"writePolicy":"allow"}`), http.StatusOK)
	if e.admin(t, "GET", "/-/api/repositories/maven-releases/config", "").json(t)["online"] != false {
		t.Fatal("omitted online reset the state")
	}
	defs, err := e.db.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if d.Name == "maven-releases" && d.IsOnline() {
			t.Fatal("offline state not persisted")
		}
	}
	// The group still lists its offline member for the UI.
	g := list()["maven-public"]
	if g == nil || len(g["members"].([]any)) != 2 {
		t.Fatalf("group: %v", g)
	}

	wantStatus(t, e.admin(t, "PUT", "/-/api/repositories/maven-releases", `{"online":true}`), http.StatusOK)
	if st := status("/repository/maven-releases/readme.txt"); st != http.StatusCreated {
		t.Fatalf("back online: %d", st)
	}
}
