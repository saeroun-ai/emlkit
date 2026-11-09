package message

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Where the two tree budgets meet everything else a parse can refuse.
//
// Each test below fixes one order and pins it, because an order that is not
// pinned is an order that drifts: the same message must always report the same
// budget, whichever of them its bytes happen to exceed at once.

// crossFixture returns the message every test here aims a budget at. Its second
// part is a container, and that container's only child — the third part
// discovered, at depth two — is the entity all of these budgets converge on.
//
// preamble is placed before the container's first delimiter and extra inside the
// deep child's header block, so that one test can make the framing line
// oversized and another the header block, without either interfering with the
// other.
func crossFixture(preamble, extra string) originFixture {
	return newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=OUTER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--OUTER\r\n"},
		[2]string{"0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "zero\r\n"},
		[2]string{"", "--OUTER\r\n"},
		[2]string{"1.header", "Content-Type: multipart/alternative; boundary=INNER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"1.body", preamble},
		[2]string{"", "--INNER\r\n"},
		[2]string{"1.0.header", "Content-Type: text/html\r\n"},
		[2]string{"", extra},
		[2]string{"", "\r\n"},
		[2]string{"1.0.body", "deep\r\n"},
		[2]string{"", "--INNER--\r\n"},
		[2]string{"", "--OUTER--\r\n"},
	)
}

// crossLimits returns every budget disabled except the ones set turns on.
func crossLimits(set func(*Limits)) Limits {
	l := allDisabled()
	set(&l)
	return l
}

// TestParse_tree_partCountOutranksMIMEDepth pins the order between the two
// budgets this item adds. A child that is both one part too many and one level
// too deep reports limit.part_count, because the count is charged first — and
// each budget on its own still reports itself, so the order is a choice between
// two live budgets rather than one of them shadowing the other.
func TestParse_tree_partCountOutranksMIMEDepth(t *testing.T) {
	f := depthChain(3, "\r\n")
	offset := f.offset(t, "depth3.header")
	path := chainPath(3)

	for _, tc := range []struct {
		name  string
		parts int64
		depth int64
		want  Code
	}{
		{"part count alone", 2, LimitDisabled, CodeLimitPartCount},
		{"depth alone", LimitDisabled, 2, CodeLimitMIMEDepth},
		{"both at once", 2, 2, CodeLimitPartCount},
		{"count tighter than depth", 2, 3, CodeLimitPartCount},
		{"depth tighter than count", 3, 2, CodeLimitMIMEDepth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := crossLimits(func(l *Limits) {
				l.MaxPartCount = tc.parts
				l.MaxMIMEDepth = tc.depth
			})

			session, got, err := parseUnder(t, f.text, limits)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if !IsFatal(got.err) {
				t.Fatalf("reading the tree = %v, want a refusal", got.err)
			}

			diags := session.Diagnostics()
			if len(diags) != 1 {
				t.Fatalf("Diagnostics() = %v, want exactly one entry", codesOfDiags(diags))
			}
			if diags[0].Code != tc.want {
				t.Errorf("Code = %q, want %q", diags[0].Code, tc.want)
			}
			if !reflect.DeepEqual(diags[0].Path, path) {
				t.Errorf("Path = %v, want %v", diags[0].Path, path)
			}
			if want := LocationAt(offset); diags[0].Location != want {
				t.Errorf("Location = %v, want %v", diags[0].Location, want)
			}
		})
	}
}

