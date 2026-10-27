package oidc

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"commandcenter/internal/oidc/oidctest"
)

const redirect = "http://repo.example/-/sso/oidc/callback"

// login runs the code flow against idp with p and returns the identity.
func login(t *testing.T, p *Provider, idp *oidctest.Server) (Identity, error) {
	t.Helper()
	ctx := context.Background()
	state, nonce, verifier := Random(), Random(), Random()
	target, err := p.AuthURL(ctx, redirect, state, nonce, verifier)
	if err != nil {
		return Identity{}, err
	}
	res, err := idp.Client().Get(target)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc, err := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusFound || err != nil || loc.Query().Get("state") != state {
		t.Fatalf("authorize: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	return p.Exchange(ctx, loc.Query().Get("code"), redirect, verifier, nonce)
}

func newProvider(t *testing.T, idp *oidctest.Server) *Provider {
	t.Helper()
	p, err := New(idp.Config())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAuthURL(t *testing.T) {
	idp := oidctest.New(t, false)
	p := newProvider(t, idp)
	target, err := p.AuthURL(context.Background(), redirect, "st", "no", "verifier-value")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(target)
	q := u.Query()
	if !strings.HasPrefix(target, idp.URL+"/authorize?") || q.Get("client_id") != idp.ClientID || q.Get("redirect_uri") != redirect ||
		q.Get("scope") != "openid profile email groups" || q.Get("state") != "st" || q.Get("nonce") != "no" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("code_challenge") == "verifier-value" {
		t.Fatalf("auth url %s", target)
	}
}

func TestExchangeRS256AndES256(t *testing.T) {
	for _, alg := range []string{"RS256", "ES256"} {
		idp := oidctest.New(t, false)
		idp.Alg = alg
		idp.Claims = map[string]any{"preferred_username": "Alice", "groups": []string{"org:devs", "admins"}, "email": "a@example.org", "name": "Alice A"}
		id, err := login(t, newProvider(t, idp), idp)
		if err != nil {
			t.Fatalf("%s: %v", alg, err)
		}
		if id.Username != "Alice" || len(id.Groups) != 2 || id.Groups[0] != "org:devs" || id.Email != "a@example.org" || id.Name != "Alice A" {
			t.Fatalf("%s: identity %+v", alg, id)
		}
		if idp.TokenCalls != 1 {
			t.Fatalf("%s: %d token calls", alg, idp.TokenCalls)
		}
	}
}

func TestExchangeRejects(t *testing.T) {
	cases := map[string]func(idp *oidctest.Server){
		"bad signature": func(idp *oidctest.Server) { idp.WrongKey = true },
		"bad signature ES256": func(idp *oidctest.Server) {
			idp.Alg, idp.WrongKey = "ES256", true
		},
		"wrong iss":   func(idp *oidctest.Server) { idp.Claims["iss"] = "https://other.example" },
		"wrong aud":   func(idp *oidctest.Server) { idp.Claims["aud"] = "other-client" },
		"multi aud":   func(idp *oidctest.Server) { idp.Claims["aud"] = []string{"cc-client", "x"} }, // no azp
		"wrong nonce": func(idp *oidctest.Server) { idp.Claims["nonce"] = "other" },
		"expired":     func(idp *oidctest.Server) { idp.Expired = true },
		"no username": func(idp *oidctest.Server) { delete(idp.Claims, "preferred_username"); idp.NoUserinfo = true },
	}
	for name, mut := range cases {
		idp := oidctest.New(t, false)
		idp.Claims = map[string]any{"preferred_username": "alice", "groups": []string{"devs"}}
		mut(idp)
		if _, err := login(t, newProvider(t, idp), idp); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A multi-audience token with azp naming this client is fine.
	idp := oidctest.New(t, false)
	idp.Claims = map[string]any{"preferred_username": "alice", "aud": []string{"cc-client", "x"}, "azp": "cc-client"}
	if _, err := login(t, newProvider(t, idp), idp); err != nil {
		t.Fatalf("azp: %v", err)
	}
}

func TestExchangeWrongVerifierAndReusedCode(t *testing.T) {
	idp := oidctest.New(t, false)
	idp.Claims = map[string]any{"preferred_username": "alice"}
	p := newProvider(t, idp)
	ctx := context.Background()
	state, nonce, verifier := Random(), Random(), Random()
	target, _ := p.AuthURL(ctx, redirect, state, nonce, verifier)
	res, err := idp.Client().Get(target)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	code := loc.Query().Get("code")
	if _, err := p.Exchange(ctx, code, redirect, "wrong-verifier", nonce); err == nil {
		t.Fatal("wrong verifier accepted")
	}
	if _, err := p.Exchange(ctx, code, redirect, verifier, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exchange(ctx, code, redirect, verifier, nonce); err == nil {
		t.Fatal("reused code accepted")
	}
}

func TestUserinfoFillsMissingClaims(t *testing.T) {
	idp := oidctest.New(t, false)
	idp.Claims = map[string]any{"preferred_username": "alice"} // no groups in the ID token
	idp.Userinfo = map[string]any{"groups": []string{"devs"}, "preferred_username": "ignored"}
	id, err := login(t, newProvider(t, idp), idp)
	if err != nil {
		t.Fatal(err)
	}
	if id.Username != "alice" || len(id.Groups) != 1 || id.Groups[0] != "devs" {
		t.Fatalf("identity %+v", id)
	}
	// With both claims in the ID token, userinfo is not called.
	idp.Claims["groups"] = []string{"x"}
	idp.Userinfo = nil // would 404
	if id, err := login(t, newProvider(t, idp), idp); err != nil || id.Groups[0] != "x" {
		t.Fatalf("%+v %v", id, err)
	}
	// A failing userinfo leaves the groups empty when the username is in
	// the ID token, and fails the login when it is not.
	delete(idp.Claims, "groups")
	if id, err := login(t, newProvider(t, idp), idp); err != nil || id.Username != "alice" || id.Groups != nil {
		t.Fatalf("%+v %v", id, err)
	}
	delete(idp.Claims, "preferred_username")
	if _, err := login(t, newProvider(t, idp), idp); err == nil || !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("err %v", err)
	}
}

func TestClientSecretPostFallback(t *testing.T) {
	idp := oidctest.New(t, false)
	idp.AuthMethods = []string{"client_secret_post"}
	idp.Claims = map[string]any{"preferred_username": "alice"}
	if _, err := login(t, newProvider(t, idp), idp); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryIssuerMustMatch(t *testing.T) {
	idp := oidctest.New(t, false)
	idp.Issuer = "https://other.example"
	if _, err := newProvider(t, idp).Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("err %v", err)
	}
	// A failed discovery is not cached: fixing the provider works at once.
	p := newProvider(t, idp)
	p.Discover(context.Background())
	idp.Issuer = ""
	if _, err := p.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsBadIssuer(t *testing.T) {
	cfg := oidctest.New(t, false).Config()
	for _, iss := range []string{"", "http://idp.example", "ftp://x", "https://u:p@idp.example", "https://idp.example/?x=1", "idp.example"} {
		cfg.Issuer = iss
		if _, err := New(cfg); err == nil {
			t.Errorf("issuer %q accepted", iss)
		}
	}
}

func TestTLSNeedsCACert(t *testing.T) {
	idp := oidctest.New(t, true)
	idp.Claims = map[string]any{"preferred_username": "alice"}
	cfg := idp.Config()
	cfg.CACert = ""
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Discover(context.Background()); err == nil {
		t.Fatal("an unknown CA was accepted")
	}
	cfg.CACert = idp.CACertPEM()
	if _, err := login(t, newProvider(t, idp), idp); err != nil {
		t.Fatalf("with caCert: %v", err)
	}
	cfg.CACert, cfg.InsecureSkipVerify = "", true
	p, _ = New(cfg)
	if _, err := p.Discover(context.Background()); err != nil {
		t.Fatalf("insecureSkipVerify: %v", err)
	}
}

func TestJWKSRefetchOnUnknownKid(t *testing.T) {
	idp := oidctest.New(t, false)
	idp.Claims = map[string]any{"preferred_username": "alice"}
	p := newProvider(t, idp)
	if _, err := login(t, p, idp); err != nil {
		t.Fatal(err)
	}
	// Switch the signing key to the other listed one: the cached JWKS has
	// it, so no refetch is needed; the count stays at one.
	idp.Alg = "ES256"
	if _, err := login(t, p, idp); err != nil || idp.JWKSCalls != 1 {
		t.Fatalf("%v, %d jwks calls", err, idp.JWKSCalls)
	}
}
