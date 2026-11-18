package s3api

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4 (AWS docs: "Authenticating Requests (AWS
// Signature Version 4)" in the S3 API reference, and "Create a signed AWS
// API request" in the IAM guide): the Authorization header form and the
// presigned query form. Signature Version 2 is not accepted.

const (
	algorithm     = "AWS4-HMAC-SHA256"
	timeFormat    = "20060102T150405Z"
	dateFormat    = "20060102"
	emptySHA256   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedBody  = "UNSIGNED-PAYLOAD"
	streamSigned  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamSignedT = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamUnsignT = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"

	// maxSkew is how far a signed request's time may be from ours (AWS: 15
	// minutes).
	maxSkew = 15 * time.Minute
	// maxPresign bounds X-Amz-Expires (AWS: seven days).
	maxPresign = 7 * 24 * time.Hour
)

// credential is the parsed Credential element: key id and scope.
type credential struct {
	key, date, region, service string
}

func (c credential) scope() string {
	return c.date + "/" + c.region + "/" + c.service + "/aws4_request"
}

func parseCredential(v string) (credential, bool) {
	p := strings.Split(v, "/")
	if len(p) != 5 || p[0] == "" || p[4] != "aws4_request" {
		return credential{}, false
	}
	return credential{key: p[0], date: p[1], region: p[2], service: p[3]}, true
}

// sigRequest is what a request's signature covers, parsed from its
// Authorization header or its query.
type sigRequest struct {
	cred      credential
	signed    []string // signed header names, lower case, sorted
	signature string
	at        time.Time
	expires   time.Duration // presigned only
	presigned bool
	payload   string // x-amz-content-sha256, or UNSIGNED-PAYLOAD
}

// signed reports whether r carries a signature of either form, or a
// Signature Version 2 query (which is refused). A request with none is
// anonymous.
func signed(r *http.Request) bool {
	q := r.URL.Query()
	return r.Header.Get("Authorization") != "" || q.Has("X-Amz-Algorithm") || q.Has("AWSAccessKeyId")
}

// parseSigned reads r's signature elements. It checks their form, not the
// signature.
func parseSigned(r *http.Request) (sigRequest, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		return parseHeader(r, h)
	}
	return parseQuery(r)
}

func parseHeader(r *http.Request, h string) (sigRequest, error) {
	alg, rest, _ := strings.Cut(h, " ")
	if alg != algorithm {
		return sigRequest{}, errf(http.StatusBadRequest, "InvalidRequest", "only AWS Signature Version 4 (%s) is supported", algorithm)
	}
	var s sigRequest
	var haveCred, haveHeaders bool
	for _, f := range strings.Split(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		switch k {
		case "Credential":
			s.cred, haveCred = parseCredential(v)
		case "SignedHeaders":
			s.signed, haveHeaders = strings.Split(v, ";"), v != ""
		case "Signature":
			s.signature = v
		}
	}
	if !haveCred || !haveHeaders || s.signature == "" {
		return sigRequest{}, errf(http.StatusBadRequest, "AuthorizationHeaderMalformed", "the Authorization header needs Credential, SignedHeaders and Signature")
	}
	s.payload = r.Header.Get("X-Amz-Content-Sha256")
	if s.payload == "" {
		return sigRequest{}, errf(http.StatusBadRequest, "InvalidRequest", "a signed request needs the x-amz-content-sha256 header")
	}
	var err error
	if v := r.Header.Get("X-Amz-Date"); v != "" {
		s.at, err = time.Parse(timeFormat, v)
	} else {
		s.at, err = http.ParseTime(r.Header.Get("Date"))
	}
	if err != nil {
		return sigRequest{}, errf(http.StatusForbidden, "AccessDenied", "a signed request needs a valid X-Amz-Date or Date header")
	}
	return s, nil
}

