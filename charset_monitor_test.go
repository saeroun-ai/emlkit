package message

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParse_utf8BodyWatched(t *testing.T) {
	for _, tc := range []struct {
		name, charset, body string
		codes               []Code
	}{
		{"valid", "utf-8", "caf\xc3\xa9 \xe2\x82\xac \xf0\x9f\x98\x80 end", nil},
		{"ascii", "UTF-8", "plain", nil},
		{"invalid byte", "utf-8", "caf\xe9 au lait", []Code{CodeCharsetInvalidSequence}},
		{"truncated at the end", "utf-8", "caf\xc3", []Code{CodeCharsetInvalidSequence}},
		{"overlong", "utf-8", "a\xc0\xafb", []Code{CodeCharsetInvalidSequence}},
		{"utf-16 mark", "utf-8", "\xff\xfeh\x00i\x00", []Code{CodeCharsetBOMMismatch, CodeCharsetInvalidSequence}},
		// Delivered as read too, and reported by what was declared: see
		// TestParse_undeclaredBodyWatched.
		{"declared us-ascii", "us-ascii", "caf\xe9", []Code{CodeCharsetDeclarationSuspect}},
	} {
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"whole":    func(r io.Reader) io.Reader { return r },
			"one byte": iotest.OneByteReader,
		} {
			text := "Content-Type: text/plain; charset=" + tc.charset + "\r\n\r\n" + tc.body
			s, e, err := Parse(wrap(strings.NewReader(text)), nil)
			if err != nil {
				t.Fatal(err)
			}
			b, rerr := io.ReadAll(e.Body)
			if string(b) != tc.body || rerr != nil {
				t.Errorf("%s/%s: body (%q, %v), want every byte as read", tc.name, name, b, rerr)
			}
			if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("%s/%s: diagnostics %v, want %v", tc.name, name, codes, tc.codes)
			}
		}
	}
}
