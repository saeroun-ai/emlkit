package message

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
)

// Limits.MaxLineBytes of LimitDisabled is no line limit (#64). A message with a
// framing line far past the reader's 4 KiB buffer — a preamble line, a padded
// delimiter line, a delimiter-like line, a long body line — is read under it
// into the same tree the default 64 KiB budget reads, however it is chunked and
// with every line ending; a framing line before the first part costs no memory
// for its length; a padded delimiter candidate inside a body is held to its
// length, which MaxInputBytes bounds; and failures and cancellation inside such
// a line are reported as the default budget reports them.

// disabledLine is the default budgets with the line budget off, and
// defaultLine the default budgets themselves: they differ in nothing else.
func disabledLine() Limits {
	l := DefaultLimits()
	l.MaxLineBytes = LimitDisabled
	return l
}

func defaultLine() Limits { return DefaultLimits() }

// lineTree is everything a traversal observes of a parse: every entity's path,
// header and body as read, what stopped the traversal, and the diagnostics.
type lineTree struct {
	entities []string
	err      string
	diags    []string
}

func snapshotLineTree(s *ParseSession, e *Entity, parseErr error) lineTree {
	var out lineTree
	if parseErr != nil {
		out.err = "parse: " + parseErr.Error()
	}
	if e != nil {
		var visit func(path string, e *Entity) error
		visit = func(path string, e *Entity) error {
			if mr := e.MultipartReader(); mr != nil {
				out.entities = append(out.entities, fmt.Sprintf("%s multipart %q", path, e.Header.Get("Content-Type")))
				for i := 0; ; i++ {
					p, err := mr.NextPart()
					if err == io.EOF {
						return nil
					}
					if err != nil {
						return err
					}
					if err := visit(fmt.Sprintf("%s/%d", path, i), p); err != nil {
						return err
					}
				}
			}
			b, err := io.ReadAll(e.Body)
			out.entities = append(out.entities, fmt.Sprintf("%s %q body=%q", path, e.Header.Get("Content-Type"), b))
			return err
		}
		if err := visit("", e); err != nil {
			out.err += "read: " + err.Error()
		}
	}
	for _, d := range s.Diagnostics() {
		out.diags = append(out.diags, fmt.Sprintf("%s %v %v", d.Code, d.Path, d.Location))
	}
	return out
}

func (a lineTree) diff(b lineTree) string {
	if a.err != b.err {
		return fmt.Sprintf("err %q vs %q", a.err, b.err)
	}
	if fmt.Sprint(a.diags) != fmt.Sprint(b.diags) {
		return fmt.Sprintf("diagnostics %v vs %v", a.diags, b.diags)
	}
	if len(a.entities) != len(b.entities) {
		return fmt.Sprintf("%d entities vs %d", len(a.entities), len(b.entities))
	}
	for i := range a.entities {
		if a.entities[i] != b.entities[i] {
			return fmt.Sprintf("entity %d differs", i)
		}
	}
	return ""
}

// longFramingMessages returns messages whose framing lines run past a buffer,
// with eol as every line ending, and how many leaves each holds.
func longFramingMessages(eol string) []struct {
	name   string
	text   string
	leaves int
} {
	pad := strings.Repeat(" \t", 4500) // 9000 bytes of transport padding
	long := strings.Repeat("p", 9000)
	head := "Content-Type: multipart/mixed; boundary=B" + eol + eol
	part := func(body string) string { return "Content-Type: text/plain" + eol + eol + body + eol }
	inner := "Content-Type: multipart/alternative; boundary=I" + eol + eol +
		long + eol + "--I" + pad + eol + part("deep") + "--I--" + pad + eol + long + eol
	return []struct {
		name   string
		text   string
		leaves int
	}{
		{"preamble line", head + long + eol + "--B" + eol + part("zero") + "--B--" + eol, 1},
		{"delimiter-like preamble line", head + "--B" + pad + "x" + eol + "--B" + eol + part("zero") + "--B--" + eol, 1},
		{"padded first delimiter", head + "--B" + pad + eol + part("zero") + "--B--" + eol, 1},
		{"padded delimiter in body", head + "--B" + eol + part("zero") + "--B" + pad + eol + part("one") + "--B--" + eol, 2},
		{"padded closing delimiter", head + "--B" + eol + part("zero") + "--B--" + pad + eol + long + eol, 1},
		{"delimiter-like body line", head + "--B" + eol + part("zero"+eol+"--B"+pad+"x") + "--B--" + eol, 1},
		{"long body line", head + "--B" + eol + part(long+eol+long) + "--B--" + eol, 1},
		{"nested long framing", head + "--B" + eol + inner + "--B" + eol + part("one") + "--B--" + eol, 2},
		{"padded delimiter after malformed prefix", head + "--B" + eol + " bad" + eol + "--B" + pad + eol + part("one") + "--B--" + eol, 2},
	}
}

