# ROB-020-001 final review evidence

Codex independently reviewed and tested the first-line continuation repair on
Linux amd64, Go 1.26.10, against the unchanged `e2a1c2e` baseline. Only this
roadmap item is complete; [issue #20](https://github.com/saeroun-ai/emlkit/issues/20)
also owns unfinished items. These are local verification results, not a CI claim.

- **16/16 independent/unit stages:** 28,314 finite cases / 222,512 assertions,
  1,110 unchanged control observations, 108 existing retry cases, 54 small-header-
  budget retry cases, and the uncached full six-package suite.
- **15/15 platform/regression stages:** full race suite, GOARCH=386 suite, build,
  formatting and whitespace checks, prior BOM/line-ending/mbox/empty-input probes.
  Plain `go vet` still reports 13 existing unkeyed-literal warnings and the
  existing WriteTo signature warning; all other analyzers pass and no new warning
  was observed. The plain-vet check verifies that exact known set.
- **Three final 30-second fuzz runs, two workers each:** 575,959 executions across
  `FuzzReadHeader_lineEndings`, `FuzzParse_firstLine` and
  `FuzzMultipartReader_malformedPrefixDelimiter`; all passed.
- Every parser/build/test/fuzz job and Claude delegation ran sequentially under
  `scripts/qa-bounded.py`: RAM 1536 MiB, swap 0, CPU quota 2, tasks 128 and a
  deadline. These final jobs exited normally; none was killed for OOM.

The final logs and exact result rows are in [verification.json](verification.json),
[unit and independent QA](post-review-r2-qa.txt),
[platform/regressions](final-platform.txt) and [fuzzing](final-fuzz.txt).
The 25 synthetic reference inputs and 175 measured Angus/Jakarta, Mime4j and
Python observations are archived separately under [references](references/README.md).
The permanent Go regression tests pin the accepted policy and corrected bugs;
the standalone matrix probes and earlier failure logs remain session artifacts
under `/tmp/emlkit-firstline/`, and are not a portable checked-in harness.

## Review corrections

1. An unproved 4 KiB boundary-plus-padding prefix was treated as a real delimiter,
   moving a valid `Subject` field into the body. The repair now proves the full
   delimiter or replays the held bytes as a header candidate.
2. The first correction used header-only budgets to bound delimiter look-ahead,
   losing a healthy sibling in 36 of 54 cases that the baseline recovered after
   retry. Framing `MaxLineBytes` now bounds that look-ahead; real delimiters spend
   no header budget, false candidates are replayed a buffer at a time and charged
   once. All 54 baseline cases now pass.
3. Error/EOF test oracles and three independent QA expectations were corrected
   against existing contracts (random generated boundary, cancellation without a
   diagnostic, and usable raw body on a sink stop). Original failure logs remain
   available. Bounds and failure assertions were not removed to hide a defect.
4. After final tests, one stale API comment was corrected. Reversing exactly that
   comment reproduces every recorded pre-QA source hash; no executable or test code
   changed. This is why full race/fuzz runs were not repeated for that last edit.

The no-prefix padded-first-field issue remains unchanged and tracked in #29.
General orphan/colonless/invalid-name healing, missing separators, the compatibility
engine and complete raw replay remain separate roadmap work. Finite scoped tests
and reference differences do not establish general parser parity or superiority.
