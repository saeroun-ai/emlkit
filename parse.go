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

	// RecognizeMboxEnvelope asks the parse to recognize one mbox "From_"
	// envelope line — "From ", a sender and a ctime-shaped timestamp, as a
	// message saved from an mbox file may still begin with — at the very first
	// byte of the input, and to keep it apart from the message:
	// ParseSession.MboxEnvelope returns it, the message's header block begins
	// after it, the session records at most one header.mbox_envelope
	// diagnostic of SeverityInfo for it under every Profile — none when a stop
	// of the parse came first — and no writer writes it. Only
	// one such line, at offset 0 and nowhere else, is recognized; this does not
	// read mbox archives, and a "From " or ">From " line anywhere else is read
	// as before. A line that is not recognized, and every input with this
	// false, which is the default, is framed exactly as it always was. Read,
	// ReadWithOptions and the header of a part never recognize one. See mbox.go
	// for the grammar recognized, its bound, and what the line costs.
	RecognizeMboxEnvelope bool

	// WalkEmbeddedMessages makes Entity.Walk descend into the message a
	// message/rfc822 or message/global entity carries (and the header block of
	// a text/rfc822-headers one): after walkFunc was given the carrying
	// entity, the embedded message is visited as its one child, at the
	// carrying entity's path followed by 0, and its own parts under it. It is
	// off by default: an embedded message is then a leaf, as it always was,
	// and Entity.EmbeddedMessage reads one on request. See embedded.go.
	WalkEmbeddedMessages bool
}

