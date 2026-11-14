# Randomized soak (ROB-130-007)

Code: `mail/soak_test.go` (`TestSoak`, and the `TestSoak_*` tests for what it
found), inputs in `mail/testdata/soak/`. Fixes: `multipart_stuck.go`,
`textproto/multipart_stuck.go`, with small hooks in `multipart.go`,
`textproto/multipart.go`, `entity.go`, `completion.go` (`Entity.Drain`) and
`mail/tree.go` (`roleOf`). Chunk invariance: `chunk_invariance.go`, with hooks
in `cte_tolerant.go`, `embedded.go`, `textproto/multipart.go` (`scanBody`) and
`mail/tree.go` (spans); tests `TestChunkInvariance_*` in the root package and
in `textproto`.

## What it generates

Every iteration has its own generator, a `math/rand/v2` PCG seeded with the
run's seed and the iteration's number. Nothing depends on the clock or on
global randomness, and nothing carries over from one iteration to the next,
so one iteration can be run alone. Every input is cut to 64 KiB.

- **Mutations** of the fixtures in `testdata/corpus/*.eml` (45%): bit flips,
  bytes overwritten with ones the parser cares about (`\r \n - : = ; " NUL
  0xff < >`), deleted ranges, duplicated lines, swapped lines, truncation,
  line endings rewritten (all CRLF, LF, CR, or mixed), a fixture spliced onto
  another, tokens inserted (delimiters, encoded-word starts, BOMs, header
  lines, uuencode markers), and short ranges repeated up to 512 times.
- **Generated messages** (55%), damaged afterwards half of the time. They use
  random header sets: addresses, RFC 2047 subjects in utf-8, euc-kr,
  iso-2022-jp and unknown charsets, broken encoded words, bad dates and
  message ids, folding, colonless lines, long lines, BOMs and mbox `From_`
  lines. Multiparts nest to depth 4 with random boundaries; the boundary is
  sometimes missing, wrong, reused from the parent or split by RFC 2231, and
  the close delimiter is sometimes missing, cut or followed by junk.
  Preambles and epilogues are random. Leaves get base64 (with junk, bad
  padding or data after padding), quoted-printable (with bad escapes and long
  lines), uuencode (with missing begin or end lines and stripped lines),
  7bit, 8bit, binary or a junk encoding token, and a charset of utf-8,
  euc-kr, iso-2022-jp, windows-1252, us-ascii, an alias or an unknown one,
  with content to match. Some parts are `message/rfc822`, `message/global`
  or `message/news`, possibly base64 or QP encoded, and some are
  `text/rfc822-headers`.

The configuration comes from the same generator. It picks the profile
(Tolerant, Forensic, Strict) and the compatibility, `WalkEmbeddedMessages`,
`RecognizeMboxEnvelope`, `FallbackCharset` and
`SkipTextAttachmentDecoding`. A third of the time it sets one to four budgets
to small values and sometimes disables one. A third of the time it adds a
`DiagnosticSink`, which may stop the parse at its first to sixth call or
discard what it is given. Finally it picks one of five consumers:

| consumer | what it does |
| --- | --- |
| drain | reads part of the root body, or of a multipart root's first part, sometimes, then `Entity.Drain` twice |
| walk | `Entity.Walk`, reading every leaf body; afterwards, what each walked message left of its carrier |
| tree | `mail.ReadTree` with random `MaxBodyBytes`, `MaxTotalBytes`, `DescendMessages` |
| spool | the same, with a `mail.Spool` with small quotas, in a `t.TempDir()`, or an in-memory `BlobStore` whose `Create` sometimes fails; every kept body is read back |
| reader | `mail.NewReader` and `NextPart` until io.EOF or a repeated error |

Each input is parsed whole (`bytes.Reader`), and again through a reader that
delivers 1 to 4096 bytes at a time with occasional runs of up to four empty
reads. Three times in four it is parsed a third time through a hostile
reader: one that fails with an I/O error, cancels the parse's context, or
stalls with `(0, nil)` forever from a random offset.

## What it asserts

For every pass:

- no panic; it is recovered, and the seed, the iteration, the config, a
  quoted and hex dump and the reproducing command are reported;
- the iteration finishes within 30 s (a watchdog; an iteration takes
  milliseconds), and dumps every goroutine's stack if not;
