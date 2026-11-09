package message

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ROB-120-008 (issue #58): a Content-Type that does not parse is refused by
// every generation and serialization path with an explicit error, instead of
// panicking on a nil parameter map or being written as some other media type.

// malformedContentTypes are the reproduction matrix of issue #58.
var malformedContentTypes = []string{
	"multipart/mixed; boundary=B; broken",
	"MULTIPART/MIXED; boundary=B; broken",
	"multipart/mixed; boundary=B; boundary=C",
	"text/plain; broken",
	"multipart/; boundary=B",
}

func headerWithContentType(ct string) Header {
	var h Header
	h.Set("Content-Type", ct)
	return h
}

// noPanic runs f and fails the test, rather than the binary, if it panics.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("%s panicked: %v", what, p)
		}
	}()
	f()
}

// checkRefusal checks that err refuses ct explicitly.
func checkRefusal(t *testing.T, ct string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%q: err = nil, want a refusal", ct)
	}
	if !strings.Contains(err.Error(), "Content-Type") || !strings.Contains(err.Error(), ct) {
		t.Errorf("%q: err = %q, want it to name the field and its value", ct, err)
	}
	if errors.Unwrap(err) == nil {
		t.Errorf("%q: err = %v wraps nothing, want the parse error", ct, err)
	}
}

func TestCreateWriter_refusesMalformedContentType(t *testing.T) {
	for _, ct := range malformedContentTypes {
		h := headerWithContentType(ct)
		var out bytes.Buffer
		var (
			w   *Writer
			err error
		)
		noPanic(t, "CreateWriter", func() { w, err = CreateWriter(&out, h) })
		checkRefusal(t, ct, err)
		if w != nil || out.Len() != 0 {
			t.Errorf("%q: CreateWriter() = %v and %d bytes, want no writer and no output", ct, w, out.Len())
		}
		// Copy-on-write: the caller's header is neither normalized nor given a
		// MIME-Version.
		if got := h.Get("Content-Type"); got != ct || h.Has("Mime-Version") {
			t.Errorf("%q: caller header changed to %q (MIME-Version %v)", ct, got, h.Has("Mime-Version"))
		}
	}
	// One whose parse error is a known sentinel stays reachable through the
	// wrap.
	_, err := CreateWriter(io.Discard, headerWithContentType("text/plain; broken"))
	if !errors.Is(err, mime.ErrInvalidMediaParameter) {
		t.Errorf("err = %v, want it to wrap mime.ErrInvalidMediaParameter", err)
	}
}

func TestCreatePart_refusesMalformedContentTypeAndContinues(t *testing.T) {
	for _, withBuffer := range []bool{false, true} {
		for _, ct := range malformedContentTypes {
			var out bytes.Buffer
			w, err := CreateWriter(&out, headerWithContentType("multipart/mixed; boundary=OUT"))
			if err != nil {
				t.Fatal(err)
			}
			writePart(t, w, "before")
			before := out.String()

			bad := headerWithContentType(ct)
			buf := bytes.NewBufferString("caller's bytes")
			var p *Writer
			noPanic(t, "CreatePart", func() {
				if withBuffer {
					p, err = w.CreatePartWithBuffer(bad, buf)
				} else {
					p, err = w.CreatePart(bad)
				}
			})
			checkRefusal(t, ct, err)
			if p != nil || out.String() != before {
				t.Errorf("%q: refused part = %v, output grew by %q", ct, p, strings.TrimPrefix(out.String(), before))
			}
			if buf.String() != "caller's bytes" || bad.Get("Content-Type") != ct {
				t.Errorf("%q: caller buffer %q or header %q changed", ct, buf.String(), bad.Get("Content-Type"))
			}

			writePart(t, w, "after")
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := readParts(t, out.String()); strings.Join(got, ",") != "before,after" {
				t.Errorf("%q: parts written = %q, want before and after", ct, got)
			}
		}
	}

	// A refused first part does not commit the writer to part mode: Close of a
	// writer that never created a part writes no closing delimiter.
	var out bytes.Buffer
	w, _ := CreateWriter(&out, headerWithContentType("multipart/mixed; boundary=OUT"))
	header := out.String()
	if _, err := w.CreatePart(headerWithContentType("text/plain; broken")); err == nil {
		t.Fatal("CreatePart() accepted a malformed Content-Type")
	}
	if err := w.Close(); err != nil || out.String() != header {
		t.Errorf("Close() after a refused first part = %v, wrote %q", err, strings.TrimPrefix(out.String(), header))
	}
}

func writePart(t *testing.T, w *Writer, body string) {
	t.Helper()
	p, err := w.CreatePart(headerWithContentType("text/plain"))
	if err != nil {
		t.Fatalf("CreatePart(text/plain) = %v", err)
	}
	if _, err := io.WriteString(p, body); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func readParts(t *testing.T, text string) []string {
	t.Helper()
	e, err := Read(strings.NewReader(text))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	var bodies []string
	mr := e.MultipartReader()
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return bodies
		}
		if err != nil {
			t.Fatalf("reading back: %v", err)
		}
		bodies = append(bodies, readBodyOf(t, p))
	}
}

