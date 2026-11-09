package message

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

// notFatal has everything a FatalError has except the marker method, which is
// what keeps the interface sealed: an unexported method can only be
// implemented in the package that declares it, so only this package can
// produce a value callers may trust to be the parser's.
type notFatal struct{}

func (notFatal) Error() string                  { return "impostor" }
func (notFatal) Diagnostic() (Diagnostic, bool) { return Diagnostic{}, false }
func (notFatal) Unwrap() error                  { return nil }

func TestFatalError_isSealed(t *testing.T) {
	// The parser's own concrete type satisfies the interface.
	if _, ok := any(&fatalParseError{}).(FatalError); !ok {
		t.Error("the parser's fatal error does not satisfy FatalError")
	}

	// A type declared elsewhere cannot, however closely it mimics the shape.
	// This is a runtime check of what the compiler enforces for real callers.
	var impostor error = notFatal{}
	if _, ok := impostor.(FatalError); ok {
		t.Error("a type without the marker method satisfies FatalError")
	}
	if IsFatal(impostor) {
		t.Error("IsFatal accepted a fabricated fatal error")
	}
	if IsFatal(fmt.Errorf("wrapped: %w", impostor)) {
		t.Error("IsFatal accepted a wrapped fabricated fatal error")
	}
}

func TestFatalError_Error(t *testing.T) {
	cause := errors.New("boom")

	tests := []struct {
		name string
		err  FatalError
		want string
	}{
		{
			name: "cause only",
			err:  newFatalError("", cause),
			want: "message: fatal parse error: boom",
		},
		{
			name: "no cause",
			err:  newFatalError("", nil),
			want: "message: fatal parse error",
		},
		{
			name: "situation and cause",
			err:  newFatalError("diagnostic sink stopped the parse", cause),
			want: "message: fatal parse error: diagnostic sink stopped the parse: boom",
		},
		{
			name: "diagnostic",
			err: newFatalDiagnostic(Diagnostic{
				Code:    CodeMultipartCloseNotFound,
				Message: "closing delimiter not found",
				Path:    []int{1},
			}),
			want: "message: fatal parse error: fatal multipart.close_not_found path=1: " +
				"closing delimiter not found",
		},
		{
			// The diagnostic renders its own cause, so it appears once.
			name: "diagnostic with a cause",
			err: newFatalDiagnostic(Diagnostic{
				Code:  CodeInputReadError,
				Cause: cause,
			}),
			want: "message: fatal parse error: fatal input.read_error: boom",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); got != test.want {
				t.Errorf("Error() =\n%q\nwant\n%q", got, test.want)
			}
		})
	}
}

// TestFatalError_Unwrap uses errors.Unwrap rather than a method call: Unwrap is
// on the concrete type and deliberately not part of the sealed interface, which
// is enough for the errors package.
func TestFatalError_Unwrap(t *testing.T) {
	cause := errors.New("boom")

	if got := errors.Unwrap(newFatalError("", cause)); got != cause {
		t.Errorf("errors.Unwrap() = %v, want the original cause %v", got, cause)
	}
	// A diagnostic-backed fatal error unwraps to the diagnostic's own Cause, so
	// the two views cannot disagree.
	fromDiag := newFatalDiagnostic(Diagnostic{Code: CodeInputReadError, Cause: cause})
	if got := errors.Unwrap(fromDiag); got != cause {
		t.Errorf("errors.Unwrap() = %v, want the diagnostic's cause %v", got, cause)
	}
	if got := errors.Unwrap(newFatalError("", nil)); got != nil {
		t.Errorf("errors.Unwrap() = %v, want nil when there is no cause", got)
	}
	if got := errors.Unwrap(newFatalDiagnostic(Diagnostic{Code: CodeMultipartCloseNotFound})); got != nil {
		t.Errorf("errors.Unwrap() = %v, want nil for a diagnostic without a cause", got)
	}
}

