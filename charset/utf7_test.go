package charset

import (
	"encoding/base64"
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

// utf7Run is UTF-16 code units, given as their bytes in big-endian order, as
// one run of standard UTF-7: '+', their base64 without padding, '-'. It is
// built with the standard library, not with the encoder of this package.
func utf7Run(units string) string {
	return "+" + base64.RawStdEncoding.EncodeToString([]byte(units)) + "-"
}

// TestStandardUTF7_longRuns: a base64 run of any length decodes, in fixed
// memory. A run is decoded utf7Chunk characters (96 code units) at a time, so
// the cases sit on and around that boundary, and one is longer than every
// buffer between the input and the decoder.
func TestStandardUTF7_longRuns(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("가", 95),
		strings.Repeat("가", 96), // exactly one chunk
		strings.Repeat("가", 97),
		strings.Repeat("日", 300),
		strings.Repeat("가", 95) + "😀" + strings.Repeat("나", 50), // a surrogate pair across the boundary
		strings.Repeat("가", 94) + "😀" + strings.Repeat("나", 50), // and one that ends on it
		strings.Repeat("😀", 200),
		strings.Repeat("한글", 2500), // 13334 base64 characters in one run
	} {
		run := utf7Run(utf16Bytes(text, false))
		open := strings.TrimSuffix(run, "-")
		for _, c := range []struct{ in, want string }{
			{run, text},
			{"a " + run + " z", "a " + text + " z"},
			{open, text},              // ended by the end of the text
			{open + "!", text + "!"},  // or by a character outside base64
			{run + run, text + text},  // two runs back to back
			{open + "--", text + "-"}, // only one '-' is absorbed
		} {
			if got := decodeAll(t, "utf-7", c.in); got != c.want {
				t.Errorf("run of %d characters: %.12q... decoded to %d bytes, want the %d of the text", len(run), c.in, len(got), len(c.want))
			}
		}
	}
}

