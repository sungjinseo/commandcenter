// Package ldaptest is an in-process fake LDAP server for tests. It speaks
// the BER subset of package ldap: simple bind, search with and/or/not,
// equality and presence filters, unbind and StartTLS.
//
// Like real servers, it accepts a bind with an empty password as an
// unauthenticated bind, so tests can prove the client never sends one.
package ldaptest

import (
	"bufio"
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/ldap/ber"
)

const (
	opBindRequest     byte = ber.ClassApplication | ber.Constructed | 0
	opBindResponse    byte = ber.ClassApplication | ber.Constructed | 1
	opUnbindRequest   byte = ber.ClassApplication | 2
	opSearchRequest   byte = ber.ClassApplication | ber.Constructed | 3
	opSearchEntry     byte = ber.ClassApplication | ber.Constructed | 4
	opSearchDone      byte = ber.ClassApplication | ber.Constructed | 5
	opExtendedRequest byte = ber.ClassApplication | ber.Constructed | 23
	opExtendedResp    byte = ber.ClassApplication | ber.Constructed | 24
)

// Entry is a directory entry. Password, when set, is its bind password.
type Entry struct {
	DN       string
	Password string
	Attrs    map[string][]string
}

// Server is a running fake.
type Server struct {
	ln net.Listener

	// TLS, when set before connections arrive, enables StartTLS.
	TLS *tls.Config

	mu       sync.Mutex
	entries  []Entry
	down     bool
	notice   bool
	filters  []string
	binds    atomic.Int64
	dials    atomic.Int64
	searches atomic.Int64
	conns    sync.WaitGroup
}

// New starts a fake on 127.0.0.1; it stops when the test ends. implicitTLS
// (non-nil) serves ldaps://.
func New(t testing.TB, implicitTLS *tls.Config) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicitTLS != nil {
		ln = tls.NewListener(ln, implicitTLS)
	}
	s := &Server{ln: ln}
	go s.accept()
	t.Cleanup(func() {
		ln.Close()
		s.conns.Wait()
	})
	return s
}

// URL is ldap://127.0.0.1:port (scheme as given).
func (s *Server) URL(scheme string) string { return scheme + "://" + s.ln.Addr().String() }

// Add stores an entry (replacing one with the same DN).
func (s *Server) Add(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if strings.EqualFold(s.entries[i].DN, e.DN) {
			s.entries[i] = e
			return
		}
	}
	s.entries = append(s.entries, e)
}

// Remove deletes the entry with dn.
func (s *Server) Remove(dn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.entries[:0]
	for _, e := range s.entries {
		if !strings.EqualFold(e.DN, dn) {
			out = append(out, e)
		}
	}
	s.entries = out
}

// SetNoticeOnSearch makes the server answer every search with a notice of
// disconnection (message id 0) and close the connection.
func (s *Server) SetNoticeOnSearch(on bool) {
	s.mu.Lock()
	s.notice = on
	s.mu.Unlock()
}

// SetDown makes the server drop every new connection at once (an outage).
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()
}

// Dials counts accepted connections, including those dropped while down.
func (s *Server) Dials() int64 { return s.dials.Load() }

// Binds and Searches count the operations served.
func (s *Server) Binds() int64    { return s.binds.Load() }
func (s *Server) Searches() int64 { return s.searches.Load() }

// Filters returns the decoded search filters received, in order.
func (s *Server) Filters() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.filters...)
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.dials.Add(1)
		s.mu.Lock()
		down := s.down
		s.mu.Unlock()
		if down {
			c.Close()
			continue
		}
		s.conns.Add(1)
		go func() {
			defer s.conns.Done()
			defer c.Close()
			s.serve(c)
		}()
	}
}

