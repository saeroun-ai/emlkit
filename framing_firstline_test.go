package message

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Malformed prefixes (ROB-020-001): a header block whose first lines begin with
// white space. Every source here is a finite string or a call-capped source.
//
// The reference fixtures are the minimized inputs of the compatibility study
// for this item, kept here as base64 exactly as they were recorded (16 and 9
// inputs, each at most 2 KiB). They were run, once each, through Eclipse Angus
// Mail 2.0.5 with Jakarta Mail API 2.1.5 (activation 2.1.4/2.0.3), Apache James
// Mime4j 0.8.15 (default, permissive and strict) and Python 3.14.10's email
// package (default, compat32 and strict policies). The inputs and the 175
// observations are archived in docs/qa/ROB-020-001/references/, whose
// manifest.json pins the versions, jar and snapshot hashes and execution
// limits; the tests do not run those engines. emlkit's behavior below is its own
// conservative choice under every Compatibility value. Intentional
// differences, recorded rather than emulated (no parity with any engine is
// claimed, and #17 owns compatibility policies):
//
//   - Angus trims a first indented line with other content and reads it as a
//     field (" Subject: shifted" becomes Subject; an indented Content-Type
//     frames a multipart), keeps a colonless one as a raw header, and drops a
//     whitespace-only first line; it fails the whole parse with an
//     IndexOutOfBoundsException when a whitespace-only first line is followed
//     by an indented field or line (blank-then-indented-field,
//     two-blanks-then-indented, blank-then-raw), and does not recognize a byte
//     order mark. emlkit keeps every such line as the prefix and reads none of
//     them as a field.
//   - Mime4j default and permissive keep an indented colon line as a field
//     with its semantic name (Subject; an indented Content-Type frames a
//     multipart), and silently drop colonless or whitespace-only runs; strict
//     fails the parse. emlkit retains every byte and interprets none.
//   - Python default and compat32 leave the lines out of the header and record
//     a FirstHeaderLineIsContinuationDefect for each physical line; strict
//     raises. It does not recognize a byte order mark (utf8-bom,
//     bom-blank-indented: no fields, everything body), and it reads a bare
//     '\r' as a line ending, so first-internal-cr is to it a prefix line, then
//     a missing separator, with no fields and the rest body, where emlkit joins
//     the line the '\r' was content of to the prefix and keeps X. On the other
//     fixtures that begin with white space emlkit's fields match the default
//     policy's; it records one diagnostic per prefix and keeps the bytes, and
//     its Strict refusal is the entity's alone.
//   - MimeKit was not run; CompatibilityMimeKit takes the conservative choice.

// A firstLineCase is one root fixture and what parsing it yields under
// Tolerant: the prefix, the fields as readLineEndingTree reports them, the
// body, and the diagnostics after the prefix's own (which comes first, after a
// byte order mark's).
type firstLineCase struct {
	name   string
	b64    string // a reference fixture; text is used when empty
	text   string
	bom    bool
	prefix string
	start  int64
	lines  int
	fields []string
	body   string
	extra  []lineEndingFacts
}

