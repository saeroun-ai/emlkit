# Standards, Tolerances & Limitations

This document describes exactly what `emlkit` implements, what malformed input it
accepts, what it refuses, and the behavioral guarantees it makes. It is the
detailed companion to the summary in the [README](../README.md).

Every claim below points at the code that backs it (file · symbol). emlkit is
layered — `textproto` (wire format) → root `message` (MIME semantics) →
`mail` (convenience API), with an optional `charset` plugin. References name the
lowest layer where the behavior lives.

## Implemented standards

| Standard | Scope | Implemented in |
|----------|-------|----------------|
| [RFC 5322](https://www.rfc-editor.org/rfc/rfc5322) | Internet Message Format — header field syntax, message structure | [`textproto/header.go`](../textproto/header.go) · `ReadHeader`, [`entity.go`](../entity.go) · `Read`, [`mail/header.go`](../mail/header.go) |
| [RFC 2045](https://www.rfc-editor.org/rfc/rfc2045) | MIME Part 1 — Content-Type and Content-Transfer-Encoding (quoted-printable, base64, 7bit, 8bit, binary) | [`encoding.go`](../encoding.go) · `encodingReader`/`encodingWriter` |
| [RFC 2046](https://www.rfc-editor.org/rfc/rfc2046) | MIME Part 2 — multipart media types and boundaries | [`textproto/multipart.go`](../textproto/multipart.go) · `MultipartReader`/`MultipartWriter` |
| [RFC 2047](https://www.rfc-editor.org/rfc/rfc2047) | MIME Part 3 — encoded-words in headers | [`charset.go`](../charset.go) · `decodeHeader`/`encodeHeader` |
| [RFC 2152](https://www.rfc-editor.org/rfc/rfc2152) | Standard UTF-7 MIME charset | [`charset/utf7.go`](../charset/utf7.go), [`charset/charset.go`](../charset/charset.go) |
| [RFC 2183](https://www.rfc-editor.org/rfc/rfc2183) | Content-Disposition — inline/attachment, filename | [`header.go`](../header.go) · `ContentDisposition`, [`mail/attachment.go`](../mail/attachment.go) · `parseFilename` |
| [RFC 2231](https://www.rfc-editor.org/rfc/rfc2231) | Parameter value continuations and charset (e.g. `filename*=`) | Go standard library `mime`, via [`header.go`](../header.go) · `parseHeaderWithParams` |
| [RFC 6532](https://www.rfc-editor.org/rfc/rfc6532) | Internationalized headers — raw UTF-8 in header values | [`mail/header.go`](../mail/header.go) · `isMultibyte`, [`textproto/header.go`](../textproto/header.go) |
| [RFC 3501](https://www.rfc-editor.org/rfc/rfc3501) §5.1.3 | Modified UTF-7 codec for IMAP mailbox names | [`utf7/`](../utf7) · `Encoding` (used directly, not wired to the MIME `utf-7` charset) |

A few details worth knowing:

- **RFC 2047 (encoded-words).** Decoding accepts both `B` (base64) and `Q`
  (quoted-printable) words in any registered charset, via the standard library's
  `mime.WordDecoder`. **Encoding emits `Q` words only** — emlkit never produces
  `B`-encoded words ([`charset.go`](../charset.go) · `encodeHeader`).
- **RFC 2152 standard UTF-7.** The optional `charset` plugin registers the MIME
  charset `utf-7` to a standard UTF-7 decoder (`+` shifts, standard base64
  `+/`). This covers legacy mail that advertises `charset=utf-7`.
- **RFC 2231.** emlkit does not implement RFC 2231 itself; it delegates entirely
  to Go's `mime.ParseMediaType` / `mime.FormatMediaType`, so its fidelity is
  exactly the standard library's. A parameter value is first 2231-decoded by the
  stdlib, then run through RFC 2047 decoding once more.
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
- **Empty keys are skipped**, not treated as an error. ([`textproto/header.go`](../textproto/header.go) · `ReadHeader`)
- **Bare LF, bare CR and mixed header line endings** (`ROB-010-002`). A header line ends at `\r\n` (one ending even when split
  across reads), at a bare `\n`, or at a bare `\r`; a stray `\r` inside a value
  stays content unless the next line begins a field (`name:`), so
  `Subject: a\rb` keeps its value `a\rb`. A bare `\r` at the start of a line is
  the empty line ending the header, `A: 1\r\r\n` is field A then the empty line
  `\r\n`, and a `\r` right before end of input is a complete line ending. The
  stored raw bytes end every physical line with CRLF — only a complete CRLF
  field is byte-exact — and `textproto.Header.LineEndings` reports what the
  input used. Under `Parse` the endings are diagnosed (`header.bare_lf`,
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
  rather than kept as a field, such as one with an empty name, is not written;
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
- **Folded values** are unfolded and whitespace-collapsed for the parsed value;
  the original raw bytes are preserved separately. ([`textproto/header.go`](../textproto/header.go) · `trimAroundNewlines`)
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
  malformed or whitespace-only first line is `ROB-020`'s.
  ([`parse.go`](../parse.go) · `Parse`, [`textproto/header.go`](../textproto/header.go) · `ReadHeader`)
- **Over-long header keys** do not crash the line-folder (a naive fold width would
  go negative; it falls back to the hard limit). ([`textproto/header.go`](../textproto/header.go) · `formatHeaderField`)

### Transfer encoding

- **Malformed quoted-printable.** A `lenientQPReader` normalizes input that would
  otherwise break the standard library's reader: a stray `=` becomes `=3D`, raw
  control bytes are hex-escaped, and over-long lines get a soft break. Conformant
  QP passes through unchanged. ([`encoding.go`](../encoding.go) · `lenientQPReader`)
- **Indented base64.** Leading spaces/tabs (against spec) are rewritten to
  newlines before decoding. ([`encoding.go`](../encoding.go) · `whitespaceReplacingReader`)
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
  before. That budget is charged on the candidate's valid prefix however the
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
- **Preamble** before the first boundary is skipped. ([`textproto/multipart.go`](../textproto/multipart.go) · `nextPart`)
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
  for bodies and RFC 2047 words alike. Only the declared label is honored:
  nothing detects a missing or wrong charset declaration.
  ([`charset/charset.go`](../charset/charset.go) · `charsets`)
- **Malformed encoded-words** degrade to the raw header value; in parameter
  parsing the decode error is swallowed entirely. ([`charset.go`](../charset.go) · `decodeHeader`)
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
- **No mbox / `From ` envelope handling.** emlkit parses a single message, not an
  mbox archive; there is no special-casing of a leading `From ` line. Swallowing
  such artifacts would break the verbatim round-trip. ([`textproto/header.go`](../textproto/header.go) · `ReadHeader`)
- **An `Entity` is consumed once.** The body is a stream; reading it (or calling
  `Walk`) exhausts the entity. There is no random-access / buffered message type.
  ([`entity.go`](../entity.go) · `Walk`)
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
  read; a malformed first line beginning with white space is not a candidate.
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
- **`MaxMessageDepth` is reserved until nested messages exist.** It counts nested
  `message/rfc822` and `message/global` entities, and this engine treats such a
  body as an opaque leaf rather than as a nested entity, so there is no
  nested-message transition to count. The field is validated, resolved and
  reported by `ParseSession.Limits`, and `limit.message_depth` is reserved but
  never emitted, until `ROB-090-001` introduces that model and `ROB-090-002`
  activates the budget. A `message/rfc822` part is meanwhile one ordinary part of
  `MaxPartCount`, and the structure inside its opaque body costs neither tree
  budget. ([`limits.go`](../limits.go))
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
  always has. Nothing is replayed (#41), and no raw API is added: the existing
  verbatim `WriteTo` path stays a conditional one-shot copy.
  ([`limit_body.go`](../limit_body.go))
- **The remaining budgets in `Limits` are still resolved but not consulted.**
  Everything outside the fifteen enforced ones — `MaxInputBytes`,
  `MaxHeaderBytes`, `MaxHeaderFieldBytes`, `MaxHeaderCount`, `MaxLineBytes`,
  `MaxTotalHeaderBytes`, `MaxTotalHeaderCount`, `MaxParameterCount`, `MaxParameterSegments`, `MaxMIMEDepth`, `MaxPartCount`,
  `MaxDiagnosticCount`, `MaxEncodedBodyBytes`, `MaxDecodedBodyBytes` and
  `MaxTotalDecodedBytes` — is validated and
  reported by `ParseSession.Limits`, and their `limit.*` codes are reserved but
  not yet emitted. ([`limits.go`](../limits.go))

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
  gives the same bytes. Original-byte replay is #41's.
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
