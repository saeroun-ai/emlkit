package message

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit/textproto"
)

// ucDuplicate is the corpus finding 5 shape: text/plain chosen, a
// multipart/mixed after it whose boundary frames the body.
const ucDuplicate = "From: a@example.com\r\n" +
	"Content-Type: text/plain; charset=us-ascii\r\n" +
	"Content-Type: multipart/mixed; boundary=\"dup-0001\"\r\n" +
	"\r\n" +
	"--dup-0001\r\n" +
	"Content-Type: text/html\r\n" +
	"\r\n" +
	"<p>secret body text</p>\r\n" +
	"--dup-0001\r\n" +
	"Content-Type: application/zip; name=\"a.zip\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"UEsDBA==\r\n" +
	"--dup-0001--\r\n"

// ucRun parses r with opts, reads every leaf body to its end with Drain, and
// returns the session and its diagnostics.
func ucRun(t *testing.T, r io.Reader, opts *ParseOptions) (*ParseSession, []Diagnostic) {
	t.Helper()
	s, e, err := Parse(r, opts)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	_ = e.Drain()
	return s, s.Diagnostics()
}

// ucCodes renders diags as "severity code path".
func ucCodes(diags []Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, fmt.Sprintf("%s %s %v", d.Severity, d.Code, d.Path))
	}
	return out
}

// ucFind returns the header.unselected_multipart diagnostics of diags.
func ucFind(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Code == CodeHeaderUnselectedMultipart {
			out = append(out, d)
		}
	}
	return out
}

// TestUnselectedMultipart_root pins the corpus finding 5 outcome: the first
// Content-Type is read, and one Info note says that the later multipart
// declaration's boundary delimits the body, naming the two media types and
// nothing else, at the first delimiter line, with Ambiguous set.
func TestUnselectedMultipart_root(t *testing.T) {
	s, e, err := Parse(strings.NewReader(ucDuplicate), nil)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	if mt, _, _ := e.Header.ContentType(); mt != "text/plain" {
		t.Fatalf("media type %q, want the first Content-Type, text/plain", mt)
	}
	if e.MultipartReader() != nil {
		t.Fatal("the entity frames parts; the first Content-Type is a leaf")
	}
	if got := ucFind(s.Diagnostics()); len(got) != 0 {
		t.Fatalf("recorded before the body was read: %v", ucCodes(got))
	}
	body, _ := io.ReadAll(e.Body)
	if !bytes.Contains(body, []byte("--dup-0001")) {
		t.Fatalf("body %q, want the whole multipart source as text", body)
	}
	diags := s.Diagnostics()
	want := []string{"recovered header.duplicate_singleton []", "info header.unselected_multipart []"}
	if got := ucCodes(diags); !slices.Equal(got, want) {
		t.Fatalf("diagnostics %v, want %v", got, want)
	}
	d := diags[1]
	header := strings.Index(ucDuplicate, "\r\n\r\n") + 4
	if d.Location.Offset != int64(header) {
		t.Errorf("location %v, want the first delimiter line at offset %d", d.Location, header)
	}
	if d.Field != "Content-Type" {
		t.Errorf("field %q", d.Field)
	}
	for _, s := range []string{`"text/plain"`, `"multipart/mixed"`} {
		if !strings.Contains(d.Message, s) {
			t.Errorf("message %q does not name %s", d.Message, s)
		}
	}
	for _, s := range []string{"dup-0001", "secret", "html", "a.zip", "charset"} {
		if strings.Contains(d.Message, s) || strings.Contains(d.Action, s) {
			t.Errorf("diagnostic carries %q from the input: %q / %q", s, d.Message, d.Action)
		}
	}
	// The Action names the declaration read and the explicit reparse, and
	// claims nothing about which parts the other reading holds.
	if !strings.Contains(d.Action, `"text/plain"`) || !strings.Contains(d.Action, "parse the result again") || strings.Contains(d.Action, "not returned") {
		t.Errorf("action %q", d.Action)
	}
	c := s.Completion()
	if !c.Ambiguous || c.State != CompletionComplete || c.Clean() {
		t.Errorf("completion %+v, want complete and ambiguous", c)
	}
}

