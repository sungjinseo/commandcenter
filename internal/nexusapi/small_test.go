package nexusapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Soft quota through the Nexus API: softQuota on create and update (limit
// in MB), the blob store list, and quota-status.
func TestBlobStoreSoftQuota(t *testing.T) {
	e := start(t, true)
	e.want("POST", v1+"/blobstores/file", "ci", `{"name":"bs2"}`, 403)
	e.want("POST", v1+"/blobstores/file", "admin", `{"name":"bs2","path":"bs2","softQuota":{"type":"bytes","limit":1}}`, 400)
	e.want("POST", v1+"/blobstores/file", "admin", `{"name":"bs2","path":"bs2","softQuota":{"type":"spaceUsedQuota","limit":1}}`, 204)
	var stores []struct {
		Name      string
		SoftQuota *struct {
			Type  string
			Limit int64
		}
	}
	e.get(v1+"/blobstores", "admin", &stores)
	if len(stores) != 2 || stores[0].Name != "bs2" || stores[0].SoftQuota == nil || *stores[0].SoftQuota != (struct {
		Type  string
		Limit int64
	}{"spaceUsedQuota", 1}) || stores[1].SoftQuota != nil {
		t.Fatalf("blobstores = %+v", stores)
	}
	var qs map[string]any
	e.get(v1+"/blobstores/bs2/quota-status", "admin", &qs)
	if qs["isViolation"] != false || qs["blobStoreName"] != "bs2" || qs["message"] != "" {
		t.Fatalf("quota-status = %v", qs)
	}
	// 2^40 MB free is more than any filesystem has
	e.want("PUT", v1+"/blobstores/file/bs2", "admin", `{"path":"bs2","softQuota":{"type":"spaceRemainingQuota","limit":1099511627776}}`, 204)
	e.get(v1+"/blobstores/bs2/quota-status", "admin", &qs)
	if qs["isViolation"] != true || !strings.Contains(qs["message"].(string), "soft quota") {
		t.Fatalf("quota-status = %v", qs)
	}
	e.want("GET", v1+"/blobstores/nope/quota-status", "admin", "", 404)
	e.want("PUT", v1+"/blobstores/file/nope", "admin", `{"softQuota":null}`, 404)
	e.want("PUT", v1+"/blobstores/file/bs2", "admin", `{}`, 204) // no softQuota: removed
	e.get(v1+"/blobstores", "admin", &stores)
	if stores[0].SoftQuota != nil {
		t.Fatalf("not removed: %+v", stores[0])
	}
}

// swagger.json is the OpenAPI document of /-/api/openapi.json, public.
func TestSwaggerJSON(t *testing.T) {
	e := start(t, false)
	a := e.want("GET", "/service/rest/swagger.json", "", "", 200)
	b := e.want("GET", "/-/api/openapi.json", "", "", 200)
	var doc struct{ OpenAPI string }
	if string(a) != string(b) || json.Unmarshal(a, &doc) != nil || doc.OpenAPI != "3.0.3" {
		t.Fatalf("swagger.json %.200s", a)
	}
}

// strictContentTypeValidation, raw.contentDisposition and httpClient
// blocked/autoBlock round-trip through the Nexus bodies and act on uploads,
// downloads and remote requests.
func TestContentAndRemoteSettings(t *testing.T) {
	e := start(t, true)
	e.want("POST", v1+"/repositories/raw/hosted", "admin", `{"name":"raw-strict","online":true,
	  "storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"},
	  "raw":{"contentDisposition":"ATTACHMENT"}}`, 201)
	var s map[string]any
	e.get(v1+"/repositories/raw/hosted/raw-strict", "admin", &s)
	if s["storage"].(map[string]any)["strictContentTypeValidation"] != true || s["raw"].(map[string]any)["contentDisposition"] != "ATTACHMENT" {
		t.Fatalf("settings = %v", s)
	}
	e.get(v1+"/repositories/raw/hosted/raw-files", "admin", &s)
	if s["storage"].(map[string]any)["strictContentTypeValidation"] != false || s["raw"].(map[string]any)["contentDisposition"] != "INLINE" {
		t.Fatalf("default settings = %v", s)
	}
	put := func(path, ctype, body string) int {
		t.Helper()
		req, _ := http.NewRequest("PUT", e.ts.URL+"/repository/"+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	e.want("POST", v1+"/repositories/maven/hosted", "admin", `{"name":"mvn-strict","online":true,
	  "storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"},"maven":{"versionPolicy":"RELEASE"}}`, 201)
	for _, c := range []struct {
		path, ctype, body string
		want              int
	}{
		{"raw-strict/a.png", "image/png", "hello", 400},              // declared image, sniffed text
		{"raw-strict/b.txt", "", "\x00\x01\x02binary", 400},          // .txt is text/plain
		{"raw-strict/c.jar", "", "not a zip", 400},                   // the extension's magic
		{"raw-strict/d.jar", "", "PK\x03\x04rest", 201},              // a zip
		{"raw-strict/notes.txt", "", "hello", 201},                   // text as declared
		{"raw-strict/blob", "application/octet-stream", "\x00", 201}, // nothing to check
		{"raw-files/a.png", "image/png", "hello", 201},               // not strict
		{"mvn-strict/org/x/a/1.0/a-1.0.jar", "", "not a zip", 400},
		{"mvn-strict/org/x/a/1.0/a-1.0.jar", "", "PK\x03\x04rest", 201},
	} {
		if got := put(c.path, c.ctype, c.body); got != c.want {
			t.Errorf("PUT %s: %d, want %d", c.path, got, c.want)
		}
	}
	_, _, h := e.do("GET", "/repository/raw-strict/notes.txt", "admin", "")
	if h.Get("Content-Disposition") != `attachment; filename=notes.txt` {
		t.Fatalf("Content-Disposition %q", h.Get("Content-Disposition"))
	}
	if _, _, h := e.do("GET", "/repository/raw-files/a.png", "admin", ""); h.Get("Content-Disposition") != "" {
		t.Fatalf("default Content-Disposition %q", h.Get("Content-Disposition"))
	}

	// A remote that fails: three requests reach it, then it is blocked.
	var hits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/-/npm/v1/security/audits/quick") {
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"forwarded":`+string(b)+`}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	e.want("POST", v1+"/repositories/raw/proxy", "admin", `{"name":"raw-px","online":true,"storage":{"blobStoreName":"default"},
	  "proxy":{"remoteUrl":"`+remote.URL+`/","contentMaxAge":1440,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1},
	  "httpClient":{"blocked":false,"autoBlock":true}}`, 201)
	for i := range 4 {
		if st, _, _ := e.do("GET", "/repository/raw-px/f"+string(rune('a'+i)), "admin", ""); st != http.StatusBadGateway {
			t.Fatalf("request %d: %d", i, st)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("remote hits %d, want 3", hits.Load())
	}
	var view map[string]any
	e.get("/-/api/repositories/raw-px", "ci", &view)
	if rs, _ := view["remoteStatus"].(map[string]any); rs["blocked"] != true || rs["auto"] != true || rs["lastError"] != "remote status 503" || rs["until"] == "" {
		t.Fatalf("remoteStatus = %v", view)
	}
	var anon map[string]any
	e.get("/-/api/repositories/raw-px", "", &anon)
	if _, ok := anon["remoteStatus"]; ok || anon["autoBlock"] != nil {
		t.Fatalf("anonymous sees the remote state: %v", anon)
	}

	// httpClient.blocked and autoBlock round-trip; npm audit is forwarded.
	e.want("POST", v1+"/repositories/npm/proxy", "admin", `{"name":"npm-px","online":true,"storage":{"blobStoreName":"default"},
	  "proxy":{"remoteUrl":"`+remote.URL+`/","contentMaxAge":1440,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1},
	  "httpClient":{"blocked":true,"autoBlock":false}}`, 201)
	e.get(v1+"/repositories/npm/proxy/npm-px", "admin", &s)
	if hc := s["httpClient"].(map[string]any); hc["blocked"] != true || hc["autoBlock"] != false {
		t.Fatalf("httpClient = %v", hc)
	}
	if st, _, _ := e.do("POST", "/repository/npm-px/-/npm/v1/security/audits/quick", "admin", `{"a":1}`); st != http.StatusBadGateway {
		t.Fatalf("audit through a blocked proxy: %d", st)
	}
	e.want("PUT", v1+"/repositories/npm/proxy/npm-px", "admin", `{"name":"npm-px","online":true,"storage":{"blobStoreName":"default"},
	  "proxy":{"remoteUrl":"`+remote.URL+`/","contentMaxAge":1440,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1},
	  "httpClient":{"blocked":false}}`, 204)
	if st, b, _ := e.do("POST", "/repository/npm-px/-/npm/v1/security/audits/quick", "", `{"a":1}`); st != 200 || string(b) != `{"forwarded":{"a":1}}` {
		t.Fatalf("audit: %d %s", st, b)
	}
	e.get(v1+"/repositories/npm/proxy/npm-px", "admin", &s)
	if hc := s["httpClient"].(map[string]any); hc["blocked"] != false || hc["autoBlock"] != false {
		t.Fatalf("autoBlock absent must keep the stored value: %v", hc)
	}
}
