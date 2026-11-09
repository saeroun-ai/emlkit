package message

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit/textproto"
)

// The three header block budgets ROB-001-003 adds — the field count, one
// logical field's bytes, and one physical line's bytes — applied to every
// entity of a parse.
//
// Every fixture below is assembled from labelled segments, so each expected
// size and each expected offset is derived from the fixture rather than counted
// out by hand: a change to a fixture cannot quietly move a boundary these tests
// claim to pin. Each budget is swept across the exact quantity it bounds at
// every depth a message has.

// allDisabled returns Limits with every budget turned off.
//
// It is what isolates one budget from the rest. Several of them can be exceeded
// by the same bytes — a long line is also a long field — so a test that means to
// say something about one of them turns the others off and says it about that
// one alone. Precedence when several are on is pinned separately, by
// TestParse_headerLimits_precedence.
func allDisabled() Limits {
	var l Limits
	for _, f := range limitFields {
		*f.get(&l) = LimitDisabled
	}
	return l
}

// withLimit returns every budget disabled except the one set applies.
func withLimit(set func(*Limits)) Limits {
	l := allDisabled()
	set(&l)
	return l
}

// A limitCase is one placement of the entity whose header block a budget is
// swept across, together with what a parse under a sufficient budget yields and
// what it recovers before an insufficient one stops it.
type limitCase struct {
	name string

	// entity labels the header block under test, using the fixture's own
	// labels: "root" for the message itself, "1" for a part of it, "0.1" for a
	// part of a part.
	entity string

	// path is the MIME path of that entity, as a diagnostic reports it.
	path []int

	// all is the body of every leaf of the message, in the order a traversal
	// reaches them, and upTo is what a traversal recovers before the refusal.
	all  []string
	upTo []string

	// baseFields is how many header fields the block under test carries before
	// the ones a test adds to it, which is what the shape of the message at this
	// placement requires.
	baseFields int64

	fixture originFixture
}

// limitFixtures returns the three placements of one header block under test.
//
// extra supplies the header lines that block carries beyond the ones the shape
// of the message requires, as labelled segments, so that a test can mark where
// it expects a budget to refuse. eol is the line ending every line of the
// message uses, so that the same tree can be presented with CRLF and with the
// bare LF that appears in the wild; offsets follow it by themselves.
func limitFixtures(eol string, extra func(eol string) [][2]string) []limitCase {
	seg := func(parts ...[][2]string) [][2]string {
		var out [][2]string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}

	root := newOriginFixture(seg(
		[][2]string{
			{"root.header", "Mime-Version: 1.0" + eol},
			{"", "Content-Type: multipart/mixed; boundary=B" + eol},
		},
		extra(eol),
		[][2]string{
			{"end", eol},
			{"root.body", "--B" + eol},
			{"0.header", "Content-Type: text/plain" + eol},
			{"", eol},
			{"0.body", "zero" + eol},
			{"", "--B--" + eol},
		},
	)...)

	child := newOriginFixture(seg(
		[][2]string{
			{"", "Mime-Version: 1.0" + eol},
			{"", "Content-Type: multipart/mixed; boundary=B" + eol},
			{"", eol},
			{"", "--B" + eol},
			{"", "Content-Type: text/plain" + eol},
			{"", eol},
			{"", "zero" + eol},
			{"", "--B" + eol},
			{"1.header", "Content-Type: text/plain" + eol},
		},
		extra(eol),
		[][2]string{
			{"end", eol},
			{"1.body", "one" + eol},
			{"", "--B" + eol},
			{"", "Content-Type: text/plain" + eol},
			{"", eol},
			{"", "two" + eol},
			{"", "--B--" + eol},
		},
	)...)

	deep := newOriginFixture(seg(
		[][2]string{
			{"", "Mime-Version: 1.0" + eol},
			{"", "Content-Type: multipart/mixed; boundary=OUTER" + eol},
			{"", eol},
			{"", "--OUTER" + eol},
			{"", "Content-Type: multipart/alternative; boundary=INNER" + eol},
			{"", eol},
			{"", "--INNER" + eol},
			{"", "Content-Type: text/plain" + eol},
			{"", eol},
			{"", "deep zero" + eol},
			{"", "--INNER" + eol},
			{"0.1.header", "Content-Type: text/html" + eol},
		},
		extra(eol),
		[][2]string{
			{"end", eol},
			{"0.1.body", "deep one" + eol},
			{"", "--INNER--" + eol},
			{"", "--OUTER" + eol},
			{"", "Content-Type: text/plain" + eol},
			{"", eol},
			{"", "second" + eol},
			{"", "--OUTER--" + eol},
		},
	)...)

	return []limitCase{
		{
			name: "root", entity: "root", path: nil,
			all: []string{"zero"}, upTo: nil,
			// Mime-Version and Content-Type.
			baseFields: 2,
			fixture:    root,
		},
		{
			name: "child", entity: "1", path: []int{1},
			all: []string{"zero", "one", "two"}, upTo: []string{"zero"},
			// Content-Type.
			baseFields: 1,
			fixture:    child,
		},
		{
			name: "deep child", entity: "0.1", path: []int{0, 1},
			all: []string{"deep zero", "deep one", "second"}, upTo: []string{"deep zero"},
			baseFields: 1,
			fixture:    deep,
		},
	}
}

// checkFieldsDiagnostic pins the one diagnostic a refused header block records
// for one of these three budgets: its code, its severity, the entity it names,
// and the exact position of the line or field that was refused.
func checkFieldsDiagnostic(t *testing.T, session *ParseSession, code Code, limit int64, path []int, offset int64) {
	t.Helper()

	diags := session.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one %s entry", codesOfDiags(diags), code)
	}
	d := diags[0]

	if d.Code != code {
		t.Errorf("Code = %q, want %q", d.Code, code)
	}
	if d.Severity != SeverityLimitExceeded {
		t.Errorf("Severity = %v, want %v", d.Severity, SeverityLimitExceeded)
	}
	if !reflect.DeepEqual(d.Path, path) {
		t.Errorf("Path = %v, want %v", d.Path, path)
	}
	if want := LocationAt(offset); d.Location != want {
		t.Errorf("Location = %v, want %v: the first byte of the refused line or field", d.Location, want)
	}
	if d.Action == "" {
		t.Errorf("Action is empty, want what was done about the defect")
	}
	if !strings.Contains(d.Message, fmt.Sprint(limit)) {
		t.Errorf("Message = %q, want the budget %d in it", d.Message, limit)
	}
	if d.Cause == nil {
		t.Errorf("Cause is nil, want the underlying budget failure")
	}
}

