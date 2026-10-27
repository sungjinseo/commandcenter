package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const srcPassword = "Nx-Pa55word-secret"

// fakeImportServer answers the import API like the server does. states are
// the answers of the job polls in order; the last one repeats.
type fakeImportServer struct {
	mu        sync.Mutex
	states    []string
	polls     int
	gotBody   map[string]any
	gotAuth   []string
	gotQuery  []string
	stops     int
	afterStop string // the answer of the polls after a stop
}

func (f *fakeImportServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/-/api/import/nexus":
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &f.gotBody)
		dry, _ := f.gotBody["dryRun"].(bool)
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"id": "import.nexus-0a1b2c", "job": "nexus.example-20261027", "dryRun": dry, "started": true})
	case r.Method == "GET" && r.URL.Path == "/-/api/import/jobs/nexus.example-20261027":
		f.gotQuery = append(f.gotQuery, r.URL.RawQuery)
		if f.stops > 0 && f.afterStop != "" {
			fmt.Fprint(w, f.afterStop)
			return
		}
		i := min(f.polls, len(f.states)-1)
		f.polls++
		fmt.Fprint(w, f.states[i])
	case r.Method == "POST" && r.URL.Path == "/-/api/import/jobs/nexus.example-20261027/stop":
		f.stops++
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"job":"nexus.example-20261027","stopping":true}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found"}`)
	}
}

const (
	stRunning  = `{"job":"nexus.example-20261027","status":"running","progress":{"job":"nexus.example-20261027","repository":"npm-h","repositoriesTotal":4,"repositoriesDone":1,"assetsTotal":40,"assetsDone":10,"assetsSkipped":2,"assetsFailed":1,"bytes":3145728}}`
	stStarting = `{"job":"nexus.example-20261027","status":"running","starting":true}`
	stOK       = `{"job":"nexus.example-20261027","status":"ok","report":{"job":"nexus.example-20261027","status":"ok","created":2,
"repositories":[{"source":"npm-h","target":"npm-h","format":"npm","type":"hosted","definition":"created","content":true,"counts":{"assets":5,"imported":3,"skipped":2,"bytes":431}}],
"unsupported":[{"format":"raw","count":1,"repositories":["raw-h"]}],"proxiesNeedingCredentials":["npm-p"],"conflicts":["maven-h: exists as npm/hosted"],
"warnings":[],"notImported":["users, roles and privileges"],"totals":{"assets":5,"imported":3,"skipped":2,"bytes":431}}}`
	stFailed = `{"job":"nexus.example-20261027","status":"failed","report":{"job":"nexus.example-20261027","status":"failed","message":"1 assets failed",
"repositories":[{"source":"pypi-h","target":"pypi-h","format":"pypi","type":"hosted","definition":"exists","content":true,"counts":{"assets":2,"imported":1,"failed":1},"errors":["packages/x.whl: checksum mismatch"]}],
"unsupported":[],"proxiesNeedingCredentials":[],"conflicts":[],"warnings":[],"notImported":[],"totals":{"assets":2,"imported":1,"failed":1}}}`
	stStopped = `{"job":"nexus.example-20261027","status":"stopped","report":{"job":"nexus.example-20261027","status":"stopped","repositories":[],"unsupported":[],
"proxiesNeedingCredentials":[],"conflicts":[],"warnings":[],"notImported":[],"totals":{"imported":3}}}`
	stDry = `{"job":"nexus.example-20261027","status":"dry-run","dryRun":true,"report":{"job":"nexus.example-20261027","dryRun":true,"status":"dry-run","created":4,
"repositories":[{"source":"mvn-h","target":"mvn-h","format":"maven2","type":"hosted","definition":"would-create","content":true,"counts":{"assets":9,"wouldImport":4,"wouldSkip":1,"skipped":4,"bytes":2048}}],
"unsupported":[],"proxiesNeedingCredentials":[],"conflicts":[],"warnings":[],"notImported":[],"totals":{"assets":9,"wouldImport":4,"wouldSkip":1,"bytes":2048}}}`
	stNoReport = `{"job":"nexus.example-20261027","status":"failed","noReport":true,"message":"the source password is not available"}`
)

// importCLI runs the import command against f with the token in the
// environment and the given stdin, and checks that no secret was printed.
func importCLI(t *testing.T, f *fakeImportServer, stdin io.Reader, args ...string) (string, string, error) {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	t.Setenv("COMMANDCENTER_TOKEN", secretToken)
	var out, errb bytes.Buffer
	oldErr, oldPoll := stderr, importPoll
	stderr, importPoll = &errb, 5*time.Millisecond
	t.Cleanup(func() { stderr, importPoll = oldErr, oldPoll })
	full := append([]string{"import", "nexus", "--server", ts.URL, "--from", "https://nexus.example"}, args...)
	err := run(full, stdin, &out)
	for _, s := range []string{out.String(), errb.String(), fmt.Sprint(err)} {
		if strings.Contains(s, secretToken) || strings.Contains(s, srcPassword) {
			t.Fatalf("a secret was printed: %q", s)
		}
	}
	return out.String(), errb.String(), err
}

