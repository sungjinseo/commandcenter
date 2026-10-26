package zipdir

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"testing"
)

// headers returns n central directory headers without name, extra or comment.
func headers(n int) []byte {
	b := make([]byte, 46*n)
	for i := range n {
		copy(b[46*i:], "PK\x01\x02")
	}
	return b
}

func zipOf(names ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		zw.Create(n)
	}
	zw.Close()
	return buf.Bytes()
}

func TestOK(t *testing.T) {
	le := binary.LittleEndian
	// Past the end record: the first header's comment is the record, and
	// zip.NewReader goes on to read the 29 headers after it.
	past := headers(30)
	le.PutUint16(past[32:], 22)
	eocd := make([]byte, 22)
	copy(eocd, "PK\x05\x06")
	le.PutUint16(eocd[8:], 30)
	le.PutUint16(eocd[10:], 30)
	le.PutUint32(eocd[12:], 46)
	past = append(append(append(past[:46:46], eocd...), past[46:]...), make([]byte, 46)...)
	// Data in front: the record puts the directory at 0, where zip.NewReader
	// finds a header and so reads 65537 headers (one, modulo 65536) instead
	// of the real directory.
	front := append(headers(65537), zipOf("a")...)
	le.PutUint32(front[len(front)-22+16:], 0)

	for _, c := range []struct {
		name       string
		z          []byte
		read       int // headers zip.NewReader reads
		maxEntries int
		maxBytes   int64
		ok         bool
	}{
		{"archive", zipOf("a", "b", "c"), 3, 3, 1 << 20, true},
		{"archive over the entry limit", zipOf("a", "b", "c"), 3, 2, 1 << 20, false},
		{"past the end record", past, 30, 100, 1 << 20, true},
		{"past the end record over the byte limit", past, 30, 100, 1000, false},
		{"data in front", front, 65537, 50000, 4 << 20, false},
	} {
		r := bytes.NewReader(c.z)
		zr, err := zip.NewReader(r, r.Size())
		if err != nil || len(zr.File) != c.read {
			t.Fatalf("%s: zip.NewReader: %v, want %d headers read", c.name, err, c.read)
		}
		if got := OK(r, r.Size(), c.maxEntries, c.maxBytes); got != c.ok {
			t.Errorf("%s: OK = %v, want %v", c.name, got, c.ok)
		}
	}
}