// checkFieldsFatal pins the error a refused header block returns: fatal,
// wrapping the sentinel of the budget that refused it, and carrying the same
// diagnostic with the severity a fatal error's diagnostic always has.
func checkFieldsFatal(t *testing.T, err error, code Code, sentinel error, path []int, offset int64) {
	t.Helper()

	if !IsFatal(err) {
		t.Fatalf("err = %v, want a FatalError", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to wrap %v", err, sentinel)
	}

	var fatal FatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v, want a FatalError", err)
	}
	d, ok := fatal.Diagnostic()
	if !ok {
		t.Fatalf("FatalError carries no diagnostic, want the one that stopped the scope")
	}
	if d.Code != code {
		t.Errorf("fatal diagnostic Code = %q, want %q", d.Code, code)
	}
	if d.Severity != SeverityFatal {
		t.Errorf("fatal diagnostic Severity = %v, want %v", d.Severity, SeverityFatal)
	}
	if !reflect.DeepEqual(d.Path, path) {
		t.Errorf("fatal diagnostic Path = %v, want %v", d.Path, path)
	}
	if want := LocationAt(offset); d.Location != want {
		t.Errorf("fatal diagnostic Location = %v, want %v", d.Location, want)
	}
}

// A headerBudgetSpec is one of the three budgets under test: how to configure
// it, what a block costs under it, where a refusal points, and what it reports.
type headerBudgetSpec struct {
	name string

	// code and sentinel are what a refusal records and wraps.
	code     Code
	sentinel error

	// extra supplies the header lines the entity under test carries, which are
	// what makes it the largest in its message by this budget's measure.
	extra func(eol string) [][2]string

	// set writes the budget into a Limits value.
	set func(l *Limits, v int64)

	// size is the exact budget at which the block still fits, derived from the
	// fixture, and unit is where the refusal points once the budget is one
	// short of it.
	size func(t *testing.T, c limitCase, eol string) int64
	unit func(t *testing.T, c limitCase, eol string) int64
}

// headerBudgetSpecs returns the three budgets with the fixtures that isolate
// each.
func headerBudgetSpecs() []headerBudgetSpec {
	// A field of exactly n bytes on one line, terminator included.
	pad := func(name string, n int, eol string) string {
		head := name + ": "
		return head + strings.Repeat("x", n-len(head)-len(eol)) + eol
	}

	return []headerBudgetSpec{
		{
			name:     "header count",
			code:     CodeLimitHeaderCount,
			sentinel: textproto.ErrTooManyHeaderFields,
			extra: func(eol string) [][2]string {
				return [][2]string{
					{"", "X-One: 1" + eol},
					{"", "X-Two: 2" + eol},
					// A field whose name is empty is skipped when the block is
					// parsed but is a candidate all the same, so it is charged
					// like any other. Counting only the fields that survive
					// parsing would let a block of these evade the budget
					// entirely.
					{"", ": nameless" + eol},
					// The last field, which is the one a budget of size-1
					// refuses.
					{"unit", "X-Four: 4" + eol},
				}
			},
			set: func(l *Limits, v int64) { l.MaxHeaderCount = v },
			size: func(t *testing.T, c limitCase, eol string) int64 {
				return c.baseFields + 4
			},
			unit: func(t *testing.T, c limitCase, eol string) int64 {
				return c.fixture.offset(t, "unit")
			},
		},
		{
			name:     "header field bytes",
			code:     CodeLimitHeaderFieldBytes,
			sentinel: textproto.ErrHeaderFieldTooBig,
			extra: func(eol string) [][2]string {
				// One logical field spanning three physical lines, so that the
				// budget has to cover a field's continuations and its line
				// terminators rather than one line of it. It is comfortably the
				// largest field in every block of every fixture, so a budget
				// swept across its size says something about it and about no
				// other field.
				return [][2]string{
					{"unit", "X-Folded: " + strings.Repeat("a", 40) + eol},
					{"", " " + strings.Repeat("b", 40) + eol},
					{"", " " + strings.Repeat("c", 40) + eol},
					{"after", "X-Tail: t" + eol},
				}
			},
			set: func(l *Limits, v int64) { l.MaxHeaderFieldBytes = v },
			size: func(t *testing.T, c limitCase, eol string) int64 {
				return c.fixture.offset(t, "after") - c.fixture.offset(t, "unit")
			},
			unit: func(t *testing.T, c limitCase, eol string) int64 {
				return c.fixture.offset(t, "unit")
			},
		},
		{
			name:     "header line bytes",
			code:     CodeLimitLineBytes,
			sentinel: textproto.ErrLineTooBig,
			extra: func(eol string) [][2]string {
				return [][2]string{
					{"", "X-Short: s" + eol},
					{"unit", pad("X-Long", 120+len(eol), eol)},
					{"after", "X-Tail: t" + eol},
				}
			},
			set: func(l *Limits, v int64) { l.MaxLineBytes = v },
			size: func(t *testing.T, c limitCase, eol string) int64 {
				// The line without the terminator that ends it.
				return c.fixture.offset(t, "after") - c.fixture.offset(t, "unit") - int64(len(eol))
			},
			unit: func(t *testing.T, c limitCase, eol string) int64 {
				return c.fixture.offset(t, "unit")
			},
		},
	}
}

// TestParse_headerLimits_boundary sweeps each budget across the exact quantity
// it bounds, at each depth a message has and with each line ending, and pins
// where the boundary falls: a block at exactly the budget is read, one unit more
// is refused, and the refusal names the entity it happened to and the exact
// position of the line or field it refused.
func TestParse_headerLimits_boundary(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			for _, eol := range []string{"\r\n", "\n"} {
				t.Run(eolName(eol), func(t *testing.T) {
					for _, tc := range limitFixtures(eol, spec.extra) {
						t.Run(tc.name, func(t *testing.T) {
							size := spec.size(t, tc, eol)
							unit := spec.unit(t, tc, eol)

							for _, delta := range []int64{-1, 0, 1} {
								t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
									limit := size + delta
									session, got, err := parseUnder(t, tc.fixture.text,
										withLimit(func(l *Limits) { spec.set(l, limit) }))

									if delta >= 0 {
										if err != nil {
											t.Fatalf("Parse() = %v, want %d to fit a budget of %d", err, size, limit)
										}
										if got.err != nil {
											t.Fatalf("reading the tree = %v", got.err)
										}
										if !reflect.DeepEqual(got.bodies, tc.all) {
											t.Errorf("bodies = %v, want %v", got.bodies, tc.all)
										}
										if d := session.Diagnostics(); d != nil {
											t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
										}
										return
									}

									stopped := err
									if stopped == nil {
										stopped = got.err
									}
									checkFieldsFatal(t, stopped, spec.code, spec.sentinel, tc.path, unit)
									checkFieldsDiagnostic(t, session, spec.code, limit, tc.path, unit)

									if !reflect.DeepEqual(got.bodies, tc.upTo) {
										t.Errorf("bodies = %v, want %v recovered before the refusal", got.bodies, tc.upTo)
									}
								})
							}
						})
					}
				})
			}
		})
	}
}

