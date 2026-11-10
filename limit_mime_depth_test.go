package message

import (
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Limits.MaxMIMEDepth: how far a parse may descend through nested multipart
// containers.
//
// Every fixture here is generated from the depth it means to exercise, so the
// expected path and the expected offset of a refusal are derived from the
// message rather than counted out by hand. The budget is swept across the exact
// depth of the message at limit-1, limit and limit+1, at every line ending, and
// under every way an input can be delivered.
//
// The tests are as much about what the budget must not do. A multipart that
// exists is not a defect; a container that sits exactly at the budget and turns
// out to be empty must end with an ordinary EOF; and a branch that ran out of
// room must not take the rest of the message with it.

// The ceilings the generated inputs here run under.
//
// A depth test proves what it proves at a depth a reader can hold in their
// head, and the generator is bounded so that a regression — a budget that stops
// refusing, or a traversal that starts recursing — fails a test rather than
// exhausting the machine running it. deepChainLevels is what the iterative
// traversal is proved against, and deepChainCap is the ceiling the generator
// refuses to build past.
const (
	deepChainLevels = 200
	deepChainCap    = 1000
)

// depthOnly returns every budget disabled except the MIME depth budget, set to
// n.
func depthOnly(n int64) Limits {
	l := allDisabled()
	l.MaxMIMEDepth = n
	return l
}

// chainPath returns the MIME path of the entity at the given depth of a chain
// fixture, which descends through part zero of every container: nil for the
// root, and that many zeros below it.
func chainPath(depth int) []int {
	if depth <= 0 {
		return nil
	}
	return make([]int, depth)
}

// depthChain returns a message that nests one multipart inside the first part
// of another, with a single text leaf at the bottom, so that the deepest entity
// of the message sits at exactly the given depth.
//
// Each container's header block is labelled "depth<n>.header", so a test names
// the position it expects a refusal at by the depth it is talking about.
func depthChain(levels int, eol string) originFixture {
	if levels < 1 || levels > deepChainCap {
		panic(fmt.Sprintf("depthChain: %d levels is outside [1, %d]", levels, deepChainCap))
	}

	segs := [][2]string{
		{"depth0.header", "Content-Type: multipart/mixed; boundary=B0" + eol},
		{"", eol},
	}
	for d := 1; d < levels; d++ {
		segs = append(segs,
			[2]string{"", fmt.Sprintf("--B%d", d-1) + eol},
			[2]string{fmt.Sprintf("depth%d.header", d), fmt.Sprintf("Content-Type: multipart/mixed; boundary=B%d", d) + eol},
			[2]string{"", eol},
		)
	}
	segs = append(segs,
		[2]string{"", fmt.Sprintf("--B%d", levels-1) + eol},
		[2]string{fmt.Sprintf("depth%d.header", levels), "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"", "leaf" + eol},
	)
	for d := levels - 1; d >= 0; d-- {
		segs = append(segs, [2]string{"", fmt.Sprintf("--B%d--", d) + eol})
	}
	return newOriginFixture(segs...)
}

// emptyAtDepth returns the same chain of containers with no leaf at all: the
// container at the given depth declares a boundary and then closes it
// immediately, so the deepest entity of the message is that empty container.
func emptyAtDepth(depth int, eol string) originFixture {
	if depth < 1 || depth > deepChainCap {
		panic(fmt.Sprintf("emptyAtDepth: %d is outside [1, %d]", depth, deepChainCap))
	}

	segs := [][2]string{
		{"depth0.header", "Content-Type: multipart/mixed; boundary=B0" + eol},
		{"", eol},
	}
	for d := 1; d <= depth; d++ {
		segs = append(segs,
			[2]string{"", fmt.Sprintf("--B%d", d-1) + eol},
			[2]string{fmt.Sprintf("depth%d.header", d), fmt.Sprintf("Content-Type: multipart/mixed; boundary=B%d", d) + eol},
			[2]string{"", eol},
		)
	}
	for d := depth; d >= 0; d-- {
		segs = append(segs, [2]string{"", fmt.Sprintf("--B%d--", d) + eol})
	}
	return newOriginFixture(segs...)
}

// checkDepthDiagnostic pins the one limit.mime_depth diagnostic a refused child
// records: its code, its severity, the child it names, and the exact position of
// the header block that proved the child existed.
func checkDepthDiagnostic(t *testing.T, session *ParseSession, limit int64, path []int, offset int64) {
	t.Helper()
	checkDepthDiagnosticIn(t, session.Diagnostics(), limit, path, offset)
}

// checkDepthDiagnosticIn is checkDepthDiagnostic holding diags, rather than
// everything the session recorded, to exactly the one entry.
func checkDepthDiagnosticIn(t *testing.T, diags []Diagnostic, limit int64, path []int, offset int64) {
	t.Helper()

	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one limit.mime_depth entry", codesOfDiags(diags))
	}
	checkDepthFacts(t, diags[0], SeverityLimitExceeded, limit, path, offset)
}

// checkDepthFacts pins one depth diagnostic, whether it came from the session or
// from the fatal error, which must agree about everything but the severity.
func checkDepthFacts(t *testing.T, d Diagnostic, severity Severity, limit int64, path []int, offset int64) {
	t.Helper()

	if d.Code != CodeLimitMIMEDepth {
		t.Errorf("Code = %q, want %q", d.Code, CodeLimitMIMEDepth)
	}
	if d.Severity != severity {
		t.Errorf("Severity = %v, want %v", d.Severity, severity)
	}
	if !reflect.DeepEqual(d.Path, path) {
		t.Errorf("Path = %v, want %v: the child that was not returned", d.Path, path)
	}
	if want := LocationAt(offset); d.Location != want {
		t.Errorf("Location = %v, want %v: the first byte of that child's header block", d.Location, want)
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

// checkDepthFatal pins the error a refused child returns: fatal, and carrying
// the same diagnostic the session holds with the severity a fatal error's
// diagnostic always has.
func checkDepthFatal(t *testing.T, err error, limit int64, path []int, offset int64) {
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
	checkDepthFacts(t, d, SeverityFatal, limit, path, offset)
}

// TestParse_mimeDepth_rootIsDepthZero pins the origin of the count. The message
// itself is at depth zero whatever it is, so a budget of one admits every direct
// child of it — and refuses the first grandchild, which is what says the step
// from a container to its child is exactly one.
func TestParse_mimeDepth_rootIsDepthZero(t *testing.T) {
	t.Run("non-multipart root", func(t *testing.T) {
		const text = "Content-Type: text/plain\r\n\r\nbody\r\n"

		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: depthOnly(1)})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		if got := readBodyOf(t, e); got != "body" {
			t.Errorf("body = %q, want %q", got, "body")
		}
		if d := session.Diagnostics(); d != nil {
			t.Errorf("Diagnostics() = %v, want none: a message with no parts spends nothing", codesOfDiags(d))
		}
	})

	t.Run("multipart root at the budget", func(t *testing.T) {
		f := depthChain(1, "\r\n")

		session, got, err := parseUnder(t, f.text, depthOnly(1))
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		if got.err != nil {
			t.Fatalf("reading the tree = %v, want the child at depth one admitted", got.err)
		}
		if want := []string{"leaf"}; !reflect.DeepEqual(got.bodies, want) {
			t.Errorf("bodies = %v, want %v", got.bodies, want)
		}
		if d := session.Diagnostics(); d != nil {
			t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
		}
	})

	t.Run("grandchild past the budget", func(t *testing.T) {
		f := depthChain(2, "\r\n")

		session, got, err := parseUnder(t, f.text, depthOnly(1))
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		checkDepthFatal(t, got.err, 1, chainPath(2), f.offset(t, "depth2.header"))
		checkDepthDiagnostic(t, session, 1, chainPath(2), f.offset(t, "depth2.header"))
	})
}

// TestParse_mimeDepth_boundary sweeps the budget across the exact depth of the
// message, at several depths and with each line ending, and pins where the
// boundary falls: a tree exactly as deep as the budget is read whole, one level
// deeper is refused, and the refusal names the first child it could not admit
// and where in the input that child began.
func TestParse_mimeDepth_boundary(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		t.Run(eolName(eol), func(t *testing.T) {
			// Two is the shallowest depth this can sweep: one below it would be a
			// budget of zero, which selects the default rather than a budget.
			for _, levels := range []int{2, 3, 5} {
				t.Run(fmt.Sprintf("%d levels", levels), func(t *testing.T) {
					f := depthChain(levels, eol)
					offset := f.offset(t, fmt.Sprintf("depth%d.header", levels))

					for _, delta := range []int64{-1, 0, 1} {
						t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
							limit := int64(levels) + delta

							session, got, err := parseUnder(t, f.text, depthOnly(limit))
							if err != nil {
								t.Fatalf("Parse() = %v", err)
							}

							if delta >= 0 {
								if got.err != nil {
									t.Fatalf("reading the tree = %v", got.err)
								}
								if want := []string{"leaf"}; !reflect.DeepEqual(got.bodies, want) {
									t.Errorf("bodies = %v, want %v", got.bodies, want)
								}
								if d := takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true); len(d) != 0 {
									t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
								}
								return
							}

							checkDepthFatal(t, got.err, limit, chainPath(levels), offset)
							if got.bodies != nil {
								t.Errorf("bodies = %v, want none recovered: the only leaf is the refused one", got.bodies)
							}
							checkDepthDiagnosticIn(t, takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true), limit, chainPath(levels), offset)
						})
					}
				})
			}
		})
	}
}