// Parse reads a message from r and returns the session collecting what the
// parser observes, the message's root entity, and the error of producing that
// entity.
//
// That error is one of three things, and they are told apart by the predicates
// below rather than by position: the failure that left no entity at all, which
// for Parse is a header that could not be read; a warning that accompanies a
// perfectly usable entity, which is what an unknown transfer encoding or
// charset is; or a fatal error — a DiagnosticSink that stopped the parse, a
// resource budget the message ran past, or, under ProfileStrict, a refusal
// scoped to the entity it is returned with. One more accompanies a usable entity
// without being either: the input's own error, when a CharsetReader building
// the entity's decoder ran into it; see the input errors below.
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
//     decoding warnings below, the fifteen enforced resource budgets, the
//     no-progress rule, the repair of a valid media type or disposition
//     with malformed parameters, the header line-ending and byte order
//     mark notes described below, the mbox envelope line a caller asked
//     to have recognized, and, under ProfileForensic, a trailing input a
//     caller read that begins like another message,
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
// A reader that returns no bytes and no error is allowed 99 such reads in a
// row; the 100th ends the parse on the same terms as MaxInputBytes, with one
// input.no_progress diagnostic at the offset the input stopped at and a
// FatalError that errors.Is matches to io.ErrNoProgress. A byte resets the
// count, and an empty request never reaches r. The same rule ends one body
// whose decoders — a CharsetReader's reader — stall without taking input
// (body.decoder_no_progress, while its siblings stay reachable), and keeps the
// raw value of a Content-Type or Content-Disposition parameter whose
// encoded-word decoder stalls (header.decoder_no_progress). Nothing turns it
// off, and a reader blocked or looping inside one call is not interrupted.
// Read and ReadWithOptions do not apply it. See progress.go.
//
// An error r returns is an ordinary failure of the input, not a defect of the
// message and not a stop: no diagnostic is recorded for it and ParseStop stays
// nil (ROB-010-006). Bytes r returns together with an error are delivered, and
// r's first error is final: r is never read again after it, io.EOF included,
// and every later read of the input returns that identical value. It is
// reported where the parse reaches it, never earlier and never lost:
//
//   - while the message header is read, by Parse, with no entity, as the very
//     value r returned;
//   - after a header that ended before it, by the first read of Body or of the
//     parts that needs input past what was already buffered, after the bytes
//     buffered before it — so Parse may return an entity and a nil error for a
//     message whose input failed in its body. Nothing already returned is
//     taken back;
//   - by a body as the identical value at the root and inside the framing's
//     errors.Join under a part, whatever its decoders made of it: a decoder
//     that masks it as io.EOF or swallows it does not hide it, and a decoder
//     error of its own — quoted-printable's wording of a cut soft break, a
//     corruption base64 found — is joined to it rather than reported instead;
//   - by Parse or MultipartReader.NextPart together with the usable entity,
//     when a CharsetReader building the entity's decoder failed because the
//     input failed under it: the input's error itself, or joined with the
//     hook's, with no charset.unknown recorded for the entity. The value is
//     whatever the input returned, so a public predicate such as
//     IsUnknownCharset or IsFatal answers about what that value holds. What
//     the hook consumed is gone, and the input is not read again. Walk ends
//     with such an error, and mail.Reader.NextPart returns it with no part.
//
// io.EOF still ends a header block, a body or a part's framing as it always
// has. Read and ReadWithOptions do not make r's error final; the header
// reader underneath all of them reports the first error a read of a header
// block returned, keeps the fields read whole before it, and keeps a line the
// failure cut out of them (see textproto.ReadHeader).
//
// Empty and header-only input is framed the same way under every Profile and
// Compatibility (ROB-010-001). An input that ends with io.EOF before its first
// byte, or right after complete CRLF-terminated fields, folded ones included,
// is read as the same input with one empty line appended: the same fields, raw
// bytes and order as read, and an empty body. Having no fields, no body or no
// empty line adds no error and no diagnostic of its own, and nothing requires a
// field: no profile, ProfileStrict included, applies RFC 5322's minimum field
// counts. What the fields say is interpreted as for any message — an unknown
// transfer encoding or charset is still a warning, a Content-Type with
// malformed parameters is still repaired or, under ProfileStrict, refused — and
// budgets and cancellation keep their rules. One empty line ends the header
// block, and every byte after it, blank lines included, is body. A header that
// only io.EOF could end is still being read at that io.EOF, so an input error,
// a cancellation or a no-progress stop arriving there instead is reported by
// Parse with no entity, as for any header, and a header budget counts such a
// block's fields alone. After an empty line it is reported where the parse
// reaches it: usually by the body, but a cancellation seen by the read that
// delivered the header's last bytes is returned by ParseContext with the
// entity. A multipart with a usable boundary and an empty body has no part:
// MultipartReader.NextPart returns an error wrapping io.EOF and then io.EOF,
// and Walk ends with that error after the root (empty-multipart policy is
// ROB-060-009's). WriteTo does not reproduce such an input; see there. A
// malformed or whitespace-only first line is ROB-020's.
//
// Every header block — the message's and each part's — ends its lines at
// "\r\n", at a bare '\n' or at a bare '\r', a stray '\r' inside a value staying
// content (ROB-010-002; see textproto.ReadHeader for the exact rule, and
// line_endings.go). That framing is the same for Read and ReadWithOptions; what
// Parse adds is reporting it. A block with a line ended by a bare LF records
// header.bare_lf (SeverityInfo), one by a bare CR header.bare_cr
// (SeverityRecovered), and one mixing kinds header.mixed_line_endings — each
// at most once per parse, at the first such line ending; a block whose last
// line io.EOF ended before any line ending records header.truncated_line
// (SeverityIncomplete). A header ended by "\r\n" throughout records nothing.
// ProfileStrict accepts LF, as it always has, and refuses an entity whose
// header has a line ended by a bare '\r' or a last line with no line ending, on
// the terms of a refused Content-Type below: the entity is returned with an
// entity-scoped FatalError, its Content-Type is not interpreted and its body is
// kept as read, and the parse and the siblings go on. Raw fields store every
// line ending as CRLF, so such a header is not written back byte for byte. A
// header whose last line ending is a bare '\r' is still being read until the
// byte after it, or io.EOF, arrives; Limits.MaxInputBytes of exactly such a
// header's length, with a body after it, leaves no entity.
//
// A header block — the message's or a part's — may begin with a byte order
// mark (ROB-010-003; see textproto.Header.ByteOrderMark and bom.go). Every
// entry point frames it alike: a UTF-8 mark is kept by the Header and the
// fields are read after it; a UTF-16 or UTF-32 mark leaves the block unread
// and the entity opaque, its body every byte from the mark on, with no media
// type, transfer encoding or charset interpreted and no transcoding. Under
// Parse each such block records input.bom — SeverityRecovered for UTF-8,
// SeverityUnsupported for UTF-16 or UTF-32, with a usable entity and a nil err
// — and ProfileStrict refuses that entity on the same terms as a bare CR
// above.
//
// With ParseOptions.RecognizeMboxEnvelope, an input that begins with an mbox
// "From_" envelope line (ROB-010-004; see mbox.go) has that line kept apart
// from the message: ParseSession.MboxEnvelope returns its bytes, the message's
// header block begins after it and is read as above — a byte order mark
// included — at positions moved by its length, and at most one
// header.mbox_envelope diagnostic of SeverityInfo is recorded before the
// header's, under every Profile; a stop of the parse published first leaves it
// unrecorded. The line is kept even when the header after it cannot be read. It
// counts towards MaxInputBytes and towards none of the header budgets, and no
// writer writes it. Only one line, at offset 0, in the grammar mbox.go states,
// is recognized; any other input, and every input without the option, is
// framed as before, and no "From " or ">From " line elsewhere is removed or
// changed.
//
// The input after the message (ROB-010-005; see trailing.go) is not read on for
// by the parse's iterators, though the buffered readers may already have
// fetched some of it, as they fetch ahead everywhere. A message that frames no
// parts has none: its body runs to the end of the input, and a second message
// pasted into it is body. After the closing delimiter of a multipart message,
// ProfileForensic — and no other profile — lets the caller read what follows:
// once an iterator of the message's own body has returned io.EOF at that
// delimiter, ParseSession.TrailingInputReader delivers those bytes verbatim,
// once, when asked, held to MaxInputBytes and not to MaxEpilogueBytes; reading
// the root's Body or WriteTo instead consumes them as body, as it always did,
// and is no inspection. ParseSession.TrailingInput reports where they
// begin, how much was read, how it ended, and whether they begin like another
// message, which records one input.trailing_messages diagnostic of
// SeverityInfo: a suspicion, since a conformant epilogue may begin the same
// way. Nothing is split off or parsed as a message, and an epilogue that does
// not begin like one records nothing.
//
// Multipart delimiter lines are framed on the same three line endings, before
// and after each delimiter, in any mix (see textproto.MultipartReader). A
// delimiter line is the ending before it, "--", the boundary, "--" for the
// closing one, spaces and tabs, and its own ending, or for a closing one the
// end of the input; anything that only begins like one — "--B-x", "--B--x" —
// is body, every byte. A '\r' before a line that is exactly a delimiter makes
// it one: that is the only way a multipart written with bare CRs can be
// framed, and RFC 2046 forbids the boundary at the start of any line of a
// body. A delimiter cut short by a failure of the input is body, followed by
// that failure. Under Parse the iterator records multipart.bare_lf
// (SeverityInfo) and multipart.bare_cr (SeverityRecovered) at the first such
// ending, and multipart.mixed_line_endings when one multipart's delimiters use
// more than one kind (SeverityInfo, SeverityRecovered with a CR) — each at
// most once per parse, at the container's path and the ending's position in
// the input. These are observed by NextPart, including the call that ends the
// iteration with io.EOF: a delimiter a Body read reached is recorded, or
// refused, by the next NextPart, not by the read. ProfileStrict accepts LF and
// LF with CRLF, and refuses a multipart with a bare CR before or after a
// delimiter: the NextPart that observes it returns, then and on every later
// call, the iterator's FatalError carrying multipart.bare_cr — the parts
// already returned stay valid, the part that delimiter introduced is not
// returned, an enclosing multipart goes on, and the parse is not stopped. A
// stop of the session, and whatever the input or a budget failed with, come
// first. With no line budget a delimiter line must fit in 4 KiB, as framing
// lines always had to, and one that does not is an ordinary framing error;
// under Limits.MaxLineBytes its white space may run to that budget.
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
// Limits.MaxMessageDepth bounds how deeply messages nest (ROB-090-002). A
// message/rfc822 or message/global body is a leaf until the caller asks for
// the message inside it — Entity.EmbeddedMessage, or Entity.Walk under
// ParseOptions.WalkEmbeddedMessages — and an embedded message past the budget
// is not read: one limit.message_depth diagnostic and an entity-scoped
// FatalError, with the carrying entity and the parse left as they were. A
// carrier nobody asks about is one ordinary part of MaxPartCount, and the
// structure inside its body costs nothing. See embedded.go.
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
