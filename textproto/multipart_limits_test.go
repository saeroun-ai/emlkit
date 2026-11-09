package textproto

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// HeaderLimits applied by a MultipartReader: the header budgets of each part,
// and the line budget of the framing between them.

// framingFixture is a multipart body with two long framing lines: a preamble
// line of preamble bytes before the first part, and the delimiter line that
// introduces the second part, padded with the trailing linear whitespace a
// delimiter line may carry, of delim bytes. Every other line in it is short, so
// a budget swept between the two says something about one of them and about no
// other.
func framingFixture(eol string, preamble, delim int) offsetFixture {
	return newOffsetFixture(
		[2]string{"preamble", strings.Repeat("p", preamble) + eol},
		[2]string{"", "--A" + eol},
		[2]string{"", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"", "one" + eol},
		[2]string{"delimiter", "--A" + strings.Repeat(" ", delim-len("--A")) + eol},
		[2]string{"", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"", "two" + eol},
		[2]string{"", "--A--" + eol},
	)
}

// TestMultipartReader_lineBytesBoundary sweeps the framing line budget across
// the exact length of the longest framing line, at each line ending, and pins
// where it falls and what the refusal says.
//
// The fixture's two long lines are a preamble line, which is read before any
// part, and a delimiter line, which is read after one has been returned. So the
// same sweep pins both that the budget applies before the first part and that a
// part already returned survives a refusal after it.
func TestMultipartReader_lineBytesBoundary(t *testing.T) {
	const n = 100

	for _, eol := range []string{"\r\n", "\n"} {
		t.Run(eolName(eol), func(t *testing.T) {
			f := framingFixture(eol, n, n)

			for _, delta := range []int64{-1, 0, 1} {
				t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
					limit := int64(n) + delta

					r := NewMultipartReader(strings.NewReader(f.body), "A")
					r.SetHeaderLimits(HeaderLimits{MaxLineBytes: limit})

					var bodies []string
					var err error
					for {
						var p *Part
						p, err = r.NextPart()
						if err != nil {
							break
						}
						bodies = append(bodies, readPartBody(t, p))
					}

					if delta >= 0 {
						if err != io.EOF {
							t.Fatalf("NextPart() = %v, want io.EOF", err)
						}
						if want := []string{"one", "two"}; !equalStrings(bodies, want) {
							t.Errorf("bodies = %v, want %v", bodies, want)
						}
						return
					}

					// The preamble line is the first framing line read, so the
					// refusal happens before any part is returned.
					if len(bodies) != 0 {
						t.Errorf("bodies = %v, want none: the preamble line was refused first", bodies)
					}
					limitErr, ok := LineLimit(err)
					if !ok {
						t.Fatalf("NextPart() = %v, want a line budget refusal", err)
					}
					if limitErr.Limit() != limit {
						t.Errorf("Limit() = %d, want %d", limitErr.Limit(), limit)
					}
					if want := f.at["preamble"]; limitErr.Offset() != want {
						t.Errorf("Offset() = %d, want %d: the first byte of the refused line", limitErr.Offset(), want)
					}
					if !errors.Is(err, ErrLineTooBig) {
						t.Errorf("err = %v, want it to wrap ErrLineTooBig", err)
					}
				})
			}
		})
	}
}

// TestMultipartReader_lineBytesAfterAPart pins that a framing line refused after
// a part has been returned leaves that part valid, ends the iteration, and
// reports io.EOF from then on rather than trying to resynchronize to a delimiter
// it never found.
func TestMultipartReader_lineBytesAfterAPart(t *testing.T) {
	const eol = "\r\n"
	const preamble = 100
	const delimiter = preamble + 10

	// A budget that admits the preamble exactly and refuses the delimiter that
	// introduces the second part, which is longer.
	f := framingFixture(eol, preamble, delimiter)

	r := NewMultipartReader(strings.NewReader(f.body), "A")
	r.SetHeaderLimits(HeaderLimits{MaxLineBytes: preamble})

	p, err := r.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v, want the first part", err)
	}
	if got := readPartBody(t, p); got != "one" {
		t.Errorf("first part = %q, want %q", got, "one")
	}

	_, err = r.NextPart()
	limitErr, ok := LineLimit(err)
	if !ok {
		t.Fatalf("NextPart() = %v, want a line budget refusal", err)
	}
	if want := f.at["delimiter"]; limitErr.Offset() != want {
		t.Errorf("Offset() = %d, want %d", limitErr.Offset(), want)
	}

	// The iteration is over, and stays over.
	for i := 0; i < 3; i++ {
		if p, err := r.NextPart(); p != nil || err != io.EOF {
			t.Errorf("call %d after the refusal = (%v, %v), want (nil, io.EOF)", i, p, err)
		}
	}
}