// TestParse_headerLimits_refusedBlockYieldsNothing pins that a refusal is all or
// nothing for the block it refused: the message's own header produces no entity
// at all, and a part's produces no part — never the fields that happened to fit,
// which no caller could tell apart from a message that lacked the rest.
func TestParse_headerLimits_refusedBlockYieldsNothing(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			cases := limitFixtures("\r\n", spec.extra)

			// The message's own header block.
			root := cases[0]
			session, e, err := Parse(strings.NewReader(root.fixture.text),
				&ParseOptions{Limits: withLimit(func(l *Limits) { spec.set(l, spec.size(t, root, "\r\n")-1) })})
			if e != nil {
				t.Errorf("Parse() = %v, want no entity when the message header was refused", e)
			}
			checkFieldsFatal(t, err, spec.code, spec.sentinel, nil, spec.unit(t, root, "\r\n"))
			checkFieldsDiagnostic(t, session, spec.code, spec.size(t, root, "\r\n")-1, nil, spec.unit(t, root, "\r\n"))

			// A part's header block.
			child := cases[1]
			_, e, err = Parse(strings.NewReader(child.fixture.text),
				&ParseOptions{Limits: withLimit(func(l *Limits) { spec.set(l, spec.size(t, child, "\r\n")-1) })})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			mr := e.MultipartReader()
			if _, err := mr.NextPart(); err != nil {
				t.Fatalf("NextPart() = %v, want the part before the refused one", err)
			}
			p, err := mr.NextPart()
			if p != nil {
				t.Errorf("NextPart() = %v, want no part from a refused header block", p)
			}
			if !IsFatal(err) {
				t.Errorf("NextPart() = %v, want a FatalError", err)
			}
		})
	}
}

// TestParse_headerLimits_disabled pins that LimitDisabled turns each budget off,
// at every depth, on a message that every one of them would otherwise refuse.
func TestParse_headerLimits_disabled(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			for _, tc := range limitFixtures("\r\n", spec.extra) {
				t.Run(tc.name, func(t *testing.T) {
					session, got, err := parseUnder(t, tc.fixture.text,
						withLimit(func(l *Limits) { spec.set(l, LimitDisabled) }))
					if err != nil {
						t.Fatalf("Parse() = %v", err)
					}
					if got.err != nil {
						t.Fatalf("reading the tree = %v", got.err)
					}
					if !reflect.DeepEqual(got.bodies, tc.all) {
						t.Errorf("bodies = %v, want %v", got.bodies, tc.all)
					}
					if d := session.Diagnostics(); d != nil {
						t.Errorf("Diagnostics() = %v, want none from a disabled budget", codesOfDiags(d))
					}
				})
			}
		})
	}
}

// TestParse_headerLimits_maxInt64IsFinite pins that math.MaxInt64 is an ordinary
// finite budget rather than a synonym for disabled: it admits every real message
// without the arithmetic that enforces it wrapping into an accepted answer.
func TestParse_headerLimits_maxInt64IsFinite(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			tc := limitFixtures("\r\n", spec.extra)[1]

			session, got, err := parseUnder(t, tc.fixture.text,
				withLimit(func(l *Limits) { spec.set(l, math.MaxInt64) }))
			if err != nil || got.err != nil {
				t.Fatalf("Parse() = %v, reading the tree = %v", err, got.err)
			}
			if !reflect.DeepEqual(got.bodies, tc.all) {
				t.Errorf("bodies = %v, want %v", got.bodies, tc.all)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
			}
			if got := session.Limits(); *specField(&got, spec) != math.MaxInt64 {
				t.Errorf("Limits() reports %d, want the budget that was asked for", *specField(&got, spec))
			}
		})
	}
}

// specField reaches the budget a spec configures inside a Limits value, so that
// a test can read back what the session reports.
func specField(l *Limits, spec headerBudgetSpec) *int64 {
	var probe Limits
	spec.set(&probe, 1)
	for _, f := range limitFields {
		if *f.get(&probe) == 1 {
			return f.get(l)
		}
	}
	panic("spec sets no budget")
}

// TestParse_headerLimits_independentScopes pins that each budget bounds the
// scope it names and no wider one. A message of many parts, each with many
// fields, is read under budgets that the whole message exceeds many times over
// but that no single block, field or line does.
func TestParse_headerLimits_independentScopes(t *testing.T) {
	const parts = 40
	const fields = 8

	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < parts; i++ {
		b.WriteString("--B\r\n")
		b.WriteString("Content-Type: text/plain\r\n")
		for j := 0; j < fields; j++ {
			fmt.Fprintf(&b, "X-Pad-%d: %s\r\n", j, strings.Repeat("x", 40))
		}
		b.WriteString("\r\n")
		fmt.Fprintf(&b, "part %d\r\n", i)
	}
	b.WriteString("--B--\r\n")

	// Budgets that exactly admit one part's header block: its own field count,
	// its longest field, its longest line.
	limits := allDisabled()
	limits.MaxHeaderCount = fields + 1
	limits.MaxHeaderFieldBytes = int64(len("X-Pad-0: ") + 40 + len("\r\n"))
	limits.MaxLineBytes = int64(len("X-Pad-0: ") + 40)

	session, got, err := parseUnder(t, b.String(), limits)
	if err != nil || got.err != nil {
		t.Fatalf("Parse() = %v, reading the tree = %v", err, got.err)
	}
	if len(got.bodies) != parts {
		t.Errorf("read %d parts, want %d: every budget here is per entity, field or line", len(got.bodies), parts)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
	}
}

