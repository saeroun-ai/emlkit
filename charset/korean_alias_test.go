package charset_test

import (
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/charset"
)

// ROB-080-011 (issue #1): the Korean compatibility labels cp949 and
// x-windows-949 name the same Windows code page 949 that windows-949,
// ks_c_5601-1987 and (as WHATWG and x/text treat it) euc-kr already decode.
//
// The fixture is fixed bytes, not bytes produced by the decoder under test: it
// is "안녕하세요 똠" as Python's cp949 codec encodes it. 똠 (0x8C 0x63) is outside
// KS X 1001, so it exists only in the CP949 extension — strict EUC-KR rejects it
// — which is what proves these labels reach a CP949-capable decoder.
const (
	cp949Bytes = "\xbe\xc8\xb3\xe7\xc7\xcf\xbc\xbc\xbf\xe4 \x8c\x63"
	cp949Text  = "안녕하세요 똠"

	// cp949Base64 is cp949Bytes in base64, computed with coreutils base64.
	cp949Base64 = "vsiz58fPvLy/5CCMYw=="
	// cp949Q is cp949Bytes in RFC 2047 Q encoding: each byte above 0x7F as
	// =XX, the space as '_', and the ASCII 'c' (0x63) of 똠 as itself.
	cp949Q = "=BE=C8=B3=E7=C7=CF=BC=BC=BF=E4_=8Cc"
)

// newLabels are the aliases this item adds; oldLabels already decoded CP949
// and must keep doing so.
var (
	newLabels = []string{"cp949", "CP949", "Cp949", "x-windows-949", "X-Windows-949", "X-WINDOWS-949"}
	oldLabels = []string{"euc-kr", "EUC-KR", "windows-949", "ks_c_5601-1987", "KS_C_5601-1987"}
)

func allLabels() []string { return append(append([]string(nil), newLabels...), oldLabels...) }

func TestReader_koreanAliases(t *testing.T) {
	for _, label := range allLabels() {
		r, err := charset.Reader(label, strings.NewReader(cp949Bytes))
		if err != nil {
			t.Errorf("Reader(%q) = %v, want a CP949 decoder", label, err)
			continue
		}
		b, err := io.ReadAll(r)
		if err != nil || string(b) != cp949Text {
			t.Errorf("Reader(%q) decoded (%q, %v), want %q", label, b, err, cp949Text)
		}
	}
}

func TestRead_koreanAliasBody(t *testing.T) {
	for _, label := range allLabels() {
		for _, enc := range []struct {
			cte, body string
		}{
			{"8bit", cp949Bytes},
			{"base64", cp949Base64},
		} {
			text := "Content-Type: text/plain; charset=" + label + "\r\n" +
				"Content-Transfer-Encoding: " + enc.cte + "\r\n\r\n" + enc.body
			e, err := message.Read(strings.NewReader(text))
			if err != nil {
				t.Errorf("%s/%s: Read() = %v, want no warning", label, enc.cte, err)
				continue
			}
			b, err := io.ReadAll(e.Body)
			if err != nil || string(b) != cp949Text {
				t.Errorf("%s/%s: body = (%q, %v), want %q", label, enc.cte, b, err, cp949Text)
			}
		}
	}
}

func TestHeader_koreanAliasEncodedWords(t *testing.T) {
	for _, label := range allLabels() {
		for _, word := range []string{
			"=?" + label + "?B?" + cp949Base64 + "?=",
			"=?" + label + "?b?" + cp949Base64 + "?=",
			"=?" + label + "?Q?" + cp949Q + "?=",
			"=?" + label + "?q?" + cp949Q + "?=",
		} {
			var h message.Header
			h.Set("Subject", word)
			got, err := h.Text("Subject")
			if err != nil || got != cp949Text {
				t.Errorf("Text(%q) = (%q, %v), want %q", word, got, err, cp949Text)
			}
		}
	}
}

// TestReader_koreanAliasesDoNotWiden pins that only the two labels were added:
// near misses are still unknown, at the plugin and through message.Read, and a
// body with no charset is not guessed to be Korean.
func TestReader_koreanAliasesDoNotWiden(t *testing.T) {
	for _, label := range []string{"cp9490", "cp-949", "x-windows-9490", "windows949x", "x-cp949"} {
		if _, err := charset.Reader(label, strings.NewReader(cp949Bytes)); err == nil {
			t.Errorf("Reader(%q) succeeded, want an unknown charset", label)
		}

		text := "Content-Type: text/plain; charset=" + label + "\r\n\r\n" + cp949Bytes
		e, err := message.Read(strings.NewReader(text))
		if e == nil || !message.IsUnknownCharset(err) {
			t.Errorf("Read(charset=%s) = (%v, %v), want a usable entity and the unknown-charset warning", label, e, err)
			continue
		}
		if b, _ := io.ReadAll(e.Body); string(b) != cp949Bytes {
			t.Errorf("charset=%s: body = %q, want the raw bytes %q", label, b, cp949Bytes)
		}
	}

	e, err := message.Read(strings.NewReader("Content-Type: text/plain\r\n\r\n" + cp949Bytes))
	if err != nil {
		t.Fatalf("Read(no charset) = %v", err)
	}
	if b, _ := io.ReadAll(e.Body); string(b) != cp949Bytes {
		t.Errorf("no charset: body = %q, want the bytes unchanged %q: nothing is guessed", b, cp949Bytes)
	}
}
