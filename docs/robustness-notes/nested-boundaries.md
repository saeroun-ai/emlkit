# Nested boundaries (ROB-060-011)

Code: `multipart_nested.go`, `textproto/multipart_resume.go`, and small hooks
in `multipart.go` (`nextPart`) and `multipart_heal.go` (`boundaryChain`).
Tests: `multipart_nested_test.go` (the cases below, 2 to 4 levels deep, under
`ProfileTolerant` and `ProfileForensic`, the input whole and one byte at a
time) and `textproto/multipart_resume_test.go`.

## The rule the wire layer applies

A delimiter line is `--`, the boundary, `--` for the closing one, then only
spaces and tabs, and its line ending. The comparison is byte for byte, so case
matters and a line `--B-x` or `--BB` is no delimiter line of `B`.

A part of a multipart ends at the first delimiter line of that multipart's
boundary. A nested multipart reads only the bytes of the part it is, so a
line that is a delimiter line of an enclosing boundary ends everything nested
inside that part, whatever the nested boundaries are. When two boundaries
collide, the line belongs to the outermost multipart whose delimiter line it
is.

## Cases

Outer boundary `O`, nested boundary `I`, as declared.

| Case | What a consumer sees | Diagnostics |
| --- | --- | --- |
| Distinct, prefixes of one another (`B` in `BB`, `BB` in `B`, 4 levels `B` ⊂ `BB` ⊂ `BBB` ⊂ `BBBB`), a trailing `-` on either, case only, trailing white space on the outer one | The declared tree | none |
| `I` = `O` (2 levels) | The nested multipart is empty; its parts, and the outer parts after it, are parts of the outer one | `multipart.nested_boundary_reused`, `multipart.empty` on the nested one; `multipart.close_shared` on the outer one |
| `I` = `O` at 3 levels | Every part is a part of the outermost; nothing after the inner closes is lost | as above, per nested multipart, and one `close_shared` per shared close read past |
| `I` = `O` across a level (`B` > `C` > `B`) | The middle one is cut at the first `--B` and is `close_not_found`; its later parts become outer parts (their `--C` lines are orphan records of the header) | `nested_boundary_reused`, `empty`, `close_not_found`, `close_shared`, `header.missing_colon` |
| `I` = `O` + white space (`"B "` in `B`) | As for `I` = `O`; the nested closing line `--B --` is no delimiter of `B` and stays in the last of those parts | `multipart.nested_boundary_conflict`, `multipart.empty` |
| `I` = `O` + `--` (`B--` in `B`) | Each nested opening line is the outer closing line: the outer reads on past each, so the nested parts are outer parts; the nested closing line `--B----` stays in the last | `nested_boundary_conflict`, `empty`, one `close_shared` per opening |
| `O` = `I` + `--` (`B` in `B--`) | The nested closing line `--B--` opens an outer part, empty; the nested one reports no close | `nested_boundary_conflict`, `close_not_found` on the nested one |
| Nested close missing | The outer delimiter ends the nested multipart; outer siblings are read | `close_not_found` on the nested one |
| Nested close present, outer close missing | Outer ends at the end of the input | `close_not_found` on the outer one (and `close_shared` when the boundary is shared) |
| Shared boundary, only an epilogue after the closes | The epilogue is not read as parts; under `ProfileForensic` it is the trailing input | `nested_boundary_reused`, `empty` |
| Inner part text holds a line that is the outer delimiter | It ends the whole nested multipart there (RFC 2046); the text after it begins an outer part, whose non-field lines are kept as orphan records of its header | `close_not_found` on the nested one, `header.missing_colon` |
| Inner part text holds a sibling multipart's delimiter line | Body text: only the enclosing boundaries frame it | none |
| Inner boundary a prefix of the outer, input cut right after an inner opening line | The outer reads the line as its own delimiter cut short (ROB-060-006); the nested part before it is kept | `close_not_found` on both |

## Reading on past a shared close

With a shared boundary every closing line, the nested ones' included, is read
by the outermost multipart, which ended at the first one. That left the outer
parts after a nested close in its epilogue, lost. Now, under `Parse` (not
`ProfileStrict`, `Read`, `ReadWithOptions` or `New`):

- Each framed part that is a multipart declaring the same boundary as an
  enclosing one lets the outermost such multipart read on past one more
  closing line. One declaring that boundary followed by `--` lets it read on
  past any number, since each of its opening lines is such a line.
- At a closing line it may read on past, the reader looks ahead, within
  `Limits.MaxPreambleBytes`, past blank lines and as many further closing
  lines as it may still read on past. An opening line there resumes the
  iteration. A line of text there resumes it too, as a part with no delimiter
  line of its own that runs to the next delimiter line, provided one follows
  within the bound. Otherwise nothing is read on: the bytes looked at are
  given back, and they stay the epilogue, and the trailing input under
  `ProfileForensic`.
- Each resumption is reported as `multipart.close_shared` (Recovered) on the
  outer multipart, at the byte after the closing line.

## Why

The tree the sender meant is ambiguous once two levels share a boundary: a
closing line may end either. Reading the nested parts as parts of the outer
one, as declared framing implies, keeps every body and reports the conflict;
attributing them back to the nested multipart would have to guess which
closing line ends which level. The look-ahead is what keeps a real epilogue from being read as parts: it is
read on only where a later delimiter line of the same boundary shows the
multipart goes on, and only as often as the nested declarations account for.

## New codes

- `multipart.nested_boundary_conflict` (Recovered): a nested boundary that is
  an enclosing one's plus trailing white space, plus `--`, or that plus `--`
  is the enclosing one's.
- `multipart.close_shared` (Recovered): the iteration read on past a closing
  line a nested multipart may have shared.

## Open

- A message embedded in a part (`message/rfc822`) whose multipart reuses an
  enclosing boundary is reported as before, but gives no credit to read on
  past its close, so outer parts after it may still be left in the epilogue.
- Text after a shared close that is longer than `MaxPreambleBytes` before the
  next delimiter line is left in the epilogue.
- The empty outer part in the `B` in `B--` case is not dropped, and nested
  closing lines no enclosing boundary frames (`--B --`, `--B----`) stay in the
  body of the part before them.
