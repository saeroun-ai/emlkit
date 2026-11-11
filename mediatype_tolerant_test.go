package message

import (
	"encoding/base64"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/korean"
)

// ROB-050-002 to ROB-050-008 (issues #22, #23, #24): the tolerant reading of
// Content-Type and Content-Disposition. Every case is read three ways — by the
// session-less function, by Parse under the tolerant and forensic profiles, and
// by Parse under the strict one — and the first two must agree.

// withKoreanCharsets installs a CharsetReader that decodes EUC-KR, under the
// labels Korean mail uses for it, and ISO-8859-1, and knows no other charset.
func withKoreanCharsets(t *testing.T) {
	t.Helper()
	withCharsetReader(t, func(cs string, in io.Reader) (io.Reader, error) {
		switch cs {
		case "euc-kr", "ks_c_5601-1987", "cp949":
			return korean.EUCKR.NewDecoder().Reader(in), nil
		case "iso-8859-1":
			return charmap.ISO8859_1.NewDecoder().Reader(in), nil
		}
		return nil, fmt.Errorf("no decoder for %q", cs)
	})
}

// eucKR returns s in EUC-KR.
func eucKR(t *testing.T, s string) string {
	t.Helper()
	b, err := korean.EUCKR.NewEncoder().String(s)
	if err != nil {
		t.Fatalf("encoding %q as EUC-KR: %v", s, err)
	}
	return b
}

// percent returns every byte of s as a percent escape.
func percent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

type tolerantCase struct {
	name string
	// field is "Content-Disposition", or empty for Content-Type.
	field  string
	value  string
	base   string
	params map[string]string
	codes  []Code
	// refused marks a field that needed a repair, which ProfileStrict refuses.
	// guessed marks one whose value Tolerant reads by a guess Strict does not
	// make, so that the two differ without a refusal.
	refused bool
	guessed bool
}

