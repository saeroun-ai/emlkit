package textproto

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"math/rand"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// Boundary tests of the bounds audit of the wire and framing code
// (ROB-120-001; see docs/robustness-notes/bounds-audit-core.md). They are
// written to mean the same on a 32-bit int: a budget that does not fit in 32
// bits must behave as the large budget it is, and every length that is
// compared with one is compared as an int64.

// baDelOneAtATime is Header.Del as it was before the audit: each field of key
// k removed on its own, last to first.
func baDelOneAtATime(l []*headerField, k string) []*headerField {
	for i := len(l) - 1; i >= 0; i-- {
		if l[i].k == k {
			l = append(l[:i], l[i+1:]...)
		}
	}
	return l
}

// Del removes in one pass what it removed one field at a time, and leaves the
// array it shares with copies of the Header value exactly as it did.
func TestBoundsCore_delMatchesOneAtATime(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 300; round++ {
		n := rng.Intn(9)
		fs := make([]*headerField, n)
		for i := range fs {
			fs[i] = &headerField{k: []string{"K", "A", "B"}[rng.Intn(3)], v: fmt.Sprint(i)}
		}
		want := append([]*headerField(nil), fs...)
		wantKept := baDelOneAtATime(want, "K")

		h := Header{l: append([]*headerField(nil), fs...), m: makeHeaderMap(fs)}
		alias := h
		h.Del("k")
		if len(h.l) != len(wantKept) || h.Has("K") {
			t.Fatalf("round %d: Del kept %d fields, want %d", round, len(h.l), len(wantKept))
		}
		for i := range want {
			if alias.l[i] != want[i] {
				t.Fatalf("round %d: slot %d of the shared array differs from the one-at-a-time result", round, i)
			}
		}
	}
}

// Del is linear in the header: fields of the key after many others cost one
// pass, not one move of all the others each. Before the audit this header
// took the square of its length, tens of seconds.
func TestBoundsCore_delIsLinear(t *testing.T) {
	const n = 200000
	fs := make([]*headerField, 0, 2*n)
	for i := 0; i < n; i++ {
		fs = append(fs, &headerField{k: "Subject"})
	}
	for i := 0; i < n; i++ {
		fs = append(fs, &headerField{k: "X"})
	}
	h := Header{l: fs, m: makeHeaderMap(fs)}
	start := time.Now()
	h.Del("Subject")
	if h.Len() != n || h.Has("Subject") {
		t.Fatalf("Len() = %d, want %d", h.Len(), n)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Del took %v", d)
	}
}

// baFramingInput is a multipart body, boundary "b", whose framing lines run
// past one buffer of the reader in every place a line is held outside it: a
// preamble line, a line that only begins like a delimiter, delimiter lines
// padded with white space, a candidate in a body that turns out to be body, a
// delimiter that ends a header block with a malformed prefix, and a damaged
// header line whose bare CR is settled past the buffer.
func baFramingInput() string {
	pad := func(c string) string { return strings.Repeat(c, 9000) }
	return pad("p") + "\r\n" +
		"--b" + pad(" ") + "x\r\n" +
		"--b" + pad(" ") + "\r\n" +
		"A: 1\r\n\r\n" +
		"one\r\n--b" + pad("\t") + "y\r\n" +
		"more\r\n--b" + pad(" ") + "\r\n" +
		" prefix\r\nB: 2\r\n--b" + pad("\t") + "\r\n" +
		"C: 3\r\nbad line\rX" + pad("z") + "\r\n cont\r\n\r\nthree\r\n" +
		"--b--" + pad(" ") + "\r\n" +
		"epilogue"
}

// baFrame reads input as a multipart of boundary "b" under limits and a
// preamble limit, and returns everything it yielded as text.
func baFrame(input io.Reader, limits HeaderLimits, preamble int64) string {
	var out strings.Builder
	mr := NewMultipartReader(input, "b")
	mr.SetHeaderLimits(limits)
	mr.SetPreambleLimit(preamble)
	for i := 0; i < 64; i++ {
		p, err := mr.NextPart()
		if err != nil {
			fmt.Fprintf(&out, "end: %v\n", err)
			break
		}
		fmt.Fprintf(&out, "part at %d/%d sep-missing %v prefix %d orphans %d:", p.HeaderOffset(), p.BodyOffset(),
			p.HeaderSeparatorMissing(), len(p.Header.MalformedPrefix().Bytes()), len(p.Header.OrphanContinuations()))
		for f := p.Header.Fields(); f.Next(); {
			fmt.Fprintf(&out, " %s=%d", f.Key(), len(f.Value()))
		}
		// The copy is capped: a body cannot be longer than the input.
		body, err := io.ReadAll(io.LimitReader(p, 1<<20))
		fmt.Fprintf(&out, "\nbody %d bytes %.24q: %v\n", len(body), body, err)
	}
	pre, over := mr.Preamble()
	fmt.Fprintf(&out, "preamble %d over %v framing %+v\n", len(pre), over, mr.Framing())
	return out.String()
}

