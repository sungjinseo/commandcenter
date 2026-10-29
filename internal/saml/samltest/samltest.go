// Package samltest is a test identity provider: a key pair, its metadata,
// and builders for signed and encrypted SAML Responses. The signatures are
// made over this repository's own canonicalization (saml.Canonicalize), so
// they test the verifier's structure checks, not interoperability; the
// canonicalization itself is tested against known outputs.
package samltest

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/saml"
)

// IdP is a test identity provider.
type IdP struct {
	EntityID string
	SSOURL   string
	Key      crypto.Signer
	Cert     *x509.Certificate
}

// New returns an identity provider with an RSA-2048 key.
func New(t testing.TB) *IdP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return withKey(t, k)
}

// NewECDSA returns an identity provider with a P-256 key.
func NewECDSA(t testing.TB) *IdP {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return withKey(t, k)
}

func withKey(t testing.TB, k crypto.Signer) *IdP {
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &IdP{EntityID: "https://idp.example/realms/corp", SSOURL: "https://idp.example/sso", Key: k, Cert: c}
}

// CertB64 is the certificate as metadata and KeyInfo carry it.
func (i *IdP) CertB64() string { return base64.StdEncoding.EncodeToString(i.Cert.Raw) }

// Metadata is the identity provider's metadata.
func (i *IdP) Metadata() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" entityID="` + i.EntityID + `">
  <md:IDPSSODescriptor WantAuthnRequestsSigned="false" protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + wrap(i.CertB64()) + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>
    <md:NameIDFormat>urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified</md:NameIDFormat>
    <md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="` + i.SSOURL + `/post"/>
    <md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="` + i.SSOURL + `"/>
  </md:IDPSSODescriptor>
