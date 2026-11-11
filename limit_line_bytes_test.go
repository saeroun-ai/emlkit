package message

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Limits.MaxLineBytes outside a header block: the framing lines of a multipart
// body, and the framing lines of a line-oriented transfer encoding.
//
// The tests here are as much about what the budget must not do as about what it
// does. A body with no line structure the parser imposes is not a line, however
// long it is, and a run of bytes that no terminator ever ends is not a line
// either; neither may be refused, and neither may be accumulated to find that
// out.

// lineOnly returns every budget disabled except the line budget, set to n.
func lineOnly(n int64) Limits {
	l := allDisabled()
	l.MaxLineBytes = n
	return l
}

// checkLineDiagnostic pins the one limit.line_bytes diagnostic a refused framing
// line records.
func checkLineDiagnostic(t *testing.T, session *ParseSession, limit int64, path []int, offset int64) {
	t.Helper()
	checkLineDiagnosticIn(t, session.Diagnostics(), limit, path, offset)
}

// checkLineDiagnosticIn is checkLineDiagnostic holding diags, rather than
// everything the session recorded, to exactly the one entry.
func checkLineDiagnosticIn(t *testing.T, diags []Diagnostic, limit int64, path []int, offset int64) {
	t.Helper()

	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one limit.line_bytes entry", codesOfDiags(diags))
	}
	d := diags[0]

	if d.Code != CodeLimitLineBytes {
		t.Errorf("Code = %q, want %q", d.Code, CodeLimitLineBytes)
	}
	if d.Severity != SeverityLimitExceeded {
		t.Errorf("Severity = %v, want %v", d.Severity, SeverityLimitExceeded)
	}
	if !reflect.DeepEqual(d.Path, path) {
		t.Errorf("Path = %v, want %v", d.Path, path)
	}
	if want := LocationAt(offset); d.Location != want {
		t.Errorf("Location = %v, want %v: the first byte of the refused line", d.Location, want)
	}
	if d.Action == "" {
		t.Errorf("Action is empty, want what was done about the defect")
	}
	if !strings.Contains(d.Message, fmt.Sprint(limit)) {
		t.Errorf("Message = %q, want the budget %d in it", d.Message, limit)
	}
}

// A framingCase is one multipart framing line under test: the message holding
// it, the container it belongs to, where it is, and what a traversal recovers
// before and after it.
type framingCase struct {
	name    string
	path    []int // the container whose framing this is
	line    string
	fixture originFixture
	all     []string
	upTo    []string
}

// rootFramed reports whether the root's first delimiter line is framed before
// c's line is refused, so that an LF fixture records its multipart.bare_lf: a
// refused line of the root's own framing — its preamble line or its first
// delimiter line — comes first, and one inside a part comes after.
func (c framingCase) rootFramed() bool { return len(c.path) > 0 }

// framingCases returns the framing lines of a multipart body, each made the
// longest line in its message so that a budget swept across it says something
// about it alone.
func framingCases(eol string) []framingCase {
	const n = 100
	preamble := strings.Repeat("p", n)
	// A delimiter line may carry trailing linear whitespace, which is how a
	// delimiter line is made long without making the boundary parameter that
	// declares it long.
	delimiter := "--B" + strings.Repeat(" ", n-len("--B"))

	root := func(body ...string) originFixture {
		segs := [][2]string{
			{"root.header", "Content-Type: multipart/mixed; boundary=B" + eol},
			{"", eol},
			{"root.body", ""},
		}
		for i, s := range body {
			label := ""
			if i == 0 {
				label = "unit"
			}
			segs = append(segs, [2]string{label, s})
		}
		return newOriginFixture(segs...)
	}

	nested := newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=OUTER" + eol},
		[2]string{"", eol},
		[2]string{"root.body", "--OUTER" + eol},
		[2]string{"0.header", "Content-Type: multipart/alternative; boundary=B" + eol},
		[2]string{"", eol},
		[2]string{"0.body", ""},
		[2]string{"unit", preamble + eol},
		[2]string{"", "--B" + eol},
		[2]string{"", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"", "inner" + eol},
		[2]string{"", "--B--" + eol},
		[2]string{"", "--OUTER" + eol},
		[2]string{"", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"", "sibling" + eol},
		[2]string{"", "--OUTER--" + eol},
	)

	return []framingCase{
		{
			name: "preamble line",
			path: nil,
			line: preamble,
			fixture: root(
				preamble+eol,
				"--B"+eol,
				"Content-Type: text/plain"+eol,
				eol,
				"zero"+eol,
				"--B--"+eol,
			),
			all:  []string{"zero"},
			upTo: nil,
		},
		{
			name: "delimiter line",
			path: nil,
			line: delimiter,
			fixture: root(
				delimiter+eol,
				"Content-Type: text/plain"+eol,
				eol,
				"zero"+eol,
				"--B--"+eol,
			),
			all:  []string{"zero"},
			upTo: nil,
		},
		{
			name:    "nested preamble line",
			path:    []int{0},
			line:    preamble,
			fixture: nested,
			all:     []string{"inner", "sibling"},
			upTo:    nil,
		},
	}
}

