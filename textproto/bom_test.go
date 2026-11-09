package textproto

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Byte order marks at the start of a header block (ROB-010-003). Every source
// is a finite string or a call-capped failing source; the one probe of a source
// failing with bufio.ErrBufferFull runs in a subprocess with a deadline.

const utf8Mark = "\xef\xbb\xbf"

// A byteOrderMarkCase is one block and everything reading it must yield: the
// mark, the fields with their raw bytes, and — when no error is expected — what
// is left in the reader and the line endings (nil for an unknown report). err
// is the prefix the error must have, or empty for none.
type byteOrderMarkCase struct {
	name   string
	in     string
	bom    ByteOrderMark
	fields []wantField
	rest   string
	ends   *wantEnds
	err    string
}

func byteOrderMarkCases() []byteOrderMarkCase {
	none := int64(-1)
	ends := func(w wantEnds) *wantEnds { return &w }
	a := []wantField{{"A", "1", "A: 1\r\n"}}
	ab := []wantField{{"A", "1", "A: 1\r\n"}, {"B", "2", "B: 2\r\n"}}
	const (
		malformedKey  = "message: malformed MIME header key"
		malformedLine = "message: malformed MIME header line"
		initialLine   = "message: malformed MIME header initial line"
	)
	utf16le := "\xff\xfeA\x00:\x00 \x001\x00\r\x00\n\x00\r\x00\n\x00b\x00"
	utf16be := "\xfe\xff\x00A\x00:\x00 \x001\x00\r\x00\n\x00\r\x00\n\x00b"
	utf32le := "\xff\xfe\x00\x00A\x00\x00\x00:\x00\x00\x00\r\x00\x00\x00\n\x00\x00\x00"
	utf32be := "\x00\x00\xfe\xff\x00\x00\x00A\x00\x00\x00:\x00\x00\x00\r\x00\x00\x00\n"
	return []byteOrderMarkCase{
		// A UTF-8 mark is consumed; offsets count it.
		{name: "utf8 crlf", in: utf8Mark + "A: 1\r\nB: 2\r\n\r\nbody", bom: BOMUTF8, fields: ab, rest: "body",
			ends: ends(wantEnds{3, 0, 0, 7, none, none, none, none})},
		{name: "utf8 lf", in: utf8Mark + "A: 1\nB: 2\n\nbody", bom: BOMUTF8, fields: ab, rest: "body",
			ends: ends(wantEnds{0, 3, 0, none, 7, none, none, none})},
		{name: "utf8 cr", in: utf8Mark + "A: 1\rB: 2\r\rbody", bom: BOMUTF8, fields: ab, rest: "body",
			ends: ends(wantEnds{0, 0, 3, none, none, 7, none, none})},
		{name: "utf8 fold", in: utf8Mark + "A: 1\r\n 2\r\n\r\n", bom: BOMUTF8,
			fields: []wantField{{"A", "1 2", "A: 1\r\n 2\r\n"}},
			ends:   ends(wantEnds{3, 0, 0, 7, none, none, none, none})},
		{name: "utf8 mark only", in: utf8Mark, bom: BOMUTF8,
			ends: ends(wantEnds{0, 0, 0, none, none, none, none, none})},
		{name: "utf8 mark and empty line", in: utf8Mark + "\r\nbody", bom: BOMUTF8, rest: "body",
			ends: ends(wantEnds{1, 0, 0, 3, none, none, none, none})},
		{name: "utf8 unterminated", in: utf8Mark + "A: 1", bom: BOMUTF8, fields: a,
			ends: ends(wantEnds{0, 0, 0, none, none, none, 3, 7})},

		// A UTF-16 or UTF-32 mark leaves the block unread.
		{name: "utf16le", in: utf16le, bom: BOMUTF16LE, rest: utf16le},
		{name: "utf16be", in: utf16be, bom: BOMUTF16BE, rest: utf16be},
		{name: "utf32le", in: utf32le, bom: BOMUTF32LE, rest: utf32le},
		{name: "utf32be", in: utf32be, bom: BOMUTF32BE, rest: utf32be},
		{name: "utf16le mark only", in: "\xff\xfe", bom: BOMUTF16LE, rest: "\xff\xfe"},
		{name: "utf16be mark only", in: "\xfe\xff", bom: BOMUTF16BE, rest: "\xfe\xff"},
		{name: "utf32le mark only", in: "\xff\xfe\x00\x00", bom: BOMUTF32LE, rest: "\xff\xfe\x00\x00"},
		{name: "utf32be mark only", in: "\x00\x00\xfe\xff", bom: BOMUTF32BE, rest: "\x00\x00\xfe\xff"},
		{name: "ff fe 00 at eof", in: "\xff\xfe\x00", bom: BOMUTF16LE, rest: "\xff\xfe\x00"},
		{name: "ff fe 00 41", in: "\xff\xfe\x00A", bom: BOMUTF16LE, rest: "\xff\xfe\x00A"},
		{name: "utf16le mark before ascii", in: "\xff\xfeA: 1\r\n\r\nbody", bom: BOMUTF16LE, rest: "\xff\xfeA: 1\r\n\r\nbody"},

		// Marks anywhere but at the start of the block are content.
		{name: "mark after the empty line", in: "\r\n\xfe\xffx", rest: "\xfe\xffx",
			ends: ends(wantEnds{1, 0, 0, 0, none, none, none, none})},
		{name: "mark in a value", in: "Subject: " + utf8Mark + "x\r\n\r\nbody",
			fields: []wantField{{"Subject", utf8Mark + "x", "Subject: " + utf8Mark + "x\r\n"}}, rest: "body",
			ends: ends(wantEnds{2, 0, 0, 13, none, none, none, none})},
		{name: "mark in the body", in: "A: 1\r\n\r\n" + utf8Mark + "body", fields: a, rest: utf8Mark + "body",
			ends: ends(wantEnds{2, 0, 0, 4, none, none, none, none})},
		{name: "wide mark in the body", in: "A: 1\r\n\r\n\xff\xfeb\x00", fields: a, rest: "\xff\xfeb\x00",
			ends: ends(wantEnds{2, 0, 0, 4, none, none, none, none})},

		// Partial, near-miss, repeated and misplaced marks are read as they
		// always were: the beginning of the first line, or of a later one.
		{name: "ef bb then a field", in: "\xef\xbbA: 1\r\n\r\n", err: malformedKey},
		{name: "ef bb be", in: "\xef\xbb\xbeA: 1\r\n\r\n", err: malformedKey},
		{name: "ef bb at eof", in: "\xef\xbb", err: malformedLine},
		{name: "ef at eof", in: "\xef", err: malformedLine},
		{name: "fe at eof", in: "\xfe", err: malformedLine},
		{name: "ff then a field", in: "\xffA: 1\r\n\r\n", err: malformedKey},
		{name: "00 00 fe at eof", in: "\x00\x00\xfe", err: malformedLine},
		{name: "00 00 ff fe", in: "\x00\x00\xff\xfeA: 1\r\n\r\n", err: malformedKey},
		{name: "repeated utf8 mark", in: utf8Mark + utf8Mark + "A: 1\r\n\r\n", bom: BOMUTF8, err: malformedKey},
		{name: "utf8 mark then utf16le mark", in: utf8Mark + "\xff\xfeA: 1\r\n\r\n", bom: BOMUTF8, err: malformedKey},
		{name: "utf8 mark then white space", in: utf8Mark + " A: 1\r\n\r\n", bom: BOMUTF8, err: initialLine},
		{name: "mark at a later field", in: "A: 1\r\n" + utf8Mark + "B: 2\r\n\r\n", fields: a, err: malformedKey},
	}
}

