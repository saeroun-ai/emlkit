package message

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/textproto"
)

// totalBlock is one header block of a totalFixture in the order the parse
// reads it, with what it costs and what reading it makes available.
type totalBlock struct {
	path       []int
	offset     int64   // where the block starts in the message
	header     string  // the block's wire bytes
	candidates []int64 // offsets, within the block, of its field candidates
	bodies     []string
}

// totalFixture is a message described block by block, so that every expected
// total, offset and recovered body is derived from the fixture itself.
type totalFixture struct {
	text   string
	blocks []totalBlock

	// lfNote is where the one header.bare_lf note of an LF fixture is: the
	// first line ending of the root's header block, which is read before any
	// part's (ROB-010-002). It is -1 for a CRLF fixture, which records none.
	lfNote int64
}

// withLFNote sets f.lfNote from f.text and the line ending it was built with.
func (f totalFixture) withLFNote(eol string) totalFixture {
	f.lfNote = -1
	if eol == "\n" {
		f.lfNote = int64(strings.IndexByte(f.text, '\n'))
	}
	return f
}

// takeLFNote checks that diags begin with f's header.bare_lf note when it has
// one, then with the root's multipart.bare_lf exactly when reached says the
// root's first delimiter line was framed and looked at, and returns the
// diagnostics after them.
func (f totalFixture) takeLFNote(t *testing.T, label string, diags []Diagnostic, reached bool) []Diagnostic {
	t.Helper()
	if f.lfNote < 0 {
		return diags
	}
	if len(diags) == 0 || diags[0].Code != CodeHeaderBareLF || diags[0].Severity != SeverityInfo ||
		diags[0].Path != nil || !diags[0].Location.HasOffset ||
		diags[0].Location.Offset != f.lfNote || diags[0].Location.EndOffset != f.lfNote+1 {
		t.Fatalf("%s: Diagnostics() = %+v, want the root's header.bare_lf at %d first", label, diags, f.lfNote)
	}
	at := int64(-1)
	if reached {
		at = rootDelimiterLF(f.text)
	}
	return takeMultipartLFNote(t, label, diags[1:], at)
}

// candidatesOf returns where each field candidate of a header block begins:
// every line that is neither a fold nor the empty line ending the block.
func candidatesOf(header string) []int64 {
	var out []int64
	for off := 0; off < len(header); {
		end := strings.IndexByte(header[off:], '\n') + off + 1
		line := strings.TrimRight(header[off:end], "\r\n")
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			out = append(out, int64(off))
		}
		off = end
	}
	return out
}

// newTotalFixture builds the tree every test below shares: a root container,
// a leaf with two fields, a part with no header block, a nested container
// whose second child carries an empty-name candidate, a part with an empty
// header block, and a folded leaf. eol is the line ending.
func newTotalFixture(eol string) totalFixture {
	var b strings.Builder
	var f totalFixture
	header := func(path []int, h string, bodies ...string) {
		h = strings.ReplaceAll(h, "\n", eol)
		f.blocks = append(f.blocks, totalBlock{path: path, offset: int64(b.Len()), header: h, candidates: candidatesOf(h), bodies: bodies})
		b.WriteString(h)
	}
	w := func(s string) { b.WriteString(strings.ReplaceAll(s, "\n", eol)) }

	header(nil, "Content-Type: multipart/mixed; boundary=OUTER\n\n")
	w("preamble that is not a header\n--OUTER\n")
	header([]int{0}, "Content-Type: text/plain\nX-A: 1\n\n", "one", "")
	w("one\n--OUTER\n")
	// A part with no header block: two delimiters in a row. It costs nothing
	// and is returned with the block before it.
	w("--OUTER\n")
	header([]int{2}, "Content-Type: multipart/alternative; boundary=INNER\n\n")
	w("--INNER\n")
	header([]int{2, 0}, "Content-Type: text/plain\n\n", "deep")
	w("deep\n--INNER\n")
	header([]int{2, 1}, ": empty name\nX-B: 2\n\n", "deep2")
	w("deep2\n--INNER--\ninner epilogue\n--OUTER\n")
	header([]int{3}, "\n", "four")
	w("four\n--OUTER\n")
	header([]int{4}, "X-C: 3\n folded\n\n", "five")
	w("five\n--OUTER--\nouter epilogue\n")
	f.text = b.String()
	return f.withLFNote(eol)
}

// cost is what one block charges to the named total.
func (b totalBlock) cost(kind string) int64 {
	if kind == "bytes" {
		return int64(len(b.header))
	}
	return int64(len(b.candidates))
}

// totalsOnly returns every budget disabled except the named total, so that what
// a test observes is that budget alone.
func totalsOnly(kind string, limit int64) Limits {
	l := allDisabled()
	if kind == "bytes" {
		l.MaxTotalHeaderBytes = limit
	} else {
		l.MaxTotalHeaderCount = limit
	}
	return l
}

func totalCode(kind string) Code {
	if kind == "bytes" {
		return CodeLimitTotalHeaderBytes
	}
	return CodeLimitTotalHeaderCount
}

// expectTotal returns what a parse of f under limit yields: the bodies recovered,
// and the block refused with where (nil when the whole message fits).
func (f totalFixture) expectTotal(kind string, limit int64) (bodies []string, refused *totalBlock, at int64) {
	bodies = []string{}
	var used int64
	for i := range f.blocks {
		blk := &f.blocks[i]
		if c := blk.cost(kind); used+c > limit {
			if kind == "bytes" {
				return bodies, blk, blk.offset + limit - used
			}
			return bodies, blk, blk.offset + blk.candidates[limit-used]
		}
		used += blk.cost(kind)
		bodies = append(bodies, blk.bodies...)
	}
	return bodies, nil, 0
}

