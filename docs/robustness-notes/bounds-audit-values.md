# Bounds audit: value parsers and upper layers (ROB-120-001, part B)

Scope: in the root package `rfc2047.go`, `rfc2231.go`, `mediatype_tolerant.go`,
`filename.go`, `charset.go`, `charset_monitor.go`, `effective.go`, `header.go`,
`diagnostic*.go`, `writer.go`; every non-test file of `mail/`, `charset/`,
`utf7/` and `internal/`. The wire format and the framing code (`textproto/`,
`entity.go`, `multipart*.go`, `cte_tolerant.go`, `encoding.go`, `limit_*.go`)
are part A and are not covered here.

Tests: `bounds_values_test.go`, `mail/bv_bounds_test.go`, and the gated
`mail/bv_tree_open_test.go` for what is left open. Audited at `e58d480`,
2026-11-11.

## What was asked of each site

Every place where a value that comes from the input — a length, an offset, a
count, a parsed number, a decoded rune, a table index, a limit from options —
is used as a slice bound, in arithmetic, in a conversion between integer
types, or as an allocation size, was read for four questions: can it go out
of range, can it overflow or go negative (with `int` 32 bits wide), can it
allocate without a budget, and can N bytes of input become much more than a
small multiple of N bytes of output, retained memory or work.

## Defects found

| # | Site | Defect | Status |
| --- | --- | --- | --- |
| 1 | `mail/address_tolerant.go: interpretBare` | The bound of 16384 entries was checked between list elements only. Addresses that no comma separates (`a@b a@b a@b …`) are one element, and every one of them became an entry: 100 000 entries from 400 KB, about 130 retained bytes for each 4 of input. | Fixed: the bound is checked inside the run, the rest of the run is not parsed, and `address.partial_list` (limit_exceeded) is reported once. |
| 2 | `mail/msgid_tolerant.go: result` | Entries in angle brackets and the bare words read in their place were each bounded by 16384 and then appended: up to 32768 entries where 16384 are documented. | Fixed: the bare words appended are those that still fit; `message_id.partial_list` (limit_exceeded) reports the rest. |
| 3 | `charset_monitor.go: utf8Monitor.validate` | A sequence a read had cut was judged by the rune alone. `utf8.DecodeRune` returns U+FFFD both for a decoding error (width 1) and for an encoded U+FFFD (width 3), so a valid U+FFFD that straddled two reads was reported as `charset.invalid_sequence`. | Fixed: the width decides. |
| 4 | `mail/tree.go: treeBuilder.read` | **Hang.** When the source fails inside a multipart, the iterator returns no part and a newly wrapped error on every call; the loop ends only on two identical error values, so it spins for ever. Reproduced under `Parse` and under `Read` with a 45-byte message whose reader fails instead of ending. | **Open** — the file was not this audit's to change. See below. |
| 5 | `mail/spool.go: Spool.Body` | `io.LimitReader(r, memRoom+1)` overflows for `SpoolOptions.MemoryBytes == math.MaxInt64`: the limit is negative, nothing is read, and every body is kept as empty with `cut == false`. | **Open**, same reason. |
| 6 | `mail/tree.go: roleOf`, `underRoot`, `RelatedRoot` | Quadratic in the parts of one multipart: `indexOf` scans the siblings for every leaf, and under a `multipart/related` whose `start` names no part (or the last one) `RelatedRoot` normalizes every sibling's Content-ID for every leaf. Measured: 2000 parts 62 ms, 4000 parts 235 ms, 8000 parts 899 ms (a 432 KB message); about 1.4 s at the default `MaxPartCount` of 10000, and minutes if that budget is raised or the entity came from `Read`, which has none. | **Open**, same reason. |

### Suggested fixes for the open ones

- **4.** In the `p == nil` branch of `treeBuilder.read`, count the calls that
  returned no part and stop at a cap — the parse's `MaxPartCount`, or 10000
  for an entity with no parse. A refused part legitimately follows another
  (under `ProfileStrict`, or a malformed part header under `Read`), so
  "stop at the first" is wrong, and the error's text must not be compared
  (`Error` of a foreign error is never called here); a count is enough,
  since each legitimate refusal consumed a part. Independently, the multipart
  iterator could return the identical value for a source failure it has
  already reported, which is what `mail.Reader.NextPart` documents ("later
  calls report the failure") and what `sameErr` assumes; that is part A's
  file.
- **5.** `memRoom = min(memRoom, math.MaxInt64-1)` before the `LimitReader`.
- **6.** Give `classify` the child's index instead of searching for it (or
  search only under signed, encrypted and report parents), and compute a
  related container's root once per container rather than once per leaf.