// TestReadHeader_byteOrderMark pins each case under every delivery and buffer
// size, through ReadHeader and ReadHeaderLimits alike: the mark, the fields and
// their raw bytes, and for a block read without error what is left in the
// reader and its line endings.
func TestReadHeader_byteOrderMark(t *testing.T) {
	for _, tc := range byteOrderMarkCases() {
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
				if got := h.ByteOrderMark(); got != tc.bom {
					t.Errorf("%s: ByteOrderMark() = %v, want %v", label, got, tc.bom)
				}
				if got := fieldsOf(t, h); !sameFields(got, tc.fields) {
					t.Errorf("%s: fields = %q, want %q", label, got, tc.fields)
				}
				if tc.err != "" {
					continue
				}
				rest, rerr := io.ReadAll(br)
				if string(rest) != tc.rest || rerr != nil {
					t.Errorf("%s: rest = (%q, %v), want %q", label, rest, rerr, tc.rest)
				}
				le := h.LineEndings()
				switch {
				case tc.ends == nil && le.Known():
					t.Errorf("%s: LineEndings() = %+v, want unknown for a block not read", label, endsOf(le))
				case tc.ends != nil && !le.Known():
					t.Errorf("%s: LineEndings() unknown for a block read whole", label)
				case tc.ends != nil && endsOf(le) != *tc.ends:
					t.Errorf("%s: line endings = %+v, want %+v", label, endsOf(le), *tc.ends)
				}
			}
		}
	}
}

