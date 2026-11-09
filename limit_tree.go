package message

import "fmt"

// Limits.MaxMIMEDepth and Limits.MaxPartCount: the two budgets on the shape of
// the MIME tree, applied to every multipart a parse descends into.
//
// # What is counted
//
// Two different things, and they are deliberately not derivable from each
// other. A tree may be one part deep and ten thousand parts wide, or one part
// wide and ten thousand deep, and either shape is what one of these budgets
// exists to refuse.
//
//   - MaxMIMEDepth counts how far the parser has descended. The root entity of
//     a message is at depth zero, whatever its media type is, and every child a
//     multipart body yields is exactly one deeper than the entity whose body it
//     came out of. A positive budget N admits every entity at depth N or less,
//     so the first child it can refuse is one at depth N+1.
//   - MaxPartCount counts the child parts a parse discovers, anywhere in the
//     tree. The root is not one of them, because nothing discovered it: it is
//     the message. Everything a multipart iterator does discover is one part,
//     with no exceptions worth carving out — a leaf, a container that holds
//     further parts, and a part with no header block at all each cost exactly
//     one.
//
// MaxPartCount is one budget for the whole tree, shared by every multipart
// iterator of the parse, and it is never reset when the parser enters a new
// container. That is what makes it a bound: a per-container budget would let a
// message spend it again for every container it declares, and a thousand
// containers of nine hundred parts each would pass a budget of a thousand.
// There is deliberately no separate per-container setting, because a total
// bound already bounds one wide container too.
//
// A tree at exactly a budget is parsed whole. Depth N is reached and iterated;
// the N-th part is returned, and the EOF after it is an ordinary EOF.
//
// # Where it is enforced, and how little is read to enforce it
//
// At the one place a child is discovered: after textproto's multipart reader
// has framed the next part and before this layer makes an Entity out of it.
// Nothing is scanned ahead, nothing is materialized, and the tree is walked
// exactly as lazily as it was before — a budget that had to count the tree
// first would cost more than the tree it was protecting.
//
// The consequence is a look-ahead of exactly one part header, and it is
// deliberate. A budget cannot be spent before the parser knows there is
// something to spend it on: a container that holds exactly the parts the budget
// allows and then ends must end with an ordinary EOF, and a multipart at the
// depth limit that turns out to be empty must not be diagnosed for children it
// never had. So the refusal waits until a child has actually been framed. That
// child's header block is read — it has to be, to find out that it exists — and
// nothing else about it is: its body is not read, no decoder is set up for it,
// no Entity is built from it, and its own parts are never looked for. The one
// header read that way is bounded by the header budgets like any other, so the
// look-ahead is bounded too.
//
// # What a failure means
//
// The two differ, and the difference follows from what each budget describes.
//
// A depth refusal is local to the multipart iterator that hit it. What ran out
// is the room to descend below this container; the entity holding it, the
// siblings already returned, and every shallower part are unaffected and stay
// valid. That iterator stops: it records one limit.mime_depth diagnostic, ends
// with a FatalError carrying it, and returns the identical error however often
// it is asked again, without recording the diagnostic a second time.
//
// A part-count refusal belongs to the whole parse. What ran out is a budget the
// entire tree shares, so there is nowhere left in the message that could
// continue: the failure is published on the session, one limit.part_count
// diagnostic is recorded once for the parse, and every later scope of it — the
// iterator that hit it, iterators built earlier for other containers, and
// Entity.Walk — reports the identical FatalError and recovers nothing further.
// Everything already returned stays valid and owned by the caller, as
// FatalError describes, including the bodies of parts already handed out.
//
// "The identical FatalError" is exact, and making it exact is most of the work
// on the session side, because which failure a parse ends with cannot be decided
// until the diagnostic has been offered to the caller's sink. See the note above
// ParseSession.chargePart for the transition that settles it, and
// ParseSession.publishStop for why it is settled in one place.
//
// # Order
//
// When one child could be refused by more than one thing, the order is fixed so
// that the same message always reports the same budget:
//
//  1. A failure of the underlying reader — a part header past a header budget,
//     a framing line past the line budget, an I/O error — comes first and is
//     reported as itself. No part was framed, so nothing is charged and neither
//     of these budgets is consulted.
//  2. MaxPartCount is charged next, for a child that was framed.
//  3. MaxMIMEDepth is judged only for a child that fit within MaxPartCount, so
//     a child that exceeds both at once reports limit.part_count.
//  4. A diagnostic sink that stops the parse while it is being told about
//     either outranks both: the stop belongs to the session, and it is what
//     every scope reports from then on.
//
// # What is not here
//
// Limits.MaxMessageDepth is not enforced, and cannot be yet. It counts nested
// message/rfc822 and message/global entities, and this engine does not parse
// those bodies as entities at all — they are opaque leaves, so there is no
// nested-message transition to count. ROB-090-001 introduces that model and
// ROB-090-002 activates the budget and its reserved limit.message_depth code.
// A message/rfc822 part is one part like any other here, and the multiparts
// inside its opaque body are not descended into and so cost nothing.

