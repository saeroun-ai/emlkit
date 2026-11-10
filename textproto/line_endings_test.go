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
	"testing/iotest"
	"time"
)

// Line endings of a header block (ROB-010-002, phase 1). Every source here is
// finite: in-memory strings, call-capped failing sources, and endless sources
// capped at endlessCap bytes. The one hang-prone probe runs in a subprocess
// with a deadline.

type wantField struct{ key, value, raw string }

// wantEnds is a LineEndings spelled out: counts and first offsets per kind
// (first -1 for none) and the unterminated range (-1 for none).
type wantEnds struct {
	crlf, lf, cr                int64
	firstCRLF, firstLF, firstCR int64
	untermStart, untermEnd      int64
}

func endsOf(l LineEndings) wantEnds {
	first := func(k LineEnding) int64 {
		if at, ok := l.First(k); ok {
			return at
		}
		return -1
	}
	w := wantEnds{
		crlf: l.Count(LineEndingCRLF), lf: l.Count(LineEndingLF), cr: l.Count(LineEndingCR),
		firstCRLF: first(LineEndingCRLF), firstLF: first(LineEndingLF), firstCR: first(LineEndingCR),
		untermStart: -1, untermEnd: -1,
	}
	if s, e, ok := l.Unterminated(); ok {
		w.untermStart, w.untermEnd = s, e
	}
	return w
}

func fieldsOf(t *testing.T, h Header) []wantField {
	t.Helper()
	var out []wantField
	fs := h.Fields()
	for fs.Next() {
		raw, err := fs.Raw()
		if err != nil {
			t.Fatalf("Raw() = %v", err)
		}
		out = append(out, wantField{fs.Key(), fs.Value(), string(raw)})
	}
	return out
}

func sameFields(a, b []wantField) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type lineEndingCase struct {
	name   string
	in     string
	fields []wantField
	rest   string
	ends   wantEnds
}

