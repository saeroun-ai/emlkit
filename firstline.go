package message

import (
	"fmt"
	"math"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Malformed prefixes (ROB-020-001).
//
// A header block — the message's or a part's — whose first lines begin with
// white space begins with no field. textproto keeps that run of lines apart as
// the header's malformed prefix, byte for byte, and reads the block on after
// it, under every entry point: Parse, ParseContext, Read, ReadWithOptions,
// textproto.ReadHeader and the header of every part. The prefix is never a
// field: Get, Has, Values, Fields, Len and Map do not see it, so a line such as
// " Content-Type: multipart/mixed; boundary=B" or " Content-Transfer-Encoding:
// base64" in it declares nothing, and the entity is interpreted from the fields
// after it alone. Header.MalformedPrefix returns it; the writers write it back
// first, before every field (see textproto.WriteHeader). Read, ReadWithOptions,
// New and the mail package record and refuse nothing for it.
//
// In a part's header that began with a malformed prefix, a line proved to be a
// delimiter line of its multipart — its line ending, or the end of the input,
// seen after the boundary and any white space — is never read as one of the
// block's lines: the block ends before it, the part has an empty body, and the
// next part is framed as usual (textproto Part.HeaderSeparatorMissing). A line
// that only begins like one, such as a field "--B : value", is read as it
// always was.
//
// Under Parse each such block records, with the other notes of its header
// block (see line_endings.go), in the order of their offsets:
//
//	Observation                       Code                            Tolerant,    Strict
//	                                                                  Forensic
//	a malformed prefix                header.first_line_continuation  Recovered    InvalidMetadata,
//	                                                                               entity refused
//	a part's header after a prefix    header.missing_body_separator   Recovered    InvalidMetadata,
//	(or an orphan record, see                                                      entity refused
//	orphan.go) ended by a
//	delimiter line
//
// header.first_line_continuation covers the whole prefix, its line endings
// included, at the entity's path; its message counts the prefix's lines and
// holds none of its bytes. header.missing_body_separator is at the point where
// the header ended, which is where the delimiter line begins and the empty body
// would. Both have no field name. A prefix's own line endings draw the usual
// line-ending notes, and a last line the input ended draws
// header.truncated_line.
//
// ProfileStrict refuses the entity on the terms of a header line ended by a
// bare CR (see line_endings.go): the entity is returned with a FatalError
// carrying a SeverityFatal copy of the first refusing note by offset — a
// byte order mark's before the prefix's — even when Limits.MaxDiagnosticCount
// omitted the recorded one; its fields after the prefix are kept, its
// Content-Type is not interpreted, its body is kept as it was read; the parse
// is not stopped, and a sibling part is read as usual: NextPart continues with
// it, while Walk ends at the refused entity as for every Strict refusal. A
// stop of the session — a sink, a budget, cancellation — outranks the refusal.
//
// Every Compatibility value behaves the same: the prefix is retained and not
// interpreted. Other engines differ on the fixtures recorded in
// framing_firstline_test.go — some read such a line as a field, some drop it,
// some fail the whole parse — and those differences are intentional; no
// parity with any of them is claimed (#17 owns compatibility policies).

const (
	// firstLineAction is what the parse did with a malformed prefix.
	firstLineAction = "kept the lines apart, byte for byte, as the header's malformed prefix: they are not a field and declare nothing; the fields after them are read as usual, and writing the header writes them first"

	// strictFirstLineAction is what ProfileStrict did instead.
	strictFirstLineAction = "refused the entity: the prefix is kept and its fields after it were read, but its Content-Type is not interpreted and its body is kept as it was read"

	// missingSeparatorAction is what the parse did with a part's header that
	// a delimiter line ended.
	missingSeparatorAction = "ended the header block before the delimiter line, which is not read as a field: the part's body is empty, and the next part is framed as usual"

	// strictMissingSeparatorAction is what ProfileStrict did instead.
	strictMissingSeparatorAction = "refused the entity: its header block ended before the delimiter line, its body is empty, and the next part is framed as usual"
)

// malformedPrefixNotes appends to notes the diagnostics a malformed prefix of
// h, the header block of the entity this context describes, calls for, and
// the missing separator of a part's header that a delimiter line ended.
func (c parseContext) malformedPrefixNotes(h *textproto.Header, strict bool, notes []headerNote) []headerNote {
	if p := h.MalformedPrefix(); p.Present() {
		start, end := p.Location()
		d := Diagnostic{
			Code:     CodeHeaderFirstLineContinuation,
			Severity: SeverityRecovered,
			Message:  fmt.Sprintf("a header block begins with white space: %s before any field", lineCount(int64(p.Lines()))),
			Action:   firstLineAction,
			Location: c.headerSpan(start, end-start),
		}
		if strict {
			d.Severity, d.Action = SeverityInvalidMetadata, strictFirstLineAction
		}
		notes = append(notes, headerNote{at: start, d: d, refuse: strict})
	}
	if c.separatorMissing {
		d := Diagnostic{
			Code:     CodeHeaderMissingBodySeparator,
			Severity: SeverityRecovered,
			Message:  "a part's header block ends at a delimiter line of its multipart, with no empty line before it",
			Action:   missingSeparatorAction,
			Location: c.bodyLocationAt(0),
		}
		if strict {
			d.Severity, d.Action = SeverityInvalidMetadata, strictMissingSeparatorAction
		}
		// It is where the block ended, after every byte of it.
		notes = append(notes, headerNote{at: math.MaxInt64, d: d, refuse: strict})
	}
	return notes
}
