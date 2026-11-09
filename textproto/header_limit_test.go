package textproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// The header byte budget at the layer that reads a header block. These tests
// pin three things about it: what it counts, that it counts the same however
// the input arrives, and that counting stops at the block rather than reaching
// into what follows it.

// budgetHeader is a header block with a continuation line, so that what is
// counted includes the bytes of a folded field and not only of its first line.
const budgetHeader = "Content-Type: text/plain;\r\n" +
	" charset=utf-8\r\n" +
	"Subject: a subject\r\n" +
	"\r\n"

const budgetBody = "the body, which no header budget may touch\r\n"

// readHeaderLimitFrom reads a header block from s under a budget, through a
// reader of the given buffer size, and reports the header, what remained
// unread, and the error.
func readHeaderLimitFrom(s string, maxBytes int64, bufSize int) (Header, string, error) {
	br := bufio.NewReaderSize(strings.NewReader(s), bufSize)
	h, err := ReadHeaderLimit(br, maxBytes)
	rest, _ := io.ReadAll(br)
	return h, string(rest), err
}

// TestReadHeaderLimit_boundary sweeps the budget across the size of a block and
// pins where it falls: a block of exactly the budget is read, and one byte more
// is refused. The block is measured by the fixture rather than counted out by
// hand, and every line ending and truncation the wire allows is measured the
// same way.
func TestReadHeaderLimit_boundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"crlf", budgetHeader},
		{"lf", strings.ReplaceAll(budgetHeader, "\r\n", "\n")},
		{"mixed endings", "A: one\r\n" + "B: two\n" + "\r\n"},
		{"one field", "A: one\r\n\r\n"},
		{"empty block", "\r\n"},
		{"long folded field", "A: one\r\n" + strings.Repeat(" continued\r\n", 40) + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size := int64(len(tc.header))

			for _, delta := range []int64{-1, 0, 1} {
				limit := size + delta
				if limit <= 0 {
					continue
				}

				h, rest, err := readHeaderLimitFrom(tc.header+budgetBody, limit, 4096)

				if delta < 0 {
					if !errors.Is(err, ErrHeaderTooBig) {
						t.Errorf("limit %d of %d bytes: err = %v, want ErrHeaderTooBig", limit, size, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("limit %d of %d bytes: err = %v, want the header to be read", limit, size, err)
				}
				// The block was read and nothing beyond it was consumed.
				if rest != budgetBody {
					t.Errorf("limit %d of %d bytes: body = %q, want %q", limit, size, rest, budgetBody)
				}
				if got := headerKeys(h); len(got) != strings.Count(tc.header, ":") {
					t.Errorf("limit %d of %d bytes: fields = %v", limit, size, got)
				}
			}
		})
	}
}

// TestReadHeaderLimit_noTerminatingBlankLine pins what is counted when the
// input ends before the empty line that would have ended the block: the bytes
// that are there, and no charge for the terminator that never arrived.
func TestReadHeaderLimit_noTerminatingBlankLine(t *testing.T) {
	const header = "A: one\r\nB: two\r\n"
	size := int64(len(header))

	for _, tc := range []struct {
		limit   int64
		wantErr bool
	}{
		{size - 1, true},
		{size, false},
		{size + 1, false},
	} {
		h, rest, err := readHeaderLimitFrom(header, tc.limit, 4096)
		if (err != nil) != tc.wantErr {
			t.Errorf("limit %d of %d bytes: err = %v, want an error: %v", tc.limit, size, err, tc.wantErr)
		}
		if tc.wantErr {
			if !errors.Is(err, ErrHeaderTooBig) {
				t.Errorf("limit %d: err = %v, want ErrHeaderTooBig", tc.limit, err)
			}
			continue
		}
		if rest != "" {
			t.Errorf("limit %d: %q left unread", tc.limit, rest)
		}
		if got := len(headerKeys(h)); got != 2 {
			t.Errorf("limit %d: %d fields, want 2", tc.limit, got)
		}
	}
}

// TestReadHeaderLimit_chunkInvariance pins that the decision does not depend on
// how the input arrives: neither on the size of the reader's buffer, which is
// what the block is read out of, nor on how the source hands out its bytes,
// including a source that reports the end of the input together with its last
// bytes.
func TestReadHeaderLimit_chunkInvariance(t *testing.T) {
	size := int64(len(budgetHeader))
	input := budgetHeader + budgetBody

	sources := map[string]func() io.Reader{
		"whole":    func() io.Reader { return strings.NewReader(input) },
		"one byte": func() io.Reader { return iotest.OneByteReader(strings.NewReader(input)) },
		"data with eof": func() io.Reader {
			return iotest.DataErrReader(strings.NewReader(input))
		},
		"one byte with eof": func() io.Reader {
			return iotest.DataErrReader(iotest.OneByteReader(strings.NewReader(input)))
		},
	}

	// A 16 byte buffer is smaller than every line of the fixture, so the block
	// is read in pieces and a "\r\n" lands across a buffer boundary.
	for _, bufSize := range []int{16, 17, 32, 64, 4096} {
		for name, src := range sources {
			for _, delta := range []int64{-1, 0, 1} {
				br := bufio.NewReaderSize(src(), bufSize)
				h, err := ReadHeaderLimit(br, size+delta)
				rest, _ := io.ReadAll(br)

				if delta < 0 {
					if !errors.Is(err, ErrHeaderTooBig) {
						t.Errorf("buffer %d from %s at limit-1: err = %v, want ErrHeaderTooBig", bufSize, name, err)
					}
					continue
				}
				if err != nil {
					t.Errorf("buffer %d from %s at limit%+d: err = %v", bufSize, name, delta, err)
					continue
				}
				if got := string(rest); got != budgetBody {
					t.Errorf("buffer %d from %s at limit%+d: body = %q, want %q", bufSize, name, delta, got, budgetBody)
				}
				if got, want := h.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
					t.Errorf("buffer %d from %s at limit%+d: Content-Type = %q, want %q", bufSize, name, delta, got, want)
				}
			}
		}
	}
}

