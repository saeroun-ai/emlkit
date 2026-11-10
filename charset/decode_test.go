package charset

import (
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// ROB-080-008: what the decoders do with input that is not valid in its
// charset, and what that costs. ROB-080-004: byte order marks.

// TestReader_malformedSequencesBecomeReplacementCharacters: a sequence the
// charset does not allow is U+FFFD, and the text on both sides of it is
// decoded. No decoder returns an error (decodeAll fails the test on one).
func TestReader_malformedSequencesBecomeReplacementCharacters(t *testing.T) {
	for _, c := range []struct{ label, in, want string }{
		// A first byte cut off by a line break, by the end, and an invalid
		// byte between two characters.
		{"shift_jis", "ab\x93\r\ncd", "ab�\r\ncd"},
		{"shift_jis", "ab\x93", "ab�"},
		{"shift_jis", "\x93\xfa\xff\x96\x7b", "日�本"},
		{"euc-jp", "ab\xc6\r\ncd", "ab�\r\ncd"},
		{"euc-jp", "ab\xc6", "ab�"},
		{"euc-jp", "\xc6\xfc\xff\xcb\xdc", "日�本"},
		{"euc-jp", "\xc6\xfc\x8f\xb0", "日�"},
		{"euc-kr", "ab\xbe\r\ncd", "ab�\r\ncd"},
		{"euc-kr", "ab\xbe", "ab�"},
		{"cp949", "\xbe\xc8\xff\xb3\xe7", "안�녕"},
		{"gbk", "ab\xb2\r\ncd", "ab�\r\ncd"},
		{"gb2312", "ab\xb2", "ab�"},
		{"gbk", "\xb2\xe2\xff\xca\xd4", "测�试"},
		{"gb18030", "ab\x81\x30\r\ncd", "ab�0\r\ncd"},
		{"gb18030", "\xb2\xe2\x81\x30\x81", "测�0�"},
		{"big5", "ab\xa4\r\ncd", "ab�\r\ncd"},
		{"big5", "ab\xa4", "ab�"},
		{"big5", "\xb4\xfa\xff\xb8\xd5", "測�試"},

		// Unpaired surrogates and a trailing odd byte.
		{"utf-16le", "A\x00\x00\xd8B\x00C", "A�B�"},
		{"utf-16be", "\x00A\xdc\x00\x00B\x00", "A�B�"},
		{"utf-16", "\xff\xfeA\x00\x00\xd8B\x00C", "A�B�"},
		{"utf-16", "\xff", "�"},
		// A code point beyond U+10FFFF and a truncated one.
		{"utf-32be", "\x00\x00\x00A\x00\x11\x00\x00\x00\x00\x00B\x00", "A�B�"},
		// Invalid and truncated UTF-8 under a label the plugin decodes.
		{"utf8", "ab\xff\xc3 z\xe2\x82", "ab�� z�"},
		// The five bytes windows-1252 does not define.
		{"windows-1252", "a\x81\x8d\x8f\x90\x9db", "a�����b"},
	} {
		if got := decodeAll(t, c.label, c.in); got != c.want {
			t.Errorf("%s: %q decoded to %q, want %q", c.label, c.in, got, c.want)
		}
	}
}

// TestReader_widenedLatin: iso-8859-1 and iso-8859-9 are read as windows-1252
// and windows-1254, and still give every byte a character of its own: where
// the code page has none, the ISO control code stays.
func TestReader_widenedLatin(t *testing.T) {
	var all [256]byte
	for i := range all {
		all[i] = byte(i)
	}
	for _, c := range []struct {
		label, windows string
		holes          string // the bytes the code page leaves undefined
		differs        string // bytes where the ISO part and the code page differ outside 0x80-0x9F
	}{
		{"iso-8859-1", "windows-1252", "\x81\x8d\x8f\x90\x9d", ""},
		{"iso-8859-9", "windows-1254", "\x81\x8d\x8e\x8f\x90\x9d\x9e", ""},
	} {
		got := []rune(decodeAll(t, c.label, string(all[:])))
		want := []rune(decodeAll(t, c.windows, string(all[:])))
		if len(got) != 256 || len(want) != 256 {
			t.Fatalf("%s: %d and %d characters for 256 bytes", c.label, len(got), len(want))
		}
		seen := make(map[rune]bool)
		for i := range 256 {
			switch hole := strings.IndexByte(c.holes, byte(i)) >= 0; {
			case hole && (want[i] != utf8.RuneError || got[i] != rune(i)):
				t.Errorf("%s: byte %#x is %U (%s has %U), want the control code %U", c.label, i, got[i], c.windows, want[i], i)
			case !hole && got[i] != want[i]:
				t.Errorf("%s: byte %#x is %U, want %U as in %s", c.label, i, got[i], want[i], c.windows)
			}
			if seen[got[i]] || got[i] == utf8.RuneError {
				t.Errorf("%s: byte %#x decodes to %U, which is not its own character", c.label, i, got[i])
			}
			seen[got[i]] = true
		}
	}

	// The same reading under the labels that name no charset.
	if got := decodeAll(t, "x-unknown", "a\x81\xe9\xe2\x82b"); got != "a\u0081éâ‚b" {
		t.Errorf("x-unknown decoded to %q", got)
	}
}

// TestReader_unknown8bit: the decoder behind us-ascii and the labels that
// name no charset. Valid UTF-8 passes through byte for byte.
func TestReader_unknown8bit(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"plain ascii\r\n", "plain ascii\r\n"},
		{"한글 😀 \xef\xbb\xbf�", "한글 😀 \xef\xbb\xbf�"},
		{"caf\xe9", "café"},
		{"\x93smart\x94 \x80 5\xa0\xb0", "“smart” € 5\xc2\xa0°"},
		{"\xc3\xa9\xe9\xc3", "ééÃ"},         // UTF-8, windows-1252, a truncated sequence
		{"\xed\xa0\x80", "í\xc2\xa0€"},      // a surrogate is not UTF-8
		{"\xc0\xaf\xf5\x80", "À¯õ€"},        // nor are overlong forms and bytes past U+10FFFF
		{"\xf0\x9f\x98", "ðŸ˜"},             // truncated at the end
		{"a\xe2\x82\xacb\xe2\x82", "a€bâ‚"}, // the same bytes, complete and cut
	} {
		for _, label := range []string{"unknown-8bit", "us-ascii", ""} {
			if got := decodeAll(t, label, c.in); got != c.want {
				t.Errorf("%q: %q decoded to %q, want %q", label, c.in, got, c.want)
			}
		}
	}
}

