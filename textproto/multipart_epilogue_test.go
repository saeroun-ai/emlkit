package textproto

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// MultipartReader.Epilogue (ROB-010-005): after a proven closing delimiter, and
// only then, the input that follows it is offered verbatim, with its offset.
// Every source and loop here is finite; reads are capped and fail visibly.

// epilogueCap bounds what readEpilogueCapped reads; a fixture longer than it
// fails the test rather than being cut short silently.
const epilogueCap = 64 << 10

// readEpilogueCapped reads r to its end, at most epilogueCap bytes and 4096
// reads, in reads of size bytes, and returns what it read and the error that
// ended it.
func readEpilogueCapped(t *testing.T, r io.Reader, size int) (string, error) {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, size)
	for i := 0; i < 4096+epilogueCap; i++ {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if b.Len() > epilogueCap {
			t.Fatalf("epilogue longer than the %d-byte test cap", epilogueCap)
		}
		if err != nil {
			return b.String(), err
		}
	}
	t.Fatalf("epilogue read did not end within the test's read cap")
	return "", nil
}

// iterateToEnd calls NextPart until it fails, reading each part's body whole
// when readBodies is set, at most 64 parts.
func iterateToEnd(t *testing.T, mr *MultipartReader, readBodies bool) error {
	t.Helper()
	for i := 0; i < 64; i++ {
		p, err := mr.NextPart()
		if err != nil {
			return err
		}
		if readBodies {
			if _, err := readEpilogueCapped(t, p, 7); err != io.EOF && err != nil && !errors.Is(err, ErrMissingBoundaryClose) {
				return err
			}
		}
	}
	t.Fatalf("more than 64 parts")
	return nil
}

func epilogueDeliveries(s string) map[string]func() io.Reader {
	return map[string]func() io.Reader{
		"whole":    func() io.Reader { return strings.NewReader(s) },
		"chunks 1": func() io.Reader { return &chunkReader{s: s, n: 1} },
		"chunks 7": func() io.Reader { return &chunkReader{s: s, n: 7} },
		"random 1": func() io.Reader { return newRandomChunkReader(s, 13, 1) },
		"random 2": func() io.Reader { return newRandomChunkReader(s, 5000, 2) },
		"buffered": func() io.Reader { return bufio.NewReaderSize(strings.NewReader(s), 16) },
	}
}

func TestMultipartReader_epilogue(t *testing.T) {
	big := strings.Repeat("0123456789abcdef\r\n", 600) // crosses the 4 KiB buffer
	cases := []struct {
		name     string
		prefix   string // everything through the closing delimiter's line ending
		epilogue string
	}{
		{"crlf", "--B\r\nA: 1\r\n\r\nbody\r\n--B--\r\n", "epilogue\r\n"},
		{"lf", "--B\nA: 1\n\nbody\n--B--\n", "epilogue\n"},
		{"bare cr", "--B\rA: 1\r\rbody\r--B--\r", "epilogue\r"},
		{"crlf longest", "--B\r\n\r\nx\r\n--B--\r\n", "\nafter"},
		{"cr then cr", "--B\r\n\r\nx\r\n--B--\r", "\rafter"},
		{"padding", "--B\r\n\r\nx\r\n--B-- \t \r\n", "epi"},
		{"empty", "--B\r\n\r\nx\r\n--B--\r\n", ""},
		{"at eof", "--B\r\n\r\nx\r\n--B--", ""},
		{"at eof padded", "--B\r\n\r\nx\r\n--B--  ", ""},
		{"cr at eof", "--B\r\n\r\nx\r\n--B--\r", ""},
		{"empty part", "--B\r\n--B--\r\n", "epi"},
		{"preamble only", "preamble\r\n--B--\r\n", "epi\r\n"},
		{"first line", "--B--\r\n", "epi"},
		{"first line at eof", "--B--", ""},
		{"first line bare cr", "x\r--B--\r", "y"},
		{"delimiters after", "--B\r\n\r\nx\r\n--B--\r\n", "--B\r\n\r\ny\r\n--B--\r\nz"},
		{"message after", "--B\r\n\r\nx\r\n--B--\r\n", "From: a@example.com\r\nTo: b@example.com\r\n\r\nhi\r\n"},
		{"binary after", "--B\r\n\r\nx\r\n--B--\r\n", "\x00\xff\xfe\r\x00"},
		{"big", "--B\r\n\r\n" + big + "\r\n--B--\r\n", big},
	}
	for _, tc := range cases {
		input := tc.prefix + tc.epilogue
		for dname, src := range epilogueDeliveries(input) {
			for _, readBodies := range []bool{true, false} {
				for _, size := range []int{1, 3, 4096, 64 << 10} {
					mr := NewMultipartReader(src(), "B")
					if _, _, ok := mr.Epilogue(); ok {
						t.Fatalf("%s/%s: Epilogue before the iteration began", tc.name, dname)
					}
					if err := iterateToEnd(t, mr, readBodies); err != io.EOF {
						t.Fatalf("%s/%s: iteration ended with %v, want io.EOF", tc.name, dname, err)
					}
					rd, off, ok := mr.Epilogue()
					if !ok || rd == nil {
						t.Fatalf("%s/%s: no epilogue after the closing delimiter", tc.name, dname)
					}
					if off != int64(len(tc.prefix)) {
						t.Errorf("%s/%s: offset %d, want %d", tc.name, dname, off, len(tc.prefix))
					}
					rd2, off2, _ := mr.Epilogue()
					if rd2 != rd || off2 != off {
						t.Errorf("%s/%s: a second call returned another reader or offset", tc.name, dname)
					}
					if _, ok := rd.(io.WriterTo); ok {
						t.Errorf("%s/%s: the epilogue reader exposes WriteTo", tc.name, dname)
					}
					got, err := readEpilogueCapped(t, rd, size)
					if err != io.EOF || got != tc.epilogue {
						t.Errorf("%s/%s/%v/%d: epilogue %q, %v; want %q, io.EOF", tc.name, dname, readBodies, size, got, err, tc.epilogue)
					}
					if _, err := mr.NextPart(); err != io.EOF {
						t.Errorf("%s/%s: NextPart after the epilogue = %v, want io.EOF", tc.name, dname, err)
					}
				}
			}
		}
	}
}

