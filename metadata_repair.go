package message

import (
	"fmt"
	"mime"
	"strings"
)

// The parse engine's reading of a Content-Type or Content-Disposition that the
// whole-field parser does not accept as it stands, or does not read in full
// (ROB-050-001 to ROB-050-008, issues #22, #23 and #24).
//
// # What is read
//
// mime.ParseMediaType refuses a whole field over one bad item, and the legacy
// fallback then reports the raw value as if it were the media type. Under
// Parse such a field is read item by item instead, from the occurrences the
// parameter budget scan retained, by the tolerant reading of
// mediatype_tolerant.go and rfc2231.go:
//
//   - The base — type "/" subtype for Content-Type, one token for
//     Content-Disposition — is kept as the whole-field parser normalizes it
//     when it is valid on its own. A base missing its type, subtype or slash is
//     read by the conservative defaults of healMediaType (ROB-050-002).
//   - Every item is read on its own, whether it comes before or after a
//     damaged one. Items are the top-level semicolon separated items found by
//     the quote-aware scan of limit_parameters.go, so a semicolon inside a
//     quoted string never separates anything. An item the whole-field parser
//     accepts reads exactly as it does there; values that are not tokens,
//     quoted strings that are never closed and stray quotes and backslashes
//     are recovered (ROB-050-003, ROB-050-004).
//   - An item with no name, no "=" or no value is malformed and declares
//     nothing. An empty item between semicolons — "multipart/mixed;;
//     boundary=B" — declares nothing either; it is not a parameter occurrence,
//     and no budget charges it. Both are reported for the field as a whole.
//   - RFC 2231 extended and continued values are assembled whatever order
//     their segments arrive in, with their charset decoded through the
//     configured CharsetReader, and a series with a gap, a bad percent escape
//     or an unknown charset keeps what it has (ROB-050-006, ROB-050-007).
//   - A parameter that repeats keeps every occurrence as a candidate and
//     chooses one by a fixed precedence, except that a boundary is never
//     chosen among differing candidates (ROB-050-005).
//   - RFC 2047 encoded words and raw 8-bit bytes in a value are read and
//     reported (ROB-050-008).
//
// A field with none of these — which is every conformant field — never
// reaches this reading, or leaves it with the result the whole-field parser
// gives and no diagnostic.
//
// # A multipart that kept no boundary
//
// A repaired multipart Content-Type can end with no boundary it may use: the
// boundary was repeated with conflicting values, a damaged item named it, an
// unterminated quote swallowed it, or the field never had one and its type was
// only inferred. Nothing is chosen or invented then, and the media type is
// still reported. But a body with no known boundary cannot be framed, and
// iterating it would fail at once and cost the siblings after it to every
// reader that stops at the first error, so such an entity is unframed:
// MultipartReader returns nil and the body is one opaque, uninterpreted part
// holding the bytes of the input. Its diagnostics are SeverityInvalidMetadata,
// and their Action says the parts inside were not read. A multipart with no
// boundary that no repair touched keeps its existing behavior; inferring a
// boundary is ROB-060-002.
//
// # What is bounded
//
// Only the occurrences the budget scan retained are read, so a field past
// MaxParameterCount or MaxParameterSegments is never read whole as a second
// attempt, and with both budgets disabled the field is scanned once before it
// is read item by item. Each item is read once, parameter names are tracked in
// maps, the segments of a series are sorted only when they arrive out of
// order, and a field records at most one diagnostic per code, each listing at
// most maxDiagnosticNames parameter names. A continued parameter among the
// occurrences a budget cut is still dropped whole, as limit_parameters.go
// explains: a series that may go on past the cut is not reported as a value.
//
// # Diagnostics and profiles
//
// A field records, after any budget diagnostic and in this order: the code of
// a repaired base (mime.content_type_missing_type, _missing_subtype,
// _missing_slash), one mime.content_type_invalid or mime.disposition_invalid
// for malformed and empty items and for a base with unexpected text, and then
// one diagnostic per parameter code that applies, in the order of
// parameterNoteOrder. They share one Action, which names the base, the
// parameters kept, and what was ignored.
//
// Findings are of two kinds. A repair — anything the whole-field parser would
// have refused, or read to a different value — is SeverityRecovered, or
// SeverityInvalidMetadata for an unframed multipart. An observation that
// changes nothing this package did not already do — encoded words decoded, raw
// UTF-8 kept, a backslash kept as a backslash, bytes left undecoded for want of
// a charset — is SeverityInfo or SeverityUnsupported.
//
//	Profile           a field that needs repair
//	Tolerant (zero)   repaired, as above
//	Forensic          the same repair and diagnostics as Tolerant
//	Strict            not interpreted: an entity-scoped FatalError, and only
//	                  the repair diagnostics, as SeverityInvalidMetadata
//
// A field with observations only is interpreted alike under every profile.
// What a strict refusal can still affect depends on when it is known, and
// nothing is rewound or read early to make it more:
//
//   - A Content-Type is read while the entity is constructed, before any
//     decoder is chosen, so its refusal leaves the body exactly as the input
//     held it: no transfer decoding, no charset conversion, no part iteration.
//   - A Content-Disposition is read only when something needs it. Read by the
//     charset stage under ReadOptions.SkipTextAttachmentDecoding, its refusal
//     keeps that stage from running; transfer decoding, which does not depend
//     on it, has already been set up. Read later — by EffectiveContentDisposition,
//     mail.Reader or a walk callback — it can only refuse the entity: the
//     decoders chosen at construction stay, and bytes already read stay read.
//
// Forensic adds nothing here because the raw field is already preserved byte
// for byte in the header, for every profile; the raw spans Forensic promises
// in general are ROB-110. Every Compatibility value takes the same choices.
// Read, ReadWithOptions, New and the Header accessors ContentType and
// ContentDisposition repair nothing.

