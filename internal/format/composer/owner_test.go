package composer

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// The stable metadata file decides who owns a package in a group: what the
// remote of a member refuses, or a page, or JSON that is no such file, fails
// closed, and only its 404 and 410 hand the name to the next member.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	rm.Set("/packages.json", repotest.Answer{ContentType: ctJSON, Body: `{"metadata-url":"/p2/%package%.json"}`})
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "composer", rm.URL+"/"), repotest.GroupOf("g", "composer", "internal", "composer-proxy"))
	has := func(s string) func(*httptest.ResponseRecorder) bool {
		return func(w *httptest.ResponseRecorder) bool { return strings.Contains(w.Body.String(), s) }
	}
	key := metadataPath("monolog/monolog", false)
	repotest.FailsClosed(t, repotest.OwnerDocument{
		Remote: rm,
		Path:   "/" + key,
		Good:   repotest.Answer{ContentType: ctJSON, Body: `{"packages":{"monolog/monolog":[{"name":"monolog/monolog","version":"99.0.0","version_normalized":"99.0.0.0"}]}}`},
		Get:    func() *httptest.ResponseRecorder { return e.do("GET", "g", key, nil) },
		Ours:   has("99.0.0"),
		Theirs: has("3.5.0"),
		Cached: func() string {
			a, _ := e.h.Store.Get(context.Background(), "internal", key)
			return a.BlobSHA256
		},
		Expire: func() { e.now = e.now.Add(2 * time.Hour) },
	})
	repotest.FailsOffline(t, e.repos["internal"], func() *httptest.ResponseRecorder { return e.do("GET", "g", key, nil) })
	// A package may have no ~dev file, and a static site says so with a 403 too.
	rm.Set("/p2/monolog/monolog~dev.json", repotest.Answer{Status: 403})
	if w := e.do("GET", "internal", "p2/monolog/monolog~dev.json", nil); w.Code != 404 {
		t.Fatalf("~dev refused: %d %s", w.Code, w.Body)
	}
}

// A root document that is no JSON, or no root, does not replace the good one.
func TestRootKeepsCopy(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "composer", rm.URL+"/"))
	repotest.KeepsCopy(t, rm, "/packages.json", repotest.Answer{ContentType: ctJSON, Body: `{"metadata-url":"/p2/%package%.json"}`},
		func() *httptest.ResponseRecorder { return e.do("GET", "internal", "packages.json", nil) },
		func() string {
			a, _ := e.h.Store.Get(context.Background(), "internal", upstreamKey)
			return a.BlobSHA256
		},
		func() { e.now = e.now.Add(2 * time.Hour) }, repotest.WrongJSON)
}
