package message

import (
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Limits.MaxPartCount: how many child parts one parse may discover in the whole
// message.
//
// The budget is one number for the whole tree, so the tests below say it twice
// over: once about a single container wide enough to spend it by itself, and
// once about a tree whose containers are each small and whose total is not. A
// counter reset when the parser enters a container would pass the first and fail
// the second, which is the point of writing both.
//
// The other half is the scope of the failure. A part-count refusal is not a
// property of the container that happened to hit it — the budget belongs to the
// parse — so every later scope of that parse reports the identical error, and
// the diagnostic is recorded once for the whole message.

// The ceiling the generated wide inputs here run under.
//
// A budget test must not need an enormous message to prove a budget works, and
// must not cost an enormous amount when the budget stops working. Everything
// generated below is bounded by this, and the one case that deliberately runs
// past the documented default stays a few parts past it rather than a multiple
// of it.
const widePartCap = 20000

// partsOnly returns every budget disabled except the total part budget, set to
// n.
func partsOnly(n int64) Limits {
	l := allDisabled()
	l.MaxPartCount = n
	return l
}

// wideContainer returns a message whose single multipart holds n text leaves,
// each header block labelled "part<i>.header" and each body "p<i>".
func wideContainer(n int, eol string) originFixture {
	if n < 1 || n > widePartCap {
		panic(fmt.Sprintf("wideContainer: %d parts is outside [1, %d]", n, widePartCap))
	}

	segs := [][2]string{
		{"root.header", "Content-Type: multipart/mixed; boundary=B" + eol},
		{"", eol},
	}
	for i := 0; i < n; i++ {
		segs = append(segs,
			[2]string{"", "--B" + eol},
			[2]string{fmt.Sprintf("part%d.header", i), "Content-Type: text/plain" + eol},
			[2]string{"", eol},
			[2]string{"", fmt.Sprintf("p%d", i) + eol},
		)
	}
	segs = append(segs, [2]string{"", "--B--" + eol})
	return newOriginFixture(segs...)
}

// wideBodies is what reading wideContainer(n) yields.
func wideBodies(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("p%d", i)
	}
	return out
}

// wideText is wideContainer without the labels, for the cases that need many
// more parts than a test could name one at a time.
func wideText(n int, eol string) string {
	if n < 1 || n > widePartCap {
		panic(fmt.Sprintf("wideText: %d parts is outside [1, %d]", n, widePartCap))
	}

	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B" + eol + eol)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "--B%sContent-Type: text/plain%s%sp%d%s", eol, eol, eol, i, eol)
	}
	b.WriteString("--B--" + eol)
	return b.String()
}