- decoded output is at most 8 × input + 4 KiB (no decoder expands, uudecode
  yields at most 1.5 bytes per byte, and a charset at most 3 bytes per byte);
  bodies are read through a limit of that size, so an endless decoder is
  caught rather than followed;
- `Completion`: no negative counters, `BodiesRead ≤ Bodies`,
  `MultipartsRead ≤ Multiparts`, `Bodies + Multiparts ≤ Entities`,
  `InputBytes ≤ len(input)` and `≤ MaxInputBytes + 1`, no `InputErr` unless
  the input failed, and then the identical error; a sink that stopped the
  parse means `stopped` (or `canceled`); a consumer that read everything
  without a stop leaves nothing in progress;
- diagnostics: well-formed codes (`group.name`), severities in range, paths
  and offsets not negative, at most `MaxDiagnosticCount` kept and delivered,
  as many kept as delivered unless the sink stopped the parse, none kept when
  discarded, no more kept than counted;
- a `Drain` that returns nil leaves no stop, and a second `Drain` after it
  returns nil, under every profile; after a `Drain` that returned an error
  while the parse was stopped, a second returns the identical stop;
- trees: children name their parent and extend its path; containers hold no
  body; `Data ≤ Size`, `Truncated` true exactly when Data is short, Data
  within `MaxBodyBytes` and `MaxTotalBytes`, the digest that of Data kept
  whole, duplicates with equal digests; a multipart that holds a body is not
  a container; the `Bodies`, `Attachments`, `Resolve` and `RelatedRoot`
  views stay inside the tree;
- spools: usage within the quotas; every kept body reads back at its kept
  size, and a body kept whole has the tree's size and digest; only
  `limit.disk_spool_bytes` diagnostics; `Close` twice without error, nothing
  held after it, no file left in the directory, and every blob of the store
  deleted exactly once;
- `mail.Reader.NextPart` ends, or repeats its error, within 65,536 calls.

Whole and chunked reads must give the same outcome: the tree, every leaf's
size, digest and raw span, the counters, and the diagnostics in the order
they were recorded in, including with a sink that stops the parse at its
n-th diagnostic. They are not compared when:

- a budget is small, since budgets are spent as bytes are read and readers
  fetch ahead;
- the profile is Strict (see the open findings).

This applies only when the spool's `Body` ended `ReadTree` in the middle of
the same body in both readings. That means the hook returned an error and
`ReadTree` returned that very value, compared by identity, with no parse stop
(`skAbandonment`). The rest of the outcome is still compared exactly. The
diagnostics only need one list to begin with the other, with the extra ones at
or under that body (`skAbandonedAlike`, iteration 202279 below).

Every 100 iterations (1000 in long mode), and at the end, it checks
resources. Goroutines must return to the count before the first iteration,
after a settle loop of up to 1 s. Open file descriptors in `/proc/self/fd`
may exceed the starting count by at most 2. After `runtime.GC`, `HeapAlloc`
must stay under 64 MiB, and at the end the median of the last quarter of the
samples may not exceed the median of the first by more than half plus
4 MiB. One iteration may not allocate more than 64 MiB; the most seen is
about 8 MiB (three passes over a 64 KiB input), 9 MiB under the race detector.

## How to run and reproduce

Every run goes through the bounded runner (see CLAUDE.md).

```sh
go test -run '^TestSoak$' ./mail/                        # 3000 iterations, seed 1300007, under 1 s
env EMLKIT_SOAK=30s go test -run TestSoak -timeout 100s ./mail/   # long, by time
env EMLKIT_SOAK=50000 go test -run TestSoak ./mail/               # long, by count: repeatable
env EMLKIT_SOAK_SEED=7 EMLKIT_SOAK=30s go test -run TestSoak ./mail/
env EMLKIT_SOAK_SEED=1300007 EMLKIT_SOAK_ITER=67 go test -run '^TestSoak$' -v ./mail/
```

- `EMLKIT_SOAK`: a duration, or a number of iterations. Long mode is skipped
  under `-short`; the default pass is not.
- `EMLKIT_SOAK_SEED`: the seed (decimal or 0x hex); 1300007 by default.
- `EMLKIT_SOAK_ITER=a,b,c`: run those iterations alone and log each one's
  config and outcome.
