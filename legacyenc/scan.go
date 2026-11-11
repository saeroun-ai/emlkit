package legacyenc

import (
	"bytes"
	"fmt"
)

// A scanner finds blocks in b, one line at a time. Every line is looked at a
// bounded number of times: once as a possible opening line, and once more
// when it is the line that ended a block without a closing line.
type scanner struct {
	b     []byte
	lim   Limits
	sniff bool // stop at an opening line once it is confirmed
}

// line returns the end of the line that starts at pos, before its line
// ending, and the start of the next line. A line ends in CRLF, LF or CR.
func (s *scanner) line(pos int) (end, next int) {
	return lineAt(s.b, pos)
}

func lineAt(b []byte, pos int) (end, next int) {
	i := bytes.IndexAny(b[pos:], "\r\n")
	if i < 0 {
		return len(b), len(b)
	}
	end = pos + i
	if b[end] == '\r' && end+1 < len(b) && b[end+1] == '\n' {
		return end, end + 2
	}
	return end, end + 1
}

// A bline is one line of a block.
type bline struct {
	pos, next int
	c         []byte // the line after the block's quote, cut at MaxLineBytes
	coff      int    // the offset of c in the body
	long      bool   // the line was cut
	ok        bool   // the line carries the block's quote
}

func (s *scanner) bline(pos int, quote []byte) bline {
	end, next := s.line(pos)
	l := bline{pos: pos, next: next}
	raw := s.b[pos:end]
	u := unquote(raw, quote)
	if u < 0 {
		return l
	}
	l.ok, l.coff, l.c = true, pos+u, raw[u:]
	if len(l.c) > s.lim.MaxLineBytes {
		l.c, l.long = l.c[:s.lim.MaxLineBytes], true
	}
	return l
}

// quoteLen returns the length of the quoting prefix of line: one or more '>',
// each after at most three spaces or tabs, and one space after the last.
func quoteLen(line []byte) int {
	i, n := 0, 0
	for i < len(line) {
		j := i
		for j < len(line) && j-i < 3 && (line[j] == ' ' || line[j] == '\t') {
			j++
		}
		if j < len(line) && line[j] == '>' {
			i, n = j+1, n+1
			continue
		}
		break
	}
	if n == 0 {
		return 0
	}
	if i < len(line) && line[i] == ' ' {
		i++
	}
	return i
}

// unquote returns how much of line is the quote prefix q, or -1 when line
// does not carry it. A line may carry q without its trailing white space, as
// an empty quoted line often does.
func unquote(line, q []byte) int {
	if len(q) == 0 {
		return 0
	}
	if bytes.HasPrefix(line, q) {
		return len(q)
	}
	if t := bytes.TrimRight(q, " \t"); len(t) < len(q) && bytes.HasPrefix(line, t) {
		return len(t)
	}
	return -1
}

func scan(body []byte, l Limits) ([]Block, []Issue) {
	var issues []Issue
	b := body
	if len(b) > l.MaxScanBytes {
		b = b[:l.MaxScanBytes]
		issues = append(issues, Issue{CodeScanLimit, l.MaxScanBytes, fmt.Sprintf("the body is %d bytes; the first %d were scanned", len(body), l.MaxScanBytes)})
	}
	s := &scanner{b: b, lim: l}
	var blocks []Block
	for pos := 0; pos < len(b); {
		end, next := s.line(pos)
		blk, after, ok := s.block(pos, end, next)
		if !ok {
			pos = next
			continue
		}
		if len(blocks) == l.MaxBlocks {
			issues = append(issues, Issue{CodeBlockLimit, blk.Start, fmt.Sprintf("more than %d blocks; the rest were not reported", l.MaxBlocks)})
			break
		}
		blocks = append(blocks, blk)
		pos = after
	}
	return blocks, issues
}

// block reads the block the line [pos, end) opens, if it opens one. It returns
// the block and where scanning goes on, which is past the line at pos.
func (s *scanner) block(pos, end, next int) (Block, int, bool) {
	line := s.b[pos:end]
	if len(line) > s.lim.MaxLineBytes {
		line = line[:s.lim.MaxLineBytes]
	}
	q := quoteLen(line)
	quote, rest := line[:q], line[q:]
	switch {
	case bytes.HasPrefix(rest, []byte("=ybegin ")):
		return s.yenc(pos, next, quote, rest[len("=ybegin "):])
	case isBanner(rest):
		return s.binhex(pos, next, quote)
	case bytes.HasPrefix(rest, []byte("begin")):
		return s.begin(pos, next, quote, rest)
	}
	return Block{}, 0, false
}

