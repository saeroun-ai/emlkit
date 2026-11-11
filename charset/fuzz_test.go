package charset_test

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/charset"
)

// Native fuzz targets of the charset plugin (ROB-130-001):
// docs/robustness-notes/fuzzing.md lists their invariants (ROB-130-002).
// Every input is capped at 4 KiB, and every read of a decoder at a small
// multiple of it.

// fzLabels are the charsets every input is decoded in besides the one the
// fuzzer names: one decoder of each kind this package has, the CJK ones
// first.
var fzLabels = []string{
	"utf-8", "euc-kr", "ks_c_5601-1987", "iso-2022-kr", "shift_jis", "euc-jp", "iso-2022-jp",
	"gb2312", "gb18030", "big5", "hz-gb-2312", "utf-7", "utf-16", "utf-16le", "us-ascii",
	"iso-8859-1", "iso-8859-9", "windows-1252", "koi8-r", "unknown-8bit",
}

var fzReaderSeeds = []struct {
	label string
	data  string
}{
	{"utf-8", "한글 日本語 中文 \xff\xfe"},
	{"euc-kr", "\xc7\xd1\xb1\xdb \xc7"},
	{"ks_c_5601-1987", "\xc7\xd1\xb1\xdb \xc6\xc4\xc0\xcf"},
	{"iso-2022-kr", "\x1b$)C\x0eGQ1[\x0f"},
	{"ISO-2022-JP", "\x1b$BF|K\\8l\x1b(B \x1b$B"},
	{"shift_jis", "\x93\xfa\x96\x7b\x8c\xea\x93"},
	{"gb2312", "\xd6\xd0\xce\xc4"},
	{"HZ-GB-2312", "~{VPND~} ~{"},
	{"utf-7", "+1VyuAA- +AAA"},
	{"utf-16", "\xff\xfe\xe5e,g\x00"},
	{" \"charset=EUC-KR\"; format=flowed", "\xc7\xd1"},
	{"x-unknown", "caf\xe9 \xed\x95\x9c"},
	{"no-such-charset", "abc"},
}

// FuzzReader resolves an arbitrary label and decodes arbitrary bytes, in that
// charset and in each of fzLabels. Lookup is deterministic, its error is
// Reader's, and a Match it returns has an encoding and a lower-case name; each
// decoder ends, yields valid UTF-8, at most three bytes per byte in plus a
// constant, and the same output whether its input arrives whole or a byte at
// a time.
func FuzzReader(f *testing.F) {
	for _, s := range fzReaderSeeds {
		f.Add(s.label, []byte(s.data))
	}
	f.Fuzz(func(t *testing.T, label string, data []byte) {
		if len(label) > 256 || len(data) > 4<<10 {
			return
		}
		m, err := charset.Lookup(label)
		m2, err2 := charset.Lookup(label)
		if (err == nil) != (err2 == nil) || m.Name != m2.Name || m.Repairs != m2.Repairs {
			t.Fatalf("Lookup(%q) is not deterministic: %+v, %v; %+v, %v", label, m, err, m2, err2)
		}
		_, rerr := charset.Reader(label, strings.NewReader(""))
		if (err == nil) != (rerr == nil) || (err != nil && err.Error() != rerr.Error()) {
			t.Fatalf("Lookup(%q) = %v, Reader %v", label, err, rerr)
		}
		labels := fzLabels
		if err == nil {
			if m.Encoding == nil || m.Name == "" || m.Name != strings.ToLower(m.Name) {
				t.Fatalf("Lookup(%q) = %+v", label, m)
			}
			labels = append([]string{label}, fzLabels...)
		}
		for _, l := range labels {
			whole := fzDecode(t, l, bytes.NewReader(data), len(data))
			bytewise := fzDecode(t, l, iotest.OneByteReader(bytes.NewReader(data)), len(data))
			if !bytes.Equal(whole, bytewise) {
				t.Fatalf("%s: decoding %q a byte at a time gave %q, whole %q", l, data, bytewise, whole)
			}
			if !utf8.Valid(whole) {
				t.Fatalf("%s: %q decoded to %q, not UTF-8", l, data, whole)
			}
			if len(whole) > 3*len(data)+16 {
				t.Fatalf("%s: %d bytes decoded to %d", l, len(data), len(whole))
			}
		}
	})
}

