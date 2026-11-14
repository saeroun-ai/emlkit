package message

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// recovered is what a healed parse of a single-entity message yields.
type recovered struct {
	fields []string
	body   string
	codes  []Code
	damage []string
}

func recoverOf(t *testing.T, src io.Reader, opts *ParseOptions) recovered {
	t.Helper()
	s, e, err := Parse(src, opts)
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v), want a healed entity", e != nil, err)
	}
	var r recovered
	f := e.Header.Fields()
	for f.Next() {
		r.fields = append(r.fields, f.Key()+"="+f.Value())
	}
	b, rerr := io.ReadAll(e.Body)
	if rerr != nil {
		t.Fatalf("reading the body = %v", rerr)
	}
	r.body = string(b)
	r.codes = codesOfDiags(s.Diagnostics())
	for _, o := range e.Header.OrphanContinuations() {
		r.damage = append(r.damage, string(o.Bytes()))
	}
	return r
}

func TestParse_headerRecovery(t *testing.T) {
	tests := []struct {
		name, text string
		want       recovered
	}{
		{
			name: "missing separator",
			text: "Subject: hi\r\nTo: a@example.com\r\nHello Bob,\r\nthis is the body\r\n\r\nbye\r\n",
			want: recovered{
				fields: []string{"Subject=hi", "To=a@example.com"},
				body:   "Hello Bob,\r\nthis is the body\r\n\r\nbye\r\n",
				codes:  []Code{CodeHeaderMissingBodySeparator},
			},
		},
		{
			name: "missing separator before html",
			text: "Subject: hi\r\n<html>\r\n<body style=\"font-family: Arial\">x</body>\r\n</html>\r\n",
			want: recovered{
				fields: []string{"Subject=hi"},
				body:   "<html>\r\n<body style=\"font-family: Arial\">x</body>\r\n</html>\r\n",
				codes:  []Code{CodeHeaderMissingBodySeparator},
			},
		},
		{
			name: "no header at all",
			text: "just some text\r\nand more\r\n",
			want: recovered{body: "just some text\r\nand more\r\n", codes: []Code{CodeHeaderMissingBodySeparator}},
		},
		{
			name: "body line at the end of the input",
			text: "Subject: hi\r\nno newline",
			want: recovered{fields: []string{"Subject=hi"}, body: "no newline", codes: []Code{CodeHeaderMissingBodySeparator}},
		},
		{
			name: "white space only separator",
			text: "Subject: hi\r\n \t\r\nthe body\r\n",
			// The white space only line is a fold, and unfolding keeps its
			// white space (#63).
			want: recovered{fields: []string{"Subject=hi \t"}, body: "the body\r\n", codes: []Code{CodeHeaderWhitespaceBodySeparator}},
		},
		{
			name: "colonless line inside the header",
			text: "Subject: hi\r\nthis line is damaged\r\nTo: a@example.com\r\nX-Last: 1\r\n\r\nbody\r\n",
			want: recovered{
				fields: []string{"Subject=hi", "To=a@example.com", "X-Last=1"},
				body:   "body\r\n",
				codes:  []Code{CodeHeaderMissingColon},
				damage: []string{"this line is damaged\r\n"},
			},
		},
		{
			name: "two damaged lines before a known field",
			text: "Subject: hi\r\nfirst damaged\r\nsecond damaged\r\nContent-Type: text/html\r\n\r\nbody\r\n",
			want: recovered{
				fields: []string{"Subject=hi", "Content-Type=text/html"},
				body:   "body\r\n",
				codes:  []Code{CodeHeaderMissingColon},
				damage: []string{"first damaged\r\n", "second damaged\r\n"},
			},
		},
		{
			name: "invalid names inside the header",
			text: "From me@example.com Thu Jan  1 00:00:00 2026\r\nSub\xffject: x\r\nBad Name: y\r\nSubject: real\r\n\r\nbody",
			want: recovered{
				fields: []string{"Subject=real"},
				body:   "body",
				codes:  []Code{CodeHeaderInvalidName},
				damage: []string{"From me@example.com Thu Jan  1 00:00:00 2026\r\n", "Sub\xffject: x\r\n", "Bad Name: y\r\n"},
			},
		},
		{
			name: "parameter line that lost its indentation",
			text: "Content-Type: text/plain;\r\ncharset=\"utf-8\"\r\nSubject: hi\r\n\r\nbody",
			want: recovered{
				fields: []string{"Content-Type=text/plain; charset=\"utf-8\"", "Subject=hi"},
				body:   "body",
				codes:  []Code{CodeHeaderUnindentedContinuation},
			},
		},
		{
			name: "unindented parameter as the last header line",
			text: "Subject: hi\r\nContent-Type: text/plain;\r\ncharset=us-ascii\r\n\r\nbody",
			want: recovered{
				fields: []string{"Subject=hi", "Content-Type=text/plain; charset=us-ascii"},
				body:   "body",
				codes:  []Code{CodeHeaderUnindentedContinuation},
			},
		},
		{
			name: "address list that lost its indentation",
			text: "To: a@example.com,\r\nb@example.com,\r\nc@example.com\r\nSubject: hi\r\n\r\nbody",
			want: recovered{
				fields: []string{"To=a@example.com, b@example.com, c@example.com", "Subject=hi"},
				body:   "body",
				codes:  []Code{CodeHeaderUnindentedContinuation},
			},
		},
		{
			name: "received clauses that lost their indentation",
			text: "Received: from a.example\r\nby b.example with ESMTP id 1;\r\nMon, 1 Jan 2024 00:00:00 +0000\r\nSubject: hi\r\n\r\nbody",
			want: recovered{
				fields: []string{"Received=from a.example by b.example with ESMTP id 1; Mon, 1 Jan 2024 00:00:00 +0000", "Subject=hi"},
				body:   "body",
				codes:  []Code{CodeHeaderUnindentedContinuation},
			},
		},
		{
			name: "bare LF",
			text: "Subject: hi\nHello\nworld\n",
			want: recovered{fields: []string{"Subject=hi"}, body: "Hello\nworld\n", codes: []Code{CodeHeaderBareLF, CodeHeaderMissingBodySeparator}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, opts := range []*ParseOptions{nil, {Profile: ProfileForensic}} {
				for name, src := range map[string]io.Reader{
					"whole":    strings.NewReader(tc.text),
					"one byte": iotest.OneByteReader(strings.NewReader(tc.text)),
				} {
					if got := recoverOf(t, src, opts); !reflect.DeepEqual(got, tc.want) {
						t.Errorf("%s: %+v\nwant %+v", name, got, tc.want)
					}
				}
			}
			// ProfileStrict heals nothing, and neither does Read.
			if _, e, err := Parse(strings.NewReader(tc.text), &ParseOptions{Profile: ProfileStrict}); e != nil || err == nil {
				t.Errorf("strict: (%v, %v), want no entity and the malformed header", e != nil, err)
			}
			if e, err := Read(strings.NewReader(tc.text)); e != nil || err == nil {
				t.Errorf("Read: (%v, %v), want no entity and the malformed header", e != nil, err)
			}
		})
	}
}

