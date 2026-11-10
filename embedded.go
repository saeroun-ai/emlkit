package message

import (
	"bufio"
	"bytes"
	"fmt"
	"io"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Embedded messages (ROB-090-001, ROB-090-002, ROB-090-007).
//
// A message/rfc822 or message/global entity carries a whole message, and a
// text/rfc822-headers entity the header block of one. Such an entity stays
// what it always was — a leaf whose Body yields that message's bytes, transfer
// decoded — until the caller asks for the message inside it:
//
//   - Entity.EmbeddedMessage reads the embedded header block from Body and
//     returns the embedded message as an entity of the same parse, one level
//     down: its path is the carrying entity's path followed by 0, its body and
//     its parts are read lazily from the same stream, and its diagnostics are
//     recorded under its own path.
//   - ParseOptions.WalkEmbeddedMessages makes Entity.Walk do that for every
//     such entity it visits, after walkFunc was given the carrying entity,
//     unless walkFunc read its Body to the end.
//
// Asking consumes the carrying entity's Body, as reading it does. Nothing is
// descended into unasked, so mail.Reader still returns an embedded message as
// one attachment.
//
// Limits.MaxMessageDepth bounds how deeply messages nest: the message itself
// is depth zero, and a message embedded in an entity of it is one deeper than
// the message that entity belongs to. An embedded message past the budget is
// not read: EmbeddedMessage returns an entity-scoped FatalError carrying one
// limit.message_depth diagnostic, the carrying entity and its Body stay as
// they were, and the parse goes on. The embedded message also counts as one
// part of Limits.MaxPartCount and one level of Limits.MaxMIMEDepth, and its
// header block is held to the header budgets like any other.
//
// Positions: when the carrying entity declares no transfer encoding that
// changes bytes (7bit, 8bit, binary or none), the embedded message's bytes are
// the input's, and its diagnostics carry input offsets. When it was Base64 or
// quoted-printable encoded they are not, and its diagnostics carry the path
// and no offset.

const (
	messageDepthAction     = "did not read the embedded message; the entity that carries it and its body stay as they were"
	embeddedInvalidAction  = "kept the entity as a leaf; the bytes read while looking for the embedded header are lost to its Body"
	embeddedEmptyAction    = "returned an embedded message with no header fields and an empty body"
	embeddedMessageNothing = "the entity carries no message"
)

// embedsMessage reports whether an entity of mediaType carries a message or a
// message's header block.
func embedsMessage(mediaType string) bool {
	switch mediaType {
	case "message/rfc822", "message/global", "message/news", "text/rfc822-headers", "message/rfc822-headers", "message/global-headers":
		return true
	}
	return false
}

// CarriesMessage reports whether EmbeddedMessage has a message to read: the
// entity's media type, as the parser interpreted it, is message/rfc822,
// message/global, message/news, or a header-only form of them
// (text/rfc822-headers, message/global-headers).
func (e *Entity) CarriesMessage() bool {
	return embedsMessage(e.mediaType)
}

// EmbeddedMessage returns the message this entity carries, parsed from its
// Body: the embedded header block is read now, the embedded body and parts
// lazily, as Parse reads a message. It returns nil and no error for an entity
// that carries none (see CarriesMessage).
//
// The first call consumes Body; later calls return the same entity and error.
// Read the embedded message through the returned entity — its Body, its
// MultipartReader, Walk — and not through this entity's Body, which now
// stands wherever the embedded reads left it.
//
// The embedded entity belongs to this entity's parse when it has one: its
// path is this entity's followed by 0, its defects are recorded in the same
// session, healed on the same terms, and held to the same budgets, including
// Limits.MaxMessageDepth (see embedded.go). An unknown transfer encoding or
// charset of the embedded message is returned as a warning with a usable
// entity, as Parse returns it. An entity not from Parse is read as Read reads
// a message.
func (e *Entity) EmbeddedMessage() (*Entity, error) {
	return e.embeddedMessage(false)
}

// embeddedMessage is EmbeddedMessage; walking reports that Walk asks, for
// which a body with nothing left in it — walkFunc read it — is no message and
// no defect.
func (e *Entity) embeddedMessage(walking bool) (*Entity, error) {
	if !e.CarriesMessage() {
		return nil, nil
	}
	if e.embeddedTried {
		return e.embedded, e.embeddedErr
	}
	e.embeddedTried = true
	e.embedded, e.embeddedErr = e.readEmbedded(walking)
	return e.embedded, e.embeddedErr
}

func (e *Entity) readEmbedded(walking bool) (*Entity, error) {
	c := e.parse
	if stop := c.fatalErr(); stop != nil {
		return nil, stop
	}

	// The nesting budget is checked before anything is read.
	msgDepth := c.msgDepth + 1
	if l, ok := c.limits(); ok && l.MaxMessageDepth != LimitDisabled && msgDepth > l.MaxMessageDepth {
		d := c.completeBody(Diagnostic{
			Code:     CodeLimitMessageDepth,
			Severity: SeverityLimitExceeded,
			Message:  fmt.Sprintf("embedded message is nested deeper than the limit of %d", l.MaxMessageDepth),
			Action:   messageDepthAction,
		})
		if fatal := c.recordBody(d); fatal != nil {
			return nil, fatal
		}
		return nil, newFatalDiagnostic(d)
	}

	cr := &countingReader{r: e.Body}
	br := bufio.NewReader(cr)
	limits := c.headerLimits()
	var h textproto.Header
	var unread []byte
	var err error
	if limits.Recover {
		h, unread, err = textproto.ReadHeaderRecover(br, limits)
	} else if c.session != nil {
		h, err = textproto.ReadHeaderLimits(br, limits)
	} else {
		h, err = textproto.ReadHeader(br)
	}
	if err != nil {
		if stop := c.fatalErr(); stop != nil {
			return nil, stop
		}
		c.recordBody(Diagnostic{
			Code:     CodeMessageEmbeddedInvalid,
			Severity: SeverityInvalidMetadata,
			Message:  "the header block of the embedded message could not be read",
			Action:   embeddedInvalidAction,
			Cause:    err,
		})
		return nil, err
	}

	var body io.Reader = br
	if len(unread) > 0 {
		body = io.MultiReader(bytes.NewReader(unread), br)
	}

	// Where the embedded message is: in the input when this entity's body is
	// the input's bytes, and nowhere otherwise.
	headerLen := cr.n - int64(br.Buffered()) - int64(len(unread))
	ctx := c.child(0, 0, headerLen)
	ctx.msgDepth = msgDepth
	if name, _ := canonicalCTE(e.encoding); (name != "" && name != "7bit" && name != "8bit" && name != "binary") || !sameReader(e.Body, e.decodedBody) {
		ctx.origin.unplaced = true
	}

	if h.Len() == 0 {
		if _, perr := br.Peek(1); perr == io.EOF && len(unread) == 0 {
			if walking {
				return nil, nil
			}
			if fatal := ctx.recordHeader(Diagnostic{
				Code:     CodeMessageEmbeddedTruncated,
				Severity: SeverityIncomplete,
				Message:  embeddedMessageNothing,
				Action:   embeddedEmptyAction,
			}); fatal != nil {
				return nil, fatal
			}
		}
	}

	// The embedded message is one more entity of the tree.
	var counter multipartReader
	if aerr := counter.admitPart(ctx); aerr != nil {
		return nil, aerr
	}
	return newWithOptions(Header{h}, body, e.skipTextAttachmentDecoding, ctx)
}

// An embeddedReader is the iterator Walk descends into an embedded message
// with: it yields that one message and then ends.
type embeddedReader struct {
	e    *Entity
	done bool
}

func (r *embeddedReader) NextPart() (*Entity, error) {
	if r.done {
		return nil, io.EOF
	}
	r.done = true
	emb, err := r.e.embeddedMessage(true)
	if emb == nil {
		// No message, or one that was refused or could not be read: that is
		// recorded, and is this entity's alone. Only a stop of the parse ends
		// the walk.
		if stop := r.e.parse.fatalErr(); stop != nil {
			return nil, stop
		}
		return nil, io.EOF
	}
	return emb, err
}

func (r *embeddedReader) Close() error { return nil }

// walkEmbedded returns the iterator Walk descends into e with, or nil when e
// carries no message, the parse did not ask for embedded messages to be
// walked, or nothing of e's body is left to read one from.
func (e *Entity) walkEmbedded() MultipartReader {
	if !e.CarriesMessage() || e.parse.session == nil || !e.parse.session.cfg.walkEmbedded {
		return nil
	}
	return &embeddedReader{e: e}
}