// TestReadHeaderLimit_unlimited pins that a budget of zero or below is no
// budget, which is what ReadHeader reads under, and that ReadHeader still does.
func TestReadHeaderLimit_unlimited(t *testing.T) {
	big := "A: " + strings.Repeat("x", 1<<20) + "\r\n\r\n"

	for _, limit := range []int64{0, -1, -1 << 40} {
		if _, _, err := readHeaderLimitFrom(big+budgetBody, limit, 4096); err != nil {
			t.Errorf("limit %d: err = %v, want no budget at all", limit, err)
		}
	}

	br := bufio.NewReader(strings.NewReader(big + budgetBody))
	if _, err := ReadHeader(br); err != nil {
		t.Errorf("ReadHeader() = %v, want no budget at all", err)
	}
}

// TestReadHeaderLimit_boundsWorkWhileReading pins that the budget is checked as
// the block is read rather than after it has been built. The source here never
// produces a line ending, so a reader that materialized the block first would
// read forever; this one must stop having read no more than the budget plus the
// one buffer it was filling.
func TestReadHeaderLimit_boundsWorkWhileReading(t *testing.T) {
	const limit = 1 << 10

	for _, bufSize := range []int{16, 4096} {
		src := &countingEndlessReader{}
		br := bufio.NewReaderSize(src, bufSize)

		if _, err := ReadHeaderLimit(br, limit); !errors.Is(err, ErrHeaderTooBig) {
			t.Fatalf("buffer %d: err = %v, want ErrHeaderTooBig", bufSize, err)
		}
		if max := int64(limit + bufSize); src.n > max {
			t.Errorf("buffer %d: read %d bytes of an endless header, want at most %d", bufSize, src.n, max)
		}
	}
}

// countingEndlessReader is a header line that never ends: it delivers 'x'
// forever and counts what it has delivered.
type countingEndlessReader struct{ n int64 }

func (r *countingEndlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.n += int64(len(p))
	return len(p), nil
}

// TestReadHeaderLimit_refusedBlockKeepsNothing pins that a refused block yields
// no header at all, rather than the fields that happened to fit before the
// budget ran out.
//
// Where in the block the budget runs out is exactly what must not matter. A
// block refused on its first field has nothing to hand back either way; one
// refused on a later field, or on a continuation of one, is the case where a
// partial header could be returned and be mistaken for the whole of it.
func TestReadHeaderLimit_refusedBlockKeepsNothing(t *testing.T) {
	const long = "Subject: " + "wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww" + "\r\n"

	for _, tc := range []struct {
		name   string
		header string
		limit  int64
	}{
		{
			name:   "on the first field",
			header: budgetHeader,
			limit:  20,
		},
		{
			name:   "on a later field",
			header: "A: one\r\n" + long + "\r\n",
			limit:  int64(len("A: one\r\n")) + 10,
		},
		{
			name:   "on a continuation of a later field",
			header: "A: one\r\nB: two\r\n" + strings.Repeat(" continued\r\n", 20) + "\r\n",
			limit:  int64(len("A: one\r\nB: two\r\n")) + 5,
		},
		{
			name:   "on the terminating empty line",
			header: "A: one\r\nB: two\r\n\r\n",
			limit:  int64(len("A: one\r\nB: two\r\n")) + 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, err := readHeaderLimitFrom(tc.header+budgetBody, tc.limit, 4096)
			if !errors.Is(err, ErrHeaderTooBig) {
				t.Fatalf("err = %v, want ErrHeaderTooBig", err)
			}
			if got := headerKeys(h); len(got) != 0 {
				t.Errorf("fields = %v, want none: a refused block is not a partial header", got)
			}
			if got := h.Get("A"); got != "" {
				t.Errorf("Get(\"A\") = %q, want nothing from a refused block", got)
			}
		})
	}
}

