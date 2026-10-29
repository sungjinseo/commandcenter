package meta

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestValidateHTTP(t *testing.T) {
	ok := DefaultHTTP()
	ok.Proxy = HTTPProxy{Host: "proxy.corp", Port: 3128, Username: "u", Password: "p", NonProxyHosts: []string{"*.corp", "10.*"}}
	ok.UserAgentSuffix = "corp/1"
	if err := ValidateHTTP(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*HTTP){
		"retries":      func(c *HTTP) { c.Retries = 11 },
		"connect 0":    func(c *HTTP) { c.ConnectTimeoutSeconds = 0 },
		"timeout big":  func(c *HTTP) { c.TimeoutSeconds = 601 },
		"host scheme":  func(c *HTTP) { c.Proxy.Host = "http://proxy" },
		"host space":   func(c *HTTP) { c.Proxy.Host = "pro xy" },
		"port":         func(c *HTTP) { c.Proxy.Port = 0 },
		"ua long":      func(c *HTTP) { c.UserAgentSuffix = strings.Repeat("x", 101) },
		"ua control":   func(c *HTTP) { c.UserAgentSuffix = "a\nb" },
		"pattern":      func(c *HTTP) { c.Proxy.NonProxyHosts = []string{"a b"} },
		"empty patter": func(c *HTTP) { c.Proxy.NonProxyHosts = []string{""} },
	} {
		c := ok.Copy()
		mut(&c)
		if err := ValidateHTTP(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateHTTP(DefaultHTTP()); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSettingRoundTripSealed(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.SetSecretKey(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	s, err := db.LoadSettings(ctx)
	if err != nil || s.HTTP.ConnectTimeoutSeconds != 10 || s.HTTP.TimeoutSeconds != 30 || s.MetricsAnonymous || len(s.TrustedCerts) != 0 {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	cfg := DefaultHTTP()
	cfg.Proxy = HTTPProxy{Host: "p", Port: 1, Username: "u", Password: "px-secret", NonProxyHosts: []string{"*.corp"}}
	if err := db.SetSettingJSON(ctx, SettingHTTP, cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSettingJSON(ctx, SettingHTTP, HTTP{Retries: 99}); err == nil {
		t.Fatal("invalid config stored")
	}
	if raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingHTTP); strings.Contains(raw, "px-secret") || !strings.Contains(raw, sealedPrefix) {
		t.Fatalf("stored password not sealed: %s", raw)
	}
	s, err = db.LoadSettings(ctx)
	if err != nil || s.HTTP.Proxy.Password != "px-secret" || s.HTTP.Proxy.Host != "p" {
		t.Fatalf("loaded: %+v %v", s.HTTP, err)
	}
	var got HTTP
	if _, err := db.GetSettingJSON(ctx, SettingHTTP, &got); err != nil || got.Proxy.Password != "px-secret" {
		t.Fatalf("GetSettingJSON: %+v %v", got, err)
	}
	s.MetricsAnonymous, s.TerraformDefaultRepository = true, "tf-hosted"
	if err := db.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	s, err = db.LoadSettings(ctx)
	if err != nil || !s.MetricsAnonymous || s.TerraformDefaultRepository != "tf-hosted" || s.HTTP.Proxy.Password != "px-secret" {
		t.Fatalf("after save: %+v %v", s, err)
	}
	if raw := rawRow(t, db, `SELECT value FROM settings WHERE key = ?`, SettingHTTP); strings.Contains(raw, "px-secret") {
		t.Fatalf("SaveSettings stored the password in plain text")
	}
}

// testCertPEM returns a self-signed certificate.
func testCertPEM(t *testing.T, cn string) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn, Organization: []string{"Test"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestTrustedCertificatesSetting(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	a, b := testCertPEM(t, "a"), testCertPEM(t, "b")
	if err := ValidateTrustedCertificates([]string{a, "not pem"}); err == nil {
		t.Fatal("bad PEM accepted")
	}
	if err := ValidateTrustedCertificates([]string{a, a}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := ValidateTrustedCertificates([]string{a + b}); err == nil {
		t.Fatal("two certificates in one entry accepted")
	}
	if err := db.SetSettingJSON(ctx, SettingTrustedCertificates, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	s, err := db.LoadSettings(ctx)
	if err != nil || len(s.TrustedCerts) != 2 || len(s.TrustedCertificates()) != 2 {
		t.Fatalf("loaded: %d %v", len(s.TrustedCerts), err)
	}
	certs, _ := ParseCertificates(a)
	if certs[0].Subject.CommonName != "a" || len(Fingerprint(certs[0])) != 64 {
		t.Fatalf("parsed %v", certs[0].Subject)
	}
}
