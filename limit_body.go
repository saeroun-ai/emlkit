package message

import (
	"errors"
	"fmt"
	"io"
	"reflect"
)

// Limits.MaxEncodedBodyBytes, MaxDecodedBodyBytes and MaxTotalDecodedBytes: the
// budgets on body bytes (ROB-001-005), enforced under Parse alone.
//
// # Which bodies
//
// The two per-body budgets bound a leaf body: the body of an entity whose
// MultipartReader returns nil when it is constructed. That is every entity whose
// media type is not multipart/*, a message/* part included, and a multipart
// entity whose damaged Content-Type was repaired with no boundary that may be
// used (see metadata_repair.go). A framed multipart container has a raw body
// too, and it can be read directly or copied verbatim with WriteTo, but that raw
// view is outside the leaf budgets and bounded by MaxInputBytes alone; iterating
// it hands each leaf to its own budgets. That is what keeps one oversized part
// from making its container overflow before the parser can step past it to the
// next one, and what keeps any byte from being charged twice.
//
// # What is counted
//
//   - MaxEncodedBodyBytes counts the wire bytes of one leaf body as they are read
//     out of the input for that body, before any decoding, whether the decoders
//     or WriteTo's verbatim copy read them. Bytes a buffered reader fetched
//     ahead are counted when the body reads them, not when they were fetched.
//   - MaxDecodedBodyBytes counts the bytes Entity.Body of one leaf delivers,
//     after every decoding stage that ran: transfer decoding, then charset
//     conversion, including a CharsetReader a caller installed. They are bytes,
//     not characters, so a body cut by it may end inside a UTF-8 sequence.
//   - MaxTotalDecodedBytes counts the same delivered bytes for every leaf of the
//     parse together. Headers, multipart framing, the bytes an iterator drains
//     past a part, and WriteTo's verbatim copies are not decoded output and are
//     not counted.
//
// # Exactness
//
// A budget N admits N bytes; reaching it is neither an overflow nor an end. Only
// a byte that exists past it is an overflow, and proving that costs reading
// exactly one byte: one wire byte from the input for the encoded budget, one
// output byte from the decoders for the decoded budgets. That byte is never
// handed on — the decoders never see more than N wire bytes and the caller never
// receives more than N decoded ones — and is not counted. A read that brings no
// byte passes on what came with it — io.EOF, an error, or (0, nil) — unchanged,
// a (0, nil) only until the no-progress rule of progress.go ends the body.
//
// # What a failure means
//
// A per-body overflow is local. It records one diagnostic for the leaf, and its
// Body returns that leaf's FatalError from then on, until a total overflow: the
// value the caller already holds stays valid, but a later read returns the
// parse's total stop instead. ParseStop stays nil for a local overflow. The
// header and the bytes already delivered stay the caller's, and the multipart
// iterator above it can drain the unread raw tail, under MaxInputBytes and
// without decoding it, and continue with the next part, unless another limit
// or a DiagnosticSink stops it.
//
// A total overflow belongs to the whole parse: it records one diagnostic for
// the leaf that proved it and publishes the session's stop, after which no leaf
// Body of the parse delivers further decoded output — one that already failed
// locally included — and every scope reports that stop. It does not touch the
// verbatim WriteTo copy, which is not decoded output.
//
// A body reports one local failure, the first proven by any reader of it: the
// encoded limiter, the encoded line monitor, or the decoded guard. A stop of the
// whole parse published before a refusal outranks it and nothing is recorded
// for the refusal. Such an earlier stop — a DiagnosticSink's, or another
// budget's — does not by itself end reading a body already returned.
//
// # Cancellation
//
// A parse started by ParseContext gates every leaf Body and every raw body
// above all of this, and checks its context before and after each read; see
// cancel.go. Once cancellation has been seen, no read of a body — one that
// already failed locally included — delivers anything more, and each reports
// the session's published stop, which is the cancellation unless a stop of the
// parse was published first. Bytes the read that saw it delivered are still
// delivered. A decoder that turned a failure into io.EOF cannot hide it, since
// the gate asks the context rather than the error. Until the context is done,
// nothing here changes, including what a body already returned may do after a
// sink or another budget stopped the parse.
//
// # What decoders cannot hide
//
// A decoder reports the failure of what it reads in its own words, or not at
// all: quoted-printable reports a cut soft line break as invalid bytes, and a
// CharsetReader may turn any error into io.EOF. The guard over the decoders
// therefore lets them end, delivering, within the decoded budgets, the output
// they made of what they were given, and then reports what actually ended the body instead of what they
// ended with: the body's local failure, or else the input's failure of this
// parse — MaxInputBytes, or a stalled input (progress.go) — when that is what
// the body's input returned, and otherwise what the decoders returned. This
// holds whether or not any body budget is enforced.
//
// The input's failure is recognized by where it came from, never by asking an
// error about itself: it is the exact value the input limiter or the
// no-progress reader of this parse fails with — its own stop, or a stop
// published earlier that it adopted — as
// the body's input returned it, directly for the root and joined first by the
// multipart framing of each enclosing part. The guard calls no Is, As or
// Unwrap of an error a caller's source returned and never replaces one, so it
// adds no call into such an error's methods. A stop published for another
// reason is not looked up.
//
// An ordinary failure of the source is not the parser's to restore as a stop,
// but it is not the decoders' to lose either (ROB-010-006). The first error the
// body's input returned that is neither io.EOF nor one of the parse's own
// stops is kept by the same provenance, as the identical value — the source's
// own at the root, the framing's join under a part — and once the decoders end
// it is reported: as itself when they passed it on, or ended with io.EOF as
// though the input had been complete (a CharsetReader that masks it); and
// together with their own error otherwise, in one errors.Join with the input's
// failure first, so that quoted-printable's wording of a cut soft break, or a
// corruption base64 found in bytes before the failure, is kept beside it rather
// than either hiding the other. The two are compared by value alone, a
// comparison that would panic counting as different. What the body delivered
// before is what the decoders made of the bytes they were given; nothing
// claims every wire byte before the failure was decoded. A failure still
// buffered ahead of what the decoders asked for has not reached the body and
// is not pulled out early.
//
// The built-in decoders end once their input fails. A CharsetReader a caller
// installed is only bounded from outside: what it reads is held to the encoded
// budget, what it delivers to the decoded ones, and 100 consecutive reads in
// which it delivers nothing and takes nothing end the body (progress.go). Its
// allocation, its CPU, a loop inside one of its calls and whether it blocks are
// not bounded here.
//
// # Raw bytes
//
// Nothing here replays raw bytes, and this adds no raw API. The wire bytes a
// body consumed, its proof byte, what decoders read ahead — including a
// CharsetReader that reads eagerly while the entity is built — and the tail a
// parent drained are gone. The existing verbatim WriteTo path stays as it was,
// a conditional one-shot copy: it applies only before Body is read and only
// when the output encoding and charset match, and the writer still refuses a
// Content-Type it cannot interpret before writing anything (ROB-120-008).
// Replay is #41.
//
// No session lock is held while a body's source or a DiagnosticSink is called.
// A body is read by one goroutine at a time, as the stream of a parse is;
// reading the stream from a second goroutine or from inside a DiagnosticSink is
// not supported, and nothing is promised about what it does. A sink may still
// inspect the session.

