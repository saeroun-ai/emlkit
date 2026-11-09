package message

import (
	"fmt"
	"mime"
	"strings"
)

// Recovery of a valid base media type or disposition whose parameters are
// malformed (ROB-050-001, issue #22).
//
// # What is repaired
//
// RFC 2045 section 5.1 makes the base of a Content-Type a type, a slash and a
// subtype, and RFC 2183 makes the base of a Content-Disposition one token. When
// that base is well formed, its meaning does not depend on the parameters after
// it: "multipart/mixed; boundary=B; broken" is a multipart/mixed entity whose
// boundary is B and whose third item is damaged. The whole-field parser cannot
// say so — mime.ParseMediaType refuses the whole field over one bad item, and
// the legacy fallback then reports the raw value as if it were the media type —
// so under Parse a field whose whole parse fails is read again, item by item,
// from the occurrences the parameter budget scan already retained:
//
//   - The base is kept, normalized as the whole-field parser normalizes it,
//     only if it is valid on its own: type "/" subtype for Content-Type, a
//     single token for Content-Disposition. A missing type, subtype or slash
//     is ROB-050-002 and is left exactly as before: the raw value, no
//     parameters, and the parser's error.
//   - Every parameter item that parses on its own is kept, whether it comes
//     before or after a damaged one. Items are the top-level semicolon
//     separated items of the field, found by the quote-aware scan of
//     limit_parameters.go, so a semicolon inside a quoted string or after a
//     backslash inside one never separates anything.
//   - An item that does not parse on its own is malformed and ignored. An
//     unterminated quoted string runs to the end of the field, as the scan
//     reads it, so the item it opens is the damaged suffix: everything before
//     it is kept and nothing is guessed about what it swallowed.
//   - An empty item between semicolons — "multipart/mixed;; boundary=B",
//     "boundary=B; ;" — declares nothing but is rejected by the whole-field
//     parser, which accepts only one trailing semicolon followed by
//     whitespace. It is ignored like a malformed item and reported as an empty
//     one. It is still not a parameter occurrence: scanParameters only counts
//     it, and no budget charges it. The single trailing semicolon mime accepts
//     never reaches the repair.
//   - A parameter that occurs more than once with one value is kept with that
//     value and without comment, as the whole-field parser keeps such a
//     parameter in a field that is otherwise conformant. One that occurs with
//     different values is dropped, every occurrence: choosing one would pick,
//     for a boundary, the framing of the whole body arbitrarily. An occurrence
//     whose value is unknown — a damaged item that still names the parameter,
//     "boundary=B; boundary=\"C", or an RFC 2231 extended value whose charset
//     has no decoder — conflicts with a well-formed one in the same way.
//     Precedence between conflicting values is ROB-050-005.
//   - Every parameter that has an RFC 2231 continuation segment among the
//     retained occurrences — the whole family, well-formed segments, damaged
//     ones, and a whole parameter of the same name alike — is dropped, as the
//     parameter budgets already drop one: in a field known to be damaged the
//     series cannot be shown complete. A well-formed segment is not malformed
//     and is not reported as one; it is dropped by this deferred policy, which
//     ROB-050-006 and ROB-050-007 own.
//   - A well-formed RFC 2231 extended value whose charset has no decoder is
//     not malformed either. The whole-field parser keeps nothing for it, and
//     neither does the repair.
//
// # A multipart that kept no boundary
//
// A repair of a multipart Content-Type can end with no boundary it may use:
// the boundary was repeated with conflicting values, a damaged item named it, or
// an unterminated quote swallowed it. Nothing is chosen or invented then, and
// the media type is still reported as what the field declares. But a body with
// no known boundary cannot be framed, and iterating it would fail at once and
// cost the siblings after it to every reader that stops at the first error, so
// such an entity is unframed: MultipartReader returns nil and the body is one
// opaque, uninterpreted part holding the bytes of the input. Its diagnostics
// are SeverityInvalidMetadata, and their Action says the parts inside were not
// read. A multipart with no boundary that no repair touched keeps its existing
// behavior; inferring a boundary is ROB-060-002.
//
// # What is bounded
//
// Only the occurrences the budget scan retained are read, so a field past
// MaxParameterCount or MaxParameterSegments is never parsed whole as a second
// attempt, and with both budgets disabled the field is scanned once before it
// is read item by item. Each item is parsed once on its own, parameter names
// are tracked in map sets, and the names a diagnostic lists are kept in
// first-occurrence order rather than sorted, so the work is linear in the
// retained bytes, never a reparse of shorter and shorter prefixes.
//
// # Profiles
//
// This producer is the first to read ParseOptions.Profile, and it reads it
// locally:
//
//	Profile           result for a valid base with damaged parameters
//	Tolerant (zero)   the repair above, with diagnostics of SeverityRecovered
//	                  (SeverityInvalidMetadata for an unframed multipart)
//	Forensic          the same repair and diagnostics as Tolerant
//	Strict            no interpretation: an entity-scoped FatalError, the same
//	                  diagnostics with SeverityInvalidMetadata; see below for
//	                  what that does to the body
//
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

