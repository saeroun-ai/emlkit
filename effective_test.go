package message

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ROB-100-007: EffectiveContentType and EffectiveContentDisposition report the
// metadata an entity was constructed with, as its parse interpreted it, read at
// most once and returned as copies. See effective.go.

const effectiveMultipart = "Content-Type: multipart/mixed; boundary=B; a=1\r\n" +
	"Content-Disposition: inline; a=1; b=2\r\n\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\none\r\n" +
	"--B--\r\n"

func parseEffective(t *testing.T, text string, limits Limits, sink DiagnosticSink) (*ParseSession, *Entity) {
	t.Helper()
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits, DiagnosticSink: sink})
	if err != nil || e == nil {
		t.Fatalf("Parse() = (%v, %v)", e, err)
	}
	return session, e
}

// TestEffective_snapshotIgnoresHeaderEdits pins the mutation contract: an edit
// to Entity.Header after construction is visible through the header and its
// standalone accessors but does not change the effective metadata or what the
// body is framed by.
func TestEffective_snapshotIgnoresHeaderEdits(t *testing.T) {
	session, e := parseEffective(t, effectiveMultipart, paramsOnly(2, 0), nil)

	e.Header.Set("Content-Type", "text/html; charset=x-other")
	e.Header.Set("Content-Disposition", "attachment")

	if mt, params, err := e.EffectiveContentType(); err != nil || mt != "multipart/mixed" || !reflect.DeepEqual(params, map[string]string{"boundary": "B", "a": "1"}) {
		t.Errorf("EffectiveContentType() = (%q, %v, %v), want the type it was parsed with", mt, params, err)
	}
	if disp, params, err := e.EffectiveContentDisposition(); err != nil || disp != "inline" || !reflect.DeepEqual(params, map[string]string{"a": "1", "b": "2"}) {
		t.Errorf("EffectiveContentDisposition() = (%q, %v, %v), want the field as constructed", disp, params, err)
	}
	if mt, _, _ := e.Header.ContentType(); mt != "text/html" {
		t.Errorf("Header.ContentType() = %q, want the edited value", mt)
	}
	if disp, _, _ := e.Header.ContentDisposition(); disp != "attachment" {
		t.Errorf("Header.ContentDisposition() = %q, want the edited value", disp)
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil, want the parts it was parsed with")
	}
	var bodies []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart() = %v", err)
		}
		bodies = append(bodies, readBodyOf(t, p))
	}
	if !reflect.DeepEqual(bodies, []string{"zero", "one"}) {
		t.Errorf("parts = %q, want zero and one", bodies)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none: both fields fit two parameters", codesOfDiags(d))
	}
}

// TestEffective_returnedMapsAreCopies pins that no returned map aliases the
// entity: modifying one changes neither a later result nor the boundary the
// body is framed by.
func TestEffective_returnedMapsAreCopies(t *testing.T) {
	_, e := parseEffective(t, effectiveMultipart, Limits{}, nil)

	_, ct, _ := e.EffectiveContentType()
	delete(ct, "boundary")
	ct["injected"] = "x"
	_, cd, _ := e.EffectiveContentDisposition()
	delete(cd, "a")
	cd["injected"] = "x"

	if _, again, _ := e.EffectiveContentType(); !reflect.DeepEqual(again, map[string]string{"boundary": "B", "a": "1"}) {
		t.Errorf("EffectiveContentType() after editing a result = %v", again)
	}
	if _, again, _ := e.EffectiveContentDisposition(); !reflect.DeepEqual(again, map[string]string{"a": "1", "b": "2"}) {
		t.Errorf("EffectiveContentDisposition() after editing a result = %v", again)
	}
	mr := e.MultipartReader()
	p, err := mr.NextPart()
	if err != nil || readBodyOf(t, p) != "zero" {
		t.Errorf("first part after editing a result = (%v, %v), want it framed by the original boundary", p, err)
	}
}

