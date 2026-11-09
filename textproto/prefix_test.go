package textproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Malformed prefixes (ROB-020-001). Every source here is a finite string or a
// call-capped failing source; nothing is read past its cap.

// A prefixCase is one block and what reading it must yield: the prefix's exact
// bytes (empty for none), where it begins and how many lines it holds, the
// fields, and — when no error is expected — what is left in the reader. err is
// the prefix the error must have, or empty for none.
type prefixCase struct {
	name   string
	in     string
	prefix string
	start  int64
	lines  int
	fields []wantField
	rest   string
	err    string
}

func prefixCases() []prefixCase {
	subject := []wantField{{"Subject", "intact", "Subject: intact\r\n"}}
	x := []wantField{{"X", "1", "X: 1\r\n"}}
	return []prefixCase{
		{name: "a field shifted by a space", in: " Subject: shifted\r\nX-Keep: yes\r\n\r\nbody",
			prefix: " Subject: shifted\r\n", lines: 1, fields: []wantField{{"X-Keep", "yes", "X-Keep: yes\r\n"}}, rest: "body"},
		{name: "a field shifted by a tab", in: "\tSubject: shifted\r\nX-Keep: yes\r\n\r\nbody",
			prefix: "\tSubject: shifted\r\n", lines: 1, fields: []wantField{{"X-Keep", "yes", "X-Keep: yes\r\n"}}, rest: "body"},
		{name: "text without a colon", in: " orphan text\r\nSubject: intact\r\n\r\nbody",
			prefix: " orphan text\r\n", lines: 1, fields: subject, rest: "body"},
		{name: "two lines", in: " orphan text\r\n\tmore text\r\nSubject: intact\r\n\r\nbody",
			prefix: " orphan text\r\n\tmore text\r\n", lines: 2, fields: subject, rest: "body"},
		{name: "white space alone", in: " \t\r\nSubject: intact\r\n\r\nbody",
			prefix: " \t\r\n", lines: 1, fields: subject, rest: "body"},
		{name: "white space alone, then the empty line", in: " \t\r\n\r\nbody",
			prefix: " \t\r\n", lines: 1, rest: "body"},
		{name: "white space lines", in: " \r\n\t\r\n\r\nbody",
			prefix: " \r\n\t\r\n", lines: 2, rest: "body"},
		{name: "text at the end of the input", in: " orphan text", prefix: " orphan text", lines: 1},
		{name: "a field at the end of the input", in: " Subject: shifted", prefix: " Subject: shifted", lines: 1},
		{name: "white space at the end of the input", in: " \t ", prefix: " \t ", lines: 1},
		{name: "one line", in: " orphan\r\n", prefix: " orphan\r\n", lines: 1},
		{name: "lf", in: " orphan\nSubject: intact\n\nbody", prefix: " orphan\n", lines: 1, fields: subject, rest: "body"},
		{name: "cr", in: " orphan\rSubject: intact\r\rbody", prefix: " orphan\r", lines: 1, fields: subject, rest: "body"},
		{name: "a cr that is content joins the next line", in: " a\rb c\r\nX: 1\r\n\r\n", prefix: " a\rb c\r\n", lines: 1, fields: x},
		{name: "a cr that folds", in: " a\r\tb\r\nX: 1\r\n\r\n", prefix: " a\r\tb\r\n", lines: 2, fields: x},
		{name: "a cr that is content at the end of the input", in: " a\rb", prefix: " a\rb", lines: 1},
		{name: "a cr at the end of the input", in: " a\r", prefix: " a\r", lines: 1},
		{name: "mixed endings", in: " a\r\n\tb\n c\rX: 1\r\n\r\n", prefix: " a\r\n\tb\n c\r", lines: 3, fields: x},
		{name: "nul and 8-bit bytes", in: " X\x00: value\r\n \xff\xfe\x01\r\nSubject: intact\r\n\r\nbody",
			prefix: " X\x00: value\r\n \xff\xfe\x01\r\n", lines: 2, fields: subject, rest: "body"},
		{name: "an indented content type declares nothing",
			in:     " Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nkept\r\n--B--\r\n",
			prefix: " Content-Type: multipart/mixed; boundary=B\r\n", lines: 1, rest: "--B\r\n\r\nkept\r\n--B--\r\n"},
		{name: "an empty name after it", in: " p\r\n: x\r\nX: 1\r\n\r\n", prefix: " p\r\n", lines: 1, fields: x},
		{name: "a folded field after it", in: " p\r\nA: 1\r\n 2\r\n\r\n", prefix: " p\r\n", lines: 1,
			fields: []wantField{{"A", "1 2", "A: 1\r\n 2\r\n"}}},
		{name: "after a utf-8 mark", in: utf8Mark + " orphan\r\nSubject: intact\r\n\r\nbody",
			prefix: " orphan\r\n", start: 3, lines: 1, fields: subject, rest: "body"},
		{name: "a line without a colon after it", in: " orphan\r\nbody line\r\n\r\n",
			prefix: " orphan\r\n", lines: 1, err: "message: malformed MIME header line"},
		{name: "a bad key after it", in: " orphan\r\nbad key: v\r\n\r\n",
			prefix: " orphan\r\n", lines: 1, err: "message: malformed MIME header key"},

		// No prefix: these begin with no white space, or not on their first line.
		{name: "valid", in: "Subject: intact\r\n\r\nbody", fields: subject, rest: "body"},
		{name: "the empty line first", in: "\r\n Subject: body\r\n", rest: " Subject: body\r\n"},
		{name: "white space after a field folds it", in: "A: 1\r\n \r\nB: 2\r\n\r\n",
			fields: []wantField{{"A", "1", "A: 1\r\n \r\n"}, {"B", "2", "B: 2\r\n"}}},
	}
}

