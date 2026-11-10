package charset

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// The 7-bit charsets of iso2022.go (ROB-080-005, ROB-080-008).

// A lcg is a fixed pseudo-random sequence: the same text on every run.
type lcg uint32

func (g *lcg) next(n int) int {
	*g = *g*1664525 + 1013904223
	return int(*g>>16) % n
}

// graphic returns n pseudo-random bytes in lo..hi.
func (g *lcg) graphic(n int, lo, hi byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = lo + byte(g.next(int(hi-lo)+1))
	}
	return string(b)
}

// TestISO2022_conformantTextDecodesAsBefore: on text that follows the RFCs,
// the decoders of this package and those of golang.org/x/text they replace
// produce the same output, whatever the characters. The text is generated,
// about 100 KiB for each charset: every double-byte value, assigned or not,
// in every state, with every line ending in ASCII as the RFCs require.
func TestISO2022_conformantTextDecodesAsBefore(t *testing.T) {
	g := lcg(1)
	ascii := func() string {
		// Printable ASCII without the tilde, which HZ escapes.
		return g.graphic(1+g.next(20), 0x20, 0x7D)
	}

	var jis strings.Builder
	for jis.Len() < 100<<10 {
		switch g.next(8) {
		case 0:
			jis.WriteString("\x1b$B" + g.graphic(2*(1+g.next(12)), 0x21, 0x7E) + "\x1b(B")
		case 1:
			jis.WriteString("\x1b$@" + g.graphic(2*(1+g.next(12)), 0x21, 0x7E) + "\x1b(J")
		case 2:
			jis.WriteString("\x1b$(D" + g.graphic(2*(1+g.next(12)), 0x21, 0x7E) + "\x1b(B")
		case 3:
			jis.WriteString("\x1b(I" + g.graphic(1+g.next(12), 0x21, 0x5F) + "\x1b(B")
		case 4:
			// One set straight after another, without ASCII between.
			jis.WriteString("\x1b$B" + g.graphic(4, 0x21, 0x7E) + "\x1b(I" + g.graphic(3, 0x21, 0x5F) +
				"\x1b$(D" + g.graphic(4, 0x21, 0x7E) + "\x1b(B")
		case 5:
			jis.WriteString("\r\n")
		default:
			jis.WriteString(ascii())
		}
	}

	var hz strings.Builder
	for hz.Len() < 100<<10 {
		switch g.next(8) {
		case 0, 1, 2:
			// GB 2312 rows end at 0x7D in HZ: 0x7E is the tilde.
			pairs := make([]byte, 0, 24)
			for range 1 + g.next(12) {
				pairs = append(pairs, g.graphic(1, 0x21, 0x7D)[0], g.graphic(1, 0x21, 0x7E)[0])
			}
			hz.WriteString("~{" + string(pairs) + "~}")
		case 3:
			hz.WriteString("~~")
		case 4:
			hz.WriteString("~\n")
		case 5:
			hz.WriteString("\r\n")
		default:
			hz.WriteString(ascii())
		}
	}

	for _, c := range []struct {
		label string
		text  string
		was   encoding.Encoding
	}{
		{"iso-2022-jp", jis.String(), japanese.ISO2022JP},
		{"hz-gb-2312", hz.String(), simplifiedchinese.HZGB2312},
	} {
		want, err := c.was.NewDecoder().String(c.text)
		if err != nil {
			t.Fatalf("%s: golang.org/x/text: %v", c.label, err)
		}
		got := decodeAll(t, c.label, c.text)
		if got == want {
			continue
		}
		// Report the first difference, not 100 KiB of text.
		i := 0
		for i < len(got) && i < len(want) && got[i] == want[i] {
			i++
		}
		t.Errorf("%s: output differs from golang.org/x/text at byte %d of %d: got %q, want %q",
			c.label, i, len(want), got[max(0, i-20):min(len(got), i+20)], want[max(0, i-20):min(len(want), i+20)])
	}
}