// errorAfterReader delivers the first n bytes of s and then fails with err,
// which is how an input that fails on its own is presented to the parser.
type errorAfterReader struct {
	s   string
	n   int
	err error
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.n <= 0 || len(r.s) == 0 {
		return 0, r.err
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	r.n -= n
	return n, nil
}

// underlyingFailures are the ways an input can fail with the very sentinel a
// budget failure carries: by returning it, and by returning something that
// wraps it. Neither one is a budget being reached, and neither may be reported
// as one.
var underlyingFailures = []struct {
	name string
	err  error
}{
	{"direct", ErrHeaderTooBig},
	{"wrapped", fmt.Errorf("reading the message: %w", ErrHeaderTooBig)},
	{"twice wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", ErrHeaderTooBig))},
}

// TestReadHeaderLimit_underlyingSentinelIsNotABudgetFailure pins the difference
// between the two things ErrHeaderTooBig can mean. A budget this package
// enforced produces a *HeaderLimitError; an input that failed with the same
// sentinel produces the input's own error, keeps the partial-result behavior
// every other read failure has, and is not dressed up as a budget that was never
// reached.
func TestReadHeaderLimit_underlyingSentinelIsNotABudgetFailure(t *testing.T) {
	const header = "A: one\r\nB: " + "twotwotwotwotwotwotwotwo" + "\r\n\r\n"

	for _, tc := range underlyingFailures {
		t.Run(tc.name, func(t *testing.T) {
			// A budget far larger than the block, so nothing here can be the
			// budget's doing.
			src := &errorAfterReader{s: header, n: len("A: one\r\n") + 4, err: tc.err}
			br := bufio.NewReaderSize(src, 4096)

			h, err := ReadHeaderLimit(br, 1<<20)

			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want the input's own failure", err)
			}
			if _, refused := HeaderLimit(err); refused {
				t.Errorf("err = %v, want no refusal: no budget was reached", err)
			}
			// Ordinary partial-result behavior is untouched: what was read
			// before the input failed is still returned.
			if got := h.Get("A"); got != "one" {
				t.Errorf("Get(\"A\") = %q, want %q: a read failure is not a refused block", got, "one")
			}
		})
	}

	// The control: the same reader, with a budget the block really does exceed,
	// is still classified as the budget failure it is.
	src := &errorAfterReader{s: header, n: len(header), err: io.EOF}
	br := bufio.NewReaderSize(src, 4096)

	if _, err := ReadHeaderLimit(br, 4); !isRefusal(err) {
		t.Errorf("err = %v, want a genuine refusal", err)
	}
}

// TestMultipartReader_underlyingSentinelIsNotABudgetFailure pins the same
// distinction where a part header is read, including that an input failure does
// not end the iteration the way a refused block does.
func TestMultipartReader_underlyingSentinelIsNotABudgetFailure(t *testing.T) {
	f := budgetPartsFixture()
	cut := int(f.at["p0.body"]) - 4 // inside the first part's header block

	for _, tc := range underlyingFailures {
		t.Run(tc.name, func(t *testing.T) {
			r := NewMultipartReader(&errorAfterReader{s: f.body, n: cut, err: tc.err}, "A")
			r.SetMaxHeaderBytes(1 << 20)

			_, err := r.NextPart()
			if !errors.Is(err, tc.err) {
				t.Fatalf("NextPart() = %v, want the input's own failure", err)
			}
			if _, refused := HeaderLimit(err); refused {
				t.Errorf("NextPart() = %v, want no refusal: no budget was reached", err)
			}
			// A refused block ends the iteration; a read failure does not, so
			// the caller is not told the message simply ended.
			if _, err := r.NextPart(); err == io.EOF {
				t.Errorf("NextPart() after an input failure = io.EOF, want the failure to stand")
			}
		})
	}
}

// adversarialFailures are the errors an input can fail with that are built to
// be mistaken for a header budget refusal. None of them is one: none was
// produced by a budget in this package refusing bytes.
//
// The one form absent here is a genuine refusal captured from this package and
// handed back as an input failure. Nothing distinguishes a value from itself,
// and HeaderLimitError documents that as the boundary rather than pretending to
// hold it.
var adversarialFailures = []struct {
	name string
	err  error
}{
	// Answers yes to every errors.Is question, including one about a target it
	// could not have obtained. This is why classification is by identity and
	// type rather than by errors.Is.
	{"match-all Is", matchAllIsError{}},
	{"wrapped match-all Is", fmt.Errorf("outer: %w", matchAllIsError{})},

	// The only HeaderLimitError another package can build. Its unexported
	// fields cannot be set from outside, so it claims no budget and no
	// position, and it carries no mark of this package.
	{"forged zero value", &HeaderLimitError{}},
	{"wrapped forged zero value", fmt.Errorf("outer: %w", &HeaderLimitError{})},

	// A typed nil inside a non-nil error interface: errors.As reports this as
	// found and leaves the caller holding nil to dereference.
	{"typed nil", (*HeaderLimitError)(nil)},
	{"wrapped typed nil", fmt.Errorf("outer: %w", (*HeaderLimitError)(nil))},

	// An error whose Unwrap hands out a forgery, so that a chain walk meets one
	// at depth rather than at the top.
	{"unwraps to a forgery", unwrapsTo{&HeaderLimitError{}}},
}

