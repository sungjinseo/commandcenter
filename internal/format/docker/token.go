package docker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"commandcenter/internal/auth"
)

// TokenTTL is how long an issued token is valid.
const TokenTTL = 5 * time.Minute

// scope is one "type:name:actions" entry of a token request or token.
type scope struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// claims is the signed body of a token: subject (the user's owner key, ""
// for anonymous), the service it is for, its expiry (Unix seconds) and the
// access it grants.
type claims struct {
	Sub    string  `json:"sub"`
	Svc    string  `json:"svc"`
	Exp    int64   `json:"exp"`
	Access []scope `json:"access"`
}

// has reports whether the token grants action on the repository name.
func (c *claims) has(typ, name, action string) bool {
	for _, s := range c.Access {
		if s.Type == typ && s.Name == name && slices.Contains(s.Actions, action) {
			return true
		}
	}
	return false
}

var errBadToken = errors.New("invalid or expired token")

// mint signs c: base64url(JSON) "." base64url(HMAC-SHA256).
func (h *Handler) mint(c claims) string {
	body, _ := json.Marshal(c)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(h.sign(body))
}

func (h *Handler) sign(body []byte) []byte {
	m := hmac.New(sha256.New, h.Key)
	m.Write(body)
	return m.Sum(nil)
}

// verify checks tok's signature, expiry and service.
func (h *Handler) verify(tok string) (*claims, error) {
	b64, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, errBadToken
	}
	enc := base64.RawURLEncoding
	body, err := enc.DecodeString(b64)
	if err != nil {
		return nil, errBadToken
	}
	mac, err := enc.DecodeString(sig)
	if err != nil || !hmac.Equal(mac, h.sign(body)) {
		return nil, errBadToken
	}
	var c claims
	if json.Unmarshal(body, &c) != nil || c.Svc != h.service() || h.now().Unix() >= c.Exp {
		return nil, errBadToken
	}
	return &c, nil
}

// service is the token audience: the host of the live baseURL.
func (h *Handler) service() string {
	u, err := url.Parse(h.baseURL())
	if err != nil || u.Host == "" {
		return "commandcenter"
	}
	return u.Host
}

func (h *Handler) baseURL() string {
	if h.BaseURL == nil {
		return ""
	}
	return strings.TrimRight(h.BaseURL(), "/")
}

// parseScope parses "type:name:actions"; a scope of another shape is skipped.
func parseScope(s string) (scope, bool) {
	parts := strings.Split(s, ":")
	if len(parts) < 3 {
		return scope{}, false
	}
	var actions []string
	for _, a := range strings.Split(parts[len(parts)-1], ",") {
		if a != "" {
			actions = append(actions, a)
		}
	}
	return scope{Type: parts[0], Name: strings.Join(parts[1:len(parts)-1], ":"), Actions: actions}, len(actions) > 0
}

// accessFor maps a registry action to the privilege it needs.
func accessFor(action string) (auth.Access, bool) {
	switch action {
	case "pull":
		return auth.Read, true
	case "push":
		return auth.Add, true
	case "delete":
		return auth.Delete, true
	}
	return 0, false
}

// grant intersects a requested scope with what p may do: pull needs read,
// push add, delete delete on the Command Center repository of the name. The
// catalog scope is granted to everyone; the listing filters by privilege.
func (h *Handler) grant(p auth.Principal, s scope) scope {
	out := scope{Type: s.Type, Name: s.Name, Actions: []string{}}
	switch s.Type {
	case "registry":
		if s.Name == "catalog" {
			out.Actions = []string{"*"}
		}
	case "repository":
		repoName, _, _ := strings.Cut(s.Name, "/")
		for _, a := range s.Actions {
			if acc, ok := accessFor(a); ok && h.Auth.Can(p, repoName, acc) {
				out.Actions = append(out.Actions, a)
			}
		}
	}
	return out
}

// token serves GET /v2/token: the caller (Basic, authenticated by the
// server, or anonymous) gets a token for the requested scopes it may use.
func (h *Handler) token(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return Err(http.StatusMethodNotAllowed, CodeUnsupported, "use GET")
	}
	p := auth.FromContext(r.Context())
	c := claims{Sub: p.Owner(), Svc: h.service(), Exp: h.now().Add(TokenTTL).Unix(), Access: []scope{}}
	for _, raw := range r.URL.Query()["scope"] {
		if s, ok := parseScope(raw); ok {
			c.Access = append(c.Access, h.grant(p, s))
		}
	}
	tok := h.mint(c)
	writeJSON(w, http.StatusOK, map[string]any{
		"token": tok, "access_token": tok,
		"expires_in": int(TokenTTL.Seconds()), "issued_at": h.now().UTC().Format(time.RFC3339),
	})
	return nil
}

// challenge is the WWW-Authenticate value of a 401 for r: the token realm,
// the service and the scope the request needs.
func (h *Handler) challenge(r *http.Request) string {
	v := `Bearer realm="` + h.baseURL() + `/v2/token",service="` + h.service() + `"`
	rt, ok := ParseRoute(strings.TrimPrefix(r.URL.Path, "/v2/"))
	switch {
	case !ok:
	case rt.Kind == KindCatalog:
		v += `,scope="registry:catalog:*"`
	case rt.Repo != "":
		v += `,scope="repository:` + rt.Name() + `:` + scopeActions(r, rt) + `"`
	}
	return v
}

// scopeActions is the scope a request needs: a write asks for pull too, as
// docker does (a push reads what it mounts and checks).
func scopeActions(r *http.Request, rt Route) string {
	a := neededAction(r, rt)
	if a == "push" {
		return "pull,push"
	}
	return a
}