// TestMultipartReader_lineBytesChunkInvariance pins that a framing line decides
// the same way however the input arrives, including when a CRLF is split across
// two reads.
func TestMultipartReader_lineBytesChunkInvariance(t *testing.T) {
	const n = 100

	for _, eol := range []string{"\r\n", "\n"} {
		f := framingFixture(eol, n, n)

		sources := map[string]func() io.Reader{
			"whole":         func() io.Reader { return strings.NewReader(f.body) },
			"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(f.body)) },
		}
		for _, k := range []int{1, 2, 3, 7, 64, 4095, 4096} {
			sources[fmt.Sprintf("chunks of %d", k)] = func() io.Reader {
				return &chunkReader{s: f.body, n: k}
			}
		}
		for _, seed := range []int64{1, 2, 3} {
			sources[fmt.Sprintf("random seed %d", seed)] = func() io.Reader {
				return newRandomChunkReader(f.body, 11, seed)
			}
		}

		for name, src := range sources {
			for _, delta := range []int64{-1, 0} {
				r := NewMultipartReader(src(), "A")
				r.SetHeaderLimits(HeaderLimits{MaxLineBytes: int64(n) + delta})

				var err error
				for {
					var p *Part
					p, err = r.NextPart()
					if err != nil {
						break
					}
					readPartBody(t, p)
				}

				_, refused := LineLimit(err)
				if want := delta < 0; refused != want {
					t.Errorf("%s/%s at limit%+d: refused = %v, want %v (err = %v)",
						eolName(eol), name, delta, refused, want, err)
				}
			}
		}
	}
}

// TestMultipartReader_lineBytesDoesNotBoundPartBodies pins the boundary of the
// budget's scope. A part's body is opaque content this reader streams, so a body
// line far past the budget is not refused, while the framing around it is
// bounded as usual.
func TestMultipartReader_lineBytesDoesNotBoundPartBodies(t *testing.T) {
	long := strings.Repeat("z", 20000)
	body := "--A\r\nContent-Type: text/plain\r\n\r\n" +
		long + "\r\n" + long + "\r\n" +
		"--A--\r\n"

	r := NewMultipartReader(strings.NewReader(body), "A")
	r.SetHeaderLimits(HeaderLimits{MaxLineBytes: 64})

	p, err := r.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v", err)
	}
	got, err := io.ReadAll(p)
	if err != nil {
		t.Fatalf("reading the part body = %v", err)
	}
	if !strings.Contains(string(got), long) {
		t.Errorf("the part body was not read whole")
	}
	if _, err := r.NextPart(); err != io.EOF {
		t.Errorf("NextPart() = %v, want io.EOF", err)
	}
}

