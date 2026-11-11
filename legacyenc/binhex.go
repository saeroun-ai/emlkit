package legacyenc

import (
	"encoding/binary"
	"fmt"
)

// BinHex 4.0 (RFC 1741): the characters between two colons carry six bits
// each; the bytes they make are run-length coded (0x90 n repeats the byte
// before it to n in all; 0x90 0 is a literal 0x90); the expanded stream is a
// header, the data fork, its CRC, the resource fork and its CRC. The CRCs are
// CRC-16-CCITT as XMODEM computes it (polynomial 0x1021, initial 0).
//
// The run-length stage expands three bytes into up to 255, so an output
// budget, not the input size, is what bounds a BinHex block.

var crc16Table = func() (t [256]uint16) {
	for i := range t {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x1021
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

func crc16(crc uint16, b byte) uint16 { return crc<<8 ^ crc16Table[byte(crc>>8)^b] }

const (
	phHeader = iota
	phData
	phDataCRC
	phRsrc
	phRsrcCRC
	phDone
)

var phaseNames = [...]string{"header", "data fork", "data fork CRC", "resource fork", "resource fork CRC", "end"}

// hqxHeaderMax is the longest header: a name length byte, up to 255 name
// bytes, version, type, creator, flags, two fork lengths and the CRC.
const hqxHeaderMax = 1 + 255 + 1 + 4 + 4 + 2 + 4 + 4 + 2

type hqx struct {
	d          *decoder
	headerOnly bool

	acc  uint32 // six-bit stage
	bits uint

	marker   bool // run-length stage: a 0x90 was read
	havePrev bool
	prev     byte

	phase      int
	hdr        [hqxHeaderMax]byte
	nhdr, need int
	left       int64 // bytes left in the current fork
	crc        uint16
	crcBuf     [2]byte
	ncrc       int
	data, rsrc []byte
	used       int64
	off        int // offset of the character being read
	full, stop bool
	h          BinHexHeader
	hasHeader  bool
}

// run decodes the block's characters until the closing colon, the end of
// the block, or a stop.
func (x *hqx) run() {
	d := x.d
	opened := false
	for !x.stop {
		c, off, ok := d.next()
		if !ok {
			return
		}
		for i := 0; i < len(c) && !x.stop; i++ {
			ch := c[i]
			if !opened {
				opened = ch == ':'
				continue
			}
			if ch == ':' {
				return
			}
			v := hqxAlphabet[ch]
			if v < 0 {
				if ch != ' ' && ch != '\t' {
					d.iss.once(CodeInvalidChar, off+i, "a character outside the BinHex alphabet was skipped")
				}
				continue
			}
			x.off = off + i
			x.acc = x.acc<<6 | uint32(v)
			x.bits += 6
			if x.bits >= 8 {
				x.bits -= 8
				x.rle(byte(x.acc >> x.bits))
				x.acc &= 1<<x.bits - 1
			}
		}
	}
}

// rle expands the run-length coding.
func (x *hqx) rle(b byte) {
	switch {
	case x.marker:
		x.marker = false
		switch {
		case b == 0:
			x.emit(0x90, 1)
			x.prev, x.havePrev = 0x90, true
		case !x.havePrev:
			x.d.iss.once(CodeBadRun, x.off, "a run-length marker with no byte before it was ignored")
		default:
			x.emit(x.prev, int(b)-1)
		}
	case b == 0x90:
		x.marker = true
	default:
		x.emit(b, 1)
		x.prev, x.havePrev = b, true
	}
}

// emit routes count copies of b to the header, the forks and their CRCs.
func (x *hqx) emit(b byte, count int) {
	for count > 0 && !x.stop {
		switch x.phase {
		case phHeader:
			x.hdr[x.nhdr] = b
			x.nhdr++
			count--
			if x.nhdr == 1 {
				x.need = hqxHeaderMax - 255 + int(b)
				if b == 0 || b > 63 {
					x.d.iss.once(CodeBadHeader, x.off, fmt.Sprintf("the name length is %d, not 1 to 63", b))
				}
			}
			if x.nhdr == x.need {
				x.header()
			}
		case phData, phRsrc:
			room := x.d.budget - x.used
			if room <= 0 {
				x.full, x.stop = true, true
				x.d.overBudget(x.off)
				return
			}
			k := min(int64(count), x.left, room)
			buf := &x.data
			if x.phase == phRsrc {
				buf = &x.rsrc
			}
			for range k {
				*buf = append(*buf, b)
				x.crc = crc16(x.crc, b)
			}
			x.used += k
			x.left -= k
			count -= int(k)
			if x.left == 0 {
				x.phase++
			}
		case phDataCRC, phRsrcCRC:
			x.crcBuf[x.ncrc] = b
			x.ncrc++
			count--
			if x.ncrc == 2 {
				x.checkFork()
			}
		default:
			x.d.iss.once(CodeTrailingData, x.off, "data after the resource fork's CRC was ignored")
			x.stop = true
			return
		}
	}
}

// header reads the complete header and moves on to the data fork.
func (x *hqx) header() {
	h := &x.h
	p := x.hdr[:x.nhdr]
	n := int(p[0])
	h.Name = string(p[1 : 1+n])
	p = p[1+n:]
	h.Version = p[0]
	copy(h.Type[:], p[1:5])
	copy(h.Creator[:], p[5:9])
	h.Flags = binary.BigEndian.Uint16(p[9:11])
	h.DataLength = binary.BigEndian.Uint32(p[11:15])
	h.ResourceLength = binary.BigEndian.Uint32(p[15:19])
	h.CRC = binary.BigEndian.Uint16(p[19:21])
	var sum uint16
	for _, b := range x.hdr[:x.nhdr-2] {
		sum = crc16(sum, b)
	}
	h.CRCValid = sum == h.CRC
	x.hasHeader = true
	if x.headerOnly {
		x.stop = true
		return
	}
	if !h.CRCValid {
		x.d.iss.add(CodeCRCMismatch, x.off, fmt.Sprintf("header CRC %04x, computed %04x", h.CRC, sum))
	}
	x.startFork(phData, int64(h.DataLength))
}

// startFork enters a fork of n declared bytes. Its buffer starts no larger
// than the budget left and the characters left allow without run-length
// expansion; a fork that expands grows within the budget.
func (x *hqx) startFork(phase int, n int64) {
	x.phase, x.left, x.crc, x.ncrc = phase, n, 0, 0
	if n == 0 {
		x.phase++
		return
	}
	c := int(max(0, min(n, x.d.budget-x.used, int64(x.d.end-x.off))))
	if phase == phData {
		x.data = make([]byte, 0, c)
	} else {
		x.rsrc = make([]byte, 0, c)
	}
}

// checkFork compares the CRC just read with the one computed.
func (x *hqx) checkFork() {
	want := binary.BigEndian.Uint16(x.crcBuf[:])
	if want != x.crc {
		x.d.iss.add(CodeCRCMismatch, x.off, fmt.Sprintf("%s CRC %04x, computed %04x", phaseNames[x.phase-1], want, x.crc))
	}
	if x.phase == phDataCRC {
		x.startFork(phRsrc, int64(x.h.ResourceLength))
		return
	}
	x.phase = phDone
}

func (d *decoder) binhex() {
	x := &hqx{d: d}
	x.run()
	r := d.res
	if x.hasHeader {
		h := x.h
		r.Block.BinHex, r.Block.Name, r.Block.Size = &h, h.Name, int64(h.DataLength)
	}
	r.Data, r.Resource = x.data, x.rsrc
	if x.full {
		return
	}
	if x.phase != phDone {
		d.iss.once(CodeTruncated, d.end, fmt.Sprintf("the data ends in the %s", phaseNames[x.phase]))
		r.Truncated = true
	}
}

// peekBinHex decodes only the header of a BinHex block.
func peekBinHex(body []byte, b *Block, l Limits) (BinHexHeader, bool) {
	var r Result
	d := &decoder{body: body, quote: []byte(b.Quote), pos: b.dataStart, end: b.dataEnd, maxLn: l.MaxLineBytes, res: &r}
	d.iss.list = &r.Issues
	x := &hqx{d: d, headerOnly: true}
	x.run()
	return x.h, x.hasHeader
}
