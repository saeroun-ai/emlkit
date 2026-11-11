package message

import (
	"errors"
	"fmt"
	"strings"
)

// LimitDisabled turns a single budget off.
//
// It is spelled out rather than left as a bare -1 so that a configuration reads
// as what it means:
//
//	opts := &message.ParseOptions{Limits: message.Limits{
//		MaxInputBytes: message.LimitDisabled,
//	}}
//
// Disabling a budget is a deliberate act. A parser with no budget is bounded
// only by its input, so a caller that turns one off is taking responsibility
// for bounding that input some other way.
const LimitDisabled int64 = -1

// ErrInvalidLimits reports that ParseOptions could not be resolved into a usable
// configuration: a budget below LimitDisabled, or two budgets for the same
// quantity that disagree.
//
// It is a caller's configuration error rather than a defect of a message, so it
// is not a FatalError, no diagnostic is recorded for it, and no byte of the
// input is read. Parse still returns a session, so its Profile, Compatibility
// and Limits can be read; see Parse.
//
// Match it with errors.Is. The wrapped text names the offending field and value
// and is for humans; it is not part of the API contract.
var ErrInvalidLimits = errors.New("message: invalid parse limits")

// The documented default budgets.
//
// Each is a finite operational starting point that balances interoperability
// against denial-of-service resistance. None of them is a statement about what
// conformant mail contains. RFC 5322 section 2.1.1 limits a physical line to
// 998 characters excluding CRLF and recommends no more than 78; that line rule
// does not itself set an overall message, header-block, or logical-field
// budget, and MIME subformats can add their own constraints. These defaults are
// not measurements of, nor claims about, the caps of any mail provider either.
// A deployment is expected to tune them from its own corpus rather than read
// them as a conformance boundary.
const (
	// defaultMaxInputBytes bounds one whole message so that an input which never
	// ends is eventually stopped.
	defaultMaxInputBytes = 256 << 20 // 256 MiB

	// defaultMaxHeaderBytes is the existing 1 MiB header budget, unchanged, and
	// deliberately shared with ReadOptions.MaxHeaderBytes rather than restated.

	// defaultMaxHeaderFieldBytes bounds one logical header field with all of its
	// continuation lines counted together.
	defaultMaxHeaderFieldBytes = 64 << 10 // 64 KiB

	// defaultMaxHeaderCount bounds the work and storage consumed by one header
	// block independently of the block's byte budget.
	defaultMaxHeaderCount = 1024

	// defaultMaxLineBytes bounds one physical syntax line: a line the parser
	// must materialize or interpret as a line, which is header and MIME framing
	// syntax. The value is deliberately much larger than RFC 5322's 998-character
	// limit so tolerant parsing can recover malformed or extension-generated
	// input while retaining a finite allocation ceiling. It is not a conformance
	// threshold. It does not bound opaque body content, which is streamed and
	// budgeted by the encoded and decoded body limits instead. See
	// Limits.MaxLineBytes for the exact scope.
	defaultMaxLineBytes = 64 << 10 // 64 KiB

	// defaultMaxTotalHeaderBytes and defaultMaxTotalHeaderCount bound the header
	// blocks of a whole parse together, which the per-entity budgets times
	// MaxPartCount do not usefully bound. Both are far above their per-entity
	// counterparts, so a single header block meets those first.
	defaultMaxTotalHeaderBytes = 32 << 20 // 32 MiB
	defaultMaxTotalHeaderCount = 100000

	// defaultMaxParameterCount bounds the parameters of one header field, such
	// as one Content-Type.
	defaultMaxParameterCount = 256

	// defaultMaxParameterSegments bounds the RFC 2231 continuation segments of
	// one parameter, so that a value split into segments costs bounded work.
	defaultMaxParameterSegments = 256

	// defaultMaxMIMEDepth bounds recursion through nested multipart containers.
	defaultMaxMIMEDepth = 100

	// defaultMaxMessageDepth bounds nested message/rfc822 and message/global
	// entities independently of multipart recursion.
	defaultMaxMessageDepth = 32

	// defaultMaxPartCount bounds the parts of the whole tree, not of one
	// container, so that many shallow containers cost as much as one deep one.
	defaultMaxPartCount = 10000

	// defaultMaxPreambleBytes and defaultMaxEpilogueBytes bound the text around
	// a multipart body, which carries no part data: the budget is what keeps a
	// gateway's leftovers, or an unterminated prologue, from being unbounded.
	defaultMaxPreambleBytes = 1 << 20 // 1 MiB
	defaultMaxEpilogueBytes = 1 << 20 // 1 MiB

	// defaultMaxEncodedBodyBytes bounds one entity's body as it appears on the
	// wire, before any decoding.
	defaultMaxEncodedBodyBytes = 128 << 20 // 128 MiB

	// defaultMaxDecodedBodyBytes bounds what one entity's body decodes to,
	// which its wire size does not already bound. The transfer encodings this
	// library decodes never produce more bytes than they consume, but charset
	// conversion to UTF-8 can, so what a body decodes to is budgeted on its own
	// rather than inferred from defaultMaxEncodedBodyBytes.
	defaultMaxDecodedBodyBytes = 128 << 20 // 128 MiB

	// defaultMaxTotalDecodedBytes bounds the decoded bytes of every entity
	// together, which is what bounds a message made of many individually
	// acceptable parts.
	defaultMaxTotalDecodedBytes = 512 << 20 // 512 MiB

	// defaultMaxDiagnosticCount bounds recorded and delivered diagnostics, so
	// that a message engineered to be defective everywhere cannot grow a
	// session, or a sink's work, without end.
	defaultMaxDiagnosticCount = 1000

	// defaultMaxMemorySpoolBytes bounds what is held in memory before raw
	// content is spooled elsewhere.
	defaultMaxMemorySpoolBytes = 8 << 20 // 8 MiB

	// defaultMaxDiskSpoolBytes bounds what one message may spool to temporary
	// files.
	defaultMaxDiskSpoolBytes = 1 << 30 // 1 GiB
)

