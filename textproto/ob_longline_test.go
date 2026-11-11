package textproto

import (
	"bufio"
	"strings"
	"testing"
)

// TestHeader_LongLine pins where LongLine finds the first line past a limit:
// its field, and its offset in the block among orphan records, a malformed
// prefix and line endings of every kind.
func TestHeader_LongLine(t *testing.T) {
	long := strings.Repeat("x", 1000)
	tests := []struct {
		name, block string
		key         string
		start, n    int64
		ok          bool
	}{
		{"none", "A: x\r\nB: y\r\n\r\n", "", 0, 0, false},
		{"field", "A: short\r\nB: " + long + "\r\n\r\n", "B", 10, 1003, true},
		{"folded", "A: x\r\nB: y\r\n " + long + "\r\n\r\n", "B", 12, 1001, true},
		{"after an orphan record", "A: x\r\nbad line\r\nSubject: " + long + "\r\nC: y\r\n\r\n", "Subject", 16, 1009, true},
		{"orphan record", "A: x\r\n" + long + "\r\nSubject: y\r\n\r\n", "", 6, 1000, true},
		{"bare LF", "A: x\nB: " + long + "\n\n", "B", 5, 1003, true},
		{"bare CR ends a line", "A: x\nB: y\r" + long + "\n\n", "B", 10, 1000, true},
		{"malformed prefix", " " + long + "\r\nA: x\r\n\r\n", "", 0, 1001, true},
		{"after a malformed prefix", " lead\r\nA: " + long + "\r\n\r\n", "A", 7, 1003, true},
		{"byte order mark", "\xef\xbb\xbfA: " + long + "\r\n\r\n", "A", 3, 1003, true},
	}
	for _, tt := range tests {
		h, _, err := ReadHeaderRecover(bufio.NewReader(strings.NewReader(tt.block)), HeaderLimits{Recover: true})
		if err != nil {
			t.Errorf("%s: ReadHeaderRecover() = %v", tt.name, err)
			continue
		}
		key, start, n, ok := h.LongLine(998)
		if key != tt.key || start != tt.start || n != tt.n || ok != tt.ok {
			t.Errorf("%s: LongLine() = %q, %d, %d, %v; want %q, %d, %d, %v", tt.name, key, start, n, ok, tt.key, tt.start, tt.n, tt.ok)
		}
	}
	var h Header
	h.Add("Subject", long)
	if _, _, _, ok := h.LongLine(998); ok {
		t.Error("a header built by hand has no block to look at")
	}
}
