package message

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Header line endings under Parse (ROB-010-002, phase 1: header blocks). The
// framing itself is textproto's and is pinned there; these tests pin what a
// parse records and refuses for it, under every profile, at the root and in
// parts, and what the entities and their bodies then hold.

// takeBareLFNote checks that diags begin with exactly one header.bare_lf note
// of the root's header block at offset at, and returns the diagnostics after
// it.
func takeBareLFNote(t *testing.T, label string, diags []Diagnostic, at int64) []Diagnostic {
	t.Helper()
	if len(diags) == 0 {
		t.Fatalf("%s: Diagnostics() = none, want header.bare_lf at %d first", label, at)
	}
	d := diags[0]
	if d.Code != CodeHeaderBareLF || d.Severity != SeverityInfo || d.Path != nil ||
		d.Location != LocationRange(at, at+1) || d.Action == "" {
		t.Fatalf("%s: Diagnostics()[0] = %+v, want the root's header.bare_lf at %d", label, d, at)
	}
	return diags[1:]
}

// takeLineNotes checks that diags begin with exactly the line-ending notes a
// fixture written with the line ending eol records once its root header has
// been read — one header.bare_lf at the root's first line ending for LF,
// nothing for CRLF — and returns the diagnostics after them. For LF it then
// takes the root's multipart.bare_lf note, which is there exactly when the
// root is a multipart and reached says the parse framed its first delimiter
// line; see takeMultipartLFNote and rootDelimiterLF. Under CRLF no note can
// appear, and the caller's comparison of the rest catches one that did.
func takeLineNotes(t *testing.T, label, eol, text string, diags []Diagnostic, reached bool) []Diagnostic {
	t.Helper()
	if eol != "\n" {
		return diags
	}
	diags = takeBareLFNote(t, label, diags, int64(strings.IndexByte(text, '\n')))
	at := int64(-1)
	if reached {
		at = rootDelimiterLF(text)
	}
	return takeMultipartLFNote(t, label, diags, at)
}

// takeMultipartLFNote checks the root container's multipart.bare_lf note. A
// negative wantAt requires that diags hold none; otherwise diags must begin
// with exactly one, SeverityInfo, of the root's (nil) path, on the one byte at
// wantAt, which the caller derives from its fixture and from what the parse
// was made to reach, never from what was recorded. It returns the diagnostics
// after the note.
func takeMultipartLFNote(t *testing.T, label string, diags []Diagnostic, wantAt int64) []Diagnostic {
	t.Helper()
	if wantAt < 0 {
		for i, d := range diags {
			if d.Code == CodeMultipartBareLF {
				t.Fatalf("%s: Diagnostics()[%d] = %+v, want no multipart.bare_lf", label, i, d)
			}
		}
		return diags
	}
	if len(diags) == 0 {
		t.Fatalf("%s: Diagnostics() = none, want multipart.bare_lf at %d", label, wantAt)
	}
	d := diags[0]
	if d.Code != CodeMultipartBareLF || d.Severity != SeverityInfo || d.Path != nil ||
		d.Location != LocationRange(wantAt, wantAt+1) || d.Action == "" {
		t.Fatalf("%s: Diagnostics()[0] = %+v, want the root's multipart.bare_lf at %d", label, d, wantAt)
	}
	return diags[1:]
}

