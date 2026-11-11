package charset_test

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/charset"
)

// cdParseBody parses one text part and returns its body and the diagnostics
// of the parse as "severity code".
func cdParseBody(t *testing.T, r io.Reader, opts *message.ParseOptions) (string, []string) {
	t.Helper()
	s, e, err := message.Parse(r, opts)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	b, rerr := io.ReadAll(e.Body)
	if rerr != nil {
		t.Fatalf("ReadAll() = %v", rerr)
	}
	var diags []string
	for _, d := range s.Diagnostics() {
		diags = append(diags, fmt.Sprintf("%s %s", d.Severity, d.Code))
	}
	return string(b), diags
}

// TestParse_utf8UnderLegacyDeclaration pins the evidence rule of
// charset_bom.go (cdDecideUTF8): a text body that declares a legacy charset
// is read as UTF-8, and reported as charset.declaration_suspect, when its
// sniff window holds no ill-formed byte and at least two multi-byte
// sequences, one of them three bytes long or, failing that, the declared
// decoder itself reading the bytes as not its own. Everything else is decoded
// as declared, silently; so is everything under ProfileStrict and Read.
func TestParse_utf8UnderLegacyDeclaration(t *testing.T) {
	const (
		ko      = "안녕하세요, world"
		eucKR   = "\xbe\xc8\xb3\xe7\xc7\xcf\xbc\xbc\xbf\xe4"
		wreath  = "\xc8\xad\xc8\xaf" // 화환 in EUC-KR, and "ȭȯ" in UTF-8
		cp1251  = "\xcf\xf0\xe8\xe2\xe5\xf2"
		suspect = "recovered charset.declaration_suspect"
	)
	for _, tc := range []struct {
		name, label, body string
		healed            bool
	}{
		{"korean under euc-kr", "euc-kr", ko, true},
		{"korean under ks_c_5601-1987", "ks_c_5601-1987", ko, true},
		{"korean under iso-2022-kr", "iso-2022-kr", ko, true},
		{"japanese under shift_jis", "Shift_JIS", "こんにちは", true},
		{"japanese under iso-2022-jp", "iso-2022-jp", "こんにちは", true},
		{"chinese under gb2312", "gb2312", "你好，世界", true},
		{"chinese under big5", "big5", "你好，世界", true},
		{"punctuation under windows-1252", "windows-1252", "“it’s”", true},
		{"latin under iso-8859-1", "iso-8859-1", "café déjà vu", true},
		{"cyrillic under windows-1251", "windows-1251", "Привет, мир", true},
		{"cyrillic under euc-kr", "euc-kr", "Привет", true},
		{"window ends inside a character", "euc-kr", strings.Repeat("a", 4089) + "가나다", true},

		{"real euc-kr", "euc-kr", eucKR, false},
		{"real euc-kr that is also utf-8", "euc-kr", wreath, false},
		// Known limits of rule 3, pinned so that a change to them is seen:
		// two-byte UTF-8 that a double-byte decoder reads as clean pairs is
		// kept as declared, and half-width katakana that pair up as UTF-8 are
		// taken for a single-byte reading.
		{"latin that euc-kr reads as pairs", "euc-kr", "café déjà", false},
		{"half-width katakana pairs", "shift_jis", "\xc3\xbd\xc3\xbd", true},
		{"real windows-1251", "windows-1251", cp1251, false},
		{"one sequence", "windows-1252", "it’s", false},
		{"one sequence, korean", "euc-kr", "가", false},
		{"an ill-formed byte", "euc-kr", ko + "\xb0", false},
		{"cut at the end of the body", "euc-kr", ko + "\xec\x95", false},
		{"ascii", "euc-kr", "hello", false},
		{"nothing in the window", "euc-kr", strings.Repeat("a", 4096) + ko, false},
		{"shifts under iso-2022-jp", "iso-2022-jp", "\x1b$B$3$s\x1b(B こんにちは", false},
		{"utf-7 keeps its runs", "utf-7", "안녕 +wUiwFQ-", false},
	} {
		input := "Content-Type: text/plain; charset=" + tc.label + "\r\n\r\n" + tc.body
		declared, err := charset.Reader(tc.label, strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		asDeclared, _ := io.ReadAll(declared)
		want, wantDiags := string(asDeclared), []string(nil)
		if tc.healed {
			want, wantDiags = tc.body, []string{suspect}
		}
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"whole":    func(r io.Reader) io.Reader { return r },
			"one byte": iotest.OneByteReader,
		} {
			got, diags := cdParseBody(t, wrap(strings.NewReader(input)), nil)
			if got != want || !reflect.DeepEqual(diags, wantDiags) {
				t.Errorf("%s/%s: body %q %v, want %q %v", tc.name, name, got, diags, want, wantDiags)
			}
		}
		// ProfileStrict and Read decode as declared and say nothing.
		if got, diags := cdParseBody(t, strings.NewReader(input), &message.ParseOptions{Profile: message.ProfileStrict}); got != string(asDeclared) || diags != nil {
			t.Errorf("%s/strict: body %q %v, want as declared and no diagnostic", tc.name, got, diags)
		}
		e, err := message.Read(strings.NewReader(input))
		if err != nil {
			t.Fatalf("%s: Read() = %v", tc.name, err)
		}
		if b, _ := io.ReadAll(e.Body); string(b) != string(asDeclared) {
			t.Errorf("%s/Read: body %q, want as declared", tc.name, b)
		}
	}
}

