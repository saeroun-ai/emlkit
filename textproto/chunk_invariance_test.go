package textproto

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"
)

// TestChunkInvariance_closeAtBufferEnd moves the closing delimiter across the
// end of the reader's buffer one byte at a time. Read whole, a full buffer
// that ended with "\n--boundary-" decided the candidate there and then: it
// was body, and the multipart had no closing delimiter. Read a byte at a time
// the buffer is never full and the delimiter was found (soak seed 1300007,
// iteration 5509).
func TestChunkInvariance_closeAtBufferEnd(t *testing.T) {
	read := func(src io.Reader) (body []byte, end error) {
		mr := NewMultipartReader(src, "B")
		p, err := mr.NextPart()
		if err != nil {
			return nil, err
		}
		body, err = io.ReadAll(p)
		if err != nil {
			return body, err
		}
		_, end = mr.NextPart()
		return body, end
	}
	for _, ending := range []string{"\r\n", "\n", "\r"} {
		for pad := 2 * peekBufferSize; pad < 2*peekBufferSize+48; pad++ {
			want := bytes.Repeat([]byte{'x'}, pad-32)
			in := append([]byte("--B\r\n\r\n"), want...)
			in = append(in, ending+"--B--"+ending...)
			for name, src := range map[string]io.Reader{
				"whole":  bytes.NewReader(in),
				"1 byte": iotest.OneByteReader(bytes.NewReader(in)),
			} {
				body, end := read(src)
				if !bytes.Equal(body, want) || end != io.EOF {
					t.Fatalf("ending %q, %d bytes, read %s: a body of %d bytes (want %d) and then %v, want io.EOF", ending, len(in), name, len(body), len(want), end)
				}
			}
		}
	}
}
