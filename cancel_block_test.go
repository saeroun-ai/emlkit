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
	"sync"
	"testing"
	"time"
)

// The boundaries of ROB-001-006 that only a blocked caller component can show:
// a source blocked in Read is not interrupted by cancellation, a
// DiagnosticSink blocked in the delivery that decides a stop does not keep a
// cancelled parse's other scopes waiting, and a CharsetReader that NewMultipart's
// setup calls may Close that body, or block while it is closed, without a
// deadlock or a worker left behind. All hold gates that only the probe
// releases, so they run in a child process the parent kills at a deadline,
// with a capped stack: a regression is a failure here, never a hung test
// binary. Every gate is also released on the probe's way out, failing or not.

const cancelProbeEnv = "EMLKIT_ROB001006_PROBE"

func TestParseContextBlocking_subprocess(t *testing.T) {
	if os.Getenv(cancelProbeEnv) == "1" {
		runCancelProbe()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestParseContextBlocking_subprocess$", "-test.count=1", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), cancelProbeEnv+"=1")
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the probe did not finish before its deadline: something blocks for ever\n%s", out.String())
	}
	if err != nil || !strings.Contains(out.String(), "probe: ok") {
		t.Fatalf("probe failed: %v\n%s", err, out.String())
	}
}

// probeGate is a gate a probe waits behind, released once.
type probeGate struct {
	entered chan struct{}
	release chan struct{}
	enter   sync.Once
	open    sync.Once
}

func newProbeGate() *probeGate {
	return &probeGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *probeGate) wait() {
	g.enter.Do(func() { close(g.entered) })
	<-g.release
}

func (g *probeGate) Open() { g.open.Do(func() { close(g.release) }) }

// blockedSource delivers head, then blocks behind its gate once, then
// delivers tail.
type blockedSource struct {
	head, tail string
	gate       *probeGate
	blocked    bool
	late       int // reads after the one that blocked
}

func (b *blockedSource) Read(p []byte) (int, error) {
	if b.blocked {
		b.late++
	}
	if b.head != "" {
		n := copy(p, b.head)
		b.head = b.head[n:]
		return n, nil
	}
	if !b.blocked {
		b.blocked = true
		b.gate.wait()
	}
	if b.tail == "" {
		return 0, io.EOF
	}
	n := copy(p, b.tail)
	b.tail = b.tail[n:]
	return n, nil
}

