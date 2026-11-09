package textproto

import (
	"bufio"
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

// Multipart delimiter lines and line endings (ROB-010-002, phase 2a). Every
// source is a finite string or a call-capped failing source; the one probe
// that could spin runs in a subprocess with a deadline.

// mpOutcome is everything one iteration of a multipart yields.
type mpOutcome struct {
	bodies  []string
	readErr []string
	offsets []int64
	final   string
	ends    LineEndings
}

func readMultipart(src io.Reader, limits HeaderLimits) mpOutcome {
	r := NewMultipartReader(src, "B")
	r.SetHeaderLimits(limits)
	var o mpOutcome
	for range 32 {
		p, err := r.NextPart()
		if err != nil {
			o.final = fmt.Sprint(err)
			if err == io.EOF {
				o.final = "io.EOF"
			}
			break
		}
		b, rerr := io.ReadAll(p)
		o.bodies = append(o.bodies, string(b))
		o.readErr = append(o.readErr, fmt.Sprint(rerr))
		o.offsets = append(o.offsets, p.HeaderOffset(), p.BodyOffset())
	}
	o.ends = r.DelimiterEndings()
	return o
}

func (o mpOutcome) String() string {
	return fmt.Sprintf("bodies %q errs %q offsets %v final %q ends %+v", o.bodies, o.readErr, o.offsets, o.final, endsOf(o.ends))
}

// mpSources delivers s every way buffering could matter: whole, with io.EOF on
// the last read, in chunks of 1, 7 and 4096, and, for short inputs, split in
// two at every position.
func mpSources(s string) map[string]func() io.Reader {
	out := map[string]func() io.Reader{
		"whole":         func() io.Reader { return strings.NewReader(s) },
		"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(s)) },
	}
	for _, n := range []int{1, 7, 4096} {
		out[fmt.Sprintf("chunks %d", n)] = func() io.Reader { return &chunkReader{s: s, n: n} }
	}
	if len(s) <= 256 {
		for k := 1; k < len(s); k++ {
			out[fmt.Sprintf("split %d", k)] = func() io.Reader {
				return io.MultiReader(strings.NewReader(s[:k]), strings.NewReader(s[k:]))
			}
		}
	}
	return out
}

// TestMultipartReader_lineEndingGrammar pins the delimiter grammar on every
// line ending, before and after, mixed freely, under every delivery: the
// bodies, their offsets, how the iteration ends, and the endings reported.
func TestMultipartReader_lineEndingGrammar(t *testing.T) {
	var all strings.Builder
	for i := range 256 {
		all.WriteByte(byte(i))
	}
	binary := all.String()
	none := int64(-1)

	for _, tc := range []struct {
		name    string
		in      string
		bodies  []string
		offsets []int64
		final   string
		ends    wantEnds
	}{
		{"cr only", "--B\rA: 1\r\rone\r--B\r\rtwo\r--B--\r",
			[]string{"one", "two"}, []int64{4, 10, 18, 19}, "io.EOF",
			wantEnds{0, 0, 5, none, none, 3, none, none}},
		{"mixed, close at end of input", "pre\n--B\r\n\r\none\n--B\r\r\ntwo\r--B--",
			[]string{"one", "two"}, []int64{9, 11, 19, 21}, "io.EOF",
			wantEnds{1, 2, 2, 7, 3, 18, none, none}},
		{"crlf", "--B\r\n\r\none\r\n--B \t\r\n\r\ntwo\r\n--B--\r\n",
			[]string{"one", "two"}, []int64{5, 7, 19, 21}, "io.EOF",
			wantEnds{5, 0, 0, 3, none, none, none, none}},
		{"cr cr lf leaves the first cr in the body", "--B\r\n\r\nx\r\r\n--B--\r\n",
			[]string{"x\r"}, []int64{5, 7}, "io.EOF",
			wantEnds{3, 0, 0, 3, none, none, none, none}},
		{"false candidates are body", "--B\r\n\r\na\r--Bx\n--B-x\r\n--B junk\r\n--B--x\r--B \tz\r\n--B--\r\n",
			[]string{"a\r--Bx\n--B-x\r\n--B junk\r\n--B--x\r--B \tz"}, []int64{5, 7}, "io.EOF",
			wantEnds{3, 0, 0, 3, none, none, none, none}},
		// A closing delimiter the input ends right after needs no line ending.
		{"close at the end of the input, no line ending", "--B\r\n\r\nnested\r\n--B--",
			[]string{"nested"}, []int64{5, 7}, "io.EOF",
			wantEnds{2, 0, 0, 3, none, none, none, none}},
		{"every byte value, crlf", "--B\r\n\r\n" + binary + "\r\n--B--\r\n",
			[]string{binary}, []int64{5, 7}, "io.EOF",
			wantEnds{3, 0, 0, 3, none, none, none, none}},
		{"every byte value, cr", "--B\r\r" + binary + "\r--B--\r",
			[]string{binary}, []int64{4, 5}, "io.EOF",
			wantEnds{0, 0, 3, none, none, 3, none, none}},
		{"every byte value, lf", "--B\n\n" + binary + "\n--B--\n",
			[]string{binary}, []int64{4, 5}, "io.EOF",
			wantEnds{0, 3, 0, none, 3, none, none, none}},
		{"empty header part", "--B\r\n--B\r\nX: 1\r\n\r\ntwo\r\n--B--\r\n",
			[]string{"", "two"}, []int64{5, 5, 10, 18}, "io.EOF",
			wantEnds{4, 0, 0, 3, none, none, none, none}},
		{"close with a bare cr at the end of input", "--B\r\n\r\none\r\n--B--\r",
			[]string{"one"}, []int64{5, 7}, "io.EOF",
			wantEnds{2, 0, 1, 3, none, 17, none, none}},
		{"delimiter cut by the end of input", "--B\r\n\r\none\r\n--B  ",
			[]string{"one"}, []int64{5, 7}, "multipart: NextPart: EOF",
			wantEnds{2, 0, 0, 3, none, none, none, none}},
	} {
		for name, src := range mpSources(tc.in) {
			label := fmt.Sprintf("%s/%s", tc.name, name)
			got := readMultipart(src(), HeaderLimits{})
			if !equalStrings(got.bodies, tc.bodies) || got.final != tc.final {
				t.Errorf("%s: %v, want bodies %q final %q", label, got, tc.bodies, tc.final)
				continue
			}
			for i, e := range got.readErr {
				if e != "<nil>" {
					t.Errorf("%s: part %d read error %s", label, i, e)
				}
			}
			if fmt.Sprint(got.offsets) != fmt.Sprint(tc.offsets) {
				t.Errorf("%s: offsets %v, want %v", label, got.offsets, tc.offsets)
			}
			if e := endsOf(got.ends); e != tc.ends {
				t.Errorf("%s: endings %+v, want %+v", label, e, tc.ends)
			}
		}
	}
}

// TestMultipartReader_nearDelimiterHeaderIsNoEmptyPart pins that a part's
// first line that only begins like a delimiter — "--B-junk" — is read as the
// header it is, rather than taken for an empty part.
func TestMultipartReader_nearDelimiterHeaderIsNoEmptyPart(t *testing.T) {
	r := NewMultipartReader(strings.NewReader("--B\r\n--B-junk\r\n\r\nbody\r\n--B--\r\n"), "B")
	p, err := r.NextPart()
	if p != nil || err == nil || isEOFClass(err) {
		t.Fatalf("NextPart() = (%v, %v), want the header's own error", p, err)
	}
}

// longDelimiter is a multipart whose second delimiter line has content bytes
// — "--B" and white space — before the line ending eol.
func longDelimiter(content int, eol string) (text string, lineAt int64) {
	head := "--B\r\n\r\none\r\n"
	return head + "--B" + strings.Repeat(" ", content-3) + eol + "\r\ntwo\r\n--B--\r\n", int64(len(head))
}

// TestMultipartReader_longDelimiterBudget pins white space past one buffer
// accepted up to the framing line budget, counted without the line endings
// before and after, and refused one byte past it with this reader's own
// *LineLimitError at the line's "--"; the part before it is whole.
func TestMultipartReader_longDelimiterBudget(t *testing.T) {
	for _, content := range []int{40, 4096, 10000} {
		for _, eol := range []string{"\r\n", "\n", "\r"} {
			text, lineAt := longDelimiter(content, eol)
			for name, src := range mpSources(text) {
				label := fmt.Sprintf("%d/%q/%s", content, eol, name)
				got := readMultipart(src(), HeaderLimits{MaxLineBytes: int64(content)})
				if !equalStrings(got.bodies, []string{"one", "two"}) || got.final != "io.EOF" {
					t.Errorf("%s: at the budget %v", label, got)
				}

				r := NewMultipartReader(src(), "B")
				r.SetHeaderLimits(HeaderLimits{MaxLineBytes: int64(content) - 1})
				p, err := r.NextPart()
				if err != nil {
					t.Fatalf("%s: first part: %v", label, err)
				}
				if b, err := io.ReadAll(p); string(b) != "one" || err != nil {
					t.Errorf("%s: first body (%q, %v), want (one, nil)", label, b, err)
				}
				_, err = r.NextPart()
				le, ok := LineLimit(err)
				if !ok || err != r.refused || le.Offset() != lineAt || le.Limit() != int64(content)-1 {
					t.Errorf("%s: NextPart() = %v, want this reader's own refusal at %d", label, err, lineAt)
				}
				if _, again := r.NextPart(); again != io.EOF {
					t.Errorf("%s: after the refusal %v, want io.EOF", label, again)
				}
			}
		}
	}
}

// TestMultipartReader_longDelimiterNoBudget pins the legacy bound with no line
// budget: a delimiter line must fit in one buffer of the reader, its own line
// ending included and the one before it not, exactly as a framing line read by
// ReadSlice had to; one byte more is this reader's own error wrapping
// bufio.ErrBufferFull, and nothing after it is read as a part.
func TestMultipartReader_longDelimiterNoBudget(t *testing.T) {
	for _, tc := range []struct {
		eol     string
		content int
		ok      bool
	}{
		{"\r\n", peekBufferSize - 2, true},
		{"\r\n", peekBufferSize - 1, false},
		{"\n", peekBufferSize - 1, true},
		{"\n", peekBufferSize, false},
		{"\r", peekBufferSize - 1, true},
		{"\r", peekBufferSize, false},
	} {
		text, _ := longDelimiter(tc.content, tc.eol)
		for name, src := range mpSources(text) {
			label := fmt.Sprintf("%q %d/%s", tc.eol, tc.content, name)
			got := readMultipart(src(), HeaderLimits{})
			if tc.ok {
				if !equalStrings(got.bodies, []string{"one", "two"}) || got.final != "io.EOF" {
					t.Errorf("%s: %v, want both parts", label, got)
				}
				continue
			}
			if !equalStrings(got.bodies, []string{"one"}) || !strings.Contains(got.final, bufio.ErrBufferFull.Error()) {
				t.Errorf("%s: %v, want the first part and a full buffer", label, got)
			}
		}
	}

	// A closing delimiter the input ends needs no line ending: its content may
	// fill the whole buffer.
	for _, tc := range []struct {
		content int
		ok      bool
	}{{peekBufferSize, true}, {peekBufferSize + 1, false}} {
		text := "--B\r\n\r\none\r\n--B--" + strings.Repeat(" ", tc.content-5)
		got := readMultipart(strings.NewReader(text), HeaderLimits{})
		if tc.ok != (got.final == "io.EOF") || !equalStrings(got.bodies, []string{"one"}) {
			t.Errorf("close of %d content bytes at the end of input: %v, accepted want %v", tc.content, got, tc.ok)
		}
	}
}

// TestMultipartReader_heldCandidateInputFailure cuts a multipart at every byte
// with a failure that is not io.EOF — EIO, a wrapped io.EOF and
// io.ErrUnexpectedEOF — delivered with or after the data, from sources that
// resume, under every chunking. A delimiter not yet decided when the input
// failed is body: the first part delivers every byte the input gave up to the
// failure and then the failure itself; a delimiter decided before it stands,
// and the failure belongs to what follows. The source is never read again.
func TestMultipartReader_heldCandidateInputFailure(t *testing.T) {
	const text = "--B\r\n\r\none\r\n--B  \r\nX: 1\r\n\r\ntwo\r\n--B--\r\n"
	const bodyStart = len("--B\r\n\r\n")
	delimStart := strings.Index(text, "\r\n--B  ")
	decided := strings.Index(text, "  \r\n") + len("  \r\n")

	for _, fail := range []error{errEIO, fmt.Errorf("cut: %w", io.EOF), io.ErrUnexpectedEOF} {
		for cut := bodyStart; cut < len(text); cut++ {
			for _, chunk := range []int{1, 7, 4096} {
				for _, withData := range []bool{false, true} {
					label := fmt.Sprintf("%v/cut %d/chunk %d/with %v", fail, cut, chunk, withData)
					src := &failAtSource{data: text, cut: cut, chunk: chunk, err: fail, withData: withData}
					r := NewMultipartReader(src, "B")
					p, err := r.NextPart()
					if err != nil {
						t.Fatalf("%s: first part: %v", label, err)
					}
					body, berr := io.ReadAll(p)
					if cut >= decided {
						if string(body) != text[bodyStart:delimStart] || berr != nil {
							t.Errorf("%s: body (%q, %v), want the whole body", label, body, berr)
						}
					} else {
						if string(body) != text[bodyStart:cut] || !errors.Is(berr, fail) || !errors.Is(berr, ErrMissingBoundaryClose) {
							t.Errorf("%s: body (%q, %v), want %q and the failure", label, body, berr, text[bodyStart:cut])
						}
					}
					for range 4 {
						if _, err := r.NextPart(); err != nil {
							break
						}
					}
					if src.readsAfter != 0 {
						t.Errorf("%s: %d reads after the failure", label, src.readsAfter)
					}
				}
			}
		}
	}
}

// TestMultipartReader_heldCandidateCuts pins each held shape at the cut: a
// delimiter name, a closing name, white space, and a '\r' whose next byte never
// came. Each is body when the input fails, and each is decided by io.EOF.
func TestMultipartReader_heldCandidateCuts(t *testing.T) {
	const head = "--B\r\n\r\none"
	for _, tc := range []struct {
		tail string
		// At io.EOF instead: the bodies, which of them ended with an error,
		// and how the iteration ends.
		eofBodies []string
		eofErrs   []bool
		eofFinal  string
	}{
		// A delimiter name the input ends: the part after it never began.
		{"\r\n--B", []string{"one"}, []bool{false}, "multipart: NextPart: EOF"},
		{"\r\n--B--", []string{"one"}, []bool{false}, "io.EOF"},
		{"\r\n--B   ", []string{"one"}, []bool{false}, "multipart: NextPart: EOF"},
		{"\r\n--B--  ", []string{"one"}, []bool{false}, "io.EOF"},
		// A '\r' io.EOF follows is a line ending: a whole delimiter, then a
		// part with no header and an empty body the input cut short.
		{"\r\n--B\r", []string{"one", ""}, []bool{false, true}, "multipart: NextPart: EOF"},
		// Not delimiters: body, cut short.
		{"\r\n--B-", []string{"one\r\n--B-"}, []bool{true}, "multipart: NextPart: EOF"},
		{"\r\n--", []string{"one\r\n--"}, []bool{true}, "multipart: NextPart: EOF"},
		{"\r", []string{"one\r"}, []bool{true}, "multipart: NextPart: EOF"},
	} {
		text := head + tc.tail
		for _, chunk := range []int{1, 7, 4096} {
			src := &failAtSource{data: text + "rest", cut: len(text), chunk: chunk, err: errEIO}
			r := NewMultipartReader(src, "B")
			p, err := r.NextPart()
			if err != nil {
				t.Fatalf("%q: %v", tc.tail, err)
			}
			body, berr := io.ReadAll(p)
			if string(body) != text[len("--B\r\n\r\n"):] || !errors.Is(berr, errEIO) {
				t.Errorf("%q/chunk %d, failure: body (%q, %v), want every byte and the failure", tc.tail, chunk, body, berr)
			}
			if src.readsAfter != 0 {
				t.Errorf("%q/chunk %d: %d reads after the failure", tc.tail, chunk, src.readsAfter)
			}

			got := readMultipart(&chunkReader{s: text, n: chunk}, HeaderLimits{})
			errsOK := len(got.readErr) == len(tc.eofErrs)
			for i := 0; errsOK && i < len(got.readErr); i++ {
				errsOK = (got.readErr[i] != "<nil>") == tc.eofErrs[i]
			}
			if !equalStrings(got.bodies, tc.eofBodies) || got.final != tc.eofFinal || !errsOK {
				t.Errorf("%q/chunk %d, end of input: %v, want bodies %q errors %v final %q",
					tc.tail, chunk, got, tc.eofBodies, tc.eofErrs, tc.eofFinal)
			}
		}
	}
}

// TestMultipartReader_heldCandidateOffsets pins the parse cursor while a long
// candidate is held and when it turns out to be body: the part after it
// begins exactly where its bytes say, however the input arrives.
func TestMultipartReader_heldCandidateOffsets(t *testing.T) {
	near := "\r\n--B" + strings.Repeat(" ", 9000) + "x"
	text := "--B\r\n\r\n" + near + "\r\n--B\r\nX: 1\r\n\r\ntwo\r\n--B--\r\n"
	head2 := int64(strings.Index(text, "X: 1"))
	for name, src := range mpSources(text) {
		got := readMultipart(src(), HeaderLimits{MaxLineBytes: 1 << 16})
		if !equalStrings(got.bodies, []string{near, "two"}) || got.final != "io.EOF" {
			t.Errorf("%s: %v", name, got)
			continue
		}
		if got.offsets[2] != head2 || got.offsets[3] != head2+int64(len("X: 1\r\n\r\n")) {
			t.Errorf("%s: second part offsets %v, want %d", name, got.offsets[2:], head2)
		}
	}
}

// TestMultipartReader_nearMissesStayFinite reads a body made of many long near
// misses, each held and then released as body, through one-byte reads: every
// byte comes back once and in order.
func TestMultipartReader_nearMissesStayFinite(t *testing.T) {
	var b strings.Builder
	for range 64 {
		b.WriteString("\r\n--B" + strings.Repeat("\t", 300) + "y")
	}
	content := b.String()
	text := "--B\r\n\r\n" + content + "\r\n--B--\r\n"
	got := readMultipart(&chunkReader{s: text, n: 1}, HeaderLimits{MaxLineBytes: 1000})
	if !equalStrings(got.bodies, []string{content}) || got.final != "io.EOF" {
		t.Errorf("%v", got)
	}
}

const multipartBufferFullEnv = "EMLKIT_TEXTPROTO_MULTIPART_BUFFER_FULL"

// TestMultipartReader_sourceBufferFullWhileHeld pins a source that fails with
// bufio.ErrBufferFull itself while a candidate is held: it is the source's
// failure, delivered after the body, never read on. Runs in a subprocess with
// a deadline, since reading on without bytes would not end.
func TestMultipartReader_sourceBufferFullWhileHeld(t *testing.T) {
	if os.Getenv(multipartBufferFullEnv) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMultipartReader_sourceBufferFullWhileHeld$", "-test.count=1")
		cmd.Env = append(os.Environ(), multipartBufferFullEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("subprocess = %v, deadline %v\n%s", err, ctx.Err(), out)
		}
		return
	}
	text := "--B\r\n\r\none\r\n--B" + strings.Repeat(" ", 5000) + "\r\n\r\ntwo\r\n--B--\r\n"
	for _, cut := range []int{12, 13, 15, 16, 100, 4096, 4100, 5010} {
		for _, budget := range []int64{0, 1 << 16} {
			src := &failAtSource{data: text, cut: cut, chunk: 4096, err: bufio.ErrBufferFull, sticky: true}
			r := NewMultipartReader(src, "B")
			r.SetHeaderLimits(HeaderLimits{MaxLineBytes: budget})
			saw := false
			for range 6 {
				p, err := r.NextPart()
				if err != nil {
					saw = saw || errors.Is(err, bufio.ErrBufferFull)
					break
				}
				if _, berr := io.ReadAll(p); errors.Is(berr, bufio.ErrBufferFull) {
					saw = true
				}
			}
			if !saw {
				t.Errorf("cut %d/budget %d: the source's failure was never reported", cut, budget)
			}
		}
	}
}

