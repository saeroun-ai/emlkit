package message

import (
	"bytes"
	"encoding/base64"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// decodeTolerant decodes wire as the body of an entity declaring enc, built
// without a parse, read through a source of one byte at a time and through one
// that returns everything at once; both must agree.
func decodeTolerant(t *testing.T, enc, wire string) string {
	t.Helper()
	var outs []string
	for _, src := range []io.Reader{strings.NewReader(wire), iotest.OneByteReader(strings.NewReader(wire)), iotest.DataErrReader(strings.NewReader(wire))} {
		dec, err := encodingReader(enc, src)
		if err != nil {
			t.Fatalf("encodingReader(%q) = %v", enc, err)
		}
		b, err := io.ReadAll(dec)
		if err != nil {
			t.Fatalf("%q: ReadAll = %v", wire, err)
		}
		outs = append(outs, string(b))
	}
	if outs[0] != outs[1] || outs[0] != outs[2] {
		t.Errorf("%q: chunking changed the result: %q", wire, outs)
	}
	return outs[0]
}

// parseBodyCodes parses a message with the given transfer encoding header value
// and body, reads the body, and returns it with the diagnostic codes recorded.
func parseBodyCodes(t *testing.T, opts *ParseOptions, cte, wire string) (string, []Code, error) {
	t.Helper()
	s, e, err := Parse(strings.NewReader("Content-Transfer-Encoding: "+cte+"\r\n\r\n"+wire), opts)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	b, rerr := io.ReadAll(e.Body)
	if rerr != nil {
		t.Fatalf("reading the body = %v", rerr)
	}
	return string(b), codesOfDiags(s.Diagnostics()), err
}

func TestTolerantBase64(t *testing.T) {
	tests := []struct {
		name, wire, want string
		codes            []Code
	}{
		{"conformant", "aGVsbG8gd29ybGQ=", "hello world", nil},
		{"wrapped", "aGVs\r\nbG8g\r\nd29y\r\nbGQ=\r\n", "hello world", nil},
		{"indented and spaced", " aGVs bG8g\r\n\td29y bGQ=", "hello world", nil},
		{"bare LF and CR", "aGVs\nbG8g\rd29ybGQ=", "hello world", nil},
		{"junk bytes", "aGVs!bG8g*d29y\x00bGQ=\r\n", "hello world", []Code{CodeCTEBase64Junk}},
		{"missing padding 2", "aGVsbG8gd29ybGQ", "hello world", []Code{CodeCTEBase64PaddingMissing}},
		{"missing padding 3", "aGVsbG8=aGk", "hellohi", []Code{CodeCTEBase64DataAfterPadding, CodeCTEBase64PaddingMissing}},
		{"comment only", "aGk=", "hi", nil},
		{"one of two pads", "aGk", "hi", []Code{CodeCTEBase64PaddingMissing}},
		{"half padded", "aA=", "h", []Code{CodeCTEBase64PaddingInvalid}},
		{"excess padding", "aGk====", "hi", []Code{CodeCTEBase64PaddingInvalid}},
		{"padding at group start", "=aGVs", "hel", []Code{CodeCTEBase64PaddingInvalid}},
		{"single trailing character", "aGVsbG8gd29ybGQ=a", "hello world", []Code{CodeCTEBase64DataAfterPadding, CodeCTEBase64LengthInvalid}},
		{"length mod 4 is 1", "aGVsb", "hel", []Code{CodeCTEBase64LengthInvalid}},
		{"concatenated segments", "aGk=\r\naGk=\r\n", "hihi", []Code{CodeCTEBase64DataAfterPadding}},
		{"url alphabet", "_-8=", "\xff\xef", []Code{CodeCTEBase64URLAlphabet}},
		{"empty", "", "", nil},
		{"only junk", "!!!!\r\n", "", []Code{CodeCTEBase64Junk}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeTolerant(t, "base64", tc.wire); got != tc.want {
				t.Errorf("legacy entry: %q, want %q", got, tc.want)
			}
			got, codes, err := parseBodyCodes(t, nil, "base64", tc.wire)
			if got != tc.want || err != nil {
				t.Errorf("Parse: (%q, %v), want %q", got, err, tc.want)
			}
			if !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("diagnostics %v, want %v", codes, tc.codes)
			}
		})
	}
}

