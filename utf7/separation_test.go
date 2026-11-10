package utf7_test

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit/utf7"
	"golang.org/x/text/transform"
)

// ROB-080-006: modified UTF-7 (RFC 3501) and standard UTF-7 (RFC 2152) are two
// encodings. This package is the first and only the first; the charset
// package's tests pin the other side.

// TestDecoder_isNotStandardUTF7: standard UTF-7 is not decoded. Where it is
// printable ASCII without '&' it is a valid mailbox name that means itself,
// and where it uses what only RFC 2152 allows after a shift it is an error.
func TestDecoder_isNotStandardUTF7(t *testing.T) {
	dec := utf7.Encoding.NewDecoder()
	for _, in := range []string{
		"caf+AOk-",           // "café" in standard UTF-7
		"+ZeVnLIqe-",         // "日本語"
		"+ADw-script+AD4-",   // "<script>"
		"Hi Mom -+Jjo--!",    // RFC 2152's own example
		"+/v8-", "a+-b", "+", // standard base64 '/', the escaped plus, a bare shift
	} {
		if out, err := dec.String(in); err != nil || out != in {
			t.Errorf("decoding standard UTF-7 %q gave (%q, %v), want it unchanged", in, out, err)
		}
	}
	for _, in := range []string{
		"&/v8-",       // '/' is not in the modified alphabet
		"&ZeVnLIqe",   // a run must be closed with '-'
		"&ZeVnLIqe!",  // and is not ended by another character
		"caf&AOk",     //
		"&AOk-&AOk-",  // two runs with nothing between them
		"&AGEAYgBj-",  // "abc" in base64: printable ASCII must not be encoded
		"caf\xc3\xa9", // octets above 0x7F
	} {
		if out, err := dec.String(in); err == nil {
			t.Errorf("decoding %q gave %q, want an error", in, out)
		}
	}
	// And the modified form of the same text does decode.
	for in, want := range map[string]string{
		"caf&AOk-":   "café",
		"&ZeVnLIqe-": "日本語",
		"&,,8-":      "\xef\xbf\xbf", // U+FFFF: ',' is the last letter of the alphabet
	} {
		if out, err := dec.String(in); err != nil || out != want {
			t.Errorf("decoding %q gave (%q, %v), want %q", in, out, err, want)
		}
	}
}

// readAll reads r to its end in at most limit reads.
func readAll(t *testing.T, r io.Reader, limit int) (string, error) {
	t.Helper()
	var out []byte
	buf := make([]byte, 512)
	for range limit {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return string(out), err
		}
	}
	t.Fatalf("no end after %d reads and %d bytes", limit, len(out))
	return "", nil
}

// TestDecoder_malformedSweep decodes every string of up to four of the bytes
// that matter to the decoder, in one piece and a byte at a time through a
// reader. Nothing may panic or fail to end, the two must agree on whether the
// string is valid and on what it decodes to, and what it decodes to must be
// text the encoder can write and the decoder reads back the same.
func TestDecoder_malformedSweep(t *testing.T) {
	alphabet := []string{"&", "-", "A", "J", ",", "+", "/", "=", "\r", "\x00", "\xff"}
	dec, enc := utf7.Encoding.NewDecoder(), utf7.Encoding.NewEncoder()
	var sweep func(in string, depth int)
	sweep = func(in string, depth int) {
		out, err := dec.String(in)
		streamed, serr := readAll(t, transform.NewReader(iotest.OneByteReader(strings.NewReader(in)), utf7.Encoding.NewDecoder()), 64)
		switch {
		case (err == nil) != (serr == io.EOF):
			t.Errorf("%q: decoding gave error %v in one piece and %v a byte at a time", in, err, serr)
		case err == nil && (out != streamed || !utf8.ValidString(out)):
			t.Errorf("%q: decoded to %q in one piece and %q a byte at a time", in, out, streamed)
		case err == nil:
			back, err := enc.String(out)
			if again, err2 := dec.String(back); err != nil || err2 != nil || again != out {
				t.Errorf("%q decoded to %q, which encodes to (%q, %v) and back to (%q, %v)", in, out, back, err, again, err2)
			}
		}
		if depth == 0 {
			return
		}
		for _, c := range alphabet {
			sweep(in+c, depth-1)
		}
	}
	sweep("", 4)
}

// TestDecoder_unterminatedShiftEnds: a shift that is never closed is an error
// however long it is and however it is read; it is not waited for.
func TestDecoder_unterminatedShiftEnds(t *testing.T) {
	in := "INBOX/&" + strings.Repeat("AAAA", 16<<10) // 64 KiB after the shift
	if out, err := utf7.Encoding.NewDecoder().String(in); err == nil {
		t.Errorf("decoding an unterminated shift gave %d bytes and no error", len(out))
	}
	for _, src := range []io.Reader{strings.NewReader(in), iotest.OneByteReader(strings.NewReader(in))} {
		out, err := readAll(t, transform.NewReader(src, utf7.Encoding.NewDecoder()), 1<<10)
		if err == nil || err == io.EOF || out != "INBOX/" {
			t.Errorf("reading an unterminated shift gave (%q, %v), want the text before it and an error", out, err)
		}
	}
}
