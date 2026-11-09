package textproto

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"testing/iotest"
)

// The header block budgets beyond the block's own bytes: the field count, one
// logical field's bytes, and one physical line's bytes. ReadHeaderLimits is
// where all four meet, so this is where the exact unit each counts, and the
// exact position each reports, are pinned.

// readHeaderLimitsFrom reads a header block from s under limits, through a
// reader of the given buffer size, and reports the header, what remained unread,
// and the error.
func readHeaderLimitsFrom(s string, limits HeaderLimits, bufSize int) (Header, string, error) {
	br := bufio.NewReaderSize(strings.NewReader(s), bufSize)
	h, err := ReadHeaderLimits(br, limits)
	rest, _ := io.ReadAll(br)
	return h, string(rest), err
}

// A limitSpec is one budget of HeaderLimits: how to set it, what it counts, and
// what a refusal of it says.
type limitSpec struct {
	name     string
	kind     HeaderLimitKind
	sentinel error
	set      func(*HeaderLimits, int64)
}

func limitSpecs() []limitSpec {
	return []limitSpec{
		{"bytes", HeaderLimitBytes, ErrHeaderTooBig, func(l *HeaderLimits, v int64) { l.MaxBytes = v }},
		{"field bytes", HeaderLimitFieldBytes, ErrHeaderFieldTooBig, func(l *HeaderLimits, v int64) { l.MaxFieldBytes = v }},
		{"count", HeaderLimitCount, ErrTooManyHeaderFields, func(l *HeaderLimits, v int64) { l.MaxCount = v }},
		{"line bytes", HeaderLimitLineBytes, ErrLineTooBig, func(l *HeaderLimits, v int64) { l.MaxLineBytes = v }},
	}
}

// only returns limits with one budget set and the rest off.
func (s limitSpec) only(v int64) HeaderLimits {
	var l HeaderLimits
	s.set(&l, v)
	return l
}

// TestReadHeaderLimits_countBoundary sweeps the field count across the exact
// number of candidates a block yields, and pins that every candidate is charged
// — including one whose name is empty, which the block does not end up with —
// and that a folded field is one candidate however many lines it spans.
func TestReadHeaderLimits_countBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   int64 // candidates
		// unit is the offset of the field a budget of want-1 refuses.
		unit int64
	}{
		{
			name:   "plain fields",
			header: "A: 1\r\nB: 2\r\nC: 3\r\n\r\n",
			want:   3,
			unit:   12,
		},
		{
			name:   "an empty name is still a candidate",
			header: "A: 1\r\n: x\r\n: y\r\nB: 2\r\n\r\n",
			want:   4,
			unit:   16,
		},
		{
			name:   "a folded field is one candidate",
			header: "A: 1\r\n cont\r\n cont\r\nB: 2\r\n\r\n",
			want:   2,
			unit:   20,
		},
		{
			name:   "lf line endings",
			header: "A: 1\nB: 2\nC: 3\n\n",
			want:   3,
			unit:   10,
		},
		{
			name:   "no terminating blank line",
			header: "A: 1\r\nB: 2\r\n",
			want:   2,
			unit:   6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, delta := range []int64{-1, 0, 1} {
				limit := tc.want + delta
				h, _, err := readHeaderLimitsFrom(tc.header, HeaderLimits{MaxCount: limit}, 4096)

				if delta >= 0 {
					if isRefusal(err) {
						t.Errorf("limit%+d: %v, want %d candidates to fit a budget of %d", delta, err, tc.want, limit)
					}
					continue
				}
				checkRefusal(t, err, HeaderLimitCount, limit, 0, tc.unit)
				if h.Len() != 0 {
					t.Errorf("limit%+d: a refused block yielded %d fields, want none", delta, h.Len())
				}
			}
		})
	}
}

