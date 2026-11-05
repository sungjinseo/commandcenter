package nexusapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// "Test connection" of a Nexus Repository Manager 3.x server in Jenkins
// (Nexus Platform plugin 3.14.431) asks for the version first, with Basic
// credentials and no Accept header, and parses the answer as XML; only a
// PRO of 3.13.0 or newer passes without its warning.
func TestWonderlandStatus(t *testing.T) {
	e := start(t, true)
	const p = "/service/rest/wonderland/status"
	st, b, hdr := e.do("GET", p, "ci", "")
	if st != 200 || hdr.Get("Content-Type") != "application/xml" ||
		string(b) != "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<status><edition>PRO</edition><version>3.91.1-04</version></status>" {
		t.Fatalf("status: %d %q %s", st, hdr.Get("Content-Type"), b)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+p, nil)
	req.SetBasicAuth("ci", "pw")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct{ Edition, Version string }
	if b, _ := io.ReadAll(resp.Body); json.Unmarshal(b, &got) != nil || got.Edition != "PRO" || got.Version != "3.91.1-04" {
		t.Fatalf("JSON status: %s", b)
	}
	// The version is for signed-in callers, as in Nexus.
	if st, _, hdr := e.do("GET", p, "", ""); st != 401 || hdr.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous: %d", st)
	}
}

// nexus3-cli 4.2.1 installs its Groovy scripts before every repository
// command (HEAD script/<name>, then POST script) and runs them (POST
// script/<name>/run). There is no script API: each request gets the
// status of a Nexus with script creation off, and the reason.
func TestScriptAPI(t *testing.T) {
	e := start(t, true)
	const why = "no script API"
	e.want("HEAD", v1+"/script/nexus3-cli-repository-get", "admin", "", 404)
	// The message is what nexus3-cli shows: it takes an answer without one for a crash.
	var res struct{ Message string }
	b := e.want("POST", v1+"/script", "admin", `{"type": "groovy", "name": "nexus3-cli-repository-get", "content": "repository.repositoryManager.get(args)"}`, 410)
	if json.Unmarshal(b, &res) != nil || !strings.Contains(res.Message, why) {
		t.Fatalf("create: %s", b)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/script/nexus3-cli-repository-get/run"}, {"GET", "/script/hello"}, {"PUT", "/script/hello"}, {"DELETE", "/script/hello"},
	} {
		if b := e.want(c.method, v1+c.path, "ci", "", 404); !strings.Contains(string(b), why) {
			t.Errorf("%s %s: %s", c.method, c.path, b)
		}
	}
	if b := e.want("GET", v1+"/script", "ci", "", 200); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("list: %s", b)
	}
	e.want("GET", v1+"/script", "", "", 401)
	e.want("POST", v1+"/script", "", `{"name":"x"}`, 401)
	e.want("POST", v1+"/script/x/run", "", "", 401)
}
