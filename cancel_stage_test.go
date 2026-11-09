package message

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Cancellation at the stages a sweep does not single out (ROB-001-006):
// decoders holding output, a CharsetReader that hides errors or reads eagerly,
// header metadata, budgets published before or after it, a caller-owned source
// made to return, and a body read while another goroutine cancels. All of it
// is finite: no source here is longer than 8 MiB, and the one that blocks is
// released by the test's own close and by its cleanup.

// parseArmed parses text in reads of one byte under a context nothing has
// cancelled yet, and returns the parse together with its source, which the
// test arms, or cancels through directly, when the cancellation is to come.
func parseArmed(t *testing.T, text string, opts *ParseOptions) (*ParseSession, *Entity, *cutSource, error) {
	t.Helper()
	ctx, src := newCut(t, text, 1, cutNever, cutNil)
	session, e, err := ParseContext(ctx, src, opts)
	return session, e, src, err
}

// TestParseContext_heldDecoderOutputIsNotDelivered pins that output a decoder
// made ahead is not handed out once the cancellation has been seen.
func TestParseContext_heldDecoderOutputIsNotDelivered(t *testing.T) {
	withTestCharsets(t)
	_, e, src, err := parseArmed(t, "Content-Type: text/plain; charset=x-test-expand\r\n\r\nabcdefghijklmnop", nil)
	if err != nil {
		t.Fatal(err)
	}
	src.armNext()
	got, err := readBy(e.Body, 1)
	checkCancelStop(t, "body", err, context.Canceled)
	if len(got) != 1 || !strings.HasPrefix("aaaabbbbcccc", string(got)) {
		t.Errorf("delivered %q, want the one byte of the read that saw the cancellation", got)
	}
	if src.after != 0 {
		t.Errorf("%d reads after the cancelling read", src.after)
	}
}

// TestParseContext_maskingCharsetCannotHideCancellation pins that a decoder
// turning the input's failure into io.EOF, or into its own error, does not
// make a cancelled body look complete or merely broken.
func TestParseContext_maskingCharsetCannotHideCancellation(t *testing.T) {
	withTestCharsets(t)
	for _, cs := range []string{"x-test-mask-eof", "x-test-mask-other"} {
		for _, mode := range []cutMode{cutNil, cutEOF, cutErr} {
			text := "Content-Type: text/plain; charset=" + cs + "\r\n\r\n0123456789"
			_, e, src, err := parseArmed(t, text, nil)
			if err != nil {
				t.Fatal(err)
			}
			src.mode = mode
			src.armNext()
			got, err := readBy(e.Body, 4)
			if err != e.ParseStop() || err == nil {
				t.Errorf("%s %s: body ended with %v, want the stop", cs, mode, err)
			}
			if !strings.HasPrefix("0123456789", string(got)) {
				t.Errorf("%s %s: delivered %q", cs, mode, got)
			}
		}
	}
}

// TestParseContext_eagerCharsetReader pins that a CharsetReader running into a
// cancellation while it builds its decoder does not leave an unknown-charset
// diagnostic behind, and that the entity comes back with the stop.
func TestParseContext_eagerCharsetReader(t *testing.T) {
	var cancel context.CancelFunc
	withCharsetReader(t, func(cs string, in io.Reader) (io.Reader, error) {
		buf := make([]byte, 3)
		switch cs {
		case "x-cancel-fail":
			_, _ = in.Read(buf)
			cancel()
			_, err := io.ReadAll(in)
			return nil, errors.Join(errors.New("hook failed"), err)
		case "x-cancel-ok":
			n, _ := in.Read(buf)
			buf = buf[:n]
			cancel()
			rest, _ := io.ReadAll(in)
			return bytes.NewReader(append(buf, rest...)), nil
		}
		return nil, errors.New("no such charset")
	})
	for _, cs := range []string{"x-cancel-fail", "x-cancel-ok"} {
		t.Run(cs, func(t *testing.T) {
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background())
			defer cancel()
			text := "Content-Type: text/plain; charset=" + cs + "\r\n\r\nbody bytes"
			session, e, err := ParseContext(ctx, &cutSource{s: text, chunk: 1, at: cutNever, cancel: func() {}}, nil)
			if e == nil {
				t.Fatalf("no entity: %v", err)
			}
			checkCancelStop(t, "Parse", err, context.Canceled)
			if IsUnknownCharset(err) {
				t.Errorf("the stop reads as an unknown charset")
			}
			if session.Len() != 0 {
				t.Errorf("diagnostics %v, want none", codesOfDiags(session.Diagnostics()))
			}
			if _, rerr := io.ReadAll(e.Body); rerr != err {
				t.Errorf("body = %v, want the stop", rerr)
			}
		})
	}

	// A charset that is genuinely unknown while the context is live is still
	// the warning it always was, and its diagnostic stays after a later
	// cancellation.
	t.Run("genuine", func(t *testing.T) {
		ctx, c := context.WithCancel(context.Background())
		defer c()
		session, e, err := ParseContext(ctx, strings.NewReader("Content-Type: text/plain; charset=x-nothing\r\n\r\nraw"), nil)
		if !IsUnknownCharset(err) || IsFatal(err) || e == nil {
			t.Fatalf("Parse() = %v, want the warning and the entity", err)
		}
		if b, err := io.ReadAll(e.Body); string(b) != "raw" || err != nil {
			t.Fatalf("body = %q, %v", b, err)
		}
		c()
		if _, err := e.Body.Read(make([]byte, 1)); err != e.ParseStop() || err == nil {
			t.Errorf("read after cancellation = %v", err)
		}
		if codes := codesOfDiags(session.Diagnostics()); len(codes) != 1 || codes[0] != CodeCharsetUnknown {
			t.Errorf("diagnostics %v, want the one charset.unknown from before", codes)
		}
	})
}

