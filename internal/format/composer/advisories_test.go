package composer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

const formType = "application/x-www-form-urlencoded"

// answerNames is an upstream's API as Packagist's behaves, with one advisory per
// name: names it knows, lists for them, sent in the form Composer sends.
func answerNames(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	out := map[string]any{}
	for k, vs := range r.PostForm {
		if strings.HasPrefix(k, "packages[") {
			for _, n := range vs {
				out[n] = []map[string]string{{"advisoryId": "PKSA-" + n, "affectedVersions": ">=1.0.0,<2.0.0"}}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"advisories": out})
}

// post sends names as Composer does: http_build_query(['packages' => $batch]).
func (e *env) post(repoName string, names ...string) *httptest.ResponseRecorder {
	var form []string
	for i, n := range names {
		form = append(form, fmt.Sprintf("packages%%5B%d%%5D=%s", i, url.QueryEscape(n)))
	}
	return e.postForm(repoName, strings.Join(form, "&"))
}

func (e *env) postForm(repoName, form string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/repository/"+repoName+"/"+advisoriesPath, strings.NewReader(form))
	r.Header.Set("Content-Type", formType)
	e.h.Serve(w, r, e.repos[repoName], advisoriesPath)
	return w
}

// advised is the names of a 200 answer.
func advised(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var got struct {
		Advisories map[string][]struct {
			AdvisoryID string `json:"advisoryId"`
		} `json:"advisories"`
	}
	decode(t, w, &got)
	names := []string{}
	for n, list := range got.Advisories {
		if len(list) != 1 || list[0].AdvisoryID != "PKSA-"+n {
			t.Errorf("advisories of %s: %+v", n, list)
		}
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// heard reports what upstream requests since mark named name, in a path or a
// body, however it was encoded.
func (e *env) heard(mark int, name string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, s := range e.seen[mark:] {
		if d, err := url.QueryUnescape(s); strings.Contains(s, name) || err == nil && strings.Contains(d, name) {
			out = append(out, s)
		}
	}
	return out
}

func (e *env) requests() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.seen)
}

func TestAdvisoriesProxy(t *testing.T) {
	e := newEnv(t)
	e.advise = answerNames

	// packages.json offers the proxy's own URL, and no Last-Modified: Composer
	// would keep a copy from before the upgrade for as long as the upstream's file
	// stays the same.
	w := e.do("GET", "composer-proxy", "packages.json", nil)
	var root struct {
		Advisories map[string]any `json:"security-advisories"`
	}
	decode(t, w, &root)
	want := map[string]any{"metadata": false, "api-url": ccBase + "/repository/composer-proxy/" + advisoriesPath}
	if fmt.Sprint(root.Advisories) != fmt.Sprint(want) {
		t.Fatalf("security-advisories: %v", root.Advisories)
	}
	if lm := w.Header().Get("Last-Modified"); lm != "" {
		t.Fatalf("packages.json has Last-Modified %s", lm)
	}

	// The form, the answer. The name is lower-cased, and what cannot be a package
	// name goes nowhere.
	mark := e.requests()
	w = e.post("composer-proxy", "monolog/monolog", "PSR/Log", "php", "ext-json", "__root__")
	if got := advised(t, w); !slices.Equal(got, []string{"monolog/monolog", "psr/log"}) {
		t.Fatalf("names: %v", got)
	}
	e.mu.Lock()
	sent := slices.Clone(e.seen[mark:])
	e.mu.Unlock()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "POST /api/security-advisories/ ") {
		t.Fatalf("upstream requests: %q", sent)
	}
	form := url.Values{"packages[]": {"monolog/monolog", "psr/log"}}.Encode()
	if body := strings.TrimPrefix(sent[0], "POST /api/security-advisories/ "); body != form {
		t.Fatalf("upstream body: %s", body)
	}
	if h := w.Header().Get("Content-Type"); h != ctJSON {
		t.Fatalf("Content-Type %s", h)
	}

	// Nothing is kept: a new advisory shows at once.
	mark = e.requests()
	e.advise = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"advisories":{"monolog/monolog":[{"advisoryId":"PKSA-new"}]}}`)
	}
	w = e.post("composer-proxy", "monolog/monolog")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "PKSA-new") || len(e.heard(mark, "monolog/monolog")) != 1 {
		t.Fatalf("second answer: %d %s", w.Code, w.Body)
	}

	// An upstream with nothing to say about anything: Packagist's PHP writes [].
	e.advise = func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"advisories":[]}`) }
	if got := advised(t, e.post("composer-proxy", "monolog/monolog")); len(got) != 0 {
		t.Fatalf("empty answer: %v", got)
	}

	// Whatever else the upstream does is a failure the client sees, never "no
	// advisories": Composer stops at the status (exit 100 with 2.11).
	for _, c := range []struct {
		name   string
		answer func(http.ResponseWriter, *http.Request)
		want   int
	}{
		{"500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, 502},
		{"503", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, 502},
		{"404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }, 502},
		{"403", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }, 502},
		{"429", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }, 429},
		{"a web page", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>maintenance</html>") }, 502},
		{"JSON without the key", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"status":"fine"}`) }, 502},
		{"cut short", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"advisories":{"a/b":[`) }, 502},
		{"an array", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[]`) }, 502},
		{"a string for the list", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"advisories":"none"}`) }, 502},
	} {
		e.advise = c.answer
		if w := e.post("composer-proxy", "monolog/monolog"); w.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body, c.want)
		}
	}

	// What the route takes.
	e.advise = answerNames
	if w := e.do("GET", "composer-proxy", advisoriesPath, nil); w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Fatalf("GET: %d %v", w.Code, w.Header())
	}
	if w := e.postForm("composer-proxy", ""); w.Code != 400 {
		t.Fatalf("no names: %d", w.Code)
	}
	if w := e.postForm("composer-proxy", "packages[]=a/b&"+strings.Repeat("x", maxAdvisoryForm)); w.Code != 413 {
		t.Fatalf("too large: %d", w.Code)
	}
	if got := advised(t, e.postForm("composer-proxy", "packages[]=monolog/monolog")); !slices.Equal(got, []string{"monolog/monolog"}) {
		t.Fatalf("packages[]: %v", got)
	}
	r := httptest.NewRequest("POST", "/repository/composer-proxy/x", strings.NewReader("packages[]=monolog/monolog"))
	r.Header.Set("Content-Type", formType)
	w = httptest.NewRecorder()
	e.h.Serve(w, r, e.repos["composer-proxy"], strings.TrimSuffix(advisoriesPath, "/"))
	if got := advised(t, w); !slices.Equal(got, []string{"monolog/monolog"}) {
		t.Fatalf("without the slash: %v", got)
	}
}

