package message

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// obDropOverlong returns diags without header.overlong_line, for tests whose
// fixtures hold long header lines to exercise something else. It returns nil
// when nothing is left.
func obDropOverlong(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Code != CodeHeaderOverlongLine {
			out = append(out, d)
		}
	}
	return out
}

// obParseAll parses input with opts, reads every leaf body to its end, and
// returns the session's diagnostics rendered as severity, code, path, location
// and field.
func obParseAll(t *testing.T, r io.Reader, opts *ParseOptions) []string {
	t.Helper()
	s, e, err := Parse(r, opts)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	_ = e.Walk(func(_ []int, p *Entity, _ error) error {
		if p.MultipartReader() == nil {
			_, _ = io.Copy(io.Discard, p.Body)
		}
		return nil
	})
	var out []string
	for _, d := range s.Diagnostics() {
		out = append(out, fmt.Sprintf("%s %s %v %s %s", d.Severity, d.Code, d.Path, d.Location, d.Field))
	}
	return out
}

// TestObservations pins the observations of ob_observe.go and the transfer
// decoding changes of the same round: each case, read whole and one byte at a
// time, records exactly the codes listed, in order.
func TestObservations(t *testing.T) {
	const h = "Content-Type: text/plain\r\n"
	long := strings.Repeat("x", 1200)
	mp := func(parts ...string) string {
		return "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n" + strings.Join(parts, "\r\n--B\r\n")
	}
	tests := []struct {
		name  string
		input string
		opts  *ParseOptions
		want  []string // codes, in order
		check func(t *testing.T, diags []string)
	}{
		{name: "NUL in 8bit text", input: h + "Content-Transfer-Encoding: 8bit\r\n\r\nseen\x00hidden\x00more\r\n",
			want: []string{"body.nul"},
			check: func(t *testing.T, d []string) {
				// The first NUL: 61 bytes of header and separator, then "seen".
				if !strings.Contains(d[0], "offset=65") {
					t.Errorf("location %q, want the first NUL at offset 65", d[0])
				}
			}},
		{name: "NUL with no transfer encoding", input: "Subject: x\r\n\r\na\x00b", want: []string{"body.nul"}},
		{name: "NUL in 7bit text", input: h + "Content-Transfer-Encoding: 7bit\r\n\r\na\x00b\r\n",
			want: []string{"cte.seven_bit_violation", "body.nul"}},
		{name: "NUL in a part", input: mp("Content-Type: text/html\r\n\r\n<p>a\x00b</p>") + "\r\n--B--\r\n",
			want: []string{"body.nul"}},
		{name: "NUL under base64 is the encoding's", input: h + "Content-Transfer-Encoding: base64\r\n\r\nYQBi\r\n"},
		{name: "NUL in binary data", input: "Content-Type: application/octet-stream\r\n\r\na\x00b"},
		{name: "NUL under ProfileStrict", input: h + "\r\na\x00b", opts: &ParseOptions{Profile: ProfileStrict}},
		{name: "no NUL", input: h + "\r\nplain text\r\n"},

		{name: "overlong header line", input: "From: a@example.com\r\nSubject: " + long + "\r\nTo: b@example.com\r\n\r\nbody\r\n",
			want: []string{"header.overlong_line"},
			check: func(t *testing.T, d []string) {
				if !strings.Contains(d[0], "offset=21..1230") || !strings.HasSuffix(d[0], " Subject") {
					t.Errorf("diagnostic %q, want the Subject line at 21..1230", d[0])
				}
			}},
		{name: "two overlong lines, one note", input: "X-A: " + long + "\r\nX-B: " + long + "\r\n\r\n",
			want: []string{"header.overlong_line"}},
		{name: "998 bytes is allowed", input: "Subject: " + strings.Repeat("y", 998-9) + "\r\n\r\n"},
		{name: "999 bytes is not", input: "Subject: " + strings.Repeat("y", 999-9) + "\r\n\r\n", want: []string{"header.overlong_line"}},
		{name: "folded field with a long line", input: "Subject: a\r\n " + long + "\r\n\r\n",
			want: []string{"header.overlong_line"}},
		{name: "folded into short lines", input: "Subject: " + strings.Repeat("word\r\n ", 300) + "end\r\n\r\n"},
		{name: "overlong line in a part header", input: mp("Content-Type: text/plain; name=\""+long+"\"\r\n\r\nx") + "\r\n--B--\r\n",
			want: []string{"header.overlong_line"}},
		{name: "overlong under ProfileStrict", input: "Subject: " + long + "\r\n\r\n", opts: &ParseOptions{Profile: ProfileStrict}},

		{name: "mbox line without the option", input: "From - Wed Nov 11 09:00:00 2026\r\nFrom: a@example.com\r\nSubject: s\r\n\r\nbody\r\n",
			want: []string{"header.invalid_name"}},
		{name: "mbox line with the option", input: "From - Wed Nov 11 09:00:00 2026\r\nFrom: a@example.com\r\nSubject: s\r\n\r\nbody\r\n",
			opts: &ParseOptions{RecognizeMboxEnvelope: true}, want: []string{"header.mbox_envelope"}},

		{name: "base64 cut by the input in an unclosed multipart",
			input: mp("Content-Transfer-Encoding: base64\r\nContent-Type: application/zip\r\n\r\nUEsDBHBs\r\nQUJ"),
			want:  []string{"cte.base64_truncated", "multipart.close_not_found"}},
		{name: "one character cut by the input",
			input: mp("Content-Transfer-Encoding: base64\r\nContent-Type: application/zip\r\n\r\nUEsDBHBsQ"),
			want:  []string{"cte.base64_truncated", "multipart.close_not_found"}},
		{name: "base64 unpadded before a closing delimiter",
			input: mp("Content-Transfer-Encoding: base64\r\nContent-Type: application/zip\r\n\r\nQUJ") + "\r\n--B--\r\n",
			want:  []string{"cte.base64_padding_missing"}},
		{name: "base64 unpadded before a cut closing delimiter",
			input: mp("Content-Transfer-Encoding: base64\r\nContent-Type: application/zip\r\n\r\nQUJ") + "\r\n--B-",
			want:  []string{"cte.base64_padding_missing", "multipart.close_truncated"}},
		{name: "unpadded root body cannot be told from truncation",
			input: "Content-Type: application/zip\r\nContent-Transfer-Encoding: base64\r\n\r\nQUJ",
			want:  []string{"cte.base64_padding_missing"}},

		{name: "quoted-printable soft break with transport padding",
			input: h + "Content-Transfer-Encoding: quoted-printable\r\n\r\nfoo= \t\r\nbar=  \nbaz\r\n"},
		{name: "quoted-printable soft break ended by a bare CR",
			input: h + "Content-Transfer-Encoding: quoted-printable\r\n\r\nfoo= \rbar\r\n",
			want:  []string{"cte.qp_invalid_soft_break"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			whole := obParseAll(t, strings.NewReader(tt.input), tt.opts)
			bytewise := obParseAll(t, iotest.OneByteReader(strings.NewReader(tt.input)), tt.opts)
			if !reflect.DeepEqual(whole, bytewise) {
				t.Errorf("read whole %q, a byte at a time %q", whole, bytewise)
			}
			var codes []string
			for _, d := range whole {
				codes = append(codes, strings.Fields(d)[1])
			}
			if !reflect.DeepEqual(codes, tt.want) {
				t.Fatalf("codes %q, want %q (%q)", codes, tt.want, whole)
			}
			if tt.check != nil {
				tt.check(t, whole)
			}
		})
	}
}