func (c firstLineCase) input() string {
	if c.b64 == "" {
		return c.text
	}
	b, err := base64.StdEncoding.DecodeString(c.b64)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// tolerant is what Tolerant and Forensic record.
func (c firstLineCase) tolerant() []lineEndingFacts {
	var out []lineEndingFacts
	if c.bom {
		out = append(out, bomNote(SeverityRecovered, 0, 3))
	}
	if c.prefix != "" {
		out = append(out, lineEndingFacts{CodeHeaderFirstLineContinuation, SeverityRecovered, nil, c.start, c.start + int64(len(c.prefix))})
	}
	return append(out, c.extra...)
}

// refusing reports whether ProfileStrict refuses an entity for code.
func refusing(code Code) bool {
	switch code {
	case CodeHeaderFirstLineContinuation, CodeHeaderMissingBodySeparator, CodeHeaderBareCR, CodeHeaderTruncatedLine, CodeInputBOM:
		return true
	}
	return false
}

// strictFacts is what Strict records where Tolerant records facts.
func strictFacts(facts []lineEndingFacts) []lineEndingFacts {
	var out []lineEndingFacts
	for _, f := range facts {
		if refusing(f.code) {
			f.severity = SeverityInvalidMetadata
		}
		out = append(out, f)
	}
	return out
}

func truncatedNote(start, end int64) lineEndingFacts {
	return lineEndingFacts{CodeHeaderTruncatedLine, SeverityIncomplete, nil, start, end}
}

func firstLineCases() []firstLineCase {
	subject := []string{`[] Subject="intact"`}
	kept := []string{`[] X="kept"`}
	longRun := " orphan\r\n" + strings.Repeat(" continuation\r\n", 8)
	return []firstLineCase{
		// fixtures.tsv
		{name: "valid", b64: "U3ViamVjdDogaW50YWN0DQoNCmJvZHk=", fields: subject, body: "body"},
		{name: "space-field", b64: "IFN1YmplY3Q6IHNoaWZ0ZWQNClgtS2VlcDogeWVzDQoNCmJvZHk=",
			prefix: " Subject: shifted\r\n", lines: 1, fields: []string{`[] X-Keep="yes"`}, body: "body"},
		{name: "tab-field", b64: "CVN1YmplY3Q6IHNoaWZ0ZWQNClgtS2VlcDogeWVzDQoNCmJvZHk=",
			prefix: "\tSubject: shifted\r\n", lines: 1, fields: []string{`[] X-Keep="yes"`}, body: "body"},
		{name: "raw-orphan", b64: "IG9ycGhhbiB0ZXh0DQpTdWJqZWN0OiBpbnRhY3QNCg0KYm9keQ==",
			prefix: " orphan text\r\n", lines: 1, fields: subject, body: "body"},
		{name: "multiple", b64: "IG9ycGhhbiB0ZXh0DQoJbW9yZSB0ZXh0DQpTdWJqZWN0OiBpbnRhY3QNCg0KYm9keQ==",
			prefix: " orphan text\r\n\tmore text\r\n", lines: 2, fields: subject, body: "body"},
		{name: "space-only", b64: "IAkNClN1YmplY3Q6IGludGFjdA0KDQpib2R5",
			prefix: " \t\r\n", lines: 1, fields: subject, body: "body"},
		{name: "orphan-eof", b64: "IG9ycGhhbiB0ZXh0", prefix: " orphan text", lines: 1, extra: []lineEndingFacts{truncatedNote(0, 12)}},
		{name: "field-eof", b64: "IFN1YmplY3Q6IHNoaWZ0ZWQ=", prefix: " Subject: shifted", lines: 1, extra: []lineEndingFacts{truncatedNote(0, 17)}},
		{name: "lf", b64: "IG9ycGhhbgpTdWJqZWN0OiBpbnRhY3QKCmJvZHk=", prefix: " orphan\n", lines: 1, fields: subject, body: "body",
			extra: []lineEndingFacts{{CodeHeaderBareLF, SeverityInfo, nil, 7, 8}}},
		{name: "cr", b64: "IG9ycGhhbg1TdWJqZWN0OiBpbnRhY3QNDWJvZHk=", prefix: " orphan\r", lines: 1, fields: subject, body: "body",
			extra: []lineEndingFacts{{CodeHeaderBareCR, SeverityRecovered, nil, 7, 8}}},
		{name: "indented-multipart", b64: "IENvbnRlbnQtVHlwZTogbXVsdGlwYXJ0L21peGVkOyBib3VuZGFyeT1CDQoNCi0tQg0KDQprZXB0DQotLUItLQ0K",
			prefix: " Content-Type: multipart/mixed; boundary=B\r\n", lines: 1, body: "--B\r\n\r\nkept\r\n--B--\r\n"},
		{name: "invalid-name", b64: "IFgAOiB2YWx1ZQ0KU3ViamVjdDogaW50YWN0DQoNCmJvZHk=",
			prefix: " X\x00: value\r\n", lines: 1, fields: subject, body: "body"},
		{name: "utf8-bom", b64: "77u/IG9ycGhhbg0KU3ViamVjdDogaW50YWN0DQoNCmJvZHk=", bom: true,
			prefix: " orphan\r\n", start: 3, lines: 1, fields: subject, body: "body"},
		{name: "first-only-crlf", b64: "IG9ycGhhbg0K", prefix: " orphan\r\n", lines: 1},
		{name: "blank-first", b64: "DQogU3ViamVjdDogYm9keQ0K", body: " Subject: body\r\n"},

		// fixtures-extra.tsv
		{name: "blank-then-indented-field", b64: "IA0KIFN1YmplY3Q6IHNoaWZ0ZWQNClg6IGtlcHQNCg0KYm9keQ==",
			prefix: " \r\n Subject: shifted\r\n", lines: 2, fields: kept, body: "body"},
		{name: "two-blanks-then-indented", b64: "IAkNCgkNCiBTdWJqZWN0OiBzaGlmdGVkDQpYOiBrZXB0DQoNCmJvZHk=",
			prefix: " \t\r\n\t\r\n Subject: shifted\r\n", lines: 3, fields: kept, body: "body"},
		{name: "indented-second-field", b64: "IFN1YmplY3Q6IGZpcnN0DQogVG86IHNlY29uZA0KWDoga2VwdA0KDQpib2R5",
			prefix: " Subject: first\r\n To: second\r\n", lines: 2, fields: kept, body: "body"},
		{name: "empty-prefix-name", b64: "IDogdmFsdWUNClg6IGtlcHQNCg0KYm9keQ==",
			prefix: " : value\r\n", lines: 1, fields: kept, body: "body"},
		{name: "spaces-eof", b64: "IAkg", prefix: " \t ", lines: 1, extra: []lineEndingFacts{truncatedNote(0, 3)}},
		{name: "blank-then-raw", b64: "CQ0KIG9ycGhhbg0KWDoga2VwdA0KDQpib2R5",
			prefix: "\t\r\n orphan\r\n", lines: 2, fields: kept, body: "body"},
		{name: "bom-blank-indented", b64: "77u/IA0KIFN1YmplY3Q6IHNoaWZ0ZWQNClg6IGtlcHQNCg0KYm9keQ==", bom: true,
			prefix: " \r\n Subject: shifted\r\n", start: 3, lines: 2, fields: kept, body: "body"},
		{name: "long-run", b64: "IG9ycGhhbg0KIGNvbnRpbnVhdGlvbg0KIGNvbnRpbnVhdGlvbg0KIGNvbnRpbnVhdGlvbg0KIGNvbnRpbnVhdGlvbg0KIGNvbnRpbnVhdGlvbg0KIGNvbnRpbnVhdGlvbg0KIGNvbnRpbnVhdGlvbg0KIGNvbnRpbnVhdGlvbg0KWDoga2VwdA0KDQpib2R5",
			prefix: longRun, lines: 9, fields: kept, body: "body"},
		{name: "first-internal-cr", b64: "IG9ycGhhbg1ub3QgYSBmaWVsZApYOiBrZXB0Cgpib2R5",
			prefix: " orphan\rnot a field\n", lines: 1, fields: kept, body: "body",
			extra: []lineEndingFacts{{CodeHeaderBareLF, SeverityInfo, nil, 19, 20}}},

		// Local cases.
		{name: "an indented transfer encoding declares nothing", text: " Content-Transfer-Encoding: base64\r\n\r\nQUJD",
			prefix: " Content-Transfer-Encoding: base64\r\n", lines: 1, body: "QUJD"},
		{name: "white space alone, then the empty line", text: " \t\r\n\r\nbody", prefix: " \t\r\n", lines: 1, body: "body"},
		{name: "a whitespace line after a field is a fold", text: "A: 1\r\n \r\nB: 2\r\n\r\nbody",
			fields: []string{`[] A="1"`, `[] B="2"`}, body: "body"},
	}
}

// checkFirstLineRefusal pins a Strict refusal: a FatalError carrying a
// SeverityFatal copy of the first refusing diagnostic recorded, the
// Content-Type not interpreted and no part iteration.
func checkFirstLineRefusal(t *testing.T, label string, e *Entity, err error, recorded []lineEndingFacts) {
	t.Helper()
	var first *lineEndingFacts
	for i := range recorded {
		if recorded[i].severity == SeverityInvalidMetadata {
			first = &recorded[i]
			break
		}
	}
	if first == nil {
		t.Fatalf("%s: nothing recorded refuses", label)
	}
	var fe FatalError
	if !IsFatal(err) || !errors.As(err, &fe) {
		t.Fatalf("%s: err = %v, want the entity's FatalError", label, err)
	}
	if d, ok := fe.Diagnostic(); !ok || d.Code != first.code || d.Severity != SeverityFatal ||
		d.Location.Offset != first.start || d.Location.EndOffset != first.end || !reflect.DeepEqual(d.Path, first.path) {
		t.Errorf("%s: fatal diagnostic = %+v, want %+v", label, d, *first)
	}
	if mt, params, terr := e.EffectiveContentType(); mt != "" || params != nil || terr != err {
		t.Errorf("%s: EffectiveContentType() = (%q, %v, %v), want nothing interpreted and the refusal", label, mt, params, terr)
	}
	if e.MultipartReader() != nil {
		t.Errorf("%s: MultipartReader() != nil for a refused entity", label)
	}
	if e.ParseStop() != nil {
		t.Errorf("%s: ParseStop() = %v, want none", label, e.ParseStop())
	}
}

// checkPrefix pins an entity's malformed prefix, absolute positions included.
func checkPrefix(t *testing.T, label string, e *Entity, prefix string, start int64, lines int) {
	t.Helper()
	p := e.Header.MalformedPrefix()
	s, end := p.Location()
	if got := string(p.Bytes()); got != prefix || p.Present() != (prefix != "") || (prefix != "" && (s != start || end != start+int64(len(prefix)) || p.Lines() != lines)) {
		t.Errorf("%s: prefix (%q, [%d, %d), %d lines), want (%q at %d, %d lines)", label, got, s, end, p.Lines(), prefix, start, lines)
	}
}

// TestParse_firstLineReferences pins the root of every fixture under every
// profile, compatibility and delivery: the prefix, the diagnostics with their
// positions, the refusal or its absence, the fields and the body.
func TestParse_firstLineReferences(t *testing.T) {
	for _, tc := range firstLineCases() {
		text := tc.input()
		if tc.prefix != "" && text[tc.start:tc.start+int64(len(tc.prefix))] != tc.prefix {
			t.Fatalf("%s: the case's prefix is not its input's", tc.name)
		}
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
			for _, compat := range framingCompatibilities {
				for name, src := range lineEndingSources(text) {
					label := fmt.Sprintf("%s/%v/%v/%s", tc.name, profile, compat, name)
					session, e, err := Parse(src(), &ParseOptions{Profile: profile, Compatibility: compat})
					if e == nil {
						t.Fatalf("%s: Parse() = (nil, %v), want the root", label, err)
					}
					want := tc.tolerant()
					if profile == ProfileStrict {
						want = strictFacts(want)
					}
					if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
						t.Errorf("%s: diagnostics = %+v, want %+v", label, got, want)
					}
					if e.ParseStop() != nil {
						t.Errorf("%s: ParseStop() = %v, want none", label, e.ParseStop())
					}
					checkPrefix(t, label, e, tc.prefix, tc.start, tc.lines)
					if got, wantAt := e.parse.origin.bodyOffset, int64(len(text)-len(tc.body)); got != wantAt {
						t.Errorf("%s: body offset %d, want %d", label, got, wantAt)
					}
					if profile == ProfileStrict && len(want) > 0 && want[0].severity == SeverityInvalidMetadata {
						checkFirstLineRefusal(t, label, e, err, want)
					} else if err != nil {
						t.Errorf("%s: Parse() err = %v, want nil", label, err)
					}
					tr := readLineEndingTree(e)
					if !reflect.DeepEqual(tr.fields, tc.fields) && !(len(tr.fields) == 0 && len(tc.fields) == 0) {
						t.Errorf("%s: fields = %q, want %q", label, tr.fields, tc.fields)
					}
					if wantBodies := []string{fmt.Sprintf("[] %q", tc.body)}; !reflect.DeepEqual(tr.bodies, wantBodies) || tr.errs != nil {
						t.Errorf("%s: bodies = %q (%q), want %q", label, tr.bodies, tr.errs, wantBodies)
					}
				}
			}
		}
	}
}