// TestUnselectedMultipart_cases runs the look over the boundary and placement
// cases, each read whole and one byte at a time, and pins the codes recorded.
func TestUnselectedMultipart_cases(t *testing.T) {
	const (
		plain = "Content-Type: text/plain\r\n"
		parts = "--B\r\nContent-Type: text/html\r\n\r\n<p>x</p>\r\n--B--\r\n"
	)
	dup := "recovered header.duplicate_singleton []"
	note := "info header.unselected_multipart []"
	long := strings.Repeat("b", 120)
	pad := func(n int) string { return strings.Repeat("x", n-2) + "\r\n" }
	// A delimiter line "--B\r\n" whose line ending is inside the window, its
	// '\n' the window's last byte, and one whose '\r' is the first byte past
	// it. A '\r' ends a line, so its '\n' need not be in the window.
	inWindow := pad(ucWindow-5) + "--B\r\n"
	pastWindow := pad(ucWindow-3) + "--B\r\n"
	mp := func(part string) string {
		return "Content-Type: multipart/mixed; boundary=OUT\r\n\r\n--OUT\r\n" + part + "\r\n--OUT--\r\n"
	}

	tests := []struct {
		name  string
		input string
		opts  *ParseOptions
		want  []string
	}{
		{"framing boundary", plain + "Content-Type: multipart/alternative; boundary=B\r\n\r\n" + parts, nil, []string{dup, note}},
		{"only a closing delimiter", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\ntext\r\n--B--\r\n", nil, []string{dup, note}},
		{"empty alternate multipart", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B--\r\n", nil, []string{dup, note}},
		{"padded closing delimiter", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B-- \t\r\n", nil, []string{dup, note}},
		{"closing delimiter, junk after", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B---\r\n--B--x\r\n--B-- -\r\n", nil, []string{dup}},
		{"name ending in --, opened", plain + "Content-Type: multipart/mixed; boundary=\"x--\"\r\n\r\n--x--\r\n", nil, []string{dup, note}},
		{"name ending in --, closed", plain + "Content-Type: multipart/mixed; boundary=\"x--\"\r\n\r\n--x----\r\n", nil, []string{dup, note}},
		{"name ending in --, other line", plain + "Content-Type: multipart/mixed; boundary=\"x--\"\r\n\r\n--x\r\n--x---\r\n", nil, []string{dup}},
		{"name ending in a space", plain + "Content-Type: multipart/mixed; boundary=\"x \"\r\n\r\n--x\r\n--x--\r\n", nil, []string{dup}},
		{"name ending in a space, delimiter", plain + "Content-Type: multipart/mixed; boundary=\"x \"\r\n\r\n--x \r\n", nil, []string{dup, note}},
		{"padded delimiter line", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B \t\r\nx\r\n", nil, []string{dup, note}},
		{"bare LF line endings", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\ntext\n--B\nx\n", nil, []string{dup, note}},
		{"bare CR line ending", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\ntext\r--B\rx", nil, []string{dup, note}},
		// At the end of the body an unterminated line counts as a closing
		// delimiter only: see TestUnselectedMultipart_eofRule.
		{"last line with no ending", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\ntext\r\n--B", nil, []string{dup}},
		{"closing line with no ending", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\ntext\r\n--B-- ", nil, []string{dup, note}},
		{"last line with no ending, no delimiter", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\ntext\r\n--Bx", nil, []string{dup}},
		{"last line with no ending, past the window", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + pad(ucWindow-2) + "--B", nil, []string{dup}},
		{"absent boundary", plain + "Content-Type: multipart/mixed\r\n\r\n" + parts, nil, []string{dup}},
		{"empty boundary", plain + "Content-Type: multipart/mixed; boundary=\"\"\r\n\r\n" + parts, nil, []string{dup}},
		{"boundary of 120 bytes", plain + "Content-Type: multipart/mixed; boundary=" + long + "\r\n\r\n--" + long + "\r\nx\r\n", nil, []string{dup, note}},
		{"boundary of 120 bytes, closed", plain + "Content-Type: multipart/mixed; boundary=" + long + "\r\n\r\nx\r\n--" + long + "--\r\n", nil, []string{dup, note}},
		{"boundary of 120 bytes, prefix only", plain + "Content-Type: multipart/mixed; boundary=" + long + "x\r\n\r\n--" + long + "\r\n", nil, []string{dup}},
		{"after 70 other fields", plain + strings.Repeat(plain, 70) + "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + parts, nil, []string{dup, note}},
		{"unreadable declaration", plain + "Content-Type: multipart/mixed; boundary=\"B\r\n\r\n" + parts, nil, []string{dup}},
		{"false boundary: longer line", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--Bx\r\n--B-x\r\n", nil, []string{dup}},
		{"false boundary: not at the line start", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\nsee --B\r\n --B\r\n", nil, []string{dup}},
		{"false boundary: other case", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--b\r\n", nil, []string{dup}},
		{"not multipart", plain + "Content-Type: text/html; boundary=B\r\n\r\n" + parts, nil, []string{dup}},
		{"same declaration twice", "Content-Type: text/plain\r\nContent-Type: text/plain\r\n\r\n" + parts, nil, []string{"info header.duplicate_singleton []"}},
		{"one Content-Type", "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + parts, nil, nil},
		{"no Content-Type, delimiter lines", "Subject: s\r\n\r\n" + parts, nil, nil},
		{"delimiter line ends at the window's end", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + inWindow, nil, []string{dup, note}},
		{"delimiter line ends past the window", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + pastWindow, nil, []string{dup}},
		{"leaf under base64", "Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n" +
			"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nQUJD\r\n--B--\r\n", nil,
			[]string{dup, "recovered cte.base64_url_alphabet []", note}},
		{"child part", mp(plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nx"), nil,
			[]string{"recovered header.duplicate_singleton [0]", "info header.unselected_multipart [0]"}},
		{"child part, false boundary", mp(plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B+\r\nx"), nil,
			[]string{"recovered header.duplicate_singleton [0]"}},
		{"chosen multipart, alternate in a part", "Content-Type: multipart/mixed; boundary=A\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n" +
			"--A\r\nContent-Type: text/plain\r\n\r\n--B\r\n\r\nx\r\n--B--\r\n\r\n--A--\r\n", nil, []string{dup, note}},
		{"chosen multipart, same boundary, other subtype", "Content-Type: multipart/mixed; boundary=A\r\nContent-Type: multipart/alternative; boundary=A\r\n\r\n" +
			"--A\r\n\r\nx\r\n--A--\r\n", nil, []string{dup, note}},
		{"chosen multipart declared twice", "Content-Type: multipart/mixed; boundary=A\r\nContent-Type: multipart/mixed; boundary=\"A\"\r\n\r\n" +
			"--A\r\n\r\nx\r\n--A--\r\n", nil, []string{dup}},
		{"chosen multipart, alternate only in the epilogue", "Content-Type: multipart/mixed; boundary=A\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n" +
			"--A\r\n\r\nx\r\n--A--\r\n--B\r\n\r\ny\r\n--B--\r\n", nil, []string{dup}},
		{"chosen multipart, alternate on the closing line's heels", "Content-Type: multipart/mixed; boundary=A\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n" +
			"--A\r\n\r\nx\r\n--A--\r\n--B--", nil, []string{dup}},
		{"chosen multipart, no alternate line", "Content-Type: multipart/mixed; boundary=A\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n" +
			"--A\r\n\r\n--Bx\r\n--A--\r\n", nil, []string{dup}},
		{"chosen multipart, unclosed", "Content-Type: multipart/mixed; boundary=A\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n" +
			"--A\r\n\r\nx\r\n--B\r\n", nil, []string{dup, "incomplete multipart.close_not_found []", note}},
		{"chosen multipart framing nothing", "Content-Type: multipart/mixed; boundary=A\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n" +
			"--B\r\nContent-Type: text/plain\r\n\r\nx\r\n--B--\r\n", nil, []string{dup, "recovered multipart.boundary_mismatch []", note}},
		{"child multipart", mp("Content-Type: multipart/related; boundary=A\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--A\r\n\r\n--B\r\n--A--"), nil,
			[]string{"recovered header.duplicate_singleton [0]", "info header.unselected_multipart [0]"}},
		{"ProfileStrict", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + parts, &ParseOptions{Profile: ProfileStrict}, []string{dup}},
		{"ProfileForensic", plain + "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + parts, &ParseOptions{Profile: ProfileForensic}, []string{dup, note}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, delivery := range []struct {
				name string
				r    io.Reader
			}{
				{"whole", strings.NewReader(tc.input)},
				{"byte at a time", iotest.OneByteReader(strings.NewReader(tc.input))},
				{"end with data", iotest.DataErrReader(strings.NewReader(tc.input))},
				{"reads of 16", ucChunks{strings.NewReader(tc.input), 16}},
			} {
				s, diags := ucRun(t, delivery.r, tc.opts)
				if got := ucCodes(diags); !slices.Equal(got, tc.want) {
					t.Errorf("%s: diagnostics %v, want %v", delivery.name, got, tc.want)
				}
				if found := len(ucFind(diags)) > 0; found && !s.Completion().Ambiguous {
					t.Errorf("%s: not Ambiguous", delivery.name)
				}
			}
		})
	}
}

// ucChunks returns at most n bytes a read.
type ucChunks struct {
	r io.Reader
	n int
}

func (c ucChunks) Read(p []byte) (int, error) {
	return c.r.Read(p[:min(len(p), c.n)])
}

// ucSelected is a multipart the first Content-Type selects, with a later
// multipart/alternative whose boundary delimits lines inside its first part.
const ucSelected = "Content-Type: multipart/mixed; boundary=a\r\n" +
	"Content-Type: multipart/alternative; boundary=b\r\n" +
	"\r\n" +
	"--a\r\nContent-Type: text/plain\r\n\r\n--b\r\n\r\nPRIVATE\r\n--b--\r\n" +
	"\r\n--a--\r\nepilogue\r\n"

// TestUnselectedMultipart_container: a multipart the first Content-Type
// selects is framed as declared, its parts and raw bytes unchanged, and the
// note is recorded once, at the root, when its parts end — or when its raw
// Body is read to its end — and only then.
func TestUnselectedMultipart_container(t *testing.T) {
	firstLine := int64(strings.Index(ucSelected, "--b\r\n"))

	t.Run("iterated", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(ucSelected), nil)
		if e == nil {
			t.Fatalf("Parse() = %v", err)
		}
		mr := e.MultipartReader()
		if mr == nil {
			t.Fatal("the first Content-Type, a multipart, frames no parts")
		}
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(p.Body)
		if string(body) != "--b\r\n\r\nPRIVATE\r\n--b--\r\n" {
			t.Errorf("part body %q", body)
		}
		if got := ucFind(s.Diagnostics()); len(got) != 0 {
			t.Fatalf("recorded before the parts ended: %v", ucCodes(got))
		}
		if _, err := mr.NextPart(); err != io.EOF {
			t.Fatalf("NextPart() = %v, want io.EOF", err)
		}
		_, _ = mr.NextPart()
		got := ucFind(s.Diagnostics())
		if len(got) != 1 || len(got[0].Path) != 0 || got[0].Location.Offset != firstLine {
			t.Fatalf("notes %v at %v, want one at the root at %d", ucCodes(got), got, firstLine)
		}
		for _, w := range []string{`"multipart/mixed"`, `"multipart/alternative"`} {
			if !strings.Contains(got[0].Message, w) {
				t.Errorf("message %q does not name %s", got[0].Message, w)
			}
		}
		if strings.Contains(got[0].Message, "PRIVATE") {
			t.Errorf("message carries body bytes: %q", got[0].Message)
		}
		if c := s.Completion(); !c.Ambiguous || c.State != CompletionComplete {
			t.Errorf("completion %+v", c)
		}
	})

	t.Run("raw body read", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(ucSelected), nil)
		if e == nil {
			t.Fatalf("Parse() = %v", err)
		}
		raw, _ := io.ReadAll(e.Body)
		if want := ucSelected[strings.Index(ucSelected, "\r\n\r\n")+4:]; string(raw) != want {
			t.Errorf("raw body %q, want %q", raw, want)
		}
		if got := ucFind(s.Diagnostics()); len(got) != 1 || got[0].Location.Offset != firstLine {
			t.Errorf("notes %v", ucCodes(got))
		}
	})

	t.Run("verbatim copy", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(ucSelected), nil)
		if e == nil {
			t.Fatalf("Parse() = %v", err)
		}
		var b strings.Builder
		if err := e.WriteTo(&b); err != nil {
			t.Fatal(err)
		}
		// The header is written as WriteTo writes one; the body is the
		// input's, byte for byte.
		if body := ucSelected[strings.Index(ucSelected, "\r\n\r\n")+4:]; !strings.HasSuffix(b.String(), "\r\n\r\n"+body) {
			t.Errorf("WriteTo %q, want the input's body", b.String())
		}
		if got := ucFind(s.Diagnostics()); len(got) != 0 {
			t.Errorf("recorded for a verbatim copy: %v", ucCodes(got))
		}
	})

	t.Run("cancellable", func(t *testing.T) {
		s, e, err := ParseContext(context.Background(), ucChunks{strings.NewReader(ucSelected), 7}, nil)
		if e == nil {
			t.Fatalf("ParseContext() = %v", err)
		}
		if err := e.Drain(); err != nil {
			t.Fatal(err)
		}
		if got := ucFind(s.Diagnostics()); len(got) != 1 || got[0].Location.Offset != firstLine {
			t.Errorf("notes %v", ucCodes(got))
		}
	})

	t.Run("sink stop", func(t *testing.T) {
		stop := errors.New("stop")
		n := 0
		opts := &ParseOptions{DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			if d.Code == CodeHeaderUnselectedMultipart {
				n++
				return stop
			}
			return nil
		})}
		s, e, _ := Parse(strings.NewReader(ucSelected), opts)
		if e == nil {
			t.Fatal("no entity")
		}
		mr := e.MultipartReader()
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, p.Body)
		// The call that ends the parts records the note, and the sink stops
		// the parse: that call returns the stop, not io.EOF.
		_, end := mr.NextPart()
		if end == nil || end == io.EOF {
			t.Fatalf("NextPart() = %v, want the parse's stop", end)
		}
		c := s.Completion()
		if c.Stop == nil || !sameError(end, c.Stop) || !errors.Is(end, stop) {
			t.Errorf("NextPart() = %v, Completion().Stop = %v, want the identical stop wrapping the sink's", end, c.Stop)
		}
		if _, again := mr.NextPart(); !sameError(again, end) {
			t.Errorf("NextPart() again = %v, want the identical stop %v", again, end)
		}
		if err := e.Drain(); !sameError(err, end) {
			t.Errorf("Drain() = %v, want the identical stop %v", err, end)
		}
		if err := e.Drain(); !sameError(err, end) {
			t.Errorf("Drain() again = %v, want the identical stop %v", err, end)
		}
		if n != 1 {
			t.Errorf("sink saw the note %d times", n)
		}
		if c := s.Completion(); c.State != CompletionStopped || !c.Ambiguous {
			t.Errorf("completion %+v, want stopped and ambiguous", c)
		}
	})

	t.Run("sink stop under Drain", func(t *testing.T) {
		stop := errors.New("stop")
		opts := &ParseOptions{DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			if d.Code == CodeHeaderUnselectedMultipart {
				return stop
			}
			return nil
		})}
		s, e, _ := Parse(strings.NewReader(ucSelected), opts)
		if e == nil {
			t.Fatal("no entity")
		}
		err := e.Drain()
		if err == nil || !sameError(err, s.Completion().Stop) || !errors.Is(err, stop) {
			t.Fatalf("Drain() = %v, want the parse's stop %v", err, s.Completion().Stop)
		}
		if again := e.Drain(); !sameError(again, err) {
			t.Errorf("Drain() again = %v, want the identical stop", again)
		}
	})
}

// TestUnselectedMultipart_declarations: every later Content-Type field is
// looked at, however many precede the multipart one; of many qualifying
// declarations one note is recorded, naming the declaration whose delimiter
// line comes first in the body; a boundary declared twice keeps its first
// declaration.
func TestUnselectedMultipart_declarations(t *testing.T) {
	var h strings.Builder
	h.WriteString("Content-Type: text/plain\r\n")
	for i := range 300 {
		fmt.Fprintf(&h, "Content-Type: multipart/x-%d; boundary=b%d\r\n", i, i)
	}
	h.WriteString("Content-Type: multipart/repeat; boundary=b5\r\n\r\n")
	head := h.String()

	tests := []struct {
		name, body, want string // want: the media type named, or "" for none
	}{
		{"first line wins", "--b40\r\nx\r\n--b3\r\n--b0\r\n", "multipart/x-40"},
		{"repeated boundary", "--b5\r\n", "multipart/x-5"},
		{"the last declaration", "--b299--\r\n", "multipart/x-299"},
		{"no declared boundary", "--b300\r\n--repeat\r\n", ""},
		{"several lines of one", strings.Repeat("--b7\r\nx\r\n", 100), "multipart/x-7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ucRun(t, strings.NewReader(head+tc.body), nil)
			got := ucFind(diags)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("recorded %v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("recorded %d notes, want 1", len(got))
			}
			if !strings.Contains(got[0].Message, fmt.Sprintf("%q", tc.want)) {
				t.Errorf("message %q, want it to name %s", got[0].Message, tc.want)
			}
			if got[0].Location.Offset != int64(len(head)) {
				t.Errorf("location %v, want the body's first line at %d", got[0].Location, len(head))
			}
		})
	}
}

// ucTestScan returns a look for the given boundaries, declared in that order
// as multipart/d0, multipart/d1 and so on, as ucWatch builds it.
func ucTestScan(boundaries ...string) *ucScan {
	s := &ucScan{found: -1}
	for i, b := range boundaries {
		s.add(fmt.Sprintf("multipart/d%d", i), b)
	}
	s.prepare()
	return s
}

// TestUnselectedMultipart_splitLines feeds the window to the look in two reads
// split at every byte, and in reads of every size, and requires the same
// answer as one read: a delimiter line split across reads is still seen, and
// the window's end is a byte count, not a read.
func TestUnselectedMultipart_splitLines(t *testing.T) {
	bodies := []string{
		"abc\r\n--B \r\nx",
		"--B\r\n",
		"x\r--B\r\n",
		"--B-\r\n--Bx\r\n--B\n",
		strings.Repeat("y", ucWindow-6) + "\r\n--B\r\n",
		strings.Repeat("y", ucWindow-5) + "\r\n--B\r\n",
		"--" + strings.Repeat("z", ucBoundaryMax) + "\r",
		"--x--\r\n",
		"--y --  \r\n",
	}
	fresh := func() *ucScan {
		return ucTestScan("B", strings.Repeat("z", ucBoundaryMax), "x--", "x", "y ")
	}
	for _, body := range bodies {
		whole := fresh()
		whole.scan([]byte(body))
		for i := 0; i <= len(body); i++ {
			split := fresh()
			split.scan([]byte(body[:i]))
			split.scan([]byte(body[i:]))
			if split.found != whole.found || split.at != whole.at {
				t.Fatalf("body %.20q split at %d: found %d at %d, read whole %d at %d", body, i, split.found, split.at, whole.found, whole.at)
			}
		}
		for size := 1; size <= 7; size++ {
			chunked := fresh()
			for i := 0; i < len(body); i += size {
				chunked.scan([]byte(body[i:min(len(body), i+size)]))
			}
			if chunked.found != whole.found || chunked.at != whole.at {
				t.Fatalf("body %.20q in reads of %d: found %d at %d, read whole %d at %d", body, size, chunked.found, chunked.at, whole.found, whole.at)
			}
		}
	}
	// The cases above that must be found, and where.
	for _, c := range []struct {
		body  string
		found int
		at    int64
	}{
		{bodies[0], 0, 5},
		{bodies[2], 0, 2},
		{bodies[3], 0, 12},
		{bodies[4], 0, ucWindow - 4},
		{bodies[5], -1, 0},
		{bodies[6], 1, 0},
		// "--x--" opens "x--" and closes "x": the declaration given first.
		{bodies[7], 2, 0},
		// A boundary that ends in a space, closed.
		{bodies[8], 4, 0},
	} {
		s := fresh()
		s.scan([]byte(c.body))
		if s.found != c.found || s.at != c.at {
			t.Errorf("body %.20q: found %d at %d, want %d at %d", c.body, s.found, s.at, c.found, c.at)
		}
	}
}

// TestUnselectedMultipart_trailingWhiteSpace: many boundaries that differ only
// in their trailing spaces and tabs share one form; setting them up walks
// each one's trailing white space once (no comparison with the others), a
// boundary declared again keeps its first declaration, and a line is matched
// by walking its own padding.
func TestUnselectedMultipart_trailingWhiteSpace(t *testing.T) {
	const n = 2048 // every pattern of 11 spaces and tabs
	var bs []string
	tail := 0
	for i := range n {
		var b strings.Builder
		b.WriteString("x")
		for bit := range 11 {
			if i>>bit&1 == 1 {
				b.WriteByte('\t')
			} else {
				b.WriteByte(' ')
			}
		}
		bs = append(bs, b.String())
		tail += 11
	}
	bs = append(bs, "x", bs[7], "x") // declared again: ignored
	s := ucTestScan(bs...)
	if len(s.types) != n+1 {
		t.Fatalf("%d declarations kept, want %d", len(s.types), n+1)
	}
	if s.steps != tail+11 {
		t.Errorf("setup walked %d nodes, want %d, the trailing white space declared", s.steps, tail+11)
	}
	if len(s.nodes) > tail+1 {
		t.Errorf("%d nodes for %d bytes of trailing white space", len(s.nodes), tail)
	}

	for _, c := range []struct {
		line  string
		found int
	}{
		// Each pattern opens its own boundary and "x"; the first declared wins.
		{"--" + bs[7] + "\r\n", 7},
		{"--x\r\n", n},
		// Padding past a declared boundary: that boundary, still the first.
		{"--" + bs[5] + "   \r\n", 5},
		// A pattern closed: that pattern alone.
		{"--" + bs[9] + "--\r\n", 9},
		{"--x--\r\n", n},
		{"--x  \t--\r\n", -1},
	} {
		s := ucTestScan(bs...)
		s.scan([]byte(c.line))
		if s.found != c.found {
			t.Errorf("line %q: found %d, want %d", c.line, s.found, c.found)
		}
	}
}

// TestUnselectedMultipart_eofRule: a line the body ends without a line
// ending counts only as a closing delimiter. The healing framing, given the
// later declaration alone, reads such a line the same way: a closing
// delimiter is framing in any place, and an opening one cut by the end of the
// input is preamble when no delimiter line came before it.
func TestUnselectedMultipart_eofRule(t *testing.T) {
	for _, c := range []struct {
		body    string
		framing bool // the healing framing does not keep the last line
	}{
		{"text\r\n--B", false},
		{"--B", false},
		{"--B \t", false},
		{"text\r\n--B--", true},
		{"--B--", true},
		{"text\r\n--B-- \t", true},
		{"text\r\n--B-", false},
		{"text\r\n--Bx", false},
	} {
		last := c.body[strings.LastIndex(c.body, "--B"):]

		// What the framing makes of the body under the later declaration.
		alt := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + c.body
		_, e, err := Parse(strings.NewReader(alt), nil)
		if e == nil {
			t.Fatalf("%q: Parse() = %v", c.body, err)
		}
		mr := e.MultipartReader()
		var kept strings.Builder
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p.Body)
			kept.Write(b)
		}
		pre, _, _ := MultipartPreamble(mr)
		kept.Write(pre)
		if framed := !strings.Contains(kept.String(), last); framed != c.framing {
			t.Errorf("%q: the framing reads the last line as framing: %v, want %v (kept %q)", c.body, framed, c.framing, kept.String())
		}

		// What the look says under the first declaration.
		in := "Content-Type: text/plain\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" + c.body
		_, diags := ucRun(t, strings.NewReader(in), nil)
		if got := len(ucFind(diags)) == 1; got != c.framing {
			t.Errorf("%q: noted %v, want %v", c.body, got, c.framing)
		}
	}
}

// TestUnselectedMultipart_boundedWork: the look reads no more than the window
// of a body, however long, and allocates nothing as it scans.
func TestUnselectedMultipart_boundedWork(t *testing.T) {
	s := ucTestScan("B", strings.Repeat("L", ucBoundaryMax), strings.Repeat("M", ucBoundaryMax+1))
	if cap(s.line) != ucWindow {
		t.Errorf("line prefix of %d bytes, want at most the window, %d", cap(s.line), ucWindow)
	}
	chunk := bytes.Repeat([]byte("--Bx\r\n--"+strings.Repeat("L", 300)+"\r\n"), 1<<4) // no delimiter line
	allocs := testing.AllocsPerRun(10, func() { s.scan(chunk) })
	for range 256 { // 1.5 MiB in all
		s.scan(chunk)
	}
	if s.seen != ucWindow {
		t.Errorf("looked at %d bytes, want the window, %d", s.seen, ucWindow)
	}
	if allocs != 0 {
		t.Errorf("scan allocated %v times a run", allocs)
	}

	// End to end: a long body is read whole and the note is still decided
	// on the window alone.
	long := "Content-Type: text/plain\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		strings.Repeat("filler line\r\n", 1<<14) + "--B\r\nx\r\n"
	_, diags := ucRun(t, strings.NewReader(long), nil)
	if got := ucFind(diags); len(got) != 0 {
		t.Errorf("a delimiter line past the window was reported: %v", ucCodes(got))
	}
}

// TestUnselectedMultipart_sinkStop: a sink that stops the parse on the note
// gets it once and the parse is stopped; a parse a sink stopped before the
// body was read records no note.
func TestUnselectedMultipart_sinkStop(t *testing.T) {
	stop := errors.New("stop")
	for _, at := range []Code{CodeHeaderUnselectedMultipart, CodeHeaderDuplicateSingleton} {
		t.Run(string(at), func(t *testing.T) {
			var seen []Code
			opts := &ParseOptions{DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
				seen = append(seen, d.Code)
				if d.Code == at {
					return stop
				}
				return nil
			})}
			s, e, _ := Parse(strings.NewReader(ucDuplicate), opts)
			if e == nil {
				t.Fatal("no entity")
			}
			_, _ = io.Copy(io.Discard, e.Body)
			// The body has ended either way; Drain reports the parse's stop.
			if err := e.Drain(); err == nil || !sameError(err, s.Completion().Stop) || !errors.Is(err, stop) {
				t.Errorf("Drain() = %v, want the parse's stop %v", err, s.Completion().Stop)
			}
			n := 0
			for _, c := range seen {
				if c == CodeHeaderUnselectedMultipart {
					n++
				}
			}
			want := 1
			if at == CodeHeaderDuplicateSingleton {
				want = 0
			}
			if n != want {
				t.Errorf("sink saw the note %d times, want %d (%v)", n, want, seen)
			}
			if c := s.Completion(); c.State != CompletionStopped {
				t.Errorf("state %v, want stopped", c.State)
			}
		})
	}
}