// TestParse_mimeDepth_chunkInvariance pins that the budget decides the same way
// however the input arrives — one byte at a time, in chunks that fall inside and
// across lines, in deterministic random pieces, and from a reader that reports
// the end of the input together with its last bytes.
func TestParse_mimeDepth_chunkInvariance(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		const levels = 3
		f := depthChain(levels, eol)
		offset := f.offset(t, fmt.Sprintf("depth%d.header", levels))

		for name, src := range chunkedSources(f.text) {
			for _, delta := range []int64{-1, 0} {
				limit := int64(levels) + delta
				label := fmt.Sprintf("%s/%s at limit%+d", eolName(eol), name, delta)

				session, e, err := Parse(src(), &ParseOptions{Limits: depthOnly(limit)})
				if err != nil {
					t.Fatalf("%s: Parse() = %v", label, err)
				}
				got := readTree(e)

				if delta == 0 {
					if got.err != nil {
						t.Errorf("%s: reading the tree = %v", label, got.err)
					}
					if want := []string{"leaf"}; !reflect.DeepEqual(got.bodies, want) {
						t.Errorf("%s: bodies = %v, want %v", label, got.bodies, want)
					}
					if d := takeLineNotes(t, label, eol, f.text, session.Diagnostics(), true); len(d) != 0 {
						t.Errorf("%s: Diagnostics() = %v, want none", label, codesOfDiags(d))
					}
					continue
				}

				if !IsFatal(got.err) {
					t.Errorf("%s: reading the tree = %v, want the depth refusal", label, got.err)
					continue
				}
				if got.bodies != nil {
					t.Errorf("%s: bodies = %v, want none recovered", label, got.bodies)
				}
				checkDepthDiagnosticIn(t, takeLineNotes(t, label, eol, f.text, session.Diagnostics(), true), limit, chainPath(levels), offset)
			}
		}
	}
}

