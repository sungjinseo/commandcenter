// Package logsafe redacts secrets before they reach the logs: tokens in
// request paths and credentials in remote URLs.
package logsafe

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Path hides secrets embedded in request paths (npm token revocation).
func Path(p string) string {
	p = escapeControl(p)
	if i := strings.Index(p, "/-/user/token/"); i >= 0 {
		return p[:i] + "/-/user/token/REDACTED"
	}
	return p
}

// escapeControl writes control characters (NUL, ESC, newline, DEL...) as \xNN so
// a request path cannot forge log lines or make grep treat the log as binary.
func escapeControl(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			fmt.Fprintf(&b, `\x%02x`, c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// URL returns rawurl without userinfo, with every query value replaced by
// "REDACTED" and without a fragment. Text that does not parse is returned as
// scheme-less "REDACTED" so nothing leaks.
func URL(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return "REDACTED"
	}
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	if u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for i, p := range parts {
			if k, _, ok := strings.Cut(p, "="); ok {
				parts[i] = k + "=REDACTED"
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	return u.String()
}

var urlRE = regexp.MustCompile(`https?://[^\s"'<>]+`)

// URLs redacts every http(s) URL in s (see URL). Text without "://" is
// returned unchanged without scanning.
func URLs(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlRE.ReplaceAllStringFunc(s, func(m string) string {
		// A trailing punctuation mark belongs to the sentence, not the URL.
		trim := strings.TrimRight(m, ".,;:)]}")
		return URL(trim) + m[len(trim):]
	})
}

// Err describes err for a log line: a *url.Error is reduced to its operation,
// the URL without credentials and query, and the inner error; any other URL in
// the message is redacted too.
func Err(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		s = strings.Replace(s, ue.Error(), ue.Op+` "`+URL(ue.URL)+`": `+Err(ue.Err), 1)
	}
	return URLs(s)
}
