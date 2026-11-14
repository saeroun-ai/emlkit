package message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// Overlong lines in a body declared 7bit (ROB-070-003, #33).

const sevenBitHead = "Content-Transfer-Encoding: 7bit\r\n\r\n"

// sevenBitDeliveries are ciDeliveries and a split after the 999th byte of the
// body, which puts a CRLF after 998 octets across two reads.
func sevenBitDeliveries(msg string) map[string]io.Reader {
	out := make(map[string]io.Reader)
	for _, d := range ciDeliveries {
		out[d.name] = d.wrap([]byte(msg))
	}
	if cut := len(sevenBitHead) + 999; strings.HasPrefix(msg, sevenBitHead) && len(msg) > cut {
		out["split at 999"] = io.MultiReader(strings.NewReader(msg[:cut]), strings.NewReader(msg[cut:]))
	}
	return out
}

// sevenBitParse parses src, reads the root body to its end and returns it
// with the diagnostics recorded.
func sevenBitParse(t *testing.T, src io.Reader, opts *ParseOptions) (string, []Diagnostic, error) {
	t.Helper()
	s, e, err := Parse(src, opts)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	b, rerr := io.ReadAll(e.Body)
	if rerr != nil {
		err = rerr
	}
	return string(b), s.Diagnostics(), err
}

func diagCodesString(ds []Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "%s@%d ", d.Code, d.Location.Offset)
	}
	return b.String()
}

func TestParse_sevenBitOverlongLine(t *testing.T) {
	x := func(n int) string { return strings.Repeat("x", n) }
	type want struct {
		code Code
		at   int // offset in the body
	}
	long := func(at int) []want { return []want{{CodeCTESevenBitOverlongLine, at}} }
	for _, tc := range []struct {
		name, body string
		want       []want
	}{
		{"998 CRLF", x(998) + "\r\nend\r\n", nil},
		{"999 CRLF", x(999) + "\r\nend\r\n", long(0)},
		{"998 LF", x(998) + "\nend\n", nil},
		{"999 LF", x(999) + "\nend\n", long(0)},
		{"998 bare CR", x(998) + "\rend\r\n", long(0)},
		{"998 then CR CRLF", x(998) + "\r\r\n", long(0)},
		{"997 bare CR", x(997) + "\r\r\n", nil},
		{"998 no ending", x(998), nil},
		{"999 no ending", x(999), long(0)},
		{"998 and a final CR", x(998) + "\r", nil},
		{"multiple lines", "short\r\n" + x(1000) + "\r\n" + x(2000) + "\r\n" + x(5000), long(7)},
		{"after a 998 line", x(998) + "\r\n" + x(999) + "\r\n", long(1000)},
		{"8-bit byte before", "caf\xe9\r\n" + x(1000) + "\r\n",
			[]want{{CodeCTESevenBitViolation, 3}, {CodeCTESevenBitOverlongLine, 6}}},
		{"8-bit byte in the line, before its 999th octet", x(10) + "\xe9" + x(1000) + "\r\n",
			[]want{{CodeCTESevenBitViolation, 10}, {CodeCTESevenBitOverlongLine, 0}}},
		{"8-bit byte in the line, after its 999th octet", x(1000) + "\xe9\r\n",
			[]want{{CodeCTESevenBitOverlongLine, 0}, {CodeCTESevenBitViolation, 1000}}},
		{"8-bit byte after", x(1000) + "\r\ncaf\xe9\r\n",
			[]want{{CodeCTESevenBitOverlongLine, 0}, {CodeCTESevenBitViolation, 1005}}},
	} {
		msg := sevenBitHead + tc.body
		var wantS string
		for _, w := range tc.want {
			wantS += fmt.Sprintf("%s@%d ", w.code, len(sevenBitHead)+w.at)
		}
		for name, src := range sevenBitDeliveries(msg) {
			got, diags, err := sevenBitParse(t, src, nil)
			if err != nil {
				t.Errorf("%s, read %s: %v", tc.name, name, err)
			}
			if got != tc.body {
				t.Errorf("%s, read %s: the body changed (%d bytes, want %d)", tc.name, name, len(got), len(tc.body))
			}
			if g := diagCodesString(diags); g != wantS {
				t.Errorf("%s, read %s: diagnostics %q, want %q", tc.name, name, g, wantS)
			}
		}
	}
}

