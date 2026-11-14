# Header recovery (ROB-020-003 to -006, -008, ROB-060-010)

Issues #20, #21, #29. Code: `textproto/recover.go`, `textproto/header.go`
(`readFields`), `header_recover.go`, `entity.go`. Tests:
`header_recover_test.go`.

## What changed

A header block with one damaged line used to fail whole: `Parse` returned no
entity, and inside a multipart the iteration ended and every later sibling was
lost. Under every profile but `ProfileStrict` a damaged line — no colon or
semicolon, or a name with a byte a name cannot hold — that no continuation line
follows is now decided instead of refused. `Read`, `ReadWithOptions`, `New` and
`ProfileStrict` behave as before.

At the wire layer this is opt-in: `textproto.HeaderLimits.Recover`, and
`textproto.ReadHeaderRecover`, which also returns the bytes it had to read past
the block.

## How a damaged line is decided

Policy classification (#62): unindented continuation joining remains a structural-recovery exception under healing profiles, restricted to the grammar below. It is not general prose joining or an inference of sender intent. The original physical lines stay available unchanged and the join is diagnosed. Duplicate singleton selection remains declaration-first (the first occurrence); standard unfolding removes line endings without collapsing value whitespace (#63).


In this order:

1. **Unindented continuation.** It continues the field before it when that
   field's value
   - ends with `;` and the line reads as a parameter (`name=...`), or
   - ends with `,` and the line holds an `@` or itself ends with `,`, or
   - is a `Received` field and the line begins a clause (`from`, `by`, `via`,
     `with`, `id`, `for`, `(`, `;`) or follows its `;`.

   The field keeps the line as its last physical line, as read, so it is
   written back unchanged. `header.unindented_continuation` (Recovered).
   This is what recovers `Content-Type: multipart/mixed;` followed by an
   unindented `boundary="..."` line.
2. **Damaged line inside the header.** If a line with a well-known field name
   (`From`, `Subject`, `Content-*`, `X-*`, `List-*`, `Received`, ...) follows
   before the block's empty line — within one buffer of the reader (4096 bytes
   under `Parse`) and 64 lines — the block evidently goes on. The line is kept
   apart from the fields, byte for byte, as a one-line orphan record
   (`Header.OrphanContinuations`), and written back in place.
   `header.missing_colon` or `header.invalid_name` (Recovered).
3. **Start of the body.** Otherwise the block ends before the line, which is
   the first line of the body. `header.missing_body_separator` (Recovered), or
   `header.whitespace_body_separator` when the line before held only white
   space (ROB-020-004: such a line stays a continuation of the field before
   it, and the block ends at the first line after it that is no field).

A part's header is healed the same way, so its siblings stay reachable
(ROB-060-010). A delimiter line of the part's multipart ends the look.

## Duplicate singletons (ROB-020-008)

A block that repeats a field RFC 5322 or RFC 2045 allows at most once records
one `header.duplicate_singleton`, under every profile. Nothing is removed. The
parser and the accessors read the first occurrence. Severity is Recovered when
a repeated `Content-Type`, `Content-Transfer-Encoding` or `Content-Disposition`
differs from the first, Info otherwise.

## The unselected Content-Type (#67)

