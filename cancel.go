package message

import (
	"context"
	"io"
)

// Cancellation of a parse by its context (ROB-001-006, issue #6).
//
// # Which parses
//
// Only a parse started by ParseContext with a context that can be cancelled. A
// nil context, and one whose Done method returns nil — context.Background,
// context.TODO, a context.WithoutCancel — cancel nothing, and such a parse
// installs nothing described here: it behaves exactly as Parse, which is
// ParseContext with no context. Read, ReadWithOptions, New and NewMultipart have
// no context and are unchanged.
//
// The context is retained by the session for the lazy life of the parse, not
// only for the ParseContext call: the body and every part of a message are read
// long after ParseContext returned, and each of those reads belongs to the same
// parse. Done is called once, by ParseContext. Err, and context.Cause (which
// calls Value), are called once cancellation has been seen, to build the stop;
// no lock of the session is held while any Context method runs.
//
// # What cancellation is
//
// The caller ending the parse, like a DiagnosticSink that stops it. It is not a
// property of the message, so it records no diagnostic, and the FatalError it
// ends the parse with carries none: Diagnostic reports false. Its cause is
// ctx.Err() and, when the context carries a different cause,
// context.Cause(ctx) as well, so errors.Is finds context.Canceled or
// context.DeadlineExceeded, and finds a custom cause of a comparable type, as
// it would anywhere. The two are never compared to each other to drop one as a
// duplicate; only the standard sentinels, whose types are comparable, are
// compared.
//
// The stop is published like every other session-wide stop, through
// publishStop: the first stop published is the one every scope of the parse
// reports for ever. A sink stop or a budget stop published before the
// cancellation was seen stays the identity; cancellation then only adds that
// nothing more is read.
//
// # Where it is seen
//
// Cancellation cannot interrupt anything. It is looked for, without blocking,
// at these points, and only there:
//
//   - before and after every read of the input: once seen, the input is never
//     read again;
//   - before and after every read of the Body of every entity of the parse —
//     leaf bodies and framed multipart containers alike — and of the raw body
//     WriteTo copies verbatim, so that bytes buffered or decoded ahead are not
//     delivered once it has been seen;
//   - at every boundary a scope already checks the session's stop at: Parse
//     itself, the construction of every entity, MultipartReader.NextPart,
//     Entity.Walk, Entity.ParseStop and so mail.Reader, the start of WriteTo,
//     and the delivery of every diagnostic — before waiting for a delivery
//     already in progress, and again once that wait is over;
//   - between the stages of decoding a parameterized header field — the budget
//     scan, with at most 4 KiB of the field crossed between two looks and one
//     at every parameter item, the whole-field parse, each parameter value
//     decoded and the last, and each item of a repair and the last — and
//     after a CharsetReader constructing a decoder returns.
//
// A header block, the framing between parts and the body of a part are read
// from buffers of at most 4 KiB — the reader of the message and the reader of
// each multipart — and every refill of one is a read of the gated input, of
// the enclosing container's body as its own framing reads it, which reaches
// the input only through that same gate, or of a replaced Body through the
// gate MultipartReader puts on it. A line or field longer than the buffer is
// accumulated across refills, so a single header field, even under disabled
// header budgets, is crossed at most one buffer between two points where
// cancellation is seen. What is done with a block once it has been read —
// splitting it into fields — is one pass over bytes the budgets bound. No
// textproto API is needed for this.
//
// The framing of a multipart is not a caller's read, and is not gated by the
// container's own Body gate. Once cancellation has been seen nothing more is
// read from the input, so framing can only work through bytes the parse
// already admitted, and only inside a read a caller had already started,
// since every later read of a Body and every NextPart report the stop first.
// What a container's reader can hold is more than its 4 KiB buffer: a
// delimiter candidate whose white space runs past the buffer is held up to
// Limits.MaxLineBytes plus one buffer (one buffer with that budget disabled),
// and is delivered as body if it turns out not to be one, and a framing line
// is accumulated to the same bound; every enclosing container, Limits.MaxMIMEDepth
// of them at most, holds its own. That is the work framing can still do in
// that read; it is not bounded by 4 KiB per container. That is what lets the framing see
// an enclosing scope's end the parse has already proven: the line ending after
// an inner closing delimiter is the outer delimiter's, so the inner body ends
// only where the outer part ends, and a body the cancellation cut ends exactly
// where the same body ends when an ordinary failure of the input cuts it at
// the same point. See Entity.framing.
//
// A read already in progress when the context is cancelled is never preempted:
// a source blocked in Read, a CharsetReader or decoder, a DiagnosticSink, a
// WalkFunc, an Unwrap method, a caller's Writer and the caller's own Context
// methods all run until they return, and the parse waits for them. One call
// into the standard library — mime.ParseMediaType of a whole field, an
// io.Copy chunk — is not split either; its size is bounded by the budgets, not
// by cancellation.
//
// # What a read returns
//
// Before a read, cancellation that has been seen returns (0, stop) without
// touching the reader below, an empty read included. After a read returns n
// bytes, cancellation seen at that point returns (n, stop): the n bytes are
// valid, are delivered and were counted by every budget as usual, and the error
// that came with them — io.EOF or the source's own — is replaced by the stop.
// While the context is live, everything a reader returns is passed on exactly as
// before. An io.EOF already returned is not revoked by a later cancellation,
// but any later read reports the stop: nothing tracks whether a body, or the
// message, was complete (ROB-010 and #10 do).
//
// # What it does not do
//
// It never closes the input, changes its deadline, or starts a goroutine or a
// context.AfterFunc to watch the context: a source blocked in Read stays
// blocked until the caller makes it return, for example by closing what it
// owns from a context.AfterFunc of its own. A Body the caller replaced is the
// caller's reader and is not gated as a field: reading it directly reads the
// caller's reader. The parse's own reads of it are: MultipartReader frames a
// replaced Body through the cancellation gate, so once the parse is cancelled
// neither that framing nor the framing of any part inside it reads the
// caller's reader again. No input budget applies to such a reader.
//
// # Compatibility
//
// Nothing changes for a parse whose context is never cancelled, and that
// includes what a body already returned may still do after a DiagnosticSink or
// a budget stopped the parse: it may still be read, as before, until the
// context is cancelled. Parse, ParseOptions, Read, ReadWithOptions and New keep
// their signatures and behavior.
//
// One behavior outside ParseContext changed with this item: the Close of a body
// NewMultipart built is now safe to call from another goroutine while the body
// is read, ends the iteration over its parts, and makes a body closed before
// its first Read stay closed rather than start writing; see NewMultipart.