const (
	encodedBodyAction  = "admitted the wire bytes of this body within the limit, read one wire byte past it only to prove the overflow and no further wire byte of this body, and stopped this body; the header and the bytes already delivered stay valid, and a parent multipart can continue with the next part unless another limit or the diagnostic sink stops it"
	decodedBodyAction  = "delivered at most the decoded bytes within the limit, which may end inside a multi-byte character, read one byte past it only to prove the overflow, and stopped this body; the header and the bytes already delivered stay valid, and a parent multipart can continue with the next part unless another limit or the diagnostic sink stops it"
	totalDecodedAction = "delivered the decoded bytes within the limit, read one byte past it only to prove the overflow, and ended the parse: no body of it delivers further decoded output; verbatim copies are not decoded output, nothing is replayed, and everything already returned stays valid"
)

// A bodySlot is what ended one leaf body, shared by every reader of it.
//
// err is the body's one local failure. The encoded limiter, the encoded line
// monitor and the decoded guard can each refuse the body; the first refusal
// fills it and records its diagnostic, and a later one adopts it and records
// nothing.
//
// src is the input's failure of this parse — a MaxInputBytes overflow or a
// stalled input (progress.go) — as the body's input returned it, recognized by
// provenance (see rawBody.note), so that the guard can report it whatever a
// decoder made of it.
//
// ioErr is the first other error the body's input returned, other than
// io.EOF and the session's published stop: an ordinary failure of the caller's
// source as it reached this body — the identical value at the root, the
// framing's join under a part — kept by provenance alone so that the guard can
// report it however a decoder ended (ROB-010-006; see bodyGuard.end).
//
// It is read and written by the one goroutine reading the body, as the stream
// is, and holds no lock.
type bodySlot struct {
	err   error
	src   error
	ioErr error
}

