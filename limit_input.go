package message

import (
	"fmt"
	"io"
)

// Limits.MaxInputBytes: one budget on the bytes a parse reads from its input.
//
// # Where it is counted
//
// Once, at the raw source, and nowhere else. Every byte any part of a parse
// reads — the message's header block, its body, the framing and headers of
// every part at every depth, the bytes a multipart iterator skips or drains past
// a part the caller did not read — comes out of the one reader Parse wraps
// around its input, so a budget there is shared by all of them without any of
// them having to know it exists, and nothing is counted twice. It sits under the
// reader that counts offsets, so a byte the budget refuses is never a byte any
// position is measured against.
//
// # When it is spent, and what proves an overflow
//
// A positive budget N admits N bytes. Reaching N is not an overflow and is not
// an end of input either: a message of exactly N bytes is read whole, and the
// io.EOF after it is the source's own. Only an actual byte after the N-th is an
// overflow, so once N bytes have been admitted the next read asks the source for
// a single byte and no more. If one arrives — whatever error comes with it —
// the message exceeded the budget, and that byte is discarded: it is never
// handed to a parser, a decoder or a caller. If none arrives, what the source
// said is passed on unchanged, so an io.EOF stays the end of a message of
// exactly N bytes, an I/O error stays that I/O error, and a read that returned
// (0, nil) returns (0, nil) rather than being retried here. Repeated (0, nil)
// reads are judged under this limiter, at the source, proof reads included,
// and so is the source's first error, which is final from then on: no read
// after it reaches the source; see progress.go.
//
// Reads before the budget is reached are only shortened to what is left of it,
// so nothing is read past N to learn that N was reached, and the bytes before N
// arrive however the source chunks them. An empty read asks the source for
// nothing.
//
// The budget is spent as raw bytes are read from the source, which is not the
// same as how far the parse has interpreted them. Parse interprets the header
// block and nothing more, and the body and every part are interpreted as the
// caller consumes them, but the buffered readers above the source read ahead:
// the call that fetched the end of the header may already have fetched body
// bytes, up to the budget. Those prefetched bytes count, because they were read
// from the input. Prefetching up to exactly N is not an overflow and costs no
// proof read; the proof byte is asked for only when something needs a byte
// beyond the N already held. A message whose tail is never consumed — a body
// never read, the epilogue after the last part a caller iterated to — is
// checked only as far as the bytes actually read, so a parse that has returned
// without a failure has not certified the size of anything it did not read, and
// the size of the whole message beyond that stays unknown. The input after the
// message that a caller reads through ParseSession.TrailingInputReader, under
// ProfileForensic, is read from the same input and counted the same way: what
// a buffer had already fetched of it was counted then, and the rest is counted
// as the caller reads it and not checked before (trailing.go).
//
// # What a failure means
//
// An overflow is a resource boundary, not a defect of the message's syntax, and
// it belongs to the whole parse: the message is one input, and there is nothing
// left to read in any scope of it. It records exactly one limit.input_bytes
// diagnostic at SeverityLimitExceeded, with the position of the first byte that
// was not admitted, which is offset N. Its path is the root's nil path, which
// names the message as a whole: the reader at the source cannot know which
// entity's bytes it was about to deliver, and does not guess at a part. The
// failure is published as the session's stop, so the read that overflowed, every
// later read, and every scope of the parse — Parse itself while it is still
// reading the header, MultipartReader.NextPart, Entity.Walk, mail.Reader and
// Entity.ParseStop — report that one FatalError. Nothing more is read from the
// source, however often the caller asks. The diagnostic is recorded through the
// session like any other, so Limits.MaxDiagnosticCount may replace it with the
// terminal limit.diagnostic_count diagnostic or omit it; the stop is published
// regardless, and its FatalError carries the limit.input_bytes diagnostic
// either way.
//
// Everything already returned stays valid: a root header that was complete
// within the budget, the parts already yielded, and every byte a body already
// produced. A header block the budget cut short yields no header, as a header
// budget's refusal does, and nothing is claimed about bytes that were never read.
//
// A DiagnosticSink that stops the parse while it is being told about the
// overflow outranks it, and so does a stop the session had already published
// before the overflow; neither is replaced, and an overflow observed after a
// published stop records nothing, because the parse it would describe has
// already ended.
//
// Read and ReadWithOptions allocate no session and have no total input budget.
// Their one long-standing cap, ReadOptions.MaxHeaderBytes on the message's own
// header block, still applies as it always has.

// inputBytesAction is what a limit.input_bytes diagnostic reports was done: no
// byte past the budget was delivered, exactly one was read to prove it exists,
// nothing further is read from the input, and nothing was repaired.
const inputBytesAction = "delivered no byte past the limit, read one byte past it only to prove the overflow, and read nothing further from the input; everything already returned stays valid"

