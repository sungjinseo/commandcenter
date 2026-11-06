package apt

import (
	"strings"
	"testing"
)

// A hosted member answers for its distribution alone (owns), and going offline
// does not change that: the remote's suite of the same name does not stand in.
// Another distribution was never its own, and stays the proxy's.
func TestOfflineHostedHoldsItsDistribution(t *testing.T) {
	e := newEnv(t)
	e.up["dists/stable/InRelease"] = []byte("-----BEGIN PGP SIGNED MESSAGE-----\n\nOrigin: Debian\nSuite: stable\n")
	if w := e.do("PUT", "apt-hosted", "pool/hello.deb", deb(t, helloControl, "control.tar.gz")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	e.repos["apt-hosted"].Online = new(false)
	for _, p := range []string{"dists/stable/InRelease", "dists/stable/Release"} {
		if w := e.do("GET", "apt-all", p, nil); w.Code != 503 {
			t.Errorf("%s: %d %.200s, want 503", p, w.Code, w.Body)
		}
	}
	if got := e.text(t, "apt-all", "dists/bookworm/InRelease"); !strings.Contains(got, "Origin: Debian") {
		t.Fatalf("bookworm through the group: %q", got)
	}
}