func TestFatalError_errorsIs(t *testing.T) {
	cause := errors.New("boom")
	fatal := newFatalError("scope stopped", cause)

	if !errors.Is(fatal, cause) {
		t.Error("errors.Is(fatal, cause) = false, want true")
	}
	if errors.Is(fatal, errors.New("boom")) {
		t.Error("errors.Is matched an unrelated error with the same text")
	}
	// Wrapping a fatal error must not hide either the fatal error or its cause.
	wrapped := fmt.Errorf("reading part 1: %w", fatal)
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is(wrapped, cause) = false, want true")
	}
	if !IsFatal(wrapped) {
		t.Error("IsFatal(wrapped) = false, want true")
	}
}

// TestFatalError_errorsAs pins the documented idiom: a caller declares the
// sealed interface and lets errors.As fill it in. A typed cause stays
// recoverable through the same chain.
func TestFatalError_errorsAs(t *testing.T) {
	inner := UnknownEncodingError{errors.New("nope")}
	want := newFatalError("scope stopped", inner)

	var fatal FatalError
	if !errors.As(error(want), &fatal) {
		t.Fatal("errors.As(err, &FatalError) = false, want true")
	}
	if fatal != want {
		t.Errorf("errors.As recovered %v, want the error that was returned", fatal)
	}
	if _, ok := fatal.Diagnostic(); ok {
		t.Error("Diagnostic() reported a diagnostic, want none")
	}

	// The same works several links down a chain.
	var throughWrapping FatalError
	if !errors.As(fmt.Errorf("part 0: %w", want), &throughWrapping) || throughWrapping != want {
		t.Error("errors.As did not find the fatal error through a wrapping error")
	}

	var unknown UnknownEncodingError
	if !errors.As(error(want), &unknown) {
		t.Fatal("errors.As(err, &UnknownEncodingError{}) = false, want true")
	}
	if unknown != inner {
		t.Errorf("errors.As recovered %v, want %v", unknown, inner)
	}
}

// TestFatalError_errorsJoin pins that combining a fatal error with others keeps
// both the fatal classification and the original cause reachable, in either
// join position.
func TestFatalError_errorsJoin(t *testing.T) {
	cause := errors.New("sink refused")
	other := errors.New("also happened")
	want := newFatalError("diagnostic sink stopped the parse", cause)

	for _, joined := range []error{
		errors.Join(other, want),
		errors.Join(want, other),
		errors.Join(other, fmt.Errorf("part 0: %w", want)),
	} {
		if !IsFatal(joined) {
			t.Errorf("IsFatal(%v) = false, want true", joined)
		}
		if !errors.Is(joined, cause) {
			t.Errorf("errors.Is(%v, cause) = false, want true", joined)
		}
		if !errors.Is(joined, other) {
			t.Errorf("errors.Is(%v, other) = false, want true", joined)
		}

		var fatal FatalError
		if !errors.As(joined, &fatal) || fatal != want {
			t.Errorf("errors.As(%v) recovered %v, want the joined fatal error", joined, fatal)
		}
	}

	// A joined error as the cause is reachable through the fatal error too.
	overJoin := newFatalError("", errors.Join(cause, other))
	if !errors.Is(overJoin, cause) || !errors.Is(overJoin, other) {
		t.Error("a fatal error hides part of a joined cause")
	}
}

// TestIsFatal_nonFatalErrors pins the boundary of the abstraction: the existing
// warnings accompany a usable result and the iterator's io.EOF is control flow,
// so none of them is a fatal error.
func TestIsFatal_nonFatalErrors(t *testing.T) {
	inner := errors.New("inner")
	tests := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain", errors.New("boom")},
		{"unknown charset", UnknownCharsetError{inner}},
		{"unknown encoding", UnknownEncodingError{inner}},
		{"wrapped unknown charset", fmt.Errorf("part 0: %w", UnknownCharsetError{inner})},
		{"EOF", io.EOF},
		{"unexpected EOF", io.ErrUnexpectedEOF},
		{"joined warnings", errors.Join(UnknownCharsetError{inner}, io.EOF)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if IsFatal(test.err) {
				t.Errorf("IsFatal(%v) = true, want false", test.err)
			}
			var fatal FatalError
			if errors.As(test.err, &fatal) {
				t.Errorf("errors.As(%v, &FatalError) = true, want false", test.err)
			}
		})
	}

	// The compatibility predicates keep working, and do not claim a fatal
	// error is one of their warnings.
	charset := UnknownCharsetError{inner}
	if !IsUnknownCharset(charset) || !IsUnknownEncoding(UnknownEncodingError{inner}) {
		t.Error("existing warning predicates changed behavior")
	}
	if IsUnknownCharset(newFatalError("", inner)) {
		t.Error("IsUnknownCharset matched a fatal error that wraps a plain cause")
	}
	// A fatal error that happens to wrap a warning still reports both, which is
	// the point of unwrapping: the classification comes from the chain.
	if fatal := newFatalError("", charset); !IsFatal(fatal) || !IsUnknownCharset(fatal) {
		t.Error("a fatal error wrapping a warning must report both")
	}
}

