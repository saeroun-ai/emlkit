package message

import (
	"fmt"

	"github.com/saeroun-ai/emlkit/textproto"
)

// The header block budgets — Limits.MaxHeaderBytes, MaxHeaderFieldBytes,
// MaxHeaderCount and MaxLineBytes — applied to every entity of a parse rather
// than only to the message's own header.
//
// # What is counted
//
// Four different things, each about the same block and each stated once, on
// Limits:
//
//   - MaxHeaderBytes: the whole block as it appears in the input, from the
//     first byte of its first field through the empty line that ends it, that
//     empty line included.
//   - MaxHeaderFieldBytes: one logical field, its continuation lines and every
//     line terminator among them included, exactly as they appear in the input.
//   - MaxHeaderCount: the logical field candidates the block yields — including
//     one skipped afterwards for an empty name, so that a block of nothing but
//     colons costs what a block of that many fields costs, and so that an
//     empty-name field cannot be used to evade the budget.
//   - MaxLineBytes: one physical line of the block, not counting the terminator
//     that ends it.
//
// A block at exactly a budget is read; one unit more is refused. Every budget is
// per entity and never cumulative: each block is read under the whole of each,
// so a message of many parts is not refused for what its earlier parts already
// spent, and a part is not judged by the size of the message header above it.
//
// When one block exceeds several at once — a single enormous line is also an
// enormous field, and eventually an enormous block — the one reported is the one
// bounding the smallest scope, in that order. The rule is stated and applied in
// textproto's headerScan so that it cannot depend on how the input chunks its
// reads.
//
// # Where it is enforced
//
// At the layer that reads the block, so that the block is bounded while it is
// read rather than built first and measured afterwards. Under Parse every block
// — the message's own and every part's — is bounded by
// textproto.ReadHeaderLimits, which counts what it consumes and stops without
// touching the reads that follow the block. One mechanism at every depth is what
// makes one boundary rule true at every depth, and what makes a refusal report
// itself as a refusal wherever it happens.
//
// The legacy path is the exception, and is left alone deliberately: Read and
// ReadWithOptions still bound the reads under the header parser with
// limitedReader, which is the mechanism their documented behavior was written
// against, down to which inputs they turn away and with which error. They budget
// nothing else at all.
//
// Neither mechanism can take a body byte with it, and neither depends on how the
// input chunks its reads.
//
// # What a failure means
//
// The scope that was reading the block ends. Nothing is recovered from a header
// block that overran a budget: what follows it has already been partly consumed,
// and nothing can say how much of the input the block would have covered, so the
// alternative to stopping is guessing. Everything already returned stays valid
// and owned by the caller, as FatalError describes — including the parent entity
// of a part that failed, and the siblings the caller already has.
//
// Only Parse enforces these on a part. Read and ReadWithOptions resolve no
// configuration and allocate no session; the message header keeps the byte
// budget and the failure it has always had, and the parts below it stay
// unbudgeted.

// headerLimitActions is what a limit.* diagnostic of a refused header block
// reports was done about the defect: nothing was repaired, and the scope stopped
// where the budget did. Each kind gets its own wording because each names a
// different thing that was stopped, but all four say the same thing about the
// outcome.
const headerLimitAction = "stopped at this entity's header block and produced no entity from it"

// headerLimitCode returns the diagnostic code of a refusal of kind.
//
// The mapping is total by construction: every kind textproto can refuse with has
// a code reserved for it in the registry, and an unknown kind — which would mean
// textproto grew one this layer has not been taught — falls back to the block
// budget's code rather than recording a diagnostic with no code at all.
func headerLimitCode(kind textproto.HeaderLimitKind) Code {
	switch kind {
	case textproto.HeaderLimitFieldBytes:
		return CodeLimitHeaderFieldBytes
	case textproto.HeaderLimitCount:
		return CodeLimitHeaderCount
	case textproto.HeaderLimitLineBytes:
		return CodeLimitLineBytes
	}
	return CodeLimitHeaderBytes
}

// headerLimitMessage returns the human-readable text of a refusal of kind with
// budget limit. It is not used for program logic; the code is.
func headerLimitMessage(kind textproto.HeaderLimitKind, limit int64) string {
	switch kind {
	case textproto.HeaderLimitFieldBytes:
		return fmt.Sprintf("header field exceeds the %d byte limit", limit)
	case textproto.HeaderLimitCount:
		return fmt.Sprintf("header block exceeds the limit of %d fields", limit)
	case textproto.HeaderLimitLineBytes:
		return fmt.Sprintf("header line exceeds the %d byte limit", limit)
	}
	return fmt.Sprintf("header block exceeds the %d byte limit", limit)
}