func runCancelProbe() {
	debug.SetMaxStack(16 << 20)
	var gates []*probeGate
	defer func() {
		for _, g := range gates {
			g.Open()
		}
	}()
	fail := func(format string, args ...any) {
		for _, g := range gates {
			g.Open()
		}
		fmt.Printf("probe: FAIL "+format+"\n", args...)
		os.Exit(3)
	}
	within := func(what string, d time.Duration, done <-chan struct{}) {
		select {
		case <-done:
		case <-time.After(d):
			fail("%s did not return within %v", what, d)
		}
	}

	// A source blocked in Read is not interrupted.
	{
		gate := newProbeGate()
		gates = append(gates, gate)
		src := &blockedSource{head: "Content-Type: text/plain\r\n\r\nabc", tail: "xyz", gate: gate}
		ctx, cancel := context.WithCancel(context.Background())
		_, e, err := ParseContext(ctx, src, nil)
		if err != nil {
			fail("ParseContext = %v", err)
		}
		var body []byte
		var berr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			body, berr = io.ReadAll(e.Body)
		}()
		within("reaching the blocked read", 5*time.Second, gate.entered)
		cancel()
		select {
		case <-done:
			fail("the blocked read returned on cancellation: %q, %v", body, berr)
		case <-time.After(100 * time.Millisecond):
		}
		gate.Open()
		within("the released read", 5*time.Second, done)
		if string(body) != "abcxyz" || berr == nil || berr != e.ParseStop() || !errors.Is(berr, context.Canceled) {
			fail("released body = %q, %v; want every byte and the stop", body, berr)
		}
		if src.late != 0 {
			fail("the source was read %d times after the cancelling read", src.late)
		}
	}

	// A sink blocked while it is told about a stop-deciding overflow, with a
	// second scope waiting for that decision.
	transitions := map[string]struct {
		limits Limits
		code   Code
	}{
		"part count":   {Limits{MaxPartCount: 1}, CodeLimitPartCount},
		"total header": {Limits{MaxTotalHeaderBytes: 80}, CodeLimitTotalHeaderBytes},
	}
	const tree = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\none\r\n" +
		"--B\r\nContent-Type: text/plain\r\nX-Pad: padding padding padding\r\n\r\ntwo\r\n--B--\r\n"
	for name, tc := range transitions {
		gate := newProbeGate()
		gates = append(gates, gate)
		ctx, cancel := context.WithCancel(context.Background())
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			if d.Code == tc.code {
				gate.wait()
			}
			return nil
		})
		_, e, err := ParseContext(ctx, strings.NewReader(tree), &ParseOptions{Limits: tc.limits, DiagnosticSink: sink})
		if err != nil {
			fail("%s: ParseContext = %v", name, err)
		}
		mr := e.MultipartReader()
		var iterErr error
		iterDone := make(chan struct{})
		go func() {
			defer close(iterDone)
			for range 4 {
				p, err := mr.NextPart()
				if err != nil {
					iterErr = err
					return
				}
				_, _ = io.ReadAll(p.Body)
			}
		}()
		within(name+": reaching the blocked sink", 5*time.Second, gate.entered)

		var waited error
		waitDone := make(chan struct{})
		go func() {
			defer close(waitDone)
			waited = e.ParseStop()
		}()
		select {
		case <-waitDone:
			fail("%s: ParseStop returned %v during the transition", name, waited)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		within(name+": the waiting scope after cancellation", 5*time.Second, waitDone)
		if waited == nil || !errors.Is(waited, context.Canceled) {
			fail("%s: the waiting scope got %v, want the cancellation", name, waited)
		}
		gate.Open()
		within(name+": the transition after its sink returned", 5*time.Second, iterDone)
		if iterErr != waited || e.ParseStop() != waited {
			fail("%s: the iterator ended with %v and ParseStop is %v, want the one stop %v", name, iterErr, e.ParseStop(), waited)
		}
	}
	// NewMultipart sets its body up with no lock held, because interpreting the
	// header calls the configured CharsetReader for an encoded word: the
	// hook may Close the body it is setting up, or block while another
	// goroutine closes it. A Close that comes before the setup's outcome is
	// published wins over that outcome, success or failure: Close returns at
	// once, a Read waiting for the setup returns at once, every Read reports
	// io.ErrClosedPipe, and no worker is ever started.
	//
	// NewMultipart itself interprets the header too, so every fixture is built
	// while the hook neither closes nor blocks, and armed only afterwards,
	// before the body's first Read.
	{
		workers := bodyWorkers()
		old := CharsetReader
		defer func() { CharsetReader = old }()
		gate := newProbeGate()
		gates = append(gates, gate)
		var hookClose func() error
		block := false
		CharsetReader = func(_ string, r io.Reader) (io.Reader, error) {
			if block {
				gate.wait()
			} else if hookClose != nil {
				_ = hookClose()
			}
			return r, nil
		}
		build := func(contentType string) (*Entity, io.Closer) {
			var h Header
			h.Set("Content-Type", contentType)
			part, err := New(Header{}, strings.NewReader("part"))
			if err != nil {
				fail("New = %v", err)
			}
			e, err := NewMultipart(h, []*Entity{part})
			if err != nil {
				fail("NewMultipart = %v", err)
			}
			return e, e.Body.(io.Closer)
		}
		read := func(e *Entity, out *error) <-chan struct{} {
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, *out = e.Body.Read(make([]byte, 8))
			}()
			return done
		}
		const (
			usable   = `multipart/mixed; boundary=B; note="=?x-hook?Q?value?="`
			refusing = `multipart/mixed; boundary=B; charset=unavailable; note="=?x-hook?Q?value?="`
		)

		// The hook closes the body it is setting up, whether the setup would
		// then have succeeded or failed: the Close came first, and wins.
		for _, ct := range []string{usable, refusing} {
			hookClose = nil
			e, c := build(ct)
			hookClose = c.Close
			var first error
			within("a Read whose CharsetReader closes its body", 3*time.Second, read(e, &first))
			hookClose = nil
			if first != io.ErrClosedPipe {
				fail("%s: Read with a closing hook = %v, want io.ErrClosedPipe", ct, first)
			}
			if n, err := e.Body.Read(make([]byte, 8)); n != 0 || err != io.ErrClosedPipe {
				fail("%s: Read after the setup = %d, %v; want io.ErrClosedPipe", ct, n, err)
			}
		}

		// The hook blocks; another goroutine closes. A second Read waiting for
		// the setup returns at once, before the hook is released; the Read in
		// the hook returns once it is.
		e, c := build(usable)
		block = true
		var blocked, waiting error
		blockedDone := read(e, &blocked)
		within("reaching the blocked hook", 5*time.Second, gate.entered)
		waitingDone := read(e, &waiting)
		// Give the second Read time to start waiting; if it starts after the
		// Close instead, it finds the body closed, with the same outcome.
		time.Sleep(20 * time.Millisecond)
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			_ = c.Close()
		}()
		within("Close during a blocked setup", time.Second, closed)
		within("the waiting Read, with the hook still blocked", time.Second, waitingDone)
		if waiting != io.ErrClosedPipe {
			fail("the waiting Read = %v, want io.ErrClosedPipe", waiting)
		}
		select {
		case <-blockedDone:
			fail("the Read inside the blocked hook returned before the hook did")
		default:
		}
		gate.Open()
		within("the released Read", 5*time.Second, blockedDone)
		if blocked != io.ErrClosedPipe {
			fail("the released Read = %v, want io.ErrClosedPipe", blocked)
		}
		if n, err := e.Body.Read(make([]byte, 8)); n != 0 || err != io.ErrClosedPipe {
			fail("Read after the setup = %d, %v", n, err)
		}
		if got := waitForBodyWorkers(workers); got != workers {
			fail("%d body workers, want %d: a worker started after Close", got, workers)
		}
	}
	fmt.Println("probe: ok")
}
