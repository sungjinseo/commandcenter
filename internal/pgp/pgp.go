// Package pgp is the little of OpenPGP (RFC 4880) an apt repository needs:
// an unencrypted RSA v4 private key read from its ASCII armor, its public
// key block, and SHA-256 signatures of a Release file, detached
// (Release.gpg) or cleartext (InRelease). golang.org/x/crypto/openpgp is
// deprecated and affected by GO-2026-5932, so this stays with the standard
// library. Nothing here decrypts, verifies or handles other algorithms;
// the tests verify what it signs by re-implementing the check.
package pgp

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
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
	hashSHA256      = 8

	sigBinary = 0x00 // signature of a binary document (Release.gpg)
	sigText   = 0x01 // of a canonical text document (InRelease)

	subCreated  = 2  // signature creation time
	subIssuer   = 16 // issuer key ID
	subIssuerFP = 33 // issuer fingerprint
)

// maxArmored bounds a key block read from the configuration.
const maxArmored = 1 << 20

// Key is a parsed RSA private key with the user IDs and signatures of its
// key block, for the public key block.
type Key struct {
	priv        *rsa.PrivateKey
	fingerprint [20]byte
	public      []byte // the public key block's packets, binary
}

type packet struct {
	tag  int
	body []byte
}

// ParsePrivateKey reads an ASCII-armored OpenPGP private key block whose
// primary key is an unencrypted v4 RSA key (gpg --export-secret-keys
// --armor of a key made without a passphrase). Subkeys of any algorithm
// are carried into the public key block; a passphrase-protected key, a v5
// or v6 key and a non-RSA primary key are errors.
func ParsePrivateKey(armored string) (*Key, error) {
	raw, err := unarmor(armored, "PGP PRIVATE KEY BLOCK")
	if err != nil {
		return nil, err
	}
	pkts, err := parsePackets(raw)
	if err != nil {
		return nil, err
	}
	k := &Key{}
	var out bytes.Buffer
	for _, p := range pkts {
		switch p.tag {
		case tagSecretKey:
			if k.priv != nil {
				return nil, errors.New("more than one primary key in the block")
			}
			pub, priv, err := parseSecretKey(p.body)
			if err != nil {
				return nil, err
			}
			k.priv = priv
			k.fingerprint = sha1.Sum(append([]byte{0x99, byte(len(pub) >> 8), byte(len(pub))}, pub...))
			writePacket(&out, tagPublicKey, pub)
		case tagSecretSubkey:
			n, err := publicKeyLen(p.body)
			if err != nil {
				return nil, fmt.Errorf("subkey: %w", err)
			}
			writePacket(&out, tagPublicSubkey, p.body[:n])
		case tagUserID, tagUserAttr, tagSignature:
			writePacket(&out, p.tag, p.body)
		}
	}
	if k.priv == nil {
		return nil, errors.New("no private key packet in the block")
	}
	k.public = out.Bytes()
	return k, nil
}

// KeyID is the long key ID (16 hex digits) apt-key and `gpg --list-keys` show.
func (k *Key) KeyID() string { return fmt.Sprintf("%X", k.fingerprint[12:]) }

// Fingerprint is the v4 fingerprint, 40 hex digits.
func (k *Key) Fingerprint() string { return fmt.Sprintf("%X", k.fingerprint[:]) }

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
// first two digest octets and the RSA PKCS#1 v1.5 signature over the
// SHA-256 of the document, the hashed part and its trailer.
func (k *Key) sign(doc []byte, sigType byte, now time.Time) ([]byte, error) {
	var hashed bytes.Buffer
	hashed.Write([]byte{4, sigType, algoRSA, hashSHA256})
	var subs bytes.Buffer
	writeSubpacket(&subs, subCreated, binary.BigEndian.AppendUint32(nil, uint32(now.Unix())))
	writeSubpacket(&subs, subIssuerFP, append([]byte{4}, k.fingerprint[:]...))
	hashed.Write([]byte{byte(subs.Len() >> 8), byte(subs.Len())})
	hashed.Write(subs.Bytes())

	h := sha256.New()
	h.Write(doc)
	h.Write(hashed.Bytes())
	h.Write([]byte{4, 0xff, byte(hashed.Len() >> 24), byte(hashed.Len() >> 16), byte(hashed.Len() >> 8), byte(hashed.Len())})
	digest := h.Sum(nil)
	sig, err := rsa.SignPKCS1v15(nil, k.priv, crypto.SHA256, digest)
	if err != nil {
		return nil, err
	}

	var unhashed bytes.Buffer
	writeSubpacket(&unhashed, subIssuer, k.fingerprint[12:])
	body := hashed.Bytes()
	body = append(body, byte(unhashed.Len()>>8), byte(unhashed.Len()))
	body = append(body, unhashed.Bytes()...)
	body = append(body, digest[0], digest[1])
	body = appendMPI(body, new(big.Int).SetBytes(sig))
	var out bytes.Buffer
	writePacket(&out, tagSignature, body)
	return out.Bytes(), nil
}

