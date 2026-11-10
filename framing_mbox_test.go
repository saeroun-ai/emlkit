package message

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit/textproto"
)

// mbox envelope lines under Parse (ROB-010-004). The recognizer and its
// grammar are pinned in mbox_internal_test.go; these tests pin what a parse
// does with a recognized line — kept on the session, the header after it read
// as the header of an input that began there, at moved positions — and that a
// line not recognized, or any input with the option off, is framed exactly as
// before. Every source is a finite string or a call-capped source; the one
// whose failure is bufio.ErrBufferFull runs in a subprocess with a deadline.

// mboxEnv is a recognized envelope line with its CRLF: 45 bytes.
const mboxEnv = mboxContent + "\r\n"

// mboxSimple is mboxEnv, a 14-byte header block of one field, and a body: the
// header begins at 45 and the body at 59.
const mboxSimple = mboxEnv + "Subject: s\r\n\r\nbody"

// mboxOpts returns the options of a parse with or without the option.
func mboxOpts(on bool, profile Profile, limits Limits) *ParseOptions {
	return &ParseOptions{RecognizeMboxEnvelope: on, Profile: profile, Limits: limits}
}

// mboxErr renders err so that two parses whose inputs differ only by a prefix
// of shift bytes render alike: a FatalError by its diagnostic, its position
// moved by shift, and any other error by its text.
func mboxErr(err error, shift int64) string {
	if err == nil {
		return ""
	}
	var fe FatalError
	if errors.As(err, &fe) {
		if d, ok := fe.Diagnostic(); ok {
			start, end := int64(-1), int64(-1)
			if d.Location.HasOffset {
				start, end = d.Location.Offset+shift, d.Location.EndOffset+shift
			}
			return fmt.Sprintf("fatal %v %v %v [%d,%d)", d.Code, d.Severity, d.Path, start, end)
		}
		return "fatal " + err.Error()
	}
	return err.Error()
}

// A mboxOutcome is everything a parse produced, positions moved by a shift.
type mboxOutcome struct {
	entity           bool
	err              string
	diags            []lineEndingFacts
	headerAt, bodyAt int64
	fields           []string
	bodies           []string
	errs             []string
}

// The caps of mboxOutcomeOf's traversal. They are far above what any finite
// fixture of these tests holds, and reaching one fails the test rather than
// cutting the outcome short: an outcome is never silently partial.
const (
	mboxMaxParts = 64      // parts of one multipart
	mboxMaxDepth = 16      // nested multiparts
	mboxMaxBody  = 1 << 16 // bytes of one body
)

// mboxOutcomeOf reads the tree of a parse, within the caps above, and reports
// its outcome, every position moved by shift. It decodes bodies, so it is used
// on finite fixtures whose decoding is well-defined, never on arbitrary input;
// FuzzParse_mboxEnvelope uses mboxRootOf instead. A multipart whose NextPart
// returns no part with an error ends there: the error is recorded once.
func mboxOutcomeOf(t *testing.T, session *ParseSession, e *Entity, err error, shift int64) mboxOutcome {
	t.Helper()
	o := mboxOutcome{entity: e != nil, err: mboxErr(err, shift), headerAt: -1, bodyAt: -1}
	if e != nil {
		o.headerAt, o.bodyAt = e.parse.origin.headerOffset+shift, e.parse.origin.bodyOffset+shift
		var visit func(path []int, e *Entity)
		visit = func(path []int, e *Entity) {
			if len(path) > mboxMaxDepth {
				t.Fatalf("test cap: the tree is deeper than %d multiparts at %v", mboxMaxDepth, path)
			}
			fs := e.Header.Fields()
			for fs.Next() {
				o.fields = append(o.fields, fmt.Sprintf("%v %s=%q", path, fs.Key(), fs.Value()))
			}
			if mr := e.MultipartReader(); mr != nil {
				for i := 0; ; i++ {
					if i == mboxMaxParts {
						t.Fatalf("test cap: more than %d parts at %v", mboxMaxParts, path)
					}
					p, err := mr.NextPart()
					if err == io.EOF {
						return
					}
					child := append(append([]int(nil), path...), i)
					if err != nil {
						o.errs = append(o.errs, fmt.Sprintf("%v %s", child, mboxErr(err, shift)))
						if p == nil {
							return
						}
					}
					visit(child, p)
				}
			}
			b, err := io.ReadAll(io.LimitReader(e.Body, mboxMaxBody+1))
			if len(b) > mboxMaxBody {
				t.Fatalf("test cap: the body at %v has more than %d bytes", path, mboxMaxBody)
			}
			o.bodies = append(o.bodies, fmt.Sprintf("%v %q", path, b))
			if err != nil {
				o.errs = append(o.errs, fmt.Sprintf("%v body %s", path, mboxErr(err, shift)))
			}
		}
		visit(nil, e)
	}
	for _, f := range factsOf(session.Diagnostics()) {
		if f.start >= 0 {
			f.start += shift
			f.end += shift
		}
		o.diags = append(o.diags, f)
	}
	return o
}

// mboxNote is the header.mbox_envelope diagnostic of an n-byte line.
func mboxNote(n int64) lineEndingFacts {
	return lineEndingFacts{CodeHeaderMboxEnvelope, SeverityInfo, nil, 0, n}
}

// checkMboxEnvelope checks that session kept exactly want, or nothing.
func checkMboxEnvelope(t *testing.T, label string, session *ParseSession, want string) {
	t.Helper()
	env, ok := session.MboxEnvelope()
	if want == "" {
		if ok || env.Bytes() != nil {
			t.Errorf("%s: MboxEnvelope() = (%q, %v), want none", label, env.Bytes(), ok)
		}
		return
	}
	if !ok || string(env.Bytes()) != want || env.Location() != LocationRange(0, int64(len(want))) {
		t.Errorf("%s: MboxEnvelope() = (%q at %+v, %v), want %q at [0, %d)", label, env.Bytes(), env.Location(), ok, want, len(want))
	}
}

// checkMboxSuffix checks the core property of a recognized line: the parse of
// text is the parse of what follows the line, without the option and at
// positions moved by its length, with header.mbox_envelope recorded first.
// limits are given to both parses; the input budget of the second is what is
// left of it after the line.
func checkMboxSuffix(t *testing.T, label, text string, n int, profile Profile, limits Limits, src func(string) io.Reader) {
	t.Helper()
	session, e, err := Parse(src(text), mboxOpts(true, profile, limits))
	got := mboxOutcomeOf(t, session, e, err, 0)
	checkMboxEnvelope(t, label, session, text[:n])
	if len(got.diags) == 0 || !reflect.DeepEqual(got.diags[0], mboxNote(int64(n))) {
		t.Errorf("%s: diagnostics %+v, want header.mbox_envelope at [0, %d) first", label, got.diags, n)
		return
	}
	got.diags = got.diags[1:]
	if len(got.diags) == 0 {
		got.diags = nil
	}
	rest := limits
	if rest.MaxInputBytes > 0 {
		rest.MaxInputBytes -= int64(n)
	}
	s2, e2, err2 := Parse(src(text[n:]), mboxOpts(false, profile, rest))
	want := mboxOutcomeOf(t, s2, e2, err2, int64(n))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: parse with the envelope\n%+v\nwant the parse of what follows it, moved by %d\n%+v", label, got, n, want)
	}
}