// boundaries returns every exact total at a block boundary, and one either
// side of it.
func (f totalFixture) boundaries(kind string) []int64 {
	seen := map[int64]bool{}
	var out []int64
	var used int64
	for _, b := range f.blocks {
		used += b.cost(kind)
		for _, d := range []int64{-1, 0, 1} {
			if v := used + d; v >= 1 && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// checkTotalStop pins the one stop a total failure publishes: a FatalError
// carrying exactly one recorded diagnostic of the total's code, naming the
// refused block's entity and the exact position, and the identical value from
// every scope that reports it.
func checkTotalStop(t *testing.T, session *ParseSession, err error, kind string, limit int64, blk *totalBlock, at int64) {
	t.Helper()
	checkTotalStopDiags(t, session, session.Diagnostics(), err, kind, limit, blk, at)
}

// checkTotalStopDiags is checkTotalStop holding diags, rather than everything
// the session recorded, to exactly the one total diagnostic.
func checkTotalStopDiags(t *testing.T, session *ParseSession, diags []Diagnostic, err error, kind string, limit int64, blk *totalBlock, at int64) {
	t.Helper()
	if !IsFatal(err) {
		t.Fatalf("err = %v, want a FatalError", err)
	}
	if stop := session.fatalErr(); stop == nil || error(stop) != err {
		t.Errorf("session stop = %v, want the identical %v", stop, err)
	}
	var fe FatalError
	errors.As(err, &fe)
	d, ok := fe.Diagnostic()
	if !ok || d.Code != totalCode(kind) {
		t.Fatalf("fatal diagnostic = %+v, want %s", d, totalCode(kind))
	}
	if len(diags) != 1 || diags[0].Code != totalCode(kind) || diags[0].Severity != SeverityLimitExceeded {
		t.Fatalf("Diagnostics() = %v, want one %s", codesOfDiags(diags), totalCode(kind))
	}
	if !reflect.DeepEqual(diags[0].Path, blk.path) && !(len(diags[0].Path) == 0 && len(blk.path) == 0) {
		t.Errorf("Path = %v, want %v", diags[0].Path, blk.path)
	}
	if diags[0].Location.Offset != at {
		t.Errorf("Offset = %d, want %d", diags[0].Location.Offset, at)
	}
	if !strings.Contains(diags[0].Message, fmt.Sprint(limit)) {
		t.Errorf("Message = %q, want the budget %d", diags[0].Message, limit)
	}
	sentinel := textproto.ErrHeaderTooBig
	if kind == "count" {
		sentinel = textproto.ErrTooManyHeaderFields
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, %v) = false", sentinel)
	}
}

// TestParse_totalHeaders_boundaries pins both totals at every block boundary
// and one either side of it, over every line ending and input chunking: a
// message within the total is read whole with nothing recorded, and one past
// it is stopped at exactly the block that crossed it, keeping every body
// returned before.
func TestParse_totalHeaders_boundaries(t *testing.T) {
	for _, eol := range []string{"\r\n", "\n"} {
		f := newTotalFixture(eol)
		for _, kind := range []string{"bytes", "count"} {
			for _, limit := range f.boundaries(kind) {
				for name, src := range chunkedSources(f.text) {
					verifyTotalParse(t, fmt.Sprintf("%s %q limit %d %s", kind, eol, limit, name), f, kind, limit, src())
				}
			}
		}
	}
}

// verifyTotalParse parses f from src under one total and holds the result to
// what the fixture predicts.
func verifyTotalParse(t *testing.T, label string, f totalFixture, kind string, limit int64, src io.Reader) {
	t.Helper()
	wantBodies, refused, at := f.expectTotal(kind, limit)
	session, e, err := Parse(src, &ParseOptions{Limits: totalsOnly(kind, limit)})
	if refused != nil && refused.path == nil {
		if e != nil {
			t.Fatalf("%s: entity returned for a refused message header", label)
		}
		checkTotalStop(t, session, err, kind, limit, refused, at)
		return
	}
	if err != nil {
		t.Fatalf("%s: Parse() = %v", label, err)
	}
	got := readTree(e)
	if !reflect.DeepEqual(got.bodies, wantBodies) && !(len(got.bodies) == 0 && len(wantBodies) == 0) {
		t.Errorf("%s: bodies = %q, want %q", label, got.bodies, wantBodies)
	}
	// The root's header was read, so an LF fixture has recorded its one
	// header.bare_lf note before anything else. The root's first NextPart
	// then frames its first delimiter line and looks at it, unless that call
	// is the one the total refuses — the first child's own block — which stops
	// the parse with nothing looked at.
	reached := refused == nil || !reflect.DeepEqual(refused.path, []int{0})
	diags := f.takeLFNote(t, label, session.Diagnostics(), reached)
	if refused == nil {
		if got.err != nil || len(diags) != 0 {
			t.Errorf("%s: (%v, %v), want a clean parse", label, got.err, codesOfDiags(diags))
		}
		return
	}
	checkTotalStopDiags(t, session, diags, got.err, kind, limit, refused, at)
	if e.ParseStop() != got.err {
		t.Errorf("%s: ParseStop() = %v, want the identical stop", label, e.ParseStop())
	}
	// Walk reports the identical stop too.
	if werr := e.Walk(func([]int, *Entity, error) error { return nil }); werr != got.err {
		t.Errorf("%s: Walk() = %v, want the identical stop", label, werr)
	}
}

// genTotalFixture builds a valid multipart tree from seed: nested containers,
// leaves with folded and empty-name fields, empty header blocks, and parts with
// no header block. Every body is plain and identity-encoded, so the oracle is
// exact and no decoder's handling of malformed input is involved.
//
// Every container holds one to four parts, as RFC 2046 requires at least one.
// A container with none would be framed by its closing delimiter alone, which
// tells the multipart reader nothing about the line ending; with LF endings it
// is then not recognized and iteration ends with a wrapped EOF. That is
// existing framing behavior, unchanged by the header totals and reproduced on
// b994934, and outside what this fixture is meant to exercise.
func genTotalFixture(seed []byte, eol string) totalFixture {
	i := 0
	next := func() byte {
		if i >= len(seed) {
			return 0
		}
		i++
		return seed[i-1]
	}
	var b strings.Builder
	var f totalFixture
	addBlock := func(path []int, h string) {
		f.blocks = append(f.blocks, totalBlock{path: append([]int(nil), path...), offset: int64(b.Len()), header: h, candidates: candidatesOf(h)})
		b.WriteString(h)
	}
	addBody := func(s string) {
		last := &f.blocks[len(f.blocks)-1]
		last.bodies = append(last.bodies, s)
	}
	leaves, boundaries := 0, 0
	var container func(path []int, depth int)
	container = func(path []int, depth int) {
		boundaries++
		bd := fmt.Sprintf("B%d", boundaries)
		addBlock(path, "Content-Type: multipart/mixed; boundary="+bd+eol+eol)
		needEOL := false
		for c := range 1 + int(next()%4) {
			if needEOL {
				b.WriteString(eol)
			}
			b.WriteString("--" + bd + eol)
			child := append(append([]int(nil), path...), c)
			needEOL = true
			switch k := next() % 4; {
			case k == 3 && depth < 3:
				container(child, depth+1)
			case k == 2:
				// No header block: the next delimiter follows at once.
				addBody("")
				needEOL = false
			case k == 1:
				addBlock(child, eol)
				leaves++
				body := fmt.Sprintf("b%d", leaves)
				addBody(body)
				b.WriteString(body)
			default:
				var h strings.Builder
				for range 1 + int(next()%3) {
					flag := next()
					if flag%5 == 0 {
						h.WriteString(": empty name")
					} else {
						fmt.Fprintf(&h, "X-F%d: v", flag%7)
					}
					if flag%3 == 0 {
						h.WriteString(eol + " folded")
					}
					h.WriteString(eol)
				}
				h.WriteString(eol)
				addBlock(child, h.String())
				leaves++
				body := fmt.Sprintf("b%d", leaves)
				addBody(body)
				b.WriteString(body)
			}
		}
		if needEOL {
			b.WriteString(eol)
		}
		b.WriteString("--" + bd + "--")
	}
	container(nil, 0)
	b.WriteString(eol + "epilogue" + eol)
	f.text = b.String()
	return f.withLFNote(eol)
}

// FuzzParse_totalHeaders holds generated valid trees to the exact outcome the
// fixture predicts under either total, across line endings and chunkings: the
// bodies before the crossing block, one diagnostic at the exact place, and the
// identical stop from every scope.
func FuzzParse_totalHeaders(f *testing.F) {
	f.Add([]byte{3, 0, 1, 3, 2, 0, 3, 1, 2, 4, 5}, uint16(50), uint8(0))
	f.Add([]byte{4, 3, 4, 0, 2, 2, 1, 3, 3, 0, 0}, uint16(7), uint8(3))
	f.Add([]byte{2, 2, 1}, uint16(1), uint8(6))
	f.Fuzz(func(t *testing.T, seed []byte, n uint16, mode uint8) {
		if len(seed) > 512 {
			return
		}
		eol, kind := "\r\n", "bytes"
		if mode&1 != 0 {
			eol = "\n"
		}
		if mode&2 != 0 {
			kind = "count"
		}
		fx := genTotalFixture(seed, eol)
		var total int64
		for _, b := range fx.blocks {
			total += b.cost(kind)
		}
		limit := 1 + int64(n)%(total+2)
		var src io.Reader = &chunkReader{s: fx.text, n: 1 + int(mode>>2)}
		if mode>>2 == 63 {
			src = strings.NewReader(fx.text)
		}
		verifyTotalParse(t, fmt.Sprintf("%s limit %d", kind, limit), fx, kind, limit, src)
	})
}

// TestParse_totalHeaders_everyIteratorStops pins that a total failure deep in
// the tree stops the iterators above it too, with the identical value, and
// that bodies already returned stay readable.
func TestParse_totalHeaders_everyIteratorStops(t *testing.T) {
	f := newTotalFixture("\r\n")
	// Exactly through [2, 0]: [2, 1] is refused.
	var limit int64
	for _, b := range f.blocks[:4] {
		limit += b.cost("bytes")
	}
	_, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: totalsOnly("bytes", limit)})
	if err != nil {
		t.Fatal(err)
	}
	outer := e.MultipartReader()
	var kept []*Entity
	for range 3 {
		p, err := outer.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		kept = append(kept, p)
	}
	inner := kept[2].MultipartReader()
	deep, err := inner.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(deep.Body); err != nil || string(b) != "deep" {
		t.Errorf("deep body = %q, %v", b, err)
	}
	_, stop := inner.NextPart()
	if !IsFatal(stop) {
		t.Fatalf("inner NextPart() = %v, want the stop", stop)
	}
	for i := range 2 {
		if _, err := inner.NextPart(); err != stop {
			t.Errorf("inner again %d = %v", i, err)
		}
		if _, err := outer.NextPart(); err != stop {
			t.Errorf("outer = %v, want the identical stop", err)
		}
	}
	// The entities already returned stay valid.
	if kept[0].Header.Get("X-A") != "1" || kept[2].Header.Get("Content-Type") == "" {
		t.Errorf("returned headers changed")
	}
}

