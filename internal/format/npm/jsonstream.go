package npm

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
)

var errNotObject = errors.New("npm: expected a JSON object")

// tokenWriter writes a JSON document token by token, adding the separators the
// structure needs, so documents can be transformed without building trees.
type tokenWriter struct {
	w     *bufio.Writer
	stack []frame
	err   error
}

type frame struct {
	object bool
	count  int // keys (object) or elements (array) written so far
}

func newTokenWriter(w io.Writer) *tokenWriter {
	return &tokenWriter{w: bufio.NewWriterSize(w, 32*1024)}
}

func (t *tokenWriter) raw(s string) {
	if t.err == nil {
		_, t.err = t.w.WriteString(s)
	}
}

// beforeValue writes the comma before an array element. Object values follow
// their key, which already wrote the separator.
func (t *tokenWriter) beforeValue() {
	if n := len(t.stack); n > 0 && !t.stack[n-1].object {
		if t.stack[n-1].count > 0 {
			t.raw(",")
		}
		t.stack[n-1].count++
	}
}

func (t *tokenWriter) Begin(object bool) {
	t.beforeValue()
	if object {
		t.raw("{")
	} else {
		t.raw("[")
	}
	t.stack = append(t.stack, frame{object: object})
}

func (t *tokenWriter) End() {
	f := t.stack[len(t.stack)-1]
	t.stack = t.stack[:len(t.stack)-1]
	if f.object {
		t.raw("}")
	} else {
		t.raw("]")
	}
}

func (t *tokenWriter) Key(k string) {
	f := &t.stack[len(t.stack)-1]
	if f.count > 0 {
		t.raw(",")
	}
	f.count++
	t.str(k)
	t.raw(":")
}

// Value writes a scalar: string, json.Number, bool or nil.
func (t *tokenWriter) Value(v any) {
	t.beforeValue()
	switch x := v.(type) {
	case string:
		t.str(x)
	case json.Number:
		t.raw(x.String())
	case bool:
		if x {
			t.raw("true")
		} else {
			t.raw("false")
		}
	case nil:
		t.raw("null")
	default:
		b, err := json.Marshal(x)
		if err != nil && t.err == nil {
			t.err = err
		}
		t.raw(string(b))
	}
}

// RawValue writes an already-encoded JSON value.
func (t *tokenWriter) RawValue(b []byte) {
	t.beforeValue()
	if t.err == nil {
		_, t.err = t.w.Write(b)
	}
}

func (t *tokenWriter) str(s string) {
	b, err := json.Marshal(s)
	if err != nil && t.err == nil {
		t.err = err
	}
	t.raw(string(b))
}

func (t *tokenWriter) Flush() error {
	if t.err != nil {
		return t.err
	}
	return t.w.Flush()
}

// copyValue copies the next value from dec to tw. rewrite, when set, may replace
// string values; path holds the object keys leading to the value.
func copyValue(dec *json.Decoder, tw *tokenWriter, path []string, rewrite func(path []string, s string) string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		object := v == '{'
		tw.Begin(object)
		for dec.More() {
			childPath := path
			if object {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				tw.Key(k)
				childPath = append(path, k)
			}
			if err := copyValue(dec, tw, childPath, rewrite); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing delimiter
			return err
		}
		tw.End()
	case string:
		if rewrite != nil {
			v = rewrite(path, v)
		}
		tw.Value(v)
	default:
		tw.Value(v)
	}
	return tw.err
}

// skipValue consumes the next value from dec.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// eachEntry walks the object that is the next value of dec; fn must consume
// each entry's value.
func eachEntry(dec *json.Decoder, fn func(key string) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errNotObject
	}
	return walkEntries(dec, fn)
}

// eachEntryIfObject is like eachEntry, but a non-object value counts as an empty
// object: arrays are skipped and scalars are already consumed, both return nil.
func eachEntryIfObject(dec *json.Decoder, fn func(key string) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if d == '[' {
		depth := 1
		for depth > 0 {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := tok.(json.Delim); ok {
				if d == '{' || d == '[' {
					depth++
				} else {
					depth--
				}
			}
		}
		return nil
	}
	return walkEntries(dec, fn)
}

// walkEntries visits the entries of an object whose opening delimiter has
// already been consumed, through its closing delimiter.
func walkEntries(dec *json.Decoder, fn func(key string) error) error {
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return errNotObject
		}
		if err := fn(key); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// eachTopLevel rewinds r and walks its top-level object.
func eachTopLevel(r io.ReadSeeker, fn func(key string, dec *json.Decoder) error) error {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return eachEntry(dec, func(key string) error { return fn(key, dec) })
}