func parseQuery(r *http.Request) (sigRequest, error) {
	q := r.URL.Query()
	bad := func(msg string) error {
		return errf(http.StatusBadRequest, "AuthorizationQueryParametersError", "%s", msg)
	}
	if q.Get("X-Amz-Algorithm") != algorithm {
		return sigRequest{}, bad("X-Amz-Algorithm must be " + algorithm)
	}
	s := sigRequest{presigned: true, signature: q.Get("X-Amz-Signature")}
	var ok bool
	if s.cred, ok = parseCredential(q.Get("X-Amz-Credential")); !ok {
		return sigRequest{}, bad("X-Amz-Credential must be <key>/<date>/<region>/s3/aws4_request")
	}
	if v := q.Get("X-Amz-SignedHeaders"); v != "" {
		s.signed = strings.Split(v, ";")
	}
	if len(s.signed) == 0 || s.signature == "" {
		return sigRequest{}, bad("a presigned URL needs X-Amz-SignedHeaders and X-Amz-Signature")
	}
	var err error
	if s.at, err = time.Parse(timeFormat, q.Get("X-Amz-Date")); err != nil {
		return sigRequest{}, bad("X-Amz-Date must be a time like 20060102T150405Z")
	}
	secs, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || secs < 1 || time.Duration(secs)*time.Second > maxPresign {
		return sigRequest{}, bad("X-Amz-Expires must be between 1 and 604800 seconds")
	}
	s.expires = time.Duration(secs) * time.Second
	if s.payload = r.Header.Get("X-Amz-Content-Sha256"); s.payload == "" {
		s.payload = unsignedBody
	}
	return s, nil
}

// checkTime refuses a request whose time is off ours by more than maxSkew,
// or a presigned URL that expired.
func (s sigRequest) checkTime(now time.Time) error {
	if s.presigned {
		if now.After(s.at.Add(s.expires)) {
			return errf(http.StatusForbidden, "AccessDenied", "the presigned URL expired")
		}
		if s.at.After(now.Add(maxSkew)) {
			return errf(http.StatusForbidden, "AccessDenied", "the presigned URL is not valid yet")
		}
		return nil
	}
	if d := now.Sub(s.at); d > maxSkew || d < -maxSkew {
		return errf(http.StatusForbidden, "RequestTimeTooSkewed", "the request time differs from the server time by more than 15 minutes")
	}
	return nil
}

// signingKey derives the key of a scope from the secret.
func signingKey(secret string, c credential) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), c.date)
	k = hmacSHA256(k, c.region)
	k = hmacSHA256(k, c.service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// expected computes the signature s should carry under key.
func (s sigRequest) expected(r *http.Request, key []byte) string {
	return hex.EncodeToString(hmacSHA256(key, stringToSign(s.at, s.cred.scope(), canonicalRequest(r, s))))
}

func stringToSign(at time.Time, scope, canonical string) string {
	return algorithm + "\n" + at.UTC().Format(timeFormat) + "\n" + scope + "\n" + sha256Hex(canonical)
}

// canonicalRequest is the AWS canonical request of r under s.
func canonicalRequest(r *http.Request, s sigRequest) string {
	var b strings.Builder
	b.WriteString(r.Method + "\n" + canonicalURI(r.URL.Path) + "\n" + canonicalQuery(r.URL.Query(), s.presigned) + "\n")
	for _, name := range s.signed {
		b.WriteString(name + ":" + headerValue(r, name) + "\n")
	}
	b.WriteString("\n" + strings.Join(s.signed, ";") + "\n" + s.payload)
	return b.String()
}

// canonicalURI encodes the path as S3 clients do: each byte but the
// unreserved characters and "/", once (S3 does not encode it twice, unlike
// the other AWS services).
func canonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	return uriEncode(p, false)
}

// canonicalQuery sorts the encoded parameters by name, then value; a
// presigned URL's signature is not part of what it signs.
func canonicalQuery(q url.Values, presigned bool) string {
	var pairs [][2]string
	for k, vs := range q {
		if presigned && k == "X-Amz-Signature" {
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, [2]string{uriEncode(k, true), uriEncode(v, true)})
		}
	}
	slices.SortFunc(pairs, func(a, b [2]string) int { return cmp.Or(strings.Compare(a[0], b[0]), strings.Compare(a[1], b[1])) })
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p[0] + "=" + p[1]
	}
	return strings.Join(out, "&")
}

// uriEncode percent-encodes every byte except A-Z a-z 0-9 - . _ ~, and "/"
// unless slash.
func uriEncode(s string, slash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' || c == '/' && !slash {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

// headerValue is the canonical value of a signed header: its values joined
// by commas, each trimmed with inner runs of spaces made one. Go's server
// keeps some headers out of r.Header: Host, and Expect, which it answers
// itself (MinIO's source notes the same of aws-cli's signed Expect).
func headerValue(r *http.Request, name string) string {
	vs := r.Header.Values(name)
	switch {
	case name == "host":
		return r.Host
	case len(vs) > 0:
	case name == "expect":
		return "100-continue"
	case name == "content-length" && r.ContentLength >= 0:
		return strconv.FormatInt(r.ContentLength, 10)
	case name == "transfer-encoding":
		return strings.Join(r.TransferEncoding, ",")
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(out, ",")
}