func tolerantCases(t *testing.T) []tolerantCase {
	const cd = "Content-Disposition"
	hangul := eucKR(t, "한글")
	return []tolerantCase{
		// ROB-050-002: a base missing its type, its subtype or its slash.
		{name: "bare text", value: "text", base: "text/plain", codes: []Code{CodeMIMEContentTypeMissingSlash}, refused: true},
		{name: "text and a slash", value: "text/", base: "text/plain", codes: []Code{CodeMIMEContentTypeMissingSubtype}, refused: true},
		{name: "subtype html alone", value: "/html; charset=utf-8", base: "text/html",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEContentTypeMissingType}, refused: true},
		{name: "subtype pdf alone", value: "/pdf", base: "application/pdf", codes: []Code{CodeMIMEContentTypeMissingType}, refused: true},
		{name: "bare multipart with a boundary", value: "multipart; boundary=B", base: "multipart/mixed",
			params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeMissingSlash}, refused: true},
		{name: "bare image", value: "image", base: "application/octet-stream", codes: []Code{CodeMIMEContentTypeMissingSlash}, refused: true},
		{name: "whitespace for the slash", value: "text plain; charset=utf-8", base: "text/plain",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEContentTypeMissingSlash}, refused: true},
		{name: "missing semicolon after the type", value: "text/html charset=utf-8", base: "text/html",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEContentTypeInvalid}, refused: true},
		{name: "whitespace around the slash", value: "text / plain", base: "text/plain", codes: []Code{CodeMIMEContentTypeInvalid}, refused: true},
		{name: "comment after the type", value: "text/plain (plain text); charset=us-ascii", base: "text/plain",
			params: map[string]string{"charset": "us-ascii"}, codes: []Code{CodeMIMEContentTypeInvalid}, refused: true},
		{name: "parameters and no type", value: "; charset=utf-8", base: "text/plain",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEContentTypeMissingType}, refused: true},
		{name: "a parameter where the type should be", value: "charset=utf-8", base: "text/plain",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEContentTypeMissingType}, refused: true},
		{name: "nothing readable", value: "@#!", base: "application/octet-stream", codes: []Code{CodeMIMEContentTypeInvalid}, refused: true},
		{name: "media type in quotes", value: `"Text/HTML"; charset=utf-8`, base: "text/html",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEContentTypeInvalid}, refused: true},
		{name: "conformant, in upper case", value: "TEXT/HTML; Charset=UTF-8", base: "text/html", params: map[string]string{"charset": "UTF-8"}},

		// ROB-050-003: unquoted values that are not tokens.
		{name: "spaces in an unquoted name", value: "application/pdf; name=my file.pdf; x=1", base: "application/pdf",
			params: map[string]string{"name": "my file.pdf", "x": "1"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "unquoted boundary with an equals sign", value: "multipart/mixed; boundary=----=_NextPart_000_0012", base: "multipart/mixed",
			params: map[string]string{"boundary": "----=_NextPart_000_0012"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "slash, question mark and equals", value: "text/plain; name=a/b?c=d", base: "text/plain",
			params: map[string]string{"name": "a/b?c=d"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "unquoted encoded word", value: "application/pdf; name==?UTF-8?B?" + b64("한글.pdf") + "?=", base: "application/pdf",
			params: map[string]string{"name": "한글.pdf"}, codes: []Code{CodeMIMEParameterUnquotedValue, CodeMIMEParameterRFC2047}, refused: true},
		{name: "unquoted raw UTF-8 with a space", field: cd, value: "attachment; filename=한글 파일.hwp", base: "attachment",
			params: map[string]string{"filename": "한글 파일.hwp"}, codes: []Code{CodeMIMEParameterUnquotedValue, CodeMIMEParameterRawUTF8}, refused: true},
		{name: "text after the closing quote", field: cd, value: `attachment; filename="report".pdf`, base: "attachment",
			params: map[string]string{"filename": "report.pdf"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "stray quote after a token", value: `text/plain; charset=utf-8"`, base: "text/plain",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "comment after a value", value: "text/plain; charset=us-ascii (Plain text)", base: "text/plain",
			params: map[string]string{"charset": "us-ascii"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "comment after a quoted value", value: `text/plain; charset="us-ascii" (Plain text); format=flowed`, base: "text/plain",
			params: map[string]string{"charset": "us-ascii", "format": "flowed"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "missing semicolon after a charset", value: "text/plain; charset=euc-kr format=flowed", base: "text/plain",
			params: map[string]string{"charset": "euc-kr"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "parenthesis in an unquoted filename", field: cd, value: "attachment; filename=report (1)", base: "attachment",
			params: map[string]string{"filename": "report (1)"}, codes: []Code{CodeMIMEParameterUnquotedValue}, refused: true},
		{name: "stray quote before the next parameter", value: `text/plain; charset=utf-8"; format=flowed`, base: "text/plain",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEParameterUnquotedValue, CodeMIMEParameterUnclosedQuote}, refused: true},
		{name: "doubled closing quote", value: `text/plain; charset="utf-8""`, base: "text/plain",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEParameterInvalidEscape}, refused: true},
		{name: "quote opened inside an unquoted value", field: cd, value: `attachment; filename=my "file.pdf; x=1`, base: "attachment",
			params: map[string]string{"filename": `my "file.pdf; x=1`}, codes: []Code{CodeMIMEParameterUnquotedValue, CodeMIMEParameterUnclosedQuote}, refused: true},
		{name: "missing semicolon after the disposition", field: cd, value: "attachment filename=a.pdf", base: "attachment",
			params: map[string]string{"filename": "a.pdf"}, codes: []Code{CodeMIMEDispositionInvalid}, refused: true},
		{name: "a parameter where the disposition should be", field: cd, value: "filename=a.pdf", base: "attachment",
			params: map[string]string{"filename": "a.pdf"}, codes: []Code{CodeMIMEDispositionInvalid}, refused: true},
		{name: "disposition with a slash", field: cd, value: "attachment/x; filename=a.pdf", base: "attachment",
			params: map[string]string{"filename": "a.pdf"}, codes: []Code{CodeMIMEDispositionInvalid}, refused: true},

		// ROB-050-004: quotes and backslashes.
		{name: "unescaped Windows path", field: cd, value: `attachment; filename="C:\Users\a\b.txt"`, base: "attachment",
			params: map[string]string{"filename": `C:\Users\a\b.txt`}, codes: []Code{CodeMIMEParameterInvalidEscape}},
		{name: "unclosed quote", field: cd, value: `attachment; filename="abc`, base: "attachment",
			params: map[string]string{"filename": "abc"}, codes: []Code{CodeMIMEParameterUnclosedQuote}, refused: true},
		{name: "unclosed quote that swallows a parameter", field: cd, value: `attachment; filename="abc; size=3`, base: "attachment",
			params: map[string]string{"filename": "abc; size=3"}, codes: []Code{CodeMIMEParameterUnclosedQuote}, refused: true},
		{name: "unescaped quotes inside", field: cd, value: `attachment; filename="my "special" file.txt"`, base: "attachment",
			params: map[string]string{"filename": `my "special" file.txt`}, codes: []Code{CodeMIMEParameterInvalidEscape}, refused: true},
		{name: "backslash at the end", field: cd, value: `attachment; filename="trailing\`, base: "attachment",
			params: map[string]string{"filename": `trailing\`}, codes: []Code{CodeMIMEParameterUnclosedQuote, CodeMIMEParameterInvalidEscape}, refused: true},
		{name: "Windows directory before the closing quote", field: cd, value: `attachment; filename="C:\dir\"`, base: "attachment",
			params: map[string]string{"filename": `C:\dir\`}, codes: []Code{CodeMIMEParameterInvalidEscape}, refused: true},
		{name: "Windows directory before another parameter", field: cd, value: `attachment; filename="C:\dir\"; size=3`, base: "attachment",
			params: map[string]string{"filename": `C:\dir\`}, codes: []Code{CodeMIMEParameterUnclosedQuote, CodeMIMEParameterInvalidEscape}, refused: true},
		{name: "UNC path with a bracket", field: cd, value: `attachment; filename="\\srv\share\[1].txt"`, base: "attachment",
			params: map[string]string{"filename": `\srv\share[1].txt`}, codes: []Code{CodeMIMEParameterInvalidEscape}},
		{name: "a conformant escape", field: cd, value: `attachment; filename="a\"b\\c"`, base: "attachment",
			params: map[string]string{"filename": `a"b\c`}},
		{name: "unclosed boundary", value: `multipart/mixed; boundary="B`, base: "multipart/mixed",
			params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEParameterUnclosedQuote}, refused: true},

		// ROB-050-005: repeated parameters.
		{name: "conflicting charset takes the first", value: "text/plain; charset=utf-8; charset=us-ascii", base: "text/plain",
			params: map[string]string{"charset": "utf-8"}, codes: []Code{CodeMIMEParameterDuplicate}, refused: true},
		{name: "conflicting filename takes the first", field: cd, value: "attachment; filename=a.txt; filename=b.txt", base: "attachment",
			params: map[string]string{"filename": "a.txt"}, codes: []Code{CodeMIMEParameterDuplicate}, refused: true},
		{name: "extended form outranks the plain one", field: cd, value: "attachment; filename=a.txt; filename*=utf-8''b.txt", base: "attachment",
			params: map[string]string{"filename": "b.txt"}},
		{name: "conflicting boundary takes none", value: "multipart/mixed; boundary=A; boundary=B", base: "multipart/mixed",
			codes: []Code{CodeMIMEParameterDuplicate}, refused: true},
		{name: "conflicting extended forms take the first", field: cd, value: "attachment; filename*=utf-8''a.txt; filename*=utf-8''b.txt", base: "attachment",
			params: map[string]string{"filename": "a.txt"}, codes: []Code{CodeMIMEParameterDuplicate}, refused: true},
		{name: "identical repeats are harmless", value: "text/plain; charset=utf-8; CHARSET=utf-8", base: "text/plain",
			params: map[string]string{"charset": "utf-8"}},

		// ROB-050-006: RFC 2231 in any order and any charset.
		{name: "encoded series in order", field: cd, base: "attachment",
			value:  "attachment; filename*0*=utf-8''" + percent("한") + "; filename*1*=" + percent("글") + "; filename*2=.txt",
			params: map[string]string{"filename": "한글.txt"}},
		{name: "encoded series out of order", field: cd, base: "attachment",
			value:  "attachment; filename*2=.txt; filename*0*=utf-8''" + percent("한") + "; filename*1*=" + percent("글"),
			params: map[string]string{"filename": "한글.txt"}},
		{name: "extended value in EUC-KR", field: cd, base: "attachment",
			value:  "attachment; filename*=EUC-KR''" + percent(hangul) + ".hwp",
			params: map[string]string{"filename": "한글.hwp"}},
		{name: "a character cut across two segments", field: cd, base: "attachment",
			value:  "attachment; filename*0*=euc-kr'ko'" + percent(hangul[:1]) + "; filename*1*=" + percent(hangul[1:]) + "; filename*2=.hwp",
			params: map[string]string{"filename": "한글.hwp"}},
		{name: "prefix repeated on every segment", field: cd, base: "attachment",
			value:  "attachment; filename*0*=utf-8''" + percent("한") + "; filename*1*=utf-8''" + percent("글"),
			params: map[string]string{"filename": "한글"}},
		{name: "prefix on the first encoded segment", field: cd, base: "attachment",
			value:  `attachment; filename*0="abc "; filename*1*=utf-8''` + percent("한"),
			params: map[string]string{"filename": "abc 한"}, codes: []Code{CodeMIMEParameterUnknownCharset}, refused: true},
		{name: "names differing in case are one series", value: "text/plain; name*0=a; NAME*1=b", base: "text/plain",
			params: map[string]string{"name": "ab"}},

		// ROB-050-007: partial RFC 2231 values.
		{name: "gap in a series", field: cd, value: "attachment; filename*0=a; filename*2=c", base: "attachment",
			params: map[string]string{"filename": "ac"}, codes: []Code{CodeMIMEParameterSegmentMissing}, refused: true},
		{name: "series numbered from one", field: cd, base: "attachment",
			value:  "attachment; filename*1*=utf-8''" + percent("한") + "; filename*2=.txt",
			params: map[string]string{"filename": "한.txt"}, codes: []Code{CodeMIMEParameterSegmentMissing}, refused: true},
		{name: "bad percent escapes", field: cd, value: "attachment; filename*=utf-8''100%25%2Gx%", base: "attachment",
			params: map[string]string{"filename": "100%%2Gx%"}, codes: []Code{CodeMIMEParameterBadPercentEscape}, refused: true},
		{name: "charset with no decoder", field: cd, value: "attachment; filename*=x-unknown''%C7%D1.txt", base: "attachment",
			params: map[string]string{"filename": "\xc7\xd1.txt"}, codes: []Code{CodeMIMEParameterUnknownCharset}},
		{name: "ASCII bytes need no decoder", field: cd, value: "attachment; filename*=x-unknown''report.pdf", base: "attachment",
			params: map[string]string{"filename": "report.pdf"}},
		{name: "extended value with no charset prefix", field: cd, value: "attachment; filename*=abc.txt", base: "attachment",
			params: map[string]string{"filename": "abc.txt"}, codes: []Code{CodeMIMEParameterUnknownCharset}, refused: true},
		{name: "section repeated with another value", field: cd, value: "attachment; filename*0=a; filename*0=b; filename*1=c", base: "attachment",
			params: map[string]string{"filename": "ac"}, codes: []Code{CodeMIMEParameterSegmentDuplicate}, refused: true},
		{name: "bytes that are not in the charset declared", field: cd, value: "attachment; filename*=utf-8''" + percent(hangul) + ".txt", base: "attachment",
			params: map[string]string{"filename": hangul + ".txt"}, codes: []Code{CodeMIMEParameterUnknownCharset}},

		// ROB-050-008: encoded words and raw 8-bit bytes.
		{name: "encoded word in a filename", field: cd, value: `attachment; filename="=?UTF-8?B?` + b64("한글.pdf") + `?="`, base: "attachment",
			params: map[string]string{"filename": "한글.pdf"}, codes: []Code{CodeMIMEParameterRFC2047}},
		{name: "adjacent encoded words", field: cd, base: "attachment",
			value:  `attachment; filename="=?UTF-8?B?` + b64("한글") + `?= =?UTF-8?Q?=2Epdf?="`,
			params: map[string]string{"filename": "한글.pdf"}, codes: []Code{CodeMIMEParameterRFC2047}},
		{name: "encoded word in EUC-KR", field: cd, value: `attachment; filename="=?EUC-KR?B?` + b64(hangul+".hwp") + `?="`, base: "attachment",
			params: map[string]string{"filename": "한글.hwp"}, codes: []Code{CodeMIMEParameterRFC2047}},
		{name: "raw UTF-8", field: cd, value: `attachment; filename="한글.pdf"`, base: "attachment",
			params: map[string]string{"filename": "한글.pdf"}, codes: []Code{CodeMIMEParameterRawUTF8}},
		{name: "raw EUC-KR beside its charset", value: `text/plain; charset=euc-kr; name="` + hangul + `.txt"`, base: "text/plain",
			params: map[string]string{"charset": "euc-kr", "name": "한글.txt"}, codes: []Code{CodeMIMEParameterUnknownCharset}, guessed: true},
		{name: "raw EUC-KR with no charset", value: `application/octet-stream; name="` + hangul + `.hwp"`, base: "application/octet-stream",
			params: map[string]string{"name": hangul + ".hwp"}, codes: []Code{CodeMIMEParameterUnknownCharset}},
		{name: "raw bytes in a boundary are left alone", value: "multipart/mixed; boundary=\"\xff\xfe\"", base: "multipart/mixed",
			params: map[string]string{"boundary": "\xff\xfe"}},
	}
}

// parseField parses a message with the case's field and returns the parse's
// reading of it.
func parseField(t *testing.T, tc tolerantCase, profile Profile) (*ParseSession, string, map[string]string, error, error) {
	t.Helper()
	text := "Content-Type: " + tc.value + "\r\n\r\nbody"
	if tc.field != "" {
		text = "Content-Type: application/octet-stream\r\n" + tc.field + ": " + tc.value + "\r\n\r\nbody"
	}
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
	if e == nil {
		t.Fatalf("Parse() returned no entity: %v", err)
	}
	if tc.field != "" {
		base, params, fieldErr := e.EffectiveContentDisposition()
		return session, base, params, err, fieldErr
	}
	base, params, fieldErr := e.EffectiveContentType()
	return session, base, params, err, fieldErr
}

func wantParams(tc tolerantCase) map[string]string {
	if tc.params == nil {
		return map[string]string{}
	}
	return tc.params
}

func TestTolerantFields(t *testing.T) {
	withKoreanCharsets(t)
	for _, tc := range tolerantCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			// The session-less reading.
			var (
				base   string
				params map[string]string
				diags  []Diagnostic
			)
			if tc.field == "" {
				base, params, diags = ParseMediaTypeTolerant(tc.value)
			} else {
				base, params, diags = ParseContentDispositionTolerant(tc.value)
			}
			if base != tc.base || !reflect.DeepEqual(params, wantParams(tc)) {
				t.Errorf("tolerant reading = (%q, %q), want (%q, %q)", base, params, tc.base, wantParams(tc))
			}
			if got := codesOfDiags(diags); !reflect.DeepEqual(got, tc.codes) {
				t.Errorf("tolerant diagnostics = %v, want %v", got, tc.codes)
			}
			for _, d := range diags {
				field := tc.field
				if field == "" {
					field = "Content-Type"
				}
				if d.Field != field || d.Message == "" || d.Action == "" || d.Location != (Location{}) || d.Path != nil {
					t.Errorf("diagnostic %+v: want the field, a message, an action and no position", d)
				}
			}
			if hasRepairDiagnostic(diags) != tc.refused {
				t.Errorf("repair diagnostics among %v: %v, want %v", codesOfDiags(diags), !tc.refused, tc.refused)
			}

			// Parse reads it to the same result under the repairing profiles.
			for _, profile := range []Profile{ProfileTolerant, ProfileForensic} {
				session, pBase, pParams, err, fieldErr := parseField(t, tc, profile)
				if err != nil || fieldErr != nil || pBase != tc.base || !reflect.DeepEqual(pParams, wantParams(tc)) {
					t.Errorf("%v: Parse = (%q, %q, %v, %v), want (%q, %q)", profile, pBase, pParams, err, fieldErr, tc.base, wantParams(tc))
				}
				if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, tc.codes) {
					t.Errorf("%v: Diagnostics() = %v, want %v", profile, got, tc.codes)
				}
				for i, d := range session.Diagnostics() {
					if d.Message != diags[i].Message || d.Action != diags[i].Action || d.Severity != diags[i].Severity || !d.Location.HasOffset {
						t.Errorf("%v: diagnostic %+v differs from the session-less %+v, or has no position", profile, d, diags[i])
					}
				}
			}

			// Strict refuses a field that needed repair, and only that entity.
			session, sBase, sParams, _, fieldErr := parseField(t, tc, ProfileStrict)
			switch {
			case tc.refused:
				if !IsFatal(fieldErr) || sBase != "" || sParams != nil {
					t.Errorf("strict: (%q, %q, %v), want no interpretation and the entity's refusal", sBase, sParams, fieldErr)
				}
				for _, d := range session.Diagnostics() {
					if d.Severity != SeverityInvalidMetadata || !strings.HasPrefix(d.Action, "strict profile:") {
						t.Errorf("strict: diagnostic %v severity %v action %q", d.Code, d.Severity, d.Action)
					}
				}
				if session.Len() == 0 {
					t.Error("strict: a refusal recorded nothing")
				}
			case tc.guessed:
				if fieldErr != nil || sBase != tc.base || utf8.ValidString(sParams["name"]) {
					t.Errorf("strict: (%q, %q, %v), want the raw bytes kept: a strict profile does not guess a charset", sBase, sParams, fieldErr)
				}
			default:
				if fieldErr != nil || sBase != tc.base || !reflect.DeepEqual(sParams, wantParams(tc)) {
					t.Errorf("strict: (%q, %q, %v), want the tolerant reading: nothing needed repair", sBase, sParams, fieldErr)
				}
				if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, tc.codes) {
					t.Errorf("strict: Diagnostics() = %v, want %v", got, tc.codes)
				}
			}
		})
	}
}

