package message

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Tests for Limits.MaxInputBytes (ROB-001-009). See limit_input.go.

// inputMeter counts what its source hands out, so that a test can prove the
// parse never took more than one byte past its input budget, and how often the
// source was asked.
type inputMeter struct {
	r     io.Reader
	read  int64
	calls int
}

func (m *inputMeter) Read(p []byte) (int, error) {
	m.calls++
	n, err := m.r.Read(p)
	m.read += int64(n)
	return n, err
}

// inputSource hands out s in reads of at most chunk bytes (0 for no bound), and
// ends it with final, which is io.EOF unless set. It covers the deliveries the
// input budget must not depend on: final together with the last bytes or on the
// read after them, and a (0, nil) read before every read that delivers.
type inputSource struct {
	s        string
	chunk    int
	final    error
	withData bool // final comes with the last bytes rather than after them
	zeros    bool // a (0, nil) read precedes every delivering read
	zeroNext bool
}

func (r *inputSource) Read(p []byte) (int, error) {
	final := r.final
	if final == nil {
		final = io.EOF
	}
	if len(r.s) == 0 {
		return 0, final
	}
	if r.zeros {
		r.zeroNext = !r.zeroNext
		if r.zeroNext {
			return 0, nil
		}
	}
	n := len(p)
	if r.chunk > 0 && n > r.chunk {
		n = r.chunk
	}
	n = copy(p[:n], r.s)
	r.s = r.s[n:]
	if len(r.s) == 0 && r.withData {
		return n, final
	}
	return n, nil
}

// inputDeliveries are the ways the same input is delivered for the boundary
// tests, each ending in io.EOF.
func inputDeliveries(text string) map[string]func() io.Reader {
	out := map[string]func() io.Reader{
		"whole":            func() io.Reader { return &inputSource{s: text} },
		"whole, eof":       func() io.Reader { return &inputSource{s: text, withData: true} },
		"zero reads":       func() io.Reader { return &inputSource{s: text, chunk: 5, zeros: true} },
		"zero reads, eof":  func() io.Reader { return &inputSource{s: text, chunk: 3, zeros: true, withData: true} },
		"iotest data+eof":  func() io.Reader { return iotest.DataErrReader(strings.NewReader(text)) },
		"iotest one byte":  func() io.Reader { return iotest.OneByteReader(strings.NewReader(text)) },
		"iotest half":      func() io.Reader { return iotest.HalfReader(strings.NewReader(text)) },
		"random chunks 17": func() io.Reader { return newRandomChunkReader(text, 17, 9) },
	}
	for _, n := range []int{1, 2, 3, 7, 4096} {
		out[fmt.Sprintf("chunks of %d", n)] = func() io.Reader { return &inputSource{s: text, chunk: n} }
		out[fmt.Sprintf("chunks of %d, eof", n)] = func() io.Reader { return &inputSource{s: text, chunk: n, withData: true} }
	}
	return out
}

// parseInput parses src under an input budget of limit and the default
// everything else.
func parseInput(src io.Reader, limit int64, sink DiagnosticSink) (*ParseSession, *Entity, error) {
	return Parse(src, &ParseOptions{Limits: Limits{MaxInputBytes: limit}, DiagnosticSink: sink})
}

// checkInputDiagnostic pins the one diagnostic of an overflow: its code,
// severity and action, the message as a whole as its path, and offset limit,
// the first byte not admitted. It also checks that the stop carries the same
// diagnostic.
func checkInputDiagnostic(t *testing.T, session *ParseSession, limit int64) {
	t.Helper()

	diags := session.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one limit.input_bytes", diags)
	}
	d := diags[0]
	if d.Code != CodeLimitInputBytes || d.Severity != SeverityLimitExceeded {
		t.Errorf("diagnostic = %v, want %s at %v", d, CodeLimitInputBytes, SeverityLimitExceeded)
	}
	if d.Path != nil {
		t.Errorf("Path = %v, want nil: the reader at the source names no part", d.Path)
	}
	if want := LocationAt(limit); d.Location != want {
		t.Errorf("Location = %v, want %v", d.Location, want)
	}
	if d.Action != inputBytesAction || d.Field != "" || d.Cause != nil {
		t.Errorf("diagnostic = %+v, want the input action, no field and no cause", d)
	}

	stop := session.fatalErr()
	if stop == nil {
		t.Fatal("no published stop after an overflow")
	}
	fd, ok := stop.Diagnostic()
	if !ok {
		t.Fatalf("stop %v carries no diagnostic", stop)
	}
	want := d
	want.Severity = SeverityFatal
	if !reflect.DeepEqual(fd, want) {
		t.Errorf("stop diagnostic = %+v, want %+v", fd, want)
	}
}