// TestMultipartReader_candidatePrefixBudget pins that a candidate's valid
// prefix — "--B" and white space, before the byte that breaks it — is charged
// to the framing line budget the same way however the bytes arrive: a prefix
// of exactly the budget followed by an invalid byte is body, one byte more is
// refused, before the invalid byte is reached, whether the whole candidate is
// in the buffer at once or arrives a byte at a time; with no budget the bound
// is one buffer, as for a framing line. The third case has the shape of the
// input the line ending fuzzer found, which is kept as it was in
// testdata/fuzz/FuzzMultipartReader_lineEndings/8c17b33a75df9e9d.
func TestMultipartReader_candidatePrefixBudget(t *testing.T) {
	for _, tc := range []struct {
		pad     int
		tail    string
		limit   int64
		refused bool
	}{
		{61, "\x7f", 64, false},
		{62, "\x7f", 64, true},
		{215, "\x7f\xff", 64, true},
		{peekBufferSize - 3, "x", 0, false},
		{peekBufferSize - 2, "x", 0, true},
	} {
		near := "\r\n--B" + strings.Repeat(" ", tc.pad) + tc.tail
		text := "--B\r\n\r\none" + near + "\r\n\r\ntwo\r\n--B--\r\n"
		for name, src := range mpSources(text) {
			label := fmt.Sprintf("pad %d limit %d/%s", tc.pad, tc.limit, name)
			got := readMultipart(src(), HeaderLimits{MaxLineBytes: tc.limit})
			if !tc.refused {
				want := "one" + near + "\r\n\r\ntwo"
				if !equalStrings(got.bodies, []string{want}) || got.final != "io.EOF" {
					t.Errorf("%s: %v, want the near miss as body", label, got)
				}
				continue
			}
			// The refused line is the candidate's, from its "--" at offset 12.
			wantErr := "multipart: line at offset 12 exceeds the limit of 64 bytes"
			if tc.limit == 0 {
				wantErr = "multipart: NextPart: " + bufio.ErrBufferFull.Error()
			}
			if !equalStrings(got.bodies, []string{"one"}) || got.final != wantErr {
				t.Errorf("%s: %v, want the first body and a refusal of the candidate", label, got)
			}
		}
	}

	// The input failing right after the prefix: a prefix past the budget is
	// refused first; one within it is body, followed by the failure.
	for _, tc := range []struct {
		pad     int
		refused bool
	}{{61, false}, {62, true}} {
		prefix := "--B\r\n\r\none\r\n--B" + strings.Repeat(" ", tc.pad)
		for _, chunk := range []int{1, 7, 4096} {
			src := &failAtSource{data: prefix + "x", cut: len(prefix), chunk: chunk, err: errEIO, sticky: true}
			r := NewMultipartReader(src, "B")
			r.SetHeaderLimits(HeaderLimits{MaxLineBytes: 64})
			p, err := r.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			body, berr := io.ReadAll(p)
			_, nerr := r.NextPart()
			if tc.refused {
				if le, ok := LineLimit(nerr); string(body) != "one" || berr != nil || !ok || le.Offset() != 12 {
					t.Errorf("pad %d/chunk %d: body (%q, %v), NextPart %v; want one, then the refusal", tc.pad, chunk, body, berr, nerr)
				}
				continue
			}
			if string(body) != prefix[len("--B\r\n\r\n"):] || !errors.Is(berr, errEIO) {
				t.Errorf("pad %d/chunk %d: body (%q, %v), want the prefix as body and the failure", tc.pad, chunk, body, berr)
			}
		}
	}
}