// inputEnd is what a body whose input failed with ioErr reports when its
// decoders ended with decErr: ioErr itself when the decoders passed it on or
// ended as though the input had ended cleanly, and otherwise both, the input's
// failure first, because the decoders' report is then a finding of its own —
// a corruption, or their wording of a cut — that the failure does not make
// untrue. Nothing of either error is asked: the two are compared by sameError
// only. A decoder that wrapped ioErr in an error of its own is not recognized
// as passing it on, so the join then holds ioErr twice, once inside the
// decoder's error.
func (s *bodySlot) inputEnd(decErr error) error {
	if decErr == io.EOF || sameError(decErr, s.ioErr) {
		return s.ioErr
	}
	return errors.Join(s.ioErr, decErr)
}

// sameError reports whether a and b are the same error value. It calls no
// method of either. Comparing two interface values of one dynamic type that is
// not comparable panics; that is recovered and read as different, as
// sameReader does.
func sameError(a, b error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// refuseBody fills slot with the failure d describes, unless it is already
// filled, and returns what the slot holds.
//
// A stop of the whole parse published before this refusal outranks it: the
// slot takes that stop and nothing is recorded, because the parse it would
// describe has already ended. The stop is read without waiting for a
// MaxPartCount transition, which can only be in progress on the goroutine
// reading the stream — the one this runs on. Otherwise d is recorded, and a
// sink that refuses it makes its stop the slot's.
func (c parseContext) refuseBody(slot *bodySlot, d Diagnostic) error {
	if slot.err != nil {
		return slot.err
	}
	if prior := c.session.publishedStop(); prior != nil {
		slot.err = prior
		return prior
	}
	d = c.completeBody(d)
	if fatal := c.recordBody(d); fatal != nil {
		slot.err = fatal
		return fatal
	}
	slot.err = newFatalDiagnostic(d)
	return slot.err
}

// publishedStop returns the session's published stop, or nil, without waiting
// for a transition in progress. A nil session has none.
func (s *ParseSession) publishedStop() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop == nil {
		return nil
	}
	return s.stop
}

// A leafBody is everything that reads one leaf body under Parse, allocated
// together: the shared slot, the raw reader at the bottom and the guard at the
// top.
type leafBody struct {
	slot  bodySlot
	raw   rawBody
	guard bodyGuard

	// charsetIn is what the charset stage reads, counting what it takes; see
	// bodyGuard.progress.
	charsetIn countedInput
}

// countCharsetInput returns r counted as the charset stage's input, or r
// itself for no leafBody. Only the count is added: every result is passed on
// unchanged, and the counter is a plain io.Reader.
func (lb *leafBody) countCharsetInput(r io.Reader) io.Reader {
	if lb == nil {
		return r
	}
	lb.charsetIn.r = r
	return &lb.charsetIn
}

// shared returns the slot of lb, or nil for no leafBody.
func (lb *leafBody) shared() *bodySlot {
	if lb == nil {
		return nil
	}
	return &lb.slot
}

