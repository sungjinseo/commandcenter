package docker

import (
	"testing"

	"commandcenter/internal/config"
)

// An offline member may own any image: a later member does not stand in for it,
// and the later member is not asked. What a member ahead of it has still answers.
func TestGroupOfflineMember(t *testing.T) {
	fr := newFakeRegistry(t)
	fr.putImage("web", "latest", []byte("web layer"))
	tr, f := true, false
	e := newEnvWith(t, proxyOf(fr, "hub", nil),
		config.Repository{Name: "down", Format: "docker", Type: "hosted", Online: &f, AnonymousRead: &tr},
		config.Repository{Name: "first", Format: "docker", Type: "group", Members: []string{"down", "hub"}, AnonymousRead: &tr},
		config.Repository{Name: "inner", Format: "docker", Type: "group", Members: []string{"down", "dh"}, AnonymousRead: &tr},
		config.Repository{Name: "outer", Format: "docker", Type: "group", Members: []string{"inner", "hub"}, AnonymousRead: &tr},
		config.Repository{Name: "last", Format: "docker", Type: "group", Members: []string{"dh", "down"}, AnonymousRead: &tr})
	resp, b := e.do("GET", "/v2/first/web/manifests/latest", nil)
	e.check(resp, b, 503, "")
	resp, b = e.do("GET", "/v2/outer/web/manifests/latest", nil)
	e.check(resp, b, 503, "")
	if fr.count("/v2/web/manifests/latest") != 0 {
		t.Fatal("the proxy was asked in place of an offline member")
	}
	e.pushImage("dh/app", "v1", [][]byte{[]byte("l")}, basic("ci"))
	resp, b = e.do("GET", "/v2/last/app/manifests/v1", nil)
	e.check(resp, b, 200, "")
	resp, b = e.do("GET", "/v2/last/nope/manifests/v1", nil)
	e.check(resp, b, 503, "")
	// A mount from such a group is an upload, as for a blob it does not have.
	resp, b = e.do("POST", "/v2/dh/new/blobs/uploads/?mount="+digestOf([]byte("l"))+"&from=first/app", nil, basic("ci"))
	e.check(resp, b, 202, "")
}
