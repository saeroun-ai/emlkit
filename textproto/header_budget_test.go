package textproto

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

// budgetSources are the ways one block is delivered. The header reader's
// buffer is fixed, so none of them may change what a shared budget decides.
func budgetSources(s string) map[string]func() io.Reader {
	out := map[string]func() io.Reader{
		"whole":         func() io.Reader { return strings.NewReader(s) },
		"one byte":      func() io.Reader { return iotest.OneByteReader(strings.NewReader(s)) },
		"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(s)) },
	}
	for _, n := range []int{2, 3, 7, 64, 4095, 4096} {
		out[fmt.Sprintf("chunks of %d", n)] = func() io.Reader { return &chunkReader{s: s, n: n} }
	}
	out["random"] = func() io.Reader { return newRandomChunkReader(s, 17, 7) }
	return out
}

// sharedBlock is one header block and the body text that follows it, which
// the reader may buffer ahead but must never charge.
type sharedBlock struct {
	header string
	fields int64 // field candidates in header
}

const sharedBody = "body bytes that are buffered ahead and never charged\r\n"

// readShared reads each block from its own reader, all under one shared
// budget, and returns what each read returned.
func readShared(blocks []sharedBlock, limits HeaderLimits, src func(string) io.Reader) ([]Header, []error) {
	hs := make([]Header, len(blocks))
	errs := make([]error, len(blocks))
	for i, b := range blocks {
		br := bufio.NewReader(src(b.header + sharedBody))
		hs[i], errs[i] = ReadHeaderLimits(br, limits)
	}
	return hs, errs
}

// sharedFixture is three blocks with folds, an empty-name candidate and an
// empty block, in the given line ending.
func sharedFixture(eol string) []sharedBlock {
	return []sharedBlock{
		{"Content-Type: multipart/mixed" + eol + "Subject: a" + eol + " folded" + eol + eol, 2},
		{": empty name" + eol + "X-A: 1" + eol + eol, 2},
		{eol, 0},
		{"X-B: 2" + eol + "X-C: 3" + eol + eol, 2},
	}
}

func TestHeaderBudget_bytesBoundary(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		blocks := sharedFixture(eol)
		var total int64
		for _, b := range blocks {
			total += int64(len(b.header))
		}
		last := len(blocks) - 1
		before := total - int64(len(blocks[last].header))

		for name, src := range budgetSources("") {
			_ = src
			for _, delta := range []int64{-1, 0, 1} {
				limit := total + delta
				b := NewHeaderBudget(limit, 0)
				hs, errs := readShared(blocks, HeaderLimits{Total: b}, func(s string) io.Reader { return budgetSources(s)[name]() })
				for i := 0; i < last; i++ {
					if errs[i] != nil {
						t.Fatalf("%q %s limit%+d: block %d: %v", eol, name, delta, i, errs[i])
					}
				}
				if delta >= 0 {
					if errs[last] != nil || b.Refusal() != nil {
						t.Fatalf("%q %s limit%+d: last block = %v, refusal %v", eol, name, delta, errs[last], b.Refusal())
					}
					if hs[last].Get("X-C") != "3" {
						t.Errorf("last header = %v", hs[last].Map())
					}
					continue
				}
				// The first byte past the budget is the last byte of the last
				// block, and the block yields nothing.
				checkRefusal(t, errs[last], HeaderLimitTotalBytes, limit, 0, limit-before)
				if errs[last] != error(b.Refusal()) {
					t.Errorf("refusal is not the budget's own value")
				}
				if hs[last].Len() != 0 {
					t.Errorf("refused block yielded %v", hs[last].Map())
				}
			}
		}
	}
}

