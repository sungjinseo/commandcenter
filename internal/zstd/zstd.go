// Package zstd decompresses Zstandard data (RFC 8878) with the standard
// library only: one or more frames, skippable frames among them; raw, RLE
// and compressed blocks; Huffman-coded literals (one or four streams, a new
// or the previous table); sequences in predefined, RLE, FSE or repeat mode;
// the content size and the content checksum (XXH64) verified when the frame
// has them. Frames that need a dictionary are refused.
//
// The window is allocated as the output grows, up to the size the frame
// declares; a declared size above Options.MaxWindow is refused before
// anything is decoded. Corrupt input is an error, never a panic. The output
// is not bounded: callers reading untrusted data wrap the reader in
// io.LimitReader.
package zstd

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// DefaultMaxWindow is the window limit of a zero Options: what zstd
// levels 17 to 19 (and --long) use, and the size decoders must support
// (RFC 8878, section 3.1.1.1.2).
const DefaultMaxWindow = 8 << 20

// Options bound a reader's memory.
type Options struct {
	// MaxWindow is the largest window a frame may declare, in bytes; 0
	// means DefaultMaxWindow.
	MaxWindow int
}

const (
	frameMagic     = 0xfd2fb528
	skippableMagic = 0x184d2a50 // to 0x184d2a5f
	maxBlock       = 128 << 10
)

func corrupt(format string, args ...any) error {
	return fmt.Errorf("zstd: corrupt data: "+format, args...)
}

// NewReader reads the Zstandard data of r. It reads up to the first
// frame's header and fails if r does not start with a frame.
func NewReader(r io.Reader, opts Options) (io.Reader, error) {
	z := &reader{in: bufio.NewReader(r), maxWin: opts.MaxWindow}
	if z.maxWin <= 0 {
		z.maxWin = DefaultMaxWindow
	}
	if err := z.frame(true); err != nil {
		return nil, err
	}
	return z, nil
}

type phase int

const (
	atFrame    phase = iota // at a frame's magic number
	atBlock                 // at a block header
	atFrameEnd              // after the last block
	atEnd                   // the end of the data was read
)

type reader struct {
	in     *bufio.Reader
	maxWin int
	win    window
	err    error
	phase  phase

	// the frame
	contentSize int64 // -1 if not declared
	checksum    bool
	blockMax    int
	out         int64 // bytes returned
	xxh         xxh64

	// what blocks take from the ones before them
	huff  huffTable
	seq   [3]*fseTable // literals lengths, offsets, match lengths
	own   [3]fseTable
	rep   [3]int
	block []byte
	lits  []byte
}

func (z *reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for z.win.pending == 0 {
		if z.err != nil {
			return 0, z.err
		}
		z.err = z.fill()
	}
	n := z.win.read(p)
	if z.checksum {
		z.xxh.write(p[:n])
	}
	z.out += int64(n)
	return n, nil
}

// fill makes progress: it decodes a block or moves to the next part of the
// data. It is called only when all output was read.
func (z *reader) fill() error {
	switch z.phase {
	case atFrame:
		return z.frame(false)
	case atBlock:
		return z.nextBlock()
	case atFrameEnd:
		if z.contentSize >= 0 && z.out != z.contentSize {
			return corrupt("the frame has %d bytes, its header says %d", z.out, z.contentSize)
		}
		if z.checksum {
			var b [4]byte
			if err := z.full(b[:]); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(b[:]) != uint32(z.xxh.sum()) {
				return corrupt("content checksum mismatch")
			}
		}
		z.phase = atFrame
		return nil
	}
	return io.EOF
}

