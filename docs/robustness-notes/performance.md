# Complexity and benchmarks (ROB-060-013, ROB-120-002, ROB-130-006)

Code: `internal/scaletest/`, `multipart_heal_nest.go`, `cte_chunk.go`,
`textproto/preamble_grow.go`, `mail/tree_scale.go`, and small changes in
`textproto/multipart.go` (`scanBody`, `keepPreamble`), `multipart_heal.go`
(`healEnd`), `cte_tolerant.go` and `mail/tree.go` (`classify`, `roleOf`,
`read`). Tests and benchmarks: `bench_parse_test.go`, `complexity_test.go`,
`multipart_heal_nest_test.go`, `textproto/bench_scan_test.go`,
`textproto/preamble_grow_test.go`, `mail/tree_scale_test.go`,
`mail/values_scale_test.go`.

## What is held

Parsing and decoding cost is proportional to the size of the input, or bounded
by a budget of `Limits` and listed under "More than linear, and bounded"
below. The complexity tests run each case at one size and at four times that
size and fail when a cost grew more than twice as fast as the input
(`scaletest.MaxGrowth`): eight times the cost for four times the bytes, where
quadratic work costs sixteen.

Three costs are compared (`internal/scaletest`):

- bytes allocated and number of allocations, from the runtime's counters.
  They are counted, not timed, and are compared under the race detector too;
- processor time of the test process (`getrusage`, user and system), with the
  collector held off while a run is timed, the best of up to six attempts,
  each later one on a heap just collected. It does not advance while the
  process waits for a processor, so load on the host does not fail it. Not
  compared under `-race` or `-short`, and on a system without `getrusage` the
  monotonic clock stands in for it.

A case that fails for time prints both times. Before believing it, run it
alone inside the bounded runner: a first run of a size also pays the system
for memory it is the first to touch, which the retries are there to cancel.

| Test | Cases |
| --- | --- |
| `TestComplexityParse` | `Parse` + `Entity.Drain` on every shape below that scales, delivered whole, 16 bytes a read and 1 byte a read |
| `TestComplexityWireHeader` | `textproto.ReadHeader` / `ReadHeaderRecover` with no budget: one long line, one long folded field, many fields, fields ended by bare CR, stray CR inside a value, white-space-only lines, a malformed prefix, damaged lines, unindented continuations; through a 4096-byte buffer, and through a 16-byte one (4096 for the recovering cases, which look no further than one buffer) fed a byte at a time |
| `TestComplexityWireMultipart` | `textproto.MultipartReader` on every near-match body: read whole with and without recovery and the 64 KiB line budget, a byte at a time, and the same lines as a preamble |
| `TestComplexityHeaderValues` (root) | `ParseMediaTypeTolerant`, `ParseContentDispositionTolerant`, `ParseParameterCandidates`, `DecodeHeaderText`, `canonicalCTE`, `SafeFilename` on 31 values that open, nest or never close one construct |
| `TestComplexityHeaderValues` (`mail`) | `ParseAddressListTolerant`, `ParseDateTolerant`, `ParseMsgIDListTolerant`, `ParseListURLs`, `ParseListID`, `ParseAuthenticationResults`, `ParseReceived`, and the strict accessors, on the same values |
| `TestComplexityClassify`, `TestComplexityReadTree` (`mail`) | classifying a container of n parts of each kind `roleOf` treats specially; `Parse` + `ReadTree` + views, and `mail.Reader`, on wide messages |

Regression thresholds the tests enforce, beside the growth ratio:

| Test | Threshold | Was |
| --- | --- | --- |
| `TestNestedInferenceCost` | at most 48 bytes allocated per byte of a message of 100 nested multiparts that declare no boundary | 463 |
| `TestSmallPartDecoderCost` | a Base64, quoted-printable or uuencoded part of 8 bytes allocates at most 2048 bytes more than a binary one | 8521, 10042, 8597 |
| `TestPreambleKeptCost` | keeping an unframed body allocates at most 4.25 times its size | 4.7 |
| `TestReadTreeLeafCost` | `Parse` + `ReadTree` allocate at most 16 KiB per small leaf | 37 KiB |
| `TestDelimiterScanDashRun` | a body of runs of dashes under a boundary of dashes costs at most 8 times a body of ordinary near matches | 19.6 |

`TestBenchmarkShapes` checks that each benchmark input is the message it is
meant to be: its entity count, the diagnostics it must record, and how much
its bodies deliver.

## Super-linear paths found, and what was done

