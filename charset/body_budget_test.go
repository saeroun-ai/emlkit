package charset_test

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	_ "github.com/saeroun-ai/emlkit/charset"
)

// The body budgets of message.Parse (ROB-001-005) with the real charset
// decoders this package installs: a decoded budget counts UTF-8 output bytes
// and may cut a character, and an encoded budget delivers everything the
// decoder made of the admitted wire bytes, however small the caller's reads.

var errCut = errors.New("cut")

type failAfter struct{ err error }

func (f failAfter) Read([]byte) (int, error) { return 0, f.err }

// decodeCut is what the decoder makes of wire cut with an error.
func decodeCut(t *testing.T, header, wire string) string {
	t.Helper()
	e, err := message.Read(strings.NewReader(header + "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := message.New(e.Header, io.MultiReader(strings.NewReader(wire), failAfter{errCut}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(d.Body)
	return string(b)
}

func readBy(r io.Reader, k int) ([]byte, error) {
	var out []byte
	buf := make([]byte, k)
	for range 1 << 16 {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
	}
	return out, errors.New("no end")
}

type chunked struct {
	s string
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if c.s == "" {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), c.n)], c.s)
	c.s = c.s[n:]
	return n, nil
}

func TestParse_bodyBudgetsWithCharsets(t *testing.T) {
	cases := []struct {
		name, header, wire, decoded string
	}{
		{"iso-8859-1", "Content-Type: text/plain; charset=iso-8859-1\r\n", "caf\xe9-\xa3-fin", "café-£-fin"},
		{"shift_jis", "Content-Type: text/plain; charset=shift_jis\r\n", "\x93\xfa\x96\x7b\x8c\xea abc", "日本語 abc"},
		{"iso-8859-1 base64", "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: base64\r\n", "6XTpIKM=", "été £"},
	}
	off := message.LimitDisabled
	for _, tc := range cases {
		text := tc.header + "\r\n" + tc.wire
		for _, kind := range []string{"encoded", "decoded"} {
			size := len(tc.decoded)
			if kind == "encoded" {
				size = len(tc.wire)
			}
			for limit := 1; limit <= size; limit++ {
				l := message.Limits{MaxEncodedBodyBytes: off, MaxDecodedBodyBytes: off, MaxTotalDecodedBytes: off}
				want := tc.decoded
				if kind == "encoded" {
					l.MaxEncodedBodyBytes = int64(limit)
					if limit < size {
						want = decodeCut(t, tc.header, tc.wire[:limit])
					}
				} else {
					l.MaxDecodedBodyBytes = int64(limit)
					want = tc.decoded[:limit]
				}
				for _, src := range []int{1, 2, 3, 7, 4096} {
					for _, dst := range []int{1, 2, 3, 7, 4096} {
						name := fmt.Sprintf("%s/%s=%d/src %d/dst %d", tc.name, kind, limit, src, dst)
						s, e, err := message.Parse(&chunked{text, src}, &message.ParseOptions{Limits: l})
						if err != nil {
							t.Fatalf("%s: Parse() = %v", name, err)
						}
						got, err := readBy(e.Body, dst)
						if string(got) != want {
							t.Errorf("%s: body = %q, want %q", name, got, want)
						}
						if over := limit < size; over != message.IsFatal(err) || (!over && err != io.EOF) {
							t.Errorf("%s: end = %v", name, err)
						}
						if limit < size && s.Len() != 1 {
							t.Errorf("%s: %d diagnostics, want one", name, s.Len())
						}
					}
				}
			}
		}
	}
}
