package mail_test

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-100-006 (issue #1): an unknown transfer encoding and an unknown charset
// are both warnings that come with a usable result, at every layer. The mail
// reader must surface either one together with the Reader or Part it describes
// and keep iterating, while every other error — structural, I/O, a resource
// budget, or a stop the caller's sink asked for — still ends it.

// warnedTree is a multipart message whose middle child is the given header,
// between two readable siblings.
func warnedTree(middle string) string {
	return "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n" +
		"--B\r\n" + middle + "\r\nraw=XX\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\ndHdv\r\n" +
		"--B--\r\n"
}

const (
	unknownCTE     = "Content-Type: text/plain\r\nContent-Transfer-Encoding: x-unknown\r\n"
	unknownCharset = "Content-Type: text/plain; charset=x-made-up\r\n"
	unknownBoth    = "Content-Type: text/plain; charset=x-made-up\r\nContent-Transfer-Encoding: x-unknown\r\n"
)

// readPart is one NextPart result, with its body read whole.
type readPart struct {
	inline bool
	body   string
	err    error
}

// readAll iterates mr to io.EOF or to the first error that comes without a part.
func readAll(t *testing.T, mr *mail.Reader) ([]readPart, error) {
	t.Helper()
	var out []readPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out, nil
		}
		if p == nil {
			if err == nil {
				t.Fatal("NextPart() = (nil, nil)")
			}
			return out, err
		}
		b, readErr := io.ReadAll(p.Body)
		if readErr != nil {
			t.Fatalf("reading a part body = %v", readErr)
		}
		_, inline := p.Header.(*mail.InlineHeader)
		out = append(out, readPart{inline: inline, body: string(b), err: err})
	}
}

func TestCreateReader_rootUnknownTransferEncodingKeepsRawBody(t *testing.T) {
	const raw = "=E9 not decoded\r\n"
	for _, tc := range []struct {
		name    string
		header  string
		warning func(error) bool
	}{
		{"unknown encoding", unknownCTE, message.IsUnknownEncoding},
		{"unknown charset", unknownCharset, message.IsUnknownCharset},
		// Both stages unknown keeps the existing report: the charset.
		{"both unknown", unknownBoth, message.IsUnknownCharset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, err := mail.CreateReader(strings.NewReader("Subject: hi\r\n" + tc.header + "\r\n" + raw))
			if mr == nil {
				t.Fatalf("CreateReader() returned no Reader (err = %v), want a usable one", err)
			}
			defer mr.Close()
			if !tc.warning(err) || message.IsFatal(err) {
				t.Fatalf("CreateReader() err = %v, want the %s warning", err, tc.name)
			}
			if got := mr.Header.Get("Subject"); got != "hi" {
				t.Errorf("Header.Get(Subject) = %q, want %q", got, "hi")
			}

			parts, err := readAll(t, mr)
			if err != nil {
				t.Fatalf("NextPart() = %v", err)
			}
			// The warning was reported by CreateReader; the single part is the
			// root itself, whose body is the bytes as they arrived.
			if len(parts) != 1 || !parts[0].inline || parts[0].body != raw || parts[0].err != nil {
				t.Errorf("parts = %+v, want the one inline root part with body %q", parts, raw)
			}
		})
	}
}

func TestReader_warnedChildBetweenReadableSiblings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		middle  string
		warning func(error) bool
	}{
		{"unknown encoding", unknownCTE, message.IsUnknownEncoding},
		{"unknown charset", unknownCharset, message.IsUnknownCharset},
		{"both unknown", unknownBoth, message.IsUnknownCharset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, err := mail.CreateReader(strings.NewReader(warnedTree(tc.middle)))
			if err != nil {
				t.Fatalf("CreateReader() = %v", err)
			}
			defer mr.Close()

			parts, err := readAll(t, mr)
			if err != nil {
				t.Fatalf("iteration ended with %v, want every sibling", err)
			}
			if len(parts) != 3 {
				t.Fatalf("got %d parts %+v, want 3", len(parts), parts)
			}
			if parts[0].err != nil || parts[0].body != "zero" || !parts[0].inline {
				t.Errorf("part 0 = %+v, want the readable inline sibling", parts[0])
			}
			if !tc.warning(parts[1].err) || message.IsFatal(parts[1].err) {
				t.Errorf("part 1 err = %v, want the %s warning", parts[1].err, tc.name)
			}
			if parts[1].body != "raw=XX" {
				t.Errorf("part 1 body = %q, want the raw bytes %q", parts[1].body, "raw=XX")
			}
			if parts[2].err != nil || parts[2].body != "two" || parts[2].inline {
				t.Errorf("part 2 = %+v, want the decoded attachment after the warned part", parts[2])
			}
		})
	}
}

