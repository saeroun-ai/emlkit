package message

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/saeroun-ai/emlkit/textproto"
)

// An Entity is either a whole message or a one of the parts in the body of a
// multipart entity.
//
// An Entity can only be consumed once: after its body is read, it can't be
// used anymore.
type Entity struct {
	Header Header    // The entity's header.
	Body   io.Reader // The decoded entity's body.

	// mediaType, mediaParams and mediaErr are the Content-Type this entity was
	// constructed with, as the parse it came from interpreted it — under that
	// parse's parameter budgets — together with the error that interpretation
	// returned. They decided the body's decoders and whether MultipartReader
	// iterates anything, and EffectiveContentType reports them; mediaParams is
	// never handed out, only copies of it. See effective.go.
	mediaType   string
	mediaParams map[string]string
	mediaErr    error

	// unframed reports that the Content-Type was repaired into a multipart
	// media type with no boundary that may be used (ROB-050-001). The body is
	// then one opaque part: MultipartReader returns nil rather than an
	// iterator that could not find a single delimiter. See metadata_repair.go.
	unframed bool

	// framing is, for a framed multipart entity of a parse, its body as the
	// parse read it, before the cancellation gate of ParseContext — what this
	// entity's own MultipartReader frames its parts from — and nil otherwise.
	//
	// The gate is for what a caller reads: once the parse is cancelled, Body
	// and every part's Body deliver nothing more. Framing is not a caller's
	// read. Where the enclosing scope's end has already been proven from bytes
	// the parse holds — the outer delimiter after an inner closing delimiter,
	// whose line ending is the outer one's — the framing must still see that
	// end as the end of its input, as it would after an ordinary failure of
	// the input at the same point, or a body the cancellation cut would end
	// differently from the same body cut by a failure. Everything this reads
	// comes, through the reader below it, from bytes the parse already took
	// from its input, or from the input behind the gate on the input itself,
	// which reads nothing once cancellation has been seen. (A Body the caller
	// replaced is not framed through this; MultipartReader gates it.) So after
	// the cancellation, framing only works through bytes the parse already
	// admitted — what each enclosing container's reader holds: its 4 KiB
	// buffer, a delimiter candidate held or being delivered as body, at most a
	// framing line under Limits.MaxLineBytes plus one buffer (one buffer with
	// that budget disabled), and its framing-line accumulator, bounded the same
	// way — at every depth of containers, Limits.MaxMIMEDepth bounding how
	// many, and only inside a read a caller had already started. NextPart and
	// Walk still report the stop before framing anything.
	framing io.Reader

	// disposition is the Content-Disposition this entity was constructed with,
	// interpreted at most once. It is nil only for an Entity that no constructor
	// built — a struct literal — and that is how such an entity is recognized.
	// See dispositionSnapshot.
	disposition *dispositionSnapshot

	// originalBody is the undecoded body as passed to New, together with the
	// transfer encoding and charset it was encoded with. WriteTo copies the
	// body through verbatim using these when the output encoding/charset are
	// unchanged, preserving the exact bytes (e.g. for DKIM body hashes).
	// originalBody shares the underlying reader with Body, so reading Body
	// consumes it too: call WriteTo before reading Body to keep this working.
	// Under Parse a leaf's originalBody is its encoded body limiter, so the
	// verbatim copy is held to Limits.MaxEncodedBodyBytes and not to the
	// decoded budgets; see limit_body.go.
	originalBody io.Reader
	encoding     string
	charset      string

	// decodedBody is the body value this Entity was constructed with (what Body
	// was initially set to). The verbatim fast path in writeBodyTo only applies
	// while Body still equals it; if the caller reassigned Body, that replacement
	// is written instead of originalBody.
	decodedBody io.Reader

	// skipTextAttachmentDecoding is propagated to child parts so that a
	// multipart tree read with this option keeps text attachments undecoded
	// all the way down. See ReadOptions.SkipTextAttachmentDecoding.
	skipTextAttachmentDecoding bool

	// parse is what the parser knows about this entity: the session collecting
	// the diagnostics of the parse it came from, and where it was found — its
	// MIME path and the byte positions of its header and body in the original
	// input. It is the zero value, meaning no session and an unknown position,
	// for an entity that was not parsed from an input stream, which is what New
	// and NewMultipart build. It is supplied at construction rather than
	// afterwards, because the defects observed while an entity is built have to
	// be recorded against it.
	parse parseContext
}

// New makes a new message with the provided header and body. The entity's
// transfer encoding and charset are automatically decoded to UTF-8.
//
// If the message uses an unknown transfer encoding or charset, New returns an
// error that verifies IsUnknownEncoding or IsUnknownCharset respectively, but
// also returns an Entity that can be read. When both are unknown, the returned
// error reports the charset.
func New(header Header, body io.Reader) (*Entity, error) {
	return newWithOptions(header, body, false, parseContext{})
}