- `EMLKIT_SOAK_KEEP_GOING=1`: report every failing iteration in one line and
  go on. A hang still leaves its goroutine running, so the next resource
  check fails.
- `EMLKIT_SOAK_DUMP=<dir>`: write the input of a failure there. Otherwise it
  goes to `t.TempDir()`, which the test removes.
- `EMLKIT_SOAK_OPEN=1`: run the tests that pin open findings instead of
  skipping them.

A failing iteration prints the command that runs it alone. A failure of the
run (a leak, the heap) prints `EMLKIT_SOAK=<iterations run>`, which runs the
same iterations again.

## What it found

Iterations below are for seed 1300007. The default pass takes about 0.5 s
for 3000 iterations, about 6500 a second (5.5 s under `-race`); long runs
went at about 2700 to 5400 a second. There were three long 30 s runs.
The last ran 22,775 iterations before it hit the 22774 hang; the
iterations found are listed below, and each was rerun alone after its fix.

The chunk-invariance round (fixes 5 to 9) ran the 30 s soak with
`EMLKIT_SOAK_KEEP_GOING=1`: 180,511 iterations with 26 differences between
whole and chunked reads, of two kinds (fixes 8 and 9). After those, 181,381
iterations in 30 s with none (about 6000 a second), and 177,355 with seed 7;
with parses a sink stops compared as well, 185,691 with none.

### Fixed

1. **`mail.ReadTree` never returned** (iterations 67, 4621, 22774). ReadTree
   goes on past a part-less `NextPart` failure until the iterator repeats
   itself, but the iterator returned a new value on every call for failures
   that never go away:
   - a multipart with no boundary where nothing heals it (ProfileStrict, or
     an entity from `message.Read`): `multipart: boundary is empty`;
   - an input that fails inside a multipart, under any profile;
   - a carrier body that ends with its own FatalError (a body budget) under
     the embedded message's multipart;
   - under Strict, a base64 carrier whose decoder fails on every read;
   - an entity from `Read` whose source keeps failing.

   The message-layer iterator now keeps a failure it cannot get past (an
   empty boundary without healing, the parse's own input failure, or a
   FatalError of its source) and returns the identical value from then on
   (`multipart_stuck.go`). The multipart also counts as read for
   `Completion`. textproto still reads a failing source again on every call,
   as documented. A failure that recurs at the same offset with the
   identical cause is now returned as the identical value
   (`textproto/multipart_stuck.go`), so a caller sees the repeat. A source
   that returns a new error value each time it fails, through `Read`, still
   gives a new value each time. `TestSoak_readTreeEnds`.
2. **A second `Drain` made up parts** (iteration 121). `MultipartReader`
   builds a new iterator on every call. After the parts were read, a second
   walk framed the epilogue, and under recovery healed it into
   `multipart.empty` or a header-less part made of the epilogue. That was
   counted in `Completion`, and how much epilogue was left depended on the
   chunking. Under recovery, an entity whose parts were read to their end
   now gets an iterator at its end, which reports the parse's stop if there
   is one. A body the caller replaced keeps the earlier behavior.
   `TestSoak_drainAgain`. Strict kept it too until 2026-11-13 (see fix 10).
3. **`Drain` left a carrier unread** (iteration 91). Under
   `WalkEmbeddedMessages`, when the embedded message could not be read,
   nothing read the carrier's body, and `Completion` stayed `in-progress`
   after a `Drain` that returned nil. Drain now also reads what each
   embedded message left of its carrier. `TestSoak_drainEmbeddedUnreadable`.
4. **A multipart read as one body was a container** (iteration 163).
   `roleOf` took `Data == nil` to mean "no body". With a `Spool` (or
   `MaxBodyBytes < 0`, or the total budget spent) such a body has no Data,
   so it was classified `RoleContainer`, and every view skipped it. It now
   takes `Size == 0` too. `TestSoak_opaqueMultipartRole`.