func TestReader_parseSessionKeepsBothDiagnostics(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n" +
		"--B\r\n" + unknownCTE + "\r\none\r\n" +
		"--B\r\n" + unknownCharset + "\r\ntwo\r\n" +
		"--B--\r\n"

	session, e, err := message.Parse(strings.NewReader(text), nil)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	mr := mail.NewReader(e)
	defer mr.Close()

	parts, err := readAll(t, mr)
	if err != nil {
		t.Fatalf("iteration ended with %v", err)
	}
	var bodies []string
	for _, p := range parts {
		bodies = append(bodies, p.body)
	}
	if want := []string{"zero", "one", "two"}; !reflect.DeepEqual(bodies, want) {
		t.Errorf("bodies = %v, want %v", bodies, want)
	}
	if !message.IsUnknownEncoding(parts[1].err) || !message.IsUnknownCharset(parts[2].err) {
		t.Errorf("warnings = %v, %v; want unknown encoding then unknown charset", parts[1].err, parts[2].err)
	}

	diags := session.Diagnostics()
	type seen struct {
		code message.Code
		path []int
	}
	var got []seen
	for _, d := range diags {
		got = append(got, seen{d.Code, d.Path})
	}
	want := []seen{{message.CodeCTEUnknown, []int{1}}, {message.CodeCharsetUnknown, []int{2}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diagnostics() = %+v, want %+v", got, want)
	}
}

func TestReader_stopWrappingAWarningEndsTraversal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		middle  string
		code    message.Code
		warning func(error) bool
	}{
		{"unknown encoding", unknownCTE, message.CodeCTEUnknown, message.IsUnknownEncoding},
		{"unknown charset", unknownCharset, message.CodeCharsetUnknown, message.IsUnknownCharset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e, err := message.Parse(strings.NewReader(warnedTree(tc.middle)), &message.ParseOptions{
				// The sink stops the parse with the warning's own cause, so the stop
				// answers to the warning predicates too.
				DiagnosticSink: message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
					return d.Cause
				}),
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			mr := mail.NewReader(e)
			defer mr.Close()

			parts, stop := readAll(t, mr)
			if !message.IsFatal(stop) || !tc.warning(stop) {
				t.Fatalf("iteration ended with %v, want a FatalError wrapping the %s warning", stop, tc.name)
			}
			if len(parts) != 1 || parts[0].body != "zero" {
				t.Errorf("parts = %+v, want only the sibling before the stop", parts)
			}
			for i := 0; i < 2; i++ {
				if p, err := mr.NextPart(); p != nil || err != stop {
					t.Errorf("NextPart() after the stop = (%v, %v), want (nil, the identical stop)", p, err)
				}
			}
		})
	}
}

func TestReader_resourceLimitStaysFatal(t *testing.T) {
	limits := message.Limits{MaxPartCount: 1}
	session, e, err := message.Parse(strings.NewReader(warnedTree(unknownCTE)), &message.ParseOptions{Limits: limits})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	mr := mail.NewReader(e)
	defer mr.Close()

	parts, stop := readAll(t, mr)
	if !message.IsFatal(stop) {
		t.Fatalf("iteration ended with %v, want the part-count FatalError", stop)
	}
	if len(parts) != 1 || parts[0].body != "zero" {
		t.Errorf("parts = %+v, want only the part within the budget", parts)
	}
	if p, err := mr.NextPart(); p != nil || err != stop {
		t.Errorf("NextPart() after the refusal = (%v, %v), want (nil, the identical error)", p, err)
	}
	if d := session.Diagnostics(); len(d) != 1 || d[0].Code != message.CodeLimitPartCount {
		t.Errorf("Diagnostics() = %v, want one limit.part_count", d)
	}
}

// failingReader yields s and then fails with err.
type failingReader struct {
	s   *strings.Reader
	err error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.s.Len() == 0 {
		return 0, r.err
	}
	return r.s.Read(p)
}

