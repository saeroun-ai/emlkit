package textproto

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// unfoldCases are folded fields, written with "\r\n" line endings, with the
// value RFC 5322 section 3.2.2 unfolding gives (rfc) — what a block read with
// HeaderLimits.Recover holds — and the value the legacy unfolding gives
// (legacy), which ReadHeader and ReadHeaderLimits keep (ROB-020-007, #63).
var unfoldCases = []struct {
	name        string
	field       string
	rfc, legacy string
}{
	{"tab fold", "Subject: a long subject that is\r\n\tfolded\r\n", "a long subject that is\tfolded", "a long subject that is folded"},
	{"space fold", "Subject: a\r\n b\r\n", "a b", "a b"},
	{"spaces around fold", "Subject: a  \r\n   b\r\n", "a     b", "a b"},
	{"tab and space fold", "Subject: a\t\r\n \tb\r\n", "a\t \tb", "a b"},
	{"two folds", "Subject: a\r\n\tb\r\n  c\r\n", "a\tb  c", "a b c"},
	{"white space only continuation at end", "Subject: s\r\n \t\r\n", "s \t", "s"},
	{"white space only continuation inside", "Subject: a\r\n \r\n b\r\n", "a  b", "a b"},
	{"trailing white space", "Subject: x \t\r\n", "x \t", "x"},
	{"inner runs", "Subject: a  b\t\tc\r\n", "a  b\t\tc", "a  b\t\tc"},
	{"leading white space", "Subject:   \t x\r\n", "x", "x"},
	{"leading fold", "Subject:\r\n\tx\r\n", "x", "x"},
	{"leading white space only line", "Subject: \r\n \r\n  x y\r\n", "x y", "x y"},
	{"empty", "Subject:\r\n", "", ""},
	{"white space only", "Subject: \r\n \t\r\n", "", ""},
	{"encoded words folded", "Subject: =?utf-8?q?a?=\r\n\t =?utf-8?q?b?=\r\n", "=?utf-8?q?a?=\t =?utf-8?q?b?=", "=?utf-8?q?a?= =?utf-8?q?b?="},
	{"encoded word and text", "Subject: =?utf-8?q?a?=\r\n  x\r\n =?utf-8?q?b?=\r\n", "=?utf-8?q?a?=  x =?utf-8?q?b?=", "=?utf-8?q?a?= x =?utf-8?q?b?="},
}

// TestUnfold_recover reads each case under recovery with each line ending
// recovery accepts — "\r\n", a bare '\n' and a bare '\r' — and checks that
// only the line endings were removed, and that the field's bytes are kept
// exactly as read.
func TestUnfold_recover(t *testing.T) {
	const next = "X-Next: 1"
	for _, tc := range unfoldCases {
		for _, end := range []string{"\r\n", "\n", "\r"} {
			field := strings.ReplaceAll(tc.field, "\r\n", end)
			block := field + next + end + end
			h, unread, err := ReadHeaderRecover(bufio.NewReader(strings.NewReader(block+"body")), HeaderLimits{})
			if err != nil || len(unread) != 0 {
				t.Errorf("%s %q: ReadHeaderRecover() = %q, %v", tc.name, end, unread, err)
				continue
			}
			if got := h.Get("Subject"); got != tc.rfc {
				t.Errorf("%s %q: Get() = %q, want %q", tc.name, end, got, tc.rfc)
			}
			if got := h.Get("X-Next"); got != "1" {
				t.Errorf("%s %q: Get(X-Next) = %q", tc.name, end, got)
			}
			if raw, _ := h.Raw("Subject"); string(raw) != tc.field {
				t.Errorf("%s %q: Raw() = %q, want %q", tc.name, end, raw, tc.field)
			}
			if raw, _ := h.RawAsRead("Subject"); string(raw) != field {
				t.Errorf("%s %q: RawAsRead() = %q, want %q", tc.name, end, raw, field)
			}
			var w bytes.Buffer
			if err := WriteHeaderAsRead(&w, h); err != nil || w.String() != block {
				t.Errorf("%s %q: WriteHeaderAsRead() = %q, %v, want %q", tc.name, end, w.String(), err, block)
			}
		}
	}
}