func TestHeaderBudget_countBoundary(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		blocks := sharedFixture(eol)
		var total int64
		for _, b := range blocks {
			total += b.fields
		}
		last := len(blocks) - 1
		for name := range budgetSources("") {
			for _, delta := range []int64{-1, 0, 1} {
				limit := total + delta
				b := NewHeaderBudget(0, limit)
				hs, errs := readShared(blocks, HeaderLimits{Total: b}, func(s string) io.Reader { return budgetSources(s)[name]() })
				for i := 0; i < last; i++ {
					if errs[i] != nil {
						t.Fatalf("%s limit%+d: block %d: %v", name, delta, i, errs[i])
					}
				}
				if delta >= 0 {
					if errs[last] != nil {
						t.Fatalf("%s limit%+d: last block = %v", name, delta, errs[last])
					}
					continue
				}
				// The refused candidate is X-C, the second field of the block.
				checkRefusal(t, errs[last], HeaderLimitTotalCount, limit, 0, int64(len("X-B: 2"+eol)))
				if hs[last].Len() != 0 {
					t.Errorf("refused block yielded %v", hs[last].Map())
				}
			}
		}
	}
}

// TestHeaderBudget_preflightEndlessCandidate pins that an exhausted field count
// refuses the next candidate from its first byte, with every byte budget off,
// however long the candidate would be.
func TestHeaderBudget_preflightEndlessCandidate(t *testing.T) {
	// A bare '\r' at the start of a line is the empty line that ends the block
	// (ROB-010-002), so "\r" then 'x' for ever is no candidate: the block ends
	// after the '\r', empty and unrefused, having read at most one buffer.
	t.Run(`"\r"`, func(t *testing.T) {
		b := NewHeaderBudget(0, 1)
		if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\n\r\n")), HeaderLimits{Total: b}); err != nil {
			t.Fatal(err)
		}
		src := &cappedEndless{prefix: "\r"}
		h, err := ReadHeaderLimits(bufio.NewReader(src), HeaderLimits{Total: b})
		if err != nil || h.Len() != 0 {
			t.Fatalf("(%v, %v), want an empty header and no error", h.Map(), err)
		}
		if src.n > 4096 {
			t.Errorf("read %d bytes, want at most one buffer", src.n)
		}
	})
	for _, prefix := range []string{"X: ", ":", "X-no-colon", "\x00"} {
		t.Run(fmt.Sprintf("%q", prefix), func(t *testing.T) {
			b := NewHeaderBudget(0, 1)
			if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\n\r\n")), HeaderLimits{Total: b}); err != nil {
				t.Fatal(err)
			}
			src := &cappedEndless{prefix: prefix}
			h, err := ReadHeaderLimits(bufio.NewReader(src), HeaderLimits{Total: b})
			if errors.Is(err, errEndlessCap) {
				t.Fatalf("the candidate was read to the test's %d-byte safety cap", endlessCap)
			}
			checkRefusal(t, err, HeaderLimitTotalCount, 1, 0, 0)
			if h.Len() != 0 {
				t.Errorf("header = %v", h.Map())
			}
			if src.n > 4096 {
				t.Errorf("read %d bytes of the refused candidate, want at most one buffer", src.n)
			}
		})
	}
}

// errEndlessCap is what cappedEndless fails with once it has delivered
// endlessCap bytes, so that a budget that does not stop an endless candidate
// fails the test with a bounded read rather than exhausting memory.
var errEndlessCap = errors.New("test safety cap: endless source read to its cap")

const endlessCap = 64 << 10

// cappedEndless delivers prefix and then 'x' until endlessCap bytes in all,
// counting what it delivered.
type cappedEndless struct {
	prefix string
	n      int64
}

func (r *cappedEndless) Read(p []byte) (int, error) {
	if r.n >= endlessCap {
		return 0, errEndlessCap
	}
	p = p[:min(int64(len(p)), endlessCap-r.n)]
	n := copy(p, r.prefix)
	r.prefix = r.prefix[n:]
	for i := n; i < len(p); i++ {
		p[i] = 'x'
	}
	r.n += int64(len(p))
	return len(p), nil
}

