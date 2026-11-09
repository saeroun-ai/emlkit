package message

import "fmt"

// diagnosticCountAction is the Action of the terminal limit.diagnostic_count
// diagnostic. It says what happened to the request the terminal stands in for
// and to every later one, and that this budget is not what stops a parse. It
// claims nothing about whether the parse did go on: the request it replaced may
// itself have been a failure that ends the parse, and the sink receiving it may
// refuse it.
const diagnosticCountAction = "the diagnostic that reached this budget and every later one are omitted, neither retained nor delivered; this budget alone does not stop parsing"

// A diagnosticSlot is what one record request is entitled to under
// Limits.MaxDiagnosticCount.
type diagnosticSlot uint8

const (
	// diagnosticOrdinary records the request itself.
	diagnosticOrdinary diagnosticSlot = iota

	// diagnosticTerminal records the one limit.diagnostic_count diagnostic in
	// place of the request, which takes the last slot of the budget.
	diagnosticTerminal

	// diagnosticOmitted records nothing: the budget was used up by an earlier
	// request.
	diagnosticOmitted
)

// claimDiagnosticSlot takes the slot one record request is entitled to.
//
// A positive budget N admits N slots, and the last of them is the terminal's:
// requests 1 through N-1 are ordinary, request N is the terminal, and every
// request after it is omitted. A budget of 1 therefore makes the first request
// the terminal. A disabled budget admits every request and counts nothing.
//
// The count is compared before it is incremented and stops at N, so it never
// exceeds the budget and a budget of math.MaxInt64 cannot overflow it.
//
// It must be called with both s.deliver and s.mu held: deliver is what makes the
// order slots are taken in the order diagnostics are recorded, and mu is what
// lets omittingDiagnostics read the result without waiting for a sink.
func (s *ParseSession) claimDiagnosticSlot() diagnosticSlot {
	if s.diagClosed {
		return diagnosticOmitted
	}
	limit := s.Limits().MaxDiagnosticCount
	if limit <= 0 {
		// LimitDisabled, the only non-positive value a resolved budget holds.
		return diagnosticOrdinary
	}
	// diagSlots < limit here: the request that brought it to limit closed the
	// diagnostics, and a closed session returned above.
	s.diagSlots++
	if s.diagSlots < limit {
		return diagnosticOrdinary
	}
	s.diagClosed = true
	return diagnosticTerminal
}

// diagnosticCountTerminal builds the one limit.diagnostic_count diagnostic of a
// session whose budget the request d has just reached.
//
// It is recorded instead of d, not after it, and it is the last diagnostic the
// session records. It reports where recording stopped — the Path and Location
// of d, the first request that was omitted — rather than any count of defects
// in the message: requests are counted only up to the budget, so the number the
// parse would have produced past it is not known. Nothing of d but its position
// is kept; its Code, Message, Field and Cause are dropped with it. When d
// describes a failure, the FatalError of that failure still carries d itself.
//
// The Path is a copy, so the terminal shares nothing with the caller's d.
func (s *ParseSession) diagnosticCountTerminal(d Diagnostic) Diagnostic {
	limit := s.Limits().MaxDiagnosticCount
	return Diagnostic{
		Code:     CodeLimitDiagnosticCount,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("parse reached the limit of %d diagnostics", limit),
		Action:   diagnosticCountAction,
		Path:     d.Clone().Path,
		Location: d.Location,
	}
}

// omittingDiagnostics reports whether every further diagnostic of this session
// is omitted because Limits.MaxDiagnosticCount has been reached.
//
// Once it reports true it does for good, so a parse context uses it to skip
// completing a diagnostic — copying the entity's path into it — that record
// would discard unread. It saves only that: whatever the producer built before
// handing the diagnostic over has already been paid for. Reporting false promises nothing: record decides again, under
// its own lock, so a request that loses the race for the last slot is still
// omitted there. It never waits for a sink, and reports false for a nil session.
func (s *ParseSession) omittingDiagnostics() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diagClosed
}