// Limits are the resource budgets a parse runs under.
//
// Every budget is an int64 and every one obeys the same rule, so a caller
// learns it once:
//
//   - zero selects the documented default, which is what the zero Limits does
//     for every field at once;
//   - LimitDisabled (-1) turns that one budget off;
//   - any other positive value is the budget itself, in the unit the field
//     names — bytes, or a count;
//   - anything below LimitDisabled is invalid and Parse rejects it with
//     ErrInvalidLimits rather than guessing what was meant.
//
// math.MaxInt64 is an ordinary finite budget, not a synonym for LimitDisabled:
// it resolves to, and is reported as, the value it is, and the arithmetic that
// enforces a budget is written so that it stays exact there.
//
// The defaults are finite operational starting points that balance
// interoperability against denial-of-service resistance. They are not a
// conformance boundary. RFC 5322 section 2.1.1 limits physical lines to 998
// characters excluding CRLF and recommends no more than 78, while that rule
// does not itself establish overall message, header-block, or logical-field
// budgets. MIME subformats can impose additional constraints. MaxLineBytes is
// intentionally more tolerant than the RFC 5322 line limit so malformed input
// can be recovered within a finite allocation ceiling. The defaults are also
// not claims about what any mail provider accepts. Tune them from the corpus a
// deployment actually handles:
//
//	Field                  Default
//	MaxInputBytes          256 MiB
//	MaxHeaderBytes         1 MiB
//	MaxHeaderFieldBytes    64 KiB
//	MaxHeaderCount         1024
//	MaxLineBytes           64 KiB
//	MaxTotalHeaderBytes    32 MiB
//	MaxTotalHeaderCount    100 000
//	MaxParameterCount      256
//	MaxParameterSegments   256
//	MaxMIMEDepth           100
//	MaxMessageDepth        32
//	MaxPartCount           10000
//	MaxPreambleBytes       1 MiB
//	MaxEpilogueBytes       1 MiB
//	MaxEncodedBodyBytes    128 MiB
//	MaxDecodedBodyBytes    128 MiB
//	MaxTotalDecodedBytes   512 MiB
//	MaxDiagnosticCount     1000
//	MaxMemorySpoolBytes    8 MiB
//	MaxDiskSpoolBytes      1 GiB
//
// DefaultLimits returns those values resolved, so a caller can read one, adjust
// it, or compare against it without repeating a number from this table.
//
// Three further budgets are enforced where their subject is asked for, and are
// not among the fifteen described next: MaxPreambleBytes by a multipart
// iterator that heals (every profile but ProfileStrict), MaxEpilogueBytes by
// MultipartEpilogue, and MaxMessageDepth by Entity.EmbeddedMessage and a walk
// under ParseOptions.WalkEmbeddedMessages. MaxMemorySpoolBytes and
// MaxDiskSpoolBytes are validated and reported and bound nothing: mail.Spool
// takes its quotas from mail.SpoolOptions.
//
// Fifteen budgets are enforced by the parse itself, and only under Parse. MaxDiagnosticCount bounds
// the diagnostics a parse records and delivers, and by itself stops no parsing:
// the request that reaches it is replaced by one terminal limit.diagnostic_count
// diagnostic in the last slot and every later request is omitted; see the field.
// The "exactly one" or "one" diagnostic each of the other fourteen records below
// is recorded only while that budget has a slot left. The failure it describes
// happens whether or not it is recorded, with the same FatalError carrying the
// same Diagnostic. Those fourteen fall into six groups by what a failure costs,
// and the grouping is part of the contract:
//
//   - MaxInputBytes bounds the bytes read from the input for the whole message,
//     shared by every read of the parse.
//   - MaxHeaderBytes, MaxHeaderFieldBytes, MaxHeaderCount and MaxLineBytes bound
//     the header block of every entity — the message's own and every part's at
//     any depth.
//   - MaxTotalHeaderBytes and MaxTotalHeaderCount bound all of those header
//     blocks together, and a failure of either ends the whole parse.
//   - MaxParameterCount and MaxParameterSegments bound the parameterized header
//     fields the parse engine interprets for itself.
//   - MaxMIMEDepth and MaxPartCount bound the shape of the MIME tree: how far
//     the parser descends through nested multipart containers, and how many
//     child parts it discovers in the whole message.
//   - MaxEncodedBodyBytes, MaxDecodedBodyBytes and MaxTotalDecodedBytes bound
//     the bodies of the leaves of the tree as they are read: the wire bytes of
//     one body, what one body delivers once decoded, and what all of them
//     deliver together.
//
// An input past MaxInputBytes ends the whole parse with one limit.input_bytes
// diagnostic; see the field for exactly when that is decided. The other groups
// fail differently from each other. A header block past one of its budgets is
// all or nothing: it yields no header, records exactly one limit.* diagnostic at
// SeverityLimitExceeded, and ends that scope with a FatalError, because a block
// that overran leaves the input somewhere that cannot be resynchronized to. A
// field past a parameter budget is not fatal at all: the media type and the
// parameters within the budget were read from bytes that are not in doubt, so
// they are kept, the remainder is ignored, one diagnostic records what was kept,
// and the parse continues into the body. A tree budget refuses one child: the
// child is not returned and nothing but its header block was ever read, one
// diagnostic names it, and a FatalError ends a scope — one multipart iterator
// for MaxMIMEDepth, and the whole parse for MaxPartCount, which is one budget
// the entire tree shares. A body past one of its budgets keeps everything it
// already delivered: a per-body budget ends that body alone and its parent goes
// on to the next part, while MaxTotalDecodedBytes ends the whole parse.
//
// Read and ReadWithOptions keep the behavior they always had: MaxHeaderBytes
// reaches the message header alone, a part's header is not budgeted, no other
// budget applies, and a refused message header is the error it has always been
// with nothing recorded. Header.ContentType and Header.ContentDisposition parse
// under no budget on every path, including Parse; see there.
//
// Every other budget is validated, resolved and reported by ParseSession.Limits,
// but nothing consults it yet. MaxMessageDepth is the one with a stated reason
// rather than a schedule: it counts nested message/rfc822 and message/global
// entities, and this engine does not parse those bodies as entities at all, so
// there is no nested-message transition to count until it does. Later releases
// add enforcement behind these same fields, with the limit.* diagnostics the
// Code registry reserves. A release may also add a budget, as
// MaxTotalHeaderBytes and MaxTotalHeaderCount were added. A positional
// Limits{...} literal then no longer compiles. A keyed literal, the zero Limits
// and DefaultLimits still compile and leave the new field zero or at its
// default, which enforces it: a parse that was accepted before may now be
// refused by the new budget. Set the new field to LimitDisabled to keep the
// earlier behavior.
//
// Limits is a plain comparable value. Copying it copies the whole
// configuration, and nothing retains the value a caller passes in.
type Limits struct {
	// MaxInputBytes bounds the bytes read from the input for one message,
	// including everything below.
	//
	// It is counted once, at the input itself, so every read of the parse
	// spends it: the message's header block, its body, the framing and headers
	// of every part, and the bytes a multipart iterator skips past a part the
	// caller did not read. A budget of N admits N bytes, and a message of
	// exactly N bytes is read whole. Only a byte that actually exists after
	// the N-th exceeds it, which costs reading that one byte, and that byte is
	// never delivered to anything.
	//
	// It is spent as raw bytes are read from the input, not as they are
	// interpreted. Parse interprets the message's header block and nothing
	// more, and the rest is interpreted as the caller consumes it, but reading
	// is buffered: body bytes fetched ahead together with the header count, and
	// may already be all of the budget. Fetching up to exactly N is not an
	// overflow and reads no proof byte. What is never read at all — a body left
	// unread past what was fetched, the epilogue after the last part — is never
	// checked against it, so a parse that ends without a failure says nothing
	// about the size of the message beyond what was read.
	//
	// Exceeding it ends the whole parse. It records exactly one
	// limit.input_bytes diagnostic at offset N, the first byte not admitted,
	// with the nil path of the message as a whole, and publishes the session's
	// FatalError: the read that exceeded it, every later read of the input, and
	// every later scope of the parse report that one error, and the input is
	// not read again. A read of a part's body reports an error wrapping it,
	// since the multipart framing adds that the part's closing delimiter was not
	// found. Everything already returned stays valid, including a message
	// header that ended within the budget and every byte already delivered; a
	// header block the budget cut short yields no header at all. When
	// MaxDiagnosticCount replaces that diagnostic with its terminal or omits
	// it, the overflow ends the parse all the same, with a FatalError that
	// still carries the limit.input_bytes diagnostic.
	//
	// An I/O error or io.EOF the input returns without a byte past the budget
	// is reported as itself and is not an overflow. A DiagnosticSink that stops
	// the parse while it is told about the overflow, or a stop published before
	// it, is the error the parse ends with instead.
	MaxInputBytes int64

	// MaxHeaderBytes bounds one header block, counted as it appears in the
	// input: from the first byte of its first field through the empty line that
	// ends the block, that empty line included. A block of exactly the budget is
	// read and one byte more is refused. A UTF-8 byte order mark the block
	// begins with is counted here and by MaxTotalHeaderBytes, and by no field,
	// line or count budget; a block that begins with a UTF-16 or UTF-32 mark is
	// not read as a header and costs no header budget (ROB-010-003, see
	// textproto.ReadHeader).
	//
	// It is per entity and never cumulative. Under Parse it applies to the
	// message's own header and to the header of every part at any depth, each
	// under the whole budget, so a message of many parts is not refused for what
	// its earlier parts already spent. MaxTotalHeaderBytes is the cumulative
	// bound.
	//
	// It is the one budget that also has a legacy spelling,
	// ReadOptions.MaxHeaderBytes. See ParseOptions.Limits for how the two are
	// reconciled, and ReadOptions.MaxHeaderBytes for what the older entry points
	// still do with it.
	MaxHeaderBytes int64

	// MaxHeaderFieldBytes bounds one header field, counted with its
	// continuation lines joined.
	MaxHeaderFieldBytes int64

	// MaxHeaderCount bounds the number of fields in one header block.
	MaxHeaderCount int64

	// MaxLineBytes bounds one physical syntax line, not counting its
	// terminator.
	//
	// Its scope is deliberately narrow: it covers a line that some part of the
	// parser has to materialize or interpret as a line, which is principally
	// header and MIME framing syntax — header field lines and their
	// continuations, multipart delimiter lines, and the line-oriented framing
	// of the transfer encodings.
	//
	// RFC 5322 section 2.1.1 requires at most 998 characters excluding CRLF and
	// recommends at most 78. This byte budget is deliberately higher so tolerant
	// parsing can recover nonconforming or extension-generated syntax without an
	// unbounded allocation; it is not the threshold for declaring conformance.
	//
	// It is not a bound on body content. An opaque text, binary or encoded body
	// that contains no line terminator is not treated as one enormous line:
	// such a body is streamed, is never accumulated into a line buffer, and is
	// never rejected for its length alone. What bounds it is
	// MaxEncodedBodyBytes and MaxDecodedBodyBytes.
	MaxLineBytes int64

	// MaxTotalHeaderBytes bounds the header blocks of one whole parse
	// together: the wire bytes MaxHeaderBytes counts in each block, summed
	// over the message's own header and every part header the parse reads, at
	// any depth. Multipart framing, preamble, epilogue, bodies, bytes drained
	// past a part, and bytes only peeked at or buffered ahead are not header
	// bytes; a part with no header block, or a container drained without being
	// iterated, costs nothing. Each byte is charged once.
	//
	// It is charged as blocks are read, one read of the parser's 4 KiB header
	// buffer at a time — a line, or one buffer of a longer line — after
	// MaxLineBytes, MaxHeaderFieldBytes and MaxHeaderBytes have admitted that
	// read, so those smaller budgets win a read that exceeds both. A read
	// that does not fit is refused whole, so what is charged stays below N by
	// the part of that read that would have fitted; the diagnostic is located
	// at the first byte past the budget. Nothing is refunded: the bytes a block
	// already had admitted stay charged when a per-entity budget refuses that
	// block, and a child refused by MaxPartCount or MaxMIMEDepth was read
	// whole and is charged whole. A total of exactly N is not an overflow.
	//
	// Exceeding it ends the whole parse: the block that crossed it yields no
	// header, one limit.total_header_bytes diagnostic names that entity, and
	// the session's FatalError is published, which Parse (for the message's
	// own header), every NextPart, Entity.Walk, Entity.ParseStop and
	// mail.Reader then report. Everything already returned stays valid. A stop
	// published before it, by a DiagnosticSink or another budget, outranks it,
	// and a sink refusing its diagnostic ends the parse with the sink's stop.
	//
	// The per-entity header budgets are unchanged by it, so disabling one of
	// those leaves this as the bound on header material.
	MaxTotalHeaderBytes int64

	// MaxTotalHeaderCount bounds the field candidates MaxHeaderCount counts in
	// each block — an empty-name candidate included, the empty line that ends
	// a block excluded — summed over every block MaxTotalHeaderBytes covers.
	//
	// A field is counted once it has been read whole. Once all N have been
	// counted, the next candidate is refused at its first byte, before any of
	// it is read, so an exhausted count bounds a candidate even with every
	// byte budget disabled; reaching the end of the input, or the empty line
	// that ends a block, is not a candidate. A block whose first line begins
	// with white space is malformed and fails with that error as before; that
	// line is no candidate, and only the byte budgets bound its length. A
	// failure ends the whole parse, on the same terms as MaxTotalHeaderBytes,
	// with one limit.total_header_count diagnostic located at the refused
	// candidate.
	MaxTotalHeaderCount int64

	// MaxParameterCount bounds the parameters parsed from one header field,
	// such as one Content-Type or Content-Disposition.
	MaxParameterCount int64

	// MaxParameterSegments bounds the RFC 2231 continuation segments of one
	// parameter.
	MaxParameterSegments int64

	// MaxMIMEDepth bounds how deeply multipart containers nest. The message
	// itself is depth zero, whatever its media type is, and every child a
	// multipart body yields is exactly one deeper than the entity whose body it
	// came out of. A budget of N admits every entity at depth N or less, so the
	// first child it can refuse is one at depth N+1.
	//
	// A container that sits exactly at the budget is iterated like any other: it
	// is a container that may hold nothing, and reaching its end is an ordinary
	// io.EOF rather than a refusal. Only a child that actually exists is
	// refused, which costs reading that one child's header block and nothing
	// else about it — no body, no decoder, and none of its own parts.
	//
	// Exceeding it ends the multipart iterator that hit it and nothing wider.
	// The entity holding that container, the siblings already returned, and
	// every shallower part stay valid, and iterating the parent continues.
	MaxMIMEDepth int64

	// MaxMessageDepth bounds how deeply message/rfc822 and message/global
	// entities nest, independently of MaxMIMEDepth.
	//
	// A message/* body stays an opaque leaf until it is asked for:
	// Entity.EmbeddedMessage, and Walk under ParseOptions.WalkEmbeddedMessages,
	// read the message it carries, and each such step counts one level here.
	// A carrier past the budget is not read: it stays a leaf, and
	// limit.message_depth is recorded for it (embedded.go). A message/rfc822
	// part that is never descended into is one ordinary part of MaxPartCount
	// and costs nothing here.
	MaxMessageDepth int64

	// MaxPartCount bounds the child parts of the whole tree.
	//
	// It is one budget for the whole message, shared by every multipart iterator
	// of the parse and never reset on entering a container, so a tree of many
	// small containers is bounded exactly as a single wide container is. There
	// is deliberately no separate per-container setting: a total bound already
	// bounds one wide container.
	//
	// The root entity is not counted — nothing discovered it, it is the message.
	// Everything a multipart iterator does discover counts one, with no
	// exceptions: a leaf, a container that holds further parts, and a part with
	// no header block at all.
	//
	// A message holding exactly the budget is read whole and ends with an
	// ordinary io.EOF. The part that exceeds it is refused as MaxMIMEDepth's is,
	// after its header block alone has been read, but the failure belongs to the
	// whole parse rather than to one iterator: every later scope of it —
	// iterators built earlier for other containers, and Entity.Walk — reports the
	// identical error. Everything already returned stays valid.
	MaxPartCount int64

	// MaxPreambleBytes bounds the text between a multipart header and its first
	// delimiter.
	MaxPreambleBytes int64

	// MaxEpilogueBytes bounds the text after a multipart closing delimiter
	// that MultipartEpilogue keeps: it reads at most this many bytes and one
	// more, and records limit.epilogue_bytes, SeverityLimitExceeded, when that
	// one was there, leaving the rest unread. LimitDisabled keeps the 1 MiB
	// default: an epilogue is never kept without a bound. Nothing else reads
	// an epilogue for the parser.
	//
	// It does not bound the input after the message that
	// ParseSession.TrailingInputReader delivers under ProfileForensic: the
	// parse does not read on for those bytes — its buffered readers may
	// already hold some of them, fetched ahead as everywhere — the caller
	// does, and they count towards MaxInputBytes as every read of the input
	// does.
	MaxEpilogueBytes int64

	// MaxEncodedBodyBytes bounds the wire bytes of one leaf body, before any
	// transfer decoding, counted as they are read for that body — by its
	// decoders or by WriteTo's verbatim copy.
	//
	// A leaf is an entity whose MultipartReader returns nil when it is
	// constructed: every entity that is not multipart/*, message/* parts
	// included, and a multipart entity whose damaged Content-Type was repaired
	// with no boundary that may be used. A framed multipart container has a raw
	// body too, but it is outside the leaf budgets: reading it directly or
	// copying it verbatim is bounded by MaxInputBytes alone, and iterating it
	// holds each part to its own budgets.
	//
	// A budget of N admits N bytes. Only a byte that exists past them is an
	// overflow; proving it reads that one byte, which no decoder ever sees.
	// Within the decoded budgets, the body then delivers what its decoders make
	// of the N admitted bytes, including output they had already buffered, and
	// then reports a FatalError carrying one limit.encoded_body_bytes
	// diagnostic, located at the first byte not admitted. What a cut leaves is the decoders' own
	// reading of a truncated stream: an incomplete base64 quantum is dropped,
	// and a cut quoted-printable escape is kept literally, as a malformed one
	// is. The failure is the body's alone: the header and the delivered prefix
	// stay valid, ParseStop stays nil, and the parent multipart can drain the
	// unread raw tail, under MaxInputBytes, and continue with the next part
	// unless another limit or a DiagnosticSink stops it.
	// Body returns that FatalError from then on, unless MaxTotalDecodedBytes
	// later ends the parse, after which it returns the parse's stop; the value
	// already returned stays valid. Bytes consumed by the body, its decoders'
	// read-ahead and the drained tail are not replayed (#41), and no raw API is
	// added: the existing verbatim WriteTo path stays a conditional one-shot
	// copy, available only before Body is read.
	MaxEncodedBodyBytes int64

	// MaxDecodedBodyBytes bounds the bytes one leaf's Body delivers, after
	// every decoding stage that ran, including a CharsetReader the caller
	// installed. It counts bytes, not characters, so a body cut by it may end
	// inside a UTF-8 sequence.
	//
	// A budget of N permits at most N bytes; a shorter body, or one that
	// fails earlier, delivers fewer. Reaching exactly N is not an overflow:
	// only a byte that exists past them is, and proving it reads that one byte
	// from the decoders and discards it. The failure is local, on the
	// same terms as MaxEncodedBodyBytes, with one limit.decoded_body_bytes
	// diagnostic located at the start of the body: decoded bytes have no
	// position in the input. A body reports one local failure, the first
	// proven. Decoders read ahead, so when the encoded and decoded budgets of
	// a body are nearly reached together, which of the two is proven first may
	// depend on how the body is read.
	MaxDecodedBodyBytes int64

	// MaxTotalDecodedBytes bounds the bytes every leaf Body of one message
	// delivers together, on the same terms as MaxDecodedBodyBytes. Headers,
	// framing, the bytes an iterator drains past a part, and WriteTo's verbatim
	// copies are not counted, and nothing is counted twice.
	//
	// Exceeding it ends the whole parse: it records one
	// limit.total_decoded_bytes diagnostic for the leaf that proved it, and
	// publishes the session's FatalError, which every scope of the parse
	// reports and which every leaf Body — one already returned, or one that
	// already failed on its own budget, included — returns instead of
	// delivering further decoded output. A verbatim WriteTo copy is not decoded
	// output and is not stopped by it. When one byte exceeds both this and a
	// body's MaxDecodedBodyBytes, this is the failure. A stop published before
	// an overflow — by a DiagnosticSink or by another budget — outranks it and
	// nothing is recorded; such an earlier stop does not by itself end reading
	// a body already returned.
	MaxTotalDecodedBytes int64

	// MaxDiagnosticCount bounds how many diagnostics one parse records and
	// delivers, the terminal limit.diagnostic_count entry included. It bounds
	// diagnostics only: reaching it ends no scope and stops no reading by
	// itself, so metadata is still healed, bodies are still read and later
	// parts are still returned unless something else ends them.
	//
	// A budget of N reserves its last slot for the terminal; the default is
	// 1000. The first N-1 diagnostic requests of the parse are recorded as they
	// are. In place of the N-th, the session records one limit.diagnostic_count
	// diagnostic at SeverityLimitExceeded, carrying the path and location of the
	// request it replaced, and delivers it to the DiagnosticSink as it would
	// any other. Every request after that is omitted: the session neither
	// copies, retains nor delivers it. A budget of 1 therefore makes the first
	// request the terminal, and a parse that makes no request records no
	// terminal. The count stops at N, so a budget of math.MaxInt64 cannot
	// overflow it, and LimitDisabled records every request.
	//
	// The terminal says recording stopped, not that the message has more than
	// N defects: a parse that makes exactly N requests records the terminal
	// too, in place of the N-th, and nothing past that point is counted.
	// Retained diagnostics stay append-only — nothing recorded before the
	// terminal is ever replaced or removed — so Diagnostics reports at most N
	// entries, the terminal last. The same count applies whether or not the
	// session retains and whether or not it has a sink, so it bounds what a
	// sink receives as well as what the session keeps. What it bounds is the
	// session's work: a producer may already have built, and formatted the
	// message of, a diagnostic the session then omits.
	//
	// Every other budget, Strict refusal and sink stop is enforced as it is
	// without it. The request the terminal replaces, or one omitted after it,
	// may itself be such a failure — an input or part-count overflow, a
	// refused header — and it still ends its scope, or the whole parse, with
	// its own FatalError, whose Diagnostic is that failure's own rather than
	// the terminal. A sink that refuses the terminal stops the parse exactly as
	// a sink refusing any other diagnostic does, and a stop published earlier
	// keeps its identity.
	MaxDiagnosticCount int64

	// MaxMemorySpoolBytes bounds what one message holds in memory when raw
	// content has to be retained for replay.
	MaxMemorySpoolBytes int64

	// MaxDiskSpoolBytes bounds what one message spools to temporary files.
	MaxDiskSpoolBytes int64
}