// TestUnselectedMultipart_unread: a body the caller did not read to its end
// records nothing, and the verbatim WriteTo copy does not feed the look.
func TestUnselectedMultipart_unread(t *testing.T) {
	s, e, err := Parse(strings.NewReader(ucDuplicate), nil)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	buf := make([]byte, 40)
	if _, err := io.ReadFull(e.Body, buf); err != nil {
		t.Fatal(err)
	}
	if got := ucFind(s.Diagnostics()); len(got) != 0 {
		t.Errorf("recorded for a body read in part: %v", ucCodes(got))
	}

	s, e, err = Parse(strings.NewReader(ucDuplicate), nil)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	if err := e.WriteTo(io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := ucFind(s.Diagnostics()); len(got) != 0 {
		t.Errorf("recorded for a verbatim copy: %v", ucCodes(got))
	}
}

// TestUnselectedMultipart_reparse is the procedure header-recovery.md gives
// for reading the unselected declaration: take the entity's raw header and
// body from a Source, remove the Content-Type fields before the one wanted,
// and parse the result again.
func TestUnselectedMultipart_reparse(t *testing.T) {
	in := []byte(ucDuplicate)
	s, e, err := ParseAt(bytes.NewReader(in), int64(len(in)), nil)
	if e == nil {
		t.Fatalf("ParseAt() = %v", err)
	}
	_ = e.Drain()
	if len(ucFind(s.Diagnostics())) != 1 {
		t.Fatalf("diagnostics %v", ucCodes(s.Diagnostics()))
	}

	rh, err := e.RawHeader()
	if err != nil {
		t.Fatal(err)
	}
	h, err := textproto.ReadHeader(bufio.NewReader(rh))
	if err != nil {
		t.Fatal(err)
	}
	fields := h.FieldsByKey("Content-Type")
	fields.Next()
	fields.Del() // the first; the next one is now read
	var head bytes.Buffer
	if err := textproto.WriteHeader(&head, h); err != nil {
		t.Fatal(err)
	}
	rb, err := e.RawBody()
	if err != nil {
		t.Fatal(err)
	}

	s2, e2, err := Parse(io.MultiReader(&head, rb), nil)
	if e2 == nil {
		t.Fatalf("Parse() = %v", err)
	}
	var types []string
	_ = e2.Walk(func(_ []int, p *Entity, _ error) error {
		mt, _, _ := p.Header.ContentType()
		types = append(types, mt)
		if p.MultipartReader() == nil {
			_, _ = io.Copy(io.Discard, p.Body)
		}
		return nil
	})
	if want := []string{"multipart/mixed", "text/html", "application/zip"}; !slices.Equal(types, want) {
		t.Errorf("reparsed tree %v, want %v", types, want)
	}
	if got := ucCodes(s2.Diagnostics()); len(got) != 0 {
		t.Errorf("reparse diagnostics %v", got)
	}
}
