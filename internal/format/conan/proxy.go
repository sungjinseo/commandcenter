package conan

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// NewFetcher returns the Fetcher of conan proxies: the generic one over a
// client that logs in to upstream Conan servers.
func NewFetcher(store *repo.Store, maxConcurrent int) *proxy.Fetcher {
	c := proxy.NewClient()
	c.Transport = &upstream{base: c.Transport, tokens: map[string]string{}}
	return proxy.New(store, c, maxConcurrent)
}

// upstream is the client side of a Conan server's login. A request goes out
// as the Fetcher built it; when it carries Basic credentials (the
// repository's remoteAuth user and password), they are first exchanged at
// the remote's v1/users/authenticate for a token, which is cached per host
// and credentials and sent as Bearer instead. A 401 with a cached token
// drops it and logs in again once. A remoteAuth bearer token, or no
// credentials (Conan Center), passes through untouched. Tokens never leave
// this transport.
type upstream struct {
	base   http.RoundTripper
	mu     sync.Mutex
	tokens map[string]string // host + credentials -> token
}

type loginRequestKey struct{}

func (u *upstream) RoundTrip(req *http.Request) (*http.Response, error) {
	creds := req.Header.Get("Authorization")
	if !strings.HasPrefix(creds, "Basic ") || req.Context().Value(loginRequestKey{}) != nil {
		return u.base.RoundTrip(req)
	}
	key := req.URL.Host + "\x00" + creds
	u.mu.Lock()
	tok := u.tokens[key]
	u.mu.Unlock()
	if tok != "" {
		resp, err := u.base.RoundTrip(withBearer(req, tok))
		if err != nil || resp.StatusCode != http.StatusUnauthorized || req.Body != nil && req.Body != http.NoBody {
			return resp, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	tok, err := u.login(req, creds)
	if err != nil {
		slog.Warn("upstream conan login failed; sending the credentials as Basic", "host", req.URL.Host, "err", logsafe.Err(err))
		return u.base.RoundTrip(req)
	}
	u.mu.Lock()
	u.tokens[key] = tok
	u.mu.Unlock()
	return u.base.RoundTrip(withBearer(req, tok))
}

func withBearer(req *http.Request, tok string) *http.Request {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

// login exchanges creds at the remote's v1/users/authenticate (next to
// the /v2/ or /v1/ of the request's path) for a token.
func (u *upstream) login(orig *http.Request, creds string) (string, error) {
	path := orig.URL.Path
	if i := strings.LastIndex(path, "/v2/"); i >= 0 {
		path = path[:i]
	} else if i := strings.LastIndex(path, "/v1/"); i >= 0 {
		path = path[:i]
	}
	au := *orig.URL
	au.Path, au.RawPath, au.RawQuery = path+"/v1/users/authenticate", "", ""
	ctx := context.WithValue(orig.Context(), loginRequestKey{}, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, au.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", proxy.UserAgent)
	req.Header.Set("Authorization", creds)
	resp, err := u.base.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(body))
	if resp.StatusCode != http.StatusOK || tok == "" || strings.ContainsAny(tok, " \n<") {
		return "", repo.Errorf(resp.StatusCode, "authenticate answered %s", resp.Status)
	}
	return tok, nil
}
