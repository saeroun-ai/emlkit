package message

import (
	"errors"
	"io"
	"mime"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Tests of the tolerant RFC 2047 decoder (ROB-030, rfc2047.go). Every input is
// a fixed string of at most about 1 MiB, and every scripted reader is capped.

// withWordCharsets installs a CharsetReader for the labels the tests in this
// file use, none of which needs the charset plugin:
//
//   - iso-8859-2 passes its input through, which is right for the ASCII the
//     RFC examples put in it;
//   - x-pair reads two hex digits as one byte and a digit left over as U+FFFD,
//     so it shows whether the bytes of adjacent words were joined before the
//     conversion;
//   - x-bomb never stops writing;
//   - x-fail returns a reader that fails;
//   - x-nil returns neither a reader nor an error;
//   - everything else is unknown.
//
// It returns the labels the hook was called with.
func withWordCharsets(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	withCharsetReader(t, func(cs string, in io.Reader) (io.Reader, error) {
		calls = append(calls, cs)
		switch cs {
		case "iso-8859-2":
			return in, nil
		case "x-pair":
			b, err := io.ReadAll(in)
			if err != nil {
				return nil, err
			}
			var out strings.Builder
			for ; len(b) >= 2; b = b[2:] {
				hi, ok1 := unhex(b[0])
				lo, ok2 := unhex(b[1])
				if !ok1 || !ok2 {
					out.WriteRune(utf8.RuneError)
					continue
				}
				out.WriteByte(hi<<4 | lo)
			}
			if len(b) > 0 {
				out.WriteRune(utf8.RuneError)
			}
			return strings.NewReader(out.String()), nil
		case "x-bomb":
			n := 0
			return readerFunc(func(p []byte) (int, error) {
				if n += len(p); n > 1<<20 {
					return 0, errCallCap
				}
				for i := range p {
					p[i] = 'A'
				}
				return len(p), nil
			}), nil
		case "x-fail":
			return readerFunc(func([]byte) (int, error) { return 0, errors.New("reader failed") }), nil
		case "x-nil":
			return nil, nil
		}
		return nil, errors.New("no such charset")
	})
	return &calls
}

const (
	hangul    = "한글"               // ED 95 9C EA B8 80
	hangulB64 = "7ZWc6riA"         // all of it
	hangulB1  = "7ZU="             // ED 95: the first two bytes of 한
	hangulB2  = "nOq4gA=="         // 9C EA B8 80: the rest
	helloB64  = "aGVsbG8="         // hello
	injectB64 = "YQpiAGM="         // a LF b NUL c
	replaced  = replacementChar    // U+FFFD, what an undecodable byte becomes
	unknownW  = "=?x-nope?Q?raw?=" // a word no charset decodes
)

func TestDecodeHeaderText(t *testing.T) {
	withWordCharsets(t)
	long := strings.Repeat("a", 200)
	tests := []struct {
		name  string
		in    string
		want  string
		codes []Code
	}{
		// ROB-030-001: conformant words, RFC 2047 section 8.
		{"plain", "hello world", "hello world", nil},
		{"empty", "", "", nil},
		{"rfc from", "=?US-ASCII?Q?Keith_Moore?= <moore@cs.utk.edu>", "Keith Moore <moore@cs.utk.edu>", nil},
		{"rfc to", "=?ISO-8859-1?Q?Keld_J=F8rn_Simonsen?= <keld@dkuug.dk>", "Keld Jørn Simonsen <keld@dkuug.dk>", nil},
		{"rfc cc", "=?ISO-8859-1?Q?Andr=E9?= Pirard <PIRARD@vm1.ulg.ac.be>", "André Pirard <PIRARD@vm1.ulg.ac.be>", nil},
		{"rfc subject", "=?ISO-8859-1?B?SWYgeW91IGNhbiByZWFkIHRoaXMgeW8=?=\r\n =?ISO-8859-2?B?dSB1bmRlcnN0YW5kIHRoZSBleGFtcGxlLg==?=",
			"If you can read this you understand the example.", nil},
		{"rfc comment", "(=?ISO-8859-1?Q?a?=)", "(a)", nil},
		{"rfc word then text", "(=?ISO-8859-1?Q?a?= b)", "(a b)", nil},
		{"rfc adjacent", "(=?ISO-8859-1?Q?a?= =?ISO-8859-1?Q?b?=)", "(ab)", nil},
		{"rfc adjacent, two spaces", "(=?ISO-8859-1?Q?a?=  =?ISO-8859-1?Q?b?=)", "(ab)", nil},
		{"rfc adjacent, folded", "(=?ISO-8859-1?Q?a?=\r\n    =?ISO-8859-1?Q?b?=)", "(ab)", nil},
		{"rfc underscore", "(=?ISO-8859-1?Q?a_b?=)", "(a b)", nil},
		{"rfc two charsets", "(=?ISO-8859-1?Q?a?= =?ISO-8859-2?Q?_b?=)", "(a b)", nil},
		{"text keeps its space", "a =?utf-8?Q?b?= c", "a b c", nil},
		{"utf-8 base64", "=?UTF-8?B?" + hangulB64 + "?=", hangul, nil},
		{"empty text", "x =?utf-8?Q??= y", "x  y", nil},
		{"lower-case b", "=?utf-8?b?" + hangulB64 + "?=", hangul, nil},
		{"lower-case q and hex", "=?utf-8?q?=ed=95=9c?=", "한", nil},
		{"language suffix", "=?utf-8*ko?B?" + hangulB64 + "?=", hangul, nil},
		{"language suffix, us-ascii", "=?US-ASCII*EN?Q?Keith_Moore?=", "Keith Moore", nil},
		{"utf8 label", "=?UTF8?B?" + hangulB64 + "?=", hangul, nil},
		{"longer than 75", "=?utf-8?Q?" + long + "?=", long, nil},
		{"space inside Q text", "=?utf-8?Q?Short subject?=", "Short subject", nil},

		// ROB-030-002: words where the grammar does not allow them.
		{"embedded", "abc=?utf-8?B?" + hangulB64 + "?=def", "abc" + hangul + "def", []Code{CodeRFC2047EmbeddedWord}},
		{"glued words", "=?utf-8?Q?a?==?utf-8?Q?b?=", "ab", []Code{CodeRFC2047EmbeddedWord, CodeRFC2047EmbeddedWord}},
		{"in a quoted string", `"=?utf-8?B?` + hangulB64 + `?=" <a@b.example>`, `"` + hangul + `" <a@b.example>`, []Code{CodeRFC2047IllegalLocation}},
		{"spaced in a quoted string", `" =?utf-8?Q?a?= =?utf-8?Q?b?= " <a@b.example>`, `" ab " <a@b.example>`,
			[]Code{CodeRFC2047IllegalLocation, CodeRFC2047IllegalLocation}},
		{"a lone quote is no quoted string", `5" =?utf-8?Q?a?=`, `5" a`, nil},
		{"folded inside B", "=?utf-8?B?7ZWc\r\n 6riA?=", hangul, []Code{CodeRFC2047InvalidBase64}},
		{"unfolded inside B", "=?utf-8?B?7ZWc 6riA?=", hangul, []Code{CodeRFC2047InvalidBase64}},
		{"folded inside Q", "=?utf-8?Q?ab\r\n cd?=", "ab cd", []Code{CodeRFC2047InvalidQ}},
		{"character split across B words", "=?utf-8?B?" + hangulB1 + "?= =?utf-8?B?" + hangulB2 + "?=", hangul, nil},
		{"character split across Q words", "=?utf-8?Q?=ED=95?=\r\n =?UTF-8?q?=9C?=", "한", nil},
		{"character split across B and Q", "=?utf-8?B?" + hangulB1 + "?= =?utf-8?Q?=9C?=", "한", nil},
		{"bytes joined before the hook", "=?x-pair?Q?414?= =?X-PAIR?Q?2?=", "AB", nil},
		{"not joined across text", "=?x-pair?Q?414?= - =?x-pair?Q?2?=", "A" + replaced + " - " + replaced, nil},
		{"no closing mark", "=?utf-8?B?" + hangulB64, hangul, []Code{CodeRFC2047InvalidWord}},
		{"no closing mark, then text", "=?utf-8?Q?abc def", "abc def", []Code{CodeRFC2047InvalidWord}},
		{"no closing mark, then a word", "=?utf-8?B?" + helloB64 + " =?utf-8?Q?_world?=", "hello world", []Code{CodeRFC2047InvalidWord}},
		{"quoted charset", `=?"utf-8"?Q?a?=`, "a", nil},
		{"empty charset", "=??Q?a?=", "a", []Code{CodeCharsetEmpty}},

		// ROB-030-003: what cannot be decoded stays, the rest is decoded.
		{"unknown charset", unknownW, unknownW, []Code{CodeRFC2047UnknownCharset}},
		{"unknown charset between words", "=?utf-8?Q?ok?= " + unknownW + " =?utf-8?Q?fine?=", "ok " + unknownW + " fine", []Code{CodeRFC2047UnknownCharset}},
		{"two unknown words stay as written", unknownW + "  " + unknownW + " x", unknownW + "  " + unknownW + " x", []Code{CodeRFC2047UnknownCharset}},
		{"unknown encoding", "a =?utf-8?X?zzz?= =?utf-8?Q?b?=", "a =?utf-8?X?zzz?= b", []Code{CodeRFC2047UnknownEncoding}},
		{"base64 without padding", "=?utf-8?B?aGVsbG8?=", "hello", []Code{CodeRFC2047InvalidBase64}},
		{"base64 with junk", "=?utf-8?B?aGVs!bG8=?=", "hello", []Code{CodeRFC2047InvalidBase64}},
		{"base64 with padding inside", "=?utf-8?B?aGU=bGxv?=", "hello", []Code{CodeRFC2047InvalidBase64}},
		{"base64 in the URL alphabet", "=?utf-8?B?Pz8_Pz4-?=", "????>>", []Code{CodeRFC2047InvalidBase64}},
		{"base64 beyond repair", "=?utf-8?Q?a?= =?utf-8?B?!!!?= =?utf-8?Q?b?=", "a =?utf-8?B?!!!?= b", []Code{CodeRFC2047InvalidBase64}},
		{"Q with bad hex", "=?utf-8?Q?a=ZZb=4?=", "a=ZZb=4", []Code{CodeRFC2047InvalidQ}},
		{"Q with a trailing equals", "=?utf-8?Q?abc=?=", "abc=", []Code{CodeRFC2047InvalidQ}},
		{"Q with a bare question mark", "=?utf-8?Q?why?_so?=", "why? so", []Code{CodeRFC2047InvalidQ}},
		{"Q with raw 8-bit", "=?iso-8859-1?Q?caf\xe9?=", "café", []Code{CodeRFC2047InvalidQ}},
		{"reader fails", "a =?x-fail?Q?b?= =?utf-8?Q?c?=", "a =?x-fail?Q?b?= c", []Code{CodeRFC2047InvalidWord}},
		{"hook returns nothing", "=?x-nil?Q?b?= c", "=?x-nil?Q?b?= c", []Code{CodeRFC2047UnknownCharset}},
		{"not a word", "=? =?= =?utf-8 =?utf-8? =?utf-8?Q ?= =?=?=?=? =?utf-8?Q?", "=? =?= =?utf-8 =?utf-8? =?utf-8?Q ?= =?=?=?=? =?utf-8?Q?", nil},

		// ROB-030-004: text outside encoded words.
		{"raw utf-8", hangul + " subject", hangul + " subject", []Code{CodeRFC2047RawUTF8}},
		{"raw utf-8 around a word", "é =?utf-8?Q?a?= é", "é a é", []Code{CodeRFC2047RawUTF8}},
		{"raw 8-bit", "caf\xe9 =?utf-8?Q?ok?= \xff\xfe", "caf" + replaced + " ok " + replaced + replaced, []Code{CodeHeaderInvalidUTF8, CodeHeaderInvalidUTF8}},
		{"us-ascii holding utf-8", "=?us-ascii?Q?=ED=95=9C?=", "한", []Code{CodeCharsetDeclarationSuspect}},
		{"us-ascii holding something else", "=?us-ascii?Q?caf=E9?=", "caf" + replaced, []Code{CodeCharsetDeclarationSuspect, CodeCharsetInvalidSequence}},
		{"unknown-8bit holding ascii", "=?unknown-8bit?Q?plain_text?=", "plain text", []Code{CodeRFC2047UnknownCharset}},
		{"unknown-8bit holding utf-8", "=?UNKNOWN-8BIT?B?" + hangulB64 + "?= =?x-unknown?Q?!?=", hangul + "!", []Code{CodeRFC2047UnknownCharset, CodeRFC2047UnknownCharset}},
		{"unknown-8bit holding something else", "a =?unknown-8bit?Q?caf=E9?= b", "a =?unknown-8bit?Q?caf=E9?= b", []Code{CodeRFC2047UnknownCharset}},
		{"utf-8 holding something else", "=?utf-8?Q?a=FFb?=", "a" + replaced + "b", []Code{CodeCharsetInvalidSequence}},

		// ROB-030-005: nothing decoded starts a new line.
		{"decoded CRLF", "=?utf-8?Q?a=0D=0AX-Evil:_1?=", "a X-Evil: 1", []Code{CodeRFC2047HeaderInjection}},
		{"decoded LF and NUL", "=?utf-8?B?" + injectB64 + "?=", "a b" + replaced + "c", []Code{CodeRFC2047HeaderInjection}},
		{"decoded bare CR", "=?iso-8859-1?Q?a=0Db?=", "a b", []Code{CodeRFC2047HeaderInjection}},
		{"CRLF split across words", "=?utf-8?Q?a=0D?= =?utf-8?Q?=0Ab?=", "a b", []Code{CodeRFC2047HeaderInjection}},

		// ROB-030-006: the hook cannot inflate a word.
		{"decode expansion", "a =?x-bomb?Q?abc?= b", "a =?x-bomb?Q?abc?= b", []Code{CodeRFC2047InvalidWord}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := DecodeHeaderText(tc.in)
			if got != tc.want {
				t.Errorf("DecodeHeaderText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if codes := codesOfDiags(diags); !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("DecodeHeaderText(%q) diagnostics = %v, want %v", tc.in, codes, tc.codes)
			}
			for _, d := range diags {
				if d.Action == "" || d.Message == "" || !d.Location.HasOffset ||
					d.Location.Offset < 0 || d.Location.EndOffset > int64(len(tc.in)) || d.Location.Offset >= d.Location.EndOffset {
					t.Errorf("diagnostic %+v: want an action, a message and a range inside the value", d)
				}
			}
			// Header.Text returns the same string.
			if text, _ := decodeHeader(tc.in); text != got {
				t.Errorf("decodeHeader(%q) = %q, want %q", tc.in, text, got)
			}
		})
	}
}

// TestDecodeHeaderText_diagnostics pins the fields of a diagnostic: where it
// points, how severe it is, and the cause it carries.
func TestDecodeHeaderText_diagnostics(t *testing.T) {
	withWordCharsets(t)
	in := "ok " + unknownW + " =?utf-8?B?!!!?= =?utf-8?Q?a=0A?= caf\xe9"
	got, diags := DecodeHeaderText(in)
	if want := "ok " + unknownW + " =?utf-8?B?!!!?= a  caf" + replaced; got != want {
		t.Fatalf("DecodeHeaderText() = %q, want %q", got, want)
	}
	type row struct {
		code     Code
		severity Severity
		text     string
		cause    bool
	}
	want := []row{
		{CodeRFC2047UnknownCharset, SeverityUnsupported, unknownW, true},
		{CodeRFC2047InvalidBase64, SeverityInvalidMetadata, "=?utf-8?B?!!!?=", false},
		{CodeRFC2047HeaderInjection, SeverityRecovered, "=?utf-8?Q?a=0A?=", false},
		{CodeHeaderInvalidUTF8, SeverityInvalidMetadata, "\xe9", false},
	}
	if len(diags) != len(want) {
		t.Fatalf("diagnostics = %v, want %d of them", codesOfDiags(diags), len(want))
	}
	for i, w := range want {
		d := diags[i]
		if d.Code != w.code || d.Severity != w.severity || in[d.Location.Offset:d.Location.EndOffset] != w.text || (d.Cause != nil) != w.cause {
			t.Errorf("diagnostic %d = %v at %q, want %v %v at %q", i, d, in[d.Location.Offset:d.Location.EndOffset], w.severity, w.code, w.text)
		}
	}
	if !IsUnknownCharset(diags[0].Cause) {
		t.Errorf("cause of the unknown charset = %v, want an UnknownCharsetError", diags[0].Cause)
	}
}

// TestHeader_Text pins the legacy accessors: the error-with-usable-result
// pattern, and the new ones naming their field.
func TestHeader_Text(t *testing.T) {
	withWordCharsets(t)
	var h Header
	h.Set("Subject", "=?utf-8?Q?ok?= "+unknownW)
	h.Set("X-Glued", "a=?utf-8?Q?b?=")
	h.Set("X-Raw", "caf\xe9")
	h.Set("X-Failing", "=?x-fail?Q?b?= =?utf-8?Q?c?=")

	got, err := h.Text("Subject")
	if got != "ok "+unknownW || !IsUnknownCharset(err) {
		t.Errorf("Text(Subject) = %q, %v; want the decoded rest and an unknown charset", got, err)
	}
	if got, err := h.Text("X-Glued"); got != "ab" || err != nil {
		t.Errorf("Text(X-Glued) = %q, %v", got, err)
	}
	if got, err := h.Text("X-Raw"); got != "caf"+replaced || err != nil || h.Get("X-Raw") != "caf\xe9" {
		t.Errorf("Text(X-Raw) = %q, %v with raw %q; want U+FFFD, no error and the raw value kept", got, err, h.Get("X-Raw"))
	}
	if got, err := h.Text("X-Failing"); got != "=?x-fail?Q?b?= c" || err == nil || IsUnknownCharset(err) {
		t.Errorf("Text(X-Failing) = %q, %v; want the reader's own error", got, err)
	}
	if got, err := h.Text("Missing"); got != "" || err != nil {
		t.Errorf("Text(Missing) = %q, %v", got, err)
	}

	got, diags := h.TextDiagnostics("Subject")
	if got != "ok "+unknownW || len(diags) != 1 || diags[0].Field != "Subject" || diags[0].Code != CodeRFC2047UnknownCharset {
		t.Errorf("TextDiagnostics(Subject) = %q, %v", got, diags)
	}
	got, diags = h.TextWithOptions("X-Raw", &HeaderTextOptions{FallbackCharset: "ISO-8859-1"})
	if got != "café" || len(diags) != 1 || diags[0].Field != "X-Raw" || diags[0].Code != CodeHeaderInvalidUTF8 || diags[0].Severity != SeverityRecovered {
		t.Errorf("TextWithOptions(X-Raw) = %q, %v", got, diags)
	}

	seen := map[string]string{}
	for fields := h.Fields(); fields.Next(); {
		text, diags := fields.TextDiagnostics()
		legacy, _ := fields.Text()
		if text != legacy {
			t.Errorf("%s: TextDiagnostics() = %q, Text() = %q", fields.Key(), text, legacy)
		}
		for _, d := range diags {
			if d.Field != fields.Key() {
				t.Errorf("%s: diagnostic names the field %q", fields.Key(), d.Field)
			}
		}
		seen[fields.Key()] = text
	}
	if seen["X-Glued"] != "ab" || len(seen) != 4 {
		t.Errorf("fields = %v", seen)
	}
}

// TestDecodeHeaderText_fallbackCharset pins ROB-030-004's configured fallback:
// only text that is not valid UTF-8 is converted, a run at a time.
func TestDecodeHeaderText_fallbackCharset(t *testing.T) {
	calls := withWordCharsets(t)
	tests := []struct {
		name, fallback, in, want string
		codes                    []Code
		severity                 Severity
	}{
		{"latin-1", "iso-8859-1", "caf\xe9 =?utf-8?Q?ok?= \xfcber", "café ok über", []Code{CodeHeaderInvalidUTF8, CodeHeaderInvalidUTF8}, SeverityRecovered},
		{"label case and quotes", ` "ISO-8859-1" `, "caf\xe9", "café", []Code{CodeHeaderInvalidUTF8}, SeverityRecovered},
		{"through the hook", "X-Pair", "4142\xff", "AB" + replaced, []Code{CodeHeaderInvalidUTF8}, SeverityRecovered},
		{"valid utf-8 is not converted", "iso-8859-1", "café", "café", []Code{CodeRFC2047RawUTF8}, SeverityInfo},
		{"unknown fallback", "x-nope", "caf\xe9", "caf" + replaced, []Code{CodeHeaderInvalidUTF8}, SeverityUnsupported},
		{"utf-8 is no fallback", "UTF-8", "caf\xe9", "caf" + replaced, []Code{CodeHeaderInvalidUTF8}, SeverityInvalidMetadata},
		{"none", "", "caf\xe9", "caf" + replaced, []Code{CodeHeaderInvalidUTF8}, SeverityInvalidMetadata},
		{"a word left verbatim is not converted", "iso-8859-1", "=?x-nope?Q?\xe9?=", "=?x-nope?Q?" + replaced + "?=", []Code{CodeRFC2047InvalidQ, CodeRFC2047UnknownCharset}, SeverityRecovered},
		{"a word labelled us-ascii", "iso-8859-1", "=?us-ascii?Q?caf=E9?=", "café", []Code{CodeCharsetDeclarationSuspect}, SeverityRecovered},
		{"a word labelled unknown-8bit", "iso-8859-1", "=?unknown-8bit?Q?caf=E9?= ok", "café ok", []Code{CodeRFC2047UnknownCharset}, SeverityRecovered},
		{"a word labelled utf-8 keeps its label", "iso-8859-1", "=?utf-8?Q?caf=E9?=", "caf" + replaced, []Code{CodeCharsetInvalidSequence}, SeverityRecovered},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			*calls = nil
			got, diags := DecodeHeaderTextWithOptions(tc.in, &HeaderTextOptions{FallbackCharset: tc.fallback})
			if got != tc.want || !reflect.DeepEqual(codesOfDiags(diags), tc.codes) || diags[0].Severity != tc.severity {
				t.Errorf("fallback %q: %q = %q, %v; want %q, %v", tc.fallback, tc.in, got, diags, tc.want, tc.codes)
			}
			if tc.name == "valid utf-8 is not converted" && len(*calls) != 0 {
				t.Errorf("the hook was asked for %v", *calls)
			}
		})
	}
}