// TestReadHeader_malformedPrefix pins each case under every delivery and
// buffer size, through ReadHeader and ReadHeaderLimits: the prefix's bytes,
// location and lines, the fields, and what is left in the reader.
func TestReadHeader_malformedPrefix(t *testing.T) {
	for _, tc := range prefixCases() {
		if tc.prefix != "" && tc.in[tc.start:tc.start+int64(len(tc.prefix))] != tc.prefix {
			t.Fatalf("%s: the case's prefix is not its input's", tc.name)
		}
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
				if tc.err != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.err)) {
					t.Errorf("%s: err = %v, want %q...", label, err, tc.err)
					continue
				}
				want := prefixOutcome{}
				if tc.prefix != "" {
					want = prefixOutcome{present: true, bytes: tc.prefix, start: tc.start, end: tc.start + int64(len(tc.prefix)), lines: tc.lines}
				}
				if got := prefixOutcomeOf(&h); got != want {
					t.Errorf("%s: prefix %+v, want %+v", label, got, want)
				}
				if got := fieldsOf(t, h); !sameFields(got, tc.fields) {
					t.Errorf("%s: fields = %q, want %q", label, got, tc.fields)
				}
				if h.Len() != len(tc.fields) {
					t.Errorf("%s: Len() = %d, want %d: the prefix is no field", label, h.Len(), len(tc.fields))
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

// TestReadHeader_malformedPrefixIsNoField pins that nothing a prefix looks like
// is found by lookup: a shifted Subject or Content-Type is not the header's.
func TestReadHeader_malformedPrefixIsNoField(t *testing.T) {
	h := readBlock(t, " Subject: shifted\r\n Content-Type: multipart/mixed; boundary=B\r\nX: 1\r\n\r\n")
	for _, k := range []string{"Subject", "Content-Type", " Subject", ""} {
		if h.Has(k) || h.Get(k) != "" || h.Values(k) != nil {
			t.Errorf("%q is found in the header", k)
		}
	}
	if m := h.Map(); len(m) != 1 || m["X"] == nil {
		t.Errorf("Map() = %v, want X alone", m)
	}
	if h.Len() != 1 || h.FieldsByKey("Subject").Len() != 0 {
		t.Errorf("Len() = %d, want 1", h.Len())
	}
}

// TestMalformedPrefix_view pins the view: the zero value, a nil Header, a fresh
// copy from Bytes on every call, and the prefix kept by Copy and by every
// mutation while HeaderFromMap builds a header without it.
func TestMalformedPrefix_view(t *testing.T) {
	var zero MalformedPrefix
	if zero.Present() || zero.Bytes() != nil || zero.Lines() != 0 {
		t.Errorf("zero value: (%v, %q, %d)", zero.Present(), zero.Bytes(), zero.Lines())
	}
	if s, e := zero.Location(); s != 0 || e != 0 {
		t.Errorf("zero value: Location() = (%d, %d)", s, e)
	}
	var nilHeader *Header
	if nilHeader.MalformedPrefix().Present() {
		t.Error("a nil Header has a prefix")
	}
	plain := readBlock(t, "A: 1\r\n\r\n")
	if (&Header{}).MalformedPrefix().Present() || plain.MalformedPrefix().Present() {
		t.Error("a header without one has a prefix")
	}

	const block = " p\r\n\tq\r\nA: 1\r\nB: 2\r\n\r\n"
	const prefix = " p\r\n\tq\r\n"
	h := readBlock(t, block)
	p := h.MalformedPrefix()
	b := p.Bytes()
	if string(b) != prefix || p.Lines() != 2 {
		t.Fatalf("Bytes() = %q, Lines() = %d", b, p.Lines())
	}
	b[0] = 'x'
	if again := p.Bytes(); string(again) != prefix {
		t.Errorf("Bytes() after changing an earlier result = %q", again)
	}
	if again := h.MalformedPrefix().Bytes(); string(again) != prefix {
		t.Errorf("the header's prefix after changing a copy = %q", again)
	}

	check := func(what string, h Header) {
		t.Helper()
		if got := string(h.MalformedPrefix().Bytes()); got != prefix {
			t.Errorf("%s: prefix %q, want %q", what, got, prefix)
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
	check("the original after the copy changed", h)
	if h.Len() != 2 || h.Get("A") != "1" {
		t.Errorf("the original's fields changed with its copy's: %v", h.Map())
	}
	if rebuilt := HeaderFromMap(h.Map()); rebuilt.MalformedPrefix().Present() {
		t.Error("HeaderFromMap kept the prefix")
	}
}

// TestWriteHeader_malformedPrefix pins what is written: the prefix first, after
// any UTF-8 mark and before every field, those added since included; a complete
// CRLF block unchanged byte for byte; a last line without a line ending, or
// ended by a bare '\r', given one. Read back, the header is what was written,
// and writing it again writes the same bytes.
func TestWriteHeader_malformedPrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		mutate func(*Header)
		want   string
	}{
		{"complete crlf block", " a\r\n\tb\r\nB: 1\r\n\r\nbody", nil, " a\r\n\tb\r\nB: 1\r\n\r\n"},
		{"after a utf-8 mark", utf8Mark + " a\r\nB: 1\r\n\r\n", nil, utf8Mark + " a\r\nB: 1\r\n\r\n"},
		{"nul and 8-bit bytes", " \x00\xff\r\nB: 1\r\n\r\n", nil, " \x00\xff\r\nB: 1\r\n\r\n"},
		{"lf", " a\nB: 1\n\nbody", nil, " a\nB: 1\r\n\r\n"},
		{"cr", " a\rB: 1\r\rbody", nil, " a\r\nB: 1\r\n\r\n"},
		{"a cr that is content", " a\rb c\r\nB: 1\r\n\r\n", nil, " a\rb c\r\nB: 1\r\n\r\n"},
		{"no line ending at the end of the input", " a", nil, " a\r\n\r\n"},
		{"a cr at the end of the input", " a\r", nil, " a\r\n\r\n"},
		{"prefix alone", " a\r\n\r\n", nil, " a\r\n\r\n"},
		{"a field added", " a\r\nB: 1\r\n\r\n", func(h *Header) { h.Add("X", "y") }, " a\r\nX: y\r\nB: 1\r\n\r\n"},
		{"a raw field added", " a\r\nB: 1\r\n\r\n", func(h *Header) { h.AddRaw([]byte("X: y\r\n")) }, " a\r\nX: y\r\nB: 1\r\n\r\n"},
		{"a field set", " a\r\nB: 1\r\n\r\n", func(h *Header) { h.Set("B", "2") }, " a\r\nB: 2\r\n\r\n"},
		{"every field deleted", " a\r\nB: 1\r\n\r\n", func(h *Header) { h.Del("B") }, " a\r\n\r\n"},
		{"a field added to an unterminated prefix", " a", func(h *Header) { h.Add("X", "y") }, " a\r\nX: y\r\n\r\n"},
		{"a field added after a cr", " a\rB: 1\r\r", func(h *Header) { h.Add("X", "y") }, " a\r\nX: y\r\nB: 1\r\n\r\n"},
	} {
		h := readBlock(t, tc.in)
		before := string(h.MalformedPrefix().Bytes())
		if tc.mutate != nil {
			tc.mutate(&h)
		}
		got := writtenHeader(t, h)
		if got != tc.want {
			t.Errorf("%s: WriteHeader() = %q, want %q", tc.name, got, tc.want)
			continue
		}
		if after := string(h.MalformedPrefix().Bytes()); after != before {
			t.Errorf("%s: writing changed the prefix's evidence to %q", tc.name, after)
		}
		again := readBlock(t, got)
		if p := string(again.MalformedPrefix().Bytes()); p != writtenPrefix(before) {
			t.Errorf("%s: read back, the prefix is %q, want %q", tc.name, p, writtenPrefix(before))
		}
		if !sameFields(fieldsOf(t, again), fieldsOf(t, h)) {
			t.Errorf("%s: read back, the fields are %q, want %q", tc.name, fieldsOf(t, again), fieldsOf(t, h))
		}
		if second := writtenHeader(t, again); second != got {
			t.Errorf("%s: written again = %q, want %q", tc.name, second, got)
		}
	}
}

// errShortWrite is what failAfterWriter fails with.
var errShortWrite = errors.New("test writer: no room")

// failAfterWriter accepts n bytes and then fails, writing what fits.
type failAfterWriter struct {
	n   int
	buf bytes.Buffer
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if len(p) <= w.n {
		w.n -= len(p)
		return w.buf.Write(p)
	}
	k := w.n
	w.n = 0
	w.buf.Write(p[:k])
	return k, errShortWrite
}

// TestWriteHeader_malformedPrefixWriteFailure pins that a writer failing at any
// byte, inside the mark, the prefix, the line ending given to it or a field,
// fails WriteHeader with its own error after the bytes it took, and that the
// header is not changed.
func TestWriteHeader_malformedPrefixWriteFailure(t *testing.T) {
	for _, in := range []string{utf8Mark + " a\r\nB: 1\r\n\r\n", " a", " a\rB: 1\r\r"} {
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

// TestReadHeader_malformedPrefixBudgets pins the budgets: the prefix is one
// logical candidate, every line and byte of it charged as it is read, and the
// field counts charged once for it; a block refused keeps no prefix and no
// field.
func TestReadHeader_malformedPrefixBudgets(t *testing.T) {
	const block = " aaaa\r\n bbbb\r\nA: 1\r\n\r\n" // prefix 14 bytes, lines of 5, block 22
	ok := func(name string, l HeaderLimits) {
		t.Helper()
		h, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block+"body")), l)
		if err != nil || string(h.MalformedPrefix().Bytes()) != " aaaa\r\n bbbb\r\n" || h.Get("A") != "1" {
			t.Errorf("%s: (%q, %v), want the prefix and A", name, h.MalformedPrefix().Bytes(), err)
		}
	}
	refused := func(name string, l HeaderLimits, kind HeaderLimitKind, at int64) {
		t.Helper()
		h, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block+"body")), l)
		e, isLimit := HeaderLimit(err)
		if !isLimit || e.Kind() != kind || e.UnitOffset() != at {
			t.Errorf("%s: err = %v, want a %v refusal at %d", name, err, kind, at)
		}
		if h.MalformedPrefix().Present() || h.Len() != 0 {
			t.Errorf("%s: a refused block kept (%q, %d fields)", name, h.MalformedPrefix().Bytes(), h.Len())
		}
	}
	ok("exact", HeaderLimits{MaxBytes: 22, MaxLineBytes: 5, MaxFieldBytes: 14, MaxCount: 2})
	refused("block bytes", HeaderLimits{MaxBytes: 21}, HeaderLimitBytes, 0)
	refused("line bytes", HeaderLimits{MaxLineBytes: 4}, HeaderLimitLineBytes, 0)
	refused("field bytes", HeaderLimits{MaxFieldBytes: 13}, HeaderLimitFieldBytes, 0)
	refused("count", HeaderLimits{MaxCount: 1}, HeaderLimitCount, 14)

	// The shared budget: bytes counted once each, the prefix counted as one
	// candidate, and a spent count refusing it at its first byte.
	ok("shared exact", HeaderLimits{Total: NewHeaderBudget(22, 2)})
	refused("shared bytes", HeaderLimits{Total: NewHeaderBudget(21, 0)}, HeaderLimitTotalBytes, 21)
	refused("shared count", HeaderLimits{Total: NewHeaderBudget(0, 1)}, HeaderLimitTotalCount, 14)
	spent := NewHeaderBudget(0, 1)
	if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("X: 1\r\n\r\n")), HeaderLimits{Total: spent}); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(strings.NewReader(block))
	h, err := ReadHeaderLimits(br, HeaderLimits{Total: spent})
	if e, isLimit := HeaderLimit(err); !isLimit || e.Kind() != HeaderLimitTotalCount || e.UnitOffset() != 0 || h.MalformedPrefix().Present() {
		t.Errorf("a spent shared count: (%q, %v), want its refusal at the prefix's first byte", h.MalformedPrefix().Bytes(), err)
	}
	if rest, _ := io.ReadAll(br); string(rest) != block {
		t.Errorf("a spent shared count read %q of the prefix", block[:len(block)-len(rest)])
	}
}

