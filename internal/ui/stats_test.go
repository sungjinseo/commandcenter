package ui

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

func TestRequestStatsAPIRangesAndTotals(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	now := time.Date(2026, 10, 20, 15, 30, 0, 0, time.UTC)
	e.ui.now = func() time.Time { return now }
	h := now.Truncate(time.Hour).Unix()
	err := e.db.FlushRequests(context.Background(), []meta.RequestDelta{
		{Hour: h, Repo: "maven-releases", Requests: 5, Bytes: 500},
		{Hour: h - 3600, Repo: "maven-releases", Requests: 2, Bytes: 20},
		{Hour: h - 3600, Repo: "npm-hosted", Requests: 1, Bytes: 1},
		{Hour: h - 3*86400, Repo: "maven-releases", Requests: 10, Bytes: 100}, // 3 days ago
		{Hour: h - 20*86400, Repo: "maven-releases", Requests: 100, Bytes: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}

	type bucket = map[string]any
	check := func(path string, wantN int, wantReq, wantBytes float64) []any {
		t.Helper()
		r := e.get(t, path, "admin")
		wantStatus(t, r, http.StatusOK)
		checkAPIHeaders(t, r)
		m := r.json(t)
		bs := m["buckets"].([]any)
		tot := m["totals"].(map[string]any)
		if len(bs) != wantN || tot["requests"] != wantReq || tot["bytes"] != wantBytes {
			t.Fatalf("%s: %d buckets, totals %v: %s", path, len(bs), tot, r.body)
		}
		return bs
	}
	bs := check("/-/api/stats/requests?range=24h", 24, 8, 521)
	last := bs[23].(bucket)
	if last["t"] != "2026-10-20T15:00:00Z" || last["requests"] != float64(5) {
		t.Fatalf("last hourly bucket: %v", last)
	}
	if bs[0].(bucket)["t"] != "2026-10-19T16:00:00Z" {
		t.Fatalf("first hourly bucket: %v", bs[0])
	}
	bs = check("/-/api/stats/requests", 24, 8, 521) // default is 24h
	_ = bs
	bs = check("/-/api/stats/requests?range=7d", 7, 18, 621)
	if d := bs[6].(bucket); d["t"] != "2026-10-20T00:00:00Z" || d["requests"] != float64(8) {
		t.Fatalf("today: %v", d)
	}
	check("/-/api/stats/requests?range=30d", 30, 118, 1621)
	check("/-/api/stats/requests?range=30d&repo=npm-hosted", 30, 1, 1)
}

func TestRequestStatsAPIAccessAndInput(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.get(t, "/-/api/stats/requests", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/stats/requests", "reader"), http.StatusForbidden)
	wantStatus(t, e.get(t, "/-/api/stats/requests?range=1y", "admin"), http.StatusBadRequest)
	wantStatus(t, e.get(t, "/-/api/stats/requests?repo=nope", "admin"), http.StatusNotFound)
	wantStatus(t, e.req(t, "POST", "/-/api/stats/requests", "", "admin", nil, true), http.StatusMethodNotAllowed)
}

func TestAssetJSONCarriesDownloads(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	p := "org/example/app/1.0/app-1.0.jar"
	path := "/-/api/repositories/maven-releases/asset?path=" + url.QueryEscape(p)
	m := e.get(t, path, "reader").json(t)
	if m["downloads"] != float64(0) || m["lastDownloaded"] != "" {
		t.Fatalf("before: %v", m)
	}
	last := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	if err := e.db.FlushDownloads(context.Background(), []meta.DownloadDelta{{Repo: "maven-releases", Path: p, N: 4, Last: last}}); err != nil {
		t.Fatal(err)
	}
	m = e.get(t, path, "reader").json(t)
	if m["downloads"] != float64(4) || m["lastDownloaded"] != "2026-10-20T01:02:03Z" {
		t.Fatalf("after: %v", m)
	}
}

// TestBrowseAndComponentsCarryDownloads: the browse listing and the component
// search give each file its download count and last download (plan 11
// Decision 8: per-asset downloads on the component screens).
func TestBrowseAndComponentsCarryDownloads(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	putAsset(t, e.db, "pypi-hosted", "packages/my-pkg/my_pkg-1.0.tar.gz", 31)
	jar := "org/example/app/1.0/app-1.0.jar"
	sdist := "packages/my-pkg/my_pkg-1.0.tar.gz"
	last := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	if err := e.db.FlushDownloads(context.Background(), []meta.DownloadDelta{
		{Repo: "maven-releases", Path: jar, N: 4, Last: last},
		{Repo: "pypi-hosted", Path: sdist, N: 2, Last: last},
	}); err != nil {
		t.Fatal(err)
	}
	r := e.get(t, "/-/api/repositories/maven-releases/browse?path="+url.QueryEscape("org/example/app/1.0/"), "reader")
	wantStatus(t, r, http.StatusOK)
	found := false
	for _, x := range r.json(t)["entries"].([]any) {
		m := x.(map[string]any)
		if m["name"] == "app-1.0.jar" {
			found = true
			if m["downloads"] != float64(4) || m["lastDownloaded"] != "2026-10-20T01:02:03Z" {
				t.Fatalf("browse entry: %v", m)
			}
		} else if m["dir"] == false && (m["downloads"] != float64(0) || m["lastDownloaded"] != "") {
			t.Fatalf("never downloaded entry: %v", m)
		}
	}
	if !found {
		t.Fatalf("no jar in %s", r.body)
	}
	for q, want := range map[string]string{"name=app&repo=maven-releases": jar, "name=my-pkg": sdist} {
		r = e.get(t, "/-/api/search/components?"+q, "admin")
		wantStatus(t, r, http.StatusOK)
		ok := false
		for _, c := range r.json(t)["components"].([]any) {
			for _, a := range c.(map[string]any)["assets"].([]any) {
				m := a.(map[string]any)
				if m["path"] != want {
					continue
				}
				ok = true
				if m["downloads"] == float64(0) || m["lastDownloaded"] != "2026-10-20T01:02:03Z" {
					t.Fatalf("%s: asset %v", q, m)
				}
			}
		}
		if !ok {
			t.Fatalf("%s: %s not found in %s", q, want, r.body)
		}
	}
}