// TestTolerantFields_duplicateMatrix pins the precedence of a repeated
// parameter (ROB-050-005): the RFC 2231 form before the plain one, the first
// occurrence within a form, a damaged repeat ignored — and for a boundary, no
// value at all unless every occurrence agrees. Every occurrence stays a
// candidate whatever was chosen.
func TestTolerantFields_duplicateMatrix(t *testing.T) {
	for _, tc := range []struct {
		params     string
		want       string // the value of p or boundary; "" for none
		duplicate  bool
		candidates int
	}{
		{"p=a; p=a", "a", false, 2},
		{"p=a; p=b", "a", true, 2},
		{"p=b; p=a", "b", true, 2},
		{"p=a; p=b; p=a", "a", true, 3},
		{`p=a; P="a"`, "a", false, 2},
		{"p=a; p*=utf-8''b", "b", false, 2},
		{"p*=utf-8''b; p=a", "b", false, 2},
		{"p=a; p*0=b; p*1=c", "bc", false, 2},
		{"p*=utf-8''a; p*=utf-8''b", "a", true, 2},
		{"p*=utf-8''a; p*0=b", "a", true, 2},
		{"p*0=b; p*=utf-8''a", "b", true, 2},
		{"p=a; p=", "a", false, 1},
		{"p=; p=a", "a", false, 1},
		{"p=a; p", "a", false, 1},
		{"boundary=a; boundary=a", "a", false, 2},
		{"boundary=a; boundary=b", "", true, 2},
		{"boundary=a; BOUNDARY=A", "", true, 2},
		{"boundary=a; boundary*=utf-8''a", "a", false, 2},
		{"boundary=a; boundary*=utf-8''b", "", true, 2},
		{"boundary=a; boundary=", "", true, 1},
		{"boundary; boundary=a", "", true, 1},
		{"boundary=a; x=1; boundary=b; boundary=a", "", true, 3},
	} {
		name := "p"
		if strings.Contains(strings.ToLower(tc.params), "boundary") {
			name = "boundary"
		}
		value := "multipart/mixed; " + tc.params
		_, params, diags := ParseMediaTypeTolerant(value)
		if got, ok := params[name]; got != tc.want || ok != (tc.want != "") {
			t.Errorf("%q: %s = %q (present %v), want %q", tc.params, name, got, ok, tc.want)
		}
		duplicate := false
		for _, d := range diags {
			duplicate = duplicate || d.Code == CodeMIMEParameterDuplicate
		}
		if duplicate != tc.duplicate {
			t.Errorf("%q: diagnostics %v, want mime.parameter_duplicate = %v", tc.params, codesOfDiags(diags), tc.duplicate)
		}
		n, chosen := 0, 0
		for _, c := range ParseParameterCandidates("Content-Type", value) {
			if c.Name == name {
				n++
				if c.Chosen {
					chosen++
				}
			}
		}
		if wantChosen := map[bool]int{true: 1, false: 0}[tc.want != ""]; n != tc.candidates || chosen != wantChosen {
			t.Errorf("%q: %d candidates of which %d chosen, want %d and %d", tc.params, n, chosen, tc.candidates, wantChosen)
		}
	}
}

