# emlkit

[![Go Reference](https://pkg.go.dev/badge/github.com/saeroun-ai/emlkit.svg)](https://pkg.go.dev/github.com/saeroun-ai/emlkit)

A maintained fork of [emersion/go-message](https://github.com/emersion/go-message).

A Go library for the Internet Message Format (email and MIME). It is
**streaming-first** (everything is built on `io.Reader` / `io.Writer`) and
**DKIM-friendly** (parsed messages re-serialize byte-for-byte). The API is
layered — pick the lowest layer that does what you need:

* [`textproto`](https://godocs.io/github.com/saeroun-ai/emlkit/textproto) — the
  raw RFC 5322 / MIME wire format, with no value decoding.
* root [`message`](https://pkg.go.dev/github.com/saeroun-ai/emlkit) — MIME
  semantics: transfer-encoding and charset decoding, the multipart tree.
* [`mail`](https://godocs.io/github.com/saeroun-ai/emlkit/mail) — a convenience
  API that models a message as text parts + attachments.

It implements:

* [RFC 5322]: Internet Message Format
* [RFC 2045], [RFC 2046] and [RFC 2047]: Multipurpose Internet Mail Extensions
* [RFC 2183]: Content-Disposition Header Field

## Quick start

Read a mail message and iterate its parts:

```go
package main

import (
	"io"
	"log"

	"github.com/saeroun-ai/emlkit/mail"
)

func main() {
	var r io.Reader // an io.Reader containing a mail message

	mr, err := mail.CreateReader(r)
	if mr == nil {
		log.Fatal(err) // no usable reader, whatever the error looks like
	} else if err != nil {
		// A warning (unknown transfer encoding or charset): the reader is
		// still usable.
		log.Printf("warning: %v", err)
	}
	defer mr.Close()

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if p == nil {
			log.Fatal(err) // structural, I/O or fatal error: stop
		} else if err != nil {
			// A warning: the part is usable, and its body is what the stage
			// with no decoder left undecoded. Iteration continues.
			log.Printf("warning: %v", err)
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			// Message text (plain and/or HTML).
			b, err := io.ReadAll(p.Body)
			if err != nil {
				// Reading a body can fail too (I/O, truncation, a budget):
				// b holds what was read before it, but it is not complete.
				log.Fatal(err)
			}
			log.Printf("Got text: %v", string(b))
		case *mail.AttachmentHeader:
			// An attachment.
			filename, _ := h.Filename()
			log.Printf("Got attachment: %v", filename)
		}
	}
}
```

### Reading under resource budgets

`mail.CreateReader` and `message.Read` bound only the message header (1 MiB).
To hold every entity of an untrusted message to the parser's budgets and to
collect structured diagnostics, parse with `message.Parse` and hand the entity
to `mail.NewReader`:

```go
session, e, err := message.Parse(r, nil) // nil: default Limits
if e == nil || message.IsFatal(err) {
	log.Fatal(err) // no usable message, or a budget/sink stopped the parse
}
mr := mail.NewReader(e)
// ...iterate mr exactly as above; a FatalError from NextPart ends it...
for _, d := range session.Diagnostics() {
	log.Println(d) // every defect observed, with its MIME path
}
```

`mail.Reader` classifies each part by the metadata the parse interpreted it with
(`Entity.EffectiveContentType` and `EffectiveContentDisposition`, which are
bounded, read once and fixed at construction), not by reparsing the raw header.
It also stops whenever the parse was stopped, including for a single-part
message.

Errors and diagnostics answer different questions, so read both: an error says
whether a scope could continue, the session says what was observed. `Parse`
enforces **thirteen** budgets today — `MaxInputBytes`, `MaxHeaderBytes`,
`MaxHeaderFieldBytes`, `MaxHeaderCount`, `MaxLineBytes`, `MaxParameterCount`,
`MaxParameterSegments`, `MaxMIMEDepth`, `MaxPartCount`, `MaxDiagnosticCount`,
`MaxEncodedBodyBytes`, `MaxDecodedBodyBytes` and `MaxTotalDecodedBytes`.
`MaxDiagnosticCount` (default 1000) caps diagnostics, not parsing: the
diagnostic that reaches it is replaced by one `limit.diagnostic_count`
diagnostic, later diagnostics are neither retained nor
delivered to a sink, and the cap alone does not stop the parse — metadata is
still healed and bodies and parts still returned, while every other budget or
Strict refusal still fails with its own `FatalError` and diagnostic. `MaxInputBytes` is
counted once at the input, on the raw bytes actually read from it — including
body bytes buffered ahead while the header was read — so a body or epilogue
never read from the input is never checked. One byte past the budget ends the
whole parse with a `FatalError` and one `limit.input_bytes` diagnostic rather
than a successful `io.EOF`. The body budgets bound leaf bodies (entities whose
`MultipartReader` is nil) as they stream: wire bytes per body (default 128 MiB),
decoded bytes per body (128 MiB, counted in bytes, so a cut may split a UTF-8
character) and decoded bytes of all leaves (512 MiB). A per-body overflow keeps
the header and the delivered prefix and ends that body with a `FatalError`; its
parent can go on to the next part unless another limit or a sink stops it. The
total ends the parse. A framed multipart container's raw body is outside them
and bounded by `MaxInputBytes`. Nothing is replayed: raw bytes a body consumed
are gone. The other `Limits` fields are validated but **not enforced yet**, and
there is no cancellation support yet. `Read` and `ReadWithOptions` have no total
input budget; only their message-header cap (`ReadOptions.MaxHeaderBytes`)
applies. See
[docs/conformance.md](docs/conformance.md).

## Features

* Streaming API
* Automatic encoding and charset handling (to decode all charsets, add
  `import _ "github.com/saeroun-ai/emlkit/charset"` to your application)
* A [`mail`](https://godocs.io/github.com/saeroun-ai/emlkit/mail) subpackage
  to read and write mail messages
* DKIM-friendly
* A [`textproto`](https://godocs.io/github.com/saeroun-ai/emlkit/textproto)
  subpackage that just implements the wire format

## Standards & conformance

emlkit targets production mail: robust against malformed input, faithful to the
standards, byte-exact on round-trip. The full, code-referenced details are in
[docs/conformance.md](docs/conformance.md) — a summary:

**Implemented standards**

| Standard | Scope |
|----------|-------|
| RFC 5322 | Internet Message Format |
| RFC 2045 / 2046 / 2047 | MIME (bodies, multipart, encoded-words) |
| RFC 2152 | standard UTF-7 (`charset` plugin) |
| RFC 2183 | Content-Disposition |
| RFC 2231 | parameter continuations (via Go `mime`) |
| RFC 6532 | UTF-8 in headers |
| RFC 3501 §5.1.3 | modified UTF-7 for IMAP (`utf7` package) |

**Tolerates** common real-world breakage — malformed quoted-printable, indented
base64, bare-LF line endings, empty/missing charset, semicolon header separators,
empty/consecutive multipart parts, a missing final boundary, over-long header
keys, and more.
([full list →](docs/conformance.md#accepted-lenient-input))

**Won't do**, by design — write a non-UTF-8 charset (decoded on read, never
emitted on write), decode exotic charsets without the opt-in `charset` plugin,
or parse mbox archives.
([details →](docs/conformance.md#not-supported--rejected))

**Guarantees** — verbatim header round-trip (DKIM), body byte-preservation when
the encoding is unchanged, and an error-with-usable-result contract where unknown
charset/encoding are warnings rather than failures, in `message` and `mail` alike.
([details →](docs/conformance.md#behavioral-guarantees))

## License

MIT — see [LICENSE](LICENSE).

This is a maintained fork of [emersion/go-message](https://github.com/emersion/go-message).
The original code and all fork modifications are licensed under the MIT license:

* Original work: Copyright (c) 2016 emersion
* Fork modifications: Copyright (c) 2026 TIENIPIA Co., Ltd.

[RFC 5322]: https://tools.ietf.org/html/rfc5322
[RFC 2045]: https://tools.ietf.org/html/rfc2045
[RFC 2046]: https://tools.ietf.org/html/rfc2046
[RFC 2047]: https://tools.ietf.org/html/rfc2047
[RFC 2183]: https://tools.ietf.org/html/rfc2183
