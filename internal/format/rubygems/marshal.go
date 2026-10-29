package rubygems

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Ruby Marshal 4.8, the subset RubyGems' legacy indexes and dependency API
// carry: nil, booleans, fixnums, UTF-8 strings, symbols, arrays, hashes,
// plain objects ('o', Gem::Dependency, Gem::Platform), marshal_dump
// objects ('U', Gem::Version, Gem::Requirement) and _dump objects ('u',
// Gem::Specification, Time). Values are the Go types below; a Go string is
// a UTF-8 String, an int a Fixnum.
type (
	sym    string
	hashKV struct{ key, val any }
	hash   []hashKV
	object struct { // 'o': ivars keyed by sym("@name")
		class string
		ivars hash
	}
	userObj struct { // 'U': what marshal_dump returned
		class string
		val   any
	}
	userDump struct { // 'u': what _dump returned, with the ivars Ruby adds to it
		class string
		data  []byte
		ivars hash
	}
)

type marshalWriter struct {
	buf  bytes.Buffer
	syms map[string]int
}

// marshalDump encodes v as Marshal.dump does (without object links, which
// a reader never needs).
func marshalDump(v any) ([]byte, error) {
	w := &marshalWriter{syms: map[string]int{}}
	w.buf.WriteString("\x04\x08")
	if err := w.value(v); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// long writes an integer as Marshal packs one (w_long).
func (w *marshalWriter) long(n int) {
	switch {
	case n == 0:
		w.buf.WriteByte(0)
	case n > 0 && n < 123:
		w.buf.WriteByte(byte(n + 5))
	case n < 0 && n > -124:
		w.buf.WriteByte(byte(int8(n - 5)))
	default:
		var tmp [4]byte
		binary.LittleEndian.PutUint32(tmp[:], uint32(int32(n)))
		k := 4
		if n > 0 {
			for k > 1 && tmp[k-1] == 0 {
				k--
			}
			w.buf.WriteByte(byte(k))
		} else {
			for k > 1 && tmp[k-1] == 0xff {
				k--
			}
			w.buf.WriteByte(byte(int8(-k)))
		}
		w.buf.Write(tmp[:k])
	}
}

func (w *marshalWriter) sym(s string) {
	if i, ok := w.syms[s]; ok {
		w.buf.WriteByte(';')
		w.long(i)
		return
	}
	w.syms[s] = len(w.syms)
	w.buf.WriteByte(':')
	w.bytes([]byte(s))
}

func (w *marshalWriter) bytes(b []byte) {
	w.long(len(b))
	w.buf.Write(b)
}

func (w *marshalWriter) pairs(h hash) error {
	w.long(len(h))
	for _, kv := range h {
		if err := w.value(kv.key); err != nil {
			return err
		}
		if err := w.value(kv.val); err != nil {
			return err
		}
	}
	return nil
}

func (w *marshalWriter) value(v any) error {
	switch x := v.(type) {
	case nil:
		w.buf.WriteByte('0')
	case bool:
		if x {
			w.buf.WriteByte('T')
		} else {
			w.buf.WriteByte('F')
		}
	case int:
		w.buf.WriteByte('i')
		w.long(x)
	case string: // I"..." with the UTF-8 encoding ivar
		w.buf.WriteString(`I"`)
		w.bytes([]byte(x))
		w.long(1)
		w.sym("E")
		w.buf.WriteByte('T')
	case sym:
		w.sym(string(x))
	case []any:
		w.buf.WriteByte('[')
		w.long(len(x))
		for _, e := range x {
			if err := w.value(e); err != nil {
				return err
			}
		}
	case hash:
		w.buf.WriteByte('{')
		return w.pairs(x)
	case object:
		w.buf.WriteByte('o')
		w.sym(x.class)
		return w.pairs(x.ivars)
	case userObj:
		w.buf.WriteByte('U')
		w.sym(x.class)
		return w.value(x.val)
	case userDump:
		if len(x.ivars) > 0 {
			w.buf.WriteByte('I')
		}
		w.buf.WriteByte('u')
		w.sym(x.class)
		w.bytes(x.data)
		if len(x.ivars) > 0 {
			return w.pairs(x.ivars)
		}
	default:
		return fmt.Errorf("marshal: cannot encode %T", v)
	}
	return nil
}

type marshalReader struct {
	b    []byte
	i    int
	syms []string
	objs []any
}

var errMarshal = errors.New("marshal: malformed data")

// marshalLoad decodes Marshal data of the subset marshalDump writes, plus
// object links ('@'). Anything else (floats, bignums, structs, regexps,
// extended or subclassed objects) is an error.
func marshalLoad(b []byte) (v any, err error) {
	if len(b) < 2 || b[0] != 4 || b[1] != 8 {
		return nil, errMarshal
	}
	defer func() {
		if recover() != nil {
			v, err = nil, errMarshal
		}
	}()
	r := &marshalReader{b: b, i: 2}
	v, err = r.value()
	if err == nil && r.i != len(b) {
		err = errMarshal
	}
	return v, err
}

func (r *marshalReader) byte() byte {
	c := r.b[r.i]
	r.i++
	return c
}

func (r *marshalReader) long() int {
	c := int(int8(r.byte()))
	switch {
	case c == 0:
		return 0
	case c > 4:
		return c - 5
	case c < -4:
		return c + 5
	case c > 0:
		n := 0
		for i := 0; i < c; i++ {
			n |= int(r.byte()) << (8 * i)
		}
		return n
	}
	n := -1 << (8 * -c)
	for i := 0; i < -c; i++ {
		n |= int(r.byte()) << (8 * i)
	}
	return n
}

func (r *marshalReader) bytes() []byte {
	n := r.long()
	if n < 0 || r.i+n > len(r.b) {
		panic(errMarshal)
	}
	b := r.b[r.i : r.i+n]
	r.i += n
	return b
}

// register adds an object to the link table and returns its slot, to fill
// once the object is complete (Ruby registers arrays, hashes and objects
// before reading their contents).
func (r *marshalReader) register(v any) int {
	r.objs = append(r.objs, v)
	return len(r.objs) - 1
}

func (r *marshalReader) symbol() (string, error) {
	switch r.byte() {
	case ':':
		s := string(r.bytes())
		r.syms = append(r.syms, s)
		return s, nil
	case ';':
		return r.syms[r.long()], nil
	}
	return "", errMarshal
}

func (r *marshalReader) pairs() (hash, error) {
	n := r.long()
	if n < 0 || n > len(r.b) {
		return nil, errMarshal
	}
	h := make(hash, 0, n)
	for i := 0; i < n; i++ {
		k, err := r.value()
		if err != nil {
			return nil, err
		}
		v, err := r.value()
		if err != nil {
			return nil, err
		}
		h = append(h, hashKV{k, v})
	}
	return h, nil
}

func (r *marshalReader) value() (any, error) {
	switch c := r.byte(); c {
	case '0':
		return nil, nil
	case 'T':
		return true, nil
	case 'F':
		return false, nil
	case 'i':
		return r.long(), nil
	case ':', ';':
		r.i--
		s, err := r.symbol()
		return sym(s), err
	case '"':
		s := string(r.bytes())
		r.register(s)
		return s, nil
	case '@':
		i := r.long()
		if i < 0 || i >= len(r.objs) {
			return nil, errMarshal
		}
		return r.objs[i], nil
	case 'I':
		if r.b[r.i] == 'u' { // _dump data: its ivars come before the object is registered
			r.i++
			class, err := r.symbol()
			if err != nil {
				return nil, err
			}
			data := r.bytes()
			ivars, err := r.pairs()
			if err != nil {
				return nil, err
			}
			u := userDump{class, data, ivars}
			r.register(u)
			return u, nil
		}
		v, err := r.value()
		if err != nil {
			return nil, err
		}
		_, err = r.pairs() // a string's encoding, which Go strings do not carry
		return v, err
	case '[':
		n := r.long()
		if n < 0 || n > len(r.b) {
			return nil, errMarshal
		}
		slot := r.register(nil)
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			e, err := r.value()
			if err != nil {
				return nil, err
			}
			arr = append(arr, e)
		}
		r.objs[slot] = arr
		return arr, nil
	case '{':
		slot := r.register(nil)
		h, err := r.pairs()
		r.objs[slot] = h
		return h, err
	case 'o':
		class, err := r.symbol()
		if err != nil {
			return nil, err
		}
		slot := r.register(nil)
		ivars, err := r.pairs()
		o := object{class, ivars}
		r.objs[slot] = o
		return o, err
	case 'U':
		class, err := r.symbol()
		if err != nil {
			return nil, err
		}
		slot := r.register(nil)
		val, err := r.value()
		u := userObj{class, val}
		r.objs[slot] = u
		return u, err
	case 'u':
		class, err := r.symbol()
		if err != nil {
			return nil, err
		}
		u := userDump{class: class, data: r.bytes()}
		r.register(u)
		return u, nil
	}
	return nil, errMarshal
}