// rootDelimiterLF returns where an LF fixture's multipart.bare_lf note is: the
// first line ending the root multipart's reader proves, which is the one
// ending the line before its first delimiter line when a preamble line comes
// before it, and otherwise the first delimiter line's own. It returns -1 when
// the root declares no multipart with a boundary, or has no delimiter line
// ended by a bare LF. The fixture is read as the parse frames it: the header
// ends at the first empty line, and its boundary is the root Content-Type's.
func rootDelimiterLF(text string) int64 {
	pos := 0
	boundary := ""
	for {
		nl := strings.IndexByte(text[pos:], '\n')
		if nl < 0 {
			return -1
		}
		line := strings.TrimSuffix(text[pos:pos+nl], "\r")
		pos += nl + 1
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Type") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "multipart/") {
			return -1
		}
		_, b, ok := strings.Cut(value, "boundary=")
		if !ok {
			return -1
		}
		boundary, _, _ = strings.Cut(strings.Trim(b, `"`), ";")
		boundary = strings.Trim(boundary, `" `)
	}
	if boundary == "" {
		return -1
	}
	for first := true; pos < len(text); first = false {
		nl := strings.IndexByte(text[pos:], '\n')
		line := text[pos:]
		if nl >= 0 {
			line = text[pos : pos+nl]
		}
		rest, isDelim := strings.CutPrefix(line, "--"+boundary)
		if isDelim && strings.TrimRight(strings.TrimPrefix(rest, "--"), " \t") == "" {
			switch {
			case !first:
				return int64(pos - 1)
			case nl < 0:
				return -1
			}
			return int64(pos + nl)
		}
		if nl < 0 {
			return -1
		}
		pos += nl + 1
	}
	return -1
}

// lineEndingFacts is what a test pins of one diagnostic.
type lineEndingFacts struct {
	code       Code
	severity   Severity
	path       []int
	start, end int64
}

// factsOf reports each diagnostic's facts; a location without a known offset
// reports -1, -1, which no fixture expects, so a lost position fails.
func factsOf(diags []Diagnostic) []lineEndingFacts {
	var out []lineEndingFacts
	for _, d := range diags {
		start, end := d.Location.Offset, d.Location.EndOffset
		if !d.Location.HasOffset {
			start, end = -1, -1
		}
		out = append(out, lineEndingFacts{d.Code, d.Severity, d.Path, start, end})
	}
	return out
}

// parseAll parses text from src under profile and reads the whole tree: every
// entity's header fields and body, and the errors met.
type lineEndingTree struct {
	fields []string // "path key=value" in walk order
	bodies []string // "path body"
	errs   []string // "path err"
}

func readLineEndingTree(e *Entity) lineEndingTree {
	var tr lineEndingTree
	var visit func(path []int, e *Entity)
	visit = func(path []int, e *Entity) {
		fs := e.Header.Fields()
		for fs.Next() {
			tr.fields = append(tr.fields, fmt.Sprintf("%v %s=%q", path, fs.Key(), fs.Value()))
		}
		if mr := e.MultipartReader(); mr != nil {
			for i := 0; i < 64; i++ {
				p, err := mr.NextPart()
				if err == io.EOF {
					return
				}
				if err != nil {
					tr.errs = append(tr.errs, fmt.Sprintf("%v %v", append(append([]int(nil), path...), i), err))
					if p == nil {
						if IsFatal(err) && e.ParseStop() == nil {
							continue
						}
						return
					}
				}
				visit(append(append([]int(nil), path...), i), p)
			}
			return
		}
		b, err := io.ReadAll(io.LimitReader(e.Body, 1<<16))
		tr.bodies = append(tr.bodies, fmt.Sprintf("%v %q", path, b))
		if err != nil {
			tr.errs = append(tr.errs, fmt.Sprintf("%v body %v", path, err))
		}
	}
	visit(nil, e)
	return tr
}

// lineEndingSources are the deliveries every fixture is read through.
func lineEndingSources(text string) map[string]func() io.Reader {
	out := map[string]func() io.Reader{
		"whole":         func() io.Reader { return strings.NewReader(text) },
		"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(text)) },
	}
	for _, n := range []int{1, 7, 4096} {
		out[fmt.Sprintf("chunks of %d", n)] = func() io.Reader { return &chunkReader{s: text, n: n} }
	}
	return out
}

