package message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit/textproto"
)

// These tests pin ROB-120-007: every classification of an error that may have
// come from a caller's component — the source io.Reader, a CharsetReader, a
// DiagnosticSink — is bounded, never asks the error about itself, and is not
// fooled by a value that only looks like the parser's.
//
// Every hostile error used in process is finite work whatever the code under
// test does: a cycle answers a bounded number of Unwrap calls and then reports
// no child, so a regression to an unbounded walk blows the fuse and fails
// instead of spinning. The one probe with a cycle that never ends runs in a
// subprocess under a deadline; see TestHostileErrorGraphs_subprocess.

const hostileFuse = 10000

// fusedLoop unwraps to itself until its fuse blows.
type fusedLoop struct {
	calls int
	blown bool
}

func (*fusedLoop) Error() string { return "fused loop" }
func (f *fusedLoop) Unwrap() error {
	f.calls++
	if f.calls > hostileFuse {
		f.blown = true
		return nil
	}
	return f
}

// fusedJoinLoop is the same through Unwrap() []error, with a nil beside it.
type fusedJoinLoop struct {
	kids  []error
	calls int
	blown bool
}

func newFusedJoinLoop() *fusedJoinLoop {
	f := &fusedJoinLoop{}
	f.kids = []error{nil, f}
	return f
}

func (*fusedJoinLoop) Error() string { return "fused join loop" }
func (f *fusedJoinLoop) Unwrap() []error {
	f.calls++
	if f.calls > hostileFuse {
		f.blown = true
		return nil
	}
	return f.kids
}

// blown reports whether any fused loop in errs blew its fuse.
func blown(errs ...error) bool {
	for _, err := range errs {
		switch f := err.(type) {
		case *fusedLoop:
			if f.blown {
				return true
			}
		case *fusedJoinLoop:
			if f.blown {
				return true
			}
		}
	}
	return false
}

// claimsAll answers yes to every Is and As question, and counts them.
type claimsAll struct{ asked *int }

func (claimsAll) Error() string   { return "claims every identity" }
func (c claimsAll) Is(error) bool { *c.asked++; return true }
func (c claimsAll) As(any) bool   { *c.asked++; return true }
func (c claimsAll) Unwrap() error { return nil }

// unwrapPanic panics in Unwrap with v, which may be nil.
type unwrapPanic struct{ v any }

func (unwrapPanic) Error() string   { return "unwrap panics" }
func (u unwrapPanic) Unwrap() error { panic(u.v) }

// joinPanic panics in Unwrap() []error.
type joinPanic struct{}

func (joinPanic) Error() string   { return "join panics" }
func (joinPanic) Unwrap() []error { panic("join") }

// nilDeref dereferences its receiver in Unwrap, so a typed nil of it panics if
// it is ever unwrapped.
type nilDeref struct{ inner error }

func (*nilDeref) Error() string   { return "nil deref" }
func (n *nilDeref) Unwrap() error { return n.inner }

// embeddingFatal is what another package can write: a struct embedding the
// FatalError interface inherits its unexported method and so satisfies it,
// with no fatal error of the parser's behind it.
type embeddingFatal struct{ FatalError }

func (embeddingFatal) Error() string { return "embeds FatalError" }

// customWrap is an ordinary caller wrapper.
type customWrap struct{ inner error }

func (c customWrap) Error() string { return "custom: " + c.inner.Error() }
func (c customWrap) Unwrap() error { return c.inner }

// manyErr unwraps to its children as they are; unlike errors.Join it keeps
// nils, which is how a long run of them reaches a walk.
type manyErr struct{ kids []error }

func (manyErr) Error() string     { return "many" }
func (m manyErr) Unwrap() []error { return m.kids }

// Values whose type is not comparable: == between two of them panics.
type (
	sliceError  []string
	structError struct{ s []int }
)

func (sliceError) Error() string  { return "slice error" }
func (structError) Error() string { return "struct error" }