// TestReadHeaderLimits_fieldBytesBoundary sweeps one logical field's byte budget
// across the exact wire size of a field, its continuation lines and every line
// terminator among them included.
func TestReadHeaderLimits_fieldBytesBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before string
		field  string
		// after is what follows the field, which is the empty line that ends
		// the block — or nothing at all, for an input that stops mid-field.
		after string
	}{
		{"one line", "A: short\r\n", "B: " + strings.Repeat("x", 100) + "\r\n", "\r\n"},
		{"folded", "A: short\r\n", "B: one\r\n two\r\n three\r\n", "\r\n"},
		{"folded with lf", "A: short\n", "B: one\n two\n three\n", "\n"},
		{"folded with mixed endings", "A: short\r\n", "B: one\n two\r\n three\n", "\r\n"},
		{"first field of the block", "", "A: " + strings.Repeat("x", 100) + "\r\n", "\r\n"},
		{"unterminated last field", "A: short\r\n", "B: " + strings.Repeat("x", 100), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := tc.before + tc.field + tc.after
			size := int64(len(tc.field))
			unit := int64(len(tc.before))

			for _, delta := range []int64{-1, 0, 1} {
				limit := size + delta
				h, _, err := readHeaderLimitsFrom(header, HeaderLimits{MaxFieldBytes: limit}, 4096)

				if delta >= 0 {
					if isRefusal(err) {
						t.Errorf("limit%+d: %v, want a field of %d bytes to fit a budget of %d", delta, err, size, limit)
					}
					continue
				}
				checkRefusal(t, err, HeaderLimitFieldBytes, limit, 0, unit)
				if h.Len() != 0 {
					t.Errorf("limit%+d: a refused block yielded %d fields, want none", delta, h.Len())
				}
			}
		})
	}
}

// TestReadHeaderLimits_lineBytesBoundary sweeps one physical line's byte budget
// across the exact length of a line, not counting the terminator that ends it —
// at each line ending, where the endings are mixed, where the line is a
// continuation, and where the input ends without terminating the line at all.
func TestReadHeaderLimits_lineBytesBoundary(t *testing.T) {
	long := strings.Repeat("x", 100)

	for _, tc := range []struct {
		name   string
		before string
		line   string
		eol    string
		after  string
	}{
		{"crlf", "A: short\r\n", "B: " + long, "\r\n", "\r\n"},
		{"lf", "A: short\n", "B: " + long, "\n", "\n"},
		{"mixed endings", "A: short\n", "B: " + long, "\r\n", "\n"},
		{"a continuation line", "A: short\r\n", " " + long, "\r\n", "\r\n"},
		{"the first line of the block", "", "A: " + long, "\r\n", "\r\n"},
		{"no terminator at all", "A: short\r\n", "B: " + long, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := tc.before + tc.line + tc.eol + tc.after
			size := int64(len(tc.line))
			unit := int64(len(tc.before))

			for _, delta := range []int64{-1, 0, 1} {
				limit := size + delta
				h, _, err := readHeaderLimitsFrom(header, HeaderLimits{MaxLineBytes: limit}, 4096)

				if delta >= 0 {
					if isRefusal(err) {
						t.Errorf("limit%+d: %v, want a line of %d bytes to fit a budget of %d", delta, err, size, limit)
					}
					continue
				}
				checkRefusal(t, err, HeaderLimitLineBytes, limit, 0, unit)
				if h.Len() != 0 {
					t.Errorf("limit%+d: a refused block yielded %d fields, want none", delta, h.Len())
				}
			}
		})
	}
}

// TestReadHeaderLimits_lineBytesAcrossBufferBoundary pins the case the line
// budget is easiest to get wrong: a "\r\n" that straddles the reader's buffer,
// so that the '\r' arrives at the end of one read and the '\n' at the start of
// the next. The terminator is not content whichever read it arrives in, and a
// '\r' that turns out not to begin one is.
func TestReadHeaderLimits_lineBytesAcrossBufferBoundary(t *testing.T) {
	const bufSize = 4096

	for _, tc := range []struct {
		name string
		line string
	}{
		{
			// The '\r' of the terminator is the last byte of the first buffer.
			name: "terminator straddles the buffer",
			line: "A: " + strings.Repeat("x", bufSize-len("A: ")-1),
		},
		{
			// A carriage return in the middle of the line, right at the buffer
			// boundary, which is content rather than a terminator.
			name: "a bare cr at the boundary is content",
			line: "A: " + strings.Repeat("x", bufSize-len("A: ")-1) + "\rmore",
		},
		{
			name: "many carriage returns",
			line: "A: " + strings.Repeat("\r", bufSize),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := tc.line + "\r\n\r\n"
			size := int64(len(tc.line))

			for _, bufSize := range []int{16, 64, 4096} {
				for _, delta := range []int64{-1, 0} {
					limit := size + delta
					_, _, err := readHeaderLimitsFrom(header, HeaderLimits{MaxLineBytes: limit}, bufSize)
					refused := isRefusal(err)
					if want := delta < 0; refused != want {
						t.Errorf("buffer %d, limit%+d: refused = %v, want %v (err = %v)",
							bufSize, delta, refused, want, err)
					}
				}
			}
		})
	}
}