</md:EntityDescriptor>
`
}

// wrap breaks base64 into 64-column lines, as many identity providers do.
func wrap(s string) string {
	var b strings.Builder
	for len(s) > 64 {
		b.WriteString(s[:64] + "\n")
		s = s[64:]
	}
	b.WriteString(s)
	return b.String()
}

// SignOpts shape a signature.
type SignOpts struct {
	SHA1         bool   // RSA-SHA1 and a SHA-1 digest
	WithComments bool   // the WithComments canonicalization algorithms
	PrefixList   string // InclusiveNamespaces of the Reference's canonicalization
	NoKeyInfo    bool
	DSPrefix     string // the XML Signature prefix; "ds" when empty, "-" for the default namespace
}

const (
	nsDS    = "http://www.w3.org/2000/09/xmldsig#"
	excC14N = "http://www.w3.org/2001/10/xml-exc-c14n#"
)

var issuerEnd = regexp.MustCompile(`</([A-Za-z0-9_.-]+:)?Issuer>`)

// Sign returns doc with an enveloped signature on the element whose ID is
// id, placed right after that element's Issuer.
func (i *IdP) Sign(t testing.TB, doc, id string, o SignOpts) string {
	t.Helper()
	at := strings.Index(doc, `ID="`+id+`"`)
	if at < 0 {
		t.Fatalf("no element with ID %s", id)
	}
	loc := issuerEnd.FindStringIndex(doc[at:])
	if loc == nil {
		t.Fatalf("element %s has no Issuer", id)
	}
	pos := at + loc[1]

	c14n := excC14N
	if o.WithComments {
		c14n += "WithComments"
	}
	sigAlg, digAlg := "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256", "http://www.w3.org/2001/04/xmlenc#sha256"
	if _, ok := i.Key.(*ecdsa.PrivateKey); ok {
		sigAlg = "http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha256"
	}
	if o.SHA1 {
		sigAlg, digAlg = "http://www.w3.org/2000/09/xmldsig#rsa-sha1", "http://www.w3.org/2000/09/xmldsig#sha1"
	}
	canon, err := saml.Canonicalize([]byte(doc), id, strings.Fields(o.PrefixList))
	if err != nil {
		t.Fatal(err)
	}
	var digest []byte
	if o.SHA1 {
		s := sha1.Sum(canon)
		digest = s[:]
	} else {
		s := sha256.Sum256(canon)
		digest = s[:]
	}
	p, decl := "ds:", ` xmlns:ds="`+nsDS+`"`
	switch o.DSPrefix {
	case "-":
		p, decl = "", ` xmlns="`+nsDS+`"`
	case "":
	default:
		p, decl = o.DSPrefix+":", ` xmlns:`+o.DSPrefix+`="`+nsDS+`"`
	}
	incl := ""
	if o.PrefixList != "" {
		incl = `<ec:InclusiveNamespaces xmlns:ec="` + excC14N + `" PrefixList="` + o.PrefixList + `"/>`
	}
	keyInfo := ""
	if !o.NoKeyInfo {
		keyInfo = fmt.Sprintf(`<%sKeyInfo><%sX509Data><%sX509Certificate>%s</%sX509Certificate></%sX509Data></%sKeyInfo>`, p, p, p, wrap(i.CertB64()), p, p, p)
	}
	sig := fmt.Sprintf(`<%sSignature%s><%sSignedInfo><%sCanonicalizationMethod Algorithm="%s"/><%sSignatureMethod Algorithm="%s"/>`+
		`<%sReference URI="#%s"><%sTransforms><%sTransform Algorithm="http://www.w3.org/2000/09/xmldsig#enveloped-signature"/>`+
		`<%sTransform Algorithm="%s">%s</%sTransform></%sTransforms><%sDigestMethod Algorithm="%s"/><%sDigestValue>%s</%sDigestValue>`+
		`</%sReference></%sSignedInfo><%sSignatureValue>SIGNATURE</%sSignatureValue>%s</%sSignature>`,
		p, decl, p, p, c14n, p, sigAlg, p, id, p, p, p, c14n, incl, p, p, p, digAlg, p, base64.StdEncoding.EncodeToString(digest), p,
		p, p, p, p, keyInfo, p)
	signed := doc[:pos] + sig + doc[pos:]
	si, err := saml.CanonicalizeSignedInfo([]byte(signed), id)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(signed, "SIGNATURE", wrap(base64.StdEncoding.EncodeToString(i.sign(t, si, o.SHA1))), 1)
}

func (i *IdP) sign(t testing.TB, data []byte, useSHA1 bool) []byte {
	h, sum := crypto.SHA256, sha256.Sum256(data)
	digest := sum[:]
	if useSHA1 {
		s := sha1.Sum(data)
		h, digest = crypto.SHA1, s[:]
	}
	switch k := i.Key.(type) {
	case *rsa.PrivateKey:
		sig, err := rsa.SignPKCS1v15(rand.Reader, k, h, digest)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, digest)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]byte, 64)
		r.FillBytes(out[:32])
		s.FillBytes(out[32:])
		return out
	}
	t.Fatal("unknown key")
	return nil
}

// Params describe a Response.
type Params struct {
	ResponseID, AssertionID string
	Issuer                  string // both Issuers
	Destination             string // the Response's; "" leaves it out
	InResponseTo            string // the Response's and the confirmation's; "" leaves them out
	Recipient               string
	Audience                string
	NameID                  string
	Now                     time.Time // IssueInstant; NotBefore is a minute earlier
	NotOnOrAfter            time.Time // the conditions' and the confirmation's
	Status                  string
	Attributes              [][2]string // name, value (repeat a name for several values)
}

// Defaults are a Response for sp's login request reqID at now, for alice
// in group cc-admins.
func (i *IdP) Defaults(entityID, acs, reqID string, now time.Time) Params {
	return Params{ResponseID: "_r" + rnd(), AssertionID: "_a" + rnd(), Issuer: i.EntityID, Destination: acs, InResponseTo: reqID,
		Recipient: acs, Audience: entityID, NameID: "Alice", Now: now, NotOnOrAfter: now.Add(5 * time.Minute),
		Status:     "urn:oasis:names:tc:SAML:2.0:status:Success",
		Attributes: [][2]string{{"email", "alice@example.org"}, {"groups", "cc-admins"}, {"groups", "staff"}, {"givenName", "Alice"}, {"sn", "Liddell"}}}
}

func rnd() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Time formats t as SAML does.
func Time(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// Assertion is p's assertion, unsigned.
func Assertion(p Params) string {
	irt := ""
	if p.InResponseTo != "" {
		irt = ` InResponseTo="` + p.InResponseTo + `"`
	}
	var attrs strings.Builder
	for _, a := range p.Attributes {
		fmt.Fprintf(&attrs, `<saml:Attribute Name="%s" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:basic"><saml:AttributeValue xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="xs:string">%s</saml:AttributeValue></saml:Attribute>`, a[0], a[1])
	}
	return fmt.Sprintf(`<saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="%s" Version="2.0" IssueInstant="%s">`+
		`<saml:Issuer>%s</saml:Issuer>`+
		`<saml:Subject><saml:NameID Format="urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified">%s</saml:NameID>`+
		`<saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData%s NotOnOrAfter="%s" Recipient="%s"/></saml:SubjectConfirmation></saml:Subject>`+
		`<saml:Conditions NotBefore="%s" NotOnOrAfter="%s"><saml:AudienceRestriction><saml:Audience>%s</saml:Audience></saml:AudienceRestriction></saml:Conditions>`+
		`<saml:AuthnStatement AuthnInstant="%s" SessionIndex="_s1"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>`+
		`<saml:AttributeStatement>%s</saml:AttributeStatement></saml:Assertion>`,
		p.AssertionID, Time(p.Now), p.Issuer, p.NameID, irt, Time(p.NotOnOrAfter), p.Recipient,
		Time(p.Now.Add(-time.Minute)), Time(p.NotOnOrAfter), p.Audience, Time(p.Now), attrs.String())
}

