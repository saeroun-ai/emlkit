package tnef

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// Code pages DecodeText handles itself.
const (
	cpUTF16LE = 1200
	cpASCII   = 20127
	cpUTF8    = 65001
)

// codepageEncoding returns the decoder of a Windows code page, or nil.
func codepageEncoding(cp int) encoding.Encoding {
	switch cp {
	case 437:
		return charmap.CodePage437
	case 850:
		return charmap.CodePage850
	case 852:
		return charmap.CodePage852
	case 855:
		return charmap.CodePage855
	case 858:
		return charmap.CodePage858
	case 860:
		return charmap.CodePage860
	case 862:
		return charmap.CodePage862
	case 863:
		return charmap.CodePage863
	case 865:
		return charmap.CodePage865
	case 866:
		return charmap.CodePage866
	case 874:
		return charmap.Windows874
	case 932:
		return japanese.ShiftJIS
	case 936:
		return simplifiedchinese.GBK
	case 949, 51949:
		// The x/text "EUC-KR" is the WHATWG one: Windows-949 (UHC), of which
		// EUC-KR is a part.
		return korean.EUCKR
	case 950:
		return traditionalchinese.Big5
	case 1250:
		return charmap.Windows1250
	case 1251:
		return charmap.Windows1251
	case 1252:
		return charmap.Windows1252
	case 1253:
		return charmap.Windows1253
	case 1254:
		return charmap.Windows1254
	case 1255:
		return charmap.Windows1255
	case 1256:
		return charmap.Windows1256
	case 1257:
		return charmap.Windows1257
	case 1258:
		return charmap.Windows1258
	case 10000:
		return charmap.Macintosh
	case 10007:
		return charmap.MacintoshCyrillic
	case 20866:
		return charmap.KOI8R
	case 21866:
		return charmap.KOI8U
	case 20932, 51932:
		return japanese.EUCJP
	case 28591:
		return charmap.ISO8859_1
	case 28592:
		return charmap.ISO8859_2
	case 28593:
		return charmap.ISO8859_3
	case 28594:
		return charmap.ISO8859_4
	case 28595:
		return charmap.ISO8859_5
	case 28596:
		return charmap.ISO8859_6
	case 28597:
		return charmap.ISO8859_7
	case 28598, 38598:
		return charmap.ISO8859_8
	case 28599:
		return charmap.ISO8859_9
	case 28603:
		return charmap.ISO8859_13
	case 28605:
		return charmap.ISO8859_15
	case 50220, 50221, 50222:
		return japanese.ISO2022JP
	case 52936:
		return simplifiedchinese.HZGB2312
	case 54936:
		return simplifiedchinese.GB18030
	}
	return nil
}

// stateful reports that ASCII bytes in the code page are not always ASCII
// text.
func stateful(cp int) bool {
	switch cp {
	case cpUTF16LE, 50220, 50221, 50222, 52936:
		return true
	}
	return false
}

// CodepageSupported reports whether DecodeText can decode the Windows code
// page cp.
func CodepageSupported(cp int) bool {
	switch cp {
	case cpUTF16LE, cpASCII, cpUTF8:
		return true
	}
	return codepageEncoding(cp) != nil
}

// DecodeText decodes b from the Windows code page cp to UTF-8. Bytes that are
// not text in the code page become U+FFFD.
//
// It reports false when it had to guess: cp is zero or a code page it does
// not support, and b is not plain ASCII. The guess is UTF-8 when b is valid
// UTF-8 and Windows-1252 otherwise.
func DecodeText(b []byte, cp int) (string, bool) {
	if !stateful(cp) && ascii(b) {
		return string(b), true
	}
	switch cp {
	case cpUTF8:
		return strings.ToValidUTF8(string(b), "\uFFFD"), true
	case cpUTF16LE:
		return decodeUTF16(b), true
	case cpASCII:
		out := make([]byte, 0, len(b)+8)
		for _, c := range b {
			if c < 0x80 {
				out = append(out, c)
			} else {
				out = utf8.AppendRune(out, utf8.RuneError)
			}
		}
		return string(out), true
	}
	if enc := codepageEncoding(cp); enc != nil {
		return decodeWith(enc, b), true
	}
	if utf8.Valid(b) {
		return string(b), false
	}
	return decodeWith(charmap.Windows1252, b), false
}

func decodeWith(enc encoding.Encoding, b []byte) string {
	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		// The decoders replace what they cannot read and do not fail; this
		// keeps a result if one ever does.
		return strings.ToValidUTF8(string(b), "\uFFFD")
	}
	return string(out)
}

func ascii(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}
