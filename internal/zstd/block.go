package zstd

import (
	"encoding/binary"
	"math/bits"
)

// A compressed block (RFC 8878, section 3.1.1.3) is a literals section and
// a sequences section: each sequence copies literals, then a match from
// the history; the literals left after the last sequence end the block.

func (z *reader) compressed(b []byte) error {
	lits, b, err := z.literals(b)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return corrupt("no sequences section")
	}
	nseq := int(b[0])
	switch {
	case nseq == 0:
		if len(b) != 1 {
			return corrupt("data after a block without sequences")
		}
		z.win.write(lits)
		return nil
	case nseq < 128:
		b = b[1:]
	case nseq < 255:
		if len(b) < 2 {
			return corrupt("truncated sequences header")
		}
		nseq, b = (nseq-128)<<8+int(b[1]), b[2:]
	default:
		if len(b) < 3 {
			return corrupt("truncated sequences header")
		}
		nseq, b = int(b[1])+int(b[2])<<8+0x7f00, b[3:]
	}
	if len(b) == 0 || b[0]&3 != 0 {
		return corrupt("sequences compression modes")
	}
	modes := b[0]
	b = b[1:]
	for k := range z.seq {
		n, err := z.table(k, modes>>(6-2*k)&3, b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return z.sequences(lits, nseq, b)
}

func (z *reader) litBuf(n int) []byte {
	if cap(z.lits) < n {
		z.lits = make([]byte, maxBlock)
	}
	return z.lits[:n]
}

// literals reads the literals section: its header gives the type (raw,
// RLE, Huffman with a new table, Huffman with the previous one), the
// regenerated size and, compressed, the compressed size and the streams.
func (z *reader) literals(b []byte) (lits, rest []byte, err error) {
	if len(b) == 0 {
		return nil, nil, corrupt("empty block")
	}
	typ, sf := b[0]&3, b[0]>>2&3
	if typ < 2 {
		hl := [4]int{1, 2, 1, 3}[sf]
		if len(b) < hl {
			return nil, nil, corrupt("truncated literals header")
		}
		size := int(le(b[:hl]) >> 4)
		if sf&1 == 0 {
			size = int(b[0] >> 3)
		}
		if size > z.blockMax {
			return nil, nil, corrupt("literals larger than the block")
		}
		b = b[hl:]
		if typ == 0 {
			if len(b) < size {
				return nil, nil, corrupt("truncated literals")
			}
			return b[:size], b[size:], nil
		}
		if len(b) == 0 {
			return nil, nil, corrupt("truncated literals")
		}
		lits = z.litBuf(size)
		for i := range lits {
			lits[i] = b[0]
		}
		return lits, b[1:], nil
	}
	hl := [4]int{3, 3, 4, 5}[sf]
	if len(b) < hl {
		return nil, nil, corrupt("truncated literals header")
	}
	n := [4]uint{10, 10, 14, 18}[sf]
	v := le(b[:hl])
	regen, comp := int(v>>4&(1<<n-1)), int(v>>(4+n)&(1<<n-1))
	if regen > z.blockMax {
		return nil, nil, corrupt("literals larger than the block")
	}
	b = b[hl:]
	if len(b) < comp {
		return nil, nil, corrupt("truncated literals")
	}
	data, rest := b[:comp], b[comp:]
	if typ == 2 {
		n, err := z.huff.read(data)
		if err != nil {
			return nil, nil, err
		}
		data = data[n:]
	} else if !z.huff.valid {
		return nil, nil, corrupt("literals use a previous Huffman table, there is none")
	}
	lits = z.litBuf(regen)
	if sf == 0 {
		err = z.huff.decode(lits, data)
	} else {
		err = z.huff.decode4(lits, data)
	}
	return lits, rest, err
}

type huffEntry struct{ sym, bits uint8 }

type huffTable struct {
	valid   bool
	maxBits uint
	e       [1 << 11]huffEntry // by the next maxBits bits
}

// read reads a Huffman tree description: the weights of the symbols but
// the last, 4 bits each or FSE-compressed; the last weight is what makes
// the sum of 2^(weight-1) a power of two, 2^maxBits.
func (h *huffTable) read(b []byte) (int, error) {
	h.valid = false
	if len(b) == 0 {
		return 0, corrupt("no Huffman tree")
	}
	var w [256]uint8
	var n, used int
	if hb := int(b[0]); hb >= 128 {
		n, used = hb-127, 1+(hb-127+1)/2
		if len(b) < used {
			return 0, corrupt("truncated Huffman tree")
		}
		for i := range n {
			c := b[1+i/2]
			if i%2 == 0 {
				c >>= 4
			}
			w[i] = c & 15
		}
	} else {
		used = 1 + hb
		if hb == 0 || len(b) < used {
			return 0, corrupt("truncated Huffman tree")
		}
		var err error
		if n, err = fseWeights(b[1:used], &w); err != nil {
			return 0, err
		}
	}
	var total uint32
	for _, x := range w[:n] {
		if x > 11 {
			return 0, corrupt("Huffman weight %d", x)
		}
		if x > 0 {
			total += 1 << (x - 1)
		}
	}
	maxBits := uint(bits.Len32(total))
	left := uint32(1)<<maxBits - total
	if total == 0 || maxBits > 11 || left&(left-1) != 0 {
		return 0, corrupt("Huffman weights")
	}
	w[n] = uint8(bits.Len32(left))
	n++
	h.maxBits = maxBits
	pos := 0
	for wt := uint8(1); uint(wt) <= maxBits; wt++ {
		e := huffEntry{bits: uint8(maxBits + 1 - uint(wt))}
		for s := range n {
			if w[s] != wt {
				continue
			}
			e.sym = uint8(s)
			for range 1 << (wt - 1) {
				h.e[pos] = e
				pos++
			}
		}
	}
	h.valid = true
	return used, nil
}

// fseWeights decodes FSE-compressed Huffman weights: two states take turns
// on one bitstream until it is used up.
func fseWeights(b []byte, w *[256]uint8) (int, error) {
	var norm [256]int16
	n, log, nsym, err := readNorm(b, 6, 255, &norm)
	if err != nil {
		return 0, err
	}
	var t fseTable
	if err := t.build(norm[:nsym], log); err != nil {
		return 0, err
	}
	var br backBits
	if err := br.init(b[n:]); err != nil {
		return 0, err
	}
	s1, s2 := int(br.read(log)), int(br.read(log))
	cnt := 0
	for {
		if cnt > 253 {
			return 0, corrupt("too many Huffman weights")
		}
		e := t.e[s1]
		w[cnt], cnt = e.sym, cnt+1
		s1 = int(e.base) + int(br.read(uint(e.bits)))
		if br.pos < 0 {
			w[cnt], cnt = t.e[s2].sym, cnt+1
			break
		}
		e = t.e[s2]
		w[cnt], cnt = e.sym, cnt+1
		s2 = int(e.base) + int(br.read(uint(e.bits)))
		if br.pos < 0 {
			w[cnt], cnt = t.e[s1].sym, cnt+1
			break
		}
	}
	if cnt > 255 {
		return 0, corrupt("too many Huffman weights")
	}
	return cnt, nil
}

// decode fills dst from one Huffman stream.
func (h *huffTable) decode(dst, src []byte) error {
	var br backBits
	if err := br.init(src); err != nil {
		return err
	}
	for i := range dst {
		e := h.e[br.peek(h.maxBits)]
		dst[i] = e.sym
		if br.pos -= int(e.bits); br.pos < 0 {
			return corrupt("Huffman stream too short")
		}
	}
	if br.pos != 0 {
		return corrupt("Huffman stream too long")
	}
	return nil
}

// decode4 fills dst from four streams, each a quarter of it (the last one
// what remains), after a table of the first three's sizes.
func (h *huffTable) decode4(dst, src []byte) error {
	if len(src) < 6 {
		return corrupt("truncated Huffman streams")
	}
	sizes := [4]int{int(binary.LittleEndian.Uint16(src)), int(binary.LittleEndian.Uint16(src[2:])), int(binary.LittleEndian.Uint16(src[4:]))}
	src = src[6:]
	sizes[3] = len(src) - sizes[0] - sizes[1] - sizes[2]
	seg := (len(dst) + 3) / 4
	if sizes[3] < 0 || 3*seg > len(dst) {
		return corrupt("Huffman stream sizes")
	}
	for i, n := range sizes {
		if err := h.decode(dst[i*seg:min((i+1)*seg, len(dst))], src[:n]); err != nil {
			return err
		}
		src = src[n:]
	}
	return nil
}

type fseEntry struct {
	sym, bits uint8
	base      uint16
}

// fseTable is an FSE decoding table: a state is an entry, whose symbol is
// decoded; the next state is base plus the next bits bits.
type fseTable struct {
	log uint
	e   []fseEntry
}

func (t *fseTable) rle(sym byte) {
	t.log, t.e = 0, append(t.e[:0], fseEntry{sym: sym})
}

// build makes the table of a distribution: norm[s] states for symbol s,
// -1 for one state at the end of the table ("less than 1").
func (t *fseTable) build(norm []int16, log uint) error {
	size := 1 << log
	sum := 0
	for _, c := range norm {
		if c < 0 {
			sum++
		} else {
			sum += int(c)
		}
	}
	if sum != size {
		return corrupt("FSE distribution")
	}
	if cap(t.e) < size {
		t.e = make([]fseEntry, size)
	}
	t.log, t.e = log, t.e[:size]
	var next [256]uint16
	high := size - 1
	for s, c := range norm {
		if c < 0 {
			t.e[high] = fseEntry{sym: uint8(s)}
			high--
			next[s] = 1
		} else {
			next[s] = uint16(c)
		}
	}
	step, mask, pos := size>>1+size>>3+3, size-1, 0
	for s, c := range norm {
		for range max(int(c), 0) {
			t.e[pos].sym = uint8(s)
			for pos = (pos + step) & mask; pos > high; pos = (pos + step) & mask {
			}
		}
	}
	if pos != 0 {
		return corrupt("FSE distribution")
	}
	for i := range t.e {
		s := t.e[i].sym
		ns := next[s]
		next[s]++
		nb := log + 1 - uint(bits.Len16(ns))
		t.e[i].bits, t.e[i].base = uint8(nb), ns<<nb-uint16(size)
	}
	return nil
}

// readNorm reads an FSE table description (RFC 8878, section 4.1.1): the
// accuracy log, then each symbol's count, in as many bits as the counts
// left need; a zero count is followed by 2-bit repeat counts of zeros.
func readNorm(b []byte, maxLog uint, maxSym int, norm *[256]int16) (n int, log uint, nsym int, err error) {
	br := fwdBits{b: b}
	log = uint(br.read(4)) + 5
	if log > maxLog {
		return 0, 0, 0, corrupt("FSE accuracy log %d above %d", log, maxLog)
	}
	remaining, threshold, nb := 1<<log+1, 1<<log, log+1
	for remaining > 1 && nsym <= maxSym {
		lim := 2*threshold - 1 - remaining
		v := int(br.peek(nb))
		if v&(threshold-1) < lim {
			v &= threshold - 1
			br.pos += int(nb) - 1
		} else {
			if v &= 2*threshold - 1; v >= threshold {
				v -= lim
			}
			br.pos += int(nb)
		}
		p := v - 1
		norm[nsym], nsym = int16(p), nsym+1
		if p < 0 {
			remaining--
		} else {
			remaining -= p
		}
		if p == 0 {
			for {
				r := int(br.read(2))
				for range r {
					if nsym > maxSym {
						return 0, 0, 0, corrupt("FSE table description")
					}
					norm[nsym], nsym = 0, nsym+1
				}
				if r != 3 {
					break
				}
			}
		}
		for remaining < threshold {
			nb--
			threshold >>= 1
		}
	}
	if remaining != 1 || br.pos > 8*len(b) {
		return 0, 0, 0, corrupt("FSE table description")
	}
	return (br.pos + 7) / 8, log, nsym, nil
}

// table sets the k-th sequences table (literals lengths, offsets, match
// lengths) for a block by its mode: predefined, RLE, FSE, repeat.
func (z *reader) table(k int, mode byte, b []byte) (int, error) {
	switch mode {
	case 0:
		z.seq[k] = &predefined[k]
		return 0, nil
	case 1:
		if len(b) == 0 || int(b[0]) > maxCode[k] {
			return 0, corrupt("RLE sequences table")
		}
		z.own[k].rle(b[0])
		z.seq[k] = &z.own[k]
		return 1, nil
	case 2:
		var norm [256]int16
		n, log, nsym, err := readNorm(b, maxLog[k], maxCode[k], &norm)
		if err != nil {
			return 0, err
		}
		if err := z.own[k].build(norm[:nsym], log); err != nil {
			return 0, err
		}
		z.seq[k] = &z.own[k]
		return n, nil
	}
	if z.seq[k] == nil {
		return 0, corrupt("sequences repeat a previous table, there is none")
	}
	return 0, nil
}

var (
	maxLog  = [3]uint{9, 8, 9}
	maxCode = [3]int{35, 31, 52}

	// the predefined distributions (RFC 8878, section 3.1.1.3.2.2)
	predefined = func() (t [3]fseTable) {
		t[0].build([]int16{4, 3, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 2, 1, 1, 1, 1, 1, -1, -1, -1, -1}, 6)
		t[1].build([]int16{1, 1, 1, 1, 1, 1, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, -1, -1, -1, -1, -1}, 5)
		t[2].build([]int16{1, 4, 3, 2, 2, 2, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
			1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, -1, -1, -1, -1, -1, -1, -1}, 6)
		return t
	}()

	llBase = [36]uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 18, 20, 22, 24, 28, 32, 40, 48, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536}
	llBits = [36]uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 3, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	mlBase = [53]uint32{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34,
		35, 37, 39, 41, 43, 47, 51, 59, 67, 83, 99, 131, 259, 515, 1027, 2051, 4099, 8195, 16387, 32771, 65539}
	mlBits = [53]uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		1, 1, 1, 1, 2, 2, 3, 3, 4, 4, 5, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
)