// TestParse_inputBytesRootBody is the reported defect: a body past the input
// budget was returned whole with no error and no diagnostic. A budget of the
// message's length, or more, reads it whole; one byte less returns every byte
// admitted, then the session's stop, identically on every later read.
func TestParse_inputBytesRootBody(t *testing.T) {
	const header = "Subject: x\r\n\r\n"
	text := header + strings.Repeat("x", 128)
	size := int64(len(text))

	for name, src := range inputDeliveries(text) {
		for _, limit := range []int64{size - 1, size, size + 1} {
			t.Run(fmt.Sprintf("%s/limit %d", name, limit-size), func(t *testing.T) {
				meter := &inputMeter{r: src()}
				session, e, err := parseInput(meter, limit, nil)
				if err != nil || e == nil {
					t.Fatalf("Parse() = (%v, %v), want the header: it fits", e, err)
				}
				if raw := headerBytes(t, e); raw != header {
					t.Errorf("header = %q, want %q unchanged", raw, header)
				}

				body, err := io.ReadAll(e.Body)
				if limit >= size {
					if err != nil || string(body) != text[len(header):] {
						t.Errorf("body = (%d bytes, %v), want all %d bytes", len(body), err, size-int64(len(header)))
					}
					if d := session.Diagnostics(); d != nil || e.ParseStop() != nil {
						t.Errorf("Diagnostics() = %v, stop = %v, want neither within the budget", d, e.ParseStop())
					}
					if meter.read != size {
						t.Errorf("source handed out %d bytes, want %d", meter.read, size)
					}
					return
				}

				if want := text[len(header):limit]; string(body) != want {
					t.Errorf("body = %q, want the %d admitted bytes %q", body, len(want), want)
				}
				stop := e.ParseStop()
				if stop == nil || err != stop {
					t.Fatalf("body error = %v, want the identical stop %v", err, stop)
				}
				checkInputDiagnostic(t, session, limit)
				if meter.read > limit+1 {
					t.Errorf("source handed out %d bytes, want at most %d", meter.read, limit+1)
				}

				// Asking again reads nothing and records nothing.
				read, calls := meter.read, meter.calls
				for i := range 3 {
					if n, err := e.Body.Read(make([]byte, 16)); n != 0 || err != stop {
						t.Errorf("read %d after the stop = (%d, %v), want (0, the stop)", i, n, err)
					}
				}
				if meter.read != read || meter.calls != calls || session.Len() != 1 {
					t.Errorf("after the stop: source %d bytes/%d calls, %d diagnostics; want %d/%d and 1", meter.read, meter.calls, session.Len(), read, calls)
				}
			})
		}
	}
}

// headerBytes serializes e's header, which for an unmodified header is the
// input's bytes.
func headerBytes(t *testing.T, e *Entity) string {
	t.Helper()
	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, e.Header.Header); err != nil {
		t.Fatalf("WriteHeader() = %v", err)
	}
	return b.String()
}

// TestParse_inputBytesRootHeader puts the boundary inside and at the end of the
// message's own header block. A block that ends within the budget is returned
// whole even when nothing after it fits, and the block itself is never what
// proves an overflow: deciding that it ended looks at no byte past its own
// empty line. A block the budget cuts short yields no entity and the stop.
func TestParse_inputBytesRootHeader(t *testing.T) {
	cases := []struct {
		name string
		text string
		// complete is the length of the shortest prefix that holds a whole
		// header block.
		complete int64
	}{
		{"crlf, nothing after", "Subject: x\r\n\r\n", 14},
		{"lf, nothing after", "Subject: x\n\n", 12},
		{"crlf, body after", "Subject: x\r\n\r\nbody", 14},
		{"lf, body after", "Subject: x\n\nbody", 12},
		{"no empty line", "Subject: x\r\n", 12},
		{"long field", "Subject: " + strings.Repeat("s", 300) + "\r\nTo: y\r\n\r\nbody", 9 + 300 + 2 + 7 + 2},
	}

	for _, tc := range cases {
		for name, src := range inputDeliveries(tc.text) {
			for _, limit := range []int64{1, tc.complete - 1, tc.complete, tc.complete + 1, int64(len(tc.text))} {
				t.Run(fmt.Sprintf("%s/%s/limit %d", tc.name, name, limit), func(t *testing.T) {
					meter := &inputMeter{r: src()}
					session, e, err := parseInput(meter, limit, nil)
					if meter.read > limit+1 {
						t.Errorf("source handed out %d bytes, want at most %d", meter.read, limit+1)
					}

					if limit < tc.complete {
						stop := session.fatalErr()
						if e != nil || stop == nil || err != stop {
							t.Fatalf("Parse() = (%v, %v), want no entity and the identical stop %v", e, err, stop)
						}
						checkInputDiagnostic(t, session, limit)
						return
					}

					if err != nil || e == nil {
						t.Fatalf("Parse() = (%v, %v), want the header: it ended within the budget", e, err)
					}
					if session.Len() != 0 {
						t.Errorf("Diagnostics() = %v after the header, want none: nothing past it was read", session.Diagnostics())
					}
					// Every field is kept whole. The header layer writes an LF field,
					// and the empty line after the fields, with CRLF, so line endings
					// are compared normalized.
					lf := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
					if raw := strings.TrimSuffix(headerBytes(t, e), "\r\n"); !strings.HasPrefix(lf(tc.text), lf(raw)) {
						t.Errorf("header fields = %q, want a verbatim prefix of the input", raw)
					}
					body, err := io.ReadAll(e.Body)
					rest := tc.text[min(int(tc.complete), len(tc.text)):]
					if limit >= int64(len(tc.text)) {
						if err != nil || string(body) != rest {
							t.Errorf("body = (%q, %v), want (%q, nil)", body, err, rest)
						}
						return
					}
					if want := tc.text[tc.complete:limit]; string(body) != want || err != session.fatalErr() || err == nil {
						t.Errorf("body = (%q, %v), want (%q, the stop)", body, err, want)
					}
					checkInputDiagnostic(t, session, limit)
				})
			}
		}
	}
}

