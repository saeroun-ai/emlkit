# emlkit

[![Go Reference](https://pkg.go.dev/badge/github.com/saeroun-ai/emlkit.svg)](https://pkg.go.dev/github.com/saeroun-ai/emlkit)

A maintained fork of [emersion/go-message](https://github.com/emersion/go-message).

A Go library for the Internet Message Format. It implements:

* [RFC 5322]: Internet Message Format
* [RFC 2045], [RFC 2046] and [RFC 2047]: Multipurpose Internet Mail Extensions
* [RFC 2183]: Content-Disposition Header Field

## Features

* Streaming API
* Automatic encoding and charset handling (to decode all charsets, add
  `import _ "github.com/saeroun-ai/emlkit/charset"` to your application)
* A [`mail`](https://godocs.io/github.com/saeroun-ai/emlkit/mail) subpackage
  to read and write mail messages
* DKIM-friendly
* A [`textproto`](https://godocs.io/github.com/saeroun-ai/emlkit/textproto)
  subpackage that just implements the wire format

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
