// Package pgp is the little of OpenPGP (RFC 4880, RFC 6637) an apt
// repository needs: a v4 private key read from its ASCII armor (Unlock
// removes its passphrase first), its public key block, and SHA-256
// signatures of a Release file, detached (Release.gpg) or cleartext
// (InRelease), made with the key's signing subkey or its primary key: RSA,
// ECDSA (NIST curves) or EdDSA (Ed25519, as gpg writes it).
// golang.org/x/crypto/openpgp is deprecated and affected by GO-2026-5932,
// so this stays with the standard library. Nothing here verifies; the
// tests check what it signs with their own reading of the format and,
// where one is installed, with sqv.
package pgp

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Packet tags (RFC 4880 §4.3) and the field values used here.
const (
	tagSignature    = 2
	tagSecretKey    = 5
	tagPublicKey    = 6
	tagSecretSubkey = 7
	tagUserID       = 13
	tagPublicSubkey = 14
	tagUserAttr     = 17

	algoRSA         = 1 // RSA encrypt or sign
	algoRSASignOnly = 3
	algoECDSA       = 19
	algoEdDSA       = 22 // EdDSA as gpg writes Ed25519 keys (RFC 4880bis "EdDSALegacy")
	hashSHA256      = 8

	sigBinary = 0x00 // signature of a binary document (Release.gpg)
	sigText   = 0x01 // of a canonical text document (InRelease)

	subCreated  = 2  // signature creation time
	subKeyExp   = 9  // key expiration time, seconds after the key's creation
	subIssuer   = 16 // issuer key ID
	subKeyFlags = 27 // key flags of a self-signature or a subkey binding
	subIssuerFP = 33 // issuer fingerprint

	flagSign = 0x02 // key flags: may sign data
)

// maxArmored bounds a key block read from the configuration.
const maxArmored = 1 << 20

// Key is a parsed private key block: the key that signs (a signing subkey,
// or the primary key) and the public key block.
type Key struct {
	signer      *signer
	fingerprint [20]byte // the primary key's
	public      []byte   // the public key block's packets, binary
}

// signer is a key with its secret part: its algorithm and fingerprint, as
// a signature names them, and sign, which signs a digest and returns the
// algorithm's signature MPIs.
type signer struct {
	algo byte
	fp   [20]byte
	pub  crypto.PublicKey // for the tests
	sign func(digest []byte) ([]byte, error)
}

type packet struct {
	tag  int
	body []byte
}

// keyInfo is what ParsePrivateKey learns about one key of the block.
type keyInfo struct {
	pub       []byte // the public key packet body
	fp        [20]byte
	secret    []byte // the cleartext secret MPIs and checksum; nil without
	encrypted bool   // the secret part is passphrase-protected
	flags     int    // key flags of its self-signature or binding; -1 when none says
	expires   uint32 // seconds after its creation it expires, by the same signature; 0: never
	revoked   bool
}

