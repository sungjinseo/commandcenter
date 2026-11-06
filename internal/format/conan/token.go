package conan

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// TokenTTL is how long a login token is valid: a year. Conan keeps the token
// and asks for credentials only when it is refused, which CI that logged in
// once (a runner set up by hand, a login baked into an image) cannot answer;
// a Nexus token never expires, so such CI must not stop within any time it
// lives unchanged. It still ends by itself once forgotten, and sooner when
// it matters: every request looks the user up (a disabled or deleted user is
// refused at once), and a changed password ends a local user's tokens.
const TokenTTL = 365 * 24 * time.Hour

// claims is the signed body of a login token: the owner key of the user
// (auth.Principal.Owner), its expiry (Unix seconds), the audience, which
// keeps it apart from the Docker registry's tokens signed with the same key,
// and the stamp of the local user's password when it was issued (none for a
// user whose password is kept elsewhere, and in a token of an older build,
// which ends within its hour).
type claims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
	Aud string `json:"aud"`
	Pwd string `json:"pwd,omitempty"`
}

const audience = "conan"

// mint signs a token for owner: base64url(JSON) "." base64url(HMAC-SHA256).
func (h *Handler) mint(owner string) string {
	body, _ := json.Marshal(claims{Sub: owner, Exp: h.Now().Add(TokenTTL).Unix(), Aud: audience, Pwd: h.Auth.Snapshot().PasswordStamp(owner)})
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(h.sign(body))
}

func (h *Handler) sign(body []byte) []byte {
	m := hmac.New(sha256.New, h.Key)
	m.Write(body)
	return m.Sum(nil)
}

// VerifyToken checks a Bearer token's signature, audience, expiry and
// password stamp and returns the owner it was issued to. It suits
// auth.Authenticator.Bearers.
func (h *Handler) VerifyToken(tok string) (owner string, ok bool) {
	b64, sig, found := strings.Cut(tok, ".")
	if !found {
		return "", false
	}
	enc := base64.RawURLEncoding
	body, err := enc.DecodeString(b64)
	if err != nil {
		return "", false
	}
	mac, err := enc.DecodeString(sig)
	if err != nil || !hmac.Equal(mac, h.sign(body)) {
		return "", false
	}
	var c claims
	if json.Unmarshal(body, &c) != nil || c.Aud != audience || c.Sub == "" || h.Now().Unix() >= c.Exp {
		return "", false
	}
	if c.Pwd != "" && c.Pwd != h.Auth.Snapshot().PasswordStamp(c.Sub) {
		return "", false
	}
	return c.Sub, true
}
