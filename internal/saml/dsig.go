package saml

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
)

// Verification of enveloped XML Signatures (https://www.w3.org/TR/xmldsig-core1/)
// in the shape SAML identity providers emit them: one Reference to the
// signed element's ID, the enveloped-signature transform then exclusive
// canonicalization, SignedInfo canonicalized exclusively, RSA-SHA256 or
// ECDSA-SHA256 (RSA-SHA1 and SHA-1 digests only when allowed). Anything
// else is refused rather than half supported. The key is never taken from
// the document: only the identity provider's metadata certificates verify.

const (
	algEnveloped   = "http://www.w3.org/2000/09/xmldsig#enveloped-signature"
	algRSASHA256   = "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"
	algRSASHA1     = "http://www.w3.org/2000/09/xmldsig#rsa-sha1"
	algECDSASHA256 = "http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha256"
	algSHA256      = "http://www.w3.org/2001/04/xmlenc#sha256"
	algSHA1        = "http://www.w3.org/2000/09/xmldsig#sha1"
)

var (
	errSignature   = errors.New("the signature does not verify")
	errUnknownCert = errors.New("it is signed with a certificate that is not in the identity provider's metadata")
)

// verifier checks signatures against the pinned certificates.
type verifier struct {
	certs     []*x509.Certificate
	allowSHA1 bool
}

// c14nMethod is a canonicalization algorithm with its PrefixList.
type c14nMethod struct {
	comments bool
	prefixes string // the PrefixList as written
}

func (m c14nMethod) inclusive() []string { return strings.Fields(m.prefixes) }

// verify checks sig, an enveloped ds:Signature child of target, and returns
// the canonical form of target without sig: exactly the bytes the signature
// covers. The caller parses those again and reads nothing else, so a node
// the signature does not cover can never be consumed.
func (v verifier) verify(target, sig *node) ([]byte, error) {
	id, _ := target.attr("ID")
	if id == "" {
		return nil, errors.New("the signed element has no ID")
	}
	kids, err := sig.elements()
	if err != nil {
		return nil, err
	}
	if len(kids) < 2 || !kids[0].is(nsDS, "SignedInfo") || !kids[1].is(nsDS, "SignatureValue") {
		return nil, errors.New("the signature lacks SignedInfo or SignatureValue")
	}
	for i, k := range kids[2:] {
		if i > 0 || !k.is(nsDS, "KeyInfo") {
			return nil, fmt.Errorf("the signature has an unsupported <%s> element", k.local)
		}
		if err := v.knownCert(k); err != nil {
			return nil, err
		}
	}
	// Canonicalize SignedInfo as it says, verify it, then read only the
	// verified copy.
	cm, sm, err := v.methods(kids[0])
	if err != nil {
		return nil, err
	}
	si := canonical(kids[0], nil, cm.comments, cm.inclusive())
	sv, err := decode64(kids[1].content())
	if err != nil {
		return nil, errors.New("the signature value is not base64")
	}
	if err := v.check(sm, si, sv); err != nil {
		return nil, err
	}
	vsi, err := parse(si, nil)
	if err != nil {
		return nil, err
	}
	cm2, sm2, err := v.methods(vsi)
	if err != nil || cm2 != cm || sm2 != sm {
		return nil, errSignature
	}
	ref, err := v.reference(vsi)
	if err != nil {
		return nil, err
	}
	if ref.uri != "#"+id {
		return nil, errors.New("the signature covers another element than the one it is in")
	}
	// A same-document reference by ID drops comments, whatever the
	// transform says (XML Signature 4.4.3.3).
	out := canonical(target, sig, false, ref.c14n.inclusive())
	var sum []byte
	if ref.digest == algSHA1 {
		s := sha1.Sum(out)
		sum = s[:]
	} else {
		s := sha256.Sum256(out)
		sum = s[:]
	}
	if !hmac.Equal(sum, ref.value) {
		return nil, errSignature
	}
	return out, nil
}

// methods reads SignedInfo's canonicalization and signature methods.
func (v verifier) methods(si *node) (c14nMethod, string, error) {
	kids, err := si.elements()
	if err != nil {
		return c14nMethod{}, "", err
	}
	if len(kids) < 3 || !kids[0].is(nsDS, "CanonicalizationMethod") || !kids[1].is(nsDS, "SignatureMethod") {
		return c14nMethod{}, "", errors.New("SignedInfo is malformed")
	}
	cm, err := c14nOf(kids[0])
	if err != nil {
		return c14nMethod{}, "", err
	}
	sm, _ := kids[1].attr("Algorithm")
	switch {
	case sm == algRSASHA256 || sm == algECDSASHA256:
	case sm == algRSASHA1 && v.allowSHA1:
	case sm == algRSASHA1:
		return c14nMethod{}, "", errors.New("the signature uses RSA-SHA1, which is refused (allowSHA1Signatures is off)")
	default:
		return c14nMethod{}, "", fmt.Errorf("unsupported signature method %q", sm)
	}
	if k, _ := kids[1].elements(); len(k) > 0 {
		return c14nMethod{}, "", errors.New("SignatureMethod has unsupported parameters")
	}
	return cm, sm, nil
}

