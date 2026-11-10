package message_test

import (
	"fmt"

	"github.com/saeroun-ai/emlkit"
)

func ExampleDecodeHeaderText() {
	// A subject as broken mail writes it: an encoded word glued to "Re:", a
	// character split across two words, and a word in a charset nothing here
	// can decode. Everything that can be read is read, the rest is kept as it
	// was written, and the diagnostics say what happened.
	text, diags := message.DecodeHeaderText("Re:=?utf-8?B?7ZU=?= =?UTF-8?B?nOq4gA==?= =?x-private?Q?raw?=")
	fmt.Println(text)
	for _, d := range diags {
		fmt.Println(d.Severity, d.Code)
	}
	// Output:
	// Re:한글 =?x-private?Q?raw?=
	// recovered rfc2047.embedded_word
	// unsupported rfc2047.unknown_charset
}

func ExampleHeader_TextDiagnostics() {
	// An encoded word that decodes to a line break cannot start a new header
	// line in the decoded text.
	var h message.Header
	h.Set("Subject", "=?utf-8?Q?line=0D=0ABcc:_x@example.org?=")

	text, diags := h.TextDiagnostics("Subject")
	fmt.Printf("%q\n", text)
	fmt.Println(diags[0].Field, diags[0].Code)
	// Output:
	// "line Bcc: x@example.org"
	// Subject rfc2047.header_injection
}