// checkMboxLikeOff checks that a parse with the option is exactly the parse
// without it, and keeps no line.
func checkMboxLikeOff(t *testing.T, label, text string, profile Profile, limits Limits, src func(string) io.Reader) {
	t.Helper()
	s1, e1, err1 := Parse(src(text), mboxOpts(true, profile, limits))
	got := mboxOutcomeOf(t, s1, e1, err1, 0)
	checkMboxEnvelope(t, label, s1, "")
	s2, e2, err2 := Parse(src(text), mboxOpts(false, profile, limits))
	want := mboxOutcomeOf(t, s2, e2, err2, 0)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: with the option\n%+v\nwant what the parse without it gives\n%+v", label, got, want)
	}
}

var mboxProfiles = []Profile{ProfileTolerant, ProfileForensic, ProfileStrict}

func mboxWhole(s string) io.Reader { return strings.NewReader(s) }

type mboxRootCase struct {
	name   string
	text   string
	env    string
	fields []string
	body   string
	header int64 // where the header block begins
	bodyAt int64
	bom    textproto.ByteOrderMark
	// tolerant is what Tolerant and Forensic record and strict what Strict
	// records; refused is the code Strict refuses the root for, if any.
	tolerant, strict []lineEndingFacts
	refused          Code
}

func mboxRootCases() []mboxRootCase {
	note45, note44 := mboxNote(45), mboxNote(44)
	only45 := []lineEndingFacts{note45}
	return []mboxRootCase{
		{name: "crlf", text: mboxSimple, env: mboxEnv,
			fields: []string{`[] Subject="s"`}, body: "body", header: 45, bodyAt: 59,
			tolerant: only45, strict: only45},
		{name: "lf", text: mboxContent + "\nA: 1\nB: 2\n\nbody", env: mboxContent + "\n",
			fields: []string{`[] A="1"`, `[] B="2"`}, body: "body", header: 44, bodyAt: 55,
			tolerant: []lineEndingFacts{note44, {CodeHeaderBareLF, SeverityInfo, nil, 48, 49}},
			strict:   []lineEndingFacts{note44, {CodeHeaderBareLF, SeverityInfo, nil, 48, 49}}},
		{name: "cr", text: mboxContent + "\rA: 1\rB: 2\r\rbody", env: mboxContent + "\r",
			fields: []string{`[] A="1"`, `[] B="2"`}, body: "body", header: 44, bodyAt: 55,
			tolerant: []lineEndingFacts{note44, {CodeHeaderBareCR, SeverityRecovered, nil, 48, 49}},
			strict:   []lineEndingFacts{note44, {CodeHeaderBareCR, SeverityInvalidMetadata, nil, 48, 49}},
			refused:  CodeHeaderBareCR},
		// The envelope's own line ending is no line of the header block.
		{name: "crlf envelope, lf header", text: mboxEnv + "A: 1\nB: 2\n\nbody", env: mboxEnv,
			fields: []string{`[] A="1"`, `[] B="2"`}, body: "body", header: 45, bodyAt: 56,
			tolerant: []lineEndingFacts{note45, {CodeHeaderBareLF, SeverityInfo, nil, 49, 50}},
			strict:   []lineEndingFacts{note45, {CodeHeaderBareLF, SeverityInfo, nil, 49, 50}}},
		{name: "lf envelope, crlf header", text: mboxContent + "\nSubject: s\r\n\r\nbody", env: mboxContent + "\n",
			fields: []string{`[] Subject="s"`}, body: "body", header: 44, bodyAt: 58,
			tolerant: []lineEndingFacts{note44}, strict: []lineEndingFacts{note44}},
		// A byte order mark after the envelope is the header block's.
		{name: "utf-8 mark after", text: mboxEnv + bomUTF8 + "A: 1\r\n\r\nbody", env: mboxEnv,
			fields: []string{`[] A="1"`}, body: "body", header: 45, bodyAt: 56, bom: textproto.BOMUTF8,
			tolerant: []lineEndingFacts{note45, bomNote(SeverityRecovered, 45, 48)},
			strict:   []lineEndingFacts{note45, bomNote(SeverityInvalidMetadata, 45, 48)},
			refused:  CodeInputBOM},
		{name: "utf-16le mark after", text: mboxEnv + markedText(textproto.BOMUTF16LE, bomGenuine), env: mboxEnv,
			body: markedText(textproto.BOMUTF16LE, bomGenuine), header: 45, bodyAt: 45, bom: textproto.BOMUTF16LE,
			tolerant: []lineEndingFacts{note45, bomNote(SeverityUnsupported, 45, 47)},
			strict:   []lineEndingFacts{note45, bomNote(SeverityInvalidMetadata, 45, 47)},
			refused:  CodeInputBOM},
		{name: "separator", text: mboxEnv + "\r\nbody", env: mboxEnv,
			body: "body", header: 45, bodyAt: 47, tolerant: only45, strict: only45},
		{name: "envelope at eof", text: mboxEnv, env: mboxEnv,
			header: 45, bodyAt: 45, tolerant: only45, strict: only45},
		{name: "cr at eof", text: mboxContent + "\r", env: mboxContent + "\r",
			header: 44, bodyAt: 44, tolerant: []lineEndingFacts{note44}, strict: []lineEndingFacts{note44}},
		{name: "cr, then an empty line", text: mboxContent + "\r\r\nbody", env: mboxContent + "\r",
			body: "body", header: 44, bodyAt: 46, tolerant: []lineEndingFacts{note44}, strict: []lineEndingFacts{note44}},
		{name: "unterminated header", text: mboxEnv + "A: 1", env: mboxEnv,
			fields: []string{`[] A="1"`}, header: 45, bodyAt: 49,
			tolerant: []lineEndingFacts{note45, {CodeHeaderTruncatedLine, SeverityIncomplete, nil, 45, 49}},
			strict:   []lineEndingFacts{note45, {CodeHeaderTruncatedLine, SeverityInvalidMetadata, nil, 45, 49}},
			refused:  CodeHeaderTruncatedLine},
		// From lines elsewhere are content, escaped or not.
		{name: "from lines in the body",
			text: mboxSimple[:59] + "From b@example.com Thu Jan  1 00:00:00 2026\r\n>From c\r\nFrom the start\r\n",
			env:  mboxEnv, fields: []string{`[] Subject="s"`}, header: 45, bodyAt: 59,
			body:     "From b@example.com Thu Jan  1 00:00:00 2026\r\n>From c\r\nFrom the start\r\n",
			tolerant: only45, strict: only45},
		{name: "thunderbird", text: "From - Mon Jan 01 00:00:00 2001\nA: 1\n\nbody", env: "From - Mon Jan 01 00:00:00 2001\n",
			fields: []string{`[] A="1"`}, body: "body", header: 32, bodyAt: 38,
			tolerant: []lineEndingFacts{mboxNote(32), {CodeHeaderBareLF, SeverityInfo, nil, 36, 37}},
			strict:   []lineEndingFacts{mboxNote(32), {CodeHeaderBareLF, SeverityInfo, nil, 36, 37}}},
		{name: "numeric zone",
			text:   "From 1234567890123456789@xxx Thu Jan 01 00:00:00 +0000 2026\r\nA: 1\r\n\r\nbody",
			env:    "From 1234567890123456789@xxx Thu Jan 01 00:00:00 +0000 2026\r\n",
			fields: []string{`[] A="1"`}, body: "body", header: 61, bodyAt: 69,
			tolerant: []lineEndingFacts{mboxNote(61)}, strict: []lineEndingFacts{mboxNote(61)}},
	}
}

