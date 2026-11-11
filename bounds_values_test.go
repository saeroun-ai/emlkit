package message_test

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit"
)

// Boundary tests of the value parsers (ROB-120-001; see
// docs/robustness-notes/bounds-audit-values.md). They hold on a 32-bit int as
// on a 64-bit one: run them with GOARCH=386 too.

// TestBVMonitor_sequenceCutByARead: a UTF-8 body is judged the same however
// its reads cut it. An encoded U+FFFD is text, and decodes to the same rune a
// decoding error is reported with; only the width tells them apart.
func TestBVMonitor_sequenceCutByARead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		invalid bool
	}{
		{"U+FFFD", "ab\xef\xbf\xbdcd", false},
		{"four bytes", "ab\xf0\x9f\x98\x80cd", false},
		{"two bytes at the end", "ab\xc3\xa9", false},
		{"cut sequence", "ab\xef\xbfAcd", true},
		{"surrogate", "ab\xed\xa0\x80cd", true},
		{"ends inside a sequence", "ab\xef\xbf", true},
	} {
		for _, chunk := range []string{"whole", "one byte"} {
			s, e, err := message.Parse(strings.NewReader("Content-Type: text/plain; charset=utf-8\r\n\r\n"+tc.body), nil)
			if e == nil {
				t.Fatalf("%s: Parse() = %v", tc.name, err)
			}
			var body io.Reader = e.Body
			if chunk == "one byte" {
				body = iotest.OneByteReader(body)
			}
			got, err := io.ReadAll(body)
			if err != nil || string(got) != tc.body {
				t.Errorf("%s, %s: body %q, %v", tc.name, chunk, got, err)
			}
			invalid := 0
			for _, d := range s.Diagnostics() {
				if d.Code == message.CodeCharsetInvalidSequence {
					invalid++
				}
			}
			if want := map[bool]int{true: 1}[tc.invalid]; invalid != want {
				t.Errorf("%s, %s: %d charset.invalid_sequence diagnostics, want %d", tc.name, chunk, invalid, want)
			}
		}
	}
}

// TestBVParameters_numericBoundaries: the numbers a parameter carries — an RFC
// 2231 section, a percent escape — at and past the ends of their ranges.
func TestBVParameters_numericBoundaries(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  string
	}{
		{"attachment; filename*0=a; filename*1=b", "ab"},
		{"attachment; filename*1=b; filename*0=a", "ab"},
		// Nine digits is the widest section there is; it fits a 32-bit int.
		{"attachment; filename*0=a; filename*999999999=z", "az"},
		{"attachment; filename*000000001=b; filename*0=a", "ab"},
		// Wider than that is no section, whatever it would overflow.
		{"attachment; filename*0=a; filename*2147483648=z", "a"},
		{"attachment; filename*0=a; filename*4294967296=z", "a"},
		{"attachment; filename*0=a; filename*18446744073709551616=z", "a"},
		{"attachment; filename*0=a; filename*-1=z", "a"},
		// A percent sign with fewer than two hex digits after it is itself.
		{"attachment; filename*=utf-8''a%", "a%"},
		{"attachment; filename*=utf-8''a%4", "a%4"},
		{"attachment; filename*=utf-8''a%zz", "a%zz"},
		{"attachment; filename*=utf-8''%41%4a", "AJ"},
		{"attachment; filename*0*=utf-8''%4; filename*1*=1", "%41"},
	} {
		_, params, _ := message.ParseContentDispositionTolerant(tc.field)
		if got := params["filename"]; got != tc.want {
			t.Errorf("%q: filename %q, want %q", tc.field, got, tc.want)
		}
	}
}

// TestBVHeaderText_escapeBoundaries: escapes and groups that the end of a
// word, or of the value, cuts.
func TestBVHeaderText_escapeBoundaries(t *testing.T) {
	label128 := strings.Repeat("x", 128)
	for _, tc := range []struct{ in, want string }{
		{"=?utf-8?q?=41?=", "A"},
		{"=?utf-8?q?a=?=", "a="},
		{"=?utf-8?q?a=4?=", "a=4"},
		{"=?utf-8?q?=4", "=4"},
		{"=?utf-8?b?QQ==?=", "A"},
		{"=?utf-8?b?QQ?=", "A"},
		{"=?utf-8?b?QUI?=", "AB"},
		{"=?utf-8?b?Q?=", "=?utf-8?b?Q?="},
		{"=?utf-8?q?", "=?utf-8?q?"},
		{"=?", "=?"},
		{"=", "="},
		{"=?" + label128 + "?q?a?=", "=?" + label128 + "?q?a?="},
		{"=?" + label128 + "x?q?a?=", "=?" + label128 + "x?q?a?="},
	} {
		got, diags := message.DecodeHeaderText(tc.in)
		if got != tc.want {
			t.Errorf("DecodeHeaderText(%q) = %q, want %q", tc.in, got, tc.want)
		}
		for _, d := range diags {
			if l := d.Location; l.Offset < 0 || l.EndOffset > int64(len(tc.in)) || l.Offset > l.EndOffset {
				t.Errorf("DecodeHeaderText(%q): %s is located outside the value", tc.in, d)
			}
		}
	}
}