// TestReadHeader_malformedPrefixReadFailure cuts a block that begins with a
// prefix at every byte, at buffer sizes 16 and 4096 and three chunkings, with
// the failure delivered with the last bytes or after them, from sources that
// keep failing and from sources that resume. A failure other than io.EOF is
// returned as itself with every line read whole before it — the prefix's lines
// as the prefix, byte for byte — and a line it cut is dropped; nothing after
// it is read. io.EOF behaves as it does from a source that keeps returning it.
func TestReadHeader_malformedPrefixReadFailure(t *testing.T) {
	const block = " orphan\r\n\tmore text\r\nA: 1\r\nB: 2\r\n\r\n"
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
							results[sticky] = fmt.Sprintf("%q %q %v", writtenHeader(t, h), h.MalformedPrefix().Bytes(), err)
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
							if got := writtenHeader(t, h); got != kept+"\r\n" {
								t.Errorf("%s: kept %q, want the whole lines %q", name, got, kept+"\r\n")
							}
							if got, want := string(h.MalformedPrefix().Bytes()), leadingWhiteLines(kept); got != want {
								t.Errorf("%s: prefix %q, want %q", name, got, want)
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

// leadingWhiteLines returns the lines of kept, each ended by "\r\n", that come
// before the first one not beginning with a space or a tab.
func leadingWhiteLines(kept string) string {
	n := 0
	for n < len(kept) && (kept[n] == ' ' || kept[n] == '\t') {
		i := strings.Index(kept[n:], "\r\n")
		if i < 0 {
			break
		}
		n += i + 2
	}
	return kept[:n]
}

// TestReadHeader_malformedPrefixCRFailure pins a failure at a '\r' that ended
// a prefix's line and was still waiting for the next line to settle it: the
// failure cuts that line too, so nothing of it stays in the prefix. A '\r' io.EOF
// follows, or that the next line settled, is kept.
func TestReadHeader_malformedPrefixCRFailure(t *testing.T) {
	const text = " x\r\n a\rb c\r\nX: 1\r\n\r\n"
	for _, tc := range []struct {
		cut    int
		prefix string
	}{
		{len(" x\r\n a"), " x\r\n"},
		{len(" x\r\n a\r"), " x\r\n"},
		{len(" x\r\n a\rb"), " x\r\n"},
		{len(" x\r\n a\rb c\r"), " x\r\n"},
		{len(" x\r\n a\rb c\r\n"), " x\r\n a\rb c\r\n"},
	} {
		for _, chunk := range []int{1, 4096} {
			for _, sticky := range []bool{true, false} {
				src := &failAtSource{data: text, cut: tc.cut, chunk: chunk, err: errEIO, sticky: sticky}
				h, err := ReadHeader(bufio.NewReader(src))
				if err != errEIO || string(h.MalformedPrefix().Bytes()) != tc.prefix || src.readsAfter != 0 {
					t.Errorf("cut %d/chunk %d/sticky %v: (%q, %v) after %d reads past the failure, want (%q, the failure)",
						tc.cut, chunk, sticky, h.MalformedPrefix().Bytes(), err, src.readsAfter, tc.prefix)
				}
			}
		}
	}
}

// multipartPrefixInput is a multipart body whose first part's header is a
// malformed prefix of one line ended by eol, followed directly by a delimiter
// line ended by eol — the closing one when closing — and otherwise by a healthy
// part.
func multipartPrefixInput(boundary, eol string, closing bool) string {
	s := "--" + boundary + "\r\n orphan" + eol
	if closing {
		return s + "--" + boundary + "--" + eol
	}
	return s + "--" + boundary + eol + "Subject: healthy\r\n\r\nKEPT\r\n--" + boundary + "--\r\n"
}

// TestMultipartReader_malformedPrefixDelimiter pins that a delimiter line after
// a part header's malformed prefix ends that header rather than being read as
// a field of it: the part keeps its prefix, has no field and an empty body,
// reports the missing separator, and the next part — or the end — follows.
// Boundaries with a colon, every line ending, opening and closing delimiters
// and three chunkings are covered.
func TestMultipartReader_malformedPrefixDelimiter(t *testing.T) {
	for _, boundary := range []string{"B", "b:c"} {
		for _, eol := range []string{"\r\n", "\n", "\r"} {
			for _, closing := range []bool{false, true} {
				for _, chunk := range []int{1, 7, 4096} {
					in := multipartPrefixInput(boundary, eol, closing)
					label := fmt.Sprintf("%q/%q/close %v/chunk %d", boundary, eol, closing, chunk)
					mr := NewMultipartReader(&chunkReader{s: in, n: chunk}, boundary)
					p, err := mr.NextPart()
					if err != nil {
						t.Fatalf("%s: first NextPart() = %v", label, err)
					}
					if got := string(p.Header.MalformedPrefix().Bytes()); got != " orphan"+eol || p.Header.Len() != 0 || !p.HeaderSeparatorMissing() {
						t.Errorf("%s: first part (%q, %d fields, separator missing %v)", label, got, p.Header.Len(), p.HeaderSeparatorMissing())
					}
					if body, berr := io.ReadAll(p); len(body) != 0 || berr != nil {
						t.Errorf("%s: first body (%q, %v), want empty", label, body, berr)
					}
					if p.BodyOffset() != int64(len("--"+boundary+"\r\n orphan"+eol)) {
						t.Errorf("%s: body offset %d", label, p.BodyOffset())
					}
					p, err = mr.NextPart()
					if closing {
						if p != nil || err != io.EOF {
							t.Errorf("%s: after it (%v, %v), want io.EOF", label, p != nil, err)
						}
						continue
					}
					if err != nil || p.Header.Get("Subject") != "healthy" || p.HeaderSeparatorMissing() {
						t.Fatalf("%s: second NextPart() = (%v, %v), want the healthy part", label, p != nil, err)
					}
					if body, berr := io.ReadAll(p); string(body) != "KEPT" || berr != nil {
						t.Errorf("%s: second body (%q, %v)", label, body, berr)
					}
					if _, err := mr.NextPart(); err != io.EOF {
						t.Errorf("%s: end = %v", label, err)
					}
				}
			}
		}
	}
}

// TestMultipartReader_malformedPrefixDelimiterCases pins the delimiter guard's
// other shapes: after a field that followed the prefix; a separator that is
// there after all; a false candidate, read as before, with the next part still
// reached by asking again; a truncated delimiter at the end of the input; and a
// header without a prefix, which is not looked at (ROB-020-005).
func TestMultipartReader_malformedPrefixDelimiterCases(t *testing.T) {
	healthy := "--B\r\nSubject: healthy\r\n\r\nKEPT\r\n--B--\r\n"
	type step struct {
		prefix, subject, body string
		missing               bool
		err                   string // a prefix of the error, "EOF" for io.EOF itself
	}
	for _, tc := range []struct {
		name  string
		in    string
		steps []step
	}{
		{"a field before the delimiter", "--B\r\n orphan\r\nSubject: x\r\n" + healthy,
			[]step{{prefix: " orphan\r\n", subject: "x", missing: true}, {subject: "healthy", body: "KEPT"}, {err: "EOF"}}},
		{"the separator is there", "--B\r\n orphan\r\n\r\nbody\r\n" + healthy,
			[]step{{prefix: " orphan\r\n", body: "body"}, {subject: "healthy", body: "KEPT"}, {err: "EOF"}}},
		{"a false candidate", "--B\r\n orphan\r\n--Bx\r\n\r\nskipped\r\n" + healthy,
			[]step{{err: "message: malformed MIME header line"}, {subject: "healthy", body: "KEPT"}, {err: "EOF"}}},
		{"a truncated delimiter", "--B\r\n orphan\r\n--B",
			[]step{{prefix: " orphan\r\n", missing: true}, {err: "multipart: NextPart: EOF"}}},
		{"white space past the delimiter", "--B\r\n orphan\r\n--B \t \r\n" + strings.TrimPrefix(healthy, "--B\r\n"),
			[]step{{prefix: " orphan\r\n", missing: true}, {subject: "healthy", body: "KEPT"}, {err: "EOF"}}},
		{"no prefix", "--B\r\nSubject: x\r\n" + healthy,
			[]step{{err: "message: malformed MIME header line"}}},
	} {
		for _, chunk := range []int{1, 7, 4096} {
			label := fmt.Sprintf("%s/chunk %d", tc.name, chunk)
			mr := NewMultipartReader(&chunkReader{s: tc.in, n: chunk}, "B")
			for i, want := range tc.steps {
				p, err := mr.NextPart()
				if want.err != "" {
					if p != nil || err == nil || (want.err == "EOF") != (err == io.EOF) || !strings.HasPrefix(err.Error(), want.err) {
						t.Errorf("%s: step %d = (%v, %v), want %q", label, i, p != nil, err, want.err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: step %d = %v", label, i, err)
				}
				body, berr := io.ReadAll(p)
				if got := string(p.Header.MalformedPrefix().Bytes()); got != want.prefix || p.Header.Get("Subject") != want.subject ||
					p.HeaderSeparatorMissing() != want.missing || string(body) != want.body || berr != nil {
					t.Errorf("%s: step %d = (%q, %q, missing %v, %q, %v), want %+v", label, i, got, p.Header.Get("Subject"),
						p.HeaderSeparatorMissing(), body, berr, want)
				}
			}
		}
	}
}

// errCut is a source failure no part of the multipart reader can confuse with
// io.EOF.
var errCut = errEIO

// describeError names an error of the multipart tests by what it is, comparing
// identities and walking only standard wrappers: io.EOF itself, the source's
// failure itself, the failure wrapped, an end of input (io.EOF or
// io.ErrUnexpectedEOF wrapped), or anything else by its type.
func describeError(err error) string {
	switch {
	case err == nil:
		return ""
	case err == io.EOF:
		return "EOF"
	case err == errCut:
		return "failure"
	case errors.Is(err, errCut):
		return "failure wrapped"
	case errors.Is(err, bufio.ErrBufferFull):
		return "buffer full"
	case isEOFClass(err):
		return "end of input"
	}
	return fmt.Sprintf("other %T", err)
}

// multipartSteps iterates the multipart body in r, framed by boundary, for at
// most limit steps or until io.EOF, and describes each: a part by its prefix,
// fields, missing separator, body and how its body ended, or the error.
func multipartSteps(t *testing.T, r io.Reader, boundary string, limits HeaderLimits, steps int) []string {
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
		out = append(out, partStep(string(p.Header.MalformedPrefix().Bytes()), fieldsOf(t, p.Header), p.HeaderSeparatorMissing(), string(body), describeError(berr)))
	}
	return out
}

func partStep(prefix string, fields []wantField, missing bool, body, end string) string {
	return fmt.Sprintf("part prefix=%q fields=%q missing=%v body=%q end=%q", prefix, fields, missing, body, end)
}

// TestMultipartReader_malformedPrefixReadFailure cuts a multipart body — a part
// whose header is a malformed prefix ended by the next delimiter, a healthy
// part, the closing delimiter — at every byte, at three chunkings, with the
// failure delivered with the last bytes or after them, and holds every step of
// the iteration to what the cut says.
//
// An ordinary failure is held to a table derived from where the cut falls: a
// line it cuts is never read as a line, so the damaged part needs its whole
// delimiter line after the prefix; the healthy part needs its whole header; its
// body is every byte before the cut — a candidate delimiter the failure left
// undecided included — and ends with the failure joined with the missing close;
// the reader is never read after the failure. io.EOF at the cut is held to what
// the same bytes read to their natural end give, and pinned where it proves a
// closing delimiter or leaves one truncated.
func TestMultipartReader_malformedPrefixReadFailure(t *testing.T) {
	const (
		opening = "--B\r\n"
		prefix  = " orphan\r\n"
		delim   = "--B\r\n"
		header  = "Subject: healthy\r\n\r\n"
		content = "KEPT"
		closing = "\r\n--B--\r\n"
	)
	text := opening + prefix + delim + header + content + closing
	damaged := partStep(prefix, nil, true, "", "")
	healthyFields := []wantField{{"Subject", "healthy", "Subject: healthy\r\n"}}
	bodyAt := len(opening + prefix + delim + header)
	expect := func(cut int) []string {
		switch {
		case cut < len(opening):
			return []string{"failure wrapped", "failure wrapped", "failure wrapped", "failure wrapped"}
		case cut < len(opening+prefix+delim):
			return []string{"failure", "failure wrapped", "failure wrapped", "failure wrapped"}
		case cut < bodyAt:
			return []string{damaged, "failure", "failure wrapped", "failure wrapped"}
		case cut < len(text):
			return []string{damaged, partStep("", healthyFields, false, text[bodyAt:cut], "failure wrapped"), "failure wrapped", "failure wrapped"}
		}
		return []string{damaged, partStep("", healthyFields, false, content, ""), "EOF"}
	}
	for cut := 0; cut <= len(text); cut++ {
		for _, chunk := range []int{1, 7, 4096} {
			for _, withData := range []bool{false, true} {
				label := fmt.Sprintf("cut %d/chunk %d/with %v", cut, chunk, withData)
				src := &failAtSource{data: text, cut: cut, chunk: chunk, err: errCut, withData: withData, sticky: true}
				if got, want := multipartSteps(t, src, "B", HeaderLimits{}, 4), expect(cut); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s: failure:\n got %q\nwant %q", label, got, want)
				}
				if src.readsAfter != 0 {
					t.Errorf("%s: %d reads after the failure", label, src.readsAfter)
				}

				src = &failAtSource{data: text, cut: cut, chunk: chunk, err: io.EOF, withData: withData, sticky: true}
				got := multipartSteps(t, src, "B", HeaderLimits{}, 4)
				if natural := multipartSteps(t, &chunkReader{s: text[:cut], n: chunk}, "B", HeaderLimits{}, 4); fmt.Sprint(got) != fmt.Sprint(natural) {
					t.Errorf("%s: io.EOF:\n got %q\nwant what the bytes alone give, %q", label, got, natural)
				}
			}
		}
	}

	// io.EOF pinned: after the closing delimiter, with or without its line
	// ending, the iteration ends normally; a delimiter cut after its name is a
	// truncated one; anything shorter is the body's, which then lacks its close.
	healthy := func(body, end string) string { return partStep("", healthyFields, false, body, end) }
	for _, tc := range []struct {
		cut  int
		want []string
	}{
		{len(text), []string{damaged, healthy(content, ""), "EOF"}},
		{len(text) - 2, []string{damaged, healthy(content, ""), "EOF"}},
		{bodyAt + len(content+"\r\n--B"), []string{damaged, healthy(content, ""), "end of input", "EOF"}},
		{bodyAt + len(content+"\r\n--B-"), []string{damaged, healthy(content+"\r\n--B-", "end of input"), "end of input", "EOF"}},
		{len(opening + prefix + "--B"), []string{damaged, "end of input", "EOF"}},
	} {
		got := multipartSteps(t, &chunkReader{s: text[:tc.cut], n: 4096}, "B", HeaderLimits{}, 4)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("io.EOF at %d:\n got %q\nwant %q", tc.cut, got, tc.want)
		}
	}
}

// longPaddingInput is a multipart body whose first part's header is a
// malformed prefix followed by line — then a healthy part's header, its body
// and the closing delimiter, all framed by boundary.
func longPaddingInput(boundary, line string) string {
	return "--" + boundary + "\r\n orphan\r\n" + line + "\r\nSubject: kept\r\n\r\nBODY\r\n--" + boundary + "--\r\n"
}

// TestMultipartReader_malformedPrefixLongPadding pins a line after a part
// header's prefix that begins like a delimiter and whose white space runs to,
// across and past the reader's buffer. Under a line budget that admits it, a
// colon after the white space makes it the field it is — the line is read
// whole, the fields after it are the part's, and no missing separator is
// reported — while a line ending after the white space proves the delimiter,
// opening or closing, and the part after it is read; for a boundary with a
// colon too. With no line budget a line still undecided past one buffer is the
// multipart reader's own framing refusal, as it is anywhere in a multipart.
// A budget the line overflows refuses it as a header line.
func TestMultipartReader_malformedPrefixLongPadding(t *testing.T) {
	budget := HeaderLimits{MaxBytes: 16384, MaxFieldBytes: 12000, MaxLineBytes: 12000}
	kept := partStep("", []wantField{{"Subject", "kept", "Subject: kept\r\n"}}, false, "BODY", "")
	for _, boundary := range []string{"B", "b:c"} {
		dash := "--" + boundary
		for _, padding := range []int{0, 1, 16, 4090, 4092, 4093, 4094, 4096, 5000} {
			pad := strings.Repeat(" ", padding)
			mixed := strings.Repeat(" \t", padding/2) + strings.Repeat(" ", padding%2)
			for _, chunk := range []int{1, 7, 4096} {
				label := fmt.Sprintf("%q/padding %d/chunk %d", boundary, padding, chunk)
				run := func(line string, limits HeaderLimits) []string {
					return multipartSteps(t, &chunkReader{s: longPaddingInput(boundary, line), n: chunk}, boundary, limits, 4)
				}

				// A field that begins like a delimiter.
				line := dash + pad + ": value"
				key, value := "--B", "value"
				if boundary == "b:c" {
					key, value = "--B", "c"+pad+": value"
				}
				field := partStep(" orphan\r\n", []wantField{{key, value, line + "\r\n"}, {"Subject", "kept", "Subject: kept\r\n"}}, false, "BODY", "")
				if got, want := run(line, budget), []string{field, "EOF"}; fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s: field:\n got %q\nwant %q", label, got, want)
				}

				// A delimiter, opening and closing, padded with spaces or with
				// spaces and tabs.
				damaged := partStep(" orphan\r\n", nil, true, "", "")
				for _, p := range []string{pad, mixed} {
					if got, want := run(dash+p, budget), []string{damaged, kept, "EOF"}; fmt.Sprint(got) != fmt.Sprint(want) {
						t.Errorf("%s: opening delimiter:\n got %q\nwant %q", label, got, want)
					}
					if got, want := run(dash+"--"+p, budget), []string{damaged, "EOF"}; fmt.Sprint(got) != fmt.Sprint(want) {
						t.Errorf("%s: closing delimiter:\n got %q\nwant %q", label, got, want)
					}
				}

				// With no budget, a line decided within one buffer is decided as
				// above; one still undecided past it is the framing refusal.
				undecided := len(dash)+padding > 4096
				if got := run(line, HeaderLimits{}); undecided {
					if fmt.Sprint(got) != fmt.Sprint([]string{"buffer full", "EOF"}) {
						t.Errorf("%s: field, no budget: %q, want the framing refusal", label, got)
					}
				} else if fmt.Sprint(got) != fmt.Sprint([]string{field, "EOF"}) {
					t.Errorf("%s: field, no budget: %q", label, got)
				}
				if got := run(dash+pad, HeaderLimits{}); undecided {
					if fmt.Sprint(got) != fmt.Sprint([]string{"buffer full", "EOF"}) {
						t.Errorf("%s: delimiter, no budget: %q, want the framing refusal", label, got)
					}
				} else if len(dash)+padding < 4096-2 && fmt.Sprint(got) != fmt.Sprint([]string{damaged, kept, "EOF"}) {
					t.Errorf("%s: delimiter, no budget: %q", label, got)
				}
			}
		}

		// A line budget the white space overflows refuses the line as a header
		// line, where it begins, whatever follows it.
		for _, tail := range []string{": value", ""} {
			line := dash + strings.Repeat(" ", 5000) + tail
			mr := NewMultipartReader(strings.NewReader(longPaddingInput(boundary, line)), boundary)
			mr.SetHeaderLimits(HeaderLimits{MaxLineBytes: 4999})
			p, err := mr.NextPart()
			e, refused := HeaderLimit(err)
			if p != nil || !refused || e.Kind() != HeaderLimitLineBytes || e.UnitOffset() != int64(len(dash+"\r\n orphan\r\n")) {
				t.Errorf("%q/tail %q: NextPart() = (%v, %v), want the line budget's refusal at the line", boundary, tail, p != nil, err)
			}
		}
	}
}

