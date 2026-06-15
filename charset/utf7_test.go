package charset

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit"
	"golang.org/x/text/transform"
)

func encodeUTF7(t *testing.T, s string) string {
	t.Helper()
	out, _, err := transform.String(standardUTF7.NewEncoder(), s)
	if err != nil {
		t.Fatalf("encode %q: %v", s, err)
	}
	return out
}

func decodeUTF7(t *testing.T, s string) string {
	t.Helper()
	r, err := Reader("utf-7", strings.NewReader(s))
	if err != nil {
		t.Fatalf("Reader(utf-7) %q: %v", s, err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return string(b)
}

func TestStandardUTF7_encode(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"hello", "hello"},
		{"café", "caf+AOk-"},
		{"<script>", "+ADw-script+AD4-"},
		{"+", "+-"},
		{"a+b", "a+-b"},
	} {
		if got := encodeUTF7(t, c.in); got != c.want {
			t.Errorf("encode %q = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStandardUTF7_roundTrip probes encoder/decoder symmetry across scripts,
// including non-BMP runes (surrogate pairs) and shift/direct/set-O boundaries.
func TestStandardUTF7_roundTrip(t *testing.T) {
	for _, in := range []string{
		"",
		"plain ascii, with punctuation!?",
		"café résumé naïve Köln",
		"日本語のメール 한국어 테스트 Ελληνικά Привет",
		"emoji 😀🎉🚀 end",     // non-BMP, consecutive
		"math 𝔘𝔫𝔦𝔠𝔬𝔡𝔢 done", // non-BMP letters
		"shift/direct: + - & = < > / , ' ( ) : ?",
		"mixed café 日本 😀 +plus& tail",
	} {
		encoded := encodeUTF7(t, in)
		if got := decodeUTF7(t, encoded); got != in {
			t.Errorf("round-trip mismatch:\n in=%q\n enc=%q\nout=%q", in, encoded, got)
		}
	}
}

// TestStandardUTF7_decodeStreaming feeds a shift-heavy input one byte at a time
// to exercise the transformer's ErrShortSrc handling across buffer boundaries.
func TestStandardUTF7_decodeStreaming(t *testing.T) {
	const encoded = "+ADw-script+AD4- +ZeVnLIqeMFMwkzBrMGEwbw-!"
	const want = "<script> 日本語こんにちは!"
	r, err := Reader("utf-7", iotest.OneByteReader(strings.NewReader(encoded)))
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != want {
		t.Errorf("streaming decode = %q, want %q", string(got), want)
	}
}

func TestStandardUTF7_decodePreservesMalformedInput(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{"+A", "+A"},              // 1 base64 char: too short for a UTF-16 unit
		{"+AB", "+AB"},            // 2 chars -> 1 byte: not a whole UTF-16 unit
		{"+ABCD-", "+ABCD-"},      // 4 chars -> 3 bytes: not whole UTF-16 units
		{"abc+", "abc+"},          // lone shift-in at EOF
		{"h\xc3\xa9llo", "héllo"}, // raw UTF-8 byte sequence in direct position
	} {
		r, err := Reader("utf-7", strings.NewReader(c.in))
		if err != nil {
			t.Fatalf("Reader(%q): unexpected setup error: %v", c.in, err)
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("decode %q: unexpected error: %v", c.in, err)
		}
		if got := string(b); got != c.want {
			t.Errorf("decode %q = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStandardUTF7_messageIntegration decodes a full text/plain message that
// advertises charset=utf-7 (Outlook-style) through message.Read + the plugin.
func TestStandardUTF7_messageIntegration(t *testing.T) {
	const raw = "Content-Type: text/plain; charset=utf-7\r\n" +
		"\r\n" +
		"Heads up: +ADw-b+AD4-caf+AOk-+ADw-/b+AD4-"
	e, err := message.Read(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	b, err := io.ReadAll(e.Body)
	if err != nil {
		t.Fatalf("ReadAll body: %v", err)
	}
	if want := "Heads up: <b>café</b>"; string(b) != want {
		t.Errorf("decoded body = %q, want %q", string(b), want)
	}
}