// TestTolerantBase64_matchesStdlib pins that every conformant encoding decodes
// to what the standard library decodes it to, at every length and wrap.
func TestTolerantBase64_matchesStdlib(t *testing.T) {
	data := make([]byte, 600)
	for i := range data {
		data[i] = byte(i*131 + 7)
	}
	for n := 0; n <= len(data); n += 7 {
		enc := base64.StdEncoding.EncodeToString(data[:n])
		var wrapped strings.Builder
		for i := 0; i < len(enc); i += 76 {
			wrapped.WriteString(enc[i:min(i+76, len(enc))])
			wrapped.WriteString("\r\n")
		}
		for _, wire := range []string{enc, wrapped.String()} {
			got, codes, err := parseBodyCodes(t, nil, "base64", wire)
			if got != string(data[:n]) || codes != nil || err != nil {
				t.Fatalf("n=%d: decoded %d bytes, diagnostics %v, err %v", n, len(got), codes, err)
			}
		}
	}
}

func TestTolerantBase64_strictKeepsCorruption(t *testing.T) {
	s, e, err := Parse(strings.NewReader("Content-Transfer-Encoding: base64\r\n\r\naGVs!bG8g"), &ParseOptions{Profile: ProfileStrict})
	if err != nil {
		t.Fatal(err)
	}
	b, rerr := io.ReadAll(e.Body)
	var corrupt base64.CorruptInputError
	if string(b) != "hel" || !asCorrupt(rerr, &corrupt) || s.Len() != 0 {
		t.Errorf("strict: (%q, %v, %v), want the bytes before the corruption and its error", b, rerr, s.Diagnostics())
	}
}

func asCorrupt(err error, target *base64.CorruptInputError) bool {
	c, ok := err.(base64.CorruptInputError)
	if ok {
		*target = c
	}
	return ok
}

func TestTolerantQP(t *testing.T) {
	tests := []struct {
		name, wire, want string
		codes            []Code
	}{
		{"conformant", "caf=C3=A9 =3D ok\r\n", "caf\xc3\xa9 = ok\r\n", nil},
		{"lower case hex", "=c3=a9", "\xc3\xa9", nil},
		{"soft break", "foo=\r\nbar", "foobar", nil},
		{"soft break bare LF", "foo=\nbar", "foobar", nil},
		{"transport padding", "foo  \t\r\nbar \r\n", "foo\r\nbar\r\n", nil},
		{"encoded trailing space", "foo=20\r\n", "foo \r\n", nil},
		{"8-bit passes", "caf\xe9\r\n", "caf\xe9\r\n", nil},
		{"bare LF kept", "a\nb\n", "a\nb\n", nil},
		{"bare CR kept", "a\rb\r\n", "a\rb\r\n", nil},
		{"soft break CR only", "foo=\rbar\r\n", "foobar\r\n", []Code{CodeCTEQPInvalidSoftBreak}},
		// Transport padding before the line ending (RFC 2045 section 6.7 rule 3).
		{"soft break trailing space", "foo= \t\r\nbar", "foobar", nil},
		{"soft break trailing space bare LF", "foo= \nbar", "foobar", nil},
		{"invalid hex", "a=ZZb =4 =\x80\r\n", "a=ZZb =4 =\x80\r\n", []Code{CodeCTEQPInvalidHex}},
		{"half escape", "a=4\r\n", "a=4\r\n", []Code{CodeCTEQPInvalidHex}},
		{"double equals", "a==41\r\n", "a=A\r\n", []Code{CodeCTEQPInvalidHex}},
		{"equals then space then text", "a= b\r\n", "a= b\r\n", []Code{CodeCTEQPInvalidHex}},
		{"trailing equals", "abc=", "abc=", []Code{CodeCTEQPTrailingEquals}},
		{"trailing half escape", "abc=4", "abc=4", []Code{CodeCTEQPInvalidHex}},
		{"control byte", "a\x00b\x01\r\n", "a\x00b\x01\r\n", []Code{CodeCTEQPControlByte}},
		{"overlong line", strings.Repeat("x", 2000) + "\r\n", strings.Repeat("x", 2000) + "\r\n", []Code{CodeCTEQPOverlongLine}},
		{"long white space run", "a" + strings.Repeat(" ", 3000) + "b", "a" + strings.Repeat(" ", 3000) + "b", nil},
		{"empty", "", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeTolerant(t, "quoted-printable", tc.wire); got != tc.want {
				t.Errorf("legacy entry: %q, want %q", got, tc.want)
			}
			got, codes, err := parseBodyCodes(t, nil, "quoted-printable", tc.wire)
			if got != tc.want || err != nil {
				t.Errorf("Parse: (%q, %v), want %q", got, err, tc.want)
			}
			if !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("diagnostics %v, want %v", codes, tc.codes)
			}
		})
	}
}

