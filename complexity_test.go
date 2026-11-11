package message

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit/internal/scaletest"
	"github.com/saeroun-ai/emlkit/textproto"
)

// Complexity tests (ROB-060-013, ROB-120-002): parsing and decoding cost stays
// proportional to the size of the input, for ordinary mail and for the shapes
// built to make a scanner look at the same bytes again — lines that nearly
// match a delimiter, long lines, damaged header lines, undeclared boundaries,
// header values that open a construct and never close it.
//
// Each case is run at one size and at four times that size by
// internal/scaletest, which says what is compared and how. The small-read
// variants deliver the same input one byte and sixteen bytes at a time: the
// quadratic that was found in the header scanner (textproto's nextChunk) only
// showed when a long line arrived through a small buffer.
//
// docs/robustness-notes/performance.md lists what these tests hold, and what
// costs more than its size and is bounded by a budget instead.

// pfDelivery is one way an input reaches the parser. div scales the size of
// the case down, since a small read costs a call of every layer per byte.
type pfDelivery struct {
	name string
	wrap func(io.Reader) io.Reader
	div  int
}

var pfDeliveries = []pfDelivery{
	{"whole", func(r io.Reader) io.Reader { return r }, 1},
	{"16-byte", func(r io.Reader) io.Reader { return scaletest.ChunkReader(r, 16) }, 8},
	{"1-byte", iotest.OneByteReader, 32},
}

// pfScaled returns the smaller size of a case: n, or less under the race
// detector, where only what is counted is compared and every byte costs more.
func pfScaled(n int) int {
	if scaletest.RaceEnabled {
		return n / 4
	}
	return n
}

// TestComplexityParse holds Parse and Entity.Drain to linear cost on every
// shape of bench_parse_test.go that scales, however the input is delivered.
func TestComplexityParse(t *testing.T) {
	for i := range pfShapes {
		sh := &pfShapes[i]
		if sh.scale == 0 {
			continue
		}
		for _, d := range pfDeliveries {
			if sh.slow && d.div > 1 {
				continue
			}
			t.Run(sh.name+"/"+d.name, func(t *testing.T) {
				n := sh.scale / d.div
				if d.div == 1 {
					n = pfScaled(n)
				}
				scaletest.Check(t, sh.build(n), sh.build(4*n), func(input []byte) {
					pfRun(t, sh, d.wrap(bytes.NewReader(input)))
				})
			})
		}
	}
}

// pfWireHeaders are header blocks for the wire layer, which holds them to no
// budget unless asked: here a line or a field may be as long as the case
// makes it.
var pfWireHeaders = []struct {
	name    string
	recover bool
	build   func(n int) []byte
}{
	{"long-line", false, func(n int) []byte {
		return append(append([]byte("X-Long: "), pfRepeat("one line with no fold in it ", n)...), "\r\n\r\n"...)
	}},
	{"long-folded", false, func(n int) []byte {
		return append(append([]byte("X-Folded: first\r\n"), pfRepeat("\tcontinuation line of a very long folded field\r\n", n)...), "\r\n"...)
	}},
	{"many-fields", false, func(n int) []byte {
		return append(pfRepeat("X-Field: an ordinary value\r\n", n), "\r\n"...)
	}},
	{"bare-cr-fields", false, func(n int) []byte {
		return append(pfRepeat("X-Field: a field ended by a bare CR\r", n), "\r\n"...)
	}},
	{"cr-inside-value", false, func(n int) []byte {
		// Each '\r' is followed by bytes that could begin a field name and
		// then do not: it is content, and the line goes on.
		return append(append([]byte("X-Stray: "), pfRepeat("stray\rcarriage returns in one long value ", n)...), "\r\n\r\n"...)
	}},
	{"white-space-lines", false, func(n int) []byte {
		return append(append([]byte("X-Blank: first\r\n"), pfRepeat(" \t \r\n", n)...), "\r\n"...)
	}},
	{"malformed-prefix", false, func(n int) []byte {
		return append(pfRepeat(" a first line that begins with white space\r\n", n), "X-After: the prefix\r\n\r\n"...)
	}},
	{"damaged-lines", true, func(n int) []byte {
		unit := strings.Repeat("a damaged header line with no colon in it\r\n", 7) + "X-Good: the block goes on\r\n"
		return append(pfRepeat(unit, n), "\r\n"...)
	}},
	{"unindented-continuations", true, func(n int) []byte {
		unit := "Content-Type: text/plain;\r\ncharset=utf-8;\r\nname=value\r\nTo: a@example.com,\r\nb@example.com,\r\nc@example.com\r\n"
		return append(pfRepeat(unit, n), "\r\n"...)
	}},
}

