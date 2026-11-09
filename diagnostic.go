package message

import (
	"strconv"
	"strings"
)

// A Severity classifies how a diagnostic affects the usability of the parsed
// result.
//
// Severities are distinct categories, not points on a scale: an incomplete
// entity, an unsupported feature and unparseable metadata are different kinds
// of outcome and none of them subsumes the others. The numeric values carry no
// ranking, so callers should switch on the severity they care about rather
// than compare severities with < or >. SeverityInfo is the zero value.
type Severity uint8

const (
	// SeverityInfo reports a nonstandard but harmless representation.
	SeverityInfo Severity = iota

	// SeverityRecovered reports that a deterministic repair was applied and
	// parsing continued.
	SeverityRecovered

	// SeverityIncomplete reports that EOF or truncation lost syntax, but the
	// partial data that was recovered is usable.
	SeverityIncomplete

	// SeverityUnsupported reports that the syntax was recognized but no
	// semantic decoder is available. The raw form stays usable.
	SeverityUnsupported

	// SeverityInvalidMetadata reports that raw content is usable but its
	// typed metadata could not be parsed.
	SeverityInvalidMetadata

	// SeverityLimitExceeded reports that a configured safety budget was
	// reached.
	SeverityLimitExceeded

	// SeverityFatal reports that the parser cannot make safe forward progress
	// for the current scope. Enclosing and already parsed scopes remain
	// usable.
	SeverityFatal
)

// String returns the canonical lower-case name of the severity, or a
// "Severity(n)" placeholder for an unknown value.
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityRecovered:
		return "recovered"
	case SeverityIncomplete:
		return "incomplete"
	case SeverityUnsupported:
		return "unsupported"
	case SeverityInvalidMetadata:
		return "invalid_metadata"
	case SeverityLimitExceeded:
		return "limit_exceeded"
	case SeverityFatal:
		return "fatal"
	}
	return "Severity(" + strconv.Itoa(int(s)) + ")"
}

// A Location describes where in the original input a Diagnostic was observed.
//
// Every part of a location is optional, because a defect discovered while
// decoding a lazily consumed body cannot always be attributed to an exact
// position. The zero Location means "position unknown".
type Location struct {
	// HasOffset reports whether Offset and EndOffset carry a meaningful
	// value. Byte offset 0 is a legitimate position, so a separate flag is
	// needed to tell the start of the input apart from an unknown position.
	HasOffset bool

	// Offset is the absolute, zero-based byte offset of the first affected
	// byte in the input passed to the parser. It is only meaningful when
	// HasOffset is true.
	Offset int64

	// EndOffset is the absolute, zero-based byte offset just past the last
	// affected byte. It is only meaningful when HasOffset is true and it is
	// greater than Offset; otherwise the affected range is unknown or empty.
	EndOffset int64

	// Line is the one-based physical line number of Offset, counting every
	// line ending observed in the input, or 0 when unknown.
	Line int

	// Column is the one-based byte column of Offset within Line, or 0 when
	// unknown.
	Column int
}

// LocationAt returns a Location for a single known byte offset.
func LocationAt(offset int64) Location {
	return Location{HasOffset: true, Offset: offset, EndOffset: offset}
}

// LocationRange returns a Location for the known, half-open byte range
// [start, end).
func LocationRange(start, end int64) Location {
	return Location{HasOffset: true, Offset: start, EndOffset: end}
}

// String returns a short human-readable rendering such as "offset=42" or
// "offset=42..50 line=3 column=1". It returns "unknown" for the zero value.
func (l Location) String() string {
	var b strings.Builder
	if l.HasOffset {
		b.WriteString("offset=")
		b.WriteString(strconv.FormatInt(l.Offset, 10))
		if l.EndOffset > l.Offset {
			b.WriteString("..")
			b.WriteString(strconv.FormatInt(l.EndOffset, 10))
		}
	}
	if l.Line > 0 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("line=")
		b.WriteString(strconv.Itoa(l.Line))
	}
	if l.Column > 0 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("column=")
		b.WriteString(strconv.Itoa(l.Column))
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// A Diagnostic describes one defect that was observed in a message, or one
// repair that was applied to it. Recovery never silently changes the
// interpretation of a message: every deviation from the input is reported as a
// Diagnostic, up to Limits.MaxDiagnosticCount. The request that reaches that
// budget is replaced by one limit.diagnostic_count diagnostic saying the
// reporting stopped there, and every later deviation is still applied but no
// longer reported.
//
// Diagnostics are collected by a ParseSession in discovery order.
type Diagnostic struct {
	// Code is the stable identifier of the observation. Program logic should
	// branch on Code, never on Message.
	Code Code

	// Severity classifies the impact on the parsed result.
	Severity Severity

	// Message is a human-readable description. It is not part of the API
	// contract and may change between releases.
	Message string

	// Path is the MIME path of the affected entity, as a list of zero-based
	// part indices, using the same convention as WalkFunc: the root entity
	// has a nil path.
	Path []int

	// Field is the name of the affected header field, if any. It is stored as
	// it appeared in the input.
	Field string

	// Action describes the repair or fallback that was applied, and is empty
	// when nothing was changed.
	Action string

	// Location is the best known position of the observation in the input.
	Location Location

	// Cause is the wrapped low-level error that triggered the diagnostic, if
	// any.
	Cause error
}

// Clone returns a copy of the diagnostic whose Path is a distinct slice, so
// the two copies can be retained and their paths modified independently.
//
// Clone is not a deep copy of everything the diagnostic refers to: Cause is an
// arbitrary error value and is retained as-is, so both copies observe the same
// error and whatever it wraps or points to.
func (d Diagnostic) Clone() Diagnostic {
	if d.Path != nil {
		path := make([]int, len(d.Path))
		copy(path, d.Path)
		d.Path = path
	}
	return d
}

// String returns a human-readable rendering of the diagnostic, intended for
// logs and tests. It is not a stable machine format, and string fields are not
// escaped, so input-derived values may contain line breaks.
func (d Diagnostic) String() string {
	var b strings.Builder
	b.WriteString(d.Severity.String())
	b.WriteByte(' ')
	if d.Code == "" {
		b.WriteString("(no code)")
	} else {
		b.WriteString(string(d.Code))
	}
	if len(d.Path) > 0 {
		b.WriteString(" path=")
		b.WriteString(formatPath(d.Path))
	}
	if d.Location.HasOffset || d.Location.Line > 0 || d.Location.Column > 0 {
		b.WriteByte(' ')
		b.WriteString(d.Location.String())
	}
	if d.Field != "" {
		b.WriteString(" field=")
		b.WriteString(d.Field)
	}
	if d.Message != "" {
		b.WriteString(": ")
		b.WriteString(d.Message)
	}
	if d.Action != "" {
		b.WriteString(" (action: ")
		b.WriteString(d.Action)
		b.WriteByte(')')
	}
	if d.Cause != nil {
		b.WriteString(": ")
		b.WriteString(d.Cause.Error())
	}
	return b.String()
}

// formatPath renders a MIME part path as dot-separated zero-based indices.
func formatPath(path []int) string {
	var b strings.Builder
	for i, idx := range path {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(idx))
	}
	return b.String()
}