// full reads len(b) bytes; the end of the data is io.ErrUnexpectedEOF.
func (z *reader) full(b []byte) error {
	_, err := io.ReadFull(z.in, b)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// frame reads skippable frames and a frame header, or the end of the data
// (not before the first frame).
func (z *reader) frame(first bool) error {
	for skipped := false; ; skipped = true {
		var m [4]byte
		n, err := io.ReadFull(z.in, m[:])
		switch {
		case n == 0 && err == io.EOF && first && !skipped:
			return errors.New("zstd: not a zstd frame")
		case n == 0 && err == io.EOF:
			z.phase = atEnd
			return nil
		case err != nil && first && !skipped:
			return errors.New("zstd: not a zstd frame")
		case err != nil:
			return io.ErrUnexpectedEOF
		}
		magic := binary.LittleEndian.Uint32(m[:])
		if magic&^0xf == skippableMagic {
			if err := z.full(m[:]); err != nil {
				return err
			}
			size := int64(binary.LittleEndian.Uint32(m[:]))
			if n, _ := io.CopyN(io.Discard, z.in, size); n != size {
				return io.ErrUnexpectedEOF
			}
			continue
		}
		if magic != frameMagic {
			if first {
				return errors.New("zstd: not a zstd frame")
			}
			return corrupt("data after the last frame")
		}
		return z.frameHeader()
	}
}

func (z *reader) frameHeader() error {
	var h [14]byte
	if err := z.full(h[:1]); err != nil {
		return err
	}
	fhd := h[0]
	single := fhd&0x20 != 0
	if fhd&0x08 != 0 {
		return corrupt("reserved bit set in the frame header")
	}
	didSize := [4]int{0, 1, 2, 4}[fhd&3]
	fcsSize := [4]int{0, 2, 4, 8}[fhd>>6]
	if single && fcsSize == 0 {
		fcsSize = 1
	}
	wdSize := 1
	if single {
		wdSize = 0
	}
	b := h[1 : 1+wdSize+didSize+fcsSize]
	if err := z.full(b); err != nil {
		return err
	}
	var window uint64
	if !single {
		exp, mant := uint64(b[0]>>3), uint64(b[0]&7)
		base := uint64(1) << (10 + exp)
		window = base + base/8*mant
		b = b[1:]
	}
	if did := le(b[:didSize]); did != 0 {
		return fmt.Errorf("zstd: the frame needs dictionary %d: dictionaries are not supported", did)
	}
	b = b[didSize:]
	z.contentSize = -1
	if fcsSize > 0 {
		fcs := le(b)
		if fcsSize == 2 {
			fcs += 256
		}
		if fcs >= 1<<62 {
			return corrupt("frame content size %d", fcs)
		}
		z.contentSize = int64(fcs)
		if single {
			window = fcs
		}
	}
	if window > uint64(z.maxWin) {
		return fmt.Errorf("zstd: the window size %d is above the limit %d (compressed with --long or --ultra?)", window, z.maxWin)
	}
	z.win.reset(int(window))
	z.blockMax = min(int(window), maxBlock)
	z.checksum = fhd&0x04 != 0
	z.xxh.reset()
	z.out = 0
	z.huff.valid = false
	z.seq = [3]*fseTable{}
	z.rep = [3]int{1, 4, 8}
	z.phase = atBlock
	return nil
}

// le is b as a little-endian number.
func le(b []byte) uint64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

// nextBlock decodes a block into the window: its 3-byte header is the last
// block flag, the type (raw, RLE, compressed) and the size.
func (z *reader) nextBlock() error {
	var h [3]byte
	if err := z.full(h[:]); err != nil {
		return err
	}
	v := uint32(h[0]) | uint32(h[1])<<8 | uint32(h[2])<<16
	size := int(v >> 3)
	if size > z.blockMax {
		return corrupt("block of %d bytes, above the maximum of %d", size, z.blockMax)
	}
	switch v >> 1 & 3 {
	case 0: // raw
		if err := z.readBlock(size); err != nil {
			return err
		}
		z.win.write(z.block)
	case 1: // RLE
		c, err := z.in.ReadByte()
		if err != nil {
			return io.ErrUnexpectedEOF
		}
		for range size {
			z.win.put(c)
		}
	case 2:
		if err := z.readBlock(size); err != nil {
			return err
		}
		if err := z.compressed(z.block); err != nil {
			return err
		}
	default:
		return corrupt("reserved block type")
	}
	if v&1 != 0 {
		z.phase = atFrameEnd
	}
	return nil
}

func (z *reader) readBlock(size int) error {
	if cap(z.block) < size {
		z.block = make([]byte, maxBlock)
	}
	z.block = z.block[:size]
	return z.full(z.block)
}

// window is the history: the last size bytes of the frame's output,
// allocated as the output grows. The last pending bytes have not been
// read yet.
type window struct {
	buf     []byte
	size    int
	pos     int   // where the next byte goes
	total   int64 // bytes written in the frame
	pending int
}

// reset starts a frame (there must be nothing pending).
func (w *window) reset(size int) {
	w.buf, w.size, w.pos, w.total = w.buf[:0], size, 0, 0
}

func (w *window) put(c byte) {
	if w.pos < len(w.buf) {
		w.buf[w.pos] = c
	} else {
		if len(w.buf) == cap(w.buf) {
			nb := make([]byte, len(w.buf), min(max(2*cap(w.buf), 64<<10), w.size))
			copy(nb, w.buf)
			w.buf = nb
		}
		w.buf = append(w.buf, c)
	}
	if w.pos++; w.pos == w.size {
		w.pos = 0
	}
	w.total++
	w.pending++
}

func (w *window) write(b []byte) {
	for _, c := range b {
		w.put(c)
	}
}

// match repeats n bytes from dist back, 1 <= dist <= min(total, size).
func (w *window) match(dist, n int) {
	for ; n > 0; n-- {
		i := w.pos - dist
		if i < 0 {
			i += len(w.buf)
		}
		w.put(w.buf[i])
	}
}

// read moves pending bytes to p.
func (w *window) read(p []byte) int {
	n := min(len(p), w.pending)
	start := w.pos - w.pending
	if start < 0 {
		start += len(w.buf)
	}
	c := copy(p[:n], w.buf[start:])
	copy(p[c:n], w.buf)
	w.pending -= n
	return n
}

// xxh64 is XXH64 with seed 0, of which a frame's checksum is the low 32 bits.
type xxh64 struct {
	v     [4]uint64
	buf   [32]byte
	n     int
	total uint64
}

const (
	prime1 = 0x9e3779b185ebca87
	prime2 = 0xc2b2ae3d27d4eb4f
	prime3 = 0x165667b19e3779f9
	prime4 = 0x85ebca77c2b2ae63
	prime5 = 0x27d4eb2f165667c5
)

func rotl(x uint64, r uint) uint64 { return x<<r | x>>(64-r) }

func round(acc, in uint64) uint64 { return rotl(acc+in*prime2, 31) * prime1 }

func (h *xxh64) reset() {
	p1 := uint64(prime1)
	*h = xxh64{v: [4]uint64{p1 + prime2, prime2, 0, -p1}}
}

func (h *xxh64) write(b []byte) {
	h.total += uint64(len(b))
	if h.n > 0 {
		c := copy(h.buf[h.n:], b)
		if h.n += c; h.n < 32 {
			return
		}
		h.stripe(h.buf[:])
		h.n, b = 0, b[c:]
	}
	for ; len(b) >= 32; b = b[32:] {
		h.stripe(b)
	}
	h.n = copy(h.buf[:], b)
}

func (h *xxh64) stripe(b []byte) {
	for i := range h.v {
		h.v[i] = round(h.v[i], binary.LittleEndian.Uint64(b[8*i:]))
	}
}

func (h *xxh64) sum() uint64 {
	var acc uint64
	if h.total >= 32 {
		v := h.v
		acc = rotl(v[0], 1) + rotl(v[1], 7) + rotl(v[2], 12) + rotl(v[3], 18)
		for _, x := range v {
			acc = (acc^round(0, x))*prime1 + prime4
		}
	} else {
		acc = prime5
	}
	acc += h.total
	b := h.buf[:h.n]
	for ; len(b) >= 8; b = b[8:] {
		acc = rotl(acc^round(0, binary.LittleEndian.Uint64(b)), 27)*prime1 + prime4
	}
	if len(b) >= 4 {
		acc = rotl(acc^uint64(binary.LittleEndian.Uint32(b))*prime1, 23)*prime2 + prime3
		b = b[4:]
	}
	for _, c := range b {
		acc = rotl(acc^uint64(c)*prime5, 11) * prime1
	}
	acc ^= acc >> 33
	acc *= prime2
	acc ^= acc >> 29
	acc *= prime3
	return acc ^ acc>>32
}