`EMLKIT_BV_OPEN=1 go test -run TestBVOpen ./mail/` runs the reproducers of 4
and 5; they are skipped otherwise, because 4 fails by never returning.

## Sites reviewed

"Safe" means the value cannot leave its range; "bounded" names what bounds
it. Sizes are worst cases per input byte.

### Root package

| Site | Input-derived value | Verdict |
| --- | --- | --- |
| `rfc2047.go: finder.next` | search position | Safe: clamped to `len(s)`; positions only move forward, so all searches together are linear. |
| `parseWord` | label length, offsets `j+1`, `j+2`, `ts`, `te` | Safe: `j+2 >= len(s)` is checked before any of them is read; the label stops at 128 bytes. |
| `noteWord` | `s[w.start+len(w.label)+3]` | Safe: it is the encoding letter `parseWord` already read. |
| `countQuotes`, `settle` | offsets, diagnostic indices | Safe: indices come from `note`, which returns -1 once the budget of 64 is spent. |
| `decodeBWord` | `len(text)/4*3+3`, 6-bit accumulator in a `uint32` | Safe: at most 13 bits are ever held; the capacity is a hint. |
| `decodeQWord`, `unhex`, `rfc2231.go: percentDecode` | `i+2 < len` before both hex digits | Safe. An escape the end cuts is literal (tested). |
| `convert` (iso-8859-1), `readConverted` | `len+len/2`, `in+in/2+16` as capacities | Safe in practice; see "32-bit". The read limit `6*in+1024` is computed with an overflow guard. |
| `readConverted` | byte count a hook's reader returns | Safe: `n < 0` and `n` past the buffer are refused before the slice is extended. |
| `wordScan.decode` (whole) | output size | Bounded: at most 3 bytes out per byte in without a hook (U+FFFD for an invalid byte or a NUL); with a hook 6× plus 1 KiB per run, 10000 words per value. 64 diagnostics per value. |
| `rfc2231.go: splitParameterName` | section number | Safe: at most 9 digits, so below 2^31; wider is not a section (tested at 2^31, 2^32, 2^64, -1). |
| `splitExtendedValue`, `assemblePieces`, `orderSegments` | quote offsets, segment order | Safe; `ordered[:1]` is reached only with two segments or more. Segments are bounded by `MaxParameterSegments`. |
| `metadataDecoder.convertCharset` | `int64(len)*8+64` read limit; `stalled` map keyed by the value | Bounded: `io.ReadAll` over a `LimitReader`; the map gains an entry only for a value whose decoder stalled. |
| `mediatype_tolerant.go: readTolerant` | `raw[:scan.head]`, `raw[occ.start:occ.end]`; maps keyed by parameter name | Offsets are the scan's (part A). Maps and candidates are bounded by `MaxParameterCount`; diagnostics name at most 8 parameters each. |
| `readParameterValue` | `val[0]`, `val[:q]`, `rest[len(rest)-1]`, `val[1:end+len(rest)]` | Safe: `val` is never empty; `q` is read only when a quote is known to be there (`hasUnclosedQuote`); `rest` is not empty where it is indexed. |
| `closingQuote`, `escapedClosingQuote`, `unquote`, `quotedLiteralBackslash` | `i+1`, `i+2` | Safe. `escapedClosingQuote` trims the remainder for every `\"`, which is linear only because the item was trimmed first; measured linear to 64 KiB. |
| `healBase`, `healMediaType`, `healDisposition` | `s[1:n-1]`, `after[1:]` | Safe: guarded by `n > 2` and `HasPrefix`. |
| `filename.go: SafeFilename`, `truncateFilename` | `s[keep]`, `max-len(ext)` | Safe: the extension kept is at most 32 bytes, so `keep >= 223`; output is never longer than its input and at most 255 bytes. |
| `filenameCandidates`, `filenameConflictDiagnostic` | `out[1:pick+1]`, `names[0]` | Safe. |
| `charset.go`, `effective.go`, `header.go`, `writer.go` | none used as a bound | Nothing to bound: they pass strings and readers on. |
| `charset_monitor.go: utf8Monitor` | `p[:min(n, len(p))]`, `carry[carryLen]` | Safe: `carryLen` is at most 3 when it is written. Defect 3 was here. |
| `fallbackReader.decide` | 4096-byte look-ahead | Bounded: one fixed buffer per text body, only with a fallback charset. |
| `diagnostic.go`, `diagnostic_code.go` | `c[:i]`, `Path` copies | Safe. A path is as long as the nesting, which `MaxMIMEDepth` bounds. |