// A fieldReading is how the parse engine read one parameterized header field:
// the interpretation, the diagnostics it is owed, and the error that goes with
// it.
type fieldReading struct {
	value  string
	params map[string]string
	owed   []Diagnostic
	err    error

	// unframed reports that a repair kept a multipart media type but no
	// boundary it may use; see "A multipart that kept no boundary" above.
	unframed bool

	// cancelled reports that the parse's context was cancelled while the field
	// was being read, and that nothing else here is an interpretation of it: a
	// reading cut short is never handed on as a complete one, and nothing it
	// owed is recorded. See cancel.go.
	cancelled bool
}

// fieldInvalidCode returns the diagnostic code of a malformed field.
func fieldInvalidCode(field string) Code {
	if field == "Content-Disposition" {
		return CodeMIMEDispositionInvalid
	}
	return CodeMIMEContentTypeInvalid
}

// parameterNoteOrder is the order a field's parameter diagnostics are recorded
// in.
var parameterNoteOrder = []Code{
	CodeMIMEParameterUnquotedValue,
	CodeMIMEParameterUnclosedQuote,
	CodeMIMEParameterInvalidEscape,
	CodeMIMEParameterDuplicate,
	CodeMIMEParameterSegmentMissing,
	CodeMIMEParameterSegmentDuplicate,
	CodeMIMEParameterBadPercentEscape,
	CodeMIMEParameterUnknownCharset,
	CodeMIMEParameterFallbackCharset,
	CodeMIMEParameterRFC2047,
	CodeMIMEParameterRawUTF8,
}

// healParameterized reads a field item by item, from the occurrences the
// budget scan retained, and returns the interpretation with the diagnostics it
// is owed; see the top of this file.
//
// scan is what scanParameters returned for raw; a refusal in it means a budget
// cut the field short. wholeErr is the error of the whole-field parse when the
// caller made one, and nil otherwise: a field that parser refused is never
// handed back as if nothing were wrong with it. The returned diagnostics are
// complete and in a fixed order. Under ProfileStrict a field that needs repair
// has an empty value, nil parameters, and an entity-scoped FatalError carrying
// the first of them.
//
// dec decodes the encoded words and charsets of the items, hint supplies the
// charset to try for raw 8-bit bytes, and noWords leaves encoded words
// undecoded, as for readTolerant.
func (c parseContext) healParameterized(field, raw string, scan parameterScan, dec *metadataDecoder, hint func() string, noWords bool, wholeErr error) fieldReading {
	poll := c.cancelPoll()
	strict := c.session != nil && c.session.Profile() == ProfileStrict
	t := readTolerant(tolerantInput{
		field: field, raw: raw, scan: scan, dec: dec, poll: poll, hint: hint, noHint: strict, noWords: noWords,
		fallback: c.cdParamFallback(),
	})
	if t.cancelled || (poll != nil && poll()) {
		// A reading the cancellation reached is discarded whole.
		return fieldReading{cancelled: true}
	}

	// unexplained: the whole-field parser refused the field and nothing found
	// here says why. It is still a field that needed this reading.
	unexplained := wholeErr != nil && !t.needsRepair()
	repair := unexplained || t.needsRepair()
	if !repair && len(t.notes) == 0 {
		return fieldReading{value: t.base, params: t.params}
	}

	refused := strict && repair
	unframed := !strict && repair && field == "Content-Type" &&
		strings.HasPrefix(t.base, "multipart/") && t.params["boundary"] == ""

	action := t.action(field, unframed)
	if refused {
		action = strictAction(field)
	}
	var owed []Diagnostic
	add := func(code Code, severity Severity, refuse bool, message string) {
		switch {
		case refused && !refuse:
			// Nothing of a refused field was interpreted, so nothing is said
			// about how its values were read.
			return
		case refuse && (refused || unframed):
			severity = SeverityInvalidMetadata
		}
		owed = append(owed, c.completeHeader(Diagnostic{
			Code: code, Severity: severity, Field: field, Message: message, Action: action,
		}))
	}

	invalid := fieldInvalidCode(field)
	if t.baseCode != "" && t.baseCode != invalid {
		add(t.baseCode, SeverityRecovered, true, field+" "+t.baseDetail)
	}
	if t.baseCode == invalid || t.malformed > 0 || t.empty > 0 || unexplained {
		add(invalid, SeverityRecovered, true, t.invalidMessage(field, unexplained))
	}
	for _, code := range parameterNoteOrder {
		if f := t.notes[code]; f != nil {
			add(code, f.severity, f.refuse, noteMessage(field, code, f))
		}
	}

	if refused {
		return fieldReading{owed: owed, err: newFatalDiagnostic(owed[0])}
	}
	return fieldReading{value: t.base, params: t.params, owed: owed, unframed: unframed}
}