// TestParse_headerLimits_precedence pins which budget a block that exceeds
// several at once is refused by: the one bounding the smallest scope. One
// enormous line is also an enormous field and an enormous block, and which of
// the three is reported must not depend on how the input happens to arrive.
func TestParse_headerLimits_precedence(t *testing.T) {
	const text = "Content-Type: text/plain\r\n" +
		"X-Long: " + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + "\r\n" +
		"\r\nbody\r\n"

	for _, tc := range []struct {
		name   string
		limits Limits
		want   Code
	}{
		{
			name: "line before field",
			limits: func() Limits {
				l := allDisabled()
				l.MaxLineBytes = 8
				l.MaxHeaderFieldBytes = 8
				return l
			}(),
			want: CodeLimitLineBytes,
		},
		{
			name: "field before block",
			limits: func() Limits {
				l := allDisabled()
				l.MaxHeaderFieldBytes = 8
				l.MaxHeaderBytes = 8
				return l
			}(),
			want: CodeLimitHeaderFieldBytes,
		},
		{
			name: "block before count",
			limits: func() Limits {
				l := allDisabled()
				l.MaxHeaderBytes = 8
				l.MaxHeaderCount = 1
				return l
			}(),
			want: CodeLimitHeaderBytes,
		},
		{
			name: "line before all",
			limits: func() Limits {
				l := allDisabled()
				l.MaxLineBytes = 4
				l.MaxHeaderFieldBytes = 4
				l.MaxHeaderBytes = 4
				l.MaxHeaderCount = 1
				return l
			}(),
			want: CodeLimitLineBytes,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{1, 3, 4096} {
				session, _, err := Parse(&chunkReader{s: text, n: chunk}, &ParseOptions{Limits: tc.limits})
				if !IsFatal(err) {
					t.Fatalf("chunks of %d: Parse() = %v, want a refusal", chunk, err)
				}
				if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{tc.want}) {
					t.Errorf("chunks of %d: Diagnostics() = %v, want %v", chunk, got, []Code{tc.want})
				}
			}
		})
	}
}

// TestParse_headerLimits_countChargesEmptyNames pins the evasion this budget has
// to be immune to. A field whose name is empty is skipped when the block is
// parsed, so a budget that counted the fields a block ended up with would admit
// any number of them.
func TestParse_headerLimits_countChargesEmptyNames(t *testing.T) {
	// Two real fields and eight nameless ones. The block ends up with two
	// fields; it was read as ten candidates.
	var b strings.Builder
	b.WriteString("Content-Type: text/plain\r\n")
	for i := 0; i < 8; i++ {
		b.WriteString(": nothing\r\n")
	}
	b.WriteString("X-Real: yes\r\n\r\nbody\r\n")
	text := b.String()

	limits := allDisabled()
	limits.MaxHeaderCount = 10
	if _, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits}); err != nil || e == nil {
		t.Fatalf("Parse() at exactly ten candidates = (%v, %v), want the block read", e, err)
	}

	limits.MaxHeaderCount = 9
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
	if e != nil {
		t.Errorf("Parse() = %v, want no entity", e)
	}
	if !IsFatal(err) || !errors.Is(err, textproto.ErrTooManyHeaderFields) {
		t.Fatalf("Parse() = %v, want the field count refusal", err)
	}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitHeaderCount}) {
		t.Errorf("Diagnostics() = %v, want the count budget", got)
	}

	// And the budget is not evaded by the block ending up with two fields: a
	// budget of three, which every surviving field would fit in, still refuses.
	limits.MaxHeaderCount = 3
	if _, _, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits}); !errors.Is(err, textproto.ErrTooManyHeaderFields) {
		t.Errorf("Parse() = %v, want nameless candidates to be charged", err)
	}
}

// TestParse_headerLimits_countIsPerFieldNotPerLine pins the other half of what a
// logical field is: a field folded across many lines is one candidate, not one
// per line.
func TestParse_headerLimits_countIsPerFieldNotPerLine(t *testing.T) {
	text := "Content-Type: text/plain\r\n" +
		"X-Folded: a\r\n b\r\n c\r\n d\r\n e\r\n f\r\n g\r\n" +
		"\r\nbody\r\n"

	limits := allDisabled()
	limits.MaxHeaderCount = 2
	if _, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits}); err != nil || e == nil {
		t.Fatalf("Parse() = (%v, %v), want two fields to fit a budget of two however many lines they span", e, err)
	}

	limits.MaxHeaderCount = 1
	if _, _, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits}); !errors.Is(err, textproto.ErrTooManyHeaderFields) {
		t.Errorf("Parse() = %v, want the second field to be charged", err)
	}
}

// TestParse_headerLimits_lineBytesExcludesTerminator pins exactly what a
// physical line's budget counts, at each line ending and where the input ends
// without one. A line of n bytes fits a budget of n whether it ends with CRLF,
// with a bare LF, or with the end of the input.
func TestParse_headerLimits_lineBytesExcludesTerminator(t *testing.T) {
	const value = "X-Line: aaaaaaaaaaaaaaaaaaaa"
	const n = int64(len(value))

	for _, tc := range []struct {
		name string
		text string
	}{
		{"crlf", "Content-Type: text/plain\r\n" + value + "\r\n\r\nbody\r\n"},
		{"lf", "Content-Type: text/plain\n" + value + "\n\nbody\n"},
		{"mixed", "Content-Type: text/plain\r\n" + value + "\n\r\nbody\r\n"},
		{"mixed the other way", "Content-Type: text/plain\n" + value + "\r\n\nbody\n"},
		{"no terminator at all", "Content-Type: text/plain\r\n" + value},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, delta := range []int64{-1, 0, 1} {
				limits := allDisabled()
				limits.MaxLineBytes = n + delta

				_, _, err := Parse(strings.NewReader(tc.text), &ParseOptions{Limits: limits})
				refused := errors.Is(err, textproto.ErrLineTooBig)
				if want := delta < 0; refused != want {
					t.Errorf("limit%+d: refused = %v, want %v (err = %v)", delta, refused, want, err)
				}
			}
		})
	}
}

// TestParse_headerLimits_chunkInvariance pins that the same input decides the
// same way however it arrives — one byte at a time, in chunks that fall inside
// lines, across them and exactly on a CRLF, in randomly sized pieces, and from a
// reader that reports the end of the input together with its last bytes.
//
// The CRLF cases are the ones with teeth. A line's budget does not count the
// terminator, and a terminator can arrive as a '\r' at the end of one read and a
// '\n' at the start of the next, so a count that resolved that wrongly would
// shift the boundary by a byte for some chunk sizes and not others.
func TestParse_headerLimits_chunkInvariance(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			for _, eol := range []string{"\r\n", "\n"} {
				for _, tc := range limitFixtures(eol, spec.extra) {
					size := spec.size(t, tc, eol)
					unit := spec.unit(t, tc, eol)

					for name, src := range chunkedSources(tc.fixture.text) {
						for _, delta := range []int64{-1, 0} {
							limit := size + delta
							session, e, err := Parse(src(), &ParseOptions{
								Limits: withLimit(func(l *Limits) { spec.set(l, limit) }),
							})
							var got treeResult
							if e != nil {
								got = readTree(e)
							}

							want := tc.all
							if delta < 0 {
								want = tc.upTo
							}
							label := fmt.Sprintf("%s/%s/%s at limit%+d", eolName(eol), tc.name, name, delta)
							if !reflect.DeepEqual(got.bodies, want) {
								t.Errorf("%s: bodies = %v, want %v", label, got.bodies, want)
							}

							stopped := err
							if stopped == nil {
								stopped = got.err
							}
							if delta == 0 {
								if stopped != nil {
									t.Errorf("%s: %v", label, stopped)
								}
								if d := session.Diagnostics(); d != nil {
									t.Errorf("%s: Diagnostics() = %v, want none", label, codesOfDiags(d))
								}
								continue
							}
							if !IsFatal(stopped) {
								t.Errorf("%s: err = %v, want a FatalError", label, stopped)
								continue
							}
							checkFieldsDiagnostic(t, session, spec.code, limit, tc.path, unit)
						}
					}
				}
			}
		})
	}
}

