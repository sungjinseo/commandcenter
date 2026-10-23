// Package ber is the small subset of ASN.1 BER that LDAPv3 needs: definite
// lengths, low tag numbers, INTEGER/ENUMERATED, BOOLEAN and OCTET STRING.
// Everything read is untrusted: lengths are bounded and malformed input is an
// error, never a panic.
package ber

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// MaxLen bounds the length of any element read (16 MiB).
const MaxLen = 16 << 20

// Tag classes and the constructed bit, combined with a tag number (< 31)
// into one identifier byte.
const (
	ClassUniversal   byte = 0x00
	ClassApplication byte = 0x40
	ClassContext     byte = 0x80
	Constructed      byte = 0x20
)

// Universal tags used by LDAP.
const (
	TagBoolean     byte = 0x01
	TagInteger     byte = 0x02
	TagOctetString byte = 0x04
	TagNull        byte = 0x05
	TagEnumerated  byte = 0x0a
	TagSequence    byte = 0x30 // constructed
	TagSet         byte = 0x31 // constructed
)

// ErrMalformed reports invalid BER.
var ErrMalformed = errors.New("ber: malformed element")

// ErrTooLong reports an element longer than MaxLen.
var ErrTooLong = errors.New("ber: element too long")

// ---- encoding ----

func appendLen(b []byte, n int) []byte {
	switch {
	case n < 0x80:
		return append(b, byte(n))
	case n <= 0xff:
		return append(b, 0x81, byte(n))
	case n <= 0xffff:
		return append(b, 0x82, byte(n>>8), byte(n))
	case n <= 0xffffff:
		return append(b, 0x83, byte(n>>16), byte(n>>8), byte(n))
	}
	return append(b, 0x84, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// TLV encodes an element with identifier tag whose content is the
// concatenation of parts.
func TLV(tag byte, parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n+6)
	out = append(out, tag)
	out = appendLen(out, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Int encodes a two's-complement integer with identifier tag.
func Int(tag byte, v int64) []byte {
	var b []byte
	for {
		b = append([]byte{byte(v)}, b...)
		if (v < 0x80 && v >= -0x80) || len(b) == 8 {
			break
		}
		v >>= 8
	}
	return TLV(tag, b)
}

// String encodes an octet string with identifier tag.
func String(tag byte, s string) []byte { return TLV(tag, []byte(s)) }

// Bool encodes a BOOLEAN.
func Bool(v bool) []byte {
	if v {
		return TLV(TagBoolean, []byte{0xff})
	}
	return TLV(TagBoolean, []byte{0x00})
}

// ---- decoding ----

// Parse splits the first element off data: its identifier, its content and
// the remaining bytes.
func Parse(data []byte) (tag byte, content, rest []byte, err error) {
	if len(data) < 2 {
		return 0, nil, nil, ErrMalformed
	}
	tag = data[0]
	if tag&0x1f == 0x1f {
		return 0, nil, nil, fmt.Errorf("%w: high tag number", ErrMalformed)
	}
	n, hdr, err := parseLen(data[1:])
	if err != nil {
		return 0, nil, nil, err
	}
	body := data[1+hdr:]
	if n > len(body) {
		return 0, nil, nil, fmt.Errorf("%w: truncated", ErrMalformed)
	}
	return tag, body[:n], body[n:], nil
}

// parseLen decodes a definite length, returning it and the header size.
func parseLen(b []byte) (n, size int, err error) {
	if len(b) == 0 {
		return 0, 0, ErrMalformed
	}
	first := b[0]
	if first < 0x80 {
		return int(first), 1, nil
	}
	k := int(first & 0x7f)
	if k == 0 {
		return 0, 0, fmt.Errorf("%w: indefinite length", ErrMalformed)
	}
	if k > 4 {
		return 0, 0, ErrTooLong
	}
	if len(b) < 1+k {
		return 0, 0, ErrMalformed
	}
	for _, c := range b[1 : 1+k] {
		n = n<<8 | int(c)
	}
	if n < 0 || n > MaxLen { // n < 0: overflow of a 32-bit int
		return 0, 0, ErrTooLong
	}
	return n, 1 + k, nil
}

// MaxChildren bounds the elements Children returns.
const MaxChildren = 4096

// ErrTooMany reports a constructed element with more than MaxChildren elements.
var ErrTooMany = errors.New("ber: too many elements")

// Children splits a constructed element's content into its elements (at
// most MaxChildren).
func Children(content []byte) ([][]byte, error) {
	var out [][]byte
	for len(content) > 0 {
		if len(out) == MaxChildren {
			return nil, ErrTooMany
		}
		_, _, rest, err := Parse(content)
		if err != nil {
			return nil, err
		}
		out = append(out, content[:len(content)-len(rest)])
		content = rest
	}
	return out, nil
}

// ParseInt decodes the content of an INTEGER or ENUMERATED (at most 8 bytes).
func ParseInt(content []byte) (int64, error) {
	if len(content) == 0 || len(content) > 8 {
		return 0, fmt.Errorf("%w: integer length %d", ErrMalformed, len(content))
	}
	v := int64(int8(content[0]))
	for _, c := range content[1:] {
		v = v<<8 | int64(c)
	}
	return v, nil
}

// Expect parses the first element of data and checks its identifier.
func Expect(data []byte, tag byte) (content, rest []byte, err error) {
	t, c, rest, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	if t != tag {
		return nil, nil, fmt.Errorf("%w: tag 0x%02x, want 0x%02x", ErrMalformed, t, tag)
	}
	return c, rest, nil
}

// ReadElement reads one complete element of at most max content bytes (and
// never more than MaxLen) from r and returns it whole (identifier, length and
// content). The length is checked before anything is allocated, and the
// content is read incrementally, so a lying length costs nothing.
func ReadElement(r *bufio.Reader, max int) ([]byte, error) {
	tag, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if tag&0x1f == 0x1f {
		return nil, fmt.Errorf("%w: high tag number", ErrMalformed)
	}
	first, err := r.ReadByte()
	if err != nil {
		return nil, unexpected(err)
	}
	hdr := []byte{tag, first}
	if first >= 0x80 {
		k := int(first & 0x7f)
		if k == 0 {
			return nil, fmt.Errorf("%w: indefinite length", ErrMalformed)
		}
		if k > 4 {
			return nil, ErrTooLong
		}
		more := make([]byte, k)
		if _, err := io.ReadFull(r, more); err != nil {
			return nil, unexpected(err)
		}
		hdr = append(hdr, more...)
	}
	n, _, err := parseLen(hdr[1:])
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, ErrTooLong
	}
	var buf bytes.Buffer
	buf.Grow(len(hdr) + min(n, 64<<10))
	buf.Write(hdr)
	if _, err := io.CopyN(&buf, r, int64(n)); err != nil {
		return nil, unexpected(err)
	}
	return buf.Bytes(), nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
