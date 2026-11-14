package charset_test

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	_ "github.com/saeroun-ai/emlkit/charset"
)

// TestParse_fallbackCharset pins ParseOptions.FallbackCharset (ROB-080-007):
// a text body that declares no charset, or declares UTF-8 or US-ASCII, and
// whose first bytes are not UTF-8 is decoded with the fallback and reported;
// a body that is UTF-8 is never touched; and with no fallback nothing is
// guessed.
func TestParse_fallbackCharset(t *testing.T) {
	// "안녕하세요" in EUC-KR and in UTF-8.
	const eucKR = "\xbe\xc8\xb3\xe7\xc7\xcf\xbc\xbc\xbf\xe4"
	const utf8Text = "안녕하세요"
	for _, tc := range []struct {
		name, contentType, body, fallback string
		want                              string
		codes                             []message.Code
	}{
		{"no charset, legacy bytes", "text/plain", eucKR, "euc-kr", utf8Text, []message.Code{message.CodeCharsetMissing}},
		{"no content type at all", "", eucKR, "EUC-KR", utf8Text, []message.Code{message.CodeCharsetMissing}},
		{"declared utf-8, legacy bytes", "text/plain; charset=utf-8", eucKR, "euc-kr", utf8Text, []message.Code{message.CodeCharsetDeclarationSuspect}},
		{"declared us-ascii, legacy bytes", "text/html; charset=us-ascii", eucKR, "euc-kr", utf8Text, []message.Code{message.CodeCharsetDeclarationSuspect}},
		{"no charset, utf-8 bytes", "text/plain", utf8Text, "euc-kr", utf8Text, nil},
		{"declared utf-8, utf-8 bytes", "text/plain; charset=utf-8", utf8Text, "euc-kr", utf8Text, nil},
		{"ascii only", "text/plain", "hello", "euc-kr", "hello", nil},
		{"empty body", "text/plain", "", "euc-kr", "", nil},
		{"declared another charset", "text/plain; charset=iso-8859-1", "caf\xe9", "euc-kr", "café", nil},
		{"not text", "application/octet-stream", eucKR, "euc-kr", eucKR, nil},
		// Not decoded, and said so at SeverityInfo: the root's
		// TestParse_undeclaredBodyWatched pins the severity.
		{"no fallback, no charset", "text/plain", eucKR, "", eucKR, []message.Code{message.CodeCharsetMissing}},
		{"no fallback, declared utf-8", "text/plain; charset=utf-8", eucKR, "", eucKR, []message.Code{message.CodeCharsetInvalidSequence}},
		{"unknown fallback", "text/plain", eucKR, "x-no-such-charset", eucKR, []message.Code{message.CodeCharsetMissing}},
		{"long body cut inside a character", "text/plain", strings.Repeat("a", 4095) + utf8Text, "euc-kr", strings.Repeat("a", 4095) + utf8Text, nil},
	} {
		header := ""
		if tc.contentType != "" {
			header = "Content-Type: " + tc.contentType + "\r\n"
		}
		// The fallback does not depend on ParseOptions.RecoverCharsetDeclarations.
		for _, optIn := range []bool{false, true} {
			s, e, err := message.Parse(strings.NewReader("Subject: s\r\n"+header+"\r\n"+tc.body), &message.ParseOptions{FallbackCharset: tc.fallback, RecoverCharsetDeclarations: optIn})
			if err != nil {
				t.Fatalf("%s: Parse() = %v", tc.name, err)
			}
			b, rerr := io.ReadAll(e.Body)
			if string(b) != tc.want || rerr != nil {
				t.Errorf("%s/optIn=%v: body (%q, %v), want %q", tc.name, optIn, b, rerr, tc.want)
			}
			var codes []message.Code
			for _, d := range s.Diagnostics() {
				codes = append(codes, d.Code)
			}
			if !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("%s/optIn=%v: diagnostics %v, want %v", tc.name, optIn, codes, tc.codes)
			}
		}
	}
}