// limitBody puts the raw reader of a leaf under body and returns it, together
// with the leafBody the guard is later taken from. A parse that budgets
// nothing — Read, ReadWithOptions and New — gets body unchanged and nil.
func (c parseContext) limitBody(body io.Reader) (io.Reader, *leafBody) {
	l, ok := c.limits()
	if !ok {
		return body, nil
	}
	lb := &leafBody{}
	lb.raw = rawBody{r: body, ctx: c, slot: &lb.slot, limit: l.MaxEncodedBodyBytes}
	return &lb.raw, lb
}

// guardBody puts the guard over the decoders of a leaf that limitBody prepared.
// A nil leafBody means no parse, and body is returned unchanged.
func (c parseContext) guardBody(body io.Reader, lb *leafBody) io.Reader {
	if lb == nil {
		return body
	}
	l, _ := c.limits()
	lb.guard = bodyGuard{
		r:         body,
		raw:       &lb.raw,
		charsetIn: &lb.charsetIn,
		ctx:       c,
		slot:      &lb.slot,
		decoded:   l.MaxDecodedBodyBytes,
		total:     l.MaxTotalDecodedBytes != LimitDisabled,
	}
	return &lb.guard
}

// A rawBody reads the wire bytes of one leaf body for the decoders, or for
// WriteTo's verbatim copy. It passes at most limit of them on, turns a proven
// byte past them into the body's failure, and notes the input budget's failure
// when that is what its input returns. With MaxEncodedBodyBytes disabled it only
// notes.
//
// Like inputLimiter it implements io.Reader alone, so no copying shortcut reads
// around it, and once it has failed it never reads its source again.
type rawBody struct {
	r     io.Reader
	ctx   parseContext
	slot  *bodySlot
	limit int64 // MaxEncodedBodyBytes: a positive count, or LimitDisabled

	// admitted is how many wire bytes were handed on, never more than limit
	// while one is enforced. The body guard reads it to tell decoders that
	// took input from decoders that made no progress.
	admitted int64
	failed   error
	proof    [1]byte
}

// Read implements io.Reader.
func (l *rawBody) Read(p []byte) (int, error) {
	if l.failed != nil {
		return 0, l.failed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if l.limit == LimitDisabled {
		n, err := l.r.Read(p)
		if n > 0 {
			// Counted only to tell the guard the decoders took input.
			l.admitted += int64(min(n, len(p)))
		}
		l.note(err)
		return n, err
	}
	// admitted never exceeds limit, so this cannot overflow.
	if remaining := l.limit - l.admitted; remaining > 0 {
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		n, err := l.r.Read(p)
		if n > 0 {
			l.admitted += int64(min(n, len(p)))
		}
		l.note(err)
		return n, err
	}
	n, err := l.r.Read(l.proof[:])
	if n <= 0 {
		l.note(err)
		return 0, err
	}
	l.failed = l.ctx.refuseBody(l.slot, Diagnostic{
		Code:     CodeLimitEncodedBodyBytes,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("body exceeds the limit of %d encoded bytes", l.limit),
		Action:   encodedBodyAction,
		// The first wire byte of the body that was not admitted.
		Location: l.ctx.bodyLocationAt(l.limit),
	})
	return 0, l.failed
}

// note keeps err when it is the input budget's own failure as it reached this
// body, so that the guard can report it whatever a decoder made of it.
//
// That is decided by provenance, never by asking err about itself: no Is, As
// or Unwrap of err is called, since err may be any error the caller's source
// returned, with methods that panic or never return. Nothing is noted unless
// the input of this parse has failed — its limiter or its no-progress reader —
// and then err must be the exact value it fails with (inputStop), or that
// value as the multipart framing of this body's ancestors reported it — at most one errors.Join per level of
// depth, each with the failure first, which is how textproto reports a read
// error under a part. A join is recognized by its dynamic type before its
// Unwrap is called, so only the standard library's own method runs, and only
// the first element of each is compared. Comparing an error with inputStop,
// whose dynamic type is a pointer, cannot panic.
//
// Any other error but io.EOF is the input's ordinary failure as it reached
// this body, and the first one is kept as ioErr, unless it is the session's
// published stop — a cancellation seen under the body, say — which is the
// parse's own and not the source's. That too is decided by comparing with a
// pointer of this parse.
func (l *rawBody) note(err error) {
	if err == nil || err == io.EOF || l.slot.src != nil {
		return
	}
	if l.isInputStop(err) {
		l.slot.src = err
		return
	}
	if l.slot.ioErr == nil {
		if stop := l.ctx.session.publishedStop(); stop == nil || err != stop {
			l.slot.ioErr = err
		}
	}
}

// isInputStop reports whether err is the input's failure of this parse as it
// reached this body; see note.
func (l *rawBody) isInputStop(err error) bool {
	stop := l.ctx.session.inputFailure()
	if stop == nil {
		return false
	}
	for e, level := err, int64(0); ; level++ {
		if e == stop {
			return true
		}
		if level >= l.ctx.depth || reflect.TypeOf(e) != joinErrorType {
			return false
		}
		errs := e.(interface{ Unwrap() []error }).Unwrap()
		if len(errs) == 0 {
			return false
		}
		e = errs[0]
	}
}

// joinErrorType is the dynamic type of what errors.Join returns, which is how
// the multipart framing wraps a failed read under a part.
var joinErrorType = reflect.TypeOf(errors.Join(io.EOF))

// inputFailure returns the value the input of this parse fails every read with
// — a MaxInputBytes overflow or a stall — or nil while it has not failed. A nil
// session has none.
func (s *ParseSession) inputFailure() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inputStop
}