1. **Classifying a tree, `mail.ReadTree` — quadratic in the parts of one
   container; fixed.** `roleOf` looked every part up among its siblings
   (`indexOf`), and for a part under a `multipart/related` asked for the root
   part again, which searches the children for the `start` parameter. A
   `multipart/related` of 8000 parts whose `start` named none of them took
   0.97 s (121 µs a part, against 12 µs for `multipart/mixed`). The position is
   now read from the parent's list and the root part found once per container
   (`mail/tree_scale.go`): 3.7 µs a part at 5000.
2. **Nested boundary inference — size × depth in time and in memory held at
   once; bounded.** A multipart whose boundary frames nothing keeps its body
   and frames it again on the boundary found. Nested, every level kept and read
   the rest of the same bytes: a 256 KiB message of 100 levels allocated
   116 MiB, 463 times its size, and took 65 ms, with a copy a level live at
   once. Inference is now tried only while at most
   `pfInferNestMax` (8) inferred multiparts enclose the one in question; a
   deeper one is returned as one part with no header, `multipart.start_not_found`
   at `SeverityIncomplete`, its bytes intact. The same message: 5.3 MiB, 5.6 ms.
   Multiparts framed as they declare do not count. This changes what a parse
   returns for mail nested deeper than that with no usable boundary at any
   level, which `multipart-recovery.md` does not yet say.
3. **Delimiter scan over runs of dashes — linear, with the length of the
   boundary as its factor; fixed.** With a boundary of dashes, the name matched
   at every byte of a longer run of dashes and was compared whole each time.
   `scanBody` now skips from a match that is not at the start of a line to the
   next line ending. `near/dash-run-match`: 180 MB/s before, 1469 MB/s after.
4. **Kept preamble — linear, copied about five times over; reduced.**
   `keepPreamble` appended a line at a time and left the growth to `append`.
   It doubles, within the limit (`pfGrow`).
5. **Fixed cost of a small part — linear in the number of parts, with large
   constants; reduced.** Every tolerant transfer decoder allocated 8 KiB of
   buffers at its first read, and `ReadTree` a 32 KiB read buffer per leaf.
   The decoders start with 512 bytes and take the full buffer once a body has
   outgrown it (`cte_chunk.go`); `ReadTree` lends one buffer from a pool. A
   message of 6068 damaged Base64 parts with no header separator: 99.6 MiB
   allocated before, 58.0 MiB after.

Nothing else measured was super-linear. In particular the multipart reader's
delimiter scan, `inferBoundary`, the header recovery look-ahead, the tolerant
decoders, the encoded-word decoder, the MIME parameter parsers and the
recording of diagnostics were linear on every shape here, read whole or a
byte at a time.

## More than linear, and bounded

These cost more than their size and are left as they are, each under a budget:

- **Depth.** Every byte of a part passes one framing reader for each
  multipart it is nested in: size × depth, depth at most `MaxMIMEDepth` (100).
  `mime/deep-100` reads at 199 MB/s where `mime/deep-10` reads at 1008 MB/s.
  Fed a byte at a time, each level costs a call per byte: about 54 ns a byte a
  level.
- **Header recovery look-ahead.** Each damaged header line looks at up to 64
  lines or 4096 bytes after it for a field that shows the block goes on
  (`goesOn`), so a header of damaged lines costs up to 64 lines for each:
  `header/damaged-lines` 97 MB/s against 215 MB/s for `header/many-fields`.
  `MaxHeaderCount` and `MaxTotalHeaderCount` bound the lines.
- **A damaged line ended by a bare CR.** `settleCR` proves what follows such
  a line over up to one field of bytes (64 KiB by default) before the line is
  decided. `Parse` refuses that line as before (ROB-020-002), so the cost is
  paid once per header; it was not measured further.
- **Bytes handed back by a part header.** A part whose header recovery gives
  the body's first line back to the framing reader copies what that reader had
  buffered, up to 4 KiB (`MultipartReader.restore`): about 10 KiB allocated a
  part in `mime/wide-damaged`, against 3.3 KiB in `mime/wide`.
- **Boundary inference.** Still up to `pfInferNestMax`+2 copies of at most
  `MaxPreambleBytes` each at one time, about twice that in allocations.
- **Parts with many MIME parameters.** `mime/wide-parameters` is the slowest
  shape a byte, 20 MB/s and 258 allocations a part: linear, all of it the
  parameter and filename readers.
