package nexusapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ui sends a web UI API request as admin (basic auth, the UI header).
func (e *env) ui(method, path, body string) (int, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	req.SetBasicAuth("admin", "pw")
	req.Header.Set("X-Command-Center-UI", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestCleanupPoliciesCompat(t *testing.T) {
	e := start(t, false)
	const path = v1 + "/cleanup-policies"
	body := `{"name":"mvn-keep","format":"maven2","notes":"keep three","criteriaLastBlobUpdated":30,
		"criteriaReleaseType":"RELEASES","criteriaAssetRegex":".*\\.jar","retain":3,"sortBy":"version"}`
	b := e.want("POST", path, "admin", body, 201)
	want := map[string]any{"name": "mvn-keep", "notes": "keep three", "format": "maven2", "criteriaLastBlobUpdated": float64(30),
		"criteriaLastDownloaded": nil, "criteriaReleaseType": "RELEASES", "criteriaAssetRegex": `.*\.jar`, "retain": float64(3), "sortBy": "version"}
	check := func(what string, got map[string]any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: %v\nwant %v", what, got, want)
		}
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	check("create", got)
	var list []map[string]any
	e.get(path, "admin", &list)
	if len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	check("list", list[0])
	e.get(path+"/mvn-keep", "admin", &got)
	check("get", got)

	// Fields Nexus does not have survive an update through this API.
	if st, b := e.ui("PUT", "/-/api/cleanup/policies/mvn-keep", `{"format":"maven","componentAgeDays":30,"releaseType":"release",
		"pathPattern":".*\\.jar","retain":3,"notes":"keep three","exclude":{"versionPattern":"1\\..*"}}`); st != 200 {
		t.Fatalf("ui put: %d %s", st, b)
	}
	e.want("PUT", path+"/mvn-keep", "admin", `{"name":"mvn-keep","format":"*","criteriaLastDownloaded":10,"criteriaReleaseType":"PRERELEASES"}`, 204)
	e.get(path+"/mvn-keep", "admin", &got)
	want = map[string]any{"name": "mvn-keep", "notes": "", "format": "*", "criteriaLastBlobUpdated": nil, "criteriaLastDownloaded": float64(10),
		"criteriaReleaseType": "PRERELEASES", "criteriaAssetRegex": nil, "retain": nil, "sortBy": nil}
	check("updated", got)
	_, ub := e.ui("GET", "/-/api/cleanup/policies/mvn-keep", "")
	if !strings.Contains(string(ub), `"exclude":{"namePattern":"","versionPattern":"1\\..*"}`) {
		t.Fatalf("exclude lost: %s", ub)
	}

	// Errors in Nexus's shapes, the UI's field names replaced.
	for _, tc := range []struct {
		method, path, body string
		status             int
		has                string
	}{
		{"POST", path, body, 409, "already exists"},
		{"POST", path, `{"name":"p2","format":"maven2","criteriaAssetRegex":"("}`, 400, `"id":"criteriaAssetRegex"`},
		{"POST", path, `{"name":"p2","format":"maven2"}`, 400, "set criteriaLastBlobUpdated, criteriaLastDownloaded or retain"},
		{"POST", path, `{"name":"p2","format":"maven2","criteriaLastBlobUpdated":1,"criteriaReleaseType":"BOGUS"}`, 400, `"id":"criteriaReleaseType"`},
		{"POST", path, `{"name":"p2","format":"bower","criteriaLastBlobUpdated":1}`, 400, "unknown format"},
		{"POST", path, `{"name":"p2","format":"maven2","retain":2,"sortBy":"size"}`, 400, `unknown sortBy`},
		{"PUT", path + "/mvn-keep", `{"name":"other","format":"maven2","retain":1}`, 400, "renamed"},
		{"PUT", path + "/missing", `{"format":"maven2","retain":1}`, 404, "not found"},
		{"GET", path + "/missing", "", 404, "not found"},
	} {
		if b := e.want(tc.method, tc.path, "admin", tc.body, tc.status); !strings.Contains(string(b), tc.has) {
			t.Errorf("%s %s %s: %s", tc.method, tc.path, tc.body, b)
		}
	}
	e.want("GET", path, "ci", "", 403)
	e.want("GET", path, "", "", 401)
	e.want("POST", path, "ci", body, 403)

	e.want("DELETE", path+"/mvn-keep", "admin", "", 204)
	e.want("DELETE", path+"/mvn-keep", "admin", "", 404)
	var events struct {
		Events []struct{ Kind, Subject, Actor string }
	}
	e.get("/-/api/events?limit=10", "admin", &events)
	var kinds []string
	for _, ev := range events.Events {
		if strings.HasPrefix(ev.Kind, "policy.") && ev.Actor == "admin" {
			kinds = append(kinds, ev.Kind)
		}
	}
	if strings.Join(kinds, ",") != "policy.delete,policy.update,policy.update,policy.create" {
		t.Fatalf("events: %v", kinds)
	}
}

func TestTasksCompat(t *testing.T) {
	e := start(t, false)
	start := time.Date(2030, 11, 1, 3, 30, 0, 0, time.UTC)
	body := fmt.Sprintf(`{"type":"blobstore.compact","name":"Compact nightly","enabled":true,"alertEmail":"ops@example.com",
		"notificationCondition":"FAILURE","frequency":{"schedule":"daily","startDate":%d,"timeZoneOffset":"+00:00","recurringDays":[0]},
		"properties":{"blobstoreName":"default"}}`, start.UnixMilli())
	var created map[string]any
	json.Unmarshal(e.want("POST", v1+"/tasks", "admin", body, 201), &created)
	id, _ := created["id"].(string)
	if id == "" || created["type"] != "blobstore.compact" || created["name"] != "Compact nightly" || created["currentState"] != "WAITING" {
		t.Fatalf("create: %v", created)
	}
	uiTask := func() map[string]any {
		t.Helper()
		var m map[string]any
		e.get("/-/api/tasks/"+id, "admin", &m)
		return m
	}
	local := start.In(time.Local)
	m := uiTask()
	if m["schedule"] != fmt.Sprintf("daily:%02d:%02d", local.Hour(), local.Minute()) || fmt.Sprint(m["params"]) != "map[blobStore:default]" ||
		fmt.Sprint(m["notifyEmail"]) != "[ops@example.com]" || m["enabled"] != true {
		t.Fatalf("stored: %v", m)
	}
	var item map[string]any
	e.get(v1+"/tasks/"+id, "admin", &item)
	if item["id"] != id || item["type"] != "blobstore.compact" {
		t.Fatalf("get: %v", item)
	}

	// Update: weekly on Monday (2) at 03:30 UTC, renamed; the rest is kept.
	e.want("PUT", v1+"/tasks/"+id, "admin", fmt.Sprintf(`{"name":"Compact weekly","enabled":false,
		"frequency":{"schedule":"weekly","startDate":%d,"recurringDays":[2]}}`, start.UnixMilli()), 204)
	m = uiTask()
	mon := time.Date(2030, 11, 4, 3, 30, 0, 0, time.UTC).In(time.Local) // the first Monday from the start
	wantSched := fmt.Sprintf("weekly:%s:%02d:%02d", strings.ToLower(mon.Weekday().String()[:3]), mon.Hour(), mon.Minute())
	if m["name"] != "Compact weekly" || m["schedule"] != wantSched || m["enabled"] != false || fmt.Sprint(m["params"]) != "map[blobStore:default]" ||
		fmt.Sprint(m["notifyEmail"]) != "[ops@example.com]" {
		t.Fatalf("updated: %v (want %s)", m, wantSched)
	}

	// Cron, a once in seconds, our own type ids and parameter names.
	for _, tc := range []struct{ body, schedule string }{
		{`{"type":"repository.cleanup","name":"c1","frequency":{"schedule":"cron","cronExpression":"0 15 4 ? * MON"}}`, "weekly:mon:04:15"},
		{`{"type":"repository.cleanup","name":"c2","frequency":{"schedule":"cron","cronExpression":"0 0 2 1 * ?"}}`, "monthly:1:02:00"},
		{`{"type":"repository.cleanup","name":"c3","frequency":{"schedule":"manual"}}`, "manual"},
		{fmt.Sprintf(`{"type":"repository.verify-blobs","name":"c4","frequency":{"schedule":"once","startDate":%d,"timeZoneOffset":"+09:00"},
			"properties":{"repositoryName":"maven-releases","checksum":"true"}}`, start.Unix()), "once:2030-11-01T12:30:00+09:00"},
	} {
		var it map[string]any
		json.Unmarshal(e.want("POST", v1+"/tasks", "admin", tc.body, 201), &it)
		var m map[string]any
		e.get("/-/api/tasks/"+it["id"].(string), "admin", &m)
		if m["schedule"] != tc.schedule {
			t.Errorf("%s: schedule %v, want %s", tc.body, m["schedule"], tc.schedule)
		}
		if params := m["params"].(map[string]any); strings.Contains(tc.body, "verify") && (params["checksum"] != true || params["repository"] != "maven-releases") {
			t.Errorf("verify params: %v", m["params"])
		}
	}

	for _, tc := range []struct {
		method, path, body string
		status             int
		has                string
	}{
		{"POST", v1 + "/tasks", `{"type":"tags.cleanup","name":"x","frequency":{"schedule":"manual"}}`, 400, "supported are blobstore.compact"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x"}`, 400, "frequency"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","frequency":{"schedule":"cron","cronExpression":"0 */5 * * * ?"}}`, 400, "cronExpression"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","frequency":{"schedule":"weekly","startDate":1900000000000,"recurringDays":[2,4]}}`, 400, "recurringDays"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","frequency":{"schedule":"daily"}}`, 400, "startDate"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","notificationCondition":"SUCCESS_FAILURE","frequency":{"schedule":"manual"}}`, 400, "notificationCondition"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","frequency":{"schedule":"manual"},"properties":{"blobstoreName":"nope"}}`, 400, "nope"},
		{"PUT", v1 + "/tasks/" + id, `{"type":"repository.cleanup"}`, 400, "cannot change"},
		{"PUT", v1 + "/tasks/missing", `{"name":"x"}`, 404, "not found"},
		{"POST", v1 + "/tasks/" + id + "/stop", "", 409, "not running"},
	} {
		if b := e.want(tc.method, tc.path, "admin", tc.body, tc.status); !strings.Contains(string(b), tc.has) {
			t.Errorf("%s %s %s: %s", tc.method, tc.path, tc.body, b)
		}
	}
	e.want("POST", v1+"/tasks", "ci", body, 403)
	e.want("DELETE", v1+"/tasks/"+id, "ci", "", 403)

	e.want("POST", v1+"/tasks/"+id+"/run", "admin", "", 204)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		e.get(v1+"/tasks/"+id, "admin", &item)
		if item["currentState"] != "RUNNING" && item["lastRunResult"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish: %v", item)
		}
	}
	e.want("DELETE", v1+"/tasks/"+id, "admin", "", 204)
	e.want("GET", v1+"/tasks/"+id, "admin", "", 404)
}

