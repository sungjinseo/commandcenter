package blob

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// OAuth2 access tokens for the GCS backend: a service account key's signed
// JWT exchanged at its token URI (RFC 7523, Google's server-to-server flow),
// or the metadata server's token (GKE Workload Identity, GCE).

// gcsScope is what the tokens may do: read and write objects.
const gcsScope = "https://www.googleapis.com/auth/devstorage.read_write"

const gcsTokenURI = "https://oauth2.googleapis.com/token"

// tokenFetch gets a new access token and how long it is valid.
type tokenFetch func() (token string, ttl time.Duration, err error)

// serviceAccount is the part of a service account key file a token needs.
type serviceAccount struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	TokenURI     string `json:"token_uri"`
	key          *rsa.PrivateKey
}

// parseServiceAccount reads a service account key file (JSON, RSA key in
// PKCS#8 or PKCS#1 PEM). No error holds the key.
func parseServiceAccount(b []byte) (*serviceAccount, error) {
	var sa serviceAccount
	if err := json.Unmarshal(b, &sa); err != nil {
		return nil, errors.New("not a service account key file: it does not parse as JSON")
	}
	switch {
	case sa.Type != "service_account":
		return nil, errors.New(`not a service account key file: "type" is not "service_account"`)
	case sa.ClientEmail == "":
		return nil, errors.New("the key file has no client_email")
	case sa.TokenURI == "":
		sa.TokenURI = gcsTokenURI
	}
	if u, err := url.Parse(sa.TokenURI); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("the key file's token_uri is not an http(s) URL")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, errors.New("the key file's private_key is not a PEM key")
	}
	var parsed any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("the key file's private_key is a %q PEM block, not a PKCS#8 or PKCS#1 RSA key", block.Type)
	}
	if err != nil {
		return nil, errors.New("the key file's private_key does not parse")
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the key file's private_key is not an RSA key")
	}
	sa.key = k
	return &sa, nil
}

// assertion is the signed JWT (RS256) that asks the token URI for an access
// token for the account, valid for an hour from now.
func (sa *serviceAccount) assertion(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid,omitempty"`
	}{"RS256", "JWT", sa.PrivateKeyID})
	c, _ := json.Marshal(struct {
		Iss   string `json:"iss"`
		Scope string `json:"scope"`
		Aud   string `json:"aud"`
		Iat   int64  `json:"iat"`
		Exp   int64  `json:"exp"`
	}{sa.ClientEmail, gcsScope, sa.TokenURI, now.Unix(), now.Add(time.Hour).Unix()})
	signed := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(nil, sa.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

// fetcher exchanges a fresh assertion for an access token.
func (sa *serviceAccount) fetcher(client *http.Client, now func() time.Time) tokenFetch {
	return func() (string, time.Duration, error) {
		a, err := sa.assertion(now())
		if err != nil {
			return "", 0, err
		}
		resp, err := client.PostForm(sa.TokenURI, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {a}})
		return readToken(resp, err, "token endpoint "+sa.TokenURI)
	}
}

const metadataTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"

// metadataFetcher asks the metadata server (GCE_METADATA_HOST, else the
// link-local address) for the instance's or workload's token, never through
// a proxy.
func metadataFetcher() tokenFetch {
	host := os.Getenv("GCE_METADATA_HOST")
	if host == "" {
		host = "169.254.169.254"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	return func() (string, time.Duration, error) {
		r, err := http.NewRequest(http.MethodGet, "http://"+host+metadataTokenPath, nil)
		if err != nil {
			return "", 0, err
		}
		r.Header.Set("Metadata-Flavor", "Google")
		resp, err := client.Do(r)
		return readToken(resp, err, "metadata server "+host)
	}
}

// defaultCredentials is Google's application default credentials as a
// server has them: the service account key file GOOGLE_APPLICATION_CREDENTIALS
// names, else the metadata server.
func defaultCredentials(client *http.Client) (tokenFetch, error) {
	p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if p == "" {
		return metadataFetcher(), nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS: %w", err)
	}
	sa, err := parseServiceAccount(b)
	if err != nil {
		return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS %s: %w", p, err)
	}
	return sa.fetcher(client, time.Now), nil
}

// readToken reads an OAuth2 token answer from src.
func readToken(resp *http.Response, err error, src string) (string, time.Duration, error) {
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", src, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("%s: %s: %s", src, resp.Status, strings.TrimSpace(string(msg)))
		if !gcsRetryable(resp.StatusCode) {
			return "", 0, permanent{err}
		}
		return "", 0, err
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil || t.AccessToken == "" {
		return "", 0, fmt.Errorf("%s: no access_token in the answer", src)
	}
	ttl := time.Duration(t.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return t.AccessToken, ttl, nil
}

// gcsAuth caches a token and fetches a new one a quarter of its lifetime
// (at most 5 minutes) before it expires. A failed refresh keeps using the
// cached token while it is valid.
// ponytail: fetches under the lock, so requests wait for a slow refresh;
// refresh in the background if that shows up.
type gcsAuth struct {
	fetch        tokenFetch
	now          func() time.Time
	mu           sync.Mutex
	tok          string
	exp, refresh time.Time
}

func newGCSAuth(fetch tokenFetch) *gcsAuth { return &gcsAuth{fetch: fetch, now: time.Now} }

func (a *gcsAuth) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.tok != "" && now.Before(a.refresh) {
		return a.tok, nil
	}
	tok, ttl, err := a.fetch()
	if err != nil {
		if a.tok != "" && now.Before(a.exp) {
			return a.tok, nil
		}
		return "", err
	}
	a.tok, a.exp, a.refresh = tok, now.Add(ttl), now.Add(ttl-min(ttl/4, 5*time.Minute))
	return tok, nil
}
