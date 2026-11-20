// Package sigv4 verifies AWS Signature Version 4 requests as S3 sends them:
// the Authorization header, presigned URLs (the signature in the query) and
// aws-chunked bodies (chunk signatures, trailing checksums). It holds no
// keys: the caller looks the secret up by Request.AccessKey.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Payload hash values of x-amz-content-sha256 other than a hex SHA-256.
const (
	UnsignedPayload          = "UNSIGNED-PAYLOAD"
	StreamingSigned          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	StreamingSignedTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	StreamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
)

const (
	algorithm  = "AWS4-HMAC-SHA256"
	timeFormat = "20060102T150405Z"
	// MaxSkew is how far a request's time may be from the server's.
	MaxSkew = 15 * time.Minute
	// MaxExpires bounds a presigned URL's X-Amz-Expires (7 days).
	MaxExpires = 7 * 24 * time.Hour
)

var (
	ErrNotSigned = errors.New("sigv4: the request is not signed")
	ErrMalformed = errors.New("sigv4: malformed authorization")
	ErrSignature = errors.New("sigv4: the signature does not match")
	ErrExpired   = errors.New("sigv4: the request has expired")
	ErrSkewed    = errors.New("sigv4: the request time is too far from the server's")
	ErrV2        = errors.New("sigv4: signature version 2 is not supported; sign with signature version 4")
)

// Request is the signature a request carries.
type Request struct {
	AccessKey     string
	Date          string // yyyymmdd of the credential scope
	Region        string
	Service       string
	Time          time.Time // X-Amz-Date
	SignedHeaders []string
	Signature     string
	Presigned     bool
	Expires       time.Duration // presigned only
	// PayloadHash is the x-amz-content-sha256 the signature covers: a hex
	// SHA-256, UnsignedPayload or a Streaming* value.
	PayloadHash string
}

// Scope is the credential scope: date/region/service/aws4_request.
func (q *Request) Scope() string {
	return q.Date + "/" + q.Region + "/" + q.Service + "/aws4_request"
}

// Parse reads the signature of r, from its Authorization header or its
// query (a presigned URL). A request with neither is ErrNotSigned.
func Parse(r *http.Request) (*Request, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		return parseHeader(r, h)
	}
	qv := r.URL.Query()
	if qv.Get("X-Amz-Algorithm") != "" || qv.Get("X-Amz-Signature") != "" {
		return parseQuery(qv)
	}
	if qv.Get("AWSAccessKeyId") != "" && qv.Get("Signature") != "" {
		return nil, ErrV2
	}
	return nil, ErrNotSigned
}

func parseHeader(r *http.Request, h string) (*Request, error) {
	scheme, rest, _ := strings.Cut(h, " ")
	if scheme == "AWS" {
		return nil, ErrV2
	}
	if scheme != algorithm {
		return nil, ErrMalformed
	}
	q := &Request{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, ErrMalformed
		}
		switch k {
		case "Credential":
			if err := q.setCredential(v); err != nil {
				return nil, err
			}
		case "SignedHeaders":
			q.SignedHeaders = strings.Split(v, ";")
		case "Signature":
			q.Signature = v
		}
	}
	t, err := requestTime(r)
	if err != nil {
		return nil, err
	}
	q.Time = t
	q.PayloadHash = r.Header.Get("X-Amz-Content-Sha256")
	if q.PayloadHash == "" {
		// S3 requires the header with header signatures; treat a missing
		// one as the hash of an empty body, which Verify then checks.
		q.PayloadHash = emptySHA256
	}
	return q, q.check()
}

func parseQuery(qv url.Values) (*Request, error) {
	if qv.Get("X-Amz-Algorithm") != algorithm {
		return nil, ErrMalformed
	}
	q := &Request{Presigned: true, Signature: qv.Get("X-Amz-Signature")}
	if err := q.setCredential(qv.Get("X-Amz-Credential")); err != nil {
		return nil, err
	}
	q.SignedHeaders = strings.Split(qv.Get("X-Amz-SignedHeaders"), ";")
	t, err := time.Parse(timeFormat, qv.Get("X-Amz-Date"))
	if err != nil {
		return nil, ErrMalformed
	}
	q.Time = t
	secs, err := strconv.ParseInt(qv.Get("X-Amz-Expires"), 10, 64)
	if err != nil || secs < 1 || time.Duration(secs)*time.Second > MaxExpires {
		return nil, ErrMalformed
	}
	q.Expires = time.Duration(secs) * time.Second
	q.PayloadHash = UnsignedPayload
	if v := qv.Get("X-Amz-Content-Sha256"); v != "" {
		q.PayloadHash = v
	}
	return q, q.check()
}

func (q *Request) setCredential(v string) error {
	parts := strings.Split(v, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" || parts[0] == "" {
		return ErrMalformed
	}
	q.AccessKey, q.Date, q.Region, q.Service = parts[0], parts[1], parts[2], parts[3]
	return nil
}

func (q *Request) check() error {
	if q.AccessKey == "" || q.Signature == "" || len(q.SignedHeaders) == 0 || q.SignedHeaders[0] == "" {
		return ErrMalformed
	}
	if q.Time.Format("20060102") != q.Date {
		return ErrMalformed
	}
	return nil
}