// TestRead_firstLineReferences pins that Read and ReadWithOptions frame every
// fixture as Parse does, with no error: they have no session and no profile.
func TestRead_firstLineReferences(t *testing.T) {
	for _, tc := range firstLineCases() {
		for api, read := range map[string]func(io.Reader) (*Entity, error){
			"Read": Read,
			"ReadWithOptions": func(r io.Reader) (*Entity, error) {
				return ReadWithOptions(r, &ReadOptions{SkipTextAttachmentDecoding: true})
			},
		} {
			label := fmt.Sprintf("%s/%s", tc.name, api)
			e, err := read(strings.NewReader(tc.input()))
			if err != nil || e == nil {
				t.Fatalf("%s: (%v, %v), want the entity and no error", label, e, err)
			}
			checkPrefix(t, label, e, tc.prefix, tc.start, tc.lines)
			tr := readLineEndingTree(e)
			if !reflect.DeepEqual(tr.fields, tc.fields) && !(len(tr.fields) == 0 && len(tc.fields) == 0) {
				t.Errorf("%s: fields = %q, want %q", label, tr.fields, tc.fields)
			}
			if want := []string{fmt.Sprintf("[] %q", tc.body)}; !reflect.DeepEqual(tr.bodies, want) || tr.errs != nil {
				t.Errorf("%s: bodies = %q (%q), want %q", label, tr.bodies, tr.errs, want)
			}
		}
	}
}

// TestParse_firstLineIndentedMetadata pins that what a prefix looks like is not
// interpreted: an indented multipart Content-Type frames no parts and an
// indented transfer encoding decodes nothing, under every profile but Strict,
// which refuses the entity.
func TestParse_firstLineIndentedMetadata(t *testing.T) {
	for _, text := range []string{
		" Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nkept\r\n--B--\r\n",
		" Content-Transfer-Encoding: base64\r\n Content-Type: text/plain; charset=koi8-r\r\n\r\nQUJD",
	} {
		_, e, err := Parse(strings.NewReader(text), nil)
		if err != nil {
			t.Fatalf("%q: Parse() = %v", text, err)
		}
		if mt, _, terr := e.EffectiveContentType(); mt != "text/plain" || terr != nil {
			t.Errorf("%q: EffectiveContentType() = (%q, %v), want text/plain", text, mt, terr)
		}
		if e.MultipartReader() != nil || e.Header.Has("Content-Type") || e.Header.Has("Content-Transfer-Encoding") {
			t.Errorf("%q: the prefix was interpreted", text)
		}
		body, berr := io.ReadAll(e.Body)
		if want := text[strings.Index(text, "\r\n\r\n")+4:]; string(body) != want || berr != nil {
			t.Errorf("%q: body (%q, %v), want %q", text, body, berr, want)
		}
	}
}

// middleChild is the middle-child reference fixture: a multipart whose middle
// part's header begins with a malformed prefix.
const middleChildB64 = "Q29udGVudC1UeXBlOiBtdWx0aXBhcnQvbWl4ZWQ7IGJvdW5kYXJ5PUINCg0KLS1CDQpTdWJqZWN0OiBmaXJzdA0KDQpvbmUNCi0tQg0KIG9ycGhhbg0KU3ViamVjdDogbWlkZGxlDQoNCnR3bw0KLS1CDQpTdWJqZWN0OiBsYXN0DQoNCnRocmVlDQotLUItLQ0K"

func middleChild() string {
	b, err := base64.StdEncoding.DecodeString(middleChildB64)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestParse_firstLineMiddleChild pins the middle-child fixture: the middle part
// keeps its prefix, its fields and its body, and the part after it is read.
// Under Tolerant and Forensic Walk visits every part; under Strict the middle
// part is returned with its own refusal, NextPart then returns the last part,
// and Walk ends at the refused part, as for every Strict refusal.
func TestParse_firstLineMiddleChild(t *testing.T) {
	text := middleChild()
	at := int64(strings.Index(text, " orphan"))
	note := lineEndingFacts{CodeHeaderFirstLineContinuation, SeverityRecovered, []int{1}, at, at + int64(len(" orphan\r\n"))}
	for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
		for _, compat := range framingCompatibilities {
			for name, src := range lineEndingSources(text) {
				label := fmt.Sprintf("%v/%v/%s", profile, compat, name)
				session, e, err := Parse(src(), &ParseOptions{Profile: profile, Compatibility: compat})
				if err != nil {
					t.Fatalf("%s: Parse() = %v", label, err)
				}
				mr := e.MultipartReader()
				var subjects, bodies []string
				for i := range 3 {
					p, err := mr.NextPart()
					if p == nil {
						t.Fatalf("%s: part %d = %v", label, i, err)
					}
					if i == 1 {
						checkPrefix(t, label, p, " orphan\r\n", 0, 1)
						if profile == ProfileStrict {
							want := note
							want.severity = SeverityInvalidMetadata
							checkFirstLineRefusal(t, label, p, err, []lineEndingFacts{want})
						} else if err != nil {
							t.Errorf("%s: part 1 err = %v", label, err)
						}
					} else if err != nil {
						t.Errorf("%s: part %d err = %v", label, i, err)
					}
					b, _ := io.ReadAll(p.Body)
					subjects = append(subjects, p.Header.Get("Subject"))
					bodies = append(bodies, string(b))
				}
				if !reflect.DeepEqual(subjects, []string{"first", "middle", "last"}) || !reflect.DeepEqual(bodies, []string{"one", "two", "three"}) {
					t.Errorf("%s: parts (%q, %q)", label, subjects, bodies)
				}
				if _, err := mr.NextPart(); err != io.EOF {
					t.Errorf("%s: end = %v", label, err)
				}
				want := []lineEndingFacts{note}
				if profile == ProfileStrict {
					want = strictFacts(want)
				}
				if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: diagnostics = %+v, want %+v", label, got, want)
				}
			}
		}

		_, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
		if err != nil {
			t.Fatal(err)
		}
		var visited []string
		werr := e.Walk(func(path []int, p *Entity, err error) error {
			visited = append(visited, fmt.Sprint(path))
			return nil
		})
		if profile == ProfileStrict {
			if !IsFatal(werr) || e.ParseStop() != nil || !reflect.DeepEqual(visited, []string{"[]", "[0]"}) {
				t.Errorf("%v: Walk() = %v after %q, want the refusal of [1]", profile, werr, visited)
			}
			continue
		}
		if werr != nil || !reflect.DeepEqual(visited, []string{"[]", "[0]", "[1]", "[2]"}) {
			t.Errorf("%v: Walk() = %v after %q, want every part", profile, werr, visited)
		}
	}
}

