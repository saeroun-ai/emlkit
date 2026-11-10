package charset

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// ROB-080-001 and ROB-080-005: the label registry and the charsets behind it.
//
// Every fixture is fixed bytes, not bytes produced by the decoder under test:
// each is the text beside it as the named Python codec encodes it, except
// where a comment says how it was put together by hand.

// A family is one decoder: the name Lookup reports for it, a fixture, and
// every label that must reach it.
type family struct {
	name   string
	bytes  string
	text   string
	labels string
}

const (
	// "Hi 한글 😀" in UTF-16 and UTF-32, both byte orders, without a mark.
	utf16LE = "H\x00i\x00 \x00\x5c\xd5\x00\xae \x00=\xd8\x00\xde"
	utf16BE = "\x00H\x00i\x00 \xd5\x5c\xae\x00\x00 \xd8=\xde\x00"
	utf32LE = "H\x00\x00\x00i\x00\x00\x00 \x00\x00\x00\x5c\xd5\x00\x00\x00\xae\x00\x00 \x00\x00\x00\x00\xf6\x01\x00"
	utf32BE = "\x00\x00\x00H\x00\x00\x00i\x00\x00\x00 \x00\x00\xd5\x5c\x00\x00\xae\x00\x00\x00\x00 \x00\x01\xf6\x00"
	hiText  = "Hi 한글 😀"

	polish = "Zażółć gęślą jaźń"
	baltic = "ąčęėįšųūž"
)

