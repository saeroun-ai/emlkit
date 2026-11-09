package mail

import (
	"container/list"
	"io"
	"strings"

	"github.com/saeroun-ai/emlkit"
)

// A PartHeader is a mail part header. It contains convenience functions to get
// and set header fields.
type PartHeader interface {
	// Add adds the key, value pair to the header.
	Add(key, value string)
	// Del deletes the values associated with key.
	Del(key string)
	// Get gets the first value associated with the given key. If there are no
	// values associated with the key, Get returns "".
	Get(key string) string
	// Set sets the header entries associated with key to the single element
	// value. It replaces any existing values associated with key.
	Set(key, value string)

	partHeader()
}

// A Part is either a mail text or an attachment. Header is either a *InlineHeader
// or an *AttachmentHeader.
type Part struct {
	Header PartHeader
	Body   io.Reader
}

// A Reader reads a mail message.
type Reader struct {
	Header Header

	e       *message.Entity
	readers *list.List
}

// NewReader creates a new mail reader.
//
// For an entity from message.Parse the reader is a scope of that parse. If a
// DiagnosticSink stopped the parse, or a budget whose scope is the whole parse
// ended it, NextPart returns the identical message.FatalError Parse and every
// other scope of it report, and no part — even when e is a single-part message
// whose only part is e itself, which Parse returned together with that error.
// Parse's error has to be handled first all the same; this is so that a reader
// built on a stopped parse cannot quietly continue it.
func NewReader(e *message.Entity) *Reader {
	mr := e.MultipartReader()
	if mr == nil {
		// Artificially create a multipart entity
		// With this header, no error will be returned by message.NewMultipart.
		//
		// This wrapper is built with message.NewMultipart, which belongs to no
		// parse, so it would hand e over whatever e's own parse had come to.
		// NextPart asks e's parse before it does anything; see there.
		var h message.Header
		h.Set("Content-Type", "multipart/mixed")
		me, _ := message.NewMultipart(h, []*message.Entity{e})
		mr = me.MultipartReader()
	}

	l := list.New()
	l.PushBack(mr)

	return &Reader{Header{e.Header}, e, l}
}

// recoverable reports whether err is a decoding warning that accompanies the
// usable entity e, rather than a failure.
//
// An unknown transfer encoding and an unknown charset are both warnings: the
// entity is fully usable and its body yields what the stage that had no decoder
// left undecoded. Anything else is a failure, and so is either warning when it
// comes without an entity, or inside a message.FatalError — a stop a
// DiagnosticSink asked for, or a resource budget, that happens to wrap one. A
// stop ends the traversal however it is described.
//
// The message package returns a warning as itself together with its entity,
// never wrapped, so only err's own dynamic type is asked about: nothing it
// wraps — a CharsetReader's error, a source's read failure, a sink's error
// inside a FatalError — is unwrapped to look further. The predicate then runs
// on that value alone and matches a genuine warning before it would unwrap
// anything; a zero UnknownEncodingError{} or UnknownCharsetError{}, which
// carries no cause and is no warning of the parser's, does not match, and
// unwrapping it calls only this module's own method.
//
// A warning is recoverable whatever its cause holds. A CharsetReader may fail
// with a FatalError from some other parse, which message.IsFatal then finds
// inside the warning, but it describes this part's undecoded charset, not a
// stop of this parse: the part is returned with the warning and the next call
// continues. A stop of this parse is the FatalError returned in place of a
// warning, or ParseStop, and ends the traversal as before.
func recoverable(e *message.Entity, err error) bool {
	if e == nil {
		return false
	}
	switch err.(type) {
	case message.UnknownEncodingError:
		return message.IsUnknownEncoding(err)
	case message.UnknownCharsetError:
		return message.IsUnknownCharset(err)
	}
	return false
}

// CreateReader reads a mail header from r and returns a new mail reader.
//
// If the message uses an unknown transfer encoding or charset, CreateReader
// returns an error that verifies message.IsUnknownEncoding or
// message.IsUnknownCharset respectively, but also returns a Reader that can be
// used; when both are unknown, the error reports the charset. The body of the
// single part such a message yields is what the skipped stage left undecoded.
// Every other error is returned with a nil Reader.
//
// CreateReader reads through message.Read, which enforces only the message
// header's byte budget. To read under the per-entity header, line, parameter
// and tree budgets, and to collect diagnostics, parse with message.Parse and
// pass the entity to NewReader.
func CreateReader(r io.Reader) (*Reader, error) {
	e, err := message.Read(r)
	if err != nil && !recoverable(e, err) {
		return nil, err
	}

	return NewReader(e), err
}