// bvValueFragments are the pieces the value parsers branch on.
var bvValueFragments = []string{
	"=?", "?=", "?q?", "?b?", "utf-8", "iso-8859-1", "us-ascii", "unknown-8bit", "=E9", "=0D=0A", "=00", "=", "_", "QQ==", "QQ",
	"%", "%41", "%e9", "''", "'", "*", "*0", "*1", "*1*", "*999999999", "*9999999999", "filename", "name", "boundary", "charset",
	";", "\"", "\\", " ", "\t", "\r\n", "(", ")", "/", ",", ":", "<", ">", "|", ".", "..", "text", "multipart", "attachment",
	"a", "CON", "aux.txt", "\xe9", "\xff", "\xef\xbf", "\x00", "\x7f", "é", "‮", " ", "\v", "\f",
}

// TestBVValueParsers_bounds runs the value parsers over a fixed pseudo-random
// series of mixtures and over long floods of one fragment. None may panic, and
// what each returns stays within a small multiple of what it was given.
func TestBVValueParsers_bounds(t *testing.T) {
	check := func(v string) {
		t.Helper()
		short := v
		if len(short) > 80 {
			short = short[:80] + "..."
		}

		text, diags := message.DecodeHeaderText(v)
		if !utf8.ValidString(text) {
			t.Errorf("DecodeHeaderText(%q) is not UTF-8", short)
		}
		// Three bytes of U+FFFD for one invalid byte is the widest step.
		if len(text) > 3*len(v) {
			t.Errorf("DecodeHeaderText(%q): %d bytes out of %d", short, len(text), len(v))
		}
		if len(diags) > 64 {
			t.Errorf("DecodeHeaderText(%q): %d diagnostics", short, len(diags))
		}
		for _, d := range diags {
			if l := d.Location; l.Offset < 0 || l.EndOffset > int64(len(v)) || l.Offset > l.EndOffset {
				t.Errorf("DecodeHeaderText(%q): %s is located outside the value", short, d)
			}
		}

		for _, field := range []string{"Content-Type", "Content-Disposition"} {
			var params map[string]string
			if field == "Content-Type" {
				_, params, _ = message.ParseMediaTypeTolerant(v)
			} else {
				_, params, _ = message.ParseContentDispositionTolerant(v)
			}
			size := 0
			for k, p := range params {
				size += len(k) + len(p)
			}
			if size > 3*len(v)+64 {
				t.Errorf("%s %q: parameters of %d bytes out of %d", field, short, size, len(v))
			}
			// The default parameter budget, and the one parameter a base
			// without its semicolon may carry.
			if cands := message.ParseParameterCandidates(field, v); len(cands) > 257 {
				t.Errorf("%s %q: %d candidates", field, short, len(cands))
			}
		}

		safe := message.SafeFilename(v)
		if safe == "" || len(safe) > 255 || !utf8.ValidString(safe) || strings.ContainsAny(safe, "/\\\x00\r\n") {
			t.Errorf("SafeFilename(%q) = %q", short, safe)
		}
	}

	seed := uint32(20261111)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := 1 + next(16); n > 0; n-- {
			b.WriteString(bvValueFragments[next(len(bvValueFragments))])
		}
		check(b.String())
	}
	// Past the default budget of one header field, which no parse hands over
	// and a caller may.
	for _, f := range bvValueFragments {
		check(strings.Repeat(f, (70<<10)/len(f)))
		// Within it, where encoded words in a parameter value are decoded.
		check("a/b; x=\"" + strings.Repeat(f, (8<<10)/len(f)))
		check("=?utf-8?q?" + strings.Repeat(f, (8<<10)/len(f)))
	}
}