// A budget past 32 bits, and math.MaxInt64, frame exactly as a budget that
// fits in 32 bits and is larger than every line: none is truncated on its way
// to a comparison with a length, whole or a byte at a time.
func TestBoundsCore_hugeBudgetsFrameAlike(t *testing.T) {
	input := baFramingInput()
	limitsOf := func(v int64, recover bool) HeaderLimits {
		return HeaderLimits{MaxBytes: v, MaxFieldBytes: v, MaxCount: v, MaxLineBytes: v, Total: NewHeaderBudget(v, v), Recover: recover}
	}
	for _, recover := range []bool{false, true} {
		want := baFrame(strings.NewReader(input), limitsOf(1<<20, recover), 1<<20)
		if strings.Count(want, "part at") != 3 || !strings.Contains(want, "sep-missing true prefix 9") ||
			!strings.Contains(want, "orphans 1") || !strings.Contains(want, "end: EOF") || !strings.Contains(want, "Closed:true") {
			t.Fatalf("recover %v: the reference framing is not the three parts and the closing delimiter:\n%s", recover, want)
		}
		for _, v := range []int64{1 << 31, 1<<32 + 7, 1 << 40, math.MaxInt64} {
			if got := baFrame(strings.NewReader(input), limitsOf(v, recover), v); got != want {
				t.Errorf("recover %v, budget %d:\n got %s\nwant %s", recover, v, got, want)
			}
		}
		if got := baFrame(iotest.OneByteReader(strings.NewReader(input)), limitsOf(math.MaxInt64, recover), math.MaxInt64); got != want {
			t.Errorf("recover %v, a byte at a time:\n got %s\nwant %s", recover, got, want)
		}
	}
}

// A header block is read alike under budgets past 32 bits, the field held
// while a bare CR is settled past the reader's buffer included.
func TestBoundsCore_hugeBudgetsReadHeaderAlike(t *testing.T) {
	input := "A: 1\r\nbad line\rX" + strings.Repeat("z", 9000) + "\r\n cont\r\nB: " + strings.Repeat("v", 70000) + "\r\n\r\nbody"
	read := func(v int64) string {
		br := bufio.NewReaderSize(strings.NewReader(input), 16)
		h, err := ReadHeaderLimits(br, HeaderLimits{MaxBytes: v, MaxFieldBytes: v, MaxCount: v, MaxLineBytes: v, Total: NewHeaderBudget(v, v)})
		rest, _ := io.ReadAll(br)
		var buf bytes.Buffer
		WriteHeader(&buf, h)
		return fmt.Sprintf("%v fields %d orphans %d rest %q written %d", err, h.Len(), len(h.OrphanContinuations()), rest, buf.Len())
	}
	want := read(1 << 20)
	if !strings.Contains(want, "<nil> fields 2 orphans 1 rest \"body\"") {
		t.Fatalf("reference read: %s", want)
	}
	for _, v := range []int64{1 << 31, 1 << 40, math.MaxInt64} {
		if got := read(v); got != want {
			t.Errorf("budget %d: %s, want %s", v, got, want)
		}
	}
}

// A boundary longer than the reader's buffer can never be matched in a body.
// It must end the iteration, not index past the buffer or read for ever.
func TestBoundsCore_boundaryLongerThanBuffer(t *testing.T) {
	boundary := strings.Repeat("B", peekBufferSize+904)
	input := "pre\r\n--" + boundary + "\r\nA: 1\r\n\r\nbody\r\n--" + boundary + "\r\nC: 2\r\n\r\ntwo\r\n--" + boundary + "--\r\n"
	for _, limits := range []HeaderLimits{{}, {MaxLineBytes: 1 << 20}, {MaxLineBytes: 1 << 20, Recover: true}, {MaxLineBytes: 64}} {
		for _, src := range []io.Reader{strings.NewReader(input), iotest.OneByteReader(strings.NewReader(input))} {
			mr := NewMultipartReader(src, boundary)
			mr.SetHeaderLimits(limits)
			var read int64
			parts := 0
			for ; parts < 8; parts++ {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				n, _ := io.Copy(io.Discard, io.LimitReader(p, 1<<20))
				read += n
			}
			if parts == 8 || read > int64(len(input)) {
				t.Errorf("limits %+v: %d parts, %d body bytes of an input of %d", limits, parts, read, len(input))
			}
		}
	}
}

// baLiar is a source that breaks the io.Reader contract: every count it
// reports is off by extra.
type baLiar struct {
	s     string
	extra int
	calls int
}

