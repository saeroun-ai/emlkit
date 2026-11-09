package message

import (
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ROB-001-008 (issue #8): Limits.MaxDiagnosticCount bounds what a session
// records and delivers. A positive budget N records requests 1..N-1 as they
// are, replaces request N with one terminal limit.diagnostic_count diagnostic,
// and omits every later request without copying, retaining or delivering it.
// It ends no scope: the parse heals and returns everything it otherwise would.

// diagnosticBudgetSession returns a session running under the default budgets
// with MaxDiagnosticCount set to n.
func diagnosticBudgetSession(n int64, opts diagnosticOptions) *ParseSession {
	l := DefaultLimits()
	l.MaxDiagnosticCount = n
	return newParseSessionWithConfig(resolvedConfig{
		profile: ProfileTolerant,
		compat:  CompatibilityConservative,
		limits:  l,
	}, opts)
}

// numberedDiagnostic is the i-th synthetic request: its Message, Path and
// Location all identify it.
func numberedDiagnostic(i int) Diagnostic {
	return Diagnostic{
		Code:     CodeMIMEContentTypeInvalid,
		Severity: SeverityRecovered,
		Message:  strconv.Itoa(i),
		Field:    "Content-Type",
		Path:     []int{i, 7},
		Location: LocationAt(int64(100 + i)),
		Cause:    errors.New("cause " + strconv.Itoa(i)),
	}
}

// checkTerminal reports what is wrong with d as the terminal that replaced the
// request numbered at.
func checkTerminal(t *testing.T, d Diagnostic, limit int64, at int) {
	t.Helper()
	want := Diagnostic{
		Code:     CodeLimitDiagnosticCount,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("parse reached the limit of %d diagnostics", limit),
		Action:   diagnosticCountAction,
		Path:     []int{at, 7},
		Location: LocationAt(int64(100 + at)),
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("terminal = %#v\nwant       %#v", d, want)
	}
}

// checkCapped reports what is wrong with got as the first min(requests, n)
// numbered requests under a budget of n.
func checkCapped(t *testing.T, got []Diagnostic, n int64, requests int) {
	t.Helper()
	want := requests
	if int64(requests) >= n {
		want = int(n)
	}
	if len(got) != want {
		t.Fatalf("got %d diagnostics, want %d", len(got), want)
	}
	for i, d := range got {
		if int64(requests) >= n && i == want-1 {
			checkTerminal(t, d, n, i)
			continue
		}
		if d.Code != CodeMIMEContentTypeInvalid || d.Message != strconv.Itoa(i) {
			t.Errorf("diagnostic %d = %v, want request %d unchanged", i, d, i)
		}
	}
}

func TestRecord_diagnosticCountSlots(t *testing.T) {
	for _, n := range []int64{1, 2, 3, 16} {
		for _, requests := range []int{0, int(n) - 1, int(n), int(n) + 1, int(n) + 5} {
			for _, mode := range []struct {
				name        string
				sink        bool
				noRetention bool
			}{
				{"retain", false, false},
				{"retain, no-retention without a sink", false, true},
				{"retain and deliver", true, false},
				{"deliver only", true, true},
			} {
				t.Run(fmt.Sprintf("N=%d/requests=%d/%s", n, requests, mode.name), func(t *testing.T) {
					var delivered []Diagnostic
					opts := diagnosticOptions{noRetention: mode.noRetention}
					if mode.sink {
						opts.sink = DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
							delivered = append(delivered, d)
							return nil
						})
					}
					s := diagnosticBudgetSession(n, opts)
					for i := range requests {
						if err := s.record(numberedDiagnostic(i)); err != nil {
							t.Fatalf("record(%d) = %v, want nil", i, err)
						}
					}

					retained := !(mode.sink && mode.noRetention)
					if retained {
						checkCapped(t, s.Diagnostics(), n, requests)
					} else if s.Len() != 0 || s.Diagnostics() != nil {
						t.Errorf("Len() = %d, want nothing retained", s.Len())
					}
					if mode.sink {
						checkCapped(t, delivered, n, requests)
					}
					if want := min(int64(requests), n); s.diagSlots != want {
						t.Errorf("diagSlots = %d, want %d", s.diagSlots, want)
					}
					if s.fatalErr() != nil {
						t.Errorf("fatalErr() = %v, want nil: the cap stops nothing", s.fatalErr())
					}
				})
			}
		}
	}
}