// TestParse_tree_headerBudgetsComeFirst pins that a part which never framed
// successfully is never charged. A header block past one of its own budgets
// fails inside the reader below this layer, so the tree budgets are not
// consulted for it and only the header's own diagnostic is recorded — even
// though the same part would have been refused by both of them a moment later.
func TestParse_tree_headerBudgetsComeFirst(t *testing.T) {
	f := crossFixture("", budgetPad(200))
	headerOffset, headerSize := headerBlock(t, f, "1.0")

	for _, header := range []struct {
		name string
		set  func(*Limits)
		want Code
	}{
		{"MaxHeaderBytes", func(l *Limits) { l.MaxHeaderBytes = headerSize - 1 }, CodeLimitHeaderBytes},
		{"MaxHeaderFieldBytes", func(l *Limits) { l.MaxHeaderFieldBytes = 199 }, CodeLimitHeaderFieldBytes},
		{"MaxHeaderCount", func(l *Limits) { l.MaxHeaderCount = 1 }, CodeLimitHeaderCount},
		{"MaxLineBytes", func(l *Limits) { l.MaxLineBytes = 197 }, CodeLimitLineBytes},
	} {
		for _, tree := range []struct {
			name string
			set  func(*Limits)
		}{
			{"with the part count", func(l *Limits) { l.MaxPartCount = 2 }},
			{"with the depth", func(l *Limits) { l.MaxMIMEDepth = 1 }},
			{"with both", func(l *Limits) { l.MaxPartCount = 2; l.MaxMIMEDepth = 1 }},
		} {
			t.Run(header.name+" "+tree.name, func(t *testing.T) {
				limits := crossLimits(func(l *Limits) {
					header.set(l)
					tree.set(l)
				})

				session, got, err := parseUnder(t, f.text, limits)
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				if !IsFatal(got.err) {
					t.Fatalf("reading the tree = %v, want the header refusal", got.err)
				}

				diags := session.Diagnostics()
				if len(diags) != 1 {
					t.Fatalf("Diagnostics() = %v, want exactly the header budget entry", codesOfDiags(diags))
				}
				if diags[0].Code != header.want {
					t.Errorf("Code = %q, want %q: the header block is refused before a part exists to charge",
						diags[0].Code, header.want)
				}
				if want := []int{1, 0}; !reflect.DeepEqual(diags[0].Path, want) {
					t.Errorf("Path = %v, want %v", diags[0].Path, want)
				}
				if diags[0].Location.Offset < headerOffset {
					t.Errorf("Location = %v, want a position inside the refused header block at %d",
						diags[0].Location, headerOffset)
				}
			})
		}
	}
}

// TestParse_tree_framingLineBudgetComesFirst pins the same thing one step
// earlier. A framing line past the line budget stops the container before any
// child is framed at all, so neither tree budget is charged and only
// limit.line_bytes is recorded.
func TestParse_tree_framingLineBudgetComesFirst(t *testing.T) {
	const n = 300
	f := crossFixture(strings.Repeat("p", n)+"\r\n", "")
	offset := f.offset(t, "1.body")

	for _, tree := range []struct {
		name string
		set  func(*Limits)
	}{
		{"with the part count", func(l *Limits) { l.MaxPartCount = 2 }},
		{"with the depth", func(l *Limits) { l.MaxMIMEDepth = 1 }},
		{"with both", func(l *Limits) { l.MaxPartCount = 2; l.MaxMIMEDepth = 1 }},
	} {
		t.Run(tree.name, func(t *testing.T) {
			limits := crossLimits(func(l *Limits) {
				l.MaxLineBytes = n - 1
				tree.set(l)
			})

			session, got, err := parseUnder(t, f.text, limits)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if !IsFatal(got.err) {
				t.Fatalf("reading the tree = %v, want the framing line refusal", got.err)
			}
			// The leaf before the container is still recovered.
			if want := []string{"zero"}; !reflect.DeepEqual(got.bodies, want) {
				t.Errorf("bodies = %v, want %v", got.bodies, want)
			}
			checkLineDiagnostic(t, session, n-1, []int{1}, offset)
		})
	}
}