// TestAdvisoriesBlocked: a proxy that may not ask its remote fails the request
// and sends nothing. The remote's own host counts for the auto-block; a remote
// that keeps failing the API is then not asked.
func TestAdvisoriesBlocked(t *testing.T) {
	e := newEnv(t)
	e.advise = answerNames
	e.repos["composer-proxy"].Blocked = true
	mark := e.requests()
	if w := e.post("composer-proxy", "monolog/monolog"); w.Code != 502 {
		t.Fatalf("blocked: %d", w.Code)
	}
	if got := e.heard(mark, "monolog/monolog"); len(got) != 0 {
		t.Fatalf("a blocked remote heard: %q", got)
	}
	e.repos["composer-proxy"].Blocked = false
	e.advise = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	for range 3 {
		e.post("composer-proxy", "monolog/monolog")
	}
	mark = e.requests()
	if w := e.post("composer-proxy", "monolog/monolog"); w.Code != 502 {
		t.Fatalf("auto-blocked: %d", w.Code)
	}
	if e.requests() != mark {
		t.Fatal("an auto-blocked remote was asked")
	}
}

// TestAdvisoriesPrivateNames pins the private-name rule (advisoryOwner): a name
// a hosted repository holds never reaches an upstream, in a POST or a GET,
// whichever way the question comes, unless a read already found it there.
func TestAdvisoriesPrivateNames(t *testing.T) {
	e := newEnv(t)
	e.advise = answerNames
	z := archive(t, "", composerJSON("smoke/secret", ""))
	if w := e.upload(t, "composer-hosted", "smoke/secret", "1.0.0", z); w.Code != 201 {
		t.Fatal(w.Code)
	}
	z = archive(t, "", composerJSON("other/secret", ""))
	if w := e.upload(t, "composer-allow", "other/secret", "1.0.0", z); w.Code != 201 { // a member of no group here
		t.Fatal(w.Code)
	}
	z = archive(t, "", composerJSON("acme/demo", ""))
	if w := e.upload(t, "composer-hosted", "acme/demo", "1.0.0", z); w.Code != 201 { // the upstream also has this name
		t.Fatal(w.Code)
	}
	private := []string{"smoke/secret", "other/secret"}
	for _, c := range []struct {
		repo string
		want []string
	}{
		{"composer-all", []string{"monolog/monolog"}},         // the group: hosted first
		{"composer-proxy-first", []string{"monolog/monolog"}}, // the upstream first
		{"composer-proxy", []string{"monolog/monolog"}},       // the client lists hosted and proxy itself
	} {
		mark := e.requests()
		got := advised(t, e.post(c.repo, "smoke/secret", "other/secret", "monolog/monolog"))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: answered %v", c.repo, got)
		}
		for _, n := range private {
			if heard := e.heard(mark, n); len(heard) != 0 {
				t.Errorf("%s: the upstream heard %s: %q", c.repo, n, heard)
			}
		}
	}

	// A name both have: the first member owns it. The group with hosted first
	// never asks. Through the others nothing is sent until a read finds it upstream.
	mark := e.requests()
	if got := advised(t, e.post("composer-all", "acme/demo")); len(got) != 0 {
		t.Errorf("hosted first: %v", got)
	}
	if got := advised(t, e.post("composer-proxy-first", "acme/demo")); len(got) != 0 {
		t.Errorf("proxy first, nothing read yet: %v", got)
	}
	if got := advised(t, e.post("composer-proxy", "acme/demo")); len(got) != 0 {
		t.Errorf("proxy alone, nothing read yet: %v", got)
	}
	if heard := e.heard(mark, "acme/demo"); len(heard) != 0 {
		t.Errorf("the upstream heard acme/demo: %q", heard)
	}
	if w := e.do("GET", "composer-proxy-first", "p2/acme/demo.json", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, r := range []string{"composer-proxy-first", "composer-proxy"} {
		if got := advised(t, e.post(r, "acme/demo")); !slices.Equal(got, []string{"acme/demo"}) {
			t.Errorf("%s, after the read: %v", r, got)
		}
	}
	mark = e.requests()
	if got := advised(t, e.post("composer-all", "acme/demo")); len(got) != 0 {
		t.Errorf("hosted first, after the read: %v", got)
	}
	if heard := e.heard(mark, "acme/demo"); len(heard) != 0 {
		t.Errorf("the upstream heard acme/demo through hosted first: %q", heard)
	}

	// An offline member's name is still private.
	e.repos["composer-hosted"].Online = new(false)
	mark = e.requests()
	if got := advised(t, e.post("composer-all", "smoke/secret", "monolog/monolog")); !slices.Equal(got, []string{"monolog/monolog"}) {
		t.Errorf("offline member: %v", got)
	}
	if heard := e.heard(mark, "smoke/secret"); len(heard) != 0 {
		t.Errorf("the upstream heard an offline member's name: %q", heard)
	}
	e.repos["composer-hosted"].Online = nil

	// A proxy member offline cannot answer: not "no advisories", unless every name
	// is one nobody would ask it about.
	e.repos["composer-proxy"].Online = new(false)
	if w := e.post("composer-all", "monolog/monolog"); w.Code != 503 {
		t.Errorf("proxy member offline: %d %s", w.Code, w.Body)
	}
	if got := advised(t, e.post("composer-all", "smoke/secret")); len(got) != 0 {
		t.Errorf("proxy member offline, a private name: %v", got)
	}
	var offered map[string]any
	decode(t, e.do("GET", "composer-all", "packages.json", nil), &offered)
	if offered["security-advisories"] == nil {
		t.Error("the offer went with the offline member, and with it the failure")
	}
	e.repos["composer-proxy"].Online = nil

	// Nothing to ask: no question at all.
	mark = e.requests()
	if got := advised(t, e.post("composer-all", "smoke/secret")); len(got) != 0 {
		t.Errorf("only private names: %v", got)
	}
	if e.requests() != mark {
		t.Error("asked the upstream with nothing to ask")
	}
}