// A body is decoded in two stages, transfer encoding and then charset, and
// either can be the one with no decoder. The Action of each warning names what
// that stage did instead, because what Entity.Body ends up holding depends on
// which stage was skipped and on what the other one did.
//
// Neither action promises that Body is the raw bytes of the wire. Skipping the
// charset stage leaves whatever the transfer stage produced, which for a
// base64 part is decoded data; skipping the transfer stage leaves bytes the
// charset stage may still have converted, because that stage runs either way.
const (
	// undecodedTransferAction reports that the transfer decoding stage was
	// skipped: its input was passed on unchanged, and is what any later charset
	// stage received.
	undecodedTransferAction = "kept the original transfer encoded bytes and passed them to any later charset stage"

	// undecodedCharsetAction reports that the charset conversion stage was
	// skipped: what the preceding transfer decoding stage produced is what the
	// body yields.
	undecodedCharsetAction = "kept the bytes produced by the preceding transfer decoding stage without charset conversion"
)

// newWithOptions is like New but, when skipTextAttachmentDecoding is true, skips
// charset decoding for text/* parts whose Content-Disposition is "attachment".
//
// ctx is what the parser knows about this entity: the session its diagnostics
// belong to, and where it was found. It is taken as an argument rather than
// assigned to the entity afterwards because the two defects this function can
// observe — an unknown transfer encoding and an unknown charset — are observed
// here, and a diagnostic recorded without that context could be given neither a
// MIME path nor a position in the input. The zero parseContext is what New and
// NewMultipart pass: it has no session, so recording is a no-op and nothing
// about those entry points changes.
//
// Both defects are reported. They are independent — a part may well declare an
// encoding and a charset that are both unknown — so each is recorded, in the
// order they are discovered, and neither hides the other. The returned error
// keeps its long-standing meaning and reports only the last of them, so that
// IsUnknownCharset and IsUnknownEncoding behave exactly as they always have;
// the session is where a caller finds everything that was observed.
func newWithOptions(header Header, body io.Reader, skipTextAttachmentDecoding bool, ctx parseContext) (*Entity, error) {
	var encoding, charset string

	var err error

	// Read through the context rather than off the header, so that the
	// parameters of this field are bounded by this parse's budgets — and, when
	// one of them refuses part of the field, so that what is kept is recorded
	// rather than quietly dropped. It is Header.ContentType for a parse with no
	// budgets, which is every entry point but Parse.
	//
	// A parse cancelled while the field was being interpreted interprets none of
	// it: the media type is empty, there are no parameters, and the error is the
	// parse's stop, so no partial reading is mistaken for a complete one. The
	// body is then left as the input held it, as for any stop below.
	//
	// The header block's line endings come first: they are how the block was
	// framed, before anything its fields say. Under ProfileStrict a block framed
	// by a bare '\r', or whose last line the input ended before any line ending,
	// is refused on the terms of a refused Content-Type, which is then not
	// interpreted at all; see line_endings.go. So is one that began with a byte
	// order mark.
	//
	// A header block that began with a UTF-16 or UTF-32 byte order mark was not
	// read, so the entity is opaque, under every entry point and whatever
	// fields the header has been given since: no media type, no transfer
	// encoding and no charset are interpreted, and the body is the bytes from
	// the mark on, as they were read. A Content-Disposition a caller gave a
	// copy of such a header is still read as usual, when asked. See bom.go.
	wide := wideMark(header.ByteOrderMark())
	var ct fieldReading
	if lineErr := ctx.headerLineEndings(&header.Header); lineErr != nil {
		ct = fieldReading{err: lineErr}
	} else if wide {
		ct = fieldReading{err: wideHeaderError{mark: header.ByteOrderMark()}}
	} else {
		ct = ctx.contentType(header)
	}
	mediaType, mediaParams, mediaErr := ct.value, ct.params, ct.err

	// A leaf body — one MultipartReader will not frame into parts — is held to
	// the body budgets of this parse. Its wire bytes go through the encoded
	// limiter, which is also what WriteTo copies verbatim, and whatever the
	// decoders below make of them goes through the guard set up at the end,
	// which also reports what ended the body whatever the decoders made of it.
	// A leaf is exactly an entity MultipartReader will return nil for. A framed
	// multipart body is a raw view of its parts and is left alone: its parts
	// are leaves of their own. A parse that budgets nothing — every entry point
	// but Parse — gets body unchanged. See limit_body.go.
	var leaf *leafBody
	if !strings.HasPrefix(mediaType, "multipart/") || ct.unframed {
		body, leaf = ctx.limitBody(body)
	}
	originalBody := body

	// The disposition is captured now, as it is, and interpreted only if
	// something asks: the charset stage below when it has to know whether this
	// is an attachment, or later EffectiveContentDisposition, which is what
	// mail.Reader classifies a part by. Whichever asks first reads it, under
	// this parse's budgets, and every later answer is that same reading, so a
	// field past a budget is diagnosed once however many times it is asked
	// about, and a field no one asks about costs nothing and reports nothing.
	disposition := newDispositionSnapshot(ctx, header)

	// A sink that stopped the parse stopped all of it, so nothing further is
	// interpreted here: the declared metadata is still read, because it
	// describes the bytes rather than interprets them, but no decoder is set up
	// and no defect is looked for. What this scope recovered — the header, and
	// the body as it appeared in the input — is returned with the fatal error.
	fatal := ctx.fatalErr()

	// Under ProfileStrict a Content-Type whose parameters would need repair is
	// not interpreted, and the refusal is this entity's alone: a FatalError
	// scoped to the entity, which leaves the parse, the parent and the siblings
	// running. It stops interpretation here just as a stop of the whole parse
	// does — no decoder is set up and the body is what the input held — but it
	// is returned only when the parse itself has not been stopped, since a stop
	// the caller asked for describes more than one entity does. See
	// metadata_repair.go.
	var local error
	if IsFatal(mediaErr) {
		local = mediaErr
	}

	// inputErr is an ordinary failure of the body's input that an eager
	// CharsetReader ran into; see the charset stage below.
	var inputErr error

	// QUIRK: RFC 2045 section 6.4 specifies that multipart messages can't have
	// a Content-Transfer-Encoding other than "7bit", "8bit" or "binary".
	// However some messages in the wild are non-conformant and have it set to
	// e.g. "quoted-printable". So we just ignore it for multipart.
	// See https://github.com/emersion/go-message/issues/48
	//
	// An opaque entity — one whose header block was not read — has no transfer
	// encoding either, whatever its header was given since.
	if !strings.HasPrefix(mediaType, "multipart/") && !wide {
		encoding = header.Get("Content-Transfer-Encoding")
		if fatal == nil && local == nil {
			// A line-framed encoding is watched as it is read, so that an
			// encoded line past Limits.MaxLineBytes is refused while the body
			// streams rather than after it has been accumulated. The monitor
			// goes under the decoder because the lines it counts are the
			// input's; see parseContext.watchCTELines, which returns body
			// unchanged when this parse has no such budget or the encoding has
			// no line framing.
			body = ctx.watchCTELines(encoding, body, leaf.shared())
			if decoded, encErr := encodingReader(encoding, body); encErr != nil {
				warning := UnknownEncodingError{encErr}
				err = warning
				fatal = ctx.recordHeader(Diagnostic{
					Code:     CodeCTEUnknown,
					Severity: SeverityUnsupported,
					Message:  fmt.Sprintf("no decoder for content transfer encoding %q", encoding),
					Field:    "Content-Transfer-Encoding",
					Action:   undecodedTransferAction,
					Cause:    warning,
				})
			} else {
				body = decoded
			}
		}
	}

	// RFC 2046 section 4.1.2: charset only applies to text/*
	if strings.HasPrefix(mediaType, "text/") {
		if ch, ok := mediaParams["charset"]; ok {
			// Record the charset even when decoding is skipped, so WriteTo can
			// still copy the original body through verbatim (see writeBodyTo).
			charset = ch
			skip := false
			if fatal == nil && skipTextAttachmentDecoding {
				// Whether this is an attachment is read from Content-Disposition
				// under the parameter budgets, which may record a diagnostic, and a
				// sink may stop the parse while it is told about it. That stop is
				// the session's, so it is taken up here like any other: it is
				// returned with the entity, and no charset decoder is set up
				// after it.
				skip = disposition.isAttachment()
				fatal = ctx.fatalErr()
				// A Content-Disposition that ProfileStrict refused to repair
				// leaves it unknown whether this is an attachment, so the
				// charset stage, which depends on that, does not run either.
				if _, _, dispErr := disposition.get(); IsFatal(dispErr) {
					local = dispErr
				}
			}
			if fatal == nil && local == nil && !skip {
				// The stage's input is counted for the body guard, which must see
				// a CharsetReader's reader take bytes a transfer decoder already
				// holds; see bodyGuard.progress.
				converted, charsetErr := charsetReader(charset, leaf.countCharsetInput(body))
				// A CharsetReader may read its input while it builds a decoder,
				// and a stop of the parse it ran into — a cancellation, or the
				// input stalling or overrunning its budget under it — makes it
				// fail for a reason that is not the charset's. So a stopped parse
				// does not record its failure as an unknown charset: the entity
				// is returned with the parse's stop instead, its body the bytes
				// before this stage. A decoder it did build is kept, since
				// nothing it already read can be given back. Nothing was
				// published before the hook ran, so a stop found here is one it
				// ran into. See cancel.go and progress.go.
				//
				// A hook that failed because the body's own input failed under it
				// is not an unknown charset either (ROB-010-006). Nothing read
				// this body before the hook, so whatever its slot holds now the
				// hook ran into. A local refusal of the body — its encoded budget —
				// is returned as the entity's own FatalError, scoped like a strict
				// refusal: the parse and the siblings go on. An ordinary failure of
				// the source is returned as itself, or joined with the hook's own
				// error when the hook did not simply pass it on; it is not made a
				// stop. Either way the body stays as the input held it, and what
				// the hook consumed is gone. A hook that built its decoder anyway
				// is kept, and its body reports the failure when it ends.
				if stop := ctx.fatalErr(); stop != nil {
					fatal = stop
					if charsetErr == nil {
						body = converted
					}
				} else if hookErr, ok := charsetErr.(UnknownCharsetError); ok && leaf != nil && leaf.slot.err != nil {
					local = leaf.slot.err
				} else if ok && leaf != nil && leaf.slot.ioErr != nil {
					inputErr = leaf.slot.inputEnd(hookErr.e)
				} else if charsetErr != nil {
					warning := UnknownCharsetError{charsetErr}
					err = warning
					fatal = ctx.recordHeader(Diagnostic{
						Code:     CodeCharsetUnknown,
						Severity: SeverityUnsupported,
						Message:  fmt.Sprintf("no decoder for charset %q", charset),
						Field:    "Content-Type",
						Action:   undecodedCharsetAction,
						Cause:    warning,
					})
				} else {
					body = converted
				}
			}
		}
	}

	// The guard goes over whatever stages ran, including none, so that what
	// Body delivers is what the decoded budgets count.
	body = ctx.guardBody(body, leaf)

	// A parse started by ParseContext gates both readers a caller can reach —
	// Body, and the raw body WriteTo copies verbatim — on its context, whether
	// the entity is a leaf or a framed multipart container, whose raw view is
	// gated like any Body and whose parts are each gated as entities. Bytes a
	// buffered reader or a decoder holds ahead are then not delivered once the
	// cancellation has been seen. A parse that cannot be cancelled gets both
	// unchanged. See cancel.go.
	//
	// A framed multipart container keeps its ungated body for its own framing
	// only, which no caller reads; see Entity.framing.
	var framing io.Reader
	if ctx.session.cancellable() {
		if strings.HasPrefix(mediaType, "multipart/") && !ct.unframed {
			framing = body
		}
		gated := ctx.session.gateReader(body)
		if sameReader(originalBody, body) {
			originalBody = gated
		} else {
			originalBody = ctx.session.gateReader(originalBody)
		}
		body = gated

		// A cancellation seen while this entity was being built — by a
		// CharsetReader, by the disposition the charset stage read — is returned
		// with it, as any stop is, rather than first being noticed by its body.
		if fatal == nil {
			fatal = ctx.session.cancelErr()
		}
	}

	e := &Entity{
		Header:                     header,
		Body:                       body,
		mediaType:                  mediaType,
		mediaParams:                mediaParams,
		mediaErr:                   mediaErr,
		unframed:                   ct.unframed,
		framing:                    framing,
		disposition:                disposition,
		originalBody:               originalBody,
		decodedBody:                body,
		encoding:                   encoding,
		charset:                    charset,
		skipTextAttachmentDecoding: skipTextAttachmentDecoding,
		parse:                      ctx,
	}

	// The entity is usable either way, so it is returned either way. A caller
	// that asked the parse to stop is told that first: a warning describes this
	// entity, but the fatal error describes the whole parse, and the diagnostic
	// the warning would have pointed at has already been delivered.
	if fatal != nil {
		return e, fatal
	}
	if local != nil {
		return e, local
	}
	if inputErr != nil {
		return e, inputErr
	}
	return e, err
}