func TestIsFatal_exactProvenance(t *testing.T) {
	genuine := newFatalError("stopped", errors.New("cause"))
	for name, err := range map[string]error{
		"itself":         genuine,
		"fmt-wrapped":    fmt.Errorf("part 1: %w", genuine),
		"joined":         errors.Join(errors.New("other"), genuine),
		"custom":         customWrap{genuine},
		"deep mix":       customWrap{fmt.Errorf("x: %w", errors.Join(io.EOF, genuine))},
		"beside a loop":  errors.Join(&fusedLoop{}, genuine),
		"beside oddness": errors.Join(sliceError{"a"}, structError{[]int{1}}, genuine),
		"after a panic":  errors.Join(unwrapPanic{nil}, joinPanic{}, genuine),
	} {
		if !IsFatal(err) {
			t.Errorf("%s: IsFatal = false, want true", name)
		}
	}

	asked := 0
	for name, err := range map[string]error{
		"nil":                         nil,
		"typed nil":                   (*fatalParseError)(nil),
		"wrapped typed nil":           fmt.Errorf("x: %w", (*fatalParseError)(nil)),
		"embedding a nil FatalError":  embeddingFatal{},
		"embedding a genuine one":     embeddingFatal{genuine},
		"pointer embedding":           &embeddingFatal{},
		"claims every identity":       claimsAll{&asked},
		"wrapped claims":              fmt.Errorf("x: %w", claimsAll{&asked}),
		"charset warning":             UnknownCharsetError{errors.New("x")},
		"EOF":                         io.EOF,
		"genuine past the node bound": deep(300, genuine),
	} {
		if IsFatal(err) {
			t.Errorf("%s: IsFatal = true, want false", name)
		}
	}
	if asked != 0 {
		t.Errorf("an Is or As method was asked %d times", asked)
	}
}

// deep wraps err in depth custom wrappers whose Error does not recurse.
func deep(depth int, err error) error {
	for range depth {
		err = flatWrap{err}
	}
	return err
}

type flatWrap struct{ inner error }

func (flatWrap) Error() string   { return "flat wrap" }
func (f flatWrap) Unwrap() error { return f.inner }

func TestIsUnknown_exactProvenance(t *testing.T) {
	cause := errors.New("no decoder")
	charset, encoding := UnknownCharsetError{cause}, UnknownEncodingError{cause}

	for name, tc := range map[string]struct {
		err               error
		charset, encoding bool
	}{
		"charset":                {charset, true, false},
		"encoding":               {encoding, false, true},
		"fmt-wrapped charset":    {fmt.Errorf("x: %w", charset), true, false},
		"joined both":            {errors.Join(encoding, charset), true, true},
		"custom-wrapped":         {customWrap{encoding}, false, true},
		"inside a fatal error":   {newFatalError("", charset), true, false},
		"cause is a fatal error": {UnknownCharsetError{newFatalError("", cause)}, true, false},
		"pointer to a warning":   {&UnknownCharsetError{charset}, true, false},
		"charset of a loop":      {UnknownCharsetError{&fusedLoop{}}, true, false},
		"zero charset":           {UnknownCharsetError{}, false, false},
		"zero encoding":          {UnknownEncodingError{}, false, false},
		"wrapped zero values":    {fmt.Errorf("x: %w", errors.Join(UnknownCharsetError{}, UnknownEncodingError{})), false, false},
		"typed nil pointer":      {(*UnknownCharsetError)(nil), false, false},
		"pointer to zero":        {&UnknownEncodingError{}, false, false},
		"claims every identity":  {claimsAll{new(int)}, false, false},
		"loop":                   {&fusedLoop{}, false, false},
		"join loop":              {newFusedJoinLoop(), false, false},
		"panicking Unwrap":       {unwrapPanic{"boom"}, false, false},
		"panic(nil) Unwrap":      {unwrapPanic{nil}, false, false},
		"panicking join":         {joinPanic{}, false, false},
		"typed nil deref":        {(*nilDeref)(nil), false, false},
		"warning past the bound": {deep(300, charset), false, false},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panicked: %v", p)
				}
			}()
			if got := IsUnknownCharset(tc.err); got != tc.charset {
				t.Errorf("IsUnknownCharset = %v, want %v", got, tc.charset)
			}
			if got := IsUnknownEncoding(tc.err); got != tc.encoding {
				t.Errorf("IsUnknownEncoding = %v, want %v", got, tc.encoding)
			}
			// The predicates search the whole graph, so a fatal error on either
			// side of a warning is found too; see TestParse_charsetReaderFailsWithAFatal
			// for what the parser does with the second shape.
			wantFatal := name == "inside a fatal error" || name == "cause is a fatal error"
			if got := IsFatal(tc.err); got != wantFatal {
				t.Errorf("IsFatal = %v, want %v", got, wantFatal)
			}
		})
	}
}