// c14nOf reads a CanonicalizationMethod or Transform naming exclusive
// canonicalization, with its optional InclusiveNamespaces.
func c14nOf(e *node) (c14nMethod, error) {
	var m c14nMethod
	switch alg, _ := e.attr("Algorithm"); alg {
	case algExcC14N:
	case algExcC14NComments:
		m.comments = true
	default:
		return m, fmt.Errorf("unsupported canonicalization %q (only exclusive canonicalization is)", alg)
	}
	kids, err := e.elements()
	if err != nil {
		return m, err
	}
	for i, k := range kids {
		if i > 0 || !k.is(nsEC, "InclusiveNamespaces") {
			return m, fmt.Errorf("unsupported <%s> in a canonicalization method", k.local)
		}
		m.prefixes, _ = k.attr("PrefixList")
	}
	return m, nil
}

type reference struct {
	uri    string
	c14n   c14nMethod
	digest string // algSHA256 or algSHA1
	value  []byte
}

// reference reads SignedInfo's only Reference: transforms enveloped
// signature then exclusive canonicalization, a SHA-256 (or allowed SHA-1)
// digest.
func (v verifier) reference(si *node) (reference, error) {
	var ref reference
	refs := si.children(nsDS, "Reference")
	if kids, _ := si.elements(); len(refs) != 1 || len(kids) != 3 || kids[2] != refs[0] {
		return ref, errors.New("the signature must have exactly one Reference")
	}
	r := refs[0]
	ref.uri, _ = r.attr("URI")
	kids, err := r.elements()
	if err != nil {
		return ref, err
	}
	if len(kids) != 3 || !kids[0].is(nsDS, "Transforms") || !kids[1].is(nsDS, "DigestMethod") || !kids[2].is(nsDS, "DigestValue") {
		return ref, errors.New("the Reference is malformed")
	}
	tf, err := kids[0].elements()
	if err != nil {
		return ref, err
	}
	if len(tf) != 2 || !tf[0].is(nsDS, "Transform") || !tf[1].is(nsDS, "Transform") {
		return ref, errors.New("the Reference must have the enveloped-signature and exclusive canonicalization transforms")
	}
	if alg, _ := tf[0].attr("Algorithm"); alg != algEnveloped {
		return ref, errors.New("the Reference must have the enveloped-signature and exclusive canonicalization transforms")
	}
	if k, _ := tf[0].elements(); len(k) > 0 {
		return ref, errors.New("the enveloped-signature transform has unsupported parameters")
	}
	if ref.c14n, err = c14nOf(tf[1]); err != nil {
		return ref, err
	}
	switch ref.digest, _ = kids[1].attr("Algorithm"); {
	case ref.digest == algSHA256:
	case ref.digest == algSHA1 && v.allowSHA1:
	case ref.digest == algSHA1:
		return ref, errors.New("the signature uses a SHA-1 digest, which is refused (allowSHA1Signatures is off)")
	default:
		return ref, fmt.Errorf("unsupported digest method %q", ref.digest)
	}
	if ref.value, err = decode64(kids[2].content()); err != nil {
		return ref, errors.New("the digest value is not base64")
	}
	return ref, nil
}

// check verifies signature sv over data with one of the pinned keys.
func (v verifier) check(sm string, data, sv []byte) error {
	h, hash := sha256.Sum256(data), crypto.SHA256
	sum := h[:]
	if sm == algRSASHA1 {
		s := sha1.Sum(data)
		sum, hash = s[:], crypto.SHA1
	}
	for _, c := range v.certs {
		switch k := c.PublicKey.(type) {
		case *rsa.PublicKey:
			if sm != algECDSASHA256 && rsa.VerifyPKCS1v15(k, hash, sum, sv) == nil {
				return nil
			}
		case *ecdsa.PublicKey:
			// XML Signature: the value is r and s, each the curve's size.
			size := (k.Curve.Params().BitSize + 7) / 8
			if sm == algECDSASHA256 && len(sv) == 2*size &&
				ecdsa.Verify(k, sum, new(big.Int).SetBytes(sv[:size]), new(big.Int).SetBytes(sv[size:])) {
				return nil
			}
		}
	}
	return errSignature
}

// knownCert refuses a KeyInfo whose X.509 certificates are all unknown: a
// signature made with another key would fail anyway, but this names why.
func (v verifier) knownCert(ki *node) error {
	var certs [][]byte
	var bad bool
	ki.walk(func(n *node) {
		if n.is(nsDS, "X509Certificate") {
			b, err := decode64(n.content())
			bad = bad || err != nil
			certs = append(certs, b)
		}
	})
	if bad {
		return errors.New("a KeyInfo certificate is not base64")
	}
	if len(certs) == 0 {
		return nil
	}
	for _, b := range certs {
		if slices.ContainsFunc(v.certs, func(c *x509.Certificate) bool { return bytes.Equal(c.Raw, b) }) {
			return nil
		}
	}
	return errUnknownCert
}

// decode64 decodes base64 that may be wrapped over lines.
func decode64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
	return base64.StdEncoding.DecodeString(s)
}