// TestParse_multipartLineBytes_boundary sweeps the line budget across the exact
// length of one framing line, at the top level and inside a nested container and
// with each line ending, and pins where the boundary falls: a line of exactly the
// budget is read, one byte more is refused, and the refusal names the container
// whose framing it is and the exact position the line began at.
func TestParse_multipartLineBytes_boundary(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		t.Run(eolName(eol), func(t *testing.T) {
			for _, tc := range framingCases(eol) {
				t.Run(tc.name, func(t *testing.T) {
					size := int64(len(tc.line))
					offset := tc.fixture.offset(t, "unit")

					for _, delta := range []int64{-1, 0, 1} {
						t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
							limit := size + delta
							session, got, err := parseUnder(t, tc.fixture.text, lineOnly(limit))
							if err != nil {
								t.Fatalf("Parse() = %v", err)
							}

							if delta >= 0 {
								if got.err != nil {
									t.Fatalf("reading the tree = %v", got.err)
								}
								if !reflect.DeepEqual(got.bodies, tc.all) {
									t.Errorf("bodies = %v, want %v", got.bodies, tc.all)
								}
								if d := takeLineNotes(t, "", eol, tc.fixture.text, session.Diagnostics(), true); len(d) != 0 {
									t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
								}
								return
							}

							if !IsFatal(got.err) {
								t.Fatalf("reading the tree = %v, want a FatalError", got.err)
							}
							if !errors.Is(got.err, textproto.ErrLineTooBig) {
								t.Errorf("err = %v, want it to wrap the line budget failure", got.err)
							}
							if !reflect.DeepEqual(got.bodies, tc.upTo) {
								t.Errorf("bodies = %v, want %v recovered before the refusal", got.bodies, tc.upTo)
							}
							checkLineDiagnosticIn(t, takeLineNotes(t, "", eol, tc.fixture.text, session.Diagnostics(), tc.rootFramed()), limit, tc.path, offset)
						})
					}
				})
			}
		})
	}
}

// TestParse_multipartLineBytes_chunkInvariance pins that a framing line decides
// the same way however the input arrives, including when a CRLF is split across
// two reads and when the reader reports the end of the input with its last
// bytes.
func TestParse_multipartLineBytes_chunkInvariance(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		for _, tc := range framingCases(eol) {
			size := int64(len(tc.line))
			offset := tc.fixture.offset(t, "unit")

			for name, src := range chunkedSources(tc.fixture.text) {
				for _, delta := range []int64{-1, 0} {
					limit := size + delta
					session, e, err := Parse(src(), &ParseOptions{Limits: lineOnly(limit)})
					if err != nil {
						t.Fatalf("%s: Parse() = %v", name, err)
					}
					got := readTree(e)

					label := fmt.Sprintf("%s/%s/%s at limit%+d", eolName(eol), tc.name, name, delta)
					want := tc.all
					if delta < 0 {
						want = tc.upTo
					}
					if !reflect.DeepEqual(got.bodies, want) {
						t.Errorf("%s: bodies = %v, want %v", label, got.bodies, want)
					}
					if delta == 0 {
						if got.err != nil {
							t.Errorf("%s: %v", label, got.err)
						}
						continue
					}
					if !IsFatal(got.err) {
						t.Errorf("%s: err = %v, want a FatalError", label, got.err)
						continue
					}
					checkLineDiagnosticIn(t, takeLineNotes(t, label, eol, tc.fixture.text, session.Diagnostics(), tc.rootFramed()), limit, tc.path, offset)
				}
			}
		}
	}
}

