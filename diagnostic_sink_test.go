package message

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The sink abstraction is an interface so that a stateful handler can
// implement it directly, and DiagnosticSinkFunc adapts a plain function.
var (
	_ DiagnosticSink = DiagnosticSinkFunc(nil)
	_ DiagnosticSink = (*collectingSink)(nil)
)

// collectingSink records every delivered diagnostic and the session that
// delivered it. It deliberately does not lock: each test below configures it
// on a single session, delivery is serialized per session, and "go test -race"
// fails if that ever stops being true. A sink shared by several sessions would
// have to guard this state itself.
type collectingSink struct {
	got      []Diagnostic
	sessions []*ParseSession
	err      error // returned once at index errAt
	errAt    int
	calls    int
}

func (s *collectingSink) Diagnose(session *ParseSession, d Diagnostic) error {
	s.calls++
	s.sessions = append(s.sessions, session)
	s.got = append(s.got, d)
	if s.err != nil && len(s.got) == s.errAt+1 {
		return s.err
	}
	return nil
}

func (s *collectingSink) codes() []Code {
	codes := make([]Code, len(s.got))
	for i, d := range s.got {
		codes[i] = d.Code
	}
	return codes
}

// deliveredBy reports whether every delivery named want as the delivering
// session.
func (s *collectingSink) deliveredBy(want *ParseSession) bool {
	for _, got := range s.sessions {
		if got != want {
			return false
		}
	}
	return true
}

func equalCodes(a, b []Code) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sessionCodes(s *ParseSession) []Code {
	diags := s.Diagnostics()
	codes := make([]Code, len(diags))
	for i, d := range diags {
		codes[i] = d.Code
	}
	return codes
}

// runWithinTimeout fails the test instead of hanging the whole package when a
// reentrant call deadlocks.
func runWithinTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out: a session call appears to deadlock")
	}
}

func TestDiagnosticSinkFunc_callsFunction(t *testing.T) {
	var (
		gotSession *ParseSession
		gotDiag    Diagnostic
	)
	want := errors.New("stop")
	wantSession := newParseSession(ProfileTolerant, CompatibilityConservative)

	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		gotSession, gotDiag = s, d
		return want
	})

	if err := sink.Diagnose(wantSession, Diagnostic{Code: CodeInputEmpty}); err != want {
		t.Errorf("Diagnose() error = %v, want %v", err, want)
	}
	if gotSession != wantSession {
		t.Errorf("sink saw session %p, want %p", gotSession, wantSession)
	}
	if gotDiag.Code != CodeInputEmpty {
		t.Errorf("sink saw Code %q, want %q", gotDiag.Code, CodeInputEmpty)
	}
}

func TestParseSession_sinkReceivesDiscoveryOrder(t *testing.T) {
	sink := &collectingSink{}
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	want := []Code{
		CodeHeaderBareLF,
		CodeMIMEParameterUnclosedQuote,
		CodeMultipartCloseNotFound,
		CodeHeaderBareLF, // a repeated code keeps its own position
	}
	for _, code := range want {
		if err := s.record(Diagnostic{Code: code}); err != nil {
			t.Fatalf("record(%q) = %v, want nil", code, err)
		}
	}

	if got := sink.codes(); !equalCodes(got, want) {
		t.Errorf("sink received %v, want %v", got, want)
	}
	if !sink.deliveredBy(s) {
		t.Errorf("sink was handed %v, want every delivery to name %p", sink.sessions, s)
	}
	// A sink does not replace retention by default.
	if got := sessionCodes(s); !equalCodes(got, want) {
		t.Errorf("Diagnostics() = %v, want %v", got, want)
	}
}