func lineEndingCases() []lineEndingCase {
	none := int64(-1)
	return []lineEndingCase{
		{"crlf", "A: 1\r\nB: 2\r\n\r\nbody",
			[]wantField{{"A", "1", "A: 1\r\n"}, {"B", "2", "B: 2\r\n"}}, "body",
			wantEnds{3, 0, 0, 4, none, none, none, none}},
		{"lf", "A: 1\nB: 2\n\nbody",
			[]wantField{{"A", "1", "A: 1\r\n"}, {"B", "2", "B: 2\r\n"}}, "body",
			wantEnds{0, 3, 0, none, 4, none, none, none}},
		{"cr", "A: 1\rB: 2\r\rbody",
			[]wantField{{"A", "1", "A: 1\r\n"}, {"B", "2", "B: 2\r\n"}}, "body",
			wantEnds{0, 0, 3, none, none, 4, none, none}},
		{"cr fold", "A: 1\r 2\r\r",
			[]wantField{{"A", "1 2", "A: 1\r\n 2\r\n"}}, "",
			wantEnds{0, 0, 3, none, none, 4, none, none}},
		{"field cr then crlf empty line", "X: 1\r\r\nY: 2\r\n\r\n",
			[]wantField{{"X", "1", "X: 1\r\n"}}, "Y: 2\r\n\r\n",
			wantEnds{1, 0, 1, 5, none, 4, none, none}},
		{"stray cr before text", "Subject: a\rb\r\nX: 1\r\n\r\nbody",
			[]wantField{{"Subject", "a\rb", "Subject: a\rb\r\n"}, {"X", "1", "X: 1\r\n"}}, "body",
			wantEnds{3, 0, 0, 12, none, none, none, none}},
		{"stray crs repeated", "Subject: a\rb\rc\r\n\r\n",
			[]wantField{{"Subject", "a\rb\rc", "Subject: a\rb\rc\r\n"}}, "",
			wantEnds{2, 0, 0, 14, none, none, none, none}},
		{"stray cr before a field", "Subject: a\rTo: b\r\n\r\n",
			[]wantField{{"Subject", "a", "Subject: a\r\n"}, {"To", "b", "To: b\r\n"}}, "",
			wantEnds{2, 0, 1, 16, none, 10, none, none}},
		{"cr before a non-name byte", "Subject: a\r\x01b\r\n\r\n",
			[]wantField{{"Subject", "a\r\x01b", "Subject: a\r\x01b\r\n"}}, "",
			wantEnds{2, 0, 0, 13, none, none, none, none}},
		{"cr before a colon", "A:1\r:x\r\n\r\n",
			[]wantField{{"A", "1\r:x", "A:1\r:x\r\n"}}, "",
			wantEnds{2, 0, 0, 6, none, none, none, none}},
		// The probe after a bare '\r' matches name *WSP ":" only, deliberately
		// without the semicolon fallback ordinary candidates have: "b;c" after
		// a '\r' continues the value, as it did before, while the same line
		// after "\r\n" is a candidate and the fallback makes it field B.
		{"semicolon line after a bare cr continues the value", "Subject: a\rb;c\r\n\r\n",
			[]wantField{{"Subject", "a\rb;c", "Subject: a\rb;c\r\n"}}, "",
			wantEnds{2, 0, 0, 14, none, none, none, none}},
		{"semicolon line after crlf is a field", "Subject: a\r\nb;c\r\n\r\n",
			[]wantField{{"Subject", "a", "Subject: a\r\n"}, {"B", "c", "b;c\r\n"}}, "",
			wantEnds{3, 0, 0, 10, none, none, none, none}},
		{"stray cr in a continuation", "X: a\r\n b\rc\r\n\r\n",
			[]wantField{{"X", "a b\rc", "X: a\r\n b\rc\r\n"}}, "",
			wantEnds{3, 0, 0, 4, none, none, none, none}},
		{"stray cr after a skipped empty name", ":x\rb\r\nY: 1\r\n\r\n",
			[]wantField{{"Y", "1", "Y: 1\r\n"}}, "",
			wantEnds{3, 0, 0, 4, none, none, none, none}},
		{"missing final line ending", "A: 1\r\nB: 2",
			[]wantField{{"A", "1", "A: 1\r\n"}, {"B", "2", "B: 2\r\n"}}, "",
			wantEnds{1, 0, 0, 4, none, none, 6, 10}},
		{"stray cr then end of input", "Subject: a\rbc",
			[]wantField{{"Subject", "a\rbc", "Subject: a\rbc\r\n"}}, "",
			wantEnds{0, 0, 0, none, none, none, 0, 13}},
		{"header-only trailing cr", "A: 1\r",
			[]wantField{{"A", "1", "A: 1\r\n"}}, "",
			wantEnds{0, 0, 1, none, none, 4, none, none}},
		{"bare cr alone", "\r", nil, "",
			wantEnds{0, 0, 1, none, none, 0, none, none}},
		{"bare cr empty line first", "\rA: 1\r\n", nil, "A: 1\r\n",
			wantEnds{0, 0, 1, none, none, 0, none, none}},
		{"bare cr empty line after crlf", "A: 1\r\n\rbody",
			[]wantField{{"A", "1", "A: 1\r\n"}}, "body",
			wantEnds{1, 0, 1, 4, none, 6, none, none}},
		{"empty", "", nil, "", wantEnds{0, 0, 0, none, none, none, none, none}},
	}
}

// lineEndingReaders are the deliveries and buffer sizes every case is read
// through, each as a fresh bufio.Reader.
func lineEndingReaders(s string) map[string]func() *bufio.Reader {
	out := map[string]func() *bufio.Reader{}
	for _, size := range []int{16, 17, 4096} {
		out[fmt.Sprintf("buf %d whole", size)] = func() *bufio.Reader {
			return bufio.NewReaderSize(strings.NewReader(s), size)
		}
		out[fmt.Sprintf("buf %d data+eof", size)] = func() *bufio.Reader {
			return bufio.NewReaderSize(iotest.DataErrReader(strings.NewReader(s)), size)
		}
		for _, n := range []int{1, 2, 3, 7} {
			out[fmt.Sprintf("buf %d chunks %d", size, n)] = func() *bufio.Reader {
				return bufio.NewReaderSize(&chunkReader{s: s, n: n}, size)
			}
		}
		for split := 1; split < len(s); split++ {
			// A long input is split near its ends, around each multiple of
			// the largest buffer, and at a sample of the positions between:
			// every position of a 5000-byte input, for every buffer size and
			// entry point, costs most of a minute and several under the race
			// detector, for cuts that differ only in where plain content is
			// divided.
			if len(s) > 512 && split > 96 && split < len(s)-96 && split%4096 > 2 && split%4096 < 4094 && split%61 != 0 {
				continue
			}
			out[fmt.Sprintf("buf %d split %d", size, split)] = func() *bufio.Reader {
				return bufio.NewReaderSize(io.MultiReader(strings.NewReader(s[:split]), strings.NewReader(s[split:])), size)
			}
		}
	}
	return out
}

