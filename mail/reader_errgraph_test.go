package mail_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-120-007 (issue #16): mail.Reader decides whether an error is a warning
// that comes with a usable part, or a failure, without walking anything a
// caller's component handed back. Every hostile error here is finite work: a
// cycle answers a bounded number of Unwrap calls and then reports no child, so
// a classification that walked one without a bound blows the fuse and fails
// rather than spinning.

const mailFuse = 10000

// fusedCycleErr unwraps to itself until its fuse blows.
type fusedCycleErr struct {
	calls int
	blown bool
}

func (*fusedCycleErr) Error() string { return "fused cycle" }
func (f *fusedCycleErr) Unwrap() error {
	f.calls++
	if f.calls > mailFuse {
		f.blown = true
		return nil
	}
	return f
}

// panicUnwrapErr panics when unwrapped.
type panicUnwrapErr struct{ v any }

func (panicUnwrapErr) Error() string   { return "panics when unwrapped" }
func (p panicUnwrapErr) Unwrap() error { panic(p.v) }

func fuseBlown(err error) bool {
	f, ok := err.(*fusedCycleErr)
	return ok && f.blown
}

// TestReader_hostileSourceFailure pins that a source failing inside a part's
// header with a cyclic or panicking error ends the reader with that failure,
// after the sibling before it, through both ways of opening a reader.
func TestReader_hostileSourceFailure(t *testing.T) {
	const tree = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n--B\r\nX: 1\r\n"

	for name, fail := range map[string]error{
		"cycle":             &fusedCycleErr{},
		"panicking Unwrap":  panicUnwrapErr{"boom"},
		"panic(nil) Unwrap": panicUnwrapErr{nil},
		"zero warning":      message.UnknownCharsetError{},
	} {
		for _, via := range []struct {
			name string
			open func(io.Reader) (*mail.Reader, error)
		}{
			{"CreateReader", mail.CreateReader},
			{"Parse+NewReader", func(r io.Reader) (*mail.Reader, error) {
				_, e, err := message.Parse(r, nil)
				if e == nil {
					return nil, err
				}
				return mail.NewReader(e), err
			}},
		} {
			t.Run(name+"/"+via.name, func(t *testing.T) {
				mr, err := via.open(&failingReader{s: strings.NewReader(tree), err: fail})
				if mr == nil || err != nil {
					t.Fatalf("opening = (%v, %v), want a reader", mr, err)
				}
				var parts []readPart
				mustNotPanic(t, func() { parts, err = readAll(t, mr) })
				if len(parts) != 1 || parts[0].body != "zero" {
					t.Errorf("parts = %v, want the first sibling", parts)
				}
				if err == nil || message.IsFatal(err) {
					t.Errorf("NextPart() = %v, want the ordinary failure", err)
				}
				if fuseBlown(fail) {
					t.Error("the fuse blew: the failure was walked without a bound")
				}
			})
		}
	}
}

// TestReader_hostileCharsetReaderWarning pins that a CharsetReader failing
// with a cyclic or panicking error still yields the part with its warning and
// its undecoded body, and the reader goes on to the sibling after it.
func TestReader_hostileCharsetReaderWarning(t *testing.T) {
	const tree = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=x-hostile\r\n\r\nraw one\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\ntwo\r\n--B--\r\n"

	for name, fail := range map[string]error{
		"cycle":            &fusedCycleErr{},
		"panicking Unwrap": panicUnwrapErr{"boom"},
	} {
		t.Run(name, func(t *testing.T) {
			old := message.CharsetReader
			t.Cleanup(func() { message.CharsetReader = old })
			message.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, fail }

			_, e, err := message.Parse(strings.NewReader(tree), nil)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			var parts []readPart
			mustNotPanic(t, func() { parts, err = readAll(t, mail.NewReader(e)) })
			if err != nil || len(parts) != 2 {
				t.Fatalf("readAll() = (%v, %v), want both parts", parts, err)
			}
			if parts[0].body != "raw one" || !message.IsUnknownCharset(parts[0].err) {
				t.Errorf("first part = %+v, want the undecoded body with the charset warning", parts[0])
			}
			if parts[1].body != "two" || parts[1].err != nil {
				t.Errorf("second part = %+v, want the sibling", parts[1])
			}
			if fuseBlown(fail) {
				t.Error("the fuse blew: the warning's cause was walked without a bound")
			}
		})
	}
}