// TestParse_inputBytesHeaderBudgetOrder fixes which budget ends a parse when a
// header block would exceed both a header budget and the input budget. A header
// budget is judged on the bytes its reader was given, so one decided within
// what the input budget admitted is reported as itself and the input budget is
// never consulted; when the header reader needs a byte the input budget
// refuses before it could decide, the input budget is what ended the parse.
// Either way exactly one diagnostic is recorded.
func TestParse_inputBytesHeaderBudgetOrder(t *testing.T) {
	// The field's line ends at offset 311, past a header byte budget of 100.
	text := "Subject: " + strings.Repeat("s", 300) + "\r\n\r\nbody"

	cases := []struct {
		name  string
		input int64
		want  Code
	}{
		{"line ends within the input budget", 400, CodeLimitHeaderBytes},
		{"input budget ends inside the line", 200, CodeLimitInputBytes},
	}
	for _, tc := range cases {
		for name, src := range inputDeliveries(text) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				session, e, err := Parse(src(), &ParseOptions{Limits: Limits{
					MaxInputBytes:  tc.input,
					MaxHeaderBytes: 100,
				}})
				if e != nil || !IsFatal(err) {
					t.Fatalf("Parse() = (%v, %v), want no entity and a FatalError", e, err)
				}
				if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{tc.want}) {
					t.Errorf("diagnostics = %v, want [%s]", got, tc.want)
				}
				d, _ := err.(FatalError).Diagnostic()
				if d.Code != tc.want {
					t.Errorf("err = %v, want %s", err, tc.want)
				}
				// Only the input budget stops the whole session.
				if stopped := session.fatalErr() != nil; stopped != (tc.want == CodeLimitInputBytes) {
					t.Errorf("session stopped = %v for %s", stopped, tc.want)
				}
			})
		}
	}
}

// inputTreeMessage is a nested message with a preamble, an epilogue, a leaf
// before and after a nested multipart, and an encoded leaf inside it.
const inputTreeMessage = "Content-Type: multipart/mixed; boundary=OUTER\r\n\r\n" +
	"preamble\r\n" +
	"--OUTER\r\n" +
	"Content-Type: text/plain\r\n\r\n" +
	"first first first first first first first first first first\r\n" +
	"--OUTER\r\n" +
	"Content-Type: multipart/alternative; boundary=INNER\r\n\r\n" +
	"--INNER\r\n" +
	"Content-Type: text/plain\r\n\r\n" +
	"deep zero deep zero deep zero deep zero\r\n" +
	"--INNER\r\n" +
	"Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
	"ZGVlcCBvbmU=\r\n" +
	"--INNER--\r\n" +
	"--OUTER\r\n" +
	"Content-Type: text/plain\r\n\r\n" +
	"last\r\n" +
	"--OUTER--\r\n" +
	"an epilogue no traversal reads\r\n"

// inputOutcome is everything a traversal under an input budget observed, in a
// form that compares across deliveries of the same input.
type inputOutcome struct {
	parseErr string
	bodies   []string
	treeErr  string
	diags    []string
}

// errClass names err relative to the session's stop: the stop itself, an error
// wrapping it, or, for any other error, its type and nothing more.
//
// The type rather than the text, because some texts depend on chunking for
// reasons this budget has nothing to do with: encoding/base64 reports the
// offset of a corrupt byte within the read that met it, so the same body read
// whole and one byte at a time names different offsets, with or without any
// input budget.
func errClass(err error, session *ParseSession) string {
	if err == nil {
		return ""
	}
	if stop := session.fatalErr(); stop != nil {
		if err == stop {
			return "stop"
		}
		if errors.Is(err, stop) && IsFatal(err) {
			return "wraps stop"
		}
	}
	return fmt.Sprintf("%T", err)
}

func diagFacts(session *ParseSession) []string {
	var out []string
	for _, d := range session.Diagnostics() {
		out = append(out, fmt.Sprintf("%s path=%v %v", d.Code, d.Path, d.Location))
	}
	return out
}

// traverseInput parses src under limit, reads every leaf it reaches, and
// checks the invariants every outcome shares: never more than one byte past the
// budget from the source, at most one limit.input_bytes, at offset limit and
// only for an input that has a byte there, and a session stop exactly when one
// was recorded.
func traverseInput(t *testing.T, text string, src io.Reader, limit int64) inputOutcome {
	t.Helper()

	meter := &inputMeter{r: src}
	session, e, err := parseInput(meter, limit, nil)
	out := inputOutcome{parseErr: errClass(err, session)}
	if e != nil {
		got := readTree(e)
		out.bodies, out.treeErr = got.bodies, errClass(got.err, session)
	}
	out.diags = diagFacts(session)

	if meter.read > limit+1 {
		t.Errorf("source handed out %d bytes under a budget of %d", meter.read, limit)
	}
	var inputDiags int
	for _, d := range session.Diagnostics() {
		if d.Code != CodeLimitInputBytes {
			continue
		}
		inputDiags++
		if d.Location != LocationAt(limit) || int64(len(text)) <= limit {
			t.Errorf("%v for a %d byte input under a budget of %d", d, len(text), limit)
		}
	}
	if inputDiags > 1 {
		t.Errorf("%d limit.input_bytes diagnostics, want at most one", inputDiags)
	}
	if (inputDiags == 1) != (session.fatalErr() != nil) {
		t.Errorf("stop = %v with %d input diagnostics", session.fatalErr(), inputDiags)
	}
	return out
}

// TestParse_inputBytesTreeEveryBoundary moves the budget across every byte of a
// nested message and compares what a full traversal observes across every
// delivery of it. Whatever the budget cuts — the root header, a framing line, a
// part header, a leaf, the encoded leaf, the closing delimiter — the outcome is
// the same however the input is chunked. The epilogue after the closing
// delimiter is never read, so a budget ending inside it is never exceeded.
func TestParse_inputBytesTreeEveryBoundary(t *testing.T) {
	text := inputTreeMessage
	size := int64(len(text))
	epilogue := int64(strings.Index(text, "an epilogue"))
	deliveries := inputDeliveries(text)

	for limit := int64(1); limit <= size+1; limit++ {
		want := traverseInput(t, text, strings.NewReader(text), limit)

		full := []string{"first first first first first first first first first first", "deep zero deep zero deep zero deep zero", "deep one", "last"}
		switch {
		case limit >= epilogue:
			if want.parseErr != "" || want.treeErr != "" || want.diags != nil || !reflect.DeepEqual(want.bodies, full) {
				t.Errorf("limit %d: %+v, want the whole tree and nothing more: the epilogue is never read", limit, want)
			}
		default:
			if len(want.diags) != 1 {
				t.Errorf("limit %d: %+v, want one limit.input_bytes", limit, want)
			}
		}

		for name, src := range deliveries {
			if got := traverseInput(t, text, src(), limit); !reflect.DeepEqual(got, want) {
				t.Errorf("limit %d, %s:\n got %+v\nwant %+v", limit, name, got, want)
			}
		}
	}
}