// TestParse_firstLineDelimiterRecovery pins that a parent's delimiter line
// after a part header's malformed prefix is never read as a field: the damaged
// part is returned with its prefix, no field and an empty body, a missing
// separator is recorded, and the healthy part after it — or the end of the
// multipart — follows on the next call. Before ROB-020-001 the damaged part
// was an error and the next call reached the same healthy part; nothing that
// was recoverable is lost. Boundaries with a colon, every line ending, opening
// and closing delimiters, every profile and three chunkings are covered.
// Strict refuses the damaged part, and a bare CR around the next delimiter
// then refuses the rest of the multipart, as it always did.
func TestParse_firstLineDelimiterRecovery(t *testing.T) {
	allowed := map[Code]bool{
		CodeHeaderFirstLineContinuation: true, CodeHeaderMissingBodySeparator: true,
		CodeHeaderBareLF: true, CodeHeaderBareCR: true, CodeHeaderMixedLineEndings: true,
		CodeMultipartBareLF: true, CodeMultipartBareCR: true, CodeMultipartMixedLineEndings: true,
	}
	for _, boundary := range []string{"B", "b:c"} {
		for _, eol := range []string{"\r\n", "\n", "\r"} {
			for _, closing := range []bool{false, true} {
				for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
					for _, chunk := range []int{1, 7, 4096} {
						root := "Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n"
						text := root + "--" + boundary + "\r\n orphan" + eol
						if closing {
							text += "--" + boundary + "--" + eol
						} else {
							text += "--" + boundary + eol + "Subject: healthy\r\n\r\nKEPT\r\n--" + boundary + "--\r\n"
						}
						label := fmt.Sprintf("%q/%q/close %v/%v/chunk %d", boundary, eol, closing, profile, chunk)
						session, e, err := Parse(&chunkReader{s: text, n: chunk}, &ParseOptions{Profile: profile,
							Limits: Limits{MaxInputBytes: 8192, MaxPartCount: 16, MaxMIMEDepth: 8}})
						if err != nil {
							t.Fatalf("%s: Parse() = %v", label, err)
						}
						mr := e.MultipartReader()
						p, err := mr.NextPart()
						if p == nil {
							t.Fatalf("%s: the damaged part was not returned: %v", label, err)
						}
						h := int64(len(root) + len("--"+boundary+"\r\n"))
						checkPrefix(t, label, p, " orphan"+eol, 0, 1)
						first := lineEndingFacts{CodeHeaderFirstLineContinuation, SeverityRecovered, []int{0}, h, h + int64(len(" orphan"+eol))}
						sep := lineEndingFacts{CodeHeaderMissingBodySeparator, SeverityRecovered, []int{0}, first.end, first.end}
						if profile == ProfileStrict {
							first.severity, sep.severity = SeverityInvalidMetadata, SeverityInvalidMetadata
							checkFirstLineRefusal(t, label, p, err, []lineEndingFacts{first, sep})
						} else if err != nil {
							t.Errorf("%s: damaged part err = %v", label, err)
						}
						if body, berr := io.ReadAll(p.Body); len(body) != 0 || berr != nil || p.Header.Len() != 0 {
							t.Errorf("%s: damaged part body (%q, %v), %d fields", label, body, berr, p.Header.Len())
						}

						p, err = mr.NextPart()
						switch {
						case profile == ProfileStrict && eol == "\r":
							if p != nil || !IsFatal(err) || e.ParseStop() != nil {
								t.Errorf("%s: after the damaged part (%v, %v), want the multipart.bare_cr refusal", label, p != nil, err)
							}
						case closing:
							if p != nil || err != io.EOF {
								t.Errorf("%s: after the damaged part (%v, %v), want io.EOF", label, p != nil, err)
							}
						default:
							if p == nil || err != nil || p.Header.Get("Subject") != "healthy" {
								t.Fatalf("%s: after the damaged part (%v, %v), want the healthy part", label, p != nil, err)
							}
							if body, berr := io.ReadAll(p.Body); string(body) != "KEPT" || berr != nil {
								t.Errorf("%s: healthy body (%q, %v)", label, body, berr)
							}
							if _, err := mr.NextPart(); err != io.EOF {
								t.Errorf("%s: end = %v", label, err)
							}
						}

						var seen []lineEndingFacts
						for _, f := range factsOf(session.Diagnostics()) {
							if !allowed[f.code] {
								t.Errorf("%s: unexpected diagnostic %+v", label, f)
							}
							if f.code == CodeHeaderFirstLineContinuation || f.code == CodeHeaderMissingBodySeparator {
								seen = append(seen, f)
							}
						}
						if !reflect.DeepEqual(seen, []lineEndingFacts{first, sep}) {
							t.Errorf("%s: prefix diagnostics %+v, want %+v", label, seen, []lineEndingFacts{first, sep})
						}
					}
				}
			}
		}
	}
}

// TestParse_firstLineStrictDiagnosticBudget pins that every part Strict refuses
// for its prefix carries its own copy of the diagnostic, whatever
// Limits.MaxDiagnosticCount let the session keep, and that the refusals stop
// nothing.
func TestParse_firstLineStrictDiagnosticBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	var at []int64
	for i := range 3 {
		b.WriteString("--B\r\n")
		at = append(at, int64(b.Len()))
		fmt.Fprintf(&b, " prefix %d\r\nX-N: %d\r\n\r\nbody %d\r\n", i, i, i)
	}
	b.WriteString("--B--\r\n")
	for _, maxDiags := range []int64{0, 1, 2} {
		opts := &ParseOptions{Profile: ProfileStrict}
		if maxDiags > 0 {
			opts.Limits.MaxDiagnosticCount = maxDiags
		}
		session, e, err := Parse(strings.NewReader(b.String()), opts)
		if err != nil || e == nil {
			t.Fatalf("max %d: Parse() = (%v, %v)", maxDiags, e, err)
		}
		mr := e.MultipartReader()
		for i := range 3 {
			p, err := mr.NextPart()
			var fe FatalError
			if p == nil || !errors.As(err, &fe) {
				t.Fatalf("max %d: part %d = (%v, %v), want the part and its FatalError", maxDiags, i, p != nil, err)
			}
			if d, ok := fe.Diagnostic(); !ok || d.Code != CodeHeaderFirstLineContinuation || d.Location.Offset != at[i] || !reflect.DeepEqual(d.Path, []int{i}) {
				t.Errorf("max %d: part %d refusal = %+v, want header.first_line_continuation of [%d] at %d", maxDiags, i, d, i, at[i])
			}
			if p.Header.Get("X-N") != fmt.Sprint(i) {
				t.Errorf("max %d: part %d lost its field", maxDiags, i)
			}
			if body, _ := io.ReadAll(p.Body); string(body) != fmt.Sprintf("body %d", i) {
				t.Errorf("max %d: part %d body %q", maxDiags, i, body)
			}
		}
		if _, err := mr.NextPart(); err != io.EOF || e.ParseStop() != nil {
			t.Errorf("max %d: after the parts (%v, stop %v), want io.EOF and no stop", maxDiags, err, e.ParseStop())
		}
		want := int64(3)
		if maxDiags > 0 {
			want = maxDiags
		}
		if got := int64(len(session.Diagnostics())); got != want {
			t.Errorf("max %d: %d diagnostics, want %d", maxDiags, got, want)
		}
	}
}