// TestMultipartReader_noEpilogue pins every end of an iteration that is not a
// proven closing delimiter: no epilogue, however often NextPart is asked.
func TestMultipartReader_noEpilogue(t *testing.T) {
	cases := map[string]string{
		"missing close":     "--B\r\n\r\nbody\r\n",
		"truncated":         "--B\r\n\r\nbody\r\n--B",
		"truncated padded":  "--B\r\n\r\nbody\r\n--B  ",
		"close with suffix": "--B\r\n\r\nbody\r\n--B--x\r\nmore",
		"close not at line": "--B\r\n\r\nbody --B--\r\n",
		"empty body":        "",
		"preamble only":     "just text\r\n",
		"half close":        "--B\r\n\r\nbody\r\n--B-",
	}
	for name, input := range cases {
		for dname, src := range epilogueDeliveries(input) {
			mr := NewMultipartReader(src(), "B")
			err := iterateToEnd(t, mr, true)
			if err == nil {
				t.Fatalf("%s/%s: iteration did not end", name, dname)
			}
			for i := 0; i < 3; i++ {
				if rd, off, ok := mr.Epilogue(); ok || rd != nil || off != 0 {
					t.Errorf("%s/%s: Epilogue = %v, %d, %v after %v; want none", name, dname, rd, off, ok, err)
				}
				mr.NextPart()
			}
		}
	}

	// A framing line past its budget is a refusal, not a close.
	mr := NewMultipartReader(strings.NewReader(strings.Repeat("p", 64)+"\r\n--B--\r\nepi"), "B")
	mr.SetHeaderLimits(HeaderLimits{MaxLineBytes: 8})
	if err := iterateToEnd(t, mr, true); err == nil || err == io.EOF {
		t.Fatalf("refused line: iteration ended with %v", err)
	}
	if _, _, ok := mr.Epilogue(); ok {
		t.Errorf("refused line: Epilogue offered")
	}
}