func TestRecord_diagnosticCountDisabledAndDefault(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		s := diagnosticBudgetSession(LimitDisabled, diagnosticOptions{})
		for i := range 2500 {
			_ = s.record(numberedDiagnostic(i))
		}
		if s.Len() != 2500 || s.diagSlots != 0 || s.diagClosed {
			t.Errorf("Len() = %d, diagSlots = %d, closed = %v; want 2500, 0, false", s.Len(), s.diagSlots, s.diagClosed)
		}
		for _, d := range s.Diagnostics() {
			if d.Code == CodeLimitDiagnosticCount {
				t.Fatal("a disabled budget recorded a terminal")
			}
		}
	})

	for name, s := range map[string]*ParseSession{
		"zero session":    {},
		"newParseSession": newParseSession(ProfileTolerant, CompatibilityConservative),
	} {
		t.Run(name, func(t *testing.T) {
			for i := range 1005 {
				_ = s.record(numberedDiagnostic(i))
			}
			checkCapped(t, s.Diagnostics(), defaultMaxDiagnosticCount, 1005)
		})
	}
}

// TestRecord_diagnosticCountMaxInt64 drives the count to the top of int64,
// where an increment past the budget would wrap.
func TestRecord_diagnosticCountMaxInt64(t *testing.T) {
	s := diagnosticBudgetSession(math.MaxInt64, diagnosticOptions{})
	s.diagSlots = math.MaxInt64 - 2
	for i := range 5 {
		_ = s.record(numberedDiagnostic(i))
	}
	got := s.Diagnostics()
	if len(got) != 2 || got[0].Message != "0" {
		t.Fatalf("Diagnostics() = %v, want request 0 and the terminal", got)
	}
	checkTerminal(t, got[1], math.MaxInt64, 1)
	if s.diagSlots != math.MaxInt64 || !s.diagClosed {
		t.Errorf("diagSlots = %d, closed = %v; want MaxInt64, true", s.diagSlots, s.diagClosed)
	}
}

// TestRecord_diagnosticCountOmitsWithoutCopying pins that an omitted request
// costs no allocation, neither in the session nor in a parse context, which
// would otherwise copy the entity's path into it.
func TestRecord_diagnosticCountOmitsWithoutCopying(t *testing.T) {
	calls := 0
	s := diagnosticBudgetSession(1, diagnosticOptions{sink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
		calls++
		return nil
	})})
	_ = s.record(numberedDiagnostic(0))
	if !s.omittingDiagnostics() || calls != 1 {
		t.Fatalf("after the terminal: omitting = %v, calls = %d", s.omittingDiagnostics(), calls)
	}

	d := numberedDiagnostic(1)
	c := parseContext{session: s, origin: entityOrigin{known: true, path: []int{3, 4, 5}, headerOffset: 9, bodyOffset: 20}}
	allocs := testing.AllocsPerRun(200, func() {
		_ = s.record(d)
		_ = c.recordHeader(Diagnostic{Code: CodeMIMEContentTypeInvalid})
		_ = c.recordBody(Diagnostic{Code: CodeMIMEContentTypeInvalid})
	})
	if allocs != 0 {
		t.Errorf("omitted requests allocated %v times per run, want 0", allocs)
	}
	if calls != 1 || s.Len() != 1 {
		t.Errorf("calls = %d, Len() = %d; want the terminal alone", calls, s.Len())
	}
}