// invalidMessage is the message of the field-level diagnostic: what is wrong
// with the base, when it is the base that is invalid, and how many items
// declare nothing.
func (t *tolerantReading) invalidMessage(field string, unexplained bool) string {
	var defects []string
	if t.malformed > 0 {
		d := fmt.Sprintf("%d malformed parameter %s", t.malformed, plural(t.malformed, "item", "items"))
		if t.unclosed {
			d += ", the last of them an unterminated quoted string running to the end of the field"
		}
		defects = append(defects, d)
	}
	if t.empty > 0 {
		defects = append(defects, fmt.Sprintf("%d empty parameter %s between semicolons",
			t.empty, plural(t.empty, "item", "items")))
	}
	if unexplained && len(defects) == 0 {
		defects = append(defects, "syntax the whole-field parser refuses")
	}
	switch {
	case t.baseCode == "":
		return fmt.Sprintf("%s has a valid %s %q but %s",
			field, baseNoun(field), t.base, strings.Join(defects, " and "))
	case t.baseCode != fieldInvalidCode(field):
		return fmt.Sprintf("%s, read as the %s %q, has %s",
			field, baseNoun(field), t.base, strings.Join(defects, " and "))
	case len(defects) == 0:
		return field + " " + t.baseDetail
	}
	return fmt.Sprintf("%s %s, and has %s", field, t.baseDetail, strings.Join(defects, " and "))
}

// noteMessage is the message of one parameter diagnostic.
func noteMessage(field string, code Code, f *finding) string {
	names := listNames(f.names)
	if f.more {
		names += " and others"
	}
	subject := fmt.Sprintf("%s %s %s", field, plural(len(f.names), "parameter", "parameters"), names)
	switch code {
	case CodeMIMEParameterUnquotedValue:
		return subject + ": an unquoted value that is not a token was read up to the next semicolon"
	case CodeMIMEParameterUnclosedQuote:
		return subject + ": a quoted string that is never closed was read to the end of the field"
	case CodeMIMEParameterInvalidEscape:
		if f.refuse {
			return subject + ": a quote or backslash inside a quoted string that escapes nothing was read literally"
		}
		return subject + ": a backslash before a character that needs no escaping was kept as a backslash"
	case CodeMIMEParameterDuplicate:
		return fmt.Sprintf("%s repeats parameters with conflicting or unreadable values: %s", field, names)
	case CodeMIMEParameterSegmentMissing:
		return subject + ": an RFC 2231 series does not run from section 0 without a gap; the segments present were joined in order"
	case CodeMIMEParameterSegmentDuplicate:
		return subject + ": an RFC 2231 section repeats with a different value; the first was kept"
	case CodeMIMEParameterBadPercentEscape:
		return subject + ": a percent sign in an RFC 2231 value is not followed by two hex digits and was kept literally"
	case CodeMIMEParameterUnknownCharset:
		switch {
		case f.refuse:
			return subject + ": an RFC 2231 extended value does not declare its charset on its first segment"
		case f.severity == SeverityUnsupported:
			return subject + ": bytes whose charset is undeclared, has no decoder, or does not match them were kept undecoded"
		}
		return subject + ": raw 8-bit bytes with no declared charset were read in the charset of the entity's Content-Type"
	case CodeMIMEParameterFallbackCharset:
		return subject + ": raw 8-bit bytes with no declared charset were read in the fallback charset, in which they are valid"
	case CodeMIMEParameterRFC2047:
		return subject + ": RFC 2047 encoded words, which a parameter value may not hold, were decoded"
	case CodeMIMEParameterRawUTF8:
		return subject + ": raw UTF-8 in a parameter value was kept"
	}
	return subject
}

