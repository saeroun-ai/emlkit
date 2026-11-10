package textproto

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Orphan continuation lines (ROB-020-002). Every source here is a finite string
// or a call-capped failing source; nothing is read past its cap, and the one
// probe whose failure could hang runs in a subprocess with a deadline.

// A wantRecord is an orphan record expected of a block: its exact bytes, which
// the test finds in the block to know where it lies, and its physical lines.
type wantRecord struct {
	bytes string
	lines int
}

// recordOutcome is an orphan record as read.
type recordOutcome struct {
	bytes      string
	start, end int64
	lines      int
}

func recordsOf(h *Header) []recordOutcome {
	var out []recordOutcome
	for _, o := range h.OrphanContinuations() {
		start, end := o.Location()
		out = append(out, recordOutcome{bytes: string(o.Bytes()), start: start, end: end, lines: o.Lines()})
	}
	return out
}

// wantRecordsIn returns the outcomes want describes in the block in, each found
// at its first occurrence after the one before.
func wantRecordsIn(t *testing.T, in string, want []wantRecord) []recordOutcome {
	t.Helper()
	var out []recordOutcome
	from := 0
	for _, w := range want {
		i := strings.Index(in[from:], w.bytes)
		if i < 0 {
			t.Fatalf("record %q is not in %q", w.bytes, in)
		}
		start := int64(from + i)
		out = append(out, recordOutcome{bytes: w.bytes, start: start, end: start + int64(len(w.bytes)), lines: w.lines})
		from += i + len(w.bytes)
	}
	return out
}

// An orphanCase is one block and what reading it must yield: the prefix (empty
// for none), the fields, the orphan records and — when no error is expected —
// what is left in the reader. err is the whole error expected, or empty.
type orphanCase struct {
	name    string
	in      string
	prefix  string
	fields  []wantField
	records []wantRecord
	rest    string
	err     string
}

func orphanCases() []orphanCase {
	subject := wantField{"Subject", "before", "Subject: before\r\n"}
	keep := wantField{"X-Keep", "after", "X-Keep: after\r\n"}
	x := wantField{"X", "1", "X: 1\r\n"}
	around := func(damaged string) string {
		return "Subject: before\r\n" + damaged + "X-Keep: after\r\n\r\nBODY"
	}
	sk := []wantField{subject, keep}
	return []orphanCase{
		// Every damaged head, with continuation lines read whole.
		{name: "no colon", in: around("broken line\r\n orphan\r\n\tmore\r\n"), fields: sk,
			records: []wantRecord{{"broken line\r\n orphan\r\n\tmore\r\n", 3}}, rest: "BODY"},
		{name: "white space in the name", in: around("Bad Name: value\r\n orphan\r\n"), fields: sk,
			records: []wantRecord{{"Bad Name: value\r\n orphan\r\n", 2}}, rest: "BODY"},
		{name: "nul in the name", in: around("X\x00: value\r\n orphan\r\n"), fields: sk,
			records: []wantRecord{{"X\x00: value\r\n orphan\r\n", 2}}, rest: "BODY"},
		{name: "8-bit name", in: around("X\xff: value\r\n orphan\r\n"), fields: sk,
			records: []wantRecord{{"X\xff: value\r\n orphan\r\n", 2}}, rest: "BODY"},
		{name: "control byte in the name", in: around("X\x01: value\r\n orphan\r\n"), fields: sk,
			records: []wantRecord{{"X\x01: value\r\n orphan\r\n", 2}}, rest: "BODY"},
		{name: "empty name", in: around(": value\r\n orphan\r\n"), fields: sk,
			records: []wantRecord{{": value\r\n orphan\r\n", 2}}, rest: "BODY"},
		{name: "empty name before a semicolon", in: around("; value\r\n orphan\r\n"), fields: sk,
			records: []wantRecord{{"; value\r\n orphan\r\n", 2}}, rest: "BODY"},
		{name: "empty name first", in: ": value\r\n orphan\r\nSubject: kept\r\n\r\nBODY",
			fields:  []wantField{{"Subject", "kept", "Subject: kept\r\n"}},
			records: []wantRecord{{": value\r\n orphan\r\n", 2}}, rest: "BODY"},
		{name: "a colon on the continuation line only", in: around("broken\r\n orphan: value\r\n"), fields: sk,
			records: []wantRecord{{"broken\r\n orphan: value\r\n", 2}}, rest: "BODY"},
		{name: "a semicolon, then a colon on the continuation line", in: around("X-Test; value\r\n folded: value\r\n"), fields: sk,
			records: []wantRecord{{"X-Test; value\r\n folded: value\r\n", 2}}, rest: "BODY"},
		{name: "an empty name, then a colon on the continuation line", in: around(": value\r\n folded: value\r\n"), fields: sk,
			records: []wantRecord{{": value\r\n folded: value\r\n", 2}}, rest: "BODY"},
		{name: "an invalid name before a semicolon", in: around("Bad key; value\r\n folded\r\n"), fields: sk,
			records: []wantRecord{{"Bad key; value\r\n folded\r\n", 2}}, rest: "BODY"},
		{name: "a continuation of white space alone", in: around("broken\r\n \t\r\n"), fields: sk,
			records: []wantRecord{{"broken\r\n \t\r\n", 2}}, rest: "BODY"},
		{name: "an indented content transfer encoding", in: around("broken\r\n Content-Transfer-Encoding: base64\r\n"), fields: sk,
			records: []wantRecord{{"broken\r\n Content-Transfer-Encoding: base64\r\n", 2}}, rest: "BODY"},
		{name: "an indented content type", in: around(": x\r\n Content-Type: multipart/mixed; boundary=B\r\n"), fields: sk,
			records: []wantRecord{{": x\r\n Content-Type: multipart/mixed; boundary=B\r\n", 2}}, rest: "BODY"},
		{name: "after a content type", in: "Content-Type: multipart/mixed\r\n: x\r\n boundary=B\r\n\r\n",
			fields:  []wantField{{"Content-Type", "multipart/mixed", "Content-Type: multipart/mixed\r\n"}},
			records: []wantRecord{{": x\r\n boundary=B\r\n", 2}}},
		{name: "nul, 8-bit and control bytes in the continuation", in: "broken\r\n \x00\xff\x01\r\nX: 1\r\n\r\n", fields: []wantField{x},
			records: []wantRecord{{"broken\r\n \x00\xff\x01\r\n", 2}}},

		// The end of the input.
		{name: "the continuation at the end of the input", in: "Subject: before\r\nbroken\r\n orphan", fields: []wantField{subject},
			records: []wantRecord{{"broken\r\n orphan", 2}}},
		{name: "an empty name's continuation at the end of the input", in: "Subject: before\r\n: value\r\n orphan", fields: []wantField{subject},
			records: []wantRecord{{": value\r\n orphan", 2}}},
		{name: "a cr at the end of the input", in: "Subject: before\r\nbroken\r\n orphan\r", fields: []wantField{subject},
			records: []wantRecord{{"broken\r\n orphan\r", 2}}},

		// Line endings.
		{name: "lf", in: "Subject: before\nbroken\n orphan\nX-Keep: after\n\nBODY", fields: sk,
			records: []wantRecord{{"broken\n orphan\n", 2}}, rest: "BODY"},
		{name: "cr", in: "broken\r orphan\rX-Keep: after\r\rBODY", fields: []wantField{keep},
			records: []wantRecord{{"broken\r orphan\r", 2}}, rest: "BODY"},
		{name: "mixed", in: "broken\r\n a\n\tb\rX: 1\r\n\r\n", fields: []wantField{x},
			records: []wantRecord{{"broken\r\n a\n\tb\r", 3}}},

		// A '\r' that proves to be content (ROB-010-002).
		{name: "a damaged line a cr joins to a line with a continuation", in: "broken\rb c\r\n orphan\r\nX: 1\r\n\r\n", fields: []wantField{x},
			records: []wantRecord{{"broken\rb c\r\n orphan\r\n", 2}}},
		{name: "an empty name a cr joins to a line with a continuation", in: ":x\rb\r\n y\r\nZ: 1\r\n\r\n",
			fields:  []wantField{{"Z", "1", "Z: 1\r\n"}},
			records: []wantRecord{{":x\rb\r\n y\r\n", 2}}},
		{name: "a record's cr joins the next line", in: "broken\r\n y\rz w\r\nX: 1\r\n\r\n", fields: []wantField{x},
			records: []wantRecord{{"broken\r\n y\rz w\r\n", 2}}},
		{name: "a record's cr before a field", in: "broken\r\n y\rZ: 1\r\n\r\n",
			fields:  []wantField{{"Z", "1", "Z: 1\r\n"}},
			records: []wantRecord{{"broken\r\n y\r", 2}}},
		{name: "a record's cr joins a line with its own continuation", in: "broken\r\n y\rz\r\n w\r\nX: 1\r\n\r\n", fields: []wantField{x},
			records: []wantRecord{{"broken\r\n y\rz\r\n w\r\n", 3}}},

		// Marks, prefixes, several records.
		{name: "after a utf-8 mark", in: utf8Mark + "broken\r\n orphan\r\nX: 1\r\n\r\n", fields: []wantField{x},
			records: []wantRecord{{"broken\r\n orphan\r\n", 2}}},
		{name: "after a malformed prefix and a field", in: " leading\r\n" + around(": broken\r\n orphan\r\n"), prefix: " leading\r\n", fields: sk,
			records: []wantRecord{{": broken\r\n orphan\r\n", 2}}, rest: "BODY"},
		{name: "right after a malformed prefix", in: " p\r\nbroken\r\n q\r\n\r\n", prefix: " p\r\n",
			records: []wantRecord{{"broken\r\n q\r\n", 2}}},
		{name: "two records among fields",
			in:      "Subject: before\r\n: one\r\n orphan1\r\nX-Middle: intact\r\nbroken\r\n orphan2\r\nX-Keep: after\r\n\r\nBODY",
			fields:  []wantField{subject, {"X-Middle", "intact", "X-Middle: intact\r\n"}, keep},
			records: []wantRecord{{": one\r\n orphan1\r\n", 2}, {"broken\r\n orphan2\r\n", 2}}, rest: "BODY"},
		{name: "two records in a row", in: "a\r\n 1\r\nb\r\n 2\r\n\r\n",
			records: []wantRecord{{"a\r\n 1\r\n", 2}, {"b\r\n 2\r\n", 2}}},

		// Read exactly as before: fields, and damaged lines with no continuation.
		{name: "a folded field", in: "Subject: before\r\nX-Good: one\r\n two\r\nX-Keep: after\r\n\r\nBODY",
			fields: []wantField{subject, {"X-Good", "one two", "X-Good: one\r\n two\r\n"}, keep}, rest: "BODY"},
		{name: "a folded field read with the semicolon", in: around("X-Bad; value\r\n orphan\r\n"),
			fields: []wantField{subject, {"X-Bad", "value orphan", "X-Bad; value\r\n orphan\r\n"}, keep}, rest: "BODY"},
		{name: "a cr that is content of a field before a continuation", in: "Subject: before\r: value\r orphan\rX-Keep: after\r\rBODY",
			fields: []wantField{{"Subject", "before\r: value orphan", "Subject: before\r: value\r\n orphan\r\n"}, keep}, rest: "BODY"},
		{name: "a field's cr joins a colonless line and its continuation", in: around("X-Good: value\rbroken\r\n orphan\r\n"),
			fields: []wantField{subject, {"X-Good", "value\rbroken orphan", "X-Good: value\rbroken\r\n orphan\r\n"}, keep}, rest: "BODY"},
		{name: "an empty name alone", in: around(": value\r\n"), fields: sk, rest: "BODY"},
		{name: "an empty name a cr joins to a line alone", in: ":x\rb\r\nY: 1\r\n\r\n", fields: []wantField{{"Y", "1", "Y: 1\r\n"}}},
		{name: "the empty line, then an indented line", in: "Subject: before\r\n\r\n orphan\r\nX-Keep: body",
			fields: []wantField{subject}, rest: " orphan\r\nX-Keep: body"},
		{name: "no colon alone", in: around("broken line\r\n"), fields: []wantField{subject},
			err: "message: malformed MIME header line: broken line\r\n"},
		{name: "an invalid name alone", in: around("Bad Name: value\r\n"), fields: []wantField{subject},
			err: "message: malformed MIME header key: Bad Name"},
		{name: "no colon at the end of the input", in: "Subject: before\r\nbroken", fields: []wantField{subject},
			err: "message: malformed MIME header line: broken\r\n"},
		{name: "a damaged line a cr joins to a line alone", in: "Subject: s\r\nbroken\rb c\r\nX: 1\r\n\r\n",
			fields: []wantField{{"Subject", "s", "Subject: s\r\n"}}, err: "message: malformed MIME header line: broken\r\n"},
		{name: "a damaged line a cr ends before a field", in: "Subject: s\r\nbroken\rX: 1\r\n\r\n",
			fields: []wantField{{"Subject", "s", "Subject: s\r\n"}}, err: "message: malformed MIME header line: broken\r\n"},
		{name: "an invalid name a cr ends before a field", in: "Subject: s\r\nBad Name: v\rX: 1\r\n\r\n",
			fields: []wantField{{"Subject", "s", "Subject: s\r\n"}}, err: "message: malformed MIME header key: Bad Name"},
	}
}