// TestParse_multipartLineBytes_stickyAndUsablePrefix pins what a refused framing
// line leaves behind: the parts already returned stay valid and readable, the
// iterator reports the identical error however often it is asked, and one
// diagnostic is recorded however many times it is asked.
func TestParse_multipartLineBytes_stickyAndUsablePrefix(t *testing.T) {
	const eol = "\r\n"
	const n = 100

	f := newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=B" + eol},
		[2]string{"", eol},
		[2]string{"root.body", "--B" + eol},
		[2]string{"", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"", "zero" + eol},
		// The delimiter that introduces the second part carries enough trailing
		// linear whitespace to be over the budget. It is framing, and it is read
		// only after the first part has been returned, so the refusal has a
		// prefix to leave behind.
		[2]string{"unit", "--B" + strings.Repeat(" ", n-len("--B")) + eol},
		[2]string{"", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"", "one" + eol},
		[2]string{"", "--B--" + eol},
	)

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: lineOnly(n - 1)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	mr := e.MultipartReader()
	first, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v", err)
	}
	if got := readBodyOf(t, first); got != "zero" {
		t.Errorf("first part = %q, want %q", got, "zero")
	}

	_, want := mr.NextPart()
	if !IsFatal(want) || !errors.Is(want, textproto.ErrLineTooBig) {
		t.Fatalf("NextPart() = %v, want the line budget refusal", want)
	}
	for i := 0; i < 3; i++ {
		p, err := mr.NextPart()
		if p != nil {
			t.Errorf("call %d after the refusal returned %v, want no part", i, p)
		}
		if err != want {
			t.Errorf("call %d after the refusal = %v, want the identical error %v", i, err, want)
		}
	}

	checkLineDiagnostic(t, session, n-1, nil, f.offset(t, "unit"))

	if first.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("the part returned before the failure lost its header")
	}
}

// TestParse_multipartLineBytes_sinkStopWins pins that a sink refusing to
// continue while it is told about a refused framing line outranks this scope's
// own failure, and that the diagnostic was delivered either way.
func TestParse_multipartLineBytes_sinkStopWins(t *testing.T) {
	stop := errors.New("stop here")
	tc := framingCases("\r\n")[0]

	var seen []Code
	_, e, err := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
		Limits: lineOnly(int64(len(tc.line)) - 1),
		DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			seen = append(seen, d.Code)
			return stop
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	got := readTree(e)
	if !errors.Is(got.err, stop) {
		t.Fatalf("reading the tree = %v, want the sink's own error", got.err)
	}
	if !IsFatal(got.err) {
		t.Errorf("err = %v, want a FatalError", got.err)
	}
	if !reflect.DeepEqual(seen, []Code{CodeLimitLineBytes}) {
		t.Errorf("the sink received %v, want exactly the line budget diagnostic", seen)
	}
}

// TestParse_multipartLineBytes_partBodiesAreNotLines pins the boundary of the
// budget's scope inside a multipart. A part's body is opaque content that this
// layer streams, so a body line far past the budget — with terminators, so that
// there is no doubt it is a line — is not refused, while the framing around it
// still is.
func TestParse_multipartLineBytes_partBodiesAreNotLines(t *testing.T) {
	long := strings.Repeat("z", 5000)
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\n" +
		long + "\r\n" + long + "\r\n" +
		"--B--\r\n"

	session, got, err := parseUnder(t, text, lineOnly(64))
	if err != nil || got.err != nil {
		t.Fatalf("Parse() = %v, reading the tree = %v", err, got.err)
	}
	if len(got.bodies) != 1 || !strings.Contains(got.bodies[0], long) {
		t.Errorf("bodies = %d entries, want the part's body whole", len(got.bodies))
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none: a part's body is not framing", codesOfDiags(d))
	}
}