### `mail/`

| Site | Input-derived value | Verdict |
| --- | --- | --- |
| `tolerant.go: scanComment`, `scanQuoted`, `unescape`, `commentText` | index after a backslash | Safe: the loop condition is rechecked; an unclosed construct returns `len(s)`. |
| `quoteForMessage` | fragment in a message | Bounded: 64 bytes before quoting. |
| `diagnosticList.add` | count | Bounded: 64, the last a `limit.diagnostic_count`. |
| `address_tolerant.go: tokenizeAddresses` | `int32` offsets; one token per byte | Safe: a value over `math.MaxInt32` bytes is refused first. 12 bytes of token per input byte, transient. |
| `scanAddrAtom`, `scanEncodedWord`, `scanDomainLiteral` | `i += 2` after a backslash | Safe: `min(i, len(s))`; every scan makes progress. |
| `parse`, `parseEntry`, `mergeAhead`, `openGroup` | token indices, group depth | Safe: `next <= i` is forced forward; the look-ahead is memoized, so linear. |
| `interpretAngle`, `addrSpec`, `localPart`, `phrase` | `toks[k:closing]`, `text[1:len-1]` | Safe: a quoted token always has both quotes. |
| `interpretBare` | entries per run | **Fixed** (defect 1). |
| `date_tolerant.go` (whole) | day, month, year, clock, zone | Safe: `smallNumber` refuses more digits than the field has (2, or 4 for the year) before converting, so nothing overflows; hour, minute, second and zone are range-checked; the day is checked against the month. 12 words are examined. Tested at each end. |
| `formatZoneOffset` | `byte('0'+h/10)` | Safe: `h <= 23`. |
| `msgid_tolerant.go: parse`, `bracketed` | offsets, entries | Safe; searches for `>` stop at the next `<`, so they do not overlap. |
| `result` | entries plus bare words | **Fixed** (defect 2). |
| `list.go: parse`, `bracketed`, `entry`, `parseListID` | offsets, entries, `lastWord[0]` | Safe; 16384 entries. |
| `received.go: parseReceived`, `parseReceivedClauses` | `i += 2`, `v[:semi]`, clauses | Safe; 16384 clauses. Comments of one clause are not counted: 16 bytes retained per `()`, 8× (left as is). |
| `authres.go: splitAuthSegments`, `authPairs`, `ParseAuthenticationResults` | segments, pairs, `head[0]` | Safe: 64 methods, 256 pairs each, comment depth capped at 64. All segments are split before the method bound applies: 32 bytes per `;`, transient (left as is). |
| `header.go: headerParser` | `p.s[0]`, `p.s[i:]` | Safe: `peek` is called behind `empty`. The strict accessors quote the whole remaining value in an error (`%q`, up to 4×), as they always did. |
| `MsgIDList`, `ListCommandURLList` | loop until empty | Safe: every iteration consumes a byte or fails. |
| `reader.go`, `writer.go`, `address.go`, `attachment.go`, `inline.go` | none | Nothing to bound. |
| `tree.go: read` (leaf) | `chunk[:min(int64(m), room)]`, `MaxBodyBytes`, `MaxTotalBytes` | Safe: `room > 0` and at most `m`. A negative budget keeps nothing. Allocates a 32 KiB buffer per leaf, which is churn, not retention. |
| `tree.go: read` (multipart loop) | calls that return no part | **Open** (defect 4). |
| `tree.go: classify`, `roleOf` | sibling scans | **Open** (defect 6). |
| `tree.go: shape` | `%q` of the `protocol` and `report-type` parameters | Bounded by the field (up to 4×); one diagnostic per node, with no count limit of its own. |
| `tree.go: Bodies` | recursion over alternatives | Bounded: quadratic in the nesting depth, which `MaxMIMEDepth` (100) bounds. |
| `tree.go: ReportFields`, `normalizeCID` | groups, `s[:4]` | Safe: 1024 groups, each header read under 1 MiB and 4096 fields. |
| `spool.go: Body` | `memRoom+1` | **Open** (defect 5). |
| `spool.go: copyLimited` | `chunk[:max(room, 0)]` | Safe. |

