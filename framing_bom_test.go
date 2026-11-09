package message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Byte order marks under Parse, the legacy entry points and the writers
// (ROB-010-003). The framing is textproto's and is pinned there; these tests
// pin what a parse records and refuses for a mark, what an opaque entity is,
// and what the writers do with a header that has a mark. Every source is a
// finite string or a call-capped source.

const bomUTF8 = "\xef\xbb\xbf"

var wideMarks = []textproto.ByteOrderMark{
	textproto.BOMUTF16BE, textproto.BOMUTF16LE, textproto.BOMUTF32BE, textproto.BOMUTF32LE,
}

// markedText is ascii encoded as m names, after its mark.
func markedText(m textproto.ByteOrderMark, ascii string) string {
	var b strings.Builder
	b.Write(m.Bytes())
	for i := 0; i < len(ascii); i++ {
		c := ascii[i]
		switch m {
		case textproto.BOMUTF16LE:
			b.WriteByte(c)
			b.WriteByte(0)
		case textproto.BOMUTF16BE:
			b.WriteByte(0)
			b.WriteByte(c)
		case textproto.BOMUTF32LE:
			b.WriteByte(c)
			b.WriteString("\x00\x00\x00")
		case textproto.BOMUTF32BE:
			b.WriteString("\x00\x00\x00")
			b.WriteByte(c)
		}
	}
	return b.String()
}

// bomGenuine is a message that, encoded as UTF-16 or UTF-32, would declare a
// decoder if its header were read; it is not.
const bomGenuine = "From: a@example.com\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\naGk=\r\n"

type bomRootCase struct {
	name   string
	text   string
	bom    textproto.ByteOrderMark
	fields []string // as readLineEndingTree reports them
	body   string
	// tolerant is what Tolerant and Forensic record, strict what Strict
	// records; Strict refuses every entity whose block began with a mark.
	tolerant, strict []lineEndingFacts
}

func bomNote(severity Severity, start, end int64) lineEndingFacts {
	return lineEndingFacts{CodeInputBOM, severity, nil, start, end}
}

func bomRootCases() []bomRootCase {
	ab := []string{`[] A="1"`, `[] B="2"`}
	recovered := []lineEndingFacts{bomNote(SeverityRecovered, 0, 3)}
	refused := []lineEndingFacts{bomNote(SeverityInvalidMetadata, 0, 3)}
	cases := []bomRootCase{
		{name: "utf8", text: bomUTF8 + "From: a\r\nSubject: s\r\n\r\nbody", bom: textproto.BOMUTF8,
			fields: []string{`[] From="a"`, `[] Subject="s"`}, body: "body", tolerant: recovered, strict: refused},
		{name: "utf8 lf", text: bomUTF8 + "A: 1\nB: 2\n\nbody", bom: textproto.BOMUTF8, fields: ab, body: "body",
			tolerant: []lineEndingFacts{bomNote(SeverityRecovered, 0, 3), {CodeHeaderBareLF, SeverityInfo, nil, 7, 8}},
			strict:   []lineEndingFacts{bomNote(SeverityInvalidMetadata, 0, 3), {CodeHeaderBareLF, SeverityInfo, nil, 7, 8}}},
		{name: "utf8 cr", text: bomUTF8 + "A: 1\rB: 2\r\rbody", bom: textproto.BOMUTF8, fields: ab, body: "body",
			tolerant: []lineEndingFacts{bomNote(SeverityRecovered, 0, 3), {CodeHeaderBareCR, SeverityRecovered, nil, 7, 8}},
			strict:   []lineEndingFacts{bomNote(SeverityInvalidMetadata, 0, 3), {CodeHeaderBareCR, SeverityInvalidMetadata, nil, 7, 8}}},
		{name: "utf8 unterminated", text: bomUTF8 + "A: 1", bom: textproto.BOMUTF8, fields: []string{`[] A="1"`},
			tolerant: []lineEndingFacts{bomNote(SeverityRecovered, 0, 3), {CodeHeaderTruncatedLine, SeverityIncomplete, nil, 3, 7}},
			strict:   []lineEndingFacts{bomNote(SeverityInvalidMetadata, 0, 3), {CodeHeaderTruncatedLine, SeverityInvalidMetadata, nil, 3, 7}}},
		{name: "utf8 mark only", text: bomUTF8, bom: textproto.BOMUTF8, tolerant: recovered, strict: refused},
		{name: "utf8 mark and empty line", text: bomUTF8 + "\r\nbody", bom: textproto.BOMUTF8, body: "body",
			tolerant: recovered, strict: refused},

		// Marks that are content are not looked at.
		{name: "utf8 mark in the body", text: "A: 1\r\n\r\n" + bomUTF8 + "body", fields: []string{`[] A="1"`}, body: bomUTF8 + "body"},
		{name: "utf16le mark in the body", text: "A: 1\r\n\r\n\xff\xfeb\x00", fields: []string{`[] A="1"`}, body: "\xff\xfeb\x00"},
		{name: "utf8 mark in a value", text: "Subject: " + bomUTF8 + "x\r\n\r\nbody",
			fields: []string{fmt.Sprintf("[] Subject=%q", bomUTF8+"x")}, body: "body"},
	}
	for _, m := range wideMarks {
		n := int64(len(m.Bytes()))
		for _, w := range []struct{ name, text string }{
			{"genuine", markedText(m, bomGenuine)},
			{"mark only", string(m.Bytes())},
		} {
			cases = append(cases, bomRootCase{name: fmt.Sprintf("%v %s", m, w.name), text: w.text, bom: m, body: w.text,
				tolerant: []lineEndingFacts{bomNote(SeverityUnsupported, 0, n)},
				strict:   []lineEndingFacts{bomNote(SeverityInvalidMetadata, 0, n)}})
		}
	}
	for _, text := range []string{"\xff\xfe\x00", "\xff\xfeA: 1\r\n\r\nbody"} {
		cases = append(cases, bomRootCase{name: fmt.Sprintf("utf16le %q", text), text: text, bom: textproto.BOMUTF16LE, body: text,
			tolerant: []lineEndingFacts{bomNote(SeverityUnsupported, 0, 2)},
			strict:   []lineEndingFacts{bomNote(SeverityInvalidMetadata, 0, 2)}})
	}
	return cases
}