// limitInput returns r bounded by this parse's Limits.MaxInputBytes, or r itself
// for a parse that budgets nothing — a nil session, which is what Read and
// ReadWithOptions pass — and for a disabled budget.
func (s *ParseSession) limitInput(r io.Reader) io.Reader {
	l, ok := s.enforcedLimits()
	if !ok || l.MaxInputBytes == LimitDisabled {
		return r
	}
	return &inputLimiter{r: r, session: s, limit: l.MaxInputBytes}
}

// An inputLimiter passes through at most limit bytes of its source, and turns a
// proven byte after them into the session's stop. See the note above for what
// it counts and why.
//
// It implements io.Reader and nothing else. Nothing reads around it: the
// readers above it are plain readers, and a bufio.Reader or io.Copy that looks
// for io.WriterTo or io.ReaderFrom finds neither here, so the budget cannot be
// bypassed by a faster copying path.
//
// It is read by one goroutine at a time, as the stream of a parse is, and holds
// no lock of its own. The session is told about an overflow exactly once:
// failed is assigned when inputOverflow returns, after the diagnostic has been
// delivered, and is checked first by every later read and never cleared. That
// relies on the stream not being read again while the overflow is being
// delivered — reading it from inside a DiagnosticSink callback, or from a second
// goroutine, is not supported, as it is not for any other read of the stream. A
// sink may inspect the session, which takes no part in this.
type inputLimiter struct {
	r       io.Reader
	session *ParseSession

	// limit is the budget: a positive count of bytes, math.MaxInt64 included.
	limit int64

	// admitted is how many bytes have been handed on, never more than limit.
	admitted int64

	// failed is the sticky error every read returns once the input has been
	// shown to exceed the budget: the session's published stop.
	failed error

	// proof receives the one byte read to learn whether the input goes on past
	// the budget. It is a field so that a read at the boundary allocates
	// nothing, and what lands in it is never handed on.
	proof [1]byte
}

// Read implements io.Reader.
func (l *inputLimiter) Read(p []byte) (int, error) {
	if l.failed != nil {
		return 0, l.failed
	}
	if len(p) == 0 {
		// Nothing was asked for, so nothing is read: neither the source's bytes
		// nor a proof byte.
		return 0, nil
	}

	// admitted never exceeds limit, so this cannot overflow, at math.MaxInt64
	// included.
	if remaining := l.limit - l.admitted; remaining > 0 {
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		n, err := l.r.Read(p)
		if n > 0 {
			// A source reporting more than it was given room for breaks the
			// io.Reader contract; it is counted as no more than the room, so
			// that admitted stays within limit.
			l.admitted += int64(min(n, len(p)))
		}
		return n, err
	}

	// The budget is spent. Only a byte that actually exists past it proves an
	// overflow, so exactly one is asked for.
	n, err := l.r.Read(l.proof[:])
	if n <= 0 {
		// None: an io.EOF is the end of a message of exactly limit bytes, any
		// other error is the source's own failure, and (0, nil) is a read that
		// made no progress. Each is passed on as it is.
		return 0, err
	}
	// One did, so the input exceeds the budget whatever err says. The byte is
	// dropped here and never handed on.
	l.failed = l.session.inputOverflow(l.limit)
	// The value every later read returns is noted, so that a leaf body can
	// recognize it by identity when a decoder ends with something else; see
	// rawBody.note.
	l.session.mu.Lock()
	if l.session.inputStop == nil {
		l.session.inputStop = l.failed
	}
	l.session.mu.Unlock()
	return 0, l.failed
}

// inputOverflow records the one limit.input_bytes diagnostic of a message longer
// than limit bytes, and publishes the session's stop, which it returns.
//
// A stop already published outranks it, and is returned with nothing recorded:
// the parse it would describe has already ended. Otherwise the diagnostic is
// delivered first, with no session lock held, so that a sink may read the
// session it is delivering for as it always could, and the budget's failure is
// published after. A sink that refused the diagnostic has by then published its
// own stop through record, and publishStop keeps it. What record returns is not
// consulted, and the stop is published unconditionally, so a diagnostic that
// record omitted under Limits.MaxDiagnosticCount still ends the parse.
//
// The published stop is read rather than waited for. The only transition that
// waits is a MaxPartCount overflow being delivered to a sink, which runs on the
// goroutine reading the stream — the one this is called on — so waiting here
// could only wait for itself.
func (s *ParseSession) inputOverflow(limit int64) error {
	s.mu.Lock()
	prior := s.stop
	s.mu.Unlock()
	if prior != nil {
		return prior
	}

	d := Diagnostic{
		Code:     CodeLimitInputBytes,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("message exceeds the limit of %d input bytes", limit),
		Action:   inputBytesAction,
		// The first byte that was not admitted. The bytes are counted from the
		// first byte of the input, so the N-th byte admitted is at offset N-1
		// and the one refused is at offset N.
		Location: LocationAt(limit),
	}
	_ = s.record(d)

	s.mu.Lock()
	published := s.publishStop(newFatalDiagnostic(d))
	s.mu.Unlock()
	return published
}
