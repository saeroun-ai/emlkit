package message

import (
	"fmt"
	"sort"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Header line endings (ROB-010-002, phase 1: header blocks).
//
// textproto reads every header block — the message's and each part's — on one
// rule: a line ends at "\r\n", at a bare '\n', or at a bare '\r', with a stray
// '\r' inside a value kept as content (see textproto.ReadHeader). It reports
// what ended the block's lines in Header.LineEndings, and that is all a parse
// looks at here; the fields themselves are what textproto returned, under
// every profile and entry point.
//
// Under Parse, an entity's header block records, before anything its fields
// say is interpreted:
//
//	Observation                         Code                        Severity     Recorded
//	a line ended by a bare LF           header.bare_lf              Info         once per parse
//	a line ended by a bare CR           header.bare_cr              Recovered    once per parse
//	more than one kind in one block     header.mixed_line_endings   Info, or     once per parse
//	                                                                 Recovered
//	                                                                 with a CR
//	the last line ended by io.EOF       header.truncated_line       Incomplete   per block
//	before any line ending
//
// The once-per-parse notes describe how the message is written, not a defect
// of one entity, so a message of ten thousand parts that all end their lines
// with LF records one of them, not ten thousand: the first, at the first such
// line ending of the block that had it. Each claims its slot under the
// session's lock before it is recorded and records with no lock held, so a
// sink that reads the session meanwhile cannot deadlock. A block ended by
// io.EOF is the last of its input, so header.truncated_line is rare by nature
// and is recorded for each. Locations are the first byte of the line ending in
// question — of the first line ending that differs from the block's first, for
// mixed endings — or, for a truncated line, that whole line; diagnostics of one
// block are recorded in the order of those offsets.
//
// ProfileStrict accepts bare LF, alone or mixed with CRLF: a header written
// with LF line endings has always been accepted under every profile, and is
// framed the same whichever ending a line has. A header block with a line
// ended by a bare '\r', or whose last line the input ended before any line
// ending, is refused instead, by the existing entity-scoped refusal of
// metadata_repair.go: a header.bare_cr or header.truncated_line diagnostic is
// recorded with SeverityInvalidMetadata, and the entity is returned with a
// FatalError carrying a copy of that diagnostic whose severity is
// SeverityFatal, as every FatalError's is; its Content-Type is not interpreted —
// EffectiveContentType reports an empty media type and that error — so no
// decoder is set up, MultipartReader returns nil and the body is the raw bytes
// after the header, held to the body budgets; the parse is not stopped, and a
// sibling part is read as usual. Every refused entity records its own
// diagnostic, whatever the once-per-parse notes have recorded, and its
// FatalError carries its own copy even when Limits.MaxDiagnosticCount omitted
// the recorded one. Profile Forensic records what Tolerant records; neither
// replays the input, whose LF, CR and missing line endings the raw fields
// store as CRLF. Every Compatibility value behaves the same.

// A headerNote is a diagnostic a header block owes, at the offset at in the
// block that orders it among the block's others, and whether ProfileStrict
// refuses the entity for it.
type headerNote struct {
	at     int64
	d      Diagnostic
	refuse bool
}

// A lineNote is one of the once-per-parse line-ending notes.
type lineNote uint8

const (
	noteBareLF lineNote = 1 << iota
	noteBareCR
	noteMixed
	noteMultipartLF
	noteMultipartCR
	noteMultipartMixed
)

// claimLineNote reports whether n has not been claimed in this parse yet, and
// claims it. It returns false for a nil session.
func (s *ParseSession) claimLineNote(n lineNote) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lineNotes&n != 0 {
		return false
	}
	s.lineNotes |= n
	return true
}

const (
	// normalizedLineAction is what the parse did with a line ending other
	// than CRLF.
	normalizedLineAction = "read it as the end of its line; the raw fields store each such line ending as CRLF"

	// truncatedLineAction is what the parse did with a last line the input
	// ended before any line ending.
	truncatedLineAction = "kept the line as read; the raw field stores it ended by CRLF"

	// strictLineAction is what ProfileStrict did instead.
	strictLineAction = "refused the entity: its Content-Type is not interpreted and its body is kept as it was read"
)