// TestTolerantFields_severities pins which findings are observations and which
// are repairs.
func TestTolerantFields_severities(t *testing.T) {
	for _, tc := range []struct {
		value    string
		code     Code
		severity Severity
	}{
		{`attachment; filename="=?UTF-8?Q?a?="`, CodeMIMEParameterRFC2047, SeverityInfo},
		{`attachment; filename="한글"`, CodeMIMEParameterRawUTF8, SeverityInfo},
		{`attachment; filename="C:\a\b"`, CodeMIMEParameterInvalidEscape, SeverityInfo},
		{`attachment; filename="a "b" c"`, CodeMIMEParameterInvalidEscape, SeverityRecovered},
		{`attachment; filename*=x-none''%FF`, CodeMIMEParameterUnknownCharset, SeverityUnsupported},
		{`attachment; filename*=abc`, CodeMIMEParameterUnknownCharset, SeverityRecovered},
		{`attachment; filename=a b`, CodeMIMEParameterUnquotedValue, SeverityRecovered},
		{`attachment; filename="a`, CodeMIMEParameterUnclosedQuote, SeverityRecovered},
		{`attachment; filename=a; filename=b`, CodeMIMEParameterDuplicate, SeverityRecovered},
		{`attachment; filename*1=a`, CodeMIMEParameterSegmentMissing, SeverityRecovered},
		{`attachment; filename*=utf-8''%zz`, CodeMIMEParameterBadPercentEscape, SeverityRecovered},
		{`attachment filename=a`, CodeMIMEDispositionInvalid, SeverityRecovered},
	} {
		_, _, diags := ParseContentDispositionTolerant(tc.value)
		if len(diags) != 1 || diags[0].Code != tc.code || diags[0].Severity != tc.severity {
			t.Errorf("%q: diagnostics %v, want one %v of severity %v", tc.value, diags, tc.code, tc.severity)
		}
	}
}

