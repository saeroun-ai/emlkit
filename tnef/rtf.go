package tnef

import (
	"errors"
	"hash/crc32"
	"unicode/utf8"
)

// Compressed RTF (MS-OXRTFCP): a 16-byte header — the size of what follows
// its first field, the size of the RTF, the compression type and a CRC — then
// the RTF, either as it is ("MELA") or compressed ("LZFu").
const (
	rtfLZFu uint32 = 0x75465A4C
	rtfMELA uint32 = 0x414C454D
)

// rtfDict is what the LZFu dictionary holds before the first byte is written.
const rtfDict = "{\\rtf1\\ansi\\mac\\deff0\\deftab720{\\fonttbl;}" +
	"{\\f0\\fnil \\froman \\fswiss \\fmodern \\fscript \\fdecor " +
	"MS Sans SerifSymbolArialTimes New RomanCourier" +
	"{\\colortbl\\red0\\green0\\blue0\r\n\\par " +
	"\\pard\\plain\\f0\\fs20\\b\\i\\u\\tab\\tx"

type rtfStatus uint8

const (
	rtfShort   rtfStatus = 1 << iota // no whole header
	rtfUnknown                       // unknown compression type
	rtfBadCRC                        // CRC does not match
	rtfCut                           // input ends before the RTF does
	rtfLimit                         // output limit reached
)

// DecompressRTF decompresses a PR_RTF_COMPRESSED value. It writes at most
// maxOutput bytes. The error says what was wrong with src; the RTF that could
// be recovered is returned with it, so a non-nil error does not mean an empty
// result. For the uncompressed form the result aliases src.
func DecompressRTF(src []byte, maxOutput int) ([]byte, error) {
	out, st := decompressRTF(src, maxOutput)
	switch {
	case st&rtfShort != 0:
		return out, errors.New("tnef: compressed RTF shorter than its header")
	case st&rtfUnknown != 0:
		return out, errors.New("tnef: compressed RTF of an unknown compression type")
	case st&rtfLimit != 0:
		return out, errors.New("tnef: compressed RTF exceeds the output limit")
	case st&rtfCut != 0:
		return out, errors.New("tnef: compressed RTF is cut short")
	case st&rtfBadCRC != 0:
		return out, errors.New("tnef: compressed RTF fails its CRC")
	}
	return out, nil
}

func decompressRTF(src []byte, max int) ([]byte, rtfStatus) {
	if len(src) < 16 {
		return nil, rtfShort
	}
	if max < 0 {
		max = 0
	}
	var st rtfStatus
	comp, raw, typ, crc := le32(src), le32(src[4:]), le32(src[8:]), le32(src[12:])
	body := src[16:]
	// The compressed size counts the twelve header bytes after it.
	switch {
	case comp >= 12 && uint64(comp-12) < uint64(len(body)):
		body = body[:comp-12]
	case uint64(comp) > uint64(len(body))+12:
		st |= rtfCut
	}
	limit := max
	if uint64(raw) < uint64(limit) {
		limit = int(raw)
	}

	switch typ {
	case rtfMELA:
		if len(body) < limit {
			limit = len(body)
			st |= rtfCut
		} else if uint64(raw) > uint64(limit) {
			st |= rtfLimit
		}
		return body[:limit:limit], st
	case rtfLZFu:
	default:
		return nil, st | rtfUnknown
	}

	// The CRC is CRC-32 with a zero start and no final inversion.
	if ^crc32.Update(^uint32(0), crc32.IEEETable, body) != crc {
		st |= rtfBadCRC
	}
	// A reference yields at most 17 bytes for two, so the output cannot pass
	// nine times the input: the declared size alone sizes nothing.
	size := limit
	if most := 9 * int64(len(body)); int64(size) > most {
		size = int(most)
	}
	out := make([]byte, 0, size)
	var dict [4096]byte
	w := copy(dict[:], rtfDict)
	done := false
	i := 0
loop:
	for i < len(body) {
		ctrl := body[i]
		i++
		for bit := 0; bit < 8; bit++ {
			if len(out) >= limit {
				done = true
				break loop
			}
			if ctrl&(1<<bit) == 0 {
				if i >= len(body) {
					break loop
				}
				c := body[i]
				i++
				out = append(out, c)
				dict[w] = c
				w = (w + 1) & 4095
				continue
			}
			if i+2 > len(body) {
				break loop
			}
			ref := int(body[i])<<8 | int(body[i+1])
			i += 2
			off, n := ref>>4, ref&15+2
			if off == w {
				done = true
				break loop
			}
			for k := 0; k < n && len(out) < limit; k++ {
				c := dict[(off+k)&4095]
				out = append(out, c)
				dict[w] = c
				w = (w + 1) & 4095
			}
		}
	}
	switch {
	case uint64(raw) > uint64(limit) && len(out) >= limit:
		st |= rtfLimit
	case !done && uint64(len(out)) < uint64(raw):
		st |= rtfCut
	}
	return out, st
}