// A limitField is one budget seen generically: its exported name, the way to
// reach it in a Limits value, and the default it selects.
//
// The table below is the single place a budget is declared. Resolution,
// validation and DefaultLimits all read it, so a field cannot be added to
// Limits and then forgotten by one of them.
type limitField struct {
	name string
	get  func(*Limits) *int64
	def  int64
}

// limitFields declares every budget once, in the order the fields are declared
// in Limits.
var limitFields = []limitField{
	{"MaxInputBytes", func(l *Limits) *int64 { return &l.MaxInputBytes }, defaultMaxInputBytes},
	{"MaxHeaderBytes", func(l *Limits) *int64 { return &l.MaxHeaderBytes }, defaultMaxHeaderBytes},
	{"MaxHeaderFieldBytes", func(l *Limits) *int64 { return &l.MaxHeaderFieldBytes }, defaultMaxHeaderFieldBytes},
	{"MaxHeaderCount", func(l *Limits) *int64 { return &l.MaxHeaderCount }, defaultMaxHeaderCount},
	{"MaxLineBytes", func(l *Limits) *int64 { return &l.MaxLineBytes }, defaultMaxLineBytes},
	{"MaxTotalHeaderBytes", func(l *Limits) *int64 { return &l.MaxTotalHeaderBytes }, defaultMaxTotalHeaderBytes},
	{"MaxTotalHeaderCount", func(l *Limits) *int64 { return &l.MaxTotalHeaderCount }, defaultMaxTotalHeaderCount},
	{"MaxParameterCount", func(l *Limits) *int64 { return &l.MaxParameterCount }, defaultMaxParameterCount},
	{"MaxParameterSegments", func(l *Limits) *int64 { return &l.MaxParameterSegments }, defaultMaxParameterSegments},
	{"MaxMIMEDepth", func(l *Limits) *int64 { return &l.MaxMIMEDepth }, defaultMaxMIMEDepth},
	{"MaxMessageDepth", func(l *Limits) *int64 { return &l.MaxMessageDepth }, defaultMaxMessageDepth},
	{"MaxPartCount", func(l *Limits) *int64 { return &l.MaxPartCount }, defaultMaxPartCount},
	{"MaxPreambleBytes", func(l *Limits) *int64 { return &l.MaxPreambleBytes }, defaultMaxPreambleBytes},
	{"MaxEpilogueBytes", func(l *Limits) *int64 { return &l.MaxEpilogueBytes }, defaultMaxEpilogueBytes},
	{"MaxEncodedBodyBytes", func(l *Limits) *int64 { return &l.MaxEncodedBodyBytes }, defaultMaxEncodedBodyBytes},
	{"MaxDecodedBodyBytes", func(l *Limits) *int64 { return &l.MaxDecodedBodyBytes }, defaultMaxDecodedBodyBytes},
	{"MaxTotalDecodedBytes", func(l *Limits) *int64 { return &l.MaxTotalDecodedBytes }, defaultMaxTotalDecodedBytes},
	{"MaxDiagnosticCount", func(l *Limits) *int64 { return &l.MaxDiagnosticCount }, defaultMaxDiagnosticCount},
	{"MaxMemorySpoolBytes", func(l *Limits) *int64 { return &l.MaxMemorySpoolBytes }, defaultMaxMemorySpoolBytes},
	{"MaxDiskSpoolBytes", func(l *Limits) *int64 { return &l.MaxDiskSpoolBytes }, defaultMaxDiskSpoolBytes},
}

