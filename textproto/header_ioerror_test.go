package textproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Read failures inside a header block (ROB-010-006). bufio.Reader hands a read
// error out once and forgets it, so a scanner that looked, failed, and read
// again could lose a one-shot failure, or read what a source sent after it.
// Every source here is finite and counts its calls; none is ever read past its
// cap.

var errEIO = errors.New("test source: input/output error")

var errFailAtCap = errors.New("test source: call cap exceeded")

// A failAtSource delivers data in reads of at most chunk bytes until cut, and
// then fails with err: together with the last bytes before cut when withData,
// on a read of its own otherwise. A sticky source keeps failing; a resuming one
// fails once and then delivers the rest of data and io.EOF, as io.Reader allows.
// readsAfter counts the reads made after the failure was returned.
type failAtSource struct {
	data             string
	cut, chunk       int
	err              error
	withData, sticky bool

	pos, calls, readsAfter int
	failed                 bool
}

func (s *failAtSource) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > 4*len(s.data)+64 {
		return 0, errFailAtCap
	}
	if s.failed {
		s.readsAfter++
		if s.sticky {
			return 0, s.err
		}
		if s.pos >= len(s.data) {
			return 0, io.EOF
		}
		n := copy(p, s.data[s.pos:min(len(s.data), s.pos+s.chunk)])
		s.pos += n
		return n, nil
	}
	if s.pos >= s.cut {
		s.failed = true
		return 0, s.err
	}
	n := copy(p, s.data[s.pos:min(s.cut, s.pos+s.chunk)])
	s.pos += n
	if s.pos == s.cut && s.withData {
		s.failed = true
		return n, s.err
	}
	return n, nil
}

// wholeLines returns what a header block keeps when its input fails after its
// first cut bytes: the lines of it that ended before the cut, up to the first
// that did not, and whether the empty line ending the block was among them.
// It is computed from the text alone.
func wholeLines(block string, cut int) (kept string, complete bool) {
	pos := 0
	for {
		i := strings.IndexByte(block[pos:], '\n')
		if i < 0 || pos+i+1 > cut {
			return kept, false
		}
		line := block[pos : pos+i+1]
		if line == "\r\n" {
			return kept, true
		}
		kept += line
		pos += i + 1
	}
}

func writtenHeader(t *testing.T, h Header) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteHeader(&b, h); err != nil {
		t.Fatalf("WriteHeader() = %v", err)
	}
	return b.String()
}

// TestReadHeader_readFailure cuts a block at every byte, through the long
// line, the continuation and the empty line, at buffer sizes 16 and 4096 and
// three chunkings, with the failure delivered with the last bytes or after
// them, from sources that keep failing and from sources that resume. A failure
// other than io.EOF is returned as itself with every field read whole before it,
// byte for byte, and nothing after it is read; a block whose empty line ended
// before it is read whole, and its body yields the bytes before the failure and
// then the failure. io.EOF behaves as it does from a source that keeps
// returning it.
func TestReadHeader_readFailure(t *testing.T) {
	const block = "A: 1\r\nLong: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\r\nB: 2\r\n\tcont\r\nC: 3\r\n\r\n"
	const text = block + "body text"
	read := map[string]func(*bufio.Reader) (Header, error){
		"ReadHeader":       ReadHeader,
		"ReadHeaderLimits": func(r *bufio.Reader) (Header, error) { return ReadHeaderLimits(r, HeaderLimits{MaxBytes: 1 << 20}) },
	}
	for api, readFn := range read {
		for cut := 0; cut <= len(text); cut++ {
			for _, size := range []int{16, 4096} {
				for _, chunk := range []int{1, 7, 4096} {
					for _, withData := range []bool{false, true} {
						for _, fail := range []error{errEIO, io.EOF} {
							results := map[bool]string{}
							for _, sticky := range []bool{true, false} {
								name := fmt.Sprintf("%s/cut %d/buf %d/chunk %d/with %v/%v/sticky %v", api, cut, size, chunk, withData, fail, sticky)
								src := &failAtSource{data: text, cut: cut, chunk: chunk, err: fail, withData: withData, sticky: sticky}
								br := bufio.NewReaderSize(src, size)
								h, err := readFn(br)
								if src.readsAfter != 0 {
									t.Errorf("%s: %d reads after the failure", name, src.readsAfter)
								}
								results[sticky] = fmt.Sprintf("%q %v", writtenHeader(t, h), err)
								if fail != errEIO {
									continue
								}
								kept, complete := wholeLines(text, cut)
								if complete {
									if err != nil || writtenHeader(t, h) != block {
										t.Errorf("%s: (%q, %v), want the whole block", name, writtenHeader(t, h), err)
										continue
									}
									body, berr := io.ReadAll(br)
									if want := text[len(block):max(cut, len(block))]; string(body) != want || berr != errEIO {
										t.Errorf("%s: body (%q, %v), want (%q, the failure)", name, body, berr, want)
									}
									continue
								}
								if err != errEIO {
									t.Errorf("%s: err = %v, want the input's own failure", name, err)
								}
								if got := writtenHeader(t, h); got != kept+"\r\n" {
									t.Errorf("%s: kept %q, want the whole lines %q", name, got, kept+"\r\n")
								}
							}
							if results[true] != results[false] {
								t.Errorf("%s/cut %d/buf %d/chunk %d/with %v/%v: a resuming source gave %s, a failing one %s",
									api, cut, size, chunk, withData, fail, results[false], results[true])
							}
						}
					}
				}
			}
		}
	}
}

