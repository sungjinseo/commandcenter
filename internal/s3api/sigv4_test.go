package s3api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The examples of AWS's S3 API reference, "Authenticating Requests: Using
// the Authorization Header (AWS Signature Version 4)" and "... Query
// Parameters", "... Transferring Payload in Multiple Chunks": they are the
// specification, so their signatures are the test vectors.
const (
	exampleKey    = "AKIAIOSFODNN7EXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	exampleAuth   = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders="
)

func exampleRequest(method, target string, headers map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = "examplebucket.s3.amazonaws.com"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestSignatureAWSExamples(t *testing.T) {
	for _, c := range []struct {
		name    string
		r       *http.Request
		signed  string
		want    string
		presign bool
	}{
		{"GET Object", exampleRequest("GET", "/test.txt", map[string]string{"Range": "bytes=0-9",
			"X-Amz-Content-Sha256": emptySHA256, "X-Amz-Date": "20130524T000000Z"}, ""),
			"host;range;x-amz-content-sha256;x-amz-date", "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41", false},
		{"PUT Object", exampleRequest("PUT", "/test$file.text", map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT",
			"X-Amz-Content-Sha256": "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072", "X-Amz-Date": "20130524T000000Z",
			"X-Amz-Storage-Class": "REDUCED_REDUNDANCY"}, "Welcome to Amazon S3."),
			"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class", "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd", false},
		{"GET Bucket Lifecycle", exampleRequest("GET", "/?lifecycle", map[string]string{"X-Amz-Content-Sha256": emptySHA256,
			"X-Amz-Date": "20130524T000000Z"}, ""),
			"host;x-amz-content-sha256;x-amz-date", "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543", false},
		{"Get Bucket (List Objects)", exampleRequest("GET", "/?max-keys=2&prefix=J", map[string]string{"X-Amz-Content-Sha256": emptySHA256,
			"X-Amz-Date": "20130524T000000Z"}, ""),
			"host;x-amz-content-sha256;x-amz-date", "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7", false},
		{"presigned GET Object", exampleRequest("GET", "/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request"+
			"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host"+
			"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404", nil, ""),
			"", "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !c.presign {
				c.r.Header.Set("Authorization", exampleAuth+c.signed+",Signature="+c.want)
			}
			s, err := parseSigned(c.r)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.expected(c.r, signingKey(exampleSecret, s.cred)); got != c.want {
				t.Fatalf("signature %s, want %s\ncanonical request:\n%s", got, c.want, canonicalRequest(c.r, s))
			}
			if s.presigned != c.presign || s.cred.key != exampleKey {
				t.Fatalf("parsed %+v", s)
			}
		})
	}
}

// chunk encodes one aws-chunked chunk; sig "" leaves out the signature.
func chunk(data []byte, sig string) string {
	h := fmt.Sprintf("%x", len(data))
	if sig != "" {
		h += ";chunk-signature=" + sig
	}
	return h + "\r\n" + string(data) + "\r\n"
}

