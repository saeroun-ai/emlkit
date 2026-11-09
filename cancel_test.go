package message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the cancellation of ParseContext (ROB-001-006, issue #6). See
// cancel.go. Every source here is finite: a cutSource refuses to be read more
// than cutSourceMaxCalls times, and none is longer than a few kilobytes.

// A cutSource delivers s in reads of at most chunk bytes. The read during which
// the delivered total reaches at cancels its context after copying, and returns
// its bytes with what mode says. A read after that one is counted in after, and
// fails, so that a test can prove the parse never read the input again.
type cutSource struct {
	s      string
	chunk  int
	at     int
	mode   cutMode
	cancel context.CancelFunc

	pos       int
	calls     int
	cancelled bool
	after     int
}

type cutMode int

const (
	cutNil cutMode = iota // the cancelling read returns (n, nil)
	cutEOF                // (n, io.EOF)
	cutErr                // (n, errCutSource)
)

func (m cutMode) String() string {
	return [...]string{"n+nil", "n+EOF", "n+error"}[m]
}

var errCutSource = errors.New("cut source's own error")

const cutSourceMaxCalls = 1 << 16

// cutNever is an offset no input of these tests reaches.
const cutNever = 1 << 30

func (c *cutSource) Read(p []byte) (int, error) {
	if c.cancelled {
		c.after++
		return 0, errors.New("cutSource: read after the cancelling read")
	}
	c.calls++
	if c.calls > cutSourceMaxCalls {
		return 0, errors.New("cutSource: read cap reached")
	}
	if c.pos >= len(c.s) {
		return 0, io.EOF
	}
	n := len(p)
	if c.chunk > 0 && n > c.chunk {
		n = c.chunk
	}
	n = copy(p[:n], c.s[c.pos:])
	c.pos += n
	if c.pos >= c.at {
		c.cancelled = true
		c.cancel()
		switch c.mode {
		case cutEOF:
			return n, io.EOF
		case cutErr:
			return n, errCutSource
		}
	}
	return n, nil
}

// armNext makes the next delivering read cancel.
func (c *cutSource) armNext() { c.at = c.pos + 1 }

// newCut returns a context and a cutSource that cancels it at offset at.
func newCut(t *testing.T, s string, chunk, at int, mode cutMode) (context.Context, *cutSource) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, &cutSource{s: s, chunk: chunk, at: at, mode: mode, cancel: cancel}
}

// checkCancelStop asserts that stop is the stop of a cancelled parse: a
// FatalError without a diagnostic that errors.Is finds the context's error in.
func checkCancelStop(t *testing.T, what string, stop error, ctxErr error) {
	t.Helper()
	if stop == nil {
		t.Fatalf("%s: no stop", what)
	}
	if !IsFatal(stop) || !errors.Is(stop, ctxErr) {
		t.Errorf("%s: stop %v is not a FatalError wrapping %v", what, stop, ctxErr)
	}
	f, ok := stop.(FatalError)
	if !ok {
		t.Fatalf("%s: stop %T is not a FatalError", what, stop)
	}
	if _, has := f.Diagnostic(); has {
		t.Errorf("%s: the cancellation stop carries a diagnostic", what)
	}
}

const cancelPlain = "Content-Type: text/plain\r\n\r\nhello, cancelled world\r\n"