5. **The root's closing delimiter was lost at the end of a full buffer**
   (iteration 5509, `testdata/soak/root-close-lost-after-inferred-boundary.eml`,
   `TestSoak_rootCloseLost`). Read whole, the root recorded
   `multipart.close_not_found` and the parse ended `incomplete`; read a few
   bytes at a time, it did not. The part framed on an inferred boundary was
   not the cause, it only placed the bytes. `textproto`'s `scanBody` told
   `match` that the buffer was full for every delimiter candidate in it, and
   `match` decides a candidate the buffer cuts as "never a delimiter", which
   is only true of one that begins the buffer. A full buffer that ended with
   `\n--boundary-`, one byte short of the closing delimiter, was delivered as
   body, and the delimiter with it. (A buffer ending with `\n--boundary` was
   saved by the hold-back at the end of `scanBody`, which looks for a prefix
   of the name only.) A candidate further in is now undecided: the body
   before it is delivered and it is looked at again from the start of the
   buffer. `TestChunkInvariance_closeAtBufferEnd` (textproto) moves the
   closing delimiter across the buffer's end a byte at a time.
6. **Diagnostics of a transfer decoder ran ahead of its consumer**
   (iterations 214 and 3553, `testdata/soak/qp-embedded-diagnostic-order.eml`,
   `TestSoak_diagnosticOrder`; a sink stopping at the n-th diagnostic then
   stopped elsewhere, 1706). A tolerant decoder decodes one block of its
   input at a time and recorded every repair of the block before its
   consumer had read the first decoded byte, so its diagnostics came before
   or after those of the stages above it — the parse of an embedded message,
   a charset monitor — depending on how much one read of the input returned.
   A settled repair is now held with its position in the decoded output; a
   `Read` never returns bytes past it, and the `Read` that begins there
   records it. The carrier's body is also read in whole blocks for the
   embedded parse, so that what its other stages report under the embedded
   reader's buffer depends on the bytes alone. `chunk_invariance.go`;
   `TestChunkInvariance_notesByPosition`.
7. **A carrier whose message could not be read kept a rest that depended on
   read-ahead** (iterations 480, 3722, 4606, 18589). What the failed header
   read had buffered was gone, so `ReadTree` kept a leaf whose Size and
   SHA256 changed with the chunking. The bytes the attempt took are given
   back, and the carrier is a leaf with its whole body.
   `TestChunkInvariance_carrierRestored`.
8. **The end of a carrier's body was read after the walk had moved on**
   (iteration 87148, and the corpus fixture forwarded-rfc822-attachment).
   A walk left a carrier when its message ended; what was left of the
   carrier's body was read by `Drain` after the whole walk, by which time the
   multipart the carrier is a part of had discarded it. The decoder then
   ended wherever its buffer had: `cte.base64_length_invalid`, and with it
   the state `incomplete`, came and went with the read size, and a carrier
   whose embedded multipart had ended counted as read only when a buffer had
   fetched its last bytes. `Walk` now reads the rest of a carrier's body
   before it goes on to the carrier's siblings (embedded-messages.md).
   `TestChunkInvariance_carrierRead`,
   `TestChunkInvariance_unreadableCarrierWalked`.
9. **`ReadTree` took a multipart child's raw span too early** (iteration
   7043, 25 more in 180,000). A part's span is known once its body was read
   to its end; for a multipart child that is when the parent's iterator has
   moved past it, and before that only if a buffer reached the delimiter
   after it. Read whole, the signed part of a `multipart/signed` that is
   itself a multipart had a span; read three bytes at a time it had none, and
   the tree reported `signed.unverifiable`. The span of each child is now
   taken after the iterator's next call. `TestSoak_signedSpan`.
10. **Drain's verdict after the walk, and Strict's second Drain** (#10,
    #70; seed 1300007 in the default pass, iterations 41, 430, 707 and 11
    more in 3,000, all Strict). Strict's second `Drain` framed the epilogue
    again and returned the iterator's `NextPart: EOF` after a first that
    returned nil; an epilogue holding a delimiter line made up parts. The
    end-of-parts iterator of fix 2 now applies under every profile, and
    returns the failure the parts ended with, if any. Separately, under
    Strict the walk does not read what an embedded message left of its
    carrier, `Drain` does after the walk, and a stop that arose there was
    lost: `Drain` returned nil, or a Strict refusal that had ended the walk
    first. It now returns the parse's stop whenever there is one. The drain
    mode's oracle now asks Strict for a second Drain too, and requires a nil
    `Drain` to leave no stop. `completion_drain_test.go`.
    A Drain after the caller had taken a multipart's first part and read some
    of it (found in review, not by the soak, whose partial read only touched
    a leaf root) framed the body anew from inside that part: Strict returned
    `NextPart: EOF`, the healing profiles nil with the parse `in-progress`.
    The iterator a caller began is now resumed and its current part finished
    first, and a part gone past unread is never counted read
    (`drain_resume.go`, `completion_resume_test.go`, completion.md). The
    drain mode's partial read now also reads part of a multipart root's first
    part; 20,000 iterations pass.