// parseCancelledWhat names the situation in the stop's message.
const parseCancelledWhat = "parse cancelled"

// A cancelCause is the cause of the stop of a cancelled parse: ctx.Err()
// first, and the context's own cause second when it may differ.
type cancelCause struct {
	errs []error
}

// newCancelCause returns the cause of a stop for a context that ended with err
// and whose context.Cause is cause.
//
// The two are compared only when err is one of the standard sentinels, whose
// dynamic types are comparable, so the comparison cannot panic whatever cause
// is; any other pair is kept as two, even when they might be equal, rather than
// compared.
func newCancelCause(err, cause error) *cancelCause {
	c := &cancelCause{errs: []error{err}}
	if cause == nil {
		return c
	}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		if err == sentinel && cause == sentinel {
			return c
		}
	}
	c.errs = append(c.errs, cause)
	return c
}

func (c *cancelCause) Error() string {
	s := c.errs[0].Error()
	if len(c.errs) > 1 {
		s += ": " + c.errs[1].Error()
	}
	return s
}

// Unwrap returns a copy of the causes, so a caller cannot rearrange them.
func (c *cancelCause) Unwrap() []error {
	return append([]error(nil), c.errs...)
}

// newCancelStop builds the stop of a parse whose context ctx is done. It calls
// ctx.Err and context.Cause, so it must be called with no session lock held.
//
// A Context whose Done channel is closed but whose Err is nil breaks the
// Context contract; it is read as context.Canceled rather than as no error.
func newCancelStop(ctx context.Context) FatalError {
	err := ctx.Err()
	if err == nil {
		err = context.Canceled
	}
	return newFatalError(parseCancelledWhat, newCancelCause(err, context.Cause(ctx)))
}

