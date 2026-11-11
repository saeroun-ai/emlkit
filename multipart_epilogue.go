package message

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"slices"
)

// The epilogue of a multipart under Parse (ROB-060-001).
//
// RFC 2046 calls the bytes after a multipart's closing delimiter line the
// epilogue, and says it is to be ignored. NextPart, Walk and mail.Reader end at
// that line and do not read on; MultipartEpilogue reads on when asked, on an
// iterator that heals, and keeps at most Limits.MaxEpilogueBytes of it. A longer
// epilogue is cut there and recorded as limit.epilogue_bytes, and the input
// after the cut is left unread but for the one byte that proved it longer.
//
// What it reads is the input of the multipart's own framing: for a multipart
// nested in a part, that part's body, which ends where the enclosing multipart's
// next delimiter line begins, so no byte of a sibling or of the enclosing
// framing is ever an epilogue; for one framed on a boundary inferred from its
// body, the rest of the kept body; for the message's own multipart body, the
// input after it. Under ProfileForensic that last is also the trailing input
// ParseSession.TrailingInputReader delivers, and the epilogue is then read
// through that reader, so that its count, its evidence and the one diagnostic
// it may record stay the reader's own: the bytes read this way are delivered,
// and are not delivered again by TrailingInputReader, which goes on after them,
// and what TrailingInputReader already delivered is not in the epilogue.

// epiDefaultEpilogueBytes is what MultipartEpilogue keeps at most when
// Limits.MaxEpilogueBytes resolves to no positive budget: LimitDisabled, or no
// parse budgets at all. An epilogue is never kept without a bound.
const epiDefaultEpilogueBytes = defaultMaxEpilogueBytes

// epiMaxEmptyReads is how many reads in a row that return nothing and no error
// end the reading of an epilogue, as bufio ends a fill.
const epiMaxEmptyReads = 100

// epiEpilogueCutAction is what MultipartEpilogue did with an epilogue longer
// than its budget.
const epiEpilogueCutAction = "kept the first bytes of the epilogue, up to the limit, and left the rest of the input unread"

// An epiState is what a healing iterator knows of its epilogue.
type epiState struct {
	// ended is that NextPart returned io.EOF at the end of the framing.
	ended bool

	// tail is the trailing input noteTrailing registered for this iterator
	// under ProfileForensic, through which its epilogue is read; nil for
	// every other iterator.
	tail *trailingTail

	// read is that the epilogue was read; b holds what was kept and over
	// whether more was there.
	read bool
	b    []byte
	over bool
}

// MultipartEpilogue returns the bytes that follow the closing delimiter line of
// the multipart body mr iterates — RFC 2046's epilogue, the line ending of that
// delimiter line excluded — as they were read, and whether they were cut at
// Limits.MaxEpilogueBytes (ROB-060-001).
//
// ok is false for an iterator that keeps none: one not from Parse, one under
// ProfileStrict, or one over a body assembled with NewMultipart. It is also
// false, and nothing is read, until NextPart has returned io.EOF at a closing
// delimiter line; a multipart whose input ended or failed before one, and a
// body with no delimiter line, has no epilogue, and nor has an iteration that
// ended with any other error.
//
// The first call that finds the epilogue reads it: at most MaxEpilogueBytes
// bytes, and one more to learn whether there was more. A MaxEpilogueBytes that
// is not positive, LimitDisabled included, keeps at most the 1 MiB default; an
// epilogue is never kept without a bound. When there was more, truncated is
// true, one limit.epilogue_bytes diagnostic of SeverityLimitExceeded is
// recorded, and the rest of the input is left unread. An error of the input,
// or a stop of the parse, before or while it reads ends the epilogue there: the
// bytes read until then are returned, and the error stays the session's, as
// Completion and ParseSession reports it; no diagnostic is recorded for it.
// Every later call reads nothing and returns the same bytes, each time as a
// copy of its own.
//
// An epilogue is read from the multipart's own input, which for a multipart
// nested in a part is that part's body: it ends where the enclosing multipart's
// next delimiter line begins. For the message's own multipart body under
// ProfileForensic, the epilogue is read through ParseSession.TrailingInputReader,
// whose snapshot, evidence and diagnostic account for those bytes as delivered:
// TrailingInputReader does not deliver them again, and bytes it delivered before
// are not in the epilogue. See multipart_epilogue.go.
//
// MultipartEpilogue is for the goroutine that iterates mr, and must not be
// called from inside a DiagnosticSink or a CharsetReader.
func MultipartEpilogue(mr MultipartReader) (epilogue []byte, truncated bool, ok bool) {
	r, isReader := mr.(*multipartReader)
	if !isReader || !r.healing {
		return nil, false, false
	}
	if !r.epi.read {
		if !r.epi.ended {
			return nil, false, false
		}
		src, off, closed := r.r.Epilogue()
		if !closed {
			return nil, false, false
		}
		r.readEpilogue(src, off)
	}
	return bytes.Clone(r.epi.b), r.epi.over, true
}

// readEpilogue reads the epilogue from src, the framing reader's epilogue,
// whose first byte is off bytes into the multipart body, and records the limit
// diagnostic when it was longer than the budget.
func (r *multipartReader) readEpilogue(src io.Reader, off int64) {
	limit := int64(epiDefaultEpilogueBytes)
	if l, ok := r.parent.limits(); ok && l.MaxEpilogueBytes > 0 {
		limit = l.MaxEpilogueBytes
	}

	// The message's trailing input is read through its one reader. What that
	// reader delivered before begins the epilogue's position later.
	var delivered int64
	if t := r.epi.tail; t != nil {
		src = t
		delivered = r.parent.session.TrailingInput().Delivered()
	}

	b, over := epiReadCapped(r.parent, src, limit)
	r.epi.read, r.epi.b, r.epi.over = true, b, over
	if !over {
		return
	}
	var at Location
	if !r.inferred {
		// The kept body an inferred boundary framed has no span in the input.
		at = r.parent.bodyLocationAt(off + delivered + limit)
	}
	// A sink that refuses it stops the parse; the stop is the session's, and
	// the epilogue read stays what it is.
	_ = r.parent.recordBody(Diagnostic{
		Code:     CodeLimitEpilogueBytes,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("multipart epilogue exceeds the limit of %d bytes", limit),
		Action:   epiEpilogueCutAction,
		Location: at,
	})
}

// epiReadCapped reads src until it has limit+1 bytes, it returns an error, it
// returns nothing epiMaxEmptyReads times in a row, or the parse ctx belongs to
// is stopped, and returns at most the first limit bytes and whether there were
// more. It grows what it holds as bytes arrive, never past limit+1.
func epiReadCapped(ctx parseContext, src io.Reader, limit int64) ([]byte, bool) {
	limit = min(limit, math.MaxInt64-1)
	want := limit + 1
	var b []byte
	for empty := 0; int64(len(b)) < want && empty < epiMaxEmptyReads; {
		if ctx.fatalErr() != nil {
			break
		}
		if len(b) == cap(b) {
			grow := int64(max(512, cap(b)))
			grow = min(grow, want-int64(len(b)))
			b = slices.Grow(b, int(grow))
		}
		room := b[len(b):cap(b)]
		if int64(len(room)) > want-int64(len(b)) {
			room = room[:want-int64(len(b))]
		}
		n, err := src.Read(room)
		n = max(0, min(n, len(room)))
		b = b[:len(b)+n]
		if err != nil {
			break
		}
		if n == 0 {
			empty++
		} else {
			empty = 0
		}
	}
	if int64(len(b)) > limit {
		return b[:limit:limit], true
	}
	return b, false
}
