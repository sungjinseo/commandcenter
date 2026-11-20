package sigv4

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
)

var (
	ErrChunkSignature  = errors.New("sigv4: a chunk signature does not match")
	ErrTrailerChecksum = errors.New("sigv4: the trailing checksum does not match the body")
	ErrChunkFormat     = errors.New("sigv4: malformed aws-chunked body")
	ErrDecodedLength   = errors.New("sigv4: the body length differs from x-amz-decoded-content-length")
)

// Streaming reports whether the payload hash names an aws-chunked body.
func Streaming(payloadHash string) bool {
	return strings.HasPrefix(payloadHash, "STREAMING-")
}

// Body returns the body of r as the client meant it: an aws-chunked body
// decoded, its chunk signatures and trailing checksum checked as it is
// read (a mismatch is a read error, after the bad chunk's bytes, so the
// caller must not commit what it read until EOF). Any other body is
// returned as it is.
func (q *Request) Body(r *http.Request, secret string) (io.ReadCloser, error) {
	if !Streaming(q.PayloadHash) {
		return r.Body, nil
	}
	d := &chunked{src: bufio.NewReaderSize(r.Body, 64<<10), closer: r.Body, decoded: -1}
	switch q.PayloadHash {
	case StreamingSigned, StreamingSignedTrailer:
		d.signed = true
		d.key = SigningKey(secret, q.Date, q.Region, q.Service)
		d.prefix = q.Time.Format(timeFormat) + "\n" + q.Scope() + "\n"
		d.prev = q.Signature
	case StreamingUnsignedTrailer:
	default:
		return nil, ErrMalformed
	}
	if q.PayloadHash != StreamingSigned {
		name := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Amz-Trailer")))
		if name != "" {
			h := NewChecksum(name)
			if h == nil {
				return nil, ErrMalformed
			}
			d.trailer, d.sum = name, h
		}
	}
	if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, ErrMalformed
		}
		d.decoded = n
	}
	return d, nil
}

type chunked struct {
	src     *bufio.Reader
	closer  io.Closer
	signed  bool
	key     []byte
	prefix  string // "<time>\n<scope>\n"
	prev    string // the previous signature
	trailer string // the checksum header the trailer holds ("" for none)
	sum     hash.Hash
	decoded int64 // x-amz-decoded-content-length, -1 when not sent

	left  int64  // bytes left in the current chunk
	sig   string // the current chunk's signature
	chunk hash.Hash
	total int64
	err   error
	done  bool
}

const maxLine = 4096

