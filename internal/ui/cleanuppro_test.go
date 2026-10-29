package ui

import (
	"context"
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

const draftPath = "/-/api/cleanup/preview"

func TestCleanupPolicyProFieldsAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.admin(t, "POST", policiesPath, `{"name":"keep3","format":"maven","retain":3,"pathPattern":".*\\.jar",
		"exclude":{"namePattern":"org\\.example:keep","versionPattern":".*-SNAPSHOT"},"notes":"three newest"}`)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	ex := m["exclude"].(map[string]any)
	if m["retain"] != float64(3) || m["retainSortBy"] != "version" || m["pathPattern"] != `.*\.jar` || m["notes"] != "three newest" ||
		ex["namePattern"] != `org\.example:keep` || ex["versionPattern"] != ".*-SNAPSHOT" {
		t.Fatalf("create: %s", r.body)
	}
	r = e.admin(t, "PUT", policiesPath+"/keep3", `{"format":"maven","retain":2,"retainSortBy":"date"}`)
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["retain"] != float64(2) || m["retainSortBy"] != "date" || m["pathPattern"] != "" || m["notes"] != "" {
		t.Fatalf("update: %s", r.body)
	}
	for _, body := range []string{
		`{"name":"p","format":"all","retain":1001}`,
		`{"name":"p","format":"all","retain":1,"retainSortBy":"size"}`,
		`{"name":"p","format":"all","retain":1,"exclude":{"namePattern":"("}}`,
		`{"name":"p","format":"all","retain":1,"exclude":{"other":"x"}}`,
	} {
		if r := e.admin(t, "POST", policiesPath, body); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, r.status, r.body)
		}
	}
}

func TestCleanupDraftPreviewAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Tasks.Now = func() time.Time { return t0.Add(60 * 24 * time.Hour) }
	ctx := context.Background()
	for _, p := range []string{"org/example/app/2.0/app-2.0.jar", "org/example/app/1.10/app-1.10.jar"} {
		if err := e.db.PutAsset(ctx, meta.Asset{Repo: "maven-releases", Path: p, BlobSHA256: "s", Size: 2, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(body string) resp { return e.admin(t, "POST", draftPath, body) }

	// Age only: every component of maven-releases (app 1.0, 1.10, 2.0 and pad-util 1.0).
	r := post(`{"format":"maven","componentAgeDays":30,"repository":"maven-releases"}`)
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if m["total"] != float64(4) || m["truncated"] != false || m["bytes"] != float64(8+2+2+11) {
		t.Fatalf("age: %s", r.body)
	}
	c := m["components"].([]any)[0].(map[string]any)
	if c["group"] != "org.example" || c["name"] != "app" || c["version"] != "1.0" || c["lastUpdated"] != rfc3339(t0) ||
		c["lastDownloaded"] != "" || c["assets"] != float64(2) || !strings.Contains(c["reason"].(string), "componentAgeDays 30") {
		t.Fatalf("component: %v", c)
	}

	// Retain 1 by version keeps app 2.0 (not 1.10) and the single pad-util.
	r = post(`{"format":"maven","retain":1,"repository":"maven-releases"}`)
	wantStatus(t, r, http.StatusOK)
	if got := versionsJSON(r.json(t)); got != "app:1.0,app:1.10" {
		t.Fatalf("retain: %s", r.body)
	}
	// Exclusion.
	r = post(`{"format":"maven","componentAgeDays":30,"exclude":{"versionPattern":"1\\..*"},"repository":"maven-releases"}`)
	if got := versionsJSON(r.json(t)); got != "app:2.0" {
		t.Fatalf("exclude: %s", r.body)
	}

	// The list cap: total still counts every candidate.
	defer func(n int) { previewDraftLimit = n }(previewDraftLimit)
	previewDraftLimit = 1
	m = post(`{"format":"maven","componentAgeDays":30,"repository":"maven-releases"}`).json(t)
	if len(m["components"].([]any)) != 1 || m["total"] != float64(4) || m["truncated"] != true {
		t.Fatalf("cap: %v", m)
	}

	// CSV: every candidate, a header row, not capped.
	r = e.admin(t, "POST", draftPath+"?format=csv", `{"name":"x","format":"all","componentAgeDays":30,"repository":"maven-releases"}`)
	wantStatus(t, r, http.StatusOK)
	if ct := r.header.Get("Content-Type"); ct != "text/csv; charset=utf-8" ||
		r.header.Get("Content-Disposition") != `attachment; filename="cleanup-preview-maven-releases.csv"` {
		t.Fatalf("csv headers: %v", r.header)
	}
	rows, err := csv.NewReader(strings.NewReader(r.body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || strings.Join(rows[0], ",") != "group,name,version,lastUpdated,lastDownloaded,assets,bytes,reason" ||
		strings.Join(rows[1][:7], ",") != "org.example,app,1.0,"+rfc3339(t0)+",,2,8" {
		t.Fatalf("csv: %q", rows)
	}

	// Refusals.
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{draftPath, `{"format":"maven","componentAgeDays":30}`, http.StatusBadRequest},                                    // no repository
		{draftPath, `{"format":"maven","componentAgeDays":30,"repository":"nope"}`, http.StatusNotFound},                  // unknown
		{draftPath, `{"format":"maven","componentAgeDays":30,"repository":"maven-public"}`, http.StatusBadRequest},        // group
		{draftPath, `{"format":"npm","componentAgeDays":30,"repository":"maven-releases"}`, http.StatusBadRequest},        // format
		{draftPath, `{"format":"maven","repository":"maven-releases"}`, http.StatusBadRequest},                            // no criterion
		{draftPath, `{"format":"maven","retain":5000,"repository":"maven-releases"}`, http.StatusBadRequest},              // invalid
		{draftPath + "?format=xml", `{"format":"maven","retain":1,"repository":"maven-releases"}`, http.StatusBadRequest}, // output
	} {
		if r := e.admin(t, "POST", tc.path, tc.body); r.status != tc.status {
			t.Errorf("%s %s: %d %s", tc.path, tc.body, r.status, r.body)
		}
	}
	body := `{"format":"maven","retain":1,"repository":"maven-releases"}`
	wantStatus(t, e.req(t, "POST", draftPath, body, "", nil, true), http.StatusUnauthorized)
	wantStatus(t, e.req(t, "POST", draftPath, body, "reader", nil, true), http.StatusForbidden)
	// Nothing was deleted.
	if as, _ := e.db.ListAssets(ctx, "maven-releases", "org/example/"); len(as) != 6 {
		t.Fatalf("assets: %d", len(as))
	}
}

func versionsJSON(m map[string]any) string {
	var out []string
	for _, c := range m["components"].([]any) {
		c := c.(map[string]any)
		out = append(out, c["name"].(string)+":"+c["version"].(string))
	}
	return strings.Join(out, ",")
}

func TestCSVCell(t *testing.T) {
	for in, want := range map[string]string{"=cmd|' /C calc'!A0": "'=cmd|' /C calc'!A0", "+1": "'+1", "-x": "'-x", "@a": "'@a", "a=b": "a=b", "": ""} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q", in, got)
		}
	}
}
