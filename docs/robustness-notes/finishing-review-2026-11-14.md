# Finishing review — 2026-11-13–14

This review covers the necessary, bounded follow-ups selected from the open
issues, starting at `ef192b2cec1a5c0ec3557763683eb0651fbbacd1` on `main`.
Claude Code implemented the product and test changes; Codex reviewed the actual
diffs, requested corrections, and ran independent public-API probes and local QA.
This review records the validation completed before publication. GitHub issue
closure is tracked separately from publishing these changes.
A follow-up passing this review does not close every requirement of its parent
roadmap item.

## Reviewed follow-ups

| Issue | Result | Focused evidence |
| --- | --- | --- |
| [#71](https://github.com/saeroun-ai/emlkit/issues/71) | Shared-boundary resume retains its scan cursor instead of rescanning each short read. | Deterministic scanned-work tests; original 128 KiB one-byte-reader reproduction; nested siblings and epilogue. |
| [#72](https://github.com/saeroun-ai/emlkit/issues/72) | Declared PSS salt length zero no longer succeeds through Go's automatic salt detection. Exact zero is `NotChecked` where the standard library cannot establish it. | OpenSSL genuine zero/default/mismatch fixtures; mismatch never `Passed`. |
| [#68](https://github.com/saeroun-ai/emlkit/issues/68) | Removed the tracked throwaway recipient private key; decryption tests generate key material in memory. | RSA/OAEP/PKCS1, CBC/GCM, recipients, authenticated attributes and tampering. Git history is unchanged. |
| [#73](https://github.com/saeroun-ai/emlkit/issues/73) | A retained raw header remains readable when the body exceeds the recording limit or storage fails. | Root, framed child and identity-embedded header reads before/during/after consumption; absent raw bodies still fail. |
| [#65](https://github.com/saeroun-ai/emlkit/issues/65) | Charset declarations win by default; `RecoverCharsetDeclarations` explicitly enables the prior semantic recovery. | Original valid Shift_JIS reproduction; 60 public cases across profiles, options and chunk sizes; BOM/source/sink tests. |
| [#63](https://github.com/saeroun-ai/emlkit/issues/63) | Healing Parse removes folded line endings while retaining the surrounding whitespace. | Exact values and raw replay, whitespace-only continuations, strict/legacy behavior; two stale differential exceptions removed. |
| [#64](https://github.com/saeroun-ai/emlkit/issues/64) | Disabled Parse line limits no longer leave the 4 KiB framing restriction for long preambles/padded delimiters. | 60 public cases, finite allocation/work tests, input caps and cancellation. Bare textproto/legacy behavior stays separate. |
| [#10](https://github.com/saeroun-ai/emlkit/issues/10), [#70](https://github.com/saeroun-ai/emlkit/issues/70) | Drain returns final session stops, resumes a partly consumed multipart and its current child, does not reframe an ended multipart, and finishes a consumed embedded message’s carrier before advancing. | Public stop/repeat/partial-read probes; nested and embedded cases, abandonment accounting, replaced Body and Strict tests. |
| [#66](https://github.com/saeroun-ai/emlkit/issues/66) | DSN status/action and DSN/MDN/ARF type guesses no longer populate declared-value fields. | Six public report cases; malformed/ambiguous values, raw fields, guess invariants and report fuzzing. |
| [#67](https://github.com/saeroun-ai/emlkit/issues/67) | A later multipart declaration with a matching body delimiter records one Info observation without changing the selected tree. | 40 public cases; direct sink-stop propagation; finite linear-work suffix tests; EOF grammar checked against actual framing. |
| [#33](https://github.com/saeroun-ai/emlkit/issues/33) | Declared 7bit bodies report the first line over 998 content octets without changing bytes. | 576 public combinations; diagnostic order, cancellation, input failure, final-line and finite buffering tests. |
| [#43](https://github.com/saeroun-ai/emlkit/issues/43) | Parse spool budgets apply cumulatively to a ReadTree tree alongside SpoolOptions; short/impossible writes and failed Close are errors; foreign writer errors are not traversed. | Public cumulative quota/hash/cleanup cases; embedded, disabled/default, multiple-spool, failed-store and cancellation tests; narrow partial-consumer soak oracle with rejection tests. |

## Consumer-visible changes

- `ParseOptions.RecoverCharsetDeclarations` defaults to false. Clients that
  deliberately want the old BOM/UTF-8 heuristic override must opt in. Strict
  continues to use the declaration.
- `RecipientStatus.StatusGuess`, `ActionGuess`, and `ReportValue.TypeGuess`
  carry inferred values. Consumers must choose explicitly whether to use them;
  an absent or invalid declared Status does not become valid because a guess
  exists.
- Under healing Parse, unfolded header values now retain whitespace that was
  previously collapsed. Original header bytes and verbatim replay are retained.
- Parse's multipart iterator now resumes its original body. Advancing past an
  unread child still discards that child's remaining bytes; Drain cannot make
  already discarded content complete.
- Parse spool defaults now also cap a tree stored with larger `SpoolOptions`.
  Raise or disable the corresponding Parse limits deliberately when more storage
  is required. Limits count per ReadTree tree; SpoolOptions count the spool as a
  whole. Raw source recording continues to use `RecordOptions` separately.
- With `WalkEmbeddedMessages`, Strict `Walk` now consumes the remaining
  carrier after its embedded message was read to its end, matching the other
  profiles. A caller that needs those raw bytes later should retain the source;
  reading a root carrier after Walk no longer retrieves that remainder.

## Verification

The final integration gate passed on `tienipia-workstation`, Linux amd64,
Go `1.26.10-X:nodwarf5`, on 2026-11-14. No tests were skipped in the final gate.
All Go execution used `scripts/qa-bounded.py`, with enforced 1.5 GiB RAM,
no swap, two CPU cores, 128 tasks and a deadline. One QA service ran at a time.

| Check | Final result |
| --- | --- |
| `go build ./...` | Passed |
| `go test -count=1 -timeout 120s ./...` | Passed |
| `go vet -composites=false -stdmethods=false ./...` | Passed with the repository's two existing analyzer exclusions |
| `gofmt -l .` and source/document diff checks | Empty / passed; the two new raw EML soak fixtures intentionally retain original CRLF, trailing whitespace and malformed bytes |
| Full race group 1: root, mail, charset, utf7, internal | Passed, fresh run without skips |
| Full race group 2: textproto | Passed, fresh run |
| Full race group 3: tnef, smime, pgpmime, icalendar, appledouble, legacyenc | Passed, fresh run |
| Python differential | 74 fixtures; 42 documented differences, 0 open, 0 unexplained, 0 stale exceptions |
| Soak seed 1300007, exact iterations 114063 and 202279 | Both passed |
| 60 s seeded soak | 355,543 iterations; no failures, 94 diagnostic codes observed |
| `FuzzParse_heal`, 30 s, 2 workers | Passed; 62,547 executions |
| Ten independent public-API probe programs | All passed, including 576 7bit and 40 duplicate-declaration combinations |

The staged diff check additionally identified whitespace warnings in the two new
raw EML soak fixtures. Both files are byte-identical to the original failure
dumps; those bytes are required by the regressions and are deliberately retained.
All other changed paths pass the diff check.

The earlier item-specific checks also include OpenSSL PSS comparisons,
report-parser fuzzing and raw-header replay fuzzing. Invocation logs and JSON
statuses for this local run are in `/tmp/emlkit-finish-20261113/final/`;
the permanent regression tests and fixtures live in the repository. This is
local Linux validation, not remote CI or Windows/device validation.

## Work deliberately left open

- [#62](https://github.com/saeroun-ai/emlkit/issues/62): declaration-first policy
  and structural exceptions are documented, and the concrete charset/header/
  report follow-ups above are implemented. Classification of every recovery in
  every robustness note is not complete.
- [#69](https://github.com/saeroun-ai/emlkit/issues/69): real-client mail and
  cryptographic fixtures still need collection and validation; generated
  fixtures are not evidence of broad client compatibility.
- [#13](https://github.com/saeroun-ai/emlkit/issues/13): Python differential
  evidence does not establish Mime4j, MimeKit or Angus interoperability.
- Long-running fuzz/performance/soak and scheduled CI work are separate from
  these local bounded checks. Existing broader parent issues retain their
  unfulfilled acceptance criteria.
- Existing limitations remain explicit: inferred/encoded embedded raw spans,
  padded body delimiter candidates held until decided under the input budget,
  and extremely long declared boundary names in the body scanner. Disabling
  the line limit does not make every parser allocation constant-memory.
- Strict still uses the standard Base64 decoder whose acceptance after padding
  can depend on read size. Repeated Drain after a non-final Strict refusal is
  also not fully pinned; see the completion and soak notes.
- The new duplicate Content-Type observation is bounded to the first 4096 wire
  bytes and valid later media-type declarations. It cannot prove that an
  alternate multipart is absent beyond that window.

See the individual notes and `docs/robustness-roadmap.md` for precise contracts,
retained historical findings, and the status of each parent item.