func (s *Server) serve(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		msg, err := ber.ReadElement(r, ber.MaxLen)
		if err != nil {
			return
		}
		body, _, err := ber.Expect(msg, ber.TagSequence)
		if err != nil {
			return
		}
		idc, rest, err := ber.Expect(body, ber.TagInteger)
		if err != nil {
			return
		}
		id, _ := ber.ParseInt(idc)
		tag, content, _, err := ber.Parse(rest)
		if err != nil {
			return
		}
		reply := func(op []byte) { c.Write(ber.TLV(ber.TagSequence, ber.Int(ber.TagInteger, id), op)) }
		switch tag {
		case opUnbindRequest:
			return
		case opBindRequest:
			s.binds.Add(1)
			reply(result(opBindResponse, s.bind(content)))
		case opSearchRequest:
			s.searches.Add(1)
			s.mu.Lock()
			notice := s.notice
			s.mu.Unlock()
			if notice {
				c.Write(ber.TLV(ber.TagSequence, ber.Int(ber.TagInteger, 0), ber.TLV(opExtendedResp, ber.Int(ber.TagEnumerated, 52),
					ber.String(ber.TagOctetString, ""), ber.String(ber.TagOctetString, "going away"), ber.String(ber.ClassContext|10, "1.3.6.1.4.1.1466.20036"))))
				return
			}
			entries, code := s.search(content)
			for _, e := range entries {
				reply(e)
			}
			reply(result(opSearchDone, code))
		case opExtendedRequest:
			if s.TLS == nil {
				reply(result(opExtendedResp, 2)) // protocolError
				continue
			}
			reply(result(opExtendedResp, 0))
			tc := tls.Server(c, s.TLS)
			if tc.Handshake() != nil {
				return
			}
			c, r = tc, bufio.NewReader(tc)
		default:
			return
		}
	}
}

func result(op byte, code int64) []byte {
	return ber.TLV(op, ber.Int(ber.TagEnumerated, code), ber.String(ber.TagOctetString, ""), ber.String(ber.TagOctetString, ""))
}

// bind returns the result code of a simple bind.
func (s *Server) bind(content []byte) int64 {
	_, rest, err := ber.Expect(content, ber.TagInteger)
	if err != nil {
		return 2
	}
	dn, rest, err := ber.Expect(rest, ber.TagOctetString)
	if err != nil {
		return 2
	}
	pw, _, err := ber.Expect(rest, ber.ClassContext|0)
	if err != nil {
		return 2
	}
	if len(pw) == 0 {
		return 0 // unauthenticated bind: real servers accept it
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if strings.EqualFold(e.DN, string(dn)) && e.Password != "" && e.Password == string(pw) {
			return 0
		}
	}
	return 49
}

func (s *Server) search(content []byte) ([][]byte, int64) {
	base, rest, err := ber.Expect(content, ber.TagOctetString)
	if err != nil {
		return nil, 2
	}
	var sizeLimit int64
	for i, want := range []byte{ber.TagEnumerated, ber.TagEnumerated, ber.TagInteger, ber.TagInteger, ber.TagBoolean} {
		var c []byte
		if c, rest, err = ber.Expect(rest, want); err != nil {
			return nil, 2
		}
		if i == 2 {
			sizeLimit, _ = ber.ParseInt(c)
		}
	}
	_, _, frest, err := ber.Parse(rest)
	if err != nil {
		return nil, 2
	}
	filter := rest[:len(rest)-len(frest)]
	attrsC, _, err := ber.Expect(frest, ber.TagSequence)
	if err != nil {
		return nil, 2
	}
	var want []string
	parts, _ := ber.Children(attrsC)
	for _, p := range parts {
		a, _, _ := ber.Expect(p, ber.TagOctetString)
		want = append(want, strings.ToLower(string(a)))
	}
	s.mu.Lock()
	s.filters = append(s.filters, describe(filter))
	entries := append([]Entry(nil), s.entries...)
	s.mu.Unlock()
	var out [][]byte
	for _, e := range entries {
		if !underBase(e.DN, string(base)) {
			continue
		}
		ok, valid := match(filter, e)
		if !valid {
			return nil, 2
		}
		if !ok {
			continue
		}
		if sizeLimit > 0 && int64(len(out)) >= sizeLimit {
			return out, 4
		}
		out = append(out, encodeEntry(e, want))
	}
	return out, 0
}