// TestParse_mboxEnvelope_root pins the root of each case under every profile,
// compatibility and delivery: the line kept, its diagnostic first, the header
// and body positions, the diagnostics of the header after it, the refusal or
// its absence, the fields and the body.
func TestParse_mboxEnvelope_root(t *testing.T) {
	if len(mboxEnv) != 45 || len(mboxSimple) != 63 {
		t.Fatalf("fixtures: %d and %d bytes, want 45 and 63", len(mboxEnv), len(mboxSimple))
	}
	for _, tc := range mboxRootCases() {
		for _, profile := range mboxProfiles {
			for _, compat := range framingCompatibilities {
				for name, src := range lineEndingSources(tc.text) {
					label := fmt.Sprintf("%s/%v/%v/%s", tc.name, profile, compat, name)
					session, e, err := Parse(src(), &ParseOptions{RecognizeMboxEnvelope: true, Profile: profile, Compatibility: compat})
					if e == nil {
						t.Fatalf("%s: Parse() = (nil, %v), want the root", label, err)
					}
					checkMboxEnvelope(t, label, session, tc.env)
					want := tc.tolerant
					if profile == ProfileStrict {
						want = tc.strict
					}
					if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
						t.Errorf("%s: diagnostics = %+v, want %+v", label, got, want)
					}
					if e.ParseStop() != nil {
						t.Errorf("%s: ParseStop() = %v, want none", label, e.ParseStop())
					}
					if got := e.parse.origin.headerOffset; got != tc.header {
						t.Errorf("%s: header offset %d, want %d", label, got, tc.header)
					}
					if got := e.parse.origin.bodyOffset; got != tc.bodyAt {
						t.Errorf("%s: body offset %d, want %d", label, got, tc.bodyAt)
					}
					if got := e.Header.ByteOrderMark(); got != tc.bom {
						t.Errorf("%s: ByteOrderMark() = %v, want %v", label, got, tc.bom)
					}
					switch {
					case profile == ProfileStrict && tc.refused != "":
						var fe FatalError
						d, ok := Diagnostic{}, false
						if errors.As(err, &fe) {
							d, ok = fe.Diagnostic()
						}
						if !ok || d.Code != tc.refused || d.Severity != SeverityFatal {
							t.Errorf("%s: err = %v, want the root refused for %s", label, err, tc.refused)
						}
					case err != nil:
						t.Errorf("%s: Parse() err = %v, want nil", label, err)
					}
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
	}
}

// TestParse_mboxEnvelope_suffix pins, for every root case, every profile and
// several deliveries, that the parse is the parse of what follows the line at
// moved positions.
func TestParse_mboxEnvelope_suffix(t *testing.T) {
	for _, tc := range mboxRootCases() {
		for _, profile := range mboxProfiles {
			for name := range lineEndingSources(tc.text) {
				srcOf := func(s string) io.Reader { return lineEndingSources(s)[name]() }
				checkMboxSuffix(t, fmt.Sprintf("%s/%v/%s", tc.name, profile, name), tc.text, len(tc.env), profile, Limits{}, srcOf)
			}
		}
	}
}

// TestParse_mboxEnvelope_off pins that without the option an envelope line is
// what it always was — a malformed header key that leaves no entity — under
// Parse with nil and zero options, Read and ReadWithOptions, and that nothing
// is kept.
func TestParse_mboxEnvelope_off(t *testing.T) {
	const want = "message: malformed MIME header key: From a@example.com Thu Jan  1 00"
	for name, opts := range map[string]*ParseOptions{
		"nil":   nil,
		"zero":  {},
		"false": {RecognizeMboxEnvelope: false, Profile: ProfileStrict},
	} {
		session, e, err := Parse(strings.NewReader(mboxSimple), opts)
		if opts == nil || opts.Profile != ProfileStrict {
			// Header recovery keeps the line apart as a damaged header line
			// (ROB-020-006); it is still no envelope.
			if e == nil || err != nil || !reflect.DeepEqual(codesOfDiags(session.Diagnostics()), []Code{CodeHeaderInvalidName}) {
				t.Errorf("%s: Parse() = (%v, %v, %v), want the healed entity and header.invalid_name", name, e != nil, err, codesOfDiags(session.Diagnostics()))
			}
			checkMboxEnvelope(t, name, session, "")
			continue
		}
		if e != nil || err == nil || err.Error() != want {
			t.Errorf("%s: Parse() = (%v, %v), want (nil, %q)", name, e != nil, err, want)
		}
		checkMboxEnvelope(t, name, session, "")
		if d := session.Diagnostics(); d != nil {
			t.Errorf("%s: diagnostics %+v, want none", name, d)
		}
	}
	for name, read := range map[string]func(io.Reader) (*Entity, error){
		"Read":            Read,
		"ReadWithOptions": func(r io.Reader) (*Entity, error) { return ReadWithOptions(r, &ReadOptions{}) },
	} {
		e, err := read(strings.NewReader(mboxSimple))
		if e != nil || err == nil || err.Error() != want {
			t.Errorf("%s = (%v, %v), want (nil, %q)", name, e != nil, err, want)
		}
	}
}

// mboxNegatives are inputs the option leaves exactly as they were: lines that
// are not envelopes, and envelopes anywhere but at offset 0.
func mboxNegatives() map[string]string {
	long := "From " + strings.Repeat("a", 969) + " Thu Jan 01 00:00:00 2026\r\n"
	part := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n" + mboxEnv + "X: 1\r\n\r\npart\r\n--B--\r\n"
	rfc822 := "Content-Type: message/rfc822\r\n\r\n" + mboxSimple
	return map[string]string{
		"from field":                   "From: a@example.com\r\nSubject: s\r\n\r\nbody",
		"from field, space before ':'": "From : a@example.com\r\n\r\nbody",
		"two spaces":                   "From  a@example.com Thu Jan  1 00:00:00 2026\r\nA: 1\r\n\r\nbody",
		"tab":                          "From\ta@example.com Thu Jan  1 00:00:00 2026\r\nA: 1\r\n\r\nbody",
		"lower case":                   "from a@example.com Thu Jan  1 00:00:00 2026\r\nA: 1\r\n\r\nbody",
		"escaped":                      ">From a@example.com Thu Jan  1 00:00:00 2026\r\nA: 1\r\n\r\nbody",
		"indented":                     " From a@example.com Thu Jan  1 00:00:00 2026\r\nA: 1\r\n\r\nbody",
		"prose with a time":            "From the 10:30 meeting\r\nA: 1\r\n\r\nbody",
		"prose":                        "From the desk of Jane\nA: 1\n\nbody",
		"no timestamp":                 "From a@example.com\r\nA: 1\r\n\r\nbody",
		"trailing space":               mboxContent + " \r\nA: 1\r\n\r\nbody",
		"two-digit year":               "From a@example.com Thu Jan  1 00:00:00 26\r\nA: 1\r\n\r\nbody",
		"8-bit sender":                 "From \xc3\xa9@example.com Thu Jan  1 00:00:00 2026\r\nA: 1\r\n\r\nbody",
		"no line ending":               mboxContent,
		"999 bytes":                    long + "A: 1\r\n\r\nbody",
		"utf-8 mark before":            bomUTF8 + mboxSimple,
		"utf-16le mark before":         markedText(textproto.BOMUTF16LE, mboxSimple),
		"envelope in a part header":    part,
		"envelope in a message body":   rfc822,
		"envelope after a header line": "Subject: s\r\n" + mboxEnv + "\r\nbody",
		"no envelope":                  "Subject: s\r\n\r\nbody",
		"empty":                        "",
	}
}

// TestParse_mboxEnvelope_negatives pins that every negative input is framed
// with the option exactly as without it, under every profile and delivery.
func TestParse_mboxEnvelope_negatives(t *testing.T) {
	for name, text := range mboxNegatives() {
		for _, profile := range mboxProfiles {
			for dname := range lineEndingSources(text) {
				src := func(s string) io.Reader { return lineEndingSources(s)[dname]() }
				checkMboxLikeOff(t, fmt.Sprintf("%s/%v/%s", name, profile, dname), text, profile, Limits{}, src)
			}
		}
	}
}

// TestParse_mboxEnvelope_onlyOne pins that a second envelope line, and one in
// the middle of the header, are header lines as before, after the first was
// kept: the parse fails on them with the malformed key it always reported.
func TestParse_mboxEnvelope_onlyOne(t *testing.T) {
	const key = "message: malformed MIME header key: From b@example.com Thu Jan  1 00"
	second := "From b@example.com Thu Jan  1 00:00:00 2026\r\n"
	for name, text := range map[string]string{
		"second envelope": mboxEnv + second + "Subject: s\r\n\r\nbody",
		"mid-header":      mboxEnv + "Subject: s\r\n" + second + "\r\nbody",
	} {
		// ProfileStrict, which heals no header line; the other profiles keep
		// such a line apart or begin the body with it (ROB-020-003).
		session, e, err := Parse(strings.NewReader(text), mboxOpts(true, ProfileStrict, Limits{}))
		if e != nil || err == nil || err.Error() != key {
			t.Errorf("%s: Parse() = (%v, %v), want (nil, %q)", name, e != nil, err, key)
		}
		checkMboxEnvelope(t, name, session, mboxEnv)
		if got, want := factsOf(session.Diagnostics()), []lineEndingFacts{mboxNote(45)}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: diagnostics %+v, want %+v", name, got, want)
		}
		checkMboxSuffix(t, name, text, 45, ProfileStrict, Limits{}, mboxWhole)
	}
}

// mboxMultipart is an envelope before a multipart of two parts, the first with
// a header ended by bare LFs and the second with one 65-byte field. The root
// header begins at 45 and its 45-byte block ends at 90; the parts' headers
// begin at 95 and 111.
var mboxMultipart = mboxEnv + "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nX: 1\n\none\r\n" +
	"--B\r\nY: " + strings.Repeat("y", 60) + "\r\n\r\ntwo\r\n--B--\r\n"

// TestParse_mboxEnvelope_parts pins the parts after an envelope: their
// positions and their diagnostics are absolute, as they always were, and a
// part budget refuses a part at its own position.
func TestParse_mboxEnvelope_parts(t *testing.T) {
	if len(mboxMultipart) != 190 {
		t.Fatalf("fixture: %d bytes, want 190", len(mboxMultipart))
	}
	lf := lineEndingFacts{CodeHeaderBareLF, SeverityInfo, []int{0}, 99, 100}
	for _, tc := range []struct {
		name   string
		limits Limits
		diags  []lineEndingFacts
	}{
		{"default", Limits{}, []lineEndingFacts{mboxNote(45), lf}},
		{"part field budget", Limits{MaxHeaderFieldBytes: 50},
			[]lineEndingFacts{mboxNote(45), lf, {CodeLimitHeaderFieldBytes, SeverityLimitExceeded, []int{1}, 111, 111}}},
		// Root 45 and the first part's 6 header bytes leave 3: the second part's
		// header is refused at its fourth byte.
		{"shared header bytes", Limits{MaxTotalHeaderBytes: 54},
			[]lineEndingFacts{mboxNote(45), lf, {CodeLimitTotalHeaderBytes, SeverityLimitExceeded, []int{1}, 114, 114}}},
		{"shared header count", Limits{MaxTotalHeaderCount: 2},
			[]lineEndingFacts{mboxNote(45), lf, {CodeLimitTotalHeaderCount, SeverityLimitExceeded, []int{1}, 111, 111}}},
	} {
		for name, src := range lineEndingSources(mboxMultipart) {
			label := fmt.Sprintf("%s/%s", tc.name, name)
			session, e, err := Parse(src(), mboxOpts(true, ProfileTolerant, tc.limits))
			if e == nil || err != nil {
				t.Fatalf("%s: Parse() = (%v, %v), want the root", label, e != nil, err)
			}
			tr := readLineEndingTree(e)
			if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, tc.diags) {
				t.Errorf("%s: diagnostics %+v, want %+v (tree %+v)", label, got, tc.diags, tr)
			}
			checkMboxEnvelope(t, label, session, mboxEnv)
			if tc.name == "default" {
				wantFields := []string{`[] Content-Type="multipart/mixed; boundary=B"`, `[0] X="1"`, `[1] Y="` + strings.Repeat("y", 60) + `"`}
				if !reflect.DeepEqual(tr.fields, wantFields) || !reflect.DeepEqual(tr.bodies, []string{`[0] "one"`, `[1] "two"`}) || tr.errs != nil {
					t.Errorf("%s: tree %+v", label, tr)
				}
			}
		}
		srcOf := func(s string) io.Reader { return strings.NewReader(s) }
		checkMboxSuffix(t, tc.name, mboxMultipart, 45, ProfileTolerant, tc.limits, srcOf)
	}
}