// TestPredicates_hostileGraphsReturn pins that no predicate hangs, panics or
// unwraps a typed nil on the hostile graphs a component can hand back.
func TestPredicates_hostileGraphsReturn(t *testing.T) {
	loop, join := &fusedLoop{}, newFusedJoinLoop()
	predicates := map[string]func(error) bool{
		"IsFatal":           IsFatal,
		"IsUnknownCharset":  IsUnknownCharset,
		"IsUnknownEncoding": IsUnknownEncoding,
		"HeaderLimit":       func(err error) bool { _, ok := textproto.HeaderLimit(err); return ok },
		"LineLimit":         func(err error) bool { _, ok := textproto.LineLimit(err); return ok },
	}
	for name, err := range map[string]error{
		"loop":                           loop,
		"join loop":                      join,
		"wrapped loop":                   fmt.Errorf("x: %w", loop),
		"warning of a loop":              UnknownEncodingError{loop},
		"panicking Unwrap":               unwrapPanic{"boom"},
		"panic(nil) Unwrap":              unwrapPanic{nil},
		"panicking join":                 joinPanic{},
		"typed nil deref":                (*nilDeref)(nil),
		"joined typed nils":              errors.Join((*nilDeref)(nil), (*fatalParseError)(nil)),
		"non-comparable":                 errors.Join(sliceError{"a"}, structError{}),
		"nil prefix past the slot bound": manyErr{append(make([]error, 1<<12), loop)},
		"diamond":                        diamond(24),
		"claims everything":              claimsAll{new(int)},
	} {
		for pname, p := range predicates {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s(%s) panicked: %v", pname, name, r)
					}
				}()
				want := pname == "IsUnknownEncoding" && name == "warning of a loop"
				if got := p(err); got != want {
					t.Errorf("%s(%s) = %v, want %v", pname, name, got, want)
				}
			}()
		}
	}
	if blown(loop, join) {
		t.Error("a loop's fuse blew: a walk was not bounded")
	}
}

// diamond builds a graph of depth levels in which every level reaches the next
// along two paths, 2^depth paths in all, from depth small joins.
func diamond(depth int) error {
	var d error = errors.New("bottom")
	for range depth {
		d = errors.Join(d, d)
	}
	return d
}

// failAfter yields s and then fails with err on every later read.
type failAfter struct {
	s   *strings.Reader
	err error
}

func (r *failAfter) Read(p []byte) (int, error) {
	if r.s.Len() == 0 {
		return 0, r.err
	}
	return r.s.Read(p)
}

// hostileSourceErrors are the source failures the parser paths below are run
// against. Each call builds fresh fused values.
func hostileSourceErrors() map[string]error {
	return map[string]error{
		"loop":              &fusedLoop{},
		"join loop":         newFusedJoinLoop(),
		"panicking Unwrap":  unwrapPanic{"boom"},
		"panic(nil) Unwrap": unwrapPanic{nil},
		"panicking join":    joinPanic{},
		"typed nil deref":   (*nilDeref)(nil),
		"claims everything": claimsAll{new(int)},
		"embedding fatal":   embeddingFatal{},
		"zero warning":      UnknownEncodingError{},
		"forged refusal":    &textproto.HeaderLimitError{},
		"ordinary":          errors.New("disk on fire"),
	}
}