// TestHeaderBudget_preflightNonCandidates pins what an exhausted field count
// does not refuse: the end of a block, the end of the input, and a fold of a
// field already admitted. A leading-whitespace line, which used to keep its own
// malformed-header error here, now begins a malformed prefix, which is a
// candidate (ROB-020-001; see TestReadHeader_malformedPrefixBudgets).
func TestHeaderBudget_preflightNonCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		fields      int
	}{
		{"empty block crlf", "\r\n", 0},
		{"empty block lf", "\n", 0},
		{"end of input", "", 0},
		{"folded last field", "A: 1\r\n 2\r\n\t3\r\n\r\n", 1},
		{"field at end of input", "A: 1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, src := range budgetSources(tc.input) {
				b := NewHeaderBudget(0, int64(tc.fields))
				if tc.fields == 0 {
					// Exhaust it first, through another block.
					b = NewHeaderBudget(0, 1)
					if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("Z: 0\r\n\r\n")), HeaderLimits{Total: b}); err != nil {
						t.Fatal(err)
					}
				}
				h, err := ReadHeaderLimits(bufio.NewReader(src()), HeaderLimits{Total: b})
				if err != nil {
					t.Fatalf("%s: err = %v", name, err)
				}
				if h.Len() != tc.fields {
					t.Errorf("%s: %d fields, want %d", name, h.Len(), tc.fields)
				}
				if b.Refusal() != nil {
					t.Errorf("%s: budget refused", name)
				}
			}
		})
	}
}

// TestHeaderBudget_precedence pins which budget a read that exceeds several
// reports, and that the refused read charges the shared budget nothing.
func TestHeaderBudget_precedence(t *testing.T) {
	const field = "A: 0123456789\r\n"
	for _, tc := range []struct {
		name   string
		limits HeaderLimits
		want   HeaderLimitKind
	}{
		{"line before total bytes", HeaderLimits{MaxLineBytes: 5}, HeaderLimitLineBytes},
		{"field before total bytes", HeaderLimits{MaxFieldBytes: 5}, HeaderLimitFieldBytes},
		{"block before total bytes", HeaderLimits{MaxBytes: 5}, HeaderLimitBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewHeaderBudget(3, 0)
			tc.limits.Total = b
			_, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(field+"\r\n")), tc.limits)
			if e, ok := HeaderLimit(err); !ok || e.Kind() != tc.want {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if b.Refusal() != nil {
				t.Errorf("the shared budget refused a read a smaller budget refused")
			}
			// Nothing was charged, so a block of exactly the budget still fits.
			h, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("B:\n")), HeaderLimits{Total: b})
			if err != nil || !h.Has("B") {
				t.Errorf("next block = %v, %v", h.Map(), err)
			}
		})
	}

	// A candidate the shared count has no room for is refused before the
	// per-block count, field, line or block budget sees any of it.
	for _, tc := range []struct {
		name   string
		limits HeaderLimits
	}{
		{"per-block count", HeaderLimits{MaxCount: 1}},
		{"field bytes", HeaderLimits{MaxFieldBytes: 8}},
		{"line bytes", HeaderLimits{MaxLineBytes: 8}},
		{"block bytes", HeaderLimits{MaxBytes: 10}},
	} {
		t.Run("preflight before "+tc.name, func(t *testing.T) {
			b := NewHeaderBudget(0, 1)
			tc.limits.Total = b
			_, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\nB: long field here\r\n\r\n")), tc.limits)
			checkRefusal(t, err, HeaderLimitTotalCount, 1, 0, int64(len("A: 1\r\n")))
		})
	}

	// When both totals are spent, the next candidate reports the field count:
	// it is decided before any byte of the candidate is charged.
	b := NewHeaderBudget(int64(len("A: 1\r\n")), 1)
	_, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\nB: 2\r\n\r\n")), HeaderLimits{Total: b})
	checkRefusal(t, err, HeaderLimitTotalCount, 1, 0, int64(len("A: 1\r\n")))
}

