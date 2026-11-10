package message

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit/textproto"
)

// ROB-050-001 (issue #22): a valid base media type or disposition is kept, with
// every independent well-formed parameter, when other parameter items of the
// same field are malformed. See metadata_repair.go.

// issue22 is the reproduction from issue #22.
const issue22 = "Content-Type: multipart/mixed; boundary=B; broken\r\n\r\n--B\r\n\r\nkept\r\n--B--\r\n"

// repairBody is a body that a multipart/mixed with boundary B frames into one
// part, kept; any other type reads it as opaque text.
const repairBody = "--B\r\n\r\nkept\r\n--B--\r\n"

var profiles = []Profile{ProfileTolerant, ProfileForensic, ProfileStrict}

type repairCase struct {
	name  string
	value string
	// mt and params are the repaired interpretation. defaulted marks a base
	// that is a default (ROB-050-002) rather than what the field names, which
	// the Action reports as read rather than kept.
	mt        string
	params    map[string]string
	defaulted bool
	codes     []Code
}

var contentTypeRepairs = []repairCase{
	{name: "issue 22: broken item after the boundary", value: "multipart/mixed; boundary=B; broken",
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "broken item before the boundary", value: "multipart/mixed; broken; boundary=B",
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "mixed case and whitespace keep the boundary's case", value: " Multipart/MIXED ;  Boundary = BoUnD ;broken ",
		mt: "multipart/mixed", params: map[string]string{"boundary": "BoUnD"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "quoted semicolon is not a separator", value: `multipart/mixed; boundary="a;b"; broken`,
		mt: "multipart/mixed", params: map[string]string{"boundary": "a;b"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "escaped quote does not end the value", value: `multipart/mixed; boundary="a\";b"; broken`,
		mt: "multipart/mixed", params: map[string]string{"boundary": `a";b`}, codes: []Code{CodeMIMEContentTypeInvalid}},
	// ROB-050-004: an unterminated quoted string is read to the end of the
	// field instead of being dropped. What it swallowed stays swallowed, so a
	// boundary behind it is still not found.
	{name: "unterminated quote keeps what precedes it", value: `multipart/mixed; boundary=B; name="open; x=1`,
		mt: "multipart/mixed", params: map[string]string{"boundary": "B", "name": "open; x=1"}, codes: []Code{CodeMIMEParameterUnclosedQuote}},
	{name: "unterminated quote hides a later boundary", value: `multipart/mixed; name="open; boundary=B`,
		mt: "multipart/mixed", params: map[string]string{"name": "open; boundary=B"}, codes: []Code{CodeMIMEParameterUnclosedQuote}},
	{name: "conflicting boundaries are both dropped", value: "multipart/mixed; boundary=B; boundary=C",
		mt: "multipart/mixed", params: map[string]string{}, codes: []Code{CodeMIMEParameterDuplicate}},
	{name: "boundaries differing in case conflict", value: "multipart/mixed; boundary=B; BOUNDARY=b",
		mt: "multipart/mixed", params: map[string]string{}, codes: []Code{CodeMIMEParameterDuplicate}},
	// mime accepts agreeing duplicates in a conformant field; the repair keeps
	// them on the same terms, without a duplicate diagnostic.
	{name: "identical boundaries around a broken item keep the one value", value: "multipart/mixed; boundary=B; broken; Boundary=B",
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "damaged second boundary conflicts", value: `multipart/mixed; boundary=B; boundary="C`,
		mt: "multipart/mixed", params: map[string]string{}, codes: []Code{CodeMIMEParameterUnclosedQuote, CodeMIMEParameterDuplicate}},
	{name: "unreadable second boundary conflicts", value: "multipart/mixed; boundary=B; boundary=",
		mt: "multipart/mixed", params: map[string]string{}, codes: []Code{CodeMIMEContentTypeInvalid, CodeMIMEParameterDuplicate}},
	// ROB-050-005: a parameter that does not frame the body takes its first
	// value rather than none.
	{name: "malformed and conflicting together", value: "text/plain; charset=utf-8; junk; charset=us-ascii; format=flowed",
		mt: "text/plain", params: map[string]string{"charset": "utf-8", "format": "flowed"}, codes: []Code{CodeMIMEContentTypeInvalid, CodeMIMEParameterDuplicate}},
	{name: "missing value", value: "text/plain; charset=; format=flowed",
		mt: "text/plain", params: map[string]string{"format": "flowed"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "missing name", value: "text/plain; =x; format=flowed",
		mt: "text/plain", params: map[string]string{"format": "flowed"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	// Stray empty items are defects mime.ParseMediaType rejects; on their own
	// they are what the repair has to explain (review of ROB-050-001). Only
	// one trailing semicolon is conformant; see TestParse_repairHappyPathUnchanged.
	{name: "stray empty items beside a broken item", value: "text/plain;; format=flowed; broken;",
		mt: "text/plain", params: map[string]string{"format": "flowed"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "empty item before the boundary", value: "multipart/mixed;; boundary=B",
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "blank item before the boundary", value: "multipart/mixed; ; boundary=B",
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "two trailing semicolons", value: "multipart/mixed; boundary=B;;",
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "blank item then a trailing semicolon", value: "multipart/mixed; boundary=B; ;",
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "RFC 2231 extended value is kept", value: "text/plain; name*=utf-8''caf%C3%A9; broken",
		mt: "text/plain", params: map[string]string{"name": "café"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	// ROB-050-006: a continued value is assembled in a damaged field as in a
	// conformant one, and the RFC 2231 form of a parameter outranks its plain
	// form.
	{name: "RFC 2231 continuation is assembled", value: "text/plain; name*0=a; name*1=b; broken; format=flowed",
		mt: "text/plain", params: map[string]string{"name": "ab", "format": "flowed"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	{name: "continuation outranks a whole parameter of one name", value: "text/plain; name=x; name*0=y; broken",
		mt: "text/plain", params: map[string]string{"name": "y"}, codes: []Code{CodeMIMEContentTypeInvalid}},
	// ROB-050-002: a base that is not valid on its own is read by conservative
	// defaults. Before, these four kept the raw value and the parse error.
	{name: "missing slash", value: "multipart; boundary=B; broken", defaulted: true,
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeMissingSlash, CodeMIMEContentTypeInvalid}},
	{name: "missing subtype", value: "multipart/; boundary=B; broken", defaulted: true,
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeMissingSubtype, CodeMIMEContentTypeInvalid}},
	{name: "missing type", value: "/mixed; boundary=B; broken", defaulted: true,
		mt: "multipart/mixed", params: map[string]string{"boundary": "B"}, codes: []Code{CodeMIMEContentTypeMissingType, CodeMIMEContentTypeInvalid}},
	// The quote opens a string that runs to the end of the field, so the type
	// before it is all that is read, and the multipart is unframed.
	{name: "quote in the base", value: `multipart/mixed"; boundary=B; broken`,
		mt: "multipart/mixed", params: map[string]string{}, codes: []Code{CodeMIMEContentTypeInvalid}},
}

func parseWith(t *testing.T, text string, opts ParseOptions) (*ParseSession, *Entity, error) {
	t.Helper()
	session, e, err := Parse(strings.NewReader(text), &opts)
	if e == nil {
		t.Fatalf("Parse() returned no entity (err = %v)", err)
	}
	return session, e, err
}

func TestParse_contentTypeRepair(t *testing.T) {
	for _, tc := range contentTypeRepairs {
		for _, profile := range profiles {
			t.Run(tc.name+"/"+profile.String()+"/root", func(t *testing.T) {
				text := "Content-Type: " + tc.value + "\r\n\r\n" + repairBody
				session, e, err := parseWith(t, text, ParseOptions{Profile: profile})
				checkRepair(t, tc, profile, session, e, err, nil)
				checkRawHeader(t, e, "Content-Type: "+tc.value+"\r\n")
			})
			t.Run(tc.name+"/"+profile.String()+"/child", func(t *testing.T) {
				text := "Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
					"--OUT\r\nContent-Type: text/plain\r\n\r\nbefore\r\n" +
					"--OUT\r\nContent-Type: " + tc.value + "\r\n\r\n" + repairBody + "\r\n" +
					"--OUT\r\nContent-Type: text/plain\r\n\r\nafter\r\n" +
					"--OUT--\r\n"
				session, root, err := parseWith(t, text, ParseOptions{Profile: profile})
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				mr := root.MultipartReader()
				before, err := mr.NextPart()
				if err != nil || readBodyOf(t, before) != "before" {
					t.Fatalf("first sibling = (%v, %v)", before, err)
				}
				child, err := mr.NextPart()
				if child == nil {
					t.Fatalf("NextPart() returned no child (err = %v), want the usable one", err)
				}
				checkRepair(t, tc, profile, session, child, err, []int{1})
				checkRawHeader(t, child, "Content-Type: "+tc.value+"\r\n")
				// Whatever happened to the child, the parent's iteration goes on
				// to the next framed sibling, and the first one stays valid.
				after, err := mr.NextPart()
				if err != nil || readBodyOf(t, after) != "after" {
					t.Errorf("following sibling = (%v, %v), want %q", after, err, "after")
				}
				if _, err := mr.NextPart(); err != io.EOF {
					t.Errorf("NextPart() at the end = %v, want io.EOF", err)
				}
				if session.fatalErr() != nil {
					t.Errorf("the parse was stopped: %v", session.fatalErr())
				}
			})
		}
	}
}

// checkRepair checks one entity that carried the case's field.
func checkRepair(t *testing.T, tc repairCase, profile Profile, session *ParseSession, e *Entity, err error, path []int) {
	t.Helper()
	diags := session.Diagnostics()
	mt, params, ctErr := e.EffectiveContentType()

	if got := codesOfDiags(diags); !reflect.DeepEqual(got, tc.codes) {
		t.Fatalf("Diagnostics() = %v, want %v", got, tc.codes)
	}
	for _, d := range diags {
		if d.Field != "Content-Type" || !reflect.DeepEqual(d.Path, path) || !d.Location.HasOffset {
			t.Errorf("diagnostic %v: field %q path %v location %v, want Content-Type at %v with a position", d.Code, d.Field, d.Path, d.Location, path)
		}
	}
	if e.ParseStop() != nil {
		t.Errorf("ParseStop() = %v, want nil: a repair or a strict refusal is not a stop", e.ParseStop())
	}

	if profile == ProfileStrict {
		if !IsFatal(err) || err != ctErr {
			t.Fatalf("strict: err = %v, EffectiveContentType err = %v, want the same entity-scoped FatalError", err, ctErr)
		}
		if mt != "" || params != nil {
			t.Errorf("strict: EffectiveContentType() = (%q, %v), want no interpretation", mt, params)
		}
		fd, ok := ctErr.(FatalError).Diagnostic()
		if !ok || fd.Code != tc.codes[0] || fd.Severity != SeverityFatal || !reflect.DeepEqual(fd.Path, path) {
			t.Errorf("strict: FatalError diagnostic = %+v, %v; want %v", fd, ok, tc.codes[0])
		}
		for _, d := range diags {
			if d.Severity != SeverityInvalidMetadata || !strings.HasPrefix(d.Action, "strict profile:") {
				t.Errorf("strict: diagnostic %v severity %v action %q", d.Code, d.Severity, d.Action)
			}
		}
		if e.MultipartReader() != nil {
			t.Error("strict: MultipartReader() != nil, want no part iteration")
		}
		if got := readBodyOf(t, e); got != strings.TrimSuffix(repairBody, "\r\n") && got != repairBody {
			t.Errorf("strict: body = %q, want the bytes as they arrived", got)
		}
		return
	}

	// Tolerant and Forensic: the same deterministic repair.
	if err != nil {
		t.Fatalf("err = %v, want none: the field was repaired", err)
	}
	if ctErr != nil || mt != tc.mt || !reflect.DeepEqual(params, tc.params) {
		t.Errorf("EffectiveContentType() = (%q, %v, %v), want (%q, %v, nil)", mt, params, ctErr, tc.mt, tc.params)
	}
	// A repaired multipart that kept no usable boundary is unframed: its
	// diagnostics say the body was not interpreted, and do not claim a repair.
	unframed := strings.HasPrefix(tc.mt, "multipart/") && tc.params["boundary"] == ""
	wantSeverity := SeverityRecovered
	if unframed {
		wantSeverity = SeverityInvalidMetadata
	}
	wantBase := "kept the media type " + fmt.Sprintf("%q", tc.mt)
	if tc.defaulted {
		wantBase = "read the media type as " + fmt.Sprintf("%q", tc.mt)
	}
	for _, d := range diags {
		if d.Severity != wantSeverity || !strings.Contains(d.Action, wantBase) ||
			!strings.Contains(d.Action, "left the raw field unchanged") ||
			strings.Contains(d.Action, "opaque") != unframed {
			t.Errorf("diagnostic %v severity %v action %q", d.Code, d.Severity, d.Action)
		}
	}
	// The standalone accessor keeps its legacy, unrepaired answer.
	if _, _, hErr := e.Header.ContentType(); hErr == nil {
		t.Error("Header.ContentType() = nil error, want the legacy parse error of the raw field")
	}

	mr := e.MultipartReader()
	switch {
	case !strings.HasPrefix(tc.mt, "multipart/"):
		if mr != nil {
			t.Error("MultipartReader() != nil for a non-multipart type")
		}
	case tc.params["boundary"] == "B":
		p, err := mr.NextPart()
		if err != nil || readBodyOf(t, p) != "kept" {
			t.Errorf("repaired multipart: NextPart() = (%v, %v), want the kept part", p, err)
		}
	case !unframed:
		// A boundary survived, but the shared body is not framed by it.
		if mr == nil {
			t.Error("repaired multipart with a boundary: MultipartReader() = nil")
		}
	default:
		// No boundary survived: nothing is chosen or invented, so nothing is
		// framed, and the body is one opaque part holding the input's bytes.
		if mr != nil {
			t.Error("unframed multipart: MultipartReader() != nil, want the opaque body")
		}
		if got := readBodyOf(t, e); got != repairBody && got != strings.TrimSuffix(repairBody, "\r\n") {
			t.Errorf("unframed multipart: body = %q, want the raw bytes", got)
		}
	}
	// Asking again records nothing more.
	before := session.Len()
	e.EffectiveContentType()
	e.EffectiveContentType()
	if session.Len() != before {
		t.Errorf("EffectiveContentType() recorded %d more diagnostics", session.Len()-before)
	}
}

func checkRawHeader(t *testing.T, e *Entity, want string) {
	t.Helper()
	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, e.Header.Header); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSuffix(b.String(), "\r\n"); got != want {
		t.Errorf("header written back as %q, want the raw bytes %q", got, want)
	}
}

// TestParse_repairHappyPathUnchanged pins that a conformant field parses as it
// always has, under every profile, with no diagnostic.
func TestParse_repairHappyPathUnchanged(t *testing.T) {
	for _, value := range []string{
		"text/plain",
		"text/plain; charset=utf-8",
		`multipart/mixed; boundary="a;b"; x="q\"y"`,
		"multipart/mixed; boundary=B; Boundary=B",
		"Text/HTML; Charset=UTF-8; format=flowed",
		"text/plain; name*0=a; name*1=b",
		"text/plain; name*=utf-8''caf%C3%A9",
		// One trailing semicolon is conformant to mime and stays untouched.
		"multipart/mixed; boundary=B;",
		"multipart/mixed; boundary=B; ",
		"text/plain;",
	} {
		wantType, wantParams, _ := mime.ParseMediaType(value)
		for _, profile := range profiles {
			session, e, err := parseWith(t, "Content-Type: "+value+"\r\n\r\nbody", ParseOptions{Profile: profile})
			mt, params, ctErr := e.EffectiveContentType()
			if err != nil || ctErr != nil || mt != wantType || !reflect.DeepEqual(params, wantParams) || session.Diagnostics() != nil {
				t.Errorf("%s %q: (%q, %v, %v, %v) diags %v, want (%q, %v)", profile, value, mt, params, ctErr, err, codesOfDiags(session.Diagnostics()), wantType, wantParams)
			}
		}
	}
}

// TestParse_dispositionRepair pins the same repair for Content-Disposition,
// and that the charset decision follows it: a repaired attachment is still an
// attachment, so SkipTextAttachmentDecoding leaves its charset unconverted.
// x-made-up has no decoder, so whether the charset stage ran shows as the
// unknown-charset warning.
func TestParse_dispositionRepair(t *testing.T) {
	const header = "Content-Type: text/plain; charset=x-made-up\r\n" +
		`Content-Disposition: Attachment; filename="a;b.txt"; broken; size=3` + "\r\n"
	for _, skip := range []bool{true, false} {
		for _, profile := range profiles {
			t.Run(fmt.Sprintf("%s/skip=%v", profile, skip), func(t *testing.T) {
				session, e, err := parseWith(t, header+"\r\nraw", ParseOptions{
					Profile:     profile,
					ReadOptions: ReadOptions{SkipTextAttachmentDecoding: skip},
				})
				disp, params, dErr := e.EffectiveContentDisposition()
				diags := session.Diagnostics()
				body := readBodyOf(t, e)
				if body != "raw" {
					t.Errorf("body = %q, want the bytes as they arrived (x-made-up has no decoder)", body)
				}
				if profile == ProfileStrict {
					if !IsFatal(dErr) || disp != "" || params != nil {
						t.Errorf("strict: EffectiveContentDisposition() = (%q, %v, %v), want no interpretation", disp, params, dErr)
					}
					if skip && err != dErr {
						t.Errorf("strict, skip: Parse err = %v, want the disposition refusal: the charset stage depends on it", err)
					}
					if !skip && !IsUnknownCharset(err) {
						t.Errorf("strict, no skip: Parse err = %v, want only the charset warning: nothing read the disposition", err)
					}
					return
				}
				if dErr != nil || disp != "attachment" || !reflect.DeepEqual(params, map[string]string{"filename": "a;b.txt", "size": "3"}) {
					t.Errorf("EffectiveContentDisposition() = (%q, %v, %v)", disp, params, dErr)
				}
				if skip && err != nil {
					t.Errorf("skip: err = %v, want none: the repaired attachment's charset is skipped", err)
				}
				if !skip && !IsUnknownCharset(err) {
					t.Errorf("no skip: err = %v, want the charset warning", err)
				}
				want := []Code{CodeMIMEDispositionInvalid}
				if !skip {
					want = []Code{CodeCharsetUnknown, CodeMIMEDispositionInvalid}
				}
				if got := codesOfDiags(diags); !reflect.DeepEqual(got, want) {
					t.Errorf("Diagnostics() = %v, want %v", got, want)
				}
				if d := diags[len(diags)-1]; d.Field != "Content-Disposition" || !strings.Contains(d.Action, `kept the disposition "attachment"`) {
					t.Errorf("disposition diagnostic = %+v", d)
				}
				e.EffectiveContentDisposition()
				if session.Len() != len(diags) {
					t.Error("asking again recorded another diagnostic")
				}
			})
		}
	}
	// A disposition with a slash is not a valid disposition base: its first
	// token is the disposition and the rest is reported (ROB-050-002). Before,
	// the raw value and the parse error were returned with no diagnostic.
	session, e, _ := parseWith(t, "Content-Disposition: attachment/x; broken\r\n\r\nraw", ParseOptions{})
	disp, params, err := e.EffectiveContentDisposition()
	if err != nil || disp != "attachment" || len(params) != 0 {
		t.Errorf("invalid disposition base: (%q, %v, %v), want the attachment with no parameters", disp, params, err)
	}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeMIMEDispositionInvalid}) {
		t.Errorf("invalid disposition base: Diagnostics() = %v, want one mime.disposition_invalid", got)
	}
}

// TestParse_repairUnderBudgets pins that the repair reads only what the
// parameter budgets retained, and never the whole field as a second attempt.
func TestParse_repairUnderBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  string
		limits Limits
		params map[string]string
		codes  []Code
	}{
		{"cut before the damage needs no repair", "multipart/mixed; boundary=B; broken", paramsOnly(1, 0),
			map[string]string{"boundary": "B"}, []Code{CodeLimitParameterCount}},
		{"the boundary past the budget is not recovered", "multipart/mixed; broken; boundary=B", paramsOnly(1, 0),
			map[string]string{}, []Code{CodeLimitParameterCount, CodeMIMEContentTypeInvalid}},
		{"a parameter past the budget is not read back", "text/plain; broken; charset=utf-8", paramsOnly(1, 0),
			map[string]string{}, []Code{CodeLimitParameterCount, CodeMIMEContentTypeInvalid}},
		{"a cut series is dropped and needs no repair", "text/plain; name*0=a; name*1=b; broken", paramsOnly(0, 1),
			map[string]string{}, []Code{CodeLimitParameterSegments}},
		{"both budgets disabled still repairs", "multipart/mixed; boundary=B; broken", allDisabled(),
			map[string]string{"boundary": "B"}, []Code{CodeMIMEContentTypeInvalid}},
		{"only the segment budget enabled still repairs", "multipart/mixed; boundary=B; broken", paramsOnly(-1, 4),
			map[string]string{"boundary": "B"}, []Code{CodeMIMEContentTypeInvalid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, e, err := parseWith(t, "Content-Type: "+tc.value+"\r\n\r\nbody", ParseOptions{Limits: tc.limits})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if _, params, _ := e.EffectiveContentType(); !reflect.DeepEqual(params, tc.params) {
				t.Errorf("params = %v, want %v", params, tc.params)
			}
			diags := session.Diagnostics()
			if got := codesOfDiags(diags); !reflect.DeepEqual(got, tc.codes) {
				t.Errorf("Diagnostics() = %v, want %v", got, tc.codes)
			}
			if len(diags) == 2 && !strings.Contains(diags[1].Action, "within the parameter budget") {
				t.Errorf("repair after a budget cut: Action %q does not say only the retained occurrences were read", diags[1].Action)
			}
		})
	}
}

// TestParse_repairSinkStop pins that a sink stop wins over the repair and over
// a strict refusal, stays sticky, and that no decoder or later diagnostic
// follows it.
func TestParse_repairSinkStop(t *testing.T) {
	sentinel := errors.New("stop")
	for _, profile := range profiles {
		t.Run(profile.String(), func(t *testing.T) {
			sink := DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return sentinel })
			text := "Content-Type: text/plain; charset=utf-8; junk; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\ndHdv"
			session, e, err := parseWith(t, text, ParseOptions{Profile: profile, DiagnosticSink: sink})
			if !IsFatal(err) || !errors.Is(err, sentinel) || err != e.ParseStop() {
				t.Fatalf("Parse() err = %v, want the published sink stop", err)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeMIMEContentTypeInvalid}) {
				t.Errorf("Diagnostics() = %v, want only the first, the one the sink stopped on", got)
			}
			if got := readBodyOf(t, e); got != "dHdv" {
				t.Errorf("body = %q, want the undecoded bytes: no decoder after the stop", got)
			}
			if e.ParseStop() != err {
				t.Error("ParseStop() is not sticky")
			}
		})
	}
}