// utf16Bytes is s in UTF-16 with the given byte order, built with the
// standard library, not with the decoder under test.
func utf16Bytes(s string, littleEndian bool) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		if littleEndian {
			b = append(b, byte(u), byte(u>>8))
		} else {
			b = append(b, byte(u>>8), byte(u))
		}
	}
	return string(b)
}

// TestReader_utf16ByteOrder: a byte order mark decides and is consumed; without
// one the generic labels look at the text, and big-endian is the answer when
// the text does not say.
func TestReader_utf16ByteOrder(t *testing.T) {
	const (
		korean  = "수요일 회의 자료를 첨부합니다. 확인 부탁드립니다."
		hangul  = "수요일회의자료를첨부합니다확인부탁드립니다감사합니다좋은하루보내세요"
		chinese = "一二三四五六七八九十，最开始的时候。"
	)
	long := strings.Repeat(korean, 5) // more than the 128 bytes that are examined
	for _, c := range []struct{ name, label, in, want string }{
		{"LE mark", "utf-16", "\xff\xfe" + utf16LE, hiText},
		{"BE mark", "utf-16", "\xfe\xff" + utf16BE, hiText},
		{"LE mark, text that looks big-endian", "utf-16", "\xff\xfe\x00H\x00i", "䠀椀"},
		// What .NET sends as charset=utf-16: little-endian, no mark.
		{"LE unmarked, ASCII and Hangul", "utf-16", utf16LE, hiText},
		{"LE unmarked, two letters", "unicode", "H\x00i\x00", "Hi"},
		{"LE unmarked, Hangul and spaces", "utf-16", utf16Bytes(korean, true), korean},
		{"LE unmarked, Hangul only", "utf-16", utf16Bytes(hangul, true), hangul},
		{"LE unmarked, CJK only", "utf-16", utf16Bytes(chinese, true), chinese},
		{"LE unmarked, long", "utf-16", utf16Bytes(long, true), long},
		// RFC 2781 text: big-endian, no mark.
		{"BE unmarked, ASCII and Hangul", "utf-16", utf16BE, hiText},
		{"BE unmarked, two letters", "utf-16", "\x00H\x00i", "Hi"},
		{"BE unmarked, Hangul and spaces", "utf-16", utf16Bytes(korean, false), korean},
		{"BE unmarked, Hangul only", "unicode", utf16Bytes(hangul, false), hangul},
		{"BE unmarked, CJK only", "ucs-2", utf16Bytes(chinese, false), chinese},
		{"BE unmarked, long", "unicode", utf16Bytes(long, false), long},
		// 一 is 0x4E 0x00 in big-endian and looks like "N" in little-endian;
		// the two characters beside it settle it.
		{"BE unmarked, bytes that end in NUL", "utf-16", "\x4e\x00\x4e\x8c\x4e\x09", "一二三"},
		// Nothing to go by: big-endian for utf-16, as RFC 2781 says, and
		// little-endian for the Windows names, as the WHATWG registry says.
		{"undecidable", "utf-16", utf16Bytes("안녕", false), "안녕"},
		{"undecidable, Windows name", "unicode", utf16Bytes("안녕", true), "안녕"},
		{"BE unmarked, Windows name", "unicode", utf16BE, hiText},
		{"BE mark, Windows name", "ucs-2", "\xfe\xff" + utf16BE, hiText},
		{"empty", "utf-16", "", ""},

		// An explicit byte order is taken at its word, and a mark is text.
		{"utf-16le is not examined", "utf-16le", "\x00H\x00i", "䠀椀"},
		{"utf-16be is not examined", "utf-16be", "H\x00i\x00", "䠀椀"},
		{"utf-16le keeps the mark", "utf-16le", "\xff\xfeA\x00", "\xef\xbb\xbfA"},
		{"utf-16be keeps the mark", "utf-16be", "\xfe\xff\x00A", "\xef\xbb\xbfA"},

		{"utf-32 LE mark", "utf-32", "\xff\xfe\x00\x00" + utf32LE, hiText},
		{"utf-32 BE mark", "utf-32", "\x00\x00\xfe\xff" + utf32BE, hiText},
		{"utf-32 unmarked is big-endian", "utf-32", utf32BE, hiText},
		{"utf-32le keeps the mark", "utf-32le", "\xff\xfe\x00\x00A\x00\x00\x00", "\xef\xbb\xbfA"},

		// utf-8: the mark is kept, as message does for the utf-8 it passes
		// through itself.
		{"utf8 keeps the mark", "utf8", "\xef\xbb\xbfab", "\xef\xbb\xbfab"},
		{"unknown-8bit keeps the mark", "x-unknown", "\xef\xbb\xbfab", "\xef\xbb\xbfab"},
	} {
		if got := decodeAll(t, c.label, c.in); got != c.want {
			t.Errorf("%s: %s of %q decoded to %q, want %q", c.name, c.label, c.in, got, c.want)
		}
	}
}