// TestParse_firstLineSinks pins a sink stopping the parse on the prefix's
// diagnostic, ahead of a Strict refusal; a sink that reads the session while
// it is told, which must not deadlock; and diagnostics delivered and discarded.
func TestParse_firstLineSinks(t *testing.T) {
	const text = " p\r\nA: 1\r\n\r\nbody"
	errSink := errors.New("sink: enough")
	for _, profile := range []Profile{ProfileTolerant, ProfileStrict} {
		sink := DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return errSink })
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile, DiagnosticSink: sink})
		if e == nil || !errors.Is(err, errSink) || session.fatalErr() == nil || error(session.fatalErr()) != err {
			t.Errorf("%v: Parse() = (%v, %v), want the entity and the session's stop", profile, e != nil, err)
		}
		if e != nil && string(e.Header.MalformedPrefix().Bytes()) != " p\r\n" {
			t.Errorf("%v: the stopped entity lost its prefix", profile)
		}
	}

	var got []Code
	reader := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		_ = s.Diagnostics()
		_ = s.Len()
		got = append(got, d.Code)
		return nil
	})
	for _, discard := range []bool{false, true} {
		got = nil
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{DiagnosticSink: reader, DiscardDeliveredDiagnostics: discard})
		if err != nil || e == nil {
			t.Fatalf("discard %v: Parse() = (%v, %v)", discard, e, err)
		}
		if !reflect.DeepEqual(got, []Code{CodeHeaderFirstLineContinuation}) {
			t.Errorf("discard %v: delivered %v", discard, got)
		}
		if n := len(session.Diagnostics()); (n == 0) != discard {
			t.Errorf("discard %v: %d retained", discard, n)
		}
	}
}

// TestParse_firstLineBudgets pins what the header budgets count: the prefix is
// one field candidate, its lines and bytes charged as they are read; a refused
// header leaves no entity and records the refusal alone.
func TestParse_firstLineBudgets(t *testing.T) {
	const text = " aaaa\r\n bbbb\r\nA: 1\r\n\r\nbody" // prefix 14, lines of 5, block 22
	exact := Limits{
		MaxHeaderBytes: 22, MaxLineBytes: 5, MaxHeaderFieldBytes: 14, MaxHeaderCount: 2,
		MaxTotalHeaderBytes: 22, MaxTotalHeaderCount: 2, MaxInputBytes: int64(len(text)),
	}
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: exact})
	if err != nil || e == nil || e.Header.Get("A") != "1" {
		t.Fatalf("exact budgets: (%v, %v)", e, err)
	}
	checkPrefix(t, "exact budgets", e, " aaaa\r\n bbbb\r\n", 0, 2)
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeHeaderFirstLineContinuation}) {
		t.Errorf("exact budgets: diagnostics %v", codes)
	}
	for _, tc := range []struct {
		name   string
		limits Limits
		code   Code
		offset int64
	}{
		{"header bytes", Limits{MaxHeaderBytes: 21}, CodeLimitHeaderBytes, 0},
		{"line bytes", Limits{MaxLineBytes: 4}, CodeLimitLineBytes, 0},
		{"field bytes", Limits{MaxHeaderFieldBytes: 13}, CodeLimitHeaderFieldBytes, 0},
		{"field count", Limits{MaxHeaderCount: 1}, CodeLimitHeaderCount, 14},
		{"total header bytes", Limits{MaxTotalHeaderBytes: 21}, CodeLimitTotalHeaderBytes, 21},
		{"total header count", Limits{MaxTotalHeaderCount: 1}, CodeLimitTotalHeaderCount, 14},
	} {
		s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: tc.limits})
		checkFramingRefusal(t, tc.name, s, e, err, tc.code, tc.offset)
	}

	// MaxInputBytes before, at and past the header: no entity until the header
	// is whole, then the entity with its prefix.
	for n := int64(1); n <= int64(len(text)); n++ {
		s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: Limits{MaxInputBytes: n}})
		if e == nil {
			if err == nil || !IsFatal(err) || error(s.fatalErr()) != err {
				t.Errorf("input %d: (nil, %v), want the input budget's stop", n, err)
			}
			continue
		}
		checkPrefix(t, fmt.Sprintf("input %d", n), e, " aaaa\r\n bbbb\r\n", 0, 2)
	}
}

// TestParse_firstLineSharedCount pins the shared field count across a part
// whose header begins with a prefix: the prefix is one candidate, refused at
// its first byte once the count is spent, and the field after it at its own.
func TestParse_firstLineSharedCount(t *testing.T) {
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	const part = " p\r\nA: 1\r\n\r\n"
	text := root + "--B\r\n" + part + "body\r\n--B--\r\n"
	h := int64(len(root) + len("--B\r\n"))
	for _, tc := range []struct {
		limit int64
		at    int64 // -1: the part is read
	}{
		{1, h},
		{2, h + int64(len(" p\r\n"))},
		{3, -1},
	} {
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: totalsOnly("count", tc.limit)})
		if err != nil {
			t.Fatal(err)
		}
		p, err := e.MultipartReader().NextPart()
		if tc.at < 0 {
			if err != nil || p == nil || p.Header.Get("A") != "1" {
				t.Errorf("limit %d: NextPart() = (%v, %v), want the part", tc.limit, p != nil, err)
			}
			continue
		}
		if p != nil {
			t.Errorf("limit %d: a part returned past the budget", tc.limit)
		}
		checkTotalStop(t, session, err, "count", tc.limit, &totalBlock{path: []int{0}}, tc.at)
	}
}

// TestParse_firstLineInputErrors cuts the input at every byte: a failure
// inside the header leaves no entity and is returned as itself with nothing
// recorded; once the header is whole the entity is returned with its prefix,
// and its body ends with the identical failure after the bytes before it. The
// input is never read after its failure.
func TestParse_firstLineInputErrors(t *testing.T) {
	const block = " orphan\r\n\tmore\r\nA: 1\r\n\r\n"
	const text = block + "body"
	for cut := 0; cut <= len(text); cut++ {
		for _, chunk := range []int{1, 7, 4096} {
			for _, withData := range []bool{false, true} {
				for _, sticky := range []bool{true, false} {
					name := fmt.Sprintf("cut %d/chunk %d/with %v/sticky %v", cut, chunk, withData, sticky)
					src := &ioSource{data: text, cut: cut, chunk: chunk, err: errSourceIO, withData: withData, sticky: sticky}
					s, e, err := Parse(src, nil)
					if cut < len(block) {
						if e != nil || err != errSourceIO || s.Len() != 0 {
							t.Errorf("%s: Parse() = (entity %v, %v, %v), want no entity, the input's error and nothing recorded",
								name, e != nil, err, codesOfDiags(s.Diagnostics()))
						}
					} else {
						if e == nil || err != nil {
							t.Fatalf("%s: Parse() = (entity %v, %v), want the entity", name, e != nil, err)
						}
						checkPrefix(t, name, e, " orphan\r\n\tmore\r\n", 0, 2)
						body, berr := io.ReadAll(e.Body)
						if !strings.HasPrefix(text[len(block):cut], string(body)) || berr != errSourceIO {
							t.Errorf("%s: body (%q, %v), want a prefix of %q and the input's error", name, body, berr, text[len(block):cut])
						}
					}
					if src.readsAfter != 0 {
						t.Errorf("%s: %d reads after the input failed", name, src.readsAfter)
					}
				}
			}
		}
	}
}