// spreadTree returns a message whose parts are spread across nested and sibling
// containers, so that no one container is wide and the tree as a whole is.
//
// Seven parts are discovered in it, in the order a caller consuming the message
// reaches them: 0, 0.0, 0.1, 1, 1.0, 1.1, 2. The widest container holds three
// children, which is the point — a budget the whole tree exceeds is one no
// single container does.
func spreadTree(eol string) originFixture {
	return newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=OUTER" + eol},
		[2]string{"", eol},
		[2]string{"root.body", "--OUTER" + eol},
		[2]string{"0.header", "Content-Type: multipart/alternative; boundary=A" + eol},
		[2]string{"", eol},
		[2]string{"0.body", "--A" + eol},
		[2]string{"0.0.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"0.0.body", "a0" + eol},
		[2]string{"", "--A" + eol},
		[2]string{"0.1.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"0.1.body", "a1" + eol},
		[2]string{"", "--A--" + eol},
		[2]string{"", "--OUTER" + eol},
		[2]string{"1.header", "Content-Type: multipart/alternative; boundary=C" + eol},
		[2]string{"", eol},
		[2]string{"1.body", "--C" + eol},
		[2]string{"1.0.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"1.0.body", "c0" + eol},
		[2]string{"", "--C" + eol},
		[2]string{"1.1.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"1.1.body", "c1" + eol},
		[2]string{"", "--C--" + eol},
		[2]string{"", "--OUTER" + eol},
		[2]string{"2.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"2.body", "t" + eol},
		[2]string{"", "--OUTER--" + eol},
	)
}

// spreadOrder is the path of every part spreadTree yields, in discovery order,
// and spreadBodies is the leaf body of each of them that has one.
var (
	spreadOrder  = [][]int{{0}, {0, 0}, {0, 1}, {1}, {1, 0}, {1, 1}, {2}}
	spreadLabels = []string{"0", "0.0", "0.1", "1", "1.0", "1.1", "2"}
	spreadBodies = []string{"a0", "a1", "c0", "c1", "t"}
)

// mixedKinds returns a message holding one of each thing that counts as a part:
// a leaf, a container that holds a further part, and a part with no header block
// at all. Four parts are discovered in it, in the order 0, 1, 1.0, 2.
func mixedKinds(eol string) originFixture {
	return newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=OUTER" + eol},
		[2]string{"", eol},
		[2]string{"root.body", "--OUTER" + eol},
		[2]string{"0.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"0.body", "zero" + eol},
		[2]string{"", "--OUTER" + eol},
		[2]string{"1.header", "Content-Type: multipart/alternative; boundary=INNER" + eol},
		[2]string{"", eol},
		[2]string{"1.body", "--INNER" + eol},
		[2]string{"1.0.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"1.0.body", "deep" + eol},
		[2]string{"", "--INNER--" + eol},
		[2]string{"", "--OUTER" + eol},
		// Two delimiter lines in a row: a part with no header block, which is
		// still one part.
		[2]string{"2.header", "--OUTER--" + eol},
	)
}

// checkPartCountDiagnostic pins the one limit.part_count diagnostic a refused
// part records: its code, its severity, the part it names, and the exact
// position of the header block that proved the part existed.
func checkPartCountDiagnostic(t *testing.T, session *ParseSession, limit int64, path []int, offset int64) {
	t.Helper()
	checkPartCountDiagnosticIn(t, session.Diagnostics(), limit, path, offset)
}

// checkPartCountDiagnosticIn is checkPartCountDiagnostic holding diags, rather
// than everything the session recorded, to exactly the one entry.
func checkPartCountDiagnosticIn(t *testing.T, diags []Diagnostic, limit int64, path []int, offset int64) {
	t.Helper()

	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one limit.part_count entry", codesOfDiags(diags))
	}
	checkPartCountFacts(t, diags[0], SeverityLimitExceeded, limit, path, offset)
}

// checkPartCountFacts pins one part-count diagnostic, whether it came from the
// session or from the fatal error, which must agree about everything but the
// severity.
func checkPartCountFacts(t *testing.T, d Diagnostic, severity Severity, limit int64, path []int, offset int64) {
	t.Helper()

	if d.Code != CodeLimitPartCount {
		t.Errorf("Code = %q, want %q", d.Code, CodeLimitPartCount)
	}
	if d.Severity != severity {
		t.Errorf("Severity = %v, want %v", d.Severity, severity)
	}
	if !reflect.DeepEqual(d.Path, path) {
		t.Errorf("Path = %v, want %v: the part that was not returned", d.Path, path)
	}
	if want := LocationAt(offset); d.Location != want {
		t.Errorf("Location = %v, want %v: the first byte of that part's header block", d.Location, want)
	}
	if d.Action == "" {
		t.Errorf("Action is empty, want what was done about the defect")
	}
	if !strings.Contains(d.Message, fmt.Sprint(limit)) {
		t.Errorf("Message = %q, want the budget %d in it", d.Message, limit)
	}
	if d.Cause != nil {
		t.Errorf("Cause = %v, want none: nothing failed underneath a budget the parser ran out of", d.Cause)
	}
}

// checkPartCountFatal pins the error a refused part returns: fatal, and carrying
// the same diagnostic the session holds with the severity a fatal error's
// diagnostic always has.
func checkPartCountFatal(t *testing.T, err error, limit int64, path []int, offset int64) {
	t.Helper()

	if !IsFatal(err) {
		t.Fatalf("err = %v, want a FatalError", err)
	}
	var fatal FatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v, want a FatalError", err)
	}
	d, ok := fatal.Diagnostic()
	if !ok {
		t.Fatalf("FatalError carries no diagnostic, want the one that stopped the scope")
	}
	checkPartCountFacts(t, d, SeverityFatal, limit, path, offset)
}

// TestParse_partCount_wideBoundary sweeps the budget across the exact number of
// parts one container holds and pins where the boundary falls: a container of
// exactly the budget is read whole and ends with an ordinary EOF, one part more
// is refused, and the refusal names the part that was not returned and where in
// the input it began.
func TestParse_partCount_wideBoundary(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		t.Run(eolName(eol), func(t *testing.T) {
			for _, n := range []int{2, 3, 8} {
				t.Run(fmt.Sprintf("%d parts", n), func(t *testing.T) {
					f := wideContainer(n, eol)
					offset := f.offset(t, fmt.Sprintf("part%d.header", n-1))

					for _, delta := range []int64{-1, 0, 1} {
						t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
							limit := int64(n) + delta

							session, got, err := parseUnder(t, f.text, partsOnly(limit))
							if err != nil {
								t.Fatalf("Parse() = %v", err)
							}

							if delta >= 0 {
								if got.err != nil {
									t.Fatalf("reading the tree = %v", got.err)
								}
								if want := wideBodies(n); !reflect.DeepEqual(got.bodies, want) {
									t.Errorf("bodies = %v, want %v", got.bodies, want)
								}
								if d := takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true); len(d) != 0 {
									t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
								}
								return
							}

							// The refused part is the last one, so what is
							// recovered is everything before it.
							checkPartCountFatal(t, got.err, limit, []int{n - 1}, offset)
							if want := wideBodies(n - 1); !reflect.DeepEqual(got.bodies, want) {
								t.Errorf("bodies = %v, want %v recovered before the refusal", got.bodies, want)
							}
							checkPartCountDiagnosticIn(t, takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true), limit, []int{n - 1}, offset)
						})
					}
				})
			}
		})
	}
}