// TestMultipartReader_malformedPrefixLongPaddingOffsets pins where a part whose
// header a long-padded delimiter ended begins its body, and that the field a
// long-padded line turned out to be is charged once: its block's bytes are the
// input's.
func TestMultipartReader_malformedPrefixLongPaddingOffsets(t *testing.T) {
	budget := HeaderLimits{MaxBytes: 16384, MaxFieldBytes: 12000, MaxLineBytes: 12000}
	line := "--B" + strings.Repeat(" ", 5000)
	mr := NewMultipartReader(strings.NewReader(longPaddingInput("B", line)), "B")
	mr.SetHeaderLimits(budget)
	p, err := mr.NextPart()
	if err != nil || !p.HeaderSeparatorMissing() || p.BodyOffset() != int64(len("--B\r\n orphan\r\n")) {
		t.Fatalf("NextPart() = (%v, %v), want the damaged part, its body at the delimiter line", p, err)
	}

	field := "--B" + strings.Repeat(" ", 5000) + ": value"
	block := " orphan\r\n" + field + "\r\nSubject: kept\r\n\r\n"
	shared := NewHeaderBudget(int64(len(block)), 0)
	limits := budget
	limits.Total = shared
	mr = NewMultipartReader(strings.NewReader(longPaddingInput("B", field)), "B")
	mr.SetHeaderLimits(limits)
	p, err = mr.NextPart()
	if err != nil || p.HeaderSeparatorMissing() || p.BodyOffset() != int64(len("--B\r\n")+len(block)) {
		t.Fatalf("NextPart() = (%v, %v), want the part with the field", p, err)
	}
	if shared.Refusal() != nil || shared.usedBytes != int64(len(block)) {
		t.Errorf("the shared budget was charged %d bytes, want the block's %d once", shared.usedBytes, len(block))
	}
	if le := p.Header.LineEndings(); le.Count(LineEndingCRLF) != 4 {
		t.Errorf("line endings %+v, want the block's four", endsOf(le))
	}
}