// The actions a tree budget diagnostic reports. Each names what was not done —
// nothing is repaired by either — and how far the stop reaches, which is the
// one way the two failures differ.
const (
	mimeDepthAction = "did not return the child and stopped descending into this multipart; the parent and the parts already returned stay valid"

	partCountAction = "did not return the part and stopped every further traversal of this parse; the parts already returned stay valid"
)

// admitPart decides whether the child this iterator has just framed may be
// returned, and returns the error that ends a scope when it may not.
//
// ctx is the context that child would be given: the session it belongs to, its
// path, where its header block begins, and how deep it is. It is built before
// this call because the diagnostic of a refusal has to name the part that was
// refused, which is the part that was never built.
//
// The two budgets are charged in the fixed order this file documents: the
// count of the whole tree first, then the depth of this branch, so that a child
// exceeding both is always reported the same way.
func (r *multipartReader) admitPart(ctx parseContext) error {
	if err := r.chargePartCount(ctx); err != nil {
		return err
	}
	return r.checkMIMEDepth(ctx)
}

// chargePartCount charges one discovered part against the parse's shared total
// part budget and returns nil while it fits.
//
// Charging is all a part that fits costs, and that is deliberate: this runs once
// for every part of every message, and the overwhelming majority of them fit.
// Nothing about the refusal — the message naming the budget, the completed
// diagnostic, the copy of the part's path it carries — is built until a caller
// has been told it is the one that must build it, so the accepted path
// allocates nothing at all.
//
// The refusal is a two-step transition, and only the caller that claimed it
// finishes it; see ParseSession.chargePart and resolvePart.
func (r *multipartReader) chargePartCount(ctx parseContext) error {
	limits, ok := ctx.limits()
	if !ok || limits.MaxPartCount == LimitDisabled {
		return nil
	}

	fatal, claimed := ctx.session.chargePart()
	if !claimed {
		// Either the part fit, and fatal is nil, or this parse has already
		// stopped — because a sink asked it to, or because another scope reached
		// this same overflow first — and fatal is the one failure every scope of
		// it reports. Neither case has anything to record: a defect recorded
		// twice is a defect reported twice.
		return fatal
	}

	// This caller claimed the overflow, so it owes the session exactly one
	// diagnostic. It is completed here, where the part that was refused is
	// known, so that the copy the session retains and the copy the published
	// failure carries name the same part at the same position.
	return ctx.session.resolvePart(ctx.completeHeader(Diagnostic{
		Code:     CodeLimitPartCount,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("message exceeds the limit of %d parts", limits.MaxPartCount),
		Action:   partCountAction,
	}))
}

// checkMIMEDepth reports whether the child ctx describes is too deeply nested,
// and ends this iterator when it is.
//
// ctx.depth is the depth the child would have, which is one more than the
// depth of the entity whose body this iterator is reading. A budget of N admits
// depth N, so the comparison is against the child's own depth and not against
// the container's.
func (r *multipartReader) checkMIMEDepth(ctx parseContext) error {
	limits, ok := ctx.limits()
	if !ok || limits.MaxMIMEDepth == LimitDisabled || ctx.depth <= limits.MaxMIMEDepth {
		return nil
	}
	return r.mimeDepthFailure(ctx, limits.MaxMIMEDepth)
}

// mimeDepthFailure records the one diagnostic of a child too deeply nested to
// be returned, ends this iterator, and returns the error every later call to it
// will return.
//
// The failure is kept for the reason every sticky failure in this package is
// kept: the iterator must report it identically however often it is asked,
// recording it twice would put one defect in the session twice, and the
// underlying reader cannot be asked again anyway — it stands at the top of a
// part this parse will never descend into.
func (r *multipartReader) mimeDepthFailure(ctx parseContext, limit int64) error {
	// Completed before it is recorded, so that the session's copy and the one
	// the fatal error carries name the same child at the same position: the
	// path it would have had, and the first byte of the header block that
	// proved it existed.
	d := ctx.completeHeader(Diagnostic{
		Code:     CodeLimitMIMEDepth,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("multipart nesting exceeds the depth limit of %d", limit),
		Action:   mimeDepthAction,
	})

	if stop := ctx.recordHeader(d); stop != nil {
		// A sink stopped the whole parse while it was being told about this
		// child. That stop outranks this scope's own failure and is what every
		// scope of the parse now reports, this one included.
		r.fatal = stop
		return stop
	}

	r.fatal = newFatalDiagnostic(d)
	return r.fatal
}

// A MaxPartCount overflow is published in two steps, and this pair of methods
// is that transition.
//
// It has to be two steps. Publishing the failure means knowing which failure it
// is, and that is not known until the limit.part_count diagnostic has been
// offered to the caller's DiagnosticSink: a sink that takes it leaves the budget
// as what ended the parse, and a sink that refuses to continue ends the parse
// with the caller's own decision instead, which outranks a budget. Delivering to
// a sink means calling caller code, which must not happen under the session
// lock. So the two cannot be one atomic step, and pretending otherwise is
// exactly the defect this shape exists to avoid: publishing the budget failure
// before the sink has answered makes it observable, and replacing it afterwards
// changes the identity of a stop that other scopes have already reported.
//
// So instead: chargePart claims the overflow and opens a transition without
// publishing anything, resolvePart delivers the diagnostic with no lock held and
// then publishes exactly one failure, and everything that asks about the
// session's stop in between waits for that publication rather than being shown
// an answer that is about to change. The claim is what makes "exactly one
// caller" true — of every goroutine that observes the same overflow, one is told
// to finish the transition and the rest wait — so the diagnostic is recorded and
// delivered once however many scopes race into it.

