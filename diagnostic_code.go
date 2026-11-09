package message

// A Code is the stable, machine-readable identifier of a Diagnostic.
//
// Codes are dotted lower-case tokens: a group, a dot, and a name within the
// group, for example "multipart.close_not_found". Program logic should branch
// on codes rather than on diagnostic messages.
//
// The codes declared below are reserved. Later releases may add more specific
// codes, but an existing code is never reused for another meaning.
type Code string

// Group reports the part of the code before the first dot, for example
// "multipart" for "multipart.close_not_found". It returns an empty string if
// the code has no dot.
func (c Code) Group() string {
	for i := 0; i < len(c); i++ {
		if c[i] == '.' {
			return string(c[:i])
		}
	}
	return ""
}

// Input and limit codes.
const (
	CodeInputEmpty Code = "input.empty"

	// CodeInputBOM reports a header block — the message's or a part's — that
	// begins with a byte order mark: Recovered for a UTF-8 mark, whose fields
	// are read after it, Unsupported for a UTF-16 or UTF-32 mark, whose entity
	// is kept opaque; InvalidMetadata, with the entity refused, under
	// ProfileStrict. Recorded once per block that has one (ROB-010-003).
	CodeInputBOM Code = "input.bom"

	CodeInputReadError         Code = "input.read_error"
	CodeInputNoProgress        Code = "input.no_progress"
	CodeInputCancelled         Code = "input.cancelled"
	CodeInputTrailingMessages  Code = "input.trailing_messages"
	CodeLimitInputBytes        Code = "limit.input_bytes"
	CodeLimitHeaderBytes       Code = "limit.header_bytes"
	CodeLimitHeaderFieldBytes  Code = "limit.header_field_bytes"
	CodeLimitHeaderCount       Code = "limit.header_count"
	CodeLimitLineBytes         Code = "limit.line_bytes"
	CodeLimitTotalHeaderBytes  Code = "limit.total_header_bytes"
	CodeLimitTotalHeaderCount  Code = "limit.total_header_count"
	CodeLimitParameterCount    Code = "limit.parameter_count"
	CodeLimitParameterSegments Code = "limit.parameter_segments"
	CodeLimitMIMEDepth         Code = "limit.mime_depth"
	CodeLimitMessageDepth      Code = "limit.message_depth"
	CodeLimitPartCount         Code = "limit.part_count"
	CodeLimitPreambleBytes     Code = "limit.preamble_bytes"
	CodeLimitEpilogueBytes     Code = "limit.epilogue_bytes"
	CodeLimitEncodedBodyBytes  Code = "limit.encoded_body_bytes"
	CodeLimitDecodedBodyBytes  Code = "limit.decoded_body_bytes"
	CodeLimitTotalDecodedBytes Code = "limit.total_decoded_bytes"
	CodeLimitDiagnosticCount   Code = "limit.diagnostic_count"
	CodeLimitMemorySpoolBytes  Code = "limit.memory_spool_bytes"
	CodeLimitDiskSpoolBytes    Code = "limit.disk_spool_bytes"
)

// Header framing and field codes.
const (
	CodeHeaderFirstLineContinuation   Code = "header.first_line_continuation"
	CodeHeaderOrphanContinuation      Code = "header.orphan_continuation"
	CodeHeaderMissingBodySeparator    Code = "header.missing_body_separator"
	CodeHeaderWhitespaceBodySeparator Code = "header.whitespace_body_separator"
	CodeHeaderMissingColon            Code = "header.missing_colon"
	CodeHeaderSemicolonSeparator      Code = "header.semicolon_separator"
	CodeHeaderEmptyName               Code = "header.empty_name"
	CodeHeaderInvalidName             Code = "header.invalid_name"
	CodeHeaderNameWhitespace          Code = "header.name_whitespace"
	CodeHeaderInvalidUTF8             Code = "header.invalid_utf8"
	CodeHeaderNUL                     Code = "header.nul"

	// CodeHeaderBareLF reports a header line ended by a '\n' with no '\r'
	// before it; recorded once per parse, at the first.
	CodeHeaderBareLF Code = "header.bare_lf"

	// CodeHeaderBareCR reports a header line ended by a '\r' not followed by
	// '\n'; recorded once per parse, at the first, and for each entity
	// ProfileStrict refuses for it.
	CodeHeaderBareCR Code = "header.bare_cr"

	// CodeHeaderMixedLineEndings reports a header block whose lines end in
	// more than one way; recorded once per parse.
	CodeHeaderMixedLineEndings Code = "header.mixed_line_endings"

	// CodeHeaderTruncatedLine reports a header block whose last line the input
	// ended, with io.EOF, before any line ending; recorded for each such block.
	CodeHeaderTruncatedLine Code = "header.truncated_line"

	CodeHeaderDuplicateSingleton    Code = "header.duplicate_singleton"
	CodeHeaderMboxEnvelope          Code = "header.mbox_envelope"
	CodeHeaderMisplacedMboxEnvelope Code = "header.misplaced_mbox_envelope"

	// CodeHeaderDecoderNoProgress reports that a CharsetReader's reader of an
	// encoded word in a Content-Type or Content-Disposition parameter the
	// parse engine interpreted returned no bytes and no error on 100
	// consecutive reads without taking input; that value was kept raw, the
	// field's other values were decoded, and the field records one such
	// diagnostic however many of its values stalled.
	CodeHeaderDecoderNoProgress Code = "header.decoder_no_progress"
)