// ---- RubyGems objects ----

func gemVersion(v string) userObj { return userObj{"Gem::Version", []any{v}} }

// gemRequirement is a Gem::Requirement from its to_s form (">= 1.0, < 2";
// "" is ">= 0"): marshal_dump is [[[op, Gem::Version], ...]].
func gemRequirement(s string) userObj {
	var reqs []any
	for _, part := range strings.Split(s, ",") {
		op, v := parseRequirement(part)
		if v == "" {
			continue
		}
		reqs = append(reqs, []any{op, gemVersion(v)})
	}
	if len(reqs) == 0 {
		reqs = []any{[]any{">=", gemVersion("0")}}
	}
	return userObj{"Gem::Requirement", []any{reqs}}
}

// parseRequirement splits one requirement ("~> 1.2", "1.0") into its
// operator ("=" when none) and version.
func parseRequirement(s string) (op, version string) {
	s = strings.TrimSpace(s)
	for _, o := range []string{">=", "<=", "!=", "~>", "=", ">", "<"} {
		if rest, ok := strings.CutPrefix(s, o); ok {
			return o, strings.TrimSpace(rest)
		}
	}
	return "=", s
}

func gemDependency(d gemDep) object {
	typ := d.Type
	if typ == "" {
		typ = "runtime"
	}
	return object{"Gem::Dependency", hash{
		{sym("@name"), d.Name}, {sym("@requirement"), gemRequirement(d.Requirement)},
		{sym("@type"), sym(typ)}, {sym("@prerelease"), false},
	}}
}