// TestParse_inputBytesSkippedParts never reads a body: every part is skipped,
// so the bytes past the budget are met while a multipart iterator drains a part
// the caller left unread. That drain is a read of the same input and is held to
// the same budget, and the iterator reports the identical stop.
func TestParse_inputBytesSkippedParts(t *testing.T) {
	text := inputTreeMessage
	inFirst := int64(strings.Index(text, "first first")) + 20
	inDeep := int64(strings.Index(text, "deep zero")) + 10

	for name, src := range inputDeliveries(text) {
		t.Run("outer skip/"+name, func(t *testing.T) {
			meter := &inputMeter{r: src()}
			session, e, err := parseInput(meter, inFirst, nil)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			mr := e.MultipartReader()
			first, err := mr.NextPart()
			if err != nil || first == nil {
				t.Fatalf("NextPart() = (%v, %v), want the first part", first, err)
			}
			// The first part's body is never read: the next call drains it.
			p, err := mr.NextPart()
			stop := e.ParseStop()
			if p != nil || stop == nil || err != stop {
				t.Fatalf("NextPart() = (%v, %v), want (nil, the identical stop %v)", p, err, stop)
			}
			checkInputDiagnostic(t, session, inFirst)
			for range 3 {
				if p, err := mr.NextPart(); p != nil || err != stop {
					t.Errorf("NextPart() again = (%v, %v), want the stop", p, err)
				}
			}
			if meter.read > inFirst+1 || session.Len() != 1 {
				t.Errorf("source %d bytes, %d diagnostics; want at most %d and 1", meter.read, session.Len(), inFirst+1)
			}
			// The part already returned stays the caller's: its admitted bytes are
			// gone to the drain, and its body reports the stop rather than a clean
			// end.
			if _, err := io.ReadAll(first.Body); !errors.Is(err, stop) {
				t.Errorf("first body after the stop: %v, want the stop", err)
			}
		})

		t.Run("deep skip/"+name, func(t *testing.T) {
			meter := &inputMeter{r: src()}
			session, e, err := parseInput(meter, inDeep, nil)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			outer := e.MultipartReader()
			for _, want := range []string{"first", "nested"} {
				if p, err := outer.NextPart(); p == nil || err != nil {
					t.Fatalf("NextPart() for %s = (%v, %v)", want, p, err)
				} else if want == "nested" {
					inner := p.MultipartReader()
					if c, err := inner.NextPart(); c == nil || err != nil {
						t.Fatalf("inner NextPart() = (%v, %v), want the deep part", c, err)
					}
				}
			}
			// The deep part, its sibling and the rest of the nested multipart are
			// all skipped by the outer iterator.
			p, err := outer.NextPart()
			stop := e.ParseStop()
			if p != nil || stop == nil || err != stop {
				t.Fatalf("outer NextPart() = (%v, %v), want the identical stop", p, err)
			}
			checkInputDiagnostic(t, session, inDeep)
			if meter.read > inDeep+1 {
				t.Errorf("source handed out %d bytes, want at most %d", meter.read, inDeep+1)
			}
		})
	}
}

// TestParse_inputBytesPartBody reads a leaf across the budget. Every byte up to
// the budget is delivered, and the read then fails with an error that is a
// FatalError wrapping the stop; the multipart framing adds that the part's
// closing delimiter was not found.
func TestParse_inputBytesPartBody(t *testing.T) {
	text := inputTreeMessage
	start := int64(strings.Index(text, "first first"))
	limit := start + 25

	for name, src := range inputDeliveries(text) {
		t.Run(name, func(t *testing.T) {
			session, e, err := parseInput(src(), limit, nil)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			p, err := e.MultipartReader().NextPart()
			if err != nil {
				t.Fatalf("NextPart() = %v", err)
			}
			body, err := io.ReadAll(p.Body)
			stop := session.fatalErr()
			if want := text[start:limit]; string(body) != want {
				t.Errorf("body = %q, want every admitted byte %q", body, want)
			}
			if stop == nil || !errors.Is(err, stop) || !IsFatal(err) || !errors.Is(err, textproto.ErrMissingBoundaryClose) {
				t.Errorf("body error = %v, want the stop %v with the missing delimiter", err, stop)
			}
			checkInputDiagnostic(t, session, limit)
		})
	}
}

// TestParse_inputBytesWalk ends a walk at the identical stop, after handing
// walkFunc every part that was reached before it.
//
// A part header that ends exactly at the budget is not enough to return a
// nested part: its last line terminator could be the start of the enclosing
// multipart's next delimiter, and only the byte after it can say, so the walk
// stops one part earlier there than when the budget ends inside that part's
// body.
func TestParse_inputBytesWalk(t *testing.T) {
	text := inputTreeMessage
	encoded := int64(strings.Index(text, "ZGVlcCBvbmU="))

	cases := []struct {
		name  string
		limit int64
		want  []string
	}{
		{"inside the encoded body", encoded + 4, []string{"[]", "[0]", "[1]", "[1 0]", "[1 1]"}},
		{"at the end of its header block", encoded, []string{"[]", "[0]", "[1]", "[1 0]"}},
	}
	for _, tc := range cases {
		for name, src := range inputDeliveries(text) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				session, e, err := parseInput(src(), tc.limit, nil)
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				var paths []string
				err = e.Walk(func(path []int, part *Entity, err error) error {
					paths = append(paths, fmt.Sprint(path))
					return err
				})
				if stop := e.ParseStop(); stop == nil || err != stop {
					t.Fatalf("Walk() = %v, want the identical stop %v", err, stop)
				}
				if !reflect.DeepEqual(paths, tc.want) {
					t.Errorf("walked %v, want %v", paths, tc.want)
				}
				checkInputDiagnostic(t, session, tc.limit)
			})
		}
	}
}