// TestParse_mimeDepth_emptyContainerAtTheBudgetIsEOF pins the reason the check
// waits for a child to exist. A container sitting exactly at the budget may hold
// nothing at all, and a budget that refused it for what it might have contained
// would turn an ordinary end of input into a defect. Iterating it is an ordinary
// EOF and records nothing.
func TestParse_mimeDepth_emptyContainerAtTheBudgetIsEOF(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		for _, depth := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("%s/depth %d", eolName(eol), depth), func(t *testing.T) {
				f := emptyAtDepth(depth, eol)

				session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
					Limits: depthOnly(int64(depth)),
				})
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}

				// Walk the containers by hand, so that the EOF of the deepest one
				// is observed as itself rather than through a helper.
				entity := e
				for d := 0; d < depth; d++ {
					mr := entity.MultipartReader()
					if mr == nil {
						t.Fatalf("depth %d: MultipartReader() = nil, want a container", d)
					}
					part, err := mr.NextPart()
					if err != nil {
						t.Fatalf("depth %d: NextPart() = %v, want the container below it", d, err)
					}
					entity = part
				}

				mr := entity.MultipartReader()
				if mr == nil {
					t.Fatalf("the container at depth %d is not multipart", depth)
				}
				if _, err := mr.NextPart(); err != io.EOF {
					t.Errorf("NextPart() on the empty container at the budget = %v, want io.EOF", err)
				}
				// The empty container is reported as such (ROB-060-009), and
				// nothing is refused.
				if d := takeLineNotes(t, "", eol, f.text, session.Diagnostics(), true); !reflect.DeepEqual(codesOfDiags(d), []Code{CodeMultipartEmpty}) {
					t.Errorf("Diagnostics() = %v, want multipart.empty alone: the container had no child to refuse", codesOfDiags(d))
				}
			})
		}
	}
}

