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
- A line with an empty name (`: value`) is still skipped; its bytes are not
  kept. ROB-020-006 is therefore partial: invalid names are kept, empty ones
  are not.
- A body whose first line reads as a valid field (`Note: ...`) is still taken
  as a field: a missing separator is only seen at a line that is no field.
- A body that begins with a forwarded header block (`From:`, `Subject:` at the
  line start, before any empty line) after a damaged line is read as header.
- Raw fields still store every line ending as CRLF (ROB-020-007 open).

## Changed test expectations

Tests that used a malformed header line as an error under the default profile
now expect the healed result there and keep the error under `ProfileStrict`:
`framing_bom_test.go`, `framing_mbox_test.go`, `framing_orphan_test.go`,
`framing_firstline_test.go`, `limit_header_bytes_test.go`,
`multipart_lineend_test.go`, `parse_test.go`, `mail/reader_lineend_test.go`.