// fuzzMultipart iterates a multipart of boundary B read from src, at most 64
// parts with at most 16 KiB of each body, and renders everything a caller can
// observe: each part's header fields with their raw bytes, its offsets, its
// body and how that body ended, how the iteration ended, and the delimiter
// endings reported.
func fuzzMultipart(src io.Reader, limits HeaderLimits) string {
	r := NewMultipartReader(src, "B")
	r.SetHeaderLimits(limits)
	var b strings.Builder
	for range 64 {
		p, err := r.NextPart()
		if err != nil {
			if err == io.EOF {
				b.WriteString("end io.EOF\n")
			} else {
				fmt.Fprintf(&b, "end %v\n", err)
			}
			break
		}
		fmt.Fprintf(&b, "bom %v\n", p.Header.ByteOrderMark())
		fs := p.Header.Fields()
		for fs.Next() {
			raw, _ := fs.Raw()
			fmt.Fprintf(&b, "field %q %q %q\n", fs.Key(), fs.Value(), raw)
		}
		body, berr := io.ReadAll(io.LimitReader(p, 16<<10))
		fmt.Fprintf(&b, "part %d %d %q %v\n", p.HeaderOffset(), p.BodyOffset(), body, berr)
	}
	fmt.Fprintf(&b, "ends %+v\n", endsOf(r.DelimiterEndings()))
	return b.String()
}