func TestTolerantUUEncode(t *testing.T) {
	// "Cat" and a 45+3 byte payload, as uuencode writes them.
	const cat = "begin 644 cat.txt\n#0V%T\n`\nend\n"
	long := strings.Repeat("0123456789", 6)
	var enc strings.Builder
	enc.WriteString("begin 600 long\r\n")
	for i := 0; i < len(long); i += 45 {
		chunk := long[i:min(i+45, len(long))]
		enc.WriteByte(byte(' ' + len(chunk)))
		for j := 0; j < len(chunk); j += 3 {
			var g [3]byte
			copy(g[:], chunk[j:min(j+3, len(chunk))])
			for _, v := range []byte{g[0] >> 2, (g[0]<<4 | g[1]>>4) & 63, (g[1]<<2 | g[2]>>6) & 63, g[2] & 63} {
				if v == 0 {
					enc.WriteByte('`')
				} else {
					enc.WriteByte(' ' + v)
				}
			}
		}
		enc.WriteString("\r\n")
	}
	enc.WriteString("`\r\nend\r\n")

	tests := []struct {
		name, cte, wire, want string
		codes                 []Code
	}{
		{"conformant", "x-uuencode", cat, "Cat", nil},
		{"alias and CRLF", "uuencode", enc.String(), long, []Code{CodeCTEInvalidToken}},
		{"text before begin and after end", "x-uue", "see attached\n\n" + cat + "trailer\n", "Cat", []Code{CodeCTEInvalidToken}},
		{"missing begin", "x-uuencode", "#0V%T\n`\nend\n", "Cat", []Code{CodeCTEUUEncodeMissingBegin}},
		{"missing end", "x-uuencode", "begin 644 cat.txt\n#0V%T\n", "Cat", []Code{CodeCTEUUEncodeMissingEnd}},
		{"stripped trailing space", "x-uuencode", "begin 644 a\n!80\nend\n", "a", nil},
		{"line with a byte out of range", "x-uuencode", "begin 644 a\n#0V%\x7f\nend\n", "Ca_", []Code{CodeCTEUUEncodeInvalid}},
		{"no data at all", "x-uuencode", "nothing here\n", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeTolerant(t, tc.cte, tc.wire); got != tc.want {
				t.Errorf("legacy entry: %q, want %q", got, tc.want)
			}
			got, codes, err := parseBodyCodes(t, nil, tc.cte, tc.wire)
			if got != tc.want || err != nil {
				t.Errorf("Parse: (%q, %v), want %q", got, err, tc.want)
			}
			if !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("diagnostics %v, want %v", codes, tc.codes)
			}
		})
	}
}

