package charset

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"
)

var standardUTF7 encoding.Encoding = standardUTF7Encoding{}

type standardUTF7Encoding struct{}

func (standardUTF7Encoding) NewDecoder() *encoding.Decoder {
	return &encoding.Decoder{Transformer: &standardUTF7Decoder{}}
}

func (standardUTF7Encoding) NewEncoder() *encoding.Encoder {
	return &encoding.Encoder{Transformer: &standardUTF7Encoder{}}
}

type standardUTF7Decoder struct{}

func (d *standardUTF7Decoder) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if ch != '+' {
			if nDst+1 > len(dst) {
				err = transform.ErrShortDst
				return
			}
			dst[nDst] = ch
			nDst++
			nSrc = i + 1
			continue
		}

		start := i + 1
		j := start
		for j < len(src) && isUTF7Base64Byte(src[j]) {
			j++
		}

		if j == start {
			if j < len(src) && src[j] == '-' {
				if nDst+1 > len(dst) {
					err = transform.ErrShortDst
					return
				}
				dst[nDst] = '+'
				nDst++
				nSrc = j + 1
				i = j
				continue
			}
			if j == len(src) && !atEOF {
				err = transform.ErrShortSrc
				return
			}
			// Malformed UTF-7 appears in mail often enough that the charset
			// decoder should not make the whole body unreadable. Preserve the
			// shift byte literally and let the following bytes be handled as
			// ordinary input.
			if nDst+1 > len(dst) {
				err = transform.ErrShortDst
				return
			}
			dst[nDst] = '+'
			nDst++
			nSrc = start
			continue
		}

		if j == len(src) && !atEOF {
			err = transform.ErrShortSrc
			return
		}

		b := decodeStandardUTF7(src[start:j])
		if len(b) == 0 {
			if nDst+1 > len(dst) {
				err = transform.ErrShortDst
				return
			}
			dst[nDst] = '+'
			nDst++
			nSrc = start
			continue
		}
		if nDst+len(b) > len(dst) {
			err = transform.ErrShortDst
			return
		}
		copy(dst[nDst:], b)
		nDst += len(b)

		if j < len(src) && src[j] == '-' {
			j++
		}
		nSrc = j
		i = j - 1
	}
	return
}

func (d *standardUTF7Decoder) Reset() {}

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

func decodeStandardUTF7(b64 []byte) []byte {
	if len(b64) == 0 || len(b64)&3 == 1 {
		return nil
	}
	for _, b := range b64 {
		if b == '=' {
			return nil
		}
	}

	padded := make([]byte, len(b64))
	copy(padded, b64)
	if rem := len(padded) & 3; rem != 0 {
		padded = append(padded, strings.Repeat("=", 4-rem)...)
	}

	utf16Bytes := make([]byte, base64.StdEncoding.DecodedLen(len(padded)))
	n, err := base64.StdEncoding.Decode(utf16Bytes, padded)
	if err != nil || n&1 == 1 {
		return nil
	}
	utf16Bytes = utf16Bytes[:n]

	out := make([]byte, n*2)
	j := 0
	for i := 0; i < n; i += 2 {
		r := rune(utf16Bytes[i])<<8 | rune(utf16Bytes[i+1])
		if utf16.IsSurrogate(r) {
			if i += 2; i == n {
				return nil
			}
			r2 := rune(utf16Bytes[i])<<8 | rune(utf16Bytes[i+1])
			if r = utf16.DecodeRune(r, r2); r == '\uFFFD' {
				return nil
			}
		}
		j += utf8.EncodeRune(out[j:], r)
	}
	return out[:j]
}

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