// headerLimitDiagnostic returns the diagnostic of a header block that exceeded
// one of its budgets, caused by cause.
//
// It is the one place that diagnostic is built, so the root of a message and a
// part five levels into it report the same code, the same severity and the same
// action, and differ only in the path and position their own scope supplies.
//
// The severity is SeverityLimitExceeded rather than SeverityFatal: what is
// recorded is that a configured budget was reached, which is what happened.
// That the scope could not continue afterwards is what the returned FatalError
// says, and newFatalDiagnostic takes its own copy for that.
func headerLimitDiagnostic(kind textproto.HeaderLimitKind, limit int64, cause error) Diagnostic {
	return Diagnostic{
		Code:     headerLimitCode(kind),
		Severity: SeverityLimitExceeded,
		Message:  headerLimitMessage(kind, limit),
		Action:   headerLimitAction,
		Cause:    cause,
	}
}

// rootHeaderError reports the failure of reading the message's own header block:
// err as it is for the legacy read path, and a budget failure turned into a
// diagnostic and a FatalError for a parse that has a session to record it in.
//
// session is nil for Read and ReadWithOptions, which is what keeps their
// documented failure exactly what it was: the same error value, not a fatal one,
// and nothing recorded anywhere.
//
// Only a budget is translated, and only a budget textproto really refused a
// block with. That question is asked of textproto.HeaderLimit and of nothing
// else: neither the sentinels the refusal carries nor the shape of the value
// carrying it can answer it, because an input can present both. An error that
// merely looks like a refusal never reached a budget, and reporting a budget
// that was not reached would be a diagnostic about something that did not
// happen. Every other way a header block can fail — malformed syntax, an I/O
// error — is nothing this item diagnoses, and is passed through untouched
// rather than dressed up as something it is not.
//
// The budget the diagnostic reports is the one the refusal carries rather than
// the one this call was configured with, so what is reported is what was
// actually enforced.
func rootHeaderError(err error, session *ParseSession) error {
	if session == nil {
		return err
	}
	// textproto.ReadHeaderLimits returns its refusal as itself, so only err's
	// own dynamic type is asked about: nothing a source's read failure wraps
	// is unwrapped to look further.
	top, ok := err.(*textproto.HeaderLimitError)
	if !ok {
		return err
	}
	limitErr, refused := textproto.HeaderLimit(top)
	// A shared-budget refusal that is not this session's own, which
	// readWithOptions has already handled, is something an input handed back.
	if !refused || isTotalHeaderKind(limitErr.Kind()) {
		return err
	}

	// The message's own header block starts at the first byte of the input, so
	// that is where its context points, and its path is the root's nil path.
	// The diagnostic points more precisely: at the first byte of the line or
	// field the budget actually refused, which for the block budget is the block
	// itself. Where the block would have ended is not a position in the input
	// and is not claimed as one. The body offset is not known — the block that
	// would have ended never did — and nothing here reports one.
	//
	// The diagnostic is completed before it is recorded, so that the copy the
	// session keeps and the copy the fatal error carries say the same thing
	// about where this happened.
	ctx := rootContext(session, 0)
	d := headerLimitDiagnostic(limitErr.Kind(), limitErr.Limit(), err)
	d.Location = ctx.headerLocationAt(limitErr.UnitOffset() - limitErr.Offset())
	d = ctx.completeHeader(d)

	if fatal := ctx.recordHeader(d); fatal != nil {
		// A sink asked this parse to stop while it was being told about the
		// budget. The stop belongs to the whole session rather than to this
		// scope, so it is what the caller is told about; the diagnostic has
		// already been delivered either way.
		return fatal
	}

	return newFatalDiagnostic(d)
}

// headerLimitFailure records the one diagnostic of a part whose header block
// exceeded a budget, ends this iterator, and returns the error every later call
// to it will return.
//
// The failure is kept because the iterator must report it identically however
// often it is asked: recording it a second time would put the same defect in
// the session twice, and the underlying reader cannot be asked again anyway —
// it stands inside a part whose extent it never learned.
func (r *multipartReader) headerLimitFailure(limit *textproto.HeaderLimitError) error {
	// The part is named and placed as the one it was about to become: the index
	// it would have been given, and the position the reader had already reached
	// when it started reading the block. The offsets the failure carries are
	// relative to the body being iterated, and child turns them into positions
	// in the original input. Its body offset is not known, so the header offset
	// stands in for it and only a header position is ever reported from here.
	ctx := r.parent.child(r.next, limit.Offset(), limit.Offset())

	// Completed before it is recorded, so that the session's copy and the one
	// the fatal error carries name the same part at the same position. The
	// position is the refused line or field, which for the block budget is where
	// the block began.
	d := headerLimitDiagnostic(limit.Kind(), limit.Limit(), limit)
	d.Location = ctx.headerLocationAt(limit.UnitOffset() - limit.Offset())
	d = ctx.completeHeader(d)

	if fatal := ctx.recordHeader(d); fatal != nil {
		// A sink stopped the whole parse while it was being told about this
		// part. That stop outranks this scope's own failure and is what every
		// scope of the parse now reports, this one included.
		r.fatal = fatal
		return fatal
	}

	r.fatal = newFatalDiagnostic(d)
	return r.fatal
}