// TestParse_repairChunkingIsInvisible pins that how the input arrives does not
// change the result: one byte at a time, and split at every offset.
func TestParse_repairChunkingIsInvisible(t *testing.T) {
	type outcome struct {
		mt     string
		params map[string]string
		child  string
		codes  []Code
	}
	read := func(r io.Reader) outcome {
		session, e, err := Parse(r, nil)
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		mt, params, _ := e.EffectiveContentType()
		p, err := e.MultipartReader().NextPart()
		if err != nil {
			t.Fatalf("NextPart() = %v", err)
		}
		return outcome{mt, params, readBodyOf(t, p), codesOfDiags(session.Diagnostics())}
	}
	want := read(strings.NewReader(issue22))
	if want.child != "kept" || want.params["boundary"] != "B" {
		t.Fatalf("whole input = %+v", want)
	}
	if got := read(iotest.OneByteReader(strings.NewReader(issue22))); !reflect.DeepEqual(got, want) {
		t.Errorf("one byte at a time = %+v, want %+v", got, want)
	}
	for k := 0; k <= len(issue22); k++ {
		r := io.MultiReader(strings.NewReader(issue22[:k]), strings.NewReader(issue22[k:]))
		if got := read(r); !reflect.DeepEqual(got, want) {
			t.Errorf("split at %d = %+v, want %+v", k, got, want)
		}
	}
}