// checkByteOrderMarkRefusal pins a Strict refusal of an entity for its mark:
// a FatalError carrying a SeverityFatal copy of the recorded input.bom
// diagnostic, the Content-Type not interpreted, no part iteration, and Walk
// ending with the refusal.
func checkByteOrderMarkRefusal(t *testing.T, label string, e *Entity, err error, recorded lineEndingFacts) {
	t.Helper()
	var fe FatalError
	if !IsFatal(err) || !errors.As(err, &fe) {
		t.Fatalf("%s: err = %v, want the entity's FatalError", label, err)
	}
	d, ok := fe.Diagnostic()
	if !ok || d.Code != CodeInputBOM || d.Severity != SeverityFatal || d.Location != LocationRange(recorded.start, recorded.end) {
		t.Errorf("%s: fatal diagnostic = %+v, want input.bom at [%d, %d)", label, d, recorded.start, recorded.end)
	}
	mt, params, terr := e.EffectiveContentType()
	if mt != "" || params != nil || terr != err {
		t.Errorf("%s: EffectiveContentType() = (%q, %v, %v), want nothing interpreted and the refusal", label, mt, params, terr)
	}
	if e.MultipartReader() != nil {
		t.Errorf("%s: MultipartReader() != nil for a refused entity", label)
	}
	if werr := e.Walk(func([]int, *Entity, error) error { return nil }); werr != err {
		t.Errorf("%s: Walk() = %v, want the refusal", label, werr)
	}
}

// checkOpaque pins an entity whose block began with a UTF-16 or UTF-32 mark
// and that was not refused: no media type, not even text/plain, an error that
// is neither a FatalError nor a warning, and no part iteration.
func checkOpaque(t *testing.T, label string, e *Entity) {
	t.Helper()
	mt, params, terr := e.EffectiveContentType()
	if mt != "" || params != nil || terr == nil || IsFatal(terr) || IsUnknownCharset(terr) || IsUnknownEncoding(terr) {
		t.Errorf("%s: EffectiveContentType() = (%q, %v, %v), want no media type and an ordinary error", label, mt, params, terr)
	}
	if e.MultipartReader() != nil {
		t.Errorf("%s: MultipartReader() != nil for an opaque entity", label)
	}
}

// TestParse_byteOrderMark_root pins the root of each case under every profile,
// compatibility and delivery: the diagnostics with their positions, the mark,
// where the body begins, the refusal or its absence, the fields and the body.
func TestParse_byteOrderMark_root(t *testing.T) {
	for _, tc := range bomRootCases() {
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
			for _, compat := range framingCompatibilities {
				for name, src := range lineEndingSources(tc.text) {
					label := fmt.Sprintf("%s/%v/%v/%s", tc.name, profile, compat, name)
					session, e, err := Parse(src(), &ParseOptions{Profile: profile, Compatibility: compat})
					if e == nil {
						t.Fatalf("%s: Parse() = (nil, %v), want the root", label, err)
					}
					want := tc.tolerant
					if profile == ProfileStrict {
						want = tc.strict
					}
					if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
						t.Errorf("%s: diagnostics = %+v, want %+v", label, got, want)
					}
					if e.ParseStop() != nil {
						t.Errorf("%s: ParseStop() = %v, want none: a mark is no stop", label, e.ParseStop())
					}
					if got := e.Header.ByteOrderMark(); got != tc.bom {
						t.Errorf("%s: ByteOrderMark() = %v, want %v", label, got, tc.bom)
					}
					if got, want := e.parse.origin.bodyOffset, int64(len(tc.text)-len(tc.body)); got != want {
						t.Errorf("%s: body offset %d, want %d", label, got, want)
					}
					switch {
					case profile == ProfileStrict && tc.bom != textproto.BOMNone:
						checkByteOrderMarkRefusal(t, label, e, err, want[0])
					case err != nil:
						t.Errorf("%s: Parse() err = %v, want nil", label, err)
					case wideMark(tc.bom):
						checkOpaque(t, label, e)
					}
					tr := readLineEndingTree(e)
					if !reflect.DeepEqual(tr.fields, tc.fields) && !(len(tr.fields) == 0 && len(tc.fields) == 0) {
						t.Errorf("%s: fields = %q, want %q", label, tr.fields, tc.fields)
					}
					if want := []string{fmt.Sprintf("[] %q", tc.body)}; !reflect.DeepEqual(tr.bodies, want) || tr.errs != nil {
						t.Errorf("%s: bodies = %q (%q), want %q", label, tr.bodies, tr.errs, want)
					}
				}
			}
		}
	}
}

