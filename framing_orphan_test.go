package message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Orphan continuation lines (ROB-020-002): a damaged header line — no colon or
// semicolon, an invalid name, an empty name — that continuation lines followed
// is kept with them apart from the fields as an orphan record. Every source
// here is a finite string or a call-capped source.

// orphanNote is the header.orphan_continuation Tolerant records for a block
// whose first record is rec, found in text at or after from.
func orphanNote(text, rec string, from int, path []int) lineEndingFacts {
	at := int64(from + strings.Index(text[from:], rec))
	return lineEndingFacts{CodeHeaderOrphanContinuation, SeverityRecovered, path, at, at + int64(len(rec))}
}

// recordsOfEntity returns the bytes of an entity's orphan records.
func recordsOfEntity(e *Entity) []string {
	var out []string
	for _, o := range e.Header.OrphanContinuations() {
		out = append(out, string(o.Bytes()))
	}
	return out
}

// An orphanRootCase is one root fixture and what parsing it yields under
// Tolerant: the records, the fields as readLineEndingTree reports them, the
// body, and the diagnostics in order of their offsets.
type orphanRootCase struct {
	name    string
	text    string
	records []string
	fields  []string
	body    string
	facts   []lineEndingFacts
}

func orphanRootCases() []orphanRootCase {
	sk := []string{`[] Subject="before"`, `[] X-Keep="after"`}
	note := func(text, rec string) lineEndingFacts { return orphanNote(text, rec, 0, nil) }
	cases := []orphanRootCase{
		{name: "no colon", text: "Subject: before\r\nbroken line\r\n orphan\r\n\tmore\r\nX-Keep: after\r\n\r\nBODY",
			records: []string{"broken line\r\n orphan\r\n\tmore\r\n"}, fields: sk, body: "BODY"},
		{name: "invalid name", text: "Subject: before\r\nBad Name: value\r\n orphan\r\nX-Keep: after\r\n\r\nBODY",
			records: []string{"Bad Name: value\r\n orphan\r\n"}, fields: sk, body: "BODY"},
		{name: "nul name", text: "Subject: before\r\nX\x00: value\r\n orphan\r\nX-Keep: after\r\n\r\nBODY",
			records: []string{"X\x00: value\r\n orphan\r\n"}, fields: sk, body: "BODY"},
		{name: "8-bit name", text: "Subject: before\r\nX\xff: value\r\n orphan\r\nX-Keep: after\r\n\r\nBODY",
			records: []string{"X\xff: value\r\n orphan\r\n"}, fields: sk, body: "BODY"},
		{name: "empty name", text: "Subject: before\r\n: value\r\n orphan\r\nX-Keep: after\r\n\r\nBODY",
			records: []string{": value\r\n orphan\r\n"}, fields: sk, body: "BODY"},
		{name: "empty name first", text: ": value\r\n orphan\r\nSubject: kept\r\n\r\nBODY",
			records: []string{": value\r\n orphan\r\n"}, fields: []string{`[] Subject="kept"`}, body: "BODY"},
		{name: "a colon on the continuation line only", text: "Subject: before\r\nbroken\r\n orphan: value\r\nX-Keep: after\r\n\r\nBODY",
			records: []string{"broken\r\n orphan: value\r\n"}, fields: sk, body: "BODY"},
		{name: "two records", text: "Subject: before\r\n: one\r\n orphan1\r\nX-Middle: intact\r\nbroken\r\n orphan2\r\nX-Keep: after\r\n\r\nBODY",
			records: []string{": one\r\n orphan1\r\n", "broken\r\n orphan2\r\n"},
			fields:  []string{`[] Subject="before"`, `[] X-Middle="intact"`, `[] X-Keep="after"`}, body: "BODY"},
		{name: "a continuation of white space alone", text: "Subject: before\r\nbroken\r\n \t\r\nX-Keep: after\r\n\r\nBODY",
			records: []string{"broken\r\n \t\r\n"}, fields: sk, body: "BODY"},
		{name: "a damaged line a cr joins to a line with a continuation", text: "broken\rb c\r\n orphan\r\nX: 1\r\n\r\nBODY",
			records: []string{"broken\rb c\r\n orphan\r\n"}, fields: []string{`[] X="1"`}, body: "BODY"},
	}
	for i := range cases {
		cases[i].facts = []lineEndingFacts{note(cases[i].text, cases[i].records[0])}
	}

	lf := "Subject: before\nbroken\n orphan\nX-Keep: after\n\nBODY"
	eof := "Subject: before\r\nbroken\r\n orphan"
	bom := bomUTF8 + "broken\r\n orphan\r\nX: 1\r\n\r\nbody"
	prefixed := " leading\r\nSubject: before\r\n: broken\r\n orphan\r\nX-Keep: after\r\n\r\nBODY"
	cr := "broken\r orphan\rX-Keep: after\r\rBODY"
	return append(cases,
		orphanRootCase{name: "lf", text: lf, records: []string{"broken\n orphan\n"}, fields: sk, body: "BODY",
			facts: []lineEndingFacts{{CodeHeaderBareLF, SeverityInfo, nil, 15, 16}, note(lf, "broken\n orphan\n")}},
		orphanRootCase{name: "the end of the input", text: eof, records: []string{"broken\r\n orphan"}, fields: []string{`[] Subject="before"`},
			facts: []lineEndingFacts{note(eof, "broken\r\n orphan"), truncatedNote(25, 32)}},
		orphanRootCase{name: "after a utf-8 mark", text: bom, records: []string{"broken\r\n orphan\r\n"}, fields: []string{`[] X="1"`}, body: "body",
			facts: []lineEndingFacts{bomNote(SeverityRecovered, 0, 3), note(bom, "broken\r\n orphan\r\n")}},
		orphanRootCase{name: "after a malformed prefix", text: prefixed, records: []string{": broken\r\n orphan\r\n"}, fields: sk, body: "BODY",
			facts: []lineEndingFacts{{CodeHeaderFirstLineContinuation, SeverityRecovered, nil, 0, 10}, note(prefixed, ": broken\r\n orphan\r\n")}},
		orphanRootCase{name: "cr", text: cr, records: []string{"broken\r orphan\r"}, fields: []string{`[] X-Keep="after"`}, body: "BODY",
			facts: []lineEndingFacts{note(cr, "broken\r orphan\r"), {CodeHeaderBareCR, SeverityRecovered, nil, 6, 7}}},
	)
}