func TestFatalError_Diagnostic(t *testing.T) {
	cause := errors.New("boom")
	d := Diagnostic{
		Code:     CodeMultipartCloseNotFound,
		Severity: SeverityIncomplete, // overridden by the constructor
		Message:  "closing delimiter not found",
		Path:     []int{1, 0},
		Cause:    cause,
	}
	fatal := newFatalDiagnostic(d)

	got, ok := fatal.Diagnostic()
	if !ok {
		t.Fatal("Diagnostic() reported no diagnostic, want one")
	}
	if got.Code != CodeMultipartCloseNotFound {
		t.Errorf("Diagnostic().Code = %q, want %q", got.Code, CodeMultipartCloseNotFound)
	}
	// The severity invariant is established inside this package, which is what
	// sealing the interface protects: a fatal error and its diagnostic cannot
	// disagree.
	if got.Severity != SeverityFatal {
		t.Errorf("Diagnostic().Severity = %v, want %v", got.Severity, SeverityFatal)
	}
	// Diagnostic.Cause semantics are preserved: the same error value, which is
	// also what the fatal error unwraps to.
	if got.Cause != cause {
		t.Errorf("Diagnostic().Cause = %v, want the original error value", got.Cause)
	}
	if !errors.Is(fatal, got.Cause) {
		t.Error("the diagnostic's cause is not reachable through the fatal error")
	}
}

// TestFatalError_Diagnostic_defensiveCopy pins that neither the caller who
// recorded the diagnostic nor a caller reading it can reach the error's copy.
func TestFatalError_Diagnostic_defensiveCopy(t *testing.T) {
	path := []int{1, 0}
	fatal := newFatalDiagnostic(Diagnostic{Code: CodeMultipartCloseNotFound, Path: path})

	// The parser may reuse its path buffer after handing the diagnostic over.
	path[0], path[1] = 9, 9

	first, _ := fatal.Diagnostic()
	if first.Path[0] != 1 || first.Path[1] != 0 {
		t.Errorf("Diagnostic().Path = %v, want [1 0]: the error aliases the parser's buffer", first.Path)
	}

	// A caller may keep and rewrite what it was given.
	first.Code = CodeInputEmpty
	first.Severity = SeverityInfo
	first.Path[0] = 9
	if first.Code != CodeInputEmpty || first.Path[0] != 9 {
		t.Fatalf("the returned copy is not the caller's to modify: %v", first)
	}

	second, _ := fatal.Diagnostic()
	if second.Code != CodeMultipartCloseNotFound || second.Severity != SeverityFatal {
		t.Errorf("Diagnostic() = %v after a caller modified an earlier copy", second)
	}
	if second.Path[0] != 1 || second.Path[1] != 0 {
		t.Errorf("Diagnostic().Path = %v, want [1 0]", second.Path)
	}
}