// TestReadHeader_readFailurePoints pins the look-ahead points by name: a whole
// field whose next byte could not be looked at, a continuation line the failure
// cut, a terminator split between '\r' and '\n', and the empty line that ends
// the block.
func TestReadHeader_readFailurePoints(t *testing.T) {
	const text = "A: 1\r\nB: 2\r\n\tcont\r\n\r\nbody"
	for _, tc := range []struct {
		name string
		cut  int
		want string
	}{
		{"before the first byte", 0, "\r\n"},
		{"a whole field before a failed look-ahead", len("A: 1\r\n"), "A: 1\r\n\r\n"},
		{"inside a field's terminator", len("A: 1\r"), "\r\n"},
		{"a continuation line cut", len("A: 1\r\nB: 2\r\n\tco"), "A: 1\r\nB: 2\r\n\r\n"},
		{"a whole continuation before a failed look-ahead", len("A: 1\r\nB: 2\r\n\tcont\r\n"), "A: 1\r\nB: 2\r\n\tcont\r\n\r\n"},
		{"inside the empty line", len("A: 1\r\nB: 2\r\n\tcont\r\n\r"), "A: 1\r\nB: 2\r\n\tcont\r\n\r\n"},
	} {
		for _, sticky := range []bool{true, false} {
			src := &failAtSource{data: text, cut: tc.cut, chunk: 4096, err: errEIO, sticky: sticky}
			h, err := ReadHeader(bufio.NewReader(src))
			if err != errEIO || writtenHeader(t, h) != tc.want || src.readsAfter != 0 {
				t.Errorf("%s/sticky %v: (%q, %v) after %d reads past the failure, want (%q, the failure) and none",
					tc.name, sticky, writtenHeader(t, h), err, src.readsAfter, tc.want)
			}
		}
	}
	// A cut that leaves no colon is the input's failure, not a malformed line.
	src := &failAtSource{data: "Subject: x\r\n\r\n", cut: 4, chunk: 4096, err: errEIO}
	if _, err := ReadHeader(bufio.NewReader(src)); err != errEIO {
		t.Errorf("a line cut before its colon: err = %v, want the input's failure", err)
	}
}

// TestReadHeader_readFailureBudgetFirst pins that bytes already read can still
// exceed a budget, which is then what is reported, and that the failure is
// otherwise returned unchanged under every budget.
func TestReadHeader_readFailureBudgetFirst(t *testing.T) {
	const text = "A: 1\r\nLong: xxxxxxxxxxxxxxxxxxxx\r\n\r\n"
	cut := strings.Index(text, "Long") + 15
	for _, size := range []int{16, 4096} {
		src := &failAtSource{data: text, cut: cut, chunk: 4096, err: errEIO, sticky: true}
		_, err := ReadHeaderLimits(bufio.NewReaderSize(src, size), HeaderLimits{MaxLineBytes: 10})
		if limit, refused := HeaderLimit(err); !refused || limit.Kind() != HeaderLimitLineBytes {
			t.Errorf("buf %d: err = %v, want the line budget the bytes before the failure exceeded", size, err)
		}
		src = &failAtSource{data: text, cut: cut, chunk: 4096, err: errEIO, sticky: true}
		h, err := ReadHeaderLimits(bufio.NewReaderSize(src, size), HeaderLimits{MaxLineBytes: 100, MaxBytes: 100, MaxFieldBytes: 100, MaxCount: 10})
		if err != errEIO || h.Get("A") != "1" || h.Has("Long") {
			t.Errorf("buf %d: (%v, %v), want field A and the failure", size, h.Map(), err)
		}
	}
}

const bufferFullEnv = "EMLKIT_TEXTPROTO_BUFFER_FULL"

