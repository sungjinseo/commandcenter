// Package ldap is a minimal LDAPv3 client (plan 12 Task 6) written against
// the standard library only: simple bind, search, unbind and StartTLS. Each
// login or check opens one short-lived connection; nothing is pooled. The
// server is untrusted input: every read is bounded by a deadline and by
// ber.MaxLen.
package ldap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"commandcenter/internal/ldap/ber"
)

// Protocol operation identifiers (RFC 4511 4.2-4.12).
const (
	opBindRequest     byte = ber.ClassApplication | ber.Constructed | 0
	opBindResponse    byte = ber.ClassApplication | ber.Constructed | 1
	opUnbindRequest   byte = ber.ClassApplication | 2
	opSearchRequest   byte = ber.ClassApplication | ber.Constructed | 3
	opSearchEntry     byte = ber.ClassApplication | ber.Constructed | 4
	opSearchDone      byte = ber.ClassApplication | ber.Constructed | 5
	opSearchReference byte = ber.ClassApplication | ber.Constructed | 19
	opExtendedRequest byte = ber.ClassApplication | ber.Constructed | 23
	opExtendedResp    byte = ber.ClassApplication | ber.Constructed | 24

	authSimple byte = ber.ClassContext | 0
	extName    byte = ber.ClassContext | 0
)

// StartTLSOID names the StartTLS extended operation.
const StartTLSOID = "1.3.6.1.4.1.1466.20037"

// Result codes used here.
const (
	ResultSuccess             = 0
	ResultSizeLimitExceeded   = 4
	ResultInvalidCredentials  = 49
	ResultUnwillingToPerform  = 53
	maxEntries                = 2 // the search size limit: two is enough to detect ambiguity
	defaultTimeout            = 5 * time.Second
	scopeWholeSubtree         = 2
	derefNever                = 0
	searchTimeLimitSeconds    = 10
	maxMessagesPerSearchReply = 64 // entries + references + done; a server may not stream forever
)

// ErrInvalidCredentials is a bind refused with invalidCredentials (49).
var ErrInvalidCredentials = errors.New("ldap: invalid credentials")

// ResultError is a non-success LDAP result.
type ResultError struct {
	Op         string
	Code       int64
	Diagnostic string
}

func (e *ResultError) Error() string {
	msg := fmt.Sprintf("ldap %s failed: result code %d", e.Op, e.Code)
	if e.Diagnostic != "" {
		msg += " (" + truncate(e.Diagnostic, 200) + ")"
	}
	return msg
}

