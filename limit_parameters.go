package message

import (
	"fmt"
	"strings"
)

// Limits.MaxParameterCount and Limits.MaxParameterSegments, applied where the
// parse engine reads a parameterized header field — Content-Type and
// Content-Disposition — of any entity of a parse.
//
// # What is counted
//
// MaxParameterCount counts the raw syntactic parameter occurrences of one
// field: the semicolon-separated items after the media type or disposition,
// exactly as they appear, before anything is interpreted. Duplicates count
// separately, because each is an occurrence the parser has to handle, and an
// RFC 2231 continuation segment counts as one occurrence for the same reason. A
// semicolon inside a quoted string separates nothing and is not one, and an
// empty item — a stray ";;" — declares no parameter and is not one either.
//
// MaxParameterSegments counts the RFC 2231 continuation occurrences of one base
// parameter, case-insensitively: filename*0, FileName*1*, filename*2 are three
// segments of one parameter named filename. It is counted per base parameter
// and before assembly, which is the point of it — the work being bounded is the
// work of assembling them.
//
// A field at exactly a budget is parsed whole; one occurrence more is over it.
//
// # Why it is counted before parsing rather than after
//
// mime.ParseMediaType assembles a value from however many parameters and
// however many continuation segments it is given. Counting what it produced
// would mean the work was already done, so what runs first is a scan that reads
// the raw value once, allocates nothing per byte, and stops at the first
// occurrence past a budget. Limits.MaxHeaderFieldBytes bounds the same work from
// the other side, and is a backstop rather than a substitute: 64 KiB of field is
// a great many parameters.
//
// # What a failure means, and does not mean
//
// It is not fatal, and it does not cost the entity. There is a safe fallback
// here that a refused header block does not have: the media type, and every
// parameter within the budget, were parsed from bytes that are not in doubt. So
// the field is parsed from its media type and the occurrences within the
// budget, the remainder is ignored, exactly one limit.parameter_count or
// limit.parameter_segments diagnostic is recorded for that field with an Action
// saying so, and the parse continues into the body. A Content-Type that
// classifies the entity, and a boundary parameter that arrived before the
// budget ran out, therefore stay usable, so an oversized parameter list at the
// end of a field does not hide the multipart children it introduces.
//
// One thing is deliberately not kept. A parameter with any RFC 2231 continuation
// segment among the retained occurrences is dropped whole, even when its
// segments all arrived before the cut, because what an assembler would build
// from a series this scan stopped reading is a value that looks complete and may
// not be. Reporting a truncated value as the parameter's value is the one
// outcome worth losing a parameter to avoid. ROB-050 owns the wider question of
// recovering malformed and partial parameters; this item only refuses to claim
// more than it knows.

// The actions a parameter budget diagnostic reports. Both say the same two
// things: what was kept, and that parsing went on.
const (
	parameterCountAction = "kept the media type and the parameters within the budget, dropped every parameter assembled from RFC 2231 segments, ignored the rest of the field, and continued parsing the body"

	parameterSegmentsAction = "kept the media type and the parameters before the over-budget segment, dropped every parameter assembled from RFC 2231 segments, ignored the rest of the field, and continued parsing the body"
)

// A parameterOccurrence is one raw semicolon-separated parameter of a
// structured header field value, located in that value and classified.
type parameterOccurrence struct {
	// start and end bound the occurrence in the raw value, semicolons
	// excluded, so that a retained one can be written back exactly as it
	// appeared.
	start, end int

	// base is the parameter's name with any RFC 2231 suffix removed, lower
	// cased, which is the identity two occurrences of one parameter share.
	base string

	// segment reports whether this occurrence is an RFC 2231 continuation
	// segment — name*0, name*1*, and so on — as opposed to a whole parameter,
	// which includes the single extended form name*.
	segment bool
}

// A parameterRefusal is a parameter budget refusing a field: which budget, what
// it was, the base parameter it was refused for when the budget is per
// parameter, and how many occurrences were retained.
type parameterRefusal struct {
	code  Code
	limit int64
	base  string

	// kept is the number of leading occurrences within the budget. The
	// occurrence that exceeded it, and everything after it, is ignored.
	kept int
}