var disabledChunkings = []int{0, 1, 16, 4096}

func chunkedText(text string, n int) io.Reader {
	if n == 0 {
		return strings.NewReader(text)
	}
	return &chunkReader{s: text, n: n}
}

// TestParse_lineBytesDisabled_sameTreeAsDefault pins the first criterion of
// #64: every message is read under a disabled line budget into exactly the
// tree, errors and diagnostics of the default budget, whole and in chunks of
// 1, 16 and 4096 bytes, with CRLF, LF and bare CR line endings — and, for the
// CRLF and LF forms, a tree with every leaf in it and no failure.
func TestParse_lineBytesDisabled_sameTreeAsDefault(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n", "\r"} {
		for _, tc := range longFramingMessages(eol) {
			for _, n := range disabledChunkings {
				label := fmt.Sprintf("%q/%s/chunks %d", eol, tc.name, n)
				ws, we, werr := Parse(chunkedText(tc.text, n), &ParseOptions{Limits: defaultLine()})
				want := snapshotLineTree(ws, we, werr)
				gs, ge, gerr := Parse(chunkedText(tc.text, n), &ParseOptions{Limits: disabledLine()})
				got := snapshotLineTree(gs, ge, gerr)
				if d := got.diff(want); d != "" {
					t.Errorf("%s: %s", label, d)
				}
				if eol == "\r" {
					continue
				}
				if got.err != "" {
					t.Errorf("%s: %s", label, got.err)
				}
				leaves := 0
				for _, s := range got.entities {
					if !strings.Contains(s, " multipart ") {
						leaves++
					}
				}
				if leaves != tc.leaves {
					t.Errorf("%s: %d leaves, want %d", label, leaves, tc.leaves)
				}
			}
		}
	}
}

// lineGenReader delivers head, n copies of fill and tail, generated as read.
type lineGenReader struct {
	head, tail string
	fill       byte
	n, pos     int64
}

func (g *lineGenReader) Read(p []byte) (int, error) {
	h, total := int64(len(g.head)), int64(len(g.head))+g.n+int64(len(g.tail))
	if g.pos >= total {
		return 0, io.EOF
	}
	k := 0
	for k < len(p) && g.pos < total {
		switch {
		case g.pos < h:
			c := copy(p[k:], g.head[g.pos:])
			k, g.pos = k+c, g.pos+int64(c)
		case g.pos < h+g.n:
			c := int(min(int64(len(p)-k), h+g.n-g.pos))
			for i := range c {
				p[k+i] = g.fill
			}
			k, g.pos = k+c, g.pos+int64(c)
		default:
			c := copy(p[k:], g.tail[g.pos-h-g.n:])
			k, g.pos = k+c, g.pos+int64(c)
		}
	}
	return k, nil
}

// allocatedBy reports how many bytes run allocated.
func allocatedBy(run func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// drainTree reads every leaf body to io.Discard and returns what stopped it.
func drainTree(e *Entity) (leaves int, err error) {
	var visit func(e *Entity) error
	visit = func(e *Entity) error {
		if mr := e.MultipartReader(); mr != nil {
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				if err := visit(p); err != nil {
					return err
				}
			}
		}
		leaves++
		_, err := io.Copy(io.Discard, e.Body)
		return err
	}
	return leaves, visit(e)
}