func requestTime(r *http.Request) (time.Time, error) {
	if v := r.Header.Get("X-Amz-Date"); v != "" {
		t, err := time.Parse(timeFormat, v)
		if err != nil {
			return time.Time{}, ErrMalformed
		}
		return t, nil
	}
	t, err := http.ParseTime(r.Header.Get("Date"))
	if err != nil {
		return time.Time{}, ErrMalformed
	}
	return t.UTC(), nil
}

// Verify checks the signature of r with secret at now: the time window
// and, for a presigned URL, its expiry. It does not read the body: a hex
// PayloadHash is the caller's to compare with the body's SHA-256, and a
// streaming one is Body's.
func (q *Request) Verify(r *http.Request, secret string, now time.Time) error {
	if q.Presigned {
		if now.Before(q.Time.Add(-MaxSkew)) {
			return ErrSkewed
		}
		if now.After(q.Time.Add(q.Expires)) {
			return ErrExpired
		}
	} else if d := now.Sub(q.Time); d > MaxSkew || d < -MaxSkew {
		return ErrSkewed
	}
	want := q.sign(r, secret)
	if !hmac.Equal([]byte(want), []byte(q.Signature)) {
		return ErrSignature
	}
	return nil
}

func (q *Request) sign(r *http.Request, secret string) string {
	creq := canonicalRequest(r, q.SignedHeaders, q.PayloadHash, q.Presigned)
	sts := algorithm + "\n" + q.Time.Format(timeFormat) + "\n" + q.Scope() + "\n" + hexSHA256([]byte(creq))
	return hex.EncodeToString(hmacSHA256(SigningKey(secret, q.Date, q.Region, q.Service), sts))
}

// SigningKey derives the key that signs a day's requests for one region
// and service.
func SigningKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func canonicalRequest(r *http.Request, signed []string, payload string, presigned bool) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(r.URL))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(r.URL.RawQuery, presigned))
	b.WriteByte('\n')
	for _, h := range signed {
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(headerValue(r, h))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(signed, ";"))
	b.WriteByte('\n')
	b.WriteString(payload)
	return b.String()
}

// canonicalURI is the path, each segment encoded once as S3 does.
func canonicalURI(u *url.URL) string {
	p := u.Path
	if p == "" {
		return "/"
	}
	return Escape(p, false)
}

func canonicalQuery(raw string, presigned bool) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k, _ = url.QueryUnescape(k)
		v, _ = url.QueryUnescape(v)
		if presigned && k == "X-Amz-Signature" {
			continue
		}
		pairs = append(pairs, kv{Escape(k, true), Escape(v, true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

func headerValue(r *http.Request, name string) string {
	switch name {
	case "host":
		return r.Host
	case "content-length":
		if v := r.Header.Get("Content-Length"); v != "" {
			return v
		}
		if r.ContentLength >= 0 {
			return strconv.FormatInt(r.ContentLength, 10)
		}
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return strings.Join(r.TransferEncoding, ",")
		}
	}
	vals := r.Header.Values(name)
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(out, ",")
}

// Escape encodes s as SigV4 does: every byte but A-Z a-z 0-9 - _ . ~ is
// %XX (upper case); "/" too only when slash is true.
func Escape(s string, slash bool) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' && !slash {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexd[c>>4])
		b.WriteByte(hexd[c&15])
	}
	return b.String()
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Sign signs r in place with an Authorization header, as an S3 client
// does: host, x-amz-date, x-amz-content-sha256 and any x-amz-* or
// content-type/content-md5 header r already has. For tests and tools.
func Sign(r *http.Request, accessKey, secret, region string, now time.Time, payloadHash string) {
	now = now.UTC()
	r.Header.Set("X-Amz-Date", now.Format(timeFormat))
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	signed := []string{"host"}
	for k := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "content-md5" {
			signed = append(signed, lk)
		}
	}
	sort.Strings(signed)
	q := &Request{AccessKey: accessKey, Date: now.Format("20060102"), Region: region, Service: "s3",
		Time: now, SignedHeaders: signed, PayloadHash: payloadHash}
	q.Signature = q.sign(r, secret)
	r.Header.Set("Authorization", algorithm+" Credential="+accessKey+"/"+q.Scope()+", SignedHeaders="+
		strings.Join(signed, ";")+", Signature="+q.Signature)
}

// Presign adds a presigned query signature to r (host signed only). For
// tests and tools.
func Presign(r *http.Request, accessKey, secret, region string, now time.Time, expires time.Duration) {
	now = now.UTC()
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	q := &Request{AccessKey: accessKey, Date: now.Format("20060102"), Region: region, Service: "s3",
		Time: now, SignedHeaders: []string{"host"}, PayloadHash: UnsignedPayload, Presigned: true, Expires: expires}
	v := r.URL.Query()
	v.Set("X-Amz-Algorithm", algorithm)
	v.Set("X-Amz-Credential", accessKey+"/"+q.Scope())
	v.Set("X-Amz-Date", now.Format(timeFormat))
	v.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	v.Set("X-Amz-SignedHeaders", "host")
	r.URL.RawQuery = strings.ReplaceAll(v.Encode(), "+", "%20")
	q.Signature = q.sign(r, secret)
	r.URL.RawQuery += "&X-Amz-Signature=" + q.Signature
}