// TestReadHeader_lineEndings pins each case's fields, raw bytes, the bytes left
// in the reader, and the reported line endings, identically under every
// delivery and buffer size, through ReadHeader and ReadHeaderLimits alike.
func TestReadHeader_lineEndings(t *testing.T) {
	for _, tc := range lineEndingCases() {
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
				if err != nil {
					t.Errorf("%s: err = %v", label, err)
					continue
				}
				if got := fieldsOf(t, h); !sameFields(got, tc.fields) {
					t.Errorf("%s: fields = %q, want %q", label, got, tc.fields)
				}
				rest, _ := io.ReadAll(br)
				if string(rest) != tc.rest {
					t.Errorf("%s: rest = %q, want %q", label, rest, tc.rest)
				}
				le := h.LineEndings()
				if !le.Known() {
					t.Errorf("%s: LineEndings() unknown for a header read whole", label)
				}
				if got := endsOf(le); got != tc.ends {
					t.Errorf("%s: line endings = %+v, want %+v", label, got, tc.ends)
				}
			}
		}
	}
}

// TestReadHeader_lineEndingsBufferEdges moves a stray '\r' across the buffer
// edges of the smallest buffers, followed by a field or by text that continues
// the value, so that the probe of the next line settles in every position.
func TestReadHeader_lineEndingsBufferEdges(t *testing.T) {
	for pad := 1; pad <= 40; pad++ {
		y := strings.Repeat("y", pad)
		for _, tc := range []struct {
			in     string
			fields []wantField
		}{
			{"X: " + y + "\rb: c\r\n\r\n", []wantField{{"X", y, "X: " + y + "\r\n"}, {"B", "c", "b: c\r\n"}}},
			{"X: " + y + "\rbc d\r\n\r\n", []wantField{{"X", y + "\rbc d", "X: " + y + "\rbc d\r\n"}}},
			{"X: " + y + "\rb\r c\r\n\r\n", []wantField{{"X", y + "\rb c", "X: " + y + "\rb\r\n c\r\n"}}},
		} {
			for _, size := range []int{16, 17, 32} {
				for _, n := range []int{1, 4096} {
					br := bufio.NewReaderSize(&chunkReader{s: tc.in, n: n}, size)
					h, err := ReadHeader(br)
					if got := fieldsOf(t, h); err != nil || !sameFields(got, tc.fields) {
						t.Errorf("%q buf %d chunk %d: (%q, %v), want %q", tc.in, size, n, got, err, tc.fields)
					}
				}
			}
		}
	}
}