func fzDecode(t *testing.T, label string, src io.Reader, n int) []byte {
	t.Helper()
	r, err := charset.Reader(label, src)
	if err != nil {
		t.Fatalf("Reader(%q) = %v", label, err)
	}
	out, err := io.ReadAll(io.LimitReader(r, int64(4*n+64)))
	if err != nil {
		t.Fatalf("%s: reading = %v", label, err)
	}
	return out
}

// FuzzDecodeHeaderText_charsets decodes header text with this package's
// decoders behind message.CharsetReader: an arbitrary value, the same bytes as
// a B and a Q word of an arbitrary label, and the value with that label as the
// fallback charset. Each decoding is deterministic, valid UTF-8, holds no line
// break or NUL the input did not, is at most three bytes per byte in plus a
// constant, and reports at most 64 declared codes.
func FuzzDecodeHeaderText_charsets(f *testing.F) {
	for _, s := range fzReaderSeeds {
		f.Add(s.label, []byte(s.data), bWord(s.label, []byte(s.data))+" and raw "+s.data)
	}
	f.Fuzz(func(t *testing.T, label string, data []byte, raw string) {
		if len(label) > 256 || len(data) > 4<<10 || len(raw) > 4<<10 {
			return
		}
		inputs := []string{raw, bWord(label, data), qWord(label, data), bWord(label, data[:len(data)/2]) + " " + bWord(label, data[len(data)/2:])}
		for _, s := range inputs {
			out, diags := message.DecodeHeaderText(s)
			fzCheckHeaderText(t, "DecodeHeaderText", s, out, diags)
			if again, diags2 := message.DecodeHeaderText(s); again != out || !reflect.DeepEqual(diags, diags2) {
				t.Fatalf("DecodeHeaderText(%q) is not deterministic", s)
			}
		}
		opts := &message.HeaderTextOptions{FallbackCharset: label}
		out, diags := message.DecodeHeaderTextWithOptions(raw, opts)
		fzCheckHeaderText(t, "DecodeHeaderTextWithOptions", raw, out, diags)
		if again, _ := message.DecodeHeaderTextWithOptions(raw, opts); again != out {
			t.Fatalf("DecodeHeaderTextWithOptions(%q, %q) is not deterministic", raw, label)
		}
		if strings.ContainsAny(label, "\r\n\"\\") {
			return
		}
		// The same bytes as a text body in that charset.
		msg := fmt.Sprintf("Content-Type: text/plain; charset=\"%s\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s", label, data)
		whole := fzParseBody(t, strings.NewReader(msg))
		if bytewise := fzParseBody(t, iotest.OneByteReader(strings.NewReader(msg))); bytewise != whole {
			t.Fatalf("parsing %q a byte at a time:\n%s\nwhole:\n%s", msg, bytewise, whole)
		}
	})
}

func fzCheckHeaderText(t *testing.T, what, s, out string, diags []message.Diagnostic) {
	t.Helper()
	if !utf8.ValidString(out) {
		t.Fatalf("%s(%q) = %q, not UTF-8", what, s, out)
	}
	if !strings.ContainsAny(s, "\r\n\x00") && strings.ContainsAny(out, "\r\n\x00") {
		t.Fatalf("%s(%q) = %q, a line break or NUL the value did not hold", what, s, out)
	}
	if len(out) > 3*len(s)+16 {
		t.Fatalf("%s(%q): %d bytes out of %d", what, s, len(out), len(s))
	}
	if len(diags) > 64 {
		t.Fatalf("%s(%q): %d diagnostics", what, s, len(diags))
	}
	for _, d := range diags {
		if d.Code.Group() == "" || d.Severity > message.SeverityFatal || d.Message == "" {
			t.Fatalf("%s(%q): diagnostic %+v", what, s, d)
		}
	}
}

// fzParseBody parses a message and renders its body and diagnostics.
func fzParseBody(t *testing.T, src io.Reader) string {
	t.Helper()
	s, e, err := message.Parse(src, &message.ParseOptions{Limits: message.Limits{MaxInputBytes: 1 << 20}})
	if e == nil {
		return fmt.Sprintf("no entity, fatal=%v", message.IsFatal(err))
	}
	body, rerr := io.ReadAll(io.LimitReader(e.Body, 1<<20))
	var b strings.Builder
	fmt.Fprintf(&b, "body %q err=%v\n", body, rerr != nil)
	for _, d := range s.Diagnostics() {
		fmt.Fprintf(&b, "%s %v\n", d.Code, d.Path)
	}
	return b.String()
}