// TestRecord_diagnosticCountSinkRefusesTerminal pins that the terminal is
// delivered like any diagnostic: a sink refusing it stops the session, and the
// stop is what every later request reports, with the sink never called again.
func TestRecord_diagnosticCountSinkRefusesTerminal(t *testing.T) {
	sentinel := errors.New("enough")
	for _, noRetention := range []bool{false, true} {
		t.Run(fmt.Sprintf("noRetention=%v", noRetention), func(t *testing.T) {
			calls := 0
			s := diagnosticBudgetSession(3, diagnosticOptions{noRetention: noRetention, sink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
				calls++
				if d.Code == CodeLimitDiagnosticCount {
					return sentinel
				}
				return nil
			})})
			for i := range 2 {
				if err := s.record(numberedDiagnostic(i)); err != nil {
					t.Fatalf("record(%d) = %v", i, err)
				}
			}
			stop := s.record(numberedDiagnostic(2))
			if !errors.Is(stop, sentinel) || !IsFatal(stop) || stop != s.fatalErr() {
				t.Fatalf("terminal record = %v, want the published FatalError wrapping the sink's error", stop)
			}
			for i := 3; i < 6; i++ {
				if err := s.record(numberedDiagnostic(i)); err != stop {
					t.Errorf("record(%d) = %v, want the identical stop", i, err)
				}
			}
			if calls != 3 {
				t.Errorf("sink called %d times, want 3", calls)
			}
			if want := 3; noRetention {
				if s.Len() != 0 {
					t.Errorf("Len() = %d, want 0", s.Len())
				}
			} else {
				checkCapped(t, s.Diagnostics(), 3, want)
			}
		})
	}
}