// TestParse_mimeDepth_failureIsLocal pins the scope of a depth refusal, which is
// the whole difference between this budget and the part count. The branch that
// ran out of room ends; the parent holding it, the sibling returned before it
// and the sibling after it are untouched, and the iterator that failed reports
// the identical error however often it is asked without recording the defect
// twice.
func TestParse_mimeDepth_failureIsLocal(t *testing.T) {
	f := newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=OUTER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--OUTER\r\n"},
		[2]string{"0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "zero\r\n"},
		[2]string{"", "--OUTER\r\n"},
		[2]string{"1.header", "Content-Type: multipart/alternative; boundary=INNER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"1.body", "--INNER\r\n"},
		[2]string{"1.0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"1.0.body", "deep\r\n"},
		[2]string{"", "--INNER--\r\n"},
		[2]string{"", "--OUTER\r\n"},
		[2]string{"2.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"2.body", "two\r\n"},
		[2]string{"", "--OUTER--\r\n"},
	)

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: depthOnly(1)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	outer := e.MultipartReader()
	if outer == nil {
		t.Fatal("MultipartReader() = nil, want the parts of a multipart message")
	}

	zero, err := outer.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v", err)
	}
	if got := readBodyOf(t, zero); got != "zero" {
		t.Errorf("first part = %q, want %q", got, "zero")
	}

	container, err := outer.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v, want the container at depth one", err)
	}

	inner := container.MultipartReader()
	if inner == nil {
		t.Fatal("the second part is not multipart")
	}

	part, want := inner.NextPart()
	if part != nil {
		t.Errorf("the over-deep child was returned as %v, want no part", part)
	}
	checkDepthFatal(t, want, 1, []int{1, 0}, f.offset(t, "1.0.header"))

	for i := 0; i < 3; i++ {
		p, err := inner.NextPart()
		if p != nil {
			t.Errorf("call %d after the refusal returned %v, want no part", i, p)
		}
		if err != want {
			t.Errorf("call %d after the refusal = %v, want the identical error %v", i, err, want)
		}
	}

	// The parent of the refused child is still the entity it was, and so is the
	// sibling handed over before the failure.
	if got := container.Header.Get("Content-Type"); !strings.HasPrefix(got, "multipart/alternative") {
		t.Errorf("the container lost its header: Content-Type = %q", got)
	}
	if got := zero.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("the sibling before the failure lost its header: Content-Type = %q", got)
	}

	// And the branch that ran out of room did not take the rest of the message
	// with it: the container's own sibling is still reachable.
	two, err := outer.NextPart()
	if err != nil {
		t.Fatalf("NextPart() after the local failure = %v, want the next sibling", err)
	}
	if got := readBodyOf(t, two); got != "two" {
		t.Errorf("third part = %q, want %q", got, "two")
	}
	if _, err := outer.NextPart(); err != io.EOF {
		t.Errorf("NextPart() at the end = %v, want io.EOF", err)
	}

	checkDepthDiagnostic(t, session, 1, []int{1, 0}, f.offset(t, "1.0.header"))
}

