package pgp

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha1"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The gpg vectors: keys made with gpg 2.4.7 (Debian trixie) and exported
// with --export-secret-keys --armor (ed25519-subkey-pass.asc with
// --export-secret-subkeys: the primary key is a stub), their public keys
// with --export --armor (*.pub). The passphrase is "pass"; gpg protects
// the secret parts with AES-128 and an iterated, salted SHA-1 S2K.
var gpgVectors = []struct {
	file, pass string
	algo       byte
	primary    string // fingerprint
	signer     string // the key gpg would sign with
	at         int64  // when to sign: after the key was made (sqv refuses older signatures)
}{
	{"rsa3072-pass", "pass", algoRSA, "01E709FE43B5B38376B97EA7CECCFBD974D69FDE", "01E709FE43B5B38376B97EA7CECCFBD974D69FDE", 1793343300},
	{"ed25519-pass", "pass", algoEdDSA, "C0674DF600CDA8973A9CDFEF8C27E0EE5113A229", "C0674DF600CDA8973A9CDFEF8C27E0EE5113A229", 1793343300},
	// certify-only primary key (a stub here), an ed25519 signing subkey and a cv25519 encryption subkey
	{"ed25519-subkey-pass", "pass", algoEdDSA, "6B2E737E0B4E4A8957105BEA6E5D92599CE883E3", "3AFFB2BAF075D4B94E5EC83E61EE801C6B022CCB", 1793343300},
	{"p256", "", algoECDSA, "0C2A70403ABF593D1CA6AB69DA171749AD9C86A3", "0C2A70403ABF593D1CA6AB69DA171749AD9C86A3", 1793343300},
	{"ed25519", "", algoEdDSA, "B85EEC5B5E6CD310CCC4D768BF64406B8B67F88C", "B85EEC5B5E6CD310CCC4D768BF64406B8B67F88C", 1793343300},
	// a signing subkey that expired a second after it was made: the primary key signs
	{"ed25519-expired-sub", "", algoEdDSA, "55B22FB3EA6666DEDF8F9C55DB318BCE6558CD70", "55B22FB3EA6666DEDF8F9C55DB318BCE6558CD70", 1793345600},
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGPGVectors(t *testing.T) {
	doc := []byte("Origin: test\nSuite: stable\n-dash\n")
	for _, v := range gpgVectors {
		t.Run(v.file, func(t *testing.T) {
			now := time.Unix(v.at, 0)
			armored := readTestdata(t, v.file+".asc")
			if v.pass != "" {
				if _, err := ParsePrivateKey(armored); err == nil || !strings.Contains(err.Error(), "passphrase-protected") {
					t.Fatalf("protected key without its passphrase: %v", err)
				}
				if _, err := Unlock(armored, "wrong"); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
					t.Fatalf("wrong passphrase: %v", err)
				}
				var err error
				if armored, err = Unlock(armored, v.pass); err != nil {
					t.Fatal(err)
				}
				if again, err := Unlock(armored, "anything"); err != nil || again != armored {
					t.Fatalf("unlocking an unlocked key: %v", err)
				}
			}
			k, err := ParsePrivateKey(armored)
			if err != nil {
				t.Fatal(err)
			}
			if k.Fingerprint() != v.primary || k.SignerFingerprint() != v.signer || k.signer.algo != v.algo {
				t.Fatalf("primary %s signer %s algo %d", k.Fingerprint(), k.SignerFingerprint(), k.signer.algo)
			}
			// the public key block is what gpg --export writes
			want, err := unarmor(readTestdata(t, v.file+".pub"), "PGP PUBLIC KEY BLOCK")
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := unarmor(k.PublicKey(), "PGP PUBLIC KEY BLOCK"); !bytes.Equal(normalized(t, got), normalized(t, want)) {
				t.Fatal("public key block differs from gpg's export")
			}
			armoredSig, err := k.DetachSign(doc, now)
			if err != nil {
				t.Fatal(err)
			}
			s := parseSignature(t, armoredSig)
			if s.pkAlgo != v.algo || s.issuerFP != v.signer || s.issuer != v.signer[24:] {
				t.Fatalf("signature fields %+v", s)
			}
			verifyAny(t, k.signer.pub, s, doc)
			clear, err := k.ClearSign(doc, now)
			if err != nil {
				t.Fatal(err)
			}
			withTools(t, v.file+".pub", doc, armoredSig, clear)
		})
	}
}

// normalized re-encodes a binary key block's packets in the new format, so
// gpg's old-format headers compare equal to ours.
func normalized(t *testing.T, b []byte) []byte {
	t.Helper()
	pkts, err := parsePackets(b)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, p := range pkts {
		writePacket(&out, p.tag, p.body)
	}
	return out.Bytes()
}

// verifyAny checks a signature over doc with the signer's public key,
// whatever its algorithm: the test's own reading of the format.
func verifyAny(t *testing.T, pub crypto.PublicKey, s signature, doc []byte) {
	t.Helper()
	digest := digestOf(s, doc)
	if s.left != [2]byte{digest[0], digest[1]} {
		t.Fatalf("left two octets %x, digest %x", s.left, digest[:2])
	}
	ok := false
	switch pk := pub.(type) {
	case *rsa.PublicKey:
		ok = len(s.mpis) == 1 && rsa.VerifyPKCS1v15(pk, crypto.SHA256, digest, s.sig) == nil
	case *ecdsa.PublicKey:
		ok = len(s.mpis) == 2 && ecdsa.Verify(pk, digest, new(big.Int).SetBytes(s.mpis[0]), new(big.Int).SetBytes(s.mpis[1]))
	case ed25519.PublicKey:
		ok = len(s.mpis) == 2 && ed25519.Verify(pk, digest, append(leftPad(s.mpis[0], 32), leftPad(s.mpis[1], 32)...))
	}
	if !ok {
		t.Fatalf("signature does not verify (%T)", pub)
	}
}

