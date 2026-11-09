package textproto

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// A framing line read under MaxLineBytes is accumulated across the reader's
// buffers, and a buffer ends wherever it ends — including between the '\r' and
// the '\n' of a terminator. The '\r' left at the end of a full buffer cannot be
// classified until the next read: it is the start of the terminator when a '\n'
// follows, and an ordinary content byte when anything else, or nothing, does.
// These tests put that '\r' exactly on the buffer edge and pin that it is
// charged only once it is known to be content, the same rule headerScan applies
// inside a header block (ROB-001-003, issue #1).

// edgeContent is the content length of a line whose terminating '\r' is the
// last byte of the k-th full buffer of that line.
func edgeContent(k int) int { return k*peekBufferSize - 1 }

// edgeSources delivers a body every way the reader's buffering could matter:
// whole, with the end of input on the last read, and in chunks that do and do
// not divide the buffer.
func edgeSources(body string) map[string]func() io.Reader {
	sources := map[string]func() io.Reader{
		"whole":         func() io.Reader { return strings.NewReader(body) },
		"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(body)) },
		"one byte":      func() io.Reader { return iotest.OneByteReader(strings.NewReader(body)) },
	}
	for _, k := range []int{7, 4095, 4096, 4097} {
		sources[fmt.Sprintf("chunks of %d", k)] = func() io.Reader {
			return &chunkReader{s: body, n: k}
		}
	}
	return sources
}

// readEdge iterates every part and reads its body, returning the bodies and the
// error that ended the iteration.
func readEdge(t *testing.T, src io.Reader, limit int64) ([]string, error) {
	t.Helper()
	r := NewMultipartReader(src, "A")
	r.SetHeaderLimits(HeaderLimits{MaxLineBytes: limit})
	var bodies []string
	for {
		p, err := r.NextPart()
		if err != nil {
			return bodies, err
		}
		bodies = append(bodies, readPartBody(t, p))
	}
}

// TestMultipartReader_lineBytesCRAtBufferEdge sweeps limit-1, limit and limit+1
// across a framing line whose content is exactly the budget and whose '\r' sits
// on a buffer edge: as the preamble line, after a short line, on the second full
// buffer, and as the delimiter line introducing a later part. The bare-'\n'
// ending, whose content byte rather than its terminator is on the edge, is the
// control.
func TestMultipartReader_lineBytesCRAtBufferEdge(t *testing.T) {
	type layout struct {
		name  string
		f     offsetFixture
		label string // the long line
		n     int64  // its content length
		parts []string
	}
	layouts := []layout{
		{"preamble, first buffer, CRLF", framingFixture("\r\n", edgeContent(1), 10), "preamble", int64(edgeContent(1)), []string{"one", "two"}},
		{"preamble, second buffer, CRLF", framingFixture("\r\n", edgeContent(2), 10), "preamble", int64(edgeContent(2)), []string{"one", "two"}},
		{"preamble, LF on the next buffer", framingFixture("\n", peekBufferSize, 10), "preamble", peekBufferSize, []string{"one", "two"}},
		{"delimiter after a part, CRLF", framingFixture("\r\n", 10, edgeContent(1)), "delimiter", int64(edgeContent(1)), []string{"one", "two"}},
		{"after a short line, CRLF", newOffsetFixture(
			[2]string{"", "x\r\n"},
			[2]string{"long", strings.Repeat("p", edgeContent(1)) + "\r\n"},
			[2]string{"", "--A\r\n\r\none\r\n--A--\r\n"},
		), "long", int64(edgeContent(1)), []string{"one"}},
	}

	for _, l := range layouts {
		for name, src := range edgeSources(l.f.body) {
			for _, delta := range []int64{-1, 0, 1} {
				t.Run(fmt.Sprintf("%s/%s/limit%+d", l.name, name, delta), func(t *testing.T) {
					limit := l.n + delta
					bodies, err := readEdge(t, src(), limit)

					if delta >= 0 {
						if err != io.EOF {
							t.Fatalf("NextPart() = %v, want io.EOF: a line of exactly %d content bytes fits a budget of %d", err, l.n, limit)
						}
						if !equalStrings(bodies, l.parts) {
							t.Errorf("bodies = %v, want %v", bodies, l.parts)
						}
						return
					}

					limitErr, ok := LineLimit(err)
					if !ok {
						t.Fatalf("NextPart() = %v, want a line budget refusal", err)
					}
					if limitErr.Limit() != limit {
						t.Errorf("Limit() = %d, want %d", limitErr.Limit(), limit)
					}
					if want := l.f.at[l.label]; limitErr.Offset() != want {
						t.Errorf("Offset() = %d, want %d: the first byte of the refused line", limitErr.Offset(), want)
					}
					if !errors.Is(err, ErrLineTooBig) {
						t.Errorf("err = %v, want it to wrap ErrLineTooBig", err)
					}
				})
			}
		}
	}
}

// TestMultipartReader_lineBytesCRAtBufferEdgeIsContent pins the other half: a
// '\r' on the edge that is not followed by '\n' was content all along and is
// charged, whether a byte, another '\r' that does begin the terminator, or the
// end of the input follows it.
func TestMultipartReader_lineBytesCRAtBufferEdgeIsContent(t *testing.T) {
	edge := strings.Repeat("a", edgeContent(1)) + "\r"
	tail := "--A\r\n\r\none\r\n--A--\r\n"

	for _, tc := range []struct {
		name string
		body string
		n    int64 // content bytes of the first line
		eof  bool  // the line is ended by the end of input rather than a terminator
	}{
		// "a"*4095 "\r" "x" "\r\n": the edge '\r' and the 'x' are both content.
		{"a byte follows", edge + "x\r\n" + tail, int64(edgeContent(1)) + 2, false},
		// "a"*4094 "\r" "\r" "\n": the first '\r' is content, the one on the edge
		// begins the terminator.
		{"another CR follows", strings.Repeat("a", edgeContent(1)-1) + "\r\r\n" + tail, int64(edgeContent(1)), false},
		// "a"*4095 "\r" then EOF: nothing follows, so the '\r' is content.
		{"the input ends", edge, int64(edgeContent(1)) + 1, true},
	} {
		for name, src := range edgeSources(tc.body) {
			for _, delta := range []int64{-1, 0} {
				t.Run(fmt.Sprintf("%s/%s/limit%+d", tc.name, name, delta), func(t *testing.T) {
					limit := tc.n + delta
					bodies, err := readEdge(t, src(), limit)

					limitErr, refused := LineLimit(err)
					if delta < 0 {
						if !refused {
							t.Fatalf("NextPart() = %v, want a line budget refusal at %d content bytes over a budget of %d", err, tc.n, limit)
						}
						if limitErr.Offset() != 0 {
							t.Errorf("Offset() = %d, want 0", limitErr.Offset())
						}
						return
					}
					if refused {
						t.Fatalf("NextPart() = %v, want the line of %d content bytes admitted under %d", err, tc.n, limit)
					}
					if tc.eof {
						// The line was admitted and the input simply ended, which is
						// the ordinary read failure it has always been.
						if !errors.Is(err, io.EOF) {
							t.Errorf("NextPart() = %v, want an EOF-class error", err)
						}
						return
					}
					if err != io.EOF || !equalStrings(bodies, []string{"one"}) {
						t.Errorf("NextPart() = (%v, %v), want ([one], io.EOF)", bodies, err)
					}
				})
			}
		}
	}
}
