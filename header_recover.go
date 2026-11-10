package message

import (
	"bytes"
	"fmt"
	"math"
	"strings"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Header recovery under Parse (ROB-020-003 to ROB-020-006, ROB-060-010).
//
// Every profile but ProfileStrict reads header blocks — the message's and
// each part's — with textproto.HeaderLimits.Recover: a damaged line that no
// continuation line follows no longer fails the block, and with it the
// message or the rest of its multipart. See textproto's recover.go for how
// such a line is decided; what was decided is reported here, once per block
// and kind:
//
//   - header.unindented_continuation: lines read as continuation lines of the
//     field before them that had lost their leading white space;
//   - header.missing_colon and header.invalid_name: damaged lines kept apart
//     from the fields, byte for byte, because the block evidently went on
//     after them;
//   - header.missing_body_separator, or header.whitespace_body_separator when
//     the line before held only white space: the block ended before a line
//     that is no field, which begins the body.
//
// ProfileStrict keeps the errors such lines always were. Read, ReadWithOptions
// and New read as before.

const (
	unindentedAction      = "read each as a continuation line of the field before it; the raw field keeps the line as it was read"
	damagedLineAction     = "kept each apart from the fields, byte for byte: it declares nothing, the fields around it are read as usual, and writing the header writes it in its place"
	missingSeparatorHeal  = "ended the header block before the first line that is no header field; that line begins the body"
	whitespaceSepHealNote = "read the white space only line as the end of the field before it and ended the header block before the first line that is no header field; that line begins the body"
)

// recoveryNotes appends the notes of what header recovery did to h.
func (c parseContext) recoveryNotes(h *textproto.Header, notes []headerNote) []headerNote {
	if n, at := h.UnindentedContinuations(); n > 0 {
		notes = append(notes, headerNote{at: at, d: Diagnostic{
			Code:     CodeHeaderUnindentedContinuation,
			Severity: SeverityRecovered,
			Message:  fmt.Sprintf("%s of a header block continue the field before them with no leading white space", lineCount(int64(n))),
			Action:   unindentedAction,
			Location: c.headerLocationAt(at),
		}})
	}

	var colonless, badName, noName int64
	colonAt, nameAt, emptyAt := int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64)
	var colonLen, nameLen, emptyLen int64
	for _, o := range h.OrphanContinuations() {
		if o.Lines() != 1 {
			continue
		}
		start, end := o.Location()
		raw := o.Bytes()
		if t := bytes.TrimLeft(raw, " \t"); len(t) > 0 && (t[0] == ':' || (t[0] == ';' && !bytes.Contains(raw, []byte(":")))) {
			noName++
			if start < emptyAt {
				emptyAt, emptyLen = start, end-start
			}
		} else if bytes.ContainsAny(raw, ":;") {
			badName++
			if start < nameAt {
				nameAt, nameLen = start, end-start
			}
		} else {
			colonless++
			if start < colonAt {
				colonAt, colonLen = start, end-start
			}
		}
	}
	if colonless > 0 {
		notes = append(notes, headerNote{at: colonAt, d: Diagnostic{
			Code:     CodeHeaderMissingColon,
			Severity: SeverityRecovered,
			Message:  fmt.Sprintf("a header block has %s with no colon that continue no field", lineCount(colonless)),
			Action:   damagedLineAction,
			Location: c.headerSpan(colonAt, colonLen),
		}})
	}
	if badName > 0 {
		notes = append(notes, headerNote{at: nameAt, d: Diagnostic{
			Code:     CodeHeaderInvalidName,
			Severity: SeverityRecovered,
			Message:  fmt.Sprintf("a header block has %s whose field name holds a byte a name cannot", lineCount(badName)),
			Action:   damagedLineAction,
			Location: c.headerSpan(nameAt, nameLen),
		}})
	}

	if noName > 0 {
		notes = append(notes, headerNote{at: emptyAt, d: Diagnostic{
			Code:     CodeHeaderEmptyName,
			Severity: SeverityRecovered,
			Message:  fmt.Sprintf("a header block has %s with no field name before the separator", lineCount(noName)),
			Action:   damagedLineAction,
			Location: c.headerSpan(emptyAt, emptyLen),
		}})
	}

	if missing, at, ws := h.BodySeparatorMissing(); missing {
		d := Diagnostic{
			Code:     CodeHeaderMissingBodySeparator,
			Severity: SeverityRecovered,
			Message:  "a header block is followed by its body with no empty line between them",
			Action:   missingSeparatorHeal,
			Location: c.headerLocationAt(at),
		}
		if ws {
			d.Code, d.Action = CodeHeaderWhitespaceBodySeparator, whitespaceSepHealNote
			d.Message = "a header block is separated from its body by a line of white space only"
		}
		notes = append(notes, headerNote{at: at, d: d})
	}
	return notes
}

// singletonFields are the fields RFC 5322 section 3.6 and RFC 2045 allow at
// most once in a header block, by canonical key.
var singletonFields = map[string]bool{
	"Date": true, "From": true, "Sender": true, "Reply-To": true, "To": true, "Cc": true,
	"Bcc": true, "Message-Id": true, "In-Reply-To": true, "References": true, "Subject": true,
	"Mime-Version": true, "Content-Type": true, "Content-Transfer-Encoding": true,
	"Content-Disposition": true, "Content-Id": true, "Content-Description": true,
}

const duplicateSingletonAction = "kept every occurrence in the header; accessors and the parser read the first"

// duplicateSingletonNotes appends one header.duplicate_singleton note for a
// block that repeats a field allowed at most once (ROB-020-008). Nothing is
// removed: every occurrence stays in the header, in order. The severity is
// SeverityRecovered when a repeated Content-Type, Content-Transfer-Encoding or
// Content-Disposition differs from the first, since the choice then changes
// how the entity is read, and SeverityInfo otherwise.
func (c parseContext) duplicateSingletonNotes(h *textproto.Header, notes []headerNote) []headerNote {
	if h.Len() < 2 {
		return notes
	}
	first := make(map[string]string, 8)
	var names []string
	seen := make(map[string]bool, 2)
	conflict := false
	fields := h.Fields()
	for fields.Next() {
		k := fields.Key()
		if !singletonFields[k] {
			continue
		}
		v, ok := first[k]
		if !ok {
			first[k] = fields.Value()
			continue
		}
		if !seen[k] {
			seen[k] = true
			names = append(names, k)
		}
		if v != fields.Value() && (k == "Content-Type" || k == "Content-Transfer-Encoding" || k == "Content-Disposition") {
			conflict = true
		}
	}
	if len(names) == 0 {
		return notes
	}
	d := Diagnostic{
		Code:     CodeHeaderDuplicateSingleton,
		Severity: SeverityInfo,
		Message:  fmt.Sprintf("a header block repeats %s, allowed at most once", strings.Join(names, ", ")),
		Field:    names[0],
		Action:   duplicateSingletonAction,
	}
	if conflict {
		d.Severity = SeverityRecovered
	}
	// After the notes of the block's own lines.
	return append(notes, headerNote{at: math.MaxInt64 - 1, d: d})
}