// TestTolerantFields_conformantUnchanged pins that a field the whole-field
// parser reads is returned exactly as Header.ContentType returns it, with no
// diagnostic, by the session-less reading and by Parse under every profile.
func TestTolerantFields_conformantUnchanged(t *testing.T) {
	for _, value := range []string{
		"text/plain",
		"text/plain; charset=utf-8",
		`multipart/mixed; boundary="a;b"; x="q\"y"`,
		"Text/HTML; Charset=UTF-8; format=flowed",
		"text/plain; name*0=a; name*1=b",
		"text/plain; name*=utf-8''caf%C3%A9",
		"text/plain; name*0*=utf-8'en'caf; name*1*=%C3%A9; name*2=\".txt\"",
		"application/x; a*x=1; b=2",
		"multipart/mixed; boundary=B;",
		`application/pdf; name="a b.pdf"`,
	} {
		var h Header
		h.Set("Content-Type", value)
		wantType, wantParams, err := h.ContentType()
		if err != nil {
			t.Fatalf("%q is not conformant: %v", value, err)
		}
		mt, params, diags := ParseMediaTypeTolerant(value)
		if mt != wantType || !reflect.DeepEqual(params, wantParams) || diags != nil {
			t.Errorf("ParseMediaTypeTolerant(%q) = (%q, %q, %v), want (%q, %q) and no diagnostic", value, mt, params, codesOfDiags(diags), wantType, wantParams)
		}
		for _, profile := range profiles {
			session, e, err := parseWith(t, "Content-Type: "+value+"\r\n\r\nbody", ParseOptions{Profile: profile})
			mt, params, ctErr := e.EffectiveContentType()
			if err != nil || ctErr != nil || mt != wantType || !reflect.DeepEqual(params, wantParams) || session.Len() != 0 {
				t.Errorf("%v %q: (%q, %q, %v, %v) %v, want (%q, %q)", profile, value, mt, params, ctErr, err, codesOfDiags(session.Diagnostics()), wantType, wantParams)
			}
		}
	}

	// The absent fields.
	if mt, params, diags := ParseMediaTypeTolerant(""); mt != "text/plain" || params != nil || diags != nil {
		t.Errorf("ParseMediaTypeTolerant(\"\") = (%q, %v, %v)", mt, params, diags)
	}
	if disp, params, diags := ParseContentDispositionTolerant(" "); disp != "" || params != nil || diags != nil {
		t.Errorf("ParseContentDispositionTolerant(\" \") = (%q, %v, %v)", disp, params, diags)
	}
	session, e, _ := parseWith(t, "Content-Type: text/plain\r\n\r\nbody", ParseOptions{})
	if disp, params, err := e.EffectiveContentDisposition(); disp != "" || params != nil || err == nil || session.Len() != 0 {
		t.Errorf("absent disposition = (%q, %v, %v) %v, want the legacy empty value and error", disp, params, err, codesOfDiags(session.Diagnostics()))
	}
}