// TestParse_headerLimits_readAheadKeepsBodies pins that budgeting a header block
// takes nothing from what follows it. A parse under exact budgets is compared
// with an unbudgeted read of the same message, entity by entity: the same tree,
// the same positions, and the same bytes.
func TestParse_headerLimits_readAheadKeepsBodies(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			for _, tc := range limitFixtures("\r\n", spec.extra) {
				t.Run(tc.name, func(t *testing.T) {
					size := spec.size(t, tc, "\r\n")

					unbudgeted, err := Read(strings.NewReader(tc.fixture.text))
					if err != nil {
						t.Fatalf("Read() = %v", err)
					}
					wantOrigins := collectOrigins(t, unbudgeted)

					for _, delta := range []int64{0, 1, 64} {
						limits := withLimit(func(l *Limits) { spec.set(l, size+delta) })

						_, e, err := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{Limits: limits})
						if err != nil {
							t.Fatalf("limit%+d: Parse() = %v", delta, err)
						}
						got := readTree(e)
						if got.err != nil {
							t.Fatalf("limit%+d: reading the tree = %v", delta, got.err)
						}
						if !reflect.DeepEqual(got.bodies, tc.all) {
							t.Errorf("limit%+d: bodies = %v, want %v", delta, got.bodies, tc.all)
						}

						_, e, err = Parse(strings.NewReader(tc.fixture.text), &ParseOptions{Limits: limits})
						if err != nil {
							t.Fatalf("limit%+d: Parse() = %v", delta, err)
						}
						if gotOrigins := collectOrigins(t, e); !reflect.DeepEqual(gotOrigins, wantOrigins) {
							t.Errorf("limit%+d: origins =\n%v\nwant\n%v", delta, gotOrigins, wantOrigins)
						}
					}
				})
			}
		})
	}
}

// TestParse_headerLimits_verbatimRoundTrip pins that a message parsed under
// budgets set to exactly what it costs still writes back out byte for byte,
// which is the invariant a DKIM signature depends on.
//
// The message is its own fixture rather than one of the placements above,
// because those carry a field with an empty name to exercise the count budget,
// and such a field has never survived a round trip: the header parser skips it,
// as it always has. That is not this item's doing and not what this test is
// about.
func TestParse_headerLimits_verbatimRoundTrip(t *testing.T) {
	const long = 200
	text := "Mime-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n" +
		"X-Folded: aaa\r\n bbb\r\n ccc\r\n" +
		"X-Long: " + strings.Repeat("x", long) + "\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain\r\n" +
		"X-Part: 1\r\n" +
		"\r\n" +
		"zero\r\n" +
		"--B--\r\n"

	// Exactly what the message costs: four fields in its largest block, and one
	// field and one line at the size of its largest.
	limits := allDisabled()
	limits.MaxHeaderCount = 4
	limits.MaxHeaderFieldBytes = int64(len("X-Long: ") + long + len("\r\n"))
	limits.MaxLineBytes = int64(len("X-Long: ") + long)

	_, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	var b bytes.Buffer
	if err := e.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo() = %v", err)
	}
	if got := b.String(); got != text {
		t.Errorf("round trip changed the message:\n%q\nwant\n%q", got, text)
	}
}

// TestParse_headerLimits_profileIndependent pins that a safety budget is not a
// matter of interpretation: every profile and every compatibility policy enforce
// it identically, because what it protects is the parser rather than the reading
// of the message.
func TestParse_headerLimits_profileIndependent(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			tc := limitFixtures("\r\n", spec.extra)[1]
			size := spec.size(t, tc, "\r\n")

			for _, profile := range []Profile{ProfileStrict, ProfileTolerant, ProfileForensic} {
				for _, compat := range []Compatibility{
					CompatibilityConservative, CompatibilityAngus,
					CompatibilityMimeKit, CompatibilityPythonEmail,
				} {
					opts := &ParseOptions{
						Profile:       profile,
						Compatibility: compat,
						Limits:        withLimit(func(l *Limits) { spec.set(l, size-1) }),
					}
					session, e, err := Parse(strings.NewReader(tc.fixture.text), opts)
					if err != nil {
						t.Fatalf("%v/%v: Parse() = %v", profile, compat, err)
					}
					got := readTree(e)
					if !IsFatal(got.err) {
						t.Errorf("%v/%v: reading the tree = %v, want the refusal", profile, compat, got.err)
					}
					if d := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(d, []Code{spec.code}) {
						t.Errorf("%v/%v: Diagnostics() = %v, want %v", profile, compat, d, spec.code)
					}

					// And the same message under a sufficient budget is accepted
					// by every profile alike.
					opts.Limits = withLimit(func(l *Limits) { spec.set(l, size) })
					_, e, err = Parse(strings.NewReader(tc.fixture.text), opts)
					if err != nil {
						t.Fatalf("%v/%v: Parse() = %v", profile, compat, err)
					}
					if got := readTree(e); got.err != nil {
						t.Errorf("%v/%v: reading the tree = %v", profile, compat, got.err)
					}
				}
			}
		})
	}
}

// TestParse_headerLimits_stickyWithoutDuplicates pins what a caller who keeps
// asking is told: the identical error every time, and one diagnostic however
// many times it asks.
func TestParse_headerLimits_stickyWithoutDuplicates(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			tc := limitFixtures("\r\n", spec.extra)[1]
			size := spec.size(t, tc, "\r\n")
			unit := spec.unit(t, tc, "\r\n")

			session, e, err := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
				Limits: withLimit(func(l *Limits) { spec.set(l, size-1) }),
			})
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
			checkFieldsFatal(t, want, spec.code, spec.sentinel, tc.path, unit)

			for i := 0; i < 3; i++ {
				p, err := mr.NextPart()
				if p != nil {
					t.Errorf("call %d after the refusal returned %v, want no part", i, p)
				}
				if err != want {
					t.Errorf("call %d after the refusal = %v, want the identical error %v", i, err, want)
				}
			}

			checkFieldsDiagnostic(t, session, spec.code, size-1, tc.path, unit)

			if first.Header.Get("Content-Type") != "text/plain" {
				t.Errorf("the part returned before the failure lost its header")
			}
		})
	}
}