func TestReader_unrelatedErrorsStayErrors(t *testing.T) {
	broken := errors.New("disk on fire")

	t.Run("header read failure", func(t *testing.T) {
		mr, err := mail.CreateReader(&failingReader{s: strings.NewReader("Subject: hi\r\n"), err: broken})
		if mr != nil || !errors.Is(err, broken) {
			t.Errorf("CreateReader() = (%v, %v), want (nil, the read error)", mr, err)
		}
	})

	t.Run("body read failure", func(t *testing.T) {
		// The input fails inside the first part, before any further delimiter.
		text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nzero"
		mr, err := mail.CreateReader(&failingReader{s: strings.NewReader(text), err: broken})
		if err != nil {
			t.Fatalf("CreateReader() = %v", err)
		}
		defer mr.Close()
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("NextPart() = %v, want the first part", err)
		}
		if _, err := io.ReadAll(p.Body); !errors.Is(err, broken) {
			t.Errorf("reading the body = %v, want the read error", err)
		}
		p, err = mr.NextPart()
		if p != nil || err == nil || message.IsUnknownEncoding(err) || message.IsUnknownCharset(err) {
			t.Errorf("NextPart() = (%v, %v), want no part and an ordinary error", p, err)
		}
	})
}

// realWarnings returns the two decoding warnings exactly as message.Read
// produces them, so that the errors below are the parser's own values rather
// than hand-built look-alikes.
func realWarnings(t *testing.T) map[string]error {
	t.Helper()
	out := map[string]error{}
	for name, header := range map[string]string{
		"unknown encoding": unknownCTE,
		"unknown charset":  unknownCharset,
	} {
		e, err := message.Read(strings.NewReader(header + "\r\nbody"))
		if e == nil || !(message.IsUnknownEncoding(err) || message.IsUnknownCharset(err)) {
			t.Fatalf("message.Read(%s) = (%v, %v), want a usable entity and the warning", name, e, err)
		}
		out[name] = err
	}
	return out
}

// TestReader_warningWithoutAResultIsAnError pins the non-nil guard of
// recoverable, which the fatal guard does not cover. An input reader that fails
// with — or with an error wrapping — a real decoding warning while a header is
// being read leaves no entity at all, so the predicate matches an error that
// describes nothing usable. CreateReader and NextPart must return no result and
// that error, not build a Reader or Part from a nil entity.
func TestReader_warningWithoutAResultIsAnError(t *testing.T) {
	for name, warning := range realWarnings(t) {
		for _, shape := range []struct {
			name string
			err  error
		}{
			{"the warning itself", warning},
			{"wrapping the warning", fmt.Errorf("read failed: %w", warning)},
		} {
			// Precondition: the injected error really answers to a warning
			// predicate, and is not fatal, so only the nil guard can reject it.
			if !(message.IsUnknownEncoding(shape.err) || message.IsUnknownCharset(shape.err)) || message.IsFatal(shape.err) {
				t.Fatalf("%s/%s: %v is not a non-fatal warning", name, shape.name, shape.err)
			}

			t.Run(name+"/"+shape.name+"/root header", func(t *testing.T) {
				src := &failingReader{s: strings.NewReader("Subject: hi\r\nContent-Type: text/pl"), err: shape.err}
				var (
					mr  *mail.Reader
					err error
				)
				mustNotPanic(t, func() { mr, err = mail.CreateReader(src) })
				if mr != nil {
					t.Errorf("CreateReader() returned a Reader, want none: no entity was read")
				}
				if !errors.Is(err, shape.err) {
					t.Errorf("CreateReader() err = %v, want the reader's error %v", err, shape.err)
				}
			})

			child := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
				"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n" +
				"--B\r\nContent-Type: text/pl"
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
				t.Run(name+"/"+shape.name+"/child header via "+via.name, func(t *testing.T) {
					mr, err := via.open(&failingReader{s: strings.NewReader(child), err: shape.err})
					if mr == nil || err != nil {
						t.Fatalf("opening = (%v, %v), want a reader for the sound root", mr, err)
					}
					defer mr.Close()

					p, err := mr.NextPart()
					if err != nil {
						t.Fatalf("NextPart() = %v, want the first part", err)
					}
					if b, _ := io.ReadAll(p.Body); string(b) != "zero" {
						t.Errorf("first part = %q, want %q", b, "zero")
					}

					mustNotPanic(t, func() { p, err = mr.NextPart() })
					if p != nil {
						t.Errorf("NextPart() returned a Part, want none: the child's header was never read")
					}
					if !errors.Is(err, shape.err) {
						t.Errorf("NextPart() err = %v, want the reader's error %v", err, shape.err)
					}
				})
			}
		}
	}
}

// mustNotPanic runs f and fails the test, rather than the binary, if it panics.
func mustNotPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panicked: %v", p)
		}
	}()
	f()
}