// Forensic adds nothing here because the raw field is already preserved
// byte for byte in the header, for every profile; the raw spans Forensic
// promises in general are ROB-110. Every Compatibility value takes the
// conservative choices above: this producer has no tie for a policy to break.
// A conformant field parses as it always has under every profile, and Read,
// ReadWithOptions, New and the Header accessors do not repair anything.

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

// repairParameterized reads a field whose whole parse failed, item by item,
// from the occurrences the budget scan retained, and reports whether it could:
// it cannot when the base itself is not valid, or when no item explains the
// failure, and the caller then reports the whole-field failure as before.
//
// scan is what scanParameters returned for raw; a refusal in it means a budget
// cut the field short. The returned diagnostics are complete, in a fixed order
// — malformed and empty items, then conflicts — and owed to the session. Under
// ProfileStrict the returned value is empty, the parameters nil, and the error
// an entity-scoped FatalError carrying the first of them.
//
// dec decodes the encoded words of the items, as for the whole-field parse.
func (c parseContext) repairParameterized(field, raw string, scan parameterScan, dec *metadataDecoder) (fieldReading, bool) {
	occurrences, truncated, empty := scan.occurrences, scan.refusal != nil, scan.strayEmpty
	base, valid := validBase(field, raw[:scan.head])
	if !valid {
		return fieldReading{}, false
	}

	var (
		malformed int
		unclosed  bool
		// order holds every parameter name in first-occurrence order, which is
		// the order every list below is reported in.
		order []string
		known = map[string]bool{}
		// count is how many whole (non-segment) occurrences declare a name,
		// whatever became of them.
		count  = map[string]int{}
		values = map[string]string{}
		// clash marks a name declared twice with different values; unknown a
		// name one of whose occurrences has a value nothing could read.
		clash     = map[string]bool{}
		unknown   = map[string]bool{}
		continued = map[string]bool{}
		undecoded = map[string]bool{}
	)
	note := func(name string) {
		if !known[name] {
			known[name] = true
			order = append(order, name)
		}
	}
	// Each item is one parse of its own, and can decode its value through the
	// configured CharsetReader, so a parse that can be cancelled is polled
	// before each; a repair the cancellation reached is discarded whole.
	poll := c.cancelPoll()
	for _, occ := range occurrences {
		if poll != nil && poll() {
			return fieldReading{cancelled: true}, true
		}
		item := raw[occ.start:occ.end]
		key, val, wellFormed, decoded := parseParameterItem(item, dec)
		switch {
		case !wellFormed:
			malformed++
			if hasUnclosedQuote(item) {
				unclosed = true
			}
			if occ.base == "" {
				continue
			}
			note(occ.base)
			if occ.segment {
				continued[occ.base] = true
				continue
			}
			count[occ.base]++
			unknown[occ.base] = true
		case occ.segment:
			// Well formed on its own, but one piece of a series; see above.
			note(occ.base)
			continued[occ.base] = true
		case !decoded:
			// Well formed, and kept by nothing: an extended value whose
			// charset has no decoder.
			note(occ.base)
			count[occ.base]++
			unknown[occ.base] = true
			undecoded[occ.base] = true
		default:
			note(key)
			count[key]++
			if prev, dup := values[key]; !dup {
				values[key] = val
			} else if prev != val {
				clash[key] = true
			}
		}
	}

	if poll != nil && poll() {
		return fieldReading{cancelled: true}, true
	}

	var kept, conflicting, dropped, unsupported []string
	for _, name := range order {
		switch {
		case continued[name]:
			// A series and a whole parameter of the same name cannot be told
			// apart from a conflict either, so neither is kept.
			dropped = append(dropped, name)
			delete(values, name)
		case count[name] > 1 && (clash[name] || unknown[name]):
			conflicting = append(conflicting, name)
			delete(values, name)
		case undecoded[name]:
			unsupported = append(unsupported, name)
		}
		if _, ok := values[name]; ok {
			kept = append(kept, name)
		}
	}

	if malformed == 0 && empty == 0 && len(conflicting) == 0 {
		// Nothing here explains why the whole field did not parse, so nothing
		// is claimed about it.
		return fieldReading{}, false
	}

	strict := c.session != nil && c.session.Profile() == ProfileStrict
	unframed := !strict && field == "Content-Type" && strings.HasPrefix(base, "multipart/") && values["boundary"] == ""

	action := repairAction(field, base, kept, conflicting, dropped, unsupported, empty > 0, truncated, unframed)
	severity := SeverityRecovered
	switch {
	case strict:
		action = strictAction(field)
		severity = SeverityInvalidMetadata
	case unframed:
		severity = SeverityInvalidMetadata
	}

	var owed []Diagnostic
	if malformed > 0 || empty > 0 {
		var defects []string
		if malformed > 0 {
			d := fmt.Sprintf("%d malformed parameter %s", malformed, plural(malformed, "item", "items"))
			if unclosed {
				d += ", the last of them an unterminated quoted string running to the end of the field"
			}
			defects = append(defects, d)
		}
		if empty > 0 {
			defects = append(defects, fmt.Sprintf("%d empty parameter %s between semicolons",
				empty, plural(empty, "item", "items")))
		}
		msg := fmt.Sprintf("%s has a valid %s %q but %s",
			field, baseNoun(field), base, strings.Join(defects, " and "))
		owed = append(owed, c.completeHeader(Diagnostic{
			Code: fieldInvalidCode(field), Severity: severity, Field: field, Message: msg, Action: action,
		}))
	}
	if len(conflicting) > 0 {
		owed = append(owed, c.completeHeader(Diagnostic{
			Code: CodeMIMEParameterDuplicate, Severity: severity, Field: field,
			Message: fmt.Sprintf("%s repeats parameters with conflicting or unreadable values: %s",
				field, strings.Join(conflicting, ", ")),
			Action: action,
		}))
	}

	if strict {
		return fieldReading{owed: owed, err: newFatalDiagnostic(owed[0])}, true
	}
	return fieldReading{value: base, params: values, owed: owed, unframed: unframed}, true
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

// parseParameterItem parses one raw parameter item on its own, exactly as the
// whole-field parser would parse it inside a field.
//
// wellFormed reports whether the item is valid syntax. decoded reports whether
// the parser also produced a value for it, and key and value are that value's
// lower-case name and decoded text. A well-formed item can produce none: an
// RFC 2231 continuation segment other than the first is only a piece of a
// value, and an extended value whose charset has no decoder is dropped by the
// whole-field parser too. dec decodes the item's encoded words.
func parseParameterItem(item string, dec *metadataDecoder) (key, value string, wellFormed, decoded bool) {
	_, params, _, err := parseHeaderWithParamsDecoder("a/b;"+item, nil, dec)
	if err != nil {
		return "", "", false, false
	}
	for k, v := range params {
		// One item declares one parameter, so this runs at most once.
		return k, v, true, true
	}
	return "", "", true, false
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

func repairAction(field, base string, kept, conflicting, dropped, unsupported []string, empty, truncated, unframed bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kept the %s %q", baseNoun(field), base)
	if len(kept) > 0 {
		fmt.Fprintf(&b, " and the well-formed parameters %s", strings.Join(kept, ", "))
	} else {
		b.WriteString(" with no parameters")
	}
	b.WriteString(", ignored every malformed item")
	if empty {
		b.WriteString(" and every empty item between semicolons")
	}
	if len(conflicting) > 0 {
		fmt.Fprintf(&b, ", dropped every occurrence of the conflicting %s", strings.Join(conflicting, ", "))
	}
	if len(dropped) > 0 {
		fmt.Fprintf(&b, ", dropped the RFC 2231 continued %s", strings.Join(dropped, ", "))
	}
	if len(unsupported) > 0 {
		fmt.Fprintf(&b, ", kept no value for the well-formed %s, for which the whole-field parser produces none either (as for an RFC 2231 extended value whose charset has no decoder)", strings.Join(unsupported, ", "))
	}
	if truncated {
		b.WriteString(", read only the occurrences within the parameter budget")
	}
	b.WriteString(", left the raw field unchanged")
	if unframed {
		b.WriteString(", found no boundary it could use and so kept the body as one opaque, uninterpreted part without reading the parts inside it")
	}
	b.WriteString(", and continued parsing")
	return b.String()
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