// TestParse_orphanContinuationRoot pins the root of every fixture under every
// profile, compatibility and delivery: the records with their locations, the
// diagnostics with their positions, the refusal or its absence, the fields and
// the body.
func TestParse_orphanContinuationRoot(t *testing.T) {
	for _, tc := range orphanRootCases() {
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
			for _, compat := range framingCompatibilities {
				for name, src := range lineEndingSources(tc.text) {
					label := fmt.Sprintf("%s/%v/%v/%s", tc.name, profile, compat, name)
					session, e, err := Parse(src(), &ParseOptions{Profile: profile, Compatibility: compat})
					if e == nil {
						t.Fatalf("%s: Parse() = (nil, %v), want the root", label, err)
					}
					want := tc.facts
					if profile == ProfileStrict {
						want = strictFacts(want)
					}
					if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
						t.Errorf("%s: diagnostics = %+v, want %+v", label, got, want)
					}
					if e.ParseStop() != nil {
						t.Errorf("%s: ParseStop() = %v, want none", label, e.ParseStop())
					}
					if got := recordsOfEntity(e); !reflect.DeepEqual(got, tc.records) {
						t.Errorf("%s: records %q, want %q", label, got, tc.records)
					}
					for _, o := range e.Header.OrphanContinuations() {
						if s, end := o.Location(); tc.text[s:end] != string(o.Bytes()) {
							t.Errorf("%s: record %q is not the input's bytes at [%d, %d)", label, o.Bytes(), s, end)
						}
					}
					if got, wantAt := e.parse.origin.bodyOffset, int64(len(tc.text)-len(tc.body)); got != wantAt {
						t.Errorf("%s: body offset %d, want %d", label, got, wantAt)
					}
					if profile == ProfileStrict {
						checkFirstLineRefusal(t, label, e, err, want)
					} else if err != nil {
						t.Errorf("%s: Parse() err = %v, want nil", label, err)
					}
					tr := readLineEndingTree(e)
					if !reflect.DeepEqual(tr.fields, tc.fields) {
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

// TestRead_orphanContinuation pins that Read and ReadWithOptions frame every
// fixture as Parse does, with no error: they have no session and no profile.
func TestRead_orphanContinuation(t *testing.T) {
	for _, tc := range orphanRootCases() {
		for api, read := range map[string]func(io.Reader) (*Entity, error){
			"Read": Read,
			"ReadWithOptions": func(r io.Reader) (*Entity, error) {
				return ReadWithOptions(r, &ReadOptions{SkipTextAttachmentDecoding: true})
			},
		} {
			label := fmt.Sprintf("%s/%s", tc.name, api)
			e, err := read(strings.NewReader(tc.text))
			if err != nil || e == nil {
				t.Fatalf("%s: (%v, %v), want the entity and no error", label, e, err)
			}
			if got := recordsOfEntity(e); !reflect.DeepEqual(got, tc.records) {
				t.Errorf("%s: records %q, want %q", label, got, tc.records)
			}
			tr := readLineEndingTree(e)
			if !reflect.DeepEqual(tr.fields, tc.fields) {
				t.Errorf("%s: fields = %q, want %q", label, tr.fields, tc.fields)
			}
			if want := []string{fmt.Sprintf("[] %q", tc.body)}; !reflect.DeepEqual(tr.bodies, want) || tr.errs != nil {
				t.Errorf("%s: bodies = %q (%q), want %q", label, tr.bodies, tr.errs, want)
			}
		}
	}
}

// TestParse_orphanContinuationUnchanged pins what this item leaves as it was:
// folded fields, a field read with the semicolon, a '\r' that is content of a
// valid field joining an indented line to it, and damaged lines with no
// continuation — the empty name skipped and the others failing the root as
// they always did, with nothing recorded for them.
func TestParse_orphanContinuationUnchanged(t *testing.T) {
	for _, tc := range []struct {
		text   string
		fields []string
		err    string
	}{
		{"Subject: before\r\nX-Good: one\r\n two\r\nX-Keep: after\r\n\r\nBODY",
			[]string{`[] Subject="before"`, `[] X-Good="one two"`, `[] X-Keep="after"`}, ""},
		{"Subject: before\r\nX-Bad; value\r\n orphan\r\nX-Keep: after\r\n\r\nBODY",
			[]string{`[] Subject="before"`, `[] X-Bad="value orphan"`, `[] X-Keep="after"`}, ""},
		{"Subject: before\r\nX-Good: value\rbroken\r\n orphan\r\nX-Keep: after\r\n\r\nBODY",
			[]string{`[] Subject="before"`, "[] X-Good=\"value\\rbroken orphan\"", `[] X-Keep="after"`}, ""},
		{"Subject: before\r\n: value\r\nX-Keep: after\r\n\r\nBODY", []string{`[] Subject="before"`, `[] X-Keep="after"`}, ""},
		{"Subject: before\r\nbroken line\r\nX-Keep: after\r\n\r\nBODY", nil, "message: malformed MIME header line: broken line\r\n"},
		{"Subject: before\r\nBad Name: value\r\nX-Keep: after\r\n\r\nBODY", nil, "message: malformed MIME header key: Bad Name"},
	} {
		for _, profile := range []Profile{ProfileTolerant, ProfileStrict} {
			session, e, err := Parse(strings.NewReader(tc.text), &ParseOptions{Profile: profile})
			if tc.err != "" && profile != ProfileStrict {
				// Header recovery keeps such a line apart (ROB-020-003).
				if e == nil || err != nil || session.Len() != 1 {
					t.Errorf("%q/%v: (entity %v, %v, %v), want the healed entity and one diagnostic", tc.text, profile, e != nil, err, codesOfDiags(session.Diagnostics()))
				}
				continue
			}
			if tc.err != "" {
				if e != nil || err == nil || !strings.Contains(err.Error(), tc.err) || IsFatal(err) || session.Len() != 0 {
					t.Errorf("%q/%v: (entity %v, %v, %v), want no entity and the malformed line", tc.text, profile, e != nil, err, codesOfDiags(session.Diagnostics()))
				}
				continue
			}
			if e == nil || err != nil || session.Len() != 0 || e.Header.OrphanContinuations() != nil {
				t.Fatalf("%q/%v: (entity %v, %v, %v), want the entity, no record and nothing recorded", tc.text, profile, e != nil, err, codesOfDiags(session.Diagnostics()))
			}
			if tr := readLineEndingTree(e); !reflect.DeepEqual(tr.fields, tc.fields) {
				t.Errorf("%q/%v: fields %q, want %q", tc.text, profile, tr.fields, tc.fields)
			}
		}
	}
}

// TestParse_orphanContinuationIndentedMetadata pins that what a record holds is
// not interpreted and changes nothing before it: an indented multipart
// Content-Type frames no parts, an indented transfer encoding decodes nothing,
// and a parameter in a record is not added to the Content-Type before it —
// under every profile but Strict, which refuses the entity.
func TestParse_orphanContinuationIndentedMetadata(t *testing.T) {
	for _, tc := range []struct {
		text, mediaType, charset string
	}{
		{": x\r\n Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nkept\r\n--B--\r\n", "text/plain", ""},
		{"broken\r\n Content-Transfer-Encoding: base64\r\n Content-Type: text/plain; charset=koi8-r\r\n\r\nQUJD", "text/plain", ""},
		{"Content-Type: text/plain; charset=us-ascii\r\nBad Name: v\r\n charset=koi8-r\r\n boundary=B\r\n\r\nQUJD", "text/plain", "us-ascii"},
	} {
		_, e, err := Parse(strings.NewReader(tc.text), nil)
		if e == nil || err != nil {
			t.Fatalf("%q: Parse() = (%v, %v)", tc.text, e, err)
		}
		mt, params, terr := e.EffectiveContentType()
		if mt != tc.mediaType || terr != nil || (tc.charset != "" && params["charset"] != tc.charset) || params["boundary"] != "" {
			t.Errorf("%q: EffectiveContentType() = (%q, %v, %v)", tc.text, mt, params, terr)
		}
		if e.Header.Has("Content-Transfer-Encoding") || len(e.Header.Values("Content-Type")) > 1 {
			t.Errorf("%q: a record was interpreted", tc.text)
		}
		if mt == "text/plain" {
			if e.MultipartReader() != nil {
				t.Errorf("%q: a record framed parts", tc.text)
			}
			body, berr := io.ReadAll(e.Body)
			if want := tc.text[strings.Index(tc.text, "\r\n\r\n")+4:]; string(body) != want || berr != nil {
				t.Errorf("%q: body (%q, %v), want %q", tc.text, body, berr, want)
			}
		}

		_, se, serr := Parse(strings.NewReader(tc.text), &ParseOptions{Profile: ProfileStrict})
		if se == nil || !IsFatal(serr) || se.ParseStop() != nil || len(se.Header.OrphanContinuations()) != 1 {
			t.Errorf("%q: Strict (entity %v, %v), want the refused entity with its record", tc.text, se != nil, serr)
		}
	}
}

// orphanMultipart is a multipart of three parts whose middle part's header is
// damaged.
func orphanMultipart(middle string) string {
	return "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nSubject: first\r\n\r\none\r\n--B\r\n" +
		middle + "two\r\n--B\r\nSubject: last\r\n\r\nthree\r\n--B--\r\n"
}

// TestParse_orphanContinuationMiddleChild pins a middle part holding a record:
// it keeps the record, its fields and its body, and the part after it is read.
// Under Tolerant and Forensic Walk visits every part; under Strict the middle
// part is returned with its own refusal, NextPart then returns the last part,
// and Walk ends at the refused part, as for every Strict refusal.
func TestParse_orphanContinuationMiddleChild(t *testing.T) {
	for _, tc := range []struct {
		name, middle, record string
	}{
		{"no colon", "Subject: middle\r\nbroken\r\n orphan\r\nX-Keep: after\r\n\r\n", "broken\r\n orphan\r\n"},
		{"empty name", "Subject: middle\r\n: value\r\n orphan\r\nX-Keep: after\r\n\r\n", ": value\r\n orphan\r\n"},
		{"invalid name", "Subject: middle\r\nBad Name: v\r\n orphan\r\nX-Keep: after\r\n\r\n", "Bad Name: v\r\n orphan\r\n"},
	} {
		text := orphanMultipart(tc.middle)
		block := strings.Index(text, tc.middle)
		note := orphanNote(text, tc.record, block, []int{1})
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
			for _, compat := range framingCompatibilities {
				for name, src := range lineEndingSources(text) {
					label := fmt.Sprintf("%s/%v/%v/%s", tc.name, profile, compat, name)
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
							if got := recordsOfEntity(p); !reflect.DeepEqual(got, []string{tc.record}) || p.Header.Get("X-Keep") != "after" {
								t.Errorf("%s: middle part records %q, X-Keep %q", label, got, p.Header.Get("X-Keep"))
							}
							if s, end := p.Header.OrphanContinuations()[0].Location(); s != int64(len("Subject: middle\r\n")) || end != s+int64(len(tc.record)) {
								t.Errorf("%s: record at [%d, %d) of its block", label, s, end)
							}
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
					t.Errorf("%s/%v: Walk() = %v after %q, want the refusal of [1]", tc.name, profile, werr, visited)
				}
				continue
			}
			if werr != nil || !reflect.DeepEqual(visited, []string{"[]", "[0]", "[1]", "[2]"}) {
				t.Errorf("%s/%v: Walk() = %v after %q, want every part", tc.name, profile, werr, visited)
			}
		}
	}
}

// TestParse_orphanContinuationDelimiterRecovery pins that a parent's delimiter
// line right after a part header's record is never read as a line of that
// header: the damaged part is returned with its record, its fields and an empty
// body, a missing separator is recorded, and the healthy part after it — or the
// end of the multipart — follows on the next call. Before ROB-020-002 the
// damaged part was an error and the next call reached the same healthy part;
// nothing that was recoverable is lost. Every line ending, opening and closing
// delimiters, every profile and three chunkings are covered. Strict refuses the
// damaged part, and a bare CR around the next delimiter then refuses the rest
// of the multipart, as it always did.
func TestParse_orphanContinuationDelimiterRecovery(t *testing.T) {
	allowed := map[Code]bool{
		CodeHeaderOrphanContinuation: true, CodeHeaderMissingBodySeparator: true,
		CodeMultipartBareLF: true, CodeMultipartBareCR: true, CodeMultipartMixedLineEndings: true,
	}
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	const header = "Subject: middle\r\nbroken\r\n orphan\r\n"
	for _, eol := range []string{"\r\n", "\n", "\r"} {
		for _, closing := range []bool{false, true} {
			for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
				for _, chunk := range []int{1, 7, 4096} {
					text := root + "--B\r\n" + header
					if closing {
						text += "--B--" + eol
					} else {
						text += "--B" + eol + "Subject: healthy\r\n\r\nKEPT\r\n--B--\r\n"
					}
					label := fmt.Sprintf("%q/close %v/%v/chunk %d", eol, closing, profile, chunk)
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
					h := int64(len(root) + len("--B\r\n"))
					rec := orphanNote(text, "broken\r\n orphan\r\n", int(h), []int{0})
					sep := lineEndingFacts{CodeHeaderMissingBodySeparator, SeverityRecovered, []int{0}, h + int64(len(header)), h + int64(len(header))}
					if profile == ProfileStrict {
						rec.severity, sep.severity = SeverityInvalidMetadata, SeverityInvalidMetadata
						checkFirstLineRefusal(t, label, p, err, []lineEndingFacts{rec, sep})
					} else if err != nil {
						t.Errorf("%s: damaged part err = %v", label, err)
					}
					if body, berr := io.ReadAll(p.Body); len(body) != 0 || berr != nil || p.Header.Get("Subject") != "middle" ||
						!reflect.DeepEqual(recordsOfEntity(p), []string{"broken\r\n orphan\r\n"}) {
						t.Errorf("%s: damaged part body (%q, %v), Subject %q, records %q", label, body, berr, p.Header.Get("Subject"), recordsOfEntity(p))
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
						if f.code == CodeHeaderOrphanContinuation || f.code == CodeHeaderMissingBodySeparator {
							seen = append(seen, f)
						}
					}
					if !reflect.DeepEqual(seen, []lineEndingFacts{rec, sep}) {
						t.Errorf("%s: record diagnostics %+v, want %+v", label, seen, []lineEndingFacts{rec, sep})
					}
				}
			}
		}
	}
}

// TestParse_orphanContinuationNested pins a record in the header of a part of
// a nested multipart: its diagnostic's path and offsets are the part's in the
// whole input, and the parts around it are read.
func TestParse_orphanContinuationNested(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=A\r\n\r\n--A\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nbroken\r\n o\r\nX: 1\r\n\r\ninner\r\n--B\r\nY: 2\r\n\r\nsecond\r\n--B--\r\n--A\r\nZ: 3\r\n\r\nouter\r\n--A--\r\n"
	for _, profile := range []Profile{ProfileTolerant, ProfileStrict} {
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
		if err != nil {
			t.Fatal(err)
		}
		tr := readLineEndingTree(e)
		wantFields := []string{`[] Content-Type="multipart/mixed; boundary=A"`, `[0] Content-Type="multipart/mixed; boundary=B"`,
			`[0 0] X="1"`, `[0 1] Y="2"`, `[1] Z="3"`}
		wantBodies := []string{`[0 0] "inner"`, `[0 1] "second"`, `[1] "outer"`}
		if !reflect.DeepEqual(tr.fields, wantFields) || !reflect.DeepEqual(tr.bodies, wantBodies) {
			t.Errorf("%v: tree (%q, %q, %q)", profile, tr.fields, tr.bodies, tr.errs)
		}
		note := orphanNote(text, "broken\r\n o\r\n", 0, []int{0, 0})
		want := []lineEndingFacts{note}
		if profile == ProfileStrict {
			want = strictFacts(want)
			if len(tr.errs) != 1 || !strings.HasPrefix(tr.errs[0], "[0 0] ") {
				t.Errorf("%v: errors %q, want the refusal of [0 0] alone", profile, tr.errs)
			}
		} else if tr.errs != nil {
			t.Errorf("%v: errors %q", profile, tr.errs)
		}
		if got := factsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
			t.Errorf("%v: diagnostics %+v, want %+v", profile, got, want)
		}
	}
}

// TestParse_orphanContinuationMbox pins the positions of a record after an
// opt-in mbox envelope line: the record's location is its block's, and its
// diagnostic's offsets are the input's.
func TestParse_orphanContinuationMbox(t *testing.T) {
	text := mboxEnv + "Subject: s\r\nbroken\r\n o\r\n\r\nbody"
	session, e, err := Parse(strings.NewReader(text), mboxOpts(true, ProfileTolerant, Limits{}))
	if err != nil || e == nil {
		t.Fatalf("Parse() = (%v, %v)", e, err)
	}
	if s, end := e.Header.OrphanContinuations()[0].Location(); s != int64(len("Subject: s\r\n")) || end != s+int64(len("broken\r\n o\r\n")) {
		t.Errorf("record at [%d, %d) of its block", s, end)
	}
	want := orphanNote(text, "broken\r\n o\r\n", 0, nil)
	var got []lineEndingFacts
	for _, f := range factsOf(session.Diagnostics()) {
		if f.code == CodeHeaderOrphanContinuation {
			got = append(got, f)
		}
	}
	if !reflect.DeepEqual(got, []lineEndingFacts{want}) {
		t.Errorf("diagnostics %+v, want %+v", got, want)
	}
	if body, _ := io.ReadAll(e.Body); string(body) != "body" {
		t.Errorf("body %q", body)
	}
}

// TestParse_orphanContinuationStrictDiagnosticBudget pins that every part
// Strict refuses for its record carries its own copy of the diagnostic,
// whatever Limits.MaxDiagnosticCount let the session keep, and that the
// refusals stop nothing.
func TestParse_orphanContinuationStrictDiagnosticBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	var at []int64
	for i := range 3 {
		b.WriteString("--B\r\n")
		at = append(at, int64(b.Len()))
		fmt.Fprintf(&b, "broken %d\r\n o\r\nX-N: %d\r\n\r\nbody %d\r\n", i, i, i)
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
			if d, ok := fe.Diagnostic(); !ok || d.Code != CodeHeaderOrphanContinuation || d.Location.Offset != at[i] || !reflect.DeepEqual(d.Path, []int{i}) {
				t.Errorf("max %d: part %d refusal = %+v, want header.orphan_continuation of [%d] at %d", maxDiags, i, d, i, at[i])
			}
			if p.Header.Get("X-N") != fmt.Sprint(i) || len(p.Header.OrphanContinuations()) != 1 {
				t.Errorf("max %d: part %d lost its field or its record", maxDiags, i)
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

// TestParse_orphanContinuationSinks pins a sink stopping the parse on the
// record's diagnostic, ahead of a Strict refusal; a sink that reads the
// session while it is told, which must not deadlock; and diagnostics delivered
// and discarded.
func TestParse_orphanContinuationSinks(t *testing.T) {
	const text = "A: 1\r\nbroken\r\n o\r\n\r\nbody"
	errSink := errors.New("sink: enough")
	for _, profile := range []Profile{ProfileTolerant, ProfileStrict} {
		sink := DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return errSink })
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile, DiagnosticSink: sink})
		if e == nil || !errors.Is(err, errSink) || session.fatalErr() == nil || error(session.fatalErr()) != err {
			t.Errorf("%v: Parse() = (%v, %v), want the entity and the session's stop", profile, e != nil, err)
		}
		if e != nil && !reflect.DeepEqual(recordsOfEntity(e), []string{"broken\r\n o\r\n"}) {
			t.Errorf("%v: the stopped entity lost its record", profile)
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
		if !reflect.DeepEqual(got, []Code{CodeHeaderOrphanContinuation}) {
			t.Errorf("discard %v: delivered %v", discard, got)
		}
		if n := len(session.Diagnostics()); (n == 0) != discard {
			t.Errorf("discard %v: %d retained", discard, n)
		}
	}
}