// TestParse_sevenBitOverlongLineScope: only a body declared 7bit under a
// healing Parse is looked at; the bytes are the same everywhere.
func TestParse_sevenBitOverlongLineScope(t *testing.T) {
	body := strings.Repeat("y", 2000) + "\r\n"
	for _, cte := range []string{"8bit", "binary", ""} {
		msg := "Content-Type: text/plain\r\n\r\n" + body
		if cte != "" {
			msg = "Content-Transfer-Encoding: " + cte + "\r\n" + msg
		}
		got, diags, err := sevenBitParse(t, strings.NewReader(msg), nil)
		if got != body || err != nil || len(diags) != 0 {
			t.Errorf("%q: (%d bytes, %v, %s), want the body and nothing recorded", cte, len(got), err, diagCodesString(diags))
		}
	}
	msg := sevenBitHead + body
	got, diags, err := sevenBitParse(t, strings.NewReader(msg), &ParseOptions{Profile: ProfileStrict})
	if got != body || err != nil || len(diags) != 0 {
		t.Errorf("strict: (%d bytes, %v, %s), want the body and nothing recorded", len(got), err, diagCodesString(diags))
	}
	for _, p := range []Profile{ProfileTolerant, ProfileForensic} {
		_, diags, _ := sevenBitParse(t, strings.NewReader(msg), &ParseOptions{Profile: p})
		if g := diagCodesString(diags); g != fmt.Sprintf("%s@%d ", CodeCTESevenBitOverlongLine, len(sevenBitHead)) {
			t.Errorf("%v: diagnostics %q", p, g)
		}
	}
	// Read and New have no session: the same bytes, nothing to report to.
	e, err := Read(strings.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(e.Body); string(b) != body || err != nil {
		t.Errorf("Read: (%d bytes, %v), want the body", len(b), err)
	}
}

// TestParse_sevenBitOverlongLinePart: a part's body ends before the line
// ending of the delimiter after it, which is not counted.
func TestParse_sevenBitOverlongLinePart(t *testing.T) {
	part := func(n int, nl string) string {
		return "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
			"--B" + nl + "Content-Transfer-Encoding: 7bit" + nl + nl +
			strings.Repeat("z", n) + nl + "--B--" + nl
	}
	for _, tc := range []struct {
		n    int
		nl   string
		want int
	}{{998, "\r\n", 0}, {999, "\r\n", 1}, {998, "\n", 0}, {999, "\n", 1}} {
		msg := part(tc.n, tc.nl)
		for _, d := range ciDeliveries {
			s, e, err := Parse(d.wrap([]byte(msg)), nil)
			if err != nil {
				t.Fatal(err)
			}
			p, err := e.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(p.Body)
			if len(b) != tc.n || err != nil {
				t.Errorf("%d %q read %s: (%d bytes, %v)", tc.n, tc.nl, d.name, len(b), err)
			}
			var got int
			for _, diag := range s.Diagnostics() {
				if diag.Code == CodeCTESevenBitOverlongLine {
					got++
					if len(diag.Path) != 1 || diag.Path[0] != 0 {
						t.Errorf("%d %q: path %v, want [0]", tc.n, tc.nl, diag.Path)
					}
				}
			}
			if got != tc.want {
				t.Errorf("%d %q read %s: %d overlong lines recorded, want %d (%s)", tc.n, tc.nl, d.name, got, tc.want, diagCodesString(s.Diagnostics()))
			}
		}
	}
}

// TestParse_sevenBitOverlongLineOrder: the overlong line is recorded when the
// consumer reaches the byte that proved it, after what a charset monitor above
// finds in the bytes before it, however the input arrives.
func TestParse_sevenBitOverlongLineOrder(t *testing.T) {
	const head = "Content-Type: text/plain; charset=utf-8\r\n" + sevenBitHead
	body := strings.Repeat("x", 10) + "\xe9" + strings.Repeat("x", 1000) + "\r\nend\r\n"
	want := fmt.Sprint([]Code{CodeCTESevenBitViolation, CodeCharsetInvalidSequence, CodeCTESevenBitOverlongLine})
	for name, src := range sevenBitDeliveries(head + body) {
		got, diags, _ := sevenBitParse(t, src, nil)
		if got != body {
			t.Errorf("read %s: the body changed", name)
		}
		var codes []Code
		for _, d := range diags {
			codes = append(codes, d.Code)
		}
		if g := fmt.Sprint(codes); g != want {
			t.Errorf("read %s: %s, want %s", name, g, want)
		}
	}
}

// TestParse_sevenBitOverlongLineInputFailure: a line already proved overlong
// is recorded even when the input then fails; the failure is still the
// body's, and the bytes before it are delivered.
func TestParse_sevenBitOverlongLineInputFailure(t *testing.T) {
	boom := errors.New("boom")
	body := strings.Repeat("x", 1500)
	src := io.MultiReader(strings.NewReader(sevenBitHead+body), iotest.ErrReader(boom))
	s, e, err := Parse(src, nil)
	if e == nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(e.Body)
	if string(b) != body || !errors.Is(err, boom) {
		t.Errorf("(%d bytes, %v), want the body and the input's failure", len(b), err)
	}
	if !strings.Contains(diagCodesString(s.Diagnostics()), string(CodeCTESevenBitOverlongLine)) {
		t.Errorf("diagnostics %s, want the overlong line", diagCodesString(s.Diagnostics()))
	}
	// An input failure before the 999th octet: no line was proved overlong.
	src = io.MultiReader(strings.NewReader(sevenBitHead+body[:998]), iotest.ErrReader(boom))
	s, e, _ = Parse(src, nil)
	io.ReadAll(e.Body)
	if strings.Contains(diagCodesString(s.Diagnostics()), string(CodeCTESevenBitOverlongLine)) {
		t.Errorf("diagnostics %s, want no overlong line", diagCodesString(s.Diagnostics()))
	}
}

// TestParse_sevenBitOverlongLineSinkStop: a sink that stops the parse when it
// is told stops the session, not the body already returned: it is recorded
// once and every byte is still delivered.
func TestParse_sevenBitOverlongLineSinkStop(t *testing.T) {
	stop := errors.New("stop")
	var told int
	opts := &ParseOptions{DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
		if d.Code == CodeCTESevenBitOverlongLine {
			told++
			return stop
		}
		return nil
	})}
	body := strings.Repeat("x", 3000) + "\r\n" + strings.Repeat("y", 3000)
	for _, d := range ciDeliveries {
		told = 0
		s, e, err := Parse(d.wrap([]byte(sevenBitHead+body)), opts)
		if e == nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(e.Body)
		if string(b) != body || told != 1 {
			t.Errorf("read %s: (%d bytes, told %d times), want every byte and one", d.name, len(b), told)
		}
		if c := s.Completion(); !errors.Is(c.Stop, stop) {
			t.Errorf("read %s: stop %v, want the sink's", d.name, c.Stop)
		}
	}
}