// TestParse_mimeDepth_walkReportsTheFailure pins that a traversal ends the same
// way an iterator does, having already visited everything above the refusal.
func TestParse_mimeDepth_walkReportsTheFailure(t *testing.T) {
	const levels = 3
	f := depthChain(levels, "\r\n")
	offset := f.offset(t, fmt.Sprintf("depth%d.header", levels))

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: depthOnly(levels - 1),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	var visited [][]int
	walkErr := e.Walk(func(path []int, part *Entity, err error) error {
		if err != nil {
			return err
		}
		visited = append(visited, path)
		return nil
	})

	checkDepthFatal(t, walkErr, levels-1, chainPath(levels), offset)
	checkDepthDiagnostic(t, session, levels-1, chainPath(levels), offset)

	want := [][]int{nil, {0}, {0, 0}}
	if !reflect.DeepEqual(visited, want) {
		t.Errorf("Walk visited %v, want %v: everything above the refusal", visited, want)
	}
}

// TestParse_mimeDepth_sinkReceivesOnce pins that a sink is told about the
// refusal exactly once, and that a session which retains nothing still delivers
// it.
func TestParse_mimeDepth_sinkReceivesOnce(t *testing.T) {
	const levels = 3
	f := depthChain(levels, "\r\n")

	var seen []Diagnostic
	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: depthOnly(levels - 1),
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

	if len(seen) != 1 {
		t.Fatalf("the sink received %v, want exactly one diagnostic", seen)
	}
	checkDepthFacts(t, seen[0], SeverityLimitExceeded, levels-1,
		chainPath(levels), f.offset(t, fmt.Sprintf("depth%d.header", levels)))

	if got := session.Diagnostics(); got != nil {
		t.Errorf("Diagnostics() = %v, want none retained", got)
	}
}

// TestParse_mimeDepth_sinkStopWins pins what a caller is told when its own sink
// refuses to continue while it is being told about the refused child: the stop
// it asked for, which belongs to the whole parse, rather than this branch's own
// failure. The diagnostic was delivered either way.
func TestParse_mimeDepth_sinkStopWins(t *testing.T) {
	const levels = 3
	f := depthChain(levels, "\r\n")
	stop := errors.New("stop here")

	var seen []Code
	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: depthOnly(levels - 1),
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
	if !reflect.DeepEqual(seen, []Code{CodeLimitMIMEDepth}) {
		t.Errorf("the sink received %v, want exactly the depth diagnostic", seen)
	}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitMIMEDepth}) {
		t.Errorf("Diagnostics() = %v, want the one diagnostic that was delivered", got)
	}
}

// TestParse_mimeDepth_disabledAndMaxInt64 pins the two ends of the range.
// LimitDisabled is off completely, and math.MaxInt64 is an ordinary finite
// budget rather than a synonym for it — neither refuses a tree no real message
// would reach.
func TestParse_mimeDepth_disabledAndMaxInt64(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
	}{
		{"disabled", LimitDisabled},
		{"max int64", math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := depthChain(deepChainLevels, "\r\n")

			session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
				Limits: depthOnly(tc.limit),
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}

			var deepest int
			walkErr := e.Walk(func(path []int, _ *Entity, err error) error {
				if err != nil {
					return err
				}
				if len(path) > deepest {
					deepest = len(path)
				}
				return nil
			})
			if walkErr != nil {
				t.Fatalf("Walk() = %v", walkErr)
			}
			if deepest != deepChainLevels {
				t.Errorf("deepest entity is at depth %d, want %d", deepest, deepChainLevels)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
			}
		})
	}
}