// TestParse_headerLimits_sinkReceivesOnce pins that a sink is told about each
// refusal exactly once, at every depth, and that a sink which retains nothing
// still receives it.
func TestParse_headerLimits_sinkReceivesOnce(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			for _, tc := range limitFixtures("\r\n", spec.extra) {
				t.Run(tc.name, func(t *testing.T) {
					var seen []Diagnostic
					session, e, _ := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
						Limits: withLimit(func(l *Limits) { spec.set(l, spec.size(t, tc, "\r\n")-1) }),
						DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
							seen = append(seen, d)
							return nil
						}),
						DiscardDeliveredDiagnostics: true,
					})
					if e != nil {
						readTree(e)
					}

					if len(seen) != 1 {
						t.Fatalf("the sink received %v, want exactly one diagnostic", seen)
					}
					if seen[0].Code != spec.code {
						t.Errorf("Code = %q, want %q", seen[0].Code, spec.code)
					}
					if want := LocationAt(spec.unit(t, tc, "\r\n")); seen[0].Location != want {
						t.Errorf("Location = %v, want %v", seen[0].Location, want)
					}
					if got := session.Diagnostics(); got != nil {
						t.Errorf("Diagnostics() = %v, want none retained", codesOfDiags(got))
					}
				})
			}
		})
	}
}

// TestParse_headerLimits_sinkStopWins pins what a caller is told when its own
// sink refuses to continue while it is being told about a refused header: the
// stop it asked for, which belongs to the whole parse, rather than this scope's
// own failure.
func TestParse_headerLimits_sinkStopWins(t *testing.T) {
	stop := errors.New("stop here")

	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			for _, tc := range limitFixtures("\r\n", spec.extra) {
				t.Run(tc.name, func(t *testing.T) {
					var seen []Code
					_, e, err := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
						Limits: withLimit(func(l *Limits) { spec.set(l, spec.size(t, tc, "\r\n")-1) }),
						DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
							seen = append(seen, d.Code)
							return stop
						}),
					})

					stopped := err
					if e != nil {
						stopped = readTree(e).err
					}
					if !errors.Is(stopped, stop) {
						t.Fatalf("err = %v, want the sink's own error", stopped)
					}
					if !IsFatal(stopped) {
						t.Errorf("err = %v, want a FatalError", stopped)
					}
					var fatal FatalError
					if errors.As(stopped, &fatal) {
						if _, ok := fatal.Diagnostic(); ok {
							t.Errorf("the sink's stop carries a diagnostic, want none")
						}
					}
					if !reflect.DeepEqual(seen, []Code{spec.code}) {
						t.Errorf("the sink received %v, want exactly %v", seen, spec.code)
					}
				})
			}
		})
	}
}

// TestParse_headerLimits_legacyPathsUnbudgeted pins the compatibility half of
// this item: Read and ReadWithOptions resolve no configuration, so none of these
// three budgets reaches them at any depth, and a message every one of them would
// refuse is read exactly as it always was.
func TestParse_headerLimits_legacyPathsUnbudgeted(t *testing.T) {
	for _, spec := range headerBudgetSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			for _, tc := range limitFixtures("\r\n", spec.extra) {
				t.Run(tc.name, func(t *testing.T) {
					for _, read := range []struct {
						name string
						fn   func(io.Reader) (*Entity, error)
					}{
						{"Read", func(r io.Reader) (*Entity, error) { return Read(r) }},
						{"ReadWithOptions", func(r io.Reader) (*Entity, error) {
							return ReadWithOptions(r, &ReadOptions{MaxHeaderBytes: 1 << 20})
						}},
					} {
						e, err := read.fn(strings.NewReader(tc.fixture.text))
						if err != nil {
							t.Fatalf("%s() = %v", read.name, err)
						}
						got := readTree(e)
						if got.err != nil {
							t.Fatalf("%s(): reading the tree = %v", read.name, got.err)
						}
						if !reflect.DeepEqual(got.bodies, tc.all) {
							t.Errorf("%s(): bodies = %v, want %v", read.name, got.bodies, tc.all)
						}
					}
				})
			}
		})
	}
}

// TestReadHeader_unbudgeted pins the same thing one layer down: the textproto
// entry point that takes no budgets still applies none, so a header block that
// every budget would refuse is read whole.
func TestReadHeader_unbudgeted(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "X-Pad-%d: %s\r\n", i, strings.Repeat("x", 500))
	}
	b.WriteString("\r\n")

	h, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(b.String())))
	if err != nil {
		t.Fatalf("ReadHeader() = %v", err)
	}
	if h.Len() != 200 {
		t.Errorf("ReadHeader() read %d fields, want 200", h.Len())
	}
}

// TestParse_headerLimits_forgedRefusalIsNotDiagnosed pins that each of these
// budgets is diagnosed only when this parser's own budget refused a block — not
// when an input presents something that looks like that refusal.
//
// The sentinels are public and the error type carrying a refusal is exported, so
// an io.Reader under this parser can hand back any of them, including a typed
// nil that a classification by errors.As would report as found and leave to be
// dereferenced. Classifying by shape would turn any of these into a limit.*
// diagnostic and a FatalError on a message nowhere near its budgets, or into a
// panic inside the parser.
func TestParse_headerLimits_forgedRefusalIsNotDiagnosed(t *testing.T) {
	f := limitFixtures("\r\n", headerBudgetSpecs()[0].extra)[1].fixture

	lookalikes := []struct {
		name string
		err  error
	}{
		{"field bytes sentinel", textproto.ErrHeaderFieldTooBig},
		{"count sentinel", textproto.ErrTooManyHeaderFields},
		{"line bytes sentinel", textproto.ErrLineTooBig},
		{"wrapped line sentinel", fmt.Errorf("reading: %w", textproto.ErrLineTooBig)},
		{"joined sentinels", errors.Join(textproto.ErrLineTooBig, textproto.ErrTooManyHeaderFields)},
		{"match-all Is", matchAllIsError{}},
		{"forged header limit", &textproto.HeaderLimitError{}},
		{"typed nil header limit", (*textproto.HeaderLimitError)(nil)},
		{"forged line limit", &textproto.LineLimitError{}},
		{"typed nil line limit", (*textproto.LineLimitError)(nil)},
		{"wrapped typed nil line limit", fmt.Errorf("outer: %w", (*textproto.LineLimitError)(nil))},
	}

	for _, where := range []struct {
		name string
		cut  int
	}{
		{"in the message header", strings.Index(f.text, "\r\n") + len("\r\n")},
		{"in a part header", int(f.offset(t, "1.header"))},
	} {
		for _, tc := range lookalikes {
			t.Run(where.name+"/"+tc.name, func(t *testing.T) {
				src := &errorAfterReader{s: f.text, n: where.cut, err: tc.err}

				// Budgets far larger than anything in the message, so nothing
				// here can be a budget's doing.
				session, e, err := Parse(src, &ParseOptions{Limits: Limits{
					MaxHeaderBytes:      1 << 20,
					MaxHeaderFieldBytes: 1 << 20,
					MaxHeaderCount:      1 << 20,
					MaxLineBytes:        1 << 20,
				}})
				if e != nil {
					err = readTree(e).err
				}
				if err == nil {
					t.Fatal("want the input's failure")
				}
				// Formatting what arrived must not be what brings the parser
				// down either.
				_ = err.Error()

				if parserProducedFatal(err) {
					t.Errorf("err = %v, want an ordinary input error, not a parse failure", err)
				}
				if d := session.Diagnostics(); d != nil {
					t.Errorf("Diagnostics() = %v, want none: no budget was reached", codesOfDiags(d))
				}
			})
		}
	}
}