// TestEffective_dispositionIsReadOnce pins that the disposition budget is
// diagnosed exactly once, whoever asks and however often: repeated calls, a
// copy of the Entity value, concurrent callers, and the charset stage that read
// it during construction.
func TestEffective_dispositionIsReadOnce(t *testing.T) {
	text := "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; a=1; b=2\r\n\r\nbody"
	limits := paramsOnly(1, 0)
	wantDisp := func(t *testing.T, e *Entity) {
		t.Helper()
		if disp, params, err := e.EffectiveContentDisposition(); err != nil || disp != "attachment" || !reflect.DeepEqual(params, map[string]string{"a": "1"}) {
			t.Errorf("EffectiveContentDisposition() = (%q, %v, %v), want attachment with the parameter within the budget", disp, params, err)
		}
	}
	once := func(t *testing.T, session *ParseSession) {
		t.Helper()
		if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterCount}) {
			t.Errorf("Diagnostics() = %v, want exactly one limit.parameter_count", got)
		}
	}

	t.Run("lazily, repeated and through a copy", func(t *testing.T) {
		session, e := parseEffective(t, text, limits, nil)
		if d := session.Diagnostics(); d != nil {
			t.Fatalf("Diagnostics() before anything asked = %v, want none", codesOfDiags(d))
		}
		for range 3 {
			wantDisp(t, e)
		}
		cp := *e
		wantDisp(t, &cp)
		once(t, session)
	})

	t.Run("concurrently", func(t *testing.T) {
		session, e := parseEffective(t, text, limits, nil)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { e.EffectiveContentDisposition() })
		}
		wg.Wait()
		wantDisp(t, e)
		once(t, session)
	})

	t.Run("after the charset stage read it", func(t *testing.T) {
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{
			Limits:      limits,
			ReadOptions: ReadOptions{SkipTextAttachmentDecoding: true},
		})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		once(t, session)
		wantDisp(t, e)
		wantDisp(t, e)
		once(t, session)
	})
}

// TestEffective_afterAStopKeepsTheBoundedResult pins that reading the
// disposition after a sink stopped the parse still bounds the work and returns
// what fits, but records nothing about a parse that has ended.
func TestEffective_afterAStopKeepsTheBoundedResult(t *testing.T) {
	sentinel := errors.New("stop")
	text := "Content-Type: text/plain; a=1; b=2\r\nContent-Disposition: attachment; a=1; b=2\r\n\r\nbody"
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{
		Limits:         paramsOnly(1, 0),
		DiagnosticSink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return sentinel }),
	})
	if e == nil || !IsFatal(err) || !errors.Is(err, sentinel) {
		t.Fatalf("Parse() = (%v, %v), want the entity and the stop on the Content-Type budget", e, err)
	}
	if stop := e.ParseStop(); stop != err {
		t.Errorf("ParseStop() = %v, want the identical stop %v", stop, err)
	}
	if disp, params, dispErr := e.EffectiveContentDisposition(); dispErr != nil || disp != "attachment" || !reflect.DeepEqual(params, map[string]string{"a": "1"}) {
		t.Errorf("EffectiveContentDisposition() = (%q, %v, %v), want the bounded result", disp, params, dispErr)
	}
	diags := session.Diagnostics()
	if len(diags) != 1 || diags[0].Field != "Content-Type" {
		t.Errorf("Diagnostics() = %+v, want only the Content-Type budget that stopped the parse", diags)
	}
}

// TestEffective_parseStopIsTheWholeParseOnly pins what ParseStop reports: nil
// for entry points with no session and for a parse still running, the identical
// published value for a parse-wide failure, and nothing for a failure confined
// to one multipart iterator.
func TestEffective_parseStopIsTheWholeParseOnly(t *testing.T) {
	e, err := Read(strings.NewReader(effectiveMultipart))
	if err != nil || e.ParseStop() != nil {
		t.Errorf("Read(): ParseStop() = %v, want nil", e.ParseStop())
	}
	built, _ := New(Header{}, strings.NewReader("x"))
	if built.ParseStop() != nil || (&Entity{}).ParseStop() != nil {
		t.Error("ParseStop() of a constructed or literal entity is not nil")
	}

	nested := "Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\nContent-Type: multipart/mixed; boundary=IN\r\n\r\n" +
		"--IN\r\nContent-Type: text/plain\r\n\r\ndeep\r\n--IN--\r\n" +
		"--OUT\r\nContent-Type: text/plain\r\n\r\nshallow\r\n" +
		"--OUT--\r\n"
	limits := allDisabled()
	limits.MaxMIMEDepth = 1
	_, root := parseEffective(t, nested, limits, nil)
	inner, err := root.MultipartReader().NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v", err)
	}
	if _, err := inner.MultipartReader().NextPart(); !IsFatal(err) {
		t.Fatalf("inner NextPart() = %v, want the depth refusal", err)
	}
	if stop := root.ParseStop(); stop != nil {
		t.Errorf("ParseStop() after a depth refusal = %v, want nil: it ended one iterator", stop)
	}

	limits = allDisabled()
	limits.MaxPartCount = 1
	_, root = parseEffective(t, effectiveMultipart, limits, nil)
	mr := root.MultipartReader()
	if _, err := mr.NextPart(); err != nil {
		t.Fatalf("NextPart() = %v", err)
	}
	_, refused := mr.NextPart()
	if !IsFatal(refused) || root.ParseStop() != refused {
		t.Errorf("ParseStop() = %v, want the identical part-count failure %v", root.ParseStop(), refused)
	}
}