// TestParse_fallbackMostlyUTF8: with a fallback charset, a body that is
// mostly UTF-8 stays UTF-8 — the bytes of its well-formed multi-byte
// sequences are more than twice its ill-formed bytes — and is watched; the
// fallback decodes the bodies that are not.
func TestParse_fallbackMostlyUTF8(t *testing.T) {
	const eucKR = "\xbe\xc8\xb3\xe7\xc7\xcf\xbc\xbc\xbf\xe4"
	for _, tc := range []struct {
		name, contentType, body, want string
		diags                         []string
	}{
		// é and € are five good bytes; the broken escape left two bad ones.
		{"damaged utf-8, declared", "text/plain; charset=utf-8", "Café € \xe2=82\xac end", "", []string{"info charset.invalid_sequence"}},
		{"damaged utf-8, undeclared", "text/plain", "Café € \xe2=82\xac end", "", []string{"info charset.missing"}},
		{"damaged utf-8, us-ascii", "text/plain; charset=us-ascii", "안녕하세요 \xff", "", []string{"info charset.declaration_suspect"}},
		// Two good bytes against one bad one is not more than twice.
		{"as much bad as good", "text/plain; charset=utf-8", "Caf\xc3\xa9 \xb0", "Caf챕 �", []string{"recovered charset.declaration_suspect"}},
		{"euc-kr, undeclared", "text/plain", eucKR, "안녕하세요", []string{"recovered charset.missing"}},
		// 화환 is well-formed UTF-8 by accident; the rest of the text is not.
		{"euc-kr with accidental sequences", "text/plain", "\xc8\xad\xc8\xaf " + eucKR, "화환 안녕하세요", []string{"recovered charset.missing"}},
	} {
		if tc.want == "" {
			tc.want = tc.body
		}
		input := "Content-Type: " + tc.contentType + "\r\n\r\n" + tc.body
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"whole":    func(r io.Reader) io.Reader { return r },
			"one byte": iotest.OneByteReader,
		} {
			got, diags := cdParseBody(t, wrap(strings.NewReader(input)), &message.ParseOptions{FallbackCharset: "euc-kr"})
			if got != tc.want || !reflect.DeepEqual(diags, tc.diags) {
				t.Errorf("%s/%s: body %q %v, want %q %v", tc.name, name, got, diags, tc.want, tc.diags)
			}
		}
	}
}

// TestParse_fallbackCharsetForParameters: a filename written as raw 8-bit
// bytes that nothing names a charset for is decoded with the fallback charset
// when the bytes are valid in it, marked and reported; with no fallback,
// under ProfileStrict, or when the bytes are not valid in it, it stays
// undecoded.
func TestParse_fallbackCharsetForParameters(t *testing.T) {
	const (
		rawName = "\xb0\xdf\xc0\xfb\xbc\xad.xls" // 견적서.xls in EUC-KR
		name    = "견적서.xls"
		latin   = "caf\xe9.pdf" // not EUC-KR
	)
	header := func(raw string) string {
		return "Content-Type: application/octet-stream; name=\"" + raw + "\"\r\n" +
			"Content-Disposition: attachment; filename=\"" + raw + "\"\r\n\r\nx"
	}
	for _, tc := range []struct {
		desc, raw string
		opts      message.ParseOptions
		want      string
		fallback  bool
		diags     []string
	}{
		{"fallback", rawName, message.ParseOptions{FallbackCharset: "EUC-KR"}, name, true,
			[]string{"recovered mime.parameter_fallback_charset", "recovered mime.parameter_fallback_charset"}},
		{"no fallback", rawName, message.ParseOptions{}, rawName, false,
			[]string{"unsupported mime.parameter_unknown_charset", "unsupported mime.parameter_unknown_charset"}},
		{"strict makes no guess", rawName, message.ParseOptions{FallbackCharset: "euc-kr", Profile: message.ProfileStrict}, rawName, false,
			[]string{"unsupported mime.parameter_unknown_charset", "unsupported mime.parameter_unknown_charset"}},
		{"not valid in the fallback", latin, message.ParseOptions{FallbackCharset: "euc-kr"}, latin, false,
			[]string{"unsupported mime.parameter_unknown_charset", "unsupported mime.parameter_unknown_charset"}},
		{"utf-8 is kept", name, message.ParseOptions{FallbackCharset: "euc-kr"}, name, false,
			[]string{"info mime.parameter_raw_utf8", "info mime.parameter_raw_utf8"}},
	} {
		s, e, err := message.Parse(strings.NewReader(header(tc.raw)), &tc.opts)
		if err != nil {
			t.Fatalf("%s: Parse() = %v", tc.desc, err)
		}
		names := e.FilenameCandidates()
		if len(names) != 2 {
			t.Fatalf("%s: %d candidates, want 2", tc.desc, len(names))
		}
		for _, n := range names {
			p := n.Parameter
			if n.Filename != tc.want || p.Fallback != tc.fallback || p.Undecoded != (tc.want == tc.raw && tc.raw != name) ||
				tc.fallback && p.Charset != "euc-kr" {
				t.Errorf("%s: %v = %q (fallback %v, undecoded %v, charset %q), want %q, fallback %v",
					tc.desc, n.Source, n.Filename, p.Fallback, p.Undecoded, p.Charset, tc.want, tc.fallback)
			}
		}
		var diags []string
		for _, d := range s.Diagnostics() {
			diags = append(diags, fmt.Sprintf("%s %s", d.Severity, d.Code))
		}
		if !reflect.DeepEqual(diags, tc.diags) {
			t.Errorf("%s: diagnostics %v, want %v", tc.desc, diags, tc.diags)
		}
	}

	// A header with no parse takes the fallback as an option.
	e, err := message.Read(strings.NewReader(header(rawName)))
	if err != nil {
		t.Fatal(err)
	}
	if n := e.Header.FilenameCandidates(); len(n) == 0 || !n[0].Parameter.Undecoded {
		t.Errorf("FilenameCandidates() = %+v, want the bytes undecoded", n)
	}
	n := e.Header.FilenameCandidatesWithOptions(&message.HeaderTextOptions{FallbackCharset: "euc-kr"})
	if len(n) == 0 || n[0].Filename != name || !n[0].Parameter.Fallback {
		t.Errorf("FilenameCandidatesWithOptions() = %+v, want %q by the fallback", n, name)
	}
}