// TestParse_totalHeaders_refusedChildAccounting pins what refused children
// leave charged. A block a per-entity budget refuses keeps what the scanner
// admitted before the refusing read; a child MaxMIMEDepth refuses was read
// whole and is charged whole. Neither is refunded, and the later sibling fits
// or fails by exactly that much.
func TestParse_totalHeaders_refusedChildAccounting(t *testing.T) {
	const (
		root   = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
		admit  = "X-A: 1\r\n"
		later  = "X-Z: 9\r\n\r\n"
		deep   = "Content-Type: multipart/mixed; boundary=C\r\n\r\n"
		inside = "--C\r\nContent-Type: text/plain\r\n\r\nin\r\n--C--\r\n"
	)
	long := "X-Long: " + strings.Repeat("y", 100) + "\r\n"

	t.Run("per-entity refusal", func(t *testing.T) {
		// The refused child is a container whose own child's header is
		// refused by MaxHeaderFieldBytes on its second field.
		text := root + "--B\r\n" + deep + "--C\r\n" + admit + long + "\r\nx\r\n--C--\r\n--B\r\n" + later + "last\r\n--B--\r\n"
		used := int64(len(root) + len(deep) + len(admit))
		for _, delta := range []int64{-1, 0} {
			l := totalsOnly("bytes", used+int64(len(later))+delta)
			l.MaxHeaderFieldBytes = 50
			session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: l})
			if err != nil {
				t.Fatal(err)
			}
			mr := e.MultipartReader()
			c, err := mr.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.MultipartReader().NextPart(); !IsFatal(err) || session.fatalErr() != nil {
				t.Fatalf("refused grandchild = %v, stop %v: want a local failure", err, session.fatalErr())
			}
			p, err := mr.NextPart()
			if delta == 0 {
				if err != nil {
					t.Fatalf("later sibling = %v, want it to fit", err)
				}
				if b, _ := io.ReadAll(p.Body); string(b) != "last" {
					t.Errorf("later body = %q", b)
				}
				continue
			}
			if !IsFatal(err) || session.fatalErr() == nil {
				t.Errorf("later sibling = %v, want the total stop", err)
			}
		}
	})

	t.Run("depth refusal", func(t *testing.T) {
		text := root + "--B\r\n" + deep + inside + "--B\r\n" + later + "last\r\n--B--\r\n"
		used := int64(len(root) + len(deep) + len("Content-Type: text/plain\r\n\r\n"))
		for _, delta := range []int64{-1, 0} {
			l := totalsOnly("bytes", used+int64(len(later))+delta)
			l.MaxMIMEDepth = 1
			session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: l})
			if err != nil {
				t.Fatal(err)
			}
			mr := e.MultipartReader()
			c, err := mr.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.MultipartReader().NextPart(); !IsFatal(err) || session.fatalErr() != nil {
				t.Fatalf("too deep child = %v, want a local failure", err)
			}
			_, err = mr.NextPart()
			if (delta == 0) != (err == nil) {
				t.Errorf("limit%+d: later sibling = %v", delta, err)
			}
		}
	})

	t.Run("part count first sees the total", func(t *testing.T) {
		// The second child crosses the total and would exceed MaxPartCount
		// too. It reports the total: its header is read before it is counted.
		first := "X: 1\r\n\r\n"
		text := root + "--B\r\n" + first + "a\r\n--B\r\n" + later + "x\r\n--B--\r\n"
		l := totalsOnly("bytes", int64(len(root)+len(first)))
		l.MaxPartCount = 1
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		mr := e.MultipartReader()
		if _, err := mr.NextPart(); err != nil {
			t.Fatal(err)
		}
		_, err = mr.NextPart()
		if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitTotalHeaderBytes}) || !IsFatal(err) {
			t.Errorf("(%v, %v), want the total", codes, err)
		}
	})
}

// TestParse_totalHeaders_notCharged pins what the totals never see: a container
// drained without being iterated, a body read directly instead of iterated,
// and multipart framing, preamble and epilogue.
func TestParse_totalHeaders_notCharged(t *testing.T) {
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	const second = "X: 2\r\n\r\n"
	nested := "Content-Type: multipart/mixed; boundary=C\r\n\r\n" + strings.Repeat("--C\r\nX-N: n\r\n\r\nn\r\n", 50) + "--C--\r\n"
	text := root + strings.Repeat("preamble\r\n", 50) + "--B\r\n" + nested + "--B\r\n" + second + "two\r\n--B--\r\n" + strings.Repeat("epilogue\r\n", 50)
	exact := int64(len(root) + len("Content-Type: multipart/mixed; boundary=C\r\n\r\n") + len(second))

	for _, delta := range []int64{-1, 0} {
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: totalsOnly("bytes", exact+delta)})
		if err != nil {
			t.Fatal(err)
		}
		mr := e.MultipartReader()
		if _, err := mr.NextPart(); err != nil {
			t.Fatal(err)
		}
		// The container's parts are never iterated; NextPart drains them.
		p, err := mr.NextPart()
		if delta < 0 {
			if session.fatalErr() == nil || err != error(session.fatalErr()) {
				t.Errorf("limit-1: %v, want the stop", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("second = %v", err)
		}
		if b, _ := io.ReadAll(p.Body); string(b) != "two" {
			t.Errorf("second body = %q", b)
		}
		if _, err := mr.NextPart(); err != io.EOF {
			t.Errorf("end = %v", err)
		}
	}

	// Reading a container's body directly charges none of its parts' headers.
	_, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: totalsOnly("bytes", int64(len(root)))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(e.Body); err != nil {
		t.Errorf("raw body = %v", err)
	}
}

