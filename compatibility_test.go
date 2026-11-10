package message

import (
	"io"
	"strings"
	"testing"
)

// TestParse_compatibilityPolicies pins the decisions a Compatibility changes
// (issue #17): how a damaged header line that continues no field is read, and
// whether Base64 data after padding is decoded.
func TestParse_compatibilityPolicies(t *testing.T) {
	// A damaged line with a known field after it, and one with none.
	const inHeader = "Subject: s\r\ndamaged line\r\nTo: a@example.com\r\n\r\nbody"
	const atEnd = "Subject: s\r\ndamaged line\r\nplain text\r\n\r\nbody"
	for _, tc := range []struct {
		compat             Compatibility
		inHeader, atEnd    string // the body each reading yields
		toKept, plainField bool
	}{
		{CompatibilityConservative, "body", "damaged line\r\nplain text\r\n\r\nbody", true, false},
		{CompatibilityPythonEmail, "damaged line\r\nTo: a@example.com\r\n\r\nbody", "damaged line\r\nplain text\r\n\r\nbody", false, false},
		{CompatibilityMimeKit, "damaged line\r\nTo: a@example.com\r\n\r\nbody", "damaged line\r\nplain text\r\n\r\nbody", false, false},
		{CompatibilityAngus, "body", "body", true, false},
	} {
		for i, text := range []string{inHeader, atEnd} {
			_, e, err := Parse(strings.NewReader(text), &ParseOptions{Compatibility: tc.compat})
			if err != nil {
				t.Fatalf("%v: Parse() = %v", tc.compat, err)
			}
			b, _ := io.ReadAll(e.Body)
			want := tc.inHeader
			if i == 1 {
				want = tc.atEnd
			}
			if string(b) != want {
				t.Errorf("%v/%d: body %q, want %q", tc.compat, i, b, want)
			}
			if i == 0 && e.Header.Has("To") != tc.toKept {
				t.Errorf("%v: To kept %v, want %v", tc.compat, e.Header.Has("To"), tc.toKept)
			}
		}
	}

	const b64 = "Content-Transfer-Encoding: base64\r\n\r\naGk=\r\naGk=\r\n"
	for compat, want := range map[Compatibility]string{
		CompatibilityConservative: "hihi",
		CompatibilityAngus:        "hihi",
		CompatibilityMimeKit:      "hihi",
		CompatibilityPythonEmail:  "hi",
	} {
		s, e, _ := Parse(strings.NewReader(b64), &ParseOptions{Compatibility: compat})
		b, err := io.ReadAll(e.Body)
		if string(b) != want || err != nil {
			t.Errorf("%v: base64 body (%q, %v), want %q", compat, b, err, want)
		}
		if codes := codesOfDiags(s.Diagnostics()); len(codes) != 1 || codes[0] != CodeCTEBase64DataAfterPadding {
			t.Errorf("%v: diagnostics %v", compat, codes)
		}
	}

	// A conformant message reads the same under every policy.
	const clean = "Subject: s\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B--\r\n"
	for _, compat := range []Compatibility{CompatibilityConservative, CompatibilityAngus, CompatibilityMimeKit, CompatibilityPythonEmail} {
		s, e, _ := Parse(strings.NewReader(clean), &ParseOptions{Compatibility: compat})
		if err := e.Drain(); err != nil || !s.Completion().Clean() {
			t.Errorf("%v: conformant message: %v, %+v", compat, err, s.Completion())
		}
	}
}
