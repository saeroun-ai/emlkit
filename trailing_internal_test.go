package message

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// The evidence of the trailing input (ROB-010-005), checked against an
// independent reference: refEvidence splits a whole, ended input into lines
// and decides from them, where the classifier looks at one byte at a time, and
// it takes the "From_" grammar from mboxReference, the regular expression
// mbox_internal_test.go writes independently of mbox.go, rather than from the
// recognizer the classifier shares with the product. Its line endings are the
// longest match, a lone '\r' at the very end of the input being decided by
// io.EOF itself (viaEOF).

// refLine is one line of an input: its bytes are [start, end), its content
// [start, cEnd); ending reports a line ending, cr that it is a lone '\r'.
type refLine struct {
	start, cEnd, end int
	ending, cr       bool
}

// refLines splits w, an input that ended, at "\r\n", '\n' and '\r', the longest
// that matches.
func refLines(w []byte) []refLine {
	var out []refLine
	for i := 0; i < len(w); {
		j := i
		for j < len(w) && w[j] != '\r' && w[j] != '\n' {
			j++
		}
		if j == len(w) {
			out = append(out, refLine{start: i, cEnd: j, end: j})
			break
		}
		l := refLine{start: i, cEnd: j, end: j + 1, ending: true}
		if w[j] == '\r' {
			if j+1 < len(w) && w[j+1] == '\n' {
				l.end = j + 2
			} else {
				l.cr = true
			}
		}
		out = append(out, l)
		i = l.end
	}
	return out
}

func refBlank(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}

// refField reports whether line l of w begins as a header field — a name of
// visible ASCII but ':', ':', then a space, a tab or its line ending — and
// where its ':' is.
func refField(w []byte, l refLine) (int, bool) {
	content := w[l.start:l.cEnd]
	c := bytes.IndexByte(content, ':')
	if c <= 0 {
		return 0, false
	}
	for _, b := range content[:c] {
		if b < 0x21 || b > 0x7e {
			return 0, false
		}
	}
	if c+1 < len(content) {
		return l.start + c, content[c+1] == ' ' || content[c+1] == '\t'
	}
	return l.start + c, l.ending
}

type refResult struct {
	ev         TrailingEvidence
	start, end int64
	viaEOF     bool // decided by io.EOF right after a lone '\r'
}

// refEvidence decides the evidence of w, an input io.EOF ended, of any
// length, as if every byte of it may be looked at.
func refEvidence(w []byte, lineCap int) refResult {
	if len(w) == 0 {
		return refResult{ev: TrailingEvidenceEmpty}
	}
	lines := refLines(w)
	i := 0
	for i < len(lines) && refBlank(w[lines[i].start:lines[i].cEnd]) {
		i++
	}
	if i == len(lines) {
		return refResult{ev: TrailingEvidenceBlank}
	}
	l := lines[i]
	content := w[l.start:l.cEnd]
	if content[0] == ' ' || content[0] == '\t' {
		return refResult{ev: TrailingEvidenceUnrecognized}
	}
	if l.ending && len(content) <= lineCap && mboxReference.Match(content) {
		return refResult{ev: TrailingEvidenceMessageLike, start: int64(l.start), end: int64(l.end), viaEOF: l.cr && l.end == len(w)}
	}
	if _, ok := refField(w, l); ok {
		j := i + 1
		for j < len(lines) && lines[j].cEnd > lines[j].start && (w[lines[j].start] == ' ' || w[lines[j].start] == '\t') {
			j++
		}
		if j < len(lines) && lines[j].cEnd > lines[j].start {
			m := lines[j]
			if c, ok := refField(w, m); ok {
				// A space or a tab after the ':' proves it with that byte; an
				// empty value with its whole line ending.
				if c+1 < m.cEnd {
					return refResult{ev: TrailingEvidenceMessageLike, start: int64(l.start), end: int64(c + 2)}
				}
				return refResult{ev: TrailingEvidenceMessageLike, start: int64(l.start), end: int64(m.end), viaEOF: m.cr && m.end == len(w)}
			}
		}
	}
	return refResult{ev: TrailingEvidenceUnrecognized}
}