// DefaultLimits returns every budget resolved to its documented default value.
//
// The result is concrete: no field is zero, so a caller can read a default,
// compare against it, or scale one without having to know that zero would have
// meant "the default" anyway.
//
//	l := message.DefaultLimits()
//	l.MaxInputBytes /= 4
//	session, e, err := message.Parse(r, &message.ParseOptions{Limits: l})
//
// It returns a fresh value each time, so modifying the result affects nothing
// else.
func DefaultLimits() Limits {
	var l Limits
	for _, f := range limitFields {
		*f.get(&l) = f.def
	}
	return l
}

// resolve returns l with every field replaced by the concrete budget it
// selects: a default for zero, and the value itself otherwise, including
// LimitDisabled.
//
// It is the central resolver. Every path that turns caller configuration into
// budgets goes through it, so the rule that zero means the default and values
// below LimitDisabled are invalid is stated in exactly one place.
//
// The first invalid field is reported, wrapping ErrInvalidLimits and naming the
// field and value. Fields are examined in declaration order, so the error a
// caller sees for a given value does not depend on map iteration or on which
// budgets happen to be enforced.
func (l Limits) resolve() (Limits, error) {
	out := l
	for _, f := range limitFields {
		p := f.get(&out)
		switch {
		case *p == 0:
			*p = f.def
		case *p < LimitDisabled:
			return Limits{}, fmt.Errorf("%w: Limits.%s = %d is below LimitDisabled (%d)",
				ErrInvalidLimits, f.name, *p, LimitDisabled)
		}
	}
	return out, nil
}