// TestRecord_diagnosticCountAfterFailedSink pins that a sink that failed before
// the cap is still reported by every request — ordinary, terminal and omitted —
// and never called again.
func TestRecord_diagnosticCountAfterFailedSink(t *testing.T) {
	sentinel := errors.New("refused first")
	calls := 0
	s := diagnosticBudgetSession(3, diagnosticOptions{sink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
		calls++
		return sentinel
	})})
	stop := s.record(numberedDiagnostic(0))
	if !errors.Is(stop, sentinel) {
		t.Fatalf("record(0) = %v", stop)
	}
	for i := 1; i < 6; i++ {
		if err := s.record(numberedDiagnostic(i)); err != stop {
			t.Errorf("record(%d) = %v, want the identical stop", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("sink called %d times, want 1", calls)
	}
	checkCapped(t, s.Diagnostics(), 3, 6)
}

// TestRecord_diagnosticCountInputOverflow pins that the input budget's stop is
// published whether its diagnostic takes the terminal slot or is omitted.
func TestRecord_diagnosticCountInputOverflow(t *testing.T) {
	t.Run("overflow takes the terminal slot", func(t *testing.T) {
		s := diagnosticBudgetSession(1, diagnosticOptions{})
		err := s.inputOverflow(10)
		checkStopDiagnostic(t, err, s, CodeLimitInputBytes)
		got := s.Diagnostics()
		if len(got) != 1 || got[0].Code != CodeLimitDiagnosticCount || got[0].Location != LocationAt(10) {
			t.Errorf("Diagnostics() = %v, want the terminal at offset 10", got)
		}
	})
	t.Run("overflow omitted", func(t *testing.T) {
		s := diagnosticBudgetSession(2, diagnosticOptions{})
		_ = s.record(numberedDiagnostic(0))
		_ = s.record(numberedDiagnostic(1))
		err := s.inputOverflow(10)
		checkStopDiagnostic(t, err, s, CodeLimitInputBytes)
		checkCapped(t, s.Diagnostics(), 2, 2)
	})
}

// checkStopDiagnostic reports what is wrong with err as the session's published
// stop carrying a diagnostic with code.
func checkStopDiagnostic(t *testing.T, err error, s *ParseSession, code Code) {
	t.Helper()
	var f FatalError
	if !errors.As(err, &f) {
		t.Fatalf("err = %v, want a FatalError", err)
	}
	if d, ok := f.Diagnostic(); !ok || d.Code != code {
		t.Errorf("stop carries %v, want %s", d, code)
	}
	if published := s.fatalErr(); published == nil || error(published) != err {
		t.Errorf("fatalErr() = %v, want the identical %v", published, err)
	}
}

// TestRecord_diagnosticCountConcurrent records from many goroutines into one
// session whose sink reads the session back, and into many sessions sharing one
// sink. Run it with -race.
func TestRecord_diagnosticCountConcurrent(t *testing.T) {
	const n, goroutines, each = 64, 16, 50

	t.Run("one session", func(t *testing.T) {
		var delivered []Diagnostic
		s := diagnosticBudgetSession(n, diagnosticOptions{sink: DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
			// Delivery is serialized per session, so no lock is needed here.
			delivered = append(delivered, d)
			ds := s.Diagnostics()
			if s.Len() != len(delivered) || len(ds) != len(delivered) || ds[len(ds)-1].Code != d.Code {
				t.Errorf("reentrant view: Len() = %d, %d retained, %d delivered", s.Len(), len(ds), len(delivered))
			}
			_ = s.Limits()
			_ = s.omittingDiagnostics()
			return nil
		})})
		var wg sync.WaitGroup
		for g := range goroutines {
			wg.Go(func() {
				for i := range each {
					if err := s.record(numberedDiagnostic(g*each + i)); err != nil {
						t.Errorf("record = %v", err)
					}
				}
			})
		}
		wg.Wait()

		retained := s.Diagnostics()
		if len(delivered) != n || !reflect.DeepEqual(retained, delivered) {
			t.Fatalf("%d delivered, %d retained, want the same %d", len(delivered), len(retained), n)
		}
		for i, d := range retained {
			if (d.Code == CodeLimitDiagnosticCount) != (i == n-1) {
				t.Errorf("diagnostic %d = %v; the terminal must be last and only last", i, d)
			}
		}
		if s.diagSlots != n {
			t.Errorf("diagSlots = %d, want %d", s.diagSlots, n)
		}
	})

	t.Run("sessions sharing a sink", func(t *testing.T) {
		var mu sync.Mutex
		perSession := map[*ParseSession][]Code{}
		sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
			mu.Lock()
			defer mu.Unlock()
			perSession[s] = append(perSession[s], d.Code)
			return nil
		})
		sessions := make([]*ParseSession, goroutines)
		var wg sync.WaitGroup
		for g := range sessions {
			s := diagnosticBudgetSession(n, diagnosticOptions{sink: sink, noRetention: g%2 == 0})
			sessions[g] = s
			wg.Go(func() {
				for i := range 200 {
					_ = s.record(numberedDiagnostic(i))
				}
			})
		}
		wg.Wait()
		for g, s := range sessions {
			got := perSession[s]
			if len(got) != n {
				t.Fatalf("session %d: %d delivered, want %d", g, len(got), n)
			}
			for i, code := range got {
				if (code == CodeLimitDiagnosticCount) != (i == n-1) {
					t.Errorf("session %d: delivery %d is %s; the terminal must be last and only last", g, i, code)
				}
			}
		}
	})
}

// diagnosticStorm is a multipart message of total parts, each with a malformed
// Content-Type parameter that Tolerant parsing heals with one diagnostic.
func diagnosticStorm(total int) string {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := range total {
		fmt.Fprintf(&b, "--B\r\nContent-Type: text/plain; broken\r\n\r\npart%d\r\n", i)
	}
	b.WriteString("--B--\r\n")
	return b.String()
}

// readStorm reads every part of e through its multipart reader and returns the
// bodies and the error that ended iteration, or nil at io.EOF.
func readStorm(e *Entity) ([]string, error) {
	mr := e.MultipartReader()
	var bodies []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return bodies, nil
		}
		if err != nil {
			return bodies, err
		}
		b, err := io.ReadAll(p.Body)
		if err != nil {
			return bodies, err
		}
		bodies = append(bodies, string(b))
	}
}

