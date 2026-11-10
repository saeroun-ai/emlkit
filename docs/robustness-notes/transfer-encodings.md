# Transfer encodings (ROB-070-001 to ROB-070-010, ROB-060-012)

Issues #32, #33, #34, #35, #49. Code: `cte_tolerant.go`, `encoding.go`,
`entity.go`. Tests: `cte_tolerant_test.go`.

## Principle

A damaged transfer-encoded body is decoded as far as its bytes say anything.
The tolerant decoders never fail on what a body holds, only on what its input
fails with. Each kind of repair is reported once per body. `ProfileStrict`
keeps the earlier decoders (standard library Base64, which stops with its
corruption error; the standard quoted-printable reader over the normalizer).

`Read`, `ReadWithOptions` and `New` use the same tolerant decoders and report
nothing, having no session.

## Content-Transfer-Encoding value (ROB-070-001, -002)

`canonicalCTE` reads the value:

| Input | Result | Diagnostic |
| --- | --- | --- |
| case, white space, `(comments)` | the mechanism | none (syntax) |
| quotes, `base64;`, `base64; x=y`, `quoted-printable foo` | the mechanism | `cte.invalid_token` (Recovered) |
| `7-bit`, `8bits`, `base-64`, `qp`, `quoted printable`, `uuencode`, `x-uue` | the standard name | `cte.invalid_token` |
| `=?us-ascii?Q?base64?=` | decoded, then as above | `cte.invalid_token` |
| anything else | unknown: raw body, `IsUnknownEncoding` warning | `cte.unknown` (Unsupported) |

Under `ProfileStrict` a value that would need a repair is unknown.

## 7bit (ROB-070-003)

A body that declares `7bit` under `Parse` records `cte.seven_bit_violation`
(Info) at the first NUL or byte above 127. Every byte is kept. An undeclared
encoding is not checked: 8-bit bytes with no declaration are too common to be
worth a diagnostic each.

## Base64 (ROB-070-004 to -007)

- White space and line endings anywhere are framing.
- A byte outside the alphabet is skipped: `cte.base64_junk`.
- `-` and `_` are read as the URL-safe 62 and 63: `cte.base64_url_alphabet`.
- Padding ends a group wherever it stands. Data after it is decoded as a
  further segment: `cte.base64_data_after_padding`. This is deliberate: a
  scanner that stops at the first padding sees less than a permissive mail
  client does.
- Misplaced or excess `=` is ignored: `cte.base64_padding_invalid`.
- A body ending inside a group yields the whole bytes of that group — one for
  two characters, two for three: `cte.base64_padding_missing`. A single
  left-over character holds no whole byte and is dropped:
  `cte.base64_length_invalid` (Incomplete).

## Quoted-printable (ROB-070-008)

Conformant input decodes exactly as before. In addition:

- `=` not followed by two hexadecimal digits or a line ending is literal text:
  `cte.qp_invalid_hex`.
- A soft line break may have white space after `=` and may end in a bare LF or
  a bare CR: `cte.qp_invalid_soft_break`.
- `=` at the end of the body is kept as a literal `=`:
  `cte.qp_trailing_equals`.
- Control bytes are kept: `cte.qp_control_byte` (Info).
- No line is too long for the decoder; an ended line longer than 998 bytes
  records `cte.qp_overlong_line` (Info). `MaxLineBytes` still applies.

## uuencode (ROB-070-009)

`x-uuencode` and the aliases `uuencode`, `x-uue`, `uue`. Lines before `begin`
are skipped. With no `begin` line, decoding starts at the first whole
uuencoded line (`cte.uuencode_missing_begin`). With no `end` line it runs to
the end of the body (`cte.uuencode_missing_end`). A line shorter than its
length character says is decoded as if padded with the spaces a relay
stripped. `begin-base64` is not recognized.

## When a diagnostic is recorded

A repair is recorded once a later line ending of the body is read, or the body
ends with `io.EOF`. A repair seen on the last, unended line of a body whose
input then failed is not recorded, and no partial Base64 group is emitted
there: those bytes may be a multipart delimiter the failure cut short, and the
failure is what the body reports (ROB-010-006). `cte.seven_bit_violation` is
recorded at once.

A caller that stops reading a body before its end may therefore miss the
repairs of the final unended line.

## Multipart and message/rfc822 (ROB-060-012)

- A multipart that declares anything but `7bit`, `8bit` or `binary` has the
  declaration ignored and its parts framed from the raw body:
  `multipart.illegal_transfer_encoding` (Recovered).
- `message/rfc822` with Base64, quoted-printable or uuencode is decoded as
  declared: `message.illegal_transfer_encoding` (Info).

## Changed expectations

- An input failure under a tolerant decoder is returned as itself, not joined
  with a decoder report; `ProfileStrict` keeps the join
  (`io_error_test.go`, `limit_body_test.go`).
- A corrupt Base64 body no longer ends at the corruption under the default
  profile.

## Known limitations

- Text appended after a Base64 body (list footers) decodes to garbage bytes
  after the real content; only the diagnostic tells.
- `-`/`_` at the start of an undelimited boundary-like line are read as data.
- `begin-base64` uuencode framing is unsupported.
