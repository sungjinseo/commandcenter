package apt

import (
	"bytes"
	"strings"
	"testing"
)

// Snapshots: MKCOL freezes dists/, PUT freezes a filtered copy, GET
// serves the frozen files and the live pool's packages under the
// snapshot, DELETE drops it.
func TestSnapshots(t *testing.T) {
	e := newEnv(t)
	hello := deb(t, helloControl, "control.tar.gz")
	e.do("PUT", "apt-hosted", "pool/x.deb", hello)
	if w := e.do("MKCOL", "apt-hosted", "snapshots/s1", nil); w.Code != 201 {
		t.Fatalf("MKCOL: %d %s", w.Code, w.Body)
	}
	if w := e.do("MKCOL", "apt-hosted", "snapshots/s1", nil); w.Code != 409 {
		t.Fatalf("MKCOL again: %d", w.Code)
	}
	frozen := e.text(t, "apt-hosted", "dists/stable/main/binary-amd64/Packages")
	e.do("PUT", "apt-hosted", "pool/y.deb", deb(t, control("hello", "2.0", "amd64"), "control.tar.gz"))
	if live := e.text(t, "apt-hosted", "dists/stable/main/binary-amd64/Packages"); !strings.Contains(live, "Version: 2.0\n") {
		t.Fatalf("live Packages:\n%s", live)
	}
	if got := e.text(t, "apt-hosted", "snapshots/s1/dists/stable/main/binary-amd64/Packages"); got != frozen {
		t.Fatalf("snapshot Packages changed:\n%s", got)
	}
	e.text(t, "apt-hosted", "snapshots/s1/dists/stable/Release")
	if w := e.do("GET", "apt-hosted", "snapshots/s1/pool/main/h/hello/hello_1.0_amd64.deb", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), hello) {
		t.Fatalf("package under the snapshot: %d", w.Code)
	}
	if got := e.text(t, "apt-all", "snapshots/s1/dists/stable/main/binary-amd64/Packages"); got != frozen {
		t.Fatal("through the group")
	}
	// PUT with a selector keeps only the listed architectures' indexes
	e.do("PUT", "apt-hosted", "pool/z.deb", deb(t, control("other", "1", "arm64"), "control.tar.gz"))
	if w := e.do("PUT", "apt-hosted", "snapshots/s2", []byte("Architectures: arm64\nComponents: main\n")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "apt-hosted", "snapshots/s2/dists/stable/main/binary-amd64/Packages", nil); w.Code != 404 {
		t.Fatalf("filtered out: %d", w.Code)
	}
	e.text(t, "apt-hosted", "snapshots/s2/dists/stable/main/binary-arm64/Packages")
	e.text(t, "apt-hosted", "snapshots/s2/dists/stable/Release")
	if w := e.do("PUT", "apt-hosted", "snapshots/s3", []byte("no field here")); w.Code != 400 {
		t.Fatalf("bad selector: %d", w.Code)
	}
	// regenerating the live dists/ leaves the snapshots alone
	if w := e.do("POST", "apt-hosted", "rebuild-indexes", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if got := e.text(t, "apt-hosted", "snapshots/s1/dists/stable/main/binary-amd64/Packages"); got != frozen {
		t.Fatal("snapshot changed by a rebuild")
	}
	if w := e.do("DELETE", "apt-hosted", "snapshots/s1", nil); w.Code != 204 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	if w := e.do("DELETE", "apt-hosted", "snapshots/s1", nil); w.Code != 404 {
		t.Fatalf("DELETE again: %d", w.Code)
	}
	if w := e.do("GET", "apt-hosted", "snapshots/s1/dists/stable/Release", nil); w.Code != 404 {
		t.Fatalf("deleted snapshot: %d", w.Code)
	}
	for _, c := range []struct{ method, repo, path string }{
		{"MKCOL", "apt-proxy", "snapshots/s1"}, {"MKCOL", "apt-hosted", "snapshots/a/b"}, {"MKCOL", "apt-hosted", "snapshots/.x"},
	} {
		if w := e.do(c.method, c.repo, c.path, nil); w.Code != 405 && w.Code != 400 {
			t.Errorf("%s %s/%s: %d", c.method, c.repo, c.path, w.Code)
		}
	}
	// an empty repository's snapshot has its (empty) Release
	if w := e.do("MKCOL", "apt-allow", "snapshots/empty", nil); w.Code != 201 {
		t.Fatalf("empty: %d %s", w.Code, w.Body)
	}
	e.text(t, "apt-allow", "snapshots/empty/dists/stable/Release")
}