// TestParse_lineEndings_root pins the root header of each fixture, under every
// profile and delivery: the fields, the body, and the line-ending diagnostics
// with their exact positions.
func TestParse_lineEndings_root(t *testing.T) {
	for _, tc := range []struct {
		name   string
		text   string
		fields []string
		body   string
		// tolerant is what Tolerant and Forensic record; strictRefused is
		// whether Strict refuses the root, recording strict instead.
		tolerant      []lineEndingFacts
		strictRefused bool
		strict        []lineEndingFacts
	}{
		{
			name:   "crlf",
			text:   "A: 1\r\nB: 2\r\n\r\nbody",
			fields: []string{`[] A="1"`, `[] B="2"`},
			body:   "body",
		},
		{
			name:     "lf",
			text:     "A: 1\nB: 2\n\nbody",
			fields:   []string{`[] A="1"`, `[] B="2"`},
			body:     "body",
			tolerant: []lineEndingFacts{{CodeHeaderBareLF, SeverityInfo, nil, 4, 5}},
			strict:   []lineEndingFacts{{CodeHeaderBareLF, SeverityInfo, nil, 4, 5}},
		},
		{
			name:   "crlf then lf",
			text:   "A: 1\r\nB: 2\n\r\nbody",
			fields: []string{`[] A="1"`, `[] B="2"`},
			body:   "body",
			// Both name the LF at 10; at one offset they keep the order bare
			// LF, mixed.
			tolerant: []lineEndingFacts{
				{CodeHeaderBareLF, SeverityInfo, nil, 10, 11},
				{CodeHeaderMixedLineEndings, SeverityInfo, nil, 10, 11},
			},
			strict: []lineEndingFacts{
				{CodeHeaderBareLF, SeverityInfo, nil, 10, 11},
				{CodeHeaderMixedLineEndings, SeverityInfo, nil, 10, 11},
			},
		},
		{
			name:          "cr",
			text:          "A: 1\rB: 2\r\rbody",
			fields:        []string{`[] A="1"`, `[] B="2"`},
			body:          "body",
			tolerant:      []lineEndingFacts{{CodeHeaderBareCR, SeverityRecovered, nil, 4, 5}},
			strictRefused: true,
			strict:        []lineEndingFacts{{CodeHeaderBareCR, SeverityInvalidMetadata, nil, 4, 5}},
		},
		{
			name:   "cr then crlf empty line",
			text:   "A: 1\r\r\nbody",
			fields: []string{`[] A="1"`},
			body:   "body",
			tolerant: []lineEndingFacts{
				{CodeHeaderBareCR, SeverityRecovered, nil, 4, 5},
				{CodeHeaderMixedLineEndings, SeverityRecovered, nil, 5, 7},
			},
			strictRefused: true,
			strict: []lineEndingFacts{
				{CodeHeaderBareCR, SeverityInvalidMetadata, nil, 4, 5},
				{CodeHeaderMixedLineEndings, SeverityRecovered, nil, 5, 7},
			},
		},
		{
			name:   "stray cr in a value",
			text:   "Subject: a\rb\r\nX: 1\r\n\r\nbody",
			fields: []string{`[] Subject="a\rb"`, `[] X="1"`},
			body:   "body",
		},
		{
			name:          "missing final line ending",
			text:          "A: 1\r\nB: 2",
			fields:        []string{`[] A="1"`, `[] B="2"`},
			tolerant:      []lineEndingFacts{{CodeHeaderTruncatedLine, SeverityIncomplete, nil, 6, 10}},
			strictRefused: true,
			strict:        []lineEndingFacts{{CodeHeaderTruncatedLine, SeverityInvalidMetadata, nil, 6, 10}},
		},
		{
			name:          "header-only trailing cr is a complete ending",
			text:          "A: 1\r",
			fields:        []string{`[] A="1"`},
			tolerant:      []lineEndingFacts{{CodeHeaderBareCR, SeverityRecovered, nil, 4, 5}},
			strictRefused: true,
			strict:        []lineEndingFacts{{CodeHeaderBareCR, SeverityInvalidMetadata, nil, 4, 5}},
		},
		{
			name:          "bare cr alone",
			text:          "\r",
			tolerant:      []lineEndingFacts{{CodeHeaderBareCR, SeverityRecovered, nil, 0, 1}},
			strictRefused: true,
			strict:        []lineEndingFacts{{CodeHeaderBareCR, SeverityInvalidMetadata, nil, 0, 1}},
		},
		{
			name:   "empty and header-only crlf stay silent",
			text:   "A: 1\r\n",
			fields: []string{`[] A="1"`},
		},
	} {
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
			for name, src := range lineEndingSources(tc.text) {
				label := fmt.Sprintf("%s/%v/%s", tc.name, profile, name)
				session, e, err := Parse(src(), &ParseOptions{Profile: profile})
				if e == nil {
					t.Fatalf("%s: Parse() = (nil, %v), want the root", label, err)
				}
				refused := profile == ProfileStrict && tc.strictRefused
				want := tc.tolerant
				if profile == ProfileStrict {
					want = tc.strict
				}
				if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: diagnostics = %+v, want %+v", label, got, want)
				}
				if e.ParseStop() != nil {
					t.Errorf("%s: ParseStop() = %v, want none: a line ending is no stop", label, e.ParseStop())
				}
				if refused {
					checkLineEndingRefusal(t, label, e, err, want)
				} else if err != nil {
					t.Errorf("%s: Parse() err = %v, want nil", label, err)
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

// checkLineEndingRefusal pins a Strict refusal of an entity for its header's
// line endings: a FatalError carrying the first refusing diagnostic, the
// Content-Type not interpreted, no part iteration, and the parse not stopped.
func checkLineEndingRefusal(t *testing.T, label string, e *Entity, err error, recorded []lineEndingFacts) {
	t.Helper()
	if !IsFatal(err) {
		t.Fatalf("%s: err = %v, want the entity's FatalError", label, err)
	}
	var fe FatalError
	if !errors.As(err, &fe) {
		t.Fatalf("%s: err = %v is no FatalError", label, err)
	}
	d, ok := fe.Diagnostic()
	if !ok || d.Severity != SeverityFatal || (d.Code != CodeHeaderBareCR && d.Code != CodeHeaderTruncatedLine) {
		t.Errorf("%s: fatal diagnostic = %+v, want header.bare_cr or header.truncated_line", label, d)
	}
	found := false
	for _, r := range recorded {
		if r.code == d.Code && r.start == d.Location.Offset && r.end == d.Location.EndOffset && r.severity == SeverityInvalidMetadata {
			found = true
		}
	}
	if !found {
		t.Errorf("%s: fatal diagnostic %+v is not among the recorded refusals %+v", label, d, recorded)
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

// TestParse_lineEndings_strictChildren pins Strict refusing each child whose
// header ends a line with a bare CR, however many there are and whatever the
// diagnostic budget has left: every one comes back with its own FatalError and
// its raw body, and the next sibling is read as usual.
func TestParse_lineEndings_strictChildren(t *testing.T) {
	const head = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	var b strings.Builder
	b.WriteString(head)
	var offsets []int64
	for i := range 5 {
		b.WriteString("--B\r\n")
		if i%2 == 1 {
			// A CRLF child between them is accepted.
			b.WriteString("Content-Type: text/plain\r\n\r\n")
		} else {
			offsets = append(offsets, int64(b.Len()+len("Content-Type: application/octet-stream")))
			b.WriteString("Content-Type: application/octet-stream\r\r")
		}
		fmt.Fprintf(&b, "body %d\x00\xff\r", i)
		b.WriteString("\r\n")
	}
	b.WriteString("--B--\r\n")
	text := b.String()

	for _, maxDiags := range []int64{0, 1, 2} {
		for name, src := range lineEndingSources(text) {
			label := fmt.Sprintf("max diagnostics %d/%s", maxDiags, name)
			opts := &ParseOptions{Profile: ProfileStrict}
			if maxDiags > 0 {
				opts.Limits.MaxDiagnosticCount = maxDiags
			}
			session, e, err := Parse(src(), opts)
			if err != nil || e == nil {
				t.Fatalf("%s: Parse() = (%v, %v)", label, e, err)
			}
			mr := e.MultipartReader()
			refusals := 0
			for i := range 5 {
				p, err := mr.NextPart()
				if p == nil {
					t.Fatalf("%s: NextPart() %d = (nil, %v), want the part", label, i, err)
				}
				body, berr := io.ReadAll(p.Body)
				if want := fmt.Sprintf("body %d\x00\xff\r", i); string(body) != want || berr != nil {
					t.Errorf("%s: part %d body = (%q, %v), want (%q, nil)", label, i, body, berr, want)
				}
				if i%2 == 1 {
					if err != nil {
						t.Errorf("%s: part %d err = %v, want none", label, i, err)
					}
					continue
				}
				var fe FatalError
				if !errors.As(err, &fe) {
					t.Fatalf("%s: part %d err = %v, want its own FatalError", label, i, err)
				}
				d, _ := fe.Diagnostic()
				if d.Code != CodeHeaderBareCR || !reflect.DeepEqual(d.Path, []int{i}) || !d.Location.HasOffset || d.Location.Offset != offsets[refusals] {
					t.Errorf("%s: part %d refusal = %+v, want header.bare_cr of [%d] at %d", label, i, d, i, offsets[refusals])
				}
				if mt, _, _ := p.EffectiveContentType(); mt != "" {
					t.Errorf("%s: part %d media type = %q, want none interpreted", label, i, mt)
				}
				refusals++
			}
			if _, err := mr.NextPart(); err != io.EOF {
				t.Errorf("%s: NextPart() after the last = %v, want io.EOF", label, err)
			}
			if e.ParseStop() != nil {
				t.Errorf("%s: ParseStop() = %v, want none", label, e.ParseStop())
			}
			if maxDiags == 0 {
				if got := len(session.Diagnostics()); got != 3 {
					t.Errorf("%s: %d diagnostics, want one per refused part", label, got)
				}
			} else if got := int64(len(session.Diagnostics())); got != maxDiags {
				t.Errorf("%s: %d diagnostics, want the budget of %d", label, got, maxDiags)
			}
		}
	}
}

// TestParse_lineEndings_oncePerParse pins that the notes about how a message
// is written are recorded once per parse however many header blocks share
// them, at the first, while a Tolerant parse reads every part.
func TestParse_lineEndings_oncePerParse(t *testing.T) {
	for _, eol := range []string{"\n", "\r"} {
		var b strings.Builder
		b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
		for i := range 20 {
			// The framing stays CRLF; only the part headers use eol.
			fmt.Fprintf(&b, "--B\r\nX-N: %d%s%sp%d\r\n", i, eol, eol, i)
		}
		b.WriteString("--B--\r\n")
		session, e, err := Parse(strings.NewReader(b.String()), nil)
		if err != nil {
			t.Fatal(err)
		}
		tr := readLineEndingTree(e)
		if len(tr.bodies) != 20 || tr.errs != nil {
			t.Fatalf("%q: bodies %q errs %q, want 20 parts", eol, tr.bodies, tr.errs)
		}
		code, severity := CodeHeaderBareLF, SeverityInfo
		if eol == "\r" {
			code, severity = CodeHeaderBareCR, SeverityRecovered
		}
		first := int64(len("Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nX-N: 0"))
		want := []lineEndingFacts{{code, severity, []int{0}, first, first + 1}}
		if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: diagnostics = %+v, want %+v", eol, got, want)
		}
	}
}

// TestParse_lineEndings_sinkStopOutranksRefusal pins that a sink stopping the
// parse on a line-ending diagnostic stops it as from any diagnostic, and that
// the stop is what the entity is returned with, ahead of a Strict refusal.
func TestParse_lineEndings_sinkStopOutranksRefusal(t *testing.T) {
	errSink := errors.New("sink: enough")
	for _, profile := range []Profile{ProfileTolerant, ProfileStrict} {
		sink := DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return errSink })
		session, e, err := Parse(strings.NewReader("A: 1\rB: 2\r\rbody"), &ParseOptions{Profile: profile, DiagnosticSink: sink})
		if e == nil || !errors.Is(err, errSink) || session.fatalErr() == nil || error(session.fatalErr()) != err {
			t.Errorf("%v: Parse() = (%v, %v), want the entity and the session's stop", profile, e, err)
		}
	}
}

// TestParse_lineEndings_legacyRecordsNothing pins that Read frames the same
// header blocks and refuses nothing: it has no session and no profile.
func TestParse_lineEndings_legacyRecordsNothing(t *testing.T) {
	for _, text := range []string{"A: 1\rB: 2\r\rbody", "A: 1\nB: 2\n\nbody", "A: 1\r\nB: 2"} {
		e, err := Read(strings.NewReader(text))
		if err != nil || e == nil {
			t.Fatalf("%q: Read() = (%v, %v), want the entity and no error", text, e, err)
		}
		if mt, _, _ := e.EffectiveContentType(); mt != "text/plain" {
			t.Errorf("%q: legacy media type = %q, want text/plain: nothing is refused", text, mt)
		}
		if e.Header.Get("A") != "1" || e.Header.Get("B") != "2" {
			t.Errorf("%q: fields = %v", text, e.Header.Map())
		}
	}
}

// TestParse_lineEndings_writeNormalizes pins that WriteTo writes an unmodified
// header a healing parse read with LF or CR line endings byte for byte, as it
// writes a CRLF one, and one with no final line ending and no empty line
// without them (ROB-020-007; it used to write CRLF and the empty line for
// them), and that the result reads back to the same fields and body and is
// written the same again.
func TestParse_lineEndings_writeNormalizes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Mime-Version: 1.0\r\nA: 1\r\n folded\r\n\r\nbody", "Mime-Version: 1.0\r\nA: 1\r\n folded\r\n\r\nbody"},
		{"Mime-Version: 1.0\nA: 1\n folded\n\nbody", "Mime-Version: 1.0\nA: 1\n folded\n\nbody"},
		{"Mime-Version: 1.0\rA: 1\r folded\r\rbody", "Mime-Version: 1.0\rA: 1\r folded\r\rbody"},
		{"Mime-Version: 1.0\r\nSubject: a\rb\r\n\r\nbody", "Mime-Version: 1.0\r\nSubject: a\rb\r\n\r\nbody"},
		{"Mime-Version: 1.0\r\nA: 1", "Mime-Version: 1.0\r\nA: 1"},
	} {
		_, e, _ := Parse(strings.NewReader(tc.in), nil)
		var out bytes.Buffer
		if err := e.WriteTo(&out); err != nil || out.String() != tc.want {
			t.Errorf("%q: WriteTo() = (%q, %v), want %q", tc.in, out.String(), err, tc.want)
			continue
		}
		// What was written with CRLF records nothing when read back; what
		// was replayed records what the input did.
		first, _, _ := Parse(strings.NewReader(tc.in), nil)
		session, again, err := Parse(bytes.NewReader(out.Bytes()), nil)
		if replayed := out.String() == tc.in; err != nil || (!replayed && session.Diagnostics() != nil) ||
			(replayed && !reflect.DeepEqual(codesOfDiags(session.Diagnostics()), codesOfDiags(first.Diagnostics()))) {
			t.Errorf("%q: reparse = (%v, %v)", tc.in, err, codesOfDiags(session.Diagnostics()))
			continue
		}
		var second bytes.Buffer
		if err := again.WriteTo(&second); err != nil || second.String() != tc.want {
			t.Errorf("%q: second WriteTo() = (%q, %v), want %q", tc.in, second.String(), err, tc.want)
		}
	}
}