// TestParse_orphanContinuationBudgets pins what the header budgets count: a
// record is one field candidate, its lines and bytes charged as they are read;
// a refused header leaves no entity and records the refusal alone.
func TestParse_orphanContinuationBudgets(t *testing.T) {
	// A: 1 at 0, the record at 6 (17 bytes, lines of 6 and 7), B: 2 at 23, the
	// empty line at 29: a block of 31 bytes and 3 candidates.
	const text = "A: 1\r\nbroken\r\n orphan\r\nB: 2\r\n\r\nbody"
	const record = "broken\r\n orphan\r\n"
	exact := Limits{
		MaxHeaderBytes: 31, MaxLineBytes: 7, MaxHeaderFieldBytes: 17, MaxHeaderCount: 3,
		MaxTotalHeaderBytes: 31, MaxTotalHeaderCount: 3, MaxInputBytes: int64(len(text)),
	}
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: exact})
	if err != nil || e == nil || e.Header.Get("A") != "1" || e.Header.Get("B") != "2" {
		t.Fatalf("exact budgets: (%v, %v)", e, err)
	}
	if !reflect.DeepEqual(recordsOfEntity(e), []string{record}) {
		t.Errorf("exact budgets: records %q", recordsOfEntity(e))
	}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeHeaderOrphanContinuation}) {
		t.Errorf("exact budgets: diagnostics %v", codes)
	}
	for _, tc := range []struct {
		name   string
		limits Limits
		code   Code
		offset int64
	}{
		{"header bytes", Limits{MaxHeaderBytes: 30}, CodeLimitHeaderBytes, 0},
		{"line bytes", Limits{MaxLineBytes: 6}, CodeLimitLineBytes, 14},
		{"field bytes", Limits{MaxHeaderFieldBytes: 16}, CodeLimitHeaderFieldBytes, 6},
		{"field count", Limits{MaxHeaderCount: 2}, CodeLimitHeaderCount, 23},
		{"total header bytes", Limits{MaxTotalHeaderBytes: 30}, CodeLimitTotalHeaderBytes, 30},
		{"total header count", Limits{MaxTotalHeaderCount: 2}, CodeLimitTotalHeaderCount, 23},
	} {
		s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: tc.limits})
		checkFramingRefusal(t, tc.name, s, e, err, tc.code, tc.offset)
	}

	// MaxInputBytes before, at and past the header: no entity until the header
	// is whole, then the entity with its record.
	for n := int64(1); n <= int64(len(text)); n++ {
		s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: Limits{MaxInputBytes: n}})
		if e == nil {
			if err == nil || !IsFatal(err) || error(s.fatalErr()) != err {
				t.Errorf("input %d: (nil, %v), want the input budget's stop", n, err)
			}
			continue
		}
		if !reflect.DeepEqual(recordsOfEntity(e), []string{record}) {
			t.Errorf("input %d: records %q", n, recordsOfEntity(e))
		}
	}
}