// TestParse_totalHeaders_replacedBody pins that the parts of a body the caller
// replaced are parts of the same parse and spend the same totals.
func TestParse_totalHeaders_replacedBody(t *testing.T) {
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	replacement := "--B\r\nX: 1\r\n\r\none\r\n--B--\r\n"
	for _, delta := range []int64{-1, 0} {
		session, e, err := Parse(strings.NewReader(root+"--B--\r\n"), &ParseOptions{Limits: totalsOnly("count", 2+delta)})
		if err != nil {
			t.Fatal(err)
		}
		e.Body = strings.NewReader(replacement)
		_, err = e.MultipartReader().NextPart()
		if (delta == 0) != (err == nil) {
			t.Errorf("limit%+d: NextPart() = %v", delta, err)
		}
		if delta < 0 {
			d := session.Diagnostics()
			if len(d) != 1 || d[0].Code != CodeLimitTotalHeaderCount || d[0].Location != (Location{}) {
				t.Errorf("Diagnostics() = %+v, want one with no position", d)
			}
		}
	}
}

// errEndlessHeaderCap is what endlessHeader fails with once it has delivered
// endlessHeaderCap bytes: a budget that does not stop the candidate fails the
// test with a bounded read instead of exhausting memory.
var errEndlessHeaderCap = errors.New("test safety cap: endless header read to its cap")

const endlessHeaderCap = 64 << 10

// endlessHeader delivers prefix and then 'a' until endlessHeaderCap bytes in
// all, counting what it delivered.
type endlessHeader struct {
	prefix string
	n      int64
}

func (r *endlessHeader) Read(p []byte) (int, error) {
	if r.n >= endlessHeaderCap {
		return 0, errEndlessHeaderCap
	}
	p = p[:min(int64(len(p)), endlessHeaderCap-r.n)]
	n := copy(p, r.prefix)
	r.prefix = r.prefix[n:]
	for i := n; i < len(p); i++ {
		p[i] = 'a'
	}
	r.n += int64(len(p))
	return len(p), nil
}

// TestParse_totalHeaders_countPreflight pins the count's bound on a candidate
// with every other budget off, MaxInputBytes included: once the count is
// spent, the next candidate is refused at its first byte, whatever kind of
// candidate it is, however long it would be. The leading-whitespace line is no
// candidate, and keeps the malformed-header error it always had.
func TestParse_totalHeaders_countPreflight(t *testing.T) {
	const head = "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n"
	// A bare '\r' at the start of a part's header is the empty line that ends
	// it (ROB-010-002): no candidate, so nothing for the spent count to refuse.
	t.Run(`"\r" is no candidate`, func(t *testing.T) {
		src := &endlessHeader{prefix: "\r"}
		session, e, err := Parse(io.MultiReader(strings.NewReader(head), src), &ParseOptions{Limits: totalsOnly("count", 1)})
		if err != nil {
			t.Fatal(err)
		}
		p, err := e.MultipartReader().NextPart()
		if err != nil || p == nil || p.Header.Len() != 0 || session.fatalErr() != nil {
			t.Fatalf("NextPart() = (%v, %v), want a part with an empty header and no stop", p, err)
		}
		if d := session.Diagnostics(); len(d) != 1 || d[0].Code != CodeHeaderBareCR || !d[0].Location.HasOffset || d[0].Location.Offset != int64(len(head)) {
			t.Errorf("diagnostics = %+v, want one header.bare_cr at %d", d, len(head))
		}
		if src.n > 2*4096 {
			t.Errorf("read %d bytes past the header, want at most two buffers", src.n)
		}
	})
	for _, prefix := range []string{"X: ", ":", "X-no-colon", "bad key\x01"} {
		t.Run(fmt.Sprintf("%q", prefix), func(t *testing.T) {
			src := &endlessHeader{prefix: prefix}
			session, e, err := Parse(io.MultiReader(strings.NewReader(head), src), &ParseOptions{Limits: totalsOnly("count", 1)})
			if err != nil {
				t.Fatal(err)
			}
			mr := e.MultipartReader()
			p, err := mr.NextPart()
			if errors.Is(err, errEndlessHeaderCap) {
				t.Fatalf("the candidate was read to the test's %d-byte safety cap", endlessHeaderCap)
			}
			_, again := mr.NextPart()
			if p != nil || err == nil || err != again || session.fatalErr() == nil {
				t.Fatalf("NextPart() = (%v, %v) then %v, want the identical stop", p, err, again)
			}
			blk := &totalBlock{path: []int{0}}
			checkTotalStop(t, session, err, "count", 1, blk, int64(len(head)))
			// At most the root's buffer and the part reader's buffer were filled
			// ahead; nothing of the candidate is read past them.
			if src.n > 2*4096 {
				t.Errorf("read %d bytes of the refused candidate, want at most two buffers", src.n)
			}
		})
	}

	t.Run("leading whitespace residual", func(t *testing.T) {
		text := head + " leading white space\r\n\r\nbody\r\n--x--\r\n"
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: totalsOnly("count", 1)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.MultipartReader().NextPart()
		if err == nil || IsFatal(err) || session.Diagnostics() != nil {
			t.Errorf("NextPart() = %v, %v: want the malformed-header error, undiagnosed", err, codesOfDiags(session.Diagnostics()))
		}
	})

	t.Run("non-candidates are free", func(t *testing.T) {
		// The root's one field spends the count; a part with an empty header
		// block, and one with none, still come through.
		text := head + "\r\none\r\n--x\r\n--x\r\n\r\nthree\r\n--x--\r\n"
		session, got, err := parseUnder(t, text, totalsOnly("count", 1))
		if err != nil || got.err != nil {
			t.Fatalf("(%v, %v)", err, got.err)
		}
		if want := []string{"one", "", "three"}; !reflect.DeepEqual(got.bodies, want) {
			t.Errorf("bodies = %q, want %q", got.bodies, want)
		}
		if session.Diagnostics() != nil {
			t.Errorf("Diagnostics() = %v", codesOfDiags(session.Diagnostics()))
		}
	})
}

