package repotest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// Answer is what a Remote answers for a path.
type Answer struct {
	Status      int // 0: 200
	ContentType string
	Body        string
}

// Remote is a remote repository whose answers a test sets while it runs; a
// path without an answer is a 404.
type Remote struct {
	URL     string
	mu      sync.Mutex
	answers map[string]Answer
}

// NewRemote starts a Remote; URL ends in no slash.
func NewRemote(t testing.TB) *Remote {
	t.Helper()
	r := &Remote{answers: map[string]Answer{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		a, ok := r.answers[q.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, q)
			return
		}
		if a.ContentType != "" {
			w.Header().Set("Content-Type", a.ContentType)
		}
		if a.Status != 0 {
			w.WriteHeader(a.Status)
		}
		io.WriteString(w, a.Body)
	}))
	t.Cleanup(ts.Close)
	r.URL = ts.URL
	return r
}

// Set makes the remote answer a for path.
func (r *Remote) Set(path string, a Answer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[path] = a
}

// With is repos with the repositories defs added: ProxyOn and GroupOf.
func With(repos map[string]*repo.Repository, defs ...config.Repository) map[string]*repo.Repository {
	cfg := &config.Config{Repositories: defs}
	for _, rp := range repos {
		cfg.Repositories = append(cfg.Repositories, rp.Repository)
	}
	return repo.Build(cfg)
}

// ProxyOn is a proxy of format on remote, that keeps its documents for an hour
// and a miss for a minute.
func ProxyOn(name, format, remote string) config.Repository {
	return config.Repository{Name: name, Format: format, Type: "proxy", Remote: remote,
		MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)}
}

// GroupOf is a group of format.
func GroupOf(name, format string, members ...string) config.Repository {
	return config.Repository{Name: name, Format: format, Type: "group", Members: members}
}

// Refusals are what an internal registry answers instead of a document it
// will not give: refusals, a sign-in page under any type, a JSON error.
var Refusals = []Answer{
	{Status: 400, ContentType: "application/json", Body: `{"error":"bad"}`},
	{Status: 403, ContentType: "text/html", Body: "<html>Forbidden</html>"},
	{Status: 451, ContentType: "text/plain", Body: "not here"},
	{ContentType: "text/html; charset=utf-8", Body: `<!DOCTYPE html><html><a href="/login">Sign in</a></html>`},
	{ContentType: "application/json", Body: `<!DOCTYPE html><html><a href="/login">Sign in</a></html>`},
	{ContentType: "text/plain", Body: "Sign in"},
	WrongJSON,
}

// WrongJSON is an answer in JSON that a check by its keys refuses.
var WrongJSON = Answer{ContentType: "application/json", Body: `{"error":"maintenance"}`}

// OwnerDocument is a document of a member of a group that decides who owns a
// package, and the requests that show it: FailsClosed.
type OwnerDocument struct {
	Remote *Remote
	Path   string // the document on Remote
	Good   Answer // the document, naming what only this member has
	// Get asks the group for the package. A later member has one of that name.
	Get func() *httptest.ResponseRecorder
	// Ours and Theirs tell whose answer a request got.
	Ours, Theirs func(*httptest.ResponseRecorder) bool
	// Cached is the blob this member holds for the document, "" for none.
	Cached func() string
	// Expire moves the clock past metadataMaxAge and the negative cache.
	Expire func()
}

// FailsClosed pins the owner rule on d: only the 404 and 410 of a member's
// remote hand the name to the next member. What it refuses or answers with what
// is no document fails the group's request and leaves nothing behind, so the
// next correct answer counts at once; and a copy cached before answers through
// all of it, unchanged.
func FailsClosed(t *testing.T, d OwnerDocument) {
	t.Helper()
	for _, status := range []int{404, 410} {
		d.Remote.Set(d.Path, Answer{Status: status, ContentType: "text/plain", Body: "no such package"})
		d.Expire()
		if w := d.Get(); w.Code != 200 || !d.Theirs(w) {
			t.Errorf("%d: %d %.200s, want the next member's", status, w.Code, w.Body)
		}
	}
	d.Expire()
	for _, a := range Refusals {
		d.Remote.Set(d.Path, a)
		w := d.Get()
		if w.Code < 500 || d.Theirs(w) {
			t.Errorf("%d %s %.30q, not cached: %d %.200s, want a 5xx", a.Status, a.ContentType, a.Body, w.Code, w.Body)
		}
		if d.Cached() != "" {
			t.Errorf("%d %s %.30q was cached", a.Status, a.ContentType, a.Body)
		}
	}
	d.Remote.Set(d.Path, d.Good)
	w := d.Get()
	if w.Code != 200 || !d.Ours(w) {
		t.Fatalf("the document after the refusals: %d %.200s, want this member's", w.Code, w.Body)
	}
	cached := d.Cached()
	if cached == "" {
		t.Fatal("the document was not cached")
	}
	for _, a := range Refusals {
		d.Remote.Set(d.Path, a)
		d.Expire()
		if w := d.Get(); w.Code != 200 || !d.Ours(w) {
			t.Errorf("%d %s %.30q, cached: %d %.200s, want the copy", a.Status, a.ContentType, a.Body, w.Code, w.Body)
		}
		if d.Cached() != cached {
			t.Errorf("%d %s %.30q replaced the copy", a.Status, a.ContentType, a.Body)
		}
	}
}

// FailsOffline pins that member, offline, does not hand what get asks a group
// for to a later member: a 503, and get's answer again once it is back.
func FailsOffline(t *testing.T, member *repo.Repository, get func() *httptest.ResponseRecorder) {
	t.Helper()
	member.Online = new(false)
	w := get()
	member.Online = nil
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("%s offline: %d %.200s, want 503", member.Name, w.Code, w.Body)
	}
	if w := get(); w.Code != http.StatusOK {
		t.Errorf("%s back: %d %.200s", member.Name, w.Code, w.Body)
	}
}

// Garbage is what no JSON document of a remote is.
var Garbage = []Answer{
	{ContentType: "text/plain", Body: "Sign in"},
	{ContentType: "application/json", Body: "<!DOCTYPE html><html>Sign in</html>"},
	{ContentType: "application/json", Body: `{"a":`},
}

// KeepsCopy pins a document that decides nothing: what is no such document
// does not replace the copy cached before, as Get then shows, where it would
// stand until metadataMaxAge ends, nor does any of also. get must succeed with
// the good document.
func KeepsCopy(t *testing.T, r *Remote, path string, good Answer, get func() *httptest.ResponseRecorder, cached func() string, expire func(), also ...Answer) {
	t.Helper()
	r.Set(path, good)
	if w := get(); w.Code != 200 {
		t.Fatalf("the document: %d %.200s", w.Code, w.Body)
	}
	before := cached()
	if before == "" {
		t.Fatal("the document was not cached")
	}
	for _, a := range slices.Concat(Garbage, also) {
		r.Set(path, a)
		expire()
		if w := get(); w.Code != 200 {
			t.Errorf("%s %.30q, cached: %d %.200s, want the copy", a.ContentType, a.Body, w.Code, w.Body)
		}
		if cached() != before {
			t.Errorf("%s %.30q replaced the copy", a.ContentType, a.Body)
		}
	}
}