// TestMultipartReader_malformedPrefixLongPaddingFailure cuts a long-padded
// line after a part header's prefix inside and after its white space: an
// ordinary failure before the line is decided is returned as itself, the line
// it cut read as no field and the reader never read after it; io.EOF after the
// white space ends a truncated delimiter, as the bytes alone do.
func TestMultipartReader_malformedPrefixLongPaddingFailure(t *testing.T) {
	budget := HeaderLimits{MaxBytes: 16384, MaxFieldBytes: 12000, MaxLineBytes: 12000}
	text := longPaddingInput("B", "--B"+strings.Repeat(" ", 5000)+": value")
	start := len("--B\r\n orphan\r\n")
	for _, at := range []int{0, 1, 3, 100, 4093, 4096, 4097, 5003, 5004, 5010} {
		cut := start + at
		for _, chunk := range []int{1, 7, 4096} {
			for _, withData := range []bool{false, true} {
				label := fmt.Sprintf("cut %d/chunk %d/with %v", cut, chunk, withData)
				src := &failAtSource{data: text, cut: cut, chunk: chunk, err: errCut, withData: withData, sticky: true}
				if got, want := multipartSteps(t, src, "B", budget, 3), []string{"failure", "failure wrapped", "failure wrapped"}; fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s: %q, want %q", label, got, want)
				}
				if src.readsAfter != 0 {
					t.Errorf("%s: %d reads after the failure", label, src.readsAfter)
				}
				src = &failAtSource{data: text, cut: cut, chunk: chunk, err: io.EOF, withData: withData, sticky: true}
				got := multipartSteps(t, src, "B", budget, 3)
				if natural := multipartSteps(t, &chunkReader{s: text[:cut], n: chunk}, "B", budget, 3); fmt.Sprint(got) != fmt.Sprint(natural) {
					t.Errorf("%s: io.EOF %q, want %q", label, got, natural)
				}
			}
		}
	}
	got := multipartSteps(t, strings.NewReader(text[:start+5003]), "B", budget, 3)
	if want := []string{partStep(" orphan\r\n", nil, true, "", ""), "end of input", "EOF"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("io.EOF after the white space: %q, want the truncated delimiter %q", got, want)
	}
}