// TestReadHeader_orphanContinuation pins each case under every delivery and
// buffer size, through ReadHeader and ReadHeaderLimits: the records' bytes,
// locations and lines, the prefix, the fields, the error and what is left in
// the reader. A record's bytes are the input's own at its location.
func TestReadHeader_orphanContinuation(t *testing.T) {
	for _, tc := range orphanCases() {
		want := wantRecordsIn(t, tc.in, tc.records)
		for name, newReader := range lineEndingReaders(tc.in) {
			for api, read := range map[string]func(*bufio.Reader) (Header, error){
				"ReadHeader": ReadHeader,
				"ReadHeaderLimits": func(r *bufio.Reader) (Header, error) {
					return ReadHeaderLimits(r, HeaderLimits{MaxBytes: 1 << 20, MaxLineBytes: 1 << 20, MaxFieldBytes: 1 << 20, MaxCount: 100})
				},
			} {
				label := fmt.Sprintf("%s/%s/%s", tc.name, name, api)
				br := newReader()
				h, err := read(br)
				if tc.err == "" && err != nil {
					t.Errorf("%s: err = %v, want none", label, err)
					continue
				}
				if tc.err != "" && (err == nil || err.Error() != tc.err) {
					t.Errorf("%s: err = %v, want %q", label, err, tc.err)
					continue
				}
				if got := recordsOf(&h); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s: records %+v, want %+v", label, got, want)
				}
				for _, r := range recordsOf(&h) {
					if tc.in[r.start:r.end] != r.bytes {
						t.Errorf("%s: record %q is not the input's bytes at [%d, %d)", label, r.bytes, r.start, r.end)
					}
				}
				if got := string(h.MalformedPrefix().Bytes()); got != tc.prefix {
					t.Errorf("%s: prefix %q, want %q", label, got, tc.prefix)
				}
				if got := fieldsOf(t, h); !sameFields(got, tc.fields) {
					t.Errorf("%s: fields = %q, want %q", label, got, tc.fields)
				}
				if h.Len() != len(tc.fields) {
					t.Errorf("%s: Len() = %d, want %d: a record is no field", label, h.Len(), len(tc.fields))
				}
				if tc.err != "" {
					continue
				}
				if rest, rerr := io.ReadAll(br); string(rest) != tc.rest || rerr != nil {
					t.Errorf("%s: rest = (%q, %v), want %q", label, rest, rerr, tc.rest)
				}
				if !h.LineEndings().Known() {
					t.Errorf("%s: LineEndings() unknown for a block read whole", label)
				}
			}
		}
	}
}

// TestReadHeader_orphanContinuationIsNoField pins that nothing a record holds
// is found by lookup, and that nothing of it is joined to the fields around
// it: a damaged name, an indented Subject, Content-Type or
// Content-Transfer-Encoding are not the header's, and the Subject and
// Content-Type before a record keep their values.
func TestReadHeader_orphanContinuationIsNoField(t *testing.T) {
	h := readBlock(t, "Subject: before\r\nContent-Type: text/plain\r\nbroken\r\n Subject: shifted\r\n"+
		" Content-Type: multipart/mixed; boundary=B\r\n Content-Transfer-Encoding: base64\r\n"+
		"Bad Name: v\r\n boundary=C\r\n: e\r\n charset=x\r\nX: 1\r\n\r\n")
	if h.Get("Subject") != "before" || h.Get("Content-Type") != "text/plain" || len(h.Values("Subject")) != 1 {
		t.Errorf("Subject %q, Content-Type %q: changed by the records after them", h.Get("Subject"), h.Get("Content-Type"))
	}
	for _, k := range []string{"Content-Transfer-Encoding", "Broken", "Bad Name", "Bad-Name", "", " Subject"} {
		if h.Has(k) || h.Get(k) != "" || h.Values(k) != nil {
			t.Errorf("%q is found in the header", k)
		}
	}
	if m := h.Map(); len(m) != 3 || h.Len() != 3 {
		t.Errorf("Map() = %v, Len() = %d, want Subject, Content-Type and X", m, h.Len())
	}
	if n := len(h.OrphanContinuations()); n != 3 {
		t.Errorf("%d records, want 3", n)
	}
}

// TestOrphanContinuation_view pins the view: the zero value, a nil Header, a
// header without records, a fresh copy from Bytes and a fresh slice from
// OrphanContinuations on every call, and the records kept by Copy and by every
// mutation while HeaderFromMap builds a header without them.
func TestOrphanContinuation_view(t *testing.T) {
	var zero OrphanContinuation
	if zero.Bytes() != nil || zero.Lines() != 0 {
		t.Errorf("zero value: (%q, %d)", zero.Bytes(), zero.Lines())
	}
	if s, e := zero.Location(); s != 0 || e != 0 {
		t.Errorf("zero value: Location() = (%d, %d)", s, e)
	}
	var nilHeader *Header
	if nilHeader.OrphanContinuations() != nil {
		t.Error("a nil Header has records")
	}
	plain := readBlock(t, "A: 1\r\n: skipped\r\n\r\n")
	if (&Header{}).OrphanContinuations() != nil || plain.OrphanContinuations() != nil {
		t.Error("a header without one has records")
	}

	const block = "A: 1\r\nbroken\r\n\tq\r\nB: 2\r\n\r\n"
	const record = "broken\r\n\tq\r\n"
	h := readBlock(t, block)
	l := h.OrphanContinuations()
	if len(l) != 1 {
		t.Fatalf("%d records, want 1", len(l))
	}
	o := l[0]
	b := o.Bytes()
	if string(b) != record || o.Lines() != 2 {
		t.Fatalf("Bytes() = %q, Lines() = %d", b, o.Lines())
	}
	if s, e := o.Location(); s != 6 || e != 6+int64(len(record)) {
		t.Errorf("Location() = (%d, %d)", s, e)
	}
	b[0] = 'x'
	if again := o.Bytes(); string(again) != record {
		t.Errorf("Bytes() after changing an earlier result = %q", again)
	}
	l[0] = OrphanContinuation{}
	if again := h.OrphanContinuations(); len(again) != 1 || string(again[0].Bytes()) != record {
		t.Errorf("the header's records after changing a returned slice = %d", len(again))
	}

	check := func(what string, h Header) {
		t.Helper()
		if l := h.OrphanContinuations(); len(l) != 1 || string(l[0].Bytes()) != record {
			t.Errorf("%s: records %+v, want %q", what, recordsOf(&h), record)
		}
	}
	c := h.Copy()
	check("Copy", c)
	c.Add("C", "3")
	check("Add", c)
	c.AddRaw([]byte("D: 4\r\n"))
	check("AddRaw", c)
	c.Set("A", "x")
	check("Set", c)
	c.Del("B")
	check("Del", c)
	fs := c.Fields()
	for fs.Next() {
		if fs.Key() == "C" {
			fs.Del()
		}
	}
	check("HeaderFields.Del", c)
	fk := c.FieldsByKey("D")
	for fk.Next() {
		fk.Del()
	}
	check("FieldsByKey.Del", c)
	check("the original after the copy changed", h)
	if h.Len() != 2 || h.Get("A") != "1" || h.Get("B") != "2" {
		t.Errorf("the original's fields changed with its copy's: %v", h.Map())
	}
	if rebuilt := HeaderFromMap(h.Map()); rebuilt.OrphanContinuations() != nil {
		t.Error("HeaderFromMap kept the records")
	}
}

