package charset

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"
)

// standardUTF7 is UTF-7 as RFC 2152 defines it, the MIME charset named
// "utf-7": '+' starts a run of base64 that carries UTF-16, '-' or any
// character outside the base64 alphabet ends it, and "+-" is a plus sign.
//
// It is not the IMAP "modified UTF-7" of RFC 3501 section 5.1.3, which shifts
// with '&', uses ',' for '/' in its base64 and exists for mailbox names only.
// That is the utf7 package, and no charset label reaches it: here '&' and ','
// are ordinary characters, and an IMAP mailbox name decodes to itself.
var standardUTF7 encoding.Encoding = standardUTF7Encoding{}

type standardUTF7Encoding struct{}

func (standardUTF7Encoding) NewDecoder() *encoding.Decoder {
	return &encoding.Decoder{Transformer: &standardUTF7Decoder{}}
}

func (standardUTF7Encoding) NewEncoder() *encoding.Encoder {
	return &encoding.Encoder{Transformer: &standardUTF7Encoder{}}
}

// utf7Chunk is how many base64 characters of a run are decoded at a time. It
// is a multiple of eight, so a chunk is a whole number of UTF-16 code units.
//
// A run no longer than this is decoded as a whole or not at all: if it is not
// well-formed UTF-16, the '+' and the run are kept as the text they probably
// are, since mail that says utf-7 and is not is more common than UTF-7 with a
// damaged run. A longer run is committed to once its first chunk has decoded
// cleanly, and decoded chunk by chunk from there, with U+FFFD for whatever is
// malformed later. That keeps the decoder's memory and look-ahead fixed
// whatever the length of a run, and whatever a run holds, a call scans,
// decodes and copies each of its bytes at most once.
const utf7Chunk = 256

// utf7MaxOut is the most UTF-8 a run of n base64 characters decodes to: three
// bytes for each code unit, for a surrogate left pending by the chunk before,
// and for a final partial unit. It is also more than the n+1 bytes of a run
// kept as text.
func utf7MaxOut(n int) int { return (n*6/16 + 2) * 3 }

type standardUTF7Decoder struct {
	inRun bool // a run longer than utf7Chunk is being decoded
	high  rune // the high surrogate the last chunk of that run ended with, or 0
}

func (d *standardUTF7Decoder) Reset() { *d = standardUTF7Decoder{} }

func (d *standardUTF7Decoder) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for nSrc < len(src) || d.inRun && atEOF {
		if d.inRun {
			n := utf7RunLen(src[nSrc:])
			end := nSrc + n
			final := n < utf7Chunk
			if final && end == len(src) && !atEOF {
				return nDst, nSrc, transform.ErrShortSrc
			}
			if len(dst)-nDst < utf7MaxOut(n) {
				return nDst, nSrc, transform.ErrShortDst
			}
			w, high, _ := decodeUTF7Run(dst[nDst:], src[nSrc:end], d.high, false, final)
			nDst += w
			d.high = high
			if final {
				d.inRun = false
				if end < len(src) && src[end] == '-' {
					end++
				}
			}
			nSrc = end
			continue
		}

		ch := src[nSrc]
		if ch != '+' {
			// A directly encoded character. An octet above 0x7F is not UTF-7;
			// UTF-8 is kept, since that is what such text almost always is,
			// and anything else is U+FFFD so that the output is valid UTF-8.
			size := 1
			if ch >= utf8.RuneSelf {
				if !atEOF && !utf8.FullRune(src[nSrc:]) {
					return nDst, nSrc, transform.ErrShortSrc
				}
				var r rune
				if r, size = utf8.DecodeRune(src[nSrc:]); r == utf8.RuneError && size == 1 {
					if len(dst)-nDst < len(replacement) {
						return nDst, nSrc, transform.ErrShortDst
					}
					nDst += copy(dst[nDst:], replacement)
					nSrc++
					continue
				}
			}
			if len(dst)-nDst < size {
				return nDst, nSrc, transform.ErrShortDst
			}
			nDst += copy(dst[nDst:], src[nSrc:nSrc+size])
			nSrc += size
			continue
		}

		start := nSrc + 1
		n := utf7RunLen(src[start:])
		end := start + n
		if n < utf7Chunk && end == len(src) && !atEOF {
			return nDst, nSrc, transform.ErrShortSrc
		}
		if len(dst)-nDst < utf7MaxOut(n) {
			return nDst, nSrc, transform.ErrShortDst
		}
		final := n < utf7Chunk
		w, high, ok := decodeUTF7Run(dst[nDst:], src[start:end], 0, true, final)
		switch {
		case n == 0 && end < len(src) && src[end] == '-':
			// "+-" is a plus sign.
			dst[nDst] = '+'
			nDst++
			nSrc = end + 1
		case n == 0 || !ok:
			// Malformed UTF-7 appears in mail often enough that the charset
			// decoder should not make the whole body unreadable. Keep the
			// shift byte and the run it failed on as the text they are. A '+'
			// inside that run is part of it and not a new shift, so every
			// byte of a failed run is looked at a fixed number of times.
			nDst += copy(dst[nDst:], src[nSrc:end])
			nSrc = end
		case final:
			nDst += w
			if end < len(src) && src[end] == '-' {
				end++
			}
			nSrc = end
		default:
			nDst += w
			d.inRun, d.high = true, high
			nSrc = end
		}
	}
	return nDst, nSrc, nil
}

