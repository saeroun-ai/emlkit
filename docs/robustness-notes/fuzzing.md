# Fuzzing (ROB-130-001, ROB-130-002)

Native Go fuzz targets of the module, what each one holds its input to, and
how to run one on this host. Every target reads a finite, capped input; none
replaces the table tests of its area, which pin exact results.

## The invariants

ROB-130-002 names five kinds of property. A target asserts only those the
documentation of its API promises; where a promise has a documented exception
the target leaves that case out and says so.

| Kind | What it means here |
|---|---|
| Progress | The call returns on every input. The inputs are capped and every reader is a finite in-memory source, so a target that does not end is a hang; the bounded runner's deadline is what catches it. |
| Determinism | The same input read twice gives deeply equal results and diagnostics. |
| Chunk invariance | A reader gives the same outcome whether its input arrives whole, a byte at a time, seven bytes at a time, or with `io.EOF` on its last bytes. |
| Bounded resources | Output is at most a small constant times the input plus a constant; diagnostics and entries stay within their documented caps. The value targets measure a result's footprint: the bytes of its distinct strings (a string shared by several fields counts once) and the elements of its slices and maps, at most 32 per input byte plus 64 KiB. |
| Stable diagnostics | Every code is one `diagnostic_code.go` declares, with a known severity and a message, and sits where the API says (field, location, path). |
| Raw preservation | What the API promises to keep byte for byte is the input's own bytes. |

## Targets

### Root package `message`