// TestParse_multipartLineBytes_disabledKeepsLegacyReading pins what turning the
// budget off means for framing lines. Nothing is refused by the budget, and the
// lines are read exactly as they were before this budget existed — which is what
// Read does too, since it resolves no configuration at all.
func TestParse_multipartLineBytes_disabledKeepsLegacyReading(t *testing.T) {
	tc := framingCases("\r\n")[0]

	for _, name := range []string{"disabled", "legacy Read"} {
		var (
			e   *Entity
			err error
			s   *ParseSession
		)
		if name == "disabled" {
			s, e, err = Parse(strings.NewReader(tc.fixture.text), &ParseOptions{Limits: lineOnly(LimitDisabled)})
		} else {
			e, err = Read(strings.NewReader(tc.fixture.text))
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := readTree(e)
		if got.err != nil {
			t.Fatalf("%s: reading the tree = %v", name, got.err)
		}
		if !reflect.DeepEqual(got.bodies, tc.all) {
			t.Errorf("%s: bodies = %v, want %v", name, got.bodies, tc.all)
		}
		if d := s.Diagnostics(); d != nil {
			t.Errorf("%s: Diagnostics() = %v, want none", name, codesOfDiags(d))
		}
	}
}

// A cteCase is one transfer-encoded body under test.
type cteCase struct {
	name string

	// encoding is what the entity declares, and body is the wire bytes of the
	// body, whose second line is the one a budget is swept across.
	encoding string
	first    string // a short, valid first line
	long     func(n int) string
	decoded  string // what the first line decodes to
}

func cteCases() []cteCase {
	return []cteCase{
		{
			// The first line carries no padding, so what follows it is not
			// data after a terminated stream — a defect of its own, and not
			// what this budget is about.
			name:     "base64",
			encoding: "base64",
			first:    "aGVs",
			// "V" decodes to "U": text, so that the body records nothing about
			// its charset (charset.missing).
			long:    func(n int) string { return strings.Repeat("V", n) },
			decoded: "hel",
		},
		{
			name:     "quoted-printable",
			encoding: "quoted-printable",
			first:    "hello",
			long:     func(n int) string { return strings.Repeat("x", n) },
			decoded:  "hello",
		},
	}
}

// cteFixture places a transfer-encoded body at one of two depths, with the line
// under test labelled.
//
// A line after the one under test is what terminates it. Inside a part that
// matters: the terminator before a delimiter belongs to the framing rather than
// to the body, so a long last line of a part is a run the body never ends, which
// is deliberately not something this budget refuses.
func cteFixture(place string, tc cteCase, n int, eol string) originFixture {
	body := tc.first + eol
	long := tc.long(n) + eol
	const tail = "dGFp" // as base64 it decodes to text, "tai"

	if place == "root" {
		return newOriginFixture(
			[2]string{"root.header", "Content-Type: text/plain" + eol},
			[2]string{"", "Content-Transfer-Encoding: " + tc.encoding + eol},
			[2]string{"", eol},
			[2]string{"root.body", body},
			[2]string{"unit", long},
			[2]string{"", tail + eol},
		)
	}
	return newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=B" + eol},
		[2]string{"", eol},
		[2]string{"root.body", "--B" + eol},
		[2]string{"0.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"0.body", "first part" + eol},
		[2]string{"", "--B" + eol},
		[2]string{"1.header", "Content-Type: text/plain" + eol},
		[2]string{"", "Content-Transfer-Encoding: " + tc.encoding + eol},
		[2]string{"", eol},
		[2]string{"1.body", body},
		[2]string{"unit", long},
		[2]string{"", tail + eol},
		[2]string{"", "--B--" + eol},
	)
}

// TestParse_cteLineBytes_boundary sweeps the line budget across the exact length
// of one encoded line, for each line-framed encoding, at the top level and
// inside a part, and with each line ending. A line of exactly the budget is
// decoded; one byte more ends the body, and the bytes decoded before it stay the
// caller's.
func TestParse_cteLineBytes_boundary(t *testing.T) {
	const n = 200

	for _, tc := range cteCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, eol := range []string{"\r\n", "\n"} {
				t.Run(eolName(eol), func(t *testing.T) {
					for _, place := range []struct {
						name string
						path []int
					}{
						{"root", nil},
						{"child", []int{1}},
					} {
						t.Run(place.name, func(t *testing.T) {
							f := cteFixture(place.name, tc, n, eol)
							offset := f.offset(t, "unit")

							for _, delta := range []int64{-1, 0, 1} {
								t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
									limit := int64(n) + delta
									session, body, err := readEncodedBody(t, f.text, place.path, lineOnly(limit))
									if err != nil && !IsFatal(err) {
										t.Fatalf("reading the body = %v", err)
									}

									if delta >= 0 {
										if IsFatal(err) {
											t.Fatalf("a line of %d bytes was refused by a budget of %d: %v", n, limit, err)
										}
										// Reaching the body of part 1 frames the root's
										// delimiters; a root that is not multipart has none.
										if d := takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true); len(d) != 0 {
											t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
										}
										return
									}

									if !IsFatal(err) {
										t.Fatalf("reading the body = %v, want a FatalError", err)
									}
									if !strings.HasPrefix(body, tc.decoded) {
										t.Errorf("body = %q, want the prefix %q decoded before the refusal", body, tc.decoded)
									}
									checkLineDiagnosticIn(t, takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true), limit, place.path, offset)
								})
							}
						})
					}
				})
			}
		})
	}
}