// TestParse_partCount_sharedAcrossTheWholeTree sweeps the same budget across the
// total of a tree whose containers are each small.
//
// This is the half a per-container counter would get wrong. The widest container
// here holds three children, so a budget that is reset on entering a container
// would never refuse anything at a budget of six; one budget shared by the whole
// tree refuses the seventh part.
func TestParse_partCount_sharedAcrossTheWholeTree(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		t.Run(eolName(eol), func(t *testing.T) {
			f := spreadTree(eol)
			total := int64(len(spreadOrder))

			for _, delta := range []int64{-1, 0, 1} {
				t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
					limit := total + delta

					session, got, err := parseUnder(t, f.text, partsOnly(limit))
					if err != nil {
						t.Fatalf("Parse() = %v", err)
					}

					if delta >= 0 {
						if got.err != nil {
							t.Fatalf("reading the tree = %v", got.err)
						}
						if !reflect.DeepEqual(got.bodies, spreadBodies) {
							t.Errorf("bodies = %v, want %v", got.bodies, spreadBodies)
						}
						if d := takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true); len(d) != 0 {
							t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
						}
						return
					}

					last := len(spreadOrder) - 1
					offset := f.offset(t, spreadLabels[last]+".header")
					checkPartCountFatal(t, got.err, limit, spreadOrder[last], offset)
					if want := spreadBodies[:len(spreadBodies)-1]; !reflect.DeepEqual(got.bodies, want) {
						t.Errorf("bodies = %v, want %v recovered before the refusal", got.bodies, want)
					}
					checkPartCountDiagnosticIn(t, takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true), limit, spreadOrder[last], offset)
				})
			}
		})
	}
}

// TestParse_partCount_refusalWalksTheWholeOrder pins the count itself, one part
// at a time: for every prefix of the discovery order, a budget of that many
// parts admits exactly those and refuses the next, naming it.
//
// It is what an off-by-one anywhere in the counting fails, and what a counter
// that charged the root would fail at the very first prefix.
func TestParse_partCount_refusalWalksTheWholeOrder(t *testing.T) {
	f := spreadTree("\r\n")

	for limit := 1; limit < len(spreadOrder); limit++ {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			session, got, err := parseUnder(t, f.text, partsOnly(int64(limit)))
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}

			// The part refused is the one after the budget's worth, since the
			// root itself is not counted.
			path := spreadOrder[limit]
			offset := f.offset(t, spreadLabels[limit]+".header")

			checkPartCountFatal(t, got.err, int64(limit), path, offset)
			checkPartCountDiagnostic(t, session, int64(limit), path, offset)
		})
	}
}

// TestParse_partCount_rootIsNotCounted pins the one entity the budget never
// charges. The message is not a part of itself — nothing discovered it — so a
// budget of one admits a message with one part, which a counter that charged the
// root would refuse.
func TestParse_partCount_rootIsNotCounted(t *testing.T) {
	t.Run("a message with no parts", func(t *testing.T) {
		const text = "Content-Type: text/plain\r\n\r\nbody\r\n"

		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: partsOnly(1)})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		if got := readBodyOf(t, e); got != "body" {
			t.Errorf("body = %q, want %q", got, "body")
		}
		if d := session.Diagnostics(); d != nil {
			t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
		}
	})

	t.Run("a message with exactly one part", func(t *testing.T) {
		f := wideContainer(1, "\r\n")

		session, got, err := parseUnder(t, f.text, partsOnly(1))
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		if got.err != nil {
			t.Fatalf("reading the tree = %v, want the one part admitted", got.err)
		}
		if want := []string{"p0"}; !reflect.DeepEqual(got.bodies, want) {
			t.Errorf("bodies = %v, want %v", got.bodies, want)
		}
		if d := session.Diagnostics(); d != nil {
			t.Errorf("Diagnostics() = %v, want none: the root is not a part", codesOfDiags(d))
		}
	})
}