// TestParse_mboxEnvelope_rootBudgets pins the root header's own budgets after
// an envelope: they count the header block alone, never the line, and refuse
// it at absolute positions, while the refusal the diagnostic carries as its
// cause keeps its offsets relative to the block.
func TestParse_mboxEnvelope_rootBudgets(t *testing.T) {
	ab := mboxEnv + "A: 1\r\nB: 2\r\n\r\nbody"
	for _, tc := range []struct {
		name   string
		text   string
		limits Limits
		code   Code  // "" for no refusal
		at     int64 // where the refusal is placed
		unit   int64 // the cause's own UnitOffset
	}{
		{"header bytes at the budget", mboxSimple, Limits{MaxHeaderBytes: 14}, "", 0, 0},
		{"header bytes over", mboxSimple, Limits{MaxHeaderBytes: 13}, CodeLimitHeaderBytes, 45, 0},
		{"field bytes at the budget", mboxSimple, Limits{MaxHeaderFieldBytes: 12}, "", 0, 0},
		{"field bytes over", mboxSimple, Limits{MaxHeaderFieldBytes: 11}, CodeLimitHeaderFieldBytes, 45, 0},
		{"field count at the budget", ab, Limits{MaxHeaderCount: 2}, "", 0, 0},
		{"field count over", ab, Limits{MaxHeaderCount: 1}, CodeLimitHeaderCount, 51, 6},
		{"line bytes at the line", mboxSimple, Limits{MaxLineBytes: 43}, "", 0, 0},
		{"shared bytes at the budget", mboxSimple, Limits{MaxTotalHeaderBytes: 14}, "", 0, 0},
		{"shared bytes over", mboxSimple, Limits{MaxTotalHeaderBytes: 13}, CodeLimitTotalHeaderBytes, 58, 13},
		{"shared count at the budget", ab, Limits{MaxTotalHeaderCount: 2}, "", 0, 0},
		{"shared count over", ab, Limits{MaxTotalHeaderCount: 1}, CodeLimitTotalHeaderCount, 51, 6},
	} {
		for name, src := range lineEndingSources(tc.text) {
			label := fmt.Sprintf("%s/%s", tc.name, name)
			session, e, err := Parse(src(), mboxOpts(true, ProfileTolerant, tc.limits))
			checkMboxEnvelope(t, label, session, mboxEnv)
			if tc.code == "" {
				if e == nil || err != nil {
					t.Errorf("%s: Parse() = (%v, %v), want the root", label, e != nil, err)
				}
				if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, []lineEndingFacts{mboxNote(45)}) {
					t.Errorf("%s: diagnostics %+v, want only the envelope's", label, got)
				}
				continue
			}
			want := []lineEndingFacts{mboxNote(45), {tc.code, SeverityLimitExceeded, nil, tc.at, tc.at}}
			if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: diagnostics %+v, want %+v", label, got, want)
			}
			var fe FatalError
			if e != nil || !errors.As(err, &fe) {
				t.Fatalf("%s: Parse() = (%v, %v), want no entity and the refusal", label, e != nil, err)
			}
			d, _ := fe.Diagnostic()
			if d.Code != tc.code || d.Location != LocationAt(tc.at) {
				t.Errorf("%s: fatal diagnostic %+v, want %s at %d", label, d, tc.code, tc.at)
			}
			// The cause is the header reader's own value, unmodified: its
			// offsets are relative to where that reader began the block.
			if le, ok := textproto.HeaderLimit(d.Cause); !ok || le.Offset() != 0 || le.UnitOffset() != tc.unit {
				t.Errorf("%s: cause %v, want a refusal at block offset 0, unit %d", label, d.Cause, tc.unit)
			}
		}
		checkMboxSuffix(t, tc.name, tc.text, 45, ProfileTolerant, tc.limits, mboxWhole)
	}
}