// ParsePrivateKey reads an ASCII-armored OpenPGP private key block of v4
// keys (gpg --export-secret-keys --armor, or --export-secret-subkeys).
// The key that signs is the newest subkey bound with the signing flag, not
// revoked or expired, whose secret part the block holds, as gpg chooses,
// else the primary key
// unless its flags exclude signing. It must be RSA (2048 bits or more),
// ECDSA on P-256, P-384 or P-521, or Ed25519, and not passphrase-protected
// (see Unlock). Every key's public part goes into the public key block.
func ParsePrivateKey(armored string) (*Key, error) {
	raw, err := unarmor(armored, "PGP PRIVATE KEY BLOCK")
	if err != nil {
		return nil, err
	}
	pkts, err := parsePackets(raw)
	if err != nil {
		return nil, err
	}
	var primary *keyInfo
	var subkeys []*keyInfo
	cur := (*keyInfo)(nil) // the key the next signatures are about
	var out bytes.Buffer
	for _, p := range pkts {
		switch p.tag {
		case tagSecretKey, tagSecretSubkey:
			if p.tag == tagSecretKey && primary != nil {
				return nil, errors.New("more than one primary key in the block")
			}
			if p.tag == tagSecretSubkey && primary == nil {
				return nil, errors.New("a subkey before the primary key")
			}
			k, err := readSecretPacket(p.body)
			if err != nil {
				return nil, err
			}
			cur = k
			if p.tag == tagSecretKey {
				primary = k
				writePacket(&out, tagPublicKey, k.pub)
			} else {
				subkeys = append(subkeys, k)
				writePacket(&out, tagPublicSubkey, k.pub)
			}
		case tagSignature:
			if cur != nil {
				noteSignature(cur, cur == primary, p.body)
			}
			writePacket(&out, p.tag, p.body)
		case tagUserID, tagUserAttr:
			writePacket(&out, p.tag, p.body)
		}
	}
	if primary == nil {
		return nil, errors.New("no private key packet in the block")
	}
	if primary.revoked {
		return nil, errors.New("the key is revoked")
	}
	k, err := chooseSigner(primary, subkeys)
	if err != nil {
		return nil, err
	}
	s, err := newSigner(k)
	if err != nil {
		return nil, err
	}
	return &Key{signer: s, fingerprint: primary.fp, public: out.Bytes()}, nil
}

// chooseSigner picks the key that signs, as ParsePrivateKey says.
func chooseSigner(primary *keyInfo, subkeys []*keyInfo) (*keyInfo, error) {
	var best *keyInfo
	for _, k := range subkeys {
		expired := k.expires != 0 && time.Now().Unix() >= int64(created(k))+int64(k.expires)
		if k.revoked || expired || k.flags < 0 || k.flags&flagSign == 0 || !canSign(k.pub[5]) || (k.secret == nil && !k.encrypted) {
			continue
		}
		if best == nil || created(k) >= created(best) {
			best = k
		}
	}
	if best == nil {
		switch {
		case primary.flags >= 0 && primary.flags&flagSign == 0:
			return nil, errors.New("the key has no signing subkey with its secret part, and the primary key may not sign")
		case primary.secret == nil && !primary.encrypted:
			return nil, errors.New("the block holds no secret part of a signing key (exported with --export-secret-subkeys without a signing subkey?)")
		case !canSign(primary.pub[5]):
			return nil, fmt.Errorf("the signing key must be RSA, ECDSA or Ed25519 (public key algorithm %d)", primary.pub[5])
		}
		best = primary
	}
	if best.encrypted {
		return nil, errors.New("the private key is passphrase-protected; give its passphrase or export it without one")
	}
	return best, nil
}

func canSign(algo byte) bool {
	return algo == algoRSA || algo == algoRSASignOnly || algo == algoECDSA || algo == algoEdDSA
}

func created(k *keyInfo) uint32 { return binary.BigEndian.Uint32(k.pub[1:5]) }

// readSecretPacket reads a v4 secret key or subkey packet body (§5.5.3):
// the public key fields, then the string-to-key usage octet: 0 for a
// cleartext secret part, 254 or 255 for an encrypted one or, with gpg's
// S2K extension 101, for none at all (a key kept elsewhere).
func readSecretPacket(b []byte) (*keyInfo, error) {
	n, err := publicKeyLen(b)
	if err != nil {
		return nil, err
	}
	k := &keyInfo{pub: b[:n], flags: -1}
	k.fp = sha1.Sum(append([]byte{0x99, byte(n >> 8), byte(n)}, b[:n]...))
	rest := b[n:]
	switch {
	case len(rest) == 0:
		return nil, errors.New("truncated secret key packet")
	case rest[0] == 0:
		k.secret = rest[1:]
	case (rest[0] == 254 || rest[0] == 255) && len(rest) >= 3 && rest[2] == s2kGNU:
		// no secret part here
	default:
		k.encrypted = true
	}
	return k, nil
}