// headerLineEndings records what the byte order mark and the line endings of
// h, the header block of the entity this context describes, call for, and
// returns that entity's refusal under ProfileStrict, or nil. A parse with no
// session — every entry point but Parse — records and refuses nothing, and so
// do the line endings of a header this parse did not read whole.
//
// The byte order mark is the first thing a block can hold, so its note, at
// offset 0 of the block, comes before every line-ending note, and under
// ProfileStrict its refusal is the one the entity is returned with; see
// byteOrderMarkNote. A malformed prefix and a part's missing separator after
// one (firstline.go), and orphan records (orphan.go), are noted with them, in
// the same order of offsets.
func (c parseContext) headerLineEndings(h *textproto.Header) error {
	s := c.session
	if s == nil {
		return nil
	}
	strict := s.Profile() == ProfileStrict

	type owed = headerNote
	var notes []owed

	if bom := h.ByteOrderMark(); bom != textproto.BOMNone {
		d, refuse := c.byteOrderMarkNote(bom, strict)
		notes = append(notes, owed{at: 0, d: d, refuse: refuse})
	}

	// A malformed prefix comes right after any mark, and a part's header that
	// a delimiter line ended after one ends after every line of the block; see
	// firstline.go.
	notes = c.malformedPrefixNotes(h, strict, notes)

	// Orphan records lie among the fields, at the offset of the first; see
	// orphan.go.
	notes = c.orphanContinuationNotes(h, strict, notes)

	// An unknown LineEndings — a block that began with a UTF-16 or UTF-32
	// mark was not read — reports nothing below.
	le := h.LineEndings()
	lf, cr := le.Count(textproto.LineEndingLF), le.Count(textproto.LineEndingCR)

	if lf > 0 && s.claimLineNote(noteBareLF) {
		at, _ := le.First(textproto.LineEndingLF)
		notes = append(notes, owed{at: at, d: Diagnostic{
			Code:     CodeHeaderBareLF,
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("a header block ends %s with a bare LF", lineCount(lf)),
			Action:   normalizedLineAction,
			Location: c.headerSpan(at, 1),
		}})
	}

	if first, other, otherLen, ok := divergentEnding(le); ok && s.claimLineNote(noteMixed) {
		severity := SeverityInfo
		if cr > 0 {
			severity = SeverityRecovered
		}
		notes = append(notes, owed{at: other, d: Diagnostic{
			Code:     CodeHeaderMixedLineEndings,
			Severity: severity,
			Message: fmt.Sprintf("a header block mixes line endings: %d CRLF, %d LF and %d CR, the first %s",
				le.Count(textproto.LineEndingCRLF), lf, cr, first),
			Action:   normalizedLineAction,
			Location: c.headerSpan(other, otherLen),
		}})
	}

	if cr > 0 {
		at, _ := le.First(textproto.LineEndingCR)
		d := Diagnostic{
			Code:     CodeHeaderBareCR,
			Severity: SeverityRecovered,
			Message:  fmt.Sprintf("a header block ends %s with a bare CR", lineCount(cr)),
			Action:   normalizedLineAction,
			Location: c.headerSpan(at, 1),
		}
		switch {
		case strict:
			d.Severity, d.Action = SeverityInvalidMetadata, strictLineAction
			notes = append(notes, owed{at: at, d: d, refuse: true})
		case s.claimLineNote(noteBareCR):
			notes = append(notes, owed{at: at, d: d})
		}
	}

	if start, end, ok := le.Unterminated(); ok {
		d := Diagnostic{
			Code:     CodeHeaderTruncatedLine,
			Severity: SeverityIncomplete,
			Message:  "the input ended inside the last line of a header block, before any line ending",
			Action:   truncatedLineAction,
			Location: c.headerSpan(start, end-start),
		}
		if strict {
			d.Severity, d.Action = SeverityInvalidMetadata, strictLineAction
		}
		notes = append(notes, owed{at: start, d: d, refuse: strict})
	}

	sort.SliceStable(notes, func(i, j int) bool { return notes[i].at < notes[j].at })

	var refusal error
	for _, n := range notes {
		d := c.completeHeader(n.d)
		// A sink that stops the parse here stops it as from any diagnostic:
		// the session publishes the stop, and the entity's construction finds it
		// there and returns it ahead of this refusal.
		c.recordHeader(d)
		if n.refuse && refusal == nil {
			refusal = newFatalDiagnostic(d)
		}
	}
	return refusal
}

