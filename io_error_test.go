package message

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Ordinary failures of the input (ROB-010-006): bytes returned with an error
// are delivered, the input's first error is final under Parse, and the failure
// reaches the caller where the parse reaches it — never lost, never turned
// into malformation or a clean end, never a stop. Every source here is finite
// and capped; the hostile-error cases run in a subprocess with a deadline.

var errSourceIO = errors.New("test source: input/output error")

var errIOCap = errors.New("test source: call cap exceeded")

// An ioSource delivers data in reads of at most chunk bytes until cut, then
// fails with err, together with the last bytes when withData or on a read of
// its own otherwise. A sticky source keeps failing; a resuming one fails once
// and then delivers the rest, as io.Reader allows. readsAfter counts the reads
// made after the failure was returned; under Parse it must stay zero.
type ioSource struct {
	data             string
	cut, chunk       int
	err              error
	withData, sticky bool

	pos, calls, readsAfter int
	failed                 bool
}

func (s *ioSource) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > 4*len(s.data)+64 {
		return 0, errIOCap
	}
	if s.failed {
		s.readsAfter++
		if s.sticky {
			return 0, s.err
		}
		if s.pos >= len(s.data) {
			return 0, io.EOF
		}
		n := copy(p, s.data[s.pos:min(len(s.data), s.pos+s.chunk)])
		s.pos += n
		return n, nil
	}
	if s.pos >= s.cut {
		s.failed = true
		return 0, s.err
	}
	n := copy(p, s.data[s.pos:min(s.cut, s.pos+s.chunk)])
	s.pos += n
	if s.pos == s.cut && s.withData {
		s.failed = true
		return n, s.err
	}
	return n, nil
}

// TestParse_sourceErrorMatrix fails the input at points of the root header,
// root bodies of three encodings, and the framing, header, body and epilogue
// of a multipart, across chunkings, with the failure delivered with the bytes
// or after them, from sticky and resuming sources, under every profile with
// default and disabled limits. The input is never read after its failure; a
// cut root header yields no entity and the identical error; a root body ends
// with the identical error after a prefix of its bytes; a part, or the
// iteration, ends with an error holding it; a failure after the closing
// delimiter, which nothing reads, is not reported; nothing is diagnosed and
// ParseStop stays nil; and a resuming source gives what a sticky one gives.
func TestParse_sourceErrorMatrix(t *testing.T) {
	runSourceErrorMatrix(t, errSourceIO, progressProfiles)
}

const bufferFullEnv = "EMLKIT_IO_BUFFER_FULL"

// TestParse_sourceBufferFull runs the same matrix with a source that fails
// with bufio.ErrBufferFull itself, which an io.Reader may return and which the
// buffered readers also use for a line longer than their buffer. Before the
// header and framing scanners told the two apart, a scanner took the source's
// failure for a full buffer and read on with no bytes for ever, without calling
// the source again — so no call cap could stop it; the matrix runs in a
// subprocess with a deadline.
func TestParse_sourceBufferFull(t *testing.T) {
	if os.Getenv(bufferFullEnv) != "" {
		runSourceErrorMatrix(t, bufio.ErrBufferFull, []Profile{ProfileTolerant})
		return
	}
	runTestInSubprocess(t, "^TestParse_sourceBufferFull$", bufferFullEnv, 120*time.Second)
}

// runTestInSubprocess runs the test named by pattern in a child process with
// env set, and fails unless it passes before the deadline.
func runTestInSubprocess(t *testing.T, pattern, env string, deadline time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+pattern, "-test.count=1")
	cmd.Env = append(os.Environ(), env+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("subprocess = %v, deadline %v\n%s", err, ctx.Err(), out)
	}
}