// maxListedNameBytes caps one parameter name in a Message or an Action. A name
// is a token, so cutting it cannot split a character.
const maxListedNameBytes = 64

// listNames joins parameter names for a Message or an Action: at most
// maxDiagnosticNames of them, each at most maxListedNameBytes long, and a count
// of the rest. A field may name as many parameters as its budget allows, and
// what is said about it stays short all the same.
func listNames(names []string) string {
	var b strings.Builder
	for i, n := range names {
		if i == maxDiagnosticNames {
			fmt.Fprintf(&b, " and %d more", len(names)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		if len(n) > maxListedNameBytes {
			n = n[:maxListedNameBytes] + "..."
		}
		b.WriteString(n)
	}
	return b.String()
}

// action is the Action every diagnostic of the field carries: what was kept of
// it and what was not.
func (t *tolerantReading) action(field string, unframed bool) string {
	var b strings.Builder
	if t.defaulted {
		fmt.Fprintf(&b, "read the %s as %q", baseNoun(field), t.base)
	} else {
		fmt.Fprintf(&b, "kept the %s %q", baseNoun(field), t.base)
	}
	switch {
	case len(t.wellFormed) > 0 && len(t.repaired) > 0:
		fmt.Fprintf(&b, " and the well-formed parameters %s, with the repaired parameters %s",
			listNames(t.wellFormed), listNames(t.repaired))
	case len(t.wellFormed) > 0:
		fmt.Fprintf(&b, " and the well-formed parameters %s", listNames(t.wellFormed))
	case len(t.repaired) > 0:
		fmt.Fprintf(&b, " and the repaired parameters %s", listNames(t.repaired))
	default:
		b.WriteString(" with no parameters")
	}
	switch {
	case t.malformed > 0 && t.empty > 0:
		b.WriteString(", ignored every malformed item and every empty item between semicolons")
	case t.malformed > 0:
		b.WriteString(", ignored every malformed item")
	case t.empty > 0:
		b.WriteString(", ignored every empty item between semicolons")
	}
	if len(t.conflicting) > 0 {
		fmt.Fprintf(&b, ", dropped every occurrence of the conflicting %s, which is never chosen among candidates", listNames(t.conflicting))
	}
	if len(t.dropped) > 0 {
		fmt.Fprintf(&b, ", dropped the RFC 2231 continued %s", listNames(t.dropped))
	}
	if t.truncated {
		b.WriteString(", read only the occurrences within the parameter budget")
	}
	b.WriteString(", left the raw field unchanged")
	if unframed {
		b.WriteString(", found no boundary it could use and so kept the body as one opaque, uninterpreted part without reading the parts inside it")
	}
	b.WriteString(", and continued parsing")
	return b.String()
}

// validBase returns the normalized base of a field and whether it is valid on
// its own: type "/" subtype for Content-Type, one token for
// Content-Disposition.
func validBase(field, raw string) (string, bool) {
	base, _, err := mime.ParseMediaType(raw)
	if err != nil || base == "" {
		return "", false
	}
	slash := strings.IndexByte(base, '/')
	if field == "Content-Disposition" {
		return base, slash < 0
	}
	// mime accepts a lone token as a media type; RFC 2045 does not, and a
	// missing slash or subtype is ROB-050-002's.
	return base, slash > 0 && slash < len(base)-1
}

// hasUnclosedQuote reports whether a quoted string opened in item is still open
// at its end, reading quotes and escapes as the parameter scan reads them.
func hasUnclosedQuote(item string) bool {
	in := false
	for i := 0; i < len(item); i++ {
		switch item[i] {
		case '\\':
			if in && i+1 < len(item) {
				i++
			}
		case '"':
			in = !in
		}
	}
	return in
}

func strictAction(field string) string {
	if field == "Content-Disposition" {
		return "strict profile: interpreted none of this Content-Disposition, left the raw field unchanged, and refused this entity; a charset stage that depends on the disposition and has not run yet does not run, but decoding that already ran, or bytes already read, are not undone; its parent and siblings are unaffected"
	}
	return "strict profile: interpreted none of this Content-Type, returned the body as it was read with no decoder and no part iteration, left the raw field unchanged, and stopped interpreting this entity; its parent and siblings are unaffected"
}

func baseNoun(field string) string {
	if field == "Content-Disposition" {
		return "disposition"
	}
	return "media type"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