// TestParse_byteOrderMark_walk pins that Walk visits an entity whose header
// began with a mark like any leaf under Tolerant: once, with no error.
func TestParse_byteOrderMark_walk(t *testing.T) {
	for _, text := range []string{bomUTF8 + "A: 1\r\n\r\nbody", markedText(textproto.BOMUTF16LE, bomGenuine)} {
		_, e, err := Parse(strings.NewReader(text), nil)
		if err != nil {
			t.Fatalf("%q: Parse() = %v", text, err)
		}
		visits := 0
		werr := e.Walk(func(path []int, p *Entity, err error) error {
			visits++
			if path != nil || p != e || err != nil {
				t.Errorf("%q: walkFunc(%v, %v, %v), want the root", text, path, p == e, err)
			}
			return nil
		})
		if werr != nil || visits != 1 {
			t.Errorf("%q: Walk() = %v after %d visits, want nil after one", text, werr, visits)
		}
	}
}

// TestRead_byteOrderMark pins that Read and ReadWithOptions frame every case
// as Parse does, with no diagnostic and no refusal: they have no session and
// no profile, and an opaque entity is opaque there too.
func TestRead_byteOrderMark(t *testing.T) {
	for _, tc := range bomRootCases() {
		for api, read := range map[string]func(io.Reader) (*Entity, error){
			"Read": Read,
			"ReadWithOptions": func(r io.Reader) (*Entity, error) {
				return ReadWithOptions(r, &ReadOptions{SkipTextAttachmentDecoding: true})
			},
		} {
			label := fmt.Sprintf("%s/%s", tc.name, api)
			e, err := read(strings.NewReader(tc.text))
			if err != nil || e == nil {
				t.Fatalf("%s: (%v, %v), want the entity and no error", label, e, err)
			}
			if got := e.Header.ByteOrderMark(); got != tc.bom {
				t.Errorf("%s: ByteOrderMark() = %v, want %v", label, got, tc.bom)
			}
			if wideMark(tc.bom) {
				checkOpaque(t, label, e)
			}
			tr := readLineEndingTree(e)
			if !reflect.DeepEqual(tr.fields, tc.fields) && !(len(tr.fields) == 0 && len(tc.fields) == 0) {
				t.Errorf("%s: fields = %q, want %q", label, tr.fields, tc.fields)
			}
			if want := []string{fmt.Sprintf("[] %q", tc.body)}; !reflect.DeepEqual(tr.bodies, want) || tr.errs != nil {
				t.Errorf("%s: bodies = %q (%q), want %q", label, tr.bodies, tr.errs, want)
			}
		}
	}
}