// TestParseContext_preCancelled pins that a context already done reads
// nothing, records nothing, and returns the stop with no entity.
func TestParseContext_preCancelled(t *testing.T) {
	custom := errors.New("caller's own cause")
	cases := map[string]struct {
		ctx  func() context.Context
		want []error
	}{
		"cancelled": {func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, []error{context.Canceled}},
		"past deadline": {func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
			t.Cleanup(cancel)
			return ctx
		}, []error{context.DeadlineExceeded}},
		"custom cause": {func() context.Context {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(custom)
			return ctx
		}, []error{context.Canceled, custom}},
		"deadline cause": {func() context.Context {
			ctx, cancel := context.WithDeadlineCause(context.Background(), time.Unix(1, 0), custom)
			t.Cleanup(cancel)
			return ctx
		}, []error{context.DeadlineExceeded, custom}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := tc.ctx()
			sink := &collectingSink{}
			src := &inputMeter{r: strings.NewReader(cancelPlain)}
			session, e, err := ParseContext(ctx, src, &ParseOptions{DiagnosticSink: sink})
			if src.calls != 0 {
				t.Errorf("the source was read %d times", src.calls)
			}
			if e != nil {
				t.Errorf("entity = %v, want none", e)
			}
			for _, want := range tc.want {
				checkCancelStop(t, "Parse", err, want)
			}
			if stop := session.fatalErr(); stop == nil || error(stop) != err {
				t.Errorf("fatalErr() = %v, want the identical %v", stop, err)
			}
			if session.Len() != 0 || sink.calls != 0 {
				t.Errorf("%d diagnostics, %d sink calls; want none", session.Len(), sink.calls)
			}
		})
	}
}

// TestParseContext_invalidLimitsBeforeCancellation pins that a configuration
// error is reported before the context is consulted at all.
func TestParseContext_invalidLimitsBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &inputMeter{r: strings.NewReader(cancelPlain)}
	session, e, err := ParseContext(ctx, src, &ParseOptions{Limits: Limits{MaxInputBytes: -2}})
	if !errors.Is(err, ErrInvalidLimits) || IsFatal(err) || e != nil || src.calls != 0 {
		t.Fatalf("ParseContext() = %v, %v after %d reads; want ErrInvalidLimits alone", e, err, src.calls)
	}
	if session.fatalErr() != nil || session.cancellable() {
		t.Errorf("the session of a rejected configuration has a stop or a context")
	}
}

