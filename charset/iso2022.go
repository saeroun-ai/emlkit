package charset

import (
	"errors"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// The 7-bit charsets that switch between ASCII and a double-byte set with
// escape or shift sequences: ISO-2022-JP (RFC 1468), ISO-2022-KR (RFC 1557)
// and HZ (RFC 1843).
//
// Each decoder here is two streaming steps: a small state machine that turns
// the 7-bit text into the 8-bit EUC form of the same character set, followed
// by the golang.org/x/text decoder for that EUC form. The tables and the
// characters they produce are therefore exactly golang.org/x/text's; only the
// framing is decided here.
//
// golang.org/x/text has decoders for ISO-2022-JP and HZ, and none for
// ISO-2022-KR. Text that follows the RFCs decodes the same through either,
// which iso2022_test.go checks against them. They differ on text that does
// not, where the decoders of golang.org/x/text lose their place:
//
//   - Every state is over at the end of a line, CR as well as LF. All three
//     RFCs require a line to end in ASCII, and golang.org/x/text agrees for a
//     bare LF in ISO-2022-JP. But mail lines end in CR LF, which it reads as
//     one double-byte character, and so decodes the following ASCII lines as
//     ideographs when a line is cut short or its closing escape is missing.
//   - A control character or a space inside a double-byte run is itself, as
//     ISO 2022 defines, and not half of a character.
//   - A first byte without a valid second byte is one U+FFFD, and the byte
//     after it is read on its own, so an escape sequence that follows a
//     truncated character still takes effect.
//
// A state is only ever changed by a sequence that consumes input, and such a
// sequence emits nothing: a flood of them costs time in proportion to its
// length, and no output.
var (
	iso2022JP encoding.Encoding = &sevenBit{
		step:    func() transform.Transformer { return new(jisToEUC) },
		euc:     japanese.EUCJP,
		encoder: japanese.ISO2022JP,
	}
	iso2022KR encoding.Encoding = &sevenBit{
		step: func() transform.Transformer { return new(iso2022KRToEUC) },
		euc:  korean.EUCKR,
	}
	hzGB2312 encoding.Encoding = &sevenBit{
		step:    func() transform.Transformer { return new(hzToEUC) },
		euc:     simplifiedchinese.GBK,
		encoder: simplifiedchinese.HZGB2312,
	}
)

// A sevenBit is a 7-bit charset decoded by way of its EUC form.
type sevenBit struct {
	step    func() transform.Transformer // 7-bit to EUC
	euc     encoding.Encoding
	encoder encoding.Encoding // nil if golang.org/x/text cannot encode the charset
}

func (s *sevenBit) NewDecoder() *encoding.Decoder {
	return &encoding.Decoder{Transformer: transform.Chain(s.step(), s.euc.NewDecoder())}
}

func (s *sevenBit) NewEncoder() *encoding.Encoder {
	if s.encoder != nil {
		return s.encoder.NewEncoder()
	}
	return &encoding.Encoder{Transformer: noEncoder{}}
}

var errNoEncoder = errors.New("charset: encoding to this charset is not supported")

// noEncoder is the encoder of a charset this package only decodes.
type noEncoder struct{ transform.NopResetter }

func (noEncoder) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	if len(src) == 0 {
		return 0, 0, nil
	}
	return 0, 0, errNoEncoder
}

// eucInvalid is a byte no EUC decoder accepts: each one becomes U+FFFD.
const eucInvalid = 0xFF

// isGraphic reports whether c is one of the 94 bytes a double-byte character
// is made of. Everything else below 0x80 is a control character, the space or
// DEL, which mean themselves in every state.
func isGraphic(c byte) bool { return 0x21 <= c && c <= 0x7E }

// jisToEUC converts ISO-2022-JP to EUC-JP. Beyond RFC 1468 it reads what the
// same label carries in practice: JIS X 0212 (ISO-2022-JP-1), JIS X 0201
// katakana by ESC ( I (code page 50221) or SO and SI (code page 50222), and
// the four-byte forms of the JIS X 0208 escapes.
type jisToEUC struct {
	set  uint8 // what ESC designated last
	kana bool  // SO is in effect
}

const (
	jisASCII uint8 = iota // ASCII or JIS X 0201 Roman, read as ASCII
	jisKana               // JIS X 0201 katakana
	jis0208
	jis0212
)

func (d *jisToEUC) Reset() { *d = jisToEUC{} }

func (d *jisToEUC) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for nSrc < len(src) {
		c := src[nSrc]
		out, n, size := [3]byte{c}, 1, 1
		switch {
		case c == 0x1B:
			set, length := jisEscape(src[nSrc:])
			if length == 0 && !atEOF && len(src)-nSrc < 4 {
				return nDst, nSrc, transform.ErrShortSrc
			}
			if length != 0 {
				d.set = set
				nSrc += length
				continue
			}
			// An escape sequence that designates nothing known: U+FFFD for the
			// ESC, and what follows it is text.
			out[0] = eucInvalid
		case c == 0x0E || c == 0x0F:
			d.kana = c == 0x0E
			nSrc++
			continue
		case c == '\r' || c == '\n':
			*d = jisToEUC{}
		case c >= 0x80:
			out[0] = eucInvalid
		case !isGraphic(c) || d.set == jisASCII && !d.kana:
			// Itself.
		case d.kana || d.set == jisKana:
			if out[0] = eucInvalid; c <= 0x5F {
				out, n = [3]byte{0x8E, c | 0x80}, 2
			}
		case nSrc+1 == len(src) && !atEOF:
			return nDst, nSrc, transform.ErrShortSrc
		case nSrc+1 == len(src) || !isGraphic(src[nSrc+1]):
			out[0] = eucInvalid
		case d.set == jis0212:
			out, n, size = [3]byte{0x8F, c | 0x80, src[nSrc+1] | 0x80}, 3, 2
		default:
			out, n, size = [3]byte{c | 0x80, src[nSrc+1] | 0x80}, 2, 2
		}
		if len(dst)-nDst < n {
			return nDst, nSrc, transform.ErrShortDst
		}
		nDst += copy(dst[nDst:], out[:n])
		nSrc += size
	}
	return nDst, nSrc, nil
}