// FuzzMultipartReader_lineEndings holds an arbitrary multipart body of at most
// 8 KiB, read under no line budget and under a small one, to one outcome
// whatever its delivery: whole, one byte or seven at a time, and with io.EOF
// together with the last bytes. It compares deliveries with each other, not
// with a second parser; every source is a finite string.
func FuzzMultipartReader_lineEndings(f *testing.F) {
	for _, s := range []string{
		"--B\rA: 1\r\rone\r--B\r\rtwo\r--B--\r",
		"pre\n--B\r\n\r\none\n--B\r\r\ntwo\r--B--",
		"--B\r\n\r\na\r--Bx\n--B-x\r\n--B junk\r\n--B--x\r--B \tz\r\n--B--\r\n",
		"--B\r\n--B\r\nX: 1\r\n\r\ntwo\r\n--B--\r\n",
		"--B\r\n\r\none\r\n--B" + strings.Repeat(" ", 300) + "\r\n\r\ntwo\r\n--B--\r\n",
		"--B\r\n\r\nx\r\r\n--B--\r\n",
		"--B\r\n\r\none\r\n--B  ",
		"--B\r\n--B-junk\r\n\r\nbody\r\n--B--\r\n",
		// Byte order marks at part header starts (ROB-010-003).
		"--B\r\n\xef\xbb\xbfA: 1\r\n\r\none\r\n--B\r\n\xff\xfex\x00\r\x00\n\x00\r\n--B--\r\n",
		"--B\n\xfe\xff\x00x\r--B\r\x00\x00\xfe\xff\n--B\r\n\xef\xbb\r\n\r\n--B--",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			return
		}
		s := string(data)
		for _, limits := range []HeaderLimits{{}, {MaxLineBytes: 64}} {
			want := fuzzMultipart(strings.NewReader(s), limits)
			for name, src := range map[string]io.Reader{
				"chunks 1":      &chunkReader{s: s, n: 1},
				"chunks 7":      &chunkReader{s: s, n: 7},
				"data with eof": iotest.DataErrReader(strings.NewReader(s)),
			} {
				if got := fuzzMultipart(src, limits); got != want {
					t.Errorf("%s, limits %+v:\n got %s\nwant %s", name, limits, got, want)
				}
			}
		}
	})
}