// first returns the first line from pos that is not blank, skipping at most
// skip blank lines; ok is false when there is none or a line lacks the quote.
func (s *scanner) first(pos int, quote []byte, skip int) (bline, bool) {
	for i := 0; i <= skip && pos < len(s.b); i++ {
		l := s.bline(pos, quote)
		if !l.ok {
			return l, false
		}
		if len(bytes.TrimSpace(l.c)) > 0 {
			return l, true
		}
		pos = l.next
	}
	return bline{}, false
}

// -- uuencode, xxencode, begin-base64 --

// parseBegin reads "begin MODE [NAME]" or "begin-base64 MODE [NAME]", MODE
// being one to six octal digits.
func parseBegin(rest []byte) (k Kind, mode int, name string, ok bool) {
	var r []byte
	switch {
	case bytes.HasPrefix(rest, []byte("begin-base64 ")):
		k, r = KindUUBase64, rest[len("begin-base64 "):]
	case bytes.HasPrefix(rest, []byte("begin ")):
		k, r = KindUUEncode, rest[len("begin "):]
	default:
		return 0, 0, "", false
	}
	r = bytes.TrimLeft(r, " \t")
	i := 0
	for i < len(r) && i < 6 && r[i] >= '0' && r[i] <= '7' {
		mode = mode*8 + int(r[i]-'0')
		i++
	}
	if i == 0 || i < len(r) && r[i] != ' ' && r[i] != '\t' {
		return 0, 0, "", false
	}
	return k, mode, string(bytes.TrimSpace(r[i:])), true
}

func (s *scanner) begin(pos, next int, quote, rest []byte) (Block, int, bool) {
	kind, mode, name, ok := parseBegin(rest)
	if !ok {
		return Block{}, 0, false
	}
	f, ok := s.first(next, quote, 2)
	if !ok {
		return Block{}, 0, false
	}
	var a *alphabet
	switch {
	case kind == KindUUBase64:
		if !b64Line(f.c, true) {
			return Block{}, 0, false
		}
	case wholeLine(f.c, &uuAlphabet):
		kind, a = KindUUEncode, &uuAlphabet
	case wholeLine(f.c, &xxAlphabet):
		kind, a = KindXXEncode, &xxAlphabet
	default:
		return Block{}, 0, false
	}
	blk := Block{Kind: kind, Start: pos, Quote: string(quote), Name: name, Mode: mode, Size: -1, dataStart: next}
	if s.sniff {
		return blk, next, true
	}
	iss := issues{list: &blk.Issues}
	closing := "end"
	if kind == KindUUBase64 {
		closing = "===="
	}
	width := len(bytes.TrimRight(f.c, " \t"))
	// Without a closing line the block ends at its last whole line: a short
	// line after it is more likely prose than data.
	lastWhole, after := next, len(s.b)
	for p := next; p < len(s.b); {
		l := s.bline(p, quote)
		if !l.ok {
			after = p
			break
		}
		if l.long {
			iss.once(CodeLineTooLong, l.coff, fmt.Sprintf("a line is longer than %d bytes; the rest of it was not read", s.lim.MaxLineBytes))
		}
		t := bytes.TrimRight(l.c, " \t")
		if string(t) == closing {
			blk.Complete, blk.dataEnd, blk.End = true, p, l.next
			return blk, l.next, true
		}
		if len(t) == 0 {
			p = l.next
			continue
		}
		var data, whole bool
		if a == nil {
			data, whole = b64Data(l.c, width)
		} else {
			data, whole = dataLine(l.c, a), wholeLine(l.c, a)
		}
		if !data {
			after = p
			break
		}
		if whole {
			lastWhole = l.next
		}
		p = l.next
	}
	blk.dataEnd, blk.End = lastWhole, lastWhole
	iss.once(CodeMissingEnd, lastWhole, fmt.Sprintf("no %q line; the block ends at its last whole encoded line", closing))
	return blk, max(after, lastWhole), true
}

// dataLine reports whether c may be a line of a uuencoded or xxencoded block:
// a valid length character, and either nothing outside the alphabet but
// trailing white space, or the length a whole line has (a character damaged in
// transit does not end the block).
func dataLine(c []byte, a *alphabet) bool {
	if len(c) == 0 {
		return false
	}
	n := a.length(c[0])
	if n < 0 {
		return false
	}
	if allIn(bytes.TrimRight(c, " \t"), a) {
		return true
	}
	need := (n + 2) / 3 * 4
	return len(c)-1 >= need && len(c)-1 <= need+2
}

func allIn(b []byte, a *alphabet) bool {
	for _, ch := range b {
		if a[ch] < 0 {
			return false
		}
	}
	return true
}

