package textproto

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// hpReadRecover reads block+rest under recovery and returns the header and
// what was left for the body: the bytes handed back and the reader's own.
func hpReadRecover(t *testing.T, in string, limits HeaderLimits) (Header, string, error) {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(in))
	h, unread, err := ReadHeaderRecover(br, limits)
	rest, rerr := io.ReadAll(br)
	if rerr != nil {
		t.Fatalf("reading the rest: %v", rerr)
	}
	return h, string(unread) + string(rest), err
}

// TestHeaderExact_replay pins what parse then write preserves for a block read
// with HeaderLimits.Recover and not changed since (ROB-020-007):
// WriteHeaderAsRead writes the block's bytes, and WriteHeader does too whenever
// the block ended with an empty line ended by CRLF or LF; any other block is
// written by WriteHeader as it always was, which is what plain is then.
func TestHeaderExact_replay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string // the header block, exactly
		rest  string // what follows it
		plain string // what WriteHeader writes, when not block
	}{
		{name: "crlf", block: "From: a@example.org\r\nSubject: x\r\n\r\n", rest: "body\r\n"},
		{name: "lf only", block: "From: a@example.org\nSubject: x\n folded\n\n", rest: "body\n"},
		{name: "cr only, crlf separator", block: "From: a@example.org\rSubject: x\r folded\r\r\n", rest: "body\r"},
		{name: "mixed endings", block: "A: 1\nB: 2\r\nC: 3\r folded\n more\r\nD: 4\n\r\n", rest: "body"},
		{name: "lf fields, crlf separator", block: "A: 1\nB: 2\n\r\n", rest: "x"},
		{name: "crlf fields, lf separator", block: "A: 1\r\nB: 2\r\n\n", rest: "x"},
		{name: "cr content in a value", block: "Subject: a\rb\r\nX: 1\r\n\r\n"},
		{name: "unusual folding", block: "Subject: a\r\n\t\tb\r\n   \r\n \t c\r\nX:\r\n v\r\n\r\n"},
		{name: "trailing white space", block: "Subject: x  \t\r\nX-Empty:   \r\nY:v \n\r\n"},
		{name: "name case and space before colon", block: "sUbJeCt: x\r\nX-ODD  :  v\r\ncontent-TYPE:text/plain\r\n\r\n"},
		{name: "order and duplicates", block: "Received: 3\r\nX: b\r\nReceived: 2\r\nX: a\r\nReceived: 1\r\nX: b\r\n\r\n"},
		{name: "8-bit value", block: "Subject: caf\xe9 \xff\xfe\x80\r\nX: \xc3\xa9\n\n"},
		{name: "semicolon separator", block: "X-Semi; v\nA: 1\n\n"},
		{name: "utf-8 mark and malformed prefix", block: "\xef\xbb\xbf  lost\n more\nA: 1\n\n", rest: "b"},
		{name: "orphan record", block: "A: 1\nbroken\n orphan\nB: 2\n\n"},
		{name: "empty name", block: "A: 1\r\n: nothing\r\nB: 2\r\n\r\n", rest: "body"},
		{name: "empty name, lf, first and last", block: ": first\nA: 1\n ; \n:\n\n"},
		{name: "empty name with a continuation", block: "A: 1\n: v\n more\nB: 2\n\n"},
		{name: "unindented continuation", block: "Content-Type: multipart/mixed;\nboundary=\"b\"\nTo: a@example.org,\nb@example.org\n\n", rest: "--b--\n"},
		{name: "missing colon line", block: "A: 1\nthis line has no colon\nSubject: x\n\n", rest: "body"},
		{name: "invalid name line", block: "A: 1\r\nBad Name: v\nSubject: x\r\n\r\n", rest: "body"},
		{name: "white space only continuation", block: "A: 1\n \t\nB: 2\n\n"},
		{name: "only the empty line", block: "\n", rest: "body"},

		// An empty line ended by a bare CR, or none in the input: WriteHeader
		// writes CRLF line endings and the empty line.
		{name: "cr only", block: "From: a@example.org\rSubject: x\r folded\r\r", rest: "body\r",
			plain: "From: a@example.org\r\nSubject: x\r\n folded\r\n\r\n"},
		{name: "cr separator before a body", block: "A: 1\r\n\r", rest: "body", plain: "A: 1\r\n\r\n"},
		{name: "missing separator", block: "A: 1\nSubject: x\n", rest: "the body begins here\nmore\n",
			plain: "A: 1\r\nSubject: x\r\n\r\n"},
		{name: "missing separator after a healed line", block: "A: 1;\nb=c\n", rest: "plain text.\n",
			plain: "A: 1;\r\nb=c\r\n\r\n"},
		{name: "end of input", block: "A: 1\r\nB: 2\r\n", plain: "A: 1\r\nB: 2\r\n\r\n"},
		{name: "unterminated last line", block: "A: 1\nB: 2", plain: "A: 1\r\nB: 2\r\n\r\n"},
		{name: "cr before the end of input", block: "A: 1\r", plain: "A: 1\r\n\r\n"},
		{name: "empty input", block: "", plain: "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rest, err := hpReadRecover(t, tc.block+tc.rest, HeaderLimits{})
			if err != nil {
				t.Fatalf("ReadHeaderRecover() = %v", err)
			}
			if rest != tc.rest {
				t.Fatalf("rest = %q, want %q", rest, tc.rest)
			}
			if !h.Intact() {
				t.Fatalf("Intact() = false for a block just read")
			}
			var asRead, plain bytes.Buffer
			if err := WriteHeaderAsRead(&asRead, h); err != nil {
				t.Fatalf("WriteHeaderAsRead() = %v", err)
			}
			if asRead.String() != tc.block {
				t.Errorf("WriteHeaderAsRead() wrote %q, want %q", asRead.String(), tc.block)
			}
			want := tc.block
			if tc.plain != "" {
				want = tc.plain
			}
			c := h.Copy()
			if err := WriteHeader(&plain, c); err != nil {
				t.Fatalf("WriteHeader() = %v", err)
			}
			if plain.String() != want {
				t.Errorf("WriteHeader() of a copy wrote %q, want %q", plain.String(), want)
			}
		})
	}
}

