package nuget

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// The flat container index decides who owns a package id in a group: what
// the remote of a member refuses, or a page, or JSON that is no such index,
// fails closed, and only its 404 and 410 hand the id to the next member. The
// other documents of a proxy keep their copy when a remote answers garbage.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	rm.Set("/index.json", repotest.Answer{ContentType: ctJSON, Body: `{"version":"3.0.0","resources":[{"@id":"` + rm.URL + `/flat/","@type":"PackageBaseAddress/3.0.0"},{"@id":"` + rm.URL + `/reg/","@type":"RegistrationsBaseUrl/3.6.0"},{"@id":"` + rm.URL + `/vuln/index.json","@type":"VulnerabilityInfo/6.7.0"}]}`})
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "nuget", rm.URL+"/index.json"), repotest.GroupOf("g", "nuget", "internal", "nuget-proxy"))
	has := func(s string) func(*httptest.ResponseRecorder) bool {
		return func(w *httptest.ResponseRecorder) bool { return strings.Contains(w.Body.String(), s) }
	}
	cached := func(key string) func() string {
		return func() string {
			a, _ := e.h.Store.Get(context.Background(), "internal", key)
			return a.BlobSHA256
		}
	}
	expire := func() { e.now = e.now.Add(2 * time.Hour) }
	repotest.FailsClosed(t, repotest.OwnerDocument{
		Remote: rm,
		Path:   "/flat/newtonsoft.json/index.json",
		Good:   repotest.Answer{ContentType: ctJSON, Body: `{"versions":["99.0.0"]}`},
		Get: func() *httptest.ResponseRecorder {
			return e.do("GET", "g", "v3-flatcontainer/newtonsoft.json/index.json", nil)
		},
		Ours:   has("99.0.0"),
		Theirs: has("13.0.1"),
		Cached: cached(flatPrefix + "newtonsoft.json/index.json"),
		Expire: expire,
	})
	repotest.FailsOffline(t, e.repos["internal"], func() *httptest.ResponseRecorder {
		return e.do("GET", "g", "v3-flatcontainer/newtonsoft.json/index.json", nil)
	})
	repotest.KeepsCopy(t, rm, "/reg/demo/index.json", repotest.Answer{ContentType: ctJSON, Body: `{"count":0,"items":[]}`},
		func() *httptest.ResponseRecorder { return e.do("GET", "internal", "registration/demo/index.json", nil) },
		cached(regPrefix+"demo/index.json"), expire)
	repotest.KeepsCopy(t, rm, "/vuln/index.json", repotest.Answer{ContentType: ctJSON, Body: `[]`},
		func() *httptest.ResponseRecorder { return e.do("GET", "internal", vulnIndex, nil) },
		cached(vulnIndex), expire)
	good := repotest.Answer{ContentType: ctJSON, Body: `{"version":"3.0.0","resources":[{"@id":"` + rm.URL + `/reg/","@type":"RegistrationsBaseUrl/3.6.0"}]}`}
	repotest.KeepsCopy(t, rm, "/index.json", good,
		func() *httptest.ResponseRecorder { return e.do("GET", "internal", "registration/demo/index.json", nil) },
		cached(upstreamKey), expire, repotest.WrongJSON)
}