// noteSignature records what a signature after k says about it: the key
// flags and expiry of the primary key's first self-signature (user ID
// certification or direct key) or of a subkey's binding, and revocations.
func noteSignature(k *keyInfo, primary bool, b []byte) {
	if len(b) < 6 || b[0] != 4 {
		return
	}
	typ := b[1]
	switch {
	case primary && typ == 0x20, !primary && typ == 0x28:
		k.revoked = true
		return
	case primary && (typ < 0x10 || typ > 0x13) && typ != 0x1f, !primary && typ != 0x18:
		return
	}
	if k.flags >= 0 {
		return
	}
	hl := int(binary.BigEndian.Uint16(b[4:6]))
	if len(b) < 6+hl {
		return
	}
	for sp := b[6 : 6+hl]; len(sp) > 0; {
		var n, hdr int
		switch {
		case sp[0] < 192:
			n, hdr = int(sp[0]), 1
		case sp[0] < 255 && len(sp) >= 2:
			n, hdr = (int(sp[0])-192)<<8+int(sp[1])+192, 2
		case sp[0] == 255 && len(sp) >= 5:
			n, hdr = int(binary.BigEndian.Uint32(sp[1:5])), 5
		default:
			return
		}
		if n < 1 || len(sp) < hdr+n {
			return
		}
		switch body := sp[hdr : hdr+n]; {
		case body[0]&0x7f == subKeyFlags && len(body) > 1:
			k.flags = int(body[1])
		case body[0]&0x7f == subKeyExp && len(body) == 5:
			k.expires = binary.BigEndian.Uint32(body[1:])
		}
		sp = sp[hdr+n:]
	}
}

// Curve OIDs (RFC 6637 §11, and gpg's for Ed25519).
var (
	oidP256    = []byte{0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}
	oidP384    = []byte{0x2b, 0x81, 0x04, 0x00, 0x22}
	oidP521    = []byte{0x2b, 0x81, 0x04, 0x00, 0x23}
	oidEd25519 = []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0xda, 0x47, 0x0f, 0x01}
)

