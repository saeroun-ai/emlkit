package message

import (
	"context"
	"io"
)

// ParseOptions configures Parse.
//
// The zero value is the recommended configuration: the tolerant profile, the
// conservative compatibility policy, every recorded diagnostic retained in the
// session — at most Limits.MaxDiagnosticCount of them, 1000 by default — and the
// default read limits. A nil *ParseOptions means the same thing, so a
// caller with nothing to configure passes nil.
//
// Options are read once, when Parse is called, and are not retained: changing
// the value afterwards does not affect a parse in progress.
type ParseOptions struct {
	// Profile selects how much interpretation the parser may apply to a
	// malformed message and how much of the input it keeps. The zero value is
	// ProfileTolerant.
	Profile Profile

	// Compatibility names the policy used to break ties when a malformed
	// construct has several plausible interpretations. The zero value is
	// CompatibilityConservative.
	Compatibility Compatibility

	// DiagnosticSink, when non-nil, receives every diagnostic the session
	// records as it is discovered, in discovery order, and may ask the parse to
	// stop by returning an error. Limits.MaxDiagnosticCount bounds what it
	// receives exactly as it bounds what is retained: at most that many calls,
	// the last of them the terminal limit.diagnostic_count diagnostic when the
	// budget is reached. See DiagnosticSink for what a sink may do and what
	// stopping means.
	DiagnosticSink DiagnosticSink

	// DiscardDeliveredDiagnostics asks the session not to retain the
	// diagnostics it delivers, for a caller that handles each one in its sink
	// as it arrives and does not want the memory of a parse to grow with the
	// number of defects in the message. ParseSession.Diagnostics then reports
	// nothing. The sink's own work is bounded separately, by
	// Limits.MaxDiagnosticCount.
	//
	// It is ignored while DiagnosticSink is nil: a diagnostic is never dropped
	// without being delivered somewhere.
	DiscardDeliveredDiagnostics bool

	// Limits are the resource budgets this parse runs under. The zero value
	// selects the documented default for every budget; see Limits.
	//
	// One budget has two spellings, because ReadOptions.MaxHeaderBytes predates
	// this field: Limits.MaxHeaderBytes and ReadOptions.MaxHeaderBytes are the
	// same limit written two ways, and both are read in exactly the same units
	// with exactly the same meaning for zero and for -1.
	//
	// Setting neither uses the default 1 MiB, which is what every existing
	// caller already gets. Setting either one uses that one. Setting both is
	// accepted only while they agree; if they resolve to different budgets,
	// Parse returns an error verifying errors.Is(err, ErrInvalidLimits) rather
	// than choosing one and silently dropping the other.
	Limits Limits

	// ReadOptions carries the existing read limits and decoding options,
	// unchanged and with the same defaults as ReadWithOptions.
	ReadOptions ReadOptions
}

