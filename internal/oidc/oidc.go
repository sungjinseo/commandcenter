// Package oidc is a minimal OpenID Connect relying party: discovery, the
// authorization code flow with PKCE, ID-token verification (RS256, ES256)
// and the userinfo call, over the standard library only.
package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
)

const (
	maxBody      = 1 << 20 // every response body is bounded to 1 MiB
	discoveryTTL = time.Hour
	jwksMinFetch = time.Minute // a JWKS refetch for an unknown kid at most this often
	maxKeys      = 50
	clockSkew    = 60 * time.Second
)

// Discovery is the part of the provider's configuration document that is
// used.
type Discovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	Algs                  []string `json:"id_token_signing_alg_values_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

// Identity is what a completed login says about the user.
type Identity struct {
	Username string   // the username claim, as the provider sent it
	Groups   []string // the groups claim; nil when absent
	Email    string
	Name     string
}

// Provider is a client of one issuer under one configuration. Build a new
// one when the configuration changes.
type Provider struct {
	cfg    meta.OIDC
	issuer *url.URL
	client *http.Client

	mu     sync.Mutex
	disc   *Discovery
	discAt time.Time
	keys   []jwk
	keysAt time.Time
}

// New builds a Provider for cfg: its HTTP client goes through the egress
// policy for the issuer's host, trusts the system pool plus cfg.CACert, and
// skips verification only with cfg.InsecureSkipVerify.
func New(cfg meta.OIDC) (*Provider, error) {
	cfg = cfg.Copy()
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if err := meta.ValidateIssuer(cfg.Issuer); err != nil {
		return nil, err
	}
	iss, err := url.Parse(cfg.Issuer)
	if err != nil {
		return nil, err
	}
	var tc *tls.Config
	if strings.TrimSpace(cfg.CACert) != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
			return nil, errors.New("oidc caCert must be one or more PEM certificates")
		}
		tc = &tls.Config{RootCAs: pool}
	}
	if cfg.InsecureSkipVerify {
		if tc == nil {
			tc = &tls.Config{}
		}
		tc.InsecureSkipVerify = true
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = meta.DefaultOIDC().TimeoutSeconds
	}
	return &Provider{cfg: cfg, issuer: iss, client: proxy.NewClientFor(iss, tc)}, nil
}

// Config returns the configuration p was built from (with the issuer
// normalised).
func (p *Provider) Config() meta.OIDC { return p.cfg.Copy() }

func (p *Provider) timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(p.cfg.TimeoutSeconds)*time.Second)
}

// getJSON fetches url (with the headers) and decodes at most maxBody bytes
// of its JSON body into v. A non-2xx answer is an error naming the status.
func (p *Provider) getJSON(ctx context.Context, method, target string, body io.Reader, hdr http.Header, v any) error {
	ctx, cancel := p.timeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "application/json")
	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := readBounded(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		return &StatusError{Status: res.StatusCode, Code: oauthError(b)}
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("the response is not JSON: %w", err)
	}
	return nil
}

// StatusError is a non-2xx answer; Code is the OAuth "error" field of a
// JSON body, if any (never its description, which may be free text).
type StatusError struct {
	Status int
	Code   string
}

func (e *StatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("the provider answered %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("the provider answered %d", e.Status)
}

func oauthError(b []byte) string {
	var v struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &v) != nil || len(v.Error) > 64 || strings.ContainsFunc(v.Error, func(r rune) bool { return r < ' ' || r > '~' }) {
		return ""
	}
	return v.Error
}

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, errors.New("the response is larger than 1 MiB")
	}
	return b, nil
}

// Discover returns the provider's configuration, fetched from the issuer's
// well-known URL and cached for an hour; a failed fetch is not cached. The
// document's issuer must equal the configured one.
func (p *Provider) Discover(ctx context.Context) (*Discovery, error) {
	p.mu.Lock()
	if p.disc != nil && time.Since(p.discAt) < discoveryTTL {
		d := *p.disc
		p.mu.Unlock()
		return &d, nil
	}
	p.mu.Unlock()
	var d Discovery
	if err := p.getJSON(ctx, http.MethodGet, p.cfg.Issuer+"/.well-known/openid-configuration", nil, nil, &d); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if strings.TrimRight(d.Issuer, "/") != p.cfg.Issuer {
		return nil, fmt.Errorf("discovery: the document's issuer %q is not the configured issuer", d.Issuer)
	}
	for _, e := range []struct{ name, v string }{{"authorization_endpoint", d.AuthorizationEndpoint}, {"token_endpoint", d.TokenEndpoint}, {"jwks_uri", d.JWKSURI}} {
		if u, err := url.Parse(e.v); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("discovery: %s is missing or not an absolute URL", e.name)
		}
	}
	if d.UserinfoEndpoint != "" {
		if u, err := url.Parse(d.UserinfoEndpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			d.UserinfoEndpoint = "" // not usable; the ID token must carry the claims
		}
	}
	p.mu.Lock()
	p.disc, p.discAt = &d, time.Now()
	p.mu.Unlock()
	out := d
	return &out, nil
}

// Random returns 32 random bytes in base64url: a state, nonce or PKCE
// verifier.
func Random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthURL builds the authorization request URL for redirectURI with the
// state, nonce and the S256 challenge of verifier.
func (p *Provider) AuthURL(ctx context.Context, redirectURI, state, nonce, verifier string) (string, error) {
	d, err := p.Discover(ctx)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.cfg.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(p.cfg.Scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	u, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	if u.RawQuery != "" {
		u.RawQuery += "&" + q.Encode()
	} else {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// Exchange redeems code at the token endpoint, verifies the ID token
// (signature, issuer, audience, expiry, nonce) and returns the identity.
// When the ID token lacks the username or groups claim and the provider
// has a userinfo endpoint, userinfo fills them in.
func (p *Provider) Exchange(ctx context.Context, code, redirectURI, verifier, nonce string) (Identity, error) {
	d, err := p.Discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	hdr := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	switch {
	case p.cfg.ClientSecret == "":
		form.Set("client_id", p.cfg.ClientID) // a public client: PKCE alone
	case len(d.TokenAuthMethods) > 0 && !contains(d.TokenAuthMethods, "client_secret_basic"):
		form.Set("client_id", p.cfg.ClientID)
		form.Set("client_secret", p.cfg.ClientSecret)
	default:
		// RFC 6749 2.3.1: the id and secret are form-encoded inside Basic.
		hdr.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
			[]byte(url.QueryEscape(p.cfg.ClientID)+":"+url.QueryEscape(p.cfg.ClientSecret))))
	}
	var tok struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := p.getJSON(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()), hdr, &tok); err != nil {
		return Identity{}, fmt.Errorf("token exchange: %w", err)
	}
	if tok.IDToken == "" {
		return Identity{}, errors.New("token exchange: the response has no id_token")
	}
	claims, err := p.verifyIDToken(ctx, tok.IDToken, nonce)
	if err != nil {
		return Identity{}, fmt.Errorf("id token: %w", err)
	}
	_, hasUser := claims[p.cfg.UsernameClaim]
	_, hasGroups := claims[p.cfg.GroupsClaim]
	if (!hasUser || !hasGroups) && d.UserinfoEndpoint != "" && tok.AccessToken != "" && (tok.TokenType == "" || strings.EqualFold(tok.TokenType, "Bearer")) {
		// A userinfo failure only matters when the username is still
		// missing: without groups the user simply maps to fewer roles.
		var info map[string]any
		h := http.Header{"Authorization": {"Bearer " + tok.AccessToken}}
		err := p.getJSON(ctx, http.MethodGet, d.UserinfoEndpoint, nil, h, &info)
		if err == nil {
			if sub, _ := info["sub"].(string); sub != "" && sub != claims["sub"] {
				err = errors.New("its sub is not the ID token's")
			}
		}
		if err != nil && !hasUser {
			return Identity{}, fmt.Errorf("userinfo: %w", err)
		}
		if err == nil {
			for k, v := range info {
				if _, ok := claims[k]; !ok {
					claims[k] = v
				}
			}
		}
	}
	id := Identity{}
	id.Username, _ = claims[p.cfg.UsernameClaim].(string)
	if id.Username == "" {
		return Identity{}, fmt.Errorf("the %q claim is missing or not a string", p.cfg.UsernameClaim)
	}
	switch g := claims[p.cfg.GroupsClaim].(type) {
	case []any:
		id.Groups = []string{}
		for _, x := range g {
			if s, ok := x.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	case string:
		id.Groups = []string{g}
	}
	id.Email, _ = claims["email"].(string)
	id.Name, _ = claims["name"].(string)
	return id, nil
}

// verifyIDToken checks raw's signature against the provider's keys and its
// claims against the configuration, and returns the claims.
func (p *Provider) verifyIDToken(ctx context.Context, raw, nonce string) (map[string]any, error) {
	t, err := parseJWS(raw)
	if err != nil {
		return nil, err
	}
	key, err := p.keyFor(ctx, t.header, false)
	if err != nil {
		return nil, err
	}
	if err := t.verify(key); err != nil {
		return nil, err
	}
	var c struct {
		Iss   string          `json:"iss"`
		Sub   string          `json:"sub"`
		Aud   json.RawMessage `json:"aud"`
		Azp   string          `json:"azp"`
		Exp   int64           `json:"exp"`
		Iat   int64           `json:"iat"`
		Nonce string          `json:"nonce"`
	}
	if err := json.Unmarshal(t.payload, &c); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	now := time.Now()
	auds := audiences(c.Aud)
	switch {
	case strings.TrimRight(c.Iss, "/") != p.cfg.Issuer:
		return nil, errors.New("wrong issuer")
	case !contains(auds, p.cfg.ClientID):
		return nil, errors.New("the audience is not this client")
	case len(auds) > 1 && c.Azp != p.cfg.ClientID:
		return nil, errors.New("azp is not this client")
	case c.Exp == 0 || now.After(time.Unix(c.Exp, 0).Add(clockSkew)):
		return nil, errors.New("expired")
	case c.Iat != 0 && time.Unix(c.Iat, 0).After(now.Add(clockSkew)):
		return nil, errors.New("issued in the future")
	case c.Nonce != nonce:
		return nil, errors.New("wrong nonce")
	case c.Sub == "":
		return nil, errors.New("no sub")
	}
	var claims map[string]any
	if err := json.Unmarshal(t.payload, &claims); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	return claims, nil
}

func audiences(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	json.Unmarshal(raw, &many)
	return many
}

// keyFor returns the key the header names: by kid when it has one, else
// the only key of the algorithm's type. An unknown kid refetches the JWKS
// once (at most once a minute).
func (p *Provider) keyFor(ctx context.Context, h jwsHeader, refetched bool) (crypto.PublicKey, error) {
	keys, err := p.jwks(ctx, refetched)
	if err != nil {
		return nil, err
	}
	var candidates []jwk
	for _, k := range keys {
		if k.Kty == h.kty() && (h.Kid == "" || k.Kid == h.Kid) && (k.Use == "" || k.Use == "sig") && (k.Alg == "" || k.Alg == h.Alg) {
			candidates = append(candidates, k)
		}
	}
	if len(candidates) == 1 {
		return candidates[0].public()
	}
	if len(candidates) == 0 && !refetched {
		return p.keyFor(ctx, h, true)
	}
	if len(candidates) == 0 {
		return nil, errors.New("no key for the token's kid")
	}
	return nil, errors.New("the token names no kid and the provider has several keys")
}

// jwks returns the cached keys, fetched when missing or when refresh is
// set and the last fetch is older than jwksMinFetch.
func (p *Provider) jwks(ctx context.Context, refresh bool) ([]jwk, error) {
	p.mu.Lock()
	keys, at := p.keys, p.keysAt
	p.mu.Unlock()
	if keys != nil && (!refresh || time.Since(at) < jwksMinFetch) {
		return keys, nil
	}
	d, err := p.Discover(ctx)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := p.getJSON(ctx, http.MethodGet, d.JWKSURI, nil, nil, &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	if len(set.Keys) > maxKeys {
		return nil, fmt.Errorf("jwks: more than %d keys", maxKeys)
	}
	if set.Keys == nil {
		set.Keys = []jwk{}
	}
	p.mu.Lock()
	p.keys, p.keysAt = set.Keys, time.Now()
	p.mu.Unlock()
	return set.Keys, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
