package message

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
)

// The mbox envelope recognizer on its own (ROB-010-004): the grammar of
// mbox.go, its line endings, its bound, how far it looks, and that it consumes
// nothing. Every source is a finite string.

// mboxContent is a recognized envelope line before its line ending: 43 bytes.
const mboxContent = "From a@example.com Thu Jan  1 00:00:00 2026"

// countingSource delivers s one byte per read and counts what it delivered.
type countingSource struct {
	s         string
	delivered int
}

func (c *countingSource) Read(p []byte) (int, error) {
	if c.delivered >= len(c.s) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = c.s[c.delivered]
	c.delivered++
	return 1, nil
}

// TestMboxEnvelopeText pins the grammar of the line after "From " and before
// its line ending, one accepted and one refused form at a time.
func TestMboxEnvelopeText(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		// ctime, as RFC 4155's default format and most writers produce it.
		{"a@example.com Thu Jan  1 00:00:00 2026", true},
		{"MAILER-DAEMON Fri Jul  8 12:08:34 2011", true},
		{"- Mon Jan 01 00:00:00 2001", true},
		{"a:b Thu Jan 01 00:00:00 2026", true},
		{"a@example.com\tSat\tJan\t3\t01:05:34\t1996", true},
		{"a@example.com  Sat  Jan  3  01:05:34  1996", true},
		{"a@example.com Sun Dec 31 23:59:60 1999", true},
		{"a@example.com Wed Feb 31 00:00:00 2026", true}, // syntax, not a calendar
		{"a@example.com Thu Jan  1 00:00:00 0000", true},
		// One zone, before or after the year.
		{"1234567890123456789@xxx Thu Jan 01 00:00:00 +0000 2026", true},
		{"a@example.com Thu Jan 01 00:00:00 -2359 2026", true},
		{"a@example.com Thu Jan 01 00:00:00 2026 +2359", true},
		{"a@example.com Thu Jan 01 00:00:00 PST 2026", true},
		{"a@example.com Thu Jan 01 00:00:00 2026 UTC", true},
		{"a@example.com Thu Jan 01 00:00:00 2026 UT", true},
		{"a@example.com Thu Jan 01 00:00:00 2026 AEDST", true},
		// remote from, after the year and its zone.
		{"a@example.com Sat Jan  3 01:05:34 1996 remote from host.example", true},
		{"a@example.com Sat Jan  3 01:05:34 1996 GMT remote from host", true},
		{"a@example.com Sat Jan  3 01:05:34 +0100 1996 remote from h", true},

		// Not a sender.
		{"", false},
		{" a@example.com Thu Jan 01 00:00:00 2026", false},
		{"\ta@example.com Thu Jan 01 00:00:00 2026", false},
		{": a@example.com", false},
		{":a Thu Jan 01 00:00:00 2026", false},
		// Prose, and prose with a time in it.
		{"the desk of Jane", false},
		{"the 10:30 meeting", false},
		{"here on:", false},
		{"a@example.com", false},
		{"a@example.com Thu Jan 01 00:00:00", false},
		// Trailing white space, and anything unexplained after the year.
		{"a@example.com Thu Jan 01 00:00:00 2026 ", false},
		{"a@example.com Thu Jan 01 00:00:00 2026\t", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 extra words", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 x", false},
		// Weekday and month: exact, case-sensitive English names.
		{"a@example.com thu Jan 01 00:00:00 2026", false},
		{"a@example.com THU Jan 01 00:00:00 2026", false},
		{"a@example.com Thursday Jan 01 00:00:00 2026", false},
		{"a@example.com Thu JAN 01 00:00:00 2026", false},
		{"a@example.com Thu jan 01 00:00:00 2026", false},
		{"a@example.com Thu January 01 00:00:00 2026", false},
		{"a@example.com Jan Thu 01 00:00:00 2026", false},
		// Day: one or two digits, 1 to 31.
		{"a@example.com Thu Jan 0 00:00:00 2026", false},
		{"a@example.com Thu Jan 00 00:00:00 2026", false},
		{"a@example.com Thu Jan 32 00:00:00 2026", false},
		{"a@example.com Thu Jan 001 00:00:00 2026", false},
		{"a@example.com Thu Jan 1a 00:00:00 2026", false},
		// Time: HH:MM:SS within range.
		{"a@example.com Thu Jan 01 24:00:00 2026", false},
		{"a@example.com Thu Jan 01 00:60:00 2026", false},
		{"a@example.com Thu Jan 01 00:00:61 2026", false},
		{"a@example.com Thu Jan 01 0:00:00 2026", false},
		{"a@example.com Thu Jan 01 00:00 2026", false},
		{"a@example.com Thu Jan 01 00:00:00.123 2026", false},
		{"a@example.com Thu Jan 01 00-00-00 2026", false},
		// Year: four digits.
		{"a@example.com Thu Jan 01 00:00:00 26", false},
		{"a@example.com Thu Jan 01 00:00:00 20260", false},
		{"a@example.com Thu Jan 01 00:00:00 +2026", false},
		// Zones: one, written as stated.
		{"a@example.com Thu Jan 01 00:00:00 PST 2026 PST", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 PST PST", false},
		{"a@example.com Thu Jan 01 00:00:00 +0000 PST 2026", false},
		{"a@example.com Thu Jan 01 00:00:00 +2400 2026", false},
		{"a@example.com Thu Jan 01 00:00:00 +0060 2026", false},
		{"a@example.com Thu Jan 01 00:00:00 +000 2026", false},
		{"a@example.com Thu Jan 01 00:00:00 0000 2026", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 pst", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 Z", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 ABCDEF", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 (UTC)", false},
		// remote from: exact, last, with a host.
		{"a@example.com Thu Jan 01 00:00:00 2026 remote from", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 remote host", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 Remote from host", false},
		{"a@example.com Thu Jan 01 00:00:00 2026 remote from host x", false},
		{"a@example.com Thu Jan 01 00:00:00 remote from host 2026", false},
		// Another order.
		{"a@example.com 2026 Thu Jan 01 00:00:00", false},
		{"a@example.com Thu, 01 Jan 2026 00:00:00 +0000", false},
	} {
		if got := mboxEnvelopeText(tc.text); got != tc.want {
			t.Errorf("mboxEnvelopeText(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// TestMboxEnvelopeLen pins what the recognizer returns for whole inputs, over
// three deliveries, and that it consumes nothing.
func TestMboxEnvelopeLen(t *testing.T) {
	long := func(sender int) string {
		return "From " + strings.Repeat("a", sender) + " Thu Jan 01 00:00:00 2026"
	}
	if n := len(long(968)); n != 998 {
		t.Fatalf("fixture: %d bytes, want 998", n)
	}
	if len(mboxContent) != 43 {
		t.Fatalf("fixture: mboxContent has %d bytes, want 43", len(mboxContent))
	}
	for _, tc := range []struct {
		name  string
		input string
		limit int
		want  int
	}{
		{"crlf", mboxContent + "\r\nA: 1\r\n\r\n", 998, 45},
		{"lf", mboxContent + "\nA: 1\n\n", 998, 44},
		{"cr before a field", mboxContent + "\rA: 1\r\r", 998, 44},
		{"cr before an empty line", mboxContent + "\r\r\nbody", 998, 44},
		{"cr before a space", mboxContent + "\r A: 1", 998, 44},
		{"cr at eof", mboxContent + "\r", 998, 44},
		{"crlf at eof", mboxContent + "\r\n", 998, 45},
		{"lf at eof", mboxContent + "\n", 998, 44},
		{"lf after cr is one ending", mboxContent + "\r\n\n", 998, 45},
		{"remote from", "From a@example.com Sat Jan  3 01:05:34 1996 remote from host\n", 998, 61},
		{"gmail", "From 1234567890123456789@xxx Thu Jan 01 00:00:00 +0000 2026\r\n", 998, 61},

		{"no line ending", mboxContent, 998, 0},
		{"empty", "", 998, 0},
		{"From only", "From", 998, 0},
		{"From and a space", "From ", 998, 0},
		{"From and a space and lf", "From \n", 998, 0},
		{"header", "From: a@example.com\r\n\r\n", 998, 0},
		{"header with space before the colon", "From : a@example.com\r\n\r\n", 998, 0},
		{"two spaces", "From  a@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"tab", "From\ta@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"lower case", "from a@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"upper case", "FROM a@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"escaped", ">From a@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"indented", " From a@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"prose", "From the 10:30 meeting\nA: 1\n\n", 998, 0},
		{"utf-8 mark first", "\xef\xbb\xbf" + mboxContent + "\n", 998, 0},
		{"utf-16le mark first", "\xff\xfe" + mboxContent + "\n", 998, 0},
		{"8-bit sender", "From \xc3\xa9@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"nul", "From a\x00@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"del", "From a\x7f@example.com Thu Jan  1 00:00:00 2026\n", 998, 0},
		{"vertical tab", "From a@example.com\vThu Jan  1 00:00:00 2026\n", 998, 0},
		{"trailing space before crlf", mboxContent + " \r\n", 998, 0},

		// The bound: 998 bytes before the line ending, or a smaller limit.
		{"998 bytes", long(968) + "\n", 998, 999},
		{"999 bytes", long(969) + "\n", 998, 0},
		{"998 bytes crlf", long(968) + "\r\n", 998, 1000},
		{"limit of the line", mboxContent + "\n", 43, 44},
		{"one byte over the limit", mboxContent + "\n", 42, 0},
		{"limit below the prefix", mboxContent + "\n", 4, 0},
	} {
		for name, src := range map[string]func() io.Reader{
			"whole":         func() io.Reader { return strings.NewReader(tc.input) },
			"one byte":      func() io.Reader { return iotest.OneByteReader(strings.NewReader(tc.input)) },
			"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(tc.input)) },
		} {
			label := fmt.Sprintf("%s/%s", tc.name, name)
			br := bufio.NewReader(src())
			if got := mboxEnvelopeLen(br, tc.limit); got != tc.want {
				t.Errorf("%s: mboxEnvelopeLen() = %d, want %d", label, got, tc.want)
			}
			rest, err := io.ReadAll(br)
			if err != nil || string(rest) != tc.input {
				t.Errorf("%s: reader then yields (%q, %v), want the whole input unconsumed", label, rest, err)
			}
		}
	}
}

// TestMboxEnvelopeLen_lookAhead pins how far the recognizer reads: through the
// line ending of an envelope and no further, one byte past a '\r', and no
// further than the byte that rules a line out.
func TestMboxEnvelopeLen_lookAhead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		delivered int
	}{
		{"lf envelope", mboxContent + "\nA: 1\n\nbody", 44},
		{"crlf envelope", mboxContent + "\r\nA: 1\r\n\r\nbody", 45},
		{"cr envelope", mboxContent + "\rA: 1\r\rbody", 45},
		{"header", "From: a@example.com\r\n\r\nbody", 5},
		{"other first byte", "Subject: s\r\n\r\nbody", 1},
		{"8-bit byte", "From a\xc3\xa9 Thu Jan  1 00:00:00 2026\n\nbody", 7},
		{"prose", "From the desk of Jane\nA: 1\n\n", 22},
	} {
		src := &countingSource{s: tc.input}
		mboxEnvelopeLen(bufio.NewReader(src), maxMboxEnvelopeContent)
		if src.delivered != tc.delivered {
			t.Errorf("%s: %d bytes read, want %d", tc.name, src.delivered, tc.delivered)
		}
	}
}

// TestMboxEnvelope_value pins the zero MboxEnvelope and a nil session.
func TestMboxEnvelope_value(t *testing.T) {
	var zero MboxEnvelope
	if zero.Bytes() != nil || zero.Location() != (Location{}) {
		t.Errorf("zero MboxEnvelope = (%q, %+v), want nothing", zero.Bytes(), zero.Location())
	}
	var s *ParseSession
	if env, ok := s.MboxEnvelope(); ok || env.Bytes() != nil {
		t.Errorf("nil session MboxEnvelope() = (%q, %v), want none", env.Bytes(), ok)
	}
	if env, ok := (&ParseSession{}).MboxEnvelope(); ok || env.Bytes() != nil {
		t.Errorf("zero session MboxEnvelope() = (%q, %v), want none", env.Bytes(), ok)
	}
}

// mboxReference is the grammar of mbox.go written as a regular expression, an
// implementation independent of the recognizer's, over a line without its
// line ending. RE2 matches in linear time.
var mboxReference = regexp.MustCompile(`^From [!-9;-~][!-~]*[ \t]+` +
	`(Mon|Tue|Wed|Thu|Fri|Sat|Sun)[ \t]+` +
	`(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[ \t]+` +
	`(0?[1-9]|[12][0-9]|3[01])[ \t]+` +
	`([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)[ \t]+` +
	`(` + mboxZoneRE + `[ \t]+[0-9]{4}|[0-9]{4}([ \t]+` + mboxZoneRE + `)?)` +
	`([ \t]+remote[ \t]+from[ \t]+[!-~]+)?$`)

const mboxZoneRE = `([A-Z]{2,5}|[+-]([01][0-9]|2[0-3])[0-5][0-9])`

// mboxReferenceLen is what mboxEnvelopeLen must return for the whole input
// data under limit, from the reference grammar.
func mboxReferenceLen(data string, limit int) int {
	i := strings.IndexAny(data, "\r\n")
	if i < 0 || i > limit || !mboxReference.MatchString(data[:i]) {
		return 0
	}
	if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
		return i + 2
	}
	return i + 1
}

// FuzzMboxEnvelopeLen checks the recognizer alone, decoding nothing: for any
// input of at most 2 KiB and a bound of 1 to 56 bytes or the default 998, it
// returns exactly the reference grammar's length, consumes nothing, and reads
// at most max(5, limit+2) bytes from a source that delivers one byte per
// read — for a recognized line, exactly the line, and one byte more after a
// bare '\r' that is not the input's last byte.
func FuzzMboxEnvelopeLen(f *testing.F) {
	for _, seed := range []string{
		mboxContent + "\r\nA: 1\r\n\r\n",
		mboxContent + "\nA: 1\n\n",
		mboxContent + "\rA: 1\r\r",
		mboxContent + "\r",
		mboxContent,
		"From - Mon Jan 01 00:00:00 2001\n",
		"From 1234567890123456789@xxx Thu Jan 01 00:00:00 +0000 2026\r\n",
		"From a@example.com Sat Jan  3 01:05:34 1996 GMT remote from host\n",
		"From the 10:30 meeting\n",
		"From: a@example.com\r\n",
		"From : a@example.com\r\n",
		"\xef\xbb\xbf" + mboxContent + "\n",
		"From " + strings.Repeat("a", 969) + " Thu Jan 01 00:00:00 2026\n",
	} {
		f.Add([]byte(seed), uint8(0))
		f.Add([]byte(seed), uint8(200+38))
	}
	f.Fuzz(func(t *testing.T, data []byte, limitSel uint8) {
		if len(data) > 2048 {
			return
		}
		limit := maxMboxEnvelopeContent
		if limitSel >= 200 {
			limit = int(limitSel) - 199
		}
		text := string(data)
		src := &countingSource{s: text}
		br := bufio.NewReader(src)
		n := mboxEnvelopeLen(br, limit)
		if want := mboxReferenceLen(text, limit); n != want {
			t.Fatalf("mboxEnvelopeLen(%q, %d) = %d, want %d", text, limit, n, want)
		}
		if bound := min(len(text), max(5, limit+2)); src.delivered > bound {
			t.Fatalf("%d bytes read, want at most %d", src.delivered, bound)
		}
		if n > 0 {
			want := n
			if text[n-1] == '\r' && n < len(text) {
				want++
			}
			if src.delivered != want {
				t.Fatalf("%d bytes read for a %d-byte line, want %d", src.delivered, n, want)
			}
		}
		rest, err := io.ReadAll(br)
		if err != nil || string(rest) != text {
			t.Fatalf("reader then yields (%q, %v), want the whole input unconsumed", rest, err)
		}
	})
}
