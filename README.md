# emlkit

[![Go Reference](https://pkg.go.dev/badge/github.com/saeroun-ai/emlkit.svg)](https://pkg.go.dev/github.com/saeroun-ai/emlkit)

A maintained fork of [emersion/go-message](https://github.com/emersion/go-message).

emlkit is a Go library for the Internet Message Format (email and MIME). Its
main use is the front end of a spam classifier: `message.Parse` reads damaged
mail as far as its bytes allow, repairs what it must, records every repair
as a coded diagnostic, and holds the whole parse to resource budgets.
`mail.ReadTree` then gives the MIME tree with each part classified, decoded
and hashed. The API is streaming (`io.Reader` / `io.Writer`), and a header
field read and not changed is written back byte for byte.

Module `github.com/saeroun-ai/emlkit`, Go 1.26 or later, one dependency:
`golang.org/x/text`.

## Packages

The core is layered; pick the lowest layer that does what you need.

| Package | What it does |
| --- | --- |
| [`textproto`](https://pkg.go.dev/github.com/saeroun-ai/emlkit/textproto) | RFC 5322 and MIME wire format, no value decoding; header recovery on request (`HeaderLimits.Recover`) |
| root [`message`](https://pkg.go.dev/github.com/saeroun-ai/emlkit) | MIME semantics: `Parse` and its profiles, healing, diagnostics, `Limits`, `Completion`, transfer and charset decoding, embedded messages, raw sources |
| [`mail`](https://pkg.go.dev/github.com/saeroun-ai/emlkit/mail) | `ReadTree` and its views, `Reader` and `Writer`, tolerant address, date, Message-ID, `Received` and `List-*` accessors, `Authentication-Results`, delivery and feedback reports, `Spool` |
| [`charset`](https://pkg.go.dev/github.com/saeroun-ai/emlkit/charset) | optional charset plugin: `import _ "github.com/saeroun-ai/emlkit/charset"` decodes most legacy charsets; adds about 1 MiB |
| [`utf7`](https://pkg.go.dev/github.com/saeroun-ai/emlkit/utf7) | the IMAP modified UTF-7 codec (RFC 3501), apart from the MIME `utf-7` charset |

Optional packages read formats that hide content inside one MIME part. The
core imports none of them and none imports the core; each takes plain values
from a `mail.Node`, has its own `Limits` and reports its own `Issue` values.

| Package | Reads | Note |
| --- | --- | --- |
| `tnef` | `winmail.dat` (`application/ms-tnef`): attachments, bodies, compressed RTF | [tnef.md](docs/robustness-notes/tnef.md) |
| `smime` | S/MIME and CMS: classification, signed content to re-parse, signer metadata, signature checks; certificate chains only to roots the caller gives | [smime.md](docs/robustness-notes/smime.md) |
| `pgpmime` | PGP/MIME structure, ASCII armor, cleartext signatures, OpenPGP packet metadata; verification and decryption only through the caller's interfaces | [pgpmime.md](docs/robustness-notes/pgpmime.md) |
| `icalendar` | `text/calendar` invitations: events, attendees, scheduling intent | [icalendar.md](docs/robustness-notes/icalendar.md) |
| `appledouble` | AppleSingle and AppleDouble: names, forks, Finder info, extended attributes | [appledouble.md](docs/robustness-notes/appledouble.md) |
| `legacyenc` | uuencode, xxencode, `begin-base64`, yEnc and BinHex blocks inside text bodies | [legacy-encodings.md](docs/robustness-notes/legacy-encodings.md) |

## Quick start

This is the flow of `mail/example_adapter_test.go`, which `go test` runs on a
damaged message.

```go
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/saeroun-ai/emlkit"
	_ "github.com/saeroun-ai/emlkit/charset" // decode legacy charsets
	"github.com/saeroun-ai/emlkit/mail"
)

func main() {
	session, root, err := message.Parse(os.Stdin, nil) // nil: tolerant profile, default Limits
	if root == nil {
		log.Fatal(err) // no usable message
	}
	tree, err := mail.ReadTree(root, nil) // reads every body to its end
	if err != nil {
		log.Println("stopped:", err) // a budget, a sink or the Body hook; the tree so far is kept
	}

	fmt.Println("completion:", session.Completion().State)
	for _, d := range session.Diagnostics() {
		fmt.Println(d.Severity, d.Code, d.Path) // branch on Code, never on Message
	}
	for _, n := range tree.Root.Bodies("text/plain", "text/html") {
		fmt.Printf("body %s: %d bytes\n", n.MediaType, n.Size)
	}
	for _, n := range tree.Root.Attachments() {
		fmt.Printf("attachment %s %q\n", n.MediaType, message.SafeFilename(n.Filename))
	}
	h := mail.Header{Header: tree.Root.Header}
	subject, _ := h.TextDiagnostics("Subject")
	fmt.Println("subject:", subject)
}
```

[docs/spam-adapter-contract.md](docs/spam-adapter-contract.md) says what each
of these results may be relied on for, what `Completion` states mean, how to
treat signed content and `Authentication-Results`, how to read raw bytes, and
lists every diagnostic code.

To iterate a message as text parts and attachments instead of a tree, pass
the entity from `Parse` to `mail.NewReader`; it classifies each part by the
metadata the parse interpreted (`Entity.EffectiveContentType` and
`EffectiveContentDisposition`) and stops when the parse was stopped.

`message.Read`, `ReadWithOptions`, `New` and `mail.CreateReader` keep the
behaviour they had before `Parse`: no session, no diagnostics, only the root
header budgeted. Use `Parse` for untrusted mail.

## Limits

`message.Limits` holds the budgets of one parse. Every field is an `int64`:
zero selects the default below, `message.LimitDisabled` (-1) turns one off,
and a value below -1 is refused with `ErrInvalidLimits`. They apply under
`Parse`, `ParseContext`, `ParseAt` and `ParseSource`; `message.DefaultLimits()`
returns these values.

| Field | Default | Bounds |
| --- | --- | --- |
| `MaxInputBytes` | 256 MiB | bytes read from the input |
| `MaxHeaderBytes` | 1 MiB | one header block |
| `MaxHeaderFieldBytes` | 64 KiB | one field with its continuation lines |
| `MaxHeaderCount` | 1024 | fields of one header block |
| `MaxLineBytes` | 64 KiB | one header, framing, Base64 or quoted-printable line |
| `MaxTotalHeaderBytes` | 32 MiB | all header blocks of the parse |
| `MaxTotalHeaderCount` | 100 000 | all header fields of the parse |
| `MaxParameterCount` | 256 | parameters of one `Content-Type` or `Content-Disposition` |
| `MaxParameterSegments` | 256 | RFC 2231 segments of one parameter |
| `MaxMIMEDepth` | 100 | multipart nesting |
| `MaxMessageDepth` | 32 | embedded-message nesting, when embedded messages are read |
| `MaxPartCount` | 10 000 | parts of the whole tree |
| `MaxPreambleBytes` | 1 MiB | preamble kept by a healing multipart iterator |
| `MaxEpilogueBytes` | 1 MiB | epilogue kept by `MultipartEpilogue` |
| `MaxEncodedBodyBytes` | 128 MiB | wire bytes of one leaf body |
| `MaxDecodedBodyBytes` | 128 MiB | decoded bytes of one leaf body |
| `MaxTotalDecodedBytes` | 512 MiB | decoded bytes of all leaf bodies |
| `MaxDiagnosticCount` | 1000 | diagnostics recorded and delivered |
| `MaxMemorySpoolBytes` | 8 MiB | not read by anything yet |
| `MaxDiskSpoolBytes` | 1 GiB | not read by anything yet |

`mail.Spool` takes its quotas from `mail.SpoolOptions` (8 MiB in memory,
1 GiB on disk by default), not from the two spool fields. What exceeding each
budget ends is in the [contract](docs/spam-adapter-contract.md#limits) and in
[docs/conformance.md](docs/conformance.md#not-supported--rejected).

## Behaviour worth knowing

- `Parse` reads only the header. Bodies and parts are read as the caller
  consumes them, so the diagnostics are complete only after `ReadTree` or
  `Entity.Drain`; `ParseSession.Completion` says how far the parse got.
- An `Entity` body is read once. A parse from `ParseAt` or `ParseSource`
  can hand out each entity's raw bytes and a fresh decoded body again
  ([raw-source.md](docs/robustness-notes/raw-source.md)), and `mail.Spool`
  keeps decoded bodies for repeated reads
  ([spool.md](docs/robustness-notes/spool.md)).
- `ParseContext` checks its context before and after every read and at scope
  boundaries. It never interrupts a blocked `Read`, never closes the input and
  starts no goroutine; close a source you own to make a blocked read return.
- An error the input returns is an input failure, not a diagnostic: it
  surfaces where the parse reaches it, from `Parse` while the header is read,
  otherwise from the body or part read that needs the missing bytes. A reader
  that returns `(0, nil)` 100 times in a row ends the parse
  (`input.no_progress`).
- `Entity.WriteTo` serializes; it is not a replay. It adds
  `Mime-Version: 1.0` to a header without one. Otherwise a header from a
  healing parse that was not changed is written as read when the body is
  copied as read
  ([header-recovery.md](docs/robustness-notes/header-recovery.md)); the
  raw readers of a `Source` give the input's own bytes.
- Writing permits only `utf-8` and `us-ascii` charsets; other charsets are
  decoded on read and never emitted.

## Testing on this host

Every local Go build, test, fuzz run and benchmark runs inside the bounded
runner, `scripts/qa-bounded.py`, one job at a time. The commands and limits
are in [CLAUDE.md](CLAUDE.md#host-safe-verification).

## Documentation

- [docs/spam-adapter-contract.md](docs/spam-adapter-contract.md): what a
  consumer may rely on, the whole flow, and every diagnostic code.
- [docs/conformance.md](docs/conformance.md): standards implemented,
  lenient input accepted, what is refused, and the behavioural contracts,
  each pointing at its code.
- [docs/robustness-notes/](docs/robustness-notes/): one note per area of
  healing, per consumer API, per optional package and per kind of testing.
  The index is in [conformance.md](docs/conformance.md#healing-and-where-it-is-specified).
- [docs/robustness-roadmap.md](docs/robustness-roadmap.md): the ledger of
  items and their status.

## Standards

| Standard | Scope |
| --- | --- |
| RFC 5322 | Internet Message Format |
| RFC 2045, 2046, 2047 | MIME bodies, multipart, encoded words (decoded tolerantly, written as Q words) |
| RFC 2183 | Content-Disposition |
| RFC 2231 | parameter continuations and charsets |
| RFC 6532 | UTF-8 in headers |
| RFC 2152 | standard UTF-7 (`charset` plugin) |
| RFC 3501 section 5.1.3 | modified UTF-7 for IMAP (`utf7`) |
| RFC 8601 | `Authentication-Results`, read and marked trusted only on the caller's terms |
| RFC 3464, 6533, 8098, 5965, 6522 | delivery status, disposition and feedback reports (`mail`) |

The optional packages read further formats; their notes name the
specifications. Details, tolerances and limits are in
[docs/conformance.md](docs/conformance.md).

Out of scope by design: writing non-UTF-8 charsets, reading mbox archives
(one leading `From_` line can be kept apart on request), fetching
`message/external-body`, unpacking archives, rendering HTML, and verifying
DKIM, SPF, DMARC or ARC.

## License

MIT — see [LICENSE](LICENSE).

This is a maintained fork of [emersion/go-message](https://github.com/emersion/go-message).
The original code and all fork modifications are licensed under the MIT license:

* Original work: Copyright (c) 2016 emersion
* Fork modifications: Copyright (c) 2026 TIENIPIA Co., Ltd.