var families = []family{
	// Unicode.
	{"utf-8", "Hi \xed\x95\x9c\xea\xb8\x80 \xf0\x9f\x98\x80", hiText,
		"utf-8 utf8 csutf8 unicode-1-1-utf-8 unicode11utf8 unicode20utf8 x-unicode20utf8 " +
			"utf_8 x-utf-8 x-utf8 x-utf_8j utf8mb3 utf8mb4 unicode-2-0-utf-8"},
	{"utf-7", "Hi +1VyuAA +2D3eAA a+-b", hiText + " a+b",
		"utf-7 csutf7 unicode-1-1-utf-7 csunicode11utf7 utf7 x-utf-7 unicode-2-0-utf-7 x-unicode-2-0-utf-7"},
	{"utf-16", "\xff\xfe" + utf16LE, hiText,
		"utf-16 utf16 csutf16 unicode ucs-2 ucs2 iso-10646-ucs-2 csunicode unicodefeff"},
	{"utf-16le", utf16LE, hiText, "utf-16le utf16le csutf16le utf-16-le ucs-2le x-utf-16le"},
	{"utf-16be", utf16BE, hiText, "utf-16be utf16be csutf16be utf-16-be ucs-2be x-utf-16be unicodefffe"},
	{"utf-32", "\x00\x00\xfe\xff" + utf32BE, hiText, "utf-32 csutf32 utf32 ucs-4 ucs4 iso-10646-ucs-4 csucs4"},
	{"utf-32be", utf32BE, hiText, "utf-32be csutf32be"},
	{"utf-32le", utf32LE, hiText, "utf-32le csutf32le"},

	// Korean. 똠 (0x8C 0x63) is outside KS X 1001: only code page 949 has it.
	{"euc-kr", "\xbe\xc8\xb3\xe7\xc7\xcf\xbc\xbc\xbf\xe4 \x8cc", "안녕하세요 똠",
		"euc-kr euckr cseuckr ks_c_5601-1987 ks_c_5601-1989 ksc5601 ksc_5601 korean iso-ir-149 windows-949 " +
			"cp949 x-windows-949 ms949 uhc x-uhc euc_kr x-euc-kr " +
			"ks_c_5601 ks_c_5601_1987 ksc5601-1987 ksc5601_1987 ks-c-5601-1987 ks-c-5601"},
	{"iso-2022-kr", "\x1b$)C\x0eGQ1[\x0f test \x0e>H3g\x0f", "한글 test 안녕",
		"iso-2022-kr csiso2022kr iso2022kr iso2022-kr"},

	// Japanese. ① (0x87 0x40) and 髙 (0xEE 0xE0) are NEC and IBM extensions of
	// code page 932, outside JIS X 0208.
	{"shift_jis", "\x93\xfa\x96{\x8c\xea\x82\xcc\x83\x81\x81[\x83\x8b \xc3\xbd\xc4 \x87@\xee\xe0", "日本語のメール ﾃｽﾄ ①髙",
		"shift_jis shift-jis sjis x-sjis ms_kanji csshiftjis shiftjis windows-31j ms932 " +
			"cp932 x-ms-cp932 windows-932 x-ms932 sjis-win s-jis x-shift-jis x-shift_jis cswindows31j"},
	// 丂 is JIS X 0212, three bytes after 0x8F.
	{"euc-jp", "\xc6\xfc\xcb\xdc\xb8\xec\xa4\xce\xa5\xe1\xa1\xbc\xa5\xeb \x8e\xc3\x8e\xbd\x8e\xc4 \x8f\xb0\xa1", "日本語のメール ﾃｽﾄ 丂",
		"euc-jp x-euc-jp cseucpkdfmtjapanese eucjp euc_jp x-eucjp ujis eucjp-win eucjp-ms cp51932"},
	// Python's iso2022_jp, iso2022_jp_ext (katakana by ESC ( I) and
	// iso2022_jp_1 (JIS X 0212 by ESC $ ( D), joined; the last run is ① by
	// hand, row 13 cell 1 of the code page 5022x table: 0x2D 0x21.
	{"iso-2022-jp", "\x1b$BF|K\x5c8l$N%a!<%k$G$9!#\x1b(Babc \x1b(IC=D\x1b(B \x1b$(D0!\x1b(B \x1b$B-!\x1b(B", "日本語のメールです。abc ﾃｽﾄ 丂 ①",
		"iso-2022-jp csiso2022jp iso2022jp iso2022-jp jis iso-2022-jp-1 iso-2022-jp-ms iso-2022-jp-cp932 x-windows-iso2022jp " +
			"cp50220 cp50221 cp50222 windows-50220 windows-50221 x-windows-50220 x-windows-50221"},

	// Chinese. 镕 (0xE9 0x46) is outside GB 2312: the gb2312 labels reach GBK.
	{"gbk", "\xb2\xe2\xca\xd4 \xd6\xd0\xce\xc4 \xe9F", "测试 中文 镕",
		"gbk x-gbk cp936 ms936 windows-936 gb2312 gb_2312-80 gb_2312 csgb2312 chinese iso-ir-58 csiso58gb231280 " +
			"gb2312-80 gb-2312 euc-cn euccn x-euc-cn x-windows-936"},
	{"gb18030", "\xb2\xe2\xca\xd4 \x952\x826 \xa2\xe3 \x815\xf47", "测试 𠀀 € ḿ",
		"gb18030 gb-18030 gb18030-2000 gb18030-2005 cp54936 windows-54936"},
	{"hz-gb-2312", "ab ~{2bJT~} cd ~~ ~{VPND~}", "ab 测试 cd ~ 中文", "hz-gb-2312 hz hz-gb2312"},
	// 碁 (0xF9 0xD6) is an ETEN extension, in code page 950.
	{"big5", "\xb4\xfa\xb8\xd5 \xa4\xa4\xa4\xe5 \xf9\xd6", "測試 中文 碁",
		"big5 csbig5 cn-big5 x-x-big5 big5-hkscs x-big5 big-5 cp950 ms950 windows-950 x-windows-950 big5-eten big5hkscs"},

	// Western and Central European. The iso-8859-1 labels reach windows-1252:
	// the fixture has 0x80, 0x93, 0x94 and 0x9E, which are C1 controls in
	// ISO 8859-1.
	{"windows-1252", "caf\xe9 \x80\x93quotes\x94 \x9e", "café €“quotes” ž",
		"windows-1252 cp1252 x-cp1252 windows1252 win-1252 win1252 " +
			"iso-8859-1 iso_8859-1 iso_8859-1:1987 iso-ir-100 latin1 l1 ibm819 cp819 csisolatin1 iso88591 " +
			"latin-1 latin_1 iso-latin-1 isolatin1 iso8859-1 iso8859_1 iso_8859_1 8859-1 8859_1"},
	{"iso-8859-1", "caf\xe9 \x85", "café \u0085", "ansi_x3.110-1983"},
	{"windows-1250", "Za\xbf\xf3\xb3\xe6 g\xea\x9cl\xb9 ja\x9f\xf1", polish, "windows-1250 cp1250 x-cp1250 windows1250 win-1250 win1250"},
	{"iso-8859-2", "Za\xbf\xf3\xb3\xe6 g\xea\xb6l\xb1 ja\xbc\xf1", polish,
		"iso-8859-2 iso_8859-2 latin2 l2 iso88592 iso8859-2 iso8859_2 iso_8859_2 8859-2 8859_2"},
	{"iso-8859-3", "\xe6\xf8\xb6\xbc\xfe\xfd", "ĉĝĥĵŝŭ", "iso-8859-3 latin3 iso8859-3 iso8859_3 iso_8859_3 8859-3 8859_3"},
	{"iso-8859-4", "\xb1\xe8\xea\xec\xe7\xb9\xf9\xfe\xbe", baltic, "iso-8859-4 latin4 iso8859-4 iso8859_4 iso_8859_4 8859-4 8859_4"},
	{"iso-8859-10", "\xb1\xe8\xea\xec\xe7\xba\xf9\xbe\xbc", baltic, "iso-8859-10 latin6 iso8859-10 iso8859_10 iso_8859_10 8859-10 8859_10"},
	{"iso-8859-13", "\xe0\xe8\xe6\xeb\xe1\xf0\xf8\xfb\xfe", baltic, "iso-8859-13 latin7 iso8859-13 iso8859_13 iso_8859_13 8859-13 8859_13"},
	{"iso-8859-14", "\xf0\xfe \xa2\xa5", "ŵŷ ḃċ", "iso-8859-14 latin8 iso8859-14 iso8859_14 iso_8859_14 8859-14 8859_14"},
	{"iso-8859-15", "caf\xe9 \xa4 \xbc", "café € Œ",
		"iso-8859-15 iso_8859-15 latin-9 latin9 latin0 iso8859-15 iso8859_15 iso_8859_15 8859-15 8859_15"},
	{"iso-8859-16", "\xba\xfe \xa4 \xbd", "șț € œ", "iso-8859-16 latin10 iso8859-16 iso8859_16 iso_8859_16 8859-16 8859_16"},
	{"windows-1257", "\xe0\xe8\xe6\xeb\xe1\xf0\xf8\xfb\xfe", baltic, "windows-1257 cp1257 win-1257 win1257"},
	{"macintosh", "caf\x8e \xaa \xb9", "café ™ π", "macintosh mac csmacintosh x-mac-roman macroman mac-roman x-macroman x-macintosh"},
	{"ibm437", "\x82 \xba \xa4", "é ║ ñ", "ibm437 cp437 437"},
	{"ibm850", "\x82 \xba \xa4 \xb8", "é ║ ñ ©", "ibm850 cp850 850"},

	// Cyrillic. і and ґ tell KOI8-U from KOI8-R.
	{"koi8-r", "\xf0\xd2\xc9\xd7\xc5\xd4, \xcd\xc9\xd2", "Привет, мир", "koi8-r koi8r koi8_r koi8 koi cskoi8r koi-8-r koi-8 x-koi8-r"},
	{"koi8-u", "\xf0\xd2\xc9\xd7\xa6\xd4, \xd3\xd7\xa6\xd4 \xad", "Привіт, світ ґ", "koi8-u koi8u koi8-ru koi8_u koi-8-u x-koi8-u"},
	{"windows-1251", "\xcf\xf0\xe8\xe2\xe5\xf2, \xec\xe8\xf0 \xb9", "Привет, мир №", "windows-1251 cp1251 x-cp1251 windows1251 win-1251 win1251"},
	{"iso-8859-5", "\xbf\xe0\xd8\xd2\xd5\xe2, \xdc\xd8\xe0", "Привет, мир",
		"iso-8859-5 iso_8859-5 cyrillic iso8859-5 iso8859_5 iso_8859_5 8859-5 8859_5"},
	{"ibm866", "\x8f\xe0\xa8\xa2\xa5\xe2, \xac\xa8\xe0", "Привет, мир", "ibm866 cp866 866 csibm866 dos-866 x-cp866 x-ibm866"},
	{"x-mac-cyrillic", "\x8f\xf0\xe8\xe2\xe5\xf2, \xec\xe8\xf0", "Привет, мир", "x-mac-cyrillic x-mac-ukrainian maccyrillic mac-cyrillic"},

	// Greek, Hebrew, Arabic, Turkish, Thai, Vietnamese. The iso-8859-9 labels
	// reach windows-1254, and the Thai ones windows-1254's sibling
	// windows-874: both fixtures have the euro sign at 0x80.
	{"iso-8859-7", "\xca\xe1\xeb\xe7\xec\xdd\xf1\xe1", "Καλημέρα",
		"iso-8859-7 greek greek8 elot_928 ecma-118 iso8859-7 iso8859_7 iso_8859_7 8859-7 8859_7"},
	{"windows-1253", "\xca\xe1\xeb\xe7\xec\xdd\xf1\xe1", "Καλημέρα", "windows-1253 cp1253 win-1253 win1253"},
	{"iso-8859-8", "\xf9\xec\xe5\xed", "שלום", "iso-8859-8 hebrew visual iso8859-8 iso8859_8 iso_8859_8 8859-8 8859_8"},
	{"iso-8859-8-i", "\xf9\xec\xe5\xed", "שלום", "iso-8859-8-i logical"},
	{"windows-1255", "\xf9\xec\xe5\xed", "שלום", "windows-1255 cp1255 win-1255 win1255"},
	{"iso-8859-6", "\xe5\xd1\xcd\xc8\xc7", "مرحبا", "iso-8859-6 arabic asmo-708 ecma-114 iso8859-6 iso8859_6 iso_8859_6 8859-6 8859_6"},
	{"windows-1256", "\xe3\xd1\xcd\xc8\xc7", "مرحبا", "windows-1256 cp1256 win-1256 win1256"},
	{"windows-1254", "\xddstanbul \xf0\xfc\xfe\xf6\xe7 \x80", "İstanbul ğüşöç €",
		"windows-1254 cp1254 x-cp1254 win-1254 win1254 " +
			"iso-8859-9 iso_8859-9 latin5 l5 csisolatin5 iso-ir-148 iso88599 iso8859-9 iso8859_9 iso_8859_9 8859-9 8859_9"},
	{"windows-874", "\xca\xc7\xd1\xca\xb4\xd5 \x80", "สวัสดี €",
		"windows-874 dos-874 tis-620 iso-8859-11 iso885911 win-874 win874 " +
			"tis620 cp874 ms874 x-windows-874 iso8859-11 iso8859_11 iso_8859_11 8859-11 8859_11"},
	{"windows-1258", "Viet \xf0 \xf5 \xfd", "Viet đ ơ ư", "windows-1258 cp1258 win-1258 win1258"},

	// No charset, and us-ascii: UTF-8 is kept, any other byte above 0x7F is
	// windows-1252. The fixture is "café" in UTF-8, "café" and the quotes in
	// windows-1252, and the euro sign in UTF-8.
	{"unknown-8bit", "caf\xc3\xa9 caf\xe9 \x93x\x94 \xe2\x82\xac", "café café “x” €",
		"unknown-8bit csunknown8bit x-unknown unknown default x-default x-user-defined none null undefined binary 8bit " +
			"us-ascii ascii ansi_x3.4-1968 iso646-us us csascii ibm367 cp367 iso-ir-6 646 usascii us_ascii"},
}

