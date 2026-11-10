package charset

import (
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// replacement is U+FFFD in UTF-8.
const replacement = "\xef\xbf\xbd"

// unknown8bitName is the registered name (RFC 1428) of text whose charset is
// not known, and the Match.Name of the decoder below.
const unknown8bitName = "unknown-8bit"

// A widened encoding is an ISO 8859 part read as the Windows code page that
// extends it: iso-8859-1 as windows-1252, iso-8859-9 as windows-1254. The two
// agree on every byte but 0x80-0x9F, where the ISO part has the C1 control
// codes, which mail does not contain, and the code page has the quotation
// marks, dashes and euro sign that mail labelled with the ISO name does.
// Browsers and mail clients have long read these labels this way.
//
// A byte the code page leaves undefined keeps its ISO meaning, so every byte
// still decodes to a character of its own and none becomes U+FFFD, as none did
// when the label was read strictly.
type widened struct {
	windows, iso *charmap.Charmap
}

var (
	latin1 = &widened{charmap.Windows1252, charmap.ISO8859_1}
	latin5 = &widened{charmap.Windows1254, charmap.ISO8859_9}
)

func (w *widened) NewDecoder() *encoding.Decoder {
	return &encoding.Decoder{Transformer: widenedDecoder{w}}
}

func (w *widened) NewEncoder() *encoding.Encoder { return w.windows.NewEncoder() }

// decodeByte is the character of one byte above 0x7F.
func (w *widened) decodeByte(c byte) rune {
	if r := w.windows.DecodeByte(c); r != utf8.RuneError {
		return r
	}
	return w.iso.DecodeByte(c)
}

type widenedDecoder struct{ *widened }

func (widenedDecoder) Reset() {}

func (d widenedDecoder) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for ; nSrc < len(src); nSrc++ {
		c := src[nSrc]
		if c < utf8.RuneSelf {
			if nDst == len(dst) {
				return nDst, nSrc, transform.ErrShortDst
			}
			dst[nDst] = c
			nDst++
			continue
		}
		r := d.decodeByte(c)
		if nDst+utf8.RuneLen(r) > len(dst) {
			return nDst, nSrc, transform.ErrShortDst
		}
		nDst += utf8.EncodeRune(dst[nDst:], r)
	}
	return nDst, nSrc, nil
}

// unknown8bit decodes text whose label names no charset, or names us-ascii
// while the text is not: every valid UTF-8 sequence is kept as it is, and any
// other byte above 0x7F is read as windows-1252, the way latin1 reads it.
// Text that is UTF-8 therefore passes through unchanged, and Western
// single-byte text, the usual content of an unlabelled or mislabelled 8-bit
// message, is readable too. The decision is made sequence by sequence, so it
// needs no look-ahead beyond one character and does not depend on how the
// input is chunked.
var unknown8bit encoding.Encoding = unknown8bitEncoding{}

type unknown8bitEncoding struct{}

func (unknown8bitEncoding) NewDecoder() *encoding.Decoder {
	return &encoding.Decoder{Transformer: unknown8bitDecoder{}}
}

// NewEncoder returns the UTF-8 encoder: UTF-8 is what the decoder reads back
// unchanged.
func (unknown8bitEncoding) NewEncoder() *encoding.Encoder {
	return unicode.UTF8.NewEncoder()
}

type unknown8bitDecoder struct{ transform.NopResetter }

func (unknown8bitDecoder) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for nSrc < len(src) {
		c := src[nSrc]
		if c < utf8.RuneSelf {
			if nDst == len(dst) {
				return nDst, nSrc, transform.ErrShortDst
			}
			dst[nDst] = c
			nDst++
			nSrc++
			continue
		}
		r, size := utf8.DecodeRune(src[nSrc:])
		if r == utf8.RuneError && size == 1 {
			if !atEOF && !utf8.FullRune(src[nSrc:]) {
				return nDst, nSrc, transform.ErrShortSrc
			}
			r = latin1.decodeByte(c)
		}
		if nDst+utf8.RuneLen(r) > len(dst) {
			return nDst, nSrc, transform.ErrShortDst
		}
		nDst += utf8.EncodeRune(dst[nDst:], r)
		nSrc += size
	}
	return nDst, nSrc, nil
}