// TestParse_repairNested pins the repair two levels down, with the diagnostic
// at that path and the siblings around it intact.
func TestParse_repairNested(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\nContent-Type: multipart/alternative; boundary=IN\r\n\r\n" +
		"--IN\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--IN\r\nContent-Type: multipart/related; broken; boundary=B\r\n\r\n" + repairBody + "\r\n" +
		"--IN--\r\n" +
		"--OUT\r\nContent-Type: text/plain\r\n\r\nafter\r\n" +
		"--OUT--\r\n"
	session, root, err := parseWith(t, text, ParseOptions{})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	var bodies []string
	if err := root.Walk(func(path []int, e *Entity, err error) error {
		if err != nil {
			return err
		}
		if e.MultipartReader() == nil {
			bodies = append(bodies, fmt.Sprintf("%v %s", path, readBodyOf(t, e)))
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk() = %v", err)
	}
	if want := []string{"[0 0] plain", "[0 1 0] kept", "[1] after"}; !reflect.DeepEqual(bodies, want) {
		t.Errorf("leaves = %q, want %q", bodies, want)
	}
	diags := session.Diagnostics()
	if len(diags) != 1 || diags[0].Code != CodeMIMEContentTypeInvalid || !reflect.DeepEqual(diags[0].Path, []int{0, 1}) {
		t.Errorf("Diagnostics() = %+v, want one at [0 1]", diags)
	}
}

// TestRead_noRepair pins the legacy entry points: no session, no repair, and
// the behavior issue #22 reported, unchanged.
func TestRead_noRepair(t *testing.T) {
	e, err := Read(strings.NewReader(issue22))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	mt, params, ctErr := e.EffectiveContentType()
	if mt != "multipart/mixed; boundary=B; broken" || params != nil || ctErr == nil {
		t.Errorf("EffectiveContentType() = (%q, %v, %v), want the legacy raw value and error", mt, params, ctErr)
	}
	if _, err := e.MultipartReader().NextPart(); err == nil {
		t.Error("legacy NextPart() succeeded, want the long-standing empty-boundary error")
	}
	var h Header
	h.Set("Content-Type", "multipart/mixed; boundary=B; broken")
	built, _ := New(h, strings.NewReader(repairBody))
	if mt, _, err := built.EffectiveContentType(); mt != "multipart/mixed; boundary=B; broken" || err == nil {
		t.Errorf("New(): EffectiveContentType() = (%q, %v), want the legacy result", mt, err)
	}
	if mt, _, err := h.ContentType(); mt != "multipart/mixed; boundary=B; broken" || err == nil {
		t.Errorf("Header.ContentType() = (%q, %v), want the legacy result", mt, err)
	}
}

// TestParse_repairReturnsCopies pins that the repaired parameters handed out
// are copies: editing one disturbs neither a later result nor the framing.
func TestParse_repairReturnsCopies(t *testing.T) {
	_, e, _ := parseWith(t, issue22, ParseOptions{})
	_, params, _ := e.EffectiveContentType()
	params["boundary"] = "X"
	if _, again, _ := e.EffectiveContentType(); again["boundary"] != "B" {
		t.Errorf("EffectiveContentType() after editing a result = %v", again)
	}
	if p, err := e.MultipartReader().NextPart(); err != nil || readBodyOf(t, p) != "kept" {
		t.Errorf("NextPart() after editing a result = (%v, %v)", p, err)
	}
}

// TestParse_repairWorkIsLinear checks that repairing a field with many damaged
// items costs work proportional to the field, not to its square.
func TestParse_repairWorkIsLinear(t *testing.T) {
	cost := func(n int) uint64 {
		value := "multipart/mixed; boundary=B" + strings.Repeat("; broken", n)
		text := "Content-Type: " + value + "\r\n\r\n"
		opts := &ParseOptions{Limits: allDisabled()}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, e, _ := Parse(strings.NewReader(text), opts)
		runtime.ReadMemStats(&after)
		if _, params, _ := e.EffectiveContentType(); params["boundary"] != "B" {
			t.Fatalf("n=%d: boundary lost", n)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := cost(2000), cost(8000)
	if ratio := float64(large) / float64(small); ratio > 4*3 {
		t.Errorf("4x the damaged items cost %.1fx the bytes (%d vs %d), want about 4x", ratio, large, small)
	}
}

// FuzzParseContentTypeRepair runs arbitrary Content-Type values through every
// profile: nothing panics, a value the whole-field parser reads in full — it
// accepts it, with a type and a subtype, and the value holds nothing only the
// item-by-item reading reads — is interpreted exactly as it always was, every
// other value is read to a valid base and only parameters the field names, and
// Strict refuses exactly where Tolerant repairs. It is a focused target for
// ROB-050-001 to ROB-050-008, not the general fuzzing of ROB-130.
func FuzzParseContentTypeRepair(f *testing.F) {
	for _, tc := range contentTypeRepairs {
		f.Add(tc.value)
	}
	f.Add(`text/plain; a="\`)
	f.Add(";;;")
	f.Add("multipart/mixed;; boundary=B")
	f.Add("multipart/mixed; boundary=B; ;")
	f.Add("multipart/mixed; boundary=B;")
	f.Add("teXt/00000; ChArset=0") // conformant, with a charset no decoder knows
	f.Fuzz(func(t *testing.T, value string) {
		if strings.ContainsAny(value, "\r\n") {
			return
		}
		text := "Content-Type: " + value + "\r\n\r\nbody"
		th, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(text)))
		if err != nil || th.Get("Content-Type") == "" {
			return
		}
		raw := th.Get("Content-Type")
		wantType, wantParams, wholeErr := parseHeaderWithParams(raw)

		tSession, tolerant, _ := Parse(strings.NewReader(text), &ParseOptions{Limits: allDisabled()})
		mt, params, ctErr := tolerant.EffectiveContentType()
		if ctErr != nil {
			t.Fatalf("tolerant %q was not read: %v", raw, ctErr)
		}
		whole := wholeErr == nil && validWholeBase("Content-Type", wantType) && wholeFieldSuffices(raw)
		if whole {
			// An unknown charset is still diagnosed; nothing about the field is.
			if mt != wantType || !reflect.DeepEqual(params, wantParams) || hasMIMEDiagnostic(tSession.Diagnostics()) {
				t.Fatalf("conformant %q changed: (%q, %v)", raw, mt, params)
			}
		} else {
			if base, ok := validBase("Content-Type", mt); !ok || base != mt {
				t.Fatalf("%q was read to an invalid base %q", raw, mt)
			}
			lower := strings.ToLower(raw)
			for k := range params {
				if !strings.Contains(lower, k) {
					t.Fatalf("%q was read to a parameter %q it does not name", raw, k)
				}
			}
		}

		// What Tolerant repaired is what the reading itself says needed repair,
		// or what the whole-field parser refused when it was asked.
		reading := readTolerant(tolerantInput{
			field: "Content-Type", raw: raw, scan: scanParameters(raw, LimitDisabled, LimitDisabled), dec: &metadataDecoder{},
		})
		repaired := reading.needsRepair() || (wholeFieldSuffices(raw) && wholeErr != nil)
		if got := hasRepairDiagnostic(tSession.Diagnostics()); got != repaired {
			t.Fatalf("%q: repair diagnostics %v, want %v", raw, codesOfDiags(tSession.Diagnostics()), repaired)
		}

		_, strict, strictErr := Parse(strings.NewReader(text), &ParseOptions{Limits: allDisabled(), Profile: ProfileStrict})
		if strict == nil {
			t.Fatalf("strict Parse(%q) returned no entity", raw)
		}
		if IsFatal(strictErr) != repaired {
			t.Fatalf("%q: strict refusal %v, tolerant repair %v", raw, strictErr, repaired)
		}
	})
}

// hasRepairDiagnostic reports whether diags hold a MIME metadata diagnostic of
// a repair rather than of an observation: one a strict profile refuses over.
func hasRepairDiagnostic(diags []Diagnostic) bool {
	for _, d := range diags {
		if !strings.HasPrefix(string(d.Code), "mime.") {
			continue
		}
		switch d.Code {
		case CodeMIMEParameterRFC2047, CodeMIMEParameterRawUTF8:
			continue
		case CodeMIMEParameterUnknownCharset, CodeMIMEParameterInvalidEscape:
			// These two report repairs and observations alike; the message of
			// a repair is its own.
			if !strings.Contains(d.Message, "does not declare its charset") && !strings.Contains(d.Message, "escapes nothing") {
				continue
			}
		}
		return true
	}
	return false
}

func hasMIMEDiagnostic(diags []Diagnostic) bool {
	for _, d := range diags {
		if strings.HasPrefix(string(d.Code), "mime.") {
			return true
		}
	}
	return false
}

// TestParse_strictRefusalEndsAWalk pins what Walk does with a strict refusal:
// it returns it, as any non-warning error, after the parts before it; the
// parse itself is not stopped.
func TestParse_strictRefusalEndsAWalk(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\nContent-Type: text/plain\r\n\r\nbefore\r\n" +
		"--OUT\r\nContent-Type: text/plain; broken\r\n\r\nrefused\r\n" +
		"--OUT\r\nContent-Type: text/plain\r\n\r\nafter\r\n" +
		"--OUT--\r\n"
	_, root, _ := parseWith(t, text, ParseOptions{Profile: ProfileStrict})
	var seen []string
	err := root.Walk(func(path []int, e *Entity, err error) error {
		if e.MultipartReader() == nil {
			seen = append(seen, readBodyOf(t, e))
		}
		return nil
	})
	if !IsFatal(err) || root.ParseStop() != nil {
		t.Fatalf("Walk() = %v, ParseStop() = %v; want the entity-scoped refusal and no stop", err, root.ParseStop())
	}
	if !reflect.DeepEqual(seen, []string{"before"}) {
		t.Errorf("parts walked = %q, want only the one before the refusal", seen)
	}
}