// TestNew_byteOrderMark_opaqueCopies pins that an entity built from a copy of a
// header whose block began with a UTF-16 or UTF-32 mark stays opaque whatever
// fields the copy was given: its body is not transfer decoded or converted. A
// copy of a header read after a UTF-8 mark is an ordinary header.
func TestNew_byteOrderMark_opaqueCopies(t *testing.T) {
	_, wide, err := Parse(strings.NewReader("\xff\xfeA\x00"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, utf8, err := Parse(strings.NewReader(bomUTF8+"A: 1\r\n\r\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		header Header
		body   string
		opaque bool
	}{
		{"wide", wide.Header, "QUJD", true},
		{"utf8", utf8.Header, "ABC", false},
	} {
		c := tc.header.Copy()
		c.Set("Content-Type", "text/plain; charset=utf-8")
		c.Set("Content-Transfer-Encoding", "base64")
		if c.ByteOrderMark() != tc.header.ByteOrderMark() {
			t.Errorf("%s: the copy's mark is %v, want %v", tc.name, c.ByteOrderMark(), tc.header.ByteOrderMark())
		}
		e, err := New(c, strings.NewReader("QUJD"))
		if err != nil {
			t.Fatalf("%s: New() = %v", tc.name, err)
		}
		if tc.opaque {
			checkOpaque(t, tc.name, e)
		}
		if body, err := io.ReadAll(e.Body); string(body) != tc.body || err != nil {
			t.Errorf("%s: body (%q, %v), want %q", tc.name, body, err, tc.body)
		}
	}
}

// TestParse_byteOrderMark_malformedUnchanged pins that a partial, repeated or
// misplaced mark is read as the first line, or a later one, exactly as before:
// the malformed header is the error, no entity is returned and nothing is
// recorded. Healing such a line is not part of recognizing a mark.
func TestParse_byteOrderMark_malformedUnchanged(t *testing.T) {
	for _, text := range []string{
		"\xef\xbbA: 1\r\n\r\nbody",
		"\xef\xbb\xbeA: 1\r\n\r\nbody",
		"\xef\xbb",
		"\xffA: 1\r\n\r\nbody",
		"\x00\x00\xff\xfeA: 1\r\n\r\nbody",
		bomUTF8 + bomUTF8 + "A: 1\r\n\r\nbody",
		bomUTF8 + "\xff\xfeA: 1\r\n\r\nbody",
		// A mark followed by white space (bomUTF8 + " A: 1") is no longer an error:
		// the white space begins the block's malformed prefix after the mark
		// (ROB-020-001), pinned with the mark in framing_firstline_test.go.
		"A: 1\r\n" + bomUTF8 + "B: 2\r\n\r\nbody",
	} {
		for _, profile := range []Profile{ProfileTolerant, ProfileStrict, ProfileForensic} {
			session, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
			if e != nil || err == nil || !strings.Contains(err.Error(), "malformed MIME header") || session.Len() != 0 {
				t.Errorf("%q/%v: Parse() = (entity %v, %v, %v), want no entity, the malformed header and nothing recorded",
					text, profile, e != nil, err, codesOfDiags(session.Diagnostics()))
			}
		}
		if e, err := Read(strings.NewReader(text)); e != nil || err == nil || !strings.Contains(err.Error(), "malformed MIME header") {
			t.Errorf("%q: Read() = (%v, %v), want no entity and the malformed header", text, e != nil, err)
		}
	}
}

// bomMultipart is a multipart whose first part's header begins with a UTF-8
// mark, whose second part is UTF-16 text after its mark, and whose third part
// is plain.
func bomMultipart() (text, wide string, at []int64) {
	const head = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	wide = markedText(textproto.BOMUTF16LE, "Content-Type: text/html\r\n\r\nx")
	text = head + "--B\r\n" + bomUTF8 + "Content-Type: text/plain\r\n\r\none\r\n--B\r\n" + wide +
		"\r\n--B\r\nContent-Type: text/plain\r\n\r\nthree\r\n--B--\r\n"
	return text, wide, []int64{int64(len(head) + len("--B\r\n")), int64(strings.Index(text, wide))}
}

// TestParse_byteOrderMark_parts pins marks in part headers under every profile
// and delivery: each block records its own input.bom at the mark in the input
// with the part's path; under Tolerant and Forensic the UTF-8 part is read with
// its fields and the UTF-16 part is an opaque leaf whose body is its every
// byte; under Strict both come back with their own FatalError and raw body
// while the parse goes on to the third part.
func TestParse_byteOrderMark_parts(t *testing.T) {
	text, wide, at := bomMultipart()
	for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
		for name, src := range lineEndingSources(text) {
			label := fmt.Sprintf("%v/%s", profile, name)
			session, e, err := Parse(src(), &ParseOptions{Profile: profile})
			if err != nil || e == nil {
				t.Fatalf("%s: Parse() = (%v, %v)", label, e, err)
			}
			strict := profile == ProfileStrict
			mr := e.MultipartReader()
			for i, w := range []struct {
				bom  textproto.ByteOrderMark
				body string
				mt   string
			}{
				{textproto.BOMUTF8, "one", "text/plain"},
				{textproto.BOMUTF16LE, wide, ""},
				{textproto.BOMNone, "three", "text/plain"},
			} {
				p, err := mr.NextPart()
				if p == nil {
					t.Fatalf("%s: NextPart() %d = (nil, %v), want the part", label, i, err)
				}
				if p.Header.ByteOrderMark() != w.bom {
					t.Errorf("%s: part %d mark %v, want %v", label, i, p.Header.ByteOrderMark(), w.bom)
				}
				mt, _, _ := p.EffectiveContentType()
				switch {
				case i < 2 && strict:
					var fe FatalError
					if !errors.As(err, &fe) {
						t.Fatalf("%s: part %d err = %v, want its own FatalError", label, i, err)
					}
					d, _ := fe.Diagnostic()
					if d.Code != CodeInputBOM || !reflect.DeepEqual(d.Path, []int{i}) || d.Location.Offset != at[i] {
						t.Errorf("%s: part %d refusal = %+v, want input.bom of [%d] at %d", label, i, d, i, at[i])
					}
					if mt != "" {
						t.Errorf("%s: part %d media type %q, want none interpreted", label, i, mt)
					}
				case err != nil:
					t.Errorf("%s: part %d err = %v, want none", label, i, err)
				case mt != w.mt:
					t.Errorf("%s: part %d media type %q, want %q", label, i, mt, w.mt)
				case i == 1:
					checkOpaque(t, label, p)
				}
				if body, berr := io.ReadAll(p.Body); string(body) != w.body || berr != nil {
					t.Errorf("%s: part %d body (%q, %v), want %q", label, i, body, berr, w.body)
				}
			}
			if _, err := mr.NextPart(); err != io.EOF {
				t.Errorf("%s: NextPart() after the last = %v, want io.EOF", label, err)
			}
			if e.ParseStop() != nil {
				t.Errorf("%s: ParseStop() = %v", label, e.ParseStop())
			}
			want := []lineEndingFacts{
				{CodeInputBOM, SeverityRecovered, []int{0}, at[0], at[0] + 3},
				{CodeInputBOM, SeverityUnsupported, []int{1}, at[1], at[1] + 2},
			}
			if strict {
				want[0].severity, want[1].severity = SeverityInvalidMetadata, SeverityInvalidMetadata
			}
			if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: diagnostics = %+v, want %+v", label, got, want)
			}
		}
	}
}

// TestParse_byteOrderMark_strictDiagnosticBudget pins that every part Strict
// refuses for its mark carries its own copy of the diagnostic, whatever
// Limits.MaxDiagnosticCount let the session keep, and that the refusals stop
// nothing.
func TestParse_byteOrderMark_strictDiagnosticBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	var at []int64
	for i := range 3 {
		b.WriteString("--B\r\n")
		at = append(at, int64(b.Len()))
		fmt.Fprintf(&b, "%sX-N: %d\r\n\r\nbody %d\r\n", bomUTF8, i, i)
	}
	b.WriteString("--B--\r\n")
	for _, maxDiags := range []int64{0, 1, 2} {
		opts := &ParseOptions{Profile: ProfileStrict}
		if maxDiags > 0 {
			opts.Limits.MaxDiagnosticCount = maxDiags
		}
		session, e, err := Parse(strings.NewReader(b.String()), opts)
		if err != nil || e == nil {
			t.Fatalf("max %d: Parse() = (%v, %v)", maxDiags, e, err)
		}
		mr := e.MultipartReader()
		for i := range 3 {
			p, err := mr.NextPart()
			var fe FatalError
			if p == nil || !errors.As(err, &fe) {
				t.Fatalf("max %d: part %d = (%v, %v), want the part and its FatalError", maxDiags, i, p != nil, err)
			}
			if d, ok := fe.Diagnostic(); !ok || d.Code != CodeInputBOM || d.Location.Offset != at[i] || !reflect.DeepEqual(d.Path, []int{i}) {
				t.Errorf("max %d: part %d refusal = %+v, want input.bom of [%d] at %d", maxDiags, i, d, i, at[i])
			}
			if body, _ := io.ReadAll(p.Body); string(body) != fmt.Sprintf("body %d", i) {
				t.Errorf("max %d: part %d body %q", maxDiags, i, body)
			}
		}
		if _, err := mr.NextPart(); err != io.EOF || e.ParseStop() != nil {
			t.Errorf("max %d: after the parts (%v, stop %v), want io.EOF and no stop", maxDiags, err, e.ParseStop())
		}
		want := int64(3)
		if maxDiags > 0 {
			want = maxDiags
		}
		if got := int64(len(session.Diagnostics())); got != want {
			t.Errorf("max %d: %d diagnostics, want %d", maxDiags, got, want)
		}
	}
}

