package charset_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	message "github.com/saeroun-ai/emlkit"
	_ "github.com/saeroun-ai/emlkit/charset"
	"golang.org/x/text/encoding/japanese"
)

// ROB-030 (issues #25, #26): the tolerant RFC 2047 decoder with the real
// decoders behind it. The Korean fixture is the fixed CP949 bytes of
// korean_alias_test.go; the Japanese one is produced by the x/text encoder,
// which is not the code under test.

func bWord(label string, b []byte) string {
	return "=?" + label + "?B?" + base64.StdEncoding.EncodeToString(b) + "?="
}

func qWord(label string, b []byte) string {
	var s strings.Builder
	for _, c := range b {
		fmt.Fprintf(&s, "=%02X", c)
	}
	return "=?" + label + "?Q?" + s.String() + "?="
}

func iso2022jp(t *testing.T, s string) []byte {
	t.Helper()
	b, err := japanese.ISO2022JP.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeHeaderText_charsets(t *testing.T) {
	const jpText = "日本語のテスト"
	jp := iso2022jp(t, jpText)
	kr := []byte(cp949Bytes)
	unknown := "=?x-no-such-charset?B?AAAA?="
	tests := []struct {
		name, in, want string
		diags          int
	}{
		{"rfc 2047 subject", "=?ISO-8859-1?B?SWYgeW91IGNhbiByZWFkIHRoaXMgeW8=?=\r\n =?ISO-8859-2?B?dSB1bmRlcnN0YW5kIHRoZSBleGFtcGxlLg==?=",
			"If you can read this you understand the example.", 0},
		{"rfc 2047 two charsets", "(=?ISO-8859-1?Q?a?= =?ISO-8859-2?Q?_b?=)", "(a b)", 0},
		{"windows-1252", "=?windows-1252?Q?caf=E9_=80?=", "café €", 0},
		{"gb2312", bWord("GB2312", []byte{178, 226, 202, 212}), "测试", 0},

		{"euc-kr", bWord("EUC-KR", kr), cp949Text, 0},
		{"euc-kr split inside a character", bWord("EUC-KR", kr[:3]) + " " + bWord("EUC-KR", kr[3:]), cp949Text, 0},
		{"euc-kr split a byte at a time", bWord("euc-kr", kr[:1]) + "\r\n " + bWord("EUC-KR", kr[1:2]) + "\r\n\t" + bWord("Euc-Kr", kr[2:]), cp949Text, 0},
		{"cp949 split inside the extended syllable", bWord("CP949", kr[:len(kr)-1]) + " " + qWord("cp949", kr[len(kr)-1:]), cp949Text, 0},
		{"ks_c_5601-1987 in Q, split", qWord("ks_c_5601-1987", kr[:5]) + " " + qWord("KS_C_5601-1987", kr[5:]), cp949Text, 0},
		{"euc-kr glued to text", "Re:" + bWord("EUC-KR", kr) + "!", "Re:" + cp949Text + "!", 1},
		{"euc-kr with a language", bWord("EUC-KR*ko", kr), cp949Text, 0},
		{"euc-kr without padding", "=?EUC-KR?B?vsiz58fPvLy/5CCMYw?=", cp949Text, 1},
		{"euc-kr folded inside the word", "=?EUC-KR?B?vsiz58fPvLy/\r\n 5CCMYw==?=", cp949Text, 1},
		{"euc-kr around an unknown word", bWord("EUC-KR", kr) + " " + unknown + " " + bWord("UTF-8", []byte(cp949Text)), cp949Text + " " + unknown + " " + cp949Text, 1},

		{"iso-2022-jp", bWord("ISO-2022-JP", jp), jpText, 0},
		{"iso-2022-jp split inside a character", bWord("ISO-2022-JP", jp[:4]) + " " + bWord("iso-2022-jp", jp[4:]), jpText, 0},
		{"iso-2022-jp split inside the escape", bWord("ISO-2022-JP", jp[:2]) + "\r\n " + bWord("ISO-2022-JP", jp[2:9]) + " " + bWord("ISO-2022-JP", jp[9:]), jpText, 0},
		{"iso-2022-jp in Q", qWord("iso-2022-jp", jp), jpText, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := message.DecodeHeaderText(tc.in)
			if got != tc.want || len(diags) != tc.diags {
				t.Errorf("DecodeHeaderText(%q)\n got %q, %v\nwant %q and %d diagnostics", tc.in, got, diags, tc.want, tc.diags)
			}
			var h message.Header
			h.Set("Subject", tc.in)
			if text, _ := h.Text("Subject"); text != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, text, tc.want)
			}
		})
	}
}