func underBase(dn, base string) bool {
	dn, base = strings.ToLower(dn), strings.ToLower(base)
	return base == "" || dn == base || strings.HasSuffix(dn, ","+base)
}

func encodeEntry(e Entry, want []string) []byte {
	var attrs [][]byte
	for name, vals := range e.Attrs {
		if len(want) > 0 && !contains(want, strings.ToLower(name)) {
			continue
		}
		var vs [][]byte
		for _, v := range vals {
			vs = append(vs, ber.String(ber.TagOctetString, v))
		}
		attrs = append(attrs, ber.TLV(ber.TagSequence, ber.String(ber.TagOctetString, name), ber.TLV(ber.TagSet, vs...)))
	}
	return ber.TLV(opSearchEntry, ber.String(ber.TagOctetString, e.DN), ber.TLV(ber.TagSequence, attrs...))
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func attrValues(e Entry, name string) []string {
	for k, v := range e.Attrs {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// match evaluates a BER filter against e; valid is false for an unknown choice.
func match(f []byte, e Entry) (ok, valid bool) {
	tag, c, _, err := ber.Parse(f)
	if err != nil {
		return false, false
	}
	switch tag {
	case ber.ClassContext | ber.Constructed | 0, ber.ClassContext | ber.Constructed | 1: // and, or
		parts, err := ber.Children(c)
		if err != nil {
			return false, false
		}
		and := tag&0x1f == 0
		for _, p := range parts {
			m, v := match(p, e)
			if !v {
				return false, false
			}
			if and && !m {
				return false, true
			}
			if !and && m {
				return true, true
			}
		}
		return and, true
	case ber.ClassContext | ber.Constructed | 2: // not
		m, v := match(c, e)
		return !m, v
	case ber.ClassContext | ber.Constructed | 3: // equality
		a, rest, err := ber.Expect(c, ber.TagOctetString)
		if err != nil {
			return false, false
		}
		v, _, err := ber.Expect(rest, ber.TagOctetString)
		if err != nil {
			return false, false
		}
		for _, x := range attrValues(e, string(a)) {
			if strings.EqualFold(x, string(v)) {
				return true, true
			}
		}
		return false, true
	case ber.ClassContext | 7: // present
		if strings.EqualFold(string(c), "objectClass") {
			return true, true
		}
		return len(attrValues(e, string(c))) > 0, true
	}
	return false, false
}

// describe renders a BER filter back as a string, for assertions.
func describe(f []byte) string {
	tag, c, _, err := ber.Parse(f)
	if err != nil {
		return "?"
	}
	switch tag {
	case ber.ClassContext | ber.Constructed | 0, ber.ClassContext | ber.Constructed | 1:
		op := "&"
		if tag&0x1f == 1 {
			op = "|"
		}
		parts, _ := ber.Children(c)
		var b strings.Builder
		b.WriteString("(" + op)
		for _, p := range parts {
			b.WriteString(describe(p))
		}
		return b.String() + ")"
	case ber.ClassContext | ber.Constructed | 2:
		return "(!" + describe(c) + ")"
	case ber.ClassContext | ber.Constructed | 3:
		a, rest, _ := ber.Expect(c, ber.TagOctetString)
		v, _, _ := ber.Expect(rest, ber.TagOctetString)
		return "(" + string(a) + "=" + escape(string(v)) + ")"
	case ber.ClassContext | 7:
		return "(" + string(c) + "=*)"
	}
	return "?"
}

// escape renders an assertion value as RFC 4515 would.
func escape(s string) string {
	r := strings.NewReplacer(`\`, `\5c`, `*`, `\2a`, `(`, `\28`, `)`, `\29`, "\x00", `\00`)
	return r.Replace(s)
}
