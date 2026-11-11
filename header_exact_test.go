package message

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit/textproto"
)

// hpWritten parses text under the default profile and writes the entity back.
func hpWritten(t *testing.T, text string) (*ParseSession, *Entity, string) {
	t.Helper()
	s, e, err := Parse(strings.NewReader(text), nil)
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v), want an entity", e != nil, err)
	}
	var b bytes.Buffer
	if err := e.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo() = %v", err)
	}
	return s, e, b.String()
}

// TestParse_headerReplay pins what Parse then Entity.WriteTo preserves of an
// unmodified message (ROB-020-007): every byte, whatever ended the header's
// lines, however the block ended and whatever recovery healed in it, as long
// as the header says MIME-Version, which WriteTo otherwise adds.
func TestParse_headerReplay(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       string // what WriteTo writes, when not text
	}{
		{name: "crlf", text: "Mime-Version: 1.0\r\nSubject: x\r\n\r\nbody\r\n"},
		{name: "lf only", text: "MIME-Version: 1.0\nSubject: x\n folded\n\nbody\nmore\n"},
		{name: "cr only", text: "MIME-Version: 1.0\rSubject: x\r folded\r\rbody\rmore\r"},
		{name: "mixed endings", text: "MIME-Version: 1.0\nA: 1\r\nB: 2\r folded\n\r\nbody\n"},
		{name: "folding, white space, case, duplicates",
			text: "mime-VERSION:1.0 \r\nX-ODD  :  v\t\r\nReceived: 2\r\n\t\r\n  by x\r\nX: a\r\nReceived: 1\r\nX: a\r\n\r\nbody"},
		{name: "8-bit value", text: "MIME-Version: 1.0\nSubject: caf\xe9 \xff\n\nbody\n"},
		{name: "unindented continuation",
			text: "MIME-Version: 1.0\nContent-Type: text/plain;\ncharset=utf-8\nTo: a@example.org,\nb@example.org\n\nbody\n"},
		{name: "missing colon line", text: "MIME-Version: 1.0\nthis line has no colon\nSubject: x\n\nbody\n"},
		{name: "invalid name line", text: "MIME-Version: 1.0\r\nBad Name: v\r\nSubject: x\r\n\r\nbody\r\n"},
		{name: "empty name line", text: "MIME-Version: 1.0\r\n: nothing\r\nSubject: x\r\n\r\nbody\r\n"},
		{name: "orphan record", text: "MIME-Version: 1.0\nbroken\n orphan\nSubject: x\n\nbody\n"},
		{name: "multipart, lf",
			text: "MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=b\n\npre\n--b\nContent-Type: text/plain\n\none\n--b--\nepilogue\n"},

		{name: "missing separator", text: "MIME-Version: 1.0\nSubject: x\nthe body begins here\nmore\n"},
		{name: "missing separator after a healed line", text: "MIME-Version: 1.0\r\nContent-Type: text/plain;\r\ncharset=utf-8\r\n<html>\r\n"},
		{name: "header only, no empty line", text: "MIME-Version: 1.0\nSubject: x\n"},
		{name: "header only, unterminated", text: "MIME-Version: 1.0\r\nSubject: x"},

		// By design not a replay: WriteTo adds MIME-Version to a header
		// without one, and the changed header is written with CRLF and ended
		// by an empty line.
		{name: "no MIME-Version", text: "Subject: x\n\nbody\n",
			want: "Mime-Version: 1.0\r\nSubject: x\r\n\r\nbody\n"},
		{name: "no MIME-Version, missing separator", text: "Subject: x\nthe body begins here\n",
			want: "Mime-Version: 1.0\r\nSubject: x\r\n\r\nthe body begins here\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.text
			}
			if _, _, got := hpWritten(t, tc.text); got != want {
				t.Errorf("WriteTo() wrote %q, want %q", got, want)
			}
		})
	}

	// Read and ProfileStrict are as they were: lines are written with CRLF.
	const lf = "MIME-Version: 1.0\nSubject: x\n\nbody\n"
	const crlf = "MIME-Version: 1.0\r\nSubject: x\r\n\r\nbody\n"
	e, err := Read(strings.NewReader(lf))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := e.WriteTo(&b); err != nil || b.String() != crlf {
		t.Errorf("Read then WriteTo wrote %q, %v, want %q", b.String(), err, crlf)
	}
	_, e, err = Parse(strings.NewReader(lf), &ParseOptions{Profile: ProfileStrict})
	if e == nil || err != nil {
		t.Fatalf("strict Parse() = (%v, %v)", e != nil, err)
	}
	b.Reset()
	if err := e.WriteTo(&b); err != nil || b.String() != crlf {
		t.Errorf("strict Parse then WriteTo wrote %q, %v, want %q", b.String(), err, crlf)
	}
}

// TestParse_headerReplaySafe pins what keeps the replay from letting a body
// become header: an unmodified header that no empty line ended, or one a bare
// CR ended, is written as it was read only before the body that was read
// with it. Before any other body, and by CreateWriter, it is written with
// CRLF and ended by an empty line, as it always was.
func TestParse_headerReplaySafe(t *testing.T) {
	for _, text := range []string{
		"MIME-Version: 1.0\nSubject: x\nthe body begins here\n",
		"MIME-Version: 1.0\rSubject: x\r\rbody",
	} {
		const want = "MIME-Version: 1.0\r\nSubject: x\r\n\r\n"
		_, e, err := Parse(strings.NewReader(text), nil)
		if e == nil || err != nil {
			t.Fatalf("Parse(%q) = (%v, %v)", text, e != nil, err)
		}
		var b bytes.Buffer
		w, err := CreateWriter(&b, e.Header)
		if err != nil || w.Close() != nil || b.String() != want {
			t.Errorf("%q: CreateWriter() wrote %q, %v, want %q", text, b.String(), err, want)
		}
		const injected = "\nInjected: yes\r\n\r\nx"
		e.Body = strings.NewReader(injected)
		b.Reset()
		if err := e.WriteTo(&b); err != nil || b.String() != want+injected {
			t.Errorf("%q: WriteTo() with another body wrote %q, %v, want %q", text, b.String(), err, want+injected)
		}
	}
}