// TestHeaderText_fallbackCharset pins raw 8-bit header text, which is common in
// Korean mail: it is decoded with the configured fallback, and without one each
// byte that is not UTF-8 is replaced while the raw value stays in the header.
func TestHeaderText_fallbackCharset(t *testing.T) {
	var h message.Header
	h.Set("Subject", "Re: "+cp949Bytes+" "+bWord("UTF-8", []byte("끝")))

	for _, label := range []string{"euc-kr", "CP949", "ks_c_5601-1987"} {
		got, diags := h.TextWithOptions("Subject", &message.HeaderTextOptions{FallbackCharset: label})
		if got != "Re: "+cp949Text+" 끝" || len(diags) != 1 || diags[0].Code != message.CodeHeaderInvalidUTF8 ||
			diags[0].Severity != message.SeverityRecovered || diags[0].Field != "Subject" {
			t.Errorf("fallback %s: %q, %v", label, got, diags)
		}
	}

	got, diags := h.TextWithOptions("Subject", &message.HeaderTextOptions{FallbackCharset: "x-no-such-charset"})
	if !strings.HasSuffix(got, " 끝") || !strings.ContainsRune(got, utf8.RuneError) || len(diags) != 1 || diags[0].Severity != message.SeverityUnsupported || diags[0].Cause == nil {
		t.Errorf("unknown fallback: %q, %v", got, diags)
	}

	got, err := h.Text("Subject")
	if err != nil || !strings.HasPrefix(got, "Re: "+string(utf8.RuneError)) || !strings.HasSuffix(got, " 끝") || !strings.Contains(h.Get("Subject"), cp949Bytes) {
		t.Errorf("Text() = %q, %v with raw %q; want U+FFFD for the raw bytes and the raw value kept", got, err, h.Get("Subject"))
	}
}

// TestHeader_parameterWords pins that the encoded words of a parameter value
// heal the same way: a filename split inside a character is read whole, and a
// word in an unknown charset does not cost the words around it.
func TestHeader_parameterWords(t *testing.T) {
	kr := []byte(cp949Bytes)
	var h message.Header
	h.Set("Content-Disposition", `attachment; filename="`+bWord("EUC-KR", kr[:3])+" "+bWord("EUC-KR", kr[3:])+`.txt"; `+
		`note="`+bWord("ks_c_5601-1987", kr)+` =?x-no-such-charset?B?AAAA?="`)
	_, params, err := h.ContentDisposition()
	if err != nil || params["filename"] != cp949Text+".txt" || params["note"] != cp949Text+" =?x-no-such-charset?B?AAAA?=" {
		t.Errorf("ContentDisposition() = %q, %v", params, err)
	}

	text := "Content-Type: text/plain\r\nContent-Disposition: " + h.Get("Content-Disposition") + "\r\n\r\nbody"
	_, e, err := message.Parse(strings.NewReader(text), nil)
	if err != nil || e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	if _, params, err := e.EffectiveContentDisposition(); err != nil || params["filename"] != cp949Text+".txt" {
		t.Errorf("EffectiveContentDisposition() = %q, %v", params, err)
	}
}
