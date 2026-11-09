package textproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Empty and header-only header blocks (ROB-010-001). Every expectation is
// written out: the fields with their raw bytes in order, what r still holds
// after the block, and what WriteHeader writes. CRLF only; other line endings
// are ROB-010-002's. Every source is finite and capped.

var errEmptyCap = errors.New("test source: call cap exceeded")

// An emptyCaseSource delivers data in reads of at most chunk bytes, or, when
// chunk is zero, split once at split; io.EOF comes with the last bytes when
// eofWith, and on a read of its own otherwise.
type emptyCaseSource struct {
	data         string
	chunk, split int
	eofWith      bool

	pos, calls int
}

func (s *emptyCaseSource) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > len(s.data)+16 {
		return 0, errEmptyCap
	}
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	end := len(s.data)
	if s.chunk > 0 {
		end = min(end, s.pos+s.chunk)
	} else if s.split > s.pos {
		end = min(end, s.split)
	}
	n := copy(p, s.data[s.pos:end])
	s.pos += n
	if s.pos == len(s.data) && s.eofWith {
		return n, io.EOF
	}
	return n, nil
}

type emptyCaseField struct{ key, value, raw string }

func emptyCaseFields(h Header) []emptyCaseField {
	var out []emptyCaseField
	fs := h.Fields()
	for fs.Next() {
		raw, err := fs.Raw()
		if err != nil {
			raw = []byte("raw error: " + err.Error())
		}
		out = append(out, emptyCaseField{fs.Key(), fs.Value(), string(raw)})
	}
	return out
}

// TestReadHeader_emptyAndHeaderOnly pins ReadHeader and ReadHeaderLimits on
// an empty input, an input that is only the empty line, and complete fields
// ended by io.EOF or by the empty line, across reads of 1, 7 and 4096 bytes,
// every two-read split, and io.EOF with the last bytes or after them. The
// block yields exactly its fields and a nil error; what follows the empty
// line, blank lines included, is left in r; WriteHeader writes the fields'
// raw bytes and one empty line, so a block io.EOF ended gains that line, and
// reading what it wrote gives the same fields and the same bytes again.
func TestReadHeader_emptyAndHeaderOnly(t *testing.T) {
	const fields = "From: a@example.com\r\nSubject: one\r\n\ttwo\r\n"
	list := []emptyCaseField{
		{"From", "a@example.com", "From: a@example.com\r\n"},
		{"Subject", "one two", "Subject: one\r\n\ttwo\r\n"},
	}
	cases := []struct {
		name, text string
		fields     []emptyCaseField
		block      int64 // the block's wire bytes
		rest       string
		written    string
	}{
		{"empty", "", nil, 0, "", "\r\n"},
		{"separator", "\r\n", nil, 2, "", "\r\n"},
		{"separator and blank lines", "\r\n\r\n\r\n", nil, 2, "\r\n\r\n", "\r\n"},
		{"fields at EOF", fields, list, int64(len(fields)), "", fields + "\r\n"},
		{"fields and separator", fields + "\r\n", list, int64(len(fields)) + 2, "", fields + "\r\n"},
		{"fields, separator and body", fields + "\r\n\r\nbody\r\n", list, int64(len(fields)) + 2, "\r\nbody\r\n", fields + "\r\n"},
	}
	type reader struct {
		name string
		read func(*bufio.Reader) (Header, error)
	}
	for _, tc := range cases {
		readers := []reader{
			{"ReadHeader", ReadHeader},
			{"ReadHeaderLimits", func(r *bufio.Reader) (Header, error) {
				// Exactly the block's bytes and fields; the empty line is in the
				// block and in no field.
				return ReadHeaderLimits(r, HeaderLimits{MaxBytes: max(tc.block, 0), MaxCount: max(1, int64(len(tc.fields))), MaxFieldBytes: 64, MaxLineBytes: 64})
			}},
		}
		var sources []func() io.Reader
		var names []string
		for _, eofWith := range []bool{false, true} {
			for _, chunk := range []int{1, 7, 4096} {
				sources = append(sources, func() io.Reader { return &emptyCaseSource{data: tc.text, chunk: chunk, eofWith: eofWith} })
				names = append(names, fmt.Sprintf("chunk %d/eof with data %v", chunk, eofWith))
			}
			for split := 1; split < len(tc.text); split++ {
				sources = append(sources, func() io.Reader { return &emptyCaseSource{data: tc.text, split: split, eofWith: eofWith} })
				names = append(names, fmt.Sprintf("split %d/eof with data %v", split, eofWith))
			}
		}
		for _, rd := range readers {
			for i, src := range sources {
				name := fmt.Sprintf("%s/%s/%s", tc.name, rd.name, names[i])
				br := bufio.NewReader(src())
				h, err := rd.read(br)
				if err != nil {
					t.Errorf("%s: err = %v", name, err)
					continue
				}
				if got := emptyCaseFields(h); !reflect.DeepEqual(got, tc.fields) {
					t.Errorf("%s: fields %q, want %q", name, got, tc.fields)
				}
				if rest, rerr := io.ReadAll(br); string(rest) != tc.rest || rerr != nil {
					t.Errorf("%s: rest (%q, %v), want (%q, nil)", name, rest, rerr, tc.rest)
				}
				var w bytes.Buffer
				if werr := WriteHeader(&w, h); werr != nil || w.String() != tc.written {
					t.Errorf("%s: WriteHeader wrote (%q, %v), want %q", name, w.String(), werr, tc.written)
					continue
				}

				br = bufio.NewReader(strings.NewReader(w.String()))
				again, err := ReadHeader(br)
				if got := emptyCaseFields(again); err != nil || !reflect.DeepEqual(got, tc.fields) {
					t.Errorf("%s: reread (%q, %v), want %q", name, got, err, tc.fields)
				}
				if rest, _ := io.ReadAll(br); len(rest) != 0 {
					t.Errorf("%s: reread left %q", name, rest)
				}
				var w2 bytes.Buffer
				if werr := WriteHeader(&w2, again); werr != nil || w2.String() != tc.written {
					t.Errorf("%s: second WriteHeader wrote (%q, %v), want %q", name, w2.String(), werr, tc.written)
				}
			}
		}
	}
}