// NewMultipart makes a new multipart message with the provided header and
// parts. The Content-Type header must begin with "multipart/".
//
// Neither decoding stage applies to such an entity: a multipart body is not
// transfer decoded, and a charset applies only to text/*. A header that meets
// the requirement above therefore produces neither of the decoding warnings
// that New can return.
//
// The body is generated lazily, when Body is first read or the entity is
// written, so a header or a part the writer refuses — a Content-Type that does
// not parse (ROB-120-008) — is reported then, not here, and every later read of
// Body reports the same error. What has been produced by then depends on whose
// header it is. The entity's own header is refused before anything: the first
// read of Body yields no bytes, and WriteTo writes nothing. A part's header is
// found only when the writer reaches that part. Body carries the parts alone —
// never the entity's own header — so it yields the bytes of the parts before
// the refused one, then the error. WriteTo has by then written the entity's
// header as well as those parts to w. Nothing already produced is withdrawn.
//
// Body, which is also the MultipartReader of such an entity, implements
// io.Closer. The body's worker goroutine starts on the first Read and exits once
// the body has been read to its end or has failed, or once Close is called; a
// caller that stops reading early calls Close to release it. Close ends both
// Body and the iteration over the parts: NextPart then reports io.EOF and Read
// io.ErrClosedPipe — except for a body whose setup had already failed when it
// was closed, which keeps reporting that failure — and a body closed before its
// first Read has finished setting it up never starts a worker and reports no
// setup failure. Close may be called from another goroutine while Body is
// read, or from a CharsetReader the setup calls; it never waits, and it wakes
// a Read waiting for that setup. It does not close the parts or their sources,
// and it cannot make a part's own
// body return: a worker blocked reading a part whose source is blocked exits
// only once that read returns. A body abandoned partway without Close keeps its
// worker waiting, holding the parts. WriteTo writes the parts itself and is not
// affected by Close.
//
// A part from ParseContext whose parse was cancelled ends the body with that
// parse's stop: a part the writer has not reached yet is not started, and one
// being written stops at the next read of its body.
func NewMultipart(header Header, parts []*Entity) (*Entity, error) {
	r := &multipartBody{
		header: header,
		parts:  parts,
	}

	return New(header, r)
}

