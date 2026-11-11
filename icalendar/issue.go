package icalendar

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// replacement stands in for invalid UTF-8.
const replacement = string(utf8.RuneError)

// A Code names one kind of repair or limit. Codes are stable strings, grouped
// by a prefix: input, line, param, structure, calendar, property, event,
// value, datetime, address, rrule, attachment and limit.
type Code string

// Repairs of the input and its lines.
const (
	CodeBOM            Code = "input.bom"             // a UTF-8 byte order mark was dropped
	CodeUTF16          Code = "input.utf16"           // the input was UTF-16 with a byte order mark and was converted
	CodeBareLineEnding Code = "line.bare_ending"      // a line ended in a bare LF or CR
	CodeEmptyLine      Code = "line.empty"            // an empty line was skipped
	CodeLeadingFold    Code = "line.leading_fold"     // the input began with a folded line
	CodeFoldSplitsUTF8 Code = "line.fold_splits_utf8" // a fold fell inside a UTF-8 sequence; unfolding rejoined it
	CodeNoColon        Code = "line.no_colon"         // a line had no colon and was dropped
	CodeBadName        Code = "line.bad_name"         // a line's name was not a name and the line was dropped
	CodeNameSpace      Code = "line.name_space"       // white space around a name was removed
	CodeNameCase       Code = "line.name_case"        // a name was not upper case; names are case-insensitive
	CodeUnfoldedText   Code = "line.unfolded_text"    // a line that is not a property was joined to the text before it
)

// Repairs of parameters.
const (
	CodeParamNoValue     Code = "param.no_value"           // a parameter had no "=" or no name
	CodeParamQuote       Code = "param.unterminated_quote" // a quoted value did not close; it was read unquoted
	CodeParamAfterQuote  Code = "param.after_quote"        // text after a closing quote was kept in the value
	CodeParamUnquotedURI Code = "param.unquoted_uri"       // a URI parameter was not quoted; its colon was kept in it
	CodeParamEncodedWord Code = "param.encoded_word"       // a parameter held an RFC 2047 encoded-word and was decoded
)

// Repairs of the component structure.
const (
	CodeBeforeBegin       Code = "structure.before_begin"       // lines before the first BEGIN were skipped
	CodeAfterEnd          Code = "structure.after_end"          // lines after the last END were skipped
	CodeMissingCalendar   Code = "structure.missing_vcalendar"  // a component outside any VCALENDAR was given one
	CodeMissingEnd        Code = "structure.missing_end"        // a component was closed without its END
	CodeStrayEnd          Code = "structure.stray_end"          // an END that closes nothing open was ignored
	CodeBadBegin          Code = "structure.bad_begin"          // a BEGIN or END without a component name
	CodeNoCalendar        Code = "structure.no_calendar"        // the input held no VCALENDAR
	CodeMultipleCalendars Code = "structure.multiple_calendars" // more than one VCALENDAR; their components were merged
)

