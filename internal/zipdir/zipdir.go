// Package zipdir bounds what archive/zip allocates for an uploaded archive.
package zipdir

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
)

// OK reports whether zip.NewReader reads at most maxEntries central directory
// headers, of at most maxBytes in all, from the zip archive r of size bytes,
// and is false when it cannot tell. zip.NewReader allocates a File for each
// header it reads, from the directory start until one is not valid, whatever
// the end records declare (it compares their count only modulo 65536). So OK
// reads those records as readDirectoryEnd does, the zip64 ones included,
// checks what they declare, and walks the headers from where zip.NewReader
// starts. Every zip.NewReader on uploaded data must come after OK.
func OK(r io.ReaderAt, size int64, maxEntries int, maxBytes int64) bool {
	le := binary.LittleEndian
	if size < 22 {
		return false
	}
	n := min(size, 65*1024) // readDirectoryEnd's window
	tail := make([]byte, n)
	if _, err := r.ReadAt(tail, size-n); err != nil && err != io.EOF {
		return false
	}
	i := bytes.LastIndex(tail[:n-18], []byte("PK\x05\x06"))
	if i < 0 || int(le.Uint16(tail[i+20:]))+22 > len(tail)-i {
		return false
	}
	count := uint64(le.Uint16(tail[i+10:]))
	dirSize := uint64(le.Uint32(tail[i+12:]))
	dirOff := uint64(le.Uint32(tail[i+16:]))
	end := uint64(size-n) + uint64(i) // start of the record that ends the directory
	if count == 0xffff || dirSize == 0xffff || dirOff == 0xffffffff {
		// archive/zip would read the zip64 record; so must we, and never trust
		// the 16-bit values when it is missing or invalid.
		if i < 20 || !bytes.Equal(tail[i-20:i-16], []byte("PK\x06\x07")) ||
			le.Uint32(tail[i-16:]) != 0 || le.Uint32(tail[i-4:]) != 1 {
			return false
		}
		z64 := le.Uint64(tail[i-12:])
		if size < 56 || z64 > uint64(size)-56 {
			return false
		}
		rec := make([]byte, 56)
		if _, err := r.ReadAt(rec, int64(z64)); err != nil || !bytes.Equal(rec[:4], []byte("PK\x06\x06")) {
			return false
		}
		count, dirSize, dirOff, end = le.Uint64(rec[32:]), le.Uint64(rec[40:]), le.Uint64(rec[48:]), z64
	}
	if count > uint64(maxEntries) || dirSize > uint64(maxBytes) || dirSize > end || dirOff > end || end-dirOff > uint64(maxBytes) {
		return false
	}
	// zip.NewReader starts at end-dirSize, or at dirOff when that lies before
	// it and holds a valid header (data in front of the archive).
	start := int64(end - dirSize)
	return headersOK(r, size, start, maxEntries, maxBytes) &&
		(int64(dirOff) >= start || headersOK(r, size, int64(dirOff), maxEntries, maxBytes))
}

// headersOK reads central directory headers from off as zip.NewReader does,
// up to the first without the signature or cut short, and reports whether
// there are at most maxEntries of them, of at most maxBytes in all.
func headersOK(r io.ReaderAt, size, off int64, maxEntries int, maxBytes int64) bool {
	br := bufio.NewReader(io.NewSectionReader(r, off, size-off))
	var h [46]byte
	for n, read := 0, int64(0); n <= maxEntries && read <= maxBytes; n++ {
		if _, err := io.ReadFull(br, h[:]); err != nil || binary.LittleEndian.Uint32(h[:]) != 0x02014b50 {
			return true
		}
		m := int(binary.LittleEndian.Uint16(h[28:])) + int(binary.LittleEndian.Uint16(h[30:])) + int(binary.LittleEndian.Uint16(h[32:]))
		if _, err := br.Discard(m); err != nil {
			return true
		}
		read += 46 + int64(m)
	}
	return false
}
