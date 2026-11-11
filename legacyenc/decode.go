package legacyenc

import (
	"bytes"
	"fmt"
	"hash/crc32"
)

// issues records Issues, each code once unless added with add.
type issues struct {
	list *[]Issue
	seen uint32
}

func (n *issues) has(c Code) bool { return n.seen&(1<<c) != 0 }

func (n *issues) once(c Code, off int, detail string) {
	if !n.has(c) {
		n.add(c, off, detail)
	}
}

func (n *issues) add(c Code, off int, detail string) {
	n.seen |= 1 << c
	*n.list = append(*n.list, Issue{c, off, detail})
}

// An alphabet maps a character to its 6-bit value, or -1.
type alphabet [256]int8

// length returns the line length a uuencode or xxencode length character
// says, or -1.
func (a *alphabet) length(c byte) int {
	if n := int(a[c]); n <= 45 {
		return n
	}
	return -1
}

func newAlphabet(chars string) alphabet {
	var a alphabet
	for i := range a {
		a[i] = -1
	}
	for i := 0; i < len(chars); i++ {
		a[chars[i]] = int8(i)
	}
	return a
}

const (
	xxChars  = "+-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	hqxChars = "!\"#$%&'()*+,-012345689@ABCDEFGHIJKLMNPQRSTUVXYZ[`abcdefhijklmpqr"
)

var (
	uuAlphabet  = uuTable()
	xxAlphabet  = newAlphabet(xxChars)
	b64Alphabet = newAlphabet(b64Chars)
	hqxAlphabet = newAlphabet(hqxChars)
)

// uuTable maps ' ' through '`' to their values; '`' and ' ' are both zero.
func uuTable() alphabet {
	var a alphabet
	for i := range a {
		a[i] = -1
	}
	for c := 0x20; c <= 0x60; c++ {
		a[c] = int8((c - 0x20) & 63)
	}
	return a
}

// A decoder reads the encoded lines of one block.
type decoder struct {
	body   []byte
	quote  []byte
	pos    int // next line
	end    int // end of the block's encoded data
	maxLn  int
	budget int64
	res    *Result
	iss    issues
}

// next returns the next line of the block, after its quote prefix and cut at
// MaxLineBytes, and the line's offset in the body.
func (d *decoder) next() (c []byte, off int, ok bool) {
	if d.pos >= d.end {
		return nil, 0, false
	}
	b := d.body[:d.end]
	start := d.pos
	end, next := lineAt(b, start)
	d.pos = next
	line := b[start:end]
	if u := unquote(line, d.quote); u > 0 {
		line, start = line[u:], start+u
	}
	if len(line) > d.maxLn {
		line = line[:d.maxLn]
	}
	return line, start, true
}

// hint returns a starting capacity for a block's output: what it declares,
// bounded by the budget and by the input left, which bounds the output of
// every encoding but BinHex's run-length coding.
func (d *decoder) hint(declared int64) int {
	n := min(int64(d.end-d.pos), d.budget)
	if declared >= 0 {
		n = min(n, declared)
	}
	return int(max(n, 0))
}

func (d *decoder) overBudget(off int) {
	d.iss.once(CodeBudget, off, fmt.Sprintf("decodes to more than %d bytes; decoding stopped there", d.budget))
	d.res.Truncated = true
}

// uu decodes a uuencoded or xxencoded block.
func (d *decoder) uu(a *alphabet) {
	data := make([]byte, 0, d.hint(-1))
	defer func() { d.res.Data = data }()
	for {
		c, off, ok := d.next()
		if !ok {
			break
		}
		if t := bytes.TrimRight(c, " \t"); len(t) == 0 || string(t) == "end" {
			continue
		}
		n := a.length(c[0])
		if n < 0 {
			d.iss.once(CodeInvalidLine, off, "a line with no valid length character was skipped")
			continue
		}
		need := (n + 2) / 3 * 4
		chars := c[1:]
		if len(chars) < need {
			d.iss.once(CodeShortLine, off, "a line is shorter than its length character says; decoded as if padded")
		} else if len(bytes.TrimRight(chars[need:], " \t")) > 0 {
			d.iss.once(CodeLineJunk, off+1+need, "a line holds more than its length character says; the rest was ignored")
		}
		for g := 0; g < n; g += 3 {
			var v [4]byte
			for k := range v {
				j := g/3*4 + k
				if j >= len(chars) {
					break
				}
				x := a[chars[j]]
				if x < 0 {
					d.iss.once(CodeInvalidChar, off+1+j, "a character outside the alphabet was read as zero")
					x = 0
				}
				v[k] = byte(x)
			}
			t := [3]byte{v[0]<<2 | v[1]>>4, v[1]<<4 | v[2]>>2, v[2]<<6 | v[3]}
			for k := 0; k < 3 && g+k < n; k++ {
				if int64(len(data)) >= d.budget {
					d.overBudget(off)
					return
				}
				data = append(data, t[k])
			}
		}
	}
	if !d.res.Block.Complete {
		d.res.Truncated = true
	}
}