// TestParse_inputBytesCopyPaths reads past the budget through the copying
// paths a caller reaches for: io.Copy into a writer that implements
// io.ReaderFrom, and Entity.WriteTo, which copies an unchanged body verbatim.
// Neither reads around the budget, and both end with the stop after writing
// every admitted byte.
func TestParse_inputBytesCopyPaths(t *testing.T) {
	const header = "Subject: x\r\n\r\n"
	text := header + strings.Repeat("y", 10000)
	const limit = 5000

	t.Run("io.Copy to io.Discard", func(t *testing.T) {
		meter := &inputMeter{r: strings.NewReader(text)}
		session, e, err := parseInput(meter, limit, nil)
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		n, err := io.Copy(io.Discard, e.Body)
		if stop := session.fatalErr(); stop == nil || err != stop {
			t.Errorf("io.Copy() = %v, want the stop %v", err, stop)
		}
		if n != limit-int64(len(header)) || meter.read > limit+1 {
			t.Errorf("copied %d, source %d; want %d and at most %d", n, meter.read, limit-len(header), limit+1)
		}
		checkInputDiagnostic(t, session, limit)
	})

	t.Run("WriteTo", func(t *testing.T) {
		meter := &inputMeter{r: strings.NewReader(text)}
		session, e, err := parseInput(meter, limit, nil)
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		var out bytes.Buffer
		err = e.WriteTo(&out)
		if stop := session.fatalErr(); stop == nil || !errors.Is(err, stop) {
			t.Errorf("WriteTo() = %v, want the stop %v", err, stop)
		}
		// The writer adds the fields it always adds to the header; the body is
		// what is copied verbatim.
		written := out.String()
		body := written[strings.Index(written, "\r\n\r\n")+4:]
		if want := text[len(header):limit]; body != want || meter.read > limit+1 {
			t.Errorf("wrote a %d byte body, source %d; want the %d admitted body bytes verbatim and at most %d read", len(body), meter.read, len(want), limit+1)
		}
		checkInputDiagnostic(t, session, limit)
	})
}

// TestParse_inputBytesSourceErrors pins how the source's own results meet the
// budget. An error that comes with the last admitted bytes, or on the read
// after them with no byte, is the source's and is passed on with no
// diagnostic, as is an io.EOF in either position. A byte past the budget proves
// the overflow whatever error accompanies it.
func TestParse_inputBytesSourceErrors(t *testing.T) {
	const header = "Subject: x\r\n\r\n"
	text := header + "0123456789"
	size := int64(len(text))
	sentinel := errors.New("source failure")

	for _, withData := range []bool{true, false} {
		for _, final := range []error{io.EOF, sentinel} {
			name := fmt.Sprintf("%v/with data %v", final, withData)

			t.Run(name+"/at the budget", func(t *testing.T) {
				src := &inputSource{s: text, chunk: 4, final: final, withData: withData}
				session, e, err := parseInput(src, size, nil)
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				body, err := io.ReadAll(e.Body)
				wantErr := final
				if final == io.EOF {
					wantErr = nil
				}
				if string(body) != "0123456789" || err != wantErr {
					t.Errorf("body = (%q, %v), want all of it and %v", body, err, wantErr)
				}
				if session.Len() != 0 || session.fatalErr() != nil {
					t.Errorf("Diagnostics() = %v, want none: the budget was not exceeded", session.Diagnostics())
				}
			})

			t.Run(name+"/one byte over", func(t *testing.T) {
				src := &inputSource{s: text, chunk: 4, final: final, withData: withData}
				session, e, err := parseInput(src, size-1, nil)
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				body, err := io.ReadAll(e.Body)
				if string(body) != "012345678" || err == nil || err != session.fatalErr() {
					t.Errorf("body = (%q, %v), want 9 bytes and the stop", body, err)
				}
				checkInputDiagnostic(t, session, size-1)
			})
		}
	}
}

// TestParse_inputBytesSinkStop lets the sink refuse the overflow's diagnostic.
// The parse then ends with the sink's own stop, which every scope reports, and
// the sink may inspect the session and ask the entity about the stop while it
// is being told, without waiting on anything.
//
// Everything is judged by what a caller can observe: ParseStop, the errors the
// traversal returned, and what the sink and the session were given. The stream
// is read by one goroutine only, the traversal's; the goroutine exists to bound
// how long the test waits, not to read the stream concurrently.
func TestParse_inputBytesSinkStop(t *testing.T) {
	text := inputTreeMessage
	limit := int64(strings.Index(text, "deep zero")) + 5
	sinkErr := errors.New("enough")

	var e *Entity
	var seen []string
	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		seen = append(seen, fmt.Sprintf("%s len=%d diags=%d stop=%v", d.Code, s.Len(), len(s.Diagnostics()), e.ParseStop()))
		return sinkErr
	})

	session, root, err := parseInput(strings.NewReader(text), limit, sink)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	e = root

	done := make(chan treeResult, 1)
	go func() { done <- readTree(root) }()
	var got treeResult
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the traversal did not finish: a sink inspecting the session must not wait")
	}

	stop := root.ParseStop()
	if stop == nil || !errors.Is(stop, sinkErr) || !IsFatal(stop) {
		t.Fatalf("ParseStop() = %v, want a FatalError wrapping the sink's error", stop)
	}
	var fatal FatalError
	if errors.As(stop, &fatal) {
		if _, ok := fatal.Diagnostic(); ok {
			t.Errorf("stop %v carries a diagnostic, want the sink's own stop", stop)
		}
	}
	if !errors.Is(got.err, stop) {
		t.Errorf("traversal = %v, want the stop", got.err)
	}
	// Later scopes report the identical value.
	if p, err := root.MultipartReader().NextPart(); p != nil || err != stop {
		t.Errorf("NextPart() on a fresh iterator = (%v, %v), want the identical stop", p, err)
	}
	if want := []string{"limit.input_bytes len=1 diags=1 stop=<nil>"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("sink saw %v, want %v", seen, want)
	}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitInputBytes}) {
		t.Errorf("diagnostics = %v, want the one overflow", codes)
	}
}

