package message

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// How a MaxPartCount overflow becomes this parse's one session-wide failure.
//
// Publishing it takes two steps that cannot be one: the limit.part_count
// diagnostic has to be offered to the caller's sink, and only the sink's answer
// says whether the parse ends with the budget's own failure or with the caller's
// decision to stop. Delivering to a sink means calling caller code, which must
// not happen under the session lock, so there is a window — and the whole point
// of the tests here is that nothing observable exists inside it.
//
// Every state transition below is driven by a channel rather than by timing. The
// one place a clock appears is a deadlock net: a lost wake-up must fail this
// test rather than hang the package.

// deadlockTimeout is the safety net for a transition that never finishes. It is
// never the thing a test concludes anything from — every assertion here is made
// after a channel said the state had changed — so it only has to be far above
// any real scheduling delay, and is kept short enough that a defect which does
// hang reports it quickly.
const deadlockTimeout = 10 * time.Second

// A blockingSink holds the parser inside Diagnose for one chosen diagnostic
// until a test releases it, which is what opens the window these tests look
// into. Every other diagnostic passes straight through.
//
// It blocks only once it has been armed, and that is not a detail. The
// diagnostic it waits on has to arrive on a goroutine the test is not itself
// standing on, so a defect that produces it earlier than the test set up for
// must not park the test inside its own call: unarmed, the sink answers
// immediately, and the assertion that expected a part instead of a failure is
// what reports the defect.
type blockingSink struct {
	// code is the diagnostic to block on, and err is what Diagnose returns for
	// it, armed or not.
	code Code
	err  error

	// armed is set by the test once the call that should reach code is about to
	// be made from somewhere the test can release.
	armed atomic.Bool

	// entered is signalled once, when Diagnose has reached the chosen
	// diagnostic while armed, and release lets that call return.
	entered chan struct{}
	release chan struct{}

	mu   sync.Mutex
	seen []Code
}

