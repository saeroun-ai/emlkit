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
  single parser-wide contract (`ROB-001-006`, `ROB-001-007`).
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
with the errors it is given, and the wider predicate audit stays with
`ROB-120-007` (#16). This fixes a defect present before
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
`(0, nil)` through unchanged, and nothing is retried there (`ROB-001-007` owns
no-progress). Reads before `N` are only shortened, so offsets and the bytes
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
  from any reader, with that same value. The wider predicate audit stays with
  `ROB-120-007` (#16): a part header's source error that unwraps to itself
  still hangs the pre-existing `errors.Is` in `textproto` `NextPart` (also on
  `b994934`), which this item does not change.
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

### ROB-001 — Budgets, cancellation, and progress

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-001-001 | Done | Centralize all configurable resource budgets and defaults. | Default/disable/overflow configuration tests. |
| ROB-001-002 | Done | Apply message and per-entity header byte limits, including child part headers. | Top/child/deep child boundary tests. |
| ROB-001-003 | Done | Limit header fields, physical line bytes, logical field bytes, parameters, and RFC 2231 segments. | `limit-1/limit/limit+1` tests. |
| ROB-001-004 | Done | Limit multipart MIME depth and total part count without recursive stack growth; nested-message depth is deferred to ROB-090-002. | Deep multipart and single-/multi-container wide generated messages. |
| ROB-001-005 | Done | Limit encoded/decoded body bytes and total decoding expansion under `Parse`: per-leaf `MaxEncodedBodyBytes`/`MaxDecodedBodyBytes` (framed containers are raw views outside them) and the shared `MaxTotalDecodedBytes`, exact at `N` with one undelivered proof byte. | Streaming overflow at `N-1/N/N+1` for identity, base64, quoted-printable, charset and custom expanding/masking/eager decoders, across source and destination chunk sizes: the header and the delivered prefix stay valid, and output decoders buffered from encoded-admitted wire bytes is still delivered within the decoded budgets; a per-body overflow ends that body alone and the parent can drain its unread raw tail under `MaxInputBytes` to reach later and shallower siblings unless another limit or a sink stops it; a total overflow ends the parse. Decoders cannot mask the body's failure or this parse's `MaxInputBytes` failure, recognized by provenance without calling `Is`/`As`/`Unwrap` on source errors, with or without body budgets (fixes the issue #3 quoted-printable masking); the guard never asks or replaces a source's own error, cyclic or panicking ones included, though a decoder may still reword an ordinary one as before. "Recoverable raw body" is the reviewed one-shot scope: no raw API or replay is added; the existing conditional verbatim `WriteTo` copy, before `Body` is read and where the writer accepts the header, is held to the encoded and input budgets only; consumed, proof, read-ahead and drained raw bytes are not replayed (#41, not done here). |
| ROB-001-006 | Planned | Add context cancellation and deadline checks to long scans/decoders. | Cancellation latency and partial-result tests. |
| ROB-001-007 | Planned | Detect repeated no-progress reads while allowing legal transient behavior. | Adversarial reader tests. |
| ROB-001-008 | Done | Cap diagnostics under `Parse` with `MaxDiagnosticCount`, and emit one terminal `limit.diagnostic_count` entry in the last slot (`N-1` ordinary, request `N` replaced, later requests omitted without copy, retention or sink call) while parsing continues. | Diagnostic storm through public `Parse`: every malformed part healed and returned under the cap; `0`/default/`-1`/`1`/`N-1`/`N`/`N+1`/`math.MaxInt64`; retain/no-retain with sink none/ok/refusing; later input budget and sink stops after the cap; concurrent records, separate sessions and reentrant inspection. |
| ROB-001-009 | Done | Enforce `MaxInputBytes` under `Parse` once, at the raw source shared by every header, body, multipart and skip/drain read: `N` bytes are admitted, a proven byte `N+1` (at most one proof byte, never handed on) records one `limit.input_bytes` diagnostic and publishes the session stop; `Read`/`ReadWithOptions` unchanged. | `N-1/N/N+1` root header and body, nested and skipped parts, chunkings, `(n>0, EOF/error)`, delayed EOF, `(0, nil)`, empty reads, default/disabled/`math.MaxInt64`, sink-stop and header-budget precedence. |
| ROB-001-010 | Done | Enforce session-wide `MaxTotalHeaderBytes` (32 MiB) and `MaxTotalHeaderCount` (100,000) under `Parse`, shared by the root and every scanned child header, while preserving per-entity limits and legacy entry points. | Exact boundaries, wide/deep trees, count exhaustion preflight before allocating the next candidate, no-refund accounting for refused headers, no body/framing/read-ahead charges, session-stop identity, sink/cap precedence, provenance, chunk/error/default/disabled/MaxInt64/386 tests, and independent QA. Independent bounded QA, full tests/race, corrected scoped fuzzing and compatibility review passed; issue #5 awaits remote inclusion of the new local commit. |

### ROB-010 — Input and message framing

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-010-001 | Planned | Treat empty input and header-only input deterministically. | All profiles and round-trip tests. |
| ROB-010-002 | Planned | Handle CRLF, bare LF, bare CR, mixed endings, and a missing final newline. | Chunk boundary matrix. |
| ROB-010-003 | Planned | Detect and preserve BOMs without letting them corrupt a field name. | UTF-8/16/32 BOM fixtures. |
| ROB-010-004 | Planned | Optionally recognize a leading mbox envelope line and preserve it separately. | Valid/invalid/mid-header `From ` fixtures. |
| ROB-010-005 | Planned | Detect concatenated messages or trailing unassigned bytes in forensic mode. | Two-message and trailing-junk fixtures. |
| ROB-010-006 | Planned | Preserve bytes returned with EOF and partial bytes before injected I/O errors. | Custom-reader tests. |

### ROB-020 — Raw header framing and preservation

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-020-001 | Planned | Recover a first-line continuation as a raw malformed field/body candidate with policy-based interpretation. | Angus/Python/Mime4j compatibility fixtures. |
| ROB-020-002 | Planned | Recover orphan continuation lines after malformed fields. | Raw preservation and diagnostic tests. |
| ROB-020-003 | Planned | Support policy choices for a colonless line: raw malformed header, semicolon separator, or start of body. | Ambiguity and deterministic-precedence tests. |
| ROB-020-004 | Planned | Recognize whitespace-only header/body separators when enabled. | Spaces/tabs/mixed fixtures. |
| ROB-020-005 | Planned | Recover missing header/body separator without losing the first body line. | Plain and multipart part fixtures. |
| ROB-020-006 | Planned | Preserve invalid/empty field names and raw 8-bit values while exposing only safe canonical lookup keys. | Control/NUL/non-ASCII key tests. |
| ROB-020-007 | Planned | Preserve original field case, order, folding, whitespace, and physical line endings. | Byte-exact write tests. |
| ROB-020-008 | Planned | Diagnose duplicate singleton fields without deleting them. | MIME-Version, Content-Type, From, Date fixtures. |
| ROB-020-009 | Planned | Make every header parser path panic-free for arbitrary bytes and pathological lengths. | Native fuzz target and allocation bound. |

### ROB-030 — RFC 2047 and unstructured text

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-030-001 | Planned | Decode conformant B/Q words and adjacent-word whitespace correctly. | RFC examples and round-trip tests. |
| ROB-030-002 | Planned | Recover embedded encoded words in tolerant mode. | Strict/tolerant differential fixtures. |
| ROB-030-003 | Planned | Preserve malformed/unknown encoded words while decoding valid neighbors. | Partial decode tests. |
| ROB-030-004 | Planned | Diagnose raw UTF-8/8-bit header text and apply configured fallback charset. | Valid/invalid UTF-8 fixtures. |
| ROB-030-005 | Planned | Reject or neutralize decoded CR/LF/NUL header injection. | Security tests. |
| ROB-030-006 | Planned | Limit encoded-word count, nesting-like work, and decode expansion. | Adversarial complexity tests. |

### ROB-040 — Structured RFC 5322 headers

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-040-001 | Planned | Return partial address lists with per-address diagnostics. | Valid-invalid-valid sequences. |
| ROB-040-002 | Planned | Support configurable loose parsing for unquoted commas and local-only addresses. | MimeKit-compatible fixtures. |
| ROB-040-003 | Done | Bound nested comments and address groups. | Deep nesting tests. |
| ROB-040-004 | Planned | Tolerate obsolete route syntax, empty groups, repeated separators, and broken angle brackets without panic. | Corpus table tests. |
| ROB-040-005 | Planned | Parse internationalized addresses and preserve undecodable originals. | RFC 6532 and IDN cases. |
| ROB-040-006 | Planned | Parse common obsolete dates, report invalid dates, and retain raw values. | Zone/year/comment/weekday table. |
| ROB-040-007 | Planned | Return partial Message-ID/References lists. | CFWS, truncation, invalid ID tests. |
| ROB-040-008 | Planned | Add tolerant Received parsing with clause-level preservation. | Real provider fixtures. |
| ROB-040-009 | Planned | Harden List-* parsing and preserve multiple URLs/NO values. | RFC 2369/2919 fixtures and fuzz. |

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
| ROB-060-001 | Planned | Preserve preamble and epilogue with raw spans. | Empty/nonempty/limited fixtures. |
| ROB-060-002 | Planned | Infer a missing boundary parameter from credible delimiter lines. | Angus-compatible positive/negative fixtures. |
| ROB-060-003 | Planned | Detect declared/actual boundary mismatch and optionally adopt the actual boundary. | Antivirus-rewrite fixtures. |
| ROB-060-004 | Planned | Rank multiple boundary candidates deterministically and diagnose ambiguity. | Preamble dashes and nested candidates. |
| ROB-060-005 | Planned | Recover missing start and closing delimiters while returning partial parts. | Python defect-compatible tests. |
| ROB-060-006 | Planned | Accept truncated final delimiter and missing terminal line ending where unambiguous. | Every truncation point. |
| ROB-060-007 | Planned | Handle boundary LWSP, invalid suffixes, mixed endings, and buffer splits. | Exhaustive split-position tests. |
| ROB-060-008 | Planned | Avoid false positives for boundary prefixes embedded in content. | Adversarial body corpus. |
| ROB-060-009 | Planned | Support empty multipart, consecutive delimiters, and headerless empty parts according to profile. | Strict/tolerant fixtures. |
| ROB-060-010 | Planned | Recover missing child header/body separators without losing sibling synchronization. | Broken-middle-child tests. |
| ROB-060-011 | Planned | Handle reused/prefix-related inner and outer boundaries. | Deep nested fixtures. |
| ROB-060-012 | Planned | Ignore or decode illegal multipart CTE according to explicit policy. | Outlook/broken-gateway fixtures. |
| ROB-060-013 | Planned | Guarantee linear-time boundary scanning under repeated near-matches. | Complexity benchmark. |

### ROB-070 — Content-Transfer-Encoding

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-070-001 | Planned | Normalize CTE tokens with comments, whitespace, and common trailing junk. | Header token tables. |
| ROB-070-002 | Planned | Preserve raw body and continue for unknown CTE at every API layer. | Root/mail/session compatibility tests. |
| ROB-070-003 | Planned | Diagnose 7bit violations without discarding bytes. | High-bit/NUL/long-line fixtures. |
| ROB-070-004 | Planned | Ignore Base64 whitespace and configured nonalphabet junk with diagnostics. | Angus/Python-compatible fixtures. |
| ROB-070-005 | Planned | Repair missing/invalid Base64 padding and truncated final quanta when possible. | Length modulo 4 matrix. |
| ROB-070-006 | Planned | Handle data after Base64 padding and URL-safe alphabet policy. | Strict/tolerant tables. |
| ROB-070-007 | Planned | Return decoded Base64 prefixes and raw fallback for unrecoverable length cases. | Prefix/fallback assertions. |
| ROB-070-008 | Planned | Complete malformed quoted-printable recovery, including CR-only soft breaks. | Invalid-hex/truncation/control tables. |
| ROB-070-009 | Planned | Support uuencode aliases and configurable missing begin/end recovery. | Angus-compatible uuencode fixtures. |
| ROB-070-010 | Planned | Apply decoded-size budgets during streaming, not after allocation. | Expansion and cancellation tests. |

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
| ROB-090-001 | Planned | Parse `message/rfc822` and `message/global` as nested entities with independent diagnostics. | Plain/encoded/truncated fixtures. |
| ROB-090-002 | Planned | Activate the reserved `MaxMessageDepth` budget for the nested entities introduced by ROB-090-001. | Deep message chain. |
| ROB-090-003 | Planned | Support `multipart/digest` default child type semantics. | Missing child Content-Type fixtures. |
| ROB-090-004 | Planned | Preserve `multipart/related` root/start/type relationships. | CID/root-selection fixtures. |
| ROB-090-005 | Planned | Model `multipart/report` and common delivery/disposition/feedback reports. | RFC samples and malformed blocks. |
| ROB-090-006 | Planned | Validate `multipart/signed` and `multipart/encrypted` shape without modifying signed raw bytes. | Signature hash invariants. |
| ROB-090-007 | Planned | Support `text/rfc822-headers` as a header block entity. | Valid/malformed header fixtures. |
| ROB-090-008 | Planned | Preserve unsupported `message/partial` and `message/external-body` metadata safely. | No external fetch, raw preservation. |

### ROB-100 — Message body and attachment view

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-100-001 | Planned | Preserve the complete MIME tree before producing any flattened convenience view. | Hierarchy round-trip tests. |
| ROB-100-002 | Planned | Classify attachments using disposition, filename candidates, media type, and related references with explainable reasons. | Cross-client corpus. |
| ROB-100-003 | Planned | Select `multipart/alternative` bodies by caller capability without discarding alternatives. | Nested alternative fixtures. |
| ROB-100-004 | Planned | Resolve `multipart/related` inline resources by normalized Content-ID/Location. | HTML CID cases. |
| ROB-100-005 | Planned | Represent embedded messages, calendars, zero-byte files, duplicates, and unknown media types. | Typed-view tests. |
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
errors still errors). The predicates themselves are unchanged; `ROB-120-007`
still owns their behavior on hostile error graphs.

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
| ROB-110-005 | Planned | Preserve untouched message/header/body bytes after semantic reads. | Parse-read-write DKIM body/header hash tests. |
| ROB-110-006 | Planned | Compute optional hashes during streaming without extra passes. | Known digest and partial-read tests. |
| ROB-110-007 | Planned | Ensure no temp files, descriptors, or goroutines leak on any error path. | Stress tests with leak assertions. |

### ROB-120 — Security hardening

| ID | Status | Requirement | Acceptance |
|---|---|---|---|
| ROB-120-001 | Planned | Audit all input-derived slice bounds, integer arithmetic, and allocation sizes. | Fuzz/32-bit-oriented boundary tests. |
| ROB-120-002 | Planned | Make parsing and decoding complexity linear or explicitly budgeted. | Near-match and nesting benchmarks. |
| ROB-120-003 | Planned | Prevent header injection in all decoded/setter/serialization paths. | CR/LF/NUL tests. |
| ROB-120-004 | Planned | Keep filename sanitization, HTML safety, archive expansion, and executable handling outside core parsing. | API boundary/documentation tests. |
| ROB-120-005 | Planned | Never perform network fetches for Content-Location/external-body while parsing. | Hostile URI tests. |
| ROB-120-006 | Planned | Preserve cryptographic raw representations and clearly mark repaired signed content as unverifiable. | DKIM/S-MIME/PGP invariants. |
| ROB-120-007 | Planned | Make every parser-wide error classification cycle-safe and non-forgeable over input-derived error graphs. | Hostile-graph tests at each predicate: no hang, no panic, no nil dereference, no misclassification. |
| ROB-120-008 | Done | Make writer generation and serialization refuse a syntactically unparseable Content-Type with an explicit error, never a panic, an invented media type, parameter map or boundary, or silent metadata loss ([issue #58](https://github.com/saeroun-ai/emlkit/issues/58)). | Lowercase/uppercase malformed tail, duplicate boundary, malformed text/plain and missing subtype, at the root and as a part, through `CreateWriter`, `CreatePart`/`CreatePartWithBuffer`, `Parse`→`WriteTo` and `NewMultipart` Body reads: no panic; no output from the refused entity or part before its header is validated (output of its parent and of earlier siblings may already have been written and is not withdrawn); a good part after a refused one, deterministic repeated reads with no blocked worker; valid boundary generation, charset/encoding policy and copy-on-write unchanged. |

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