// TestParse_partCount_everyKindCostsOne pins that the count has no exceptions in
// it. A leaf, a container that holds further parts, and a part with no header
// block at all each cost exactly one, so a tree holding one of each spends four
// and is refused at three.
func TestParse_partCount_everyKindCostsOne(t *testing.T) {
	f := mixedKinds("\r\n")
	const total = 4

	t.Run("at the budget", func(t *testing.T) {
		session, got, err := parseUnder(t, f.text, partsOnly(total))
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		if got.err != nil {
			t.Fatalf("reading the tree = %v", got.err)
		}
		// The container contributes no body of its own, and the headerless part
		// has an empty one.
		if want := []string{"zero", "deep", ""}; !reflect.DeepEqual(got.bodies, want) {
			t.Errorf("bodies = %v, want %v", got.bodies, want)
		}
		if d := session.Diagnostics(); d != nil {
			t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
		}
	})

	t.Run("one under it", func(t *testing.T) {
		session, got, err := parseUnder(t, f.text, partsOnly(total-1))
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		offset := f.offset(t, "2.header")
		checkPartCountFatal(t, got.err, total-1, []int{2}, offset)
		checkPartCountDiagnostic(t, session, total-1, []int{2}, offset)
		if want := []string{"zero", "deep"}; !reflect.DeepEqual(got.bodies, want) {
			t.Errorf("bodies = %v, want %v recovered before the refusal", got.bodies, want)
		}
	})
}

// TestParse_partCount_exactlyTheBudgetThenEOF pins that a container holding
// exactly the budget ends the way any container ends. The refusal waits for a
// part to exist, so the end of the input is an ordinary io.EOF and not a
// diagnosed defect.
func TestParse_partCount_exactlyTheBudgetThenEOF(t *testing.T) {
	const n = 4
	f := wideContainer(n, "\r\n")

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: partsOnly(n)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil, want the parts of a multipart message")
	}
	for i := 0; i < n; i++ {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("NextPart() %d = %v", i, err)
		}
		if got, want := readBodyOf(t, p), fmt.Sprintf("p%d", i); got != want {
			t.Errorf("part %d body = %q, want %q", i, got, want)
		}
	}
	if p, err := mr.NextPart(); err != io.EOF {
		t.Errorf("NextPart() after the last part = (%v, %v), want io.EOF", p, err)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
	}
}

// TestParse_partCount_refusedPartIsNotReturned pins what a caller holds after
// the budget runs out: everything up to it, and nothing of the part that
// exceeded it. The parts already returned keep their headers and the bodies
// already read stay exactly what they were.
func TestParse_partCount_refusedPartIsNotReturned(t *testing.T) {
	const n = 4
	f := wideContainer(n, "\r\n")
	limit := int64(n - 1)

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: partsOnly(limit)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil, want the parts of a multipart message")
	}

	var kept []*Entity
	var bodies []string
	for i := int64(0); i < limit; i++ {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("NextPart() %d = %v", i, err)
		}
		bodies = append(bodies, readBodyOf(t, p))
		kept = append(kept, p)
	}

	part, want := mr.NextPart()
	if part != nil {
		t.Errorf("the over-budget part was returned as %v, want no part", part)
	}
	checkPartCountFatal(t, want, limit, []int{n - 1}, f.offset(t, fmt.Sprintf("part%d.header", n-1)))

	for i := 0; i < 3; i++ {
		p, err := mr.NextPart()
		if p != nil {
			t.Errorf("call %d after the refusal returned %v, want no part", i, p)
		}
		if err != want {
			t.Errorf("call %d after the refusal = %v, want the identical error %v", i, err, want)
		}
	}

	if got := wideBodies(int(limit)); !reflect.DeepEqual(bodies, got) {
		t.Errorf("bodies = %v, want %v", bodies, got)
	}
	for i, p := range kept {
		if got := p.Header.Get("Content-Type"); got != "text/plain" {
			t.Errorf("part %d lost its header after the refusal: Content-Type = %q", i, got)
		}
	}
	checkPartCountDiagnostic(t, session, limit, []int{n - 1}, f.offset(t, fmt.Sprintf("part%d.header", n-1)))
}