// A bodyGuard is what Entity.Body of a leaf is under Parse: it delivers the
// output of the decoders within MaxDecodedBodyBytes and the shared
// MaxTotalDecodedBytes, and reports what ended the body whatever the decoders
// made of it.
//
// It does not cut a body short when a reader below it fails. A decoder may hold
// output it produced from wire bytes it was given when the next read fails — it
// reads ahead — and that output is the caller's. So the guard keeps reading
// until the decoders end, and only then replaces whatever they ended with by
// what ended the body; see end.
//
// It implements io.Reader alone.
type bodyGuard struct {
	r    io.Reader
	ctx  parseContext
	slot *bodySlot

	// raw is the body's wire reader and charsetIn the charset stage's input;
	// what they count tells decoders that took input from decoders that made
	// no progress, and empty counts the consecutive reads that did neither.
	// See progress and noProgress in progress.go.
	raw       *rawBody
	charsetIn *countedInput
	empty     int

	// decoded is MaxDecodedBodyBytes, used the bytes delivered so far.
	decoded int64
	used    int64

	// total reports whether MaxTotalDecodedBytes is enforced; the usage is the
	// session's.
	total bool

	// err is the sticky failure this guard returned: what ended the body, or
	// the session's total stop. Errors of the decoders that end nothing are
	// passed on as they are.
	err   error
	proof [1]byte
}