// TestParse_headerRecoveryMultipart pins that a damaged part header no longer
// ends the iteration: the part is healed and its siblings are reached
// (ROB-060-010), and a boundary parameter on a line that lost its indentation
// still frames the parts.
func TestParse_headerRecoveryMultipart(t *testing.T) {
	const text = "Content-Type: multipart/mixed;\r\nboundary=\"B\"\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\nno separator here\r\nsecond line\r\n" +
		"--B\r\nthis part has no header at all\r\n" +
		"--B\r\nContent-Type: text/html\r\ndamaged line\r\nContent-Transfer-Encoding: 7bit\r\n\r\n<b>ok</b>\r\n" +
		"--B--\r\n"
	s, e, err := Parse(strings.NewReader(text), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	werr := e.Walk(func(path []int, p *Entity, err error) error {
		if err != nil {
			return err
		}
		if p.MultipartReader() != nil {
			return nil
		}
		mt, _, _ := p.EffectiveContentType()
		b, rerr := io.ReadAll(p.Body)
		if rerr != nil {
			return rerr
		}
		got = append(got, mt+"|"+string(b))
		return nil
	})
	if werr != nil {
		t.Fatalf("Walk = %v", werr)
	}
	want := []string{
		"text/plain|no separator here\r\nsecond line",
		"text/plain|this part has no header at all",
		"text/html|<b>ok</b>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parts %q\nwant  %q", got, want)
	}
	wantCodes := []Code{CodeHeaderUnindentedContinuation, CodeHeaderMissingBodySeparator, CodeHeaderMissingBodySeparator, CodeHeaderMissingColon}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, wantCodes) {
		t.Errorf("diagnostics %v, want %v", codes, wantCodes)
	}
	// The positions are those of the input.
	for _, d := range s.Diagnostics() {
		if d.Code != CodeHeaderMissingBodySeparator || !d.Location.HasOffset {
			continue
		}
		rest := text[d.Location.Offset:]
		if !strings.HasPrefix(rest, "no separator here") && !strings.HasPrefix(rest, "this part has no header") {
			t.Errorf("header.missing_body_separator at %d: %q", d.Location.Offset, rest[:min(20, len(rest))])
		}
	}
}

