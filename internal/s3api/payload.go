package s3api

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The request body as the client meant it: the aws-chunked encoding of the
// streaming payload forms decoded (AWS docs: "Signature Calculations for
// the Authorization Header: Transferring Payload in Multiple Chunks" and
// "... Including Trailing Headers"), and every digest the client declared
// checked when the body ends: x-amz-content-sha256, Content-MD5 and one
// x-amz-checksum-* (a header, or a trailer of the chunked body). A reader
// that fails at its end makes the blob store drop what it wrote, so nothing
// unverified is stored.

// checksums are the x-amz-checksum-* algorithms (AWS docs: "Checking object
// integrity"). CRC64NVME is CRC-64/NVME: reflected, polynomial
// 0xad93d23594c935a9, given here reversed as hash/crc64 takes it.
var checksums = map[string]func() hash.Hash{
	"crc32":     func() hash.Hash { return crc32.NewIEEE() },
	"crc32c":    func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) },
	"crc64nvme": func() hash.Hash { return crc64.New(crc64NVME) },
	"sha1":      sha1.New,
	"sha256":    sha256.New,
}

var crc64NVME = crc64.MakeTable(0x9a6c9329ac4bc9b5)

const checksumHeader = "X-Amz-Checksum-"

// maxChunkLine bounds a chunk header or trailer line.
const maxChunkLine = 4096

// body is a verified request body. Once Read returned io.EOF, checksum
// holds the x-amz-checksum-* the client declared (name and base64 value)
// for the response to echo, and md5 the content's MD5.
type body struct {
	r      io.Reader
	md5    hash.Hash
	sha    hash.Hash // x-amz-content-sha256 is a digest
	sum    hash.Hash // the x-amz-checksum-* algorithm's
	n      int64
	length int64 // x-amz-decoded-content-length, -1 if none
	limit  int64

	wantMD5, wantSHA string
	alg, wantSum     string // the checksum's name (crc32, ...) and value; wantSum "" until a trailer gives it
	chunks           *chunkReader

	checksum [2]string
	done     bool
	err      error
}

// newBody wraps r's body for the payload form sig names (nil: anonymous);
// a limit above 0 bounds the content.
func newBody(r *http.Request, sig *signer, limit int64) (*body, error) {
	b := &body{r: r.Body, md5: md5.New(), length: -1, limit: limit}
	if v := r.Header.Get("Content-MD5"); v != "" {
		d, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(d) != md5.Size {
			return nil, errf(http.StatusBadRequest, "InvalidDigest", "Content-MD5 is not a base64 MD5 digest")
		}
		b.wantMD5 = hex.EncodeToString(d)
	}
	for name := range checksums {
		if v := r.Header.Get(checksumHeader + name); v != "" {
			b.alg, b.wantSum = name, v
		}
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if sig != nil {
		payload = sig.payload
	}
	switch payload {
	case "", unsignedBody:
	case streamSigned, streamSignedT, streamUnsignT:
		n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return nil, errf(http.StatusLengthRequired, "MissingContentLength", "a streaming upload needs x-amz-decoded-content-length")
		}
		b.length = n
		if payload != streamUnsignT && sig == nil {
			return nil, errf(http.StatusForbidden, "AccessDenied", "a signed streaming payload needs a signed request")
		}
		c := &chunkReader{br: bufio.NewReaderSize(r.Body, maxChunkLine)}
		if payload != streamUnsignT {
			c.sig = sig
			c.prev = sig.signature
		}
		if payload != streamSigned {
			t := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Amz-Trailer")))
			name, ok := strings.CutPrefix(t, "x-amz-checksum-")
			if _, known := checksums[name]; !ok || !known {
				return nil, errf(http.StatusBadRequest, "InvalidRequest", "unsupported x-amz-trailer %q", t)
			}
			b.alg, b.wantSum, c.trailer = name, "", t
		}
		b.r, b.chunks = c, c
	default:
		if strings.HasPrefix(payload, "STREAMING-") {
			return nil, notImplemented("the payload form " + payload)
		}
		if _, err := hex.DecodeString(payload); err != nil || len(payload) != 64 {
			return nil, errf(http.StatusBadRequest, "InvalidArgument", "x-amz-content-sha256 must be a SHA-256 digest or a payload form")
		}
		b.sha, b.wantSHA = sha256.New(), payload
	}
	if b.alg != "" {
		b.sum = checksums[b.alg]()
	}
	return b, nil
}