// A utf16AutoEncoding decodes the labels that say UTF-16 without saying which
// byte order.
//
// A byte order mark decides, and is consumed. Without one RFC 2781 says
// big-endian, but the mail that omits the mark is mostly little-endian: .NET
// labels UTF-16LE bodies "utf-16" and writes no mark. So the first
// utf16SniffLen bytes are examined (see utf16Endianness), and only when they
// do not clearly say is the label's own default used: big-endian for utf-16,
// as RFC 2781 has it and as this package always read it, and little-endian
// for the Windows names unicode and ucs-2, as the WHATWG registry has it and
// as this package always read those.
//
// utf-16le and utf-16be are never examined: the label is taken at its word,
// and a leading U+FEFF is text.
type utf16AutoEncoding struct {
	unmarked unicode.Endianness // the byte order when neither a mark nor the text says
}

var (
	utf16Auto        encoding.Encoding = utf16AutoEncoding{unicode.BigEndian}
	utf16AutoWindows encoding.Encoding = utf16AutoEncoding{unicode.LittleEndian}
)

func (e utf16AutoEncoding) NewDecoder() *encoding.Decoder {
	return &encoding.Decoder{Transformer: &utf16AutoDecoder{unmarked: e.unmarked}}
}

// NewEncoder returns an encoder that writes a byte order mark, in the
// label's default byte order.
func (e utf16AutoEncoding) NewEncoder() *encoding.Encoder {
	return unicode.UTF16(e.unmarked, unicode.UseBOM).NewEncoder()
}

// utf16SniffLen is how many bytes of unmarked UTF-16 decide its byte order.
// Nothing is delivered until they are in, or the input has ended.
const utf16SniffLen = 128

type utf16AutoDecoder struct {
	unmarked unicode.Endianness
	t        transform.Transformer // the decoder chosen, once it is
}

func (d *utf16AutoDecoder) Reset() { d.t = nil }

func (d *utf16AutoDecoder) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	if d.t == nil {
		switch {
		case len(src) >= 2 && (src[0] == 0xFE && src[1] == 0xFF || src[0] == 0xFF && src[1] == 0xFE):
			d.t = unicode.UTF16(d.unmarked, unicode.UseBOM).NewDecoder()
		case len(src) < utf16SniffLen && !atEOF:
			return 0, 0, transform.ErrShortSrc
		default:
			order := utf16Endianness(src[:min(len(src), utf16SniffLen)], d.unmarked)
			d.t = unicode.UTF16(order, unicode.IgnoreBOM).NewDecoder()
		}
	}
	return d.t.Transform(dst, src, atEOF)
}

// utf16Endianness guesses the byte order of UTF-16 text without a byte order
// mark from its first bytes, and returns unmarked if they do not say.
//
// Text in one script has few distinct high bytes and many distinct low bytes:
// 0x00 for Latin, 0x04 for Cyrillic, 0x30 for kana, 0xAC-0xD7 for Hangul, and
// so on. So the byte position with clearly fewer distinct values is the high
// one. When that is not clear, as in a text of a few characters, the ASCII
// characters decide: a NUL beside an ASCII byte is that character's high byte.
func utf16Endianness(b []byte, unmarked unicode.Endianness) unicode.Endianness {
	// clearly reports whether few is clearly fewer than many: by four, or by
	// two when that is at least half, which is all a short text can show.
	clearly := func(few, many int) bool {
		return few+4 <= many || few+2 <= many && few*2 <= many
	}
	var first, second [256]bool
	var nFirst, nSecond, be, le int
	for i := 0; i+1 < len(b); i += 2 {
		x, y := b[i], b[i+1]
		if !first[x] {
			first[x] = true
			nFirst++
		}
		if !second[y] {
			second[y] = true
			nSecond++
		}
		switch {
		case x == 0 && 0 < y && y < 0x80:
			be++
		case y == 0 && 0 < x && x < 0x80:
			le++
		}
	}
	switch {
	case clearly(nSecond, nFirst):
		return unicode.LittleEndian
	case clearly(nFirst, nSecond):
		return unicode.BigEndian
	case le > be:
		return unicode.LittleEndian
	case be > le:
		return unicode.BigEndian
	}
	return unmarked
}
