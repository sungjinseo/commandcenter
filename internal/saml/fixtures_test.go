package saml_test

import (
	"strings"
	"testing"
	"time"

	"commandcenter/internal/saml/samltest"
)

// Responses shaped like real identity providers' (written by hand from the
// structure of public samples), signed here: Keycloak declares the
// assertion namespace both as a prefix and as the default, signs with the
// "dsig" prefix and pretty-prints; Entra ID (Azure AD) uses default
// namespaces throughout, a default-namespace Signature, fractional seconds
// and claim URIs as attribute names.

const keycloakResponse = `<?xml version="1.0" encoding="UTF-8"?>
<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" Destination="{ACS}" ID="ID_7b3f0c1e-91a2-4c55-8a0e-2f6a8d9c1b11" InResponseTo="{REQ}" IssueInstant="{NOW}" Version="2.0">
    <saml:Issuer>https://kc.example/realms/corp</saml:Issuer>
    <samlp:Status>
        <samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/>
    </samlp:Status>
    <saml:Assertion xmlns="urn:oasis:names:tc:SAML:2.0:assertion" ID="ID_0e5c2a77-5f1d-4b6e-9c3a-7d8e9f0a1b22" IssueInstant="{NOW}" Version="2.0">
        <saml:Issuer>https://kc.example/realms/corp</saml:Issuer>
        <saml:Subject>
            <saml:NameID Format="urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified">alice</saml:NameID>
            <saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer">
                <saml:SubjectConfirmationData InResponseTo="{REQ}" NotOnOrAfter="{LATER}" Recipient="{ACS}"/>
            </saml:SubjectConfirmation>
        </saml:Subject>
        <saml:Conditions NotBefore="{EARLIER}" NotOnOrAfter="{LATER}">
            <saml:AudienceRestriction>
                <saml:Audience>{SP}</saml:Audience>
            </saml:AudienceRestriction>
        </saml:Conditions>
        <saml:AuthnStatement AuthnInstant="{NOW}" SessionIndex="5a6b7c8d-1e2f-3a4b-5c6d-7e8f9a0b1c2d::0d1e2f3a" SessionNotOnOrAfter="{MUCHLATER}">
            <saml:AuthnContext>
                <saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:unspecified</saml:AuthnContextClassRef>
            </saml:AuthnContext>
        </saml:AuthnStatement>
        <saml:AttributeStatement>
            <saml:Attribute FriendlyName="email" Name="urn:oid:1.2.840.113549.1.9.1" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:uri">
                <saml:AttributeValue xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="xs:string">alice@corp.example</saml:AttributeValue>
            </saml:Attribute>
            <saml:Attribute Name="Role" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:basic">
                <saml:AttributeValue xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="xs:string">cc-admins</saml:AttributeValue>
            </saml:Attribute>
            <saml:Attribute Name="Role" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:basic">
                <saml:AttributeValue xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="xs:string">offline_access</saml:AttributeValue>
            </saml:Attribute>
        </saml:AttributeStatement>
    </saml:Assertion>
</samlp:Response>
`

const entraResponse = `<samlp:Response ID="_6c0f5b7e-3d2a-4e1f-9b8c-7a6d5e4f3c2b" Version="2.0" IssueInstant="{NOWFRAC}" Destination="{ACS}" InResponseTo="{REQ}" xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"><Issuer xmlns="urn:oasis:names:tc:SAML:2.0:assertion">https://sts.windows.net/8f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b/</Issuer><samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status><Assertion ID="_d71a3a8e-9fcc-45ad-8b3c-1b2c3d4e5f60" IssueInstant="{NOWFRAC}" Version="2.0" xmlns="urn:oasis:names:tc:SAML:2.0:assertion"><Issuer>https://sts.windows.net/8f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b/</Issuer><Subject><NameID Format="urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress">Alice@Contoso.example</NameID><SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><SubjectConfirmationData InResponseTo="{REQ}" NotOnOrAfter="{LATER}" Recipient="{ACS}"/></SubjectConfirmation></Subject><Conditions NotBefore="{EARLIER}" NotOnOrAfter="{LATER}"><AudienceRestriction><Audience>{SP}</Audience></AudienceRestriction></Conditions><AttributeStatement><Attribute Name="http://schemas.microsoft.com/identity/claims/tenantid"><AttributeValue>8f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b</AttributeValue></Attribute><Attribute Name="http://schemas.microsoft.com/identity/claims/objectidentifier"><AttributeValue>2b3c4d5e-6f70-4182-93a4-b5c6d7e8f901</AttributeValue></Attribute><Attribute Name="http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname"><AttributeValue>Alice</AttributeValue></Attribute><Attribute Name="http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname"><AttributeValue>Liddell</AttributeValue></Attribute><Attribute Name="http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"><AttributeValue>alice@contoso.example</AttributeValue></Attribute><Attribute Name="http://schemas.microsoft.com/ws/2008/06/identity/claims/groups"><AttributeValue>7d1c2b3a-0000-4000-8000-000000000001</AttributeValue><AttributeValue>7d1c2b3a-0000-4000-8000-000000000002</AttributeValue></Attribute></AttributeStatement><AuthnStatement AuthnInstant="{NOWFRAC}" SessionIndex="_d71a3a8e-9fcc-45ad-8b3c-1b2c3d4e5f60"><AuthnContext><AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:Password</AuthnContextClassRef></AuthnContext></AuthnStatement></Assertion></samlp:Response>`