// TestByteOrderMark_values pins the names and bytes of every mark, and that
// Bytes hands out a new slice each time.
func TestByteOrderMark_values(t *testing.T) {
	for _, tc := range []struct {
		m     ByteOrderMark
		name  string
		bytes string
	}{
		{BOMNone, "none", ""},
		{BOMUTF8, "UTF-8", "\xef\xbb\xbf"},
		{BOMUTF16BE, "UTF-16BE", "\xfe\xff"},
		{BOMUTF16LE, "UTF-16LE", "\xff\xfe"},
		{BOMUTF32BE, "UTF-32BE", "\x00\x00\xfe\xff"},
		{BOMUTF32LE, "UTF-32LE", "\xff\xfe\x00\x00"},
		{ByteOrderMark(99), "ByteOrderMark(99)", ""},
	} {
		if got := tc.m.String(); got != tc.name {
			t.Errorf("%d: String() = %q, want %q", tc.m, got, tc.name)
		}
		b := tc.m.Bytes()
		if string(b) != tc.bytes || (tc.bytes == "") != (b == nil) {
			t.Errorf("%v: Bytes() = %q, want %q", tc.m, b, tc.bytes)
		}
		if len(b) > 0 {
			b[0] = 'x'
			if again := tc.m.Bytes(); string(again) != tc.bytes {
				t.Errorf("%v: Bytes() after changing an earlier result = %q", tc.m, again)
			}
		}
	}
	var h *Header
	if got := h.ByteOrderMark(); got != BOMNone {
		t.Errorf("nil Header: ByteOrderMark() = %v", got)
	}
	if got := (&Header{}).ByteOrderMark(); got != BOMNone {
		t.Errorf("zero Header: ByteOrderMark() = %v", got)
	}
}

func readBlock(t *testing.T, s string) Header {
	t.Helper()
	h, err := ReadHeader(bufio.NewReader(strings.NewReader(s)))
	if err != nil {
		t.Fatalf("ReadHeader(%q) = %v", s, err)
	}
	return h
}

