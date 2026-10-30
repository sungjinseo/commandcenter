package apt

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/pgp"
)

// A flat remote's Release and Packages are metadata: they expire by
// metadataMaxAge (they were content, cached for ever).
func TestProxyFlat(t *testing.T) {
	e := newEnv(t)
	e.up["Release"], e.up["Packages"] = []byte("Origin: flat\nDate: 1\n"), []byte("Package: jenkins\n")
	e.up["binary/jenkins_2.4_all.deb"] = deb(t, control("jenkins", "2.4", "all"), "control.tar.gz")
	if got := e.text(t, "apt-flat", "Release"); got != "Origin: flat\nDate: 1\n" {
		t.Fatalf("Release %q", got)
	}
	e.text(t, "apt-flat", "binary/jenkins_2.4_all.deb")
	e.up["Release"] = []byte("Origin: flat\nDate: 2\n")
	e.now = e.now.Add(30 * time.Minute)
	if got := e.text(t, "apt-flat", "Release"); got != "Origin: flat\nDate: 1\n" {
		t.Fatalf("within metadataMaxAge: %q", got)
	}
	e.hits.Store(0)
	e.now = e.now.Add(time.Hour)
	if got := e.text(t, "apt-flat", "Release"); got != "Origin: flat\nDate: 2\n" {
		t.Fatalf("after metadataMaxAge: %q", got)
	}
	e.text(t, "apt-flat", "binary/jenkins_2.4_all.deb") // a package stays cached
	if e.hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1", e.hits.Load())
	}
}

// A Release that comes back changed expires the metadata cached with the
// old one, so apt never gets a Packages its InRelease does not list.
func TestProxyReleaseCoherence(t *testing.T) {
	e := newEnv(t)
	pk, byHash := "dists/bookworm/main/binary-amd64/Packages", "dists/bookworm/main/binary-amd64/by-hash/SHA256/aa"
	e.up[pk], e.up[byHash] = []byte("Package: a\n"), []byte("by hash")
	e.text(t, "apt-proxy", "dists/bookworm/InRelease")
	e.now = e.now.Add(50 * time.Minute)
	e.text(t, "apt-proxy", "dists/bookworm/Release")
	e.text(t, "apt-proxy", pk)
	e.text(t, "apt-proxy", byHash)
	e.up["dists/bookworm/InRelease"], e.up[pk] = []byte("new InRelease"), []byte("Package: b\n")
	e.up["dists/bookworm/Release"] = []byte("new Release")
	e.now = e.now.Add(20 * time.Minute) // InRelease expired, the rest fresh
	if got := e.text(t, "apt-proxy", pk); got != "Package: a\n" {
		t.Fatalf("fresh Packages before the InRelease refresh: %q", got)
	}
	if got := e.text(t, "apt-proxy", "dists/bookworm/InRelease"); got != "new InRelease" {
		t.Fatalf("InRelease %q", got)
	}
	e.hits.Store(0)
	if got := e.text(t, "apt-proxy", pk); got != "Package: b\n" {
		t.Fatalf("Packages after the InRelease changed: %q", got)
	}
	if got := e.text(t, "apt-proxy", "dists/bookworm/Release"); got != "new Release" {
		t.Fatalf("Release after the InRelease changed: %q", got)
	}
	e.text(t, "apt-proxy", byHash)
	if e.hits.Load() != 2 {
		t.Fatalf("upstream hits %d, want 2 (Packages and Release; by-hash stays)", e.hits.Load())
	}
	// an unchanged InRelease expires nothing
	e.now = e.now.Add(2 * time.Hour)
	e.text(t, "apt-proxy", "dists/bookworm/InRelease")
	e.hits.Store(0)
	e.now = e.now.Add(time.Minute)
	e.text(t, "apt-proxy", "dists/bookworm/Release")
	if e.hits.Load() != 1 { // its own expiry only
		t.Fatalf("hits %d", e.hits.Load())
	}
}