// An RTFKind says what an RTF document carries.
type RTFKind uint8

const (
	// RTFPlain is RTF written as RTF.
	RTFPlain RTFKind = iota
	// RTFHTML is RTF that encapsulates an HTML document (\fromhtml).
	RTFHTML
	// RTFText is RTF that encapsulates plain text (\fromtext).
	RTFText
)

func (k RTFKind) String() string {
	switch k {
	case RTFHTML:
		return "html"
	case RTFText:
		return "text"
	}
	return "rtf"
}

const maxRTFDepth = 1024

// Destinations whose text is not part of the document.
func rtfSkipped(word []byte) bool {
	switch string(word) {
	case "fonttbl", "colortbl", "stylesheet", "info", "pict", "object",
		"objdata", "themedata", "colorschememapping", "latentstyles",
		"datastore", "listtable", "listoverridetable", "rsidtbl", "revtbl",
		"filetbl", "xmlnstbl", "header", "headerl", "headerr", "headerf",
		"footer", "footerl", "footerr", "footerf", "fldinst", "generator",
		"bkmkstart", "bkmkend", "mhtmltag":
		return true
	}
	return false
}

type rtfGroup struct {
	skip    bool  // inside a destination that is not content
	tag     bool  // inside {\*\htmltag}
	htmlrtf bool  // \htmlrtf: RTF that stands in for the HTML, not part of it
	uc      uint8 // \uc: fallback characters after each \u
}

type rtfReader struct {
	kind RTFKind
	cp   int
	max  int
	out  []byte
	raw  []byte // text bytes in the code page, not yet decoded
	cur  rtfGroup
	skip int  // fallback characters still to drop
	hi   rune // pending high surrogate of a \u pair
	full bool
}

func (x *rtfReader) emitting() bool {
	return !x.cur.skip && (x.kind != RTFHTML || x.cur.tag || !x.cur.htmlrtf)
}

// flush decodes the pending text bytes. A high surrogate is pending only
// while there are none.
func (x *rtfReader) flush() {
	if x.hi != 0 {
		x.hi = 0
		x.out = utf8.AppendRune(x.out, utf8.RuneError)
	}
	if len(x.raw) > 0 {
		s, _ := DecodeText(x.raw, x.cp)
		x.out = append(x.out, s...)
		x.raw = x.raw[:0]
	}
	if len(x.out) > x.max {
		x.full = true
	}
}

// char takes one byte of text in the document's code page.
func (x *rtfReader) char(c byte) {
	if x.skip > 0 {
		x.skip--
		return
	}
	if !x.emitting() {
		return
	}
	if x.hi != 0 {
		x.flush()
	}
	x.raw = append(x.raw, c)
	// Decode in pieces, so that what is pending stays small. A piece ends
	// after a byte below 0x80, which ends a character in every multi-byte
	// code page read here, or is cut at 8 KiB all the same.
	if n := len(x.raw); (n >= 4096 && c < 0x80) || n >= 8192 || len(x.out)+n > x.max {
		x.flush()
	}
}

// rune takes one character given as Unicode.
func (x *rtfReader) rune(r rune) {
	if !x.emitting() {
		return
	}
	x.flush()
	x.out = utf8.AppendRune(x.out, r)
}

func (x *rtfReader) text(s string) {
	if x.emitting() {
		x.flush()
		x.out = append(x.out, s...)
	}
}

// unicode takes the argument of \u.
func (x *rtfReader) unicode(n int) {
	x.skip = int(x.cur.uc)
	if !x.emitting() {
		return
	}
	r := rune(uint16(n))
	switch {
	case r >= 0xD800 && r < 0xDC00:
		x.flush()
		x.hi = r
		return
	case r >= 0xDC00 && r < 0xE000:
		if x.hi == 0 {
			r = utf8.RuneError
		} else {
			r = 0x10000 + (x.hi-0xD800)<<10 + (r - 0xDC00)
			x.hi = 0
		}
	}
	x.rune(r)
}

func (x *rtfReader) newline() {
	if x.kind == RTFHTML {
		x.text("\r\n")
	} else {
		x.text("\n")
	}
}

