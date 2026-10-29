package zstd

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// The fixtures in testdata are the inputs below compressed by zstd 1.5.7
// (the postgres:17-alpine image's):
//
//	go test -run TestInputs ./internal/zstd -inputs=DIR && cd DIR
//	for l in 1 3 19; do zstd -q -C -$l -c text > text-$l.zst; done
//	zstd -q --ultra -22 --long=23 -c text > text-22.zst # the window fitted to the input
//	zstd -q --ultra -22 --long=23 -c < text > text-22-stdin.zst # an 8 MiB window
//	zstd -q --ultra -22 -c < text > text-ultra.zst # a 128 MiB window
//	zstd -q --no-check -3 -c text > text-nocheck.zst
//	zstd -q -c random > random.zst; zstd -q -c zeros > zeros.zst; zstd -q -c empty > empty.zst
//	zstd -q -c records > records.zst
//	for l in 3 19; do zstd -q -$l --target-compressed-block-size=1340 -c text > text-small-$l.zst; done
//	for l in 3 19; do zstd -q -$l -c log > log-$l.zst; done
//	cat text-3.zst random.zst zeros.zst > multi.zst
var inputsDir = flag.String("inputs", "", "write the fixtures' inputs to this directory")

// prng is splitmix64: the same bytes on every platform and Go release.
type prng uint64

func (p *prng) next() uint64 {
	*p += 0x9e3779b97f4a7c15
	z := uint64(*p)
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	return z ^ z>>31
}

// text is n bytes of words, numbers and line ends.
func text(n int, seed uint64) []byte {
	words := strings.Fields("the quick brown fox jumps over a lazy dog while registry stores package version release index " +
		"block stream frame window literal match offset length entropy table state symbol checksum header footer " +
		"padding dictionary repository upload download proxy hosted group metadata control archive debian conda")
	p := prng(seed)
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(words[p.next()%uint64(len(words))])
		switch p.next() % 12 {
		case 0:
			b.WriteString(".\n")
		case 1:
			fmt.Fprintf(&b, " %d ", p.next()%100000)
		default:
			b.WriteByte(' ')
		}
	}
	return b.Bytes()[:n]
}

// log is n bytes of lines of few words: compressible, more than one block.
func log(n int, seed uint64) []byte {
	words := strings.Fields("upload download package component")
	p := prng(seed)
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(words[p.next()%4])
		if p.next()%8 == 0 {
			b.WriteString("\n")
		} else {
			b.WriteByte(' ')
		}
	}
	return b.Bytes()[:n]
}

// records are lines of 4 random bytes and the same suffix: sequences of
// one literals length, offset and match length.
func records(n int, seed uint64) []byte {
	p := prng(seed)
	var b bytes.Buffer
	for b.Len() < n {
		v := p.next()
		b.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
		b.WriteString(" commandcenter record\n")
	}
	return b.Bytes()[:n]
}

func random(n int, seed uint64) []byte {
	p := prng(seed)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(p.next())
	}
	return b
}

var inputs = map[string][]byte{
	"text":    text(16<<10, 1),
	"log":     log(300<<10, 3),
	"random":  random(3005, 2), // 3005 = 32*93 + 8 + 4 + 1: every XXH64 tail
	"records": records(8<<10, 4),
	"zeros":   make([]byte, 300000), // more than a block: the blocks after the first are RLE
	"empty":   nil,
}

