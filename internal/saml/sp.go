// Package saml is a SAML 2.0 service provider for SP-initiated web single
// sign-on: AuthnRequests over the HTTP-Redirect binding, Responses over
// HTTP-POST, verified with this package's own exclusive canonicalization
// and XML Signature code over the standard library.
//
// A Response is accepted only when a signature this SP checks covers the
// assertion it reads (the Response's or the assertion's own, made with a
// certificate from the identity provider's metadata), and only the bytes a
// signature covered are read: the verified canonical form is parsed again
// and everything else is dropped. A document must hold exactly one
// assertion, directly in the Response, and unique IDs; signatures may only
// sit on the Response and that assertion and must reference the element
// they are in. That leaves no room for signature wrapping.
package saml

import (
	"bytes"
	"compress/flate"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// MaxSize bounds a Response document.
const MaxSize = 256 << 10

const (
	statusSuccess = "urn:oasis:names:tc:SAML:2.0:status:Success"
	cmBearer      = "urn:oasis:names:tc:SAML:2.0:cm:bearer"
	formatEntity  = "urn:oasis:names:tc:SAML:2.0:nameid-format:entity"
)

// SP is the service provider under one configuration.
type SP struct {
	EntityID string // the audience responses must name
	ACSURL   string // where responses are posted
	IdP      *IdP

	// At least one must be set; a Response passes when every signature on
	// a checked position verifies and at least one such signature exists.
	ValidateResponseSignature  bool
	ValidateAssertionSignature bool
	AllowSHA1                  bool
	ClockSkew                  time.Duration

	Key          *rsa.PrivateKey   // decrypts assertions and signs requests; nil for none
	Cert         *x509.Certificate // Key's certificate, published in the metadata
	SignRequests bool
}

// Attribute is one assertion attribute.
type Attribute struct {
	Name, FriendlyName string
	Values             []string
}

// Assertion is what a verified Response says about the user.
type Assertion struct {
	ID           string
	NameID       string
	NameIDFormat string
	Attributes   []Attribute
	// Expires is when the assertion stops being acceptable: keep its ID
	// until then to refuse a replay.
	Expires time.Time
}

// Values returns the values of the attributes whose Name or FriendlyName
// is name.
func (a *Assertion) Values(name string) []string {
	var out []string
	for _, at := range a.Attributes {
		if at.Name == name || (at.FriendlyName != "" && at.FriendlyName == name) {
			out = append(out, at.Values...)
		}
	}
	return out
}

// AuthnRequestURL returns a new request's ID and the URL that carries it to
// the identity provider over the HTTP-Redirect binding, with relayState.
// With SignRequests the query is signed with RSA-SHA256.
func (sp *SP) AuthnRequestURL(relayState string, now time.Time) (id, target string, err error) {
	b := make([]byte, 20)
	rand.Read(b)
	id = "_" + hex.EncodeToString(b)
	req := fmt.Sprintf(`<samlp:AuthnRequest xmlns:samlp="%s" xmlns:saml="%s" ID="%s" Version="2.0" IssueInstant="%s" Destination="%s" AssertionConsumerServiceURL="%s" ProtocolBinding="%s"><saml:Issuer>%s</saml:Issuer><samlp:NameIDPolicy AllowCreate="true"/></samlp:AuthnRequest>`,
		nsP, nsA, id, now.UTC().Format("2006-01-02T15:04:05Z"), esc(sp.IdP.SSOURL), esc(sp.ACSURL), bindingPOST, esc(sp.EntityID))
	var z bytes.Buffer
	w, _ := flate.NewWriter(&z, flate.BestCompression)
	w.Write([]byte(req))
	w.Close()
	q := "SAMLRequest=" + url.QueryEscape(base64.StdEncoding.EncodeToString(z.Bytes()))
	if relayState != "" {
		q += "&RelayState=" + url.QueryEscape(relayState)
	}
	if sp.SignRequests {
		if sp.Key == nil {
			return "", "", errors.New("request signing is on, but there is no service provider key")
		}
		q += "&SigAlg=" + url.QueryEscape(algRSASHA256)
		sum := sha256.Sum256([]byte(q))
		sig, err := rsa.SignPKCS1v15(nil, sp.Key, crypto.SHA256, sum[:])
		if err != nil {
			return "", "", err
		}
		q += "&Signature=" + url.QueryEscape(base64.StdEncoding.EncodeToString(sig))
	}
	sep := "?"
	if strings.Contains(sp.IdP.SSOURL, "?") {
		sep = "&"
	}
	return id, sp.IdP.SSOURL + sep + q, nil
}

// ParseResponse verifies the base64 SAMLResponse form value and returns its
// assertion. requestID is the ID of the AuthnRequest this login sent; ""
// accepts only a response to no request (IdP-initiated), which the caller
// must have allowed. now is checked against the validity windows with
// sp.ClockSkew of tolerance. A replay is the caller's to refuse
// (Assertion.Expires says how long to remember the ID).
func (sp *SP) ParseResponse(encoded, requestID string, now time.Time) (*Assertion, error) {
	if !sp.ValidateResponseSignature && !sp.ValidateAssertionSignature {
		return nil, errors.New("neither response nor assertion signatures are checked")
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(MaxSize)+MaxSize/32 { // room for line breaks
		return nil, errors.New("the response is larger than 256 KiB")
	}
	raw, err := decode64(encoded)
	if err != nil {
		return nil, errors.New("the response is not base64")
	}
	if len(raw) > MaxSize {
		return nil, errors.New("the response is larger than 256 KiB")
	}
	doc, err := parse(raw, nil)
	if err != nil {
		return nil, err
	}
	if !doc.is(nsP, "Response") {
		return nil, errors.New("the document is not a SAML Response")
	}
	// The status first: a failed login carries no assertion.
	if err := checkStatus(doc); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	if err := uniqueIDs(doc, ids); err != nil {
		return nil, err
	}
	var asrts, sigs []*node
	doc.walk(func(n *node) {
		switch {
		case n.is(nsA, "Assertion") || n.is(nsA, "EncryptedAssertion"):
			asrts = append(asrts, n)
		case n.is(nsDS, "Signature"):
			sigs = append(sigs, n)
		}
	})
	if len(asrts) != 1 || asrts[0].parent != doc {
		return nil, errors.New("the response must hold exactly one assertion, directly in the Response")
	}
	orig := asrts[0]
	var respSig, asrtSig *node
	for _, s := range sigs {
		switch {
		case s.parent == doc && respSig == nil:
			respSig = s
		case s.parent == orig && asrtSig == nil && orig.local == "Assertion":
			asrtSig = s
		default:
			return nil, errors.New("the response has a signature where none belongs")
		}
	}

	v := verifier{certs: sp.IdP.Certs, allowSHA1: sp.AllowSHA1}
	resp, respOK := doc, false
	if respSig != nil && sp.ValidateResponseSignature {
		b, err := v.verify(doc, respSig)
		if err != nil {
			return nil, fmt.Errorf("the response signature: %w", err)
		}
		if resp, err = parse(b, nil); err != nil {
			return nil, err
		}
		respOK = true
	}
	// The assertion to read: from the verified response when there is one.
	cur := orig
	if respOK {
		c := append(resp.children(nsA, "Assertion"), resp.children(nsA, "EncryptedAssertion")...)
		if len(c) != 1 {
			return nil, errSignature
		}
		cur = c[0]
	}
	asrtOK := false
	if cur.local == "EncryptedAssertion" {
		plain, err := decrypt(cur, sp.Key)
		if err != nil {
			return nil, err
		}
		if len(plain) > MaxSize {
			return nil, errDecrypt
		}
		if cur, err = parse(plain, cur.scope()); err != nil || !cur.is(nsA, "Assertion") {
			return nil, errDecrypt
		}
		if err := uniqueIDs(cur, ids); err != nil {
			return nil, err
		}
		var sig *node
		var bad bool
		cur.walk(func(n *node) {
			if n.is(nsDS, "Signature") {
				bad = bad || n.parent != cur || sig != nil
				sig = n
			}
			bad = bad || (n != cur && (n.is(nsA, "Assertion") || n.is(nsA, "EncryptedAssertion")))
		})
		if bad {
			return nil, errors.New("the decrypted assertion has a nested assertion or a misplaced signature")
		}
		if sig != nil && sp.ValidateAssertionSignature {
			b, err := v.verify(cur, sig)
			if err != nil {
				return nil, fmt.Errorf("the assertion signature: %w", err)
			}
			if cur, err = parse(b, nil); err != nil {
				return nil, err
			}
			asrtOK = true
		}
	} else if asrtSig != nil && sp.ValidateAssertionSignature {
		// Verified in the document as received, so the namespaces in scope
		// are the ones the identity provider signed with.
		b, err := v.verify(orig, asrtSig)
		if err != nil {
			return nil, fmt.Errorf("the assertion signature: %w", err)
		}
		if cur, err = parse(b, nil); err != nil {
			return nil, err
		}
		asrtOK = true
	}
	if !respOK && !asrtOK {
		return nil, errors.New(sp.unsignedMessage())
	}
	if err := sp.checkResponse(resp, requestID, now); err != nil {
		return nil, err
	}
	return sp.checkAssertion(cur, requestID, now)
}

func (sp *SP) unsignedMessage() string {
	switch {
	case !sp.ValidateAssertionSignature:
		return "the response is not signed (only Response signatures are checked)"
	case !sp.ValidateResponseSignature:
		return "the assertion is not signed (only assertion signatures are checked)"
	}
	return "neither the response nor the assertion is signed"
}

// uniqueIDs refuses a document in which two elements share an ID; ids
// collects them across documents (a response and its decrypted assertion).
func uniqueIDs(root *node, ids map[string]bool) error {
	var err error
	root.walk(func(n *node) {
		for _, a := range n.attrs {
			if (a.space == "" && (a.local == "ID" || a.local == "Id" || a.local == "id")) || (a.space == nsXML && a.local == "id") {
				if ids[a.value] && err == nil {
					err = errors.New("the response has two elements with the same ID")
				}
				ids[a.value] = true
			}
		}
	})
	return err
}

func checkStatus(resp *node) error {
	st, err := resp.child(nsP, "Status")
	if err != nil {
		return err
	}
	sc, err := st.child(nsP, "StatusCode")
	if err != nil {
		return err
	}
	if v, _ := sc.attr("Value"); v != statusSuccess {
		msg := "the identity provider answered " + trunc(v, 128)
		for _, sub := range sc.children(nsP, "StatusCode") {
			s, _ := sub.attr("Value")
			msg += " / " + trunc(s, 128)
		}
		for _, m := range st.children(nsP, "StatusMessage") {
			msg += ": " + trunc(strings.TrimSpace(m.content()), 200)
		}
		return errors.New(msg)
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (sp *SP) checkResponse(resp *node, requestID string, now time.Time) error {
	if v, _ := resp.attr("Version"); v != "2.0" {
		return errors.New("the response is not SAML 2.0")
	}
	if d, ok := resp.attr("Destination"); ok && d != sp.ACSURL {
		return fmt.Errorf("the response is addressed to %q, not to this server's ACS URL", trunc(d, 200))
	}
	irt, ok := resp.attr("InResponseTo")
	switch {
	case requestID == "" && ok:
		return errors.New("the response answers a request this login did not send")
	case requestID != "" && irt != requestID:
		return errors.New("the response does not answer this login's request")
	}
	if err := sp.checkIssuer(resp, false); err != nil {
		return err
	}
	if err := sp.notFuture(resp, "response", now); err != nil {
		return err
	}
	return checkStatus(resp)
}

// checkIssuer checks n's Issuer, which must be there unless optional.
func (sp *SP) checkIssuer(n *node, required bool) error {
	is := n.children(nsA, "Issuer")
	switch {
	case len(is) > 1:
		return errors.New("more than one Issuer")
	case len(is) == 0 && required:
		return errors.New("the assertion has no Issuer")
	case len(is) == 0:
		return nil
	}
	if f, ok := is[0].attr("Format"); ok && f != formatEntity {
		return errors.New("the Issuer is not an entity ID")
	}
	if got := strings.TrimSpace(is[0].content()); got != sp.IdP.EntityID {
		return fmt.Errorf("the issuer %q is not the identity provider %q", trunc(got, 200), sp.IdP.EntityID)
	}
	return nil
}

// notFuture refuses an IssueInstant later than now plus the skew.
func (sp *SP) notFuture(n *node, what string, now time.Time) error {
	v, _ := n.attr("IssueInstant")
	t, err := parseTime(v)
	if err != nil {
		return fmt.Errorf("the %s's IssueInstant: %w", what, err)
	}
	if t.After(now.Add(sp.ClockSkew)) {
		return fmt.Errorf("the %s is issued in the future", what)
	}
	return nil
}

func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, errors.New("missing")
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a UTC dateTime", trunc(v, 64))
	}
	return t, nil
}

// checkAssertion checks the assertion's issuer, subject confirmation,
// conditions and authentication statement and returns what it says.
func (sp *SP) checkAssertion(a *node, requestID string, now time.Time) (*Assertion, error) {
	out := &Assertion{}
	out.ID, _ = a.attr("ID")
	if v, _ := a.attr("Version"); v != "2.0" || out.ID == "" {
		return nil, errors.New("the assertion is not SAML 2.0 or has no ID")
	}
	if err := sp.checkIssuer(a, true); err != nil {
		return nil, err
	}
	if err := sp.notFuture(a, "assertion", now); err != nil {
		return nil, err
	}
	skew := sp.ClockSkew
	// notYet reports whether n's optional NotBefore-like attribute is still ahead.
	notYet := func(n *node, name string) (bool, error) {
		v, ok := n.attr(name)
		if !ok {
			return false, nil
		}
		t, err := parseTime(v)
		if err != nil {
			return false, fmt.Errorf("%s: %w", name, err)
		}
		return now.Add(skew).Before(t), nil
	}
	// expired reads the NotOnOrAfter of n and reports whether it passed.
	expired := func(n *node, name string, required bool) (time.Time, bool, error) {
		v, ok := n.attr(name)
		if !ok {
			if required {
				return time.Time{}, false, fmt.Errorf("%s is missing", name)
			}
			return time.Time{}, false, nil
		}
		t, err := parseTime(v)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("%s: %w", name, err)
		}
		return t, !now.Add(-skew).Before(t), nil
	}

	subj, err := a.child(nsA, "Subject")
	if err != nil {
		return nil, err
	}
	if len(subj.children(nsA, "EncryptedID")) > 0 || len(subj.children(nsA, "BaseID")) > 0 {
		return nil, errors.New("the subject's identifier is encrypted or not a NameID, which is not supported")
	}
	nid, err := subj.child(nsA, "NameID")
	if err != nil {
		return nil, err
	}
	out.NameID = strings.TrimSpace(nid.content())
	out.NameIDFormat, _ = nid.attr("Format")

	// A bearer confirmation for this ACS URL, this request and now.
	var until time.Time
	why := errors.New("the assertion has no bearer subject confirmation")
	for _, sc := range subj.children(nsA, "SubjectConfirmation") {
		if m, _ := sc.attr("Method"); m != cmBearer {
			continue
		}
		d, err := sc.child(nsA, "SubjectConfirmationData")
		if err != nil {
			why = err
			continue
		}
		irt, hasIRT := d.attr("InResponseTo")
		exp, gone, err := expired(d, "NotOnOrAfter", true)
		early, err2 := notYet(d, "NotBefore")
		switch r, _ := d.attr("Recipient"); {
		case err != nil:
			why = fmt.Errorf("the subject confirmation: %w", err)
		case err2 != nil:
			why = fmt.Errorf("the subject confirmation: %w", err2)
		case r != sp.ACSURL:
			why = fmt.Errorf("the subject confirmation's recipient %q is not this server's ACS URL", trunc(r, 200))
		case gone:
			why = errors.New("the subject confirmation has expired")
		case early:
			why = errors.New("the subject confirmation is not valid yet")
		case requestID == "" && hasIRT:
			why = errors.New("the subject confirmation answers a request this login did not send")
		case requestID != "" && irt != requestID:
			why = errors.New("the subject confirmation does not answer this login's request")
		default:
			why, until = nil, exp
		}
		if why == nil {
			break
		}
	}
	if why != nil {
		return nil, why
	}

	conds := a.children(nsA, "Conditions")
	if len(conds) != 1 {
		return nil, errors.New("the assertion must have exactly one Conditions")
	}
	c := conds[0]
	early, err := notYet(c, "NotBefore")
	if err != nil {
		return nil, fmt.Errorf("the assertion's conditions: %w", err)
	}
	if early {
		return nil, errors.New("the assertion is not valid yet")
	}
	exp, gone, err := expired(c, "NotOnOrAfter", false)
	if err != nil {
		return nil, fmt.Errorf("the assertion's conditions: %w", err)
	}
	if gone {
		return nil, errors.New("the assertion has expired")
	}
	if !exp.IsZero() && exp.Before(until) {
		until = exp
	}
	ars := c.children(nsA, "AudienceRestriction")
	if len(ars) == 0 {
		return nil, errors.New("the assertion has no audience restriction")
	}
	for _, ar := range ars { // each restriction must admit this SP
		ok := false
		for _, au := range ar.children(nsA, "Audience") {
			ok = ok || strings.TrimSpace(au.content()) == sp.EntityID
		}
		if !ok {
			return nil, fmt.Errorf("the assertion's audience is not this server (%s)", sp.EntityID)
		}
	}

	stmts := a.children(nsA, "AuthnStatement")
	if len(stmts) == 0 {
		return nil, errors.New("the assertion has no authentication statement")
	}
	for _, s := range stmts {
		if _, gone, err := expired(s, "SessionNotOnOrAfter", false); err != nil {
			return nil, fmt.Errorf("the authentication statement: %w", err)
		} else if gone {
			return nil, errors.New("the authentication session has ended")
		}
	}
	for _, st := range a.children(nsA, "AttributeStatement") {
		for _, at := range st.children(nsA, "Attribute") {
			x := Attribute{}
			x.Name, _ = at.attr("Name")
			x.FriendlyName, _ = at.attr("FriendlyName")
			for _, v := range at.children(nsA, "AttributeValue") {
				x.Values = append(x.Values, strings.TrimSpace(v.content()))
			}
			out.Attributes = append(out.Attributes, x)
		}
	}
	out.Expires = until.Add(skew)
	return out, nil
}