func newBlockingSink(code Code, err error) *blockingSink {
	return &blockingSink{
		code:    code,
		err:     err,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// arm makes the next matching diagnostic block until release is closed.
func (s *blockingSink) arm() {
	s.armed.Store(true)
}

// Diagnose implements DiagnosticSink.
func (s *blockingSink) Diagnose(_ *ParseSession, d Diagnostic) error {
	s.mu.Lock()
	s.seen = append(s.seen, d.Code)
	s.mu.Unlock()

	if d.Code != s.code {
		return nil
	}
	if !s.armed.Load() {
		return s.err
	}
	close(s.entered)
	<-s.release
	return s.err
}

// delivered returns the codes this sink was handed, in order.
func (s *blockingSink) delivered() []Code {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Code(nil), s.seen...)
}

// await waits for a channel the test expects to be signalled, and fails rather
// than hanging when it is not.
func await(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(deadlockTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// awaitGroup is await for a WaitGroup.
func awaitGroup(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	await(t, done, what)
}

// pendingState reports what the session would tell a scope right now, read
// directly rather than through fatalErr, which would wait.
//
// It is the assertion that makes these tests deterministic instead of timed:
// while the sink is provably blocked, the published stop must be nil and a
// transition must be open, and reading the fields says so without racing
// anything.
func pendingState(s *ParseSession) (published FatalError, transitionOpen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop, s.partPending != nil
}

// An observation is one goroutine's attempt to learn this parse's failure while
// the transition was open: what it got, and whether it got it before the sink
// was released.
type observation struct {
	name  string
	got   error
	early bool
}

// A waiterSet runs the two calls a parse scope makes to learn that its parse has
// stopped — fatalErr, which every scope makes at its progress boundaries, and
// chargePart, which the next part to be discovered would make — from several
// goroutines at once, while a transition is open.
//
// The ordering it establishes is what the assertions rest on, so it is worth
// being exact about which parts of it are facts and which are detection:
//
//   - Every waiter is started only after the sink has been entered, and every
//     waiter announces itself before it calls. start does not return until all
//     of them have announced, so a waiter reaching its call after the release is
//     the only ordering not excluded outright.
//   - That ordering is what settle narrows. It is not where correctness comes
//     from: the invariant "nothing is published while the sink is deciding" is
//     asserted from the session's own state, which needs no timing at all.
//     settle exists so that a waiter which does not wait is caught here, by
//     finished, rather than only when its value turns out wrong.
//   - What every waiter got is checked after the release, and that check is a
//     fact: whatever it received must be the one published failure.
type waiterSet struct {
	session  *ParseSession
	released *atomic.Bool

	out      []observation
	wg       sync.WaitGroup
	finished atomic.Int32
}

// start launches n waiters of each kind and returns once all of them have
// announced that they are about to call.
func (w *waiterSet) start(t *testing.T, n int) {
	t.Helper()

	w.out = make([]observation, 2*n)
	announced := make(chan struct{}, 2*n)

	record := func(i int, o observation) {
		w.out[i] = o
		w.finished.Add(1)
	}

	for i := 0; i < n; i++ {
		w.wg.Add(2)

		go func(i int) {
			defer w.wg.Done()
			announced <- struct{}{}
			got := w.session.fatalErr()
			record(2*i, observation{
				name:  fmt.Sprintf("fatalErr waiter %d", i),
				got:   got,
				early: !w.released.Load(),
			})
		}(i)

		go func(i int) {
			defer w.wg.Done()
			announced <- struct{}{}
			got, claimed := w.session.chargePart()
			if claimed {
				// A second claim would mean two callers recording one defect.
				t.Errorf("chargePart waiter %d claimed the overflow, want the claim already taken", i)
			}
			record(2*i+1, observation{
				name:  fmt.Sprintf("chargePart waiter %d", i),
				got:   got,
				early: !w.released.Load(),
			})
		}(i)
	}

	for i := 0; i < 2*n; i++ {
		select {
		case <-announced:
		case <-time.After(deadlockTimeout):
			t.Fatal("timed out waiting for the waiters to start")
		}
	}
}

// settle gives every announced waiter room to run to completion if it is not
// going to wait, and then reports how many have finished.
//
// It is a detector, not a proof. A waiter that honors the transition cannot
// finish here however long this takes, so a non-zero result is a real defect;
// a zero result is not by itself the reason the test passes.
func (w *waiterSet) settle() int32 {
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	time.Sleep(20 * time.Millisecond)
	return w.finished.Load()
}

// collect waits for every waiter and returns what each of them observed.
func (w *waiterSet) collect(t *testing.T) []observation {
	t.Helper()
	awaitGroup(t, &w.wg, "the waiters to observe the published failure")
	return w.out
}

// TestParse_partCount_publicationIsAtomic is A, B and C together: a sink held
// inside the part-count diagnostic, healthy and failing, with diagnostics
// retained and discarded.
//
// What it pins is one thing said four ways. While the sink is deciding, the
// session has published nothing, so no scope can observe a value that is about
// to change. When it has decided, exactly one failure is published — the
// budget's if the sink took the diagnostic, the caller's if the sink refused to
// continue — and the overflow caller, every waiter, every later fatalErr, the
// iterator that was already open and a traversal begun afterwards all receive
// that identical value.
func TestParse_partCount_publicationIsAtomic(t *testing.T) {
	sentinel := errors.New("stop here")

	for _, tc := range []struct {
		name        string
		sinkErr     error
		noRetention bool
	}{
		{"healthy sink", nil, false},
		{"healthy sink discarding delivered", nil, true},
		{"failing sink", sentinel, false},
		{"failing sink discarding delivered", sentinel, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Two parts under a budget of one: the first is charged, the second
			// overflows.
			f := wideContainer(2, "\r\n")
			offset := f.offset(t, "part1.header")

			sink := newBlockingSink(CodeLimitPartCount, tc.sinkErr)
			session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
				Limits:                      partsOnly(1),
				DiagnosticSink:              sink,
				DiscardDeliveredDiagnostics: tc.noRetention,
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}

			mr := e.MultipartReader()
			if mr == nil {
				t.Fatal("MultipartReader() = nil, want the parts of a multipart message")
			}
			first, err := mr.NextPart()
			if err != nil {
				t.Fatalf("NextPart() = %v, want the part within the budget", err)
			}
			if got := readBodyOf(t, first); got != "p0" {
				t.Errorf("first part = %q, want %q", got, "p0")
			}

			// The overflow runs on its own goroutine so that the test can watch
			// the session while the sink holds it. Arming happens first, and
			// before that goroutine exists, so the call it makes is the one the
			// sink waits on.
			sink.arm()
			overflow := make(chan error, 1)
			go func() {
				p, err := mr.NextPart()
				if p != nil {
					t.Errorf("the over-budget part was returned as %v, want no part", p)
				}
				overflow <- err
			}()

			// Watching the overflow too, so that an implementation which never
			// reaches the budget says so at once rather than through the net.
			select {
			case <-sink.entered:
			case err := <-overflow:
				t.Fatalf("the overflow call finished with %v without the budget being reached", err)
			case <-time.After(deadlockTimeout):
				t.Fatal("timed out waiting for the sink to be handed the part-count diagnostic")
			}

			// Deterministic, because the sink is provably inside Diagnose: the
			// session has decided nothing and says so by having nothing to say.
			published, open := pendingState(session)
			if published != nil {
				t.Errorf("the session published %v while the sink was still deciding, want nothing published", published)
			}
			if !open {
				t.Error("no transition is open while the sink is deciding, want one")
			}

			// Every kind of scope that would ask the session what happened,
			// asking while there is nothing to tell it.
			var released atomic.Bool
			waiters := &waiterSet{session: session, released: &released}
			waiters.start(t, 4)

			if done := waiters.settle(); done != 0 {
				t.Errorf("%d waiters returned while the sink was still deciding, want them all to wait", done)
			}
			// The state has not moved either, which is the timing-free half of
			// the same statement.
			if published, open := pendingState(session); published != nil || !open {
				t.Errorf("state while the sink decides = (published %v, transition open %v), want (nil, true)", published, open)
			}

			released.Store(true)
			close(sink.release)

			final := <-overflow
			if final == nil {
				t.Fatal("the overflow caller got no error, want the published failure")
			}
			if !IsFatal(final) {
				t.Fatalf("the overflow caller got %v, want a FatalError", final)
			}

			for _, o := range waiters.collect(t) {
				if o.got != final {
					t.Errorf("%s got %v, want the identical published failure %v", o.name, o.got, final)
				}
				if o.early {
					t.Errorf("%s returned before the sink was released, want it to wait for the decision", o.name)
				}
			}

			// Everything that can still be asked reports the same value.
			if got := error(session.fatalErr()); got != final {
				t.Errorf("fatalErr() = %v, want the identical %v", got, final)
			}
			if p, err := mr.NextPart(); p != nil || err != final {
				t.Errorf("the iterator that hit it = (%v, %v), want (nil, %v)", p, err, final)
			}
			var visited [][]int
			walkErr := e.Walk(func(path []int, _ *Entity, err error) error {
				visited = append(visited, path)
				return err
			})
			if walkErr != final {
				t.Errorf("Walk() = %v, want the identical %v", walkErr, final)
			}
			if visited != nil {
				t.Errorf("Walk visited %v, want nothing: the parse had already stopped", visited)
			}

			// The sink was called once for this diagnostic, and only for it.
			if got := sink.delivered(); !reflect.DeepEqual(got, []Code{CodeLimitPartCount}) {
				t.Errorf("the sink received %v, want exactly one part-count diagnostic", got)
			}

			// What the parse ended with, and what it carries.
			var fatal FatalError
			if !errors.As(final, &fatal) {
				t.Fatalf("final = %v, want a FatalError", final)
			}
			d, hasDiag := fatal.Diagnostic()
			if tc.sinkErr != nil {
				if !errors.Is(final, sentinel) {
					t.Errorf("final = %v, want it to wrap the sink's own error", final)
				}
				if hasDiag {
					t.Errorf("the sink's stop carries diagnostic %v, want none: it is not a defect of the message", d)
				}
			} else {
				if errors.Is(final, sentinel) {
					t.Errorf("final = %v, want the budget's own failure", final)
				}
				if !hasDiag {
					t.Fatal("the budget's failure carries no diagnostic, want the one that stopped the parse")
				}
				checkPartCountFacts(t, d, SeverityFatal, 1, []int{1}, offset)
			}

			// Retention is unchanged by any of this: delivered once either way,
			// retained exactly as the policy says.
			retained := session.Diagnostics()
			if tc.noRetention {
				if retained != nil {
					t.Errorf("Diagnostics() = %v, want none retained", codesOfDiags(retained))
				}
				return
			}
			if len(retained) != 1 {
				t.Fatalf("Diagnostics() = %v, want exactly one part-count entry", codesOfDiags(retained))
			}
			checkPartCountFacts(t, retained[0], SeverityLimitExceeded, 1, []int{1}, offset)
		})
	}
}

// TestParse_partCount_competingStopWhilePending is D's first order: a second
// diagnostic reaches the session while the overflow is still being decided.
//
// Delivery is serialized, so the second one waits for the sink call the overflow
// opened. When that call refuses to continue, the parse ends with the caller's
// decision, and the pending overflow converges on it rather than publishing a
// second failure — so both callers, and every later reader, hold one value.
func TestParse_partCount_competingStopWhilePending(t *testing.T) {
	sentinel := errors.New("stop here")

	limits := DefaultLimits()
	limits.MaxPartCount = 1
	sink := newBlockingSink(CodeLimitPartCount, sentinel)
	session := newParseSessionWithConfig(resolvedConfig{
		profile: ProfileTolerant,
		compat:  CompatibilityConservative,
		limits:  limits,
	}, diagnosticOptions{sink: sink})

	// The one part within the budget.
	if fatal, claimed := session.chargePart(); fatal != nil || claimed {
		t.Fatalf("chargePart() = (%v, %v), want the part to fit", fatal, claimed)
	}

	overflowDiag := Diagnostic{Code: CodeLimitPartCount, Severity: SeverityLimitExceeded, Path: []int{1}}
	fatal, claimed := session.chargePart()
	if fatal != nil || !claimed {
		t.Fatalf("chargePart() = (%v, %v), want the overflow claimed", fatal, claimed)
	}

	sink.arm()
	overflow := make(chan error, 1)
	go func() { overflow <- session.resolvePart(overflowDiag) }()

	await(t, sink.entered, "the sink to be handed the part-count diagnostic")

	// A second diagnostic, from another scope of the same parse, arrives while
	// the first is still being delivered. It blocks on the delivery lock, which
	// is the existing contract, and must not deadlock against the transition.
	competing := make(chan error, 1)
	go func() {
		competing <- session.record(Diagnostic{Code: CodeCTEUnknown, Severity: SeverityUnsupported})
	}()

	if published, open := pendingState(session); published != nil || !open {
		t.Errorf("state while the sink decides = (published %v, transition open %v), want (nil, true)", published, open)
	}

	close(sink.release)

	final := <-overflow
	if final == nil {
		t.Fatal("resolvePart() returned nothing, want the published failure")
	}
	if !errors.Is(final, sentinel) {
		t.Errorf("final = %v, want the sink's own error: a caller's stop outranks a budget", final)
	}

	var competingErr error
	select {
	case competingErr = <-competing:
	case <-time.After(deadlockTimeout):
		t.Fatal("the competing record never returned")
	}
	if competingErr != final {
		t.Errorf("the competing record = %v, want the identical published failure %v", competingErr, final)
	}
	if got := error(session.fatalErr()); got != final {
		t.Errorf("fatalErr() = %v, want the identical %v", got, final)
	}

	// The sink was entered once; the competing diagnostic arrived after it had
	// already failed, so it was retained without being delivered.
	if got := sink.delivered(); !reflect.DeepEqual(got, []Code{CodeLimitPartCount}) {
		t.Errorf("the sink received %v, want only the part-count diagnostic", got)
	}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitPartCount, CodeCTEUnknown}) {
		t.Errorf("Diagnostics() = %v, want both in discovery order and neither twice", got)
	}
}