func (r *baLiar) Read(p []byte) (int, error) {
	if r.calls++; r.calls > 1000 {
		return 0, io.ErrNoProgress
	}
	if r.s == "" {
		return r.extra, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n + r.extra, nil
}

// A source that reports a count outside [0, len(p)] does not reach the
// reader's bufio.Reader with it, which panics on a negative count and slices
// past its buffer on one too large.
func TestBoundsCore_sourceCountOutOfRange(t *testing.T) {
	input := "pre\r\n--b\r\nA: 1\r\n\r\none\r\n--b\r\n\r\ntwo\r\n--b--\r\n"
	for _, extra := range []int{1, 1 << 20, -1, -(1 << 20), math.MaxInt, math.MinInt} {
		mr := NewMultipartReader(&baLiar{s: input, extra: extra}, "b")
		parts := 0
		for ; parts < 8; parts++ {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			io.Copy(io.Discard, io.LimitReader(p, 1<<20))
		}
		if extra > 0 && parts != 2 {
			t.Errorf("extra %d: %d parts, want 2", extra, parts)
		}
	}
}

// FuzzBoundsCore_framingBudgets frames arbitrary input under arbitrary
// budgets — negative, tiny, and past 32 bits — and in arbitrary chunks. No
// budget may make the reader panic, loop, hand out more body than the input
// holds, or keep more preamble than its limit.
func FuzzBoundsCore_framingBudgets(f *testing.F) {
	f.Add([]byte(baFramingInput()), int64(1<<40), int64(1<<40), uint8(0), true)
	f.Add([]byte(baFramingInput()), int64(100), int64(7), uint8(1), true)
	f.Add([]byte(baFramingInput()), int64(-1), int64(-1), uint8(3), false)
	f.Add([]byte(baFramingInput()), int64(math.MaxInt64), int64(math.MaxInt64), uint8(5), true)
	f.Add([]byte(baFramingInput()), int64(math.MinInt64), int64(math.MinInt64), uint8(2), true)
	f.Add([]byte("x\r\n--b\r\n k\rv\r\n--b \r"), int64(3), int64(1), uint8(1), true)
	f.Add([]byte("--b\nA\r :\r\n--b--"), int64(1), int64(0), uint8(4), false)

	f.Fuzz(func(t *testing.T, data []byte, budget, preamble int64, chunk uint8, recover bool) {
		if len(data) > 128<<10 {
			return
		}
		// The framing runs beside a watchdog: a reader that stops making
		// progress fails the run instead of hanging it.
		done := make(chan string, 1)
		go func() { done <- baFuzzFrame(data, budget, preamble, chunk, recover) }()
		select {
		case msg := <-done:
			if msg != "" {
				t.Fatal(msg)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("no end after 20s: %d bytes, budget %d, preamble %d, chunk %d, recover %v", len(data), budget, preamble, chunk, recover)
		}
	})
}

// baFuzzFrame is one run of FuzzBoundsCore_framingBudgets; it returns what
// was wrong, or "".
func baFuzzFrame(data []byte, budget, preamble int64, chunk uint8, recover bool) string {
	var src io.Reader = bytes.NewReader(data)
	switch chunk % 3 {
	case 1:
		src = iotest.OneByteReader(src)
	case 2:
		src = iotest.HalfReader(src)
	}
	mr := NewMultipartReader(src, "b")
	mr.SetHeaderLimits(HeaderLimits{MaxBytes: budget, MaxFieldBytes: budget, MaxCount: budget, MaxLineBytes: budget,
		Total: NewHeaderBudget(budget, budget), Recover: recover})
	mr.SetPreambleLimit(preamble)
	var body int64
	// Every part but the last consumes a delimiter line of at least 3 bytes.
	for parts := 0; ; parts++ {
		if parts > len(data)/3+2 {
			return fmt.Sprintf("%d parts from %d bytes", parts, len(data))
		}
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		n, _ := io.Copy(io.Discard, io.LimitReader(p, int64(len(data))+1))
		if n2, ended := p.BodyLen(); n2 != n || !ended {
			return fmt.Sprintf("BodyLen() = %d, %v after reading %d", n2, ended, n)
		}
		body += n
		var w bytes.Buffer
		WriteHeader(&w, p.Header)
	}
	if body > int64(len(data)) {
		return fmt.Sprintf("%d body bytes from %d bytes of input", body, len(data))
	}
	limit := preamble
	if limit <= 0 {
		limit = defaultPreambleMax
	}
	if pre, _ := mr.Preamble(); int64(len(pre)) > limit || len(pre) > len(data) {
		return fmt.Sprintf("preamble of %d bytes: limit %d, input %d", len(pre), limit, len(data))
	}
	return ""
}