// TestParse_partCount_failureIsSessionWide pins the scope of a part-count
// refusal, which is the whole difference between this budget and the depth one.
// What ran out belongs to the parse, so an iterator built earlier for another
// container reports the identical error, and a traversal begun afterwards
// visits nothing at all.
func TestParse_partCount_failureIsSessionWide(t *testing.T) {
	f := spreadTree("\r\n")
	const limit = 2

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: partsOnly(limit)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	outer := e.MultipartReader()
	if outer == nil {
		t.Fatal("MultipartReader() = nil, want the parts of a multipart message")
	}

	container, err := outer.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v, want the first part", err)
	}
	inner := container.MultipartReader()
	if inner == nil {
		t.Fatal("the first part is not multipart")
	}
	first, err := inner.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v, want the second part", err)
	}

	// The third part discovered is one past the budget.
	part, want := inner.NextPart()
	if part != nil {
		t.Errorf("the over-budget part was returned as %v, want no part", part)
	}
	checkPartCountFatal(t, want, limit, []int{0, 1}, f.offset(t, "0.1.header"))

	if _, err := inner.NextPart(); err != want {
		t.Errorf("the same iterator asked again = %v, want the identical error", err)
	}

	// A different iterator, built before the budget ran out, reports the same
	// failure rather than its own next sibling.
	p, err := outer.NextPart()
	if p != nil {
		t.Errorf("the containing iterator returned %v, want no part", p)
	}
	if err != want {
		t.Errorf("the containing iterator = %v, want the identical error %v", err, want)
	}

	// And a traversal begun after the stop visits nothing, not even the root.
	var visited [][]int
	walkErr := e.Walk(func(path []int, _ *Entity, err error) error {
		visited = append(visited, path)
		return err
	})
	if walkErr != want {
		t.Errorf("Walk() = %v, want the identical error %v", walkErr, want)
	}
	if visited != nil {
		t.Errorf("Walk visited %v, want nothing: the parse had already stopped", visited)
	}

	// Everything handed over before the failure is still the caller's.
	if got := container.Header.Get("Content-Type"); !strings.HasPrefix(got, "multipart/alternative") {
		t.Errorf("the container lost its header: Content-Type = %q", got)
	}
	if got := first.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("the part before the failure lost its header: Content-Type = %q", got)
	}

	checkPartCountDiagnostic(t, session, limit, []int{0, 1}, f.offset(t, "0.1.header"))
}

// TestParse_partCount_walkReportsTheFailure pins that a traversal already under
// way ends the same way an iterator does, having visited everything discovered
// before the refusal.
func TestParse_partCount_walkReportsTheFailure(t *testing.T) {
	f := spreadTree("\r\n")
	const limit = 2

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: partsOnly(limit)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	var visited [][]int
	walkErr := e.Walk(func(path []int, _ *Entity, err error) error {
		if err != nil {
			return err
		}
		visited = append(visited, path)
		return nil
	})

	offset := f.offset(t, "0.1.header")
	checkPartCountFatal(t, walkErr, limit, []int{0, 1}, offset)
	checkPartCountDiagnostic(t, session, limit, []int{0, 1}, offset)

	want := [][]int{nil, {0}, {0, 0}}
	if !reflect.DeepEqual(visited, want) {
		t.Errorf("Walk visited %v, want %v: the root and the parts within the budget", visited, want)
	}
}