// TestParse_totalHeaders_precedence pins how a total failure ranks against
// everything else that can end a parse at the same place.
func TestParse_totalHeaders_precedence(t *testing.T) {
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	// The child's one line is longer than the root's, so a per-entity line or
	// field budget between the two refuses the child alone.
	child := "X-A: " + strings.Repeat("0", 100) + "\r\n\r\n"
	text := root + "--B\r\n" + child + "one\r\n--B--\r\n"

	t.Run("per-entity budget first", func(t *testing.T) {
		l := totalsOnly("bytes", int64(len(root))+3)
		l.MaxLineBytes = 60
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.MultipartReader().NextPart()
		if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitLineBytes}) || session.fatalErr() != nil {
			t.Errorf("(%v, %v), want only the local line refusal", codes, err)
		}
	})

	t.Run("count preflight before per-entity budgets", func(t *testing.T) {
		l := totalsOnly("count", 1)
		l.MaxHeaderCount, l.MaxLineBytes, l.MaxHeaderFieldBytes = 1, 60, 62
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.MultipartReader().NextPart()
		if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitTotalHeaderCount}) || !IsFatal(err) {
			t.Errorf("(%v, %v), want the total count", codes, err)
		}
	})

	t.Run("sink refusal wins", func(t *testing.T) {
		refuse := errors.New("caller stop")
		session, e, err := Parse(strings.NewReader(text), &ParseOptions{
			Limits:         totalsOnly("bytes", int64(len(root))),
			DiagnosticSink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return refuse }),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.MultipartReader().NextPart()
		if !errors.Is(err, refuse) || error(session.fatalErr()) != err {
			t.Errorf("NextPart() = %v, want the sink's stop", err)
		}
		var fe FatalError
		if errors.As(err, &fe) {
			if _, ok := fe.Diagnostic(); ok {
				t.Errorf("the sink stop carries a diagnostic")
			}
		}
	})

	t.Run("earlier stop wins and records nothing", func(t *testing.T) {
		calls := 0
		refuse := errors.New("caller stop")
		// A parameter diagnostic on the root stops the parse first.
		params := "Content-Type: multipart/mixed; boundary=B; a=1; b=2\r\n\r\n"
		l := totalsOnly("bytes", int64(len(params)))
		l.MaxParameterCount = 1
		session, e, err := Parse(strings.NewReader(params+"--B\r\n"+child+"one\r\n--B--\r\n"), &ParseOptions{
			Limits:         l,
			DiagnosticSink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { calls++; return refuse }),
		})
		if e == nil || !errors.Is(err, refuse) {
			t.Fatalf("Parse() = %v", err)
		}
		if mr := e.MultipartReader(); mr != nil {
			if _, err := mr.NextPart(); !errors.Is(err, refuse) {
				t.Errorf("NextPart() = %v, want the earlier stop", err)
			}
		}
		if calls != 1 || session.Len() != 1 {
			t.Errorf("sink calls %d, retained %d: want only the earlier diagnostic", calls, session.Len())
		}
	})

	t.Run("diagnostic cap does not hide it", func(t *testing.T) {
		params := "Content-Type: multipart/mixed; boundary=B; a=1; b=2\r\n\r\n"
		l := totalsOnly("bytes", int64(len(params)))
		l.MaxParameterCount, l.MaxDiagnosticCount = 1, 1
		session, e, err := Parse(strings.NewReader(params+"--B\r\n"+child+"one\r\n--B--\r\n"), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.MultipartReader().NextPart()
		var fe FatalError
		if !errors.As(err, &fe) {
			t.Fatalf("NextPart() = %v, want a FatalError", err)
		}
		if d, _ := fe.Diagnostic(); d.Code != CodeLimitTotalHeaderBytes {
			t.Errorf("stop diagnostic = %s", d.Code)
		}
		if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitDiagnosticCount}) {
			t.Errorf("Diagnostics() = %v, want only the terminal", codes)
		}
	})

	t.Run("input budget during the same read wins", func(t *testing.T) {
		// The child block has no terminator inside the input budget, so the
		// header reader needs the byte the input refuses.
		in := root + "--B\r\nX-A: " + strings.Repeat("q", 200)
		l := totalsOnly("bytes", int64(len(root))+10)
		l.MaxInputBytes = int64(len(in))
		session, e, err := Parse(strings.NewReader(in+"\r\n\r\nx\r\n--B--\r\n"), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.MultipartReader().NextPart()
		if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitInputBytes}) || !IsFatal(err) {
			t.Fatalf("(%v, %v), want the input stop alone", codes, err)
		}
	})
}