// writtenRecords is what WriteHeader writes of records: each, and a line
// ending after a last line that has none or ends with a bare '\r'.
func writtenRecords(rs []recordOutcome) []string {
	var out []string
	for _, r := range rs {
		out = append(out, writtenPrefix(r.bytes))
	}
	return out
}

func recordBytes(rs []recordOutcome) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.bytes)
	}
	return out
}

// TestWriteHeader_orphanContinuation pins what is written: each record in its
// place among the fields read with it, after any fields added since, through
// every mutation; a complete CRLF block unchanged byte for byte; a last line
// without a line ending, or ended by a bare '\r', given one. Read back, the
// header is what was written — records and fields, the values of the fields
// before a record unchanged — and writing it again writes the same bytes.
func TestWriteHeader_orphanContinuation(t *testing.T) {
	const base = "A: 1\r\nbroken\r\n o\r\nB: 2\r\n\r\n"
	const two = "A: 1\r\nr1\r\n o\r\nM: 2\r\nr2\r\n p\r\nB: 3\r\n\r\n"
	for _, tc := range []struct {
		name   string
		in     string
		mutate func(*Header)
		want   string
	}{
		{"complete crlf block", "Subject: before\r\nbroken line\r\n orphan\r\n\tmore\r\nX-Keep: after\r\n\r\nBODY", nil,
			"Subject: before\r\nbroken line\r\n orphan\r\n\tmore\r\nX-Keep: after\r\n\r\n"},
		{"two records", two, nil, two},
		{"a record first", ": value\r\n orphan\r\nSubject: kept\r\n\r\n", nil, ": value\r\n orphan\r\nSubject: kept\r\n\r\n"},
		{"a record last", "A: 1\r\nbroken\r\n o\r\n\r\n", nil, "A: 1\r\nbroken\r\n o\r\n\r\n"},
		{"records in a row", "a\r\n 1\r\nb\r\n 2\r\n\r\n", nil, "a\r\n 1\r\nb\r\n 2\r\n\r\n"},
		{"a mark and a prefix", utf8Mark + " p\r\n" + base, nil, utf8Mark + " p\r\n" + base},
		{"nul, 8-bit and control bytes", "A: 1\r\nX\x00\xff: v\r\n \x01\r\n\r\n", nil, "A: 1\r\nX\x00\xff: v\r\n \x01\r\n\r\n"},
		{"lf", "A: 1\nbroken\n o\nB: 2\n\nbody", nil, "A: 1\r\nbroken\n o\nB: 2\r\n\r\n"},
		{"cr", "broken\r o\rB: 2\r\rbody", nil, "broken\r o\r\nB: 2\r\n\r\n"},
		{"a cr before a field", "broken\r\n y\rZ: 1\r\n\r\n", nil, "broken\r\n y\r\nZ: 1\r\n\r\n"},
		{"a cr that is content", "broken\rb c\r\n o\r\nZ: 1\r\n\r\n", nil, "broken\rb c\r\n o\r\nZ: 1\r\n\r\n"},
		{"no line ending at the end of the input", "A: 1\r\nbroken\r\n o", nil, "A: 1\r\nbroken\r\n o\r\n\r\n"},
		{"a cr at the end of the input", "A: 1\r\nbroken\r\n o\r", nil, "A: 1\r\nbroken\r\n o\r\n\r\n"},
		{"a field added", base, func(h *Header) { h.Add("X", "y") }, "X: y\r\n" + base},
		{"a raw field added", base, func(h *Header) { h.AddRaw([]byte("X: y\r\n")) }, "X: y\r\n" + base},
		{"a field added before a first record", "broken\r\n o\r\nA: 1\r\n\r\n", func(h *Header) { h.Add("X", "y") },
			"X: y\r\nbroken\r\n o\r\nA: 1\r\n\r\n"},
		{"a field added after a prefix", " p\r\n" + base, func(h *Header) { h.Add("X", "y") }, " p\r\nX: y\r\n" + base},
		{"the field before deleted", base, func(h *Header) { h.Del("A") }, "broken\r\n o\r\nB: 2\r\n\r\n"},
		{"the field after deleted", base, func(h *Header) { h.Del("B") }, "A: 1\r\nbroken\r\n o\r\n\r\n"},
		{"the field before set", base, func(h *Header) { h.Set("A", "z") }, "A: z\r\nbroken\r\n o\r\nB: 2\r\n\r\n"},
		{"the field after set", base, func(h *Header) { h.Set("B", "z") }, "B: z\r\nA: 1\r\nbroken\r\n o\r\n\r\n"},
		{"every field deleted", base, func(h *Header) { h.Del("A"); h.Del("B") }, "broken\r\n o\r\n\r\n"},
		{"the field before deleted while iterating", base, func(h *Header) {
			fs := h.Fields()
			for fs.Next() {
				if fs.Key() == "A" {
					fs.Del()
				}
			}
		}, "broken\r\n o\r\nB: 2\r\n\r\n"},
		{"the field after deleted by key", base, func(h *Header) {
			fs := h.FieldsByKey("B")
			for fs.Next() {
				fs.Del()
			}
		}, "A: 1\r\nbroken\r\n o\r\n\r\n"},
		{"the field between two records deleted", two, func(h *Header) { h.Del("M") },
			"A: 1\r\nr1\r\n o\r\nr2\r\n p\r\nB: 3\r\n\r\n"},
		{"a field added to an unterminated record", "A: 1\r\nbroken\r\n o", func(h *Header) { h.Add("X", "y") },
			"X: y\r\nA: 1\r\nbroken\r\n o\r\n\r\n"},
	} {
		h := readBlock(t, tc.in)
		before := recordsOf(&h)
		if tc.mutate != nil {
			tc.mutate(&h)
		}
		got := writtenHeader(t, h)
		if got != tc.want {
			t.Errorf("%s: WriteHeader() = %q, want %q", tc.name, got, tc.want)
			continue
		}
		if after := recordsOf(&h); fmt.Sprint(after) != fmt.Sprint(before) {
			t.Errorf("%s: writing changed the records' evidence to %+v", tc.name, after)
		}
		again := readBlock(t, got)
		if rs := recordBytes(recordsOf(&again)); fmt.Sprint(rs) != fmt.Sprint(writtenRecords(before)) {
			t.Errorf("%s: read back, the records are %q, want %q", tc.name, rs, writtenRecords(before))
		}
		if !sameFields(fieldsOf(t, again), fieldsOf(t, h)) {
			t.Errorf("%s: read back, the fields are %q, want %q", tc.name, fieldsOf(t, again), fieldsOf(t, h))
		}
		if second := writtenHeader(t, again); second != got {
			t.Errorf("%s: written again = %q, want %q", tc.name, second, got)
		}
	}
}

// TestWriteHeader_orphanContinuationWriteFailure pins that a writer failing at
// any byte, inside a field, a record or the line ending given to it, fails
// WriteHeader with its own error after the bytes it took, and that the header
// is not changed.
func TestWriteHeader_orphanContinuationWriteFailure(t *testing.T) {
	for _, in := range []string{"A: 1\r\nbroken\r\n o\r\nB: 2\r\n\r\n", "A: 1\r\nbroken\r\n o", "broken\r\n y\rZ: 1\r\n\r\n", " p\r\na\r\n 1\r\n\r\n"} {
		h := readBlock(t, in)
		whole := writtenHeader(t, h)
		for n := 0; n < len(whole); n++ {
			w := &failAfterWriter{n: n}
			if err := WriteHeader(w, h); err != errShortWrite {
				t.Errorf("%q/%d: WriteHeader() = %v, want the writer's error", in, n, err)
			}
			if !strings.HasPrefix(whole, w.buf.String()) || w.buf.Len() > n {
				t.Errorf("%q/%d: wrote %q, want at most %d bytes of %q", in, n, w.buf.String(), n, whole)
			}
		}
		if writtenHeader(t, h) != whole {
			t.Errorf("%q: the header changed with the failed writes", in)
		}
	}
}

// TestReadHeader_orphanContinuationBudgets pins the budgets: a record is one
// logical candidate, every line and byte of it charged once as it is read, and
// the field counts charged once for it; a block refused keeps no record, no
// prefix and no field.
func TestReadHeader_orphanContinuationBudgets(t *testing.T) {
	// A: 1 at 0 (6 bytes), the record at 6 (17 bytes, lines of 6 and 7), B: 2
	// at 23 (6 bytes), the empty line at 29: 31 bytes, 3 candidates.
	const block = "A: 1\r\nbroken\r\n orphan\r\nB: 2\r\n\r\n"
	const record = "broken\r\n orphan\r\n"
	ok := func(name string, l HeaderLimits) {
		t.Helper()
		h, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block+"body")), l)
		if err != nil || fmt.Sprint(recordBytes(recordsOf(&h))) != fmt.Sprint([]string{record}) || h.Get("A") != "1" || h.Get("B") != "2" {
			t.Errorf("%s: (%+v, %v), want the record, A and B", name, recordsOf(&h), err)
		}
	}
	refused := func(name string, l HeaderLimits, kind HeaderLimitKind, at int64) {
		t.Helper()
		h, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block+"body")), l)
		e, isLimit := HeaderLimit(err)
		if !isLimit || e.Kind() != kind || e.UnitOffset() != at {
			t.Errorf("%s: err = %v, want a %v refusal at %d", name, err, kind, at)
		}
		if h.OrphanContinuations() != nil || h.Len() != 0 {
			t.Errorf("%s: a refused block kept (%+v, %d fields)", name, recordsOf(&h), h.Len())
		}
	}
	ok("exact", HeaderLimits{MaxBytes: 31, MaxLineBytes: 7, MaxFieldBytes: 17, MaxCount: 3})
	refused("block bytes", HeaderLimits{MaxBytes: 30}, HeaderLimitBytes, 0)
	refused("line bytes", HeaderLimits{MaxLineBytes: 6}, HeaderLimitLineBytes, 14)
	refused("field bytes", HeaderLimits{MaxFieldBytes: 16}, HeaderLimitFieldBytes, 6)
	refused("count", HeaderLimits{MaxCount: 2}, HeaderLimitCount, 23)

	// The shared budget: bytes counted once each, the record counted as one
	// candidate, and a spent count refusing it at its first byte.
	ok("shared exact", HeaderLimits{Total: NewHeaderBudget(31, 3)})
	refused("shared bytes", HeaderLimits{Total: NewHeaderBudget(30, 0)}, HeaderLimitTotalBytes, 30)
	refused("shared count", HeaderLimits{Total: NewHeaderBudget(0, 2)}, HeaderLimitTotalCount, 23)
	shared := NewHeaderBudget(31, 3)
	if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block)), HeaderLimits{Total: shared}); err != nil {
		t.Fatal(err)
	}
	if shared.usedBytes != 31 || shared.usedCount != 3 {
		t.Errorf("the shared budget was charged (%d, %d), want (31, 3): every byte and candidate once", shared.usedBytes, shared.usedCount)
	}

	spent := NewHeaderBudget(0, 2)
	if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("X: 1\r\n\r\n")), HeaderLimits{Total: spent}); err != nil {
		t.Fatal(err)
	}
	const tail = "broken\r\n orphan\r\n\r\n"
	br := bufio.NewReader(strings.NewReader("A: 1\r\n" + tail))
	h, err := ReadHeaderLimits(br, HeaderLimits{Total: spent})
	if e, isLimit := HeaderLimit(err); !isLimit || e.Kind() != HeaderLimitTotalCount || e.UnitOffset() != 6 || h.OrphanContinuations() != nil {
		t.Errorf("a spent shared count: (%+v, %v), want its refusal at the record's first byte", recordsOf(&h), err)
	}
	if rest, _ := io.ReadAll(br); string(rest) != tail {
		t.Errorf("a spent shared count read %q of the record", tail[:len(tail)-len(rest)])
	}
}