// TestParse_byteOrderMark_sinkStop pins that a sink stopping the parse on
// input.bom stops it as from any diagnostic, ahead of a Strict refusal.
func TestParse_byteOrderMark_sinkStop(t *testing.T) {
	errSink := errors.New("sink: enough")
	for _, text := range []string{bomUTF8 + "A: 1\r\n\r\nbody", markedText(textproto.BOMUTF16LE, bomGenuine)} {
		for _, profile := range []Profile{ProfileTolerant, ProfileStrict} {
			sink := DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return errSink })
			session, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile, DiagnosticSink: sink})
			if e == nil || !errors.Is(err, errSink) || session.fatalErr() == nil || error(session.fatalErr()) != err {
				t.Errorf("%q/%v: Parse() = (%v, %v), want the entity and the session's stop", text, profile, e != nil, err)
			}
		}
	}
}

// TestParse_byteOrderMark_budgets pins what the header budgets count under
// Parse: a UTF-8 mark is bytes of the block and of the parse's header total and
// of no line, field or count, refused where its block began; a UTF-16 or
// UTF-32 mark costs no header budget at all, and its whole region is body,
// counted by the body budgets.
func TestParse_byteOrderMark_budgets(t *testing.T) {
	const text = bomUTF8 + "A: 1\r\n\r\nbody" // the block is 11 bytes
	exact := Limits{
		MaxHeaderBytes: 11, MaxLineBytes: 4, MaxHeaderFieldBytes: 6, MaxHeaderCount: 1,
		MaxTotalHeaderBytes: 11, MaxTotalHeaderCount: 1, MaxInputBytes: int64(len(text)),
	}
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: exact})
	if err != nil || e == nil || e.Header.Get("A") != "1" {
		t.Fatalf("exact budgets: (%v, %v)", e, err)
	}
	if body, berr := io.ReadAll(e.Body); string(body) != "body" || berr != nil {
		t.Errorf("exact budgets: body (%q, %v)", body, berr)
	}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputBOM}) {
		t.Errorf("exact budgets: diagnostics %v, want input.bom alone", codes)
	}

	for _, tc := range []struct {
		name   string
		limits Limits
		code   Code
		offset int64
	}{
		{"header bytes", Limits{MaxHeaderBytes: 10}, CodeLimitHeaderBytes, 0},
		{"header bytes inside the mark", Limits{MaxHeaderBytes: 2}, CodeLimitHeaderBytes, 0},
		{"line bytes", Limits{MaxLineBytes: 3}, CodeLimitLineBytes, 3},
		{"field bytes", Limits{MaxHeaderFieldBytes: 5}, CodeLimitHeaderFieldBytes, 3},
		{"total header bytes", Limits{MaxTotalHeaderBytes: 10}, CodeLimitTotalHeaderBytes, 10},
	} {
		s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: tc.limits})
		checkFramingRefusal(t, tc.name, s, e, err, tc.code, tc.offset)
	}

	wide := markedText(textproto.BOMUTF16LE, "A: 1\r\n\r\nbody")
	n := int64(len(wide))
	tight := Limits{
		MaxHeaderBytes: 1, MaxLineBytes: 1, MaxHeaderFieldBytes: 1, MaxHeaderCount: 1,
		MaxTotalHeaderBytes: 1, MaxTotalHeaderCount: 1, MaxInputBytes: n,
		MaxEncodedBodyBytes: n, MaxDecodedBodyBytes: n, MaxTotalDecodedBytes: n,
	}
	session, e, err = Parse(strings.NewReader(wide), &ParseOptions{Limits: tight})
	if err != nil || e == nil || e.Header.ByteOrderMark() != textproto.BOMUTF16LE {
		t.Fatalf("wide under tight header budgets: (%v, %v)", e, err)
	}
	if body, berr := io.ReadAll(e.Body); string(body) != wide || berr != nil {
		t.Errorf("wide: body (%q, %v), want every byte", body, berr)
	}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputBOM}) {
		t.Errorf("wide: diagnostics %v, want input.bom alone", codes)
	}

	session, e, err = Parse(strings.NewReader(wide), &ParseOptions{Limits: Limits{MaxEncodedBodyBytes: n - 1}})
	if err != nil || e == nil {
		t.Fatalf("wide over its body budget: (%v, %v)", e, err)
	}
	if _, berr := io.ReadAll(e.Body); !IsFatal(berr) {
		t.Errorf("wide over its body budget: body ended with %v, want the body budget's refusal", berr)
	}
	if codes := codesOfDiags(session.Diagnostics()); len(codes) < 2 || codes[0] != CodeInputBOM || codes[1] != CodeLimitEncodedBodyBytes {
		t.Errorf("wide over its body budget: diagnostics %v", codes)
	}
}