// A repeater is an endless source of one unit of text, to be read through an
// io.LimitedReader. It counts the reads it serves.
type repeater struct {
	unit  string
	at    int
	reads int
}

func (r *repeater) Read(p []byte) (int, error) {
	r.reads++
	for i := range p {
		p[i] = r.unit[r.at]
		r.at = (r.at + 1) % len(r.unit)
	}
	return len(p), nil
}

// A meter is a sink that keeps the size of what is written and whether all of
// it was valid UTF-8, a write at a time, without keeping the text.
type meter struct {
	n       int64
	invalid bool
	tail    []byte // an incomplete character at the end of the last write
}

func (m *meter) Write(p []byte) (int, error) {
	m.n += int64(len(p))
	b := append(m.tail, p...)
	for len(b) > 0 && utf8.FullRune(b) {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			m.invalid = true
		}
		b = b[size:]
	}
	m.tail = append(m.tail[:0], b...)
	return len(p), nil
}

// decodeFlood decodes size bytes of unit repeated and returns the output's
// size and the number of reads the decoder made of its input. The input is
// capped by an io.LimitedReader and nothing is accumulated.
func decodeFlood(t *testing.T, label, unit string, size int64) (out int64, reads int) {
	t.Helper()
	src := &repeater{unit: unit}
	r, err := Reader(label, &io.LimitedReader{R: src, N: size})
	if err != nil {
		t.Fatalf("Reader(%q) = %v", label, err)
	}
	var m meter
	buf := make([]byte, 4096)
	// A capped loop: a decoder that stopped making progress fails the test
	// here instead of spinning.
	for reads := 0; ; reads++ {
		n, err := r.Read(buf)
		m.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil || reads > int(size) {
			t.Fatalf("%s flood of %q: read %d returned (%d, %v) with %d bytes out", label, unit, reads, n, err, m.n)
		}
	}
	if m.invalid || len(m.tail) != 0 {
		t.Errorf("%s flood of %q: output is not valid UTF-8", label, unit)
	}
	return m.n, src.reads
}