// gemPlatform is a Gem::Platform object for a platform string: cpu-os[-
// version], an os alone ("java") having no cpu.
// ponytail: Gem::Platform#initialize has more rules (mswin, dalvik, universal);
// gem matches platforms by to_s, which this reproduces.
func gemPlatform(p string) any {
	if p == "ruby" {
		return "ruby"
	}
	parts := strings.SplitN(p, "-", 3)
	var cpu, os, version any
	switch len(parts) {
	case 1:
		os = parts[0]
	case 2:
		cpu, os = parts[0], parts[1]
	default:
		cpu, os, version = parts[0], parts[1], parts[2]
	}
	return object{"Gem::Platform", hash{{sym("@cpu"), cpu}, {sym("@os"), os}, {sym("@version"), version}}}
}

// rubyTime is Time#_dump's 8 bytes (a UTC date) with the zone ivar Ruby adds.
func rubyTime(t time.Time) userDump {
	t = t.UTC()
	p := uint32(1)<<31 | uint32(1)<<30 | uint32(t.Year()-1900)<<14 | uint32(t.Month()-1)<<10 | uint32(t.Day())<<5 | uint32(t.Hour())
	s := uint32(t.Minute())<<26 | uint32(t.Second())<<20 | uint32(t.Nanosecond()/1000)
	var b [8]byte
	binary.LittleEndian.PutUint32(b[:4], p)
	binary.LittleEndian.PutUint32(b[4:], s)
	return userDump{"Time", b[:], hash{{sym("zone"), "UTC"}}}
}

// gemSpecification is Gem::Specification#_dump: the 19-element array
// _load reads (specification version 4), itself Marshal-dumped inside a
// 'u' object.
func gemSpecification(a gemAttrs) (userDump, error) {
	date := time.Now().UTC()
	if t, err := time.Parse("2006-01-02", a.Date); err == nil {
		date = t
	}
	deps := make([]any, 0, len(a.Dependencies))
	for _, d := range a.Dependencies {
		deps = append(deps, gemDependency(d))
	}
	platform := a.Platform
	if platform == "" {
		platform = "ruby"
	}
	metadata := make(hash, 0, len(a.Metadata))
	for _, k := range sortedKeys(a.Metadata) {
		metadata = append(metadata, hashKV{k, a.Metadata[k]})
	}
	rubygemsVersion := a.RubygemsVersion
	if rubygemsVersion == "" {
		rubygemsVersion = "3.0.0"
	}
	data, err := marshalDump([]any{
		rubygemsVersion, 4, a.Name, gemVersion(a.Version), rubyTime(date), a.Summary,
		gemRequirement(a.RequiredRubyVersion), gemRequirement(a.RequiredRubygemsVersion), platform, deps,
		"", strs(a.Email), strs(a.Authors), a.Description, a.Homepage, true, gemPlatform(platform), strs(a.Licenses), metadata,
	})
	return userDump{class: "Gem::Specification", data: data}, err
}

func strs(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}