// TestFatalError_Diagnostic_absent pins that "no diagnostic" is represented
// explicitly rather than as a zero Diagnostic a caller could mistake for one.
func TestFatalError_Diagnostic_absent(t *testing.T) {
	for _, test := range []struct {
		name string
		err  FatalError
	}{
		{"no cause", newFatalError("", nil)},
		{"cause only", newFatalError("diagnostic sink stopped the parse", errors.New("boom"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := test.err.Diagnostic()
			if ok {
				t.Errorf("Diagnostic() reported a diagnostic %v, want none", got)
			}
			if got.Code != "" || got.Severity != SeverityInfo || got.Path != nil || got.Cause != nil {
				t.Errorf("Diagnostic() = %v, want the zero Diagnostic", got)
			}
		})
	}
}

// TestParseSession_sinkErrorIsFatalError pins the one concrete fatal producer
// that exists today. A sink error is the caller asking to stop, so it becomes a
// fatal error that wraps what the sink returned and carries no diagnostic: a
// callback failure is not a defect of the message and must not be dressed up as
// one.
func TestParseSession_sinkErrorIsFatalError(t *testing.T) {
	sinkErr := errors.New("sink failed")
	sink := &collectingSink{err: sinkErr, errAt: 0}
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	err := s.record(Diagnostic{Code: CodeHeaderBareLF})
	if err == nil {
		t.Fatal("record() = nil, want a fatal error")
	}
	if !IsFatal(err) {
		t.Errorf("IsFatal(%v) = false, want true", err)
	}
	// The sink's own error stays matchable, so a caller that owns the sink can
	// recognize its own failure.
	if !errors.Is(err, sinkErr) {
		t.Errorf("errors.Is(%v, sinkErr) = false, want true", err)
	}

	var fatal FatalError
	if !errors.As(err, &fatal) {
		t.Fatal("errors.As(err, &FatalError) = false, want true")
	}
	if got := errors.Unwrap(fatal); got != sinkErr {
		t.Errorf("errors.Unwrap() = %v, want the sink's own error %v", got, sinkErr)
	}
	if d, ok := fatal.Diagnostic(); ok {
		t.Errorf("Diagnostic() = %v, want none for a callback failure", d)
	}
}

// TestParseSession_sinkErrorFatalIsSticky pins that the first sink error is
// wrapped exactly once: every later record reports the identical fatal error
// value, and the sink is not called again.
func TestParseSession_sinkErrorFatalIsSticky(t *testing.T) {
	sinkErr := errors.New("sink failed")
	sink := &collectingSink{err: sinkErr, errAt: 0}
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	first := s.record(Diagnostic{Code: CodeHeaderBareLF})
	second := s.record(Diagnostic{Code: CodeHeaderBareCR})
	third := s.record(Diagnostic{Code: CodeCTEBase64Truncated})

	if first != second || second != third {
		t.Errorf("record() returned %v, %v and %v, want one identical sticky error", first, second, third)
	}
	if !IsFatal(third) || !errors.Is(third, sinkErr) {
		t.Errorf("the sticky error %v lost its classification or its cause", third)
	}
	if sink.calls != 1 {
		t.Errorf("sink was called %d times, want 1", sink.calls)
	}

	// errors.As finds the same value every time, so a caller may compare what
	// it extracted across calls.
	var a, b FatalError
	if !errors.As(first, &a) || !errors.As(third, &b) || a != b {
		t.Errorf("errors.As recovered %v and %v, want one identical fatal error", a, b)
	}
}

// TestParseSession_recordReturnsUntypedNil pins that a session with no failure
// returns a genuinely nil error, whatever the type of the field holding the
// sticky one.
func TestParseSession_recordReturnsUntypedNil(t *testing.T) {
	for _, test := range []struct {
		name string
		s    *ParseSession
	}{
		{"no sink", newParseSession(ProfileTolerant, CompatibilityConservative)},
		{"healthy sink", newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
			diagnosticOptions{sink: &collectingSink{}})},
		{"nil session", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.s.record(Diagnostic{Code: CodeHeaderBareLF}); err != nil {
				t.Errorf("record() = %v (%T), want a nil error", err, err)
			}
		})
	}

	var zero ParseSession
	if err := zero.record(Diagnostic{Code: CodeHeaderBareLF}); err != nil {
		t.Errorf("zero ParseSession.record() = %v (%T), want a nil error", err, err)
	}
}

