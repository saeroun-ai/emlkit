package message

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// The lifecycle of a body built by NewMultipart (ROB-001-006, issue #6): what
// Close releases, from which goroutine it may be called, and what a body
// closed before its first Read does. The worker is observed through its own
// frame (bodyWorkers in writer_malformed_test.go), never through a count of
// every goroutine, and every gated reader is released in cleanup. Bodies are
// at most 64 KiB, and every loop has a fixed count.

func lifecyclePart(t *testing.T, body io.Reader) *Entity {
	t.Helper()
	var h Header
	h.Set("Content-Type", "text/plain")
	part, err := New(h, body)
	if err != nil {
		t.Fatal(err)
	}
	return part
}

func lifecycleBody(t *testing.T, parts ...*Entity) (*Entity, io.Closer) {
	t.Helper()
	var h Header
	h.Set("Content-Type", "multipart/mixed; boundary=B")
	e, err := NewMultipart(h, parts)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := e.Body.(io.Closer)
	if !ok {
		t.Fatalf("Body %T is not an io.Closer", e.Body)
	}
	return e, c
}

// TestNewMultipart_firstReadAndCloseRace runs the first Read and Close of a
// body concurrently, a fixed number of times. Under the race detector it is
// the regression for the unsynchronized setup and pipe pointer.
func TestNewMultipart_firstReadAndCloseRace(t *testing.T) {
	workers := bodyWorkers()
	for range 100 {
		e, c := lifecycleBody(t, lifecyclePart(t, strings.NewReader(strings.Repeat("x", 256))))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var readErr error
		go func() {
			defer wg.Done()
			<-start
			_, readErr = e.Body.Read(make([]byte, 16))
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = c.Close()
		}()
		close(start)
		wg.Wait()
		if readErr != nil && readErr != io.ErrClosedPipe {
			t.Fatalf("Read() racing Close = %v", readErr)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("second Close() = %v", err)
		}
		if n, err := e.Body.Read(make([]byte, 1)); n != 0 || err != io.ErrClosedPipe {
			t.Fatalf("Read() after Close = %d, %v", n, err)
		}
	}
	if got := waitForBodyWorkers(workers); got > workers {
		t.Errorf("%d body workers left, want %d: Close did not release them", got, workers)
	}
}

// TestNewMultipart_closeBeforeRead pins that a body closed before its first
// Read stays closed and never starts a worker, and that Close ends the
// iteration as well.
func TestNewMultipart_closeBeforeRead(t *testing.T) {
	workers := bodyWorkers()
	part := lifecyclePart(t, strings.NewReader("one"))
	e, c := lifecycleBody(t, part)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if n, err := e.Body.Read(make([]byte, 8)); n != 0 || err != io.ErrClosedPipe {
			t.Errorf("Read() after Close = %d, %v; want io.ErrClosedPipe", n, err)
		}
	}
	if p, err := e.MultipartReader().NextPart(); p != nil || err != io.EOF {
		t.Errorf("NextPart() after Close = %v, %v; want io.EOF", p, err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := bodyWorkers(); got != workers {
		t.Errorf("%d body workers, want %d: a closed body started one", got, workers)
	}

	// The iteration's own Close is the same Close.
	e, _ = lifecycleBody(t, lifecyclePart(t, strings.NewReader("two")))
	mr := e.MultipartReader()
	if p, err := mr.NextPart(); p == nil || err != nil {
		t.Fatalf("NextPart() = %v, %v", p, err)
	}
	if err := mr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Body.Read(make([]byte, 8)); err != io.ErrClosedPipe {
		t.Errorf("Read() after the iterator's Close = %v", err)
	}
}

// TestNewMultipart_closeReleasesAnAbandonedWorker pins that a body read only
// partway keeps its worker waiting on the pipe, and that Close is what makes
// it exit.
func TestNewMultipart_closeReleasesAnAbandonedWorker(t *testing.T) {
	workers := bodyWorkers()
	e, c := lifecycleBody(t, lifecyclePart(t, strings.NewReader(strings.Repeat("y", 64<<10))))
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.ReadFull(e.Body, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := bodyWorkers(); got != workers+1 {
		t.Fatalf("%d body workers while abandoned, want %d: the worker should still wait", got, workers+1)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := waitForBodyWorkers(workers); got != workers {
		t.Errorf("%d body workers after Close, want %d", got, workers)
	}
	if _, err := e.Body.Read(make([]byte, 8)); err != io.ErrClosedPipe {
		t.Errorf("Read() after Close = %v", err)
	}
}

// lifecycleGate returns its bytes only once release is closed.
type lifecycleGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	s       string
}

func (g *lifecycleGate) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	if g.s == "" {
		return 0, io.EOF
	}
	n := copy(p, g.s)
	g.s = g.s[n:]
	return n, nil
}

// TestNewMultipart_closeCannotReleaseABlockedPart pins the limit of Close: a
// worker blocked reading a part's own source exits only once that source
// returns, and then it does, without writing anything more.
func TestNewMultipart_closeCannotReleaseABlockedPart(t *testing.T) {
	workers := bodyWorkers()
	gate := &lifecycleGate{entered: make(chan struct{}), release: make(chan struct{}), s: "late"}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)

	e, c := lifecycleBody(t, lifecyclePart(t, gate))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(e.Body)
		readDone <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never reached the part's source")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err != io.ErrClosedPipe {
			t.Errorf("the reader's Read ended with %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the reader of the body")
	}
	time.Sleep(20 * time.Millisecond)
	if got := bodyWorkers(); got != workers+1 {
		t.Fatalf("%d body workers, want %d: the worker should still wait on the part", got, workers+1)
	}
	release()
	if got := waitForBodyWorkers(workers); got != workers {
		t.Errorf("%d body workers after the part returned, want %d", got, workers)
	}
}

