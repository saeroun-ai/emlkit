package mail

import (
	"errors"
	"io"
	"math"
)

// Bounds on the reader a CharsetReader returns for one RFC 2047 encoded word
// that decodeWord has mime.WordDecoder decode.
//
// That decoder copies the hook's reader to its end with io.Copy and trusts it
// entirely: a reader that returns (0, nil) for ever spins, one that keeps
// delivering grows the result without limit, and one that returns a count
// outside its buffer panics the copy. The root package's decoder
// (message.DecodeHeaderText; rfc2047.go and progress.go there) holds a hook
// reader to three rules, and the same three apply here:
//
//   - at most cdWordStallReads consecutive reads that deliver nothing and
//     take none of the word's bytes, then io.ErrNoProgress;
//   - output of at most six times the input plus 1 KiB, which no charset
//     conversion comes near. The input is counted as the length of the
//     encoded word as written, which is a little more than the bytes it
//     decodes to;
//   - a byte count below zero or beyond the buffer is an error, not a result.
//
// A word that breaks one of them is kept as it was written, like any word
// that cannot be decoded.

// cdWordStallReads is how many reads in a row may deliver nothing and consume
// nothing: the number the root decoder and io's own readers use.
const cdWordStallReads = 100

var (
	cdErrWordExpansion = errors.New("mail: charset decoder output exceeded the expansion bound of an encoded word")
	cdErrWordReader    = errors.New("mail: charset decoder returned an invalid byte count")
)

// cdWordOutputLimit is the most output a hook reader may deliver for an
// encoded word of n bytes.
func cdWordOutputLimit(n int) int {
	if n >= (math.MaxInt-1024)/6 {
		return math.MaxInt
	}
	return 6*n + 1024
}

// A cdCountedInput is the input handed to a CharsetReader, counting what the
// hook's reader takes of it. It implements io.Reader alone.
type cdCountedInput struct {
	r io.Reader
	n int64
}

func (c *cdCountedInput) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && n <= len(p) {
		c.n += int64(n)
	}
	return n, err
}

// A cdWordReader is a CharsetReader's reader of one encoded word under the
// bounds above. It implements io.Reader alone, so io.Copy cannot go around it
// through a WriterTo of the hook's reader.
type cdWordReader struct {
	r     io.Reader
	in    *cdCountedInput
	left  int // output still allowed
	empty int // consecutive reads without progress
}

func (w *cdWordReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	before := w.in.n
	n, err := w.r.Read(p)
	if n < 0 || n > len(p) {
		return 0, cdErrWordReader
	}
	if n > w.left {
		return 0, cdErrWordExpansion
	}
	w.left -= n
	if n == 0 && err == nil && w.in.n == before {
		if w.empty++; w.empty >= cdWordStallReads {
			return 0, io.ErrNoProgress
		}
		return 0, nil
	}
	w.empty = 0
	return n, err
}