// orphanKept returns what a block that begins with "A: 1\r\nbroken\r\n" keeps of
// kept, its lines read whole before a failure: the lines, less the damaged
// line when no continuation line of it was read whole, and the record — the
// damaged line and its whole continuation lines — if there is one.
func orphanKept(kept string) (written, record string) {
	const head = "A: 1\r\nbroken\r\n"
	if !strings.HasPrefix(kept, head) {
		return kept, ""
	}
	n := len(head)
	for n < len(kept) && kept[n] == ' ' {
		n += strings.Index(kept[n:], "\r\n") + 2
	}
	if n == len(head) {
		return kept[:len(head)-len("broken\r\n")] + kept[n:], ""
	}
	return kept, kept[len("A: 1\r\n"):n]
}

// TestReadHeader_orphanContinuationReadFailure cuts a block holding a record at
// every byte, at buffer sizes 16 and 4096 and three chunkings, with the
// failure delivered with the last bytes or after them, from sources that keep
// failing and from sources that resume. A failure other than io.EOF is returned
// as itself — never the damaged line's own error, since the failure is what
// kept its continuation from being seen — with every line read whole before
// it: the record holds its whole lines, and none when no continuation line was
// read whole; a line it cut is dropped; nothing after it is read. io.EOF
// behaves as it does from a source that keeps returning it.
func TestReadHeader_orphanContinuationReadFailure(t *testing.T) {
	const block = "A: 1\r\nbroken\r\n orphan\r\n z\r\nB: 2\r\n\r\n"
	const text = block + "body text"
	for cut := 0; cut <= len(text); cut++ {
		for _, size := range []int{16, 4096} {
			for _, chunk := range []int{1, 7, 4096} {
				for _, withData := range []bool{false, true} {
					for _, fail := range []error{errEIO, io.EOF} {
						results := map[bool]string{}
						for _, sticky := range []bool{true, false} {
							name := fmt.Sprintf("cut %d/buf %d/chunk %d/with %v/%v/sticky %v", cut, size, chunk, withData, fail, sticky)
							src := &failAtSource{data: text, cut: cut, chunk: chunk, err: fail, withData: withData, sticky: sticky}
							br := bufio.NewReaderSize(src, size)
							h, err := ReadHeader(br)
							if src.readsAfter != 0 {
								t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
							}
							results[sticky] = fmt.Sprintf("%q %q %v", writtenHeader(t, h), recordBytes(recordsOf(&h)), err)
							if fail != errEIO {
								continue
							}
							kept, complete := wholeLines(text, cut)
							if complete {
								if err != nil || writtenHeader(t, h) != block {
									t.Errorf("%s: (%q, %v), want the whole block", name, writtenHeader(t, h), err)
								}
								continue
							}
							if err != errEIO {
								t.Errorf("%s: err = %v, want the input's own failure", name, err)
							}
							written, record := orphanKept(kept)
							if got := writtenHeader(t, h); got != written+"\r\n" {
								t.Errorf("%s: kept %q, want the whole lines %q", name, got, written+"\r\n")
							}
							var want []string
							if record != "" {
								want = []string{record}
							}
							if got := recordBytes(recordsOf(&h)); fmt.Sprint(got) != fmt.Sprint(want) {
								t.Errorf("%s: records %q, want %q", name, got, want)
							}
						}
						if results[true] != results[false] {
							t.Errorf("cut %d/buf %d/chunk %d/with %v/%v: a resuming source gave %s, a failing one %s",
								cut, size, chunk, withData, fail, results[false], results[true])
						}
					}
				}
			}
		}
	}
}

// TestReadHeader_orphanContinuationCRFailure pins a failure at a '\r' that is
// still waiting for the next line to settle it: after a record's continuation
// line, the failure cuts that line too, so a record left with no continuation
// is not kept; after a damaged line, the line it was to join is cut and nothing
// is kept. A '\r' the next line settled is kept with what it ended.
func TestReadHeader_orphanContinuationCRFailure(t *testing.T) {
	for _, tc := range []struct {
		text   string
		cut    int
		record string
	}{
		{"broken\r\n y\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\r\n y"), ""},
		{"broken\r\n y\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\r\n y\r"), ""},
		{"broken\r\n y\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\r\n y\rb"), ""},
		{"broken\r\n y\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\r\n y\rb c\r"), ""},
		{"broken\r\n y\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\r\n y\rb c\r\n"), "broken\r\n y\rb c\r\n"},
		{"broken\r\n y\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\r\n y\rb c\r\n z\r\n"), "broken\r\n y\rb c\r\n z\r\n"},
		{"broken\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken"), ""},
		{"broken\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\r"), ""},
		{"broken\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\rb"), ""},
		{"broken\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\rb c\r\n"), ""},
		{"broken\rb c\r\n z\r\nX: 1\r\n\r\n", len("broken\rb c\r\n z\r\n"), "broken\rb c\r\n z\r\n"},
		{": x\rb\r\n y\r\nX: 1\r\n\r\n", len(": x\rb"), ""},
		{": x\rb\r\n y\r\nX: 1\r\n\r\n", len(": x\rb\r\n y\r\n"), ": x\rb\r\n y\r\n"},
	} {
		for _, chunk := range []int{1, 4096} {
			for _, sticky := range []bool{true, false} {
				src := &failAtSource{data: tc.text, cut: tc.cut, chunk: chunk, err: errEIO, sticky: sticky}
				h, err := ReadHeader(bufio.NewReader(src))
				var want []string
				if tc.record != "" {
					want = []string{tc.record}
				}
				if got := recordBytes(recordsOf(&h)); err != errEIO || fmt.Sprint(got) != fmt.Sprint(want) || src.readsAfter != 0 || h.Len() != 0 {
					t.Errorf("%q/cut %d/chunk %d/sticky %v: (%q, %d fields, %v) after %d reads past the failure, want (%q, the failure)",
						tc.text, tc.cut, chunk, sticky, got, h.Len(), err, src.readsAfter, want)
				}
			}
		}
	}
}

// orphanSteps iterates the multipart body in r, framed by boundary, for at most
// steps steps or until io.EOF, and describes each: a part by its records,
// prefix, fields, missing separator, body and how its body ended, or the error.
func orphanSteps(t *testing.T, r io.Reader, boundary string, limits HeaderLimits, steps int) []string {
	t.Helper()
	mr := NewMultipartReader(r, boundary)
	mr.SetHeaderLimits(limits)
	var out []string
	for range steps {
		p, err := mr.NextPart()
		if err != nil {
			out = append(out, describeError(err))
			if err == io.EOF {
				break
			}
			continue
		}
		body, berr := io.ReadAll(io.LimitReader(p, 1<<14))
		if len(body) >= 1<<14 {
			t.Fatalf("a body reached the test's cap")
		}
		out = append(out, orphanStep(recordBytes(recordsOf(&p.Header)), fieldsOf(t, p.Header), p.HeaderSeparatorMissing(), string(body), describeError(berr)))
	}
	return out
}

func orphanStep(records []string, fields []wantField, missing bool, body, end string) string {
	return fmt.Sprintf("part records=%q fields=%q missing=%v body=%q end=%q", records, fields, missing, body, end)
}