// TestNewMultipart_cancelledParsedPart pins that a part from ParseContext whose
// parse was cancelled ends a generated body with that parse's stop: a part not
// started is not started, and one being written stops at its next read.
func TestNewMultipart_cancelledParsedPart(t *testing.T) {
	const text = "Content-Type: text/plain\r\n\r\n" + "parsed part body, long enough to be read in several reads"

	t.Run("before", func(t *testing.T) {
		workers := bodyWorkers()
		ctx, cancel := context.WithCancel(context.Background())
		_, parsed, err := ParseContext(ctx, strings.NewReader(text), nil)
		if err != nil {
			t.Fatal(err)
		}
		e, _ := lifecycleBody(t, lifecyclePart(t, strings.NewReader("first")), parsed)
		cancel()
		out, err := io.ReadAll(e.Body)
		if stop := parsed.ParseStop(); stop == nil || err != stop {
			t.Fatalf("generated body ended with %v, want the parse's stop %v", err, stop)
		}
		if strings.Contains(string(out), "parsed part body") || strings.Count(string(out), "Content-Type") != 1 {
			t.Errorf("generated %q: the cancelled part was started", out)
		}
		if got := waitForBodyWorkers(workers); got != workers {
			t.Errorf("%d body workers, want %d", got, workers)
		}
	})

	t.Run("during", func(t *testing.T) {
		workers := bodyWorkers()
		ctx, src := newCut(t, text, 1, cutNever, cutNil)
		_, parsed, err := ParseContext(ctx, src, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Armed before the worker exists: from here on only the worker reads
		// the source, a few bytes into the part's body.
		src.at = src.pos + 5
		e, _ := lifecycleBody(t, parsed)
		out, err := io.ReadAll(e.Body)
		if !strings.HasPrefix(string(out), "--B\r\nContent-Type: text/plain\r\n\r\n") {
			t.Errorf("generated %q, want the part's header first", out)
		}
		if stop := parsed.ParseStop(); stop == nil || err != stop {
			t.Fatalf("generated body ended with %v, want the parse's stop %v", err, stop)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%v does not wrap context.Canceled", err)
		}
		if got := waitForBodyWorkers(workers); got != workers {
			t.Errorf("%d body workers, want %d", got, workers)
		}
	})
}

// TestNewMultipart_setupFailureBeforeCloseIsKept pins the one half of the
// first-terminal-state rule a single goroutine can show: a setup failure
// published before any Close keeps being reported, before and after Close. A
// Close that comes before the setup's outcome is published wins instead; that
// needs a CharsetReader inside the setup, and is pinned by the probe of
// TestParseContextBlocking_subprocess.
func TestNewMultipart_setupFailureBeforeCloseIsKept(t *testing.T) {
	workers := bodyWorkers()
	e, err := NewMultipart(headerWithContentType(malformedContentTypes[0]), []*Entity{lifecyclePart(t, strings.NewReader("x"))})
	if err != nil {
		t.Fatal(err)
	}
	c := e.Body.(io.Closer)
	_, first := e.Body.Read(make([]byte, 8))
	if first == nil || first == io.ErrClosedPipe {
		t.Fatalf("Read() of a refused header = %v, want the writer's refusal", first)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if n, err := e.Body.Read(make([]byte, 8)); n != 0 || err != first {
			t.Errorf("Read() after Close = %d, %v; want the identical setup failure %v", n, err, first)
		}
	}
	if got := bodyWorkers(); got != workers {
		t.Errorf("%d body workers, want %d", got, workers)
	}
}