The first `Content-Type` stays the one read. When a later `Content-Type` of
the same block declares a `multipart/*` whose boundary delimits lines of the
body, the body is also readable as that multipart, and a client that takes the
later field shows parts this parse does not, or other parts than it does
(corpus finding 5). Which one the sender meant is not in the bytes (#62), so
nothing is re-read: Parse, under every profile but `ProfileStrict`, records
`header.unselected_multipart` (Info), which sets `Completion.Ambiguous`.

- What is looked at: every entity, whether its first Content-Type makes it a
  leaf or a framed multipart. Every later Content-Type field is parsed with
  `mime.ParseMediaType`; the header block was already admitted under the
  header budgets, and what is kept is bounded by it. A field that does not
  parse, is not `multipart/*`, has no boundary or an empty one, has a boundary
  holding a line ending or too long for a delimiter line inside the window
  (over 4093 bytes), or repeats the chosen media type and boundary, is
  skipped. Boundaries are not capped at RFC 2046's 70 bytes, since the framing
  reader is not. A boundary declared twice keeps its first declaration.
- Evidence: a line, in the first 4096 wire bytes of the body (the charset
  sniff window), that is `--`, the boundary, `--` for a closing delimiter,
  then only spaces and tabs before a CRLF, LF or bare CR: the rule textproto
  frames delimiters by. A closing delimiter alone counts (an empty alternate
  multipart). A last line the body ends without a line ending, inside the
  window, counts only as a closing delimiter: the healing framing accepts
  `--B--` there in any place, but keeps a cut `--B` as preamble when no
  delimiter line came before it, and after one this look has already found
  its line (`TestUnselectedMultipart_eofRule`). A longer line (`--B-x`, `--B---`), a line not at the line start,
  or a line whose ending begins past the window is no evidence. A line that
  reads as a delimiter of two declared boundaries (`--x--` opens `x--` and
  closes `x`) names the declaration given first. Bytes are read before
  transfer decoding, as a multipart body would be.
- Where the bytes are seen: a tap under the decoders of a leaf, or under the
  framing of a multipart the first field selects. For the latter, lines from
  the end of its closing delimiter line on are its epilogue, which the framing
  reads only as far as its buffer went, so they are not counted.
- Cost: the tap sees the wire bytes the decoders or the framing read; it reads
  nothing itself and stops looking at 4096 bytes. It keeps one line prefix of
  at most four bytes more than the longest boundary, never more than 4096
  bytes, and allocates nothing per byte. The answer depends on the bytes
  alone, not on how they were delivered.
- Recording: once per entity, when a leaf body ends with `io.EOF`, when the
  iteration of a multipart's parts ends with `io.EOF` (after its framing
  notes, such as `multipart.close_not_found`), or when a multipart's raw `Body`
  is read to `io.EOF`. A body or iteration not read to its end, one that
  failed or was stopped, and the verbatim copy `WriteTo` makes record nothing.
  A sink that stops the parse on the note stops it as on any diagnostic: the
  `NextPart` call that recorded it returns the parse's stop instead of
  `io.EOF`, as every later call and `Drain` do; a body read to its end still
  ends with `io.EOF`, and `Drain` returns the stop.
- Setup: the boundaries are kept in a map from their form without trailing
  spaces and tabs to a small tree of those trailing bytes, so many boundaries
  differing only there cost the bytes they are declared with, to set up and to
  match; a boundary declared again keeps its first declaration.
  `Message` names the chosen and the unselected media type and nothing else;
  `Location` is where the first delimiter line begins; `Action` names the
  declaration read and the explicit reparse, and says nothing of which parts
  the other reading would give, which may overlap with this one's (a chosen
  multipart, or a boundary recovery already framed on). When several
  declarations qualify, the one whose delimiter line comes first is named.

To read the unselected declaration, rewrite the header and parse again. With
a `Source` (`ParseAt`, `ParseSource`), or slicing a kept copy of the input at
`Entity.RawSpan`:

```go
rh, _ := e.RawHeader()
h, _ := textproto.ReadHeader(bufio.NewReader(rh))
f := h.FieldsByKey("Content-Type")
f.Next()
f.Del() // repeat for every field before the one wanted
var head bytes.Buffer
textproto.WriteHeader(&head, h)
rb, _ := e.RawBody()
session, alt, err := message.Parse(io.MultiReader(&head, rb), nil)
```

The other fields keep their bytes. The result is a second parse with its own
session and offsets relative to the rewritten input, not a branch of the
first tree; no API offers both readings at once.
`TestUnselectedMultipart_reparse` runs this procedure.

