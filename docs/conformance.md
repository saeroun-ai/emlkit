# Standards, Tolerances & Limitations

This document describes exactly what `emlkit` implements, what malformed input it
accepts, what it refuses, and the behavioral guarantees it makes. It is the
detailed companion to the summary in the [README](../README.md).

Every claim below points at the code that backs it (file · symbol). emlkit is
layered — `textproto` (wire format) → root `message` (MIME semantics) →
`mail` (convenience API), with an optional `charset` plugin and optional
packages for formats inside one part (`tnef`, `smime`, `pgpmime`,
`icalendar`, `appledouble`, `legacyenc`). References name the lowest layer
where the behavior lives.


## Healing and where it is specified

Since 2026-11-10 `Parse` heals damaged mail under every profile but
`ProfileStrict`. The rule for each case, the diagnostics it records and its
known limits are written per area in `docs/robustness-notes/`:

| Healing area | Note |
| --- | --- |
| Damaged header lines, missing separators, duplicate singletons, byte-exact header replay | [`header-recovery.md`](robustness-notes/header-recovery.md) |
| Unclosed, undeclared or mismatched multipart boundaries, preamble, epilogue | [`multipart-recovery.md`](robustness-notes/multipart-recovery.md) |
| Nested multiparts whose boundaries are reused, collide or are prefixes | [`nested-boundaries.md`](robustness-notes/nested-boundaries.md) |
| Base64, quoted-printable, uuencode, Content-Transfer-Encoding values | [`transfer-encodings.md`](robustness-notes/transfer-encodings.md) |
| RFC 2047 encoded words and header text | [`rfc2047.md`](robustness-notes/rfc2047.md) |
| Media types, parameters, RFC 2231, filenames | [`mime-parameters.md`](robustness-notes/mime-parameters.md) |
| Charset labels, legacy decoders, byte order mark conflicts | [`charset-plugin.md`](robustness-notes/charset-plugin.md) |
| Embedded messages, digests, message depth | [`embedded-messages.md`](robustness-notes/embedded-messages.md) |
| Addresses, dates, Message-IDs, Received, List-* | [`mail-structured-headers.md`](robustness-notes/mail-structured-headers.md) |

| Consumer API | Note |
| --- | --- |
| MIME tree view, roles, related resources, signed spans and repairs | [`structure-view.md`](robustness-notes/structure-view.md) |
| Delivery status, disposition and feedback reports | [`reports.md`](robustness-notes/reports.md) |
| Raw bytes of every entity, recorded and seekable sources | [`raw-source.md`](robustness-notes/raw-source.md) |
| Spooling bodies for repeated reads | [`spool.md`](robustness-notes/spool.md) |
| What a parse has established | [`completion.md`](robustness-notes/completion.md) |

| Optional package | Note |
| --- | --- |
| `tnef`: winmail.dat | [`tnef.md`](robustness-notes/tnef.md) |
| `smime`: S/MIME and CMS | [`smime.md`](robustness-notes/smime.md) |
| `pgpmime`: PGP/MIME, armor, OpenPGP packets | [`pgpmime.md`](robustness-notes/pgpmime.md) |
| `icalendar`: `text/calendar` | [`icalendar.md`](robustness-notes/icalendar.md) |
| `appledouble`: AppleSingle and AppleDouble | [`appledouble.md`](robustness-notes/appledouble.md) |
| `legacyenc`: uuencode, xxencode, yEnc, BinHex inside text | [`legacy-encodings.md`](robustness-notes/legacy-encodings.md) |

| Testing and audits | Note |
| --- | --- |
| Synthetic regression corpus and its findings | [`corpus.md`](robustness-notes/corpus.md) |
| Fuzz targets and their invariants | [`fuzzing.md`](robustness-notes/fuzzing.md) |
| Randomized soak with leak and memory ceilings | [`soak.md`](robustness-notes/soak.md) |
| Differential testing against Python's `email` | [`differential.md`](robustness-notes/differential.md) |
| Complexity tests and benchmarks | [`performance.md`](robustness-notes/performance.md) |
| Bounds audit of the wire and framing code | [`bounds-audit-core.md`](robustness-notes/bounds-audit-core.md) |
| Bounds audit of the value parsers and upper layers | [`bounds-audit-values.md`](robustness-notes/bounds-audit-values.md) |

`docs/spam-adapter-contract.md` says what a consumer may rely on. The sections
below describe the framing and budget contracts that preceded this work; where
one of them says a malformed header line or an unclosed multipart is an error,
that now holds for `ProfileStrict`, `Read`, `ReadWithOptions` and `New` only.

## Implemented standards