// Read implements io.Reader.
func (g *bodyGuard) Read(p []byte) (int, error) {
	// The total stop comes first, even for a body that already failed: it ends
	// all decoded output of the parse. It is the only stop of the whole parse
	// this guard looks up, because it is the only one that bounds decoded
	// output; a body already returned when a sink or another budget stopped the
	// parse may still be read, as it always could.
	totalLeft := int64(-1) // -1: not bounded by the total
	if g.total {
		var stop error
		stop, totalLeft = g.ctx.session.decodedState()
		if stop != nil {
			g.err = stop
			return 0, stop
		}
	}
	if g.err != nil {
		return 0, g.err
	}
	if len(p) == 0 {
		return 0, nil
	}

	localLeft := int64(-1)
	if g.decoded != LimitDisabled {
		localLeft = g.decoded - g.used
	}

	window := int64(len(p))
	if localLeft >= 0 && localLeft < window {
		window = localLeft
	}
	if totalLeft >= 0 && totalLeft < window {
		window = totalLeft
	}

	before := g.progress()
	if window > 0 {
		q := p[:window]
		n, err := g.r.Read(q)
		if n > 0 {
			n = min(n, len(q))
			g.used += int64(n)
			if g.total {
				g.ctx.session.chargeDecoded(int64(n))
			}
			g.empty = 0
		}
		if err != nil {
			return n, g.end(err)
		}
		if n == 0 {
			if stall := g.noProgress(before); stall != nil {
				return 0, stall
			}
		}
		return n, nil
	}

	// No room is left, so only a byte that exists proves an overflow.
	n, err := g.r.Read(g.proof[:])
	if n <= 0 {
		if err != nil {
			return 0, g.end(err)
		}
		if n == 0 {
			if stall := g.noProgress(before); stall != nil {
				return 0, stall
			}
		}
		return 0, nil
	}
	if totalLeft == 0 {
		// The same byte exceeds the total, whatever it does to this body's own
		// budget, and the total ends the whole parse.
		g.err = g.ctx.session.decodedOverflow(g.ctx)
		return 0, g.err
	}
	g.err = g.ctx.refuseBody(g.slot, Diagnostic{
		Code:     CodeLimitDecodedBodyBytes,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("body exceeds the limit of %d decoded bytes", g.decoded),
		Action:   decodedBodyAction,
	})
	return 0, g.err
}

// end returns what the guard reports for the error its decoders ended a read
// with, io.EOF included: the body's local failure when there is one, else a
// MaxInputBytes failure as the body's input returned it, each sticky from then
// on, and err itself otherwise.
//
// After those two, an ordinary failure of the body's input (ioErr) is
// reported however the decoders ended — as itself when they passed it on or
// ended as though the input had ended cleanly, joined with their own report
// otherwise — and is sticky like them. See bodySlot.inputEnd.
func (g *bodyGuard) end(err error) error {
	switch {
	case g.slot.err != nil:
		g.err = g.slot.err
	case g.slot.src != nil:
		g.err = g.slot.src
	case g.slot.ioErr != nil:
		g.err = g.slot.inputEnd(err)
	default:
		return err
	}
	return g.err
}

// decodedState returns the stop of a total overflow, if there is one, and how
// much of MaxTotalDecodedBytes is left.
func (s *ParseSession) decodedState() (error, int64) {
	limit := s.Limits().MaxTotalDecodedBytes
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decodedStop != nil {
		return s.decodedStop, 0
	}
	return nil, limit - s.decodedUsed
}

// chargeDecoded adds n delivered bytes to the total. The guard never delivers
// more than what is left, so the usage stays within the limit.
func (s *ParseSession) chargeDecoded(n int64) {
	limit := s.Limits().MaxTotalDecodedBytes
	s.mu.Lock()
	s.decodedUsed += min(n, limit-s.decodedUsed)
	s.mu.Unlock()
}

// decodedOverflow turns a proven byte past MaxTotalDecodedBytes into the
// session's stop, records its one diagnostic against the leaf c describes, and
// returns the stop.
//
// It follows inputOverflow: a stop published before it outranks it and nothing
// is recorded; otherwise the diagnostic is delivered with no lock held and the
// budget's stop is published after, so that a sink refusing it wins. Either way
// the stop becomes the total's own, which every leaf of the parse reports from
// then on instead of delivering more.
func (s *ParseSession) decodedOverflow(c parseContext) error {
	s.mu.Lock()
	if s.decodedStop != nil {
		stop := s.decodedStop
		s.mu.Unlock()
		return stop
	}
	prior := s.stop
	s.mu.Unlock()

	if prior == nil {
		limit := s.Limits().MaxTotalDecodedBytes
		d := c.completeBody(Diagnostic{
			Code:     CodeLimitTotalDecodedBytes,
			Severity: SeverityLimitExceeded,
			Message:  fmt.Sprintf("message exceeds the limit of %d decoded bytes", limit),
			Action:   totalDecodedAction,
		})
		_ = c.recordBody(d)
		prior = newFatalDiagnostic(d)
	}

	s.mu.Lock()
	stop := s.publishStop(prior)
	s.decodedStop = stop
	s.mu.Unlock()
	return stop
}