// TestObservations_text pins what the observations say: no byte of the input
// in the message, and the mbox hint pointing to the option.
func TestObservations_text(t *testing.T) {
	read := func(input string) []Diagnostic {
		s, e, err := Parse(strings.NewReader(input), nil)
		if e == nil {
			t.Fatalf("Parse() = %v", err)
		}
		_, _ = io.Copy(io.Discard, e.Body)
		return s.Diagnostics()
	}
	for _, input := range []string{
		"Subject: " + strings.Repeat("SECRET", 200) + "\r\n\r\n",
		"Content-Type: text/plain\r\n\r\nSECRET\x00SECRET",
	} {
		for _, d := range read(input) {
			if strings.Contains(d.Message, "SECRET") || strings.Contains(d.Action, "SECRET") {
				t.Errorf("%s carries input bytes: %q", d.Code, d.Message)
			}
		}
	}

	d := read("From - Wed Nov 11 09:00:00 2026\r\nFrom: a@example.com\r\n\r\n")
	if len(d) != 1 || d[0].Code != CodeHeaderInvalidName || !strings.Contains(d[0].Message, "mbox From_") ||
		!strings.Contains(d[0].Action, "RecognizeMboxEnvelope") {
		t.Errorf("mbox line: %v", d)
	}
	for _, input := range []string{
		"Bad Name: x\r\nFrom: a@example.com\r\n\r\n",                       // not a From_ line
		"From the meeting: x\r\nFrom: a@example.com\r\n\r\n",               // no timestamp
		"\xef\xbb\xbfFrom - Wed Nov 11 09:00:00 2026\r\nFrom: a@x\r\n\r\n", // after a byte order mark
	} {
		for _, d := range read(input) {
			if d.Code == CodeHeaderInvalidName && (strings.Contains(d.Message, "mbox") || strings.Contains(d.Action, "Mbox")) {
				t.Errorf("%q: hint given: %v", input, d)
			}
		}
	}
	if d := read("From - Wed Nov 11 09:00:00 2026\r\nFrom: a@example.com\r\n\r\n"); !bytes.Contains([]byte(d[0].Message), []byte("1 line")) {
		t.Errorf("the count of damaged lines is lost: %q", d[0].Message)
	}
}