const defaultMaxHeaderBytes = 1 << 20 // 1 MB

// errHeaderTooBig is what the limited reader below stops with, which is what
// the legacy read path fails with when a message header block runs past its
// budget.
//
// It is textproto's sentinel, so that a header budget failure reads as one
// wherever it came from: errors.Is finds it here and in the HeaderLimitError
// that textproto.ReadHeaderLimit refuses a block with. What it does not do is
// identify whose budget was reached — only that error does, which is why
// classification tests for it and not for this. See rootHeaderError.
var errHeaderTooBig = textproto.ErrHeaderTooBig

// limitedReader is the same as io.LimitedReader, but returns a custom error.
type limitedReader struct {
	R io.Reader
	N int64
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	if lr.N <= 0 {
		return 0, errHeaderTooBig
	}
	if int64(len(p)) > lr.N {
		p = p[0:lr.N]
	}
	n, err := lr.R.Read(p)
	lr.N -= int64(n)
	return n, err
}

// ReadOptions are options for ReadWithOptions.
type ReadOptions struct {
	// MaxHeaderBytes limits the maximum permissible size of a message header
	// block. If exceeded, an error will be returned.
	//
	// Set to -1 for no limit, set to 0 for the default value (1 MiB).
	//
	// This is the older spelling of Limits.MaxHeaderBytes: one budget, the same
	// units, the same meaning for 0 and -1. ReadWithOptions reads this field
	// alone and keeps its long-standing behavior, in which any negative value
	// means no limit. Parse resolves both spellings together and is stricter
	// about the rest of that range; see ParseOptions.Limits.
	//
	// What the budget reaches also differs between the two, and deliberately so.
	// Read and ReadWithOptions apply it to the message's own header block and to
	// no other: the header of a part is not budgeted on these paths, as it never
	// has been. Parse applies it to the header of every entity it reaches, the
	// message's and every part's at any depth. A caller that wants a part's
	// header bounded uses Parse.
	MaxHeaderBytes int64

	// SkipTextAttachmentDecoding, when true, leaves text/* parts whose
	// Content-Disposition is "attachment" undecoded: their charset is not
	// converted to UTF-8 and their body is returned as-is. This preserves the
	// exact bytes of text attachments whose declared charset may be wrong or
	// invalid. By default (false) every text/* part is decoded to UTF-8.
	SkipTextAttachmentDecoding bool
}

