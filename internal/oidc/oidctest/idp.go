// Package oidctest is a fake OpenID Connect provider for tests: discovery,
// a JWKS with an RSA and an EC key, an authorization endpoint that redirects
// at once with a code, a token endpoint that checks PKCE and the client
// credentials, and userinfo.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

// Server is the fake provider. The exported fields may be set between
// requests (under Lock when the server is busy); Claims overrides or adds
// ID-token claims, Userinfo is what /userinfo answers (nil: 404).
type Server struct {
	URL          string
	ClientID     string
	ClientSecret string

	mu           sync.Mutex
	Alg          string         // "RS256" (default) or "ES256"
	Claims       map[string]any // added to, or overriding, the standard claims
	Userinfo     map[string]any // answered by /userinfo; nil answers 404
	Issuer       string         // the iss claim and discovery issuer; defaults to URL
	WrongKey     bool           // sign with a key the JWKS does not list
	AuthMethods  []string       // token_endpoint_auth_methods_supported; nil omits the field
	NoUserinfo   bool           // omit userinfo_endpoint from discovery
	Expired      bool           // issue tokens that expired an hour ago
	codes        map[string]codeRec
	Authorizes   []url.Values // recorded authorization requests
	TokenCalls   int
	JWKSCalls    int
	rsaKey       *rsa.PrivateKey
	ecKey        *ecdsa.PrivateKey
	otherKey     *rsa.PrivateKey
	accessTokens map[string]string // access token -> sub
	ts           *httptest.Server
}

type codeRec struct {
	challenge, nonce, redirect, client string
	used                               bool
}

// New starts a provider; with useTLS it serves https with httptest's
// self-signed certificate (CACertPEM).
func New(t testing.TB, useTLS bool) *Server {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{ClientID: "cc-client", ClientSecret: "cc-secret&chars", Alg: "RS256", rsaKey: rk, ecKey: ek, otherKey: ok,
		codes: map[string]codeRec{}, accessTokens: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("GET /jwks", s.jwks)
	mux.HandleFunc("GET /authorize", s.authorize)
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("GET /userinfo", s.userinfo)
	if useTLS {
		s.ts = httptest.NewTLSServer(mux)
	} else {
		s.ts = httptest.NewServer(mux)
	}
	t.Cleanup(s.ts.Close)
	s.URL = s.ts.URL
	return s
}

// CACertPEM is the PEM of the TLS server's certificate (self-signed, so it
// is its own CA); "" for a plain server.
func (s *Server) CACertPEM() string {
	if s.ts.TLS == nil || len(s.ts.TLS.Certificates) == 0 || len(s.ts.TLS.Certificates[0].Certificate) == 0 {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.ts.TLS.Certificates[0].Certificate[0]}))
}

// Config returns an OIDC configuration for this provider.
func (s *Server) Config() meta.OIDC {
	c := meta.DefaultOIDC()
	c.Issuer, c.ClientID, c.ClientSecret, c.CACert = s.URL, s.ClientID, s.ClientSecret, s.CACertPEM()
	c.TimeoutSeconds = 5
	return c
}

func (s *Server) issuer() string {
	if s.Issuer != "" {
		return s.Issuer
	}
	return s.URL
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := map[string]any{
		"issuer": s.issuer(), "authorization_endpoint": s.URL + "/authorize", "token_endpoint": s.URL + "/token",
		"jwks_uri": s.URL + "/jwks", "scopes_supported": []string{"openid", "profile", "email", "groups"},
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		"response_types_supported":              []string{"code"},
	}
	if !s.NoUserinfo {
		d["userinfo_endpoint"] = s.URL + "/userinfo"
	}
	if s.AuthMethods != nil {
		d["token_endpoint_auth_methods_supported"] = s.AuthMethods
	}
	writeJSON(w, http.StatusOK, d)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (s *Server) jwks(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.JWKSCalls++
	s.mu.Unlock()
	pub := s.rsaKey.PublicKey
	ec := s.ecKey.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{
		{"kty": "RSA", "kid": "rsa-1", "use": "sig", "alg": "RS256", "n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes())},
		{"kty": "EC", "kid": "ec-1", "use": "sig", "alg": "ES256", "crv": "P-256",
			"x": b64(ec.X.FillBytes(make([]byte, 32))), "y": b64(ec.Y.FillBytes(make([]byte, 32)))},
	}})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Authorizes = append(s.Authorizes, q)
	if q.Get("response_type") != "code" || q.Get("client_id") != s.ClientID || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := "code-" + b64(randBytes(12))
	s.codes[code] = codeRec{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri"), client: q.Get("client_id")}
	u, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := u.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TokenCalls++
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
		if len(s.AuthMethods) > 0 && !contains(s.AuthMethods, "client_secret_basic") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
			return
		}
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.ClientID || secret != s.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	code := r.PostForm.Get("code")
	rec, ok := s.codes[code]
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if r.PostForm.Get("grant_type") != "authorization_code" || !ok || rec.used || rec.redirect != r.PostForm.Get("redirect_uri") || b64(sum[:]) != rec.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	rec.used = true
	s.codes[code] = rec
	now := time.Now()
	claims := map[string]any{"iss": s.issuer(), "sub": "sub-1", "aud": s.ClientID, "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": rec.nonce}
	if s.Expired {
		claims["exp"] = now.Add(-time.Hour).Unix()
		claims["iat"] = now.Add(-2 * time.Hour).Unix()
	}
	for k, v := range s.Claims {
		claims[k] = v
	}
	at := "at-" + b64(randBytes(12))
	s.accessTokens[at], _ = claims["sub"].(string)
	writeJSON(w, http.StatusOK, map[string]any{"id_token": s.sign(claims), "access_token": at, "token_type": "Bearer"})
}

// sign returns a compact JWS of claims under Alg (or the unlisted key).
func (s *Server) sign(claims map[string]any) string {
	hdr := map[string]string{"alg": s.Alg, "typ": "JWT", "kid": "rsa-1"}
	if s.Alg == "ES256" {
		hdr["kid"] = "ec-1"
	}
	hb, _ := json.Marshal(hdr)
	pb, _ := json.Marshal(claims)
	signed := b64(hb) + "." + b64(pb)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	switch {
	case s.Alg == "ES256" && s.WrongKey:
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		sig = ecSig(k, sum[:])
	case s.Alg == "ES256":
		sig = ecSig(s.ecKey, sum[:])
	case s.WrongKey:
		sig, _ = rsa.SignPKCS1v15(rand.Reader, s.otherKey, crypto.SHA256, sum[:])
	default:
		sig, _ = rsa.SignPKCS1v15(rand.Reader, s.rsaKey, crypto.SHA256, sum[:])
	}
	return signed + "." + b64(sig)
}

func ecSig(k *ecdsa.PrivateKey, digest []byte) []byte {
	r, sv, _ := ecdsa.Sign(rand.Reader, k, digest)
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	sv.FillBytes(out[32:])
	return out
}

func (s *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	sub, ok := s.accessTokens[tok]
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	if s.Userinfo == nil {
		http.NotFound(w, r)
		return
	}
	out := map[string]any{"sub": sub}
	for k, v := range s.Userinfo {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// Client returns a client that trusts the server's certificate and does
// not follow redirects: for driving the authorization endpoint in tests.
func (s *Server) Client() *http.Client {
	c := s.ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// Lock guards the exported fields while the server may be serving.
func (s *Server) Lock() { s.mu.Lock() }

// Unlock releases Lock.
func (s *Server) Unlock() { s.mu.Unlock() }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
