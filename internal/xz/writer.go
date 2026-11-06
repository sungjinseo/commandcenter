package xz

import (
	"bytes"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
)

const (
	// storedChunk is the largest uncompressed LZMA2 chunk.
	storedChunk = 1 << 16
	// storedDict is the properties byte of a 64 KiB dictionary, as much as
	// a stored chunk needs: a reader allocates what the stream declares.
	storedDict = 0x08
)

// StoredWriter writes an .xz file without compressing: one stream of one block
// of uncompressed LZMA2 chunks, CRC64 checked. It needs no compressor and any
// xz reader takes it; the file is the size of the data, 3 bytes for every
// 64 KiB and some 60 bytes of framing more.
type StoredWriter struct {
	w       io.Writer
	buf     []byte
	crc     hash.Hash64
	data    int64 // the block's compressed data bytes written
	size    int64 // the data's bytes written
	started bool
	err     error
}

// NewStoredWriter returns a writer of the .xz file that goes to w. Close
// finishes the file; it does not close w.
func NewStoredWriter(w io.Writer) *StoredWriter {
	return &StoredWriter{w: w, buf: make([]byte, 0, storedChunk), crc: crc64.New(crc64Table)}
}

func (z *StoredWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 && z.err == nil {
		m := min(len(p), storedChunk-len(z.buf))
		z.buf = append(z.buf, p[:m]...)
		z.crc.Write(p[:m])
		z.size += int64(m)
		p = p[m:]
		n += m
		if len(z.buf) == storedChunk {
			z.chunk()
		}
	}
	return n, z.err
}

func (z *StoredWriter) put(b []byte) {
	if z.err == nil {
		_, z.err = z.w.Write(b)
	}
}

// start writes the stream and block headers.
func (z *StoredWriter) start() {
	z.started = true
	flags := []byte{0, checkCRC64}
	z.put(streamMagic)
	z.put(flags)
	z.put(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(flags)))
	// Size (12 bytes), no flags (one filter, no sizes), LZMA2 with its
	// properties, padding, CRC32.
	h := []byte{12/4 - 1, 0, filterLZMA2, 1, storedDict, 0, 0, 0}
	z.put(binary.LittleEndian.AppendUint32(h, crc32.ChecksumIEEE(h)))
}

// chunk writes the buffer as a chunk; the first one resets the dictionary.
func (z *StoredWriter) chunk() {
	if !z.started {
		z.start()
	}
	if len(z.buf) == 0 {
		return
	}
	control := byte(2)
	if z.data == 0 {
		control = 1
	}
	z.put([]byte{control, byte((len(z.buf) - 1) >> 8), byte(len(z.buf) - 1)})
	z.put(z.buf)
	z.data += 3 + int64(len(z.buf))
	z.buf = z.buf[:0]
}

// Close writes the rest of the file: the last chunk, the block's end, the index
// and the stream footer.
func (z *StoredWriter) Close() error {
	z.chunk()
	z.put([]byte{0}) // the end of the LZMA2 data
	z.data++
	z.put(make([]byte, (4-z.data%4)%4))
	z.put(binary.LittleEndian.AppendUint64(nil, z.crc.Sum64()))

	const headerSize, checkSize = 12, 8
	var index bytes.Buffer
	index.WriteByte(0)                                                          // the index indicator
	index.WriteByte(1)                                                          // one block
	index.Write(binary.AppendUvarint(nil, uint64(headerSize+z.data+checkSize))) // its unpadded size
	index.Write(binary.AppendUvarint(nil, uint64(z.size)))                      // its uncompressed size
	index.Write(make([]byte, (4-index.Len()%4)%4))
	z.put(index.Bytes())
	z.put(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(index.Bytes())))

	footer := binary.LittleEndian.AppendUint32(nil, uint32((index.Len()+4)/4-1)) // the backward size
	footer = append(footer, 0, checkCRC64)
	z.put(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(footer)))
	z.put(footer)
	z.put([]byte("YZ"))
	return z.err
}