// divergentEnding reports, for a block whose lines end in more than one way,
// the kind of its first line ending, and the offset and length of the first
// line ending of another kind.
func divergentEnding(le textproto.LineEndings) (first textproto.LineEnding, other int64, otherLen int64, ok bool) {
	kinds := []textproto.LineEnding{textproto.LineEndingCRLF, textproto.LineEndingLF, textproto.LineEndingCR}
	present := 0
	firstAt := int64(-1)
	for _, k := range kinds {
		if at, has := le.First(k); has {
			present++
			if firstAt < 0 || at < firstAt {
				first, firstAt = k, at
			}
		}
	}
	if present < 2 {
		return 0, 0, 0, false
	}
	other = -1
	for _, k := range kinds {
		if at, has := le.First(k); has && k != first && (other < 0 || at < other) {
			other, otherLen = at, 1
			if k == textproto.LineEndingCRLF {
				otherLen = 2
			}
		}
	}
	return first, other, otherLen, true
}

// headerSpan returns the location of n bytes that begin delta bytes into this
// entity's header block, or the zero Location when the entity's position is not
// known.
func (c parseContext) headerSpan(delta, n int64) Location {
	l := c.headerLocationAt(delta)
	if l.HasOffset {
		l.EndOffset = l.Offset + n
	}
	return l
}

// lineCount renders a number of lines for a diagnostic message.
func lineCount(n int64) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

// Multipart delimiter line endings (ROB-010-002, phase 2b).
//
// textproto frames a multipart's delimiter lines on any of "\r\n", a bare '\n'
// and a bare '\r', before and after each delimiter, and counts what it proved
// in MultipartReader.DelimiterEndings. It proves a delimiter as soon as the
// body before it has been read to it — by a read of that part's Body, which
// may come long before the next NextPart — so this layer does not compare
// snapshots around a call: each iterator keeps what it has already looked at,
// and looks at what was added since every time its low-level NextPart returns
// a part or io.EOF (the closing delimiter included). A diagnostic, or a Strict
// refusal, for a delimiter a Body read proved is therefore observed by the
// next NextPart, never by the Body read.
//
//	Observation                          Code                          Severity    Recorded
//	a delimiter line ending bare LF      multipart.bare_lf             Info        once per parse
//	a delimiter line ending bare CR      multipart.bare_cr             Recovered   once per parse
//	one multipart's delimiters use more  multipart.mixed_line_endings  Info, or    once per parse
//	than one kind                                                       Recovered
//	                                                                    with a CR
//
// Locations are the first such ending of the multipart in the input, its
// container's body offset added, and the path is the container's; a container
// whose Body the caller replaced has neither. ProfileStrict accepts LF and
// LF with CRLF. A bare CR before or after a delimiter is refused locally: the
// first NextPart that observes one records multipart.bare_cr of
// SeverityInvalidMetadata for that iterator — whatever the once-per-parse
// notes have recorded — and returns, then and on every later call, a
// FatalError carrying a SeverityFatal copy of it, even when
// Limits.MaxDiagnosticCount omitted the recorded one. The part that delimiter
// introduced is not returned; the parts already returned, and their bodies,
// stay valid; the parse is not stopped, so an enclosing multipart goes on
// with its next part.
//
// Precedence. A stop of the session — a sink, a budget, a cancellation, the
// parse's shared header budget refused on this call — outranks the refusal:
// the stop is returned, and with the session already stopped, or the shared
// header budget refused, nothing is looked at. A call whose low-level NextPart
// failed otherwise — an input failure, a malformed part header — still has
// its delimiters looked at, but reports its own failure first: the refusal is
// recorded on that call and kept as the iterator's sticky failure, and every
// later call returns it, so that no part past the refused delimiter is
// returned even though the low-level reader would resume past the malformed
// header. A part header or framing line budget refused by this iterator on
// the same call is reported, and replaces the refusal as the sticky failure:
// the multipart ends either way, and the budget failure is the one the call
// returned. The bare_cr diagnostic is recorded in all these cases but the
// session stop.