// Parse reads a message from r and returns the session collecting what the
// parser observes, the message's root entity, and the error of producing that
// entity.
//
// That error is one of three things, and they are told apart by the predicates
// below rather than by position: the failure that left no entity at all, which
// for Parse is a header that could not be read; a warning that accompanies a
// perfectly usable entity, which is what an unknown transfer encoding or
// charset is; or a fatal error — a DiagnosticSink that stopped the parse, or a
// resource budget the message ran past.
//
// Parse is the entry point that gives a caller a ParseSession. Read and
// ReadWithOptions keep their signatures and their behavior; they simply have
// nowhere to return a session to, so they allocate none.
//
// Like Read, Parse reads only the header eagerly. The body stays a lazy stream
// and the multipart tree is only walked as the caller consumes it, so most
// defects are discovered after Parse has returned — which is exactly why the
// session outlives the call:
//
//	session, e, err := message.Parse(r, nil)
//	// consume e.Body, e.MultipartReader() or e.Walk(...)
//	for _, d := range session.Diagnostics() {
//		log.Println(d)
//	}
//
// The three results answer three different questions, and each must be read on
// its own terms:
//
//   - The session is never nil, including when the header could not be read at
//     all, so there is always somewhere to look. What it holds, though, is what
//     some producer has been implemented to record, which today is the two
//     decoding warnings below, the fifteen enforced resource budgets, and the
//     repair of a valid media type or disposition with malformed parameters,
//     up to Limits.MaxDiagnosticCount, whose last slot holds one terminal
//     limit.diagnostic_count diagnostic in place of the rest. A framing
//     failure that nothing diagnoses yet is carried by err alone and leaves the
//     session empty, so read both rather than treating an empty session as a
//     clean parse. Later releases add diagnostics for the remaining resource
//     limits and for header framing, and they will appear here without changing
//     this contract.
//   - A non-nil entity is usable and belongs to the caller, whatever err says.
//     An unknown transfer encoding or charset is a warning of exactly this
//     kind: err verifies IsUnknownEncoding or IsUnknownCharset, the entity is
//     fully usable, and the stage that had no decoder is skipped rather than
//     guessed at. Each skipped stage records a diagnostic whose Action says
//     what the body therefore holds — a skipped charset stage still yields
//     transfer decoded bytes, and a skipped transfer stage yields bytes the
//     charset stage may still have converted. When both are unknown, err
//     reports the charset, and the session holds both diagnostics in discovery
//     order.
//   - A nil entity means no usable result could be constructed, which for Parse
//     means the header itself could not be read.
//
// Options that cannot be resolved into a configuration — a budget below
// LimitDisabled, or two budgets for the same quantity that disagree — are a
// caller's error rather than a defect of a message, and are reported before the
// input is touched: err verifies errors.Is(err, ErrInvalidLimits), the entity
// is nil, not one byte is read from r, and no diagnostic is recorded, because
// there is nothing about the message to report. The session is still returned
// and still reports the requested Profile and Compatibility; its Limits are the
// defaults, since the requested ones were never settled.
//
// Limits.MaxInputBytes bounds every byte this parse reads from r, counted once
// at r whichever scope reads it — the header, a body, a part's framing or
// header, or a part a multipart iterator skips. It is spent as raw bytes are
// read from r, which includes body bytes buffered ahead while the header was
// read, and only that far: a tail never read from r is never checked. A byte that
// actually exists past the budget ends the whole parse: one limit.input_bytes
// diagnostic at the offset of that byte, which is never delivered, and the
// session's FatalError, returned by Parse itself when the message header did not
// end within the budget, and otherwise by the read that reached it and every
// later scope. Everything already returned stays valid. See Limits.MaxInputBytes.
//
// A header block that exceeds Limits.MaxHeaderBytes, MaxHeaderFieldBytes,
// MaxHeaderCount or MaxLineBytes is refused, and refused wherever it is found:
// the message's own header block and the header block of every part, at any
// depth, are each held to the whole of every one of those budgets. The refusal
// records one limit.header_bytes, limit.header_field_bytes, limit.header_count
// or limit.line_bytes diagnostic naming the entity, and where in the input the
// line or field it refused began, and ends the scope that was reading it with a
// FatalError carrying that diagnostic — the whole parse for the message header,
// and the iteration over one multipart for a part of it. A block that exceeds
// several at once reports the budget bounding the smallest scope, in the order
// line, field, block, count. Nothing is recovered from the refused block itself;
// everything already returned stays valid, as FatalError describes.
//
// Limits.MaxTotalHeaderBytes and MaxTotalHeaderCount bound all of those header
// blocks together, and exceeding either ends the whole parse with one
// limit.total_header_bytes or limit.total_header_count diagnostic. See the
// fields for what is charged and when.
//
// Limits.MaxLineBytes reaches past the header blocks to the other lines the
// parser must read as lines: a multipart body's framing lines, and the
// line-oriented framing of the base64 and quoted-printable transfer encodings.
// Either records one limit.line_bytes diagnostic and ends that scope — the
// iteration over one multipart, or the reading of one body — while everything
// already returned stays valid, including the parts already yielded and every
// byte the body already produced. It never bounds opaque body content: a body
// with no line terminator in it is streamed rather than accumulated, and is
// never refused for its length alone.
//
// Limits.MaxParameterCount and Limits.MaxParameterSegments bound the
// Content-Type and Content-Disposition fields the parser interprets for itself,
// and are the one group of budgets that is not fatal. A field past either keeps
// its media type and the parameters within the budget, ignores the remainder,
// records one limit.parameter_count or limit.parameter_segments diagnostic whose
// Action says what was kept, and the parse continues into the body — so an
// oversized parameter list does not hide the multipart children a boundary
// parameter introduces. Content-Disposition is read only when something needs
// it — the charset stage under ReadOptions.SkipTextAttachmentDecoding, or a
// caller of Entity.EffectiveContentDisposition such as mail.Reader — and at most
// once per entity, so it is diagnosed at most once. Entity.EffectiveContentType
// and EffectiveContentDisposition report the bounded result. Header.ContentType
// and Header.ContentDisposition are unaffected and parse under no budget; see
// there.
//
// Limits.MaxMIMEDepth and Limits.MaxPartCount bound the shape of the tree the
// message describes: how far Parse descends through nested multipart
// containers, counting the message itself as depth zero, and how many child
// parts it discovers in the whole message, not counting the message itself.
// MaxPartCount is one budget the whole tree shares and is never reset on
// entering a container.
//
// Both are charged where a child is discovered and not before, so a tree exactly
// as deep or as wide as its budget is read whole and ends with an ordinary
// io.EOF. A child past either is not returned: only its header block was read —
// bounded by the header budgets like any other — and nothing else about it was
// touched. Each records one limit.mime_depth or limit.part_count diagnostic
// naming that child and where its header block began, with a FatalError carrying
// the same diagnostic. What ends differs, and follows from what each budget
// bounds: MaxMIMEDepth ends the one multipart iterator that hit it, leaving the
// parent, the siblings and every shallower part usable and the parent's
// iteration able to continue, while MaxPartCount ends the whole parse, so every
// later scope of it — iterators built earlier for other containers, and
// Entity.Walk — reports the identical error and one diagnostic is recorded for
// the message. Everything already returned stays valid either way. A child that
// exceeds both at once reports limit.part_count, which is charged first.
//
// Every "records one diagnostic" above, and every diagnostic a producer
// records, is recorded only while Limits.MaxDiagnosticCount has a slot left. The
// request that reaches a positive budget N is replaced by one
// limit.diagnostic_count diagnostic, and every later request is omitted —
// neither retained nor delivered. That budget ends nothing by itself: parsing
// continues, and a failure whose diagnostic was replaced or omitted — a budget,
// a Strict refusal — still ends its scope with its own FatalError, which still
// carries its own Diagnostic. See Limits.MaxDiagnosticCount.
//
// Limits.MaxMessageDepth is reserved and enforced by nothing: a message/rfc822
// or message/global body is an opaque leaf to this engine rather than a nested
// entity, so no nested-message transition exists to count. Such a part is one
// ordinary part of MaxPartCount, and the structure inside its body costs
// neither budget.
//
// A Content-Type or Content-Disposition whose base is valid — type/subtype, or
// a disposition token — but whose parameters are malformed, stray empty items
// between semicolons included, is read item by item from what the parameter
// budgets retained (ROB-050-001; see metadata_repair.go). Under the default
// ProfileTolerant, and ProfileForensic,
// the base and every well-formed, unambiguous parameter are kept, a conflicting
// or damaged repeat of a parameter is dropped rather than chosen, the raw field
// is left as it was, and one mime.content_type_invalid or
// mime.disposition_invalid and, for a conflict, one mime.parameter_duplicate
// diagnostic of SeverityRecovered say what was kept. So "multipart/mixed;
// boundary=B; broken" is still a multipart with boundary B. A repaired multipart
// left with no boundary it may use — "boundary=B; boundary=C" — chooses none:
// its body is one opaque part with the input's bytes, MultipartReader returns
// nil, the siblings after it are read as usual, and its diagnostics are
// SeverityInvalidMetadata, saying the parts inside were not read. Under
// ProfileStrict the field is not interpreted and the entity is refused with an
// entity-scoped FatalError carrying that diagnostic, while the parse continues —
// its parent and siblings are unaffected and ParseStop stays nil. What the
// refusal does to the body depends on when it is known. A refused Content-Type
// is known during construction, so the body is exactly as it was read: no
// decoder and no part iteration. A Content-Disposition is read lazily: refused
// by the charset stage under SkipTextAttachmentDecoding, it keeps that stage
// from running, though transfer decoding is already set up; refused later, by
// a caller, it refuses the entity without undoing decoding already chosen or
// bytes already read. A base that is not valid on its own is
// ROB-050-002's and is reported as before. Every Compatibility value behaves the
// same here.
//
// If a DiagnosticSink stops the parse, err is a FatalError wrapping the sink's
// own error and the entity is the usable prefix recovered before the stop. No
// diagnostic is recorded for that failure: a sink refusing to continue is a
// property of the caller, not a defect of the message. The stop belongs to the
// whole session, so every later scope of this parse — MultipartReader.NextPart
// and Entity.Walk — reports the identical error and recovers nothing further,
// while everything already returned stays valid.
//
// Parse cannot be cancelled. It is ParseContext with no context; see there for
// a parse that can be.
func Parse(r io.Reader, opts *ParseOptions) (*ParseSession, *Entity, error) {
	return parse(context.Background(), r, opts)
}