// base64 decodes a begin-base64 block.
func (d *decoder) base64() {
	data := make([]byte, 0, d.hint(-1))
	defer func() { d.res.Data = data }()
	var q [4]byte
	nq, qoff := 0, -1
	// flush emits the whole bytes of a group of nq characters.
	flush := func(off int) bool {
		t := [3]byte{q[0]<<2 | q[1]>>4, q[1]<<4 | q[2]>>2, q[2]<<6 | q[3]}
		if nq == 1 {
			d.iss.once(CodeTruncated, off, "a Base64 group of one character holds no byte; dropped")
		}
		for k := 0; k < nq-1; k++ {
			if int64(len(data)) >= d.budget {
				d.overBudget(off)
				return false
			}
			data = append(data, t[k])
		}
		nq, q = 0, [4]byte{}
		return true
	}
	for {
		c, off, ok := d.next()
		if !ok {
			break
		}
		if string(bytes.TrimRight(c, " \t")) == "====" {
			break
		}
		for i, ch := range c {
			v := b64Alphabet[ch]
			switch {
			case v >= 0:
				if nq == 0 {
					qoff = off + i
				}
				q[nq] = byte(v)
				nq++
				if nq == 4 && !flush(qoff) {
					return
				}
			case ch == '=':
				if nq > 0 && !flush(qoff) {
					return
				}
			case ch == ' ' || ch == '\t':
			default:
				d.iss.once(CodeInvalidChar, off+i, "a character outside the Base64 alphabet was skipped")
			}
		}
	}
	if nq > 0 && !flush(qoff) {
		return
	}
	if !d.res.Block.Complete {
		d.res.Truncated = true
	}
}

// yenc decodes a yEnc block and checks it against its sizes and CRCs.
func (d *decoder) yenc() {
	b := &d.res.Block
	data := make([]byte, 0, d.hint(b.Size))
	esc, escOff := false, 0
	full := false
lines:
	for {
		c, off, ok := d.next()
		if !ok {
			break
		}
		for i := 0; i < len(c); i++ {
			ch := c[i]
			if esc {
				esc, ch = false, ch-64
			} else if ch == '=' {
				esc, escOff = true, off+i
				continue
			}
			if int64(len(data)) >= d.budget {
				full = true
				d.overBudget(off + i)
				break lines
			}
			data = append(data, ch-42)
		}
	}
	d.res.Data = data
	if esc {
		d.iss.once(CodeDanglingEscape, escOff, "the data ends with an escape character; it was dropped")
	}
	if full {
		return
	}
	n := int64(len(data))
	if h := b.YEnc; h != nil {
		whole := h.Part == 0 || h.Total == 1
		if h.EndSize >= 0 && h.EndSize != n {
			d.iss.add(CodeSizeMismatch, b.dataEnd, fmt.Sprintf("=yend size=%d, decoded %d", h.EndSize, n))
		}
		if h.PartBegin > 0 && h.PartEnd >= h.PartBegin && h.PartEnd-h.PartBegin+1 != n {
			d.iss.add(CodeSizeMismatch, b.dataStart, fmt.Sprintf("=ypart range holds %d bytes, decoded %d", h.PartEnd-h.PartBegin+1, n))
		}
		if whole && h.Size >= 0 && h.Size != n {
			d.iss.add(CodeSizeMismatch, b.Start, fmt.Sprintf("=ybegin size=%d, decoded %d", h.Size, n))
		}
		sum := crc32.ChecksumIEEE(data)
		if h.HasPartCRC32 && h.PartCRC32 != sum {
			d.iss.add(CodeCRCMismatch, b.dataEnd, fmt.Sprintf("pcrc32=%08x, computed %08x", h.PartCRC32, sum))
		}
		if whole && h.HasCRC32 && h.CRC32 != sum {
			d.iss.add(CodeCRCMismatch, b.dataEnd, fmt.Sprintf("crc32=%08x, computed %08x", h.CRC32, sum))
		}
	}
	if !b.Complete || b.Size >= 0 && n < b.Size {
		d.res.Truncated = true
	}
}