// refWindowed is refEvidence with the 4096-byte window: of an input longer
// than it only the window is looked at, and what only the end of the input
// could have decided — blank lines, a lone '\r' last — is decided by the
// arrival of the byte after the window instead, which cannot be looked at.
func refWindowed(w []byte, lineCap int) refResult {
	if len(w) <= trailingEvidenceWindow {
		return refEvidence(w, lineCap)
	}
	r := refEvidence(w[:trailingEvidenceWindow], lineCap)
	if r.ev == TrailingEvidenceBlank || r.viaEOF {
		return refResult{ev: TrailingEvidenceUnrecognized}
	}
	return r
}

// classify feeds w to a classifier in the given chunks and ends it with io.EOF.
func classify(w []byte, lineCap int, chunks []int) (trailingClassifier, int) {
	c := trailingClassifier{lineCap: lineCap}
	newly := 0
	for len(w) > 0 {
		n := len(w)
		if len(chunks) > 0 {
			n = min(n, max(1, chunks[0]))
			chunks = chunks[1:]
		}
		if c.feed(w[:n]) {
			newly++
		}
		w = w[n:]
	}
	if c.finish() {
		newly++
	}
	return c, newly
}

// checkClassifier checks the classifier on w against the reference, whole and
// in chunks, and that what any prefix decided without io.EOF is the final
// result.
func checkClassifier(t *testing.T, name string, w []byte, lineCap int, rnd *rand.Rand) {
	t.Helper()
	want := refWindowed(w, lineCap)
	ones := make([]int, len(w))
	for i := range ones {
		ones[i] = 1
	}
	// Whole; one byte at a time; a fixed mix, the rest whole; and, given rnd,
	// random chunks of 1 to 97 bytes.
	splits := [][]int{nil, ones, {3, 7, 1, 4096}}
	if rnd != nil {
		s := make([]int, 0, len(w))
		for n := 0; n < len(w); {
			k := 1 + rnd.Intn(97)
			s = append(s, k)
			n += k
		}
		splits = append(splits, s)
	}
	for si, chunks := range splits {
		c, newly := classify(w, lineCap, chunks)
		if c.evidence != want.ev {
			t.Fatalf("%s (cap %d, split %d): evidence %v, want %v", name, lineCap, si, c.evidence, want.ev)
		}
		if want.ev == TrailingEvidenceMessageLike {
			if c.evStart != want.start || c.evEnd != want.end {
				t.Fatalf("%s: evidence [%d, %d), want [%d, %d)", name, c.evStart, c.evEnd, want.start, want.end)
			}
			if newly != 1 {
				t.Fatalf("%s: message-like evidence reported %d times", name, newly)
			}
		} else if newly != 0 {
			t.Fatalf("%s: %v evidence reported as message-like", name, c.evidence)
		}
		if c.line != nil {
			t.Fatalf("%s: a decided classifier still holds a line", name)
		}
	}
	// A decision made before io.EOF is final, needs no byte past the window,
	// and is made no earlier than its evidence ends. Fed one byte at a time,
	// the classifier after k bytes is the classifier of the k-byte prefix.
	c := trailingClassifier{lineCap: lineCap}
	for k := 1; k <= len(w); k++ {
		c.feed(w[k-1 : k])
		if len(c.line) > lineCap {
			t.Fatalf("%s: holds %d bytes of a line, over %d", name, len(c.line), lineCap)
		}
		if !c.decided {
			if k > trailingEvidenceWindow {
				t.Fatalf("%s: undecided after %d bytes", name, k)
			}
			continue
		}
		if c.evidence != want.ev || c.evStart != want.start || c.evEnd != want.end {
			t.Fatalf("%s: prefix %d decided %v [%d, %d), the whole %v [%d, %d)", name, k, c.evidence, c.evStart, c.evEnd, want.ev, want.start, want.end)
		}
		if want.ev == TrailingEvidenceMessageLike && want.end > int64(k) {
			t.Fatalf("%s: decided at %d before its evidence ends at %d", name, k, want.end)
		}
		break
	}
}