// TestParse_byteOrderMark_inputErrors cuts the input inside and after a mark:
// a failure before the mark is decided, or inside a block a UTF-8 mark began,
// leaves no entity and is returned as itself with nothing recorded; once a
// UTF-16 or UTF-32 mark is decided the entity is returned, and its body ends
// with the identical failure after the bytes before it. The input is never
// read after its failure.
func TestParse_byteOrderMark_inputErrors(t *testing.T) {
	for _, tc := range []struct {
		text   string
		bom    textproto.ByteOrderMark
		decide int // bytes needed to decide the mark, or the block's length for UTF-8
	}{
		{bomUTF8 + "A: 1\r\n\r\nbody", textproto.BOMUTF8, 11},
		{markedText(textproto.BOMUTF16LE, "A: 1\r\n\r\nbody"), textproto.BOMUTF16LE, 3},
		{markedText(textproto.BOMUTF16BE, "A: 1\r\n\r\nbody"), textproto.BOMUTF16BE, 2},
		{markedText(textproto.BOMUTF32LE, "A: 1"), textproto.BOMUTF32LE, 4},
		{markedText(textproto.BOMUTF32BE, "A: 1"), textproto.BOMUTF32BE, 4},
	} {
		for cut := 0; cut <= len(tc.text); cut++ {
			for _, chunk := range []int{1, 7, 4096} {
				for _, withData := range []bool{false, true} {
					for _, sticky := range []bool{true, false} {
						name := fmt.Sprintf("%v/cut %d/chunk %d/with %v/sticky %v", tc.bom, cut, chunk, withData, sticky)
						src := &ioSource{data: tc.text, cut: cut, chunk: chunk, err: errSourceIO, withData: withData, sticky: sticky}
						s, e, err := Parse(src, nil)
						if cut < tc.decide {
							if e != nil || err != errSourceIO || s.Len() != 0 {
								t.Errorf("%s: Parse() = (entity %v, %v, %v), want no entity, the input's error and nothing recorded",
									name, e != nil, err, codesOfDiags(s.Diagnostics()))
							}
						} else {
							if e == nil || err != nil || e.Header.ByteOrderMark() != tc.bom {
								t.Fatalf("%s: Parse() = (entity %v, %v), want the entity", name, e != nil, err)
							}
							body, berr := io.ReadAll(e.Body)
							start := int(e.parse.origin.bodyOffset)
							if !strings.HasPrefix(tc.text[start:cut], string(body)) || berr != errSourceIO {
								t.Errorf("%s: body (%q, %v), want a prefix of %q and the input's error", name, body, berr, tc.text[start:cut])
							}
						}
						if src.readsAfter != 0 {
							t.Errorf("%s: %d reads after the input failed", name, src.readsAfter)
						}
					}
				}
			}
		}
	}
}