// TestMultipartReader_orphanContinuation pins a part header holding a record:
// a first, middle and last part; a separator present; a delimiter line, opening
// or closing and with every line ending, right after the record or after a
// field after it, ending that header rather than being read as one of its
// lines; lines that only begin like a delimiter read as the fields they are;
// and a damaged line whose '\r' a delimiter would settle, decided as it always
// was with the delimiter left for the next part.
func TestMultipartReader_orphanContinuation(t *testing.T) {
	const last = "--B\r\nSubject: last\r\n\r\nTHREE\r\n--B--\r\n"
	lastStep := orphanStep(nil, []wantField{{"Subject", "last", "Subject: last\r\n"}}, false, "THREE", "")
	one := orphanStep(nil, nil, false, "ONE", "")
	rec := func(r ...string) []string { return r }
	malformedLine := "other *errors.errorString"
	for _, tc := range []struct {
		name  string
		in    string
		steps []string
	}{
		{"a middle part with a separator",
			"--B\r\n\r\nONE\r\n--B\r\nSubject: before\r\nbroken\r\n orphan\r\nX-Keep: after\r\n\r\nBODY\r\n" + last,
			[]string{one, orphanStep(rec("broken\r\n orphan\r\n"), []wantField{{"Subject", "before", "Subject: before\r\n"}, {"X-Keep", "after", "X-Keep: after\r\n"}}, false, "BODY", ""), lastStep, "EOF"}},
		{"a middle part with an empty name",
			"--B\r\n\r\nONE\r\n--B\r\nSubject: before\r\n: value\r\n orphan\r\nX-Keep: after\r\n\r\nBODY\r\n" + last,
			[]string{one, orphanStep(rec(": value\r\n orphan\r\n"), []wantField{{"Subject", "before", "Subject: before\r\n"}, {"X-Keep", "after", "X-Keep: after\r\n"}}, false, "BODY", ""), lastStep, "EOF"}},
		{"a middle part whose separator is missing",
			"--B\r\n\r\nONE\r\n--B\r\nSubject: middle\r\nbroken\r\n orphan\r\n" + last,
			[]string{one, orphanStep(rec("broken\r\n orphan\r\n"), []wantField{{"Subject", "middle", "Subject: middle\r\n"}}, true, "", ""), lastStep, "EOF"}},
		{"a first part whose separator is missing after a field",
			"--B\r\nbroken\r\n o\r\nX: 1\r\n" + last,
			[]string{orphanStep(rec("broken\r\n o\r\n"), []wantField{{"X", "1", "X: 1\r\n"}}, true, "", ""), lastStep, "EOF"}},
		{"a last part ended by the closing delimiter",
			"--B\r\n\r\nONE\r\n--B\r\nbroken\r\n o\r\n--B--\r\n",
			[]string{one, orphanStep(rec("broken\r\n o\r\n"), nil, true, "", ""), "EOF"}},
		{"a delimiter ended by lf",
			"--B\r\nbroken\r\n o\r\n--B\nSubject: last\r\n\r\nTHREE\r\n--B--\r\n",
			[]string{orphanStep(rec("broken\r\n o\r\n"), nil, true, "", ""), lastStep, "EOF"}},
		{"a delimiter ended by cr",
			"--B\r\nbroken\r\n o\r\n--B\rSubject: last\r\n\r\nTHREE\r\n--B--\r\n",
			[]string{orphanStep(rec("broken\r\n o\r\n"), nil, true, "", ""), lastStep, "EOF"}},
		{"a delimiter padded with white space",
			"--B\r\nbroken\r\n o\r\n--B \t \r\nSubject: last\r\n\r\nTHREE\r\n--B--\r\n",
			[]string{orphanStep(rec("broken\r\n o\r\n"), nil, true, "", ""), lastStep, "EOF"}},
		{"a delimiter after a record's cr",
			"--B\r\nbroken\r\n o\r--B\r\nSubject: last\r\n\r\nTHREE\r\n--B--\r\n",
			[]string{orphanStep(rec("broken\r\n o\r"), nil, true, "", ""), lastStep, "EOF"}},
		{"a field that begins like a delimiter",
			"--B\r\nbroken\r\n o\r\n--Bx: 1\r\n\r\nbody\r\n--B--\r\n",
			[]string{orphanStep(rec("broken\r\n o\r\n"), []wantField{{"--Bx", "1", "--Bx: 1\r\n"}}, false, "body", ""), "EOF"}},
		{"a padded field that begins like a delimiter",
			"--B\r\nbroken\r\n o\r\n--B : v\r\n\r\nbody\r\n--B--\r\n",
			[]string{orphanStep(rec("broken\r\n o\r\n"), []wantField{{"--B", "v", "--B : v\r\n"}}, false, "body", ""), "EOF"}},
		{"a damaged line a cr joins to a line with a continuation",
			"--B\r\nbroken\rb c\r\n z\r\n\r\nBODY\r\n--B--\r\n",
			[]string{orphanStep(rec("broken\rb c\r\n z\r\n"), nil, false, "BODY", ""), "EOF"}},
		{"a damaged line whose cr a delimiter would settle",
			"--B\r\nbroken\r--B\r\nSubject: healthy\r\n\r\nKEPT\r\n--B--\r\n",
			[]string{malformedLine, orphanStep(nil, []wantField{{"Subject", "healthy", "Subject: healthy\r\n"}}, false, "KEPT", ""), "EOF"}},
		{"after a record, a damaged line whose cr a delimiter would settle",
			"--B\r\nr\r\n o\r\nbroken\r--B\r\nSubject: healthy\r\n\r\nKEPT\r\n--B--\r\n",
			[]string{malformedLine, orphanStep(nil, []wantField{{"Subject", "healthy", "Subject: healthy\r\n"}}, false, "KEPT", ""), "EOF"}},
		{"a damaged line with no continuation, then a delimiter",
			"--B\r\nSubject: x\r\nbroken\r\n--B\r\nSubject: healthy\r\n\r\nKEPT\r\n--B--\r\n",
			[]string{malformedLine, orphanStep(nil, []wantField{{"Subject", "healthy", "Subject: healthy\r\n"}}, false, "KEPT", ""), "EOF"}},
	} {
		for _, chunk := range []int{1, 7, 4096} {
			if got := orphanSteps(t, &chunkReader{s: tc.in, n: chunk}, "B", HeaderLimits{}, 5); fmt.Sprint(got) != fmt.Sprint(tc.steps) {
				t.Errorf("%s/chunk %d:\n got %q\nwant %q", tc.name, chunk, got, tc.steps)
			}
		}
	}
}

// orphanPaddingInput is a multipart body whose first part's header is a record
// followed by line — then a healthy part's header, its body and the closing
// delimiter.
func orphanPaddingInput(line string) string {
	return "--B\r\nbroken\r\n o\r\n" + line + "\r\nSubject: kept\r\n\r\nBODY\r\n--B--\r\n"
}

// TestMultipartReader_orphanContinuationLongPadding pins a line after a part
// header's record that begins like a delimiter and whose white space runs past
// the reader's buffer: a delimiter is proved within the framing line budget and
// charged to no header budget — not even one the record and the sibling's
// header spend exactly — while a field padded alike is the field it is, charged
// once; with no line budget a line still undecided past one buffer is the
// multipart reader's framing refusal.
func TestMultipartReader_orphanContinuationLongPadding(t *testing.T) {
	const record = "broken\r\n o\r\n"
	const sibling = "Subject: kept\r\n\r\n"
	damaged := orphanStep([]string{record}, nil, true, "", "")
	kept := orphanStep(nil, []wantField{{"Subject", "kept", "Subject: kept\r\n"}}, false, "BODY", "")
	for _, padding := range []int{0, 16, 4093, 5000} {
		pad := strings.Repeat(" ", padding)
		for _, chunk := range []int{1, 4096} {
			label := fmt.Sprintf("padding %d/chunk %d", padding, chunk)
			for _, l := range []HeaderLimits{
				{MaxBytes: 16384, MaxFieldBytes: 12000, MaxLineBytes: 12000},
				{MaxBytes: int64(len(sibling)), MaxLineBytes: 12000},
				{MaxFieldBytes: int64(len("Subject: kept\r\n")), MaxLineBytes: 12000},
				{Total: NewHeaderBudget(int64(len(record+sibling)), 2), MaxLineBytes: 12000},
			} {
				got := orphanSteps(t, &chunkReader{s: orphanPaddingInput("--B" + pad), n: chunk}, "B", l, 4)
				if want := []string{damaged, kept, "EOF"}; fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s/%+v: delimiter:\n got %q\nwant %q", label, l, got, want)
				}
				if tot := l.Total; tot != nil && (tot.Refusal() != nil || tot.usedBytes != int64(len(record+sibling)) || tot.usedCount != 2) {
					t.Errorf("%s: shared budget used (%d, %d), refused %v; want the header blocks alone", label, tot.usedBytes, tot.usedCount, tot.Refusal())
				}
			}

			line := "--B" + pad + ": value"
			field := orphanStep([]string{record}, []wantField{{"--B", "value", line + "\r\n"}, {"Subject", "kept", "Subject: kept\r\n"}}, false, "BODY", "")
			budget := HeaderLimits{MaxBytes: 16384, MaxFieldBytes: 12000, MaxLineBytes: 12000}
			if got, want := orphanSteps(t, &chunkReader{s: orphanPaddingInput(line), n: chunk}, "B", budget, 4), []string{field, "EOF"}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s: field:\n got %q\nwant %q", label, got, want)
			}
			shared := NewHeaderBudget(1<<20, 0)
			budget.Total = shared
			orphanSteps(t, &chunkReader{s: orphanPaddingInput(line), n: chunk}, "B", budget, 4)
			if want := int64(len(record + line + "\r\n" + sibling)); shared.usedBytes != want {
				t.Errorf("%s: field charged %d shared bytes, want %d: each byte once", label, shared.usedBytes, want)
			}

			if padding > 4096 {
				if got := orphanSteps(t, &chunkReader{s: orphanPaddingInput("--B" + pad), n: chunk}, "B", HeaderLimits{}, 4); fmt.Sprint(got) != fmt.Sprint([]string{"buffer full", "EOF"}) {
					t.Errorf("%s: no line budget: %q, want the framing refusal", label, got)
				}
			}
		}
	}
}

// TestMultipartReader_orphanContinuationReadFailure cuts a multipart body — a
// part whose header is a record ended by the next delimiter, a healthy part,
// the closing delimiter — at every byte, at three chunkings, with the failure
// delivered with the last bytes or after them, and holds every step to what
// the cut says, as for a malformed prefix: the damaged part needs its whole
// delimiter line after the record, a failure before it is the header's own,
// and the reader is never read after the failure. io.EOF at the cut is held to
// what the same bytes read to their natural end give.
func TestMultipartReader_orphanContinuationReadFailure(t *testing.T) {
	const (
		opening = "--B\r\n"
		record  = "broken\r\n orphan\r\n"
		delim   = "--B\r\n"
		header  = "Subject: healthy\r\n\r\n"
		content = "KEPT"
		closing = "\r\n--B--\r\n"
	)
	text := opening + record + delim + header + content + closing
	damaged := orphanStep([]string{record}, nil, true, "", "")
	healthy := func(body, end string) string {
		return orphanStep(nil, []wantField{{"Subject", "healthy", "Subject: healthy\r\n"}}, false, body, end)
	}
	bodyAt := len(opening + record + delim + header)
	expect := func(cut int) []string {
		switch {
		case cut < len(opening):
			return []string{"failure wrapped", "failure wrapped", "failure wrapped", "failure wrapped"}
		case cut < len(opening+record+delim):
			return []string{"failure", "failure wrapped", "failure wrapped", "failure wrapped"}
		case cut < bodyAt:
			return []string{damaged, "failure", "failure wrapped", "failure wrapped"}
		case cut < len(text):
			return []string{damaged, healthy(text[bodyAt:cut], "failure wrapped"), "failure wrapped", "failure wrapped"}
		}
		return []string{damaged, healthy(content, ""), "EOF"}
	}
	for cut := 0; cut <= len(text); cut++ {
		for _, chunk := range []int{1, 7, 4096} {
			for _, withData := range []bool{false, true} {
				label := fmt.Sprintf("cut %d/chunk %d/with %v", cut, chunk, withData)
				src := &failAtSource{data: text, cut: cut, chunk: chunk, err: errCut, withData: withData, sticky: true}
				if got, want := orphanSteps(t, src, "B", HeaderLimits{}, 4), expect(cut); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s: failure:\n got %q\nwant %q", label, got, want)
				}
				if src.readsAfter != 0 {
					t.Errorf("%s: %d reads after the failure", label, src.readsAfter)
				}

				src = &failAtSource{data: text, cut: cut, chunk: chunk, err: io.EOF, withData: withData, sticky: true}
				got := orphanSteps(t, src, "B", HeaderLimits{}, 4)
				if natural := orphanSteps(t, &chunkReader{s: text[:cut], n: chunk}, "B", HeaderLimits{}, 4); fmt.Sprint(got) != fmt.Sprint(natural) {
					t.Errorf("%s: io.EOF:\n got %q\nwant what the bytes alone give, %q", label, got, natural)
				}
			}
		}
	}
}

