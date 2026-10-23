package ldap

import (
	"errors"
	"fmt"
	"strings"

	"commandcenter/internal/ldap/ber"
)

// Search filter choices (RFC 4511 4.5.1).
const (
	filterAnd      byte = ber.ClassContext | ber.Constructed | 0
	filterOr       byte = ber.ClassContext | ber.Constructed | 1
	filterNot      byte = ber.ClassContext | ber.Constructed | 2
	filterEquality byte = ber.ClassContext | ber.Constructed | 3
	filterPresent  byte = ber.ClassContext | 7
)

// maxFilterDepth bounds the nesting of a filter.
const maxFilterDepth = 16

// EscapeFilter escapes s for use as an assertion value in a search filter
// (RFC 4515 3): '\', '*', '(', ')' and NUL become \5c, \2a, \28, \29, \00.
func EscapeFilter(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\5c`)
		case '*':
			b.WriteString(`\2a`)
		case '(':
			b.WriteString(`\28`)
		case ')':
			b.WriteString(`\29`)
		case 0:
			b.WriteString(`\00`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// UserFilter substitutes the escaped user name for every {user} in filter.
func UserFilter(filter, user string) string {
	return strings.ReplaceAll(filter, "{user}", EscapeFilter(user))
}

// CompileFilter parses an RFC 4515 filter string into its BER encoding. It
// supports '&', '|', '!', equality and presence ("attr=*"); substring,
// ordering, approximate and extensible matches are refused.
func CompileFilter(s string) ([]byte, error) {
	p := &filterParser{s: s}
	out, err := p.filter(0)
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, fmt.Errorf("ldap filter: unexpected %q at offset %d", p.s[p.i:], p.i)
	}
	return out, nil
}

type filterParser struct {
	s string
	i int
}

var errFilterEnd = errors.New("ldap filter: unexpected end")

func (p *filterParser) filter(depth int) ([]byte, error) {
	if depth > maxFilterDepth {
		return nil, fmt.Errorf("ldap filter: nested deeper than %d", maxFilterDepth)
	}
	if p.i >= len(p.s) || p.s[p.i] != '(' {
		return nil, fmt.Errorf("ldap filter: expected '(' at offset %d", p.i)
	}
	p.i++
	if p.i >= len(p.s) {
		return nil, errFilterEnd
	}
	var out []byte
	var err error
	switch p.s[p.i] {
	case '&', '|':
		tag := filterAnd
		if p.s[p.i] == '|' {
			tag = filterOr
		}
		p.i++
		var parts [][]byte
		for p.i < len(p.s) && p.s[p.i] == '(' {
			f, err := p.filter(depth + 1)
			if err != nil {
				return nil, err
			}
			parts = append(parts, f)
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("ldap filter: empty filter list at offset %d", p.i)
		}
		out = ber.TLV(tag, parts...)
	case '!':
		p.i++
		f, err := p.filter(depth + 1)
		if err != nil {
			return nil, err
		}
		out = ber.TLV(filterNot, f)
	default:
		if out, err = p.item(); err != nil {
			return nil, err
		}
	}
	if p.i >= len(p.s) || p.s[p.i] != ')' {
		return nil, fmt.Errorf("ldap filter: expected ')' at offset %d", p.i)
	}
	p.i++
	return out, nil
}

func isAttrChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == ';'
}

func (p *filterParser) item() ([]byte, error) {
	start := p.i
	for p.i < len(p.s) && isAttrChar(p.s[p.i]) {
		p.i++
	}
	attr := p.s[start:p.i]
	if attr == "" {
		return nil, fmt.Errorf("ldap filter: expected an attribute at offset %d", start)
	}
	if p.i >= len(p.s) {
		return nil, errFilterEnd
	}
	if p.s[p.i] != '=' {
		return nil, fmt.Errorf("ldap filter: only '=' matches are supported (offset %d)", p.i)
	}
	p.i++
	vstart := p.i
	for p.i < len(p.s) && p.s[p.i] != ')' {
		if p.s[p.i] == '(' {
			return nil, fmt.Errorf("ldap filter: unescaped '(' at offset %d", p.i)
		}
		p.i++
	}
	raw := p.s[vstart:p.i]
	if raw == "*" {
		return ber.String(filterPresent, attr), nil
	}
	val, err := unescapeValue(raw)
	if err != nil {
		return nil, err
	}
	return ber.TLV(filterEquality, ber.String(ber.TagOctetString, attr), ber.String(ber.TagOctetString, val)), nil
}

// unescapeValue decodes \XX escapes; an unescaped '*' (a substring match) is refused.
func unescapeValue(raw string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch c {
		case '*':
			return "", errors.New("ldap filter: substring matches are not supported")
		case '\\':
			if i+2 >= len(raw) {
				return "", errors.New("ldap filter: incomplete escape")
			}
			hi, ok1 := unhex(raw[i+1])
			lo, ok2 := unhex(raw[i+2])
			if !ok1 || !ok2 {
				return "", errors.New("ldap filter: invalid escape")
			}
			b.WriteByte(hi<<4 | lo)
			i += 2
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