// TestHeaderBudget_noRefund pins that what a refused block had admitted stays
// charged, while the read that was refused charges nothing.
func TestHeaderBudget_noRefund(t *testing.T) {
	first := "A: 1\r\n"
	refused := "B: " + strings.Repeat("x", 40) + "\r\n"
	next := "C: 3\r\n\r\n"
	for _, delta := range []int64{-1, 0} {
		b := NewHeaderBudget(int64(len(first)+len(next))+delta, 0)
		_, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(first+refused+"\r\n")),
			HeaderLimits{MaxFieldBytes: 10, Total: b})
		if e, ok := HeaderLimit(err); !ok || e.Kind() != HeaderLimitFieldBytes {
			t.Fatalf("err = %v, want the field budget", err)
		}
		_, err = ReadHeaderLimits(bufio.NewReader(strings.NewReader(next)), HeaderLimits{Total: b})
		if delta == 0 && err != nil {
			t.Errorf("limit%+d: next = %v, want it to fit what the refused block left", delta, err)
		}
		if delta < 0 {
			checkRefusal(t, err, HeaderLimitTotalBytes, int64(len(first)+len(next))+delta, 0, int64(len(next))-1)
		}
	}
}

// TestHeaderBudget_disabledAndSticky pins that a dimension that is off never
// refuses, and that a budget that refused keeps reporting that one refusal —
// of the kind that caused it — for every later charge.
func TestHeaderBudget_disabledAndSticky(t *testing.T) {
	block := strings.Repeat("X: y\r\n", 200) + "\r\n"
	for _, b := range []*HeaderBudget{NewHeaderBudget(0, 0), NewHeaderBudget(-1, -1), &HeaderBudget{}} {
		for i := 0; i < 3; i++ {
			if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(block)), HeaderLimits{Total: b}); err != nil {
				t.Fatalf("disabled budget refused: %v", err)
			}
		}
	}

	// Bytes refuse, the field count is off: the preflight of a later candidate
	// reports the bytes refusal, not a field one.
	b := NewHeaderBudget(4, -1)
	_, first := ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\n\r\n")), HeaderLimits{Total: b})
	checkRefusal(t, first, HeaderLimitTotalBytes, 4, 0, 4)
	for _, later := range []string{"B: 2\r\n\r\n", "\r\n", ":\r\n"} {
		_, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader(later)), HeaderLimits{Total: b})
		if err != first {
			t.Errorf("%q: err = %v, want the identical first refusal", later, err)
		}
	}
	// An input with nothing left charges nothing and is not refused.
	if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("")), HeaderLimits{Total: b}); err != nil {
		t.Errorf("empty input = %v", err)
	}
	if b.Refusal() != first {
		t.Errorf("Refusal() changed")
	}
	var nilBudget *HeaderBudget
	if nilBudget.Refusal() != nil {
		t.Errorf("nil budget has a refusal")
	}
}

// TestHeaderBudget_maxInt64 pins that the largest budget is finite and that
// the arithmetic does not wrap at it.
func TestHeaderBudget_maxInt64(t *testing.T) {
	b := NewHeaderBudget(math.MaxInt64, math.MaxInt64)
	if _, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\n\r\n")), HeaderLimits{Total: b}); err != nil {
		t.Fatal(err)
	}

	b = NewHeaderBudget(math.MaxInt64, math.MaxInt64)
	b.usedBytes, b.usedCount = math.MaxInt64-1, math.MaxInt64-1
	if e := b.chargeBytes(1, 0, 0); e != nil {
		t.Fatal("the last byte was refused")
	}
	if e := b.chargeField(0, 0); e != nil {
		t.Fatal("the last field was refused")
	}
	e := b.chargeBytes(1, 10, 5)
	if e == nil || e.Kind() != HeaderLimitTotalBytes || e.Limit() != math.MaxInt64 || e.Offset() != 10 || e.UnitOffset() != 15 {
		t.Errorf("chargeBytes past MaxInt64 = %v", e)
	}
	if b.usedBytes != math.MaxInt64 || b.usedCount != math.MaxInt64 {
		t.Errorf("usage = %d, %d", b.usedBytes, b.usedCount)
	}
}