// cancellingWords installs a CharsetReader that cancels when it is asked to
// decode an encoded word in the charset x-cancel-word, and counts the calls.
func cancellingWords(t *testing.T, cancel *context.CancelFunc) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	withCharsetReader(t, func(cs string, in io.Reader) (io.Reader, error) {
		if cs == "x-cancel-word" {
			calls.Add(1)
			(*cancel)()
		}
		return in, nil
	})
	return &calls
}

// TestParseContext_metadataCancellation pins that a structured field whose
// reading the cancellation reached is not interpreted at all: the raw header
// stays, no partial parameters are handed out, nothing is recorded, and the
// stop is the error.
func TestParseContext_metadataCancellation(t *testing.T) {
	var cancel context.CancelFunc
	calls := cancellingWords(t, &cancel)
	word := `"=?x-cancel-word?Q?v?="`
	for name, tc := range map[string]struct {
		ct     string
		limits Limits
	}{
		"whole field":     {"text/plain; a=" + word + "; b=2", Limits{}},
		"unbudgeted":      {"text/plain; a=" + word + "; b=2", Limits{MaxParameterCount: LimitDisabled, MaxParameterSegments: LimitDisabled}},
		"repair":          {"text/plain; a=1; broken; c=" + word, Limits{}},
		"budget fallback": {"text/plain; a=" + word + "; b=2; c=3", Limits{MaxParameterCount: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			ct := tc.ct
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background())
			defer cancel()
			text := "Content-Type: " + ct + "\r\n\r\nbody"
			session, e, err := ParseContext(ctx, strings.NewReader(text), &ParseOptions{Limits: tc.limits})
			if e == nil {
				t.Fatalf("no entity: %v", err)
			}
			checkCancelStop(t, "Parse", err, context.Canceled)
			if got := e.Header.Get("Content-Type"); got != ct {
				t.Errorf("raw header %q, want %q", got, ct)
			}
			mt, params, merr := e.EffectiveContentType()
			if mt != "" || params != nil || merr != err {
				t.Errorf("EffectiveContentType() = %q, %v, %v; want nothing and the stop", mt, params, merr)
			}
			if session.Len() != 0 {
				t.Errorf("diagnostics %v, want none", codesOfDiags(session.Diagnostics()))
			}
		})
	}

	t.Run("disposition", func(t *testing.T) {
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		before := calls.Load()
		text := "Content-Type: text/plain\r\nContent-Disposition: attachment; filename=" + word + "\r\n\r\nbody"
		session, e, err := ParseContext(ctx, strings.NewReader(text), nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			disp, params, derr := e.EffectiveContentDisposition()
			if disp != "" || params != nil || derr == nil || derr != e.ParseStop() {
				t.Errorf("call %d: EffectiveContentDisposition() = %q, %v, %v; want nothing and the stop", i, disp, params, derr)
			}
		}
		if got := calls.Load() - before; got != 1 {
			t.Errorf("the hook ran %d times, want once: a cancelled reading is not repeated past its first poll", got)
		}
		if e.disposition.observedErr() != nil {
			t.Errorf("a cancelled reading was published")
		}
		if session.Len() != 0 {
			t.Errorf("diagnostics %v, want none", codesOfDiags(session.Diagnostics()))
		}
	})
}

