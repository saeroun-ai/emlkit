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

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

func main() {
	var r io.Reader // an io.Reader containing a mail message

	mr, err := mail.CreateReader(r)
	// An unknown-charset error is not fatal: the reader is still usable.
	if err != nil && !message.IsUnknownCharset(err) {
		log.Fatal(err)
	}
	defer mr.Close()

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			// Message text (plain and/or HTML).
			b, _ := io.ReadAll(p.Body)
			log.Printf("Got text: %v", string(b))
		case *mail.AttachmentHeader:
			// An attachment.
			filename, _ := h.Filename()
			log.Printf("Got attachment: %v", filename)
		}
	}
}
```

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
| RFC 2183 | Content-Disposition |
| RFC 2231 | parameter continuations (via Go `mime`) |
| RFC 6532 | UTF-8 in headers |
| RFC 3501 §5.1.3 | modified UTF-7 (opt-in charset plugin) |

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
charset/encoding are warnings rather than failures.
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
