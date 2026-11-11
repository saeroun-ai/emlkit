# MIME media types, parameters and filenames (ROB-050-002 to ROB-050-010)

Implementation notes for GitHub issues #22, #23 and #24. Every item below is
implemented and tested and awaits independent review; none is claimed `Done`.
The ledger in `robustness-roadmap.md` is not edited by this change.

The goal is to read broken real-world `Content-Type` and `Content-Disposition`
fields as well as possible, keep the raw value, and say what was repaired.
Nothing here rejects a message. Source: `mediatype_tolerant.go`, `rfc2231.go`,
`filename.go`, `metadata_repair.go`, `limit_parameters.go`, `effective.go`.

## Where the healing applies

| Path | Behavior |
|---|---|
| `Parse` → `Entity.EffectiveContentType` / `EffectiveContentDisposition`, and what `mail.Reader` classifies by | Healed, with diagnostics recorded in the session. |
| `ParseMediaTypeTolerant`, `ParseContentDispositionTolerant`, `ParseParameterCandidates`, `Header.FilenameCandidates`, `mail.AttachmentHeader.Filename` | Healed without a parse. Diagnostics are returned, or not produced. |
| `Header.ContentType`, `Header.ContentDisposition`, `Read`, `ReadWithOptions`, `New` | Unchanged: the legacy error for a field `mime.ParseMediaType` refuses. |

A conformant field behaves exactly as before. A field the whole-field parser
reads in full, and that holds no `*`, `=?`, backslash or 8-bit byte, never
reaches the tolerant reading. Any other field is read item by item, and each
plain `name=value` item is first given to `mime.ParseMediaType` on its own, so
an item the standard library accepts keeps the value it always had.

Profiles: `ProfileTolerant` (default) and `ProfileForensic` repair identically.
`ProfileStrict` refuses an entity whose field needs a *repair* (an
entity-scoped `FatalError`, no interpretation, the parse continues) and makes
no charset guess. A field with *observations* only is interpreted alike under
every profile.