// withDefaults returns a sanitised version of the options with defaults/special
// values accounted for.
func (o *ReadOptions) withDefaults() *ReadOptions {
	var out ReadOptions
	if o != nil {
		out = *o
	}
	if out.MaxHeaderBytes == 0 {
		out.MaxHeaderBytes = defaultMaxHeaderBytes
	} else if out.MaxHeaderBytes < 0 {
		out.MaxHeaderBytes = math.MaxInt64
	}
	return &out
}

// ReadWithOptions see Read, but allows overriding some parameters with
// ReadOptions.
//
// If the message uses an unknown transfer encoding or charset, ReadWithOptions
// returns an error that verifies IsUnknownCharset or IsUnknownEncoding, but
// also returns an Entity that can be read.
func ReadWithOptions(r io.Reader, opts *ReadOptions) (*Entity, error) {
	return readWithOptions(r, opts, nil)
}

// readWithOptions is ReadWithOptions with the parse session made explicit. It
// is the seam the diagnostics work is built on: the entity it returns, and
// every part reached through it, carries session, so that a defect discovered
// while the caller consumes the message — long after this function has
// returned — is recorded against the parse it belongs to.
//
// session may be nil, and is nil for every entry point but Parse: Read and
// ReadWithOptions keep their long-standing signatures, so they have nowhere to
// return a session to and allocate none. Recording through a nil session is a
// no-op, so the threading costs those callers nothing and changes nothing they
// observe.
func readWithOptions(r io.Reader, opts *ReadOptions, session *ParseSession) (*Entity, error) {
	// Reading a message is a scope of this parse, and the outermost one, so it
	// does not begin on a session that has already been stopped. Nothing has
	// been recovered at this point — not even a header — so there is nothing to
	// hand back with the error. Parse always starts from a fresh session, so
	// this only fires for a session reused across messages.
	if err := session.fatalErr(); err != nil {
		return nil, err
	}

	opts = opts.withDefaults()

	// The input budget goes under the counting reader, at the raw source, so
	// that every read of this parse — header, body, every part and every byte a
	// multipart iterator skips — shares it and is counted once, and so that a
	// byte it refuses is never one a position is measured against. A parse that
	// budgets nothing, which is Read and ReadWithOptions, reads r as it is; see
	// limit_input.go.
	//
	// Under the input budget, at the very source, sits the cancellation gate of
	// a parse started by ParseContext, so that once its context is seen to be
	// done the input is never read again, and so that a read the gate cut short
	// still counts every byte it delivered. A parse that cannot be cancelled
	// reads r as it is; see cancel.go.
	//
	// Under the gate, directly over r, a parse counts the reads of r that make
	// no progress, so that the count sees every read whatever buffers, decoders
	// or copies sit above. Read and ReadWithOptions read r as it is; see
	// progress.go.
	cr := &countingReader{r: session.limitInput(session.gateReader(session.watchProgress(r)))}
	lr := &limitedReader{R: cr, N: math.MaxInt64}
	br := bufio.NewReader(lr)

	// A parse asked to recognize an mbox envelope line looks for one at the
	// first byte of the input, before the header block, and keeps it apart:
	// the block then begins after it. Every other parse, and one whose input
	// does not begin with such a line, reads the block from where it stands,
	// with nothing consumed. A stop met with a line recognized is returned
	// here, the line kept. See mbox.go.
	headerOffset, err := session.readMboxEnvelope(br)
	if err != nil {
		return nil, err
	}

	// The message's own header block is held to the same budget as every other
	// entity's when there is a parse to hold it to one, and to the one it has
	// always been held to otherwise. The two differ in where the budget sits,
	// which is what makes them differ at all:
	//
	//   - A parse budgets the block itself, so that it is refused for its own
	//     wire bytes, exactly as the header of every part below it is, and so
	//     that overrunning it is reported as the budget failure it is rather
	//     than as whatever a shortened read happened to look like.
	//   - Read and ReadWithOptions keep shortening the reads under the header
	//     parser, which is the mechanism their documented behavior was written
	//     against, down to which inputs it turns away and with which error.
	//
	// Either way the budget applies to the header block alone: the limit is
	// lifted before a byte of body is read, and the body's own bytes are never
	// what a header budget refuses.
	var headerLimits textproto.HeaderLimits
	if session != nil {
		headerLimits = session.headerLimits()
		// The two spellings of the header byte budget are reconciled before a
		// parse begins, and ReadOptions carries the result; see resolveConfig.
		// It is used rather than the session's copy of the same budget so that
		// the block is held to the value that reconciliation settled.
		headerLimits.MaxBytes = opts.MaxHeaderBytes
	} else {
		lr.N = opts.MaxHeaderBytes
	}

	h, err := textproto.ReadHeaderLimits(br, headerLimits)
	if err != nil {
		// The input budget can end the parse while the header block is still
		// being read, and its stop is the session's: it outranks whatever the
		// header reader made of the failed read, and is returned as the
		// identical value every other scope reports. The block did not end
		// within the budget, so there is no header to return.
		if stop := session.fatalErr(); stop != nil {
			return nil, stop
		}
		// The parse's shared header budget ran out on this block; its failure
		// is the whole parse's. See limit_header_total.go.
		if ref, ok := session.headerTotalRefusal(err); ok {
			return nil, session.headerTotalFailure(rootContextAt(session, headerOffset, headerOffset), ref)
		}
		return nil, rootHeaderError(err, session, headerOffset)
	}

	lr.N = math.MaxInt64

	// The body starts where the header parser stopped: what the buffered
	// reader pulled in, less what it still holds beyond that point. It is read
	// before the body is wrapped in decoders, so that a decoder reading ahead
	// on construction could not move it.
	bodyOffset := cr.n - int64(br.Buffered())

	return newWithOptions(Header{h}, br, opts.SkipTextAttachmentDecoding, rootContextAt(session, headerOffset, bodyOffset))
}