// TestStandardUTF7_faultsInLongRuns: once the first chunk of a long run has
// decoded, a fault further on is U+FFFD and the rest of the run still decodes.
// A fault in the first chunk leaves the text as it is, like a short run.
func TestStandardUTF7_faultsInLongRuns(t *testing.T) {
	const text = "가"
	chunk := utf16Bytes(strings.Repeat(text, 96), false)
	after := utf16Bytes("나다", false)
	for _, c := range []struct{ name, in, want string }{
		{"a base64 character too many",
			strings.TrimSuffix(utf7Run(chunk), "-") + "A!", strings.Repeat(text, 96) + "�!"},
		{"an unpaired low surrogate",
			utf7Run(chunk + "\xdc\x00" + after), strings.Repeat(text, 96) + "�나다"},
		{"an unpaired high surrogate",
			utf7Run(chunk + "\xd8\x3d" + after), strings.Repeat(text, 96) + "�나다"},
		{"a high surrogate at the end of the run",
			utf7Run(chunk + after + "\xd8\x3d"), strings.Repeat(text, 96) + "나다�"},
		{"a high surrogate at the end of the first chunk and of the run",
			utf7Run(chunk[:len(chunk)-2] + "\xd8\x3d"), strings.Repeat(text, 95) + "�"},
		// 0xDC00 then 200 times 'A': the base64 has no '+' in it, so the
		// whole run reads as the ASCII text it is.
		{"an unpaired surrogate in the first chunk",
			utf7Run("\xdc\x00" + strings.Repeat("\x00A", 200)), utf7Run("\xdc\x00" + strings.Repeat("\x00A", 200))},
	} {
		if got := decodeAll(t, "utf-7", c.in); got != c.want {
			t.Errorf("%s: %.12q... decoded to %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestStandardUTF7_failedRunIsText: a run that is not UTF-16 is kept whole,
// with its '+'. A '+' inside it is a base64 character of that run and does not
// start another, which is what keeps the decoder's work linear.
func TestStandardUTF7_failedRunIsText(t *testing.T) {
	for in, want := range map[string]string{
		"1+1=2":        "1+1=2",
		"a+b":          "a+b",
		"C++ and C#":   "C++ and C#",
		"+A+AOk-":      "+A+AOk-",
		"x +2AA- y":    "x +2AA- y",    // U+D800 alone
		"x +3ADYAA- y": "x +3ADYAA- y", // a low surrogate before a high one
		"+A-+AOk-":     "+A-é",         // the next run is its own
	} {
		if got := decodeAll(t, "utf-7", in); got != want {
			t.Errorf("%q decoded to %q, want %q", in, got, want)
		}
	}
}

// TestStandardUTF7_octetsAbove7F: UTF-7 is seven-bit. UTF-8 in it is kept, as
// before, and any other octet is U+FFFD, so the output is always valid UTF-8.
func TestStandardUTF7_octetsAbove7F(t *testing.T) {
	for in, want := range map[string]string{
		"h\xc3\xa9llo \xf0\x9f\x98\x80": "héllo 😀",
		"a\xe9b":                        "a�b",
		"a\xc3":                         "a�",
		"+AOk-\xff\xfe+AOk-":            "é��é",
		"\xed\xa0\x80":                  "���",
	} {
		if got := decodeAll(t, "utf-7", in); got != want {
			t.Errorf("%q decoded to %q, want %q", in, got, want)
		}
	}
}

// TestStandardUTF7_isNotIMAPModifiedUTF7 (ROB-080-006): every label that means
// UTF-7 decodes RFC 2152 and nothing of RFC 3501: '&' does not shift, ',' is
// not base64, and a mailbox name decodes to itself. No label reaches the IMAP
// codec, which is the utf7 package.
func TestStandardUTF7_isNotIMAPModifiedUTF7(t *testing.T) {
	var labels []string
	for _, f := range families {
		if f.name == "utf-7" {
			labels = strings.Fields(f.labels)
		}
	}
	if len(labels) < 8 {
		t.Fatalf("%d utf-7 labels in the registry test, want all of them", len(labels))
	}
	for _, label := range labels {
		for in, want := range map[string]string{
			// IMAP mailbox names: not decoded.
			"Entw&APw-rfe":                    "Entw&APw-rfe",
			"&ZeVnLIqe-":                      "&ZeVnLIqe-",
			"~peter/mail/&U,BTFw-/&ZeVnLIqe-": "~peter/mail/&U,BTFw-/&ZeVnLIqe-",
			"&-":                              "&-",
			"a&-b":                            "a&-b",
			// The same text in standard UTF-7: decoded.
			"Entw+APw-rfe": "Entwürfe",
			"+ZeVnLIqe-":   "日本語",
			"+U/BTFw-":     "台北", // '/' is base64 here; IMAP writes ','
			// The IMAP alphabet after a standard shift: the comma ends the
			// run, and what is left is too short to be one.
			"+U,BTFw-": "+U,BTFw-",
			"+AOk,-":   "é,-",
		} {
			if got := decodeAll(t, label, in); got != want {
				t.Errorf("%s: %q decoded to %q, want %q", label, in, got, want)
			}
		}
	}
	for _, label := range []string{
		"utf-7-imap", "utf7-imap", "imap-utf-7", "x-imap4-modified-utf7", "x-imap-mailbox-name", "imap-mailbox-name", "x-modified-utf-7", "mutf-7",
	} {
		if m, err := Lookup(label); err == nil {
			t.Errorf("Lookup(%q) = %q, want an unknown charset: IMAP modified UTF-7 is not a MIME charset", label, m.Name)
		}
	}
}

// TestStandardUTF7_malformedSweep decodes every string of up to four of the
// bytes that matter to the decoder. decodeAll fails the test on an error, on
// output that is not valid UTF-8 and on output that depends on how the input
// is cut; a string with no shift and no octet above 0x7F must come out as it
// went in.
func TestStandardUTF7_malformedSweep(t *testing.T) {
	alphabet := []string{"+", "-", "A", "2", "/", "&", ",", "\xe9", "\n"}
	var sweep func(prefix string, depth int)
	sweep = func(prefix string, depth int) {
		got := decodeAll(t, "utf-7", prefix)
		if !strings.Contains(prefix, "+") && !strings.Contains(prefix, "\xe9") && got != prefix {
			t.Errorf("%q decoded to %q, want it unchanged", prefix, got)
		}
		if depth == 0 {
			return
		}
		for _, c := range alphabet {
			sweep(prefix+c, depth-1)
		}
	}
	sweep("", 4)
}