// withTools verifies the signatures with sqv (what apt uses on Debian 13)
// and gpg --verify when they are installed: an implementation that is
// not ours. The test image (golang, Debian) has both.
func withTools(t *testing.T, pubFile string, doc []byte, detached, clear string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pub, _ := filepath.Abs(filepath.Join("testdata", pubFile))
	data, sig, in := write("Release", string(doc)), write("Release.gpg", detached), write("InRelease", clear)
	if sqv, err := exec.LookPath("sqv"); err == nil {
		if out, err := exec.Command(sqv, "--keyring", pub, sig, data).CombinedOutput(); err != nil {
			t.Fatalf("sqv: %v\n%s", err, out)
		}
		if out, err := exec.Command(sqv, "--keyring", pub, sig, write("Other", "Origin: other\n")).CombinedOutput(); err == nil {
			t.Fatalf("sqv verified a signature over other data\n%s", out)
		}
	} else {
		t.Log("sqv is not installed")
	}
	if gpg, err := exec.LookPath("gpg"); err == nil {
		home := filepath.Join(dir, "gnupg")
		os.Mkdir(home, 0o700)
		run := func(args ...string) ([]byte, error) {
			return exec.Command(gpg, append([]string{"--homedir", home, "--batch", "--no-autostart"}, args...)...).CombinedOutput()
		}
		if out, err := run("--import", pub); err != nil {
			t.Fatalf("gpg --import: %v\n%s", err, out)
		}
		for _, args := range [][]string{{"--verify", in}, {"--verify", sig, data}} {
			if out, err := run(args...); err != nil || !strings.Contains(string(out), "Good signature") {
				t.Fatalf("gpg %v: %v\n%s", args, err, out)
			}
		}
	} else {
		t.Log("gpg is not installed")
	}
}

func TestUnlockErrors(t *testing.T) {
	good := readTestdata(t, "ed25519-pass.asc")
	for name, c := range map[string]struct{ in, want string }{
		"not armor": {"x", "not an ASCII-armored"},
		"public":    {readTestdata(t, "ed25519-pass.pub"), "not an ASCII-armored pgp private key block"},
	} {
		if _, err := Unlock(c.in, "pass"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// a flipped ciphertext octet is a failed check, never a key
	raw, _ := unarmor(good, "PGP PRIVATE KEY BLOCK")
	pkts, _ := parsePackets(raw)
	var out bytes.Buffer
	for i, p := range pkts {
		if i == 0 {
			p.body = append([]byte{}, p.body...)
			p.body[len(p.body)-3] ^= 1
		}
		writePacket(&out, p.tag, p.body)
	}
	if _, err := Unlock(armor("PGP PRIVATE KEY BLOCK", out.Bytes()), "pass"); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("corrupted secret part: %v", err)
	}
	// without its signing subkey, a block whose primary key may only
	// certify (and is a stub) has no key to sign with
	unlocked, err := Unlock(readTestdata(t, "ed25519-subkey-pass.asc"), "pass")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = unarmor(unlocked, "PGP PRIVATE KEY BLOCK")
	pkts, _ = parsePackets(raw)
	out.Reset()
	skip := false
	for _, p := range pkts {
		if p.tag == tagSecretSubkey {
			skip = p.body[5] == algoEdDSA
		}
		if !skip {
			writePacket(&out, p.tag, p.body)
		}
	}
	if _, err := ParsePrivateKey(armor("PGP PRIVATE KEY BLOCK", out.Bytes())); err == nil || !strings.Contains(err.Error(), "may not sign") {
		t.Fatalf("certify-only stub: %v", err)
	}
}

func sha1Of(b []byte) []byte { s := sha1.Sum(b); return s[:] }

func TestS2K(t *testing.T) {
	// RFC 4880 §3.7.1.3: the coded count 96 is 65536 octets, 255 is 65011712
	for c, want := range map[byte]int{96: 65536, 255: 65011712, 0: 1024} {
		got := (16 + int(c)&15) << (int(c)>>4 + 6)
		if got != want {
			t.Errorf("count %d = %d, want %d", c, got, want)
		}
	}
	// a key longer than the hash takes a second, zero-prefixed context
	spec := append([]byte{s2kSalted, 2}, "saltsalt"...)
	key, rest, err := s2k(append(spec, 9), []byte("pw"), 32)
	if err != nil || len(key) != 32 || !bytes.Equal(rest, []byte{9}) {
		t.Fatalf("s2k: %x %x %v", key, rest, err)
	}
	h1, h2 := sha1Of([]byte("saltsaltpw")), sha1Of(append([]byte{0}, "saltsaltpw"...))
	if !bytes.Equal(key, append(h1, h2...)[:32]) {
		t.Fatalf("salted S2K %x", key)
	}
	if _, _, err := s2k([]byte{s2kIterated, 2, 1, 2}, nil, 16); err == nil {
		t.Fatal("truncated specifier accepted")
	}
	if _, _, err := s2k([]byte{4, 2}, nil, 16); err == nil {
		t.Fatal("unknown S2K type accepted")
	}
}
