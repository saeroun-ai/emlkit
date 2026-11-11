package mail

import "github.com/saeroun-ai/emlkit"

// Diagnostic codes of the report model (ROB-090-005). See report.go and
// docs/robustness-notes/reports.md. The shape check of ReadTree reports a
// multipart/report whose parts do not fit with message.CodeReportInvalid;
// these codes say what was found inside the report.
const (
	// CodeReportMissingField reports a field the report's RFC requires and the
	// report lacks: Reporting-MTA, Final-Recipient, Action or Status of a
	// delivery status, Final-Recipient or Disposition of a disposition
	// notification, Feedback-Type, User-Agent or Version of a feedback report,
	// or a delivery status with no recipient at all. Recovered when a value
	// could be inferred, invalid_metadata when it stays empty.
	CodeReportMissingField message.Code = "report.missing_field"

	// CodeReportInvalidStatus reports a Status that is not a bare RFC 3463
	// class.subject.detail code: recovered when one was read out of the text
	// around it or out of an SMTP reply code, invalid_metadata when none was.
	CodeReportInvalidStatus message.Code = "report.invalid_status"

	// CodeReportInvalidAction reports an Action that is not one of failed,
	// delayed, delivered, relayed or expanded: recovered when a known variant
	// was mapped to one of them, invalid_metadata when not.
	CodeReportInvalidAction message.Code = "report.invalid_action"

	// CodeReportInvalidField reports any other field whose value does not
	// read as its RFC writes it: a typed value with no type, angle brackets
	// around an address, a Disposition missing its mode, an unknown feedback
	// type, an unparseable Source-IP or Incidents.
	CodeReportInvalidField message.Code = "report.invalid_field"

	// CodeReportMisplacedField reports a field found outside the group it
	// belongs to — a per-message field of a delivery status among a
	// recipient's fields, or report fields in the report part's MIME header —
	// and read where it belongs.
	CodeReportMisplacedField message.Code = "report.misplaced_field"

	// CodeReportDuplicateField reports a field given twice in a group where
	// it may appear once. The first is used; every one stays in Fields.
	CodeReportDuplicateField message.Code = "report.duplicate_field"

	// CodeReportMissingGroupSeparator reports a delivery status whose groups
	// run into each other with no blank line between them; a new group was
	// begun where a recipient's identifying field repeats.
	CodeReportMissingGroupSeparator message.Code = "report.missing_group_separator"

	// CodeReportUnexpectedBlankLine reports a blank line inside a report that
	// is one group of fields (a disposition notification or a feedback
	// report). The fields after it are read as part of the same group.
	CodeReportUnexpectedBlankLine message.Code = "report.unexpected_blank_line"

	// CodeReportGarbageLine reports a line that is neither a field nor the
	// continuation of one. It is skipped and kept in Unparsed.
	CodeReportGarbageLine message.Code = "report.garbage_line"

	// CodeReportUnindentedContinuation reports a line of free text read as
	// the continuation of the field before it although it is not indented.
	CodeReportUnindentedContinuation message.Code = "report.unindented_continuation"

	// CodeReportNonASCII reports a field value holding bytes above 0x7f: at
	// info severity in a report whose RFC allows only ASCII, recovered when
	// the bytes are not valid UTF-8 and were replaced in the typed value.
	CodeReportNonASCII message.Code = "report.non_ascii"

	// CodeReportTypeMismatch reports a multipart/report whose report-type
	// parameter is missing or disagrees with the media type of its report
	// part, or a report part whose kind had to be taken from the parameter or
	// from its fields because its own media type names none.
	CodeReportTypeMismatch message.Code = "report.type_mismatch"

	// CodeReportMissingPart reports a multipart/report with no part that
	// reads as a report, or with no human-readable part.
	CodeReportMissingPart message.Code = "report.missing_part"

	// CodeReportTruncated reports a report part whose body was not kept
	// whole, so its fields were read from what was kept.
	CodeReportTruncated message.Code = "report.truncated"

	// CodeReportLimitExceeded reports that a report held more bytes, fields
	// or recipients than are read; what lies past the bound is not read.
	CodeReportLimitExceeded message.Code = "report.limit_exceeded"
)
