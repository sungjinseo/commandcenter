package xz

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The LZMA decoder, as the LZMA SDK's lzma-specification.txt describes
// it: a range decoder of adaptive bit probabilities, literals coded with
// the previous byte's high bits and the position's low bits as context,
// matches as a length and a distance, the four last distances reusable.

const (
	maxMatchLen = 273
	minDict     = 4096
	probInit    = 1 << 10
)

// window is the dictionary: the last size bytes of the output, allocated
// as the output grows. The last pending bytes have not been read yet.
type window struct {
	buf     []byte
	size    int
	pos     int   // where the next byte goes
	total   int64 // bytes written since the last reset
	pending int
}

// reset forgets the history (there must be nothing pending) and sets the
// size of the next one.
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

// at is the byte dist back, 1 <= dist <= min(total, size).
func (w *window) at(dist int) byte {
	i := w.pos - dist
	if i < 0 {
		i += len(w.buf)
	}
	return w.buf[i]
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

type rangeDecoder struct {
	br        io.ByteReader
	rng, code uint32
	err       error // of br; the decoder then reads zero bytes
}

func (rc *rangeDecoder) init(br io.ByteReader) error {
	rc.br, rc.rng, rc.code, rc.err = br, 0xffffffff, 0, nil
	var b [5]byte
	for i := range b {
		c, err := br.ReadByte()
		if err != nil {
			return corrupt("LZMA data ends early")
		}
		b[i] = c
	}
	rc.code = binary.BigEndian.Uint32(b[1:])
	if b[0] != 0 || rc.code == rc.rng {
		return corrupt("LZMA range coder start")
	}
	return nil
}

// shift takes the next input byte, when the range is below 1<<24.
func (rc *rangeDecoder) shift() {
	c, err := rc.br.ReadByte()
	if err != nil && rc.err == nil {
		rc.err = err
	}
	rc.rng <<= 8
	rc.code = rc.code<<8 | uint32(c)
}

func (rc *rangeDecoder) bit(p *uint16) uint32 {
	v := uint32(*p)
	bound := (rc.rng >> 11) * v
	var b uint32
	if rc.code < bound {
		rc.rng = bound
		*p = uint16(v + (1<<11-v)>>5)
	} else {
		rc.rng -= bound
		rc.code -= bound
		*p = uint16(v - v>>5)
		b = 1
	}
	if rc.rng < 1<<24 {
		rc.shift()
	}
	return b
}

// direct reads n bits of probability one half.
func (rc *rangeDecoder) direct(n uint) uint32 {
	var v uint32
	for ; n > 0; n-- {
		rc.rng >>= 1
		var b uint32
		if rc.code >= rc.rng {
			rc.code -= rc.rng
			b = 1
		}
		v = v<<1 | b
		if rc.rng < 1<<24 {
			rc.shift()
		}
	}
	return v
}

// tree reads an n-bit symbol, high bit first, from a tree of 1<<n probabilities.
func (rc *rangeDecoder) tree(probs []uint16, n uint) uint32 {
	m := uint32(1)
	for i := uint(0); i < n; i++ {
		m = m<<1 | rc.bit(&probs[m])
	}
	return m - 1<<n
}

// reverse reads an n-bit symbol, low bit first.
func (rc *rangeDecoder) reverse(probs []uint16, n uint) uint32 {
	m, sym := uint32(1), uint32(0)
	for i := uint(0); i < n; i++ {
		b := rc.bit(&probs[m])
		m = m<<1 | b
		sym |= b << i
	}
	return sym
}

type lengthDecoder struct {
	choice, choice2 uint16
	low, mid        [16][8]uint16
	high            [256]uint16
}

// decode is a match length less 2, 0 to 271.
func (l *lengthDecoder) decode(rc *rangeDecoder, posState uint32) uint32 {
	if rc.bit(&l.choice) == 0 {
		return rc.tree(l.low[posState][:], 3)
	}
	if rc.bit(&l.choice2) == 0 {
		return 8 + rc.tree(l.mid[posState][:], 3)
	}
	return 16 + rc.tree(l.high[:], 8)
}

type lzma struct {
	lc, lp, pb uint
	state      uint32
	rep        [4]uint32
	rc         rangeDecoder
	w          *window

	literal    []uint16
	isMatch    [12 << 4]uint16
	isRep      [12]uint16
	isRepG0    [12]uint16
	isRepG1    [12]uint16
	isRepG2    [12]uint16
	isRep0Long [12 << 4]uint16
	posSlot    [4][64]uint16
	posSpecial [1 + 128 - 14]uint16
	align      [16]uint16
	matchLen   lengthDecoder
	repLen     lengthDecoder
}

// lzmaProps splits a properties byte, (pb*5 + lp)*9 + lc.
func lzmaProps(b byte) (lc, lp, pb uint, ok bool) {
	if b >= 9*5*5 {
		return 0, 0, 0, false
	}
	return uint(b % 9), uint(b / 9 % 5), uint(b / 45), true
}

// reset sets the properties and starts over: probabilities, state, distances.
func (d *lzma) reset(lc, lp, pb uint) {
	d.lc, d.lp, d.pb = lc, lp, pb
	d.state, d.rep = 0, [4]uint32{}
	n := 0x300 << (lc + lp)
	if cap(d.literal) < n {
		d.literal = make([]uint16, n)
	}
	d.literal = d.literal[:n]
	fill := func(p []uint16) {
		for i := range p {
			p[i] = probInit
		}
	}
	fill(d.literal)
	fill(d.isMatch[:])
	fill(d.isRep[:])
	fill(d.isRepG0[:])
	fill(d.isRepG1[:])
	fill(d.isRepG2[:])
	fill(d.isRep0Long[:])
	for i := range d.posSlot {
		fill(d.posSlot[i][:])
	}
	fill(d.posSpecial[:])
	fill(d.align[:])
	for _, l := range []*lengthDecoder{&d.matchLen, &d.repLen} {
		l.choice, l.choice2 = probInit, probInit
		for i := range l.low {
			fill(l.low[i][:])
			fill(l.mid[i][:])
		}
		fill(l.high[:])
	}
}

var errLZMA = corrupt("invalid LZMA data")

// step decodes one literal or match into the window. left bounds what it
// may produce (-1: no bound). eos reports the end marker.
func (d *lzma) step(left int64) (eos bool, err error) {
	w := d.w
	posState := uint32(w.total) & (1<<d.pb - 1)
	s := d.state
	if d.rc.bit(&d.isMatch[s<<4+posState]) == 0 {
		if left == 0 {
			return false, errLZMA
		}
		return false, d.decodeLiteral()
	}
	var length uint32
	if d.rc.bit(&d.isRep[s]) != 0 {
		if left == 0 || w.total == 0 {
			return false, errLZMA
		}
		if d.rc.bit(&d.isRepG0[s]) == 0 {
			if d.rc.bit(&d.isRep0Long[s<<4+posState]) == 0 {
				d.state = 9
				if s >= 7 {
					d.state = 11
				}
				return false, d.copy(1)
			}
		} else {
			var dist uint32
			if d.rc.bit(&d.isRepG1[s]) == 0 {
				dist = d.rep[1]
			} else {
				if d.rc.bit(&d.isRepG2[s]) == 0 {
					dist = d.rep[2]
				} else {
					dist = d.rep[3]
					d.rep[3] = d.rep[2]
				}
				d.rep[2] = d.rep[1]
			}
			d.rep[1] = d.rep[0]
			d.rep[0] = dist
		}
		length = d.repLen.decode(&d.rc, posState)
		d.state = 8
		if s >= 7 {
			d.state = 11
		}
	} else {
		d.rep[3], d.rep[2], d.rep[1] = d.rep[2], d.rep[1], d.rep[0]
		length = d.matchLen.decode(&d.rc, posState)
		d.state = 7
		if s >= 7 {
			d.state = 10
		}
		d.rep[0] = d.distance(length)
		if d.rep[0] == 0xffffffff {
			return true, nil
		}
		if left == 0 {
			return false, errLZMA
		}
	}
	n := int(length) + 2
	if left >= 0 && int64(n) > left {
		return false, corrupt("LZMA match past the end of the data")
	}
	return false, d.copy(n)
}

func (d *lzma) decodeLiteral() error {
	w := d.w
	prev := uint32(0)
	if w.total > 0 {
		prev = uint32(w.at(1))
	}
	ctx := (uint32(w.total)&(1<<d.lp-1))<<d.lc + prev>>(8-d.lc)
	probs := d.literal[0x300*ctx : 0x300*ctx+0x300]
	sym := uint32(1)
	if d.state >= 7 {
		dist := int64(d.rep[0]) + 1
		if dist > w.total {
			return errLZMA
		}
		match := uint32(w.at(int(dist)))
		for sym < 0x100 {
			mbit := match >> 7 & 1
			match <<= 1
			b := d.rc.bit(&probs[(1+mbit)<<8+sym])
			sym = sym<<1 | b
			if b != mbit {
				break
			}
		}
	}
	for sym < 0x100 {
		sym = sym<<1 | d.rc.bit(&probs[sym])
	}
	w.put(byte(sym))
	switch s := d.state; {
	case s < 4:
		d.state = 0
	case s < 10:
		d.state = s - 3
	default:
		d.state = s - 6
	}
	return nil
}

// distance decodes a match distance less one (0xffffffff: the end marker).
func (d *lzma) distance(length uint32) uint32 {
	slot := d.rc.tree(d.posSlot[min(length, 3)][:], 6)
	if slot < 4 {
		return slot
	}
	n := uint(slot>>1) - 1
	dist := (2 | slot&1) << n
	if slot < 14 {
		return dist + d.rc.reverse(d.posSpecial[dist-slot:], n)
	}
	dist += d.rc.direct(n-4) << 4
	return dist + d.rc.reverse(d.align[:], 4)
}

// copy repeats n bytes from rep[0]+1 back.
func (d *lzma) copy(n int) error {
	dist := int64(d.rep[0]) + 1
	if dist > d.w.total || dist > int64(d.w.size) {
		return corrupt("LZMA match distance %d beyond the data", dist)
	}
	for ; n > 0; n-- {
		d.w.put(d.w.at(int(dist)))
	}
	return nil
}

// NewLZMAReader reads the legacy .lzma format: a properties byte, the
// dictionary size (32 bits) and the uncompressed size (64 bits, all ones
// when unknown; the data then ends with the end marker), then LZMA data.
func NewLZMAReader(r io.Reader, opts Options) (io.Reader, error) {
	br := bufio.NewReader(r)
	var h [13]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return nil, errors.New("xz: not an .lzma stream")
	}
	lc, lp, pb, ok := lzmaProps(h[0])
	if !ok || lc+lp > 4 {
		return nil, errors.New("xz: not an .lzma stream (or lc + lp > 4)")
	}
	dict := max(int64(binary.LittleEndian.Uint32(h[1:])), minDict)
	if max := opts.maxWindow(); dict > int64(max) {
		return nil, fmt.Errorf("xz: the dictionary size %d is above the limit %d", dict, max)
	}
	z := &alone{left: -1}
	if size := binary.LittleEndian.Uint64(h[5:]); size != 1<<64-1 {
		if size >= 1<<62 {
			return nil, errors.New("xz: not an .lzma stream")
		}
		z.left = int64(size)
	}
	z.win.reset(int(dict))
	z.lz.reset(lc, lp, pb)
	z.lz.w = &z.win
	if err := z.lz.rc.init(br); err != nil {
		return nil, err
	}
	return z, nil
}

type alone struct {
	win  window
	lz   lzma
	left int64 // -1: unknown, to the end marker
	err  error
}

func (z *alone) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for z.win.pending == 0 {
		if z.err != nil {
			return 0, z.err
		}
		z.err = z.fill(len(p))
	}
	return z.win.read(p), nil
}

func (z *alone) fill(max int) error {
	limit := min(max, z.win.size-maxMatchLen)
	for z.win.pending < limit {
		if z.left == 0 && z.lz.rc.code == 0 {
			return io.EOF
		}
		before := z.win.pending
		eos, err := z.lz.step(z.left)
		if z.lz.rc.err != nil {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		if eos {
			if z.lz.rc.code != 0 || z.left > 0 {
				return errLZMA
			}
			return io.EOF
		}
		if z.left > 0 {
			z.left -= int64(z.win.pending - before)
		}
	}
	return nil
}
