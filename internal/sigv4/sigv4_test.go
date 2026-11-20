package sigv4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The examples of the Amazon S3 API reference ("Signature Calculations for
// the Authorization Header", "Query string authentication", "Signature
// Calculations for the Authorization Header: Transferring Payload in
// Multiple Chunks").
const (
	exampleKey    = "AKIAIOSFODNN7EXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var exampleTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func TestAWSGetObjectExample(t *testing.T) {
	r := httptest.NewRequest("GET", "http://examplebucket.s3.amazonaws.com/test.txt", nil)
	r.Header.Set("Range", "bytes=0-9")
	r.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,"+
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
	q, err := Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if q.AccessKey != exampleKey || q.Region != "us-east-1" || q.Service != "s3" || q.Presigned {
		t.Fatalf("parsed %+v", q)
	}
	if err := q.Verify(r, exampleSecret, exampleTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := q.Verify(r, exampleSecret+"x", exampleTime); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong secret: %v", err)
	}
	r.Header.Set("Range", "bytes=0-8")
	if err := q.Verify(r, exampleSecret, exampleTime); !errors.Is(err, ErrSignature) {
		t.Fatalf("changed header: %v", err)
	}
	r.Header.Set("Range", "bytes=0-9")
	if err := q.Verify(r, exampleSecret, exampleTime.Add(16*time.Minute)); !errors.Is(err, ErrSkewed) {
		t.Fatalf("skew: %v", err)
	}
}

func TestAWSPresignedExample(t *testing.T) {
	r := httptest.NewRequest("GET", "http://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256"+
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z"+
		"&X-Amz-Expires=86400&X-Amz-SignedHeaders=host"+
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404", nil)
	q, err := Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Presigned || q.Expires != 24*time.Hour {
		t.Fatalf("parsed %+v", q)
	}
	if err := q.Verify(r, exampleSecret, exampleTime.Add(23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := q.Verify(r, exampleSecret, exampleTime.Add(25*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestAWSChunkedExample(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 66560)
	r := httptest.NewRequest("PUT", "http://s3.amazonaws.com/examplebucket/chunkObject.txt", nil)
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", "66560")
	r.Header.Set("Content-Length", "66824")
	r.Header.Set("X-Amz-Content-Sha256", StreamingSigned)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,"+
		"SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class,"+
		"Signature=4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9")
	q, err := Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Verify(r, exampleSecret, exampleTime); err != nil {
		t.Fatalf("seed signature: %v", err)
	}
	enc := EncodeChunked(q, exampleSecret, body, 65536, "")
	if len(enc) != 66824 {
		t.Fatalf("encoded length %d", len(enc))
	}
	for _, sig := range []string{"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"} {
		if !bytes.Contains(enc, []byte("chunk-signature="+sig)) {
			t.Fatalf("chunk signature %s missing", sig)
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(enc))
	got := decode(t, q, r)
	if !bytes.Equal(got, body) {
		t.Fatalf("decoded %d bytes", len(got))
	}
	bad := bytes.Clone(enc)
	bad[200] = 'b'
	r.Body = io.NopCloser(bytes.NewReader(bad))
	if _, err := readBody(q, r); !errors.Is(err, ErrChunkSignature) {
		t.Fatalf("tampered chunk: %v", err)
	}
}

func decode(t *testing.T, q *Request, r *http.Request) []byte {
	t.Helper()
	b, err := readBody(q, r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readBody(q *Request, r *http.Request) ([]byte, error) {
	rc, err := q.Body(r, exampleSecret)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(rc)
}

func TestSignRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for _, target := range []string{"http://s3.test/bucket/a%20b/%ED%95%9C%EA%B8%80~x+y.txt?uploads",
		"http://s3.test/?list-type=2&prefix=a%2Fb&delimiter=%2F", "http://b.s3.test:9000/k"} {
		r := httptest.NewRequest("PUT", target, nil)
		r.Header.Set("X-Amz-Meta-Note", "  two   spaces ")
		r.Header.Set("Content-Type", "text/plain")
		Sign(r, "CCKEXAMPLE", "secret", "us-east-1", now, UnsignedPayload)
		q, err := Parse(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Verify(r, "secret", now.Add(time.Minute)); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
	}
	r := httptest.NewRequest("GET", "http://s3.test/bucket/key?response-content-type=text%2Fplain", nil)
	Presign(r, "CCKEXAMPLE", "secret", "eu-west-1", now, time.Hour)
	q, err := Parse(httptest.NewRequest("GET", r.URL.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if q.Region != "eu-west-1" {
		t.Fatal(q.Region)
	}
	if err := q.Verify(httptest.NewRequest("GET", r.URL.String(), nil), "secret", now); err != nil {
		t.Fatal(err)
	}
}

func TestParseErrors(t *testing.T) {
	r := httptest.NewRequest("GET", "http://s3.test/b/k", nil)
	if _, err := Parse(r); !errors.Is(err, ErrNotSigned) {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "AWS AKID:c2ln")
	if _, err := Parse(r); !errors.Is(err, ErrV2) {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=x, Signature=y")
	if _, err := Parse(r); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "http://s3.test/b/k?AWSAccessKeyId=a&Signature=b&Expires=1", nil)
	if _, err := Parse(r); !errors.Is(err, ErrV2) {
		t.Fatal(err)
	}
}

func TestTrailers(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	body := bytes.Repeat([]byte("0123456789"), 10000)
	for _, mode := range []string{StreamingUnsignedTrailer, StreamingSignedTrailer} {
		for _, algo := range []string{"x-amz-checksum-crc32", "x-amz-checksum-crc32c", "x-amz-checksum-crc64nvme", "x-amz-checksum-sha256", "x-amz-checksum-sha1"} {
			r := httptest.NewRequest("PUT", "http://s3.test/b/k", nil)
			r.Header.Set("X-Amz-Trailer", algo)
			r.Header.Set("X-Amz-Decoded-Content-Length", "100000")
			Sign(r, "CCKEXAMPLE", exampleSecret, "us-east-1", now, mode)
			q, err := Parse(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Verify(r, exampleSecret, now); err != nil {
				t.Fatal(err)
			}
			enc := EncodeChunked(q, exampleSecret, body, 8192, algo)
			r.Body = io.NopCloser(bytes.NewReader(enc))
			if got := decode(t, q, r); !bytes.Equal(got, body) {
				t.Fatalf("%s %s: %d bytes", mode, algo, len(got))
			}
			// A wrong checksum value.
			i := bytes.LastIndex(enc, []byte(algo+":")) + len(algo) + 1
			bad := bytes.Clone(enc)
			if bad[i] == 'A' {
				bad[i] = 'B'
			} else {
				bad[i] = 'A'
			}
			r.Body = io.NopCloser(bytes.NewReader(bad))
			if _, err := readBody(q, r); !errors.Is(err, ErrTrailerChecksum) && !errors.Is(err, ErrChunkSignature) {
				t.Fatalf("%s %s: bad checksum read %v", mode, algo, err)
			}
		}
	}
}

func TestDecodedLength(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("PUT", "http://s3.test/b/k", nil)
	r.Header.Set("X-Amz-Decoded-Content-Length", "5")
	Sign(r, "CCKEXAMPLE", exampleSecret, "us-east-1", now, StreamingSigned)
	q, _ := Parse(r)
	r.Body = io.NopCloser(bytes.NewReader(EncodeChunked(q, exampleSecret, []byte("abcd"), 10, "")))
	if _, err := readBody(q, r); !errors.Is(err, ErrDecodedLength) {
		t.Fatal(err)
	}
	r.Body = io.NopCloser(strings.NewReader("zz;chunk-signature=x\r\n"))
	if _, err := readBody(q, r); !errors.Is(err, ErrChunkFormat) {
		t.Fatal(err)
	}
}

func TestChecksumCheckValues(t *testing.T) {
	for name, want := range map[string]uint64{"crc32": 0xcbf43926, "crc32c": 0xe3069283, "crc64nvme": 0xae8b14860a799888} {
		h := NewChecksum(name)
		h.Write([]byte("123456789"))
		s := h.Sum(nil)
		var got uint64
		if len(s) == 4 {
			got = uint64(binary.BigEndian.Uint32(s))
		} else {
			got = binary.BigEndian.Uint64(s)
		}
		if got != want {
			t.Errorf("%s: %x, want %x", name, got, want)
		}
	}
	if NewChecksum("x-amz-checksum-md7") != nil {
		t.Fatal("unknown algorithm")
	}
}