// TestReadHeader_lineEndingsRejoinBudgets pins that a '\r' that turns out to
// be content makes one physical line and one field of what it joins, charged
// whole: exactly the joined size is read and one byte less is refused, where
// the line and the field began, under every buffer size and delivery.
func TestReadHeader_lineEndingsRejoinBudgets(t *testing.T) {
	for _, tc := range []struct {
		in         string
		line, fild int64 // the joined line's content, the joined field's wire bytes
	}{
		// The probe settles at the end of the line.
		{"Subject: aaaa\rbbbb\r\n\r\n", 18, 20},
		// The probe settles at a space followed by a name byte.
		{"Subject: aaaa\rbb cc\r\n\r\n", 19, 21},
	} {
		for name, newReader := range lineEndingReaders(tc.in) {
			for _, delta := range []int64{0, -1} {
				for _, kind := range []HeaderLimitKind{HeaderLimitLineBytes, HeaderLimitFieldBytes} {
					lim := HeaderLimits{MaxLineBytes: tc.line + delta}
					if kind == HeaderLimitFieldBytes {
						lim = HeaderLimits{MaxFieldBytes: tc.fild + delta}
					}
					label := fmt.Sprintf("%q/%s/%v%+d", tc.in, name, kind, delta)
					h, err := ReadHeaderLimits(newReader(), lim)
					if delta == 0 {
						if err != nil || h.Len() != 1 {
							t.Errorf("%s: (%v, %v), want the one joined field", label, h.Map(), err)
						}
						continue
					}
					e, ok := HeaderLimit(err)
					if !ok || e.Kind() != kind || e.UnitOffset() != 0 || h.Len() != 0 {
						t.Errorf("%s: (%v, %v), want %v refused at 0", label, h.Map(), err, kind)
					}
				}
			}
		}
	}
}

// TestReadHeader_lineEndingsSharedCount pins that a candidate a '\r' still has
// to settle is a candidate to the shared field budget: one that turns out to
// continue the field before it is never charged, and an exhausted budget
// refuses it at its first byte, as it refuses any candidate.
func TestReadHeader_lineEndingsSharedCount(t *testing.T) {
	// One field joined from two lines spends one field: after it, a budget of
	// two has room for exactly one more.
	b := NewHeaderBudget(0, 2)
	h, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("Subject: a\rb\r\n\r\n")), HeaderLimits{Total: b})
	if err != nil || h.Get("Subject") != "a\rb" {
		t.Fatalf("joined field = (%v, %v)", h.Map(), err)
	}
	if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("C: 1\r\n\r\n")), HeaderLimits{Total: b}); err != nil {
		t.Fatalf("second field = %v, want it to fit: the joined line was never charged", err)
	}
	_, err = ReadHeaderLimits(bufio.NewReader(strings.NewReader("D: 1\r\n\r\n")), HeaderLimits{Total: b})
	checkRefusal(t, err, HeaderLimitTotalCount, 2, 0, 0)

	// A spent count refuses the line after a '\r' at its first byte, before
	// anything settles whether it would have continued the field.
	b = NewHeaderBudget(0, 1)
	_, err = ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\rbc\r\n\r\n")), HeaderLimits{Total: b})
	checkRefusal(t, err, HeaderLimitTotalCount, 1, 0, int64(len("A: 1\r")))
}

// TestReadHeader_lineEndingsEndlessCandidate pins that a candidate a '\r' is
// still probing is bounded by its own budgets while it is undecided: an endless
// run of name bytes after the '\r' is refused where it began, after at most a
// buffer past the budget.
func TestReadHeader_lineEndingsEndlessCandidate(t *testing.T) {
	for _, kind := range []HeaderLimitKind{HeaderLimitLineBytes, HeaderLimitFieldBytes} {
		lim := HeaderLimits{MaxLineBytes: 100}
		if kind == HeaderLimitFieldBytes {
			lim = HeaderLimits{MaxFieldBytes: 100}
		}
		src := &cappedEndless{prefix: "A: 1\r"}
		h, err := ReadHeaderLimits(bufio.NewReader(src), lim)
		if errors.Is(err, errEndlessCap) {
			t.Fatalf("%v: read to the test's %d-byte safety cap", kind, endlessCap)
		}
		checkRefusal(t, err, kind, 100, 0, int64(len("A: 1\r")))
		if h.Len() != 0 || src.n > 2*4096 {
			t.Errorf("%v: header %v after %d bytes, want nothing and at most two buffers", kind, h.Map(), src.n)
		}
	}
}

