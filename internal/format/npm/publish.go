package npm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/repo"
)

const maxManifest = 10 << 20

// maxDepth bounds JSON nesting so a body of brackets cannot grow the scanner stack.
const maxDepth = 64

// publishDoc is the part of an npm publish document Command Center uses.
type publishDoc struct {
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
}

type versionManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Shasum    string `json:"shasum"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

func badJSON(err error) error {
	return repo.Errorf(http.StatusBadRequest, "invalid publish document: %v", err)
}

// splitPublish reads an npm publish document. The base64 "data" of every
// _attachments entry is decoded and streamed to store as it is read; the rest
// of the document is returned with those strings emptied.
func splitPublish(r io.Reader, store func(name string, data io.Reader) error) ([]byte, error) {
	br := bufio.NewReaderSize(r, 32*1024)
	var out bytes.Buffer
	type scanFrame struct {
		object    bool
		expectKey bool
		key       string // object: key whose value is being read
	}
	var stack []scanFrame
	for {
		c, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if out.Len() > maxManifest {
			return nil, repo.Errorf(http.StatusRequestEntityTooLarge, "publish document is too large")
		}
		switch c {
		case '{', '[':
			if len(stack) >= maxDepth {
				return nil, badJSON(fmt.Errorf("nesting depth exceeds %d", maxDepth))
			}
			stack = append(stack, scanFrame{object: c == '{', expectKey: c == '{'})
			out.WriteByte(c)
		case '}', ']':
			if len(stack) == 0 {
				return nil, badJSON(errors.New("unbalanced brackets"))
			}
			stack = stack[:len(stack)-1]
			out.WriteByte(c)
		case ',':
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].expectKey = true
			}
			out.WriteByte(c)
		case '"':
			n := len(stack)
			if n > 0 && stack[n-1].object && stack[n-1].expectKey {
				raw, err := readString(br)
				if err != nil {
					return nil, err
				}
				var key string
				if err := json.Unmarshal(raw, &key); err != nil {
					return nil, badJSON(err)
				}
				stack[n-1].key, stack[n-1].expectKey = key, false
				out.Write(raw)
				continue
			}
			if n == 3 && stack[0].key == "_attachments" && stack[1].object && stack[2].object && stack[2].key == "data" {
				if err := streamData(br, stack[1].key, store); err != nil {
					return nil, err
				}
				out.WriteString(`""`)
				continue
			}
			raw, err := readString(br)
			if err != nil {
				return nil, err
			}
			out.Write(raw)
		default:
			out.WriteByte(c) // whitespace, ':', numbers, literals
		}
	}
	if len(stack) != 0 {
		return nil, badJSON(io.ErrUnexpectedEOF)
	}
	return out.Bytes(), nil
}

// readString reads the rest of a JSON string whose opening quote was consumed
// and returns it including both quotes.
func readString(br *bufio.Reader) ([]byte, error) {
	buf := []byte{'"'}
	for {
		c, err := br.ReadByte()
		if err != nil {
			return nil, readErr(err)
		}
		buf = append(buf, c)
		if len(buf) > maxManifest {
			return nil, repo.Errorf(http.StatusRequestEntityTooLarge, "publish document is too large")
		}
		switch c {
		case '\\':
			e, err := br.ReadByte()
			if err != nil {
				return nil, readErr(err)
			}
			buf = append(buf, e)
		case '"':
			return buf, nil
		}
	}
}

// streamData decodes one base64 attachment string into store.
func streamData(br *bufio.Reader, name string, store func(string, io.Reader) error) error {
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := store(name, base64.NewDecoder(base64.StdEncoding, pr))
		pr.CloseWithError(err) // unblock the writer if store stopped early
		errc <- err
	}()
	werr := copyStringBody(br, pw)
	pw.CloseWithError(werr)
	serr := <-errc
	if serr != nil {
		var cie base64.CorruptInputError
		if errors.As(serr, &cie) {
			return badJSON(fmt.Errorf("attachment %q is not valid base64: %v", name, serr))
		}
	}
	if werr != nil {
		return werr
	}
	if serr != nil {
		return fmt.Errorf("attachment %q: %w", name, serr)
	}
	return nil
}

// copyStringBody writes the unescaped characters of a JSON string to w until its
// closing quote. Only the escapes that occur in base64 text are supported.
func copyStringBody(br *bufio.Reader, w io.Writer) error {
	buf := make([]byte, 0, 32*1024)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		_, err := w.Write(buf)
		buf = buf[:0]
		return err
	}
	for {
		c, err := br.ReadByte()
		if err != nil {
			return readErr(err)
		}
		switch c {
		case '"':
			return flush()
		case '\\':
			e, err := br.ReadByte()
			if err != nil {
				return readErr(err)
			}
			switch e {
			case '/', '\\', '"':
				c = e
			case 'n', 'r', 't':
				continue // line breaks inside base64 are ignored
			default:
				return badJSON(fmt.Errorf("unsupported escape \\%c in attachment data", e))
			}
		}
		buf = append(buf, c)
		if len(buf) == cap(buf) {
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

// verifyTarball checks dist.shasum (hex SHA-1) and a sha512 dist.integrity
// (Subresource Integrity) against the stored content. Empty fields are skipped.
func verifyTarball(shasum, integrity string, sums blob.Sums) error {
	if shasum != "" && !strings.EqualFold(shasum, sums.SHA1) {
		return repo.Errorf(http.StatusBadRequest, "tarball shasum mismatch")
	}
	for _, item := range strings.Fields(integrity) {
		alg, val, ok := strings.Cut(item, "-")
		if !ok || alg != "sha512" {
			continue
		}
		raw, err := hex.DecodeString(sums.SHA512)
		if err != nil || base64.StdEncoding.EncodeToString(raw) != val {
			return repo.Errorf(http.StatusBadRequest, "tarball integrity mismatch")
		}
	}
	return nil
}

// deprecationOf returns the deprecated message of stored version attrs ("" if
// none or unreadable).
func deprecationOf(attrs string) string {
	var v struct {
		Deprecated any `json:"deprecated"`
	}
	json.Unmarshal([]byte(attrs), &v)
	s, _ := v.Deprecated.(string)
	return s
}

// applyDeprecation returns the stored attrs with only `deprecated` taken from
// newManifest (absent or "" clears it, as npm undeprecate sends ""). Every other
// field, including dist, keeps its stored value.
func applyDeprecation(newManifest json.RawMessage, oldAttrs string) (string, error) {
	var nm, om map[string]json.RawMessage
	if err := json.Unmarshal(newManifest, &nm); err != nil {
		return "", badJSON(err)
	}
	if nm == nil {
		return "", badJSON(errors.New("version manifest must be an object"))
	}
	if err := json.Unmarshal([]byte(oldAttrs), &om); err != nil {
		return "", err
	}
	if om == nil {
		return "", errors.New("stored version attrs must be an object")
	}
	var dep string
	if raw, ok := nm["deprecated"]; ok {
		json.Unmarshal(raw, &dep) // non-string values count as absent
	}
	if dep != "" {
		om["deprecated"], _ = json.Marshal(dep)
	} else {
		delete(om, "deprecated")
	}
	b, err := json.Marshal(om)
	return string(b), err
}

// withServerDist returns the manifest with dist.tarball, dist.shasum and
// dist.integrity set from the stored tarball. Other client dist fields are kept.
func withServerDist(raw json.RawMessage, file string, sums blob.Sums) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", badJSON(err)
	}
	if m == nil {
		return "", badJSON(errors.New("version manifest is null"))
	}
	dist := map[string]any{}
	if d, ok := m["dist"]; ok {
		if err := json.Unmarshal(d, &dist); err != nil {
			return "", badJSON(err)
		}
		if dist == nil {
			dist = map[string]any{}
		}
	}
	sum512, err := hex.DecodeString(sums.SHA512)
	if err != nil {
		return "", err
	}
	dist["tarball"] = "/-/" + file
	dist["shasum"] = sums.SHA1
	dist["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(sum512)
	db, err := json.Marshal(dist)
	if err != nil {
		return "", err
	}
	m["dist"] = db
	out, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// readErr maps a failed read of the request body: truncation is a bad document,
// anything else (such as an exceeded upload cap) passes through unchanged.
func readErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return badJSON(io.ErrUnexpectedEOF)
	}
	return err
}

// publishSem bounds how many publish documents are scanned and decoded at once,
// so concurrent manifests of up to maxManifest bytes stay inside the memory budget.
var publishSem = make(chan struct{}, 2)

// publishWaitTimeout bounds how long a publish waits for a slot.
var publishWaitTimeout = 30 * time.Second

// publishReadTimeout bounds how long a publish may go without receiving body
// data while it holds a slot; every successful read extends it.
var publishReadTimeout = 5 * time.Minute

// acquirePublish waits for a publish slot or the context. The returned release
// is idempotent.
// Waiting is bounded by publishWaitTimeout (503 on expiry).
func acquirePublish(ctx context.Context) (func(), error) {
	timer := time.NewTimer(publishWaitTimeout)
	defer timer.Stop()
	select {
	case publishSem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-publishSem }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, repo.Errorf(http.StatusServiceUnavailable, "too many concurrent publishes, retry")
	}
}