// TestParse_orphanContinuationSharedCount pins the shared field count across a
// part whose header holds a record: the record is one candidate, refused at its
// first byte once the count is spent, and the field after it at its own.
func TestParse_orphanContinuationSharedCount(t *testing.T) {
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	const part = "broken\r\n o\r\nA: 1\r\n\r\n"
	text := root + "--B\r\n" + part + "body\r\n--B--\r\n"
	h := int64(len(root) + len("--B\r\n"))
	for _, tc := range []struct {
		limit int64
		at    int64 // -1: the part is read
	}{
		{1, h},
		{2, h + int64(len("broken\r\n o\r\n"))},
		{3, -1},
	} {
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: totalsOnly("count", tc.limit)})
		if err != nil {
			t.Fatal(err)
		}
		p, err := e.MultipartReader().NextPart()
		if tc.at < 0 {
			if err != nil || p == nil || p.Header.Get("A") != "1" || len(p.Header.OrphanContinuations()) != 1 {
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

// TestParse_orphanContinuationInputErrors cuts the input at every byte: a
// failure inside the header leaves no entity and is returned as itself with
// nothing recorded — never the damaged line's own error, since the failure is
// what kept its continuation from being seen; once the header is whole the
// entity is returned with its record, and its body ends with the identical
// failure after the bytes before it. The input is never read after its
// failure.
func TestParse_orphanContinuationInputErrors(t *testing.T) {
	const block = "A: 1\r\nbroken\r\n orphan\r\n z\r\nB: 2\r\n\r\n"
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
						if !reflect.DeepEqual(recordsOfEntity(e), []string{"broken\r\n orphan\r\n z\r\n"}) {
							t.Errorf("%s: records %q", name, recordsOfEntity(e))
						}
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

// TestParseContext_orphanContinuationCancel cancels the parse at every byte of
// the header, delivered one byte at a time, the cancelling read returning its
// byte with nil, io.EOF or an error: before the header is whole no entity is
// returned and the stop is; once it is, the entity is returned with its
// record, with the stop or without it. Nothing is read after the cancelling
// read.
func TestParseContext_orphanContinuationCancel(t *testing.T) {
	const block = "A: 1\r\nbroken\r\n orphan\r\n z\r\nB: 2\r\n\r\n"
	const text = block + "body"
	const record = "broken\r\n orphan\r\n z\r\n"
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
				if !reflect.DeepEqual(recordsOfEntity(e), []string{record}) {
					t.Errorf("%s: records %q", name, recordsOfEntity(e))
				}
			}
			if src.after != 0 {
				t.Errorf("%s: %d reads after the cancelling read", name, src.after)
			}
		}
	}

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
		if !reflect.DeepEqual(recordsOfEntity(e), []string{record}) {
			t.Errorf("%s: records %q", name, recordsOfEntity(e))
		}
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

// TestParse_orphanContinuationNoProgress stalls the input inside the header:
// one empty read fewer than the limit changes nothing, the limit stops the
// parse there with no entity.
func TestParse_orphanContinuationNoProgress(t *testing.T) {
	const block = "A: 1\r\nbroken\r\n orphan\r\n z\r\nB: 2\r\n\r\n"
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
					if !reflect.DeepEqual(recordsOfEntity(e), []string{"broken\r\n orphan\r\n z\r\n"}) {
						t.Errorf("%s: records %q", name, recordsOfEntity(e))
					}
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

// TestEntity_orphanContinuationWrite pins the writers: each record is written
// in its place among the fields read with it, after the MIME-Version WriteTo
// adds, a complete CRLF header that already has one byte for byte; the
// caller's header is not changed; the output parses back to the same records,
// fields and body, and is written the same again. A part built by NewMultipart
// from a header with a record, and a header given to CreateWriter, are written
// with it.
func TestEntity_orphanContinuationWrite(t *testing.T) {
	for _, tc := range []struct {
		in     string
		mutate func(*Header)
		want   string
	}{
		{"Mime-Version: 1.0\r\nA: 1\r\nbroken\r\n o\r\nB: 2\r\n\r\nbody", nil, "Mime-Version: 1.0\r\nA: 1\r\nbroken\r\n o\r\nB: 2\r\n\r\nbody"},
		{"broken\r\n o\r\nX-Keep: yes\r\n\r\nbody", nil, "Mime-Version: 1.0\r\nbroken\r\n o\r\nX-Keep: yes\r\n\r\nbody"},
		{"A: 1\nbroken\n o\n\nbody", nil, "Mime-Version: 1.0\r\nA: 1\r\nbroken\n o\n\r\nbody"},
		{"A: 1\r\nbroken\r\n o", nil, "Mime-Version: 1.0\r\nA: 1\r\nbroken\r\n o\r\n\r\n"},
		{"A: 1\r\nbroken\r\n o\r\nB: 2\r\n\r\nbody", func(h *Header) { h.Set("Subject", "new") },
			"Mime-Version: 1.0\r\nSubject: new\r\nA: 1\r\nbroken\r\n o\r\nB: 2\r\n\r\nbody"},
		{"A: 1\r\nbroken\r\n o\r\nB: 2\r\n\r\nbody", func(h *Header) { h.Del("A") }, "Mime-Version: 1.0\r\nbroken\r\n o\r\nB: 2\r\n\r\nbody"},
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
			!reflect.DeepEqual(recordsOfEntity(e), recordsOfEntity(&Entity{Header: before})) {
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

	_, src, err := Parse(strings.NewReader("broken\r\n o\r\nSubject: middle\r\n\r\ntwo"), nil)
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
	if !strings.Contains(out.String(), "--X\r\nbroken\r\n o\r\nSubject: middle\r\n") {
		t.Errorf("NewMultipart wrote %q, want the part's record in its place", out.String())
	}
	_, e, err := Parse(bytes.NewReader(out.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.MultipartReader().NextPart()
	if err != nil || p.Header.Get("Subject") != "middle" || !reflect.DeepEqual(recordsOfEntity(p), []string{"broken\r\n o\r\n"}) {
		t.Fatalf("reparse: (%v, %v)", p, err)
	}
	if body, _ := io.ReadAll(p.Body); string(body) != "two" {
		t.Errorf("reparse: body %q", body)
	}

	_, ce, err := Parse(strings.NewReader("A: 1\r\nbroken\r\n o\r\n\r\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w, err := CreateWriter(&out, ce.Header)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if !strings.Contains(out.String(), "A: 1\r\nbroken\r\n o\r\n\r\n") || ce.Header.Has("Mime-Version") {
		t.Errorf("CreateWriter wrote %q, want the record after A and the caller's header unchanged", out.String())
	}
}

// orphanOutcome is what one root parse yields of the header framing alone:
// whether there is an entity, the error's text, the prefix, the records with
// their locations, the fields' raw bytes and where the body begins.
type orphanOutcome struct {
	entity  bool
	err     string
	prefix  string
	records []string
	fields  []string
	bodyAt  int64
}

func orphanOutcomeOf(t *testing.T, src io.Reader, profile Profile) orphanOutcome {
	t.Helper()
	limits := Limits{MaxHeaderCount: LimitDisabled, MaxTotalHeaderCount: LimitDisabled}
	_, e, err := Parse(src, &ParseOptions{Profile: profile, Limits: limits})
	var o orphanOutcome
	if err != nil {
		o.err = err.Error()
	}
	if e == nil {
		return o
	}
	o.entity = true
	o.prefix = string(e.Header.MalformedPrefix().Bytes())
	for _, r := range e.Header.OrphanContinuations() {
		s, end := r.Location()
		o.records = append(o.records, fmt.Sprintf("%d:%d:%d:%q", s, end, r.Lines(), r.Bytes()))
	}
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

// FuzzParse_orphanContinuation holds the root framing of an input of at most 4
// KiB to what the text says, with nothing decoded and no body read:
//
//   - The outcome is the same whole and read one or seven bytes at a time.
//   - Every record is the input's own bytes at its location, has at least two
//     lines, begins with a byte that is not white space and is followed, in
//     the input, by a byte that does not begin a continuation line.
//   - Strict returns an entity exactly when Tolerant does, with the same
//     records and fields.
//   - The header written and read back has the records that were written.
func FuzzParse_orphanContinuation(f *testing.F) {
	for _, tc := range orphanRootCases() {
		f.Add([]byte(tc.text))
	}
	f.Add([]byte("A: 1\r\nbroken\r\n y\rz w\r\n\tq\r\nB: 2\r\n\r\nbody"))
	f.Add([]byte(":x\rb\r\n y\r\n;q\r\n z\r\n\r\n"))
	f.Add([]byte("X-Test; value\r\n folded: value\r\nbad key; v\r\n w\r\n\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || len(data) > 4096 {
			return
		}
		text := string(data)
		want := orphanOutcomeOf(t, strings.NewReader(text), ProfileTolerant)
		for _, n := range []int{1, 7} {
			if got := orphanOutcomeOf(t, &chunkReader{s: text, n: n}, ProfileTolerant); !reflect.DeepEqual(got, want) {
				t.Fatalf("chunks of %d:\n got %+v\nwant %+v", n, got, want)
			}
		}
		if !want.entity {
			return
		}
		_, e, _ := Parse(strings.NewReader(text), &ParseOptions{Limits: Limits{MaxHeaderCount: LimitDisabled, MaxTotalHeaderCount: LimitDisabled}})
		for _, r := range e.Header.OrphanContinuations() {
			s, end := r.Location()
			b := r.Bytes()
			if s < 0 || end > int64(len(text)) || text[s:end] != string(b) || r.Lines() < 2 {
				t.Fatalf("record %q at [%d, %d), %d lines, is not the input's", b, s, end, r.Lines())
			}
			if b[0] == ' ' || b[0] == '\t' {
				t.Fatalf("record %q begins with white space", b)
			}
			if end < int64(len(text)) && (text[end] == ' ' || text[end] == '\t') {
				t.Fatalf("record %q is followed by a continuation line", b)
			}
		}
		strict := orphanOutcomeOf(t, strings.NewReader(text), ProfileStrict)
		if strict.entity != want.entity || !reflect.DeepEqual(strict.records, want.records) || !reflect.DeepEqual(strict.fields, want.fields) {
			t.Fatalf("Strict %+v, Tolerant %+v", strict, want)
		}
		var out bytes.Buffer
		if err := textproto.WriteHeader(&out, e.Header.Header); err != nil {
			return
		}
		_, again, err := Parse(bytes.NewReader(out.Bytes()), &ParseOptions{Limits: Limits{MaxHeaderCount: LimitDisabled, MaxTotalHeaderCount: LimitDisabled}})
		if err != nil || again == nil {
			return
		}
		var written []string
		for _, r := range e.Header.OrphanContinuations() {
			written = append(written, writtenRawLines(string(r.Bytes())))
		}
		if got := recordsOfEntity(again); !reflect.DeepEqual(got, written) {
			t.Fatalf("read back, the records are %q, want %q", got, written)
		}
	})
}

// writtenRawLines is what textproto.WriteHeader writes of a record b: b, and a
// line ending after a last line that has none or ends with a bare '\r'.
func writtenRawLines(b string) string {
	switch {
	case b == "" || strings.HasSuffix(b, "\n"):
		return b
	case strings.HasSuffix(b, "\r"):
		return b + "\n"
	}
	return b + "\r\n"
}