// The AWS example of a chunked upload: 65 KiB of "a" in a 64 KiB chunk, a
// 1 KiB chunk and the final one, each signed from the seed signature.
func TestChunkedAWSExample(t *testing.T) {
	seed := "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	sigs := []string{"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"}
	data := bytes.Repeat([]byte("a"), 66560)
	encoded := chunk(data[:65536], sigs[0]) + chunk(data[65536:], sigs[1]) + "0;chunk-signature=" + sigs[2] + "\r\n\r\n"
	headers := map[string]string{"Content-Encoding": "aws-chunked", "X-Amz-Content-Sha256": streamSigned, "X-Amz-Date": "20130524T000000Z",
		"X-Amz-Decoded-Content-Length": "66560", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"}
	signedHeaders := "content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class"

	read := func(body string) ([]byte, error) {
		r := exampleRequest("PUT", "/examplebucket/chunkObject.txt", headers, body)
		r.Host = "s3.amazonaws.com"
		r.Header.Set("Content-Length", "66824")
		r.Header.Set("Authorization", exampleAuth+signedHeaders+",Signature="+seed)
		s, err := parseSigned(r)
		if err != nil {
			t.Fatal(err)
		}
		key := signingKey(exampleSecret, s.cred)
		if got := s.expected(r, key); got != seed {
			t.Fatalf("seed signature %s, want %s\n%s", got, seed, canonicalRequest(r, s))
		}
		b, err := newBody(r, &signer{key: key, at: s.at, scope: s.cred.scope(), signature: s.signature, payload: s.payload}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return io.ReadAll(b)
	}
	if len(encoded) != 66824 {
		t.Fatalf("encoded length %d", len(encoded))
	}
	got, err := read(encoded)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("decoded %d bytes, %v", len(got), err)
	}
	// One changed byte in the second chunk breaks its signature.
	if _, err := read(strings.Replace(encoded, "aaaa\r\n0;", "aaab\r\n0;", 1)); asS3Error(err).code != "SignatureDoesNotMatch" {
		t.Fatalf("tampered chunk: %v", err)
	}
	// So does a chunk signed for another place in the chain.
	if _, err := read(chunk(data[:65536], sigs[1]) + chunk(data[65536:], sigs[0]) + "0;chunk-signature=" + sigs[2] + "\r\n\r\n"); asS3Error(err).code != "SignatureDoesNotMatch" {
		t.Fatalf("reordered signatures: %v", err)
	}
	if _, err := read(encoded[:40000]); err == nil {
		t.Fatal("a cut body was accepted")
	}
}

func crc32Of(b []byte) string {
	h := crc32.NewIEEE()
	h.Write(b)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// The trailer forms current SDKs send: unsigned chunks with a trailing
// checksum (STREAMING-UNSIGNED-PAYLOAD-TRAILER), and signed chunks with a
// signed trailer.
func TestTrailers(t *testing.T) {
	data := []byte(strings.Repeat("checksum me ", 1000))
	good, bad := crc32Of(data), crc32Of([]byte("other"))
	unsigned := func(sum string) string {
		return chunk(data[:5000], "") + chunk(data[5000:], "") + "0\r\nx-amz-checksum-crc32:" + sum + "\r\n\r\n"
	}
	newReq := func(payload, body string) *http.Request {
		r := httptest.NewRequest("PUT", "/b/k", strings.NewReader(body))
		r.Header.Set("X-Amz-Content-Sha256", payload)
		r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(data)))
		return r
	}
	sig := &signer{key: []byte("k"), at: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), scope: "20260102/us-east-1/s3/aws4_request", payload: streamUnsignT}
	for _, c := range []struct {
		name, body, code string
		sig              *signer
	}{
		{"unsigned trailer", unsigned(good), "", sig},
		{"unsigned trailer, wrong checksum", unsigned(bad), "BadDigest", sig},
		{"unsigned trailer, anonymous", unsigned(good), "", nil},
		{"no trailer", chunk(data, "") + "0\r\n\r\n", "InvalidRequest", sig},
	} {
		b, err := newBody(newReq(streamUnsignT, c.body), c.sig, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(b)
		if c.code == "" && (err != nil || !bytes.Equal(got, data)) || c.code != "" && (err == nil || asS3Error(err).code != c.code) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.code == "" && b.checksum != [2]string{"X-Amz-Checksum-crc32", good} {
			t.Fatalf("%s: checksum %v", c.name, b.checksum)
		}
	}

	// The signed form: the chunks chain from the seed, the trailer from
	// the last chunk's signature.
	signed := *sig
	signed.signature, signed.payload = strings.Repeat("0", 64), streamSignedT
	encode := func(trailerSum string, trailerSig func(prev string) string) string {
		var out strings.Builder
		prev := signed.signature
		for _, part := range [][]byte{data[:5000], data[5000:], nil} {
			s := signed.chained("AWS4-HMAC-SHA256-PAYLOAD", prev, emptySHA256+"\n"+sha256Hex(string(part)))
			if part == nil {
				out.WriteString("0;chunk-signature=" + s + "\r\n")
			} else {
				out.WriteString(chunk(part, s))
			}
			prev = s
		}
		out.WriteString("x-amz-checksum-crc32:" + trailerSum + "\r\nx-amz-trailer-signature:" + trailerSig(prev) + "\r\n\r\n")
		return out.String()
	}
	right := func(prev string) string {
		return signed.chained("AWS4-HMAC-SHA256-TRAILER", prev, sha256Hex("x-amz-checksum-crc32:"+good+"\n"))
	}
	for _, c := range []struct {
		name, body, code string
	}{
		{"signed trailer", encode(good, right), ""},
		{"signed trailer, wrong signature", encode(good, func(string) string { return strings.Repeat("1", 64) }), "SignatureDoesNotMatch"},
		{"signed trailer, checksum changed", encode(bad, right), "SignatureDoesNotMatch"},
	} {
		b, err := newBody(newReq(streamSignedT, c.body), &signed, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(b)
		if c.code == "" && (err != nil || !bytes.Equal(got, data)) || c.code != "" && (err == nil || asS3Error(err).code != c.code) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// CRC-64/NVME's check value (the CRC of "123456789").
func TestCRC64NVME(t *testing.T) {
	if got := crc64.Checksum([]byte("123456789"), crc64NVME); got != 0xae8b14860a799888 {
		t.Fatalf("crc64nvme %x", got)
	}
}

// Content-MD5 and x-amz-content-sha256 are checked when the body ends.
func TestBodyDigests(t *testing.T) {
	for _, c := range []struct {
		name, header, value, code string
	}{
		{"md5", "Content-MD5", "XUFAKrxLKna5cZ2REBfFkg==", ""}, // MD5 of "hello"
		{"wrong md5", "Content-MD5", "1B2M2Y8AsgTpgAmY7PhCfg==", "BadDigest"},
		{"sha256", "X-Amz-Content-Sha256", sha256Hex("hello"), ""},
		{"wrong sha256", "X-Amz-Content-Sha256", sha256Hex("other"), "XAmzContentSHA256Mismatch"},
		{"crc32 header", "X-Amz-Checksum-Crc32", crc32Of([]byte("hello")), ""},
		{"wrong crc32 header", "X-Amz-Checksum-Crc32", crc32Of([]byte("other")), "BadDigest"},
	} {
		r := httptest.NewRequest("PUT", "/b/k", strings.NewReader("hello"))
		r.Header.Set(c.header, c.value)
		b, err := newBody(r, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(b)
		if c.code == "" && err != nil || c.code != "" && (err == nil || asS3Error(err).code != c.code) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	r := httptest.NewRequest("PUT", "/b/k", strings.NewReader("hello"))
	if b, _ := newBody(r, nil, 4); b != nil {
		if _, err := io.ReadAll(b); asS3Error(err).code != "EntityTooLarge" {
			t.Fatalf("over the limit: %v", err)
		}
	}
}

// A long step answers 200 at once and keeps the connection busy; its error
// ends up in the body, where the SDKs look for it.
func TestSlow(t *testing.T) {
	w := httptest.NewRecorder()
	slow(w, httptest.NewRequest("POST", "/b/k?uploadId=x", nil), func() (any, error) {
		time.Sleep(keepAliveEvery + keepAliveEvery/2)
		return nil, errNoSuchUpload
	})
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(b, "?>\n <Error><Code>NoSuchUpload</Code>") {
		t.Fatalf("%d %q", w.Code, b)
	}
}