func runSourceErrorMatrix(t *testing.T, fail error, profiles []Profile) {
	plain := "Subject: one\r\nContent-Type: text/plain\r\n\r\nhello body line\r\nsecond line\r\n"
	b64 := "Content-Transfer-Encoding: base64\r\n\r\nZGVjb2RlZCBiYXNlNjQgcGF5bG9hZCB0ZXh0Cg==\r\n"
	qp := "Content-Transfer-Encoding: quoted-printable\r\n\r\nhello =3D world\r\nline two\r\n"
	multi := "Content-Type: multipart/mixed; boundary=B\r\n\r\npre\r\n--B\r\nContent-Type: text/plain\r\n\r\nfirst body\r\n" +
		"--B\r\nContent-Transfer-Encoding: base64\r\n\r\nc2Vjb25k\r\n--B--\r\nepilogue\r\n"
	end := func(s string) int { return strings.Index(s, "\r\n\r\n") + 4 }
	at := func(s, sub string, d int) int { return strings.Index(s, sub) + d }
	type message struct {
		name, text string
		cuts       []int
		leaf       bool
	}
	messages := []message{
		{"plain", plain, []int{0, 5, 14, end(plain), end(plain) + 7, len(plain)}, true},
		{"base64", b64, []int{3, end(b64), end(b64) + 6, end(b64) + 9, len(b64)}, true},
		// Not between the two hex digits of an escape: a cut escape is kept
		// literally, which is no prefix of the decoded body.
		{"qp", qp, []int{end(qp), at(qp, "=3D", 1), at(qp, "line", 2), len(qp)}, true},
		{"multipart", multi, []int{0, 10, end(multi), at(multi, "Content-Type: text", 6), at(multi, "first body", 3),
			at(multi, "c2Vjb25k", 4), at(multi, "epilogue", 3)}, false},
	}
	epilogue := at(multi, "epilogue", 0)

	for _, m := range messages {
		headerEnd := end(m.text)
		for lname, limits := range map[string]Limits{"default": {}, "disabled": allDisabled()} {
			for _, profile := range profiles {
				opts := &ParseOptions{Profile: profile, Limits: limits}
				base := consumeStall(strings.NewReader(m.text), opts)
				for _, cut := range m.cuts {
					for _, chunk := range []int{1, 7, 4096} {
						for _, withData := range []bool{false, true} {
							results := map[bool]string{}
							for _, sticky := range []bool{true, false} {
								name := fmt.Sprintf("%s/%s/%v/cut %d/chunk %d/with %v/sticky %v", m.name, lname, profile, cut, chunk, withData, sticky)
								src := &ioSource{data: m.text, cut: cut, chunk: chunk, err: fail, withData: withData, sticky: sticky}
								got := consumeStall(src, opts)
								results[sticky] = got.summary() + " " + got.errs()
								checkSourceError(t, name, got, base, src, fail, cut, headerEnd, m.leaf, !m.leaf && cut >= epilogue)
							}
							if results[true] != results[false] {
								t.Errorf("%s/%s/%v/cut %d/chunk %d/with %v: resuming %s, failing %s",
									m.name, lname, profile, cut, chunk, withData, results[false], results[true])
							}
						}
					}
				}
			}
		}
	}
}

func checkSourceError(t *testing.T, name string, got, base stallOutcome, src *ioSource, fail error, cut, headerEnd int, leaf, unread bool) {
	t.Helper()
	if src.readsAfter != 0 {
		t.Errorf("%s: %d reads after the input failed", name, src.readsAfter)
	}
	if got.session.Len() != 0 || got.session.fatalErr() != nil {
		t.Errorf("%s: diagnostics %v, stop %v; want neither for an input error", name, codesOfDiags(got.session.Diagnostics()), got.session.fatalErr())
	}
	if cut < headerEnd {
		if got.entity != nil || got.parseErr != fail {
			t.Errorf("%s: Parse() = (%v, %v), want no entity and the identical error", name, got.entity != nil, got.parseErr)
		}
		return
	}
	if got.entity == nil || got.parseErr != nil {
		t.Errorf("%s: Parse() = (%v, %v), want the entity and no error", name, got.entity != nil, got.parseErr)
		return
	}
	for i, b := range got.bodies {
		if i >= len(base.bodies) || !strings.HasPrefix(base.bodies[i], b) || (i < len(got.bodies)-1 && b != base.bodies[i]) {
			t.Errorf("%s: body %d = %q, want a prefix of %q, whole before the last", name, i, b, base.bodies)
		}
	}
	switch {
	case unread:
		if got.iterErr != io.EOF || got.summary() != base.summary() {
			t.Errorf("%s: %s / %s, want the whole message: a failure nothing reads is not reported", name, got.summary(), got.errs())
		}
	case leaf:
		if len(got.bodyErrs) != 1 || got.bodyErrs[0] != fail {
			t.Errorf("%s: body ended with %v, want the identical input error", name, got.bodyErrs)
		}
	default:
		if !errors.Is(got.iterErr, fail) {
			t.Errorf("%s: iteration ended with %v, want the input error", name, got.iterErr)
		}
		if last := len(got.bodyErrs) - 1; last >= 0 && got.bodyErrs[last] != nil && !errors.Is(got.bodyErrs[last], fail) {
			t.Errorf("%s: the last part ended with %v", name, got.bodyErrs[last])
		}
	}
}