// TestReadHeader_lineEndingsReadFailure cuts a block whose lines end with
// bare CRs, a fold, a stray '\r' that continues a value and a CR empty line, at
// every byte, with failures that are not io.EOF — a wrapped io.EOF among them —
// delivered with or after the data, from sticky and resuming sources: the
// failure is returned as itself, nothing is read after it, a block whose empty
// line was decided before it is read whole, and otherwise every field kept is
// one the whole block has, its last possibly with fewer lines.
func TestReadHeader_lineEndingsReadFailure(t *testing.T) {
	const block = "A: 1\rB: 2\r b\rc\r\nD: 4\r\r"
	const text = block + "body"
	full, err := ReadHeader(bufio.NewReader(strings.NewReader(text)))
	if err != nil {
		t.Fatal(err)
	}
	whole := fieldsOf(t, full)
	if want := []wantField{{"A", "1", "A: 1\r\n"}, {"B", "2 b\rc", "B: 2\r\n b\rc\r\n"}, {"D", "4", "D: 4\r\n"}}; !sameFields(whole, want) {
		t.Fatalf("whole block = %q, want %q", whole, want)
	}
	// The empty line's '\r' is decided by the byte after it.
	decided := len(block) + 1

	for _, fail := range []error{errEIO, fmt.Errorf("cut: %w", io.EOF), io.ErrUnexpectedEOF} {
		for cut := 0; cut <= len(text); cut++ {
			for _, size := range []int{16, 4096} {
				for _, chunk := range []int{1, 7, 4096} {
					for _, withData := range []bool{false, true} {
						results := map[bool]string{}
						for _, sticky := range []bool{true, false} {
							label := fmt.Sprintf("%v/cut %d/buf %d/chunk %d/with %v/sticky %v", fail, cut, size, chunk, withData, sticky)
							src := &failAtSource{data: text, cut: cut, chunk: chunk, err: fail, withData: withData, sticky: sticky}
							h, err := ReadHeader(bufio.NewReaderSize(src, size))
							if src.readsAfter != 0 {
								t.Errorf("%s: %d reads after the failure", label, src.readsAfter)
							}
							got := fieldsOf(t, h)
							results[sticky] = fmt.Sprintf("%q %v", got, err)
							if cut >= decided {
								if err != nil || !sameFields(got, whole) {
									t.Errorf("%s: (%q, %v), want the whole block", label, got, err)
								}
								continue
							}
							if err != fail {
								t.Errorf("%s: err = %v, want the input's own failure", label, err)
							}
							if len(got) > len(whole) {
								t.Fatalf("%s: %d fields, more than the whole block's %d", label, len(got), len(whole))
							}
							for i, f := range got {
								w := whole[i]
								if f.key != w.key || !strings.HasPrefix(w.raw, f.raw) || (i < len(got)-1 && f != w) {
									t.Errorf("%s: field %d = %q, want %q or its first lines", label, i, f, w)
								}
							}
						}
						if results[true] != results[false] {
							t.Errorf("%v/cut %d/buf %d/chunk %d/with %v: resuming %s, failing %s",
								fail, cut, size, chunk, withData, results[false], results[true])
						}
					}
				}
			}
		}
	}
}

const lineEndingBufferFullEnv = "EMLKIT_TEXTPROTO_LINEEND_BUFFER_FULL"

// TestReadHeader_lineEndingsSourceBufferFull pins a source that fails with
// bufio.ErrBufferFull itself inside a block framed by bare CRs: the scanner
// never asks bufio for more than its buffer holds, so that value is always the
// source's failure, reported as itself, and never read on. It runs in a
// subprocess with a deadline, since reading on without bytes would not end.
func TestReadHeader_lineEndingsSourceBufferFull(t *testing.T) {
	if os.Getenv(lineEndingBufferFullEnv) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadHeader_lineEndingsSourceBufferFull$", "-test.count=1")
		cmd.Env = append(os.Environ(), lineEndingBufferFullEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("subprocess = %v, deadline %v\n%s", err, ctx.Err(), out)
		}
		return
	}

	const block = "A: 1\rB: 2\r b\rc\r\nD: 4\r\r"
	const text = block + "body"
	for cut := 0; cut <= len(block); cut++ {
		for _, size := range []int{16, 4096} {
			for _, chunk := range []int{1, 7, 4096} {
				for _, withData := range []bool{false, true} {
					src := &failAtSource{data: text, cut: cut, chunk: chunk, err: bufio.ErrBufferFull, withData: withData, sticky: true}
					_, err := ReadHeader(bufio.NewReaderSize(src, size))
					if err != bufio.ErrBufferFull || src.readsAfter != 0 {
						t.Errorf("cut %d/buf %d/chunk %d/with %v: err = %v after %d reads past it, want the source's own failure and none",
							cut, size, chunk, withData, err, src.readsAfter)
					}
				}
			}
		}
	}
}