// TestEntityWriteTo_refusesMalformedContentType pins the documented
// limitation: an entity parsed from a damaged header keeps that header byte for
// byte, and a writer cannot serialize what it cannot interpret, so WriteTo
// refuses it before writing anything — through Parse, whose tolerant repair
// still gives the entity a usable interpretation, and through Read.
func TestEntityWriteTo_refusesMalformedContentType(t *testing.T) {
	for _, ct := range malformedContentTypes {
		text := "Content-Type: " + ct + "\r\n\r\nbody"
		parsed := func() *Entity { _, e, _ := Parse(strings.NewReader(text), nil); return e }
		read := func() *Entity { e, _ := Read(strings.NewReader(text)); return e }
		for name, open := range map[string]func() *Entity{"Parse": parsed, "Read": read} {
			e := open()
			var out bytes.Buffer
			var err error
			noPanic(t, name+" WriteTo", func() { err = e.WriteTo(&out) })
			checkRefusal(t, ct, err)
			if out.Len() != 0 {
				t.Errorf("%s %q: WriteTo wrote %q before refusing", name, ct, out.String())
			}
			if got := e.Header.Get("Content-Type"); got != ct {
				t.Errorf("%s %q: header changed to %q", name, ct, got)
			}
		}
	}
}

// readWithin reads r whole on another goroutine and fails the test if that
// takes longer than the deadline, which is how a Read that blocks for ever
// shows up.
//
// A panic on that reading goroutine — where multipartBody.Read creates the
// body's writer — is recovered there and reported as a test failure, so the
// helper says which read panicked instead of aborting the binary. A panic on
// the separate worker goroutine that writes the parts cannot be recovered from
// here; it still aborts the test binary, which fails the run as loudly.
func readWithin(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	type result struct {
		b     []byte
		err   error
		panic any
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{panic: p}
			}
		}()
		b, err := io.ReadAll(r)
		done <- result{b: b, err: err}
	}()
	select {
	case res := <-done:
		if res.panic != nil {
			t.Fatalf("reading the body panicked: %v", res.panic)
		}
		return res.b, res.err
	case <-time.After(5 * time.Second):
		t.Fatal("reading the body did not return: a Read is blocked")
		return nil, nil
	}
}