// TestReadHeaderLimits_chunkInvariance pins that each budget decides the same
// way however the input arrives and whatever buffer it is read through.
func TestReadHeaderLimits_chunkInvariance(t *testing.T) {
	const header = "A: one\r\nB: " + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + "\r\n cont\r\nC: three\r\n\r\n"

	for _, spec := range limitSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			// The smallest budget of this kind the block fits in, found by
			// sweeping upward from one.
			var size int64
			for size = 1; size < 4096; size++ {
				if _, _, err := readHeaderLimitsFrom(header, spec.only(size), 4096); !isRefusal(err) {
					break
				}
			}
			if size >= 4096 {
				t.Fatalf("no budget of kind %v admits the fixture", spec.kind)
			}

			sources := map[string]func() io.Reader{
				"whole":         func() io.Reader { return strings.NewReader(header) },
				"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(header)) },
			}
			for _, n := range []int{1, 2, 3, 7, 64, 4095, 4096} {
				sources[fmt.Sprintf("chunks of %d", n)] = func() io.Reader {
					return &chunkReader{s: header, n: n}
				}
			}
			for _, seed := range []int64{1, 2, 3} {
				sources[fmt.Sprintf("random seed %d", seed)] = func() io.Reader {
					return newRandomChunkReader(header, 13, seed)
				}
			}

			for name, src := range sources {
				for _, bufSize := range []int{16, 64, 4096} {
					for _, delta := range []int64{-1, 0} {
						br := bufio.NewReaderSize(src(), bufSize)
						_, err := ReadHeaderLimits(br, spec.only(size+delta))
						refused := isRefusal(err)
						if want := delta < 0; refused != want {
							t.Errorf("%s, buffer %d, limit%+d: refused = %v, want %v (err = %v)",
								name, bufSize, delta, refused, want, err)
						}
					}
				}
			}
		})
	}
}

// TestReadHeaderLimits_maxInt64IsFinite pins that math.MaxInt64 is an ordinary
// finite budget for each kind: the arithmetic that enforces it does not wrap
// into an accepted answer, and nothing real is refused by it.
func TestReadHeaderLimits_maxInt64IsFinite(t *testing.T) {
	for _, spec := range limitSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			h, rest, err := readHeaderLimitsFrom(budgetHeader+budgetBody, spec.only(math.MaxInt64), 4096)
			if err != nil {
				t.Fatalf("ReadHeaderLimits() = %v", err)
			}
			if h.Len() == 0 {
				t.Errorf("read no fields")
			}
			if rest != budgetBody {
				t.Errorf("rest = %q, want the body untouched", rest)
			}
		})
	}
}

// TestReadHeaderLimits_disabledAndUnset pins that a budget of zero or below is
// no budget: a block every budget would otherwise refuse is read whole.
func TestReadHeaderLimits_disabledAndUnset(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "X-Pad-%d: %s\r\n", i, strings.Repeat("x", 500))
	}
	b.WriteString("\r\n")
	header := b.String()

	for _, limits := range []HeaderLimits{
		{},
		{MaxBytes: -1, MaxFieldBytes: -1, MaxCount: -1, MaxLineBytes: -1},
		{MaxBytes: -1000, MaxFieldBytes: math.MinInt64, MaxCount: -2, MaxLineBytes: -3},
	} {
		h, _, err := readHeaderLimitsFrom(header, limits, 4096)
		if err != nil {
			t.Errorf("%+v: ReadHeaderLimits() = %v", limits, err)
		}
		if h.Len() != 100 {
			t.Errorf("%+v: read %d fields, want 100", limits, h.Len())
		}
	}
}