// TestMultipartReader_lineBytesUnsetKeepsLegacyReading pins what a reader with
// no line budget does, which is what every reader did before the budget existed:
// a framing line longer than its buffer fails, as it always has. A budget is
// what should decide how long a line may be, and where there is none there is
// nothing to change the long-standing answer to.
func TestMultipartReader_lineBytesUnsetKeepsLegacyReading(t *testing.T) {
	// A preamble line longer than the reader's buffer.
	body := strings.Repeat("p", peekBufferSize+10) + "\r\n" +
		"--A\r\nContent-Type: text/plain\r\n\r\none\r\n--A--\r\n"

	for _, tc := range []struct {
		name   string
		limits *HeaderLimits
	}{
		{"never configured", nil},
		{"configured with no line budget", &HeaderLimits{MaxBytes: 1 << 20}},
		{"line budget disabled", &HeaderLimits{MaxLineBytes: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewMultipartReader(strings.NewReader(body), "A")
			if tc.limits != nil {
				r.SetHeaderLimits(*tc.limits)
			}

			_, err := r.NextPart()
			if err == nil {
				t.Fatalf("NextPart() = nil, want the long-standing buffer failure")
			}
			if _, refused := LineLimit(err); refused {
				t.Errorf("NextPart() = %v, want an ordinary failure rather than a budget refusal", err)
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				t.Errorf("NextPart() = %v, want the buffer failure it always returned", err)
			}
		})
	}

	// And with a budget, the same line is read and bounded by the budget alone.
	r := NewMultipartReader(strings.NewReader(body), "A")
	r.SetHeaderLimits(HeaderLimits{MaxLineBytes: peekBufferSize + 100})
	p, err := r.NextPart()
	if err != nil {
		t.Fatalf("NextPart() under a sufficient line budget = %v", err)
	}
	if got := readPartBody(t, p); got != "one" {
		t.Errorf("part = %q, want %q", got, "one")
	}
}

// TestMultipartReader_lineBytesBoundsWorkWhileReading pins that a framing line
// that never ends costs bounded work: the budget stops it, rather than the line
// being accumulated to the end of an endless input.
func TestMultipartReader_lineBytesBoundsWorkWhileReading(t *testing.T) {
	src := &countingReader{r: &endlessLineReader{line: strings.Repeat("p", 997)}}

	r := NewMultipartReader(src, "A")
	r.SetHeaderLimits(HeaderLimits{MaxLineBytes: 1 << 16})

	if _, err := r.NextPart(); !errors.Is(err, ErrLineTooBig) {
		t.Fatalf("NextPart() = %v, want the line budget refusal", err)
	}
	if src.n > 1<<20 {
		t.Errorf("read %d bytes of an endless line, want the budget to stop it far sooner", src.n)
	}
}

// TestMultipartReader_partHeaderLimits pins that every header budget reaches a
// part's header block, that each names the budget it refused and where, and that
// a refused block yields no part.
func TestMultipartReader_partHeaderLimits(t *testing.T) {
	// The middle part carries a folded field that is by far the largest thing
	// in any header block of the body, so each budget can be swept across it.
	folded := "X-Folded: " + strings.Repeat("a", 60) + "\r\n" +
		" " + strings.Repeat("b", 60) + "\r\n"
	f := newOffsetFixture(
		[2]string{"", "--A\r\n"},
		[2]string{"p0.header", "Content-Type: text/plain\r\n\r\n"},
		[2]string{"p0.body", "one\r\n"},
		[2]string{"", "--A\r\n"},
		[2]string{"p1.header", "Content-Type: text/plain\r\n"},
		[2]string{"p1.field", folded},
		[2]string{"p1.end", "\r\n"},
		[2]string{"p1.body", "two\r\n"},
		[2]string{"", "--A--\r\n"},
	)

	for _, tc := range []struct {
		name string
		kind HeaderLimitKind
		size int64
		unit int64
		set  func(*HeaderLimits, int64)
	}{
		{
			name: "field bytes",
			kind: HeaderLimitFieldBytes,
			size: f.at["p1.end"] - f.at["p1.field"],
			unit: f.at["p1.field"],
			set:  func(l *HeaderLimits, v int64) { l.MaxFieldBytes = v },
		},
		{
			name: "line bytes",
			kind: HeaderLimitLineBytes,
			size: int64(len("X-Folded: ") + 60),
			unit: f.at["p1.field"],
			set:  func(l *HeaderLimits, v int64) { l.MaxLineBytes = v },
		},
		{
			name: "count",
			kind: HeaderLimitCount,
			size: 2, // Content-Type and the folded field
			unit: f.at["p1.field"],
			set:  func(l *HeaderLimits, v int64) { l.MaxCount = v },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, delta := range []int64{-1, 0, 1} {
				var limits HeaderLimits
				tc.set(&limits, tc.size+delta)

				r := NewMultipartReader(strings.NewReader(f.body), "A")
				r.SetHeaderLimits(limits)

				first, err := r.NextPart()
				if err != nil {
					t.Fatalf("limit%+d: NextPart() = %v, want the first part", delta, err)
				}
				if got := readPartBody(t, first); got != "one" {
					t.Errorf("limit%+d: first part = %q, want %q", delta, got, "one")
				}

				second, err := r.NextPart()
				if delta >= 0 {
					if err != nil {
						t.Fatalf("limit%+d: NextPart() = %v, want the second part", delta, err)
					}
					if got := readPartBody(t, second); got != "two" {
						t.Errorf("limit%+d: second part = %q, want %q", delta, got, "two")
					}
					continue
				}

				if second != nil {
					t.Errorf("limit%+d: NextPart() returned a part from a refused header block", delta)
				}
				checkRefusal(t, err, tc.kind, tc.size+delta, f.at["p1.header"], tc.unit)

				// The iteration is over: the reader is inside a part whose
				// extent it never learned.
				if p, err := r.NextPart(); p != nil || err != io.EOF {
					t.Errorf("limit%+d: after the refusal = (%v, %v), want (nil, io.EOF)", delta, p, err)
				}
			}
		})
	}
}