// TestEffective_legacyAndLiteralEntities pins the entry points with no budgets:
// their effective metadata is the standalone accessors' answer for the header
// as constructed, including for a field the accessors cannot parse, and a
// struct literal has no snapshot and reads its current header.
func TestEffective_legacyAndLiteralEntities(t *testing.T) {
	var many strings.Builder
	many.WriteString("Content-Type: text/plain")
	for i := range 400 {
		fmt.Fprintf(&many, "; x%d=%d", i, i)
	}
	many.WriteString("\r\nContent-Disposition: attachment; a=1; a=2\r\n\r\nbody")

	e, err := Read(strings.NewReader(many.String()))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	wantType, wantTypeParams, wantTypeErr := e.Header.ContentType()
	gotType, gotTypeParams, gotTypeErr := e.EffectiveContentType()
	if gotType != wantType || !reflect.DeepEqual(gotTypeParams, wantTypeParams) || gotTypeErr != wantTypeErr || len(gotTypeParams) != 400 {
		t.Errorf("EffectiveContentType() = (%q, %d params, %v), want Header.ContentType's unbudgeted answer", gotType, len(gotTypeParams), gotTypeErr)
	}
	wantDisp, wantDispParams, wantDispErr := e.Header.ContentDisposition()
	gotDisp, gotDispParams, gotDispErr := e.EffectiveContentDisposition()
	if gotDisp != wantDisp || gotDispParams != nil || wantDispParams != nil || (gotDispErr == nil) != (wantDispErr == nil) {
		t.Errorf("EffectiveContentDisposition() = (%q, %v, %v), want Header.ContentDisposition's (%q, %v, %v)",
			gotDisp, gotDispParams, gotDispErr, wantDisp, wantDispParams, wantDispErr)
	}

	malformed, _ := Read(strings.NewReader("Content-Type: text/plain; =bad\r\n\r\nbody"))
	mt, params, ctErr := malformed.EffectiveContentType()
	if mt != "text/plain; =bad" || params != nil || ctErr == nil {
		t.Errorf("EffectiveContentType() of an unparseable field = (%q, %v, %v), want the raw value, no parameters and the error", mt, params, ctErr)
	}

	bare, _ := New(Header{}, strings.NewReader("x"))
	if mt, params, err := bare.EffectiveContentType(); mt != "text/plain" || params != nil || err != nil {
		t.Errorf("EffectiveContentType() with no field = (%q, %v, %v), want text/plain", mt, params, err)
	}
	if disp, params, err := bare.EffectiveContentDisposition(); disp != "" || params != nil || err == nil {
		t.Errorf("EffectiveContentDisposition() with no field = (%q, %v, %v), want the accessor's empty result and error", disp, params, err)
	}

	var h Header
	h.Set("Content-Type", "image/png")
	literal := &Entity{Header: h}
	h.Set("Content-Disposition", "attachment; filename=a.png")
	if mt, _, _ := literal.EffectiveContentType(); mt != "image/png" {
		t.Errorf("literal EffectiveContentType() = %q, want the current header's", mt)
	}
	if disp, params, _ := literal.EffectiveContentDisposition(); disp != "attachment" || params["filename"] != "a.png" {
		t.Errorf("literal EffectiveContentDisposition() = (%q, %v), want the current header's", disp, params)
	}
}

// withinDeadline runs f and fails the test if it has not returned in time,
// which is how a sink re-entering a lock its own delivery holds shows up.
func withinDeadline(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: a sink reading the entity blocked its own delivery", what)
	}
}

// reentrantText has a Content-Disposition past a one-parameter budget and
// nothing else past a budget, so the only diagnostic is the one its first
// reading owes, and Parse itself delivers nothing.
const reentrantText = "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; a=1; a=2\r\n\r\nbody"

// reentrantSink returns a sink that, while it is told about the disposition
// budget, reads back the entity it is about through every effective accessor
// and ParseStop, records what it saw, and then returns result.
type reentrantSink struct {
	target atomic.Pointer[Entity]
	result error

	mu    sync.Mutex
	calls int
	seen  []string
}