## Not changed

- A damaged line ended by a bare CR is decided as before (ROB-020-002).
- A body whose first line reads as a valid field (`Note: ...`) is still taken
  as a field: a missing separator is only seen at a line that is no field.
- A body that begins with a forwarded header block (`From:`, `Subject:` at the
  line start, before any empty line) after a damaged line is read as header.

Two items this list used to hold — empty names skipped without their bytes,
and raw fields storing every line ending as CRLF — are closed by "Byte
preservation" below.

## Changed test expectations

Tests that used a malformed header line as an error under the default profile
now expect the healed result there and keep the error under `ProfileStrict`:
`framing_bom_test.go`, `framing_mbox_test.go`, `framing_orphan_test.go`,
`framing_firstline_test.go`, `limit_header_bytes_test.go`,
`multipart_lineend_test.go`, `parse_test.go`, `mail/reader_lineend_test.go`.

## Byte preservation (ROB-020-006, ROB-020-007)

Code: `textproto/header_exact.go`, `header_exact.go`, small edits in
`textproto/header.go` (`readFields`, `WriteHeader`), `Entity.WriteTo` and
`CreateWriter`. Tests: `textproto/header_exact_test.go`,
`header_exact_test.go`.

A block read with `textproto.HeaderLimits.Recover` — every header under
`Parse` but `ProfileStrict`'s — now keeps every byte it was read from, and an
unmodified header writes them back.

### Empty names (ROB-020-006)

A line with nothing before its separator (`: value`, or `; value` with no
colon) was skipped without keeping its bytes. Under recovery it is kept as an
orphan record of one line, the same accessor the other damaged lines use
(`Header.OrphanContinuations`), and reported once per block as
`header.empty_name` (Recovered). It is no field and no lookup key: `Get`,
`Has`, `Values`, `Fields`, `Len`, `Raw` and `Map` never see it.

Budgets: it costs exactly what it cost when it was skipped — one candidate of
`MaxHeaderCount` and `MaxTotalHeaderCount`, and its bytes as read of
`MaxLineBytes`, `MaxHeaderFieldBytes`, `MaxHeaderBytes` and
`MaxTotalHeaderBytes`. What is kept is those bytes, so the budgets that
admitted them bound it. Unlike the other single-line records it is always
read past (it never begins the body, as it never did), and it does not make
the lines after it in a part's header be looked at as delimiter lines. So
nothing about the accounting moves: the budget matrices
(`limit_header_*_test.go`) keep every expectation, and only the extra
diagnostic appears in them.

An empty-name line whose bare CR the next line settles is kept the same way,
with whatever that CR joined to it.

Raw 8-bit bytes: a field's value keeps its non-UTF-8 bytes in `Header.Raw`,
`HeaderFields.Raw` and `Get`; `Text` is the view that is always valid UTF-8
(each invalid byte becomes U+FFFD). Pinned by `TestParse_headerRaw8bit`.

### Line endings and the end of the block (ROB-020-007)

- A field keeps two spellings when they differ. `Raw` is unchanged: every
  physical line ended by CRLF. `Header.RawAsRead` is the input's: bare LF,
  bare CR, or no line ending on a last line the input ended. Name case, white
  space around the colon, folding, trailing white space and 8-bit bytes were
  already the same in both. A field whose lines all ended with CRLF keeps one
  spelling; any other is held twice, so a block retains at most twice its
  byte budget.
- The block's empty line is remembered as it was: CRLF, LF, a bare CR, or
  none (end of input, missing separator, delimiter line).
- `Header.Intact` reports that a header is exactly the block read: read
  whole under recovery, every byte kept, no field added or deleted. `Copy`
  keeps it; a `Del` that deletes nothing keeps it. Whether every byte was
  kept is checked, not assumed: the kept bytes are counted against the bytes
  the block consumed, and a block for which they differ is not intact.

What is written for an intact header:

| Block ended with | `textproto.WriteHeader`, `CreateWriter`, `CreatePart` | `textproto.WriteHeaderAsRead`, `Entity.WriteTo` with the body as read |
| --- | --- | --- |
| empty line, CRLF or LF | exact bytes | exact bytes |
| empty line, bare CR | CRLF lines, CRLF empty line (as before) | exact bytes |
| no empty line | CRLF lines, CRLF empty line added (as before) | exact bytes |

The middle column is deliberate. After a block with no empty line a body
would be read as header, and after an empty line that is a bare CR a body
beginning with LF would lose that byte to the line ending. So a writer that
does not know what follows writes those two as it always did.
`Entity.WriteTo` knows: when it is about to copy the body exactly as it was
read (the condition of its verbatim body copy), it writes the header as read;
with any other body it does not (`TestParse_headerReplaySafe`).

Exact through parse then write (`TestHeaderExact_replay`,
`TestParse_headerReplay`, and a fuzz target, `FuzzHeaderExact_replay`, that
holds every block a recovering read accepts to the replay): LF-only, CR-only
and mixed line endings; a CR that is content of a value; folding with tabs
and white space only lines; trailing white space; name case and space before
the colon; order and duplicates; 8-bit values; the semicolon separator; a
UTF-8 mark and a malformed prefix; orphan records; unindented continuations;
missing-colon, invalid-name and empty-name lines; a missing separator; a
header the input ended, with or without a final line ending.

### Not exact, by design

- `Read`, `ReadWithOptions`, `New`, `ProfileStrict`, and `ReadHeader` /
  `ReadHeaderLimits` without `Recover`: unchanged. Lines are written with
  CRLF, an empty-name line is not written, an empty line is always added.
- A header that has been changed (a field added or deleted) is written as
  before: CRLF lines, damaged lines in their places, one CRLF empty line.
- `Entity.WriteTo` adds `Mime-Version: 1.0` to a header without one, which
  changes it. `textproto.WriteHeaderAsRead` on `e.Header.Header` followed by
  the body is the replay for such a message.
- `Entity.WriteTo` refuses an entity whose Content-Type does not parse, and
  re-encodes a body it cannot copy verbatim; neither is this item's.
- A damaged line ended by a bare CR is still decided as before
  (ROB-020-002). After a field that a bare CR ended it becomes content of
  that field's value, and is replayed with it. Where nothing takes it —
  `broken line\rSubject: x\r\r`, or the same after a CRLF-ended field — the
  block is still refused under recovery too, so there is nothing to replay.
  An empty-name line there is kept. Open: heal that case as the LF and CRLF
  ones are.

### Changed test expectations

- `framing_lineend_test.go` (`TestParse_lineEndings_writeNormalizes`): a
  healing parse now writes LF, CR and unterminated headers as read, not as
  CRLF with an empty line.
- `framing_empty_test.go` (`checkFramingRewrite`): a header-only input that
  says MIME-Version is written without the empty line it did not have, under
  healing profiles only.
- `framing_orphan_test.go`: the empty-name line is kept and reported under
  the tolerant profile; the fuzz oracle allows records of one line, compares
  with `ProfileStrict` only where recovery healed nothing, and expects exact
  bytes back where the header was replayed.
- `limit_header_fields_test.go`, `limit_header_total_test.go`: the fixtures'
  empty-name lines now add `header.empty_name` notes, which those tests set
  aside (`hpNoEmptyNameNotes`); every budget expectation is unchanged.

## Unfolding (ROB-020-007 follow-up, #63)

Code: `unfoldValue` in `textproto/header.go`, used by `pendingField.field`
for a block read with `HeaderLimits.Recover`. Tests:
`textproto/header_unfold_test.go`, `unfold_parse_test.go`.

