# Multipart recovery (ROB-060-001 to -006, -009, -011)

Issues #29, #30, #31. Code: `multipart_heal.go`, `multipart.go`, `entity.go`,
`textproto/multipart.go`. Tests: `multipart_heal_test.go`.

## What changed

Under every profile but `ProfileStrict`, `Parse` reads a multipart body with
`textproto.HeaderLimits.Recover` and heals what its framing lacks. The
iteration no longer ends with an error for these, and `Entity.Walk` and
`mail.Reader` reach every part that is there. `ProfileStrict`, `Read`,
`ReadWithOptions` and `New` frame as before.

| Defect | Result | Diagnostic |
| --- | --- | --- |
| Input ends with no closing delimiter | last part runs to the end and ends with `io.EOF`; `NextPart` returns `io.EOF` | `multipart.close_not_found` (Incomplete) |
| Last delimiter cut by the end of the input (`--boundary-`, or at least six bytes of `--boundary`) | read as a delimiter line | `multipart.close_truncated` (Recovered), or `close_not_found` when it was an opening one |
| No boundary declared, or declared empty | body framed on the boundary its delimiter lines use | `multipart.boundary_inferred` (Recovered) |
| Declared boundary appears on no line; the body uses another | body framed on the boundary found | `multipart.boundary_mismatch` (Recovered) |
| More than one boundary qualifies | the best, earliest among equals | plus `multipart.boundary_ambiguous` (Info) |
| No delimiter line of any boundary | the body is returned as one part with no header | `multipart.start_not_found` (Recovered) |
| Body holds no part at all | no part, `io.EOF` | `multipart.empty` (Info) |
| Inner multipart declares an enclosing boundary | framed as declared; its parts are read as parts of the outer one | `multipart.nested_boundary_reused` (Recovered) |
| Inner multipart never closed | ends where the outer part ends; outer siblings are read | `multipart.close_not_found` on the inner one |

## Boundary inference (ROB-060-002 to -004)

It runs only when no line of the body is a delimiter of the declared boundary.
A candidate is the text after `--` of a line that begins with it: 1 to 70
bytes, a letter or digit among them. It qualifies when it introduces at least
one part and either

- a line `--candidate--` closes it, or
- a part it introduces begins with a header field or an empty line, and it is
  seen more than once or that field is a `Content-*` field.

A candidate equal to the declared boundary but for case or surrounding white
space is preferred. Lines of dashes and `-- ` signature separators do not
qualify. At most 64 distinct candidates are tracked.

Inference is tried only while at most 8 (`pfInferNestMax`) enclosing
multiparts were themselves framed on an inferred boundary. A deeper one is
returned as one part with no header holding its kept body, with
`multipart.start_not_found` (Incomplete), so a message nested deeper than
that with no usable boundary at any level loses its inner structure, not its
bytes. Multiparts framed on their declared boundary do not count
(`multipart_heal_nest.go`; see performance.md, item 2).

## Preamble (ROB-060-001)

The bytes before the first delimiter line are kept, up to
`Limits.MaxPreambleBytes` (1 MiB by default), and returned by
`message.MultipartPreamble(mr)`. A longer preamble is cut there and recorded
as `limit.preamble_bytes`; it ends nothing, and delimiters after it are still
found. The kept bytes are also what inference reads, so a body with no
delimiter line that is longer than the budget is returned as one part holding
its first `MaxPreambleBytes` bytes (`multipart.start_not_found`, Incomplete).

## Epilogue (ROB-060-001)

Code: `multipart_epilogue.go`. Tests: `multipart_epilogue_test.go`.

`message.MultipartEpilogue(mr)` returns the bytes after the closing delimiter
line (its line ending excluded), whether they were cut, and ok. ok is false
for an iterator that does not heal (not from `Parse`, `ProfileStrict`,
`NewMultipart`) and until `NextPart` has returned `io.EOF` at a closing
delimiter line: an unclosed multipart has no epilogue. Iteration never reads
it; the first call does, at most `Limits.MaxEpilogueBytes` (1 MiB by default;
`LimitDisabled` keeps the default, never unbounded) plus one byte. A longer
epilogue is cut there, recorded as `limit.epilogue_bytes` (LimitExceeded), and
the rest is left unread. Later calls return the same bytes without reading.

- A nested multipart's epilogue ends where its enclosing part ends.
- With an inferred boundary it is the rest of the kept body.
- An input error or a parse stop ends it there with the bytes read; the error
  stays the session's and adds no diagnostic.
- For the message's own body under `ProfileForensic` it is read through
  `ParseSession.TrailingInputReader`, so `TrailingInput`'s count and evidence
  (and `input.trailing_messages`) account for it; those bytes are not
  delivered again, and bytes that reader delivered first are not in it.

## Changed expectations

- An empty multipart body: `NextPart` returns `io.EOF` at once and records
  `multipart.empty`, where it returned an error wrapping `io.EOF` and recorded
  nothing (`framing_empty_test.go`, `mail/reader_empty_test.go`,
  `limit_mime_depth_test.go`, `limit_diagnostic_count_test.go`).
- A multipart with no boundary parameter is framed by inference
  (`metadata_repair_unframed_test.go`).
- `MaxPreambleBytes` is enforced (`limit_header_fields_test.go`).

## Known limitations

- A delimiter line with text after the boundary (`--boundary junk`) is body,
  as before.
- Parts after a closing delimiter are epilogue and are not read as parts,
  except past a closing line a nested multipart may have shared
  (`multipart.close_shared`, nested-boundaries.md).
- Reused and colliding nested boundaries: see `nested-boundaries.md`.
- A repaired multipart with conflicting boundary parameters stays one opaque
  part (ROB-050-001); inference is not tried for it.

## Observations

ROB-130-003, corpus finding list-footer-after-close. Code:
`mail/tree_epilogue.go`, a few lines in `mail/tree.go`. Tests:
`TestReadTree_epilogue`, `TestReadTree_epilogueForensic`,
`TestReadTree_corpusObservations` in `mail/ob_tree_test.go`.

`mail.ReadTree` keeps each multipart's preamble and epilogue on its node
(`Node.Preamble`, `Node.Epilogue` and their `Truncated` flags), read through
`MultipartPreamble` and `MultipartEpilogue` once its last part has been read,
and records `multipart.trailing_junk` (Info) in `Tree.Diagnostics` — not in
the session — for each epilogue that holds anything but spaces, tabs and line
endings. Its Message gives the number of bytes kept ("more than" when cut at
`MaxEpilogueBytes`) and what they read as, in this order: further delimiter
lines of the multipart's own boundary (parts a closing delimiter cut off), a
header block (a first line that is a field or `From `), text (UTF-8 with no
control bytes but white space), or binary data. No byte of the epilogue is in
it; the bytes are the node's. An unclosed multipart has no epilogue and no
note.

`ReadTree` now reads epilogues, which it did not: at most
`MaxEpilogueBytes` plus one byte each, a nested one from its enclosing part,
the message's own from the input after the closing delimiter. A longer one
records `limit.epilogue_bytes` against the container, which makes it
`Repaired`. Under `ProfileForensic` the message's own epilogue is read
through `TrailingInputReader`, as `MultipartEpilogue` documents: its count,
evidence and `input.trailing_messages` account for those bytes, and the
reader does not deliver them again. A stop the read runs into is returned by
`ReadTree`, as any stop is.

The parse's own diagnostics, and so the corpus manifest, are unchanged; a
caller that does not use `ReadTree` asks `MultipartEpilogue` itself.