// Read reads a message from r. The message's encoding and charset are
// automatically decoded to raw UTF-8. Note that this function only reads the
// message header.
//
// If the message uses an unknown transfer encoding or charset, Read returns an
// error that verifies IsUnknownCharset or IsUnknownEncoding, but also returns
// an Entity that can be read.
func Read(r io.Reader) (*Entity, error) {
	return ReadWithOptions(r, nil)
}

// MultipartReader returns a MultipartReader that reads parts from this entity's
// body. If this entity is not multipart, it returns nil.
//
// It also returns nil for an entity from Parse whose multipart Content-Type had
// to be repaired and kept no boundary that may be used — a conflicting or
// damaged boundary parameter, or one an unterminated quote swallowed. Such a
// body cannot be framed without choosing or inventing a boundary, so it is kept
// as one opaque part: Body yields its bytes as they were read, and the media
// type EffectiveContentType reports is still the multipart one the field
// declares. A diagnostic of SeverityInvalidMetadata was recorded for it. See
// metadata_repair.go.
//
// Under Parse the body of an entity this frames is a raw view of its parts,
// outside the per-body and total decoded budgets: the parts it yields are held
// to those budgets one by one, and reading Body directly instead is bounded by
// Limits.MaxInputBytes alone. A body kept opaque as above is a leaf, held to the
// body budgets like any other. See Limits.MaxEncodedBodyBytes.
func (e *Entity) MultipartReader() MultipartReader {
	if !strings.HasPrefix(e.mediaType, "multipart/") || e.unframed {
		return nil
	}
	if mb, ok := e.Body.(*multipartBody); ok {
		return mb
	}

	parent := e.parse
	src := e.Body
	if !sameReader(e.Body, e.decodedBody) {
		// The caller replaced the body after this entity was parsed, so what
		// is about to be iterated is not the input's parts. Neither their
		// positions nor their place in the parsed tree can be attributed to
		// the input, so the parts are given no origin rather than a wrong one.
		// They stay part of this parse, so the session travels down anyway.
		parent = parent.detached()
		// The framing reads the caller's reader through this parse's
		// cancellation gate, so that once the parse is cancelled the framing
		// asks nothing more of it — and nor does the framing of any part
		// inside, which reads through this one. Body itself stays the
		// caller's reader, ungated, as cancel.go says; only the parse's own
		// reads of it are gated. No input budget applies to it, and its
		// parts get no origin.
		src = parent.session.gateReader(e.Body)
	} else if e.framing != nil {
		// The parse's own framing reads the body as the parse read it, behind
		// no cancellation gate of this entity's; see Entity.framing.
		src = e.framing
	}

	r := textproto.NewMultipartReader(src, e.mediaParams["boundary"])

	// Every entity's header block is budgeted, and each gets the whole of every
	// budget: the parts of this body are entities of the same parse, so the
	// header of one is bounded exactly as the message's own header was. The
	// line budget reaches past the header blocks to the framing lines between
	// the parts, which is the other place this layer has to read a line as a
	// line; see textproto.MultipartReader.SetHeaderLimits. A parse that budgets
	// nothing — which is every entry point but Parse — passes nothing down, and
	// the parts of a message read that way stay as unbounded as they have
	// always been.
	r.SetHeaderLimits(parent.headerLimits())

	return &multipartReader{
		r:                          r,
		skipTextAttachmentDecoding: e.skipTextAttachmentDecoding,
		parent:                     parent,
	}
}