// TestTolerantFields_classification pins what the repaired metadata does to an
// entity: a healed text type decodes its body, a healed multipart frames its
// parts, and a healed disposition is the attachment mail.Reader classifies by.
func TestTolerantFields_classification(t *testing.T) {
	withKoreanCharsets(t)

	t.Run("a bare text type decodes by its charset", func(t *testing.T) {
		_, e, err := parseWith(t, "Content-Type: text; charset=euc-kr\r\n\r\n"+eucKR(t, "한글"), ParseOptions{})
		if body := readBodyOf(t, e); err != nil || body != "한글" {
			t.Errorf("body = %q, %v; want the EUC-KR text decoded", body, err)
		}
	})

	t.Run("a charset followed by junk still decodes the body", func(t *testing.T) {
		for _, value := range []string{
			"text/plain; charset=euc-kr format=flowed",
			`text/plain; charset="euc-kr`,
			`text/plain; charset=euc-kr"`,
			"text/plain; charset=EUC-KR (Korean)",
			"text/plain charset=euc-kr",
			"text/plain; charset=euc-kr; charset=utf-8",
			"text/plain; charset*=us-ascii'en'euc-kr",
		} {
			session, e, err := parseWith(t, "Content-Type: "+value+"\r\n\r\n"+eucKR(t, "한글"), ParseOptions{})
			if body := readBodyOf(t, e); err != nil || body != "한글" {
				t.Errorf("%q: body %q, %v; want the EUC-KR text decoded", value, body, err)
			}
			if value != "text/plain; charset*=us-ascii'en'euc-kr" && !hasRepairDiagnostic(session.Diagnostics()) {
				t.Errorf("%q: Diagnostics() = %v, want the repair reported", value, codesOfDiags(session.Diagnostics()))
			}
		}
	})

	t.Run("a bare multipart with a loose boundary frames its parts", func(t *testing.T) {
		text := "Content-Type: multipart boundary=--=_b 1\r\n\r\n----=_b 1\r\n\r\none\r\n----=_b 1\r\nContent-Type: /html\r\n\r\ntwo\r\n----=_b 1--\r\n"
		session, e, err := parseWith(t, text, ParseOptions{})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		mr := e.MultipartReader()
		if mr == nil {
			t.Fatal("MultipartReader() = nil, want the parts framed by the recovered boundary")
		}
		var bodies, types []string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("NextPart() = %v", err)
			}
			mt, _, _ := p.EffectiveContentType()
			types = append(types, mt)
			bodies = append(bodies, readBodyOf(t, p))
		}
		if !reflect.DeepEqual(bodies, []string{"one", "two"}) || !reflect.DeepEqual(types, []string{"text/plain", "text/html"}) {
			t.Errorf("parts = %q of types %q", bodies, types)
		}
		want := []Code{CodeMIMEContentTypeMissingSlash, CodeMIMEParameterUnquotedValue, CodeMIMEContentTypeMissingType}
		if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, want) {
			t.Errorf("Diagnostics() = %v, want %v", got, want)
		}
		if d := session.Diagnostics()[2]; !reflect.DeepEqual(d.Path, []int{1}) {
			t.Errorf("the child's diagnostic has path %v, want [1]", d.Path)
		}
	})

	t.Run("the raw header is never rewritten", func(t *testing.T) {
		const value = `attachment filename="C:\dir\한글.hwp`
		_, e, _ := parseWith(t, "Content-Disposition: "+value+"\r\n\r\nbody", ParseOptions{})
		if disp, params, err := e.EffectiveContentDisposition(); err != nil || disp != "attachment" || params["filename"] != `C:\dir\한글.hwp` {
			t.Errorf("EffectiveContentDisposition() = (%q, %q, %v)", disp, params, err)
		}
		checkRawHeader(t, e, "Content-Disposition: "+value+"\r\n")
		if _, _, err := e.Header.ContentDisposition(); err == nil {
			t.Error("Header.ContentDisposition() = nil error, want the legacy accessor unchanged")
		}
	})

	t.Run("raw bytes in a disposition are read in the Content-Type's charset", func(t *testing.T) {
		raw := eucKR(t, "한글") + ".txt"
		text := "Content-Type: text/plain; charset=ks_c_5601-1987\r\nContent-Disposition: attachment; filename=\"" + raw + "\"\r\n\r\nbody"
		session, e, _ := parseWith(t, text, ParseOptions{})
		if _, params, err := e.EffectiveContentDisposition(); err != nil || params["filename"] != "한글.txt" {
			t.Errorf("filename = %q, %v; want it decoded as EUC-KR", params["filename"], err)
		}
		d := session.Diagnostics()
		if len(d) != 1 || d[0].Code != CodeMIMEParameterUnknownCharset || d[0].Severity != SeverityRecovered || d[0].Field != "Content-Disposition" {
			t.Errorf("Diagnostics() = %+v", d)
		}
		// Strict makes no such guess, and refuses nothing either.
		_, e, _ = parseWith(t, text, ParseOptions{Profile: ProfileStrict})
		if _, params, err := e.EffectiveContentDisposition(); err != nil || params["filename"] != raw {
			t.Errorf("strict: filename = %q, %v; want the raw bytes", params["filename"], err)
		}
	})
}

// TestParameterCandidates pins the candidates API: every occurrence is kept, in
// field order, with its raw text, and at most one is chosen.
func TestParameterCandidates(t *testing.T) {
	withKoreanCharsets(t)
	hangul := eucKR(t, "한글")
	type cand struct {
		name, value, raw string
		form             ParameterForm
		chosen           bool
	}
	for _, tc := range []struct {
		name, field, value string
		want               []cand
	}{
		{"first of conflicting plain values", "Content-Type", "text/plain; charset=utf-8; Charset=euc-kr", []cand{
			{"charset", "utf-8", "charset=utf-8", ParameterPlain, true},
			{"charset", "euc-kr", "Charset=euc-kr", ParameterPlain, false},
		}},
		{"conflicting boundaries choose none", "content-type", "multipart/mixed; boundary=A; boundary=\"B\"", []cand{
			{"boundary", "A", "boundary=A", ParameterPlain, false},
			{"boundary", "B", `boundary="B"`, ParameterPlain, false},
		}},
		{"plain, extended and continued forms of one name", "Content-Disposition",
			"attachment; filename=fallback.txt; filename*1=b; filename*=utf-8''ext.txt; filename*0=a", []cand{
				{"filename", "fallback.txt", "filename=fallback.txt", ParameterPlain, false},
				{"filename", "ab", "filename*1=b; filename*0=a", ParameterContinued, true},
				{"filename", "ext.txt", "filename*=utf-8''ext.txt", ParameterExtended, false},
			}},
		{"a readable plain value outranks an undecodable extended one", "Content-Disposition",
			"attachment; filename*=x-none''" + percent(hangul) + "; filename=ok.txt", []cand{
				{"filename", hangul, "filename*=x-none''" + percent(hangul), ParameterExtended, false},
				{"filename", "ok.txt", "filename=ok.txt", ParameterPlain, true},
			}},
		{"a field with no parameters", "Content-Type", "text/plain", nil},
		{"another field", "Subject", "a=b", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(label string, got []ParameterCandidate) {
				t.Helper()
				var have []cand
				for _, c := range got {
					have = append(have, cand{c.Name, c.Value, c.Raw, c.Form, c.Chosen})
				}
				if !reflect.DeepEqual(have, tc.want) {
					t.Errorf("%s = %+v, want %+v", label, have, tc.want)
				}
			}
			check("ParseParameterCandidates", ParseParameterCandidates(tc.field, tc.value))

			session, e, _ := parseWith(t, tc.field+": "+tc.value+"\r\n\r\nbody", ParseOptions{})
			before := session.Len()
			check("Entity.ParameterCandidates", e.ParameterCandidates(tc.field))
			got := e.ParameterCandidates(tc.field)
			if len(got) > 0 {
				got[0].Value = "edited"
				check("Entity.ParameterCandidates after editing a result", e.ParameterCandidates(tc.field))
			}
			if session.Len() != before {
				t.Errorf("asking for candidates recorded %d diagnostics", session.Len()-before)
			}

			var h Header
			h.Set(tc.field, tc.value)
			literal := &Entity{Header: h}
			check("a struct-literal entity", literal.ParameterCandidates(tc.field))
		})
	}

	// The extended candidate says what it declared, and whether it was decoded.
	c := ParseParameterCandidates("Content-Disposition", "attachment; filename*=EUC-KR'ko'"+percent(hangul))
	if len(c) != 1 || c[0].Charset != "EUC-KR" || c[0].Language != "ko" || c[0].Undecoded || c[0].Value != "한글" {
		t.Errorf("extended candidate = %+v", c)
	}
	c = ParseParameterCandidates("Content-Disposition", "attachment; filename*=x-none'ko'"+percent(hangul))
	if len(c) != 1 || !c[0].Undecoded || c[0].Value != hangul || !c[0].Chosen {
		t.Errorf("undecodable candidate = %+v, want its bytes kept and chosen for want of another", c)
	}
}