// enforceDistribution: a proxy (alone or in a group) has no other
// dists/<name>/, without asking its remote.
func TestEnforceDistribution(t *testing.T) {
	e := newEnv(t)
	e.up["dists/trixie/Release"] = []byte("trixie")
	for _, name := range []string{"apt-enforce", "apt-enforce-group"} {
		if w := e.do("GET", name, "dists/trixie/Release", nil); w.Code != 404 {
			t.Fatalf("%s other distribution: %d", name, w.Code)
		}
		if got := e.text(t, name, "dists/bookworm/Release"); got != "Origin: Debian\nSuite: stable\n" {
			t.Fatalf("%s: %q", name, got)
		}
		e.text(t, name, "pool/main/u/upstream/upstream_2.0_amd64.deb")
	}
	if e.hits.Load() != 2 {
		t.Fatalf("upstream hits %d, want 2", e.hits.Load())
	}
	if got := e.text(t, "apt-proxy", "dists/trixie/Release"); got != "trixie" {
		t.Fatalf("without enforcement: %q", got)
	}
}

// A hosted repository answers the pool path Nexus gives a package, for
// URLs of a migrated repository; directly and in a group.
func TestNexusPoolPath(t *testing.T) {
	e := newEnv(t)
	hello := deb(t, helloControl, "control.tar.gz")
	e.do("PUT", "apt-hosted", "pool/x.deb", hello)
	for _, name := range []string{"apt-hosted", "apt-all"} {
		w := e.do("GET", name, "pool/h/hello/hello_1.0_amd64.deb", nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), hello) || w.Header().Get("Content-Type") != ctDeb {
			t.Fatalf("%s: %d %v", name, w.Code, w.Header())
		}
	}
	for _, p := range []string{"pool/h/hello/hello_2.0_amd64.deb", "pool/x/hello/hello_1.0_amd64.deb", "pool/h/other/hello_1.0_amd64.deb"} {
		if w := e.do("GET", "apt-hosted", p, nil); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

// POST rebuild-indexes regenerates dists/ (Nexus's endpoint).
func TestRebuildIndexes(t *testing.T) {
	e := newEnv(t)
	e.do("PUT", "apt-hosted", "pool/x.deb", deb(t, helloControl, "control.tar.gz"))
	if err := e.h.Store.DB.DeleteAsset(t.Context(), "apt-hosted", "dists/stable/main/binary-amd64/Packages"); err != nil {
		t.Fatal(err)
	}
	if w := e.do("POST", "apt-hosted", "rebuild-indexes", nil); w.Code != 200 {
		t.Fatalf("POST rebuild-indexes: %d %s", w.Code, w.Body)
	}
	if got := e.text(t, "apt-hosted", "dists/stable/main/binary-amd64/Packages"); !strings.HasPrefix(got, "Package: hello\n") {
		t.Fatalf("Packages %q", got)
	}
	if w := e.do("POST", "apt-proxy", "rebuild-indexes", nil); w.Code != 405 {
		t.Fatalf("proxy: %d", w.Code)
	}
	if w := e.do("GET", "apt-hosted", "rebuild-indexes", nil); w.Code != 404 {
		t.Fatalf("GET: %d", w.Code)
	}
}

// dists/ that do not follow the settings (the update's regeneration
// failed) are regenerated on the request that misses them.
func TestHeal(t *testing.T) {
	e := newEnv(t)
	e.do("PUT", "apt-hosted", "pool/x.deb", deb(t, helloControl, "control.tar.gz"))
	rp := e.repos["apt-hosted"]
	rp.Distribution = "trixie"
	if got := e.text(t, "apt-hosted", "dists/trixie/Release"); !strings.Contains(got, "\nSuite: trixie\n") {
		t.Fatalf("renamed distribution:\n%s", got)
	}
	if w := e.do("GET", "apt-hosted", "dists/stable/Release", nil); w.Code != 404 {
		t.Fatalf("old distribution: %d", w.Code)
	}
	if w := e.do("GET", "apt-hosted", "dists/trixie/InRelease", nil); w.Code != 404 {
		t.Fatalf("unsigned: %d", w.Code)
	}
	rp.SigningKey = testSigningKey(t)
	if got := e.text(t, "apt-hosted", "dists/trixie/InRelease"); !strings.HasPrefix(got, "-----BEGIN PGP SIGNED MESSAGE-----\n") {
		t.Fatalf("key added:\n%s", got)
	}
	// nothing to repair: a missing architecture stays a plain 404
	if w := e.do("GET", "apt-hosted", "dists/trixie/main/binary-i386/Packages", nil); w.Code != 404 {
		t.Fatalf("missing arch: %d", w.Code)
	}
}

// Keys from gpg: Ed25519 with a signing subkey (the primary a stub),
// unlocked with its passphrase, signs Release files apt's verifier (sqv)
// and gpg accept with the key public.key serves.
func TestSignedWithGPGKey(t *testing.T) {
	e := newEnv(t)
	e.now = time.Unix(1793343300, 0) // after the test keys were made
	armored, err := os.ReadFile(filepath.Join("..", "..", "pgp", "testdata", "ed25519-subkey-pass.asc"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := pgp.Unlock(string(armored), "pass")
	if err != nil {
		t.Fatal(err)
	}
	e.repos["apt-signed"].SigningKey = key
	e.do("PUT", "apt-signed", "pool/x.deb", deb(t, helloControl, "control.tar.gz"))
	dir := t.TempDir()
	for name, p := range map[string]string{"key.asc": "public.key", "Release": "dists/jammy/Release", "Release.gpg": "dists/jammy/Release.gpg", "InRelease": "dists/jammy/InRelease"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(e.text(t, "apt-signed", p)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in := e.text(t, "apt-signed", "dists/jammy/InRelease")
	if !strings.Contains(in, "\nComponents: internal\n") {
		t.Fatalf("InRelease:\n%s", in)
	}
	sqv, err := exec.LookPath("sqv")
	if err != nil {
		t.Skip("sqv is not installed")
	}
	if out, err := exec.Command(sqv, "--keyring", filepath.Join(dir, "key.asc"), filepath.Join(dir, "Release.gpg"), filepath.Join(dir, "Release")).CombinedOutput(); err != nil {
		t.Fatalf("sqv: %v\n%s", err, out)
	}
}

// A change in the same second as the previous one still has a newer
// Last-Modified (apt's If-Modified-Since has whole seconds), and an
// index that did not change keeps its own.
func TestRegeneratedLastModified(t *testing.T) {
	e := newEnv(t) // its clock stands still
	e.do("PUT", "apt-hosted", "pool/a.deb", deb(t, control("aa", "1", "amd64"), "control.tar.gz"))
	e.do("PUT", "apt-hosted", "pool/b.deb", deb(t, control("bb", "1", "arm64"), "control.tar.gz"))
	lm := func(p string) string {
		t.Helper()
		w := e.do("GET", "apt-hosted", p, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d", p, w.Code)
		}
		return w.Header().Get("Last-Modified")
	}
	release, amd64 := lm("dists/stable/Release"), lm("dists/stable/main/binary-amd64/Packages")
	e.do("PUT", "apt-hosted", "pool/c.deb", deb(t, control("cc", "1", "arm64"), "control.tar.gz"))
	if w := e.do("GET", "apt-hosted", "dists/stable/Release", nil, "If-Modified-Since", release); w.Code != 200 {
		t.Fatalf("changed Release in the same second: %d", w.Code)
	}
	if got := lm("dists/stable/main/binary-amd64/Packages"); got != amd64 {
		t.Fatalf("unchanged Packages: Last-Modified %s, was %s", got, amd64)
	}
}