// ---- keys ----

// parseSecretKey reads a v4 secret key packet body (§5.5.3): the public
// key fields, then the string-to-key usage octet (0: unencrypted), the
// secret MPIs d, p, q, u and a checksum. It returns the public key packet
// body and the RSA key.
func parseSecretKey(b []byte) (pub []byte, priv *rsa.PrivateKey, err error) {
	n, err := publicKeyLen(b)
	if err != nil {
		return nil, nil, err
	}
	if algo := b[5]; algo != algoRSA && algo != algoRSASignOnly {
		return nil, nil, fmt.Errorf("the signing key must be RSA (public key algorithm %d)", algo)
	}
	r := &mpiReader{b: b[6:n]}
	N, E := r.mpi(), r.mpi()
	if r.err != nil || !E.IsInt64() || E.Int64() < 3 {
		return nil, nil, errors.New("malformed RSA public key")
	}
	rest := b[n:]
	if len(rest) == 0 {
		return nil, nil, errors.New("truncated secret key packet")
	}
	if rest[0] != 0 {
		return nil, nil, errors.New("the private key is passphrase-protected; export it without a passphrase")
	}
	r = &mpiReader{b: rest[1:]}
	D, P, Q := r.mpi(), r.mpi(), r.mpi()
	r.mpi() // u = p^-1 mod q; Go recomputes its own CRT values
	if r.err != nil || len(r.b) < 2 {
		return nil, nil, errors.New("malformed RSA secret key")
	}
	priv = &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: N, E: int(E.Int64())}, D: D, Primes: []*big.Int{P, Q}}
	priv.Precompute()
	if err := priv.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid RSA key: %w", err)
	}
	if priv.N.BitLen() < 2048 {
		return nil, nil, fmt.Errorf("RSA key of %d bits: use 2048 or more", priv.N.BitLen())
	}
	return b[:n], priv, nil
}

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
	case 19, 22: // ECDSA, EdDSA: curve OID then one MPI
		r.oid()
		r.mpis(1)
	case 18: // ECDH: curve OID, one MPI, KDF parameters
		r.oid()
		r.mpis(1)
		r.oid()
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

func (r *mpiReader) mpi() *big.Int {
	if r.err != nil {
		return new(big.Int)
	}
	if len(r.b) < 2 {
		r.err = errors.New("truncated multiprecision integer")
		return new(big.Int)
	}
	n := (int(binary.BigEndian.Uint16(r.b)) + 7) / 8
	if len(r.b) < 2+n {
		r.err = errors.New("truncated multiprecision integer")
		return new(big.Int)
	}
	v := new(big.Int).SetBytes(r.b[2 : 2+n])
	r.b = r.b[2+n:]
	return v
}

func (r *mpiReader) mpis(n int) {
	for i := 0; i < n; i++ {
		r.mpi()
	}
}

// oid skips a one-octet-length field (a curve OID or KDF parameters).
func (r *mpiReader) oid() {
	if r.err != nil {
		return
	}
	if len(r.b) < 1 || len(r.b) < 1+int(r.b[0]) {
		r.err = errors.New("truncated key field")
		return
	}
	r.b = r.b[1+int(r.b[0]):]
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