// TestParse_hostileSourceFailure pins the defect this item was opened for: a
// source that fails with an error whose graph never ends, or whose methods
// panic, inside a part's header block made NextPart hang. Each such failure is
// now returned as the ordinary failure it is, at the root and at a child, by
// the iterator, by Walk, and with the sibling before it intact; nothing is
// diagnosed and the parse is not stopped.
func TestParse_hostileSourceFailure(t *testing.T) {
	const sibling = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n--B\r\nX: 1\r\n"

	for name, fail := range hostileSourceErrors() {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panicked: %v", p)
				}
			}()

			// The root's own header block.
			session, _, err := Parse(&failAfter{strings.NewReader("X: 1\r\n"), fail}, nil)
			if err == nil || session.fatalErr() != nil || session.Diagnostics() != nil {
				t.Errorf("root: err %v, stop %v, diagnostics %v: want the ordinary failure", err, session.fatalErr(), session.Diagnostics())
			}

			// A child's, after a sibling that is complete.
			session, e, err := Parse(&failAfter{strings.NewReader(sibling), fail}, nil)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			mr := e.MultipartReader()
			zero, err := mr.NextPart()
			if err != nil {
				t.Fatalf("first NextPart() = %v", err)
			}
			if got := readBodyOf(t, zero); got != "zero" {
				t.Errorf("first part = %q, want %q", got, "zero")
			}
			part, err := mr.NextPart()
			if part != nil || err == nil || IsFatal(err) {
				t.Errorf("second NextPart() = (%v, %v), want no part and the ordinary failure", part, err)
			}
			if session.fatalErr() != nil || session.Diagnostics() != nil {
				t.Errorf("stop %v, diagnostics %v: want neither", session.fatalErr(), session.Diagnostics())
			}

			// Walk reaches the same failure, after visiting the root and the
			// sibling.
			_, e, _ = Parse(&failAfter{strings.NewReader(sibling), fail}, nil)
			var visited int
			err = e.Walk(func(_ []int, part *Entity, _ error) error {
				if part == nil {
					t.Error("Walk handed over a nil entity")
				}
				visited++
				return nil
			})
			if err == nil || visited != 2 {
				t.Errorf("Walk() = %v after %d entities, want the failure after 2", err, visited)
			}
			if blown(fail) {
				t.Error("the fuse blew: a classification walked the failure without a bound")
			}
		})
	}
}

// withCharsetReader installs r as CharsetReader for the duration of t.
func withCharsetReader(t *testing.T, r func(string, io.Reader) (io.Reader, error)) {
	t.Helper()
	old := CharsetReader
	t.Cleanup(func() { CharsetReader = old })
	CharsetReader = r
}

const hostileCharsetTree = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain; charset=x-hostile\r\n\r\nraw one\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\ntwo\r\n--B--\r\n"

// TestParse_hostileCharsetReaderWarning pins that a CharsetReader failing with
// a hostile error still yields what a warning always has: the part, its body
// undecoded, the warning with it, and the sibling after it. Walk asks only the
// warning's own type, so the hostile cause is never walked to classify it.
func TestParse_hostileCharsetReaderWarning(t *testing.T) {
	for name, fail := range map[string]error{
		"loop":             &fusedLoop{},
		"join loop":        newFusedJoinLoop(),
		"panicking Unwrap": unwrapPanic{"boom"},
		"typed nil deref":  (*nilDeref)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			withCharsetReader(t, func(_ string, r io.Reader) (io.Reader, error) { return r, fail })

			_, e, err := Parse(strings.NewReader(hostileCharsetTree), nil)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			type seen struct {
				body    string
				warning bool
			}
			var got []seen
			err = e.Walk(func(path []int, part *Entity, err error) error {
				if len(path) == 0 {
					return nil
				}
				got = append(got, seen{readBodyOf(t, part), IsUnknownCharset(err)})
				return nil
			})
			want := []seen{{"raw one", true}, {"two", false}}
			if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
				t.Errorf("Walk() = %v, visited %v, want %v", err, got, want)
			}
			if blown(fail) {
				t.Error("the fuse blew: the warning's cause was walked without a bound")
			}
		})
	}
}

// otherParsesStop returns a genuine fatal error from a parse other than any the
// caller is about to run: the stop a sink asked for over an unknown charset.
func otherParsesStop(t *testing.T) error {
	t.Helper()
	withCharsetReader(t, nil)
	_, e, err := Parse(strings.NewReader("Content-Type: text/plain; charset=x-other\r\n\r\nbody"),
		&ParseOptions{DiagnosticSink: stopOnCharset(errors.New("the other parse was stopped"))})
	if e == nil || !isParserFatal(err) {
		t.Fatalf("the other parse returned (%v, %v), want an entity and its stop", e, err)
	}
	return err
}