// ParseContext is Parse for a parse that ctx can cancel, or give a deadline.
// Everything Parse documents holds for it, and only what cancellation adds is
// described here; cancel.go has the full account.
//
// A nil ctx, and one whose Done method returns nil, such as
// context.Background, cancel nothing: ParseContext is then exactly Parse. Read,
// ReadWithOptions and New have no context and are unchanged.
//
// The context governs the whole lazy life of the parse, not only this call:
// the body and the parts of the message are read after ParseContext returns,
// and each of those reads belongs to the parse ctx was given for. The session
// keeps ctx for as long as the parse can be read, so cancelling it — as the
// usual defer cancel() does — is also how a caller ends the reading of a
// message it has abandoned.
//
// Options that cannot be resolved are reported first, exactly as by Parse and
// before ctx is consulted at all, so a configuration error is never hidden by a
// context that is already done. Otherwise a context already done when
// ParseContext is called reads nothing from r and returns no entity, together
// with the stop below.
//
// Once ctx is done, the parse ends with a FatalError that records no diagnostic
// and carries none: the caller cancelling a parse is not a defect of the
// message, as a DiagnosticSink stopping it is not. errors.Is finds ctx.Err()
// through it — context.Canceled or context.DeadlineExceeded — and finds the
// context's cause too, when context.WithCancelCause or a similar constructor
// gave it one; as everywhere, errors.Is compares a cause of a type that is not
// comparable only through that cause's own Is method. The stop belongs to the
// whole session, like every session-wide stop: the first one published is the
// one every scope of the parse reports from then on, so a sink or a budget that
// stopped the parse before the cancellation was seen stays what is reported.
//
// Cancellation is seen only at the points cancel.go lists — before and after
// every read of r and of every entity's Body, at every scope boundary, and at
// the work boundaries of reading a structured header field — and never
// interrupts anything. A source blocked in Read keeps ParseContext, or the
// read of a Body, blocked until the source returns. ParseContext never closes
// r, never changes a deadline of it, and starts no goroutine to watch ctx;
// closing a source the caller owns from a context.AfterFunc of the caller's
// own is how such a read is made to return.
//
// Before a read, a cancellation already seen returns no bytes and the stop,
// without calling the reader below. After a read, the bytes it returned are
// delivered, and if the cancellation is seen at that point the stop replaces
// whatever error came with them, io.EOF included. Everything already returned
// stays valid, as it does for every FatalError.
func ParseContext(ctx context.Context, r io.Reader, opts *ParseOptions) (*ParseSession, *Entity, error) {
	return parse(ctx, r, opts)
}

// parse is Parse and ParseContext. ctx may be nil, which cancels nothing.
func parse(ctx context.Context, r io.Reader, opts *ParseOptions) (*ParseSession, *Entity, error) {
	var o ParseOptions
	if opts != nil {
		o = *opts
	}

	diagOpts := diagnosticOptions{
		sink:        o.DiagnosticSink,
		noRetention: o.DiscardDeliveredDiagnostics,
	}

	// Configuration is settled before the input is touched, so that a caller's
	// mistake costs no bytes and cannot be confused with a defect of the
	// message. The session of a rejected configuration is a real session — it
	// reports the profile and policy that were asked for, and the default
	// budgets, since the requested ones never resolved — but nothing was
	// parsed, so nothing is recorded in it.
	cfg, read, err := resolveConfig(o)
	if err != nil {
		return newParseSessionWithOptions(o.Profile, o.Compatibility, diagOpts), nil, err
	}

	session := newParseSessionWithConfig(cfg, diagOpts)
	// Before anything can see the session: the context is fixed for the life
	// of the parse. See cancel.go.
	session.setContext(ctx)

	e, parseErr := readWithOptions(r, &read, session)
	return session, e, parseErr
}