// TestParseContext_noCancellationInstallsNothing pins that a context that
// cannot be cancelled is no context: nothing is gated.
func TestParseContext_noCancellationInstallsNothing(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"nil":            nil,
		"background":     context.Background(),
		"without cancel": context.WithoutCancel(func() context.Context { c, cancel := context.WithCancel(context.Background()); cancel(); return c }()),
	} {
		session, e, err := ParseContext(ctx, strings.NewReader(cancelPlain), nil)
		if err != nil || session.cancellable() {
			t.Fatalf("%s: ParseContext() = %v, cancellable %v", name, err, session.cancellable())
		}
		if _, gated := e.Body.(*cancelReader); gated {
			t.Errorf("%s: the body is gated", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, e, _ := ParseContext(ctx, strings.NewReader(cancelPlain), nil)
	if _, gated := e.Body.(*cancelReader); !gated {
		t.Errorf("the body of a cancellable parse is not gated")
	}
}

// TestNewCancelCause pins the cause's shape: one element for a standard
// sentinel given as its own cause, two otherwise, and no comparison that could
// panic on a cause of a type that is not comparable.
func TestNewCancelCause(t *testing.T) {
	cases := []struct {
		name       string
		err, cause error
		n          int
	}{
		{"sentinel twice", context.Canceled, context.Canceled, 1},
		{"deadline twice", context.DeadlineExceeded, context.DeadlineExceeded, 1},
		{"no cause", context.Canceled, nil, 1},
		{"custom", context.Canceled, errCutSource, 2},
		{"not comparable", context.Canceled, notComparable{[]string{"x"}}, 2},
		{"not comparable twice", notComparable{[]string{"y"}}, notComparable{[]string{"y"}}, 2},
		{"different sentinels", context.Canceled, context.DeadlineExceeded, 2},
	}
	for _, tc := range cases {
		c := newCancelCause(tc.err, tc.cause)
		if got := len(c.Unwrap()); got != tc.n {
			t.Errorf("%s: %d causes, want %d", tc.name, got, tc.n)
		}
		if c.Error() == "" {
			t.Errorf("%s: empty message", tc.name)
		}
	}
	// A cause of a type that is not comparable reaches a cancelled parse.
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(notComparable{[]string{"not", "comparable"}})
	_, _, err := ParseContext(ctx, strings.NewReader(cancelPlain), nil)
	checkCancelStop(t, "not comparable cause", err, context.Canceled)
}

// notComparable is an error whose dynamic type is not comparable: comparing
// two of them as interfaces panics.
type notComparable struct{ v []string }

func (e notComparable) Error() string { return strings.Join(e.v, ",") }

// reentrantCtx is a context whose Err and Value read the session they belong
// to, as a Context method may.
type reentrantCtx struct {
	context.Context
	session atomic.Pointer[ParseSession]
	calls   atomic.Int64
}

func (c *reentrantCtx) touch() {
	c.calls.Add(1)
	if s := c.session.Load(); s != nil {
		_ = s.Diagnostics()
		_ = s.Len()
		_ = s.Profile()
		_ = s.Compatibility()
		_ = s.Limits()
	}
}

func (c *reentrantCtx) Err() error            { c.touch(); return c.Context.Err() }
func (c *reentrantCtx) Value(k any) any       { c.touch(); return c.Context.Value(k) }
func (c *reentrantCtx) Done() <-chan struct{} { return c.Context.Done() }

// TestParseContext_contextMethodsMayReadTheSession pins that no session lock is
// held while a Context method runs.
func TestParseContext_contextMethodsMayReadTheSession(t *testing.T) {
	inner, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &reentrantCtx{Context: inner}
	src := &cutSource{s: cancelPlain, chunk: 1, at: cutNever, cancel: cancel}
	session, e, err := ParseContext(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx.session.Store(session)
	src.armNext()
	withinDeadline(t, "a read seeing the cancellation", func() {
		_, err = io.ReadAll(e.Body)
	})
	checkCancelStop(t, "body", err, context.Canceled)
	if ctx.calls.Load() == 0 {
		t.Error("no Context method was called to build the stop")
	}
}

// TestParseContext_firstStopWins pins the identity: a sink's stop published
// before the cancellation stays what is reported, and a body that stayed
// readable after it, as it always did, stops being read once the context is
// cancelled.
func TestParseContext_firstStopWins(t *testing.T) {
	text := "Content-Type: text/plain\r\nContent-Disposition: attachment; a=1; b=2\r\n\r\n" + strings.Repeat("x", 64)
	refuse := errors.New("sink refuses")
	ctx, src := newCut(t, text, 1, cutNever, cutNil)
	session, e, err := ParseContext(ctx, src, &ParseOptions{
		Limits:         Limits{MaxParameterCount: 1},
		DiagnosticSink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return refuse }),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = e.EffectiveContentDisposition()
	sinkStop := e.ParseStop()
	if sinkStop == nil || !errors.Is(sinkStop, refuse) {
		t.Fatalf("ParseStop() = %v, want the sink's stop", sinkStop)
	}
	// The body stays readable after the sink's stop while the context is live.
	buf := make([]byte, 8)
	if n, err := e.Body.Read(buf); n == 0 || err != nil {
		t.Fatalf("Read() after the sink's stop = %d, %v; want bytes", n, err)
	}
	src.armNext()
	_, err = io.ReadAll(e.Body)
	if err != sinkStop {
		t.Errorf("read after cancellation = %v, want the sink's identical stop", err)
	}
	if n, err := e.Body.Read(nil); n != 0 || err != sinkStop {
		t.Errorf("empty read = %d, %v; want the stop", n, err)
	}
	if e.ParseStop() != sinkStop || error(session.fatalErr()) != sinkStop {
		t.Errorf("the stop changed identity")
	}
	var out bytes.Buffer
	if err := e.WriteTo(&out); err != sinkStop || out.Len() != 0 {
		t.Errorf("WriteTo() = %v with %d bytes; want the stop and nothing written", err, out.Len())
	}
	if src.after != 0 {
		t.Errorf("the source was read %d times after the cancelling read", src.after)
	}
}

// TestParseContext_cancellationRecordsNothing pins that the cancellation adds
// no diagnostic and that no sink is called after it was seen, while what was
// recorded before it stays.
func TestParseContext_cancellationRecordsNothing(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; a=1; b=2\r\n\r\none\r\n" +
		"--B\r\nContent-Type: text/plain; a=1; b=2\r\n\r\ntwo\r\n--B--\r\n"
	ctx, src := newCut(t, text, 1, cutNever, cutNil)
	sink := &collectingSink{}
	session, e, err := ParseContext(ctx, src, &ParseOptions{Limits: Limits{MaxParameterCount: 1}, DiagnosticSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	first, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 || session.Len() != 1 {
		t.Fatalf("%d sink calls, %d diagnostics after the first part; want 1", sink.calls, session.Len())
	}
	src.armNext()
	_, _ = io.ReadAll(first.Body)
	p, err := mr.NextPart()
	if p != nil {
		t.Errorf("NextPart() returned a part after cancellation")
	}
	checkCancelStop(t, "NextPart", err, context.Canceled)
	if sink.calls != 1 || session.Len() != 1 {
		t.Errorf("%d sink calls, %d diagnostics after cancellation; want the one from before", sink.calls, session.Len())
	}
}

// TestParseContext_emptyReads pins empty reads: nothing before cancellation,
// the stop after it, and the source untouched either way.
func TestParseContext_emptyReads(t *testing.T) {
	ctx, src := newCut(t, cancelPlain, 1, cutNever, cutNil)
	_, e, err := ParseContext(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := src.calls
	if n, err := e.Body.Read(nil); n != 0 || err != nil || src.calls != calls {
		t.Fatalf("empty read before cancellation = %d, %v, %d source reads", n, err, src.calls-calls)
	}
	src.cancel()
	n, err := e.Body.Read([]byte{})
	checkCancelStop(t, "empty read", err, context.Canceled)
	if n != 0 || src.calls != calls {
		t.Errorf("empty read after cancellation = %d, %d source reads", n, src.calls-calls)
	}
}

// TestParseContext_inFlightRead pins what the read that sees the cancellation
// returns: its bytes, and the stop in place of whatever came with them.
func TestParseContext_inFlightRead(t *testing.T) {
	for _, mode := range []cutMode{cutNil, cutEOF, cutErr} {
		t.Run(mode.String(), func(t *testing.T) {
			ctx, src := newCut(t, cancelPlain, 4, cutNever, mode)
			session, e, err := ParseContext(ctx, src, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Drain what the header read buffered, without cancelling.
			var got []byte
			for len(got) < 1 {
				buf := make([]byte, 1)
				n, err := e.Body.Read(buf)
				got = append(got, buf[:n]...)
				if err != nil {
					t.Fatalf("Read() = %v before cancellation", err)
				}
			}
			src.armNext()
			rest, err := readBy(e.Body, 64)
			got = append(got, rest...)
			checkCancelStop(t, "body", err, context.Canceled)
			if err != error(session.fatalErr()) {
				t.Errorf("the body's stop is not the session's")
			}
			body := cancelPlain[strings.Index(cancelPlain, "\r\n\r\n")+4:]
			if !strings.HasPrefix(body, string(got)) || len(got) < 2 {
				t.Errorf("body %q is not a prefix of %q with the in-flight read's bytes", got, body)
			}
			if src.after != 0 {
				t.Errorf("%d reads after the cancelling read", src.after)
			}
		})
	}
}

// TestParseContext_liveEquivalence pins that a context that is never cancelled
// changes nothing: the same tree, bytes, errors, diagnostics and stops as
// Parse, over plain, healed, opaque, budget and sink-stop cases.
func TestParseContext_liveEquivalence(t *testing.T) {
	withTestCharsets(t)
	refuseAt := func(n int) DiagnosticSink {
		calls := 0
		return DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
			calls++
			if calls == n {
				return errors.New("sink refuses")
			}
			return nil
		})
	}
	type liveCase struct {
		name string
		text string
		opts func() *ParseOptions
	}
	var cases []liveCase
	for _, f := range bodyFixtures() {
		text := f.header + "\r\n" + f.wire
		cases = append(cases, liveCase{"body " + f.name, text, func() *ParseOptions { return nil }})
		for _, kind := range []string{"encoded", "decoded", "total"} {
			cases = append(cases, liveCase{"body " + f.name + " " + kind, text, func() *ParseOptions {
				return &ParseOptions{Limits: onlyBody(kind, 5)}
			}})
		}
	}
	cases = append(cases,
		liveCase{"plain", cancelPlain, func() *ParseOptions { return nil }},
		liveCase{"storm", diagnosticStorm(40), func() *ParseOptions { return nil }},
		liveCase{"storm capped", diagnosticStorm(40), func() *ParseOptions { return &ParseOptions{Limits: Limits{MaxDiagnosticCount: 7}} }},
		liveCase{"storm sink stop", diagnosticStorm(40), func() *ParseOptions { return &ParseOptions{DiagnosticSink: refuseAt(3)} }},
		liveCase{"healed", cancelHealed, func() *ParseOptions { return nil }},
		liveCase{"healed strict", cancelHealed, func() *ParseOptions { return &ParseOptions{Profile: ProfileStrict} }},
		liveCase{"opaque", cancelOpaque, func() *ParseOptions { return nil }},
		liveCase{"unknown decoders", cancelUnknown, func() *ParseOptions { return nil }},
		liveCase{"part count", cancelHealed, func() *ParseOptions { return &ParseOptions{Limits: Limits{MaxPartCount: 2}} }},
		liveCase{"depth", cancelHealed, func() *ParseOptions { return &ParseOptions{Limits: Limits{MaxMIMEDepth: 1}} }},
		liveCase{"input", cancelHealed, func() *ParseOptions { return &ParseOptions{Limits: Limits{MaxInputBytes: 150}} }},
		liveCase{"total header", cancelHealed, func() *ParseOptions { return &ParseOptions{Limits: Limits{MaxTotalHeaderBytes: 120}} }},
		liveCase{"parameters", cancelHealed, func() *ParseOptions { return &ParseOptions{Limits: Limits{MaxParameterCount: 1}} }},
		liveCase{"parameters sink stop", cancelHealed, func() *ParseOptions {
			return &ParseOptions{Limits: Limits{MaxParameterCount: 1}, DiagnosticSink: refuseAt(1)}
		}},
	)
	for _, tc := range cases {
		for _, k := range []int{1, 7, 4096} {
			want := parseDigest(t, tc.text, k, func(r io.Reader) (*ParseSession, *Entity, error) { return Parse(r, tc.opts()) })
			ctx, cancel := context.WithCancel(context.Background())
			got := parseDigest(t, tc.text, k, func(r io.Reader) (*ParseSession, *Entity, error) { return ParseContext(ctx, r, tc.opts()) })
			cancel()
			if got != want {
				t.Errorf("%s, chunks of %d: a live context changed the parse\n got: %s\nwant: %s", tc.name, k, got, want)
			}
		}
	}
}

const cancelHealed = "Content-Type: multipart/mixed; boundary=B; broken\r\n\r\n" +
	"--B\r\nContent-Type: text/plain; charset=utf-8; ;\r\n\r\none\r\n" +
	"--B\r\nContent-Type: multipart/alternative; boundary=C\r\n\r\n" +
	"--C\r\nContent-Type: text/plain\r\n\r\nnested\r\n--C--\r\n" +
	"--B\r\nContent-Type: application/octet-stream; name=a; name=b\r\nContent-Disposition: attachment; filename=x; x=1; y=2\r\n\r\nthree\r\n" +
	"--B--\r\n"

const cancelOpaque = "Content-Type: multipart/mixed; boundary=B; boundary=C\r\n\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\ninside\r\n--B--\r\n"

const cancelUnknown = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain; charset=x-nothing-knows\r\n\r\nraw one\r\n" +
	"--B\r\nContent-Transfer-Encoding: x-unknown\r\n\r\nraw two\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\nthree\r\n--B--\r\n"

// parseDigest parses text, delivered in chunks of k, walks the whole tree —
// reading every leaf body and going on past every error a part comes with —
// then reads the root body again, and describes everything observed. The stop
// is described by identity with the session's, so two digests compare equal
// when the same scopes reported the same stop.
func parseDigest(t *testing.T, text string, k int, parse func(io.Reader) (*ParseSession, *Entity, error)) string {
	t.Helper()
	session, e, err := parse(&inputSource{s: text, chunk: k})
	var b strings.Builder
	stopOf := func(err error) string {
		switch {
		case err == nil:
			return "nil"
		case err == error(session.fatalErr()):
			return "STOP(" + err.Error() + ")"
		case IsFatal(err):
			return "fatal(" + err.Error() + ")"
		}
		return err.Error()
	}
	fmt.Fprintf(&b, "parse=%s entity=%v;", stopOf(err), e != nil)
	if e != nil {
		walkErr := e.Walk(func(path []int, part *Entity, err error) error {
			fmt.Fprintf(&b, " part%v err=%s", path, stopOf(err))
			if part.MultipartReader() == nil {
				body, berr := io.ReadAll(part.Body)
				fmt.Fprintf(&b, " body=%q end=%s", body, stopOf(berr))
			}
			return nil
		})
		fmt.Fprintf(&b, " walk=%s parsestop=%s;", stopOf(walkErr), stopOf(e.ParseStop()))
		rest, rerr := io.ReadAll(e.Body)
		fmt.Fprintf(&b, " rootrest=%q end=%s;", rest, stopOf(rerr))
	}
	for _, d := range session.Diagnostics() {
		fmt.Fprintf(&b, " diag %s %s %v %v %q;", d.Code, d.Severity, d.Path, d.Location, d.Action)
	}
	return b.String()
}

// TestParseContext_waitingDiagnosticAfterCancellation pins the second look
// record takes: a diagnostic that waited behind a delivery to a blocked sink,
// while the context was cancelled, is neither delivered nor retained once the
// wait is over. The delivery already running is not interrupted, and what it
// delivered stays recorded. The sink may read the session while it is blocked.
func TestParseContext_waitingDiagnosticAfterCancellation(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Disposition: attachment; broken\r\n\r\na\r\n" +
		"--B\r\nContent-Disposition: attachment; damaged\r\n\r\nb\r\n--B--\r\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	open := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(open)
	var calls atomic.Int32
	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		if calls.Add(1) == 1 {
			_, _ = s.Len(), s.Diagnostics()
			close(entered)
			<-release
		}
		return nil
	})
	session, e, err := ParseContext(ctx, strings.NewReader(text), &ParseOptions{DiagnosticSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	var parts [2]*Entity
	for i := range parts {
		if parts[i], err = mr.NextPart(); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, parts[i].Body)
	}

	var wg sync.WaitGroup
	t.Cleanup(func() {
		open()
		wg.Wait()
	})
	for _, p := range parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = p.EffectiveContentDisposition()
		}()
		if p == parts[0] {
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the first delivery never reached the sink")
			}
		}
	}
	// Give the second reading time to reach the delivery lock; if it has not
	// yet, the first look catches the cancellation instead, and the outcome
	// asserted below is the same.
	time.Sleep(40 * time.Millisecond)
	cancel()
	stop := e.ParseStop()
	open()
	joined := make(chan struct{})
	go func() {
		wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("a reading did not return after its sink was released")
	}

	checkCancelStop(t, "ParseStop", stop, context.Canceled)
	if got := calls.Load(); got != 1 {
		t.Errorf("the sink was called %d times, want once: the waiting diagnostic was delivered after the cancellation", got)
	}
	diags := session.Diagnostics()
	if len(diags) != 1 || diags[0].Code != CodeMIMEDispositionInvalid {
		t.Errorf("diagnostics %v, want only the one delivered before the cancellation", codesOfDiags(diags))
	}
	if e.ParseStop() != stop {
		t.Errorf("the stop changed identity")
	}
}