// TestComplexityWireHeader holds textproto's header reader to linear cost,
// through a reader buffer of 4096 bytes and through a source that delivers a
// byte at a time: into the smallest buffer there is, or, for the cases that
// recover damaged lines, into one of 4096 bytes, since recovery looks no
// further than one buffer and ends the block at the first damaged line when
// that shows it nothing.
func TestComplexityWireHeader(t *testing.T) {
	for _, c := range pfWireHeaders {
		small := 16
		if c.recover {
			small = 4096
		}
		for _, d := range []struct {
			name    string
			size    int
			oneByte bool
			n       int
		}{
			{"buffer-4096", 4096, false, 256 << 10},
			{fmt.Sprintf("buffer-%d/1-byte", small), small, true, 16 << 10},
		} {
			t.Run(c.name+"/"+d.name, func(t *testing.T) {
				n := pfScaled(d.n)
				scaletest.Check(t, c.build(n), c.build(4*n), func(input []byte) {
					var src io.Reader = bytes.NewReader(input)
					if d.oneByte {
						src = iotest.OneByteReader(src)
					}
					br := bufio.NewReaderSize(src, d.size)
					var h textproto.Header
					var err error
					if c.recover {
						h, _, err = textproto.ReadHeaderRecover(br, textproto.HeaderLimits{})
					} else {
						h, err = textproto.ReadHeader(br)
					}
					if err != nil {
						t.Fatalf("header not read: %v", err)
					}
					// The block is the whole input: a reader that stopped
					// early measured nothing.
					if rest, _ := io.Copy(io.Discard, br); rest != 0 {
						t.Fatalf("%d of %d bytes were left unread (%d fields)", rest, len(input), h.Len())
					}
				})
			})
		}
	}
}

// TestComplexityWireMultipart holds textproto's multipart reader to linear
// cost on the near-match bodies, read whole and a byte at a time, with and
// without the recovery and line budget Parse reads under, and on a preamble
// of the same lines.
func TestComplexityWireMultipart(t *testing.T) {
	limits := textproto.HeaderLimits{Recover: true, MaxLineBytes: 64 << 10}
	for i := range pfShapes {
		sh := &pfShapes[i]
		if sh.boundary == "" {
			continue
		}
		body := func(n int) []byte {
			msg := sh.build(n)
			return msg[bytes.Index(msg, []byte("\r\n\r\n"))+4:]
		}
		preamble := func(n int) []byte {
			// The same lines with no delimiter line before them, where they
			// are read as framing lines and kept as the preamble.
			return bytes.TrimPrefix(body(n), []byte("--"+sh.boundary+"\r\n"))
		}
		for _, d := range []struct {
			name    string
			limited bool
			small   bool
			build   func(n int) []byte
			n       int
		}{
			{"body/whole", true, false, body, 256 << 10},
			{"body/unlimited", false, false, body, 256 << 10},
			{"body/1-byte", true, true, body, 16 << 10},
			{"preamble/whole", true, false, preamble, 128 << 10},
			{"preamble/1-byte", true, true, preamble, 16 << 10},
		} {
			t.Run(sh.name+"/"+d.name, func(t *testing.T) {
				n := pfScaled(d.n)
				scaletest.Check(t, d.build(n), d.build(4*n), func(input []byte) {
					var src io.Reader = bytes.NewReader(input)
					buf := make([]byte, 32<<10)
					if d.small {
						src = iotest.OneByteReader(src)
						buf = buf[:1]
					}
					mr := textproto.NewMultipartReader(src, sh.boundary)
					if d.limited {
						mr.SetHeaderLimits(limits)
					}
					pfReadParts(mr, buf)
				})
			})
		}
	}
}