// readEncodedBody parses text, walks to the entity at path, and reads its body
// whole, returning what it decoded and what stopped it.
func readEncodedBody(t *testing.T, text string, path []int, limits Limits) (*ParseSession, string, error) {
	t.Helper()

	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
	if err != nil {
		return session, "", err
	}
	for _, want := range path {
		mr := e.MultipartReader()
		if mr == nil {
			t.Fatalf("no multipart at %v", path)
		}
		for i := 0; ; i++ {
			p, err := mr.NextPart()
			if err != nil {
				t.Fatalf("NextPart() = %v", err)
			}
			if i == want {
				e = p
				break
			}
			io.Copy(io.Discard, p.Body)
		}
	}
	b, err := io.ReadAll(e.Body)
	return session, string(b), err
}

// TestParse_cteLineBytes_noTerminatorIsNotALine pins the exception this budget
// turns on. A run of encoded bytes that no terminator ever ends has not been
// shown to be a line at all, so it is not refused however long it is — and it is
// not accumulated to decide that, which is what makes the budget safe to apply
// to a stream.
func TestParse_cteLineBytes_noTerminatorIsNotALine(t *testing.T) {
	for _, tc := range cteCases() {
		t.Run(tc.name, func(t *testing.T) {
			// A body of one enormous run with no line terminator anywhere.
			text := "Content-Type: text/plain\r\n" +
				"Content-Transfer-Encoding: " + tc.encoding + "\r\n\r\n" +
				tc.long(1<<20)

			session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: lineOnly(64)})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			n, err := io.Copy(io.Discard, e.Body)
			if err != nil {
				t.Fatalf("reading the body = %v, want a body with no line in it to be read whole", err)
			}
			if n == 0 {
				t.Errorf("read %d bytes, want the body", n)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("Diagnostics() = %v, want none: no terminator ever proved a line", codesOfDiags(d))
			}
		})
	}
}

// TestParse_cteLineBytes_opaqueEncodingsAreNotLines pins the other half of the
// scope. An encoding whose wire form imposes no line structure the parser
// interprets is opaque content, so a body of enormous terminated lines under one
// is not refused either.
func TestParse_cteLineBytes_opaqueEncodingsAreNotLines(t *testing.T) {
	long := strings.Repeat("z", 5000)

	for _, enc := range []string{"", "7bit", "8bit", "binary", "x-nonsense"} {
		name := enc
		if name == "" {
			name = "none"
		}
		t.Run(name, func(t *testing.T) {
			text := "Content-Type: text/plain\r\n"
			if enc != "" {
				text += "Content-Transfer-Encoding: " + enc + "\r\n"
			}
			text += "\r\n" + long + "\r\n" + long + "\r\n"

			session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: lineOnly(64)})
			if e == nil {
				t.Fatalf("Parse() = (nil, %v), want a usable entity", err)
			}
			b, readErr := io.ReadAll(e.Body)
			if readErr != nil {
				t.Fatalf("reading the body = %v", readErr)
			}
			if !strings.Contains(string(b), long) {
				t.Errorf("body was not read whole")
			}
			for _, d := range session.Diagnostics() {
				if d.Code == CodeLimitLineBytes {
					t.Errorf("Diagnostics() = %v, want no line budget entry for an opaque body", codesOfDiags(session.Diagnostics()))
				}
			}
		})
	}
}