// sameReader reports whether a and b are the same reader value. It detects
// whether the caller reassigned Entity.Body after construction. Comparing
// interface values panics if their dynamic type is not comparable, so that is
// recovered and treated as "not the same" — the safe default of not taking the
// verbatim fast path.
func sameReader(a, b io.Reader) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// writeBodyTo writes this entity's body to w (without the header).
func (e *Entity) writeBodyTo(w *Writer) error {
	var err error
	if mb, ok := e.Body.(*multipartBody); ok {
		err = mb.writeBodyTo(w)
	} else if e.originalBody != nil && sameReader(e.Body, e.decodedBody) && strings.EqualFold(w.encoding, e.encoding) && strings.EqualFold(w.charset, e.charset) {
		// The output transfer encoding and charset match the input, so copy the
		// original (undecoded) body verbatim instead of decoding and
		// re-encoding it. This preserves the exact bytes (e.g. for DKIM body
		// hashes) that re-encoding could otherwise alter.
		_, err = io.Copy(w.rawWriter, e.originalBody)
	} else {
		_, err = io.Copy(w, e.Body)
	}
	return err
}

// WriteTo writes this entity's header and body to w.
//
// It writes through CreateWriter, so an entity whose own Content-Type does not
// parse is refused with an error before anything is written (ROB-120-008). A
// part of a body built by NewMultipart is written through CreatePart once the
// writer reaches it, so a part whose Content-Type does not parse fails WriteTo
// after this entity's header and the parts before it are already on w; they
// are not withdrawn. The guarantee of no output covers this entity's own
// header, not its parts. The refusal of the entity's own header
// includes an entity from Parse whose damaged Content-Type the parse repaired
// for interpretation: the repair is of the entity's view, never of its header,
// which keeps the input's bytes, and the writer does not serialize a field it
// cannot interpret. Replaying such an entity's raw bytes is not what WriteTo
// does; raw replay, including of a body already consumed, is #41's.
//
// WriteTo serializes the entity; it does not replay the input. It adds a
// "Mime-Version: 1.0" field at the top of a header that has no MIME-Version,
// always ends the header with one empty line, and otherwise writes the header
// as CreateWriter does, which may for instance give a multipart a usable
// boundary or drop its Content-Transfer-Encoding. So an empty input is written
// as "Mime-Version: 1.0\r\n\r\n", and unmodified ordinary fields that io.EOF
// ended are written raw and in order after that added field, followed by the
// empty line (ROB-010-001); for such input, parsing what WriteTo wrote and
// writing it again gives the same bytes.
//
// A header read after a UTF-8 byte order mark is written with the mark first,
// before the MIME-Version field WriteTo may add. An entity whose header block
// began with a UTF-16 or UTF-32 mark was not read as a header, and WriteTo
// refuses it with an error before writing anything; its Body is left unread
// and stays usable, every byte from the mark on. Under a body built by
// NewMultipart such a part is refused when the writer reaches it, after the
// entity's header and the parts before it have been written. See bom.go;
// nothing here transcodes or replays the input (ROB-010-003).
//
// For an entity from ParseContext whose context is already done when WriteTo
// is called, nothing is written and the parse's stop is returned. A
// cancellation that comes later is seen by the reads of the body, before and
// after each: the header — and, for a body built by NewMultipart, the parts
// before it — may already be on w, as may the bytes the read that saw the
// cancellation still delivered, and nothing read after that is written. A write
// to w already in progress is the caller's and is not interrupted. Whatever was
// written is not withdrawn, and WriteTo returns the stop. See cancel.go.
func (e *Entity) WriteTo(w io.Writer) error {
	if stop := e.parse.session.cancelErr(); stop != nil {
		return stop
	}
	ew, err := CreateWriter(w, e.Header)
	if err != nil {
		return err
	}

	if err := e.writeBodyTo(ew); err != nil {
		ew.Close()
		return err
	}

	return ew.Close()
}

// WalkFunc is the type of the function called for each part visited by Walk.
//
// The path argument is a list of multipart indices leading to the part. The
// root part has a nil path.
//
// If there was an encoding error walking to a part, the incoming error will
// describe the problem and the function can decide how to handle that error.
// Such an error is only ever forwarded together with the part it describes, so
// entity is never nil: an error that arrives without a part — a child a budget
// refused, for instance — is not handed to the function at all, and Walk
// returns it.
//
// Unlike IMAP part paths, indices start from 0 (instead of 1) and a
// non-multipart message has a nil path (instead of {1}).
//
// If an error is returned, processing stops.
type WalkFunc func(path []int, entity *Entity, err error) error