// sequences decodes and executes the block's sequences: from a backward
// bitstream, the three initial states, then for each sequence the offset,
// match length and literals length extra bits and the state updates.
func (z *reader) sequences(lits []byte, nseq int, b []byte) error {
	ll, of, ml := z.seq[0], z.seq[1], z.seq[2]
	var br backBits
	if err := br.init(b); err != nil {
		return err
	}
	sl, so, sm := int(br.read(ll.log)), int(br.read(of.log)), int(br.read(ml.log))
	out := 0
	for i := range nseq {
		lc, oc, mc := ll.e[sl].sym, of.e[so].sym, ml.e[sm].sym
		if oc > 31 {
			return corrupt("offset code %d", oc)
		}
		offVal := 1<<oc + int(br.read(uint(oc)))
		mlen := int(mlBase[mc]) + int(br.read(uint(mlBits[mc])))
		llen := int(llBase[lc]) + int(br.read(uint(llBits[lc])))
		if i < nseq-1 {
			e := ll.e[sl]
			sl = int(e.base) + int(br.read(uint(e.bits)))
			e = ml.e[sm]
			sm = int(e.base) + int(br.read(uint(e.bits)))
			e = of.e[so]
			so = int(e.base) + int(br.read(uint(e.bits)))
		}
		if br.pos < 0 {
			return corrupt("sequences bitstream too short")
		}
		var off int
		if offVal > 3 {
			off = offVal - 3
			z.rep = [3]int{off, z.rep[0], z.rep[1]}
		} else {
			idx := offVal - 1
			if llen == 0 {
				idx++
			}
			switch idx {
			case 0:
				off = z.rep[0]
			case 1:
				off = z.rep[1]
				z.rep[0], z.rep[1] = off, z.rep[0]
			case 2:
				off = z.rep[2]
				z.rep = [3]int{off, z.rep[0], z.rep[1]}
			default:
				off = z.rep[0] - 1
				if off == 0 {
					return corrupt("repeat offset 0")
				}
				z.rep = [3]int{off, z.rep[0], z.rep[1]}
			}
		}
		if llen > len(lits) {
			return corrupt("sequence past the literals")
		}
		if out += llen + mlen; out > z.blockMax {
			return corrupt("block larger than %d bytes", z.blockMax)
		}
		z.win.write(lits[:llen])
		lits = lits[llen:]
		if int64(off) > z.win.total || off > z.win.size {
			return corrupt("match offset %d beyond the history", off)
		}
		z.win.match(off, mlen)
	}
	if br.pos != 0 {
		return corrupt("sequences bitstream too long")
	}
	if out += len(lits); out > z.blockMax {
		return corrupt("block larger than %d bytes", z.blockMax)
	}
	z.win.write(lits)
	return nil
}