const prefixBufferFullEnv = "EMLKIT_TEXTPROTO_PREFIX_BUFFER_FULL"

// TestMalformedPrefix_sourceBufferFull pins a source that keeps failing with
// bufio.ErrBufferFull itself — the value the buffered reader also uses for a
// full buffer — inside a malformed prefix, at the look past it, inside a
// delimiter candidate after it and inside a long-padded one being held: that
// value is the source's failure, returned as itself, never read on. It runs in
// a subprocess with a deadline, as the other such probes do.
func TestMalformedPrefix_sourceBufferFull(t *testing.T) {
	if os.Getenv(prefixBufferFullEnv) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMalformedPrefix_sourceBufferFull$", "-test.count=1")
		cmd.Env = append(os.Environ(), prefixBufferFullEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("subprocess = %v, deadline %v\n%s", err, ctx.Err(), out)
		}
		return
	}

	const block = " orphan\r\n\tmore\r\nA: 1\r\n\r\n"
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
				if err != bufio.ErrBufferFull || string(h.MalformedPrefix().Bytes()) != leadingWhiteLines(kept) {
					t.Errorf("%s: (%q, %v), want %q and the source's own error", name, h.MalformedPrefix().Bytes(), err, leadingWhiteLines(kept))
				}
				if src.readsAfter != 0 {
					t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
				}
			}
		}
	}

	budget := HeaderLimits{MaxBytes: 16384, MaxFieldBytes: 12000, MaxLineBytes: 12000}
	start := len("--B\r\n orphan\r\n")
	for _, tc := range []struct {
		line   string
		proved int // the bytes of the line that prove a delimiter, or 0
	}{
		{"--B\r\nSubject: healthy", len("--B\r\n")},
		{"--B" + strings.Repeat(" ", 5000) + ": value", 0},
	} {
		text := longPaddingInput("B", tc.line)
		for _, at := range []int{-3, -1, 0, 1, 2, 3, 4, 5, 4093, 4096, 5003} {
			cut := start + at
			if cut > len(text) {
				continue
			}
			for _, chunk := range []int{1, 4096} {
				name := fmt.Sprintf("multipart %q/cut %d/chunk %d", tc.line[:5], cut, chunk)
				src := &failAtSource{data: text, cut: cut, chunk: chunk, err: bufio.ErrBufferFull, sticky: true}
				mr := NewMultipartReader(src, "B")
				mr.SetHeaderLimits(budget)
				p, err := mr.NextPart()
				if tc.proved > 0 && at >= tc.proved {
					// The delimiter after the prefix was proved before the
					// failure: the damaged part is returned.
					if err != nil || p == nil || !p.HeaderSeparatorMissing() {
						t.Errorf("%s: (%v, %v), want the damaged part", name, p != nil, err)
					}
				} else if p != nil || err != bufio.ErrBufferFull {
					// Inside the prefix, at the look past it, or inside a line
					// not yet decided: the header's failure, as itself.
					t.Errorf("%s: (%v, %v), want the source's own error", name, p != nil, err)
				}
				if src.readsAfter != 0 {
					t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
				}
			}
		}
	}
}