// TestReader_escapeFloodsAreLinear: sequences that change a decoder's state
// and produce nothing cost input and nothing else. Each flood is 256 KiB,
// generated as it is read and capped twice, by an io.LimitedReader and by a
// bound on the reads; nothing is kept. The decoder must take it in
// buffer-sized reads, which it does only if it consumes what it is given, and
// emit exactly what is expected.
func TestReader_escapeFloodsAreLinear(t *testing.T) {
	const size = 72 * 3640 // 256 KiB less 64 bytes: a whole number of every unit below
	for _, c := range []struct {
		label, unit string
		want        int64 // output bytes, or -1 for whatever stays within bounds
	}{
		{"iso-2022-jp", "\x1b$B", 0},
		{"iso-2022-jp", "\x1b$B\x1b(B", 0},
		{"iso-2022-jp", "\x1b$(D\x1b(I\x0e\x0f", 0},
		{"iso-2022-jp", "\x1b", size * 3},        // unknown escapes: U+FFFD each
		{"iso-2022-jp", "\x1b$B!", size / 4 * 3}, // a lone first byte before each escape
		{"iso-2022-kr", "\x0e\x0f", 0},           // shift out, shift in
		{"iso-2022-kr", "\x1b$)C", 0},            // the designation, over and over
		{"hz-gb-2312", "~{~}", 0},                // into GB and out again
		{"hz-gb-2312", "~{", 0},                  //
		{"hz-gb-2312", "~\n", 0},                 // line continuations
		{"utf-7", "+-", size / 2},                // plus signs
		{"utf-7", "+", size / 8 * 9},             // one endless run: '+' is a base64 character too
		{"utf-7", "+2AA ", size},                 // a lone surrogate in every run: kept as text
		{"utf-7", "+AAAAAAA ", size},             // runs that do not end on a code unit: kept as text
		{"utf-7", "+A ", size},                   //
		{"utf-7", "+2AA", -1},                    // the same without an end: one run, '+' being base64
		// As many shifts as possible, each before a run that only fails at
		// its end: every window of 256 has the unpaired surrogates in it.
		{"utf-7", strings.Repeat("+A", 116) + "22222222", -1},
		{"utf-16be", "\xd8\x00", size / 2 * 3},     // unpaired surrogates
		{"gb18030", "\x81\x30", -1},                // four-byte sequences cut in half
		{"euc-jp", "\x8f", size * 3},               // first bytes with nothing after them
		{"shift_jis", "\x81", -1},                  //
		{"big5", "\xa4", -1},                       //
		{"cp949", "\x81", -1},                      //
		{"unknown-8bit", "\xf0\x9f\x98", size * 2}, // UTF-8 cut short: ð, Ÿ and ˜, two bytes each
	} {
		out, reads := decodeFlood(t, c.label, c.unit, size)
		if c.want >= 0 && out != c.want {
			t.Errorf("%s flood of %q: %d bytes out, want %d", c.label, c.unit, out, c.want)
		}
		if out > 3*size {
			t.Errorf("%s flood of %q: %d bytes out of %d in, more than threefold", c.label, c.unit, out, size)
		}
		// The x/text reader fills a 4096-byte buffer; allow for short refills.
		if reads > size/1024 {
			t.Errorf("%s flood of %q: %d reads for %d bytes", c.label, c.unit, reads, size)
		}
	}
}

