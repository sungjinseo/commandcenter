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
		{"POST", path, `{"name":"p2","format":"maven2"}`, 400, "set criteriaLastBlobUpdated, criteriaLastDownloaded, retain or criteriaAssetRegex"},
		{"POST", path, `{"name":"tmp-only","format":"raw","criteriaAssetRegex":"tmp/.*"}`, 201, `"criteriaAssetRegex":"tmp/.*"`},
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
	if strings.Join(kinds, ",") != "policy.delete,policy.create,policy.update,policy.update,policy.create" { // the second create: tmp-only
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
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","frequency":{"schedule":"cron","cronExpression":"0 0 1 * * *"}}`, 400, "cronExpression"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","frequency":{"schedule":"weekly","startDate":1900000000000,"recurringDays":[2,8]}}`, 400, "recurringDays"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","frequency":{"schedule":"monthly","startDate":1900000000000,"recurringDays":[]}}`, 400, "recurringDays"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","frequency":{"schedule":"daily"}}`, 400, "startDate"},
		{"POST", v1 + "/tasks", `{"type":"blobstore.compact","name":"x","notificationCondition":"ALWAYS","frequency":{"schedule":"manual"}}`, 400, "notificationCondition"},
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

// Nexus task bodies Command Center now takes (wave 6): any cron expression,
// several days a week or a month, mail on success, the type ids it serves
// under another name, their properties, and the ids it refuses with a
// reason.
func TestTasksCompatSchedulesAndTypes(t *testing.T) {
	e := start(t, false)
	if st, b := e.ui("POST", "/-/api/repositories", `{"name":"apt-proxy","format":"apt","type":"proxy","remote":"https://deb.example.org/debian"}`); st != 201 {
		t.Fatalf("apt proxy: %d %s", st, b)
	}
	start := time.Date(2030, 11, 1, 3, 30, 0, 0, time.UTC)
	_, off := start.In(time.Local).Zone()
	zone := fmt.Sprintf("%+03d:%02d", off/3600, off%3600/60) // the server's offset: days do not move
	local := start.In(time.Local)
	hm := fmt.Sprintf("%02d:%02d", local.Hour(), local.Minute())
	create := func(body string) (string, map[string]any) {
		t.Helper()
		var it map[string]any
		json.Unmarshal(e.want("POST", v1+"/tasks", "admin", body, 201), &it)
		var m map[string]any
		e.get("/-/api/tasks/"+it["id"].(string), "admin", &m)
		return it["type"].(string), m
	}
	for _, tc := range []struct{ body, schedule string }{
		{`{"type":"repository.cleanup","name":"q","frequency":{"schedule":"cron","cronExpression":"0 0/15 * * * ?"}}`, "cron:0 0/15 * * * ?"},
		{`{"type":"repository.cleanup","name":"w","frequency":{"schedule":"cron","cronExpression":"0 0 2 ? * mon-fri"}}`, "cron:0 0 2 ? * MON-FRI"},
		{`{"type":"repository.cleanup","name":"m","frequency":{"schedule":"cron","cronExpression":"0 0 2 31 * ?"}}`, "monthly:31:02:00"},
		{fmt.Sprintf(`{"type":"repository.cleanup","name":"wk","frequency":{"schedule":"weekly","startDate":%d,"timeZoneOffset":%q,"recurringDays":[6,2,4]}}`,
			start.UnixMilli(), zone), "weekly:mon,wed,fri:" + hm},
		{fmt.Sprintf(`{"type":"repository.cleanup","name":"mo","frequency":{"schedule":"monthly","startDate":%d,"timeZoneOffset":%q,"recurringDays":[15,1,999]}}`,
			start.UnixMilli(), zone), "monthly:1,15,last:" + hm},
	} {
		if _, m := create(tc.body); m["schedule"] != tc.schedule {
			t.Errorf("%s: schedule %v, want %s", tc.body, m["schedule"], tc.schedule)
		}
	}

	// Mail on success.
	typ, m := create(`{"type":"blobstore.compact","name":"s","notificationCondition":"SUCCESS_FAILURE","alertEmail":"ops@example.com","frequency":{"schedule":"manual"}}`)
	if typ != "blobstore.compact" || m["notifyOn"] != "success_failure" {
		t.Fatalf("success mail: %v", m)
	}
	id := m["id"].(string)
	e.want("PUT", v1+"/tasks/"+id, "admin", `{"notificationCondition":"FAILURE"}`, 204)
	e.get("/-/api/tasks/"+id, "admin", &m)
	if m["notifyOn"] != "failure" {
		t.Fatalf("back to failure: %v", m)
	}

	// Type ids and properties.
	for _, tc := range []struct{ body, nexusType, ccType, params string }{
		{`{"type":"repository.apt.rebuild.metadata","name":"a","frequency":{"schedule":"manual"},
			"properties":{"repositoryName":"*","rebuildAptMetadataFullRebuild":"true","resetProxyMetadata":"true"}}`,
			"repository.apt.rebuild.metadata", "repository.rebuild-metadata", "map[format:apt resetProxyMetadata:true]"},
		{`{"type":"repository.apt.rebuild.metadata","name":"a2","frequency":{"schedule":"manual"},"properties":{"repositoryName":"apt-proxy","resetProxyMetadata":"true"}}`,
			"repository.apt.rebuild.metadata", "repository.rebuild-metadata", "map[format:apt repo:apt-proxy resetProxyMetadata:true]"},
		{`{"type":"blobstore.metrics.reconcile","name":"r","frequency":{"schedule":"manual"},"properties":{"blobstoreName":"(All Blob Stores)"}}`,
			"blobstore.metrics.reconcile", "blobstore.recalculate-size", "map[]"},
		{`{"type":"blobstore.compact","name":"c","frequency":{"schedule":"manual"},"properties":{"blobstoreName":"(All Blob Stores)","blobsOlderThan":"0"}}`,
			"blobstore.compact", "blobstore.compact", "map[]"},
		{`{"type":"blobstore.delete-temp-files","name":"t","frequency":{"schedule":"manual"},"properties":{"blobstoreName":"default","olderThanDays":"2"}}`,
			"blobstore.delete-temp-files", "blobstore.delete-temp-files", "map[blobStore:default olderThanDays:2]"},
		{`{"type":"repository.purge-unused","name":"p","frequency":{"schedule":"manual"},"properties":{"repositoryName":"maven-central","lastUsed":"7"}}`,
			"repository.purge-unused", "repository.purge-unused", "map[lastUsed:7 repos:[maven-central]]"},
		{`{"type":"repository.purge-unused","name":"p2","frequency":{"schedule":"manual"},"properties":{"repositoryName":"*","lastUsed":"30"}}`,
			"repository.purge-unused", "repository.purge-unused", "map[lastUsed:30 repos:[]]"},
	} {
		typ, m := create(tc.body)
		if typ != tc.nexusType || m["type"] != tc.ccType || fmt.Sprint(m["params"]) != tc.params {
			t.Errorf("%s: listed as %s, stored %v %v", tc.body, typ, m["type"], m["params"])
		}
		var item map[string]any
		e.get(v1+"/tasks/"+m["id"].(string), "admin", &item)
		if item["type"] != tc.nexusType {
			t.Errorf("%s: GET type %v", tc.nexusType, item["type"])
		}
	}
	// An update of an apt task keeps it an apt task.
	var list struct{ Items []map[string]any }
	e.get(v1+"/tasks", "admin", &list)
	var aptID string
	for _, it := range list.Items {
		if it["name"] == "a2" {
			aptID = it["id"].(string)
		}
	}
	e.want("PUT", v1+"/tasks/"+aptID, "admin", `{"type":"repository.apt.rebuild.metadata","properties":{"repositoryName":"*","resetProxyMetadata":"true"}}`, 204)
	e.get("/-/api/tasks/"+aptID, "admin", &m)
	if fmt.Sprint(m["params"]) != "map[format:apt resetProxyMetadata:true]" {
		t.Fatalf("apt update: %v", m["params"])
	}

	for _, tc := range []struct {
		body string
		has  string
	}{
		{`{"type":"repository.maven.publish-dotindex","name":"x","frequency":{"schedule":"manual"},"properties":{"repositoryName":"maven-releases"}}`, "/service/rest/v1/search"},
		{`{"type":"repository.maven.unpublish-dotindex","name":"x","frequency":{"schedule":"manual"}}`, "nothing to unpublish"},
		{`{"type":"create.browse.nodes","name":"x","frequency":{"schedule":"manual"}}`, "not needed"},
		{`{"type":"h2.backup.task","name":"x","frequency":{"schedule":"manual"}}`, "pg_dump"},
		{`{"type":"blobstore.compact","name":"x","frequency":{"schedule":"manual"},"properties":{"blobsOlderThan":"3"}}`, "keeps no deleted blobs"},
		{`{"type":"repository.apt.rebuild.metadata","name":"x","frequency":{"schedule":"manual"},"properties":{"repositoryName":"apt-proxy"}}`, "resetProxyMetadata"},
		{`{"type":"repository.apt.rebuild.metadata","name":"x","frequency":{"schedule":"manual"},"properties":{"repositoryName":"maven-releases"}}`, "not apt"},
		{`{"type":"nope","name":"x","frequency":{"schedule":"manual"}}`, "repository.apt.rebuild.metadata"},
	} {
		if b := e.want("POST", v1+"/tasks", "admin", tc.body, 400); !strings.Contains(string(b), tc.has) {
			t.Errorf("%s: %s", tc.body, b)
		}
	}
}