// wholeLine reports whether c is a whole uuencoded or xxencoded line: a
// length character, the characters it needs, all in the alphabet, and at most
// two more (a checksum character, white space).
func wholeLine(c []byte, a *alphabet) bool {
	if len(c) == 0 {
		return false
	}
	n := a.length(c[0])
	if n < 0 {
		return false
	}
	need := (n + 2) / 3 * 4
	if len(c)-1 < need || len(c)-1 > need+2 {
		return false
	}
	return allIn(c[1:1+need], a)
}

// b64Line reports whether c, without trailing white space, is all Base64
// characters; whole, that it is also a non-empty multiple of four.
func b64Line(c []byte, whole bool) bool {
	t := bytes.TrimRight(c, " \t")
	if len(t) == 0 {
		return false
	}
	for _, ch := range t {
		if b64Alphabet[ch] < 0 && ch != '=' {
			return false
		}
	}
	return !whole || len(t)%4 == 0
}

// b64Data reports whether c may be a line of a begin-base64 block: all Base64
// characters, or as wide as the block's first line; whole, that its length is
// also a multiple of four.
func b64Data(c []byte, width int) (data, whole bool) {
	n := len(bytes.TrimRight(c, " \t"))
	data = b64Line(c, false) || n == width
	return data, data && n%4 == 0
}

// -- yEnc --

// yfields calls fn for each key=value field of a yEnc keyword line. The value
// of "name" is the rest of the line.
func yfields(b []byte, fn func(k string, v []byte)) {
	for {
		b = bytes.TrimLeft(b, " \t")
		if len(b) == 0 {
			return
		}
		if bytes.HasPrefix(b, []byte("name=")) {
			fn("name", bytes.TrimRight(b[len("name="):], " \t"))
			return
		}
		tok := b
		if i := bytes.IndexAny(b, " \t"); i >= 0 {
			tok, b = b[:i], b[i:]
		} else {
			b = nil
		}
		if k, v, ok := bytes.Cut(tok, []byte("=")); ok {
			fn(string(k), v)
		}
	}
}

