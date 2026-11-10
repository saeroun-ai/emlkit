// Package utf7 implements modified UTF-7 encoding defined in RFC 3501 section 5.1.3
//
// Modified UTF-7 is the encoding of IMAP mailbox names and of nothing else. It
// is not the MIME charset "utf-7" of RFC 2152, which shifts with '+' where
// this one shifts with '&', writes '/' where this one writes ',', and lets a
// run end without '-'. Text in one is not text in the other: this package
// passes "caf+AOk-" through as the eight ASCII characters it is, and the
// charset package, which decodes the charset labels utf-7 and
// unicode-1-1-utf-7 as RFC 2152, does the same with "caf&AOk-". No charset
// label selects this package.
//
// The decoder is strict, as RFC 3501 requires of mailbox names: anything that
// is not canonical modified UTF-7 is ErrInvalidUTF7, never a guess. It reads a
// shifted run as a whole, so through a streaming reader a run must fit the
// reader's buffer (4096 bytes for golang.org/x/text/transform.NewReader); a
// longer one fails with transform.ErrShortSrc. Mailbox names are far shorter.
package utf7

import (
	"encoding/base64"

	"golang.org/x/text/encoding"
)

const (
	min = 0x20 // Minimum self-representing UTF-7 value
	max = 0x7E // Maximum self-representing UTF-7 value

	repl = '\uFFFD' // Unicode replacement code point
)

var b64Enc = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,")

type enc struct{}

func (e enc) NewDecoder() *encoding.Decoder {
	return &encoding.Decoder{
		Transformer: &decoder{true},
	}
}

func (e enc) NewEncoder() *encoding.Encoder {
	return &encoding.Encoder{
		Transformer: &encoder{},
	}
}

// Encoding is the modified UTF-7 encoding.
var Encoding encoding.Encoding = enc{}