// TestParse_inputBytesAfterStop overflows a parse that a sink had already
// stopped. A body already returned stays readable after a stop — the existing
// salvage of what a scope returned, which this item keeps — and it is still held
// to the budget: the read past the budget reports the stop that was published
// first, and nothing is recorded about a parse that had already ended.
func TestParse_inputBytesAfterStop(t *testing.T) {
	header := "Content-Type: text/plain\r\nContent-Transfer-Encoding: x-unknown\r\n\r\n"
	text := header + strings.Repeat("z", 100)
	limit := int64(len(text) - 50)
	sinkErr := errors.New("stop at the first diagnostic")
	sink := DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return sinkErr })

	meter := &inputMeter{r: strings.NewReader(text)}
	session, e, err := parseInput(meter, limit, sink)
	if e == nil {
		t.Fatalf("Parse() = (nil, %v), want the entity with the sink's stop", err)
	}
	stop := e.ParseStop()
	if stop == nil || err != stop || !errors.Is(stop, sinkErr) {
		t.Fatalf("Parse() error = %v, ParseStop() = %v, want the sink's identical stop", err, stop)
	}
	body, err := io.ReadAll(e.Body)
	if string(body) != text[len(header):limit] || err != stop {
		t.Errorf("body = (%d bytes, %v), want the 50 admitted bytes and the earlier stop", len(body), err)
	}
	if e.ParseStop() != stop {
		t.Errorf("ParseStop() changed to %v after the overflow, want %v", e.ParseStop(), stop)
	}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeCTEUnknown}) {
		t.Errorf("diagnostics = %v, want only the one that stopped the parse", codes)
	}
	if meter.read > limit+1 {
		t.Errorf("source handed out %d bytes, want at most %d", meter.read, limit+1)
	}
}

// TestParse_inputBytesPrefetch pins what counts: raw bytes read from the input,
// not bytes interpreted. Reading the header block fetches body bytes with it,
// up to the budget, and those count — but reaching exactly the budget that way
// is not an overflow and costs no proof read, and the complete header survives
// the overflow the first body read then proves.
func TestParse_inputBytesPrefetch(t *testing.T) {
	const header = "Subject: x\r\nTo: y\r\n\r\n"
	text := header + strings.Repeat("p", 200)

	for name, src := range inputDeliveries(text) {
		for _, limit := range []int64{int64(len(header)) + 50, int64(len(text))} {
			t.Run(fmt.Sprintf("%s/limit %d", name, limit), func(t *testing.T) {
				meter := &inputMeter{r: src()}
				session, e, err := parseInput(meter, limit, nil)
				if err != nil || e == nil {
					t.Fatalf("Parse() = (%v, %v)", e, err)
				}
				if meter.read > limit {
					t.Errorf("Parse read %d bytes, want at most the %d admitted: no proof read yet", meter.read, limit)
				}
				if name == "whole" && meter.read != limit {
					t.Errorf("Parse read %d bytes from a whole source, want all %d admitted: prefetched body bytes count", meter.read, limit)
				}
				if session.Len() != 0 || e.ParseStop() != nil {
					t.Errorf("after Parse: %v, stop %v; want neither", session.Diagnostics(), e.ParseStop())
				}

				body, err := io.ReadAll(e.Body)
				if want := text[len(header):limit]; string(body) != want {
					t.Errorf("body = %d bytes, want the %d admitted", len(body), len(want))
				}
				if limit == int64(len(text)) {
					if err != nil || session.Len() != 0 || meter.read != limit {
						t.Errorf("exact: (%v, %d diagnostics, %d read), want a clean end and no byte past it", err, session.Len(), meter.read)
					}
				} else {
					if stop := e.ParseStop(); stop == nil || err != stop {
						t.Errorf("body error = %v, want the stop %v", err, stop)
					}
					if meter.read != limit+1 {
						t.Errorf("source handed out %d bytes, want %d: the admitted ones and one proof byte", meter.read, limit+1)
					}
					checkInputDiagnostic(t, session, limit)
				}
				// The header that ended within the budget is untouched by it.
				if e.Header.Get("Subject") != "x" || e.Header.Get("To") != "y" || headerBytes(t, e) != header {
					t.Errorf("header = %q after the body read, want %q", headerBytes(t, e), header)
				}
			})
		}
	}
}