A field's value (`Get`, `HeaderFields.Value`, and every view built on it:
`Text`, `mail.Header.Subject`, the address and date accessors) is now
unfolded as RFC 5322 section 3.2.2 says under `Parse` with every profile but
`ProfileStrict`: the line endings are removed and nothing else. CRLF, and the
bare LF and bare CR that recovery accepts as line endings, all go; the fold's
white space stays as written.

| Field | Before | Now (healing `Parse`) |
| --- | --- | --- |
| `Subject: that is\r\n\tfolded` | `that is folded` | `that is\tfolded` |
| `Subject: a  \r\n   b` | `a b` | `a     b` |
| `Subject: s\r\n \t\r\n` (white space only continuation) | `s` | `s \t` |
| `Subject: x \t` (trailing white space) | `x` | `x \t` |
| `Subject: a\r\n \r\n b` | `a b` | `a  b` |

What stays:

- The white space (and any folds) between the colon and the value's first
  other byte is not part of the value, as before: `Subject:\r\n\tx` is `x`.
- A line recovery joined as a continuation that lost its leading white space
  (`header.unindented_continuation`) has no fold white space to keep; one
  space stands for what it lost, as before, so `Received: from a` +
  `by b` is `from a by b`, not `from aby b`.
- A CR that recovery found to be content of a value (not a line ending)
  stays in it, as before.
- RFC 2047: white space between adjacent encoded words is still dropped,
  tabs and folds included, so `=?utf-8?q?a?=\r\n\t =?utf-8?q?b?=` reads `ab`.
  Text between them keeps its white space.
- The bytes: `Raw`, `RawAsRead`, `WriteHeader`, `WriteHeaderAsRead`, the
  intact replay and `TestParse_headerReplay` are untouched; only the value
  string changed.

Decision on the other paths: `Read`, `ReadWithOptions`, `New` (with a header
from them), `ProfileStrict`, `textproto.ReadHeader`, `ReadHeaderLimits`
without `Recover`, and `Header.AddRaw` keep the legacy unfolding (each fold
and the white space around it one space, a white space only continuation
dropped, each line trimmed). They are the strict, compatible surface, and
their callers' values must not move; `TestUnfold_legacy`,
`TestUnfold_addRaw` and `TestUnfold_parseProfiles` pin both rules side by
side. Every other `textproto.ReadHeaderRecover` caller gets the RFC rule
too: the `mail` report views (`rptOriginalHeader`, the delivery-status
groups of `mail/tree.go`), whose field readers already trim each value.

Cost: one pass over the value, the same as before; `TestUnfold_manyFolds`
checks 2,000 folds against an independently built value.

### Changed test expectations (#63)

- `textproto/header_exact_test.go` (`TestHeaderExact_views`): `Get` of
  `Subject: caf\xe9\n\tnext \n` is `"caf\xe9\tnext "`, was `"caf\xe9 next"`.
- `header_recover_test.go` (`white space only separator`): the Subject is
  `"hi \t"`, was `"hi"`.
- `framing_firstline_test.go` (`a whitespace line after a field is a fold`):
  under the healing profiles `A` is `"1 "` (new `healed` column), was `"1"`;
  Read, ReadWithOptions and `ProfileStrict` keep `"1"`.
- `framing_empty_test.go`: the Subject `one\r\n\ttwo` is `"one\ttwo"` under
  the healing profiles (`framingForm.under`), `"one two"` under Read,
  ReadWithOptions and `ProfileStrict`.
- `testdata/corpus/manifest.json` (`header-whitespace-separator`): the
  Subject record is `Subject 3 "s"` (three bytes, `s \t`; the snippet trims),
  was `Subject 1 "s"`. `charset`'s `TestCorpusWithPlugin` reads the same
  expectation.
- `testdata/differential/exceptions.json`: the two open exceptions
  `header-whitespace-separator` and `dx-subject-folded` (field `subject`)
  were reported stale by `EMLKIT_DIFFERENTIAL=1` and are removed.

