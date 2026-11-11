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