// TestParse_diagnosticStormHealsEveryPart is the issue #8 acceptance test
// through Parse: every malformed part is healed and returned with its body,
// while the session records and delivers no more than the budget.
func TestParse_diagnosticStormHealsEveryPart(t *testing.T) {
	for _, tc := range []struct {
		total int
		limit int64 // 0 for the default
	}{
		{1005, 0}, {1005, 1005}, {1005, 1006}, {1005, LimitDisabled},
		{20, 1}, {20, 19}, {20, 20}, {20, 21}, {0, 1}, {1, 1}, {1, 2},
	} {
		for _, mode := range []string{"retain", "retain and deliver", "deliver only"} {
			t.Run(fmt.Sprintf("total=%d/limit=%d/%s", tc.total, tc.limit, mode), func(t *testing.T) {
				var delivered []Diagnostic
				opts := &ParseOptions{Limits: Limits{MaxDiagnosticCount: tc.limit}}
				if mode != "retain" {
					opts.DiagnosticSink = DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
						delivered = append(delivered, d)
						return nil
					})
					opts.DiscardDeliveredDiagnostics = mode == "deliver only"
				}
				s, e, err := Parse(strings.NewReader(diagnosticStorm(tc.total)), opts)
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				bodies, err := readStorm(e)
				if err != nil || e.ParseStop() != nil {
					t.Fatalf("iteration ended with %v, ParseStop() = %v", err, e.ParseStop())
				}
				if len(bodies) != tc.total {
					t.Fatalf("%d parts returned, want %d", len(bodies), tc.total)
				}
				for i, b := range bodies {
					if b != fmt.Sprintf("part%d", i) {
						t.Fatalf("part %d body = %q", i, b)
					}
				}

				limit := s.Limits().MaxDiagnosticCount
				got := s.Diagnostics()
				if mode == "deliver only" {
					if len(got) != 0 {
						t.Errorf("%d retained, want none", len(got))
					}
					got = delivered
				} else if mode == "retain and deliver" && !reflect.DeepEqual(got, delivered) {
					t.Errorf("retained and delivered disagree: %d vs %d", len(got), len(delivered))
				}
				want, terminal := tc.total, false
				if limit != LimitDisabled && int64(tc.total) >= limit {
					want, terminal = int(limit), true
				}
				if len(got) != want {
					t.Fatalf("%d diagnostics, want %d", len(got), want)
				}
				for i, d := range got {
					if !reflect.DeepEqual(d.Path, []int{i}) {
						t.Errorf("diagnostic %d path = %v, want [%d]", i, d.Path, i)
					}
					isTerminal := d.Code == CodeLimitDiagnosticCount
					if isTerminal != (terminal && i == want-1) {
						t.Errorf("diagnostic %d = %v", i, d)
					}
					if !isTerminal && d.Code != CodeMIMEContentTypeInvalid {
						t.Errorf("diagnostic %d code = %s", i, d.Code)
					}
				}
			})
		}
	}
}

// TestParse_diagnosticCapThenInputBudget pins that the input budget still ends
// the parse after the diagnostics were closed, with its own stop.
func TestParse_diagnosticCapThenInputBudget(t *testing.T) {
	text := diagnosticStorm(10)
	s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: Limits{
		MaxDiagnosticCount: 1,
		MaxInputBytes:      int64(strings.Index(text, "part8") + 3),
	}})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	bodies, err := readStorm(e)
	if !IsFatal(err) || e.ParseStop() == nil || !errors.Is(err, e.ParseStop()) {
		t.Fatalf("iteration ended with %v, ParseStop() = %v; want the input stop", err, e.ParseStop())
	}
	checkStopDiagnostic(t, e.ParseStop(), s, CodeLimitInputBytes)
	if len(bodies) != 8 {
		t.Errorf("%d parts read before the overflow, want 8", len(bodies))
	}
	if got := s.Diagnostics(); len(got) != 1 || got[0].Code != CodeLimitDiagnosticCount {
		t.Errorf("Diagnostics() = %v, want the terminal alone", got)
	}
	if _, again := e.MultipartReader().NextPart(); again != e.ParseStop() {
		t.Errorf("NextPart() after the stop = %v, want the identical stop", again)
	}
}