func (e *ResultError) Is(target error) bool {
	return target == ErrInvalidCredentials && e.Code == ResultInvalidCredentials
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Entry is one search result. Attribute names are lower-cased.
type Entry struct {
	DN    string
	Attrs map[string][]string
}

// First returns the first value of attr, or "".
func (e Entry) First(attr string) string {
	if v := e.Attrs[strings.ToLower(attr)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// DialOptions describe how to reach the server.
type DialOptions struct {
	URL                string // ldap://host[:389] or ldaps://host[:636]
	StartTLS           bool   // only with ldap://
	InsecureSkipVerify bool
	Timeout            time.Duration // per operation; 0 means 5s
	RootCAs            *x509.CertPool
}

// Conn is one LDAP connection. It is not safe for concurrent use.
type Conn struct {
	nc      net.Conn
	r       *bufio.Reader
	id      int64
	timeout time.Duration
	host    string
	stop    func() bool
}

// parseURL returns the scheme, host name and host:port of an LDAP URL.
func parseURL(raw string) (scheme, host, addr string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", "", fmt.Errorf("ldap: invalid URL")
	}
	scheme = strings.ToLower(u.Scheme)
	port := u.Port()
	switch scheme {
	case "ldap":
		if port == "" {
			port = "389"
		}
	case "ldaps":
		if port == "" {
			port = "636"
		}
	default:
		return "", "", "", fmt.Errorf("ldap: URL scheme must be ldap or ldaps")
	}
	host = u.Hostname()
	return scheme, host, net.JoinHostPort(host, port), nil
}

// Dial connects to the server. For ldaps:// the TLS handshake is part of
// the dial; StartTLS is a separate call. ctx cancels any later operation too.
func Dial(ctx context.Context, o DialOptions) (*Conn, error) {
	scheme, host, addr, err := parseURL(o.URL)
	if err != nil {
		return nil, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	d := net.Dialer{Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ldap: connect to %s: %w", addr, err)
	}
	c := &Conn{nc: nc, timeout: timeout, host: host}
	if scheme == "ldaps" {
		tc := tls.Client(nc, tlsConfig(host, o))
		nc.SetDeadline(time.Now().Add(timeout))
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, fmt.Errorf("ldap: TLS handshake with %s: %w", addr, err)
		}
		c.nc = tc
	}
	c.r = bufio.NewReader(c.nc)
	c.watch(ctx)
	return c, nil
}

// watch makes ctx's end break any blocked read or write.
func (c *Conn) watch(ctx context.Context) {
	if c.stop != nil {
		c.stop()
	}
	nc := c.nc
	c.stop = context.AfterFunc(ctx, func() { nc.SetDeadline(time.Unix(1, 0)) })
}

func tlsConfig(host string, o DialOptions) *tls.Config {
	return &tls.Config{ServerName: host, InsecureSkipVerify: o.InsecureSkipVerify, RootCAs: o.RootCAs,
		MinVersion: tls.VersionTLS12}
}

// Close sends an UnbindRequest (best effort) and closes the connection.
func (c *Conn) Close() error {
	if c.stop != nil {
		c.stop()
	}
	c.nc.SetDeadline(time.Now().Add(time.Second))
	c.id++
	c.nc.Write(ber.TLV(ber.TagSequence, ber.Int(ber.TagInteger, c.id), ber.TLV(opUnbindRequest)))
	return c.nc.Close()
}

// send writes one LDAPMessage and returns its id.
func (c *Conn) send(op []byte) (int64, error) {
	c.id++
	if err := c.nc.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	if _, err := c.nc.Write(ber.TLV(ber.TagSequence, ber.Int(ber.TagInteger, c.id), op)); err != nil {
		return 0, fmt.Errorf("ldap: write: %w", err)
	}
	return c.id, nil
}

// recv reads one LDAPMessage for id and returns its protocol op identifier
// and content. Controls are ignored. A message for another id (including an
// unsolicited notice of disconnection, id 0) is an error.
func (c *Conn) recv(id int64) (byte, []byte, error) {
	msg, err := ber.ReadElement(c.r, maxMessageBytes)
	if err != nil {
		return 0, nil, fmt.Errorf("ldap: read: %w", err)
	}
	body, _, err := ber.Expect(msg, ber.TagSequence)
	if err != nil {
		return 0, nil, err
	}
	idc, rest, err := ber.Expect(body, ber.TagInteger)
	if err != nil {
		return 0, nil, err
	}
	got, err := ber.ParseInt(idc)
	if err != nil {
		return 0, nil, err
	}
	tag, content, _, err := ber.Parse(rest)
	if err != nil {
		return 0, nil, err
	}
	if got != id {
		if got == 0 {
			return 0, nil, errors.New("ldap: the server closed the connection")
		}
		return 0, nil, fmt.Errorf("ldap: unexpected message id %d", got)
	}
	return tag, content, nil
}

// parseResult decodes an LDAPResult (resultCode, matchedDN, diagnosticMessage).
func parseResult(op string, content []byte) error {
	code, rest, err := ber.Expect(content, ber.TagEnumerated)
	if err != nil {
		return err
	}
	n, err := ber.ParseInt(code)
	if err != nil {
		return err
	}
	_, rest, err = ber.Expect(rest, ber.TagOctetString) // matchedDN
	if err != nil {
		return err
	}
	diag, _, err := ber.Expect(rest, ber.TagOctetString)
	if err != nil {
		return err
	}
	if n != ResultSuccess {
		return &ResultError{Op: op, Code: n, Diagnostic: string(diag)}
	}
	return nil
}

// StartTLS upgrades a plain connection.
func (c *Conn) StartTLS(ctx context.Context, o DialOptions) error {
	id, err := c.send(ber.TLV(opExtendedRequest, ber.String(extName, StartTLSOID)))
	if err != nil {
		return err
	}
	tag, content, err := c.recv(id)
	if err != nil {
		return err
	}
	if tag != opExtendedResp {
		return fmt.Errorf("%w: unexpected StartTLS response 0x%02x", ber.ErrMalformed, tag)
	}
	if err := parseResult("StartTLS", content); err != nil {
		return err
	}
	if c.r.Buffered() > 0 {
		return errors.New("ldap: unexpected data before the TLS handshake")
	}
	tc := tls.Client(c.nc, tlsConfig(c.host, o))
	if err := tc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("ldap: TLS handshake: %w", err)
	}
	c.nc = tc
	c.r = bufio.NewReader(tc)
	c.watch(ctx)
	return nil
}

// Bind performs a simple bind. An empty password is refused here: servers
// treat it as an unauthenticated bind that "succeeds" (RFC 4513 5.1.2).
func (c *Conn) Bind(dn, password string) error {
	if password == "" {
		return ErrInvalidCredentials
	}
	id, err := c.send(ber.TLV(opBindRequest,
		ber.Int(ber.TagInteger, 3), ber.String(ber.TagOctetString, dn), ber.String(authSimple, password)))
	if err != nil {
		return err
	}
	tag, content, err := c.recv(id)
	if err != nil {
		return err
	}
	if tag != opBindResponse {
		return fmt.Errorf("%w: unexpected bind response 0x%02x", ber.ErrMalformed, tag)
	}
	return parseResult("bind", content)
}

// SearchRequest is a whole-subtree search.
type SearchRequest struct {
	BaseDN     string
	Filter     string // RFC 4515; compiled with CompileFilter
	Attributes []string
	SizeLimit  int
}

// Search runs a whole-subtree search and returns the entries. A result of
// sizeLimitExceeded still returns the entries received, with the error.
func (c *Conn) Search(req SearchRequest) ([]Entry, error) {
	filter, err := CompileFilter(req.Filter)
	if err != nil {
		return nil, err
	}
	var attrs [][]byte
	want := map[string]bool{}
	for _, a := range req.Attributes {
		if a != "" {
			want[strings.ToLower(a)] = true
			attrs = append(attrs, ber.String(ber.TagOctetString, a))
		}
	}
	id, err := c.send(ber.TLV(opSearchRequest,
		ber.String(ber.TagOctetString, req.BaseDN),
		ber.Int(ber.TagEnumerated, scopeWholeSubtree),
		ber.Int(ber.TagEnumerated, derefNever),
		ber.Int(ber.TagInteger, int64(req.SizeLimit)),
		ber.Int(ber.TagInteger, searchTimeLimitSeconds),
		ber.Bool(false),
		filter,
		ber.TLV(ber.TagSequence, attrs...)))
	if err != nil {
		return nil, err
	}
	var out []Entry
	for i := 0; i < maxMessagesPerSearchReply; i++ {
		tag, content, err := c.recv(id)
		if err != nil {
			return nil, err
		}
		switch tag {
		case opSearchEntry:
			e, err := parseEntry(content, want)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
			if req.SizeLimit > 0 && len(out) > req.SizeLimit {
				return out, &ResultError{Op: "search", Code: ResultSizeLimitExceeded}
			}
		case opSearchReference:
			// referrals are not followed
		case opSearchDone:
			return out, parseResult("search", content)
		default:
			return nil, fmt.Errorf("%w: unexpected search response 0x%02x", ber.ErrMalformed, tag)
		}
	}
	return nil, errors.New("ldap: too many search responses")
}

// Bounds of a search result entry.
const (
	maxAttrsPerEntry = 64
	maxValuesPerAttr = 1024
	maxMessageBytes  = 1 << 20 // one LDAPMessage
)

// parseEntry decodes a SearchResultEntry, keeping only the attributes in
// want (lower-cased; all when want is empty). It walks the elements in
// place without building intermediate lists, and refuses more than
// maxAttrsPerEntry attributes or maxValuesPerAttr values of one attribute,
// requested or not.
func parseEntry(content []byte, want map[string]bool) (Entry, error) {
	dn, rest, err := ber.Expect(content, ber.TagOctetString)
	if err != nil {
		return Entry{}, err
	}
	list, _, err := ber.Expect(rest, ber.TagSequence)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{DN: string(dn), Attrs: map[string][]string{}}
	for n := 0; len(list) > 0; n++ {
		if n == maxAttrsPerEntry {
			return Entry{}, fmt.Errorf("ldap: entry has more than %d attributes", maxAttrsPerEntry)
		}
		var body []byte
		if body, list, err = ber.Expect(list, ber.TagSequence); err != nil {
			return Entry{}, err
		}
		name, rest, err := ber.Expect(body, ber.TagOctetString)
		if err != nil {
			return Entry{}, err
		}
		set, _, err := ber.Expect(rest, ber.TagSet)
		if err != nil {
			return Entry{}, err
		}
		key := strings.ToLower(string(name))
		keep := len(want) == 0 || want[key]
		for i := 0; len(set) > 0; i++ {
			if i == maxValuesPerAttr {
				return Entry{}, fmt.Errorf("ldap: attribute %.64q has more than %d values", key, maxValuesPerAttr)
			}
			var v []byte
			if v, set, err = ber.Expect(set, ber.TagOctetString); err != nil {
				return Entry{}, err
			}
			if keep {
				e.Attrs[key] = append(e.Attrs[key], string(v))
			}
		}
	}
	return e, nil
}