// newSigner reads k's cleartext secret part: the algorithm's secret MPIs
// and their two-octet checksum.
func newSigner(k *keyInfo) (*signer, error) {
	pub := &mpiReader{b: k.pub[6:]}
	sec := &mpiReader{b: k.secret}
	s := &signer{algo: k.pub[5], fp: k.fp}
	switch s.algo {
	case algoRSA, algoRSASignOnly:
		N, E := pub.mpi(), pub.mpi()
		if pub.err != nil || !E.IsInt64() || E.Int64() < 3 || E.Int64() > 1<<31-1 {
			return nil, errors.New("malformed RSA public key")
		}
		D, P, Q := sec.mpi(), sec.mpi(), sec.mpi()
		sec.mpi() // u = p^-1 mod q; Go recomputes its own CRT values
		if err := sec.checksum(k.secret); err != nil {
			return nil, err
		}
		priv := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: N, E: int(E.Int64())}, D: D, Primes: []*big.Int{P, Q}}
		priv.Precompute()
		if err := priv.Validate(); err != nil {
			return nil, fmt.Errorf("invalid RSA key: %w", err)
		}
		if priv.N.BitLen() < 2048 {
			return nil, fmt.Errorf("RSA key of %d bits: use 2048 or more", priv.N.BitLen())
		}
		s.pub = &priv.PublicKey
		s.sign = func(digest []byte) ([]byte, error) {
			sig, err := rsa.SignPKCS1v15(nil, priv, crypto.SHA256, digest)
			return appendMPI(nil, new(big.Int).SetBytes(sig)), err
		}
	case algoECDSA:
		oid, point := pub.field(), pub.mpiBytes()
		var curve elliptic.Curve
		switch {
		case bytes.Equal(oid, oidP256):
			curve = elliptic.P256()
		case bytes.Equal(oid, oidP384):
			curve = elliptic.P384()
		case bytes.Equal(oid, oidP521):
			curve = elliptic.P521()
		default:
			return nil, errors.New("ECDSA keys must be on P-256, P-384 or P-521")
		}
		d := sec.mpiBytes()
		if err := sec.checksum(k.secret); err != nil {
			return nil, err
		}
		size := (curve.Params().BitSize + 7) / 8
		if pub.err != nil || len(d) > size {
			return nil, errors.New("malformed ECDSA key")
		}
		priv, err := ecdsa.ParseRawPrivateKey(curve, leftPad(d, size))
		if err != nil {
			return nil, fmt.Errorf("invalid ECDSA key: %w", err)
		}
		if b, err := priv.PublicKey.Bytes(); err != nil || !bytes.Equal(b, point) {
			return nil, errors.New("invalid ECDSA key: the secret part does not match the public key")
		}
		s.pub = &priv.PublicKey
		s.sign = func(digest []byte) ([]byte, error) {
			der, err := ecdsa.SignASN1(rand.Reader, priv, digest)
			if err != nil {
				return nil, err
			}
			var rs struct{ R, S *big.Int }
			if _, err := asn1.Unmarshal(der, &rs); err != nil {
				return nil, err
			}
			return appendMPI(appendMPI(nil, rs.R), rs.S), nil
		}
	case algoEdDSA:
		oid, point := pub.field(), pub.mpiBytes()
		if !bytes.Equal(oid, oidEd25519) {
			return nil, errors.New("EdDSA keys must be Ed25519")
		}
		seed := sec.mpiBytes()
		if err := sec.checksum(k.secret); err != nil {
			return nil, err
		}
		if pub.err != nil || len(point) != 33 || point[0] != 0x40 || len(seed) > ed25519.SeedSize {
			return nil, errors.New("malformed Ed25519 key")
		}
		priv := ed25519.NewKeyFromSeed(leftPad(seed, ed25519.SeedSize))
		if !bytes.Equal(priv.Public().(ed25519.PublicKey), point[1:]) {
			return nil, errors.New("invalid Ed25519 key: the secret part does not match the public key")
		}
		s.pub = priv.Public()
		s.sign = func(digest []byte) ([]byte, error) {
			sig := ed25519.Sign(priv, digest) // EdDSA signs the digest (RFC 4880bis §5.2.4)
			r, sv := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
			return appendMPI(appendMPI(nil, r), sv), nil
		}
	default:
		return nil, fmt.Errorf("the signing key must be RSA, ECDSA or Ed25519 (public key algorithm %d)", s.algo)
	}
	return s, nil
}

func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	return append(make([]byte, n-len(b)), b...)
}

// KeyID is the long key ID (16 hex digits) of the primary key, as apt-key
// and `gpg --list-keys` show it.
func (k *Key) KeyID() string { return fmt.Sprintf("%X", k.fingerprint[12:]) }

// Fingerprint is the primary key's v4 fingerprint, 40 hex digits.
func (k *Key) Fingerprint() string { return fmt.Sprintf("%X", k.fingerprint[:]) }

// SignerFingerprint is the fingerprint of the key that signs: a subkey's
// or the primary key's.
func (k *Key) SignerFingerprint() string { return fmt.Sprintf("%X", k.signer.fp[:]) }

// PublicKey is the ASCII-armored public key block: the key block's user
// IDs, signatures and subkeys with the secret parts dropped, as `gpg
// --export --armor` writes it.
func (k *Key) PublicKey() string { return armor("PGP PUBLIC KEY BLOCK", k.public) }

// DetachSign signs data as a binary document and returns the armored
// signature (Release.gpg).
func (k *Key) DetachSign(data []byte, now time.Time) (string, error) {
	sig, err := k.sign(data, sigBinary, now)
	if err != nil {
		return "", err
	}
	return armor("PGP SIGNATURE", sig), nil
}