func TestInputs(t *testing.T) {
	if *inputsDir == "" {
		t.Skip("no -inputs")
	}
	for name, b := range inputs {
		if err := os.WriteFile(filepath.Join(*inputsDir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fixture(t testing.TB, name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(b []byte, opts Options) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(b), opts)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestFixtures(t *testing.T) {
	for _, c := range []struct {
		file, input string
		opts        Options
		err         string
	}{
		{"text-1.zst", "text", Options{}, ""},
		{"text-3.zst", "text", Options{}, ""},
		{"text-19.zst", "text", Options{}, ""},
		{"text-22.zst", "text", Options{}, ""},
		{"text-22-stdin.zst", "text", Options{}, ""},
		{"text-22-stdin.zst", "text", Options{MaxWindow: 4 << 20}, "the window size 8388608 is above the limit 4194304"},
		{"text-ultra.zst", "text", Options{}, "the window size 134217728 is above the limit 8388608"},
		{"text-ultra.zst", "text", Options{MaxWindow: 128 << 20}, ""},
		{"text-nocheck.zst", "text", Options{}, ""},
		{"random.zst", "random", Options{}, ""},
		{"zeros.zst", "zeros", Options{}, ""},
		{"empty.zst", "empty", Options{}, ""},
		{"text-small-3.zst", "text", Options{}, ""},
		{"text-small-19.zst", "text", Options{}, ""},
		{"records.zst", "records", Options{}, ""},
		{"log-3.zst", "log", Options{}, ""},
		{"log-19.zst", "log", Options{}, ""},
		{"multi.zst", "multi", Options{}, ""},
	} {
		want := inputs[c.input]
		if c.input == "multi" {
			want = bytes.Join([][]byte{inputs["text"], inputs["random"], inputs["zeros"]}, nil)
		}
		got, err := decode(fixture(t, c.file), c.opts)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %v, want %q", c.file, err, c.err)
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.file, err)
		case c.err == "" && !bytes.Equal(got, want):
			t.Errorf("%s: %d bytes, not the %d of %s", c.file, len(got), len(want), c.input)
		}
	}
}

// TestReaders checks the io.Reader contract (iotest.TestReader) on input
// that arrives a byte at a time.
func TestReaders(t *testing.T) {
	for _, c := range [][2]string{{"text-3.zst", "text"}, {"log-19.zst", "log"}, {"random.zst", "random"}, {"zeros.zst", "zeros"}} {
		r, err := NewReader(iotest.OneByteReader(bytes.NewReader(fixture(t, c[0]))), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := iotest.TestReader(r, inputs[c[1]]); err != nil {
			t.Errorf("%s: %v", c[0], err)
		}
	}
}

// TestLazyWindow: a frame declaring 128 MiB gets what its output needs.
func TestLazyWindow(t *testing.T) {
	r, err := NewReader(bytes.NewReader(fixture(t, "text-ultra.zst")), Options{MaxWindow: 128 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if z := r.(*reader); z.win.size != 128<<20 || cap(z.win.buf) > 1<<20 {
		t.Fatalf("window of %d bytes allocated %d", z.win.size, cap(z.win.buf))
	}
}

// TestFrames: skippable frames anywhere, dictionaries refused, what may
// follow a frame.
func TestFrames(t *testing.T) {
	skip := func(n int) []byte {
		b := binary.LittleEndian.AppendUint32(nil, skippableMagic+uint32(n%16))
		return append(binary.LittleEndian.AppendUint32(b, uint32(n)), make([]byte, n)...)
	}
	text3, random := fixture(t, "text-3.zst"), fixture(t, "random.zst")
	b := bytes.Join([][]byte{skip(0), text3, skip(5), skip(17), random, skip(3)}, nil)
	if got, err := decode(b, Options{}); err != nil || !bytes.Equal(got, append(bytes.Clone(inputs["text"]), inputs["random"]...)) {
		t.Fatalf("with skippable frames: %d bytes, %v", len(got), err)
	}
	if got, err := decode(skip(9), Options{}); err != nil || len(got) != 0 {
		t.Fatalf("only a skippable frame: %d bytes, %v", len(got), err)
	}
	for name, c := range map[string]struct {
		b   []byte
		err string
	}{
		"empty":         {nil, "not a zstd frame"},
		"not zstd":      {[]byte("plain text, not compressed"), "not a zstd frame"},
		"trailing data": {append(bytes.Clone(text3), "junk"...), "data after the last frame"},
		"short skip":    {skip(10)[:12], "unexpected EOF"},
		"dictionary":    {[]byte{0x28, 0xb5, 0x2f, 0xfd, 0x01, 0x58, 0x07}, "needs dictionary 7"},
		"reserved bit":  {[]byte{0x28, 0xb5, 0x2f, 0xfd, 0x08, 0x58}, "reserved bit"},
		"bad checksum":  {append(bytes.Clone(text3[:len(text3)-1]), text3[len(text3)-1]^1), "checksum mismatch"},
	} {
		if _, err := decode(c.b, Options{}); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v, want %q", name, err, c.err)
		}
	}
}

// TestCorrupt: truncations are errors, and changed bytes in a frame with
// a checksum are errors or change nothing (the frame header's unused bit,
// the bits after an FSE table description); nothing panics. Every
// position of the first and last 64 bytes is tried, some 300 of the rest.
func TestCorrupt(t *testing.T) {
	for _, c := range [][2]string{{"text-3.zst", "text"}, {"text-small-19.zst", "text"}, {"log-19.zst", "log"}, {"records.zst", "records"}, {"random.zst", "random"}, {"zeros.zst", "zeros"}, {"multi.zst", "multi"}} {
		name, b := c[0], fixture(t, c[0])
		want, _ := decode(b, Options{})
		complete := map[int]bool{len(b): true}
		if name == "multi.zst" { // its frames end there
			n := len(fixture(t, "text-3.zst"))
			complete[n], complete[n+len(fixture(t, "random.zst"))] = true, true
		}
		step := max(7, len(b)/300)
		for i := range b {
			if i%step != 0 && i > 64 && i < len(b)-64 && !complete[i] {
				continue
			}
			if _, err := decode(b[:i], Options{}); err == nil && !complete[i] {
				t.Errorf("%s cut to %d bytes: no error", name, i)
			}
			m := bytes.Clone(b)
			m[i] ^= 1 << (i % 8)
			if got, err := decode(m, Options{}); err == nil && !bytes.Equal(got, want) {
				t.Errorf("%s with byte %d changed: other output, no error", name, i)
			}
		}
	}
}

// TestHandmade decodes frames built by hand for what the CLI does not
// write for small inputs: a block of literals only, RLE sequence tables,
// the 3-byte sequence count.
func TestHandmade(t *testing.T) {
	frame := func(fhd byte, fcs []byte, block ...byte) []byte {
		b := append([]byte{0x28, 0xb5, 0x2f, 0xfd, fhd}, fcs...)
		h := uint32(len(block))<<3 | 2<<1 | 1 // compressed, last
		return append(append(b, byte(h), byte(h>>8), byte(h>>16)), block...)
	}
	for name, c := range map[string]struct {
		b    []byte
		want string
	}{
		// raw literals "hello", no sequences
		"literals only": {frame(0x00, []byte{0x00}, 5<<3, 'h', 'e', 'l', 'l', 'o', 0), "hello"},
		// raw literals "abcd"; one sequence, the three tables RLE (literals
		// length 4, offset code 2, match length code 0: 3), the offset's 2
		// extra bits 11 (offset value 7: offset 4) under the end mark
		// (a 1 KiB window: the block is larger than its output)
		"RLE tables": {frame(0x00, []byte{0x00}, 4<<3, 'a', 'b', 'c', 'd', 1, 0x54, 4, 2, 0, 0x07), "abcdabc"},
		// RLE literals: 32512 x; 0x7f00 sequences (3-byte count) of 1
		// literal and 3 bytes at the repeat offset 1, no extra bits
		"many sequences": {frame(0xa0, []byte{0x00, 0xfc, 0x01, 0x00}, 0x0d, 0xf0, 0x07, 'x', 0xff, 0, 0, 0x54, 1, 0, 0, 0x01), strings.Repeat("x", 4*0x7f00)},
	} {
		got, err := decode(c.b, Options{})
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
}

func TestXXH64(t *testing.T) {
	// the reference values of XXH64 with seed 0
	for in, want := range map[string]uint64{"": 0xef46db3751d8e999, "a": 0xd24ec4f1a98c6e5b, "abc": 0x44bc2cf5ad770999} {
		var h xxh64
		h.reset()
		h.write([]byte(in))
		if got := h.sum(); got != want {
			t.Errorf("XXH64(%q) = %#x, want %#x", in, got, want)
		}
	}
	// writes of any size give the one-shot sum
	b := inputs["text"][:1000]
	var one xxh64
	one.reset()
	one.write(b)
	for _, step := range []int{1, 7, 31, 32, 33, 100} {
		var h xxh64
		h.reset()
		for i := 0; i < len(b); i += step {
			h.write(b[i:min(i+step, len(b))])
		}
		if h.sum() != one.sum() {
			t.Errorf("writes of %d: %#x, want %#x", step, h.sum(), one.sum())
		}
	}
}

func FuzzReader(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.zst")
	for _, name := range files {
		b, _ := os.ReadFile(name)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if r, err := NewReader(bytes.NewReader(b), Options{}); err == nil {
			io.Copy(io.Discard, io.LimitReader(r, 1<<20))
		}
	})
}
