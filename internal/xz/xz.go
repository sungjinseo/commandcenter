// Package xz decompresses the .xz format (The .xz File Format 1.0.4) and
// the legacy .lzma ("LZMA alone") format with the standard library only.
//
// An .xz file is one or more streams, each a header, blocks, an index and a
// footer, with stream padding between them. A block is decoded through its
// filter chain: LZMA2, optionally after delta filters; the branch/call/jump
// (BCJ) filters are refused. The block check (none, CRC32, CRC64 or SHA-256)
// is verified, as are the block sizes the header and the index declare and
// the index against the blocks.
//
// The dictionary is allocated as the output grows, up to the size the
// stream declares; a declared size above Options.MaxWindow is refused
// before anything is decoded. Corrupt input is an error, never a panic. The
// output is not bounded: callers reading untrusted data wrap the reader in
// io.LimitReader.
package xz

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
)

// DefaultMaxWindow is the dictionary limit of a zero Options: what xz -6
// (the default preset, and dpkg's) declares.
const DefaultMaxWindow = 8 << 20

// Options bound a reader's memory.
type Options struct {
	// MaxWindow is the largest dictionary a stream may declare, in bytes;
	// 0 means DefaultMaxWindow.
	MaxWindow int
}

func (o Options) maxWindow() int {
	if o.MaxWindow <= 0 {
		return DefaultMaxWindow
	}
	return o.MaxWindow
}

func corrupt(format string, args ...any) error {
	return fmt.Errorf("xz: corrupt data: "+format, args...)
}

var (
	streamMagic = []byte{0xfd, '7', 'z', 'X', 'Z', 0}
	crc64Table  = crc64.MakeTable(crc64.ECMA)
)

const (
	checkNone   = 0x00
	checkCRC32  = 0x01
	checkCRC64  = 0x04
	checkSHA256 = 0x0a

	filterDelta = 0x03
	filterLZMA2 = 0x21
)

var checkSizes = map[byte]int{checkNone: 0, checkCRC32: 4, checkCRC64: 8, checkSHA256: 32}

// bcjFilters are the branch/call/jump filters, by the xz option naming them.
var bcjFilters = map[uint64]string{0x04: "x86", 0x05: "powerpc", 0x06: "ia64", 0x07: "arm", 0x08: "armthumb", 0x09: "sparc", 0x0a: "arm64", 0x0b: "riscv"}

// NewReader reads the .xz data of r. It reads the first stream header and
// fails if r does not start with one.
func NewReader(r io.Reader, opts Options) (io.Reader, error) {
	z := &reader{in: bufio.NewReader(r), maxWin: opts.maxWindow()}
	var h [12]byte
	if err := z.full(h[:]); err != nil || !bytes.Equal(h[:6], streamMagic) {
		return nil, errors.New("xz: not an .xz stream")
	}
	if err := z.streamHeader(h); err != nil {
		return nil, err
	}
	return z, nil
}

type phase int

const (
	atBlock     phase = iota // at a block header or the index
	atControl                // at an LZMA2 chunk's control byte
	inCopy                   // in an uncompressed chunk
	inLZMA                   // in an LZMA chunk
	atBlockEnd               // after the LZMA2 end marker
	atStreamEnd              // done
)

type reader struct {
	in     *bufio.Reader
	n      int64 // bytes read from in
	maxWin int
	win    window
	lz     lzma
	err    error
	phase  phase

	// the stream
	flags   [2]byte
	blocks  uint64    // blocks decoded
	records hash.Hash // of the blocks' (unpadded, uncompressed) sizes, to compare with the index's

	// the block
	check       hash.Hash // nil for check none
	checkSize   int
	deltas      []delta
	headerSize  int64
	dataStart   int64 // n where the compressed data began
	compSize    int64 // declared, -1 if not
	uncompSize  int64 // declared, -1 if not
	out         int64 // uncompressed bytes returned
	needReset   bool  // the next chunk must reset the dictionary
	needProps   bool  // the next LZMA chunk must set the properties
	left        int   // bytes the current chunk still produces
	chunk       bytes.Reader
	buf         []byte
	recordBytes [2 * binary.MaxVarintLen64]byte
}

func (z *reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for z.win.pending == 0 {
		if z.err != nil {
			return 0, z.err
		}
		z.err = z.fill(len(p))
	}
	n := z.win.read(p)
	for i := len(z.deltas) - 1; i >= 0; i-- {
		z.deltas[i].decode(p[:n])
	}
	if z.check != nil {
		z.check.Write(p[:n])
	}
	z.out += int64(n)
	return n, nil
}

