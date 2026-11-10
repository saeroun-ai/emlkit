package message

import (
	"fmt"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Orphan continuation lines (ROB-020-002).
//
// A continuation line of a header block continues the logical candidate
// before it. When that candidate is damaged — it has no colon or semicolon, its
// name holds a byte a name cannot hold, or its name is empty — no field can be
// read from it, and its continuation lines continue nothing. textproto keeps
// such a candidate that has at least one continuation line read whole, its
// damaged line and its continuation lines together, apart from the fields as
// an orphan record, byte for byte, and reads the block on after it, under
// every entry point: Parse, ParseContext, Read, ReadWithOptions,
// textproto.ReadHeader and the header of every part. A record is never a
// field: Get, Has, Values, Fields, Len and Map do not see it, so an indented
// " Content-Type: multipart/mixed; boundary=B" or " Content-Transfer-Encoding:
// base64" in one declares nothing, and nothing of it is joined to a field
// before it — a Subject or a Content-Type before a record keeps the value it
// had. Header.OrphanContinuations returns the records; the writers write each
// in its place among the fields (see textproto.WriteHeader). A damaged line
// with no continuation line is read as it always was: an empty name is
// skipped, and any other fails the block with the error it always did
// (ROB-020-003 and ROB-020-006 own those). Read, ReadWithOptions, New and the
// mail package record and refuse nothing for a record.
//
// In a part's header in which a record was recovered, a line proved to be a
// delimiter line of its multipart is never read as one of the block's lines,
// as after a malformed prefix (firstline.go): the block ends before it, the
// part has an empty body, and the next part is framed as usual.
//
// Under Parse each header block that holds records records, with the other
// notes of its block (see line_endings.go), in the order of their offsets:
//
//	Observation                       Code                            Tolerant,    Strict
//	                                                                  Forensic
//	orphan records                    header.orphan_continuation      Recovered    InvalidMetadata,
//	                                                                               entity refused
//	a part's header after a record    header.missing_body_separator   Recovered    InvalidMetadata,
//	ended by a delimiter line                                                      entity refused
//
// header.orphan_continuation is recorded once per block, located over the
// whole of the block's first record, its line endings included; its message
// counts the records and their continuation lines and holds none of their
// bytes, which the records themselves keep. It has no field name.
//
// ProfileStrict refuses the entity on the terms of a malformed prefix: the
// entity is returned with a FatalError carrying a SeverityFatal copy of the
// first refusing note by offset, even when Limits.MaxDiagnosticCount omitted
// the recorded one; its fields and records are kept, its Content-Type is not
// interpreted, its body is kept as it was read; the parse is not stopped, and
// a sibling part is read as usual. A stop of the session — a sink, a budget,
// cancellation — outranks the refusal.
//
// Every Compatibility value behaves the same: the record is retained and not
// interpreted. Other engines read such lines differently — some keep the
// damaged line as a header with its continuations, some drop it with defects,
// some fail — and no parity with any of them is claimed (#17 owns
// compatibility policies).

const (
	// orphanAction is what the parse did with orphan records.
	orphanAction = "kept each damaged line and the continuation lines after it apart, byte for byte, as an orphan record: they continue no field and declare nothing; the fields around them are read as usual, and writing the header writes them in their place"

	// strictOrphanAction is what ProfileStrict did instead.
	strictOrphanAction = "refused the entity: the orphan records and the fields are kept, but its Content-Type is not interpreted and its body is kept as it was read"
)

// orphanContinuationNotes appends to notes the diagnostic the orphan records
// of h, the header block of the entity this context describes, call for.
func (c parseContext) orphanContinuationNotes(h *textproto.Header, strict bool, notes []headerNote) []headerNote {
	records := h.OrphanContinuations()
	if len(records) == 0 {
		return notes
	}
	var lines int64
	for _, o := range records {
		lines += int64(o.Lines() - 1)
	}
	start, end := records[0].Location()
	runs := "1 damaged header line"
	if len(records) != 1 {
		runs = fmt.Sprintf("%d damaged header lines", len(records))
	}
	d := Diagnostic{
		Code:     CodeHeaderOrphanContinuation,
		Severity: SeverityRecovered,
		Message: fmt.Sprintf("a header block has %s, with no colon, an invalid name or an empty name, followed by %s of continuation that no field began",
			runs, lineCount(lines)),
		Action:   orphanAction,
		Location: c.headerSpan(start, end-start),
	}
	if strict {
		d.Severity, d.Action = SeverityInvalidMetadata, strictOrphanAction
	}
	return append(notes, headerNote{at: start, d: d, refuse: strict})
}