// TestLineLimit_rejectsForgeries pins that only a refusal this package made is
// reported as one. The type is exported, so another package can write a value of
// it — the zero value, which claims nothing — and an input can hand back a typed
// nil that errors.As would report as found and leave to be dereferenced.
func TestLineLimit_rejectsForgeries(t *testing.T) {
	var typedNil *LineLimitError

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"the sentinel itself", ErrLineTooBig},
		{"wrapped sentinel", fmt.Errorf("outer: %w", ErrLineTooBig)},
		{"forged zero value", &LineLimitError{}},
		{"wrapped forged zero value", fmt.Errorf("outer: %w", &LineLimitError{})},
		{"typed nil", typedNil},
		{"wrapped typed nil", fmt.Errorf("outer: %w", typedNil)},
		{"match-all Is", matchAllIsError{}},
		{"wrapped match-all Is", fmt.Errorf("outer: %w", matchAllIsError{})},
		{"joined forgeries", errors.Join(&LineLimitError{}, typedNil, ErrLineTooBig)},
		{"a header refusal is not a line refusal", newHeaderLimitError(HeaderLimitLineBytes, 1, 2, 3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit, ok := LineLimit(tc.err)
			if ok {
				t.Errorf("LineLimit(%v) = (%v, true), want no refusal", tc.err, limit)
			}
			if limit != nil {
				t.Errorf("LineLimit(%v) returned %v, want nil", tc.err, limit)
			}
		})
	}

	// A genuine refusal is still found, at the top of a chain and inside one.
	genuine := newLineLimitError(42, 7)
	for _, err := range []error{
		genuine,
		fmt.Errorf("outer: %w", genuine),
		unwrapsTo{genuine},
		errors.Join(errors.New("other"), genuine),
		multiUnwrap{[]error{nil, &LineLimitError{}, genuine}},
	} {
		limit, ok := LineLimit(err)
		if !ok {
			t.Fatalf("LineLimit(%v) found no refusal", err)
		}
		if limit.Limit() != 42 || limit.Offset() != 7 {
			t.Errorf("limit = (%d, %d), want (42, 7)", limit.Limit(), limit.Offset())
		}
	}
}