func (b *body) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.r.Read(p)
	if n > 0 {
		b.n += int64(n)
		b.md5.Write(p[:n])
		if b.sha != nil {
			b.sha.Write(p[:n])
		}
		if b.sum != nil {
			b.sum.Write(p[:n])
		}
	}
	if b.limit > 0 && b.n > b.limit {
		err = errf(http.StatusRequestEntityTooLarge, "EntityTooLarge", "the upload is larger than the maxUploadMiB setting allows")
	} else if err == io.EOF {
		if verr := b.verify(); verr != nil {
			err = verr
		}
	}
	if err != nil {
		b.err = err
	}
	return n, err
}

// verify checks the digests once the body ended.
func (b *body) verify() error {
	b.done = true
	if b.length >= 0 && b.n != b.length {
		return errf(http.StatusBadRequest, "IncompleteBody", "the body has %d bytes, x-amz-decoded-content-length says %d", b.n, b.length)
	}
	if b.wantSHA != "" && hex.EncodeToString(b.sha.Sum(nil)) != b.wantSHA {
		return errf(http.StatusBadRequest, "XAmzContentSHA256Mismatch", "the body's SHA-256 is not the x-amz-content-sha256 the request was signed with")
	}
	if b.wantMD5 != "" && hex.EncodeToString(b.md5.Sum(nil)) != b.wantMD5 {
		return errf(http.StatusBadRequest, "BadDigest", "the body's MD5 is not its Content-MD5")
	}
	if b.sum == nil {
		return nil
	}
	want := b.wantSum
	if b.chunks != nil && b.chunks.trailer != "" {
		want = b.chunks.trailers[b.chunks.trailer]
	}
	got := base64.StdEncoding.EncodeToString(b.sum.Sum(nil))
	if want != got {
		return errf(http.StatusBadRequest, "BadDigest", "the body's %s checksum is not the x-amz-checksum-%s the client sent", b.alg, b.alg)
	}
	b.checksum = [2]string{checksumHeader + b.alg, got}
	return nil
}

// md5Hex is the content's MD5, valid once the body ended.
func (b *body) md5Hex() string { return hex.EncodeToString(b.md5.Sum(nil)) }

// echo sets the checksum response header of a verified body.
func (b *body) echo(w http.ResponseWriter) {
	if b.checksum[0] != "" {
		w.Header().Set(b.checksum[0], b.checksum[1])
	}
}

// chunkReader decodes aws-chunked: chunks of "<hex size>[;chunk-signature=
// <sig>]\r\n<data>\r\n", a last chunk of size 0, then trailer lines
// ("name:value\r\n", the signed form's last one x-amz-trailer-signature)
// and an empty line. With sig every chunk's signature chains from the seed
// signature (prev) and is checked when the chunk ends, the data streamed
// meanwhile; the trailer is signed the same way.
type chunkReader struct {
	br       *bufio.Reader
	sig      *signer // nil: unsigned chunks
	prev     string
	trailer  string            // the trailer the client announced, "" if none
	trailers map[string]string // read once the last chunk ended

	left    int64     // data bytes left in the current chunk
	want    string    // its signature
	hash    hash.Hash // of its data
	inChunk bool
	eof     bool
}