// backBits reads a bitstream backward: from the bit below the highest set
// bit of the last byte (the end mark) down to bit 0 of the first byte,
// each read taking the next bits as a number, high bit first. Bits below
// the start read as 0; pos then goes negative.
type backBits struct {
	b   []byte
	pos int // bits left
}

func (r *backBits) init(b []byte) error {
	if len(b) == 0 || b[len(b)-1] == 0 {
		return corrupt("bitstream without an end mark")
	}
	r.b, r.pos = b, 8*len(b)-9+bits.Len8(b[len(b)-1])
	return nil
}

// peek returns the next n (at most 56) bits.
func (r *backBits) peek(n uint) uint64 {
	lo := r.pos - int(n)
	var v uint64
	if lo >= 0 {
		v = r.load(lo>>3) >> (lo & 7)
	} else {
		v = r.load(0) << uint(-lo)
	}
	return v & (1<<n - 1)
}

func (r *backBits) read(n uint) uint64 {
	v := r.peek(n)
	r.pos -= int(n)
	return v
}

// load is the 8 bytes from i, little-endian, zero past the end.
func (r *backBits) load(i int) uint64 {
	if i+8 <= len(r.b) {
		return binary.LittleEndian.Uint64(r.b[i:])
	}
	var v uint64
	for j := len(r.b) - 1; j >= i; j-- {
		v = v<<8 | uint64(r.b[j])
	}
	return v
}

// fwdBits reads a bitstream forward, low bit first; zero past the end.
type fwdBits struct {
	b   []byte
	pos int
}

func (r *fwdBits) peek(n uint) uint32 {
	i := r.pos >> 3
	var v uint32
	for j := 3; j >= 0; j-- {
		v <<= 8
		if i+j < len(r.b) {
			v |= uint32(r.b[i+j])
		}
	}
	return v >> (r.pos & 7) & (1<<n - 1)
}

func (r *fwdBits) read(n uint) uint32 {
	v := r.peek(n)
	r.pos += int(n)
	return v
}