// TestParse_tree_recoveredBoundaryIsStillCounted pins that a container whose
// parameter list was cut short still yields children, and that those children
// are counted and placed exactly as any others.
//
// The parameter budgets are the one group that is not fatal: a boundary that
// arrived before the cut keeps working. The parts it introduces are ordinary
// parts, so the tree budgets see them and report the same path and position they
// would have reported for a container nobody had to repair.
func TestParse_tree_recoveredBoundaryIsStillCounted(t *testing.T) {
	f := newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=OUTER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--OUTER\r\n"},
		[2]string{"0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "zero\r\n"},
		[2]string{"", "--OUTER\r\n"},
		// The boundary arrives first and is kept; everything after it is past
		// the parameter budget and is ignored.
		[2]string{"1.header", "Content-Type: multipart/alternative; boundary=INNER; a=1; b=2; c=3\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"1.body", "--INNER\r\n"},
		[2]string{"1.0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"1.0.body", "deep\r\n"},
		[2]string{"", "--INNER--\r\n"},
		[2]string{"", "--OUTER--\r\n"},
	)

	t.Run("the children are found and counted", func(t *testing.T) {
		limits := crossLimits(func(l *Limits) {
			l.MaxParameterCount = 1
			l.MaxPartCount = 3
		})

		session, got, err := parseUnder(t, f.text, limits)
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		if got.err != nil {
			t.Fatalf("reading the tree = %v, want the recovered boundary to yield its child", got.err)
		}
		if want := []string{"zero", "deep"}; !reflect.DeepEqual(got.bodies, want) {
			t.Errorf("bodies = %v, want %v", got.bodies, want)
		}
		if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitParameterCount}) {
			t.Errorf("Diagnostics() = %v, want only the parameter budget entry", codes)
		}
	})

	for _, tc := range []struct {
		name string
		set  func(*Limits)
		want Code
	}{
		{"the part count still refuses the child", func(l *Limits) { l.MaxPartCount = 2 }, CodeLimitPartCount},
		{"the depth still refuses the child", func(l *Limits) { l.MaxMIMEDepth = 1 }, CodeLimitMIMEDepth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := crossLimits(func(l *Limits) {
				l.MaxParameterCount = 1
				tc.set(l)
			})

			session, got, err := parseUnder(t, f.text, limits)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if !IsFatal(got.err) {
				t.Fatalf("reading the tree = %v, want the tree budget to refuse", got.err)
			}

			diags := session.Diagnostics()
			want := []Code{CodeLimitParameterCount, tc.want}
			if !reflect.DeepEqual(codesOfDiags(diags), want) {
				t.Fatalf("Diagnostics() = %v, want %v in discovery order", codesOfDiags(diags), want)
			}
			last := diags[len(diags)-1]
			if wantPath := []int{1, 0}; !reflect.DeepEqual(last.Path, wantPath) {
				t.Errorf("Path = %v, want %v", last.Path, wantPath)
			}
			if wantLoc := LocationAt(f.offset(t, "1.0.header")); last.Location != wantLoc {
				t.Errorf("Location = %v, want %v", last.Location, wantLoc)
			}
		})
	}
}

// TestParse_tree_warningsSurviveTheBudgetFatal pins how the two kinds of report
// sit together. An unknown charset and an unknown transfer encoding are warnings
// about entities that are perfectly usable; a tree budget running out is a
// failure of the parse. The warnings are recorded where they were discovered,
// the parts they describe stay readable, and the budget's own diagnostic comes
// last because that is when it happened.
func TestParse_tree_warningsSurviveTheBudgetFatal(t *testing.T) {
	f := newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=B\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--B\r\n"},
		[2]string{"0.header", "Content-Type: text/plain; charset=made-up-charset\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "zero\r\n"},
		[2]string{"", "--B\r\n"},
		[2]string{"1.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "Content-Transfer-Encoding: nonsense\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"1.body", "one\r\n"},
		[2]string{"", "--B\r\n"},
		[2]string{"2.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"2.body", "two\r\n"},
		[2]string{"", "--B--\r\n"},
	)

	session, got, err := parseUnder(t, f.text, partsOnly(2))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if !IsFatal(got.err) {
		t.Fatalf("reading the tree = %v, want the part-count refusal", got.err)
	}

	// The usable prefix is intact: both warned-about parts yielded their raw
	// bytes, which is what the warnings say they would.
	if want := []string{"zero", "one"}; !reflect.DeepEqual(got.bodies, want) {
		t.Errorf("bodies = %v, want %v", got.bodies, want)
	}

	want := []Code{CodeCharsetUnknown, CodeCTEUnknown, CodeLimitPartCount}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, want) {
		t.Fatalf("Diagnostics() = %v, want %v in discovery order", codes, want)
	}

	diags := session.Diagnostics()
	last := diags[len(diags)-1]
	if wantPath := []int{2}; !reflect.DeepEqual(last.Path, wantPath) {
		t.Errorf("Path = %v, want %v", last.Path, wantPath)
	}
	if wantLoc := LocationAt(f.offset(t, "2.header")); last.Location != wantLoc {
		t.Errorf("Location = %v, want %v", last.Location, wantLoc)
	}
}