// Response wraps assertion (signed, encrypted or not) in p's Response.
func Response(p Params, assertion string) string {
	extra := ""
	if p.Destination != "" {
		extra += ` Destination="` + p.Destination + `"`
	}
	if p.InResponseTo != "" {
		extra += ` InResponseTo="` + p.InResponseTo + `"`
	}
	return fmt.Sprintf(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="%s" Version="2.0" IssueInstant="%s"%s>`+
		`<saml:Issuer>%s</saml:Issuer><samlp:Status><samlp:StatusCode Value="%s"/></samlp:Status>%s</samlp:Response>`,
		p.ResponseID, Time(p.Now), extra, p.Issuer, p.Status, assertion)
}

// Encode is the SAMLResponse form value of doc.
func Encode(doc string) string { return base64.StdEncoding.EncodeToString([]byte(doc)) }

// Encryption algorithms for Encrypt.
const (
	AES128CBC = "http://www.w3.org/2001/04/xmlenc#aes128-cbc"
	AES256CBC = "http://www.w3.org/2001/04/xmlenc#aes256-cbc"
	AES128GCM = "http://www.w3.org/2009/xmlenc11#aes128-gcm"
	AES256GCM = "http://www.w3.org/2009/xmlenc11#aes256-gcm"
)

// Encrypt returns a saml:EncryptedAssertion holding assertion, encrypted
// with alg under a key transported with RSA-OAEP (MGF1-SHA1, SHA-256
// digest; MGF1-SHA-256 in the XML Encryption 1.1 form when oaep11) to cert's key.
func Encrypt(t testing.TB, assertion string, cert *x509.Certificate, alg string, oaep11 bool) string {
	t.Helper()
	keyLen := map[string]int{AES128CBC: 16, AES256CBC: 32, AES128GCM: 16, AES256GCM: 32}[alg]
	cek := make([]byte, keyLen)
	rand.Read(cek)
	c, _ := aes.NewCipher(cek)
	var data []byte
	if strings.HasSuffix(alg, "gcm") {
		g, _ := cipher.NewGCM(c)
		nonce := make([]byte, g.NonceSize())
		rand.Read(nonce)
		data = g.Seal(nonce, nonce, []byte(assertion), nil)
	} else {
		pad := aes.BlockSize - len(assertion)%aes.BlockSize
		plain := []byte(assertion + strings.Repeat("\x00", pad-1) + string(rune(pad)))
		data = make([]byte, aes.BlockSize+len(plain))
		rand.Read(data[:aes.BlockSize])
		cipher.NewCBCEncrypter(c, data[:aes.BlockSize]).CryptBlocks(data[aes.BlockSize:], plain)
	}
	pub := cert.PublicKey.(*rsa.PublicKey)
	opts := &rsa.OAEPOptions{Hash: crypto.SHA256, MGFHash: crypto.SHA1} // mgf1p: MGF1 is SHA-1
	method := `<xenc:EncryptionMethod Algorithm="http://www.w3.org/2001/04/xmlenc#rsa-oaep-mgf1p"><ds:DigestMethod Algorithm="http://www.w3.org/2001/04/xmlenc#sha256"/></xenc:EncryptionMethod>`
	if oaep11 {
		opts.MGFHash = crypto.SHA256
		method = `<xenc:EncryptionMethod Algorithm="http://www.w3.org/2009/xmlenc11#rsa-oaep"><ds:DigestMethod Algorithm="http://www.w3.org/2001/04/xmlenc#sha256"/><xenc11:MGF xmlns:xenc11="http://www.w3.org/2009/xmlenc11#" Algorithm="http://www.w3.org/2009/xmlenc11#mgf1sha256"/></xenc:EncryptionMethod>`
	}
	wrapped, err := rsa.EncryptOAEPWithOptions(rand.Reader, pub, cek, opts)
	if err != nil {
		t.Fatal(err)
	}
	return `<saml:EncryptedAssertion><xenc:EncryptedData xmlns:xenc="http://www.w3.org/2001/04/xmlenc#" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" Type="http://www.w3.org/2001/04/xmlenc#Element">` +
		`<xenc:EncryptionMethod Algorithm="` + alg + `"/><ds:KeyInfo><xenc:EncryptedKey>` + method +
		`<xenc:CipherData><xenc:CipherValue>` + wrap(base64.StdEncoding.EncodeToString(wrapped)) + `</xenc:CipherValue></xenc:CipherData></xenc:EncryptedKey></ds:KeyInfo>` +
		`<xenc:CipherData><xenc:CipherValue>` + wrap(base64.StdEncoding.EncodeToString(data)) + `</xenc:CipherValue></xenc:CipherData></xenc:EncryptedData></saml:EncryptedAssertion>`
}