// TestParse_lineBytesDisabled_memoryDoesNotScaleWithLine pins the second
// criterion of #64 with a finite generated input: a framing line before the
// first part, and a body line, cost the same allocation at 256 KiB as at 4 MiB.
// The preamble a tolerant parse keeps is bounded by MaxPreambleBytes, as it is
// under any line budget.
func TestParse_lineBytesDisabled_memoryDoesNotScaleWithLine(t *testing.T) {
	const small, large = 256 << 10, 4 << 20
	head := "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	for _, tc := range []struct {
		name       string
		start, end string
		fill       byte
	}{
		{"preamble line", head, "\r\n--B\r\n\r\nzero\r\n--B--\r\n", 'p'},
		{"padded first delimiter", head + "--B", "\r\n\r\nzero\r\n--B--\r\n", ' '},
		{"delimiter-like preamble line", head + "--B", "x\r\n--B\r\n\r\nzero\r\n--B--\r\n", '\t'},
		{"body line", head + "--B\r\n\r\n", "\r\n--B--\r\n", 'z'},
		{"delimiter-like body line", head + "--B\r\n\r\nzero\r\n--Bx", "\r\n--B--\r\n", 'x'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost := func(n int64) uint64 {
				return allocatedBy(func() {
					l := disabledLine()
					l.MaxInputBytes = LimitDisabled
					l.MaxEncodedBodyBytes, l.MaxDecodedBodyBytes, l.MaxTotalDecodedBytes = LimitDisabled, LimitDisabled, LimitDisabled
					l.MaxPreambleBytes = 64 << 10
					_, e, err := Parse(&lineGenReader{head: tc.start, tail: tc.end, fill: tc.fill, n: n}, &ParseOptions{Limits: l})
					if err != nil {
						t.Fatalf("Parse() = %v", err)
					}
					if leaves, err := drainTree(e); err != nil || leaves != 1 {
						t.Fatalf("reading the tree = %d leaves, %v; want 1 and no failure", leaves, err)
					}
				})
			}
			a, b := cost(small), cost(large)
			// 3.75 MiB more input; a line held whole would cost at least that.
			if b > a+(1<<20) {
				t.Errorf("allocated %d bytes for a %d-byte line and %d for a %d-byte one: it grows with the line", a, small, b, large)
			}
		})
	}
}

// TestParse_lineBytesDisabled_heldCandidateBoundedByInput pins what is left
// that does grow, and what bounds it: a delimiter candidate inside a body whose
// padding is not yet decided is held, so its memory follows its length; with
// MaxInputBytes it cannot be longer than the input admitted, and the parse
// stops at that budget. Unbudgeted, a 1 MiB padded delimiter is still proved.
func TestParse_lineBytesDisabled_heldCandidateBoundedByInput(t *testing.T) {
	head := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nzero\r\n--B"
	tail := "\r\n\r\none\r\n--B--\r\n"

	l := disabledLine()
	l.MaxInputBytes = LimitDisabled
	_, e, err := Parse(&lineGenReader{head: head, tail: tail, fill: ' ', n: 1 << 20}, &ParseOptions{Limits: l})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if leaves, err := drainTree(e); err != nil || leaves != 2 {
		t.Fatalf("reading the tree = %d leaves, %v; want the padded delimiter proved", leaves, err)
	}

	var stopped error
	alloc := allocatedBy(func() {
		l := disabledLine()
		l.MaxInputBytes = 256 << 10
		_, e, err := Parse(&lineGenReader{head: head, tail: tail, fill: ' ', n: 8 << 20}, &ParseOptions{Limits: l})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		_, stopped = drainTree(e)
	})
	if !IsFatal(stopped) {
		t.Errorf("reading the tree = %v, want the input budget to stop it", stopped)
	}
	if alloc > 4<<20 {
		t.Errorf("allocated %d bytes for a candidate the 256 KiB input budget cuts", alloc)
	}
}

// TestParse_lineBytesDisabled_failuresAsDefault pins that a failure of the
// input, and a cancellation, inside a long framing line are reported under a
// disabled line budget exactly as under the default one: the same tree, the
// same error, no refusal, and nothing read after a cancellation.
func TestParse_lineBytesDisabled_failuresAsDefault(t *testing.T) {
	cause := errors.New("input broke")
	for _, tc := range longFramingMessages("\r\n") {
		for _, cut := range []int{60, 3000, 4200, 9000, len(tc.text) / 2, len(tc.text) - 3} {
			run := func(l Limits) lineTree {
				s, e, err := Parse(&errorAfterReader{s: tc.text, n: cut, err: cause}, &ParseOptions{Limits: l})
				return snapshotLineTree(s, e, err)
			}
			want, got := run(defaultLine()), run(disabledLine())
			if d := got.diff(want); d != "" {
				t.Errorf("%s/cut %d: %s", tc.name, cut, d)
			}
			if strings.Contains(got.err, bufio.ErrBufferFull.Error()) {
				t.Errorf("%s/cut %d: %s, want no buffer refusal", tc.name, cut, got.err)
			}

			cancelRun := func(l Limits) (lineTree, int) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				src := &cutSource{s: tc.text, chunk: 1000, at: cut, cancel: cancel}
				s, e, err := ParseContext(ctx, src, &ParseOptions{Limits: l})
				return snapshotLineTree(s, e, err), src.after
			}
			wantC, _ := cancelRun(defaultLine())
			gotC, after := cancelRun(disabledLine())
			if d := gotC.diff(wantC); d != "" {
				t.Errorf("%s/cancel at %d: %s", tc.name, cut, d)
			}
			if after != 0 {
				t.Errorf("%s/cancel at %d: read the input %d times after the cancellation", tc.name, cut, after)
			}
		}
	}
}