// TestParse_totalHeaders_foreignRefusal pins provenance: an input that fails
// with a total-kind refusal it did not get from this parse — forged, or
// genuine from another budget — is an ordinary read failure, not a stop.
func TestParse_totalHeaders_foreignRefusal(t *testing.T) {
	other := textproto.NewHeaderBudget(1, 0)
	_, foreign := textproto.ReadHeaderLimits(bufio.NewReader(strings.NewReader("A: 1\r\n\r\n")), textproto.HeaderLimits{Total: other})
	if foreign == nil {
		t.Fatal("no foreign refusal")
	}
	// The cycle is fused, so that a classification that walked it without a
	// bound would blow the fuse and fail here rather than spin. Before
	// ROB-120-007 the child case hung in textproto's errors.Is and was skipped.
	cyclic := &fusedLoop{}
	for name, fail := range map[string]error{
		"foreign":        foreign,
		"wrapped":        fmt.Errorf("x: %w", foreign),
		"forged":         &textproto.HeaderLimitError{},
		"cyclic":         cyclic,
		"panicking As":   &trappedError{},
		"typed nil kind": (*textproto.HeaderLimitError)(nil),
	} {
		const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nX: 1\r\n"
		for _, at := range []string{"root", "child"} {
			in := root
			if at == "root" {
				in = "X: 1\r\n"
			}
			session, e, err := Parse(io.MultiReader(strings.NewReader(in), failingReader{fail}), &ParseOptions{Limits: totalsOnly("bytes", 1000)})
			if at == "child" {
				if err != nil {
					t.Fatalf("%s %s: Parse() = %v", name, at, err)
				}
				_, err = e.MultipartReader().NextPart()
			}
			// A type assertion, so that nothing asks the hostile error anything.
			if _, fatal := err.(FatalError); err == nil || fatal || session.fatalErr() != nil || session.Diagnostics() != nil {
				t.Errorf("%s %s: err %v, stop %v, %v: want an ordinary failure", name, at, err, session.fatalErr(), codesOfDiags(session.Diagnostics()))
			}
		}
	}
	if cyclic.blown {
		t.Error("the cycle's fuse blew: a classification walked it without a bound")
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// TestParse_totalHeaders_configuration pins the totals' configuration: zero is
// the default, LimitDisabled turns one off without the other, values below it
// are rejected, and math.MaxInt64 is a finite budget.
func TestParse_totalHeaders_configuration(t *testing.T) {
	big := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + strings.Repeat("--B\r\nX: 1\r\nY: 2\r\n\r\nb\r\n", 200) + "--B--\r\n"

	for _, l := range []Limits{
		{MaxTotalHeaderBytes: LimitDisabled, MaxTotalHeaderCount: LimitDisabled},
		{MaxTotalHeaderBytes: math.MaxInt64, MaxTotalHeaderCount: math.MaxInt64},
		{},
	} {
		session, got, err := parseUnder(t, big, l)
		if err != nil || got.err != nil || len(got.bodies) != 200 || session.Diagnostics() != nil {
			t.Errorf("%+v: (%v, %v, %d bodies)", l, err, got.err, len(got.bodies))
		}
	}
	// Disabling one total leaves the other enforced.
	l := Limits{MaxTotalHeaderBytes: LimitDisabled, MaxTotalHeaderCount: 100}
	if _, got, _ := parseUnder(t, big, l); !IsFatal(got.err) {
		t.Errorf("count with bytes disabled = %v", got.err)
	}
	l = Limits{MaxTotalHeaderBytes: 500, MaxTotalHeaderCount: LimitDisabled}
	if _, got, _ := parseUnder(t, big, l); !IsFatal(got.err) {
		t.Errorf("bytes with count disabled = %v", got.err)
	}
	for _, l := range []Limits{{MaxTotalHeaderBytes: -2}, {MaxTotalHeaderCount: math.MinInt64}} {
		_, e, err := Parse(strings.NewReader(big), &ParseOptions{Limits: l})
		if e != nil || !errors.Is(err, ErrInvalidLimits) {
			t.Errorf("%+v: (%v, %v), want ErrInvalidLimits", l, e, err)
		}
	}
	if s, _, _ := Parse(strings.NewReader(big), nil); s.Limits().MaxTotalHeaderBytes != 32<<20 || s.Limits().MaxTotalHeaderCount != 100000 {
		t.Errorf("defaults = %d, %d", s.Limits().MaxTotalHeaderBytes, s.Limits().MaxTotalHeaderCount)
	}
	// Both disabled: no shared budget exists at all.
	s, _, _ := Parse(strings.NewReader(big), &ParseOptions{Limits: Limits{MaxTotalHeaderBytes: LimitDisabled, MaxTotalHeaderCount: LimitDisabled}})
	if s.headerLimits().Total != nil {
		t.Errorf("a shared budget exists with both totals disabled")
	}
}

// TestParse_totalHeaders_legacyUnchanged pins that Read and ReadWithOptions
// have no totals: a message far past both defaults' counterparts at this
// scale reads whole under them.
func TestParse_totalHeaders_legacyUnchanged(t *testing.T) {
	big := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + strings.Repeat("--B\r\nX: 1\r\nY: 2\r\n\r\nb\r\n", 200) + "--B--\r\n"
	for _, read := range []func() (*Entity, error){
		func() (*Entity, error) { return Read(strings.NewReader(big)) },
		func() (*Entity, error) {
			return ReadWithOptions(strings.NewReader(big), &ReadOptions{MaxHeaderBytes: -1})
		},
	} {
		e, err := read()
		if err != nil {
			t.Fatal(err)
		}
		if got := readTree(e); got.err != nil || len(got.bodies) != 200 {
			t.Errorf("(%v, %d bodies)", got.err, len(got.bodies))
		}
	}
}

// TestParse_totalHeaders_tolerantRepairUnaffected pins that a message whose
// headers need tolerant repair, within the totals, parses and is diagnosed
// exactly as it is with the totals off.
func TestParse_totalHeaders_tolerantRepairUnaffected(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B; broken\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=us-ascii; ;\r\n: empty\r\nX; semicolon\r\n\r\none\r\n--B--\r\n"
	run := func(l Limits) (treeResult, []Code) {
		session, got, err := parseUnder(t, text, l)
		if err != nil {
			t.Fatal(err)
		}
		return got, codesOfDiags(session.Diagnostics())
	}
	off := DefaultLimits()
	off.MaxTotalHeaderBytes, off.MaxTotalHeaderCount = LimitDisabled, LimitDisabled
	wantTree, wantCodes := run(off)
	exact := DefaultLimits()
	exact.MaxTotalHeaderBytes = int64(strings.Index(text, "one\r\n")) - int64(len("--B\r\n"))
	exact.MaxTotalHeaderCount = 5
	for _, l := range []Limits{DefaultLimits(), exact} {
		gotTree, gotCodes := run(l)
		if !reflect.DeepEqual(gotTree, wantTree) || !reflect.DeepEqual(gotCodes, wantCodes) {
			t.Errorf("%+v: (%v, %v), want (%v, %v)", l, gotTree, gotCodes, wantTree, wantCodes)
		}
	}
}

// The accepted-path allocation check (ROB-130-010, issue #61) compares a parse
// with the totals at their defaults against one with both disabled. The
// totals' one shared budget is a constant cost of the parse; what the check
// exists to catch is a cost that grows with the header blocks and fields read.
// The two are measured apart:
//
//   - the constant: the mean extra allocations per parse at
//     totalAllocSmallParts parts, at most totalAllocMaxConstant (the one
//     budget, and one to spare, as before);
//   - the growth: how much that extra grows from totalAllocSmallParts to
//     totalAllocLargeParts parts, at most totalAllocMaxGrowth in all over the
//     49 more header blocks (98 more fields).
//
// A single comparison at 50 parts, as the check used to make, reads both at
// once against a bound of two, in a quantity that is not exact under the race
// detector. Every NextPart closes the part before it with
// io.Copy(io.Discard, part), whose buffer comes from a sync.Pool, and under
// -race sync.Pool.Put drops one value in four at random, so the next close
// allocates a new buffer. That is a source-level account, not a measured
// attribution: a model of it alone (each close allocating two objects with
// probability 1/4, the totals adding one) predicts the old check failing in
// about 22% of runs, against the 19 and 23 of 100 reported in #61. Whatever
// its source, a per-part noise of that kind grows with the parts, and the
// constant is measured where there are few, the growth over enough runs for
// its mean to settle; see totalAllocLargeRuns.
//
// Without -race, Put drops nothing at random, so that source of variation is
// absent and each quantity is expected to come out at its nominal increment.
// That is an expectation, not a guarantee of exact counts: a collection in the
// window still clears the pool, a pool can still miss, and the runtime or
// anything else in the process can still allocate in the window, all of which
// Mallocs counts. Each measurement starts with a collection to make the first
// less likely (see totalAllocMean); none of them is ruled out.
const (
	totalAllocSmallParts = 1
	totalAllocLargeParts = 50

	// totalAllocSmallRuns and totalAllocLargeRuns are the fixed numbers of
	// measured parses at each size, after one warm-up parse. They bound the
	// work: at most (1+1+runs) parses of the fixture per measurement, the
	// first of them checked against the safety caps below before any
	// repetition. Under the per-close model above, the difference of two
	// means over runs parses of p parts has a standard deviation of
	// sqrt(1.5*p/runs) allocations: about 0.09 for the constant and 0.4 for
	// the growth, so the one allocation between the totals' constant and its
	// bound is about 11 deviations, and the bound on the growth about 5. The
	// model is only a model: the bounds are not derived from it, it explains
	// why these run counts let the check settle.
	totalAllocSmallRuns = 200
	totalAllocLargeRuns = 500

	// totalAllocMaxConstant is the most the totals may add to a parse of
	// totalAllocSmallParts parts, as a mean: the one shared budget, and one
	// allocation to spare, which is the bound the check has always had.
	totalAllocMaxConstant = 2

	// totalAllocMaxGrowth is the most that added cost may grow, as a mean,
	// from totalAllocSmallParts to totalAllocLargeParts parts. One allocation
	// per header block or per field grows it by 49 or 98, and one per eighth
	// part by 6; amortized growth, such as a slice that gains one entry per
	// block and doubles when full, by about 5 over those sizes. What grows by
	// less — a few allocations in all — is below what this check resolves.
	totalAllocMaxGrowth = 2

	// totalAllocCapMallocs and totalAllocCapBytes bound one parse of either
	// fixture, checked on a single parse before it is repeated. They are far
	// above what a parse makes — the former check measured about 810
	// allocations for a 50-part parse of a one-field fixture (#61), and the
	// parse's own buffers are a few KiB each — so they never decide the check:
	// they stop a parse that has started to allocate without bound from being
	// replayed hundreds of times.
	totalAllocCapMallocs = 1 << 14
	totalAllocCapBytes   = 4 << 20

	// totalAllocMaxReads bounds the reads of one part's body.
	totalAllocMaxReads = 8
)

const totalAllocRootType = "multipart/mixed; boundary=B"

// totalAllocText is the measured fixture: a root container of parts parts,
// each with a header block of two fields and the body "b".
func totalAllocText(parts int) string {
	return "Content-Type: " + totalAllocRootType + "\r\n\r\n" +
		strings.Repeat("--B\r\nX: 1\r\nY: 2\r\n\r\nb\r\n", parts) + "--B--\r\n"
}

// totalAllocCase is one configuration a measurement parses under: the limits,
// whether they create the shared budget, and the deliberate allocations a
// control adds.
type totalAllocCase struct {
	limits    Limits
	wantTotal bool

	// perParse allocations are made once per parse, and one more every
	// everyNthPart parts (none when it is zero), each a 32-byte object kept in
	// totalAllocSink so that the compiler cannot elide it.
	perParse     int
	everyNthPart int
}

var totalAllocSink *[32]byte

func totalAllocWith() totalAllocCase {
	return totalAllocCase{limits: DefaultLimits(), wantTotal: true}
}

func totalAllocWithout() totalAllocCase {
	l := DefaultLimits()
	l.MaxTotalHeaderBytes, l.MaxTotalHeaderCount = LimitDisabled, LimitDisabled
	return totalAllocCase{limits: l}
}

// totalAllocRecord collects, without allocating, how many parses ran and what
// the first one that did not read the fixture exactly as written got wrong.
// what is a constant string and err the value the parse returned, so recording
// them allocates nothing; they are formatted only by check, after the
// measurement.
type totalAllocRecord struct {
	runs  int
	bad   int // 1-based; 0 if every run read the fixture exactly
	what  string
	err   error
	parts int
}

func (r *totalAllocRecord) fail(what string, err error, parts int) {
	if r.bad == 0 {
		r.bad, r.what, r.err, r.parts = r.runs, what, err, parts
	}
}

// check fails the test unless exactly runs parses ran and every one read the
// whole fixture: a parse that failed or stopped early allocates less, and must
// not pass for that.
func (r *totalAllocRecord) check(t *testing.T, label string, runs int) {
	t.Helper()
	if r.runs != runs {
		t.Fatalf("%s: %d parses ran, want %d", label, r.runs, runs)
	}
	if r.bad != 0 {
		t.Fatalf("%s: parse %d of %d: %s after %d parts (err %v)", label, r.bad, runs, r.what, r.parts, r.err)
	}
}

var (
	errTotalAllocBodyLong  = errors.New("part body longer than the fixture's")
	errTotalAllocBodyReads = errors.New("part body not ended within the read cap")
)

// readSmallBody reads r to its end into buf, in at most totalAllocMaxReads
// reads, without allocating.
func readSmallBody(r io.Reader, buf []byte) (int, error) {
	n := 0
	for range totalAllocMaxReads {
		m, err := r.Read(buf[n:])
		n += m
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if n == len(buf) {
			return n, errTotalAllocBodyLong
		}
	}
	return n, errTotalAllocBodyReads
}

// sessionHasTotal reports whether the parse created its shared budget, without
// creating one as headerLimits would.
func sessionHasTotal(s *ParseSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headerTotal != nil
}

// totalAllocRun is one measured parse: it parses the fixture of parts parts
// under c and reads the whole tree as a caller does, every body to its end,
// adding c's deliberate allocations. Every result is held to the fixture —
// the parse's error and entity, the root's and every part's fields and body,
// the part count, the end of the iteration, no diagnostic, no stop, and
// whether the shared budget exists — and the first difference recorded in rec.
// Nothing here formats, reflects or calls t.
//
// The loop asks for at most parts+1 parts and each body takes at most
// totalAllocMaxReads reads, whatever the parser returns.
func totalAllocRun(text string, parts int, c *totalAllocCase, rec *totalAllocRecord) {
	rec.runs++
	for range c.perParse {
		totalAllocSink = new([32]byte)
	}
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: c.limits})
	if err != nil || e == nil {
		rec.fail("Parse failed", err, 0)
		return
	}
	if e.Header.Len() != 1 || e.Header.Get("Content-Type") != totalAllocRootType {
		rec.fail("root header differs", nil, 0)
		return
	}
	mr := e.MultipartReader()
	if mr == nil {
		rec.fail("root is not multipart", nil, 0)
		return
	}
	var buf [8]byte
	n := 0
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil || p == nil {
			rec.fail("NextPart failed", err, n)
			return
		}
		if n++; n > parts {
			rec.fail("more parts than the fixture has", nil, n)
			return
		}
		if c.everyNthPart > 0 && n%c.everyNthPart == 0 {
			totalAllocSink = new([32]byte)
		}
		if p.Header.Len() != 2 || p.Header.Get("X") != "1" || p.Header.Get("Y") != "2" {
			rec.fail("part header differs", nil, n)
			return
		}
		got, err := readSmallBody(p.Body, buf[:])
		if err != nil {
			rec.fail("part body read failed", err, n)
			return
		}
		if string(buf[:got]) != "b" {
			rec.fail("part body differs", nil, n)
			return
		}
	}
	if n != parts {
		rec.fail("fewer parts than the fixture has", nil, n)
		return
	}
	if session.Len() != 0 {
		rec.fail("diagnostics recorded", nil, n)
		return
	}
	if stop := e.ParseStop(); stop != nil {
		rec.fail("parse stopped", stop, n)
		return
	}
	if sessionHasTotal(session) != c.wantTotal {
		rec.fail("shared budget presence differs", nil, n)
	}
}