// TestParseSession_fatalErr pins the state a parse scope checks: nil while the
// parse may continue, and then the identical sticky error, on a nil session
// too.
func TestParseSession_fatalErr(t *testing.T) {
	var nilSession *ParseSession
	if got := nilSession.fatalErr(); got != nil {
		t.Errorf("nil ParseSession.fatalErr() = %v, want nil", got)
	}
	var zero ParseSession
	if got := zero.fatalErr(); got != nil {
		t.Errorf("zero ParseSession.fatalErr() = %v, want nil", got)
	}

	// A session without a sink can never stop, however much it records.
	quiet := newParseSession(ProfileTolerant, CompatibilityConservative)
	quiet.record(Diagnostic{Code: CodeHeaderBareLF})
	if got := quiet.fatalErr(); got != nil {
		t.Errorf("fatalErr() = %v, want nil for a session that cannot be stopped", got)
	}

	sinkErr := errors.New("sink failed")
	sink := &collectingSink{err: sinkErr, errAt: 1}
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	s.record(Diagnostic{Code: CodeHeaderBareLF})
	if got := s.fatalErr(); got != nil {
		t.Errorf("fatalErr() = %v, want nil while the sink is healthy", got)
	}

	err := s.record(Diagnostic{Code: CodeHeaderBareCR})
	if got := s.fatalErr(); got != err {
		t.Errorf("fatalErr() = %v, want the identical error record returned %v", got, err)
	}
	if got := s.fatalErr(); got != err {
		t.Errorf("fatalErr() = %v on a second call, want the identical error %v", got, err)
	}
	if !errors.Is(s.fatalErr(), sinkErr) {
		t.Error("the state a scope reads lost the sink's own error")
	}
}

// TestParseSession_fatalErrConcurrentWithSinkFailure exercises the state under
// "go test -race": scopes read fatalErr while another goroutine's delivery
// fails. Every non-nil value a reader observes must be the one sticky error.
func TestParseSession_fatalErrConcurrentWithSinkFailure(t *testing.T) {
	const (
		readers   = 4
		perReader = 64
	)

	sinkErr := errors.New("sink failed")
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
			return sinkErr
		})})

	// Each reader keeps its own observations, so the test itself adds no race.
	observed := make([][]FatalError, readers)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < perReader; i++ {
			s.record(Diagnostic{Code: CodeHeaderBareLF, Path: []int{i}})
		}
	}()
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < perReader; i++ {
				if got := s.fatalErr(); got != nil {
					observed[r] = append(observed[r], got)
				}
			}
		}(r)
	}
	wg.Wait()

	want := s.fatalErr()
	if want == nil {
		t.Fatal("fatalErr() = nil after a failing sink, want the sticky error")
	}
	if !errors.Is(want, sinkErr) {
		t.Errorf("fatalErr() = %v, want it to wrap %v", want, sinkErr)
	}
	for r, got := range observed {
		for i, e := range got {
			if e != want {
				t.Fatalf("reader %d observation %d = %v, want the identical sticky error %v", r, i, e, want)
			}
		}
	}
}

// recoverLines is a stand-in for a parse scope: it recovers what it safely can,
// and stops at the first fatal error, returning the prefix it had already built
// together with that error. Every scope in the parser will have this shape, so
// the tests below can pin the result contract before the Parse API exists.
//
// It shows the two checks a real scope owes the session, because a session
// cannot interrupt a scan that never looks at it: fatalErr before the scope
// begins, so a scope on a stopped session recovers nothing at all, and fatalErr
// again at each safe progress boundary, here before every line it would accept.
//
// An empty line stands for a defect worth diagnosing; the sink configured on
// the session decides whether the parse may continue.
func recoverLines(s *ParseSession, lines []string) ([]string, error) {
	// Nothing may be started on a session that has already stopped.
	if err := s.fatalErr(); err != nil {
		return nil, err
	}

	var out []string
	for _, line := range lines {
		// A safe progress boundary: out is a complete result as it stands.
		if err := s.fatalErr(); err != nil {
			return out, err
		}
		if line == "" {
			if err := s.record(Diagnostic{Code: CodeHeaderBareLF}); err != nil {
				return out, err
			}
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// TestFatalError_resultRemainsUsable pins the central promise of the
// abstraction: a non-nil result returned with a fatal error is the recovered
// prefix, it belongs to the caller, and the error does not invalidate it.
func TestFatalError_resultRemainsUsable(t *testing.T) {
	sinkErr := errors.New("caller stopped the parse")
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
			return sinkErr
		})})

	got, err := recoverLines(s, []string{"first", "second", "", "third"})

	if !IsFatal(err) {
		t.Fatalf("recoverLines() error = %v, want a fatal error", err)
	}
	if !errors.Is(err, sinkErr) {
		t.Errorf("errors.Is(%v, sinkErr) = false, want true", err)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("recovered prefix = %v, want [first second]", got)
	}

	// The result is the caller's: nothing in the session or the error reaches
	// it, and using it after a fatal error is exactly what it is for.
	got[0] = "rewritten"
	got = append(got, "kept")
	if len(got) != 3 || got[2] != "kept" {
		t.Errorf("result = %v, want a caller-owned slice", got)
	}

	// Already recorded diagnostics survive the failure, so the scope can still
	// explain what it saw.
	if n := s.Len(); n != 1 {
		t.Errorf("Len() = %d, want 1: the diagnostic that triggered the stop is retained", n)
	}
}

