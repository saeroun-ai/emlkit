package message

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// cdSeverityCodes returns the diagnostics as "severity code".
func cdSeverityCodes(diags []Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Severity.String()+" "+string(d.Code))
	}
	return out
}

// TestParse_undeclaredBodyWatched: a text body that declares no charset, or
// US-ASCII, is delivered as it was read, and bytes in it that are neither
// US-ASCII nor UTF-8 are reported once at SeverityInfo — charset.missing or
// charset.declaration_suspect — so that a caller who named no fallback
// charset learns the text is undecoded. UTF-8 and ASCII are not reported.
func TestParse_undeclaredBodyWatched(t *testing.T) {
	const eucKR = "\xbe\xc8\xb3\xe7\xc7\xcf\xbc\xbc\xbf\xe4"
	for _, tc := range []struct {
		name, header, body string
		want               []string
	}{
		// Text only by default: nothing declares these bytes to be text.
		{"no content type", "", eucKR, nil},
		{"no charset", "Content-Type: text/plain\r\n", eucKR, []string{"info charset.missing"}},
		{"no charset, html", "Content-Type: text/html\r\n", "<p>" + eucKR, []string{"info charset.missing"}},
		{"us-ascii", "Content-Type: text/plain; charset=us-ascii\r\n", eucKR, []string{"info charset.declaration_suspect"}},
		{"no charset, utf-8", "Content-Type: text/plain\r\n", "안녕하세요", nil},
		{"no charset, ascii", "Content-Type: text/plain\r\n", "hello", nil},
		{"not text", "Content-Type: application/octet-stream\r\n", eucKR, nil},
	} {
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"whole":    func(r io.Reader) io.Reader { return r },
			"one byte": iotest.OneByteReader,
		} {
			s, e, err := Parse(wrap(strings.NewReader("Subject: s\r\n"+tc.header+"\r\n"+tc.body)), nil)
			if err != nil {
				t.Fatal(err)
			}
			if b, rerr := io.ReadAll(e.Body); string(b) != tc.body || rerr != nil {
				t.Errorf("%s/%s: body (%q, %v), want every byte as read", tc.name, name, b, rerr)
			}
			if got := cdSeverityCodes(s.Diagnostics()); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s/%s: diagnostics %v, want %v", tc.name, name, got, tc.want)
			}
		}
		// ProfileStrict reports what it always did: nothing.
		s, e, err := Parse(strings.NewReader("Subject: s\r\n"+tc.header+"\r\n"+tc.body), &ParseOptions{Profile: ProfileStrict})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, e.Body)
		if s.Len() != 0 {
			t.Errorf("%s/strict: diagnostics %v, want none", tc.name, cdSeverityCodes(s.Diagnostics()))
		}
	}
}

// TestParse_parameterWordUnknownCharset: an RFC 2047 encoded word in a
// parameter value whose charset has no decoder stays as it was written, and
// the candidate says so (Undecoded, mime.parameter_unknown_charset) instead
// of passing the word off as the name. A word that decodes is not affected.
func TestParse_parameterWordUnknownCharset(t *testing.T) {
	const word = "=?x-no-such-charset?B?sN/A+7yt?="
	for _, tc := range []struct {
		value, want string
		undecoded   bool
		diags       []string
	}{
		{word, word, true, []string{"unsupported mime.parameter_unknown_charset"}},
		{"=?utf-8?B?6rCA?= " + word, "가 " + word, true, []string{"unsupported mime.parameter_unknown_charset", "info mime.parameter_rfc2047"}},
		{"=?utf-8?B?6rCA?=.pdf", "가.pdf", false, []string{"info mime.parameter_rfc2047"}},
	} {
		s, e, err := Parse(strings.NewReader("Content-Type: application/pdf; name=\""+tc.value+"\"\r\n\r\nx"), nil)
		if err != nil {
			t.Fatal(err)
		}
		names := e.FilenameCandidates()
		if len(names) != 1 || names[0].Filename != tc.want || names[0].Parameter.Undecoded != tc.undecoded {
			t.Errorf("%q: candidates %+v, want %q, undecoded %v", tc.value, names, tc.want, tc.undecoded)
		}
		if got := cdSeverityCodes(s.Diagnostics()); !reflect.DeepEqual(got, tc.diags) {
			t.Errorf("%q: diagnostics %v, want %v", tc.value, got, tc.diags)
		}
		// The same without a parse.
		if n := e.Header.FilenameCandidates(); len(n) != 1 || n[0].Parameter.Undecoded != tc.undecoded {
			t.Errorf("%q: Header.FilenameCandidates() = %+v, want undecoded %v", tc.value, n, tc.undecoded)
		}
	}
}

// TestUTF8Scan_chunkInvariant: the scan of a sniff window counts the same
// whatever pieces the window arrives in, and leaves a sequence the end of a
// full window cuts out of the count.
func TestUTF8Scan_chunkInvariant(t *testing.T) {
	for _, w := range []string{
		"plain",
		"caf\xc3\xa9 \xe2\x82\xac \xf0\x9f\x98\x80",
		"\xb0\xa1\xc8\xad\xc8\xaf\xe4",
		"a\x1bb\xc3",
		strings.Repeat("a", charsetSniff-1) + "\xec\x95\x88",
		strings.Repeat("\xea\xb0\x80", charsetSniff/3) + "\xea",
	} {
		w = w[:min(len(w), charsetSniff)]
		whole := cdScanWindow([]byte(w))
		var step cdUTF8Scan
		for i := 1; i <= len(w); i++ {
			step.scan([]byte(w[:i]), i == len(w) && len(w) < charsetSniff)
		}
		if whole != step {
			t.Errorf("%q...: whole %+v, a byte at a time %+v", w[:min(len(w), 12)], whole, step)
		}
	}
	if s := cdScanWindow([]byte(strings.Repeat("a", charsetSniff-1) + "\xec")); s.bad != 0 || s.multi != 0 {
		t.Errorf("a sequence cut by a full window: %+v, want it left out", s)
	}
	if s := cdScanWindow([]byte("ab\xec")); s.bad != 1 {
		t.Errorf("a sequence cut by the end of the body: %+v, want one ill-formed byte", s)
	}
}