// TestParseSession_sinkReceivesDeliveringSession pins the parameter that makes
// eager delivery usable: a sink that has no way to reach the session — as is
// the case while the parse call that would return it is still running — is
// still handed the exact session, and can query it reentrantly.
func TestParseSession_sinkReceivesDeliveringSession(t *testing.T) {
	var (
		gotSession *ParseSession
		seenLen    int
		seenCode   Code
		profile    Profile
		compat     Compatibility
	)

	// Nothing in this sink is captured from the session: it uses only what it
	// is given.
	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		gotSession = s
		seenLen = s.Len()
		if diags := s.Diagnostics(); len(diags) > 0 {
			seenCode = diags[len(diags)-1].Code
		}
		profile, compat = s.Profile(), s.Compatibility()
		return nil
	})

	s := newParseSessionWithOptions(ProfileForensic, CompatibilityAngus,
		diagnosticOptions{sink: sink})

	runWithinTimeout(t, 10*time.Second, func() {
		s.record(Diagnostic{Code: CodeHeaderBareLF})
	})

	if gotSession != s {
		t.Fatalf("sink was handed session %p, want %p", gotSession, s)
	}
	if seenLen != 1 {
		t.Errorf("Len() inside the sink = %d, want 1", seenLen)
	}
	if seenCode != CodeHeaderBareLF {
		t.Errorf("last diagnostic inside the sink = %q, want %q", seenCode, CodeHeaderBareLF)
	}
	if profile != ProfileForensic || compat != CompatibilityAngus {
		t.Errorf("inside the sink Profile/Compatibility = %v/%v, want %v/%v",
			profile, compat, ProfileForensic, CompatibilityAngus)
	}
}

// TestParseSession_sinkDeliveryIsSynchronous pins that record does not defer or
// batch delivery: the sink has seen the diagnostic by the time record returns.
func TestParseSession_sinkDeliveryIsSynchronous(t *testing.T) {
	sink := &collectingSink{}
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	for i := 1; i <= 3; i++ {
		s.record(Diagnostic{Code: CodeInputEmpty})
		if sink.calls != i {
			t.Fatalf("after %d records the sink was called %d times, want %d", i, sink.calls, i)
		}
	}
}

func TestParseSession_noRetention(t *testing.T) {
	sink := &collectingSink{}
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink, noRetention: true})

	want := []Code{CodeHeaderBareLF, CodeMultipartCloseNotFound}
	for _, code := range want {
		s.record(Diagnostic{Code: code})
	}

	if got := sink.codes(); !equalCodes(got, want) {
		t.Errorf("sink received %v, want %v", got, want)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0 without retention", got)
	}
	if got := s.Diagnostics(); got != nil {
		t.Errorf("Diagnostics() = %v, want nil without retention", got)
	}
}

// TestParseSession_noRetentionWithoutSinkStillRetains pins that diagnostics are
// never dropped without being delivered somewhere: no-retention only takes
// effect when a sink is configured to take delivery.
func TestParseSession_noRetentionWithoutSinkStillRetains(t *testing.T) {
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{noRetention: true})

	s.record(Diagnostic{Code: CodeHeaderBareLF})

	if got := s.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 when no sink takes delivery", got)
	}
}

func TestParseSession_sinkReceivesDefensiveCopy(t *testing.T) {
	var delivered []Diagnostic
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			// A sink may keep and rewrite what it was given.
			d.Code = CodeInputEmpty
			d.Path[0] = 9
			delivered = append(delivered, d)
			return nil
		})})

	path := []int{1, 2}
	s.record(Diagnostic{Code: CodeMultipartEmptyPart, Path: path})

	// Neither the parser's own buffer nor the retained diagnostic may be
	// disturbed by the sink.
	if path[0] != 1 || path[1] != 2 {
		t.Errorf("caller Path = %v, want [1 2]", path)
	}
	got := s.Diagnostics()
	if len(got) != 1 {
		t.Fatalf("Diagnostics() returned %d diagnostics, want 1", len(got))
	}
	if got[0].Code != CodeMultipartEmptyPart {
		t.Errorf("retained Code = %q, want %q", got[0].Code, CodeMultipartEmptyPart)
	}
	if got[0].Path[0] != 1 || got[0].Path[1] != 2 {
		t.Errorf("retained Path = %v, want [1 2]", got[0].Path)
	}
	if len(delivered) != 1 || delivered[0].Path[0] != 9 {
		t.Errorf("sink kept %v, want its own modified copy", delivered)
	}
}