// boundParameters scans the raw value of a parameterized header field and
// reports the first parameter budget it exceeds, if any, together with the
// occurrences that fit.
//
// The scan is single-pass and stops at the first occurrence past a budget, so
// what it costs is bounded by the budgets themselves rather than by the field:
// it records at most one occurrence more than the larger of the two budgets
// allows, and it reads no further.
//
// A budget of zero or below is off, which is what a resolved LimitDisabled is.
//
// The two budgets are checked in a fixed order for each occurrence — the count
// of the field first, then the segments of that occurrence's parameter — so that
// a field that exceeds both at once is reported the same way every time.
func boundParameters(raw string, maxCount, maxSegments int64) (head int, occurrences []parameterOccurrence, refusal *parameterRefusal) {
	var (
		count    int64
		segments map[string]int64
	)

	s := newParameterScanner(raw)
	for {
		start, end, ok := s.next()
		if !ok {
			break
		}

		item := raw[start:end]
		if strings.TrimSpace(item) == "" {
			// A stray or trailing semicolon declares no parameter. It is not an
			// occurrence, so it is not charged as one.
			continue
		}

		base, segment := parameterName(item)

		count++
		if maxCount > 0 && count > maxCount {
			return s.head, occurrences, &parameterRefusal{
				code:  CodeLimitParameterCount,
				limit: maxCount,
				kept:  len(occurrences),
			}
		}

		if segment {
			if segments == nil {
				segments = make(map[string]int64)
			}
			segments[base]++
			if maxSegments > 0 && segments[base] > maxSegments {
				return s.head, occurrences, &parameterRefusal{
					code:  CodeLimitParameterSegments,
					limit: maxSegments,
					base:  base,
					kept:  len(occurrences),
				}
			}
		}

		occurrences = append(occurrences, parameterOccurrence{
			start:   start,
			end:     end,
			base:    base,
			segment: segment,
		})
	}

	return s.head, occurrences, nil
}

// A parameterScanner walks the semicolon-separated parameter items of a
// structured header field value, one at a time.
//
// It yields them one at a time on purpose. Collecting them all first would make
// the cost of asking how many parameters a field declares proportional to how
// many it declares, which is the cost this whole file exists to bound; a scanner
// that stops where its caller stops keeps that cost proportional to the budget
// instead.
//
// A semicolon only separates at the top level. One inside a quoted string is
// part of a value — a filename may contain one — and a backslash inside a
// quoted string escapes the byte after it, so neither a quoted semicolon nor an
// escaped quote is mistaken for syntax. An unterminated quote runs to the end of
// the value, which is what a parser has to assume when a message never closes
// one.
type parameterScanner struct {
	raw string

	// i is where the next byte is read from.
	i int

	// start is where the current item began, or -1 before the first top-level
	// semicolon has been seen — that is, while the media type or disposition is
	// still being crossed.
	start int

	// head is the index of the first top-level semicolon, which is where the
	// media type or disposition ends, or len(raw) while none has been seen.
	head int

	done bool
}

func newParameterScanner(raw string) *parameterScanner {
	return &parameterScanner{raw: raw, start: -1, head: len(raw)}
}

// next returns the bounds of the next parameter item, semicolons excluded, and
// whether there was one.
func (s *parameterScanner) next() (start, end int, ok bool) {
	if s.done {
		return 0, 0, false
	}

	inQuote := false
	for s.i < len(s.raw) {
		switch c := s.raw[s.i]; {
		case inQuote && c == '\\':
			// The escaped byte cannot end the quoted string, so it is skipped
			// whole. A trailing backslash escapes nothing and is left alone.
			if s.i+1 < len(s.raw) {
				s.i++
			}
		case c == '"':
			inQuote = !inQuote
		case c == ';' && !inQuote:
			from := s.start
			if from < 0 {
				s.head = s.i
			}
			s.start = s.i + 1
			s.i++
			if from >= 0 {
				return from, s.i - 1, true
			}
			continue
		}
		s.i++
	}

	s.done = true
	if s.start >= 0 {
		from := s.start
		s.start = -1
		return from, len(s.raw), true
	}
	return 0, 0, false
}

// parameterName returns the base name of a raw parameter item, lower cased, and
// whether the item is an RFC 2231 continuation segment.
//
// The name is what precedes the first "=", which is the attribute whether or not
// the item has a value at all. RFC 2231 spells a segment name*N, and an encoded
// segment name*N*, so an asterisk followed by digits — optionally followed by a
// second asterisk — is a section number and what precedes it is the base. The
// single extended form name* carries a whole value rather than a section of one,
// so it is not a segment; neither is anything else after an asterisk, which is
// not RFC 2231 syntax at all and is left to be whatever the assembler makes of
// it.
func parameterName(item string) (base string, segment bool) {
	attr := item
	if eq := strings.IndexByte(attr, '='); eq >= 0 {
		attr = attr[:eq]
	}
	attr = strings.TrimSpace(attr)

	star := strings.IndexByte(attr, '*')
	if star < 0 {
		return strings.ToLower(attr), false
	}

	base = strings.ToLower(attr[:star])
	section := attr[star+1:]
	section = strings.TrimSuffix(section, "*")
	if section == "" {
		// name* or name**: an extended value, not a section of one.
		return base, false
	}
	for i := 0; i < len(section); i++ {
		if section[i] < '0' || section[i] > '9' {
			return base, false
		}
	}
	return base, true
}