// matchAllIsError claims to be every error it is asked about, and to be
// assignable to every target asked of it.
type matchAllIsError struct{}

func (matchAllIsError) Error() string { return "adversarial: is everything" }
func (matchAllIsError) Is(error) bool { return true }
func (matchAllIsError) As(any) bool   { return true }

// unwrapsTo hands out another error when a chain is walked.
type unwrapsTo struct{ inner error }

func (u unwrapsTo) Error() string { return "adversarial: unwraps to " + u.inner.Error() }
func (u unwrapsTo) Unwrap() error { return u.inner }

// endlessChain unwraps to itself forever.
type endlessChain struct{}

func (e endlessChain) Error() string { return "adversarial: endless chain" }
func (e endlessChain) Unwrap() error { return e }

// TestHeaderLimit_rejectsForgeries pins the predicate itself: only a refusal
// this package made is one. Every shape an input can present — an error that
// claims every identity, a HeaderLimitError another package wrote, a typed nil,
// a forgery met at depth, a chain that does not end — is refused, without
// panicking and without hanging.
func TestHeaderLimit_rejectsForgeries(t *testing.T) {
	cases := append([]struct {
		name string
		err  error
	}{
		// A chain that never ends. It is tested here rather than injected into
		// a parse because the walk this predicate promises to bound is the one
		// under test; Go's own errors.Is, which the pre-existing EOF
		// classification uses, does not bound its walk and would not return.
		{"endless chain", endlessChain{}},
		{"wrapped endless chain", unwrapsTo{endlessChain{}}},
	}, adversarialFailures...)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, ok := HeaderLimit(tc.err)
			if ok {
				t.Errorf("HeaderLimit(%v) reported a refusal", tc.err)
			}
			if limit != nil {
				t.Errorf("HeaderLimit(%v) = %v, want nil alongside false", tc.err, limit)
			}
			// Whatever it returns must be safe to read, which is the whole
			// point of returning it rather than what errors.As would assign.
			_ = limit.Limit()
			_ = limit.Offset()
			_ = limit.Error()
		})
	}

	// A genuine refusal is still found, at the top of a chain and inside one.
	genuine := newHeaderLimitError(HeaderLimitBytes, 42, 7, 7)
	for _, err := range []error{genuine, fmt.Errorf("outer: %w", genuine), unwrapsTo{genuine}} {
		limit, ok := HeaderLimit(err)
		if !ok {
			t.Fatalf("HeaderLimit(%v) found no refusal", err)
		}
		if limit.Limit() != 42 || limit.Offset() != 7 {
			t.Errorf("limit = (%d, %d), want (42, 7)", limit.Limit(), limit.Offset())
		}
	}
}

// multiUnwrap is an error that unwraps to several errors at once, the way
// errors.Join does, so that a tree can be built without depending on how the
// standard library happens to assemble one.
type multiUnwrap struct{ children []error }

func (m multiUnwrap) Error() string   { return "adversarial: multi" }
func (m multiUnwrap) Unwrap() []error { return m.children }

// TestHeaderLimit_walksErrorTrees pins that the predicate searches the whole
// error tree an input or a caller can build — both Unwrap() error and
// Unwrap() []error — and that only a genuine refusal is ever found in it.
//
// errors.Join is the shape that matters most here: an error joined with others
// is not on any single chain, so a walk that follows one link per node would
// report no refusal for a message that really was refused.
func TestHeaderLimit_walksErrorTrees(t *testing.T) {
	genuine := newHeaderLimitError(HeaderLimitBytes, 64, 9, 9)
	other := errors.New("something else")
	forged := &HeaderLimitError{}
	var typedNil *HeaderLimitError

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"genuine direct", genuine, true},
		{"genuine single wrap", fmt.Errorf("outer: %w", genuine), true},
		{"genuine double wrap", fmt.Errorf("a: %w", fmt.Errorf("b: %w", genuine)), true},

		{"join, genuine first", errors.Join(genuine, other, other), true},
		{"join, genuine middle", errors.Join(other, genuine, other), true},
		{"join, genuine last", errors.Join(other, other, genuine), true},
		{"join of two, genuine second", errors.Join(other, genuine), true},

		{"wrap around a join", fmt.Errorf("outer: %w", errors.Join(other, genuine)), true},
		{"join of wraps", errors.Join(other, fmt.Errorf("inner: %w", genuine)), true},
		{"join inside a join", errors.Join(other, errors.Join(other, genuine)), true},
		{"deep nest of wrap and join", fmt.Errorf("a: %w",
			errors.Join(other, fmt.Errorf("b: %w", errors.Join(other, genuine)))), true},

		{"join of forgeries", errors.Join(forged, forged, forged), false},
		{"join with a typed nil", errors.Join(other, typedNil, forged), false},
		{"join of forgeries and a genuine", errors.Join(forged, typedNil, genuine), true},
		{"multi unwrap with a typed nil first", multiUnwrap{[]error{typedNil, genuine}}, true},
		{"multi unwrap of nothing", multiUnwrap{nil}, false},
		{"multi unwrap holding nils", multiUnwrap{[]error{nil, nil}}, false},

		{"match-all wrapper around a genuine", unwrapsTo{genuine}, true},
		{"match-all wrapper around a forgery", unwrapsTo{forged}, false},
		{"join of a match-all and a genuine", errors.Join(matchAllIsError{}, genuine), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit, ok := HeaderLimit(tc.err)
			if ok != tc.want {
				t.Fatalf("HeaderLimit(%v) = %v, want %v", tc.err, ok, tc.want)
			}
			if !ok {
				if limit != nil {
					t.Errorf("HeaderLimit() = %v alongside false, want nil", limit)
				}
				return
			}
			if limit != genuine {
				t.Errorf("HeaderLimit() = %v, want the genuine refusal", limit)
			}
			if limit.Limit() != 64 || limit.Offset() != 9 {
				t.Errorf("limit = (%d, %d), want (64, 9)", limit.Limit(), limit.Offset())
			}
		})
	}
}