// TestParse_mboxEnvelope_lineBudget pins Limits.MaxLineBytes as the
// recognizer's bound: a line longer than it is not recognized and is refused
// by the header reader as before; one within it is kept.
func TestParse_mboxEnvelope_lineBudget(t *testing.T) {
	for limit := int64(1); limit <= 46; limit++ {
		limits := Limits{MaxLineBytes: limit}
		label := fmt.Sprintf("MaxLineBytes %d", limit)
		if limit < 43 {
			checkMboxLikeOff(t, label, mboxSimple, ProfileTolerant, limits, mboxWhole)
			_, _, err := Parse(strings.NewReader(mboxSimple), mboxOpts(true, ProfileTolerant, limits))
			var fe FatalError
			if !errors.As(err, &fe) {
				t.Fatalf("%s: err %v, want the line refused", label, err)
			}
			if d, _ := fe.Diagnostic(); d.Code != CodeLimitLineBytes || d.Location != LocationAt(0) {
				t.Errorf("%s: fatal diagnostic %+v, want limit.line_bytes at 0", label, d)
			}
			continue
		}
		checkMboxSuffix(t, label, mboxSimple, 45, ProfileTolerant, limits, mboxWhole)
	}
}

// TestParse_mboxEnvelope_inputBudget sweeps Limits.MaxInputBytes over the
// whole input, every delivery alike: a budget that ends before the line ending
// keeps no line and is the parse without the option; one that admits it keeps
// it and is the parse of what follows it under what is left of the budget.
func TestParse_mboxEnvelope_inputBudget(t *testing.T) {
	for n := int64(1); n <= int64(len(mboxSimple))+1; n++ {
		limits := Limits{MaxInputBytes: n}
		var first *mboxOutcome
		var firstEnv string
		for name, src := range lineEndingSources(mboxSimple) {
			label := fmt.Sprintf("MaxInputBytes %d/%s", n, name)
			session, e, err := Parse(src(), mboxOpts(true, ProfileTolerant, limits))
			o := mboxOutcomeOf(t, session, e, err, 0)
			env, _ := session.MboxEnvelope()
			if first == nil {
				first, firstEnv = &o, string(env.Bytes())
			} else if !reflect.DeepEqual(o, *first) || string(env.Bytes()) != firstEnv {
				t.Errorf("%s: outcome %+v and line %q, want what every delivery gives: %+v and %q", label, o, env.Bytes(), *first, firstEnv)
			}
			srcOf := func(s string) io.Reader { return lineEndingSources(s)[name]() }
			switch {
			case n < 45:
				checkMboxLikeOff(t, label, mboxSimple, ProfileTolerant, limits, srcOf)
			case n == 45:
				// The line is admitted whole; the first byte after it is not.
				checkMboxEnvelope(t, label, session, mboxEnv)
				if e != nil || len(o.diags) != 2 || !reflect.DeepEqual(o.diags[0], mboxNote(45)) ||
					o.diags[1].code != CodeLimitInputBytes || o.diags[1].start != 45 {
					t.Errorf("%s: outcome %+v, want the line kept and limit.input_bytes at 45", label, o)
				}
			default:
				checkMboxSuffix(t, label, mboxSimple, 45, ProfileTolerant, limits, srcOf)
			}
		}
	}
}

// TestParse_mboxEnvelope_diagnosticBudgets pins the line under Limits.MaxDiagnosticCount and
// DiscardDeliveredDiagnostics: the diagnostic may go, the line stays.
func TestParse_mboxEnvelope_diagnosticBudgets(t *testing.T) {
	session, e, err := Parse(strings.NewReader(mboxSimple), mboxOpts(true, ProfileTolerant, Limits{MaxDiagnosticCount: 1}))
	if e == nil || err != nil {
		t.Fatalf("MaxDiagnosticCount 1: Parse() = (%v, %v)", e != nil, err)
	}
	checkMboxEnvelope(t, "MaxDiagnosticCount 1", session, mboxEnv)
	if d := session.Diagnostics(); len(d) != 1 || d[0].Code != CodeLimitDiagnosticCount {
		t.Errorf("MaxDiagnosticCount 1: diagnostics %+v, want the terminal limit.diagnostic_count alone", d)
	}

	var delivered []Code
	session, e, err = Parse(strings.NewReader(mboxSimple[:45]+"A: 1\n\nbody"), &ParseOptions{
		RecognizeMboxEnvelope:       true,
		DiscardDeliveredDiagnostics: true,
		DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			delivered = append(delivered, d.Code)
			return nil
		}),
	})
	if e == nil || err != nil {
		t.Fatalf("discard: Parse() = (%v, %v)", e != nil, err)
	}
	checkMboxEnvelope(t, "discard", session, mboxEnv)
	if session.Diagnostics() != nil || !reflect.DeepEqual(delivered, []Code{CodeHeaderMboxEnvelope, CodeHeaderBareLF}) {
		t.Errorf("discard: retained %+v, delivered %v, want none retained and the envelope's then the bare LF's delivered", session.Diagnostics(), delivered)
	}
}

