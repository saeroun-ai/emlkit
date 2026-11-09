package message

import (
	"fmt"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Byte order marks (ROB-010-003).
//
// textproto checks the first bytes of every header block — the message's and
// each part's, and those bytes only — for a byte order mark, under every entry
// point alike; see textproto.ReadHeader and textproto.Header.ByteOrderMark. A
// UTF-8 mark is consumed and the fields are read after it, so it no longer
// makes the first field's name invalid. A UTF-16 or UTF-32 mark leaves the
// block unread: the Header has no fields, and the mark and every byte after it
// are the entity's body.
//
// Such an entity is opaque under every entry point — Parse, Read,
// ReadWithOptions, New — whatever fields its Header has been given since:
// its Content-Type and Content-Transfer-Encoding are not interpreted, so
// EffectiveContentType reports an empty media type and an error saying why,
// with no text/plain inferred for it; no transfer or charset decoder is set
// up, so Body yields the bytes as they were read, the mark included; and
// MultipartReader returns nil. Under Parse the body is a leaf held to the body
// budgets. Its text is never transcoded. The writers refuse such a header
// before writing anything; see createWriter.
//
// Content-Disposition is not part of that: it is read as for any entity, when
// something asks (EffectiveContentDisposition, mail.Reader). A header that was
// parsed from such a block has no fields, so it declares no disposition, and
// with no media type mail.Reader classifies the part as an attachment. A
// caller who gives a copy of such a header a Content-Disposition and builds an
// entity from it with New gets that disposition interpreted — "inline" makes
// it an inline part to mail.Reader — while its body is still not decoded.
//
// Under Parse each header block that begins with a mark records one input.bom
// diagnostic, at the mark's bytes (offset 0 of the block, two to four bytes
// long; for an opaque entity its header offset is its body offset):
//
//	Mark              Tolerant, Forensic          Strict
//	UTF-8             Recovered                   InvalidMetadata, entity refused
//	UTF-16, UTF-32    Unsupported, entity opaque  InvalidMetadata, entity refused
//
// It is recorded before the block's line-ending notes, which can only come
// after it. ProfileStrict refuses the entity on the terms of a header line
// ended by a bare CR (see line_endings.go): the entity is returned with a
// FatalError carrying a SeverityFatal copy of the diagnostic — even when
// Limits.MaxDiagnosticCount omitted the recorded one — its Content-Type is not
// interpreted and its body is kept as it was read; the parse is not stopped,
// siblings are read as usual, and a stop of the session outranks the refusal.
// For a UTF-8 mark the fields read after it are kept. Every Compatibility
// value behaves the same.

// wideMark reports whether m is a UTF-16 or UTF-32 byte order mark: one whose
// header block was not read.
func wideMark(m textproto.ByteOrderMark) bool {
	switch m {
	case textproto.BOMUTF16BE, textproto.BOMUTF16LE, textproto.BOMUTF32BE, textproto.BOMUTF32LE:
		return true
	}
	return false
}

// wideHeaderError is the error EffectiveContentType reports for an entity
// whose header block began with a UTF-16 or UTF-32 byte order mark and was
// therefore not read, when no refusal was made instead. It is no FatalError
// and no warning: the entity is usable, opaque, and the parse goes on.
type wideHeaderError struct {
	mark textproto.ByteOrderMark
}

func (e wideHeaderError) Error() string {
	return fmt.Sprintf("message: the header block began with a %v byte order mark and was not read; the entity is opaque, its body the bytes from the mark on", e.mark)
}

const (
	// utf8MarkAction is what the parse did with a UTF-8 mark.
	utf8MarkAction = "read the fields after the mark; the header keeps the mark, and writing the header writes it once before the fields"

	// wideMarkAction is what the parse did with a UTF-16 or UTF-32 mark.
	wideMarkAction = "read no field and transcoded nothing: the entity is opaque, and its body is every byte from the mark on, as it was read"

	// strictUTF8MarkAction and strictWideMarkAction are what ProfileStrict
	// did instead.
	strictUTF8MarkAction = "refused the entity: its fields were read after the mark, but its Content-Type is not interpreted and its body is kept as it was read"
	strictWideMarkAction = "refused the entity: no field was read, and its body is every byte from the mark on, as it was read"
)

// byteOrderMarkNote returns the input.bom diagnostic of a header block of the
// entity this context describes that began with bom, and whether ProfileStrict
// refuses the entity for it.
func (c parseContext) byteOrderMarkNote(bom textproto.ByteOrderMark, strict bool) (Diagnostic, bool) {
	d := Diagnostic{
		Code:     CodeInputBOM,
		Severity: SeverityRecovered,
		Message:  fmt.Sprintf("a header block begins with a %v byte order mark", bom),
		Action:   utf8MarkAction,
		Location: c.headerSpan(0, int64(len(bom.Bytes()))),
	}
	wide := wideMark(bom)
	if wide {
		d.Severity = SeverityUnsupported
		d.Message = fmt.Sprintf("a header block begins with a %v byte order mark; text encoded as %v is not read", bom, bom)
		d.Action = wideMarkAction
	}
	if strict {
		d.Severity, d.Action = SeverityInvalidMetadata, strictUTF8MarkAction
		if wide {
			d.Action = strictWideMarkAction
		}
	}
	return d, strict
}

// errWideHeaderWrite returns the error a writer refuses a header with whose
// block began with the UTF-16 or UTF-32 byte order mark m.
func errWideHeaderWrite(m textproto.ByteOrderMark) error {
	return fmt.Errorf("message: cannot write an entity whose header block began with a %v byte order mark: the block was not read, and its bytes are the entity's body", m)
}