// TestHeaderLimit_boundedOnHostileGraphs pins that the walk finishes on the
// error graphs an input can build to make it not finish: ones that cycle, and
// ones too wide to hold. Each case must return, and none may find a refusal
// that is not there.
//
// The test asserts termination and the verdict, not how much of a graph was
// searched. What is guaranteed is the bound, not a particular reach into a
// graph past it.
func TestHeaderLimit_boundedOnHostileGraphs(t *testing.T) {
	forged := &HeaderLimitError{}
	genuine := newHeaderLimitError(HeaderLimitBytes, 1, 2, 2)

	// A cycle of two nodes, tied after construction.
	ringA := &mutableWrap{}
	ringB := &mutableWrap{inner: ringA}
	ringA.inner = ringB

	// A ring that also carries a forgery, so a walk has something to examine
	// while going round.
	ringWithForgery := &mutableWrap{}
	ringWithForgery.inner = multiUnwrap{[]error{forged, ringWithForgery}}

	// A join far wider than anything held pending. The children are one value
	// repeated, so building it costs one slice rather than a million errors.
	const huge = 1 << 20
	wide := make([]error, huge)
	for i := range wide {
		wide[i] = forged
	}

	wideWithGenuineFirst := make([]error, huge)
	copy(wideWithGenuineFirst, wide)
	wideWithGenuineFirst[0] = genuine

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"self cycle", endlessChain{}, false},
		{"self cycle in a join", errors.Join(endlessChain{}, forged), false},
		{"two node cycle", ringA, false},
		{"cycle carrying a forgery", ringWithForgery, false},
		{"huge join of forgeries", multiUnwrap{wide}, false},
		{"huge join under a wrap", fmt.Errorf("outer: %w", multiUnwrap{wide}), false},
		{"huge join, genuine first", multiUnwrap{wideWithGenuineFirst}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			var got bool

			go func() {
				defer close(done)
				_, got = HeaderLimit(tc.err)
			}()

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("HeaderLimit did not return: the walk is not bounded")
			}

			if got != tc.want {
				t.Errorf("HeaderLimit() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHeaderLimit_allocationIsFixed pins that what the walk costs is decided by
// the walk and not by the graph: searching a join of a million errors allocates
// no more than searching a join of three.
func TestHeaderLimit_allocationIsFixed(t *testing.T) {
	forged := &HeaderLimitError{}

	small := make([]error, 3)
	huge := make([]error, 1<<20)
	for i := range small {
		small[i] = forged
	}
	for i := range huge {
		huge[i] = forged
	}

	perRun := func(children []error) float64 {
		err := error(multiUnwrap{children})
		return testing.AllocsPerRun(20, func() { HeaderLimit(err) })
	}

	if got, want := perRun(huge), perRun(small); got > want {
		t.Errorf("searching a join of %d allocated %v, a join of %d allocated %v: the cost must not follow the graph",
			len(huge), got, len(small), want)
	}
}

// mutableWrap is a single-child wrapper whose child can be set after it exists,
// which is how a cycle is tied.
type mutableWrap struct{ inner error }

func (m *mutableWrap) Error() string { return "adversarial: ring" }
func (m *mutableWrap) Unwrap() error { return m.inner }

// nilHostileWrap reads a field in Unwrap, so unwrapping a nil one panics. An
// error a caller or an input writes is free to be written this way — most are,
// since most are never nil — and a typed nil of such a type is something an
// input can hand over.
type nilHostileWrap struct{ inner error }

func (n *nilHostileWrap) Error() string {
	if n == nil {
		return "adversarial: nil hostile wrap"
	}
	return "adversarial: hostile wrap"
}

func (n *nilHostileWrap) Unwrap() error { return n.inner }

// TestHeaderLimit_doesNotUnwrapNilValues pins that a node holding nil is never
// asked to unwrap itself. Nothing here may panic, and a genuine refusal beside
// such a node must still be found.
func TestHeaderLimit_doesNotUnwrapNilValues(t *testing.T) {
	genuine := newHeaderLimitError(HeaderLimitBytes, 8, 3, 3)
	var hostile *nilHostileWrap

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"typed nil that would panic", hostile, false},
		{"wrapped typed nil that would panic", fmt.Errorf("outer: %w", hostile), false},
		{"typed nil that would panic, in a join", errors.Join(hostile, genuine), true},
		{"typed nil of this package's own type", (*HeaderLimitError)(nil), false},
		{"a live one still unwraps", &nilHostileWrap{inner: genuine}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit, ok := HeaderLimit(tc.err)
			if ok != tc.want {
				t.Fatalf("HeaderLimit(%v) = %v, want %v", tc.err, ok, tc.want)
			}
			if ok && limit != genuine {
				t.Errorf("HeaderLimit() = %v, want the genuine refusal", limit)
			}
		})
	}
}