// FuzzMultipartReader_malformedPrefixDelimiter holds a line after a part
// header's malformed prefix — "--", a boundary with or without a colon, white
// space up to 6000 bytes, then up to 64 bytes of anything but a line ending —
// to an independent reading of the line, under a line budget that admits it:
// the part's separator is reported missing exactly when the line is a whole
// delimiter, opening or closing; then its body is empty and the healthy part,
// or the end, follows; otherwise the line is read as a line, and no part after
// it is reported to have a missing separator. Every step is the same read one,
// seven or 4096 bytes at a time. Nothing is decoded; every source is a finite
// string.
func FuzzMultipartReader_malformedPrefixDelimiter(f *testing.F) {
	for _, padding := range []uint16{0, 1, 4090, 4093, 4096, 5000} {
		for _, tail := range []string{"", ": value", "--", "\t", "x", " : v"} {
			f.Add(padding, byte(0), []byte(tail))
			f.Add(padding, byte(1), []byte(tail))
		}
	}
	f.Fuzz(func(t *testing.T, padding uint16, which byte, tail []byte) {
		if padding > 6000 || len(tail) > 64 || bytes.ContainsAny(tail, "\r\n") {
			return
		}
		boundary := "B"
		if which&1 == 1 {
			boundary = "b:c"
		}
		if which&2 == 2 && len(tail) > 0 {
			tail = append([]byte("--"), tail...)
		}
		dash := "--" + boundary
		line := dash + strings.Repeat(" ", int(padding)) + string(tail)
		rest := line[len(dash):]
		closing := strings.HasPrefix(rest, "--")
		if closing {
			rest = rest[2:]
		}
		isDelim := strings.Trim(rest, " \t") == ""
		budget := HeaderLimits{MaxBytes: 16384, MaxFieldBytes: 12000, MaxLineBytes: 12000}
		text := longPaddingInput(boundary, line)

		want := multipartSteps(t, strings.NewReader(text), boundary, budget, 4)
		for _, n := range []int{1, 7} {
			if got := multipartSteps(t, &chunkReader{s: text, n: n}, boundary, budget, 4); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("chunks of %d:\n got %q\nwant %q", n, got, want)
			}
		}
		damaged := partStep(" orphan\r\n", nil, true, "", "")
		kept := partStep("", []wantField{{"Subject", "kept", "Subject: kept\r\n"}}, false, "BODY", "")
		switch {
		case isDelim && closing:
			if fmt.Sprint(want) != fmt.Sprint([]string{damaged, "EOF"}) {
				t.Fatalf("closing delimiter %q: %q", line, want)
			}
		case isDelim:
			if fmt.Sprint(want) != fmt.Sprint([]string{damaged, kept, "EOF"}) {
				t.Fatalf("delimiter %q: %q", line, want)
			}
		default:
			for _, step := range want {
				if strings.Contains(step, "missing=true") {
					t.Fatalf("no delimiter %q, but a missing separator: %q", line, want)
				}
			}
		}
	})
}