// TestParse_mboxEnvelope_sink pins a DiagnosticSink: it reads the line while it
// is told about it, and stopping the parse there ends it with no entity and
// the line kept.
func TestParse_mboxEnvelope_sink(t *testing.T) {
	var seen []string
	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		env, ok := s.MboxEnvelope()
		seen = append(seen, fmt.Sprintf("%s %q %v", d.Code, env.Bytes(), ok))
		if strings.Contains(d.Message, "a@example.com") {
			t.Errorf("diagnostic message %q carries the line's bytes", d.Message)
		}
		return nil
	})
	_, e, err := Parse(strings.NewReader(mboxSimple), &ParseOptions{RecognizeMboxEnvelope: true, DiagnosticSink: sink})
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v)", e != nil, err)
	}
	if want := []string{fmt.Sprintf("%s %q true", CodeHeaderMboxEnvelope, mboxEnv)}; !reflect.DeepEqual(seen, want) {
		t.Errorf("sink saw %q, want %q", seen, want)
	}

	errStop := errors.New("test sink: stop at the envelope")
	// One byte per read: the recognizer reads exactly the line's 45 bytes, and
	// the stop reads not one byte of the header after it.
	touched := false
	tail := &mboxTail{s: mboxSimple, after: 45, touched: &touched}
	session, e, err := Parse(tail, &ParseOptions{
		RecognizeMboxEnvelope: true,
		DiagnosticSink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
			return errStop
		}),
	})
	if e != nil || !IsFatal(err) || !errors.Is(err, errStop) {
		t.Errorf("stopping sink: Parse() = (%v, %v), want no entity and the sink's stop", e != nil, err)
	}
	checkMboxEnvelope(t, "stopping sink", session, mboxEnv)
	if touched || tail.pos != 45 {
		t.Errorf("stopping sink: input read to offset %d (past the line: %v), want 45 and no header byte", tail.pos, touched)
	}
	if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, []lineEndingFacts{mboxNote(45)}) {
		t.Errorf("stopping sink: diagnostics %+v, want the envelope's alone", got)
	}
}

// A mboxTail delivers s one byte per read and records whether a byte at or
// after offset after was ever read.
type mboxTail struct {
	s       string
	after   int
	pos     int
	touched *bool
}

func (m *mboxTail) Read(p []byte) (int, error) {
	if m.pos >= len(m.s) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if m.pos >= m.after {
		*m.touched = true
	}
	p[0] = m.s[m.pos]
	m.pos++
	return 1, nil
}

// mboxIOFailures are the ordinary failures the source matrix is run with: none
// of them ends the input the way io.EOF itself does.
var mboxIOFailures = map[string]error{
	"io error":       errSourceIO,
	"wrapped EOF":    fmt.Errorf("test source: %w", io.EOF),
	"unexpected EOF": io.ErrUnexpectedEOF,
}

// TestParse_mboxEnvelope_sourceErrors fails the input at every byte of
// mboxSimple, sticky and resuming, with and after the bytes, in three
// chunkings. A failure before the line ending keeps no line and is returned
// with no entity, as itself; a failure after it keeps the line, and the header
// or the body after it reports the failure as itself, where it always did. The
// input is never read after its failure.
func TestParse_mboxEnvelope_sourceErrors(t *testing.T) {
	for name, fail := range mboxIOFailures {
		runMboxSourceErrors(t, name, fail)
	}
}

const mboxBufferFullEnv = "EMLKIT_MBOX_BUFFER_FULL"

// TestParse_mboxEnvelope_sourceBufferFull runs the same matrix with a source
// that fails with bufio.ErrBufferFull itself, in a subprocess with a deadline:
// a scanner that took it for a full buffer would read on for ever.
func TestParse_mboxEnvelope_sourceBufferFull(t *testing.T) {
	if os.Getenv(mboxBufferFullEnv) != "" {
		runMboxSourceErrors(t, "buffer full", bufio.ErrBufferFull)
		return
	}
	runTestInSubprocess(t, "^TestParse_mboxEnvelope_sourceBufferFull$", mboxBufferFullEnv, 120*time.Second)
}

func runMboxSourceErrors(t *testing.T, name string, fail error) {
	t.Helper()
	for cut := 0; cut <= len(mboxSimple); cut++ {
		for _, chunk := range []int{1, 3, 4096} {
			for _, withData := range []bool{false, true} {
				for _, sticky := range []bool{true, false} {
					label := fmt.Sprintf("%s/cut %d/chunk %d/with data %v/sticky %v", name, cut, chunk, withData, sticky)
					src := &ioSource{data: mboxSimple, cut: cut, chunk: chunk, err: fail, withData: withData, sticky: sticky}
					session, e, err := Parse(src, mboxOpts(true, ProfileTolerant, Limits{}))
					var want []lineEndingFacts
					if cut >= 45 {
						checkMboxEnvelope(t, label, session, mboxEnv)
						want = []lineEndingFacts{mboxNote(45)}
					} else {
						checkMboxEnvelope(t, label, session, "")
					}
					switch {
					case cut < 59:
						if e != nil || err != fail {
							t.Errorf("%s: Parse() = (%v, %v), want (nil, the source's failure)", label, e != nil, err)
						}
					default:
						if e == nil || err != nil {
							t.Fatalf("%s: Parse() = (%v, %v), want the root", label, e != nil, err)
						}
						body, berr := io.ReadAll(e.Body)
						if string(body) != mboxSimple[59:cut] || berr != fail {
							t.Errorf("%s: body (%q, %v), want (%q, the source's failure)", label, body, berr, mboxSimple[59:cut])
						}
					}
					if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
						t.Errorf("%s: diagnostics %+v, want %+v", label, got, want)
					}
					if src.readsAfter != 0 {
						t.Errorf("%s: %d reads after the failure, want none", label, src.readsAfter)
					}
				}
			}
		}
	}
}