// TestAdvisoriesRoutingRule: a rule that refuses a name's metadata keeps the name
// from that proxy. One that refuses the API takes the offer away, as a direct
// POST would be refused by the server.
func TestAdvisoriesRoutingRule(t *testing.T) {
	e := newEnv(t)
	e.advise = answerNames
	refused := []string{"/p2/acme/"}
	e.h.Fetcher.RouteAllowed = func(rp *repo.Repository, path string) bool {
		return !slices.ContainsFunc(refused, func(p string) bool { return strings.HasPrefix(path, p) })
	}
	for _, r := range []string{"composer-proxy", "composer-all"} {
		mark := e.requests()
		if got := advised(t, e.post(r, "acme/internal", "monolog/monolog")); !slices.Equal(got, []string{"monolog/monolog"}) {
			t.Errorf("%s: %v", r, got)
		}
		if heard := e.heard(mark, "acme/internal"); len(heard) != 0 {
			t.Errorf("%s: the upstream heard a refused name: %q", r, heard)
		}
	}
	refused = append(refused, "/"+advisoriesPath)
	var root map[string]any
	decode(t, e.do("GET", "composer-proxy", "packages.json", nil), &root)
	if root["security-advisories"] != nil {
		t.Errorf("offered although the rule refuses it: %v", root)
	}
	mark := e.requests()
	if got := advised(t, e.post("composer-all", "monolog/monolog")); len(got) != 0 {
		t.Errorf("group: %v", got)
	}
	if e.requests() != mark {
		t.Error("a member whose rule refuses the API was asked")
	}
}