// TestParse_diagnosticCapThenPartCount pins the same for the part-count budget,
// whose transition delivers before it publishes.
func TestParse_diagnosticCapThenPartCount(t *testing.T) {
	for _, withSink := range []bool{false, true} {
		t.Run(fmt.Sprintf("sink=%v", withSink), func(t *testing.T) {
			calls := 0
			opts := &ParseOptions{Limits: Limits{MaxDiagnosticCount: 2, MaxPartCount: 5}}
			if withSink {
				opts.DiagnosticSink = DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
					calls++
					return nil
				})
			}
			s, e, err := Parse(strings.NewReader(diagnosticStorm(10)), opts)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			bodies, err := readStorm(e)
			if len(bodies) != 5 || !IsFatal(err) || err != e.ParseStop() {
				t.Fatalf("%d parts, iteration ended with %v; want 5 and the part-count stop", len(bodies), err)
			}
			checkStopDiagnostic(t, err, s, CodeLimitPartCount)
			if got := s.Diagnostics(); len(got) != 2 || got[1].Code != CodeLimitDiagnosticCount {
				t.Errorf("Diagnostics() = %v, want one repair and the terminal", got)
			}
			if withSink && calls != 2 {
				t.Errorf("sink called %d times, want 2", calls)
			}
		})
	}
}

// TestParse_diagnosticCapSinkRefusesTerminal pins that a sink refusing the
// terminal stops the parse, and that the stop keeps its identity.
func TestParse_diagnosticCapSinkRefusesTerminal(t *testing.T) {
	sentinel := errors.New("no more")
	calls := 0
	s, e, err := Parse(strings.NewReader(diagnosticStorm(10)), &ParseOptions{
		Limits: Limits{MaxDiagnosticCount: 2},
		DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			calls++
			if d.Code == CodeLimitDiagnosticCount {
				return sentinel
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	_, err = readStorm(e)
	stop := e.ParseStop()
	if !errors.Is(err, sentinel) || stop == nil || !errors.Is(stop, sentinel) {
		t.Fatalf("iteration ended with %v, ParseStop() = %v; want the sink's stop", err, stop)
	}
	for range 2 {
		if _, again := e.MultipartReader().NextPart(); again != stop {
			t.Errorf("NextPart() after the stop = %v, want the identical stop", again)
		}
	}
	if calls != 2 || s.Len() != 2 {
		t.Errorf("calls = %d, Len() = %d; want 2 and 2", calls, s.Len())
	}
}

// TestRecord_diagnosticCountKeepsPublishedStop pins that neither the terminal
// nor an omitted request replaces or hides a budget stop published before them.
func TestRecord_diagnosticCountKeepsPublishedStop(t *testing.T) {
	sentinel := errors.New("refuse the terminal")
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sinkRefusesTerminal=%v", refuse), func(t *testing.T) {
			calls := 0
			s := diagnosticBudgetSession(2, diagnosticOptions{sink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
				calls++
				if refuse && d.Code == CodeLimitDiagnosticCount {
					return sentinel
				}
				return nil
			})})
			stop := s.inputOverflow(10) // takes slot 1 and publishes the budget's stop
			checkStopDiagnostic(t, stop, s, CodeLimitInputBytes)

			// The terminal: a sink refusing it fails the sink, but the budget's
			// stop was published first and is what record reports.
			got := s.record(numberedDiagnostic(1))
			if refuse {
				if got != stop || errors.Is(got, sentinel) {
					t.Errorf("terminal record = %v, want the identical budget stop", got)
				}
			} else if got != nil {
				t.Errorf("terminal record = %v, want nil", got)
			}
			// Omitted requests answer as ordinary ones do after the sink stopped
			// being called: the published stop for a failed sink, nil otherwise.
			for i := 2; i < 5; i++ {
				got := s.record(numberedDiagnostic(i))
				if refuse && got != stop || !refuse && got != nil {
					t.Errorf("omitted record(%d) = %v", i, got)
				}
			}
			if published := s.fatalErr(); published != stop {
				t.Errorf("fatalErr() = %v, want the identical budget stop", published)
			}
			if calls != 2 {
				t.Errorf("sink called %d times, want 2", calls)
			}
			got2 := s.Diagnostics()
			if len(got2) != 2 || got2[0].Code != CodeLimitInputBytes {
				t.Fatalf("Diagnostics() = %v", got2)
			}
			checkTerminal(t, got2[1], 2, 1)
		})
	}
}