// utf7RunLen is the number of base64 characters b starts with, up to
// utf7Chunk.
func utf7RunLen(b []byte) int {
	n := 0
	for n < len(b) && n < utf7Chunk && isUTF7Base64Byte(b[n]) {
		n++
	}
	return n
}

// decodeUTF7Run decodes the base64 characters of run, which carry UTF-16 in
// big-endian order, into dst as UTF-8. dst must have room for
// utf7MaxOut(len(run)) bytes. high is a high surrogate the previous chunk of
// the same run ended with, and the one returned is what this chunk ends with;
// final says the run ends here, so that nothing may be left pending.
//
// A run is malformed if it has an unpaired surrogate or, at its end, a base64
// character that completes no code unit. If strict is set that makes ok false
// and whatever was written is to be discarded; otherwise each such fault
// becomes U+FFFD.
func decodeUTF7Run(dst, run []byte, high rune, strict, final bool) (n int, pending rune, ok bool) {
	var bits uint32
	var nbits uint
	for _, c := range run {
		bits = bits<<6 | uint32(utf7Value(c))
		if nbits += 6; nbits < 16 {
			continue
		}
		nbits -= 16
		u := rune(bits >> nbits & 0xFFFF)
		bits &= 1<<nbits - 1

		if high != 0 {
			if 0xDC00 <= u && u <= 0xDFFF {
				n += utf8.EncodeRune(dst[n:], utf16.DecodeRune(high, u))
				high = 0
				continue
			}
			if strict {
				return 0, 0, false
			}
			n += copy(dst[n:], replacement)
			high = 0
		}
		switch {
		case 0xD800 <= u && u <= 0xDBFF:
			high = u
		case 0xDC00 <= u && u <= 0xDFFF:
			if strict {
				return 0, 0, false
			}
			n += copy(dst[n:], replacement)
		default:
			n += utf8.EncodeRune(dst[n:], u)
		}
	}
	if final {
		if high != 0 {
			if strict {
				return 0, 0, false
			}
			n += copy(dst[n:], replacement)
			high = 0
		}
		if nbits >= 6 {
			if strict {
				return 0, 0, false
			}
			n += copy(dst[n:], replacement)
		}
	}
	return n, high, true
}

// utf7Value is the six bits a character of the base64 alphabet stands for.
func utf7Value(c byte) byte {
	switch {
	case 'A' <= c && c <= 'Z':
		return c - 'A'
	case 'a' <= c && c <= 'z':
		return c - 'a' + 26
	case '0' <= c && c <= '9':
		return c - '0' + 52
	case c == '+':
		return 62
	}
	return 63
}

type standardUTF7Encoder struct{}

func (e *standardUTF7Encoder) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 && !atEOF && !utf8.FullRune(src[i:]) {
			err = transform.ErrShortSrc
			return
		}

		var b []byte
		if r == '+' {
			b = []byte("+-")
			i += size
		} else if isUTF7DirectRune(r) {
			b = src[i : i+size]
			i += size
		} else {
			start := i
			for i < len(src) {
				r, size = utf8.DecodeRune(src[i:])
				if r == utf8.RuneError && size == 1 && !atEOF && !utf8.FullRune(src[i:]) {
					err = transform.ErrShortSrc
					return
				}
				if r == '+' || isUTF7DirectRune(r) {
					break
				}
				i += size
			}
			b = encodeStandardUTF7(src[start:i])
		}

		if nDst+len(b) > len(dst) {
			err = transform.ErrShortDst
			return
		}
		copy(dst[nDst:], b)
		nDst += len(b)
		nSrc = i
	}
	return
}

func (e *standardUTF7Encoder) Reset() {}

func encodeStandardUTF7(s []byte) []byte {
	utf16Bytes := make([]byte, 0, len(s))
	for len(s) > 0 {
		r, size := utf8.DecodeRune(s)
		if r > utf8.MaxRune {
			r, size = utf8.RuneError, 1
		}
		s = s[size:]
		if r1, r2 := utf16.EncodeRune(r); r1 != '\uFFFD' {
			utf16Bytes = append(utf16Bytes, byte(r1>>8), byte(r1))
			r = r2
		}
		utf16Bytes = append(utf16Bytes, byte(r>>8), byte(r))
	}

	b64 := base64.StdEncoding.EncodeToString(utf16Bytes)
	b64 = strings.TrimRight(b64, "=")
	return []byte("+" + b64 + "-")
}

func isUTF7Base64Byte(ch byte) bool {
	return 'A' <= ch && ch <= 'Z' ||
		'a' <= ch && ch <= 'z' ||
		'0' <= ch && ch <= '9' ||
		ch == '+' || ch == '/'
}

func isUTF7DirectByte(ch byte) bool {
	return ch == '\t' || ch == '\r' || ch == '\n' || (0x20 <= ch && ch <= 0x7e && ch != '+')
}

func isUTF7DirectRune(r rune) bool {
	if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
		return true
	}
	if 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' {
		return true
	}
	switch r {
	case '\'', '(', ')', ',', '-', '.', '/', ':', '?':
		return true
	default:
		return false
	}
}