// setContext makes ctx this session's context, before the session is shared
// with anything. A nil context, and one that can never be cancelled, leave the
// session without one.
func (s *ParseSession) setContext(ctx context.Context) {
	if ctx == nil {
		return
	}
	done := ctx.Done()
	if done == nil {
		return
	}
	s.cancelCtx, s.cancelDone = ctx, done
}

// cancellable reports whether this session has a context that can be
// cancelled. It is fixed once the session is constructed.
func (s *ParseSession) cancellable() bool {
	return s != nil && s.cancelDone != nil
}

// cancelSignalled reports whether this session's context is done. It only
// receives from the channel Done returned, without blocking, and calls no
// method of the context, so it is safe under any lock.
func (s *ParseSession) cancelSignalled() bool {
	if !s.cancellable() {
		return false
	}
	select {
	case <-s.cancelDone:
		return true
	default:
		return false
	}
}

// cancelStop returns the session's published stop once this session's context
// is done, publishing the cancellation as that stop if nothing was published
// before it, and nil while the context is live or for a session with none.
//
// It must be called with no session lock held: building the stop calls the
// context's Err and Value methods. Two callers may both build one; publishStop
// keeps the first, and both return it.
func (s *ParseSession) cancelStop() FatalError {
	if !s.cancelSignalled() {
		return nil
	}
	s.mu.Lock()
	published := s.stop
	s.mu.Unlock()
	if published != nil {
		return published
	}

	f := newCancelStop(s.cancelCtx)

	s.mu.Lock()
	published = s.publishStop(f)
	s.mu.Unlock()
	return published
}

// cancelErr is cancelStop as an error, nil when there is no stop to report.
func (s *ParseSession) cancelErr() error {
	if stop := s.cancelStop(); stop != nil {
		return stop
	}
	return nil
}

// cancelPoll returns the poll a long scan of this parse calls at its work
// boundaries, or nil for a parse that cannot be cancelled. The poll calls no
// Context method, so a scan may call it while holding an entity's lock.
func (c parseContext) cancelPoll() func() bool {
	if !c.session.cancellable() {
		return nil
	}
	return c.session.cancelSignalled
}

// gateReader returns r gated on this session's context, or r itself for a
// session that cannot be cancelled and for a nil reader.
func (s *ParseSession) gateReader(r io.Reader) io.Reader {
	if r == nil || !s.cancellable() {
		return r
	}
	return &cancelReader{r: r, session: s}
}

// A cancelReader checks for cancellation before and after every read of r; see
// "What a read returns" above.
//
// It implements io.Reader and nothing else, so an io.Copy cannot read around
// it through a WriterTo of r, or of a bufio.Reader holding prefetched bytes.
type cancelReader struct {
	r       io.Reader
	session *ParseSession
}

// Read implements io.Reader.
func (c *cancelReader) Read(p []byte) (int, error) {
	if stop := c.session.cancelStop(); stop != nil {
		return 0, stop
	}
	// While the context is live every read, an empty one included, is passed
	// on exactly as it was asked for, so that a parse whose context is never
	// cancelled reads exactly as Parse does. The readers below never pass an
	// empty read on to the input: the input budget and the no-progress reader
	// (progress.go) answer one themselves, and the buffered readers do not
	// issue one.
	n, err := c.r.Read(p)
	if stop := c.session.cancelStop(); stop != nil {
		return n, stop
	}
	return n, err
}