// TestParse_tree_walkNeverHandsOverARefusedChild pins Walk against the one way
// a refused child could reach walkFunc disguised as a warning.
//
// A tree budget refuses a child by returning no part at all. When the caller's
// sink stops the parse while it is being told about that refusal, the error the
// refusal returns is the session's stop, and that stop wraps whatever the sink
// returned — which is the caller's own value, and may well wrap an
// UnknownEncodingError or UnknownCharsetError the caller kept from earlier.
// Walk forwards those two warnings to walkFunc together with the part they
// describe. A refusal has no part, so it must end the walk as the identical stop
// every other scope reports, rather than be forwarded with a nil entity that a
// walkFunc continuing past warnings would then have Walk descend into.
//
// The header budget case is the same path out of NextPart, and is pinned beside
// the two tree budgets because one guard in Walk covers all three.
func TestParse_tree_walkNeverHandsOverARefusedChild(t *testing.T) {
	warnings := []struct {
		name string
		err  error
	}{
		{"encoding warning", UnknownEncodingError{errors.New("kept from an earlier part")}},
		{"charset warning", UnknownCharsetError{errors.New("kept from an earlier part")}},
	}
	refusals := []struct {
		name    string
		text    string
		limits  Limits
		code    Code
		visited [][]int
	}{
		{"part count", spreadTree("\r\n").text, partsOnly(2), CodeLimitPartCount, [][]int{nil, {0}, {0, 0}}},
		{"mime depth", depthChain(3, "\r\n").text, depthOnly(2), CodeLimitMIMEDepth, [][]int{nil, {0}, {0, 0}}},
		{
			"header field bytes",
			crossFixture("", budgetPad(200)).text,
			crossLimits(func(l *Limits) { l.MaxHeaderFieldBytes = 199 }),
			CodeLimitHeaderFieldBytes,
			[][]int{nil, {0}, {1}},
		},
	}

	for _, refusal := range refusals {
		for _, warning := range warnings {
			t.Run(refusal.name+" "+warning.name, func(t *testing.T) {
				sinkErr := fmt.Errorf("stopped at a budget: %w", warning.err)

				session, e, err := Parse(strings.NewReader(refusal.text), &ParseOptions{
					Limits: refusal.limits,
					DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
						if d.Code == refusal.code {
							return sinkErr
						}
						return nil
					}),
				})
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}

				var (
					visited  [][]int
					nilParts int
					panicked any
				)
				walkErr := func() error {
					defer func() { panicked = recover() }()
					return e.Walk(func(path []int, part *Entity, err error) error {
						if part == nil {
							nilParts++
						} else {
							visited = append(visited, path)
						}
						// The ordinary shape of a walkFunc: a warning describes a
						// usable part, so it is noted and the walk goes on.
						if IsUnknownEncoding(err) || IsUnknownCharset(err) {
							return nil
						}
						return err
					})
				}()

				if panicked != nil {
					t.Fatalf("Walk panicked: %v", panicked)
				}
				if nilParts != 0 {
					t.Errorf("walkFunc was handed a nil entity %d times, want never", nilParts)
				}

				stop := session.fatalErr()
				if stop == nil {
					t.Fatal("fatalErr() = nil, want the sink's stop published")
				}
				if walkErr != error(stop) {
					t.Errorf("Walk() = %v, want the identical published stop %v", walkErr, stop)
				}
				if !errors.Is(walkErr, sinkErr) {
					t.Errorf("Walk() = %v, want it to wrap the sink's own error", walkErr)
				}
				// The precondition that makes this test mean something: the stop
				// really does answer to a warning predicate.
				if !IsUnknownEncoding(walkErr) && !IsUnknownCharset(walkErr) {
					t.Fatalf("Walk() = %v, want an error a warning predicate accepts", walkErr)
				}

				if !reflect.DeepEqual(visited, refusal.visited) {
					t.Errorf("Walk visited %v, want %v: everything before the refusal", visited, refusal.visited)
				}
				if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{refusal.code}) {
					t.Errorf("Diagnostics() = %v, want exactly the refusal %q", got, refusal.code)
				}
			})
		}
	}
}