// TestReadHeaderLimits_precedence pins which budget is reported when one block
// exceeds several at once: the one bounding the smallest scope, and the same one
// whatever buffer the input is read through.
func TestReadHeaderLimits_precedence(t *testing.T) {
	const header = "A: " + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + "\r\n\r\n"

	for _, tc := range []struct {
		name   string
		limits HeaderLimits
		want   HeaderLimitKind
	}{
		{"line before field", HeaderLimits{MaxLineBytes: 4, MaxFieldBytes: 4}, HeaderLimitLineBytes},
		{"field before block", HeaderLimits{MaxFieldBytes: 4, MaxBytes: 4}, HeaderLimitFieldBytes},
		{"block before count", HeaderLimits{MaxBytes: 4, MaxCount: 1}, HeaderLimitBytes},
		{
			"line before everything",
			HeaderLimits{MaxLineBytes: 4, MaxFieldBytes: 4, MaxBytes: 4, MaxCount: 1},
			HeaderLimitLineBytes,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, bufSize := range []int{16, 64, 4096} {
				_, _, err := readHeaderLimitsFrom(header, tc.limits, bufSize)
				limit, ok := HeaderLimit(err)
				if !ok {
					t.Fatalf("buffer %d: ReadHeaderLimits() = %v, want a refusal", bufSize, err)
				}
				if limit.Kind() != tc.want {
					t.Errorf("buffer %d: Kind() = %v, want %v", bufSize, limit.Kind(), tc.want)
				}
			}
		})
	}
}

// TestReadHeaderLimits_blockEndIsNotAField pins the unit MaxFieldBytes counts at
// its lower boundary: the empty line that ends a block is not a logical field,
// so no field budget is charged for it and the smallest field budget there is
// cannot refuse a block for a field the block does not contain.
//
// A block that is nothing but that empty line has no field in it at all. Under
// MaxFieldBytes of 1 it must still be read, its header must be empty, and the
// body after it must be left untouched — the two bytes of a CRLF are the whole
// of the defect this pins, so the budget is set below them deliberately.
func TestReadHeaderLimits_blockEndIsNotAField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		body   string
		fields int
		limits HeaderLimits
	}{
		{"crlf empty header", "\r\nbody", "body", 0, HeaderLimits{MaxFieldBytes: 1}},
		{"lf empty header", "\nbody", "body", 0, HeaderLimits{MaxFieldBytes: 1}},
		{"crlf empty header, every budget at one", "\r\nbody", "body", 0, HeaderLimits{MaxFieldBytes: 1, MaxCount: 1, MaxLineBytes: 1}},
		{"lf empty header, every budget at one", "\nbody", "body", 0, HeaderLimits{MaxFieldBytes: 1, MaxCount: 1, MaxLineBytes: 1}},
		{"empty header, no body", "\r\n", "", 0, HeaderLimits{MaxFieldBytes: 1}},
		{
			// The block end after a real field is charged to no field either,
			// which a budget that exactly fits the field would otherwise expose.
			name:   "block end after a field the budget exactly fits",
			input:  "A: 1\r\n\r\nbody",
			body:   "body",
			fields: 1,
			limits: HeaderLimits{MaxFieldBytes: 6},
		},
		{
			name:   "block end after a field the budget exactly fits, lf",
			input:  "A: 1\n\nbody",
			body:   "body",
			fields: 1,
			limits: HeaderLimits{MaxFieldBytes: 5},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, bufSize := range []int{16, 64, 4096} {
				h, rest, err := readHeaderLimitsFrom(tc.input, tc.limits, bufSize)
				if err != nil {
					t.Fatalf("buffer %d: ReadHeaderLimits() = %v, want the block to be read", bufSize, err)
				}
				if rest != tc.body {
					t.Errorf("buffer %d: body = %q, want %q", bufSize, rest, tc.body)
				}
				if h.Len() != tc.fields {
					t.Errorf("buffer %d: Len() = %d, want %d", bufSize, h.Len(), tc.fields)
				}
			}
		})
	}
}

// TestReadHeaderLimits_blockEndChunkInvariance pins that the same holds however
// the input arrives. The empty line that ends a block is recognized from at most
// two bytes, and a CRLF split across two reads is the case that decides whether
// that recognition is a property of the input's chunking or of the parser.
func TestReadHeaderLimits_blockEndChunkInvariance(t *testing.T) {
	for _, tc := range []struct {
		input  string
		fields int
	}{
		{"\r\nbody", 0},
		{"\nbody", 0},
		{"A: 1\r\n\r\nbody", 1},
		{"A: 1\n\nbody", 1},
	} {
		for _, bufSize := range []int{16, 64, 4096} {
			for _, n := range []int{1, 2, 3, 7, 4095, 4096} {
				src := bufio.NewReaderSize(&chunkReader{s: tc.input, n: n}, bufSize)
				h, err := ReadHeaderLimits(src, HeaderLimits{MaxFieldBytes: 6})
				if err != nil {
					t.Errorf("%q buffer %d chunks of %d: %v", tc.input, bufSize, n, err)
					continue
				}
				rest, _ := io.ReadAll(src)
				if string(rest) != "body" {
					t.Errorf("%q buffer %d chunks of %d: body = %q, want %q", tc.input, bufSize, n, rest, "body")
				}
				if h.Len() != tc.fields {
					t.Errorf("%q buffer %d chunks of %d: Len() = %d, want %d", tc.input, bufSize, n, h.Len(), tc.fields)
				}
			}
		}
	}
}