// TestHeaderExact_emptyName pins ROB-020-006: under recovery a line with an
// empty name keeps its bytes as a record of one line, is no lookup key, and
// costs what it cost when it was skipped. Without recovery it is skipped.
func TestHeaderExact_emptyName(t *testing.T) {
	const in = "A: 1\r\n: caf\xe9\r\n;x\r\nB: 2\r\n\r\n"

	h, _, err := hpReadRecover(t, in, HeaderLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range h.OrphanContinuations() {
		if o.Lines() != 1 {
			t.Errorf("record %q has %d lines, want 1", o.Bytes(), o.Lines())
		}
		start, end := o.Location()
		if in[start:end] != string(o.Bytes()) {
			t.Errorf("record %q is not at [%d, %d)", o.Bytes(), start, end)
		}
		got = append(got, string(o.Bytes()))
	}
	if want := []string{": caf\xe9\r\n", ";x\r\n"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("records = %q, want %q", got, want)
	}
	if h.Len() != 2 || h.Has("") || h.Get("") != "" || len(h.Map()) != 2 {
		t.Errorf("an empty name became a field: Len %d, Map %v", h.Len(), h.Map())
	}

	// One candidate each, and its bytes: the budgets are what they were.
	if _, _, err := hpReadRecover(t, in, HeaderLimits{MaxCount: 4, MaxBytes: int64(len(in))}); err != nil {
		t.Errorf("at the block's exact cost: %v", err)
	}
	if _, _, err := hpReadRecover(t, in, HeaderLimits{MaxCount: 3}); !errors.Is(err, ErrTooManyHeaderFields) {
		t.Errorf("MaxCount 3 = %v, want the count refusal", err)
	}
	if _, _, err := hpReadRecover(t, in, HeaderLimits{MaxBytes: int64(len(in)) - 1}); !errors.Is(err, ErrHeaderTooBig) {
		t.Errorf("MaxBytes one short = %v, want the byte refusal", err)
	}

	// Without recovery: skipped, not kept, not written, as it always was.
	plain, err := ReadHeader(bufio.NewReader(strings.NewReader(in)))
	if err != nil || plain.OrphanContinuations() != nil || plain.Intact() {
		t.Fatalf("ReadHeader() = %v, records %v, intact %v", err, plain.OrphanContinuations(), plain.Intact())
	}
	var b bytes.Buffer
	if err := WriteHeader(&b, plain); err != nil || b.String() != "A: 1\r\nB: 2\r\n\r\n" {
		t.Errorf("WriteHeader() without recovery wrote %q, %v", b.String(), err)
	}
}

// TestHeaderExact_views pins the two spellings of a field and when a header
// stops being intact.
func TestHeaderExact_views(t *testing.T) {
	const in = "Subject: caf\xe9\n\tnext \nX: 1\r\n\n"
	h, _, err := hpReadRecover(t, in, HeaderLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := h.Raw("subject"); string(raw) != "Subject: caf\xe9\r\n\tnext \r\n" {
		t.Errorf("Raw() = %q, want every line ended by CRLF", raw)
	}
	if raw, _ := h.RawAsRead("subject"); string(raw) != "Subject: caf\xe9\n\tnext \n" {
		t.Errorf("RawAsRead() = %q, want the input's bytes", raw)
	}
	if raw, _ := h.RawAsRead("x"); string(raw) != "X: 1\r\n" {
		t.Errorf("RawAsRead(X) = %q", raw)
	}
	if got := h.Get("Subject"); got != "caf\xe9 next" {
		t.Errorf("Get() = %q", got)
	}

	write := func(h Header) string {
		var b bytes.Buffer
		if err := WriteHeader(&b, h); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	// A no-op deletion changes nothing; adding or deleting a field does, and
	// the header is then written as it always was, safely ended.
	c := h.Copy()
	c.Del("Absent")
	if !c.Intact() || write(c) != in {
		t.Errorf("after deleting nothing: intact %v, wrote %q", c.Intact(), write(c))
	}
	c.Add("New", "v")
	if c.Intact() || write(c) != "New: v\r\nSubject: caf\xe9\r\n\tnext \r\nX: 1\r\n\r\n" {
		t.Errorf("after Add: intact %v, wrote %q", c.Intact(), write(c))
	}
	d := h.Copy()
	d.Del("X")
	if d.Intact() || write(d) != "Subject: caf\xe9\r\n\tnext \r\n\r\n" {
		t.Errorf("after Del: intact %v, wrote %q", d.Intact(), write(d))
	}
	if !h.Intact() || write(h) != in {
		t.Errorf("the header the copies were made of changed: wrote %q", write(h))
	}

	// Without recovery nothing changes: lines are written with CRLF.
	plain, err := ReadHeader(bufio.NewReader(strings.NewReader(in)))
	if err != nil || plain.Intact() || write(plain) != "Subject: caf\xe9\r\n\tnext \r\nX: 1\r\n\r\n" {
		t.Errorf("ReadHeader(): %v, intact %v, wrote %q", err, plain.Intact(), write(plain))
	}
	if raw, _ := plain.RawAsRead("subject"); string(raw) != "Subject: caf\xe9\r\n\tnext \r\n" {
		t.Errorf("RawAsRead() without recovery = %q", raw)
	}
}

// TestHeaderExact_chunked pins that what is kept does not depend on how the
// input chunks its reads or on the reader's buffer.
func TestHeaderExact_chunked(t *testing.T) {
	const block = "A: 1\n: none\rB: 2\r\n folded\rC: 3\nbroken line\nSubject: x\n\r"
	const in = block + "body"
	for _, size := range []int{16, 64, 4096} {
		br := bufio.NewReaderSize(iotest1(in), size)
		h, unread, err := ReadHeaderRecover(br, HeaderLimits{})
		if err != nil || len(unread) != 0 {
			t.Fatalf("size %d: ReadHeaderRecover() = %q, %v", size, unread, err)
		}
		var b bytes.Buffer
		if err := WriteHeaderAsRead(&b, h); err != nil || b.String() != block {
			t.Errorf("size %d: wrote %q, %v, want %q", size, b.String(), err, block)
		}
	}
}

// iotest1 returns a reader that yields s one byte a read.
func iotest1(s string) io.Reader { return &hpOneByte{s: s} }

type hpOneByte struct{ s string }

func (r *hpOneByte) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.s[0]
	r.s = r.s[1:]
	return 1, nil
}

// hpReplayCheck reads data under recovery and, when the block was read, holds
// the header to the replay: it is intact, and WriteHeaderAsRead writes exactly
// the bytes the read consumed.
func hpReplayCheck(t *testing.T, data []byte, size int) {
	t.Helper()
	br := bufio.NewReaderSize(bytes.NewReader(data), size)
	h, unread, err := ReadHeaderRecover(br, HeaderLimits{})
	if err != nil {
		return
	}
	rest, _ := io.ReadAll(br)
	rest = append(unread, rest...)
	if !bytes.HasSuffix(data, rest) {
		t.Fatalf("%q: what is left, %q, is not the input's end", data, rest)
	}
	block := data[:len(data)-len(rest)]
	if !h.Intact() {
		t.Fatalf("%q: the block %q was read but is not intact", data, block)
	}
	var b bytes.Buffer
	if err := WriteHeaderAsRead(&b, h); err != nil || !bytes.Equal(b.Bytes(), block) {
		t.Fatalf("%q: replayed %q, %v, want %q", data, b.Bytes(), err, block)
	}
}

// FuzzHeaderExact_replay holds every block a recovering read accepts to a
// byte-exact replay, whatever its line endings and damage.
func FuzzHeaderExact_replay(f *testing.F) {
	for _, s := range []string{
		"A: 1\r\nB: 2\r\n\r\nbody",
		"A: 1\n: none\rB: 2\r\n folded\rC: 3\nbroken line\nSubject: x\n\rbody",
		"\xef\xbb\xbf  lost\n more\nA: 1\n\n",
		":x\rb\r\n y\r\n;q\r\n z\r\n\r\n",
		"Content-Type: a/b;\nboundary=x\nplain text\n",
		"A: 1\rbroken\r cont\rB: 2\r\r",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 2048 {
			return
		}
		hpReplayCheck(t, data, 16)
		hpReplayCheck(t, data, 4096)
	})
}

// TestHeaderExact_generated runs the same check over blocks put together from
// the pieces damaged headers are made of.
func TestHeaderExact_generated(t *testing.T) {
	pieces := []string{"A: 1", "Subject: s", ": x", ";", "broken", " cont", "\tw", "Bad Name: v", "X;y", "b=c;", "\xff:", ""}
	ends := []string{"\r\n", "\n", "\r", "\r\n", "\n"}
	seed := uint32(1)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}
	for i := 0; i < 3000; i++ {
		var b []byte
		for j := next(7); j >= 0; j-- {
			b = append(b, pieces[next(len(pieces))]...)
			if j > 0 || next(4) > 0 {
				b = append(b, ends[next(len(ends))]...)
			}
		}
		hpReplayCheck(t, b, 16+next(2)*4080)
	}
}
