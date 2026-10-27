package ldap_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/ldap"
	"commandcenter/internal/ldap/ber"
	"commandcenter/internal/ldap/ldaptest"
	"commandcenter/internal/meta"
)

const (
	serviceDN = "cn=svc,dc=example,dc=org"
	servicePW = "svc-secret"
	aliceDN   = "uid=alice,ou=people,dc=example,dc=org"
)

func seed(s *ldaptest.Server) {
	s.Add(ldaptest.Entry{DN: serviceDN, Password: servicePW})
	s.Add(ldaptest.Entry{DN: aliceDN, Password: "alice-pw", Attrs: map[string][]string{
		"uid": {"alice"}, "cn": {"Alice Liddell"}, "mail": {"alice@example.org"}, "objectClass": {"person"},
		"memberOf": {"cn=cc-admins,ou=groups,dc=example,dc=org", "cn=devs,ou=groups,dc=example,dc=org"}}})
	s.Add(ldaptest.Entry{DN: "uid=bob,ou=people,dc=example,dc=org", Password: "bob-pw", Attrs: map[string][]string{
		"uid": {"bob"}, "cn": {"Bob"}, "objectClass": {"person"}}})
}

func config(url string) meta.LDAP {
	c := meta.DefaultLDAP()
	c.URL = url
	c.BindDN, c.BindPassword = serviceDN, servicePW
	c.UserBaseDN = "ou=people,dc=example,dc=org"
	c.TimeoutSeconds = 2
	return c
}

func TestEscapeFilter(t *testing.T) {
	got := ldap.EscapeFilter("a*b(c)d\\e\x00f")
	if want := `a\2ab\28c\29d\5ce\00f`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := ldap.UserFilter("(&(objectClass=person)(uid={user}))", "*)(uid=*"); got != `(&(objectClass=person)(uid=\2a\29\28uid=\2a))` {
		t.Fatalf("UserFilter = %q", got)
	}
}