// TestParse_sevenBitOverlongLineCancel: bytes the monitor holds past the
// overlong line are not delivered once the parse is cancelled, and nothing is
// recorded after it.
func TestParse_sevenBitOverlongLineCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := strings.Repeat("x", 3000)
	s, e, err := ParseContext(ctx, strings.NewReader(sevenBitHead+body), nil)
	if e == nil {
		t.Fatal(err)
	}
	// Read up to the byte that proves the line overlong; the monitor stops
	// every read there.
	p := make([]byte, 4096)
	total := 0
	for total < 998 {
		n, err := e.Body.Read(p)
		total += n
		if err != nil {
			t.Fatalf("read at %d: %v", total, err)
		}
	}
	if total != 998 || strings.Contains(diagCodesString(s.Diagnostics()), string(CodeCTESevenBitOverlongLine)) {
		t.Fatalf("read %d bytes, diagnostics %s; want the 998 before the proving byte and nothing yet", total, diagCodesString(s.Diagnostics()))
	}
	cancel()
	n, err := e.Body.Read(p)
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Errorf("read after cancel (%d, %v), want the cancellation", n, err)
	}
	if strings.Contains(diagCodesString(s.Diagnostics()), string(CodeCTESevenBitOverlongLine)) {
		t.Errorf("diagnostics %s, want no overlong line after the cancellation", diagCodesString(s.Diagnostics()))
	}
}

// TestSevenBitMonitor_bounded: the monitor reads each byte of its source once
// and holds at most what one read returned past the proving byte, once.
func TestSevenBitMonitor_bounded(t *testing.T) {
	line := bytes.Repeat([]byte("x"), 1<<20)
	for _, size := range []int{1, 7, 999, 4096, 1 << 16} {
		src := &countReader{r: bytes.NewReader(line)}
		m := &sevenBitMonitor{r: src, notes: &cteNotes{}}
		var out bytes.Buffer
		p := make([]byte, size)
		maxHeld, holds := 0, 0
		for reads := 0; ; reads++ {
			if reads > 2*len(line)+8 {
				t.Fatalf("size %d: no end after %d reads", size, reads)
			}
			before := len(m.held)
			n, err := m.Read(p)
			out.Write(p[:n])
			if before == 0 && len(m.held) > 0 {
				holds++
			}
			maxHeld = max(maxHeld, len(m.held))
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(out.Bytes(), line) {
			t.Errorf("size %d: the bytes changed", size)
		}
		if src.n != int64(len(line)) {
			t.Errorf("size %d: the source gave %d bytes, want %d", size, src.n, len(line))
		}
		if maxHeld > size || holds > 1 {
			t.Errorf("size %d: held up to %d bytes in %d holds, want at most one read's worth once", size, maxHeld, holds)
		}
	}
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