// TestParseContext_firstLineCancel cancels the parse at every byte of the
// header, delivered one byte at a time, the cancelling read returning its byte
// with nil, io.EOF or an error: before the header is whole no entity is
// returned and the stop is; once it is, the entity is returned with its
// prefix, with the stop or without it. Nothing is read after the cancelling
// read.
func TestParseContext_firstLineCancel(t *testing.T) {
	const block = " orphan\r\n\tmore\r\nA: 1\r\n\r\n"
	const text = block + "body"
	for at := 1; at <= len(block)+1; at++ {
		for _, mode := range []cutMode{cutNil, cutEOF, cutErr} {
			name := fmt.Sprintf("at %d/%v", at, mode)
			ctx, src := newCut(t, text, 1, at, mode)
			s, e, err := ParseContext(ctx, src, nil)
			if at < len(block) {
				if e != nil {
					t.Errorf("%s: an entity, want none", name)
				}
				checkCancelStop(t, name, err, context.Canceled)
			} else if e == nil {
				t.Errorf("%s: no entity (%v), want the entity", name, err)
			} else {
				if err != nil && err != s.fatalErr() {
					t.Errorf("%s: err = %v, want nil or the stop", name, err)
				}
				checkPrefix(t, name, e, " orphan\r\n\tmore\r\n", 0, 2)
			}
			if src.after != 0 {
				t.Errorf("%s: %d reads after the cancelling read", name, src.after)
			}
		}
	}

	// One read delivers the whole input and cancels: the header is buffered
	// whole, so the entity is returned, with its prefix, and its body ends with
	// the stop after the bytes already buffered.
	for _, mode := range []cutMode{cutNil, cutEOF, cutErr} {
		name := fmt.Sprintf("buffered/%v", mode)
		ctx, src := newCut(t, text, 4096, 1, mode)
		s, e, err := ParseContext(ctx, src, nil)
		if e == nil {
			t.Fatalf("%s: no entity (%v), want the entity", name, err)
		}
		if err != nil && err != s.fatalErr() {
			t.Errorf("%s: err = %v, want nil or the stop", name, err)
		}
		checkPrefix(t, name, e, " orphan\r\n\tmore\r\n", 0, 2)
		body, berr := io.ReadAll(e.Body)
		if !strings.HasPrefix("body", string(body)) {
			t.Errorf("%s: body %q, want a prefix of the input's", name, body)
		}
		checkCancelStop(t, name, berr, context.Canceled)
		if src.after != 0 {
			t.Errorf("%s: %d reads after the cancelling read", name, src.after)
		}
	}
}

// TestParse_firstLineNoProgress stalls the input inside the prefix and the
// header after it: one empty read fewer than the limit changes nothing, the
// limit stops the parse there with no entity.
func TestParse_firstLineNoProgress(t *testing.T) {
	const block = " orphan\r\n\tmore\r\nA: 1\r\n\r\n"
	const text = block + "body"
	for k := 1; k < len(block); k++ {
		for _, chunk := range []int{1, 4096} {
			for _, z := range []int{maxConsecutiveEmptyReads - 1, maxConsecutiveEmptyReads} {
				name := fmt.Sprintf("at %d/chunk %d/%d empty reads", k, chunk, z)
				src := newZeroSource(text, chunk, map[int]int{k: z})
				s, e, err := Parse(src, nil)
				if z < maxConsecutiveEmptyReads {
					if err != nil || e == nil {
						t.Errorf("%s: (%v, %v), want the entity", name, e != nil, err)
						continue
					}
					checkPrefix(t, name, e, " orphan\r\n\tmore\r\n", 0, 2)
					continue
				}
				if e != nil || err == nil || err != s.fatalErr() {
					t.Errorf("%s: (entity %v, %v), want no entity and the stop", name, e != nil, err)
					continue
				}
				if d := noProgressStop(t, err, CodeInputNoProgress); d.Location.Offset != int64(k) {
					t.Errorf("%s: input.no_progress at %v, want offset %d", name, d.Location, k)
				}
				if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputNoProgress}) {
					t.Errorf("%s: diagnostics %v, want one input.no_progress", name, codes)
				}
			}
		}
	}
}