// TestParse_partCount_chunkInvariance pins that the budget decides the same way
// however the input arrives.
func TestParse_partCount_chunkInvariance(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		f := spreadTree(eol)
		total := int64(len(spreadOrder))
		last := len(spreadOrder) - 1
		offset := f.offset(t, spreadLabels[last]+".header")

		for name, src := range chunkedSources(f.text) {
			for _, delta := range []int64{-1, 0} {
				limit := total + delta
				label := fmt.Sprintf("%s/%s at limit%+d", eolName(eol), name, delta)

				session, e, err := Parse(src(), &ParseOptions{Limits: partsOnly(limit)})
				if err != nil {
					t.Fatalf("%s: Parse() = %v", label, err)
				}
				got := readTree(e)

				if delta == 0 {
					if got.err != nil {
						t.Errorf("%s: reading the tree = %v", label, got.err)
					}
					if !reflect.DeepEqual(got.bodies, spreadBodies) {
						t.Errorf("%s: bodies = %v, want %v", label, got.bodies, spreadBodies)
					}
					if d := takeLineNotes(t, label, eol, f.text, session.Diagnostics(), true); len(d) != 0 {
						t.Errorf("%s: Diagnostics() = %v, want none", label, codesOfDiags(d))
					}
					continue
				}

				if !IsFatal(got.err) {
					t.Errorf("%s: reading the tree = %v, want the part-count refusal", label, got.err)
					continue
				}
				if want := spreadBodies[:len(spreadBodies)-1]; !reflect.DeepEqual(got.bodies, want) {
					t.Errorf("%s: bodies = %v, want %v", label, got.bodies, want)
				}
				checkPartCountDiagnosticIn(t, takeLineNotes(t, label, eol, f.text, session.Diagnostics(), true), limit, spreadOrder[last], offset)
			}
		}
	}
}

// TestParse_partCount_sinkReceivesOnce pins that a sink is told about the
// refusal exactly once for the whole parse, and that a session which retains
// nothing still delivers it.
func TestParse_partCount_sinkReceivesOnce(t *testing.T) {
	f := spreadTree("\r\n")
	const limit = 2

	var seen []Diagnostic
	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: partsOnly(limit),
		DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			seen = append(seen, d)
			return nil
		}),
		DiscardDeliveredDiagnostics: true,
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	readTree(e)
	// Asking again after the stop must not produce a second delivery.
	readTree(e)

	if len(seen) != 1 {
		t.Fatalf("the sink received %v, want exactly one diagnostic", seen)
	}
	checkPartCountFacts(t, seen[0], SeverityLimitExceeded, limit, []int{0, 1}, f.offset(t, "0.1.header"))

	if got := session.Diagnostics(); got != nil {
		t.Errorf("Diagnostics() = %v, want none retained", got)
	}
}

// TestParse_partCount_sinkStopWins pins what a caller is told when its own sink
// refuses to continue while it is being told about the refused part: the stop it
// asked for, rather than the budget the parser ran out of. Both belong to the
// whole session, and the caller's own decision is the one reported.
func TestParse_partCount_sinkStopWins(t *testing.T) {
	f := spreadTree("\r\n")
	stop := errors.New("stop here")

	var seen []Code
	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: partsOnly(2),
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
	var fatal FatalError
	if errors.As(got.err, &fatal) {
		if _, ok := fatal.Diagnostic(); ok {
			t.Errorf("the sink's stop carries a diagnostic, want none: it is not a defect of the message")
		}
	}
	if !reflect.DeepEqual(seen, []Code{CodeLimitPartCount}) {
		t.Errorf("the sink received %v, want exactly the part-count diagnostic", seen)
	}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitPartCount}) {
		t.Errorf("Diagnostics() = %v, want the one diagnostic that was delivered", got)
	}
}

// TestParse_partCount_sinkStoppedEarlierRecordsNothing pins that a parse already
// stopped never reaches the budget: the iterator observes the stop before it asks
// for a part, so no part is refused and nothing is recorded for one.
func TestParse_partCount_sinkStoppedEarlierRecordsNothing(t *testing.T) {
	f := spreadTree("\r\n")
	// The first leaf declares an encoding nothing decodes, so a diagnostic is
	// delivered — and the sink stops the parse — before the budget runs out.
	text := strings.Replace(f.text,
		"--A\r\nContent-Type: text/plain\r\n\r\na0\r\n",
		"--A\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: nonsense\r\n\r\na0\r\n", 1)
	if text == f.text {
		t.Fatal("fixture did not change; the test would not exercise an earlier diagnostic")
	}

	stop := errors.New("stop here")
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{
		Limits: partsOnly(2),
		DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, _ Diagnostic) error {
			return stop
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	got := readTree(e)
	if !errors.Is(got.err, stop) {
		t.Fatalf("reading the tree = %v, want the sink's stop", got.err)
	}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeCTEUnknown}) {
		t.Errorf("Diagnostics() = %v, want only the diagnostic that stopped the parse", codes)
	}
}