// delimiterEndings records what the delimiter line endings this iterator's
// reader proved since it last looked call for, and returns the iterator's
// Strict refusal, the session's stop, or nil. framed reports that the
// low-level NextPart framed a part or ended with io.EOF. Otherwise it failed
// and its failure is what the caller reports first: a Strict refusal is then
// recorded and kept in r.fatal all the same, so that the next call — which
// the low-level reader could otherwise answer with a later part, as it heals
// past a malformed part header — returns it, and nil is returned. A session
// stop is returned whether or not framed.
func (r *multipartReader) delimiterEndings(framed bool) error {
	s := r.parent.session
	if s == nil {
		return nil
	}
	cur := r.r.DelimiterEndings()
	prev := r.ends
	r.ends = cur
	added := func(k textproto.LineEnding) bool { return cur.Count(k) > prev.Count(k) }
	newLF, newCR := added(textproto.LineEndingLF), added(textproto.LineEndingCR)
	_, _, _, mixed := divergentEnding(cur)
	if !newLF && !newCR && !(mixed && added(textproto.LineEndingCRLF)) {
		return nil
	}
	strict := s.Profile() == ProfileStrict
	crCount := cur.Count(textproto.LineEndingCR)

	type owed struct {
		at     int64
		d      Diagnostic
		refuse bool
	}
	var notes []owed
	if newLF && s.claimLineNote(noteMultipartLF) {
		at, _ := cur.First(textproto.LineEndingLF)
		notes = append(notes, owed{at: at, d: Diagnostic{
			Code:     CodeMultipartBareLF,
			Severity: SeverityInfo,
			Message:  "a multipart delimiter line has a bare LF line ending before or after it",
			Action:   delimiterAction,
			Location: r.parent.bodySpan(at, 1),
		}})
	}
	if first, other, otherLen, ok := divergentEnding(cur); ok && s.claimLineNote(noteMultipartMixed) {
		severity := SeverityInfo
		if crCount > 0 {
			severity = SeverityRecovered
		}
		notes = append(notes, owed{at: other, d: Diagnostic{
			Code:     CodeMultipartMixedLineEndings,
			Severity: severity,
			Message: fmt.Sprintf("a multipart's delimiter lines mix line endings: %d CRLF, %d LF and %d CR, the first %s",
				cur.Count(textproto.LineEndingCRLF), cur.Count(textproto.LineEndingLF), crCount, first),
			Action:   delimiterAction,
			Location: r.parent.bodySpan(other, otherLen),
		}})
	}
	if newCR {
		at, _ := cur.First(textproto.LineEndingCR)
		d := Diagnostic{
			Code:     CodeMultipartBareCR,
			Severity: SeverityRecovered,
			Message:  "a multipart delimiter line has a bare CR line ending before or after it",
			Action:   delimiterAction,
			Location: r.parent.bodySpan(at, 1),
		}
		switch {
		case strict:
			d.Severity, d.Action = SeverityInvalidMetadata, strictDelimiterAction
			notes = append(notes, owed{at: at, d: d, refuse: true})
		case s.claimLineNote(noteMultipartCR):
			notes = append(notes, owed{at: at, d: d})
		}
	}
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].at < notes[j].at })

	var refusal error
	for _, n := range notes {
		d := r.parent.completeBody(n.d)
		r.parent.recordBody(d)
		if n.refuse && refusal == nil {
			refusal = newFatalDiagnostic(d)
		}
	}
	// A stop of the session — a sink that refused one of these, a budget, a
	// cancellation — outranks this iterator's own refusal.
	if stop := r.parent.fatalErr(); stop != nil {
		return stop
	}
	if refusal == nil {
		return nil
	}
	// The refusal is this iterator's sticky failure from now on. A call that
	// failed otherwise reports its own failure first, so the refusal is kept
	// for the next call rather than returned; a budget refusal of this
	// iterator's own, recorded after this, replaces it.
	r.fatal = refusal
	if !framed {
		return nil
	}
	return refusal
}

const (
	// delimiterAction is what the parse did with a delimiter line ending
	// other than CRLF.
	delimiterAction = "framed the parts on it; no part's body includes it"

	// strictDelimiterAction is what ProfileStrict did instead.
	strictDelimiterAction = "refused the rest of this multipart: no further part is returned, and the parts already returned are kept"
)

// bodySpan returns the location of n bytes that begin delta bytes into this
// entity's body, or the zero Location when the entity's position is not known.
func (c parseContext) bodySpan(delta, n int64) Location {
	l := c.bodyLocationAt(delta)
	if l.HasOffset {
		l.EndOffset = l.Offset + n
	}
	return l
}