// TestEntity_firstLineWrite pins the writers: the prefix is written first,
// before every field and the MIME-Version WriteTo adds, a complete CRLF header
// that already has one byte for byte; the caller's header is not changed; the
// output parses back to the same prefix, fields and body, and is written the
// same again. A part built by NewMultipart from a header with a prefix is
// written with it.
func TestEntity_firstLineWrite(t *testing.T) {
	for _, tc := range []struct {
		in     string
		mutate func(*Header)
		want   string
	}{
		{" p\r\nMime-Version: 1.0\r\nA: 1\r\n\r\nbody", nil, " p\r\nMime-Version: 1.0\r\nA: 1\r\n\r\nbody"},
		{bomUTF8 + " p\r\nMime-Version: 1.0\r\n\r\nbody", nil, bomUTF8 + " p\r\nMime-Version: 1.0\r\n\r\nbody"},
		{" Subject: shifted\r\nX-Keep: yes\r\n\r\nbody", nil, " Subject: shifted\r\nMime-Version: 1.0\r\nX-Keep: yes\r\n\r\nbody"},
		{" p\nA: 1\n\nbody", nil, " p\nMime-Version: 1.0\r\nA: 1\r\n\r\nbody"},
		{" p\rA: 1\r\rbody", nil, " p\r\nMime-Version: 1.0\r\nA: 1\r\n\r\nbody"},
		{" p", nil, " p\r\nMime-Version: 1.0\r\n\r\n"},
		{" p\r\nA: 1\r\n\r\nbody", func(h *Header) { h.Set("Subject", "new") }, " p\r\nMime-Version: 1.0\r\nSubject: new\r\nA: 1\r\n\r\nbody"},
		{" p\r\nA: 1\r\n\r\nbody", func(h *Header) { h.Del("A") }, " p\r\nMime-Version: 1.0\r\n\r\nbody"},
	} {
		_, e, err := Parse(strings.NewReader(tc.in), nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.mutate != nil {
			tc.mutate(&e.Header)
		}
		before := e.Header.Copy()
		var out bytes.Buffer
		if err := e.WriteTo(&out); err != nil || out.String() != tc.want {
			t.Errorf("%q: WriteTo() = (%q, %v), want %q", tc.in, out.String(), err, tc.want)
			continue
		}
		if e.Header.Has("Mime-Version") != before.Has("Mime-Version") || e.Header.Len() != before.Len() ||
			!bytes.Equal(e.Header.MalformedPrefix().Bytes(), before.MalformedPrefix().Bytes()) {
			t.Errorf("%q: WriteTo changed the entity's header", tc.in)
		}
		_, again, err := Parse(bytes.NewReader(out.Bytes()), nil)
		if err != nil {
			t.Errorf("%q: reparse = %v", tc.in, err)
			continue
		}
		var second bytes.Buffer
		if err := again.WriteTo(&second); err != nil || second.String() != tc.want {
			t.Errorf("%q: second WriteTo() = (%q, %v), want %q", tc.in, second.String(), err, tc.want)
		}
	}

	_, src, err := Parse(strings.NewReader(" orphan\r\nSubject: middle\r\n\r\ntwo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	part, err := New(src.Header, strings.NewReader("two"))
	if err != nil {
		t.Fatal(err)
	}
	var mh Header
	mh.Set("Content-Type", "multipart/mixed; boundary=X")
	m, err := NewMultipart(mh, []*Entity{part})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := m.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--X\r\n orphan\r\nSubject: middle\r\n") {
		t.Errorf("NewMultipart wrote %q, want the part's prefix first in its header", out.String())
	}
	_, e, err := Parse(bytes.NewReader(out.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.MultipartReader().NextPart()
	if err != nil || p.Header.Get("Subject") != "middle" || string(p.Header.MalformedPrefix().Bytes()) != " orphan\r\n" {
		t.Fatalf("reparse: (%v, %v)", p, err)
	}
	if body, _ := io.ReadAll(p.Body); string(body) != "two" {
		t.Errorf("reparse: body %q", body)
	}
}

// firstLineOutcome is what one root parse yields of the header framing alone:
// whether there is an entity, the error's text, the prefix, the fields' raw
// bytes, where the body begins and the diagnostics. No body is read.
type firstLineOutcome struct {
	entity bool
	err    string
	prefix string
	fields []string
	bodyAt int64
	facts  []lineEndingFacts
}

func firstLineOutcomeOf(t *testing.T, src io.Reader, profile Profile) firstLineOutcome {
	t.Helper()
	limits := Limits{MaxHeaderCount: LimitDisabled, MaxTotalHeaderCount: LimitDisabled}
	s, e, err := Parse(src, &ParseOptions{Profile: profile, Limits: limits})
	o := firstLineOutcome{facts: factsOf(s.Diagnostics())}
	if err != nil {
		o.err = err.Error()
	}
	if e == nil {
		return o
	}
	o.entity = true
	o.prefix = string(e.Header.MalformedPrefix().Bytes())
	fs := e.Header.Fields()
	for fs.Next() {
		raw, rerr := fs.Raw()
		if rerr != nil {
			t.Fatalf("Raw() = %v", rerr)
		}
		o.fields = append(o.fields, string(raw))
	}
	o.bodyAt = e.parse.origin.bodyOffset
	if e.ParseStop() != nil {
		t.Fatalf("ParseStop() = %v for a finite string under default budgets", e.ParseStop())
	}
	return o
}

// FuzzParse_firstLine holds the root framing of an input of at most 4 KiB that
// begins with white space to what the text says, with nothing decoded and no
// body read (#34 stays out of it):
//
//   - The outcome is the same whole and read one or seven bytes at a time.
//   - The prefix textproto reads is the input's own first bytes, from a byte
//     of white space; whatever follows it does not begin with white space.
//   - Suffix equivalence: a Tolerant parse of the input is the Tolerant parse
//     of what follows the prefix — the same entity or none, the same error, the
//     same fields, the body at the same place moved by the prefix's length —
//     with the prefix and one header.first_line_continuation over it, first,
//     added. What follows is not compared when it begins with a byte that can
//     begin a byte order mark, which only the start of a block recognizes, nor
//     the other diagnostics, which the prefix's own line endings may change.
//   - Strict returns an entity exactly when Tolerant does, refused for the
//     prefix, with the same prefix and fields and no stop.
func FuzzParse_firstLine(f *testing.F) {
	for _, tc := range firstLineCases() {
		f.Add([]byte(tc.input()))
	}
	f.Add([]byte(middleChild()))
	f.Add([]byte(" a\rb c\r\nX: 1\r\n\r\nbody"))
	f.Add([]byte(" a\rX: 1\r\rbody"))
	f.Add([]byte(" a\r\n\xef\xbb\xbfX: 1\r\n\r\n"))
	f.Add([]byte(" a\r\nno colon\r\n\r\n"))
	f.Add([]byte(" a\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || len(data) > 4096 || (data[0] != ' ' && data[0] != '\t') {
			return
		}
		text := string(data)
		want := firstLineOutcomeOf(t, strings.NewReader(text), ProfileTolerant)
		for _, n := range []int{1, 7} {
			if got := firstLineOutcomeOf(t, &chunkReader{s: text, n: n}, ProfileTolerant); !reflect.DeepEqual(got, want) {
				t.Fatalf("chunks of %d:\n got %+v\nwant %+v", n, got, want)
			}
		}

		h, _ := textproto.ReadHeader(bufio.NewReader(strings.NewReader(text)))
		prefix := string(h.MalformedPrefix().Bytes())
		if prefix == "" || !strings.HasPrefix(text, prefix) {
			t.Fatalf("prefix %q is not the input's first bytes", prefix)
		}
		rest := text[len(prefix):]
		if rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			t.Fatalf("what follows the prefix %q begins with white space: %q", prefix, rest)
		}
		if want.entity {
			note := lineEndingFacts{CodeHeaderFirstLineContinuation, SeverityRecovered, nil, 0, int64(len(prefix))}
			if want.prefix != prefix || len(want.facts) == 0 || !reflect.DeepEqual(want.facts[0], note) {
				t.Fatalf("entity prefix %q, diagnostics %+v, want %q and %+v first", want.prefix, want.facts, prefix, note)
			}
		}

		if rest == "" || (rest[0] != 0xef && rest[0] != 0xfe && rest[0] != 0xff && rest[0] != 0x00) {
			suffix := firstLineOutcomeOf(t, strings.NewReader(rest), ProfileTolerant)
			if suffix.entity != want.entity || suffix.err != want.err || !reflect.DeepEqual(suffix.fields, want.fields) ||
				suffix.prefix != "" || (want.entity && suffix.bodyAt+int64(len(prefix)) != want.bodyAt) {
				t.Fatalf("what follows the prefix parses to %+v, the whole input to %+v", suffix, want)
			}
		}

		strict := firstLineOutcomeOf(t, strings.NewReader(text), ProfileStrict)
		if strict.entity != want.entity {
			t.Fatalf("Strict entity %v, Tolerant %v", strict.entity, want.entity)
		}
		if want.entity && (strict.prefix != want.prefix || !reflect.DeepEqual(strict.fields, want.fields) ||
			!strings.Contains(strict.err, string(CodeHeaderFirstLineContinuation))) {
			t.Fatalf("Strict %+v, want the Tolerant prefix and fields and the prefix's refusal", strict)
		}
	})
}

// TestParse_firstLineLongPadding pins, through Parse, a line after a part
// header's prefix that begins like a delimiter, its white space running to,
// across and past the 4 KiB buffer, under line and field budgets that admit it
// (the case Codex's boundary QA found: a valid field `--B` + 4093 spaces +
// `: value` was taken for a delimiter). A colon after the white space makes it
// a field: the part keeps it and the fields after it, its body is its own, and
// no missing separator is recorded. A line ending after the white space proves
// the delimiter: the damaged part has an empty body, header.missing_body_separator
// is recorded where the delimiter line begins, and the part after it is read.
func TestParse_firstLineLongPadding(t *testing.T) {
	limits := Limits{MaxInputBytes: 32768, MaxHeaderBytes: 16384, MaxHeaderFieldBytes: 12000, MaxLineBytes: 12000}
	for _, boundary := range []string{"B", "b:c"} {
		root := "Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n--" + boundary + "\r\n"
		const prefix = " orphan\r\n"
		for _, padding := range []int{0, 1, 16, 4090, 4093, 4096, 5000} {
			pad := strings.Repeat(" ", padding)
			for _, chunk := range []int{1, 7, 4096} {
				for _, profile := range []Profile{ProfileTolerant, ProfileForensic} {
					label := fmt.Sprintf("%q/padding %d/chunk %d/%v", boundary, padding, chunk, profile)
					parse := func(line string) (*ParseSession, MultipartReader) {
						t.Helper()
						text := root + prefix + line + "\r\nSubject: kept\r\n\r\nBODY\r\n--" + boundary + "--\r\n"
						s, e, err := Parse(&chunkReader{s: text, n: chunk}, &ParseOptions{Profile: profile, Limits: limits})
						if err != nil || e == nil {
							t.Fatalf("%s: Parse() = (%v, %v)", label, e, err)
						}
						return s, e.MultipartReader()
					}

					// The field.
					s, mr := parse("--" + boundary + pad + ": value")
					p, err := mr.NextPart()
					if err != nil || p == nil {
						t.Fatalf("%s: field: NextPart() = (%v, %v)", label, p, err)
					}
					wantKey, wantValue := "--"+boundary, "value"
					if boundary == "b:c" {
						wantKey, wantValue = "--b", "c"+pad+": value"
					}
					if p.Header.Get("Subject") != "kept" || p.Header.Get(wantKey) != wantValue || string(p.Header.MalformedPrefix().Bytes()) != prefix {
						t.Errorf("%s: field: header (%q, %q, %q)", label, p.Header.Get("Subject"), p.Header.Get(wantKey), p.Header.MalformedPrefix().Bytes())
					}
					if body, berr := io.ReadAll(p.Body); string(body) != "BODY" || berr != nil {
						t.Errorf("%s: field: body (%q, %v)", label, body, berr)
					}
					if _, err := mr.NextPart(); err != io.EOF {
						t.Errorf("%s: field: end = %v", label, err)
					}
					for _, d := range s.Diagnostics() {
						if d.Code == CodeHeaderMissingBodySeparator {
							t.Errorf("%s: field: a missing separator was recorded", label)
						}
					}

					// The delimiter.
					s, mr = parse("--" + boundary + pad)
					p, err = mr.NextPart()
					if err != nil || p == nil || p.Header.Len() != 0 {
						t.Fatalf("%s: delimiter: NextPart() = (%v, %v)", label, p, err)
					}
					if body, berr := io.ReadAll(p.Body); len(body) != 0 || berr != nil {
						t.Errorf("%s: delimiter: damaged body (%q, %v)", label, body, berr)
					}
					p, err = mr.NextPart()
					if err != nil || p == nil || p.Header.Get("Subject") != "kept" {
						t.Fatalf("%s: delimiter: next NextPart() = (%v, %v), want the healthy part", label, p, err)
					}
					if body, berr := io.ReadAll(p.Body); string(body) != "BODY" || berr != nil {
						t.Errorf("%s: delimiter: healthy body (%q, %v)", label, body, berr)
					}
					if _, err := mr.NextPart(); err != io.EOF {
						t.Errorf("%s: delimiter: end = %v", label, err)
					}
					at := int64(len(root + prefix))
					found := 0
					for _, f := range factsOf(s.Diagnostics()) {
						if f.code == CodeHeaderMissingBodySeparator {
							found++
							if f.start != at || f.end != at || !reflect.DeepEqual(f.path, []int{0}) {
								t.Errorf("%s: delimiter: %+v, want the missing separator of [0] at %d", label, f, at)
							}
						}
					}
					if found != 1 {
						t.Errorf("%s: delimiter: %d missing separators recorded, want 1", label, found)
					}
				}
			}
		}
	}
}

// TestParse_firstLineLongPaddingHeaderBudgets pins, through Parse, that a
// padded delimiter after a part header's prefix is proved under the default-
// shaped line budget however little the header budgets have left: every block
// and field budget, and the parse's shared header bytes and count, set to
// exactly what the header blocks spend. The healthy part after an opening
// delimiter is read, a closing one ends the multipart, and nothing stops.
func TestParse_firstLineLongPaddingHeaderBudgets(t *testing.T) {
	const prefix = " orphan\r\n"
	const sibling = "Subject: kept\r\n\r\n"
	for _, boundary := range []string{"B", "b:c"} {
		root := "Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n"
		for _, padding := range []int{0, 4093, 5000} {
			for _, closing := range []bool{false, true} {
				for _, chunk := range []int{1, 4096} {
					label := fmt.Sprintf("%q/padding %d/close %v/chunk %d", boundary, padding, closing, chunk)
					delim := "--" + boundary + strings.Repeat(" ", padding)
					blocks, count := int64(len(root+prefix+sibling)), int64(3)
					if closing {
						delim = "--" + boundary + "--" + strings.Repeat(" ", padding)
						blocks, count = int64(len(root+prefix)), 2
					}
					text := root + "--" + boundary + "\r\n" + prefix + delim + "\r\n" + sibling + "BODY\r\n--" + boundary + "--\r\n"
					limits := Limits{
						MaxInputBytes: 32768, MaxLineBytes: 12000,
						MaxHeaderBytes: int64(len(root)), MaxHeaderFieldBytes: int64(len(root) - 2), MaxHeaderCount: 1,
						MaxTotalHeaderBytes: blocks, MaxTotalHeaderCount: count,
					}
					s, e, err := Parse(&chunkReader{s: text, n: chunk}, &ParseOptions{Limits: limits})
					if err != nil || e == nil {
						t.Fatalf("%s: Parse() = (%v, %v)", label, e, err)
					}
					mr := e.MultipartReader()
					p, err := mr.NextPart()
					if err != nil || p == nil || !bytes.Equal(p.Header.MalformedPrefix().Bytes(), []byte(prefix)) {
						t.Fatalf("%s: NextPart() = (%v, %v), want the damaged part", label, p, err)
					}
					if body, berr := io.ReadAll(p.Body); len(body) != 0 || berr != nil {
						t.Errorf("%s: damaged body (%q, %v)", label, body, berr)
					}
					if !closing {
						p, err = mr.NextPart()
						if err != nil || p == nil || p.Header.Get("Subject") != "kept" {
							t.Fatalf("%s: NextPart() = (%v, %v), want the healthy part", label, p, err)
						}
						if body, berr := io.ReadAll(p.Body); string(body) != "BODY" || berr != nil {
							t.Errorf("%s: healthy body (%q, %v)", label, body, berr)
						}
					}
					if _, err := mr.NextPart(); err != io.EOF || e.ParseStop() != nil {
						t.Errorf("%s: end = %v, stop %v", label, err, e.ParseStop())
					}
					for _, c := range codesOfDiags(s.Diagnostics()) {
						if c != CodeHeaderFirstLineContinuation && c != CodeHeaderMissingBodySeparator {
							t.Errorf("%s: unexpected diagnostic %s", label, c)
						}
					}
				}
			}
		}
	}
}