// TestParse_headerRecoveryRoundTrip pins that a healed header is written back
// byte for byte, damaged lines and unindented continuations included, and that
// the body that began without a separator follows the empty line a written
// header ends with.
func TestParse_headerRecoveryRoundTrip(t *testing.T) {
	const header = "MIME-Version: 1.0\r\nSubject: hi\r\ndamaged line\r\nContent-Type: text/plain;\r\ncharset=utf-8\r\nX-Last: 1\r\n"
	_, e, err := Parse(strings.NewReader(header+"\r\nbody\r\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := e.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	if out.String() != header+"\r\nbody\r\n" {
		t.Errorf("written %q\nwant    %q", out.String(), header+"\r\nbody\r\n")
	}
}

// TestParse_headerRecoveryHostile checks that damaged header blocks of many
// shapes end in an entity or an ordinary error, never a panic, and that the
// bytes after the header reach the body whole.
func TestParse_headerRecoveryHostile(t *testing.T) {
	lines := []string{"Subject: x", "damaged", "Bad Name: y", ": empty", " indented", "X-A: 1", "a=b", "from x", "", "\xff\xfe", "To: a,", "Content-Type: text/plain;"}
	for i := 0; i < len(lines); i++ {
		for j := 0; j < len(lines); j++ {
			for k := 0; k < len(lines); k++ {
				for _, eol := range []string{"\r\n", "\n"} {
					text := lines[i] + eol + lines[j] + eol + lines[k] + eol + "tail"
					s, e, err := Parse(strings.NewReader(text), nil)
					if e == nil {
						if err == nil {
							t.Fatalf("%q: no entity and no error", text)
						}
						continue
					}
					b, rerr := io.ReadAll(e.Body)
					if rerr != nil {
						t.Fatalf("%q: body = %v", text, rerr)
					}
					if !strings.HasSuffix(string(b), "tail") {
						t.Fatalf("%q: body %q lost the end of the input (%v)", text, b, codesOfDiags(s.Diagnostics()))
					}
				}
			}
		}
	}
}

func TestParse_duplicateSingleton(t *testing.T) {
	tests := []struct {
		name, header string
		codes        []Code
		severity     Severity
		mediaType    string
	}{
		{"none", "Subject: a\r\nReceived: x\r\nReceived: y\r\n", nil, 0, "text/plain"},
		{"subject twice", "Subject: a\r\nSubject: b\r\n", []Code{CodeHeaderDuplicateSingleton}, SeverityInfo, "text/plain"},
		{"same content type twice", "Content-Type: text/html\r\nContent-Type: text/html\r\n", []Code{CodeHeaderDuplicateSingleton}, SeverityInfo, "text/html"},
		{"conflicting content types", "Content-Type: text/html\r\nFrom: a@example.com\r\nContent-Type: text/plain\r\n", []Code{CodeHeaderDuplicateSingleton}, SeverityRecovered, "text/html"},
		{"several", "Date: x\r\nDate: y\r\nFrom: a\r\nFrom: b\r\nMIME-Version: 1.0\r\nMime-version: 1.0\r\n", []Code{CodeHeaderDuplicateSingleton}, SeverityInfo, "text/plain"},
	}
	for _, tc := range tests {
		for _, profile := range []Profile{ProfileTolerant, ProfileStrict} {
			s, e, err := Parse(strings.NewReader(tc.header+"\r\nbody"), &ParseOptions{Profile: profile})
			if err != nil {
				t.Fatalf("%s: Parse() = %v", tc.name, err)
			}
			if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("%s/%v: diagnostics %v, want %v", tc.name, profile, codes, tc.codes)
			}
			if tc.codes != nil && s.Diagnostics()[0].Severity != tc.severity {
				t.Errorf("%s/%v: severity %v, want %v", tc.name, profile, s.Diagnostics()[0].Severity, tc.severity)
			}
			if mt, _, _ := e.EffectiveContentType(); mt != tc.mediaType {
				t.Errorf("%s/%v: media type %q, want %q: the first occurrence is read", tc.name, profile, mt, tc.mediaType)
			}
			// Nothing is removed.
			if n := e.Header.Len(); n != strings.Count(tc.header, "\r\n") {
				t.Errorf("%s/%v: %d fields kept of %d", tc.name, profile, n, strings.Count(tc.header, "\r\n"))
			}
		}
	}
}
