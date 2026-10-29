package saml_test

import (
	"bytes"
	"compress/flate"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/saml"
	"commandcenter/internal/saml/samltest"
)

const (
	entityID = "https://cc.example/-/sso/saml/metadata"
	acs      = "https://cc.example/-/sso/saml/acs"
	reqID    = "_req1"
)

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func newSP(t *testing.T, idp *samltest.IdP) *saml.SP {
	t.Helper()
	md, err := saml.ParseMetadata([]byte(idp.Metadata()))
	if err != nil {
		t.Fatal(err)
	}
	return &saml.SP{EntityID: entityID, ACSURL: acs, IdP: md, ValidateResponseSignature: true,
		ValidateAssertionSignature: true, ClockSkew: 2 * time.Minute}
}

// build returns a Response for p: the assertion signed when signA, the
// response when signR.
func build(t *testing.T, idp *samltest.IdP, p samltest.Params, signA, signR bool) string {
	t.Helper()
	a := samltest.Assertion(p)
	if signA {
		a = idp.Sign(t, a, p.AssertionID, samltest.SignOpts{})
	}
	r := samltest.Response(p, a)
	if signR {
		r = idp.Sign(t, r, p.ResponseID, samltest.SignOpts{})
	}
	return r
}

func mustOK(t *testing.T, sp *saml.SP, doc, req string) *saml.Assertion {
	t.Helper()
	a, err := sp.ParseResponse(samltest.Encode(doc), req, now)
	if err != nil {
		t.Fatalf("refused: %v\n%s", err, doc)
	}
	return a
}

func mustFail(t *testing.T, sp *saml.SP, name, doc, req, want string) {
	t.Helper()
	_, err := sp.ParseResponse(samltest.Encode(doc), req, now)
	if err == nil {
		t.Fatalf("%s: accepted", name)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: error %q, want %q", name, err, want)
	}
}