// TestParseContext_byteOrderMark_cancel cancels the parse at every byte of a
// mark and just past it: before the mark is decided, or inside a block a UTF-8
// mark began, no entity is returned and the stop is; once a wide mark is
// decided the entity is returned, with the stop or without it, and its body
// delivers a prefix of the input and then the stop. Nothing is read after the
// cancelling read.
func TestParseContext_byteOrderMark_cancel(t *testing.T) {
	for _, tc := range []struct {
		text   string
		decide int
	}{
		{bomUTF8 + "A: 1\r\n\r\nbody", 11},
		{markedText(textproto.BOMUTF16LE, "A: 1\r\n\r\nbody"), 3},
		{markedText(textproto.BOMUTF32LE, "A: 1"), 4},
	} {
		for at := 1; at <= 6; at++ {
			name := fmt.Sprintf("%q/at %d", tc.text[:4], at)
			ctx, src := newCut(t, tc.text, 1, at, cutNil)
			s, e, err := ParseContext(ctx, src, nil)
			if at < tc.decide {
				if e != nil {
					t.Errorf("%s: an entity, want none", name)
				}
				checkCancelStop(t, name, err, context.Canceled)
			} else {
				if e == nil {
					t.Fatalf("%s: no entity (%v), want the entity", name, err)
				}
				if err != nil && err != s.fatalErr() {
					t.Errorf("%s: Parse() err = %v, want nil or the stop", name, err)
				}
				body, berr := io.ReadAll(e.Body)
				if !strings.HasPrefix(tc.text, string(body)) {
					t.Errorf("%s: body %q, want a prefix of the input", name, body)
				}
				checkCancelStop(t, name, berr, context.Canceled)
			}
			if src.after != 0 {
				t.Errorf("%s: %d reads after the cancelling read", name, src.after)
			}
		}
	}
}

// TestParse_byteOrderMark_noProgress stalls the input inside a mark: one empty
// read fewer than the limit changes nothing, the limit stops the parse there
// with no entity.
func TestParse_byteOrderMark_noProgress(t *testing.T) {
	for _, tc := range []struct {
		text   string
		bom    textproto.ByteOrderMark
		decide int
	}{
		{bomUTF8 + "A: 1\r\n\r\nbody", textproto.BOMUTF8, 3},
		{markedText(textproto.BOMUTF16LE, "A: 1\r\n\r\nbody"), textproto.BOMUTF16LE, 3},
		{markedText(textproto.BOMUTF32BE, "A: 1"), textproto.BOMUTF32BE, 4},
	} {
		for k := 1; k < tc.decide; k++ {
			for _, chunk := range []int{1, 4096} {
				for _, z := range []int{maxConsecutiveEmptyReads - 1, maxConsecutiveEmptyReads} {
					name := fmt.Sprintf("%v/at %d/chunk %d/%d empty reads", tc.bom, k, chunk, z)
					src := newZeroSource(tc.text, chunk, map[int]int{k: z})
					s, e, err := Parse(src, nil)
					if z < maxConsecutiveEmptyReads {
						if err != nil || e == nil || e.Header.ByteOrderMark() != tc.bom {
							t.Errorf("%s: (%v, %v), want the entity and its mark", name, e != nil, err)
						}
						continue
					}
					if e != nil || err == nil || err != s.fatalErr() {
						t.Errorf("%s: (entity %v, %v), want no entity and the stop", name, e != nil, err)
						continue
					}
					if d := noProgressStop(t, err, CodeInputNoProgress); d.Location.Offset != int64(k) {
						t.Errorf("%s: input.no_progress at %v, want offset %d", name, d.Location, k)
					}
					if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputNoProgress}) {
						t.Errorf("%s: diagnostics %v, want one input.no_progress", name, codes)
					}
				}
			}
		}
	}
}