func TestCanonicalCTE(t *testing.T) {
	tests := []struct {
		raw, name string
		repaired  bool
	}{
		{"base64", "base64", false},
		{" BASE64 ", "base64", false},
		{"Quoted-Printable", "quoted-printable", false},
		{"base64 (as usual)", "base64", false},
		{"(first (nested)) 7bit", "7bit", false},
		{"", "", false},
		{"8bit", "8bit", false},
		{"binary", "binary", false},
		{`"base64"`, "base64", true},
		{"base64;", "base64", true},
		{"base64; charset=utf-8", "base64", true},
		{"quoted-printable foo", "quoted-printable", true},
		{"quoted printable", "quoted-printable", true},
		{"7-bit", "7bit", true},
		{"8bits", "8bit", true},
		{"base-64", "base64", true},
		{"QP", "quoted-printable", true},
		{"uuencode", "x-uuencode", true},
		{"=?us-ascii?Q?base64?=", "base64", true},
		{"=?utf-8?B?YmFzZTY0?=", "base64", true},
		{"x-unknown", "x-unknown", false},
		{"base64 (unclosed", "base64", true},
		{"gzip; base64", "gzip; base64", false},
	}
	for _, tc := range tests {
		name, repaired := canonicalCTE(tc.raw)
		if name != tc.name || repaired != tc.repaired {
			t.Errorf("canonicalCTE(%q) = (%q, %v), want (%q, %v)", tc.raw, name, repaired, tc.name, tc.repaired)
		}
	}
	// Arbitrary and long values are answered without a panic.
	for _, raw := range []string{strings.Repeat("(", 5000), strings.Repeat("=?", 3000), "\x00\xff(", "((((" + strings.Repeat(")", 9), strings.Repeat("a", 1<<16)} {
		canonicalCTE(raw)
	}
}

func TestParse_cteTokenRepair(t *testing.T) {
	got, codes, err := parseBodyCodes(t, nil, `"Base64"; x`, "aGk=")
	if got != "hi" || err != nil || !reflect.DeepEqual(codes, []Code{CodeCTEInvalidToken}) {
		t.Errorf("tolerant: (%q, %v, %v), want hi and cte.invalid_token", got, err, codes)
	}
	// ProfileStrict repairs nothing: the value is unknown and the body raw.
	got, codes, err = parseBodyCodes(t, &ParseOptions{Profile: ProfileStrict}, `"Base64"; x`, "aGk=")
	if got != "aGk=" || !IsUnknownEncoding(err) || !reflect.DeepEqual(codes, []Code{CodeCTEUnknown}) {
		t.Errorf("strict: (%q, %v, %v), want the raw body and an unknown encoding", got, err, codes)
	}
	// A comment is syntax, not a repair, under every profile.
	for _, opts := range []*ParseOptions{nil, {Profile: ProfileStrict}} {
		got, codes, err = parseBodyCodes(t, opts, "base64 (RFC 2045)", "aGk=")
		if got != "hi" || err != nil || codes != nil {
			t.Errorf("comment: (%q, %v, %v), want hi and nothing recorded", got, err, codes)
		}
	}
	// An unknown mechanism keeps the raw body and is a warning, as before
	// (ROB-070-002).
	got, codes, err = parseBodyCodes(t, nil, "x-rot13", "uryyb")
	if got != "uryyb" || !IsUnknownEncoding(err) || !reflect.DeepEqual(codes, []Code{CodeCTEUnknown}) {
		t.Errorf("unknown: (%q, %v, %v)", got, err, codes)
	}
}

func TestParse_sevenBitViolation(t *testing.T) {
	got, codes, err := parseBodyCodes(t, nil, "7bit", "plain\r\ncaf\xe9\x00\r\n")
	if got != "plain\r\ncaf\xe9\x00\r\n" || err != nil || !reflect.DeepEqual(codes, []Code{CodeCTESevenBitViolation, CodeBodyNUL}) {
		t.Errorf("(%q, %v, %v), want every byte, one cte.seven_bit_violation and the NUL's body.nul", got, err, codes)
	}
	s, e, _ := Parse(strings.NewReader("Content-Transfer-Encoding: 7bit\r\n\r\nab\xff"), nil)
	io.ReadAll(e.Body)
	if d := s.Diagnostics(); len(d) != 1 || !d[0].Location.HasOffset || d[0].Location.Offset != int64(len("Content-Transfer-Encoding: 7bit\r\n\r\nab")) {
		t.Errorf("diagnostics %v, want the offset of the first violating byte", d)
	}
	// Clean 7bit, 8bit and undeclared bodies record nothing.
	for _, tc := range [][2]string{{"7bit", "clean\r\n"}, {"8bit", "caf\xe9"}, {"binary", "\x00\xff"}} {
		if _, codes, _ := parseBodyCodes(t, nil, tc[0], tc[1]); codes != nil {
			t.Errorf("%s: diagnostics %v, want none", tc[0], codes)
		}
	}
}