| Standard | Scope | Implemented in |
|----------|-------|----------------|
| [RFC 5322](https://www.rfc-editor.org/rfc/rfc5322) | Internet Message Format — header field syntax, message structure | [`textproto/header.go`](../textproto/header.go) · `ReadHeader`, [`entity.go`](../entity.go) · `Read`, [`mail/header.go`](../mail/header.go) |
| [RFC 2045](https://www.rfc-editor.org/rfc/rfc2045) | MIME Part 1 — Content-Type and Content-Transfer-Encoding (quoted-printable, base64, 7bit, 8bit, binary) | [`cte_tolerant.go`](../cte_tolerant.go) · `transferDecoder` (reading; `encodingReader` for a body of no parse), [`encoding.go`](../encoding.go) · `legacyEncodingReader` (`ProfileStrict`), `encodingWriter` (writing) |
| [RFC 2046](https://www.rfc-editor.org/rfc/rfc2046) | MIME Part 2 — multipart media types and boundaries | [`textproto/multipart.go`](../textproto/multipart.go) · `MultipartReader`/`MultipartWriter` |
| [RFC 2047](https://www.rfc-editor.org/rfc/rfc2047) | MIME Part 3 — encoded-words in headers | [`rfc2047.go`](../rfc2047.go) · `DecodeHeaderText` (decoding), [`charset.go`](../charset.go) · `encodeHeader` (encoding) |
| [RFC 2152](https://www.rfc-editor.org/rfc/rfc2152) | Standard UTF-7 MIME charset | [`charset/utf7.go`](../charset/utf7.go), [`charset/charset.go`](../charset/charset.go) |
| [RFC 2183](https://www.rfc-editor.org/rfc/rfc2183) | Content-Disposition — inline/attachment, filename | [`header.go`](../header.go) · `ContentDisposition`, [`mail/attachment.go`](../mail/attachment.go) · `parseFilename` |
| [RFC 2231](https://www.rfc-editor.org/rfc/rfc2231) | Parameter value continuations and charset (e.g. `filename*=`) | [`rfc2231.go`](../rfc2231.go), [`mediatype_tolerant.go`](../mediatype_tolerant.go) under `Parse` and the tolerant functions; Go standard library `mime` for `Header.ContentType`/`ContentDisposition`, via [`header.go`](../header.go) · `parseHeaderWithParams` |
| [RFC 6532](https://www.rfc-editor.org/rfc/rfc6532) | Internationalized headers — raw UTF-8 in header values | [`mail/header.go`](../mail/header.go) · `isMultibyte`, [`textproto/header.go`](../textproto/header.go) |
| [RFC 3501](https://www.rfc-editor.org/rfc/rfc3501) §5.1.3 | Modified UTF-7 codec for IMAP mailbox names | [`utf7/`](../utf7) · `Encoding` (used directly, not wired to the MIME `utf-7` charset) |

Read only, in `mail`:

| Standard | Scope | Implemented in |
|----------|-------|----------------|
| [RFC 8601](https://www.rfc-editor.org/rfc/rfc8601) | `Authentication-Results`, parsed; trusted only for the caller's authserv-ids and position | [`mail/authres.go`](../mail/authres.go) · `Header.AuthenticationResults` |
| [RFC 6522](https://www.rfc-editor.org/rfc/rfc6522), [RFC 3464](https://www.rfc-editor.org/rfc/rfc3464), [RFC 6533](https://www.rfc-editor.org/rfc/rfc6533), [RFC 8098](https://www.rfc-editor.org/rfc/rfc8098), [RFC 5965](https://www.rfc-editor.org/rfc/rfc5965) | `multipart/report` and delivery status, disposition notification and feedback reports | [`mail/report.go`](../mail/report.go), [`mail/report_node.go`](../mail/report_node.go) · `Node.Report` |
| [RFC 2387](https://www.rfc-editor.org/rfc/rfc2387), [RFC 2557](https://www.rfc-editor.org/rfc/rfc2557) | `multipart/related` root part and `cid:` / Content-Location references; nothing is fetched | [`mail/tree.go`](../mail/tree.go) · `Node.RelatedRoot`, `Node.Resolve` |
| [RFC 1847](https://www.rfc-editor.org/rfc/rfc1847) | shape of `multipart/signed` and `multipart/encrypted`; no verification or decryption in the core | [`mail/tree.go`](../mail/tree.go) · `shape`, [`mail/tree_signed.go`](../mail/tree_signed.go) |

The optional packages read further formats (TNEF, CMS and S/MIME, PGP/MIME
and OpenPGP, iCalendar, AppleSingle and AppleDouble, uuencode, yEnc and
BinHex); each note in `docs/robustness-notes/` names the specifications and
what of them is read.

A few details worth knowing:

- **RFC 2047 (encoded-words).** Decoding is emlkit's own, in
  [`rfc2047.go`](../rfc2047.go): `B` and `Q` words in either case, tolerant of
  words glued to text, split by folding, unpadded or unclosed, with adjacent
  words of one charset joined before conversion; a word that cannot be decoded
  is left as written and its neighbours are still decoded
  ([`rfc2047.md`](robustness-notes/rfc2047.md)). **Encoding emits `Q` words
  only** — emlkit never produces `B`-encoded words
  ([`charset.go`](../charset.go) · `encodeHeader`).
- **RFC 2152 standard UTF-7.** The optional `charset` plugin registers the MIME
  charset `utf-7` to a standard UTF-7 decoder (`+` shifts, standard base64
  `+/`). This covers legacy mail that advertises `charset=utf-7`.
- **RFC 2231.** Under `Parse` (`Entity.EffectiveContentType`,
  `EffectiveContentDisposition`, `FilenameCandidates`) and in the tolerant
  functions (`ParseMediaTypeTolerant`, `ParseContentDispositionTolerant`,
  `ParseParameterCandidates`), a field that the standard library reads in full
  and that holds no `*`, `=?`, backslash or 8-bit byte is read by it as
  before; any other field is read by [`rfc2231.go`](../rfc2231.go) and
  [`mediatype_tolerant.go`](../mediatype_tolerant.go): segments in any order,
  any charset the `CharsetReader` knows, damaged series kept as far as they go
  ([`mime-parameters.md`](robustness-notes/mime-parameters.md)).
  `Header.ContentType` and `Header.ContentDisposition` still use Go's
  `mime.ParseMediaType`, and writing uses `mime.FormatMediaType`.
- **RFC 6532.** Header *values* carry raw bytes verbatim (only header *keys* are
  restricted to printable ASCII), so UTF-8 values pass through. The `mail` parser
  additionally treats multi-byte runes as valid atom text.
- **RFC 3501 modified UTF-7.** The `utf7` package decodes/encodes the IMAP
  mailbox-name encoding for direct use (e.g. with an IMAP client). It is
  deliberately separate from the MIME charset `utf-7`: that name means
  *standard* UTF-7 (RFC 2152), a different encoding (`+`/`/` vs the modified
  `&`/`,`).

## Accepted lenient input

Real-world mail is full of standards violations. emlkit normalizes or tolerates
the following malformed input **on read**, so a bad message yields usable data
instead of an error. Byte-exact round-tripping of unmodified content is still
preserved (see [Behavioral guarantees](#behavioral-guarantees)).

### Header parsing

- **Semicolon as field separator.** When a header line has no colon, a semicolon
  is accepted as the key/value separator. ([`textproto/header.go`](../textproto/header.go) · `ReadHeader`)
- **Whitespace before the colon.** `Subject : value` — spaces/tabs between the
  key and colon are trimmed. ([`textproto/header.go`](../textproto/header.go) · `ReadHeader`)
- **Empty keys are skipped**, not treated as an error — when no continuation
  line follows them; one that continuation lines follow is an orphan record
  (below). ([`textproto/header.go`](../textproto/header.go) · `ReadHeader`)
  Under a healing `Parse` (`textproto.HeaderLimits.Recover`) such a line is
  kept as a one-line orphan record instead and reported as `header.empty_name`
  ([`header-recovery.md`](robustness-notes/header-recovery.md)).
- **Continuation lines after a damaged line** (`ROB-020-002`). A continuation
  line (one beginning with a space or a tab) belongs to the logical candidate
  before it. When that candidate is damaged — no colon or semicolon anywhere in
  it, a name holding a byte a name cannot hold (white space, NUL, control,
  8-bit, or the line ending of its first line when the only colon is on a
  continuation line; `X-Test; value` then ` folded: value` included, since the
  colon wins over the semicolon as it always did), or an empty name — and at
  least one continuation line was read whole, the damaged line and its
  continuation lines are kept together as an **orphan record**: an immutable
  `textproto.OrphanContinuation` (`Bytes` — a fresh copy each call, the input's
  exact bytes, line endings, NUL and 8-bit bytes as they were — `Location` in
  the block, a UTF-8 mark counted, and `Lines`, at least two) returned by
  `textproto.Header.OrphanContinuations` in reading order. Such a block used to
  fail with "malformed MIME header line/key" (no entity at the root; the part
  an error, the next part reached by asking again), or, for an empty name, to
  lose the lines silently. A record is **not a field**: `Get`, `Has`, `Values`,
  `Fields`, `Len`, `Raw` and `Map` never see it, nothing of it is joined to a
  field around it (a `Subject` or `Content-Type` before it keeps its value), and
  an indented `Content-Type` or `Content-Transfer-Encoding` in it declares
  nothing, under every `Profile` and `Compatibility`; the block is read on after
  it as usual. Valid folding, a field the semicolon fallback accepts (`X-Bad;
  value` then ` more`), and a `\r` that is content of a valid field joining an
  indented line to it (`Subject: before\r: value\r orphan` stays one Subject)
  are read exactly as before. A damaged line **with no continuation line** is,
  without recovery, read as before — an empty name skipped, any other the error
  it always was; under a healing `Parse` it is decided as
  [`header-recovery.md`](robustness-notes/header-recovery.md) says
  (`ROB-020-003`, `ROB-020-006`) — except that a read failure other than
  `io.EOF` that kept its continuation from being looked at is returned as
  itself instead of that error. A damaged line whose last line ended with a
  bare `\r` before a name byte is a record only if the bytes after the `\r`
  prove that the next line is its content and brings a continuation line,
  within what the block's budgets would admit of them (the joined line's
  content to `MaxLineBytes`, its bytes to what `MaxHeaderFieldBytes`,
  `MaxHeaderBytes` and the shared total have left; 64 KiB when no byte budget
  is in force — not the reader's buffer size). Otherwise it is decided as
  before, and everything after the `\r` — a following field included — is in
  the reader afterwards exactly as before, even when the proof had to take it
  out (it is given back; a caller's `bufio.Reader` whose buffer that proof
  outgrew has its buffer grown, at least doubled, to hold it). A proof that
  does not fit the budgets is the old error, not a budget refusal; a spent
  shared field count refuses first. In a part's header a real delimiter line
  among those bytes is no record and is left for the next part. A record is one
  field candidate for the budgets (the shared count asked before its first
  byte, charged once, every byte once); a refused block keeps no record; a read
  failure keeps only records of whole lines. Its exact bytes are rebuilt from
  the kept lines and the line ending each had, which the scanner notes one byte
  a line in fixed storage (no allocation for a candidate of up to 16 lines);
  only a damaged candidate allocates, once, its exact size. `Copy` and every
  field edit keep the records; the writers write each in its place among the
  fields read with it — before the first of the fields that followed it that
  the header still has, after fields added since (`Add`, `Set`, `MIME-Version`)
  — a last line with no ending or a bare-CR ending followed by `\r\n` or `\n`;
  a record begins with its damaged line, never with white space, so it never
  continues the field before it, and an unmodified complete CRLF header whose
  only damaged lines are records round-trips byte for byte (without recovery,
  damaged lines with no continuation are not written; under recovery they are
  one-line records written in their place, `ROB-020-007`, see
  [`header-recovery.md`](robustness-notes/header-recovery.md)).
  In a part's header in which a record was recovered, later lines are first
  looked at as delimiter lines of the multipart, exactly as after a malformed
  prefix (`ROB-020-001`, below): a proved delimiter ends the header, the part's
  body is empty, `textproto.Part.HeaderSeparatorMissing` reports it and the
  next part follows; a damaged line whose `\r` such a line would settle is
  decided as before with nothing consumed past it. Under `Parse` each block
  with records records one `header.orphan_continuation` (Recovered) over its
  first record, counting records and lines and holding none of their bytes, and
  such a part `header.missing_body_separator` (Recovered); `ProfileStrict`
  refuses that entity locally, keeping records, fields and the raw body, and
  siblings are read as usual. `Read`, `ReadWithOptions`, `textproto` and `mail`
  heal the same way and record nothing. Other engines keep such a damaged line
  as a raw header, drop it with defects, or fail; no parity is claimed.
  ([`textproto/orphan.go`](../textproto/orphan.go), [`orphan.go`](../orphan.go))
- **Bare LF, bare CR and mixed header line endings** (`ROB-010-002`). A header line ends at `\r\n` (one ending even when split
  across reads), at a bare `\n`, or at a bare `\r`; a stray `\r` inside a value
  stays content unless the next line begins a field (`name:`), so
  `Subject: a\rb` keeps its value `a\rb`. A bare `\r` at the start of a line is
  the empty line ending the header, `A: 1\r\r\n` is field A then the empty line
  `\r\n`, and a `\r` right before end of input is a complete line ending. The
  stored raw bytes (`Raw`) end every physical line with CRLF — only a complete
  CRLF field is byte-exact there — and `textproto.Header.LineEndings` reports
  what the input used; a block read with recovery also keeps the input's own
  line endings (`Header.RawAsRead`, [`header-recovery.md`](robustness-notes/header-recovery.md)). Under `Parse` the endings are diagnosed (`header.bare_lf`,
  `header.bare_cr`, `header.mixed_line_endings` once per parse;
  `header.truncated_line` per block), and `ProfileStrict` refuses only the
  entity whose header has a bare-CR ending or no final line ending.
  ([`textproto/header.go`](../textproto/header.go) · `ReadHeader`, [`line_endings.go`](../line_endings.go))
- **Byte order marks at the start of a header block** (`ROB-010-003`). Only the first bytes of the message's header and of each part's are
  looked at, on the longest match (`FF FE 00 00` is UTF-32LE; `FF FE` and
  `FF FE 00` at the end of input are UTF-16LE). A UTF-8 mark is consumed, so it
  no longer corrupts the first field's name: the fields are read after it, the
  mark counts toward `MaxHeaderBytes` and the shared header total but toward no
  line, field or count budget, offsets include it, and
  `textproto.Header.ByteOrderMark` keeps it through `Copy` and field edits, so
  `WriteHeader` and `WriteTo` write it once, before every field — on their
  usual terms, so only a well-formed complete CRLF block with its empty line,
  every field kept and unchanged, is written byte for byte — a line skipped
  rather than kept as a field, such as one with an empty name and no
  continuation line, is not written;
  this is not raw replay. A UTF-16 or
  UTF-32 mark leaves the block unread: no field, no header budget spent, and an
  opaque entity on every entry point whose body is every byte from the mark on —
  no media type (not even text/plain), no transfer or charset decoding, and,
  having no fields, an attachment to `mail.Reader` (a Content-Disposition a
  caller adds to a copy is still interpreted) — which the writers refuse to
  serialize, before writing anything. Nothing is transcoded. Under `Parse` each such block
  records `input.bom` (Recovered for UTF-8, Unsupported for UTF-16/32), and
  `ProfileStrict` refuses that entity locally. A second, partial or misplaced
  mark is read as it always was: the start of a malformed line (`ROB-020`); a
  mark in a value or body is content.
  ([`textproto/bom.go`](../textproto/bom.go), [`bom.go`](../bom.go))
- **A header block that begins with white space** (`ROB-020-001`). A block —
  the message's or a part's, after any UTF-8 mark — whose first line begins
  with a space or a tab used to be refused with "malformed MIME header initial
  line", losing every field after it and, for a caller that stopped at the first
  error, the parts after it. Its first logical candidate — that line, the
  continuation lines after it, and what a `\r` that proves to be content joins
  to them; whitespace-only lines included, the empty line never — is now kept
  apart as `textproto.Header.MalformedPrefix`: an immutable view whose `Bytes`
  (a fresh copy each call) are the input's exact bytes, line endings, NUL and
  8-bit bytes as they were, with its `Location` in the block and its `Lines`.
  It is **not a field**: `Get`, `Has`, `Values`, `Fields`, `Len` and `Map` never
  see it, so an indented `Subject`, `Content-Type` or
  `Content-Transfer-Encoding` declares nothing, under every `Profile` and every
  `Compatibility` value; the block is read on after it as usual. It is one field
  candidate for the budgets (the shared count asked before its first byte,
  charged once), every byte charged once; a refused block keeps no prefix; a
  read failure keeps only whole lines and is returned as itself; a last line
  `io.EOF` ended is kept. `Copy` and every field edit keep it; the writers write
  it first, after any UTF-8 mark and before every field — those added since
  included — every byte in order, a last line with no ending or a bare-CR ending
  being followed by `\r\n` or `\n`, so an unmodified complete CRLF header
  round-trips byte for byte; this is not raw replay. In a part's header that
  began with such a prefix, a line proved to be a delimiter line of its
  multipart (opening or closing, any line ending or the end of input after the
  boundary and white space, a boundary with a colon included) is never read as
  a field: the header ends before it, the part's body is empty, the next part
  follows, and `textproto.Part.HeaderSeparatorMissing` reports it. A line that
  only begins like one (`--B` + spaces + `: value`) is the field it is, even
  when its white space runs past the 4 KiB buffer: such a line is held, within
  the framing line budget `MaxLineBytes` and no header-only budget, until a byte
  decides it. A proved delimiter is never charged to, or refused by, the
  header's byte, field, count or shared budgets; a line that proves to be a
  field is charged to them once and refused where it overflows one. With no
  line budget a line still undecided past one buffer is the multipart reader's
  usual framing refusal, which ends the iteration. Recovering a missing
  separator in general is `ROB-020-005`'s. Under `Parse` each such block
  records `header.first_line_continuation` (Recovered) over the whole prefix,
  and such a part `header.missing_body_separator` (Recovered) where its header
  ended; `ProfileStrict` refuses that entity locally, keeping the prefix and
  fields, and siblings are read as usual. `Read`, `ReadWithOptions`,
  `textproto` and `mail` heal the same way and record nothing. Other engines
  read such lines differently (as fields, dropped, or fatal); no parity is
  claimed.
  ([`textproto/prefix.go`](../textproto/prefix.go), [`firstline.go`](../firstline.go))
- **One leading mbox `From_` envelope line, on request** (`ROB-010-004`).
  Off by default. With `ParseOptions.RecognizeMboxEnvelope`, `Parse` and
  `ParseContext` recognize one envelope line at input offset 0 and keep it
  apart from the message: `ParseSession.MboxEnvelope` returns its bytes, line
  ending included, and its location `[0, n)`, even when the header after it
  cannot be read. The recognized shape is `From`, one space, a sender token of
  visible ASCII not beginning with `:`, then a ctime-shaped timestamp —
  English weekday and month, day 1–31, `HH:MM:SS`, four-digit year, at most one
  zone (2–5 upper-case letters or `±HHMM`) immediately before or after the
  year — and an optional `remote from host`, separated by spaces or tabs, with
  no trailing white space, ended by CRLF, LF or CR (a CR decided by the byte
  after it or by `io.EOF`). Weekday and day are syntax only, not checked against
  a calendar. RFC 4155 notes that separator lines in the wild vary; other forms
  (no timestamp, other date orders, 8-bit senders, a byte order mark before
  `From`, a line with no line ending) are not recognized and are framed exactly
  as without the option. The line may have at most 998 bytes before its line
  ending, or `Limits.MaxLineBytes` if smaller: a recognition bound of this
  library, not an mbox length rule. The header block begins after the line and
  is read as the header of an input that began there — a byte order mark at its
  start included — with diagnostics, refusals and offsets moved by the line's
  length; bodies and parts keep absolute offsets. The line lies outside the RFC
  5322 header, so no header budget counts it; `MaxInputBytes` does, and at
  most 1,000 bytes are kept. At most one `header.mbox_envelope` (Info) is
  recorded, under every profile, Strict included; none when a stop of the parse
  came first. The sender and timestamp are only shape-checked by the grammar:
  they are unauthenticated, not interpreted as an address or a date, and no
  writer writes the line. Whether a line is recognized, and the root header
  framing after it, do not depend on read chunking; decoded bodies are outside
  this item.
  ([`mbox.go`](../mbox.go))
- **Folded values** are unfolded as RFC 5322 section 3.2.2 says under `Parse`
  with every profile but `ProfileStrict`: line endings (CRLF, and the bare LF
  and CR recovery accepts) are removed and the fold's white space kept; the
  white space after the colon is not part of the value, and a line that lost
  its indentation is joined with one space. `Read`, `ReadWithOptions`,
  `ProfileStrict` and `AddRaw` keep the legacy whitespace-collapsed value.
  The original raw bytes are preserved separately either way.
  ([`textproto/header.go`](../textproto/header.go) · `unfoldValue`,
  `trimAroundNewlines`; issue #63)
- **Empty and header-only input** is framed identically under every `Profile`
  and `Compatibility` (`ROB-010-001`). An empty input, an input that is only the
  empty line, and complete CRLF-terminated fields (folded ones included) ended
  by `io.EOF` all yield an entity with exactly the fields read, raw bytes and
  order kept, and an empty body; after the empty line every byte, blank lines
  included, is body. The missing body or empty line adds no error or diagnostic
  of its own, and no profile requires RFC 5322's mandatory fields; what the
  fields declare — an unknown encoding or charset, a Content-Type to repair or,
  under Strict, refuse — is interpreted as for any message. A header only
  `io.EOF` could end is still being read at that `io.EOF`, so an input error,
  cancellation or no-progress stop there leaves no entity, as for any header. A
  header-only multipart with a usable boundary has no part (`NextPart`: an error
  wrapping `io.EOF`, then `io.EOF`; the empty-container policy is
  `ROB-060-009`'s). Other line endings are described above (`ROB-010-002`); a
  first line beginning with white space is the malformed prefix above
  (`ROB-020-001`), a damaged line with continuation lines an orphan record
  (`ROB-020-002`), and other malformed lines are `ROB-020`'s.
  ([`parse.go`](../parse.go) · `Parse`, [`textproto/header.go`](../textproto/header.go) · `ReadHeader`)
- **Over-long header keys** do not crash the line-folder (a naive fold width would
  go negative; it falls back to the hard limit). ([`textproto/header.go`](../textproto/header.go) · `formatHeaderField`)

### Transfer encoding

- **Tolerant decoders.** Every entry point but `ProfileStrict` — `Parse`,
  and `Read`, `ReadWithOptions` and `New`, which record nothing — decodes
  Base64, quoted-printable and uuencode with the decoders of
  [`cte_tolerant.go`](../cte_tolerant.go): junk, URL-safe characters, data
  after padding, bad escapes and missing `begin`/`end` lines are decoded as
  far as the bytes say anything, and under `Parse` each kind of repair is
  recorded once per body ([`transfer-encodings.md`](robustness-notes/transfer-encodings.md)).
- **Malformed quoted-printable under `ProfileStrict`.** A `lenientQPReader`
  normalizes input that would otherwise break the standard library's reader:
  a stray `=` becomes `=3D`, raw control bytes are hex-escaped, and over-long
  lines get a soft break. Conformant QP passes through unchanged.
  ([`encoding.go`](../encoding.go) · `lenientQPReader`)
- **Indented base64 under `ProfileStrict`.** Leading spaces/tabs (against
  spec) are rewritten to newlines before the standard library decodes.
  ([`encoding.go`](../encoding.go) · `whitespaceReplacingReader`)
- **Line wrapping on write** at 76 columns (base64) / 998 columns (7bit, 8bit),
  with lone `LF` upgraded to `CRLF`. ([`encoding.go`](../encoding.go) · `lineWrapper`)
- **Unknown Content-Transfer-Encoding** is a warning, not a fatal error: the raw
  (undecoded) body is returned together with an `IsUnknownEncoding` error.
  ([`entity.go`](../entity.go) · `newWithOptions`)
- **CTE on multipart is ignored** per RFC 2045 §6.4 — a `multipart/*` part is
  never run through a transfer decoder even if one is (illegally) declared.
  ([`entity.go`](../entity.go) · `newWithOptions`)

### Multipart

- **Consecutive boundaries** produce an empty part (with an empty header) rather
  than a parse error. ([`textproto/multipart.go`](../textproto/multipart.go) · `populateHeaders`)
- **CRLF, bare LF, bare CR and mixed delimiter lines** (`ROB-010-002`). A
  delimiter is the line ending before it (`\r\n` matched whole, else a bare `\n`
  or `\r`; none at the start of a body), `--`, the boundary, `--` for the
  closing one, spaces and tabs, and its own line ending — or, for a closing one,
  the end of input. There is no mode: each delimiter's endings are its own.
  Anything that only begins like a delimiter (`--B-x`, `--B--x`, `--B junk`)
  is body, every byte; a delimiter a read failure cut short is body followed by
  that failure. A `\r` before a line that is exactly a delimiter makes it one —
  the inherent reading of CR-delimited input. White space before the ending may
  run to `MaxLineBytes`; with no line budget the line must fit in 4 KiB, as
  before, on the legacy entry points, while a disabled `MaxLineBytes` under
  `Parse` reads it at any length (#64). That budget is charged on the candidate's valid prefix however the
  input arrives, so one past it is refused before the byte that breaks it. Under
  `Parse`, `multipart.bare_lf`, `multipart.bare_cr` and
  `multipart.mixed_line_endings` are recorded once per parse, and
  `ProfileStrict` refuses the rest of a multipart with a bare-CR delimiter
  ending — also after a call that met it failed for another reason, such as a
  malformed part header, which that call reports first; LF and LF+CRLF are
  accepted.
  ([`textproto/multipart.go`](../textproto/multipart.go) · `Part.Read`, [`line_endings.go`](../line_endings.go))
- **Missing closing boundary.** On EOF before the final `--boundary--`, buffered
  bytes are flushed as body and an `ErrMissingBoundaryClose` is reported, instead
  of dropping data or looping forever. ([`textproto/multipart.go`](../textproto/multipart.go) · `scanBody` / `bodyFailure`)
  Under `Parse` (not `ProfileStrict`) the last part instead ends with `io.EOF`,
  `NextPart` returns `io.EOF`, and `multipart.close_not_found` (Incomplete) is
  recorded ([`multipart-recovery.md`](robustness-notes/multipart-recovery.md)).
- **The input after the message, under `ProfileForensic`, on request**
  (`ROB-010-005`). The iterators end at the closing delimiter of the message's
  own multipart body and do not read on; the buffered readers may already
  have fetched some of what follows, counted against `MaxInputBytes`, as they
  fetch ahead everywhere. RFC 2046 calls what follows the epilogue and says to
  ignore it, and it is also where a concatenated message would be. Under
  `ProfileForensic` only, once an iterator of that body (`NextPart`, `Walk`,
  `mail.Reader`) returns `io.EOF` at a proven closing delimiter,
  `ParseSession.TrailingInputReader` offers one reader of the bytes after the
  delimiter's line ending, and `ParseSession.TrailingInput` a snapshot: status
  (`unknown`, `none`, `open`, `ended`, `failed`, `stopped`), absolute start
  (with a known flag), bytes delivered, the error that ended it, and evidence.
  Offering it reads no input (it allocates bounded state); nothing more is read
  for it until the caller reads it; the bytes are delivered verbatim and once,
  from the buffers and then the input, and are not retained or replayable.
  Reading the root's `Body` or `WriteTo` — whose verbatim copy of a multipart
  body includes the epilogue — consumes those bytes as body, as before, and is
  no inspection; mixing either, or a second iterator, with the reader is not
  supported. While open, a non-empty read reports a stop of the parse
  (cancellation included, even with the bytes already buffered) before and
  after reading; exact `io.EOF` ends it; any other input error is returned as
  itself and records no error diagnostic, though message-like evidence already
  proved is still reported. The first end is final: a later stop or
  cancellation does not replace an `io.EOF` or input error already returned. A
  zero-length read does no I/O and no inspection. `MaxInputBytes` counts it as
  any read, with its existing prefetch semantics: a stop keeps what was
  delivered, but not every admitted byte is promised to be delivered.
  `MaxEpilogueBytes` does not bound this reader; it bounds what
  `MultipartEpilogue` keeps of an epilogue, and for the message's own body
  under `ProfileForensic` that epilogue is read through this reader
  ([`multipart_epilogue.go`](../multipart_epilogue.go),
  [`multipart-recovery.md`](robustness-notes/multipart-recovery.md)). A message that
  frames no parts reports `none`: its body runs to the end of the input, and a
  message pasted into it is body — no body is scanned, split or flagged.
  Evidence comes from the first 4096 bytes delivered: after blank lines, an
  mbox `From_` line of `ROB-010-004`'s grammar, or two header field lines
  (name, `:`, then space, tab or line ending; continuation lines between
  allowed, a blank line ending the candidate) are *message-like*, which records
  one `input.trailing_messages` diagnostic at Info. Line endings are the
  longest match; a `\r` that ends the proof is decided by the byte after it
  (a `\n` is included in the evidence, another byte is not) or by `io.EOF`
  itself, an ordinary failure first leaving it undecided, and a next byte past
  the window deciding against. That is a suspicion only: a conformant epilogue
  can look the same, and a concatenated message that does not begin that way
  within the window is not recognized. An ordinary, blank, empty or binary
  epilogue records nothing. The preamble and epilogue of every multipart a
  healing parse iterates, nested ones included, are offered by
  `MultipartPreamble` and `MultipartEpilogue`, and the input's raw bytes by a
  `Source` ([`raw-source.md`](robustness-notes/raw-source.md)). Other profiles
  are unchanged.
  ([`trailing.go`](../trailing.go), [`textproto/multipart.go`](../textproto/multipart.go) · `MultipartReader.Epilogue`)
- **Preamble** before the first boundary is skipped by the iterators. Under
  `Parse` (not `ProfileStrict`) up to `MaxPreambleBytes` of it is kept,
  returned by `MultipartPreamble`, and read for boundary inference; a longer
  one records `limit.preamble_bytes`. ([`textproto/multipart.go`](../textproto/multipart.go) · `nextPart`, `keepPreamble`; [`multipart_heal.go`](../multipart_heal.go) · `MultipartPreamble`)
- **Trailing whitespace** after a boundary delimiter is tolerated. ([`textproto/multipart.go`](../textproto/multipart.go) · `skipLWSPChar`)
- **Malformed/empty `boundary` parameter on write** is re-synchronized: the writer
  rewrites the Content-Type header to match the boundary it actually emits, so the
  header and body delimiters never disagree. ([`writer.go`](../writer.go) · `createWriter`)

### Charset and header text

- **Empty or missing charset** is treated as unspecified (effectively us-ascii)
  and passed through without error. ([`charset.go`](../charset.go) · `charsetReader`)
- **Malformed UTF-7 shifts** in the optional `charset` plugin are preserved
  literally instead of making the body unreadable; valid shifted sequences are
  still decoded as standard UTF-7. ([`charset/utf7.go`](../charset/utf7.go) · `standardUTF7Decoder`)
- **Korean `cp949` / `x-windows-949` labels** (any case) are accepted by the
  optional `charset` plugin as code page 949, the same decoder `euc-kr`,
  `windows-949` and `ks_c_5601-1987` use, extended Hangul syllables included,
  for bodies and RFC 2047 words alike.
  ([`charset/charset.go`](../charset/charset.go) · `charsets`)
- **Missing or wrong charset declarations** are not guessed by default.
  Under `Parse` (not `ProfileStrict`), a text body whose byte order mark
  contradicts the declared label, or that is UTF-8 under a legacy label, is
  decoded as declared and reported at Info (`charset.bom_mismatch`,
  `charset.declaration_suspect`), naming the other encoding; only with
  `ParseOptions.RecoverCharsetDeclarations` is it decoded as the bytes say
  (issue #65; [`charset_bom.go`](../charset_bom.go),
  [`charset-plugin.md`](robustness-notes/charset-plugin.md)). With
  `ParseOptions.FallbackCharset`, a text body that declares no charset, UTF-8
  or US-ASCII and is not UTF-8 is decoded with the fallback
  (`charset.missing`, `charset.declaration_suspect`;
  [`charset_monitor.go`](../charset_monitor.go)).
- **Malformed encoded-words** are decoded as far as they go and the rest of
  the value is decoded around them; a word whose charset has no decoder is
  left as written. In parameter values the same decoder is used, and text
  outside encoded words is passed through byte for byte.
  ([`rfc2047.go`](../rfc2047.go), [`rfc2047.md`](robustness-notes/rfc2047.md))
- **Missing Content-Type** defaults to `text/plain`. ([`header.go`](../header.go) · `ContentType`)

### `mail` convenience layer

- **Unknown transfer-encoding and unknown-charset parts are surfaced** alike, as
  in the `message` layer: `CreateReader` returns a usable `Reader` and `NextPart`
  a usable `Part` together with the `IsUnknownEncoding` / `IsUnknownCharset`
  warning (the charset one when both stages are unknown), the body is what the
  skipped stage left undecoded, and the next `NextPart` continues with the
  following sibling. An error that matches a warning predicate but comes without
  a usable result, and a `FatalError` even when it wraps a warning, are not
  warnings: the call returns no `Reader` / `Part`. A `FatalError` from a
  `Parse` entity — a budget or a `DiagnosticSink` stop — is the session's stop,
  so later calls repeat it; other errors, such as an I/O failure, carry no such
  guarantee.
  ([`mail/reader.go`](../mail/reader.go) · `recoverable`)
- **`CreateReader` bounds only the message header**, through `message.Read`. For
  the per-entity budgets and structured diagnostics, use `message.Parse` and pass
  the entity to `mail.NewReader`; stop rather than iterate when `Parse` itself
  returns a `FatalError`. ([`mail/reader.go`](../mail/reader.go) · `CreateReader`)
- **Ambiguous parts default to attachments.** A part is treated as inline only if
  its disposition is `inline`, or it is `text/*` without an explicit `attachment`
  disposition; everything else is an attachment. ([`mail/reader.go`](../mail/reader.go) · `NextPart`)

## Not supported / rejected

These are deliberate boundaries, each with a reason.

- **Writing a non-UTF-8 charset is rejected.** The writer only accepts `utf-8` or
  `us-ascii` (or empty); anything else returns an `unhandled charset` error. This
  prevents silently shipping bytes under a label emlkit can't actually produce.
  Non-UTF-8 is decoded on *read* but never emitted on *write* (a deliberate
  asymmetry). ([`writer.go`](../writer.go) · `createWriter`)
- **Decoding non-UTF-8 needs the `charset` plugin.** Out of the box only
  utf-8 / us-ascii are understood; other charsets yield an `IsUnknownCharset`
  warning. Add `import _ "github.com/saeroun-ai/emlkit/charset"` to decode ~all
  common charsets. Even unknown, the **raw body is still readable**.
  ([`charset.go`](../charset.go) · `charsetReader`, [`charset/charset.go`](../charset/charset.go) · `init`)
- **The `charset` plugin is opt-in.** It pulls in `golang.org/x/text` charset
  tables (~1 MiB), so it is a side-effect import you add only if you need it.
  ([`charset/charset.go`](../charset/charset.go))
- **No mbox archive handling.** emlkit parses a single message, not an mbox
  archive. Apart from the one opt-in leading envelope line above (`ROB-010-004`),
  which is kept separately rather than swallowed, there is no special-casing of
  `From ` lines: no splitting at later `From ` lines, no `>From` unescaping, no
  `Content-Length` interpretation, no inferred message end, and a `From ` line
  anywhere else — a second one, one inside the header, one in a part header or
  a body — is read as it always was. A `From_` line at the start of the input
  after a multipart message's closing delimiter is only evidence that a
  `ProfileForensic` caller may read about (`ROB-010-005`, above); nothing is
  split there either. `Read`, `ReadWithOptions`, `New`,
  `textproto` and the `mail` constructors never recognize an envelope, and no
  writer writes one; the input's own bytes, the line included, are readable
  through a `Source` (`ParseAt`, `ParseSource`;
  [`raw-source.md`](robustness-notes/raw-source.md)). This is not a claim of
  parity with any mbox reader. ([`mbox.go`](../mbox.go), [`textproto/header.go`](../textproto/header.go) · `ReadHeader`)
- **An `Entity` is consumed once.** The body is a stream; reading it (or calling
  `Walk`) exhausts the entity. ([`entity.go`](../entity.go) · `Walk`) A parse
  from `ParseAt` or `ParseSource` keeps the input readable beside the stream:
  `RawReader`, `RawHeader`, `RawBody` and a fresh `DecodedBody` per entity
  ([`source.go`](../source.go), [`raw-source.md`](robustness-notes/raw-source.md)),
  and `mail.Spool` keeps decoded bodies for repeated reads
  ([`spool.md`](robustness-notes/spool.md)).
- **Encoded-words are written as `Q` only**, never `B`. ([`charset.go`](../charset.go) · `encodeHeader`)
- **Header size is bounded.** Every header block defaults to a 1 MiB cap
  (`Limits.MaxHeaderBytes`, or the same budget spelled
  `ReadOptions.MaxHeaderBytes`; set either to `-1` to disable). The budget is per
  entity and never cumulative: it counts one block as it appears on the wire,
  through the empty line that ends it, so a block of exactly the budget is read
  and one byte more is refused. A refused block yields **no header at all**,
  never the fields that happened to fit, and only a budget this parser enforced
  counts as one. A refusal is identified with `textproto.HeaderLimit`, so an
  `io.Reader` that fails with `textproto.ErrHeaderTooBig`, or hands back a
  `HeaderLimitError` value of its own, stays an ordinary input error rather than
  a diagnosed budget.
  Under `Parse` it applies to the message header **and to the header of every
  part at any depth**; a refused block records one `limit.header_bytes`
  diagnostic and ends that scope with a `FatalError`, leaving the entities and
  siblings already returned valid. `Read` and `ReadWithOptions` keep their older
  behavior unchanged: the budget applies to the message header only, part headers
  are unbounded, and a refused message header is the ordinary error it always
  was, with nothing recorded.
  ([`limit_header.go`](../limit_header.go), [`textproto/header.go`](../textproto/header.go) · `ReadHeaderLimit`)
- **All header blocks of a parse are bounded together.** Under `Parse`,
  `MaxTotalHeaderBytes` (32 MiB) and `MaxTotalHeaderCount` (100 000) bound the
  wire bytes and field candidates of the message header and every part header
  the parse reads, summed; framing, preamble, epilogue, bodies, drained parts,
  peeks and read-ahead are not charged. Bytes are charged per read of the 4 KiB
  header buffer after the per-entity budgets admitted it, a read that does not
  fit is refused whole (so usage may stay below the budget while the diagnostic
  names the first byte past it), and nothing is refunded. Once the count is
  spent, the next field candidate is refused at its first byte before it is
  read; a malformed prefix — first lines beginning with white space
  (`ROB-020-001`) — is such a candidate.
  Exceeding either records one `limit.total_header_bytes` or
  `limit.total_header_count` diagnostic and ends the whole parse with one
  `FatalError`; content already returned stays valid. `Read` and
  `ReadWithOptions` have no totals. Keyed `Limits` literals inherit these
  defaults. ([`limit_header_total.go`](../limit_header_total.go),
  [`textproto/header_budget.go`](../textproto/header_budget.go))
- **A header block is bounded four ways, not one.** Alongside `MaxHeaderBytes`,
  `Parse` enforces `MaxHeaderFieldBytes` (64 KiB), `MaxHeaderCount` (1 024) and
  `MaxLineBytes` (64 KiB) on the header block of every entity at any depth. Each
  counts a different thing, and each counts it as it appears on the wire:
  `MaxHeaderFieldBytes` counts one **logical** field — its first physical line,
  every continuation line, and the line terminators of each; `MaxHeaderCount`
  counts logical field **candidates**, including one later skipped for an empty
  name, so a block of nothing but colons cannot buy extra fields;
  `MaxLineBytes` counts one **physical** line **excluding** its terminator. The
  empty line that ends a block belongs to the block and to no field in it:
  `MaxHeaderBytes` counts its bytes and `MaxLineBytes` sees a line of empty
  content, while `MaxHeaderFieldBytes` and `MaxHeaderCount` charge it nothing, so
  a header that is nothing but that empty line is read under any budget down to
  one byte. Every one is per entity and never cumulative. Exceeding any is all-or-nothing:
  the block yields no header, records exactly one `limit.header_field_bytes`,
  `limit.header_count` or `limit.line_bytes` diagnostic at `LimitExceeded`
  severity, and ends that scope with a `FatalError`, because a block that overran
  leaves the input somewhere the parser cannot resynchronize to. When one block
  exceeds several at once, the budget bounding the **smallest scope** is the one
  reported, in the order line, field, block, count.
  ([`limit_header.go`](../limit_header.go), [`textproto/header.go`](../textproto/header.go) · `headerScan`)
- **`MaxLineBytes` bounds syntax lines, never body content.** Its scope is
  deliberately narrow: header physical lines, multipart framing lines (preamble,
  delimiters, and the empty line before one), and the line-oriented framing of
  the `base64` and `quoted-printable` transfer encodings. An opaque `7bit`,
  `8bit`, `binary` or unencoded body containing no line terminator is **not**
  treated as one enormous line — it is streamed, never accumulated into a line
  buffer, and never refused for its length alone. Even for the framed encodings
  the check is a streaming monitor that holds no bytes and refuses only once a
  terminator **proves** the line it ended was over budget, so an encoded body
  that never terminates a line costs fixed memory and is not refused. What bounds
  opaque bodies is `MaxEncodedBodyBytes` / `MaxDecodedBodyBytes`.
  ([`limit_line_bytes.go`](../limit_line_bytes.go) · `cteLineMonitor`)
- **Parameter lists are bounded, and losing them does not cost the entity.**
  `MaxParameterCount` (256) bounds the raw semicolon-separated parameter
  occurrences of one `Content-Type` or `Content-Disposition`, and
  `MaxParameterSegments` (256) bounds the RFC 2231 continuation segments per
  case-insensitive base parameter. Both are counted by a quote-aware scan that
  runs **before** `mime.ParseMediaType`, so a semicolon inside a quoted string
  separates nothing, duplicates and segments each count as occurrences, and the
  work is bounded by the budget rather than by the field. Unlike the header
  budgets this is **not fatal**: there is a safe fallback, so the media type and
  the parameters within the budget are kept, the remainder is ignored, one
  `limit.parameter_count` or `limit.parameter_segments` diagnostic records what
  was kept, and parsing continues into the body — a `boundary` parameter that
  arrived before the cut still yields the multipart children it introduces. One
  deliberate loss: a parameter with any RFC 2231 segment among the retained
  occurrences is dropped whole rather than reported from a series the scan
  stopped reading. ([`limit_parameters.go`](../limit_parameters.go))
- **The parameter budgets reach the parse engine, not the public accessors.**
  `Header.ContentType` and `Header.ContentDisposition` parse whatever they are
  given under no budget, exactly as they always have: a `Header` may never have
  come from a parse, so there is no configuration to hold it to. The budgets
  apply where the engine itself interprets those fields on the entities `Parse`
  produces. A caller parsing a field of unknown provenance bounds it itself, or
  reaches it through `Parse`. ([`header.go`](../header.go) · `ContentType`)
- **The MIME tree is bounded two ways: how deep it goes and how many parts it
  has.** `Parse` enforces `MaxMIMEDepth` (100) and `MaxPartCount` (10 000), and
  each counts a different thing. **Depth**: the message itself is depth **zero**,
  whatever its media type, and every child a multipart body yields is exactly one
  deeper than the entity whose body it came out of, so a budget of N admits every
  entity at depth N or less. **Count**: the root is **not** counted — nothing
  discovered it — while every child a multipart iterator does discover counts
  one, a leaf, a container holding further parts and a headerless empty part
  alike. `MaxPartCount` is **one budget for the whole tree**, shared by every
  iterator and never reset on entering a container, so many small containers are
  bounded exactly as one wide container is; there is deliberately **no**
  per-container part budget. A tree exactly as deep or as wide as its budget is
  read whole and ends with an ordinary `io.EOF`. Both are charged lazily, where a
  child is discovered and not before: a container sitting at the depth budget may
  hold nothing, so refusing it in advance would turn an ordinary end of input
  into a defect. The cost of that is a look-ahead of exactly **one over-limit
  child header** — it has to be read to learn the child exists, it is bounded by
  the header budgets like any other, and nothing else about that child is touched
  (no body, no decoder, no entity, none of its own parts). **The two failures
  differ in scope**: `limit.mime_depth` ends only the multipart iterator that hit
  it — the parent, the earlier siblings and every shallower part stay valid and
  the parent's iteration continues — while `limit.part_count` ends the **whole
  parse**, so every later scope of it, including iterators built earlier for
  other containers and `Entity.Walk`, reports the identical `FatalError` and one
  diagnostic is recorded for the message. A child exceeding both at once reports
  `limit.part_count`, which is charged first; a child whose header block exceeds
  a header budget never frames at all and reports only that. ([`limit_tree.go`](../limit_tree.go))
- **A session's failure is published once and never replaced.** Whatever ends a
  whole parse — a `DiagnosticSink` that refuses to continue, or `MaxPartCount` —
  every scope of that parse reports the **same error value**, so two errors a
  caller holds from two scopes can be compared. Deciding which of the two a parse
  ends with needs the `limit.part_count` diagnostic delivered to the sink first,
  and delivery calls caller code, which never happens under the session lock; so
  the overflow is published as a two-phase transition. One caller claims it and
  nothing is published, that caller delivers the diagnostic with no lock held, and
  it then publishes exactly one failure — the budget's if the sink took the
  diagnostic, the sink's if the sink stopped the parse — before releasing anyone
  else. Anything asking about the failure in between waits for that publication
  rather than seeing a provisional answer, which is safe because the synchronous
  sink contract already blocks the parse for as long as the sink blocks. A sink
  may still read the session it is delivering for: `Diagnostics`, `Len`, `Profile`
  and `Compatibility` take no part in the transition. Exactly one caller ever
  claims an overflow, so the diagnostic is recorded once and delivered once
  however many scopes race into it.
  ([`session.go`](../session.go) · `publishStop`, [`limit_tree.go`](../limit_tree.go) · `chargePart`/`resolvePart`)
- **`MaxMessageDepth` bounds embedded messages when they are read.** A
  `message/rfc822` or `message/global` body is a leaf until the caller asks
  for the message inside it: `Entity.EmbeddedMessage`, `Entity.Walk` and
  `Entity.Drain` under `ParseOptions.WalkEmbeddedMessages`, or `mail.ReadTree`
  with `TreeOptions.DescendMessages`. The message itself is depth zero; an
  embedded message past the budget (32 by default) is not read: one
  `limit.message_depth` diagnostic and an entity-scoped `FatalError`, with the
  carrier, its body and the parse left as they were. An embedded message that
  is read is one part of `MaxPartCount` and one level of `MaxMIMEDepth`; a
  carrier nobody asks about costs one part and nothing inside it is counted.
  ([`embedded.go`](../embedded.go),
  [`embedded-messages.md`](robustness-notes/embedded-messages.md)) The doc
  comments of `Limits` and `Limits.MaxMessageDepth` in `limits.go` still call
  it reserved; the code in `embedded.go` enforces it.
- **`MaxInputBytes` is counted once, at the input, and only as it is read.**
  Under `Parse` one reader at the raw source bounds every read of the parse —
  header, bodies, multipart framing and part headers, and the bytes an iterator
  drains past a part the caller skipped — so nothing is counted twice and no
  path reads around it, `io.WriterTo`/`io.ReaderFrom` copies included. A budget
  `N` admits `N` bytes; only an actual byte after them is an overflow, learned by
  reading that one byte, which is never delivered. The overflow records one
  `limit.input_bytes` diagnostic at offset `N`, with the message's nil path
  because the source cannot know which entity it was feeding, and publishes the
  session's stop; the source is never read again. An `io.EOF` or I/O error with
  no byte past the budget, and a `(0, nil)` read, are passed through unchanged;
  100 consecutive `(0, nil)` reads are judged below the limiter as
  `input.no_progress` (`ROB-001-007`, see `progress.go`).
  The budget counts raw bytes as they are read, not as they are interpreted:
  body bytes buffered ahead while the header was read count, and fetching up to
  exactly `N` reads no proof byte; bytes never read from the input — an unread
  body tail, an epilogue — are never checked, so the size of the whole message
  beyond them stays unknown. A nested part header ending exactly at the budget
  may still need the next byte to rule out the enclosing delimiter. A sink stop
  during the delivery, or a stop already published, wins. `Read` and
  `ReadWithOptions` have no total input budget; their message-header cap still
  applies. ([`limit_input.go`](../limit_input.go))
- **`Parse` caps diagnostics with `MaxDiagnosticCount` without capping
  parsing.** A positive budget `N` counts every diagnostic the session would
  record, whether or not it retains and whether or not it has a sink. The first
  `N-1` are recorded and delivered as they are; in place of the `N`-th, one
  `limit.diagnostic_count` diagnostic at `SeverityLimitExceeded` is recorded and
  delivered, carrying the path and location of the one it replaced; every later
  diagnostic is omitted: the session neither copies, retains nor delivers it
  (a producer may already have built and formatted it). `N=1` makes the first
  diagnostic the terminal, a parse with no diagnostic records none, and a parse
  with exactly `N` diagnostics records it too, so it means "recording stopped",
  not "more than `N` defects". Nothing recorded before it is replaced. The
  budget alone ends no scope: metadata is still healed and every body and later
  part is still returned unless something else ends them. Every other budget,
  Strict refusal and sink stop still ends its scope with its own `FatalError` —
  including when its own diagnostic is the request the terminal replaced, or one
  omitted after it, and that `FatalError` still carries the failure's own
  diagnostic. A sink that refuses the terminal stops the parse like any other
  refusal, and a stop already published keeps its identity. The default is
  1000; `LimitDisabled` records everything; the count stops at the budget, so
  `math.MaxInt64` cannot overflow. Every "records one diagnostic" statement in
  this document holds while the budget has a slot left.
  ([`limit_diagnostics.go`](../limit_diagnostics.go))
- **Leaf bodies are budgeted as they stream.** `MaxEncodedBodyBytes` (128 MiB)
  bounds the wire bytes of one leaf body — an entity whose `MultipartReader` is
  nil — as its decoders or the verbatim `WriteTo` copy read them;
  `MaxDecodedBodyBytes` (128 MiB) bounds what its `Body` delivers after every
  decoding stage, in bytes, so a cut may split a UTF-8 character; and
  `MaxTotalDecodedBytes` (512 MiB) bounds what every leaf `Body` delivers
  together. Each permits at most `N`; reaching `N` is not an overflow, and one
  proof byte past it is read, never delivered. A per-body overflow records one
  diagnostic and ends that body with a `FatalError`; the header and the
  delivered prefix stay valid, output the decoders buffered from admitted wire
  bytes is still delivered within the decoded budgets, and the parent can drain
  the unread raw tail to the next part unless another limit or a sink stops it.
  The total ends the parse and stops all further decoded output, earlier bodies
  included. A framed container's raw body is outside these budgets and bounded
  by `MaxInputBytes`. Whether or not any is enforced, the body's own failure,
  or this parse's `MaxInputBytes` failure when that is what the body's input
  returned, is reported after the decoders end, instead of their `io.EOF` or
  own error. That input failure is recognized by provenance — the exact value
  the input limiter fails with, joined only by the multipart framing — and the
  guard calls no `Is`, `As` or `Unwrap` of a source's own error and never
  replaces one; a decoder may still reword an ordinary source error, as it
  always has. Nothing of the one-shot `Body` is replayed, and the verbatim
  `WriteTo` path stays a conditional one-shot copy; a parse given a `Source`
  can read the raw body again, and `DecodedBody` decodes it again under the
  same per-body budgets ([`raw-source.md`](robustness-notes/raw-source.md)).
  ([`limit_body.go`](../limit_body.go))
- **Two budgets in `Limits` bound the spool, not the parse.** Beside the
  fifteen above, `MaxMessageDepth` (above), `MaxPreambleBytes` (the preamble a
  healing multipart iterator keeps, and how far boundary inference and the
  look-ahead past a shared close read; `limit.preamble_bytes`) and
  `MaxEpilogueBytes` (what `MultipartEpilogue` keeps; `limit.epilogue_bytes`)
  are enforced ([`multipart-recovery.md`](robustness-notes/multipart-recovery.md),
  [`nested-boundaries.md`](robustness-notes/nested-boundaries.md)).
  `MaxMemorySpoolBytes` and `MaxDiskSpoolBytes` are applied by `mail.Spool`
  to the tree `mail.ReadTree` reads from the parse, beside the spool's own
  `SpoolOptions` quotas; the smaller room wins (#43). A body that spills from
  memory goes to disk whole and is not reported, so `limit.memory_spool_bytes`
  is never emitted; a body cut on disk records `limit.disk_spool_bytes` in
  `Spool.Diagnostics()`. The parse itself retains nothing under them.
  ([`limits.go`](../limits.go), [`mail/spool_limits.go`](../mail/spool_limits.go),
  [`spool.md`](robustness-notes/spool.md))

## Behavioral guarantees

- **Verbatim round-trip (DKIM-friendly).** A header field that was parsed and not
  modified is re-serialized byte-for-byte — including original whitespace, folding,
  and field order. Each field keeps its raw bytes; only mutated or synthesized
  fields are reformatted. This is what makes parsed-then-written messages safe for
  DKIM signatures. ([`textproto/header.go`](../textproto/header.go) · `headerField.raw` / `WriteHeader`)
  This is per field, not a replay of the input: `Entity.WriteTo` adds
  `Mime-Version: 1.0` at the top of a header without one, always writes the
  empty line that ends the header, and otherwise follows `CreateWriter` (which
  may, for example, set a multipart boundary or drop a multipart's
  `Content-Transfer-Encoding`). So an empty input is written as
  `Mime-Version: 1.0\r\n\r\n`, and a header of unmodified ordinary fields that
  `io.EOF` ended gains that field and that line; writing such output again
  gives the same bytes. Under a healing `Parse` an unchanged header that has
  `MIME-Version` is written as read when its body is copied as read
  ([`header-recovery.md`](robustness-notes/header-recovery.md)), and the
  input's own bytes are readable through a `Source`
  ([`raw-source.md`](robustness-notes/raw-source.md)).
  ([`entity.go`](../entity.go) · `WriteTo`)
- **Body byte-preservation.** When the output transfer-encoding and charset match
  the input (case-insensitively), the original undecoded body bytes are copied
  through verbatim instead of decoded and re-encoded — preserving exact bytes for
  body hashes. ([`entity.go`](../entity.go) · `writeBodyTo`)
- **Error with a usable result.** `Read`, `New`, `MultipartReader.NextPart`,
  `mail.CreateReader` and `mail.Reader.NextPart` may return an error that
  satisfies `IsUnknownCharset` or `IsUnknownEncoding` **together with a fully
  usable** `Entity` / `Reader` / `Part`. Treat these predicates as warnings only
  when the result is non-nil and the error is not `IsFatal` (the undecoded body
  is still readable); bail on every other error.
  ([`charset.go`](../charset.go) · `IsUnknownCharset`, [`encoding.go`](../encoding.go) · `IsUnknownEncoding`, [`mail/reader.go`](../mail/reader.go) · `CreateReader`)
- **Copy-on-write headers.** Writer constructors copy the caller's `Header` before
  mutating it, so building a writer never changes the caller's view.
  ([`writer.go`](../writer.go) · `CreateWriter`, [`mail/writer.go`](../mail/writer.go))
- **Sealed interfaces.** `HeaderFields` and `mail.PartHeader` have unexported marker
  methods, so they cannot be implemented outside the module; new concrete types
  belong inside emlkit. ([`textproto/header.go`](../textproto/header.go), [`mail/reader.go`](../mail/reader.go) · `PartHeader`)