// NextPart returns the next mail part. If there is no more part, io.EOF is
// returned as error.
//
// The returned Part.Body must be read completely before the next call to
// NextPart, otherwise it will be discarded.
//
// If the part uses an unknown transfer encoding or charset, NextPart returns an
// error that verifies message.IsUnknownEncoding or message.IsUnknownCharset
// respectively, but also returns a Part that can be used, and the next call
// continues with the part after it. Every other error is returned with a nil
// Part. That includes a message.FatalError, even one wrapping either warning:
// on an entity from message.Parse it is a resource budget or a DiagnosticSink
// stop, and later calls report it again rather than continue. The one
// FatalError that ends less is the refusal of one part under
// message.ProfileStrict, when its Content-Type or Content-Disposition has a
// valid base but malformed parameters: that part is not returned, the parse is
// not stopped (Entity.ParseStop stays nil), and the next call continues with
// the part after it.
//
// Under every other profile such a field is repaired by the parse — its valid
// base and well-formed parameters kept, the defect recorded as a diagnostic —
// and the part is returned and classified by the repaired metadata with no
// error, even though its raw header, which is unchanged, still fails the
// standalone Header accessors.
//
// A part is an attachment or inline text according to the metadata it was
// constructed with: message.Entity.EffectiveContentType and
// EffectiveContentDisposition, which for an entity from message.Parse are read
// under that parse's parameter budgets, and are the same values its decoders
// were chosen by. The raw header is not reparsed, so an oversized parameter
// list is not read past its budget, and a text attachment whose charset was
// left unconverted under message.ReadOptions.SkipTextAttachmentDecoding is
// never offered as inline text. Editing Entity.Header after the entity was
// constructed does not change how it is classified, as it does not change how
// its body is decoded. The returned Part.Header is the entity's header, raw
// bytes unchanged, and its own ContentType and ContentDisposition keep parsing
// it under no budget.
//
// Reading the disposition records its budget diagnostic, once per part, if the
// parse had not read it already. If a DiagnosticSink stops the parse there,
// NextPart returns that stop and no part, like any other stop.
func (r *Reader) NextPart() (*Part, error) {
	// Every call is a scope of the parse r.e came from, so none begins on a
	// parse that has been stopped. The multipart iterators of that parse ask
	// for themselves, but the wrapper NewReader builds around a single-part
	// message does not belong to the parse and would hand the root over
	// regardless; asking here is what makes a stopped root stay stopped,
	// whatever err Parse returned it with and however often this is called.
	// An entity that is not from Parse has no parse to stop, and this is nil.
	if stop := r.e.ParseStop(); stop != nil {
		return nil, stop
	}

	for r.readers.Len() > 0 {
		e := r.readers.Back()
		mr := e.Value.(message.MultipartReader)

		p, err := mr.NextPart()
		if err == io.EOF {
			// This whole multipart entity has been read, continue with the next one
			r.readers.Remove(e)
			continue
		} else if err != nil && !recoverable(p, err) {
			return nil, err
		}

		if pmr := p.MultipartReader(); pmr != nil {
			// This is a multipart part, read it
			r.readers.PushBack(pmr)
		} else {
			// This is a non-multipart part, return a mail part
			mp := &Part{Body: p.Body}
			t, _, typeErr := p.EffectiveContentType()
			disp, _, dispErr := p.EffectiveContentDisposition()
			// Reading the disposition may have recorded a diagnostic, and a
			// sink told about it may have stopped the parse. The stop is the
			// caller ending the parse, so it outranks the part, and any warning
			// the part came with, exactly as it does everywhere else.
			if stop := p.ParseStop(); stop != nil {
				return nil, stop
			}
			// Under message.ProfileStrict a field whose parameters would need
			// repair is not interpreted, and the refusal is a FatalError scoped
			// to this one entity: there is nothing to classify it by. It is
			// returned without a part, but it ends nothing else, so the next
			// call continues with the part after it. A field that was
			// repaired, or that failed as it always has, carries no such error
			// and is classified as before.
			for _, local := range []error{typeErr, dispErr} {
				if message.IsFatal(local) {
					return nil, local
				}
			}
			if disp == "inline" || (disp != "attachment" && strings.HasPrefix(t, "text/")) {
				mp.Header = &InlineHeader{p.Header}
			} else {
				mp.Header = &AttachmentHeader{p.Header}
			}
			return mp, err
		}
	}

	return nil, io.EOF
}

// Close finishes the reader.
func (r *Reader) Close() error {
	for r.readers.Len() > 0 {
		e := r.readers.Back()
		mr := e.Value.(message.MultipartReader)

		if err := mr.Close(); err != nil {
			return err
		}

		r.readers.Remove(e)
	}

	return nil
}