// TestParse_charsetReaderFailsWithAFatal pins that the parser decides a
// warning by the error it returns, not by searching it. A CharsetReader that
// fails with a genuine fatal error from another parse makes a warning whose
// cause IsFatal finds, but it is still a warning about this part: the part,
// its undecoded body and the siblings after it are returned, and this parse is
// not stopped. This was already so for Walk and the iterator; it is pinned
// because it is the one place where the public predicates and the parser's own
// decision give different answers about the same error.
func TestParse_charsetReaderFailsWithAFatal(t *testing.T) {
	other := otherParsesStop(t)
	withCharsetReader(t, func(_ string, r io.Reader) (io.Reader, error) { return r, other })

	t.Run("root", func(t *testing.T) {
		session, e, err := Parse(strings.NewReader("Content-Type: text/plain; charset=x-hostile\r\n\r\nraw"), nil)
		if e == nil || !IsUnknownCharset(err) || !IsFatal(err) || isParserFatal(err) {
			t.Fatalf("Parse() = (%v, %v), want the entity with a warning whose cause is fatal", e, err)
		}
		if got := readBodyOf(t, e); got != "raw" {
			t.Errorf("body = %q, want the undecoded %q", got, "raw")
		}
		if session.fatalErr() != nil || e.ParseStop() != nil {
			t.Errorf("this parse was stopped: %v", session.fatalErr())
		}
	})

	t.Run("iterator", func(t *testing.T) {
		session, e, err := Parse(strings.NewReader(hostileCharsetTree), nil)
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		mr := e.MultipartReader()
		one, err := mr.NextPart()
		if one == nil || !IsUnknownCharset(err) || !IsFatal(err) {
			t.Fatalf("first NextPart() = (%v, %v), want the part and the warning", one, err)
		}
		if got := readBodyOf(t, one); got != "raw one" {
			t.Errorf("first part = %q, want %q", got, "raw one")
		}
		two, err := mr.NextPart()
		if err != nil || readBodyOf(t, two) != "two" {
			t.Errorf("second NextPart() = (%v, %v), want the sibling", two, err)
		}
		if _, err := mr.NextPart(); err != io.EOF {
			t.Errorf("third NextPart() = %v, want io.EOF", err)
		}
		if session.fatalErr() != nil {
			t.Errorf("this parse was stopped: %v", session.fatalErr())
		}
	})

	t.Run("Walk", func(t *testing.T) {
		_, e, err := Parse(strings.NewReader(hostileCharsetTree), nil)
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		var bodies []string
		var warned int
		err = e.Walk(func(path []int, part *Entity, err error) error {
			if len(path) == 0 {
				return nil
			}
			if IsUnknownCharset(err) && IsFatal(err) {
				warned++
			}
			bodies = append(bodies, readBodyOf(t, part))
			return nil
		})
		if err != nil || warned != 1 || len(bodies) != 2 || bodies[0] != "raw one" || bodies[1] != "two" {
			t.Errorf("Walk() = %v, %d warnings, bodies %q: want both parts and one warning", err, warned, bodies)
		}
	})
}

// TestParse_hostileSinkStopIdentity pins that a DiagnosticSink stopping the
// parse with a hostile error is reported as the session's identical stop by
// the iterator and by Walk, which do not walk the sink's error to decide that.
func TestParse_hostileSinkStopIdentity(t *testing.T) {
	withCharsetReader(t, nil)
	for name, fail := range map[string]error{
		"loop":             &fusedLoop{},
		"join loop":        newFusedJoinLoop(),
		"panicking Unwrap": unwrapPanic{"boom"},
		"zero warning":     UnknownCharsetError{},
	} {
		t.Run(name, func(t *testing.T) {
			opts := func() *ParseOptions {
				return &ParseOptions{DiagnosticSink: stopOnCharset(fail)}
			}

			session, e, err := Parse(strings.NewReader(hostileCharsetTree), opts())
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			_, err = e.MultipartReader().NextPart()
			if stop := session.fatalErr(); stop == nil || err != stop || !IsFatal(err) {
				t.Errorf("NextPart() = %v, want the identical session stop %v", err, stop)
			}

			session, e, _ = Parse(strings.NewReader(hostileCharsetTree), opts())
			err = e.Walk(func([]int, *Entity, error) error { return nil })
			if stop := session.fatalErr(); stop == nil || err != stop {
				t.Errorf("Walk() = %v, want the identical session stop %v", err, stop)
			}
			if blown(fail) {
				t.Error("the fuse blew: the sink's error was walked without a bound")
			}
		})
	}
}