// TestHeaderBudget_multipart pins that a multipart reader charges its part
// header blocks and nothing else: not the preamble, the delimiters, the
// bodies it skips, the epilogue, or a part with no header block.
func TestHeaderBudget_multipart(t *testing.T) {
	headers := []string{"Content-Type: text/plain\r\n\r\n", "", "X: 1\r\nY: 2\r\n\r\n", "\r\n"}
	var body strings.Builder
	body.WriteString("preamble line\r\n")
	var total, fields int64
	for i, h := range headers {
		body.WriteString("--B\r\n")
		if h != "" {
			// Two delimiters in a row are a part with no header block.
			body.WriteString(h)
			fmt.Fprintf(&body, "body %d %s\r\n", i, strings.Repeat("z", 100))
		}
		total += int64(len(h))
		fields += int64(strings.Count(h, ":"))
	}
	body.WriteString("--B--\r\nepilogue\r\n")

	read := func(b *HeaderBudget, skip bool) (int, error) {
		r := NewMultipartReader(strings.NewReader(body.String()), "B")
		r.SetHeaderLimits(HeaderLimits{Total: b})
		n := 0
		for {
			p, err := r.NextPart()
			if err == io.EOF {
				return n, nil
			}
			if err != nil {
				return n, err
			}
			if !skip {
				io.Copy(io.Discard, p)
			}
			n++
		}
	}
	for _, skip := range []bool{false, true} {
		for _, delta := range []int64{-1, 0} {
			n, err := read(NewHeaderBudget(total+delta, 0), skip)
			if delta == 0 && (err != nil || n != len(headers)) {
				t.Errorf("bytes exact, skip %v: %d parts, %v", skip, n, err)
			}
			if delta < 0 && (n != len(headers)-1 || err == nil) {
				t.Errorf("bytes-1, skip %v: %d parts, %v", skip, n, err)
			}
			n, err = read(NewHeaderBudget(0, fields+delta), skip)
			wantParts := len(headers)
			if delta < 0 {
				wantParts = 2 // the refused candidate is in the third part
			}
			if n != wantParts || (delta == 0) != (err == nil) {
				t.Errorf("count%+d, skip %v: %d parts, %v", delta, skip, n, err)
			}
		}
	}
}

// TestHeaderBudget_identity pins that only the budget's own refusal is its
// refusal: an input that fails with a forged or foreign refusal is an ordinary
// failure, and the budget's Refusal stays nil.
func TestHeaderBudget_identity(t *testing.T) {
	other := NewHeaderBudget(1, 0)
	_, foreign := ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\n\r\n")), HeaderLimits{Total: other})
	for name, fail := range map[string]error{
		"zero value":       &HeaderLimitError{},
		"typed nil":        (*HeaderLimitError)(nil),
		"foreign refusal":  foreign,
		"wrapped foreign":  fmt.Errorf("wrapped: %w", foreign),
		"sentinel":         ErrTooManyHeaderFields,
		"joined sentinels": errors.Join(ErrHeaderTooBig, foreign),
	} {
		b := NewHeaderBudget(100, 100)
		h, err := ReadHeaderLimits(bufio.NewReader(&errorAfterReader{s: "A: 1\r\n", n: 6, err: fail}), HeaderLimits{Total: b})
		if b.Refusal() != nil {
			t.Errorf("%s: the budget adopted %v", name, b.Refusal())
		}
		// An ordinary read failure keeps what was read, as it always has.
		if err == nil || h.Get("A") != "1" {
			t.Errorf("%s: (%v, %v), want the field read before the failure", name, h.Map(), err)
		}
	}
}