// word takes one control word. It returns how many bytes of binary data
// follow it (\bin).
func (x *rtfReader) word(w []byte, star, has bool, n int) int {
	switch string(w) {
	case "htmltag":
		if star && x.kind == RTFHTML && !x.cur.skip {
			x.cur.tag = true
		} else {
			x.cur.skip = true
		}
	case "fromhtml", "fromtext":
		// The header says so, before any content; later it is only a word.
		if len(x.out) == 0 && len(x.raw) == 0 && (!has || n != 0) {
			x.kind = RTFText
			if w[4] == 'h' {
				x.kind = RTFHTML
			}
		}
	case "htmlrtf":
		x.cur.htmlrtf = !has || n != 0
	case "par", "line", "sect", "page", "row":
		x.newline()
	case "tab", "cell":
		x.text("\t")
	case "lquote":
		x.rune(0x2018)
	case "rquote":
		x.rune(0x2019)
	case "ldblquote":
		x.rune(0x201C)
	case "rdblquote":
		x.rune(0x201D)
	case "bullet":
		x.rune(0x2022)
	case "endash":
		x.rune(0x2013)
	case "emdash":
		x.rune(0x2014)
	case "u":
		x.unicode(n)
	case "uc":
		if has && n >= 0 && n <= 16 {
			x.cur.uc = uint8(n)
		}
	case "ansicpg":
		if has && n > 0 && CodepageSupported(n) {
			x.cp = n
		}
	case "bin":
		if has && n > 0 {
			return n
		}
	default:
		if x.cur.tag {
			break
		}
		if star || rtfSkipped(w) {
			x.cur.skip = true
		}
	}
	return 0
}

func hexval(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func letter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

// ExtractRTF returns what an RTF document carries: the HTML document or the
// plain text it encapsulates (MS-OXRTFEX), or for ordinary RTF its text
// without formatting. The result is UTF-8 and at most maxOutput bytes;
// codepage is the code page of 8-bit characters when the RTF declares none
// (\ansicpg), and zero is Windows-1252. It reports false when the document
// was read only in part: it is nested too deep or its content exceeds
// maxOutput.
//
// The rendering of ordinary RTF is a reading aid, not a conversion: tables,
// lists and fields lose their structure, and characters are read in the
// document's code page, not in the one of each font.
func ExtractRTF(rtf []byte, codepage int, maxOutput int) (RTFKind, string, bool) {
	x := rtfReader{max: maxOutput, cp: codepage, cur: rtfGroup{uc: 1}}
	if !CodepageSupported(x.cp) {
		x.cp = 1252
	}
	if x.max < 0 {
		x.max = 0
	}
	complete := true
	var stack []rtfGroup
	star := false
	i := 0
	for i < len(rtf) && !x.full {
		c := rtf[i]
		i++
		switch c {
		case '{':
			if len(stack) >= maxRTFDepth {
				complete = false
				i = len(rtf)
				break
			}
			stack = append(stack, x.cur)
			x.skip, star = 0, false
		case '}':
			if n := len(stack); n > 0 {
				x.cur, stack = stack[n-1], stack[:n-1]
			}
			x.skip, star = 0, false
		case '\r', '\n', 0:
		case '\\':
			if i >= len(rtf) {
				break
			}
			c = rtf[i]
			if !letter(c) {
				i++
				switch c {
				case '\'':
					if i+1 < len(rtf) {
						if h, l := hexval(rtf[i]), hexval(rtf[i+1]); h >= 0 && l >= 0 {
							x.char(byte(h<<4 | l))
						}
						i += 2
					} else {
						i = len(rtf)
					}
				case '\\', '{', '}':
					x.char(c)
				case '~':
					x.rune(0xA0)
				case '_':
					x.char('-')
				case '\r', '\n':
					x.newline()
				case '*':
					star = true
					continue
				}
				star = false
				break
			}
			start := i
			for i < len(rtf) && i-start < 32 && letter(rtf[i]) {
				i++
			}
			word := rtf[start:i]
			neg, has, n := false, false, 0
			if i+1 < len(rtf) && rtf[i] == '-' && rtf[i+1] >= '0' && rtf[i+1] <= '9' {
				neg = true
				i++
			}
			for d := 0; i < len(rtf) && d < 10 && rtf[i] >= '0' && rtf[i] <= '9'; d++ {
				if n < 1<<26 {
					n = n*10 + int(rtf[i]-'0')
				}
				has = true
				i++
			}
			if neg {
				n = -n
			}
			if i < len(rtf) && rtf[i] == ' ' {
				i++
			}
			if bin := x.word(word, star, has, n); bin > 0 {
				if bin > len(rtf)-i {
					bin = len(rtf) - i
				}
				i += bin
			}
			star = false
		default:
			x.char(c)
			star = false
		}
	}
	x.flush()
	if x.full {
		complete = false
	}
	out := x.out
	if len(out) > x.max {
		n := x.max
		for n > 0 && out[n]&0xC0 == 0x80 {
			n--
		}
		out = out[:n]
	}
	return x.kind, string(out), complete
}