const orphanBufferFullEnv = "EMLKIT_TEXTPROTO_ORPHAN_BUFFER_FULL"

// TestOrphanContinuation_sourceBufferFull pins a source that keeps failing with
// bufio.ErrBufferFull itself — the value the buffered reader also uses for a
// full buffer — inside a record, at the look for its continuation lines, and
// inside a delimiter line after it in a part's header: that value is the
// source's failure, returned as itself, never read on. It runs in a subprocess
// with a deadline, as the other such probes do.
func TestOrphanContinuation_sourceBufferFull(t *testing.T) {
	if os.Getenv(orphanBufferFullEnv) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOrphanContinuation_sourceBufferFull$", "-test.count=1")
		cmd.Env = append(os.Environ(), orphanBufferFullEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("subprocess = %v, deadline %v\n%s", err, ctx.Err(), out)
		}
		return
	}

	const block = "A: 1\r\nbroken\r\n orphan\r\nB: 2\r\n\r\n"
	for cut := 0; cut <= len(block); cut++ {
		for _, chunk := range []int{1, 4096} {
			for _, withData := range []bool{false, true} {
				name := fmt.Sprintf("header/cut %d/chunk %d/with %v", cut, chunk, withData)
				src := &failAtSource{data: block, cut: cut, chunk: chunk, err: bufio.ErrBufferFull, withData: withData, sticky: true}
				h, err := ReadHeader(bufio.NewReaderSize(src, 16))
				kept, complete := wholeLines(block, cut)
				if complete {
					continue
				}
				written, record := orphanKept(kept)
				var want []string
				if record != "" {
					want = []string{record}
				}
				if err != bufio.ErrBufferFull || fmt.Sprint(recordBytes(recordsOf(&h))) != fmt.Sprint(want) || writtenHeader(t, h) != written+"\r\n" {
					t.Errorf("%s: (%q, %v), want %q and the source's own error", name, recordBytes(recordsOf(&h)), err, want)
				}
				if src.readsAfter != 0 {
					t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
				}
			}
		}
	}

	text := "--B\r\nbroken\r\n orphan\r\n--B\r\nSubject: healthy\r\n\r\nKEPT\r\n--B--\r\n"
	proved := len("--B\r\nbroken\r\n orphan\r\n--B\r\n")
	for cut := len("--B\r\nb"); cut <= proved+3; cut++ {
		for _, chunk := range []int{1, 4096} {
			name := fmt.Sprintf("multipart/cut %d/chunk %d", cut, chunk)
			src := &failAtSource{data: text, cut: cut, chunk: chunk, err: bufio.ErrBufferFull, sticky: true}
			mr := NewMultipartReader(src, "B")
			mr.SetHeaderLimits(HeaderLimits{MaxBytes: 16384, MaxFieldBytes: 12000, MaxLineBytes: 12000})
			p, err := mr.NextPart()
			if cut >= proved {
				if err != nil || p == nil || !p.HeaderSeparatorMissing() {
					t.Errorf("%s: (%v, %v), want the damaged part", name, p != nil, err)
				}
			} else if p != nil || err != bufio.ErrBufferFull {
				t.Errorf("%s: (%v, %v), want the source's own error", name, p != nil, err)
			}
			if src.readsAfter != 0 {
				t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
			}
		}
	}
}

// TestOrphanContinuation_foldedHealthyAllocations is a relative check within
// this build only: a block of 20 healthy two-line (folded) fields costs no more
// allocations than a block of 20 one-line fields of the same shape. The line
// endings noted for orphan records are one stored byte a line in the scan's
// fixed storage for candidates of up to 16 lines, so continuation lines must
// not add allocations of their own. It does not compare with an earlier build
// and does not establish that this item adds no allocation at all; that is
// measured separately on finite inputs. Every measured parse is checked to
// read the block whole, with its fields and no record.
func TestOrphanContinuation_foldedHealthyAllocations(t *testing.T) {
	measure := func(block, value string) float64 {
		t.Helper()
		var bad error
		allocs := testing.AllocsPerRun(50, func() {
			h, err := ReadHeader(bufio.NewReader(strings.NewReader(block)))
			switch {
			case err != nil:
				bad = err
			case h.Len() != 20 || h.Get("X") != value || h.OrphanContinuations() != nil:
				bad = fmt.Errorf("%d fields, X %q, %d records", h.Len(), h.Get("X"), len(h.OrphanContinuations()))
			}
		})
		if bad != nil {
			t.Fatalf("%q: a measured parse gave %v", block[:16], bad)
		}
		return allocs
	}
	flat := strings.Repeat("X: yyyyyyyy\r\n", 20) + "\r\n"
	folded := strings.Repeat("X: y\r\n yyy\r\n", 20) + "\r\n"
	if f, g := measure(flat, "yyyyyyyy"), measure(folded, "y yyy"); g > f {
		t.Errorf("a folded block costs %v allocations, a flat one %v", g, f)
	}
}

// TestReadHeader_orphanCRLeavesNextUnread pins the regression Codex's QA found
// in the first implementation: a colonless or invalid damaged line whose last
// line ends with a bare '\r' before a name byte, and that no continuation line
// follows, is refused with the error it always was and everything after its
// '\r' — a following field included — is in the reader afterwards, as at
// 5b7086c; never read and dropped. Some of these are decided only past the
// reader's buffer (a 5000-byte name, joined line or chain): what was taken out
// to decide them is given back. Every delivery and buffer size gives the same.
func TestReadHeader_orphanCRLeavesNextUnread(t *testing.T) {
	const line = "message: malformed MIME header line: broken\r\n"
	long := strings.Repeat("v", 5000)
	for _, tc := range []struct {
		name, head, after string
		fields            []wantField
		err               string
	}{
		{"a field", "broken\r", "Next: value\r\nX-Keep: next\r\n\r\nBODY", nil, line},
		{"an invalid name before a field", "Subject: s\r\nBad Name: v\r", "Next: value\r\n\r\nBODY",
			[]wantField{{"Subject", "s", "Subject: s\r\n"}}, "message: malformed MIME header key: Bad Name"},
		{"a line the cr joins, then a field", "broken\r", "b c\r\nX: 1\r\n\r\nBODY", nil, line},
		{"lf", "broken\r", "Next: v\nX: 1\n\nBODY", nil, line},
		{"cr", "broken\r", "Next: v\rX: 1\r\rBODY", nil, line},
		{"a field at the end of the input", "broken\r", "Next", nil, line},
		{"a joined line at the end of the input", "broken\r", "b c", nil, line},
		{"a joined line, then a cr at the end of the input", "broken\r", "b c\r", nil, line},
		{"a long field", "broken\r", "Next: " + long + "\r\n\r\nBODY", nil, line},
		{"a long name", "broken\r", strings.Repeat("a", 5000) + ": v\r\n\r\nBODY", nil, line},
		{"a long joined line, then a field", "broken\r", "b " + long + "\r\nX: 1\r\n\r\nBODY", nil, line},
		{"a long chain of content crs, then a field", "broken\r", strings.Repeat("b c\r", 1500) + "d\r\nX: 1\r\n\r\nBODY", nil, line},
		{"a field after a chain of content crs", "broken\r", "b c\rd e\rNext: v\r\n\r\nBODY", nil, line},
		{"a long joined line at the end of the input", "broken\r", "b " + long, nil, line},
		{"a delimiter-shaped line", "broken\r", "--B\r\nSubject: healthy\r\n\r\nKEPT", nil, line},
	} {
		in := tc.head + tc.after
		for name, newReader := range lineEndingReaders(in) {
			for api, read := range map[string]func(*bufio.Reader) (Header, error){
				"ReadHeader": ReadHeader,
				"ReadHeaderLimits": func(r *bufio.Reader) (Header, error) {
					return ReadHeaderLimits(r, HeaderLimits{MaxBytes: 1 << 20, MaxLineBytes: 1 << 20, MaxFieldBytes: 1 << 20, MaxCount: 100})
				},
			} {
				label := fmt.Sprintf("%s/%s/%s", tc.name, name, api)
				br := newReader()
				h, err := read(br)
				if err == nil || err.Error() != tc.err {
					t.Errorf("%s: err = %v, want %q", label, err, tc.err)
				}
				if got := fieldsOf(t, h); !sameFields(got, tc.fields) || h.OrphanContinuations() != nil {
					t.Errorf("%s: fields %q, records %+v, want %q and none", label, got, recordsOf(&h), tc.fields)
				}
				if rest, rerr := io.ReadAll(br); string(rest) != tc.after || rerr != nil {
					t.Errorf("%s: left (%q, %v), want everything after the cr unread, %q", label, rest, rerr, tc.after)
				}
			}
		}
	}
}

