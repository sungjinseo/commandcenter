package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// NewFetcher returns the Fetcher of docker proxies: the generic one over a
// client that speaks the Distribution token flow to upstream registries.
func NewFetcher(store *repo.Store, maxConcurrent int) *proxy.Fetcher {
	c := proxy.NewClient()
	c.Transport = &upstream{base: c.Transport, tokens: map[string]upstreamToken{}, scopes: map[string]string{}}
	return proxy.New(store, c, maxConcurrent)
}

// upstream is the client side of the Distribution token flow. A request
// goes out as the Fetcher built it (its Authorization, when the repository
// has remoteAuth, is Basic for the remote host). On a 401 whose
// WWW-Authenticate is a Bearer challenge, a token is fetched from the
// challenge's realm with those credentials, or anonymously, and the request
// is repeated with it. Tokens are cached per realm, service and scope until
// they expire, and the scope a registry name challenged with is remembered
// so later requests for the name carry the token at once. Tokens and
// credentials never leave this transport: the Fetcher stores bodies, not
// headers, and clients of this registry get its own tokens.
//
// A tags list comes back whole: the pages a registry splits it into are
// fetched and merged here (allTags), so the Fetcher caches one document.
type upstream struct {
	base http.RoundTripper

	mu     sync.Mutex
	tokens map[string]upstreamToken // by realm, service, scope and credentials
	scopes map[string]string        // by host, registry name and credentials: the token key
	// ponytail: pruned of expired tokens on insert only; fine for the
	// handful of scopes a proxy pulls.
}

type upstreamToken struct {
	value string
	exp   time.Time
}

type tokenRequestKey struct{}

// errTokenDenied is a realm's 401 or 403: the credentials are refused.
var errTokenDenied = errors.New("token endpoint refused the credentials")

func (u *upstream) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Context().Value(tokenRequestKey{}) != nil {
		return u.base.RoundTrip(req)
	}
	if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/tags/list") {
		return u.roundTrip(req)
	}
	// Never conditional: a 304 for the first page says nothing about the
	// later ones, and would keep a list cached before pages were followed.
	req = req.Clone(req.Context())
	req.Header.Del("If-None-Match")
	req.Header.Del("If-Modified-Since")
	resp, err := u.roundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	return u.allTags(req, resp)
}

// roundTrip sends req with the token flow.
func (u *upstream) roundTrip(req *http.Request) (*http.Response, error) {
	// Keyed by the credentials too: two repositories with different
	// remoteAuth for one upstream never share a token.
	creds := req.Header.Get("Authorization")
	scopeKey := req.URL.Host + "\x00" + registryName(req.URL.Path) + "\x00" + creds
	if tok := u.cached(scopeKey); tok != "" {
		req = withBearer(req, tok)
	}
	resp, err := u.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || req.Body != nil && req.Body != http.NoBody {
		return resp, err
	}
	ch, ok := parseChallenge(resp.Header.Values("WWW-Authenticate"))
	if !ok {
		return resp, nil
	}
	tok, err := u.fetchToken(req, ch, creds)
	if errors.Is(err, errTokenDenied) {
		return resp, nil // the realm refused the credentials: the registry's 401 stands
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		// Any other failure (realm down, timeout, bad realm) is a remote
		// failure, not the registry's 401: that would be negative-cached as
		// not found (DeniedIsNotFound) for the rest of negativeCacheTTL.
		slog.Warn("upstream registry token", "host", req.URL.Host, "err", logsafe.Err(err))
		return nil, fmt.Errorf("upstream token: %w", err)
	}
	u.remember(scopeKey, ch.key()+"\x00"+creds, tok)
	return u.base.RoundTrip(withBearer(req, tok.value))
}

// maxTagPages bounds the pages of one tags list.
// ponytail: 500 pages, and the pages together are bounded by maxManifest
// (about 200,000 tags), as the one page was; raise both for a longer list.
const maxTagPages = 500

// nextLinkRE finds the target of a Link header's rel="next" entry.
var nextLinkRE = regexp.MustCompile(`<([^>]*)>[^<]*\brel="?next"?`)

func nextLink(resp *http.Response) string {
	if m := nextLinkRE.FindStringSubmatch(strings.Join(resp.Header.Values("Link"), ", ")); m != nil {
		return m[1]
	}
	return ""
}