var errEncoding = errf(http.StatusBadRequest, "InvalidRequest", "malformed aws-chunked body")

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.eof {
		return 0, io.EOF
	}
	for c.left == 0 {
		if c.inChunk { // a data chunk ended: its CRLF, then its signature
			if l, err := c.line(); err != nil || l != "" {
				return 0, errEncoding
			}
			if err := c.checkChunk(); err != nil {
				return 0, err
			}
			c.inChunk = false
		}
		last, err := c.startChunk()
		if err != nil {
			return 0, err
		}
		if last {
			if err := c.checkChunk(); err != nil {
				return 0, err
			}
			if err := c.readTrailers(); err != nil {
				return 0, err
			}
			c.eof = true
			return 0, io.EOF
		}
		c.inChunk = true
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.br.Read(p)
	c.left -= int64(n)
	if c.hash != nil {
		c.hash.Write(p[:n])
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// line reads one CRLF-terminated line without its terminator.
func (c *chunkReader) line() (string, error) {
	l, err := c.br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errEncoding
	}
	if err == io.EOF {
		return "", io.ErrUnexpectedEOF
	}
	if err != nil {
		return "", err
	}
	l, ok := bytes.CutSuffix(l, []byte("\r\n"))
	if !ok {
		return "", errEncoding
	}
	return string(l), nil
}

// startChunk reads a chunk header; last reports the size-0 chunk.
func (c *chunkReader) startChunk() (last bool, err error) {
	l, err := c.line()
	if err != nil {
		return false, err
	}
	size, ext, _ := strings.Cut(l, ";")
	n, err := strconv.ParseInt(size, 16, 64)
	if err != nil || n < 0 {
		return false, errEncoding
	}
	if c.sig != nil {
		var ok bool
		if c.want, ok = strings.CutPrefix(ext, "chunk-signature="); !ok {
			return false, errEncoding
		}
		c.hash = sha256.New()
	}
	c.left = n
	return n == 0, nil
}

// checkChunk checks the signature of the chunk just read.
func (c *chunkReader) checkChunk() error {
	if c.sig == nil {
		return nil
	}
	s := c.sig.chained("AWS4-HMAC-SHA256-PAYLOAD", c.prev, emptySHA256+"\n"+hex.EncodeToString(c.hash.Sum(nil)))
	if !hmac.Equal([]byte(s), []byte(c.want)) {
		return errf(http.StatusForbidden, "SignatureDoesNotMatch", "a chunk's signature does not match")
	}
	c.prev = s
	return nil
}

// readTrailers reads the trailer lines up to the empty line, checking the
// signed form's x-amz-trailer-signature.
func (c *chunkReader) readTrailers() error {
	c.trailers = map[string]string{}
	var canonical strings.Builder
	sig := ""
	for {
		l, err := c.line()
		if err != nil {
			return err
		}
		if l == "" {
			break
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			return errEncoding
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if k == "x-amz-trailer-signature" {
			sig = v
			continue
		}
		c.trailers[k] = v
		canonical.WriteString(k + ":" + v + "\n")
	}
	if c.trailer != "" && c.trailers[c.trailer] == "" {
		return errf(http.StatusBadRequest, "InvalidRequest", "the body has no %s trailer", c.trailer)
	}
	if c.sig != nil && c.trailer != "" {
		s := c.sig.chained("AWS4-HMAC-SHA256-TRAILER", c.prev, sha256Hex(canonical.String()))
		if !hmac.Equal([]byte(s), []byte(sig)) {
			return errf(http.StatusForbidden, "SignatureDoesNotMatch", "the trailer's signature does not match")
		}
	}
	return nil
}

// signer is a verified request's signing context, for the chunks and
// trailer that chain from its signature.
type signer struct {
	key       []byte
	at        time.Time
	scope     string
	signature string
	payload   string
}

func (s *signer) chained(alg, prev, rest string) string {
	return hex.EncodeToString(hmacSHA256(s.key, alg+"\n"+s.at.UTC().Format(timeFormat)+"\n"+s.scope+"\n"+prev+"\n"+rest))
}