// TestParse_partCount_competingStopBeforeOverflow is D's other order: the sink
// has already ended the parse when the budget runs out.
//
// There is nothing to claim and nothing to record. The charge reports the
// failure that is already published, which is what keeps a caller's own decision
// from being reported back as a defect of the message.
func TestParse_partCount_competingStopBeforeOverflow(t *testing.T) {
	sentinel := errors.New("stop here")

	limits := DefaultLimits()
	limits.MaxPartCount = 1
	session := newParseSessionWithConfig(resolvedConfig{
		profile: ProfileTolerant,
		compat:  CompatibilityConservative,
		limits:  limits,
	}, diagnosticOptions{
		sink: DiagnosticSinkFunc(func(_ *ParseSession, _ Diagnostic) error { return sentinel }),
	})

	stopped := session.record(Diagnostic{Code: CodeCTEUnknown, Severity: SeverityUnsupported})
	if !errors.Is(stopped, sentinel) {
		t.Fatalf("record() = %v, want the sink's stop", stopped)
	}

	// The budget is not charged at all past a stop, so both of these report it.
	for i := 0; i < 3; i++ {
		fatal, claimed := session.chargePart()
		if claimed {
			t.Fatalf("charge %d claimed an overflow, want the published stop reported instead", i)
		}
		if fatal != stopped {
			t.Errorf("charge %d = %v, want the identical published failure %v", i, fatal, stopped)
		}
	}
	if got := error(session.fatalErr()); got != stopped {
		t.Errorf("fatalErr() = %v, want the identical %v", got, stopped)
	}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeCTEUnknown}) {
		t.Errorf("Diagnostics() = %v, want only the diagnostic that stopped the parse", got)
	}
}