// TestParse_inputBytesDefaultAndDisabled checks the budget configured with no
// value, LimitDisabled, and the legacy entry points that budget nothing.
func TestParse_inputBytesDefaultAndDisabled(t *testing.T) {
	const header = "Subject: x\r\n\r\n"

	t.Run("resolution", func(t *testing.T) {
		src := strings.NewReader("")
		if got := (*ParseSession)(nil).limitInput(src); got != io.Reader(src) {
			t.Errorf("nil session wrapped the input: %T", got)
		}
		disabled := newParseSessionWithConfig(resolvedConfig{limits: Limits{MaxInputBytes: LimitDisabled}}, diagnosticOptions{})
		if got := disabled.limitInput(src); got != io.Reader(src) {
			t.Errorf("disabled budget wrapped the input: %T", got)
		}
		session, _, _ := Parse(strings.NewReader(header), nil)
		l, ok := session.limitInput(src).(*inputLimiter)
		if !ok || l.limit != defaultMaxInputBytes {
			t.Errorf("default budget = %+v, want %d", l, defaultMaxInputBytes)
		}
	})

	big := header + strings.Repeat("b", 1<<16)
	t.Run("disabled", func(t *testing.T) {
		session, e, err := parseInput(strings.NewReader(big), LimitDisabled, nil)
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		if b, err := io.ReadAll(e.Body); err != nil || len(b) != 1<<16 || session.Len() != 0 {
			t.Errorf("body = (%d, %v), %d diagnostics; want all of it and none", len(b), err, session.Len())
		}
	})
	t.Run("ReadWithOptions", func(t *testing.T) {
		e, err := ReadWithOptions(strings.NewReader(big), &ReadOptions{MaxHeaderBytes: 64})
		if err != nil {
			t.Fatalf("ReadWithOptions() = %v", err)
		}
		if b, err := io.ReadAll(e.Body); err != nil || len(b) != 1<<16 {
			t.Errorf("body = (%d, %v), want all of it", len(b), err)
		}
	})

	t.Run("default enforced", func(t *testing.T) {
		if testing.Short() {
			t.Skip("reads 512 MiB")
		}
		// The body budgets are disabled so that this isolates the input budget:
		// with every default, a single body this large is stopped first by
		// MaxEncodedBodyBytes at 128 MiB (TestParse_bodyBudgetsRealDefaults).
		// Only MaxInputBytes is left at its default.
		bodyOff := &ParseOptions{Limits: Limits{
			MaxEncodedBodyBytes:  LimitDisabled,
			MaxDecodedBodyBytes:  LimitDisabled,
			MaxTotalDecodedBytes: LimitDisabled,
		}}
		for _, extra := range []int64{0, 1} {
			size := int64(defaultMaxInputBytes) + extra
			meter := &inputMeter{r: &repeatReader{header: header, size: size}}
			session, e, err := Parse(meter, bodyOff)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			n, err := io.Copy(io.Discard, e.Body)
			t.Logf("default%+d: copied %d body bytes, source handed out %d, err %v", extra, n, meter.read, err)
			if extra == 0 {
				if err != nil || n != size-int64(len(header)) || session.Len() != 0 {
					t.Errorf("exact default: copied (%d, %v), %d diagnostics", n, err, session.Len())
				}
				continue
			}
			if err == nil || err != session.fatalErr() || n != defaultMaxInputBytes-int64(len(header)) {
				t.Errorf("default+1: copied (%d, %v)", n, err)
			}
			if meter.read > defaultMaxInputBytes+1 {
				t.Errorf("source handed out %d bytes", meter.read)
			}
			checkInputDiagnostic(t, session, defaultMaxInputBytes)
		}
	})
}

// repeatReader is a header followed by filler, size bytes in all, generated
// rather than held.
type repeatReader struct {
	header string
	size   int64
	off    int64
}

var repeatFill = bytes.Repeat([]byte("r"), 32<<10)

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	if rest := r.size - r.off; int64(len(p)) > rest {
		p = p[:rest]
	}
	var n int
	if r.off < int64(len(r.header)) {
		n = copy(p, r.header[r.off:])
	} else {
		n = copy(p, repeatFill)
	}
	r.off += int64(n)
	return n, nil
}