### Replay fuzz oracle correction (#63 review)

A 30 s run of `FuzzHeaderExact_replay` during this work failed on the
two-byte input `FE FF`: "the block "" was read but is not intact". The
failure has nothing to do with unfolding. It reproduces on a clean export of
`HEAD` (`ef192b2`), without any of this change. And no product code was wrong. A block that a
UTF-16 or UTF-32 mark begins is not read (bom.go, `readHeader`):
`ReadHeaderRecover` returns a Header with the mark, no fields and no error,
consumes nothing, and `WriteHeaderAsRead` refuses that Header. The oracle,
`hpReplayCheck`, took every result without an error to be an accepted block
that must be intact and replay exactly, which a block that was not read
cannot be.

The oracle now has a separate branch for a wide mark. That branch holds the
read to the documented contract rather than skipping it: nothing consumed or
handed back, the whole input still in the reader with the mark first, no
field, orphan record or malformed prefix, not `Intact`, and
`WriteHeaderAsRead` returns an error and writes nothing. Every other
nil-error result is checked as before. The four marks alone, each mark
followed by a header in its own encoding, and marks followed by ASCII text
are permanent seeds of the fuzz target and the cases of
`TestHeaderExact_wideBOM`. Against the old oracle all eleven failed with the
same message. Product BOM semantics are unchanged.

## Observations

ROB-130-003, corpus findings subject-5000 and mbox-from-line. Code:
`ob_observe.go`, `textproto/ob_longline.go`, one line each in
`line_endings.go` and `header_recover.go`. Tests: `TestObservations`,
`TestObservations_text` in `ob_observe_test.go`.

- `header.overlong_line` (Info): a header block — the message's or a part's —
  has a line of more than the 998 bytes RFC 5322 section 2.1.1 allows before
  its line ending. Recorded once per block, at the first such line, with its
  field's key in `Field` (empty for a malformed prefix or an orphan record);
  the Message gives the length, no byte of the line. The line is read whole
  as before; `Limits.MaxLineBytes` (64 KiB) is what refuses one.
  `textproto.Header.LongLine` finds it in the bytes the header kept, a line
  ending at CRLF, a bare LF or a bare CR. A folded field whose physical lines
  are all short records nothing. Not under `ProfileStrict`.
- `header.invalid_name` for the input's first line, when that line is an mbox
  `From_` line of exactly the grammar `ParseOptions.RecognizeMboxEnvelope`
  recognizes (mbox.go) and the option is off: the code, severity and what is
  parsed are unchanged; the Message adds that the first damaged line is an
  mbox envelope line and the Action names the option. Only the root block at
  offset 0 of the input is looked at — not one after a byte order mark, not a
  part's, not an embedded message's.

Changed expectations: tests whose fixtures hold header lines past 998 bytes
to exercise a budget now drop `header.overlong_line` before comparing
(`obDropOverlong`): `limit_header_total_test.go`
(`TestParse_totalHeaders_realDefaultBytes`), `limit_parameters_test.go`
(`TestParse_parameters_disabled`, `checkParameterDiagnostic`),
`mediatype_tolerant_test.go` (`TestTolerantFields_budgets`),
`progress_metadata_test.go` (`TestParse_metadataNoProgressBounds`) and
`limits_test.go` (`TestDefaultLimits_headerBytesUnchanged`). No budget
expectation moved.

### Independent review of #63 (2026-11-13)

Codex reviewed the unfolding implementation, unchanged legacy paths, exact replay tests, corpus changes, and the wide-BOM fuzz oracle correction. Focused root/textproto race tests, fresh full unit tests, build, gofmt and diff checks passed. The Python differential run reports 74 fixtures, 42 explained differences, no open/unexplained differences and no stale exceptions. Claude's bounded replay fuzz run passed 1,313,683 executions in 30 seconds. This closes the #63 follow-up; other header replay limitations recorded in the ledger remain separate.