// TestScanParametersPoll pins the scan's work boundaries: a poll between items
// and at most cancelScanStride bytes between two polls within one, escaped
// pairs included, and a cancelled scan reporting nothing at all.
func TestScanParametersPoll(t *testing.T) {
	raw := `text/plain; a="` + strings.Repeat("x", 3*cancelScanStride) + `"; b=2`
	var polls int
	scan, cancelled := scanParametersPoll(raw, LimitDisabled, LimitDisabled, func() bool { polls++; return false })
	if cancelled || len(scan.occurrences) != 2 {
		t.Fatalf("scan = %+v, %v", scan, cancelled)
	}
	if polls < 3 {
		t.Errorf("%d polls across %d bytes, want one every %d", polls, len(raw), cancelScanStride)
	}
	want := scanParameters(raw, LimitDisabled, LimitDisabled)
	if scan.head != want.head || len(scan.occurrences) != len(want.occurrences) {
		t.Errorf("a polled scan differs from the plain one")
	}

	// Escaped pairs are two bytes crossed in one step, and the bound is in
	// bytes: a quoted value of nothing but escapes still polls once every
	// cancelScanStride bytes. Three polls are the ones between items (before a,
	// before b, and before the end); every other one is within the value.
	escaped := `text/plain; a="` + strings.Repeat(`\"`, 3*cancelScanStride) + `"; b=2`
	escPolls := 0
	escScan, escCancelled := scanParametersPoll(escaped, LimitDisabled, LimitDisabled, func() bool { escPolls++; return false })
	if escCancelled || len(escScan.occurrences) != 2 {
		t.Fatalf("escaped scan = %+v, %v", escScan, escCancelled)
	}
	if want := len(escaped)/cancelScanStride + 3; escPolls < want {
		t.Errorf("%d polls across %d escape-heavy bytes, want at least %d: the stride is counted in steps, not bytes", escPolls, len(escaped), want)
	}

	for stopAt := 1; stopAt <= polls; stopAt++ {
		n := 0
		scan, cancelled := scanParametersPoll(raw, LimitDisabled, LimitDisabled, func() bool { n++; return n >= stopAt })
		if !cancelled || scan.occurrences != nil || scan.refusal != nil {
			t.Errorf("stopped at poll %d: %+v, %v; want a cancelled, empty scan", stopAt, scan, cancelled)
		}
	}

	if _, _, cancelled, _ := parseHeaderWithParamsPoll("text/plain; a=1", func() bool { return true }); !cancelled {
		t.Error("parseHeaderWithParamsPoll ignored its poll")
	}
	f, p, cancelled, err := parseHeaderWithParamsPoll("text/plain; a=1", nil)
	wf, wp, werr := parseHeaderWithParams("text/plain; a=1")
	if cancelled || f != wf || p["a"] != wp["a"] || err != werr {
		t.Error("parseHeaderWithParamsPoll without a poll differs from parseHeaderWithParams")
	}
}

// TestParseContext_budgetPrecedence pins the order between cancellation and
// the body and input budgets: a body's local failure gives way to the
// cancellation, and a stop of the parse published first stays the identity.
func TestParseContext_budgetPrecedence(t *testing.T) {
	const header = "Content-Type: text/plain\r\n\r\n"
	body := strings.Repeat("b", 32)

	t.Run("local then cancel", func(t *testing.T) {
		_, e, src, err := parseArmed(t, header+body, &ParseOptions{Limits: Limits{MaxEncodedBodyBytes: 4}})
		if err != nil {
			t.Fatal(err)
		}
		_, local := io.ReadAll(e.Body)
		if !IsFatal(local) || e.ParseStop() != nil {
			t.Fatalf("local failure %v, stop %v", local, e.ParseStop())
		}
		src.cancel()
		_, err = e.Body.Read(make([]byte, 4))
		checkCancelStop(t, "after the local failure", err, context.Canceled)
		if err == local || err != e.ParseStop() {
			t.Errorf("read = %v, want the cancellation, not the local failure %v", err, local)
		}
	})

	for name, limits := range map[string]Limits{
		"total then cancel": {MaxTotalDecodedBytes: 4},
		"input then cancel": {MaxInputBytes: int64(len(header) + 4)},
	} {
		t.Run(name, func(t *testing.T) {
			session, e, src, err := parseArmed(t, header+body, &ParseOptions{Limits: limits})
			if err != nil {
				t.Fatal(err)
			}
			_, first := io.ReadAll(e.Body)
			stop := e.ParseStop()
			if stop == nil || first != stop {
				t.Fatalf("body ended with %v, stop %v; want the budget's stop", first, stop)
			}
			if _, has := stop.(FatalError).Diagnostic(); !has {
				t.Fatalf("the budget's stop has no diagnostic")
			}
			n := session.Len()
			src.cancel()
			if _, err := e.Body.Read(make([]byte, 4)); err != stop {
				t.Errorf("read after cancellation = %v, want the budget's identical stop", err)
			}
			if e.ParseStop() != stop || session.Len() != n {
				t.Errorf("the stop changed, or the cancellation recorded something")
			}
		})
	}

	t.Run("cancel then budgets", func(t *testing.T) {
		session, e, src, err := parseArmed(t, header+body, &ParseOptions{Limits: Limits{MaxInputBytes: int64(len(header) + 4), MaxEncodedBodyBytes: 2}})
		if err != nil {
			t.Fatal(err)
		}
		src.cancel()
		_, err = io.ReadAll(e.Body)
		checkCancelStop(t, "body", err, context.Canceled)
		if session.Len() != 0 {
			t.Errorf("diagnostics %v, want none after the cancellation", codesOfDiags(session.Diagnostics()))
		}
	})
}

