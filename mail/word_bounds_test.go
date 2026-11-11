package mail

import (
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
)

// cdHookReader is a CharsetReader's reader that misbehaves in one way.
type cdHookReader struct {
	mode string
	in   io.Reader
}

func (r *cdHookReader) Read(p []byte) (int, error) {
	switch r.mode {
	case "stall":
		return 0, nil
	case "flood":
		for i := range p {
			p[i] = 'A'
		}
		return len(p), nil
	case "negative":
		return -1, nil
	case "overrun":
		return len(p) + 1, nil
	case "slow":
		// One empty read before each byte: slow, and never stalled.
		r.mode = "slow-byte"
		return 0, nil
	case "slow-byte":
		r.mode = "slow"
		return r.in.Read(p[:1])
	}
	return r.in.Read(p)
}

// TestDecodeWord_hookReaderBounds: decodeWord holds the reader a
// CharsetReader returns to the bounds the root decoder has — no endless wait
// on empty reads, no unbounded output, no panic on an impossible count — and
// keeps a word that breaks one as it was written. A reader that is merely
// slow still decodes.
func TestDecodeWord_hookReaderBounds(t *testing.T) {
	old := message.CharsetReader
	t.Cleanup(func() { message.CharsetReader = old })
	const word = "=?x-test?Q?abc?="
	for _, tc := range []struct {
		mode, want string
		encoded    bool
	}{
		{"stall", word, false},
		{"flood", word, false},
		{"negative", word, false},
		{"overrun", word, false},
		{"slow", "abc", true},
		{"", "abc", true},
	} {
		message.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) {
			return &cdHookReader{mode: tc.mode, in: in}, nil
		}
		got, encoded, unknown := decodeWord(word)
		if got != tc.want || encoded != tc.encoded || unknown {
			t.Errorf("%q: decodeWord() = %q, %v, %v; want %q, %v, false", tc.mode, got, encoded, unknown, tc.want, tc.encoded)
		}
	}

	// A hook that returns neither a reader nor an error names no charset.
	message.CharsetReader = func(string, io.Reader) (io.Reader, error) { return nil, nil }
	if got, encoded, unknown := decodeWord(word); got != word || encoded || !unknown {
		t.Errorf("nil reader: decodeWord() = %q, %v, %v; want the word, false, true", got, encoded, unknown)
	}

	// The output bound is that of the root decoder: six times the input and
	// 1 KiB. A conversion within it is not refused.
	message.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) {
		return strings.NewReader(strings.Repeat("x", 6*len(word)+1024)), nil
	}
	if got, encoded, _ := decodeWord(word); !encoded || len(got) != 6*len(word)+1024 {
		t.Errorf("output at the bound: %d bytes, encoded %v; want it decoded", len(got), encoded)
	}
}