// TestParse_partCount_disabledAndMaxInt64 pins the two ends of the range.
// LimitDisabled is off completely, and math.MaxInt64 is an ordinary finite
// budget rather than a synonym for it — neither refuses a message no budget
// should.
func TestParse_partCount_disabledAndMaxInt64(t *testing.T) {
	f := spreadTree("\r\n")

	for _, tc := range []struct {
		name  string
		limit int64
	}{
		{"disabled", LimitDisabled},
		{"max int64", math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, got, err := parseUnder(t, f.text, partsOnly(tc.limit))
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if got.err != nil {
				t.Fatalf("reading the tree = %v", got.err)
			}
			if !reflect.DeepEqual(got.bodies, spreadBodies) {
				t.Errorf("bodies = %v, want %v", got.bodies, spreadBodies)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
			}
		})
	}
}

// TestParse_partCount_profileIndependent pins that this budget is not a matter
// of interpretation: every profile and every compatibility policy enforce it
// identically.
func TestParse_partCount_profileIndependent(t *testing.T) {
	f := spreadTree("\r\n")
	last := len(spreadOrder) - 1
	offset := f.offset(t, spreadLabels[last]+".header")
	total := int64(len(spreadOrder))

	for _, profile := range []Profile{ProfileStrict, ProfileTolerant, ProfileForensic} {
		for _, compat := range []Compatibility{
			CompatibilityConservative, CompatibilityAngus,
			CompatibilityMimeKit, CompatibilityPythonEmail,
		} {
			opts := &ParseOptions{
				Profile:       profile,
				Compatibility: compat,
				Limits:        partsOnly(total - 1),
			}
			session, e, err := Parse(strings.NewReader(f.text), opts)
			if err != nil {
				t.Fatalf("%v/%v: Parse() = %v", profile, compat, err)
			}
			got := readTree(e)
			checkPartCountFatal(t, got.err, total-1, spreadOrder[last], offset)
			checkPartCountDiagnostic(t, session, total-1, spreadOrder[last], offset)

			opts.Limits = partsOnly(total)
			session, e, err = Parse(strings.NewReader(f.text), opts)
			if err != nil {
				t.Fatalf("%v/%v: Parse() = %v", profile, compat, err)
			}
			if got := readTree(e); got.err != nil {
				t.Errorf("%v/%v: reading at the budget = %v", profile, compat, got.err)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("%v/%v: Diagnostics() = %v, want none at the budget", profile, compat, codesOfDiags(d))
			}
		}
	}
}

