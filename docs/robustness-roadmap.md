# EML Parser Robustness Roadmap

This document is the implementation ledger for evolving `emlkit` into a
production-grade, best-effort EML parser whose robustness meets or exceeds
Eclipse Angus Mail and other widely deployed MIME engines.

It is intentionally more specific than a feature list. Every item has a stable
ID, an observable behavior, and a verification contract. Work is performed one
item at a time. An item is complete only after its implementation, focused
tests, full regression tests, formatting check, and this ledger have all been
reviewed.

The list is meant to be extensible, not frozen. A newly discovered malformed
message must first become a minimized regression fixture and a requirement or
diagnostic entry here before its parser repair is accepted.

## 1. Objective

For every finite input and configured resource budget, the parser must do all
of the following:

1. Never panic, deadlock, spin forever, recurse without a bound, or allocate
   without a bound because of input bytes.
2. Return every entity, header, and body byte that can be recovered
   unambiguously or by an explicitly documented deterministic repair.
3. Preserve original bytes independently of decoded or repaired views.
4. Report every observed defect and every applied repair; recovery must never
   silently change the interpretation of a message.
5. Isolate local damage. A broken child part must not discard already parsed
   siblings or the containing message.
6. Produce the same result regardless of input `Read` chunking.
7. Retain the existing streaming and DKIM-friendly behavior for conformant
   input.

“Accept everything” is not the objective. When multiple interpretations are
plausible, the parser must preserve raw data, choose only according to a named
policy, and emit a diagnostic describing the ambiguity.

## 2. Reference behavior

Compatibility is evaluated against several independent references rather than
one implementation:

- RFC 5322, RFC 6532: Internet Message Format and internationalized headers.
- RFC 2045–2047, RFC 2183, RFC 2231: MIME, encoded words, dispositions, and
  extended parameters.
- RFC 3464, RFC 3798, RFC 8098: delivery, disposition, and feedback reports.
- Eclipse Angus Mail: real-world JavaMail compatibility and repair switches.
- Apache James Mime4j: tolerant streaming parsing and explicit resource limits.
- MimeKit: loose/strict structured-header parsing, nesting limits, and a broad
  interoperability corpus.
- Python `email`: per-entity defect collection and partial-result semantics.
- Go standard library behavior where `emlkit` delegates parsing or decoding.

Differing reference results are not automatically bugs. They must be recorded
in the differential corpus with the chosen `emlkit` behavior and rationale.

### 2.1 Pinned Angus source baseline

The initial compatibility audit was performed on 2026-08-12 against immutable
upstream revisions, not only generated API documentation:

- Eclipse Angus Mail 2.0.5, commit
  [`a7a4a37844717d3967418b1640456e49153a7e7c`](https://github.com/eclipse-ee4j/angus-mail/tree/a7a4a37844717d3967418b1640456e49153a7e7c).
- Jakarta Mail API 2.1.5, commit
  [`e1873219fe587d5c2b5bb4196e859ae71319a45c`](https://github.com/jakartaee/mail-api/tree/e1873219fe587d5c2b5bb4196e859ae71319a45c).

This split matters. The `MimeMessage`, `MimeMultipart`, `MimeBodyPart`, header,
address, date, and parameter parsers are implemented in Jakarta Mail API;
Angus supplies the runtime provider, line reader, transfer decoders, MIME data
handlers, and DSN support. “Angus-compatible” in this ledger refers to the
combined behavior of those two pinned releases unless an item names a different
baseline.

### 2.2 Source-to-ledger traceability

| Upstream source inspected | Behavior used by this roadmap | Primary ledger coverage |
|---|---|---|
| [`MimeMessage.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/MimeMessage.java), [`MimeBodyPart.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/MimeBodyPart.java) | Lazy body/content handling, filename decoding policy, illegal multipart/message CTE handling, nested message handling, and cached multipart content. | `ROB-000`, `ROB-070`, `ROB-090`, `ROB-100`, `ROB-110` |
| [`MimeMultipart.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/MimeMultipart.java) | Boundary inference, declared-boundary policy, missing closing boundary recovery, empty multipart policy, delimiter validation, preamble preservation, and completion state. | `ROB-060` |
| [`InternetHeaders.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/InternetHeaders.java), [`HeaderTokenizer.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/HeaderTokenizer.java) | Continuations, malformed fields, whitespace-only separators, comments, quoted strings, and raw header retention. | `ROB-010`, `ROB-020`, `ROB-040`, `ROB-050` |
| [`ParameterList.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/ParameterList.java), [`ContentType.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/ContentType.java), [`ContentDisposition.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/ContentDisposition.java) | Loose parameter parsing, Apple/Windows filename cases, RFC 2231 continuations, partial decode fallback, and disposition strictness. | `ROB-050` |
| [`MimeUtility.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/MimeUtility.java), [`InternetAddress.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/InternetAddress.java), [`MailDateFormat.java`](https://github.com/jakartaee/mail-api/blob/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/main/java/jakarta/mail/internet/MailDateFormat.java) | Encoded-word strictness, unknown encoding policy, UTF-8 policy, address recovery, groups/comments, and obsolete date recovery. | `ROB-030`, `ROB-040`, `ROB-070`, `ROB-080` |
| [`LineInputStream.java`](https://github.com/eclipse-ee4j/angus-mail/blob/a7a4a37844717d3967418b1640456e49153a7e7c/core/src/main/java/org/eclipse/angus/mail/util/LineInputStream.java) | CRLF, LF, CR, pushback/reset, EOF, and split-read line behavior. | `ROB-001`, `ROB-010`, `ROB-020` |
| [`BASE64DecoderStream.java`](https://github.com/eclipse-ee4j/angus-mail/blob/a7a4a37844717d3967418b1640456e49153a7e7c/core/src/main/java/org/eclipse/angus/mail/util/BASE64DecoderStream.java), [`QPDecoderStream.java`](https://github.com/eclipse-ee4j/angus-mail/blob/a7a4a37844717d3967418b1640456e49153a7e7c/core/src/main/java/org/eclipse/angus/mail/util/QPDecoderStream.java), [`QDecoderStream.java`](https://github.com/eclipse-ee4j/angus-mail/blob/a7a4a37844717d3967418b1640456e49153a7e7c/core/src/main/java/org/eclipse/angus/mail/util/QDecoderStream.java), [`UUDecoderStream.java`](https://github.com/eclipse-ee4j/angus-mail/blob/a7a4a37844717d3967418b1640456e49153a7e7c/core/src/main/java/org/eclipse/angus/mail/util/UUDecoderStream.java) | Decoder error tolerance, whitespace/junk handling, truncation, padding, and uuencode recovery switches. | `ROB-070` |
| [`message_rfc822.java`](https://github.com/eclipse-ee4j/angus-mail/blob/a7a4a37844717d3967418b1640456e49153a7e7c/core/src/main/java/org/eclipse/angus/mail/handlers/message_rfc822.java), [`multipart_mixed.java`](https://github.com/eclipse-ee4j/angus-mail/blob/a7a4a37844717d3967418b1640456e49153a7e7c/core/src/main/java/org/eclipse/angus/mail/handlers/multipart_mixed.java), [DSN module](https://github.com/eclipse-ee4j/angus-mail/tree/a7a4a37844717d3967418b1640456e49153a7e7c/dsn/src/main/java/org/eclipse/angus/mail/dsn) | Nested messages, multipart materialization, delivery-status structures, and raw fallback. | `ROB-090`, `ROB-100` |
| [Jakarta Mail MIME tests](https://github.com/jakartaee/mail-api/tree/e1873219fe587d5c2b5bb4196e859ae71319a45c/api/src/test/java/jakarta/mail/internet), [Angus provider tests](https://github.com/eclipse-ee4j/angus-mail/tree/a7a4a37844717d3967418b1640456e49153a7e7c/providers/angus-mail/src/test) | Regression examples and expected compatibility behavior. These are inputs to, not substitutes for, emlkit's own minimized corpus. | `ROB-130` |

### 2.3 Parse-facing Angus compatibility switches

These upstream properties expose behaviors that emlkit must represent through
typed profiles or policies. emlkit must not copy Java process-global system
properties into its API.

| Angus/Jakarta property | Pinned default | Required emlkit coverage |
|---|---:|---|
| `mail.mime.multipart.ignoremissingendboundary` | `true` | Recover the final part to EOF, mark the container incomplete, and diagnose the missing close (`ROB-060-005`). |
| `mail.mime.multipart.ignoremissingboundaryparameter` | `true` | Infer the first credible delimiter while rejecting all-dash false candidates (`ROB-060-002`, `ROB-060-004`). |
| `mail.mime.multipart.ignoreexistingboundaryparameter` | `false` | Prefer the declared boundary unless an explicit compatibility policy permits replacement (`ROB-060-003`). |
| `mail.mime.multipart.allowempty` | `false` | Make empty-container handling profile-controlled (`ROB-060-009`). |
| `mail.mime.ignorewhitespacelines` | `false` | Make whitespace-only header separators explicit and diagnosable (`ROB-020-004`). |
| `mail.mime.decodeparameters` / `mail.mime.encodeparameters` | `true` / `true` | Decode RFC 2231 while retaining original parameter occurrences (`ROB-050-006`, `ROB-050-007`). |
| `mail.mime.decodeparameters.strict` | `false` | On decode failure, preserve and expose raw/partial values (`ROB-050-007`). |
| `mail.mime.parameters.strict` | `true` | Offer a named loose-policy path for broken parameter tails (`ROB-050-003`). |
| `mail.mime.applefilenames` / `mail.mime.windowsfilenames` | `false` / `false` | Cover client-specific filename repairs explicitly (`ROB-050-004`). |
| `mail.mime.splitlongparameters` | `true` | Accept continuation segments without assuming arrival order (`ROB-050-006`). |
| `mail.mime.contentdisposition.strict` | `true` | Preserve invalid disposition metadata and expose a tolerant typed view (`ROB-050-001`–`ROB-050-005`). |
| `mail.mime.decodetext.strict` | `true` | Make embedded encoded-word recovery profile-controlled (`ROB-030-002`). |
| `mail.mime.ignoreunknownencoding` | `false` | Preserve raw body and make strict/tolerant behavior explicit at every API layer (`ROB-070-002`). |
| `mail.mime.allowutf8` | scope-dependent | Support RFC 6532 deliberately; do not inherit inconsistent class-local defaults (`ROB-030-004`, `ROB-040-005`, `ROB-080`). |
| `mail.mime.decodefilename` | `false` | Decode nonstandard RFC 2047 filenames only under tolerant policy (`ROB-050-008`). |
| `mail.mime.ignoremultipartencoding` | `true` | Diagnose and policy-control illegal multipart CTE (`ROB-060-012`). |
| `mail.mime.allowencodedmessages` | `false` | Diagnose and policy-control illegal encoded `message/*` containers (`ROB-090-001`). |
| `mail.mime.base64.ignoreerrors` | `false` | Supply explicit strict and recovering Base64 modes with prefix/raw fallback (`ROB-070-004`–`ROB-070-007`). |
| `mail.mime.uudecode.ignoreerrors` / `mail.mime.uudecode.ignoremissingbeginend` | `false` / `false` | Support compatible uuencode aliases and opt-in repairs (`ROB-070-009`). |
| `mail.mime.address.ignorebogusgroupname` | `true` | Recover common bogus group-name forms without losing valid neighbors (`ROB-040-001`–`ROB-040-004`). |

Additional parse-relevant switches tracked by the corpus include
`mail.mime.address.strict`, `mail.mime.address.usecanonicalhostname`,
`mail.mime.foldencodedwords`, `mail.mime.foldtext`, and
`mail.mime.contenttypehandler`. Write-only formatting and host-name resolution
must stay outside the core parse-recovery policy unless a ledger item explicitly
requires them.

### 2.4 Current emlkit gap snapshot

The source audit also identified concrete baseline gaps. These are observations
at the start of the roadmap, not claims that every existing path is defective:

- `newWithOptions` discards `Header.ContentType` errors, so malformed MIME
  metadata can silently change entity classification (`ROB-050-001`). Under
  `Parse`, a valid base with malformed parameters is now repaired or (Strict)
  refused with a diagnostic (`ROB-050-001`). An invalid base is still reported
  as the raw value with the error and no diagnostic (`ROB-050-002`). `Read`,
  `ReadWithOptions` and `New` are unchanged by design.
- ~~The top-level header has a default size limit, but child-part headers do not
  receive an equivalent per-entity budget~~ — closed by `ROB-001-002`, which
  applies `MaxHeaderBytes` per entity at every depth under `Parse`. The legacy
  `Read`/`ReadWithOptions` paths keep the older behavior by design.
- A missing multipart boundary parameter is not inferred (`ROB-060-002`).
- A missing closing boundary exposes recovered data but returns a generic
  unexpected-EOF error instead of a structured incomplete result
  (`ROB-000-003`, `ROB-060-005`).
- Existing quoted-printable and Base64 repairs are not surfaced as structured
  diagnostics (`ROB-000`, `ROB-070`).
- ~~Unknown charset and unknown transfer encoding warnings are not accumulated
  independently, and the `mail` convenience layer does not treat both
  consistently~~ — accumulated independently since `ROB-000-005`, and treated
  alike by `mail.Reader` since `ROB-100-006`.
- Limits are not yet unified across field count/size, line size, nesting, part
  count, decoded expansion, and diagnostics (`ROB-001`).
- Cancellation and repeated `(0, nil)` reader progress handling are not yet a
  single parser-wide contract (`ROB-001-006`, `ROB-001-007`). `ROB-001-006`
  adds `ParseContext`; `ROB-001-007` adds the no-progress rule
  under `Parse`, below.
- There is no checked-in native fuzz/differential corpus proving behavior
  against Angus and other references (`ROB-130`).

### 2.5 Evidence maintenance rule

Every compatibility item must add a minimized fixture and record the exact
reference version, profile/switches, result tree, recovered bytes, diagnostics,
and intentional differences. Upstream source changes are reviewed by diffing a
new pinned revision; a moving branch or undocumented memory of JavaMail behavior
is never sufficient evidence. Large-provider parser internals and corpora are
generally proprietary, so “provider-grade” is an engineering target established
by public standards, measured corpora, fuzzing, differential tests, and explicit
resource guarantees—not a claim of unseen implementation parity.

## 3. Status vocabulary

| Status | Meaning |
|---|---|
| `Planned` | Requirement is specified but implementation has not started. |
| `Implementing` | Exactly one executor currently owns the item. |
| `Review` | Implementation exists; independent diff and test review is pending. |
| `Done` | Acceptance criteria and all verification gates passed. |
| `Blocked` | A documented external or architectural dependency prevents work. |

Only the reviewing orchestrator moves an item to `Done`. An implementation
executor may move its assigned item only as far as `Review`.

## 4. Parser profiles

### 4.1 Strict

- RFC violations that affect framing or semantics stop the current entity.
- Already parsed parent/sibling results remain available.
- Unsupported but syntactically valid features are not RFC violations; their
  raw form remains usable.

### 4.2 Tolerant

- This is the recommended production profile.
- Known deterministic repairs are applied and reported.
- Truncated entities return usable partial data and an `Incomplete` diagnostic.
- Ambiguous repairs use stable precedence rules documented by their diagnostic.

### 4.3 Forensic

- Includes all tolerant results and diagnostics.
- Preserves raw spans for message, headers, fields, preamble, epilogue, parts,
  transfer-encoded bodies, and uninterpreted damaged regions.
- Does not discard bytes merely because they cannot be assigned a semantic
  meaning.

### 4.4 Compatibility

Named compatibility policies may tune individual choices without creating
separate parsers:

- `AngusCompatible`
- `MimeKitCompatible`
- `PythonEmailCompatible`
- `Conservative` (prefer raw/incomplete over speculative structure)

The default remains `Tolerant + Conservative` unless a later compatibility
study justifies another choice.

## 5. Result and diagnostic contract

### 5.1 Required result model

The target API needs a parse session because body and multipart diagnostics can
be discovered lazily:

```go
session, entity, err := Parse(r, options)
// Consume Entity.Body, MultipartReader, or session.Walk(...).
diagnostics := session.Diagnostics()
```

Existing `Read`, `ReadWithOptions`, `Entity.Walk`, and `mail.Reader` APIs remain
source compatible. They may wrap the new engine. New APIs must not force the
entire body into memory.

### 5.2 Diagnostic fields

Every diagnostic must be a structured value with at least:

- stable `Code`;
- `Severity`;
- human-readable `Message` that is not used for program logic;
- MIME `Path` using zero-based part indices;
- absolute byte `Offset` when known;
- optional `EndOffset`, `Line`, and `Column`;
- affected header field name when applicable;
- `Action` describing the repair or fallback;
- wrapped low-level `Cause` when applicable.

Diagnostics must be append-only, safe to read after parsing, and ordered by
discovery. Implementations must cap their count using the configured budget.

### 5.3 Severity classes

| Severity | Meaning |
|---|---|
| `Info` | Nonstandard but harmless representation was observed. |
| `Recovered` | A deterministic repair was applied and parsing continued. |
| `Incomplete` | EOF/truncation lost syntax, but partial data is usable. |
| `Unsupported` | Syntax is recognized but no semantic decoder is available. |
| `InvalidMetadata` | Raw content is usable but typed metadata could not be parsed. |
| `LimitExceeded` | A configured safety budget was reached. |
| `Fatal` | The parser cannot make safe forward progress for the current scope. |

### 5.4 Stable diagnostic registry

Codes below are reserved from the beginning. An implementation may add more
specific codes, but must not reuse an existing code for another meaning.

#### Input and limits

- `input.empty`
- `input.bom`
- `input.read_error`
- `input.no_progress`
- `input.cancelled`
- `input.trailing_messages`
- `limit.input_bytes`
- `limit.header_bytes`
- `limit.header_field_bytes`
- `limit.header_count`
- `limit.line_bytes`
- `limit.total_header_bytes`
- `limit.total_header_count`
- `limit.parameter_count`
- `limit.parameter_segments`
- `limit.mime_depth`
- `limit.message_depth`
- `limit.part_count`
- `limit.preamble_bytes`
- `limit.epilogue_bytes`
- `limit.encoded_body_bytes`
- `limit.decoded_body_bytes`
- `limit.total_decoded_bytes`
- `limit.diagnostic_count`
- `limit.memory_spool_bytes`
- `limit.disk_spool_bytes`

#### Header framing and fields

- `header.first_line_continuation`
- `header.orphan_continuation`
- `header.missing_body_separator`
- `header.whitespace_body_separator`
- `header.missing_colon`
- `header.semicolon_separator`
- `header.empty_name`
- `header.invalid_name`
- `header.name_whitespace`
- `header.invalid_utf8`
- `header.nul`
- `header.bare_lf`
- `header.bare_cr`
- `header.mixed_line_endings`
- `header.truncated_line`
- `header.duplicate_singleton`
- `header.mbox_envelope`
- `header.misplaced_mbox_envelope`
- `header.decoder_no_progress` (added by `ROB-001-007`)

#### MIME metadata and parameters

- `mime.content_type_invalid`
- `mime.content_type_missing_type`
- `mime.content_type_missing_subtype`
- `mime.content_type_missing_slash`
- `mime.disposition_invalid`
- `mime.parameter_missing_name`
- `mime.parameter_missing_equals`
- `mime.parameter_missing_value`
- `mime.parameter_unquoted_value`
- `mime.parameter_unclosed_quote`
- `mime.parameter_invalid_escape`
- `mime.parameter_duplicate`
- `mime.parameter_raw_utf8`
- `mime.parameter_rfc2047`
- `mime.parameter_bad_percent_escape`
- `mime.parameter_segment_missing`
- `mime.parameter_segment_duplicate`
- `mime.parameter_segment_mixed_encoding`
- `mime.parameter_unknown_charset`
- `mime.filename_conflict`

#### Multipart

- `multipart.boundary_missing`
- `multipart.boundary_empty`
- `multipart.boundary_invalid`
- `multipart.boundary_too_long`
- `multipart.boundary_inferred`
- `multipart.boundary_mismatch`
- `multipart.boundary_ambiguous`
- `multipart.start_not_found`
- `multipart.close_not_found`
- `multipart.close_truncated`
- `multipart.invalid_delimiter_suffix`
- `multipart.mixed_line_endings`
- `multipart.bare_cr` (added by ROB-010-002)
- `multipart.bare_lf` (added by ROB-010-002)
- `multipart.empty`
- `multipart.empty_part`
- `multipart.part_header_missing`
- `multipart.part_separator_missing`
- `multipart.nested_boundary_reused`
- `multipart.false_boundary_candidate`
- `multipart.trailing_junk`
- `multipart.illegal_transfer_encoding`

#### Transfer encodings

- `cte.unknown`
- `cte.invalid_token`
- `cte.illegal_for_multipart`
- `cte.illegal_for_message`
- `cte.seven_bit_violation`
- `cte.base64_junk`
- `cte.base64_padding_missing`
- `cte.base64_padding_invalid`
- `cte.base64_data_after_padding`
- `cte.base64_length_invalid`
- `cte.base64_truncated`
- `cte.base64_url_alphabet`
- `cte.qp_invalid_hex`
- `cte.qp_trailing_equals`
- `cte.qp_invalid_soft_break`
- `cte.qp_control_byte`
- `cte.qp_overlong_line`
- `cte.uuencode_invalid`
- `cte.uuencode_missing_begin`
- `cte.uuencode_missing_end`

#### Charset and encoded words

- `charset.missing`
- `charset.empty`
- `charset.unknown`
- `charset.alias_recovered`
- `charset.bom_mismatch`
- `charset.invalid_sequence`
- `charset.decode_truncated`
- `charset.declaration_suspect`
- `rfc2047.invalid_word`
- `rfc2047.unknown_charset`
- `rfc2047.unknown_encoding`
- `rfc2047.invalid_base64`
- `rfc2047.invalid_q`
- `rfc2047.illegal_location`
- `rfc2047.embedded_word`
- `rfc2047.raw_utf8`
- `rfc2047.header_injection`

#### Structured metadata

- `address.invalid`
- `address.partial_list`
- `address.unquoted_comma`
- `address.local_only`
- `address.group_depth`
- `address.comment_depth`
- `address.raw_utf8`
- `date.invalid`
- `date.obsolete_zone`
- `date.missing_zone`
- `date.weekday_mismatch`
- `message_id.invalid`
- `message_id.partial_list`
- `received.invalid`
- `list_header.invalid`

#### Nested and special entities

- `message.embedded_invalid`
- `message.embedded_truncated`
- `message.illegal_transfer_encoding`
- `report.invalid`
- `signed.structure_invalid`
- `encrypted.structure_invalid`
- `feature.unsupported_media_type`

#### Body decoding

- `body.decoder_no_progress` (added by `ROB-001-007`)

### 5.5 Resource budgets and defaults

Every budget in the `limit.*` registry above is a field of the public `Limits`
value, is an `int64`, and obeys one rule: `0` selects the documented default,
`LimitDisabled` (`-1`) turns that budget off, any other positive value is the
budget itself, and anything below `-1` is a configuration error. `math.MaxInt64`
is a finite budget, not a synonym for disabled. `DefaultLimits()` returns the
table below fully resolved.

These defaults are finite operational starting points that balance
interoperability against denial-of-service resistance. They are **not** a
conformance boundary. RFC 5322 section 2.1.1 requires each physical line to be
no more than 998 characters excluding CRLF and recommends no more than 78. That
line rule does not itself establish overall message, header-block, or logical-
field budgets, and MIME subformats can impose additional constraints. The
defaults are **not** measurements of, nor claims about, the caps of any mail
provider either. Every deployment is expected to tune them from the corpus it
actually handles.

| Field | Default | Unit | Reserved diagnostic code |
|---|---:|---|---|
| `MaxInputBytes` | 256 MiB | bytes of one message | `limit.input_bytes` |
| `MaxHeaderBytes` | 1 MiB | bytes of one header block | `limit.header_bytes` |
| `MaxHeaderFieldBytes` | 64 KiB | bytes of one logical field | `limit.header_field_bytes` |
| `MaxHeaderCount` | 1 024 | fields per header block | `limit.header_count` |
| `MaxLineBytes` | 64 KiB | bytes of one physical syntax line | `limit.line_bytes` |
| `MaxTotalHeaderBytes` | 32 MiB | header-block bytes of the whole parse | `limit.total_header_bytes` |
| `MaxTotalHeaderCount` | 100 000 | header field candidates of the whole parse | `limit.total_header_count` |
| `MaxParameterCount` | 256 | parameters per header field | `limit.parameter_count` |
| `MaxParameterSegments` | 256 | RFC 2231 segments per parameter | `limit.parameter_segments` |
| `MaxMIMEDepth` | 100 | nested multipart containers | `limit.mime_depth` |
| `MaxMessageDepth` | 32 | nested `message/*` entities | `limit.message_depth` |
| `MaxPartCount` | 10 000 | parts in the whole tree | `limit.part_count` |
| `MaxPreambleBytes` | 1 MiB | bytes before the first delimiter | `limit.preamble_bytes` |
| `MaxEpilogueBytes` | 1 MiB | bytes after the closing delimiter | `limit.epilogue_bytes` |
| `MaxEncodedBodyBytes` | 128 MiB | wire bytes of one body | `limit.encoded_body_bytes` |
| `MaxDecodedBodyBytes` | 128 MiB | decoded bytes of one body | `limit.decoded_body_bytes` |
| `MaxTotalDecodedBytes` | 512 MiB | decoded bytes of all entities | `limit.total_decoded_bytes` |
| `MaxDiagnosticCount` | 1 000 | diagnostics per parse | `limit.diagnostic_count` |
| `MaxMemorySpoolBytes` | 8 MiB | in-memory retained raw content | `limit.memory_spool_bytes` |
| `MaxDiskSpoolBytes` | 1 GiB | temporary-file bytes per message | `limit.disk_spool_bytes` |

`MaxHeaderBytes` has a second, older spelling, `ReadOptions.MaxHeaderBytes`, with
the same units and the same meaning for `0` and `-1`. `Parse` resolves both
through one resolver: either alone is honored, both together are accepted only
while they resolve to the same budget, and a disagreement is a configuration
error rather than a silent choice between them. `Read` and `ReadWithOptions`
resolve nothing and keep their documented legacy behavior, in which any negative
value means no limit.

`MaxLineBytes` has a deliberately narrow scope, fixed here before anything
enforces it. It bounds a *physical syntax line* — one that a parser component
must materialize or interpret as a line, which is principally header and MIME
framing syntax: header field lines and continuations, multipart delimiters, and
the line-oriented framing of the transfer encodings. Its 64 KiB default is
deliberately much higher than RFC 5322's 998-character limit: it is a tolerant
operational ceiling that permits bounded recovery of malformed or extension-
generated syntax, not a conformance threshold. It must never turn an opaque
text, binary or encoded body containing no newline into one giant line
allocation, and must never reject such a body for its length alone. Opaque
bodies are streamed and are governed by `MaxEncodedBodyBytes` and
`MaxDecodedBodyBytes`. `ROB-001-003` implemented the budget within that scope.

A configuration error is the caller's, not the message's: `Parse` reports it
before reading a byte, returns no entity and records no diagnostic, and still
returns a session carrying the requested profile and policy.

`ROB-001-001` centralized configuration only and enforced nothing new.
`ROB-001-002` added the first enforcement, `MaxHeaderBytes`. `ROB-001-003` added
five more — `MaxHeaderFieldBytes`, `MaxHeaderCount`, `MaxLineBytes`,
`MaxParameterCount` and `MaxParameterSegments`. `ROB-001-004` added the two tree
budgets, `MaxMIMEDepth` and `MaxPartCount`. `ROB-001-009` added
`MaxInputBytes`. `ROB-001-008` added `MaxDiagnosticCount`. `ROB-001-005` added
the three body budgets. `ROB-001-010` added the two shared header totals, so
**fifteen** budgets are enforced, all of them under
`Parse` alone.

`MaxEncodedBodyBytes`, `MaxDecodedBodyBytes` and `MaxTotalDecodedBytes`
(`ROB-001-005`, [issue #4](https://github.com/saeroun-ai/emlkit/issues/4)) bound
**leaf** bodies: entities whose `MultipartReader` returns nil at construction —
everything not `multipart/*`, `message/*` included, and a multipart whose
damaged `Content-Type` was repaired with no usable boundary (the `unframed`
repair; a clean multipart with a missing boundary is still framed and is other
pending work). A framed container has a raw body too, but that raw view is
outside the leaf budgets: read directly or copied verbatim by `WriteTo` it is
bounded by `MaxInputBytes` alone, and iterating it holds each leaf to its own
budgets, so an oversized part never makes its container overflow before the
parser can step past it, and no byte is charged twice.

The encoded budget counts a leaf's wire bytes as they are read for that body,
by its decoders or by `WriteTo`'s verbatim copy. The decoded budgets count the
bytes `Entity.Body` delivers after every stage that ran, a caller's
`CharsetReader` included — bytes, not characters, so a cut may fall inside a
UTF-8 sequence. `N` admits `N`; only a byte that exists past it is an overflow,
proven by reading exactly that one byte (one wire byte, which no decoder sees,
or one decoded byte), which is never delivered or counted. A read that brings
no byte passes `io.EOF`, an error or `(0, nil)` through unchanged.

A per-body overflow is local: one diagnostic for the leaf (`encoded` located at
the first byte not admitted, `decoded` at the body start, since decoded bytes
have no input position), a sticky `FatalError` from that `Body`, `ParseStop`
nil, the header and delivered prefix kept, and the parent iterator draining the
unread raw tail under `MaxInputBytes` to continue with the next sibling. A body
reports one local failure, the first proven among the encoded limiter, the
encoded line monitor and the decoded guard; when encoded and decoded budgets are
nearly reached together, which is proven first can depend on decoder read-ahead
and read sizes. A total overflow ends the parse: one diagnostic for the leaf
that proved it, the session's stop published (a sink refusing it wins), and no
leaf `Body` delivers further decoded output. That includes one that already
failed locally: the local error a caller holds stays valid, but a later read of
that body returns the total stop, with no input read and nothing recorded. A
byte over both the total and a body's decoded budget is the total's failure. A
stop published earlier by a sink or another budget outranks a later body
refusal, which then records nothing; that earlier stop alone does not end
reading a body already returned. Every failure is enforced past
`MaxDiagnosticCount`.

Decoders cannot hide what ended a body. Under `Parse` every leaf `Body` is
guarded, whether or not any body budget is enforced: the guard lets the
decoders deliver, within the decoded budgets, the output they made of what they
were given, including output buffered when the failing read happened, and then
reports the body's local
failure or, failing that, this parse's `MaxInputBytes` failure when that is
what the body's input returned, instead of the `io.EOF` or error the decoders
ended with. The input's failure is recognized by provenance, not by asking an
error about itself: it is the exact value the input limiter fails with (its own
stop, or a sink stop published before it that it adopted), either as is for the
root or joined first by the multipart framing once per enclosing part, at most
the entity's depth; a join is recognized by its standard-library type before
its `Unwrap` runs. The guard calls no `Is`, `As` or `Unwrap` of an error a
caller's source returned and never replaces one, so such an error — one whose
`Unwrap` cycles or whose `As` panics included — is never asked anything by it;
a plain body hands it back unchanged as before this item, while a decoder may
still reword it, as quoted-printable rewords an ordinary read error. A first
pass that asked every source error `IsFatal` regressed exactly that and was
corrected in review. Nothing is claimed about what an arbitrary decoder does
with the errors it is given. The wider predicate audit is `ROB-120-007` (#16),
which leaves this guard's identity test as it is. This fixes a defect present before
this item (recorded on [issue #3](https://github.com/saeroun-ai/emlkit/issues/3)):
a quoted-printable body whose soft line break the input budget cut, such as
`foo=\r\nbar` with the budget ending after the `\r`, ended with the decoder's ordinary
"invalid bytes after =" error while `ParseStop` held the `limit.input_bytes`
stop. Only that failure is restored, so a plain I/O error stays the decoder's
report, and a stop published for another reason is not looked up. A cut base64 quantum is dropped and a cut quoted-printable escape is
kept literally, as the tolerant decoder keeps a malformed one; the decoders are
not rewritten. `Read`, `ReadWithOptions` and `New` are unchanged.

Raw recovery is one-shot, and this item adds no raw API or replay. The header
and the delivered prefix stay valid. The existing verbatim `WriteTo` path stays
what it was, a conditional copy: it applies only before `Body` is read and only
when the output encoding and charset match the entity's, the writer still
refuses a `Content-Type` it cannot interpret before writing anything
(`ROB-120-008`) even when `Parse` healed the entity's view of it, and a
`CharsetReader` that reads eagerly may already have consumed raw input. Where
it applies to a leaf it is held to the encoded and input budgets and not to the
decoded ones. Wire bytes a body consumed, its proof byte, decoder read-ahead and
a tail the parent drained are not replayed; replay and spooling are #41. A
caller's `CharsetReader` is bounded only from outside, by what it reads and
delivers; its allocation, CPU and blocking are not (`ROB-080-008`,
`ROB-001-006`). With every default, a single body larger than 128 MiB now meets
`MaxEncodedBodyBytes` before `MaxInputBytes`.

`MaxDiagnosticCount` (`ROB-001-008`, [issue #8](https://github.com/saeroun-ai/emlkit/issues/8))
bounds diagnostics, not parsing. Every request the session would record takes
one slot of a positive budget `N`, whether or not the session retains and
whether or not it has a sink, so it bounds what a sink receives as well as what
the session keeps. The last slot is reserved: requests `1` through `N-1` are
recorded and delivered as they are, request `N` is replaced by one
`limit.diagnostic_count` diagnostic at `SeverityLimitExceeded` carrying that
request's path and location and an `Action` saying that request and every later
one are omitted and that this budget alone does not stop parsing (it does not
claim the parse went on: the replaced request may itself be a failure that ends
it, and the sink may refuse the terminal), and every later request is discarded
before a parse context copies a path into it, before it is retained and without
a sink call. `N = 1` makes the first request
the terminal, and a parse with no diagnostic records none. Because request `N`
itself takes the terminal slot, the terminal means "recording stopped", never
"an `N+1`-th defect was observed" or a total count of defects; nothing recorded
before it is replaced, so the session stays append-only and reports at most `N`
entries, the terminal last. The count is compared before it is incremented and
stops at `N`, so `math.MaxInt64` cannot overflow; `LimitDisabled` counts nothing.
The terminal is built inside the recording call rather than recorded by a
second one, so it cannot recurse. Producers that already built a diagnostic —
including formatting its message — still pay for building it; only what
follows — the context's path copy, the session's copy, retention and delivery
— is bounded.

Reaching the budget ends no scope by itself. Metadata is still healed, every
body is still read and every later part is still returned unless something
else ends them. Every other failure is decided by its own code independently of
whether its diagnostic was kept, including when its diagnostic is the very
request the terminal replaced: an input, header or tree budget, or a Strict
refusal, still ends its scope or the parse with its own `FatalError`, which
still carries that failure's own diagnostic (the `MaxInputBytes` and
`MaxPartCount` transitions publish their stop unconditionally after recording),
and a sink that refuses the terminal stops the parse like any other refusal. A
stop already published keeps its identity, a failed sink is never called again,
and an omitted request returns exactly what an ordinary request returns once the
sink is not called — the published stop only when the sink had failed, nil
otherwise — so it can neither replace nor hide a published stop, which scopes
read from the session as before. The terminal returns nil unless the caller's
sink refuses it or had already failed. Every "records exactly one" diagnostic
statement elsewhere in this roadmap holds while the budget has a slot left. This generalizes
the earlier statement that a parse reaching the budget ends with the terminal
entry: the terminal is the last diagnostic recorded, not the last event of the
parse.

`MaxInputBytes` (`ROB-001-009`, [issue #3](https://github.com/saeroun-ai/emlkit/issues/3))
is counted once, by one reader at the raw source under the offset counter, so
the root header, every body, multipart framing and part headers, and the bytes
an iterator drains past a skipped part all spend the same budget and none is
counted twice. A budget `N` admits `N` bytes; reaching `N` is neither an
overflow nor an end of input. Once `N` bytes are admitted the next read asks the
source for exactly one byte: a byte — whatever error accompanies it — proves the
overflow and is never handed on; no byte passes the source's `io.EOF`, error or
`(0, nil)` through unchanged, and nothing is retried there; repeated `(0, nil)`
is judged below it, at the source, by `ROB-001-007`. Reads before `N` are only shortened, so offsets and the bytes
before `N` do not depend on chunking, and an empty read asks the source for
nothing. The limiter implements `io.Reader` alone, so `io.WriterTo` and
`io.ReaderFrom` copies go through it.

The budget is spent as raw bytes are read from the source, not as they are
interpreted. `Parse` interprets only the header, but the buffered reader above
the source may already have fetched body bytes with it, up to the budget; those
count, and fetching up to exactly `N` reads no proof byte. A tail never read
from the source — an unread body, the epilogue after a closing delimiter — is
not validated, so the size of the whole message beyond it stays unknown, and a
header that ended within the budget is never what proves an overflow. `Read` and
`ReadWithOptions` have no total input budget; their message-header cap is
unchanged. One
consequence is exact rather than incidental: a nested part's header block that
ends exactly at the budget is not returned, because the enclosing multipart
must read the next byte to rule out that its last terminator starts a
delimiter. When one header block would exceed both a header budget and this
one, the header budget wins when its refusal is decided from bytes already
admitted, and this one wins when the header reader needs a byte it refuses first.

An overflow records exactly one `limit.input_bytes` diagnostic at
`SeverityLimitExceeded`, at offset `N`, with the nil path of the message as a
whole — the source cannot know which entity it was about to feed, and no part
path is invented. Its failure belongs to the whole parse and is published as the
session's stop: `Parse` returns it, with no entity, when the root header did not
end within the budget; otherwise the read that reached it, every later read, and
`NextPart`, `Walk`, `mail.Reader` and `ParseStop` return the identical value, and
the source is not read again. A read of a part's body returns an error that
wraps that stop, joined by the multipart framing with `ErrMissingBoundaryClose`.
Unlike the part-count transition this needs no pending state: the overflow is
claimed by the single reader of the stream, the diagnostic is delivered with no
lock held, and only then is the stop published, so a sink may still read the
session and ask `ParseStop`, which answers nil until publication. A sink that
refuses the diagnostic, or a stop published earlier, wins; after an earlier stop
nothing is recorded. Everything already returned stays valid, and a header
block cut short yields no header.

Under `Parse` it bounds one header block per entity — the message's own and
every part's, at any nesting depth — counted as the block appears on the wire
through the empty line that ends it, so a block of exactly the budget is read and
one byte over is refused. The budget is never cumulative across blocks.
`LimitDisabled` turns it off at every depth. A refused block records exactly one
`limit.header_bytes` diagnostic with `LimitExceeded` severity, naming the
entity's path and the offset its header block began at, and ends the scope
reading it with a `FatalError` carrying that diagnostic: the whole parse for the
message header, the iteration over one multipart for a part. Nothing is recovered
from the refused block, and everything already returned stays valid.

Two properties of the refusal are part of the contract. It is all or nothing: a
refused block yields no header, never the fields that fit before the budget ran
out, which no caller could tell apart from a message that lacked the rest. And it
has an identity an input cannot claim. A refusal is identified by
`textproto.HeaderLimit`, which reports only a `*HeaderLimitError` this parser
built, carrying an unexported mark. Neither the public `ErrHeaderTooBig`
sentinel, nor an error answering `Is` for every target, nor a `HeaderLimitError`
value another package wrote — including a typed nil — is accepted as one, so an
`io.Reader` presenting any of them yields an ordinary input error that is not
diagnosed, not fatal, and not dereferenced. Internally the same rule holds by
exact identity rather than by `errors.Is` traversal. The one indistinguishable
case is a genuine refusal captured from the parser and handed back to it; that
is documented on the type as the boundary rather than papered over.

`Read` and `ReadWithOptions` are unchanged: the budget reaches the message's own
header block and no other, it is enforced by bounding the reads under the header
parser as it always was, the failure is the error it has always returned, and no
diagnostic is recorded. A part's header is unbudgeted on those paths.

The five budgets `ROB-001-003` added divide into two groups by what a failure
costs, and the division is part of the contract.

The three remaining header-block budgets behave exactly as `MaxHeaderBytes`
does, per entity and never cumulative, each counted as the block appears on the
wire: `MaxHeaderFieldBytes` counts one logical field — its first physical line,
every continuation line, and the line terminators of each; `MaxHeaderCount`
counts logical field candidates, a candidate later skipped for an empty name
included, so that repeating an empty-name field cannot evade it; `MaxLineBytes`
counts one physical line excluding its terminator.

The empty line that ends a block belongs to the block and to no field in it.
`MaxHeaderBytes` counts its bytes and `MaxLineBytes` sees a line whose content is
empty, while `MaxHeaderFieldBytes` and `MaxHeaderCount` do not charge it at all —
a block holding no field spends neither, so a header that is nothing but that
empty line is read under any budget down to one byte. The distinction is settled
before the line's bytes are charged, from at most two bytes that are looked at
and not consumed, because a refusal is returned rather than taken back and a
field longer than a budget must still be refused while it is read. Exceeding any is all or
nothing, records exactly one diagnostic at `LimitExceeded`, and ends that scope
with a `FatalError`. A block that exceeds several at once reports the budget
bounding the smallest scope — line, then field, then block, then count — a
precedence fixed in the scanner so that it cannot depend on how the input chunks
its reads.

`MaxLineBytes` also reaches the two places outside a header block where the
parser must read a line as a line: a multipart body's framing lines, and the
line-oriented framing of `base64` and `quoted-printable`. Neither bounds opaque
body content. The transfer-encoding side is a streaming monitor that holds no
bytes and refuses only once a terminator proves the line it ended was over
budget, so a body that never terminates a line is streamed at fixed memory cost
and is never refused for its length.

`MaxParameterCount` and `MaxParameterSegments` are the exception to the fatal
rule, because they are the one place with a safe fallback. Both are counted by a
quote-aware scan that runs before `mime.ParseMediaType`, so the work is bounded
by the budget rather than by the field, a semicolon inside a quoted string is not
a separator, and duplicates and RFC 2231 segments each count as occurrences. A
field past either keeps its media type and the parameters within the budget,
ignores the remainder, records one diagnostic whose `Action` says what was kept,
and the parse continues — so a `boundary` obtained before the cut still yields
its children. A parameter with any RFC 2231 segment among the retained
occurrences is dropped whole rather than reported from a series the scan stopped
reading; `ROB-050` owns the wider recovery question. The budgets are per field
and never cumulative across fields or entities, and they reach only the parse
engine's own reading of those fields — `Header.ContentType` and
`Header.ContentDisposition` are unchanged and unbudgeted on every path.

`ROB-001-003` was reopened for two corrections
([issue #1](https://github.com/saeroun-ai/emlkit/issues/1)). First, a multipart
framing line read under `MaxLineBytes` charged a `'\r'` that ended a full
4 096-byte buffer as content before the next read could show it began the
`"\r\n"`, so a line of exactly the budget was refused whenever its terminator
straddled a buffer edge. The framing reader now holds that `'\r'` back and
charges it only when what follows is not `'\n'` or is the end of input, the rule
`headerScan` already applied to header lines (`textproto`
`TestMultipartReader_lineBytesCRAtBufferEdge*`: first/second buffer, after a
short line and after a part, CRLF/LF, `'\r'` as content, every chunking, at
`limit-1/limit/limit+1`). Second, with `SkipTextAttachmentDecoding`, the
`Content-Disposition` read that decides the charset stage could record a
parameter diagnostic whose sink stopped the parse, and the stop was dropped:
the entity came back with a nil error and the charset stage still ran. The stop
is now taken up there, returned with the usable entity as the session's
identical `FatalError`, and no charset decoder follows it
(`TestParse_dispositionBudget*`: root and child, inline and attachment, a sink
error that is or wraps a decoding warning, earlier warnings kept in order, and
the unstopped paths unchanged).

`MaxTotalHeaderBytes` and `MaxTotalHeaderCount` (`ROB-001-010`,
[issue #5](https://github.com/saeroun-ai/emlkit/issues/5)) are the cumulative
bound the per-entity header budgets deliberately are not (the item is in
Review; issue #5 is not closed by this text). Under `Parse` the
message's own header block and every part header block the parse reads, at any
depth (a replaced body's parts included), spend one `textproto.HeaderBudget`
the session creates; `Read`, `ReadWithOptions` and `New` have none.

- Bytes are what `MaxHeaderBytes` counts per block, summed; field candidates
  are what `MaxHeaderCount` counts, summed. Multipart framing, preamble,
  epilogue, bodies, drained bytes, peeks and buffered read-ahead are not
  charged, nor are a part with no header block or a container drained without
  being iterated. Each byte is charged once.
- Bytes are charged per read of the 4 KiB header buffer (a line, or one buffer
  of a longer line), after the line, field and block budgets admitted that
  read, so those win a read that exceeds both. A read that does not fit is
  refused whole and charges nothing: usage stays below `N` by the part of that
  read that would have fitted, while the diagnostic is located at the first
  byte past `N`. This depends on the fixed buffer, never on how the source
  chunks its reads. Nothing is refunded: a block a per-entity budget refuses
  keeps what it had admitted, and a child refused by `MaxPartCount` or
  `MaxMIMEDepth` was read whole and is charged whole.
- A field is counted once read whole. Once `N` fields are counted, the next
  candidate is refused at its first byte before any of it is read, ahead of
  every budget of that candidate, so an exhausted count bounds a candidate with
  every byte budget disabled. The empty line ending a block and an input with
  no byte left are not candidates. A block whose first line begins with white
  space keeps its existing malformed-header error: that line is not a
  candidate, and only byte budgets bound its length.
- Exceeding either ends the whole parse. The crossing block yields no header,
  one `limit.total_header_bytes` or `limit.total_header_count` diagnostic names
  its entity, and one `FatalError` is published through the `MaxPartCount`
  transition, which `Parse` (for the root), every `NextPart`, `Walk`,
  `ParseStop` and `mail.Reader` report. A smaller per-entity refusal on the same
  read keeps its own scope: a child's ends that iterator, and an ancestor may
  still advance unless the session stops. A stop already published — a sink,
  `MaxInputBytes` during the same read, another budget — wins with nothing
  recorded; a sink refusing the diagnostic wins; `MaxDiagnosticCount` may drop
  the diagnostic but not the stop. Content already returned stays valid.
- A failure is recognized only as the exact value the session's budget refused
  with (`HeaderBudget.Refusal`), by pointer comparison; no `Is`, `As` or
  `Unwrap` of a source error is called for it. A total-kind refusal from
  anywhere else is an ordinary input error. The budget records its first
  refusal under its lock, from the charge that did not fit, so `Refusal` and
  the refusal's position name that charge; it then refuses every later charge,
  from any reader, with that same value. A part header's source error that
  unwraps to itself hung the pre-existing `errors.Is` in `textproto` `NextPart`
  (on `b994934` and `b5db93f`); this item did not change that, and
  `ROB-120-007` (#16) removed it, after which the child case of this item's
  provenance test is no longer skipped.
- Additions: two `Limits` fields, and in `textproto` the `HeaderBudget` type,
  `NewHeaderBudget`, `HeaderBudget.Refusal`, `HeaderLimits.Total` and two
  `HeaderLimitKind` values. Positional `Limits{...}` literals no longer
  compile. Keyed literals, the zero `Limits` and `DefaultLimits` compile
  unchanged but enforce the new defaults, so a message they accepted before
  may now be refused; setting the new fields to `LimitDisabled` restores the
  earlier behavior. Future embedded-message parsing (`ROB-090`) must pass the
  same budget to embedded headers.

Every other budget is validated, resolved and reported by `ParseSession.Limits`,
and nothing consults it yet; their `limit.*` codes in the table are reserved, not
yet emitted. `ROB-001-004` through `ROB-001-008` add the enforcement that the
current entity model can exercise, and the diagnostics, behind these same fields.
`ROB-001-010` instead added two fields, with the compatibility effects listed
above.

There is one explicit dependency exception. `MaxMessageDepth` cannot be enforced
until `ROB-090-001` makes `message/rfc822` and `message/global` into nested
entities: today those bodies are opaque leaves, so no nested-message transition
exists to count. `ROB-090-002` therefore owns activating the already-reserved
budget and `limit.message_depth` diagnostic immediately after that model exists.
`ROB-001-004` owns `MaxMIMEDepth` and `MaxPartCount` only. `MaxPartCount` is the
one public part-count budget and is total across the tree; there is no separate
per-container setting. A total bound necessarily bounds one wide container too,
so its tests cover both one container with many children and many containers
whose individually small child sets exceed the same shared total.

`ROB-001-004` settled the two tree budgets as follows.

`MaxMIMEDepth` counts descent. The root entity of a message is at depth zero
whatever its media type is, and every child a multipart body yields is exactly
one deeper than the entity whose body it came out of, so a positive budget `N`
admits every entity at depth `N` or less and the first child it can refuse is one
at depth `N+1`. `MaxPartCount` counts discovery. The root is not counted —
nothing discovered it, it is the message — while every child a multipart iterator
does discover counts exactly one, a leaf, a container that holds further parts
and a part with no header block at all alike. That count is one budget the whole
tree shares, never reset when the parser enters a container.

Both are exact at the boundary and charged lazily. A tree exactly as deep, or as
wide, as its budget is parsed whole, and the end of a container that spent
precisely the budget is an ordinary `io.EOF` rather than a refusal. Nothing is
scanned ahead and nothing is materialized: the check happens where a child has
just been framed, because a container sitting at the depth budget may hold
nothing at all and a budget spent before the parser knows there is something to
spend it on would diagnose children a message never had. The cost of that is a
bounded look-ahead of exactly one over-limit child header block, which has to be
read to learn the child exists; it is bounded by the header budgets like any
other block, and nothing else about that child is touched — no body, no decoder,
no `Entity`, and none of its own parts.

The two failures differ in scope, and that difference follows from what each
budget describes. A `limit.mime_depth` refusal is local to the multipart iterator
that hit it: what ran out is the room to descend below one container, so that
iterator ends with a sticky `FatalError` while the entity holding it, the
siblings already returned, every shallower part, and the parent's own continued
iteration are unaffected. A `limit.part_count` refusal belongs to the whole
parse, because the budget does: the failure is installed on the session, exactly
one diagnostic is recorded for the message, and every later scope — the iterator
that hit it, iterators built earlier for other containers, and `Entity.Walk` —
reports the identical `FatalError` and recovers nothing further. Everything
already returned stays valid either way.

Order is fixed rather than incidental. A failure of the underlying reader — a
part header past a header budget, a framing line past `MaxLineBytes`, an I/O
error — comes first and is reported as itself, since no part was framed and
neither tree budget is consulted. `MaxPartCount` is charged next, and
`MaxMIMEDepth` is judged only for a child that fit within it, so a child
exceeding both at once deterministically reports `limit.part_count`. A
`DiagnosticSink` that stops the parse while it is being told about either
outranks both.

That last clause is where the review of `ROB-001-004` found a defect, and the
requirement it produced is now part of the contract rather than a property of one
implementation. A session's failure is **published once and never replaced**: the
error one scope reports and the error another scope reports are the same value,
for the whole life of the session. That is not free, because deciding *which* of
the two failures a parse ends with requires delivering the `limit.part_count`
diagnostic to the sink first, and delivering to a sink means calling caller code,
which must not happen under the session lock. Publishing the budget's failure
before the sink answers makes a value observable that a sink stop would then have
to replace, which is exactly the identity change the contract forbids.

So the overflow is published as an explicit two-phase transition. One caller
claims the overflow and **nothing is published**; that caller delivers the
diagnostic with no session lock held; it then publishes exactly one failure — the
budget's if the sink took the diagnostic, the sink's if the sink refused to
continue — and only then releases the other callers. Anything that asks the
session about its failure in between waits for that publication rather than being
shown a provisional answer. Waiting is safe under the existing synchronous sink
contract, which already blocks the parse for as long as the sink blocks; the
caller finishing the transition never waits on it itself, and the accessors a
sink is documented to call — `Diagnostics`, `Len`, `Profile`, `Compatibility` —
take no part in it, so a sink may still read the session it is delivering for.
Exactly one caller is ever told it claimed the overflow, which is what makes
"recorded once, delivered once" true however many scopes race into the same
overflow.

The accepted path pays for none of this. Charging a part that fits is the whole
cost — no diagnostic, no message naming the budget, no copy of the part's path —
and that is pinned by an allocation test rather than by a timing one.

A second review of `ROB-001-004` found one more defect, in `Entity.Walk` rather
than in the budgets themselves. Both tree budgets refuse a child by returning no
part, and a `DiagnosticSink` that stops the parse while it is told about the
refusal makes the returned error the session's stop: a `FatalError` wrapping the
sink's own error, which the caller chose and which may wrap an
`UnknownEncodingError` or `UnknownCharsetError`. `Walk` forwarded every error
answering to those two predicates to `walkFunc` together with the nil part and
then descended into it, so a `walkFunc` that continues past warnings made `Walk`
dereference nil instead of returning the identical stop. `Walk` now forwards a
warning only together with the part it describes, and an error that arrives
with no part ends the walk as itself. A refused part header or framing line
leaves `NextPart` the same way and is covered by the same guard.

The same precedence had a second gap. A child whose unknown transfer encoding or
charset made the sink stop the parse comes back usable, together with the
session's stop, and a sink that returns or wraps the diagnostic's `Cause` makes
that stop answer to the warning predicates too; `Walk` then handed the stopped
child to `walkFunc`. `Walk` now asks the session after every `NextPart` and
returns its stop, the identical value, before forwarding anything, so a stopped
child is never delivered while an unstopped parse forwards its warnings exactly
as before.

## 6. Cross-cutting verification matrix

Every parser defect item must cover the applicable dimensions below. Handwritten
tests cover semantic expectations; generators cover the Cartesian product.

### 6.1 Input delivery

- complete buffer;
- one byte per `Read`;
- fixed chunks of 2, 3, 7, 64, 4,095, and 4,096 bytes;
- random deterministic chunks;
- readers returning `(n > 0, io.EOF)`;
- every possible truncation point;
- injected non-EOF error after every significant grammar token;
- cancellation before parse and during header/body reads;
- a guarded misbehaving reader that returns repeated `(0, nil)`.

### 6.2 Placement

- top-level message;
- first, middle, and last multipart child;
- nested multipart child;
- embedded `message/rfc822`;
- signed/encrypted container where raw bytes are especially important.

### 6.3 Representation

- CRLF, LF, CR, and mixed line endings;
- no CTE, Base64, quoted-printable, 7bit, 8bit, binary, and unknown CTE;
- seekable and non-seekable sources;
- strict, tolerant, and forensic profiles.

### 6.4 Assertions

Each applicable test asserts:

1. no panic, hang, goroutine leak, file leak, or unbounded allocation;
2. exact entity tree and part paths;
3. exact raw body and decoded body independently;
4. exact diagnostic codes, severities, paths, and repair actions;
5. deterministic output across chunking variants;
6. strict and tolerant behavior differ only as specified;
7. byte-exact serialization of untouched raw content;
8. resource limit behavior at `limit-1`, `limit`, and `limit+1`;
9. a minimized corpus seed exists for every real-world regression.

## 7. Implementation ledger

### ROB-000 — Parse session, profiles, and diagnostics

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-000-001 | Done | Add public parse profile, diagnostic severity/code/value, and append-only parse session types without breaking existing APIs. | Type/API tests; race-safe ordered collection; documentation examples. |
| ROB-000-002 | Done | Add diagnostic sink/callback support so streaming callers need not retain every diagnostic. | Sink ordering and sink-error policy tests. |
| ROB-000-003 | Done | Define partial-result fatal error semantics and error unwrapping. | `errors.Is/As` tests and usable-result tests. |
| ROB-000-004 | Done | Thread part path and best-known byte offsets through lazy multipart traversal. | Nested-path and chunk-invariance tests. |
| ROB-000-005 | Done | Bridge legacy `IsUnknownCharset`/`IsUnknownEncoding` behavior to structured diagnostics. | Existing API compatibility plus new-session tests. |
| ROB-000-006 | Done | State what a parse has and has not established ([issue #10](https://github.com/saeroun-ai/emlkit/issues/10)): `ParseSession.Completion` reports not-consumed, in-progress, complete, incomplete, limited, canceled, stopped or failed, with read and unread scopes, diagnostic counts and ambiguity; `Entity.Drain` reads what is left so the answer is final. See docs/robustness-notes/completion.md. | Header-only, partial read, skipped part, missing close, budget, sink, cancellation and input-error fixtures. |

### ROB-001 — Budgets, cancellation, and progress

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-001-001 | Done | Centralize all configurable resource budgets and defaults. | Default/disable/overflow configuration tests. |
| ROB-001-002 | Done | Apply message and per-entity header byte limits, including child part headers. | Top/child/deep child boundary tests. |
| ROB-001-003 | Done | Limit header fields, physical line bytes, logical field bytes, parameters, and RFC 2231 segments. | `limit-1/limit/limit+1` tests. |
| ROB-001-004 | Done | Limit multipart MIME depth and total part count without recursive stack growth; nested-message depth is deferred to ROB-090-002. | Deep multipart and single-/multi-container wide generated messages. |
| ROB-001-005 | Done | Limit encoded/decoded body bytes and total decoding expansion under `Parse`: per-leaf `MaxEncodedBodyBytes`/`MaxDecodedBodyBytes` (framed containers are raw views outside them) and the shared `MaxTotalDecodedBytes`, exact at `N` with one undelivered proof byte. | Streaming overflow at `N-1/N/N+1` for identity, base64, quoted-printable, charset and custom expanding/masking/eager decoders, across source and destination chunk sizes: the header and the delivered prefix stay valid, and output decoders buffered from encoded-admitted wire bytes is still delivered within the decoded budgets; a per-body overflow ends that body alone and the parent can drain its unread raw tail under `MaxInputBytes` to reach later and shallower siblings unless another limit or a sink stops it; a total overflow ends the parse. Decoders cannot mask the body's failure or this parse's `MaxInputBytes` failure, recognized by provenance without calling `Is`/`As`/`Unwrap` on source errors, with or without body budgets (fixes the issue #3 quoted-printable masking); the guard never asks or replaces a source's own error, cyclic or panicking ones included, though a decoder may still reword an ordinary one as before. "Recoverable raw body" is the reviewed one-shot scope: no raw API or replay is added; the existing conditional verbatim `WriteTo` copy, before `Body` is read and where the writer accepts the header, is held to the encoded and input budgets only; consumed, proof, read-ahead and drained raw bytes are not replayed (#41, not done here). |
| ROB-001-006 | Done | Add context cancellation and deadline checks to long scans/decoders: `ParseContext`, observed at reads, scope boundaries and metadata-scan work boundaries; see "ROB-001-006 — context cancellation" below. | Cancellation latency and partial-result tests; see below. Codex code review and final bounded QA passed: full tests/race/build, 15,813 cancellation cases, 19 contract checks, blocking/lifecycle/metadata probes, compatibility matrices and 696,287 fuzz executions. Issue #6 awaits remote inclusion of the local commit. |
| ROB-001-007 | Done | Detect repeated no-progress reads while allowing legal transient behavior: under `Parse`/`ParseContext`, 100 consecutive `(0, nil)` reads at the input end the parse (`input.no_progress`), at a leaf body's decoders end that body (`body.decoder_no_progress`), and at an encoded-word decoder of an interpreted parameter keep its raw value (`header.decoder_no_progress`); see "ROB-001-007 — no-progress reads" below. | Adversarial reader tests at 0/1/99/100/101 with call caps; see below. |
| ROB-001-008 | Done | Cap diagnostics under `Parse` with `MaxDiagnosticCount`, and emit one terminal `limit.diagnostic_count` entry in the last slot (`N-1` ordinary, request `N` replaced, later requests omitted without copy, retention or sink call) while parsing continues. | Diagnostic storm through public `Parse`: every malformed part healed and returned under the cap; `0`/default/`-1`/`1`/`N-1`/`N`/`N+1`/`math.MaxInt64`; retain/no-retain with sink none/ok/refusing; later input budget and sink stops after the cap; concurrent records, separate sessions and reentrant inspection. |
| ROB-001-009 | Done | Enforce `MaxInputBytes` under `Parse` once, at the raw source shared by every header, body, multipart and skip/drain read: `N` bytes are admitted, a proven byte `N+1` (at most one proof byte, never handed on) records one `limit.input_bytes` diagnostic and publishes the session stop; `Read`/`ReadWithOptions` unchanged. | `N-1/N/N+1` root header and body, nested and skipped parts, chunkings, `(n>0, EOF/error)`, delayed EOF, `(0, nil)`, empty reads, default/disabled/`math.MaxInt64`, sink-stop and header-budget precedence. |
| ROB-001-010 | Done | Enforce session-wide `MaxTotalHeaderBytes` (32 MiB) and `MaxTotalHeaderCount` (100,000) under `Parse`, shared by the root and every scanned child header, while preserving per-entity limits and legacy entry points. | Exact boundaries, wide/deep trees, count exhaustion preflight before allocating the next candidate, no-refund accounting for refused headers, no body/framing/read-ahead charges, session-stop identity, sink/cap precedence, provenance, chunk/error/default/disabled/MaxInt64/386 tests, and independent QA. Independent bounded QA, full tests/race, corrected scoped fuzzing and compatibility review passed; issue #5 awaits remote inclusion of the new local commit. |

#### ROB-001-006 — context cancellation (Done)

Status: Done locally for [issue #6](https://github.com/saeroun-ai/emlkit/issues/6).
Codex reviewed the actual changes and independently verified the final result;
the issue remains open until the reviewed commit is included remotely.

**API and compatibility.** `ParseContext(ctx, r, opts)` is `Parse` under a
context; `Parse` is `ParseContext` with `context.Background()`. A nil context,
or one whose `Done` returns nil, installs nothing, so `Parse`, `ParseOptions`
(no field was added), `Read`, `ReadWithOptions` and `New` keep their behavior.
The session retains the context for the lazy life of the parse. Options that
cannot be resolved are reported before the context is consulted.

**The stop.** Cancellation is the caller ending the parse: no diagnostic is
recorded for it, and its `FatalError` carries none. Its cause holds `ctx.Err()`
and, unless it is the same standard sentinel, `context.Cause(ctx)`; only the
standard sentinels are ever compared, so a cause of a type that is not
comparable is kept rather than compared. It is published through the existing
first-wins `publishStop`, so a sink or budget stop published earlier stays the
identity every scope reports. Context methods run with no session lock held.

**Where it is seen.** Without blocking, and only at these points: before and
after every read of the input and of every entity `Body` and verbatim raw body
(leaves and framed containers alike, gated by an `io.Reader`-only wrapper so no
`WriterTo`/`ReaderFrom` path reads around it); at every existing scope boundary
(`Parse`, entity construction, `NextPart`, `Walk`, `ParseStop` and so
`mail.Reader`, `WriteTo` entry); in `record`, before waiting for a delivery in
progress and again once that wait is over, before a slot is claimed; while a
parameterized field is interpreted (between items, with at most 4 KiB of the
field crossed between two looks, escaped pairs included, around the
whole-field parse, each decoded value and the last, and each repair item and
the last); and after a `CharsetReader` builds a decoder. A reading the
cancellation reached is discarded — empty media type, no parameters, the stop
as its error, nothing recorded, nothing cached — and a `CharsetReader` failure
after cancellation is not recorded as `charset.unknown`. Waits for a
part-count or header-total transition also wake on cancellation.

**What a read returns.** Before a read, once cancellation has been seen,
`(0, stop)` without touching the reader below, empty reads included. After a
read that returned n bytes, `(n, stop)` in place of whatever came with them,
`io.EOF` included. While the context is live everything passes through
unchanged. An `io.EOF` already returned is not revoked; completion tracking is
#10's. Decoder output at a cut is what the decoder makes of the bytes it was
given, and multipart framing cut inside a delimiter returns the bytes it could
not prove were delimiter — "one one one\r" with the '\n' still to come — as
body, exactly as `Parse` does for any input that fails there.

**NewMultipart.** The body's setup creates its writer with no lock held, since
interpreting the header can call a `CharsetReader`; Close never waits, may be
called from another goroutine or from that hook. The first terminal state
published wins: a setup failure published before any Close is kept; a Close
before the setup's outcome is published wins over it, success or failure, so no
pipe, worker or late setup failure is published. A Read waiting for the setup
is woken by Close at once; the Read inside a blocked hook returns when the hook
does. Close ends `Body` (`io.ErrClosedPipe`) and the iteration (`io.EOF`). The
writer still receives a struct copy of the header that shares its fields with
the entity's and the caller's header, so filling in a missing boundary reaches
them as it did before; that is not changed here.
A part from `ParseContext` whose parse was cancelled ends a generated body with
that stop.

**Limits.** Nothing is preempted: a source blocked in `Read`, a
`CharsetReader` or decoder, a sink call already running, a `WalkFunc`, an
`Unwrap`, the caller's `Writer`, a Context method, or one call of
`mime.ParseMediaType` runs until it returns. The input is never closed and its
deadline never changed; a caller makes a blocked read return by closing what it
owns, for example from its own `context.AfterFunc`. No goroutine or `AfterFunc`
watches the context; this was established by direct source review, not by a
test. A caller-replaced `Body` is not gated. A `NewMultipart` body abandoned
without Close keeps its worker, and Close cannot release a worker blocked on a
part's own source. `ROB-001-007` (#7), #10 and #41 are not advanced.

**Reviewed evidence.** Final verification passed all 14 sequential steps under
the bounded runner (1.5 GiB memory, no swap, two CPU cores, 128 tasks and a
runtime deadline). Full tests and race tests passed in all six packages;
build, filtered vet (`-composites=false -stdmethods=false`, the existing vet
exceptions), focused 386 tests, formatting and whitespace checks passed.
Independent QA passed a 15,813-case cancellation matrix, 19 contract checks,
whole-field and repair metadata checks, blocked source and caller-owned
net.Pipe-close checks, hook-initiated Close for successful and failed setup,
a waiter released before its blocked setup hook returned, and the queued
post-cancel diagnostic check. Existing header/input/body/diagnostic/healing
matrices passed, including 11 prior probes under a live cancellable context
and all 235 bounded error-classification probes. Header-total fuzzing completed
696,287 executions without a failure. The final service reported 1.3 GiB peak
memory. No cancellation watcher was added; source review and the finite
subprocess lifecycle checks cover the relevant worker cleanup paths.

Review found and corrected a lifecycle lock held across a charset hook,
post-cancel diagnostic delivery after a sink wait, an escaped-pair scan stride
larger than 4 KiB, a late setup error overriding Close, and a setup waiter not
released by Close. Tests now compare cut delimiter/decoder results against the
same wire prefix and an ordinary input error, and arm blocking hooks after
fixture construction. The first suite also observed the unchanged mail comment
allocation test at 5,592 bytes against 4,096 once; a focused rerun, both final
baseline/current count-three runs, and the final full/race suites passed. Its
cause is not established; the threshold was not relaxed. The executor did not
run tests; Codex ran and reviewed the checks above.

#### ROB-001-007 — no-progress reads (Done)

Status: Done locally for the ROB-001-007 portion of
[issue #7](https://github.com/saeroun-ai/emlkit/issues/7). Codex reviewed the
changes and independently verified the final result. ROB-010-006 remains
Planned, and the reviewed commit is not yet included remotely. The package
comment of `progress.go` defines the contract.

**The rule.** Under `Parse` and `ParseContext` — every profile, every `Limits`
value, `LimitDisabled` included, with no option to turn it off — 100
consecutive reads that asked for bytes and got exactly `(0, nil)` are a stall;
reads 1–99 are passed on unchanged, the reader is called exactly 100 times and
never again. A legal positive count resets the count, and its bytes are
delivered with any error that came with them. An empty request is answered
without calling the reader and counts nothing. An error — `io.EOF`, and a
reader's own `io.ErrNoProgress` — neither counts nor resets and is passed on as
that reader's own. The threshold is bufio's. No retry, sleep or goroutine is
added.

**Where.**

- *Input.* Directly over the caller's reader, below the cancellation gate, the
  `MaxInputBytes` limiter (so its proof reads are counted), the buffered readers,
  decoder loops (`base64`, `transform.Reader`) and `WriterTo`/`ReaderFrom`
  copies. A stall records one `input.no_progress` (`SeverityFatal`, nil path,
  offset of the next input byte, `Cause` `io.ErrNoProgress`) and publishes the
  session stop on the `MaxInputBytes` terms: a stop published first wins with
  nothing recorded, a sink refusing the diagnostic wins, `MaxDiagnosticCount`
  may drop the diagnostic but not the stop, cancellation seen first wins. The
  value is the session's `inputStop`, so a leaf body restores it after decoders
  that reword or mask it (quoted-printable, a hook turning it into `io.EOF`):
  as the identical value at the root, and under a part as the framing's
  existing `errors.Join(stop, ErrMissingBoundaryClose)`, which `errors.Is`
  matches to the stop; `ParseStop`, and `NextPart` and `Walk` after it, report
  the identical stop.
  A root header cut by a stall yields no entity. `errors.Is(stop,
  io.ErrNoProgress)` holds, as it did for the bufio error some of these paths
  returned before.
- *Leaf body decoders.* `bodyGuard` counts reads in which the decoders returned
  `(0, nil)` while neither the body's raw reader handed out a wire byte nor
  the charset stage took a byte of its own input (counted by a `Read`-only
  wrapper of that input, because a transfer decoder such as quoted-printable
  or base64 may already hold that input decoded, so the wire count alone does
  not move); input taken at either point, or output delivered, resets it, so
  a decoder that consumes input while emitting nothing is bounded by the body
  budgets, not by this rule. A stall
  reports the body's own local failure if it has one, else the input's failure
  as the body received it, else a local `body.decoder_no_progress`
  (`SeverityFatal`, leaf path and body offset) through the body's own slot,
  which a stop published earlier outranks. `ParseStop` stays nil; a parent
  multipart drains the raw tail and returns the following parts. A body that
  already failed locally keeps that failure.
- *Interpreted parameters.* Only the engine's own reading of `Content-Type` and
  `Content-Disposition` — whole field, a budget's retained prefix, the
  item-by-item repair, the lazy disposition — decodes encoded words through a
  per-field `metadataDecoder`. Each `CharsetReader` result is wrapped in a
  `Read`-only guard (no `WriterTo` shortcut) whose input is a `Read`-only
  counter of the encoded word it was given; a read that takes input resets the
  count. A stall is decided per parameter value, by the guard's own flag and
  never by an error value: the stalled word fails, `mime.WordDecoder` stops at
  it, and that value keeps its whole raw text, as for an unknown charset. Every
  other value of the field is decoded independently, so the result never
  depends on the order parameters are visited in, and a value that stalled is
  remembered for that one interpretation so the repair or retained-prefix
  reading does not hand it to the hook again. Any number of stalled values owe
  the field one `header.decoder_no_progress` (`SeverityInvalidMetadata`,
  field, header location and path, `Cause` `io.ErrNoProgress`) after its other
  diagnostics, recorded with no entity lock held as before. The cost is bounded
  by the existing budgets: the parameter budgets bound the values, the header
  budgets their length, and each hook reader gets at most 100 reads without
  progress. The media type, the boundary and the other parameters stay usable; the profile matters only where an
  independent repair already refuses under `ProfileStrict`. The guard also polls
  cancellation before and after each hook read; a cancelled reading is
  discarded, owes nothing and becomes the stop outside the lock, as in
  `ROB-001-006`. A hook's own failure or its own `io.ErrNoProgress` keeps the
  existing silent raw-value fallback.
- *Eager body hooks.* After a body `CharsetReader` returns, a session stop
  published while it ran — cancellation, the input stalling or overrunning
  `MaxInputBytes` under it — is returned with the entity instead of recording
  `charset.unknown`; a decoder it built is kept, and input it consumed is not
  refunded.

**Not changed or not covered.** `Read`, `ReadWithOptions`, `New`,
`NewMultipart`, `mail.CreateReader`, the `Header` accessors (`Text`,
`ContentType`, `ContentDisposition`) and `textproto` behave as before and can
still spin on an endless `(0, nil)` reader. A reader or hook that blocks, or
loops inside one call without returning, cannot be preempted, and a hook doing
hidden internal work behind more than 99 empty returns is stopped by the fixed
cap. A reader polling with `(0, nil)` meaning "try later" is treated as
stalled. A count below zero or above `len(p)` breaks `io.Reader`: it is passed
on, neither counted nor reset, and its existing bufio panic belongs to
`ROB-120-001`. The general preservation of bytes returned with errors across
decoders and textproto (including errors textproto's peeks discard) is
`ROB-010-006`, still Planned; this item only keeps `(n, err)` intact in the
wrappers it adds.

**Reviewed coverage.** `progress_test.go`, `progress_metadata_test.go`,
`mail/reader_progress_test.go` and `charset/progress_test.go`: the guard's
threshold, resets, error and empty-request pass-through; a 0/1/99/100/101
matrix over root header, plain/base64/quoted-printable bodies and multipart
framing, part headers and part bodies, chunks 1/7/4096, EOF with or apart from
the last bytes, three profiles, default and disabled limits, with chunk
invariance of stalled results; sink refusal, an earlier stop, the diagnostic
cap, sink-only delivery, cancellation during and after; the input proof
boundary; quoted-printable rewording at the root and under a part; a hook
looping on its input; eager hooks; `WriteTo` into a `ReaderFrom`; a
`NewMultipart` worker over a stalled part; decoder stalls at 0/1/99/100/101,
250- and 1000-byte consuming decoders, prefix-then-stall, swallowed and masked
input stops, a swallowed body-budget refusal, siblings through `NextPart`,
`Walk` and `mail.Reader`; a charset reader taking one byte per empty read of
input that quoted-printable or base64 already decoded, at chunks 1/7/4096 and
every profile, and the same reader stopping after exactly 100 empty reads once
it stops taking input; parameter stalls on the whole field, the retained
prefix, the repair, the lazy disposition read back by its sink and the charset
stage, every stalled value raw with one diagnostic per field, a stalled value
beside a decoding one and beside an encoded boundary (repeated, since
parameters are visited in map order), the hook reader's `WriteTo` unused,
foreign errors, cancellation inside the copy, and legacy `Read` unchanged.
Codex's independent QA found the quoted-printable false stall and the
order-dependent field-wide suppression; both were corrected and re-verified.
Every scripted reader has a call cap; no reader is endless and no guard is
disabled. The source matrix initially required a part body's framing join to
be identical to the session stop. The oracle was corrected to retain the
existing join, match its actual stop, and still verify exact stop identity at
later parse scopes, prefix bytes, and no further source calls.

Final bounded verification passed all 15 steps: full tests and full race tests
in six packages, build, all packages on 386, filtered vet, and the exact
baseline plain-vet warning audit (13 unkeyed composite literals and one
WriteTo-signature warning, unchanged). Independent QA passed 2,320 cases:
576 transient-zero combinations, 1,152 source-stall combinations, 165 decoder
and sibling cases, 160 whole/repair/lazy-metadata cases, 256 repeated parameter
and boundary salvage cases, and 11 stop/error contracts. Prior cancellation
QA (15,813 cut combinations plus its contracts and blocking/lifecycle probes),
live-context compatibility checks, input/body/header/diagnostic/healing
matrices and all 235 hostile-error probes passed. Scoped input fuzzing passed
1,037,084 executions and metadata-repair fuzzing passed 580,243 executions;
formatting and whitespace checks passed. All work ran sequentially under
RAM 1.5 GiB, swap 0, two CPU cores, 128 tasks and deadlines; the final service
reported 1.3 GiB peak memory. Claude ran no tests; Codex ran the checks, reviewed
their output and the actual changes. All three delegation results reported
empty permission-denial arrays.

### ROB-010 — Input and message framing

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-010-001 | Done | Treat empty input and header-only input deterministically: an empty input, an empty line alone, and complete CRLF fields ended by `io.EOF` or by the empty line are framed identically under every profile and compatibility as a usable entity with exactly those fields and an empty body or the bytes after the empty line, the framing adding no error or diagnostic of its own while the fields' MIME metadata is interpreted as usual; documented and pinned, no parser change. See "ROB-010-001 — empty and header-only input" below. | All profiles and round-trip tests; see below. |
| ROB-010-002 | Done | Handle CRLF, bare LF, bare CR, mixed endings, and a missing final newline in headers and multipart delimiters, preserving false-candidate body bytes and source errors within the budgets. Parse records original-byte locations; Strict refuses the affected entity or iterator locally. See "ROB-010-002 — line endings" below. | Independent chunk, source-error, budget, cancellation and profile matrices; bounded native fuzzing; full unit/race/386/build checks. |
| ROB-010-003 | Done | Detect and preserve BOMs without letting them corrupt a field name: a UTF-8 mark at the start of a header block is consumed, kept by the Header and written back; a UTF-16 or UTF-32 mark leaves the block unread and the entity opaque, every byte its body, never transcoded and refused by the writers; `input.bom` under Parse, Strict refuses locally. See "ROB-010-003 — byte order marks" below. | UTF-8/16/32 BOM fixtures; root/part/textproto/legacy/mail/writer, chunk, budget, source-error, cancellation and profile matrices; bounded fuzz oracles. |
| ROB-010-004 | Done | Optionally recognize a leading mbox envelope line and preserve it separately: `ParseOptions.RecognizeMboxEnvelope` (off by default) lets `Parse`/`ParseContext` keep one ctime-shaped `From_` line at offset 0 on the session (`ParseSession.MboxEnvelope`), the header read after it at moved positions, `header.mbox_envelope` Info under every profile; nothing else changes. See "ROB-010-004 — mbox envelope lines" below. | Valid/invalid/mid-header `From ` fixtures; grammar, line-ending, bound, look-ahead, root/profile/compatibility/delivery, suffix-equivalence, negative/option-off, BOM, part and root/shared budget, input budget, source-error, cancellation, no-progress, sink, immutability, writer and `mail` matrices; capped fuzz oracle. |
| ROB-010-005 | Done | Detect concatenated messages or trailing unassigned bytes in forensic mode: under `ProfileForensic`, after the message's own multipart closing delimiter, `ParseSession.TrailingInputReader` delivers the bytes after it verbatim and once when the caller reads, and `ParseSession.TrailingInput` reports their position, how reading ended, and message-like evidence (one `input.trailing_messages` Info suspicion, never a definite second message); a message that frames no parts has none, and no body is scanned or split. See "ROB-010-005 — input after the message" below. | Two-message and trailing-junk fixtures; line-ending, traversal, chunking, profile-parity, none/unknown, nested, mbox, input-budget, every-cut source-error, cancellation, no-progress, sink, concurrency and empty-read matrices; textproto epilogue tests; reference classifier and bounded fuzz oracles. |
| ROB-010-006 | Done | Preserve bytes returned with EOF and partial bytes before injected I/O errors: under `Parse` the input's first error is final and reported where the parse reaches it, a body reports it however its decoders ended, an eager `CharsetReader` failing on it returns it with the entity; every entry point's header scanner keeps a block's first read failure and the fields read whole before it. See "ROB-010-006 — input errors" below. | Custom-reader tests at every cut, chunk and buffer size, sticky and resuming sources; see below. |

#### ROB-010-006 — input errors (Done)

Status: Done for [issue #7](https://github.com/saeroun-ai/emlkit/issues/7),
taken first in `ROB-010` by the reviewed dependency note in §8. Codex reviewed
the implementation, requested corrections, and independently verified the
result under enforced host limits. Remote inclusion is tracked in the issue.

**An input error is not a stop.** An error the caller's reader returns is an
ordinary failure of the input, not a defect of the message: no diagnostic, no
`FatalError` made for it, `ParseStop` nil. A foreign `FatalError` or
`io.ErrNoProgress` the reader returns stays that ordinary value. The parser's
own stops (budgets, no-progress, cancellation, sink) keep their precedence.

**First error final, under `Parse`.** The innermost reader of a parse
(`progressReader`) delivers the bytes that came with an error and makes the
first error, `io.EOF` included, the answer to every later read, as the
identical value, without asking the reader again. `Read`, `ReadWithOptions`
and `New` do not.

**Header scanner, every entry point.** `textproto`'s header scanner keeps the
first error any read of a block returned (`headerScan.fail`): every `Peek`,
`ReadSlice` and the continuation look go through it. An internal full-buffer
result means read on; a source's own `bufio.ErrBufferFull` is handled as
described below. Once a read failed only bytes the `bufio.Reader` already
holds are read, with no refill. A failure other than `io.EOF` is returned as
the reader's own value with every field whose lines were read whole — a whole
field whose next byte could not be looked at included, a field whose
continuation line was cut kept with its earlier lines — and a line the failure
cut is dropped, with no terminator supplied. Bytes already read are charged
first, so a budget they exceed is what is reported. A failure still buffered
past the block's empty line was never observed by the block and is left for
the next read. `io.EOF` keeps its meaning (a header-only message; an
unterminated last line at EOF is still accepted, which is `ROB-010-002`'s to
revisit). Compatibility outside `Parse`: `ReadHeader`, `ReadHeaderLimit(s)`,
part headers and legacy `Read` now report a block's read failure as itself
instead of losing a one-shot failure, reading past it, or calling a cut line
malformed, and no longer return the cut line as a field. `bufio.Reader` forgets
an error once handed out, so a caller of the public functions that reads its
reader again after an error may reach the source again; that is the caller's to
avoid. A root header cut by a failure still yields no entity (`ROB-020`/#10).
A source may fail with `bufio.ErrBufferFull` itself, the value `ReadSlice`
also uses for a line longer than the buffer: the header and framing scanners
read on only when that value came with exactly the whole buffer, and otherwise
record it as the source's failure (the first implementation read on for ever
there; Codex's bounded QA caught it). The one indistinguishable case, a source
failing so just as it fills the buffer, reads that buffer on and is decided by
the next read: reported when the source keeps failing, as it does under
`Parse`, and read on when a source passed to the public functions resumes.

**Where a failure surfaces.** By `Parse` while the root header is read, with
no entity. Otherwise lazily: a header that ended before the failure gives an
entity and a nil error, and the first `Body` or part read needing input past
what was buffered delivers those bytes and then the failure; nothing returned
is withdrawn, and a failure after a tail nothing reads (an epilogue after the
closing delimiter) is never reported. A root body reports the identical value;
a part reports the framing's existing `errors.Join(err,
ErrMissingBoundaryClose)`; `NextPart` reports textproto's wrapping of it.

**Through decoders.** `rawBody.note` keeps the first error of the body's input
that is neither `io.EOF` nor the parse's own stop (`bodySlot.ioErr`, by
identity only). After the body's local failure and the parse's own input stop,
`bodyGuard.end` reports it however the decoders ended: as itself when they
passed it on (compared by `sameError`, a recovered `==`) or ended with
`io.EOF`, and otherwise in one sticky `errors.Join(ioErr, decoderErr)` — so
quoted-printable's wording of a cut soft break and a hook's own error are kept
beside it. A decoder corruption found before the failure reached the body is
reported alone, and a failure still in read-ahead is not pulled out; no chunk
invariance is claimed for malformed base64 (#34). Only what the decoders
produced is delivered. A decoder that swallows the failure and stops making
progress reports it, with no `body.decoder_no_progress`. A source error whose
type cannot be compared appears twice in the join; a decoder that wrapped it
does too. No `Is`, `As`, `Unwrap`, `Error` or `Format` of a source error is
called.

**Eager `CharsetReader`.** A hook that fails while the body's input failed
under it records no `charset.unknown`: `Parse` or `NextPart` returns the usable
entity with the input's error, or a join with the hook's own (the hook's error
is taken only from this package's own `UnknownCharsetError`). That value is
whatever the source returned, so the public predicates answer about what it
holds, a genuine warning or `FatalError` replayed by the source included. A hook that
failed because the body's own budget refused the read returns the entity with
that local `FatalError`, scoped like a Strict refusal: `NextPart` continues
with the next part, `mail.Reader.NextPart` returns no part and the next call
continues, `Walk` ends. A hook that built a decoder anyway gives `(entity,
nil)` and the body reports the failure. What a hook consumed is not replayed
(#41).

**Not changed.** `lenientQPReader` and the standard base64 newline filter
drop or re-read after an error; under `Parse` the final input error and the
body's provenance make that harmless, and outside `Parse` it is unchanged.
Completion state (#10), raw replay (#41), header healing (`ROB-020`) and
illegal read counts (`ROB-120-001`) are separate. A genuine warning value a
source hands back is indistinguishable from one the parser made (the
`ROB-120-007` boundary).

**Tests verified.** `textproto/header_ioerror_test.go` (every cut of a block
with a long line and a continuation, buffers 16/4096, chunks 1/7/4096, error
with or after the data, sticky and resuming sources, `ReadHeader` and
`ReadHeaderLimits`: whole-line prefix byte for byte, the identical error, no
read after it, lazy body error; named look-ahead points; budget first; part
headers), `io_error_test.go` (a root header/body/multipart matrix over
profiles and default/disabled limits; lazy boundary; legacy `Read`; decoders
passing, masking, swallowing, adding their own error; quoted-printable at the
root and in a part; unreached corruption; a non-comparable error; eager hooks
at the root and in a part, input error and local refusal; `WriteTo` into
`ReaderFrom` and plain writers; own stops first; foreign `FatalError` and
`io.ErrNoProgress`; hostile errors in a subprocess; header round trip),
`mail/reader_ioerror_test.go`, and the updated plain-input-error case of
`TestParse_bodyInputStopNotMasked`.

**Codex review and independent QA.** The initial implementation hung when a
source returned `bufio.ErrBufferFull`: the sticky source error was mistaken for
an internal full-buffer result. A separate three-second probe caught it; the
same finite probe on `92eaec1` terminated (but lost the error). The correction
qualifies full buffers in both header and multipart framing, including pending
CR accounting. Small, exactly 4096-byte, and multi-buffer cut lines now end.
Two failing test expectations were corrected to the reviewed contract: a
source error is final rather than counted toward a later no-progress stop,
and the sibling in a local-budget test must itself fit that budget.

The final fifteen-step bounded run passed: uncached tests and race checks in
all six packages, build, filtered vet (`-composites=false -stdmethods=false`;
the existing unkeyed-composite and WriteTo-signature warnings are not claimed
fixed), all six packages on GOARCH=386, formatting and whitespace. Independent
input QA covered 3,840 Parse combinations with zero source rereads, zero
sticky/resuming result differences and zero lost injected errors, 150 header
checks, 605 decoder checks and 22 hostile/error-collision subprocess probes.
The 7,680 legacy/public-buffer observations retain their documented differences;
this does not claim to make legacy body readers sticky. The prior 2,320
no-progress checks, 15,813 cancellation combinations plus contract/lifecycle
checks, live-context regressions, header/input/body/diagnostic/healing matrices
and 235 error-predicate checks passed. Thirty-second fuzz runs completed
937,615 input-budget and 501,990 total-header executions without failure.

Evidence: `/tmp/emlkit-ioerror-verification.json` and `.out`, the per-step logs,
`/tmp/emlkit-ioerror-initial-snapshot.json`, and
`/tmp/emlkit-ioerror-bufferfull-baseline.out`. The service
`emlkit-bounded-27e92ceb9e` enforced 1,536 MiB RAM, no swap, two CPU cores,
128 tasks and a deadline; observed peak memory was 1,433,858,048 bytes. Jobs
ran sequentially, with finite sources and short subprocess deadlines for hangs.
Every Claude invocation for this ID returned an empty permission-denial array.

#### ROB-010-001 — empty and header-only input (Done)

Status: Done for [issue #19](https://github.com/saeroun-ai/emlkit/issues/19),
after `ROB-010-006`. The audit found the behavior already deterministic, so
this item documents and pins it; no parser or writer code changed. Claude ran
no tests; Codex reviewed the actual diff and completed the bounded QA below.

**Contract.** Under `Parse`, `ParseContext`, `Read`/`ReadWithOptions`,
`textproto.ReadHeader(Limits)` and `mail.Reader` alike:

- An empty input, an input that is only the empty line, and complete
  CRLF-terminated fields (folded ones included) ended by `io.EOF` give a usable
  entity — `textproto`: a header — with exactly those fields, raw bytes and
  order as read. A block `io.EOF` ended is the same block with the empty line
  appended; the body is empty. The framing adds no error and no diagnostic of
  its own: for the fixtures here, which declare nothing to interpret, that is a
  nil error and an empty session. What fields do declare is interpreted as for
  any message — an unknown transfer encoding or charset is still a warning with
  its diagnostic, a multipart whose repair kept no usable boundary is still an
  opaque leaf (`ROB-050-001`), `ProfileStrict` still refuses such a field — and
  budgets and cancellation keep their rules.
- One empty line ends the block; every byte after it, blank lines included,
  is body (`"\r\n\r\n"` has body `"\r\n"`). The body begins right after the
  block, at the input's length when `io.EOF` ended it.
- Nothing requires a field. No profile applies RFC 5322's minimum field
  counts; `ProfileStrict` does not refuse a message without `From`, `Date` or
  `MIME-Version`.
- A block only `io.EOF` could end is still being read at that `io.EOF`: an
  input error, a cancellation or a no-progress stop arriving there instead is
  reported by `Parse` with no entity, the identical value, as for any header
  (`ROB-010-006`, `ROB-001-006`, `ROB-001-007`). After an empty line it is
  usually reported by the body after every byte before it; a cancellation seen
  by the read that delivered the header's last bytes is instead returned by
  `ParseContext` with the entity, whose body then delivers a prefix.
- `MaxHeaderBytes` counts a block `io.EOF` ended as its fields alone and an
  empty line as part of the block; `MaxHeaderCount` charges the empty line
  nothing; `MaxInputBytes` of exactly the input's length reads it whole.
- A multipart with a usable boundary (the fixtures: `boundary=B`) whose body
  is empty has no part: the first `NextPart` returns an error wrapping `io.EOF`
  (`"multipart: NextPart: EOF"`), later ones `io.EOF`; `Walk` ends with that
  error after the root; `mail.Reader.NextPart` returns it with no part, then
  `io.EOF`. Pinned as is; the empty-container policy is `ROB-060-009`'s.
- A message that is not multipart is its own one part to `mail.Reader`,
  classified as usual: inline `text/plain` with no Content-Type or
  Content-Disposition, an attachment for a non-text type or an `attachment`
  disposition.

**Serialization is not replay.** `Entity.WriteTo` adds `Mime-Version: 1.0` at
the top of a header without a MIME-Version, always writes the empty line, and
otherwise keeps its existing writer rules (`CreateWriter` may set a multipart
boundary or drop a multipart's `Content-Transfer-Encoding`, and refuses what it
cannot interpret). For the fixtures here — ordinary unmodified fields — the
fields are written raw and in order: `""` and `"\r\n"` become
`"Mime-Version: 1.0\r\n\r\n"`, and fields `io.EOF` ended gain that line;
parsing that output gives the same body and the written fields, and writing it
again gives the same bytes. `WriteHeader` likewise always writes the empty
line. Reconstructing the input's own bytes is #41's; `WriteTo` keeps the
one-shot body contract.

**Not changed, not claimed.** Bare CR, bare LF, mixed endings and an
unterminated last line (`ROB-010-002`); a malformed, colonless or
whitespace-only first line and partial root-header healing (`ROB-020`);
completion state (#10); raw replay (#41); the overall profile and compatibility
policy (#17). Fixtures here use CRLF only.

**Tests verified by Codex.** `framing_empty_test.go`: ten literal
fixtures over every profile and compatibility, default and disabled limits,
reads of 1/7/4096 bytes, `io.EOF` with or after the data, every two-read split;
fields, raw bytes, order, body, body offset, empty session, multipart and
`Walk` outcome, each from a fresh parse; `WriteTo` of a fresh entity,
reparse and second-write stability; legacy `Read`/`ReadWithOptions`; injected
I/O error at the end, cancellation before and at the `io.EOF` read, 99 and
100 empty reads at the end, exact and one-over budgets; and header-only
fields whose interpretation is unchanged — unknown encoding and charset
warnings, a conflicting-boundary multipart repaired to an opaque leaf or
refused under Strict — with and without the empty line.
`textproto/header_empty_test.go`: `ReadHeader` and exact `ReadHeaderLimits`
over the same reads and splits, bytes left in the reader, `WriteHeader` and
reread stability. `mail/reader_empty_test.go`: `CreateReader` and
`NewReader` over `Parse` under every profile, and the usual inline/attachment
classification of header-only parts.

**Codex review and verification.** Review narrowed the initial documentation's
blanket success and byte-replay claims: MIME warnings/refusals, cancellation
seen while the header is read, unusable-boundary fallback, attachment
classification and writer transformations retain their existing meanings. The
new tests cover those exceptions without changing product code.

Final bounded verification passed all nine steps: uncached tests and race in
all six packages, build, filtered vet (the documented composites and stdmethods
warnings remain), GOARCH=386, formatting and whitespace. Independent QA passed
1,872 Parse/ParseContext combinations, 3,336 header splits and 78 legacy
combinations (24,399 assertions), plus 265 metadata/classification/writer
exception checks. Evidence: `/tmp/emlkit-empty-verification.json` and `.out`.
All work used one bounded service at a time, 1,536 MiB RAM, no swap, two CPU
cores, 128 tasks and a deadline; final service `emlkit-bounded-6800a930a6`.
Both Claude calls returned empty permission-denial arrays.

A preceding full run failed the existing `TestHeader_commentScanAllocationIsConstant`
once (5,592 bytes against 4,096). The same unchanged test also failed on the
older `92eaec1` baseline (5,736 bytes, isolated 30-run check); current isolated
30 and current/baseline full-mail 20-run checks passed. This is a confirmed
pre-existing intermittent allocation-measurement failure, not evidence of a
product regression from these test/comment-only changes. The failed run is
preserved in `/tmp/emlkit-empty-final-failure.out`, comparison evidence in
`/tmp/emlkit-empty-allocation-investigation.json` and `.out`, and stabilization
is tracked separately in [issue #59](https://github.com/saeroun-ai/emlkit/issues/59).
The passing final run does not claim that intermittent failure has been fixed.

#### ROB-010-002 — line endings (Done)

Status: implemented, independently reviewed and verified by Codex for
[issue #19](https://github.com/saeroun-ai/emlkit/issues/19). This covers header
blocks, multipart delimiter framing, and their Parse diagnostics and Strict
policy. BOM handling and unassigned/trailing input remain separate open items;
this does not complete issue #19.

**Rule, every entry point.** A header line ends at `"\r\n"` (one ending even
across reads), at a bare `'\n'`, or at a bare `'\r'`. A bare `'\r'` ends the
line when it begins it (the empty line ending the block), comes right before
`io.EOF`, or is followed by white space (a fold) or another `'\r'` (so
`"A: 1\r\r\n"` is field A, then the empty line `"\r\n"` — never one three-byte
ending). Followed by a byte that can begin a field name, it ends the line only
if the next line begins `1*name-byte *WSP ":"`; otherwise it and that line are
content of the line it was in, so a stray `'\r'` in a value (`"Subject: a\rb"`)
keeps its long-standing value. Any other bare `'\r'` is content. This is the
same for `Read`, `ReadWithOptions`, `textproto.ReadHeader(Limits)` and part
headers; only `Parse` reports and refuses.

**Look-ahead and input errors.** Exactly one byte after a `'\r'` is looked at
before it is consumed (`Peek` of at most the buffer, so a source's own
`bufio.ErrBufferFull` is always its failure and never read on). A `'\r'` whose
next byte a failure other than `io.EOF` prevented reading, or that the next line
was still settling, is a byte of the line the failure cut: it is charged as
content and dropped with that line, and the failure is returned as itself
(`ROB-010-006`). A wrapped `io.EOF` is such a failure. A header whose last
ending is a bare `'\r'` is still being read until one more byte or `io.EOF`:
`MaxInputBytes` of exactly its length with a body after it leaves no entity,
and legacy `ReadOptions.MaxHeaderBytes` of exactly its length refuses it.

**Budgets.** Charged on the input's own bytes before they are kept; a line
ending is not line content but is field and block bytes; nothing is refunded.
A next line that a `'\r'` is still settling is charged as a line and field of
its own until it settles; if it continues the field, the line (previous line,
the `'\r'` as content, and it) and the field are charged whole at once and
refused where they began if that exceeds `MaxLineBytes` or `MaxFieldBytes`. Such
a line is not charged to any field count; an exhausted shared count refuses it
at its first byte, like any candidate. The joined field is built once.

**Report.** `textproto.Header.LineEndings` (`LineEnding`, `LineEndings`) reports
per kind the count and first offset, and an unterminated last line's range,
relative to the block. Known only for a header read whole and its `Copy`;
`Add`, `AddRaw`, `Set`, `Del` and `HeaderFields.Del` make it unknown; built
headers have none; invalid kinds report nothing; counts saturate.

**Parse policy** (`line_endings.go`): `header.bare_lf` Info, `header.bare_cr`
Recovered, `header.mixed_line_endings` Info (Recovered with a CR) once per
parse; `header.truncated_line` Incomplete per block; ordered by offset; locked
claims, no lock held while recording. `ProfileStrict` keeps accepting LF and
CRLF+LF, and refuses an entity with a bare-CR ending or no final ending through
the existing Content-Type refusal: entity plus local `FatalError` (recorded
`SeverityInvalidMetadata` per refused entity regardless of the once-per-parse
notes, carried even when `MaxDiagnosticCount` omits it), Content-Type not
interpreted, raw leaf body, siblings continue, no session stop. Sink stops and
cancellation keep their precedence. Forensic records what Tolerant records; no
replay is claimed: LF, CR and missing endings are stored as CRLF, and only
complete CRLF fields stay byte-exact.

**Multipart framing (textproto).** The multipart
reader frames delimiter lines on the same endings with no first-line mode: a
delimiter is the line ending before it (`"\r\n"` matched whole, else a bare
`'\n'` or `'\r'`; none at the start of a body), `"--"`, the boundary, `"--"`
for the closing one, spaces and tabs, and its own ending — or the end of input
for a closing one. Candidates that do not fit are body, every byte (the ending
before them too); `"--B--x"` is such a candidate. Because the line ending after
a nested multipart's inner closing delimiter is the outer delimiter's, the
inner body ends only where the outer part ends; under `ParseContext` a framed
container's own framing therefore reads its body behind no cancellation gate of
its own (`Entity.framing`), reaching the input only through the input's gate,
so a cancellation that arrives while the outer delimiter is decided cuts the
inner body exactly where an ordinary failure at the same point would. Every
caller-visible `Body`, the raw `WriteTo` body and `NextPart` stay gated. After
a cancellation that framing reads nothing new, but may still work, inside a read
a caller already started, through what every enclosing container's reader
holds: its 4 KiB buffer, a held or released candidate up to `MaxLineBytes`
plus one buffer (one buffer with that budget disabled), and its framing-line
accumulator to the same bound, for up to `MaxMIMEDepth` containers — not a
4 KiB-per-container bound. A `Body` the caller replaced is not framed through
`Entity.framing`: `MultipartReader` frames it through the parse's cancellation
gate (the field itself stays the caller's ungated reader, with no input budget
and no origin), so neither it nor the framing of any multipart inside it reads
the caller's reader after a cancellation. A candidate whose white space runs past the buffer is held
outside it, bounded by `MaxLineBytes` on the line content (endings excluded);
past it the body ends before the candidate and `NextPart` returns the reader's
own `*LineLimitError`; with no budget a delimiter line must fit one buffer,
its own ending included, as before, else the reader's own error wrapping
`bufio.ErrBufferFull` (at the `Parse` layer an ordinary framing error, not a
diagnosed budget: no new fatal API for it). The candidate's valid prefix —
`"--"`, the boundary, `"--"` and white space, up to the byte that decides it —
is charged before that byte is acted on, whether it then fails, waits for more,
or the input failed, so the decision does not depend on how the bytes arrive: a
prefix of exactly the budget followed by an invalid byte is body, one byte more
is refused at the line's `"--"` even when the invalid byte is already buffered,
and a prefix past the budget is refused ahead of an input failure after it. The
invalid byte is not counted, and opaque body lines and candidates that stay
within the budget are never line-limited (found by
`FuzzMultipartReader_lineEndings`; the input is kept in
`textproto/testdata/fuzz` with a literal regression beside it). A failure other than `io.EOF`
while a candidate is undecided makes it body, followed by the failure;
`io.EOF` decides it. An empty-header part needs a whole delimiter line.
Preamble lines keep their line budget and now end at a bare `'\r'` too.
`MultipartReader.DelimiterEndings` reports the endings of proven delimiters;
the near-miss diagnostics and suffix policies of `ROB-060-007` are not done
here.

**Multipart policy (Parse).** Registry
adds `multipart.bare_cr` and `multipart.bare_lf`; `multipart.mixed_line_endings`
is used only for an actual mix. The report is cumulative and is updated by
`Part.Read` as soon as a body reaches its delimiter, which can be before
`NextPart` is called, so each root iterator keeps the endings it has already
looked at and inspects what was added after every low-level `NextPart`
(including the `io.EOF` of a close), unless the session has stopped:
`multipart.bare_lf` Info, `multipart.bare_cr` Recovered, mixed Info or
Recovered with a CR, once per parse each, at the first such ending's input
offset (container body offset added) and the container's path; none for a
caller-replaced body. `ProfileStrict` accepts LF and LF+CRLF; on a framed part
or `io.EOF` with a newly observed bare CR it records `multipart.bare_cr` of
`SeverityInvalidMetadata` for that iterator (whatever the once-per-parse
notes did) and returns a sticky iterator-scoped `FatalError` carrying a
`SeverityFatal` copy, even past `MaxDiagnosticCount`; the part that delimiter
introduced is not returned, earlier parts stay valid, an enclosing multipart
continues, `ParseStop` stays nil. A session stop (sink, budget, cancellation,
or the shared header total refused on that call, which is not inspected)
outranks it. A low-level failure of the call that met the bare CR — an input
error, a malformed part header — is returned by that call as itself, with the
refusal recorded there and kept as the iterator's sticky failure, so every later
call returns the refusal instead of the part the low-level reader would resume
to (Tolerant and Forensic resume). An own part-header or framing-line budget
refusal on that call is returned and replaces it as the sticky failure; the
`multipart.bare_cr` diagnostic is still recorded before it. Body reads alone
never record: the diagnostic or refusal is observed at the next `NextPart`.
`mail.Reader.NextPart` returns the refusal with no part, again on every later
call, and so reaches no later part of that multipart or of one enclosing it.

**Not done here:** CR-aware transfer-encoding line counting and
quoted-printable CR soft breaks (`ROB-070`), raw line-ending replay
(`ROB-020-007`, #41), BOMs (`ROB-010-003`), `ROB-060-007`'s near-miss
diagnostics.

**Tests added or updated.** `textproto/line_endings_test.go`;
`framing_lineend_test.go`; expectations updated, never filtered, in
`textproto/header_budget_test.go`, `textproto/header_limits_test.go`,
`limit_header_total_test.go`, `limit_header_fields_test.go`,
`limit_input_test.go`, `limit_part_count_test.go`, `limit_mime_depth_test.go` and
`limit_line_bytes_test.go` (LF variants now assert the root's one
`header.bare_lf` note; a leading `"\r"` is now the empty line).

**Phase 1 QA (Codex, bounded, one job at a time).** Unit, race, GOARCH=386,
build and filtered vet passed in all six packages, with formatting and diff
checks clean. Independent probes passed 6,962 framing/profile/compatibility/chunk
cases (29,290 assertions), 130 Strict-sibling/mail/serialization assertions,
1,139 input-error/`bufio.ErrBufferFull`/wrapped-`io.EOF`/read-timing/budget/
buffer-edge cases (3,234 assertions), and the earlier empty-input QA (24,399
assertions, 265 exceptions); a finite one-byte-source scan-cost probe from 2 KiB to
64 KiB showed approximately linear scaling. Evidence: `/tmp/emlkit-lineend-header-verification.json`,
`/tmp/emlkit-lineend-header-platform.json`,
`/tmp/emlkit-lineend-error-budget-qa.out`,
`/tmp/emlkit-lineend-scan-cost-before.out`. Those earlier runs covered the header phase. The final combined checks below
include all later code and regression additions.

**Final Codex review and verification (2026-11-10, Linux amd64, Go 1.26.10).**
The full six-package unit, race and GOARCH=386 suites, build, formatting,
whitespace and vet excluding the documented baseline `composites` and
`stdmethods` categories passed. Plain vet still reports the same 13 unkeyed
composite-literal warnings and one existing WriteTo-signature warning
(`/tmp/emlkit-lineend-final-plain-vet.out`); it is not a clean plain-vet run.
The fresh independent matrices passed: 144
candidate-prefix budget cases; 432 Strict retry/mail/profile/cap/sink cases
(3,536 assertions); 1,299 multipart-policy cases (7,226 assertions); 531
replacement-body cancellation cases; 5,879 multipart-byte/framing cases
(87,623 assertions); 840 long-boundary/buffer-edge cases; 514 multipart
source-error/budget cases (3,410 assertions); 6,962 header cases (29,290
assertions); 1,139 header-error/budget cases (3,234 assertions); and 130
Strict-header/serialization assertions. The prior empty-input matrix passed
24,399 assertions and its exception probe passed 265 checks. Evidence is in
`/tmp/emlkit-lineend-final-verification.json` and
`/tmp/emlkit-lineend-final-platform.json`, with their `.out` logs.

Both finite-input fuzz targets passed fresh 30-second runs with two workers:
`FuzzMultipartReader_lineEndings` executed 698,156 inputs and
`FuzzReadHeader_lineEndings` 927,306. The earlier multipart fuzz failure is
preserved in `textproto/testdata/fuzz/FuzzMultipartReader_lineEndings/8c17b33a75df9e9d`
and a literal regression. The correction charges the valid candidate prefix
consistently before a later invalid byte, including one-buffer legacy limits;
opaque body lines remain outside framing-line limits. Review also corrected a
Strict refusal lost after a malformed-header error, the cancellation gate for
caller-replaced container bodies, and cancellation at nested closing
delimiters. A premature-close shortcut was rejected because `--B--x` lost body
bytes; the final full delimiter grammar passes the independent byte oracle.
Existing budget tests now require diagnostic presence and exact fixture-derived
offsets instead of conditionally accepting whatever was recorded.

All execution used one bounded service at a time (RAM 1,536 MiB, no swap,
two-core CPU quota, 128 tasks and a deadline). Final matrix/fuzz/platform
services peaked at 451.8 MiB / 470 MiB / about 1.2 GiB, respectively, with
successful exits. Claude made code/test changes and ran no Go verification;
Codex inspected the changes and ran the checks. The final correction result
reported no permission denials. An early header implementation hit its runtime
deadline; edits were retained and independently checked before continuation.
No Windows or physical-device validation, raw non-CRLF replay, or differential
parity with another MIME engine is claimed.

#### ROB-010-003 — byte order marks (Done)

Status: implemented by Claude and independently reviewed and verified by Codex
for [issue #19](https://github.com/saeroun-ai/emlkit/issues/19). Claude ran no Go
build, test or fuzz job. This
covers byte order marks only. At that completion, `ROB-010-005`
(concatenated messages and trailing bytes) still remained under #19, and
`ROB-010-004` (mbox envelope lines) belonged to #46; their later completion
is recorded separately below.

**Recognition, every entry point.** Only the first bytes of a header block —
the input's first block, and each part's block right after its delimiter
line — are looked at, through the scanner's sticky `peek`: a bounded prefix
look-ahead of at most four bytes, a further byte only while the bytes held
still begin a mark, so a partial or near-miss prefix (`EF BB`, `FF FE 00`
at a non-`io.EOF` failure, `00 00 FE`) is still looked at that far before it
is known to have none. This decision comes first in the block, before the
fields' own checks: a spent shared header field count (`HeaderBudget`), which
without a mark refuses the first candidate at its first byte, refuses only
after the up to four bytes were looked at, and those may be read from the
source before that refusal; and, as with any `bufio.Reader` peek, looking may
fill the reader's buffer from the source. The order in which a block's budgets
are checked against each other is otherwise unchanged. Longest match: `EF BB BF` UTF-8, `FE FF` UTF-16BE,
`FF FE` UTF-16LE, `00 00 FE FF` UTF-32BE, `FF FE 00 00` UTF-32LE. `io.EOF`
decides on the bytes before it (`FF FE` and `FF FE 00` are UTF-16LE). A
failure other than `io.EOF` before the mark is decided leaves no mark; the
bytes are then the first line that failure cut and the failure is returned as
itself (`ROB-010-006`), including a source's own `bufio.ErrBufferFull`. A
failure still buffered past a decided mark is what the next read returns.
Only one mark is recognized: a second, partial, near-miss or later one is the
beginning of a line read as before (a malformed key or line, `ROB-020`), and
U+FEFF in a value or body is content.

**UTF-8.** The three bytes are consumed and charged as bytes of the block —
`MaxBytes`/`MaxHeaderBytes` and the shared total bytes — but as no line's and
no field's content and no field candidate. Offsets, `LineEndings` and refusal
positions count them: the first field starts at 3, a block budget below the
mark refuses the block at its start, a spent shared count refuses the first
field at 3. `textproto.Header.ByteOrderMark` reports `BOMUTF8`; a block
refused by its own budget is the empty Header (no mark, all or nothing), and
an ordinary failure keeps the mark beside the fields read whole. Raw fields
never include the mark. `Copy` and every field mutation keep it (they drop
`LineEndings` as before); there is no setter — `HeaderFromMap(h.Map())`
rebuilds a header without it, losing order, case and raw bytes. `WriteHeader`
writes it once, first, on its usual terms: only a well-formed complete block —
every field read as a field, kept and unchanged, every line, the empty line
ending it included, ended with CRLF — round-trips byte for byte; bare LF and CR
endings are written as CRLF, a mark alone, an unterminated last line or a block
with no empty line gains the CRLF endings and the empty line, as without a
mark, and a line the reader skipped rather than kept, such as one with an
empty name (existing behavior), is not written. Keeping the mark is not raw
replay; replaying the whole input is `ROB-020-007`/`ROB-110`'s.
`Entity.WriteTo` writes the mark before the `Mime-Version` it may add.

**UTF-16 and UTF-32.** In text in these encodings the ASCII characters that
frame a header — field-name letters, the colon, CR and LF — are two or four
bytes wide with zero bytes among them, so stripping the mark would not leave a
header this package can read; no implicit transcoding is implemented, and no
attempt is made to tell ASCII text after such a mark from text in its encoding.
Nothing is consumed or charged, `ReadHeader` returns a Header with no fields,
the mark reported and unknown `LineEndings`, and a nil error; the mark and
everything after it are what the reader yields next, so a root's body starts
at 0 and a part's at its header offset. Every entry point (`Parse`, `Read`,
`ReadWithOptions`, `New`) builds an opaque entity from such a header, whatever
fields a copy of it was given: no media type (not text/plain),
`EffectiveContentType` reporting a private non-fatal error, no transfer or
charset decoder, `MultipartReader` nil, the body a leaf under the body budgets.
Content-Disposition is not part of that and is interpreted as usual when asked:
a parsed such header has no fields and so declares none, and `mail.Reader`
classifies the part as an attachment; a caller who gives a copy
`Content-Disposition: inline` and builds an entity with `New` gets an inline
part, its body still undecoded. Nothing is transcoded. Writers
refuse such a header before any output — `textproto.WriteHeader`,
`MultipartWriter.CreatePart`, `CreateWriter`, `Writer.CreatePart`, hence
`Entity.WriteTo` and a `NewMultipart` body, where the parts before it are
already written — and the entity's Body stays unread and usable.

**Parse policy** (`bom.go`, through the line-ending notes of
`line_endings.go`): one `input.bom` per marked block at the mark's bytes
(offset 0 of the block; length 3, 2 or 4), before the block's line-ending
notes. Tolerant and Forensic: Recovered for UTF-8, Unsupported for UTF-16/32,
nil parse error. Strict: `SeverityInvalidMetadata` and the entity-scoped
refusal of `ROB-010-002` — entity returned with a `FatalError` carrying a
`SeverityFatal` copy even past `MaxDiagnosticCount`, Content-Type not
interpreted, raw body, siblings continue, no session stop; for UTF-8 the
fields are kept. Sink stops, cancellation, no-progress and budget stops keep
their precedence. Every `Compatibility` value behaves the same.

**Compatibility implications.** Inputs that failed before now succeed: a
UTF-8-marked header used to fail with a malformed key under every entry point
(no entity under `Parse`), and a UTF-16/32-marked input now returns an opaque
entity, also from `textproto.ReadHeader` and the legacy `Read`, instead of an
error. Callers that must not treat it as a message check
`Header.ByteOrderMark`, or the `input.bom` diagnostic under Parse. The legacy
`ReadOptions.MaxHeaderBytes` shortens the reads under the header, so a budget
smaller than the up to four bytes the decision looks at fails as a header
error there. A block whose first byte cannot begin a mark (anything but
`EF`, `FE`, `FF` or `00`) is looked at no further than before and framed as
before; a block that only begins like a mark is framed as before, after the
look-ahead described above.

**Not done here:** transcoding UTF-16/32 messages; body charset and mark
conflicts (`ROB-080-004`); raw replay of the input (`ROB-020-007`,
`ROB-110`, #41); general malformed-key repair, including repeated marks
(`ROB-020`); mbox envelope lines (`ROB-010-004`, #46) and concatenated
messages or trailing bytes (`ROB-010-005`, #19).

**Tests added or updated (not run by Claude).** `textproto/bom_test.go`
(case table over every delivery and buffer size through `ReadHeader` and
`ReadHeaderLimits`; values; UTF-8 write, `Copy` and mutations; wide-header
write and `MultipartWriter` refusals; budgets; read-failure matrix at every
cut; `io.EOF` decisions; `bufio.ErrBufferFull` in a subprocess with a
deadline; multipart part marks with and without tight budgets);
`framing_bom_test.go` (root over every profile, compatibility and delivery;
`Walk`; `Read`/`ReadWithOptions`; opaque copies through `New`; malformed
marks unchanged; parts; Strict diagnostic budget; sink stop; budgets;
input errors; cancellation; no progress; writers and `NewMultipart`);
`mail/reader_bom_test.go`; the `ByteOrderMark` added to the outcome of
`FuzzReadHeader_lineEndings` and `FuzzMultipartReader_lineEndings`, with
marked seeds.

**Final Codex review and verification (Linux amd64, Go 1.26.10).**
All 15 independent/unit verification steps passed, including 60 unchanged
baseline controls, 1,095 cancellation/diagnostic cases (4,131 assertions),
1,260 BOM/profile/compatibility/entry-point/writer cases (14,742 assertions),
and 1,934 source-error and budget cases (6,564 assertions), plus every prior
line-ending matrix and the full six-package unit suite. Eight platform checks
passed: full race, GOARCH=386, build, vet with the two documented baseline
categories excluded, formatting, whitespace and both prior empty-input probes
(24,399 assertions and 265 exception checks). Plain vet retains the same 13
unkeyed-composite warnings and one WriteTo-signature warning; it is not clean.
Fresh bounded 30-second fuzz runs with two workers passed 618,090 multipart
and 945,836 header executions; the outcome oracles now include the BOM.
Evidence: `/tmp/emlkit-bom-initial-verification.json`,
`/tmp/emlkit-bom-final-platform.json`, `/tmp/emlkit-bom-final-fuzz.json` and
their `.out` logs; plain vet in `/tmp/emlkit-bom-final-plain-vet.out`.

Review corrected documentation to distinguish bounded marker look-ahead from
field-count preflight, ASCII framing code units from arbitrary Unicode text,
caller-supplied Content-Disposition from an unchanged parsed wide header, and
well-formed complete CRLF header round trips from raw replay. Independent
finite probes confirmed the last two distinctions. Final corrections were
prose/comments only; formatting and whitespace were rechecked after them.
All four Claude calls reported no permission denials. Every execution ran
alone in the enforced RAM 1,536 MiB / swap 0 / CPU two cores / tasks 128 /
deadline service. Matrix/fuzz/platform peak memory was approximately
454.2 MiB / 581.9 MiB / 1.2 GiB, with normal exits. No host OOM, Windows
validation, cross-engine parity, UTF-16/32 transcoding or general raw replay
is claimed by these checks.

#### ROB-010-004 — mbox envelope lines (Done)

Status: implemented by Claude for
[issue #46](https://github.com/saeroun-ai/emlkit/issues/46) from a design
Codex reviewed, then independently reviewed and verified by Codex. Claude
ran no Go build, test, fuzz or benchmark; only `gofmt` was run. Primary
sources: RFC 4155 (`/tmp/emlkit-rfc4155.txt`); MimeKit's pinned
`ParserOptions`/`MimeParser` sources and Codex's notes
(`/tmp/emlkit-mbox-reference-review.md`) were read as references only. Their
broad prefix matching, escaped-line handling, scanning and archive mode are not
emlkit policy, and no differential execution or parity is claimed.

**Scope.** `ParseOptions.RecognizeMboxEnvelope`, false by default; with it
false nothing new runs and no byte more is looked at. `Parse` and
`ParseContext` only, at the root only: `Read`, `ReadWithOptions`, `New`,
`textproto`, the `mail` constructors, multipart part headers and the bodies of
embedded messages never recognize one. One line, at input offset 0. This is
the parsing of one message, not of an mbox archive: no splitting, no `>From`
unescaping, no `Content-Length`, no inferred message end, and a `From ` or
`>From ` line anywhere else — a second envelope, one inside the header, one in a
part header or any body — is framed exactly as before (a malformed header key,
or content). `header.misplaced_mbox_envelope` stays reserved with no producer.

**Grammar** (`mbox.go`, case-sensitive): `"From" SP sender 1*WSP weekday 1*WSP
month 1*WSP day 1*WSP HH:MM:SS 1*WSP (year / zone 1*WSP year / year 1*WSP
zone) [1*WSP "remote" 1*WSP "from" 1*WSP host] EOL`; sender and host are
1*VCHAR, the sender not beginning with `:`; English three-letter weekday and
month; day 1–31 as one or two digits; time 00–23, 00–59, 00–60; year four
digits; zone 2–5 upper-case letters or `±HHMM` with hours 00–23 and minutes
00–59, at most one, immediately before or after the year; WSP is SP or HTAB;
no white space before the sender or after the last token; only visible ASCII,
SP and HTAB before the line ending. Weekday and day are not checked against a
calendar: this is syntax recognition, with no timestamp semantics. EOL is
CRLF, LF or CR, the longest; a CR is decided by the byte after it, or by
`io.EOF` itself, and a CR that any other failure follows is undecided. A line
that io.EOF ends with no line ending is not an envelope. Prose such as "From
the 10:30 meeting" is not recognized. RFC 4155 says separator formats vary;
this grammar recognizes the ctime shape common writers produce (`-` and
`MAILER-DAEMON` senders, repeated spaces, tabs, numeric zones as in common
exports, `remote from`) and does not claim to be universal. Unsupported, among
others: no timestamp, other date orders, two-digit years, fractional seconds,
two zones, other zone spellings, trailing tokens or padding, 8-bit or control
bytes, and any byte order mark before `From`.

**Bound.** At most 998 bytes before the line ending (1,000 wire bytes with
CRLF), or `Limits.MaxLineBytes` if smaller. This is an explicit local
recognition bound, not an RFC 4155 or mbox length rule. The recognizer is a
bounded scan, not an incremental grammar parser: it reads one byte past what
it holds only while the bytes do not decide the line, stops early at a byte
that cannot begin `From ` or cannot be in the line (controls, 8-bit) and once
the bound is passed, checks the sender and timestamp grammar only when the
line ending is found, and reads at most one byte past a CR — at most
max(5, bound+2) bytes looked at, while the buffered reader below may already
hold up to 4,096 bytes of the source. A
candidate over the bound, or any line not recognized, is not consumed and
goes to the existing header path: its bytes are never removed silently.

**Result.** The line, line ending included, is copied once onto the session.
`ParseSession.MboxEnvelope` returns an immutable `MboxEnvelope` with
`Bytes()` (a fresh copy) and `Location()` `[0, n)`. It is nil-safe, takes the
lock only briefly, may be called from a `DiagnosticSink`, and keeps the line
whatever then happens to the header. It is stored on neither `Header` nor
`Entity`, and there is no sender, address or date accessor: those bytes are
shape-checked by the grammar only, unauthenticated, and never interpreted as an
address or a date. The root header block begins at `n` and is read as the header
of an input that began there, a BOM at its start included (`input.bom` at
`[n, n+len)`; a UTF-16/32 mark there makes the entity opaque, body from `n`).
Its diagnostics, refusals and `Location`s, its shared-budget and per-block
budget refusals included, move by `n`. Body and part offsets stay absolute.
The `textproto.HeaderLimitError` a refusal carries as its cause is the header
reader's own value, unmodified, with offsets relative to the block. The
envelope's own line ending is in no `LineEndings` and draws no line-ending
note.

**Budgets.** The line lies outside the RFC 5322 header block, so it is
charged to no header field, byte, count or shared header budget. As input it
counts towards `MaxInputBytes`. `MaxLineBytes` lowers the recognition bound.
Retention is the one line, at most 1,000 bytes.

**Diagnostics and profiles.** At most one `header.mbox_envelope`, `SeverityInfo`,
nil path, at `[0, n)`, recorded before every diagnostic of the header, under
every profile, Strict included, and every compatibility value, with no envelope
bytes in its message. `MaxDiagnosticCount` may replace or omit it and
`DiscardDeliveredDiagnostics` may leave it unretained, and a stop published
before it was recorded leaves it unrecorded (below); the line is kept in each
case.

**Errors and stops.** Under `Parse` the input's first error is final, so an
ordinary failure — including a source's own `bufio.ErrBufferFull`, a wrapped
`io.EOF` and `io.ErrUnexpectedEOF` — before the line is decided leaves no
envelope, and the header reader returns that identical error as before; the
source is never read after it. After a decided line, the header and body report
input errors where they always did. A stop (input budget, no progress,
cancellation) that keeps a deciding byte from arriving invents no envelope. A
stop published while bytes already admitted decided a whole envelope (a read
that delivered them and more, or a cancellation seen by that read) keeps the
line, records no diagnostic, and ends the parse with that stop and no entity.
A sink that stops on `header.mbox_envelope` does the same, with the diagnostic
recorded. Recognition, the kept line and the root header framing after it are
chunk-invariant for normal input and for a source error at a fixed cut; that is
not claimed for every input-budget or cancellation timing, since a large read
can publish a stop before the prefix is processed. Nothing is claimed for
decoded bodies: a known truncated base64 input already decodes differently by
chunking with the option off (#34, #29, #12), outside this item.

**Serialization.** No writer writes the line: `Entity.WriteTo`,
`textproto.WriteHeader` and the writers write the message alone, and the
header still round-trips byte for byte on the usual terms. Writer header
ownership is unchanged; #60 is not touched. Raw replay is not provided
(`ROB-020-007`, `ROB-110`).

**API added.** `ParseOptions.RecognizeMboxEnvelope`, `type MboxEnvelope`
(`Bytes`, `Location`), `ParseSession.MboxEnvelope`. Internally,
`rootOriginAt`/`rootContextAt` take the header offset (`rootOrigin`/
`rootContext` keep their behavior), and `rootHeaderError` takes it as well.

**Tests added (not run by Claude).** `mbox_internal_test.go`: grammar table
(accepted and refused forms per token); recognizer over three deliveries with
line endings, bound 998/999, `MaxLineBytes`-style limits and no consumption;
look-ahead byte counts; zero value and nil session. `framing_mbox_test.go`:
root cases under every profile, compatibility and delivery, with exact
envelope bytes, header/body offsets, diagnostics and Strict refusals,
including BOM after the envelope and `From` lines in the body; the
suffix-equivalence oracle (the parse equals the option-off parse of what
follows the line, moved by its length); option off under `Parse`, `Read` and
`ReadWithOptions`; negatives identical to option off (field-like lines, prose
with a time, BOM before, 999 bytes, envelopes in a part header, in a
message/rfc822 body and after a header line); second and mid-header
envelopes; multipart part and shared-budget offsets; root per-block and
shared budgets with cause offsets; a `MaxLineBytes` sweep; a `MaxInputBytes`
sweep across deliveries; `MaxDiagnosticCount` and discard; a sink reading and
stopping; a source-error matrix at every cut (sticky/resuming, with/after data,
three chunkings, three failures, and `bufio.ErrBufferFull` in a subprocess
with a deadline); CR at `io.EOF` versus other failures; cancellation at every
read through the header; no progress; immutability; concurrent reads; writers.
`mail/reader_mbox_test.go`: `mail.NewReader` over such a `Parse`.
`FuzzParse_mboxEnvelope` (root framing only, nothing decoded and no part
iterated): input capped at 4 KiB; the root's error, diagnostics, raw header
fields, BOM, line endings, header/body offsets and undecoded original body
(exactly the input from the body offset, capped at 8 KiB with a visible
failure) are chunk-invariant; not recognized implies the root equals option
off; recognized implies a grammatical prefix ending with its one line ending
and a root equal to the option-off root of what follows it, moved by its
length. Seeds include the known truncated base64 input with and without an
envelope. `FuzzMboxEnvelopeLen` (recognizer alone): exact length against an
independent regular-expression reference for bounds 1–56 and 998, nothing
consumed, at most max(5, bound+2) bytes read, and exactly the line (plus one
byte after a bare CR that is not last) for a recognized one. The finite
traversal helper of the other tests fails visibly at 64 parts, depth 16 or
64 KiB of one body instead of truncating.

**Independent verification completed.** Option-off observations matched all
360 baseline records. Four finite public-API matrices passed 12,579 cases and
74,799 assertions: grammar and preservation; absolute offsets and budgets;
every-cut I/O failures and cancellation; no-progress, scope and sink behavior.
All six packages' unit tests and fourteen prior BOM/line-ending QA steps passed
(`/tmp/emlkit-mbox-initial-verification.json` and `.out`). Review required
visible helper caps, a root/raw-body fuzz oracle instead of claiming parity
for known damaged-base64 decoding, an asserted sink-stop position, and precise
look-ahead/retention/grammar-stage documentation. Those corrections are in the
final version; no parser defect was found in the independent matrices.

Two bounded 45-second fuzz runs passed: `FuzzParse_mboxEnvelope` 1,287,854
executions and `FuzzMboxEnvelopeLen` 1,934,454, 3,222,308 total
(`/tmp/emlkit-mbox-final-fuzz.json` and `.out`). Each uses finite input/work
caps and two workers. No other engine's behavior or broad decoder parity is
claimed; the existing chunk-sensitive damaged-base64 case remains #34.

The first final race run exposed the existing shared-header allocation test
instability, also reproduced on the pre-mbox baseline. It was registered
before changes and independently fixed under #61 / ROB-130-010. The final
13-step sequential QA then passed fixed 100-repeat normal/race allocation
checks, all six packages' full unit/race/386 suites, build, filtered vet,
format/diff and earlier empty/header-only/exceptional-input QA
(`/tmp/emlkit-totalheader-mbox-final-qa.json` and `.out`). Mbox file hashes
remained identical throughout the #61 changes, so the successful fuzz and
independent matrices still cover the final mbox code. Fresh plain vet retains
only the established 13 unkeyed-literal and one WriteTo-signature warnings.

Every implementation delegation and Go execution ran one at a time with
RAM 1536 MiB, swap 0, CPU quota 2, tasks 128 and a deadline. The final service
finished normally with a reported 1.2 GiB peak. All three mbox Claude result
files report no permission denials. This completes only ROB-010-004; trailing
input (#19), general preamble/epilogue preservation (#30), raw replay (#41),
writer ownership (#60) and mbox archive parsing are not completed here.

#### ROB-010-005 — input after the message (Done)

Status: implemented by Claude for
[issue #19](https://github.com/saeroun-ai/emlkit/issues/19) from a design
Codex reviewed and corrected, then independently reviewed and verified by
Codex under enforced resource limits. Codex's initial bounded QA found a classifier defect — the empty
value of the second field accepted a `\r` before the byte after it decided
it, so an ordinary failure there was reported message-like and a `\r\n`
split — a fuzz oracle that read its snapshot before reading the tail,
a reference that shared the product's `From_` recognizer, and documentation
overclaims; all were corrected in the same item. Claude ran no Go build, test,
fuzz or benchmark; only `gofmt` was run. Primary sources: RFC 2046 §5.1.1 (`/tmp/emlkit-rfc2046.txt`: the
epilogue is discard-text, "to be ignored") and RFC 4155 for the `From_`
grammar `ROB-010-004` already implements. No other engine's behavior is
claimed.

**Scope.** One input holds one message. A message that frames no parts has
no end of its own before the input's: its body runs to the end, and a header
block or a whole message pasted into it is body — nothing is scanned, split
or flagged (`TrailingNone`). A multipart message ends at its closing
delimiter; what follows is RFC 2046's epilogue, conformant whatever it says,
and is also where a concatenated message would be. Only that region, after
the message's own (root) closing delimiter, is this item's. Nested
epilogues, preambles, `MaxPreambleBytes`/`MaxEpilogueBytes` and their spans
are `ROB-060-001` (#30); raw replay is #41; message completion state is #10.
No archive splitting, unescaping or `Content-Length`.

**Contract.** `ProfileForensic` only. The first iterator of the message's own
multipart body (root path, known origin, depth 0 — a replaced body is
excluded) whose `NextPart` returns `io.EOF` at a *proven* closing delimiter
registers the trailing input; no input is read for that (a bounded reader
and its state are allocated, once: a later `NextPart` of an ended iterator
finds it registered and allocates nothing), and no iterator, `Walk`,
`mail.Reader`, `Close` or accessor peeks or drains it for this feature. The
buffered readers may already hold some of those bytes, fetched ahead and
counted by `MaxInputBytes` as everywhere. Reading the root's `Body`, or
`WriteTo`, whose verbatim copy of a multipart body includes everything after
its closing delimiter, consumes them as body as before and is no inspection
(`unknown`); mixing either, or a second iterator, with the reader after the
close is unsupported one-shot use, with no guarantee of what it delivers.
`ParseSession.TrailingInputReader` returns that one reader (same value every
call); `ParseSession.TrailingInput` a value snapshot with `Status`, `Start`
(with a known flag; none for unknown/none), `Delivered`, `Location` (the
delivered span; the whole tail only when `TrailingEnded`), `Evidence` and
`Err`. Statuses: `unknown` (zero: not Forensic, not iterated to a proven
close — missing, truncated, failed, refused, body read directly, written by
`WriteTo` or replaced),
`none` (root frames no parts — structural, not a claim about how much input
was read), `open`, `ended` (exact `io.EOF`), `failed` (any other input error,
returned as the identical value: wrapped `io.EOF`, `io.ErrUnexpectedEOF`,
`bufio.ErrBufferFull` included; no error diagnostic and `ParseStop` nil,
though message-like evidence proved by the bytes delivered before or with it
is kept and reported), `stopped` (the session's stop). While open, each
non-empty read checks the session's stop — cancellation included, so
buffered bytes are not delivered after it — before reading and again after;
it delivers `n` bytes with whatever ended it. The first end is final and
returned by every later read with no I/O: a stop or cancellation after an
`io.EOF` or input error replaces neither. A zero-length read does no I/O and
no inspection: `(0, nil)` while open, even after a stop, and the end after. The reader is one goroutine's, not reentrant from a
sink or a `CharsetReader`; abandoning it leaves the rest unread in the
caller's input. It keeps no copy and releases only its own reference on
ending; consumed bytes are not replayable.

**Primitive.** `textproto.(*MultipartReader).Epilogue() (io.Reader, int64,
bool)`: ok only after `NextPart` consumed a closing delimiter on any of its
three paths (body scan; framing line; closing line ended by `io.EOF`), with
the longest line ending, `\r` decided by the next byte or `io.EOF`. The
reader delivers the multipart reader's buffer, then its input (for the root,
the message's buffered reader, then the source), and never more than that
input — for a nested reader, its enclosing part's end. A generic end of
iteration (EOF-class error, refusal, empty body) is not a close. It is the
minimal shared foundation #30 may reuse; it does not complete #30.

**Budgets.** The bytes come from the parse's input, so `MaxInputBytes` counts
them once, at the source, with its existing prefetch semantics unchanged: a
tail past it ends with the session's `limit.input_bytes` stop, bytes already
delivered and the `n` of the read that met it stay delivered, and no promise
is made that every admitted byte still in a buffer is delivered first. `MaxEpilogueBytes` stays reserved and
is explicitly not applied, since the caller, not the parse, drives this
stream. A stop published before registration — including one a source
budget proves while the same read carries a closing delimiter whose line
ending needs one more byte — keeps its precedence, and the status stays
`unknown`. Retention is constant whatever the budgets: no tail bytes, and the
classifier's one line of at most min(998, `MaxLineBytes`) bytes.

**Evidence** (`trailingClassifier`, incremental over delivered bytes only,
chunk-invariant): the first 4096 bytes decide. After blank lines (spaces and
tabs only), the first other line is *message-like* when it is an mbox `From_`
line of `ROB-010-004`'s grammar and bound, or when it and the next
non-continuation line both begin as header fields — visible-ASCII name, `:`,
then space, tab or a line ending — a blank line between them deciding
against. Line endings are the longest match. A `\r` that ends the proof — of
a `From_` line, or of an empty second value — is decided by the byte after it
(a `\n` is included in the evidence, any other byte is not) or by exact
`io.EOF`; an ordinary failure first, a wrapped `io.EOF` included, leaves it
undecided, and that byte arriving past the window decides against. A space,
a tab or a `\n` after the second `:` proves it at once, without waiting for
the value. The evidence range runs from that line to the end of the `From_`
line's ending, of the second field's space, tab or `\n`, or of its empty
value's line ending. One `input.trailing_messages` at `SeverityInfo`, nil path, that
range, no input bytes in its text — unless a stop was seen first. Snapshot
evidence and delivered count are published before the sink runs; a refusing
sink returns `(n, stop)`. `MaxDiagnosticCount`/discard affect only the
diagnostic. `empty`/`blank` need exact `io.EOF` within the window;
`unrecognized` means the bytes looked at decided against, or a byte past the
window arrived first (no claim about the rest); `undecided` means not read
far enough, and stays so after a failure or stop before a decision. Proven
evidence survives later errors and stops. False positives (a header-like
conformant epilogue) and false negatives (other text, a BOM, leading white
space, one field, decisive bytes past 4096) are documented; it is a
suspicion, never a parse. An ordinary, blank, empty or binary epilogue
records nothing.

**Profiles.** Without reading the trailing input, Forensic reads the same
bytes and records the same diagnostics as Tolerant; Tolerant and Strict
offer no reader. `profile.go` now qualifies the parity statement: an explicit
Forensic inspection may record an informational diagnostic about a
conformant message.

**API added.** `TrailingStatus`, `TrailingEvidence` (both with `String`),
`TrailingInput` (`Status`, `Start`, `Delivered`, `Location`, `Evidence`,
`Err`), `ParseSession.TrailingInput`, `ParseSession.TrailingInputReader`,
`textproto.(*MultipartReader).Epilogue`. Internally `Entity.framesParts`
(now also `MultipartReader`'s test). `MultipartReader` interfaces are
unchanged.

**Tests added (not run by Claude).** `textproto/multipart_epilogue_test.go`:
every closing path and line ending, close at `io.EOF`, preamble-only and
empty parts, large tails, six deliveries, four read sizes; no epilogue for
missing/truncated/suffixed closes, empty bodies and refused lines; an
every-cut source-error matrix (sticky/resuming, with/after data, three
chunkings) with identical errors and no reread; nested epilogue ending at its
part; no I/O from `Epilogue` or empty reads. `trailing_internal_test.go`: an
independent line-based reference (`refEvidence`, `refWindowed`) whose `From_`
grammar is `mbox_internal_test.go`'s independent regular expression
(`mboxReference`), not the product recognizer the classifier shares; a
fixture table including every `\r` proof case; window and line-cap
boundaries, a `\r` as the window's last byte with io.EOF, a byte after it,
or neither; undecided prefixes, prefix-finality and one-line retention
checks; and `FuzzTrailingEvidence` (8 KiB cap, any cap/chunking). `framing_trailing_test.go`: exact tails for four close
endings × nine tails (ordinary, empty, blank, binary, message, mbox, blank
then message, delimiters, 15 KB) plus close-at-EOF and no-part cases, three
traversals, five deliveries, two read sizes, with reference evidence and
diagnostics otherwise equal to Tolerant's; mbox offsets; profile parity of
bytes read and diagnostics; none/unknown matrices (leaf with pasted message
or `From_`, header-only, empty, unframed, UTF-16; missing/truncated/suffix,
inner-only close, not iterated, one part, Body read, Body replaced, nil and
zero values); nested epilogues; a `MaxInputBytes` sweep with timing-aware
assertions; an every-cut source-error matrix for three errors and
`bufio.ErrBufferFull` in a subprocess with a deadline, including evidence
proven before the failure keeping its diagnostic; cancellation while
buffered and while reading; no progress; sinks (refusal with reentrant
snapshot, diagnostic budget, discard, a stop before reading with a returned
part kept valid, an empty read after the stop inspecting nothing); concurrent
snapshots; empty reads; `\r` proof cases through the public reader with exact
spans for `io.EOF`, an ordinary error and a wrapped `io.EOF`, at the start
and at the window's end; the first end staying final after a later
cancellation; repeated `NextPart` after the end reading and registering
nothing; `WriteTo` copying the epilogue as body, unchanged from Tolerant;
`FuzzParse_trailing` (16 KiB cap, explicit part/depth budgets; every leaf's
raw, undecoded body drained, so decoder parity, #34, stays out of it;
Forensic and Tolerant, and two deliveries, compared on entities, raw bytes,
read and walk errors and diagnostics; the snapshot taken after the tail is
read; tail equals the input from its start; reference evidence; at most one
diagnostic added by reading it). `mail/reader_trailing_test.go`: `mail.Reader` under
Forensic and the default profile, and a single-part message.

**Independent review and verification.** Codex inspected the source, API,
error and ownership contracts and the actual corrections, then ran:

- Seven final steps: 126 unchanged no-inspection control observations;
  6,255 independent public-API cases with 3,480,712 assertions (raw bytes and
  absolute positions, every-cut input errors including hostile error methods,
  cancellation, sink stops, nested boundaries, WriteTo, budgets and stalled
  readers); and all six packages' unit tests, uncached. The 135 CR/window
  edge cases now pass, with 74,743 assertions.
- Fifteen platform/regression steps, all passed: all packages with `-race`,
  all packages on `GOARCH=386`, build, vet with the two existing warning
  categories disabled, a plain-vet baseline comparison, fourteen prior
  BOM/line-ending checks, mbox controls and four independent matrices, prior
  empty/header-only and exception matrices, formatting and diff checks.
  Plain `go vet ./...` still exits 1 with the same 13 unkeyed-literal warnings
  and one WriteTo-signature warning, and no new warning; it is not clean.
- Two separate 45-second fuzz runs with two workers and finite input/work
  caps: `FuzzTrailingEvidence` 1,420,683 executions and
  `FuzzParse_trailing` 982,782 executions, all passed (2,403,465 total).
  The latter compares raw framing and tails, not decoded-body parity (#34).

The first failed runs are retained, not relabeled as successes. The product's
premature CR proof and two newly written fuzz-test mistakes (a snapshot taken
before the read, then nil-versus-empty diagnostic-list comparison) were fixed
and rechecked. Codex's own temporary QA harness had a syntax mistake and a
no-progress loop bound that was too small; those were corrected separately,
without changing the product. The final slice comparison still detects changed
prior diagnostics and requires exactly the expected added diagnostic.

Evidence: `/tmp/emlkit-trailing-post-review-r2-qa.json` and per-step outputs,
`/tmp/emlkit-trailing-final-platform.json`,
`/tmp/emlkit-trailing-final-fuzz.json`, the preserved initial/post-review logs,
and `/tmp/emlkit-trailing-review-notes.md`. All four Claude result files report
no permission denials and no Go verification by Claude. Every delegation and
Go/parser QA job ran singly in the bounded service (1536 MiB RAM, zero swap,
two-core CPU quota, 128 tasks, deadline); the final platform service completed
normally in 53.735 seconds, with about 1.3 GiB peak memory, and fuzz in 95.156
seconds with 367.8 MiB peak. No Windows/device or cross-engine parity is claimed.

**Not done here.** Body scanning for pasted messages (deliberately not done),
nested epilogue/preamble spans and budgets (#30), raw replay (#41),
completion state (#10), and no claim that a parse read or validated the
whole input.


### ROB-020 — Raw header framing and preservation

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-020-001 | Done | Recover a first-line continuation as a raw malformed field/body candidate with policy-based interpretation. Implemented as a header's malformed prefix, kept byte for byte and interpreted as nothing under every profile and compatibility; Strict refuses locally; a part's prefix never consumes its multipart's delimiter. See "ROB-020-001 — malformed prefixes" below. | Angus/Python/Mime4j compatibility fixtures. |
| ROB-020-002 | Done | Recover orphan continuation lines after malformed fields. Implemented as orphan records: a damaged candidate (no colon or semicolon, invalid name, empty name) with at least one whole continuation line is kept with them byte for byte, interpreted as nothing, written in place; Strict refuses locally; the prefix delimiter guard extended to parts with a record. A read failure met while a bare-CR candidate is being proved is returned as itself, ahead of a delimiter guess cut short by it; a delimiter line already ended by its own line ending keeps the old malformed-line error and the failure is met by the next read. The legacy 64 KiB proof cap counts joined content, so the exact edge is one byte later than the independent probe assumed (not a defect). See "ROB-020-002 — orphan continuation lines" below. | Raw preservation and diagnostic tests. |
| ROB-020-003 | Done | Support policy choices for a colonless line: raw malformed header, semicolon separator, or start of body. Implemented as header recovery under every profile but Strict (textproto.HeaderLimits.Recover): a colonless or invalid-name line is joined to the field before it as an unindented continuation, kept apart as a damaged line when a well-known field follows, or begins the body; the semicolon fallback is unchanged. See docs/robustness-notes/header-recovery.md. | Ambiguity and deterministic-precedence tests. |
| ROB-020-004 | Done | Recognize whitespace-only header/body separators when enabled. A white space only line stays a continuation of the field before it and the block ends at the first line after it that is no field (header.whitespace_body_separator). See docs/robustness-notes/header-recovery.md. | Spaces/tabs/mixed fixtures. |
| ROB-020-005 | Done | Recover missing header/body separator without losing the first body line. The block ends before the first line that is no field, which is handed back and begins the body, at the root and in parts (header.missing_body_separator). See docs/robustness-notes/header-recovery.md. | Plain and multipart part fixtures. |
| ROB-020-006 | Review | Preserve invalid/empty field names and raw 8-bit values while exposing only safe canonical lookup keys. Partial: invalid-name lines are kept apart byte for byte and never become lookup keys (header.invalid_name); empty-name lines are still skipped without keeping their bytes, because the header budget matrices count on that. See docs/robustness-notes/header-recovery.md. | Control/NUL/non-ASCII key tests. |
| ROB-020-007 | Planned | Preserve original field case, order, folding, whitespace, and physical line endings. | Byte-exact write tests. |
| ROB-020-008 | Done | Diagnose duplicate singleton fields without deleting them. Implemented: one header.duplicate_singleton per block, nothing removed, the first occurrence read; Recovered when a repeated Content-Type, Content-Transfer-Encoding or Content-Disposition differs. See docs/robustness-notes/header-recovery.md. | MIME-Version, Content-Type, From, Date fixtures. |
| ROB-020-009 | Planned | Make every header parser path panic-free for arbitrary bytes and pathological lengths. | Native fuzz target and allocation bound. |

#### ROB-020-001 — malformed prefixes (Done)

Status: implemented by Claude for
[issue #20](https://github.com/saeroun-ai/emlkit/issues/20), independently reviewed
and verified by Codex on Linux amd64 / Go 1.26.10. Only ROB-020-001 is Done;
other ROB-020 items remain separate. All five delegation calls ended normally
with no permission denials. Claude ran no Go build, test, fuzz or benchmark;
Codex executed the verification below.

**Region.** A header block — the message's or a part's, after a UTF-8 byte
order mark and after an opt-in mbox envelope, which lies outside it — whose
first line begins with SP or HTAB begins with a malformed prefix: its first
logical candidate, read exactly as a field's lines are read (that line, every
continuation line after it, and whatever a `'\r'` that proves to be content
joins to them, on `ROB-010-002`'s line rules). Whitespace-only lines are
lines of the prefix; only an empty line ends a block (`ROB-020-004` is not
done here). The line after the prefix is read as the block's next line always
was: the empty line, or a field candidate held to every existing rule, a
colonless or bad-key line still failing as before (`ROB-020-003`,
`ROB-020-005`, `ROB-020-006`). A UTF-16/32-marked block is not read and has no
prefix. Before, every entry point refused such a block ("malformed MIME header
initial line"), with no entity under `Parse` and the iteration ended at a
part for a caller that stopped at the first error.

**Retention and interpretation.** `textproto.Header.MalformedPrefix` returns an
immutable `MalformedPrefix` (`Present`, `Bytes` — a fresh copy each call —
`Location` relative to the block on `LineEndings`' terms, the mark counted, and
`Lines`); zero value and nil Header report none; getters do no I/O. Its bytes
are captured as read, after the charge that admitted them: the input's own
bytes, endings, NUL, 8-bit and control bytes as they were. It is no field:
`Get`, `Has`, `Values`, `Fields`, `FieldsByKey`, `Len` and `Map` are unchanged
and never see it, so an indented `Subject`, `Content-Type` or
`Content-Transfer-Encoding` declares nothing under every profile and every
`Compatibility` value; the entity is interpreted from the fields after it. No
interpretation view is offered (an earlier `AsField` proposal was dropped as
unverified for Angus's edge behavior).

**Budgets and errors.** The prefix is one field candidate: the shared count is
asked before its first byte (a spent count refuses it there, unread), it is
charged to the field counts once when read whole, and its lines and bytes to
`MaxLineBytes`, `MaxHeaderFieldBytes`, `MaxHeaderBytes` and the shared bytes as
read, each byte once; capturing charges nothing. A refused block keeps neither
prefix nor fields. A failure other than `io.EOF` keeps only lines read whole —
a cut line, and a line whose `'\r'` the cut line was still settling, are
dropped from the prefix and its capture — and is returned as the identical
value; `io.EOF` keeps an unterminated last line. Retention is at most the
admitted bytes (`MaxHeaderFieldBytes`, and the shared total across a parse).

**Parts.** In a part's header that began with a prefix — and only there —
every later line is first looked at as a delimiter line of its multipart
(`MultipartReader.match` at the start of a line: opening or closing, white
space after the boundary, a boundary with a colon included). Only a proved
delimiter — its line ending (`"\r\n"`, `'\n'`, or a `'\r'` decided by the byte
after it or `io.EOF`) or the end of the input seen after the white space —
ends the block before it, uncharged to the header; a `'\r'` waiting on the
previous line is a line ending; the part's body is empty and the multipart
frames the delimiter as usual. `textproto.Part.HeaderSeparatorMissing` reports
it. Most lines are decided by the scanner's sticky peek within one buffer. A
line whose white space runs past the buffer is held out of it, bounded by the
framing line budget `MaxLineBytes` — the bound the multipart reader puts on a
delimiter line — or by one buffer when there is no line budget, and by no
budget of the header alone: a delimiter is not header content, so a proved one
is never refused for what `MaxHeaderBytes`, `MaxHeaderFieldBytes`,
`MaxHeaderCount` or the shared header totals have left, and the hold may look
at more bytes than those have left before the line is classified, as any
input look-ahead may (input budget, cancellation and no-progress apply to it as
to every read). Proved, the held bytes become the multipart reader's held
candidate (`MultipartReader.cand`), which the empty body proves and
line-budgets as framing, and no header budget is charged; otherwise they are
replayed as the first bytes of the line (`headerScan.replay`, a buffer at a
time) and charged then, once, to every header budget, which refuses the line
where it overflows one — so `--B` + 4093 spaces + `: value` is the field it is,
the fields after it stay the part's, and a field the header budgets cannot
hold is refused by them where they say; a line held to the line budget
overflows it, and a spent shared field count refuses it at its first byte.
With no line budget (legacy entry points, or a disabled `MaxLineBytes`) the
multipart reader bounds an undecided framing candidate to its 4 KiB buffer,
and so does this guard: a line decided within that buffer is read as above,
and one still undecided past it is the multipart reader's own
`bufio.ErrBufferFull` framing refusal, which ends the iteration as it does for
any such framing line. `Parse` always has a positive line budget by default. A
failure before the line is decided, a false candidate (`--Bx`, `--B : value`)
and a header without a prefix are read as before; a boundary containing `'\r'`
or `'\n'` is never held.

Codex's QA of this item found the guard twice taking the wrong side and
corrected it here: first an unproved long padding (`--B` + 4093 spaces +
`: value`) taken for a delimiter, losing the fields after it; then a proved
long-padded delimiter refused for exhausted header-only budgets (block, field,
shared bytes), losing a healthy sibling the old error-and-retry path recovered
(54 baseline cases against 36 lost). The hold is now bounded by the framing
line budget alone. This preserves recovery that existed: the parent's
baselines (108 retry cases, and the 54 header-budget retry cases) reached the
healthy sibling by calling `NextPart` again after the initial-line error; now
the damaged part itself is returned and the sibling follows. General
delimiter-aware header ending, for blocks without a prefix, is
`ROB-020-005`'s; the guard is the narrowly shared foundation it may reuse.

**Parse policy** (`firstline.go`, through `headerLineEndings`): one
`header.first_line_continuation` per block over the whole prefix (Recovered;
input-free message counting lines), and for such a part
`header.missing_body_separator` (Recovered) at the point its header ended,
ordered by offset with `input.bom` and the line-ending notes. `ProfileStrict`
records them as InvalidMetadata and refuses the entity on `ROB-010-002`'s
terms with the first refusing note by offset (a mark's first): prefix and
fields kept, Content-Type not interpreted, raw body, FatalError copy carried
past `MaxDiagnosticCount`, no session stop, siblings read (`NextPart`
continues; `Walk` ends at the refused entity as for every Strict refusal).
Sink stops, budgets, cancellation and no-progress keep their precedence.
`Read`, `ReadWithOptions`, `New`, `textproto` and `mail` heal identically and
record nothing.

**Serialization.** `WriteHeader` (hence `CreateWriter`, `CreatePart`,
`Entity.WriteTo`, `NewMultipart` bodies) writes the prefix first, after any
UTF-8 mark and before every field, those added since included, so it never
becomes a field's continuation; every byte in order, a last line with no
ending followed by `\r\n` and one ended by a bare `'\r'` by `\n` (the evidence
`Bytes` is unchanged). An unmodified complete CRLF header with a prefix
round-trips byte for byte; reparsing written output gives the written prefix
and writing again the same bytes. `Copy` and every mutation keep the prefix;
`HeaderFromMap` does not. Write errors are returned as the writer's own. Not
raw replay (`ROB-020-007`, #41); no consumed body is replayed.

**Compatibility evidence.** The 16 + 9 minimized fixtures, run once each through
Angus 2.0.5/Jakarta Mail 2.1.5, Mime4j 0.8.15 and Python 3.14.10, are archived
with their 175 observations in
[`docs/qa/ROB-020-001/references/`](qa/ROB-020-001/references/README.md)
(`manifest.json` pins versions, jar and snapshot hashes and limits) and pinned
in `framing_firstline_test.go` with emlkit's results. Recorded intentional
differences: Angus reads a trimmed first line as a field and frames an indented
multipart, keeps colonless lines as raw headers, drops a whitespace-only first
line, and throws `IndexOutOfBoundsException` for a whitespace-only first line
followed by indented lines; Mime4j default/permissive keeps indented colon
lines as semantic fields (an indented Content-Type frames a multipart) and
drops other runs, strict fails; Python default/compat32 omits the lines with one
defect per line, reads a bare CR as a line ending (so `first-internal-cr` is a
missing separator to it) and recognizes no byte order mark, strict raises; MimeKit
was not run (`CompatibilityMimeKit` takes the conservative choice). No parity
with any engine is claimed; #17 stays open.

**API added.** `textproto.MalformedPrefix` (`Present`, `Bytes`, `Location`,
`Lines`), `textproto.Header.MalformedPrefix`,
`textproto.Part.HeaderSeparatorMissing`. Producers of the reserved codes
`header.first_line_continuation` and `header.missing_body_separator`
(the latter only as above). No signature changed; `HeaderLimits` unchanged.
Compatibility implication: inputs that failed at every entry point now
succeed; a caller that must reject them checks `MalformedPrefix().Present()`
or the diagnostic. The error string "malformed MIME header initial line" is no
longer produced.

**Tests added or updated (not run by Claude).** `textproto/prefix_test.go`
(case table over every delivery and buffer size; view, ownership and
mutations; writer output, write failures at every byte, reparse stability;
block, line, field, count and shared budgets; every-cut read failures; CR
settlement failures; multipart delimiter guard over boundaries `B` and `b:c`,
three endings, opening/closing, false/truncated candidates; every-cut multipart
failures held to an exact table for ordinary failures and to the bytes alone
for `io.EOF`; long padding 0–5000 around the buffer, fields and delimiters,
with and without budgets, offsets and once-only charging, failures inside the
held padding; a deadline subprocess for a source failing with
`bufio.ErrBufferFull` inside the prefix and the delimiter look-ahead and hold;
`FuzzMultipartReader_malformedPrefixDelimiter`, boundary-aware raw framing with
an independent delimiter reading; padded opening and closing delimiters under
exactly spent block, field, shared-byte and shared-count budgets, with the
shared budget charged the header blocks alone, and padded fields refused by
the budget they overflow); `framing_firstline_test.go` (reference
fixtures with provenance over
profiles × compatibilities × deliveries; legacy entry points; indented
metadata; middle child under NextPart/Walk/Strict; delimiter recovery matrix
mirroring the retry baseline; Strict diagnostic cap; sinks, reentrant reads and
discard; budgets and input-budget sweep; shared count; every-cut input
errors; cancellation per byte and buffered; no-progress 99/100;
writers and `NewMultipart`; the long-padding matrix through `Parse` with its
diagnostics, and with every header budget exactly spent); `mail/reader_firstline_test.go`. Updated:
`limit_header_bytes_test.go` (the unrelated malformed part header is now a
line without a separator, the prefix having become healed),
`textproto/header_budget.go` (stale doc),
`textproto/bom_test.go` (a mark then white space now yields a prefix),
`framing_bom_test.go` (that input moved out of the unchanged-malformed list),
`limit_header_total_test.go` (the leading-whitespace residual is now a
candidate refused at its first byte; white space added to the endless-candidate
preflight list), and `FuzzReadHeader_lineEndings` (prefix compared across
deliveries, held to the input's bytes, and to what writing it back gives).

**Final Codex verification.** The final functional code passed 16/16 sequential
independent/unit stages: 28,314 finite cases and 222,512 assertions, 1,110
unchanged observations (including source call counts; only a generated random
boundary normalized), 108 baseline retry cases, all 54 small-header-budget retry
cases, and the uncached full six-package suite. The 15/15 platform/regression
stages passed full race, GOARCH=386, build, formatting, whitespace and previous
BOM/line-ending/mbox/empty-input regressions. Plain vet still reports exactly
13 existing unkeyed literals and the existing WriteTo signature warning; the
other analyzers pass, with no new warning. Three final 30-second, two-worker
fuzz runs passed: header line endings 472,258 executions, first-line Parse
37,602 and multipart prefix delimiters 66,099 (575,959 total).

The two concrete delimiter regressions described above were fixed and retested,
not accepted as compatibility changes. Error-cut test expectations and the
parent QA's random-boundary, cancellation and sink-stop assumptions were checked
against existing behavior before correction; original failures remain recorded.
One public API comment was corrected after the final tests; reversing exactly
that comment reproduces all 165 source/doc hashes captured before verification,
so no executable or test code changed afterwards. Final logs, source hashes,
known limitations and reference snapshots are committed under
[ROB-020-001 QA evidence](qa/ROB-020-001/README.md). Standalone probes and initial
failure logs remain under `/tmp/emlkit-firstline/`; they are not a portable
checked-in differential harness. Every Go/parser QA and Claude invocation ran
as the only bounded service, with RAM 1536 MiB, swap 0, CPU quota 2, 128 tasks
and a deadline; final jobs exited normally with no OOM. This is local QA, not
completion of CI, corpus or generic fuzz infrastructure (#11–#14).

**Not done here.** Orphan continuations after fields (`ROB-020-002`), colonless
lines (`ROB-020-003`), whitespace separators (`ROB-020-004`), general missing
separators (`ROB-020-005`), invalid names (`ROB-020-006`), raw replay
(`ROB-020-007`, #41), completion state (#10), the profile/compatibility engine
(#17). A part header without a prefix whose first field is shaped like a padded
delimiter (`--B` + long white space + `: value`) can still be taken for a
headerless part by the existing empty-header check; that is unchanged here and
tracked with #29 (`ROB-060-007` to `ROB-060-009`).

#### ROB-020-002 — orphan continuation lines (Review)

Status: implemented by Claude for
[issue #20](https://github.com/saeroun-ai/emlkit/issues/20) within the scope
Codex reviewed; awaiting Codex's independent review and QA. Claude ran no Go
build, test, fuzz, vet or benchmark and claims no result; nothing here is Done
until that verification. Only this item changed status.

**Scope.** A continuation line belongs to the logical candidate before it, so
it is an orphan exactly when that candidate is damaged — when the rules a block
has always been read with make no field of it, judged on the whole candidate as
they always were: no colon or semicolon anywhere in it; a name (before the first
colon, or the first semicolon when there is no colon) holding white space, NUL,
a control or 8-bit byte, or its first line's ending because the only colon is on
a continuation line (so `X-Test; value` + ` folded: value` qualifies: the colon
wins, as before, and no new semicolon interpretation is made); or an empty name.
Such a candidate with **at least one continuation line read whole** is kept,
damaged line included, as an orphan record, and the block is read on. The
damaged line is kept because it is what makes the lines orphans and what keeps
them from continuing the field before them when written back. Before, every
such block failed ("malformed MIME header line/key": no root entity; the part
an error, the sibling reached by retrying), or an empty name lost the lines
silently. Unchanged: valid folding; a field the semicolon fallback accepts
(`X-Bad; value` + ` orphan` stays `X-Bad: value orphan`); a `\r` that is content
of a valid field (`ROB-010-002`: `Subject: before\r: value\r orphan` stays one
Subject); a damaged line with no continuation (empty name skipped, others the
same error) — `ROB-020-003` and `ROB-020-006` still own those, and nothing here
gives a damaged line a name, key or value. A blank line followed by an indented
line is still the body.

**Settling a `\r`.** A damaged line whose last line ends with a bare `\r` before
a name byte: if the next line is its content (`ROB-010-002`) and brings a whole
continuation line, the whole is a record. An empty name is held and read on
past as it always was (baseline already read and skipped its joined lines and
read a following field as a field). A colonless or invalid line was refused at
its `\r` with the next line unread, and still is unless `settleCR` proves a
record: the next line does not begin `1*name-byte *WSP ":"`, and it (or a line
a further content `\r` joins to it) ends and is followed by SP/HTAB. Only then
are those bytes read, as the record's, and charged once. Otherwise the same
error as before is returned and every byte after the `\r` — a field like
`Next: value` included — is in the reader exactly as at 5b7086c. (QA of the
first implementation found it read and dropped such a field, 72 of 144
bare-CR observations; a later 16-byte peek window fixed that but left
records longer than 16 bytes unrecovered, which review rejected.)

*Proof window.* Bounded by the budgets, not by any buffer: the joined physical
line's content (each content `\r` included, line endings excluded) by
`MaxLineBytes − damaged line content − 1`; all joined bytes with their line
endings by what `MaxFieldBytes`, `MaxBytes` and the shared byte budget have
left; the one byte proving the continuation by none. With none of the four in
force (`ReadHeader`, `Read`'s own header reader), 64 KiB (`crLegacyCap`, the
parse defaults for a field and a line). A proof that does not fit is no proof:
the old error, not a budget refusal; a record the proof admits is then read as
any field and a budget its continuation lines overflow refuses the block
(`TestReadHeader_orphanCRBudgets` pins both sides). The shared field count is
asked first; a spent count refuses before anything is looked at — new
precedence over the old malformed error, as for any line after an unsettled
`\r` (`ROB-010-002`). In a part's header, a whole delimiter line of the parent
(dash-boundary, optional `--`, white space, a line ending or the end of the
input) beginning after the `\r` or after any `\r`/`\n` among the looked-at bytes
is no record: the old error, the delimiter left for retry. A line that only
begins like one (`--Bx…`, `--B` + spaces + other bytes) joins as any line.

*Looking and giving back.* Bytes are peeked, one more than held at a time,
while they fit in the reader's buffer; past it they are taken out, exactly as
many as are looked at, so a failure or `io.EOF` is met at the same byte for
every buffer and chunking. A record's taken-out bytes become the scanner's
replay (read first by `nextChunk`, which now handles line endings in it, with
`hasContinuationLine` and `atBlockEnd` looking at it first through `vpeek`; a
`\r` it ends with is decided by the reader's next byte). Otherwise they are
given back before the error: a part's to its `MultipartReader`
(`restore`: one flat `pre` over the sticky source, subtracted in `offset` and
`windowAt`; the buffered reader keeps its pointer and size); any other reader
by `restoreReader`: its buffered bytes are moved after them, a copy of the
reader (its buffer then empty, its source and any undelivered failure kept)
becomes the source of a new reader of those bytes and that copy, installed in
place, so no buffer is shared and nothing reads itself. **The reader's buffer
grows**: at least doubled and at least to hold what was given back. A restore
happens only for a proof that looked past the whole buffer, so each one doubles
it, a proof that fits needs none, and the copies a reader accumulates are as
many as its doublings — logarithmic in the longest proof, independent of how
many headers are read (300 restores pinned) — and their buffers sum to less
than the final one. `Size()` is observable and increases only so. `Read` and
`Parse`'s own root reader is discarded on that error. A read failure other than
`io.EOF` met before the proof decides is returned as itself after what was
taken out is given back; a failure the source had not delivered stays in the
copy. Source calls may come sooner than before; remainders and results do not
change.

**Failures.** A failure other than `io.EOF` that kept a damaged line's
continuation from being looked at (the look past it, or a cut continuation or
settling line) is returned as itself, identical, instead of the damaged line's
error; only whole lines are kept, so a record left without a whole continuation
is dropped. `io.EOF` keeps an unterminated last line. Nothing is read after an
observed failure. A source failing with `bufio.ErrBufferFull` is that source's
failure (subprocess-deadline probe).

**Representation and cost.** `textproto.OrphanContinuation` (`Bytes` — fresh
copy, `Location` in the block on `LineEndings`' terms, `Lines` ≥ 2; zero value
reports nothing) and `Header.OrphanContinuations` (fresh slice, reading
order). Internally an immutable `orphanRecord` (exact bytes, block offset,
lines, `after` = fields read before it; no damage class or other speculative
state for `ROB-020-003`/`006` is kept). The 001 capture is not used: a candidate is
known to be damaged only once read whole, and capturing every candidate would
copy every healthy field. Instead the scanner notes each kept physical line's
ending (CRLF, LF, CR or none) in a 16-entry array in the per-block scan, reused
per candidate (a longer candidate grows one overflow slice, reused by the
block), and a damaged candidate's exact bytes are rebuilt from its kept lines —
whose content is the input's and in which every `"\r\n"` is a line ending —
after the candidate was read and charged. Healthy path: one stored byte per
line, no allocation for candidates of up to 16 lines, and `headerField` gains a
read-order `seq` (56 → 64 bytes, same size class). Retained: at most the bytes
the budgets admitted per record. Transient: one exact-size allocation per
damaged candidate (and one per CR-held damaged line, whether or not it becomes
a record).

**Budgets.** A record is one candidate: shared count preflight at its first
byte, field counts charged once, every byte charged once to line, field, block
and shared budgets; a refused block keeps no record, prefix or field.

**Serialization and mutation.** `Copy` and every mutation keep the records;
`HeaderFromMap` does not. `WriteHeader` (hence `CreateWriter`, `CreatePart`,
`Entity.WriteTo`, `NewMultipart`) writes each record before the first field
read after it that the header still has (by `seq`), after fields added since,
or at the end; deleting a neighbour never moves a record among the fields read
with it nor drops it. Bytes as read; a last line without an ending gets `\r\n`,
one ended by a bare `\r` gets `\n` (evidence unchanged). A record begins with
its damaged line (never white space), so it can never continue the field
written before it, and reparsing gives the written records and the same fields.

**Parts — reviewed dependency on `ROB-020-005`.** Reading on past the damage
must not read the parent's delimiter as a header line, or the old retry path's
sibling would be lost (baseline fixture `part-missing-separator`). So the 001
delimiter guard (`parentDelimiter`/`holdDelimiter`) also runs in a part's
header after a recovered record, unchanged: only a proved delimiter ends the
header (framing `MaxLineBytes`, no header budget charged for it), false or long
candidates are replayed a buffer at a time and charged once, no line budget
keeps the one-buffer framing refusal. `Part.HeaderSeparatorMissing` and
`header.missing_body_separator` now have this second narrow producer. General
missing-separator recovery stays `ROB-020-005`'s (Planned), and the no-prefix
padded-first-field behavior stays #29's.

**Parse policy** (`orphan.go`): one `header.orphan_continuation` per block, over
its first record, ordered by offset with `input.bom`, the prefix and the
line-ending notes; Recovered under Tolerant/Forensic; Strict records
InvalidMetadata and refuses the entity locally on 001's terms (first refusing
note by offset, FatalError copy kept past `MaxDiagnosticCount`, Content-Type
not interpreted, raw body, no session stop, `NextPart` continues, `Walk` ends
at the refused entity). Sink stops, budgets, cancellation and no-progress keep
their precedence. Every Compatibility value is the same; no engine parity is
claimed (#17).

**Tests added (not run by Claude).** `textproto/orphan_test.go`: case table
over every delivery and buffer for every damaged head, colon-only-in-continuation,
whitespace-only continuation, indented metadata, NUL/8-bit/control bytes, EOF,
LF/CR/mixed endings, CR content in damaged lines and records, BOM, prefix, several
records, and the unchanged cases (folding, semicolon, the CR baseline, damaged
lines alone, blank-then-indented); lookup isolation; view, ownership and every
mutation; writer order through every mutation, byte-exact round trip, terminators,
reparse stability and write failures at every byte; budget edges and shared
charges; every-cut ordinary failures and EOF (sticky/one-shot, with/without data,
bytes and error); unsettled-CR failures; multipart first/middle/last parts,
missing separators with every line ending, false/padded candidates, CR-held
damaged lines before a delimiter; long padding with exactly spent header budgets
and no-line-budget refusal; every-cut multipart failures; a deadline subprocess
for `bufio.ErrBufferFull`; a within-build folded-versus-flat healthy allocation
comparison with every measured parse checked (not a comparison with the previous
commit); record checks in
`FuzzReadHeader_lineEndings`. `framing_orphan_test.go`: root fixtures under every
profile, compatibility and delivery with exact diagnostics; legacy entry points;
unchanged inputs; indented metadata; middle child (NextPart/Walk/Strict); delimiter
recovery; nested; mbox offsets; Strict diagnostic cap; sinks; budgets and input
cap sweep; shared count; every-cut input errors; cancellation; no-progress;
writers; `FuzzParse_orphanContinuation`. `mail/reader_orphan_test.go`.
`refusing` in `framing_firstline_test.go` now includes the new code.
Bare-CR tests (added after QA, not run by Claude):
`TestReadHeader_orphanCRLeavesNextUnread` (a following field, invalid name,
joined line then field, LF/CR/EOF, >buffer field, name, joined line and
content-CR chain followed by a field, a long joined line at the end of the
input, a delimiter-shaped line: the old error and every byte after the `\r` in
the reader, every delivery and buffer), `TestReadHeader_orphanCRLongRecord`
(4096- and 5000-byte joined lines, an invalid-name head, a 6000-byte content-CR
chain, a bare-CR continuation, 8-bit/NUL content, under `ReadHeader` and finite
budgets, every delivery and buffer), `TestReadHeader_orphanCRBudgets` (line,
field, block and shared rooms at the edge and one less, the proof that does
not fit versus a record a budget then refuses, `MaxInt64`, spent shared count
before any look, nothing charged for the look, the 64 KiB legacy cap and one
past), `TestReadHeader_orphanCRRestoreReader` (300 restores on one caller
reader with the buffer size fixed after the first, a pending one-shot or sticky
failure kept after the given-back bytes, failures inside the look returned as
themselves with what was looked at given back), `TestMultipartReader_orphanCRLong`
(long record, false delimiter-like prefix, padding then non-padding, real
opening/closing delimiters right after the `\r` and padded past the buffer, a
long field: retry reaches the sibling; a hundred given-back parts at their
offsets with the reader's own buffer), `TestReadHeader_orphanCRLookFailure`.
`genTotalFixture` (`limit_header_total_test.go`, `FuzzParse_totalHeaders`) no
longer folds its empty-name lines: a folded empty name is now an orphan record
that records a diagnostic, outside the valid trees that fixture's exact oracle
models (its zero-byte seed tail produced one); records under the totals are
pinned in `framing_orphan_test.go` instead.

**Not done here.** Damaged lines without continuation (`ROB-020-003`,
`ROB-020-006`), whitespace separators (`ROB-020-004`), general missing
separators (`ROB-020-005`), whole raw replay (`ROB-020-007`, #41), #17, #29.

### ROB-030 — RFC 2047 and unstructured text

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-030-001 | Done | Decode conformant B/Q words and adjacent-word whitespace correctly. Implemented by the bounded decoder in rfc2047.go, which Header.Text and parameter values use; conformant values decode as before. See docs/robustness-notes/rfc2047.md. | RFC examples and round-trip tests. |
| ROB-030-002 | Done | Recover embedded encoded words in tolerant mode. Implemented: glued words, words in quoted strings, folds inside a word, unterminated words, and adjacent same-charset words joined as bytes before conversion. See docs/robustness-notes/rfc2047.md. | Strict/tolerant differential fixtures. |
| ROB-030-003 | Done | Preserve malformed/unknown encoded words while decoding valid neighbors. Implemented: an undecodable word stays verbatim while its neighbours decode; Base64 is repaired and bad Q escapes kept literal. See docs/robustness-notes/rfc2047.md. | Partial decode tests. |
| ROB-030-004 | Done | Diagnose raw UTF-8/8-bit header text and apply configured fallback charset. Implemented: raw UTF-8 passes with rfc2047.raw_utf8; invalid raw 8-bit uses HeaderTextOptions.FallbackCharset or U+FFFD; the raw value stays in the header. See docs/robustness-notes/rfc2047.md. | Valid/invalid UTF-8 fixtures. |
| ROB-030-005 | Done | Reject or neutralize decoded CR/LF/NUL header injection. Implemented: decoded CR and LF become a space and NUL becomes U+FFFD, reported as rfc2047.header_injection. See docs/robustness-notes/rfc2047.md. | Security tests. |
| ROB-030-006 | Done | Limit encoded-word count, nesting-like work, and decode expansion. Implemented: one forward pass, 10 000 words per value, bounded hook output and 64 diagnostics per value (rfc2047.word_limit). See docs/robustness-notes/rfc2047.md. | Adversarial complexity tests. |

### ROB-040 — Structured RFC 5322 headers

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-040-001 | Done | Return partial address lists with per-address diagnostics. Implemented by mail.ParseAddressListTolerant and Header.AddressListTolerant: every entry with its raw text and diagnostics, valid ones typed. See docs/robustness-notes/mail-structured-headers.md. | Valid-invalid-valid sequences. |
| ROB-040-002 | Done | Support configurable loose parsing for unquoted commas and local-only addresses. Unquoted commas in display names and local-only addresses are read by default and can be switched off (AddressOptions). See docs/robustness-notes/mail-structured-headers.md. | MimeKit-compatible fixtures. |
| ROB-040-003 | Done | Bound nested comments and address groups. | Deep nesting tests. |
| ROB-040-004 | Done | Tolerate obsolete route syntax, empty groups, repeated separators, and broken angle brackets without panic. Routes, empty groups, repeated separators, broken angle brackets and unclosed quotes, comments and groups are read without a panic. See docs/robustness-notes/mail-structured-headers.md. | Corpus table tests. |
| ROB-040-005 | Done | Parse internationalized addresses and preserve undecodable originals. Raw UTF-8 addresses are accepted and undecodable bytes kept verbatim; no IDNA conversion. See docs/robustness-notes/mail-structured-headers.md. | RFC 6532 and IDN cases. |
| ROB-040-006 | Done | Parse common obsolete dates, report invalid dates, and retain raw values. Implemented by mail.ParseDateTolerant and Header.DateTolerant: obsolete zones, two-digit years, missing seconds or zone, comments, wrong weekday; the raw value is kept. See docs/robustness-notes/mail-structured-headers.md. | Zone/year/comment/weekday table. |
| ROB-040-007 | Done | Return partial Message-ID/References lists. Implemented by mail.ParseMsgIDListTolerant, Header.MsgIDListTolerant and Header.MessageIDTolerant. See docs/robustness-notes/mail-structured-headers.md. | CFWS, truncation, invalid ID tests. |
| ROB-040-008 | Done | Add tolerant Received parsing with clause-level preservation. Implemented by mail.ParseReceived and Header.Received: clauses in order with their raw text, the date after the semicolon. See docs/robustness-notes/mail-structured-headers.md. | Real provider fixtures. |
| ROB-040-009 | Done | Harden List-* parsing and preserve multiple URLs/NO values. Implemented by mail.ParseListURLs, Header.ListURLs, ParseListID and Header.ListID: every URL, NO and damaged entries kept. See docs/robustness-notes/mail-structured-headers.md. | RFC 2369/2919 fixtures and fuzz. |

`ROB-040-003` bounds the work of comments and groups; it does not cap their
depth. The bound is a single in-place pass: skipping a comment costs time
linear in its bytes and no memory beyond a depth counter, so a well-formed
comment nested to any depth is accepted, and the size of the header field that
holds it is the only size bound it needs. `Parse` applies its header budgets;
standalone header and address accessors do not acquire those budgets, so their
callers must bound input size themselves. Refusing deep but valid nesting would
trade a recoverable value for no saving.

- **msg-id and list-command accessors.** `Header.MessageID`, `MsgIDList` and
  `ListCommandURLList` share one comment scanner. It used to collect each
  skipped comment with per-byte string concatenation, so an `n`-byte comment
  cost about `n²/2` bytes of garbage (565,546,464 bytes cumulative for the
  public 32 KiB reproduction in
  [issue #9](https://github.com/saeroun-ai/emlkit/issues/9)). No caller reads a
  comment's text, so it is now scanned without being built and allocates
  nothing (16 bytes for that reproduction, the returned identifier). Where a
  comment ends, how backslash escapes apply (an escaped `(`, `)` or `\` does
  not change the nesting; a trailing lone backslash escapes nothing), and
  whether it is unterminated are byte-for-byte unchanged, checked against the
  previous loop as an oracle.
- **Results and errors are unchanged.** A comment that is never closed
  consumes the rest of the value and the accessor returns its existing error.
  `MsgIDList` and `ListCommandURLList` still return the entries completed
  before the damage together with that error; the damaged entry is not
  reported as parsed. Keeping a msg-id that is complete before a broken
  trailing comment is `ROB-040-007`'s partial-list work. These standalone
  accessors have no `ParseSession`, so their failures are returned errors, not
  session diagnostics.
- **Addresses and groups.** `Header.AddressList`, `ParseAddress` and
  `ParseAddressList` call `net/mail` unchanged, and no local scanner is added.
  `net/mail` (Go 1.26) skips comments iteratively with a depth counter, and
  costs memory linear in the comment because it still builds the text;
  it parses group members with groups disabled, so call depth is fixed and a
  group inside a group is refused. Its lists remain all-or-nothing, and it
  refuses a comment before an address although RFC 5322 allows one there; tests
  record both as current behavior for `ROB-040-001` and `ROB-040-004` to change.
- **Evidence.** `mail/header_comment_test.go` and
  `mail/header_comment_internal_test.go`: cumulative heap bytes
  (`runtime.MemStats.TotalAlloc`) at N, 2N, 4N and 8N for flat, nested, escaped
  and repeated comments before, between and after msg-ids and list commands
  (constant), and for `net/mail` comment and group shapes (linear);
  escaped-delimiter, nested, truncated, leading, trailing and multiple-comment
  tables; adversarial and nested groups; verbatim header write-back after every
  accessor; and focused fuzz targets `FuzzSkipComment` (oracle differential)
  and `FuzzHeaderCommentAccessors` (no panic, raw header unchanged). These
  targets do not advance `ROB-130`.

### ROB-050 — MIME media types, dispositions, and parameters

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-050-001 | Done | Parse and retain the base media type even when parameters are malformed. | Invalid-tail matrix; no silent multipart misclassification. |
| ROB-050-002 | Planned | Recover missing type/subtype/slash using conservative defaults and diagnostics. | Strict/tolerant behavior tests. |
| ROB-050-003 | Planned | Parse loose unquoted parameter values up to the next semicolon. | Angus/MimeKit-compatible fixtures. |
| ROB-050-004 | Planned | Recover unclosed quotes, bad escapes, and Windows filename backslashes. | Apple/Windows fixtures. |
| ROB-050-005 | Planned | Define deterministic duplicate-parameter precedence while preserving all raw occurrences. | First/last/conflicting values. |
| ROB-050-006 | Planned | Fully support RFC 2231 extended and continuation parameters in any segment order. | Encoded/unencoded/mixed tables. |
| ROB-050-007 | Planned | Preserve partial RFC 2231 values when segments, percent escapes, or charsets are invalid. | Partial decode diagnostics. |
| ROB-050-008 | Planned | Support nonstandard RFC 2047 and raw UTF-8 filenames in tolerant mode. | Common client fixtures. |
| ROB-050-009 | Planned | Resolve filename/name/filename* conflicts without discarding candidates. | Candidate API and precedence tests. |
| ROB-050-010 | Planned | Separate original filename from an optional filesystem-safe display/storage name. | Traversal, NUL, separators, reserved names. |

`ROB-050-001` ([issue #22](https://github.com/saeroun-ai/emlkit/issues/22))
keeps a valid base type or disposition when its parameters are malformed.
Before it, `Content-Type: multipart/mixed; boundary=B; broken` went through
`Parse` with no error and no diagnostic. The whole-field parser rejected the
field over one item, the raw value became the "media type", and the multipart
iterator then failed with an empty boundary. The contract is:

- **Scope.** Only `Parse`, only a field whose bounded whole-field parse fails,
  and only when its base is valid on its own: `type "/" subtype` for
  `Content-Type` (RFC 2045 §5.1; there is no default subtype), one token for
  `Content-Disposition`. A missing or invalid base keeps the old result — raw
  value, no parameters, the error, no diagnostic — for `ROB-050-002`. A
  conformant field parses exactly as before under every profile. `Read`,
  `ReadWithOptions`, `New` and the standalone `Header` accessors repair
  nothing.
- **What is kept.** The normalized base, plus every item that parses on its own
  (before or after the damage, with name case-folded and value case kept, so a
  boundary stays case-sensitive per RFC 2046 §5.1.1). Items are split by the
  existing quote-aware scan, so a quoted or escaped semicolon never separates
  items. An unterminated quote is the damaged suffix: what precedes it is kept,
  and nothing it swallowed is guessed at. A parameter repeated with one value
  is kept silently, as `mime.ParseMediaType` already keeps it.
- **Stray empty items.** `mime.ParseMediaType` rejects an empty item between
  semicolons (`multipart/mixed;; boundary=B`, `; ;`, `boundary=B;;`,
  `boundary=B; ;`), accepting only one trailing semicolon followed by
  whitespace. Such empty items are repaired like malformed ones: they declare
  nothing, the parameters around them are kept, and they are reported in the
  field-level diagnostic. They are still not parameter occurrences: no budget
  charges them, and the scan only counts them, with no allocation. The single
  trailing semicolon `mime` accepts stays a conformant field and is unchanged.
  Under a budget cut, the retained prefix is rebuilt without empty items, so a
  stray one before the cut sends the field to the repair rather than vanishing
  unreported.
- **What is dropped.** A parameter is dropped entirely, so that no framing
  boundary is chosen arbitrarily (precedence is `ROB-050-005`), when it is
  repeated with conflicting values, or repeated by an occurrence whose value
  is unknown: a damaged item that still names it, or an RFC 2231 extended
  value whose charset has no decoder. Every parameter with an RFC 2231
  continuation segment among the retained occurrences is also dropped
  entirely, including its well-formed segments and any whole parameter of
  the same name, as the budgets already drop one (`ROB-050-006`,
  `ROB-050-007`). Well-formed segments and undecodable extended values are
  well-formed syntax: they are not counted or reported as malformed, only
  dropped under this deferred policy, and the Action says which.
- **A repaired multipart without a usable boundary.** If a repair leaves a
  multipart `Content-Type` with no boundary it may use — a conflicting or
  damaged repeat, or one hidden by an unterminated quote — no boundary is
  chosen or invented. The entity is *unframed*:
  - `EffectiveContentType` still reports the declared multipart type;
  - `MultipartReader` returns nil;
  - the body is one opaque, uninterpreted part holding the input's bytes;
  - the iterator, `Walk` and `mail.Reader` hand that part out and go on to the
    siblings after it.

  Its diagnostics are `SeverityInvalidMetadata`, and their Action says the
  parts inside were not read, so nothing claims the multipart was rebuilt. A
  valid multipart with no boundary that no repair touched keeps its existing
  behavior; boundary inference is `ROB-060-002`.
- **Bounded.** Repair reads only the occurrences the existing count and segment
  budgets retained, so an over-budget field is never parsed whole a second
  time. With both budgets disabled the field is scanned once, and its size is
  bounded only by the header budgets. Each item is parsed once, names are
  tracked in map sets, and diagnostic lists keep first-occurrence order rather
  than being sorted. The work is therefore linear in the retained bytes (a
  structural argument; the timing and allocation test only checks it).
- **Diagnostics.** At most one `mime.content_type_invalid` or
  `mime.disposition_invalid` (malformed items, including an unterminated quote)
  and one `mime.parameter_duplicate` (conflicting or unreadable repeats) per
  field, after any budget diagnostic. They carry the field, path and header
  position. Their Action names the base, the kept and dropped parameters in
  first-occurrence order, whether only budgeted occurrences were read, and
  whether the body was left uninterpreted. They are recorded once at
  construction, or once at the first lazy disposition read. A sink stop wins,
  is sticky, and leaves no decoder and no later diagnostic behind it.
- **Profiles (local decision table for this producer).** Tolerant (the
  default) and Forensic repair identically, with `SeverityRecovered`
  (`SeverityInvalidMetadata` for an unframed multipart). Strict records the
  same codes as `SeverityInvalidMetadata` and refuses only this entity, with an
  entity-scoped `FatalError` carrying that diagnostic and an empty effective
  type or disposition. What a refusal still prevents depends on when it is
  known, and nothing is rewound or read early:
  - **`Content-Type`** is read at construction, so its refusal leaves the body
    exactly as read: no decoder, no part iteration.
  - **`Content-Disposition`** is read lazily. If the charset stage reads it
    (under `SkipTextAttachmentDecoding`), the refusal keeps that stage from
    running, although transfer decoding is already set up. If it is read later
    by `EffectiveContentDisposition`, `mail.Reader` or a walk callback, it only
    refuses the entity: decoders already chosen stay and bytes already read
    stay read.

  The parse is not stopped (`ParseStop` stays nil), and the parent iterator
  continues with the next sibling. `mail.Reader` returns the refusal without a
  part and then continues. `Walk` ends with a refusal as soon as one is
  observed:
  - before the callback, for the root's `Content-Type`, a child's refusal from
    its iterator, or a disposition already read by the charset stage or an
    earlier caller;
  - after the callback, if the callback itself read it, before any of the
    entity's children;
  - before reading another part of a container that a callback has meanwhile
    found refused.

  It reads no field to find out, a session stop outranks the refusal, and the
  container check costs the tree depth per part, bounded by `MaxMIMEDepth`,
  for strict parses only. Every `Compatibility` value behaves the same. Forensic adds nothing
  here: the raw field is already byte-exact in the header for every profile,
  and Forensic's raw spans in general are `ROB-110`. This is not #17's general
  profile activation.
- **Views.** `EffectiveContentType` and `EffectiveContentDisposition` return the
  repaired interpretation with a nil error, and they drive the decoders,
  `MultipartReader`, the `SkipTextAttachmentDecoding` charset decision and
  `mail.Reader` classification, so a repaired attachment stays an attachment.
  `Header.ContentType`/`ContentDisposition`, and `mail` accessors built on them
  such as `AttachmentHeader.Filename`, still report the legacy failure for the
  unchanged raw field. Filename policy is `ROB-050-009`.
- **Writer defect found during this item, fixed separately.** While this item
  was in review, `Entity.WriteTo` panicked ("assignment to entry in nil map")
  for any entity whose raw multipart `Content-Type` fails the legacy parse,
  through `Read` as well as `Parse`. The defect predated this item and was not
  part of it, so the tests here check raw header and body bytes instead. It was
  fixed as `ROB-120-008` ([issue #58](https://github.com/saeroun-ai/emlkit/issues/58)):
  `WriteTo` now refuses such an entity with an explicit error before writing
  anything, rather than panicking. That refusal is a documented limitation and
  not raw replay (#41); see the `ROB-120-008` contract.
- **Evidence.**
  - `metadata_repair_test.go`:
    - a 22-row invalid-tail matrix: broken item before/after the boundary,
      mixed case and whitespace, quoted semicolon, escaped quote, unterminated
      quote before and hiding a boundary, conflicting/case-variant/damaged/
      identical duplicates, missing name or value, empty items, RFC 2231
      extended and continued, and invalid bases left alone. Each row runs at
      the root and as a child between two siblings, under all three profiles,
      with raw header writeback and repeat-accessor checks;
    - the conformant happy path unchanged under every profile;
    - disposition repair with the charset decision (skip on/off, Strict);
    - budget interactions: count, segments, both disabled, one disabled;
    - sink stop under every profile;
    - every split point and one-byte input;
    - a nested path;
    - legacy `Read`/`New`/accessor regression;
    - returned-map copies;
    - a linear-cost check;
    - a focused fuzz target `FuzzParseContentTypeRepair`: conformant input
      unchanged, a valid kept base, no invented parameter names, and Strict
      refusing exactly where Tolerant repairs.
  - `metadata_repair_unframed_test.go`:
    - the unframed fallback for conflicting, damaged and hidden boundaries and
      for a budget cut behind a broken item, at the root and as a child: exact
      body bytes, raw header, the following sibling, `Walk` leaves, and
      unchanged behavior for an unrepaired missing boundary and for Strict;
    - Strict refusals seen by `Walk`: root `Content-Type`, an `Entity` copy, a
      disposition read before, during and by the charset stage, a disposition
      nobody read (no diagnostic forced), sink-stop precedence, and a child
      refusal that ends only the walk while the iterator reaches both siblings;
    - accurate RFC 2231 labels: a well-formed series, a lone later segment, a
      damaged segment family, an undecodable extended value alone and
      repeating a plain one, and first-occurrence order.
  - `mail/reader_repair_test.go`: the repaired root, a repaired attachment
    with exact bytes and the charset decision, a Strict
    child/disposition/root refused and continued, a sink stop on a lazy
    repair, and the opaque part with its following sibling, flat and nested.

  The fuzz target does not advance `ROB-130`.

  Reference behavior: during review, Codex ran Mime4j 0.8.15 (DEFAULT,
  PERMISSIVE and STRICT) and Python 3.14.10 on nine synthetic values. The
  evidence is recorded in the review notes and an issue #22 comment; the
  executor did not run the references. Both engines keep the boundary and the
  body for standalone malformed items, for unrelated unclosed tails, and for
  all four stray-empty-separator forms; Python reports the last as "parameter
  entry with no content". On a `B`/`C` boundary conflict, Python keeps the
  first value and Mime4j the last; this item deliberately keeps neither. This
  is a small synthetic sample, not a differential harness, and no parity with
  any engine is claimed.

### ROB-060 — Multipart recovery

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-060-001 | Review | Preserve preamble and epilogue with raw spans. Partial: the preamble is kept up to MaxPreambleBytes and returned by MultipartPreamble (limit.preamble_bytes when cut); the epilogue is reachable as before and MaxEpilogueBytes is not enforced yet. See docs/robustness-notes/multipart-recovery.md. | Empty/nonempty/limited fixtures. |
| ROB-060-002 | Done | Infer a missing boundary parameter from credible delimiter lines. Implemented: a multipart with no boundary is framed on the boundary its delimiter lines use (multipart.boundary_inferred). See docs/robustness-notes/multipart-recovery.md. | Angus-compatible positive/negative fixtures. |
| ROB-060-003 | Done | Detect declared/actual boundary mismatch and optionally adopt the actual boundary. Implemented: when no line is a delimiter of the declared boundary, the boundary the body uses is adopted (multipart.boundary_mismatch). See docs/robustness-notes/multipart-recovery.md. | Antivirus-rewrite fixtures. |
| ROB-060-004 | Done | Rank multiple boundary candidates deterministically and diagnose ambiguity. Implemented: candidates are scored and the earliest of the best is chosen; a second qualifying boundary records multipart.boundary_ambiguous. See docs/robustness-notes/multipart-recovery.md. | Preamble dashes and nested candidates. |
| ROB-060-005 | Done | Recover missing start and closing delimiters while returning partial parts. Implemented: a missing closing delimiter ends the last part at the end of the input with io.EOF (multipart.close_not_found); a body with no delimiter is returned as one part (multipart.start_not_found). See docs/robustness-notes/multipart-recovery.md. | Python defect-compatible tests. |
| ROB-060-006 | Done | Accept truncated final delimiter and missing terminal line ending where unambiguous. Implemented: a delimiter cut by the end of the input, at least six bytes of it, is read as one (multipart.close_truncated). See docs/robustness-notes/multipart-recovery.md. | Every truncation point. |
| ROB-060-007 | Planned | Handle boundary LWSP, invalid suffixes, mixed endings, and buffer splits. | Exhaustive split-position tests. |
| ROB-060-008 | Planned | Avoid false positives for boundary prefixes embedded in content. | Adversarial body corpus. |
| ROB-060-009 | Done | Support empty multipart, consecutive delimiters, and headerless empty parts according to profile. An empty multipart yields no part and io.EOF with multipart.empty; consecutive delimiters and headerless parts were already read as empty and headerless parts; ProfileStrict keeps the earlier error. See docs/robustness-notes/multipart-recovery.md. | Strict/tolerant fixtures. |
| ROB-060-010 | Done | Recover missing child header/body separators without losing sibling synchronization. A part header is healed like the root's, so a damaged part no longer ends the iteration and its siblings are reached. See docs/robustness-notes/header-recovery.md. | Broken-middle-child tests. |
| ROB-060-011 | Review | Handle reused/prefix-related inner and outer boundaries. Partial: a boundary an enclosing multipart already uses is reported (multipart.nested_boundary_reused) and framed as declared; boundaries that are prefixes of one another were already told apart. See docs/robustness-notes/multipart-recovery.md. | Deep nested fixtures. |
| ROB-060-012 | Done | Ignore or decode illegal multipart CTE according to explicit policy. Policy: the declared encoding of a multipart is ignored and the parts are framed from the raw body, reported as multipart.illegal_transfer_encoding (Recovered) unless it is 7bit, 8bit or binary; message/rfc822 with base64 or quoted-printable is decoded as declared and reported as message.illegal_transfer_encoding (Info). See docs/robustness-notes/transfer-encodings.md. | Outlook/broken-gateway fixtures. |
| ROB-060-013 | Planned | Guarantee linear-time boundary scanning under repeated near-matches. | Complexity benchmark. |

### ROB-070 — Content-Transfer-Encoding

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-070-001 | Done | Normalize CTE tokens with comments, whitespace, and common trailing junk. Implemented by canonicalCTE (cte_tolerant.go): comments, case and white space are syntax; quotes, trailing text, encoded words and known misspellings are repaired with cte.invalid_token; ProfileStrict repairs nothing. See docs/robustness-notes/transfer-encodings.md. | Header token tables. |
| ROB-070-002 | Done | Preserve raw body and continue for unknown CTE at every API layer. Unknown mechanisms keep the raw body with the cte.unknown warning at Read, Parse and mail.Reader; pinned by TestParse_cteTokenRepair and the existing warning tests. See docs/robustness-notes/transfer-encodings.md. | Root/mail/session compatibility tests. |
| ROB-070-003 | Done | Diagnose 7bit violations without discarding bytes. Implemented for bodies that declare 7bit under Parse: the first NUL or 8-bit byte records cte.seven_bit_violation (Info) and every byte is kept. See docs/robustness-notes/transfer-encodings.md. | High-bit/NUL/long-line fixtures. |
| ROB-070-004 | Done | Ignore Base64 whitespace and configured nonalphabet junk with diagnostics. Implemented by tolerantBase64: white space anywhere is framing, bytes outside the alphabet are skipped with cte.base64_junk. See docs/robustness-notes/transfer-encodings.md. | Angus/Python-compatible fixtures. |
| ROB-070-005 | Done | Repair missing/invalid Base64 padding and truncated final quanta when possible. Implemented: a final group of two or three characters yields its whole bytes (cte.base64_padding_missing), misplaced padding is ignored (cte.base64_padding_invalid). See docs/robustness-notes/transfer-encodings.md. | Length modulo 4 matrix. |
| ROB-070-006 | Done | Handle data after Base64 padding and URL-safe alphabet policy. Implemented: data after padding is decoded as a further segment (cte.base64_data_after_padding); '-' and '_' are read as the URL-safe alphabet (cte.base64_url_alphabet); ProfileStrict keeps the standard decoder and its corruption error. See docs/robustness-notes/transfer-encodings.md. | Strict/tolerant tables. |
| ROB-070-007 | Done | Return decoded Base64 prefixes and raw fallback for unrecoverable length cases. Implemented: the decoded prefix is always delivered; a single left-over character is dropped with cte.base64_length_invalid; a body cut by an input failure yields whole groups and the failure itself. See docs/robustness-notes/transfer-encodings.md. | Prefix/fallback assertions. |
| ROB-070-008 | Done | Complete malformed quoted-printable recovery, including CR-only soft breaks. Implemented by tolerantQP, a streaming state machine replacing the normalizer over the standard reader: literal malformed escapes, soft breaks with trailing white space or ended by a bare CR or LF, control bytes kept, no line limit. See docs/robustness-notes/transfer-encodings.md. | Invalid-hex/truncation/control tables. |
| ROB-070-009 | Done | Support uuencode aliases and configurable missing begin/end recovery. Implemented by tolerantUU for x-uuencode and its aliases: text before begin skipped, missing begin or end recovered with diagnostics, stripped trailing spaces restored. See docs/robustness-notes/transfer-encodings.md. | Angus-compatible uuencode fixtures. |
| ROB-070-010 | Done | Apply decoded-size budgets during streaming, not after allocation. The decoders are streams with one fixed buffer; the decoded budgets of ROB-001-005 bound their output as it is delivered. See docs/robustness-notes/transfer-encodings.md. | Expansion and cancellation tests. |

### ROB-080 — Charset decoding

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-080-001 | Planned | Maintain a tested alias/quirk registry for common mail charset labels. | Provider corpus and IANA aliases. |
| ROB-080-002 | Planned | Expose raw bytes and decoded UTF-8 view simultaneously. | Read-order independence tests. |
| ROB-080-003 | Planned | Define strict/replace/raw policies for invalid byte sequences and midstream errors. | Prefix preservation tests. |
| ROB-080-004 | Planned | Detect BOM/declaration conflicts without silently overriding metadata. | UTF-8/16/32 matrix. |
| ROB-080-005 | Planned | Support important legacy mail charsets through the optional plugin. | Japanese/Korean/Chinese/Cyrillic fixtures. |
| ROB-080-006 | Planned | Keep standard UTF-7 and IMAP modified UTF-7 behavior separate and explicit. | Cross-codec negative tests. |
| ROB-080-007 | Planned | Add opt-in declaration-suspicion heuristics without making guessing the default. | False-positive/negative corpus. |
| ROB-080-008 | Planned | Bound charset decode expansion and state-machine work. | Adversarial charset tests. |
| ROB-080-011 | Done | Accept the Korean compatibility labels `cp949` and `x-windows-949`, case-insensitively, in the optional charset plugin. | Fixed CP949 bytes including an extended syllable, body and RFC 2047 B/Q headers, near-miss labels still unknown. |

`ROB-080-011` ([issue #1](https://github.com/saeroun-ai/emlkit/issues/1)) maps
`cp949` and `x-windows-949` to `golang.org/x/text/encoding/korean.EUCKR`, the
WHATWG `EUC-KR` decoder that `euc-kr`, `windows-949` and `ks_c_5601-1987`
already resolve to, which is code page 949 including its extended syllables.
Before it, both labels were an unknown charset. Evidence:
`charset/korean_alias_test.go`, on the fixed bytes Python's `cp949` codec
produces for "안녕하세요 똠" — 똠 (`0x8C 0x63`) exists only in the CP949
extension, so strict EUC-KR would reject it — through `charset.Reader`, a
`message.Read` body in `8bit` and `base64`, and `Header.Text` on B and Q encoded
words, for both labels in mixed and upper case and for the three existing labels
unchanged; near-miss labels stay unknown with the raw body kept, and a body that
declares no charset is not guessed to be Korean. It adds two labels and nothing
else: no detection, no repair of wrong declarations, no recovery diagnostics.
`ROB-080-001`, the general alias registry, and `ROB-080-005` remain Planned.

### ROB-090 — Nested messages and special MIME structures

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-090-001 | Done | Parse `message/rfc822` and `message/global` as nested entities with independent diagnostics. Implemented on request: Entity.EmbeddedMessage and ParseOptions.WalkEmbeddedMessages read the embedded message as an entity of the same parse with its own path and diagnostics; unasked it stays a leaf. See docs/robustness-notes/embedded-messages.md. | Plain/encoded/truncated fixtures. |
| ROB-090-002 | Done | Activate the reserved `MaxMessageDepth` budget for the nested entities introduced by ROB-090-001. MaxMessageDepth is enforced where an embedded message is read (limit.message_depth, entity-scoped). See docs/robustness-notes/embedded-messages.md. | Deep message chain. |
| ROB-090-003 | Done | Support `multipart/digest` default child type semantics. A part of a multipart/digest with no Content-Type is message/rfc822. See docs/robustness-notes/embedded-messages.md. | Missing child Content-Type fixtures. |
| ROB-090-004 | Done | Preserve `multipart/related` root/start/type relationships. Implemented by Node.RelatedRoot and Node.Resolve. See docs/robustness-notes/structure-view.md. | CID/root-selection fixtures. |
| ROB-090-005 | Review | Model `multipart/report` and common delivery/disposition/feedback reports. Partial: multipart/report shape is checked and Node.ReportFields returns the field groups of a report part; no typed model per report kind. See docs/robustness-notes/structure-view.md. | RFC samples and malformed blocks. |
| ROB-090-006 | Done | Validate `multipart/signed` and `multipart/encrypted` shape without modifying signed raw bytes. Shape checked (signed.structure_invalid, encrypted.structure_invalid); Entity.RawSpan and Node.SpanStart/SpanEnd give the signed part's bytes in the input untouched. See docs/robustness-notes/structure-view.md. | Signature hash invariants. |
| ROB-090-007 | Done | Support `text/rfc822-headers` as a header block entity. text/rfc822-headers and message/global-headers are read by EmbeddedMessage as a header block entity. See docs/robustness-notes/embedded-messages.md. | Valid/malformed header fixtures. |
| ROB-090-008 | Done | Preserve unsupported `message/partial` and `message/external-body` metadata safely. message/partial and message/external-body are kept as they are and reported as feature.unsupported_media_type; nothing is reassembled or fetched. See docs/robustness-notes/structure-view.md. | No external fetch, raw preservation. |

### ROB-100 — Message body and attachment view

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-100-001 | Done | Preserve the complete MIME tree before producing any flattened convenience view. Implemented by mail.ReadTree, which keeps every entity as a Node at its place before any view is taken. See docs/robustness-notes/structure-view.md. | Hierarchy round-trip tests. |
| ROB-100-002 | Done | Classify attachments using disposition, filename candidates, media type, and related references with explainable reasons. Implemented: each Node has a Role and the Reason for it, from disposition, filename, media type and multipart/related membership. See docs/robustness-notes/structure-view.md. | Cross-client corpus. |
| ROB-100-003 | Done | Select `multipart/alternative` bodies by caller capability without discarding alternatives. Implemented by Node.Bodies, which picks one alternative by the caller's preference and discards none. See docs/robustness-notes/structure-view.md. | Nested alternative fixtures. |
| ROB-100-004 | Done | Resolve `multipart/related` inline resources by normalized Content-ID/Location. Implemented by Node.Resolve over Content-ID and Content-Location. See docs/robustness-notes/structure-view.md. | HTML CID cases. |
| ROB-100-005 | Done | Represent embedded messages, calendars, zero-byte files, duplicates, and unknown media types. Embedded messages (RoleMessage, DescendMessages), calendar alternatives, zero-byte parts (Size 0), duplicates (DuplicateOf by SHA-256) and unknown media types (attachment) are represented. See docs/robustness-notes/structure-view.md. | Typed-view tests. |
| ROB-100-006 | Done | Make unknown charset/CTE nonfatal in `mail.Reader` while retaining diagnostics. | Existing inconsistency regression tests. |
| ROB-100-007 | Done | Preserve the parse engine's bounded effective MIME metadata consistently in `mail.Reader`, without reparsing original headers to undo decisions or budgets. | Root/child classification agrees with charset decisions and multipart structure; no duplicate diagnostics; unchanged raw headers and standalone accessors; explicit defensively copied snapshot/mutation contract; stopped root/synthetic wrapper respects session stop. |

`ROB-100-006` ([issue #1](https://github.com/saeroun-ai/emlkit/issues/1)) closed
the inconsistency the gap snapshot recorded: `mail.CreateReader` returned a nil
`Reader` for a root with an unknown transfer encoding, and `mail.Reader.NextPart`
dropped such a part, although `message.Read` and the multipart iterator returned
a usable entity with the warning. Both now accept `IsUnknownEncoding` exactly as
they accepted `IsUnknownCharset`, but only together with a non-nil entity and
never inside a `FatalError`, so a sink stop or resource budget that wraps either
warning still ends traversal — and a `(nil, FatalError)` wrapping the charset
warning, which used to be dereferenced, is now returned. Evidence:
`mail/reader_warning_test.go` (raw root body, a warned child between readable
siblings with iteration continuing, unknown charset and both-unknown unchanged,
`Parse` + `NewReader` keeping both diagnostics at paths `[1]` and `[2]`, sink
stops wrapping either warning, `MaxPartCount` still fatal, header and body I/O
errors still errors). The predicates themselves were unchanged by this item;
`ROB-120-007` later made them, and `mail.Reader`'s own test, bounded and exact.

`ROB-100-007` ([issue #15](https://github.com/saeroun-ai/emlkit/issues/15))
closes a cross-layer inconsistency. Under `Parse` the engine reads
`Content-Type`, and `Content-Disposition` when it needs it, under the parameter
budgets, and decides decoders from what it read. `mail.Reader` instead reparsed
the raw header with the unbudgeted `Header` accessors. So with
`MaxParameterCount: 1`, `Content-Disposition: attachment; a=1; a=2` was an
attachment to the engine — its charset skipped under
`SkipTextAttachmentDecoding`, one `limit.parameter_count` recorded — and inline
text to `mail.Reader`, where the duplicate parameter is a parse error. The
contract is:

- **Effective metadata.** `Entity.EffectiveContentType` and
  `EffectiveContentDisposition` return what the entity was constructed with, as
  its parse interpreted it: under the budgets for `Parse`, and equal to the
  `Header` accessors' answer for `New`, `Read` and `ReadWithOptions`, including a
  field they cannot parse (raw value, no parameters, the error). A struct-literal
  `Entity` has no snapshot and reads its current header. `mail.Reader` classifies
  by these; the inline/attachment rule itself is unchanged.
- **A construction snapshot.** Editing `Entity.Header` after construction does
  not reinterpret the entity, just as it never re-chose its decoders or its
  multipart boundary. The edit stays visible through `Header.ContentType`,
  `Header.ContentDisposition` and `WriteTo`. This also applies to entities from
  `New` and `Read` given to `mail.NewReader` after an edit, which used to be
  classified by the edited header; constructing a new entity is how an edit
  takes effect. Every returned parameter map is a fresh copy, and none aliases
  the parser's maps.
- **Read once.** `Content-Disposition` is captured at construction and
  interpreted at most once, by whichever of the charset stage and
  `EffectiveContentDisposition` asks first. A field nobody asks about still
  costs and reports nothing (the existing rule that budgets apply where the
  engine reads), and a field past a budget is diagnosed once whoever asks and
  however often — repeated calls, `Entity` value copies, concurrent callers.
  `mail.Reader` asking under `Parse` without `SkipTextAttachmentDecoding` is
  such a read, so the budget now applies there and is diagnosed once. A read
  after a stop keeps the bounded result and records nothing.
- **Readable from a sink.** The first read interprets the field under a
  short entity lock. That step is pure and calls no callback. The result is
  published, the lock released, and only then is the diagnostic delivered. So a
  `DiagnosticSink` told about the refusal may read the same entity back — or a
  copy of the `Entity` value — through either effective accessor and call
  `ParseStop`, and gets the published value instead of deadlocking on its own
  delivery (the first review found that a `sync.Once` held across the delivery
  did deadlock). A stop the sink returns afterwards is the parse's sticky stop as
  usual. A concurrent first reader in another goroutine gets the same value
  but may get it before the delivery finishes. Not supported: a sink making the
  *first* over-budget reading of an entity nothing has asked about yet. That is
  a recording from inside a delivery, which the session supports for no
  diagnostic and which blocks; a read within budget records nothing and is
  safe.
- **Stops.** `Entity.ParseStop` is the read-only view of a whole-parse stop (a
  sink stop or a parse-scoped budget such as `MaxPartCount`; not an
  iterator-scoped `MaxMIMEDepth` refusal), identical to the value every scope
  reports. `mail.Reader.NextPart` checks it before every call. The single-part
  wrapper `NewReader` builds belongs to no parse, so it used to hand over a root
  `Parse` had returned with a stop, and to report `io.EOF` after a sink stopped
  the parse while the root body was being read; both now return the identical
  stop on every retry, including a stop that wraps a decoding warning. A stop
  raised while the part is being classified ends traversal the same way. `Parse`'s
  own error must still be handled first.
- **Not claimed.** No new healing. Without a budget cutting it, a duplicate or
  otherwise unparseable parameter list still fails both the engine and
  `mail.Reader` with the same verdict; recovering such fields is #22 /
  `ROB-050`. Filename selection (`ROB-050-009`), body-tree selection
  (`ROB-100-001`–`003`) and error-graph classification (`ROB-120-007`) are
  untouched.
- **Evidence.** `mail/reader_metadata_test.go`: the issue reproduction at the
  root and in a child, with the option on and off; a matrix of
  duplicate/unterminated/segment tails, inline vs attachment, and media-type-only
  parts between two readable siblings, with exact body bytes and whether the
  charset stage ran; sink stops during `Parse`, during iteration, during
  classification and during the root body read, with sentinel and
  warning-wrapping stop errors and repeated retries; issue #1 warnings preserved;
  the mutation contract for `Parse`, `New` and literal entities; one-byte input
  and half-chunk body reads through a nested tree, with the raw header bytes
  written back. `effective_test.go`: header edits, returned-map edits against
  later calls and the multipart boundary, once-only diagnostics, the post-stop
  read, the scope of `ParseStop`, and legacy/literal equivalence with the
  standalone accessors. For sink reentry, deadline-guarded tests have a sink read
  the entity (direct and copied) and then continue or return a sentinel stop.
  They check the cached values, a single diagnostic, the identical `ParseStop`
  on retries and from `Walk`, concurrent first readers combined with a reading
  sink, and `mail.Reader.NextPart` returning that stop on every retry. These
  tests block, and fail, when the lock is held across the delivery.

### ROB-110 — Raw storage, replay, and spooling

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-110-001 | Planned | Define raw span/source abstraction for seekable and non-seekable inputs. | Equivalent-result tests. |
| ROB-110-002 | Planned | Add configurable memory-to-temp-file spooling with deterministic cleanup. | Threshold, close, failure, cancellation tests. |
| ROB-110-003 | Planned | Support caller-provided blob storage and quotas. | Fake-store contract tests. |
| ROB-110-004 | Planned | Permit repeatable raw/decoded reads in indexed mode while retaining one-shot streaming mode. | Read-order and concurrent-read policy tests. |
| ROB-110-005 | Review | Preserve untouched message/header/body bytes after semantic reads. Partial: Entity.RawSpan gives the untouched input range of a part after any semantic read; the caller keeps the input. See docs/robustness-notes/structure-view.md. | Parse-read-write DKIM body/header hash tests. |
| ROB-110-006 | Planned | Compute optional hashes during streaming without extra passes. | Known digest and partial-read tests. |
| ROB-110-007 | Planned | Ensure no temp files, descriptors, or goroutines leak on any error path. | Stress tests with leak assertions. |

### ROB-120 — Security hardening

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-120-001 | Planned | Audit all input-derived slice bounds, integer arithmetic, and allocation sizes. | Fuzz/32-bit-oriented boundary tests. |
| ROB-120-002 | Planned | Make parsing and decoding complexity linear or explicitly budgeted. | Near-match and nesting benchmarks. |
| ROB-120-003 | Done | Prevent header injection in all decoded/setter/serialization paths. Writers refuse a caller-set field whose value holds CR, LF or NUL or whose name is no field name, before writing it; the encoding setters encode such text; decoded header text has CR, LF and NUL neutralized (ROB-030-005). A writer that could not be made is returned as a nil interface. Pinned by mail/writer_injection_test.go. | CR/LF/NUL tests. |
| ROB-120-004 | Planned | Keep filename sanitization, HTML safety, archive expansion, and executable handling outside core parsing. | API boundary/documentation tests. |
| ROB-120-005 | Done | Never perform network fetches for Content-Location/external-body while parsing. No code path opens a network connection; Node.Resolve only matches parts of the message. See docs/robustness-notes/structure-view.md. | Hostile URI tests. |
| ROB-120-006 | Planned | Preserve cryptographic raw representations and clearly mark repaired signed content as unverifiable. | DKIM/S-MIME/PGP invariants. |
| ROB-120-007 | Done | Make every parser-wide error classification cycle-safe and non-forgeable over input-derived error graphs. | Hostile-graph tests at each predicate: no hang, no panic, no nil dereference, no misclassification. |
| ROB-120-008 | Done | Make writer generation and serialization refuse a syntactically unparseable Content-Type with an explicit error, never a panic, an invented media type, parameter map or boundary, or silent metadata loss ([issue #58](https://github.com/saeroun-ai/emlkit/issues/58)). | Lowercase/uppercase malformed tail, duplicate boundary, malformed text/plain and missing subtype, at the root and as a part, through `CreateWriter`, `CreatePart`/`CreatePartWithBuffer`, `Parse`→`WriteTo` and `NewMultipart` Body reads: no panic; no output from the refused entity or part before its header is validated (output of its parent and of earlier siblings may already have been written and is not withdrawn); a good part after a refused one, deterministic repeated reads with no blocked worker; valid boundary generation, charset/encoding policy and copy-on-write unchanged. |
| ROB-120-009 | Done | Isolate NewMultipart from caller Header mutation while keeping the generated entity header consistent with its body and all header views ([issue #60](https://github.com/saeroun-ai/emlkit/issues/60)). Implemented: NewMultipart copies the header, settles the boundary and drops the transfer encoding in its own copy at once, and generates the body from that copy; the caller's Header is never modified. Pinned by newmultipart_header_test.go. | Generated/replaced/valid boundary, CTE removal, setup failure and Close/retry cases; Get/Map/Fields/Raw/WriteHeader consistency, original field bytes/order/duplicates, post-construction mutation policy, full/race/386 and independent QA. |

**New issue registration — ROB-120-009.** Independent finite QA on `64d11b8`
confirmed that lazily reading `NewMultipart`'s Body can mutate the caller's
Header: generated/replaced boundaries leak to it, deleting transfer encoding
can leave lookup and serialization inconsistent, and writer setup can change
the boundary even when charset setup then fails. A valid-boundary control
stays unchanged. Six synthetic cases, each body capped at 64 KiB, passed
through the host-safe runner as a reproduction (`/tmp/emlkit-multipart-header-audit.go`
and `.out`, service `emlkit-bounded-91f818714b`). Issue #60 was filed before
implementation. This registration does not implement the fix, reopen the
malformed-Content-Type panic fix, complete raw replay, or claim concurrent
caller mutation is supported. Review must establish header ownership without
making the generated entity's boundary disagree with its body. The issue is
separate from the reviewed line-ending work, and remains Planned.


`ROB-120-008` ([issue #58](https://github.com/saeroun-ai/emlkit/issues/58))
closes a writer defect that predates the robustness work (QA baseline
`76fd211`). `createWriter` discarded the `Header.ContentType` error. A
lowercase malformed multipart, such as `multipart/mixed; boundary=B; broken`,
then wrote a generated boundary into the nil parameter map and panicked. An
uppercase one, or a malformed `text/plain`, was written as whatever the raw
string's prefix suggested. The QA matrix of 20 cases (five values × four paths)
found 12 panics and 6 silent successes. The contract is:

- **Refusal, not repair.** Every generation and serialization path —
  `CreateWriter`, `Writer.CreatePart`/`CreatePartWithBuffer`, `Entity.WriteTo`
  and `NewMultipart`'s lazily generated `Body` — refuses a `Content-Type`
  that does not parse. The error names the field and its value and wraps the
  parse error. Nothing is guessed: no media type is read off the raw string,
  and no parameter map or boundary is invented. A missing `Content-Type` is
  still text/plain. The well-formed paths are unchanged: boundary generation
  for a missing or invalid boundary in the written header, the utf-8/us-ascii
  charset policy, transfer encoding, and copy-on-write of the caller's header.
- **Before output.** `CreateWriter` refuses before writing anything.
  `CreatePart`/`CreatePartWithBuffer` refuse before that part's delimiter,
  header or buffer use, and leave the writer as it was: the parts already
  written stand, a later part may still be created, and a refused first part
  does not commit `Close` to writing a closing delimiter. Undoing output already
  written is not promised.
- **`NewMultipart`.** Construction is unchanged. The refusal is reported when
  `Body` is read or the entity is written. `multipartBody.Read` used to publish
  its pipe before creating the writer, so after a refusal a second read blocked
  for ever. The pipe is now published only once the writer exists, and a setup
  failure is sticky: every read returns the same error. A part the writer
  refuses fails the body after the parts before it; every later read reports
  that error, and the worker goroutine exits. Only a refusal of the entity's
  own header produces no output. `Body` carries the parts alone, never the
  entity's own header, so for a refused part it has already yielded the bytes
  of the parts before it. `WriteTo` has by then written the entity's header as
  well as those parts to its destination. Those bytes are not withdrawn.
- **Parsed entities.** `Parse` still repairs a damaged `Content-Type` for
  interpretation (`ROB-050-001`) and still keeps the raw header byte for byte.
  Such an entity, and one from `Read` with the same header, is refused by
  `WriteTo` before any output, because the refused header is its own. That is
  a documented limitation, not raw
  replay; replay, especially of a consumed body, is #41's.
- **Not in scope.** Media types `mime.ParseMediaType` accepts that RFC 2045
  would not (a lone token), header injection (`ROB-120-003`), disposition
  canonicalization, writer transactionality, and other writer lifecycle
  issues, such as a worker blocked by a reader that stops early without
  `Close`.
- **Evidence.** `writer_malformed_test.go`:
  - the five-value matrix through `CreateWriter`, `CreatePart` and
    `CreatePartWithBuffer` (no panic, no output, caller header and buffer
    intact, the part before and a good part after kept, no closing delimiter
    after a refused first part);
  - `Parse`→`WriteTo` and `Read`→`WriteTo` (refusal with no output, header
    unchanged);
  - a `NewMultipart` with a malformed header: two reads under a deadline
    return the same error, and no worker is started (this test blocks against
    the previous `multipart.go`);
  - the writer setup failure that predates this item, on the same path: a
    well-formed `text/plain; charset=unavailable` passed to `NewMultipart`
    yields a usable entity with the unknown-charset warning, and three reads of
    its body each return the unhandled-charset refusal (this also blocks
    against the previous `multipart.go`);
  - a `NewMultipart` with a malformed child between good ones: the part
    before it is delivered, the same error on repeat, and the worker exits;
  - unchanged absent-type, generated-boundary, uppercase well-formed and
    charset behavior.

  Reads run on a helper goroutine under a 5-second deadline, and a panic on
  that goroutine is recovered and reported as a test failure. Worker exit is
  observed by counting goroutines that hold the worker function's own stack
  frame, not the global goroutine count, and a companion test checks that a
  live worker is counted. A panic on the worker goroutine itself cannot be
  recovered by a test; it aborts the test binary, which still fails the run.
  The QA programs `/tmp/emlkit-writer-paths-qa.go` (`cases=20 failures=0`) and
  `/tmp/emlkit-writer-repeat-qa.go` (`repeat_read_failures=0`) pass.

`ROB-120-007` owns two parser-wide issues found while reviewing `ROB-001-002`,
where they were fixed for the header byte budget alone and left standing
everywhere else.

An error the parser classifies may have come from the caller's `io.Reader`, so
its shape, its methods and its graph are input. Two consequences were
demonstrated:

- `errors.Is` and `errors.As` ask the error under test whether it matches. An
  error whose `Is(error) bool` or `As(any) bool` returns true for everything
  claims every identity, including identities it could not otherwise obtain, and
  every predicate built on those functions believes it. `IsFatal`,
  `IsUnknownCharset` and `IsUnknownEncoding` are such predicates, as is the
  EOF-class classification in `textproto.MultipartReader.NextPart`. A concrete
  consequence: a `(nil, err)` from `NextPart` whose `err` claims to be an
  unknown-encoding warning reached `Entity.Walk`, which then dereferenced the nil
  part. The review of `ROB-001-004` made `Walk` forward a warning only together
  with a part, which closes that dereference; the misclassification itself, and
  every other predicate, remain this item's.
- Neither `errors.Is` nor `errors.As` bounds its walk. An error that unwraps to
  itself, or a ring of them, does not terminate; `errors.Join` and
  `Unwrap() []error` widen the same graph.

The item must give every input-facing classification the properties
`textproto.HeaderLimit` now has: match by exact dynamic type and by provenance
the parser alone can set, never invoke a custom `Is` or `As`, bound both nodes
examined and children held pending, follow `Unwrap() error` and
`Unwrap() []error` alike, and skip nil and typed-nil nodes before calling their
methods. Predicates that are documented as `errors.As` semantics and must stay
that way for compatibility should say so, and the parser's own decisions should
stop depending on them.

A third consequence was found while the item was being designed, and is what
moved it forward (see the ordering exception below): a source that fails inside
a part's header block with an error that unwraps to itself made the child's
`NextPart` hang in `textproto`'s `errors.Is`, after `Parse` had succeeded. It
reproduces on `b994934` and on `b5db93f`. A `CharsetReader` failing with such
an error hung `Entity.Walk` (in `IsUnknownEncoding`) and `mail.Reader` (in
`IsFatal`) the same way, because both asked the predicates about a warning
whose cause they then walked without a bound.

**Contract.** The item was implemented as follows.

- One traversal, `internal/errgraph.Find`, used by every classification in the
  module and by no public API of its own. It walks breadth first over
  `Unwrap() error` and `Unwrap() []error` from the error given. It never calls
  `Error`, `Is`, `As` or `Format`; it never asks a nil or typed-nil value to
  unwrap itself; it treats an `Unwrap` that panics — `panic(nil)` included — as
  having no children; and it hashes and compares nothing, so a value of a
  non-comparable type cannot make it panic. Its match callbacks are the
  module's own: an exact type assertion, a field only the module can set, or
  `==` against a pointer-typed target.
- Fixed bounds: at most 256 errors examined, which is what ends a cycle, a ring
  or a diamond; at most 64 held pending, in a fixed array; and at most 256 child
  slots inspected across the walk, one per `Unwrap() error` call and one per
  element of an `Unwrap() []error` result, nil and typed-nil elements included,
  so a join whose children are a long run of nils is not scanned to its end.
  A match deeper or wider than these is reported as absent (a false negative).
  Nothing the parser builds comes near them. The numbers are not a
  compatibility promise.
- **Public predicates.** `IsFatal`, `IsUnknownEncoding` and `IsUnknownCharset`
  use that walk. `IsFatal` matches only a non-nil `*fatalParseError`, the
  parser's own concrete type; it no longer accepts any value that satisfies
  `FatalError`, since a struct in another package embedding a `FatalError`
  field inherits the unexported method — a nil one included. The two warning
  predicates match the exact value types, as `errors.As` did, but only with a
  non-nil cause: every warning the parser reports carries one, and the zero
  `UnknownEncodingError{}` / `UnknownCharsetError{}` describe nothing. The
  cause is unexported, so a composite literal in another package can only build
  that zero value; a genuine warning the parser returned can still be copied
  and handed on like any value, and is still matched. A pointer to either type
  is not itself a match, as before; it is unwrapped like any wrapper, unless it
  is nil. Being graph searches, these answer whether such a value is anywhere
  in an error, which is not the parser's own warning decision below: a warning
  whose cause holds a genuine fatal error satisfies both `IsUnknownCharset` and
  `IsFatal`.
- **Parser decisions.** They decide by top-level type, reader-local identity or
  session identity, and do not unwrap what a caller's component supplied, with
  one deliberate exception: the EOF class of `textproto.MultipartReader.NextPart`
  follows a source error's own `Unwrap` methods through the bounded walk, to
  keep recognizing a wrapped `io.EOF`. That is the only foreign method it calls;
  the walk bounds the number of calls, not what one call does. Nothing here
  promises that no foreign method runs anywhere else — a decoder or a
  `CharsetReader` may do what it likes with an error it is given.
  `textproto.MultipartReader.NextPart` recognizes its own header and
  line refusals by identity with the refusal that reader created and returned
  (an unexported field), so a refusal an input hands back — forged, or genuine
  but made by another reader — is an ordinary failure, also at the framing line
  where type and mark used to decide. Its EOF class is the bounded walk with an
  identity match against `io.EOF` and `io.ErrUnexpectedEOF`. The `message`
  multipart iterator and the root header read ask `textproto.HeaderLimit` and
  `LineLimit` only after a top-level type assertion, because textproto returns
  its refusals as themselves; given a value of their own type those predicates
  call no method but that type's. `Entity.Walk` and `mail.Reader` accept a
  warning only when the error is itself one of the two warning types and comes
  with an entity; `mail` then confirms the cause with the public predicate on
  that value alone, which matches a genuine warning before it would unwrap
  anything. A warning is recoverable whatever its cause holds: a
  `CharsetReader` failing with a `FatalError` from another parse leaves the part,
  its raw body and the siblings after it usable, because only this parse's own
  stop ends the traversal. The
  existing identities are unchanged: the shared header budget's refusal
  (`HeaderBudget.Refusal`, pointer comparison), the session's published stop,
  and the body guard's input-limiter failure (`inputStop`, at most one standard
  join per level), with their existing precedence.

**Compatibility.**

- The three public predicates no longer call `As` methods, no longer follow
  `Unwrap` without a bound, and no longer accept an embedded or nil
  `FatalError`, a zero warning, or a match past the bounds. They still find a
  genuine value through `fmt.Errorf` with `%w` (one or several), `errors.Join`
  and custom `Unwrap` wrappers, and inside a `FatalError`. An error type that
  exposes one of these values only through an `As` method is no longer
  recognized.
- `textproto.MultipartReader.NextPart` no longer consults `Is` methods for its
  EOF class: an error that claims to be `io.EOF` only through `Is` no longer
  ends the iteration, while `io.EOF` and `io.ErrUnexpectedEOF` wrapped by `%w`,
  joins or custom `Unwrap` still do. A bare `io.EOF` inside a header block
  still ends that block, as before. A genuine refusal made elsewhere and handed
  back by the source no longer ends the iteration.
- `mail.Reader` (`NextPart`, `CreateReader`) used to reject a warning for which
  `IsFatal` was true, so a `CharsetReader` failing with a genuine `FatalError`
  from another parse ended the reader with no part. It now returns that part
  with its warning and continues, as `Entity.Walk` already did; a stop of the
  reader's own parse is still returned in place of the warning and still ends
  it.
- `FatalError`'s documentation no longer claims that satisfying the interface
  proves origin. It restricts the type assertion example to an error a parse
  API returned directly, and `errors.As` to chains whose every wrapper is
  trusted: `IsFatal` returning true does not make `errors.As` safe on the same
  error, since `errors.As` still calls every `As` method and follows `Unwrap`
  without a bound on its own way through the graph.
- No public API was added. `internal/errgraph` is internal to the module.

**Limits that remain.**

- A walk bounds how many `Unwrap` calls it makes, not what one call does. An
  `Unwrap` that blocks, spins, allocates without end, calls `runtime.Goexit`
  or exhausts the stack is the caller's code on the caller's goroutine, and no
  in-process walk can interrupt it. Neither is anything claimed for `Error` or
  `fmt` of such an error, for a caller's own `errors.Is`/`errors.As`, or for
  what a `CharsetReader` or decoder does with an error it is given.
- Replay. A genuine value presented again somewhere it did not come from is the
  parser's value, and nothing in the type system tells a value from itself.
  `IsFatal` reports a replayed fatal error as fatal; `textproto.HeaderLimit`
  and `LineLimit` report a replayed refusal as genuine; and the `message`
  multipart iterator and root header read, which see a refusal only through
  `textproto`'s public API, diagnose a genuine refusal the source fails with
  at the top level as a budget refusal of their own. Only `textproto`'s own
  iterator decision is exact by identity.
- The message's bytes cannot produce Go methods. Every hostile error here is a
  caller's component — its `io.Reader`, `CharsetReader` or `DiagnosticSink` —
  failing with a value of a type its own code declares.

**Acceptance evidence.** `internal/errgraph/errgraph_test.go` (genuine through
wrapping and joins; nil and typed nil; fused self, join and ring cycles; a
2^30-path diamond; the node, pending and slot bounds at their edges, including
a run of 65,536 nils and the shared charge of `Unwrap() error`; panicking
single and multi `Unwrap` with `panic(nil)`; typed nils never unwrapped;
`Is`/`As`/`Error` never called; non-comparable nodes);
`textproto/multipart_errgraph_test.go` (wrapped `io.EOF` and
`io.ErrUnexpectedEOF` still end the iteration at a framing line and in a part
header; an `Is` liar, cycles, panics, typed nils and an EOF past the bounds do
not, and do not hang or panic; own header and line refusals end it; replayed,
forged, typed-nil and joined refusals do not, with and without budgets);
`errgraph_test.go` (exact provenance of the three predicates, external
`FatalError` embedding, zero warnings, hostile graphs for every predicate,
source failures at the root and at a child after an intact sibling through
`NextPart` and `Walk`, a hostile `CharsetReader` keeping the part, its raw
body, its warning and the sibling after it, a hostile sink's stop returned as
the identical session stop, a `CharsetReader` failing with a genuine fatal
error from another parse — a warning `IsFatal` also matches — keeping the
part, its raw body and its siblings at the root, through the iterator and
through `Walk`, and an independent subprocess probe with cycles that never
end, under a 30 s deadline and a 16 MiB stack cap);
`mail/reader_errgraph_test.go` (the same source, `CharsetReader` and sink
cases through `mail.Reader`, and the genuine-fatal cause through
`CreateReader` and `NewReader`); `TestParse_totalHeaders_foreignRefusal`, whose
child cycle case is no longer skipped; and the existing `HeaderLimit` and
`LineLimit` hostile-graph tests in `textproto`, whose self cycles and rings are
now fused fixtures with a blown-fuse assertion. Every in-process cycle is fused
at 10,000 `Unwrap` calls without allocating, so a regression fails instead of
spinning; cycles that never end run only in the subprocess probes.

### ROB-130 — Corpus, fuzzing, differential testing, and performance

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-130-001 | Planned | Add native Go fuzz targets for raw headers, MIME parameters, multipart, Base64, QP, charset, and full message walk. | Seed corpus runs without panic/hang. |
| ROB-130-002 | Planned | Define fuzz invariants: progress, chunk invariance, bounded resources, stable diagnostics, raw preservation. | Automated property assertions. |
| ROB-130-003 | Planned | Maintain sanitized conformant, malformed, truncated, adversarial, and provider-generated corpora. | Fixture manifest with provenance and expected diagnostics. |
| ROB-130-004 | Planned | Build differential harnesses for Angus, Python `email`, MimeKit, and optionally Mime4j. | Machine-readable result comparison. |
| ROB-130-005 | Planned | Record intentional parser differences with rationale instead of forcing equality. | Reviewed compatibility exceptions file. |
| ROB-130-006 | Planned | Add parser/decoder benchmarks for small mail, huge attachments, deep/wide MIME, and near-boundary attacks. | Baselines and regression thresholds. |
| ROB-130-007 | Planned | Add long-running randomized soak tests with leak and memory ceilings. | Repeatable seeded job. |
| ROB-130-008 | Planned | Run `go test`, race tests, fuzz smoke tests, formatting, vet/static analysis, and benchmarks in CI. | Required green checks. |
| ROB-130-009 | Done | Stabilize comment-parser cumulative-allocation regression checks without weakening their ability to detect allocation growth ([issue #59](https://github.com/saeroun-ai/emlkit/issues/59)). | Bounded repeated checks, finite deliberate-allocation positive controls, original accessor results and scanner no-allocation assertions, full/race/386 verification; no threshold-only relaxation or retry-until-pass. |
| ROB-130-010 | Done | Stabilize the shared-header accepted-path allocation regression check under race without hiding per-header allocation growth; see issue #61 and the section below. | Finite baseline/current reproduction, checked parse results, bounded positive allocation controls, repeatability and full unit/race/386/build review. |

`ROB-130-009` changes test measurement only, in `mail/header_comment_test.go`.
`allocBytesPerOp` replaces the single-call `TotalAlloc` reading with one
reading around a fixed 32 back-to-back calls, at `GOMAXPROCS` 1 (restored by
`defer`), divided by 32 and rounded up — the averaging `testing.AllocsPerRun`
uses for counts, without its warm-up call. The result is the mean cumulative
bytes per accessor call, not each call's own cost: a cost every call pays,
such as a copy of the comment, is in the mean at full size, while one paid by
only some calls is diluted by their share (a once-only cost at 1/32). Foreign
allocations can only add to the process-wide counter, so the mean never falls
below the calls' own mean; bytes from elsewhere are spread over 32 calls
instead of landing on one. This lowers the odds of a noisy failure; repeated
passes do not prove the window free of foreign bytes. The 4 KiB budget, the
4–32 KiB sizes, the linear-growth rule and the accessor results are unchanged.
Every measured call's error and result is now checked, not only the last
one's: each call compares against expected values built before the
measurement, without allocating, and the first failing call is reported
afterwards (the trailing `Message-Id` result and the address counts of the
linear-growth test are newly checked). The constant-budget cases and the
32 KiB issue #9 reproduction each measure a 1 KiB comment first on the same
budget, and the first size over budget stops the case, so a reintroduced
quadratic copy costs about 16 MiB per case before failing instead of being
replayed 32 times at 32 KiB (about 16 GiB); the 32 KiB measurements still run
when the preflight passes. `TestAllocBytesPerOp_controls` runs both gates
against known allocations: a per-call payload one byte over budget and the
pre-#9 `comment += byte` loop on a 256-byte comment must fail the budget,
1 KiB per call (over budget only when summed) and no allocation must pass, and
the pre-#9 loop at 64–512 bytes must fail the growth rule while 64 bytes per
input byte must pass. The pre-#9 replica refuses, by panicking before copying,
any input over 1,024 bytes including its closing delimiter and suffix, and a
control checks that refusal with a just-over-cap input.
`TestSkipComment_noAllocation` and the scanner are untouched. The source of the
extra bytes in issue #59 is still not established; this bounds its effect, it
does not explain it.

**Codex review and verification for ROB-130-009.** Review required the
hard-capped positive-control replica, the independent 1 KiB gate before the
32 KiB reproduction, and checking every measured result/error. The final change
is confined to test code and this ledger. All ten bounded verification steps
passed: full uncached tests/race/GOARCH=386 across all six packages, build,
filtered vet (the existing composites/stdmethods warnings remain), formatting
and whitespace; the allocation/linear-growth/control tests repeated 100 times,
and all mail tests repeated 25 times. The internal scanner's no-allocation
assertion remains covered. Independent measurements checked 64 accessor/shape/
size cases: MessageID 16 bytes/call, References 80, List-Unsubscribe 184 across
4–32 KiB comments; a modest 16 KiB allocation repeated 64 times measured
1,048,576 bytes and was detected. The committed controls additionally verify
budget+1 and bounded quadratic growth, and reject an over-cap replica input.

Evidence: `/tmp/emlkit-allocation-verification.json` and `.out`, per-step logs,
plus the preserved pre-fix current/baseline failures in
`/tmp/emlkit-empty-allocation-investigation.out`. Service
`emlkit-bounded-5579c678a5` enforced 1,536 MiB RAM, no swap, two CPU cores,
128 tasks and a deadline. Its memory peak reached the enforced limit; observed
`memory.events` reported max=134 and oom=0/oom_kill=0/oom_group_kill=0 during
race, and the whole job exited successfully. All jobs ran sequentially. Both
Claude calls had empty permission-denial arrays. This is empirical stability
under the tested conditions, not proof that process-wide measurement noise or
one-time-cost dilution cannot occur.

#### ROB-130-010 — shared-header allocation measurement (Done)

Registered before implementation as [issue #61](https://github.com/saeroun-ai/emlkit/issues/61).
During ROB-010-004 final QA, the existing
`TestParse_totalHeaders_acceptedPathAllocations` failed under race: 815
allocations with the shared budgets, 812 without, against an allowed difference
of two. A finite 100-run comparison failed 19 times on the mbox working tree
and 23 times on an isolated pre-mbox `2defe99` archive. The same baseline's full
race suite failed this test too (814 versus 811). This confirms an existing
unstable measurement, not a demonstrated mbox parser regression.

The test compares two `testing.AllocsPerRun(20)` measurements of a 50-part
parse. Its traversal closes parts through `io.Copy(io.Discard, part)`. The
installed Go implementation uses a `sync.Pool` there, whose race-mode Put may
randomly discard entries; this is a source-level lead to verify, not a measured
attribution of all extra allocations. Unlike ROB-130-009 / #59, this concerns
the shared-header accepted path, not mail comment TotalAlloc measurements.

Keep the regression's purpose: detect added allocation per header block,
verify the actual parse and traversal results, and validate the measurement
with a finite positive control. Do not merely increase the threshold, ignore
a failure or retry until it passes. No product behavior changes are required
by this item. Use the host-safe runner, finite work/byte caps and one job at a
time. Evidence: `/tmp/emlkit-mbox-allocation-investigation.json` and `.out`,
current/baseline repeat and full-race logs; the original failure is retained in
`/tmp/emlkit-mbox-final-platform-race.out`.

**Design, independently reviewed.** The change is limited to test code
in `limit_header_total_test.go` and to this section. No product code, budget,
default, mbox file or other ledger item changes. The single 50-part
`AllocsPerRun(20)` comparison (with − without ≤ 2) is replaced by two
quantities, each measured where its noise is small:

- *constant*: the totals' mean extra allocations per parse of a one-part
  fixture, bound `totalAllocMaxConstant` = 2. This is the shared budget plus
  one spare, the bound the test always had. From the source, the expected
  value is one: `session.headerBudget` creates one `HeaderBudget`, and
  `textproto`'s `TestHeaderBudget_acceptedChargeAllocatesNothing` pins zero
  allocations per accepted charge and per block.
- *growth*: how much that extra grows from 1 to 50 parts (49 more header
  blocks, 98 more fields), bound `totalAllocMaxGrowth` = 2. One allocation per
  block or field grows it by 49 or 98. One allocation per eighth block grows it
  by 6, and an amortized slice gaining an entry per block grows it by about 5.

The fixture is a root container plus parts of two fields (`X: 1`, `Y: 2`) and
body `b`. `totalAllocMean` is `AllocsPerRun`'s procedure: GOMAXPROCS 1, one
warm-up call, then `MemStats.Mallocs` around a fixed number of calls. It adds
a `runtime.GC()` first and does not truncate the mean to an integer;
truncating two means before subtracting can shift the difference by one, which
is the bound's margin. Fixed work: 200 measured parses at 1 part and 500 at 50
parts, each after one warm-up and one preflight parse. The byte metric of
`ROB-130-009` was not copied: a pool miss allocates an 8 KiB buffer, so bytes
are far noisier here than counts.

The cause is a source-level account, not a measured attribution. Every
`NextPart` closes the previous part via `io.Copy(io.Discard, part)`
(`textproto/multipart.go`), `io.Discard.ReadFrom` takes its buffer from
`blackHolePool`, and under `-race` `sync.Pool.Put` drops one value in four.
Each drop costs the next close two allocations (`make([]byte, 8192)` and its
escaping slice header). A Python model of this alone — two allocations with
probability 1/4 per close, the totals adding one, `AllocsPerRun` truncation —
predicts the old check failing in 22.1% of runs, with failing differences of
3–5. That matches 19/100 and 23/100 in #61. The same model with a constant of
2 predicts 40%, and with a second such pool 30%. Under this model, the
standard deviation of the new constant is about 0.09 and of the growth about
0.4 allocations, so the bounds sit about 11 and 5 deviations from the expected
values. Without `-race`, Put drops nothing at random, so the measurements are
expected to land at their nominal increments. That is not a guarantee of exact
counts. A collection in the window still clears the pool, a pool can still
miss, and the runtime or other code in the process can still allocate, and
`Mallocs` counts all of it. The model explains the chosen run counts; the
bounds were not derived from it.

Parent's initial bounded QA (`/tmp/emlkit-totalheader-alloc-initial.json` and
`.out`) included an `io.Discard`-only control, 50 calls per run over 50 pairs.
In normal mode allocations were 0..0 and the largest same-work difference was 0.
Under `-race` they were 22..29, with a largest difference of 4 and 8 of 50
pairs over 2. This is consistent with the pool account, but it is not an
attribution of the parser's own variation. The main and control tests passed
once in normal mode and once under `-race` there; repeated and full QA are
still pending.

Every measured parse, plus its warm-up and preflight, is checked without
allocating inside the window. `totalAllocRun` records into
`totalAllocRecord`, and `check` fails afterwards on any of the following:

- a `Parse` error or nil entity, or a wrong root header or a non-multipart root;
- a `NextPart` error, or any part count other than exactly the fixture's;
- a wrong field count or value in a part;
- a body that is not exactly `b`, or a body read error;
- any retained diagnostic, or a `ParseStop`;
- a shared budget that is missing (totals on) or present (totals off).

A failed or partial parse cannot pass by allocating less.

Hard caps independent of the parser:

- input is fixed (at most about 1.2 KiB);
- the traversal asks for at most parts+1 parts and makes at most 8 reads per
  body, into an 8-byte stack buffer;
- one preflight parse at each size must stay within 16,384 allocations and
  4 MiB before it is repeated;
- control allocations are one 32-byte object each, with at most one per part
  plus three per parse.

No guard is disabled, and no old unbounded or quadratic function is restored.

`TestParse_totalHeaders_acceptedPathAllocationControls` runs the same
measurement and verdict against the totals-off configuration, comparing it to
a separately measured totals-off baseline, so each expected difference is
the nominal increment the control adds. The measurement is expected to come
close to it, not to reproduce it exactly, in either mode:

| Control | Added (constant, growth) | Constant check | Growth check |
|---|---|---|---|
| unchanged | (0, 0) | pass | pass |
| one per parse | (1, 0) | pass | pass |
| three per parse | (3, 0) | fail | pass |
| one per header block | (1, 49) | pass | fail |
| one per eighth block | (0, 6) | pass | fail |

Each control is at least one allocation from each bound.

The allocations are made by the traversal, not inside the parser. The
process-wide count cannot tell the two apart, so the controls prove the
measurement and verdict can detect such allocations, not where a regression
would be.

Limits of what the check catches:

- growth of at most two allocations in total over 49 blocks is not resolved;
- a constant of up to two allocations is tolerated;
- `Mallocs` is process-wide, so any foreign allocation in a window (another
  goroutine, a runtime collection) is charged to that configuration, diluted
  by the run count. In a totals-on window it can cause a false failure; in a
  totals-off window it can hide up to that amount;
- the tests must not run in parallel (none does).

Work is about 4,000 parses of 50 parts in total, or about 200,000 part
parses, versus about 2,100 before. Scaling #61's 0.02–0.03 s, expect about 2–4
s per run of both tests under `-race`. This is an estimate, not measured.

**Independent review and verification completed.** Codex reviewed the final
measurement, checked every control's margin and the parse-result checks, and
requested qualified normal-mode claims and post-measurement `-v` logging.
Both Claude result files report no permission denials. No parser code changed.

The final fixed-count normal and race runs each executed both accepted-path
tests 100 times with zero failures (34.239 s and 253.942 s respectively),
without retrying a failure. A separate verbose run measured the totals'
constant/growth at 0.990/0.010 normally and 0.960/0.018 under race. The five
controls all produced their required verdicts, including three per parse,
one per block and one per eighth block. Fractional normal-mode variation was
visible too; the count is not promised exact.

All 13 steps in `/tmp/emlkit-totalheader-mbox-final-qa.json` and `.out` passed:
verbose measurements and both fixed repetitions; all six packages' full unit,
race and 386 tests; build; vet excluding the two established warning classes;
previous empty/header-only and exceptional-input QA; formatting and diff.
Fresh unfiltered vet still reports exactly the prior 13 unkeyed literals and
one WriteTo-signature warning, with no other output
(`/tmp/emlkit-totalheader-mbox-plain-vet.json` and `.out`). It is not clean vet.

The final sequential service, `emlkit-bounded-9e739c7782`, finished normally
in 341.287 s with a reported 1.2 GiB peak, inside the enforced 1.5 GiB RAM,
zero swap, CPU quota 2, 128 tasks and deadline. No guard was disabled. This
completes #61's local implementation/review/QA, not CI or the general
performance audit. Original failing runs remain available at the paths above.


### ROB-140 — Optional interoperability extensions

These do not block the core robustness target but are tracked to avoid treating
common provider artifacts as unknown forever.

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-140-001 | Planned | Optional TNEF (`winmail.dat`) extraction module with strict budgets. | Outlook fixtures and no core dependency. |
| ROB-140-002 | Planned | Optional AppleDouble metadata/resource-fork handling. | Apple Mail fixtures. |
| ROB-140-003 | Planned | Optional iCalendar convenience parser with raw fallback. | Invite/update/cancel fixtures. |
| ROB-140-004 | Planned | Optional S/MIME structure/decrypt/verify integration. | Cryptographic test vectors. |
| ROB-140-005 | Planned | Optional PGP/MIME structure/decrypt/verify integration. | Cryptographic test vectors. |
| ROB-140-006 | Planned | Evaluate BinHex and yEnc only from measured corpus demand. | Design decision and fixtures before implementation. |

## 8. Execution order

Items are implemented in this dependency order:

1. `ROB-000`: diagnostics and parse contract.
2. `ROB-001`: budgets, cancellation, and progress.
3. `ROB-010` and `ROB-020`: input/header framing.
4. `ROB-050`: MIME metadata, needed before correct multipart classification.
5. `ROB-060`: multipart recovery.
6. `ROB-070` and `ROB-080`: transfer and charset decoding.
7. `ROB-030` and `ROB-040`: encoded and structured headers.
8. `ROB-090`: nested and special MIME.
9. `ROB-110`: replay/spooling once spans and diagnostics are stable.
10. `ROB-100`: convenience body/attachment view on top of the complete tree.
11. `ROB-120` and `ROB-130` run throughout; final audit closes them.
12. `ROB-140` is demand-driven after the core target.

Within a milestone, use ascending item order unless a reviewed dependency note
is added to this document.

**Reviewed next-item dependency — `ROB-010-006`.** After finishing
`ROB-001-007`, audit ordinary data-plus-error propagation before the other
`ROB-010` framing items. The progress investigation found header peeks that
consume an error and then read again, and decoder paths may rewrite ordinary
source errors. Establishing the byte/error contract first gives the upcoming
EOF, header-only and truncated-input recovery work a reliable basis. This is
the remaining item of #7, handled in its own execution and review; no partial
header healing, completion-state API or other framing item is completed by
this ordering decision.

**Reviewed dependency note — `ROB-010-001` and #17.** Issue #19 references
[#17](https://github.com/saeroun-ai/emlkit/issues/17) (activating profiles and
compatibility policies). This item needs no new profile producer: empty and
header-only input is accepted by the existing common path, every `Profile` and
`Compatibility` value presently frames it the same (metadata producers keep
their own documented profile behavior), and its round-trip
contract is the existing serializer's. It documents and pins that; it does not
claim #17 or any part of it is finished.

**Reviewed validation dependency — `ROB-130-009`.** After `ROB-010-001`,
address the existing intermittent allocation-regression test before further
framing implementation. Codex reproduced it on both the current test-only
change and the older `92eaec1` baseline; evidence and acceptance criteria are
in [issue #59](https://github.com/saeroun-ai/emlkit/issues/59). Reliable
regression measurements are needed for the following implementation reviews.
This changes the test measurement only; it does not reopen the fixed comment
scanner, complete CI (#14), or complete the performance audit (#18).

**Reviewed validation dependency — `ROB-130-010`.** ROB-010-004 stayed in
Review while the pre-existing shared-header allocation test's race instability
was addressed under #61. Codex reproduced it on both the working tree and the
pre-mbox `2defe99` baseline. The independent test-measurement fix and final
ROB-010-004 verification are now complete; the next framing item is
ROB-010-005. This did not alter parser budgets or complete CI/performance-audit
items #14/#18.

**Reviewed dependency note — `ROB-010-005` and #30/#41/#10.** ROB-010-005
needs exactly one piece of what `ROB-060-001` (#30) will generalize: a way to
read the input after a proven closing delimiter without losing what the
multipart reader buffered. It adds that minimal shared foundation,
`textproto.(*MultipartReader).Epilogue`, and uses it only for the message's
own closing delimiter under `ProfileForensic`, read only when the caller asks.
It does not preserve or budget nested epilogues or preambles, enforce
`MaxPreambleBytes`/`MaxEpilogueBytes`, retain or replay raw bytes (#41,
`ROB-110`), or define message completion (#10); those items stay Planned and
may build on the primitive.

**Reviewed dependency note — `ROB-020-001` and #17, `ROB-020-005`.** Issue #20
depends on the profile and compatibility engine of
[#17](https://github.com/saeroun-ai/emlkit/issues/17), which stays open. This
item documents one narrow producer policy only: every `Compatibility` value
keeps the malformed prefix raw and uninterpreted, Tolerant and Forensic record
it, Strict refuses the entity locally. It needs, and adds, one prefix-specific
piece of missing-separator recovery — a part header that began with a prefix
ends before its multipart's delimiter line — so that healing the first line
cannot consume a delimiter that the earlier error-and-retry path recovered.
General separator recovery and its diagnostics for other blocks remain
`ROB-020-005`'s, which stays Planned.

**Exception — `ROB-100-006`.** It was taken ahead of `ROB-010` through `ROB-090`
at the user's request, as an independent compatibility fix
([issue #1](https://github.com/saeroun-ai/emlkit/issues/1)): it changes only how
`mail.Reader` classifies the two decoding warnings the `message` layer already
returns, and depends on nothing those milestones introduce. It does not advance,
start or complete any other `ROB-100` item or any earlier milestone.

**Exception — `ROB-080-011`.** It was taken ahead of `ROB-010` through `ROB-070`
and of `ROB-080-001` at the user's request, as a scoped compatibility fix
([issue #1](https://github.com/saeroun-ai/emlkit/issues/1)): two label entries
in the optional charset plugin's existing quirks table, depending on nothing
those items introduce. It does not advance, start or complete `ROB-080-001`,
`ROB-080-005` or any other item.

**Exception — `ROB-040-003`.** It was taken ahead of `ROB-010` through
`ROB-080`, of `ROB-030`, and of `ROB-040-001` and `ROB-040-002`, at the user's
request, as an independent resource fix
([issue #9](https://github.com/saeroun-ai/emlkit/issues/9)): it replaces the
comment scanner shared by the msg-id and list-command accessors with an
allocation-free pass that returns identical results, and depends on nothing
those items introduce. It does not advance, start or complete `ROB-040-001`,
`ROB-040-004`, `ROB-040-007`, `ROB-040-009`, `ROB-130` or any other item.

**Exception — `ROB-100-007`.** It was added to the ledger and taken ahead of
`ROB-010` through `ROB-090` and of `ROB-100-001` through `ROB-100-005`, at the
user's request, as an independent fix of an existing cross-layer inconsistency
([issue #15](https://github.com/saeroun-ai/emlkit/issues/15)). It makes
`mail.Reader` use the metadata the existing engine already interpreted, and
respect that engine's existing session stop. It depends on no tree model,
malformed-MIME recovery (#22), filename policy or error-graph work that is not
yet built. It does not advance, start or complete `ROB-050`, `ROB-100-001`
through `ROB-100-005`, `ROB-120-007` or any other item.

**Exception — `ROB-050-001`.** It was taken ahead of `ROB-010` through
`ROB-020` at the user's request, after `ROB-100-007` was Done, as the first
item of [issue #22](https://github.com/saeroun-ai/emlkit/issues/22). It
depends on nothing those items introduce: the field value, the quote-aware
parameter scan and the budgets already exist. Issue #22 lists
[#17](https://github.com/saeroun-ai/emlkit/issues/17) (activating profiles and
compatibility policies) as a prerequisite. That was reviewed as a dependency
exception: this item implements and documents the profile decision table for
its own producer only — Tolerant and Forensic repair alike, Strict refuses
locally, every Compatibility value takes the same conservative choices — and
does not claim #17, any other producer's profile behavior, or parity with any
reference engine. It does not advance, start or complete `ROB-050-002`
through `ROB-050-010`, `ROB-060-002` or any other item.

**Exception — `ROB-120-008`.** It was added to the ledger before
implementation and taken ahead of `ROB-010` through `ROB-110` and of
`ROB-120-001` through `ROB-120-007`, at the user's request, after
`ROB-050-001` was reviewed, as an independent fix of an existing writer panic
([issue #58](https://github.com/saeroun-ai/emlkit/issues/58)). The defect
reproduces on the pre-#22 baseline `76fd211`. Its fix depends on nothing those
items introduce: it makes the writer stop ignoring an error it was already
given. It does not advance, start or complete raw replay (#41, `ROB-110`),
header injection (`ROB-120-003`), any other `ROB-120` item, or any other item.

**Exception — `ROB-001-009`.** It was added to the ledger before
implementation, at the user's request, after every earlier reviewed item, as
the input-byte budget of [issue #3](https://github.com/saeroun-ai/emlkit/issues/3),
and taken ahead of the still-Planned `ROB-001-005` through `ROB-001-008`. It is
a primitive enforcement at the raw source that depends on nothing those items
introduce: it neither bounds a body nor decoding (`ROB-001-005`, #4), cancels a
blocked read (`ROB-001-006`, #6), judges repeated no-progress reads
(`ROB-001-007`, #7), nor caps diagnostics (`ROB-001-008`). It does not advance,
start or complete any of them, and none of them depends on it beyond sharing
the published session stop that already exists.

**Exception — `ROB-001-008`.** It was taken after `ROB-001-009` was reviewed,
at the user's request, as the diagnostic cap of
[issue #8](https://github.com/saeroun-ai/emlkit/issues/8), ahead of the
still-Planned `ROB-001-005` through `ROB-001-007`. It bounds what a session
records and delivers and depends on nothing those items introduce: it neither
bounds a body nor decoding (`ROB-001-005`, #4), cancels a read (`ROB-001-006`,
#6), nor judges no-progress reads (`ROB-001-007`, #7). It does not advance,
start or complete any of them, the shared profile engine (#17), or raw replay
(#41).

**Exception — `ROB-001-010`.** Added before implementation for
[issue #5](https://github.com/saeroun-ai/emlkit/issues/5), after `ROB-001-005`
was reviewed and committed. It completes cumulative header accounting before
still-Planned `ROB-001-006`/`ROB-001-007` and `ROB-010` onward. It depends on
the existing input/session/header infrastructure, not on cancellation,
no-progress detection, general header recovery, embedded-message parsing or
raw replay. Those items are not advanced by this work. Future embedded-message
parsing must reuse the same session budget rather than resetting it.

**Exception — `ROB-120-007`.** Taken after `ROB-001-010` was Done, for
[issue #16](https://github.com/saeroun-ai/emlkit/issues/16), ahead of
`ROB-010` through `ROB-110` and of `ROB-120-001` through `ROB-120-006`,
because QA found a concrete hang, independently confirmed on `b994934` and
`b5db93f`: a source failing inside a child's header block with an error that
unwraps to itself made `NextPart` never return. It depends on nothing those
items introduce: it changes how existing errors are classified, not what is
parsed. It does not advance, start or complete cancellation of a blocked read
(`ROB-001-006`, #6), no-progress detection (`ROB-001-007`, #7), raw replay
(#41) or any other item.

## 9. Per-item implementation protocol

Each Claude Code execution receives exactly one ledger ID and must:

1. Read `CLAUDE.md`, this roadmap, the affected implementation, and related
   tests before editing.
2. Confirm the item does not require an unapproved public API break.
3. Add focused tests that demonstrate failure before or alongside the repair.
4. Make the smallest coherent implementation for only that item.
5. Preserve existing raw round-trip and streaming behavior.
6. Run focused tests, `go test ./...`, and `gofmt -l .`.
7. Move the assigned row from `Planned` to `Review`, never directly to `Done`.
8. Report changed files, semantic decisions, tests run, and remaining risks.
9. Avoid commits, pushes, destructive Git operations, dependency upgrades, and
   unrelated cleanup unless the item explicitly requires them.

The reviewing orchestrator then independently:

1. inspects the complete diff;
2. checks public API and compatibility risk;
3. runs focused and full verification;
4. adds missing adversarial tests if necessary;
5. marks the row `Done`, or returns the same item for correction.

No subsequent ledger item starts while the current one remains
`Implementing`, `Review`, or has unresolved review findings.

## 10. Global release gates

The parser may be described as “Angus-grade or better” only after:

- all non-optional `ROB-000` through `ROB-130` items are `Done`;
- every accepted recovery produces a stable diagnostic;
- the Angus differential corpus has no unexplained loss of recoverable parts or
  bytes;
- fuzzing has no known panic, hang, unbounded allocation, or chunk-dependent
  result;
- child headers and decoded output are protected by budgets;
- raw message and signed-content preservation are verified;
- large-message streaming/spooling tests stay within documented memory and disk
  ceilings;
- all intentional differences from reference parsers are reviewed and recorded.

Until those gates pass, documentation should say that `emlkit` is designed for
robust production parsing but should not claim complete parity with proprietary
large-provider parsers, whose behavior and corpora are not public.

Codex reviewed the `ROB-001-004` corrections for [issue #1](https://github.com/saeroun-ai/emlkit/issues/1), inspected the actual diff, and independently passed the refused-child, stopped-child, and session-stop precedence regression tests before marking the item Done.

Codex independently reviewed the `ROB-001-003` correction diff and regression fixtures for issue #1, reproduced the corrected 4095-byte framing boundary and Content-Disposition sink stop, and passed the full race suite, build, and formatting checks. The remaining vet analyzers also pass with the two pre-existing warning categories (`composites` and `stdmethods`) excluded; the ordinary vet command still reports those baseline findings. The item is Done.

Codex reviewed `ROB-100-006` against issue #1, verified the returned-object and fatal-precedence guards and their public-API regression tests, and independently confirmed root/child unknown-CTE preservation and the following sibling with the original reproduction program. Full tests, build, and formatting checks pass. The item is Done; callers must still handle a fatal result from Parse before constructing a mail reader.

Codex reviewed `ROB-080-011` for issue #1, independently checked the fixed CP949 fixture against Python, inspected both alias entries and the public body/header tests, and confirmed the original reproduction now decodes both labels correctly. The final full race/coverage suite, build, formatting, README example compilation, and remaining vet analyzers pass; ordinary vet retains only the documented baseline warnings. The item is Done.

Codex reviewed `ROB-040-003` for [issue #9](https://github.com/saeroun-ai/emlkit/issues/9), inspected the scanner and both test files, and independently measured 16 cumulative allocated bytes at each 4/8/16/32 KiB comment size. A separate 38,227-case public-API comparison retained the exact pre-change result digest. The full race suite, build, 386-target comment/address tests, formatting and whitespace checks pass; the executor also passed the full standard suite and 30/45-second focused fuzz runs. No new comment-depth rejection was introduced. This closes the resource item, not the separate partial-address and damaged-trailing-comment recovery requirements.

Codex reviewed `ROB-100-007` for [issue #15](https://github.com/saeroun-ai/emlkit/issues/15), independently passed 36 root/child/nested/chunked CP949 classification-and-body scenarios, and found and returned a same-entity diagnostic-callback deadlock for correction. After the cache-publication/delivery split, the independent reentry probe, callback-stop/concurrency regressions and full fresh race suite pass. Build, standard tests, formatting and whitespace checks pass; vet passes with only the pre-existing `composites` and `stdmethods` categories excluded. The construction-snapshot mutation change for `New`/`Read` as well as `Parse` was reviewed as deliberate: edited headers do not silently change the interpretation of an already-created decoder. Normal vet retains the documented baseline warnings.

Codex reviewed `ROB-050-001` for [issue #22](https://github.com/saeroun-ai/emlkit/issues/22) and returned three gaps for correction: an ambiguous multipart losing later siblings, a known Strict refusal appearing successful through `Walk`, and empty-only parameter separators escaping repair. The corrected implementation independently passes 240 root/nested/chunked tolerant/forensic recovery scenarios, 288 opaque-body/Walk/mail scenarios with exact bytes and following siblings, the previous 36 attachment/charset scenarios, and the reentrant accessor probe. A fresh full race suite, build, formatting, whitespace checks and vet with the two documented baseline categories excluded pass. The executor also passed the full standard suite and the final 60-second focused fuzz run; no permission denials occurred. Nine identical synthetic inputs were actually exercised with Python 3.14.10 and Mime4j 0.8.15; conflicting boundaries intentionally differ, and broad reference parity is not claimed. `ROB-050-002` and the other recovery items remain Planned. The writer panic found during this review is tracked separately in #58.

Codex reviewed `ROB-120-008` for [issue #58](https://github.com/saeroun-ai/emlkit/issues/58): the independent writer matrix passes 28 cases (the initial 20 had 12 panics and six silent successes), and repeated reads after malformed-header or unsupported-charset setup failures return without blocking. The 240 healing and 288 opaque-body/Walk/mail scenarios still pass. A fresh full race suite, build, formatting and whitespace checks pass, as do the non-baseline vet analyzers; ordinary vet retains the documented composites and WriteTo-signature warnings. The executor passed the full standard suite. Review corrected the worker-exit test to observe its own worker, checked its positive control and panic handling, and qualified output guarantees for the entity versus descendants and for Body versus WriteTo. All delegation permission-denial arrays were empty. Early consumer abandonment/Close propagation remains a lifecycle checkpoint in #6, and raw replay remains #41.

Codex reviewed `ROB-001-009` for [issue #3](https://github.com/saeroun-ai/emlkit/issues/3): independent QA passes 2,160 plain-input boundary/source-error cases, 288 multipart/skip/Walk/mail cases and 200 decoder/charset cases. A generated 256 MiB stream verifies the default without materializing the input: at most N admitted bytes plus one proof byte. A fresh full race suite, build, formatting and whitespace checks pass after review corrections; the executor also passed the full standard suite, 386 checks and scoped input fuzzing (90 s plus 45 s after review). Review corrected read-ahead, proof-byte, stop identity and legacy-header-cap documentation and added the prefetch regression. Fuzzing independently reproduced preexisting corrupt-Base64 chunk dependence on clean `6fb8b93`; issue #34 tracks it, and the scoped oracle does not claim malformed decoder chunk invariance. Plain vet retains its documented 13 composite and one WriteTo-signature warnings; the other analyzers pass. Both delegation permission-denial arrays were empty. Unread tails, cancellation, no-progress handling and body limits remain separate work.

Codex reviewed `ROB-001-008` for [issue #8](https://github.com/saeroun-ai/emlkit/issues/8): the independent public cap matrix passes 196 cases; input-after-cap, terminal sink refusal and Strict local failures retain their own errors and preserve valid siblings. A 10,000-part malformed-parameter storm returns every body while the default cap delivers exactly 1,000 callbacks with no retained diagnostics in sink-only mode. The final full race suite, build, formatting and whitespace checks pass on Go 1.26.10 linux/amd64; the executor also passed the full standard suite, focused 386 checks and all prior input/healing/opaque probes. Review corrected the terminal Action so it does not promise continuation after an independent failure, qualified diagnostic completeness claims, verified FatalError keeps its original diagnostic, and fixed the enforced-budget test map and a test failure-path index. MaxDiagnosticCount reserves its final slot and does not stop parsing; producer-side formatting can still cost work after recording is capped. Both delegation permission-denial arrays were empty. Baseline vet categories remain unchanged.

Codex reviewed ROB-001-005 for issue #4. Independent public QA passed 168 body-budget cases, 1,080 encoded/charset cases over source and destination read sizes, and six generated-stream default boundary checks at 128/128/512 MiB. Local-limit sibling recovery and the local-then-total stop regression pass. Prior input 2,160 + multipart 288 + decoder 200 cases, diagnostic matrix 196 and 10,000-part storm, heal 240, opaque 288 and bounded-mail 36 all pass on the final functional code. Full non-short race passed in all five packages on Go 1.26.10 linux/amd64, as did build, formatting, whitespace and non-baseline vet checks. Codex input fuzzing passed 30 s / 4,253,656 executions with the scoped oracle; executor body fuzzing and 386 checks also passed. Review fixed dropped global-stop precedence for a previously failed body and restored input-limit errors masked by QP (also reproduced on clean c484941). A new arbitrary errors.As call in the guard was rejected after process-isolated source-cycle and As-panic regressions; the final implementation uses the actual input-limiter failure identity and only bounded stdlib-join layers. Both independent hostile-source probes now return the same source error normally. The wider predicate audit remains #16. Only ROB-001-005 is completed; ROB-070-010, ROB-080-008, cancellation/no-progress, and raw replay remain separate. Raw WriteTo stays conditional and one-shot; decoded limits count bytes and may cut UTF-8, container raw views are governed by MaxInputBytes, and per-body refusals permit later siblings within other budgets. All delegation permission-denial arrays were empty. Plain vet retains the documented composites and WriteTo-signature warnings. Final review also corrected source-error wording, prefix retention under interacting budgets and branch-neutral diagnostic actions; the final wording-only update passed focused tests and formatting without repeating unrelated full fuzz/race runs.

Codex reviewed `ROB-001-010` for [issue #5](https://github.com/saeroun-ai/emlkit/issues/5).
Independent public QA passed 2,352 header tree/boundary/chunk/path/offset cases,
199 sink/cap/mail/provenance checks, three count-only endless-candidate probes,
and six lazily generated default boundary checks (32 MiB and 100,000 fields).
Prior input (2,160 + 288 + 200), body (168 + 1,080), diagnostic (196 and a
10,000-part storm), heal (240), opaque (288) and bounded-mail (36) probes passed
on the final functional code. Full non-short tests and race passed in all five
packages, as did build, focused 386, gofmt/diff checks and vet excluding the two
existing warning categories. A final full test rerun after the test-generator
and wording corrections passed. Scoped header fuzzing passed 30 seconds and
1,033,524 executions. The empty LF multipart seed is retained; its pre-existing
wrapped-EOF behavior is tracked in #29, and the generator now creates nonempty
containers without weakening budget outcome assertions. Review fixed atomic
first-refusal publication and exact offsets across concurrent shared readers.
The pre-existing cyclic child-source error hang was independently reproduced on
clean `b994934` and is tracked in #16, not claimed fixed here.

An earlier unbounded preflight mutation caused a host OOM (textproto.test,
about 39 GiB RSS); that run is failed evidence. The interrupted implementation
left no final JSON, and its mutation was restored before validation. New
adversarial sources are capped at 64 KiB and concurrent loops are finite.
`scripts/qa-bounded.py` and `CLAUDE.md` now require enforced per-job RAM 1.5 GiB,
zero swap, CPU quota two cores, 128 tasks and deadlines; the cgroup settings,
nonzero exit propagation and deadline termination were verified. All subsequent
build/test/fuzz/QA runs used these limits, one QA job at a time. The successful
fuzz run's observed memory peak was 236,122,112 bytes. Completed delegation
JSONs have empty permission-denial arrays; the interrupted run is not a success.
This operational safeguard does not complete CI (#14). The wider cancellation,
no-progress, embedded-message, raw-replay and predicate-audit items remain open.


Codex reviewed `ROB-120-007` for [issue #16](https://github.com/saeroun-ai/emlkit/issues/16).
The independent 60-case baseline had eight timeouts, 13 panics and six false
positive classifications; every case now returns normally without a false
match. Genuine wrappers (18 cases), EOF classification (nine), source/hook/sink
flows across all three profiles (147), and 85 exact node/pending/child-slot
boundary checks pass. These ran as 235 deadline-bound subprocess invocations;
the boundary executable also passes with `GODEBUG=panicnil=1`. Full non-short
tests and race pass in all six packages on Go 1.26.10 linux/amd64, with build,
focused 386 checks, formatting, whitespace and vet excluding the two existing
warning categories. The existing header totals (2,352 matrix + 199 integration,
three candidate preflights and six default boundaries), input (2,160 + 288 +
200), body (168 + 1,080), diagnostic (196 and a 10,000-part storm), healing
(240), opaque (288) and mail (36) QA probes still pass. Scoped total-header
fuzzing passed 30 seconds / 694,037 executions.

Review kept the public graph search distinct from the parser's own top-level
warning decision: a charset hook replaying a genuine fatal error from another
parse leaves this part and its siblings usable; this parse's actual stop still
wins. That mail behavior change is accepted within the requested healing scope
and documented above. Review also corrected exact child-slot charging, converted
the relevant old in-process endless fixtures to fused ones, qualified the
external-method and warning claims, and restored the previously skipped #5
child-cycle regression. Exact infinite cycles run only in a deadline subprocess
with a 16 MiB stack cap. Every build/test/QA run used the enforced 1.5 GiB RAM,
zero-swap, two-core, 128-task bounded runner, one job at a time; no guard-disabling
mutation was run. All completed delegation JSONs have empty permission-denial
arrays. Foreign-method execution, bounded-search false negatives and genuine
error replay remain the explicitly documented limits; this does not complete
cancellation (#6) or no-progress handling (#7).