// totalAllocMean returns the mean heap allocations (runtime.MemStats.Mallocs)
// of one call of f over runs calls. It is testing.AllocsPerRun — one warm-up
// call, then runs calls at GOMAXPROCS 1 — with two differences: the heap is
// collected first, so that every measurement starts from a like heap and a
// collection inside the window is less likely; and the mean is not truncated
// to an integer, which before subtracting two means could add up to one
// allocation to their difference, the same size as the bounds' margins.
//
// Mallocs is process-wide: anything else that allocates in the window is
// charged to f, spread over runs calls. Like AllocsPerRun it must not run
// during a parallel test; none of the tests that call it is parallel.
func totalAllocMean(runs int, f func()) float64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	runtime.GC()
	f()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return float64(after.Mallocs-before.Mallocs) / float64(runs)
}

// totalAllocSample is a configuration's mean allocations per parse at the two
// fixture sizes.
type totalAllocSample struct {
	small, large float64
}

// measureTotalAlloc measures c at both sizes. Before each size is repeated,
// one parse of it must read the fixture exactly within the safety caps; every
// measured parse, the warm-up included, must too.
func measureTotalAlloc(t *testing.T, label string, c totalAllocCase) totalAllocSample {
	t.Helper()
	at := func(parts, runs int) float64 {
		t.Helper()
		label := fmt.Sprintf("%s, %d parts", label, parts)
		text := totalAllocText(parts)
		var rec totalAllocRecord
		f := func() { totalAllocRun(text, parts, &c, &rec) }

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		rec.check(t, label+", preflight", 1)
		if m, b := after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc; m > totalAllocCapMallocs || b > totalAllocCapBytes {
			t.Fatalf("%s: one parse made %d allocations of %d bytes, over the safety caps %d and %d; not repeating it",
				label, m, b, totalAllocCapMallocs, totalAllocCapBytes)
		}

		mean := totalAllocMean(runs, f)
		rec.check(t, label, 1+1+runs)
		return mean
	}
	return totalAllocSample{
		small: at(totalAllocSmallParts, totalAllocSmallRuns),
		large: at(totalAllocLargeParts, totalAllocLargeRuns),
	}
}

// totalAllocVerdict is what one configuration adds over a baseline: the
// constant, at the small size, and its growth from the small size to the large.
type totalAllocVerdict struct {
	constant, growth float64
}

func compareTotalAlloc(with, without totalAllocSample) totalAllocVerdict {
	constant := with.small - without.small
	return totalAllocVerdict{constant: constant, growth: (with.large - without.large) - constant}
}