// TestHeader_byteOrderMarkWrite pins what a UTF-8 mark does to a Header: a block
// read whole with its empty line is written back byte for byte, the mark once,
// before the fields and outside their raw bytes; Copy and every mutation keep
// the mark while mutations drop the line endings; HeaderFromMap does not keep
// it; a block io.EOF ended gains its empty line, as without a mark.
func TestHeader_byteOrderMarkWrite(t *testing.T) {
	const block = utf8Mark + "A: 1\r\nB: 2\r\n\r\n"
	h := readBlock(t, block)
	if got := writtenHeader(t, h); got != block {
		t.Fatalf("WriteHeader() = %q, want the block %q", got, block)
	}
	if raw, err := h.Raw("A"); err != nil || string(raw) != "A: 1\r\n" {
		t.Errorf("Raw(A) = (%q, %v), want the field without the mark", raw, err)
	}
	c := h.Copy()
	if c.ByteOrderMark() != BOMUTF8 || !c.LineEndings().Known() || writtenHeader(t, c) != block {
		t.Errorf("Copy(): mark %v, line endings known %v, written %q", c.ByteOrderMark(), c.LineEndings().Known(), writtenHeader(t, c))
	}

	for _, tc := range []struct {
		name string
		edit func(h *Header)
		want string
	}{
		{"Add", func(h *Header) { h.Add("C", "3") }, utf8Mark + "C: 3\r\nA: 1\r\nB: 2\r\n\r\n"},
		{"AddRaw", func(h *Header) { h.AddRaw([]byte("C: 3\r\n")) }, utf8Mark + "C: 3\r\nA: 1\r\nB: 2\r\n\r\n"},
		{"Set", func(h *Header) { h.Set("A", "9") }, utf8Mark + "A: 9\r\nB: 2\r\n\r\n"},
		{"Del", func(h *Header) { h.Del("A") }, utf8Mark + "B: 2\r\n\r\n"},
		{"Fields().Del", func(h *Header) {
			fs := h.Fields()
			for fs.Next() {
				if fs.Key() == "B" {
					fs.Del()
				}
			}
		}, utf8Mark + "A: 1\r\n\r\n"},
	} {
		c := h.Copy()
		tc.edit(&c)
		if c.ByteOrderMark() != BOMUTF8 {
			t.Errorf("%s: ByteOrderMark() = %v, want the mark kept", tc.name, c.ByteOrderMark())
		}
		if c.LineEndings().Known() {
			t.Errorf("%s: LineEndings() still known after a mutation", tc.name)
		}
		if got := writtenHeader(t, c); got != tc.want {
			t.Errorf("%s: WriteHeader() = %q, want %q", tc.name, got, tc.want)
		}
		again := readBlock(t, writtenHeader(t, c))
		if again.ByteOrderMark() != BOMUTF8 || writtenHeader(t, again) != tc.want {
			t.Errorf("%s: reread = (%v, %q)", tc.name, again.ByteOrderMark(), writtenHeader(t, again))
		}
	}
	if writtenHeader(t, h) != block || !h.LineEndings().Known() {
		t.Errorf("the original changed with its copies: %q", writtenHeader(t, h))
	}

	plain := HeaderFromMap(h.Map())
	if plain.ByteOrderMark() != BOMNone || writtenHeader(t, plain) != "A: 1\r\nB: 2\r\n\r\n" {
		t.Errorf("HeaderFromMap: (%v, %q), want no mark", plain.ByteOrderMark(), writtenHeader(t, plain))
	}

	for in, want := range map[string]string{
		utf8Mark:                utf8Mark + "\r\n",
		utf8Mark + "\r\n":       utf8Mark + "\r\n",
		utf8Mark + "A: 1":       utf8Mark + "A: 1\r\n\r\n",
		utf8Mark + "A: 1\r\n":   utf8Mark + "A: 1\r\n\r\n",
		utf8Mark + "A: 1\n\n":   utf8Mark + "A: 1\r\n\r\n",
		utf8Mark + "\r\nbody\r": utf8Mark + "\r\n",
	} {
		if got := writtenHeader(t, readBlock(t, in)); got != want {
			t.Errorf("%q: WriteHeader() = %q, want %q", in, got, want)
		}
	}
}