// TestParse_declarationLookIsChunkInvariant: over seeded bodies mixed from
// ASCII, UTF-8, EUC-KR and stray bytes, under labels of every kind and with
// and without a fallback charset, the body and the diagnostics do not depend
// on how the input is cut into reads; a body is either what the declared
// decoder makes of it, or — with charset.declaration_suspect — its own bytes.
// The inputs are a fixed 1500, each of at most 6 KiB.
func TestParse_declarationLookIsChunkInvariant(t *testing.T) {
	pieces := []string{
		"plain text ", "<p>", "\r\n", "안녕하세요 ", "가", "é", "“x” ", "Привет ",
		"\xbe\xc8\xb3\xe7 ", "\xc8\xad\xc8\xaf", "\xff", "\xe2\x82", "\x1b$B", "=", strings.Repeat("a", 1500),
	}
	labels := []string{"euc-kr", "ks_c_5601-1987", "shift_jis", "iso-2022-jp", "gb2312", "big5", "windows-1252", "iso-8859-1", "windows-1251", "utf-7", "utf-8", "us-ascii", ""}
	seed := uint32(20261111)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	for i := 0; i < 1500; i++ {
		var body strings.Builder
		for k := next(8); k >= 0 && body.Len() < 6000; k-- {
			body.WriteString(pieces[next(len(pieces))])
		}
		label := labels[next(len(labels))]
		opts := &message.ParseOptions{}
		if next(3) == 0 {
			opts.FallbackCharset = "euc-kr"
		}
		ct := "text/plain"
		if label != "" {
			ct += "; charset=" + label
		}
		input := "Content-Type: " + ct + "\r\n\r\n" + body.String()

		want, wantDiags := cdParseBody(t, strings.NewReader(input), opts)
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"one byte": iotest.OneByteReader,
			"half":     iotest.HalfReader,
			"data err": iotest.DataErrReader,
		} {
			got, diags := cdParseBody(t, wrap(strings.NewReader(input)), opts)
			if got != want || !reflect.DeepEqual(diags, wantDiags) {
				t.Fatalf("%q under %q, %s: body %q %v; read whole %q %v", body.String(), label, name, got, diags, want, wantDiags)
			}
		}
		if label == "" || label == "utf-8" || label == "us-ascii" || opts.FallbackCharset != "" {
			continue
		}
		healed := reflect.DeepEqual(wantDiags, []string{"recovered charset.declaration_suspect"}) ||
			reflect.DeepEqual(wantDiags, []string{"recovered charset.declaration_suspect", "info charset.invalid_sequence"})
		declared, err := charset.Reader(label, strings.NewReader(body.String()))
		if err != nil {
			t.Fatal(err)
		}
		asDeclared, _ := io.ReadAll(declared)
		switch {
		case healed && want != body.String():
			t.Fatalf("%q under %q: healed to %q, want its own bytes", body.String(), label, want)
		case !healed && (want != string(asDeclared) || wantDiags != nil):
			t.Fatalf("%q under %q: %q %v, want it as declared, %q, and no diagnostic", body.String(), label, want, wantDiags, asDeclared)
		}
	}
}