// TestParse_cteLineBytes_stickyOnceAndDisabled pins that a refused encoded line
// is reported identically however often the body is read afterwards, that it is
// recorded once, and that a disabled budget refuses nothing.
func TestParse_cteLineBytes_stickyOnceAndDisabled(t *testing.T) {
	const n = 200
	tc := cteCases()[0]
	f := cteFixture("root", tc, n, "\r\n")

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: lineOnly(n - 1)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	buf := make([]byte, 512)
	var got []byte
	var stopped error
	for {
		k, err := e.Body.Read(buf)
		got = append(got, buf[:k]...)
		if err != nil {
			stopped = err
			break
		}
	}
	if !IsFatal(stopped) {
		t.Fatalf("reading the body = %v, want a FatalError", stopped)
	}
	if string(got) != tc.decoded {
		t.Errorf("body = %q, want the prefix %q", got, tc.decoded)
	}
	for i := 0; i < 3; i++ {
		k, err := e.Body.Read(buf)
		if k != 0 {
			t.Errorf("read %d bytes after the refusal, want none", k)
		}
		if err != stopped {
			t.Errorf("read %d after the refusal = %v, want the identical error", i, err)
		}
	}
	checkLineDiagnostic(t, session, n-1, nil, f.offset(t, "unit"))

	// The same body under a disabled budget is decoded whole.
	session, e, err = Parse(strings.NewReader(f.text), &ParseOptions{Limits: lineOnly(LimitDisabled)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if _, err := io.ReadAll(e.Body); err != nil {
		t.Errorf("reading the body under a disabled budget = %v", err)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none from a disabled budget", codesOfDiags(d))
	}
}

// TestParse_cteLineBytes_legacyPathsUnwatched pins that no line budget reaches
// Read or ReadWithOptions: a body they decode is read exactly as it always was,
// however long its lines.
func TestParse_cteLineBytes_legacyPathsUnwatched(t *testing.T) {
	for _, tc := range cteCases() {
		f := cteFixture("root", tc, 5000, "\r\n")
		e, err := Read(strings.NewReader(f.text))
		if err != nil {
			t.Fatalf("%s: Read() = %v", tc.name, err)
		}
		if _, err := io.ReadAll(e.Body); err != nil {
			t.Errorf("%s: reading the body = %v", tc.name, err)
		}
	}
}

// TestParse_cteLineBytes_boundedMemoryOnEndlessInput pins that the monitor holds
// nothing. An encoded body that never ends and never terminates a line is
// streamed past it, so the parse costs what the caller's own reads cost and not
// what the input's length costs.
func TestParse_cteLineBytes_boundedMemoryOnEndlessInput(t *testing.T) {
	src := &endlessReader{
		prefix: "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n",
		line:   strings.Repeat("V", 4096), // decodes to "U": text
		cap:    endlessCap,
	}

	session, e, err := Parse(src, &ParseOptions{Limits: lineOnly(64)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	// Read the whole thing. Nothing may be refused, and nothing may be
	// accumulated: a monitor that buffered a line would have to hold all 8 MiB
	// of a body with no terminator in it.
	n, err := io.Copy(io.Discard, e.Body)
	if err != nil {
		t.Fatalf("reading the body = %v", err)
	}
	if n == 0 {
		t.Errorf("decoded %d bytes, want the body", n)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
	}
}

// TestParse_cteLineBytes_underlyingFailureIsNotConflated pins that a body whose
// own reader fails is not reported as a refused line. The monitor refuses only
// what its own counter refused, so an input failure — including one carrying the
// very sentinel a refusal wraps — passes through as the input failure it is.
func TestParse_cteLineBytes_underlyingFailureIsNotConflated(t *testing.T) {
	f := cteFixture("root", cteCases()[0], 200, "\r\n")

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ordinary", errors.New("input broke")},
		{"line sentinel", textproto.ErrLineTooBig},
		{"forged line limit", &textproto.LineLimitError{}},
		{"typed nil line limit", (*textproto.LineLimitError)(nil)},
		{"match-all Is", matchAllIsError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &errorAfterReader{s: f.text, n: int(f.offset(t, "unit")) + 8, err: tc.err}

			session, e, err := Parse(src, &ParseOptions{Limits: lineOnly(1 << 20)})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			_, readErr := io.ReadAll(e.Body)
			if readErr == nil {
				t.Fatal("reading the body succeeded, want the input's failure")
			}
			_ = readErr.Error()
			if parserProducedFatal(readErr) {
				t.Errorf("err = %v, want an ordinary input error, not a parse failure", readErr)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("Diagnostics() = %v, want none: no budget was reached", codesOfDiags(d))
			}
		})
	}
}

// TestParse_lineBytes_profileIndependent pins that this budget, like every
// other, is not a matter of interpretation: every profile and every
// compatibility policy enforce it identically, for framing lines and encoded
// lines alike.
func TestParse_lineBytes_profileIndependent(t *testing.T) {
	framing := framingCases("\r\n")[0]
	encoded := cteFixture("root", cteCases()[0], 200, "\r\n")

	for _, profile := range []Profile{ProfileStrict, ProfileTolerant, ProfileForensic} {
		for _, compat := range []Compatibility{
			CompatibilityConservative, CompatibilityAngus,
			CompatibilityMimeKit, CompatibilityPythonEmail,
		} {
			opts := &ParseOptions{
				Profile:       profile,
				Compatibility: compat,
				Limits:        lineOnly(int64(len(framing.line)) - 1),
			}
			session, e, err := Parse(strings.NewReader(framing.fixture.text), opts)
			if err != nil {
				t.Fatalf("%v/%v: Parse() = %v", profile, compat, err)
			}
			if got := readTree(e); !IsFatal(got.err) {
				t.Errorf("%v/%v: reading the tree = %v, want the framing refusal", profile, compat, got.err)
			}
			if d := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(d, []Code{CodeLimitLineBytes}) {
				t.Errorf("%v/%v: Diagnostics() = %v", profile, compat, d)
			}

			opts.Limits = lineOnly(199)
			session, e, err = Parse(strings.NewReader(encoded.text), opts)
			if err != nil {
				t.Fatalf("%v/%v: Parse() = %v", profile, compat, err)
			}
			if _, err := io.ReadAll(e.Body); !IsFatal(err) {
				t.Errorf("%v/%v: reading the body = %v, want the encoded line refusal", profile, compat, err)
			}
			if d := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(d, []Code{CodeLimitLineBytes}) {
				t.Errorf("%v/%v: Diagnostics() = %v", profile, compat, d)
			}
		}
	}
}

// TestParse_multipartLineBytes_forgedRefusalIsNotDiagnosed pins the same thing
// for framing lines: only this parser's own refusal is diagnosed, and a value
// shaped like one that an input hands back is neither diagnosed nor
// dereferenced.
func TestParse_multipartLineBytes_forgedRefusalIsNotDiagnosed(t *testing.T) {
	tc := framingCases("\r\n")[0]

	for _, lookalike := range []struct {
		name string
		err  error
	}{
		{"line sentinel", textproto.ErrLineTooBig},
		{"wrapped line sentinel", fmt.Errorf("outer: %w", textproto.ErrLineTooBig)},
		{"forged line limit", &textproto.LineLimitError{}},
		{"typed nil line limit", (*textproto.LineLimitError)(nil)},
		{"joined forgeries", errors.Join(&textproto.LineLimitError{}, textproto.ErrLineTooBig)},
		{"match-all Is", matchAllIsError{}},
	} {
		t.Run(lookalike.name, func(t *testing.T) {
			src := &errorAfterReader{
				s:   tc.fixture.text,
				n:   int(tc.fixture.offset(t, "root.body")),
				err: lookalike.err,
			}

			session, e, err := Parse(src, &ParseOptions{Limits: lineOnly(1 << 20)})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			got := readTree(e)
			if got.err == nil {
				t.Fatal("reading the tree succeeded, want the input's failure")
			}
			_ = got.err.Error()
			if parserProducedFatal(got.err) {
				t.Errorf("err = %v, want an ordinary input error, not a parse failure", got.err)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("Diagnostics() = %v, want none: no budget was reached", codesOfDiags(d))
			}
		})
	}
}