func TestParse_multipartIllegalTransferEncoding(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\npart\r\n--B--\r\n"
	s, e, err := Parse(strings.NewReader(text), nil)
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	p, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(p.Body)
	if string(b) != "part" {
		t.Errorf("part body %q, want the part framed from the raw body", b)
	}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeMultipartIllegalTransferEncoding}) {
		t.Errorf("diagnostics %v, want multipart.illegal_transfer_encoding", codes)
	}
	// 7bit, 8bit and binary are what a multipart may declare.
	for _, cte := range []string{"7bit", "8BIT", "binary"} {
		s, _, _ := Parse(strings.NewReader(strings.Replace(text, "base64", cte, 1)), nil)
		if s.Len() != 0 {
			t.Errorf("%s: diagnostics %v, want none", cte, s.Diagnostics())
		}
	}
}

func TestParse_messageRFC822TransferEncoding(t *testing.T) {
	inner := "Subject: inner\r\n\r\nbody"
	text := "Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(inner))
	s, e, err := Parse(strings.NewReader(text), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(e.Body)
	if string(b) != inner {
		t.Errorf("body %q, want the decoded message", b)
	}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeMessageIllegalTransferEncoding}) {
		t.Errorf("diagnostics %v", codes)
	}
}

// TestTolerantDecoders_hostile feeds each decoder bytes of every kind and
// checks only that it ends, without a panic, having read its whole input.
func TestTolerantDecoders_hostile(t *testing.T) {
	var all bytes.Buffer
	for i := 0; i < 4; i++ {
		for c := 0; c < 256; c++ {
			all.WriteByte(byte(c))
			all.WriteByte(byte(c * 7))
		}
	}
	inputs := []string{
		all.String(),
		strings.Repeat("=", 10000),
		strings.Repeat("=\r", 5000),
		strings.Repeat("= ", 5000),
		strings.Repeat("A=", 5000),
		strings.Repeat("\r", 10000),
		strings.Repeat("M", 100000),
		strings.Repeat("begin \n", 2000),
		strings.Repeat("`\n", 5000),
	}
	for _, enc := range []string{"base64", "quoted-printable", "x-uuencode", "7bit"} {
		for i, in := range inputs {
			for _, src := range []io.Reader{strings.NewReader(in), iotest.OneByteReader(strings.NewReader(in)), iotest.HalfReader(strings.NewReader(in))} {
				s, e, err := Parse(io.MultiReader(strings.NewReader("Content-Transfer-Encoding: "+enc+"\r\n\r\n"), src), nil)
				if err != nil {
					t.Fatalf("%s/%d: Parse = %v", enc, i, err)
				}
				n, err := io.Copy(io.Discard, e.Body)
				if err != nil || n > int64(len(in))+16 {
					t.Errorf("%s/%d: read %d bytes of %d, err %v", enc, i, n, len(in), err)
				}
				if s.Len() > int(cteNoteCount) {
					t.Errorf("%s/%d: %d diagnostics, want each kind once", enc, i, s.Len())
				}
			}
		}
	}
}

// TestTolerantDecoders_failureAddsNothing pins that a body whose input fails
// yields what whole groups decoded to, the failure itself, and no diagnostic
// of the unended line the failure cut.
func TestTolerantDecoders_failureAddsNothing(t *testing.T) {
	for _, tc := range []struct{ cte, wire, want string }{
		{"base64", "aGVsbG8h\r\n!aGk", "hello!"},
		{"quoted-printable", "foo=Z\x00", "foo=Z\x00"},
		{"x-uuencode", "begin 644 a\n#0V%T\n#0V", "Cat"},
	} {
		text := "Content-Transfer-Encoding: " + tc.cte + "\r\n\r\n" + tc.wire
		s, e, err := Parse(&failingSource{s: text, err: errCut}, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, rerr := io.ReadAll(e.Body)
		if string(b) != tc.want || rerr != errCut || s.Len() != 0 {
			t.Errorf("%s: (%q, %v, %v), want (%q, the failure, no diagnostics)", tc.cte, b, rerr, codesOfDiags(s.Diagnostics()), tc.want)
		}
	}
}