// TestFatalError_nilResultMeansNothingRecovered pins the other half: when a
// scope could not construct anything safe, it returns nil, and the caller can
// tell the two cases apart from the returned value alone — there is no flag on
// the error claiming otherwise.
func TestFatalError_nilResultMeansNothingRecovered(t *testing.T) {
	sinkErr := errors.New("caller stopped the parse")
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
			return sinkErr
		})})

	got, err := recoverLines(s, []string{"", "unreached"})

	if !IsFatal(err) {
		t.Fatalf("recoverLines() error = %v, want a fatal error", err)
	}
	if got != nil {
		t.Errorf("recovered prefix = %v, want nil when nothing could be recovered", got)
	}
}

// TestParseSession_sinkStopEndsEveryScopeOfThatSession pins the scope of a sink
// stop. It is the whole session, not the one scope that happened to be
// recording: a scope started afterwards recovers nothing at all, an active
// scope keeps only what it had before it observed the stop, and what earlier
// scopes returned stays usable.
func TestParseSession_sinkStopEndsEveryScopeOfThatSession(t *testing.T) {
	sinkErr := errors.New("caller stopped the parse")
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
			return sinkErr
		})})

	// An active scope: it recovered "a" before the stop, and stops at the
	// defect that triggered it. "b" is past the stop and is not recovered.
	stopped, err := recoverLines(s, []string{"a", "", "b"})
	if !IsFatal(err) {
		t.Fatalf("active scope error = %v, want a fatal error", err)
	}
	if len(stopped) != 1 || stopped[0] != "a" {
		t.Errorf("active scope result = %v, want only [a], recovered before the stop", stopped)
	}
	// The session itself reports the stop, with the identical error.
	if got := s.fatalErr(); got != err {
		t.Errorf("fatalErr() = %v, want the identical error the scope returned %v", got, err)
	}

	// A scope started after the stop must not recover anything: it checks
	// before it begins, so no "c" is accepted, and the sink is not consulted
	// again.
	next, nextErr := recoverLines(s, []string{"c", "", "d"})
	if next != nil {
		t.Errorf("new scope result = %v, want nil on an already stopped session", next)
	}
	if nextErr != err {
		t.Errorf("new scope error = %v, want the identical sticky error %v", nextErr, err)
	}

	// What the active scope returned before the stop is untouched by all this.
	if len(stopped) != 1 || stopped[0] != "a" {
		t.Errorf("earlier result = %v, want [a] to stay usable", stopped)
	}
}

// TestParseSession_sinkStopIsPerSession pins the other edge of that scope: it
// stops one session and not a sink. Two sessions are two parses, not two scopes
// of one message, so a shared sink keeps serving the other one.
func TestParseSession_sinkStopIsPerSession(t *testing.T) {
	sinkErr := errors.New("caller stopped the parse")
	sink := DiagnosticSinkFunc(func(s *ParseSession, _ Diagnostic) error {
		if s.Profile() == ProfileStrict {
			return sinkErr
		}
		return nil
	})

	stopped := newParseSessionWithOptions(ProfileStrict, CompatibilityConservative,
		diagnosticOptions{sink: sink})
	other := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	if _, err := recoverLines(stopped, []string{"a", "", "b"}); !IsFatal(err) {
		t.Fatalf("stopped session error = %v, want a fatal error", err)
	}

	got, err := recoverLines(other, []string{"c", "", "d"})
	if err != nil {
		t.Fatalf("other session error = %v, want nil", err)
	}
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Errorf("other session result = %v, want [c d]", got)
	}
}