// TestParse_lineEndings_binaryBody pins that no byte after the header block is
// rewritten: every byte value, CR and LF included, reaches an opaque body as it
// was, whatever ended the header's lines.
func TestParse_lineEndings_binaryBody(t *testing.T) {
	var all []byte
	for i := range 256 {
		all = append(all, byte(i))
	}
	all = append(all, "\r\r\n\n\r"...)
	for _, sep := range []string{"\r\n\r\n", "\n\n", "\r\r", "\r\r\n", "\n\r\n"} {
		text := "Content-Type: application/octet-stream" + sep + string(all)
		for name, src := range lineEndingSources(text) {
			_, e, err := Parse(src(), nil)
			if err != nil {
				t.Fatalf("%q/%s: Parse() = %v", sep, name, err)
			}
			body, err := io.ReadAll(e.Body)
			if err != nil || !bytes.Equal(body, all) {
				t.Errorf("%q/%s: body = (%q, %v), want every byte as it was", sep, name, body, err)
			}
		}
	}
}

// TestParse_lineEndings_inputErrorsStayErrors pins that a failure of the input
// inside a header block framed by bare CRs is reported as the input's own
// error, with no entity and no diagnostic: it is not a line-ending defect.
func TestParse_lineEndings_inputErrorsStayErrors(t *testing.T) {
	errEIO := errors.New("test source: input/output error")
	const text = "A: 1\rB: 2\r\rbody"
	// The last cut leaves the empty line's '\r' undecided too.
	for cut := 0; cut <= len("A: 1\rB: 2\r\r"); cut++ {
		src := io.MultiReader(strings.NewReader(text[:cut]), iotest.ErrReader(errEIO))
		session, e, err := Parse(src, nil)
		if e != nil || err != errEIO || session.Diagnostics() != nil {
			t.Errorf("cut %d: Parse() = (%v, %v, %v), want no entity, the input's error and nothing recorded",
				cut, e, err, codesOfDiags(session.Diagnostics()))
		}
	}
}