// requestsBefore returns how many diagnostics a parse of text under limits, with
// the diagnostic budget disabled, records before the first one with code.
func requestsBefore(t *testing.T, text string, limits Limits, code Code) int {
	t.Helper()
	limits.MaxDiagnosticCount = LimitDisabled
	s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
	if err == nil {
		_, _ = readStorm(e)
	}
	for i, d := range s.Diagnostics() {
		if d.Code == code {
			return i
		}
	}
	t.Fatalf("the uncapped parse recorded no %s: %v", code, s.Diagnostics())
	return 0
}

// TestParse_diagnosticCountTerminalReplacesFailure makes the request the
// terminal replaces a budget failure that ends the whole parse. The failure must
// still end it with its own FatalError carrying its own diagnostic, unless the
// sink refuses the terminal, whose stop outranks the budget's.
func TestParse_diagnosticCountTerminalReplacesFailure(t *testing.T) {
	text := diagnosticStorm(10)
	for _, tc := range []struct {
		name   string
		limits Limits
		code   Code
	}{
		{"input bytes", Limits{MaxInputBytes: int64(strings.Index(text, "part8") + 3)}, CodeLimitInputBytes},
		{"part count", Limits{MaxPartCount: 5}, CodeLimitPartCount},
	} {
		k := requestsBefore(t, text, tc.limits, tc.code)
		for _, refuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sinkRefusesTerminal=%v", tc.name, refuse), func(t *testing.T) {
				sentinel := errors.New("refuse the terminal")
				calls := 0
				limits := tc.limits
				limits.MaxDiagnosticCount = int64(k + 1)
				s, e, err := Parse(strings.NewReader(text), &ParseOptions{
					Limits: limits,
					DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
						calls++
						if refuse && d.Code == CodeLimitDiagnosticCount {
							return sentinel
						}
						return nil
					}),
				})
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				_, err = readStorm(e)
				stop := e.ParseStop()
				if stop == nil || !IsFatal(err) || !errors.Is(err, stop) {
					t.Fatalf("iteration ended with %v, ParseStop() = %v; want the parse stopped", err, stop)
				}
				var f FatalError
				if !errors.As(stop, &f) {
					t.Fatalf("ParseStop() = %v, want a FatalError", stop)
				}
				failure, has := f.Diagnostic()
				if refuse {
					if !errors.Is(stop, sentinel) || has {
						t.Errorf("ParseStop() = %v, want the sink's stop", stop)
					}
				} else if !has || failure.Code != tc.code {
					t.Errorf("ParseStop() carries %v, want the %s failure itself", failure, tc.code)
				}

				got := s.Diagnostics()
				if len(got) != k+1 || calls != k+1 {
					t.Fatalf("%d retained, %d delivered; want %d", len(got), calls, k+1)
				}
				terminal := got[k]
				if terminal.Code != CodeLimitDiagnosticCount || terminal.Action != diagnosticCountAction || terminal.Severity != SeverityLimitExceeded {
					t.Errorf("last diagnostic = %v, want the terminal", terminal)
				}
				if has && (terminal.Location != failure.Location || !reflect.DeepEqual(terminal.Path, failure.Path)) {
					t.Errorf("terminal at %v %v, want the replaced failure's %v %v", terminal.Path, terminal.Location, failure.Path, failure.Location)
				}
				for range 2 {
					if _, again := e.MultipartReader().NextPart(); again != stop {
						t.Errorf("NextPart() after the stop = %v, want the identical stop", again)
					}
				}
			})
		}
	}
}
