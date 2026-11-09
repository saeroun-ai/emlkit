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

	mediaType   string
	mediaParams map[string]string

	// originalBody is the undecoded body as passed to New, together with the
	// transfer encoding and charset it was encoded with. WriteTo copies the
	// body through verbatim using these when the output encoding/charset are
	// unchanged, preserving the exact bytes (e.g. for DKIM body hashes).
	// originalBody shares the underlying reader with Body, so reading Body
	// consumes it too: call WriteTo before reading Body to keep this working.
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
	originalBody := body
	var encoding, charset string

	var err error

	// Read through the context rather than off the header, so that the
	// parameters of this field are bounded by this parse's budgets — and, when
	// one of them refuses part of the field, so that what is kept is recorded
	// rather than quietly dropped. It is Header.ContentType for a parse with no
	// budgets, which is every entry point but Parse.
	mediaType, mediaParams, _ := ctx.contentType(header)

	// A sink that stopped the parse stopped all of it, so nothing further is
	// interpreted here: the declared metadata is still read, because it
	// describes the bytes rather than interprets them, but no decoder is set up
	// and no defect is looked for. What this scope recovered — the header, and
	// the body as it appeared in the input — is returned with the fatal error.
	fatal := ctx.fatalErr()

	// QUIRK: RFC 2045 section 6.4 specifies that multipart messages can't have
	// a Content-Transfer-Encoding other than "7bit", "8bit" or "binary".
	// However some messages in the wild are non-conformant and have it set to
	// e.g. "quoted-printable". So we just ignore it for multipart.
	// See https://github.com/emersion/go-message/issues/48
	if !strings.HasPrefix(mediaType, "multipart/") {
		encoding = header.Get("Content-Transfer-Encoding")
		if fatal == nil {
			// A line-framed encoding is watched as it is read, so that an
			// encoded line past Limits.MaxLineBytes is refused while the body
			// streams rather than after it has been accumulated. The monitor
			// goes under the decoder because the lines it counts are the
			// input's; see parseContext.watchCTELines, which returns body
			// unchanged when this parse has no such budget or the encoding has
			// no line framing.
			body = ctx.watchCTELines(encoding, body)
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
				skip = ctx.isAttachment(header)
				fatal = ctx.fatalErr()
			}
			if fatal == nil && !skip {
				if converted, charsetErr := charsetReader(charset, body); charsetErr != nil {
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

	e := &Entity{
		Header:                     header,
		Body:                       body,
		mediaType:                  mediaType,
		mediaParams:                mediaParams,
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
	return e, err
}

// NewMultipart makes a new multipart message with the provided header and
// parts. The Content-Type header must begin with "multipart/".
//
// Neither decoding stage applies to such an entity: a multipart body is not
// transfer decoded, and a charset applies only to text/*. A header that meets
// the requirement above therefore produces neither of the decoding warnings
// that New can return.
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

	cr := &countingReader{r: r}
	lr := &limitedReader{R: cr, N: math.MaxInt64}
	br := bufio.NewReader(lr)

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
		return nil, rootHeaderError(err, session)
	}

	lr.N = math.MaxInt64

	// The body starts where the header parser stopped: what the buffered
	// reader pulled in, less what it still holds beyond that point. It is read
	// before the body is wrapped in decoders, so that a decoder reading ahead
	// on construction could not move it.
	bodyOffset := cr.n - int64(br.Buffered())

	return newWithOptions(Header{h}, br, opts.SkipTextAttachmentDecoding, rootContext(session, bodyOffset))
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
func (e *Entity) MultipartReader() MultipartReader {
	if !strings.HasPrefix(e.mediaType, "multipart/") {
		return nil
	}
	if mb, ok := e.Body.(*multipartBody); ok {
		return mb
	}

	parent := e.parse
	if !sameReader(e.Body, e.decodedBody) {
		// The caller replaced the body after this entity was parsed, so what
		// is about to be iterated is not the input's parts. Neither their
		// positions nor their place in the parsed tree can be attributed to
		// the input, so the parts are given no origin rather than a wrong one.
		// They stay part of this parse, so the session travels down anyway.
		parent = parent.detached()
	}

	r := textproto.NewMultipartReader(e.Body, e.mediaParams["boundary"])

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
func (e *Entity) WriteTo(w io.Writer) error {
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
func (e *Entity) Walk(walkFunc WalkFunc) error {
	var multipartReaders []MultipartReader
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
				path = path[:len(path)-1]
				continue
			} else if part != nil && (IsUnknownEncoding(err) || IsUnknownCharset(err)) {
				// Forward the warning to walkFunc, together with the usable
				// part it describes.
				//
				// A warning always arrives with its part, so an error that
				// answers to one of these predicates with no part is not a
				// warning about anything walkFunc could be handed. A scope that
				// refused a child returns no part, and its FatalError wraps
				// whatever a sink returned or the input failed with, which may
				// itself wrap a warning; forwarding that would hand walkFunc a
				// nil entity and then descend into it. It ends the walk as
				// itself instead, below.
			} else if err != nil {
				return err
			}

			// A part forwarded with a warning may have been the one a sink
			// stopped the parse over, and the stop it returned with is then the
			// session's: a FatalError wrapping the sink's own error, which may be
			// or wrap that very warning and so pass the test above. The stop is
			// the caller ending the parse, not a warning about a part, so it
			// outranks the forwarding and is returned — the identical value
			// every other scope reports — before walkFunc is handed the part.
			// Asking the session rather than err is what makes this exact: an
			// unstopped parse reports nil whatever err claims to be.
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

		if err := walkFunc(pathCopy, part, err); err != nil {
			return err
		}

		if mr := part.MultipartReader(); mr != nil {
			multipartReaders = append(multipartReaders, mr)
			path = append(path, -1)
		}

		part = nil
	}

	return nil
}