// TestParse_mboxEnvelope_carriageReturnAtFailure pins a line ended by a '\r'
// that the input's last delivered byte is: io.EOF itself after it decides it,
// any other failure leaves it undecided, and no line is kept.
func TestParse_mboxEnvelope_carriageReturnAtFailure(t *testing.T) {
	text := mboxContent + "\rA: 1\r\rbody"
	failures := map[string]error{"EOF": io.EOF}
	for name, fail := range mboxIOFailures {
		failures[name] = fail
	}
	for name, fail := range failures {
		for _, withData := range []bool{false, true} {
			label := fmt.Sprintf("%s/with data %v", name, withData)
			src := &ioSource{data: text, cut: 44, chunk: 4096, err: fail, withData: withData, sticky: true}
			session, e, err := Parse(src, mboxOpts(true, ProfileTolerant, Limits{}))
			if fail == io.EOF {
				checkMboxEnvelope(t, label, session, mboxContent+"\r")
				if e == nil || err != nil || e.Header.Len() != 0 {
					t.Errorf("%s: Parse() = (%v, %v), want an empty root", label, e != nil, err)
				}
				continue
			}
			checkMboxEnvelope(t, label, session, "")
			if e != nil || err != fail || session.Diagnostics() != nil {
				t.Errorf("%s: Parse() = (%v, %v) with %+v, want (nil, the failure) and nothing recorded", label, e != nil, err, session.Diagnostics())
			}
		}
	}
}

// A mboxCancelSource delivers data one byte per read and cancels its parse
// during read number at, after the byte of that read.
type mboxCancelSource struct {
	data   string
	at     int
	cancel context.CancelFunc
	calls  int
}

func (s *mboxCancelSource) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > len(s.data)+16 {
		return 0, errIOCap
	}
	if s.calls > len(s.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = s.data[s.calls-1]
	if s.calls == s.at {
		s.cancel()
	}
	return 1, nil
}

// TestParse_mboxEnvelope_cancel cancels the parse during each read up to the
// one that delivers the header's last byte: a
// cancellation before the line is decided keeps no line, one seen by the read
// that decided it keeps the line and records nothing, one after it keeps the
// line and its diagnostic; the source is never read after the cancellation.
func TestParse_mboxEnvelope_cancel(t *testing.T) {
	for at := 0; at <= 59; at++ {
		label := fmt.Sprintf("cancel at read %d", at)
		ctx, cancel := context.WithCancel(context.Background())
		src := &mboxCancelSource{data: mboxSimple, at: at, cancel: cancel}
		if at == 0 {
			cancel()
		}
		session, e, err := ParseContext(ctx, src, mboxOpts(true, ProfileTolerant, Limits{}))
		if src.calls != at {
			t.Errorf("%s: %d reads, want %d", label, src.calls, at)
		}
		switch {
		case at < 45:
			checkMboxEnvelope(t, label, session, "")
		default:
			checkMboxEnvelope(t, label, session, mboxEnv)
		}
		var want []lineEndingFacts
		if at > 45 {
			want = []lineEndingFacts{mboxNote(45)}
		}
		if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: diagnostics %+v, want %+v", label, got, want)
		}
		if at < 59 && (e != nil || !IsFatal(err) || !errors.Is(err, context.Canceled)) {
			t.Errorf("%s: ParseContext() = (%v, %v), want no entity and the cancellation", label, e != nil, err)
		}
		cancel()
	}
}

// TestParse_mboxEnvelope_noProgress stalls the input at several offsets: a
// stall before the line ending keeps no line and is the parse without the
// option; one after it keeps the line and is the parse of what follows it.
func TestParse_mboxEnvelope_noProgress(t *testing.T) {
	for _, at := range []int{0, 5, 30, 44, 45, 50, 59} {
		label := fmt.Sprintf("stall at %d", at)
		if at < 45 {
			src := func(s string) io.Reader { return newZeroSource(s, 1, map[int]int{at: 100}) }
			checkMboxLikeOff(t, label, mboxSimple, ProfileTolerant, Limits{}, src)
			continue
		}
		// The same stall, at the same place of what follows the line.
		session, e, err := Parse(newZeroSource(mboxSimple, 1, map[int]int{at: 100}), mboxOpts(true, ProfileTolerant, Limits{}))
		got := mboxOutcomeOf(t, session, e, err, 0)
		checkMboxEnvelope(t, label, session, mboxEnv)
		s2, e2, err2 := Parse(newZeroSource(mboxSimple[45:], 1, map[int]int{at - 45: 100}), mboxOpts(false, ProfileTolerant, Limits{}))
		want := mboxOutcomeOf(t, s2, e2, err2, 45)
		want.diags = append([]lineEndingFacts{mboxNote(45)}, want.diags...)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: outcome\n%+v\nwant\n%+v", label, got, want)
		}
	}
}

// TestParse_mboxEnvelope_immutable pins that the line a session keeps is its
// own: changing what Bytes returned, or the caller's input buffer after the
// parse, changes nothing it returns.
func TestParse_mboxEnvelope_immutable(t *testing.T) {
	buf := []byte(mboxSimple)
	session, e, err := Parse(bytes.NewReader(buf), mboxOpts(true, ProfileTolerant, Limits{}))
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v)", e != nil, err)
	}
	env, _ := session.MboxEnvelope()
	b := env.Bytes()
	b[0] = 'X'
	for i := range buf {
		buf[i] = 'Y'
	}
	checkMboxEnvelope(t, "after changes", session, mboxEnv)
	if string(env.Bytes()) != mboxEnv {
		t.Errorf("value Bytes() = %q, want %q", env.Bytes(), mboxEnv)
	}
}

// TestParse_mboxEnvelope_concurrent reads the line from several goroutines
// while the message is read.
func TestParse_mboxEnvelope_concurrent(t *testing.T) {
	session, e, err := Parse(strings.NewReader(mboxMultipart), mboxOpts(true, ProfileTolerant, Limits{}))
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v)", e != nil, err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if env, ok := session.MboxEnvelope(); !ok || string(env.Bytes()) != mboxEnv {
					t.Errorf("concurrent MboxEnvelope() = (%q, %v)", env.Bytes(), ok)
					return
				}
				session.Diagnostics()
			}
		}()
	}
	readLineEndingTree(e)
	wg.Wait()
}

// TestParse_mboxEnvelope_writers pins that no writer writes the line: the
// header is written without it, byte for byte, and WriteTo writes the message
// alone.
func TestParse_mboxEnvelope_writers(t *testing.T) {
	const message = "MIME-Version: 1.0\r\nSubject: s\r\n\r\nbody"
	session, e, err := Parse(strings.NewReader(mboxEnv+message), mboxOpts(true, ProfileTolerant, Limits{}))
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v)", e != nil, err)
	}
	checkMboxEnvelope(t, "writers", session, mboxEnv)
	var h bytes.Buffer
	if err := textproto.WriteHeader(&h, e.Header.Header); err != nil || h.String() != message[:33] {
		t.Errorf("WriteHeader() = (%q, %v), want %q", h.String(), err, message[:33])
	}
	var w bytes.Buffer
	if err := e.WriteTo(&w); err != nil || w.String() != message {
		t.Errorf("WriteTo() = (%q, %v), want %q", w.String(), err, message)
	}
}

// mboxBase64Fixture is the known truncated base64 input of issues #34, #29
// and #12: how its decoded body ends depends on chunking, before and without
// this item. FuzzParse_mboxEnvelope never decodes a body, so it is a seed of
// that oracle like any other, with and without an envelope before it.
const mboxBase64Fixture = "Content-Type: multipart/mixed; boundary=O\n\n--O\nContent-Type: multipart/mixed; boundary=I\n\n--I\nContent-Transfer-Encoding: base64\n\n00000000000=0000000000000000"