// TestParse_headerLimits_emptyBlockIsNotAField pins, at the parse layer, that a
// header block with no field in it costs no field budget.
//
// The empty line that ends a block is a line of the block, not a logical field:
// MaxHeaderBytes counts its bytes and MaxLineBytes sees a line of no content,
// but MaxHeaderFieldBytes counts a field, and there is none here. So an entity
// whose header is empty must parse under a field budget it contains no field to
// spend, keep its body byte for byte, and record nothing.
//
// Each placement is held to the tightest budget it can be held to, which is not
// the same number everywhere and is why the budget is computed rather than
// written down. A message whose root header is empty admits a budget of 1, and
// that is the reported defect exactly: a CRLF block end charged to a field
// overruns it by the two bytes it is not supposed to cost. A message with parts
// cannot go that low, because its root has to carry a Content-Type to be
// multipart at all, so those cases sit at the exact wire size of that field —
// enough to pin that the empty child header spends none of it, while the
// sub-two-byte boundary is pinned where it can be, on the block itself and in
// textproto's own TestReadHeaderLimits_blockEndIsNotAField.
func TestParse_headerLimits_emptyBlockIsNotAField(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		outer := "Content-Type: multipart/mixed; boundary=B" + eol
		inner := "Content-Type: multipart/mixed; boundary=C" + eol

		for _, tc := range []struct {
			name   string
			text   string
			bodies []string
			// limit is the field budget this placement is read under: the
			// smallest one its own real fields leave available.
			limit int64
		}{
			{
				name:   "root with an empty header",
				text:   eol + "body",
				bodies: []string{"body"},
				limit:  1,
			},
			{
				name: "a multipart child with an empty header",
				text: outer + eol +
					"--B" + eol + eol + "first" + eol +
					"--B" + eol + "X-A: 1" + eol + eol + "second" + eol +
					"--B--" + eol,
				bodies: []string{"first", "second"},
				limit:  int64(len(outer)),
			},
			{
				name: "a deep child with an empty header",
				text: outer + eol +
					"--B" + eol + inner + eol +
					"--C" + eol + eol + "deep" + eol +
					"--C--" + eol +
					"--B--" + eol,
				bodies: []string{"deep"},
				limit:  int64(len(inner)),
			},
		} {
			t.Run(eolName(eol)+"/"+tc.name, func(t *testing.T) {
				limits := allDisabled()
				limits.MaxHeaderFieldBytes = tc.limit

				session, e, err := Parse(strings.NewReader(tc.text), &ParseOptions{Limits: limits})
				if err != nil {
					t.Fatalf("Parse() = %v, want an empty header block to cost no field budget", err)
				}
				got := readTree(e)
				if got.err != nil {
					t.Fatalf("reading the tree = %v", got.err)
				}
				if !reflect.DeepEqual(got.bodies, tc.bodies) {
					t.Errorf("bodies = %v, want %v", got.bodies, tc.bodies)
				}
				if d := session.Diagnostics(); d != nil {
					t.Errorf("Diagnostics() = %v, want none: no budget was reached", codesOfDiags(d))
				}
			})
		}
	}
}

// TestParse_headerLimits_emptyBlockPrecedence pins which budget an empty block
// is refused by when it is refused at all.
//
// An empty block has exactly one unit that can exceed anything: its own bytes.
// So MaxHeaderBytes refuses it and limit.header_bytes is recorded, and that
// stays true with the smallest possible field budget set alongside — reporting a
// field budget would be reporting a unit the block does not contain.
func TestParse_headerLimits_emptyBlockPrecedence(t *testing.T) {
	limits := allDisabled()
	limits.MaxHeaderBytes = 1
	limits.MaxHeaderFieldBytes = 1

	session, e, err := Parse(strings.NewReader("\r\nbody"), &ParseOptions{Limits: limits})
	if e != nil {
		t.Errorf("Parse() = %v, want no entity from a refused block", e)
	}
	if !IsFatal(err) {
		t.Fatalf("Parse() = %v, want a refusal", err)
	}
	if got, want := codesOfDiags(session.Diagnostics()), []Code{CodeLimitHeaderBytes}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Diagnostics() = %v, want %v: an empty block has no field to refuse", got, want)
	}
}

// TestParse_headerLimits_boundedWorkOnEndlessInput pins that an input that never
// ends costs bounded work and bounded memory. Each budget is reached and the
// parse stops; a budget that only decided after the block had been built would
// read the endless input to the end of memory instead.
func TestParse_headerLimits_boundedWorkOnEndlessInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		line   string
		limits func() Limits
		want   Code
	}{
		{
			name: "endless fields",
			line: "X-Pad: filler\r\n",
			limits: func() Limits {
				l := allDisabled()
				l.MaxHeaderCount = 64
				return l
			},
			want: CodeLimitHeaderCount,
		},
		{
			name: "one endless line",
			line: strings.Repeat("x", 4096),
			limits: func() Limits {
				l := allDisabled()
				l.MaxLineBytes = 1 << 16
				return l
			},
			want: CodeLimitLineBytes,
		},
		{
			name: "one endless folded field",
			line: " continuation\r\n",
			limits: func() Limits {
				l := allDisabled()
				l.MaxHeaderFieldBytes = 1 << 16
				return l
			},
			want: CodeLimitHeaderFieldBytes,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &endlessReader{prefix: "X-First: a\r\n", line: tc.line, cap: endlessCap}

			session, e, err := Parse(src, &ParseOptions{Limits: tc.limits()})
			if e != nil {
				t.Errorf("Parse() = %v, want no entity from a refused header", e)
			}
			if !IsFatal(err) {
				t.Fatalf("Parse() = %v, want a refusal", err)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{tc.want}) {
				t.Fatalf("Diagnostics() = %v, want %v", got, []Code{tc.want})
			}
			// The parse stopped near the budget rather than near the reader's
			// own ceiling, which is what says the work was bounded by the budget.
			if src.n > endlessBudgetHeadroom {
				t.Errorf("read %d bytes of an endless input, want the budget to stop it far sooner", src.n)
			}
		})
	}
}