// jisEscape reads the escape sequence at the start of b, which begins with
// ESC. It returns the set the sequence designates and its length, or a zero
// length if it is not one this decoder knows, or not all there yet.
func jisEscape(b []byte) (set uint8, length int) {
	if len(b) < 3 {
		return 0, 0
	}
	switch string(b[1:3]) {
	case "(B", "(J":
		return jisASCII, 3
	case "(I":
		return jisKana, 3
	case "$@", "$B":
		return jis0208, 3
	case "$(":
		if len(b) < 4 {
			return 0, 0
		}
		switch b[3] {
		case '@', 'B':
			return jis0208, 4
		case 'D':
			return jis0212, 4
		}
	}
	return 0, 0
}

// iso2022KRToEUC converts ISO-2022-KR to EUC-KR: SO starts a run of KS X 1001
// characters and SI ends it. The designation ESC $ ) C, which RFC 1557 puts at
// the head of the text, carries no information here and is dropped wherever it
// stands.
//
// A byte above 0x7F has no place in ISO-2022-KR. It is passed on to the
// EUC-KR decoder, which is code page 949: text that is 8-bit Korean under
// this label decodes, and anything else becomes U+FFFD there.
type iso2022KRToEUC struct {
	shifted bool // SO is in effect
}

func (d *iso2022KRToEUC) Reset() { d.shifted = false }

func (d *iso2022KRToEUC) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	const designation = "\x1b$)C"
	for nSrc < len(src) {
		c := src[nSrc]
		out, n, size := [2]byte{c}, 1, 1
		switch {
		case c == 0x1B:
			rest := src[nSrc:]
			if len(rest) < len(designation) && !atEOF {
				return nDst, nSrc, transform.ErrShortSrc
			}
			if len(rest) >= len(designation) && string(rest[:len(designation)]) == designation {
				nSrc += len(designation)
				continue
			}
			out[0] = eucInvalid
		case c == 0x0E || c == 0x0F:
			d.shifted = c == 0x0E
			nSrc++
			continue
		case c == '\r' || c == '\n':
			d.shifted = false
		case !d.shifted || !isGraphic(c):
			// Itself.
		case nSrc+1 == len(src) && !atEOF:
			return nDst, nSrc, transform.ErrShortSrc
		case nSrc+1 == len(src) || !isGraphic(src[nSrc+1]):
			out[0] = eucInvalid
		default:
			out, n, size = [2]byte{c | 0x80, src[nSrc+1] | 0x80}, 2, 2
		}
		if len(dst)-nDst < n {
			return nDst, nSrc, transform.ErrShortDst
		}
		nDst += copy(dst[nDst:], out[:n])
		nSrc += size
	}
	return nDst, nSrc, nil
}

// hzToEUC converts HZ to EUC-CN, which the GBK decoder reads: "~{" starts a
// run of GB 2312 characters and "~}" ends it, "~~" is a tilde, and a tilde
// before a line break joins the two lines.
//
// A tilde followed by anything else is not HZ. Outside a run it is kept as the
// tilde it most likely is, in a URL or a path, and inside one it is U+FFFD.
type hzToEUC struct {
	gb bool // between "~{" and "~}"
}

func (d *hzToEUC) Reset() { d.gb = false }

func (d *hzToEUC) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for nSrc < len(src) {
		c := src[nSrc]
		rest := src[nSrc+1:]
		out, n, size := [2]byte{c}, 1, 1
		switch {
		case c == '~':
			// "~\r\n" is the longest sequence a tilde starts.
			if !atEOF && (len(rest) == 0 || len(rest) == 1 && rest[0] == '\r') {
				return nDst, nSrc, transform.ErrShortSrc
			}
			switch {
			case len(rest) > 0 && (rest[0] == '{' || rest[0] == '}'):
				d.gb = rest[0] == '{'
				nSrc += 2
				continue
			case len(rest) > 0 && rest[0] == '~':
				size = 2
			case len(rest) > 0 && rest[0] == '\n':
				nSrc += 2
				continue
			case len(rest) > 1 && rest[0] == '\r' && rest[1] == '\n':
				nSrc += 3
				continue
			case d.gb:
				out[0] = eucInvalid
			}
		case c == '\r' || c == '\n':
			d.gb = false
		case c >= 0x80:
			out[0] = eucInvalid
		case !d.gb || !isGraphic(c):
			// Itself.
		case len(rest) == 0 && !atEOF:
			return nDst, nSrc, transform.ErrShortSrc
		case len(rest) == 0 || !isGraphic(rest[0]):
			out[0] = eucInvalid
		default:
			out, n, size = [2]byte{c | 0x80, rest[0] | 0x80}, 2, 2
		}
		if len(dst)-nDst < n {
			return nDst, nSrc, transform.ErrShortDst
		}
		nDst += copy(dst[nDst:], out[:n])
		nSrc += size
	}
	return nDst, nSrc, nil
}