// TestInputLimiter exercises the reader at the source directly, where the edges
// of the io.Reader contract and of int64 can be reached without gigabytes of
// input.
func TestInputLimiter(t *testing.T) {
	newLimiter := func(src io.Reader, limit, admitted int64) (*inputLimiter, *ParseSession) {
		s := newParseSessionWithConfig(resolvedConfig{limits: DefaultLimits()}, diagnosticOptions{})
		return &inputLimiter{r: src, session: s, limit: limit, admitted: admitted}, s
	}

	t.Run("math.MaxInt64", func(t *testing.T) {
		meter := &inputMeter{r: strings.NewReader("abcde")}
		l, s := newLimiter(meter, math.MaxInt64, math.MaxInt64-2)
		buf := make([]byte, 10)
		if n, err := l.Read(buf); n != 2 || err != nil || string(buf[:2]) != "ab" {
			t.Fatalf("Read() = (%d, %v), want the 2 bytes left", n, err)
		}
		if n, err := l.Read(buf); n != 0 || err == nil || err != s.fatalErr() {
			t.Fatalf("Read() = (%d, %v), want the stop", n, err)
		}
		if meter.read != 3 {
			t.Errorf("source handed out %d bytes, want 3", meter.read)
		}
		checkInputDiagnostic(t, s, math.MaxInt64)
	})

	t.Run("fresh at math.MaxInt64", func(t *testing.T) {
		l, s := newLimiter(strings.NewReader("abc"), math.MaxInt64, 0)
		b, err := io.ReadAll(l)
		if string(b) != "abc" || err != nil || s.Len() != 0 {
			t.Errorf("ReadAll() = (%q, %v)", b, err)
		}
	})

	t.Run("empty read", func(t *testing.T) {
		meter := &inputMeter{r: strings.NewReader("abc")}
		l, _ := newLimiter(meter, 3, 0)
		for _, admitted := range []int64{0, 3} {
			l.admitted = admitted
			if n, err := l.Read(nil); n != 0 || err != nil {
				t.Errorf("Read(nil) = (%d, %v)", n, err)
			}
			if n, err := l.Read([]byte{}); n != 0 || err != nil {
				t.Errorf("Read(empty) = (%d, %v)", n, err)
			}
		}
		if meter.calls != 0 {
			t.Errorf("empty reads reached the source %d times", meter.calls)
		}
	})

	t.Run("zero read at the boundary", func(t *testing.T) {
		meter := &inputMeter{r: &inputSource{s: "x", zeros: true}}
		l, s := newLimiter(meter, 2, 2)
		if n, err := l.Read(make([]byte, 8)); n != 0 || err != nil {
			t.Errorf("Read() = (%d, %v), want the source's (0, nil)", n, err)
		}
		if meter.calls != 1 {
			t.Errorf("source asked %d times, want once: (0, nil) is passed on, not retried", meter.calls)
		}
		if n, err := l.Read(make([]byte, 8)); n != 0 || err != s.fatalErr() || err == nil {
			t.Errorf("Read() = (%d, %v), want the stop once a byte arrives", n, err)
		}
	})

	t.Run("source error at the boundary", func(t *testing.T) {
		sentinel := errors.New("down")
		l, s := newLimiter(iotest.ErrReader(sentinel), 4, 4)
		for range 2 {
			if n, err := l.Read(make([]byte, 8)); n != 0 || err != sentinel {
				t.Errorf("Read() = (%d, %v), want the source's error", n, err)
			}
		}
		if s.Len() != 0 || s.fatalErr() != nil {
			t.Errorf("a source error was diagnosed: %v", s.Diagnostics())
		}
	})

	t.Run("proof byte with an error", func(t *testing.T) {
		sentinel := errors.New("down")
		l, s := newLimiter(&inputSource{s: "x", final: sentinel, withData: true}, 4, 4)
		if n, err := l.Read(make([]byte, 8)); n != 0 || err == nil || err != s.fatalErr() || errors.Is(err, sentinel) {
			t.Errorf("Read() = (%d, %v), want the stop alone", n, err)
		}
	})

	t.Run("reads are shortened, never exceeded", func(t *testing.T) {
		meter := &inputMeter{r: strings.NewReader(strings.Repeat("q", 100))}
		l, _ := newLimiter(meter, 10, 0)
		buf := make([]byte, 64)
		if n, err := l.Read(buf); n != 10 || err != nil || meter.read != 10 {
			t.Errorf("Read() = (%d, %v), source %d; want 10 bytes and no more", n, err, meter.read)
		}
	})
}

// FuzzParse_inputBytes parses arbitrary input under an arbitrary budget,
// whole and chunked. Both must keep the budget's universal invariants, which
// traverseInput checks on every run whatever the outcome: never more than one
// byte past the budget from the source, at most one limit.input_bytes, at
// offset N and only for an input with a byte there, and a session stop exactly
// when one was recorded.
//
// The oracle that compares the two outcomes is deliberately scoped. It runs
// only when neither traversal ended on an error other than the budget's stop,
// and it compares other errors by type, not text. Transfer decoding of
// malformed input is not chunk-invariant today, with or without any budget: a
// corrupt base64 body reports an offset relative to the read that met it, and
// one with data after its padding inside a truncated multipart yields a
// different prefix and error read one byte at a time than read whole. That
// reproduces on the pre-ROB-001-009 baseline 6fb8b93 under a disabled budget
// too, and is tracked as issue #34 (ROB-070). This fuzz therefore does not
// claim chunk invariance for malformed decoder input; it claims it only for the
// input budget and for traversals the decoders did not end.
func FuzzParse_inputBytes(f *testing.F) {
	f.Add([]byte(inputTreeMessage), uint16(200), uint8(3))
	f.Add([]byte(inputTreeMessage), uint16(len(inputTreeMessage)), uint8(1))
	f.Add([]byte("Subject: x\r\n\r\nbody"), uint16(14), uint8(2))
	f.Add([]byte("Subject: x\n\n"), uint16(11), uint8(7))
	f.Add([]byte("Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nZGVlcCBvbmU=\r\n"), uint16(60), uint8(4))
	// A corrupt base64 body under no effective budget: its error text names a
	// chunk-relative offset, which errClass deliberately does not compare.
	f.Add([]byte("Content-TrAnsfer-EnCoding: BAse64 \n\n0000!00000000000"), uint16(482), uint8(1))
	// A base64 body with data after its padding, inside an unterminated nested
	// multipart: its decoder fails differently by chunking (issue #34), so the
	// two traversals are checked against the invariants but not compared.
	f.Add([]byte("Content-Type: multipart/mixed; boundary=O\n\n--O\nContent-Type: multipart/mixed; boundary=I\n\n--I\nContent-Transfer-Encoding: base64\n\n00000000000=0000000000000000"), uint16(400), uint8(1))

	// comparable reports whether a traversal ended at its natural end or at the
	// budget's stop, rather than at an error of its own.
	comparable := func(o inputOutcome) bool {
		for _, class := range []string{o.parseErr, o.treeErr} {
			if class != "" && class != "stop" && class != "wraps stop" {
				return false
			}
		}
		return true
	}

	f.Fuzz(func(t *testing.T, data []byte, n uint16, chunk uint8) {
		if len(data) > 8<<10 {
			return
		}
		text := string(data)
		limit := 1 + int64(n)%int64(len(text)+2)
		want := traverseInput(t, text, strings.NewReader(text), limit)
		src := &inputSource{s: text, chunk: 1 + int(chunk%16), withData: chunk%2 == 0, zeros: chunk%5 == 0}
		got := traverseInput(t, text, src, limit)
		if comparable(want) && comparable(got) && !reflect.DeepEqual(got, want) {
			t.Errorf("chunk %d, limit %d:\n got %+v\nwant %+v", chunk, limit, got, want)
		}
	})
}
