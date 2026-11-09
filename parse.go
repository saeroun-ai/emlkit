package message

import "io"

// ParseOptions configures Parse.
//
// The zero value is the recommended configuration: the tolerant profile, the
// conservative compatibility policy, every diagnostic retained in the session,
// and the default read limits. A nil *ParseOptions means the same thing, so a
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

	// DiagnosticSink, when non-nil, receives every diagnostic as it is
	// discovered, in discovery order, and may ask the parse to stop by
	// returning an error. See DiagnosticSink for what a sink may do and what
	// stopping means.
	DiagnosticSink DiagnosticSink

	// DiscardDeliveredDiagnostics asks the session not to retain the
	// diagnostics it delivers, for a caller that handles each one in its sink
	// as it arrives and does not want the memory of a parse to grow with the
	// number of defects in the message. ParseSession.Diagnostics then reports
	// nothing.
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
//     decoding warnings below, the nine enforced resource budgets, and the
//     repair of a valid media type or disposition with malformed parameters. A framing
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
func Parse(r io.Reader, opts *ParseOptions) (*ParseSession, *Entity, error) {
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

	e, parseErr := readWithOptions(r, &read, session)
	return session, e, parseErr
}