// TestUnfold_legacy pins the unfolding of ReadHeader and ReadHeaderLimits,
// which stay as they were: each fold and the white space around it become one
// space, and a line of white space alone is dropped.
func TestUnfold_legacy(t *testing.T) {
	for _, tc := range unfoldCases {
		for _, end := range []string{"\r\n", "\n"} {
			block := strings.ReplaceAll(tc.field+"\r\n", "\r\n", end)
			h, err := ReadHeader(bufio.NewReader(strings.NewReader(block)))
			if err != nil {
				t.Errorf("%s %q: ReadHeader() = %v", tc.name, end, err)
				continue
			}
			if got := h.Get("Subject"); got != tc.legacy {
				t.Errorf("%s %q: ReadHeader Get() = %q, want %q", tc.name, end, got, tc.legacy)
			}
			h, err = ReadHeaderLimits(bufio.NewReader(strings.NewReader(block)), HeaderLimits{MaxLineBytes: 1 << 10})
			if err != nil || h.Get("Subject") != tc.legacy {
				t.Errorf("%s %q: ReadHeaderLimits Get() = %q, %v, want %q", tc.name, end, h.Get("Subject"), err, tc.legacy)
			}
		}
	}
}

// TestUnfold_addRaw pins that AddRaw keeps the legacy unfolding.
func TestUnfold_addRaw(t *testing.T) {
	var h Header
	h.AddRaw([]byte("Subject: a\r\n\tb \r\n"))
	if got := h.Get("Subject"); got != "a b" {
		t.Errorf("Get() = %q", got)
	}
}

// TestUnfold_unindented pins the value of a field to which recovery joined a
// line that lost its leading white space (UnindentedContinuations): one space
// stands for the white space lost, and the white space of real folds stays.
func TestUnfold_unindented(t *testing.T) {
	cases := []struct{ field, want string }{
		{"Content-Type: text/plain;\r\ncharset=\"utf-8\"\r\n", "text/plain; charset=\"utf-8\""},
		{"Content-Type: text/plain;\t\r\ncharset=\"utf-8\"\r\n", "text/plain;\t charset=\"utf-8\""},
		{"To: a@example.com,\r\nb@example.com\r\n", "a@example.com, b@example.com"},
		{"Received: from a.example\r\n\t(a [192.0.2.1])\r\nby b.example; Mon, 1 Jan 2024 00:00:00 +0000\r\n",
			"from a.example\t(a [192.0.2.1]) by b.example; Mon, 1 Jan 2024 00:00:00 +0000"},
	}
	for _, tc := range cases {
		for _, end := range []string{"\r\n", "\n"} {
			field := strings.ReplaceAll(tc.field, "\r\n", end)
			block := field + "X-Next: 1" + end + end
			h, _, err := ReadHeaderRecover(bufio.NewReader(strings.NewReader(block)), HeaderLimits{})
			if err != nil {
				t.Errorf("%q: ReadHeaderRecover() = %v", field, err)
				continue
			}
			if n, _ := h.UnindentedContinuations(); n != 1 {
				t.Errorf("%q: UnindentedContinuations() = %d, want 1", field, n)
			}
			fs := h.Fields()
			var got string
			for fs.Next() {
				if fs.Key() != "X-Next" {
					got = fs.Value()
				}
			}
			if got != tc.want {
				t.Errorf("%q: value %q, want %q", field, got, tc.want)
			}
			var w bytes.Buffer
			if err := WriteHeaderAsRead(&w, h); err != nil || w.String() != block {
				t.Errorf("%q: WriteHeaderAsRead() = %q, %v", field, w.String(), err)
			}
		}
	}
}

// TestUnfold_contentCR pins that a '\r' recovery found to be content, not a
// line ending, stays in the value.
func TestUnfold_contentCR(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\r\nb", "a b"}, // a line that lost its white space: one space for it
		{"a\nb", "a b"},
		{"a\rb\r\n", "a\rb"},
		{"a\r\r\n\tb\r\n", "a\r\tb"},
		{"\r\n \r\n\ta\n b", "a b"},
		{"\rx", "\rx"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := unfoldValue([]byte(tc.in)); got != tc.want {
			t.Errorf("unfoldValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestUnfold_manyFolds checks a value of many folds, each of white space
// alone, against the value built independently: every byte but the line
// endings, in order.
func TestUnfold_manyFolds(t *testing.T) {
	var in, want strings.Builder
	in.WriteString("Subject: x")
	want.WriteString("x")
	for i := range 2000 {
		ws := []string{" ", "\t", "  \t"}[i%3]
		in.WriteString("\r\n")
		in.WriteString(ws)
		want.WriteString(ws)
	}
	in.WriteString("\r\n\r\n")
	h, _, err := ReadHeaderRecover(bufio.NewReader(strings.NewReader(in.String())), HeaderLimits{MaxLineBytes: 1 << 10, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get("Subject"); got != want.String() {
		t.Errorf("Get() has %d bytes, want %d", len(got), want.Len())
	}
}
