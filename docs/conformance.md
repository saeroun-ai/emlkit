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
| [RFC 2183](https://www.rfc-editor.org/rfc/rfc2183) | Content-Disposition — inline/attachment, filename | [`header.go`](../header.go) · `ContentDisposition`, [`mail/attachment.go`](../mail/attachment.go) · `parseFilename` |
| [RFC 2231](https://www.rfc-editor.org/rfc/rfc2231) | Parameter value continuations and charset (e.g. `filename*=`) | Go standard library `mime`, via [`header.go`](../header.go) · `parseHeaderWithParams` |
| [RFC 6532](https://www.rfc-editor.org/rfc/rfc6532) | Internationalized headers — raw UTF-8 in header values | [`mail/header.go`](../mail/header.go) · `isMultibyte`, [`textproto/header.go`](../textproto/header.go) |
| [RFC 3501](https://www.rfc-editor.org/rfc/rfc3501) §5.1.3 | Modified UTF-7 (IMAP mailbox names) | [`utf7/`](../utf7) · `Encoding`, [`charset/charset.go`](../charset/charset.go) |

A few details worth knowing:

- **RFC 2047 (encoded-words).** Decoding accepts both `B` (base64) and `Q`
  (quoted-printable) words in any registered charset, via the standard library's
  `mime.WordDecoder`. **Encoding emits `Q` words only** — emlkit never produces
  `B`-encoded words ([`charset.go`](../charset.go) · `encodeHeader`).
- **RFC 2231.** emlkit does not implement RFC 2231 itself; it delegates entirely
  to Go's `mime.ParseMediaType` / `mime.FormatMediaType`, so its fidelity is
  exactly the standard library's. A parameter value is first 2231-decoded by the
  stdlib, then run through RFC 2047 decoding once more.
- **RFC 6532.** Header *values* carry raw bytes verbatim (only header *keys* are
  restricted to printable ASCII), so UTF-8 values pass through. The `mail` parser
  additionally treats multi-byte runes as valid atom text.
- **RFC 3501 modified UTF-7.** Registered under the charset name `utf-7`
  (case-insensitive), but only when the `charset` plugin is imported (see
  [Not supported](#not-supported--rejected)). Without the plugin it is an unknown
  charset.

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
- **Bare LF line endings.** Headers terminated by `\n` (no `\r`) are accepted and
  normalized to CRLF in the stored raw bytes. ([`textproto/header.go`](../textproto/header.go) · `readContinuedLineSlice`)
- **Folded values** are unfolded and whitespace-collapsed for the parsed value;
  the original raw bytes are preserved separately. ([`textproto/header.go`](../textproto/header.go) · `trimAroundNewlines`)
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
- **Bare LF boundary lines.** If the first boundary ends in a lone `\n`, the
  reader switches to LF mode for the rest of the body. ([`textproto/multipart.go`](../textproto/multipart.go) · `isBoundaryDelimiterLine`)
- **Missing closing boundary.** On EOF before the final `--boundary--`, buffered
  bytes are flushed as body and an `ErrMissingBoundaryClose` is reported, instead
  of dropping data or looping forever. ([`textproto/multipart.go`](../textproto/multipart.go) · `scanUntilBoundary`)
- **Preamble** before the first boundary is skipped. ([`textproto/multipart.go`](../textproto/multipart.go) · `nextPart`)
- **Trailing whitespace** after a boundary delimiter is tolerated. ([`textproto/multipart.go`](../textproto/multipart.go) · `skipLWSPChar`)
- **Malformed/empty `boundary` parameter on write** is re-synchronized: the writer
  rewrites the Content-Type header to match the boundary it actually emits, so the
  header and body delimiters never disagree. ([`writer.go`](../writer.go) · `createWriter`)

### Charset and header text

- **Empty or missing charset** is treated as unspecified (effectively us-ascii)
  and passed through without error. ([`charset.go`](../charset.go) · `charsetReader`)
- **Malformed encoded-words** degrade to the raw header value; in parameter
  parsing the decode error is swallowed entirely. ([`charset.go`](../charset.go) · `decodeHeader`)
- **Missing Content-Type** defaults to `text/plain`. ([`header.go`](../header.go) · `ContentType`)

### `mail` convenience layer

- **Unknown-charset parts are surfaced** (raw body, with the error forwarded) so
  iteration continues. Note an asymmetry: an unknown *transfer-encoding* part does
  stop `NextPart`. ([`mail/reader.go`](../mail/reader.go) · `NextPart`)
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
- **Header size is bounded.** The header block defaults to a 1 MiB cap
  (`MaxHeaderBytes`; set it to `-1` to disable). There is no per-line length limit
  on read. ([`entity.go`](../entity.go) · `withDefaults`)

## Behavioral guarantees

- **Verbatim round-trip (DKIM-friendly).** A header field that was parsed and not
  modified is re-serialized byte-for-byte — including original whitespace, folding,
  and field order. Each field keeps its raw bytes; only mutated or synthesized
  fields are reformatted. This is what makes parsed-then-written messages safe for
  DKIM signatures. ([`textproto/header.go`](../textproto/header.go) · `headerField.raw` / `WriteHeader`)
- **Body byte-preservation.** When the output transfer-encoding and charset match
  the input (case-insensitively), the original undecoded body bytes are copied
  through verbatim instead of decoded and re-encoded — preserving exact bytes for
  body hashes. ([`entity.go`](../entity.go) · `writeBodyTo`)
- **Error with a usable result.** `Read`, `New`, and `mail.CreateReader` may return
  an error that satisfies `IsUnknownCharset` or `IsUnknownEncoding` **together with
  a fully usable** `Entity` / `Reader`. Treat these predicates as warnings (the raw
  body is still readable); bail only on other errors.
  ([`charset.go`](../charset.go) · `IsUnknownCharset`, [`encoding.go`](../encoding.go) · `IsUnknownEncoding`, [`mail/reader.go`](../mail/reader.go) · `CreateReader`)
- **Copy-on-write headers.** Writer constructors copy the caller's `Header` before
  mutating it, so building a writer never changes the caller's view.
  ([`writer.go`](../writer.go) · `CreateWriter`, [`mail/writer.go`](../mail/writer.go))
- **Sealed interfaces.** `HeaderFields` and `mail.PartHeader` have unexported marker
  methods, so they cannot be implemented outside the module; new concrete types
  belong inside emlkit. ([`textproto/header.go`](../textproto/header.go), [`mail/reader.go`](../mail/reader.go) · `PartHeader`)