// Several cleanup policies through the Nexus repository API (core-14).
func TestRepositoryPolicyNames(t *testing.T) {
	e := start(t, false)
	for _, p := range []string{"weekly", "keep"} {
		e.want("POST", v1+"/cleanup-policies", "admin", fmt.Sprintf(`{"name":%q,"format":"maven2","criteriaLastBlobUpdated":30}`, p), 201)
	}
	body := `{"name":"maven-releases","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW_ONCE"},
	  "cleanup":{"policyNames":["weekly","keep"]},"maven":{"versionPolicy":"RELEASE","layoutPolicy":"STRICT"}}`
	e.want("PUT", v1+"/repositories/maven/hosted/maven-releases", "admin", body, 204)
	var got map[string]any
	e.get(v1+"/repositories/maven/hosted/maven-releases", "admin", &got)
	if names := fmt.Sprint(got["cleanup"].(map[string]any)["policyNames"]); names != "[weekly keep]" {
		t.Fatalf("policyNames: %s", names)
	}
	e.want("PUT", v1+"/repositories/maven/hosted/maven-releases", "admin", strings.Replace(body, `["weekly","keep"]`, `[]`, 1), 204)
	e.get(v1+"/repositories/maven/hosted/maven-releases", "admin", &got)
	if names := fmt.Sprint(got["cleanup"].(map[string]any)["policyNames"]); names != "[]" {
		t.Fatalf("cleared: %s", names)
	}
}