// ClearSign returns text in the cleartext signature framework (RFC 4880
// §7): the text, dash-escaped, followed by the armored signature of its
// canonical form (InRelease).
func (k *Key) ClearSign(text []byte, now time.Time) (string, error) {
	sig, err := k.sign(canonical(text), sigText, now)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n")
	for _, line := range bytes.SplitAfter(text, []byte("\n")) {
		if len(line) > 0 && line[0] == '-' {
			sb.WriteString("- ")
		}
		sb.Write(line)
	}
	if len(text) > 0 && text[len(text)-1] != '\n' {
		sb.WriteByte('\n')
	}
	sb.WriteString(armor("PGP SIGNATURE", sig))
	return sb.String(), nil
}

// canonical is text as a text-document signature hashes it: trailing
// spaces and tabs removed from every line, lines ended with CRLF, and no
// line ending after the last line.
func canonical(text []byte) []byte {
	lines := bytes.Split(bytes.TrimSuffix(text, []byte("\n")), []byte("\n"))
	for i, l := range lines {
		lines[i] = bytes.TrimRight(l, " \t\r")
	}
	return bytes.Join(lines, []byte("\r\n"))
}

// sign builds a v4 signature packet (§5.2.3): the hashed subpackets
// (creation time, issuer fingerprint), the issuer key ID unhashed, the
// first two digest octets and the signing key's signature over the
// SHA-256 of the document, the hashed part and its trailer.
func (k *Key) sign(doc []byte, sigType byte, now time.Time) ([]byte, error) {
	s := k.signer
	var hashed bytes.Buffer
	hashed.Write([]byte{4, sigType, s.algo, hashSHA256})
	var subs bytes.Buffer
	writeSubpacket(&subs, subCreated, binary.BigEndian.AppendUint32(nil, uint32(now.Unix())))
	writeSubpacket(&subs, subIssuerFP, append([]byte{4}, s.fp[:]...))
	hashed.Write([]byte{byte(subs.Len() >> 8), byte(subs.Len())})
	hashed.Write(subs.Bytes())

	h := sha256.New()
	h.Write(doc)
	h.Write(hashed.Bytes())
	h.Write([]byte{4, 0xff, byte(hashed.Len() >> 24), byte(hashed.Len() >> 16), byte(hashed.Len() >> 8), byte(hashed.Len())})
	digest := h.Sum(nil)
	mpis, err := s.sign(digest)
	if err != nil {
		return nil, err
	}

	var unhashed bytes.Buffer
	writeSubpacket(&unhashed, subIssuer, s.fp[12:])
	body := hashed.Bytes()
	body = append(body, byte(unhashed.Len()>>8), byte(unhashed.Len()))
	body = append(body, unhashed.Bytes()...)
	body = append(body, digest[0], digest[1])
	body = append(body, mpis...)
	var out bytes.Buffer
	writePacket(&out, tagSignature, body)
	return out.Bytes(), nil
}

// ---- keys ----

// publicKeyLen is the length of the public key fields that start a v4
// key packet body (version, creation time, algorithm and the algorithm's
// public fields, §5.5.2): what a public key packet holds, and what a secret
// key packet begins with.
func publicKeyLen(b []byte) (int, error) {
	if len(b) < 6 {
		return 0, errors.New("truncated key packet")
	}
	if b[0] != 4 {
		return 0, fmt.Errorf("version %d key: only version 4 keys are supported", b[0])
	}
	r := &mpiReader{b: b[6:]}
	switch algo := b[5]; algo {
	case algoRSA, 2, algoRSASignOnly:
		r.mpis(2)
	case 16: // ElGamal
		r.mpis(3)
	case 17: // DSA
		r.mpis(4)
	case algoECDSA, algoEdDSA: // curve OID then one MPI
		r.field()
		r.mpis(1)
	case 18: // ECDH: curve OID, one MPI, KDF parameters
		r.field()
		r.mpis(1)
		r.field()
	default:
		return 0, fmt.Errorf("public key algorithm %d is not supported", algo)
	}
	if r.err != nil {
		return 0, r.err
	}
	return len(b) - len(r.b), nil
}

type mpiReader struct {
	b   []byte
	err error
}

