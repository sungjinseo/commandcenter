package xz

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

// The fixtures in testdata are the inputs below compressed by xz 5.2.5:
//
//	go test -run TestInputs ./internal/xz -inputs=DIR && cd DIR
//	for l in 0 6 9e; do xz -k -c -$l text > text-$l.xz; done
//	for c in none crc32 sha256; do xz -c --check=$c text > text-$c.xz; done
//	xz -c --block-size=4KiB text > text-blocks.xz
//	xz -c -T2 --block-size=4KiB text > text-mt.xz # block headers with sizes
//	xz -c --delta=dist=4 --lzma2=preset=6 text > text-delta.xz
//	xz -c --x86 --lzma2 text > text-x86.xz
//	xz -c --format=lzma text > text.lzma
//	xz -c random > random.xz; xz -c zeros > zeros.xz; xz -c < /dev/null > empty.xz
//	(cat text-6.xz; printf '\0\0\0\0'; cat random.xz empty.xz) > concat.xz
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

func random(n int, seed uint64) []byte {
	p := prng(seed)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(p.next())
	}
	return b
}

var inputs = map[string][]byte{
	"text":   text(16<<10, 1),
	"random": random(3000, 2),
	"zeros":  make([]byte, 100000),
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

func decode(t *testing.T, name string, opts Options) ([]byte, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	newReader := NewReader
	if strings.HasSuffix(name, ".lzma") {
		newReader = NewLZMAReader
	}
	r, err := newReader(bytes.NewReader(b), opts)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestFixtures(t *testing.T) {
	big := Options{MaxWindow: 64 << 20}
	for _, c := range []struct {
		file, input string
		opts        Options
		err         string
	}{
		{"text-0.xz", "text", Options{}, ""},
		{"text-6.xz", "text", Options{}, ""},
		{"text-9e.xz", "text", Options{}, "the dictionary size 67108864 is above the limit 8388608"},
		{"text-9e.xz", "text", big, ""},
		{"text-6.xz", "text", Options{MaxWindow: 4 << 20}, "above the limit 4194304"},
		{"text-none.xz", "text", Options{}, ""},
		{"text-crc32.xz", "text", Options{}, ""},
		{"text-sha256.xz", "text", Options{}, ""},
		{"text-blocks.xz", "text", Options{}, ""},
		{"text-mt.xz", "text", Options{}, ""},
		{"text-delta.xz", "text", Options{}, ""},
		{"text-x86.xz", "text", Options{}, "the BCJ filter --x86 is not supported"},
		{"random.xz", "random", Options{}, ""},
		{"zeros.xz", "zeros", Options{}, ""},
		{"empty.xz", "", Options{}, ""},
		{"concat.xz", "concat", Options{}, ""},
		{"text.lzma", "text", Options{}, ""},
	} {
		want := inputs[c.input]
		if c.input == "concat" {
			want = append(append([]byte(nil), inputs["text"]...), inputs["random"]...)
		}
		got, err := decode(t, c.file, c.opts)
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
	for _, c := range [][2]string{{"text-6.xz", "text"}, {"text-blocks.xz", "text"}, {"text-delta.xz", "text"}, {"random.xz", "random"}, {"text.lzma", "text"}} {
		b, _ := os.ReadFile(filepath.Join("testdata", c[0]))
		newReader := NewReader
		if strings.HasSuffix(c[0], ".lzma") {
			newReader = NewLZMAReader
		}
		r, err := newReader(iotest.OneByteReader(bytes.NewReader(b)), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := iotest.TestReader(r, inputs[c[1]]); err != nil {
			t.Errorf("%s: %v", c[0], err)
		}
	}
}

// TestLazyWindow: a stream declaring 64 MiB gets what its output needs.
func TestLazyWindow(t *testing.T) {
	b, _ := os.ReadFile("testdata/text-9e.xz")
	r, err := NewReader(bytes.NewReader(b), Options{MaxWindow: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if z := r.(*reader); z.win.size != 64<<20 || cap(z.win.buf) > 1<<20 {
		t.Fatalf("window of %d bytes allocated %d", z.win.size, cap(z.win.buf))
	}
}

// TestCorrupt: truncations are errors, and so are changed bytes in a
// stream with a check; nothing panics. Every position of the headers,
// index and footer is tried, every 7th of the rest.
func TestCorrupt(t *testing.T) {
	text6, _ := os.ReadFile("testdata/text-6.xz")
	for _, name := range []string{"text-6.xz", "text-mt.xz", "random.xz", "concat.xz", "text-delta.xz", "text.lzma"} {
		b, _ := os.ReadFile(filepath.Join("testdata", name))
		newReader := NewReader
		if strings.HasSuffix(name, ".lzma") {
			newReader = NewLZMAReader
		}
		run := func(b []byte) error {
			r, err := newReader(bytes.NewReader(b), Options{})
			if err == nil {
				_, err = io.Copy(io.Discard, r)
			}
			return err
		}
		complete := map[int]bool{len(b): true}
		if name == "concat.xz" { // its streams end there
			complete[len(text6)], complete[len(text6)+4], complete[len(b)-32] = true, true, true
		}
		for i := range b {
			if i%7 != 0 && i > 64 && i < len(b)-64 && !complete[i] {
				continue
			}
			if run(b[:i]) == nil && !complete[i] {
				t.Errorf("%s cut to %d bytes: no error", name, i)
			}
			c := bytes.Clone(b)
			c[i] ^= 1 << (i % 8)
			if err := run(c); err == nil && name != "text.lzma" {
				t.Errorf("%s with byte %d changed: no error", name, i)
			}
		}
	}
}

func TestLZMAKnownSize(t *testing.T) {
	b, _ := os.ReadFile("testdata/text.lzma")
	want := inputs["text"]
	// the size in the header, the end marker still there: both are read
	sized := bytes.Clone(b)
	binary.LittleEndian.PutUint64(sized[5:], uint64(len(want)))
	r, err := NewLZMAReader(bytes.NewReader(sized), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
	// a declared size the data does not reach, or exceeds
	for _, n := range []int{len(want) + 1, len(want) - 1} {
		binary.LittleEndian.PutUint64(sized[5:], uint64(n))
		r, _ := NewLZMAReader(bytes.NewReader(sized), Options{})
		if _, err := io.ReadAll(r); err == nil {
			t.Errorf("size %d: no error", n)
		}
	}
	for _, h := range [][]byte{nil, []byte("short"), append([]byte{225}, b[1:]...)} {
		if _, err := NewLZMAReader(bytes.NewReader(h), Options{}); err == nil {
			t.Errorf("%q: no error", h)
		}
	}
	big := bytes.Clone(b)
	binary.LittleEndian.PutUint32(big[1:], 64<<20)
	if _, err := NewLZMAReader(bytes.NewReader(big), Options{}); err == nil || !strings.Contains(err.Error(), "above the limit") {
		t.Errorf("64 MiB dictionary: %v", err)
	}
}

func TestNotXZ(t *testing.T) {
	for _, b := range []string{"", "plain text that is long enough", "\xfd7zXZ\x00\x00\x04garbage!"} {
		if _, err := NewReader(strings.NewReader(b), Options{}); err == nil {
			t.Errorf("%q: no error", b)
		}
	}
	// trailing garbage after a stream
	b, _ := os.ReadFile("testdata/empty.xz")
	r, _ := NewReader(bytes.NewReader(append(b, "junk"...)), Options{})
	if _, err := io.ReadAll(r); err == nil {
		t.Error("trailing data: no error")
	}
}

func FuzzReader(f *testing.F) {
	files, _ := filepath.Glob("testdata/*")
	for _, name := range files {
		b, _ := os.ReadFile(name)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, newReader := range []func(io.Reader, Options) (io.Reader, error){NewReader, NewLZMAReader} {
			if r, err := newReader(bytes.NewReader(b), Options{}); err == nil {
				io.Copy(io.Discard, io.LimitReader(r, 1<<20))
			}
		}
	})
}