// TestDecodeHeaderText_conformantUnchanged pins ROB-030-001's other half: a
// conformant value decodes to exactly what the standard library's decoder,
// which Header.Text used before, makes of it, with no error and no diagnostic.
func TestDecodeHeaderText_conformantUnchanged(t *testing.T) {
	withWordCharsets(t)
	std := mime.WordDecoder{CharsetReader: charsetReader}
	values := []string{
		"", "plain text", "a = b ? c", "Re: [list] =?UTF-8?B?" + hangulB64 + "?= (was: =?utf-8?q?old_topic?=)",
		"=?US-ASCII?Q?Keith_Moore?= <moore@cs.utk.edu>",
		"=?ISO-8859-1?Q?Keld_J=F8rn_Simonsen?= <keld@dkuug.dk>",
		"=?ISO-8859-1?Q?Andr=E9?= Pirard <PIRARD@vm1.ulg.ac.be>",
		"=?ISO-8859-1?B?SWYgeW91IGNhbiByZWFkIHRoaXMgeW8=?= =?ISO-8859-2?B?dSB1bmRlcnN0YW5kIHRoZSBleGFtcGxlLg==?=",
		"(=?ISO-8859-1?Q?a?=)", "(=?ISO-8859-1?Q?a?= b)", "(=?ISO-8859-1?Q?a?= =?ISO-8859-1?Q?b?=)",
		"(=?ISO-8859-1?Q?a?=  =?ISO-8859-1?Q?b?=)", "(=?ISO-8859-1?Q?a_b?=)", "(=?ISO-8859-1?Q?a?= =?ISO-8859-2?Q?_b?=)",
		"=?utf-8?Q?=ED=95?= =?utf-8?Q?=9C?= tail", " =?utf-8?Q?lead?= ", "=?utf-8?Q??=",
		encodeHeader(strings.Repeat(hangul+" 제목 ", 40)), encodeHeader("Plan de complémentarité de l'Homme"),
	}
	for _, in := range values {
		want, err := std.DecodeHeader(in)
		if err != nil {
			t.Fatalf("the standard library rejects %q: %v", in, err)
		}
		if got, err := decodeHeader(in); got != want || err != nil {
			t.Errorf("decodeHeader(%q) = %q, %v; the standard library has %q", in, got, err, want)
		}
		if got, diags := DecodeHeaderText(in); got != want || len(diags) != 0 {
			t.Errorf("DecodeHeaderText(%q) = %q, %v; the standard library has %q", in, got, codesOfDiags(diags), want)
		}
		if got := decodeParameterValue(in, charsetReader); got != want {
			t.Errorf("decodeParameterValue(%q) = %q; the standard library has %q", in, got, want)
		}
	}
}

