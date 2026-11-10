package charset_test

import (
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/charset"
)

// TestReader_unknownCharsets: a label the registry cannot resolve is an error
// from Reader, which message reports as the unknown-charset warning together
// with a usable entity whose body is the raw bytes. Nothing is decoded on a
// guess, and nothing is turned into U+FFFD.
func TestReader_unknownCharsets(t *testing.T) {
	const raw = "caf\xe9 \x1b$)C\x0eGQ\x0f \xbe\xc8"
	for _, label := range []string{
		"idontexist",
		// Registered, and not decodable here.
		"iso-2022-cn", "iso-2022-cn-ext", "iso-2022-jp-2", "iso-2022-jp-3", "johab", "viscii", "euc-tw", "armscii-8", "tcvn",
		// The WHATWG "replacement" encoding and its labels: one U+FFFD for the
		// whole body is not a decoding.
		"replacement", "csiso2022cn",
		// IMAP modified UTF-7 is not a MIME charset (ROB-080-006).
		"utf-7-imap", "x-imap-mailbox-name",
		// Near misses of ROB-080-011, with and without decoration.
		"cp-949", "x-cp949", "cp9490",
		// Detection requests, which name no single charset.
		"_autodetect", "_autodetect_all",
		// Not a label at all.
		strings.Repeat("x", 300),
	} {
		if r, err := charset.Reader(label, strings.NewReader(raw)); err == nil || r != nil {
			t.Errorf("Reader(%.20q) = (%v, %v), want an error and no reader", label, r, err)
			continue
		} else if len(err.Error()) > 200 {
			t.Errorf("Reader(%.20q): the error is %d bytes long", label, len(err.Error()))
		}
		if _, err := charset.Lookup(label); err == nil {
			t.Errorf("Lookup(%.20q) succeeded", label)
		}

		text := "Content-Type: text/plain; charset=\"" + label + "\"\r\n\r\n" + raw
		e, err := message.Read(strings.NewReader(text))
		if e == nil || !message.IsUnknownCharset(err) {
			t.Errorf("Read(charset=%.20q) = (%v, %v), want a usable entity and the unknown-charset warning", label, e, err)
			continue
		}
		if b, _ := io.ReadAll(e.Body); string(b) != raw {
			t.Errorf("charset=%.20q: body = %q, want the raw bytes", label, b)
		}
	}
}

// TestRead_decoratedAndFallbackLabels: the labels the registry repairs, as a
// message carries them.
func TestRead_decoratedAndFallbackLabels(t *testing.T) {
	for _, c := range []struct{ contentType, body, want string }{
		// Quotes inside the quoted parameter value.
		{`text/plain; charset="\"ks_c_5601-1987\""`, cp949Bytes, cp949Text},
		{`text/plain; charset="'euc-kr'"`, cp949Bytes, cp949Text},
		{`text/plain; charset="charset=cp949"`, cp949Bytes, cp949Text},
		{`text/plain; charset="euc-kr; format=flowed"`, cp949Bytes, cp949Text},
		// Mislabels.
		{`text/plain; charset=iso-8859-1`, "\x93caf\xe9\x94 \x80", "“café” €"},
		{`text/plain; charset=x-unknown`, "caf\xe9 caf\xc3\xa9", "café café"},
		{`text/plain; charset=unknown-8bit`, "caf\xe9 caf\xc3\xa9", "café café"},
		{`text/plain; charset=ascii`, "caf\xe9 caf\xc3\xa9", "café café"},
		// .NET: UTF-16LE without a byte order mark, labelled utf-16.
		{`text/plain; charset=utf-16`, "H\x00i\x00 \x00\x5c\xd5\x00\xae", "Hi 한글"},
		{`text/plain; charset=iso-2022-kr`, "\x1b$)C\x0eGQ1[\x0f", "한글"},
	} {
		e, err := message.Read(strings.NewReader("Content-Type: " + c.contentType + "\r\n\r\n" + c.body))
		if err != nil {
			t.Errorf("%s: Read() = %v, want no warning", c.contentType, err)
			continue
		}
		if b, err := io.ReadAll(e.Body); err != nil || string(b) != c.want {
			t.Errorf("%s: body = (%q, %v), want %q", c.contentType, b, err, c.want)
		}
	}
}