func TestValidSignatures(t *testing.T) {
	idp := samltest.New(t)
	sp := newSP(t, idp)
	for _, c := range []struct {
		name         string
		signA, signR bool
	}{{"response signed", false, true}, {"assertion signed", true, false}, {"both signed", true, true}} {
		p := idp.Defaults(entityID, acs, reqID, now)
		a, err := sp.ParseResponse(samltest.Encode(build(t, idp, p, c.signA, c.signR)), reqID, now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if a.NameID != "Alice" || a.ID != p.AssertionID || strings.Join(a.Values("groups"), ",") != "cc-admins,staff" ||
			strings.Join(a.Values("email"), "") != "alice@example.org" || !a.Expires.Equal(p.NotOnOrAfter.Add(2*time.Minute)) {
			t.Fatalf("%s: %+v", c.name, a)
		}
	}
	// Unsigned: refused, whatever is checked.
	p := idp.Defaults(entityID, acs, reqID, now)
	mustFail(t, sp, "unsigned", build(t, idp, p, false, false), reqID, "neither the response nor the assertion is signed")
	// Only the checked position counts.
	sp.ValidateResponseSignature = false
	mustFail(t, sp, "response-signed, assertion checks only", build(t, idp, p, false, true), reqID, "the assertion is not signed")
	mustOK(t, sp, build(t, idp, p, true, false), reqID)
	sp.ValidateResponseSignature, sp.ValidateAssertionSignature = true, false
	mustFail(t, sp, "assertion-signed, response checks only", build(t, idp, p, true, false), reqID, "the response is not signed")
	sp.ValidateResponseSignature = false
	mustFail(t, sp, "nothing checked", build(t, idp, p, true, true), reqID, "neither")
}

func TestSignatureVariants(t *testing.T) {
	idp := samltest.NewECDSA(t)
	sp := newSP(t, idp)
	p := idp.Defaults(entityID, acs, reqID, now)
	mustOK(t, sp, build(t, idp, p, true, true), reqID)
	// The WithComments algorithms, a PrefixList, the default namespace, no KeyInfo.
	rsaIdP := samltest.New(t)
	sp = newSP(t, rsaIdP)
	for _, o := range []samltest.SignOpts{{WithComments: true}, {PrefixList: "xs saml"}, {DSPrefix: "-"}, {DSPrefix: "dsig"}, {NoKeyInfo: true}} {
		p := rsaIdP.Defaults(entityID, acs, reqID, now)
		a := rsaIdP.Sign(t, samltest.Assertion(p), p.AssertionID, o)
		mustOK(t, sp, samltest.Response(p, a), reqID)
	}
}

func TestSHA1(t *testing.T) {
	idp := samltest.New(t)
	sp := newSP(t, idp)
	p := idp.Defaults(entityID, acs, reqID, now)
	doc := samltest.Response(p, idp.Sign(t, samltest.Assertion(p), p.AssertionID, samltest.SignOpts{SHA1: true}))
	mustFail(t, sp, "sha1", doc, reqID, "RSA-SHA1, which is refused")
	sp.AllowSHA1 = true
	mustOK(t, sp, doc, reqID)
}

func TestWrongCertificate(t *testing.T) {
	idp, other := samltest.New(t), samltest.New(t)
	sp := newSP(t, idp)
	p := other.Defaults(entityID, acs, reqID, now)
	mustFail(t, sp, "other cert", build(t, other, p, true, true), reqID, "not in the identity provider's metadata")
	a := other.Sign(t, samltest.Assertion(p), p.AssertionID, samltest.SignOpts{NoKeyInfo: true})
	mustFail(t, sp, "other key without KeyInfo", samltest.Response(p, a), reqID, "does not verify")
	// The right key, but another certificate in KeyInfo: refused by name.
	a = idp.Sign(t, samltest.Assertion(p), p.AssertionID, samltest.SignOpts{})
	a = certRE.ReplaceAllString(a, "<ds:X509Certificate>"+other.CertB64()+"</ds:X509Certificate>")
	mustFail(t, sp, "unknown KeyInfo certificate", samltest.Response(p, a), reqID, "not in the identity provider's metadata")
}

var certRE = regexp.MustCompile(`(?s)<ds:X509Certificate>.*?</ds:X509Certificate>`)

func TestTampered(t *testing.T) {
	idp := samltest.New(t)
	sp := newSP(t, idp)
	for _, c := range []struct{ signA, signR bool }{{true, false}, {false, true}, {true, true}} {
		p := idp.Defaults(entityID, acs, reqID, now)
		doc := strings.Replace(build(t, idp, p, c.signA, c.signR), ">Alice<", ">admin<", 1)
		mustFail(t, sp, "tampered NameID", doc, reqID, "does not verify")
	}
	// A comment inserted into a signed NameID does not break the digest
	// (bare-name references drop comments), and the whole name is read.
	p := idp.Defaults(entityID, acs, reqID, now)
	p.NameID = "alice@corp.example.attacker.example"
	doc := strings.Replace(build(t, idp, p, true, false), "alice@corp.example.attacker", "alice@corp.example<!---->.attacker", 1)
	if a := mustOK(t, sp, doc, reqID); a.NameID != "alice@corp.example.attacker.example" {
		t.Fatalf("NameID %q", a.NameID)
	}
}

func TestSignatureWrapping(t *testing.T) {
	idp := samltest.New(t)
	sp := newSP(t, idp)
	p := idp.Defaults(entityID, acs, reqID, now)
	signedA := idp.Sign(t, samltest.Assertion(p), p.AssertionID, samltest.SignOpts{})
	evilP := p
	evilP.AssertionID, evilP.NameID = "_evil", "admin"
	evil := samltest.Assertion(evilP)

	// The signed assertion moved under Extensions, an unsigned one beside it.
	doc := strings.Replace(samltest.Response(p, evil), "<samlp:Status>", "<samlp:Extensions>"+signedA+"</samlp:Extensions><samlp:Status>", 1)
	mustFail(t, sp, "assertion under Extensions", doc, reqID, "exactly one assertion")
	// The evil assertion wrapping the signed one (in its Advice).
	doc = samltest.Response(p, strings.Replace(evil, "<saml:Conditions", "<saml:Advice>"+signedA+"</saml:Advice><saml:Conditions", 1))
	mustFail(t, sp, "assertion in Advice", doc, reqID, "exactly one assertion")
	// Two elements with the assertion's ID.
	doc = strings.Replace(samltest.Response(p, signedA), "<samlp:Status>", `<samlp:Extensions><x ID="`+p.AssertionID+`"/></samlp:Extensions><samlp:Status>`, 1)
	mustFail(t, sp, "duplicate IDs", doc, reqID, "same ID")
	// The response's signature cut out and pasted into the assertion: it
	// references another element than the one it is in.
	signedR := idp.Sign(t, samltest.Response(p, samltest.Assertion(p)), p.ResponseID, samltest.SignOpts{})
	sigStart, sigEnd := strings.Index(signedR, "<ds:Signature"), strings.Index(signedR, "</ds:Signature>")+len("</ds:Signature>")
	sig := signedR[sigStart:sigEnd]
	moved := signedR[:sigStart] + signedR[sigEnd:]
	at := strings.Index(moved, `ID="`+p.AssertionID+`"`)
	end := at + strings.Index(moved[at:], "</saml:Issuer>") + len("</saml:Issuer>")
	moved = moved[:end] + sig + moved[end:]
	mustFail(t, sp, "reference to a different element", moved, reqID, "covers another element")
	// The response's signature, then the assertion replaced.
	doc = strings.Replace(signedR, samltest.Assertion(p), evil, 1)
	if doc == signedR {
		t.Fatal("bad fixture")
	}
	mustFail(t, sp, "assertion replaced under a response signature", doc, reqID, "does not verify")
	// A signature somewhere else.
	doc = strings.Replace(signedR, sig, "", 1)
	doc = strings.Replace(doc, "<samlp:Status>", "<samlp:Extensions>"+sig+"</samlp:Extensions><samlp:Status>", 1)
	mustFail(t, sp, "signature under Extensions", doc, reqID, "signature where none belongs")
	// Two signatures on the response.
	doc = strings.Replace(signedR, sig, sig+sig, 1)
	mustFail(t, sp, "two response signatures", doc, reqID, "signature where none belongs")
	// An Object in the signature.
	doc = strings.Replace(signedA, "</ds:SignatureValue>", "</ds:SignatureValue><ds:Object>"+evil+"</ds:Object>", 1)
	mustFail(t, sp, "object", samltest.Response(p, doc), reqID, "exactly one assertion")
	doc = strings.Replace(signedA, "</ds:SignatureValue>", "</ds:SignatureValue><ds:Object/>", 1)
	mustFail(t, sp, "object", samltest.Response(p, doc), reqID, "unsupported <Object>")
	// A second Reference smuggled into SignedInfo breaks its signature,
	// and the structure is refused anyway.
	doc = strings.Replace(signedA, "</ds:SignedInfo>", `<ds:Reference URI="#_evil"/></ds:SignedInfo>`, 1)
	mustFail(t, sp, "second reference", samltest.Response(p, doc), reqID, "")
}

func TestConditions(t *testing.T) {
	idp := samltest.New(t)
	sp := newSP(t, idp)
	for _, c := range []struct {
		name string
		mut  func(*samltest.Params)
		req  string
		want string
	}{
		{"expired", func(p *samltest.Params) { p.Now, p.NotOnOrAfter = now.Add(-time.Hour), now.Add(-10*time.Minute) }, reqID, "expired"},
		{"just within the skew", func(p *samltest.Params) { p.NotOnOrAfter = now.Add(-time.Minute) }, reqID, ""},
		{"not yet valid", func(p *samltest.Params) { p.Now, p.NotOnOrAfter = now.Add(10*time.Minute), now.Add(time.Hour) }, reqID, "future"},
		{"wrong audience", func(p *samltest.Params) { p.Audience = "https://other.example/sp" }, reqID, "audience"},
		{"wrong recipient", func(p *samltest.Params) { p.Recipient = "https://evil.example/acs" }, reqID, "recipient"},
		{"wrong destination", func(p *samltest.Params) { p.Destination = "https://evil.example/acs" }, reqID, "addressed to"},
		{"no destination", func(p *samltest.Params) { p.Destination = "" }, reqID, ""},
		{"wrong issuer", func(p *samltest.Params) { p.Issuer = "https://evil.example" }, reqID, "issuer"},
		{"InResponseTo mismatch", func(p *samltest.Params) {}, "_other", "does not answer this login's request"},
		{"IdP-initiated with a stale InResponseTo", func(p *samltest.Params) {}, "", "did not send"},
		{"IdP-initiated", func(p *samltest.Params) { p.InResponseTo = "" }, "", ""},
		{"status", func(p *samltest.Params) { p.Status = "urn:oasis:names:tc:SAML:2.0:status:Responder" }, reqID, "answered urn:oasis:names:tc:SAML:2.0:status:Responder"},
	} {
		p := idp.Defaults(entityID, acs, reqID, now)
		c.mut(&p)
		doc := build(t, idp, p, true, true)
		if c.want == "" {
			mustOK(t, sp, doc, c.req)
			continue
		}
		mustFail(t, sp, c.name, doc, c.req, c.want)
	}
}

func TestDocumentLimits(t *testing.T) {
	idp := samltest.New(t)
	sp := newSP(t, idp)
	p := idp.Defaults(entityID, acs, reqID, now)
	doc := build(t, idp, p, true, false)
	mustFail(t, sp, "DTD", `<!DOCTYPE samlp:Response [<!ENTITY x "admin">]>`+doc, reqID, "DTD")
	big := strings.Replace(doc, "</saml:Issuer>", "</saml:Issuer><!--"+strings.Repeat("x", saml.MaxSize)+"-->", 1)
	mustFail(t, sp, "oversized", big, reqID, "larger than 256 KiB")
	if _, err := sp.ParseResponse("%%%", reqID, now); err == nil {
		t.Fatal("not base64 accepted")
	}
	mustFail(t, sp, "not a response", `<x/>`, reqID, "not a SAML Response")
	// Line-wrapped base64 is fine.
	enc := samltest.Encode(doc)
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	if _, err := sp.ParseResponse(b.String(), reqID, now); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedAssertion(t *testing.T) {
	idp := samltest.New(t)
	sp := newSP(t, idp)
	keyPEM, certPEM, err := saml.GenerateKeyPair("cc.example")
	if err != nil {
		t.Fatal(err)
	}
	if sp.Key, sp.Cert, err = saml.ParseKeyPair(keyPEM, certPEM); err != nil {
		t.Fatal(err)
	}
	for _, alg := range []string{samltest.AES128GCM, samltest.AES256GCM, samltest.AES128CBC, samltest.AES256CBC} {
		for _, oaep11 := range []bool{false, true} {
			p := idp.Defaults(entityID, acs, reqID, now)
			// Signed inside the encryption (Azure's way).
			enc := samltest.Encrypt(t, idp.Sign(t, samltest.Assertion(p), p.AssertionID, samltest.SignOpts{}), sp.Cert, alg, oaep11)
			a := mustOK(t, sp, samltest.Response(p, enc), reqID)
			if a.NameID != "Alice" {
				t.Fatalf("%s: %+v", alg, a)
			}
			// Unsigned inside, the response signed around it.
			enc = samltest.Encrypt(t, samltest.Assertion(p), sp.Cert, alg, oaep11)
			mustOK(t, sp, idp.Sign(t, samltest.Response(p, enc), p.ResponseID, samltest.SignOpts{}), reqID)
			// Unsigned inside and out.
			mustFail(t, sp, "unsigned encrypted", samltest.Response(p, enc), reqID, "neither")
		}
	}
	p := idp.Defaults(entityID, acs, reqID, now)
	enc := samltest.Encrypt(t, idp.Sign(t, samltest.Assertion(p), p.AssertionID, samltest.SignOpts{}), sp.Cert, samltest.AES256GCM, false)
	// A changed ciphertext character (on the first line of the last CipherValue).
	i := strings.LastIndex(enc, "<xenc:CipherValue>") + len("<xenc:CipherValue>") + 10
	c := "A"
	if enc[i] == 'A' {
		c = "B"
	}
	bad := enc[:i] + c + enc[i+1:]
	mustFail(t, sp, "tampered ciphertext", samltest.Response(p, bad), reqID, "could not be decrypted")
	// Another SP's key.
	other, otherCert, _ := saml.GenerateKeyPair("other")
	_, oc, _ := saml.ParseKeyPair(other, otherCert)
	mustFail(t, sp, "wrong key", samltest.Response(p, samltest.Encrypt(t, samltest.Assertion(p), oc, samltest.AES256GCM, false)), reqID, "could not be decrypted")
	// No key at all.
	sp.Key = nil
	mustFail(t, sp, "no key", samltest.Response(p, enc), reqID, "could not be decrypted")
	// PKCS#1 v1.5 key transport is refused.
	sp.Key, _, _ = saml.ParseKeyPair(keyPEM, certPEM)
	v15 := strings.Replace(enc, "http://www.w3.org/2001/04/xmlenc#rsa-oaep-mgf1p", "http://www.w3.org/2001/04/xmlenc#rsa-1_5", 1)
	mustFail(t, sp, "rsa-1_5", samltest.Response(p, v15), reqID, "only RSA-OAEP")
}

func TestMetadata(t *testing.T) {
	idp := samltest.New(t)
	md, err := saml.ParseMetadata([]byte(idp.Metadata()))
	if err != nil {
		t.Fatal(err)
	}
	if md.EntityID != idp.EntityID || md.SSOURL != idp.SSOURL || len(md.Certs) != 1 || !md.Certs[0].Equal(idp.Cert) {
		t.Fatalf("%+v", md)
	}
	// Keycloak's descriptor wraps it in EntitiesDescriptor.
	wrapped := strings.Replace(idp.Metadata(), `<?xml version="1.0" encoding="UTF-8"?>`, `<md:EntitiesDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" Name="urn:keycloak">`, 1) + `</md:EntitiesDescriptor>`
	if md, err := saml.ParseMetadata([]byte(wrapped)); err != nil || md.EntityID != idp.EntityID {
		t.Fatalf("entities: %v", err)
	}
	for name, doc := range map[string]string{
		"no redirect binding": strings.Replace(idp.Metadata(), "bindings:HTTP-Redirect", "bindings:SOAP", 1),
		"no signing cert":     strings.Replace(idp.Metadata(), `use="signing"`, `use="encryption"`, 1),
		"not metadata":        `<x/>`,
		"dtd":                 `<!DOCTYPE x>` + idp.Metadata()[strings.Index(idp.Metadata(), "<md:"):],
	} {
		if _, err := saml.ParseMetadata([]byte(doc)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// The SP's own metadata.
	keyPEM, certPEM, _ := saml.GenerateKeyPair("cc.example")
	sp := newSP(t, idp)
	sp.Key, sp.Cert, _ = saml.ParseKeyPair(keyPEM, certPEM)
	sp.SignRequests = true
	out := string(sp.Metadata())
	for _, want := range []string{`entityID="` + entityID + `"`, `AuthnRequestsSigned="true"`, `use="signing"`, `use="encryption"`,
		`Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="` + acs + `"`,
		"nameid-format:unspecified", "nameid-format:persistent", "nameid-format:emailAddress", base64.StdEncoding.EncodeToString(sp.Cert.Raw)} {
		if !strings.Contains(out, want) {
			t.Errorf("SP metadata lacks %s:\n%s", want, out)
		}
	}
}

func TestAuthnRequest(t *testing.T) {
	idp := samltest.New(t)
	sp := newSP(t, idp)
	keyPEM, certPEM, _ := saml.GenerateKeyPair("cc.example")
	sp.Key, sp.Cert, _ = saml.ParseKeyPair(keyPEM, certPEM)
	for _, signed := range []bool{false, true} {
		sp.SignRequests = signed
		id, target, err := sp.AuthnRequestURL("state123", now)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(target)
		q := u.Query()
		if u.Scheme+"://"+u.Host+u.Path != idp.SSOURL || q.Get("RelayState") != "state123" || !strings.HasPrefix(id, "_") {
			t.Fatalf("%s", target)
		}
		raw, _ := base64.StdEncoding.DecodeString(q.Get("SAMLRequest"))
		xml, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`ID="` + id + `"`, `Version="2.0"`, `IssueInstant="2026-10-29T12:00:00Z"`, `Destination="` + idp.SSOURL + `"`,
			`AssertionConsumerServiceURL="` + acs + `"`, `<saml:Issuer>` + entityID + `</saml:Issuer>`, "bindings:HTTP-POST"} {
			if !strings.Contains(string(xml), want) {
				t.Fatalf("request lacks %s: %s", want, xml)
			}
		}
		if !signed {
			if q.Get("Signature") != "" {
				t.Fatal("signed")
			}
			continue
		}
		// The signature covers the query as sent, up to SigAlg.
		signedPart := target[strings.Index(target, "?")+1 : strings.Index(target, "&Signature=")]
		sig, _ := base64.StdEncoding.DecodeString(q.Get("Signature"))
		sum := sha256.Sum256([]byte(signedPart))
		if q.Get("SigAlg") != "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256" ||
			rsa.VerifyPKCS1v15(&sp.Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
			t.Fatalf("request signature does not verify: %s", target)
		}
	}
}