// Walk walks the entity's multipart tree, calling walkFunc for each part in
// the tree, including the root entity.
//
// Walk consumes the entity.
//
// A traversal is a scope of the parse the entity came from, so a DiagnosticSink
// that stopped that parse stops this one too. Walk returns the identical fatal
// error the rest of the parse reports, and it observes the stop before calling
// walkFunc rather than after: a caller that asked the parse to end is not handed
// further parts. A walk stopped partway through keeps nothing back — every part
// walkFunc was already given stays valid and owned by the caller — and a walk
// begun on an already stopped parse visits nothing at all, not even the root.
//
// An entity that was not parsed with a session, which is every entity but one
// from Parse, has no parse that could be stopped, and Walk behaves exactly as
// it always has.
//
// For an entity from ParseContext, the cancellation of the parse's context is
// such a stop, and Walk sees it at the same boundaries. A walkFunc already
// running is not interrupted; the bodies it reads report the stop.
//
// Under ProfileStrict an entity refused for a valid base with malformed
// parameters ends the walk with its entity-scoped FatalError, before walkFunc
// is given it: a child whose iterator returns the refusal, the root itself when
// its Content-Type was refused, and any entity whose Content-Disposition was
// refused when it was read — by the charset stage, by a caller before the walk,
// or by walkFunc during it, in which case the walk ends after walkFunc returns.
// Walk asks only about what has been observed and reads no field for the
// purpose. A stop of the whole parse outranks the refusal. The parse is not
// stopped by it: the refused entity and its raw body stay the caller's, and a
// caller that wants the siblings after a refused part iterates MultipartReader
// directly, where the next call continues with them.
func (e *Entity) Walk(walkFunc WalkFunc) error {
	var multipartReaders []MultipartReader
	// containers holds the entity whose parts each reader yields, so that a
	// refusal observed for one of them after the walk descended into it — a
	// walkFunc reading the disposition of an ancestor it captured — ends the
	// walk before another of its parts is read. Only a strict parse can refuse
	// anything, so only a strict parse keeps them.
	var containers []*Entity
	strict := e.parse.session != nil && e.parse.session.Profile() == ProfileStrict
	var path []int
	part := e
	for {
		// The scope's entry check and its safe progress boundary are the same
		// point: on the first pass nothing has been visited yet, and on every
		// later one the parts already handed to walkFunc are a complete result
		// as it stands. Checking here is what keeps a stop from reaching
		// walkFunc, since NextPart is not consulted for the root and a part
		// already in hand would otherwise be delivered.
		if err := e.parse.fatalErr(); err != nil {
			return err
		}
		// Asking each container costs one look at what has been observed, so a
		// strict walk pays the depth of the tree per part, which
		// Limits.MaxMIMEDepth bounds; nothing is read for the purpose.
		for _, c := range containers {
			if fail := c.localFailure(); fail != nil {
				return fail
			}
		}

		var err error
		if part == nil {
			if len(multipartReaders) == 0 {
				break
			}

			// Get the next part from the last multipart reader
			mr := multipartReaders[len(multipartReaders)-1]
			part, err = mr.NextPart()
			if err == io.EOF {
				multipartReaders = multipartReaders[:len(multipartReaders)-1]
				if strict {
					containers = containers[:len(containers)-1]
				}
				path = path[:len(path)-1]
				continue
			} else if part != nil && isWarning(err) {
				// Forward the warning to walkFunc, together with the usable
				// part it describes. A part's warning is returned as itself, so
				// only err's own type is asked about: whatever the warning wraps
				// came from a CharsetReader or the decoders, and is not walked.
				//
				// A warning always arrives with its part, so an error with no
				// part is not a warning about anything walkFunc could be
				// handed. A scope that refused a child returns no part, and its
				// FatalError wraps whatever a sink returned or the input failed
				// with, which may itself wrap a warning; forwarding that would
				// hand walkFunc a nil entity and then descend into it. It ends
				// the walk as itself instead, below.
			} else if err != nil {
				return err
			}

			// A part that would be forwarded may belong to a parse that has been
			// stopped meanwhile. A sink that stops the parse over a part's own
			// warning makes the part come back with the session's stop rather
			// than the warning, which the test above already returns, but the
			// stop is the caller ending the parse, not a warning about a part,
			// so it outranks the forwarding wherever it came from and is
			// returned — the identical value every other scope reports — before
			// walkFunc is handed the part. Asking the session rather than err
			// is what makes this exact: an unstopped parse reports nil whatever
			// err claims to be.
			if stop := e.parse.fatalErr(); stop != nil {
				return stop
			}

			path[len(path)-1]++
		}

		// Copy the path since we'll mutate it on the next iteration
		var pathCopy []int
		if len(path) > 0 {
			pathCopy = make([]int, len(path))
			copy(pathCopy, path)
		}

		// An entity whose own metadata was refused under ProfileStrict ends the
		// walk with that refusal, as a refused child already does when its
		// iterator returns it. The root has no iterator to return it, so it is
		// asked here, and so is a part whose disposition was refused when it was
		// first read: by the charset stage, or by any caller before the walk got
		// to it. Only what has been observed is asked about — nothing is read for
		// the purpose — and the walk is all that ends: the parse is not stopped,
		// and the entity, its raw body included, stays the caller's.
		if fail := part.localFailure(); fail != nil {
			return fail
		}

		if err := walkFunc(pathCopy, part, err); err != nil {
			return err
		}

		// walkFunc may have read the disposition for the first time, and so
		// learned of a refusal, or had a sink stop the parse while it was told
		// about one. The stop outranks the refusal, as everywhere else.
		if stop := e.parse.fatalErr(); stop != nil {
			return stop
		}
		if fail := part.localFailure(); fail != nil {
			return fail
		}

		if mr := part.MultipartReader(); mr != nil {
			multipartReaders = append(multipartReaders, mr)
			if strict {
				containers = append(containers, part)
			}
			path = append(path, -1)
		}

		part = nil
	}

	return nil
}