// TestReadHeaderLimits_blockEndDoesNotConsumeCount pins that an empty block
// spends no field candidate. A budget of one candidate must admit a block with
// one field in it, and the block end must not have taken that candidate first.
func TestReadHeaderLimits_blockEndDoesNotConsumeCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  int
	}{
		{"empty block", "\r\n", 0},
		{"one field and the block end", "A: 1\r\n\r\n", 1},
		{"one field and the block end, lf", "A: 1\n\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, err := readHeaderLimitsFrom(tc.input, HeaderLimits{MaxCount: 1}, 4096)
			if err != nil {
				t.Fatalf("ReadHeaderLimits() = %v, want the block to be read", err)
			}
			if h.Len() != tc.want {
				t.Errorf("Len() = %d, want %d", h.Len(), tc.want)
			}
		})
	}
}

// TestReadHeaderLimits_blockEndPrecedence pins which budget refuses an empty
// block when several could.
//
// The block's own bytes are the only unit an empty block has: MaxBytes counts
// the empty line, and MaxFieldBytes counts nothing at all because there is no
// field. So a block budget of 1 against a CRLF block end is a refusal of the
// block, and it stays one whatever field budget accompanies it — a refusal
// naming a field would be a refusal of something the input never contained.
func TestReadHeaderLimits_blockEndPrecedence(t *testing.T) {
	for _, bufSize := range []int{16, 64, 4096} {
		_, _, err := readHeaderLimitsFrom("\r\nbody", HeaderLimits{MaxBytes: 1, MaxFieldBytes: 1}, bufSize)
		checkRefusal(t, err, HeaderLimitBytes, 1, 0, 0)
		if limit, ok := HeaderLimit(err); ok && limit.Kind() == HeaderLimitFieldBytes {
			t.Errorf("buffer %d: refused with %v, want the block budget: an empty block has no field to refuse", bufSize, limit.Kind())
		}
	}
}

// TestReadHeaderLimits_boundsWorkWhileReading pins that each budget stops an
// endless input rather than deciding after the block has been built. What is
// read past a budget is at most one of the reader's buffers.
func TestReadHeaderLimits_boundsWorkWhileReading(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader func() io.Reader
		limits HeaderLimits
	}{
		{
			name:   "endless fields",
			reader: func() io.Reader { return &endlessLineReader{line: "X: y\r\n"} },
			limits: HeaderLimits{MaxCount: 100},
		},
		{
			name:   "one endless line",
			reader: func() io.Reader { return &endlessLineReader{line: "xxxxxxxx"} },
			limits: HeaderLimits{MaxLineBytes: 1 << 16},
		},
		{
			name:   "one endless folded field",
			reader: func() io.Reader { return &endlessLineReader{line: " more\r\n"} },
			limits: HeaderLimits{MaxFieldBytes: 1 << 16},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &countingReader{r: tc.reader()}
			br := bufio.NewReaderSize(io.MultiReader(strings.NewReader("A: start\r\n"), src), 4096)

			h, err := ReadHeaderLimits(br, tc.limits)
			if !isRefusal(err) {
				t.Fatalf("ReadHeaderLimits() = %v, want a refusal", err)
			}
			if h.Len() != 0 {
				t.Errorf("a refused block yielded %d fields, want none", h.Len())
			}
			// Generously more than any budget here, and vastly less than an
			// input that never ends.
			if src.n > 1<<20 {
				t.Errorf("read %d bytes of an endless input, want the budget to stop it far sooner", src.n)
			}
		})
	}
}

// endlessLineReader repeats one string for ever.
type endlessLineReader struct {
	line string
	pos  int
}

func (r *endlessLineReader) Read(p []byte) (int, error) {
	n := copy(p, r.line[r.pos:])
	r.pos += n
	if r.pos == len(r.line) {
		r.pos = 0
	}
	return n, nil
}

