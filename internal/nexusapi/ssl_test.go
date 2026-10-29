package nexusapi_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

func TestSSLCompat(t *testing.T) {
	e := start(t, false)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "nexus-ca", Organization: []string{"O"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	id := meta.Fingerprint(cert)

	e.want("POST", v1+"/security/ssl/truststore", "ci", pemText, http.StatusForbidden)
	body := e.want("POST", v1+"/security/ssl/truststore", "admin", pemText, http.StatusCreated)
	var got map[string]any
	json.Unmarshal(body, &got)
	if got["id"] != id || got["subjectCommonName"] != "nexus-ca" || got["subjectOrganization"] != "O" || got["issuedOn"].(float64) <= 0 {
		t.Fatalf("added %s", body)
	}
	e.want("POST", v1+"/security/ssl/truststore", "admin", pemText, http.StatusConflict)
	var list []map[string]any
	e.get(v1+"/security/ssl", "admin", &list)
	if len(list) != 1 || list[0]["fingerprint"] != id {
		t.Fatalf("list %v", list)
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	var leaf map[string]any
	e.get(v1+"/security/ssl?host=127.0.0.1&port="+u.Port(), "admin", &leaf)
	if leaf["id"] != meta.Fingerprint(ts.Certificate()) || leaf["pem"] == "" {
		t.Fatalf("retrieve %v", leaf)
	}
	e.want("DELETE", v1+"/security/ssl/truststore/"+id, "admin", "", http.StatusNoContent)
	e.want("DELETE", v1+"/security/ssl/truststore/"+id, "admin", "", http.StatusNotFound)
	e.get(v1+"/security/ssl", "admin", &list)
	if len(list) != 0 {
		t.Fatalf("after delete %v", list)
	}
}