func TestCompileFilter(t *testing.T) {
	for _, ok := range []string{"(uid=x)", "(&(objectClass=person)(uid=x))", "(|(a=1)(b=2))", "(!(a=1))", "(cn=*)",
		`(uid=\2a\29)`, "(&(objectClass=person)(|(uid=a)(mail=a@b))(!(disabled=TRUE)))"} {
		if _, err := ldap.CompileFilter(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "uid=x", "(uid=x", "(uid=x))", "(uid=a*)", "(uid>=1)", "(&)", "(=x)",
		`(uid=\2)`, `(uid=\zz)`, "(uid=x)(uid=y)", "(uid=(x)", strings.Repeat("(!", 40) + "(a=1)" + strings.Repeat(")", 40)} {
		if _, err := ldap.CompileFilter(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// The escaped injection compiles to one equality with the literal value.
	f, err := ldap.CompileFilter(ldap.UserFilter("(uid={user})", "*)(uid=*"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(f, []byte("*)(uid=*")) {
		t.Fatalf("literal value missing: %x", f)
	}
}

func TestAuthenticate(t *testing.T) {
	s := ldaptest.New(t, nil)
	seed(s)
	cl := &ldap.Client{}
	ctx := context.Background()
	cfg := config(s.URL("ldap"))

	u, err := cl.Authenticate(ctx, cfg, "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "alice" || u.Name != "Alice Liddell" || u.Email != "alice@example.org" || u.DN != aliceDN || len(u.Groups) != 2 {
		t.Fatalf("user %+v", u)
	}
	if _, err := cl.Authenticate(ctx, cfg, "alice", "wrong"); !errors.Is(err, ldap.ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := cl.Authenticate(ctx, cfg, "nobody", "x"); !errors.Is(err, ldap.ErrNoSuchUser) {
		t.Fatalf("unknown: %v", err)
	}
	// Service bind failure.
	bad := cfg
	bad.BindPassword = "nope"
	if _, err := cl.Authenticate(ctx, bad, "alice", "alice-pw"); !errors.Is(err, ldap.ErrInvalidCredentials) {
		t.Fatalf("service bind: %v", err)
	}
}

func TestAuthenticateRefusesEmptyPassword(t *testing.T) {
	s := ldaptest.New(t, nil)
	seed(s)
	cl := &ldap.Client{}
	cfg := config(s.URL("ldap"))
	// The fake (like real servers) accepts an unauthenticated bind; the
	// client must never send one.
	before := s.Binds()
	if _, err := cl.Authenticate(context.Background(), cfg, "alice", ""); !errors.Is(err, ldap.ErrInvalidCredentials) {
		t.Fatalf("empty password: %v", err)
	}
	if _, err := cl.Authenticate(context.Background(), cfg, "", "x"); !errors.Is(err, ldap.ErrInvalidCredentials) {
		t.Fatalf("empty user: %v", err)
	}
	if s.Binds() != before {
		t.Fatalf("contacted the server: %d binds", s.Binds()-before)
	}
}

func TestFilterInjection(t *testing.T) {
	s := ldaptest.New(t, nil)
	seed(s)
	cl := &ldap.Client{}
	cfg := config(s.URL("ldap"))
	cfg.UserFilter = "(&(objectClass=person)(uid={user}))"
	for _, name := range []string{"*", "*)(uid=*", "alice)(|(uid=*", "al*"} {
		if _, err := cl.Authenticate(context.Background(), cfg, name, "alice-pw"); !errors.Is(err, ldap.ErrNoSuchUser) {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, f := range s.Filters() {
		if strings.Count(f, "(") != 3 { // (&(objectClass=person)(uid=…)) only
			t.Errorf("search widened: %s", f)
		}
	}
}

func TestSearchAmbiguous(t *testing.T) {
	s := ldaptest.New(t, nil)
	seed(s)
	s.Add(ldaptest.Entry{DN: "uid=alice,ou=other,ou=people,dc=example,dc=org", Password: "alice-pw",
		Attrs: map[string][]string{"uid": {"alice"}}})
	cl := &ldap.Client{}
	if _, err := cl.Authenticate(context.Background(), config(s.URL("ldap")), "alice", "alice-pw"); !errors.Is(err, ldap.ErrAmbiguous) {
		t.Fatalf("two entries: %v", err)
	}
}

func TestFind(t *testing.T) {
	s := ldaptest.New(t, nil)
	seed(s)
	cl := &ldap.Client{}
	cfg := config(s.URL("ldap"))
	u, err := cl.Find(context.Background(), cfg, "alice")
	if err != nil || u.ID != "alice" {
		t.Fatalf("%+v %v", u, err)
	}
	s.Remove(aliceDN)
	if _, err := cl.Find(context.Background(), cfg, "alice"); !errors.Is(err, ldap.ErrNoSuchUser) {
		t.Fatalf("removed: %v", err)
	}
	s.SetDown(true)
	_, err = cl.Find(context.Background(), cfg, "bob")
	if err == nil || errors.Is(err, ldap.ErrNoSuchUser) {
		t.Fatalf("outage must be an error distinct from no such user: %v", err)
	}
}

func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

func TestStartTLSAndLDAPS(t *testing.T) {
	tc, pool := selfSigned(t)
	ctx := context.Background()

	s := ldaptest.New(t, nil)
	s.TLS = tc
	seed(s)
	cfg := config(s.URL("ldap"))
	cfg.StartTLS = true
	if _, err := ldap.NewTestClient(pool).Authenticate(ctx, cfg, "alice", "alice-pw"); err != nil {
		t.Fatalf("starttls: %v", err)
	}
	// Verification is on by default: an untrusted certificate fails.
	if _, err := (&ldap.Client{}).Authenticate(ctx, cfg, "alice", "alice-pw"); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("untrusted starttls: %v", err)
	}
	cfg.InsecureSkipVerify = true
	if _, err := (&ldap.Client{}).Authenticate(ctx, cfg, "alice", "alice-pw"); err != nil {
		t.Fatalf("insecure starttls: %v", err)
	}

	ss := ldaptest.New(t, tc)
	seed(ss)
	cfg = config(ss.URL("ldaps"))
	if _, err := ldap.NewTestClient(pool).Authenticate(ctx, cfg, "alice", "alice-pw"); err != nil {
		t.Fatalf("ldaps: %v", err)
	}
	if _, err := (&ldap.Client{}).Authenticate(ctx, cfg, "alice", "alice-pw"); err == nil {
		t.Fatal("untrusted ldaps accepted")
	}
}

// rawServer answers every connection with reply, then waits for the client.
func rawServer(t *testing.T, reply []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(c)
				ber.ReadElement(r, ber.MaxLen) // the request
				c.Write(reply)
				r.ReadByte()
			}()
		}
	}()
	return "ldap://" + ln.Addr().String()
}

func TestMalformedResponses(t *testing.T) {
	ctx := context.Background()
	cases := map[string][]byte{
		"oversized length":  {0x30, 0x84, 0x7f, 0xff, 0xff, 0xff},
		"length over 4":     {0x30, 0x85, 0x01, 0x00, 0x00, 0x00, 0x00},
		"indefinite length": {0x30, 0x80, 0x00, 0x00},
		"truncated":         {0x30, 0x10, 0x02, 0x01, 0x01},
		"wrong id":          ber.TLV(0x30, ber.Int(0x02, 7), ber.TLV(0x61, ber.Int(0x0a, 0), ber.String(4, ""), ber.String(4, ""))),
		"inner overrun":     {0x30, 0x05, 0x02, 0x09, 0x01, 0x61, 0x00},
		"not a sequence":    {0x04, 0x01, 0x00},
		"high tag":          {0x1f, 0x01, 0x00},
		"bad result":        ber.TLV(0x30, ber.Int(0x02, 1), ber.TLV(0x61, ber.String(4, "x"))),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			url := rawServer(t, reply)
			c, err := ldap.Dial(ctx, ldap.DialOptions{URL: url, Timeout: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			start := time.Now()
			if err := c.Bind("cn=x", "pw"); err == nil {
				t.Fatal("accepted")
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("not bounded")
			}
		})
	}
}

func TestBERRoundTrip(t *testing.T) {
	for _, v := range []int64{0, 1, 127, 128, 255, 256, 65535, 1 << 24, -1, -128, -129} {
		_, c, rest, err := ber.Parse(ber.Int(ber.TagInteger, v))
		if err != nil || len(rest) != 0 {
			t.Fatal(err)
		}
		if got, err := ber.ParseInt(c); err != nil || got != v {
			t.Fatalf("%d: got %d %v", v, got, err)
		}
	}
	long := strings.Repeat("x", 70000)
	_, c, _, err := ber.Parse(ber.String(ber.TagOctetString, long))
	if err != nil || string(c) != long {
		t.Fatal("long string")
	}
	if _, err := ber.ParseInt(make([]byte, 9)); err == nil {
		t.Fatal("9-byte integer accepted")
	}
}

func TestReadTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			time.Sleep(3 * time.Second) // never answers
			c.Close()
		}
	}()
	c, err := ldap.Dial(context.Background(), ldap.DialOptions{URL: "ldap://" + ln.Addr().String(), Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if err := c.Bind("cn=x", "pw"); err == nil {
		t.Fatal("no timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

// A context that ended between two operations stops the next one: setting
// that operation's deadline must not undo the one the context's end set.
func TestCancelBetweenOperations(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			time.Sleep(3 * time.Second) // never answers
			c.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	c, err := ldap.Dial(ctx, ldap.DialOptions{URL: "ldap://" + ln.Addr().String(), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	time.Sleep(100 * time.Millisecond) // the end of ctx reaches the connection before the bind
	start := time.Now()
	if err := c.Bind("cn=x", "pw"); !errors.Is(err, context.Canceled) {
		t.Fatalf("bind after cancel: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("bind after cancel took %v", took)
	}
}

func TestMapRoles(t *testing.T) {
	cfg := meta.DefaultLDAP()
	cfg.GroupMappings = []meta.GroupMapping{
		{Group: "CC-Admins", Roles: []string{"admin"}},
		{Group: "cn=devs,ou=groups,dc=example,dc=org", Roles: []string{"deployer", "reader"}},
		{Group: "cn=devs,ou=other,dc=example,dc=org", Roles: []string{"other"}},
	}
	cfg.DefaultRoles = []string{"reader", "base"}
	roles, matched := ldap.MapRoles(cfg, []string{"cn=cc-admins,ou=groups,dc=example,dc=org", "CN=Devs,OU=Groups,DC=example,DC=org", "cn=nobody"})
	if strings.Join(roles, ",") != "admin,deployer,reader,base" || len(matched) != 2 {
		t.Fatalf("roles %v matched %v", roles, matched)
	}
	if roles, _ := ldap.MapRoles(cfg, nil); strings.Join(roles, ",") != "reader,base" {
		t.Fatalf("defaults only: %v", roles)
	}
	if ldap.GroupMatches("cn=x\\,y,ou=g", "x\\,y") != true || ldap.GroupMatches("ou=g", "") {
		t.Fatal("GroupMatches")
	}
}

func TestDiagnose(t *testing.T) {
	s := ldaptest.New(t, nil)
	seed(s)
	cfg := config(s.URL("ldap"))
	cfg.GroupMappings = []meta.GroupMapping{{Group: "cc-admins", Roles: []string{"admin"}}}
	steps := (&ldap.Client{}).Diagnose(context.Background(), cfg, "alice", "alice-pw")
	var names []string
	for _, st := range steps {
		names = append(names, st.Step)
		if !st.OK {
			t.Errorf("step %s failed: %s", st.Step, st.Detail)
		}
		if strings.Contains(st.Detail, servicePW) || strings.Contains(st.Detail, "alice-pw") {
			t.Errorf("secret in %s: %s", st.Step, st.Detail)
		}
	}
	if strings.Join(names, ",") != "connect,bind,search,userBind,groups" {
		t.Fatalf("steps %v", names)
	}
	if d := steps[len(steps)-1].Detail; !strings.Contains(d, "cc-admins") || !strings.Contains(d, "admin") {
		t.Fatalf("groups detail %q", d)
	}
	// A wrong password stops at userBind.
	steps = (&ldap.Client{}).Diagnose(context.Background(), cfg, "alice", "bad")
	if last := steps[len(steps)-1]; last.Step != "userBind" || last.OK {
		t.Fatalf("%+v", steps)
	}
	// Without a user only the connection and the service bind run.
	steps = (&ldap.Client{}).Diagnose(context.Background(), cfg, "", "")
	if len(steps) != 2 || !steps[1].OK {
		t.Fatalf("%+v", steps)
	}
	// A bad service password is reported, never echoed.
	cfg.BindPassword = "the-wrong-secret"
	steps = (&ldap.Client{}).Diagnose(context.Background(), cfg, "", "")
	if last := steps[len(steps)-1]; last.Step != "bind" || last.OK || strings.Contains(last.Detail, "the-wrong-secret") {
		t.Fatalf("%+v", steps)
	}
	// Unreachable server.
	s.SetDown(true)
	steps = (&ldap.Client{}).Diagnose(context.Background(), cfg, "", "")
	if len(steps) == 0 || steps[len(steps)-1].OK {
		t.Fatalf("%+v", steps)
	}
}

// searchReply builds LDAPMessages with id 1 for a raw server.
func msg1(op []byte) []byte { return ber.TLV(0x30, ber.Int(0x02, 1), op) }

func searchDone() []byte {
	return msg1(ber.TLV(0x65, ber.Int(0x0a, 0), ber.String(4, ""), ber.String(4, "")))
}

func entryMsg(dn string, attrs ...[]byte) []byte {
	return msg1(ber.TLV(0x64, ber.String(4, dn), ber.TLV(0x30, attrs...)))
}

func attr(name string, vals ...string) []byte {
	var vs [][]byte
	for _, v := range vals {
		vs = append(vs, ber.String(4, v))
	}
	return ber.TLV(0x30, ber.String(4, name), ber.TLV(0x31, vs...))
}

func rawSearch(t *testing.T, reply []byte, attrs ...string) ([]ldap.Entry, error) {
	t.Helper()
	c, err := ldap.Dial(context.Background(), ldap.DialOptions{URL: rawServer(t, reply), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.Search(ldap.SearchRequest{BaseDN: "dc=x", Filter: "(uid=a)", Attributes: attrs, SizeLimit: 2})
}

// allocated runs f and returns the bytes it allocated.
func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

func TestOversizedMessageRejected(t *testing.T) {
	// One 2 MiB entry: over the 1 MiB message cap, refused before it is read.
	big := append(entryMsg("uid=a,dc=x", attr("uid", strings.Repeat("x", 2<<20))), searchDone()...)
	var err error
	n := allocated(func() { _, err = rawSearch(t, big, "uid") })
	if err == nil || !errors.Is(err, ber.ErrTooLong) {
		t.Fatalf("oversized message: %v", err)
	}
	t.Logf("allocated %d bytes", n)
	if n > 1<<20 {
		t.Fatalf("allocated %d bytes for a refused message", n)
	}
}

func TestHighCardinalityEntryRejected(t *testing.T) {
	// ~900 KiB of 2-byte values in one attribute (under the message cap).
	vals := make([]string, 450_000)
	reply := append(entryMsg("uid=a,dc=x", attr("memberOf", vals...)), searchDone()...)
	var err error
	n := allocated(func() { _, err = rawSearch(t, reply, "memberOf") })
	if err == nil {
		t.Fatal("450k values accepted")
	}
	t.Logf("allocated %d bytes", n)
	if n > 10<<20 {
		t.Fatalf("allocated %d bytes", n)
	}
	// Too many attributes.
	var attrs [][]byte
	for i := 0; i < 65; i++ {
		attrs = append(attrs, attr(fmt.Sprintf("a%d", i), "v"))
	}
	if _, err := rawSearch(t, append(entryMsg("uid=a,dc=x", attrs...), searchDone()...), "uid"); err == nil {
		t.Fatal("65 attributes accepted")
	}
	// Many values of an attribute nobody asked for are still bounded.
	vals = make([]string, 2000)
	if _, err := rawSearch(t, append(entryMsg("uid=a,dc=x", attr("junk", vals...)), searchDone()...), "uid"); err == nil {
		t.Fatal("2000 unrequested values accepted")
	}
	// Children caps the number of elements.
	many := make([]byte, 0, 5000*2)
	for i := 0; i < 5000; i++ {
		many = append(many, 0x04, 0x00)
	}
	if _, err := ber.Children(many); err == nil {
		t.Fatal("Children: 5000 elements accepted")
	}
}

func TestOnlyRequestedAttributesKept(t *testing.T) {
	reply := append(entryMsg("uid=a,dc=x", attr("uid", "a"), attr("jpegPhoto", strings.Repeat("p", 1000)), attr("CN", "A")), searchDone()...)
	es, err := rawSearch(t, reply, "uid", "cn")
	if err != nil || len(es) != 1 {
		t.Fatalf("%v %v", es, err)
	}
	if _, ok := es[0].Attrs["jpegphoto"]; ok || es[0].First("cn") != "A" || es[0].First("uid") != "a" {
		t.Fatalf("attrs %v", es[0].Attrs)
	}
}

func TestSearchReferenceIgnored(t *testing.T) {
	ref := msg1(ber.TLV(0x73, ber.String(4, "ldap://elsewhere/dc=x")))
	reply := append(append(ref, entryMsg("uid=a,dc=x", attr("uid", "a"))...), searchDone()...)
	es, err := rawSearch(t, reply, "uid")
	if err != nil || len(es) != 1 || es[0].DN != "uid=a,dc=x" {
		t.Fatalf("%v %v", es, err)
	}
}

func TestNoticeOfDisconnection(t *testing.T) {
	notice := ber.TLV(0x30, ber.Int(0x02, 0), ber.TLV(0x78, ber.Int(0x0a, 52), ber.String(4, ""), ber.String(4, "bye"),
		ber.String(0x8a, "1.3.6.1.4.1.1466.20036")))
	if _, err := rawSearch(t, notice, "uid"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("notice of disconnection: %v", err)
	}
}

// Fix round 2: dial-phase failures are typed (*ldap.DialError); failures
// after the service bind are not.
func TestDialErrorPhases(t *testing.T) {
	ctx := context.Background()
	cl := &ldap.Client{}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	cfg := config("ldap://" + closed)
	var de *ldap.DialError
	if _, err := cl.Authenticate(ctx, cfg, "alice", "pw"); !errors.As(err, &de) || de.Phase != "connect" {
		t.Fatalf("connect: %v", err)
	}
	s := ldaptest.New(t, nil)
	seed(s)
	cfg = config(s.URL("ldap"))
	cfg.StartTLS = true // the fake has no TLS: StartTLS fails
	if _, err := cl.Authenticate(ctx, cfg, "alice", "alice-pw"); !errors.As(err, &de) || de.Phase != "starttls" {
		t.Fatalf("starttls: %v", err)
	}
	cfg.StartTLS = false
	s.SetDown(true) // connect works, the service bind reads EOF
	if _, err := cl.Authenticate(ctx, cfg, "alice", "alice-pw"); !errors.As(err, &de) || de.Phase != "bind" {
		t.Fatalf("service bind I/O: %v", err)
	}
	s.SetDown(false)
	bad := cfg
	bad.BindPassword = "wrong"
	if _, err := cl.Authenticate(ctx, bad, "alice", "alice-pw"); errors.As(err, &de) || !errors.Is(err, ldap.ErrInvalidCredentials) {
		t.Fatalf("service bind refused is an answer, not a dial error: %v", err)
	}
	s.SetNoticeOnSearch(true)
	if _, err := cl.Authenticate(ctx, cfg, "alice", "alice-pw"); err == nil || errors.As(err, &de) {
		t.Fatalf("notice after the bind: %v", err)
	}
}