// TestHeaderBudget_concurrentReaders pins that readers sharing one budget never
// admit more than it allows and all report the one refusal.
func TestHeaderBudget_concurrentReaders(t *testing.T) {
	const limit, readers = 500, 8
	b := NewHeaderBudget(0, limit)
	var mu sync.Mutex
	var admitted int
	var errs []error
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Bounded, so that a budget that never refuses fails rather than
			// spins.
			for range 2 * limit {
				_, err := ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\n\r\n")), HeaderLimits{Total: b})
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// The budget only refuses once every field it allows was charged. A block
	// whose field was charged may still be refused at its end by another
	// reader's refusal, which is sticky, so fewer blocks than that may succeed;
	// never more.
	if b.usedCount != limit {
		t.Errorf("charged %d fields, want %d", b.usedCount, limit)
	}
	if admitted > limit || admitted < limit-readers {
		t.Errorf("admitted %d blocks, want at most %d", admitted, limit)
	}
	if len(errs) != readers {
		t.Errorf("%d readers refused, want %d", len(errs), readers)
	}
	for _, err := range errs {
		if err != error(b.Refusal()) {
			t.Errorf("err = %v, want the budget's one refusal", err)
		}
	}
}

// TestHeaderBudget_firstRefusalIsTheCrossingCharge pins, in a fixed order of
// charges from two readers, that the refusal is recorded by the charge that
// did not fit — its kind, budget and position, in that reader's input — and is
// visible from the moment that charge returns, and that every later charge
// from any reader is refused with that identical value, even one that would
// have fitted.
func TestHeaderBudget_firstRefusalIsTheCrossingCharge(t *testing.T) {
	const baseA, baseB = 100, 500

	b := NewHeaderBudget(10, 2)
	if e := b.chargeBytes(6, baseA, 0); e != nil || b.Refusal() != nil {
		t.Fatalf("first charge = %v, refusal %v", e, b.Refusal())
	}
	// Four bytes are left: B's six do not fit, and the first byte past the
	// budget is the fifth of them, three bytes into B's block.
	first := b.chargeBytes(6, baseB, 3)
	if first == nil || b.Refusal() != first {
		t.Fatalf("crossing charge = %v, Refusal() = %v: want it recorded by the charge itself", first, b.Refusal())
	}
	checkRefusal(t, first, HeaderLimitTotalBytes, 10, baseB, baseB+3+4)
	for name, e := range map[string]*HeaderLimitError{
		"A's bytes that would fit": b.chargeBytes(2, baseA, 6),
		"A's field":                b.chargeField(baseA, 0),
		"A's preflight":            b.preflightField(baseA, 8),
	} {
		if e != first {
			t.Errorf("%s = %v, want the identical first refusal", name, e)
		}
	}
	if b.usedBytes != 6 || b.usedCount != 0 {
		t.Errorf("usage = %d bytes, %d fields: a refused budget admits nothing", b.usedBytes, b.usedCount)
	}

	// The same for the field count, reached by a preflight.
	b = NewHeaderBudget(-1, 1)
	if e := b.chargeField(baseA, 0); e != nil {
		t.Fatal(e)
	}
	first = b.preflightField(baseB, 7)
	if first == nil || b.Refusal() != first {
		t.Fatalf("preflight = %v, Refusal() = %v", first, b.Refusal())
	}
	checkRefusal(t, first, HeaderLimitTotalCount, 1, baseB, baseB+7)
	if e := b.chargeBytes(1, baseA, 0); e != first {
		t.Errorf("bytes after a field refusal = %v, want the identical refusal", e)
	}
}

// TestHeaderBudget_concurrentFirstRefusal pins, under real concurrency, that
// the one refusal every reader sees names a charge that actually crossed the
// budget: the reader it names refused there, and the position it reports is
// that charge's position plus what was left of the budget, which no later
// charge changed.
func TestHeaderBudget_concurrentFirstRefusal(t *testing.T) {
	const (
		limit   = 1000
		step    = 7
		readers = 8
		base    = 1 << 20
	)
	b := NewHeaderBudget(limit, -1)
	refusedAt := make([]int64, readers)
	got := make([]*HeaderLimitError, readers)
	var wg sync.WaitGroup
	for g := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Bounded: limit/step charges exhaust the budget on their own.
			for i := range int64(limit/step + 2) {
				if e := b.chargeBytes(step, int64(g)*base, i*step); e != nil {
					refusedAt[g], got[g] = i*step, e
					return
				}
			}
		}()
	}
	wg.Wait()

	first := b.Refusal()
	if first == nil {
		t.Fatal("the budget never refused")
	}
	for g, e := range got {
		if e != first {
			t.Errorf("reader %d = %v, want the identical refusal", g, e)
		}
	}
	g := first.Offset() / base
	if first.Offset() != g*base || g < 0 || g >= readers {
		t.Fatalf("refusal names block %d, which no reader read", first.Offset())
	}
	left := int64(limit) - b.usedBytes
	if left >= step || b.usedBytes > limit {
		t.Errorf("usage %d: the refused charge would have fitted", b.usedBytes)
	}
	if want := first.Offset() + refusedAt[g] + left; first.UnitOffset() != want {
		t.Errorf("UnitOffset() = %d, want %d: reader %d's crossing charge", first.UnitOffset(), want, g)
	}
}