const testFromLine = "From MAILER-DAEMON Fri Jul  8 12:08:34 2011"

func TestTrailingClassifier(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ev   TrailingEvidence
		end  int // for message-like evidence, which starts at first
		from int
	}{
		{"empty", "", TrailingEvidenceEmpty, 0, 0},
		{"crlf", "\r\n", TrailingEvidenceBlank, 0, 0},
		{"blank lines", " \t\r\n\n\r \r\n\t", TrailingEvidenceBlank, 0, 0},
		{"rfc 2046 epilogue", "This is the epilogue.  It is also to be ignored.\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"binary", "\x00\xff\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"two fields", "From: a@example.com\r\nTo: b@example.com\r\n\r\nhi", TrailingEvidenceMessageLike, 25, 0},
		{"two fields lf", "A: 1\nB: 2\n", TrailingEvidenceMessageLike, 8, 0},
		{"two fields cr", "A: 1\rB: 2\r", TrailingEvidenceMessageLike, 8, 0},
		{"tab separators", "A:\t1\r\nB:\t2", TrailingEvidenceMessageLike, 9, 0},
		{"empty values", "A:\r\nB:\r\n", TrailingEvidenceMessageLike, 8, 0},
		{"empty value at eof", "A:\r\nB:", TrailingEvidenceUnrecognized, 0, 0},
		{"empty value crlf", "A: 1\r\nB:\r\nrest", TrailingEvidenceMessageLike, 10, 0},
		{"empty value cr at eof", "A: 1\r\nB:\r", TrailingEvidenceMessageLike, 9, 0},
		{"empty value cr then byte", "A: 1\r\nB:\rX", TrailingEvidenceMessageLike, 9, 0},
		{"empty value cr then cr", "A: 1\r\nB:\r\r", TrailingEvidenceMessageLike, 9, 0},
		{"empty value lf", "A: 1\r\nB:\n", TrailingEvidenceMessageLike, 9, 0},
		{"second space at eof", "A: 1\r\nB: ", TrailingEvidenceMessageLike, 9, 0},
		{"after blank lines", "\r\n\r\nReceived: x\r\nFrom: y\r\n", TrailingEvidenceMessageLike, 23, 4},
		{"continuation", "Received: a\r\n b\r\n\tc\r\nTo: d\r\n", TrailingEvidenceMessageLike, 25, 0},
		{"blank between", "A: 1\r\n\r\nB: 2\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"one field", "Note: scanned\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"one field then text", "Note: scanned\r\nby a gateway\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"urls", "http://a.example\r\nhttps://b.example\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"no space after colon", "A:1\r\nB:2\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"second without space", "A: 1\r\nB:2\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"space in name", "Not a: header\r\nB: 2\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"empty name", ": x\r\nB: 2\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"leading space", " A: 1\r\nB: 2\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"bom", "\xef\xbb\xbfA: 1\r\nB: 2\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"from line", testFromLine + "\r\nbody", TrailingEvidenceMessageLike, len(testFromLine) + 2, 0},
		{"from line lf", testFromLine + "\nx", TrailingEvidenceMessageLike, len(testFromLine) + 1, 0},
		{"from line cr", testFromLine + "\rx", TrailingEvidenceMessageLike, len(testFromLine) + 1, 0},
		{"from line cr at eof", testFromLine + "\r", TrailingEvidenceMessageLike, len(testFromLine) + 1, 0},
		{"from line at eof", testFromLine, TrailingEvidenceUnrecognized, 0, 0},
		{"escaped from", ">" + testFromLine + "\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"from prose", "From the 10:30 meeting\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"from field", "From: a\r\nTo: b\r\n", TrailingEvidenceMessageLike, 13, 0},
		{"from trailing space", testFromLine + " \r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"from 8-bit", "From a\xff Fri Jul  8 12:08:34 2011\r\n", TrailingEvidenceUnrecognized, 0, 0},
		{"delimiters", "--B\r\n\r\nx\r\n--B--\r\n", TrailingEvidenceUnrecognized, 0, 0},
	}
	for _, tc := range cases {
		c, _ := classify([]byte(tc.in), maxMboxEnvelopeContent, nil)
		if c.evidence != tc.ev {
			t.Errorf("%s: evidence %v, want %v", tc.name, c.evidence, tc.ev)
			continue
		}
		if tc.ev == TrailingEvidenceMessageLike && (c.evStart != int64(tc.from) || c.evEnd != int64(tc.end)) {
			t.Errorf("%s: evidence [%d, %d), want [%d, %d)", tc.name, c.evStart, c.evEnd, tc.from, tc.end)
		}
		checkClassifier(t, tc.name, []byte(tc.in), maxMboxEnvelopeContent, rand.New(rand.NewSource(int64(len(tc.in)))))
	}
}

// TestTrailingClassifierWindow places the decisive bytes at and around the end
// of the 4096-byte window, and checks a "From_" line at and past its bound.
func TestTrailingClassifierWindow(t *testing.T) {
	two := "A: 1\r\nB: 2\r\n"
	for pad := trailingEvidenceWindow - len(two) - 3; pad <= trailingEvidenceWindow+2; pad++ {
		for _, tail := range []string{"", "x", "\r\nmore"} {
			w := []byte(strings.Repeat("\r\n", pad/2) + strings.Repeat(" ", pad%2) + "\r\n" + two + tail)
			checkClassifier(t, "padded header", w, maxMboxEnvelopeContent, nil)
			w = []byte(strings.Repeat("\n", pad) + testFromLine + "\r" + tail)
			checkClassifier(t, "padded from", w, maxMboxEnvelopeContent, nil)
		}
		blank := []byte(strings.Repeat(" ", pad))
		checkClassifier(t, "blank", blank, maxMboxEnvelopeContent, nil)
	}

	// An empty second value ended by a '\r', which the byte after it or io.EOF
	// decides, around the window's last byte.
	empty := "A: 1\r\nB:\r"
	for pad := trailingEvidenceWindow - len(empty) - 3; pad <= trailingEvidenceWindow-len(empty)+2; pad++ {
		for _, tail := range []string{"", "x", "\n", "\r", "\nmore"} {
			checkClassifier(t, "padded empty value", []byte(strings.Repeat("\n", pad)+empty+tail), maxMboxEnvelopeContent, nil)
		}
	}
	pad := trailingEvidenceWindow - len(empty) // the '\r' is the window's last byte
	w := []byte(strings.Repeat("\n", pad) + empty)
	if c, _ := classify(w, maxMboxEnvelopeContent, nil); c.evidence != TrailingEvidenceMessageLike || c.evStart != int64(pad) || c.evEnd != trailingEvidenceWindow {
		t.Errorf("'\\r' last in the window, then io.EOF: %v [%d, %d)", c.evidence, c.evStart, c.evEnd)
	}
	for _, next := range []string{"\n", "x"} {
		if c, _ := classify(append(w[:len(w):len(w)], next...), maxMboxEnvelopeContent, nil); c.evidence != TrailingEvidenceUnrecognized {
			t.Errorf("'\\r' last in the window, then %q: %v", next, c.evidence)
		}
	}
	undecided := trailingClassifier{lineCap: maxMboxEnvelopeContent}
	if undecided.feed(w); undecided.decided {
		t.Errorf("'\\r' last in the window, no io.EOF: decided %v", undecided.evidence)
	}
	// Exactly the window of blanks then io.EOF is blank; one byte more is not.
	c, _ := classify([]byte(strings.Repeat("\n", trailingEvidenceWindow)), maxMboxEnvelopeContent, nil)
	if c.evidence != TrailingEvidenceBlank {
		t.Errorf("a window of blank lines: %v", c.evidence)
	}
	c, _ = classify([]byte(strings.Repeat("\n", trailingEvidenceWindow+1)), maxMboxEnvelopeContent, nil)
	if c.evidence != TrailingEvidenceUnrecognized {
		t.Errorf("a window and one of blank lines: %v", c.evidence)
	}

	// A "From_" line is held to the line cap, "From " included.
	sender := "From " + strings.Repeat("s", 10) + " Fri Jul  8 12:08:34 2011"
	for _, lineCap := range []int{1, 4, 5, len(sender) - 1, len(sender), len(sender) + 1, 998} {
		w := []byte(sender + "\r\nx")
		checkClassifier(t, "from at cap", w, lineCap, nil)
		c, _ := classify(w, lineCap, nil)
		if want := lineCap >= len(sender); (c.evidence == TrailingEvidenceMessageLike) != want {
			t.Errorf("cap %d: evidence %v for a %d-byte line", lineCap, c.evidence, len(sender))
		}
	}
	long := "From " + strings.Repeat("s", 998-len(" Fri Jul  8 12:08:34 2011")-5) + " Fri Jul  8 12:08:34 2011"
	if len(long) != 998 {
		t.Fatalf("fixture is %d bytes", len(long))
	}
	for i, w := range []string{long + "\n", long[:5] + "s" + long[5:] + "\n"} {
		checkClassifier(t, "from bound", []byte(w), maxMboxEnvelopeContent, nil)
		c, _ := classify([]byte(w), maxMboxEnvelopeContent, nil)
		if want := i == 0; (c.evidence == TrailingEvidenceMessageLike) != want {
			t.Errorf("a %d-byte \"From_\" line: evidence %v", len(w)-1, c.evidence)
		}
	}
}

// TestTrailingClassifierUndecided pins that a classifier given only some of
// the bytes, with no io.EOF, decides nothing it cannot prove.
func TestTrailingClassifierUndecided(t *testing.T) {
	for _, in := range []string{"", "\r\n", "  ", "A: 1\r\n", "A: 1\r\nB", "A: 1\r\nB:", "A: 1\r\nB:\r", "A:\r", testFromLine, testFromLine + "\r", "Fro"} {
		c := trailingClassifier{lineCap: maxMboxEnvelopeContent}
		c.feed([]byte(in))
		if c.decided || c.evidence != TrailingEvidenceUndecided {
			t.Errorf("%q without io.EOF: %v", in, c.evidence)
		}
	}
}

// FuzzTrailingEvidence checks the classifier against the reference for any
// input of at most 8 KiB, any line cap and any chunking.
func FuzzTrailingEvidence(f *testing.F) {
	f.Add([]byte("A: 1\r\nB: 2\r\n"), uint16(998), int64(1))
	f.Add([]byte(testFromLine+"\r\nx"), uint16(998), int64(2))
	f.Add([]byte("\r\n \r\nReceived: a\r\n b\r\nTo: c\r\n"), uint16(40), int64(3))
	f.Add([]byte("This is the epilogue.\r\n"), uint16(998), int64(4))
	f.Add([]byte(strings.Repeat("\n", 4095)+"A: 1\nB: 2\n"), uint16(998), int64(5))
	f.Fuzz(func(t *testing.T, data []byte, lineCap uint16, seed int64) {
		if len(data) > 8<<10 {
			return
		}
		checkClassifier(t, "fuzz", data, int(lineCap)%(maxMboxEnvelopeContent+1), rand.New(rand.NewSource(seed)))
	})
}
