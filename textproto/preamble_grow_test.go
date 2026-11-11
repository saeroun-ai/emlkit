package textproto

import (
	"bytes"
	"io"
	"testing"

	"github.com/saeroun-ai/emlkit/internal/scaletest"
)

func TestPfGrow(t *testing.T) {
	b := pfGrow(nil, 10, 1<<20)
	if len(b) != 0 || cap(b) < 512 {
		t.Fatalf("len %d cap %d, want room for a first line", len(b), cap(b))
	}
	b = append(b, "kept bytes"...)
	if same := pfGrow(b, cap(b)-len(b), 1<<20); &same[0] != &b[0] {
		t.Error("a slice with room was copied")
	}
	grown := pfGrow(b, cap(b), 1<<20)
	if string(grown) != "kept bytes" || cap(grown) < 2*cap(b) {
		t.Errorf("grown to %q with cap %d, want the bytes kept and the room doubled", grown, cap(grown))
	}
	// Never past the limit, and always enough for what was asked.
	if capped := pfGrow(b, 600, 700); cap(capped) != 700 {
		t.Errorf("cap %d under a limit of 700, want 700", cap(capped))
	}
	if exact := pfGrow(b, 690, 700); cap(exact) != 700 || len(exact) != len(b) {
		t.Errorf("cap %d len %d, want exactly the limit", cap(exact), len(exact))
	}
}

// TestPreambleKeptCost holds what keeping a body in which no delimiter line
// is found allocates to a small multiple of the body: appended to a line at a
// time, it used to be copied about five times over.
func TestPreambleKeptCost(t *testing.T) {
	body := bytes.Repeat([]byte("a line of a multipart body that no delimiter line frames\r\n"), 12000)
	read := func() {
		mr := NewMultipartReader(bytes.NewReader(body), "never-found")
		mr.SetHeaderLimits(HeaderLimits{Recover: true, MaxLineBytes: 64 << 10})
		if _, err := mr.NextPart(); err != io.EOF {
			t.Fatalf("NextPart: %v, want io.EOF", err)
		}
		if pre, over := mr.Preamble(); over || !bytes.Equal(pre, body) {
			t.Fatalf("kept %d bytes of %d (cut: %v)", len(pre), len(body), over)
		}
	}
	read()
	cost := scaletest.Measure(1, read)
	if limit := uint64(17 * len(body) / 4); cost.Bytes > limit {
		t.Errorf("allocated %d bytes to keep a body of %d, want at most %d", cost.Bytes, len(body), limit)
	}
}