// Findings of the convenience layer.
const (
	CodeNoVersion        Code = "calendar.no_version"               // VCALENDAR without VERSION
	CodeVersion          Code = "calendar.version"                  // VERSION is not 2.0
	CodeMethodMismatch   Code = "calendar.method_mismatch"          // METHOD differs from the Content-Type method parameter
	CodeMethodFromType   Code = "calendar.method_from_content_type" // no METHOD; the Content-Type method parameter was used
	CodeDuplicate        Code = "property.duplicate"                // a property that occurs once occurred again; the first is used
	CodeNoUID            Code = "event.no_uid"                      // an event or to-do without UID
	CodeEndAndDuration   Code = "event.end_and_duration"            // both DTEND and DURATION
	CodeInvalidUTF8      Code = "value.invalid_utf8"                // invalid UTF-8 in a text value was replaced
	CodeBadInteger       Code = "value.bad_integer"                 // an integer value did not parse; zero is used
	CodeBadDuration      Code = "value.bad_duration"                // a DURATION did not parse
	CodeBadDateTime      Code = "datetime.invalid"                  // a date or date-time did not parse
	CodeDateTimeLoose    Code = "datetime.loose"                    // a date-time in a tolerated non-RFC form
	CodeDateNoValue      Code = "datetime.date_without_value"       // a DATE without VALUE=DATE
	CodeTZIDWithUTC      Code = "datetime.tzid_with_utc"            // a UTC date-time with a TZID; the TZID is ignored
	CodeUnknownTZID      Code = "datetime.unknown_tzid"             // a TZID that could not be resolved; the instant is unknown
	CodeZoneMapped       Code = "datetime.zone_mapped"              // a TZID that is no IANA name was resolved through a table or VTIMEZONE
	CodeAddressNoScheme  Code = "address.no_scheme"                 // a calendar address without mailto:
	CodeAddressEscape    Code = "address.bad_escape"                // a mailto: address with a bad percent escape, kept as written
	CodeRRuleNoFreq      Code = "rrule.no_freq"                     // RRULE without FREQ
	CodeRRuleInvalid     Code = "rrule.invalid"                     // an RRULE part did not parse
	CodeRRuleCountUntil  Code = "rrule.count_and_until"             // RRULE with both COUNT and UNTIL
	CodeAttachmentBase64 Code = "attachment.bad_base64"             // an inline attachment did not decode
)

// Budgets. Hitting one keeps what was read before it.
const (
	CodeLimitInput      Code = "limit.input_bytes"
	CodeLimitLine       Code = "limit.line_bytes"
	CodeLimitLines      Code = "limit.lines"
	CodeLimitDepth      Code = "limit.depth"
	CodeLimitComponents Code = "limit.components"
	CodeLimitProperties Code = "limit.properties"
	CodeLimitParams     Code = "limit.params"
	CodeLimitAttendees  Code = "limit.attendees"
	CodeLimitAttachment Code = "limit.attachment_bytes"
	CodeLimitZones      Code = "limit.zones"
	CodeLimitIssues     Code = "limit.issues"
)

// An Issue is one repair the parser made, or one budget it hit.
type Issue struct {
	Code Code
	// Line is the physical line, from 1, where the content line concerned
	// begins; 0 when the issue is not about one line.
	Line int
	// Detail says what was found, for a person; it is not stable.
	Detail string
	// Count is how many times the issue occurred. Issues that can repeat on
	// every line (empty lines, bare line endings, name case and a few more)
	// are recorded once, at their first line, and counted.
	Count int
}

func (i Issue) String() string {
	s := string(i.Code)
	if i.Line > 0 {
		s = "line " + strconv.Itoa(i.Line) + ": " + s
	}
	if i.Detail != "" {
		s += ": " + i.Detail
	}
	if i.Count > 1 {
		s += " (x" + strconv.Itoa(i.Count) + ")"
	}
	return s
}

// issues collects the issues of one parse, within Limits.MaxIssues.
type issues struct {
	max    int
	list   []Issue
	once   map[Code]int
	capped bool
}

func (s *issues) add(code Code, line int, detail string) {
	if len(s.list) >= s.max {
		if !s.capped {
			s.capped = true
			s.list = append(s.list, Issue{Code: CodeLimitIssues, Detail: "later issues were not recorded", Count: 1})
		}
		return
	}
	s.list = append(s.list, Issue{Code: code, Line: line, Detail: preview(detail), Count: 1})
}

// addOnce records code the first time and counts it after.
func (s *issues) addOnce(code Code, line int, detail string) {
	if i, ok := s.once[code]; ok {
		s.list[i].Count++
		return
	}
	if len(s.list) >= s.max {
		s.add(code, line, detail)
		return
	}
	if s.once == nil {
		s.once = make(map[Code]int)
	}
	s.once[code] = len(s.list)
	s.add(code, line, detail)
}

// preview shortens text taken from the input for a Detail.
func preview(s string) string {
	const n = 80
	if len(s) > n {
		s = s[:n] + "..."
	}
	return strings.ToValidUTF8(s, replacement)
}
