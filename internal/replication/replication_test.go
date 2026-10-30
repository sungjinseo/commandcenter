package replication

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// proxyOf is a replicating proxy whose remote is remote, signing in as u:pw.
func proxyOf(t *testing.T, remote string) *repo.Repository {
	u, err := url.Parse(remote)
	if err != nil {
		t.Fatal(err)
	}
	return &repo.Repository{RemoteURL: u, Repository: config.Repository{Name: "p", Format: "maven", Type: "proxy", Remote: remote,
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "pw"}, Replication: &config.Replication{PreemptivePullEnabled: true}}}
}

// A source of three pages under a context path: the list follows the
// tokens with the credentials and reads both time spellings; a source that
// repeats a token, refuses the credentials, lacks the repository or does
// not speak the API fails the listing with the reason.
func TestList(t *testing.T) {
	pages := map[string]string{
		"":   `{"items":[{"path":"a/1.jar","checksum":{"sha1":"aa"},"lastModified":"2026-10-30T01:02:03.456+00:00","downloadUrl":"http://x/a"}],"continuationToken":"t1"}`,
		"t1": `{"items":[{"path":"b/2.jar","checksum":{"sha256":"bb"},"lastModified":"2026-10-30T01:02:04.5+0000"},{"path":"c","lastModified":null}],"continuationToken":"t2"}`,
		"t2": `{"items":[],"continuationToken":null}`,
	}
	var status int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, pw, _ := r.BasicAuth(); u != "u" || pw != "pw" || r.URL.Path != "/nx/service/rest/v1/assets" || r.URL.Query().Get("repository") != "src" {
			http.Error(w, "unexpected "+r.URL.String(), 500)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		body, ok := pages[r.URL.Query().Get("continuationToken")]
		if !ok {
			http.Error(w, "bad token", 400)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer ts.Close()
	rp := proxyOf(t, ts.URL+"/nx/repository/src/")
	base, src, ok := config.ReplicationSource(rp.Remote)
	if !ok || base != ts.URL+"/nx" || src != "src" {
		t.Fatalf("source: %q %q %v", base, src, ok)
	}
	s := &Service{Fetcher: &proxy.Fetcher{Client: ts.Client()}}
	var got []Asset
	if err := s.list(context.Background(), rp, base, src, func(a Asset) { got = append(got, a) }); err != nil {
		t.Fatal(err)
	}
	want := []Asset{
		{Path: "a/1.jar", SHA1: "aa", DownloadURL: "http://x/a", LastModified: time.Date(2026, 10, 30, 1, 2, 3, 456e6, time.UTC)},
		{Path: "b/2.jar", SHA256: "bb", LastModified: time.Date(2026, 10, 30, 1, 2, 4, 5e8, time.UTC)},
		{Path: "c"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if g := got[i]; g.Path != want[i].Path || g.SHA1 != want[i].SHA1 || g.SHA256 != want[i].SHA256 || g.DownloadURL != want[i].DownloadURL || !g.LastModified.Equal(want[i].LastModified) {
			t.Errorf("asset %d: %+v, want %+v", i, g, want[i])
		}
	}

	pages["t2"] = `{"items":[],"continuationToken":"t1"}`
	for st, msg := range map[int]string{0: "repeated a continuation token", 401: "refused the remote credentials (HTTP 401)",
		404: `no repository "src"`, 503: "answered HTTP 503"} {
		status = st
		if err := s.list(context.Background(), rp, base, src, func(Asset) {}); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("status %d: %v", st, err)
		}
	}
	status = 0
	pages[""] = `<html>`
	if err := s.list(context.Background(), rp, base, src, func(Asset) {}); err == nil || !strings.Contains(err.Error(), "not the Nexus REST API's") {
		t.Errorf("html: %v", err)
	}
}

func TestReplicationSource(t *testing.T) {
	for remote, want := range map[string][2]string{
		"https://nx.example/repository/maven-hosted/":   {"https://nx.example", "maven-hosted"},
		"https://u:p@nx.example/nexus/repository/r?x=1": {"https://u:p@nx.example/nexus", "r"},
		"https://nx.example/repository/":                {},
		"https://nx.example/repository/a/b/":            {},
		"https://repo1.maven.org/maven2/":               {},
		"/repository/r/":                                {},
	} {
		base, name, ok := config.ReplicationSource(remote)
		if ok != (want[1] != "") || base != want[0] || name != want[1] {
			t.Errorf("%s: %q %q %v", remote, base, name, ok)
		}
	}
}

// What is replicated and where a proxy is asked for it.
func TestWantedAndRequest(t *testing.T) {
	for _, c := range []struct {
		format, path, regex string
		want                bool
	}{
		{"maven", "org/ex/lib/1.0/lib-1.0.jar", "", true},
		{"maven", "org/ex/lib/maven-metadata.xml", "", false}, // an index document: the proxy's own
		{"maven", "org/ex/lib/1.0/lib-1.0.jar", `^/org/`, true},
		{"maven", "org/ex/lib/1.0/lib-1.0.jar", `^org/ex/`, true},
		{"maven", "org/ex/lib/1.0/lib-1.0.pom", `\.jar$`, false},
		{"helm", "index.yaml", "", false},
	} {
		var re *regexp.Regexp
		if c.regex != "" {
			re = regexp.MustCompile(c.regex)
		}
		if got := wanted(c.format, c.path, re); got != c.want {
			t.Errorf("wanted(%s, %s, %q) = %v", c.format, c.path, c.regex, got)
		}
	}
	for _, p := range []string{"", "/a", "a/../b", "a//b", "a/./b", "a/", "..", "a\x00b"} {
		if safePath(p) {
			t.Errorf("safePath(%q)", p)
		}
	}
	for _, c := range []struct{ format, path, dl, req, key string }{
		{"maven", "a/b.jar", "", "a/b.jar", "a/b.jar"},
		{"nuget", "demo/1.0.0/demo.1.0.0.nupkg", "", "v3-flatcontainer/demo/1.0.0/demo.1.0.0.nupkg", "v3-flatcontainer/demo/1.0.0/demo.1.0.0.nupkg"},
		{"swift", "mona/ll/1.0.0/source-archive.zip", "", "mona/ll/1.0.0.zip", "mona/ll/1.0.0/source-archive.zip"},
		{"composer", "dist/acme/demo/1.0.0.0.zip", "http://s/repository/c/dist/acme/demo/1.0.0.0.zip", "", ""},
	} {
		req, key := request(c.format, Asset{Path: c.path, DownloadURL: c.dl})
		if c.format == "composer" { // the sha1 of the dist URL
			if !strings.HasPrefix(req, "dist/acme/demo/1.0.0.0/") || len(req) != len("dist/acme/demo/1.0.0.0/")+40+4 || key != req {
				t.Errorf("composer: %q %q", req, key)
			}
			continue
		}
		if req != c.req || key != c.key {
			t.Errorf("request(%s, %s) = %q, %q", c.format, c.path, req, key)
		}
	}
}
