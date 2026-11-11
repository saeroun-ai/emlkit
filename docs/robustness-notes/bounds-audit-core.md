# Bounds audit: wire and framing code (ROB-120-001, part A)

Scope: every non-test file of `textproto/`, and in the root package
`entity.go`, `multipart.go`, `multipart_heal.go`, `cte_tolerant.go`,
`encoding.go`, `header_recover.go`, `embedded.go`, `completion.go`,
`limit_*.go`, `limits.go`, `progress.go`, `trailing.go`, `mbox.go`,
`origin*.go`, `session.go`, `firstline.go`, `orphan.go`, `line_endings.go`.
The value parsers (`rfc2047.go`, `rfc2231.go`, `mediatype_tolerant.go`,
`filename.go`, `charset*.go`, `effective.go`) and `mail/`, `charset/`, `utf7/`
are part B and are not covered here.

Code: `bounds_core.go`, and one-line guards in `entity.go`, `progress.go`,
`origin.go`, `textproto/multipart.go`, `textproto/header.go`. Tests:
`bounds_core_test.go`, `textproto/bounds_core_test.go`.

## Question asked of each site

For every value that comes from the input (a length, an offset, a count, a
parsed number) or from the caller's options (a budget), where it is used as a
slice index or bound, in arithmetic, in a conversion between `int`, `int64`,
`uint`, `uint32`, or as an allocation size: can it go out of range, overflow
(with a 32-bit `int`, GOARCH=386, in mind), go negative, or allocate without a
budget?

How it was looked at: the scanners and readers were read line by line
(`textproto/header.go` from `headerField` to `WriteHeader`,
`textproto/multipart.go`, `orphan.go` from `settleCR`, `prefix.go`,
`recover.go`, `header_budget.go`, `line_endings.go`, `bom.go`; root
`cte_tolerant.go`, `encoding.go`, `mbox.go`, `trailing.go` from the tail
reader on, `limit_line_bytes.go`, `limit_body.go`, `limit_input.go`,
`progress.go`, `multipart.go`, `multipart_heal.go`, `limit_parameters.go`,
`origin.go`, and the read path, `MultipartReader` and `Walk` of `entity.go`).
The rest of the scope (`session.go`, `completion.go`, `header_recover.go`,
`firstline.go`, `orphan.go`, `line_endings.go`, `limit_header*.go`,
`limit_tree.go`, `limit_diagnostics.go`, `limits.go`, `origin_public.go`,
`textproto/limits.go`, and the parts of `entity.go` that build and write
entities) was searched for every integer conversion, every index or slice
expression with arithmetic in it, every `make`, `append` in a loop, `Grow`,
`ReadAll`, `Repeat`, and every `++`/`+=` on a field, and each hit was read in
its function.

## Defects found and fixed

| # | Site | Defect | Fix |
| --- | --- | --- | --- |
| 1 | `progress.go: progressReader.Read`, `origin.go: countingReader.Read`, `textproto/multipart.go: stickyErrorReader.Read` | A caller's reader that reports a count below zero or above `len(p)` reached `bufio.Reader` with it: a negative count panics in bufio ("reader returned negative count"), one too large panics later with a slice bound out of range. The roadmap had assigned this to ROB-120-001. Reachable from `Parse`, `Read`, `ReadWithOptions`, an embedded message's header read and `textproto.NewMultipartReader`. | The count is held to 0..`len(p)` at the three places a caller's reader sits directly under a buffered reader of this module (`baClampCount`; `max(0, min(n, len(p)))` in textproto). Below zero is no bytes — with a nil error it then counts toward the no-progress rule of a parse; above the room is the room, the most the reader can have written. Errors are passed on unchanged. |
| 2 | `entity.go: Entity.MultipartReader` → `textproto.MultipartReader.SetPreambleLimit` | `Limits.MaxPreambleBytes: LimitDisabled` reached textproto as -1, which that layer reads as "its own 1 MiB default", not "no limit". A disabled budget therefore cut the preamble at 1 MiB and recorded `limit.preamble_bytes` naming a limit of 1048576 the caller had turned off. (The header budgets are passed down as they are because for them textproto does read zero or below as no budget; the preamble limit is the one place the two spellings differ.) | `baPreambleLimit`: a disabled budget is passed as `math.MaxInt64`, which `keepPreamble` compares with the room left and never sums. |
| 3 | `textproto/header.go: Header.Del` | Removed the fields of a key one at a time, each removal moving every field after it: the square of the header for N fields of the key that follow N others in the block. `Header.Set` calls it. Under `Parse` the default `MaxHeaderCount` of 1024 hides it; under `Read`, whose only budget is 1 MiB of header, a block of about 170k + 170k three-byte fields made one `Set` move on the order of 240 GB. Not a bound or an overflow, but found while reading for them, and a hang by any measure. | One pass. The array is left exactly as the old loop left it, slot for slot (a `Header` value copied before the call shares it, and `multipartBody.Read` relies on that), which `TestBoundsCore_delMatchesOneAtATime` checks against the old loop. |