// TestHeader_lineEndingsSnapshot pins what LineEndings reports and when: known
// for a block read whole and for a Copy of it, unknown once the header is
// changed in any way and for a header built rather than read, and nothing for a
// value that names no line ending.
func TestHeader_lineEndingsSnapshot(t *testing.T) {
	read := func() Header {
		h, err := ReadHeader(bufio.NewReader(strings.NewReader("A: 1\nB: 2\r\nB: 3\r\n\r\n")))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	h := read()
	le := h.LineEndings()
	if !le.Known() || le.Count(LineEndingLF) != 1 || le.Count(LineEndingCRLF) != 3 {
		t.Fatalf("LineEndings() = %+v", endsOf(le))
	}
	for _, k := range []LineEnding{0, LineEndingCR + 1, 255} {
		if n := le.Count(k); n != 0 {
			t.Errorf("Count(%v) = %d, want 0", k, n)
		}
		if _, ok := le.First(k); ok {
			t.Errorf("First(%v) reports one", k)
		}
	}
	if s := LineEnding(0).String(); s != "LineEnding(0)" {
		t.Errorf("String() = %q", s)
	}

	c := h.Copy()
	if got := c.LineEndings(); got != le {
		t.Errorf("Copy().LineEndings() = %+v, want the original's", endsOf(got))
	}
	// A copy's snapshot is its own value: changing the copy leaves the
	// original known.
	c.Add("X", "1")
	if c.LineEndings().Known() || !h.LineEndings().Known() {
		t.Errorf("after changing a copy: copy known %v, original known %v", c.LineEndings().Known(), h.LineEndings().Known())
	}

	for name, change := range map[string]func(*Header){
		"Add":               func(h *Header) { h.Add("X", "1") },
		"AddRaw":            func(h *Header) { h.AddRaw([]byte("X: 1\r\n")) },
		"Set":               func(h *Header) { h.Set("A", "2") },
		"Del":               func(h *Header) { h.Del("A") },
		"Fields().Del":      func(h *Header) { fs := h.Fields(); fs.Next(); fs.Del() },
		"FieldsByKey().Del": func(h *Header) { fs := h.FieldsByKey("B"); fs.Next(); fs.Del() },
	} {
		h := read()
		change(&h)
		if le := h.LineEndings(); le.Known() || le.Count(LineEndingCRLF) != 0 {
			t.Errorf("%s: LineEndings() = known %v %+v, want unknown", name, le.Known(), endsOf(le))
		}
	}

	var zero Header
	var nilHeader *Header
	built := HeaderFromMap(map[string][]string{"A": {"1"}})
	for name, l := range map[string]LineEndings{
		"zero":          zero.LineEndings(),
		"nil":           nilHeader.LineEndings(),
		"HeaderFromMap": built.LineEndings(),
	} {
		if l.Known() || l != (LineEndings{}) {
			t.Errorf("%s: LineEndings() = %+v, want unknown", name, endsOf(l))
		}
		if _, _, ok := l.Unterminated(); ok {
			t.Errorf("%s: Unterminated() reports one", name)
		}
	}

	// A refused or failed block has no header to report on.
	h, _ = ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\n\r\n")), HeaderLimits{MaxBytes: 3})
	if h.LineEndings().Known() {
		t.Error("a refused block reports line endings")
	}
	h, _ = ReadHeader(bufio.NewReader(&failAtSource{data: "A: 1\r\nB", cut: 7, chunk: 4096, err: errEIO, sticky: true}))
	if h.LineEndings().Known() {
		t.Error("a failed block reports line endings")
	}
}

// lineEndingOutcome is everything one read of a block yields that a caller can
// observe: the fields with their raw bytes, the line-ending report, the byte
// order mark, what is left in the reader, and the error, by its text.
type lineEndingOutcome struct {
	fields  []wantField
	ends    LineEndings
	bom     ByteOrderMark
	prefix  prefixOutcome
	records string // the orphan records (ROB-020-002), with their locations
	rest    string
	err     string
}

// prefixOutcome is what a Header reports of its malformed prefix (ROB-020-001).
type prefixOutcome struct {
	present    bool
	bytes      string
	start, end int64
	lines      int
}

func prefixOutcomeOf(h *Header) prefixOutcome {
	p := h.MalformedPrefix()
	start, end := p.Location()
	return prefixOutcome{present: p.Present(), bytes: string(p.Bytes()), start: start, end: end, lines: p.Lines()}
}

func readLineEndingOutcome(t *testing.T, br *bufio.Reader) lineEndingOutcome {
	t.Helper()
	h, err := ReadHeader(br)
	o := lineEndingOutcome{fields: fieldsOf(t, h), ends: h.LineEndings(), bom: h.ByteOrderMark(), prefix: prefixOutcomeOf(&h),
		records: fmt.Sprintf("%+v", recordsOf(&h))}
	if err != nil {
		o.err = err.Error()
	}
	rest, rerr := io.ReadAll(br)
	if rerr != nil {
		t.Fatalf("reading what follows the block = %v", rerr)
	}
	o.rest = string(rest)
	return o
}

// FuzzReadHeader_lineEndings holds an arbitrary block, of at most 8 KiB, to one
// outcome whatever its delivery: read whole through the default buffer, and
// one byte or seven at a time, with io.EOF after or together with the last
// bytes, through the smallest buffer and the default one. It compares
// deliveries with each other rather than with a second parser; every source is
// a finite string.
func FuzzReadHeader_lineEndings(f *testing.F) {
	for _, tc := range lineEndingCases() {
		f.Add([]byte(tc.in))
	}
	f.Add([]byte("A: 1\r\nLong: " + strings.Repeat("x", 40) + "\r\n\tcont\r\n\r\nbody"))
	f.Add([]byte("X: " + strings.Repeat("y", 14) + "\rb c\r\n\r\nbody"))
	f.Add([]byte(" lead\r\n\r\n"))
	f.Add([]byte("no colon\r\nA: 1\r\n\r\n"))
	// Byte order marks, whole, partial, repeated and wide (ROB-010-003).
	for _, tc := range byteOrderMarkCases() {
		f.Add([]byte(tc.in))
	}
	// Malformed prefixes (ROB-020-001).
	for _, tc := range prefixCases() {
		f.Add([]byte(tc.in))
	}
	// Orphan continuation lines (ROB-020-002).
	for _, tc := range orphanCases() {
		f.Add([]byte(tc.in))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			return
		}
		s := string(data)
		want := readLineEndingOutcome(t, bufio.NewReader(strings.NewReader(s)))
		for name, newReader := range map[string]func() *bufio.Reader{
			"chunks 1, buf 16":       func() *bufio.Reader { return bufio.NewReaderSize(&chunkReader{s: s, n: 1}, 16) },
			"chunks 7, buf 16":       func() *bufio.Reader { return bufio.NewReaderSize(&chunkReader{s: s, n: 7}, 16) },
			"chunks 1, default":      func() *bufio.Reader { return bufio.NewReader(&chunkReader{s: s, n: 1}) },
			"whole, buf 16":          func() *bufio.Reader { return bufio.NewReaderSize(strings.NewReader(s), 16) },
			"data with eof, buf 16":  func() *bufio.Reader { return bufio.NewReaderSize(iotest.DataErrReader(strings.NewReader(s)), 16) },
			"data with eof, default": func() *bufio.Reader { return bufio.NewReader(iotest.DataErrReader(strings.NewReader(s))) },
			"chunks 7 with eof, buf 16": func() *bufio.Reader {
				return bufio.NewReaderSize(iotest.DataErrReader(&chunkReader{s: s, n: 7}), 16)
			},
		} {
			got := readLineEndingOutcome(t, newReader())
			if !sameFields(got.fields, want.fields) || got.ends != want.ends || got.bom != want.bom || got.prefix != want.prefix ||
				got.records != want.records || got.rest != want.rest || got.err != want.err {
				t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
			}
		}
		checkPrefixOutcome(t, s, want)
		checkOrphanOutcome(t, s)
	})
}