// TestReader_expansionIsBounded runs every decoder of the registry over bytes
// chosen to be invalid, or as wide as possible, in some charset: the output is
// valid UTF-8 and never more than three times the input.
func TestReader_expansionIsBounded(t *testing.T) {
	const size = 16 << 10
	units := []string{
		"\xff", "\x80", "\x81", "\xa1", "\x8e", "\x8f", "\xfe\xfe", "\x1b", "~", "+", "\x0e",
		"\x00\xd8", "\xd8\x00", "\x00", "\xe2\x82", "\xca", "\x1b$B\x21", "~{\x21", "+2A", "\xa1\xa1",
	}
	for _, f := range families {
		label := strings.Fields(f.labels)[0]
		for _, unit := range units {
			if out, _ := decodeFlood(t, label, unit, size); out > 3*size {
				t.Errorf("%s over %q: %d bytes out of %d in, more than threefold", label, unit, out, size)
			}
		}
	}
}

var errCut = errors.New("cut")

// A script is a reader that plays a fixed sequence of read results, then
// fails the test's expectations loudly instead of blocking.
type script struct {
	steps []step
	calls int
}

type step struct {
	data  string
	err   error
	empty int // this many (0, nil) reads first
}

func (s *script) Read(p []byte) (int, error) {
	s.calls++
	for len(s.steps) > 0 {
		st := &s.steps[0]
		if st.empty > 0 {
			st.empty--
			return 0, nil
		}
		if st.data != "" {
			n := copy(p, st.data)
			st.data = st.data[n:]
			return n, nil
		}
		s.steps = s.steps[1:]
		if st.err != nil {
			return 0, st.err
		}
	}
	return 0, io.EOF
}

// TestReader_inputErrors: the decoder's only errors are its input's, returned
// unchanged after everything decoded before them.
func TestReader_inputErrors(t *testing.T) {
	for _, c := range []struct {
		label, in, want string
	}{
		{"euc-kr", "\xbe\xc8\xb3\xe7\xc7", "안녕"}, // cut inside a character
		{"iso-2022-jp", "\x1b$BF|K\x5c8", "日本"},
		{"iso-2022-kr", "\x1b$)C\x0eGQ1", "한"},
		{"hz-gb-2312", "~{2bJT~", "测试"},
		{"utf-7", "caf+AOk-+AO", "café"},
		{"windows-1252", "caf\xe9", "café"},
		{"x-unknown", "caf\xe9\xe2\x82", "café"},
		{"utf-16", "\xff\xfeH\x00i\x00\x00", "Hi"},
	} {
		r, err := Reader(c.label, &script{steps: []step{{data: c.in, err: errCut}}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		if string(b) != c.want || err != errCut {
			t.Errorf("%s: %q then an error decoded to (%q, %v), want (%q, %v)", c.label, c.in, b, err, c.want, errCut)
		}
	}
}

// TestReader_stalledInput: an input that returns no bytes and no error for
// ever ends the read with io.ErrNoProgress after maxEmptyReads reads, where
// the x/text reader alone would spin; one that only hesitates is read on.
func TestReader_stalledInput(t *testing.T) {
	for _, label := range []string{"euc-kr", "iso-2022-jp", "utf-7", "utf-16", "x-unknown"} {
		// The stall is capped far above the limit, so that a regression ends
		// in the wrong error and not in a hang.
		src := &script{steps: []step{{data: "ab"}, {empty: 4 * maxEmptyReads, err: errCut}}}
		r, err := Reader(label, src)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		if err != io.ErrNoProgress || src.calls > maxEmptyReads+2 {
			t.Errorf("%s: stalled input ended with (%q, %v) after %d reads, want io.ErrNoProgress after %d",
				label, b, err, src.calls, maxEmptyReads)
		}
	}

	// Fewer empty reads than the limit, again and again, are not a stall.
	var steps []step
	for range 8 {
		steps = append(steps, step{empty: maxEmptyReads - 1, data: "\xbe\xc8"})
	}
	r, err := Reader("cp949", &script{steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(r); string(b) != strings.Repeat("안", 8) || err != nil {
		t.Errorf("hesitating input decoded to (%q, %v)", b, err)
	}
}