// TestISO2022_brokenFraming: where text does not follow the RFCs, the decoders
// keep their place. Each case notes what golang.org/x/text makes of it.
func TestISO2022_brokenFraming(t *testing.T) {
	for _, c := range []struct{ label, in, want string }{
		// A line that ends inside a JIS X 0208 run: the escape back to ASCII is
		// missing or was cut off. x/text reads CR LF as one character and the
		// ASCII lines after it as ideographs ("ab日�邃�繙").
		{"iso-2022-jp", "ab\x1b$BF|\r\ncd\r\nef", "ab日\r\ncd\r\nef"},
		{"iso-2022-jp", "ab\x1b$BF|\ncd", "ab日\ncd"},
		// The same with the line cut in the middle of a character.
		{"iso-2022-jp", "ab\x1b$BF|F\r\ncd\r\nef", "ab日�\r\ncd\r\nef"},
		{"iso-2022-jp", "ab\x1b$BF", "ab�"},
		// A character cut short before an escape: the escape still counts.
		// x/text takes ESC for the second byte and prints "(Bcd" as text.
		{"iso-2022-jp", "ab\x1b$BF|F\x1b(Bcd", "ab日�cd"},
		// A space and a tab inside a run are themselves.
		{"iso-2022-jp", "\x1b$BF| F|\tF|\x1b(B", "日 日\t日"},
		// Katakana to the end of the line (x/text: "ｱｲ��z").
		{"iso-2022-jp", "\x1b(I12\r\n\x1b(Bz", "ｱｲ\r\nz"},
		{"iso-2022-jp", "\x1b(I1\x7e2\x1b(B", "ｱ�ｲ"},
		// Katakana shifted in and out, as code page 50222 writes it.
		{"cp50222", "a\x0e12\x0fb", "aｱｲb"},
		{"iso-2022-jp", "\x1b$BF|\x0e1\x0fF|\x1b(B", "日ｱ日"},
		// The four-byte forms of the JIS X 0208 escapes.
		{"iso-2022-jp", "\x1b$(BF|\x1b(B \x1b$(@F|\x1b(B", "日 日"},
		// Escape sequences that designate nothing known, or are cut off.
		{"iso-2022-jp", "ab\x1b", "ab�"},
		{"iso-2022-jp", "ab\x1b$", "ab�$"},
		{"iso-2022-jp", "ab\x1b$(", "ab�$("},
		{"iso-2022-jp", "ab\x1bXYz", "ab�XYz"},
		{"iso-2022-jp", "ab\x1b$Az", "ab�$Az"}, // GB 2312 of ISO-2022-JP-2: not supported
		// A byte above 0x7F is not ISO-2022-JP in any state.
		{"iso-2022-jp", "a\xa4\xffz\x1b$B\xa4F|\x1b(B", "a��z�日"},

		// HZ. A line that ends inside a GB run (x/text: "ab测�沅�彐").
		{"hz-gb-2312", "ab~{2b\r\ncd\r\nef", "ab测\r\ncd\r\nef"},
		{"hz-gb-2312", "ab~{2b2\r\ncd", "ab测�\r\ncd"},
		{"hz-gb-2312", "ab~{2", "ab�"},
		{"hz-gb-2312", "~{2b JT~}", "测 试"},
		// A tilde that starts no escape is a tilde outside a run (x/text
		// swallows the character after it: "http://example.org/�ser").
		{"hz-gb-2312", "http://example.org/~user", "http://example.org/~user"},
		{"hz-gb-2312", "ab~", "ab~"},
		{"hz-gb-2312", "~{2b~", "测�"},
		// A line continuation, with either line ending.
		{"hz-gb-2312", "ab~\ncd~\r\nef", "abcdef"},
		{"hz-gb-2312", "ab~\rcd", "ab~\rcd"},
		{"hz-gb-2312", "a\xb2\xe2z", "a��z"},

		// ISO-2022-KR. The shift is over at the end of the line.
		{"iso-2022-kr", "\x1b$)C\x0eGQ1[\r\nab", "한글\r\nab"},
		{"iso-2022-kr", "\x0eGQ1\x0fab", "한�ab"},
		{"iso-2022-kr", "\x0eG", "�"},
		{"iso-2022-kr", "\x0eGQ 1[\x0f", "한 글"},
		// The designation is needed for nothing, and may come late or twice.
		{"iso-2022-kr", "\x0eGQ\x0f \x1b$)C\x1b$)C\x0e1[\x0f", "한 글"},
		{"iso-2022-kr", "a\x1bXb\x1b$)", "a�Xb�$)"},
		// 8-bit Korean under the 7-bit label is read as code page 949.
		{"iso-2022-kr", "\xbe\xc8\xb3\xe7 ab \x8cc", "안녕 ab 똠"},
		{"iso-2022-kr", "a\xff\x80z", "a��z"},
	} {
		if got := decodeAll(t, c.label, c.in); got != c.want {
			t.Errorf("%s: %q decoded to %q, want %q", c.label, c.in, got, c.want)
		}
	}
}

// TestISO2022_encoders: a charset golang.org/x/text can encode is encoded by
// it, as before; ISO-2022-KR, which it cannot, says so instead of writing
// something else.
func TestISO2022_encoders(t *testing.T) {
	m, err := Lookup("iso-2022-jp")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.Encoding.NewEncoder().String("日本"); err != nil || got != "\x1b$BF|K\\\x1b(B" {
		t.Errorf("iso-2022-jp encoder: (%q, %v)", got, err)
	}
	if m, err = Lookup("iso-2022-kr"); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Encoding.NewEncoder().String("한글"); err == nil {
		t.Errorf("iso-2022-kr encoder returned %q, want an error", got)
	}
	if got, err := m.Encoding.NewEncoder().String(""); err != nil || got != "" {
		t.Errorf("iso-2022-kr encoder on nothing: (%q, %v)", got, err)
	}
}