// TestParseSession_sinkErrorIsSticky pins the sink failure policy: the first
// error is kept, the failed sink is never called again, later diagnostics are
// still recorded and retained, and record keeps reporting the first error so
// that a parser can propagate the request to stop.
func TestParseSession_sinkErrorIsSticky(t *testing.T) {
	first := errors.New("sink failed")
	sink := &collectingSink{err: first, errAt: 1}
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	if err := s.record(Diagnostic{Code: CodeHeaderBareLF}); err != nil {
		t.Fatalf("record() = %v, want nil before the sink fails", err)
	}
	if err := s.record(Diagnostic{Code: CodeMultipartCloseNotFound}); !errors.Is(err, first) {
		t.Fatalf("record() = %v, want %v", err, first)
	}
	if err := s.record(Diagnostic{Code: CodeCTEBase64Truncated}); !errors.Is(err, first) {
		t.Fatalf("record() after failure = %v, want the sticky %v", err, first)
	}

	if sink.calls != 2 {
		t.Errorf("sink was called %d times, want 2: a failed sink is not called again", sink.calls)
	}
	want := []Code{CodeHeaderBareLF, CodeMultipartCloseNotFound, CodeCTEBase64Truncated}
	if got := sessionCodes(s); !equalCodes(got, want) {
		t.Errorf("Diagnostics() = %v, want %v: retention outlives a failed sink", got, want)
	}
}

// TestParseSession_sinkErrorKeepsFirstError pins that a later, different error
// cannot replace the sticky one. The second sink error is only reachable if the
// failed sink were called again, so this also guards the no-recall rule.
func TestParseSession_sinkErrorKeepsFirstError(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")

	calls := 0
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			calls++
			if calls == 1 {
				return first
			}
			return second
		})})

	s.record(Diagnostic{Code: CodeHeaderBareLF})
	if err := s.record(Diagnostic{Code: CodeHeaderBareCR}); !errors.Is(err, first) {
		t.Errorf("record() = %v, want the first error %v", err, first)
	}
	if calls != 1 {
		t.Errorf("sink was called %d times, want 1", calls)
	}
}

// TestParseSession_sinkErrorWithoutRetention pins that a failed sink does not
// turn a no-retention session into a retaining one.
func TestParseSession_sinkErrorWithoutRetention(t *testing.T) {
	first := errors.New("sink failed")
	sink := &collectingSink{err: first, errAt: 0}
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink, noRetention: true})

	if err := s.record(Diagnostic{Code: CodeHeaderBareLF}); !errors.Is(err, first) {
		t.Fatalf("record() = %v, want %v", err, first)
	}
	if err := s.record(Diagnostic{Code: CodeHeaderBareCR}); !errors.Is(err, first) {
		t.Fatalf("record() after failure = %v, want the sticky %v", err, first)
	}

	if sink.calls != 1 {
		t.Errorf("sink was called %d times, want 1", sink.calls)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0 without retention", got)
	}
}