// TestParse_partCount_legacyPathsUnbounded pins the compatibility half of this
// item. Read and ReadWithOptions resolve no configuration and allocate no
// session, so there is no counter at all on those paths: a message with more
// parts than the documented default is read exactly as it always was, while the
// same message under Parse is held to the budget.
func TestParse_partCount_legacyPathsUnbounded(t *testing.T) {
	// A few parts past the documented default, so that a budget leaking into
	// these paths would be visible rather than merely possible.
	n := int(DefaultLimits().MaxPartCount) + 5
	text := wideText(n, "\r\n")

	for _, tc := range []struct {
		name string
		read func(io.Reader) (*Entity, error)
	}{
		{"Read", Read},
		{"ReadWithOptions", func(r io.Reader) (*Entity, error) { return ReadWithOptions(r, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := tc.read(strings.NewReader(text))
			if err != nil {
				t.Fatalf("%s() = %v", tc.name, err)
			}
			got := readTree(e)
			if got.err != nil {
				t.Fatalf("reading the tree = %v, want no budget on these paths", got.err)
			}
			if len(got.bodies) != n {
				t.Errorf("read %d parts, want %d", len(got.bodies), n)
			}
		})
	}

	t.Run("Parse holds the same message to the budget", func(t *testing.T) {
		_, got, err := parseUnder(t, text, DefaultLimits())
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		if !IsFatal(got.err) {
			t.Fatalf("reading the tree = %v, want the default budget to refuse", got.err)
		}
		if want := int(DefaultLimits().MaxPartCount); len(got.bodies) != want {
			t.Errorf("read %d parts before the refusal, want %d", len(got.bodies), want)
		}
	})

	t.Run("NewMultipart", func(t *testing.T) {
		var lh Header
		lh.Set("Content-Type", "text/plain")

		parts := make([]*Entity, 0, 32)
		for i := 0; i < 32; i++ {
			leaf, err := New(lh, strings.NewReader("leaf"))
			if err != nil {
				t.Fatalf("New() = %v", err)
			}
			parts = append(parts, leaf)
		}

		var mh Header
		mh.Set("Content-Type", "multipart/mixed")
		e, err := NewMultipart(mh, parts)
		if err != nil {
			t.Fatalf("NewMultipart() = %v", err)
		}

		var visited int
		if err := e.Walk(func(_ []int, _ *Entity, err error) error {
			if err != nil {
				return err
			}
			visited++
			return nil
		}); err != nil {
			t.Fatalf("Walk() = %v", err)
		}
		if visited != len(parts)+1 {
			t.Errorf("Walk visited %d entities, want %d", visited, len(parts)+1)
		}
	})
}

// TestParse_partCount_boundedWorkOnWideInput pins that the budget bounds the
// work rather than describing it afterwards. A multipart body that goes on for
// ever is stopped a part past the budget, so what the parse reads is a function
// of the budget and not of how much input there is.
func TestParse_partCount_boundedWorkOnWideInput(t *testing.T) {
	const limit = 8

	src := &endlessReader{
		prefix: "Content-Type: multipart/mixed; boundary=B\r\n\r\n",
		line:   "--B\r\nContent-Type: text/plain\r\n\r\nx\r\n",
		cap:    endlessCap,
	}

	session, e, err := Parse(src, &ParseOptions{Limits: partsOnly(limit)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	got := readTree(e)
	if !IsFatal(got.err) {
		t.Fatalf("reading the tree = %v, want the part-count refusal", got.err)
	}
	if len(got.bodies) != limit {
		t.Errorf("read %d parts, want %d", len(got.bodies), limit)
	}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitPartCount}) {
		t.Errorf("Diagnostics() = %v, want one part-count entry", codes)
	}
	if src.n >= endlessBudgetHeadroom {
		t.Errorf("the parse read %d bytes of an endless body, want it stopped within %d",
			src.n, endlessBudgetHeadroom)
	}
}

// TestParseSession_chargePart_concurrent pins the counter itself under
// concurrency, which is the one property no message can demonstrate: a parse
// consumes one multipart at a time, but the counter is session state and the
// session is documented as safe for concurrent use.
//
// Every goroutine charges the same budget and reads the session's failure while
// it does. Exactly the budget's worth of charges may be accepted, exactly one
// caller may be told it observed the overflow — that one owes the session the
// diagnostic, and two of them would record it twice — and every refusal must be
// the identical error the session reports afterwards.
func TestParseSession_chargePart_concurrent(t *testing.T) {
	const (
		limit      = 100
		goroutines = 8
		each       = 50
	)

	limits := DefaultLimits()
	limits.MaxPartCount = limit
	session := newParseSessionWithConfig(resolvedConfig{
		profile: ProfileTolerant,
		compat:  CompatibilityConservative,
		limits:  limits,
	}, diagnosticOptions{})

	d := Diagnostic{Code: CodeLimitPartCount, Severity: SeverityLimitExceeded}

	var (
		mu       sync.Mutex
		accepted int
		claims   int
		refusals []error
	)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				fatal, claimed := session.chargePart()
				if claimed {
					// The one caller told to finish the transition owes the
					// session the diagnostic; every other caller is waiting for
					// it, so this is not optional.
					fatal = session.resolvePart(d)
				}

				// Reading the session's own view concurrently is what a scope
				// does at each of its progress boundaries.
				_ = session.fatalErr()

				mu.Lock()
				if fatal == nil {
					accepted++
				} else {
					refusals = append(refusals, fatal)
				}
				if claimed {
					claims++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted != limit {
		t.Errorf("%d charges were accepted, want exactly the budget %d", accepted, limit)
	}
	if claims != 1 {
		t.Errorf("%d callers were told they observed the overflow, want exactly 1", claims)
	}
	if want := goroutines*each - limit; len(refusals) != want {
		t.Errorf("%d charges were refused, want %d", len(refusals), want)
	}

	stuck := session.fatalErr()
	if stuck == nil {
		t.Fatal("fatalErr() = nil after the budget ran out, want the session's sticky failure")
	}
	for i, err := range refusals {
		if err != error(stuck) {
			t.Fatalf("refusal %d = %v, want the identical error %v", i, err, stuck)
		}
	}
	if got := session.fatalErr(); got != stuck {
		t.Errorf("fatalErr() = %v on a second call, want the identical error", got)
	}

	// A charge after the overflow keeps reporting the same thing and never
	// claims the observation again.
	if fatal, claimed := session.chargePart(); fatal != error(stuck) || claimed {
		t.Errorf("chargePart() after the overflow = (%v, %v), want (%v, false)", fatal, claimed, stuck)
	}

	// And exactly one diagnostic reached the session, however many goroutines
	// raced into the same overflow.
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitPartCount}) {
		t.Errorf("Diagnostics() = %v, want exactly one part-count entry", got)
	}
}