// TestParse_sourceErrorLazyBoundary pins that a failure buffered past a
// complete header is reported by the body, after the bytes before it, and that
// Parse itself returns the entity with no error.
func TestParse_sourceErrorLazyBoundary(t *testing.T) {
	const text = "Subject: x\r\n\r\nbody bytes"
	for _, chunk := range []int{1, 7, 4096} {
		src := &ioSource{data: text, cut: len(text) - 3, chunk: chunk, err: errSourceIO, withData: true}
		s, e, err := Parse(src, nil)
		if err != nil || e == nil || e.Header.Get("Subject") != "x" {
			t.Fatalf("chunk %d: Parse() = %v", chunk, err)
		}
		b, berr := io.ReadAll(e.Body)
		if string(b) != "body by" || berr != errSourceIO || s.Len() != 0 || e.ParseStop() != nil {
			t.Errorf("chunk %d: body (%q, %v), want the bytes before the failure and the failure", chunk, b, berr)
		}
		if n, again := e.Body.Read(make([]byte, 4)); n != 0 || again != errSourceIO {
			t.Errorf("chunk %d: read again = (%d, %v)", chunk, n, again)
		}
	}
}

// TestRead_sourceErrorInHeader pins the header reader's change outside Parse:
// a line the input's failure cut is reported as that failure rather than as a
// malformed line, through Read as through Parse, while Read still does not
// make the input's error final.
func TestRead_sourceErrorInHeader(t *testing.T) {
	const text = "Subject: x\r\n\r\nbody"
	for _, cut := range []int{4, 9, 11} {
		_, err := Read(&ioSource{data: text, cut: cut, chunk: 4096, err: errSourceIO, sticky: true})
		if err != errSourceIO {
			t.Errorf("Read cut at %d: err = %v, want the input's failure", cut, err)
		}
		_, _, err = Parse(&ioSource{data: text, cut: cut, chunk: 4096, err: errSourceIO}, nil)
		if err != errSourceIO {
			t.Errorf("Parse cut at %d: err = %v, want the input's failure", cut, err)
		}
	}
}

// A hookDecoder is a scripted CharsetReader result over its input: it passes
// bytes through and, when its input fails, does what mode says.
type hookDecoder struct {
	in     io.Reader
	mode   string // "pass", "eof", "own", "swallow"
	failed bool
	calls  int
}

var errHookOwn = errors.New("hook: conversion failed")

func (d *hookDecoder) Read(p []byte) (int, error) {
	d.calls++
	if d.calls > 10000 {
		return 0, errIOCap
	}
	if d.failed {
		return 0, nil
	}
	n, err := d.in.Read(p)
	if err == nil || err == io.EOF {
		return n, err
	}
	switch d.mode {
	case "eof":
		return n, io.EOF
	case "own":
		return n, errHookOwn
	case "swallow":
		d.failed = true
		return n, nil
	}
	return n, err
}