| Target | File | Reads | Asserts |
|---|---|---|---|
| `FuzzParse_heal` | `fuzz_heal_test.go` | a whole message, ≤ 16 KiB, every body and embedded message, under Tolerant and Forensic, and Strict | no panic; the tree, the diagnostic codes in order and the completion state the same whole and a byte at a time; completion counters that add up; Strict ends cleanly |
| `FuzzTolerantDecoders` | `fuzz_heal_test.go` | ≤ 16 KiB through the base64, quoted-printable and uuencode readers | ends; chunk invariance; at most 1.5 times the input plus 2 bytes out |
| `FuzzMIMEParameters` | `fuzz_values_test.go` | a Content-Type and a Content-Disposition value, together ≤ 8 KiB: `ParseMediaTypeTolerant`, `ParseContentDispositionTolerant`, `ParseParameterCandidates`, and `Header.FilenameCandidates` over both | determinism; candidates independent of the case of the field name and nil for another field; an empty value reads to no parameters, diagnostics or candidates; a Content-Type is always a type and a subtype; diagnostics declared, each naming the field, with no location and no path, and no code twice (one diagnostic per code per field); a candidate name lower-case without an RFC 2231 suffix; a candidate's `Raw` is in the field (except a continued series, whose segments are joined); a value is UTF-8 unless `Undecoded` (the boundary, which is never rewritten, excepted); the map holds exactly the chosen candidates, one per name; filename candidates in the documented order with the one to use first, each from the right parameter and form, and each passed through the `SafeFilename` checks below; bounded footprint |
| `FuzzSafeFilename` | `fuzz_values_test.go` | a name ≤ 8 KiB | the result is never empty, never only dots, holds no `/ \ < > : " \| ? *`, no control, format or non-space white space character, is valid UTF-8, at most 255 bytes, has no leading space and no trailing space or dot, has no Windows device stem, and `SafeFilename` is idempotent |
| `FuzzDecodeHeaderText` | `fuzz_values_test.go` | header text ≤ 8 KiB through `DecodeHeaderText`, `DecodeHeaderTextWithOptions` (fallback iso-8859-1), `Header.Text`'s decoding and the parameter-value decoding | determinism; valid UTF-8; no CR, LF or NUL the value did not hold; valid UTF-8 without `=?` returned as it is, with no diagnostic when it is ASCII, and never converted by a fallback; `Header.Text` decodes to the same string; at most three bytes out per byte in plus 16; at most 64 diagnostics, the 64th `limit.diagnostic_count`, each declared, with no field and its location within the value; a parameter value without `=?` passed on byte for byte |
| `FuzzParseContentTypeRepair` | `metadata_repair_test.go` | a Content-Type value through every profile | a conformant value read as it always was; any other read to a valid base naming only its own parameters; Strict refuses exactly what Tolerant repairs (ROB-050, not ROB-130) |
| `FuzzParse_firstLine` | `framing_firstline_test.go` | ≤ 4 KiB root framing | chunk invariance; the prefix is the input's own bytes; suffix equivalence; Strict and Tolerant agree on the entity |
| `FuzzParse_orphanContinuation` | `framing_orphan_test.go` | ≤ 4 KiB root framing | chunk invariance; records are the input's bytes at their location; Strict and Tolerant agree; records survive write and read back |
| `FuzzParse_mboxEnvelope`, `FuzzMboxEnvelopeLen` | `framing_mbox_test.go`, `mbox_internal_test.go` | ≤ 4 KiB / 2 KiB | the envelope matches the reference grammar; the root without it is the root of what follows; chunk invariance; the recognizer reads no more than it needs |
| `FuzzParse_trailing`, `FuzzTrailingEvidence` | `framing_trailing_test.go`, `trailing_internal_test.go` | ≤ 16 KiB / 8 KiB | framing and trailing input the same under both profiles and deliveries; trailing input is the input's own tail; the classifier matches its reference under any chunking |
| `FuzzParse_inputBytes` | `limit_input_test.go` | input budgets | the budget stop exactly where the budget is crossed, chunk invariant for traversals the decoders did not end (malformed transfer decoding is issue #34, ROB-070) |
| `FuzzParse_bodyBudgets` | `limit_body_test.go` | small body budgets | no leaf over its budget, the leaves within the total, a failure repeated identically |
| `FuzzParse_totalHeaders` | `limit_header_total_test.go` | generated valid trees | the exact stop and diagnostic the fixture predicts under either total, across line endings and chunkings |

### `textproto`

| Target | File | Reads | Asserts |
|---|---|---|---|
| `FuzzReadHeaderRecover` | `fuzz_recover_test.go` | a block ≤ 16 KiB through `ReadHeaderRecover`, under generous budgets and under tight ones (1 KiB, 256-byte fields, 16 fields, 128-byte lines) | ends; the fields, line endings, mark, prefix, records, repairs, unread bytes, what is left in the reader and the error are the same whole, a byte at a time, seven bytes at a time and with `io.EOF` on the last bytes (always through the default 4096-byte buffer: the decision may depend on a smaller one, as `recover.go` says); the unread bytes are exactly the last bytes consumed, returned only when the block ended at a body line, at the place `BodySeparatorMissing` reports; every record is the input's bytes inside the block; written back, a complete CRLF block is unchanged and one that ended at a body line is that block plus the empty line it lacked — blocks with other line endings (stored as CRLF, ROB-020-007) and with a line of empty name (dropped, ROB-020-006) are left out, as `header-recovery.md` documents; read back, the written header has the same fields and writes the same bytes (for a header under the 4096-byte recovery window) |
| `FuzzReadHeader_lineEndings` | `line_endings_test.go` | a block ≤ 8 KiB through `ReadHeader` | one outcome under seven deliveries and two buffer sizes; prefix and orphan records are the input's bytes and survive write and read back |
| `FuzzMultipartReader_lineEndings` | `multipart_lineend_test.go` | a multipart body ≤ 8 KiB | one outcome whole, a byte or seven at a time, and with `io.EOF` on the last bytes, under no line budget and a small one |
| `FuzzMultipartReader_malformedPrefixDelimiter` | `prefix_test.go` | a line after a part header's malformed prefix | a missing separator reported exactly for a whole delimiter, against an independent reading; chunk invariance |

### `mail`

| Target | File | Reads | Asserts |
|---|---|---|---|
| `FuzzTolerantHeaderValues` | `fuzz_values_test.go` | a field value ≤ 8 KiB through `ParseAddressListTolerant` (both option sets), `ParseDateTolerant`, `ParseMsgIDListTolerant`, `ParseReceived`, `ParseListURLs`, `ParseListID`, `ParseAuthenticationResults`, and the `Header` accessors | everything `TestTolerantAccessors_arbitraryBytes` checks (raw value kept, entries are parts of the value, the 16384-entry and 64-diagnostic caps, agreement with the strict parsers and the `Header` accessors); determinism; bounded footprint; diagnostics declared, with a known severity, a message, no location, no path and no field (except the field name `ParseAuthenticationResults` always sets); Authentication-Results within 64 methods and 256 properties each, method, result and property names lower-cased, `Index`, `ReceivedAbove` and `Trusted` as documented under a `Received` field, trusted by its own identifier in any case exactly when it has one, and never trusted below a `Received` field with `MaxReceivedAbove` 0 |
| `FuzzReadTree` | `fuzz_tree_test.go` | a message ≤ 16 KiB, parsed and read whole by `ReadTree` with embedded messages descended into and 4 KiB of each leaf kept | the tree, its body and attachment views and both lists of diagnostics the same whole and a byte at a time; each child's path is its parent's plus its index and its parent is that node; a known role; `Filename` is the first candidate's; a container holds no data, size or digest; a leaf's data within the cap, and whole and matching size and digest unless truncated; a duplicate is an earlier leaf of the same non-empty content; a span lies within the input; `Attachments` returns only attachments and messages; `Resolve`, `RelatedRoot` and `ReportFields` do not fail |
| `FuzzHeaderCommentAccessors` | `header_comment_test.go` | a value in four comment-skipping fields | no panic; the header written back byte for byte |
| `FuzzSkipComment` | `header_comment_internal_test.go` | a value | `skipComment` matches its reference |

### `charset`

| Target | File | Reads | Asserts |
|---|---|---|---|
| `FuzzReader` | `fuzz_test.go` | a label ≤ 256 bytes and data ≤ 4 KiB, decoded in that charset and in twenty fixed ones (UTF-8, EUC-KR, CP949, ISO-2022-KR, Shift_JIS, EUC-JP, ISO-2022-JP, GB2312, GB18030, Big5, HZ, UTF-7, UTF-16, UTF-16LE, US-ASCII, ISO-8859-1, ISO-8859-9, windows-1252, KOI8-R, unknown-8bit) | `Lookup` deterministic, its error `Reader`'s, a match with an encoding and a lower-case name; each decoder ends, gives valid UTF-8, at most three bytes per byte in plus 16, and the same output whole and a byte at a time |
| `FuzzDecodeHeaderText_charsets` | `fuzz_test.go` | a label, data and raw text, each ≤ 4 KiB, with the plugin installed: the raw text, the data as a B word, a Q word and two B words split in the middle, the raw text with the label as fallback charset, and the data as a `text/plain` body in that charset | each decoding deterministic, valid UTF-8, no CR, LF or NUL the input did not hold, at most three bytes per byte in plus 16, at most 64 diagnostics; the parsed body and its diagnostics the same whole and a byte at a time |

`utf7` (IMAP mailbox names) and `internal/errgraph` have no fuzz target.

## Seeds

Each new target has a dozen or more seeds: conformant values, the damaged
forms its notes describe, and Korean, Japanese and Chinese text in UTF-8 and
in the legacy charsets (EUC-KR, ISO-2022-JP, Shift_JIS, GB2312, Big5, HZ).
Plain `go test` runs the seeds and every input saved under
`testdata/fuzz/<Target>/`; for each package that pass takes milliseconds.

## Running a target

On this host every Go command runs in the bounded runner (see CLAUDE.md), one
job at a time; agents working in parallel share a lock:

```bash
# the seeds and saved inputs of one target, as part of the normal suite
flock -w 540 /tmp/emlkit-qa.lock rtk proxy python3 scripts/qa-bounded.py --seconds 120 -- \
    go test -run '^FuzzReadHeaderRecover$' ./textproto/

# fuzzing: one target per run
flock -w 540 /tmp/emlkit-qa.lock rtk proxy python3 scripts/qa-bounded.py --seconds 90 -- \
    go test -run '^$' -fuzz '^FuzzReadHeaderRecover$' -fuzztime 15s -parallel 2 ./textproto/
```

The fuzzer keeps what it learns in `$GOCACHE/fuzz/<module path>/<Target>`, and
writes a failing input to the package's `testdata/fuzz/<Target>/`, where it
becomes a permanent regression input; commit it with the fix. A new target
must cap its input (these use 4 to 16 KiB) and bound its own work, and read
only finite sources.

Under the runner's two-core quota the fuzzer's progress line can show
`0/sec` for several seconds. For `FuzzMIMEParameters` and `FuzzReader`,
every input the cache had collected was run again on its own and took well
under a millisecond, so those pauses are the host, not a slow input.

## What the runs found

Each new target was fuzzed for 15 seconds with two workers.

- **`mail.ReadTree` classified an unframed multipart by whether it kept data
  (fixed).** `FuzzReadTree`, input
  `mail/testdata/fuzz/FuzzReadTree/2731b81b86c2673e` (`Content-TYpe:multipArt`).
  A multipart that kept no usable boundary is read as one opaque leaf, but
  `roleOf` told a framed multipart from it by `Data == nil`. When the leaf's
  `Data` was empty — an empty body, `MaxBodyBytes` below zero, or a `Body`
  hook such as `Spool.Body` — it became a `RoleContainer` with no children,
  so its content was in neither `Bodies` nor `Attachments`, unlike
  `mail.Reader`, which hands it out as an attachment. A framed multipart is
  now told by having no digest: every leaf has one. Regression test:
  `TestReadTree_unframedMultipartIsALeaf`.

No other target failed. Findings that are questions rather than bugs:

- `Node.Bodies(prefer...)` lower-cases and trims the caller's `prefer` slice
  in place, so `Bodies(prefs...)` changes `prefs`.
- `ParseAuthenticationResults` reads an authserv-id written as `""` as an
  empty `AuthServID` with no diagnostic. Such a field is never trusted, since
  trust needs a non-empty identifier.