// TestAdvisoriesTwoProxies: the first proxy that has a package answers for it,
// and the other never hears of it; the answers are merged, and one that fails
// fails the whole.
func TestAdvisoriesTwoProxies(t *testing.T) {
	e := newEnv(t)
	e.advise = answerNames
	var second []string
	secondAnswer := answerNames
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		e.mu.Lock()
		second = append(second, r.Method+" "+r.URL.Path+" "+string(body))
		e.mu.Unlock()
		switch {
		case r.URL.Path == "/packages.json":
			fmt.Fprintf(w, `{"packages":[],"metadata-url":"/p2/%%package%%.json","security-advisories":{"api-url":"/api/security-advisories/"}}`)
		case r.URL.Path == "/api/security-advisories/":
			secondAnswer(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	e.repos = repo.Build(&config.Config{Repositories: []config.Repository{
		{Name: "composer-hosted", Format: "composer", Type: "hosted"},
		{Name: "composer-proxy", Format: "composer", Type: "proxy", Remote: e.up + "/", MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "composer-other", Format: "composer", Type: "proxy", Remote: ts.URL + "/", MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "both", Format: "composer", Type: "group", Members: []string{"composer-hosted", "composer-proxy", "composer-other"}},
		{Name: "nested", Format: "composer", Type: "group", Members: []string{"composer-hosted", "both"}},
	}})
	for _, group := range []string{"both", "nested"} {
		second = nil
		mark := e.requests()
		got := advised(t, e.post(group, "monolog/monolog", "elsewhere/pkg"))
		if !slices.Equal(got, []string{"elsewhere/pkg", "monolog/monolog"}) {
			t.Fatalf("%s: %v", group, got)
		}
		// the first proxy was asked whether it has elsewhere/pkg (a GET, as a read
		// does) and not given it in the POST; the second knows nothing of monolog/monolog
		for _, h := range e.heard(mark, "elsewhere/pkg") {
			if !strings.HasPrefix(h, "GET /p2/elsewhere/pkg") {
				t.Errorf("%s: the first proxy heard %q", group, h)
			}
		}
		if s := fmt.Sprint(second); strings.Contains(s, "monolog") || !strings.Contains(s, "packages%5B%5D=elsewhere%2Fpkg") {
			t.Errorf("%s: the second proxy heard %q", group, second)
		}
	}
	secondAnswer = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	if w := e.post("both", "monolog/monolog", "elsewhere/pkg"); w.Code != 502 {
		t.Fatalf("second fails: %d %s", w.Code, w.Body)
	}
}

// TestAdvisoriesNoData: a hosted repository and a group without a proxy have no
// data, do not offer the API and answer an empty list.
func TestAdvisoriesNoData(t *testing.T) {
	e := newEnv(t)
	e.repos = repo.Build(&config.Config{Repositories: []config.Repository{
		{Name: "composer-hosted", Format: "composer", Type: "hosted"},
		{Name: "only-hosted", Format: "composer", Type: "group", Members: []string{"composer-hosted"}},
		{Name: "with-proxy", Format: "composer", Type: "group", Members: []string{"composer-hosted", "composer-proxy"}},
		{Name: "composer-proxy", Format: "composer", Type: "proxy", Remote: e.up + "/nothing/", MetadataMaxAge: config.Duration(time.Hour), NegativeCacheTTL: config.Duration(time.Minute)},
	}})
	e.docs = map[string]string{"/nothing/packages.json": `{"packages":[],"metadata-url":"/p2/%package%.json"}`}
	for _, r := range []string{"composer-hosted", "only-hosted", "composer-proxy"} {
		var root map[string]any
		decode(t, e.do("GET", r, "packages.json", nil), &root)
		if root["security-advisories"] != nil {
			t.Errorf("%s offers: %v", r, root["security-advisories"])
		}
		w := e.post(r, "monolog/monolog")
		if got := advised(t, w); len(got) != 0 || w.Body.String() != `{"advisories":{}}` {
			t.Errorf("%s answers: %s", r, w.Body)
		}
	}
	// A group offers it without asking its proxy (a dead upstream must not take its
	// packages.json down); an upstream without the API then answers nothing.
	mark := e.requests()
	var root struct {
		Advisories map[string]any `json:"security-advisories"`
	}
	decode(t, e.do("GET", "with-proxy", "packages.json", nil), &root)
	if root.Advisories["api-url"] != ccBase+"/repository/with-proxy/"+advisoriesPath {
		t.Errorf("group: %v", root.Advisories)
	}
	if e.requests() != mark {
		t.Error("the group's packages.json asked its proxy's upstream")
	}
	if got := advised(t, e.post("with-proxy", "monolog/monolog")); len(got) != 0 {
		t.Errorf("no API: %v", got)
	}
}

// TestHostedPackages is the query behind the rule, on rows of other formats and
// repositories of other types too.
func TestHostedPackages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	z := archive(t, "", composerJSON("acme/one", ""))
	e.upload(t, "composer-hosted", "acme/one", "1.0.0", z)
	e.upload(t, "composer-allow", "acme/one", "1.0.0", z)
	z = archive(t, "", composerJSON("acme/two", ""))
	e.upload(t, "composer-allow", "acme/two", "1.0.0", z)
	got, err := e.h.Store.DB.HostedPackages(ctx, "composer", []string{"acme/one", "acme/two", "acme/none"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got["acme/one"])
	if len(got) != 2 || !slices.Equal(got["acme/one"], []string{"composer-allow", "composer-hosted"}) || !slices.Equal(got["acme/two"], []string{"composer-allow"}) {
		t.Fatalf("%v", got)
	}
	if got, err := e.h.Store.DB.HostedPackages(ctx, "npm", []string{"acme/one"}); err != nil || len(got) != 0 {
		t.Fatalf("another format: %v %v", got, err)
	}
	many := make([]string, 0, 601)
	for i := range 600 {
		many = append(many, fmt.Sprintf("x/y%d", i))
	}
	got, err = e.h.Store.DB.HostedPackages(ctx, "composer", append(many, "acme/two"))
	if err != nil || len(got) != 1 {
		t.Fatalf("more than a batch: %v %v", got, err)
	}
}

// TestAdvisoryURLOfUpstream: the upstream's offer read when relative, and when
// it has no API.
func TestAdvisoryURLOfUpstream(t *testing.T) {
	e := newEnv(t)
	e.docs = map[string]string{"/rel/packages.json": `{"packages":[],"metadata-url":"/p2/%package%.json","security-advisories":{"metadata":true,"api-url":"/rel/api/"}}`,
		"/meta/packages.json": `{"packages":[],"metadata-url":"/p2/%package%.json","security-advisories":{"metadata":true}}`}
	e.repos = repo.Build(&config.Config{Repositories: []config.Repository{
		{Name: "rel", Format: "composer", Type: "proxy", Remote: e.up + "/rel/", MetadataMaxAge: config.Duration(time.Hour)},
		{Name: "meta", Format: "composer", Type: "proxy", Remote: e.up + "/meta/", MetadataMaxAge: config.Duration(time.Hour)},
	}})
	up, _, err := e.h.upstreamFor(context.Background(), e.repos["rel"])
	if err != nil || up.advisoryURL != e.up+"/rel/api/" {
		t.Fatalf("%q %v", up.advisoryURL, err)
	}
	// Advisories only inline need the upstream's own package list, which a proxy
	// passes on: it is not offered, as there is nothing to ask.
	var root map[string]any
	decode(t, e.do("GET", "meta", "packages.json", nil), &root)
	if root["security-advisories"] != nil {
		t.Fatalf("offered without an API: %v", root)
	}
}