// TestMultipartReader_malformedPrefixLongPaddingHeaderBudgets pins that a
// delimiter after a part header's prefix is no header content: under a framing
// line budget that admits it, a padded opening or closing delimiter — padded
// within the buffer or past it — is proved however little the header's own
// budgets have left, block, field, shared bytes and shared count each exactly
// spent by the header around it, and the healthy part after it, or the end, is
// read; the shared budget is charged the header blocks alone. A field padded
// the same way is charged once, as the line it is, and refused by the header
// budget it overflows, where that budget says.
func TestMultipartReader_malformedPrefixLongPaddingHeaderBudgets(t *testing.T) {
	const prefix = " orphan\r\n"
	const sibling = "Subject: kept\r\n\r\n"
	type budgets struct {
		name  string
		limit func(closing bool) HeaderLimits
	}
	line := func(l HeaderLimits) HeaderLimits { l.MaxLineBytes = 12000; return l }
	pick := func(closing bool, open, closed int64) int64 {
		if closing {
			return closed
		}
		return open
	}
	for _, b := range []budgets{
		{"block", func(c bool) HeaderLimits {
			return line(HeaderLimits{MaxBytes: pick(c, int64(len(sibling)), int64(len(prefix)))})
		}},
		{"field", func(c bool) HeaderLimits {
			return line(HeaderLimits{MaxFieldBytes: pick(c, int64(len("Subject: kept\r\n")), int64(len(prefix)))})
		}},
		{"shared bytes", func(c bool) HeaderLimits {
			return line(HeaderLimits{Total: NewHeaderBudget(pick(c, int64(len(prefix+sibling)), int64(len(prefix))), 0)})
		}},
		{"shared count", func(c bool) HeaderLimits { return line(HeaderLimits{Total: NewHeaderBudget(0, pick(c, 2, 1))}) }},
	} {
		for _, boundary := range []string{"B", "b:c"} {
			dash := "--" + boundary
			for _, padding := range []int{0, 16, 4093, 5000} {
				pad := strings.Repeat(" ", padding)
				for _, chunk := range []int{1, 7, 4096} {
					label := fmt.Sprintf("%s/%q/padding %d/chunk %d", b.name, boundary, padding, chunk)
					damaged := partStep(prefix, nil, true, "", "")
					kept := partStep("", []wantField{{"Subject", "kept", "Subject: kept\r\n"}}, false, "BODY", "")
					for _, closing := range []bool{false, true} {
						l := b.limit(closing)
						delim, want := dash+pad, []string{damaged, kept, "EOF"}
						if closing {
							delim, want = dash+"--"+pad, []string{damaged, "EOF"}
						}
						got := multipartSteps(t, &chunkReader{s: longPaddingInput(boundary, delim), n: chunk}, boundary, l, 4)
						if fmt.Sprint(got) != fmt.Sprint(want) {
							t.Errorf("%s/close %v:\n got %q\nwant %q", label, closing, got, want)
						}
						if tot := l.Total; tot != nil {
							wantBytes, wantCount := int64(len(prefix+sibling)), int64(2)
							if closing {
								wantBytes, wantCount = int64(len(prefix)), 1
							}
							if tot.Refusal() != nil || (tot.maxBytes > 0 && tot.usedBytes != wantBytes) || (tot.maxCount > 0 && tot.usedCount != wantCount) {
								t.Errorf("%s/close %v: shared budget used (%d, %d), refused %v; want the header blocks alone", label, closing, tot.usedBytes, tot.usedCount, tot.Refusal())
							}
						}
					}
				}
			}

			// A field padded alike overflows the header budget that is left.
			base := int64(len(dash + "\r\n"))
			for _, padding := range []int{40, 4093, 5000} {
				field := dash + strings.Repeat(" ", padding) + ": value"
				for _, chunk := range []int{1, 4096} {
					l := b.limit(false)
					kind, at := HeaderLimitBytes, base
					switch b.name {
					case "field":
						kind, at = HeaderLimitFieldBytes, base+int64(len(prefix))
					case "shared bytes":
						kind, at = HeaderLimitTotalBytes, base+int64(len(prefix+sibling))
					case "shared count":
						l.Total = NewHeaderBudget(0, 1)
						kind, at = HeaderLimitTotalCount, base+int64(len(prefix))
					}
					mr := NewMultipartReader(&chunkReader{s: longPaddingInput(boundary, field), n: chunk}, boundary)
					mr.SetHeaderLimits(l)
					p, err := mr.NextPart()
					e, refused := HeaderLimit(err)
					if p != nil || !refused || e.Kind() != kind || e.UnitOffset() != at {
						t.Errorf("%s/%q/field padding %d/chunk %d: NextPart() = (%v, %v), want a %v refusal at %d",
							b.name, boundary, padding, chunk, p != nil, err, kind, at)
					}
				}
			}
		}
	}
}