// TestReader_hostileSinkStop pins that a DiagnosticSink stopping the parse
// with a cyclic error is reported as the parse's identical stop.
func TestReader_hostileSinkStop(t *testing.T) {
	old := message.CharsetReader
	t.Cleanup(func() { message.CharsetReader = old })
	message.CharsetReader = nil

	fail := &fusedCycleErr{}
	_, e, err := message.Parse(strings.NewReader(warnedTree(unknownCharset)), &message.ParseOptions{
		DiagnosticSink: message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
			if d.Code == message.CodeCharsetUnknown {
				return fail
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	parts, err := readAll(t, mail.NewReader(e))
	if stop := e.ParseStop(); stop == nil || err != stop || !errors.Is(stop, fail) {
		t.Errorf("readAll() = (%v, %v), want the identical stop %v", parts, err, stop)
	}
	if len(parts) != 1 || parts[0].body != "zero" {
		t.Errorf("parts = %v, want the sibling before the stop", parts)
	}
	if fail.blown {
		t.Error("the fuse blew: the sink's error was walked without a bound")
	}
}

// TestReader_charsetReaderFailsWithAFatal pins that mail.Reader decides a
// warning by the error it is handed with the part, not by searching it. A
// CharsetReader failing with a genuine FatalError from another parse makes a
// warning whose cause message.IsFatal finds; it is still a warning about this
// part, so the part, its undecoded body and the siblings after it are
// returned. Before ROB-120-007 the reader rejected it and ended with no part.
func TestReader_charsetReaderFailsWithAFatal(t *testing.T) {
	old := message.CharsetReader
	t.Cleanup(func() { message.CharsetReader = old })

	// A genuine stop of another parse: a sink refusing its unknown charset.
	message.CharsetReader = nil
	_, oe, other := message.Parse(strings.NewReader(unknownCharset+"\r\nbody"), &message.ParseOptions{
		DiagnosticSink: message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
			if d.Code == message.CodeCharsetUnknown {
				return errors.New("the other parse was stopped")
			}
			return nil
		}),
	})
	if oe == nil || oe.ParseStop() == nil || other != oe.ParseStop() {
		t.Fatalf("the other parse returned (%v, %v), want an entity and its stop", oe, other)
	}

	message.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, other }

	t.Run("CreateReader root", func(t *testing.T) {
		mr, err := mail.CreateReader(strings.NewReader(unknownCharset + "\r\nraw"))
		if mr == nil || !message.IsUnknownCharset(err) || !message.IsFatal(err) {
			t.Fatalf("CreateReader() = (%v, %v), want a reader and a warning whose cause is fatal", mr, err)
		}
		defer mr.Close()
		// The warning was reported by CreateReader; the single part is the
		// root itself, as the bytes arrived.
		parts, err := readAll(t, mr)
		if err != nil || len(parts) != 1 || parts[0].body != "raw" || parts[0].err != nil {
			t.Errorf("readAll() = (%+v, %v), want the one undecoded root part", parts, err)
		}
	})

	t.Run("Parse+NewReader child", func(t *testing.T) {
		_, e, err := message.Parse(strings.NewReader(warnedTree(unknownCharset)), nil)
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		parts, err := readAll(t, mail.NewReader(e))
		if err != nil || len(parts) != 3 {
			t.Fatalf("readAll() = (%+v, %v), want all three parts", parts, err)
		}
		if parts[0].body != "zero" || parts[0].err != nil {
			t.Errorf("first part = %+v, want the sibling before", parts[0])
		}
		if parts[1].body != "raw=XX" || !message.IsUnknownCharset(parts[1].err) || !message.IsFatal(parts[1].err) {
			t.Errorf("second part = %+v, want the undecoded body with the warning", parts[1])
		}
		if parts[2].body != "two" || parts[2].err != nil {
			t.Errorf("third part = %+v, want the sibling after", parts[2])
		}
		if e.ParseStop() != nil {
			t.Errorf("this parse was stopped: %v", e.ParseStop())
		}
	})
}