// decimal parses a non-negative decimal of at most 18 digits.
func decimal(v []byte) (int64, bool) {
	if len(v) == 0 || len(v) > 18 {
		return 0, false
	}
	var n int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// hex32 parses one to eight hexadecimal digits.
func hex32(v []byte) (uint32, bool) {
	if len(v) == 0 || len(v) > 8 {
		return 0, false
	}
	var n uint32
	for _, c := range v {
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		n = n<<4 | uint32(d)
	}
	return n, true
}

// keyword reports whether c is the yEnc keyword line kw ("=yend", "=ypart")
// and returns its fields.
func keyword(c []byte, kw string) ([]byte, bool) {
	if !bytes.HasPrefix(c, []byte(kw)) {
		return nil, false
	}
	r := c[len(kw):]
	if len(r) > 0 && r[0] != ' ' && r[0] != '\t' {
		return nil, false
	}
	return r, true
}

func (s *scanner) yenc(pos, next int, quote, fields []byte) (Block, int, bool) {
	h := &YEncHeader{Size: -1, EndSize: -1}
	blk := Block{Kind: KindYEnc, Start: pos, Quote: string(quote), Mode: -1, Size: -1, YEnc: h, dataStart: next}
	iss := issues{list: &blk.Issues}
	bad := func(where string, k string) {
		iss.once(CodeBadHeader, pos, fmt.Sprintf("%s field %q does not parse; ignored", where, k))
	}
	named, sized := false, false
	yfields(fields, func(k string, v []byte) {
		var n int64
		var ok bool
		switch k {
		case "name":
			blk.Name, named = string(v), true
			return
		case "line", "size", "part", "total":
			n, ok = decimal(v)
		default:
			return
		}
		if !ok {
			bad("=ybegin", k)
			return
		}
		switch k {
		case "line":
			h.Line = n
		case "size":
			h.Size, sized = n, true
		case "part":
			h.Part = n
		case "total":
			h.Total = n
		}
	})
	if !named && !sized {
		// "=ybegin" in prose; a real header always has both.
		return Block{}, 0, false
	}
	if s.sniff {
		return blk, next, true
	}
	if f, ok := s.first(next, quote, 0); ok {
		if r, ok := keyword(f.c, "=ypart"); ok {
			blk.dataStart = f.next
			yfields(r, func(k string, v []byte) {
				if k != "begin" && k != "end" {
					return
				}
				n, ok := decimal(v)
				switch {
				case !ok:
					bad("=ypart", k)
				case k == "begin":
					h.PartBegin = n
				default:
					h.PartEnd = n
				}
			})
		}
	}
	after := len(s.b)
	for p := blk.dataStart; p < len(s.b); {
		l := s.bline(p, quote)
		if !l.ok {
			after = p
			break
		}
		if l.long {
			iss.once(CodeLineTooLong, l.coff, fmt.Sprintf("a line is longer than %d bytes; the rest of it was not read", s.lim.MaxLineBytes))
		}
		if r, ok := keyword(l.c, "=yend"); ok {
			blk.Complete, blk.dataEnd, blk.End, after = true, p, l.next, l.next
			yfields(r, func(k string, v []byte) {
				switch k {
				case "size", "part":
					n, ok := decimal(v)
					switch {
					case !ok:
						bad("=yend", k)
					case k == "size":
						h.EndSize = n
					default:
						h.EndPart = n
					}
				case "crc32", "pcrc32":
					n, ok := hex32(v)
					switch {
					case !ok:
						bad("=yend", k)
					case k == "crc32":
						h.CRC32, h.HasCRC32 = n, true
					default:
						h.PartCRC32, h.HasPartCRC32 = n, true
					}
				}
			})
			break
		}
		if bytes.HasPrefix(l.c, []byte("=ybegin ")) {
			after = p
			break
		}
		p = l.next
	}
	if !blk.Complete {
		blk.dataEnd, blk.End = after, after
		iss.once(CodeMissingEnd, after, "no \"=yend\" line; the block runs to where its lines stop")
	}
	switch {
	case h.EndSize >= 0:
		blk.Size = h.EndSize
	case h.PartBegin > 0 && h.PartEnd >= h.PartBegin:
		blk.Size = h.PartEnd - h.PartBegin + 1
	case h.Part == 0:
		blk.Size = h.Size
	}
	switch {
	case h.Part > 0 && h.Total > 0 && h.Part > h.Total:
		iss.once(CodePartRange, pos, fmt.Sprintf("part %d of %d", h.Part, h.Total))
	case h.PartBegin != 0 || h.PartEnd != 0:
		if h.PartBegin < 1 || h.PartEnd < h.PartBegin || h.Size >= 0 && h.PartEnd > h.Size {
			iss.once(CodePartRange, pos, fmt.Sprintf("=ypart begin=%d end=%d of a %d-byte file", h.PartBegin, h.PartEnd, h.Size))
		}
	case h.Part > 0 && h.Total != 1:
		iss.once(CodePartRange, pos, "a multi-part block without \"=ypart\"")
	}
	if h.EndPart > 0 && h.Part > 0 && h.EndPart != h.Part {
		iss.once(CodePartRange, pos, fmt.Sprintf("=ybegin part=%d but =yend part=%d", h.Part, h.EndPart))
	}
	return blk, after, true
}

// -- BinHex --

var banner = []byte("(this file must be converted with binhex")

func isBanner(rest []byte) bool {
	t := bytes.TrimLeft(rest, " \t")
	return len(t) >= len(banner) && bytes.EqualFold(t[:len(banner)], banner)
}

func (s *scanner) binhex(pos, next int, quote []byte) (Block, int, bool) {
	f, ok := s.first(next, quote, 3)
	if !ok || bytes.TrimLeft(f.c, " \t")[0] != ':' {
		return Block{}, 0, false
	}
	blk := Block{Kind: KindBinHex, Start: pos, Quote: string(quote), Mode: -1, Size: -1, dataStart: f.pos}
	if s.sniff {
		return blk, f.next, true
	}
	iss := issues{list: &blk.Issues}
	// A line with a character outside the alphabet ends the block, unless it
	// is as wide as the first line: a character damaged in transit.
	width := len(bytes.TrimSpace(f.c))
	last, after := f.next, len(s.b)
	for p := f.pos; p < len(s.b); {
		l := s.bline(p, quote)
		if !l.ok {
			after = p
			break
		}
		if l.long {
			iss.once(CodeLineTooLong, l.coff, fmt.Sprintf("a line is longer than %d bytes; the rest of it was not read", s.lim.MaxLineBytes))
		}
		c, i := l.c, 0
		if p == f.pos {
			i = bytes.IndexByte(c, ':') + 1
		}
		closing, bad := -1, false
		for ; i < len(c); i++ {
			ch := c[i]
			if ch == ':' {
				closing = l.coff + i
				break
			}
			if ch != ' ' && ch != '\t' && hqxAlphabet[ch] < 0 {
				bad = true
			}
		}
		if bad && p != f.pos && len(bytes.TrimSpace(l.c)) != width {
			after = p
			break
		}
		if closing >= 0 {
			blk.Complete, blk.dataEnd, blk.End, after = true, closing, l.next, l.next
			break
		}
		p, last = l.next, l.next
	}
	if !blk.Complete {
		blk.dataEnd, blk.End = last, last
		iss.once(CodeMissingEnd, last, "no closing ':'; the block ends at its last BinHex line")
		after = max(after, last)
	}
	if h, ok := peekBinHex(s.b, &blk, s.lim); ok {
		blk.BinHex, blk.Name, blk.Size = &h, h.Name, int64(h.DataLength)
	}
	return blk, after, true
}
