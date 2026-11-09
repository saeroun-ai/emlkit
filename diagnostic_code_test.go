package message

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// reservedCodes lists every diagnostic code reserved by
// docs/robustness-roadmap.md section 5.4. Codes are reserved from the
// beginning: an implementation may add more specific codes, but it must never
// drop one of these or reuse it for another meaning.
const reservedCodes = `
input.empty
input.bom
input.read_error
input.no_progress
input.cancelled
input.trailing_messages
limit.input_bytes
limit.header_bytes
limit.header_field_bytes
limit.header_count
limit.line_bytes
limit.total_header_bytes
limit.total_header_count
limit.parameter_count
limit.parameter_segments
limit.mime_depth
limit.message_depth
limit.part_count
limit.preamble_bytes
limit.epilogue_bytes
limit.encoded_body_bytes
limit.decoded_body_bytes
limit.total_decoded_bytes
limit.diagnostic_count
limit.memory_spool_bytes
limit.disk_spool_bytes
header.first_line_continuation
header.orphan_continuation
header.missing_body_separator
header.whitespace_body_separator
header.missing_colon
header.semicolon_separator
header.empty_name
header.invalid_name
header.name_whitespace
header.invalid_utf8
header.nul
header.bare_lf
header.bare_cr
header.mixed_line_endings
header.truncated_line
header.duplicate_singleton
header.mbox_envelope
header.misplaced_mbox_envelope
header.decoder_no_progress
mime.content_type_invalid
mime.content_type_missing_type
mime.content_type_missing_subtype
mime.content_type_missing_slash
mime.disposition_invalid
mime.parameter_missing_name
mime.parameter_missing_equals
mime.parameter_missing_value
mime.parameter_unquoted_value
mime.parameter_unclosed_quote
mime.parameter_invalid_escape
mime.parameter_duplicate
mime.parameter_raw_utf8
mime.parameter_rfc2047
mime.parameter_bad_percent_escape
mime.parameter_segment_missing
mime.parameter_segment_duplicate
mime.parameter_segment_mixed_encoding
mime.parameter_unknown_charset
mime.filename_conflict
multipart.boundary_missing
multipart.boundary_empty
multipart.boundary_invalid
multipart.boundary_too_long
multipart.boundary_inferred
multipart.boundary_mismatch
multipart.boundary_ambiguous
multipart.start_not_found
multipart.close_not_found
multipart.close_truncated
multipart.invalid_delimiter_suffix
multipart.mixed_line_endings
multipart.empty
multipart.empty_part
multipart.part_header_missing
multipart.part_separator_missing
multipart.nested_boundary_reused
multipart.false_boundary_candidate
multipart.trailing_junk
multipart.illegal_transfer_encoding
cte.unknown
cte.invalid_token
cte.illegal_for_multipart
cte.illegal_for_message
cte.seven_bit_violation
cte.base64_junk
cte.base64_padding_missing
cte.base64_padding_invalid
cte.base64_data_after_padding
cte.base64_length_invalid
cte.base64_truncated
cte.base64_url_alphabet
cte.qp_invalid_hex
cte.qp_trailing_equals
cte.qp_invalid_soft_break
cte.qp_control_byte
cte.qp_overlong_line
cte.uuencode_invalid
cte.uuencode_missing_begin
cte.uuencode_missing_end
charset.missing
charset.empty
charset.unknown
charset.alias_recovered
charset.bom_mismatch
charset.invalid_sequence
charset.decode_truncated
charset.declaration_suspect
rfc2047.invalid_word
rfc2047.unknown_charset
rfc2047.unknown_encoding
rfc2047.invalid_base64
rfc2047.invalid_q
rfc2047.illegal_location
rfc2047.embedded_word
rfc2047.raw_utf8
rfc2047.header_injection
address.invalid
address.partial_list
address.unquoted_comma
address.local_only
address.group_depth
address.comment_depth
address.raw_utf8
date.invalid
date.obsolete_zone
date.missing_zone
date.weekday_mismatch
message_id.invalid
message_id.partial_list
received.invalid
list_header.invalid
message.embedded_invalid
message.embedded_truncated
message.illegal_transfer_encoding
report.invalid
signed.structure_invalid
encrypted.structure_invalid
feature.unsupported_media_type
body.decoder_no_progress
`

// declaredCodes parses the registry source and returns every declared Code
// constant, keyed by its value. Parsing the source rather than a hand-written
// list makes the uniqueness check cover constants added later.
func declaredCodes(t *testing.T) map[string]string {
	t.Helper()

	const filename = "diagnostic_code.go"
	f, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("failed to parse %s: %v", filename, err)
	}

	byValue := make(map[string]string)
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "Code" {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					t.Fatalf("%s: constant %s has no value", filename, name.Name)
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: constant %s is not a string literal", filename, name.Name)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: constant %s: %v", filename, name.Name, err)
				}
				if previous, dup := byValue[s]; dup {
					t.Errorf("code %q is declared by both %s and %s", s, previous, name.Name)
				}
				byValue[s] = name.Name
			}
		}
	}
	if len(byValue) == 0 {
		t.Fatalf("%s: no Code constants found", filename)
	}
	return byValue
}

func TestCodes_reservedAreDeclared(t *testing.T) {
	declared := declaredCodes(t)
	for code := range strings.FieldsSeq(reservedCodes) {
		if _, ok := declared[code]; !ok {
			t.Errorf("reserved code %q is not declared in the registry", code)
		}
	}
}

func TestCodes_format(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	for code, name := range declaredCodes(t) {
		if !valid.MatchString(code) {
			t.Errorf("%s: code %q is not a dotted lower-case token", name, code)
		}
	}
}

func TestCode_Group(t *testing.T) {
	tests := []struct {
		code Code
		want string
	}{
		{CodeMultipartCloseNotFound, "multipart"},
		{CodeMessageIDInvalid, "message_id"},
		{Code(""), ""},
		{Code("nodot"), ""},
	}
	for _, test := range tests {
		if got := test.code.Group(); got != test.want {
			t.Errorf("Code(%q).Group() = %q, want %q", string(test.code), got, test.want)
		}
	}
}

// TestCodes_values pins a sample of constant-to-value mappings so a rename
// cannot silently point a constant at a different reserved code.
func TestCodes_values(t *testing.T) {
	tests := []struct {
		code Code
		want string
	}{
		{CodeInputEmpty, "input.empty"},
		{CodeLimitDiagnosticCount, "limit.diagnostic_count"},
		{CodeLimitTotalHeaderBytes, "limit.total_header_bytes"},
		{CodeLimitTotalHeaderCount, "limit.total_header_count"},
		{CodeHeaderMissingBodySeparator, "header.missing_body_separator"},
		{CodeMIMEParameterSegmentMixedEncoding, "mime.parameter_segment_mixed_encoding"},
		{CodeMultipartCloseNotFound, "multipart.close_not_found"},
		{CodeCTEBase64PaddingMissing, "cte.base64_padding_missing"},
		{CodeRFC2047InvalidQ, "rfc2047.invalid_q"},
		{CodeFeatureUnsupportedMediaType, "feature.unsupported_media_type"},
	}
	for _, test := range tests {
		if string(test.code) != test.want {
			t.Errorf("code = %q, want %q", string(test.code), test.want)
		}
	}
}
