package pgp

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/rsa2048.asc: gpg 2.3 `--quick-gen-key rsa2048` without a
// passphrase plus an rsa2048 encryption subkey, exported with
// --export-secret-keys --armor (old-format packet headers). locked.asc is
// a key with a passphrase.
const (
	keyID       = "8DF1B5FF5D95FA5B"
	fingerprint = "3ACFED104C683E9D165E5CEA8DF1B5FF5D95FA5B"
)

func testKey(t *testing.T) *Key {
	t.Helper()
	b, err := os.ReadFile("testdata/rsa2048.asc")
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParsePrivateKey(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParsePrivateKey(t *testing.T) {
	k := testKey(t)
	if k.KeyID() != keyID || k.Fingerprint() != fingerprint {
		t.Fatalf("key id %s fingerprint %s", k.KeyID(), k.Fingerprint())
	}
	// the public key block: primary, user id, self-sig, subkey, binding sig
	raw, err := unarmor(k.PublicKey(), "PGP PUBLIC KEY BLOCK")
	if err != nil {
		t.Fatal(err)
	}
	pkts, err := parsePackets(raw)
	if err != nil {
		t.Fatal(err)
	}
	var tags []int
	for _, p := range pkts {
		tags = append(tags, p.tag)
	}
	if want := []int{tagPublicKey, tagUserID, tagSignature, tagPublicSubkey, tagSignature}; !equalInts(tags, want) {
		t.Fatalf("public block packets %v, want %v", tags, want)
	}
	pub := pkts[0].body
	if n, err := publicKeyLen(pub); err != nil || n != len(pub) {
		t.Fatalf("public key packet: len %d of %d, %v", n, len(pub), err)
	}
	r := &mpiReader{b: pub[6:]}
	if N, E := r.mpi(), r.mpi(); N.Cmp(k.signer.pub.(*rsa.PublicKey).N) != 0 || E.Int64() != int64(k.signer.pub.(*rsa.PublicKey).E) {
		t.Fatal("public key packet does not hold the private key's modulus and exponent")
	}
	if string(pkts[1].body) != "Command Center Test <apt@example.test>" {
		t.Fatalf("user id %q", pkts[1].body)
	}
	// the same block again, ignoring a Version: header and CRLF
	again := strings.ReplaceAll(strings.Replace(k.PublicKey(), "-----\n\n", "-----\nVersion: test\nComment: x\n\n", 1), "\n", "\r\n")
	if raw2, err := unarmor(again, "PGP PUBLIC KEY BLOCK"); err != nil || !bytes.Equal(raw, raw2) {
		t.Fatalf("armor with headers: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	locked, _ := os.ReadFile("testdata/locked.asc")
	good, _ := os.ReadFile("testdata/rsa2048.asc")
	corrupt := strings.Replace(string(good), "=J6jy", "=AAAA", 1)
	for name, c := range map[string]struct{ in, want string }{
		"passphrase": {string(locked), "passphrase-protected"},
		"not armor":  {"hello", "not an ASCII-armored"},
		"public":     {testKey(t).PublicKey(), "not an ASCII-armored pgp private key block"},
		"crc":        {corrupt, "checksum mismatch"},
		"no end":     {strings.SplitN(string(good), "=J6jy", 2)[0], "no END line"},
		"base64":     {"-----BEGIN PGP PRIVATE KEY BLOCK-----\n\n!!!!\n-----END PGP PRIVATE KEY BLOCK-----\n", "not valid base64"},
	} {
		_, err := ParsePrivateKey(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// signature is a parsed v4 signature packet, the test's own reading of
// RFC 4880 §5.2.3 (the package only writes them).
type signature struct {
	typ, pkAlgo, hashAlgo byte
	hashed                []byte // version through the hashed subpackets
	created               time.Time
	issuer, issuerFP      string
	left                  [2]byte
	sig                   []byte   // the first MPI (RSA's only one)
	mpis                  [][]byte // all of them (r and s for ECDSA and EdDSA)
}

func parseSignature(t *testing.T, armored string) signature {
	t.Helper()
	raw, err := unarmor(armored, "PGP SIGNATURE")
	if err != nil {
		t.Fatal(err)
	}
	pkts, err := parsePackets(raw)
	if err != nil || len(pkts) != 1 || pkts[0].tag != tagSignature {
		t.Fatalf("signature packets %+v: %v", pkts, err)
	}
	b := pkts[0].body
	if b[0] != 4 {
		t.Fatalf("signature version %d", b[0])
	}
	s := signature{typ: b[1], pkAlgo: b[2], hashAlgo: b[3]}
	hl := int(binary.BigEndian.Uint16(b[4:6]))
	s.hashed = b[:6+hl]
	subs := func(sp []byte) {
		for len(sp) > 0 {
			n := int(sp[0]) // our subpackets are shorter than 192 bytes
			data := sp[2 : 1+n]
			switch sp[1] {
			case subCreated:
				s.created = time.Unix(int64(binary.BigEndian.Uint32(data)), 0).UTC()
			case subIssuer:
				s.issuer = strings.ToUpper(bytesHex(data))
			case subIssuerFP:
				s.issuerFP = strings.ToUpper(bytesHex(data[1:]))
			}
			sp = sp[1+n:]
		}
	}
	subs(b[6 : 6+hl])
	b = b[6+hl:]
	ul := int(binary.BigEndian.Uint16(b[:2]))
	subs(b[2 : 2+ul])
	b = b[2+ul:]
	copy(s.left[:], b[:2])
	r := &mpiReader{b: b[2:]}
	for len(r.b) > 0 && r.err == nil {
		s.mpis = append(s.mpis, r.mpiBytes())
	}
	if r.err != nil || len(s.mpis) == 0 {
		t.Fatalf("signature MPIs: %v", r.err)
	}
	s.sig = s.mpis[0]
	return s
}

func bytesHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// verify checks s over doc with the public key: the digest of the document,
// the hashed part and the v4 trailer, the left two octets, and RSA PKCS#1
// v1.5 with SHA-256.
func verify(t *testing.T, pub *rsa.PublicKey, s signature, doc []byte) {
	t.Helper()
	digest := digestOf(s, doc)
	if s.left != [2]byte{digest[0], digest[1]} {
		t.Fatalf("left two octets %x, digest %x", s.left, digest[:2])
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest, s.sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func digestOf(s signature, doc []byte) []byte {
	h := sha256.New()
	h.Write(doc)
	h.Write(s.hashed)
	h.Write([]byte{4, 0xff, byte(len(s.hashed) >> 24), byte(len(s.hashed) >> 16), byte(len(s.hashed) >> 8), byte(len(s.hashed))})
	return h.Sum(nil)
}

func TestDetachSign(t *testing.T) {
	k := testKey(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	doc := []byte("Origin: test\nSuite: stable\n")
	armored, err := k.DetachSign(doc, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(armored, "-----BEGIN PGP SIGNATURE-----\n\n") || !strings.HasSuffix(armored, "-----END PGP SIGNATURE-----\n") {
		t.Fatalf("armor:\n%s", armored)
	}
	s := parseSignature(t, armored)
	if len(s.mpis) != 1 || s.typ != sigBinary || s.pkAlgo != algoRSA || s.hashAlgo != hashSHA256 || !s.created.Equal(now) || s.issuer != keyID || s.issuerFP != fingerprint {
		t.Fatalf("signature fields %+v", s)
	}
	verify(t, k.signer.pub.(*rsa.PublicKey), s, doc)
	// deterministic (PKCS#1 v1.5): the same input signs the same, so a
	// regenerated Release.gpg does not change without cause
	if again, _ := k.DetachSign(doc, now); again != armored {
		t.Fatal("signature differs between runs")
	}
	// another document, or a corrupted signature, does not verify
	if err := rsa.VerifyPKCS1v15(k.signer.pub.(*rsa.PublicKey), crypto.SHA256, digestOf(s, []byte("Origin: other\n")), s.sig); err == nil {
		t.Fatal("a signature over other text verified")
	}
	s.sig[10] ^= 1
	if err := rsa.VerifyPKCS1v15(k.signer.pub.(*rsa.PublicKey), crypto.SHA256, digestOf(s, doc), s.sig); err == nil {
		t.Fatal("a corrupted signature verified")
	}
	t.Logf("DETACHED-BEGIN\n%sDETACHED-END", armored)
}

func TestClearSign(t *testing.T) {
	k := testKey(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	// trailing blanks, a dash line and a missing final newline exercise
	// the canonical form and dash escaping
	text := []byte("Origin: test  \nSuite: stable\t\n-dash line\n\nLast")
	out, err := k.ClearSign(text, now)
	if err != nil {
		t.Fatal(err)
	}
	const header = "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n"
	body, sig, ok := strings.Cut(strings.TrimPrefix(out, header), "-----BEGIN PGP SIGNATURE-----")
	if !strings.HasPrefix(out, header) || !ok {
		t.Fatalf("framework:\n%s", out)
	}
	if body != "Origin: test  \nSuite: stable\t\n- -dash line\n\nLast\n" {
		t.Fatalf("body %q", body)
	}
	// a reader un-escapes the dashes and hashes the canonical text
	var lines []string
	for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		lines = append(lines, strings.TrimRight(strings.TrimPrefix(l, "- "), " \t"))
	}
	canon := strings.Join(lines, "\r\n")
	if canon != "Origin: test\r\nSuite: stable\r\n-dash line\r\n\r\nLast" {
		t.Fatalf("canonical %q", canon)
	}
	s := parseSignature(t, "-----BEGIN PGP SIGNATURE-----"+sig)
	if s.typ != sigText || s.issuer != keyID {
		t.Fatalf("signature fields %+v", s)
	}
	verify(t, k.signer.pub.(*rsa.PublicKey), s, []byte(canon))
	// text that already ends with a newline gets no second one
	out2, _ := k.ClearSign([]byte("A: 1\n"), now)
	if !strings.Contains(out2, "\n\nA: 1\n-----BEGIN PGP SIGNATURE-----\n") {
		t.Fatalf("framework:\n%s", out2)
	}
	t.Logf("CLEAR-BEGIN\n%sCLEAR-END", out)
}

func TestPacketLengths(t *testing.T) {
	for _, n := range []int{0, 1, 191, 192, 8383, 8384, 70000} {
		var buf bytes.Buffer
		body := bytes.Repeat([]byte{7}, n)
		writePacket(&buf, tagUserID, body)
		pkts, err := parsePackets(buf.Bytes())
		if err != nil || len(pkts) != 1 || pkts[0].tag != tagUserID || !bytes.Equal(pkts[0].body, body) {
			t.Fatalf("len %d: %v %d", n, err, len(pkts))
		}
	}
	for _, bad := range [][]byte{{0x00}, {0xc2}, {0xc2, 5, 1}, {0xc2, 224}, {0x8b, 1}, {0x8b, 0, 5, 1}} {
		if _, err := parsePackets(bad); err == nil {
			t.Errorf("no error for % x", bad)
		}
	}
	if v := appendMPI(nil, big.NewInt(511)); !bytes.Equal(v, []byte{0, 9, 1, 255}) {
		t.Fatalf("mpi %x", v)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPublicKeyID(t *testing.T) {
	k := testKey(t)
	id, err := PublicKeyID(k.PublicKey())
	if err != nil || id != k.KeyID() {
		t.Fatalf("PublicKeyID = %q, %v; want %q", id, err, k.KeyID())
	}
	if _, err := PublicKeyID("nope"); err == nil {
		t.Fatal("garbage accepted")
	}
	// gpg --armor --export of an ed25519 key with a cv25519 subkey: the
	// primary key's id, as gpg --list-keys --keyid-format long shows it.
	if id, err := PublicKeyID(gpgEd25519Public); err != nil || id != "07BB5F69718F7C89" {
		t.Fatalf("gpg export: %q, %v", id, err)
	}
}

const gpgEd25519Public = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEauKcjxYJKwYBBAHaRw8BAQdAWb+7JIhw9YyJK5ekNm1dw6Mt+dQs+muo4Pj8
56VVeFy0GFRGIFRlc3QgPHRmQGV4YW1wbGUuY29tPoiUBBMWCgA8FiEEc/zGzH8+
FR8uhfH5B7tfaXGPfIkFAmrinI8CGwMFCwkIBwIDIgIBBhUKCQgLAgQWAgMBAh4H
AheAAAoJEAe7X2lxj3yJYMYBAMEdBi+9zGpF1Xw/ludgCobUm6zkMKx69/dmEccT
D6fbAP986bcflKqmd5sDrM2Su4P5pzo/vYnMxPqnWW8a7xkoA7g4BGrinJUSCisG
AQQBl1UBBQEBB0BVOG9RD8XHqqK74L/xXlM+rL/d4d3ynn+v52mgSIYFIgMBCAeI
eAQYFgoAIBYhBHP8xsx/PhUfLoXx+Qe7X2lxj3yJBQJq4pyVAhsMAAoJEAe7X2lx
j3yJvVgBALkp4ojqDNB70YpGo9Kp1Gs0837kuUW3zXAJbcfrUQVFAP9aBIRU4bjf
tLBOztB4Eq/SG0lOBmVlCvnRhjboEvvUAg==
=pU3b
-----END PGP PUBLIC KEY BLOCK-----
`