// Body decoding codes.
const (
	// CodeBodyDecoderNoProgress reports that the decoders of one leaf body
	// returned no bytes and no error on 100 consecutive reads during which no
	// stage took input — neither a wire byte of the body nor a byte of the
	// charset stage's input; that body was ended, and nothing wider.
	CodeBodyDecoderNoProgress Code = "body.decoder_no_progress"
)

// MIME media type, disposition and parameter codes.
const (
	CodeMIMEContentTypeInvalid            Code = "mime.content_type_invalid"
	CodeMIMEContentTypeMissingType        Code = "mime.content_type_missing_type"
	CodeMIMEContentTypeMissingSubtype     Code = "mime.content_type_missing_subtype"
	CodeMIMEContentTypeMissingSlash       Code = "mime.content_type_missing_slash"
	CodeMIMEDispositionInvalid            Code = "mime.disposition_invalid"
	CodeMIMEParameterMissingName          Code = "mime.parameter_missing_name"
	CodeMIMEParameterMissingEquals        Code = "mime.parameter_missing_equals"
	CodeMIMEParameterMissingValue         Code = "mime.parameter_missing_value"
	CodeMIMEParameterUnquotedValue        Code = "mime.parameter_unquoted_value"
	CodeMIMEParameterUnclosedQuote        Code = "mime.parameter_unclosed_quote"
	CodeMIMEParameterInvalidEscape        Code = "mime.parameter_invalid_escape"
	CodeMIMEParameterDuplicate            Code = "mime.parameter_duplicate"
	CodeMIMEParameterRawUTF8              Code = "mime.parameter_raw_utf8"
	CodeMIMEParameterRFC2047              Code = "mime.parameter_rfc2047"
	CodeMIMEParameterBadPercentEscape     Code = "mime.parameter_bad_percent_escape"
	CodeMIMEParameterSegmentMissing       Code = "mime.parameter_segment_missing"
	CodeMIMEParameterSegmentDuplicate     Code = "mime.parameter_segment_duplicate"
	CodeMIMEParameterSegmentMixedEncoding Code = "mime.parameter_segment_mixed_encoding"
	CodeMIMEParameterUnknownCharset       Code = "mime.parameter_unknown_charset"
	CodeMIMEFilenameConflict              Code = "mime.filename_conflict"
)

// Multipart codes.
const (
	CodeMultipartBoundaryMissing        Code = "multipart.boundary_missing"
	CodeMultipartBoundaryEmpty          Code = "multipart.boundary_empty"
	CodeMultipartBoundaryInvalid        Code = "multipart.boundary_invalid"
	CodeMultipartBoundaryTooLong        Code = "multipart.boundary_too_long"
	CodeMultipartBoundaryInferred       Code = "multipart.boundary_inferred"
	CodeMultipartBoundaryMismatch       Code = "multipart.boundary_mismatch"
	CodeMultipartBoundaryAmbiguous      Code = "multipart.boundary_ambiguous"
	CodeMultipartStartNotFound          Code = "multipart.start_not_found"
	CodeMultipartCloseNotFound          Code = "multipart.close_not_found"
	CodeMultipartCloseTruncated         Code = "multipart.close_truncated"
	CodeMultipartInvalidDelimiterSuffix Code = "multipart.invalid_delimiter_suffix"
	// CodeMultipartMixedLineEndings reports one multipart whose delimiter
	// lines end, or are preceded by, line endings of more than one kind.
	CodeMultipartMixedLineEndings Code = "multipart.mixed_line_endings"
	// CodeMultipartBareCR reports a bare CR before or after a multipart
	// delimiter line; ProfileStrict refuses the rest of that multipart for it.
	CodeMultipartBareCR Code = "multipart.bare_cr"
	// CodeMultipartBareLF reports a bare LF before or after a multipart
	// delimiter line.
	CodeMultipartBareLF Code = "multipart.bare_lf"

	CodeMultipartEmpty                   Code = "multipart.empty"
	CodeMultipartEmptyPart               Code = "multipart.empty_part"
	CodeMultipartPartHeaderMissing       Code = "multipart.part_header_missing"
	CodeMultipartPartSeparatorMissing    Code = "multipart.part_separator_missing"
	CodeMultipartNestedBoundaryReused    Code = "multipart.nested_boundary_reused"
	CodeMultipartFalseBoundaryCandidate  Code = "multipart.false_boundary_candidate"
	CodeMultipartTrailingJunk            Code = "multipart.trailing_junk"
	CodeMultipartIllegalTransferEncoding Code = "multipart.illegal_transfer_encoding"
)