// mboxMaxOriginal caps what mboxRootOf reads of a root's undecoded body.
// FuzzParse_mboxEnvelope's inputs are smaller, and reaching it fails the test.
const mboxMaxOriginal = 1 << 13

// A mboxRoot is what a parse produced at the root alone, positions moved by a
// shift: the error, the diagnostics recorded by then, the header's raw fields,
// mark and line endings, where its header and body begin, and the body as the
// input held it, undecoded.
type mboxRoot struct {
	entity           bool
	err              string
	diags            []lineEndingFacts
	headerAt, bodyAt int64
	fields           []framingField
	bom              textproto.ByteOrderMark
	ends             textproto.LineEndings
	original         string
	originalErr      string
}

// mboxRootOf reports the root of a parse without decoding anything or
// iterating any part: the undecoded body is read from the entity's original
// body, within mboxMaxOriginal bytes.
func mboxRootOf(t *testing.T, session *ParseSession, e *Entity, err error, shift int64) mboxRoot {
	t.Helper()
	r := mboxRoot{entity: e != nil, err: mboxErr(err, shift), headerAt: -1, bodyAt: -1}
	if e != nil {
		r.headerAt, r.bodyAt = e.parse.origin.headerOffset+shift, e.parse.origin.bodyOffset+shift
		r.fields = framingFieldsOf(e.Header.Fields())
		r.bom = e.Header.ByteOrderMark()
		r.ends = e.Header.LineEndings()
		if e.originalBody == nil {
			t.Fatalf("a parsed root has no original body")
		}
		b, rerr := io.ReadAll(io.LimitReader(e.originalBody, mboxMaxOriginal+1))
		if len(b) > mboxMaxOriginal {
			t.Fatalf("test cap: the root body has more than %d bytes", mboxMaxOriginal)
		}
		r.original, r.originalErr = string(b), mboxErr(rerr, shift)
	}
	for _, f := range factsOf(session.Diagnostics()) {
		if f.start >= 0 {
			f.start += shift
			f.end += shift
		}
		r.diags = append(r.diags, f)
	}
	return r
}

// FuzzParse_mboxEnvelope checks, for any input of at most 4 KiB, the envelope
// and the root's framing, never a decoded body or a part: a parse with the
// option either keeps no line and its root is exactly the root without the
// option, or keeps a prefix of the input that the grammar recognizes and its
// root is the root of what follows that prefix, without the option, at moved
// positions; the root's undecoded body is exactly the input from its body
// offset on; and how the input is chunked changes none of it.
func FuzzParse_mboxEnvelope(f *testing.F) {
	for _, seed := range []string{
		mboxSimple,
		mboxMultipart,
		mboxContent + "\nA: 1\nB: 2\n\nbody",
		mboxContent + "\rA: 1\rB: 2\r\rbody",
		mboxContent + "\r",
		mboxEnv + bomUTF8 + "A: 1\r\n\r\nbody",
		mboxEnv + markedText(textproto.BOMUTF16LE, bomGenuine),
		bomUTF8 + mboxSimple,
		"From - Mon Jan 01 00:00:00 2001\nA: 1\n\nbody",
		"From 1234567890123456789@xxx Thu Jan 01 00:00:00 +0000 2026\r\n\r\n",
		"From a@example.com Sat Jan  3 01:05:34 1996 remote from host\r\nA: 1\r\n\r\n",
		"From the 10:30 meeting\r\nA: 1\r\n\r\nbody",
		"From: a@example.com\r\n\r\nbody",
		"From : a@example.com\r\n\r\nbody",
		mboxEnv + mboxEnv + "\r\n",
		mboxEnv + "Subject: s\r\n\r\nFrom b Thu Jan  1 00:00:00 2026\r\n>From c\r\n",
		mboxBase64Fixture,
		mboxEnv + mboxBase64Fixture,
		mboxContent + "\n" + mboxBase64Fixture,
	} {
		for _, chunk := range []uint8{0, 3} {
			f.Add([]byte(seed), chunk, uint8(0))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, chunk, profileSel uint8) {
		if len(data) > 4096 {
			return
		}
		text := string(data)
		profile := mboxProfiles[int(profileSel)%len(mboxProfiles)]
		limits := Limits{MaxDiagnosticCount: LimitDisabled}
		n := 1 + int(chunk)%16

		s1, e1, err1 := Parse(strings.NewReader(text), mboxOpts(true, profile, limits))
		whole := mboxRootOf(t, s1, e1, err1, 0)
		env, ok := s1.MboxEnvelope()
		if whole.entity && (whole.original != text[whole.bodyAt:] || whole.originalErr != "") {
			t.Fatalf("undecoded body (%q, %s), want the input from offset %d: %q", whole.original, whole.originalErr, whole.bodyAt, text[whole.bodyAt:])
		}

		s2, e2, err2 := Parse(&chunkReader{s: text, n: n}, mboxOpts(true, profile, limits))
		if got := mboxRootOf(t, s2, e2, err2, 0); !reflect.DeepEqual(got, whole) {
			t.Fatalf("chunks of %d: %+v, want what the whole input gives: %+v", n, got, whole)
		}
		if env2, _ := s2.MboxEnvelope(); !bytes.Equal(env.Bytes(), env2.Bytes()) {
			t.Fatalf("chunks of %d: line %q, want %q", n, env2.Bytes(), env.Bytes())
		}

		if !ok {
			s3, e3, err3 := Parse(strings.NewReader(text), mboxOpts(false, profile, limits))
			if off := mboxRootOf(t, s3, e3, err3, 0); !reflect.DeepEqual(whole, off) {
				t.Fatalf("no line kept: root %+v, want the root without the option: %+v", whole, off)
			}
			return
		}
		raw := env.Bytes()
		k := len(raw)
		content := strings.TrimRight(string(raw), "\r\n")
		if !strings.HasPrefix(text, string(raw)) || k > 1000 || len(content) > maxMboxEnvelopeContent ||
			!strings.HasPrefix(content, "From ") || !mboxEnvelopeText(content[5:]) {
			t.Fatalf("line %q kept, want a prefix of the input the grammar recognizes", raw)
		}
		if strings.ContainsAny(content, "\r\n") || k-len(content) > 2 ||
			(strings.HasSuffix(string(raw), "\r") && strings.HasPrefix(text[k:], "\n")) {
			t.Fatalf("line %q kept, want it ended by its one line ending", raw)
		}
		if env.Location() != LocationRange(0, int64(k)) {
			t.Fatalf("line location %+v, want [0, %d)", env.Location(), k)
		}
		if len(whole.diags) == 0 || !reflect.DeepEqual(whole.diags[0], mboxNote(int64(k))) {
			t.Fatalf("diagnostics %+v, want header.mbox_envelope at [0, %d) first", whole.diags, k)
		}
		got := whole
		got.diags = got.diags[1:]
		if len(got.diags) == 0 {
			got.diags = nil
		}
		s3, e3, err3 := Parse(strings.NewReader(text[k:]), mboxOpts(false, profile, limits))
		if want := mboxRootOf(t, s3, e3, err3, int64(k)); !reflect.DeepEqual(got, want) {
			t.Fatalf("root with the line\n%+v\nwant the root of what follows it, moved by %d\n%+v", got, k, want)
		}
	})
}