// allTags follows the Link: rel="next" of a tags list answer (quay.io and
// ghcr.io answer 100 tags at a time) and returns the answer with the tags
// of every page and without the first page's validators. An answer without
// the link is returned as it is. The pages stay on the registry's host,
// where the request's credentials and token belong, and a page that fails
// fails the list: a part of it is never cached as the whole.
func (u *upstream) allTags(req *http.Request, first *http.Response) (*http.Response, error) {
	next := nextLink(first)
	if next == "" {
		return first, nil
	}
	type list struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	var all list
	budget := int64(maxManifest)
	for page, resp := 1, first; ; page++ {
		var doc list
		body := &io.LimitedReader{R: resp.Body, N: budget}
		err := json.NewDecoder(body).Decode(&doc)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("upstream tags list, page %d: %w", page, err)
		}
		budget = body.N
		if page == 1 {
			all.Name = doc.Name
		}
		all.Tags = append(all.Tags, doc.Tags...)
		if next == "" {
			break
		}
		target, err := req.URL.Parse(next)
		if err != nil || target.Scheme != req.URL.Scheme || target.Host != req.URL.Host {
			return nil, fmt.Errorf("upstream tags list, page %d: the next page is on another host", page)
		}
		if page == maxTagPages {
			return nil, fmt.Errorf("upstream tags list has more than %d pages", maxTagPages)
		}
		pageReq := req.Clone(req.Context())
		pageReq.URL = target
		if resp, err = u.roundTrip(pageReq); err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("upstream tags list, page %d: %s", page+1, resp.Status)
		}
		next = nextLink(resp)
	}
	body, _ := json.Marshal(all)
	for _, k := range []string{"Link", "ETag", "Last-Modified", "Content-Length"} {
		first.Header.Del(k)
	}
	first.Body, first.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	return first, nil
}

func withBearer(req *http.Request, tok string) *http.Request {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

// registryName is the name a registry path addresses: what follows /v2/ up
// to the manifests, blobs or tags segment.
func registryName(path string) string {
	_, rest, _ := strings.Cut(path, "/v2/")
	for _, sep := range []string{"/manifests/", "/blobs/", "/tags/list"} {
		if i := strings.Index(rest, sep); i >= 0 {
			rest = rest[:i]
		}
	}
	return rest
}

// challenge is a parsed Bearer challenge.
type challenge struct {
	realm, service, scope string
}

func (c challenge) key() string { return c.realm + "\x00" + c.service + "\x00" + c.scope }

// parseChallenge finds the Bearer challenge among WWW-Authenticate values
// and its realm, service and scope parameters (quoted values may hold
// commas: scope="repository:x:pull,push").
func parseChallenge(values []string) (challenge, bool) {
	for _, v := range values {
		params, ok := strings.CutPrefix(strings.TrimSpace(v), "Bearer ")
		if !ok {
			continue
		}
		var c challenge
		for params != "" {
			key, rest, found := strings.Cut(params, "=")
			if !found {
				break
			}
			var val string
			if strings.HasPrefix(rest, `"`) {
				val, rest, _ = strings.Cut(rest[1:], `"`)
			} else {
				val, rest, _ = strings.Cut(rest, ",")
			}
			rest = strings.TrimLeft(rest, ", ")
			switch strings.TrimSpace(key) {
			case "realm":
				c.realm = val
			case "service":
				c.service = val
			case "scope":
				c.scope = val
			}
			params = rest
		}
		return c, c.realm != ""
	}
	return challenge{}, false
}

func (u *upstream) cached(scopeKey string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if t, ok := u.tokens[u.scopes[scopeKey]]; ok && time.Now().Before(t.exp) {
		return t.value
	}
	return ""
}

func (u *upstream) remember(scopeKey, tokenKey string, tok upstreamToken) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, t := range u.tokens {
		if !time.Now().Before(t.exp) {
			delete(u.tokens, k)
		}
	}
	u.tokens[tokenKey] = tok
	u.scopes[scopeKey] = tokenKey
}

// fetchToken gets a token for ch from its realm, with the Basic credentials
// of the failed request when it had any. The realm must be https unless the
// registry itself is plain http (a test or internal registry); it goes
// through the same egress policy as the registry.
func (u *upstream) fetchToken(orig *http.Request, ch challenge, creds string) (upstreamToken, error) {
	realm, err := url.Parse(ch.realm)
	if err != nil || realm.Host == "" || realm.Scheme != "https" && (realm.Scheme != "http" || orig.URL.Scheme != "http") {
		return upstreamToken{}, errors.New("challenge realm is not an https URL")
	}
	q := realm.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	if ch.scope != "" {
		q.Set("scope", ch.scope)
	}
	realm.RawQuery = q.Encode()
	ctx := context.WithValue(orig.Context(), tokenRequestKey{}, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return upstreamToken{}, err
	}
	req.Header.Set("User-Agent", proxy.UserAgent)
	if strings.HasPrefix(creds, "Basic ") {
		req.Header.Set("Authorization", creds)
	}
	resp, err := u.base.RoundTrip(req)
	if err != nil {
		return upstreamToken{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return upstreamToken{}, fmt.Errorf("%w: %s", errTokenDenied, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return upstreamToken{}, errors.New("token endpoint answered " + resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return upstreamToken{}, err
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return upstreamToken{}, errors.New("token endpoint answered without a token")
	}
	if body.ExpiresIn <= 0 {
		body.ExpiresIn = 60 // the spec's default
	}
	return upstreamToken{value: tok, exp: time.Now().Add(time.Duration(body.ExpiresIn)*time.Second - 10*time.Second)}, nil
}