// hostileFields are values chosen to reach every branch of the tolerant
// reading with input it was not written for.
var hostileFields = []string{
	"", " ", ";", ";;;", "=", "==", "/", "//", "a/", "/a", "a/b/c", "\"", "\"\"", "\\", "a=\\", "a=\"\\", "a=\"\\\"",
	"*", "**", "*=", "a*", "a*=", "a**=x", "a*0", "a*0=", "a*0*=", "a*0*='", "a*0*=''", "a*0*='''", "a*=%", "a*=%%", "a*=%4", "a*='%'%",
	"a*999999999=x", "a*9999999999=x", "a*00=x; a*0=y", "a*-1=x", "a*1*=utf-8''x; a*0*=''y",
	"text/plain; a*0=x; a*0=x; a*0=y", "text/plain; =", "text/plain; ==", "text/plain; =?=", "text/plain; a==?=", "text/plain; a==?utf-8?b?!!!?=",
	"text/plain; a=\"=?utf-8?q?", "text/plain; a=\"=?x?b?AAAA?=\"", "text/plain; \x00=\x00", "text/plain; a=\x00\x01\x7f",
	"text/plain; a=\xff\xfe\xfd", "text/plain; charset=\xff; a=\xff", "text/plain; charset=utf-16; a=\xff",
	"\xff/\xfe; \xfd=\xfc", "multipart; boundary", "multipart/; boundary=", "multipart/mixed; boundary=\"", "multipart/mixed; boundary=\"\"; boundary=\"\"",
	"multipart/mixed; boundary=a; boundary*=utf-8''a; boundary*0=a", "text/plain;\t;\t; ;", "text/plain; a=b; A=B; a=\"b\"; a*=utf-8''b; a*0=b",
	"a b c d e f", "text/plain garbage more=garbage; x", "(comment) text/plain", "text/plain; (comment)", "text/plain; a=(b)", "text/plain; a=\"b\"c\"d",
	"text/plain; a=\"b\" \"c\"", "text/plain; a=b\\;c", "text/plain; a=\"\r\n\"", "text/plain; a=\"b\r\n c\"", "'text/plain'", "<text/plain>",
	strings.Repeat("\"", 257), strings.Repeat("\\", 257), strings.Repeat(";a*0=x", 300), strings.Repeat(";a=\"", 300), strings.Repeat("%", 300) + "*=" + strings.Repeat("%", 300),
	"a*=" + strings.Repeat("'", 300), strings.Repeat("=?", 300), "x; a=" + strings.Repeat("=?utf-8?b?", 100), strings.Repeat("a/", 300), strings.Repeat("*0", 300) + "=x",
}

// TestTolerantFields_neverPanic runs a small hostile set through every entry
// point and profile: nothing panics, every reading returns a base and a
// non-nil parameter map, and no parameter is invented.
func TestTolerantFields_neverPanic(t *testing.T) {
	withKoreanCharsets(t)
	for _, value := range hostileFields {
		for _, field := range []string{"Content-Type", "Content-Disposition"} {
			var (
				base   string
				params map[string]string
			)
			if field == "Content-Type" {
				base, params, _ = ParseMediaTypeTolerant(value)
			} else {
				base, params, _ = ParseContentDispositionTolerant(value)
			}
			blank := strings.TrimSpace(value) == ""
			if !blank && (base == "" || params == nil) {
				t.Errorf("%s %q: tolerant reading = (%q, %v), want a base and a parameter map", field, value, base, params)
			}
			if field == "Content-Type" && !blank && strings.Count(base, "/") != 1 {
				t.Errorf("%s %q: base %q is not a type and a subtype", field, value, base)
			}
			lower := strings.ToLower(value)
			for k := range params {
				if !strings.Contains(lower, k) {
					t.Errorf("%s %q: parameter %q is not named by the field", field, value, k)
				}
			}
			chosen := map[string]int{}
			for _, c := range ParseParameterCandidates(field, value) {
				if c.Chosen {
					chosen[c.Name]++
					if params[c.Name] != c.Value {
						t.Errorf("%s %q: chosen candidate %+v is not the parameter's value %q", field, value, c, params[c.Name])
					}
				}
			}
			for name, n := range chosen {
				if n != 1 {
					t.Errorf("%s %q: %d chosen candidates for %s", field, value, n, name)
				}
			}

			var h Header
			h.Set(field, value)
			h.FilenameCandidates()

			if strings.ContainsAny(value, "\r\n") {
				// Not a value one header line can hold.
				continue
			}
			for _, profile := range profiles {
				text := field + ": " + value + "\r\n\r\nbody"
				_, e, _ := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
				if e == nil {
					continue
				}
				e.EffectiveContentType()
				e.EffectiveContentDisposition()
				e.ParameterCandidates(field)
				e.FilenameCandidates()
				if mr := e.MultipartReader(); mr != nil {
					mr.NextPart()
				} else {
					io.Copy(io.Discard, e.Body)
				}
			}
		}
	}
}