// TestParse_partCount_noSinkPublishesTheBudget pins the plainest configuration,
// which the blocking tests above cannot exercise: with no sink there is nothing
// to decide, and the transition still publishes exactly one budget failure.
func TestParse_partCount_noSinkPublishesTheBudget(t *testing.T) {
	f := wideContainer(2, "\r\n")

	session, got, err := parseUnder(t, f.text, partsOnly(1))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	checkPartCountFatal(t, got.err, 1, []int{1}, f.offset(t, "part1.header"))
	if final := error(session.fatalErr()); final != got.err {
		t.Errorf("fatalErr() = %v, want the identical error the scope reported %v", final, got.err)
	}
	if _, open := pendingState(session); open {
		t.Error("a transition is still open after the failure was published, want none")
	}
	checkPartCountDiagnostic(t, session, 1, []int{1}, f.offset(t, "part1.header"))
}

// TestParse_partCount_acceptedPartBuildsNothing is E: the cost of a part that
// fits.
//
// chargePartCount runs once for every part of every message, and almost every
// part fits. Nothing about a refusal — the message naming the budget, the
// diagnostic, the copy of the part's path it would carry — may be built on that
// path, and an allocation count is the way to say so that does not depend on a
// clock. The context below is two levels deep, so a path copy would be a real
// allocation rather than a nil slice.
func TestParse_partCount_acceptedPartBuildsNothing(t *testing.T) {
	limits := DefaultLimits()
	// A budget nothing reaches, so every run takes the accepted path.
	limits.MaxPartCount = 1 << 40
	session := newParseSessionWithConfig(resolvedConfig{
		profile: ProfileTolerant,
		compat:  CompatibilityConservative,
		limits:  limits,
	}, diagnosticOptions{})

	ctx := rootContext(session, 10).child(0, 4, 20).child(1, 6, 30)
	if len(ctx.origin.path) != 2 {
		t.Fatalf("the fixture context has path %v, want one a copy would allocate for", ctx.origin.path)
	}
	r := &multipartReader{parent: ctx}

	var err error
	allocs := testing.AllocsPerRun(200, func() {
		err = r.chargePartCount(ctx)
	})
	if err != nil {
		t.Fatalf("chargePartCount() = %v, want the part to fit", err)
	}
	if allocs != 0 {
		t.Errorf("an accepted part allocates %v times, want 0: nothing about a refusal may be built here", allocs)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none: nothing was refused", codesOfDiags(d))
	}

	// The control: the same call on the refusing path does build it, completed
	// for the part that was refused. A fix that stopped allocating by no longer
	// diagnosing would pass above and fail here.
	small := DefaultLimits()
	small.MaxPartCount = 1
	refusing := newParseSessionWithConfig(resolvedConfig{
		profile: ProfileTolerant,
		compat:  CompatibilityConservative,
		limits:  small,
	}, diagnosticOptions{})

	rctx := rootContext(refusing, 10).child(0, 4, 20).child(1, 6, 30)
	rr := &multipartReader{parent: rctx}
	if err := rr.chargePartCount(rctx); err != nil {
		t.Fatalf("the first charge = %v, want it to fit", err)
	}
	if err := rr.chargePartCount(rctx); !IsFatal(err) {
		t.Fatalf("the second charge = %v, want the part-count refusal", err)
	}
	diags := refusing.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one part-count entry", codesOfDiags(diags))
	}
	checkPartCountFacts(t, diags[0], SeverityLimitExceeded, 1, []int{0, 1}, rctx.origin.headerOffset)
}