func (s *reentrantSink) Diagnose(_ *ParseSession, d Diagnostic) error {
	e := s.target.Load()
	if e == nil || d.Code != CodeLimitParameterCount {
		return nil
	}
	disp, params, err := e.EffectiveContentDisposition()
	mt, _, _ := e.EffectiveContentType()
	stop := e.ParseStop()
	s.mu.Lock()
	s.calls++
	s.seen = append(s.seen, fmt.Sprintf("%s %v %v %s %v", disp, params, err, mt, stop))
	s.mu.Unlock()
	return s.result
}

// TestEffective_sinkMayReadTheEntityItIsToldAbout is the regression for the
// review finding on ROB-100-007: the first reading of a disposition past a
// budget delivers its diagnostic synchronously, and a sink that reads the same
// entity back — directly or through a copy of the Entity value — must get the
// interpretation just published rather than wait for the delivery it is part
// of. The diagnostic is recorded once, the sink sees the bounded result, and a
// stop the sink returns afterwards is the parse's sticky stop.
func TestEffective_sinkMayReadTheEntityItIsToldAbout(t *testing.T) {
	const wantSeen = "attachment map[a:1] <nil> text/plain <nil>"
	sentinel := errors.New("stop after reading")
	for _, tc := range []struct {
		name   string
		result error
		copied bool
	}{
		{"sink continues", nil, false},
		{"sink stops afterwards", sentinel, false},
		{"sink reads a copy and stops afterwards", sentinel, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &reentrantSink{result: tc.result}
			session, e, err := Parse(strings.NewReader(reentrantText), &ParseOptions{
				Limits:         paramsOnly(1, 0),
				DiagnosticSink: sink,
			})
			if err != nil || e == nil {
				t.Fatalf("Parse() = (%v, %v), want the entity with nothing read yet", e, err)
			}
			if tc.copied {
				cp := *e
				sink.target.Store(&cp)
			} else {
				sink.target.Store(e)
			}

			for i := range 3 {
				withinDeadline(t, "EffectiveContentDisposition", func() {
					disp, params, err := e.EffectiveContentDisposition()
					if err != nil || disp != "attachment" || !reflect.DeepEqual(params, map[string]string{"a": "1"}) {
						t.Errorf("call %d: EffectiveContentDisposition() = (%q, %v, %v), want the bounded attachment", i, disp, params, err)
					}
				})
			}
			if sink.calls != 1 || len(sink.seen) != 1 || sink.seen[0] != wantSeen {
				t.Errorf("sink ran %d times and saw %q, want once and %q", sink.calls, sink.seen, wantSeen)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterCount}) {
				t.Errorf("Diagnostics() = %v, want exactly one limit.parameter_count", got)
			}

			stop := e.ParseStop()
			if tc.result == nil {
				if stop != nil {
					t.Errorf("ParseStop() = %v, want nil: the sink did not stop", stop)
				}
				return
			}
			if !IsFatal(stop) || !errors.Is(stop, sentinel) {
				t.Fatalf("ParseStop() = %v, want a FatalError wrapping the sink's error", stop)
			}
			for range 3 {
				if again := e.ParseStop(); again != stop {
					t.Errorf("ParseStop() = %v, want the identical stop %v", again, stop)
				}
			}
			if err := e.Walk(func([]int, *Entity, error) error { return nil }); err != stop {
				t.Errorf("Walk() = %v, want the identical stop", err)
			}
		})
	}
}

// TestEffective_concurrentFirstReadersWithAReadingSink combines concurrent
// first readers with a sink that reads the entity back: nobody blocks, every
// reader gets the same bounded result, and the diagnostic is recorded once.
func TestEffective_concurrentFirstReadersWithAReadingSink(t *testing.T) {
	sink := &reentrantSink{}
	session, e, err := Parse(strings.NewReader(reentrantText), &ParseOptions{
		Limits:         paramsOnly(1, 0),
		DiagnosticSink: sink,
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	sink.target.Store(e)

	results := make([]string, 16)
	withinDeadline(t, "concurrent EffectiveContentDisposition", func() {
		var wg sync.WaitGroup
		for i := range results {
			wg.Go(func() {
				disp, params, err := e.EffectiveContentDisposition()
				results[i] = fmt.Sprintf("%s %v %v", disp, params, err)
			})
		}
		wg.Wait()
	})
	for i, r := range results {
		if r != "attachment map[a:1] <nil>" {
			t.Errorf("reader %d got %q, want the bounded attachment", i, r)
		}
	}
	if sink.calls != 1 {
		t.Errorf("sink ran %d times, want once", sink.calls)
	}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterCount}) {
		t.Errorf("Diagnostics() = %v, want exactly one limit.parameter_count", got)
	}
}