// TestParseContext_callerClosesItsSource pins the cooperative boundary: a
// source the caller owns, blocked in Read, returns when the caller closes it
// from a context.AfterFunc of its own, and the parse reports its stop.
func TestParseContext_callerClosesItsSource(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		server.Close()
		client.Close()
	})
	go func() {
		_, _ = io.WriteString(server, cancelPlain[:strings.Index(cancelPlain, "\r\n\r\n")+4]+"partial")
		<-done
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopClosing := context.AfterFunc(ctx, func() { client.Close() })
	defer stopClosing()

	_, e, err := ParseContext(ctx, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("partial"))
	if _, err := io.ReadFull(e.Body, buf); err != nil || string(buf) != "partial" {
		t.Fatalf("body = %q, %v", buf, err)
	}
	var rerr error
	time.AfterFunc(20*time.Millisecond, cancel)
	withinDeadline(t, "a body read the caller's close released", func() {
		_, rerr = e.Body.Read(make([]byte, 8))
	})
	checkCancelStop(t, "body", rerr, context.Canceled)
	if rerr != e.ParseStop() {
		t.Errorf("body = %v, want the parse's stop", rerr)
	}
}

// generatedBody is a lazily generated message of a header and n bytes of 'x',
// which never holds more than one read's worth of it.
type generatedBody struct {
	header string
	n      int
	pos    int
	seen   atomic.Int64
}

func (g *generatedBody) Read(p []byte) (int, error) {
	total := len(g.header) + g.n
	if g.pos >= total {
		return 0, io.EOF
	}
	k := 0
	for k < len(p) && g.pos < total {
		if g.pos < len(g.header) {
			p[k] = g.header[g.pos]
		} else {
			p[k] = 'x'
		}
		k++
		g.pos++
	}
	g.seen.Store(int64(g.pos))
	return k, nil
}

// TestParseContext_concurrentCancellation cancels from another goroutine while
// a body of a generated 8 MiB stream is being read. Under the race detector it
// checks that cancellation and reading share no unsynchronized state.
func TestParseContext_concurrentCancellation(t *testing.T) {
	const size = 8 << 20
	src := &generatedBody{header: "Content-Type: application/octet-stream\r\n\r\n", n: size}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, e, err := ParseContext(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The canceller is bounded three ways — the test finishing, a deadline,
	// and reaching its threshold — and is joined on every way out, so it never
	// outlives the test even if a regression ends the body early.
	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	t.Cleanup(func() {
		close(finished)
		<-watcherDone
	})
	go func() {
		defer close(watcherDone)
		deadline := time.After(10 * time.Second)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for src.seen.Load() < 64<<10 {
			select {
			case <-finished:
				return
			case <-deadline:
				return
			case <-tick.C:
			}
		}
		cancel()
	}()
	var total int
	buf := make([]byte, 4096)
	var end error
	for range size/len(buf) + 2 {
		n, err := e.Body.Read(buf)
		if bytes.Count(buf[:n], []byte{'x'}) != n {
			t.Fatalf("delivered bytes other than the stream's")
		}
		total += n
		if err != nil {
			end = err
			break
		}
	}
	switch {
	case end == io.EOF && total == size:
	case end != nil && end == e.ParseStop():
		checkCancelStop(t, "body", end, context.Canceled)
	default:
		t.Errorf("body ended with %v after %d bytes", end, total)
	}
}