// Content-Transfer-Encoding codes.
const (
	CodeCTEUnknown                Code = "cte.unknown"
	CodeCTEInvalidToken           Code = "cte.invalid_token"
	CodeCTEIllegalForMultipart    Code = "cte.illegal_for_multipart"
	CodeCTEIllegalForMessage      Code = "cte.illegal_for_message"
	CodeCTESevenBitViolation      Code = "cte.seven_bit_violation"
	CodeCTEBase64Junk             Code = "cte.base64_junk"
	CodeCTEBase64PaddingMissing   Code = "cte.base64_padding_missing"
	CodeCTEBase64PaddingInvalid   Code = "cte.base64_padding_invalid"
	CodeCTEBase64DataAfterPadding Code = "cte.base64_data_after_padding"
	CodeCTEBase64LengthInvalid    Code = "cte.base64_length_invalid"
	CodeCTEBase64Truncated        Code = "cte.base64_truncated"
	CodeCTEBase64URLAlphabet      Code = "cte.base64_url_alphabet"
	CodeCTEQPInvalidHex           Code = "cte.qp_invalid_hex"
	CodeCTEQPTrailingEquals       Code = "cte.qp_trailing_equals"
	CodeCTEQPInvalidSoftBreak     Code = "cte.qp_invalid_soft_break"
	CodeCTEQPControlByte          Code = "cte.qp_control_byte"
	CodeCTEQPOverlongLine         Code = "cte.qp_overlong_line"
	CodeCTEUUEncodeInvalid        Code = "cte.uuencode_invalid"
	CodeCTEUUEncodeMissingBegin   Code = "cte.uuencode_missing_begin"
	CodeCTEUUEncodeMissingEnd     Code = "cte.uuencode_missing_end"
)

// Charset and RFC 2047 encoded-word codes.
const (
	CodeCharsetMissing            Code = "charset.missing"
	CodeCharsetEmpty              Code = "charset.empty"
	CodeCharsetUnknown            Code = "charset.unknown"
	CodeCharsetAliasRecovered     Code = "charset.alias_recovered"
	CodeCharsetBOMMismatch        Code = "charset.bom_mismatch"
	CodeCharsetInvalidSequence    Code = "charset.invalid_sequence"
	CodeCharsetDecodeTruncated    Code = "charset.decode_truncated"
	CodeCharsetDeclarationSuspect Code = "charset.declaration_suspect"
	CodeRFC2047InvalidWord        Code = "rfc2047.invalid_word"
	CodeRFC2047UnknownCharset     Code = "rfc2047.unknown_charset"
	CodeRFC2047UnknownEncoding    Code = "rfc2047.unknown_encoding"
	CodeRFC2047InvalidBase64      Code = "rfc2047.invalid_base64"
	CodeRFC2047InvalidQ           Code = "rfc2047.invalid_q"
	CodeRFC2047IllegalLocation    Code = "rfc2047.illegal_location"
	CodeRFC2047EmbeddedWord       Code = "rfc2047.embedded_word"
	CodeRFC2047RawUTF8            Code = "rfc2047.raw_utf8"
	CodeRFC2047HeaderInjection    Code = "rfc2047.header_injection"
)

// Structured metadata codes.
const (
	CodeAddressInvalid       Code = "address.invalid"
	CodeAddressPartialList   Code = "address.partial_list"
	CodeAddressUnquotedComma Code = "address.unquoted_comma"
	CodeAddressLocalOnly     Code = "address.local_only"
	CodeAddressGroupDepth    Code = "address.group_depth"
	CodeAddressCommentDepth  Code = "address.comment_depth"
	CodeAddressRawUTF8       Code = "address.raw_utf8"
	CodeDateInvalid          Code = "date.invalid"
	CodeDateObsoleteZone     Code = "date.obsolete_zone"
	CodeDateMissingZone      Code = "date.missing_zone"
	CodeDateWeekdayMismatch  Code = "date.weekday_mismatch"
	CodeMessageIDInvalid     Code = "message_id.invalid"
	CodeMessageIDPartialList Code = "message_id.partial_list"
	CodeReceivedInvalid      Code = "received.invalid"
	CodeListHeaderInvalid    Code = "list_header.invalid"
)

// Nested and special entity codes.
const (
	CodeMessageEmbeddedInvalid         Code = "message.embedded_invalid"
	CodeMessageEmbeddedTruncated       Code = "message.embedded_truncated"
	CodeMessageIllegalTransferEncoding Code = "message.illegal_transfer_encoding"
	CodeReportInvalid                  Code = "report.invalid"
	CodeSignedStructureInvalid         Code = "signed.structure_invalid"
	CodeEncryptedStructureInvalid      Code = "encrypted.structure_invalid"
	CodeFeatureUnsupportedMediaType    Code = "feature.unsupported_media_type"
)