func noPrompt(t *testing.T) {
	t.Helper()
	old := readSecret
	readSecret = func(string) (string, error) { t.Fatal("unexpected password prompt"); return "", nil }
	t.Cleanup(func() { readSecret = old })
}

func TestImportCLIUsage(t *testing.T) {
	t.Setenv("COMMANDCENTER_TOKEN", "")
	t.Setenv("NEXUS_PASSWORD", "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"import"}, "usage: commandcenter import nexus"},
		{[]string{"import", "artifactory"}, "usage: commandcenter import nexus"},
		{[]string{"import", "nexus", "--from", "https://n.example"}, "--server and --from are required"},
		{[]string{"import", "nexus", "--server", "ftp://cc", "--from", "https://n.example"}, "--server must be an http(s) URL"},
		{[]string{"import", "nexus", "--server", "http://localhost:8081", "--from", "https://admin:pw@n.example"}, "--from must not contain credentials"},
		{[]string{"import", "nexus", "--server", "http://localhost:8081", "--from", "https://n.example"}, "a token is required"},
		{[]string{"import", "nexus", "--password", "x"}, "flag provided but not defined: -password"},
	} {
		err := run(c.args, nil, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
}

// The password comes from NEXUS_PASSWORD; the flags map to the API body.
func TestImportCLIStartWithEnvPassword(t *testing.T) {
	noPrompt(t)
	t.Setenv("NEXUS_PASSWORD", srcPassword)
	f := &fakeImportServer{states: []string{stOK}}
	out, _, err := importCLI(t, f, nil, "--user", "admin", "--repos", "npm-h, mvn-h,", "--definitions=false",
		"--blob-store", "fast", "--rename-prefix", "nx-", "--job", "j1", "--concurrency", "8", "--retry-failed", "--insecure-skip-verify")
	if err != nil {
		t.Fatal(err)
	}
	b := f.gotBody
	if b["password"] != srcPassword || b["username"] != "admin" || b["url"] != "https://nexus.example" || b["importDefinitions"] != false ||
		b["importContent"] != true || b["blobStore"] != "fast" || b["renamePrefix"] != "nx-" || b["job"] != "j1" || b["concurrency"] != float64(8) ||
		b["retryFailed"] != true || b["insecureSkipVerify"] != true || b["dryRun"] != false || fmt.Sprint(b["repositories"]) != "[npm-h mvn-h]" {
		t.Fatalf("body: %v", b)
	}
	if f.gotAuth[0] != "Bearer "+secretToken {
		t.Fatalf("auth: %q", f.gotAuth[0])
	}
	if !strings.Contains(out, "import started: job nexus.example-20261027") || f.polls != 0 {
		t.Fatalf("without --wait: %q, %d polls", out, f.polls)
	}
}

func TestImportCLIPrompt(t *testing.T) {
	t.Setenv("NEXUS_PASSWORD", "")
	old := readSecret
	t.Cleanup(func() { readSecret = old })
	var asked string
	readSecret = func(p string) (string, error) { asked = p; return srcPassword, nil }
	f := &fakeImportServer{states: []string{stOK}}
	if _, _, err := importCLI(t, f, nil, "--user", "admin"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, "admin") || !strings.Contains(asked, "nexus.example") || f.gotBody["password"] != srcPassword {
		t.Fatalf("prompt %q, body %v", asked, f.gotBody)
	}
	// No terminal: nothing is sent and the message names the variable.
	readSecret = func(string) (string, error) { return "", errNoTTY }
	f = &fakeImportServer{states: []string{stOK}}
	if _, _, err := importCLI(t, f, nil, "--user", "admin"); !errors.Is(err, errNoTTY) || !strings.Contains(err.Error(), "NEXUS_PASSWORD") || f.gotBody != nil {
		t.Fatalf("no tty: %v %v", err, f.gotBody)
	}
	// Anonymous source: no password is asked or sent.
	readSecret = func(string) (string, error) { t.Fatal("prompted without --user"); return "", nil }
	f = &fakeImportServer{states: []string{stOK}}
	if _, _, err := importCLI(t, f, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.gotBody["password"]; ok {
		t.Fatalf("body: %v", f.gotBody)
	}
}

// --wait prints a progress line per poll, then the report; the polls carry the run id.
func TestImportCLIWait(t *testing.T) {
	noPrompt(t)
	t.Setenv("NEXUS_PASSWORD", srcPassword)
	f := &fakeImportServer{states: []string{stStarting, stRunning, stRunning, stOK}}
	out, errOut, err := importCLI(t, f, nil, "--user", "admin", "--wait", "--insecure-skip-verify")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"starting", "repositories 1/4  assets 13/40 (imported 10, skipped 2, failed 1)  3.0 MiB  current: npm-h",
		"import job nexus.example-20261027: ok", "npm-h", "npm/hosted", "unsupported in this version: raw (1): raw-h",
		"proxies needing credentials: npm-p", "conflict: maven-h: exists as npm/hosted", "not imported: users, roles and privileges"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "--insecure-skip-verify") {
		t.Errorf("no insecure warning: %q", errOut)
	}
	if f.gotQuery[0] != "run=import.nexus-0a1b2c" {
		t.Errorf("poll query %q", f.gotQuery[0])
	}
}

func TestImportCLIWaitOutcomes(t *testing.T) {
	noPrompt(t)
	t.Setenv("NEXUS_PASSWORD", srcPassword)
	_, _, err := importCLI(t, &fakeImportServer{states: []string{stFailed}}, nil, "--user", "admin", "--wait")
	if err == nil || !strings.Contains(err.Error(), "1 assets failed") {
		t.Fatalf("failed assets: %v", err)
	}
	out, _, err := importCLI(t, &fakeImportServer{states: []string{stDry}}, nil, "--user", "admin", "--wait", "--dry-run")
	if err != nil || !strings.Contains(out, "dry run (nothing is written) started") || !strings.Contains(out, "would-create") || !strings.Contains(out, "4 assets (2048 bytes) would be imported") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if _, _, err = importCLI(t, &fakeImportServer{states: []string{stNoReport}}, nil, "--user", "admin", "--wait"); err == nil || !strings.Contains(err.Error(), "password is not available") {
		t.Fatalf("no report: %v", err)
	}
}

// fakeInterrupts replaces Ctrl-C with a channel the test sends on.
func fakeInterrupts(t *testing.T) chan os.Signal {
	t.Helper()
	c := make(chan os.Signal, 2)
	old := interrupts
	interrupts = func() (<-chan os.Signal, func()) { return c, func() {} }
	t.Cleanup(func() { interrupts = old })
	return c
}

// Ctrl-C asks; "y" stops the import through the API and the run ends stopped.
func TestImportCLICtrlCStops(t *testing.T) {
	noPrompt(t)
	t.Setenv("NEXUS_PASSWORD", srcPassword)
	sig := fakeInterrupts(t)
	sig <- os.Interrupt
	f := &fakeImportServer{states: []string{stRunning}, afterStop: stStopped}
	out, errOut, err := importCLI(t, f, strings.NewReader("y\n"), "--user", "admin", "--wait")
	if f.stops != 1 || err == nil || !strings.Contains(err.Error(), "stopped") || !strings.Contains(errOut, "stop import job nexus.example-20261027 on the server?") ||
		!strings.Contains(out, "stopping the import") {
		t.Fatalf("stops %d, err %v\n%s\n%s", f.stops, err, out, errOut)
	}
}

// "n" keeps the import running and following goes on.
func TestImportCLICtrlCContinues(t *testing.T) {
	noPrompt(t)
	t.Setenv("NEXUS_PASSWORD", srcPassword)
	sig := fakeInterrupts(t)
	sig <- os.Interrupt
	f := &fakeImportServer{states: []string{stRunning, stRunning, stOK}}
	_, errOut, err := importCLI(t, f, strings.NewReader("n\n"), "--user", "admin", "--wait")
	if err != nil || f.stops != 0 || !strings.Contains(errOut, "the import goes on") {
		t.Fatalf("stops %d, err %v, %q", f.stops, err, errOut)
	}
}

// A second Ctrl-C while asking leaves the import running and exits.
func TestImportCLICtrlCTwiceDetaches(t *testing.T) {
	noPrompt(t)
	t.Setenv("NEXUS_PASSWORD", srcPassword)
	sig := fakeInterrupts(t)
	sig <- os.Interrupt
	pr, pw := io.Pipe() // no answer is ever typed
	t.Cleanup(func() { pw.Close() })
	go func() {
		time.Sleep(50 * time.Millisecond)
		sig <- os.Interrupt
	}()
	f := &fakeImportServer{states: []string{stRunning}}
	_, _, err := importCLI(t, f, pr, "--user", "admin", "--wait")
	if !errors.Is(err, errDetached) || f.stops != 0 {
		t.Fatalf("err %v, stops %d", err, f.stops)
	}
}
