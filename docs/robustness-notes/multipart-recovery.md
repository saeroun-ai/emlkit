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

## Preamble (ROB-060-001)

The bytes before the first delimiter line are kept, up to
`Limits.MaxPreambleBytes` (1 MiB by default), and returned by
`message.MultipartPreamble(mr)`. A longer preamble is cut there and recorded
as `limit.preamble_bytes`; it ends nothing, and delimiters after it are still
found. The kept bytes are also what inference reads, so a body with no
delimiter line that is longer than the budget is returned as one part holding
its first `MaxPreambleBytes` bytes (`multipart.start_not_found`, Incomplete).

The epilogue is unchanged: `textproto.MultipartReader.Epilogue` at the wire
layer, and `ParseSession.TrailingInputReader` under `ProfileForensic` for the
root. `MaxEpilogueBytes` is still not enforced.

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
- Parts after a closing delimiter are epilogue and are not read as parts.
- With a reused boundary the outer multipart's own later parts may fall after
  the inner closing delimiter and be lost as epilogue.
- A repaired multipart with conflicting boundary parameters stays one opaque
  part (ROB-050-001); inference is not tried for it.