// TestTolerantFields_budgets pins that the parameter budgets bound the
// tolerant reading as they bound every other: only the occurrences within a
// budget are read, the candidates are those occurrences, a continued parameter
// a budget cut is dropped whole, and what a field records stays short however
// many of its parameters are defective.
func TestTolerantFields_budgets(t *testing.T) {
	var loose strings.Builder
	loose.WriteString("text/plain")
	for i := range 1000 {
		fmt.Fprintf(&loose, "; p%d=a b", i)
	}

	t.Run("count budget", func(t *testing.T) {
		session, e, err := parseWith(t, "Content-Type: "+loose.String()+"\r\n\r\nbody", ParseOptions{Limits: paramsOnly(8, 0)})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		_, params, _ := e.EffectiveContentType()
		if len(params) != 8 || params["p7"] != "a b" || params["p8"] != "" {
			t.Errorf("%d parameters, want the eight within the budget", len(params))
		}
		want := []Code{CodeLimitParameterCount, CodeMIMEParameterUnquotedValue}
		if got := codesOfDiags(obDropOverlong(session.Diagnostics())); !reflect.DeepEqual(got, want) {
			t.Errorf("Diagnostics() = %v, want %v", got, want)
		}
		if d := obDropOverlong(session.Diagnostics())[1]; !strings.Contains(d.Action, "within the parameter budget") {
			t.Errorf("Action %q does not say only the retained occurrences were read", d.Action)
		}
		if c := e.ParameterCandidates("Content-Type"); len(c) != 8 {
			t.Errorf("%d candidates, want the eight within the budget", len(c))
		}
	})

	t.Run("segment budget", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("attachment; size=1")
		for i := range 500 {
			fmt.Fprintf(&b, "; filename*%d=x", i)
		}
		text := "Content-Type: text/plain\r\nContent-Disposition: " + b.String() + "\r\n\r\nbody"
		session, e, _ := parseWith(t, text, ParseOptions{Limits: paramsOnly(0, 4)})
		_, params, err := e.EffectiveContentDisposition()
		if err != nil || !reflect.DeepEqual(params, map[string]string{"size": "1"}) {
			t.Errorf("params = %q, %v; want the series the budget cut dropped whole", params, err)
		}
		if got := codesOfDiags(obDropOverlong(session.Diagnostics())); !reflect.DeepEqual(got, []Code{CodeLimitParameterSegments}) {
			t.Errorf("Diagnostics() = %v, want only the budget's", got)
		}
		c := e.ParameterCandidates("Content-Disposition")
		if len(c) != 2 || c[1].Chosen || strings.Count(c[1].Raw, ";") != 3 {
			t.Errorf("candidates = %+v, want size and the four retained segments, not chosen", c)
		}
	})

	t.Run("what a field records stays short", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("text")
		for i := range 200 {
			fmt.Fprintf(&b, `; %s%d=a b; q%d="c; r%d*=x; r%d*=y`, strings.Repeat("n", 100), i, i, i, i)
		}
		_, params, diags := ParseMediaTypeTolerant(b.String())
		if len(params) == 0 || len(diags) > len(parameterNoteOrder)+3 {
			t.Fatalf("%d parameters and %d diagnostics", len(params), len(diags))
		}
		for _, d := range diags {
			if len(d.Message) > 2048 || len(d.Action) > 4096 {
				t.Errorf("%v: message of %d bytes and action of %d", d.Code, len(d.Message), len(d.Action))
			}
		}
	})

	t.Run("a megabyte of defects under the default budgets", func(t *testing.T) {
		start := time.Now()
		value := "text" + strings.Repeat("; a=b c", 150000)
		_, params, diags := ParseMediaTypeTolerant(value)
		if len(params) != 1 || len(diags) == 0 || diags[0].Code != CodeLimitParameterCount {
			t.Errorf("%d parameters, diagnostics %v; want the budget to cut the field", len(params), codesOfDiags(diags))
		}
		unclosed := `attachment; filename="` + strings.Repeat(`\"x;`, 250000)
		_, params, diags = ParseContentDispositionTolerant(unclosed)
		if len(params["filename"]) != 750000 || len(diags) != 1 || diags[0].Code != CodeMIMEParameterUnclosedQuote {
			t.Errorf("filename of %d bytes, diagnostics %v", len(params["filename"]), codesOfDiags(diags))
		}
		series := "attachment" + strings.Repeat("; filename*1*=%41", 60000)
		if _, params, diags = ParseContentDispositionTolerant(series); len(params) != 0 || len(diags) != 1 || diags[0].Code != CodeLimitParameterCount {
			t.Errorf("params %q, diagnostics %v", params, codesOfDiags(diags))
		}
		// The encoded-word decoder is not linear in its input, and nothing
		// bounds a value a caller holds, so one longer than a header field a
		// parse admits by default keeps its words as they are. Within that
		// size they are decoded.
		words := strings.Repeat("=?a?b?x", 150000) + "?="
		_, params, diags = ParseContentDispositionTolerant(`attachment; filename="` + words + `"`)
		if params["filename"] != words || len(diags) != 0 {
			t.Errorf("filename of %d bytes, diagnostics %v; want the megabyte of encoded words left undecoded", len(params["filename"]), codesOfDiags(diags))
		}
		var h Header
		h.Set("Content-Disposition", `attachment; filename="`+words+`"`)
		if names := h.FilenameCandidates(); len(names) != 1 || names[0].Filename != words {
			t.Errorf("FilenameCandidates() of a megabyte of encoded words = %d candidates", len(names))
		}
		small := `attachment; filename="` + strings.Repeat("=?UTF-8?Q?a?= ", 1000) + `"`
		if _, params, _ = ParseContentDispositionTolerant(small); params["filename"] != strings.Repeat("a", 1000)+" " {
			t.Errorf("a kilobyte-scale run of encoded words decoded to %d bytes", len(params["filename"]))
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("the megabyte-scale fields took %v", d)
		}
	})

	t.Run("segments out of order within a disabled budget", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("attachment")
		for i := 1999; i >= 0; i-- {
			fmt.Fprintf(&b, "; filename*%d=%d", i, i%10)
		}
		text := "Content-Disposition: " + b.String() + "\r\n\r\nbody"
		session, e, _ := parseWith(t, text, ParseOptions{Limits: allDisabled()})
		_, params, _ := e.EffectiveContentDisposition()
		if got := params["filename"]; len(got) != 2000 || !strings.HasPrefix(got, "0123456789") || len(obDropOverlong(session.Diagnostics())) != 0 {
			t.Errorf("filename of %d bytes starting %.12q, %v", len(got), got, codesOfDiags(session.Diagnostics()))
		}
	})
}

// TestTolerantFields_stalledCharset pins that a CharsetReader stalling on an
// RFC 2231 value is bounded like one stalling on an encoded word: the value
// keeps its bytes, the field's other values are read, and one diagnostic says
// so.
func TestTolerantFields_stalledCharset(t *testing.T) {
	built := withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
	text := "Content-Type: text/plain\r\nContent-Disposition: attachment; filename*=x-meta''%41%42; size=3\r\n\r\nbody"
	session, e, err := parseWith(t, text, ParseOptions{})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	disp, params, dErr := e.EffectiveContentDisposition()
	if dErr != nil || disp != "attachment" || params["size"] != "3" || params["filename"] != "AB" {
		t.Errorf("EffectiveContentDisposition() = (%q, %q, %v), want the ASCII bytes kept beside size", disp, params, dErr)
	}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeHeaderDecoderNoProgress}) {
		t.Errorf("Diagnostics() = %v, want one header.decoder_no_progress", got)
	}
	if len(*built) != 1 || (*built)[0].zeroCalls != maxConsecutiveEmptyReads {
		t.Errorf("%d hook readers, want one stopped after %d empty reads", len(*built), maxConsecutiveEmptyReads)
	}
}