// checkOrphanOutcome holds the orphan records of one read of a finite string s
// to what the text alone says: each is the input's own bytes at its location,
// has at least two lines, begins with a byte that is neither white space nor
// a line ending, and is followed by a byte that does not begin a continuation
// line; records do not overlap the prefix or each other. Written back and read
// again, the records are what was written of them, and the fields are the
// same.
func checkOrphanOutcome(t *testing.T, s string) {
	t.Helper()
	h, _ := ReadHeader(bufio.NewReader(strings.NewReader(s)))
	_, prev := h.MalformedPrefix().Location()
	for _, r := range recordsOf(&h) {
		if r.start < prev || r.end > int64(len(s)) || s[r.start:r.end] != r.bytes || r.lines < 2 {
			t.Fatalf("record %+v is not the input's bytes at its location after %d", r, prev)
		}
		if c := r.bytes[0]; c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			t.Fatalf("record %q begins with %q", r.bytes, c)
		}
		if r.end < int64(len(s)) && (s[r.end] == ' ' || s[r.end] == '\t') {
			t.Fatalf("record %q is followed by a continuation line", r.bytes)
		}
		prev = r.end
	}
	var b bytes.Buffer
	if err := WriteHeader(&b, h); err != nil {
		return
	}
	again, _ := ReadHeader(bufio.NewReader(&b))
	if got, want := recordBytes(recordsOf(&again)), writtenRecords(recordsOf(&h)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("read back, the records are %q, want what was written of them, %q", got, want)
	}
	if !sameFields(fieldsOf(t, again), fieldsOf(t, h)) {
		t.Errorf("read back, the fields are %q, want %q", fieldsOf(t, again), fieldsOf(t, h))
	}
}

