package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// A compact JWS (RFC 7515) signed with RS256 or ES256; nothing else is
// accepted ("none" and the HMAC algorithms never are).

type jwsHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// kty returns the JWK key type the header's algorithm needs.
func (h jwsHeader) kty() string {
	switch h.Alg {
	case "RS256":
		return "RSA"
	case "ES256":
		return "EC"
	}
	return ""
}

type jws struct {
	header  jwsHeader
	payload []byte
	signed  []byte // header.payload, what the signature covers
	sig     []byte
}

func parseJWS(raw string) (*jws, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a compact JWS")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("bad header encoding")
	}
	t := &jws{signed: []byte(parts[0] + "." + parts[1])}
	if err := json.Unmarshal(hb, &t.header); err != nil {
		return nil, errors.New("bad header")
	}
	if t.header.kty() == "" {
		return nil, fmt.Errorf("unsupported algorithm %q", t.header.Alg)
	}
	if t.payload, err = base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
		return nil, errors.New("bad payload encoding")
	}
	if t.sig, err = base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		return nil, errors.New("bad signature encoding")
	}
	return t, nil
}

// verify checks the signature with key, which must match the algorithm.
func (t *jws) verify(key crypto.PublicKey) error {
	sum := sha256.Sum256(t.signed)
	switch k := key.(type) {
	case *rsa.PublicKey:
		if t.header.Alg != "RS256" {
			return errors.New("key and algorithm differ")
		}
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], t.sig); err != nil {
			return errors.New("bad signature")
		}
	case *ecdsa.PublicKey:
		if t.header.Alg != "ES256" || k.Curve != elliptic.P256() || len(t.sig) != 64 {
			return errors.New("key and algorithm differ")
		}
		r, s := new(big.Int).SetBytes(t.sig[:32]), new(big.Int).SetBytes(t.sig[32:])
		if !ecdsa.Verify(k, sum[:], r, s) {
			return errors.New("bad signature")
		}
	default:
		return errors.New("unsupported key")
	}
	return nil
}

// jwk is a JSON Web Key (RFC 7517) of the two supported types.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k jwk) public() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("jwks: bad RSA key")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 || pub.E < 3 {
			return nil, errors.New("jwks: RSA key too small")
		}
		return pub, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, errors.New("jwks: unsupported curve")
		}
		x, err1 := base64.RawURLEncoding.DecodeString(k.X)
		y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
		if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
			return nil, errors.New("jwks: bad EC key")
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return nil, errors.New("jwks: bad EC key")
		}
		return pub, nil
	}
	return nil, errors.New("jwks: unsupported key type")
}