// TestParse_tree_sessionStopOutranksAnEarlierDepthFailure pins how the two
// failures compose over time, which neither budget's own tests can show.
//
// A depth refusal is local and sticky: the iterator that hit it keeps reporting
// it, while the parent goes on. When the parse later stops as a whole — the part
// count running out elsewhere, or the caller's sink refusing to continue while
// it is told so — that iterator is one of the "iterators built earlier for other
// containers", and from then on it must report the identical session-wide stop
// rather than its own older failure. That holds only because NextPart consults
// the session before its own sticky error, which is what this pins.
//
// The depth-refused child is charged against the part count, because the count
// is charged first and the depth is judged only for a child that fit; so the
// container, the refused child and the next sibling spend a budget of three and
// the sibling after them is the one refused.
func TestParse_tree_sessionStopOutranksAnEarlierDepthFailure(t *testing.T) {
	f := newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=OUTER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--OUTER\r\n"},
		[2]string{"0.header", "Content-Type: multipart/alternative; boundary=INNER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "--INNER\r\n"},
		[2]string{"0.0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.0.body", "deep\r\n"},
		[2]string{"", "--INNER--\r\n"},
		[2]string{"", "--OUTER\r\n"},
		[2]string{"1.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"1.body", "one\r\n"},
		[2]string{"", "--OUTER\r\n"},
		[2]string{"2.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"2.body", "two\r\n"},
		[2]string{"", "--OUTER--\r\n"},
	)
	limits := crossLimits(func(l *Limits) {
		l.MaxMIMEDepth = 1
		l.MaxPartCount = 3
	})

	sentinel := errors.New("stop here")
	for _, tc := range []struct {
		name    string
		sinkErr error
	}{
		{"the part count stops the parse", nil},
		{"the sink stops the parse at the part count", sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
				Limits: limits,
				DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
					if d.Code == CodeLimitPartCount {
						return tc.sinkErr
					}
					return nil
				}),
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}

			outer := e.MultipartReader()
			if outer == nil {
				t.Fatal("MultipartReader() = nil, want the parts of a multipart message")
			}
			container, err := outer.NextPart()
			if err != nil {
				t.Fatalf("NextPart() = %v, want the container", err)
			}
			inner := container.MultipartReader()
			if inner == nil {
				t.Fatal("the first part is not multipart")
			}

			// The local failure, sticky on its own iterator.
			_, depthErr := inner.NextPart()
			checkDepthFatal(t, depthErr, 1, []int{0, 0}, f.offset(t, "0.0.header"))
			if _, err := inner.NextPart(); err != depthErr {
				t.Errorf("the depth-failed iterator asked again = %v, want its identical error %v", err, depthErr)
			}

			// The parent goes on past it.
			one, err := outer.NextPart()
			if err != nil {
				t.Fatalf("NextPart() after the local failure = %v, want the next sibling", err)
			}
			if got := readBodyOf(t, one); got != "one" {
				t.Errorf("second part = %q, want %q", got, "one")
			}

			// And then the parse stops as a whole.
			last, stop := outer.NextPart()
			if last != nil {
				t.Errorf("the over-budget part was returned as %v, want no part", last)
			}
			if tc.sinkErr != nil {
				if !errors.Is(stop, tc.sinkErr) || !IsFatal(stop) {
					t.Fatalf("NextPart() = %v, want the sink's stop", stop)
				}
			} else {
				checkPartCountFatal(t, stop, 3, []int{2}, f.offset(t, "2.header"))
			}
			if published := error(session.fatalErr()); published != stop {
				t.Fatalf("fatalErr() = %v, want the identical %v", published, stop)
			}

			// Every scope reports that stop from here on — the depth-failed
			// iterator included, which no longer reports its own older failure.
			for i := 0; i < 2; i++ {
				p, err := inner.NextPart()
				if p != nil {
					t.Errorf("call %d on the depth-failed iterator returned %v, want no part", i, p)
				}
				if err != stop {
					t.Errorf("call %d on the depth-failed iterator = %v, want the session's stop %v", i, err, stop)
				}
			}
			if _, err := outer.NextPart(); err != stop {
				t.Errorf("the parent iterator = %v, want the session's stop %v", err, stop)
			}
			var visited [][]int
			walkErr := e.Walk(func(path []int, _ *Entity, err error) error {
				visited = append(visited, path)
				return err
			})
			if walkErr != stop {
				t.Errorf("Walk() = %v, want the session's stop %v", walkErr, stop)
			}
			if visited != nil {
				t.Errorf("Walk visited %v, want nothing: the parse had already stopped", visited)
			}

			// Each failure was recorded once, in discovery order.
			diags := session.Diagnostics()
			if got, want := codesOfDiags(diags), []Code{CodeLimitMIMEDepth, CodeLimitPartCount}; !reflect.DeepEqual(got, want) {
				t.Fatalf("Diagnostics() = %v, want %v", got, want)
			}
			checkDepthFacts(t, diags[0], SeverityLimitExceeded, 1, []int{0, 0}, f.offset(t, "0.0.header"))
			checkPartCountFacts(t, diags[1], SeverityLimitExceeded, 3, []int{2}, f.offset(t, "2.header"))
		})
	}
}