- **One parameter repeated.** A Content-Type or Content-Disposition that
  repeats one parameter allocates faster than it grows while it is short —
  7.7 KiB for 51 repeats in 510 bytes, 193 KiB for 204 in 2 KiB — and no
  faster than the tests allow from 2 KiB on. Seen in the allocation counts
  only; the field budget bounds it, and it was not looked into.
- **`mail.Node.Resolve`** walks the parts of its `multipart/related` on every
  call. A caller that resolves every reference of an HTML part does
  references × parts work; not changed.
- **`bytes.Index` under a boundary that begins like the body.**
  `near/dash-run` (a run of dashes, a boundary of dashes and one other byte)
  reads at 892 MB/s: the standard library's fallback search, linear.

The structured-header parsers in `mail` are linear in the tests. On values of
128 KiB, twice what a field may hold by default, the address-list parser cost
about three times as much a byte as on 2 KiB, and
`ParseAuthenticationResults` more on a value of nothing but semicolons: growth
of the results they build, not a second pass.

## Benchmarks

```bash
rtk proxy python3 scripts/qa-bounded.py --seconds 120 -- \
  go test -run '^$' -bench 'ParseDrain|DecodeHeaderText|MultipartScan|ReadHeaderShapes|ReadTree' \
  -benchtime 200ms . ./textproto/ ./mail/
```

Every input is built in memory at a fixed size of at most 8 MiB and parsed
with the default `ParseOptions`.

### Baseline

Measured 2026-11-11 **inside the bounded runner on the development host**:
AMD Ryzen 7 H 255, Linux, Go 1.26, a CPU quota of two cores, `GOMAXPROCS=2`,
`GOMEMLIMIT=512MiB`, `-benchtime 200ms`, one run, with other bounded jobs
taking turns on the host. They are numbers to compare a change against on
this host under this runner, and say nothing about another machine.

`BenchmarkParseDrain` — `Parse`, then `Entity.Drain`:

| Shape | Input | MB/s | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| `small/plain` | 624 B plain text | 98 | 12 176 | 75 |
| `typical/alternative+attachment` | 133 KiB: QP text and HTML, Base64 attachment | 287 | 83 055 | 223 |
| `attachment/base64` | 4 MiB | 450 | 36 329 | 105 |
| `attachment/base64-damaged` | 1 MiB of junk, URL alphabet, inner padding | 458 | 38 464 | 119 |
| `text/quoted-printable` | 1 MiB | 183 | 23 782 | 72 |
| `text/quoted-printable-damaged` | 1 MiB | 158 | 29 015 | 77 |
| `text/uuencode` | 1 MiB | 452 | 23 309 | 68 |
| `mime/deep-100` | 100 nested multiparts, 1 MiB leaf | 199 | 1 366 033 | 3 570 |
| `mime/deep-10` | 10 nested multiparts, 1 MiB leaf | 1 008 | 138 093 | 412 |
| `mime/wide` | 9 000 parts, 1.0 MiB | 45 | 29 934 170 | 198 187 |
| `mime/wide-parameters` | 2 194 parts with RFC 2231 and RFC 2047 parameters, 859 KiB | 20 | 51 751 705 | 565 669 |
| `mime/wide-damaged` | 6 826 parts, no header separator, damaged Base64, 1.0 MiB | 40 | 68 395 820 | 240 824 |
| `mime/unclosed` | 1 MiB part, no closing delimiter | 15 862 | 22 913 | 91 |
| `mime/boundary-inferred` | 942 KiB, 900 parts, no declared boundary | 175 | 5 117 238 | 19 906 |
| `mime/boundary-candidates` | 900 KiB of lines that each begin another candidate | 236 | 2 145 200 | 259 |
| `mime/boundary-inferred-nested-100` | 900 KiB, 100 levels with no declared boundary | 51 | 21 262 936 | 1 943 |
| `near/dashes` | 1 MiB of `--` lines | 1 885 | 22 785 | 91 |
| `near/prefix` | lines of the first 8 bytes of the boundary | 1 921 | 22 765 | 91 |
| `near/minus-one` | the delimiter less its last byte | 3 188 | 22 746 | 91 |
| `near/70-shared-prefix` | 70-byte boundary, lines sharing 69 bytes | 5 182 | 23 082 | 93 |
| `near/trailing-junk` | `--boundary junk`, `--boundary--junk`, `--boundary-` | 1 193 | 22 939 | 95 |
| `near/padded` | the delimiter, 300 bytes of white space, then text | 1 820 | 65 951 | 335 |
| `near/padded-past-buffer` | the same with 6 000 bytes of white space | 1 452 | 848 027 | 347 |
| `near/cr-only` | near matches ended by bare CR | 3 084 | 22 758 | 91 |
| `near/bare-lf` | near matches ended by bare LF | 3 155 | 22 758 | 91 |
| `near/one-long-line` | `--boundary` repeated, no line ending | 2 188 | 22 746 | 91 |
| `near/dash-run` | a run of dashes, boundary of 68 dashes and `x` | 892 | 23 084 | 93 |
| `near/dash-run-match` | lines of 200 dashes, boundary of 70 dashes | 1 469 | 23 243 | 95 |
| `header/many-fields` | about 920 fields, 64 KiB | 215 | 510 064 | 4 676 |
| `header/long-folded` | 17 folded fields of 56 KiB | 534 | 5 984 845 | 427 |
| `header/long-lines` | 17 one-line fields of 56 KiB | 866 | 5 430 240 | 283 |
| `header/damaged-lines` | 7 damaged lines before each good field, 48 KiB | 97 | 443 408 | 4 042 |
| `header/rfc2047` | 128 KiB of encoded-word fields, each decoded | 141 | 917 575 | 10 661 |
| `header/rfc2047-unclosed` | 17 fields of 56 KiB of unclosed encoded words, each decoded | 144 | 7 250 516 | 64 090 |

