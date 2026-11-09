package textproto

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// These tests pin how MultipartReader.NextPart classifies the errors it
// returns (ROB-120-007): which end the iteration, by what test, and that no
// error a source fails with can make that test hang, panic or lie.
//
// Every hostile error here is finite work whatever NextPart does: a cycle
// answers a bounded number of Unwrap calls, then reports no child. A walk that
// regressed to an unbounded one fails on the fuse rather than spinning.

const errgraphFuse = 10000

// fusedSelf unwraps to itself until its fuse blows.
type fusedSelf struct {
	calls int
	blown bool
}

func (*fusedSelf) Error() string { return "fused self cycle" }
func (f *fusedSelf) Unwrap() error {
	f.calls++
	if f.calls > errgraphFuse {
		f.blown = true
		return nil
	}
	return f
}

// fusedJoinSelf is the same cycle through Unwrap() []error.
type fusedJoinSelf struct {
	kids  []error
	calls int
	blown bool
}

func newFusedJoinSelf() *fusedJoinSelf {
	f := &fusedJoinSelf{}
	f.kids = []error{nil, f}
	return f
}

func (*fusedJoinSelf) Error() string { return "fused join cycle" }
func (f *fusedJoinSelf) Unwrap() []error {
	f.calls++
	if f.calls > errgraphFuse {
		f.blown = true
		return nil
	}
	return f.kids
}

// eofLiar claims to be io.EOF through Is, and counts every time it is asked.
type eofLiar struct{ asked *int }

func (eofLiar) Error() string { return "claims to be EOF" }
func (l eofLiar) Is(target error) bool {
	*l.asked++
	return target == io.EOF || target == io.ErrUnexpectedEOF
}

// unwrapPanics panics when unwrapped, with v.
type unwrapPanics struct{ v any }

func (unwrapPanics) Error() string   { return "unwrap panics" }
func (u unwrapPanics) Unwrap() error { panic(u.v) }

// multiPanics panics when unwrapped as a join.
type multiPanics struct{}

func (multiPanics) Error() string   { return "multi panics" }
func (multiPanics) Unwrap() []error { panic("multi") }

// nextCalls runs NextPart calls calls times on a reader that delivers prefix
// and then fails with fail, and returns every error. A part it is handed is
// left unread; the next call drains it.
func nextCalls(t *testing.T, prefix string, fail error, budget HeaderLimits, calls int) []error {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("NextPart panicked: %v", p)
		}
	}()
	mr := NewMultipartReader(&errorAfterReader{s: prefix, n: len(prefix), err: fail}, "B")
	mr.SetHeaderLimits(budget)
	errs := make([]error, calls)
	for i := range errs {
		_, errs[i] = mr.NextPart()
	}
	return errs
}

// failureSites are where a failure reaches the reader: at the first framing line,
// and inside a part's header block, after one complete field.
var failureSites = map[string]string{
	"framing line": "",
	"part header":  "--B\r\nX: 1\r\n",
}

// endsAfterFailure reports whether errs reports a failure that is not io.EOF
// and then io.EOF itself, which is what an iteration that ended on an
// EOF-class failure looks like. A bare io.EOF inside a header block is a header
// that ends there, so the failure may come one call later, out of the framing
// line after that part.
func endsAfterFailure(errs []error) bool {
	for i := 1; i < len(errs); i++ {
		if errs[i] == io.EOF {
			return errs[i-1] != nil && errs[i-1] != io.EOF
		}
	}
	return false
}

// TestMultipartReader_eofClassThroughWrapping pins the compatibility the
// bounded walk keeps: io.EOF and io.ErrUnexpectedEOF still end the iteration
// when the source wraps them with fmt.Errorf, joins them, or wraps them in an
// error of its own, at a framing line and in a part's header block alike.
func TestMultipartReader_eofClassThroughWrapping(t *testing.T) {
	for name, fail := range map[string]error{
		"EOF":                     io.EOF,
		"unexpected EOF":          io.ErrUnexpectedEOF,
		"fmt-wrapped EOF":         fmt.Errorf("read: %w", io.EOF),
		"fmt-wrapped unexpected":  fmt.Errorf("read: %w", io.ErrUnexpectedEOF),
		"joined unexpected":       errors.Join(errors.New("other"), io.ErrUnexpectedEOF),
		"custom-wrapped EOF":      unwrapsTo{io.EOF},
		"custom join with nil":    multiUnwrap{[]error{nil, io.ErrUnexpectedEOF}},
		"beside a panicking node": errors.Join(unwrapPanics{"x"}, io.EOF),
		"beside a cycle":          errors.Join(&fusedSelf{}, io.ErrUnexpectedEOF),
	} {
		for at, prefix := range failureSites {
			if errs := nextCalls(t, prefix, fail, HeaderLimits{}, 4); !endsAfterFailure(errs) {
				t.Errorf("%s at %s: NextPart() = %v, want the failure and then io.EOF", name, at, errs)
			}
		}
	}
}