// decodeAll decodes in under label twice, in one piece and a byte at a time,
// and fails the test if the two disagree, a read fails, or the output is not
// valid UTF-8: no decoder may depend on how its input is cut.
func decodeAll(t *testing.T, label, in string) string {
	t.Helper()
	var outs [2]string
	for i, src := range []io.Reader{strings.NewReader(in), iotest.OneByteReader(strings.NewReader(in))} {
		r, err := Reader(label, src)
		if err != nil {
			t.Fatalf("Reader(%q) = %v", label, err)
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("Reader(%q) on %q: read error %v after %q", label, in, err, b)
		}
		outs[i] = string(b)
	}
	if outs[0] != outs[1] {
		t.Fatalf("Reader(%q) on %q: %q in one piece, %q a byte at a time", label, in, outs[0], outs[1])
	}
	if !utf8.ValidString(outs[0]) {
		t.Fatalf("Reader(%q) on %q: output %q is not valid UTF-8", label, in, outs[0])
	}
	return outs[0]
}

func TestLookup_everyLabelDecodesItsFixture(t *testing.T) {
	seen := make(map[string]bool)
	for _, f := range families {
		for _, label := range strings.Fields(f.labels) {
			if seen[label] {
				t.Errorf("label %q is listed twice", label)
			}
			seen[label] = true
			for _, l := range []string{label, strings.ToUpper(label)} {
				m, err := Lookup(l)
				if err != nil {
					t.Errorf("Lookup(%q) = %v, want %s", l, err, f.name)
					continue
				}
				if m.Name != f.name || m.Encoding == nil {
					t.Errorf("Lookup(%q) = %q, want %q", l, m.Name, f.name)
				}
				if got := decodeAll(t, l, f.bytes); got != f.text {
					t.Errorf("Reader(%q) decoded %q to %q, want %q", l, f.bytes, got, f.text)
				}
			}
		}
	}

	// The empty label, which strings.Fields cannot list.
	if got := decodeAll(t, "", "caf\xe9 \xe2\x82\xac"); got != "café €" {
		t.Errorf(`Reader("") decoded to %q, want "café €"`, got)
	}
	seen[""] = true

	// No alias may be without a fixture.
	for label := range aliases {
		if !seen[label] {
			t.Errorf("alias %q is in no family of this test", label)
		}
	}
	t.Logf("%d labels in %d families, %d of them from the alias table", len(seen), len(families), len(aliases))
}