// TestMultipartReader_epilogueSourceError cuts the input at every byte after
// the closing delimiter: the epilogue delivers exactly the bytes before the cut
// and then the identical error, and the input is not read after it.
func TestMultipartReader_epilogueSourceError(t *testing.T) {
	prefix := "--B\r\n\r\nbody\r\n--B--\r\n"
	input := prefix + "epilogue line\r\nmore\r\n"
	for cut := len(prefix); cut <= len(input); cut++ {
		for _, chunk := range []int{1, 7, 4096} {
			for _, withData := range []bool{true, false} {
				for _, sticky := range []bool{true, false} {
					src := &failAtSource{data: input, cut: cut, chunk: chunk, err: errEIO, withData: withData, sticky: sticky}
					mr := NewMultipartReader(src, "B")
					if err := iterateToEnd(t, mr, true); err != io.EOF {
						t.Fatalf("cut %d: iteration ended with %v", cut, err)
					}
					rd, off, ok := mr.Epilogue()
					if !ok || off != int64(len(prefix)) {
						t.Fatalf("cut %d: Epilogue = %v, %d", cut, ok, off)
					}
					got, err := readEpilogueCapped(t, rd, 5)
					if got != input[len(prefix):cut] || err != errEIO {
						t.Errorf("cut %d chunk %d data %v sticky %v: %q, %v; want %q, the identical error", cut, chunk, withData, sticky, got, err, input[len(prefix):cut])
					}
					if _, err := rd.Read(make([]byte, 8)); err != errEIO {
						t.Errorf("cut %d: a later read returned %v, want the identical error", cut, err)
					}
					if src.readsAfter != 0 {
						t.Errorf("cut %d: the input was read %d times after its failure", cut, src.readsAfter)
					}
				}
			}
		}
	}
}

// TestMultipartReader_epilogueNested reads the epilogue of a multipart nested
// in a part of another: it ends where that part ends, and the enclosing
// iteration goes on to its next part undisturbed.
func TestMultipartReader_epilogueNested(t *testing.T) {
	outer := "--O\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n" +
		"--I\r\n\r\nx\r\n--I--\r\ninner epilogue\r\n--O\r\n\r\nsecond\r\n--O--\r\nouter epilogue"
	for dname, src := range epilogueDeliveries(outer) {
		omr := NewMultipartReader(src(), "O")
		p, err := omr.NextPart()
		if err != nil {
			t.Fatalf("%s: first outer part: %v", dname, err)
		}
		imr := NewMultipartReader(p, "I")
		if err := iterateToEnd(t, imr, true); err != io.EOF {
			t.Fatalf("%s: inner iteration ended with %v", dname, err)
		}
		rd, _, ok := imr.Epilogue()
		if !ok {
			t.Fatalf("%s: no inner epilogue", dname)
		}
		if got, err := readEpilogueCapped(t, rd, 3); got != "inner epilogue" || err != io.EOF {
			t.Errorf("%s: inner epilogue %q, %v", dname, got, err)
		}
		p2, err := omr.NextPart()
		if err != nil {
			t.Fatalf("%s: second outer part: %v", dname, err)
		}
		if got, err := readEpilogueCapped(t, p2, 3); got != "second" || err != io.EOF {
			t.Errorf("%s: second part %q, %v", dname, got, err)
		}
		if _, err := omr.NextPart(); err != io.EOF {
			t.Fatalf("%s: outer iteration ended with %v", dname, err)
		}
		rd, _, _ = omr.Epilogue()
		if got, err := readEpilogueCapped(t, rd, 4096); got != "outer epilogue" || err != io.EOF {
			t.Errorf("%s: outer epilogue %q, %v", dname, got, err)
		}
	}
}

// countingSource counts its reads.
type countingSource struct {
	r     io.Reader
	reads int
}

func (c *countingSource) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

// TestMultipartReader_epilogueReadsNothing pins that neither Epilogue nor an
// empty read of its reader reads the input, and that NextPart does not read
// past the closing delimiter's line for it.
func TestMultipartReader_epilogueReadsNothing(t *testing.T) {
	prefix := "--B\r\n\r\nx\r\n--B--\r\n"
	src := &countingSource{r: &chunkReader{s: prefix + "tail", n: 1}}
	mr := NewMultipartReader(src, "B")
	if err := iterateToEnd(t, mr, true); err != io.EOF {
		t.Fatalf("iteration ended with %v", err)
	}
	before := src.reads
	rd, _, _ := mr.Epilogue()
	if n, err := rd.Read(nil); n != 0 || err != nil {
		t.Errorf("empty read = %d, %v", n, err)
	}
	mr.Epilogue()
	if src.reads != before {
		t.Errorf("Epilogue and an empty read read the input %d times", src.reads-before)
	}
	if got, err := readEpilogueCapped(t, rd, 1); got != "tail" || err != io.EOF {
		t.Errorf("epilogue %q, %v", got, err)
	}
}