// TestReadHeader_orphanCRLongRecord pins records a '\r' that is content makes
// whose proof runs far past any buffer — a 4096- and a 5000-byte joined line,
// a long chain of content '\r's, a joined line ended by a bare '\r' before
// its continuation, after colonless and invalid-name lines — under
// ReadHeader, whose proof is bounded by crLegacyCap, and under generous
// finite budgets, through every delivery and buffer size: the record keeps
// every byte at its location, the field after it is a field, and the body is
// left.
func TestReadHeader_orphanCRLongRecord(t *testing.T) {
	limits := HeaderLimits{MaxBytes: 65536, MaxFieldBytes: 49152, MaxLineBytes: 49152, MaxCount: 100}
	for _, tc := range []struct{ name, head, joined string }{
		{"4096 joined bytes", "broken\r", "b " + strings.Repeat("c", 4094) + "\r\n z\r\n"},
		{"5000 joined bytes", "broken\r", "b " + strings.Repeat("c", 4998) + "\r\n z\r\n"},
		{"an invalid name", "Bad Name: v\r", "b " + strings.Repeat("c", 4998) + "\r\n z\r\n"},
		{"a long chain", "broken\r", strings.Repeat("b c\r", 1500) + "d\r\n z\r\n"},
		{"a bare cr before the continuation", "broken\r", "b " + strings.Repeat("c", 4998) + "\r z\r\n"},
		{"a joined line of 8-bit and nul bytes", "broken\r", "b " + strings.Repeat("\xff\x00", 2500) + "\r\n\tz\r\n"},
	} {
		record := tc.head + tc.joined
		in := "A: 1\r\n" + record + "X: 1\r\n\r\nBODY"
		for name, newReader := range lineEndingReaders(in) {
			for api, read := range map[string]func(*bufio.Reader) (Header, error){
				"ReadHeader":       ReadHeader,
				"ReadHeaderLimits": func(r *bufio.Reader) (Header, error) { return ReadHeaderLimits(r, limits) },
			} {
				label := fmt.Sprintf("%s/%s/%s", tc.name, name, api)
				br := newReader()
				h, err := read(br)
				want := []recordOutcome{{bytes: record, start: 6, end: 6 + int64(len(record)), lines: strings.Count(record, "\n") + strings.Count(tc.joined, "\r ")}}
				if err != nil || fmt.Sprint(recordsOf(&h)) != fmt.Sprint(want) || h.Get("A") != "1" || h.Get("X") != "1" || h.Len() != 2 {
					t.Errorf("%s: (%d fields, %v), records %.80q", label, h.Len(), err, recordBytes(recordsOf(&h)))
				}
				if rest, rerr := io.ReadAll(br); string(rest) != "BODY" || rerr != nil {
					t.Errorf("%s: rest (%.40q, %v), want the body", label, rest, rerr)
				}
			}
		}
	}
}

// TestReadHeader_orphanCRBudgets pins the bound of the proof against each
// budget, and the difference between a proof that does not fit — the damaged
// line's old error, everything past its '\r' left unread — and a record the
// proof admits whose later bytes a budget refuses as it reads them, which is
// that budget's refusal. The joined line's content is held to MaxLineBytes
// less the damaged line and the '\r' that becomes content, its line ending
// not counted; its bytes and line ending to what MaxFieldBytes, MaxBytes and
// the shared budget have left; the byte after them that proves the
// continuation to none. A spent shared count refuses before anything is
// looked at, record or not. Nothing the proof looks at is charged; a record is
// charged once, as it is read.
func TestReadHeader_orphanCRBudgets(t *testing.T) {
	// The damaged line "broken\r" (7 bytes, content 6), the joined line
	// "b c\r\n" (content 3, 5 bytes), the continuation " z\r\n", the empty
	// line: a block of 18 bytes and one candidate.
	const block = "broken\rb c\r\n z\r\n\r\n"
	const record = "broken\rb c\r\n z\r\n"
	const old = "message: malformed MIME header line: broken\r\n"
	ok := func(name string, l HeaderLimits) {
		t.Helper()
		h, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block+"body")), l)
		if err != nil || fmt.Sprint(recordBytes(recordsOf(&h))) != fmt.Sprint([]string{record}) {
			t.Errorf("%s: (%+v, %v), want the record", name, recordsOf(&h), err)
		}
	}
	unproved := func(name string, l HeaderLimits) {
		t.Helper()
		br := bufio.NewReader(strings.NewReader(block + "body"))
		h, err := ReadHeaderLimits(br, l)
		if _, isLimit := HeaderLimit(err); isLimit || err == nil || err.Error() != old || h.OrphanContinuations() != nil {
			t.Errorf("%s: (%+v, %v), want the old error and no record", name, recordsOf(&h), err)
		}
		if rest, _ := io.ReadAll(br); string(rest) != block[7:]+"body" {
			t.Errorf("%s: left %q, want everything after the cr", name, rest)
		}
	}
	refused := func(name string, l HeaderLimits, kind HeaderLimitKind, at int64) {
		t.Helper()
		h, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block+"body")), l)
		e, isLimit := HeaderLimit(err)
		if !isLimit || e.Kind() != kind || e.UnitOffset() != at {
			t.Errorf("%s: err = %v, want a %v refusal at %d", name, err, kind, at)
		}
		if h.OrphanContinuations() != nil || h.Len() != 0 {
			t.Errorf("%s: a refused block kept %+v", name, recordsOf(&h))
		}
	}
	ok("exact", HeaderLimits{MaxBytes: 18, MaxLineBytes: 10, MaxFieldBytes: 16, MaxCount: 1})
	ok("max int", HeaderLimits{MaxBytes: math.MaxInt64, MaxLineBytes: math.MaxInt64, MaxFieldBytes: math.MaxInt64, MaxCount: math.MaxInt64,
		Total: NewHeaderBudget(math.MaxInt64, math.MaxInt64)})

	// The line: 10 admits the joined line's content (3 after 6 and the '\r'),
	// its line ending and the proof; 9 does not.
	unproved("line one less", HeaderLimits{MaxLineBytes: 9})

	// The field and the block: the proof needs 5 bytes after the 7 read.
	unproved("field room 4", HeaderLimits{MaxFieldBytes: 11})
	refused("field room 5, the continuation over", HeaderLimits{MaxFieldBytes: 12}, HeaderLimitFieldBytes, 0)
	refused("field one less", HeaderLimits{MaxFieldBytes: 15}, HeaderLimitFieldBytes, 0)
	unproved("block room 4", HeaderLimits{MaxBytes: 11})
	refused("block room 5, the rest over", HeaderLimits{MaxBytes: 12}, HeaderLimitBytes, 0)
	refused("block one less", HeaderLimits{MaxBytes: 17}, HeaderLimitBytes, 0)

	// The shared bytes alike.
	unproved("shared room 4", HeaderLimits{Total: NewHeaderBudget(11, 0)})
	refused("shared room 5, the rest over", HeaderLimits{Total: NewHeaderBudget(12, 0)}, HeaderLimitTotalBytes, 12)
	ok("shared exact", HeaderLimits{Total: NewHeaderBudget(18, 2)})
	shared := NewHeaderBudget(18, 2)
	if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block)), HeaderLimits{Total: shared}); err != nil {
		t.Fatal(err)
	}
	if shared.usedBytes != 18 || shared.usedCount != 1 {
		t.Errorf("the shared budget was charged (%d, %d), want (18, 1)", shared.usedBytes, shared.usedCount)
	}

	// A spent shared count refuses at the '\r''s next byte, before any look,
	// a record or not.
	refused("shared count", HeaderLimits{Total: NewHeaderBudget(0, 1)}, HeaderLimitTotalCount, 7)
	for _, in := range []string{"broken\rNext: value\r\n\r\n", "broken\r" + strings.Repeat("a", 5000) + ": v\r\n\r\n"} {
		br := bufio.NewReaderSize(strings.NewReader(in), 16)
		_, err := ReadHeaderLimits(br, HeaderLimits{Total: NewHeaderBudget(0, 1)})
		if e, isLimit := HeaderLimit(err); !isLimit || e.Kind() != HeaderLimitTotalCount || e.UnitOffset() != 7 || br.Size() != 16 || br.Buffered() > 16 {
			t.Errorf("%.20q: err = %v, size %d, want the count's refusal before any look", in, err, br.Size())
		}
	}

	// What the proof looks at and leaves is charged nothing: the damaged line
	// alone is charged before its error, however long the look.
	for _, in := range []string{"broken\rNext: value\r\n\r\n", "broken\r" + strings.Repeat("a", 5000) + ": v\r\n\r\n"} {
		sharedNext := NewHeaderBudget(1<<20, 0)
		br := bufio.NewReaderSize(strings.NewReader(in), 16)
		if _, err := ReadHeaderLimits(br, HeaderLimits{Total: sharedNext}); err == nil || sharedNext.usedBytes != int64(len("broken\r")) {
			t.Errorf("%.20q: (%v), %d shared bytes charged, want the error and %d", in, err, sharedNext.usedBytes, len("broken\r"))
		}
		if rest, _ := io.ReadAll(br); string(rest) != in[7:] {
			t.Errorf("%.20q: left %.40q", in, rest)
		}
	}

	// With no byte budget the proof is bounded by crLegacyCap.
	for _, tc := range []struct {
		n      int
		record bool
	}{{crLegacyCap - 3, true}, {crLegacyCap, false}} {
		in := "broken\rb" + strings.Repeat("c", tc.n-1) + "\r\n z\r\n\r\nBODY"
		br := bufio.NewReaderSize(strings.NewReader(in), 16)
		h, err := ReadHeader(br)
		if got := h.OrphanContinuations() != nil; got != tc.record || (tc.record && err != nil) || (!tc.record && (err == nil || err.Error() != old)) {
			t.Errorf("cap, %d content bytes: record %v, err %v; want record %v", tc.n, got, err, tc.record)
		}
		if rest, _ := io.ReadAll(br); !tc.record && string(rest) != in[7:] {
			t.Errorf("cap, %d content bytes: left %d bytes, want %d", tc.n, len(rest), len(in)-7)
		}
	}
}