## Sites reviewed

Verdict "safe" means: cannot go out of range, overflow, go negative or
allocate past a budget, on a 64-bit or a 32-bit `int`, for the reason given.

### textproto

| Site | Input-derived value | Verdict |
| --- | --- | --- |
| `header.go: headerScan.charge`, `take`, `rejoin`, `satAdd` | line, field, block and shared byte counts (`int64`) against budgets up to `math.MaxInt64` | safe: every check compares a charge with the room left (`limit - used`), never a sum; `rejoin` adds with saturation |
| `header.go: nextChunk` | indexes `i`, `i+1`, `i+2` into the peeked window or the replay; `min(len(rp), r.Size()+1)` | safe: each index is checked against the window's length before use; chunks never exceed one buffer |
| `header.go: readLineSlice` | `b[:len(b)-term]`, `len(b)-1` | safe: `term` is the line ending `nextChunk` just reported at the end of `b` |
| `header.go: readContinuedLineSlice`, `readFields` | growth of `line`, `kv`, `capture`, `orphans`, `fs` | bounded by the header budgets (charged before bytes are kept, so at most one buffer past a budget); with no budget (`ReadHeader`) unbounded as documented there |
| `header.go: readFields` (`scan.consumed -= int64(len(raw))`) | length of the handed-back line | safe: those bytes were charged to `consumed` when read |
| `header.go: pendingField.join`, `dropLastLine`, `field` | `len(p.kv)-2`, `lastLine`, `rawLast`, `valueStart` | safe: `kv` always ends with the "\r\n" `readContinuedLineSlice` appended; the offsets were taken from the same slices |
| `header.go: classifyCandidate`, `damageError`, `trim` | `kv[:i]` | safe: `i` is an `IndexByte` result checked for -1 |
| `header.go: vpeek` | `n` (1 or 2), `vbuf [2]byte` | safe: the replay is shorter than `n` on that path, so at most 2 bytes are appended |
| `header.go: AddRaw` | `kv[:colon]`, `kv[colon+1:]` | safe: panics by contract on a value with no colon (a caller's value, documented); not reached from parsing |
| `header.go: headerFields.index`, `headerFieldsByKey.index` | cursor arithmetic | safe: guarded; out-of-order use panics by documented contract (caller misuse, not input) |
| `header.go: foldLine`, `formatHeaderField` | `maxlen`, `foldAt-1`, key longer than 998 | safe: `maxlen` clamped to 1, `foldAt >= 1` wherever `foldAt-1` is read; the first-line clamp can emit one empty fold but the loop always advances after it |
| `header.go: Header.Del` | number of fields | **fixed (3)** |
| `header.go: Header.Values`, `Copy`, `Map`, `makeHeaderMap` | `make` sized by field count | safe: sized by fields already held in memory |
| `multipart.go: match`, `matchTail` | `c+elen`, `head+len(db)`, `k`, `k+1` | safe: `len(buf)-head < len(db)` is checked first; every later index is compared with `len(buf)` before use |
| `multipart.go: scanBody` | `j-1`, `j-2`, `q-1`, `q+1`, `q+2`, `max(0, len(buf)-len(db)-2)` | safe: each guarded (`j >= 2`, `q > 0`, `q+1 < len(buf)`); a boundary longer than the buffer can never match and is never indexed (test: `boundaryLongerThanBuffer`) |
| `multipart.go: decide`, `scanHeld`, `checkLine` | `m.end-m.endLen-(c+elen)`, `held+m.k` as `int64` against `MaxLineBytes` or the buffer size | safe: values are within one buffer plus what is held; compared as `int64`, no narrowing |
| `multipart.go: heldCandidate.held`, `MultipartReader.out` | bytes of one framing line held outside the buffer | bounded by `MaxLineBytes`, or by one buffer with no line budget |
| `multipart.go: partReader.Read` | `p.n` (`int`), `p.total` (`int64`) | safe: `p.n` is at most what the buffer holds; `bufio.Read` returns no more than asked |
| `multipart.go: offset`, `windowAt` | `src.n` less buffered, held and restored bytes | safe: all are bytes already counted in `src.n` |
| `multipart.go: restore`, `orphan.go: restoreReader` | `make(len(held)+n+len(pre))`; buffer size doubled, clamped at `math.MaxInt/2` | safe: sizes are bytes in hand; a restore happens only after a proof past the whole buffer, the proof is bounded by the header budgets or `crLegacyCap` (64 KiB), so the doubling stops there |
| `multipart.go: readFramingLine` | `budget-data`, `lineBuf` growth, `i+elen`, `n-1` | safe: charge compared with room left; with no budget a line is one buffer |
| `multipart.go: keepPreamble` | `limit - len(preamble)`, `line[:min(len(line), room)]` | safe: `preamble` never passes `limit`, so `room >= 0`; `min` is taken in `int64` and is at most `len(line)` |
| `multipart.go: SetPreambleLimit` | caller's limit; zero or below means the 1 MiB default | safe here; the root package passed `LimitDisabled` into it — **fixed (2)** |
| `multipart.go: partsRead`, `MultipartFraming.Parts` | `int` count of parts | safe in practice; see "Left" (a) |
| `multipart.go: stickyErrorReader.Read` | count reported by the caller's reader | **fixed (1)** |
| `multipart.go: isBoundaryDelimiterLine`, `delimiterLine` | `line[:len(line)-len(e)]`, `content[len(db):]` | safe: guarded by `HasSuffix` / `HasPrefix` |
| `multipart.go: MultipartWriter.SetBoundary`, `randomBoundary` | boundary length 1..70 | safe (writer side) |
| `orphan.go: settleCR` | `lineRoom`, `byteRoom` (`int64`), `i`, `seg`, `make(0, 2*i)` | safe: rooms are differences of a budget and a usage that never passes it, compared and never summed; `i` grows by one per byte looked at and is bounded by the rooms or `crLegacyCap`; `2*i` is at most twice the buffer size when the hold begins |
| `orphan.go: exact`, `termAt`, `noteTerm` | `size`, `i-len(termsFix)` | safe: `size` is at most `len(kv)`; `termAt` guards both ends |
| `orphan.go: prefixReader.Read`, `writeRawLines` | `p.b[n:]`, `b[len(b)-1]` | safe: `copy` result; guarded by `len(b) == 0` |
| `prefix.go: holdLimit`, `holdDelimiter` | `limit` (`int64`, up to `math.MaxInt64`), `room := limit - len(held)`, `b[:room]`, `Discard(int(room))`, `make(0, m.k+64)` | safe: `int(room)` is only taken when `room < i <= len(b)`; `m.k` is within one buffer; `held` never passes `limit` |
| `prefix.go: parentDelimiter` | `s.peek(r, n+1)` | safe: only while `n < r.Size()` |
| `prefix.go: unkeep`, `prefixOf` | `capture[:n]` | safe: guarded by `n < len(capture)` |
| `recover.go: knownField` | `lower [64]byte` indexed by position in the name | safe: the colon is at index 64 at most, so the name is 64 bytes at most |
| `recover.go: goesOn` | `pos+j`, `next` within the peeked window | safe: `j` is an index into `buf[pos:]`; window is `min(r.Size(), recoverWindow)` and at most `recoverLines` lines |
| `recover.go: unindented`, `receivedClause`, `parameterLike` | `value[len(value)-1]`, `line[:len(w)]` | safe: each guarded by a length check |
| `header_budget.go: chargeBytes`, `chargeField`, `roomBytes` | shared byte and field counts | safe: compared with the room left; counts never pass their budget |
| `line_endings.go: LineEndings.note`, `LineEnding.index` | `count [3]int64`, array index from an enum | safe: counts saturate; index range-checked |
| `bom.go: ByteOrderMark.Bytes`, `byteOrderMark` | `bomBytes[m]`, peeks of 1..4 bytes | safe: unsigned type, range-checked; every peek's length is checked |
| `limits.go` | enum to string and sentinel | safe: switches, no indexing |

### root package

| Site | Input-derived value | Verdict |
| --- | --- | --- |
| `entity.go: limitedReader.Read` | `p[0:lr.N]` with `N` an `int64` budget | safe: only when `len(p) > N`, so `N` fits an `int`; `N` only shrinks by counts now held to the contract |
| `entity.go: readWithOptions` (`bodyOffset`) | `cr.n - Buffered - len(unread)` | safe: all three are bytes counted in `cr.n` |
| `entity.go: ReadOptions.withDefaults` | negative `MaxHeaderBytes` → `math.MaxInt64` | safe: only compared and subtracted from |
| `entity.go: Entity.MultipartReader` | `MaxPreambleBytes` passed to textproto | **fixed (2)** |
| `entity.go: Entity.Walk` | `path[len(path)-1]`, `multipartReaders[len-1]`, per-part `make([]int, len(path))` | safe: the three stacks grow and shrink together; depth is bounded by `MaxMIMEDepth` under `Parse`, unbounded under `Read` as documented |
| `origin.go: entityOrigin.child`, `pathCopy` | `make(len(path)+1)`, offset sums | safe: path length is the depth; offsets are positions in the input |
| `origin.go: countingReader.Read` | count reported by the reader below | **fixed (1)** |
| `progress.go: progressReader.Read` | count reported by the caller's reader | **fixed (1)** |
| `progress.go: countedInput`, `metadataReader`, `bodyGuard.noProgress` | counts from a `CharsetReader`'s reader | safe: counted only when in `1..len(p)`; passed on, see "Left" (c) |
| `limit_input.go: inputLimiter.Read` | `p[:remaining]`, `admitted` | safe: `remaining` is only used as a bound when below `len(p)`; `admitted += min(n, len(p))` |
| `limit_body.go: rawBody.Read`, `bodyGuard.read` | `p[:remaining]`, `p[:window]`, `decoded - used`, `totalLeft` | safe: same pattern; `window` is the minimum of `len(p)` and the rooms, all `int64` |
| `limit_body.go: chargeDecoded`, `decodedState` | `limit - decodedUsed` | safe: usage is clamped to the limit; see "Left" (d) for concurrent readers |
| `limit_body.go: rawBody.isInputStop` | `level` against `ctx.depth` | safe: bounded by depth, one `Unwrap` of a standard join per level |
| `limit_line_bytes.go: cteLineMonitor.Read`, `inspect` | `p[:n]`, `pos - lineStart`, `int(keep)` | safe: `n` comes from this module's own readers, whose counts are in range; `keep` is at most `len(b)` when it is non-negative |
| `limit_parameters.go: parameterScanner.next`, `scanParametersPoll`, `parameterName`, `retainedParameters` | `s.i+1`, `raw[start:end]`, `attr[star+1:]`, `segments` map, `occurrences` | safe: every index guarded; map and slice bounded by `MaxParameterCount` (or by the field, which the header budgets bound) |
| `limit_parameters.go: oversizedField` | `int64(len(raw))` | safe |
| `limit_tree.go: chargePart`, `limit_diagnostics.go: claimDiagnosticSlot` | part and diagnostic counts (`int64`) | safe: `withinLimit` compares with the room left; the slot count stops at the budget |
| `limits.go: Limits.resolve`, `bridgeMaxHeaderBytes` | caller's budgets | safe: zero is the default, below `LimitDisabled` is rejected, `math.MaxInt64` is an ordinary budget |
| `cte_tolerant.go: tolerantBase64`, `tolerantQP`, `tolerantUU` (`Read`) | `n` from the reader below, `d.out` growth | safe: `n = max(0, min(n, len(d.in)))`; output per 4096-byte chunk is at most 3/4 of it (Base64), the chunk plus one held run of 1024 (QP), about 1.5 times it (uuencode) |
| `cte_tolerant.go: tolerantUU.endLine`, `uuWhole` | length character `n` (0..63), `(n+2)/3*4`, `len(data)*3/4`, `at(i)` | safe: `line` is capped at 256 bytes; `at` checks `i < len(data)`; a length character alone cannot stand for bytes the line does not hold |
| `cte_tolerant.go: cteNotes` | `1 << k` in a `uint32`, `at [cteNoteCount]int64` | safe: `k < cteNoteCount`, which is below 32 |
| `cte_tolerant.go: canonicalCTE`, `stripCTEComments` | header value length | safe: encoded-word decoding only for 256 bytes or fewer, comment stripping for 1024 or fewer |
| `cte_tolerant.go: sevenBitMonitor.Read` | `p[:min(n, len(p))]` | safe |
| `encoding.go: lenientQPReader`, `whitespaceReplacingReader` | `pending` growth, `p[i]` for `i < n` | safe: at most 6 bytes appended per input byte and drained each read; `n` comes from this module's own readers |
| `encoding.go: lineWrapper.Write`, `cutLine` | `maxLineLen - curLineLen` (may reach -1) | safe: a negative width cuts an empty line, which ends the line and resets the count (writer side) |
| `mbox.go: mboxEnvelopeLen`, `readMboxEnvelope` | `limit` (at most 998, or a smaller `MaxLineBytes`), `Peek(i+1)`, `held[5:n]` | safe: `int(l)` only when `l < 998`; looks at most `limit+2` bytes, within the 4096-byte buffer |
| `mbox.go: mboxDigits` | `v = v*10 + digit` over a token of up to ~990 digits | wraps silently on a long token, harmless: every caller also requires `len(s)` of 2 or 4 before trusting `v`, and Go integer overflow does not trap |
| `mbox.go: mboxEnvelopeText`, `mboxFields` | `t[i]`, `t[i+1]` | safe: `len(t) >= 6` and `i+1 < len(t)` checked |
| `trailing.go: noteTrailing`, `trailingClassifier` | `lineCap` (`int(l)` only when `l < 998`), `pos`, `name`, `line`, `off` | safe: the classifier looks at 4096 bytes at most and holds `lineCap` bytes at most; `c.line[5:]` only once 5 bytes matched |
| `trailing.go: trailingTail.Read` | `n` from the epilogue reader | safe: held to 0..`len(p)` |
| `multipart.go: multipartReader.nextPart` (`r.next`), `multipartBody.NextPart` | part index | safe: see "Left" (a) |
| `multipart_heal.go: inferBoundary`, `cutLineAt`, `fieldLine` | `stats` map, `line[2:]`, `name[:n-2]`, `after[:8]`, `pos`/`next` | safe: map capped at 64 candidates; each slice guarded by a length check; the scan is one pass over a body `MaxPreambleBytes` bounds |
| `multipart_heal.go: healEnd` | recursion through `nextPart` | safe: once, guarded by `inferred` |
| `multipart_heal.go: boundaryChain.has` | chain length | safe: walk capped at 1024 |
| `embedded.go: embeddedMessage` (`headerLen`) | `cr.n - Buffered - len(unread)` | safe: as `bodyOffset` |
| `completion.go: noteInput`, severity counters | `n` from the input, `severities[d.Severity]` | safe: `n` now in range; index guarded by `int(d.Severity) < len(...)` |
| `session.go: record`, `Diagnostics` | `diags` growth | bounded by `MaxDiagnosticCount` (1000), unbounded if the caller disables it |
| `header_recover.go`, `firstline.go`, `orphan.go`, `line_endings.go` | line counts, `math.MaxInt64` sort keys, `end-start` | safe: counts of lines held in memory; the sentinels are only compared |

## 32-bit results (GOARCH=386, on the x86-64 host)

- `go build ./...`: clean, before and after the changes.
- `go vet -composites=false -stdmethods=false ./...`: clean, before and after.
- `go test -timeout 170s ./textproto/ .`: both packages pass, before the
  changes and after them with the new tests. The host executes 386 test
  binaries.
- No 32-bit defect was found. The reason is structural rather than luck:
  every budget, offset and running count is an `int64`; a budget is compared
  with a length by widening the length (`int64(len(p)) > remaining`), and an
  `int64` is narrowed to `int` only on a branch that has just shown it to be
  smaller than an `int` length. `TestBoundsCore_hugeBudgets*` pin that with
  budgets of 2^31, just past 2^32, 2^40 and `math.MaxInt64` in every place a framing
  or header line is held outside the reader's buffer.

## Judged risky, and left

- (a) **`int` part counters on 32-bit.** `textproto.MultipartReader.partsRead`
  (`Framing().Parts`) and the root `multipartReader.next` (the path index) are
  `int`. On a 32-bit `int` they wrap after 2^31 parts, which needs an input of
  more than 8 GiB with `MaxInputBytes` and `MaxPartCount` both disabled. The
  result would be a negative path index and a wrong `Parts`, not a memory
  fault. Widening them changes `MultipartFraming` and the `[]int` paths, which
  are public API.
- (b) **A disabled `MaxLineBytes` is the one-buffer framing, not "no limit".**
  `Limits{MaxLineBytes: LimitDisabled}` reads framing lines the way `Read`
  does: a preamble or delimiter line longer than the 4096-byte buffer ends the
  iteration with `multipart: NextPart: bufio: buffer full` and no diagnostic,
  where the default 64 KiB budget reads the same message. That is pinned on
  purpose (`TestParse_multipartLineBytes_disabledKeepsLegacyReading`) and
  documented on `textproto.MultipartReader.SetHeaderLimits`, so it is left;
  but it is the same kind of sentinel mismatch as fix 2, it makes "disabled"
  less tolerant than any finite budget, and `Limits.MaxLineBytes` does not
  say so. A caller who wants no line limit should set a large finite budget.
- (c) **Counts from a `CharsetReader`'s reader or a replaced `Entity.Body`.**
  Fix 1 holds counts to the contract where a reader enters a buffered reader
  of this module. A caller's reader that is read directly by the caller's own
  code through `Entity.Body` — a charset hook's reader, a body the caller
  replaced — still has a negative count passed on to that caller (`bodyGuard`
  clamps only counts above the room). Nothing in this module indexes with it.
- (d) **`MaxTotalDecodedBytes` with bodies read concurrently.** Each
  `bodyGuard.read` takes the room left, reads, then charges; two bodies read
  on two goroutines can each be given the same room, so together they may
  deliver more than the budget by up to one read each. The charge is clamped,
  so the count cannot overflow; the overshoot is bounded by the callers' read
  sizes.
- (e) **Budgets a caller turns off are off.** With `LimitDisabled` the
  preamble (after fix 2), a held framing line under a huge `MaxLineBytes`, the
  header block, and the diagnostics list grow with the input. That is what the
  setting asks for; `MaxInputBytes` is the backstop, and it too can be
  disabled.
- (f) **`Read`, `ReadWithOptions`, `New` and bare `textproto`** budget only
  the root header (1 MiB by default): part headers, nesting depth and the
  number of parts are unbounded there, as documented. `HeaderFields.Del` in a
  caller's loop is still one move of the remaining fields per call.
- (g) **`Header.AddRaw` and the `HeaderFields` cursor panic** on a value with
  no colon and on use before `Next` or after it returned false. Both are
  documented contracts on caller-supplied arguments, not reachable from input.

## Tests

- `TestBoundsCore_sourceCountOutOfRange` (root and textproto): readers that
  report `n+1`, `n+2^20`, `n-1`, `n-2^20`, and counts near `math.MaxInt` and
  `math.MinInt`, through `Parse`, `Read` and `NewMultipartReader`: no panic,
  and nothing lost for a count that is only too large.
- `TestBoundsCore_preambleLimitDisabled`: default, disabled, 2^40 and
  `math.MaxInt64` over a preamble just past 1 MiB.
- `TestBoundsCore_hugeBudgets`, `TestBoundsCore_hugeBudgetsFrameAlike`,
  `TestBoundsCore_hugeBudgetsReadHeaderAlike`: budgets past 32 bits behave as
  a budget that fits, whole and a byte at a time.
- `TestBoundsCore_boundaryLongerThanBuffer`.
- `TestBoundsCore_delMatchesOneAtATime`, `TestBoundsCore_delIsLinear`.
- `FuzzBoundsCore_framingBudgets`: arbitrary input under arbitrary budgets
  (negative, tiny, past 32 bits) and chunking, beside a watchdog. Run it with
  `-fuzzminimizetime 1s`: the seeds are 72 KiB, and the default minimization
  of each new input otherwise takes most of a short run.