// TestLineLimit_boundedOnHostileGraphs pins that the walk terminates on an error
// graph an input shaped: a cycle, a ring, a chain deeper than the walk's bound,
// and a join wider than it. Each case must return, and none may find a refusal
// that is not there.
func TestLineLimit_boundedOnHostileGraphs(t *testing.T) {
	forged := &LineLimitError{}
	genuine := newLineLimitError(1, 2)

	ringA := &mutableWrap{}
	ringB := &mutableWrap{inner: ringA}
	ringA.inner = ringB

	forgedRingA := &mutableWrap{}
	forgedRingB := &mutableWrap{inner: forgedRingA}
	forgedRingC := &mutableWrap{inner: forgedRingB}
	forgedRingA.inner = forgedRingC

	// Every cycle is a fused fixture (see cycleFuse), so a regression fails
	// here rather than spins.
	self := &endlessChain{}
	t.Cleanup(func() {
		for _, ring := range []*mutableWrap{ringA, ringB, forgedRingA, forgedRingB, forgedRingC} {
			if ring.blown() {
				t.Error("a ring's fuse blew: LineLimit's walk is not bounded")
			}
		}
		if self.blown() {
			t.Error("a cycle's fuse blew: LineLimit's walk is not bounded")
		}
	})

	deep := error(errors.New("bottom"))
	for i := 0; i < 5000; i++ {
		deep = fmt.Errorf("layer %d: %w", i, deep)
	}
	deepWithRefusal := error(genuine)
	for i := 0; i < 5000; i++ {
		deepWithRefusal = fmt.Errorf("layer %d: %w", i, deepWithRefusal)
	}

	wide := make([]error, 100000)
	for i := range wide {
		wide[i] = forged
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"self cycle", self, false},
		{"two node ring", ringA, false},
		{"ring with forgeries", forgedRingA, false},
		{"chain deeper than the bound", deep, false},
		{"chain deeper than the bound with a refusal at the bottom", deepWithRefusal, false},
		{"join wider than the bound", multiUnwrap{wide}, false},
		{"join wider than the bound with a refusal first", multiUnwrap{append([]error{genuine}, wide...)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			var got bool
			go func() {
				defer close(done)
				_, got = LineLimit(tc.err)
			}()
			<-done
			if got != tc.want {
				t.Errorf("LineLimit() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLineLimitError_nilSafe pins that a typed nil of the exported type — which
// an input can present — answers every method without panicking, so that a
// caller logging what arrived is not what brings a parser down.
func TestLineLimitError_nilSafe(t *testing.T) {
	var e *LineLimitError

	if got := e.Limit(); got != 0 {
		t.Errorf("Limit() = %d, want 0", got)
	}
	if got := e.Offset(); got != 0 {
		t.Errorf("Offset() = %d, want 0", got)
	}
	if got := e.Error(); got == "" {
		t.Errorf("Error() = %q, want something readable", got)
	}
	if got := e.Unwrap(); got != ErrLineTooBig {
		t.Errorf("Unwrap() = %v, want %v", got, ErrLineTooBig)
	}
}

// TestMultipartReader_forgedLineRefusalFromInput pins that an input handing back
// something shaped like a framing line refusal is not mistaken for one — and, in
// the case of a typed nil, is not dereferenced. The reader classifies by exact
// dynamic type and by a mark it alone can set.
func TestMultipartReader_forgedLineRefusalFromInput(t *testing.T) {
	const body = "--A\r\nContent-Type: text/plain\r\n\r\none\r\n--A--\r\n"

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the sentinel itself", ErrLineTooBig},
		{"wrapped sentinel", fmt.Errorf("outer: %w", ErrLineTooBig)},
		{"forged zero value", &LineLimitError{}},
		{"typed nil", (*LineLimitError)(nil)},
		{"wrapped typed nil", fmt.Errorf("outer: %w", (*LineLimitError)(nil))},
		{"match-all Is", matchAllIsError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &errorAfterReader{s: body, n: 2, err: tc.err}
			r := NewMultipartReader(src, "A")
			r.SetHeaderLimits(HeaderLimits{MaxLineBytes: 1 << 20})

			_, err := r.NextPart()
			if err == nil {
				t.Fatal("NextPart() = nil, want the input's failure")
			}
			// Formatting what arrived must not panic either.
			_ = err.Error()

			if _, refused := LineLimit(err); refused {
				t.Errorf("NextPart() = %v, want an ordinary input failure rather than a refusal", err)
			}
		})
	}
}

// equalStrings compares two string slices.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// eolName names a line ending for a subtest.
func eolName(eol string) string {
	if eol == "\r\n" {
		return "crlf"
	}
	return "lf"
}