// TestHeaderBudget_unchangedWithoutIt pins that a block reads exactly as it did
// with no shared budget, or with one that never runs out: the same header, the
// same error, and the same bytes left for whatever follows.
func TestHeaderBudget_unchangedWithoutIt(t *testing.T) {
	inputs := []string{
		"A: 1\r\n\r\nbody", "A: 1\r\n", "", "\r\n", "\r", " lead\r\n\r\n", "no colon\r\n\r\n",
		"A: 1\r\n folded\r\n\r\nbody", ":\r\nB: 2\r\n\r\n", "A: 1\r\nbad key\x01: 2\r\n\r\n",
	}
	for _, in := range inputs {
		for _, limits := range []HeaderLimits{{MaxCount: 1}, {}} {
			run := func(l HeaderLimits) (string, string, string) {
				br := bufio.NewReader(&errorAfterReader{s: in, n: len(in), err: io.ErrUnexpectedEOF})
				h, err := ReadHeaderLimits(br, l)
				rest, _ := io.ReadAll(br)
				return fmt.Sprint(h.Map()), fmt.Sprint(err), string(rest)
			}
			wantH, wantErr, wantRest := run(limits)
			for _, total := range []*HeaderBudget{NewHeaderBudget(0, 0), NewHeaderBudget(1<<20, 1<<20)} {
				l := limits
				l.Total = total
				h, err, rest := run(l)
				if h != wantH || err != wantErr || rest != wantRest {
					t.Errorf("%q: (%s, %s, %q), want (%s, %s, %q)", in, h, err, rest, wantH, wantErr, wantRest)
				}
			}
		}
	}
}

// TestHeaderBudget_acceptedChargeAllocatesNothing pins that charging a shared
// budget that fits costs no allocation.
func TestHeaderBudget_acceptedChargeAllocatesNothing(t *testing.T) {
	b := NewHeaderBudget(math.MaxInt64, math.MaxInt64)
	if allocs := testing.AllocsPerRun(100, func() {
		b.chargeBytes(10, 0, 0)
		b.chargeField(0, 0)
		b.preflightField(0, 0)
	}); allocs != 0 {
		t.Errorf("%v allocations per accepted charge", allocs)
	}
	block := strings.Repeat("X: y\r\n", 20) + "\r\n"
	measure := func(l HeaderLimits) float64 {
		return testing.AllocsPerRun(50, func() {
			ReadHeaderLimits(bufio.NewReader(strings.NewReader(block)), l)
		})
	}
	if with, without := measure(HeaderLimits{Total: b}), measure(HeaderLimits{}); with != without {
		t.Errorf("a block costs %v allocations with a shared budget, %v without", with, without)
	}
}

func TestHeaderLimitKind_totalKinds(t *testing.T) {
	if HeaderLimitTotalBytes.String() == HeaderLimitTotalCount.String() ||
		HeaderLimitTotalBytes.String() == HeaderLimitBytes.String() {
		t.Error("total kinds are not distinct")
	}
	if HeaderLimitTotalBytes.sentinel() != ErrHeaderTooBig || HeaderLimitTotalCount.sentinel() != ErrTooManyHeaderFields {
		t.Error("total kinds unwrap to the wrong sentinels")
	}
}