// countingReader counts what was read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestReadHeaderLimit_isReadHeaderLimitsWithOneBudget pins that the older entry
// point is the newer one with only the block budget set, so that everything
// written against it keeps its meaning.
func TestReadHeaderLimit_isReadHeaderLimitsWithOneBudget(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "X-Pad-%d: %s\r\n", i, strings.Repeat("x", 500))
	}
	b.WriteString("\r\n")
	header := b.String()
	size := int64(len(header))

	for _, delta := range []int64{-1, 0} {
		one, _, oneErr := readHeaderLimitFrom(header, size+delta, 4096)
		many, _, manyErr := readHeaderLimitsFrom(header, HeaderLimits{MaxBytes: size + delta}, 4096)

		if one.Len() != many.Len() {
			t.Errorf("limit%+d: ReadHeaderLimit read %d fields, ReadHeaderLimits read %d",
				delta, one.Len(), many.Len())
		}
		if isRefusal(oneErr) != isRefusal(manyErr) {
			t.Errorf("limit%+d: ReadHeaderLimit = %v, ReadHeaderLimits = %v", delta, oneErr, manyErr)
		}
		if delta < 0 {
			checkRefusal(t, oneErr, HeaderLimitBytes, size+delta, 0, 0)
			checkRefusal(t, manyErr, HeaderLimitBytes, size+delta, 0, 0)
		}
	}
}

// TestHeaderLimitError_kinds pins what a refusal of each kind reports and what
// errors.Is finds through it.
func TestHeaderLimitError_kinds(t *testing.T) {
	for _, spec := range limitSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			e := newHeaderLimitError(spec.kind, 42, 7, 11)

			if e.Kind() != spec.kind {
				t.Errorf("Kind() = %v, want %v", e.Kind(), spec.kind)
			}
			if e.Limit() != 42 {
				t.Errorf("Limit() = %d, want 42", e.Limit())
			}
			if e.Offset() != 7 {
				t.Errorf("Offset() = %d, want 7: the first byte of the block", e.Offset())
			}
			if e.UnitOffset() != 11 {
				t.Errorf("UnitOffset() = %d, want 11: the first byte of what was refused", e.UnitOffset())
			}
			if !errors.Is(e, spec.sentinel) {
				t.Errorf("errors.Is(%v, %v) = false", e, spec.sentinel)
			}
			if e.Error() == "" {
				t.Errorf("Error() is empty")
			}

			// Every sentinel is distinct, so a refusal of one kind is not
			// mistaken for another.
			for _, other := range limitSpecs() {
				if other.kind == spec.kind {
					continue
				}
				if errors.Is(e, other.sentinel) {
					t.Errorf("a %v refusal also reports %v", spec.kind, other.sentinel)
				}
			}

			// And a value another package could write claims nothing.
			forged := &HeaderLimitError{}
			if _, ok := HeaderLimit(forged); ok {
				t.Errorf("HeaderLimit accepted a forged value")
			}
			if _, ok := HeaderLimit(e); !ok {
				t.Errorf("HeaderLimit rejected a genuine %v refusal", spec.kind)
			}
		})
	}
}

// TestHeaderLimitKind_stringsAreDistinct pins that every kind renders as
// something, and as something of its own, so an error text says which budget was
// reached.
func TestHeaderLimitKind_stringsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range limitSpecs() {
		s := spec.kind.String()
		if s == "" {
			t.Errorf("%d has no name", spec.kind)
		}
		if seen[s] {
			t.Errorf("two kinds render as %q", s)
		}
		seen[s] = true
	}
	if got := HeaderLimitKind(99).String(); !strings.Contains(got, "99") {
		t.Errorf("HeaderLimitKind(99).String() = %q, want the value in it", got)
	}
}

// checkRefusal pins that err is a genuine refusal of the given kind, budget and
// positions.
func checkRefusal(t *testing.T, err error, kind HeaderLimitKind, limit, block, unit int64) {
	t.Helper()

	e, ok := HeaderLimit(err)
	if !ok {
		t.Fatalf("err = %v, want a %v refusal", err, kind)
	}
	if e.Kind() != kind {
		t.Errorf("Kind() = %v, want %v", e.Kind(), kind)
	}
	if e.Limit() != limit {
		t.Errorf("Limit() = %d, want %d", e.Limit(), limit)
	}
	if e.Offset() != block {
		t.Errorf("Offset() = %d, want %d", e.Offset(), block)
	}
	if e.UnitOffset() != unit {
		t.Errorf("UnitOffset() = %d, want %d", e.UnitOffset(), unit)
	}
	if !errors.Is(err, kind.sentinel()) {
		t.Errorf("errors.Is(err, %v) = false", kind.sentinel())
	}
}