// A resolvedConfig is the immutable configuration of one parse: the profile and
// compatibility policy it runs under, and every budget resolved to a concrete
// value.
//
// It exists so that a session holds one settled configuration rather than the
// caller's options, which may still say "zero, meaning the default" and which
// the caller may change afterwards. Resolution happens once, in Parse, before
// any byte of input is read; nothing downstream re-derives a budget or has to
// know the encoding of an unset one.
//
// The zero resolvedConfig is the zero session's: the default profile and
// policy, and no limits resolved yet. ParseSession.Limits reports DefaultLimits
// for it, which is the same configuration a parse with no options would run
// under.
type resolvedConfig struct {
	profile Profile
	compat  Compatibility
	limits  Limits

	// mbox is ParseOptions.RecognizeMboxEnvelope; see mbox.go.
	mbox bool

	// walkEmbedded is ParseOptions.WalkEmbeddedMessages; see embedded.go.
	walkEmbedded bool

	// fallbackCharset is ParseOptions.FallbackCharset; see charset_monitor.go.
	fallbackCharset string
}

// resolveConfig turns caller options into the configuration one parse runs
// under, and into the ReadOptions the existing read path is given.
//
// The two results are not independent: ReadOptions.MaxHeaderBytes and
// Limits.MaxHeaderBytes are two spellings of one budget, and this is where they
// are reconciled. See bridgeMaxHeaderBytes.
//
// No input is read here, and nothing is recorded. An error means the
// configuration could not be settled at all, and the caller gets it before any
// byte of the message is touched.
func resolveConfig(o ParseOptions) (resolvedConfig, ReadOptions, error) {
	limits, err := o.Limits.resolve()
	if err != nil {
		return resolvedConfig{}, ReadOptions{}, err
	}

	header, err := bridgeMaxHeaderBytes(o.Limits.MaxHeaderBytes, o.ReadOptions.MaxHeaderBytes, limits.MaxHeaderBytes)
	if err != nil {
		return resolvedConfig{}, ReadOptions{}, err
	}
	limits.MaxHeaderBytes = header

	// The read path takes the resolved budget in its own long-standing
	// encoding, where a negative value means no limit. Resolution has already
	// reduced every accepted spelling to a positive count or LimitDisabled, so
	// what it is handed here is exactly what it has always understood.
	read := o.ReadOptions
	read.MaxHeaderBytes = header

	return resolvedConfig{profile: o.Profile, compat: o.Compatibility, limits: limits, mbox: o.RecognizeMboxEnvelope, walkEmbedded: o.WalkEmbeddedMessages, fallbackCharset: strings.TrimSpace(o.FallbackCharset)}, read, nil
}