// TestDecodeHeaderText_roundTrip pins that what SetText writes, Text reads.
func TestDecodeHeaderText_roundTrip(t *testing.T) {
	for _, s := range []string{
		"",
		"plain",
		"Plan de complémentarité de l'Homme",
		"tab\tand = equals ? question _ underscore",
		strings.Repeat(hangul+" 제목 ", 40),
		strings.Repeat("😀", 60),
		"mixed " + strings.Repeat("é", 100) + " tail",
	} {
		enc := encodeHeader(s)
		if got, err := decodeHeader(enc); got != s || err != nil {
			t.Errorf("decodeHeader(encodeHeader(%q)) = %q, %v", s, got, err)
		}
		if got, diags := DecodeHeaderText(enc); got != s || len(diags) != 0 {
			t.Errorf("DecodeHeaderText(encodeHeader(%q)) = %q, %v", s, got, codesOfDiags(diags))
		}
		var h Header
		h.SetText("Subject", s)
		if got, err := h.Text("Subject"); got != s || err != nil {
			t.Errorf("Text after SetText(%q) = %q, %v", s, got, err)
		}
	}
}

// TestDecodeParameterValue pins the parameter side: the same words are decoded
// the same way, and nothing outside them is touched.
func TestDecodeParameterValue(t *testing.T) {
	withWordCharsets(t)
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"\xff=?utf-8?Q?a?=\xfe\r\n\x00", "\xffa\xfe\r\n\x00"},
		{"=?utf-8?B?" + hangulB1 + "?= =?utf-8?B?" + hangulB2 + "?=", hangul},
		{"=?utf-8?Q?ok?= " + unknownW, "ok " + unknownW},
		{"=?utf-8?Q?a=0D=0Ab?=", "a b"},
	} {
		if got := decodeParameterValue(tc.in, charsetReader); got != tc.want {
			t.Errorf("decodeParameterValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
		var nilDecoder *metadataDecoder
		if got := nilDecoder.decode(tc.in); got != tc.want {
			t.Errorf("nil metadataDecoder.decode(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := (&metadataDecoder{}).decode(tc.in); got != tc.want {
			t.Errorf("metadataDecoder.decode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	var h Header
	h.Set("Content-Disposition", `attachment; filename="=?utf-8?B?`+hangulB1+`?= =?utf-8?B?`+hangulB2+`?=.txt"; note="`+unknownW+` =?utf-8?Q?kept?="`)
	_, params, err := h.ContentDisposition()
	if err != nil || params["filename"] != hangul+".txt" || params["note"] != unknownW+" kept" {
		t.Errorf("ContentDisposition() params = %q, %v", params, err)
	}
	e, err := Read(strings.NewReader("Content-Disposition: " + h.Get("Content-Disposition") + "\r\n\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func() (string, map[string]string, error){
		"Read":  e.EffectiveContentDisposition,
		"Parse": parsedDisposition(t, h.Get("Content-Disposition")),
	} {
		if _, params, err := read(); err != nil || params["filename"] != hangul+".txt" || params["note"] != unknownW+" kept" {
			t.Errorf("%s: EffectiveContentDisposition() params = %q, %v", name, params, err)
		}
	}
}

func parsedDisposition(t *testing.T, value string) func() (string, map[string]string, error) {
	t.Helper()
	_, e, err := Parse(strings.NewReader("Content-Disposition: "+value+"\r\n\r\nbody"), nil)
	if err != nil || e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	return e.EffectiveContentDisposition
}

// TestDecodeHeaderText_stalledReader pins that the text of a header gives up on
// a hook reader that returns nothing 100 times, leaves its word verbatim, and
// asks the hook for no other reader for that value.
func TestDecodeHeaderText_stalledReader(t *testing.T) {
	built := withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
	in := "=?x-meta?Q?abc?= mid =?x-meta?Q?def?= =?utf-8?Q?ok?="
	got, err := decodeHeader(in)
	if got != "=?x-meta?Q?abc?= mid =?x-meta?Q?def?= ok" || err != io.ErrNoProgress {
		t.Errorf("decodeHeader() = %q, %v; want both words verbatim and io.ErrNoProgress", got, err)
	}
	if len(*built) != 1 || (*built)[0].zeroCalls != maxConsecutiveEmptyReads {
		t.Errorf("%d hook readers, the first read %d times without progress; want 1 and 100", len(*built), (*built)[0].zeroCalls)
	}
	_, diags := DecodeHeaderText(in)
	if codes := codesOfDiags(diags); !reflect.DeepEqual(codes, []Code{CodeRFC2047InvalidWord, CodeRFC2047InvalidWord}) ||
		diags[0].Cause != io.ErrNoProgress || diags[0].Severity != SeverityInvalidMetadata {
		t.Errorf("diagnostics = %v", diags)
	}

	// A reader that is slow but moving is waited for.
	withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = maxConsecutiveEmptyReads - 1 })
	if got, err := decodeHeader("=?x-meta?Q?abc?="); got != "abc" || err != nil {
		t.Errorf("decodeHeader() after 99 empty reads = %q, %v", got, err)
	}
}

// TestDecodeHeaderText_wordLimit pins ROB-030-006's count: the words up to the
// limit are decoded, the rest of the value is left as it was written.
func TestDecodeHeaderText_wordLimit(t *testing.T) {
	const word = "=?utf-8?Q?a?= "
	in := strings.Repeat(word, maxEncodedWords+1)
	want := strings.Repeat("a", maxEncodedWords) + " " + word
	got, diags := DecodeHeaderText(in)
	if got != want {
		t.Errorf("DecodeHeaderText() = %d bytes ending %q, want %d ending %q", len(got), tail(got, 24), len(want), tail(want, 24))
	}
	if codes := codesOfDiags(diags); !reflect.DeepEqual(codes, []Code{CodeRFC2047WordLimit}) || diags[0].Severity != SeverityLimitExceeded {
		t.Errorf("diagnostics = %v", diags)
	}
	if text, err := decodeHeader(in); text != want || err != nil {
		t.Errorf("decodeHeader() = %d bytes, %v", len(text), err)
	}
	if got, diags := DecodeHeaderText(strings.Repeat(word, maxEncodedWords)); got != strings.Repeat("a", maxEncodedWords)+" " || len(diags) != 0 {
		t.Errorf("at the limit: %d bytes, %v", len(got), codesOfDiags(diags))
	}

	// Words left verbatim count too, and so do the diagnostics.
	bad := strings.Repeat("=?utf-8?X?a?= ", 200)
	got, diags = DecodeHeaderText(bad)
	if got != bad || len(diags) != maxHeaderTextDiagnostics || diags[len(diags)-1].Code != CodeLimitDiagnosticCount {
		t.Errorf("200 unknown encodings: changed %v, %d diagnostics ending in %v", got != bad, len(diags), diags[len(diags)-1].Code)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// hostileValues is a small fixed set of values that are wrong in every way a
// header value has been seen to be.
func hostileValues() []string {
	const sample = "a \"=?utf-8?Q?=ED=95?= =?UTF-8*ko?b?nOq4gA?=\" (=?x-nope?Q?r\xe9?=)=?iso-8859-1?q?=e9=0D=0A=00?= =?utf-8?X?z?= caf\xe9 =?utf-8?B?!!?= =?utf-8?Q?tail"
	values := []string{
		"=?", "=?=", "?=", "=?utf-8", "=?utf-8?", "=?utf-8?Q", "=?utf-8?Q?", "=?utf-8?Q?=", "=?utf-8?Q?=?", "=?utf-8?Q?=?=",
		"=?utf-8?B?=?=", "=?utf-8?B?====?=", "=?utf-8?B?A?=", "=?utf-8?B?A===A?=", "=?\xff?Q?a?=", "=?utf-8?\xff?a?=",
		"=?utf-8?Q?\xff\xfe?=", "\xff\xfe=?utf-8?Q?a?=\x80", "=?utf-8?Q?a?=\x00", "=?utf-16?B?AAA?=", "\"=?utf-8?Q?a?=",
		"\\\"=?utf-8?Q?a?=\"", "=?utf-8*?Q?a?=", "=?*?Q?a?=", "=?'\"'?Q?a?=", "=?utf-8?Q?a?==?utf-8?Q?", "=??Q??=", "=???=", "=????=",
		"=?utf-8?Q?=C3?=", "=?utf-8?Q?=C3?= =?utf-8?B?!?= =?utf-8?Q?=A9?=", "\x00\r\n\x00", "\xc3", "\xed\xa0\x80", "\xf4\x90\x80\x80",
		"=?" + strings.Repeat("a", 200) + "?Q?a?=", "=?x-bomb?Q?a?=", "=?x-fail?B?AAAA?=", "=?x-nil?B?AAAA?=", "=?x-pair?B?AAAA?=",
		strings.Repeat("=?", 500), strings.Repeat("?=", 500), strings.Repeat("=?x?B?", 300) + "?=", strings.Repeat("=?utf-8?Q?", 300) + "?=",
		strings.Repeat("\"", 301) + "=?utf-8?Q?a?=", strings.Repeat("=?utf-8?Q?a?=", 300), strings.Repeat("=?utf-8?Q?=ED?= ", 300),
	}
	for i := 0; i <= len(sample); i++ {
		values = append(values, sample[:i], sample[i:])
	}

	// A fixed pseudo-random walk over the bytes the scanner branches on, bare
	// and as the text of a word: 4000 values of at most 40 bytes, the same on
	// every run.
	const alphabet = "=?=?=?QBbqX-_ 8*'\"()\\\r\n\x00\xff\xed\x95\x9cutf!A/+"
	state := uint32(2047)
	next := func(n int) int {
		state = state*1664525 + 1013904223
		return int(state>>16) % n
	}
	for i := 0; i < 4000; i++ {
		b := make([]byte, next(41))
		for j := range b {
			b[j] = alphabet[next(len(alphabet))]
		}
		switch i % 4 {
		case 1:
			values = append(values, "=?utf-8?Q?"+string(b)+"?=")
		case 2:
			values = append(values, "a =?UTF-8?b?"+string(b)+"?= =?iso-8859-1?q?"+string(b))
		default:
			values = append(values, string(b))
		}
	}
	return values
}

// TestDecodeHeaderText_hostile pins what holds for every input: no panic, a
// result that is valid UTF-8 with no CR, LF or NUL that the raw text did not
// have, the same string from every text accessor, and a parameter decoding
// that keeps the bytes outside encoded words.
func TestDecodeHeaderText_hostile(t *testing.T) {
	withWordCharsets(t)
	for _, in := range hostileValues() {
		got, diags := DecodeHeaderText(in)
		if !utf8.ValidString(got) {
			t.Errorf("DecodeHeaderText(%q) = %q, not valid UTF-8", in, got)
		}
		for _, c := range []string{"\r", "\n", "\x00"} {
			if strings.Count(got, c) > strings.Count(in, c) {
				t.Errorf("DecodeHeaderText(%q) = %q has more %q than its input", in, got, c)
			}
		}
		if len(diags) > maxHeaderTextDiagnostics {
			t.Errorf("DecodeHeaderText(%q): %d diagnostics", in, len(diags))
		}
		for _, d := range diags {
			if d.Location.Offset < 0 || d.Location.EndOffset > int64(len(in)) || d.Location.Offset > d.Location.EndOffset {
				t.Errorf("DecodeHeaderText(%q): diagnostic %v outside the value", in, d)
			}
		}
		if text, _ := decodeHeader(in); text != got {
			t.Errorf("decodeHeader(%q) = %q, DecodeHeaderText = %q", in, text, got)
		}
		if again, _ := DecodeHeaderTextWithOptions(in, &HeaderTextOptions{FallbackCharset: "iso-8859-1"}); !utf8.ValidString(again) {
			t.Errorf("with a fallback, %q = %q, not valid UTF-8", in, again)
		}
		param := decodeParameterValue(in, charsetReader)
		if !strings.Contains(in, "=?") && param != in {
			t.Errorf("decodeParameterValue(%q) = %q, want it unchanged", in, param)
		}
	}
}

// TestDecodeHeaderText_linear pins ROB-030-006's work bound on the shapes that
// make a rescanning decoder quadratic. Each value is about 1 MiB; a quadratic
// pass over one would not finish inside the test deadline.
func TestDecodeHeaderText_linear(t *testing.T) {
	withWordCharsets(t)
	const size = 1 << 20
	fill := func(unit string) string { return strings.Repeat(unit, size/len(unit)) }
	shapes := map[string]string{
		"starts":                  fill("=?"),
		"starts with a label":     fill("=?a"),
		"ends":                    fill("?="),
		"open words, one close":   fill("=?x?Z?") + "?=",
		"open base64, one close":  fill("=?x?B?") + "?=",
		"open words, no close":    fill("=?utf-8?Q?"),
		"open words with text":    fill("=?utf-8?Q?ab "),
		"one endless word":        "=?utf-8?B?" + fill("A"),
		"one endless junk word":   "=?utf-8?B?" + fill("!") + "?=",
		"many words":              fill("=?utf-8?Q?a?= "),
		"many unknown words":      fill("=?x-nope?Q?a?= =?x-none?Q?a?= "),
		"many unknown encodings":  fill("=?utf-8?X?a?= "),
		"many quotes":             fill("\"") + "=?utf-8?Q?a?=",
		"nested starts in a word": "=?utf-8?Q?" + fill("=?") + "?=",
		"raw 8-bit":               fill("\xff") + "=?utf-8?Q?a?=",
	}
	for name, in := range shapes {
		start := time.Now()
		got, diags := DecodeHeaderText(in)
		if !utf8.ValidString(got) || len(got) > 4*len(in) || len(diags) > maxHeaderTextDiagnostics {
			t.Errorf("%s: %d bytes in, %d out, %d diagnostics", name, len(in), len(got), len(diags))
		}
		if param := decodeParameterValue(in, charsetReader); len(param) > 4*len(in) {
			t.Errorf("%s: parameter value of %d bytes from %d", name, len(param), len(in))
		}
		// A linear pass takes milliseconds; this is the margin of a loaded
		// host, not a benchmark.
		if took := time.Since(start); took > 20*time.Second {
			t.Fatalf("%s: %d bytes took %v, which is not linear work", name, len(in), took)
		}
	}
}