func TestLookup_repairs(t *testing.T) {
	for _, c := range []struct {
		label string
		name  string
		want  Repair
	}{
		{"utf-8", "utf-8", 0},
		{"UTF-8", "utf-8", 0},
		{"euc-kr", "euc-kr", 0},
		{"ks_c_5601-1987", "euc-kr", 0},
		{"iso-2022-jp", "iso-2022-jp", 0},
		{"utf-7", "utf-7", 0},
		{"utf-16", "utf-16", 0},
		{"cp949", "euc-kr", RepairAlias},
		{"x-sjis", "shift_jis", RepairAlias},
		{"utf8", "utf-8", RepairAlias},
		{"win-1251", "windows-1251", RepairAlias},
		{"iso-8859-1", "windows-1252", RepairSuperset},
		{"latin1", "windows-1252", RepairSuperset},
		{"iso-8859-9", "windows-1254", RepairSuperset},
		{"us-ascii", "unknown-8bit", RepairSuperset},
		{"latin-1", "windows-1252", RepairAlias | RepairSuperset},
		{"iso8859_9", "windows-1254", RepairAlias | RepairSuperset},
		{"unknown-8bit", "unknown-8bit", RepairFallback},
		{"x-unknown", "unknown-8bit", RepairAlias | RepairFallback},
		{"", "unknown-8bit", RepairAlias | RepairFallback},
		{`"utf-8"`, "utf-8", RepairLabel},
		{"charset=cp949;", "euc-kr", RepairLabel | RepairAlias},
		{` "ISO-8859-1" `, "windows-1252", RepairLabel | RepairSuperset},
		{`""`, "unknown-8bit", RepairLabel | RepairAlias | RepairFallback},
	} {
		m, err := Lookup(c.label)
		if err != nil || m.Name != c.name || m.Repairs != c.want {
			t.Errorf("Lookup(%q) = (%q, repairs %04b, %v), want (%q, repairs %04b)", c.label, m.Name, m.Repairs, err, c.name, c.want)
		}
	}
}