// retainedParameters returns the value to parse in place of raw when a budget
// refused it: the media type or disposition that head bounds, followed by the
// occurrences that fit and that can be reported without claiming more than was
// read.
//
// Each retained occurrence is written back exactly as it appeared, so what is
// parsed is a prefix of the field's own syntax rather than a rewriting of it.
func retainedParameters(raw string, head int, occurrences []parameterOccurrence) string {
	// A parameter that is assembled from continuation segments is dropped
	// whole. See the note at the top of this file: an assembler handed part of
	// a series produces a value indistinguishable from a complete one.
	var continued map[string]bool
	for _, occ := range occurrences {
		if occ.segment {
			if continued == nil {
				continued = make(map[string]bool)
			}
			continued[occ.base] = true
		}
	}

	var b strings.Builder
	b.WriteString(raw[:head])
	for _, occ := range occurrences {
		if continued[occ.base] {
			continue
		}
		b.WriteByte(';')
		b.WriteString(raw[occ.start:occ.end])
	}
	return b.String()
}

// parameterDiagnostic returns the diagnostic of a field that exceeded a
// parameter budget.
//
// The severity is SeverityLimitExceeded, as for every budget, and the Action
// says what was kept — which matters more here than for the budgets that stop a
// scope, because this one does not stop anything and a caller reading the
// resulting metadata needs to know it is a documented subset of what the field
// declared.
func parameterDiagnostic(field string, ref *parameterRefusal) Diagnostic {
	d := Diagnostic{
		Severity: SeverityLimitExceeded,
		Code:     ref.code,
		Field:    field,
	}
	if ref.code == CodeLimitParameterSegments {
		d.Message = fmt.Sprintf("%s parameter %q exceeds the limit of %d RFC 2231 segments",
			field, ref.base, ref.limit)
		d.Action = parameterSegmentsAction
		return d
	}
	d.Message = fmt.Sprintf("%s declares more than %d parameters", field, ref.limit)
	d.Action = parameterCountAction
	return d
}

// contentType parses the entity's Content-Type under this parse's parameter
// budgets. It is what the parse engine uses in place of Header.ContentType,
// whose behavior is unchanged and unbudgeted; see there.
func (c parseContext) contentType(h Header) (string, map[string]string, error) {
	v := h.Get("Content-Type")
	if v == "" {
		return "text/plain", nil, nil
	}
	return c.parseParameterized("Content-Type", v)
}

// contentDisposition parses the entity's Content-Disposition under this parse's
// parameter budgets, on the same terms as contentType.
func (c parseContext) contentDisposition(h Header) (string, map[string]string, error) {
	return c.parseParameterized("Content-Disposition", h.Get("Content-Disposition"))
}

// isAttachment reports whether the entity's Content-Disposition is "attachment",
// reading that field under this parse's parameter budgets.
func (c parseContext) isAttachment(h Header) bool {
	disp, _, err := c.contentDisposition(h)
	return err == nil && strings.EqualFold(disp, "attachment")
}

// parseParameterized parses one parameterized header field under this parse's
// parameter budgets.
//
// A parse that budgets nothing — which is every entry point but Parse — parses
// the field exactly as the public accessors do, so nothing about those paths
// changes.
//
// Otherwise the raw value is scanned first and parsed second, and when a budget
// refused it what is parsed is the retained prefix rather than the whole field.
// The diagnostic is recorded before the parse, so that it is recorded whether or
// not what remains turns out to be parseable at all.
//
// A field whose retained prefix does not parse is reported exactly as an
// unparseable field has always been reported: the raw value as the media type,
// no parameters, and the error. The whole value is deliberately not parsed as a
// second attempt — reaching for the assembler this scan exists to bound would
// give back the work it saved.
func (c parseContext) parseParameterized(field, raw string) (string, map[string]string, error) {
	limits, ok := c.limits()
	if !ok || (limits.MaxParameterCount <= 0 && limits.MaxParameterSegments <= 0) {
		// Nothing to bound, so nothing is scanned: a scan that can refuse
		// nothing would only walk the field to reach the same answer.
		return parseHeaderWithParams(raw)
	}

	head, occurrences, refusal := boundParameters(raw, limits.MaxParameterCount, limits.MaxParameterSegments)
	if refusal == nil {
		return parseHeaderWithParams(raw)
	}

	// A parse a sink already stopped records nothing further: the budget still
	// bounds the work, because the work is what it is for, but a diagnostic
	// about a parse that has ended is not what the caller asked to be told.
	if c.fatalErr() == nil {
		// The failure of a sink here is picked up by the scope that called this,
		// which reads it from the session as it reads every other stop.
		_ = c.recordHeader(parameterDiagnostic(field, refusal))
	}

	kept := retainedParameters(raw, head, occurrences)
	f, params, err := parseHeaderWithParams(kept)
	if err != nil {
		return raw, nil, err
	}
	return f, params, nil
}
