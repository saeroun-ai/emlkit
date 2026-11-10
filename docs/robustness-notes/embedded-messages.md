# Embedded messages and digests (ROB-090-001, -002, -003, -007)

Issues #37, #168 (upstream digest). Code: `embedded.go`, `entity.go`
(`Walk`, `newWithOptions`), `origin.go`. Tests: `embedded_test.go`.

## API

- `Entity.CarriesMessage()` — the entity's media type is `message/rfc822`,
  `message/global`, `message/news`, or a header-only form
  (`text/rfc822-headers`, `message/global-headers`).
- `Entity.EmbeddedMessage()` — reads the embedded header block from `Body` and
  returns the embedded message as an entity one level down. The embedded body
  and parts are read lazily from the same stream. The first call consumes
  `Body`; later calls return the same entity and error. Returns `nil, nil` for
  an entity that carries no message.
- `ParseOptions.WalkEmbeddedMessages` — `Entity.Walk` visits the carrying
  entity, then the embedded message as its one child (path + `0`), then that
  message's own tree. Off by default.

Nothing descends unasked. `mail.Reader` still returns an embedded message as
one attachment, and a default `Walk` still visits it as a leaf whose `Body` is
the embedded message's bytes.

## Same parse, own diagnostics

The embedded entity belongs to the carrier's parse: same session, same
healing, same header budgets, one part of `MaxPartCount`, one level of
`MaxMIMEDepth`. Its diagnostics carry its own path.

Positions: when the carrier declares 7bit, 8bit, binary or nothing, the
embedded bytes are the input's and diagnostics carry input offsets. When the
carrier was Base64 or quoted-printable encoded they carry the path and no
offset.

## MaxMessageDepth (ROB-090-002)

The message itself is depth zero. An embedded message past the budget is not
read: `limit.message_depth` (LimitExceeded) and an entity-scoped `FatalError`
from `EmbeddedMessage`. The carrier, its `Body` and the parse are unaffected;
a `Walk` goes on with the carrier's siblings.

## multipart/digest (ROB-090-003)

A part of a `multipart/digest` that declares no `Content-Type` is
`message/rfc822` (RFC 2046 section 5.1.5). `EffectiveContentType` reports it,
and `EmbeddedMessage` reads it.

## Diagnostics

| Code | When |
| --- | --- |
| `limit.message_depth` | embedded message deeper than `MaxMessageDepth` |
| `message.embedded_invalid` | the embedded header block could not be read |
| `message.embedded_truncated` | the carrier holds no bytes at all (explicit call only) |
| `message.illegal_transfer_encoding` | `message/rfc822` declared Base64 or quoted-printable; decoded as declared |

## Limitations

- Reading the embedded message consumes the carrier's `Body`, so the carrier
  can no longer be written back verbatim afterwards.
- A walkFunc that reads the carrier's `Body` leaves nothing to descend into;
  that is not reported.
- `message/partial` and `message/external-body` are not interpreted
  (ROB-090-008).