// TestParse_sourceErrorThroughDecoders pins what a body reports when its input
// fails under decoders: the failure itself when they pass it on, mask it as
// io.EOF or swallow it (without a no-progress diagnostic); both when they add
// a report of their own — quoted-printable's wording of a cut soft break, a
// hook's own error — in one join with the input's failure first; and a
// decoder's own corruption alone when the failure never reached the body.
func TestParse_sourceErrorThroughDecoders(t *testing.T) {
	for _, mode := range []string{"pass", "eof", "own", "swallow"} {
		t.Run("hook "+mode, func(t *testing.T) {
			withCharsetReader(t, func(_ string, in io.Reader) (io.Reader, error) { return &hookDecoder{in: in, mode: mode}, nil })
			text := "Content-Type: text/plain; charset=x-hook\r\n\r\nbody text"
			got := consumeStall(&ioSource{data: text, cut: len(text) - 4, chunk: 4096, err: errSourceIO, withData: true}, nil)
			err := got.bodyErrs[0]
			if got.bodies[0] != "body " || got.session.Len() != 0 || got.entity.ParseStop() != nil {
				t.Fatalf("body %q, %v, stop %v", got.bodies[0], codesOfDiags(got.session.Diagnostics()), got.entity.ParseStop())
			}
			if mode == "own" {
				checkJoin(t, err, errSourceIO, errHookOwn)
				return
			}
			if err != errSourceIO {
				t.Errorf("body ended with %v, want the identical input error", err)
			}
		})
	}

	t.Run("quoted-printable soft break", func(t *testing.T) {
		text := "Content-Transfer-Encoding: quoted-printable\r\n\r\nfoo=\r"
		for _, sticky := range []bool{true, false} {
			// The tolerant decoder has no report of its own (ROB-070-008): the
			// failure is what the body ends with.
			got := consumeStall(&ioSource{data: text, cut: len(text), chunk: 4096, err: errSourceIO, sticky: sticky}, nil)
			if got.bodies[0] != "foo" || got.bodyErrs[0] != errSourceIO || got.session.Len() != 0 {
				t.Errorf("tolerant: (%q, %v, %v), want foo and the identical input error", got.bodies[0], got.bodyErrs[0], codesOfDiags(got.session.Diagnostics()))
			}
			// ProfileStrict keeps the standard library's decoder and its report.
			got = consumeStall(&ioSource{data: text, cut: len(text), chunk: 4096, err: errSourceIO, sticky: sticky}, &ParseOptions{Profile: ProfileStrict})
			if got.bodies[0] != "foo" {
				t.Errorf("body %q, want foo", got.bodies[0])
			}
			parts := checkJoin(t, got.bodyErrs[0], errSourceIO, nil)
			if len(parts) == 2 && !strings.Contains(parts[1].Error(), "quotedprintable") {
				t.Errorf("second error %v, want quoted-printable's own report", parts[1])
			}
		}
	})

	t.Run("quoted-printable soft break under a part", func(t *testing.T) {
		text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nfoo=\r"
		got := consumeStall(&ioSource{data: text + "\n--B--\r\n", cut: len(text), chunk: 4096, err: errSourceIO, sticky: true}, nil)
		if got.bodies[0] != "foo" || !errors.Is(got.bodyErrs[0], errSourceIO) || !errors.Is(got.iterErr, errSourceIO) {
			t.Errorf("part (%q, %v), iteration %v", got.bodies[0], got.bodyErrs[0], got.iterErr)
		}
	})

	t.Run("corruption the failure never reached", func(t *testing.T) {
		text := "Content-Transfer-Encoding: base64\r\n\r\nQUJD!!!!QUJD"
		got := consumeStall(&ioSource{data: text, cut: len(text), chunk: 4096, err: errSourceIO, withData: true}, &ParseOptions{Profile: ProfileStrict})
		var corrupt base64.CorruptInputError
		if !errors.As(got.bodyErrs[0], &corrupt) || errors.Is(got.bodyErrs[0], errSourceIO) {
			t.Errorf("body ended with %v, want base64's own corruption alone", got.bodyErrs[0])
		}
		// The tolerant decoder skips the bytes outside the alphabet and reads
		// on, so the failure is reached (ROB-070-004).
		got = consumeStall(&ioSource{data: text, cut: len(text), chunk: 4096, err: errSourceIO, withData: true}, nil)
		if got.bodies[0] != "ABCABC" || got.bodyErrs[0] != errSourceIO {
			t.Errorf("tolerant: (%q, %v), want ABCABC and the identical input error", got.bodies[0], got.bodyErrs[0])
		}
	})

	t.Run("a source error that cannot be compared", func(t *testing.T) {
		fail := structError{s: []int{1, 2}}
		_, e, err := Parse(&failingSource{s: "\r\nabc", err: fail}, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, berr := io.ReadAll(e.Body)
		if string(b) != "abc" || reflect.TypeOf(berr) != joinErrorType {
			t.Fatalf("(%q, %T %v), want the bytes and a join holding the failure", b, berr, berr)
		}
		for _, part := range berr.(interface{ Unwrap() []error }).Unwrap() {
			if _, ok := part.(structError); !ok {
				t.Errorf("join holds %T, want only the source's own value", part)
			}
		}
	})
}

// checkJoin checks that err is one errors.Join of first and then a second
// error, which is want when want is not nil, and returns its elements.
func checkJoin(t *testing.T, err, first, want error) []error {
	t.Helper()
	if reflect.TypeOf(err) != joinErrorType {
		t.Errorf("error %T %v, want one join", err, err)
		return nil
	}
	parts := err.(interface{ Unwrap() []error }).Unwrap()
	if len(parts) != 2 || parts[0] != first || (want != nil && parts[1] != want) {
		t.Errorf("join %v, want %v first and %v", parts, first, want)
	}
	return parts
}

// TestParse_sourceErrorInEagerHook pins a CharsetReader that reads its input
// while it builds a decoder: when it fails because the input failed, the entity
// is returned with the input's error (or both, when the hook added its own),
// not an unknown charset; when the body's own budget refused the read, with the
// body's local FatalError, and the following part is still returned; and a
// hook that built its decoder anyway leaves the failure to the body.
func TestParse_sourceErrorInEagerHook(t *testing.T) {
	hook := func(mode string) func(string, io.Reader) (io.Reader, error) {
		return func(_ string, in io.Reader) (io.Reader, error) {
			b, err := io.ReadAll(in)
			switch {
			case err == nil:
				return bytes.NewReader(b), nil
			case mode == "keep":
				return bytes.NewReader(b), nil
			case mode == "wrap":
				return nil, fmt.Errorf("hook: %w", err)
			}
			return nil, err
		}
	}
	const root = "Content-Type: text/plain; charset=x-eager\r\n\r\nbody text"

	for _, mode := range []string{"fail", "wrap"} {
		t.Run("root/"+mode, func(t *testing.T) {
			withCharsetReader(t, hook(mode))
			s, e, err := Parse(&ioSource{data: root, cut: len(root) - 4, chunk: 4096, err: errSourceIO, sticky: true}, nil)
			if e == nil || e.Header.Get("Content-Type") == "" {
				t.Fatalf("Parse() = (nil, %v), want the usable entity", err)
			}
			if IsUnknownCharset(err) || IsFatal(err) || s.Len() != 0 || e.ParseStop() != nil {
				t.Errorf("Parse() error %v, %v; want the input's error, no warning, no diagnostic, no stop", err, codesOfDiags(s.Diagnostics()))
			}
			if mode == "fail" && err != errSourceIO {
				t.Errorf("Parse() error %v, want the identical input error", err)
			}
			if mode == "wrap" {
				if parts := checkJoin(t, err, errSourceIO, nil); len(parts) == 2 && !errors.Is(parts[1], errSourceIO) {
					t.Errorf("second error %v, want the hook's", parts[1])
				}
			}
			if _, berr := io.ReadAll(e.Body); berr != errSourceIO {
				t.Errorf("body ended with %v, want the input error", berr)
			}
		})
	}

	t.Run("root/keep", func(t *testing.T) {
		withCharsetReader(t, hook("keep"))
		s, e, err := Parse(&ioSource{data: root, cut: len(root) - 4, chunk: 4096, err: errSourceIO, sticky: true}, nil)
		if err != nil || s.Len() != 0 {
			t.Fatalf("Parse() = %v, %v", err, codesOfDiags(s.Diagnostics()))
		}
		if b, berr := io.ReadAll(e.Body); string(b) != "body " || berr != errSourceIO {
			t.Errorf("body (%q, %v), want what the hook kept and then the input error", b, berr)
		}
	})

	tree := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=x-eager\r\n\r\nfirst part\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nsecond part\r\n--B--\r\n"

	t.Run("child/input error", func(t *testing.T) {
		withCharsetReader(t, hook("fail"))
		src := &ioSource{data: tree, cut: strings.Index(tree, "first") + 3, chunk: 4096, err: errSourceIO, sticky: true}
		_, e, err := Parse(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		p, err := e.MultipartReader().NextPart()
		if p == nil || !errors.Is(err, errSourceIO) || IsUnknownCharset(err) || IsFatal(err) {
			t.Fatalf("NextPart() = (%v, %v), want the entity with the input error", p, err)
		}
		_, e, _ = Parse(&ioSource{data: tree, cut: src.cut, chunk: 4096, err: errSourceIO, sticky: true}, nil)
		if werr := e.Walk(func([]int, *Entity, error) error { return nil }); !errors.Is(werr, errSourceIO) {
			t.Errorf("Walk() = %v, want the input error", werr)
		}
	})

	t.Run("child/local refusal", func(t *testing.T) {
		withCharsetReader(t, hook("fail"))
		// The budget refuses the first part's ten bytes and admits the whole of
		// the second's two.
		short := strings.Replace(tree, "second part", "ok", 1)
		limits := allDisabled()
		limits.MaxEncodedBodyBytes = 3
		s, e, err := Parse(strings.NewReader(short), &ParseOptions{Limits: limits})
		if err != nil {
			t.Fatal(err)
		}
		mr := e.MultipartReader()
		p, err := mr.NextPart()
		var f FatalError
		if p == nil || !errors.As(err, &f) || IsUnknownCharset(err) || e.ParseStop() != nil {
			t.Fatalf("NextPart() = (%v, %v), want the entity with its local refusal", p, err)
		}
		if d, _ := f.Diagnostic(); d.Code != CodeLimitEncodedBodyBytes {
			t.Errorf("refusal %s, want the encoded budget's", d.Code)
		}
		next, err := mr.NextPart()
		if err != nil {
			t.Fatalf("second NextPart() = %v, want the next part", err)
		}
		if b, berr := io.ReadAll(next.Body); string(b) != "ok" || berr != nil {
			t.Errorf("second part (%q, %v), want it whole", b, berr)
		}
		if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitEncodedBodyBytes}) {
			t.Errorf("Diagnostics() = %v, want no charset.unknown", codes)
		}
	})
}