func (v totalAllocVerdict) constantOK() bool { return v.constant <= totalAllocMaxConstant }
func (v totalAllocVerdict) growthOK() bool   { return v.growth <= totalAllocMaxGrowth }

func (v totalAllocVerdict) String() string {
	return fmt.Sprintf("constant %.3f (bound %d), growth %.3f (bound %d)", v.constant, totalAllocMaxConstant, v.growth, totalAllocMaxGrowth)
}

// TestParse_totalHeaders_acceptedPathAllocations pins that a parse within the
// totals allocates no more per header block or field than one with them
// disabled, and nothing beyond the shared budget itself and one to spare.
// Every measured parse reads the whole fixture exactly; see totalAllocRun.
func TestParse_totalHeaders_acceptedPathAllocations(t *testing.T) {
	with := measureTotalAlloc(t, "totals on", totalAllocWith())
	without := measureTotalAlloc(t, "totals off", totalAllocWithout())
	v := compareTotalAlloc(with, without)
	// Both measurements have ended; nothing below is in a measured window.
	t.Logf("the totals add %v (mean allocations per parse: with %+v, without %+v)", v, with, without)
	if !v.constantOK() || !v.growthOK() {
		t.Errorf("the totals add %v (with %+v, without %+v)", v, with, without)
	}
}

// TestParse_totalHeaders_acceptedPathAllocationControls runs the same
// measurement and verdict against deliberate allocations of known size, on
// the configuration with the totals off so that the expected difference is
// only what each control adds: one allocation per parse and none must pass;
// three per parse must fail the constant and not the growth; one per part's
// header block, and one per eighth of them, must fail the growth and not the
// constant. The allocations are made by the traversal rather than inside the
// parser, which the process-wide count does not tell apart. Each control's own
// parses are held to the fixture as the measured ones are.
func TestParse_totalHeaders_acceptedPathAllocationControls(t *testing.T) {
	base := measureTotalAlloc(t, "baseline", totalAllocWithout())
	// The nominal difference each control adds, which every one keeps at least
	// one allocation from its bound. It is what the measurement is expected to
	// come close to, not a count it is guaranteed to reproduce: the same
	// process-wide noise as in the measured check applies, with or without
	// -race.
	const s, l = totalAllocSmallParts, totalAllocLargeParts
	for _, ctl := range []struct {
		name                     string
		perParse, everyNth       int
		wantConstant, wantGrowth float64
		constantOK, growthOK     bool
	}{
		{"unchanged", 0, 0, 0, 0, true, true},
		{"one per parse", 1, 0, 1, 0, true, true},
		{"three per parse", 3, 0, 3, 0, false, true},
		{"one per header block", 0, 1, s, l - s, true, false},
		{"one per eighth block", 0, 8, s / 8, l/8 - s/8, true, false},
	} {
		c := totalAllocWithout()
		c.perParse, c.everyNthPart = ctl.perParse, ctl.everyNth
		v := compareTotalAlloc(measureTotalAlloc(t, ctl.name, c), base)
		if v.constantOK() != ctl.constantOK || v.growthOK() != ctl.growthOK {
			t.Errorf("%s: %v, want constant within bound %v and growth within bound %v (adds nominally %v and %v)",
				ctl.name, v, ctl.constantOK, ctl.growthOK, ctl.wantConstant, ctl.wantGrowth)
		}
		// Reported after the control's measurement has ended, for -v.
		t.Logf("%s: %v; adds nominally constant %v, growth %v", ctl.name, v, ctl.wantConstant, ctl.wantGrowth)
	}
}

// headerOfSize returns a header block of exactly size wire bytes, its fields
// within the default per-entity field and line budgets.
func headerOfSize(size int) string {
	var b strings.Builder
	rest := size - 2 // the empty line that ends the block
	for rest > 0 {
		n := min(rest, 60000)
		if rest-n > 0 && rest-n < 8 {
			n -= 8 // never leave a remainder too short to be a field
		}
		b.WriteString("X-P: " + strings.Repeat("p", n-7) + "\r\n")
		rest -= n
	}
	b.WriteString("\r\n")
	return b.String()
}

// TestParse_totalHeaders_realDefaultCount pins the default field total: exactly
// 100 000 candidates across the message read whole, one more refused at that
// candidate.
func TestParse_totalHeaders_realDefaultCount(t *testing.T) {
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	build := func(last int) (string, int64) {
		var b strings.Builder
		b.WriteString(root)
		var lastField int64
		for p := range 100 {
			b.WriteString("--B\r\n")
			n := 1000
			if p == 99 {
				n = last
			}
			for range n {
				lastField = int64(b.Len())
				b.WriteString("X: y\r\n")
			}
			b.WriteString("\r\nb\r\n")
		}
		b.WriteString("--B--\r\n")
		return b.String(), lastField
	}
	// The root's one field plus 99 000 plus 999 is exactly the default.
	text, _ := build(999)
	session, got, err := parseUnder(t, text, Limits{})
	if err != nil || got.err != nil || len(got.bodies) != 100 || session.Diagnostics() != nil {
		t.Fatalf("exact default: (%v, %v, %d bodies)", err, got.err, len(got.bodies))
	}
	text, at := build(1000)
	session, got, err = parseUnder(t, text, Limits{})
	if err != nil || len(got.bodies) != 99 {
		t.Fatalf("default+1: (%v, %d bodies)", err, len(got.bodies))
	}
	checkTotalStop(t, session, got.err, "count", 100000, &totalBlock{path: []int{99}}, at)
}

// partStream generates a multipart message lazily, holding one part at a
// time: the root, then parts whose header blocks total headers bytes in blocks
// of at most a million, each followed by the body "b".
type partStream struct {
	root    string
	rest    int
	buf     string
	started bool
	closed  bool
}

func (s *partStream) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		switch {
		case !s.started:
			s.buf, s.started = s.root, true
		case s.rest > 0:
			size := min(s.rest, 1_000_000)
			s.buf = "--B\r\n" + headerOfSize(size) + "b\r\n"
			s.rest -= size
		case !s.closed:
			s.buf, s.closed = "--B--\r\n", true
		default:
			return 0, io.EOF
		}
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// TestParse_totalHeaders_realDefaultBytes pins the default byte total with a
// lazily generated message of exactly 32 MiB of header blocks, and of one byte
// more, without materializing either message.
func TestParse_totalHeaders_realDefaultBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 32 MiB of headers")
	}
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	for _, extra := range []int{0, 1} {
		headers := 32<<20 - len(root) + extra
		parts := (headers + 999_999) / 1_000_000
		session, e, err := Parse(&partStream{root: root, rest: headers}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := readTree(e)
		if extra == 0 {
			if got.err != nil || len(got.bodies) != parts || session.Diagnostics() != nil {
				t.Fatalf("exact default: (%v, %d of %d bodies)", got.err, len(got.bodies), parts)
			}
			continue
		}
		if len(got.bodies) != parts-1 {
			t.Fatalf("default+1: (%v, %d of %d bodies)", got.err, len(got.bodies), parts)
		}
		// The last block's last byte is the first past the budget.
		lastStart := int64(len(root) + (parts-1)*(len("--B\r\n")+1_000_000+len("b\r\n")) + len("--B\r\n"))
		lastSize := int64(headers - (parts-1)*1_000_000)
		checkTotalStop(t, session, got.err, "bytes", 32<<20, &totalBlock{path: []int{parts - 1}}, lastStart+lastSize-1)
	}
}