// TestParseSession_sinkErrorIsPerSession pins that the sticky error belongs to
// the session, not to the sink: another session sharing the same sink keeps
// delivering.
func TestParseSession_sinkErrorIsPerSession(t *testing.T) {
	failFirstOf := errors.New("sink failed")

	var (
		mu    sync.Mutex
		calls = map[*ParseSession]int{}
	)
	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		// A shared sink guards its own state; the session tells it which
		// stream it is looking at.
		mu.Lock()
		defer mu.Unlock()
		calls[s]++
		if s.Profile() == ProfileStrict {
			return failFirstOf
		}
		return nil
	})

	failing := newParseSessionWithOptions(ProfileStrict, CompatibilityConservative,
		diagnosticOptions{sink: sink})
	healthy := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	for i := 0; i < 3; i++ {
		if err := failing.record(Diagnostic{Code: CodeHeaderBareLF}); !errors.Is(err, failFirstOf) {
			t.Fatalf("failing.record() = %v, want %v", err, failFirstOf)
		}
		if err := healthy.record(Diagnostic{Code: CodeHeaderBareLF}); err != nil {
			t.Fatalf("healthy.record() = %v, want nil", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if calls[failing] != 1 {
		t.Errorf("failed session delivered %d times, want 1", calls[failing])
	}
	if calls[healthy] != 3 {
		t.Errorf("healthy session delivered %d times, want 3", calls[healthy])
	}
}

func TestParseSession_sinkOnZeroAndNilSession(t *testing.T) {
	// A zero session has no sink: record retains and reports no error.
	var zero ParseSession
	if err := zero.record(Diagnostic{Code: CodeInputEmpty}); err != nil {
		t.Errorf("zero ParseSession.record() = %v, want nil", err)
	}
	if got := zero.Len(); got != 1 {
		t.Errorf("zero ParseSession.Len() = %d, want 1", got)
	}

	// A nil session has nowhere to deliver: record is a no-op, not a panic.
	var nilSession *ParseSession
	if err := nilSession.record(Diagnostic{Code: CodeInputEmpty}); err != nil {
		t.Errorf("nil ParseSession.record() = %v, want nil", err)
	}
	if got := nilSession.Len(); got != 0 {
		t.Errorf("nil ParseSession.Len() = %d, want 0", got)
	}
}

// TestParseSession_concurrentRecordWithSink exercises delivery under
// "go test -race". The sink keeps unsynchronized state and reads the session
// reentrantly, so both a delivery race and a reentrant deadlock would fail.
func TestParseSession_concurrentRecordWithSink(t *testing.T) {
	const (
		writers   = 4
		perWriter = 32
	)

	// One session, so per-session serialization makes this sink safe without a
	// lock of its own.
	calls := 0
	s := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
			calls++
			if got := s.Len(); got != calls {
				t.Errorf("during delivery %d the session held %d diagnostics, want %d", calls, got, calls)
			}
			return nil
		})})

	runWithinTimeout(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					s.record(Diagnostic{Code: CodeMultipartEmptyPart, Path: []int{w, i}})
				}
			}(w)
		}
		wg.Wait()
	})

	if calls != writers*perWriter {
		t.Errorf("sink was called %d times, want %d", calls, writers*perWriter)
	}
	if got := s.Len(); got != writers*perWriter {
		t.Errorf("Len() = %d, want %d", got, writers*perWriter)
	}
}

// TestDiagnosticSink_sharedSessionsDeliverConcurrently pins the documented
// boundary of delivery serialization: it is per session only. Two sessions
// sharing one sink must be able to be inside it at the same time, which is why
// a sink with state shared between sessions has to synchronize it itself. The
// rendezvous below cannot complete if delivery were serialized across
// sessions.
func TestDiagnosticSink_sharedSessionsDeliverConcurrently(t *testing.T) {
	var (
		mu   sync.Mutex
		seen = map[*ParseSession]int{}
		both sync.WaitGroup
	)
	both.Add(2)

	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		mu.Lock()
		seen[s]++
		mu.Unlock()

		both.Done()
		both.Wait() // returns only once the other session is inside the sink
		return nil
	})

	a := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})
	b := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	runWithinTimeout(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		for _, s := range []*ParseSession{a, b} {
			wg.Add(1)
			go func(s *ParseSession) {
				defer wg.Done()
				s.record(Diagnostic{Code: CodeHeaderBareLF})
			}(s)
		}
		wg.Wait()
	})

	mu.Lock()
	defer mu.Unlock()
	if seen[a] != 1 || seen[b] != 1 {
		t.Errorf("deliveries per session = %d and %d, want 1 each", seen[a], seen[b])
	}
}