// TestParse_tree_verbatimRoundTrip pins that neither budget disturbs the bytes.
// A message parsed under a budget that admits it writes back out byte for byte,
// which is the invariant a DKIM signature depends on.
//
// The generated fixtures declare no MIME-Version, which the writer would supply
// for the message itself, so it is prepended here rather than to the generators:
// what is under test is the budget, not what a header a message omits is filled
// in with.
func TestParse_tree_verbatimRoundTrip(t *testing.T) {
	const mimeVersion = "Mime-Version: 1.0\r\n"

	for _, tc := range []struct {
		name   string
		text   string
		limits Limits
	}{
		{"at the part count", mimeVersion + spreadTree("\r\n").text, partsOnly(int64(len(spreadOrder)))},
		{"at the depth", mimeVersion + depthChain(4, "\r\n").text, depthOnly(4)},
		{"at both", mimeVersion + spreadTree("\r\n").text, crossLimits(func(l *Limits) {
			l.MaxPartCount = int64(len(spreadOrder))
			l.MaxMIMEDepth = 2
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e, err := Parse(strings.NewReader(tc.text), &ParseOptions{Limits: tc.limits})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}

			var b bytes.Buffer
			if err := e.WriteTo(&b); err != nil {
				t.Fatalf("WriteTo() = %v", err)
			}
			if got := b.String(); got != tc.text {
				t.Errorf("round trip changed the message:\n%q\nwant\n%q", got, tc.text)
			}
		})
	}
}