// TestReadHeader_orphanCRRestoreReader pins what a caller's reader is after a
// proof that looked past its buffer did not prove a record: everything after
// the '\r' is still in it, in order, with a failure its source had not yet
// returned still to come; its buffer grew, at least twice; and reading such
// headers from it again and again — hundreds — grows it no further once a
// proof fits, so what it holds does not grow with the number of headers.
func TestReadHeader_orphanCRRestoreReader(t *testing.T) {
	const old = "message: malformed MIME header line: broken\r\n"
	tail := strings.Repeat("a", 5000) + ": v\r\n\r\n"
	br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("broken\r"+tail, 300)+"END"), 16)
	var size int
	for i := range 300 {
		h, err := ReadHeader(br)
		if err == nil || err.Error() != old || h.Len() != 0 {
			t.Fatalf("header %d: (%d fields, %v), want the old error", i, h.Len(), err)
		}
		next, err := ReadHeader(br)
		if err != nil || next.Get(strings.Repeat("a", 5000)) != "v" {
			t.Fatalf("header %d: the field after the cr was lost: (%v, %v)", i, next.Map(), err)
		}
		if i == 0 {
			size = br.Size()
			if size < 32 || size < 5000 || size > 2*(5000+16+16) {
				t.Errorf("after the first restore the buffer is %d bytes", size)
			}
		} else if br.Size() != size {
			t.Fatalf("header %d: the buffer is %d bytes, was %d: it grew with the number of headers", i, br.Size(), size)
		}
	}
	if rest, err := io.ReadAll(br); string(rest) != "END" || err != nil {
		t.Errorf("rest (%q, %v)", rest, err)
	}

	// A failure the source has not returned yet survives the restore, after
	// every byte, with the data or on a read of its own, and the source is not
	// read again by the header.
	text := "broken\r" + tail
	for _, withData := range []bool{false, true} {
		for _, sticky := range []bool{false, true} {
			src := &failAtSource{data: text, cut: len(text), chunk: 4096, err: errEIO, withData: withData, sticky: sticky}
			br := bufio.NewReaderSize(src, 16)
			if _, err := ReadHeader(br); err == nil || err.Error() != old || src.readsAfter != 0 {
				t.Errorf("with %v/sticky %v: err %v after %d reads past the failure", withData, sticky, err, src.readsAfter)
			}
			if rest, err := io.ReadAll(br); string(rest) != tail || err != errEIO {
				t.Errorf("with %v/sticky %v: rest (%d bytes, %v), want %d and the failure", withData, sticky, len(rest), err, len(tail))
			}
		}
	}

	// A failure met by the look itself is returned as itself, and what was
	// looked at is still in the reader before it.
	for _, cut := range []int{7 + 1, 7 + 100, 7 + 4096, 7 + 4097, len(text) - 10} {
		for _, sticky := range []bool{false, true} {
			src := &failAtSource{data: text, cut: cut, chunk: 7, err: errEIO, sticky: sticky}
			br := bufio.NewReaderSize(src, 16)
			if _, err := ReadHeader(br); err != errEIO || src.readsAfter != 0 {
				t.Errorf("cut %d/sticky %v: err %v after %d reads past the failure, want the failure", cut, sticky, err, src.readsAfter)
			}
			rest, _ := io.ReadAll(br)
			if !strings.HasPrefix(text[7:], string(rest)) || len(rest) < cut-7 {
				t.Errorf("cut %d/sticky %v: left %d bytes, want at least the %d looked at", cut, sticky, len(rest), cut-7)
			}
		}
	}
}

// TestMultipartReader_orphanCRLong pins proofs past the multipart reader's
// buffer in a part's header: a long record is the part's, its body where it
// is; a long line that only begins like a delimiter, or is padded far and then
// not one, joins as any line; a real delimiter right after the '\r', padded
// past the buffer, is no record — the damaged line is its old error and the
// delimiter is left for the next part; a long field after the '\r' is left
// too, and asking again reaches the sibling, at its offset. A hundred such
// parts are read with the reader's own buffer, whatever was given back.
func TestMultipartReader_orphanCRLong(t *testing.T) {
	budget := HeaderLimits{MaxBytes: 65536, MaxFieldBytes: 49152, MaxLineBytes: 49152}
	malformed := "other *errors.errorString"
	long := strings.Repeat("c", 5000)
	pad := strings.Repeat(" ", 5000)
	last := "--B\r\nSubject: last\r\n\r\nTHREE\r\n--B--\r\n"
	lastStep := orphanStep(nil, []wantField{{"Subject", "last", "Subject: last\r\n"}}, false, "THREE", "")
	for _, tc := range []struct {
		name  string
		in    string
		steps []string
	}{
		{"a long record", "--B\r\nbroken\rb " + long + "\r\n z\r\nX: 1\r\n\r\nBODY\r\n" + last,
			[]string{orphanStep([]string{"broken\rb " + long + "\r\n z\r\n"}, []wantField{{"X", "1", "X: 1\r\n"}}, false, "BODY", ""), lastStep, "EOF"}},
		{"a line that begins like a delimiter", "--B\r\nbroken\r--Bx" + long + "\r\n z\r\n\r\nBODY\r\n" + last,
			[]string{orphanStep([]string{"broken\r--Bx" + long + "\r\n z\r\n"}, nil, false, "BODY", ""), lastStep, "EOF"}},
		{"padding then no delimiter", "--B\r\nbroken\r--B" + pad + "x\r\n z\r\n\r\nBODY\r\n" + last,
			[]string{orphanStep([]string{"broken\r--B" + pad + "x\r\n z\r\n"}, nil, false, "BODY", ""), lastStep, "EOF"}},
		{"a real delimiter", "--B\r\nbroken\r--B\r\n z: x\r\n\r\nNEXT\r\n--B--\r\n",
			[]string{malformed, orphanStep(nil, nil, false, "NEXT", ""), "EOF"}},
		{"a real delimiter padded past the buffer", "--B\r\nbroken\r--B" + pad + "\r\n z: x\r\n\r\nNEXT\r\n--B--\r\n",
			[]string{malformed, orphanStep(nil, nil, false, "NEXT", ""), "EOF"}},
		{"a real closing delimiter", "--B\r\nbroken\r--B--\r\n z\r\n",
			[]string{malformed, "EOF"}},
		{"a long field", "--B\r\nbroken\r" + strings.Repeat("a", 5000) + ": v\r\n\r\nBODY\r\n" + last,
			[]string{malformed, lastStep, "EOF"}},
	} {
		for _, chunk := range []int{1, 7, 4096} {
			if got := orphanSteps(t, &chunkReader{s: tc.in, n: chunk}, "B", budget, 5); fmt.Sprint(got) != fmt.Sprint(tc.steps) {
				t.Errorf("%s/chunk %d:\n got %.300q\nwant %.300q", tc.name, chunk, got, tc.steps)
			}
		}
	}

	// A hundred parts whose damaged header is given back, each followed by a
	// healthy one at its own offset.
	var b strings.Builder
	var at []int64
	for i := range 100 {
		b.WriteString("--B\r\nbroken\r")
		b.WriteString(strings.Repeat("a", 5000))
		b.WriteString(": v\r\n\r\nx\r\n--B\r\n")
		at = append(at, int64(b.Len()))
		fmt.Fprintf(&b, "N: %d\r\n\r\nbody %d\r\n", i, i)
	}
	b.WriteString("--B--\r\n")
	mr := NewMultipartReader(strings.NewReader(b.String()), "B")
	mr.SetHeaderLimits(budget)
	for i := range 100 {
		if p, err := mr.NextPart(); p != nil || err == nil {
			t.Fatalf("part %d: (%v, %v), want the damaged header's error", i, p != nil, err)
		}
		p, err := mr.NextPart()
		if err != nil || p.Header.Get("N") != fmt.Sprint(i) || p.HeaderOffset() != at[i] {
			t.Fatalf("part %d: (%v, %v), want N %d at %d", i, p != nil, err, i, at[i])
		}
		if body, _ := io.ReadAll(p); string(body) != fmt.Sprintf("body %d", i) {
			t.Errorf("part %d: body %q", i, body)
		}
		if mr.bufReader.Size() != peekBufferSize {
			t.Fatalf("part %d: the multipart reader's buffer is %d bytes", i, mr.bufReader.Size())
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("end: %v", err)
	}
}

// TestReadHeader_orphanCRLookFailure cuts the input at every byte of the look
// past a damaged line's '\r', with sticky and one-shot failures: a failure met
// before the look decides is returned as itself — the continuation unknown —
// and nothing is kept of the damaged line or read after the failure; once the
// look has decided, a following field is the old error, and a record is read
// as its whole lines allow.
func TestReadHeader_orphanCRLookFailure(t *testing.T) {
	const field = "broken\rNext: value\r\n\r\n"
	const rec = "broken\rb c\r\n z\r\n\r\n"
	for _, tc := range []struct {
		text   string
		cut    int
		err    error // nil: the malformed line
		record string
	}{
		{field, len("broken\rN"), errEIO, ""},
		{field, len("broken\rNext"), errEIO, ""},
		{field, len("broken\rNext:"), nil, ""},
		{field, len(field), nil, ""},
		{rec, len("broken\rb"), errEIO, ""},
		{rec, len("broken\rb c\r"), errEIO, ""},
		{rec, len("broken\rb c\r\n"), errEIO, ""},
		{rec, len("broken\rb c\r\n "), errEIO, ""},
		{rec, len("broken\rb c\r\n z\r\n"), errEIO, "broken\rb c\r\n z\r\n"},
	} {
		for _, chunk := range []int{1, 4096} {
			for _, sticky := range []bool{true, false} {
				for _, withData := range []bool{false, true} {
					label := fmt.Sprintf("%q/cut %d/chunk %d/sticky %v/with %v", tc.text, tc.cut, chunk, sticky, withData)
					src := &failAtSource{data: tc.text, cut: tc.cut, chunk: chunk, err: errEIO, sticky: sticky, withData: withData}
					h, err := ReadHeader(bufio.NewReader(src))
					switch {
					case tc.err != nil && err != tc.err:
						t.Errorf("%s: err = %v, want the input's own failure", label, err)
					case tc.err == nil && (err == nil || err.Error() != "message: malformed MIME header line: broken\r\n"):
						t.Errorf("%s: err = %v, want the malformed line", label, err)
					}
					var want []string
					if tc.record != "" {
						want = []string{tc.record}
					}
					if got := recordBytes(recordsOf(&h)); fmt.Sprint(got) != fmt.Sprint(want) || h.Len() != 0 {
						t.Errorf("%s: records %q, %d fields, want %q", label, got, h.Len(), want)
					}
					if src.readsAfter != 0 {
						t.Errorf("%s: %d reads after the failure", label, src.readsAfter)
					}
				}
			}
		}
	}
}