// TestParse_headerReplayAsRead pins the same replay one layer down, by
// textproto.WriteHeaderAsRead, for a header without MIME-Version.
func TestParse_headerReplayAsRead(t *testing.T) {
	const header = "Subject: x\r\nTo: a@example.org\r\n"
	const body = "the body begins here\r\nmore\r\n"
	s, e, err := Parse(strings.NewReader(header+body), nil)
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v)", e != nil, err)
	}
	if got := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeHeaderMissingBodySeparator}) {
		t.Errorf("Diagnostics() = %v", got)
	}
	var b bytes.Buffer
	if err := textproto.WriteHeaderAsRead(&b, e.Header.Header); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadFrom(e.Body); err != nil {
		t.Fatal(err)
	}
	if b.String() != header+body {
		t.Errorf("replayed %q, want %q", b.String(), header+body)
	}
}

// TestParse_headerEmptyName pins ROB-020-006 at the message layer: a line
// with an empty name is kept apart byte for byte, reported, and no key.
func TestParse_headerEmptyName(t *testing.T) {
	const text = "MIME-Version: 1.0\r\nSubject: x\r\n: caf\xe9 \r\n;also\r\nTo: a@example.org\r\n\r\nbody\r\n"
	s, e, got := hpWritten(t, text)
	if got != text {
		t.Errorf("WriteTo() wrote %q, want the input", got)
	}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeHeaderEmptyName}) {
		t.Errorf("Diagnostics() = %v, want one %v", codes, CodeHeaderEmptyName)
	}
	var kept []string
	for _, o := range e.Header.OrphanContinuations() {
		kept = append(kept, string(o.Bytes()))
	}
	if want := []string{": caf\xe9 \r\n", ";also\r\n"}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept %q, want %q", kept, want)
	}
	if e.Header.Len() != 3 || e.Header.Has("") {
		t.Errorf("an empty name became a field: %v", e.Header.Map())
	}

	// ProfileStrict and Read skip the line, as they always did.
	s, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if e == nil || err != nil || s.Len() != 0 || e.Header.OrphanContinuations() != nil || e.Header.Len() != 3 {
		t.Errorf("strict Parse() = (%v, %v), %d diagnostics", e != nil, err, s.Len())
	}
	if e, err := Read(strings.NewReader(text)); err != nil || e.Header.OrphanContinuations() != nil || e.Header.Len() != 3 {
		t.Errorf("Read() = %v", err)
	}
}

// TestParse_headerRaw8bit pins that a value's 8-bit bytes that are no UTF-8
// stay retrievable byte for byte, while Text gives valid UTF-8.
func TestParse_headerRaw8bit(t *testing.T) {
	const field = "Subject: caf\xe9 \xff\xfe \x80end\r\n"
	const lfField = "X-Lf: \xa4\xa4\n \xb0\n"
	_, e, _ := hpWritten(t, "MIME-Version: 1.0\r\n"+field+lfField+"\r\nbody\r\n")

	if raw, err := e.Header.Raw("Subject"); err != nil || string(raw) != field {
		t.Errorf("Raw() = %q, %v, want %q", raw, err, field)
	}
	if got := e.Header.Get("Subject"); got != "caf\xe9 \xff\xfe \x80end" {
		t.Errorf("Get() = %q", got)
	}
	bad := string(utf8.RuneError)
	text, _ := e.Header.Text("Subject")
	if want := "caf" + bad + " " + bad + bad + " " + bad + "end"; !utf8.ValidString(text) || text != want {
		t.Errorf("Text() = %q, want %q", text, want)
	}
	fields := e.Header.FieldsByKey("Subject")
	if !fields.Next() {
		t.Fatal("no Subject field")
	}
	if raw, err := fields.Raw(); err != nil || string(raw) != field {
		t.Errorf("HeaderFields.Raw() = %q, %v", raw, err)
	}

	if raw, _ := e.Header.Raw("X-Lf"); string(raw) != "X-Lf: \xa4\xa4\r\n \xb0\r\n" {
		t.Errorf("Raw(X-Lf) = %q, want CRLF line endings", raw)
	}
	if raw, _ := e.Header.RawAsRead("X-Lf"); string(raw) != lfField {
		t.Errorf("RawAsRead(X-Lf) = %q, want %q", raw, lfField)
	}
	if got := e.Header.Get("X-Lf"); got != "\xa4\xa4 \xb0" {
		t.Errorf("Get(X-Lf) = %q", got)
	}
}

// hpNoEmptyNameNotes returns diags without its header.empty_name notes, or
// nil when nothing else is left. The header budget fixtures carry lines with
// an empty name to show that the count charges them; a healing parse now
// keeps and reports such a line (ROB-020-006), which is not what those tests
// are about.
func hpNoEmptyNameNotes(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Code != CodeHeaderEmptyName {
			out = append(out, d)
		}
	}
	return out
}