func (c *chunked) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.left == 0 {
		if c.done {
			return 0, io.EOF
		}
		if err := c.next(); err != nil {
			c.err = err
			return 0, err
		}
		if c.done {
			return 0, io.EOF
		}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.src.Read(p)
	if n > 0 {
		c.left -= int64(n)
		c.total += int64(n)
		if c.chunk != nil {
			c.chunk.Write(p[:n])
		}
		if c.sum != nil {
			c.sum.Write(p[:n])
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		c.err = err
		return n, err
	}
	if c.left == 0 {
		if err := c.endChunk(); err != nil {
			c.err = err
			return n, err
		}
	}
	return n, nil
}

// next reads a chunk header; a zero-length chunk ends the body.
func (c *chunked) next() error {
	line, err := c.line()
	if err != nil {
		return err
	}
	size, ext, _ := strings.Cut(line, ";")
	n, err := strconv.ParseInt(strings.TrimSpace(size), 16, 64)
	if err != nil || n < 0 {
		return ErrChunkFormat
	}
	c.sig = ""
	if c.signed {
		sig, ok := strings.CutPrefix(strings.TrimSpace(ext), "chunk-signature=")
		if !ok {
			return ErrChunkFormat
		}
		c.sig = sig
		c.chunk = sha256.New()
	}
	if n > 0 {
		c.left = n
		return nil
	}
	if err := c.verifyChunk(); err != nil { // the final chunk is signed too
		return err
	}
	if err := c.readTrailer(); err != nil {
		return err
	}
	if c.decoded >= 0 && c.total != c.decoded {
		return ErrDecodedLength
	}
	c.done = true
	return nil
}

func (c *chunked) endChunk() error {
	if line, err := c.line(); err != nil || line != "" {
		return ErrChunkFormat
	}
	return c.verifyChunk()
}

func (c *chunked) verifyChunk() error {
	if !c.signed {
		return nil
	}
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.prefix + c.prev + "\n" + emptySHA256 + "\n" + hex.EncodeToString(c.chunk.Sum(nil))
	want := hex.EncodeToString(hmacSHA256(c.key, sts))
	if !hmac.Equal([]byte(want), []byte(c.sig)) {
		return ErrChunkSignature
	}
	c.prev = c.sig
	return nil
}

// readTrailer reads the trailing header lines up to the empty line and
// checks the checksum (and, signed, the trailer signature).
func (c *chunked) readTrailer() error {
	var lines []string
	sig := ""
	for {
		line, err := c.line()
		if errors.Is(err, io.EOF) && len(lines) == 0 && c.trailer == "" {
			return nil // no trailer, and the client ended without the empty line
		}
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return ErrChunkFormat
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if k == "x-amz-trailer-signature" {
			sig = v
			continue
		}
		lines = append(lines, k+":"+v)
		if k == c.trailer {
			if c.sum == nil || base64.StdEncoding.EncodeToString(c.sum.Sum(nil)) != v {
				return ErrTrailerChecksum
			}
			c.sum = nil
		}
		if len(lines) > 16 {
			return ErrChunkFormat
		}
	}
	if c.sum != nil { // announced in x-amz-trailer but not sent
		return ErrTrailerChecksum
	}
	if c.signed && c.trailer != "" {
		canon := strings.Join(lines, "\n") + "\n"
		sts := "AWS4-HMAC-SHA256-TRAILER\n" + c.prefix + c.prev + "\n" + hexSHA256([]byte(canon))
		want := hex.EncodeToString(hmacSHA256(c.key, sts))
		if !hmac.Equal([]byte(want), []byte(sig)) {
			return ErrChunkSignature
		}
	}
	return nil
}

// line reads one CRLF (or LF) terminated line without its terminator.
func (c *chunked) line() (string, error) {
	var b []byte
	for {
		frag, err := c.src.ReadSlice('\n')
		b = append(b, frag...)
		if len(b) > maxLine {
			return "", ErrChunkFormat
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(b) > 0 {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	s := strings.TrimSuffix(string(b), "\n")
	return strings.TrimSuffix(s, "\r"), nil
}

func (c *chunked) Close() error { return c.closer.Close() }

// CRC-64/NVME (reflected polynomial), the checksum newer AWS SDKs default to.
var crc64nvme = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// NewChecksum returns the hash of an x-amz-checksum-* header name (crc32,
// crc32c, crc64nvme, sha1, sha256); its Sum, base64 encoded, is the header
// value. nil for another name.
func NewChecksum(header string) hash.Hash {
	switch strings.TrimPrefix(strings.ToLower(header), "x-amz-checksum-") {
	case "crc32":
		return crc32.NewIEEE()
	case "crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "crc64nvme":
		return &crc64h{tab: crc64nvme}
	case "sha1":
		return sha1.New()
	case "sha256":
		return sha256.New()
	}
	return nil
}

// crc64h is crc64 with the NVME table (crc64.New only takes ISO and ECMA
// sums in big-endian; this is the same, kept explicit).
type crc64h struct {
	tab *crc64.Table
	crc uint64
}

func (h *crc64h) Write(p []byte) (int, error) { h.crc = crc64.Update(h.crc, h.tab, p); return len(p), nil }
func (h *crc64h) Sum(b []byte) []byte         { return binary.BigEndian.AppendUint64(b, h.crc) }
func (h *crc64h) Reset()                      { h.crc = 0 }
func (h *crc64h) Size() int                   { return 8 }
func (h *crc64h) BlockSize() int              { return 1 }

// EncodeChunked encodes body as aws-chunked for tests and tools: signed
// with the seed signature of q (q.PayloadHash a Streaming* value), with a
// trailing checksum named trailer ("" for none).
func EncodeChunked(q *Request, secret string, body []byte, chunkSize int, trailer string) []byte {
	var b strings.Builder
	signed := q.PayloadHash != StreamingUnsignedTrailer
	key := SigningKey(secret, q.Date, q.Region, q.Service)
	prefix := q.Time.Format(timeFormat) + "\n" + q.Scope() + "\n"
	prev := q.Signature
	var sum hash.Hash
	if trailer != "" {
		sum = NewChecksum(trailer)
	}
	emit := func(data []byte) {
		b.WriteString(strconv.FormatInt(int64(len(data)), 16))
		if signed {
			sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + prefix + prev + "\n" + emptySHA256 + "\n" + hexSHA256(data)
			prev = hex.EncodeToString(hmacSHA256(key, sts))
			b.WriteString(";chunk-signature=" + prev)
		}
		b.WriteString("\r\n")
		b.Write(data)
		if len(data) > 0 {
			b.WriteString("\r\n")
		}
		if sum != nil {
			sum.Write(data)
		}
	}
	for len(body) > 0 {
		n := min(chunkSize, len(body))
		emit(body[:n])
		body = body[n:]
	}
	emit(nil)
	if trailer != "" {
		line := strings.ToLower(trailer) + ":" + base64.StdEncoding.EncodeToString(sum.Sum(nil))
		b.WriteString(line + "\r\n")
		if signed {
			sts := "AWS4-HMAC-SHA256-TRAILER\n" + prefix + prev + "\n" + hexSHA256([]byte(line+"\n"))
			b.WriteString("x-amz-trailer-signature:" + hex.EncodeToString(hmacSHA256(key, sts)) + "\r\n")
		}
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}
