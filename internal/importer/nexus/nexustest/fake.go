// Package nexustest serves a fake Nexus Repository 3 for tests, built from
// the responses recorded from a real Nexus 3.97 in ../testdata: the
// repositories list, the repository settings, the asset listings of the
// hosted maven2, npm, pypi and cargo repositories (paged with
// continuationToken), the artifacts themselves, the npm packuments and the
// cargo sparse index.
package nexustest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The address the fixtures were recorded from; replaced by the fake's URL.
const recordedBase = "http://localhost:18093"

// Fake is a running fake source.
type Fake struct {
	*httptest.Server
	User, Password string

	// PageSize splits the asset listings into pages; 0 serves one page.
	PageSize int
	// BeforeDownload, when set, runs before an artifact is served; it may
	// block (it should return when the request's context is done).
	BeforeDownload func(r *http.Request, repo, path string)

	// FailAuthAfter, when > 0, answers 401 to every request once that many
	// artifacts were served (credentials revoked mid-run).
	FailAuthAfter int64
	// Settings, when set, rewrites the repositorySettings response.
	Settings func([]byte) []byte

	Downloads atomic.Int64 // artifacts served
	mu        sync.Mutex
	nonGET    []string
	override  map[string][]byte // repo/path -> body
	dir       string
}

// Dir is the testdata directory of the fixtures.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata")
}

var assetFiles = map[string]string{"mvn-h": "real-assets-maven.json", "npm-h": "real-assets-npm.json",
	"pypi-h": "real-assets-pypi.json", "cargo-h": "real-assets-cargo.json"}

var packuments = map[string]string{"demo": "real-npm-packument-demo.json", "@acme/scoped": "real-npm-packument-acme-scoped.json"}

// New starts a fake that requires Basic auth with user and password.
func New(t testing.TB, user, password string) *Fake {
	f := &Fake{User: user, Password: password, dir: Dir(), override: map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// NonGET returns the requests that were not GET (the importer must send none).
func (f *Fake) NonGET() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.nonGET...)
}

func (f *Fake) read(name string) []byte {
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		panic(err)
	}
	return bytes.ReplaceAll(b, []byte(recordedBase), []byte(f.URL))
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.mu.Lock()
		f.nonGET = append(f.nonGET, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	revoked := f.FailAuthAfter > 0 && f.Downloads.Load() >= f.FailAuthAfter
	if u, p, ok := r.BasicAuth(); !ok || u != f.User || p != f.Password || revoked {
		w.Header().Set("WWW-Authenticate", `BASIC realm="Sonatype Nexus Repository Manager"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch p := r.URL.Path; {
	case p == "/service/rest/v1/repositories":
		writeJSON(w, f.read("real-repositories.json"))
	case p == "/service/rest/v1/repositorySettings":
		b := f.read("real-repository-settings.json")
		if f.Settings != nil {
			b = f.Settings(b)
		}
		writeJSON(w, b)
	case p == "/service/rest/v1/assets":
		f.assets(w, r)
	case strings.HasPrefix(p, "/repository/"):
		repo, rest, _ := strings.Cut(strings.TrimPrefix(p, "/repository/"), "/")
		f.content(w, r, repo, rest)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (f *Fake) assets(w http.ResponseWriter, r *http.Request) {
	file := assetFiles[r.URL.Query().Get("repository")]
	if file == "" {
		writeJSON(w, []byte(`{"items":[],"continuationToken":null}`))
		return
	}
	if f.PageSize <= 0 {
		writeJSON(w, f.read(file))
		return
	}
	var all struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(f.read(file), &all); err != nil {
		panic(err)
	}
	start := 0
	if tok := r.URL.Query().Get("continuationToken"); tok != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(tok, "page-"))
	}
	end := min(start+f.PageSize, len(all.Items))
	page := map[string]any{"items": all.Items[start:end], "continuationToken": nil}
	if end < len(all.Items) {
		page["continuationToken"] = fmt.Sprintf("page-%d", end)
	}
	b, _ := json.Marshal(page)
	writeJSON(w, b)
}

// Override serves body for repo/path instead of the recorded artifact (the
// listing's checksums are unchanged, so the download fails verification).
func (f *Fake) Override(repo, path string, body []byte) {
	f.mu.Lock()
	f.override[repo+"/"+path] = body
	f.mu.Unlock()
}

func (f *Fake) content(w http.ResponseWriter, r *http.Request, repo, path string) {
	if repo == "npm-h" {
		if file := packuments[path]; file != "" {
			writeJSON(w, f.read(file))
			return
		}
	}
	if repo == "cargo-h" && (path == "index/de/mo/democrate" || path == "de/mo/democrate") {
		w.Write(f.read("real-cargo-index-democrate"))
		return
	}
	if f.BeforeDownload != nil {
		f.BeforeDownload(r, repo, path)
		if r.Context().Err() != nil {
			return
		}
	}
	f.mu.Lock()
	body, ok := f.override[repo+"/"+path]
	f.mu.Unlock()
	if !ok {
		var err error
		if repo == "mvn-h" {
			// The recorded Maven files are empty (their checksums are those
			// of zero bytes).
			if strings.HasPrefix(path, "com/ex/app/1.0/app-1.0.") {
				body, ok = []byte{}, true
			}
		} else if body, err = os.ReadFile(filepath.Join(f.dir, "files", repo, filepath.FromSlash(path))); err == nil {
			ok = true
		}
	}
	if !ok || strings.Contains(path, "..") {
		http.NotFound(w, r)
		return
	}
	f.Downloads.Add(1)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}