// TestHeader_byteOrderMarkWideWrite pins that a Header whose block began with a
// UTF-16 or UTF-32 mark is refused by WriteHeader and by MultipartWriter
// before anything is written, through Copy and after fields were added, and
// that rebuilding it from its fields is how it is made writable.
func TestHeader_byteOrderMarkWideWrite(t *testing.T) {
	for _, in := range []string{"\xff\xfeA\x00", "\xfe\xff", "\xff\xfe\x00\x00", "\x00\x00\xfe\xff"} {
		h := readBlock(t, in)
		var b bytes.Buffer
		if err := WriteHeader(&b, h); err == nil || b.Len() != 0 {
			t.Errorf("%q: WriteHeader() = (%q, %v), want an error and nothing written", in, b.String(), err)
		}
		c := h.Copy()
		c.Add("A", "1")
		if c.ByteOrderMark() != h.ByteOrderMark() || h.Len() != 0 {
			t.Errorf("%q: after Add, ByteOrderMark() = %v and the original has %d fields", in, c.ByteOrderMark(), h.Len())
		}
		if err := WriteHeader(&b, c); err == nil || b.Len() != 0 {
			t.Errorf("%q: WriteHeader() after Add = (%q, %v), want an error and nothing written", in, b.String(), err)
		}
		rebuilt := HeaderFromMap(c.Map())
		if err := WriteHeader(&b, rebuilt); err != nil || b.String() != "A: 1\r\n\r\n" {
			t.Errorf("%q: rebuilt WriteHeader() = (%q, %v)", in, b.String(), err)
		}
	}

	// A multipart: the UTF-8 part keeps its mark through the writer and the
	// reader; the UTF-16 part is refused with nothing written, and the part
	// after it is written as usual.
	var out bytes.Buffer
	mw := NewMultipartWriter(&out)
	mw.SetBoundary("B")
	pw, err := mw.CreatePart(readBlock(t, utf8Mark+"A: 1\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(pw, "one")
	n := out.Len()
	if _, err := mw.CreatePart(readBlock(t, "\xff\xfex\x00")); err == nil || out.Len() != n {
		t.Errorf("CreatePart(UTF-16) = %v after writing %q, want an error and nothing written", err, out.String()[n:])
	}
	plain := Header{}
	plain.Add("C", "3")
	pw, err = mw.CreatePart(plain)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(pw, "three")
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	const want = "--B\r\n" + utf8Mark + "A: 1\r\n\r\none\r\n--B\r\nC: 3\r\n\r\nthree\r\n--B--\r\n"
	if out.String() != want {
		t.Fatalf("written %q, want %q", out.String(), want)
	}
	r := NewMultipartReader(strings.NewReader(want), "B")
	for i, w := range []struct {
		bom  ByteOrderMark
		body string
	}{{BOMUTF8, "one"}, {BOMNone, "three"}} {
		p, err := r.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		body, _ := io.ReadAll(p)
		if p.Header.ByteOrderMark() != w.bom || string(body) != w.body {
			t.Errorf("part %d: (%v, %q), want (%v, %q)", i, p.Header.ByteOrderMark(), body, w.bom, w.body)
		}
	}
}

// TestReadHeader_byteOrderMarkBudgets pins what the budgets count: a UTF-8 mark
// is bytes of the block and of the shared budget, and of no line, field or
// count; a refused block keeps nothing, the mark included; a UTF-16 or UTF-32
// mark costs nothing at all.
func TestReadHeader_byteOrderMarkBudgets(t *testing.T) {
	const text = utf8Mark + "A: 1\r\n\r\nbody" // the block is 11 bytes
	read := func(s string, limits HeaderLimits) (Header, string, error) {
		br := bufio.NewReader(strings.NewReader(s))
		h, err := ReadHeaderLimits(br, limits)
		rest, _ := io.ReadAll(br)
		return h, string(rest), err
	}
	for _, limits := range []HeaderLimits{
		{MaxBytes: 11},
		{MaxLineBytes: 4},
		{MaxFieldBytes: 6},
		{MaxCount: 1},
		{Total: NewHeaderBudget(11, 1)},
		{MaxBytes: 11, MaxLineBytes: 4, MaxFieldBytes: 6, MaxCount: 1, Total: NewHeaderBudget(11, 1)},
	} {
		h, rest, err := read(text, limits)
		if err != nil || h.ByteOrderMark() != BOMUTF8 || h.Get("A") != "1" || rest != "body" {
			t.Errorf("%+v: (%v, %v, %q, %v), want the block within its budgets", limits, h.ByteOrderMark(), h.Map(), rest, err)
		}
	}

	total := func(b *HeaderBudget) HeaderLimits { return HeaderLimits{Total: b} }
	for _, tc := range []struct {
		name   string
		limits HeaderLimits
		kind   HeaderLimitKind
		unit   int64
	}{
		{"block one byte short", HeaderLimits{MaxBytes: 10}, HeaderLimitBytes, 0},
		{"block shorter than the mark", HeaderLimits{MaxBytes: 2}, HeaderLimitBytes, 0},
		{"line", HeaderLimits{MaxLineBytes: 3}, HeaderLimitLineBytes, 3},
		{"field", HeaderLimits{MaxFieldBytes: 5}, HeaderLimitFieldBytes, 3},
		{"shared bytes one short", total(NewHeaderBudget(10, 0)), HeaderLimitTotalBytes, 10},
		{"shared bytes inside the mark", total(NewHeaderBudget(2, 0)), HeaderLimitTotalBytes, 2},
	} {
		h, _, err := read(text, tc.limits)
		limit, refused := HeaderLimit(err)
		if !refused || limit.Kind() != tc.kind || limit.Offset() != 0 || limit.UnitOffset() != tc.unit {
			t.Errorf("%s: err = %v, want %v refused at %d", tc.name, err, tc.kind, tc.unit)
			continue
		}
		if h.Len() != 0 || h.ByteOrderMark() != BOMNone {
			t.Errorf("%s: a refused block kept %v and mark %v", tc.name, h.Map(), h.ByteOrderMark())
		}
	}

	// A spent shared field count refuses the first field after the mark, at its
	// first byte.
	b := NewHeaderBudget(0, 1)
	if _, _, err := read("A: 1\r\n\r\n", total(b)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := read(text, total(b)); err == nil {
		t.Error("a spent shared count admitted the field after the mark")
	} else if limit, refused := HeaderLimit(err); !refused || limit.Kind() != HeaderLimitTotalCount || limit.UnitOffset() != 3 {
		t.Errorf("spent shared count: err = %v, want the total count refused at 3", err)
	}

	// A wide mark is not charged at all: the tightest budgets admit it, and the
	// shared budget is left whole for the next block.
	const wide = "\xff\xfeA\x00:\x00 \x001\x00\r\x00\n\x00"
	shared := NewHeaderBudget(6, 1)
	h, rest, err := read(wide, HeaderLimits{MaxBytes: 1, MaxLineBytes: 1, MaxFieldBytes: 1, MaxCount: 1, Total: shared})
	if err != nil || h.ByteOrderMark() != BOMUTF16LE || h.Len() != 0 || rest != wide {
		t.Errorf("wide: (%v, %d fields, %q, %v), want the block unread", h.ByteOrderMark(), h.Len(), rest, err)
	}
	if h, _, err := read("A: 1\r\n", total(shared)); err != nil || h.Get("A") != "1" || shared.Refusal() != nil {
		t.Errorf("the next block under the shared budget: (%v, %v), want it read whole", h.Map(), err)
	}
}

// wideFailureTexts are UTF-16 and UTF-32 blocks with the number of bytes it
// takes to decide their mark.
var wideFailureTexts = []struct {
	text   string
	bom    ByteOrderMark
	decide int
}{
	{"\xfe\xff\x00A\x00:\x00\r\x00\n", BOMUTF16BE, 2},
	{"\xff\xfeA\x00:\x00\r\x00\n\x00", BOMUTF16LE, 3},
	{"\xff\xfe\x00\x00A\x00\x00\x00", BOMUTF32LE, 4},
	{"\x00\x00\xfe\xffA", BOMUTF32BE, 4},
}

// TestReadHeader_byteOrderMarkReadFailure cuts a block that begins with a mark
// at every byte, at buffer sizes 16 and 4096 and three chunkings, with the
// failure delivered with the last bytes or after them, from sources that keep
// failing and from sources that resume. A failure before a mark is decided is
// returned as itself with no field and no mark; once a UTF-8 mark is decided
// the block is read on it as any block is, the mark kept beside the fields read
// whole; a UTF-16 or UTF-32 mark once decided is returned with no error, and
// the failure, never read past, is what the next read of the reader returns
// after every byte before it.
func TestReadHeader_byteOrderMarkReadFailure(t *testing.T) {
	const block = "A: 1\r\nB: 2\r\n\r\n"
	const text = utf8Mark + block + "body"
	read := map[string]func(*bufio.Reader) (Header, error){
		"ReadHeader":       ReadHeader,
		"ReadHeaderLimits": func(r *bufio.Reader) (Header, error) { return ReadHeaderLimits(r, HeaderLimits{MaxBytes: 1 << 20}) },
	}
	for api, readFn := range read {
		for _, size := range []int{16, 4096} {
			for _, chunk := range []int{1, 7, 4096} {
				for _, withData := range []bool{false, true} {
					for _, sticky := range []bool{true, false} {
						for cut := 0; cut <= len(text); cut++ {
							name := fmt.Sprintf("utf8/%s/cut %d/buf %d/chunk %d/with %v/sticky %v", api, cut, size, chunk, withData, sticky)
							src := &failAtSource{data: text, cut: cut, chunk: chunk, err: errEIO, withData: withData, sticky: sticky}
							br := bufio.NewReaderSize(src, size)
							h, err := readFn(br)
							if src.readsAfter != 0 {
								t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
							}
							if cut < len(utf8Mark) {
								if err != errEIO || h.ByteOrderMark() != BOMNone || h.Len() != 0 {
									t.Errorf("%s: (%v, %v, %v), want no mark, no field and the failure", name, h.ByteOrderMark(), h.Map(), err)
								}
								continue
							}
							kept, complete := wholeLines(block, cut-len(utf8Mark))
							if complete {
								if err != nil || writtenHeader(t, h) != utf8Mark+block {
									t.Errorf("%s: (%q, %v), want the whole block", name, writtenHeader(t, h), err)
									continue
								}
								body, berr := io.ReadAll(br)
								if want := text[len(utf8Mark+block):max(cut, len(utf8Mark+block))]; string(body) != want || berr != errEIO {
									t.Errorf("%s: body (%q, %v), want (%q, the failure)", name, body, berr, want)
								}
								continue
							}
							if err != errEIO || h.ByteOrderMark() != BOMUTF8 {
								t.Errorf("%s: (%v, %v), want the mark and the input's own failure", name, h.ByteOrderMark(), err)
							}
							if got := writtenHeader(t, h); got != utf8Mark+kept+"\r\n" {
								t.Errorf("%s: kept %q, want the mark and the whole lines %q", name, got, kept)
							}
						}

						for _, w := range wideFailureTexts {
							for cut := 0; cut <= len(w.text); cut++ {
								name := fmt.Sprintf("%v/%s/cut %d/buf %d/chunk %d/with %v/sticky %v", w.bom, api, cut, size, chunk, withData, sticky)
								src := &failAtSource{data: w.text, cut: cut, chunk: chunk, err: errEIO, withData: withData, sticky: sticky}
								br := bufio.NewReaderSize(src, size)
								h, err := readFn(br)
								if src.readsAfter != 0 {
									t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
								}
								if cut < w.decide {
									if err != errEIO || h.ByteOrderMark() != BOMNone || h.Len() != 0 {
										t.Errorf("%s: (%v, %v), want no mark and the failure", name, h.ByteOrderMark(), err)
									}
									continue
								}
								if err != nil || h.ByteOrderMark() != w.bom || h.Len() != 0 {
									t.Errorf("%s: (%v, %d fields, %v), want the mark, no field and no error", name, h.ByteOrderMark(), h.Len(), err)
									continue
								}
								rest, rerr := io.ReadAll(br)
								if string(rest) != w.text[:cut] || rerr != errEIO {
									t.Errorf("%s: rest (%q, %v), want (%q, the failure)", name, rest, rerr, w.text[:cut])
								}
							}
						}
					}
				}
			}
		}
	}
}

// TestReadHeader_byteOrderMarkEOF pins io.EOF while a mark is decided: it
// decides on the bytes before it.
func TestReadHeader_byteOrderMarkEOF(t *testing.T) {
	for _, tc := range []struct {
		in  string
		bom ByteOrderMark
	}{
		{"\xff\xfe", BOMUTF16LE},
		{"\xff\xfe\x00", BOMUTF16LE},
		{"\xff\xfe\x00\x00", BOMUTF32LE},
		{"\xfe\xff", BOMUTF16BE},
		{"\x00\x00\xfe\xff", BOMUTF32BE},
		{utf8Mark, BOMUTF8},
	} {
		for _, chunk := range []int{1, 2, 4096} {
			for _, withData := range []bool{false, true} {
				src := &failAtSource{data: tc.in, cut: len(tc.in), chunk: chunk, err: io.EOF, withData: withData, sticky: true}
				br := bufio.NewReaderSize(src, 16)
				h, err := ReadHeader(br)
				if err != nil || h.ByteOrderMark() != tc.bom || h.Len() != 0 || src.readsAfter != 0 {
					t.Errorf("%q/chunk %d/with %v: (%v, %v) after %d reads past io.EOF, want %v",
						tc.in, chunk, withData, h.ByteOrderMark(), err, src.readsAfter, tc.bom)
				}
			}
		}
	}
}

const bomBufferFullEnv = "EMLKIT_TEXTPROTO_BOM_BUFFER_FULL"

// TestReadHeader_byteOrderMarkSourceBufferFull pins a source that keeps failing
// with bufio.ErrBufferFull itself while a mark is decided, or right after one:
// that value is the source's failure, returned as itself, never read on. It
// runs in a subprocess with a deadline, as the general case does.
func TestReadHeader_byteOrderMarkSourceBufferFull(t *testing.T) {
	if os.Getenv(bomBufferFullEnv) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadHeader_byteOrderMarkSourceBufferFull$", "-test.count=1")
		cmd.Env = append(os.Environ(), bomBufferFullEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("subprocess = %v, deadline %v\n%s", err, ctx.Err(), out)
		}
		return
	}

	type text struct {
		s      string
		bom    ByteOrderMark
		decide int
	}
	texts := []text{{utf8Mark + "A: 1\r\n\r\n", BOMUTF8, 3}}
	for _, w := range wideFailureTexts {
		texts = append(texts, text{w.text, w.bom, w.decide})
	}
	for _, tx := range texts {
		for cut := 0; cut <= tx.decide; cut++ {
			for _, chunk := range []int{1, 4096} {
				for _, withData := range []bool{false, true} {
					name := fmt.Sprintf("%v/cut %d/chunk %d/with %v", tx.bom, cut, chunk, withData)
					src := &failAtSource{data: tx.s, cut: cut, chunk: chunk, err: bufio.ErrBufferFull, withData: withData, sticky: true}
					br := bufio.NewReaderSize(src, 16)
					h, err := ReadHeader(br)
					switch {
					case cut < tx.decide || tx.bom == BOMUTF8:
						want := BOMNone
						if cut >= tx.decide {
							want = BOMUTF8
						}
						if err != bufio.ErrBufferFull || h.ByteOrderMark() != want || h.Len() != 0 {
							t.Errorf("%s: (%v, %v), want %v and the source's own error", name, h.ByteOrderMark(), err, want)
						}
					default:
						if err != nil || h.ByteOrderMark() != tx.bom {
							t.Errorf("%s: (%v, %v), want %v and no error", name, h.ByteOrderMark(), err, tx.bom)
						}
						if rest, rerr := io.ReadAll(br); string(rest) != tx.s[:cut] || rerr != bufio.ErrBufferFull {
							t.Errorf("%s: rest (%q, %v), want (%q, the source's own error)", name, rest, rerr, tx.s[:cut])
						}
					}
					if src.readsAfter > 1 {
						t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
					}
				}
			}
		}
	}
}

// TestMultipartReader_byteOrderMarkParts pins marks at the start of part
// headers under every delivery, with no budgets and with budgets that admit
// the blocks only if the mark is no line's and no field's: a UTF-8 part keeps
// its mark and fields, a UTF-16 part has none and its body is its every byte
// from the mark on, at its header offset, and the part after it is read as
// usual.
func TestMultipartReader_byteOrderMarkParts(t *testing.T) {
	const wide = "\xff\xfex\x00\r\x00\n\x00"
	const text = "--B\r\n" + utf8Mark + "A: 1\r\n\r\none\r\n--B\r\n" + wide + "\r\n--B\r\nC: 3\r\n\r\nthree\r\n--B--\r\n"
	want := []struct {
		bom    ByteOrderMark
		fields []wantField
		body   string
		header int64
		start  int64
	}{
		{BOMUTF8, []wantField{{"A", "1", "A: 1\r\n"}}, "one", 5, 16},
		{BOMUTF16LE, nil, wide, 26, 26},
		{BOMNone, []wantField{{"C", "3", "C: 3\r\n"}}, "three", 41, 49},
	}
	// "A: 1" is 4 bytes and 7 with the mark, "A: 1\r\n" 6 and 9; the closing
	// delimiter's "--B--" needs a line budget of 5.
	for _, limits := range []HeaderLimits{{}, {MaxBytes: 11, MaxLineBytes: 6, MaxFieldBytes: 6, MaxCount: 1}} {
		for name, src := range mpSources(text) {
			label := fmt.Sprintf("%+v/%s", limits, name)
			r := NewMultipartReader(src(), "B")
			r.SetHeaderLimits(limits)
			for i, w := range want {
				p, err := r.NextPart()
				if err != nil {
					t.Fatalf("%s: part %d: %v", label, i, err)
				}
				body, berr := io.ReadAll(p)
				if p.Header.ByteOrderMark() != w.bom || !sameFields(fieldsOf(t, p.Header), w.fields) ||
					string(body) != w.body || berr != nil || p.HeaderOffset() != w.header || p.BodyOffset() != w.start {
					t.Errorf("%s: part %d = (%v, %q, %q, %v, %d, %d), want (%v, %q, %q, nil, %d, %d)", label, i,
						p.Header.ByteOrderMark(), fieldsOf(t, p.Header), body, berr, p.HeaderOffset(), p.BodyOffset(),
						w.bom, w.fields, w.body, w.header, w.start)
				}
			}
			if _, err := r.NextPart(); err != io.EOF {
				t.Errorf("%s: after the last part: %v, want io.EOF", label, err)
			}
		}
	}
}