// TestComplexityHeaderValues holds the parsers of one header value to linear
// cost on values that open, nest or leave unfinished one construct as often
// as their length allows. A field may be 64 KiB under the default budgets:
// quadratic work on one would be four thousand million steps.
func TestComplexityHeaderValues(t *testing.T) {
	parsers := []struct {
		name  string
		parse func(v string)
	}{
		{"media-type", func(v string) { ParseMediaTypeTolerant("text/plain" + v) }},
		{"disposition", func(v string) { ParseContentDispositionTolerant("attachment" + v) }},
		{"parameter-candidates", func(v string) { ParseParameterCandidates("Content-Type", "text/plain"+v) }},
		{"header-text", func(v string) { DecodeHeaderText(v) }},
		{"transfer-encoding", func(v string) { canonicalCTE(v) }},
		{"safe-filename", func(v string) { SafeFilename(v) }},
	}
	// The same sizes under the race detector: below them a field of one
	// parameter repeated allocates faster than it grows, up to a cap (see
	// performance.md), which is not what this test is about.
	const n = 2 << 10
	for _, p := range parsers {
		for _, c := range scaletest.ValueCases() {
			t.Run(p.name+"/"+c.Name, func(t *testing.T) {
				scaletest.Check(t, c.Build(n), c.Build(4*n), p.parse)
			})
		}
	}
}

// pfReadParts reads every part of mr to its end through buf and returns how
// many bytes the parts delivered.
func pfReadParts(mr *textproto.MultipartReader, buf []byte) (read int64) {
	for {
		p, err := mr.NextPart()
		if err != nil {
			return read
		}
		for {
			n, err := p.Read(buf)
			read += int64(n)
			if err != nil {
				break
			}
		}
	}
}

// TestDelimiterScanDashRun holds the delimiter scan over runs of dashes that
// a boundary of dashes matches at every byte to the cost of other near
// matches. Both are linear in the body, which TestComplexityWireMultipart
// holds them to; this one is about the factor, which was the length of the
// boundary: the whole name used to be compared at every byte of a run, and
// such a body was read seventeen times slower than a near match that fails at
// its last byte.
func TestDelimiterScanDashRun(t *testing.T) {
	if !scaletest.Timed() {
		t.Skip("compares processor time")
	}
	const size = 1 << 20
	body := func(boundary, unit string) []byte {
		return append(append([]byte("--"+boundary+"\r\n\r\n"), pfRepeat(unit, size)...), "\r\n--"+boundary+"--\r\n"...)
	}
	dashes := strings.Repeat("-", 70)
	run, near := body(dashes, strings.Repeat("-", 200)+"\r\n"), body(pfBoundary, "--"+pfBoundary[:len(pfBoundary)-1]+"\r\n")
	buf := make([]byte, 32<<10)
	cost := func(boundary string, input []byte) int64 {
		c := scaletest.Measure(1, func() {
			if read := pfReadParts(textproto.NewMultipartReader(bytes.NewReader(input), boundary), buf); read < size {
				t.Fatalf("the parts delivered %d bytes of %d", read, len(input))
			}
		})
		return int64(c.CPU)
	}
	var bestRun, bestNear int64
	for attempt := 0; attempt < 6; attempt++ {
		if c := cost(dashes, run); bestRun == 0 || c < bestRun {
			bestRun = c
		}
		if c := cost(pfBoundary, near); bestNear == 0 || c < bestNear {
			bestNear = c
		}
		if bestNear > 0 && bestRun <= 8*bestNear {
			t.Logf("runs of dashes: %dµs, near matches: %dµs", bestRun/1000, bestNear/1000)
			return
		}
	}
	t.Errorf("a body of runs of dashes cost %dµs and one of near matches %dµs: more than 8 times as much", bestRun/1000, bestNear/1000)
}