// TestParse_lineEndings_exactInputBudget pins the one look-ahead a bare CR
// costs: a header whose last line ending is a bare CR is read within an input
// budget of exactly its length only when nothing follows; with a body after it,
// deciding the '\r' needs the byte after the budget, and the parse stops there
// with no entity. A CRLF header is read within its own length either way.
func TestParse_lineEndings_exactInputBudget(t *testing.T) {
	for _, tc := range []struct {
		header, rest string
		entity       bool
	}{
		{"A: 1\r\r", "", true},
		{"A: 1\r\r", "body", false},
		{"A: 1\r\n\r\n", "", true},
		{"A: 1\r\n\r\n", "body", true},
	} {
		limits := Limits{MaxInputBytes: int64(len(tc.header))}
		_, e, err := Parse(strings.NewReader(tc.header+tc.rest), &ParseOptions{Limits: limits})
		if (e != nil) != tc.entity {
			t.Errorf("%q+%q: Parse() = (%v, %v), want entity %v", tc.header, tc.rest, e, err, tc.entity)
		}
	}
}

// TestRead_lineEndings_headerLookAhead pins the legacy MaxHeaderBytes edge of
// that look-ahead: Read shortens the reads under its header to the budget, so a
// header of exactly that many bytes ending with a bare CR, and followed by a
// body, cannot be decided within it.
func TestRead_lineEndings_headerLookAhead(t *testing.T) {
	const header = "A: 1\r\r"
	if _, err := ReadWithOptions(strings.NewReader(header+"body"), &ReadOptions{MaxHeaderBytes: int64(len(header))}); !errors.Is(err, textproto.ErrHeaderTooBig) {
		t.Errorf("ReadWithOptions(exact) = %v, want the header budget", err)
	}
	if e, err := ReadWithOptions(strings.NewReader(header+"body"), &ReadOptions{MaxHeaderBytes: int64(len(header)) + 1}); err != nil || e.Header.Get("A") != "1" {
		t.Errorf("ReadWithOptions(exact+1) = (%v, %v), want the header", e, err)
	}
}