// checkPrefixOutcome holds the malformed prefix of one read of a finite string
// s, which no read failure but io.EOF ends, to what the text alone says: a
// block not left unread begins with one exactly when its first byte after any
// UTF-8 mark is a space or a tab; its bytes are the input's own at its
// location, from that byte; it has at least one line. Written back and read
// again, the prefix is what was written of it.
func checkPrefixOutcome(t *testing.T, s string, o lineEndingOutcome) {
	t.Helper()
	off := 0
	if o.bom == BOMUTF8 {
		off = 3
	}
	if o.bom.wide() {
		if o.prefix != (prefixOutcome{}) {
			t.Errorf("a block left unread reports a prefix %+v", o.prefix)
		}
		return
	}
	starts := len(s) > off && (s[off] == ' ' || s[off] == '\t')
	if o.prefix.present != starts {
		t.Fatalf("prefix present = %v, want %v for %q", o.prefix.present, starts, s)
	}
	if !starts {
		if o.prefix != (prefixOutcome{}) {
			t.Errorf("no prefix, but %+v reported", o.prefix)
		}
		return
	}
	if o.prefix.start != int64(off) || o.prefix.end <= o.prefix.start || o.prefix.end > int64(len(s)) ||
		s[o.prefix.start:o.prefix.end] != o.prefix.bytes || o.prefix.lines < 1 {
		t.Fatalf("prefix %+v is not the input's bytes at its location", o.prefix)
	}
	h, _ := ReadHeader(bufio.NewReader(strings.NewReader(s)))
	var b bytes.Buffer
	if err := WriteHeader(&b, h); err != nil {
		return
	}
	again, _ := ReadHeader(bufio.NewReader(&b))
	if got, want := string(again.MalformedPrefix().Bytes()), writtenPrefix(o.prefix.bytes); got != want {
		t.Errorf("read back, the prefix is %q, want what was written of it, %q", got, want)
	}
}

// writtenPrefix is what WriteHeader writes of a prefix p: p, and a line ending
// after a last line that has none or ends with a bare '\r'.
func writtenPrefix(p string) string {
	switch {
	case p == "" || strings.HasSuffix(p, "\n"):
		return p
	case strings.HasSuffix(p, "\r"):
		return p + "\n"
	}
	return p + "\r\n"
}