// TestMultipartReader_notEOFClass pins what does not end the iteration: an
// ordinary failure, an error that claims to be io.EOF only through its Is
// method (which is never asked), one that reaches io.EOF only past the walk's
// bounds, and the hostile shapes a source may fail with, none of which may
// hang or panic NextPart.
func TestMultipartReader_notEOFClass(t *testing.T) {
	asked := 0
	self, join := &fusedSelf{}, newFusedJoinSelf()
	for name, fail := range map[string]error{
		"ordinary":            errors.New("disk on fire"),
		"Is lies":             eofLiar{&asked},
		"wrapped Is lies":     fmt.Errorf("read: %w", eofLiar{&asked}),
		"match-all":           matchAllIsError{},
		"self cycle":          self,
		"join cycle":          join,
		"panicking Unwrap":    unwrapPanics{"boom"},
		"panic(nil) Unwrap":   unwrapPanics{nil},
		"panicking join":      multiPanics{},
		"typed nil":           (*HeaderLimitError)(nil),
		"EOF past the bounds": deepChain(300, io.EOF),
	} {
		for at, prefix := range failureSites {
			for i, err := range nextCalls(t, prefix, fail, HeaderLimits{}, 3) {
				if err == nil || err == io.EOF {
					t.Errorf("%s at %s: call %d returned %v, want the failure every time", name, at, i, err)
				}
			}
		}
	}
	if asked != 0 {
		t.Errorf("an Is method was asked %d times", asked)
	}
	if self.blown || join.blown {
		t.Error("a cycle's fuse blew: the walk was not bounded")
	}
}

// layer is a wrapper whose Error does not recurse, so a deep chain of them is
// cheap to build and to print.
type layer struct{ inner error }

func (layer) Error() string   { return "layer" }
func (l layer) Unwrap() error { return l.inner }

// deepChain wraps err in depth layers.
func deepChain(depth int, err error) error {
	for range depth {
		err = layer{err}
	}
	return err
}

// TestMultipartReader_ownRefusalByIdentity pins that the refusals this reader
// makes end the iteration, and that one an input hands back does not, whether
// it is forged or genuine but made elsewhere: the reader recognizes its own by
// identity, not by type and mark.
func TestMultipartReader_ownRefusalByIdentity(t *testing.T) {
	t.Run("own header refusal", func(t *testing.T) {
		mr := NewMultipartReader(strings.NewReader("--B\r\nX: "+strings.Repeat("v", 64)+"\r\n\r\nbody\r\n--B--\r\n"), "B")
		mr.SetHeaderLimits(HeaderLimits{MaxBytes: 16})
		_, err := mr.NextPart()
		if _, ok := HeaderLimit(err); !ok {
			t.Fatalf("NextPart() = %v, want the reader's header refusal", err)
		}
		if _, err := mr.NextPart(); err != io.EOF {
			t.Errorf("after a refusal NextPart() = %v, want io.EOF", err)
		}
	})

	t.Run("own line refusal", func(t *testing.T) {
		mr := NewMultipartReader(strings.NewReader(strings.Repeat("p", 64)+"\r\n--B\r\n\r\nbody\r\n--B--\r\n"), "B")
		mr.SetHeaderLimits(HeaderLimits{MaxLineBytes: 16})
		_, err := mr.NextPart()
		if _, ok := LineLimit(err); !ok {
			t.Fatalf("NextPart() = %v, want the reader's line refusal", err)
		}
		if _, err := mr.NextPart(); err != io.EOF {
			t.Errorf("after a refusal NextPart() = %v, want io.EOF", err)
		}
	})

	_, otherReaders := ReadHeaderLimits(bufio.NewReader(strings.NewReader("Long: "+strings.Repeat("v", 64)+"\r\n\r\n")), HeaderLimits{MaxBytes: 8})
	if _, ok := HeaderLimit(otherReaders); !ok {
		t.Fatalf("no genuine refusal to replay: %v", otherReaders)
	}
	for name, fail := range map[string]error{
		"replayed header refusal":  otherReaders,
		"replayed line refusal":    newLineLimitError(4, 2),
		"wrapped replayed refusal": fmt.Errorf("x: %w", otherReaders),
		"forged header refusal":    &HeaderLimitError{},
		"forged line refusal":      &LineLimitError{},
		"typed nil line refusal":   (*LineLimitError)(nil),
		"joined replayed refusals": errors.Join(otherReaders, newLineLimitError(4, 2)),
	} {
		for _, budget := range []HeaderLimits{{}, {MaxBytes: 1 << 10, MaxLineBytes: 1 << 10}} {
			for at, prefix := range failureSites {
				for i, err := range nextCalls(t, prefix, fail, budget, 3) {
					if err == nil || err == io.EOF {
						t.Errorf("%s at %s under %+v: call %d returned %v, want the failure every time", name, at, budget, i, err)
					}
				}
			}
		}
	}
}