### Resolved follow-ups (2026-11-14)

- **A 7bit violation noted or not by how far a spool got** (seed 1300007,
  iteration 202279, found 2026-11-13 by a 60 s run; present at `ef192b2`).
  Triaged and independently reviewed 2026-11-14 (#43): the oracle was wrong, not the spool. The
  store fails at its first `Create` (`store=true/1`). With `MemoryBytes: -1`,
  `Spool.Body` has read one decoded byte to learn the body is not empty, and
  then `ReadTree` ends with the store's error, leaving the
  `text/calendar; charset=ks_c_5601-1987` body unfinished. The charset decoder
  above the 7bit monitor reads ahead of that byte by one read of the input.
  The monitor reports the first 8-bit byte when it hands it on, so read whole,
  `cte.seven_bit_violation` is already recorded, and read 16 bytes at a time
  it is not. Drained, both readings record the same diagnostics. When both
  readings let go of the same body through the spool's Body error,
  `skAbandonedAlike` now requires an identical tree, counters and stop. One
  diagnostic list must begin with the other, and every extra diagnostic must
  be at or under the abandoned body's path. Pinned by
  `TestSoak_abandonedSpoolBody`
  (`testdata/soak/spool-store-failure-abandoned-body.eml`). The iteration
  passes. `TestSoakOracle_abandonedAlike` and
  `TestSoakOracle_abandonment` reject changed tree/counters/order/path and
  require an actual Body-hook error with no parse stop.
- **The walk consumer reads everything and a body is not counted read**
  (seed 1300007, iteration 114063, found 2026-11-14). The carrier of an
  embedded leaf that ended on its own line budget was left unread; the new
  abandonment accounting correctly refused to count the late read of bytes
  already discarded. Earlier success depended on read-ahead. `Walk` now
  reads a consumed embedded message's carrier rest before moving on, under
  every profile; a skipped message remains unread. This also fixes Strict
  carrier rests larger than a read-ahead. Pinned by `TestWalkCarrierRest`
  and `TestSoak_walkCarrierRest`, with the original fixture at
  `testdata/soak/walk-carrier-rest-after-cut-leaf.eml`. The exact iteration,
  fresh full units and 30 s / 172,985 seeded iterations pass. The completion
  oracle is unchanged. See [completion.md](completion.md).

### Open

- **Strict base64 depends on read size** (iterations 6478, 11189, 19668,
  `testdata/soak/strict-base64-padding-chunked.eml`,
  `TestSoak_strictBase64Chunked`). ProfileStrict keeps `encoding/base64`'s
  streaming decoder. It refuses data after padding that arrives in the same
  read, and accepts it when a read ended at the padding: `aGk=\naGk=` is
  "hi" plus an error read whole, and "hihi" read a byte at a time. This is
  Strict's earlier behavior, left as it is. The same shows on the corpus
  fixtures base64-data-after-padding (another body, an error or none) and
  base64-junk-and-missing-padding (the error's offset, 4 or 0), under
  ProfileStrict only: `Read`, `ReadWithOptions`, `New` and the healing
  profiles use the tolerant decoder and give one result
  (`TestChunkInvariance_legacyBase64`, `TestChunkInvariance_fixtures`).
- Not part of the promise: `Completion.InputBytes` and `InputEnded` say how
  much of the input was fetched, and what follows a closing delimiter is not
  read, so they depend on buffering; a budget is spent as bytes are read, so
  a parse that runs into one may do so at another place.

## Limits

- It checks invariants and agreement between read sizes, not expected
  results; the corpus (`corpus_test.go`) does that.
- Only Linux counts file descriptors.
- A hung iteration's goroutine cannot be stopped. The run reports it and
  fails, and the goroutine runs until the test binary exits.
- `message.CharsetReader` is the charset plugin's only during `TestSoak`.
  An `init` in the file puts the default back, because the other tests of
  the package expect no CharsetReader.