// fill makes progress: it produces output or moves to the next part of the
// stream. It is called only when all output was read.
func (z *reader) fill(max int) error {
	switch z.phase {
	case atBlock:
		return z.blockHeader()
	case atControl:
		return z.control()
	case inCopy:
		n := min(z.left, max, z.win.size)
		if len(z.buf) < n {
			z.buf = make([]byte, 64<<10)
		}
		if err := z.full(z.buf[:n]); err != nil {
			return err
		}
		z.win.write(z.buf[:n])
		if z.left -= n; z.left == 0 {
			z.phase = atControl
		}
		return nil
	case inLZMA:
		return z.lzmaChunk(max)
	case atBlockEnd:
		return z.blockEnd()
	}
	return io.EOF
}

// full reads len(b) bytes; the end of the data is io.ErrUnexpectedEOF.
func (z *reader) full(b []byte) error {
	n, err := io.ReadFull(z.in, b)
	z.n += int64(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (z *reader) ReadByte() (byte, error) {
	b, err := z.in.ReadByte()
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		z.n++
	}
	return b, err
}

// streamHeader checks the 12-byte stream header h and starts the stream.
func (z *reader) streamHeader(h [12]byte) error {
	if crc32.ChecksumIEEE(h[6:8]) != binary.LittleEndian.Uint32(h[8:]) {
		return corrupt("stream header CRC32 mismatch")
	}
	if h[6] != 0 || h[7]&0xf0 != 0 {
		return fmt.Errorf("xz: unsupported stream flags %#x %#x", h[6], h[7])
	}
	if _, ok := checkSizes[h[7]]; !ok {
		return fmt.Errorf("xz: unsupported check type %#x", h[7])
	}
	z.flags = [2]byte{h[6], h[7]}
	z.blocks = 0
	z.records = crc64.New(crc64Table)
	z.phase = atBlock
	return nil
}

// readVLI reads a variable-length integer: 7 bits a byte, low first, at
// most 9 bytes, no trailing zero byte.
func readVLI(br io.ByteReader) (uint64, error) {
	var v uint64
	for i := 0; i < 9; i++ {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint64(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			if b == 0 && i > 0 {
				return 0, corrupt("integer with a trailing zero byte")
			}
			return v, nil
		}
	}
	return 0, corrupt("integer longer than 9 bytes")
}

func (z *reader) blockHeader() error {
	size, err := z.ReadByte()
	if err != nil {
		return err
	}
	if size == 0 {
		return z.index()
	}
	hdr := make([]byte, (int(size)+1)*4)
	hdr[0] = size
	if err := z.full(hdr[1:]); err != nil {
		return err
	}
	body := hdr[:len(hdr)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(hdr[len(body):]) {
		return corrupt("block header CRC32 mismatch")
	}
	flags := hdr[1]
	if flags&0x3c != 0 {
		return fmt.Errorf("xz: unsupported block flags %#x", flags)
	}
	br := bytes.NewReader(body[2:])
	vli := func(name string, present, zeroOK bool) (int64, error) {
		if !present {
			return -1, nil
		}
		v, err := readVLI(br)
		if err != nil || v == 0 && !zeroOK || v > 1<<62 {
			return 0, corrupt("block header %s", name)
		}
		return int64(v), nil
	}
	if z.compSize, err = vli("compressed size", flags&0x40 != 0, false); err != nil {
		return err
	}
	if z.uncompSize, err = vli("uncompressed size", flags&0x80 != 0, true); err != nil {
		return err
	}
	z.deltas = z.deltas[:0]
	var dict int64
	nfilters := int(flags&3) + 1
	for i := 0; i < nfilters; i++ {
		id, err := readVLI(br)
		if err != nil {
			return corrupt("block header filter flags")
		}
		n, err := readVLI(br)
		if err != nil || n > uint64(br.Len()) {
			return corrupt("block header filter properties")
		}
		props := make([]byte, n)
		br.Read(props)
		last := i == nfilters-1
		switch {
		case id == filterLZMA2 && last:
			if n != 1 || props[0] > 40 {
				return corrupt("LZMA2 properties")
			}
			dict = lzma2Dict(props[0])
		case id == filterDelta && !last:
			if n != 1 {
				return corrupt("delta properties")
			}
			z.deltas = append(z.deltas, delta{dist: int(props[0]) + 1})
		case bcjFilters[id] != "":
			return fmt.Errorf("xz: the BCJ filter --%s is not supported", bcjFilters[id])
		default:
			return fmt.Errorf("xz: unsupported filter chain (filter %#x at position %d of %d; only delta then LZMA2 is supported)", id, i+1, nfilters)
		}
	}
	for br.Len() > 0 {
		if b, _ := br.ReadByte(); b != 0 {
			return corrupt("block header padding")
		}
	}
	if dict > int64(z.maxWin) {
		return fmt.Errorf("xz: the dictionary size %d is above the limit %d (compressed with a preset above -6?)", dict, z.maxWin)
	}
	z.win.reset(max(int(dict), minDict))
	z.check, z.checkSize = newCheck(z.flags[1]), checkSizes[z.flags[1]]
	z.headerSize, z.dataStart, z.out = int64(len(hdr)), z.n, 0
	z.needReset, z.needProps = true, true
	z.phase = atControl
	return nil
}

// lzma2Dict is the dictionary size an LZMA2 properties byte (at most 40)
// encodes: 2 or 3 times a power of two, 4 KiB to 3 GiB, or 4 GiB - 1.
func lzma2Dict(p byte) int64 {
	if p == 40 {
		return 1<<32 - 1
	}
	return (2 | int64(p)&1) << (p/2 + 11)
}

func newCheck(t byte) hash.Hash {
	switch t {
	case checkCRC32:
		return crc32.NewIEEE()
	case checkCRC64:
		return crc64.New(crc64Table)
	case checkSHA256:
		return sha256.New()
	}
	return nil
}

// control reads an LZMA2 chunk header: 0x00 ends the data; 0x01 and 0x02
// start an uncompressed chunk, with and without a dictionary reset; 0x80
// and above an LZMA chunk, bits 5-6 resetting nothing, the state, the
// state and the properties, or all of them and the dictionary, bits 0-4
// the high bits of the uncompressed size less one.
func (z *reader) control() error {
	c, err := z.ReadByte()
	if err != nil {
		return err
	}
	if c == 0 {
		z.phase = atBlockEnd
		return nil
	}
	if c > 2 && c < 0x80 {
		return corrupt("invalid LZMA2 control byte %#x", c)
	}
	if c == 1 || c >= 0xe0 {
		z.win.reset(z.win.size)
		z.needReset, z.needProps = false, true
	} else if z.needReset {
		return corrupt("LZMA2 data does not start with a dictionary reset")
	}
	var h [5]byte
	n := 2
	if c >= 0x80 {
		n = 4
		if c >= 0xc0 {
			n = 5
		}
	}
	if err := z.full(h[:n]); err != nil {
		return err
	}
	if c < 0x80 {
		z.left = int(binary.BigEndian.Uint16(h[:])) + 1
		z.phase = inCopy
		return nil
	}
	z.left = int(c&0x1f)<<16 + int(binary.BigEndian.Uint16(h[:])) + 1
	packed := int(binary.BigEndian.Uint16(h[2:])) + 1
	switch {
	case c >= 0xc0:
		lc, lp, pb, ok := lzmaProps(h[4])
		if !ok || lc+lp > 4 {
			return corrupt("LZMA properties %#x", h[4])
		}
		z.lz.reset(lc, lp, pb)
		z.needProps = false
	case z.needProps:
		return corrupt("LZMA chunk without properties after a dictionary reset")
	case c >= 0xa0:
		z.lz.reset(z.lz.lc, z.lz.lp, z.lz.pb)
	}
	if len(z.buf) < packed {
		z.buf = make([]byte, 64<<10)
	}
	if err := z.full(z.buf[:packed]); err != nil {
		return err
	}
	z.chunk.Reset(z.buf[:packed])
	if err := z.lz.rc.init(&z.chunk); err != nil {
		return err
	}
	z.lz.w = &z.win
	z.phase = inLZMA
	return nil
}

// lzmaChunk decodes up to max bytes of the current LZMA chunk.
func (z *reader) lzmaChunk(max int) error {
	limit := min(max, z.win.size-maxMatchLen)
	for z.win.pending < limit && z.left > 0 {
		before := z.win.pending
		eos, err := z.lz.step(int64(z.left))
		if z.lz.rc.err != nil {
			return corrupt("LZMA chunk ends early")
		}
		if err != nil {
			return err
		}
		if eos {
			return corrupt("end marker in LZMA2 data")
		}
		z.left -= z.win.pending - before
	}
	if z.left == 0 {
		if z.lz.rc.code != 0 || z.chunk.Len() != 0 {
			return corrupt("LZMA chunk size mismatch")
		}
		z.phase = atControl
	}
	return nil
}

func (z *reader) blockEnd() error {
	comp := z.n - z.dataStart
	if z.compSize >= 0 && comp != z.compSize || z.uncompSize >= 0 && z.out != z.uncompSize {
		return corrupt("block sizes differ from its header's")
	}
	pad := make([]byte, (4-comp%4)%4+int64(z.checkSize))
	if err := z.full(pad); err != nil {
		return err
	}
	sum := pad[len(pad)-z.checkSize:]
	if !bytes.Equal(pad[:len(pad)-z.checkSize], make([]byte, len(pad)-z.checkSize)) {
		return corrupt("block padding")
	}
	var ok bool
	switch h := z.check.(type) {
	case nil:
		ok = true
	case hash.Hash32:
		ok = binary.LittleEndian.Uint32(sum) == h.Sum32()
	case hash.Hash64:
		ok = binary.LittleEndian.Uint64(sum) == h.Sum64()
	default:
		ok = bytes.Equal(sum, h.Sum(nil))
	}
	if !ok {
		return corrupt("block check mismatch")
	}
	z.record(z.records, uint64(z.headerSize+comp+int64(z.checkSize)), uint64(z.out))
	z.blocks++
	z.deltas, z.check = z.deltas[:0], nil
	z.phase = atBlock
	return nil
}

func (z *reader) record(h hash.Hash, unpadded, uncompressed uint64) {
	b := binary.AppendUvarint(z.recordBytes[:0], unpadded)
	h.Write(binary.AppendUvarint(b, uncompressed))
}

// hashingReader is the reader's bytes, also written to h.
type hashingReader struct {
	z *reader
	h hash.Hash32
}

func (r hashingReader) ReadByte() (byte, error) {
	b, err := r.z.ReadByte()
	r.h.Write([]byte{b})
	return b, err
}

// index reads the index (its indicator byte read), the stream footer and
// what follows: stream padding, then another stream or the end.
func (z *reader) index() error {
	start := z.n - 1
	crc := crc32.NewIEEE()
	crc.Write([]byte{0})
	hr := hashingReader{z, crc}
	count, err := readVLI(hr)
	if err != nil {
		return err
	}
	if count != z.blocks {
		return corrupt("the index lists %d blocks, the stream has %d", count, z.blocks)
	}
	records := crc64.New(crc64Table)
	for i := uint64(0); i < count; i++ {
		unpadded, err := readVLI(hr)
		if err != nil {
			return err
		}
		uncompressed, err := readVLI(hr)
		if err != nil {
			return err
		}
		z.record(records, unpadded, uncompressed)
	}
	if !bytes.Equal(records.Sum(nil), z.records.Sum(nil)) {
		return corrupt("the index does not match the blocks")
	}
	for (z.n-start)%4 != 0 {
		if b, err := hr.ReadByte(); err != nil || b != 0 {
			return corrupt("index padding")
		}
	}
	var f [16]byte // the index CRC32, then the stream footer
	if err := z.full(f[:]); err != nil {
		return err
	}
	size := z.n - 12 - start
	ft := f[4:]
	switch {
	case binary.LittleEndian.Uint32(f[:]) != crc.Sum32():
		return corrupt("index CRC32 mismatch")
	case crc32.ChecksumIEEE(ft[4:10]) != binary.LittleEndian.Uint32(ft):
		return corrupt("stream footer CRC32 mismatch")
	case string(ft[10:]) != "YZ":
		return corrupt("no stream footer magic")
	case (int64(binary.LittleEndian.Uint32(ft[4:]))+1)*4 != size:
		return corrupt("the stream footer's index size is not the index's")
	case ft[8] != z.flags[0] || ft[9] != z.flags[1]:
		return corrupt("the stream footer's flags are not the header's")
	}
	// stream padding (groups of 4 zero bytes), then a stream or the end
	for {
		var h [12]byte
		n, err := io.ReadFull(z.in, h[:4])
		z.n += int64(n)
		switch {
		case n == 0 && err == io.EOF:
			z.phase = atStreamEnd
			return nil
		case err != nil:
			return corrupt("stream padding")
		case h[0]|h[1]|h[2]|h[3] == 0:
			continue
		}
		if err := z.full(h[4:]); err != nil {
			return err
		}
		if !bytes.Equal(h[:6], streamMagic) {
			return corrupt("data after the stream")
		}
		return z.streamHeader(h)
	}
}

// delta undoes the delta filter: each byte was stored less the byte dist
// before it.
type delta struct {
	dist int
	pos  byte
	hist [256]byte
}

func (d *delta) decode(b []byte) {
	for i := range b {
		b[i] += d.hist[d.pos-byte(d.dist)]
		d.hist[d.pos] = b[i]
		d.pos++
	}
}
