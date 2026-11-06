package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"commandcenter/internal/blob"
	"commandcenter/internal/repo"
)

const (
	// jsonCheckMax bounds what JSONDocument reads, and so the token a decoder
	// holds: a body of one long string costs about twice this, and a server of
	// 128 MiB runs 32 fetches at once. What the check is for shows far sooner, a
	// page or text at the first byte. A body that is still JSON there passes:
	// the format's reader has its own limit.
	jsonCheckMax = 1 << 20
	// jsonCheckDepth bounds the nesting: the decoder keeps an entry per level.
	jsonCheckDepth = 512
)

// JSONDocument returns a Target.Check for a document that must be JSON, as
// a stream, so that a sign-in page or error text answered with 200 does not
// replace a good copy for metadataMaxAge. Without keys it must be one object
// or array. With keys it must be an object with one of them at its top level,
// which is as far as it is read: the answer of a remote that says what it
// refuses in JSON has none of them.
func JSONDocument(keys ...string) func(context.Context, io.ReadSeeker, blob.Sums) (blob.Sums, error) {
	return func(_ context.Context, body io.ReadSeeker, sums blob.Sums) (blob.Sums, error) {
		lr := &io.LimitedReader{R: body, N: jsonCheckMax}
		if err := checkJSON(lr, keys); err != nil && lr.N > 0 {
			return sums, repo.Errorf(http.StatusBadGateway, "the remote's answer is not JSON of the expected kind: %v", err)
		}
		return sums, nil
	}
}

func checkJSON(r io.Reader, keys []string) error {
	br := bufio.NewReader(r)
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\xef\xbb\xbf")) { // .NET servers write one
		br.Discard(3)
	}
	dec := json.NewDecoder(br)
	dec.UseNumber() // a number need not fit a float
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, _ := tok.(json.Delim); d != '{' && (d != '[' || len(keys) > 0) {
		return errors.New("a wrong value at its top level")
	}
	if len(keys) == 0 {
		if err := skipRest(dec); err != nil {
			return err
		}
		if _, err := dec.Token(); err != io.EOF {
			return errors.New("data after the JSON document")
		}
		return nil
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return err
		}
		for _, want := range keys {
			if k == want {
				return nil
			}
		}
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, _ := tok.(json.Delim); d == '{' || d == '[' {
			if err := skipRest(dec); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("none of %q at its top level", keys)
}

// skipRest reads to the end of the value whose opening delimiter was read.
func skipRest(dec *json.Decoder) error {
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			if depth++; depth > jsonCheckDepth {
				return errors.New("JSON nested too deeply")
			}
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}