func TestSystemInformationCompat(t *testing.T) {
	t.Setenv("COMMANDCENTER_SECRET_TEST", "topsecret")
	t.Setenv("HTTPS_PROXY", "http://user:pw@proxy.example:3128")
	e := start(t, false)
	const path = "/service/rest/atlas/system-information"
	e.want("GET", path, "", "", 401)
	e.want("GET", path, "ci", "", 403)
	b := e.want("GET", path, "admin", "", 200)
	var m map[string]map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	for _, k := range []string{"nexus-status", "nexus-node", "nexus-configuration", "nexus-properties", "system-time", "system-runtime", "system-filestores"} {
		if m[k] == nil {
			t.Fatalf("missing %s: %s", k, b)
		}
	}
	rt := m["system-runtime"]
	if rt["goroutines"] == nil || rt["gomaxprocs"] == nil || rt["availableProcessors"] == nil || rt["heapAlloc"] == nil {
		t.Fatalf("runtime: %v", rt)
	}
	fs, _ := m["system-filestores"]["default"].(map[string]any)
	if fs == nil || fs["type"] != "file" || fs["usableSpace"] == nil {
		t.Fatalf("filestores: %v", m["system-filestores"])
	}
	if m["nexus-properties"]["HTTPS_PROXY"] != "yes" || m["nexus-status"]["edition"] != "Command Center" {
		t.Fatalf("properties/status: %v %v", m["nexus-properties"], m["nexus-status"])
	}
	for _, bad := range []string{"topsecret", "COMMANDCENTER_SECRET_TEST", "proxy.example", "$2a$", "passwordHash"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("leaked %q: %s", bad, b)
		}
	}
}