// TestReadHeader_sourceBufferFull pins a source that fails with
// bufio.ErrBufferFull itself — the value ReadSlice also uses for a line longer
// than the buffer. Before the scanners told the two apart they read on with no
// bytes for ever, without calling the source again, so no call cap could stop
// them: every case runs in a subprocess with a deadline.
//
// The two are told apart by the bytes ReadSlice returns with the value: the
// whole buffer is a long line, anything less the source's failure. A source
// failing so after exactly filling the buffer is the one case bufio cannot tell
// apart; that buffer is read on, and the next read decides. So a source that
// keeps failing gives the exact result after at most one more read, and a
// resuming one gives it whenever it was not read again — and otherwise only has
// to end.
func TestReadHeader_sourceBufferFull(t *testing.T) {
	if os.Getenv(bufferFullEnv) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadHeader_sourceBufferFull$", "-test.count=1")
		cmd.Env = append(os.Environ(), bufferFullEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("subprocess = %v, deadline %v\n%s", err, ctx.Err(), out)
		}
		return
	}

	const block = "A: 1\r\nLong: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\r\nB: 2\r\n\tcont\r\nC: 3\r\n\r\n"
	const text = block + "body text"
	read := map[string]func(*bufio.Reader) (Header, error){
		"ReadHeader":       ReadHeader,
		"ReadHeaderLimits": func(r *bufio.Reader) (Header, error) { return ReadHeaderLimits(r, HeaderLimits{MaxLineBytes: 1000}) },
	}
	for api, readFn := range read {
		for cut := 0; cut <= len(text); cut++ {
			for _, size := range []int{16, 4096} {
				for _, chunk := range []int{1, 7, 4096} {
					for _, withData := range []bool{false, true} {
						for _, sticky := range []bool{true, false} {
							name := fmt.Sprintf("%s/cut %d/buf %d/chunk %d/with %v/sticky %v", api, cut, size, chunk, withData, sticky)
							src := &failAtSource{data: text, cut: cut, chunk: chunk, err: bufio.ErrBufferFull, withData: withData, sticky: sticky}
							h, err := readFn(bufio.NewReaderSize(src, size))
							kept, complete := wholeLines(text, cut)
							switch {
							case complete:
								if err != nil || writtenHeader(t, h) != block {
									t.Errorf("%s: (%q, %v), want the whole block", name, writtenHeader(t, h), err)
								}
							case sticky || src.readsAfter == 0:
								if err != bufio.ErrBufferFull || writtenHeader(t, h) != kept+"\r\n" {
									t.Errorf("%s: (%q, %v), want the whole lines %q and the source's own error", name, writtenHeader(t, h), err, kept+"\r\n")
								}
								if src.readsAfter > 1 {
									t.Errorf("%s: %d reads after the failure, want at most one", name, src.readsAfter)
								}
							}
						}
					}
				}
			}
		}
	}

	// Framing lines, with and without a line budget: the budgeted reader
	// accumulated a line across buffers on the same value.
	const parts = "preamble " + "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyy" + "\r\n--B\r\nA: 1\r\n\r\nbody\r\n--B--\r\n"
	for _, budget := range []int64{0, 100} {
		for cut := 0; cut <= len(parts); cut++ {
			for _, chunk := range []int{1, 7, 4096} {
				src := &failAtSource{data: parts, cut: cut, chunk: chunk, err: bufio.ErrBufferFull, sticky: true}
				mr := NewMultipartReader(src, "B")
				mr.SetHeaderLimits(HeaderLimits{MaxLineBytes: budget})
				saw := false
				for range 6 {
					p, err := mr.NextPart()
					if err != nil {
						saw = saw || errors.Is(err, bufio.ErrBufferFull)
						if err == io.EOF {
							break
						}
						continue
					}
					if _, berr := io.ReadAll(p); errors.Is(berr, bufio.ErrBufferFull) {
						saw = true
					}
				}
				if want := cut < len(parts); saw != want {
					t.Errorf("budget %d/cut %d/chunk %d: saw the failure %v, want %v", budget, cut, chunk, saw, want)
				}
			}
		}
	}
}

// TestMultipartReader_partHeaderReadFailure pins the same rule for a part's
// header block: the iteration reports the input's failure, and nothing after it
// is read.
func TestMultipartReader_partHeaderReadFailure(t *testing.T) {
	const text = "--B\r\nA: 1\r\nB: 2\r\n\r\nbody\r\n--B--\r\n"
	for cut := len("--B\r\n"); cut < strings.Index(text, "body"); cut++ {
		for _, sticky := range []bool{true, false} {
			src := &failAtSource{data: text, cut: cut, chunk: 4096, err: errEIO, sticky: sticky}
			mr := NewMultipartReader(src, "B")
			p, err := mr.NextPart()
			if p != nil || !errors.Is(err, errEIO) || src.readsAfter != 0 {
				t.Errorf("cut %d/sticky %v: NextPart() = (%v, %v) after %d reads past the failure", cut, sticky, p, err, src.readsAfter)
			}
		}
	}
}