// TestHeaderLimitError_nilSafe pins that a typed nil of this type can be
// formatted and read without bringing anything down. An input can present one,
// and what arrives from an input is exactly what must not be trusted to be
// well formed.
func TestHeaderLimitError_nilSafe(t *testing.T) {
	var e *HeaderLimitError

	if got := e.Error(); got == "" {
		t.Errorf("Error() = %q, want some text", got)
	}
	if got := fmt.Sprintf("%v", error(e)); got == "" {
		t.Errorf("formatting a typed nil produced nothing")
	}
	if got := e.Limit(); got != 0 {
		t.Errorf("Limit() = %d, want 0", got)
	}
	if got := e.Offset(); got != 0 {
		t.Errorf("Offset() = %d, want 0", got)
	}
	if err := e.Unwrap(); !errors.Is(err, ErrHeaderTooBig) {
		t.Errorf("Unwrap() = %v, want ErrHeaderTooBig", err)
	}
}

// TestHeaderLimitError_textIsLayerNeutral pins that the human text does not
// name a layer the error may not have come from. A block read by
// ReadHeaderLimit is not a part of a multipart, and saying so would be wrong in
// a log and misleading in a bug report.
//
// The exact wording is not a contract; that it does not claim a layer is.
func TestHeaderLimitError_textIsLayerNeutral(t *testing.T) {
	_, _, err := readHeaderLimitFrom(budgetHeader, 8, 4096)
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, claim := range []string{"part", "multipart"} {
		if strings.Contains(err.Error(), claim) {
			t.Errorf("Error() = %q, want no claim about %q for a block read directly", err, claim)
		}
	}
}

// TestReadHeaderLimit_adversarialInputIsNotARefusal pins the predicate where a
// header block is read directly: an input that fails with something built to
// look like a refusal is an ordinary read failure.
func TestReadHeaderLimit_adversarialInputIsNotARefusal(t *testing.T) {
	const header = "A: one\r\nB: two\r\n\r\n"

	for _, tc := range adversarialFailures {
		t.Run(tc.name, func(t *testing.T) {
			src := &errorAfterReader{s: header, n: len("A: one\r\n"), err: tc.err}
			br := bufio.NewReaderSize(src, 4096)

			// A budget far larger than the block, so nothing here is the
			// budget's doing.
			_, err := ReadHeaderLimit(br, 1<<20)
			if err == nil {
				t.Fatal("want the input's failure")
			}
			if _, refused := HeaderLimit(err); refused {
				t.Errorf("err = %v, reported as a refusal", err)
			}
			_ = err.Error() // must not panic
		})
	}
}

// TestMultipartReader_adversarialInputIsNotARefusal pins the same where a part
// header is read.
func TestMultipartReader_adversarialInputIsNotARefusal(t *testing.T) {
	f := budgetPartsFixture()
	cut := int(f.at["p0.body"]) - len("\r\n")

	for _, tc := range adversarialFailures {
		t.Run(tc.name, func(t *testing.T) {
			r := NewMultipartReader(&errorAfterReader{s: f.body, n: cut, err: tc.err}, "A")
			r.SetMaxHeaderBytes(1 << 20)

			_, err := r.NextPart()
			if err == nil {
				t.Fatal("want the input's failure")
			}
			if _, refused := HeaderLimit(err); refused {
				t.Errorf("NextPart() = %v, reported as a refusal", err)
			}
			_ = err.Error() // must not panic
		})
	}
}

// TestReadHeader_bufferSizeInvariance pins the line reading the budget is
// threaded through: a header read in pieces, with a buffer small enough to
// split lines and to land a "\r\n" across a boundary, must produce the same
// fields and the same bytes back out as one read whole. This is what keeps the
// verbatim round trip verbatim.
func TestReadHeader_bufferSizeInvariance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"crlf", budgetHeader},
		{"lf", strings.ReplaceAll(budgetHeader, "\r\n", "\n")},
		{"folded", "A: one\r\n\tfolded with a tab\r\n continued with a space\r\nB: two\r\n\r\n"},
		{"long line", "A: " + strings.Repeat("word ", 200) + "\r\n\r\n"},
		{"no trailing newline", "A: one\r\nB: two"},
		{"empty value", "A:\r\nB: \r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := writeHeaderOf(t, tc.header, 4096)
			for _, bufSize := range []int{16, 17, 18, 19, 23, 32, 64, 512} {
				if got := writeHeaderOf(t, tc.header, bufSize); got != want {
					t.Errorf("buffer %d: re-serialized to\n%q\nwant\n%q", bufSize, got, want)
				}
			}
		})
	}
}