// TestParse_tree_detachedBodyKeepsTheBudgets pins what happens when a caller
// replaces an entity's body and then iterates it.
//
// The replacement is not the input's bytes, so the parts it yields get no path
// and no position — a diagnostic about them says the position is unknown rather
// than claiming one. What does not change is that they are parts of this parse:
// the shared count still charges them, and the depth keeps counting from the
// entity whose body was replaced rather than starting over at zero.
func TestParse_tree_detachedBodyKeepsTheBudgets(t *testing.T) {
	// A message whose first part is a container, so that the body replaced below
	// belongs to an entity at depth one.
	const text = "Content-Type: multipart/mixed; boundary=OUTER\r\n\r\n" +
		"--OUTER\r\n" +
		"Content-Type: multipart/alternative; boundary=A\r\n\r\n" +
		"--A\r\n\r\noriginal\r\n" +
		"--A--\r\n" +
		"--OUTER--\r\n"

	replacement := func() string {
		var b strings.Builder
		for i := 0; i < 3; i++ {
			fmt.Fprintf(&b, "--A\r\nContent-Type: text/plain\r\n\r\nr%d\r\n", i)
		}
		b.WriteString("--A--\r\n")
		return b.String()
	}

	open := func(t *testing.T, limits Limits) (*ParseSession, MultipartReader) {
		t.Helper()

		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		outer := e.MultipartReader()
		if outer == nil {
			t.Fatal("MultipartReader() = nil, want the parts of a multipart message")
		}
		container, err := outer.NextPart()
		if err != nil {
			t.Fatalf("NextPart() = %v, want the container", err)
		}

		container.Body = strings.NewReader(replacement())
		mr := container.MultipartReader()
		if mr == nil {
			t.Fatal("MultipartReader() on the replaced body = nil")
		}
		return session, mr
	}

	t.Run("the shared count still charges", func(t *testing.T) {
		// One part is spent on the container itself, so two of the replacement's
		// three parts fit and the third does not.
		session, mr := open(t, partsOnly(3))

		for i := 0; i < 2; i++ {
			if _, err := mr.NextPart(); err != nil {
				t.Fatalf("NextPart() %d on the replaced body = %v", i, err)
			}
		}
		p, err := mr.NextPart()
		if p != nil {
			t.Errorf("the over-budget part was returned as %v, want no part", p)
		}
		if !IsFatal(err) {
			t.Fatalf("NextPart() = %v, want the part-count refusal", err)
		}

		diags := session.Diagnostics()
		if len(diags) != 1 || diags[0].Code != CodeLimitPartCount {
			t.Fatalf("Diagnostics() = %v, want one part-count entry", codesOfDiags(diags))
		}
		if diags[0].Path != nil {
			t.Errorf("Path = %v, want none: a replaced body has no place in the parsed tree", diags[0].Path)
		}
		if diags[0].Location != (Location{}) {
			t.Errorf("Location = %v, want unknown: a replaced body has no position in the input", diags[0].Location)
		}
	})

	t.Run("the depth keeps counting", func(t *testing.T) {
		// The replaced body belongs to the entity at depth one, so its parts are
		// at depth two and a budget of one refuses the first of them. A depth
		// that restarted at zero for a replaced body would admit them all.
		session, mr := open(t, depthOnly(1))

		p, err := mr.NextPart()
		if p != nil {
			t.Errorf("the over-deep part was returned as %v, want no part", p)
		}
		if !IsFatal(err) {
			t.Fatalf("NextPart() = %v, want the depth refusal", err)
		}

		diags := session.Diagnostics()
		if len(diags) != 1 || diags[0].Code != CodeLimitMIMEDepth {
			t.Fatalf("Diagnostics() = %v, want one mime-depth entry", codesOfDiags(diags))
		}
	})

	t.Run("a budget with room admits the replacement", func(t *testing.T) {
		session, mr := open(t, crossLimits(func(l *Limits) {
			l.MaxPartCount = 4
			l.MaxMIMEDepth = 2
		}))

		var bodies []string
		for i := 0; i < 3; i++ {
			p, err := mr.NextPart()
			if err != nil {
				t.Fatalf("NextPart() %d = %v", i, err)
			}
			bodies = append(bodies, readBodyOf(t, p))
		}
		if want := []string{"r0", "r1", "r2"}; !reflect.DeepEqual(bodies, want) {
			t.Errorf("bodies = %v, want %v", bodies, want)
		}
		if d := session.Diagnostics(); d != nil {
			t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
		}
	})
}