// TestLookup_decoratedLabels: what mail puts around a charset name is removed
// when the label matches nothing as it stands.
func TestLookup_decoratedLabels(t *testing.T) {
	for label, want := range map[string]string{
		`"utf-8"`:                      "utf-8",
		`'utf-8'`:                      "utf-8",
		"  utf-8\t":                    "utf-8",
		"utf-8;":                       "utf-8",
		"utf-8; format=flowed":         "utf-8",
		"charset=utf-8":                "utf-8",
		`charset="utf-8"`:              "utf-8",
		"charset = euc-kr":             "euc-kr",
		"Charset: EUC-KR":              "euc-kr",
		"charset=charset=gbk":          "gbk",
		`"ks_c_5601-1987"`:             "euc-kr",
		"euc-kr\r\n":                   "euc-kr",
		"=utf-8":                       "utf-8",
		"utf-8*en":                     "utf-8",
		"us-ascii'en'":                 "unknown-8bit",
		`\"shift_jis\"`:                "shift_jis",
		"<iso-2022-jp>":                "iso-2022-jp",
		`"windows-1252"; name="a.txt"`: "windows-1252",
		"utf-8\x00":                    "utf-8",
		"big5 (traditional chinese)":   "big5",
		"\xc2\xa0koi8-r\xc2\xa0":       "koi8-r",
		`"'cp949'"`:                    "euc-kr",
	} {
		m, err := Lookup(label)
		if err != nil || m.Name != want || m.Repairs&RepairLabel == 0 {
			t.Errorf("Lookup(%q) = (%q, repairs %04b, %v), want %q with RepairLabel", label, m.Name, m.Repairs, err, want)
		}
	}

	// A name that matches as it stands is never cleaned, and cleaning does
	// not turn a near miss into a match.
	if m, err := Lookup("iso_8859-1:1987"); err != nil || m.Repairs&RepairLabel != 0 {
		t.Errorf(`Lookup("iso_8859-1:1987") = (%+v, %v), want a match without RepairLabel`, m, err)
	}
	for _, label := range []string{`"cp-949"`, "charset=x-cp949", "x utf-8", "charset", "utf-8x;"} {
		if m, err := Lookup(label); err == nil {
			t.Errorf("Lookup(%q) = %q, want an unknown charset", label, m.Name)
		}
	}
}

func TestRegisterEncoding(t *testing.T) {
	t.Cleanup(func() {
		delete(charsets, "x-emlkit-test")
		delete(charsets, "cp949")
	})

	RegisterEncoding("X-Emlkit-Test", charmap.KOI8R)
	if got := decodeAll(t, "x-EMLKIT-test", "\xf0\xd2\xc9\xd7\xc5\xd4"); got != "Привет" {
		t.Errorf("registered encoding decoded to %q", got)
	}
	if m, err := Lookup("X-Emlkit-Test"); err != nil || m.Name != "x-emlkit-test" || m.Repairs != 0 {
		t.Errorf("Lookup of a registered encoding = (%+v, %v)", m, err)
	}

	// A registered encoding wins over the alias table, and nil disables.
	RegisterEncoding("cp949", nil)
	if _, err := Reader("CP949", strings.NewReader("x")); err == nil {
		t.Error("a charset disabled with RegisterEncoding still decodes")
	}
}