// The ceilings every endless-input test runs under.
//
// A test of a budget must not need an enormous input to prove the budget works,
// and must not cost an enormous amount when it does not. The budgets these tests
// set are at most 64 KiB, so a parse that honors one stops within a buffer of it;
// endlessBudgetHeadroom is four times that, which is the threshold that actually
// fails the test, and endlessCap is sixteen times it again, which is only the
// safety net.
//
// The cap is what keeps a regression a failing test rather than an exhausted
// machine. An unenforced budget reads its input to the end, and the header
// reader's per-field overhead is many times the wire bytes it came from, so a cap
// measured in tens of MiB is a cap measured in GiB of retained fields — enough on
// a developer's machine for the process, the test run, and the terminal around it
// to be killed together, which reports nothing about which budget failed. A cap
// of 1 MiB bounds that to something a test binary absorbs while leaving the
// assertion below, at a sixteenth of it, the thing that reports the defect.
const (
	endlessCap            = 1 << 20
	endlessBudgetHeadroom = 256 << 10
)

// endlessReader delivers prefix once and then repeats line for ever, up to a cap
// that keeps a failing test from running out of memory rather than failing.
type endlessReader struct {
	prefix string
	line   string
	cap    int64

	n   int64
	pos int
}

func (r *endlessReader) Read(p []byte) (int, error) {
	if r.n >= r.cap {
		return 0, io.EOF
	}
	if r.prefix != "" {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		r.n += int64(n)
		return n, nil
	}
	n := copy(p, r.line[r.pos:])
	r.pos += n
	if r.pos == len(r.line) {
		r.pos = 0
	}
	r.n += int64(n)
	return n, nil
}

// TestParse_otherBudgetsStayUnenforced pins the other half of this item's scope:
// the budgets it does not implement are still not consulted.
//
// Every budget but the ones already enforced is set to one — the smallest value
// that would refuse anything at all — and a message that exceeds each of them
// many times over is parsed, walked and read without a word about any of them. A
// later item turning one of these on is expected to change this test, which is
// the point of writing it down.
func TestParse_otherBudgetsStayUnenforced(t *testing.T) {
	enforced := map[string]bool{
		"MaxHeaderBytes":       true,
		"MaxHeaderFieldBytes":  true,
		"MaxHeaderCount":       true,
		"MaxLineBytes":         true,
		"MaxParameterCount":    true,
		"MaxParameterSegments": true,
		// ROB-001-004.
		"MaxMIMEDepth": true,
		"MaxPartCount": true,
		// ROB-001-009.
		"MaxInputBytes": true,
		// ROB-001-008. The fixture records no diagnostic, so pinching it to 1
		// would pass without saying anything; it is enforced and listed here.
		"MaxDiagnosticCount": true,
	}

	limits := DefaultLimits()
	var pinched []string
	for _, f := range limitFields {
		if enforced[f.name] {
			continue
		}
		*f.get(&limits) = 1
		pinched = append(pinched, f.name)
	}
	if len(pinched) == 0 {
		t.Fatal("every budget is enforced; this test has nothing to say")
	}

	// A message that exceeds each of them: deeper than one container, more than one part, a preamble and an epilogue, and
	// bodies of more than one byte, encoded and decoded.
	text := "Content-Type: multipart/mixed; boundary=OUTER\r\n\r\n" +
		"a preamble\r\n" +
		"--OUTER\r\n" +
		"Content-Type: multipart/alternative; boundary=INNER\r\n\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/plain; charset=us-ascii\r\n\r\n" +
		"deep zero\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"ZGVlcCBvbmU=\r\n" +
		"--INNER--\r\n" +
		"--OUTER\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"second\r\n" +
		"--OUTER--\r\n" +
		"an epilogue\r\n"

	session, got, err := parseUnder(t, text, limits)
	if err != nil {
		t.Fatalf("Parse() = %v, want none of %v to be enforced", err, pinched)
	}
	if got.err != nil {
		t.Fatalf("reading the tree = %v, want none of %v to be enforced", got.err, pinched)
	}
	want := []string{"deep zero", "deep one", "second"}
	if !reflect.DeepEqual(got.bodies, want) {
		t.Errorf("bodies = %v, want %v", got.bodies, want)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none: %v are not enforced yet", codesOfDiags(d), pinched)
	}
}

// parseUnder parses under limits and reads everything it can.
func parseUnder(t *testing.T, text string, limits Limits) (*ParseSession, treeResult, error) {
	t.Helper()

	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
	if e == nil {
		return session, treeResult{}, err
	}
	return session, readTree(e), err
}

// chunkedSources returns the ways the same input is delivered: whole, one byte
// at a time, in fixed chunks that fall inside lines and across them, in
// deterministic random pieces, and with the end of the input reported together
// with the last bytes.
func chunkedSources(text string) map[string]func() io.Reader {
	sources := map[string]func() io.Reader{
		"whole":         func() io.Reader { return strings.NewReader(text) },
		"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(text)) },
	}
	for _, n := range []int{1, 2, 3, 7, 64, 4095, 4096} {
		sources[fmt.Sprintf("chunks of %d", n)] = func() io.Reader {
			return &chunkReader{s: text, n: n}
		}
		sources[fmt.Sprintf("chunks of %d with eof", n)] = func() io.Reader {
			return iotest.DataErrReader(&chunkReader{s: text, n: n})
		}
	}
	for _, seed := range []int64{1, 2, 3} {
		sources[fmt.Sprintf("random chunks seed %d", seed)] = func() io.Reader {
			return newRandomChunkReader(text, 17, seed)
		}
	}
	return sources
}

// eolName names a line ending for a subtest.
func eolName(eol string) string {
	if eol == "\r\n" {
		return "crlf"
	}
	return "lf"
}