// mpiBytes reads a multiprecision integer's octets (§3.2).
func (r *mpiReader) mpiBytes() []byte {
	if r.err != nil {
		return nil
	}
	if len(r.b) < 2 {
		r.err = errors.New("truncated multiprecision integer")
		return nil
	}
	n := (int(binary.BigEndian.Uint16(r.b)) + 7) / 8
	if len(r.b) < 2+n {
		r.err = errors.New("truncated multiprecision integer")
		return nil
	}
	v := r.b[2 : 2+n]
	r.b = r.b[2+n:]
	return v
}

func (r *mpiReader) mpi() *big.Int { return new(big.Int).SetBytes(r.mpiBytes()) }

func (r *mpiReader) mpis(n int) {
	for i := 0; i < n; i++ {
		r.mpi()
	}
}

// field reads a one-octet-length field (a curve OID or KDF parameters).
func (r *mpiReader) field() []byte {
	if r.err != nil {
		return nil
	}
	if len(r.b) < 1 || len(r.b) < 1+int(r.b[0]) {
		r.err = errors.New("truncated key field")
		return nil
	}
	v := r.b[1 : 1+int(r.b[0])]
	r.b = r.b[1+int(r.b[0]):]
	return v
}

// checksum checks what follows the secret MPIs read from all: the
// two-octet sum of their octets (§5.5.3), and nothing after it.
func (r *mpiReader) checksum(all []byte) error {
	if r.err != nil || len(r.b) != 2 {
		return errors.New("malformed secret key")
	}
	if binary.BigEndian.Uint16(r.b) != sum16(all[:len(all)-2]) {
		return errors.New("secret key checksum mismatch")
	}
	return nil
}

func sum16(b []byte) uint16 {
	var s uint16
	for _, c := range b {
		s += uint16(c)
	}
	return s
}

func appendMPI(b []byte, v *big.Int) []byte {
	bits := v.BitLen()
	return append(append(b, byte(bits>>8), byte(bits)), v.Bytes()...)
}

// ---- packets ----

// parsePackets splits a binary key block into packets, old and new header
// formats (§4.2). Partial body lengths and indeterminate lengths, which
// keys never use, are errors.
func parsePackets(b []byte) ([]packet, error) {
	var out []packet
	for len(b) > 0 {
		ctb := b[0]
		if ctb&0x80 == 0 {
			return nil, errors.New("not an OpenPGP packet")
		}
		var tag, hdr, n int
		if ctb&0x40 != 0 { // new format
			tag = int(ctb & 0x3f)
			switch {
			case len(b) < 2:
				return nil, errors.New("truncated packet header")
			case b[1] < 192:
				hdr, n = 2, int(b[1])
			case b[1] < 224:
				if len(b) < 3 {
					return nil, errors.New("truncated packet header")
				}
				hdr, n = 3, (int(b[1])-192)<<8+int(b[2])+192
			case b[1] == 255:
				if len(b) < 6 {
					return nil, errors.New("truncated packet header")
				}
				hdr, n = 6, int(binary.BigEndian.Uint32(b[2:6]))
			default:
				return nil, errors.New("partial body lengths are not supported")
			}
		} else {
			tag = int(ctb>>2) & 0x0f
			switch ctb & 3 {
			case 0:
				hdr = 2
			case 1:
				hdr = 3
			case 2:
				hdr = 5
			default:
				return nil, errors.New("indeterminate packet length is not supported")
			}
			if len(b) < hdr {
				return nil, errors.New("truncated packet header")
			}
			for _, c := range b[1:hdr] {
				n = n<<8 | int(c)
			}
		}
		if n < 0 || len(b) < hdr+n {
			return nil, errors.New("truncated packet")
		}
		out = append(out, packet{tag: tag, body: b[hdr : hdr+n]})
		b = b[hdr+n:]
	}
	return out, nil
}

