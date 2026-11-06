package terraform

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// The version list decides who owns a module or provider in a group: what the
// remote of a member refuses, or a page, or JSON that is no such list, fails
// closed, and only its 404 and 410 hand the name to the next member. A
// provider's download document keeps its copy when a remote answers garbage.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	rm.Set("/.well-known/terraform.json", repotest.Answer{ContentType: ctJSON, Body: `{"modules.v1":"/v1/modules/","providers.v1":"/v1/providers/"}`})
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "terraform", rm.URL+"/"), repotest.GroupOf("g", "terraform", "internal", "tf-proxy"))
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
	for _, c := range []struct{ path, ours, theirs, good string }{
		{"v1/modules/hashicorp/consul/aws/versions", "99.0.0", "0.11.0", `{"modules":[{"versions":[{"version":"99.0.0"}]}]}`},
		{"v1/providers/hashicorp/random/versions", "99.0.0", "3.6.0", `{"id":"hashicorp/random","versions":[{"version":"99.0.0"}]}`},
	} {
		repotest.FailsClosed(t, repotest.OwnerDocument{
			Remote: rm,
			Path:   "/" + c.path,
			Good:   repotest.Answer{ContentType: ctJSON, Body: c.good},
			Get:    func() *httptest.ResponseRecorder { return e.do("GET", "g", c.path, nil) },
			Ours:   has(c.ours),
			Theirs: has(c.theirs),
			Cached: cached(c.path),
			Expire: expire,
		})
		repotest.FailsOffline(t, e.repos["internal"], func() *httptest.ResponseRecorder { return e.do("GET", "g", c.path, nil) })
	}
	path := "v1/providers/hashicorp/random/99.0.0/download/linux/amd64"
	repotest.KeepsCopy(t, rm, "/"+path, repotest.Answer{ContentType: ctJSON, Body: `{"os":"linux","arch":"amd64","filename":"p.zip","download_url":"p.zip","shasum":"` + strings.Repeat("0", 64) + `"}`},
		func() *httptest.ResponseRecorder { return e.do("GET", "internal", path, nil) },
		cached(downloadKey(Route{Name: "hashicorp/random", Version: "99.0.0", OS: "linux", Arch: "amd64"})), expire)
	repotest.KeepsCopy(t, rm, "/"+discoveryPath, repotest.Answer{ContentType: ctJSON, Body: `{"modules.v1":"/v1/modules/","providers.v1":"/v1/providers/"}`},
		func() *httptest.ResponseRecorder {
			return e.do("GET", "internal", "v1/providers/hashicorp/random/versions", nil)
		},
		cached(discoveryPath), expire, repotest.WrongJSON)
}