// writeHeaderOf reads a header block through a reader of the given buffer size
// and writes it back out, which is how a raw field is compared without reaching
// into the parser.
func writeHeaderOf(t *testing.T, header string, bufSize int) string {
	t.Helper()

	br := bufio.NewReaderSize(strings.NewReader(header+budgetBody), bufSize)
	h, err := ReadHeader(br)
	if err != nil {
		t.Fatalf("ReadHeader() = %v", err)
	}

	var b bytes.Buffer
	if err := WriteHeader(&b, h); err != nil {
		t.Fatalf("WriteHeader() = %v", err)
	}
	return b.String()
}

// budgetPartsFixture is a multipart body whose middle part has by far the
// largest header block, so that one budget can be swept across that block while
// the parts around it stay well within it.
func budgetPartsFixture() offsetFixture {
	return newOffsetFixture(
		[2]string{"", "--A\r\n"},
		[2]string{"p0.header", "Content-Type: text/plain\r\n\r\n"},
		[2]string{"p0.body", "one\r\n"},
		[2]string{"", "--A\r\n"},
		[2]string{"p1.header", "Content-Type: text/plain\r\nX-Pad: " + strings.Repeat("x", 200) + "\r\n\r\n"},
		[2]string{"p1.body", "two\r\n"},
		[2]string{"", "--A\r\n"},
		[2]string{"p2.header", "Content-Type: text/plain\r\n\r\n"},
		[2]string{"p2.body", "three\r\n"},
		[2]string{"", "--A--\r\n"},
	)
}

// budgetPartBodies is what the parts of budgetPartsFixture contain.
var budgetPartBodies = []string{"one", "two", "three"}

// TestMultipartReader_SetMaxHeaderBytes sweeps the budget across the size of
// the largest part header and pins where it falls, what the failure says, and
// what survives it.
func TestMultipartReader_SetMaxHeaderBytes(t *testing.T) {
	f := budgetPartsFixture()
	size := f.at["p1.body"] - f.at["p1.header"]

	for _, delta := range []int64{-1, 0, 1} {
		limit := size + delta

		r := NewMultipartReader(strings.NewReader(f.body), "A")
		r.SetMaxHeaderBytes(limit)

		// The part before the oversized one is read and is complete.
		p, err := r.NextPart()
		if err != nil {
			t.Fatalf("limit%+d: part 0: %v", delta, err)
		}
		if got := readPartBody(t, p); got != budgetPartBodies[0] {
			t.Errorf("limit%+d: part 0 = %q, want %q", delta, got, budgetPartBodies[0])
		}

		p, err = r.NextPart()

		if delta < 0 {
			if p != nil {
				t.Errorf("limit-1: part 1 = %v, want none", p)
			}
			limitErr, refused := HeaderLimit(err)
			if !refused {
				t.Fatalf("limit-1: part 1: err = %v, want a genuine refusal", err)
			}
			if !errors.Is(err, ErrHeaderTooBig) {
				t.Errorf("limit-1: part 1: err = %v, want it to wrap ErrHeaderTooBig", err)
			}
			if limitErr.Limit() != limit {
				t.Errorf("limit-1: Limit() = %d, want %d", limitErr.Limit(), limit)
			}
			if want := f.at["p1.header"]; limitErr.Offset() != want {
				t.Errorf("limit-1: Offset() = %d, want the header block at %d", limitErr.Offset(), want)
			}
			// The reader cannot resynchronize, so it ends rather than
			// inventing a part out of whatever it is now standing on.
			for i := 0; i < 3; i++ {
				if _, err := r.NextPart(); err != io.EOF {
					t.Fatalf("limit-1: NextPart() after the failure = %v, want io.EOF", err)
				}
			}
			continue
		}

		if err != nil {
			t.Fatalf("limit%+d: part 1: %v", delta, err)
		}
		if got := readPartBody(t, p); got != budgetPartBodies[1] {
			t.Errorf("limit%+d: part 1 = %q, want %q", delta, got, budgetPartBodies[1])
		}
		// The budget is per part: the parts before this one spent nothing of it.
		p, err = r.NextPart()
		if err != nil {
			t.Fatalf("limit%+d: part 2: %v", delta, err)
		}
		if got := readPartBody(t, p); got != budgetPartBodies[2] {
			t.Errorf("limit%+d: part 2 = %q, want %q", delta, got, budgetPartBodies[2])
		}
		if _, err := r.NextPart(); err != io.EOF {
			t.Errorf("limit%+d: NextPart() after the last part = %v, want io.EOF", delta, err)
		}
	}
}