// TestEntity_byteOrderMarkWrite pins the writers. An entity read after a UTF-8
// mark is written with the mark once, before everything, the MIME-Version
// WriteTo adds included, and a header that already had one round-trips byte
// for byte. An entity whose block began with a UTF-16 or UTF-32 mark is refused
// by WriteTo, CreateWriter and CreatePart before anything of it is written —
// through a copy given fields too — and its Body stays whole; a body built by
// NewMultipart reaches it after its header and the parts before it.
func TestEntity_byteOrderMarkWrite(t *testing.T) {
	for in, want := range map[string]string{
		bomUTF8 + "Mime-Version: 1.0\r\nA: 1\r\n\r\nbody": bomUTF8 + "Mime-Version: 1.0\r\nA: 1\r\n\r\nbody",
		bomUTF8 + "A: 1\r\n\r\nbody":                      bomUTF8 + "Mime-Version: 1.0\r\nA: 1\r\n\r\nbody",
		bomUTF8:                                           bomUTF8 + "Mime-Version: 1.0\r\n\r\n",
	} {
		_, e, err := Parse(strings.NewReader(in), nil)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := e.WriteTo(&out); err != nil || out.String() != want {
			t.Errorf("%q: WriteTo() = (%q, %v), want %q", in, out.String(), err, want)
			continue
		}
		_, again, err := Parse(bytes.NewReader(out.Bytes()), nil)
		if err != nil || again.Header.ByteOrderMark() != textproto.BOMUTF8 {
			t.Errorf("%q: reparse = (%v, %v)", in, again, err)
			continue
		}
		var second bytes.Buffer
		if err := again.WriteTo(&second); err != nil || second.String() != want {
			t.Errorf("%q: second WriteTo() = (%q, %v), want %q", in, second.String(), err, want)
		}
	}

	wideIn := markedText(textproto.BOMUTF16LE, bomGenuine)
	parseWide := func() *Entity {
		_, e, err := Parse(strings.NewReader(wideIn), nil)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	checkWhole := func(what string, e *Entity) {
		t.Helper()
		if body, err := io.ReadAll(e.Body); string(body) != wideIn || err != nil {
			t.Errorf("%s: body afterwards (%q, %v), want every byte", what, body, err)
		}
	}

	e := parseWide()
	var out bytes.Buffer
	if err := e.WriteTo(&out); err == nil || out.Len() != 0 {
		t.Errorf("WriteTo() = (%q, %v), want an error and nothing written", out.String(), err)
	}
	checkWhole("WriteTo", e)

	e = parseWide()
	c := e.Header.Copy()
	c.Set("Content-Type", "text/plain")
	for what, h := range map[string]Header{"header": e.Header, "copy with fields": c} {
		if w, err := CreateWriter(&out, h); err == nil || w != nil || out.Len() != 0 {
			t.Errorf("CreateWriter(%s) = (%v, %v) after %q, want an error and nothing written", what, w != nil, err, out.String())
		}
	}

	var mh, th Header
	mh.Set("Content-Type", "multipart/mixed; boundary=B")
	th.Set("Content-Type", "text/plain")
	mw, err := CreateWriter(&out, mh)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := mw.CreatePart(th)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(pw, "one")
	pw.Close()
	n := out.Len()
	if w, err := mw.CreatePart(e.Header); err == nil || w != nil || out.Len() != n {
		t.Errorf("CreatePart(wide) = (%v, %v) after writing %q, want an error and nothing written", w != nil, err, out.String()[n:])
	}
	pw, err = mw.CreatePart(th)
	if err != nil {
		t.Fatalf("CreatePart after a refused one = %v", err)
	}
	io.WriteString(pw, "three")
	pw.Close()
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasSuffix(got, "--B\r\nContent-Type: text/plain\r\n\r\none\r\n--B\r\nContent-Type: text/plain\r\n\r\nthree\r\n--B--\r\n") {
		t.Errorf("multipart written %q", got)
	}
	checkWhole("CreatePart", e)

	wide := parseWide()
	one, err := New(th, strings.NewReader("one"))
	if err != nil {
		t.Fatal(err)
	}
	me, err := NewMultipart(mh, []*Entity{one, wide})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := me.WriteTo(&out); err == nil || !strings.Contains(out.String(), "\r\n\r\none") || strings.Contains(out.String(), "\xff\xfe") {
		t.Errorf("NewMultipart WriteTo() = (%q, %v), want the part before the refused one and the error", out.String(), err)
	}
	checkWhole("NewMultipart part", wide)

	e = parseWide()
	me, err = NewMultipart(e.Header, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := me.WriteTo(&out); err == nil || out.Len() != 0 {
		t.Errorf("NewMultipart(wide header) WriteTo() = (%q, %v), want an error and nothing written", out.String(), err)
	}
}