### `charset/`, `utf7/`, `internal/errgraph`

| Site | Input-derived value | Verdict |
| --- | --- | --- |
| `charset.go: Lookup`, `cleanLabel` | `label[:128]`, `rest[0]` | Safe; four cleaning passes at most. |
| `stallGuard` | empty reads | Bounded: 65536. |
| `decoders.go: widenedDecoder`, `unknown8bitDecoder` | `nDst+RuneLen(r)` | Safe: every byte decodes to a defined rune, so `RuneLen` is never -1. 3× at most. |
| `utf16AutoDecoder`, `utf16Endianness` | `src[:min(len, 128)]`, `[256]bool` indexed by a byte | Safe. |
| `iso2022.go: jisToEUC`, `iso2022KRToEUC`, `hzToEUC`, `jisEscape` | `src[nSrc+1]`, `rest[0]`, `b[3]` | Safe: each read is behind a length check, with `ErrShortSrc` when more may come. A state change emits nothing; 3× at most after the EUC step. |
| `utf7.go: Transform`, `decodeUTF7Run`, `utf7MaxOut` | run length, `bits>>nbits`, writes into `dst[n:]` | Safe: a run is at most 256 characters per step; `utf7MaxOut` covers a pending surrogate and a final partial unit, and a failed run kept as text (`n+1 <= utf7MaxOut(n)`). |
| `utf7/decoder.go: decode` | `b64[n-1]`, `n+DecodedLen(n)*3`, `b[i+1]` | Safe: the run is never empty; an odd length is refused before pairs are read. |
| `utf7/encoder.go`, `charset/utf7.go` encoders | `n -= 2 - (len(b)+2)%3` | Safe; not reached from parsing. |
| `errgraph.Find` | queue index, node, pending and slot counts | Safe: fixed array, three caps. |

## 32-bit

Run through the bounded runner on x86-64 Linux, which executes 386 binaries:

- `GOARCH=386 go build ./...` — builds.
- `GOARCH=386 go test ./mail/ ./charset/ ./utf7/ ./internal/...` — all pass.
- `GOARCH=386 go test -run TestBV .` — the root package's test binary builds
  for 386 and the new boundary tests pass. The rest of the root package's
  tests, and `textproto/`, were not run on 386 here; they are part A's.

Nothing in scope depends on `int` being 64 bits wide. The places that would
are guarded: address token offsets are `int32` behind a length check, an RFC
2231 section has at most nine digits, every date number is refused by its
digit count before `strconv.Atoi` sees it, and offsets in diagnostics are
`int64`.

One theoretical case is left: `convert` and `readConverted` in `rfc2047.go`
size a buffer as `len + len/2` (+16), which overflows a 32-bit `int` for an
encoded-word run over 1.43 GiB and would panic in `make`. No parse hands over
such a value (a header field is 64 KiB by default), and a 386 process cannot
hold the value, its decoded bytes and that buffer at once.

## Judged risky and left

- The open defects 4, 5 and 6 above.
- `mail.decodeWord` (display names, List-Id) decodes through
  `mime.WordDecoder`, which reads a `CharsetReader`'s output without the
  expansion cap and the no-progress guard the root package's decoder has. The
  charset plugin's readers are bounded on their own; a hostile hook is not
  input.
- Constant-factor amplification that is linear and transient, and so was not
  capped: 12 bytes of token per byte of an address list, 32 bytes per `;` of
  an Authentication-Results value, 16 bytes per word or comment of a Received
  clause.
- `Tree.Diagnostics` has no count bound of its own; it holds at most one
  diagnostic per node, and nodes are bounded by `MaxPartCount`.
- `ReadTree` recurses once per nesting level and allocates a 32 KiB read
  buffer per leaf. Depth is bounded by `MaxMIMEDepth` and `MaxMessageDepth`
  under `Parse`; an entity from `Read` has neither bound.