// chargePart charges one discovered part against this parse's shared total part
// budget, Limits.MaxPartCount, and opens the transition above when it does not
// fit.
//
// The three outcomes are three different obligations for the caller:
//
//   - (nil, false): the part fits and has been charged. This is the path every
//     part of every well-formed message takes, and it allocates nothing.
//   - (fatal, false): this parse has already stopped, and fatal is the one
//     failure every scope of it reports. Nothing is charged and nothing is owed.
//     If a transition was open, this call waited for it to finish.
//   - (nil, true): this caller claimed the overflow and must call resolvePart
//     with the completed limit.part_count diagnostic. Exactly one caller is ever
//     told this for one session.
//
// A sink that has already stopped the parse outranks the budget: its failure is
// what is reported, no claim is made, and nothing is charged or recorded.
//
// A disabled budget accumulates no usage. There would be nothing to compare a
// total against, and a running total nothing bounds is the arithmetic this whole
// area exists to avoid.
//
// chargePart is safe for concurrent use, and returns (nil, false) for a nil
// session. It holds no lock while it waits.
func (s *ParseSession) chargePart() (fatal error, claimed bool) {
	if s == nil {
		return nil, false
	}

	// Read outside the lock: a session's configuration is written once, before
	// anything is parsed, and never afterwards.
	limit := s.Limits().MaxPartCount

	for {
		// A cancelled parse charges nothing more: its stop is what this part
		// reports. Asked with no lock held; see cancel.go.
		if stop := s.cancelStop(); stop != nil {
			return stop, false
		}

		s.mu.Lock()

		if s.stop != nil {
			published := s.stop
			s.mu.Unlock()
			return published, false
		}

		if pending := s.partPending; pending != nil {
			s.mu.Unlock()
			// Another caller is deciding what this parse stops with. Wait for
			// that, then look again: the transition publishes before it closes
			// the channel, so the next pass takes the branch above. A
			// cancellation also ends the wait, and the next pass reports it.
			select {
			case <-pending:
			case <-s.cancelDone:
			}
			continue
		}

		// withinLimit compares against the remaining headroom rather than
		// summing, so a budget of math.MaxInt64 is enforced exactly instead of
		// wrapping. Its error reports a negative amount, which the counting here
		// cannot produce; treating it as a refusal anyway is what makes this
		// fail closed.
		if ok, err := withinLimit(limit, s.partsUsed, 1); ok && err == nil {
			if limit != LimitDisabled {
				s.partsUsed++
			}
			s.mu.Unlock()
			return nil, false
		}

		// The overflow is claimed here and nothing is published: from now until
		// resolvePart, this session has no answer to give about its stop, and
		// anything that asks waits on this channel.
		s.partPending = make(chan struct{})
		s.mu.Unlock()
		return nil, true
	}
}

// resolvePart finishes the transition a claim from chargePart opened: it records
// d, and publishes the one session-wide FatalError this parse reports from then
// on, which it returns.
//
// Only the caller that was told it claimed the overflow may call this, and it
// must call it — every other scope of the parse is waiting for it.
//
// The order of the two steps is the contract. d is delivered first, with no
// session lock held, so that a sink may read the session it is delivering for
// exactly as it always could. Only then is the stop published, and it is
// published through publishStop rather than assigned, so that a stop the sink
// asked for — which record has already published by the time it returns, and
// which outranks a budget — is what survives. Publishing something is
// unconditional, so a caller waiting on this transition is never woken to find
// no answer.
//
// The channel is closed after the publication and never before, which is what
// makes a woken waiter see the published value rather than race with it. It is
// closed exactly once because exactly one caller holds a claim and the claim is
// withdrawn under the same lock.
//
// Finishing happens in a deferred call, so that a sink which panics — which its
// contract forbids, and which nothing here can prevent — leaves that panic to
// propagate rather than leaving every other scope of the parse waiting for a
// transition that would never finish.
func (s *ParseSession) resolvePart(d Diagnostic) (published error) {
	defer func() {
		s.mu.Lock()
		// Unconditional, so a waiter is never woken to find no answer, and
		// through publishStop, so a stop the sink asked for — which record has
		// already published by the time it returns, and which outranks a budget
		// — is what survives.
		final := s.publishStop(newFatalDiagnostic(d))
		pending := s.partPending
		s.partPending = nil
		s.mu.Unlock()

		if pending != nil {
			close(pending)
		}
		published = final
	}()

	// The result is deliberately not the decision here: a stop the sink asked
	// for is published by record itself, so what publishStop finds above is the
	// answer, and keeping the choice in one place is what keeps the two from
	// disagreeing.
	_ = s.record(d)
	return nil
}