// TestParse_sourceErrorWriteTo pins the verbatim copy: the bytes before the
// failure are written and the failure is returned, into a writer with ReadFrom
// and into a plain one.
func TestParse_sourceErrorWriteTo(t *testing.T) {
	for _, text := range []string{
		"Subject: x\r\n\r\nbody text",
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nxyz\r\n--B--\r\n",
	} {
		cut := strings.Index(text, "\r\n\r\n") + 6
		for _, plain := range []bool{false, true} {
			_, e, err := Parse(&ioSource{data: text, cut: cut, chunk: 4096, err: errSourceIO, sticky: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			var w io.Writer = &buf
			if plain {
				w = struct{ io.Writer }{&buf}
			}
			werr := e.WriteTo(w)
			if !errors.Is(werr, errSourceIO) || !strings.HasSuffix(buf.String(), text[cut-2:cut]) {
				t.Errorf("plain %v: WriteTo() = %v, wrote %q; want the bytes before the failure and the failure", plain, werr, buf.String())
			}
		}
	}
}

// TestParse_sourceErrorKeepsOwnStops pins that the parser's own stops keep
// their precedence over an ordinary failure: an input budget reached before
// the failure, and a cancellation.
func TestParse_sourceErrorKeepsOwnStops(t *testing.T) {
	const text = "Subject: x\r\n\r\nbody text"
	limits := Limits{MaxInputBytes: int64(len(text) - 6)}
	got := consumeStall(&ioSource{data: text, cut: len(text) - 2, chunk: 4096, err: errSourceIO, sticky: true}, &ParseOptions{Limits: limits})
	stop := got.entity.ParseStop()
	if stop == nil || got.bodyErrs[0] != stop || errors.Is(stop, errSourceIO) {
		t.Errorf("body ended with %v, stop %v; want the input budget's stop", got.bodyErrs[0], stop)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, e, err := ParseContext(ctx, &ioSource{data: text, cut: len(text) - 2, chunk: 4096, err: errSourceIO, sticky: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, berr := io.ReadAll(e.Body); !errors.Is(berr, context.Canceled) || berr != e.ParseStop() {
		t.Errorf("body ended with %v, want the cancellation", berr)
	}
}

// TestParse_sourceErrorForeignValues pins that a source's own FatalError or
// io.ErrNoProgress is an ordinary error: returned as itself, never this parse's
// stop.
func TestParse_sourceErrorForeignValues(t *testing.T) {
	_, _, other := Parse(strings.NewReader("\r\n"), &ParseOptions{Limits: Limits{MaxInputBytes: 1}})
	if !IsFatal(other) {
		t.Fatalf("setup: Parse() = %v, want another parse's FatalError", other)
	}
	for _, fail := range []error{io.ErrNoProgress, other} {
		got := consumeStall(&ioSource{data: "\r\nabc", cut: 5, chunk: 4096, err: fail, sticky: true}, nil)
		if got.bodyErrs[0] != fail || got.entity.ParseStop() != nil || got.session.Len() != 0 {
			t.Errorf("%v: body ended with %v, stop %v", fail, got.bodyErrs[0], got.entity.ParseStop())
		}
	}
}

const ioErrorEnv = "EMLKIT_IO_ERROR_HOSTILE"

// TestParse_sourceErrorHostileJoin pins that the join of a hostile source error
// with a decoder's report asks the source's error nothing: an error whose
// Unwrap never ends, or whose As panics, ends a quoted-printable body cut at a
// soft break inside a join whose first element is that very value. Each case
// runs in a subprocess with a deadline.
func TestParse_sourceErrorHostileJoin(t *testing.T) {
	if kind := os.Getenv(ioErrorEnv); kind != "" {
		var fail error = &cyclicError{}
		if kind == "as" {
			fail = &trappedError{}
		}
		text := "Content-Transfer-Encoding: quoted-printable\r\n\r\nfoo=\r"
		// ProfileStrict, whose decoder has a report of its own to join.
		_, e, err := Parse(&failingSource{s: text, err: fail}, &ParseOptions{Profile: ProfileStrict})
		if err != nil {
			fmt.Println("CHILD-FAIL Parse", reflect.TypeOf(err))
			os.Exit(1)
		}
		b, berr := io.ReadAll(e.Body)
		if string(b) != "foo" || reflect.TypeOf(berr) != joinErrorType {
			fmt.Println("CHILD-FAIL body", string(b), reflect.TypeOf(berr))
			os.Exit(1)
		}
		if parts := berr.(interface{ Unwrap() []error }).Unwrap(); len(parts) != 2 || parts[0] != fail {
			fmt.Println("CHILD-FAIL join")
			os.Exit(1)
		}
		fmt.Println("CHILD-PASS")
		os.Exit(0)
	}
	for _, kind := range []string{"cycle", "as"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestParse_sourceErrorHostileJoin$", "-test.count=1")
		cmd.Env = append(os.Environ(), ioErrorEnv+"="+kind)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil || !bytes.Contains(out, []byte("CHILD-PASS")) {
			t.Errorf("%s: subprocess = %v\n%s", kind, err, out)
		}
	}
}

// TestParse_sourceErrorRoundTrip pins that a complete header read before an
// input failure in the body is the input's header byte for byte.
func TestParse_sourceErrorRoundTrip(t *testing.T) {
	const header = "Subject:  spaced \r\nX-Folded: a\r\n\tb\r\n\r\n"
	_, e, err := Parse(&ioSource{data: header + "body", cut: len(header) + 2, chunk: 7, err: errSourceIO, sticky: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, e.Header.Header); err != nil || b.String() != header {
		t.Errorf("WriteHeader() = %q, %v; want the input's header", b.String(), err)
	}
}
