package charset

import (
	"fmt"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

// An alias is what one label of the alias table resolves to.
type alias struct {
	enc    encoding.Encoding
	name   string // the lower-case MIME name of what enc decodes
	repair Repair // RepairSuperset or RepairFallback, where that applies
}

// aliases is the alias table: the labels this package resolves itself, ahead
// of the IANA and WHATWG registries. Keys are lower-case.
//
// It holds three kinds of label:
//
//   - registered charsets golang.org/x/text has no decoder for, or which this
//     package decodes itself (utf-7, utf-32, iso-2022-kr, the generic utf-16
//     labels);
//   - vendor names and misspellings no registry lists (cp949, x-ms-cp932,
//     win-1251, iso8859_1, utf_8, ...), mapped to the charset they mean;
//   - labels that name no charset (unknown-8bit, x-unknown, default, the empty
//     label), mapped to the unknown-8bit decoder.
//
// A label is added only for a charset it unambiguously means. Near misses
// such as "cp-949" or "x-cp949" stay unknown, as ROB-080-011 pinned them: an
// unknown charset keeps the raw body, a wrong guess does not.
var aliases = buildAliases()

func buildAliases() map[string]alias {
	groups := []struct {
		name   string
		enc    encoding.Encoding
		repair Repair
		labels string
	}{
		// Unicode. utf-7 is standard UTF-7 (RFC 2152), never the IMAP mailbox
		// encoding of the utf7 package: see utf7.go.
		{"utf-8", unicode.UTF8, 0, "utf_8 x-utf-8 x-utf8 x-utf_8j utf8mb3 utf8mb4 unicode-2-0-utf-8"},
		{"utf-7", standardUTF7, 0, "utf-7 csutf7 unicode-1-1-utf-7 csunicode11utf7 utf7 x-utf-7 unicode-2-0-utf-7 x-unicode-2-0-utf-7"},
		{"utf-16", utf16AutoWindows, 0, "unicode ucs-2 ucs2 iso-10646-ucs-2 csunicode unicodefeff"},
		{"utf-16le", unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), 0, "utf-16-le ucs-2le x-utf-16le"},
		{"utf-16be", unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), 0, "utf-16-be ucs-2be x-utf-16be"},
		{"utf-32", utf32.UTF32(utf32.BigEndian, utf32.UseBOM), 0, "utf-32 csutf32 utf32 ucs-4 ucs4 iso-10646-ucs-4 csucs4"},
		{"utf-32be", utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), 0, "utf-32be csutf32be"},
		{"utf-32le", utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), 0, "utf-32le csutf32le"},

		// Korean. korean.EUCKR is the WHATWG "EUC-KR" decoder, which is Windows
		// code page 949 (Unified Hangul Code) with its extended syllables: what
		// euc-kr, windows-949 and ks_c_5601-1987 already resolve to.
		{"euc-kr", korean.EUCKR, 0, "cp949 x-windows-949 ms949 uhc x-uhc euc_kr x-euc-kr " +
			"ks_c_5601 ks_c_5601_1987 ksc5601-1987 ksc5601_1987 ks-c-5601-1987 ks-c-5601"},
		{"iso-2022-kr", iso2022KR, 0, "iso-2022-kr csiso2022kr iso2022kr iso2022-kr"},

		// Japanese. japanese.ShiftJIS is the WHATWG decoder, which is Windows
		// code page 932 with the NEC and IBM extensions, and the ISO-2022-JP
		// decoder reads the same table, so the code page 5022x labels decode
		// their extension characters too.
		{"shift_jis", japanese.ShiftJIS, 0, "cp932 x-ms-cp932 windows-932 x-ms932 sjis-win s-jis x-shift-jis x-shift_jis cswindows31j"},
		{"euc-jp", japanese.EUCJP, 0, "eucjp euc_jp x-eucjp ujis eucjp-win eucjp-ms cp51932"},
		{"iso-2022-jp", iso2022JP, 0, "iso2022-jp jis iso-2022-jp-1 iso-2022-jp-ms iso-2022-jp-cp932 x-windows-iso2022jp " +
			"cp50220 cp50221 cp50222 windows-50220 windows-50221 x-windows-50220 x-windows-50221"},

		// Chinese. gb2312 itself is decoded as its superset GBK by the WHATWG
		// registry, as every mail client does.
		{"gbk", simplifiedchinese.GBK, 0, "gb2312-80 gb-2312 euc-cn euccn x-euc-cn x-windows-936"},
		{"gb18030", simplifiedchinese.GB18030, 0, "gb-18030 gb18030-2000 gb18030-2005 cp54936 windows-54936"},
		{"hz-gb-2312", hzGB2312, 0, "hz hz-gb2312"},
		{"big5", traditionalchinese.Big5, 0, "x-big5 big-5 cp950 ms950 windows-950 x-windows-950 big5-eten big5hkscs"},

		// Single-byte sets.
		{"windows-1252", latin1, RepairSuperset, "latin-1 latin_1 iso-latin-1 isolatin1"},
		{"iso-8859-1", charmap.ISO8859_1, 0, "ansi_x3.110-1983"}, // RFC 1345 page 62, mostly a superset of ISO 8859-1
		{"iso-8859-15", charmap.ISO8859_15, 0, "latin9 latin0"},
		{"iso-8859-13", charmap.ISO8859_13, 0, "latin7"},
		{"windows-874", charmap.Windows874, 0, "tis620 cp874 ms874 x-windows-874"},
		{"koi8-r", charmap.KOI8R, 0, "koi-8-r koi-8 x-koi8-r"},
		{"koi8-u", charmap.KOI8U, 0, "koi8_u koi-8-u x-koi8-u"},
		{"ibm866", charmap.CodePage866, 0, "dos-866 x-cp866 x-ibm866"},
		{"x-mac-cyrillic", charmap.MacintoshCyrillic, 0, "maccyrillic mac-cyrillic"},
		{"macintosh", charmap.Macintosh, 0, "macroman mac-roman x-macroman x-macintosh"},

		// No charset at all, and us-ascii spelled in ways no registry lists
		// (the registered spellings are widened in find).
		{unknown8bitName, unknown8bit, RepairFallback, "unknown-8bit csunknown8bit x-unknown unknown default x-default " +
			"x-user-defined none null undefined binary 8bit"},
		{unknown8bitName, unknown8bit, RepairSuperset, "646 usascii us_ascii"},
	}

	m := make(map[string]alias)
	add := func(label string, a alias) {
		if _, dup := m[label]; dup {
			panic("charset: duplicate alias " + label)
		}
		m[label] = a
	}
	for _, g := range groups {
		for _, label := range strings.Fields(g.labels) {
			add(label, alias{g.enc, g.name, g.repair})
		}
	}
	// The empty label: a charset parameter with nothing in it once cleaned.
	add("", alias{unknown8bit, unknown8bitName, RepairFallback})

	// Windows code pages without their hyphen, or as "win": win-1251 and
	// win1251 are common in Cyrillic mail.
	windows := map[int]encoding.Encoding{
		874: charmap.Windows874, 1250: charmap.Windows1250, 1251: charmap.Windows1251,
		1252: charmap.Windows1252, 1253: charmap.Windows1253, 1254: charmap.Windows1254,
		1255: charmap.Windows1255, 1256: charmap.Windows1256, 1257: charmap.Windows1257,
		1258: charmap.Windows1258,
	}
	for n, enc := range windows {
		for _, form := range []string{"win-%d", "win%d"} {
			add(fmt.Sprintf(form, n), alias{enc, fmt.Sprintf("windows-%d", n), 0})
		}
	}

	// ISO 8859 parts spelled the way Java, glibc and hand-written headers
	// spell them. Parts 1 and 9 are widened to their Windows supersets and
	// part 11 is windows-874, exactly as their registered names are.
	iso := map[int]alias{
		1:  {latin1, "windows-1252", RepairSuperset},
		2:  {charmap.ISO8859_2, "iso-8859-2", 0},
		3:  {charmap.ISO8859_3, "iso-8859-3", 0},
		4:  {charmap.ISO8859_4, "iso-8859-4", 0},
		5:  {charmap.ISO8859_5, "iso-8859-5", 0},
		6:  {charmap.ISO8859_6, "iso-8859-6", 0},
		7:  {charmap.ISO8859_7, "iso-8859-7", 0},
		8:  {charmap.ISO8859_8, "iso-8859-8", 0},
		9:  {latin5, "windows-1254", RepairSuperset},
		10: {charmap.ISO8859_10, "iso-8859-10", 0},
		11: {charmap.Windows874, "windows-874", 0},
		13: {charmap.ISO8859_13, "iso-8859-13", 0},
		14: {charmap.ISO8859_14, "iso-8859-14", 0},
		15: {charmap.ISO8859_15, "iso-8859-15", 0},
		16: {charmap.ISO8859_16, "iso-8859-16", 0},
	}
	for n, a := range iso {
		for _, form := range []string{"iso8859-%d", "iso8859_%d", "iso_8859_%d", "8859-%d", "8859_%d"} {
			add(fmt.Sprintf(form, n), a)
		}
	}
	return m
}
