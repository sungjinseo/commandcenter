package saml

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"
)

// MaxMetadataSize bounds an identity provider's metadata document.
const MaxMetadataSize = 1 << 20

const (
	bindingRedirect = "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect"
	bindingPOST     = "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"
)

// IdP is what the identity provider's metadata says.
type IdP struct {
	EntityID string
	SSOURL   string // the HTTP-Redirect SingleSignOnService
	Certs    []*x509.Certificate
	// WantAuthnRequestsSigned is the IdP's request that AuthnRequests be signed.
	WantAuthnRequestsSigned bool
}

// ParseMetadata reads an EntityDescriptor (or an EntitiesDescriptor holding
// exactly one identity provider) with an IDPSSODescriptor for SAML 2.0, its
// signing certificates and its HTTP-Redirect single sign-on URL. The
// metadata's own signature is not checked: it is pasted by an admin or
// fetched over https from the configured URL.
func ParseMetadata(b []byte) (*IdP, error) {
	if len(b) > MaxMetadataSize {
		return nil, errors.New("the metadata is larger than 1 MiB")
	}
	root, err := parse(b, nil)
	if err != nil {
		return nil, err
	}
	var eds []*node
	switch {
	case root.is(nsMD, "EntityDescriptor"):
		eds = []*node{root}
	case root.is(nsMD, "EntitiesDescriptor"):
		for _, e := range root.children(nsMD, "EntityDescriptor") {
			if len(e.children(nsMD, "IDPSSODescriptor")) > 0 {
				eds = append(eds, e)
			}
		}
		if len(eds) != 1 {
			return nil, errors.New("the metadata must describe exactly one identity provider")
		}
	default:
		return nil, errors.New("the metadata is not a SAML EntityDescriptor")
	}
	ed := eds[0]
	idp := &IdP{}
	idp.EntityID, _ = ed.attr("entityID")
	if idp.EntityID == "" {
		return nil, errors.New("the metadata has no entityID")
	}
	var desc *node
	for _, d := range ed.children(nsMD, "IDPSSODescriptor") {
		if v, _ := d.attr("protocolSupportEnumeration"); slices.Contains(strings.Fields(v), nsP) {
			desc = d
			break
		}
	}
	if desc == nil {
		return nil, errors.New("the metadata has no IDPSSODescriptor for SAML 2.0")
	}
	idp.WantAuthnRequestsSigned = boolAttr(desc, "WantAuthnRequestsSigned")
	for _, kd := range desc.children(nsMD, "KeyDescriptor") {
		if use, _ := kd.attr("use"); use != "" && use != "signing" {
			continue
		}
		var err error
		kd.walk(func(n *node) {
			if !n.is(nsDS, "X509Certificate") || err != nil {
				return
			}
			var c *x509.Certificate
			if c, err = parseCert(n.content()); err == nil &&
				!slices.ContainsFunc(idp.Certs, func(x *x509.Certificate) bool { return x.Equal(c) }) {
				idp.Certs = append(idp.Certs, c)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	if len(idp.Certs) == 0 {
		return nil, errors.New("the metadata has no signing certificate")
	}
	for _, s := range desc.children(nsMD, "SingleSignOnService") {
		if b, _ := s.attr("Binding"); b == bindingRedirect {
			idp.SSOURL, _ = s.attr("Location")
			break
		}
	}
	if u, err := url.Parse(idp.SSOURL); idp.SSOURL == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("the metadata has no HTTP-Redirect SingleSignOnService with an absolute URL")
	}
	return idp, nil
}

func boolAttr(n *node, name string) bool {
	v, _ := n.attr(name)
	return v == "true" || v == "1"
}

// parseCert reads a base64 DER certificate with a key strong enough to
// trust: RSA of at least 2048 bits or ECDSA on P-256, P-384 or P-521.
func parseCert(s string) (*x509.Certificate, error) {
	der, err := decode64(s)
	if err != nil {
		return nil, errors.New("a metadata certificate is not base64")
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("a metadata certificate does not parse: %w", err)
	}
	switch k := c.PublicKey.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, errors.New("a metadata certificate has an RSA key shorter than 2048 bits")
		}
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() && k.Curve != elliptic.P521() {
			return nil, errors.New("a metadata certificate has an unsupported elliptic curve")
		}
	default:
		return nil, errors.New("a metadata certificate has neither an RSA nor an ECDSA key")
	}
	return c, nil
}

// Fingerprint is a certificate's SHA-256 fingerprint, colon-separated hex.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	var b strings.Builder
	for i, x := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", x)
	}
	return b.String()
}

// Metadata is the service provider's metadata: its entity ID, the
// HTTP-POST assertion consumer service, the name ID formats it accepts and,
// with a key pair, the certificate for request signing (when sp signs
// requests) and for encryption.
func (sp *SP) Metadata() []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, `<md:EntityDescriptor xmlns:md="%s" xmlns:ds="%s" entityID="%s">`, nsMD, nsDS, esc(sp.EntityID))
	fmt.Fprintf(&b, `<md:SPSSODescriptor AuthnRequestsSigned="%t" WantAssertionsSigned="%t" protocolSupportEnumeration="%s">`,
		sp.SignRequests && sp.Key != nil, sp.ValidateAssertionSignature, nsP)
	if sp.Cert != nil {
		cert := base64.StdEncoding.EncodeToString(sp.Cert.Raw)
		uses := []string{"encryption"}
		if sp.SignRequests {
			uses = []string{"signing", "encryption"}
		}
		for _, use := range uses {
			fmt.Fprintf(&b, `<md:KeyDescriptor use="%s"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo>`, use, cert)
			if use == "encryption" {
				for _, alg := range []string{algAES256GCM, algAES128GCM, algAES256CBC, algAES128CBC, algRSAOAEP, algRSAOAEP11} {
					fmt.Fprintf(&b, `<md:EncryptionMethod Algorithm="%s"/>`, alg)
				}
			}
			b.WriteString(`</md:KeyDescriptor>`)
		}
	}
	for _, f := range []string{"urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified",
		"urn:oasis:names:tc:SAML:2.0:nameid-format:persistent", "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"} {
		fmt.Fprintf(&b, `<md:NameIDFormat>%s</md:NameIDFormat>`, f)
	}
	fmt.Fprintf(&b, `<md:AssertionConsumerService Binding="%s" Location="%s" index="0" isDefault="true"/>`, bindingPOST, esc(sp.ACSURL))
	b.WriteString(`</md:SPSSODescriptor></md:EntityDescriptor>` + "\n")
	return b.Bytes()
}

// esc escapes s for XML text or a double-quoted attribute.
func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// GenerateKeyPair makes a service provider key pair: RSA 2048 with a
// self-signed certificate for cn, valid for ten years. Both are PEM.
func GenerateKeyPair(cn string) (keyPEM, certPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

// ParseKeyPair reads a key pair GenerateKeyPair made.
func ParseKeyPair(keyPEM, certPEM string) (*rsa.PrivateKey, *x509.Certificate, error) {
	kb, _ := pem.Decode([]byte(keyPEM))
	cb, _ := pem.Decode([]byte(certPEM))
	if kb == nil || cb == nil {
		return nil, nil, errors.New("the service provider key pair is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("the service provider key: %w", err)
	}
	key, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("the service provider key is not RSA")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("the service provider certificate: %w", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errors.New("the service provider certificate is not the key's")
	}
	return key, cert, nil
}