| Kind | Severity | Codes |
|---|---|---|
| Repair (Strict refuses) | `Recovered`; `InvalidMetadata` for an unframed multipart or a strict refusal | `mime.content_type_missing_type`, `_missing_subtype`, `_missing_slash`, `mime.content_type_invalid`, `mime.disposition_invalid`, `mime.parameter_unquoted_value`, `_unclosed_quote`, `_invalid_escape` (stray quote or trailing backslash), `_duplicate`, `_segment_missing`, `_segment_duplicate`, `_bad_percent_escape`, `_unknown_charset` (extended value with no charset prefix) |
| Observation | `Info` | `mime.parameter_rfc2047`, `mime.parameter_raw_utf8`, `mime.parameter_invalid_escape` (backslash kept as a backslash), `mime.filename_conflict` |
| Observation | `Unsupported`, or `Recovered` when a charset guess decoded the bytes | `mime.parameter_unknown_charset` |
| Observation | `Recovered` | `mime.parameter_fallback_charset` (raw 8-bit bytes read in the caller's fallback charset) |

A field records at most one diagnostic per code, in a fixed order, after any
`limit.parameter_*` diagnostic. All of a field's diagnostics share one
`Action` naming the base and the parameters kept.

## ROB-050-002 — missing type, subtype or slash

Status: implemented. No new API beyond `ParseMediaTypeTolerant`.

| Field value | Read as | Code |
|---|---|---|
| `text`, `text/` | `text/plain` | `missing_slash`, `missing_subtype` |
| `multipart`, `multipart/` | `multipart/mixed` | same |
| any other bare type (`image`, `message`, `foo`) | `application/octet-stream` | same |
| `text plain` | `text/plain` (whitespace for the slash) | `missing_slash` |
| `/html`, `/plain`, `/mixed`, `/jpeg`, `/pdf` | `text/html`, `text/plain`, `multipart/mixed`, `image/jpeg`, `application/pdf` | `missing_type` |
| `; charset=x`, `charset=x` | `text/plain`, parameters kept | `missing_type` |
| `text/html charset=x`, `text / plain`, `text/plain (comment)`, `"text/plain"` | the type named; trailing text read as a parameter when it is one | `content_type_invalid` |
| nothing readable (`@#!`) | `application/octet-stream` | `content_type_invalid` |

The defaults follow RFC 2045 §5.2 (no type: `text/plain`) and RFC 2046
(unrecognized subtype: `text/plain`, `multipart/mixed`, otherwise
`application/octet-stream`). For `Content-Disposition`, a field with parameters
and no readable type is `attachment` (RFC 2183 §2.8), `attachment filename=a`
is read as if the semicolon were there, and `attachment/x` keeps `attachment`
(`mime.disposition_invalid`). An absent `Content-Disposition` is not a defect.

A multipart type that was only inferred, or repaired, and has no usable
boundary is *unframed*: one opaque part, as ROB-050-001 defined.

## ROB-050-003 — loose unquoted values

Status: implemented. An unquoted value that is not a token runs to the next
top-level semicolon, trimmed: `name=my file.pdf; x=1`,
`boundary=----=_NextPart_000`, `name=a/b?c=d`, `filename==?UTF-8?B?…?=`.
Text after a closing quote is kept (`"report".pdf` → `report.pdf`). A token
with a stray quote (`charset=utf-8"`) is the token. A trailing RFC 822 comment
is dropped (`charset=us-ascii (Plain text)`), except in `name` and `filename`,
where a parenthesis is taken for part of the name (`report (1).pdf`). A
`charset` is always one token, so a loose one ends at its first whitespace
(`charset=euc-kr format=flowed` → `euc-kr`), which keeps the body decodable.
Code: `mime.parameter_unquoted_value`.

## ROB-050-004 — unclosed quotes, bad escapes, Windows paths

Status: implemented.

- An unclosed quoted string runs to the end of the field
  (`mime.parameter_unclosed_quote`). What it swallowed stays in the value; the
  parameters after it are not re-read (see limitations).
- `filename="C:\Users\a\b.txt"` keeps its backslashes, as the standard library
  already does for a backslash before a character that needs no escaping. It is
  now reported (`mime.parameter_invalid_escape`, `Info`).
- `filename="C:\dir\"` — a backslash read as an escape of the closing quote —
  is read as `C:\dir\`. Unescaped inner quotes (`"my "special" file.txt"`) run
  to the last quote. A backslash at the very end is kept.
  (`mime.parameter_invalid_escape`, `Recovered`.)

## ROB-050-005 — duplicate parameters

Status: implemented. API: `ParameterCandidate`, `ParameterForm`,
`ParseParameterCandidates(field, value)`, `Entity.ParameterCandidates(field)`.

Every occurrence is a candidate, in field order, with its raw text; the one
used is `Chosen`.

1. `boundary` is never chosen among differing candidates: repeated with
   different values, or beside an unreadable occurrence, it has no value and
   the multipart is unframed.
2. Any other parameter takes its RFC 2231 form (`name*`, `name*N`) before its
   plain form, and within a form the first occurrence.
3. A candidate whose bytes could not be decoded is passed over while another
   is readable.
4. A damaged repeat (`p=a; p=`) is a malformed item and does not unseat the
   readable one — except for `boundary`.

`mime.parameter_duplicate` is recorded when candidates of one form differ, or
a boundary is ambiguous. Identical repeats are silent, as before. A plain
value beside an extended one (`filename=x; filename*=…`) is the designed
fallback pair and is not reported at the parameter level.

Behavior change: a conflicting non-boundary parameter used to be dropped; it
now has its first value.

## ROB-050-006 — RFC 2231 in any order and charset

Status: implemented. Segments are assembled in ascending section order
whatever order they arrive in; names match case-insensitively; encoded and
unencoded segments may be mixed; adjacent encoded segments are charset-decoded
as one run, so a multi-byte character cut across segments survives. A charset
other than `utf-8`/`us-ascii` is decoded through `CharsetReader`, under the
no-progress guard and an output cap of 8× the input + 64 bytes. A repeated
`charset'lang'` prefix on later segments is removed.

Behavior change: `filename*=EUC-KR''%C7%D1…` used to yield no value at all,
because the standard library decodes only UTF-8 and US-ASCII.

## ROB-050-007 — partial RFC 2231 values

Status: implemented.

| Defect | Result | Code |
|---|---|---|
| Gap, or series not starting at 0 | Every segment present, joined in order. Chosen over the contiguous prefix because generators that number from 1 would otherwise lose the whole name. | `segment_missing` |
| Section repeated with another value | First occurrence kept. | `segment_duplicate` |
| `%` not followed by two hex digits | Literal `%` kept. | `bad_percent_escape` |
| Charset with no decoder, or bytes not valid in the charset declared | Percent-decoded bytes kept as the value (not UTF-8); candidate `Undecoded`. All-ASCII bytes need no decoder. | `unknown_charset` (`Unsupported`) |
| `name*=value` with no `charset'lang'` prefix, or the prefix on a later segment | Read; UTF-8 assumed when valid. | `unknown_charset` (`Recovered`) |

## ROB-050-008 — RFC 2047 words and raw 8-bit bytes

Status: implemented.

- Encoded words in a value, adjacent or unquoted, are decoded through the
  existing `metadataDecoder`/`mime.WordDecoder` path, as before, and reported
  (`mime.parameter_rfc2047`, `Info`).
- Raw UTF-8 is kept (`mime.parameter_raw_utf8`, `Info`).
- Raw bytes that are not UTF-8 are decoded with the `charset` parameter of the
  entity's `Content-Type` when it has a decoder (`unknown_charset`,
  `Recovered`); otherwise kept as bytes, `Undecoded` (`Unsupported`). Strict
  makes no such guess. A `boundary` is never rewritten.

### Fallback charset and undecodable words

Added after the corpus review (corpus.md findings 8 and 9).

- **Raw 8-bit bytes, last resort.** When the bytes are not UTF-8 and the
  Content-Type's charset is absent, `utf-8`/`us-ascii`, or has no decoder, the
  caller's fallback charset is tried: `ParseOptions.FallbackCharset` for an
  entity of a parse (`Entity.FilenameCandidates`, `ParameterCandidates`,
  `EffectiveContentType`/`Disposition`), `HeaderTextOptions.FallbackCharset`
  for `Header.FilenameCandidatesWithOptions`. It is used only when the bytes
  are **valid in it** — a decoding that holds U+FFFD is refused and the bytes
  stay `Undecoded`. A candidate decoded this way has `Fallback` set and
  `Charset` naming the fallback, and the field records
  `mime.parameter_fallback_charset` (`Recovered`, an observation: it refuses
  nothing). ProfileStrict makes no such guess. RFC 2231 values and encoded
  words declare their own charset and never use it.
- **An encoded word in a charset with no decoder** stays in the value as it
  was written, as before, but the candidate is now `Undecoded` and the field
  records `mime.parameter_unknown_charset` (`Unsupported`), as for an RFC 2231
  value in an unknown charset. Before, `=?euc-kr?B?…?=` came back as the
  filename with nothing to say it was not one. The other words of the value
  are still decoded (`mime.parameter_rfc2047`). A `boundary` is exempt.
- `mail.AttachmentHeader.Filename` reads a header with no parse and takes no
  fallback; `FilenameCandidatesWithOptions` is the call for that.

## ROB-050-009 — filename candidates

Status: implemented. API: `FilenameCandidate`, `FilenameSource`,
`Header.FilenameCandidates()`, `Entity.FilenameCandidates()`.
`mail.AttachmentHeader.Filename()` and `InlineAttachmentHeader.Filename()`
return the first candidate.

Precedence: `Content-Disposition filename*` → `Content-Disposition filename` →
`Content-Type name*` → `Content-Type name`. The first that is not empty and
was decoded is returned first, then the first non-empty, then the first; the
rest follow in precedence order with their raw text.
`Entity.FilenameCandidates` records one `mime.filename_conflict` (`Info`) the
first time the candidates name different files.

Behavior change: `Filename()` now returns a name from a damaged field, falls
back to `name` when `filename` is empty, and always returns a nil error.

## ROB-050-010 — safe filename

Status: implemented. API: `SafeFilename(name string) string`, a pure function
never applied implicitly. It keeps the last path component (`/` and `\`),
replaces invalid UTF-8 and `< > : " | ? *` with `_`, removes control and
invisible format characters (bidi overrides, zero-width spaces, BOM), turns
whitespace into spaces, trims leading spaces and trailing spaces and dots,
prefixes Windows device names (`CON`, `PRN`, `AUX`, `NUL`, `COM0`–`COM9`,
`LPT0`–`LPT9`, `CONIN$`, `CONOUT$`, with any extension) with `_`, cuts to 255
bytes on a UTF-8 boundary keeping a short extension, and returns `unnamed`
for nothing usable. It is idempotent.

## Limits

- Only the occurrences within `MaxParameterCount` / `MaxParameterSegments` are
  read; the session-less functions use the default budgets (256 / 256).
- The session-less functions have no header budget above them, so a value
  longer than 64 KiB (the default `MaxHeaderFieldBytes`) keeps its RFC 2047
  encoded words undecoded: the existing word decoder is not linear in its
  input. Everything else they do is linear in the value.
- A continued parameter among the occurrences a budget cut is still dropped
  whole: past a cut nothing says whether the series went on.
- Work is linear in the retained bytes; segments are sorted only when out of
  order. One diagnostic per code per field, each naming at most 8 parameters
  of at most 64 bytes.
- Section numbers have at most 9 digits.

## Known limitations

- Parameters swallowed by an unclosed or stray quote are not re-read: the item
  scan is the budget authority and charges them as one occurrence. Likewise a
  semicolon missing between two parameters (`charset=utf-8 format=flowed`)
  costs the second one; only a semicolon missing after the media type or the
  disposition is supplied.
- `"\\server\share\[1].txt"` loses backslashes before `\` and other tspecials,
  as it always has: that reading is the standard library's and is kept for
  conformant fields. The raw text is in the candidate.
- Raw non-UTF-8 bytes are decoded only with the charset of the same entity's
  `Content-Type`, and failing that with the caller's fallback charset. No
  charset is guessed from siblings or by detection;
  `ParameterCandidate.Undecoded` marks the bytes for a caller that knows better.
- An encoded word that splits a multi-byte character across two words is
  decoded word by word (the existing decoder).
- `Strict` still decodes RFC 2047 words in parameters, as every profile always
  has; it reports them rather than refusing.
- `Entity.ParameterCandidates` and `FilenameCandidates` read the retained
  occurrences once more and may call `CharsetReader` again.
- `mime.filename_conflict` is recorded only by `Entity.FilenameCandidates`.
  `mail.Reader` hands out headers, not entities, so a caller of
  `AttachmentHeader.Filename` sees a conflict only by comparing the
  candidates of `FilenameCandidates` itself.
- `Header.ContentType` / `ContentDisposition` keep the legacy error. The doc
  comments in `parse.go` and the roadmap still describe ROB-050-001 only.
- `mime.parameter_missing_name`, `_missing_equals`, `_missing_value` and
  `_segment_mixed_encoding` are not emitted: malformed items stay under the
  field-level code, and mixed encoding is legal.