`mime/unclosed` and the near-match shapes are fast because nothing decodes
their one opaque part: what is measured is the delimiter scan. The input sizes
in the table are the nominal ones; `go test -bench` reports MB/s from the
exact length.

`BenchmarkDecodeHeaderText` (2.1 KiB value of encoded words): 230 MB/s,
4 960 B/op, 115 allocs/op.

`BenchmarkMultipartScan` (`textproto`, 1 MiB body, recovery on, 64 KiB line
budget):

| Body | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `text` | 23 529 | 6 143 | 17 |
| `base64` | 23 838 | 6 143 | 17 |
| `many-parts` (10 811 parts) | 136 | 13 964 660 | 117 716 |
| `near/dashes` | 2 135 | 6 170 | 17 |
| `near/minus-one` | 3 820 | 6 146 | 17 |
| `near/70-shared-prefix` | 6 746 | 6 206 | 17 |
| `near/trailing-junk` | 1 235 | 6 166 | 17 |
| `near/padded` | 1 983 | 49 427 | 250 |
| `near/cr-only` | 3 758 | 6 146 | 17 |
| `near/bare-lf` | 3 730 | 6 176 | 17 |
| `near/one-long-line` | 2 157 | 6 171 | 17 |
| `near/dash-run-match` | 1 531 | 6 226 | 17 |
| `preamble/near-minus-one` | 803 | 2 105 070 | 24 |

`BenchmarkReadHeaderShapes` (`textproto`, one block of 1 MiB, no budget):

| Block | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `many-fields` | 172 | 11 310 040 | 87 488 |
| `long-folded` | 495 | 6 412 336 | 55 |
| `long-line` | 693 | 6 266 424 | 28 |
| `cr-inside-value` | 262 | 12 846 392 | 25 612 |
| `damaged-lines` (recover) | 112 | 10 171 608 | 79 983 |
| `unindented-continuations` (recover) | 70 | 24 541 150 | 188 188 |

`BenchmarkReadTree` (`mail`, `Parse` + `ReadTree`):

| Message | Time | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `typical` (449 B, three leaves) | 25 µs | 43 977 | 282 |
| `wide-5000/mixed` | 19.3 ms | 21 539 052 | 180 148 |
| `wide-5000/related-start-unmatched` | 18.6 ms | 21 526 118 | 180 161 |
| `wide-5000/report` | 19.7 ms | 21 543 696 | 180 168 |

### Reading a regression

The tests above are what fails a change. The benchmarks are not run by any
test and have no gate; as a rule for a reviewer on this host and runner, a
shape that falls below half its MB/s here, or more than doubles its B/op or
allocs/op, is a regression to explain before it is merged. Single runs at
`-benchtime 200ms` under a shared CPU quota vary by 10 to 20 %.

## Not done

- No benchmark runs in CI, and no stored history: the baseline is this table.
- Nothing here measures `Entity.WriteTo`, the writers, or the `charset`
  package's decoders.
- `MultipartReader.restore` still allocates per part; `Node.Resolve` is still
  a walk per call; both are noted above.
- `indexOf` and `underRoot` in `mail/tree.go` are no longer called by the
  classification and were left in place.
- The bound on nested inference is a constant, not a field of `Limits`.