// TestParse_mimeDepth_walkStaysIterative pins that reaching the bottom of a deep
// message costs no Go stack. Walk keeps its own list of open iterators and calls
// itself never; a traversal rewritten to recurse would put one frame per level
// under the callback, and this test is what would notice.
//
// The frames are counted by the prefix of their name rather than by the whole
// of it, so that a rewriting which recursed through a closure — whose frames are
// Walk.func1 and not Walk — is counted too.
func TestParse_mimeDepth_walkStaysIterative(t *testing.T) {
	f := depthChain(deepChainLevels, "\r\n")

	_, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: depthOnly(LimitDisabled),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	var frames int
	var visited int
	walkErr := e.Walk(func(path []int, _ *Entity, err error) error {
		if err != nil {
			return err
		}
		visited++
		if len(path) != deepChainLevels {
			return nil
		}
		buf := make([]byte, 1<<20)
		frames = strings.Count(string(buf[:runtime.Stack(buf, false)]), "(*Entity).Walk")
		return nil
	})
	if walkErr != nil {
		t.Fatalf("Walk() = %v", walkErr)
	}
	if visited != deepChainLevels+1 {
		t.Fatalf("Walk visited %d entities, want %d", visited, deepChainLevels+1)
	}
	if frames != 1 {
		t.Errorf("the stack under walkFunc holds %d Walk frames at depth %d, want exactly 1",
			frames, deepChainLevels)
	}
}

// TestParse_mimeDepth_profileIndependent pins that this budget is not a matter
// of interpretation: every profile and every compatibility policy enforce it
// identically.
func TestParse_mimeDepth_profileIndependent(t *testing.T) {
	const levels = 3
	f := depthChain(levels, "\r\n")
	offset := f.offset(t, fmt.Sprintf("depth%d.header", levels))

	for _, profile := range []Profile{ProfileStrict, ProfileTolerant, ProfileForensic} {
		for _, compat := range []Compatibility{
			CompatibilityConservative, CompatibilityAngus,
			CompatibilityMimeKit, CompatibilityPythonEmail,
		} {
			opts := &ParseOptions{
				Profile:       profile,
				Compatibility: compat,
				Limits:        depthOnly(levels - 1),
			}
			session, e, err := Parse(strings.NewReader(f.text), opts)
			if err != nil {
				t.Fatalf("%v/%v: Parse() = %v", profile, compat, err)
			}
			got := readTree(e)
			checkDepthFatal(t, got.err, levels-1, chainPath(levels), offset)
			checkDepthDiagnostic(t, session, levels-1, chainPath(levels), offset)

			opts.Limits = depthOnly(levels)
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

// TestParse_mimeDepth_legacyPathsUnbounded pins the compatibility half of this
// item: the entry points that resolve no configuration budget no nesting, so a
// message far deeper than any default is read exactly as it always was.
func TestParse_mimeDepth_legacyPathsUnbounded(t *testing.T) {
	// Deeper than the documented default, so that a budget leaking into these
	// paths would be visible rather than merely possible.
	levels := int(DefaultLimits().MaxMIMEDepth) + 5
	f := depthChain(levels, "\r\n")

	for _, tc := range []struct {
		name string
		read func(io.Reader) (*Entity, error)
	}{
		{"Read", Read},
		{"ReadWithOptions", func(r io.Reader) (*Entity, error) { return ReadWithOptions(r, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := tc.read(strings.NewReader(f.text))
			if err != nil {
				t.Fatalf("%s() = %v", tc.name, err)
			}
			got := readTree(e)
			if got.err != nil {
				t.Fatalf("reading the tree = %v, want no budget on these paths", got.err)
			}
			if want := []string{"leaf"}; !reflect.DeepEqual(got.bodies, want) {
				t.Errorf("bodies = %v, want %v", got.bodies, want)
			}
		})
	}

	t.Run("New and NewMultipart", func(t *testing.T) {
		var h Header
		h.Set("Content-Type", "text/plain")
		leaf, err := New(h, strings.NewReader("leaf"))
		if err != nil {
			t.Fatalf("New() = %v", err)
		}

		entity := leaf
		for d := 0; d < levels; d++ {
			var mh Header
			mh.Set("Content-Type", "multipart/mixed")
			entity, err = NewMultipart(mh, []*Entity{entity})
			if err != nil {
				t.Fatalf("NewMultipart() = %v", err)
			}
		}

		var deepest int
		if err := entity.Walk(func(path []int, _ *Entity, err error) error {
			if err != nil {
				return err
			}
			if len(path) > deepest {
				deepest = len(path)
			}
			return nil
		}); err != nil {
			t.Fatalf("Walk() = %v", err)
		}
		if deepest != levels {
			t.Errorf("deepest assembled entity is at depth %d, want %d", deepest, levels)
		}
	})
}