// TestMultipartReader_maxHeaderBytesChunkInvariance pins that the budget's
// decision does not depend on how the body arrives.
func TestMultipartReader_maxHeaderBytesChunkInvariance(t *testing.T) {
	f := budgetPartsFixture()
	size := f.at["p1.body"] - f.at["p1.header"]

	sources := map[string]func() io.Reader{
		"whole":         func() io.Reader { return strings.NewReader(f.body) },
		"one byte":      func() io.Reader { return iotest.OneByteReader(strings.NewReader(f.body)) },
		"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(f.body)) },
		"random chunks": func() io.Reader { return newRandomChunkReader(f.body, 13, 1) },
	}

	for name, src := range sources {
		for _, delta := range []int64{-1, 0, 1} {
			r := NewMultipartReader(src(), "A")
			r.SetMaxHeaderBytes(size + delta)

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

			want := budgetPartBodies
			if delta < 0 {
				want = budgetPartBodies[:1]
			}
			if len(bodies) != len(want) {
				t.Errorf("%s at limit%+d: read %v, want %v", name, delta, bodies, want)
				continue
			}
			for i := range want {
				if bodies[i] != want[i] {
					t.Errorf("%s at limit%+d: part %d = %q, want %q", name, delta, i, bodies[i], want[i])
				}
			}
			if delta < 0 && !errors.Is(err, ErrHeaderTooBig) {
				t.Errorf("%s at limit-1: err = %v, want ErrHeaderTooBig", name, err)
			}
			if delta >= 0 && err != io.EOF {
				t.Errorf("%s at limit%+d: err = %v, want io.EOF", name, delta, err)
			}
		}
	}
}

// TestMultipartReader_maxHeaderBytesUnset pins that a reader budgets nothing
// until it is asked to, which is what every existing caller of
// NewMultipartReader gets.
func TestMultipartReader_maxHeaderBytesUnset(t *testing.T) {
	f := budgetPartsFixture()

	for _, tc := range []struct {
		name string
		set  func(*MultipartReader)
	}{
		{"never set", func(*MultipartReader) {}},
		{"set to zero", func(r *MultipartReader) { r.SetMaxHeaderBytes(0) }},
		{"set below zero", func(r *MultipartReader) { r.SetMaxHeaderBytes(-1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewMultipartReader(strings.NewReader(f.body), "A")
			tc.set(r)

			for i, want := range budgetPartBodies {
				p, err := r.NextPart()
				if err != nil {
					t.Fatalf("part %d: %v", i, err)
				}
				if got := readPartBody(t, p); got != want {
					t.Errorf("part %d = %q, want %q", i, got, want)
				}
			}
			if _, err := r.NextPart(); err != io.EOF {
				t.Errorf("NextPart() after the last part = %v, want io.EOF", err)
			}
		})
	}
}

// TestMultipartReader_maxHeaderBytesEmptyBlocks pins the smallest header blocks
// a part can have against the budget. A part introduced by a second delimiter
// line has no header block at all and costs nothing, so the smallest budget
// admits it; a part whose block is just the empty line costs those two bytes and
// is refused by a budget of one.
func TestMultipartReader_maxHeaderBytesEmptyBlocks(t *testing.T) {
	f := newOffsetFixture(
		[2]string{"", "--A\r\n"},
		[2]string{"p0.header", ""}, // no header block: the next line is a delimiter
		[2]string{"p0.body", ""},
		[2]string{"", "--A\r\n"},
		[2]string{"p1.header", "\r\n"}, // an empty header block
		[2]string{"p1.body", "second\r\n"},
		[2]string{"", "--A--\r\n"},
	)

	for _, tc := range []struct {
		limit     int64
		wantParts int
	}{
		{1, 1}, // the headerless part fits; the empty block of two bytes does not
		{2, 2},
		{3, 2},
	} {
		r := NewMultipartReader(strings.NewReader(f.body), "A")
		r.SetMaxHeaderBytes(tc.limit)

		var got int
		var err error
		for {
			var p *Part
			if p, err = r.NextPart(); err != nil {
				break
			}
			readPartBody(t, p)
			got++
		}

		if got != tc.wantParts {
			t.Errorf("limit %d: read %d parts, want %d", tc.limit, got, tc.wantParts)
		}
		if tc.wantParts < 2 {
			if !errors.Is(err, ErrHeaderTooBig) {
				t.Errorf("limit %d: err = %v, want ErrHeaderTooBig", tc.limit, err)
			}
		} else if err != io.EOF {
			t.Errorf("limit %d: err = %v, want io.EOF", tc.limit, err)
		}
	}
}

// readPartBody reads a part's body whole, with its trailing line ending
// removed so that fixtures read as what they contain.
func readPartBody(t *testing.T, p *Part) string {
	t.Helper()
	b, err := io.ReadAll(p)
	if err != nil {
		t.Fatalf("reading part body: %v", err)
	}
	return strings.TrimSuffix(string(b), "\r\n")
}

// isRefusal reports whether err is a header budget refusal this package made.
func isRefusal(err error) bool {
	_, ok := HeaderLimit(err)
	return ok
}

// headerKeys returns the keys of a header in document order.
func headerKeys(h Header) []string {
	var keys []string
	fs := h.Fields()
	for fs.Next() {
		keys = append(keys, fs.Key())
	}
	return keys
}