// stopOnCharset is a sink that stops the parse with fail at the first unknown
// charset, and lets every other diagnostic through.
func stopOnCharset(fail error) DiagnosticSink {
	return DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
		if d.Code == CodeCharsetUnknown {
			return fail
		}
		return nil
	})
}

// probeEnv selects the body of TestHostileErrorGraphs_subprocess.
const probeEnv = "EMLKIT_ROB120007_PROBE"

// endlessCycle unwraps to itself, with no fuse.
type endlessCycle struct{}

func (*endlessCycle) Error() string   { return "endless" }
func (e *endlessCycle) Unwrap() error { return e }

// endlessJoin unwraps to itself as a join, with no fuse. Its children are
// built once, so no step of the cycle allocates.
type endlessJoin struct{ kids []error }

func newEndlessJoin() *endlessJoin {
	e := &endlessJoin{}
	e.kids = []error{nil, e}
	return e
}

func (*endlessJoin) Error() string     { return "endless join" }
func (e *endlessJoin) Unwrap() []error { return e.kids }

// TestHostileErrorGraphs_subprocess is an independent reproducer of the
// reported hang, with cycles that never end rather than fused ones, so it runs
// in a child process the parent kills at a deadline: a regression is a failure
// here, not a hung test binary. No step of either cycle allocates, and the
// child caps its own stack, so a regression to the standard library's
// recursive walk of a join crashes the child at that cap rather than growing a
// stack toward the runtime's default gigabyte; otherwise the child holds the
// memory of a parse of a few hundred bytes.
func TestHostileErrorGraphs_subprocess(t *testing.T) {
	if os.Getenv(probeEnv) == "1" {
		runHostileProbe()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostileErrorGraphs_subprocess$", "-test.count=1", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), probeEnv+"=1")
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the probe did not finish before its deadline: a classification hangs\n%s", out.String())
	}
	if err != nil || !strings.Contains(out.String(), "probe: ok") {
		t.Fatalf("probe failed: %v\n%s", err, out.String())
	}
}

// runHostileProbe runs in the child. It prints "probe: ok" only if every step
// returned with the expected classification.
func runHostileProbe() {
	debug.SetMaxStack(16 << 20)
	fail := func(format string, args ...any) {
		fmt.Printf("probe: FAIL "+format+"\n", args...)
		os.Exit(3)
	}
	const tree = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nX: 1\r\n"

	for _, cycle := range []error{&endlessCycle{}, newEndlessJoin()} {
		for _, p := range []func(error) bool{IsFatal, IsUnknownCharset, IsUnknownEncoding} {
			if p(cycle) {
				fail("a predicate matched %v", cycle)
			}
		}
		if !IsUnknownCharset(UnknownCharsetError{cycle}) || IsFatal(UnknownCharsetError{cycle}) {
			fail("warning of %v misclassified", cycle)
		}

		// The reported hang: a child header's source failure.
		_, e, err := Parse(&failAfter{strings.NewReader(tree), cycle}, nil)
		if err != nil {
			fail("Parse = %v", err)
		}
		if _, err := e.MultipartReader().NextPart(); err != cycle {
			fail("NextPart = %v, want the source failure", err)
		}
		_, e, _ = Parse(&failAfter{strings.NewReader(tree), cycle}, nil)
		if err := e.Walk(func([]int, *Entity, error) error { return nil }); err != cycle {
			fail("Walk = %v, want the source failure", err)
		}

		// A CharsetReader failing with it: the warning and the part survive.
		old := CharsetReader
		CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, cycle }
		_, e, _ = Parse(strings.NewReader(hostileCharsetTree), nil)
		warned := 0
		err = e.Walk(func(_ []int, part *Entity, err error) error {
			if IsUnknownCharset(err) {
				warned++
			}
			return nil
		})
		CharsetReader = old
		if err != nil || warned != 1 {
			fail("Walk with a hostile CharsetReader = %v, %d warnings", err, warned)
		}

		// A DiagnosticSink stopping with it.
		CharsetReader = nil
		session, e, _ := Parse(strings.NewReader(hostileCharsetTree), &ParseOptions{DiagnosticSink: stopOnCharset(cycle)})
		err = e.Walk(func([]int, *Entity, error) error { return nil })
		CharsetReader = old
		if stop := session.fatalErr(); stop == nil || err != stop {
			fail("Walk with a hostile sink = %v, want the stop", err)
		}
	}
	fmt.Println("probe: ok")
}
