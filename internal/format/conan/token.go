package conan

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// TokenTTL is how long a login token is valid. Conan clients log in again
// (with their stored credentials, or by asking) when a token is refused.
const TokenTTL = time.Hour

// claims is the signed body of a login token: the owner key of the user
// (auth.Principal.Owner), its expiry (Unix seconds) and the audience, which
// keeps it apart from the Docker registry's tokens signed with the same key.
type claims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
	Aud string `json:"aud"`
}

const audience = "conan"

// mint signs a token for owner: base64url(JSON) "." base64url(HMAC-SHA256).
func (h *Handler) mint(owner string) string {
	body, _ := json.Marshal(claims{Sub: owner, Exp: h.Now().Add(TokenTTL).Unix(), Aud: audience})
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(h.sign(body))
}

func (h *Handler) sign(body []byte) []byte {
	m := hmac.New(sha256.New, h.Key)
	m.Write(body)
	return m.Sum(nil)
}

// VerifyToken checks a Bearer token's signature, audience and expiry and
// returns the owner it was issued to. It suits auth.Authenticator.Bearers.
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
	return c.Sub, true
}