// bridgeMaxHeaderBytes reconciles the two spellings of the header byte budget
// and returns the resolved value both of them then mean.
//
// fromLimits and fromRead are the raw fields as the caller wrote them, where
// zero means "not set"; resolved is what Limits.MaxHeaderBytes already resolved
// to, which is the default when the caller set nothing.
//
// The rules, in order:
//
//   - An unset ReadOptions.MaxHeaderBytes leaves the decision to Limits, set or
//     defaulted. This is the ordinary case and the one every existing caller of
//     Parse is in.
//   - A set ReadOptions.MaxHeaderBytes goes through the same resolver as any
//     other budget, so a value below LimitDisabled is rejected here rather than
//     silently meaning "no limit". That legacy reading survives where it was
//     always documented — ReadWithOptions, which does not resolve anything —
//     but Parse is a new entry point and does not inherit it.
//   - Two set values are accepted only when they resolve to the same budget.
//     Choosing one over the other would silently discard a limit the caller
//     asked for, which is the one outcome a safety budget must never have, so
//     the disagreement is reported instead.
func bridgeMaxHeaderBytes(fromLimits, fromRead, resolved int64) (int64, error) {
	if fromRead == 0 {
		return resolved, nil
	}
	if fromRead < LimitDisabled {
		return 0, fmt.Errorf("%w: ReadOptions.MaxHeaderBytes = %d is below LimitDisabled (%d)",
			ErrInvalidLimits, fromRead, LimitDisabled)
	}
	if fromLimits != 0 && fromRead != resolved {
		return 0, fmt.Errorf("%w: Limits.MaxHeaderBytes = %d and ReadOptions.MaxHeaderBytes = %d are two budgets for the same header bytes and disagree",
			ErrInvalidLimits, resolved, fromRead)
	}
	return fromRead, nil
}