// bodyWorkers counts the goroutines currently running the worker that
// multipartBody.Read starts to write a generated body, found by that
// function's frame in a dump of every goroutine's stack. Counting that one
// function, rather than every goroutine, keeps the test independent of
// whatever else the runtime or other tests have running.
func bodyWorkers() int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "(*multipartBody).Read.func1(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitForBodyWorkers waits, up to a deadline, for the number of body workers
// to fall to at most n, and reports the number it ended with.
func waitForBodyWorkers(n int) int {
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := bodyWorkers()
		if got <= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNewMultipart_malformedHeaderFailsTheBody pins the lazy refusal: building
// the entity cannot fail for a header it does not interpret, but reading its
// body reports the refusal, and every later read reports the same error rather
// than blocking on a pipe no writer will ever feed. No worker is started for a
// body whose writer could not be created.
func TestNewMultipart_malformedHeaderFailsTheBody(t *testing.T) {
	for _, ct := range malformedContentTypes {
		child, _ := New(Header{}, strings.NewReader("child"))
		var (
			e   *Entity
			err error
		)
		noPanic(t, "NewMultipart", func() { e, err = NewMultipart(headerWithContentType(ct), []*Entity{child}) })
		if e == nil {
			t.Fatalf("%q: NewMultipart() = nil, %v", ct, err)
		}
		workers := bodyWorkers()
		_, first := readWithin(t, e.Body)
		_, second := readWithin(t, e.Body)
		checkRefusal(t, ct, first)
		if second == nil || second.Error() != first.Error() {
			t.Errorf("%q: second read = %v, want the same refusal %v", ct, second, first)
		}
		if got := bodyWorkers(); got > workers {
			t.Errorf("%q: %d body workers, want no new one for a body that was never set up", ct, got)
		}
		var out bytes.Buffer
		if err := e.WriteTo(&out); err == nil || out.Len() != 0 {
			t.Errorf("%q: WriteTo() = %v after %d bytes, want a refusal before any output", ct, err, out.Len())
		}
	}
}

// TestNewMultipart_writerSetupFailureIsSticky pins that the sticky error covers
// the writer setup failure that predates this item, on the same path: a
// well-formed Content-Type with a charset the writer does not handle. New
// reports the charset as the usual warning together with a usable entity, and
// reading that entity's body refuses on every read without blocking.
func TestNewMultipart_writerSetupFailureIsSticky(t *testing.T) {
	child, _ := New(Header{}, strings.NewReader("child"))
	e, err := NewMultipart(headerWithContentType("text/plain; charset=unavailable"), []*Entity{child})
	if e == nil || !IsUnknownCharset(err) {
		t.Fatalf("NewMultipart() = (%v, %v), want a usable entity with the unknown-charset warning", e, err)
	}
	for i := range 3 {
		if _, err := readWithin(t, e.Body); err == nil || !strings.Contains(err.Error(), "unhandled charset") {
			t.Errorf("read %d = %v, want the writer's unhandled-charset refusal", i, err)
		}
	}
}

// TestNewMultipart_malformedChildFailsTheBody pins the other half: a valid
// multipart whose child has a malformed Content-Type delivers the parts before
// it, then the refusal, to every reader, and the worker writing the body exits.
func TestNewMultipart_malformedChildFailsTheBody(t *testing.T) {
	good := func(s string) *Entity {
		e, _ := New(headerWithContentType("text/plain"), strings.NewReader(s))
		return e
	}
	bad, _ := New(headerWithContentType("text/plain; broken"), strings.NewReader("bad"))

	workers := bodyWorkers()
	e, err := NewMultipart(headerWithContentType("multipart/mixed; boundary=OUT"), []*Entity{good("before"), bad, good("after")})
	if err != nil {
		t.Fatalf("NewMultipart() = %v", err)
	}
	b, first := readWithin(t, e.Body)
	_, second := readWithin(t, e.Body)
	checkRefusal(t, "text/plain; broken", first)
	if second == nil || second.Error() != first.Error() {
		t.Errorf("second read = %v, want the same refusal %v", second, first)
	}
	if !strings.Contains(string(b), "before") || strings.Contains(string(b), "bad") || strings.Contains(string(b), "after") {
		t.Errorf("body before the refusal = %q, want the part before it and nothing of the refused one", b)
	}
	// The worker that wrote the body has exited.
	if got := waitForBodyWorkers(workers); got > workers {
		t.Errorf("%d body workers left, want %d: the worker for this body did not exit", got, workers)
	}
}

// TestBodyWorkers_seesALiveWorker keeps bodyWorkers honest: a worker held open
// by a child body that has not finished is counted, and stops being counted
// once it finishes. Without this, a renamed function would make the exit
// checks above pass vacuously.
func TestBodyWorkers_seesALiveWorker(t *testing.T) {
	release := make(chan struct{})
	child, _ := New(headerWithContentType("text/plain"), &gatedReader{release: release, data: "late"})
	e, err := NewMultipart(headerWithContentType("multipart/mixed; boundary=OUT"), []*Entity{child})
	if err != nil {
		t.Fatal(err)
	}
	workers := bodyWorkers()
	// The first bytes — the part's delimiter and header — come before the
	// child body, which the worker then waits on.
	if _, err := e.Body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if got := bodyWorkers(); got != workers+1 {
		t.Fatalf("bodyWorkers() = %d with a worker waiting, want %d", got, workers+1)
	}
	close(release)
	if b, err := readWithin(t, e.Body); err != nil || !strings.Contains(string(b), "late") {
		t.Fatalf("rest of the body = (%q, %v)", b, err)
	}
	if got := waitForBodyWorkers(workers); got != workers {
		t.Errorf("bodyWorkers() = %d after the worker finished, want %d", got, workers)
	}
}

// gatedReader yields data once release is closed.
type gatedReader struct {
	release chan struct{}
	data    string
	done    bool
}

func (r *gatedReader) Read(p []byte) (int, error) {
	<-r.release
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), nil
}

// TestCreateWriter_wellFormedBehaviorUnchanged pins the paths this item must
// not change: an absent Content-Type, a missing or invalid boundary replaced by
// a generated one in the written header only, upper-case well-formed types,
// and the charset policy.
func TestCreateWriter_wellFormedBehaviorUnchanged(t *testing.T) {
	var out bytes.Buffer
	if w, err := CreateWriter(&out, Header{}); err != nil || w.mw != nil {
		t.Errorf("absent Content-Type: (%v, %v), want a plain-text writer", w, err)
	}

	for _, ct := range []string{"multipart/mixed", `multipart/mixed; boundary="` + strings.Repeat("x", 71) + `"`} {
		out.Reset()
		h := headerWithContentType(ct)
		w, err := CreateWriter(&out, h)
		if err != nil {
			t.Fatalf("%q: CreateWriter() = %v", ct, err)
		}
		writePart(t, w, "only")
		w.Close()
		if h.Get("Content-Type") != ct {
			t.Errorf("%q: caller header changed to %q", ct, h.Get("Content-Type"))
		}
		if got := readParts(t, out.String()); strings.Join(got, ",") != "only" {
			t.Errorf("%q: parts = %q, want the one written under a generated boundary", ct, got)
		}
	}

	out.Reset()
	w, err := CreateWriter(&out, headerWithContentType("MULTIPART/MIXED; boundary=B"))
	if err != nil || w.mw == nil || w.mw.Boundary() != "B" {
		t.Errorf("upper-case well-formed multipart: (%v, %v), want a multipart writer with boundary B", w, err)
	}

	if _, err := CreateWriter(io.Discard, headerWithContentType("text/plain; charset=iso-8859-1")); err == nil ||
		!strings.Contains(err.Error(), "unhandled charset") {
		t.Errorf("charset policy: err = %v, want the existing unhandled-charset refusal", err)
	}
}
