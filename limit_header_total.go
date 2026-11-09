package message

import (
	"fmt"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Limits.MaxTotalHeaderBytes and MaxTotalHeaderCount: the header blocks of a
// whole parse, bounded together.
//
// # Where it is counted
//
// In textproto's header scanner, through one textproto.HeaderBudget the session
// creates and hands to every header read of the parse: the message's own block
// and the block of every part the parse reads, at any depth, including the parts
// of a body the caller replaced. It is charged as each block is read, before
// what was read is kept, so an exhausted budget stops a block within one read
// of the scanner's buffer, and an exhausted field count stops the next field at
// its first byte; see textproto.HeaderBudget for exactly what is charged.
// Nothing outside a header block is charged, and nothing is refunded.
//
// # What a failure means
//
// The budget belongs to the whole parse, so its failure does too, as for
// MaxPartCount: one limit.total_header_bytes or limit.total_header_count
// diagnostic names the entity whose block crossed it, and one FatalError is
// published as the session's stop, which every scope then reports. The block
// that crossed it yields no header. Everything already returned stays valid.
//
// # Provenance
//
// A failure is this budget's only when the error is the exact value the
// session's own budget refused with, HeaderBudget.Refusal. That is a pointer
// comparison: no Is, As or Unwrap of an error an input returned is called to
// decide it. A refusal of a total kind that is not that value — one an input
// handed back from another parse — is an ordinary input error, passed on
// untouched and not diagnosed.
//
// # Order
//
// A stop already published — by a sink, by MaxInputBytes during the same
// read, or by another budget — comes first and records nothing. Within one
// block the smaller budgets come first, as textproto.HeaderBudget states, and a
// child that fits both totals is then judged by MaxPartCount and MaxMIMEDepth.
// A sink that refuses the diagnostic ends the parse with its own stop.

const headerTotalAction = "stopped at this entity's header block, produced no entity from it, and stopped every further traversal of this parse; everything already returned stays valid"

// isTotalHeaderKind reports whether kind is a budget of the shared header
// budget rather than of one block.
func isTotalHeaderKind(kind textproto.HeaderLimitKind) bool {
	return kind == textproto.HeaderLimitTotalBytes || kind == textproto.HeaderLimitTotalCount
}

// headerTotalRefusal reports whether err is the refusal of this session's shared
// header budget, by identity alone.
func (s *ParseSession) headerTotalRefusal(err error) (*textproto.HeaderLimitError, bool) {
	if s == nil {
		return nil, false
	}
	// A type assertion calls no method of err, and the comparison is of two
	// pointers.
	e, ok := err.(*textproto.HeaderLimitError)
	if !ok || e == nil {
		return nil, false
	}
	s.mu.Lock()
	b := s.headerTotal
	s.mu.Unlock()
	if ref := b.Refusal(); ref == nil || ref != e {
		return nil, false
	}
	return e, true
}

// headerTotalFailure records the one diagnostic of a parse whose header blocks
// exceeded a shared budget, publishes the session's stop, and returns it. ctx
// is the entity whose block crossed it.
//
// It is the transition MaxPartCount uses — claim, deliver with no lock held,
// then publish — so a scope that asks about the stop meanwhile waits for the
// one published value, and a stop published earlier wins with nothing recorded.
func (s *ParseSession) headerTotalFailure(ctx parseContext, ref *textproto.HeaderLimitError) error {
	if stop, claimed := s.claimStop(); !claimed {
		return stop
	}

	code, unit := CodeLimitTotalHeaderBytes, "bytes"
	if ref.Kind() == textproto.HeaderLimitTotalCount {
		code, unit = CodeLimitTotalHeaderCount, "fields"
	}
	d := Diagnostic{
		Code:     code,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("header blocks of this message exceed the total limit of %d %s", ref.Limit(), unit),
		Action:   headerTotalAction,
		Cause:    ref,
	}
	d.Location = ctx.headerLocationAt(ref.UnitOffset() - ref.Offset())
	return s.resolvePart(ctx.completeHeader(d))
}

// claimStop claims the right to publish this session's stop through
// resolvePart, as chargePart does for a MaxPartCount overflow. It returns the
// stop already published, without a claim, when there is one, and waits — with
// no lock held — while another caller's transition is open.
func (s *ParseSession) claimStop() (stop error, claimed bool) {
	for {
		s.mu.Lock()
		if s.stop != nil {
			published := s.stop
			s.mu.Unlock()
			return published, false
		}
		if pending := s.partPending; pending != nil {
			s.mu.Unlock()
			<-pending
			continue
		}
		s.partPending = make(chan struct{})
		s.mu.Unlock()
		return nil, true
	}
}