// writePacket appends a new-format packet.
func writePacket(w *bytes.Buffer, tag int, body []byte) {
	w.WriteByte(0xc0 | byte(tag))
	switch n := len(body); {
	case n < 192:
		w.WriteByte(byte(n))
	case n < 8384:
		n -= 192
		w.Write([]byte{byte(n>>8) + 192, byte(n)})
	default:
		w.Write([]byte{255, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	}
	w.Write(body)
}

// writeSubpacket appends a signature subpacket (§5.2.3.1); all of ours are short.
func writeSubpacket(w *bytes.Buffer, typ byte, data []byte) {
	w.WriteByte(byte(1 + len(data)))
	w.WriteByte(typ)
	w.Write(data)
}

// ---- armor (§6) ----

func armor(kind string, b []byte) string {
	var sb strings.Builder
	sb.WriteString("-----BEGIN " + kind + "-----\n\n")
	enc := base64.StdEncoding.EncodeToString(b)
	for len(enc) > 64 {
		sb.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	sb.WriteString(enc + "\n=" + base64.StdEncoding.EncodeToString(crc24(b)) + "\n-----END " + kind + "-----\n")
	return sb.String()
}

// unarmor decodes the first block of the given kind: its header lines are
// skipped, the base64 body ends at the "=CRC" line or the END line, and
// the CRC, when present, must match.
func unarmor(s, kind string) ([]byte, error) {
	if len(s) > maxArmored {
		return nil, fmt.Errorf("key block is larger than %d bytes", maxArmored)
	}
	_, rest, ok := strings.Cut(s, "-----BEGIN "+kind+"-----")
	if !ok {
		return nil, errors.New("not an ASCII-armored " + strings.ToLower(kind))
	}
	lines := strings.Split(rest, "\n")[1:] // after the rest of the BEGIN line
	var body strings.Builder
	var crc string
	inBody := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "-----END "):
			if !inBody {
				return nil, errors.New("empty key block")
			}
			raw, err := base64.StdEncoding.DecodeString(body.String())
			if err != nil {
				return nil, errors.New("key block is not valid base64")
			}
			if crc != "" {
				want, err := base64.StdEncoding.DecodeString(crc)
				if err != nil || !bytes.Equal(want, crc24(raw)) {
					return nil, errors.New("key block checksum mismatch")
				}
			}
			return raw, nil
		case !inBody && line == "":
			inBody = true // headers end at the blank line
		case !inBody && strings.Contains(line, ":"):
			// an armor header (Version:, Comment:)
		case !inBody:
			inBody = true // no headers: the body starts at once
			body.WriteString(line)
		case strings.HasPrefix(line, "="):
			crc = line[1:]
		default:
			body.WriteString(line)
		}
	}
	return nil, errors.New("key block has no END line")
}

func crc24(b []byte) []byte {
	crc := uint32(0xb704ce)
	for _, c := range b {
		crc ^= uint32(c) << 16
		for i := 0; i < 8; i++ {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= 0x1864cfb
			}
		}
	}
	return []byte{byte(crc >> 16), byte(crc >> 8), byte(crc)}
}

// PublicKeyID reads an ASCII-armored OpenPGP public key block (gpg
// --export --armor) and returns the long key ID (16 hex digits) of its v4
// primary key, as Terraform's registry protocol lists signing keys. The
// key's algorithm does not matter: only the fingerprint is computed.
func PublicKeyID(armored string) (string, error) {
	raw, err := unarmor(armored, "PGP PUBLIC KEY BLOCK")
	if err != nil {
		return "", err
	}
	pkts, err := parsePackets(raw)
	if err != nil {
		return "", err
	}
	for _, p := range pkts {
		if p.tag != tagPublicKey {
			continue
		}
		if len(p.body) < 6 || p.body[0] != 4 {
			return "", errors.New("only v4 keys are supported")
		}
		fp := sha1.Sum(append([]byte{0x99, byte(len(p.body) >> 8), byte(len(p.body))}, p.body...))
		return fmt.Sprintf("%X", fp[12:]), nil
	}
	return "", errors.New("no public key packet in the block")
}