func fill(tmpl string) string {
	return strings.NewReplacer("{ACS}", acs, "{SP}", entityID, "{REQ}", reqID, "{NOW}", samltest.Time(now),
		"{NOWFRAC}", now.Format("2006-01-02T15:04:05.123Z"), "{EARLIER}", samltest.Time(now.Add(-5*time.Minute)),
		"{LATER}", samltest.Time(now.Add(5*time.Minute)), "{MUCHLATER}", samltest.Time(now.Add(10*time.Hour))).Replace(tmpl)
}

func TestKeycloakShaped(t *testing.T) {
	idp := samltest.New(t)
	idp.EntityID = "https://kc.example/realms/corp"
	sp := newSP(t, idp)
	doc := fill(keycloakResponse)
	// Keycloak signs the document (and, when configured, the assertion).
	for _, signA := range []bool{false, true} {
		d := doc
		if signA {
			d = idp.Sign(t, d, "ID_0e5c2a77-5f1d-4b6e-9c3a-7d8e9f0a1b22", samltest.SignOpts{DSPrefix: "dsig"})
		}
		d = idp.Sign(t, d, "ID_7b3f0c1e-91a2-4c55-8a0e-2f6a8d9c1b11", samltest.SignOpts{DSPrefix: "dsig"})
		a := mustOK(t, sp, d, reqID)
		if a.NameID != "alice" || strings.Join(a.Values("email"), "") != "alice@corp.example" ||
			strings.Join(a.Values("urn:oid:1.2.840.113549.1.9.1"), "") != "alice@corp.example" || strings.Join(a.Values("Role"), ",") != "cc-admins,offline_access" {
			t.Fatalf("%+v", a)
		}
		// The pretty-printing is signed: white space added inside the
		// assertion breaks it.
		mustFail(t, sp, "whitespace", strings.Replace(d, "<saml:NameID", " <saml:NameID", 1), reqID, "does not verify")
	}
}

func TestEntraShaped(t *testing.T) {
	idp := samltest.New(t)
	idp.EntityID = "https://sts.windows.net/8f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b/"
	sp := newSP(t, idp)
	// Entra ID signs the assertion by default, with a default-namespace Signature.
	d := idp.Sign(t, fill(entraResponse), "_d71a3a8e-9fcc-45ad-8b3c-1b2c3d4e5f60", samltest.SignOpts{DSPrefix: "-"})
	if !strings.Contains(d, `<Signature xmlns="http://www.w3.org/2000/09/xmldsig#"><SignedInfo>`) {
		t.Fatal("bad fixture")
	}
	a := mustOK(t, sp, d, reqID)
	if a.NameID != "Alice@Contoso.example" || len(a.Values("http://schemas.microsoft.com/ws/2008/06/identity/claims/groups")) != 2 ||
		strings.Join(a.Values("http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname"), "") != "Alice" {
		t.Fatalf("%+v", a)
	}
	// The Response itself is unsigned: its fields are still checked.
	mustFail(t, sp, "destination", strings.Replace(d, `Destination="`+acs+`"`, `Destination="https://evil.example/acs"`, 1), reqID, "addressed to")
	// Signing the response too (Entra's "sign SAML response and assertion").
	mustOK(t, sp, idp.Sign(t, d, "_6c0f5b7e-3d2a-4e1f-9b8c-7a6d5e4f3c2b", samltest.SignOpts{DSPrefix: "-"}), reqID)
}
